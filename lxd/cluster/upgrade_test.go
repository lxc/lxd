package cluster_test

import (
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/driver"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A node can unblock other nodes that were waiting for a cluster upgrade to
// complete.
func TestNotifyUpgradeCompleted(t *testing.T) {
	f := heartbeatFixture{t: t}
	defer f.Cleanup()

	gateway0 := f.Bootstrap()
	gateway1 := f.Grow()

	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		gateway1.WaitUpgradeNotification()
		wg.Done()
	}()

	state0 := f.State(gateway0)
	serverCert0 := gateway0.ServerCert()
	err := cluster.NotifyUpgradeCompleted(state0, serverCert0, serverCert0)
	require.NoError(t, err)

	wg.Wait()
}

// The task function checks if the node is out of date and runs whatever is in
// LXD_CLUSTER_UPDATE if so.
func TestMaybeUpdate_Upgrade(t *testing.T) {
	dir, err := ioutil.TempDir("", "")
	require.NoError(t, err)

	defer os.RemoveAll(dir)

	// Create a stub upgrade script that just touches a stamp file.
	stamp := filepath.Join(dir, "stamp")
	script := filepath.Join(dir, "cluster-upgrade")
	data := []byte(fmt.Sprintf("#!/bin/sh\ntouch %s\n", stamp))
	err = ioutil.WriteFile(script, data, 0755)
	require.NoError(t, err)

	state, cleanup := state.NewTestState(t)
	defer cleanup()

	state.Node.Transaction(func(tx *db.NodeTx) error {
		nodes := []db.RaftNode{
			{ID: 1, Address: "0.0.0.0:666"},
			{ID: 2, Address: "1.2.3.4:666"},
		}
		err := tx.ReplaceRaftNodes(nodes)
		require.NoError(t, err)
		return nil
	})

	state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		id, err := tx.CreateNode("buzz", "1.2.3.4:666")
		require.NoError(t, err)

		node, err := tx.GetNodeByName("buzz")
		require.NoError(t, err)

		version := node.Version()
		version[0]++

		err = tx.SetNodeVersion(id, version)
		require.NoError(t, err)

		return nil
	})

	os.Setenv("LXD_CLUSTER_UPDATE", script)
	defer os.Unsetenv("LXD_CLUSTER_UPDATE")

	cluster.MaybeUpdate(state)

	_, err = os.Stat(stamp)
	require.NoError(t, err)
}

// If the node is up-to-date, nothing is done.
func TestMaybeUpdate_NothingToDo(t *testing.T) {
	dir, err := ioutil.TempDir("", "")
	require.NoError(t, err)

	defer os.RemoveAll(dir)

	// Create a stub upgrade script that just touches a stamp file.
	stamp := filepath.Join(dir, "stamp")
	script := filepath.Join(dir, "cluster-upgrade")
	data := []byte(fmt.Sprintf("#!/bin/sh\ntouch %s\n", stamp))
	err = ioutil.WriteFile(script, data, 0755)
	require.NoError(t, err)

	state, cleanup := state.NewTestState(t)
	defer cleanup()

	os.Setenv("LXD_CLUSTER_UPDATE", script)
	defer os.Unsetenv("LXD_CLUSTER_UPDATE")

	cluster.MaybeUpdate(state)

	_, err = os.Stat(stamp)
	require.True(t, os.IsNotExist(err))
}

func TestUpgradeMembersWithoutRole(t *testing.T) {
	state, cleanup := state.NewTestState(t)
	defer cleanup()

	serverCert := shared.TestingKeyPair()
	mux := http.NewServeMux()
	server := newServer(serverCert, mux)
	defer server.Close()

	address := server.Listener.Addr().String()
	setRaftRole(t, state.Node, address)

	gateway := newGateway(t, state.Node, serverCert, serverCert)
	defer gateway.Shutdown()

	trustedCerts := func() map[db.CertificateType]map[string]x509.Certificate {
		return nil
	}

	for path, handler := range gateway.HandlerFuncs(nil, trustedCerts) {
		mux.HandleFunc(path, handler)
	}

	var err error
	require.NoError(t, state.Cluster.Close())
	store := gateway.NodeStore()
	dial := gateway.DialFunc()
	state.Cluster, err = db.OpenCluster(
		"db.bin", store, address, "/unused/db/dir", 5*time.Second, nil,
		driver.WithDialFunc(dial))
	require.NoError(t, err)
	gateway.Cluster = state.Cluster

	// Add a couple of members to the database.
	var members []db.NodeInfo
	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		_, err := tx.CreateNode("foo", "1.2.3.4")
		require.NoError(t, err)
		_, err = tx.CreateNode("bar", "5.6.7.8")
		require.NoError(t, err)
		members, err = tx.GetNodes()
		require.NoError(t, err)
		return nil
	})
	require.NoError(t, err)

	err = cluster.UpgradeMembersWithoutRole(gateway, members)
	require.NoError(t, err)

	// The members have been added to the raft configuration.
	nodes, err := gateway.RaftNodes()
	require.NoError(t, err)

	assert.Len(t, nodes, 3)
	assert.Equal(t, uint64(1), nodes[0].ID)
	assert.Equal(t, address, nodes[0].Address)
	assert.Equal(t, uint64(2), nodes[1].ID)
	assert.Equal(t, "1.2.3.4", nodes[1].Address)
	assert.Equal(t, uint64(3), nodes[2].ID)
	assert.Equal(t, "5.6.7.8", nodes[2].Address)
}
