//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/instance/instancetype"
)

func TestGetInstanceSnapshots(t *testing.T) {
	tx, cleanup := db.NewTestClusterTx(t)
	defer cleanup()

	nodeID1 := int64(1) // This is the default local node

	addContainer(t, tx, nodeID1, "c1")
	addContainer(t, tx, nodeID1, "c2")

	addInstanceSnapshot(t, tx, 1, "snap1")
	addInstanceSnapshot(t, tx, 2, "snap2")
	addInstanceSnapshot(t, tx, 2, "snap3")

	addInstanceSnapshotConfig(t, tx, "c2", "snap2", "x", "y")

	addInstanceSnapshotDevice(t, tx, "c2", "snap2", "eth0", "nic", nil)
	addInstanceSnapshotDevice(t, tx, "c2", "snap3", "root", "disk", map[string]string{"x": "y"})

	filter := db.InstanceSnapshotFilter{}
	snapshots, err := tx.GetInstanceSnapshots(filter)
	require.NoError(t, err)
	assert.Len(t, snapshots, 3)

	s1 := snapshots[0]
	assert.Equal(t, "snap1", s1.Name)
	assert.Equal(t, "c1", s1.Instance)
	assert.Equal(t, map[string]string{}, s1.Config)
	assert.Len(t, s1.Devices, 0)

	s2 := snapshots[1]
	assert.Equal(t, "snap2", s2.Name)
	assert.Equal(t, "c2", s2.Instance)
	assert.Equal(t, map[string]string{"x": "y"}, s2.Config)
	assert.Len(t, s2.Devices, 1)
	assert.Equal(t, map[string]string{"type": "nic"}, s2.Devices["eth0"])

	s3 := snapshots[2]
	assert.Equal(t, "snap3", s3.Name)
	assert.Equal(t, "c2", s3.Instance)
	assert.Equal(t, map[string]string{}, s3.Config)
	assert.Len(t, s3.Devices, 1)
	assert.Equal(t, map[string]string{"type": "disk", "x": "y"}, s3.Devices["root"])
}

func TestGetInstanceSnapshots_FilterByInstance(t *testing.T) {
	tx, cleanup := db.NewTestClusterTx(t)
	defer cleanup()

	nodeID1 := int64(1) // This is the default local node

	addContainer(t, tx, nodeID1, "c1")
	addContainer(t, tx, nodeID1, "c2")
	addInstanceSnapshot(t, tx, 1, "snap1")
	addInstanceSnapshot(t, tx, 2, "snap1")
	addInstanceSnapshot(t, tx, 2, "snap2")

	filter := db.InstanceSnapshotFilter{Project: "default", Instance: "c2"}
	snapshots, err := tx.GetInstanceSnapshots(filter)
	require.NoError(t, err)
	assert.Len(t, snapshots, 2)

	s1 := snapshots[0]
	assert.Equal(t, "snap1", s1.Name)
	assert.Equal(t, "c2", s1.Instance)

	s2 := snapshots[1]
	assert.Equal(t, "snap2", s2.Name)
	assert.Equal(t, "c2", s2.Instance)
}

func TestGetInstanceSnapshots_SameNameInDifferentProjects(t *testing.T) {
	tx, cleanup := db.NewTestClusterTx(t)
	defer cleanup()

	// Create an additional project
	project1 := db.Project{}
	project1.Name = "p1"
	_, err := tx.CreateProject(project1)
	require.NoError(t, err)

	// Create an instance in the default project.
	i1default := db.Instance{
		Project:      "default",
		Name:         "i1",
		Node:         "none",
		Type:         instancetype.Container,
		Architecture: 1,
		Ephemeral:    false,
		Stateful:     true,
	}
	_, err = tx.CreateInstance(i1default)
	require.NoError(t, err)

	// Create an instance in project p1 using the same name.
	i1p1 := db.Instance{
		Project:      "p1",
		Name:         "i1",
		Node:         "none",
		Type:         instancetype.Container,
		Architecture: 1,
		Ephemeral:    false,
		Stateful:     true,
	}
	_, err = tx.CreateInstance(i1p1)
	require.NoError(t, err)

	// Create two snapshots with the same names.
	s1default := db.InstanceSnapshot{
		Project:  "default",
		Instance: "i1",
		Name:     "s1",
	}
	_, err = tx.CreateInstanceSnapshot(s1default)
	require.NoError(t, err)

	s1p1 := db.InstanceSnapshot{
		Project:  "p1",
		Instance: "i1",
		Name:     "s1",
	}
	_, err = tx.CreateInstanceSnapshot(s1p1)
	require.NoError(t, err)

	filter := db.InstanceSnapshotFilter{Project: "p1", Instance: "i1"}
	snapshots, err := tx.GetInstanceSnapshots(filter)
	require.NoError(t, err)

	assert.Len(t, snapshots, 1)

	assert.Equal(t, "p1", snapshots[0].Project)
	assert.Equal(t, "i1", snapshots[0].Instance)
	assert.Equal(t, "s1", snapshots[0].Name)

	snapshot, err := tx.GetInstanceSnapshot("default", "i1", "s1")
	require.NoError(t, err)

	assert.Equal(t, "default", snapshot.Project)
	assert.Equal(t, "i1", snapshot.Instance)
	assert.Equal(t, "s1", snapshot.Name)
}

func addInstanceSnapshot(t *testing.T, tx *db.ClusterTx, instanceID int64, name string) {
	stmt := `
INSERT INTO instances_snapshots(instance_id, name, creation_date) VALUES (?, ?, ?)
`
	_, err := tx.Tx().Exec(stmt, instanceID, name, time.Now())
	require.NoError(t, err)
}

// Return the instance snapshot ID given its name and instance name.
func getInstanceSnapshotID(t *testing.T, tx *db.ClusterTx, instance, name string) int64 {
	var id int64

	stmt := `
SELECT instances_snapshots.id
FROM instances_snapshots
JOIN instances ON instances.id=instances_snapshots.instance_id
WHERE instances.name=? AND instances_snapshots.name=?
`
	row := tx.Tx().QueryRow(stmt, instance, name)
	err := row.Scan(&id)
	require.NoError(t, err)

	return id
}

func addInstanceSnapshotConfig(t *testing.T, tx *db.ClusterTx, instance, name, key, value string) {
	id := getInstanceSnapshotID(t, tx, instance, name)

	stmt := `
INSERT INTO instances_snapshots_config(instance_snapshot_id, key, value) VALUES (?, ?, ?)
`
	_, err := tx.Tx().Exec(stmt, id, key, value)
	require.NoError(t, err)
}

// Return the instance snapshot device ID given its instance snapshot ID and name.
func getInstanceSnapshotDeviceID(t *testing.T, tx *db.ClusterTx, instanceSnapshotID int64, name string) int64 {
	var id int64

	stmt := "SELECT id FROM instances_snapshots_devices WHERE instance_snapshot_id=? AND name=?"
	row := tx.Tx().QueryRow(stmt, instanceSnapshotID, name)
	err := row.Scan(&id)
	require.NoError(t, err)

	return id
}

func addInstanceSnapshotDevice(t *testing.T, tx *db.ClusterTx, instance, snapshot, name, typ string, config map[string]string) {
	id := getInstanceSnapshotID(t, tx, instance, snapshot)

	code, err := db.DeviceTypeToInt(typ)
	require.NoError(t, err)

	stmt := `
INSERT INTO instances_snapshots_devices(instance_snapshot_id, name, type) VALUES (?, ?, ?)
`
	_, err = tx.Tx().Exec(stmt, id, name, code)
	require.NoError(t, err)

	deviceID := getInstanceSnapshotDeviceID(t, tx, id, name)

	for key, value := range config {
		stmt := `
INSERT INTO instances_snapshots_devices_config(instance_snapshot_device_id, key, value) VALUES (?, ?, ?)
`
		_, err = tx.Tx().Exec(stmt, deviceID, key, value)
		require.NoError(t, err)
	}
}
