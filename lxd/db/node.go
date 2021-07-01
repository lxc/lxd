//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/db/query"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/version"
)

// ClusterRole represents the role of a member in a cluster.
type ClusterRole string

// ClusterRoleDatabase represents the database role in a cluster.
const ClusterRoleDatabase = ClusterRole("database")

// ClusterRoleDatabaseStandBy represents the database stand-by role in a cluster.
const ClusterRoleDatabaseStandBy = ClusterRole("database-standby")

// ClusterRoles maps role ids into human-readable names.
//
// Note: the database role is currently stored directly in the raft
// configuration which acts as single source of truth for it. This map should
// only contain LXD-specific cluster roles.
var ClusterRoles = map[int]ClusterRole{}

// Numeric type codes identifying different cluster member states.
const (
	ClusterMemberStateCreated   = 0
	ClusterMemberStatePending   = 1
	ClusterMemberStateEvacuated = 2
)

// NodeInfo holds information about a single LXD instance in a cluster.
type NodeInfo struct {
	ID            int64     // Stable node identifier
	Name          string    // User-assigned name of the node
	Address       string    // Network address of the node
	Description   string    // Node description (optional)
	Schema        int       // Schema version of the LXD code running the node
	APIExtensions int       // Number of API extensions of the LXD code running on the node
	Heartbeat     time.Time // Timestamp of the last heartbeat
	Roles         []string  // List of cluster roles
	Architecture  int       // Node architecture
	State         int       // Node state
}

// IsOffline returns true if the last successful heartbeat time of the node is
// older than the given threshold.
func (n NodeInfo) IsOffline(threshold time.Duration) bool {
	return nodeIsOffline(threshold, n.Heartbeat)
}

// ToAPI returns a LXD API entry.
func (n NodeInfo) ToAPI(cluster *Cluster, node *Node) (*api.ClusterMember, error) {
	// Load some needed data.
	var err error
	var offlineThreshold time.Duration
	var maxVersion [2]int
	var failureDomain string

	// From cluster database.
	err = cluster.Transaction(func(tx *ClusterTx) error {
		// Get offline threshold.
		offlineThreshold, err = tx.GetNodeOfflineThreshold()
		if err != nil {
			return errors.Wrap(err, "Load offline threshold config")
		}

		// Get failure domains.
		nodesDomains, err := tx.GetNodesFailureDomains()
		if err != nil {
			return errors.Wrap(err, "Load nodes failure domains")
		}

		domainsNames, err := tx.GetFailureDomainsNames()
		if err != nil {
			return errors.Wrap(err, "Load failure domains names")
		}

		domainID := nodesDomains[n.Address]
		failureDomain = domainsNames[domainID]

		// Get the highest schema and API versions.
		maxVersion, err = tx.GetNodeMaxVersion()
		if err != nil {
			return errors.Wrap(err, "Get max version")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// From local database.
	var raftNode *RaftNode
	err = node.Transaction(func(tx *NodeTx) error {
		nodes, err := tx.GetRaftNodes()
		if err != nil {
			return errors.Wrap(err, "Load offline threshold config")
		}

		for _, node := range nodes {
			if node.Address != n.Address {
				continue
			}

			raftNode = &node
			break
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Fill in the struct.
	result := api.ClusterMember{}
	result.Description = n.Description
	result.ServerName = n.Name
	result.URL = fmt.Sprintf("https://%s", n.Address)
	result.Database = false
	result.Roles = n.Roles
	if raftNode != nil && raftNode.Role == RaftVoter {
		result.Roles = append(result.Roles, string(ClusterRoleDatabase))
		result.Database = true
	}
	if raftNode != nil && raftNode.Role == RaftStandBy {
		result.Roles = append(result.Roles, string(ClusterRoleDatabaseStandBy))
		result.Database = true
	}
	result.Architecture, err = osarch.ArchitectureName(n.Architecture)
	if err != nil {
		return nil, err
	}
	result.FailureDomain = failureDomain

	if n.IsOffline(offlineThreshold) {
		result.Status = "Offline"
		result.Message = fmt.Sprintf("No heartbeat for %s (%s)", time.Now().Sub(n.Heartbeat), n.Heartbeat)
	} else {
		// Check if up to date.
		n, err := util.CompareVersions(maxVersion, n.Version())
		if err != nil {
			return nil, err
		}

		if n == 1 {
			result.Status = "Blocked"
			result.Message = "Needs updating to newer version"
		} else {
			result.Status = "Online"
			result.Message = "Fully operational"
		}
	}

	return &result, nil
}

// Version returns the node's version, composed by its schema level and
// number of extensions.
func (n NodeInfo) Version() [2]int {
	return [2]int{n.Schema, n.APIExtensions}
}

// GetNodeByAddress returns the node with the given network address.
func (c *ClusterTx) GetNodeByAddress(address string) (NodeInfo, error) {
	null := NodeInfo{}
	nodes, err := c.nodes(false /* not pending */, "address=?", address)
	if err != nil {
		return null, err
	}
	switch len(nodes) {
	case 0:
		return null, ErrNoSuchObject
	case 1:
		return nodes[0], nil
	default:
		return null, fmt.Errorf("more than one node matches")
	}
}

// GetNodeMaxVersion returns the highest version possible on the cluster.
func (c *ClusterTx) GetNodeMaxVersion() ([2]int, error) {
	version := [2]int{}

	// Get the maximum DB schema.
	var maxSchema int
	row := c.tx.QueryRow("SELECT MAX(schema) FROM nodes")
	err := row.Scan(&maxSchema)
	if err != nil {
		return version, err
	}

	// Get the maximum API extension.
	var maxAPI int
	row = c.tx.QueryRow("SELECT MAX(api_extensions) FROM nodes")
	err = row.Scan(&maxAPI)
	if err != nil {
		return version, err
	}

	// Compute the combined version.
	version = [2]int{maxSchema, maxAPI}

	return version, nil
}

// GetNodeWithID returns the node with the given ID.
func (c *ClusterTx) GetNodeWithID(nodeID int) (NodeInfo, error) {
	null := NodeInfo{}
	nodes, err := c.nodes(false /* not pending */, "id=?", nodeID)
	if err != nil {
		return null, err
	}
	switch len(nodes) {
	case 0:
		return null, ErrNoSuchObject
	case 1:
		return nodes[0], nil
	default:
		return null, fmt.Errorf("more than one node matches")
	}
}

// GetPendingNodeByAddress returns the pending node with the given network address.
func (c *ClusterTx) GetPendingNodeByAddress(address string) (NodeInfo, error) {
	null := NodeInfo{}
	nodes, err := c.nodes(true /*pending */, "address=?", address)
	if err != nil {
		return null, err
	}
	switch len(nodes) {
	case 0:
		return null, ErrNoSuchObject
	case 1:
		return nodes[0], nil
	default:
		return null, fmt.Errorf("more than one node matches")
	}
}

// GetNodeByName returns the node with the given name.
func (c *ClusterTx) GetNodeByName(name string) (NodeInfo, error) {
	null := NodeInfo{}
	nodes, err := c.nodes(false /* not pending */, "name=?", name)
	if err != nil {
		return null, err
	}
	switch len(nodes) {
	case 0:
		return null, ErrNoSuchObject
	case 1:
		return nodes[0], nil
	default:
		return null, fmt.Errorf("more than one node matches")
	}
}

// GetLocalNodeName returns the name of the node this method is invoked on.
func (c *ClusterTx) GetLocalNodeName() (string, error) {
	stmt := "SELECT name FROM nodes WHERE id=?"
	names, err := query.SelectStrings(c.tx, stmt, c.nodeID)
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", nil
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("inconsistency: non-unique node ID")
	}
}

// GetLocalNodeAddress returns the address of the node this method is invoked on.
func (c *ClusterTx) GetLocalNodeAddress() (string, error) {
	stmt := "SELECT address FROM nodes WHERE id=?"
	addresses, err := query.SelectStrings(c.tx, stmt, c.nodeID)
	if err != nil {
		return "", err
	}
	switch len(addresses) {
	case 0:
		return "", nil
	case 1:
		return addresses[0], nil
	default:
		return "", fmt.Errorf("inconsistency: non-unique node ID")
	}
}

// NodeIsOutdated returns true if there's some cluster node having an API or
// schema version greater than the node this method is invoked on.
func (c *ClusterTx) NodeIsOutdated() (bool, error) {
	nodes, err := c.nodes(false /* not pending */, "")
	if err != nil {
		return false, errors.Wrap(err, "Failed to fetch nodes")
	}

	// Figure our own version.
	version := [2]int{}
	for _, node := range nodes {
		if node.ID == c.nodeID {
			version = node.Version()
		}
	}
	if version[0] == 0 || version[1] == 0 {
		return false, fmt.Errorf("Inconsistency: local node not found")
	}

	// Check if any of the other nodes is greater than us.
	for _, node := range nodes {
		if node.ID == c.nodeID {
			continue
		}
		n, err := util.CompareVersions(node.Version(), version)
		if err != nil {
			errors.Wrapf(err, "Failed to compare with version of node %s", node.Name)
		}

		if n == 1 {
			// The other node's version is greater than ours.
			return true, nil
		}
	}

	return false, nil
}

// GetNodes returns all LXD nodes part of the cluster.
//
// If this LXD instance is not clustered, a list with a single node whose
// address is 0.0.0.0 is returned.
func (c *ClusterTx) GetNodes() ([]NodeInfo, error) {
	return c.nodes(false /* not pending */, "")
}

// GetNodesCount returns the number of nodes in the LXD cluster.
//
// Since there's always at least one node row, even when not-clustered, the
// return value is greater than zero
func (c *ClusterTx) GetNodesCount() (int, error) {
	count, err := query.Count(c.tx, "nodes", "")
	if err != nil {
		return 0, errors.Wrap(err, "failed to count existing nodes")
	}
	return count, nil
}

// RenameNode changes the name of an existing node.
//
// Return an error if a node with the same name already exists.
func (c *ClusterTx) RenameNode(old, new string) error {
	count, err := query.Count(c.tx, "nodes", "name=?", new)
	if err != nil {
		return errors.Wrap(err, "failed to check existing nodes")
	}
	if count != 0 {
		return ErrAlreadyDefined
	}
	stmt := `UPDATE nodes SET name=? WHERE name=?`
	result, err := c.tx.Exec(stmt, new, old)
	if err != nil {
		return errors.Wrap(err, "failed to update node name")
	}
	n, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "failed to get rows count")
	}
	if n != 1 {
		return fmt.Errorf("expected to update one row, not %d", n)
	}
	return nil
}

// SetDescription changes the description of the given node.
func (c *ClusterTx) SetDescription(id int64, description string) error {
	stmt := `UPDATE nodes SET description=? WHERE id=?`
	result, err := c.tx.Exec(stmt, description, id)
	if err != nil {
		return errors.Wrap(err, "Failed to update node name")
	}

	n, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "Failed to get rows count")
	}

	if n != 1 {
		return fmt.Errorf("Expected to update one row, not %d", n)
	}

	return nil
}

// Nodes returns all LXD nodes part of the cluster.
func (c *ClusterTx) nodes(pending bool, where string, args ...interface{}) ([]NodeInfo, error) {
	// Get node roles
	sql := "SELECT node_id, role FROM nodes_roles"

	nodeRoles := map[int64][]string{}
	rows, err := c.tx.Query(sql)
	if err != nil {
		if err.Error() != "no such table: nodes_roles" {
			return nil, err
		}
	} else {
		// Don't fail on a missing table, we need to handle updates
		defer rows.Close()

		for i := 0; rows.Next(); i++ {
			var nodeID int64
			var role int
			err := rows.Scan(&nodeID, &role)
			if err != nil {
				return nil, err
			}

			if nodeRoles[nodeID] == nil {
				nodeRoles[nodeID] = []string{}
			}

			roleName := string(ClusterRoles[role])

			nodeRoles[nodeID] = append(nodeRoles[nodeID], roleName)
		}
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	// Process node entries
	nodes := []NodeInfo{}
	dest := func(i int) []interface{} {
		nodes = append(nodes, NodeInfo{})
		return []interface{}{
			&nodes[i].ID,
			&nodes[i].Name,
			&nodes[i].Address,
			&nodes[i].Description,
			&nodes[i].Schema,
			&nodes[i].APIExtensions,
			&nodes[i].Heartbeat,
			&nodes[i].Architecture,
			&nodes[i].State,
		}
	}
	if pending {
		args = append([]interface{}{ClusterMemberStatePending}, args...)
	} else {
		args = append([]interface{}{ClusterMemberStateCreated}, args...)
	}

	// Get the node entries
	sql = "SELECT id, name, address, description, schema, api_extensions, heartbeat, arch, state FROM nodes WHERE state=?"
	if where != "" {
		sql += fmt.Sprintf("AND %s ", where)
	}
	sql += "ORDER BY id"

	stmt, err := c.tx.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	err = query.SelectObjects(stmt, dest, args...)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to fetch nodes")
	}

	// Add the roles
	for i, node := range nodes {
		roles, ok := nodeRoles[node.ID]
		if ok {
			nodes[i].Roles = roles
		}
	}

	return nodes, nil
}

// CreateNode adds a node to the current list of LXD nodes that are part of the
// cluster. The node's architecture will be the architecture of the machine the
// method is being run on. It returns the ID of the newly inserted row.
func (c *ClusterTx) CreateNode(name string, address string) (int64, error) {
	arch, err := osarch.ArchitectureGetLocalID()
	if err != nil {
		return -1, err
	}
	return c.CreateNodeWithArch(name, address, arch)
}

// CreateNodeWithArch is the same as NodeAdd, but lets setting the node
// architecture explicitly.
func (c *ClusterTx) CreateNodeWithArch(name string, address string, arch int) (int64, error) {
	columns := []string{"name", "address", "schema", "api_extensions", "arch"}
	values := []interface{}{name, address, cluster.SchemaVersion, version.APIExtensionsCount(), arch}
	return query.UpsertObject(c.tx, "nodes", columns, values)
}

// SetNodePendingFlag toggles the pending flag for the node. A node is pending when
// it's been accepted in the cluster, but has not yet actually joined it.
func (c *ClusterTx) SetNodePendingFlag(id int64, pending bool) error {
	value := 0
	if pending {
		value = 1
	}
	result, err := c.tx.Exec("UPDATE nodes SET state=? WHERE id=?", value, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("query updated %d rows instead of 1", n)
	}
	return nil
}

// UpdateNode updates the name an address of a node.
func (c *ClusterTx) UpdateNode(id int64, name string, address string) error {
	result, err := c.tx.Exec("UPDATE nodes SET name=?, address=? WHERE id=?", name, address, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("query updated %d rows instead of 1", n)
	}
	return nil
}

// CreateNodeRole adds a role to the node.
func (c *ClusterTx) CreateNodeRole(id int64, role ClusterRole) error {
	// Translate role names to ids
	roleID := -1
	for k, v := range ClusterRoles {
		if v == role {
			roleID = k
			break
		}
	}

	if roleID < 0 {
		return fmt.Errorf("Invalid role: %v", role)
	}

	// Update the database record
	_, err := c.tx.Exec("INSERT INTO nodes_roles (node_id, role) VALUES (?, ?)", id, roleID)
	if err != nil {
		return err
	}

	return nil
}

// RemoveNodeRole removes a role from the node.
func (c *ClusterTx) RemoveNodeRole(id int64, role ClusterRole) error {
	// Translate role names to ids
	roleID := -1
	for k, v := range ClusterRoles {
		if v == role {
			roleID = k
			break
		}
	}

	if roleID < 0 {
		return fmt.Errorf("Invalid role: %v", role)
	}

	// Update the database record
	_, err := c.tx.Exec("DELETE FROM nodes_roles WHERE node_id=? AND role=?", id, roleID)
	if err != nil {
		return err
	}

	return nil
}

// UpdateNodeRoles changes the list of roles on a member.
func (c *ClusterTx) UpdateNodeRoles(id int64, roles []ClusterRole) error {
	getRoleID := func(role ClusterRole) (int, error) {
		for k, v := range ClusterRoles {
			if v == role {
				return k, nil
			}
		}

		return -1, fmt.Errorf("Invalid cluster role '%s'", role)
	}

	// Translate role names to ids
	roleIDs := []int{}
	for _, role := range roles {
		// Skip internal-only roles.
		if role == ClusterRoleDatabase {
			continue
		}

		roleID, err := getRoleID(role)
		if err != nil {
			return err
		}

		roleIDs = append(roleIDs, roleID)
	}

	// Update the database record
	_, err := c.tx.Exec("DELETE FROM nodes_roles WHERE node_id=?", id)
	if err != nil {
		return err
	}

	for _, roleID := range roleIDs {
		_, err := c.tx.Exec("INSERT INTO nodes_roles (node_id, role) VALUES (?, ?)", id, roleID)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateNodeFailureDomain changes the failure domain of a node.
func (c *ClusterTx) UpdateNodeFailureDomain(id int64, domain string) error {
	var domainID interface{}

	if domain == "" {
		return fmt.Errorf("Failure domain name can't be empty")
	}

	if domain == "default" {
		domainID = nil
	} else {
		row := c.tx.QueryRow("SELECT id FROM nodes_failure_domains WHERE name=?", domain)
		err := row.Scan(&domainID)
		if err != nil {
			if err != sql.ErrNoRows {
				return errors.Wrapf(err, "Load failure domain name")
			}
			result, err := c.tx.Exec("INSERT INTO nodes_failure_domains (name) VALUES (?)", domain)
			if err != nil {
				return errors.Wrapf(err, "Create new failure domain")
			}
			domainID, err = result.LastInsertId()
			if err != nil {
				return errors.Wrapf(err, "Get last inserted ID")
			}
		}
	}

	result, err := c.tx.Exec("UPDATE nodes SET failure_domain_id=? WHERE id=?", domainID, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("Query updated %d rows instead of 1", n)
	}

	return nil
}

// UpdateNodeStatus changes the state of a node.
func (c *ClusterTx) UpdateNodeStatus(id int64, state int) error {
	result, err := c.tx.Exec("UPDATE nodes SET state=? WHERE id=?", state, id)
	if err != nil {
		return err
	}

	n, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if n != 1 {
		return fmt.Errorf("Query updated %d rows instead of 1", n)
	}

	return nil
}

// GetNodeFailureDomain returns the failure domain associated with the node with the given ID.
func (c *ClusterTx) GetNodeFailureDomain(id int64) (string, error) {
	stmt := `
SELECT coalesce(nodes_failure_domains.name,'default')
  FROM nodes LEFT JOIN nodes_failure_domains ON nodes.failure_domain_id = nodes_failure_domains.id
 WHERE nodes.id=?
`
	var domain string

	err := c.tx.QueryRow(stmt, id).Scan(&domain)
	if err != nil {
		return "", err
	}
	return domain, nil
}

// GetNodesFailureDomains returns a map associating each node address with its
// failure domain code.
func (c *ClusterTx) GetNodesFailureDomains() (map[string]uint64, error) {
	stmt, err := c.tx.Prepare("SELECT address, coalesce(failure_domain_id, 0) FROM nodes")
	if err != nil {
		return nil, err
	}

	rows := []struct {
		Address         string
		FailureDomainID int64
	}{}

	dest := func(i int) []interface{} {
		rows = append(rows, struct {
			Address         string
			FailureDomainID int64
		}{})
		return []interface{}{&rows[len(rows)-1].Address, &rows[len(rows)-1].FailureDomainID}
	}

	err = query.SelectObjects(stmt, dest)
	if err != nil {
		return nil, err
	}

	domains := map[string]uint64{}

	for _, row := range rows {
		domains[row.Address] = uint64(row.FailureDomainID)
	}

	return domains, nil
}

// GetFailureDomainsNames return a map associating failure domain IDs to their
// names.
func (c *ClusterTx) GetFailureDomainsNames() (map[uint64]string, error) {
	stmt, err := c.tx.Prepare("SELECT id, name FROM nodes_failure_domains")
	if err != nil {
		return nil, err
	}

	rows := []struct {
		ID   int64
		Name string
	}{}

	dest := func(i int) []interface{} {
		rows = append(rows, struct {
			ID   int64
			Name string
		}{})
		return []interface{}{&rows[len(rows)-1].ID, &rows[len(rows)-1].Name}
	}

	err = query.SelectObjects(stmt, dest)
	if err != nil {
		return nil, err
	}

	domains := map[uint64]string{
		0: "default", // Default failure domain, when not set
	}

	for _, row := range rows {
		domains[uint64(row.ID)] = row.Name
	}

	return domains, nil
}

// RemoveNode removes the node with the given id.
func (c *ClusterTx) RemoveNode(id int64) error {
	result, err := c.tx.Exec("DELETE FROM nodes WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("query deleted %d rows instead of 1", n)
	}
	return nil
}

// SetNodeHeartbeat updates the heartbeat column of the node with the given address.
func (c *ClusterTx) SetNodeHeartbeat(address string, heartbeat time.Time) error {
	stmt := "UPDATE nodes SET heartbeat=? WHERE address=?"
	result, err := c.tx.Exec(stmt, heartbeat, address)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if n < 1 {
		return ErrNoSuchObject
	} else if n > 1 {
		return fmt.Errorf("Expected to update one row and not %d", n)
	}

	return nil
}

// NodeIsEmpty returns an empty string if the node with the given ID has no
// containers or images associated with it. Otherwise, it returns a message
// say what's left.
func (c *ClusterTx) NodeIsEmpty(id int64) (string, error) {
	// Check if the node has any instances.
	containers, err := query.SelectStrings(c.tx, "SELECT name FROM instances WHERE node_id=?", id)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to get instances for node %d", id)
	}
	if len(containers) > 0 {
		message := fmt.Sprintf(
			"Node still has the following containers: %s", strings.Join(containers, ", "))
		return message, nil
	}

	// Check if the node has any images available only in it.
	images := []struct {
		fingerprint string
		nodeID      int64
	}{}
	dest := func(i int) []interface{} {
		images = append(images, struct {
			fingerprint string
			nodeID      int64
		}{})
		return []interface{}{&images[i].fingerprint, &images[i].nodeID}

	}
	stmt, err := c.tx.Prepare(`
SELECT fingerprint, node_id FROM images JOIN images_nodes ON images.id=images_nodes.image_id`)
	if err != nil {
		return "", err
	}
	defer stmt.Close()
	err = query.SelectObjects(stmt, dest)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to get image list for node %d", id)
	}
	index := map[string][]int64{} // Map fingerprints to IDs of nodes
	for _, image := range images {
		index[image.fingerprint] = append(index[image.fingerprint], image.nodeID)
	}

	fingerprints := []string{}
	for fingerprint, ids := range index {
		if len(ids) > 1 {
			continue
		}
		if ids[0] == id {
			fingerprints = append(fingerprints, fingerprint)
		}
	}

	if len(fingerprints) > 0 {
		message := fmt.Sprintf(
			"Node still has the following images: %s", strings.Join(fingerprints, ", "))
		return message, nil
	}

	// Check if the node has any custom volumes.
	volumes, err := query.SelectStrings(
		c.tx, "SELECT storage_volumes.name FROM storage_volumes JOIN storage_pools ON storage_volumes.storage_pool_id=storage_pools.id WHERE storage_volumes.node_id=? AND storage_volumes.type=? AND storage_pools.driver NOT IN ('ceph', 'cephfs')",
		id, StoragePoolVolumeTypeCustom)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to get custom volumes for node %d", id)
	}
	if len(volumes) > 0 {
		message := fmt.Sprintf(
			"Node still has the following custom volumes: %s", strings.Join(volumes, ", "))
		return message, nil
	}

	return "", nil
}

// ClearNode removes any instance or image associated with this node.
func (c *ClusterTx) ClearNode(id int64) error {
	_, err := c.tx.Exec("DELETE FROM instances WHERE node_id=?", id)
	if err != nil {
		return err
	}

	// Get the IDs of the images this node is hosting.
	ids, err := query.SelectIntegers(c.tx, "SELECT image_id FROM images_nodes WHERE node_id=?", id)
	if err != nil {
		return err
	}

	// Delete the association
	_, err = c.tx.Exec("DELETE FROM images_nodes WHERE node_id=?", id)
	if err != nil {
		return err
	}

	// Delete the image as well if this was the only node with it.
	for _, id := range ids {
		count, err := query.Count(c.tx, "images_nodes", "image_id=?", id)
		if err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		_, err = c.tx.Exec("DELETE FROM images WHERE id=?", id)
		if err != nil {
			return err
		}
	}

	return nil
}

// GetNodeOfflineThreshold returns the amount of time that needs to elapse after
// which a series of unsuccessful heartbeat will make the node be considered
// offline.
func (c *ClusterTx) GetNodeOfflineThreshold() (time.Duration, error) {
	threshold := time.Duration(DefaultOfflineThreshold) * time.Second
	values, err := query.SelectStrings(
		c.tx, "SELECT value FROM config WHERE key='cluster.offline_threshold'")
	if err != nil {
		return -1, err
	}
	if len(values) > 0 {
		seconds, err := strconv.Atoi(values[0])
		if err != nil {
			return -1, err
		}
		threshold = time.Duration(seconds) * time.Second
	}
	return threshold, nil
}

// GetNodeWithLeastInstances returns the name of the non-offline node with with
// the least number of containers (either already created or being created with
// an operation). If archs is not empty, then return only nodes with an
// architecture in that list.
func (c *ClusterTx) GetNodeWithLeastInstances(archs []int, defaultArch int) (string, error) {
	threshold, err := c.GetNodeOfflineThreshold()
	if err != nil {
		return "", errors.Wrap(err, "failed to get offline threshold")
	}

	nodes, err := c.GetNodes()
	if err != nil {
		return "", errors.Wrap(err, "failed to get current nodes")
	}

	name := ""
	containers := -1
	isDefaultArchChosen := false
	for _, node := range nodes {
		if node.IsOffline(threshold) {
			continue
		}

		// Get personalities too.
		personalities, err := osarch.ArchitecturePersonalities(node.Architecture)
		if err != nil {
			return "", err
		}

		supported := []int{node.Architecture}
		supported = append(supported, personalities...)

		match := false
		isDefaultArch := false
		for _, entry := range supported {
			if shared.IntInSlice(entry, archs) {
				match = true
			}
			if entry == defaultArch {
				isDefaultArch = true
			}
		}
		if len(archs) > 0 && !match {
			continue
		}
		if !isDefaultArch && isDefaultArchChosen {
			continue
		}

		// Fetch the number of containers already created on this node.
		created, err := query.Count(c.tx, "instances", "node_id=?", node.ID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get instances count")
		}

		// Fetch the number of containers currently being created on this node.
		pending, err := query.Count(
			c.tx, "operations", "node_id=? AND type=?", node.ID, OperationInstanceCreate)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get pending instances count")
		}

		count := created + pending
		if containers == -1 || count < containers || (isDefaultArch == true && isDefaultArchChosen == false) {
			containers = count
			name = node.Name
			if isDefaultArch {
				isDefaultArchChosen = true
			}
		}
	}
	return name, nil
}

// SetNodeVersion updates the schema and API version of the node with the
// given id. This is used only in tests.
func (c *ClusterTx) SetNodeVersion(id int64, version [2]int) error {
	stmt := "UPDATE nodes SET schema=?, api_extensions=? WHERE id=?"

	result, err := c.tx.Exec(stmt, version[0], version[1], id)
	if err != nil {
		return errors.Wrap(err, "Failed to update nodes table")
	}

	n, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "Failed to get affected rows")
	}

	if n != 1 {
		return fmt.Errorf("Expected exactly one row to be updated")
	}

	return nil
}

func nodeIsOffline(threshold time.Duration, heartbeat time.Time) bool {
	return heartbeat.Before(time.Now().Add(-threshold))
}

// DefaultOfflineThreshold is the default value for the
// cluster.offline_threshold configuration key, expressed in seconds.
const DefaultOfflineThreshold = 20
