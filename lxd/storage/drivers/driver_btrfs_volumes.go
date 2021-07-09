package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/instancewriter"
	"github.com/lxc/lxd/shared/ioprogress"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/units"
)

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied filler function.
func (d *btrfs) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	volPath := vol.MountPath()

	// Setup revert.
	revert := revert.New()
	defer revert.Fail()

	// Create the volume itself.
	_, err := shared.RunCommand("btrfs", "subvolume", "create", volPath)
	if err != nil {
		return err
	}
	revert.Add(func() {
		d.deleteSubvolume(volPath, false)
		os.Remove(volPath)
	})

	// Create sparse loopback file if volume is block.
	rootBlockPath := ""
	if vol.contentType == ContentTypeBlock {
		// We expect the filler to copy the VM image into this path.
		rootBlockPath, err = d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}
	}

	err = d.runFiller(vol, rootBlockPath, filler)
	if err != nil {
		return err
	}

	// If we are creating a block volume, resize it to the requested size or the default.
	// We expect the filler function to have converted the qcow2 image to raw into the rootBlockPath.
	if vol.contentType == ContentTypeBlock {
		// Convert to bytes.
		sizeBytes, err := units.ParseByteSizeString(vol.ConfigSize())
		if err != nil {
			return err
		}

		// Normally we pass VolumeTypeImage to unsupportedResizeTypes to ensureVolumeBlockFile as block
		// image volumes cannot be resized because they have a readonly snapshot which the instances are
		// created from, and that doesn't get updated when the original volumes size is changed.
		// However during initial volume fill we allow growing of image volumes because the snapshot hasn't
		// been taken yet. This is why no unsupported volume types are passed to ensureVolumeBlockFile.
		// This is important, as combined with not enabling allowUnsafeResize it still prevents us from
		// accidentally shrinking the filled volume if it is larger than vol.ConfigSize().
		// In that situation ensureVolumeBlockFile returns ErrCannotBeShrunk, but we ignore it as this just
		// means the filler run above has needed to increase the volume size beyond the default block
		// volume size.
		_, err = ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, false)
		if err != nil && errors.Cause(err) != ErrCannotBeShrunk {
			return err
		}

		// Move the GPT alt header to end of disk if needed and if filler specified.
		if vol.IsVMBlock() && filler != nil && filler.Fill != nil {
			err = d.moveGPTAltHeader(rootBlockPath)
			if err != nil {
				return err
			}
		}
	} else if vol.contentType == ContentTypeFS {
		// Set initial quota for filesystem volumes.
		err := d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
		if err != nil {
			return err
		}
	}

	// Tweak any permissions that need tweaking after filling.
	err = vol.EnsureMountPath()
	if err != nil {
		return err
	}

	// Attempt to mark image read-only.
	if vol.volType == VolumeTypeImage {
		err = d.setSubvolumeReadonlyProperty(volPath, true)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// CreateVolumeFromBackup restores a backup tarball onto the storage device.
func (d *btrfs) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
	// Handle the non-optimized tarballs through the generic unpacker.
	if !*srcBackup.OptimizedStorage {
		return genericVFSBackupUnpack(d, vol, srcBackup.Snapshots, srcData, op)
	}

	if d.HasVolume(vol) {
		return nil, nil, fmt.Errorf("Cannot restore volume, already exists on target")
	}

	revert := revert.New()
	defer revert.Fail()

	// Define a revert function that will be used both to revert if an error occurs inside this
	// function but also return it for use from the calling functions if no error internally.
	revertHook := func() {
		for _, snapName := range srcBackup.Snapshots {
			fullSnapshotName := GetSnapshotVolumeName(vol.name, snapName)
			snapVol := NewVolume(d, d.name, vol.volType, vol.contentType, fullSnapshotName, vol.config, vol.poolConfig)
			d.DeleteVolumeSnapshot(snapVol, op)
		}

		// And lastly the main volume.
		d.DeleteVolume(vol, op)
	}
	// Only execute the revert function if we have had an error internally.
	revert.Add(revertHook)

	// Find the compression algorithm used for backup source data.
	srcData.Seek(0, 0)
	_, _, unpacker, err := shared.DetectCompressionFile(srcData)
	if err != nil {
		return nil, nil, err
	}

	// Load optimized backup header file if specified.
	var optimizedHeader *BTRFSMetaDataHeader
	if *srcBackup.OptimizedHeader {
		optimizedHeader, err = d.loadOptimizedBackupHeader(srcData)
		if err != nil {
			return nil, nil, err
		}
	}

	// Populate optimized header with pseudo data for unified handling when backup doesn't contain the
	// optimized header file. This approach can only be used to restore root subvolumes (not sub-subvolumes).
	if optimizedHeader == nil {
		optimizedHeader = &BTRFSMetaDataHeader{}
		for _, snapName := range srcBackup.Snapshots {
			optimizedHeader.Subvolumes = append(optimizedHeader.Subvolumes, BTRFSSubVolume{
				Snapshot: snapName,
				Path:     string(filepath.Separator),
				Readonly: true, // Snapshots are made readonly.
			})
		}

		optimizedHeader.Subvolumes = append(optimizedHeader.Subvolumes, BTRFSSubVolume{
			Snapshot: "",
			Path:     string(filepath.Separator),
			Readonly: false,
		})
	}

	// Create a temporary directory to unpack the backup into.
	tmpUnpackDir, err := ioutil.TempDir(GetVolumeMountPath(d.name, vol.volType, ""), "backup.")
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Failed to create temporary directory %q", tmpUnpackDir)
	}
	defer os.RemoveAll(tmpUnpackDir)

	err = os.Chmod(tmpUnpackDir, 0100)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Failed to chmod temporary directory %q", tmpUnpackDir)
	}

	// unpackSubVolume unpacks a subvolume file from a backup tarball file.
	unpackSubVolume := func(r io.ReadSeeker, unpacker []string, srcFile string, targetPath string) (string, error) {
		tr, cancelFunc, err := shared.CompressedTarReader(context.Background(), r, unpacker)
		if err != nil {
			return "", err
		}
		defer cancelFunc()

		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break // End of archive.
			}
			if err != nil {
				return "", err
			}

			if hdr.Name == srcFile {
				subVolRecvPath, err := d.receiveSubVolume(tr, targetPath)
				if err != nil {
					return "", err
				}

				cancelFunc()
				return subVolRecvPath, nil
			}
		}

		return "", fmt.Errorf("Could not find %q", srcFile)
	}

	// unpackVolume unpacks all subvolumes in a LXD volume from a backup tarball file.
	unpackVolume := func(v Volume, srcFilePrefix string) error {
		_, snapName, _ := shared.InstanceGetParentAndSnapshotName(v.name)

		for _, subVol := range optimizedHeader.Subvolumes {
			if subVol.Snapshot != snapName {
				continue // Skip any subvolumes that dont belong to our volume (empty for main).
			}

			// Figure out what file we are looking for in the backup file.
			srcFilePath := filepath.Join("backup", fmt.Sprintf("%s.bin", srcFilePrefix))
			if subVol.Path != string(filepath.Separator) {
				// If subvolume is non-root, then we expect the file to be encoded as its original
				// path with the leading / removed.
				srcFilePath = filepath.Join("backup", fmt.Sprintf("%s_%s.bin", srcFilePrefix, PathNameEncode(strings.TrimPrefix(subVol.Path, string(filepath.Separator)))))
			}

			// Define where we will move the subvolume after it is unpacked.
			subVolTargetPath := filepath.Join(v.MountPath(), subVol.Path)

			d.Logger().Debug("Unpacking optimized volume", log.Ctx{"name": v.name, "source": srcFilePath, "unpackPath": tmpUnpackDir, "path": subVolTargetPath})

			// Unpack the volume into the temporary unpackDir.
			unpackedSubVolPath, err := unpackSubVolume(srcData, unpacker, srcFilePath, tmpUnpackDir)
			if err != nil {
				return err
			}

			// Clear the target for the subvol to use.
			os.Remove(subVolTargetPath)

			// Move unpacked subvolume into its final location.
			err = os.Rename(unpackedSubVolPath, subVolTargetPath)
			if err != nil {
				return err
			}
		}

		return nil
	}

	if len(srcBackup.Snapshots) > 0 {
		// Create new snapshots directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return nil, nil, err
		}

		// Restore backup snapshots from oldest to newest.
		for _, snapName := range srcBackup.Snapshots {
			snapVol, _ := vol.NewSnapshot(snapName)
			snapDir := "snapshots"
			srcFilePrefix := snapName
			if vol.volType == VolumeTypeVM {
				snapDir = "virtual-machine-snapshots"
				if vol.contentType == ContentTypeFS {
					srcFilePrefix = fmt.Sprintf("%s-config", snapName)
				}
			} else if vol.volType == VolumeTypeCustom {
				snapDir = "volume-snapshots"
			}

			srcFilePrefix = filepath.Join(snapDir, srcFilePrefix)
			err = unpackVolume(snapVol, srcFilePrefix)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	// Extract main volume.
	srcFilePrefix := "container"
	if vol.volType == VolumeTypeVM {
		if vol.contentType == ContentTypeFS {
			srcFilePrefix = "virtual-machine-config"
		} else {
			srcFilePrefix = "virtual-machine"
		}
	} else if vol.volType == VolumeTypeCustom {
		srcFilePrefix = "volume"
	}

	err = unpackVolume(vol, srcFilePrefix)
	if err != nil {
		return nil, nil, err
	}

	// Restore readonly property on subvolumes that need it.
	for _, subVol := range optimizedHeader.Subvolumes {
		if !subVol.Readonly {
			continue // All subvolumes are made writable during unpack process so we can skip these.
		}

		v := vol
		if subVol.Snapshot != "" {
			v, _ = vol.NewSnapshot(subVol.Snapshot)
		}

		path := filepath.Join(v.MountPath(), subVol.Path)
		d.logger.Debug("Setting subvolume readonly", log.Ctx{"name": v.name, "path": path})
		err = d.setSubvolumeReadonlyProperty(path, true)
		if err != nil {
			return nil, nil, err
		}
	}

	revert.Success()
	return nil, revertHook, nil
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *btrfs) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	// Scan source for subvolumes (so we can apply the readonly properties on the new volume).
	subVols, err := d.getSubvolumesMetaData(srcVol)
	if err != nil {
		return err
	}

	target := vol.MountPath()

	// Recursively copy the main volume.
	err = d.snapshotSubvolume(srcVol.MountPath(), target, true)
	if err != nil {
		return err
	}

	revert.Add(func() { d.deleteSubvolume(target, true) })

	// Restore readonly property on subvolumes in reverse order (except root which should be left writable).
	subVolCount := len(subVols)
	for i := range subVols {
		i = subVolCount - 1 - i
		subVol := subVols[i]
		if subVol.Readonly && subVol.Path != string(filepath.Separator) {
			targetSubVolPath := filepath.Join(target, subVol.Path)
			err = d.setSubvolumeReadonlyProperty(targetSubVolPath, true)
			if err != nil {
				return err
			}
		}
	}

	// Resize volume to the size specified. Only uses volume "size" property and does not use pool/defaults
	// to give the caller more control over the size being used.
	err = d.SetVolumeQuota(vol, vol.config["size"], false, op)
	if err != nil {
		return err
	}

	// Fixup permissions after snapshot created.
	err = vol.EnsureMountPath()
	if err != nil {
		return err
	}

	var snapshots []string

	// Get snapshot list if copying snapshots.
	if copySnapshots && !srcVol.IsSnapshot() {
		// Get the list of snapshots.
		snapshots, err = d.VolumeSnapshots(srcVol, op)
		if err != nil {
			return err
		}
	}

	// Copy any snapshots needed.
	if len(snapshots) > 0 {
		// Create the parent directory.
		err = createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}

		// Copy the snapshots.
		for _, snapName := range snapshots {
			srcSnapshot := GetVolumeMountPath(d.name, srcVol.volType, GetSnapshotVolumeName(srcVol.name, snapName))
			dstSnapshot := GetVolumeMountPath(d.name, vol.volType, GetSnapshotVolumeName(vol.name, snapName))

			err = d.snapshotSubvolume(srcSnapshot, dstSnapshot, true)
			if err != nil {
				return err
			}

			err = d.setSubvolumeReadonlyProperty(dstSnapshot, true)
			if err != nil {
				return err
			}

			revert.Add(func() { d.deleteSubvolume(dstSnapshot, true) })
		}
	}

	revert.Success()
	return nil
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *btrfs) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	// Handle simple rsync and block_and_rsync through generic.
	if volTargetArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volTargetArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		return genericVFSCreateVolumeFromMigration(d, nil, vol, conn, volTargetArgs, preFiller, op)
	} else if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_BTRFS {
		return ErrNotSupported
	}

	revert := revert.New()
	defer revert.Fail()

	var migrationHeader BTRFSMetaDataHeader

	// Inspect negotiated features to see if we are expecting to get a metadata migration header frame.
	if shared.StringInSlice(migration.BTRFSFeatureMigrationHeader, volTargetArgs.MigrationType.Features) {
		buf, err := ioutil.ReadAll(conn)
		if err != nil {
			return errors.Wrapf(err, "Failed reading migration header")
		}

		err = json.Unmarshal(buf, &migrationHeader)
		if err != nil {
			return errors.Wrapf(err, "Failed decoding migration header")
		}

		d.logger.Debug("Received migration meta data header", log.Ctx{"name": vol.name})
	} else {
		// Populate the migrationHeader subvolumes with root volumes only to support older LXD sources.
		for _, snapName := range volTargetArgs.Snapshots {
			migrationHeader.Subvolumes = append(migrationHeader.Subvolumes, BTRFSSubVolume{
				Snapshot: snapName,
				Path:     string(filepath.Separator),
				Readonly: true, // Snapshots are made readonly.
			})
		}

		migrationHeader.Subvolumes = append(migrationHeader.Subvolumes, BTRFSSubVolume{
			Snapshot: "",
			Path:     string(filepath.Separator),
			Readonly: false,
		})
	}

	// receiveVolume receives all subvolumes in a LXD volume from the source.
	receiveVolume := func(v Volume, receivePath string) error {
		_, snapName, _ := shared.InstanceGetParentAndSnapshotName(v.name)

		for _, subVol := range migrationHeader.Subvolumes {
			if subVol.Snapshot != snapName {
				continue // Skip any subvolumes that dont belong to our volume (empty for main).
			}

			subVolTargetPath := filepath.Join(v.MountPath(), subVol.Path)
			d.logger.Debug("Receiving volume", log.Ctx{"name": v.name, "receivePath": receivePath, "path": subVolTargetPath})
			subVolRecvPath, err := d.receiveSubVolume(conn, receivePath)
			if err != nil {
				return err
			}

			// Clear the target for the subvol to use.
			os.Remove(subVolTargetPath)

			// And move it to the target path.
			err = os.Rename(subVolRecvPath, subVolTargetPath)
			if err != nil {
				return errors.Wrapf(err, "Failed to rename '%s' to '%s'", subVolRecvPath, subVolTargetPath)
			}
		}

		return nil
	}

	// Get instances directory (e.g. /var/lib/lxd/storage-pools/btrfs/containers).
	instancesPath := GetVolumeMountPath(d.name, vol.volType, "")

	// Create a temporary directory which will act as the parent directory of the received ro snapshot.
	tmpVolumesMountPoint, err := ioutil.TempDir(instancesPath, "migration.")
	if err != nil {
		return errors.Wrapf(err, "Failed to create temporary directory under %q", instancesPath)
	}
	defer os.RemoveAll(tmpVolumesMountPoint)

	err = os.Chmod(tmpVolumesMountPoint, 0100)
	if err != nil {
		return errors.Wrapf(err, "Failed to chmod %q", tmpVolumesMountPoint)
	}

	// Handle btrfs send/receive migration.
	if len(volTargetArgs.Snapshots) > 0 {
		// Create the parent directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}
		revert.Add(func() { deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name) })

		// Transfer the snapshots.
		for _, snapName := range volTargetArgs.Snapshots {
			snapVol, _ := vol.NewSnapshot(snapName)
			err = receiveVolume(snapVol, tmpVolumesMountPoint)
			if err != nil {
				return err
			}
		}
	}

	// Receive main volume.
	err = receiveVolume(vol, tmpVolumesMountPoint)
	if err != nil {
		return err
	}

	// Restore readonly property on subvolumes that need it.
	for _, subVol := range migrationHeader.Subvolumes {
		if !subVol.Readonly {
			continue // All subvolumes are made writable during receive process so we can skip these.
		}

		v := vol
		if subVol.Snapshot != "" {
			v, _ = vol.NewSnapshot(subVol.Snapshot)
		}

		path := filepath.Join(v.MountPath(), subVol.Path)
		d.logger.Debug("Setting subvolume readonly", log.Ctx{"name": v.name, "path": path})
		err = d.setSubvolumeReadonlyProperty(path, true)
		if err != nil {
			return err
		}
	}

	if vol.contentType == ContentTypeFS {
		// Apply the size limit.
		err = d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// RefreshVolume provides same-pool volume and specific snapshots syncing functionality.
func (d *btrfs) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, op)
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
func (d *btrfs) DeleteVolume(vol Volume, op *operations.Operation) error {
	// Check that we don't have snapshots.
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return fmt.Errorf("Cannot remove a volume that has snapshots")
	}

	// If the volume doesn't exist, then nothing more to do.
	volPath := GetVolumeMountPath(d.name, vol.volType, vol.name)
	if !shared.PathExists(volPath) {
		return nil
	}

	// Delete the volume (and any subvolumes).
	err = d.deleteSubvolume(volPath, true)
	if err != nil {
		return err
	}

	// Although the volume snapshot directory should already be removed, lets remove it here
	// to just in case the top-level directory is left.
	err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name)
	if err != nil {
		return err
	}

	return nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *btrfs) HasVolume(vol Volume) bool {
	return genericVFSHasVolume(vol)
}

// ValidateVolume validates the supplied volume config.
func (d *btrfs) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	return d.validateVolume(vol, nil, removeUnknownKeys)
}

// UpdateVolume applies config changes to the volume.
func (d *btrfs) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	newSize, sizeChanged := changedConfig["size"]
	if sizeChanged {
		err := d.SetVolumeQuota(vol, newSize, false, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

// GetVolumeUsage returns the disk space used by the volume.
func (d *btrfs) GetVolumeUsage(vol Volume) (int64, error) {
	// Attempt to get the qgroup information.
	_, usage, err := d.getQGroup(vol.MountPath())
	if err != nil {
		if err == errBtrfsNoQuota {
			return -1, ErrNotSupported
		}

		return -1, err
	}

	return usage, nil
}

// SetVolumeQuota applies a size limit on volume.
// Does nothing if supplied with an empty/zero size for block volumes, and for filesystem volumes removes quota.
func (d *btrfs) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	// Convert to bytes.
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	// For VM block files, resize the file if needed.
	if vol.contentType == ContentTypeBlock {
		// Do nothing if size isn't specified.
		if sizeBytes <= 0 {
			return nil
		}

		rootBlockPath, err := d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}

		// Pass VolumeTypeImage as unsupported resize type, as if the image volume doesn't match the
		// requested size and allowUnsafeResize=false, this needs to be rejected back to caller as
		// ErrNotSupported so that the caller can take the appropriate action. In the case of optimized
		// image volumes, this will cause the image volume to be deleted and regenerated with the new size.
		// In other cases this is probably a bug and the operation should fail anyway.
		resized, err := ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, allowUnsafeResize, VolumeTypeImage)
		if err != nil {
			return err
		}

		// Move the GPT alt header to end of disk if needed and resize has taken place (not needed in
		// unsafe resize mode as it is expected the caller will do all necessary post resize actions
		// themselves).
		if vol.IsVMBlock() && resized && !allowUnsafeResize {
			err = d.moveGPTAltHeader(rootBlockPath)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// For non-VM block volumes, set filesystem quota.
	volPath := vol.MountPath()

	// Try to locate an existing quota group.
	qgroup, _, err := d.getQGroup(volPath)
	if err != nil && !d.state.OS.RunningInUserNS {
		// If quotas are disabled, attempt to enable them.
		if err == errBtrfsNoQuota {
			if sizeBytes <= 0 {
				// Nothing to do if the quota is being removed and we don't currently have quota.
				return nil
			}

			path := GetPoolMountPath(d.name)

			_, err = shared.RunCommand("btrfs", "quota", "enable", path)
			if err != nil {
				return err
			}

			// Try again.
			qgroup, _, err = d.getQGroup(volPath)
		}

		// If there's no qgroup, attempt to create one.
		if err == errBtrfsNoQGroup {
			// Find the volume ID.
			var output string
			output, err = shared.RunCommand("btrfs", "subvolume", "show", volPath)
			if err != nil {
				return errors.Wrap(err, "Failed to get subvol information")
			}

			id := ""
			for _, line := range strings.Split(output, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "Subvolume ID:") {
					fields := strings.Split(line, ":")
					id = strings.TrimSpace(fields[len(fields)-1])
				}
			}

			if id == "" {
				return fmt.Errorf("Failed to find subvolume id for %q", volPath)
			}

			// Create a qgroup.
			_, err = shared.RunCommand("btrfs", "qgroup", "create", fmt.Sprintf("0/%s", id), volPath)
			if err != nil {
				return err
			}

			// Try to get the qgroup again.
			qgroup, _, err = d.getQGroup(volPath)
		}

		if err != nil {
			return err
		}
	}

	// Modify the limit.
	if sizeBytes > 0 {
		// Custom handling for filesystem volume associated with a VM.
		if vol.volType == VolumeTypeVM && shared.PathExists(filepath.Join(volPath, genericVolumeDiskFile)) {
			// Get the size of the VM image.
			blockSize, err := BlockDiskSizeBytes(filepath.Join(volPath, genericVolumeDiskFile))
			if err != nil {
				return err
			}

			// Add that to the requested filesystem size (to ignore it from the quota).
			sizeBytes += blockSize
		}

		// Apply the limit.
		_, err := shared.RunCommand("btrfs", "qgroup", "limit", fmt.Sprintf("%d", sizeBytes), volPath)
		if err != nil {
			return err
		}
	} else if qgroup != "" {
		// Remove the limit.
		_, err := shared.RunCommand("btrfs", "qgroup", "limit", "none", qgroup, volPath)
		if err != nil {
			return err
		}
	}

	return nil
}

// GetVolumeDiskPath returns the location and file format of a disk volume.
func (d *btrfs) GetVolumeDiskPath(vol Volume) (string, error) {
	return genericVFSGetVolumeDiskPath(vol)
}

// ListVolumes returns a list of LXD volumes in storage pool.
func (d *btrfs) ListVolumes() ([]Volume, error) {
	return nil, ErrNotImplemented
}

// MountVolume simulates mounting a volume.
func (d *btrfs) MountVolume(vol Volume, op *operations.Operation) error {
	unlock := vol.MountLock()
	defer unlock()

	// Don't attempt to modify the permission of an existing custom volume root.
	// A user inside the instance may have modified this and we don't want to reset it on restart.
	if !shared.PathExists(vol.MountPath()) || vol.volType != VolumeTypeCustom {
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}
	}

	vol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolume() when done.
	return nil
}

// UnmountVolume simulates unmounting a volume.
// As driver doesn't have volumes to unmount it returns false indicating the volume was already unmounted.
func (d *btrfs) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock := vol.MountLock()
	defer unlock()

	vol.MountRefCountDecrement()
	return false, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *btrfs) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	return genericVFSRenameVolume(d, vol, newVolName, op)
}

// MigrateVolume sends a volume for migration.
func (d *btrfs) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error {
	// Handle simple rsync and block_and_rsync through generic.
	if volSrcArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volSrcArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
	} else if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_BTRFS {
		return ErrNotSupported
	}

	// Handle btrfs send/receive migration.
	if volSrcArgs.FinalSync {
		// This is not needed if the migration is performed using btrfs send/receive.
		return nil
	}

	// Generate restoration header, containing info on the subvolumes and how they should be restored.
	migrationHeader, err := d.restorationHeader(vol, volSrcArgs.Snapshots)
	if err != nil {
		return err
	}

	// If we haven't negotiated subvolume support, check if we have any subvolumes in source and fail,
	// otherwise we would end up not materialising all of the source's files on the target.
	if !shared.StringInSlice(migration.BTRFSFeatureMigrationHeader, volSrcArgs.MigrationType.Features) || !shared.StringInSlice(migration.BTRFSFeatureSubvolumes, volSrcArgs.MigrationType.Features) {
		for _, subVol := range migrationHeader.Subvolumes {
			if subVol.Path != string(filepath.Separator) {
				return fmt.Errorf("Subvolumes detected in source but target does not support receiving subvolumes")
			}
		}
	}

	// Send metadata migration header frame with subvolume info if we have negotiated that feature.
	if shared.StringInSlice(migration.BTRFSFeatureMigrationHeader, volSrcArgs.MigrationType.Features) {
		headerJSON, err := json.Marshal(migrationHeader)
		if err != nil {
			return errors.Wrapf(err, "Failed encoding migration header")
		}

		_, err = conn.Write(headerJSON)
		if err != nil {
			return errors.Wrapf(err, "Failed sending migration header")
		}

		err = conn.Close() //End the frame.
		if err != nil {
			return errors.Wrapf(err, "Failed closing migration header frame")
		}

		d.logger.Debug("Sent migration meta data header", log.Ctx{"name": vol.name})
	}

	// sendVolume sends a volume and its subvolumes (if negotiated subvolumes feature) to recipient.
	sendVolume := func(v Volume, sourcePrefix string, parentPrefix string) error {
		snapName := "" // Default to empty (sending main volume) from migrationHeader.Subvolumes.

		// Detect if we are sending a snapshot by comparing to main volume name.
		// We can't only use IsSnapshot() as the main vol may itself be a snapshot.
		if v.IsSnapshot() && v.name != vol.name {
			_, snapName, _ = shared.InstanceGetParentAndSnapshotName(v.name)
		}

		// Setup progress tracking.
		var wrapper *ioprogress.ProgressTracker
		if volSrcArgs.TrackProgress {
			wrapper = migration.ProgressTracker(op, "fs_progress", v.name)
		}

		sentVols := 0

		// Send volume (and any subvolumes if supported) to target.
		for _, subVolume := range migrationHeader.Subvolumes {
			if subVolume.Snapshot != snapName {
				continue // Only sending subvolumes related to snapshot name (empty for main vol).
			}

			if subVolume.Path != string(filepath.Separator) && !shared.StringInSlice(migration.BTRFSFeatureSubvolumes, volSrcArgs.MigrationType.Features) {
				continue // Skip sending subvolumes of volume if subvolumes feature not negotiated.
			}

			// Detect if parent subvolume exists, and if so use it for differential.
			parentPath := ""
			if parentPrefix != "" && btrfsIsSubVolume(filepath.Join(parentPrefix, subVolume.Path)) {
				parentPath = filepath.Join(parentPrefix, subVolume.Path)

				// Set parent subvolume readonly if needed so we can send the subvolume.
				if !BTRFSSubVolumeIsRo(parentPath) {
					err = d.setSubvolumeReadonlyProperty(parentPath, true)
					if err != nil {
						return err
					}
					defer d.setSubvolumeReadonlyProperty(parentPath, false)
				}
			}

			// Set subvolume readonly if needed so we can send it.
			sourcePath := filepath.Join(sourcePrefix, subVolume.Path)
			if !BTRFSSubVolumeIsRo(sourcePath) {
				err = d.setSubvolumeReadonlyProperty(sourcePath, true)
				if err != nil {
					return err
				}
				defer d.setSubvolumeReadonlyProperty(sourcePath, false)
			}

			d.logger.Debug("Sending subvolume", log.Ctx{"name": v.name, "source": sourcePath, "parent": parentPath, "path": subVolume.Path})
			err = d.sendSubvolume(sourcePath, parentPath, conn, wrapper)
			if err != nil {
				return errors.Wrapf(err, "Failed sending volume %v:%s", v.name, subVolume.Path)
			}
			sentVols++
		}

		// Ensure we found and sent at least root subvolume of the volume requested.
		if sentVols < 1 {
			return fmt.Errorf("No matching subvolume(s) for %q found in subvolumes list", v.name)
		}

		return nil
	}

	// Transfer the snapshots (and any subvolumes if supported) to target first.
	lastVolPath := "" // Used as parent for differential transfers.
	for _, snapName := range volSrcArgs.Snapshots {
		snapVol, _ := vol.NewSnapshot(snapName)
		err = sendVolume(snapVol, snapVol.MountPath(), lastVolPath)
		if err != nil {
			return err
		}

		lastVolPath = snapVol.MountPath()
	}

	// Get instances directory (e.g. /var/lib/lxd/storage-pools/btrfs/containers).
	instancesPath := GetVolumeMountPath(d.name, vol.volType, "")

	// Create a temporary directory which will act as the parent directory of the read-only snapshot.
	tmpVolumesMountPoint, err := ioutil.TempDir(instancesPath, "migration.")
	if err != nil {
		return errors.Wrapf(err, "Failed to create temporary directory under %q", instancesPath)
	}
	defer os.RemoveAll(tmpVolumesMountPoint)

	err = os.Chmod(tmpVolumesMountPoint, 0100)
	if err != nil {
		return errors.Wrapf(err, "Failed to chmod %q", tmpVolumesMountPoint)
	}

	// Make recursive read-only snapshot of the subvolume as writable subvolumes cannot be sent.
	migrationSendSnapshotPrefix := filepath.Join(tmpVolumesMountPoint, ".migration-send")
	err = d.snapshotSubvolume(vol.MountPath(), migrationSendSnapshotPrefix, true)
	if err != nil {
		return err
	}
	defer d.deleteSubvolume(migrationSendSnapshotPrefix, true)

	// Send main volume (and any subvolumes if supported) to target.
	return sendVolume(vol, migrationSendSnapshotPrefix, lastVolPath)
}

// BackupVolume copies a volume (and optionally its snapshots) to a specified target path.
// This driver does not support optimized backups.
func (d *btrfs) BackupVolume(vol Volume, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots []string, op *operations.Operation) error {
	// Handle the non-optimized tarballs through the generic packer.
	if !optimized {
		// Because the generic backup method will not take a consistent backup if files are being modified
		// as they are copied to the tarball, as BTRFS allows us to take a quick snapshot without impacting
		// the parent volume we do so here to ensure the backup taken is consistent.
		if vol.contentType == ContentTypeFS {
			sourcePath := vol.MountPath()
			poolPath := GetPoolMountPath(d.name)
			tmpDir, err := ioutil.TempDir(poolPath, "backup.")
			if err != nil {
				return errors.Wrapf(err, "Failed to create temporary directory under %q", poolPath)
			}
			defer os.RemoveAll(tmpDir)

			err = os.Chmod(tmpDir, 0100)
			if err != nil {
				return errors.Wrapf(err, "Failed to chmod %q", tmpDir)
			}

			// Override volume's mount path with location of snapshot so genericVFSBackupVolume reads
			// from there instead of main volume.
			vol.customMountPath = filepath.Join(tmpDir, vol.name)

			// Create the read-only snapshot.
			mountPath := vol.MountPath()
			err = d.snapshotSubvolume(sourcePath, mountPath, true)
			if err != nil {
				return err
			}

			err = d.setSubvolumeReadonlyProperty(mountPath, true)
			if err != nil {
				return err
			}

			d.logger.Debug("Created read-only backup snapshot", log.Ctx{"sourcePath": sourcePath, "path": mountPath})
			defer d.deleteSubvolume(mountPath, true)
		}

		return genericVFSBackupVolume(d, vol, tarWriter, snapshots, op)
	}

	// Optimized backup.

	if len(snapshots) > 0 {
		// Check requested snapshot match those in storage.
		err := vol.SnapshotsMatch(snapshots, op)
		if err != nil {
			return err
		}
	}

	// Generate driver restoration header.
	optimizedHeader, err := d.restorationHeader(vol, snapshots)
	if err != nil {
		return err
	}

	// Convert to YAML.
	optimizedHeaderYAML, err := yaml.Marshal(&optimizedHeader)
	if err != nil {
		return err
	}
	r := bytes.NewReader(optimizedHeaderYAML)

	indexFileInfo := instancewriter.FileInfo{
		FileName:    "backup/optimized_header.yaml",
		FileSize:    int64(len(optimizedHeaderYAML)),
		FileMode:    0644,
		FileModTime: time.Now(),
	}

	// Write to tarball.
	err = tarWriter.WriteFileFromReader(r, &indexFileInfo)
	if err != nil {
		return err
	}

	// sendToFile sends a subvolume to backup file.
	sendToFile := func(path string, parent string, fileName string) error {
		// Prepare btrfs send arguments.
		args := []string{"send"}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		args = append(args, path)

		// Create temporary file to store output of btrfs send.
		backupsPath := shared.VarPath("backups")
		tmpFile, err := ioutil.TempFile(backupsPath, fmt.Sprintf("%s_btrfs", backup.WorkingDirPrefix))
		if err != nil {
			return errors.Wrapf(err, "Failed to open temporary file for BTRFS backup")
		}
		defer tmpFile.Close()
		defer os.Remove(tmpFile.Name())

		// Write the subvolume to the file.
		d.logger.Debug("Generating optimized volume file", log.Ctx{"sourcePath": path, "parent": parent, "file": tmpFile.Name(), "name": fileName})
		err = shared.RunCommandWithFds(nil, tmpFile, "btrfs", args...)
		if err != nil {
			return err
		}

		// Get info (importantly size) of the generated file for tarball header.
		tmpFileInfo, err := os.Lstat(tmpFile.Name())
		if err != nil {
			return err
		}

		err = tarWriter.WriteFile(fileName, tmpFile.Name(), tmpFileInfo, false)
		if err != nil {
			return err
		}

		return tmpFile.Close()
	}

	// addVolume adds a volume and its subvolumes to backup file.
	addVolume := func(v Volume, sourcePrefix string, parentPrefix string, fileNamePrefix string) error {
		snapName := "" // Default to empty (sending main volume) from migrationHeader.Subvolumes.

		// Detect if we are adding a snapshot by comparing to main volume name.
		// We can't only use IsSnapshot() as the main vol may itself be a snapshot.
		if v.IsSnapshot() && v.name != vol.name {
			_, snapName, _ = shared.InstanceGetParentAndSnapshotName(v.name)
		}

		sentVols := 0

		// Add volume (and any subvolumes if supported) to backup file.
		for _, subVolume := range optimizedHeader.Subvolumes {
			if subVolume.Snapshot != snapName {
				continue // Only add subvolumes related to snapshot name (empty for main vol).
			}

			// Detect if parent subvolume exists, and if so use it for differential.
			parentPath := ""
			if parentPrefix != "" && btrfsIsSubVolume(filepath.Join(parentPrefix, subVolume.Path)) {
				parentPath = filepath.Join(parentPrefix, subVolume.Path)

				// Set parent subvolume readonly if needed so we can add the subvolume.
				if !BTRFSSubVolumeIsRo(parentPath) {
					err = d.setSubvolumeReadonlyProperty(parentPath, true)
					if err != nil {
						return err
					}
					defer d.setSubvolumeReadonlyProperty(parentPath, false)
				}
			}

			// Set subvolume readonly if needed so we can add it.
			sourcePath := filepath.Join(sourcePrefix, subVolume.Path)
			if !BTRFSSubVolumeIsRo(sourcePath) {
				err = d.setSubvolumeReadonlyProperty(sourcePath, true)
				if err != nil {
					return err
				}
				defer d.setSubvolumeReadonlyProperty(sourcePath, false)
			}

			// Default to no subvolume name for root subvolume to maintain backwards compatibility
			// with earlier optimized dump format. Although restoring this backup file on an earlier
			// system will not restore the subvolumes stored inside the backup.
			subVolName := ""
			if subVolume.Path != string(filepath.Separator) {
				// Encode the path of the subvolume (without the leading /) into the filename so
				// that we find the file from the optimized header's Path field on restore.
				subVolName = fmt.Sprintf("_%s", PathNameEncode(strings.TrimPrefix(subVolume.Path, string(filepath.Separator))))
			}

			fileName := fmt.Sprintf("%s%s.bin", fileNamePrefix, subVolName)
			err = sendToFile(sourcePath, parentPath, filepath.Join("backup", fileName))
			if err != nil {
				return errors.Wrapf(err, "Failed adding volume %v:%s", v.name, subVolume.Path)
			}

			sentVols++
		}

		// Ensure we found and sent at least root subvolume of the volume requested.
		if sentVols < 1 {
			return fmt.Errorf("No matching subvolume(s) for %q found in subvolumes list", v.name)
		}

		return nil
	}

	// Backup snapshots if populated.
	lastVolPath := "" // Used as parent for differential exports.
	for _, snapName := range snapshots {
		snapVol, _ := vol.NewSnapshot(snapName)

		// Make a binary btrfs backup.
		snapDir := "snapshots"
		fileName := snapName
		if vol.volType == VolumeTypeVM {
			snapDir = "virtual-machine-snapshots"
			if vol.contentType == ContentTypeFS {
				fileName = fmt.Sprintf("%s-config", snapName)
			}
		} else if vol.volType == VolumeTypeCustom {
			snapDir = "volume-snapshots"
		}

		fileNamePrefix := filepath.Join(snapDir, fileName)
		err := addVolume(snapVol, snapVol.MountPath(), lastVolPath, fileNamePrefix)
		if err != nil {
			return err
		}

		lastVolPath = snapVol.MountPath()
	}

	// Make a temporary copy of the instance.
	sourceVolume := vol.MountPath()
	instancesPath := GetVolumeMountPath(d.name, vol.volType, "")

	tmpInstanceMntPoint, err := ioutil.TempDir(instancesPath, "backup.")
	if err != nil {
		return errors.Wrapf(err, "Failed to create temporary directory under %q", instancesPath)
	}
	defer os.RemoveAll(tmpInstanceMntPoint)

	err = os.Chmod(tmpInstanceMntPoint, 0100)
	if err != nil {
		return errors.Wrapf(err, "Failed to chmod %q", tmpInstanceMntPoint)
	}

	// Create the read-only snapshot.
	targetVolume := fmt.Sprintf("%s/.backup", tmpInstanceMntPoint)
	err = d.snapshotSubvolume(sourceVolume, targetVolume, true)
	if err != nil {
		return err
	}
	defer d.deleteSubvolume(targetVolume, true)

	err = d.setSubvolumeReadonlyProperty(targetVolume, true)
	if err != nil {
		return err
	}

	// Dump the instance to a file.
	fileNamePrefix := "container"
	if vol.volType == VolumeTypeVM {
		if vol.contentType == ContentTypeFS {
			fileNamePrefix = "virtual-machine-config"
		} else {
			fileNamePrefix = "virtual-machine"
		}
	} else if vol.volType == VolumeTypeCustom {
		fileNamePrefix = "volume"
	}

	err = addVolume(vol, targetVolume, lastVolPath, fileNamePrefix)
	if err != nil {
		return err
	}

	// Ensure snapshot sub volumes are removed.
	err = d.deleteSubvolume(targetVolume, true)
	if err != nil {
		return err
	}

	return nil
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *btrfs) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	srcPath := GetVolumeMountPath(d.name, snapVol.volType, parentName)
	snapPath := snapVol.MountPath()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	err = d.snapshotSubvolume(srcPath, snapPath, true)
	if err != nil {
		return err
	}

	err = d.setSubvolumeReadonlyProperty(snapPath, true)
	if err != nil {
		return err
	}

	// Set any subvolumes that were readonly in the source also readonly in the snapshot.
	srcVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, snapVol.config, snapVol.poolConfig)
	subVols, err := d.getSubvolumesMetaData(srcVol)
	if err != nil {
		return err
	}

	for _, subVol := range subVols {
		if subVol.Readonly {
			err = d.setSubvolumeReadonlyProperty(filepath.Join(snapPath, subVol.Path), true)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device. The volName and snapshotName
// must be bare names and should not be in the format "volume/snapshot".
func (d *btrfs) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	snapPath := snapVol.MountPath()

	// Delete the snapshot.
	err := d.deleteSubvolume(snapPath, true)
	if err != nil {
		return err
	}

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	err = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	return nil
}

// MountVolumeSnapshot sets up a read-only mount on top of the snapshot to avoid accidental modifications.
func (d *btrfs) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	snapPath := snapVol.MountPath()

	// Don't attempt to modify the permission of an existing custom volume root.
	// A user inside the instance may have modified this and we don't want to reset it on restart.
	if !shared.PathExists(snapPath) || snapVol.volType != VolumeTypeCustom {
		err := snapVol.EnsureMountPath()
		if err != nil {
			return false, err
		}
	}

	return mountReadOnly(snapPath, snapPath)
}

// UnmountVolumeSnapshot removes the read-only mount placed on top of a snapshot.
func (d *btrfs) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	snapPath := snapVol.MountPath()
	return forceUnmount(snapPath)
}

// VolumeSnapshots returns a list of snapshots for the volume (in no particular order).
func (d *btrfs) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	return genericVFSVolumeSnapshots(d, vol, op)
}

// RestoreVolume restores a volume from a snapshot.
func (d *btrfs) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	srcVol := NewVolume(d, d.name, vol.volType, vol.contentType, GetSnapshotVolumeName(vol.name, snapshotName), vol.config, vol.poolConfig)

	// Scan source for subvolumes (so we can apply the readonly properties on the restored snapshot).
	subVols, err := d.getSubvolumesMetaData(srcVol)
	if err != nil {
		return err
	}

	target := vol.MountPath()

	// Create a backup so we can revert.
	backupSubvolume := fmt.Sprintf("%s%s", target, tmpVolSuffix)
	err = os.Rename(target, backupSubvolume)
	if err != nil {
		return errors.Wrapf(err, "Failed to rename %q to %q", target, backupSubvolume)
	}
	revert.Add(func() { os.Rename(backupSubvolume, target) })

	// Restore the snapshot.
	err = d.snapshotSubvolume(srcVol.MountPath(), target, true)
	if err != nil {
		return err
	}
	revert.Add(func() { d.deleteSubvolume(target, true) })

	// Restore readonly property on subvolumes in reverse order (except root which should be left writable).
	subVolCount := len(subVols)
	for i := range subVols {
		i = subVolCount - 1 - i
		subVol := subVols[i]
		if subVol.Readonly && subVol.Path != string(filepath.Separator) {
			targetSubVolPath := filepath.Join(target, subVol.Path)
			err = d.setSubvolumeReadonlyProperty(targetSubVolPath, true)
			if err != nil {
				return err
			}
		}
	}

	revert.Success()

	// Remove the backup subvolume.
	return d.deleteSubvolume(backupSubvolume, true)
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *btrfs) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	return genericVFSRenameVolumeSnapshot(d, snapVol, newSnapshotName, op)
}
