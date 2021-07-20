package drivers

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/instancewriter"
	"github.com/lxc/lxd/shared/ioprogress"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/units"
	"github.com/lxc/lxd/shared/validate"
)

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied
// filler function.
func (d *zfs) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	// Revert handling
	revert := revert.New()
	defer revert.Fail()

	if vol.contentType == ContentTypeFS {
		// Create mountpoint.
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}

		revert.Add(func() { os.Remove(vol.MountPath()) })
	}

	// Look for previously deleted images.
	if vol.volType == VolumeTypeImage && d.checkDataset(d.dataset(vol, true)) {
		canRestore := true

		// For block volumes check if the cached image volume is larger than the current pool volume.size
		// setting (if so we won't be able to resize the snapshot to that the smaller size later).
		if vol.contentType == ContentTypeBlock {
			volSize, err := d.getDatasetProperty(d.dataset(vol, true), "volsize")
			if err != nil {
				return err
			}

			volSizeBytes, err := strconv.ParseInt(volSize, 10, 64)
			if err != nil {
				return err
			}

			poolVolSize := defaultBlockSize
			if vol.poolConfig["volume.size"] != "" {
				poolVolSize = vol.poolConfig["volume.size"]
			}

			poolVolSizeBytes, err := units.ParseByteSizeString(poolVolSize)
			if err != nil {
				return err
			}

			// Round to block boundary.
			poolVolSizeBytes = roundVolumeBlockFileSizeBytes(poolVolSizeBytes)

			// If the cached volume size is different than the pool volume size, then we can't use the
			// deleted cached image volume and instead we will rename it to a random UUID so it can't
			// be restored in the future and a new cached image volume will be created instead.
			if volSizeBytes != poolVolSizeBytes {
				d.logger.Debug("Renaming deleted cached image volume so that regeneration is used", "fingerprint", vol.Name())
				randomVol := NewVolume(d, d.name, vol.volType, vol.contentType, uuid.NewRandom().String(), vol.config, vol.poolConfig)

				_, err := shared.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(vol, true), d.dataset(randomVol, true))
				if err != nil {
					return err
				}

				if vol.IsVMBlock() {
					fsVol := vol.NewVMBlockFilesystemVolume()
					randomFsVol := randomVol.NewVMBlockFilesystemVolume()

					_, err := shared.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(fsVol, true), d.dataset(randomFsVol, true))
					if err != nil {
						return err
					}
				}

				// We have renamed the deleted cached image volume, so we don't want to try and
				// restore it.
				canRestore = false
			}
		}

		// Restore the image.
		if canRestore {
			d.logger.Debug("Restoring previously deleted cached image volume", "fingerprint", vol.Name())
			_, err := shared.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(vol, true), d.dataset(vol, false))
			if err != nil {
				return err
			}

			if vol.IsVMBlock() {
				fsVol := vol.NewVMBlockFilesystemVolume()

				_, err := shared.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(fsVol, true), d.dataset(fsVol, false))
				if err != nil {
					return err
				}
			}

			revert.Success()
			return nil
		}
	}

	// After this point we'll have a volume, so setup revert.
	revert.Add(func() { d.DeleteVolume(vol, op) })

	if vol.contentType == ContentTypeFS {
		// Create the filesystem dataset.
		err := d.createDataset(d.dataset(vol, false), fmt.Sprintf("mountpoint=%s", vol.MountPath()), "canmount=noauto")
		if err != nil {
			return err
		}

		// Apply the size limit.
		err = d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
		if err != nil {
			return err
		}
	} else {
		sizeBytes, err := units.ParseByteSizeString(vol.ConfigSize())
		if err != nil {
			return err
		}

		// Use volmode=none so volume is invisible until mounted.
		opts := []string{"volmode=none"}

		loopPath := loopFilePath(d.name)
		if d.config["source"] == loopPath {
			// Create the volume dataset with sync disabled (to avoid kernel lockups when using a disk based pool).
			opts = append(opts, "sync=disabled")
		}

		// Create the volume dataset.
		err = d.createVolume(d.dataset(vol, false), sizeBytes, opts...)
		if err != nil {
			return err
		}
	}

	// For VM images, create a filesystem volume too.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.CreateVolume(fsVol, nil, op)
		if err != nil {
			return err
		}

		revert.Add(func() { d.DeleteVolume(fsVol, op) })
	}

	err := vol.MountTask(func(mountPath string, op *operations.Operation) error {
		// Run the volume filler function if supplied.
		if filler != nil && filler.Fill != nil {
			var err error
			var devPath string

			if vol.contentType == ContentTypeBlock {
				// Get the device path.
				devPath, err = d.GetVolumeDiskPath(vol)
				if err != nil {
					return err
				}
			}

			// Run the filler.
			err = d.runFiller(vol, devPath, filler)
			if err != nil {
				return err
			}

			// Move the GPT alt header to end of disk if needed.
			if vol.IsVMBlock() {
				err = d.moveGPTAltHeader(devPath)
				if err != nil {
					return err
				}
			}
		}

		if vol.contentType == ContentTypeFS {
			// Run EnsureMountPath again after mounting and filling to ensure the mount directory has
			// the correct permissions set.
			err := vol.EnsureMountPath()
			if err != nil {
				return err
			}
		}

		return nil
	}, op)
	if err != nil {
		return err
	}

	// Setup snapshot and unset mountpoint on image.
	if vol.volType == VolumeTypeImage {
		// Create snapshot of the main dataset.
		_, err := shared.RunCommand("zfs", "snapshot", fmt.Sprintf("%s@readonly", d.dataset(vol, false)))
		if err != nil {
			return err
		}

		if vol.contentType == ContentTypeBlock {
			// Re-create the FS config volume's readonly snapshot now that the filler function has run and unpacked into both config and block volumes.
			fsVol := NewVolume(d, d.name, vol.volType, ContentTypeFS, vol.name, vol.config, vol.poolConfig)

			_, err := shared.RunCommand("zfs", "destroy", fmt.Sprintf("%s@readonly", d.dataset(fsVol, false)))
			if err != nil {
				return err
			}

			_, err = shared.RunCommand("zfs", "snapshot", fmt.Sprintf("%s@readonly", d.dataset(fsVol, false)))
			if err != nil {
				return err
			}
		}
	}

	// All done.
	revert.Success()

	return nil
}

// CreateVolumeFromBackup re-creates a volume from its exported state.
func (d *zfs) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
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

	// Define function to unpack a volume from a backup tarball file.
	unpackVolume := func(v Volume, r io.ReadSeeker, unpacker []string, srcFile string, target string) error {
		d.Logger().Debug("Unpacking optimized volume", log.Ctx{"source": srcFile, "target": target})
		tr, cancelFunc, err := shared.CompressedTarReader(context.Background(), r, unpacker)
		if err != nil {
			return err
		}
		defer cancelFunc()

		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break // End of archive.
			}
			if err != nil {
				return err
			}

			if hdr.Name == srcFile {
				// Extract the backup.
				if v.ContentType() == ContentTypeBlock {
					err = shared.RunCommandWithFds(tr, nil, "zfs", "receive", "-F", target)
				} else {
					err = shared.RunCommandWithFds(tr, nil, "zfs", "receive", "-x", "mountpoint", "-F", target)
				}

				if err != nil {
					return err
				}

				cancelFunc()
				return nil
			}
		}

		return fmt.Errorf("Could not find %q", srcFile)
	}

	var postHook VolumePostHook

	// Create a list of actual volumes to unpack.
	var vols []Volume
	if vol.IsVMBlock() {
		vols = append(vols, vol.NewVMBlockFilesystemVolume())
	}

	vols = append(vols, vol)

	for _, v := range vols {
		// Find the compression algorithm used for backup source data.
		srcData.Seek(0, 0)
		_, _, unpacker, err := shared.DetectCompressionFile(srcData)
		if err != nil {
			return nil, nil, err
		}

		if len(srcBackup.Snapshots) > 0 {
			// Create new snapshots directory.
			err := createParentSnapshotDirIfMissing(d.name, v.volType, v.name)
			if err != nil {
				return nil, nil, err
			}
		}

		// Restore backups from oldest to newest.
		for _, snapName := range srcBackup.Snapshots {
			prefix := "snapshots"
			fileName := fmt.Sprintf("%s.bin", snapName)
			if v.volType == VolumeTypeVM {
				prefix = "virtual-machine-snapshots"
				if v.contentType == ContentTypeFS {
					fileName = fmt.Sprintf("%s-config.bin", snapName)
				}
			} else if v.volType == VolumeTypeCustom {
				prefix = "volume-snapshots"
			}

			srcFile := fmt.Sprintf("backup/%s/%s", prefix, fileName)
			dstSnapshot := fmt.Sprintf("%s@snapshot-%s", d.dataset(v, false), snapName)
			err = unpackVolume(v, srcData, unpacker, srcFile, dstSnapshot)
			if err != nil {
				return nil, nil, err
			}
		}

		// Extract main volume.
		fileName := "container.bin"
		if v.volType == VolumeTypeVM {
			if v.contentType == ContentTypeFS {
				fileName = "virtual-machine-config.bin"
			} else {
				fileName = "virtual-machine.bin"
			}
		} else if v.volType == VolumeTypeCustom {
			fileName = "volume.bin"
		}

		err = unpackVolume(v, srcData, unpacker, fmt.Sprintf("backup/%s", fileName), d.dataset(v, false))
		if err != nil {
			return nil, nil, err
		}

		// Strip internal snapshots.
		entries, err := d.getDatasets(d.dataset(v, false))
		if err != nil {
			return nil, nil, err
		}

		// Filter only the snapshots.
		for _, entry := range entries {
			if strings.HasPrefix(entry, "@snapshot-") {
				continue
			}

			if strings.HasPrefix(entry, "@") {
				_, err := shared.RunCommand("zfs", "destroy", fmt.Sprintf("%s%s", d.dataset(v, false), entry))
				if err != nil {
					return nil, nil, err
				}
			}
		}

		// Re-apply the base mount options.
		if v.contentType == ContentTypeFS {
			err := d.setDatasetProperties(d.dataset(v, false), fmt.Sprintf("mountpoint=%s", v.MountPath()), "canmount=noauto")
			if err != nil {
				return nil, nil, err
			}
		}

		// Only mount instance filesystem volumes for backup.yaml access.
		if v.volType != VolumeTypeCustom && v.contentType != ContentTypeBlock {
			// The import requires a mounted volume, so mount it and have it unmounted as a post hook.
			err = d.MountVolume(v, op)
			if err != nil {
				return nil, nil, err
			}
			revert.Add(func() { d.UnmountVolume(v, false, op) })

			postHook = func(postVol Volume) error {
				_, err := d.UnmountVolume(postVol, false, op)
				return err
			}
		}
	}

	revertExternal := revert.Clone() // Clone before calling revert.Success() so we can return the Fail func.

	revert.Success()
	return postHook, revertExternal.Fail, nil
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *zfs) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, op *operations.Operation) error {
	// Revert handling
	revert := revert.New()
	defer revert.Fail()

	if vol.contentType == ContentTypeFS {
		// Create mountpoint.
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}

		revert.Add(func() { os.Remove(vol.MountPath()) })
	}

	// For VMs, also copy the filesystem dataset.
	if vol.IsVMBlock() {
		// For VMs, also copy the filesystem volume.
		srcFSVol := srcVol.NewVMBlockFilesystemVolume()
		fsVol := vol.NewVMBlockFilesystemVolume()

		err := d.CreateVolumeFromCopy(fsVol, srcFSVol, copySnapshots, op)
		if err != nil {
			return err
		}

		// Delete on revert.
		revert.Add(func() { d.DeleteVolume(fsVol, op) })
	}

	// Retrieve snapshots on the source.
	snapshots := []string{}
	if !srcVol.IsSnapshot() && copySnapshots {
		var err error
		snapshots, err = d.VolumeSnapshots(srcVol, op)
		if err != nil {
			return err
		}
	}

	var srcSnapshot string
	if srcVol.volType == VolumeTypeImage {
		srcSnapshot = fmt.Sprintf("%s@readonly", d.dataset(srcVol, false))
	} else if srcVol.IsSnapshot() {
		srcSnapshot = d.dataset(srcVol, false)
	} else {
		// Create a new snapshot for copy.
		srcSnapshot = fmt.Sprintf("%s@copy-%s", d.dataset(srcVol, false), uuid.NewRandom().String())

		_, err := shared.RunCommand("zfs", "snapshot", srcSnapshot)
		if err != nil {
			return err
		}

		// If using "zfs.clone_copy" delete the snapshot at the end.
		if (d.config["zfs.clone_copy"] != "" && !shared.IsTrue(d.config["zfs.clone_copy"])) || len(snapshots) > 0 {
			// Delete the snapshot at the end.
			defer shared.RunCommand("zfs", "destroy", srcSnapshot)
		} else {
			// Delete the snapshot on revert.
			revert.Add(func() {
				shared.RunCommand("zfs", "destroy", srcSnapshot)
			})
		}
	}

	// If zfs.clone_copy is disabled or source volume has snapshots, then use full copy mode.
	if (d.config["zfs.clone_copy"] != "" && !shared.IsTrue(d.config["zfs.clone_copy"])) || len(snapshots) > 0 {
		snapName := strings.SplitN(srcSnapshot, "@", 2)[1]

		// Send/receive the snapshot.
		var sender *exec.Cmd
		var receiver *exec.Cmd
		if vol.ContentType() == ContentTypeBlock {
			receiver = exec.Command("zfs", "receive", d.dataset(vol, false))
		} else {
			receiver = exec.Command("zfs", "receive", "-x", "mountpoint", d.dataset(vol, false))
		}

		// Handle transferring snapshots.
		if len(snapshots) > 0 {
			sender = exec.Command("zfs", "send", "-R", srcSnapshot)
		} else {
			if d.config["zfs.clone_copy"] == "rebase" {
				var err error
				origin := d.dataset(srcVol, false)
				for {
					fields := strings.SplitN(origin, "@", 2)

					// If the origin is a @readonly snapshot under a /images/ path (/images or deleted/images), we're done.
					if len(fields) > 1 && strings.Contains(fields[0], "/images/") && fields[1] == "readonly" {
						break
					}

					origin, err = d.getDatasetProperty(origin, "origin")
					if err != nil {
						return err
					}

					if origin == "" || origin == "-" {
						origin = ""
						break
					}
				}

				if origin != "" && origin != srcSnapshot {
					sender = exec.Command("zfs", "send", "-i", origin, srcSnapshot)
				} else {
					sender = exec.Command("zfs", "send", srcSnapshot)
				}
			} else {
				sender = exec.Command("zfs", "send", srcSnapshot)
			}
		}

		// Configure the pipes.
		receiver.Stdin, _ = sender.StdoutPipe()
		receiver.Stdout = os.Stdout
		receiver.Stderr = os.Stderr

		// Run the transfer.
		err := receiver.Start()
		if err != nil {
			return err
		}

		err = sender.Run()
		if err != nil {
			return err
		}

		err = receiver.Wait()
		if err != nil {
			return err
		}

		// Delete the snapshot.
		_, err = shared.RunCommand("zfs", "destroy", fmt.Sprintf("%s@%s", d.dataset(vol, false), snapName))
		if err != nil {
			return err
		}

		// Cleanup unexpected snapshots.
		if len(snapshots) > 0 {
			children, err := d.getDatasets(d.dataset(vol, false))
			if err != nil {
				return err
			}

			for _, entry := range children {
				// Check if expected snapshot.
				if strings.HasPrefix(entry, "@snapshot-") {
					name := strings.TrimPrefix(entry, "@snapshot-")
					if shared.StringInSlice(name, snapshots) {
						continue
					}
				}

				// Delete the rest.
				_, err := shared.RunCommand("zfs", "destroy", fmt.Sprintf("%s%s", d.dataset(vol, false), entry))
				if err != nil {
					return err
				}
			}
		}
	} else {
		// Perform volume clone.
		args := []string{"clone"}

		if vol.contentType == ContentTypeBlock {
			// Use volmode=none so volume is invisible until mounted.
			args = append(args, "-o", "volmode=none")
		}

		args = append(args, srcSnapshot, d.dataset(vol, false))

		// Clone the snapshot.
		_, err := shared.RunCommand("zfs", args...)
		if err != nil {
			return err
		}

		// Delete on revert.
		revert.Add(func() { d.DeleteVolume(vol, op) })
	}

	// Apply the properties.
	if vol.contentType == ContentTypeFS {
		err := d.setDatasetProperties(d.dataset(vol, false), fmt.Sprintf("mountpoint=%s", vol.MountPath()), "canmount=noauto")
		if err != nil {
			return err
		}

		// Mount the volume and ensure the permissions are set correctly inside the mounted volume.
		err = vol.MountTask(func(_ string, _ *operations.Operation) error {
			return vol.EnsureMountPath()
		}, op)
		if err != nil {
			return err
		}
	}

	// Resize volume to the size specified. Only uses volume "size" property and does not use pool/defaults
	// to give the caller more control over the size being used.
	err := d.SetVolumeQuota(vol, vol.config["size"], false, op)
	if err != nil {
		return err
	}

	// All done.
	revert.Success()
	return nil
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *zfs) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	// Handle simple rsync and block_and_rsync through generic.
	if volTargetArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volTargetArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		return genericVFSCreateVolumeFromMigration(d, nil, vol, conn, volTargetArgs, preFiller, op)
	} else if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_ZFS {
		return ErrNotSupported
	}

	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.CreateVolumeFromMigration(fsVol, conn, volTargetArgs, preFiller, op)
		if err != nil {
			return err
		}
	}

	// Handle zfs send/receive migration.
	if len(volTargetArgs.Snapshots) > 0 {
		// Create the parent directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}

		// Transfer the snapshots.
		for _, snapName := range volTargetArgs.Snapshots {
			fullSnapshotName := GetSnapshotVolumeName(vol.name, snapName)
			wrapper := migration.ProgressWriter(op, "fs_progress", fullSnapshotName)

			err = d.receiveDataset(vol, conn, wrapper)
			if err != nil {
				return err
			}
		}
	}

	// Transfer the main volume.
	wrapper := migration.ProgressWriter(op, "fs_progress", vol.name)
	err := d.receiveDataset(vol, conn, wrapper)
	if err != nil {
		return err
	}

	// Strip internal snapshots.
	entries, err := d.getDatasets(d.dataset(vol, false))
	if err != nil {
		return err
	}

	// keepDataset returns whether to keep the data set or delete it. Data sets that are non-snapshots or
	// snapshots that match the requested snapshots in volTargetArgs.Snapshots are kept. Any other snapshot
	// data sets should be removed.
	keepDataset := func(dataSetName string) bool {
		// Keep non-snapshot data sets and snapshots that don't have the LXD snapshot prefix indicator.
		dataSetSnapshotPrefix := "@snapshot-"
		if !strings.HasPrefix(dataSetName, "@") || !strings.HasPrefix(dataSetName, dataSetSnapshotPrefix) {
			return false
		}

		// Check if snapshot data set matches one of the requested snapshots in volTargetArgs.Snapshots.
		// If so, then keep it, otherwise request it be removed.
		entrySnapName := strings.TrimPrefix(dataSetName, dataSetSnapshotPrefix)
		for _, snapName := range volTargetArgs.Snapshots {
			if entrySnapName == snapName {
				return true // Keep snapshot data set if present in the requested snapshots list.
			}
		}

		return false // Delete any other snapshot data sets that have been transferred.
	}

	// Remove any snapshots that were transferred but are not needed.
	for _, entry := range entries {
		if !keepDataset(entry) {
			_, err := shared.RunCommand("zfs", "destroy", fmt.Sprintf("%s%s", d.dataset(vol, false), entry))
			if err != nil {
				return err
			}
		}
	}

	if vol.contentType == ContentTypeFS {
		// Create mountpoint.
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}

		// Re-apply the base mount options.
		err = d.setDatasetProperties(d.dataset(vol, false), fmt.Sprintf("mountpoint=%s", vol.MountPath()), "canmount=noauto")
		if err != nil {
			return err
		}

		// Apply the size limit.
		err = d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// RefreshVolume updates an existing volume to match the state of another.
func (d *zfs) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, op)
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
func (d *zfs) DeleteVolume(vol Volume, op *operations.Operation) error {
	// Check that we have a dataset to delete.
	if d.checkDataset(d.dataset(vol, false)) {
		// Handle clones.
		clones, err := d.getClones(d.dataset(vol, false))
		if err != nil {
			return err
		}

		if len(clones) > 0 {
			// Move to the deleted path.
			_, err := shared.RunCommand("/proc/self/exe", "forkzfs", "--", "rename", d.dataset(vol, false), d.dataset(vol, true))
			if err != nil {
				return err
			}
		} else {
			err := d.deleteDatasetRecursive(d.dataset(vol, false))
			if err != nil {
				return err
			}
		}
	}

	if vol.contentType == ContentTypeFS {
		// Delete the mountpoint if present.
		err := os.Remove(vol.MountPath())
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "Failed to remove '%s'", vol.MountPath())
		}

		// Delete the snapshot storage.
		err = os.RemoveAll(GetVolumeSnapshotDir(d.name, vol.volType, vol.name))
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "Failed to remove '%s'", GetVolumeSnapshotDir(d.name, vol.volType, vol.name))
		}
	}

	// For VMs, also delete the filesystem dataset.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.DeleteVolume(fsVol, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *zfs) HasVolume(vol Volume) bool {
	// Check if the dataset exists.
	return d.checkDataset(d.dataset(vol, false))
}

// ValidateVolume validates the supplied volume config.
func (d *zfs) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	rules := map[string]func(value string) error{
		"zfs.remove_snapshots": validate.Optional(validate.IsBool),
		"zfs.use_refquota":     validate.Optional(validate.IsBool),
	}

	return d.validateVolume(vol, rules, removeUnknownKeys)
}

// UpdateVolume applies config changes to the volume.
func (d *zfs) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	for k, v := range changedConfig {
		if k == "size" {
			return d.SetVolumeQuota(vol, v, false, nil)
		}

		if k == "zfs.use_refquota" {
			// Get current value.
			cur := vol.ExpandedConfig("zfs.use_refquota")

			// Get current size.
			size := changedConfig["size"]
			if size == "" {
				size = vol.ExpandedConfig("size")
			}

			// Skip if no current quota.
			if size == "" {
				continue
			}

			// Skip if no change in effective value.
			if shared.IsTrue(v) == shared.IsTrue(cur) {
				continue
			}

			// Set new quota by temporarily modifying the volume config.
			vol.config["zfs.use_refquota"] = v
			err := d.SetVolumeQuota(vol, size, false, nil)
			vol.config["zfs.use_refquota"] = cur
			if err != nil {
				return err
			}

			// Unset old quota.
			err = d.SetVolumeQuota(vol, "", false, nil)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// GetVolumeUsage returns the disk space used by the volume.
func (d *zfs) GetVolumeUsage(vol Volume) (int64, error) {
	// Determine what key to use.
	key := "used"

	// If volume isn't snapshot then we can take into account the zfs.use_refquota setting.
	// Snapshots should also use the "used" ZFS property because the snapshot usage size represents the CoW
	// usage not the size of the snapshot volume.
	if !vol.IsSnapshot() {
		if shared.IsTrue(vol.ExpandedConfig("zfs.use_refquota")) {
			key = "referenced"
		}

		// Shortcut for mounted refquota filesystems.
		if key == "referenced" && vol.contentType == ContentTypeFS && shared.IsMountPoint(vol.MountPath()) {
			var stat unix.Statfs_t
			err := unix.Statfs(vol.MountPath(), &stat)
			if err != nil {
				return -1, err
			}

			return int64(stat.Blocks-stat.Bfree) * int64(stat.Bsize), nil
		}
	}

	// Get the current value.
	value, err := d.getDatasetProperty(d.dataset(vol, false), key)
	if err != nil {
		return -1, err
	}

	// Convert to int.
	valueInt, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return -1, err
	}

	return valueInt, nil
}

// SetVolumeQuota sets the quota on the volume.
// Does nothing if supplied with an empty/zero size for block volumes, and for filesystem volumes removes quota.
func (d *zfs) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	// Convert to bytes.
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	// Handle volume datasets.
	if vol.contentType == ContentTypeBlock {
		// Do nothing if size isn't specified.
		if sizeBytes <= 0 {
			return nil
		}

		sizeBytes = roundVolumeBlockFileSizeBytes(sizeBytes)

		oldSizeBytesStr, err := d.getDatasetProperty(d.dataset(vol, false), "volsize")
		if err != nil {
			return err
		}

		oldVolSizeBytesInt, err := strconv.ParseInt(oldSizeBytesStr, 10, 64)
		if err != nil {
			return err
		}
		oldVolSizeBytes := int64(oldVolSizeBytesInt)

		if oldVolSizeBytes == sizeBytes {
			return nil
		}

		// Block image volumes cannot be resized because they have a readonly snapshot that doesn't get
		// updated when the volume's size is changed, and this is what instances are created from.
		// During initial volume fill allowUnsafeResize is enabled because snapshot hasn't been taken yet.
		if !allowUnsafeResize && vol.volType == VolumeTypeImage {
			return ErrNotSupported
		}

		// Only perform pre-resize checks if we are not in "unsafe" mode.
		// In unsafe mode we expect the caller to know what they are doing and understand the risks.
		if !allowUnsafeResize {
			if sizeBytes < oldVolSizeBytes {
				return errors.Wrap(ErrCannotBeShrunk, "Block volumes cannot be shrunk")
			}

			if vol.MountInUse() {
				return ErrInUse // We don't allow online resizing of block volumes.
			}
		}

		err = d.setDatasetProperties(d.dataset(vol, false), fmt.Sprintf("volsize=%d", sizeBytes))
		if err != nil {
			return err
		}

		// Move the VM GPT alt header to end of disk if needed (not needed in unsafe resize mode as
		// it is expected the caller will do all necessary post resize actions themselves).
		if vol.IsVMBlock() && !allowUnsafeResize {
			err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
				devPath, err := d.GetVolumeDiskPath(vol)
				if err != nil {
					return err
				}

				return d.moveGPTAltHeader(devPath)
			}, op)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// Handle filesystem datasets.
	key := "quota"
	if shared.IsTrue(vol.ExpandedConfig("zfs.use_refquota")) {
		key = "refquota"
	}

	value := fmt.Sprintf("%d", sizeBytes)
	if sizeBytes == 0 {
		value = "none"
	}

	err = d.setDatasetProperties(d.dataset(vol, false), fmt.Sprintf("%s=%s", key, value))
	if err != nil {
		return err
	}

	return nil
}

// GetVolumeDiskPath returns the location of a root disk block device.
func (d *zfs) GetVolumeDiskPath(vol Volume) (string, error) {
	// Shortcut for udev.
	if shared.PathExists(filepath.Join("/dev/zvol", d.dataset(vol, false))) {
		return filepath.Join("/dev/zvol", d.dataset(vol, false)), nil
	}

	// Locate zvol_id.
	zvolid := "/lib/udev/zvol_id"
	if !shared.PathExists(zvolid) {
		var err error

		zvolid, err = exec.LookPath("zvol_id")
		if err != nil {
			return "", err
		}
	}

	// List all the device nodes.
	entries, err := ioutil.ReadDir("/dev")
	if err != nil {
		return "", errors.Wrap(err, "Failed to read /dev")
	}

	for _, entry := range entries {
		entryName := entry.Name()

		// Ignore non-zvol devices.
		if !strings.HasPrefix(entryName, "zd") {
			continue
		}

		if strings.Contains(entryName, "p") {
			continue
		}

		// Resolve the dataset path.
		entryPath := filepath.Join("/dev", entryName)
		output, err := shared.RunCommand(zvolid, entryPath)
		if err != nil {
			continue
		}

		if strings.TrimSpace(output) == d.dataset(vol, false) {
			return entryPath, nil
		}
	}

	return "", fmt.Errorf("Could not locate a zvol for %s", d.dataset(vol, false))
}

// ListVolumes returns a list of LXD volumes in storage pool.
func (d *zfs) ListVolumes() ([]Volume, error) {
	return nil, ErrNotImplemented
}

// MountVolume mounts a volume and increments ref counter. Please call UnmountVolume() when done with the volume.
func (d *zfs) MountVolume(vol Volume, op *operations.Operation) error {
	unlock := vol.MountLock()
	defer unlock()

	revert := revert.New()
	defer revert.Fail()

	dataset := d.dataset(vol, false)

	// Check if filesystem volume already mounted.
	if vol.contentType == ContentTypeFS {
		mountPath := vol.MountPath()
		if !shared.IsMountPoint(mountPath) {
			err := vol.EnsureMountPath()
			if err != nil {
				return err
			}

			// Mount the dataset.
			_, err = shared.RunCommand("zfs", "mount", dataset)
			if err != nil {
				return err
			}

			d.logger.Debug("Mounted ZFS dataset", log.Ctx{"dev": dataset, "path": mountPath})
		}
	} else if vol.contentType == ContentTypeBlock {
		// For block devices, we make them appear.
		// Check if already active.
		current, err := d.getDatasetProperty(dataset, "volmode")
		if err != nil {
			return err
		}

		if current != "dev" {
			// Activate.
			err = d.setDatasetProperties(dataset, "volmode=dev")
			if err != nil {
				return err
			}
			revert.Add(func() { d.setDatasetProperties(dataset, "volmode=none") })

			// Wait half a second to give udev a chance to kick in.
			time.Sleep(500 * time.Millisecond)

			d.logger.Debug("Activated ZFS volume", log.Ctx{"dev": dataset})
		}

		if vol.IsVMBlock() {
			// For VMs, also mount the filesystem dataset.
			fsVol := vol.NewVMBlockFilesystemVolume()
			err = d.MountVolume(fsVol, op)
			if err != nil {
				return err
			}
		}
	}

	vol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolume() when done.
	revert.Success()
	return nil
}

// UnmountVolume unmounts volume if mounted and not in use. Returns true if this unmounted the volume.
// keepBlockDev indicates if backing block device should be not be deactivated when volume is unmounted.
func (d *zfs) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock := vol.MountLock()
	defer unlock()

	ourUnmount := false
	dataset := d.dataset(vol, false)
	refCount := vol.MountRefCountDecrement()

	if vol.contentType == ContentTypeFS {
		// Check if mounted.
		mountPath := vol.MountPath()
		if shared.IsMountPoint(mountPath) {
			if refCount > 0 {
				d.logger.Debug("Skipping unmount as in use", "refCount", refCount)
				return false, ErrInUse
			}

			// Unmount the dataset.
			err := TryUnmount(mountPath, 0)
			if err != nil {
				return false, err
			}

			d.logger.Debug("Unmounted ZFS dataset", log.Ctx{"dev": dataset, "path": mountPath})
			ourUnmount = true
		}
	} else if vol.contentType == ContentTypeBlock {
		// For VMs, also mount the filesystem dataset.
		if vol.IsVMBlock() {
			fsVol := vol.NewVMBlockFilesystemVolume()
			_, err := d.UnmountVolume(fsVol, false, op)
			if err != nil {
				return false, err
			}
		}

		// For block devices, we make them disappear if active.
		if !keepBlockDev {
			current, err := d.getDatasetProperty(d.dataset(vol, false), "volmode")
			if err != nil {
				return false, err
			}

			if current == "dev" {
				if refCount > 0 {
					d.logger.Debug("Skipping unmount as in use", "refCount", refCount)
					return false, ErrInUse
				}

				devPath, _ := d.GetVolumeDiskPath(vol)
				if err != nil {
					return false, errors.Wrapf(err, "Failed locating zvol for deactivation")
				}

				waitDuration := time.Duration(time.Second * time.Duration(5))
				waitUntil := time.Now().Add(waitDuration)
				for {
					// Sometimes it takes multiple attempts for ZFS to actually apply this.
					err = d.setDatasetProperties(dataset, "volmode=none")
					if err != nil {
						return false, err
					}

					if !shared.PathExists(devPath) {
						d.logger.Debug("Deactivated ZFS volume", log.Ctx{"dev": dataset})
						break
					}

					if time.Now().After(waitUntil) {
						return false, fmt.Errorf("Failed to deactivate zvol after %v", waitDuration)
					}

					d.logger.Debug("Waiting for ZFS volume to deactivate", log.Ctx{"dev": dataset})
					time.Sleep(time.Millisecond * time.Duration(500))
				}

				ourUnmount = true
			}
		}
	}

	return ourUnmount, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *zfs) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	newVol := NewVolume(d, d.name, vol.volType, vol.contentType, newVolName, vol.config, vol.poolConfig)

	// Revert handling.
	revert := revert.New()
	defer revert.Fail()

	// First rename the VFS paths.
	err := genericVFSRenameVolume(d, vol, newVolName, op)
	if err != nil {
		return err
	}

	revert.Add(func() {
		genericVFSRenameVolume(d, newVol, vol.name, op)
	})

	// Rename the ZFS datasets.
	_, err = shared.RunCommand("zfs", "rename", d.dataset(vol, false), d.dataset(newVol, false))
	if err != nil {
		return err
	}

	revert.Add(func() {
		shared.RunCommand("zfs", "rename", d.dataset(newVol, false), d.dataset(vol, false))
	})

	// Update the mountpoints.
	if vol.contentType == ContentTypeFS {
		err = d.setDatasetProperties(d.dataset(newVol, false), fmt.Sprintf("mountpoint=%s", newVol.MountPath()))
		if err != nil {
			return err
		}
	}

	// For VM images, create a filesystem volume too.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.RenameVolume(fsVol, newVolName, op)
		if err != nil {
			return err
		}

		revert.Add(func() {
			newFsVol := NewVolume(d, d.name, newVol.volType, ContentTypeFS, newVol.name, newVol.config, newVol.poolConfig)
			d.RenameVolume(newFsVol, vol.name, op)
		})
	}

	// All done.
	revert.Success()

	return nil
}

// MigrateVolume sends a volume for migration.
func (d *zfs) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error {
	// Handle simple rsync and block_and_rsync through generic.
	if volSrcArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volSrcArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
	} else if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_ZFS {
		return ErrNotSupported
	}

	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.MigrateVolume(fsVol, conn, volSrcArgs, op)
		if err != nil {
			return err
		}
	}

	// Handle zfs send/receive migration.
	var finalParent string
	if !volSrcArgs.FinalSync {
		// Transfer the snapshots first.
		for i, snapName := range volSrcArgs.Snapshots {
			snapshot, _ := vol.NewSnapshot(snapName)

			// Figure out parent and current subvolumes.
			parent := ""
			if i > 0 {
				oldSnapshot, _ := vol.NewSnapshot(volSrcArgs.Snapshots[i-1])
				parent = d.dataset(oldSnapshot, false)
			}

			// Setup progress tracking.
			var wrapper *ioprogress.ProgressTracker
			if volSrcArgs.TrackProgress {
				wrapper = migration.ProgressTracker(op, "fs_progress", snapshot.name)
			}

			// Send snapshot to recipient (ensure local snapshot volume is mounted if needed).
			err := d.sendDataset(d.dataset(snapshot, false), parent, volSrcArgs, conn, wrapper)
			if err != nil {
				return err
			}

			finalParent = d.dataset(snapshot, false)
		}
	}

	// Setup progress tracking.
	var wrapper *ioprogress.ProgressTracker
	if volSrcArgs.TrackProgress {
		wrapper = migration.ProgressTracker(op, "fs_progress", vol.name)
	}

	srcSnapshot := d.dataset(vol, false)
	if !vol.IsSnapshot() {
		// Create a temporary read-only snapshot.
		srcSnapshot = fmt.Sprintf("%s@migration-%s", d.dataset(vol, false), uuid.NewRandom().String())
		_, err := shared.RunCommand("zfs", "snapshot", srcSnapshot)
		if err != nil {
			return err
		}

		if volSrcArgs.MultiSync {
			if volSrcArgs.FinalSync {
				if volSrcArgs.Data != nil {
					finalParent = volSrcArgs.Data.(map[ContentType]string)[vol.ContentType()]
				}

				defer shared.RunCommand("zfs", "destroy", finalParent)
				defer shared.RunCommand("zfs", "destroy", srcSnapshot)
			} else {
				if volSrcArgs.Data == nil {
					volSrcArgs.Data = map[ContentType]string{}
				}
				volSrcArgs.Data.(map[ContentType]string)[vol.ContentType()] = srcSnapshot // Persist parent state for final sync.
			}
		} else {
			defer shared.RunCommand("zfs", "destroy", srcSnapshot)
		}
	}

	// Send the volume itself.
	err := d.sendDataset(srcSnapshot, finalParent, volSrcArgs, conn, wrapper)
	if err != nil {
		return err
	}

	return nil
}

// BackupVolume creates an exported version of a volume.
func (d *zfs) BackupVolume(vol Volume, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots []string, op *operations.Operation) error {
	// Handle the non-optimized tarballs through the generic packer.
	if !optimized {
		// Because the generic backup method will not take a consistent backup if files are being modified
		// as they are copied to the tarball, as ZFS allows us to take a quick snapshot without impacting
		// the parent volume we do so here to ensure the backup taken is consistent.
		if vol.contentType == ContentTypeFS {
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

			// Create a temporary snapshot.
			srcSnapshot := fmt.Sprintf("%s@backup-%s", d.dataset(vol, false), uuid.NewRandom().String())
			_, err = shared.RunCommand("zfs", "snapshot", srcSnapshot)
			if err != nil {
				return err
			}
			defer shared.RunCommand("zfs", "destroy", srcSnapshot)
			d.logger.Debug("Created backup snapshot", log.Ctx{"dev": srcSnapshot})

			// Override volume's mount path with location of snapshot so genericVFSBackupVolume reads
			// from there instead of main volume.
			vol.customMountPath = tmpDir

			// Mount the snapshot directly (not possible through ZFS tools), so that the volume is
			// already mounted by the time genericVFSBackupVolume tries to mount it below,
			// thus preventing it from trying to unmount it at the end, as this is a custom snapshot,
			// the normal mount and unmount logic will fail.
			err = TryMount(srcSnapshot, vol.MountPath(), "zfs", 0, "")
			if err != nil {
				return err
			}
			d.logger.Debug("Mounted ZFS snapshot dataset", log.Ctx{"dev": srcSnapshot, "path": vol.MountPath()})

			defer func(dataset string, mountPath string) {
				_, err := forceUnmount(mountPath)
				if err != nil {
					return
				}

				d.logger.Debug("Unmounted ZFS snapshot dataset", log.Ctx{"dev": dataset, "path": mountPath})
			}(srcSnapshot, vol.MountPath())
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

	// Backup VM config volumes first.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.BackupVolume(fsVol, tarWriter, optimized, snapshots, op)
		if err != nil {
			return err
		}
	}

	// Handle the optimized tarballs.
	sendToFile := func(path string, parent string, fileName string) error {
		// Prepare zfs send arguments.
		args := []string{"send"}
		if parent != "" {
			args = append(args, "-i", parent)
		}
		args = append(args, path)

		// Create temporary file to store output of ZFS send.
		backupsPath := shared.VarPath("backups")
		tmpFile, err := ioutil.TempFile(backupsPath, fmt.Sprintf("%s_zfs", backup.WorkingDirPrefix))
		if err != nil {
			return errors.Wrapf(err, "Failed to open temporary file for ZFS backup")
		}
		defer tmpFile.Close()
		defer os.Remove(tmpFile.Name())

		// Write the subvolume to the file.
		d.logger.Debug("Generating optimized volume file", log.Ctx{"sourcePath": path, "file": tmpFile.Name(), "name": fileName})

		// Write the subvolume to the file.
		err = shared.RunCommandWithFds(nil, tmpFile, "zfs", args...)
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

	// Handle snapshots.
	finalParent := ""
	if len(snapshots) > 0 {
		for i, snapName := range snapshots {
			snapshot, _ := vol.NewSnapshot(snapName)

			// Figure out parent and current subvolumes.
			parent := ""
			if i > 0 {
				oldSnapshot, _ := vol.NewSnapshot(snapshots[i-1])
				parent = d.dataset(oldSnapshot, false)
			}

			// Make a binary zfs backup.
			prefix := "snapshots"
			fileName := fmt.Sprintf("%s.bin", snapName)
			if vol.volType == VolumeTypeVM {
				prefix = "virtual-machine-snapshots"
				if vol.contentType == ContentTypeFS {
					fileName = fmt.Sprintf("%s-config.bin", snapName)
				}
			} else if vol.volType == VolumeTypeCustom {
				prefix = "volume-snapshots"
			}

			target := fmt.Sprintf("backup/%s/%s", prefix, fileName)
			err := sendToFile(d.dataset(snapshot, false), parent, target)
			if err != nil {
				return err
			}

			finalParent = d.dataset(snapshot, false)
		}
	}

	// Create a temporary read-only snapshot.
	srcSnapshot := fmt.Sprintf("%s@backup-%s", d.dataset(vol, false), uuid.NewRandom().String())
	_, err := shared.RunCommand("zfs", "snapshot", srcSnapshot)
	if err != nil {
		return err
	}
	defer shared.RunCommand("zfs", "destroy", srcSnapshot)

	// Dump the container to a file.
	fileName := "container.bin"
	if vol.volType == VolumeTypeVM {
		if vol.contentType == ContentTypeFS {
			fileName = "virtual-machine-config.bin"
		} else {
			fileName = "virtual-machine.bin"
		}
	} else if vol.volType == VolumeTypeCustom {
		fileName = "volume.bin"
	}

	err = sendToFile(srcSnapshot, finalParent, fmt.Sprintf("backup/%s", fileName))
	if err != nil {
		return err
	}

	return nil
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *zfs) CreateVolumeSnapshot(vol Volume, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(vol.name)

	// Revert handling.
	revert := revert.New()
	defer revert.Fail()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, vol.volType, parentName)
	if err != nil {
		return err
	}

	// Create snapshot directory.
	err = vol.EnsureMountPath()
	if err != nil {
		return err
	}

	// Make the snapshot.
	_, err = shared.RunCommand("zfs", "snapshot", d.dataset(vol, false))
	if err != nil {
		return err
	}

	revert.Add(func() { d.DeleteVolumeSnapshot(vol, op) })

	// For VM images, create a filesystem volume too.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.CreateVolumeSnapshot(fsVol, op)
		if err != nil {
			return err
		}

		revert.Add(func() { d.DeleteVolumeSnapshot(fsVol, op) })
	}

	// All done.
	revert.Success()

	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device.
func (d *zfs) DeleteVolumeSnapshot(vol Volume, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(vol.name)

	// Handle clones.
	clones, err := d.getClones(d.dataset(vol, false))
	if err != nil {
		return err
	}

	if len(clones) > 0 {
		// Move to the deleted path.
		_, err := shared.RunCommand("zfs", "rename", d.dataset(vol, false), d.dataset(vol, true))
		if err != nil {
			return err
		}
	} else {
		// Delete the snapshot.
		_, err := shared.RunCommand("zfs", "destroy", d.dataset(vol, false))
		if err != nil {
			return err
		}
	}

	// Delete the mountpoint.
	err = os.Remove(vol.MountPath())
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "Failed to remove '%s'", vol.MountPath())
	}

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, parentName)
	if err != nil {
		return err
	}

	// For VM images, create a filesystem volume too.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.DeleteVolumeSnapshot(fsVol, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// MountVolumeSnapshot simulates mounting a volume snapshot.
func (d *zfs) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	var err error
	ourMounts := 0
	mountPath := snapVol.MountPath()
	snapshotDataset := d.dataset(snapVol, false)

	revert := revert.New()
	defer revert.Fail()

	// Check if filesystem volume already mounted.
	if snapVol.contentType == ContentTypeFS {
		if !shared.IsMountPoint(mountPath) {
			err := snapVol.EnsureMountPath()
			if err != nil {
				return false, err
			}

			// Mount the snapshot directly (not possible through tools).
			err = TryMount(snapshotDataset, mountPath, "zfs", 0, "")
			if err != nil {
				return false, err
			}

			d.logger.Debug("Mounted ZFS snapshot dataset", log.Ctx{"dev": snapshotDataset, "path": mountPath})
			ourMounts++
		}
	} else if snapVol.contentType == ContentTypeBlock {
		// For block devices, we make them appear by enabling volmode=dev and snapdev=visible on the parent volume.
		// Ensure snap volume parent is activated to avoid issues activating the snapshot volume device.
		parent, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.Name())
		parentVol := NewVolume(d, d.Name(), snapVol.volType, snapVol.contentType, parent, snapVol.config, snapVol.poolConfig)
		err = d.MountVolume(parentVol, op)
		if err != nil {
			return false, err
		}
		revert.Add(func() { d.UnmountVolume(parentVol, false, op) })

		parentDataset := d.dataset(parentVol, false)

		// Check if parent already active.
		parentVolMode, err := d.getDatasetProperty(parentDataset, "volmode")
		if err != nil {
			return false, err
		}

		// Order is important here, the parent volmode=dev must be set before snapdev=visible otherwise
		// it won't take effect.
		if parentVolMode != "dev" {
			return false, fmt.Errorf("Parent block volume needs to be mounted first")
		}

		// Check if snapdev already set visible.
		parentSnapdevMode, err := d.getDatasetProperty(parentDataset, "snapdev")
		if err != nil {
			return false, err
		}

		if parentSnapdevMode != "visible" {
			err = d.setDatasetProperties(parentDataset, "snapdev=visible")
			if err != nil {
				return false, err
			}

			// Wait half a second to give udev a chance to kick in.
			time.Sleep(500 * time.Millisecond)

			d.logger.Debug("Activated ZFS snapshot volume", log.Ctx{"dev": snapshotDataset})
			ourMounts++
		}

		if snapVol.IsVMBlock() {
			// For VMs, also mount the filesystem dataset.
			fsVol := snapVol.NewVMBlockFilesystemVolume()
			ourMount, err := d.MountVolumeSnapshot(fsVol, op)
			if err != nil {
				return false, err
			}
			if ourMount {
				ourMounts++
			}
		}
	}

	revert.Success()
	return ourMounts > 0, nil
}

// UnmountVolume simulates unmounting a volume snapshot.
func (d *zfs) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	mountPath := snapVol.MountPath()
	snapshotDataset := d.dataset(snapVol, false)

	// For VMs, also mount the filesystem dataset.
	if snapVol.IsVMBlock() {
		fsSnapVol := snapVol.NewVMBlockFilesystemVolume()
		_, err := d.UnmountVolumeSnapshot(fsSnapVol, op)
		if err != nil {
			return false, err
		}
	}

	// For block devices, we make them disappear.
	if snapVol.contentType == ContentTypeBlock {
		parent, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.Name())
		parentVol := NewVolume(d, d.Name(), snapVol.volType, snapVol.contentType, parent, snapVol.config, snapVol.poolConfig)
		parentDataset := d.dataset(parentVol, false)

		err := d.setDatasetProperties(parentDataset, "snapdev=hidden")
		if err != nil {
			return false, err
		}

		d.logger.Debug("Deactivated ZFS snapshot volume", log.Ctx{"dev": snapshotDataset})

		// Ensure snap volume parent is deactivated in case we activated it when mounting snapshot.
		_, err = d.UnmountVolume(parentVol, false, op)
		if err != nil {
			return false, err
		}

		return true, nil
	}

	// Check if still mounted.
	if shared.IsMountPoint(mountPath) {
		_, err := forceUnmount(mountPath)
		if err != nil {
			return false, err
		}

		d.logger.Debug("Unmounted ZFS snapshot dataset", log.Ctx{"dev": snapshotDataset, "path": mountPath})
		return true, nil
	}

	return false, nil
}

// VolumeSnapshots returns a list of snapshots for the volume (in no particular order).
func (d *zfs) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	// Get all children datasets.
	entries, err := d.getDatasets(d.dataset(vol, false))
	if err != nil {
		return nil, err
	}

	// Filter only the snapshots.
	snapshots := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry, "@snapshot-") {
			snapshots = append(snapshots, strings.TrimPrefix(entry, "@snapshot-"))
		}
	}

	return snapshots, nil
}

// RestoreVolume restores a volume from a snapshot.
func (d *zfs) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	snapVol := NewVolume(d, d.name, vol.volType, vol.contentType, fmt.Sprintf("%s/%s", vol.name, snapshotName), vol.config, vol.poolConfig)

	// Get the list of snapshots.
	entries, err := d.getDatasets(d.dataset(vol, false))
	if err != nil {
		return err
	}

	// Check if more recent snapshots exist.
	idx := -1
	snapshots := []string{}
	for i, entry := range entries {
		if entry == fmt.Sprintf("@snapshot-%s", snapshotName) {
			// Located the current snapshot.
			idx = i
			continue
		} else if idx < 0 {
			// Skip any previous snapshot.
			continue
		}

		if strings.HasPrefix(entry, "@snapshot-") {
			// Located a normal snapshot following ours.
			snapshots = append(snapshots, strings.TrimPrefix(entry, "@snapshot-"))
			continue
		}

		if strings.HasPrefix(entry, "@") {
			// Located an internal snapshot.
			return fmt.Errorf("Snapshot %q cannot be restored due to subsequent internal snapshot(s) (from a copy)", snapshotName)
		}
	}

	// Check if snapshot removal is allowed.
	if len(snapshots) > 0 {
		if !shared.IsTrue(vol.ExpandedConfig("zfs.remove_snapshots")) {
			return fmt.Errorf("Snapshot %q cannot be restored due to subsequent snapshot(s). Set zfs.remove_snapshots to override", snapshotName)
		}

		// Setup custom error to tell the backend what to delete.
		err := ErrDeleteSnapshots{}
		err.Snapshots = snapshots
		return err
	}

	// Restore the snapshot.
	_, err = shared.RunCommand("zfs", "rollback", d.dataset(snapVol, false))
	if err != nil {
		return err
	}

	// For VM images, restore the associated filesystem dataset too.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.RestoreVolume(fsVol, snapshotName, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *zfs) RenameVolumeSnapshot(vol Volume, newSnapshotName string, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(vol.name)
	newVol := NewVolume(d, d.name, vol.volType, vol.contentType, fmt.Sprintf("%s/%s", parentName, newSnapshotName), vol.config, vol.poolConfig)

	// Revert handling.
	revert := revert.New()
	defer revert.Fail()

	// First rename the VFS paths.
	err := genericVFSRenameVolumeSnapshot(d, vol, newSnapshotName, op)
	if err != nil {
		return err
	}

	revert.Add(func() {
		genericVFSRenameVolumeSnapshot(d, newVol, vol.name, op)
	})

	// Rename the ZFS datasets.
	_, err = shared.RunCommand("zfs", "rename", d.dataset(vol, false), d.dataset(newVol, false))
	if err != nil {
		return err
	}

	revert.Add(func() {
		shared.RunCommand("zfs", "rename", d.dataset(newVol, false), d.dataset(vol, false))
	})

	// For VM images, create a filesystem volume too.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.RenameVolumeSnapshot(fsVol, newSnapshotName, op)
		if err != nil {
			return err
		}

		revert.Add(func() {
			newFsVol := NewVolume(d, d.name, newVol.volType, ContentTypeFS, newVol.name, newVol.config, newVol.poolConfig)
			d.RenameVolumeSnapshot(newFsVol, vol.name, op)
		})
	}

	// All done.
	revert.Success()

	return nil
}
