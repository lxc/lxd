package drivers

import (
	"io"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/instancewriter"
	"github.com/lxc/lxd/shared/logger"
)

// driver is the extended internal interface.
type driver interface {
	Driver

	init(state *state.State, name string, config map[string]string, logger logger.Logger, volIDFunc func(volType VolumeType, volName string) (int64, error), commonRules *Validators)
	load() error
	isRemote() bool
}

// Driver represents a low-level storage driver.
type Driver interface {
	// Internal.
	Info() Info
	HasVolume(vol Volume) bool

	// Export struct details.
	Name() string
	Config() map[string]string
	Logger() logger.Logger

	// Pool.
	Create() error
	Delete(op *operations.Operation) error
	// Mount mounts a storage pool if needed, returns true if we caused a new mount, false if already mounted.
	Mount() (bool, error)

	// Unmount unmounts a storage pool if needed, returns true if unmounted, false if was not mounted.
	Unmount() (bool, error)
	GetResources() (*api.ResourcesStoragePool, error)
	Validate(config map[string]string) error
	Update(changedConfig map[string]string) error
	ApplyPatch(name string) error

	// Volumes.
	FillVolumeConfig(vol Volume) error
	ValidateVolume(vol Volume, removeUnknownKeys bool) error
	CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error
	CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, op *operations.Operation) error
	RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error
	DeleteVolume(vol Volume, op *operations.Operation) error
	RenameVolume(vol Volume, newName string, op *operations.Operation) error
	UpdateVolume(vol Volume, changedConfig map[string]string) error
	GetVolumeUsage(vol Volume) (int64, error)
	SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error
	GetVolumeDiskPath(vol Volume) (string, error)
	ListVolumes() ([]Volume, error)

	// MountVolume mounts a storage volume (if not mounted) and increments reference counter.
	MountVolume(vol Volume, op *operations.Operation) error

	// MountVolumeSnapshot mounts a storage volume snapshot as readonly, returns true if we
	// caused a new mount, false if already mounted.
	MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error)

	// UnmountVolume unmounts a storage volume, returns true if unmounted, false if was not
	// mounted.
	UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error)

	// UnmountVolume unmounts a storage volume snapshot, returns true if unmounted, false if was
	// not mounted.
	UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error)

	CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error
	DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error
	RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error
	VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error)
	RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error

	// Migration.
	MigrationTypes(contentType ContentType, refresh bool) []migration.Type
	MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error
	CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error

	// Backup.
	BackupVolume(vol Volume, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots []string, op *operations.Operation) error
	CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (VolumePostHook, revert.Hook, error)
}
