package cluster

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/canonical/go-dqlite/app"
	"github.com/canonical/go-dqlite/client"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/version"
	"github.com/pkg/errors"
)

// Bootstrap turns a non-clustered LXD instance into the first (and leader)
// node of a new LXD cluster.
//
// This instance must already have its cluster.https_address set and be listening
// on the associated network address.
func Bootstrap(state *state.State, gateway *Gateway, serverName string) error {
	// Check parameters
	if serverName == "" {
		return fmt.Errorf("Server name must not be empty")
	}

	err := membershipCheckNoLeftoverClusterCert(state.OS.VarDir)
	if err != nil {
		return err
	}

	var address string

	err = state.Node.Transaction(func(tx *db.NodeTx) error {
		// Fetch current network address and raft nodes
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "failed to fetch node configuration")
		}

		address = config.ClusterAddress()

		// Make sure node-local database state is in order.
		err = membershipCheckNodeStateForBootstrapOrJoin(tx, address)
		if err != nil {
			return err
		}

		// Add ourselves as first raft node
		err = tx.CreateFirstRaftNode(address)
		if err != nil {
			return errors.Wrap(err, "failed to insert first raft node")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Update our own entry in the nodes table.
	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Make sure cluster database state is in order.
		err := membershipCheckClusterStateForBootstrapOrJoin(tx)
		if err != nil {
			return err
		}

		// Add ourselves to the nodes table.
		err = tx.UpdateNode(1, serverName, address)
		if err != nil {
			return errors.Wrap(err, "Failed updating cluster member")
		}

		err = EnsureServerCertificateTrusted(serverName, state.ServerCert(), tx)
		if err != nil {
			return errors.Wrap(err, "Failed ensuring server certificate is trusted")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Reload the trusted certificate cache to enable the certificate we just added to the local trust store
	// to be used when validating endpoint connections. This will allow Dqlite to connect to ourselves.
	state.UpdateCertificateCache()

	// Shutdown the gateway. This will trash any dqlite connection against
	// our in-memory dqlite driver and shutdown the associated raft
	// instance. We also lock regular access to the cluster database since
	// we don't want any other database code to run while we're
	// reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "failed to acquire cluster database lock")
	}

	err = gateway.Shutdown()
	if err != nil {
		return errors.Wrap(err, "failed to shutdown gRPC SQL gateway")
	}

	// The cluster CA certificate is a symlink against the regular server CA certificate.
	if shared.PathExists(filepath.Join(state.OS.VarDir, "server.ca")) {
		err := os.Symlink("server.ca", filepath.Join(state.OS.VarDir, "cluster.ca"))
		if err != nil {
			return errors.Wrap(err, "Failed to symlink server CA cert to cluster CA cert")
		}
	}

	// Generate a new cluster certificate.
	clusterCert, err := util.LoadClusterCert(state.OS.VarDir)
	if err != nil {
		return errors.Wrap(err, "Failed to create cluster cert")
	}

	// If endpoint listeners are active, apply new cluster certificate.
	if state.Endpoints != nil {
		gateway.networkCert = clusterCert
		state.Endpoints.NetworkUpdateCert(clusterCert)
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	err = gateway.init(true)
	if err != nil {
		return errors.Wrap(err, "Failed to re-initialize gRPC SQL gateway")
	}

	err = gateway.waitLeadership()
	if err != nil {
		return err
	}

	// Make sure we can actually connect to the cluster database through
	// the network endpoint. This also releases the previously acquired
	// lock and makes the Go SQL pooling system invalidate the old
	// connection, so new queries will be executed over the new network
	// connection.
	err = state.Cluster.ExitExclusive(func(tx *db.ClusterTx) error {
		_, err := tx.GetNodes()
		return err
	})
	if err != nil {
		return errors.Wrap(err, "cluster database initialization failed")
	}

	return nil
}

// EnsureServerCertificateTrusted adds the serverCert to the DB trusted certificates store using the serverName.
// If a certificate with the same fingerprint is already in the trust store, but is of the wrong type or name then
// the existing certificate is updated to the correct type and name. If the existing certificate is the correct
// type but the wrong name then an error is returned. And if the existing certificate is the correct type and name
// then nothing more is done.
func EnsureServerCertificateTrusted(serverName string, serverCert *shared.CertInfo, tx *db.ClusterTx) error {
	// Parse our server certificate and prepare to add it to DB trust store.
	serverCertx509, err := x509.ParseCertificate(serverCert.KeyPair().Certificate[0])
	if err != nil {
		return err
	}

	fingerprint := shared.CertFingerprint(serverCertx509)

	dbCert := db.Certificate{
		Fingerprint: fingerprint,
		Type:        db.CertificateTypeServer, // Server type for intra-member communication.
		Name:        serverName,
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertx509.Raw})),
	}

	// Add our server cert to the DB trust store (so when other members join this cluster they will be
	// able to trust intra-cluster requests from this member).
	existingCert, _ := tx.GetCertificate(dbCert.Fingerprint)
	if existingCert != nil {
		if existingCert.Name != dbCert.Name && existingCert.Type == db.CertificateTypeServer {
			// Don't alter an existing server certificate that has our fingerprint but not our name.
			// Something is wrong as this shouldn't happen.
			return fmt.Errorf("Existing server certificate with different name %q already in trust store", existingCert.Name)
		} else if existingCert.Name != dbCert.Name && existingCert.Type != db.CertificateTypeServer {
			// Ensure that if a client certificate already exists that matches our fingerprint, that it
			// has the correct name and type for cluster operation, to allow us to associate member
			// server names to certificate names.
			err = tx.UpdateCertificate(dbCert.Fingerprint, dbCert)
			if err != nil {
				return errors.Wrap(err, "Failed updating certificate name and type in trust store")
			}
		}
	} else {
		_, err = tx.CreateCertificate(dbCert)
		if err != nil {
			return errors.Wrapf(err, "Failed adding server certifcate to trust store")
		}
	}

	return nil
}

// Accept a new node and add it to the cluster.
//
// This instance must already be clustered.
//
// Return an updated list raft database nodes (possibly including the newly
// accepted node).
func Accept(state *state.State, gateway *Gateway, name, address string, schema, api, arch int) ([]db.RaftNode, error) {
	var maxVoters int64
	var maxStandBy int64

	// Check parameters
	if name == "" {
		return nil, fmt.Errorf("node name must not be empty")
	}
	if address == "" {
		return nil, fmt.Errorf("node address must not be empty")
	}

	// Insert the new node into the nodes table.
	var id int64
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		config, err := ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "Load cluster configuration")
		}
		maxVoters = config.MaxVoters()
		maxStandBy = config.MaxStandBy()

		// Check that the node can be accepted with these parameters.
		err = membershipCheckClusterStateForAccept(tx, name, address, schema, api)
		if err != nil {
			return err
		}

		// Add the new node
		id, err = tx.CreateNodeWithArch(name, address, arch)
		if err != nil {
			return errors.Wrap(err, "Failed to insert new node into the database")
		}

		// Mark the node as pending, so it will be skipped when
		// performing heartbeats or sending cluster
		// notifications.
		err = tx.SetNodePendingFlag(id, true)
		if err != nil {
			return errors.Wrap(err, "Failed to mark the new node as pending")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Possibly insert the new node into the raft_nodes table (if we have
	// less than 3 database nodes).
	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get raft nodes from the log")
	}
	count := len(nodes) // Existing nodes
	voters := 0
	standbys := 0
	for _, node := range nodes {
		switch node.Role {
		case db.RaftVoter:
			voters++
		case db.RaftStandBy:
			standbys++
		}
	}
	node := db.RaftNode{ID: uint64(id), Address: address, Role: db.RaftSpare}
	if count > 1 && voters < int(maxVoters) {
		node.Role = db.RaftVoter
	} else if standbys < int(maxStandBy) {
		node.Role = db.RaftStandBy
	}
	nodes = append(nodes, node)

	return nodes, nil
}

// Join makes a non-clustered LXD node join an existing cluster.
//
// It's assumed that Accept() was previously called against the leader node,
// which handed the raft server ID.
//
// The cert parameter must contain the keypair/CA material of the cluster being
// joined.
func Join(state *state.State, gateway *Gateway, networkCert *shared.CertInfo, serverCert *shared.CertInfo, name string, raftNodes []db.RaftNode) error {
	// Check parameters
	if name == "" {
		return fmt.Errorf("node name must not be empty")
	}

	var address string
	err := state.Node.Transaction(func(tx *db.NodeTx) error {
		// Fetch current network address and raft nodes
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "failed to fetch node configuration")
		}
		address = config.ClusterAddress()

		// Make sure node-local database state is in order.
		err = membershipCheckNodeStateForBootstrapOrJoin(tx, address)
		if err != nil {
			return err
		}

		// Set the raft nodes list to the one that was returned by Accept().
		err = tx.ReplaceRaftNodes(raftNodes)
		if err != nil {
			return errors.Wrap(err, "failed to set raft nodes")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Get the local config keys for the cluster pools and networks. It
	// assumes that the local storage pools and networks match the cluster
	// networks, if not an error will be returned. Also get any outstanding
	// operation, typically there will be just one, created by the POST
	// /cluster/nodes request which triggered this code.
	var pools map[string]map[string]string
	var networks map[string]map[string]string
	var operations []db.Operation

	err = state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		pools, err = tx.GetStoragePoolsLocalConfig()
		if err != nil {
			return err
		}
		networks, err = tx.GetNetworksLocalConfig()
		if err != nil {
			return err
		}
		filter := db.OperationFilter{NodeID: tx.GetNodeID()}
		operations, err = tx.GetOperations(filter)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Lock regular access to the cluster database since we don't want any
	// other database code to run while we're reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "failed to acquire cluster database lock")
	}

	// Shutdown the gateway and wipe any raft data. This will trash any
	// gRPC SQL connection against our in-memory dqlite driver and shutdown
	// the associated raft instance.
	err = gateway.Shutdown()
	if err != nil {
		return errors.Wrap(err, "failed to shutdown gRPC SQL gateway")
	}
	err = os.RemoveAll(state.OS.GlobalDatabaseDir())
	if err != nil {
		return errors.Wrap(err, "failed to remove existing raft data")
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	gateway.networkCert = networkCert
	err = gateway.init(false)
	if err != nil {
		return errors.Wrap(err, "failed to re-initialize gRPC SQL gateway")
	}

	// If we are listed among the database nodes, join the raft cluster.
	var info *db.RaftNode
	for _, node := range raftNodes {
		if node.Address == address {
			info = &node
		}
	}
	if info == nil {
		panic("joining node not found")
	}
	logger.Info("Joining dqlite raft cluster", log15.Ctx{"id": info.ID, "address": info.Address, "role": info.Role})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := client.FindLeader(
		ctx, gateway.NodeStore(),
		client.WithDialFunc(gateway.raftDial()),
		client.WithLogFunc(DqliteLog),
	)
	if err != nil {
		return errors.Wrap(err, "Failed to connect to cluster leader")
	}
	defer client.Close()

	logger.Info("Adding node to cluster", log15.Ctx{"id": info.ID, "address": info.Address, "role": info.Role})
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err = client.Add(ctx, *info)
	if err != nil {
		return errors.Wrap(err, "Failed to join cluster")
	}

	// Make sure we can actually connect to the cluster database through
	// the network endpoint. This also releases the previously acquired
	// lock and makes the Go SQL pooling system invalidate the old
	// connection, so new queries will be executed over the new gRPC
	// network connection. Also, update the storage_pools and networks
	// tables with our local configuration.
	logger.Info("Migrate local data to cluster database")
	err = state.Cluster.ExitExclusive(func(tx *db.ClusterTx) error {
		node, err := tx.GetPendingNodeByAddress(address)
		if err != nil {
			return errors.Wrap(err, "failed to get ID of joining node")
		}
		state.Cluster.NodeID(node.ID)
		tx.NodeID(node.ID)

		// Storage pools.
		ids, err := tx.GetNonPendingStoragePoolsNamesToIDs()
		if err != nil {
			return errors.Wrap(err, "failed to get cluster storage pool IDs")
		}
		for name, id := range ids {
			err := tx.UpdateStoragePoolAfterNodeJoin(id, node.ID)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's to the pool")
			}

			driver, err := tx.GetStoragePoolDriver(id)
			if err != nil {
				return errors.Wrap(err, "failed to get storage pool driver")
			}

			if shared.StringInSlice(driver, []string{"ceph", "cephfs"}) {
				// For ceph pools we have to create volume
				// entries for the joining node.
				err := tx.UpdateCephStoragePoolAfterNodeJoin(id, node.ID)
				if err != nil {
					return errors.Wrap(err, "failed to create ceph volumes for joining node")
				}
			} else {
				// For other pools we add the config provided by the joining node.
				config, ok := pools[name]
				if !ok {
					return fmt.Errorf("joining node has no config for pool %s", name)
				}
				err = tx.CreateStoragePoolConfig(id, node.ID, config)
				if err != nil {
					return errors.Wrap(err, "failed to add joining node's pool config")
				}
			}
		}

		// Networks.
		ids, err = tx.GetNonPendingNetworkIDs()
		if err != nil {
			return errors.Wrap(err, "failed to get cluster network IDs")
		}
		for name, id := range ids {
			config, ok := networks[name]
			if !ok {
				return fmt.Errorf("joining node has no config for network %s", name)
			}
			err := tx.NetworkNodeJoin(id, node.ID)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's to the network")
			}
			err = tx.CreateNetworkConfig(id, node.ID, config)
			if err != nil {
				return errors.Wrap(err, "failed to add joining node's network config")
			}
		}

		// Migrate outstanding operations.
		for _, operation := range operations {
			op := db.Operation{
				UUID:   operation.UUID,
				Type:   operation.Type,
				NodeID: *tx.GetNodeID(),
			}
			_, err := tx.CreateOrReplaceOperation(op)
			if err != nil {
				return errors.Wrapf(err, "failed to migrate operation %s", operation.UUID)
			}
		}

		// Remove the pending flag for ourselves
		// notifications.
		err = tx.SetNodePendingFlag(node.ID, false)
		if err != nil {
			return errors.Wrapf(err, "failed to unmark the node as pending")
		}

		// Generate partial heartbeat request containing just a raft node list.
		notifyNodesUpdate(raftNodes, info.ID, networkCert, serverCert)

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "cluster database initialization failed")
	}

	return nil
}

// Attempt to send a heartbeat to all other nodes to notify them of a new or changed member.
func notifyNodesUpdate(raftNodes []db.RaftNode, id uint64, networkCert *shared.CertInfo, serverCert *shared.CertInfo) {
	// Generate partial heartbeat request containing just a raft node list.
	hbState := &APIHeartbeat{}
	hbState.Time = time.Now().UTC()

	nodes := make([]db.NodeInfo, len(raftNodes))
	for i, raftNode := range raftNodes {
		nodes[i].ID = int64(raftNode.ID)
		nodes[i].Address = raftNode.Address
	}
	hbState.Update(false, raftNodes, nodes, 0)
	for _, node := range raftNodes {
		if node.ID == id {
			continue
		}
		go HeartbeatNode(context.Background(), node.Address, networkCert, serverCert, hbState)
	}
}

// Rebalance the raft cluster, trying to see if we have a spare online node
// that we can promote to voter node if we are below membershipMaxRaftVoters,
// or to standby if we are below membershipMaxStandBys.
//
// If there's such spare node, return its address as well as the new list of
// raft nodes.
func Rebalance(state *state.State, gateway *Gateway) (string, []db.RaftNode, error) {
	// If we're a standalone node, do nothing.
	if gateway.memoryDial != nil {
		return "", nil, nil
	}

	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return "", nil, errors.Wrap(err, "Get current raft nodes")
	}

	roles, err := newRolesChanges(state, gateway, nodes)
	if err != nil {
		return "", nil, err
	}

	role, candidates := roles.Adjust(gateway.info.ID)

	if role == -1 {
		// No node to promote
		return "", nodes, nil
	}

	// Check if we have a spare node that we can promote to the missing role.
	address := candidates[0].Address
	logger.Infof("Found node %s whose role needs to be changed to %s", address, role)

	for i, node := range nodes {
		if node.Address == address {
			nodes[i].Role = role
			break
		}
	}

	return address, nodes, nil
}

// Assign a new role to the local dqlite node.
func Assign(state *state.State, gateway *Gateway, nodes []db.RaftNode) error {
	// Figure out our own address.
	address := ""
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		address, err = tx.GetLocalNodeAddress()
		if err != nil {
			return errors.Wrap(err, "Failed to fetch the address of this cluster member")
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Ensure we actually have an address.
	if address == "" {
		return fmt.Errorf("Cluster member is not exposed on the network")
	}

	// Figure out our node identity.
	var info *db.RaftNode
	for i, node := range nodes {
		if node.Address == address {
			info = &nodes[i]
		}
	}

	// Ensure that our address was actually included in the given list of raft nodes.
	if info == nil {
		return fmt.Errorf("This node is not included in the given list of database nodes")
	}

	// Replace our local list of raft nodes with the given one (which
	// includes ourselves).
	err = state.Node.Transaction(func(tx *db.NodeTx) error {
		err = tx.ReplaceRaftNodes(nodes)
		if err != nil {
			return errors.Wrap(err, "Failed to set raft nodes")
		}

		return nil
	})
	if err != nil {
		return err
	}

	var transactor func(func(tx *db.ClusterTx) error) error

	// If we are already running a dqlite node, it means we have cleanly
	// joined the cluster before, using the roles support API. In that case
	// there's no need to restart the gateway and we can just change our
	// dqlite role.
	if gateway.IsDqliteNode() {
		transactor = state.Cluster.Transaction
		goto assign
	}

	// If we get here it means that we are an upgraded node from cluster
	// without roles support, or we didn't cleanly join the cluster. Either
	// way, we don't have a dqlite node running, so we need to restart the
	// gateway.

	// Lock regular access to the cluster database since we don't want any
	// other database code to run while we're reconfiguring raft.
	err = state.Cluster.EnterExclusive()
	if err != nil {
		return errors.Wrap(err, "Failed to acquire cluster database lock")
	}
	transactor = state.Cluster.ExitExclusive

	// Wipe all existing raft data, for good measure (perhaps they were
	// somehow leftover).
	err = os.RemoveAll(state.OS.GlobalDatabaseDir())
	if err != nil {
		return errors.Wrap(err, "Failed to remove existing raft data")
	}

	// Re-initialize the gateway. This will create a new raft factory an
	// dqlite driver instance, which will be exposed over gRPC by the
	// gateway handlers.
	err = gateway.init(false)
	if err != nil {
		return errors.Wrap(err, "failed to re-initialize gRPC SQL gateway")
	}

assign:
	logger.Info(
		"Changing dqlite raft role",
		log15.Ctx{"id": info.ID, "address": info.Address, "role": info.Role})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client, err := client.FindLeader(ctx, gateway.NodeStore(), client.WithDialFunc(gateway.raftDial()))
	if err != nil {
		return errors.Wrap(err, "Connect to cluster leader")
	}
	defer client.Close()

	// Figure out our current role.
	role := db.RaftRole(-1)
	cluster, err := client.Cluster(ctx)
	if err != nil {
		return errors.Wrap(err, "Fetch current cluster configuration")
	}
	for _, server := range cluster {
		if server.ID == info.ID {
			role = server.Role
			break
		}
	}
	if role == -1 {
		return fmt.Errorf("Node %s does not belong to the current raft configuration", address)
	}

	// If we're stepping back from voter to spare, let's first transition
	// to stand-by first and wait for the configuration change to be
	// notified to us. This prevent us from thinking we're still voters and
	// potentially disrupt the cluster.
	if role == db.RaftVoter && info.Role == db.RaftSpare {
		err = client.Assign(ctx, info.ID, db.RaftStandBy)
		if err != nil {
			return errors.Wrap(err, "Failed to step back to stand-by")
		}
		local, err := gateway.getClient()
		if err != nil {
			return errors.Wrap(err, "Failed to get local dqlite client")
		}
		notified := false
		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			servers, err := local.Cluster(context.Background())
			if err != nil {
				return errors.Wrap(err, "Failed to get current cluster")
			}
			for _, server := range servers {
				if server.ID != info.ID {
					continue
				}
				if server.Role == db.RaftStandBy {
					notified = true
					break
				}
			}
			if notified {
				break
			}
		}
		if !notified {
			return fmt.Errorf("Timeout waiting for configuration change notification")
		}
	}

	// Give the Assign operation a bit more budget in case we're promoting
	// to voter, since that might require a snapshot transfer.
	if info.Role == db.RaftVoter {
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
	}

	err = client.Assign(ctx, info.ID, info.Role)
	if err != nil {
		return errors.Wrap(err, "Failed to assign role")
	}

	gateway.info = info

	// Unlock regular access to our cluster database.
	err = transactor(func(tx *db.ClusterTx) error {
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "Cluster database initialization failed")
	}

	// Generate partial heartbeat request containing just a raft node list.
	notifyNodesUpdate(nodes, info.ID, gateway.networkCert, gateway.serverCert())

	return nil
}

// Leave a cluster.
//
// If the force flag is true, the node will leave even if it still has
// containers and images.
//
// The node will only leave the raft cluster, and won't be removed from the
// database. That's done by Purge().
//
// Upon success, return the address of the leaving node.
//
// This function must be called by the cluster leader.
func Leave(state *state.State, gateway *Gateway, name string, force bool) (string, error) {
	logger.Debugf("Make node %s leave the cluster", name)

	// Check if the node can be deleted and track its address.
	var address string
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node (if it doesn't exists an error is returned).
		node, err := tx.GetNodeByName(name)
		if err != nil {
			return err
		}

		// Check that the node is eligeable for leaving.
		if !force {
			err := membershipCheckClusterStateForLeave(tx, node.ID)
			if err != nil {
				return err
			}
		}

		address = node.Address
		return nil
	})
	if err != nil {
		return "", err
	}

	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return "", err
	}
	var info *db.RaftNode // Raft node to remove, if any.
	for i, node := range nodes {
		if node.Address == address {
			info = &nodes[i]
			break
		}
	}

	if info == nil {
		// The node was not part of the raft cluster, nothing left to
		// do.
		return address, nil
	}

	// Get the address of another database node,
	logger.Info(
		"Remove node from dqlite raft cluster",
		log15.Ctx{"id": info.ID, "address": info.Address})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := gateway.getClient()
	if err != nil {
		return "", errors.Wrap(err, "Failed to connect to cluster leader")
	}
	defer client.Close()
	err = client.Remove(ctx, info.ID)
	if err != nil {
		return "", errors.Wrap(err, "Failed to leave the cluster")
	}

	return address, nil
}

// Handover looks for a non-voter member that can be promoted to replace a the
// member with the given address, which is shutting down. It returns the
// address of such member along with an updated list of nodes, with the ne role
// set.
//
// It should be called only by the current leader.
func Handover(state *state.State, gateway *Gateway, address string) (string, []db.RaftNode, error) {
	nodes, err := gateway.currentRaftNodes()
	if err != nil {
		return "", nil, errors.Wrap(err, "Get current raft nodes")
	}

	var nodeID uint64
	for _, node := range nodes {
		if node.Address == address {
			nodeID = node.ID
		}

	}
	if nodeID == 0 {
		return "", nil, errors.Wrapf(err, "No dqlite node has address %s", address)
	}

	roles, err := newRolesChanges(state, gateway, nodes)
	if err != nil {
		return "", nil, err
	}

	role, candidates := roles.Handover(nodeID)
	if role == -1 {
		return "", nil, nil
	}

	for i, node := range nodes {
		if node.Address == candidates[0].Address {
			nodes[i].Role = role
			return node.Address, nodes, nil
		}
	}

	return "", nil, nil
}

// Build an app.RolesChanges object feeded with the current cluster state.
func newRolesChanges(state *state.State, gateway *Gateway, nodes []db.RaftNode) (*app.RolesChanges, error) {
	var maxVoters int
	var maxStandBy int
	var domains map[string]uint64

	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		config, err := ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "Load cluster configuration")
		}
		maxVoters = int(config.MaxVoters())
		maxStandBy = int(config.MaxStandBy())

		domains, err = tx.GetNodesFailureDomains()
		if err != nil {
			return errors.Wrap(err, "Load failure domains")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	cluster := map[client.NodeInfo]*client.NodeMetadata{}

	for _, node := range nodes {
		if HasConnectivity(gateway.networkCert, gateway.serverCert(), node.Address) {
			cluster[node] = &client.NodeMetadata{
				FailureDomain: domains[node.Address],
			}
		} else {
			cluster[node] = nil
		}

	}

	roles := &app.RolesChanges{
		Config: app.RolesConfig{
			Voters:   maxVoters,
			StandBys: maxStandBy,
		},
		State: cluster,
	}

	return roles, nil
}

// Purge removes a node entirely from the cluster database.
func Purge(cluster *db.Cluster, name string) error {
	logger.Debugf("Remove node %s from the database", name)

	return cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get the node (if it doesn't exists an error is returned).
		node, err := tx.GetNodeByName(name)
		if err != nil {
			return errors.Wrapf(err, "Failed to get member %q", name)
		}

		err = tx.ClearNode(node.ID)
		if err != nil {
			return errors.Wrapf(err, "Failed to clear member %q", name)
		}

		err = tx.RemoveNode(node.ID)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove member %q", name)
		}

		certType := db.CertificateTypeServer
		filter := db.CertificateFilter{Name: name, Type: &certType}
		err = tx.DeleteCertificates(filter)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove member %q certificate from trust store", name)
		}

		return nil
	})
}

// Count is a convenience for checking the current number of nodes in the
// cluster.
func Count(state *state.State) (int, error) {
	var count int
	err := state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		count, err = tx.GetNodesCount()
		return err
	})
	return count, err
}

// Enabled is a convenience that returns true if clustering is enabled on this
// node.
func Enabled(node *db.Node) (bool, error) {
	enabled := false
	err := node.Transaction(func(tx *db.NodeTx) error {
		addresses, err := tx.GetRaftNodeAddresses()
		if err != nil {
			return err
		}
		enabled = len(addresses) > 0
		return nil
	})
	return enabled, err
}

// Check that node-related preconditions are met for bootstrapping or joining a
// cluster.
func membershipCheckNodeStateForBootstrapOrJoin(tx *db.NodeTx, address string) error {
	nodes, err := tx.GetRaftNodes()
	if err != nil {
		return errors.Wrap(err, "failed to fetch current raft nodes")
	}

	hasClusterAddress := address != ""
	hasRaftNodes := len(nodes) > 0

	// Ensure that we're not in an inconsistent situation, where no cluster address is set, but still there
	// are entries in the raft_nodes table.
	if !hasClusterAddress && hasRaftNodes {
		return fmt.Errorf("inconsistent state: found leftover entries in raft_nodes")
	}

	if !hasClusterAddress {
		return fmt.Errorf("no cluster.https_address config is set on this node")
	}
	if hasRaftNodes {
		return fmt.Errorf("the node is already part of a cluster")
	}

	return nil
}

// Check that cluster-related preconditions are met for bootstrapping or
// joining a cluster.
func membershipCheckClusterStateForBootstrapOrJoin(tx *db.ClusterTx) error {
	nodes, err := tx.GetNodes()
	if err != nil {
		return errors.Wrap(err, "failed to fetch current cluster nodes")
	}
	if len(nodes) != 1 {
		return fmt.Errorf("inconsistent state: found leftover entries in nodes")
	}
	return nil
}

// Check that cluster-related preconditions are met for accepting a new node.
func membershipCheckClusterStateForAccept(tx *db.ClusterTx, name string, address string, schema int, api int) error {
	nodes, err := tx.GetNodes()
	if err != nil {
		return errors.Wrap(err, "Failed to fetch current cluster nodes")
	}

	if len(nodes) == 1 && nodes[0].Address == "0.0.0.0" {
		return fmt.Errorf("Clustering isn't enabled")
	}

	for _, node := range nodes {
		if node.Name == name {
			return fmt.Errorf("The cluster already has a member with name: %s", name)
		}

		if node.Address == address {
			return fmt.Errorf("The cluster already has a member with address: %s", address)
		}

		if node.Schema != schema {
			return fmt.Errorf("The joining server version doesn't (expected %s with DB schema %v)", version.Version, schema)
		}

		if node.APIExtensions != api {
			return fmt.Errorf("The joining server version doesn't (expected %s with API count %v)", version.Version, api)
		}
	}

	return nil
}

// Check that cluster-related preconditions are met for leaving a cluster.
func membershipCheckClusterStateForLeave(tx *db.ClusterTx, nodeID int64) error {
	// Check that it has no containers or images.
	message, err := tx.NodeIsEmpty(nodeID)
	if err != nil {
		return err
	}
	if message != "" {
		return fmt.Errorf(message)
	}

	// Check that it's not the last node.
	nodes, err := tx.GetNodes()
	if err != nil {
		return err
	}
	if len(nodes) == 1 {
		return fmt.Errorf("node is the only node in the cluster")
	}
	return nil
}

// Check that there is no left-over cluster certificate in the LXD var dir of
// this node.
func membershipCheckNoLeftoverClusterCert(dir string) error {
	// Ensure that there's no leftover cluster certificate.
	for _, basename := range []string{"cluster.crt", "cluster.key", "cluster.ca"} {
		if shared.PathExists(filepath.Join(dir, basename)) {
			return fmt.Errorf("inconsistent state: found leftover cluster certificate")
		}
	}
	return nil
}

// SchemaVersion holds the version of the cluster database schema.
var SchemaVersion = cluster.SchemaVersion
