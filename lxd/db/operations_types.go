package db

// OperationType is a numeric code indentifying the type of an Operation.
type OperationType int64

// Possible values for OperationType
//
// WARNING: The type codes are stored in the database, so this list of
//          definitions should be normally append-only. Any other change
//          requires a database update.
const (
	OperationUnknown OperationType = iota
	OperationClusterBootstrap
	OperationClusterJoin
	OperationBackupCreate
	OperationBackupRename
	OperationBackupRestore
	OperationBackupRemove
	OperationConsoleShow
	OperationInstanceCreate
	OperationInstanceUpdate
	OperationInstanceRename
	OperationInstanceMigrate
	OperationInstanceLiveMigrate
	OperationInstanceFreeze
	OperationInstanceUnfreeze
	OperationInstanceDelete
	OperationInstanceStart
	OperationInstanceStop
	OperationInstanceRestart
	OperationCommandExec
	OperationSnapshotCreate
	OperationSnapshotRename
	OperationSnapshotRestore
	OperationSnapshotTransfer
	OperationSnapshotUpdate
	OperationSnapshotDelete
	OperationImageDownload
	OperationImageDelete
	OperationImageToken
	OperationImageRefresh
	OperationVolumeCopy
	OperationVolumeCreate
	OperationVolumeMigrate
	OperationVolumeMove
	OperationVolumeSnapshotCreate
	OperationVolumeSnapshotDelete
	OperationVolumeSnapshotUpdate
	OperationProjectRename
	OperationImagesExpire
	OperationImagesPruneLeftover
	OperationImagesUpdate
	OperationImagesSynchronize
	OperationLogsExpire
	OperationInstanceTypesUpdate
	OperationBackupsExpire
	OperationSnapshotsExpire
	OperationCustomVolumeSnapshotsExpire
	OperationCustomVolumeBackupCreate
	OperationCustomVolumeBackupRemove
	OperationCustomVolumeBackupRename
	OperationCustomVolumeBackupRestore
	OperationWarningsPruneResolved
	OperationClusterJoinToken
	OperationVolumeSnapshotRename
	OperationClusterMemberEvacuate
	OperationClusterMemberRestore
)

// Description return a human-readable description of the operation type.
func (t OperationType) Description() string {
	switch t {
	case OperationClusterBootstrap:
		return "Creating bootstrap node"
	case OperationClusterJoin:
		return "Joining cluster"
	case OperationBackupCreate:
		return "Backing up instance"
	case OperationBackupRename:
		return "Renaming instance backup"
	case OperationBackupRestore:
		return "Restoring backup"
	case OperationBackupRemove:
		return "Removing instance backup"
	case OperationConsoleShow:
		return "Showing console"
	case OperationInstanceCreate:
		return "Creating instance"
	case OperationInstanceUpdate:
		return "Updating instance"
	case OperationInstanceRename:
		return "Renaming instance"
	case OperationInstanceMigrate:
		return "Migrating instance"
	case OperationInstanceLiveMigrate:
		return "Live-migrating instance"
	case OperationInstanceFreeze:
		return "Freezing instance"
	case OperationInstanceUnfreeze:
		return "Unfreezing instance"
	case OperationInstanceDelete:
		return "Deleting instance"
	case OperationInstanceStart:
		return "Starting instance"
	case OperationInstanceStop:
		return "Stopping instance"
	case OperationInstanceRestart:
		return "Restarting instance"
	case OperationCommandExec:
		return "Executing command"
	case OperationSnapshotCreate:
		return "Snapshotting instance"
	case OperationSnapshotRename:
		return "Renaming snapshot"
	case OperationSnapshotRestore:
		return "Restoring snapshot"
	case OperationSnapshotTransfer:
		return "Transferring snapshot"
	case OperationSnapshotUpdate:
		return "Updating snapshot"
	case OperationSnapshotDelete:
		return "Deleting snapshot"
	case OperationImageDownload:
		return "Downloading image"
	case OperationImageDelete:
		return "Deleting image"
	case OperationImageToken:
		return "Image download token"
	case OperationImageRefresh:
		return "Refreshing image"
	case OperationVolumeCopy:
		return "Copying storage volume"
	case OperationVolumeCreate:
		return "Creating storage volume"
	case OperationVolumeMigrate:
		return "Migrating storage volume"
	case OperationVolumeMove:
		return "Moving storage volume"
	case OperationVolumeSnapshotCreate:
		return "Creating storage volume snapshot"
	case OperationVolumeSnapshotDelete:
		return "Deleting storage volume snapshot"
	case OperationVolumeSnapshotUpdate:
		return "Updating storage volume snapshot"
	case OperationVolumeSnapshotRename:
		return "Renaming storage volume snapshot"
	case OperationProjectRename:
		return "Renaming project"
	case OperationImagesExpire:
		return "Cleaning up expired images"
	case OperationImagesPruneLeftover:
		return "Pruning leftover image files"
	case OperationImagesUpdate:
		return "Updating images"
	case OperationImagesSynchronize:
		return "Synchronizing images"
	case OperationLogsExpire:
		return "Expiring log files"
	case OperationInstanceTypesUpdate:
		return "Updating instance types"
	case OperationBackupsExpire:
		return "Cleaning up expired instance backups"
	case OperationSnapshotsExpire:
		return "Cleaning up expired instance snapshots"
	case OperationCustomVolumeSnapshotsExpire:
		return "Cleaning up expired volume snapshots"
	case OperationCustomVolumeBackupCreate:
		return "Creating custom volume backup"
	case OperationCustomVolumeBackupRemove:
		return "Deleting custom volume backup"
	case OperationCustomVolumeBackupRename:
		return "Renaming custom volume backup"
	case OperationCustomVolumeBackupRestore:
		return "Restoring custom volume backup"
	case OperationWarningsPruneResolved:
		return "Pruning resolved warnings"
	case OperationClusterMemberEvacuate:
		return "Evacuating cluster member"
	case OperationClusterMemberRestore:
		return "Restoring cluster member"
	default:
		return "Executing operation"
	}
}

// Permission returns the needed RBAC permission to cancel the oepration
func (t OperationType) Permission() string {
	switch t {
	case OperationBackupCreate:
		return "operate-containers"
	case OperationBackupRename:
		return "operate-containers"
	case OperationBackupRestore:
		return "operate-containers"
	case OperationBackupRemove:
		return "operate-containers"
	case OperationConsoleShow:
		return "operate-containers"
	case OperationInstanceFreeze:
		return "operate-containers"
	case OperationInstanceUnfreeze:
		return "operate-containers"
	case OperationInstanceStart:
		return "operate-containers"
	case OperationInstanceStop:
		return "operate-containers"
	case OperationInstanceRestart:
		return "operate-containers"
	case OperationCommandExec:
		return "operate-containers"
	case OperationSnapshotCreate:
		return "operate-containers"
	case OperationSnapshotRename:
		return "operate-containers"
	case OperationSnapshotTransfer:
		return "operate-containers"
	case OperationSnapshotUpdate:
		return "operate-containers"
	case OperationSnapshotDelete:
		return "operate-containers"

	case OperationInstanceCreate:
		return "manage-containers"
	case OperationInstanceUpdate:
		return "manage-containers"
	case OperationInstanceRename:
		return "manage-containers"
	case OperationInstanceMigrate:
		return "manage-containers"
	case OperationInstanceLiveMigrate:
		return "manage-containers"
	case OperationInstanceDelete:
		return "manage-containers"
	case OperationSnapshotRestore:
		return "manage-containers"

	case OperationImageDownload:
		return "manage-images"
	case OperationImageDelete:
		return "manage-images"
	case OperationImageToken:
		return "manage-images"
	case OperationImageRefresh:
		return "manage-images"
	case OperationImagesUpdate:
		return "manage-images"
	case OperationImagesSynchronize:
		return "manage-images"

	case OperationCustomVolumeSnapshotsExpire:
		return "operate-volumes"
	case OperationCustomVolumeBackupCreate:
		return "manage-storage-volumes"
	case OperationCustomVolumeBackupRemove:
		return "manage-storage-volumes"
	case OperationCustomVolumeBackupRename:
		return "manage-storage-volumes"
	case OperationCustomVolumeBackupRestore:
		return "manage-storage-volumes"
	}

	return ""
}
