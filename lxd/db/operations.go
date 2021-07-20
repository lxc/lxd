//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"github.com/lxc/lxd/lxd/db/query"
)

//go:generate -command mapper lxd-generate db mapper -t operations.mapper.go
//go:generate mapper reset
//go:generate mapper stmt -p db -e operation objects
//go:generate mapper stmt -p db -e operation create-or-replace struct=Operation
//go:generate mapper stmt -p db -e operation delete

//go:generate mapper method -p db -e operation List
//go:generate mapper method -p db -e operation CreateOrReplace struct=Operation
//go:generate mapper method -p db -e operation DeleteOne
//go:generate mapper method -p db -e operation DeleteMany

// Operation holds information about a single LXD operation running on a node
// in the cluster.
type Operation struct {
	ID          int64         `db:"primary=yes"`                               // Stable database identifier
	UUID        string        `db:"primary=yes"`                               // User-visible identifier
	NodeAddress string        `db:"join=nodes.address&omit=create-or-replace"` // Address of the node the operation is running on
	ProjectID   *int64        // ID of the project for the operation.
	NodeID      int64         // ID of the node the operation is running on
	Type        OperationType // Type of the operation
}

// OperationFilter specifies potential query parameter fields.
type OperationFilter struct {
	ID     *int64 // ID is a pointer so it can be omitted.
	NodeID *int64 // NodeID is a pointer so it can be omitted.
	UUID   string
}

// GetNodesWithRunningOperations returns a list of nodes that have running operations
func (c *ClusterTx) GetNodesWithRunningOperations(project string) ([]string, error) {
	stmt := `
SELECT DISTINCT nodes.address
  FROM operations
  LEFT OUTER JOIN projects ON projects.id = operations.project_id
  JOIN nodes ON nodes.id = operations.node_id
 WHERE projects.name = ? OR operations.project_id IS NULL
`
	return query.SelectStrings(c.tx, stmt, project)
}

// GetOperationsOfType returns a list operations that belong to the specified project and have the desired type.
func (c *ClusterTx) GetOperationsOfType(projectName string, opType OperationType) ([]Operation, error) {
	var ops []Operation

	stmt := `
SELECT operations.id, operations.uuid, operations.type, nodes.address
  FROM operations
  LEFT JOIN projects on projects.id = operations.project_id
  JOIN nodes on nodes.id = operations.node_id
WHERE (projects.name = ? OR operations.project_id IS NULL) and operations.type = ?
`
	rows, err := c.tx.Query(stmt, projectName, opType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var op Operation
		err := rows.Scan(&op.ID, &op.UUID, &op.Type, &op.NodeAddress)
		if err != nil {
			return nil, err
		}

		ops = append(ops, op)
	}
	if rows.Err() != nil {
		return nil, err
	}

	return ops, nil
}
