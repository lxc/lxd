package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	yaml "gopkg.in/yaml.v2"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/lifecycle"
	"github.com/lxc/lxd/lxd/locking"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/lxd/storage/memorypipe"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/instancewriter"
	"github.com/lxc/lxd/shared/ioprogress"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
)

type lxdBackend struct {
	driver drivers.Driver
	id     int64
	db     api.StoragePool
	name   string
	state  *state.State
	logger logger.Logger
	nodes  map[int64]db.StoragePoolNode
}

// ID returns the storage pool ID.
func (b *lxdBackend) ID() int64 {
	return b.id
}

// Name returns the storage pool name.
func (b *lxdBackend) Name() string {
	return b.name
}

// Description returns the storage pool description.
func (b *lxdBackend) Description() string {
	return b.db.Description
}

// Status returns the storage pool status.
func (b *lxdBackend) Status() string {
	return b.db.Status
}

// LocalStatus returns storage pool status of the local cluster member.
func (b *lxdBackend) LocalStatus() string {
	node, exists := b.nodes[b.state.Cluster.GetNodeID()]
	if !exists {
		return api.StoragePoolStatusUnknown
	}

	return db.StoragePoolStateToAPIStatus(node.State)
}

// Driver returns the storage pool driver.
func (b *lxdBackend) Driver() drivers.Driver {
	return b.driver
}

// MigrationTypes returns the migration transport method preferred when sending a migration,
// based on the migration method requested by the driver's ability.
func (b *lxdBackend) MigrationTypes(contentType drivers.ContentType, refresh bool) []migration.Type {
	return b.driver.MigrationTypes(contentType, refresh)
}

// Create creates the storage pool layout on the storage device.
// localOnly is used for clustering where only a single node should do remote storage setup.
func (b *lxdBackend) Create(clientType request.ClientType, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"config": b.db.Config, "description": b.db.Description, "clientType": clientType})
	logger.Debug("create started")
	defer logger.Debug("create finished")

	revert := revert.New()
	defer revert.Fail()

	path := drivers.GetPoolMountPath(b.name)

	if shared.IsDir(path) {
		return fmt.Errorf("Storage pool directory %q already exists", path)
	}

	// Create the storage path.
	err := os.MkdirAll(path, 0711)
	if err != nil {
		return errors.Wrapf(err, "Failed to create storage pool directory %q", path)
	}

	revert.Add(func() { os.RemoveAll(path) })

	if b.driver.Info().Remote && clientType != request.ClientTypeNormal {
		if !b.driver.Info().MountedRoot {
			// Create the directory structure.
			err = b.createStorageStructure(path)
			if err != nil {
				return err
			}
		}

		// Dealing with a remote storage pool, we're done now.
		revert.Success()
		return nil
	}

	// Validate config.
	err = b.driver.Validate(b.db.Config)
	if err != nil {
		return err
	}

	// Create the storage pool on the storage device.
	err = b.driver.Create()
	if err != nil {
		return err
	}

	// Mount the storage pool.
	ourMount, err := b.driver.Mount()
	if err != nil {
		return err
	}

	// We expect the caller of create to mount the pool if needed, so we should unmount after
	// storage struct has been created.
	if ourMount {
		defer b.driver.Unmount()
	}

	// Create the directory structure.
	err = b.createStorageStructure(path)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// newVolume returns a new Volume instance containing copies of the supplied volume config and the pools config,
func (b *lxdBackend) newVolume(volType drivers.VolumeType, contentType drivers.ContentType, volName string, volConfig map[string]string) drivers.Volume {
	// Copy the config map to avoid internal modifications affecting external state.
	newConfig := map[string]string{}
	for k, v := range volConfig {
		newConfig[k] = v
	}

	// Copy the pool config map to avoid internal modifications affecting external state.
	newPoolConfig := map[string]string{}
	for k, v := range b.db.Config {
		newPoolConfig[k] = v
	}

	return drivers.NewVolume(b.driver, b.name, volType, contentType, volName, newConfig, newPoolConfig)
}

// GetResources returns utilisation information about the pool.
func (b *lxdBackend) GetResources() (*api.ResourcesStoragePool, error) {
	logger := logging.AddContext(b.logger, nil)
	logger.Debug("GetResources started")
	defer logger.Debug("GetResources finished")

	return b.driver.GetResources()
}

// IsUsed returns whether the storage pool is used by any volumes or profiles (excluding image volumes).
func (b *lxdBackend) IsUsed() (bool, error) {
	// Get all users of the storage pool.
	var err error
	poolUsedBy := []string{}
	err = b.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		poolUsedBy, err = tx.GetStoragePoolUsedBy(b.name)
		return err
	})
	if err != nil {
		return false, err
	}

	for _, entry := range poolUsedBy {
		// Images are never considered a user of the pool.
		if strings.HasPrefix(entry, "/1.0/images/") {
			continue
		}

		return true, nil
	}

	return false, nil
}

// Update updates the pool config.
func (b *lxdBackend) Update(clientType request.ClientType, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("Update started")
	defer logger.Debug("Update finished")

	// Validate config.
	err := b.driver.Validate(newConfig)
	if err != nil {
		return err
	}

	// Diff the configurations.
	changedConfig, userOnly := b.detectChangedConfig(b.db.Config, newConfig)

	// Check if the pool source is being changed that the local state is still pending, otherwise prevent it.
	_, sourceChanged := changedConfig["source"]
	if sourceChanged && b.LocalStatus() != api.StoragePoolStatusPending {
		return fmt.Errorf("Pool source cannot be changed when not in pending state")
	}

	// Apply changes to local node if both global pool and node are not pending and non-user config changed.
	// Otherwise just apply changes to DB (below) ready for the actual global create request to be initiated.
	if len(changedConfig) > 0 && b.Status() != api.StoragePoolStatusPending && b.LocalStatus() != api.StoragePoolStatusPending && !userOnly {
		err = b.driver.Update(changedConfig)
		if err != nil {
			return err
		}
	}

	// Update the database if something changed and we're in ClientTypeNormal mode.
	if clientType == request.ClientTypeNormal && (len(changedConfig) > 0 || newDesc != b.db.Description) {
		err = b.state.Cluster.UpdateStoragePool(b.name, newDesc, newConfig)
		if err != nil {
			return err
		}
	}

	return nil

}

// Delete removes the pool.
func (b *lxdBackend) Delete(clientType request.ClientType, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"clientType": clientType})
	logger.Debug("Delete started")
	defer logger.Debug("Delete finished")

	// If completely gone, just return
	path := shared.VarPath("storage-pools", b.name)
	if !shared.PathExists(path) {
		return nil
	}

	if clientType != request.ClientTypeNormal && b.driver.Info().Remote {
		if b.driver.Info().MountedRoot {
			_, err := b.driver.Unmount()
			if err != nil {
				return err
			}
		} else {
			// Remote storage may have leftover entries caused by
			// volumes that were moved or delete while a particular system was offline.
			err := os.RemoveAll(path)
			if err != nil {
				return err
			}
		}
	} else {
		// Delete the low-level storage.
		err := b.driver.Delete(op)
		if err != nil {
			return err
		}
	}

	// Delete the mountpoint.
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "Failed to remove directory %q", path)
	}

	return nil
}

// Mount mounts the storage pool.
func (b *lxdBackend) Mount() (bool, error) {
	logger := logging.AddContext(b.logger, nil)
	logger.Debug("Mount started")
	defer logger.Debug("Mount finished")

	return b.driver.Mount()
}

// Unmount unmounts the storage pool.
func (b *lxdBackend) Unmount() (bool, error) {
	logger := logging.AddContext(b.logger, nil)
	logger.Debug("Unmount started")
	defer logger.Debug("Unmount finished")

	return b.driver.Unmount()
}

// ApplyPatch runs the requested patch at both backend and driver level.
func (b *lxdBackend) ApplyPatch(name string) error {
	// Run early backend patches.
	patch, ok := lxdEarlyPatches[name]
	if ok {
		err := patch(b)
		if err != nil {
			return err
		}
	}

	// Run the driver patch itself.
	err := b.driver.ApplyPatch(name)
	if err != nil {
		return err
	}

	// Run late backend patches.
	patch, ok = lxdLatePatches[name]
	if ok {
		err := patch(b)
		if err != nil {
			return err
		}
	}

	return nil
}

// ensureInstanceSymlink creates a symlink in the instance directory to the instance's mount path
// if doesn't exist already.
func (b *lxdBackend) ensureInstanceSymlink(instanceType instancetype.Type, projectName string, instanceName string, mountPath string) error {
	if shared.IsSnapshot(instanceName) {
		return fmt.Errorf("Instance must not be snapshot")
	}

	symlinkPath := InstancePath(instanceType, projectName, instanceName, false)

	// Remove any old symlinks left over by previous bugs that may point to a different pool.
	if shared.PathExists(symlinkPath) {
		err := os.Remove(symlinkPath)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove symlink %q", symlinkPath)
		}
	}

	// Create new symlink.
	err := os.Symlink(mountPath, symlinkPath)
	if err != nil {
		return errors.Wrapf(err, "Failed to create symlink from %q to %q", mountPath, symlinkPath)
	}

	return nil
}

// removeInstanceSymlink removes a symlink in the instance directory to the instance's mount path.
func (b *lxdBackend) removeInstanceSymlink(instanceType instancetype.Type, projectName string, instanceName string) error {
	symlinkPath := InstancePath(instanceType, projectName, instanceName, false)

	if shared.PathExists(symlinkPath) {
		err := os.Remove(symlinkPath)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove symlink %q", symlinkPath)
		}
	}

	return nil
}

// ensureInstanceSnapshotSymlink creates a symlink in the snapshot directory to the instance's
// snapshot path if doesn't exist already.
func (b *lxdBackend) ensureInstanceSnapshotSymlink(instanceType instancetype.Type, projectName string, instanceName string) error {
	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(instanceType)
	if err != nil {
		return err
	}

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(instanceName)
	snapshotSymlink := InstancePath(instanceType, projectName, parentName, true)
	volStorageName := project.Instance(projectName, parentName)

	snapshotTargetPath := drivers.GetVolumeSnapshotDir(b.name, volType, volStorageName)

	// Remove any old symlinks left over by previous bugs that may point to a different pool.
	if shared.PathExists(snapshotSymlink) {
		err = os.Remove(snapshotSymlink)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove symlink %q", snapshotSymlink)
		}
	}

	// Create new symlink.
	err = os.Symlink(snapshotTargetPath, snapshotSymlink)
	if err != nil {
		return errors.Wrapf(err, "Failed to create symlink from %q to %q", snapshotTargetPath, snapshotSymlink)
	}

	return nil
}

// removeInstanceSnapshotSymlinkIfUnused removes the symlink in the snapshot directory to the
// instance's snapshot path if the snapshot path is missing. It is expected that the driver will
// remove the instance's snapshot path after the last snapshot is removed or the volume is deleted.
func (b *lxdBackend) removeInstanceSnapshotSymlinkIfUnused(instanceType instancetype.Type, projectName string, instanceName string) error {
	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(instanceType)
	if err != nil {
		return err
	}

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(instanceName)
	snapshotSymlink := InstancePath(instanceType, projectName, parentName, true)
	volStorageName := project.Instance(projectName, parentName)

	snapshotTargetPath := drivers.GetVolumeSnapshotDir(b.name, volType, volStorageName)

	// If snapshot parent directory doesn't exist, remove symlink.
	if !shared.PathExists(snapshotTargetPath) {
		if shared.PathExists(snapshotSymlink) {
			err := os.Remove(snapshotSymlink)
			if err != nil {
				return errors.Wrapf(err, "Failed to remove symlink %q", snapshotSymlink)
			}
		}
	}

	return nil
}

// instanceRootVolumeConfig returns the instance's root volume config.
func (b *lxdBackend) instanceRootVolumeConfig(inst instance.Instance) (map[string]string, error) {
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return nil, err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return nil, err
	}

	// Get volume config.
	_, vol, err := b.state.Cluster.GetLocalStoragePoolVolume(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil, errors.Wrapf(err, "Volume doesn't exist for %q on pool %q", project.Instance(inst.Project(), inst.Name()), b.Name())
		}

		return nil, err
	}

	// Get the root disk device config.
	_, rootDiskConf, err := shared.GetRootDiskDevice(inst.ExpandedDevices().CloneNative())
	if err != nil {
		return nil, err
	}

	// Override size property from instance root device config.
	if rootDiskConf["size"] != "" {
		vol.Config["size"] = rootDiskConf["size"]
	}

	if rootDiskConf["size.state"] != "" {
		vol.Config["size.state"] = rootDiskConf["size.state"]
	}

	return vol.Config, nil
}

// FillInstanceConfig populates the supplied instance volume config map with any defaults based on the storage
// pool and instance type being used.
func (b *lxdBackend) FillInstanceConfig(inst instance.Instance, config map[string]string) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("FillInstanceConfig started")
	defer logger.Debug("FillInstanceConfig finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Fill default config in volume (creates internal copy of supplied config and modifies that).
	vol := b.newVolume(volType, contentType, volStorageName, config)
	err = b.driver.FillVolumeConfig(vol)
	if err != nil {
		return err
	}

	// Copy filled volume config back into supplied config map.
	for k, v := range vol.Config() {
		config[k] = v
	}

	return nil
}

// CreateInstance creates an empty instance.
func (b *lxdBackend) CreateInstance(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("CreateInstance started")
	defer logger.Debug("CreateInstance finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	revert := true
	defer func() {
		if !revert {
			return
		}
		b.DeleteInstance(inst, op)
	}()

	contentType := InstanceContentType(inst)

	// Find the root device config for instance.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.CreateVolume(vol, nil, op)
	if err != nil {
		return err
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	err = inst.DeferTemplateApply(instance.TemplateTriggerCreate)
	if err != nil {
		return err
	}

	revert = false
	return nil
}

// CreateInstanceFromBackup restores a backup file onto the storage device. Because the backup file
// is unpacked and restored onto the storage device before the instance is created in the database
// it is necessary to return two functions; a post hook that can be run once the instance has been
// created in the database to run any storage layer finalisations, and a revert hook that can be
// run if the instance database load process fails that will remove anything created thus far.
func (b *lxdBackend) CreateInstanceFromBackup(srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (func(instance.Instance) error, revert.Hook, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": srcBackup.Project, "instance": srcBackup.Name, "snapshots": srcBackup.Snapshots, "optimizedStorage": *srcBackup.OptimizedStorage})
	logger.Debug("CreateInstanceFromBackup started")
	defer logger.Debug("CreateInstanceFromBackup finished")

	// Get the volume name on storage.
	volStorageName := project.Instance(srcBackup.Project, srcBackup.Name)

	// Get the instance type.
	instanceType, err := instancetype.New(string(srcBackup.Type))
	if err != nil {
		return nil, nil, err
	}

	// Get the volume type.
	volType, err := InstanceTypeToVolumeType(instanceType)
	if err != nil {
		return nil, nil, err
	}

	contentType := drivers.ContentTypeFS
	if volType == drivers.VolumeTypeVM {
		contentType = drivers.ContentTypeBlock
	}

	// We don't know the volume's config yet as tarball hasn't been unpacked.
	// We will apply the config as part of the post hook function returned if driver needs to.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	revert := revert.New()
	defer revert.Fail()

	// Unpack the backup into the new storage volume(s).
	volPostHook, revertHook, err := b.driver.CreateVolumeFromBackup(vol, srcBackup, srcData, op)
	if err != nil {
		return nil, nil, err
	}

	if revertHook != nil {
		revert.Add(revertHook)
	}

	err = b.ensureInstanceSymlink(instanceType, srcBackup.Project, srcBackup.Name, vol.MountPath())
	if err != nil {
		return nil, nil, err
	}

	revert.Add(func() {
		b.removeInstanceSymlink(instanceType, srcBackup.Project, srcBackup.Name)
	})

	if len(srcBackup.Snapshots) > 0 {
		err = b.ensureInstanceSnapshotSymlink(instanceType, srcBackup.Project, srcBackup.Name)
		if err != nil {
			return nil, nil, err
		}

		revert.Add(func() {
			b.removeInstanceSnapshotSymlinkIfUnused(instanceType, srcBackup.Project, srcBackup.Name)
		})
	}

	// Update pool information in the backup.yaml file.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		return backup.UpdateInstanceConfigStoragePool(b.state.Cluster, srcBackup, mountPath)
	}, op)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Error updating backup file")
	}

	var postHook func(instance.Instance) error

	// Create a post hook function that will use the instance (that will be created) to setup a new volume
	// containing the instance's root disk device's config so that the driver's post hook function can access
	// that config to perform any post instance creation setup.
	postHook = func(inst instance.Instance) error {
		logger.Debug("CreateInstanceFromBackup post hook started")
		defer logger.Debug("CreateInstanceFromBackup post hook finished")

		// Get the root disk device config.
		rootDiskConf, err := b.instanceRootVolumeConfig(inst)
		if err != nil {
			return err
		}

		// Get the volume name on storage.
		volStorageName := project.Instance(inst.Project(), inst.Name())

		volType, err := InstanceTypeToVolumeType(inst.Type())
		if err != nil {
			return err
		}

		contentType := InstanceContentType(inst)

		// If the driver returned a post hook, run it now.
		if volPostHook != nil {
			// Initialise new volume containing root disk config supplied in instance.
			vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
			err = volPostHook(vol)
			if err != nil {
				return err
			}
		}

		// Apply quota config from root device if its set. Should be done after driver's post hook if set
		// so that any volume initialisation has been completed first.
		if rootDiskConf["size"] != "" {
			logger.Debug("Applying volume quota from root disk config", log.Ctx{"size": rootDiskConf["size"]})

			allowUnsafeResize := false

			if vol.Type() == drivers.VolumeTypeContainer {
				// Enable allowUnsafeResize for container imports so that filesystem resize
				// safety checks are avoided in order to allow more imports to succeed when
				// otherwise the pre-resize estimated checks of resize2fs would prevent
				// import. If there is truly insufficient size to complete the import the
				// resize will still fail, but its OK as we will then delete the volume
				// rather than leaving it in a corrupted state. We don't need to do this
				// for non-container volumes (nor should we) because block volumes won't
				// error if we shrink them too much, and custom volumes can be created at
				// the correct size immediately and don't need a post-import resize step.
				allowUnsafeResize = true
			}

			err = b.driver.SetVolumeQuota(vol, rootDiskConf["size"], allowUnsafeResize, op)
			if err != nil {
				// The restored volume can end up being larger than the root disk config's size
				// property due to the block boundary rounding some storage drivers use. As such
				// if the restored volume is larger than the config's size and it cannot be shrunk
				// to the equivalent size on the target storage driver, don't fail as the backup
				// has still been restored successfully.
				if errors.Cause(err) == drivers.ErrCannotBeShrunk {
					logger.Warn("Could not apply volume quota from root disk config as restored volume cannot be shrunk", log.Ctx{"size": rootDiskConf["size"]})
				} else {
					return errors.Wrapf(err, "Failed applying volume quota to root disk")
				}
			}
		}

		return nil
	}

	revert.Success()
	return postHook, revertHook, nil
}

// CreateInstanceFromCopy copies an instance volume and optionally its snapshots to new volume(s).
func (b *lxdBackend) CreateInstanceFromCopy(inst instance.Instance, src instance.Instance, snapshots bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name(), "snapshots": snapshots})
	logger.Debug("CreateInstanceFromCopy started")
	defer logger.Debug("CreateInstanceFromCopy finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	if src.Type() == instancetype.VM && src.IsRunning() {
		return errors.Wrap(drivers.ErrNotImplemented, "Unable to perform VM live migration")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Initialise a new volume containing the root disk config supplied in the new instance.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	if b.driver.HasVolume(vol) {
		return fmt.Errorf("Cannot create volume, already exists on target")
	}

	// Setup reverter.
	revert := revert.New()
	defer revert.Fail()

	// Get the source storage pool.
	srcPool, err := GetPoolByInstance(b.state, src)
	if err != nil {
		return err
	}

	// Some driver backing stores require that running instances be frozen during copy.
	if !src.IsSnapshot() && b.driver.Info().RunningCopyFreeze && src.IsRunning() && !src.IsFrozen() {
		err = src.Freeze()
		if err != nil {
			return err
		}

		defer src.Unfreeze()
	}

	revert.Add(func() { b.DeleteInstance(inst, op) })

	if b.Name() == srcPool.Name() {
		logger.Debug("CreateInstanceFromCopy same-pool mode detected")

		// Get the src volume name on storage.
		srcVolStorageName := project.Instance(src.Project(), src.Name())

		// We don't need to use the source instance's root disk config, so set to nil.
		srcVol := b.newVolume(volType, contentType, srcVolStorageName, nil)

		err = b.driver.CreateVolumeFromCopy(vol, srcVol, snapshots, op)
		if err != nil {
			return err
		}
	} else {
		// We are copying volumes between storage pools so use migration system as it will
		// be able to negotiate a common transfer method between pool types.
		logger.Debug("CreateInstanceFromCopy cross-pool mode detected")

		// If we are copying snapshots, retrieve a list of snapshots from source volume.
		snapshotNames := []string{}
		if snapshots {
			snapshots, err := VolumeSnapshotsGet(b.state, src.Project(), srcPool.Name(), src.Name(), volDBType)
			if err != nil {
				return err
			}

			for _, snapshot := range snapshots {
				_, snapShotName, _ := shared.InstanceGetParentAndSnapshotName(snapshot.Name)
				snapshotNames = append(snapshotNames, snapShotName)
			}
		}

		// Negotiate the migration type to use.
		offeredTypes := srcPool.MigrationTypes(contentType, false)
		offerHeader := migration.TypesToHeader(offeredTypes...)
		migrationTypes, err := migration.MatchTypes(offerHeader, FallbackMigrationType(contentType), b.MigrationTypes(contentType, false))
		if err != nil {
			return fmt.Errorf("Failed to negotiate copy migration type: %v", err)
		}

		var srcVolumeSize int64

		// For VMs, get source volume size so that target can create the volume the same size.
		if src.Type() == instancetype.VM {
			srcVolumeSize, err = InstanceDiskBlockSize(srcPool, src, op)
			if err != nil {
				return errors.Wrapf(err, "Failed getting source disk size")
			}
		}

		ctx, cancel := context.WithCancel(context.Background())

		// Use in-memory pipe pair to simulate a connection between the sender and receiver.
		aEnd, bEnd := memorypipe.NewPipePair(ctx)

		// Run sender and receiver in separate go routines to prevent deadlocks.
		aEndErrCh := make(chan error, 1)
		bEndErrCh := make(chan error, 1)
		go func() {
			err := srcPool.MigrateInstance(src, aEnd, &migration.VolumeSourceArgs{
				Name:          src.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationTypes[0],
				TrackProgress: true, // Do use a progress tracker on sender.
			}, op)

			if err != nil {
				cancel()
			}
			aEndErrCh <- err
		}()

		go func() {
			err := b.CreateInstanceFromMigration(inst, bEnd, migration.VolumeTargetArgs{
				Name:          inst.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationTypes[0],
				VolumeSize:    srcVolumeSize, // Block size setting override.
				TrackProgress: false,         // Do not use a progress tracker on receiver.
			}, op)

			if err != nil {
				cancel()
			}
			bEndErrCh <- err
		}()

		// Capture errors from the sender and receiver from their result channels.
		errs := []error{}
		aEndErr := <-aEndErrCh
		if aEndErr != nil {
			errs = append(errs, aEndErr)
		}

		bEndErr := <-bEndErrCh
		if bEndErr != nil {
			errs = append(errs, bEndErr)
		}

		cancel()

		if len(errs) > 0 {
			return fmt.Errorf("Create instance volume from copy failed: %v", errs)
		}
	}

	// Setup the symlinks.
	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// RefreshInstance synchronises one instance's volume (and optionally snapshots) over another.
// Snapshots that are not present in the source but are in the destination are removed from the
// destination if snapshots are included in the synchronisation.
func (b *lxdBackend) RefreshInstance(inst instance.Instance, src instance.Instance, srcSnapshots []instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name(), "srcSnapshots": len(srcSnapshots)})
	logger.Debug("RefreshInstance started")
	defer logger.Debug("RefreshInstance finished")

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Initialise a new volume containing the root disk config supplied in the new instance.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	// Get the src volume name on storage.
	srcVolStorageName := project.Instance(src.Project(), src.Name())

	// We don't need to use the source instance's root disk config, so set to nil.
	srcVol := b.newVolume(volType, contentType, srcVolStorageName, nil)

	srcSnapVols := []drivers.Volume{}
	for _, snapInst := range srcSnapshots {
		// Initialise a new volume containing the root disk config supplied in the
		// new instance. We don't need to use the source instance's snapshot root
		// disk config, so set to nil. This is because snapshots are immutable yet
		// the instance and its snapshots can be transferred between pools, so using
		// the data from the snapshot is incorrect.

		// Get the snap volume name on storage.
		snapVolStorageName := project.Instance(snapInst.Project(), snapInst.Name())
		srcSnapVol := b.newVolume(volType, contentType, snapVolStorageName, nil)
		srcSnapVols = append(srcSnapVols, srcSnapVol)
	}

	srcPool, err := GetPoolByInstance(b.state, src)
	if err != nil {
		return err
	}

	if b.Name() == srcPool.Name() {
		logger.Debug("RefreshInstance same-pool mode detected")
		err = b.driver.RefreshVolume(vol, srcVol, srcSnapVols, op)
		if err != nil {
			return err
		}
	} else {
		// We are copying volumes between storage pools so use migration system as it will
		// be able to negotiate a common transfer method between pool types.
		logger.Debug("RefreshInstance cross-pool mode detected")

		// Retrieve a list of snapshots we are copying.
		snapshotNames := []string{}
		for _, srcSnapVol := range srcSnapVols {
			_, snapShotName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapVol.Name())
			snapshotNames = append(snapshotNames, snapShotName)
		}

		// Negotiate the migration type to use.
		offeredTypes := srcPool.MigrationTypes(contentType, true)
		offerHeader := migration.TypesToHeader(offeredTypes...)
		migrationTypes, err := migration.MatchTypes(offerHeader, FallbackMigrationType(contentType), b.MigrationTypes(contentType, true))
		if err != nil {
			return fmt.Errorf("Failed to negotiate copy migration type: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		// Use in-memory pipe pair to simulate a connection between the sender and receiver.
		aEnd, bEnd := memorypipe.NewPipePair(ctx)

		// Run sender and receiver in separate go routines to prevent deadlocks.
		aEndErrCh := make(chan error, 1)
		bEndErrCh := make(chan error, 1)
		go func() {
			err := srcPool.MigrateInstance(src, aEnd, &migration.VolumeSourceArgs{
				Name:          src.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationTypes[0],
				TrackProgress: true, // Do use a progress tracker on sender.
			}, op)

			if err != nil {
				cancel()
			}
			aEndErrCh <- err
		}()

		go func() {
			err := b.CreateInstanceFromMigration(inst, bEnd, migration.VolumeTargetArgs{
				Name:          inst.Name(),
				Snapshots:     snapshotNames,
				MigrationType: migrationTypes[0],
				Refresh:       true,  // Indicate to receiver volume should exist.
				TrackProgress: false, // Do not use a progress tracker on receiver.
			}, op)

			if err != nil {
				cancel()
			}
			bEndErrCh <- err
		}()

		// Capture errors from the sender and receiver from their result channels.
		errs := []error{}
		aEndErr := <-aEndErrCh
		if aEndErr != nil {
			errs = append(errs, aEndErr)
		}

		bEndErr := <-bEndErrCh
		if bEndErr != nil {
			errs = append(errs, bEndErr)
		}

		cancel()

		if len(errs) > 0 {
			return fmt.Errorf("Create instance volume from copy failed: %v", errs)
		}
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	err = inst.DeferTemplateApply(instance.TemplateTriggerCopy)
	if err != nil {
		return err
	}

	return nil
}

// imageFiller returns a function that can be used as a filler function with CreateVolume().
// The function returned will unpack the specified image archive into the specified mount path
// provided, and for VM images, a raw root block path is required to unpack the qcow2 image into.
func (b *lxdBackend) imageFiller(fingerprint string, op *operations.Operation) func(vol drivers.Volume, rootBlockPath string, allowUnsafeResize bool) (int64, error) {
	return func(vol drivers.Volume, rootBlockPath string, allowUnsafeResize bool) (int64, error) {
		var tracker *ioprogress.ProgressTracker
		if op != nil { // Not passed when being done as part of pre-migration setup.
			metadata := make(map[string]interface{})
			tracker = &ioprogress.ProgressTracker{
				Handler: func(percent, speed int64) {
					shared.SetProgressMetadata(metadata, "create_instance_from_image_unpack", "Unpack", percent, 0, speed)
					op.UpdateMetadata(metadata)
				}}
		}
		imageFile := shared.VarPath("images", fingerprint)
		return ImageUnpack(imageFile, vol, rootBlockPath, b.driver.Info().BlockBacking, b.state.OS.RunningInUserNS, allowUnsafeResize, tracker)
	}
}

// CreateInstanceFromImage creates a new volume for an instance populated with the image requested.
// On failure caller is expected to call DeleteInstance() to clean up.
func (b *lxdBackend) CreateInstanceFromImage(inst instance.Instance, fingerprint string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("CreateInstanceFromImage started")
	defer logger.Debug("CreateInstanceFromImage finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	// Leave reverting on failure to caller, they are expected to call DeleteInstance().

	// If the driver doesn't support optimized image volumes then create a new empty volume and
	// populate it with the contents of the image archive.
	if !b.driver.Info().OptimizedImages {
		volFiller := drivers.VolumeFiller{
			Fingerprint: fingerprint,
			Fill:        b.imageFiller(fingerprint, op),
		}

		err = b.driver.CreateVolume(vol, &volFiller, op)
		if err != nil {
			return err
		}
	} else {
		// If the driver supports optimized images then ensure the optimized image volume has been created
		// for the images's fingerprint and that it matches the pool's current volume settings, and if not
		// recreating using the pool's current volume settings.
		err = b.EnsureImage(fingerprint, op)
		if err != nil {
			return err
		}

		// Try and load existing volume config on this storage pool so we can compare filesystems if needed.
		_, imgDBVol, err := b.state.Cluster.GetLocalStoragePoolVolume(project.Default, fingerprint, db.StoragePoolVolumeTypeImage, b.ID())
		if err != nil {
			return errors.Wrapf(err, "Failed loading image record for %q", fingerprint)
		}

		imgVol := b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, imgDBVol.Config)

		// Derive the volume size to use for a new volume when copying from a source volume.
		// Where possible (if the source volume has a volatile.rootfs.size property), it checks that the
		// source volume isn't larger than the volume's "size" and the pool's "volume.size" setting.
		logger.Debug("Checking volume size")
		newVolSize, err := vol.ConfigSizeFromSource(imgVol)
		if err != nil {
			return err
		}

		// Set the derived size directly as the "size" property on the new volume so that it is applied.
		vol.SetConfigSize(newVolSize)
		logger.Debug("Set new volume size", log.Ctx{"size": newVolSize})

		// Proceed to create a new volume by copying the optimized image volume.
		err = b.driver.CreateVolumeFromCopy(vol, imgVol, false, op)

		// If the driver returns ErrCannotBeShrunk, this means that the cached volume that the new volume
		// is to be created from is larger than the requested new volume size, and cannot be shrunk.
		// So we unpack the image directly into a new volume rather than use the optimized snapsot.
		// This is slower but allows for individual volumes to be created from an image that are smaller
		// than the pool's volume settings.
		if errors.Cause(err) == drivers.ErrCannotBeShrunk {
			logger.Debug("Cached image volume is larger than new volume and cannot be shrunk, creating non-optimized volume")

			volFiller := drivers.VolumeFiller{
				Fingerprint: fingerprint,
				Fill:        b.imageFiller(fingerprint, op),
			}

			err = b.driver.CreateVolume(vol, &volFiller, op)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	err = inst.DeferTemplateApply(instance.TemplateTriggerCreate)
	if err != nil {
		return err
	}

	return nil
}

// CreateInstanceFromMigration receives an instance being migrated.
// The args.Name and args.Config fields are ignored and, instance properties are used instead.
func (b *lxdBackend) CreateInstanceFromMigration(inst instance.Instance, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "args": args})
	logger.Debug("CreateInstanceFromMigration started")
	defer logger.Debug("CreateInstanceFromMigration finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	if args.Config != nil {
		return fmt.Errorf("Migration VolumeTargetArgs.Config cannot be set")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Find the root device config for instance.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Override args.Name and args.Config to ensure volume is created based on instance.
	args.Config = rootDiskConf
	args.Name = inst.Name()

	// If migration header supplies a volume size, then use that as block volume size instead of pool default.
	// This way if the volume being received is larger than the pool default size, the block volume created
	// will still be able to accommodate it.
	if args.VolumeSize > 0 && contentType == drivers.ContentTypeBlock {
		b.logger.Debug("Setting volume size from offer header", log.Ctx{"size": args.VolumeSize})
		args.Config["size"] = fmt.Sprintf("%d", args.VolumeSize)
	} else if args.Config["size"] != "" {
		b.logger.Debug("Using volume size from root disk config", log.Ctx{"size": args.Config["size"]})
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), args.Name)

	vol := b.newVolume(volType, contentType, volStorageName, args.Config)

	volExists := b.driver.HasVolume(vol)
	if args.Refresh && !volExists {
		return fmt.Errorf("Cannot refresh volume, doesn't exist on target")
	} else if !args.Refresh && volExists {
		return fmt.Errorf("Cannot create volume, already exists on target")
	}

	var preFiller drivers.VolumeFiller

	revert := true

	if !args.Refresh {
		defer func() {
			if !revert {
				return
			}
			b.DeleteInstance(inst, op)
		}()

		// If the negotiated migration method is rsync and the instance's base image is
		// already on the host then setup a pre-filler that will unpack the local image
		// to try and speed up the rsync of the incoming volume by avoiding the need to
		// transfer the base image files too.
		if args.MigrationType.FSType == migration.MigrationFSType_RSYNC {
			fingerprint := inst.ExpandedConfig()["volatile.base_image"]

			// Confirm that the image is present in the project.
			_, _, err = b.state.Cluster.GetImage(inst.Project(), fingerprint, false)
			if err != db.ErrNoSuchObject && err != nil {
				return err
			}

			// Then make sure that the image is available locally too (not guaranteed in clusters).
			local := shared.PathExists(shared.VarPath("images", fingerprint))

			if err == nil && local {
				logger.Debug("Using optimised migration from existing image", log.Ctx{"fingerprint": fingerprint})

				// Populate the volume filler with the fingerprint and image filler
				// function that can be used by the driver to pre-populate the
				// volume with the contents of the image.
				preFiller = drivers.VolumeFiller{
					Fingerprint: fingerprint,
					Fill:        b.imageFiller(fingerprint, op),
				}

				// Ensure if the image doesn't yet exist on a driver which supports
				// optimized storage, then it gets created first.
				err = b.EnsureImage(preFiller.Fingerprint, op)
				if err != nil {
					return err
				}
			}
		}
	}

	err = b.driver.CreateVolumeFromMigration(vol, conn, args, &preFiller, op)
	if err != nil {
		conn.Close()
		return err
	}

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	if len(args.Snapshots) > 0 {
		err = b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), inst.Name())
		if err != nil {
			return err
		}
	}

	revert = false
	return nil
}

// RenameInstance renames the instance's root volume and any snapshot volumes.
func (b *lxdBackend) RenameInstance(inst instance.Instance, newName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newName": newName})
	logger.Debug("RenameInstance started")
	defer logger.Debug("RenameInstance finished")

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance cannot be a snapshot")
	}

	if shared.IsSnapshot(newName) {
		return fmt.Errorf("New name cannot be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	// Get any snapshots the instance has in the format <instance name>/<snapshot name>.
	snapshots, err := b.state.Cluster.GetInstanceSnapshotsNames(inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		revert.Add(func() {
			b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), newName)
			b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), inst.Name())
		})
	}

	// Rename each snapshot DB record to have the new parent volume prefix.
	for _, srcSnapshot := range snapshots {
		_, snapName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapshot)
		newSnapVolName := drivers.GetSnapshotVolumeName(newName, snapName)
		err = b.state.Cluster.RenameStoragePoolVolume(inst.Project(), srcSnapshot, newSnapVolName, volDBType, b.ID())
		if err != nil {
			return err
		}

		revert.Add(func() {
			b.state.Cluster.RenameStoragePoolVolume(inst.Project(), newSnapVolName, srcSnapshot, volDBType, b.ID())
		})
	}

	// Rename the parent volume DB record.
	err = b.state.Cluster.RenameStoragePoolVolume(inst.Project(), inst.Name(), newName, volDBType, b.ID())
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.state.Cluster.RenameStoragePoolVolume(inst.Project(), newName, inst.Name(), volDBType, b.ID())
	})

	// Rename the volume and its snapshots on the storage device.
	volStorageName := project.Instance(inst.Project(), inst.Name())
	newVolStorageName := project.Instance(inst.Project(), newName)
	contentType := InstanceContentType(inst)

	// There's no need to pass config as it's not needed when renaming a volume.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	err = b.driver.RenameVolume(vol, newVolStorageName, op)
	if err != nil {
		return err
	}

	revert.Add(func() {
		// There's no need to pass config as it's not needed when renaming a volume.
		newVol := b.newVolume(volType, contentType, newVolStorageName, nil)
		b.driver.RenameVolume(newVol, volStorageName, op)
	})

	// Remove old instance symlink and create new one.
	err = b.removeInstanceSymlink(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), drivers.GetVolumeMountPath(b.name, volType, volStorageName))
	})

	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), newName, drivers.GetVolumeMountPath(b.name, volType, newVolStorageName))
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.removeInstanceSymlink(inst.Type(), inst.Project(), newName)
	})

	// Remove old instance snapshot symlink and create a new one if needed.
	err = b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		err = b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), newName)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// DeleteInstance removes the instance's root volume (all snapshots need to be removed first).
func (b *lxdBackend) DeleteInstance(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("DeleteInstance started")
	defer logger.Debug("DeleteInstance finished")

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance must not be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	// Get any snapshots the instance has in the format <instance name>/<snapshot name>.
	snapshots, err := b.state.Cluster.GetInstanceSnapshotsNames(inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	// Check all snapshots are already removed.
	if len(snapshots) > 0 {
		return fmt.Errorf("Cannot remove an instance volume that has snapshots")
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())
	contentType := InstanceContentType(inst)

	// There's no need to pass config as it's not needed when deleting a volume.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	// Delete the volume from the storage device. Must come after snapshots are removed.
	// Must come before DB RemoveStoragePoolVolume so that the volume ID is still available.
	logger.Debug("Deleting instance volume", log.Ctx{"volName": volStorageName})

	if b.driver.HasVolume(vol) {
		err = b.driver.DeleteVolume(vol, op)
		if err != nil {
			return errors.Wrapf(err, "Error deleting storage volume")
		}
	}

	// Remove symlinks.
	err = b.removeInstanceSymlink(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	err = b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	// Remove the volume record from the database.
	err = b.state.Cluster.RemoveStoragePoolVolume(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil && errors.Cause(err) != db.ErrNoSuchObject {
		return errors.Wrapf(err, "Error deleting storage volume from database")
	}

	return nil
}

// UpdateInstance updates an instance volume's config.
func (b *lxdBackend) UpdateInstance(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateInstance started")
	defer logger.Debug("UpdateInstance finished")

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance cannot be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	volStorageName := project.Instance(inst.Project(), inst.Name())
	contentType := InstanceContentType(inst)

	// Validate config.
	newVol := b.newVolume(volType, contentType, volStorageName, newConfig)
	err = b.driver.ValidateVolume(newVol, false)
	if err != nil {
		return err
	}

	// Get current config to compare what has changed.
	_, curVol, err := b.state.Cluster.GetLocalStoragePoolVolume(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return errors.Wrapf(err, "Volume doesn't exist for %q on pool %q", project.Instance(inst.Project(), inst.Name()), b.Name())
		}

		return err
	}

	// Apply config changes if there are any.
	changedConfig, userOnly := b.detectChangedConfig(curVol.Config, newConfig)
	if len(changedConfig) != 0 {
		// Check that the volume's size property isn't being changed.
		if changedConfig["size"] != "" {
			return fmt.Errorf("Instance volume 'size' property cannot be changed")
		}

		// Check that the volume's block.filesystem property isn't being changed.
		if changedConfig["block.filesystem"] != "" {
			return fmt.Errorf("Instance volume 'block.filesystem' property cannot be changed")
		}

		// Get the root disk device config.
		rootDiskConf, err := b.instanceRootVolumeConfig(inst)
		if err != nil {
			return err
		}

		curVol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
		if !userOnly {
			err = b.driver.UpdateVolume(curVol, changedConfig)
			if err != nil {
				return err
			}
		}
	}

	// Update the database if something changed.
	if len(changedConfig) != 0 || newDesc != curVol.Description {
		err = b.state.Cluster.UpdateStoragePoolVolume(inst.Project(), inst.Name(), volDBType, b.ID(), newDesc, newConfig)
		if err != nil {
			return err
		}
	}

	b.state.Events.SendLifecycle(inst.Project(), lifecycle.StorageVolumeUpdated.Event(newVol, string(newVol.Type()), inst.Project(), op, nil))

	return nil
}

// UpdateInstanceSnapshot updates an instance snapshot volume's description.
// Volume config is not allowed to be updated and will return an error.
func (b *lxdBackend) UpdateInstanceSnapshot(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateInstanceSnapshot started")
	defer logger.Debug("UpdateInstanceSnapshot finished")

	if !inst.IsSnapshot() {
		return fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	return b.updateVolumeDescriptionOnly(inst.Project(), inst.Name(), volDBType, newDesc, newConfig, op)
}

// MigrateInstance sends an instance volume for migration.
// The args.Name field is ignored and the name of the instance is used instead.
func (b *lxdBackend) MigrateInstance(inst instance.Instance, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "args": args})
	logger.Debug("MigrateInstance started")
	defer logger.Debug("MigrateInstance finished")

	// rsync+dd can't handle running source instances
	if inst.IsRunning() && args.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		return fmt.Errorf("Rsync based migration doesn't support running virtual machines")
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	if len(args.Snapshots) > 0 && args.FinalSync {
		return fmt.Errorf("Snapshots should not be transferred during final sync")
	}

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	args.Name = inst.Name() // Override args.Name to ensure instance volume is sent.

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), args.Name)

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.MigrateVolume(vol, conn, args, op)
	if err != nil {
		return err
	}

	return nil
}

// BackupInstance creates an instance backup.
func (b *lxdBackend) BackupInstance(inst instance.Instance, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "optimized": optimized, "snapshots": snapshots})
	logger.Debug("BackupInstance started")
	defer logger.Debug("BackupInstance finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Ensure the backup file reflects current config.
	err = b.UpdateInstanceBackupFile(inst, op)
	if err != nil {
		return err
	}

	var snapNames []string
	if snapshots {
		// Get snapshots in age order, oldest first, and pass names to storage driver.
		instSnapshots, err := inst.Snapshots()
		if err != nil {
			return err
		}

		snapNames = make([]string, 0, len(instSnapshots))
		for _, instSnapshot := range instSnapshots {
			_, snapName, _ := shared.InstanceGetParentAndSnapshotName(instSnapshot.Name())
			snapNames = append(snapNames, snapName)
		}
	}

	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.BackupVolume(vol, tarWriter, optimized, snapNames, op)
	if err != nil {
		return err
	}

	return nil
}

// GetInstanceUsage returns the disk usage of the instance's root volume.
func (b *lxdBackend) GetInstanceUsage(inst instance.Instance) (int64, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("GetInstanceUsage started")
	defer logger.Debug("GetInstanceUsage finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return -1, err
	}

	contentType := InstanceContentType(inst)

	// There's no need to pass config as it's not needed when retrieving the volume usage.
	volStorageName := project.Instance(inst.Project(), inst.Name())
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	return b.driver.GetVolumeUsage(vol)
}

// SetInstanceQuota sets the quota on the instance's root volume.
// Returns ErrInUse if the instance is running and the storage driver doesn't support online resizing.
func (b *lxdBackend) SetInstanceQuota(inst instance.Instance, size string, vmStateSize string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "size": size, "vm_state_size": vmStateSize})
	logger.Debug("SetInstanceQuota started")
	defer logger.Debug("SetInstanceQuota finished")

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentVolume := InstanceContentType(inst)
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Apply the main volume quota.
	// There's no need to pass config as it's not needed when setting quotas.
	vol := b.newVolume(volType, contentVolume, volStorageName, nil)
	err = b.driver.SetVolumeQuota(vol, size, false, op)
	if err != nil {
		return err
	}

	// Apply the filesystem volume quota (only when main volume is block).
	if vol.IsVMBlock() {
		if vmStateSize == "" {
			vmStateSize = drivers.DefaultVMBlockFilesystemSize
		}

		fsVol := vol.NewVMBlockFilesystemVolume()
		err := b.driver.SetVolumeQuota(fsVol, vmStateSize, false, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// MountInstance mounts the instance's root volume.
func (b *lxdBackend) MountInstance(inst instance.Instance, op *operations.Operation) (*MountInfo, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("MountInstance started")
	defer logger.Debug("MountInstance finished")

	revert := revert.New()
	defer revert.Fail()

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return nil, err
	}

	// Get the root disk device config.
	var rootDiskConf map[string]string
	if inst.ID() > -1 {
		rootDiskConf, err = b.instanceRootVolumeConfig(inst)
		if err != nil {
			return nil, err
		}
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	err = b.driver.MountVolume(vol, op)
	if err != nil {
		return nil, err
	}
	revert.Add(func() { b.driver.UnmountVolume(vol, false, op) })

	diskPath, err := b.getInstanceDisk(inst)
	if err != nil && err != drivers.ErrNotSupported {
		return nil, errors.Wrapf(err, "Failed getting disk path")
	}

	mountInfo := &MountInfo{
		DiskPath: diskPath,
	}

	revert.Success() // From here on it is up to caller to call UnmountInstance() when done.
	return mountInfo, nil
}

// UnmountInstance unmounts the instance's root volume.
func (b *lxdBackend) UnmountInstance(inst instance.Instance, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("UnmountInstance started")
	defer logger.Debug("UnmountInstance finished")

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return false, err
	}

	// Get the root disk device config.
	var rootDiskConf map[string]string
	if inst.ID() > -1 {
		rootDiskConf, err = b.instanceRootVolumeConfig(inst)
		if err != nil {
			return false, err
		}
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	return b.driver.UnmountVolume(vol, false, op)
}

// getInstanceDisk returns the location of the disk.
func (b *lxdBackend) getInstanceDisk(inst instance.Instance) (string, error) {
	if inst.Type() != instancetype.VM {
		return "", drivers.ErrNotSupported
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return "", err
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Get the volume.
	// There's no need to pass config as it's not needed when getting the
	// location of the disk block device.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	// Get the location of the disk block device.
	diskPath, err := b.driver.GetVolumeDiskPath(vol)
	if err != nil {
		return "", err
	}

	return diskPath, nil
}

// CreateInstanceSnapshot creates a snaphot of an instance volume.
func (b *lxdBackend) CreateInstanceSnapshot(inst instance.Instance, src instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name()})
	logger.Debug("CreateInstanceSnapshot started")
	defer logger.Debug("CreateInstanceSnapshot finished")

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	if !inst.IsSnapshot() {
		return fmt.Errorf("Instance must be a snapshot")
	}

	if src.IsSnapshot() {
		return fmt.Errorf("Source instance cannot be a snapshot")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	// Some driver backing stores require that running instances be frozen during snapshot.
	if b.driver.Info().RunningCopyFreeze && src.IsRunning() && !src.IsFrozen() {
		err = src.Freeze()
		if err != nil {
			return err
		}

		defer src.Unfreeze()
	}

	isSnap := inst.IsSnapshot()

	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Get the volume.
	// There's no need to pass config as it's not needed when creating volume snapshots.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	err = b.driver.CreateVolumeSnapshot(vol, op)
	if err != nil {
		return err
	}

	err = b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	return nil
}

// RenameInstanceSnapshot renames an instance snapshot.
func (b *lxdBackend) RenameInstanceSnapshot(inst instance.Instance, newName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "newName": newName})
	logger.Debug("RenameInstanceSnapshot started")
	defer logger.Debug("RenameInstanceSnapshot finished")

	revert := revert.New()
	defer revert.Fail()

	if !inst.IsSnapshot() {
		return fmt.Errorf("Instance must be a snapshot")
	}

	if shared.IsSnapshot(newName) {
		return fmt.Errorf("New name cannot be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	parentName, oldSnapshotName, isSnap := shared.InstanceGetParentAndSnapshotName(inst.Name())
	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	contentType := InstanceContentType(inst)
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Rename storage volume snapshot. No need to pass config as it's not needed when renaming a volume.
	snapVol := b.newVolume(volType, contentType, volStorageName, nil)
	err = b.driver.RenameVolumeSnapshot(snapVol, newName, op)
	if err != nil {
		return err
	}

	newVolName := drivers.GetSnapshotVolumeName(parentName, newName)

	revert.Add(func() {
		// Revert rename. No need to pass config as it's not needed when renaming a volume.
		newSnapVol := b.newVolume(volType, contentType, project.Instance(inst.Project(), newVolName), nil)
		b.driver.RenameVolumeSnapshot(newSnapVol, oldSnapshotName, op)
	})

	// Rename DB volume record.
	err = b.state.Cluster.RenameStoragePoolVolume(inst.Project(), inst.Name(), newVolName, volDBType, b.ID())
	if err != nil {
		return err
	}

	revert.Add(func() {
		// Rename DB volume record back.
		b.state.Cluster.RenameStoragePoolVolume(inst.Project(), newVolName, inst.Name(), volDBType, b.ID())
	})

	// Ensure the backup file reflects current config.
	err = b.UpdateInstanceBackupFile(inst, op)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// DeleteInstanceSnapshot removes the snapshot volume for the supplied snapshot instance.
func (b *lxdBackend) DeleteInstanceSnapshot(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("DeleteInstanceSnapshot started")
	defer logger.Debug("DeleteInstanceSnapshot finished")

	parentName, snapName, isSnap := shared.InstanceGetParentAndSnapshotName(inst.Name())
	if !inst.IsSnapshot() || !isSnap {
		return fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume types needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Get the parent volume name on storage.
	parentStorageName := project.Instance(inst.Project(), parentName)

	// Delete the snapshot from the storage device.
	// Must come before DB RemoveStoragePoolVolume so that the volume ID is still available.
	logger.Debug("Deleting instance snapshot volume", log.Ctx{"volName": parentStorageName, "snapshotName": snapName})

	snapVolName := drivers.GetSnapshotVolumeName(parentStorageName, snapName)

	// There's no need to pass config as it's not needed when deleting a volume snapshot.
	vol := b.newVolume(volType, contentType, snapVolName, nil)

	if b.driver.HasVolume(vol) {
		err = b.driver.DeleteVolumeSnapshot(vol, op)
		if err != nil {
			return err
		}
	}

	// Delete symlink if needed.
	err = b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	// Remove the snapshot volume record from the database if exists.
	err = b.state.Cluster.RemoveStoragePoolVolume(inst.Project(), drivers.GetSnapshotVolumeName(parentName, snapName), volDBType, b.ID())
	if err != nil && err != db.ErrNoSuchObject {
		return err
	}

	return nil
}

// RestoreInstanceSnapshot restores an instance snapshot.
func (b *lxdBackend) RestoreInstanceSnapshot(inst instance.Instance, src instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name(), "src": src.Name()})
	logger.Debug("RestoreInstanceSnapshot started")
	defer logger.Debug("RestoreInstanceSnapshot finished")

	if inst.Type() != src.Type() {
		return fmt.Errorf("Instance types must match")
	}

	if inst.IsSnapshot() {
		return fmt.Errorf("Instance must not be snapshot")
	}

	if !src.IsSnapshot() {
		return fmt.Errorf("Source instance must be a snapshot")
	}

	// Target instance must not be running.
	if inst.IsRunning() {
		return fmt.Errorf("Instance must not be running to restore")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	// Find the root device config for source snapshot instance.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	_, snapshotName, isSnap := shared.InstanceGetParentAndSnapshotName(src.Name())
	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	// Use the source snapshot's rootfs config (as this will later be restored into inst too).
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)
	err = b.driver.RestoreVolume(vol, snapshotName, op)
	if err != nil {
		snapErr, ok := err.(drivers.ErrDeleteSnapshots)
		if ok {
			// We need to delete some snapshots and try again.
			snaps, err := inst.Snapshots()
			if err != nil {
				return err
			}

			// Go through all the snapshots.
			for _, snap := range snaps {
				_, snapName, _ := shared.InstanceGetParentAndSnapshotName(snap.Name())
				if !shared.StringInSlice(snapName, snapErr.Snapshots) {
					continue
				}

				// Delete snapshot instance if listed in the error as one that needs removing.
				err := snap.Delete(true)
				if err != nil {
					return err
				}
			}

			// Now try restoring again.
			err = b.driver.RestoreVolume(vol, snapshotName, op)
			if err != nil {
				return err
			}

			return nil
		}

		return err
	}

	return nil
}

// MountInstanceSnapshot mounts an instance snapshot. It is mounted as read only so that the
// snapshot cannot be modified.
func (b *lxdBackend) MountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (*MountInfo, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("MountInstanceSnapshot started")
	defer logger.Debug("MountInstanceSnapshot finished")

	if !inst.IsSnapshot() {
		return nil, fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return nil, err
	}

	contentType := InstanceContentType(inst)

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return nil, err
	}

	// Get the parent and snapshot name.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	_, err = b.driver.MountVolumeSnapshot(vol, op)
	if err != nil {
		return nil, err
	}

	diskPath, err := b.getInstanceDisk(inst)
	if err != nil && err != drivers.ErrNotSupported {
		return nil, errors.Wrapf(err, "Failed getting disk path")
	}

	mountInfo := &MountInfo{
		DiskPath: diskPath,
	}

	return mountInfo, nil
}

// UnmountInstanceSnapshot unmounts an instance snapshot.
func (b *lxdBackend) UnmountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("UnmountInstanceSnapshot started")
	defer logger.Debug("UnmountInstanceSnapshot finished")

	if !inst.IsSnapshot() {
		return false, fmt.Errorf("Instance must be a snapshot")
	}

	// Check we can convert the instance to the volume type needed.
	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return false, err
	}

	// Get the root disk device config.
	rootDiskConf, err := b.instanceRootVolumeConfig(inst)
	if err != nil {
		return false, err
	}

	contentType := InstanceContentType(inst)

	// Get the parent and snapshot name.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	// Get the volume.
	vol := b.newVolume(volType, contentType, volStorageName, rootDiskConf)

	return b.driver.UnmountVolumeSnapshot(vol, op)
}

// poolBlockFilesystem returns the filesystem used for new block device filesystems.
func (b *lxdBackend) poolBlockFilesystem() string {
	if b.db.Config["volume.block.filesystem"] != "" {
		return b.db.Config["volume.block.filesystem"]
	}

	return drivers.DefaultFilesystem
}

// EnsureImage creates an optimized volume of the image if supported by the storage pool driver and the volume
// doesn't already exist. If the volume already exists then it is checked to ensure it matches the pools current
// volume settings ("volume.size" and "block.filesystem" if applicable). If not the optimized volume is removed
// and regenerated to apply the pool's current volume settings.
func (b *lxdBackend) EnsureImage(fingerprint string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"fingerprint": fingerprint})
	logger.Debug("EnsureImage started")
	defer logger.Debug("EnsureImage finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	if !b.driver.Info().OptimizedImages {
		return nil // Nothing to do for drivers that don't support optimized images volumes.
	}

	// We need to lock this operation to ensure that the image is not being created multiple times.
	// Uses a lock name of "EnsureImage_<fingerprint>" to avoid deadlocking with CreateVolume below that also
	// establishes a lock on the volume type & name if it needs to mount the volume before filling.
	unlock := locking.Lock(drivers.OperationLockName("EnsureImage", b.name, drivers.VolumeTypeImage, "", fingerprint))
	defer unlock()

	// Load image info from database.
	_, image, err := b.state.Cluster.GetImageFromAnyProject(fingerprint)
	if err != nil {
		return err
	}

	// Derive content type from image type. Image types are not the same as instance types, so don't use
	// instance type constants for comparison.
	contentType := drivers.ContentTypeFS

	if image.Type == "virtual-machine" {
		contentType = drivers.ContentTypeBlock
	}

	// Try and load any existing volume config on this storage pool so we can compare filesystems if needed.
	_, imgDBVol, err := b.state.Cluster.GetLocalStoragePoolVolume(project.Default, fingerprint, db.StoragePoolVolumeTypeImage, b.ID())
	if err != nil {
		if err != db.ErrNoSuchObject {
			return err
		}
	}

	// Create the new image volume. No config for an image volume so set to nil.
	// Pool config values will be read by the underlying driver if needed.
	imgVol := b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, nil)

	// If an existing DB row was found, check if filesystem is the same as the current pool's filesystem.
	// If not we need to delete the existing cached image volume and re-create using new filesystem.
	// We need to do this for VM block images too, as they create a filesystem based config volume too.
	if imgDBVol != nil {
		// Add existing image volume's config to imgVol.
		imgVol = b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, imgDBVol.Config)

		if b.Driver().Info().BlockBacking && imgVol.Config()["block.filesystem"] != b.poolBlockFilesystem() {
			logger.Debug("Filesystem of pool has changed since cached image volume created, regenerating image volume")
			err = b.DeleteImage(fingerprint, op)
			if err != nil {
				return err
			}

			// Reset img volume variables as we just deleted the old one.
			imgDBVol = nil
			imgVol = b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, nil)
		}
	}

	// Check if we already have a suitable volume on storage device.
	if b.driver.HasVolume(imgVol) {
		if imgDBVol != nil {
			// Work out what size the image volume should be as if we were creating from scratch.
			// This takes into account the existing volume's "volatile.rootfs.size" setting if set so
			// as to avoid trying to shrink a larger image volume back to the default size when it is
			// allowed to be larger than the default as the pool doesn't specify a volume.size.
			logger.Debug("Checking image volume size")
			newVolSize, err := imgVol.ConfigSizeFromSource(imgVol)
			if err != nil {
				return err
			}

			imgVol.SetConfigSize(newVolSize)

			// Try applying the current size policy to the existing volume. If it is the same the
			// driver should make no changes, and if not then attempt to resize it to the new policy.
			logger.Debug("Setting image volume size", "size", imgVol.ConfigSize())
			err = b.driver.SetVolumeQuota(imgVol, imgVol.ConfigSize(), false, op)
			if errors.Cause(err) == drivers.ErrCannotBeShrunk || errors.Cause(err) == drivers.ErrNotSupported {
				// If the driver cannot resize the existing image volume to the new policy size
				// then delete the image volume and try to recreate using the new policy settings.
				logger.Debug("Volume size of pool has changed since cached image volume created and cached volume cannot be resized, regenerating image volume")
				err = b.DeleteImage(fingerprint, op)
				if err != nil {
					return err
				}

				// Reset img volume variables as we just deleted the old one.
				imgDBVol = nil
				imgVol = b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, nil)
			} else if err != nil {
				return err
			} else {
				// We already have a valid volume at the correct size, just return.
				return nil
			}
		} else {
			// We somehow have an unrecorded on-disk volume, assume it's a partial unpack and delete it.
			logger.Warn("Deleting leftover/partially unpacked image volume")
			err = b.driver.DeleteVolume(imgVol, op)
			if err != nil {
				return errors.Wrapf(err, "Failed deleting leftover/partially unpacked image volume")
			}
		}
	}

	volFiller := drivers.VolumeFiller{
		Fingerprint: fingerprint,
		Fill:        b.imageFiller(fingerprint, op),
	}

	revert := revert.New()
	defer revert.Fail()

	err = b.driver.CreateVolume(imgVol, &volFiller, op)
	if err != nil {
		return err
	}
	revert.Add(func() { b.driver.DeleteVolume(imgVol, op) })

	var volConfig map[string]string

	// If the volume filler has recorded the size of the unpacked volume, then store this in the image DB row.
	if volFiller.Size != 0 {
		volConfig = map[string]string{
			"volatile.rootfs.size": fmt.Sprintf("%d", volFiller.Size),
		}
	}

	err = VolumeDBCreate(b.state, b, project.Default, fingerprint, "", drivers.VolumeTypeImage, false, volConfig, time.Time{}, contentType)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// DeleteImage removes an image from the database and underlying storage device if needed.
func (b *lxdBackend) DeleteImage(fingerprint string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"fingerprint": fingerprint})
	logger.Debug("DeleteImage started")
	defer logger.Debug("DeleteImage finished")

	// We need to lock this operation to ensure that the image is not being deleted multiple times.
	unlock := locking.Lock(drivers.OperationLockName("DeleteImage", b.name, drivers.VolumeTypeImage, "", fingerprint))
	defer unlock()

	// Load image info from database.
	_, image, err := b.state.Cluster.GetImageFromAnyProject(fingerprint)
	if err != nil {
		return err
	}

	contentType := drivers.ContentTypeFS

	// Image types are not the same as instance types, so don't use instance type constants.
	if image.Type == "virtual-machine" {
		contentType = drivers.ContentTypeBlock
	}

	// Load the storage volume in order to get the volume config which is needed for some drivers.
	_, storageVol, err := b.state.Cluster.GetLocalStoragePoolVolume(project.Default, fingerprint, db.StoragePoolVolumeTypeImage, b.ID())
	if err != nil {
		return err
	}

	vol := b.newVolume(drivers.VolumeTypeImage, contentType, fingerprint, storageVol.Config)

	if b.driver.HasVolume(vol) {
		err = b.driver.DeleteVolume(vol, op)
		if err != nil {
			return err
		}
	}

	err = b.state.Cluster.RemoveStoragePoolVolume(project.Default, fingerprint, db.StoragePoolVolumeTypeImage, b.ID())
	if err != nil {
		return err
	}

	b.state.Events.SendLifecycle(project.Default, lifecycle.StorageVolumeDeleted.Event(vol, string(vol.Type()), project.Default, op, nil))

	return nil
}

// updateVolumeDescriptionOnly is a helper function used when handling update requests for volumes
// that only allow their descriptions to be updated. If any config supplied differs from the
// current volume's config then an error is returned.
func (b *lxdBackend) updateVolumeDescriptionOnly(project string, volName string, dbVolType int, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	// Get current config to compare what has changed.
	_, curVol, err := b.state.Cluster.GetLocalStoragePoolVolume(project, volName, dbVolType, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return errors.Wrapf(err, "Volume doesn't exist")
		}

		return err
	}

	if newConfig != nil {
		changedConfig, _ := b.detectChangedConfig(curVol.Config, newConfig)
		if len(changedConfig) != 0 {
			return fmt.Errorf("Volume config is not editable")
		}
	}

	// Update the database if description changed. Use current config.
	if newDesc != curVol.Description {
		err = b.state.Cluster.UpdateStoragePoolVolume(project, volName, dbVolType, b.ID(), newDesc, curVol.Config)
		if err != nil {
			return err
		}
	}

	// Get content type.
	dbContentType, err := VolumeContentTypeNameToContentType(curVol.ContentType)
	if err != nil {
		return err
	}

	contentType, err := VolumeDBContentTypeToContentType(dbContentType)
	if err != nil {
		return err
	}

	// Validate config.
	vol := b.newVolume(drivers.VolumeType(curVol.Type), contentType, volName, newConfig)

	if !vol.IsSnapshot() {
		b.state.Events.SendLifecycle(project, lifecycle.StorageVolumeUpdated.Event(vol, string(vol.Type()), project, op, nil))
	} else {
		b.state.Events.SendLifecycle(project, lifecycle.StorageVolumeSnapshotUpdated.Event(vol, string(vol.Type()), project, op, nil))
	}

	return nil
}

// UpdateImage updates image config.
func (b *lxdBackend) UpdateImage(fingerprint, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"fingerprint": fingerprint, "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateImage started")
	defer logger.Debug("UpdateImage finished")

	return b.updateVolumeDescriptionOnly(project.Default, fingerprint, db.StoragePoolVolumeTypeImage, newDesc, newConfig, op)
}

// CreateCustomVolume creates an empty custom volume.
func (b *lxdBackend) CreateCustomVolume(projectName string, volName string, desc string, config map[string]string, contentType drivers.ContentType, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName, "desc": desc, "config": config, "contentType": contentType})
	logger.Debug("CreateCustomVolume started")
	defer logger.Debug("CreateCustomVolume finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	// Validate config.
	vol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, config)
	err := b.driver.ValidateVolume(vol, false)
	if err != nil {
		return err
	}

	storagePoolSupported := false
	for _, supportedType := range b.Driver().Info().VolumeTypes {
		if supportedType == drivers.VolumeTypeCustom {
			storagePoolSupported = true
			break
		}
	}

	if !storagePoolSupported {
		return fmt.Errorf("Storage pool does not support custom volume type")
	}

	// Create database entry for new storage volume.
	err = VolumeDBCreate(b.state, b, projectName, volName, desc, vol.Type(), false, vol.Config(), time.Time{}, vol.ContentType())
	if err != nil {
		return err
	}

	revertDB := true
	defer func() {
		if revertDB {
			b.state.Cluster.RemoveStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
		}
	}()

	// Create the empty custom volume on the storage device.
	err = b.driver.CreateVolume(vol, nil, op)
	if err != nil {
		return err
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeCreated.Event(vol, string(vol.Type()), projectName, op, log.Ctx{"type": vol.Type()}))

	revertDB = false
	return nil
}

// CreateCustomVolumeFromCopy creates a custom volume from an existing custom volume.
// It copies the snapshots from the source volume by default, but can be disabled if requested.
func (b *lxdBackend) CreateCustomVolumeFromCopy(projectName string, srcProjectName string, volName string, desc string, config map[string]string, srcPoolName, srcVolName string, srcVolOnly bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "srcProjectName": srcProjectName, "volName": volName, "desc": desc, "config": config, "srcPoolName": srcPoolName, "srcVolName": srcVolName, "srcVolOnly": srcVolOnly})
	logger.Debug("CreateCustomVolumeFromCopy started")
	defer logger.Debug("CreateCustomVolumeFromCopy finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	if srcProjectName == "" {
		srcProjectName = projectName
	}

	// Setup the source pool backend instance.
	var srcPool *lxdBackend
	if b.name == srcPoolName {
		srcPool = b // Source and target are in the same pool so share pool var.
	} else {
		// Source is in a different pool to target, so load the pool.
		tmpPool, err := GetPoolByName(b.state, srcPoolName)
		if err != nil {
			return err
		}

		// Convert to lxdBackend so we can access driver.
		tmpBackend, ok := tmpPool.(*lxdBackend)
		if !ok {
			return fmt.Errorf("Pool is not an lxdBackend")
		}

		srcPool = tmpBackend
	}

	// Check source volume exists and is custom type.
	_, srcVolRow, err := b.state.Cluster.GetLocalStoragePoolVolume(srcProjectName, srcVolName, db.StoragePoolVolumeTypeCustom, srcPool.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Source volume doesn't exist")
		}

		return err
	}

	// Use the source volume's config if not supplied.
	if config == nil {
		config = srcVolRow.Config
	}

	// Use the source volume's description if not supplied.
	if desc == "" {
		desc = srcVolRow.Description
	}

	contentDBType, err := VolumeContentTypeNameToContentType(srcVolRow.ContentType)
	if err != nil {
		return err
	}

	// Get the source volume's content type.
	contentType := drivers.ContentTypeFS

	if contentDBType == db.StoragePoolVolumeContentTypeBlock {
		contentType = drivers.ContentTypeBlock
	}

	storagePoolSupported := false
	for _, supportedType := range b.Driver().Info().VolumeTypes {
		if supportedType == drivers.VolumeTypeCustom {
			storagePoolSupported = true
			break
		}
	}

	if !storagePoolSupported {
		return fmt.Errorf("Storage pool does not support custom volume type")
	}

	// If we are copying snapshots, retrieve a list of snapshots from source volume.
	snapshotNames := []string{}
	if !srcVolOnly {
		snapshots, err := VolumeSnapshotsGet(b.state, srcProjectName, srcPoolName, srcVolName, db.StoragePoolVolumeTypeCustom)
		if err != nil {
			return err
		}

		for _, snapshot := range snapshots {
			_, snapShotName, _ := shared.InstanceGetParentAndSnapshotName(snapshot.Name)
			snapshotNames = append(snapshotNames, snapShotName)
		}
	}

	// If the source and target are in the same pool then use CreateVolumeFromCopy rather than
	// migration system as it will be quicker.
	if srcPool == b {
		logger.Debug("CreateCustomVolumeFromCopy same-pool mode detected")

		// Create slice to record DB volumes created if revert needed later.
		revertDBVolumes := []string{}
		defer func() {
			// Remove any DB volume rows created if we are reverting.
			for _, volName := range revertDBVolumes {
				b.state.Cluster.RemoveStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
			}
		}()

		// Get the volume name on storage.
		volStorageName := project.StorageVolume(projectName, volName)
		vol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, config)

		// Get the src volume name on storage.
		srcVolStorageName := project.StorageVolume(srcProjectName, srcVolName)
		srcVol := b.newVolume(drivers.VolumeTypeCustom, contentType, srcVolStorageName, srcVolRow.Config)

		// Check the supplied config and remove any fields not relevant for pool type.
		err := b.driver.ValidateVolume(vol, true)
		if err != nil {
			return err
		}

		// Create database entry for new storage volume.
		err = VolumeDBCreate(b.state, b, projectName, volName, desc, vol.Type(), false, vol.Config(), time.Time{}, vol.ContentType())
		if err != nil {
			return err
		}

		revertDBVolumes = append(revertDBVolumes, volName)

		if len(snapshotNames) > 0 {
			for _, snapName := range snapshotNames {
				newSnapshotName := drivers.GetSnapshotVolumeName(volName, snapName)

				// Create database entry for new storage volume snapshot.
				err = VolumeDBCreate(b.state, b, projectName, newSnapshotName, desc, vol.Type(), true, vol.Config(), time.Time{}, vol.ContentType())
				if err != nil {
					return err
				}

				revertDBVolumes = append(revertDBVolumes, newSnapshotName)
			}
		}

		err = b.driver.CreateVolumeFromCopy(vol, srcVol, !srcVolOnly, op)
		if err != nil {
			return err
		}

		b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeCreated.Event(vol, string(vol.Type()), projectName, op, log.Ctx{"type": vol.Type()}))

		revertDBVolumes = nil
		return nil
	}

	// We are copying volumes between storage pools so use migration system as it will be able
	// to negotiate a common transfer method between pool types.
	logger.Debug("CreateCustomVolumeFromCopy cross-pool mode detected")

	// Negotiate the migration type to use.
	offeredTypes := srcPool.MigrationTypes(contentType, false)
	offerHeader := migration.TypesToHeader(offeredTypes...)
	migrationTypes, err := migration.MatchTypes(offerHeader, FallbackMigrationType(contentType), b.MigrationTypes(contentType, false))
	if err != nil {
		return fmt.Errorf("Failed to negotiate copy migration type: %v", err)
	}

	// If we're copying block volumes, the target block volume needs to be
	// at least the size of the source volume, otherwise we'll run into
	// "no space left on device".
	var volSize int64

	if contentType == drivers.ContentTypeBlock {
		// Get the src volume name on storage.
		srcVolStorageName := project.StorageVolume(srcProjectName, srcVolName)
		srcVol := srcPool.newVolume(drivers.VolumeTypeCustom, contentType, srcVolStorageName, srcVolRow.Config)

		srcVol.MountTask(func(mountPath string, op *operations.Operation) error {
			volDiskPath, err := srcPool.driver.GetVolumeDiskPath(srcVol)
			if err != nil {
				return err
			}

			volSize, err = drivers.BlockDiskSizeBytes(volDiskPath)
			if err != nil {
				return err
			}

			return nil
		}, nil)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Use in-memory pipe pair to simulate a connection between the sender and receiver.
	aEnd, bEnd := memorypipe.NewPipePair(ctx)

	// Run sender and receiver in separate go routines to prevent deadlocks.
	aEndErrCh := make(chan error, 1)
	bEndErrCh := make(chan error, 1)
	go func() {
		err := srcPool.MigrateCustomVolume(srcProjectName, aEnd, &migration.VolumeSourceArgs{
			Name:          srcVolName,
			Snapshots:     snapshotNames,
			MigrationType: migrationTypes[0],
			TrackProgress: true, // Do use a progress tracker on sender.
			ContentType:   string(contentType),
		}, op)

		if err != nil {
			cancel()
		}
		aEndErrCh <- err
	}()

	go func() {
		err := b.CreateCustomVolumeFromMigration(projectName, bEnd, migration.VolumeTargetArgs{
			Name:          volName,
			Description:   desc,
			Config:        config,
			Snapshots:     snapshotNames,
			MigrationType: migrationTypes[0],
			TrackProgress: false, // Do not use a progress tracker on receiver.
			ContentType:   string(contentType),
			VolumeSize:    volSize, // Block size setting override.
		}, op)

		if err != nil {
			cancel()
		}
		bEndErrCh <- err
	}()

	// Capture errors from the sender and receiver from their result channels.
	errs := []error{}
	aEndErr := <-aEndErrCh
	if aEndErr != nil {
		aEnd.Close()
		errs = append(errs, aEndErr)
	}

	bEndErr := <-bEndErrCh
	if bEndErr != nil {
		errs = append(errs, bEndErr)
	}

	cancel()

	if len(errs) > 0 {
		return fmt.Errorf("Create custom volume from copy failed: %v", errs)
	}

	return nil
}

// MigrateCustomVolume sends a volume for migration.
func (b *lxdBackend) MigrateCustomVolume(projectName string, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": args.Name, "args": args})
	logger.Debug("MigrateCustomVolume started")
	defer logger.Debug("MigrateCustomVolume finished")

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, args.Name)

	dbContentType, err := VolumeContentTypeNameToContentType(args.ContentType)
	if err != nil {
		return err
	}

	contentType, err := VolumeDBContentTypeToContentType(dbContentType)
	if err != nil {
		return err
	}

	// Volume config not needed to send a volume so set to nil.
	vol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, nil)

	err = b.driver.MigrateVolume(vol, conn, args, op)
	if err != nil {
		return err
	}

	return nil
}

// CreateCustomVolumeFromMigration receives a volume being migrated.
func (b *lxdBackend) CreateCustomVolumeFromMigration(projectName string, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": args.Name, "args": args})
	logger.Debug("CreateCustomVolumeFromMigration started")
	defer logger.Debug("CreateCustomVolumeFromMigration finished")

	if b.Status() == api.StoragePoolStatusPending {
		return fmt.Errorf("Specified pool is not fully created")
	}

	storagePoolSupported := false
	for _, supportedType := range b.Driver().Info().VolumeTypes {
		if supportedType == drivers.VolumeTypeCustom {
			storagePoolSupported = true
			break
		}
	}

	if !storagePoolSupported {
		return fmt.Errorf("Storage pool does not support custom volume type")
	}

	// Create slice to record DB volumes created if revert needed later.
	revertDBVolumes := []string{}
	defer func() {
		// Remove any DB volume rows created if we are reverting.
		for _, volName := range revertDBVolumes {
			b.state.Cluster.RemoveStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
		}
	}()

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, args.Name)

	// Check the supplied config and remove any fields not relevant for destination pool type.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(args.ContentType), volStorageName, args.Config)

	// VolumeSize is set to the actual size of the underlying block device.
	// The target should use this value if present, otherwise it might get an error like
	// "no space left on device".
	if args.VolumeSize > 0 {
		vol.SetConfigSize(fmt.Sprintf("%d", args.VolumeSize))
	}

	err := b.driver.ValidateVolume(vol, true)
	if err != nil {
		return err
	}

	// Create database entry for new storage volume.
	err = VolumeDBCreate(b.state, b, projectName, args.Name, args.Description, vol.Type(), false, vol.Config(), time.Time{}, vol.ContentType())
	if err != nil {
		return err
	}

	revertDBVolumes = append(revertDBVolumes, args.Name)

	if len(args.Snapshots) > 0 {
		for _, snapName := range args.Snapshots {
			newSnapshotName := drivers.GetSnapshotVolumeName(args.Name, snapName)

			// Create database entry for new storage volume snapshot.
			err = VolumeDBCreate(b.state, b, projectName, newSnapshotName, args.Description, vol.Type(), true, vol.Config(), time.Time{}, vol.ContentType())
			if err != nil {
				return err
			}

			revertDBVolumes = append(revertDBVolumes, newSnapshotName)
		}
	}

	err = b.driver.CreateVolumeFromMigration(vol, conn, args, nil, op)
	if err != nil {
		conn.Close()
		return err
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeCreated.Event(vol, string(vol.Type()), projectName, op, log.Ctx{"type": vol.Type()}))

	revertDBVolumes = nil
	return nil
}

// RenameCustomVolume renames a custom volume and its snapshots.
func (b *lxdBackend) RenameCustomVolume(projectName string, volName string, newVolName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName, "newVolName": newVolName})
	logger.Debug("RenameCustomVolume started")
	defer logger.Debug("RenameCustomVolume finished")

	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume name cannot be a snapshot")
	}

	if shared.IsSnapshot(newVolName) {
		return fmt.Errorf("New volume name cannot be a snapshot")
	}

	revert := revert.New()
	defer revert.Fail()

	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return err
	}

	// Rename each snapshot to have the new parent volume prefix.
	snapshots, err := VolumeSnapshotsGet(b.state, projectName, b.name, volName, db.StoragePoolVolumeTypeCustom)
	if err != nil {
		return err
	}

	for _, srcSnapshot := range snapshots {
		_, snapName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapshot.Name)
		newSnapVolName := drivers.GetSnapshotVolumeName(newVolName, snapName)
		err = b.state.Cluster.RenameStoragePoolVolume(projectName, srcSnapshot.Name, newSnapVolName, db.StoragePoolVolumeTypeCustom, b.ID())
		if err != nil {
			return err
		}

		revert.Add(func() {
			b.state.Cluster.RenameStoragePoolVolume(projectName, newSnapVolName, srcSnapshot.Name, db.StoragePoolVolumeTypeCustom, b.ID())
		})
	}

	// Rename each backup to have the new parent volume prefix.
	backups, err := b.state.Cluster.GetStoragePoolVolumeBackups(projectName, volName, b.ID())
	if err != nil {
		return err
	}

	for _, br := range backups {
		backupRow := br // Local var for revert.
		_, backupName, _ := shared.InstanceGetParentAndSnapshotName(backupRow.Name)
		newVolBackupName := drivers.GetSnapshotVolumeName(newVolName, backupName)
		volBackup := backup.NewVolumeBackup(b.state, projectName, b.name, volName, backupRow.ID, backupRow.Name, backupRow.CreationDate, backupRow.ExpiryDate, backupRow.VolumeOnly, backupRow.OptimizedStorage)
		err = volBackup.Rename(newVolBackupName)
		if err != nil {
			return errors.Wrapf(err, "Failed renaming backup %q to %q", backupRow.Name, newVolBackupName)
		}

		revert.Add(func() {
			volBackup.Rename(backupRow.Name)
		})
	}

	err = b.state.Cluster.RenameStoragePoolVolume(projectName, volName, newVolName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.state.Cluster.RenameStoragePoolVolume(projectName, newVolName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	})

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)
	newVolStorageName := project.StorageVolume(projectName, newVolName)

	// There's no need to pass the config as it's not needed when renaming a volume.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), volStorageName, nil)

	err = b.driver.RenameVolume(vol, newVolStorageName, op)
	if err != nil {
		return err
	}

	vol = b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), newVolStorageName, nil)
	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeRenamed.Event(vol, string(vol.Type()), projectName, op, log.Ctx{"old_name": volName}))

	revert.Success()
	return nil
}

// detectChangedConfig returns the config that has changed between current and new config maps.
// Also returns a boolean indicating whether all of the changed keys start with "user.".
// Deleted keys will be returned as having an empty string value.
func (b *lxdBackend) detectChangedConfig(curConfig, newConfig map[string]string) (map[string]string, bool) {
	// Diff the configurations.
	changedConfig := make(map[string]string)
	userOnly := true
	for key := range curConfig {
		if curConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			changedConfig[key] = newConfig[key] // Will be empty string on deleted keys.
		}
	}

	for key := range newConfig {
		if curConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			changedConfig[key] = newConfig[key]
		}
	}

	return changedConfig, userOnly
}

// UpdateCustomVolume applies the supplied config to the custom volume.
func (b *lxdBackend) UpdateCustomVolume(projectName string, volName string, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName, "newDesc": newDesc, "newConfig": newConfig})
	logger.Debug("UpdateCustomVolume started")
	defer logger.Debug("UpdateCustomVolume finished")

	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume name cannot be a snapshot")
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	// Get current config to compare what has changed.
	_, curVol, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return errors.Wrapf(err, "Volume doesn't exist")
		}

		return err
	}

	// Get content type.
	dbContentType, err := VolumeContentTypeNameToContentType(curVol.ContentType)
	if err != nil {
		return err
	}

	contentType, err := VolumeDBContentTypeToContentType(dbContentType)
	if err != nil {
		return err
	}

	// Validate config.
	newVol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, newConfig)
	err = b.driver.ValidateVolume(newVol, false)
	if err != nil {
		return err
	}

	// Apply config changes if there are any.
	changedConfig, userOnly := b.detectChangedConfig(curVol.Config, newConfig)
	if len(changedConfig) != 0 {
		// Check that the volume's block.filesystem property isn't being changed.
		if changedConfig["block.filesystem"] != "" {
			return fmt.Errorf("Custom volume 'block.filesystem' property cannot be changed")
		}

		// Check that security.unmapped and security.shifted aren't set together.
		if shared.IsTrue(newConfig["security.unmapped"]) && shared.IsTrue(newConfig["security.shifted"]) {
			return fmt.Errorf("security.unmapped and security.shifted are mutually exclusive")
		}

		// Check for config changing that is not allowed when running instances are using it.
		if changedConfig["security.shifted"] != "" {
			err = VolumeUsedByInstanceDevices(b.state, b.name, projectName, curVol, true, func(dbInst db.Instance, project api.Project, profiles []api.Profile, usedByDevices []string) error {
				inst, err := instance.Load(b.state, db.InstanceToArgs(&dbInst), profiles)
				if err != nil {
					return err
				}

				// Confirm that no running instances are using it when changing shifted state.
				if inst.IsRunning() && changedConfig["security.shifted"] != "" {
					return fmt.Errorf("Cannot modify shifting with running instances using the volume")
				}

				return nil
			})
			if err != nil {
				return err
			}
		}

		curVol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, curVol.Config)
		if !userOnly {
			err = b.driver.UpdateVolume(curVol, changedConfig)
			if err != nil {
				return err
			}
		}
	}

	// Unset idmap keys if volume is unmapped.
	if shared.IsTrue(newConfig["security.unmapped"]) {
		delete(newConfig, "volatile.idmap.last")
		delete(newConfig, "volatile.idmap.next")
	}

	// Update the database if something changed.
	if len(changedConfig) != 0 || newDesc != curVol.Description {
		err = b.state.Cluster.UpdateStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID(), newDesc, newConfig)
		if err != nil {
			return err
		}
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeUpdated.Event(newVol, string(newVol.Type()), projectName, op, nil))

	return nil
}

// UpdateCustomVolumeSnapshot updates the description of a custom volume snapshot.
// Volume config is not allowed to be updated and will return an error.
func (b *lxdBackend) UpdateCustomVolumeSnapshot(projectName string, volName string, newDesc string, newConfig map[string]string, newExpiryDate time.Time, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName, "newDesc": newDesc, "newConfig": newConfig, "newExpiryDate": newExpiryDate})
	logger.Debug("UpdateCustomVolumeSnapshot started")
	defer logger.Debug("UpdateCustomVolumeSnapshot finished")

	if !shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume must be a snapshot")
	}

	// Get current config to compare what has changed.
	volID, curVol, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return errors.Wrapf(err, "Volume doesn't exist")
		}

		return err
	}

	curExpiryDate, err := b.state.Cluster.GetStorageVolumeSnapshotExpiry(volID)
	if err != nil {
		return err
	}

	if newConfig != nil {
		changedConfig, _ := b.detectChangedConfig(curVol.Config, newConfig)
		if len(changedConfig) != 0 {
			return fmt.Errorf("Volume config is not editable")
		}
	}

	// Update the database if description changed. Use current config.
	if newDesc != curVol.Description || newExpiryDate != curExpiryDate {
		err = b.state.Cluster.UpdateStorageVolumeSnapshot(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID(), newDesc, curVol.Config, newExpiryDate)
		if err != nil {
			return err
		}
	}

	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(curVol.ContentType), curVol.Name, curVol.Config)
	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeSnapshotUpdated.Event(vol, string(vol.Type()), projectName, op, nil))

	return nil
}

// DeleteCustomVolume removes a custom volume and its snapshots.
func (b *lxdBackend) DeleteCustomVolume(projectName string, volName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName})
	logger.Debug("DeleteCustomVolume started")
	defer logger.Debug("DeleteCustomVolume finished")

	_, _, isSnap := shared.InstanceGetParentAndSnapshotName(volName)
	if isSnap {
		return fmt.Errorf("Volume name cannot be a snapshot")
	}

	// Retrieve a list of snapshots.
	snapshots, err := VolumeSnapshotsGet(b.state, projectName, b.name, volName, db.StoragePoolVolumeTypeCustom)
	if err != nil {
		return err
	}

	// Remove each snapshot.
	for _, snapshot := range snapshots {
		err = b.DeleteCustomVolumeSnapshot(projectName, snapshot.Name, op)
		if err != nil {
			return err
		}
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	// Get the volume.
	_, poolVol, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	// Get the content type.
	dbContentType, err := VolumeContentTypeNameToContentType(poolVol.ContentType)
	if err != nil {
		return err
	}

	contentType, err := VolumeDBContentTypeToContentType(dbContentType)
	if err != nil {
		return err
	}

	// There's no need to pass config as it's not needed when deleting a volume.
	vol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, nil)

	// Delete the volume from the storage device. Must come after snapshots are removed.
	if b.driver.HasVolume(vol) {
		err = b.driver.DeleteVolume(vol, op)
		if err != nil {
			return err
		}
	}

	// Remove backups directory for volume.
	backupsPath := shared.VarPath("backups", "custom", b.name, project.StorageVolume(projectName, volName))
	if shared.PathExists(backupsPath) {
		err := os.RemoveAll(backupsPath)
		if err != nil {
			return err
		}
	}

	// Finally, remove the volume record from the database.
	err = b.state.Cluster.RemoveStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeDeleted.Event(vol, string(vol.Type()), projectName, op, nil))

	return nil
}

// GetCustomVolumeDisk returns the location of the disk.
func (b *lxdBackend) GetCustomVolumeDisk(projectName, volName string) (string, error) {
	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return "", err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	// There's no need to pass config as it's not needed when getting the volume usage.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), volStorageName, nil)

	return b.driver.GetVolumeDiskPath(vol)
}

// GetCustomVolumeUsage returns the disk space used by the custom volume.
func (b *lxdBackend) GetCustomVolumeUsage(projectName, volName string) (int64, error) {
	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return -1, err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	// There's no need to pass config as it's not needed when getting the volume usage.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), volStorageName, nil)

	return b.driver.GetVolumeUsage(vol)
}

// MountCustomVolume mounts a custom volume.
func (b *lxdBackend) MountCustomVolume(projectName, volName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName})
	logger.Debug("MountCustomVolume started")
	defer logger.Debug("MountCustomVolume finished")

	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), volStorageName, volume.Config)

	return b.driver.MountVolume(vol, op)
}

// UnmountCustomVolume unmounts a custom volume.
func (b *lxdBackend) UnmountCustomVolume(projectName, volName string, op *operations.Operation) (bool, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName})
	logger.Debug("UnmountCustomVolume started")
	defer logger.Debug("UnmountCustomVolume finished")

	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return false, err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), volStorageName, volume.Config)

	return b.driver.UnmountVolume(vol, false, op)
}

// CreateCustomVolumeSnapshot creates a snapshot of a custom volume.
func (b *lxdBackend) CreateCustomVolumeSnapshot(projectName, volName string, newSnapshotName string, newExpiryDate time.Time, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName, "newSnapshotName": newSnapshotName, "newExpiryDate": newExpiryDate})
	logger.Debug("CreateCustomVolumeSnapshot started")
	defer logger.Debug("CreateCustomVolumeSnapshot finished")

	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume cannot be snapshot")
	}

	if shared.IsSnapshot(newSnapshotName) {
		return fmt.Errorf("Snapshot name is not a valid snapshot name")
	}

	fullSnapshotName := drivers.GetSnapshotVolumeName(volName, newSnapshotName)

	// Check snapshot volume doesn't exist already.
	_, _, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, fullSnapshotName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != db.ErrNoSuchObject {
		if err != nil {
			return err
		}

		return fmt.Errorf("Snapshot by that name already exists")
	}

	// Load parent volume information and check it exists.
	_, parentVol, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Parent volume doesn't exist")
		}

		return err
	}

	// Create database entry for new storage volume snapshot.
	err = VolumeDBCreate(b.state, b, projectName, fullSnapshotName, parentVol.Description, drivers.VolumeTypeCustom, true, parentVol.Config, newExpiryDate, drivers.ContentType(parentVol.ContentType))
	if err != nil {
		return err
	}

	revertDB := true
	defer func() {
		if revertDB {
			b.state.Cluster.RemoveStoragePoolVolume(projectName, fullSnapshotName, db.StoragePoolVolumeTypeCustom, b.ID())
		}
	}()

	volDBContentType, err := VolumeContentTypeNameToContentType(parentVol.ContentType)
	if err != nil {
		return err
	}

	contentType, err := VolumeDBContentTypeToContentType(volDBContentType)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, fullSnapshotName)
	vol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, parentVol.Config)

	// Create the snapshot on the storage device.
	err = b.driver.CreateVolumeSnapshot(vol, op)
	if err != nil {
		return err
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeSnapshotCreated.Event(vol, string(vol.Type()), projectName, op, log.Ctx{"type": vol.Type()}))

	revertDB = false
	return nil
}

// RenameCustomVolumeSnapshot renames a custom volume.
func (b *lxdBackend) RenameCustomVolumeSnapshot(projectName, volName string, newSnapshotName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName, "newSnapshotName": newSnapshotName})
	logger.Debug("RenameCustomVolumeSnapshot started")
	defer logger.Debug("RenameCustomVolumeSnapshot finished")

	parentName, oldSnapshotName, isSnap := shared.InstanceGetParentAndSnapshotName(volName)
	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	if shared.IsSnapshot(newSnapshotName) {
		return fmt.Errorf("Invalid new snapshot name")
	}

	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	// There's no need to pass config as it's not needed when renaming a volume.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), volStorageName, nil)

	err = b.driver.RenameVolumeSnapshot(vol, newSnapshotName, op)
	if err != nil {
		return err
	}

	newVolName := drivers.GetSnapshotVolumeName(parentName, newSnapshotName)
	err = b.state.Cluster.RenameStoragePoolVolume(projectName, volName, newVolName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		// Get the volume name on storage.
		newVolStorageName := project.StorageVolume(projectName, newVolName)

		// Revert rename.
		newVol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), newVolStorageName, nil)
		b.driver.RenameVolumeSnapshot(newVol, oldSnapshotName, op)
		return err
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeSnapshotRenamed.Event(vol, string(vol.Type()), projectName, op, log.Ctx{"old_name": oldSnapshotName}))

	return nil
}

// DeleteCustomVolumeSnapshot removes a custom volume snapshot.
func (b *lxdBackend) DeleteCustomVolumeSnapshot(projectName, volName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName})
	logger.Debug("DeleteCustomVolumeSnapshot started")
	defer logger.Debug("DeleteCustomVolumeSnapshot finished")

	isSnap := shared.IsSnapshot(volName)

	if !isSnap {
		return fmt.Errorf("Volume name must be a snapshot")
	}

	// Get the volume.
	_, poolVol, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	// Get the content type.
	dbContentType, err := VolumeContentTypeNameToContentType(poolVol.ContentType)
	if err != nil {
		return err
	}

	contentType, err := VolumeDBContentTypeToContentType(dbContentType)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	// There's no need to pass config as it's not needed when deleting a volume snapshot.
	vol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, nil)

	// Delete the snapshot from the storage device.
	// Must come before DB RemoveStoragePoolVolume so that the volume ID is still available.
	if b.driver.HasVolume(vol) {
		err := b.driver.DeleteVolumeSnapshot(vol, op)
		if err != nil {
			return err
		}
	}

	// Remove the snapshot volume record from the database.
	err = b.state.Cluster.RemoveStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		return err
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeSnapshotDeleted.Event(vol, string(vol.Type()), projectName, op, nil))

	return nil
}

// RestoreCustomVolume restores a custom volume from a snapshot.
func (b *lxdBackend) RestoreCustomVolume(projectName, volName string, snapshotName string, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volName": volName, "snapshotName": snapshotName})
	logger.Debug("RestoreCustomVolume started")
	defer logger.Debug("RestoreCustomVolume finished")

	// Quick checks.
	if shared.IsSnapshot(volName) {
		return fmt.Errorf("Volume cannot be snapshot")
	}

	if shared.IsSnapshot(snapshotName) {
		return fmt.Errorf("Invalid snapshot name")
	}

	// Get current volume.
	_, curVol, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return errors.Wrapf(err, "Volume doesn't exist")
		}

		return err
	}

	// Check that the volume isn't in use by running instances.
	err = VolumeUsedByInstanceDevices(b.state, b.Name(), projectName, curVol, true, func(dbInst db.Instance, project api.Project, profiles []api.Profile, usedByDevices []string) error {
		inst, err := instance.Load(b.state, db.InstanceToArgs(&dbInst), profiles)
		if err != nil {
			return err
		}

		if inst.IsRunning() {
			return fmt.Errorf("Cannot restore custom volume used by running instances")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Get the volume config.
	_, dbVol, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.ID())
	if err != nil {
		if err == db.ErrNoSuchObject {
			return errors.Wrapf(err, "Volume doesn't exist")
		}

		return err
	}

	dbContentType, err := VolumeContentTypeNameToContentType(dbVol.ContentType)
	if err != nil {
		return err
	}

	contentType, err := VolumeDBContentTypeToContentType(dbContentType)
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)
	vol := b.newVolume(drivers.VolumeTypeCustom, contentType, volStorageName, dbVol.Config)

	err = b.driver.RestoreVolume(vol, snapshotName, op)
	if err != nil {
		snapErr, ok := err.(drivers.ErrDeleteSnapshots)
		if ok {
			// We need to delete some snapshots and try again.
			for _, snapName := range snapErr.Snapshots {
				err := b.DeleteCustomVolumeSnapshot(projectName, fmt.Sprintf("%s/%s", volName, snapName), op)
				if err != nil {
					return err
				}
			}

			// Now try again.
			err = b.driver.RestoreVolume(vol, snapshotName, op)
			if err != nil {
				return err
			}
		}

		return err
	}

	b.state.Events.SendLifecycle(projectName, lifecycle.StorageVolumeRestored.Event(vol, string(vol.Type()), projectName, op, log.Ctx{"snapshot": snapshotName}))

	return nil
}

func (b *lxdBackend) createStorageStructure(path string) error {
	for _, volType := range b.driver.Info().VolumeTypes {
		for _, name := range drivers.BaseDirectories[volType] {
			path := filepath.Join(path, name)
			err := os.MkdirAll(path, 0711)
			if err != nil && !os.IsExist(err) {
				return errors.Wrapf(err, "Failed to create directory %q", path)
			}
		}
	}

	return nil
}

// UpdateInstanceBackupFile writes the instance's config to the backup.yaml file on the storage device.
func (b *lxdBackend) UpdateInstanceBackupFile(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("UpdateInstanceBackupFile started")
	defer logger.Debug("UpdateInstanceBackupFile finished")

	// We only write backup files out for actual instances.
	if inst.IsSnapshot() {
		return nil
	}

	// Immediately return if the instance directory doesn't exist yet.
	if !shared.PathExists(inst.Path()) {
		return os.ErrNotExist
	}

	// Generate the YAML.
	ci, _, err := inst.Render()
	if err != nil {
		return errors.Wrap(err, "Failed to render instance metadata")
	}

	snapshots, err := inst.Snapshots()
	if err != nil {
		return errors.Wrap(err, "Failed to get snapshots")
	}

	var sis []*api.InstanceSnapshot

	for _, s := range snapshots {
		si, _, err := s.Render()
		if err != nil {
			return err
		}

		sis = append(sis, si.(*api.InstanceSnapshot))
	}

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	volDBType, err := VolumeTypeToDBType(volType)
	if err != nil {
		return err
	}

	contentType := InstanceContentType(inst)

	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(inst.Project(), inst.Name(), volDBType, b.ID())
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(&backup.Config{
		Container: ci.(*api.Instance),
		Snapshots: sis,
		Pool:      &b.db,
		Volume:    volume,
	})
	if err != nil {
		return err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())
	vol := b.newVolume(volType, contentType, volStorageName, volume.Config)

	// Update pool information in the backup.yaml file.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		// Write the YAML
		path := filepath.Join(inst.Path(), "backup.yaml")
		f, err := os.Create(path)
		if err != nil {
			return errors.Wrapf(err, "Failed to create file %q", path)
		}
		defer f.Close()

		err = f.Chmod(0400)
		if err != nil {
			return err
		}

		err = shared.WriteAll(f, data)
		if err != nil {
			return err
		}

		return nil
	}, op)

	return err
}

// CheckInstanceBackupFileSnapshots compares the snapshots on the storage device to those defined in the backup
// config supplied and returns an error if they do not match (if deleteMissing argument is false).
// If deleteMissing argument is true, then any snapshots that exist on the storage device but not in the backup
// config are removed from the storage device, and any snapshots that exist in the backup config but do not exist
// on the storage device are ignored. The remaining set of snapshots that exist on both the storage device and the
// backup config are returned. They set can be used to re-create the snapshot database entries when importing.
func (b *lxdBackend) CheckInstanceBackupFileSnapshots(backupConf *backup.Config, projectName string, deleteMissing bool, op *operations.Operation) ([]*api.InstanceSnapshot, error) {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "instance": backupConf.Container.Name, "deleteMissing": deleteMissing})
	logger.Debug("CheckInstanceBackupFileSnapshots started")
	defer logger.Debug("CheckInstanceBackupFileSnapshots finished")

	instType, err := instancetype.New(string(backupConf.Container.Type))
	if err != nil {
		return nil, err
	}

	volType, err := InstanceTypeToVolumeType(instType)
	if err != nil {
		return nil, err
	}

	// Get the volume name on storage.
	volStorageName := project.Instance(projectName, backupConf.Container.Name)

	contentType := drivers.ContentTypeFS
	if volType == drivers.VolumeTypeVM {
		contentType = drivers.ContentTypeBlock
	}

	// We don't need to use the volume's config for mounting so set to nil.
	vol := b.newVolume(volType, contentType, volStorageName, nil)

	// Get a list of snapshots that exist on storage device.
	driverSnapshots, err := vol.Snapshots(op)
	if err != nil {
		return nil, err
	}

	if len(backupConf.Snapshots) != len(driverSnapshots) {
		if !deleteMissing {
			return nil, errors.Wrap(ErrBackupSnapshotsMismatch, "Snapshot count in backup config and storage device are different")
		}
	}

	// Delete snapshots that do not exist in backup config.
	for _, driverSnapVol := range driverSnapshots {
		_, driverSnapOnly, _ := shared.InstanceGetParentAndSnapshotName(driverSnapVol.Name())

		inBackupFile := false
		for _, backupFileSnap := range backupConf.Snapshots {
			backupFileSnapOnly := backupFileSnap.Name

			// Handle legacy usage by internalImport (that prefixes the instance name to snapshot name).
			// TODO remove this once interalImport is removed.
			if strings.Contains(backupFileSnapOnly, shared.SnapshotDelimiter) {
				_, backupFileSnapOnly, _ = shared.InstanceGetParentAndSnapshotName(backupFileSnapOnly)
			}

			if driverSnapOnly == backupFileSnapOnly {
				inBackupFile = true
				break
			}
		}

		if inBackupFile {
			continue
		}

		if !deleteMissing {
			return nil, errors.Wrapf(ErrBackupSnapshotsMismatch, "Snapshot %q exists on storage device but not in backup config", driverSnapOnly)
		}

		err = b.driver.DeleteVolumeSnapshot(driverSnapVol, op)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to delete snapshot %q", driverSnapOnly)
		}

		logger.Warn("Deleted snapshot as not present in backup config", log.Ctx{"snapshot": driverSnapOnly})
	}

	// Check the snapshots in backup config exist on storage device.
	existingSnapshots := []*api.InstanceSnapshot{}
	for _, backupFileSnap := range backupConf.Snapshots {
		backupFileSnapOnly := backupFileSnap.Name

		// Handle legacy usage by internalImport (that prefixes the instance name to snapshot name).
		// TODO remove this once interalImport is removed.
		if strings.Contains(backupFileSnapOnly, shared.SnapshotDelimiter) {
			_, backupFileSnapOnly, _ = shared.InstanceGetParentAndSnapshotName(backupFileSnapOnly)
		}

		onStorageDevice := false
		for _, driverSnapVol := range driverSnapshots {
			_, driverSnapOnly, _ := shared.InstanceGetParentAndSnapshotName(driverSnapVol.Name())
			if driverSnapOnly == backupFileSnapOnly {
				onStorageDevice = true
				break
			}
		}

		if !onStorageDevice {
			if !deleteMissing {
				return nil, errors.Wrapf(ErrBackupSnapshotsMismatch, "Snapshot %q exists in backup config but not on storage device", backupFileSnapOnly)
			}

			logger.Warn("Skipped snapshot in backup config as not present on storage device", log.Ctx{"snapshot": backupFileSnap})
			continue // Skip snapshots missing on storage device.
		}

		existingSnapshots = append(existingSnapshots, backupFileSnap)
	}

	return existingSnapshots, nil
}

// ListInstances returns instances that exist on the storage pool but don't have records in the database.
// It attempts to mount each unknown instance volume and parses the the backup file.
// Returns each unknown instance's parsed backup config keyed on project name, as well as any snapshots keyed on
// project and instance name.
func (b *lxdBackend) ListUnknownInstances(op *operations.Operation) (map[string][]*backup.Config, map[string]map[string][]*api.InstanceSnapshot, error) {
	// Get a list of volumes on the storage pool. We only expect to get 1 volume per logical LXD volume.
	// So for VMs we only expect to get the block volume for a VM and not its filesystem one too. This way we
	// can operate on the volume using the existing storage pool functions and let the pool then handle the
	// associated filesystem volume as needed.
	poolVols, err := b.driver.ListVolumes()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Failed getting pool volumes")
	}

	projectInsts := make(map[string][]*backup.Config)
	projectInstSnapshots := make(map[string]map[string][]*api.InstanceSnapshot)

	for _, poolVol := range poolVols {
		volType := poolVol.Type()

		// Skip non-instance volumes.
		if volType != drivers.VolumeTypeVM && volType != drivers.VolumeTypeContainer {
			continue
		}

		// If the storage driver has returned a filesystem volume for a VM, this is a break of protocol.
		if volType == drivers.VolumeTypeVM && poolVol.ContentType() == drivers.ContentTypeFS {
			return nil, nil, fmt.Errorf("Storage driver returned unexpected VM volume with filesystem content type (%q)", poolVol.Name())
		}

		volDBType, err := VolumeTypeToDBType(volType)
		if err != nil {
			return nil, nil, err
		}

		projectName, instName := project.InstanceParts(poolVol.Name())

		// Check if an entry for the instance already exists in the DB.
		instID, err := b.state.Cluster.GetInstanceID(projectName, instName)
		if err != nil && errors.Cause(err) != db.ErrNoSuchObject {
			return nil, nil, err
		}

		// Check if any entry for the instance volume already exists in the DB.
		// This will return no record for any temporary pool structs being used (as ID is -1).
		volID, _, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, instName, volDBType, b.ID())
		if err != nil && errors.Cause(err) != db.ErrNoSuchObject {
			return nil, nil, err
		}

		if instID > 0 && volID > 0 {
			continue // Instance already fully exists in DB, no recovery needed.
		} else if instID > 0 {
			return nil, nil, fmt.Errorf("Instance %q in project %q already has instance DB record", instName, projectName)
		} else if volID > 0 {
			return nil, nil, fmt.Errorf("Instance %q in project %q already has storage DB record", instName, projectName)
		}

		backupYamlPath := filepath.Join(poolVol.MountPath(), "backup.yaml")
		var backupConf *backup.Config

		// If the instance is running, it should already be mounted, so check if the backup file
		// is already accessible, and if so parse it directly, without disturbing the mount count.
		if shared.PathExists(backupYamlPath) {
			backupConf, err = backup.ParseConfigYamlFile(backupYamlPath)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "Failed parsing backup file %q", backupYamlPath)
			}
		} else {
			// If backup file not accessible, we take this to mean the instance isn't running
			// and so we need to mount the volume to access the backup file and then unmount.
			// This will also create the mount path if needed.
			err = poolVol.MountTask(func(_ string, _ *operations.Operation) error {
				backupConf, err = backup.ParseConfigYamlFile(backupYamlPath)
				if err != nil {
					return errors.Wrapf(err, "Failed parsing backup file %q", backupYamlPath)
				}

				return nil
			}, op)
			if err != nil {
				return nil, nil, err
			}
		}

		// Run some consistency checks on the backup file contents.
		if backupConf.Pool != nil {
			if backupConf.Pool.Name != b.name {
				return nil, nil, fmt.Errorf("Instance %q in project %q has pool name mismatch in its backup file (%q doesn't match's pool's %q)", instName, projectName, backupConf.Pool.Name, b.name)
			}

			if backupConf.Pool.Driver != b.Driver().Info().Name {
				return nil, nil, fmt.Errorf("Instance %q in project %q has pool driver mismatch in its backup file (%q doesn't match's pool's %q)", instName, projectName, backupConf.Pool.Driver, b.Driver().Name())
			}
		}

		if backupConf.Container == nil {
			return nil, nil, fmt.Errorf("Instance %q in project %q has no instance information in its backup file", instName, projectName)
		}

		if instName != backupConf.Container.Name {
			return nil, nil, fmt.Errorf("Instance %q in project %q has a different instance name in its backup file (%q)", instName, projectName, backupConf.Container.Name)
		}

		apiInstType, err := VolumeTypeToAPIInstanceType(volType)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "Failed checking instance type for instance %q in project %q", instName, projectName)
		}

		if apiInstType != api.InstanceType(backupConf.Container.Type) {
			return nil, nil, fmt.Errorf("Instance %q in project %q has a different instance type in its backup file (%q)", instName, projectName, backupConf.Container.Type)
		}

		if backupConf.Volume == nil {
			return nil, nil, fmt.Errorf("Instance %q in project %q has no volume information in its backup file", instName, projectName)
		}

		if instName != backupConf.Volume.Name {
			return nil, nil, fmt.Errorf("Instance %q in project %q has a different volume name in its backup file (%q)", instName, projectName, backupConf.Volume.Name)
		}

		instVolDBType, err := VolumeTypeNameToDBType(backupConf.Volume.Type)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "Failed checking instance volume type for instance %q in project %q", instName, projectName)
		}

		instVolType, err := VolumeDBTypeToType(instVolDBType)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "Failed checking instance volume type for instance %q in project %q", instName, projectName)
		}

		if volType != instVolType {
			return nil, nil, fmt.Errorf("Instance %q in project %q has a different volume type in its backup file (%q)", instName, projectName, backupConf.Volume.Type)
		}

		// Add to volume to unknown volumes list for the project.
		if projectInsts[projectName] == nil {
			projectInsts[projectName] = []*backup.Config{backupConf}
		} else {
			projectInsts[projectName] = append(projectInsts[projectName], backupConf)
		}

		// Check snapshots are consistent.
		snapshots, err := b.CheckInstanceBackupFileSnapshots(backupConf, projectName, false, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("Instance %q in project %q has snapshot inconsistency: %v", instName, projectName, err)
		}

		// Store consistent snapshot list project and instance for consumption after validation scan.
		if projectInstSnapshots[projectName] == nil {
			projectInstSnapshots[projectName] = map[string][]*api.InstanceSnapshot{
				instName: snapshots,
			}
		} else {
			projectInstSnapshots[projectName][instName] = snapshots
		}
	}

	return projectInsts, projectInstSnapshots, nil
}

// ImportInstance takes an existing instance volume on the storage backend and ensures that the volume directories
// and symlinks are restored as needed to make it operational with LXD. Used during the recovery import stage.
func (b *lxdBackend) ImportInstance(inst instance.Instance, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": inst.Project(), "instance": inst.Name()})
	logger.Debug("ImportInstance started")
	defer logger.Debug("ImportInstance finished")

	volType, err := InstanceTypeToVolumeType(inst.Type())
	if err != nil {
		return err
	}

	// Get any snapshots the instance has in the format <instance name>/<snapshot name>.
	snapshots, err := b.state.Cluster.GetInstanceSnapshotsNames(inst.Project(), inst.Name())
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	contentType := InstanceContentType(inst)

	// Get the volume name on storage.
	volStorageName := project.Instance(inst.Project(), inst.Name())

	vol := b.newVolume(volType, contentType, volStorageName, nil)

	if inst.IsRunning() {
		// If the instance is running then this implies the volume is mounted, but if the LXD daemon has
		// been restarted since the DB records were removed then there will be no mount reference counter
		// showing the volume is in use. If this is the case then call mount the volume to increment the
		// reference counter.
		if !vol.MountInUse() {
			_, err = b.MountInstance(inst, op)
			if err != nil {
				return errors.Wrapf(err, "Failed mounting instance")
			}
		}
	} else {
		// If the instance isn't running then try and unmount it to ensure consistent state after import.
		_, err = b.UnmountInstance(inst, op)
		if err != nil {
			return errors.Wrapf(err, "Failed unmounting instance")
		}
	}

	// Create symlink.
	err = b.ensureInstanceSymlink(inst.Type(), inst.Project(), inst.Name(), vol.MountPath())
	if err != nil {
		return err
	}

	revert.Add(func() {
		// Remove symlinks.
		b.removeInstanceSymlink(inst.Type(), inst.Project(), inst.Name())
		b.removeInstanceSnapshotSymlinkIfUnused(inst.Type(), inst.Project(), inst.Name())
	})

	// Create snapshot mount paths and snapshot symlink if needed.
	if len(snapshots) > 0 {
		for _, snapName := range snapshots {
			_, snapOnlyName, _ := shared.InstanceGetParentAndSnapshotName(snapName)
			logger.Debug("Ensuring instance snapshot mount path", log.Ctx{"snapshot": snapOnlyName})

			snapVol, err := vol.NewSnapshot(snapOnlyName)
			if err != nil {
				return err
			}

			err = snapVol.EnsureMountPath()
			if err != nil {
				return err
			}
		}

		err = b.ensureInstanceSnapshotSymlink(inst.Type(), inst.Project(), inst.Name())
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

func (b *lxdBackend) BackupCustomVolume(projectName string, volName string, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots bool, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": projectName, "volume": volName, "optimized": optimized, "snapshots": snapshots})
	logger.Debug("BackupCustomVolume started")
	defer logger.Debug("BackupCustomVolume finished")

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(projectName, volName)

	_, volume, err := b.state.Cluster.GetLocalStoragePoolVolume(projectName, volName, db.StoragePoolVolumeTypeCustom, b.id)
	if err != nil {
		return err
	}

	var snapNames []string
	if snapshots {
		// Get snapshots in age order, oldest first, and pass names to storage driver.
		volSnaps, err := b.state.Cluster.GetLocalStoragePoolVolumeSnapshotsWithType(projectName, volName, db.StoragePoolVolumeTypeCustom, b.id)
		if err != nil {
			return err
		}

		snapNames = make([]string, 0, len(volSnaps))
		for _, volSnap := range volSnaps {
			_, snapName, _ := shared.InstanceGetParentAndSnapshotName(volSnap.Name)
			snapNames = append(snapNames, snapName)
		}
	}

	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(volume.ContentType), volStorageName, volume.Config)

	err = b.driver.BackupVolume(vol, tarWriter, optimized, snapNames, op)
	if err != nil {
		return err
	}

	return nil
}

func (b *lxdBackend) CreateCustomVolumeFromBackup(srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) error {
	logger := logging.AddContext(b.logger, log.Ctx{"project": srcBackup.Project, "volume": srcBackup.Name, "snapshots": srcBackup.Snapshots, "optimizedStorage": *srcBackup.OptimizedStorage})
	logger.Debug("CreateCustomVolumeFromBackup started")
	defer logger.Debug("CreateCustomVolumeFromBackup finished")

	if srcBackup.Config == nil || srcBackup.Config.Volume == nil {
		return fmt.Errorf("Valid volume config not found in index")
	}

	if len(srcBackup.Snapshots) != len(srcBackup.Config.VolumeSnapshots) {
		return fmt.Errorf("Valid volume snapshot config not found in index")
	}

	// Check whether we are allowed to create volumes.
	req := api.StorageVolumesPost{
		StorageVolumePut: api.StorageVolumePut{
			Config: srcBackup.Config.Volume.Config,
		},
		Name: srcBackup.Name,
	}
	err := b.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		return project.AllowVolumeCreation(tx, srcBackup.Project, req)
	})
	if err != nil {
		return errors.Wrapf(err, "Failed checking volume creation allowed")
	}

	revert := revert.New()
	defer revert.Fail()

	// Get the volume name on storage.
	volStorageName := project.StorageVolume(srcBackup.Project, srcBackup.Name)

	// Validate config.
	vol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(srcBackup.Config.Volume.ContentType), volStorageName, srcBackup.Config.Volume.Config)

	// Strip any unsupported config keys (in case the export was made from a different type of storage pool).
	err = b.driver.ValidateVolume(vol, true)
	if err != nil {
		return err
	}

	// Create database entry for new storage volume using the validated config.
	err = VolumeDBCreate(b.state, b, srcBackup.Project, srcBackup.Name, srcBackup.Config.Volume.Description, vol.Type(), false, vol.Config(), time.Time{}, vol.ContentType())
	if err != nil {
		return err
	}

	revert.Add(func() {
		b.state.Cluster.RemoveStoragePoolVolume(srcBackup.Project, srcBackup.Name, db.StoragePoolVolumeTypeCustom, b.ID())
	})

	// Create database entries fro new storage volume snapshots.
	for _, s := range srcBackup.Config.VolumeSnapshots {
		snapshot := s // Local var for revert.
		_, snapName, _ := shared.InstanceGetParentAndSnapshotName(snapshot.Name)
		fullSnapName := drivers.GetSnapshotVolumeName(srcBackup.Name, snapName)
		snapVolStorageName := project.StorageVolume(srcBackup.Project, fullSnapName)
		snapVol := b.newVolume(drivers.VolumeTypeCustom, drivers.ContentType(srcBackup.Config.Volume.ContentType), snapVolStorageName, srcBackup.Config.Volume.Config)

		// Strip any unsupported config keys (in case the export was made from a different type of storage pool).
		err := b.driver.ValidateVolume(snapVol, true)
		if err != nil {
			return err
		}

		err = VolumeDBCreate(b.state, b, srcBackup.Project, fullSnapName, snapshot.Description, snapVol.Type(), true, snapVol.Config(), *snapshot.ExpiresAt, snapVol.ContentType())
		if err != nil {
			return err
		}

		revert.Add(func() {
			b.state.Cluster.RemoveStoragePoolVolume(srcBackup.Project, fullSnapName, db.StoragePoolVolumeTypeCustom, b.ID())
		})
	}

	// Unpack the backup into the new storage volume(s).
	volPostHook, revertHook, err := b.driver.CreateVolumeFromBackup(vol, srcBackup, srcData, op)
	if err != nil {
		return err
	}

	if revertHook != nil {
		revert.Add(revertHook)
	}

	// If the driver returned a post hook, return error as custom volumes don't need post hooks and we expect
	// the storage driver to understand this distinction and ensure that all activities done in the postHook
	// normally are done in CreateVolumeFromBackup as the DB record is created ahead of time.
	if volPostHook != nil {
		return fmt.Errorf("Custom volume restore doesn't support post hooks")
	}

	b.state.Events.SendLifecycle(srcBackup.Project, lifecycle.StorageVolumeCreated.Event(vol, string(vol.Type()), srcBackup.Project, op, log.Ctx{"type": vol.Type()}))

	revert.Success()
	return nil
}
