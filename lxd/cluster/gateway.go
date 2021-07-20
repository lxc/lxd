package cluster

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"
	"unsafe"

	dqlite "github.com/canonical/go-dqlite"
	client "github.com/canonical/go-dqlite/client"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/lxd/warnings"
	"github.com/lxc/lxd/shared"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/pkg/errors"
)

// NewGateway creates a new Gateway for managing access to the dqlite cluster.
//
// When a new gateway is created, the node-level database is queried to check
// what kind of role this node plays and if it's exposed over the network. It
// will initialize internal data structures accordingly, for example starting a
// local dqlite server if this node is a database node.
//
// After creation, the Daemon is expected to expose whatever http handlers the
// HandlerFuncs method returns and to access the dqlite cluster using the
// dialer returned by the DialFunc method.
func NewGateway(db *db.Node, networkCert *shared.CertInfo, serverCert func() *shared.CertInfo, options ...Option) (*Gateway, error) {
	ctx, cancel := context.WithCancel(context.Background())

	o := newOptions()
	for _, option := range options {
		option(o)

	}

	gateway := &Gateway{
		db:          db,
		networkCert: networkCert,
		serverCert:  serverCert,
		options:     o,
		ctx:         ctx,
		cancel:      cancel,
		upgradeCh:   make(chan struct{}, 0),
		acceptCh:    make(chan net.Conn),
		store:       &dqliteNodeStore{},
	}

	err := gateway.init(false)
	if err != nil {
		return nil, err
	}

	return gateway, nil
}

// Gateway mediates access to the dqlite cluster using a gRPC SQL client, and
// possibly runs a dqlite replica on this LXD node (if we're configured to do
// so).
type Gateway struct {
	db          *db.Node
	networkCert *shared.CertInfo
	serverCert  func() *shared.CertInfo
	options     *options

	// The raft instance to use for creating the dqlite driver. It's nil if
	// this LXD node is not supposed to be part of the raft cluster.
	info *db.RaftNode

	// The gRPC server exposing the dqlite driver created by this
	// gateway. It's nil if this LXD node is not supposed to be part of the
	// raft cluster.
	server   *dqlite.Node
	acceptCh chan net.Conn
	stopCh   chan struct{}

	// A dialer that will connect to the dqlite server using a loopback
	// net.Conn. It's non-nil when clustering is not enabled on this LXD
	// node, and so we don't expose any dqlite or raft network endpoint,
	// but still we want to use dqlite as backend for the "cluster"
	// database, to minimize the difference between code paths in
	// clustering and non-clustering modes.
	memoryDial client.DialFunc

	// Used when shutting down the daemon to cancel any ongoing gRPC
	// dialing attempt.
	ctx    context.Context
	cancel context.CancelFunc

	// Used to unblock nodes that are waiting for other nodes to upgrade
	// their version.
	upgradeCh chan struct{}

	// Used to track whether we already triggered an upgrade because we
	// detected a peer with an higher version.
	upgradeTriggered bool

	// Used for the heartbeat handler
	Cluster                   *db.Cluster
	HeartbeatNodeHook         func(*APIHeartbeat)
	HeartbeatOfflineThreshold time.Duration
	heartbeatCancel           context.CancelFunc
	heartbeatCancelLock       sync.Mutex

	// NodeStore wrapper.
	store *dqliteNodeStore

	lock          sync.RWMutex
	heartbeatLock sync.Mutex

	// Abstract unix socket that the local dqlite task is listening to.
	bindAddress string

	// Keep track of skews
	timeSkew bool
}

// Current dqlite protocol version.
const dqliteVersion = 1

// Set the dqlite version header.
func setDqliteVersionHeader(request *http.Request) {
	request.Header.Set("X-Dqlite-Version", fmt.Sprintf("%d", dqliteVersion))
}

// HandlerFuncs returns the HTTP handlers that should be added to the REST API
// endpoint in order to handle database-related requests.
//
// There are two handlers, one for the /internal/raft endpoint and the other
// for /internal/db, which handle respectively raft and gRPC-SQL requests.
//
// These handlers might return 404, either because this LXD node is a
// non-clustered node not available over the network or because it is not a
// database node part of the dqlite cluster.
func (g *Gateway) HandlerFuncs(nodeRefreshTask func(*APIHeartbeat), trustedCerts func() map[db.CertificateType]map[string]x509.Certificate) map[string]http.HandlerFunc {
	database := func(w http.ResponseWriter, r *http.Request) {
		g.lock.RLock()
		defer g.lock.RUnlock()

		if !tlsCheckCert(r, g.networkCert, g.serverCert(), trustedCerts()) {
			http.Error(w, "403 invalid client certificate", http.StatusForbidden)
			return
		}

		// Compare the dqlite version of the connecting client
		// with our own one.
		versionHeader := r.Header.Get("X-Dqlite-Version")
		if versionHeader == "" {
			// No version header means an old pre dqlite 1.0 client.
			versionHeader = "0"
		}
		version, err := strconv.Atoi(versionHeader)
		if err != nil {
			http.Error(w, "400 invalid dqlite version", http.StatusBadRequest)
			return
		}
		if version != dqliteVersion {
			if version > dqliteVersion {
				if !g.upgradeTriggered {
					err = triggerUpdate()
					if err == nil {
						g.upgradeTriggered = true
					}
				}
				http.Error(w, "503 unsupported dqlite version", http.StatusServiceUnavailable)
			} else {
				http.Error(w, "426 dqlite version too old ", http.StatusUpgradeRequired)
			}
			return
		}

		// Handle heatbeats (these normally come from leader, but can come from joining nodes too).
		if r.Method == "PUT" {
			var heartbeatData APIHeartbeat
			err := json.NewDecoder(r.Body).Decode(&heartbeatData)
			if err != nil {
				logger.Error("Error decoding heartbeat body", log.Ctx{"err": err})
				http.Error(w, "400 invalid heartbeat payload", http.StatusBadRequest)
				return
			}

			// Look for time skews.
			now := time.Now().UTC()

			if heartbeatData.Time.Add(5*time.Second).Before(now) || heartbeatData.Time.Add(-5*time.Second).After(now) {
				if !g.timeSkew {
					logger.Warn("Time skew detected between leader and local", log.Ctx{"leaderTime": heartbeatData.Time, "localTime": now})

					if g.Cluster != nil {
						err := g.Cluster.UpsertWarningLocalNode("", -1, -1, db.WarningClusterTimeSkew, fmt.Sprintf("leaderTime: %s, localTime: %s", heartbeatData.Time, now))
						if err != nil {
							logger.Warn("Failed to create cluster time skew warning", log.Ctx{"err": err})
						}
					}
				}
				g.timeSkew = true
			} else {
				if g.timeSkew {
					logger.Warn("Time skew resolved")

					if g.Cluster != nil {
						err := warnings.ResolveWarningsByLocalNodeAndType(g.Cluster, db.WarningClusterTimeSkew)
						if err != nil {
							logger.Warn("Failed to resolve cluster time skew warning", log.Ctx{"err": err})
						}
					}

					g.timeSkew = false
				}
			}

			raftNodes := make([]db.RaftNode, 0)
			for _, node := range heartbeatData.Members {
				if node.RaftID > 0 {
					raftNodes = append(raftNodes, db.RaftNode{
						ID:      node.RaftID,
						Address: node.Address,
						Role:    db.RaftRole(node.RaftRole),
					})
				}
			}

			// Check we have been sent at least 1 raft node before wiping our set.
			if len(raftNodes) > 0 {
				// Accept Raft node updates from any node (joining nodes just send raft nodes heartbeat data).
				logger.Debugf("Replace current raft nodes with %+v", raftNodes)
				err = g.db.Transaction(func(tx *db.NodeTx) error {
					return tx.ReplaceRaftNodes(raftNodes)
				})
				if err != nil {
					logger.Error("Error updating raft members", log.Ctx{"err": err})
					http.Error(w, "500 failed to update raft nodes", http.StatusInternalServerError)
					return
				}

				// If there is an ongoing heartbeat round (and by implication this is the leader),
				// then this could be a problem because it could be broadcasting the stale member
				// state information which in turn could lead to incorrect decisions being made.
				// So calling heartbeatRestart will request any ongoing heartbeat round to cancel
				// itself prematurely and restart another one. If there is no ongoing heartbeat
				// round then this function call is a no-op.
				g.heartbeatRestart()
			} else {
				logger.Error("Empty raft member set received")
			}

			// Only perform node refresh task if we have received a full state list from leader.
			if !heartbeatData.FullStateList {
				logger.Debugf("Partial node list heartbeat received, skipping full update")
				return
			}

			// If node refresh task is specified, run it async.
			if nodeRefreshTask != nil {
				go nodeRefreshTask(&heartbeatData)
			}

			return
		}

		// Handle database upgrade notifications.
		if r.Method == "PATCH" {
			select {
			case g.upgradeCh <- struct{}{}:
			default:
			}
			return
		}

		// From here on we require that this node is part of the raft
		// cluster.
		if g.server == nil || g.memoryDial != nil {
			http.NotFound(w, r)
			return
		}

		// NOTE: this is kept for backward compatibility when upgrading
		// a cluster with version <= 4.2.
		//
		// Once all nodes are on >= 4.3 this code is effectively
		// unused.
		if r.Method == "HEAD" {
			// We can safely know about current leader only if we are a voter.
			if g.info.Role != db.RaftVoter {
				http.NotFound(w, r)
				return
			}
			client, err := g.getClient()
			if err != nil {
				http.Error(w, "500 failed to get dqlite client", http.StatusInternalServerError)
				return
			}
			defer client.Close()
			leader, err := client.Leader(context.Background())
			if err != nil {
				http.Error(w, "500 failed to get leader address", http.StatusInternalServerError)
				return
			}
			if leader == nil || leader.ID != g.info.ID {
				http.Error(w, "503 not leader", http.StatusServiceUnavailable)
				return
			}
			return
		}

		// Handle leader address requests.
		if r.Method == "GET" {
			leader, err := g.LeaderAddress()
			if err != nil {
				http.Error(w, "500 no elected leader", http.StatusInternalServerError)
				return
			}
			util.WriteJSON(w, map[string]string{"leader": leader}, false)
			return
		}

		if r.Header.Get("Upgrade") != "dqlite" {
			http.Error(w, "Missing or invalid upgrade header", http.StatusBadRequest)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "Webserver doesn't support hijacking", http.StatusInternalServerError)
			return
		}

		conn, _, err := hijacker.Hijack()
		if err != nil {
			message := errors.Wrap(err, "Failed to hijack connection").Error()
			http.Error(w, message, http.StatusInternalServerError)
			return
		}

		// Write the status line and upgrade header by hand since w.WriteHeader()
		// would fail after Hijack()
		data := []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: dqlite\r\n\r\n")
		if n, err := conn.Write(data); err != nil || n != len(data) {
			conn.Close()
			return
		}

		g.acceptCh <- conn
	}

	return map[string]http.HandlerFunc{
		databaseEndpoint: database,
	}
}

// Snapshot can be used to manually trigger a RAFT snapshot
func (g *Gateway) Snapshot() error {
	g.lock.RLock()
	defer g.lock.RUnlock()

	// TODO: implement support for forcing a snapshot in dqlite v1
	return fmt.Errorf("Not supported")
}

// WaitUpgradeNotification waits for a notification from another node that all
// nodes in the cluster should now have been upgraded and have matching schema
// and API versions.
func (g *Gateway) WaitUpgradeNotification() {
	select {
	case <-g.upgradeCh:
	case <-time.After(time.Minute):
	}
}

// IsDqliteNode returns true if this gateway is running a dqlite node.
func (g *Gateway) IsDqliteNode() bool {
	g.lock.RLock()
	defer g.lock.RUnlock()

	if g.info != nil {
		if g.server == nil {
			panic("gateway has node identity but no dqlite server")
		}
		return true
	}

	if g.server != nil {
		panic("gateway dqlite server but no node identity")
	}

	return true
}

// DialFunc returns a dial function that can be used to connect to one of the
// dqlite nodes.
func (g *Gateway) DialFunc() client.DialFunc {
	return func(ctx context.Context, address string) (net.Conn, error) {
		g.lock.RLock()
		defer g.lock.RUnlock()

		// Memory connection.
		if g.memoryDial != nil {
			return g.memoryDial(ctx, address)
		}

		conn, err := dqliteNetworkDial(ctx, address, g)
		if err != nil {
			return nil, err
		}

		// We successfully established a connection with the leader. Maybe the
		// leader is ourselves, and we were recently elected. In that case
		// trigger a full heartbeat now: it will be a no-op if we aren't
		// actually leaders.
		logger.Debug("Triggering an out of schedule hearbeat", log.Ctx{"address": address})
		go g.heartbeat(g.ctx, hearbeatInitial)

		return conn, nil
	}
}

// Dial function for establishing raft connections.
func (g *Gateway) raftDial() client.DialFunc {
	return func(ctx context.Context, address string) (net.Conn, error) {
		nodeAddress, err := g.nodeAddress(address)
		if err != nil {
			return nil, err
		}
		conn, err := dqliteNetworkDial(ctx, nodeAddress, g)
		if err != nil {
			return nil, err
		}

		listener, err := net.Listen("unix", "")
		if err != nil {
			return nil, errors.Wrap(err, "Failed to create unix listener")
		}

		goUnix, err := net.Dial("unix", listener.Addr().String())
		if err != nil {
			return nil, errors.Wrap(err, "Failed to connect to unix listener")
		}

		cUnix, err := listener.Accept()
		if err != nil {
			return nil, errors.Wrap(err, "Failed to connect to unix listener")
		}

		listener.Close()

		go dqliteProxy(g.stopCh, conn, goUnix)

		return cUnix, nil
	}
}

// Context returns a cancellation context to pass to dqlite.NewDriver as
// option.
//
// This context gets cancelled by Gateway.Kill() and at that point any
// connection failure won't be retried.
func (g *Gateway) Context() context.Context {
	return g.ctx
}

// NodeStore returns a dqlite server store that can be used to lookup the
// addresses of known database nodes.
func (g *Gateway) NodeStore() client.NodeStore {
	return g.store
}

// Kill is an API that the daemon calls before it actually shuts down and calls
// Shutdown(). It will abort any ongoing or new attempt to establish a SQL gRPC
// connection with the dialer (typically for running some pre-shutdown
// queries).
func (g *Gateway) Kill() {
	logger.Debug("Cancel ongoing or future gRPC connection attempts")
	g.cancel()
}

// TransferLeadership attempts to transfer leadership to another node.
func (g *Gateway) TransferLeadership() error {
	client, err := g.getClient()
	if err != nil {
		return err
	}
	defer client.Close()

	// Try to find a voter that is also online.
	servers, err := client.Cluster(context.Background())
	if err != nil {
		return err
	}
	var id uint64
	for _, server := range servers {
		if server.ID == g.info.ID || server.Role != db.RaftVoter {
			continue
		}
		address, err := g.nodeAddress(server.Address)
		if err != nil {
			return err
		}
		if !HasConnectivity(g.networkCert, g.serverCert(), address) {
			continue
		}
		id = server.ID
		break
	}

	if id == 0 {
		return fmt.Errorf("No online voter found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return client.Transfer(ctx, id)
}

// DemoteOfflineNode force demoting an offline node.
func (g *Gateway) DemoteOfflineNode(raftID uint64) error {
	cli, err := g.getClient()
	if err != nil {
		return errors.Wrap(err, "Connect to local dqlite node")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = cli.Assign(ctx, raftID, db.RaftSpare)
	if err != nil {
		return err
	}

	return nil
}

// Shutdown this gateway, stopping the gRPC server and possibly the raft factory.
func (g *Gateway) Shutdown() error {
	logger.Infof("Stop database gateway")

	if g.server != nil {
		if g.info.Role == db.RaftVoter {
			g.Sync()
		}

		g.server.Close()
		close(g.stopCh)

		// Unset the memory dial, since Shutdown() is also called for
		// switching between in-memory and network mode.
		g.lock.Lock()
		g.memoryDial = nil
		g.lock.Unlock()
	}

	return nil
}

// Sync dumps the content of the database to disk. This is useful for
// inspection purposes, and it's also needed by the activateifneeded command so
// it can inspect the database in order to decide whether to activate the
// daemon or not.
func (g *Gateway) Sync() {
	g.lock.RLock()
	defer g.lock.RUnlock()

	if g.server == nil || g.info.Role != db.RaftVoter {
		return
	}

	client, err := g.getClient()
	if err != nil {
		logger.Warnf("Failed to get client: %v", err)
		return
	}
	defer client.Close()

	files, err := client.Dump(context.Background(), "db.bin")
	if err != nil {
		// Just log a warning, since this is not fatal.
		logger.Warnf("Failed get database dump: %v", err)
		return
	}

	dir := filepath.Join(g.db.Dir(), "global")
	for _, file := range files {
		path := filepath.Join(dir, file.Name)
		err := ioutil.WriteFile(path, file.Data, 0600)
		if err != nil {
			logger.Warnf("Failed to dump database file %s: %v", file.Name, err)

		}
	}
}

func (g *Gateway) getClient() (*client.Client, error) {
	return client.New(context.Background(), g.bindAddress)
}

// Reset the gateway, shutting it down and starting against from scratch using
// the given certificate.
//
// This is used when disabling clustering on a node.
func (g *Gateway) Reset(networkCert *shared.CertInfo) error {
	err := g.Shutdown()
	if err != nil {
		return err
	}
	err = os.RemoveAll(filepath.Join(g.db.Dir(), "global"))
	if err != nil {
		return err
	}
	err = g.db.Transaction(func(tx *db.NodeTx) error {
		return tx.ReplaceRaftNodes(nil)
	})
	if err != nil {
		return err
	}
	g.networkCert = networkCert

	return g.init(false)
}

// ErrNodeIsNotClustered indicates the node is not clustered.
var ErrNodeIsNotClustered error = fmt.Errorf("Node is not clustered")

// LeaderAddress returns the address of the current raft leader.
func (g *Gateway) LeaderAddress() (string, error) {
	g.lock.RLock()
	defer g.lock.RUnlock()

	// If we aren't clustered, return an error.
	if g.memoryDial != nil {
		return "", ErrNodeIsNotClustered
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// If this is a voter node, return the address of the current leader, or
	// wait a bit until one is elected.
	if g.server != nil && g.info.Role == db.RaftVoter {
		for ctx.Err() == nil {
			client, err := g.getClient()
			if err != nil {
				return "", errors.Wrap(err, "Failed to get dqlite client")
			}
			defer client.Close()
			leader, err := client.Leader(context.Background())
			if err != nil {
				return "", errors.Wrap(err, "Failed to get leader address")
			}
			if leader != nil {
				return leader.Address, nil
			}
			time.Sleep(time.Second)
		}
		return "", ctx.Err()
	}

	// If this isn't a raft node, contact a raft node and ask for the
	// address of the current leader.
	config, err := tlsClientConfig(g.networkCert, g.serverCert())
	if err != nil {
		return "", err
	}
	addresses := []string{}
	err = g.db.Transaction(func(tx *db.NodeTx) error {
		nodes, err := tx.GetRaftNodes()
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if node.Role != db.RaftVoter {
				continue
			}
			addresses = append(addresses, node.Address)
		}
		return nil
	})
	if err != nil {
		return "", errors.Wrap(err, "Failed to fetch raft nodes addresses")
	}

	if len(addresses) == 0 {
		// This should never happen because the raft_nodes table should
		// be never empty for a clustered node, but check it for good
		// measure.
		return "", fmt.Errorf("No raft node known")
	}

	transport, cleanup := tlsTransport(config)
	defer cleanup()

	for _, address := range addresses {
		url := fmt.Sprintf("https://%s%s", address, databaseEndpoint)
		request, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return "", err
		}
		setDqliteVersionHeader(request)
		request = request.WithContext(ctx)
		client := &http.Client{Transport: transport}
		response, err := client.Do(request)
		if err != nil {
			logger.Debugf("Failed to fetch leader address from %s", address)
			continue
		}
		if response.StatusCode != http.StatusOK {
			logger.Debugf("Request for leader address from %s failed", address)
			continue
		}

		info := map[string]string{}
		err = json.NewDecoder(response.Body).Decode(&info)
		if err != nil {
			logger.Debugf("Failed to parse leader address from %s", address)
			continue
		}

		leader := info["leader"]
		if leader == "" {
			logger.Debugf("Raft node %s returned no leader address", address)
			continue
		}
		return leader, nil
	}

	return "", fmt.Errorf("RAFT cluster is unavailable")
}

// NetworkUpdateCert sets a new network certificate for the gateway
// Use with Endpoints.NetworkUpdateCert() to fully update the API endpoint
func (g *Gateway) NetworkUpdateCert(cert *shared.CertInfo) {
	g.lock.Lock()
	defer g.lock.Unlock()

	g.networkCert = cert
}

// Initialize the gateway, creating a new raft factory and gRPC server (if this
// node is a database node), and a gRPC dialer.
// @bootstrap should only be true when turning a non-clustered LXD instance into
// the first (and leader) node of a new LXD cluster.
func (g *Gateway) init(bootstrap bool) error {
	logger.Debugf("Initializing database gateway")
	g.stopCh = make(chan struct{}, 0)

	info, err := loadInfo(g.db, g.networkCert)
	if err != nil {
		return errors.Wrap(err, "Failed to create raft factory")
	}

	dir := filepath.Join(g.db.Dir(), "global")
	if shared.PathExists(filepath.Join(dir, "logs.db")) {
		err := shared.DirCopy(dir, dir+".bak")
		if err != nil {
			return errors.Wrap(err, "Failed to backup global database")
		}
		err = MigrateToDqlite10(dir)
		if err != nil {
			return errors.Wrap(err, "Failed to migrate to dqlite 1.0")
		}
		os.Remove(filepath.Join(dir, "logs.db"))
		os.RemoveAll(filepath.Join(dir, "snapshots"))
	}

	// If the resulting raft instance is not nil, it means that this node
	// should serve as database node, so create a dqlite driver possibly
	// exposing it over the network.
	if info != nil {
		// Use the autobind feature of abstract unix sockets to get a
		// random unused address.
		listener, err := net.Listen("unix", "")
		if err != nil {
			return errors.Wrap(err, "Failed to autobind unix socket")
		}
		g.bindAddress = listener.Addr().String()
		listener.Close()

		options := []dqlite.Option{
			dqlite.WithBindAddress(g.bindAddress),
		}

		if info.Address == "1" {
			if info.ID != 1 {
				panic("unexpected server ID")
			}
			g.memoryDial = dqliteMemoryDial(g.bindAddress)
			g.store.inMemory = client.NewInmemNodeStore()
			g.store.Set(context.Background(), []client.NodeInfo{*info})
		} else {
			go runDqliteProxy(g.stopCh, g.bindAddress, g.acceptCh)
			g.store.inMemory = nil
			options = append(options, dqlite.WithDialFunc(g.raftDial()))
		}

		server, err := dqlite.New(
			info.ID,
			info.Address,
			dir,
			options...,
		)
		if err != nil {
			return errors.Wrap(err, "Failed to create dqlite server")
		}

		// Force the correct configuration into the bootstrap node, this is needed
		// when the raft node already has log entries, in which case a regular
		// bootstrap fails, resulting in the node containing outdated configuration.
		if bootstrap {
			logger.Debugf("Bootstrap database gateway ID:%v Address:%v",
				info.ID, info.Address)
			cluster := []dqlite.NodeInfo{
				{ID: uint64(info.ID), Address: info.Address},
			}
			err = server.Recover(cluster)
			if err != nil {
				return errors.Wrap(err, "Failed to recover database state")
			}
		}

		err = server.Start()
		if err != nil {
			return errors.Wrap(err, "Failed to start dqlite server")
		}

		g.lock.Lock()
		g.server = server
		g.info = info
		g.lock.Unlock()
	} else {
		g.lock.Lock()
		g.server = nil
		g.info = nil
		g.store.inMemory = nil
		g.lock.Unlock()
	}

	g.lock.Lock()
	g.store.onDisk = client.NewNodeStore(
		g.db.DB(), "main", "raft_nodes", "address")
	g.lock.Unlock()

	return nil
}

// Wait for the raft node to become leader. Should only be used by Bootstrap,
// since we know that we'll self elect.
func (g *Gateway) waitLeadership() error {
	n := 80
	sleep := 250 * time.Millisecond
	for i := 0; i < n; i++ {
		g.lock.RLock()
		isLeader, err := g.isLeader()
		if err != nil {
			g.lock.RUnlock()
			return err
		}
		if isLeader {
			g.lock.RUnlock()
			return nil
		}
		g.lock.RUnlock()

		time.Sleep(sleep)
	}
	return fmt.Errorf("RAFT node did not self-elect within %s", time.Duration(n)*sleep)
}

func (g *Gateway) isLeader() (bool, error) {
	if g.server == nil || g.info.Role != db.RaftVoter {
		return false, nil
	}
	client, err := g.getClient()
	if err != nil {
		return false, errors.Wrap(err, "Failed to get dqlite client")
	}
	defer client.Close()
	leader, err := client.Leader(context.Background())
	if err != nil {
		return false, errors.Wrap(err, "Failed to get leader address")
	}
	return leader != nil && leader.ID == g.info.ID, nil
}

// ErrNotLeader signals that a node not the leader.
var ErrNotLeader = fmt.Errorf("Not leader")

// Return information about the LXD nodes that a currently part of the raft
// cluster, as configured in the raft log. It returns an error if this node is
// not the leader.
func (g *Gateway) currentRaftNodes() ([]db.RaftNode, error) {
	g.lock.RLock()
	defer g.lock.RUnlock()

	if g.info == nil || g.info.Role != db.RaftVoter {
		return nil, ErrNotLeader
	}

	isLeader, err := g.isLeader()
	if err != nil {
		return nil, err
	}
	if !isLeader {
		return nil, ErrNotLeader
	}
	client, err := g.getClient()
	if err != nil {
		return nil, err
	}
	defer client.Close()

	servers, err := client.Cluster(context.Background())
	if err != nil {
		return nil, err
	}
	for i, server := range servers {
		address, err := g.nodeAddress(server.Address)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to fetch raft server address")
		}
		servers[i].Address = address
	}
	return servers, nil
}

// Translate a raft address to a node address. They are always the same except
// for the bootstrap node, which has address "1".
func (g *Gateway) nodeAddress(raftAddress string) (string, error) {
	if raftAddress != "1" && raftAddress != "0" {
		return raftAddress, nil
	}
	var address string
	err := g.db.Transaction(func(tx *db.NodeTx) error {
		var err error
		address, err = tx.GetRaftNodeAddress(1)
		if err != nil {
			if err != db.ErrNoSuchObject {
				return errors.Wrap(err, "Failed to fetch raft server address")
			}
			// Use the initial address as fallback. This is an edge
			// case that happens when listing members on a
			// non-clustered node.
			address = raftAddress
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return address, nil
}

func dqliteNetworkDial(ctx context.Context, addr string, g *Gateway) (net.Conn, error) {
	config, err := tlsClientConfig(g.networkCert, g.serverCert())
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("https://%s%s", addr, databaseEndpoint)

	// Establish the connection
	request := &http.Request{
		Method:     "POST",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       addr,
	}
	request.URL, err = url.Parse(path)
	if err != nil {
		return nil, err
	}

	request.Header.Set("Upgrade", "dqlite")
	setDqliteVersionHeader(request)
	request = request.WithContext(ctx)

	deadline, _ := ctx.Deadline()
	dialer := &net.Dialer{Timeout: time.Until(deadline)}

	conn, err := tls.DialWithDialer(dialer, "tcp", addr, config)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to connect to HTTP endpoint")
	}

	err = request.Write(conn)
	if err != nil {
		return nil, errors.Wrap(err, "Sending HTTP request failed")
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to read response")
	}

	// If the remote server has detected that we are out of date, let's
	// trigger an upgrade.
	if response.StatusCode == http.StatusUpgradeRequired {
		g.lock.Lock()
		defer g.lock.Unlock()
		if !g.upgradeTriggered {
			err = triggerUpdate()
			if err == nil {
				g.upgradeTriggered = true
			}
		}
		return nil, fmt.Errorf("Upgrade needed")
	}

	if response.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("Dialing failed: expected status code 101 got %d", response.StatusCode)
	}
	if response.Header.Get("Upgrade") != "dqlite" {
		return nil, fmt.Errorf("Missing or unexpected Upgrade header in response")
	}

	return conn, nil
}

// Create a dial function that connects to the local dqlite.
func dqliteMemoryDial(bindAddress string) client.DialFunc {
	return func(ctx context.Context, address string) (net.Conn, error) {
		return net.Dial("unix", bindAddress)
	}
}

// The LXD API endpoint path that gets routed to a dqlite server handler for
// performing SQL queries against the dqlite server running on this node.
const databaseEndpoint = "/internal/database"

// DqliteLog redirects dqlite's logs to our own logger
func DqliteLog(l client.LogLevel, format string, a ...interface{}) {
	format = fmt.Sprintf("Dqlite: %s", format)
	switch l {
	case client.LogDebug:
		logger.Debugf(format, a...)
	case client.LogInfo:
		logger.Debugf(format, a...)
	case client.LogWarn:
		logger.Warnf(format, a...)
	case client.LogError:
		logger.Errorf(format, a...)
	}
}

// Copy incoming TLS streams from upgraded HTTPS connections into Unix sockets
// connected to the dqlite task.
func runDqliteProxy(stopCh chan struct{}, bindAddress string, acceptCh chan net.Conn) {
	for {
		remote := <-acceptCh
		local, err := net.Dial("unix", bindAddress)
		if err != nil {
			continue
		}

		go dqliteProxy(stopCh, remote, local)
	}
}

// Copies data between a remote TLS network connection and a local unix socket.
func dqliteProxy(stopCh chan struct{}, remote net.Conn, local net.Conn) {
	// Go doesn't currently expose the underlying TCP connection of a TLS
	// connection, but we need it in order to gracefully stop proxying with
	// ReadClose(). We use some reflect/unsafe magic to extract the
	// private remote.conn field, which is indeed the underlying TCP
	// connection.
	field := reflect.ValueOf(remote.(*tls.Conn)).Elem().FieldByName("conn")
	field = reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	tcp := field.Interface().(*net.TCPConn)

	remoteToLocal := make(chan error, 0)
	localToRemote := make(chan error, 0)

	// Start copying data back and forth until either the client or the
	// server get closed or hit an error.
	go func() {
		_, err := io.Copy(local, remote)
		remoteToLocal <- err
	}()

	go func() {
		_, err := io.Copy(remote, local)
		localToRemote <- err
	}()

	errs := make([]error, 2)

	select {
	case <-stopCh:
		// Force closing, ignore errors.
		remote.Close()
		local.Close()
		<-remoteToLocal
		<-localToRemote
	case err := <-remoteToLocal:
		if err != nil {
			errs[0] = fmt.Errorf("remote -> local: %v", err)
		}
		local.(*net.UnixConn).CloseRead()
		if err := <-localToRemote; err != nil {
			errs[1] = fmt.Errorf("local -> remote: %v", err)
		}
		remote.Close()
		local.Close()
	case err := <-localToRemote:
		if err != nil {
			errs[0] = fmt.Errorf("local -> remote: %v", err)
		}
		tcp.CloseRead()
		if err := <-remoteToLocal; err != nil {
			errs[1] = fmt.Errorf("remote -> local: %v", err)
		}
		local.Close()

	}

	if errs[0] != nil || errs[1] != nil {
		err := dqliteProxyError{first: errs[0], second: errs[1]}
		logger.Warnf("Dqlite proxy: %v", err)
	}
}

type dqliteProxyError struct {
	first  error
	second error
}

func (e dqliteProxyError) Error() string {
	msg := ""
	if e.first != nil {
		msg += "first: " + e.first.Error()
	}
	if e.second != nil {
		if e.first != nil {
			msg += " "
		}
		msg += "second: " + e.second.Error()
	}
	return msg
}

// Conditionally uses the in-memory or the on-disk server store.
type dqliteNodeStore struct {
	inMemory client.NodeStore
	onDisk   client.NodeStore
}

func (s *dqliteNodeStore) Get(ctx context.Context) ([]client.NodeInfo, error) {
	if s.inMemory != nil {
		return s.inMemory.Get(ctx)
	}
	return s.onDisk.Get(ctx)
}

func (s *dqliteNodeStore) Set(ctx context.Context, servers []client.NodeInfo) error {
	if s.inMemory != nil {
		return s.inMemory.Set(ctx, servers)
	}
	return s.onDisk.Set(ctx, servers)
}
