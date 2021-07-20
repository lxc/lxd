//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db/cluster"
)

// ErrUnknownEntityID describes the unknown entity ID error
var ErrUnknownEntityID = fmt.Errorf("Unknown entity ID")

// GetURIFromEntity returns the URI for the given entity type and entity ID.
func (c *Cluster) GetURIFromEntity(entityType int, entityID int) (string, error) {
	if entityID == -1 || entityType == -1 {
		return "", nil
	}

	_, ok := cluster.EntityNames[entityType]
	if !ok {
		return "", fmt.Errorf("Unknown entity type")
	}

	var err error
	var uri string

	switch entityType {
	case cluster.TypeImage:
		var images []Image

		err = c.transaction(func(tx *ClusterTx) error {
			images, err = tx.GetImages(ImageFilter{})
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get images")
		}

		for _, image := range images {
			if image.ID != entityID {
				continue
			}

			uri = fmt.Sprintf(cluster.EntityURIs[entityType], image.Fingerprint, image.Project)
			break
		}

		if uri == "" {
			return "", ErrUnknownEntityID
		}
	case cluster.TypeProfile:
		var profiles []Profile

		err = c.transaction(func(tx *ClusterTx) error {
			profiles, err = tx.GetProfiles(ProfileFilter{})
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get profiles")
		}

		for _, profile := range profiles {
			if profile.ID != entityID {
				continue
			}

			uri = fmt.Sprintf(cluster.EntityURIs[entityType], profile.Name, profile.Project)
			break
		}

		if uri == "" {
			return "", ErrUnknownEntityID
		}
	case cluster.TypeProject:
		projects := make(map[int64]string)

		err = c.transaction(func(tx *ClusterTx) error {
			projects, err = tx.GetProjectIDsToNames()
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get project names and IDs")
		}

		name, ok := projects[int64(entityID)]
		if !ok {
			return "", ErrUnknownEntityID
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], name)
	case cluster.TypeCertificate:
		var certificates []Certificate

		err = c.transaction(func(tx *ClusterTx) error {
			certificates, err = tx.GetCertificates(CertificateFilter{})
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get certificates")
		}

		for _, cert := range certificates {
			if cert.ID != entityID {
				continue
			}

			uri = fmt.Sprintf(cluster.EntityURIs[entityType], cert.Name)
			break
		}

		if uri == "" {
			return "", ErrUnknownEntityID
		}
	case cluster.TypeContainer:
		fallthrough
	case cluster.TypeInstance:
		var instances []Instance

		err = c.transaction(func(tx *ClusterTx) error {
			instances, err = tx.GetInstances(*InstanceFilterAllInstances())
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get instances")
		}

		for _, instance := range instances {
			if instance.ID != entityID {
				continue
			}

			uri = fmt.Sprintf(cluster.EntityURIs[entityType], instance.Name, instance.Project)
			break
		}

		if uri == "" {
			return "", ErrUnknownEntityID
		}
	case cluster.TypeInstanceBackup:
		instanceBackup, err := c.GetInstanceBackupWithID(entityID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get instance backup")
		}

		var instances []Instance

		err = c.transaction(func(tx *ClusterTx) error {
			instances, err = tx.GetInstances(*InstanceFilterAllInstances())
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get instances")
		}

		for _, instance := range instances {
			if instance.ID != instanceBackup.InstanceID {
				continue
			}

			uri = fmt.Sprintf(cluster.EntityURIs[entityType], instance.Name, instanceBackup.Name, instance.Project)
			break
		}

		if uri == "" {
			return "", ErrUnknownEntityID
		}
	case cluster.TypeInstanceSnapshot:
		var snapshots []InstanceSnapshot

		err = c.transaction(func(tx *ClusterTx) error {
			snapshots, err = tx.GetInstanceSnapshots(InstanceSnapshotFilter{})
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get instance snapshots")
		}

		for _, snapshot := range snapshots {
			if snapshot.ID != entityID {
				continue
			}

			uri = fmt.Sprintf(cluster.EntityURIs[entityType], snapshot.Name, snapshot.Project)
			break
		}

		if uri == "" {
			return "", ErrUnknownEntityID
		}
	case cluster.TypeNetwork:
		networkName, projectName, err := c.GetNetworkNameAndProjectWithID(entityID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get network name and project name")
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], networkName, projectName)
	case cluster.TypeNetworkACL:
		networkACLName, projectName, err := c.GetNetworkACLNameAndProjectWithID(entityID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get network ACL name and project name")
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], networkACLName, projectName)
	case cluster.TypeNode:
		var nodeInfo NodeInfo

		err := c.transaction(func(tx *ClusterTx) error {
			nodeInfo, err = tx.GetNodeWithID(entityID)
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get node information")
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], nodeInfo.Name)
	case cluster.TypeOperation:
		var op Operation

		err = c.transaction(func(tx *ClusterTx) error {
			id := int64(entityID)
			filter := OperationFilter{ID: &id}
			ops, err := tx.GetOperations(filter)
			if err != nil {
				return err
			}

			if len(ops) > 1 {
				return fmt.Errorf("More than one operation matches")
			}

			op = ops[0]
			return nil
		})
		if err != nil {
			return "", errors.Wrap(err, "Failed to get operation")
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], op.UUID)
	case cluster.TypeStoragePool:
		_, pool, _, err := c.GetStoragePoolWithID(entityID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get storage pool")
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], pool.Name)
	case cluster.TypeStorageVolume:
		args, err := c.GetStoragePoolVolumeWithID(entityID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get storage volume")
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], args.PoolName, args.TypeName, args.Name, args.ProjectName)
	case cluster.TypeStorageVolumeBackup:
		backup, err := c.GetStoragePoolVolumeBackupWithID(entityID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get volume backup")
		}

		instance, err := c.GetStoragePoolVolumeWithID(int(backup.ID))
		if err != nil {
			return "", errors.Wrap(err, "Failed to get storage volume")
		}

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], instance.PoolName, instance.Name, backup.Name, instance.ProjectName)
	case cluster.TypeStorageVolumeSnapshot:
		snapshot, err := c.GetStorageVolumeSnapshotWithID(entityID)
		if err != nil {
			return "", errors.Wrap(err, "Failed to get volume snapshot")
		}

		fields := strings.Split(snapshot.Name, "/")

		uri = fmt.Sprintf(cluster.EntityURIs[entityType], snapshot.PoolName, snapshot, snapshot.TypeName, fields[0], fields[1], snapshot.ProjectName)
	}

	return uri, nil
}
