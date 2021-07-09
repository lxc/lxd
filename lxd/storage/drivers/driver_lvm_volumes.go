package drivers

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/rsync"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/instancewriter"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/validate"
)

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied filler function.
func (d *lvm) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	volPath := vol.MountPath()
	err := vol.EnsureMountPath()
	if err != nil {
		return err
	}
	revert.Add(func() { os.RemoveAll(volPath) })

	err = d.createLogicalVolume(d.config["lvm.vg_name"], d.thinpoolName(), vol, d.usesThinpool())
	if err != nil {
		return errors.Wrapf(err, "Error creating LVM logical volume")
	}
	revert.Add(func() { d.DeleteVolume(vol, op) })

	// For VMs, also create the filesystem volume.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.CreateVolume(fsVol, nil, op)
		if err != nil {
			return err
		}

		revert.Add(func() { d.DeleteVolume(fsVol, op) })
	}

	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
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
			err = vol.EnsureMountPath()
			if err != nil {
				return err
			}
		}

		return nil
	}, op)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// CreateVolumeFromBackup restores a backup tarball onto the storage device.
func (d *lvm) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
	return genericVFSBackupUnpack(d, vol, srcBackup.Snapshots, srcData, op)
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *lvm) CreateVolumeFromCopy(vol, srcVol Volume, copySnapshots bool, op *operations.Operation) error {
	var err error
	var srcSnapshots []Volume

	if copySnapshots && !srcVol.IsSnapshot() {
		// Get the list of snapshots from the source.
		srcSnapshots, err = srcVol.Snapshots(op)
		if err != nil {
			return err
		}
	}

	// We can use optimised copying when the pool is backed by an LVM thinpool.
	if d.usesThinpool() {
		err = d.copyThinpoolVolume(vol, srcVol, srcSnapshots, false)
		if err != nil {
			return err
		}

		// For VMs, also copy the filesystem volume.
		if vol.IsVMBlock() {
			srcFSVol := srcVol.NewVMBlockFilesystemVolume()
			fsVol := vol.NewVMBlockFilesystemVolume()
			return d.copyThinpoolVolume(fsVol, srcFSVol, srcSnapshots, false)
		}

		return nil
	}

	// Before doing a generic volume copy, we need to ensure volume (or snap volume parent) is activated to
	// avoid failing with warnings about changing the origin of the snapshot when trying to activate it.
	parent, _, _ := shared.InstanceGetParentAndSnapshotName(srcVol.Name())
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], srcVol.volType, srcVol.contentType, parent)
	activated, err := d.activateVolume(volDevPath)
	if err != nil {
		return err
	}
	if activated {
		defer d.deactivateVolume(volDevPath)
	}

	// Otherwise run the generic copy.
	return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, false, op)
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *lvm) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	return genericVFSCreateVolumeFromMigration(d, nil, vol, conn, volTargetArgs, preFiller, op)
}

// RefreshVolume provides same-pool volume and specific snapshots syncing functionality.
func (d *lvm) RefreshVolume(vol, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	// We can use optimised copying when the pool is backed by an LVM thinpool.
	if d.usesThinpool() {
		return d.copyThinpoolVolume(vol, srcVol, srcSnapshots, true)
	}

	// Otherwise run the generic copy.
	return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, op)
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then this function
// will return an error.
func (d *lvm) DeleteVolume(vol Volume, op *operations.Operation) error {
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return fmt.Errorf("Cannot remove a volume that has snapshots")
	}

	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	lvExists, err := d.logicalVolumeExists(volDevPath)
	if err != nil {
		return err
	}

	if lvExists {
		if vol.contentType == ContentTypeFS {
			_, err = d.UnmountVolume(vol, false, op)
			if err != nil {
				return errors.Wrapf(err, "Error unmounting LVM logical volume")
			}
		}

		err = d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name))
		if err != nil {
			return errors.Wrapf(err, "Error removing LVM logical volume")
		}
	}

	if vol.contentType == ContentTypeFS {
		// Remove the volume from the storage device.
		mountPath := vol.MountPath()
		err = os.RemoveAll(mountPath)
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "Error removing LVM logical volume mount path %q", mountPath)
		}

		// Although the volume snapshot directory should already be removed, lets remove it here to just in
		// case the top-level directory is left.
		err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}
	}

	// For VMs, also delete the filesystem volume.
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
func (d *lvm) HasVolume(vol Volume) bool {
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	volExists, err := d.logicalVolumeExists(volDevPath)
	if err != nil {
		return false
	}

	return volExists
}

// FillVolumeConfig populate volume with default config.
func (d *lvm) FillVolumeConfig(vol Volume) error {
	// Only validate filesystem config keys for filesystem volumes or VM block volumes (which have an
	// associated filesystem volume).
	if vol.ContentType() == ContentTypeFS || vol.IsVMBlock() {
		// Inherit filesystem from pool if not set.
		if vol.config["block.filesystem"] == "" {
			vol.config["block.filesystem"] = d.config["volume.block.filesystem"]
		}

		// Default filesystem if neither volume nor pool specify an override.
		if vol.config["block.filesystem"] == "" {
			// Unchangeable volume property: Set unconditionally.
			vol.config["block.filesystem"] = DefaultFilesystem
		}

		// Inherit filesystem mount options from pool if not set.
		if vol.config["block.mount_options"] == "" {
			vol.config["block.mount_options"] = d.config["volume.block.mount_options"]
		}

		// Default filesystem mount options if neither volume nor pool specify an override.
		if vol.config["block.mount_options"] == "" {
			// Unchangeable volume property: Set unconditionally.
			vol.config["block.mount_options"] = "discard"
		}
	}

	return nil
}

// ValidateVolume validates the supplied volume config.
func (d *lvm) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	rules := map[string]func(value string) error{
		"block.filesystem": validate.Optional(func(value string) error {
			return validate.IsOneOf(value, lvmAllowedFilesystems)
		}),
		"lvm.stripes":      validate.Optional(validate.IsUint32),
		"lvm.stripes.size": validate.Optional(validate.IsSize),
	}

	err := d.validateVolume(vol, rules, removeUnknownKeys)
	if err != nil {
		return err
	}

	if d.usesThinpool() && vol.config["lvm.stripes"] != "" {
		return fmt.Errorf("lvm.stripes cannot be used with thin pool volumes")
	}

	if d.usesThinpool() && vol.config["lvm.stripes.size"] != "" {
		return fmt.Errorf("lvm.stripes.size cannot be used with thin pool volumes")
	}

	return nil
}

// UpdateVolume applies config changes to the volume.
func (d *lvm) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	newSize, sizeChanged := changedConfig["size"]
	if sizeChanged {
		err := d.SetVolumeQuota(vol, newSize, false, nil)
		if err != nil {
			return err
		}
	}

	if _, changed := changedConfig["lvm.stripes"]; changed {
		return fmt.Errorf("lvm.stripes cannot be changed")
	}

	if _, changed := changedConfig["lvm.stripes.size"]; changed {
		return fmt.Errorf("lvm.stripes.size cannot be changed")
	}

	return nil
}

// GetVolumeUsage returns the disk space used by the volume (this is not currently supported).
func (d *lvm) GetVolumeUsage(vol Volume) (int64, error) {
	// Snapshot usage not supported for LVM.
	if vol.IsSnapshot() {
		return -1, ErrNotSupported
	}

	// For non-snapshot filesystem volumes, we only return usage when the volume is mounted.
	// This is because to get an accurate value we cannot use blocks allocated, as the filesystem will likely
	// consume blocks and not free them when files are deleted in the volume. This avoids returning different
	// values depending on whether the volume is mounted or not.
	if vol.contentType == ContentTypeFS && shared.IsMountPoint(vol.MountPath()) {
		var stat unix.Statfs_t
		err := unix.Statfs(vol.MountPath(), &stat)
		if err != nil {
			return -1, err
		}

		return int64(stat.Blocks-stat.Bfree) * int64(stat.Bsize), nil
	} else if vol.contentType == ContentTypeBlock && d.usesThinpool() {
		// For non-snapshot thin pool block volumes we can calculate an approximate usage using the space
		// allocated to the volume from the thin pool.
		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
		_, usedSize, err := d.thinPoolVolumeUsage(volDevPath)
		if err != nil {
			return -1, err
		}

		return int64(usedSize), nil
	}

	return -1, ErrNotSupported
}

// SetVolumeQuota applies a size limit on volume.
// Does nothing if supplied with an empty/zero size.
func (d *lvm) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	// Do nothing if size isn't specified.
	if size == "" || size == "0" {
		return nil
	}

	sizeBytes, err := d.roundedSizeBytesString(size)
	if err != nil {
		return err
	}

	// Read actual size of current volume.
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	oldSizeBytes, err := d.logicalVolumeSize(volDevPath)
	if err != nil {
		return err
	}

	// Get the volume group's physical extent size, as we use this to figure out if the new and old sizes are
	// going to change beyond 1 extent size, otherwise there is no point in trying to resize as LVM do it.
	vgExtentSize, err := d.volumeGroupExtentSize(d.config["lvm.vg_name"])
	if err != nil {
		return err
	}

	// Round up the number of extents required for new quota size, as this is what the lvresize tool will do.
	newNumExtents := math.Ceil(float64(sizeBytes) / float64(vgExtentSize))
	oldNumExtents := math.Ceil(float64(oldSizeBytes) / float64(vgExtentSize))
	extentDiff := int(newNumExtents - oldNumExtents)

	// If old and new extents required are the same, nothing to do, as LVM won't resize them.
	if extentDiff == 0 {
		return nil
	}

	logCtx := log.Ctx{"dev": volDevPath, "size": fmt.Sprintf("%db", sizeBytes)}

	// Activate volume if needed.
	activated, err := d.activateVolume(volDevPath)
	if err != nil {
		return err
	}
	if activated {
		defer d.deactivateVolume(volDevPath)
	}

	inUse := vol.MountInUse()

	// Resize filesystem if needed.
	if vol.contentType == ContentTypeFS {
		fsType := vol.ConfigBlockFilesystem()

		if sizeBytes < oldSizeBytes {
			if !filesystemTypeCanBeShrunk(fsType) {
				return errors.Wrapf(ErrCannotBeShrunk, "Filesystem %q cannot be shrunk", fsType)
			}

			if inUse {
				return ErrInUse // We don't allow online shrinking of filesytem volumes.
			}

			// Shrink filesystem first.
			// Pass allowUnsafeResize to allow disabling of filesystem resize safety checks.
			// We do this as a separate step rather than passing -r to lvresize in resizeLogicalVolume
			// so that we can have more control over when we trigger unsafe filesystem resize mode,
			// otherwise by passing -f to lvresize (required for other reasons) this would then pass
			// -f onto resize2fs as well.
			err = shrinkFileSystem(fsType, volDevPath, vol, sizeBytes, allowUnsafeResize)
			if err != nil {
				return err
			}
			d.logger.Debug("Logical volume filesystem shrunk", logCtx)

			// Shrink the block device.
			err = d.resizeLogicalVolume(volDevPath, sizeBytes)
			if err != nil {
				return err
			}
		} else if sizeBytes > oldSizeBytes {
			// Grow block device first.
			err = d.resizeLogicalVolume(volDevPath, sizeBytes)
			if err != nil {
				return err
			}

			// Grow the filesystem to fill block device.
			err = growFileSystem(fsType, volDevPath, vol)
			if err != nil {
				return err
			}
			d.logger.Debug("Logical volume filesystem grown", logCtx)
		}
	} else {
		// Only perform pre-resize checks if we are not in "unsafe" mode.
		// In unsafe mode we expect the caller to know what they are doing and understand the risks.
		if !allowUnsafeResize {
			if sizeBytes < oldSizeBytes {
				return errors.Wrap(ErrCannotBeShrunk, "Block volumes cannot be shrunk")
			}

			if inUse {
				return ErrInUse // We don't allow online resizing of block volumes.
			}
		}

		err = d.resizeLogicalVolume(volDevPath, sizeBytes)
		if err != nil {
			return err
		}

		// Move the VM GPT alt header to end of disk if needed (not needed in unsafe resize mode as it is
		// expected the caller will do all necessary post resize actions themselves).
		if vol.IsVMBlock() && !allowUnsafeResize {
			err = d.moveGPTAltHeader(volDevPath)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// GetVolumeDiskPath returns the location of a disk volume.
func (d *lvm) GetVolumeDiskPath(vol Volume) (string, error) {
	if vol.IsVMBlock() || vol.volType == VolumeTypeCustom && vol.contentType == ContentTypeBlock {
		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
		return volDevPath, nil
	}

	return "", ErrNotSupported
}

// ListVolumes returns a list of LXD volumes in storage pool.
func (d *lvm) ListVolumes() ([]Volume, error) {
	var vols []Volume

	cmd := exec.Command("lvs", "--noheadings", "-o", "lv_name", d.config["lvm.vg_name"])
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		lvName := strings.TrimSpace(scanner.Text())
		var volType VolumeType

		for lvmVolumeType, lvmVolumeTypePrefix := range lvmVolumeTypePrefixes {
			if strings.HasPrefix(lvName, fmt.Sprintf("%s_", lvmVolumeTypePrefix)) {
				volType = lvmVolumeType
			}
		}

		if volType == "" {
			d.logger.Debug("Ignoring unrecognised volume type", log.Ctx{"lvName": lvName})
			continue // Ignore unrecognised volume.
		}

		parts := strings.SplitN(lvName, "_", 2)
		if len(parts) < 2 {
			d.logger.Debug("Ignoring unrecognised volume", log.Ctx{"lvName": lvName})
			continue // Ignore unrecognised volume.
		}

		volName := parts[1]

		lvSnapSepCount := strings.Count(volName, lvmSnapshotSeparator)
		if lvSnapSepCount%2 != 0 {
			// If snapshot separator count is odd, then this means we have a lone lvmSnapshotSeparator
			// that is not part of the lvmEscapedHyphen pair, which means this volume is a snapshot.
			d.logger.Debug("Ignoring snapshot volume", log.Ctx{"lvName": lvName})
			continue // Ignore snapshot volumes.
		}

		isBlock := strings.HasSuffix(volName, lvmBlockVolSuffix)

		if volType == VolumeTypeVM && !isBlock {
			continue // Ignore VM filesystem volumes as we will just return the VM's block volume.
		}

		// Unescape raw LVM name to LXD storage volume name. Safe to do now we know we are not dealing
		// with snapshot volumes.
		volName = strings.Replace(volName, lvmEscapedHyphen, "-", -1)

		contentType := ContentTypeFS
		if volType == VolumeTypeVM || isBlock {
			contentType = ContentTypeBlock
			volName = strings.TrimSuffix(volName, lvmBlockVolSuffix)
		}

		vols = append(vols, NewVolume(d, d.name, volType, contentType, volName, nil, d.config))
	}

	errMsg, err := ioutil.ReadAll(stderr)
	if err != nil {
		return nil, err
	}

	err = cmd.Wait()
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get volume list: %v", strings.TrimSpace(string(errMsg)))
	}

	return vols, nil
}

// MountVolume mounts a volume and increments ref counter. Please call UnmountVolume() when done with the volume.
func (d *lvm) MountVolume(vol Volume, op *operations.Operation) error {
	unlock := vol.MountLock()
	defer unlock()

	revert := revert.New()
	defer revert.Fail()

	// Activate LVM volume if needed.
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	activated, err := d.activateVolume(volDevPath)
	if err != nil {
		return err
	}
	if activated {
		revert.Add(func() { d.deactivateVolume(volDevPath) })
	}

	if vol.contentType == ContentTypeFS {
		// Check if already mounted.
		mountPath := vol.MountPath()
		if !shared.IsMountPoint(mountPath) {
			err = vol.EnsureMountPath()
			if err != nil {
				return err
			}

			mountFlags, mountOptions := resolveMountOptions(vol.ConfigBlockMountOptions())
			err = TryMount(volDevPath, mountPath, vol.ConfigBlockFilesystem(), mountFlags, mountOptions)
			if err != nil {
				return errors.Wrapf(err, "Failed to mount LVM logical volume")
			}
			d.logger.Debug("Mounted logical volume", log.Ctx{"dev": volDevPath, "path": mountPath, "options": mountOptions})
		}
	} else if vol.contentType == ContentTypeBlock {
		// For VMs, mount the filesystem volume.
		if vol.IsVMBlock() {
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
func (d *lvm) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock := vol.MountLock()
	defer unlock()

	var err error
	ourUnmount := false
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	refCount := vol.MountRefCountDecrement()

	// Check if already mounted.
	if vol.contentType == ContentTypeFS {
		mountPath := vol.MountPath()
		if shared.IsMountPoint(mountPath) {
			if refCount > 0 {
				d.logger.Debug("Skipping unmount as in use", "refCount", refCount)
				return false, ErrInUse
			}

			err = TryUnmount(mountPath, 0)
			if err != nil {
				return false, errors.Wrapf(err, "Failed to unmount LVM logical volume")
			}
			d.logger.Debug("Unmounted logical volume", log.Ctx{"path": mountPath, "keepBlockDev": keepBlockDev})

			// We only deactivate filesystem volumes if an unmount was needed to better align with our
			// unmount return value indicator.
			if !keepBlockDev {
				_, err = d.deactivateVolume(volDevPath)
				if err != nil {
					return false, err
				}
			}

			ourUnmount = true
		}
	} else if vol.contentType == ContentTypeBlock {
		// For VMs, unmount the filesystem volume.
		if vol.IsVMBlock() {
			fsVol := vol.NewVMBlockFilesystemVolume()
			_, err = d.UnmountVolume(fsVol, false, op)
			if err != nil {
				return false, err
			}
		}

		if !keepBlockDev && shared.PathExists(volDevPath) {
			if refCount > 0 {
				d.logger.Debug("Skipping unmount as in use", "refCount", refCount)
				return false, ErrInUse
			}

			_, err = d.deactivateVolume(volDevPath)
			if err != nil {
				return false, err
			}

			ourUnmount = true
		}
	}

	return ourUnmount, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *lvm) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)

	return vol.UnmountTask(func(op *operations.Operation) error {
		snapNames, err := d.VolumeSnapshots(vol, op)
		if err != nil {
			return err
		}

		revert := revert.New()
		defer revert.Fail()

		// Rename snapshots (change volume prefix to use new parent volume name).
		for _, snapName := range snapNames {
			snapVolName := GetSnapshotVolumeName(vol.name, snapName)
			snapVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, snapVolName)
			newSnapVolName := GetSnapshotVolumeName(newVolName, snapName)
			newSnapVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, newSnapVolName)
			err = d.renameLogicalVolume(snapVolDevPath, newSnapVolDevPath)
			if err != nil {
				return err
			}
			revert.Add(func() { d.renameLogicalVolume(newSnapVolDevPath, snapVolDevPath) })
		}

		// Rename snapshots dir if present.
		if vol.contentType == ContentTypeFS {
			srcSnapshotDir := GetVolumeSnapshotDir(d.name, vol.volType, vol.name)
			dstSnapshotDir := GetVolumeSnapshotDir(d.name, vol.volType, newVolName)
			if shared.PathExists(srcSnapshotDir) {
				err = os.Rename(srcSnapshotDir, dstSnapshotDir)
				if err != nil {
					return errors.Wrapf(err, "Error renaming LVM logical volume snapshot directory from %q to %q", srcSnapshotDir, dstSnapshotDir)
				}
				revert.Add(func() { os.Rename(dstSnapshotDir, srcSnapshotDir) })
			}
		}

		// Rename actual volume.
		newVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, newVolName)
		err = d.renameLogicalVolume(volDevPath, newVolDevPath)
		if err != nil {
			return err
		}
		revert.Add(func() { d.renameLogicalVolume(newVolDevPath, volDevPath) })

		// Rename volume dir.
		if vol.contentType == ContentTypeFS {
			srcVolumePath := GetVolumeMountPath(d.name, vol.volType, vol.name)
			dstVolumePath := GetVolumeMountPath(d.name, vol.volType, newVolName)
			err = os.Rename(srcVolumePath, dstVolumePath)
			if err != nil {
				return errors.Wrapf(err, "Error renaming LVM logical volume mount path from %q to %q", srcVolumePath, dstVolumePath)
			}
			revert.Add(func() { os.Rename(dstVolumePath, srcVolumePath) })
		}

		// For VMs, also rename the filesystem volume.
		if vol.IsVMBlock() {
			fsVol := vol.NewVMBlockFilesystemVolume()
			err = d.RenameVolume(fsVol, newVolName, op)
			if err != nil {
				return err
			}
		}

		revert.Success()
		return nil
	}, false, op)
}

// MigrateVolume sends a volume for migration.
func (d *lvm) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error {
	// Before doing a generic volume migration, we need to ensure volume (or snap volume parent) is activated
	// to avoid failing with warnings about changing the origin of the snapshot when trying to activate it.
	parent, _, _ := shared.InstanceGetParentAndSnapshotName(vol.Name())
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, parent)
	activated, err := d.activateVolume(volDevPath)
	if err != nil {
		return err
	}
	if activated {
		defer d.deactivateVolume(volDevPath)
	}

	return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
}

// BackupVolume copies a volume (and optionally its snapshots) to a specified target path.
// This driver does not support optimized backups.
func (d *lvm) BackupVolume(vol Volume, tarWriter *instancewriter.InstanceTarWriter, _ bool, snapshots []string, op *operations.Operation) error {
	return genericVFSBackupVolume(d, vol, tarWriter, snapshots, op)
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *lvm) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, snapVol.config, snapVol.poolConfig)
	snapPath := snapVol.MountPath()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	// Create snapshot directory.
	err = snapVol.EnsureMountPath()
	if err != nil {
		return err
	}
	revert.Add(func() { os.RemoveAll(snapPath) })

	_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], parentVol, snapVol, true, d.usesThinpool())
	if err != nil {
		return errors.Wrapf(err, "Error creating LVM logical volume snapshot")
	}

	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)

	revert.Add(func() {
		d.removeLogicalVolume(volDevPath)
	})

	// For VMs, also snapshot the filesystem.
	if snapVol.IsVMBlock() {
		parentFSVol := parentVol.NewVMBlockFilesystemVolume()
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], parentFSVol, fsVol, true, d.usesThinpool())
		if err != nil {
			return errors.Wrapf(err, "Error creating LVM logical volume snapshot")
		}
	}

	revert.Success()
	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device. The volName and snapshotName
// must be bare names and should not be in the format "volume/snapshot".
func (d *lvm) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	// Remove the snapshot from the storage device.
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)
	lvExists, err := d.logicalVolumeExists(volDevPath)
	if err != nil {
		return err
	}

	if lvExists {
		_, err = d.UnmountVolume(snapVol, false, op)
		if err != nil {
			return errors.Wrapf(err, "Error unmounting LVM logical volume")
		}

		err = d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name))
		if err != nil {
			return errors.Wrapf(err, "Error removing LVM logical volume")
		}
	}

	// For VMs, also remove the snapshot filesystem volume.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		err = d.DeleteVolumeSnapshot(fsVol, op)
		if err != nil {
			return err
		}
	}

	// Remove the snapshot mount path from the storage device.
	snapPath := snapVol.MountPath()
	err = os.RemoveAll(snapPath)
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "Error removing LVM snapshot mount path %q", snapPath)
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
func (d *lvm) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	var err error
	mountPath := snapVol.MountPath()

	// Check if already mounted.
	if snapVol.contentType == ContentTypeFS && !shared.IsMountPoint(mountPath) {
		revert := revert.New()
		defer revert.Fail()

		err = snapVol.EnsureMountPath()
		if err != nil {
			return false, err
		}

		// Default to mounting the original snapshot directly. This may be changed below if a temporary
		// snapshot needs to be taken.
		mountVol := snapVol
		mountFlags, mountOptions := resolveMountOptions(mountVol.ConfigBlockMountOptions())

		// Regenerate filesystem UUID if needed. This is because some filesystems do not allow mounting
		// multiple volumes that share the same UUID. As snapshotting a volume will copy its UUID we need
		// to potentially regenerate the UUID of the snapshot now that we are trying to mount it.
		// This is done at mount time rather than snapshot time for 2 reasons; firstly snapshots need to be
		// as fast as possible, and on some filesystems regenerating the UUID is a slow process, secondly
		// we do not want to modify a snapshot in case it is corrupted for some reason, so at mount time
		// we take another snapshot of the snapshot, regenerate the temporary snapshot's UUID and then
		// mount that.
		regenerateFSUUID := renegerateFilesystemUUIDNeeded(snapVol.ConfigBlockFilesystem())
		if regenerateFSUUID {
			// Instantiate a new volume to be the temporary writable snapshot.
			tmpVolName := fmt.Sprintf("%s%s", snapVol.name, tmpVolSuffix)
			tmpVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, tmpVolName, snapVol.config, snapVol.poolConfig)

			// Create writable snapshot from source snapshot named with a tmpVolSuffix suffix.
			_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], snapVol, tmpVol, false, d.usesThinpool())
			if err != nil {
				return false, errors.Wrapf(err, "Error creating temporary LVM logical volume snapshot")
			}

			revert.Add(func() {
				d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], tmpVol.volType, tmpVol.contentType, tmpVol.name))
			})

			// We are going to mount the temporary volume instead.
			mountVol = tmpVol
		}

		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], mountVol.volType, mountVol.contentType, mountVol.name)

		// Activate volume if needed.
		_, err = d.activateVolume(volDevPath)
		if err != nil {
			return false, err
		}

		if regenerateFSUUID {
			tmpVolFsType := mountVol.ConfigBlockFilesystem()

			// When mounting XFS filesystems temporarily we can use the nouuid option rather than fully
			// regenerating the filesystem UUID.
			if tmpVolFsType == "xfs" {
				idx := strings.Index(mountOptions, "nouuid")
				if idx < 0 {
					mountOptions += ",nouuid"
				}
			} else {
				d.logger.Debug("Regenerating filesystem UUID", log.Ctx{"dev": volDevPath, "fs": tmpVolFsType})
				err = regenerateFilesystemUUID(mountVol.ConfigBlockFilesystem(), volDevPath)
				if err != nil {
					return false, err
				}
			}
		}

		// Finally attempt to mount the volume that needs mounting.
		err = TryMount(volDevPath, mountPath, mountVol.ConfigBlockFilesystem(), mountFlags|unix.MS_RDONLY, mountOptions)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to mount LVM snapshot volume")
		}
		d.logger.Debug("Mounted logical volume snapshot", log.Ctx{"dev": volDevPath, "path": mountPath, "options": mountOptions})

		revert.Success()
		return true, nil
	}

	activated := false
	if snapVol.contentType == ContentTypeBlock {
		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)

		// Activate volume if needed.
		activated, err = d.activateVolume(volDevPath)
		if err != nil {
			return false, err
		}
	}

	// For VMs, mount the filesystem volume.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		return d.MountVolumeSnapshot(fsVol, op)
	}

	return activated, nil
}

// UnmountVolumeSnapshot removes the read-only mount placed on top of a snapshot.
// If a temporary snapshot volume exists then it will attempt to remove it.
func (d *lvm) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	var err error
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)
	mountPath := snapVol.MountPath()

	// Check if already mounted.
	if snapVol.contentType == ContentTypeFS && shared.IsMountPoint(mountPath) {
		err = TryUnmount(mountPath, 0)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to unmount LVM snapshot volume")
		}
		d.logger.Debug("Unmounted logical volume snapshot", log.Ctx{"path": mountPath})

		// Check if a temporary snapshot exists, and if so remove it.
		tmpVolName := fmt.Sprintf("%s%s", snapVol.name, tmpVolSuffix)
		tmpVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, tmpVolName)
		exists, err := d.logicalVolumeExists(tmpVolDevPath)
		if err != nil {
			return true, errors.Wrapf(err, "Failed to check existence of temporary LVM snapshot volume %q", tmpVolDevPath)
		}

		if exists {
			err = d.removeLogicalVolume(tmpVolDevPath)
			if err != nil {
				return true, errors.Wrapf(err, "Failed to remove temporary LVM snapshot volume %q", tmpVolDevPath)
			}
		}

		// We only deactivate filesystem volumes if an unmount was needed to better align with our
		// unmount return value indicator.
		_, err = d.deactivateVolume(volDevPath)
		if err != nil {
			return false, err
		}

		return true, nil
	}

	deactivated := false
	if snapVol.contentType == ContentTypeBlock {
		deactivated, err = d.deactivateVolume(volDevPath)
		if err != nil {
			return false, err
		}
	}

	// For VMs, unmount the filesystem volume.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		return d.UnmountVolumeSnapshot(fsVol, op)
	}

	return deactivated, nil
}

// VolumeSnapshots returns a list of snapshots for the volume (in no particular order).
func (d *lvm) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	// We use the volume list rather than inspecting the logical volumes themselves because the origin
	// property of an LVM snapshot can be removed/changed when restoring snapshots, such that they are no
	// marked as origin of the parent volume. Instead we use prefix matching on the volume names to find the
	// snapshot volumes.
	cmd := exec.Command("lvs", "--noheadings", "-o", "lv_name", d.config["lvm.vg_name"])
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	snapshots := []string{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		snapName := d.parseLogicalVolumeSnapshot(vol, strings.TrimSpace(scanner.Text()))
		if snapName == "" {
			continue // Skip logical volumes that are not recognised as a snapshot of our parent vol.
		}

		snapshots = append(snapshots, snapName)
	}

	errMsg, err := ioutil.ReadAll(stderr)
	if err != nil {
		return nil, err
	}

	err = cmd.Wait()
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get snapshot list for volume %q: %v", vol.name, strings.TrimSpace(string(errMsg)))
	}

	return snapshots, nil
}

// RestoreVolume restores a volume from a snapshot.
func (d *lvm) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	// Instantiate snapshot volume from snapshot name.
	snapVol, err := vol.NewSnapshot(snapshotName)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	// If the pool uses thinpools, then the process for restoring a snapshot is as follows:
	// 1. Rename the original volume to a temporary name (so we can revert later if needed).
	// 2. Create a writable snapshot with the original name from the snapshot being restored.
	// 3. Delete the renamed original volume.
	if d.usesThinpool() {
		_, err = d.UnmountVolume(vol, false, op)
		if err != nil {
			return errors.Wrapf(err, "Error unmounting LVM logical volume")
		}

		originalVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
		tmpVolName := fmt.Sprintf("%s%s", vol.name, tmpVolSuffix)
		tmpVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, tmpVolName)

		// Rename original logical volume to temporary new name so we can revert if needed.
		err = d.renameLogicalVolume(originalVolDevPath, tmpVolDevPath)
		if err != nil {
			return errors.Wrapf(err, "Error temporarily renaming original LVM logical volume")
		}

		revert.Add(func() {
			// Rename the original volume back to the original name.
			d.renameLogicalVolume(tmpVolDevPath, originalVolDevPath)
		})

		// Create writable snapshot from source snapshot named as target volume.
		_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], snapVol, vol, false, true)
		if err != nil {
			return errors.Wrapf(err, "Error restoring LVM logical volume snapshot")
		}

		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)

		revert.Add(func() {
			d.removeLogicalVolume(volDevPath)
		})

		// If the volume's filesystem needs to have its UUID regenerated to allow mount then do so now.
		if vol.contentType == ContentTypeFS && renegerateFilesystemUUIDNeeded(vol.ConfigBlockFilesystem()) {
			_, err = d.activateVolume(volDevPath)
			if err != nil {
				return err
			}

			d.logger.Debug("Regenerating filesystem UUID", log.Ctx{"dev": volDevPath, "fs": vol.ConfigBlockFilesystem()})
			err = regenerateFilesystemUUID(vol.ConfigBlockFilesystem(), volDevPath)
			if err != nil {
				return err
			}
		}

		// Finally remove the original logical volume. Should always be the last step to allow revert.
		err = d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, tmpVolName))
		if err != nil {
			return errors.Wrapf(err, "Error removing original LVM logical volume")
		}

		revert.Success()
		return nil
	}

	// If the pool uses classic logical volumes, then the process for restoring a snapshot is as follows:
	// 1. Mount source and target.
	// 2. Rsync source to target.
	// 3. Unmount source and target.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		// Copy source to destination (mounting each volume if needed).
		err = snapVol.MountTask(func(srcMountPath string, op *operations.Operation) error {
			bwlimit := d.config["rsync.bwlimit"]
			_, err := rsync.LocalCopy(srcMountPath, mountPath, bwlimit, true)
			return err
		}, op)
		if err != nil {
			return err
		}

		// Run EnsureMountPath after mounting and syncing to ensure the mounted directory has the
		// correct permissions set.
		err = vol.EnsureMountPath()
		if err != nil {
			return err
		}

		return nil
	}, op)
	if err != nil {
		return errors.Wrapf(err, "Error restoring LVM logical volume snapshot")
	}

	revert.Success()
	return nil
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *lvm) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	newSnapVolName := GetSnapshotVolumeName(parentName, newSnapshotName)
	newVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, newSnapVolName)
	err := d.renameLogicalVolume(volDevPath, newVolDevPath)
	if err != nil {
		return errors.Wrapf(err, "Error renaming LVM logical volume")
	}

	oldPath := snapVol.MountPath()
	newPath := GetVolumeMountPath(d.name, snapVol.volType, newSnapVolName)
	err = os.Rename(oldPath, newPath)
	if err != nil {
		return errors.Wrapf(err, "Error renaming snapshot mount path from %q to %q", oldPath, newPath)
	}

	return nil
}
