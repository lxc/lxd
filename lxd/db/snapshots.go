//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"fmt"
	"time"

	"github.com/lxc/lxd/shared"
)

// Code generation directives.
//
//go:generate -command mapper lxd-generate db mapper -t snapshots.mapper.go
//go:generate mapper reset
//
//go:generate mapper stmt -p db -e instance_snapshot objects
//go:generate mapper stmt -p db -e instance_snapshot id
//go:generate mapper stmt -p db -e instance_snapshot config-ref
//go:generate mapper stmt -p db -e instance_snapshot devices-ref
//go:generate mapper stmt -p db -e instance_snapshot create struct=InstanceSnapshot
//go:generate mapper stmt -p db -e instance_snapshot create-config-ref
//go:generate mapper stmt -p db -e instance_snapshot create-devices-ref
//go:generate mapper stmt -p db -e instance_snapshot rename
//go:generate mapper stmt -p db -e instance_snapshot delete
//
//go:generate mapper method -p db -e instance_snapshot List
//go:generate mapper method -p db -e instance_snapshot Get
//go:generate mapper method -p db -e instance_snapshot ID struct=InstanceSnapshot
//go:generate mapper method -p db -e instance_snapshot Exists struct=InstanceSnapshot
//go:generate mapper method -p db -e instance_snapshot Create struct=InstanceSnapshot
//go:generate mapper method -p db -e instance_snapshot ConfigRef
//go:generate mapper method -p db -e instance_snapshot DevicesRef
//go:generate mapper method -p db -e instance_snapshot Rename
//go:generate mapper method -p db -e instance_snapshot DeleteOne

// InstanceSnapshot is a value object holding db-related details about a snapshot.
type InstanceSnapshot struct {
	ID           int
	Project      string `db:"primary=yes&join=projects.name&via=instance"`
	Instance     string `db:"primary=yes&join=instances.name"`
	Name         string `db:"primary=yes"`
	CreationDate time.Time
	Stateful     bool
	Description  string `db:"coalesce=''"`
	Config       map[string]string
	Devices      map[string]map[string]string
	ExpiryDate   time.Time
}

// InstanceSnapshotFilter specifies potential query parameter fields.
type InstanceSnapshotFilter struct {
	Project  string
	Instance string
	Name     string
}

// InstanceSnapshotToInstance is a temporary convenience function to merge
// together an Instance struct and a SnapshotInstance struct into into a the
// legacy Instance struct for a snapshot.
func InstanceSnapshotToInstance(instance *Instance, snapshot *InstanceSnapshot) Instance {
	return Instance{
		ID:           snapshot.ID,
		Project:      snapshot.Project,
		Name:         instance.Name + shared.SnapshotDelimiter + snapshot.Name,
		Node:         instance.Node,
		Type:         instance.Type,
		Snapshot:     true,
		Architecture: instance.Architecture,
		Ephemeral:    false,
		CreationDate: snapshot.CreationDate,
		Stateful:     snapshot.Stateful,
		LastUseDate:  time.Time{},
		Description:  snapshot.Description,
		Config:       snapshot.Config,
		Devices:      snapshot.Devices,
		Profiles:     instance.Profiles,
		ExpiryDate:   snapshot.ExpiryDate,
	}
}

// UpdateInstanceSnapshotConfig inserts/updates/deletes the provided config keys.
func (c *ClusterTx) UpdateInstanceSnapshotConfig(id int, values map[string]string) error {
	insertSQL := "INSERT OR REPLACE INTO instances_snapshots_config (instance_snapshot_id, key, value) VALUES"
	deleteSQL := "DELETE FROM instances_snapshots_config WHERE key IN %s AND instance_snapshot_id=?"
	return c.configUpdate(id, values, insertSQL, deleteSQL)
}

// UpdateInstanceSnapshot updates the description and expiry date of the
// instance snapshot with the given ID.
func (c *ClusterTx) UpdateInstanceSnapshot(id int, description string, expiryDate time.Time) error {
	str := fmt.Sprintf("UPDATE instances_snapshots SET description=?, expiry_date=? WHERE id=?")
	stmt, err := c.tx.Prepare(str)
	if err != nil {
		return err
	}
	defer stmt.Close()

	if expiryDate.IsZero() {
		_, err = stmt.Exec(description, "", id)
	} else {
		_, err = stmt.Exec(description, expiryDate, id)
	}
	if err != nil {
		return err
	}

	return nil
}

// GetInstanceSnapshotID returns the ID of the snapshot with the given name.
func (c *Cluster) GetInstanceSnapshotID(project, instance, name string) (int, error) {
	var id int64
	err := c.Transaction(func(tx *ClusterTx) error {
		var err error
		id, err = tx.GetInstanceSnapshotID(project, instance, name)
		return err
	})
	return int(id), err
}
