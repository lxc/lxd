package drivers

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/db"
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
func (d *ceph) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	// Function to rename an RBD volume.
	renameVolume := func(oldName string, newName string) error {
		_, err := shared.RunCommand(
			"rbd",
			"--id", d.config["ceph.user.name"],
			"--cluster", d.config["ceph.cluster_name"],
			"mv",
			oldName,
			newName,
		)
		return err
	}

	// Revert handling.
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

	// Create a "zombie" deleted volume representation of the specified volume to look for its existence.
	deletedVol := NewVolume(d, d.name, cephVolumeTypeZombieImage, vol.contentType, vol.name, vol.config, vol.poolConfig)

	// Check if we have a deleted zombie image. If so, restore it otherwise create a new image volume.
	if vol.volType == VolumeTypeImage && d.HasVolume(deletedVol) {
		canRestore := true

		// Check if the cached image volume is larger than the current pool volume.size setting
		// (if so we won't be able to resize the snapshot to that the smaller size later).
		volSizeBytes, err := d.getVolumeSize(d.getRBDVolumeName(deletedVol, "", false, true))
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

		// If the cached volume size is different than the pool volume size, then we can't use the
		// deleted cached image volume and instead we will rename it to a random UUID so it can't
		// be restored in the future and a new cached image volume will be created instead.
		if volSizeBytes != poolVolSizeBytes {
			d.logger.Debug("Renaming deleted cached image volume so that regeneration is used", "fingerprint", vol.Name())
			randomVol := NewVolume(d, d.name, deletedVol.volType, deletedVol.contentType, strings.Replace(uuid.NewRandom().String(), "-", "", -1), deletedVol.config, deletedVol.poolConfig)
			err = renameVolume(d.getRBDVolumeName(deletedVol, "", false, true), d.getRBDVolumeName(randomVol, "", false, true))
			if err != nil {
				return err
			}

			if vol.IsVMBlock() {
				fsDeletedVol := deletedVol.NewVMBlockFilesystemVolume()
				fsRandomVol := randomVol.NewVMBlockFilesystemVolume()

				err = renameVolume(d.getRBDVolumeName(fsDeletedVol, "", false, true), d.getRBDVolumeName(fsRandomVol, "", false, true))
				if err != nil {
					return err
				}
			}

			canRestore = false
		}

		// Restore the image.
		if canRestore {
			d.logger.Debug("Restoring previously deleted cached image volume", "fingerprint", vol.Name())
			err = renameVolume(d.getRBDVolumeName(deletedVol, "", false, true), d.getRBDVolumeName(vol, "", false, true))
			if err != nil {
				return err
			}

			if vol.IsVMBlock() {
				fsDeletedVol := deletedVol.NewVMBlockFilesystemVolume()
				fsVol := vol.NewVMBlockFilesystemVolume()

				err = renameVolume(d.getRBDVolumeName(fsDeletedVol, "", false, true), d.getRBDVolumeName(fsVol, "", false, true))
				if err != nil {
					return err
				}
			}

			revert.Success()
			return nil
		}
	}

	// Create volume.
	err := d.rbdCreateVolume(vol, vol.ConfigSize())
	if err != nil {
		return err
	}

	revert.Add(func() { d.DeleteVolume(vol, op) })

	devPath, err := d.rbdMapVolume(vol)
	if err != nil {
		return err
	}

	revert.Add(func() { d.rbdUnmapVolume(vol, true) })

	// Get filesystem.
	RBDFilesystem := vol.ConfigBlockFilesystem()

	if vol.contentType == ContentTypeFS {
		_, err = makeFSType(devPath, RBDFilesystem, nil)
		if err != nil {
			return err
		}
	}

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

	// Create a readonly snapshot of the image volume which will be used a the
	// clone source for future non-image volumes.
	if vol.volType == VolumeTypeImage {
		err = d.rbdUnmapVolume(vol, true)
		if err != nil {
			return err
		}

		err = d.rbdCreateVolumeSnapshot(vol, "readonly")
		if err != nil {
			return err
		}

		revert.Add(func() { d.deleteVolumeSnapshot(vol, "readonly") })

		err = d.rbdProtectVolumeSnapshot(vol, "readonly")
		if err != nil {
			return err
		}

		if vol.contentType == ContentTypeBlock {
			// Re-create the FS config volume's readonly snapshot now that the filler function has run
			// and unpacked into both config and block volumes.
			fsVol := NewVolume(d, d.name, vol.volType, ContentTypeFS, vol.name, vol.config, vol.poolConfig)

			err := d.rbdUnprotectVolumeSnapshot(fsVol, "readonly")
			if err != nil {
				return err
			}

			_, err = d.deleteVolumeSnapshot(fsVol, "readonly")
			if err != nil {
				return err
			}

			err = d.rbdCreateVolumeSnapshot(fsVol, "readonly")
			if err != nil {
				return err
			}

			revert.Add(func() { d.deleteVolumeSnapshot(fsVol, "readonly") })

			err = d.rbdProtectVolumeSnapshot(fsVol, "readonly")
			if err != nil {
				return err
			}
		}
	}

	revert.Success()
	return nil
}

// getVolumeSize returns the volume's size in bytes.
func (d *ceph) getVolumeSize(volumeName string) (int64, error) {
	volInfo := struct {
		Size int64 `json:"size"`
	}{}

	jsonInfo, err := shared.TryRunCommand(
		"rbd",
		"info",
		"--format", "json",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		volumeName,
	)
	if err != nil {
		return -1, err
	}

	err = json.Unmarshal([]byte(jsonInfo), &volInfo)
	if err != nil {
		return -1, err
	}

	return volInfo.Size, nil
}

// CreateVolumeFromBackup re-creates a volume from its exported state.
func (d *ceph) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
	return genericVFSBackupUnpack(d, vol, srcBackup.Snapshots, srcData, op)
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *ceph) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, op *operations.Operation) error {
	var err error
	revert := revert.New()
	defer revert.Fail()

	// Function to run once the volume is created, which will regenerate the filesystem UUID (if needed),
	// ensure permissions on mount path inside the volume are correct, and resize the volume to specified size.
	postCreateTasks := func(v Volume) error {
		// Map the RBD volume.
		devPath, err := d.rbdMapVolume(v)
		if err != nil {
			return err
		}
		defer d.rbdUnmapVolume(v, true)

		if vol.contentType == ContentTypeFS {
			// Re-generate the UUID. Do this first as ensuring permissions and setting quota can
			// rely on being able to mount the volume.
			err = d.generateUUID(v.ConfigBlockFilesystem(), devPath)
			if err != nil {
				return err
			}

			// Mount the volume and ensure the permissions are set correctly inside the mounted volume.
			err = v.MountTask(func(_ string, _ *operations.Operation) error {
				return v.EnsureMountPath()
			}, op)
			if err != nil {
				return err
			}
		}

		// Resize volume to the size specified. Only uses volume "size" property and does not use
		// pool/defaults to give the caller more control over the size being used.
		err = d.SetVolumeQuota(vol, vol.config["size"], false, op)
		if err != nil {
			return err
		}

		return nil
	}

	// Retrieve snapshots on the source.
	snapshots := []string{}
	if !srcVol.IsSnapshot() && copySnapshots {
		snapshots, err = d.VolumeSnapshots(srcVol, op)
		if err != nil {
			return err
		}
	}

	// Copy without snapshots.
	if !copySnapshots || len(snapshots) == 0 {
		// If lightweight clone mode isn't enabled, perform a full copy of the volume.
		if d.config["ceph.rbd.clone_copy"] != "" && !shared.IsTrue(d.config["ceph.rbd.clone_copy"]) {
			_, err = shared.RunCommand(
				"rbd",
				"--id", d.config["ceph.user.name"],
				"--cluster", d.config["ceph.cluster_name"],
				"cp",
				d.getRBDVolumeName(srcVol, "", false, true),
				d.getRBDVolumeName(vol, "", false, true),
			)
			if err != nil {
				return err
			}

			revert.Add(func() { d.DeleteVolume(vol, op) })

			_, err = d.rbdMapVolume(vol)
			if err != nil {
				return err
			}

			revert.Add(func() { d.rbdUnmapVolume(vol, true) })
		} else {
			parentVol := srcVol
			snapshotName := "readonly"

			if srcVol.volType != VolumeTypeImage {
				snapshotName = fmt.Sprintf("zombie_snapshot_%s", uuid.NewRandom().String())

				if srcVol.IsSnapshot() {
					srcParentName, srcSnapOnlyName, _ := shared.InstanceGetParentAndSnapshotName(srcVol.name)
					snapshotName = fmt.Sprintf("snapshot_%s", srcSnapOnlyName)
					parentVol = NewVolume(d, d.name, srcVol.volType, srcVol.contentType, srcParentName, nil, nil)
				} else {
					// Create snapshot.
					err := d.rbdCreateVolumeSnapshot(srcVol, snapshotName)
					if err != nil {
						return err
					}
				}

				// Protect volume so we can create clones of it.
				err = d.rbdProtectVolumeSnapshot(parentVol, snapshotName)
				if err != nil {
					return err
				}

				revert.Add(func() { d.rbdUnprotectVolumeSnapshot(parentVol, snapshotName) })
			}

			err = d.rbdCreateClone(parentVol, snapshotName, vol)
			if err != nil {
				return err
			}

			revert.Add(func() { d.DeleteVolume(vol, op) })
		}

		// For VMs, also copy the filesystem volume.
		if vol.IsVMBlock() {
			srcFSVol := srcVol.NewVMBlockFilesystemVolume()
			fsVol := vol.NewVMBlockFilesystemVolume()
			err := d.CreateVolumeFromCopy(fsVol, srcFSVol, false, op)
			if err != nil {
				return err
			}
		}

		err = postCreateTasks(vol)
		if err != nil {
			return err
		}

		revert.Success()
		return nil
	}

	// Copy with snapshots.

	// Create empty placeholder volume
	err = d.rbdCreateVolume(vol, "0")
	if err != nil {
		return err
	}

	revert.Add(func() { d.rbdDeleteVolume(vol) })

	// Receive over the placeholder volume we created above.
	targetVolumeName := d.getRBDVolumeName(vol, "", false, true)

	lastSnap := ""

	if len(snapshots) > 0 {
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}
	}

	for i, snap := range snapshots {
		prev := ""
		if i > 0 {
			prev = fmt.Sprintf("snapshot_%s", snapshots[i-1])
		}

		lastSnap = fmt.Sprintf("snapshot_%s", snap)
		sourceVolumeName := d.getRBDVolumeName(srcVol, lastSnap, false, true)
		err = d.copyWithSnapshots(sourceVolumeName, targetVolumeName, prev)
		if err != nil {
			return err
		}

		revert.Add(func() { d.rbdDeleteVolumeSnapshot(vol, snap) })

		snapVol, err := vol.NewSnapshot(snap)
		if err != nil {
			return err
		}

		err = snapVol.EnsureMountPath()
		if err != nil {
			return err
		}
	}

	// Copy snapshot.
	sourceVolumeName := d.getRBDVolumeName(srcVol, "", false, true)

	err = d.copyWithSnapshots(sourceVolumeName, targetVolumeName, lastSnap)
	if err != nil {
		return err
	}

	err = postCreateTasks(vol)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *ceph) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	// Handle simple rsync and block_and_rsync through generic.
	if volTargetArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volTargetArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		return genericVFSCreateVolumeFromMigration(d, nil, vol, conn, volTargetArgs, preFiller, op)
	} else if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_RBD {
		return ErrNotSupported
	}

	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.CreateVolumeFromMigration(fsVol, conn, volTargetArgs, preFiller, op)
		if err != nil {
			return err
		}
	}

	recvName := d.getRBDVolumeName(vol, "", false, true)

	if !d.HasVolume(vol) {
		err := d.rbdCreateVolume(vol, "0")
		if err != nil {
			return err
		}
	}

	err := vol.EnsureMountPath()
	if err != nil {
		return err
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
			fullSnapshotName := d.getRBDVolumeName(vol, snapName, false, true)
			wrapper := migration.ProgressWriter(op, "fs_progress", fullSnapshotName)

			err = d.receiveVolume(recvName, conn, wrapper)
			if err != nil {
				return err
			}

			snapVol, err := vol.NewSnapshot(snapName)
			if err != nil {
				return err
			}

			err = snapVol.EnsureMountPath()
			if err != nil {
				return err
			}
		}
	}

	defer func() {
		// Delete all migration-send-* snapshots.
		snaps, err := d.rbdListVolumeSnapshots(vol)
		if err != nil {
			return
		}

		for _, snap := range snaps {
			if !strings.HasPrefix(snap, "migration-send") {
				continue
			}

			d.rbdDeleteVolumeSnapshot(vol, snap)
		}
	}()

	wrapper := migration.ProgressWriter(op, "fs_progress", vol.name)

	err = d.receiveVolume(recvName, conn, wrapper)
	if err != nil {
		return err
	}

	if volTargetArgs.Live {
		err = d.receiveVolume(recvName, conn, wrapper)
		if err != nil {
			return err
		}
	}

	// Map the RBD volume.
	devPath, err := d.rbdMapVolume(vol)
	if err != nil {
		return err
	}
	defer d.rbdUnmapVolume(vol, true)

	// Re-generate the UUID.
	err = d.generateUUID(vol.ConfigBlockFilesystem(), devPath)
	if err != nil {
		return err
	}

	return nil
}

// RefreshVolume updates an existing volume to match the state of another.
func (d *ceph) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	return genericVFSCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, op)
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
func (d *ceph) DeleteVolume(vol Volume, op *operations.Operation) error {
	if !d.HasVolume(vol) {
		return nil
	}

	if vol.volType == VolumeTypeImage {
		// Unmount and unmap.
		_, err := d.UnmountVolume(vol, false, op)
		if err != nil {
			return err
		}

		hasReadonlySnapshot := d.hasVolume(d.getRBDVolumeName(vol, "readonly", false, false))
		hasDependendantSnapshots := false

		if hasReadonlySnapshot {
			dependantSnapshots, err := d.rbdListSnapshotClones(vol, "readonly")
			if err != nil && err != db.ErrNoSuchObject {
				return err
			}

			hasDependendantSnapshots = len(dependantSnapshots) > 0
		}

		if hasDependendantSnapshots {
			// If the image has dependant snapshots, then we just mark it as deleted, but don't
			// actually remove it yet.
			err = d.rbdMarkVolumeDeleted(vol, vol.name)
			if err != nil {
				return err
			}
		} else {
			if hasReadonlySnapshot {
				// Unprotect snapshot.
				err := d.rbdUnprotectVolumeSnapshot(vol, "readonly")
				if err != nil {
					return err
				}
			}

			// Delete snapshots.
			_, err := shared.RunCommand(
				"rbd",
				"--id", d.config["ceph.user.name"],
				"--cluster", d.config["ceph.cluster_name"],
				"--pool", d.config["ceph.osd.pool_name"],
				"snap",
				"purge",
				d.getRBDVolumeName(vol, "", false, false))
			if err != nil {
				return err
			}

			// Delete image.
			err = d.rbdDeleteVolume(vol)
			if err != nil {
				return err
			}
		}
	} else {
		// Unmount and unmap.
		_, err := d.UnmountVolume(vol, false, op)
		if err != nil {
			return err
		}

		_, err = d.deleteVolume(vol)
		if err != nil {
			return errors.Wrap(err, "Failed to delete volume")
		}
	}

	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()

		err := d.DeleteVolume(fsVol, op)
		if err != nil {
			return err
		}
	}

	mountPath := vol.MountPath()

	if vol.contentType == ContentTypeFS && shared.PathExists(mountPath) {
		err := wipeDirectory(mountPath)
		if err != nil {
			return err
		}

		err = os.Remove(mountPath)
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "Failed to remove '%s'", mountPath)
		}
	}

	return nil
}

// hasVolume indicates whether a specific RBD volume exists on the storage pool.
func (d *ceph) hasVolume(rbdVolumeName string) bool {
	_, err := shared.RunCommand(
		"rbd",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		"info",
		rbdVolumeName,
	)

	return err == nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *ceph) HasVolume(vol Volume) bool {
	return d.hasVolume(d.getRBDVolumeName(vol, "", false, false))
}

// FillVolumeConfig populate volume with default config.
func (d *ceph) FillVolumeConfig(vol Volume) error {
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
func (d *ceph) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	rules := map[string]func(value string) error{
		"block.filesystem":    validate.IsAny,
		"block.mount_options": validate.IsAny,
	}

	return d.validateVolume(vol, rules, removeUnknownKeys)
}

// UpdateVolume applies config changes to the volume.
func (d *ceph) UpdateVolume(vol Volume, changedConfig map[string]string) error {
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
func (d *ceph) GetVolumeUsage(vol Volume) (int64, error) {
	isSnap := vol.IsSnapshot()

	// If mounted, use the filesystem stats for pretty accurate usage information.
	if !isSnap && vol.contentType == ContentTypeFS && shared.IsMountPoint(vol.MountPath()) {
		var stat unix.Statfs_t

		err := unix.Statfs(vol.MountPath(), &stat)
		if err != nil {
			return -1, err
		}

		return int64(stat.Blocks-stat.Bfree) * int64(stat.Bsize), nil
	}

	// If not mounted (or not mountable), query the usage from ceph directly.
	// This is rather inaccurate as there is no way to get the size of a
	// volume without its snapshots. Instead we need to merge the size
	// of all snapshots with the delta since last snapshot. This leads to
	// volumes with lots of changes between snapshots potentially adding up far
	// more usage than they actually have.
	type cephDuLine struct {
		Name            string `json:"name"`
		Snapshot        string `json:"snapshot"`
		ProvisionedSize int64  `json:"provisioned_size"`
		UsedSize        int64  `json:"used_size"`
	}

	type cephDuInfo struct {
		Images []cephDuLine `json:"images"`
	}

	jsonInfo, err := shared.TryRunCommand(
		"rbd",
		"du",
		"--format", "json",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		d.getRBDVolumeName(vol, "", false, false),
	)
	if err != nil {
		return -1, err
	}

	var usedSize int64
	var result cephDuInfo

	err = json.Unmarshal([]byte(jsonInfo), &result)
	if err != nil {
		return -1, err
	}

	_, snapName, _ := shared.InstanceGetParentAndSnapshotName(vol.Name())
	snapName = fmt.Sprintf("snapshot_%s", snapName)

	// rbd du gives the output of all related rbd images, snapshots included.
	for _, image := range result.Images {
		if isSnap {
			// For snapshot volumes we only want to get the specific image used so we can
			// indicate how much CoW usage that snapshot has.
			if image.Snapshot == snapName {
				usedSize = image.UsedSize
				break
			}
		} else {
			// For non-snapshot volumes, to get the total size of the volume we need to add up
			// all of the image's usage.
			usedSize += image.UsedSize
		}
	}

	return usedSize, nil
}

// SetVolumeQuota applies a size limit on volume.
// Does nothing if supplied with an empty/zero size.
func (d *ceph) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	// Convert to bytes.
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	// Do nothing if size isn't specified.
	if sizeBytes <= 0 {
		return nil
	}

	ourMap, devPath, err := d.getRBDMappedDevPath(vol, true)
	if err != nil {
		return err
	}

	if ourMap {
		defer d.rbdUnmapVolume(vol, true)
	}

	oldSizeBytes, err := BlockDiskSizeBytes(devPath)
	if err != nil {
		return errors.Wrapf(err, "Error getting current size")
	}

	// Do nothing if volume is already specified size (+/- 512 bytes).
	if oldSizeBytes+512 > sizeBytes && oldSizeBytes-512 < sizeBytes {
		return nil
	}

	// Block image volumes cannot be resized because they have a readonly snapshot that doesn't get
	// updated when the volume's size is changed, and this is what instances are created from.
	// During initial volume fill allowUnsafeResize is enabled because snapshot hasn't been taken yet.
	if !allowUnsafeResize && vol.volType == VolumeTypeImage {
		return ErrNotSupported
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

			// Shrink filesystem first. Pass allowUnsafeResize to allow disabling of filesystem
			// resize safety checks.
			err = shrinkFileSystem(fsType, devPath, vol, sizeBytes, allowUnsafeResize)
			if err != nil {
				return err
			}

			// Shrink the block device.
			err = d.resizeVolume(vol, sizeBytes, true)
			if err != nil {
				return err
			}
		} else if sizeBytes > oldSizeBytes {
			// Grow block device first.
			err = d.resizeVolume(vol, sizeBytes, false)
			if err != nil {
				return err
			}

			// Grow the filesystem to fill block device.
			err = growFileSystem(fsType, devPath, vol)
			if err != nil {
				return err
			}
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

		// Resize block device.
		err = d.resizeVolume(vol, sizeBytes, allowUnsafeResize)
		if err != nil {
			return err
		}

		// Move the VM GPT alt header to end of disk if needed (not needed in unsafe resize mode as it is
		// expected the caller will do all necessary post resize actions themselves).
		if vol.IsVMBlock() && !allowUnsafeResize {
			err = d.moveGPTAltHeader(devPath)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// GetVolumeDiskPath returns the location of a root disk block device.
func (d *ceph) GetVolumeDiskPath(vol Volume) (string, error) {
	if vol.IsVMBlock() || vol.volType == VolumeTypeCustom && vol.contentType == ContentTypeBlock {
		_, devPath, err := d.getRBDMappedDevPath(vol, false)
		return devPath, err
	}

	return "", ErrNotSupported
}

// ListVolumes returns a list of LXD volumes in storage pool.
func (d *ceph) ListVolumes() ([]Volume, error) {
	return nil, ErrNotImplemented
}

// MountVolume mounts a volume and increments ref counter. Please call UnmountVolume() when done with the volume.
func (d *ceph) MountVolume(vol Volume, op *operations.Operation) error {
	unlock := vol.MountLock()
	defer unlock()

	revert := revert.New()
	defer revert.Fail()

	// Activate RBD volume if needed.
	activated, devPath, err := d.getRBDMappedDevPath(vol, true)
	if err != nil {
		return err
	}
	if activated {
		revert.Add(func() { d.rbdUnmapVolume(vol, true) })
	}

	if vol.contentType == ContentTypeFS {
		mountPath := vol.MountPath()
		if !shared.IsMountPoint(mountPath) {
			err := vol.EnsureMountPath()
			if err != nil {
				return err
			}

			RBDFilesystem := vol.ConfigBlockFilesystem()
			mountFlags, mountOptions := resolveMountOptions(vol.ConfigBlockMountOptions())
			err = TryMount(devPath, mountPath, RBDFilesystem, mountFlags, mountOptions)
			if err != nil {
				return err
			}

			d.logger.Debug("Mounted RBD volume", log.Ctx{"dev": devPath, "path": mountPath, "options": mountOptions})
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

// UnmountVolume simulates unmounting a volume.
// keepBlockDev indicates if backing block device should be not be unmapped if volume is unmounted.
func (d *ceph) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock := vol.MountLock()
	defer unlock()

	ourUnmount := false
	refCount := vol.MountRefCountDecrement()

	// Attempt to unmount the volume.
	if vol.contentType == ContentTypeFS {
		mountPath := vol.MountPath()
		if shared.IsMountPoint(mountPath) {
			if refCount > 0 {
				d.logger.Debug("Skipping unmount as in use", "refCount", refCount)
				return false, ErrInUse
			}

			err := TryUnmount(mountPath, unix.MNT_DETACH)
			if err != nil {
				return false, err
			}
			d.logger.Debug("Unmounted RBD volume", log.Ctx{"path": mountPath, "keepBlockDev": keepBlockDev})

			// Attempt to unmap.
			if !keepBlockDev {
				err = d.rbdUnmapVolume(vol, true)
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
			_, err := d.UnmountVolume(fsVol, false, op)
			if err != nil {
				return false, err
			}
		}

		if !keepBlockDev {
			// Check if device is currently mapped (but don't map if not).
			_, devPath, _ := d.getRBDMappedDevPath(vol, false)
			if devPath != "" && shared.PathExists(devPath) {
				if refCount > 0 {
					d.logger.Debug("Skipping unmount as in use", "refCount", refCount)
					return false, ErrInUse
				}

				// Attempt to unmap.
				err := d.rbdUnmapVolume(vol, true)
				if err != nil {
					return false, err
				}

				ourUnmount = true
			}
		}
	}

	return ourUnmount, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *ceph) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	return vol.UnmountTask(func(op *operations.Operation) error {
		revert := revert.New()
		defer revert.Fail()

		err := d.rbdRenameVolume(vol, newVolName)
		if err != nil {
			return err
		}

		newVol := NewVolume(d, d.name, vol.volType, vol.contentType, newVolName, nil, nil)
		revert.Add(func() { d.rbdRenameVolume(newVol, vol.name) })

		// Rename volume dir.
		if vol.contentType == ContentTypeFS {
			err = genericVFSRenameVolume(d, vol, newVolName, op)
			if err != nil {
				return err
			}
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
func (d *ceph) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error {
	// If data is set, this request is coming from the clustering code.
	// In this case, we only need to unmap and rename the rbd image.
	if volSrcArgs.Data != nil {
		data, ok := volSrcArgs.Data.(string)
		if ok {
			err := d.rbdUnmapVolume(vol, true)
			if err != nil {
				return err
			}

			// Rename volume.
			if vol.name != data {
				err = d.rbdRenameVolume(vol, data)
				if err != nil {
					return err
				}
			}

			return nil
		}
	}

	// Handle simple rsync and block_and_rsync through generic.
	if volSrcArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volSrcArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		// Before doing a generic volume migration, we need to ensure volume (or snap volume parent) is
		// activated to avoid issues activating the snapshot volume device.
		parent, _, _ := shared.InstanceGetParentAndSnapshotName(vol.Name())
		parentVol := NewVolume(d, d.Name(), vol.volType, vol.contentType, parent, vol.config, vol.poolConfig)
		err := d.MountVolume(parentVol, op)
		if err != nil {
			return err
		}
		defer d.UnmountVolume(parentVol, false, op)

		return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
	} else if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_RBD {
		return ErrNotSupported
	}

	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.MigrateVolume(fsVol, conn, volSrcArgs, op)
		if err != nil {
			return err
		}
	}

	if vol.IsSnapshot() {
		parentName, snapOnlyName, _ := shared.InstanceGetParentAndSnapshotName(vol.name)
		sendName := fmt.Sprintf("%s/snapshots_%s_%s_start_clone", d.name, parentName, snapOnlyName)

		cloneVol := NewVolume(d, d.name, vol.volType, vol.contentType, vol.name, nil, nil)

		// Mounting the volume snapshot will create the clone "snapshots_<parent>_<snap>_start_clone".
		_, err := d.MountVolumeSnapshot(cloneVol, op)
		if err != nil {
			return err
		}
		defer d.UnmountVolumeSnapshot(cloneVol, op)

		// Setup progress tracking.
		var wrapper *ioprogress.ProgressTracker
		if volSrcArgs.TrackProgress {
			wrapper = migration.ProgressTracker(op, "fs_progress", vol.name)
		}

		err = d.sendVolume(conn, sendName, "", wrapper)
		if err != nil {
			return err
		}

		return nil
	}

	lastSnap := ""

	if !volSrcArgs.FinalSync {
		for i, snapName := range volSrcArgs.Snapshots {
			snapshot, _ := vol.NewSnapshot(snapName)

			prev := ""

			if i > 0 {
				prev = fmt.Sprintf("snapshot_%s", volSrcArgs.Snapshots[i-1])
			}

			lastSnap = fmt.Sprintf("snapshot_%s", snapName)
			sendSnapName := d.getRBDVolumeName(vol, lastSnap, false, true)

			// Setup progress tracking.
			var wrapper *ioprogress.ProgressTracker

			if volSrcArgs.TrackProgress {
				wrapper = migration.ProgressTracker(op, "fs_progress", snapshot.name)
			}

			err := d.sendVolume(conn, sendSnapName, prev, wrapper)
			if err != nil {
				return err
			}
		}
	}

	// Setup progress tracking.
	var wrapper *ioprogress.ProgressTracker
	if volSrcArgs.TrackProgress {
		wrapper = migration.ProgressTracker(op, "fs_progress", vol.name)
	}

	runningSnapName := fmt.Sprintf("migration-send-%s", uuid.NewRandom().String())

	err := d.rbdCreateVolumeSnapshot(vol, runningSnapName)
	if err != nil {
		return err
	}
	defer d.rbdDeleteVolumeSnapshot(vol, runningSnapName)

	cur := d.getRBDVolumeName(vol, runningSnapName, false, true)

	err = d.sendVolume(conn, cur, lastSnap, wrapper)
	if err != nil {
		return err
	}

	return nil
}

// BackupVolume creates an exported version of a volume.
func (d *ceph) BackupVolume(vol Volume, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots []string, op *operations.Operation) error {
	return genericVFSBackupVolume(d, vol, tarWriter, snapshots, op)
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *ceph) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	parentName, snapshotOnlyName, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	sourcePath := GetVolumeMountPath(d.name, snapVol.volType, parentName)
	snapshotName := fmt.Sprintf("snapshot_%s", snapshotOnlyName)

	if shared.IsMountPoint(sourcePath) {
		// This is costly but we need to ensure that all cached data has
		// been committed to disk. If we don't then the rbd snapshot of
		// the underlying filesystem can be inconsistent or - worst case
		// - empty.
		unix.Sync()

		_, err := shared.TryRunCommand("fsfreeze", "--freeze", sourcePath)
		if err == nil {
			defer shared.TryRunCommand("fsfreeze", "--unfreeze", sourcePath)
		}
	}

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	err = snapVol.EnsureMountPath()
	if err != nil {
		return err
	}

	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, nil, nil)

	err = d.rbdCreateVolumeSnapshot(parentVol, snapshotName)
	if err != nil {
		return err
	}

	revert.Add(func() { d.DeleteVolumeSnapshot(snapVol, op) })

	// For VM images, create a filesystem volume too.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		err := d.CreateVolumeSnapshot(fsVol, op)
		if err != nil {
			return err
		}

		revert.Add(func() { d.DeleteVolumeSnapshot(fsVol, op) })
	}

	revert.Success()
	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device.
func (d *ceph) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	// Check if snapshot exists, and return if not.
	_, err := shared.RunCommand(
		"rbd",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		"info",
		d.getRBDVolumeName(snapVol, "", false, false))
	if err != nil {
		return nil
	}

	parentName, snapshotOnlyName, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	snapshotName := fmt.Sprintf("snapshot_%s", snapshotOnlyName)

	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, nil, nil)

	_, err = d.deleteVolumeSnapshot(parentVol, snapshotName)
	if err != nil {
		return errors.Wrap(err, "Failed to delete volume snapshot")
	}

	mountPath := snapVol.MountPath()

	if snapVol.contentType == ContentTypeFS && shared.PathExists(mountPath) {
		err = wipeDirectory(mountPath)
		if err != nil {
			return err
		}

		err = os.Remove(mountPath)
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "Failed to remove '%s'", mountPath)
		}
	}

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	err = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	// For VM images, delete the filesystem volume too.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		err := d.DeleteVolumeSnapshot(fsVol, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// MountVolumeSnapshot simulates mounting a volume snapshot.
func (d *ceph) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	mountPath := snapVol.MountPath()

	if snapVol.contentType == ContentTypeFS && !shared.IsMountPoint(mountPath) {
		revert := revert.New()
		defer revert.Fail()

		err := snapVol.EnsureMountPath()
		if err != nil {
			return false, err
		}

		parentName, snapshotOnlyName, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
		prefixedSnapOnlyName := fmt.Sprintf("snapshot_%s", snapshotOnlyName)

		parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, nil, nil)

		// Protect snapshot to prevent data loss.
		err = d.rbdProtectVolumeSnapshot(parentVol, prefixedSnapOnlyName)
		if err != nil {
			return false, err
		}

		revert.Add(func() { d.rbdUnprotectVolumeSnapshot(parentVol, prefixedSnapOnlyName) })

		// Clone snapshot.
		cloneName := fmt.Sprintf("%s_%s_start_clone", parentName, snapshotOnlyName)
		cloneVol := NewVolume(d, d.name, VolumeType("snapshots"), ContentTypeFS, cloneName, nil, nil)

		err = d.rbdCreateClone(parentVol, prefixedSnapOnlyName, cloneVol)
		if err != nil {
			return false, err
		}

		revert.Add(func() { d.rbdDeleteVolume(cloneVol) })

		// Map volume.
		rbdDevPath, err := d.rbdMapVolume(cloneVol)
		if err != nil {
			return false, err
		}

		revert.Add(func() { d.rbdUnmapVolume(cloneVol, true) })

		if shared.IsMountPoint(mountPath) {
			return false, nil
		}

		RBDFilesystem := snapVol.ConfigBlockFilesystem()
		mountFlags, mountOptions := resolveMountOptions(snapVol.ConfigBlockMountOptions())

		if renegerateFilesystemUUIDNeeded(RBDFilesystem) {
			if RBDFilesystem == "xfs" {
				idx := strings.Index(mountOptions, "nouuid")
				if idx < 0 {
					mountOptions += ",nouuid"
				}
			} else {
				err = d.generateUUID(RBDFilesystem, rbdDevPath)
				if err != nil {
					return false, err
				}
			}
		}

		err = TryMount(rbdDevPath, mountPath, RBDFilesystem, mountFlags, mountOptions)
		if err != nil {
			return false, err
		}
		d.logger.Debug("Mounted RBD volume snapshot", log.Ctx{"dev": rbdDevPath, "path": mountPath, "options": mountOptions})

		revert.Success()

		return true, nil
	}

	var err error
	ourMount := false
	if snapVol.contentType == ContentTypeBlock {
		// Activate RBD volume if needed.
		ourMount, _, err = d.getRBDMappedDevPath(snapVol, true)
		if err != nil {
			return false, err
		}
	}

	// For VMs, mount the filesystem volume.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		return d.MountVolumeSnapshot(fsVol, op)
	}

	return ourMount, nil
}

// UnmountVolume simulates unmounting a volume snapshot.
func (d *ceph) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock := snapVol.MountLock()
	defer unlock()

	mountPath := snapVol.MountPath()

	if snapVol.contentType == ContentTypeFS && shared.IsMountPoint(mountPath) {
		err := TryUnmount(mountPath, unix.MNT_DETACH)
		if err != nil {
			return false, err
		}
		d.logger.Debug("Unmounted RBD volume snapshot", log.Ctx{"path": mountPath})

		parentName, snapshotOnlyName, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
		cloneName := fmt.Sprintf("%s_%s_start_clone", parentName, snapshotOnlyName)
		cloneVol := NewVolume(d, d.name, VolumeType("snapshots"), ContentTypeFS, cloneName, nil, nil)

		err = d.rbdUnmapVolume(cloneVol, true)
		if err != nil {
			return false, err
		}

		if !d.HasVolume(cloneVol) {
			return true, nil
		}

		// Delete the temporary RBD volume.
		err = d.rbdDeleteVolume(cloneVol)
		if err != nil {
			return false, err
		}
	}

	if snapVol.contentType == ContentTypeBlock {
		err := d.rbdUnmapVolume(snapVol, true)
		if err != nil {
			return false, err
		}
	}

	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		return d.UnmountVolumeSnapshot(fsVol, op)
	}

	return true, nil
}

// VolumeSnapshots returns a list of snapshots for the volume (in no particular order).
func (d *ceph) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	snapshots, err := d.rbdListVolumeSnapshots(vol)
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil, nil
		}

		return nil, err
	}

	var ret []string

	for _, snap := range snapshots {
		// Ignore zombie snapshots as these are only used internally and
		// not relevant for users.
		if strings.HasPrefix(snap, "zombie_") || strings.HasPrefix(snap, "migration-send-") {
			continue
		}

		ret = append(ret, strings.TrimPrefix(snap, "snapshot_"))
	}

	return ret, nil
}

// RestoreVolume restores a volume from a snapshot.
func (d *ceph) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	ourUmount, err := d.UnmountVolume(vol, false, op)
	if err != nil {
		return err
	}

	if ourUmount {
		defer d.MountVolume(vol, op)
	}

	_, err = shared.RunCommand(
		"rbd",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		"snap",
		"rollback",
		"--snap", fmt.Sprintf("snapshot_%s", snapshotName),
		d.getRBDVolumeName(vol, "", false, false))
	if err != nil {
		return err
	}

	snapVol, err := vol.NewSnapshot(snapshotName)
	if err != nil {
		return err
	}

	// Map the RBD volume.
	devPath, err := d.rbdMapVolume(snapVol)
	if err != nil {
		return err
	}
	defer d.rbdUnmapVolume(snapVol, true)

	// Re-generate the UUID.
	err = d.generateUUID(snapVol.ConfigBlockFilesystem(), devPath)
	if err != nil {
		return err
	}
	return nil
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *ceph) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	parentName, snapshotOnlyName, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	oldSnapOnlyName := fmt.Sprintf("snapshot_%s", snapshotOnlyName)
	newSnapOnlyName := fmt.Sprintf("snapshot_%s", newSnapshotName)

	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, nil, nil)

	err := d.rbdRenameVolumeSnapshot(parentVol, oldSnapOnlyName, newSnapOnlyName)
	if err != nil {
		return err
	}

	revert.Add(func() { d.rbdRenameVolumeSnapshot(parentVol, newSnapOnlyName, oldSnapOnlyName) })

	if snapVol.contentType == ContentTypeFS {
		err = genericVFSRenameVolumeSnapshot(d, snapVol, newSnapshotName, op)
		if err != nil {
			return err
		}
	}

	// For VM images, create a filesystem volume too.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		err := d.RenameVolumeSnapshot(fsVol, newSnapshotName, op)
		if err != nil {
			return err
		}

		revert.Add(func() {
			newFsVol := NewVolume(d, d.name, snapVol.volType, ContentTypeFS, fmt.Sprintf("%s/%s", parentName, newSnapshotName), snapVol.config, snapVol.poolConfig)
			d.RenameVolumeSnapshot(newFsVol, snapVol.name, op)
		})
	}

	revert.Success()
	return nil
}
