//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/canonical/go-dqlite/driver"
	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/db/node"
	"github.com/lxc/lxd/lxd/db/query"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"
)

// Node mediates access to LXD's data stored in the node-local SQLite database.
type Node struct {
	db  *sql.DB // Handle to the node-local SQLite database file.
	dir string  // Reference to the directory where the database file lives.
}

// OpenNode creates a new Node object.
//
// The fresh hook parameter is used by the daemon to mark all known patch names
// as applied when a brand new database is created.
//
// The legacyPatches parameter is used as a mean to apply the legacy V10, V11,
// V15, V29 and V30 non-db updates during the database upgrade sequence, to
// avoid any change in semantics wrt the old logic (see PR #3322).
//
// Return the newly created Node object, and a Dump of the pre-clustering data
// if we've migrating to a cluster-aware version.
func OpenNode(dir string, fresh func(*Node) error, legacyPatches map[int]*LegacyPatch) (*Node, *Dump, error) {
	// When updating the node database schema we'll detect if we're
	// transitioning to the dqlite-based database and dump all the data
	// before purging the schema. This data will be then imported by the
	// daemon into the dqlite database.
	var dump *Dump

	db, err := node.Open(dir)
	if err != nil {
		return nil, nil, err
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	legacyHook := legacyPatchHook(legacyPatches)
	hook := func(version int, tx *sql.Tx) error {
		if version == node.UpdateFromPreClustering {
			logger.Debug("Loading pre-clustering sqlite data")
			var err error
			dump, err = LoadPreClusteringData(tx)
			if err != nil {
				return err
			}
		}
		return legacyHook(version, tx)
	}
	initial, err := node.EnsureSchema(db, dir, hook)
	if err != nil {
		return nil, nil, err
	}

	node := &Node{
		db:  db,
		dir: dir,
	}

	if initial == 0 {
		if fresh != nil {
			err := fresh(node)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	return node, dump, nil
}

// ForLegacyPatches is a aid for the hack in initializeDbObject, which sets
// the db-related Deamon attributes upfront, to be backward compatible with the
// legacy patches that need to interact with the database.
func ForLegacyPatches(db *sql.DB) *Node {
	return &Node{db: db}
}

// DB returns the low level database handle to the node-local SQLite
// database.
//
// FIXME: this is used for compatibility with some legacy code, and should be
//        dropped once there are no call sites left.
func (n *Node) DB() *sql.DB {
	return n.db
}

// Dir returns the directory of the underlying SQLite database file.
func (n *Node) Dir() string {
	return n.dir
}

// Transaction creates a new NodeTx object and transactionally executes the
// node-level database interactions invoked by the given function. If the
// function returns no error, all database changes are committed to the
// node-level database, otherwise they are rolled back.
func (n *Node) Transaction(f func(*NodeTx) error) error {
	nodeTx := &NodeTx{}
	return query.Transaction(n.db, func(tx *sql.Tx) error {
		nodeTx.tx = tx
		return f(nodeTx)
	})
}

// Close the database facade.
func (n *Node) Close() error {
	return n.db.Close()
}

// Begin a new transaction against the local database. Legacy method.
func (n *Node) Begin() (*sql.Tx, error) {
	return begin(n.db)
}

// Cluster mediates access to LXD's data stored in the cluster dqlite database.
type Cluster struct {
	db        *sql.DB // Handle to the cluster dqlite database, gated behind gRPC SQL.
	nodeID    int64   // Node ID of this LXD instance.
	mu        sync.RWMutex
	stmts     map[int]*sql.Stmt // Prepared statements by code.
	closing   bool              // True when daemon is shutting down, prevents retries
	clusterMu sync.Mutex
}

// OpenCluster creates a new Cluster object for interacting with the dqlite
// database.
//
// - name: Basename of the database file holding the data. Typically "db.bin".
// - dialer: Function used to connect to the dqlite backend via gRPC SQL.
// - address: Network address of this node (or empty string).
// - dir: Base LXD database directory (e.g. /var/lib/lxd/database)
// - timeout: Give up trying to open the database after this amount of time.
// - dump: If not nil, a copy of 2.0 db data, for migrating to 3.0.
//
// The address and api parameters will be used to determine if the cluster
// database matches our version, and possibly trigger a schema update. If the
// schema update can't be performed right now, because some nodes are still
// behind, an Upgrading error is returned.
func OpenCluster(name string, store driver.NodeStore, address, dir string, timeout time.Duration, dump *Dump, options ...driver.Option) (*Cluster, error) {
	db, err := cluster.Open(name, store, options...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open database")
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Test that the cluster database is operational. We wait up to the
	// given timeout , in case there's no quorum of nodes online yet.
	timer := time.After(timeout)
	for i := 0; ; i++ {
		// Log initial attempts at debug level, but use warn
		// level after the 5'th attempt (about 10 seconds).
		// After the 15'th attempt (about 30 seconds), log
		// only one attempt every 5.
		logPriority := 1 // 0 is discard, 1 is Debug, 2 is Warn
		if i > 5 {
			logPriority = 2
			if i > 15 && !((i % 5) == 0) {
				logPriority = 0
			}
		}

		err = db.Ping()
		if err == nil {
			break
		}

		cause := errors.Cause(err)
		if cause != driver.ErrNoAvailableLeader {
			return nil, err
		}

		switch logPriority {
		case 1:
			logger.Debugf("Failed connecting to global database (attempt %d): %v", i, err)
		case 2:
			logger.Warnf("Failed connecting to global database (attempt %d): %v", i, err)
		}

		time.Sleep(2 * time.Second)
		select {
		case <-timer:
			return nil, fmt.Errorf("failed to connect to cluster database")
		default:
		}
	}

	// FIXME: https://github.com/canonical/dqlite/issues/163
	_, err = db.Exec("PRAGMA cache_size=-50000")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to set page cache size")
	}

	if dump != nil {
		logger.Infof("Migrating data from local to global database")
		err := query.Transaction(db, func(tx *sql.Tx) error {
			return importPreClusteringData(tx, dump)
		})
		if err != nil {
			// Restore the local sqlite3 backup and wipe the raft
			// directory, so users can fix problems and retry.
			path := filepath.Join(dir, "local.db")
			copyErr := shared.FileCopy(path+".bak", path)
			if copyErr != nil {
				// Ignore errors here, there's not much we can do
				logger.Errorf("Failed to restore local database: %v", copyErr)
			}
			rmErr := os.RemoveAll(filepath.Join(dir, "global"))
			if rmErr != nil {
				// Ignore errors here, there's not much we can do
				logger.Errorf("Failed to cleanup global database: %v", rmErr)
			}

			return nil, errors.Wrap(err, "Failed to migrate data to global database")
		}
	}

	nodesVersionsMatch, err := cluster.EnsureSchema(db, address, dir)
	if err != nil {
		return nil, errors.Wrap(err, "failed to ensure schema")
	}

	if !nodesVersionsMatch {
		cluster := &Cluster{
			db:    db,
			stmts: map[int]*sql.Stmt{},
		}

		return cluster, ErrSomeNodesAreBehind
	}

	stmts, err := cluster.PrepareStmts(db, false)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to prepare statements")
	}

	cluster := &Cluster{
		db:    db,
		stmts: stmts,
	}

	err = cluster.Transaction(func(tx *ClusterTx) error {
		// Figure out the ID of this node.
		nodes, err := tx.GetNodes()
		if err != nil {
			return errors.Wrap(err, "Failed to fetch nodes")
		}

		nodeID := int64(-1)
		if len(nodes) == 1 && nodes[0].Address == "0.0.0.0" {
			// We're not clustered
			nodeID = 1
		} else {
			for _, node := range nodes {
				if node.Address == address {
					nodeID = node.ID
					break
				}
			}
		}

		if nodeID < 0 {
			return fmt.Errorf("No node registered with address %s", address)
		}

		// Set the local node ID
		cluster.NodeID(nodeID)

		// Delete any operation tied to this node
		filter := OperationFilter{NodeID: &nodeID}
		err = tx.DeleteOperations(filter)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return cluster, err
}

// ErrSomeNodesAreBehind is returned by OpenCluster if some of the nodes in the
// cluster have a schema or API version that is less recent than this node.
var ErrSomeNodesAreBehind = fmt.Errorf("some nodes are behind this node's version")

// ForLocalInspection is a aid for the hack in initializeDbObject, which
// sets the db-related Deamon attributes upfront, to be backward compatible
// with the legacy patches that need to interact with the database.
func ForLocalInspection(db *sql.DB) *Cluster {
	return &Cluster{db: db}
}

// ForLocalInspectionWithPreparedStmts is the same as ForLocalInspection but it
// also prepares the statements used in auto-generated database code.
func ForLocalInspectionWithPreparedStmts(db *sql.DB) (*Cluster, error) {
	c := ForLocalInspection(db)

	stmts, err := cluster.PrepareStmts(c.db, true)
	if err != nil {
		return nil, errors.Wrap(err, "Prepare database statements")
	}

	c.stmts = stmts

	return c, nil
}

// Kill should be called upon shutdown, it will prevent retrying failed
// database queries.
func (c *Cluster) Kill() {
	c.clusterMu.Lock()
	c.closing = true
	c.clusterMu.Unlock()
}

// GetNodeID returns the current nodeID (0 if not set)
func (c *Cluster) GetNodeID() int64 {
	return c.nodeID
}

// Transaction creates a new ClusterTx object and transactionally executes the
// cluster database interactions invoked by the given function. If the function
// returns no error, all database changes are committed to the cluster database
// database, otherwise they are rolled back.
//
// If EnterExclusive has been called before, calling Transaction will block
// until ExitExclusive has been called as well to release the lock.
func (c *Cluster) Transaction(f func(*ClusterTx) error) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.transaction(f)
}

// EnterExclusive acquires a lock on the cluster db, so any successive call to
// Transaction will block until ExitExclusive has been called.
func (c *Cluster) EnterExclusive() error {
	logger.Debug("Acquiring exclusive lock on cluster db")

	ch := make(chan struct{})
	go func() {
		c.mu.Lock()
		ch <- struct{}{}
	}()

	timeout := 20 * time.Second
	select {
	case <-ch:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout (%s)", timeout)
	}
}

// ExitExclusive runs the given transaction and then releases the lock acquired
// with EnterExclusive.
func (c *Cluster) ExitExclusive(f func(*ClusterTx) error) error {
	logger.Debug("Releasing exclusive lock on cluster db")
	defer c.mu.Unlock()
	return c.transaction(f)
}

func (c *Cluster) transaction(f func(*ClusterTx) error) error {
	clusterTx := &ClusterTx{
		nodeID: c.nodeID,
		stmts:  c.stmts,
	}

	return c.retry(func() error {
		return query.Transaction(c.db, func(tx *sql.Tx) error {
			clusterTx.tx = tx
			return f(clusterTx)
		})
	})
}

func (c *Cluster) retry(f func() error) error {
	c.clusterMu.Lock()
	closing := c.closing
	c.clusterMu.Unlock()

	if closing {
		return f()
	}
	return query.Retry(f)
}

// NodeID sets the the node NodeID associated with this cluster instance. It's used for
// backward-compatibility of all db-related APIs that were written before
// clustering and don't accept a node NodeID, so in those cases we automatically
// use this value as implicit node NodeID.
func (c *Cluster) NodeID(id int64) {
	c.nodeID = id
}

// Close the database facade.
func (c *Cluster) Close() error {
	for _, stmt := range c.stmts {
		stmt.Close()
	}
	return c.db.Close()
}

// DB returns the low level database handle to the cluster database.
//
// FIXME: this is used for compatibility with some legacy code, and should be
//        dropped once there are no call sites left.
func (c *Cluster) DB() *sql.DB {
	return c.db
}

// Begin a new transaction against the cluster database.
//
// FIXME: legacy method.
func (c *Cluster) Begin() (*sql.Tx, error) {
	return begin(c.db)
}

func begin(db *sql.DB) (*sql.Tx, error) {
	for i := 0; i < 1000; i++ {
		tx, err := db.Begin()
		if err == nil {
			return tx, nil
		}
		if !query.IsRetriableError(err) {
			logger.Debugf("DbBegin: error %q", err)
			return nil, err
		}
		time.Sleep(30 * time.Millisecond)
	}

	logger.Debugf("DbBegin: DB still locked")
	logger.Debugf(logger.GetStack())
	return nil, fmt.Errorf("DB is locked")
}

// TxCommit commits the given transaction.
func TxCommit(tx *sql.Tx) error {
	err := tx.Commit()
	if err == nil || err == sql.ErrTxDone { // Ignore duplicate commits/rollbacks
		return nil
	}
	return err
}

func dbQueryRowScan(c *Cluster, q string, args []interface{}, outargs []interface{}) error {
	return c.retry(func() error {
		return query.Transaction(c.db, func(tx *sql.Tx) error {
			return tx.QueryRow(q, args...).Scan(outargs...)
		})
	})
}

func doDbQueryScan(c *Cluster, q string, args []interface{}, outargs []interface{}) ([][]interface{}, error) {
	result := [][]interface{}{}

	err := c.retry(func() error {
		return query.Transaction(c.db, func(tx *sql.Tx) error {
			rows, err := tx.Query(q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()

			for rows.Next() {
				ptrargs := make([]interface{}, len(outargs))
				for i := range outargs {
					switch t := outargs[i].(type) {
					case string:
						str := ""
						ptrargs[i] = &str
					case int:
						integer := 0
						ptrargs[i] = &integer
					case int64:
						integer := int64(0)
						ptrargs[i] = &integer
					case bool:
						boolean := bool(false)
						ptrargs[i] = &boolean
					default:
						return fmt.Errorf("Bad interface type: %s", t)
					}
				}
				err = rows.Scan(ptrargs...)
				if err != nil {
					return err
				}
				newargs := make([]interface{}, len(outargs))
				for i := range ptrargs {
					switch t := outargs[i].(type) {
					case string:
						newargs[i] = *ptrargs[i].(*string)
					case int:
						newargs[i] = *ptrargs[i].(*int)
					case int64:
						newargs[i] = *ptrargs[i].(*int64)
					case bool:
						newargs[i] = *ptrargs[i].(*bool)
					default:
						return fmt.Errorf("Bad interface type: %s", t)
					}
				}
				result = append(result, newargs)
			}
			err = rows.Err()
			if err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		return [][]interface{}{}, err
	}

	return result, nil

}

/*
 * . db a reference to a sql.DB instance
 * . q is the database query
 * . inargs is an array of interfaces containing the query arguments
 * . outfmt is an array of interfaces containing the right types of output
 *   arguments, i.e.
 *      var arg1 string
 *      var arg2 int
 *      outfmt := {}interface{}{arg1, arg2}
 *
 * The result will be an array (one per output row) of arrays (one per output argument)
 * of interfaces, containing pointers to the actual output arguments.
 */
func queryScan(c *Cluster, q string, inargs []interface{}, outfmt []interface{}) ([][]interface{}, error) {
	return doDbQueryScan(c, q, inargs, outfmt)
}

func exec(c *Cluster, q string, args ...interface{}) error {
	err := c.retry(func() error {
		return query.Transaction(c.db, func(tx *sql.Tx) error {
			_, err := tx.Exec(q, args...)
			return err
		})
	})
	return err
}
