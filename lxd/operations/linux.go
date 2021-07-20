//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package operations

import (
	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
)

func registerDBOperation(op *Operation, opType db.OperationType) error {
	if op.state == nil {
		return nil
	}

	err := op.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		opInfo := db.Operation{
			UUID:   op.id,
			Type:   opType,
			NodeID: *tx.GetNodeID(),
		}

		if op.projectName != "" {
			projectID, err := tx.GetProjectID(op.projectName)
			if err != nil {
				return errors.Wrap(err, "Fetch project ID")
			}
			opInfo.ProjectID = &projectID
		}

		_, err := tx.CreateOrReplaceOperation(opInfo)
		return err
	})
	if err != nil {
		return errors.Wrapf(err, "failed to add %q Operation %s to database", opType.Description(), op.id)
	}

	return nil
}

func removeDBOperation(op *Operation) error {
	if op.state == nil {
		return nil
	}

	err := op.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		filter := db.OperationFilter{UUID: op.id}
		return tx.DeleteOperation(filter)
	})

	return err
}

func getServerName(op *Operation) (string, error) {
	if op.state == nil {
		return "", nil
	}

	var serverName string
	var err error
	err = op.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		serverName, err = tx.GetLocalNodeName()
		return err
	})
	if err != nil {
		return "", err
	}

	return serverName, nil
}

func (op *Operation) sendEvent(eventMessage interface{}) {
	if op.events == nil {
		return
	}

	op.events.Send(op.projectName, "operation", eventMessage)
}
