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
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/instancewriter"
	"github.com/lxc/lxd/shared/logger"
)

type mockBackend struct {
	name   string
	state  *state.State
	logger logger.Logger
	driver drivers.Driver
}

func (b *mockBackend) ID() int64 {
	return 1 //  The tests expect the storage pool ID to be 1.
}

func (b *mockBackend) Name() string {
	return b.name
}

func (b *mockBackend) Description() string {
	return ""
}

func (b *mockBackend) Status() string {
	return api.NetworkStatusUnknown
}

func (b *mockBackend) LocalStatus() string {
	return api.NetworkStatusUnknown
}

func (b *mockBackend) Driver() drivers.Driver {
	return b.driver
}

func (b *mockBackend) MigrationTypes(contentType drivers.ContentType, refresh bool) []migration.Type {
	return []migration.Type{
		{
			FSType:   FallbackMigrationType(contentType),
			Features: []string{"xattrs", "delete", "compress", "bidirectional"},
		},
	}
}

func (b *mockBackend) GetResources() (*api.ResourcesStoragePool, error) {
	return nil, nil
}

func (b *mockBackend) IsUsed() (bool, error) {
	return false, nil
}

func (b *mockBackend) Delete(clientType request.ClientType, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) Update(clientType request.ClientType, newDescription string, newConfig map[string]string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) Create(clientType request.ClientType, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) Mount() (bool, error) {
	return true, nil
}

func (b *mockBackend) Unmount() (bool, error) {
	return true, nil
}

func (b *mockBackend) ApplyPatch(name string) error {
	return nil
}

func (b *mockBackend) FillInstanceConfig(inst instance.Instance, config map[string]string) error {
	return nil
}

func (b *mockBackend) CreateInstance(inst instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CreateInstanceFromBackup(srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (func(instance.Instance) error, revert.Hook, error) {
	return nil, nil, nil
}

func (b *mockBackend) CreateInstanceFromCopy(inst instance.Instance, src instance.Instance, snapshots bool, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CreateInstanceFromImage(inst instance.Instance, fingerprint string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CreateInstanceFromMigration(inst instance.Instance, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) RenameInstance(inst instance.Instance, newName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) DeleteInstance(inst instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) UpdateInstance(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) UpdateInstanceBackupFile(inst instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CheckInstanceBackupFileSnapshots(backupConf *backup.Config, projectName string, deleteMissing bool, op *operations.Operation) ([]*api.InstanceSnapshot, error) {
	return nil, nil
}

func (b *mockBackend) ListUnknownInstances(op *operations.Operation) (map[string][]*backup.Config, map[string]map[string][]*api.InstanceSnapshot, error) {
	return nil, nil, nil
}

func (b *mockBackend) ImportInstance(inst instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) MigrateInstance(inst instance.Instance, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) RefreshInstance(i instance.Instance, src instance.Instance, srcSnapshots []instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) BackupInstance(inst instance.Instance, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots bool, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) GetInstanceUsage(inst instance.Instance) (int64, error) {
	return 0, nil
}

func (b *mockBackend) SetInstanceQuota(inst instance.Instance, size string, vmStateSize string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) MountInstance(inst instance.Instance, op *operations.Operation) (*MountInfo, error) {
	return &MountInfo{}, nil
}

func (b *mockBackend) UnmountInstance(inst instance.Instance, op *operations.Operation) (bool, error) {
	return true, nil
}

func (b *mockBackend) CreateInstanceSnapshot(i instance.Instance, src instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) RenameInstanceSnapshot(inst instance.Instance, newName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) DeleteInstanceSnapshot(inst instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) RestoreInstanceSnapshot(inst instance.Instance, src instance.Instance, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) MountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (*MountInfo, error) {
	return &MountInfo{}, nil
}

func (b *mockBackend) UnmountInstanceSnapshot(inst instance.Instance, op *operations.Operation) (bool, error) {
	return true, nil
}

func (b *mockBackend) UpdateInstanceSnapshot(inst instance.Instance, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) EnsureImage(fingerprint string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) DeleteImage(fingerprint string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) UpdateImage(fingerprint, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CreateCustomVolume(projectName string, volName string, desc string, config map[string]string, contentType drivers.ContentType, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CreateCustomVolumeFromCopy(projectName string, srcProjectName string, volName string, desc string, config map[string]string, srcPoolName string, srcVolName string, srcVolOnly bool, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) RenameCustomVolume(projectName string, volName string, newName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) UpdateCustomVolume(projectName string, volName string, newDesc string, newConfig map[string]string, op *operations.Operation) error {
	return drivers.ErrNotImplemented
}

func (b *mockBackend) DeleteCustomVolume(projectName string, volName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) MigrateCustomVolume(projectName string, conn io.ReadWriteCloser, args *migration.VolumeSourceArgs, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CreateCustomVolumeFromMigration(projectName string, conn io.ReadWriteCloser, args migration.VolumeTargetArgs, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) GetCustomVolumeDisk(projectName string, volName string) (string, error) {
	return "", nil
}

func (b *mockBackend) GetCustomVolumeUsage(projectName string, volName string) (int64, error) {
	return 0, nil
}

func (b *mockBackend) MountCustomVolume(projectName string, volName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) UnmountCustomVolume(projectName string, volName string, op *operations.Operation) (bool, error) {
	return true, nil
}

func (b *mockBackend) CreateCustomVolumeSnapshot(projectName string, volName string, newSnapshotName string, expiryDate time.Time, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) RenameCustomVolumeSnapshot(projectName string, volName string, newName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) DeleteCustomVolumeSnapshot(projectName string, volName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) UpdateCustomVolumeSnapshot(projectName string, volName string, newDesc string, newConfig map[string]string, expiryDate time.Time, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) RestoreCustomVolume(projectName string, volName string, snapshotName string, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) BackupCustomVolume(projectName string, volName string, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots bool, op *operations.Operation) error {
	return nil
}

func (b *mockBackend) CreateCustomVolumeFromBackup(srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) error {
	return nil
}
