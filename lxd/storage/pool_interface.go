package storage

import (
	"io"
	"time"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/instancewriter"
)

// MountInfo represents info about the result of a mount operation.
type MountInfo struct {
	DiskPath string // The location of the block disk (if supported).
}

// Pool represents a LXD storage pool.
type Pool interface {
	// Pool.
	ID() int64
	Name() string
	Driver() drivers.Driver
	Description() string
	Status() string
	LocalStatus() string

	GetResources() (*api.ResourcesStoragePool, error)
	IsUsed() (bool, error)
	Delete(clientType request.ClientType, op *operations.Operation) error
	Update(clientType request.ClientType, newDesc string, newConfig map[string]string, op *operations.Operation) error

	Create(clientType request.ClientType, op *operations.Operation) error
	Mount() (bool, error)
	Unmount() (bool, error)

	ApplyPatch(name string) error

	// Instances.
	FillInstanceConfig(inst instance.Instance, config map[string]string) error
	CreateInstance(inst instance.Instance, op *operations.Operation) error
	CreateInstanceFromBackup(srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (func(instance.Instance) error, revert.Hook, error)
	CreateInstanceFromCopy(inst instance.Instance, src instance.Instance, snapshots bool, op *operations.Operation) error
	CreateInstanceFromImage(inst instance.Instance, fingerprint string, op *operations.Operation) error
	CreateInstanceFromMigration(inst instance.Instance, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error
	RenameInstance(inst instance.Instance, newName string, op *operations.Operation) error
	DeleteInstance(inst instance.Instance, op *operations.Operation) error
	UpdateInstance(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error
	UpdateInstanceBackupFile(inst instance.Instance, op *operations.Operation) error
	CheckInstanceBackupFileSnapshots(backupConf *backup.Config, projectName string, deleteMissing bool, op *operations.Operation) ([]*api.InstanceSnapshot, error)
	ListUnknownInstances(op *operations.Operation) (map[string][]*backup.Config, map[string]map[string][]*api.InstanceSnapshot, error)
	ImportInstance(inst instance.Instance, op *operations.Operation) error

	MigrateInstance(inst instance.Instance, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error
	RefreshInstance(inst instance.Instance, src instance.Instance, srcSnapshots []instance.Instance, op *operations.Operation) error
	BackupInstance(inst instance.Instance, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots bool, op *operations.Operation) error

	GetInstanceUsage(inst instance.Instance) (int64, error)
	SetInstanceQuota(inst instance.Instance, size string, vmStateSize string, op *operations.Operation) error

	MountInstance(inst instance.Instance, op *operations.Operation) (*MountInfo, error)
	UnmountInstance(inst instance.Instance, op *operations.Operation) (bool, error)

	// Instance snapshots.
	CreateInstanceSnapshot(inst instance.Instance, src instance.Instance, op *operations.Operation) error
	RenameInstanceSnapshot(inst instance.Instance, newName string, op *operations.Operation) error
	DeleteInstanceSnapshot(inst instance.Instance, op *operations.Operation) error
	RestoreInstanceSnapshot(inst instance.Instance, src instance.Instance, op *operations.Operation) error
	MountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (*MountInfo, error)
	UnmountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (bool, error)
	UpdateInstanceSnapshot(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error

	// Images.
	EnsureImage(fingerprint string, op *operations.Operation) error
	DeleteImage(fingerprint string, op *operations.Operation) error
	UpdateImage(fingerprint string, newDesc string, newConfig map[string]string, op *operations.Operation) error

	// Custom volumes.
	CreateCustomVolume(projectName string, volName string, desc string, config map[string]string, contentType drivers.ContentType, op *operations.Operation) error
	CreateCustomVolumeFromCopy(projectName string, srcProjectName string, volName, desc string, config map[string]string, srcPoolName, srcVolName string, srcVolOnly bool, op *operations.Operation) error
	UpdateCustomVolume(projectName string, volName string, newDesc string, newConfig map[string]string, op *operations.Operation) error
	RenameCustomVolume(projectName string, volName string, newVolName string, op *operations.Operation) error
	DeleteCustomVolume(projectName string, volName string, op *operations.Operation) error
	GetCustomVolumeDisk(projectName string, volName string) (string, error)
	GetCustomVolumeUsage(projectName string, volName string) (int64, error)
	MountCustomVolume(projectName string, volName string, op *operations.Operation) error
	UnmountCustomVolume(projectName string, volName string, op *operations.Operation) (bool, error)

	// Custom volume snapshots.
	CreateCustomVolumeSnapshot(projectName string, volName string, newSnapshotName string, newExpiryDate time.Time, op *operations.Operation) error
	RenameCustomVolumeSnapshot(projectName string, volName string, newSnapshotName string, op *operations.Operation) error
	DeleteCustomVolumeSnapshot(projectName string, volName string, op *operations.Operation) error
	UpdateCustomVolumeSnapshot(projectName string, volName string, newDesc string, newConfig map[string]string, newExpiryDate time.Time, op *operations.Operation) error
	RestoreCustomVolume(projectName string, volName string, snapshotName string, op *operations.Operation) error

	// Custom volume migration.
	MigrationTypes(contentType drivers.ContentType, refresh bool) []migration.Type
	CreateCustomVolumeFromMigration(projectName string, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error
	MigrateCustomVolume(projectName string, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error

	// Custom volume backups.
	BackupCustomVolume(projectName string, volName string, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots bool, op *operations.Operation) error
	CreateCustomVolumeFromBackup(srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) error
}
