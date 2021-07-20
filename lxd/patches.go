package main

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/db/query"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/rsync"
	driver "github.com/lxc/lxd/lxd/storage"
	storagePools "github.com/lxc/lxd/lxd/storage"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
)

type patchStage int

// Define the stages that patches can run at.
const (
	patchNoStageSet patchStage = iota
	patchPreDaemonStorage
	patchPostDaemonStorage
)

// Leave the string type in here! This guarantees that go treats this is as a
// typed string constant. Removing it causes go to treat these as untyped string
// constants which is not what we want.
const (
	patchStoragePoolVolumeAPIEndpointContainers string = "containers"
	patchStoragePoolVolumeAPIEndpointVMs        string = "virtual-machines"
	patchStoragePoolVolumeAPIEndpointImages     string = "images"
	patchStoragePoolVolumeAPIEndpointCustom     string = "custom"
)

/* Patches are one-time actions that are sometimes needed to update
   existing container configuration or move things around on the
   filesystem.

   Those patches are applied at startup time after the database schema
   has been fully updated. Patches can therefore assume a working database.

   At the time the patches are applied, the containers aren't started
   yet and the daemon isn't listening to requests.

   DO NOT use this mechanism for database update. Schema updates must be
   done through the separate schema update mechanism.


   Only append to the patches list, never remove entries and never re-order them.
*/
var patches = []patch{
	{name: "shrink_logs_db_file", stage: patchPostDaemonStorage, run: patchShrinkLogsDBFile},
	{name: "invalid_profile_names", stage: patchPostDaemonStorage, run: patchInvalidProfileNames},
	{name: "leftover_profile_config", stage: patchPostDaemonStorage, run: patchLeftoverProfileConfig},
	{name: "network_permissions", stage: patchPostDaemonStorage, run: patchNetworkPermissions},
	{name: "storage_api", stage: patchPostDaemonStorage, run: patchStorageApi},
	{name: "storage_api_v1", stage: patchPostDaemonStorage, run: patchStorageApiV1},
	{name: "storage_api_dir_cleanup", stage: patchPostDaemonStorage, run: patchStorageApiDirCleanup},
	{name: "storage_api_lvm_keys", stage: patchPostDaemonStorage, run: patchStorageApiLvmKeys},
	{name: "storage_api_keys", stage: patchPostDaemonStorage, run: patchStorageApiKeys},
	{name: "storage_api_update_storage_configs", stage: patchPostDaemonStorage, run: patchStorageApiUpdateStorageConfigs},
	{name: "storage_api_lxd_on_btrfs", stage: patchPostDaemonStorage, run: patchStorageApiLxdOnBtrfs},
	{name: "storage_api_lvm_detect_lv_size", stage: patchPostDaemonStorage, run: patchStorageApiDetectLVSize},
	{name: "storage_api_insert_zfs_driver", stage: patchPostDaemonStorage, run: patchStorageApiInsertZfsDriver},
	{name: "storage_zfs_noauto", stage: patchPostDaemonStorage, run: patchStorageZFSnoauto},
	{name: "storage_zfs_volume_size", stage: patchPostDaemonStorage, run: patchStorageZFSVolumeSize},
	{name: "network_dnsmasq_hosts", stage: patchPostDaemonStorage, run: patchNetworkDnsmasqHosts},
	{name: "storage_api_dir_bind_mount", stage: patchPostDaemonStorage, run: patchStorageApiDirBindMount},
	{name: "fix_uploaded_at", stage: patchPostDaemonStorage, run: patchFixUploadedAt},
	{name: "storage_api_ceph_size_remove", stage: patchPostDaemonStorage, run: patchStorageApiCephSizeRemove},
	{name: "devices_new_naming_scheme", stage: patchPostDaemonStorage, run: patchDevicesNewNamingScheme},
	{name: "storage_api_permissions", stage: patchPostDaemonStorage, run: patchStorageApiPermissions},
	{name: "container_config_regen", stage: patchPostDaemonStorage, run: patchContainerConfigRegen},
	{name: "lvm_node_specific_config_keys", stage: patchPostDaemonStorage, run: patchLvmNodeSpecificConfigKeys},
	{name: "candid_rename_config_key", stage: patchPostDaemonStorage, run: patchCandidConfigKey},
	{name: "move_backups", stage: patchPostDaemonStorage, run: patchMoveBackups},
	{name: "storage_api_rename_container_snapshots_dir", stage: patchPostDaemonStorage, run: patchStorageApiRenameContainerSnapshotsDir},
	{name: "storage_api_rename_container_snapshots_links", stage: patchPostDaemonStorage, run: patchStorageApiUpdateContainerSnapshots},
	{name: "fix_lvm_pool_volume_names", stage: patchPostDaemonStorage, run: patchRenameCustomVolumeLVs},
	{name: "storage_api_rename_container_snapshots_dir_again", stage: patchPostDaemonStorage, run: patchStorageApiRenameContainerSnapshotsDir},
	{name: "storage_api_rename_container_snapshots_links_again", stage: patchPostDaemonStorage, run: patchStorageApiUpdateContainerSnapshots},
	{name: "storage_api_rename_container_snapshots_dir_again_again", stage: patchPostDaemonStorage, run: patchStorageApiRenameContainerSnapshotsDir},
	{name: "clustering_add_roles", stage: patchPostDaemonStorage, run: patchClusteringAddRoles},
	{name: "clustering_add_roles_again", stage: patchPostDaemonStorage, run: patchClusteringAddRoles},
	{name: "storage_create_vm", stage: patchPostDaemonStorage, run: patchGenericStorage},
	{name: "storage_zfs_mount", stage: patchPostDaemonStorage, run: patchGenericStorage},
	{name: "network_pid_files", stage: patchPostDaemonStorage, run: patchNetworkPIDFiles},
	{name: "storage_create_vm_again", stage: patchPostDaemonStorage, run: patchGenericStorage},
	{name: "storage_zfs_volmode", stage: patchPostDaemonStorage, run: patchGenericStorage},
	{name: "storage_rename_custom_volume_add_project", stage: patchPreDaemonStorage, run: patchGenericStorage},
	{name: "storage_lvm_skipactivation", stage: patchPostDaemonStorage, run: patchGenericStorage},
	{name: "clustering_drop_database_role", stage: patchPostDaemonStorage, run: patchClusteringDropDatabaseRole},
	{name: "network_clear_bridge_volatile_hwaddr", stage: patchPostDaemonStorage, run: patchNetworkClearBridgeVolatileHwaddr},
	{name: "move_backups_instances", stage: patchPostDaemonStorage, run: patchMoveBackupsInstances},
	{name: "network_ovn_enable_nat", stage: patchPostDaemonStorage, run: patchNetworkOVNEnableNAT},
	{name: "network_ovn_remove_routes", stage: patchPostDaemonStorage, run: patchNetworkOVNRemoveRoutes},
	{name: "network_fan_enable_nat", stage: patchPostDaemonStorage, run: patchNetworkFANEnableNAT},
	{name: "thinpool_typo_fix", stage: patchPostDaemonStorage, run: patchThinpoolTypoFix},
	{name: "vm_rename_uuid_key", stage: patchPostDaemonStorage, run: patchVMRenameUUIDKey},
	{name: "db_nodes_autoinc", stage: patchPreDaemonStorage, run: patchDBNodesAutoInc},
	{name: "network_acl_remove_defaults", stage: patchPostDaemonStorage, run: patchNetworkACLRemoveDefaults},
	{name: "clustering_server_cert_trust", stage: patchPreDaemonStorage, run: patchClusteringServerCertTrust},
	{name: "warnings_remove_empty_node", stage: patchPostDaemonStorage, run: patchRemoveWarningsWithEmptyNode},
}

type patch struct {
	name  string
	stage patchStage
	run   func(name string, d *Daemon) error
}

func (p *patch) apply(d *Daemon) error {
	logger.Infof("Applying patch %q", p.name)

	err := p.run(p.name, d)
	if err != nil {
		return errors.Wrapf(err, "Failed applying patch %q", p.name)
	}

	err = d.db.MarkPatchAsApplied(p.name)
	if err != nil {
		return errors.Wrapf(err, "Failed marking patch applied %q", p.name)
	}

	return nil
}

// Return the names of all available patches.
func patchesGetNames() []string {
	names := make([]string, len(patches))
	for i, patch := range patches {
		if patch.stage == patchNoStageSet {
			continue // Ignore any patch without explicitly set stage (it is defined incorrectly).
		}

		names[i] = patch.name
	}
	return names
}

// patchesApplyPostDaemonStorage applies the patches that need to run after the daemon storage is initialised.
func patchesApply(d *Daemon, stage patchStage) error {
	appliedPatches, err := d.db.GetAppliedPatches()
	if err != nil {
		return err
	}

	for _, patch := range patches {
		if patch.stage == patchNoStageSet {
			return fmt.Errorf("Patch %q has no stage set: %d", patch.name, patch.stage)
		}

		if shared.StringInSlice(patch.name, appliedPatches) {
			continue
		}

		err := patch.apply(d)
		if err != nil {
			return err
		}
	}

	return nil
}

// Patches begin here

func patchRemoveWarningsWithEmptyNode(name string, d *Daemon) error {
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		warnings, err := tx.GetWarnings()
		if err != nil {
			return err
		}

		for _, w := range warnings {
			if w.Node == "" {
				err = tx.DeleteWarning(w.UUID)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func patchClusteringServerCertTrust(name string, d *Daemon) error {
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return err
	}

	if !clustered {
		return nil
	}

	var serverName string
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		serverName, err = tx.GetLocalNodeName()
		return err
	})
	if err != nil {
		return err
	}

	// Add our server cert to DB trust store.
	serverCert, err := util.LoadServerCert(d.os.VarDir)
	if err != nil {
		return err
	}
	// Update our own entry in the nodes table.
	logger.Infof("Adding local server certificate to global trust store for %q patch", name)
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		return cluster.EnsureServerCertificateTrusted(serverName, serverCert, tx)
	})
	if err != nil {
		return err
	}
	logger.Infof("Added local server certificate to global trust store for %q patch", name)

	// Check all other members have done the same.
	for {
		var err error
		var dbCerts []db.Certificate
		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			dbCerts, err = tx.GetCertificates(db.CertificateFilter{})
			return err
		})
		if err != nil {
			return err
		}

		trustedServerCerts := make(map[string]*db.Certificate)

		for _, c := range dbCerts {
			if c.Type == db.CertificateTypeServer {
				trustedServerCerts[c.Name] = &c
			}
		}

		var nodes []db.NodeInfo
		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			nodes, err = tx.GetNodes()
			if err != nil {
				return err
			}

			return nil
		})
		if err != nil {
			return err
		}

		missingCerts := false
		for _, n := range nodes {
			if _, found := trustedServerCerts[n.Name]; !found {
				logger.Warnf("Missing trusted server certificate for cluster member %q", n.Name)
				missingCerts = true
				break
			}
		}

		if missingCerts {
			logger.Warnf("Waiting for %q patch to be applied on all cluster members", name)
			time.Sleep(time.Second)
			continue
		}

		logger.Infof("Trusted server certificates found in trust store for all cluster members")
		break
	}

	// Now switch to using our server certificate for intra-cluster communication and load the trusted server
	// certificates for the other members into the in-memory trusted cache.
	logger.Infof("Set client certificate to server certificate %v", serverCert.Fingerprint())
	d.serverCertInt = serverCert
	updateCertificateCache(d)

	return nil
}

// patchNetworkACLRemoveDefaults removes the "default.action" and "default.logged" settings from network ACLs.
// It was decided that the user experience of having the default actions at the ACL level was confusing when using
// multiple ACLs, and that the interplay between conflicting default actions on multiple ACLs was difficult to
// understand. Instead it will be replace with a network and NIC level defaults settings.
func patchNetworkACLRemoveDefaults(name string, d *Daemon) error {
	var err error
	var projectNames []string

	// Get projects.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		projectNames, err = tx.GetProjectNames()
		return err
	})
	if err != nil {
		return err
	}

	// Get ACLs in projects.
	for _, projectName := range projectNames {
		aclNames, err := d.cluster.GetNetworkACLs(projectName)
		if err != nil {
			return err
		}

		for _, aclName := range aclNames {
			aclID, acl, err := d.cluster.GetNetworkACL(projectName, aclName)
			if err != nil {
				return err
			}

			modified := false

			// Remove the offending keys if found.
			if _, found := acl.Config["default.action"]; found {
				delete(acl.Config, "default.action")
				modified = true
			}

			if _, found := acl.Config["default.logged"]; found {
				delete(acl.Config, "default.logged")
				modified = true
			}

			// Write back modified config if needed.
			if modified {
				err = d.cluster.UpdateNetworkACL(aclID, &acl.NetworkACLPut)
				if err != nil {
					return errors.Wrapf(err, "Failed updating network ACL %d", aclID)
				}
			}
		}
	}

	return nil
}

// patchDBNodesAutoInc re-creates the nodes table id column as AUTOINCREMENT.
// Its done as a patch rather than a schema update so we can use PRAGMA foreign_keys = OFF without a transaction.
func patchDBNodesAutoInc(name string, d *Daemon) error {
	for {
		// Only apply patch if schema needs it.
		var schemaSQL string
		row := d.State().Cluster.DB().QueryRow("SELECT sql FROM sqlite_master WHERE name = 'nodes'")
		err := row.Scan(&schemaSQL)
		if err != nil {
			return err
		}

		if strings.Contains(schemaSQL, "id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL") {
			logger.Debugf(`Skipping %q patch as "nodes" table id column already AUTOINCREMENT`, name)
			return nil // Nothing to do.
		}

		// Only apply patch on leader, otherwise wait for it to be applied.
		clusterAddress, err := node.ClusterAddress(d.db)
		if err != nil {
			return err
		}

		leaderAddress, err := d.gateway.LeaderAddress()
		if err != nil {
			if errors.Cause(err) == cluster.ErrNodeIsNotClustered {
				break // Apply change on standalone node.
			}

			return err
		}

		if clusterAddress == leaderAddress {
			break // Apply change on leader node.
		}

		logger.Warnf("Waiting for %q patch to be applied on leader cluster member", name)
		time.Sleep(time.Second)
	}

	// Apply patch.
	_, err := d.State().Cluster.DB().Exec(`
PRAGMA foreign_keys=OFF; -- So that integrity doesn't get in the way for now.
PRAGMA legacy_alter_table = ON; -- So that views referencing this table don't block change.

CREATE TABLE nodes_new (
	id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
	name TEXT NOT NULL,
	description TEXT DEFAULT '',
	address TEXT NOT NULL,
	schema INTEGER NOT NULL,
	api_extensions INTEGER NOT NULL,
	heartbeat DATETIME DEFAULT CURRENT_TIMESTAMP,
	state INTEGER NOT NULL DEFAULT 0,
	arch INTEGER NOT NULL DEFAULT 0 CHECK (arch > 0),
	failure_domain_id INTEGER DEFAULT NULL REFERENCES nodes_failure_domains (id) ON DELETE SET NULL,
	UNIQUE (name),
	UNIQUE (address)
);

INSERT INTO nodes_new (id, name, description, address, schema, api_extensions, heartbeat, state, arch, failure_domain_id)
	SELECT id, name, description, address, schema, api_extensions, heartbeat, state, arch, failure_domain_id FROM nodes;

DROP TABLE nodes;
ALTER TABLE nodes_new RENAME TO nodes;

PRAGMA foreign_keys=ON; -- Make sure we turn integrity checks back on.
PRAGMA legacy_alter_table = OFF; -- So views check integrity again.
`)

	return err
}

// patchVMRenameUUIDKey renames the volatile.vm.uuid key to volatile.uuid in instance and snapshot configs.
func patchVMRenameUUIDKey(name string, d *Daemon) error {
	oldUUIDKey := "volatile.vm.uuid"
	newUUIDKey := "volatile.uuid"

	return d.State().Cluster.InstanceList(nil, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
		if inst.Type != instancetype.VM {
			return nil
		}

		return d.State().Cluster.Transaction(func(tx *db.ClusterTx) error {
			uuid := inst.Config[oldUUIDKey]
			if uuid != "" {
				changes := map[string]string{
					oldUUIDKey: "",
					newUUIDKey: uuid,
				}

				logger.Debugf("Renaming config key %q to %q for VM %q (Project %q)", oldUUIDKey, newUUIDKey, inst.Name, inst.Project)
				err := tx.UpdateInstanceConfig(inst.ID, changes)
				if err != nil {
					return errors.Wrapf(err, "Failed renaming config key %q to %q for VM %q (Project %q)", oldUUIDKey, newUUIDKey, inst.Name, inst.Project)
				}
			}

			snaps, err := tx.GetInstanceSnapshotsWithName(inst.Project, inst.Name)
			if err != nil {
				return err
			}

			for _, snap := range snaps {
				uuid := snap.Config[oldUUIDKey]
				if uuid != "" {
					changes := map[string]string{
						oldUUIDKey: "",
						newUUIDKey: uuid,
					}

					logger.Debugf("Renaming config key %q to %q for VM %q (Project %q)", oldUUIDKey, newUUIDKey, snap.Name, snap.Project)
					err = tx.UpdateInstanceSnapshotConfig(snap.ID, changes)
					if err != nil {
						return errors.Wrapf(err, "Failed renaming config key %q to %q for VM %q (Project %q)", oldUUIDKey, newUUIDKey, snap.Name, snap.Project)
					}
				}
			}

			return nil
		})
	})
}

// patchThinpoolTypoFix renames any config incorrectly set config file entries due to the lvm.thinpool_name typo.
func patchThinpoolTypoFix(name string, d *Daemon) error {
	revert := revert.New()
	defer revert.Fail()

	// Setup a transaction.
	tx, err := d.cluster.Begin()
	if err != nil {
		return errors.Wrap(err, "Failed to begin transaction")
	}

	revert.Add(func() { tx.Rollback() })

	// Fetch the IDs of all existing nodes.
	nodeIDs, err := query.SelectIntegers(tx, "SELECT id FROM nodes")
	if err != nil {
		return errors.Wrap(err, "Failed to get IDs of current nodes")
	}

	// Fetch the IDs of all existing lvm pools.
	poolIDs, err := query.SelectIntegers(tx, "SELECT id FROM storage_pools WHERE driver='lvm'")
	if err != nil {
		return errors.Wrap(err, "Failed to get IDs of current lvm pools")
	}

	for _, poolID := range poolIDs {
		// Fetch the config for this lvm pool and check if it has the lvm.thinpool_name.
		config, err := query.SelectConfig(
			tx, "storage_pools_config", "storage_pool_id=? AND node_id IS NULL", poolID)
		if err != nil {
			return errors.Wrap(err, "Failed to fetch of lvm pool config")
		}

		value, ok := config["lvm.thinpool_name"]
		if !ok {
			continue
		}

		// Delete the current key
		_, err = tx.Exec(`
DELETE FROM storage_pools_config WHERE key='lvm.thinpool_name' AND storage_pool_id=? AND node_id IS NULL
`, poolID)
		if err != nil {
			return errors.Wrapf(err, "Failed to delete lvm.thinpool_name config")
		}

		// Add the config entry for each node
		for _, nodeID := range nodeIDs {
			_, err := tx.Exec(`
INSERT INTO storage_pools_config(storage_pool_id, node_id, key, value)
  VALUES(?, ?, 'lvm.thinpool_name', ?)
`, poolID, nodeID, value)
			if err != nil {
				return errors.Wrapf(err, "Failed to create lvm.thinpool_name node config")
			}
		}
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "Failed to commit transaction")
	}

	revert.Success()
	return nil
}

// patchNetworkFANEnableNAT sets "ipv4.nat=true" on fan bridges that are missing the "ipv4.nat" setting.
// This prevents outbound connectivity breaking on existing fan networks now that the default behaviour of not
// having "ipv4.nat" set is to disable NAT (bringing in line with the non-fan bridge behavior and docs).
func patchNetworkFANEnableNAT(name string, d *Daemon) error {
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		projectNetworks, err := tx.GetCreatedNetworks()
		if err != nil {
			return err
		}

		for _, networks := range projectNetworks {
			for networkID, network := range networks {
				if network.Type != "bridge" {
					continue
				}

				if network.Config["bridge.mode"] != "fan" {
					continue
				}

				modified := false

				// Enable ipv4.nat if setting not specified.
				if _, found := network.Config["ipv4.nat"]; !found {
					modified = true
					network.Config["ipv4.nat"] = "true"
				}

				if modified {
					err = tx.UpdateNetwork(networkID, network.Description, network.Config)
					if err != nil {
						return errors.Wrapf(err, "Failed setting ipv4.nat=true for fan network %q (%d)", network.Name, networkID)
					}

					logger.Debugf("Set ipv4.nat=true for fan network %q (%d)", network.Name, networkID)
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// patchNetworkOVNRemoveRoutes removes the "ipv4.routes.external" and "ipv6.routes.external" settings from OVN
// networks. It was decided that the OVN NIC level equivalent settings were sufficient.
func patchNetworkOVNRemoveRoutes(name string, d *Daemon) error {
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		projectNetworks, err := tx.GetCreatedNetworks()
		if err != nil {
			return err
		}

		for _, networks := range projectNetworks {
			for networkID, network := range networks {
				if network.Type != "ovn" {
					continue
				}

				modified := false

				// Ensure existing behaviour of having NAT enabled if IP address was set.
				if _, found := network.Config["ipv4.routes.external"]; found {
					modified = true
					delete(network.Config, "ipv4.routes.external")
				}

				if _, found := network.Config["ipv6.routes.external"]; found {
					modified = true
					delete(network.Config, "ipv6.routes.external")
				}

				if modified {
					err = tx.UpdateNetwork(networkID, network.Description, network.Config)
					if err != nil {
						return errors.Wrapf(err, "Failed removing OVN external route settings for %q (%d)", network.Name, networkID)
					}

					logger.Debugf("Removing external route settings for OVN network %q (%d)", network.Name, networkID)
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// patchNetworkOVNEnableNAT adds "ipv4.nat" and "ipv6.nat" keys set to "true" to OVN networks if not present.
// This is to ensure existing networks retain the old behaviour of always having NAT enabled as we introduce
// the new NAT settings which default to disabled if not specified.
// patchNetworkCearBridgeVolatileHwaddr removes the unsupported `volatile.bridge.hwaddr` config key from networks.
func patchNetworkOVNEnableNAT(name string, d *Daemon) error {
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		projectNetworks, err := tx.GetCreatedNetworks()
		if err != nil {
			return err
		}

		for _, networks := range projectNetworks {
			for networkID, network := range networks {
				if network.Type != "ovn" {
					continue
				}

				modified := false

				// Ensure existing behaviour of having NAT enabled if IP address was set.
				if network.Config["ipv4.address"] != "" && network.Config["ipv4.nat"] == "" {
					modified = true
					network.Config["ipv4.nat"] = "true"
				}

				if network.Config["ipv6.address"] != "" && network.Config["ipv6.nat"] == "" {
					modified = true
					network.Config["ipv6.nat"] = "true"
				}

				if modified {
					err = tx.UpdateNetwork(networkID, network.Description, network.Config)
					if err != nil {
						return errors.Wrapf(err, "Failed saving OVN NAT settings for %q (%d)", network.Name, networkID)
					}

					logger.Debugf("Enabling NAT for OVN network %q (%d)", network.Name, networkID)
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// Moves backups from shared.VarPath("backups") to shared.VarPath("backups", "instances").
func patchMoveBackupsInstances(name string, d *Daemon) error {
	if !shared.PathExists(shared.VarPath("backups")) {
		return nil // Nothing to do, no backups directory.
	}

	backupsPath := shared.VarPath("backups", "instances")

	err := os.MkdirAll(backupsPath, 0700)
	if err != nil {
		return errors.Wrapf(err, "Failed creating instances backup directory %q", backupsPath)
	}

	backups, err := ioutil.ReadDir(shared.VarPath("backups"))
	if err != nil {
		return errors.Wrapf(err, "Failed listing existing backup directory %q", shared.VarPath("backups"))
	}

	for _, backupDir := range backups {
		if backupDir.Name() == "instances" || strings.HasPrefix(backupDir.Name(), backup.WorkingDirPrefix) {
			continue // Don't try and move our new instances directory or temporary directories.
		}

		oldPath := shared.VarPath("backups", backupDir.Name())
		newPath := filepath.Join(backupsPath, backupDir.Name())
		logger.Debugf("Moving backup from %q to %q", oldPath, newPath)
		err = os.Rename(oldPath, newPath)
		if err != nil {
			return errors.Wrapf(err, "Failed moving backup from %q to %q", oldPath, newPath)
		}
	}

	return nil
}

func patchNetworkPIDFiles(name string, d *Daemon) error {
	networks, err := ioutil.ReadDir(shared.VarPath("networks"))
	if err != nil {
		return err
	}

	for _, network := range networks {
		networkName := network.Name()
		networkPath := filepath.Join(shared.VarPath("networks"), networkName)

		for _, pidFile := range []string{"dnsmasq.pid", "forkdns.pid"} {
			pidPath := filepath.Join(networkPath, pidFile)
			if !shared.PathExists(pidPath) {
				continue
			}

			content, err := ioutil.ReadFile(pidPath)
			if err != nil {
				logger.Errorf("Failed to read PID file '%s': %v", pidPath, err)
				continue
			}

			pid, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
			if err != nil {
				logger.Errorf("Failed to parse PID file '%s': %v", pidPath, err)
				continue
			}

			err = ioutil.WriteFile(pidPath, []byte(fmt.Sprintf("pid: %d\n", pid)), 0600)
			if err != nil {
				logger.Errorf("Failed to write new PID file '%s': %v", pidPath, err)
				continue
			}
		}
	}

	return nil
}

func patchGenericStorage(name string, d *Daemon) error {
	// Load all the pools.
	pools, _ := d.cluster.GetStoragePoolNames()

	for _, poolName := range pools {
		pool, err := storagePools.GetPoolByName(d.State(), poolName)
		if err != storageDrivers.ErrUnknownDriver {
			if err != nil {
				return err
			}

			err = pool.ApplyPatch(name)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchRenameCustomVolumeLVs(name string, d *Daemon) error {
	// Ignore the error since it will also fail if there are no pools.
	pools, _ := d.cluster.GetStoragePoolNames()

	for _, poolName := range pools {
		poolID, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			return err
		}

		if pool.Driver != "lvm" {
			continue
		}

		volumes, err := d.cluster.GetLocalStoragePoolVolumesWithType(project.Default, db.StoragePoolVolumeTypeCustom, poolID)
		if err != nil {
			return err
		}

		vgName := poolName
		if pool.Config["lvm.vg_name"] != "" {
			vgName = pool.Config["lvm.vg_name"]
		}

		for _, volume := range volumes {
			oldName := fmt.Sprintf("%s/custom_%s", vgName, volume)
			newName := fmt.Sprintf("%s/custom_%s", vgName, lvmNameToLVName(volume))

			exists, err := lvmLVExists(newName)
			if err != nil {
				return err
			}

			if exists || oldName == newName {
				continue
			}

			err = lvmLVRename(vgName, oldName, newName)
			if err != nil {
				return err
			}

			logger.Info("Successfully renamed LV", log.Ctx{"old_name": oldName, "new_name": newName})
		}
	}

	return nil
}

func patchLeftoverProfileConfig(name string, d *Daemon) error {
	return d.cluster.RemoveUnreferencedProfiles()
}

func patchInvalidProfileNames(name string, d *Daemon) error {
	profiles, err := d.cluster.GetProfileNames("default")
	if err != nil {
		return err
	}

	for _, profile := range profiles {
		if strings.Contains(profile, "/") || shared.StringInSlice(profile, []string{".", ".."}) {
			logger.Info("Removing unreachable profile (invalid name)", log.Ctx{"name": profile})
			err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
				filter := db.ProfileFilter{Project: project.Default, Name: profile}
				return tx.DeleteProfile(filter)
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchNetworkPermissions(name string, d *Daemon) error {
	// Get the list of networks
	// Pass project.Default, as networks didn't support projects here.
	networks, err := d.cluster.GetNetworks(project.Default)
	if err != nil {
		return err
	}

	// Fix the permissions
	err = os.Chmod(shared.VarPath("networks"), 0711)
	if err != nil {
		return err
	}

	for _, network := range networks {
		if !shared.PathExists(shared.VarPath("networks", network)) {
			continue
		}

		err = os.Chmod(shared.VarPath("networks", network), 0711)
		if err != nil {
			return err
		}

		if shared.PathExists(shared.VarPath("networks", network, "dnsmasq.hosts")) {
			err = os.Chmod(shared.VarPath("networks", network, "dnsmasq.hosts"), 0644)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// This patch used to shrink the database/global/logs.db file, but it's not
// needed anymore since dqlite 1.0.
func patchShrinkLogsDBFile(name string, d *Daemon) error {
	return nil
}

func patchStorageApi(name string, d *Daemon) error {
	var daemonConfig map[string]string
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		daemonConfig, err = tx.Config()
		return err
	})
	if err != nil {
		return err
	}

	lvmVgName := daemonConfig["storage.lvm_vg_name"]
	zfsPoolName := daemonConfig["storage.zfs_pool_name"]
	defaultPoolName := "default"
	preStorageApiStorageType := "dir"

	if lvmVgName != "" {
		preStorageApiStorageType = "lvm"
		defaultPoolName = lvmVgName
	} else if zfsPoolName != "" {
		preStorageApiStorageType = "zfs"
		defaultPoolName = zfsPoolName
	} else if d.os.BackingFS == "btrfs" {
		preStorageApiStorageType = "btrfs"
	} else {
		// Dir storage pool.
	}

	// In case we detect that an lvm name or a zfs name exists it makes
	// sense to create a storage pool in the database, independent of
	// whether anything currently exists on that pool. We can still probably
	// safely assume that the user at least once used that pool.
	// However, when we detect {dir, btrfs}, we can't rely on that guess
	// since the daemon doesn't record any name for the pool anywhere.  So
	// in the {dir, btrfs} case we check whether anything exists on the
	// pool, if not, then we don't create a default pool. The user will then
	// be forced to run lxd init again and can start from a pristine state.
	// Check if this LXD instace currently has any containers, snapshots, or
	// images configured. If so, we create a default storage pool in the
	// database. Otherwise, the user will have to run LXD init.
	cRegular, err := d.cluster.LegacyContainersList()
	if err != nil {
		return err
	}

	// Get list of existing snapshots.
	cSnapshots, err := d.cluster.LegacySnapshotsList()
	if err != nil {
		return err
	}

	// Get list of existing public images.
	imgPublic, err := d.cluster.GetImagesFingerprints("default", true)
	if err != nil {
		return err
	}

	// Get list of existing private images.
	imgPrivate, err := d.cluster.GetImagesFingerprints("default", false)
	if err != nil {
		return err
	}

	// Nothing exists on the pool so we're not creating a default one,
	// thereby forcing the user to run lxd init.
	if len(cRegular) == 0 && len(cSnapshots) == 0 && len(imgPublic) == 0 && len(imgPrivate) == 0 {
		return nil
	}

	// If any of these are actually called, there's no way back.
	poolName := defaultPoolName
	switch preStorageApiStorageType {
	case "btrfs":
		err = upgradeFromStorageTypeBtrfs(name, d, defaultPoolName, preStorageApiStorageType, cRegular, cSnapshots, imgPublic, imgPrivate)
	case "dir":
		err = upgradeFromStorageTypeDir(name, d, defaultPoolName, preStorageApiStorageType, cRegular, cSnapshots, imgPublic, imgPrivate)
	case "lvm":
		err = upgradeFromStorageTypeLvm(name, d, defaultPoolName, preStorageApiStorageType, cRegular, cSnapshots, imgPublic, imgPrivate)
	case "zfs":
		// The user is using a zfs dataset. This case needs to be
		// handled with care:

		// - The pool name that is used in the storage backends needs
		//   to be set to a sane name that doesn't contain a slash "/".
		//   This is what this snippet is for.
		// - The full dataset name <pool_name>/<volume_name> needs to be
		//   set as the source value.
		if strings.Contains(defaultPoolName, "/") {
			poolName = "default"
		}
		err = upgradeFromStorageTypeZfs(name, d, defaultPoolName, preStorageApiStorageType, cRegular, []string{}, imgPublic, imgPrivate)
	default: // Shouldn't happen.
		return fmt.Errorf("Invalid storage type. Upgrading not possible")
	}
	if err != nil {
		return err
	}

	// The new storage api enforces that the default storage pool on which
	// containers are created is set in the default profile. If it isn't
	// set, then LXD will refuse to create a container until either an
	// appropriate device including a pool is added to the default profile
	// or the user explicitly passes the pool the container's storage volume
	// is supposed to be created on.
	allcontainers := append(cRegular, cSnapshots...)
	err = updatePoolPropertyForAllObjects(d, poolName, allcontainers)
	if err != nil {
		return err
	}

	// Unset deprecated storage keys.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		config, err := cluster.ConfigLoad(tx)
		if err != nil {
			return err
		}
		_, err = config.Patch(map[string]interface{}{
			"storage.lvm_fstype":           "",
			"storage.lvm_mount_options":    "",
			"storage.lvm_thinpool_name":    "",
			"storage.lvm_vg_name":          "",
			"storage.lvm_volume_size":      "",
			"storage.zfs_pool_name":        "",
			"storage.zfs_remove_snapshots": "",
			"storage.zfs_use_refquota":     "",
		})
		return err
	})
	if err != nil {
		return err
	}

	return setupStorageDriver(d.State(), true)
}

func upgradeFromStorageTypeBtrfs(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	poolSubvolumePath := driver.GetStoragePoolMountPoint(defaultPoolName)
	poolConfig["source"] = poolSubvolumePath

	err := storagePoolValidateConfig(defaultPoolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(defaultPoolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	var poolID int64
	pools, err := d.cluster.GetStoragePoolNames()
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(defaultPoolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s", defaultPoolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, _, err := d.cluster.GetStoragePoolInAnyState(defaultPoolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = d.cluster.UpdateStoragePool(defaultPoolName, "", pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.ErrNoSuchObject { // Likely a pristine upgrade.
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.State(), defaultPoolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			return err
		}
		poolID = tmp
		poolInfo := api.StoragePoolsPost{
			StoragePoolPut: api.StoragePoolPut{
				Config:      poolConfig,
				Description: "",
			},
			Name:   defaultPoolName,
			Driver: defaultStorageTypeName,
		}

		_, err = storagePools.CreatePool(d.State(), poolID, &poolInfo)
		if err != nil {
			return err
		}
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	if len(cRegular) > 0 {
		// ${LXD_DIR}/storage-pools/<name>
		containersSubvolumePath := driver.GetContainerMountPoint("default", defaultPoolName, "")
		if !shared.PathExists(containersSubvolumePath) {
			err := os.MkdirAll(containersSubvolumePath, 0711)
			if err != nil {
				return err
			}
		}
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, _, err := d.cluster.GetStoragePoolInAnyState(defaultPoolName)
	if err != nil {
		return err
	}

	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, ct, db.StoragePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container")
			err := d.cluster.UpdateStoragePoolVolume("default", ct, db.StoragePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", ct, "", db.StoragePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\"", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Rename the btrfs subvolume and making it a
		// subvolume of the subvolume of the storage pool:
		// mv ${LXD_DIR}/containers/<container_name> ${LXD_DIR}/storage-pools/<pool>/<container_name>
		oldContainerMntPoint := shared.VarPath("containers", ct)
		newContainerMntPoint := driver.GetContainerMountPoint("default", defaultPoolName, ct)
		if shared.PathExists(oldContainerMntPoint) && !shared.PathExists(newContainerMntPoint) {
			err = os.Rename(oldContainerMntPoint, newContainerMntPoint)
			if err != nil {
				err := btrfsSubVolumeCreate(newContainerMntPoint)
				if err != nil {
					return err
				}

				_, err = rsync.LocalCopy(oldContainerMntPoint, newContainerMntPoint, "", true)
				if err != nil {
					logger.Errorf("Failed to rsync: %v", err)
					return err
				}

				btrfsSubVolumesDelete(oldContainerMntPoint)
				if shared.PathExists(oldContainerMntPoint) {
					err = os.RemoveAll(oldContainerMntPoint)
					if err != nil {
						return err
					}
				}
			}
		}

		// Create a symlink to the mountpoint of the container:
		// ${LXD_DIR}/containers/<container_name> to
		// ${LXD_DIR}/storage-pools/<pool>/containers/<container_name>
		doesntMatter := false
		err = driver.CreateContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			return err
		}

		// Check if we need to account for snapshots for this container.
		ctSnapshots, err := d.cluster.GetInstanceSnapshotsNames("default", ct)
		if err != nil {
			return err
		}

		if len(ctSnapshots) > 0 {
			// Create the snapshots directory in
			// the new storage pool:
			// ${LXD_DIR}/storage-pools/<pool>/snapshots
			newSnapshotsMntPoint := driver.GetSnapshotMountPoint("default", defaultPoolName, ct)
			if !shared.PathExists(newSnapshotsMntPoint) {
				err := os.MkdirAll(newSnapshotsMntPoint, 0700)
				if err != nil {
					return err
				}
			}
		}

		for _, cs := range ctSnapshots {
			// Insert storage volumes for snapshots into the
			// database. Note that snapshots have already been moved
			// and symlinked above. So no need to do any work here.
			// Initialize empty storage volume configuration for the
			// container.
			snapshotPoolVolumeConfig := map[string]string{}
			err = volumeFillDefault(snapshotPoolVolumeConfig, defaultPool)
			if err != nil {
				return err
			}

			_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, cs, db.StoragePoolVolumeTypeContainer, poolID)
			if err == nil {
				logger.Warnf("Storage volumes database already contains an entry for the snapshot")
				err := d.cluster.UpdateStoragePoolVolume("default", cs, db.StoragePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
				if err != nil {
					return err
				}
			} else if err == db.ErrNoSuchObject {
				// Insert storage volumes for containers into the database.
				_, err := d.cluster.CreateStorageVolumeSnapshot("default", cs, "", db.StoragePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig, time.Time{})
				if err != nil {
					logger.Errorf("Could not insert a storage volume for snapshot \"%s\"", cs)
					return err
				}
			} else {
				logger.Errorf("Failed to query database: %s", err)
				return err
			}

			// We need to create a new snapshot since we can't move
			// readonly snapshots.
			oldSnapshotMntPoint := shared.VarPath("snapshots", cs)
			newSnapshotMntPoint := driver.GetSnapshotMountPoint("default", defaultPoolName, cs)
			if shared.PathExists(oldSnapshotMntPoint) && !shared.PathExists(newSnapshotMntPoint) {
				err = btrfsSnapshot(d.State(), oldSnapshotMntPoint, newSnapshotMntPoint, true)
				if err != nil {
					err := btrfsSubVolumeCreate(newSnapshotMntPoint)
					if err != nil {
						return err
					}

					output, err := rsync.LocalCopy(oldSnapshotMntPoint, newSnapshotMntPoint, "", true)
					if err != nil {
						logger.Errorf("Failed to rsync: %s: %s", output, err)
						return err
					}

					btrfsSubVolumesDelete(oldSnapshotMntPoint)
					if shared.PathExists(oldSnapshotMntPoint) {
						err = os.RemoveAll(oldSnapshotMntPoint)
						if err != nil {
							return err
						}
					}
				} else {
					// Delete the old subvolume.
					err = btrfsSubVolumesDelete(oldSnapshotMntPoint)
					if err != nil {
						return err
					}
				}
			}
		}

		if len(ctSnapshots) > 0 {
			// Create a new symlink from the snapshots directory of
			// the container to the snapshots directory on the
			// storage pool:
			// ${LXD_DIR}/snapshots/<container_name> to ${LXD_DIR}/storage-pools/<pool>/snapshots/<container_name>
			snapshotsPath := shared.VarPath("snapshots", ct)
			newSnapshotMntPoint := driver.GetSnapshotMountPoint("default", defaultPoolName, ct)
			os.Remove(snapshotsPath)
			if !shared.PathExists(snapshotsPath) {
				err := os.Symlink(newSnapshotMntPoint, snapshotsPath)
				if err != nil {
					return err
				}
			}
		}
	}

	// Insert storage volumes for images into the database. Images don't
	// move. The tarballs remain in their original location.
	images := append(imgPublic, imgPrivate...)
	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, img, db.StoragePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image")
			err := d.cluster.UpdateStoragePoolVolume("default", img, db.StoragePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", img, "", db.StoragePoolVolumeTypeImage, poolID, imagePoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\"", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		imagesMntPoint := driver.GetImageMountPoint(defaultPoolName, "")
		if !shared.PathExists(imagesMntPoint) {
			err := os.MkdirAll(imagesMntPoint, 0700)
			if err != nil {
				return err
			}
		}

		oldImageMntPoint := shared.VarPath("images", img+".btrfs")
		newImageMntPoint := driver.GetImageMountPoint(defaultPoolName, img)
		if shared.PathExists(oldImageMntPoint) && !shared.PathExists(newImageMntPoint) {
			err := os.Rename(oldImageMntPoint, newImageMntPoint)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func upgradeFromStorageTypeDir(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	poolConfig["source"] = shared.VarPath("storage-pools", defaultPoolName)

	err := storagePoolValidateConfig(defaultPoolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(defaultPoolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	var poolID int64
	pools, err := d.cluster.GetStoragePoolNames()
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(defaultPoolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s", defaultPoolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, _, err := d.cluster.GetStoragePoolInAnyState(defaultPoolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = d.cluster.UpdateStoragePool(defaultPoolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.ErrNoSuchObject { // Likely a pristine upgrade.
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.State(), defaultPoolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			return err
		}
		poolID = tmp
		poolInfo := api.StoragePoolsPost{
			StoragePoolPut: api.StoragePoolPut{
				Config:      poolConfig,
				Description: "",
			},
			Name:   defaultPoolName,
			Driver: defaultStorageTypeName,
		}

		_, err = storagePools.CreatePool(d.State(), poolID, &poolInfo)
		if err != nil {
			return err
		}
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, _, err := d.cluster.GetStoragePoolInAnyState(defaultPoolName)
	if err != nil {
		return err
	}

	// Insert storage volumes for containers into the database.
	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, ct, db.StoragePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container")
			err := d.cluster.UpdateStoragePoolVolume("default", ct, db.StoragePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", ct, "", db.StoragePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\"", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Create the new path where containers will be located on the
		// new storage api.
		containersMntPoint := driver.GetContainerMountPoint("default", defaultPoolName, "")
		if !shared.PathExists(containersMntPoint) {
			err := os.MkdirAll(containersMntPoint, 0711)
			if err != nil {
				return err
			}
		}

		// Simply rename the container when they are directories.
		oldContainerMntPoint := shared.VarPath("containers", ct)
		newContainerMntPoint := driver.GetContainerMountPoint("default", defaultPoolName, ct)
		if shared.PathExists(oldContainerMntPoint) && !shared.PathExists(newContainerMntPoint) {
			// First try to rename.
			err := os.Rename(oldContainerMntPoint, newContainerMntPoint)
			if err != nil {
				output, err := rsync.LocalCopy(oldContainerMntPoint, newContainerMntPoint, "", true)
				if err != nil {
					logger.Errorf("Failed to rsync: %s: %s", output, err)
					return err
				}
				err = os.RemoveAll(oldContainerMntPoint)
				if err != nil {
					return err
				}
			}
		}

		doesntMatter := false
		err = driver.CreateContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			return err
		}

		// Check if we need to account for snapshots for this container.
		oldSnapshotMntPoint := shared.VarPath("snapshots", ct)
		if !shared.PathExists(oldSnapshotMntPoint) {
			continue
		}

		// If the snapshots directory for that container is empty,
		// remove it.
		isEmpty, _ := shared.PathIsEmpty(oldSnapshotMntPoint)
		if isEmpty {
			os.Remove(oldSnapshotMntPoint)
			continue
		}

		// Create the new path where snapshots will be located on the
		// new storage api.
		snapshotsMntPoint := shared.VarPath("storage-pools", defaultPoolName, "snapshots")
		if !shared.PathExists(snapshotsMntPoint) {
			err := os.MkdirAll(snapshotsMntPoint, 0711)
			if err != nil {
				return err
			}
		}

		// Now simply rename the snapshots directory as well.
		newSnapshotMntPoint := driver.GetSnapshotMountPoint("default", defaultPoolName, ct)
		if shared.PathExists(oldSnapshotMntPoint) && !shared.PathExists(newSnapshotMntPoint) {
			err := os.Rename(oldSnapshotMntPoint, newSnapshotMntPoint)
			if err != nil {
				output, err := rsync.LocalCopy(oldSnapshotMntPoint, newSnapshotMntPoint, "", true)
				if err != nil {
					logger.Errorf("Failed to rsync: %s: %s", output, err)
					return err
				}
				err = os.RemoveAll(oldSnapshotMntPoint)
				if err != nil {
					return err
				}
			}
		}

		// Create a symlink for this container.  snapshots.
		err = driver.CreateSnapshotMountpoint(newSnapshotMntPoint, newSnapshotMntPoint, oldSnapshotMntPoint)
		if err != nil {
			return err
		}
	}

	// Insert storage volumes for snapshots into the database. Note that
	// snapshots have already been moved and symlinked above. So no need to
	// do any work here.
	for _, cs := range cSnapshots {
		// Insert storage volumes for snapshots into the
		// database. Note that snapshots have already been moved
		// and symlinked above. So no need to do any work here.
		// Initialize empty storage volume configuration for the
		// container.
		snapshotPoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(snapshotPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, cs, db.StoragePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the snapshot")
			err := d.cluster.UpdateStoragePoolVolume("default", cs, db.StoragePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStorageVolumeSnapshot("default", cs, "", db.StoragePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig, time.Time{})
			if err != nil {
				logger.Errorf("Could not insert a storage volume for snapshot \"%s\"", cs)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
	}

	// Insert storage volumes for images into the database. Images don't
	// move. The tarballs remain in their original location.
	images := append(imgPublic, imgPrivate...)
	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, img, db.StoragePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image")
			err := d.cluster.UpdateStoragePoolVolume("default", img, db.StoragePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", img, "", db.StoragePoolVolumeTypeImage, poolID, imagePoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\"", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
	}

	return nil
}

func upgradeFromStorageTypeLvm(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	poolConfig["source"] = defaultPoolName

	// Set it only if it is not the default value.
	var daemonConfig map[string]string
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		daemonConfig, err = tx.Config()
		return err
	})
	if err != nil {
		return err
	}
	fsType := daemonConfig["storage.lvm_fstype"]
	if fsType != "" && fsType != "ext4" {
		poolConfig["volume.block.filesystem"] = fsType
	}

	// Set it only if it is not the default value.
	fsMntOpts := daemonConfig["storage.lvm_mount_options"]
	if fsMntOpts != "" && fsMntOpts != "discard" {
		poolConfig["volume.block.mount_options"] = fsMntOpts
	}

	poolConfig["lvm.thinpool_name"] = daemonConfig["storage.lvm_thinpool_name"]
	if poolConfig["lvm.thinpool_name"] == "" {
		// If empty we need to set it to the old default.
		poolConfig["lvm.thinpool_name"] = "LXDThinPool"
	}

	poolConfig["lvm.vg_name"] = daemonConfig["storage.lvm_vg_name"]

	poolConfig["volume.size"] = daemonConfig["storage.lvm_volume_size"]
	if poolConfig["volume.size"] != "" {
		// In case stuff like GiB is used which
		// share.dParseByteSizeString() doesn't handle.
		if strings.Contains(poolConfig["volume.size"], "i") {
			poolConfig["volume.size"] = strings.Replace(poolConfig["volume.size"], "i", "", 1)
		}
	}
	// On previous upgrade versions, "size" was set instead of
	// "volume.size", so unset it.
	poolConfig["size"] = ""

	err = storagePoolValidateConfig(defaultPoolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(defaultPoolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	// Activate volume group
	err = lvmVGActivate(defaultPoolName)
	if err != nil {
		logger.Errorf("Could not activate volume group \"%s\". Manual intervention needed", defaultPoolName)
		return err
	}

	// Peek into the storage pool database to see whether any storage pools
	// are already configured. If so, we can assume that a partial upgrade
	// has been performed and can skip the next steps.
	var poolID int64
	pools, err := d.cluster.GetStoragePoolNames()
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(defaultPoolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s", defaultPoolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, _, err := d.cluster.GetStoragePoolInAnyState(defaultPoolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = d.cluster.UpdateStoragePool(defaultPoolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.ErrNoSuchObject { // Likely a pristine upgrade.
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.State(), defaultPoolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			return err
		}
		poolID = tmp
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	// Create pool mountpoint if it doesn't already exist.
	poolMntPoint := driver.GetStoragePoolMountPoint(defaultPoolName)
	if !shared.PathExists(poolMntPoint) {
		err = os.MkdirAll(poolMntPoint, 0711)
		if err != nil {
			logger.Warnf("Failed to create pool mountpoint: %s", poolMntPoint)
		}
	}

	if len(cRegular) > 0 {
		// Create generic containers folder on the storage pool.
		newContainersMntPoint := driver.GetContainerMountPoint("default", defaultPoolName, "")
		if !shared.PathExists(newContainersMntPoint) {
			err = os.MkdirAll(newContainersMntPoint, 0711)
			if err != nil {
				logger.Warnf("Failed to create containers mountpoint: %s", newContainersMntPoint)
			}
		}
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, _, err := d.cluster.GetStoragePoolInAnyState(defaultPoolName)
	if err != nil {
		return err
	}

	// Insert storage volumes for containers into the database.
	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, ct, db.StoragePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container")
			err := d.cluster.UpdateStoragePoolVolume("default", ct, db.StoragePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", ct, "", db.StoragePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\"", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Unmount the logical volume.
		oldContainerMntPoint := shared.VarPath("containers", ct)
		if shared.IsMountPoint(oldContainerMntPoint) {
			err := storageDrivers.TryUnmount(oldContainerMntPoint, unix.MNT_DETACH)
			if err != nil {
				logger.Errorf("Failed to unmount LVM logical volume \"%s\": %s", oldContainerMntPoint, err)
				return err
			}
		}

		// Create the new path where containers will be located on the
		// new storage api. We do os.Rename() here to preserve
		// permissions and ownership.
		newContainerMntPoint := driver.GetContainerMountPoint("default", defaultPoolName, ct)
		ctLvName := lvmNameToLVName(ct)
		newContainerLvName := fmt.Sprintf("%s_%s", patchStoragePoolVolumeAPIEndpointContainers, ctLvName)
		containerLvDevPath := lvmDevPath("default", defaultPoolName, patchStoragePoolVolumeAPIEndpointContainers, ctLvName)
		if !shared.PathExists(containerLvDevPath) {
			oldLvDevPath := fmt.Sprintf("/dev/%s/%s", defaultPoolName, ctLvName)
			// If the old LVM device path for the logical volume
			// exists we call lvrename. Otherwise this is likely a
			// mixed-storage LXD instance which we need to deal
			// with.
			if shared.PathExists(oldLvDevPath) {
				// Rename the logical volume mountpoint.
				if shared.PathExists(oldContainerMntPoint) && !shared.PathExists(newContainerMntPoint) {
					err = os.Rename(oldContainerMntPoint, newContainerMntPoint)
					if err != nil {
						logger.Errorf("Failed to rename LVM container mountpoint from %s to %s: %s", oldContainerMntPoint, newContainerMntPoint, err)
						return err
					}
				}

				// Remove the old container mountpoint.
				if shared.PathExists(oldContainerMntPoint + ".lv") {
					err := os.Remove(oldContainerMntPoint + ".lv")
					if err != nil {
						logger.Errorf("Failed to remove old LVM container mountpoint %s.lv: %s", oldContainerMntPoint, err)
						return err
					}
				}

				// Rename the logical volume.
				msg, err := shared.TryRunCommand("lvrename", defaultPoolName, ctLvName, newContainerLvName)
				if err != nil {
					logger.Errorf("Failed to rename LVM logical volume from %s to %s: %s", ctLvName, newContainerLvName, msg)
					return err
				}
			} else if shared.PathExists(oldContainerMntPoint) && shared.IsDir(oldContainerMntPoint) {
				// This is a directory backed container and it
				// means that this was a mixed-storage LXD
				// instance.

				// Load the container from the database.
				ctStruct, err := instance.LoadByProjectAndName(d.State(), "default", ct)
				if err != nil {
					logger.Errorf("Failed to load LVM container %s: %s", ct, err)
					return err
				}

				pool, err := storagePools.GetPoolByInstance(d.State(), ctStruct)
				if err != nil {
					return err
				}

				// Create an empty LVM logical volume for the container.
				err = pool.CreateInstance(ctStruct, nil)
				if err != nil {
					logger.Errorf("Failed to create empty LVM logical volume for container %s: %s", ct, err)
					return err
				}

				err = func() error {
					// In case the new LVM logical volume for the container is not mounted mount it.
					if !shared.IsMountPoint(newContainerMntPoint) {
						_, err = pool.MountInstance(ctStruct, nil)
						if err != nil {
							logger.Errorf("Failed to mount new empty LVM logical volume for container %s: %s", ct, err)
							return err
						}
						defer pool.UnmountInstance(ctStruct, nil)
					}

					// Use rsync to fill the empty volume.
					output, err := rsync.LocalCopy(oldContainerMntPoint, newContainerMntPoint, "", true)
					if err != nil {
						pool.DeleteInstance(ctStruct, nil)
						return fmt.Errorf("rsync failed: %s", string(output))
					}

					return nil
				}()
				if err != nil {
					return err
				}

				// Remove the old container.
				err = os.RemoveAll(oldContainerMntPoint)
				if err != nil {
					logger.Errorf("Failed to remove old container %s: %s", oldContainerMntPoint, err)
					return err
				}
			}
		}

		// Create the new container mountpoint.
		doesntMatter := false
		err = driver.CreateContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			logger.Errorf("Failed to create container mountpoint \"%s\" for LVM logical volume: %s", newContainerMntPoint, err)
			return err
		}

		// Guaranteed to be set.
		lvFsType := containerPoolVolumeConfig["block.filesystem"]
		mountOptions := containerPoolVolumeConfig["block.mount_options"]
		if mountOptions == "" {
			// Set to default.
			mountOptions = "discard"
		}

		// Check if we need to account for snapshots for this container.
		ctSnapshots, err := d.cluster.GetInstanceSnapshotsNames("default", ct)
		if err != nil {
			return err
		}

		for _, cs := range ctSnapshots {
			// Insert storage volumes for snapshots into the
			// database. Note that snapshots have already been moved
			// and symlinked above. So no need to do any work here.
			// Initialize empty storage volume configuration for the
			// container.
			snapshotPoolVolumeConfig := map[string]string{}
			err = volumeFillDefault(snapshotPoolVolumeConfig, defaultPool)
			if err != nil {
				return err
			}

			_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, cs, db.StoragePoolVolumeTypeContainer, poolID)
			if err == nil {
				logger.Warnf("Storage volumes database already contains an entry for the snapshot")
				err := d.cluster.UpdateStoragePoolVolume("default", cs, db.StoragePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
				if err != nil {
					return err
				}
			} else if err == db.ErrNoSuchObject {
				// Insert storage volumes for containers into the database.
				_, err := d.cluster.CreateStorageVolumeSnapshot("default", cs, "", db.StoragePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig, time.Time{})
				if err != nil {
					logger.Errorf("Could not insert a storage volume for snapshot \"%s\"", cs)
					return err
				}
			} else {
				logger.Errorf("Failed to query database: %s", err)
				return err
			}

			// Create the snapshots directory in the new storage
			// pool:
			// ${LXD_DIR}/storage-pools/<pool>/snapshots
			newSnapshotMntPoint := driver.GetSnapshotMountPoint("default", defaultPoolName, cs)
			if !shared.PathExists(newSnapshotMntPoint) {
				err := os.MkdirAll(newSnapshotMntPoint, 0700)
				if err != nil {
					return err
				}
			}

			oldSnapshotMntPoint := shared.VarPath("snapshots", cs)
			os.Remove(oldSnapshotMntPoint + ".lv")

			// Make sure we use a valid lv name.
			csLvName := lvmNameToLVName(cs)
			newSnapshotLvName := fmt.Sprintf("%s_%s", patchStoragePoolVolumeAPIEndpointContainers, csLvName)
			snapshotLvDevPath := lvmDevPath("default", defaultPoolName, patchStoragePoolVolumeAPIEndpointContainers, csLvName)
			if !shared.PathExists(snapshotLvDevPath) {
				oldLvDevPath := fmt.Sprintf("/dev/%s/%s", defaultPoolName, csLvName)
				if shared.PathExists(oldLvDevPath) {
					// Unmount the logical volume.
					if shared.IsMountPoint(oldSnapshotMntPoint) {
						err := storageDrivers.TryUnmount(oldSnapshotMntPoint, unix.MNT_DETACH)
						if err != nil {
							logger.Errorf("Failed to unmount LVM logical volume \"%s\": %s", oldSnapshotMntPoint, err)
							return err
						}
					}

					// Rename the snapshot mountpoint to preserve acl's and
					// so on.
					if shared.PathExists(oldSnapshotMntPoint) && !shared.PathExists(newSnapshotMntPoint) {
						err := os.Rename(oldSnapshotMntPoint, newSnapshotMntPoint)
						if err != nil {
							logger.Errorf("Failed to rename LVM container mountpoint from %s to %s: %s", oldSnapshotMntPoint, newSnapshotMntPoint, err)
							return err
						}
					}

					// Rename the logical volume.
					msg, err := shared.TryRunCommand("lvrename", defaultPoolName, csLvName, newSnapshotLvName)
					if err != nil {
						logger.Errorf("Failed to rename LVM logical volume from %s to %s: %s", csLvName, newSnapshotLvName, msg)
						return err
					}
				} else if shared.PathExists(oldSnapshotMntPoint) && shared.IsDir(oldSnapshotMntPoint) {
					// This is a directory backed container
					// and it means that this was a
					// mixed-storage LXD instance.

					// Load the snapshot from the database.
					csStruct, err := instance.LoadByProjectAndName(d.State(), "default", cs)
					if err != nil {
						logger.Errorf("Failed to load LVM container %s: %s", cs, err)
						return err
					}

					pool, err := storagePools.GetPoolByInstance(d.State(), csStruct)
					if err != nil {
						return err
					}

					parent, _, _ := shared.InstanceGetParentAndSnapshotName(csStruct.Name())
					parentInst, err := instance.LoadByProjectAndName(d.State(), csStruct.Project(), parent)
					if err != nil {
						logger.Errorf("Failed to load parent LVM container %s: %s", cs, err)
						return err
					}

					// Create an empty LVM logical volume for the snapshot.
					err = pool.CreateInstanceSnapshot(csStruct, parentInst, nil)
					if err != nil {
						logger.Errorf("Failed to create LVM logical volume snapshot for container %s: %s", cs, err)
						return err
					}

					err = func() error {
						// In case the new LVM logical volume for the snapshot is not mounted mount it.
						if !shared.IsMountPoint(newSnapshotMntPoint) {
							_, err = pool.MountInstanceSnapshot(csStruct, nil)
							if err != nil {
								logger.Errorf("Failed to mount new empty LVM logical volume for container %s: %s", cs, err)
								return err
							}
							defer pool.UnmountInstanceSnapshot(csStruct, nil)
						}

						// Use rsync to fill the snapshot volume.
						output, err := rsync.LocalCopy(oldSnapshotMntPoint, newSnapshotMntPoint, "", true)
						if err != nil {
							pool.DeleteInstanceSnapshot(csStruct, nil)
							return fmt.Errorf("rsync failed: %s", string(output))
						}

						// Remove the old snapshot.
						err = os.RemoveAll(oldSnapshotMntPoint)
						if err != nil {
							logger.Errorf("Failed to remove old container %s: %s", oldSnapshotMntPoint, err)
							return err
						}

						return nil
					}()
					if err != nil {
						return err
					}
				}
			}
		}

		if len(ctSnapshots) > 0 {
			// Create a new symlink from the snapshots directory of
			// the container to the snapshots directory on the
			// storage pool:
			// ${LXD_DIR}/snapshots/<container_name> to ${LXD_DIR}/storage-pools/<pool>/snapshots/<container_name>
			snapshotsPath := shared.VarPath("snapshots", ct)
			newSnapshotsPath := driver.GetSnapshotMountPoint("default", defaultPoolName, ct)
			if shared.PathExists(snapshotsPath) {
				// On a broken update snapshotsPath will contain
				// empty directories that need to be removed.
				err := os.RemoveAll(snapshotsPath)
				if err != nil {
					return err
				}
			}
			if !shared.PathExists(snapshotsPath) {
				err = os.Symlink(newSnapshotsPath, snapshotsPath)
				if err != nil {
					return err
				}
			}
		}

		if !shared.IsMountPoint(newContainerMntPoint) {
			err := storageDrivers.TryMount(containerLvDevPath, newContainerMntPoint, lvFsType, 0, mountOptions)
			if err != nil {
				logger.Errorf("Failed to mount LVM logical \"%s\" onto \"%s\" : %s", containerLvDevPath, newContainerMntPoint, err)
				return err
			}
		}
	}

	images := append(imgPublic, imgPrivate...)
	if len(images) > 0 {
		imagesMntPoint := driver.GetImageMountPoint(defaultPoolName, "")
		if !shared.PathExists(imagesMntPoint) {
			err := os.MkdirAll(imagesMntPoint, 0700)
			if err != nil {
				return err
			}
		}
	}

	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, img, db.StoragePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image")
			err := d.cluster.UpdateStoragePoolVolume("default", img, db.StoragePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", img, "", db.StoragePoolVolumeTypeImage, poolID, imagePoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\"", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Unmount the logical volume.
		oldImageMntPoint := shared.VarPath("images", img+".lv")
		if shared.IsMountPoint(oldImageMntPoint) {
			err := storageDrivers.TryUnmount(oldImageMntPoint, unix.MNT_DETACH)
			if err != nil {
				return err
			}
		}

		if shared.PathExists(oldImageMntPoint) {
			err := os.Remove(oldImageMntPoint)
			if err != nil {
				return err
			}
		}

		newImageMntPoint := driver.GetImageMountPoint(defaultPoolName, img)
		if !shared.PathExists(newImageMntPoint) {
			err := os.MkdirAll(newImageMntPoint, 0700)
			if err != nil {
				return err
			}
		}

		// Rename the logical volume device.
		newImageLvName := fmt.Sprintf("%s_%s", patchStoragePoolVolumeAPIEndpointImages, img)
		imageLvDevPath := lvmDevPath("default", defaultPoolName, patchStoragePoolVolumeAPIEndpointImages, img)
		oldLvDevPath := fmt.Sprintf("/dev/%s/%s", defaultPoolName, img)
		// Only create logical volumes for images that have a logical
		// volume on the pre-storage-api LXD instance. If not, we don't
		// care since LXD will create a logical volume on demand.
		if !shared.PathExists(imageLvDevPath) && shared.PathExists(oldLvDevPath) {
			_, err := shared.TryRunCommand("lvrename", defaultPoolName, img, newImageLvName)
			if err != nil {
				return err
			}
		}

		if !shared.PathExists(imageLvDevPath) {
			// This image didn't exist as a logical volume on the
			// old LXD instance so we need to kick it from the
			// storage volumes database for this pool.
			err := d.cluster.RemoveStoragePoolVolume("default", img, db.StoragePoolVolumeTypeImage, poolID)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func upgradeFromStorageTypeZfs(name string, d *Daemon, defaultPoolName string, defaultStorageTypeName string, cRegular []string, cSnapshots []string, imgPublic []string, imgPrivate []string) error {
	poolConfig := map[string]string{}
	oldLoopFilePath := shared.VarPath("zfs.img")
	poolName := defaultPoolName
	// In case we are given a dataset, we need to chose a sensible name.
	if strings.Contains(defaultPoolName, "/") {
		// We are given a dataset and need to chose a sensible name.
		poolName = "default"
	}

	// Peek into the storage pool database to see whether any storage pools
	// are already configured. If so, we can assume that a partial upgrade
	// has been performed and can skip the next steps. Otherwise we might
	// run into problems. For example, the "zfs.img" file might have already
	// been moved into ${LXD_DIR}/disks and we might therefore falsely
	// conclude that we're using an existing storage pool.
	err := storagePoolValidateConfig(poolName, defaultStorageTypeName, poolConfig, nil)
	if err != nil {
		return err
	}

	err = storagePoolFillDefault(poolName, defaultStorageTypeName, poolConfig)
	if err != nil {
		return err
	}

	// Peek into the storage pool database to see whether any storage pools
	// are already configured. If so, we can assume that a partial upgrade
	// has been performed and can skip the next steps.
	var poolID int64
	pools, err := d.cluster.GetStoragePoolNames()
	if err == nil { // Already exist valid storage pools.
		// Check if the storage pool already has a db entry.
		if shared.StringInSlice(poolName, pools) {
			logger.Warnf("Database already contains a valid entry for the storage pool: %s", poolName)
		}

		// Get the pool ID as we need it for storage volume creation.
		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}
		poolID = tmp

		// Update the pool configuration on a post LXD 2.9.1 instance
		// that still runs this upgrade code because of a partial
		// upgrade.
		if pool.Config == nil {
			pool.Config = poolConfig
		}
		err = d.cluster.UpdateStoragePool(poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	} else if err == db.ErrNoSuchObject { // Likely a pristine upgrade.
		poolConfig["zfs.pool_name"] = defaultPoolName
		if shared.PathExists(oldLoopFilePath) {
			// This is a loop file pool.
			poolConfig["source"] = shared.VarPath("disks", poolName+".img")
			err := shared.FileMove(oldLoopFilePath, poolConfig["source"])
			if err != nil {
				return err
			}
		} else {
			// This is a block device pool.
			// Here, we need to use "defaultPoolName" since we want
			// to refer to the on-disk name of the pool in the
			// "source" propert and not the db name of the pool.
			poolConfig["source"] = defaultPoolName
		}

		// Querying the size of a storage pool only makes sense when it
		// is not a dataset.
		if poolName == defaultPoolName {
			output, err := shared.RunCommand("zpool", "get", "size", "-p", "-H", defaultPoolName)
			if err == nil {
				lidx := strings.LastIndex(output, "\t")
				fidx := strings.LastIndex(output[:lidx-1], "\t")
				poolConfig["size"] = output[fidx+1 : lidx]
			}
		}

		// (Use a tmp variable as Go's scoping is freaking me out.)
		tmp, err := dbStoragePoolCreateAndUpdateCache(d.State(), poolName, "", defaultStorageTypeName, poolConfig)
		if err != nil {
			logger.Warnf("Storage pool already exists in the database, proceeding...")
		}
		poolID = tmp
	} else { // Shouldn't happen.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	// Get storage pool from the db after having updated it above.
	_, defaultPool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
	if err != nil {
		return err
	}

	if len(cRegular) > 0 {
		containersSubvolumePath := driver.GetContainerMountPoint("default", poolName, "")
		if !shared.PathExists(containersSubvolumePath) {
			err := os.MkdirAll(containersSubvolumePath, 0711)
			if err != nil {
				logger.Warnf("Failed to create path: %s", containersSubvolumePath)
			}
		}
	}

	failedUpgradeEntities := []string{}
	for _, ct := range cRegular {
		// Initialize empty storage volume configuration for the
		// container.
		containerPoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(containerPoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, ct, db.StoragePoolVolumeTypeContainer, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the container")
			err := d.cluster.UpdateStoragePoolVolume("default", ct, db.StoragePoolVolumeTypeContainer, poolID, "", containerPoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", ct, "", db.StoragePoolVolumeTypeContainer, poolID, containerPoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for container \"%s\"", ct)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Unmount the container zfs doesn't really seem to care if we
		// do this.
		// Here "defaultPoolName" must be used since we want to refer to
		// the on-disk name of the zfs pool when moving the datasets
		// around.
		ctDataset := fmt.Sprintf("%s/containers/%s", defaultPoolName, ct)
		oldContainerMntPoint := shared.VarPath("containers", ct)
		if shared.IsMountPoint(oldContainerMntPoint) {
			_, err := shared.TryRunCommand("zfs", "unmount", "-f", ctDataset)
			if err != nil {
				logger.Warnf("Failed to unmount ZFS filesystem via zfs unmount, trying lazy umount (MNT_DETACH)...")
				err := storageDrivers.TryUnmount(oldContainerMntPoint, unix.MNT_DETACH)
				if err != nil {
					failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("containers/%s: Failed to umount zfs filesystem.", ct))
					continue
				}
			}
		}

		os.Remove(oldContainerMntPoint)

		os.Remove(oldContainerMntPoint + ".zfs")

		// Changing the mountpoint property should have actually created
		// the path but in case it somehow didn't let's do it ourselves.
		doesntMatter := false
		newContainerMntPoint := driver.GetContainerMountPoint("default", poolName, ct)
		err = driver.CreateContainerMountpoint(newContainerMntPoint, oldContainerMntPoint, doesntMatter)
		if err != nil {
			logger.Warnf("Failed to create mountpoint for the container: %s", newContainerMntPoint)
			failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("containers/%s: Failed to create container mountpoint: %s", ct, err))
			continue
		}

		// Set new mountpoint for the container's dataset it will be
		// automatically mounted.
		output, err := shared.RunCommand(
			"zfs",
			"set",
			fmt.Sprintf("mountpoint=%s", newContainerMntPoint),
			ctDataset)
		if err != nil {
			logger.Warnf("Failed to set new ZFS mountpoint: %s", output)
			failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("containers/%s: Failed to set new zfs mountpoint: %s", ct, err))
			continue
		}

		// Check if we need to account for snapshots for this container.
		ctSnapshots, err := d.cluster.GetInstanceSnapshotsNames("default", ct)
		if err != nil {
			logger.Errorf("Failed to query database")
			return err
		}

		snapshotsPath := shared.VarPath("snapshots", ct)
		for _, cs := range ctSnapshots {
			// Insert storage volumes for snapshots into the
			// database. Note that snapshots have already been moved
			// and symlinked above. So no need to do any work here.
			// Initialize empty storage volume configuration for the
			// container.
			snapshotPoolVolumeConfig := map[string]string{}
			err = volumeFillDefault(snapshotPoolVolumeConfig, defaultPool)
			if err != nil {
				return err
			}

			_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, cs, db.StoragePoolVolumeTypeContainer, poolID)
			if err == nil {
				logger.Warnf("Storage volumes database already contains an entry for the snapshot")
				err := d.cluster.UpdateStoragePoolVolume("default", cs, db.StoragePoolVolumeTypeContainer, poolID, "", snapshotPoolVolumeConfig)
				if err != nil {
					return err
				}
			} else if err == db.ErrNoSuchObject {
				// Insert storage volumes for containers into the database.
				_, err := d.cluster.CreateStorageVolumeSnapshot("default", cs, "", db.StoragePoolVolumeTypeContainer, poolID, snapshotPoolVolumeConfig, time.Time{})
				if err != nil {
					logger.Errorf("Could not insert a storage volume for snapshot \"%s\"", cs)
					return err
				}
			} else {
				logger.Errorf("Failed to query database: %s", err)
				return err
			}

			// Create the new mountpoint for snapshots in the new
			// storage api.
			newSnapshotMntPoint := driver.GetSnapshotMountPoint("default", poolName, cs)
			if !shared.PathExists(newSnapshotMntPoint) {
				err = os.MkdirAll(newSnapshotMntPoint, 0711)
				if err != nil {
					logger.Warnf("Failed to create mountpoint for snapshot: %s", newSnapshotMntPoint)
					failedUpgradeEntities = append(failedUpgradeEntities, fmt.Sprintf("snapshots/%s: Failed to create mountpoint for snapshot.", cs))
					continue
				}
			}
		}

		os.RemoveAll(snapshotsPath)

		// Create a symlink for this container's snapshots.
		if len(ctSnapshots) != 0 {
			newSnapshotsMntPoint := driver.GetSnapshotMountPoint("default", poolName, ct)
			if !shared.PathExists(newSnapshotsMntPoint) {
				err := os.Symlink(newSnapshotsMntPoint, snapshotsPath)
				if err != nil {
					logger.Warnf("Failed to create symlink for snapshots: %s to %s", snapshotsPath, newSnapshotsMntPoint)
				}
			}
		}
	}

	// Insert storage volumes for images into the database. Images don't
	// move. The tarballs remain in their original location.
	images := append(imgPublic, imgPrivate...)
	for _, img := range images {
		imagePoolVolumeConfig := map[string]string{}
		err = volumeFillDefault(imagePoolVolumeConfig, defaultPool)
		if err != nil {
			return err
		}

		_, err = d.cluster.GetStoragePoolNodeVolumeID(project.Default, img, db.StoragePoolVolumeTypeImage, poolID)
		if err == nil {
			logger.Warnf("Storage volumes database already contains an entry for the image")
			err := d.cluster.UpdateStoragePoolVolume("default", img, db.StoragePoolVolumeTypeImage, poolID, "", imagePoolVolumeConfig)
			if err != nil {
				return err
			}
		} else if err == db.ErrNoSuchObject {
			// Insert storage volumes for containers into the database.
			_, err := d.cluster.CreateStoragePoolVolume("default", img, "", db.StoragePoolVolumeTypeImage, poolID, imagePoolVolumeConfig, db.StoragePoolVolumeContentTypeFS)
			if err != nil {
				logger.Errorf("Could not insert a storage volume for image \"%s\"", img)
				return err
			}
		} else {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		imageMntPoint := driver.GetImageMountPoint(poolName, img)
		if !shared.PathExists(imageMntPoint) {
			err := os.MkdirAll(imageMntPoint, 0700)
			if err != nil {
				logger.Warnf("Failed to create image mountpoint, proceeding...")
			}
		}

		oldImageMntPoint := shared.VarPath("images", img+".zfs")
		// Here "defaultPoolName" must be used since we want to refer to
		// the on-disk name of the zfs pool when moving the datasets
		// around.
		imageDataset := fmt.Sprintf("%s/images/%s", defaultPoolName, img)
		if shared.PathExists(oldImageMntPoint) && shared.IsMountPoint(oldImageMntPoint) {
			_, err := shared.TryRunCommand("zfs", "unmount", "-f", imageDataset)
			if err != nil {
				logger.Warnf("Failed to unmount ZFS filesystem via zfs unmount, trying lazy umount (MNT_DETACH)...")
				err := storageDrivers.TryUnmount(oldImageMntPoint, unix.MNT_DETACH)
				if err != nil {
					logger.Warnf("Failed to unmount ZFS filesystem: %s", err)
				}
			}

			os.Remove(oldImageMntPoint)
		}

		// Set new mountpoint for the container's dataset it will be
		// automatically mounted.
		output, err := shared.RunCommand("zfs", "set", "mountpoint=none", imageDataset)
		if err != nil {
			logger.Warnf("Failed to set new ZFS mountpoint: %s", output)
		}
	}

	var finalErr error
	if len(failedUpgradeEntities) > 0 {
		finalErr = fmt.Errorf(strings.Join(failedUpgradeEntities, "\n"))
	}

	return finalErr
}

func updatePoolPropertyForAllObjects(d *Daemon, poolName string, allcontainers []string) error {
	// The new storage api enforces that the default storage pool on which
	// containers are created is set in the default profile. If it isn't
	// set, then LXD will refuse to create a container until either an
	// appropriate device including a pool is added to the default profile
	// or the user explicitly passes the pool the container's storage volume
	// is supposed to be created on.
	profiles, err := d.cluster.GetProfileNames("default")
	if err == nil {
		for _, pName := range profiles {
			_, p, err := d.cluster.GetProfile("default", pName)
			if err != nil {
				logger.Errorf("Could not query database: %s", err)
				return err
			}

			// Check for a root disk device entry
			k, _, _ := shared.GetRootDiskDevice(p.Devices)
			if k != "" {
				if p.Devices[k]["pool"] != "" {
					continue
				}

				p.Devices[k]["pool"] = poolName
			} else if k == "" && pName == "default" {
				// The default profile should have a valid root
				// disk device entry.
				rootDev := map[string]string{}
				rootDev["type"] = "disk"
				rootDev["path"] = "/"
				rootDev["pool"] = poolName
				if p.Devices == nil {
					p.Devices = map[string]map[string]string{}
				}

				// Make sure that we do not overwrite a device the user
				// is currently using under the name "root".
				rootDevName := "root"
				for i := 0; i < 100; i++ {
					if p.Devices[rootDevName] == nil {
						break
					}
					rootDevName = fmt.Sprintf("root%d", i)
					continue
				}
				p.Devices["root"] = rootDev
			}

			// This is nasty, but we need to clear the profiles config and
			// devices in order to add the new root device including the
			// newly added storage pool.
			err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
				return tx.UpdateProfile("default", pName, db.Profile{
					Project: "default",
					Name:    pName,
					Config:  p.Config,
					Devices: p.Devices,
				})
			})
			if err != nil {
				logger.Errorf("Failed to update old configuration for profile %s: %s", pName, err)
				return err
			}
		}
	}

	// Make sure all containers and snapshots have a valid disk configuration
	for _, ct := range allcontainers {
		c, err := instance.LoadByProjectAndName(d.State(), "default", ct)
		if err != nil {
			continue
		}

		args := db.InstanceArgs{
			Architecture: c.Architecture(),
			Config:       c.LocalConfig(),
			Description:  c.Description(),
			Ephemeral:    c.IsEphemeral(),
			Profiles:     c.Profiles(),
			Type:         c.Type(),
			Snapshot:     c.IsSnapshot(),
		}

		// Check if the container already has a valid root device entry (profile or previous upgrade)
		expandedDevices := c.ExpandedDevices()
		k, d, _ := shared.GetRootDiskDevice(expandedDevices.CloneNative())
		if k != "" && d["pool"] != "" {
			continue
		}

		// Look for a local root device entry
		localDevices := c.LocalDevices()
		k, _, _ = shared.GetRootDiskDevice(localDevices.CloneNative())
		if k != "" {
			localDevices[k]["pool"] = poolName
		} else {
			rootDev := map[string]string{}
			rootDev["type"] = "disk"
			rootDev["path"] = "/"
			rootDev["pool"] = poolName

			// Make sure that we do not overwrite a device the user
			// is currently using under the name "root".
			rootDevName := "root"
			for i := 0; i < 100; i++ {
				if expandedDevices[rootDevName] == nil {
					break
				}

				rootDevName = fmt.Sprintf("root%d", i)
				continue
			}

			localDevices[rootDevName] = rootDev
		}
		args.Devices = localDevices

		err = c.Update(args, false)
		if err != nil {
			logger.Warnf("Unable to add pool name to '%s': %v", c.Name(), err)
			continue
		}
	}

	return nil
}

func patchStorageApiV1(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil && err == db.ErrNoSuchObject {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	if len(pools) != 1 {
		logger.Warnf("More than one storage pool found. Not rerunning upgrade")
		return nil
	}

	cRegular, err := d.cluster.LegacyContainersList()
	if err != nil {
		return err
	}

	// Get list of existing snapshots.
	cSnapshots, err := d.cluster.LegacySnapshotsList()
	if err != nil {
		return err
	}

	allcontainers := append(cRegular, cSnapshots...)
	err = updatePoolPropertyForAllObjects(d, pools[0], allcontainers)
	if err != nil {
		return err
	}

	return nil
}

func patchContainerConfigRegen(name string, d *Daemon) error {
	cts, err := d.cluster.LegacyContainersList()
	if err != nil {
		return err
	}

	for _, ct := range cts {
		// Load the container from the database.
		inst, err := instance.LoadByProjectAndName(d.State(), "default", ct)
		if err != nil {
			logger.Errorf("Failed to open container '%s': %v", ct, err)
			continue
		}

		if inst.Type() != instancetype.Container {
			continue
		}

		if !inst.IsRunning() {
			continue
		}

		err = inst.SaveConfigFile()
		if err != nil {
			logger.Errorf("Failed to save LXC config for %q: %v", inst.Name(), err)
		}
	}

	return nil
}

// The lvm.thinpool_name and lvm.vg_name config keys are node-specific and need
// to be linked to nodes.
func patchLvmNodeSpecificConfigKeys(name string, d *Daemon) error {
	tx, err := d.cluster.Begin()
	if err != nil {
		return errors.Wrap(err, "failed to begin transaction")
	}

	// Fetch the IDs of all existing nodes.
	nodeIDs, err := query.SelectIntegers(tx, "SELECT id FROM nodes")
	if err != nil {
		return errors.Wrap(err, "failed to get IDs of current nodes")
	}

	// Fetch the IDs of all existing lvm pools.
	poolIDs, err := query.SelectIntegers(tx, "SELECT id FROM storage_pools WHERE driver='lvm'")
	if err != nil {
		return errors.Wrap(err, "failed to get IDs of current lvm pools")
	}

	for _, poolID := range poolIDs {
		// Fetch the config for this lvm pool and check if it has the
		// lvn.thinpool_name or lvm.vg_name keys.
		config, err := query.SelectConfig(
			tx, "storage_pools_config", "storage_pool_id=? AND node_id IS NULL", poolID)
		if err != nil {
			return errors.Wrap(err, "failed to fetch of lvm pool config")
		}

		for _, key := range []string{"lvm.thinpool_name", "lvm.vg_name"} {
			value, ok := config[key]
			if !ok {
				continue
			}

			// Delete the current key
			_, err = tx.Exec(`
DELETE FROM storage_pools_config WHERE key=? AND storage_pool_id=? AND node_id IS NULL
`, key, poolID)
			if err != nil {
				return errors.Wrapf(err, "failed to delete %s config", key)
			}

			// Add the config entry for each node
			for _, nodeID := range nodeIDs {
				_, err := tx.Exec(`
INSERT INTO storage_pools_config(storage_pool_id, node_id, key, value)
  VALUES(?, ?, ?, ?)
`, poolID, nodeID, key, value)
				if err != nil {
					return errors.Wrapf(err, "failed to create %s node config", key)
				}
			}
		}
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "failed to commit transaction")
	}

	return err
}

func patchStorageApiDirCleanup(name string, d *Daemon) error {
	fingerprints, err := d.cluster.GetImagesFingerprints("default", false)
	if err != nil {
		return err
	}
	return d.cluster.RemoveStorageVolumeImages(fingerprints)
}

func patchStorageApiLvmKeys(name string, d *Daemon) error {
	return d.cluster.UpgradeStorageVolumConfigToLVMThinPoolNameKey()
}

func patchStorageApiKeys(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil && err == db.ErrNoSuchObject {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about zfs and lvm.
		if pool.Driver != "zfs" && pool.Driver != "lvm" {
			continue
		}

		// This is a loop backed pool.
		if filepath.IsAbs(pool.Config["source"]) {
			continue
		}

		// Ensure that the source and the zfs.pool_name or lvm.vg_name
		// are lined up. After creation of the pool they should never
		// differ except in the loop backed case.
		if pool.Driver == "zfs" {
			pool.Config["zfs.pool_name"] = pool.Config["source"]
		} else if pool.Driver == "lvm" {
			// On previous upgrade versions, "size" was set instead
			// of "volume.size", so transfer the value and then
			// unset it.
			if pool.Config["size"] != "" {
				pool.Config["volume.size"] = pool.Config["size"]
				pool.Config["size"] = ""
			}
			pool.Config["lvm.vg_name"] = pool.Config["source"]
		}

		// Update the config in the database.
		err = d.cluster.UpdateStoragePool(poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}
	}

	return nil
}

// In case any of the objects images/containers/snapshots are missing storage
// volume configuration entries, let's add the defaults.
func patchStorageApiUpdateStorageConfigs(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		poolID, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Make sure that config is not empty.
		if pool.Config == nil {
			pool.Config = map[string]string{}
		}

		// Insert default values.
		err = storagePoolFillDefault(poolName, pool.Driver, pool.Config)
		if err != nil {
			return err
		}

		// Manually check for erroneously set keys.
		switch pool.Driver {
		case "btrfs":
			// Unset "size" property on non loop-backed pools.
			if pool.Config["size"] != "" {
				// Unset if either not an absolute path or not a
				// loop file.
				if !filepath.IsAbs(pool.Config["source"]) ||
					(filepath.IsAbs(pool.Config["source"]) &&
						!strings.HasSuffix(pool.Config["source"], ".img")) {
					pool.Config["size"] = ""
				}
			}
		case "dir":
			// Unset "size" property for all dir backed pools.
			if pool.Config["size"] != "" {
				pool.Config["size"] = ""
			}
		case "lvm":
			// Unset "size" property for volume-group level.
			if pool.Config["size"] != "" {
				pool.Config["size"] = ""
			}

			// Unset default values.
			if pool.Config["volume.block.mount_options"] == "discard" {
				pool.Config["volume.block.mount_options"] = ""
			}

			if pool.Config["volume.block.filesystem"] == "ext4" {
				pool.Config["volume.block.filesystem"] = ""
			}
		case "zfs":
			// Unset default values.
			if !shared.IsTrue(pool.Config["volume.zfs.use_refquota"]) {
				pool.Config["volume.zfs.use_refquota"] = ""
			}

			if !shared.IsTrue(pool.Config["volume.zfs.remove_snapshots"]) {
				pool.Config["volume.zfs.remove_snapshots"] = ""
			}

			// Unset "size" property on non loop-backed pools.
			if pool.Config["size"] != "" && !filepath.IsAbs(pool.Config["source"]) {
				pool.Config["size"] = ""
			}
		}

		// Update the storage pool config.
		err = d.cluster.UpdateStoragePool(poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}

		// Get all storage volumes on the storage pool.
		volumes, err := d.cluster.GetLocalStoragePoolVolumes("default", poolID, supportedVolumeTypes)
		if err != nil {
			if err == db.ErrNoSuchObject {
				continue
			}
			return err
		}

		for _, volume := range volumes {
			// Make sure that config is not empty.
			if volume.Config == nil {
				volume.Config = map[string]string{}
			}

			// Insert default values.
			err := volumeFillDefault(volume.Config, pool)
			if err != nil {
				return err
			}

			// Manually check for erroneously set keys.
			switch pool.Driver {
			case "btrfs":
				// Unset "size" property.
				if volume.Config["size"] != "" {
					volume.Config["size"] = ""
				}
			case "dir":
				// Unset "size" property for all dir backed pools.
				if volume.Config["size"] != "" {
					volume.Config["size"] = ""
				}
			case "lvm":
				// Unset default values.
				if volume.Config["block.mount_options"] == "discard" {
					volume.Config["block.mount_options"] = ""
				}
			case "zfs":
				// Unset default values.
				if !shared.IsTrue(volume.Config["zfs.use_refquota"]) {
					volume.Config["zfs.use_refquota"] = ""
				}
				if !shared.IsTrue(volume.Config["zfs.remove_snapshots"]) {
					volume.Config["zfs.remove_snapshots"] = ""
				}
				// Unset "size" property.
				if volume.Config["size"] != "" {
					volume.Config["size"] = ""
				}
			}

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeType, _ := driver.VolumeTypeNameToDBType(volume.Type)
			// Update the volume config.
			err = d.cluster.UpdateStoragePoolVolume("default", volume.Name, volumeType, poolID, volume.Description, volume.Config)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchStorageApiLxdOnBtrfs(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Make sure that config is not empty.
		if pool.Config == nil {
			pool.Config = map[string]string{}

			// Insert default values.
			err = storagePoolFillDefault(poolName, pool.Driver, pool.Config)
			if err != nil {
				return err
			}
		}

		if d.os.BackingFS != "btrfs" {
			continue
		}

		if pool.Driver != "btrfs" {
			continue
		}

		source := pool.Config["source"]
		cleanSource := filepath.Clean(source)
		loopFilePath := shared.VarPath("disks", poolName+".img")
		if cleanSource != loopFilePath {
			continue
		}

		pool.Config["source"] = driver.GetStoragePoolMountPoint(poolName)

		// Update the storage pool config.
		err = d.cluster.UpdateStoragePool(poolName, pool.Description, pool.Config)
		if err != nil {
			return err
		}

		os.Remove(loopFilePath)
	}

	return nil
}

func patchStorageApiDetectLVSize(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		poolID, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// Make sure that config is not empty.
		if pool.Config == nil {
			pool.Config = map[string]string{}

			// Insert default values.
			err = storagePoolFillDefault(poolName, pool.Driver, pool.Config)
			if err != nil {
				return err
			}
		}

		// We're only interested in LVM pools.
		if pool.Driver != "lvm" {
			continue
		}

		// Get all storage volumes on the storage pool.
		volumes, err := d.cluster.GetLocalStoragePoolVolumes("default", poolID, supportedVolumeTypes)
		if err != nil {
			if err == db.ErrNoSuchObject {
				continue
			}
			return err
		}

		poolName := pool.Config["lvm.vg_name"]
		if poolName == "" {
			logger.Errorf("The \"lvm.vg_name\" key should not be empty")
			return fmt.Errorf("The \"lvm.vg_name\" key should not be empty")
		}

		storagePoolVolumeTypeNameToAPIEndpoint := func(volumeTypeName string) (string, error) {
			switch volumeTypeName {
			case db.StoragePoolVolumeTypeNameContainer:
				return patchStoragePoolVolumeAPIEndpointContainers, nil
			case db.StoragePoolVolumeTypeNameVM:
				return patchStoragePoolVolumeAPIEndpointVMs, nil
			case db.StoragePoolVolumeTypeNameImage:
				return patchStoragePoolVolumeAPIEndpointImages, nil
			case db.StoragePoolVolumeTypeNameCustom:
				return patchStoragePoolVolumeAPIEndpointCustom, nil
			}

			return "", fmt.Errorf("Invalid storage volume type name")
		}

		for _, volume := range volumes {
			// Make sure that config is not empty.
			if volume.Config == nil {
				volume.Config = map[string]string{}

				// Insert default values.
				err := volumeFillDefault(volume.Config, pool)
				if err != nil {
					return err
				}
			}

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeTypeApiEndpoint, _ := storagePoolVolumeTypeNameToAPIEndpoint(volume.Type)
			lvmName := lvmNameToLVName(volume.Name)
			lvmLvDevPath := lvmDevPath("default", poolName, volumeTypeApiEndpoint, lvmName)
			size, err := lvmGetLVSize(lvmLvDevPath)
			if err != nil {
				logger.Errorf("Failed to detect size of logical volume: %s", err)
				return err
			}

			if volume.Config["size"] == size {
				continue
			}

			volume.Config["size"] = size

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeType, _ := driver.VolumeTypeNameToDBType(volume.Type)
			// Update the volume config.
			err = d.cluster.UpdateStoragePoolVolume("default", volume.Name, volumeType, poolID, volume.Description, volume.Config)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchStorageApiInsertZfsDriver(name string, d *Daemon) error {
	return d.cluster.FillMissingStoragePoolDriver()
}

func patchStorageZFSnoauto(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil
		}
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		if pool.Driver != "zfs" {
			continue
		}

		zpool := pool.Config["zfs.pool_name"]
		if zpool == "" {
			continue
		}

		containersDatasetPath := fmt.Sprintf("%s/containers", zpool)
		customDatasetPath := fmt.Sprintf("%s/custom", zpool)
		paths := []string{}
		for _, v := range []string{containersDatasetPath, customDatasetPath} {
			_, err := shared.RunCommand("zfs", "get", "-H", "-p", "-o", "value", "name", v)
			if err == nil {
				paths = append(paths, v)
			}
		}

		args := []string{"list", "-t", "filesystem", "-o", "name", "-H", "-r"}
		args = append(args, paths...)

		output, err := shared.RunCommand("zfs", args...)
		if err != nil {
			return fmt.Errorf("Unable to list containers on zpool: %s", zpool)
		}

		for _, entry := range strings.Split(output, "\n") {
			if entry == "" {
				continue
			}

			if shared.StringInSlice(entry, paths) {
				continue
			}

			_, err := shared.RunCommand("zfs", "set", "canmount=noauto", entry)
			if err != nil {
				return fmt.Errorf("Unable to set canmount=noauto on: %s", entry)
			}
		}
	}

	return nil
}

func patchStorageZFSVolumeSize(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil && err == db.ErrNoSuchObject {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		poolID, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about zfs
		if pool.Driver != "zfs" {
			continue
		}

		// Get all storage volumes on the storage pool.
		volumes, err := d.cluster.GetLocalStoragePoolVolumes("default", poolID, supportedVolumeTypes)
		if err != nil {
			if err == db.ErrNoSuchObject {
				continue
			}
			return err
		}

		for _, volume := range volumes {
			if volume.Type != "container" && volume.Type != "image" {
				continue
			}

			// ZFS storage volumes for containers and images should
			// never have a size property set directly on the
			// storage volume itself. For containers the size
			// property is regulated either via a profiles root disk
			// device size property or via the containers local
			// root disk device size property. So unset it here
			// unconditionally.
			if volume.Config["size"] != "" {
				volume.Config["size"] = ""
			}

			// It shouldn't be possible that false volume types
			// exist in the db, so it's safe to ignore the error.
			volumeType, _ := driver.VolumeTypeNameToDBType(volume.Type)
			// Update the volume config.
			err = d.cluster.UpdateStoragePoolVolume("default", volume.Name,
				volumeType, poolID, volume.Description,
				volume.Config)
			if err != nil {
				return err
			}
		}

	}

	return nil
}

func patchNetworkDnsmasqHosts(name string, d *Daemon) error {
	// Get the list of networks
	// Pass project.Default, as dnsmasq (bridge) networks don't support projects.
	networks, err := d.cluster.GetNetworks(project.Default)
	if err != nil {
		return err
	}

	for _, network := range networks {
		// Remove the old dhcp-hosts file (will be re-generated on startup)
		if shared.PathExists(shared.VarPath("networks", network, "dnsmasq.hosts")) {
			err = os.Remove(shared.VarPath("networks", network, "dnsmasq.hosts"))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func patchStorageApiDirBindMount(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil && err == db.ErrNoSuchObject {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about dir
		if pool.Driver != "dir" {
			continue
		}

		source := pool.Config["source"]
		if source == "" {
			msg := fmt.Sprintf(`No "source" property for storage `+
				`pool "%s" found`, poolName)
			logger.Errorf(msg)
			return fmt.Errorf(msg)
		}
		cleanSource := filepath.Clean(source)
		poolMntPoint := driver.GetStoragePoolMountPoint(poolName)

		if cleanSource == poolMntPoint {
			continue
		}

		if shared.PathExists(poolMntPoint) {
			err := os.Remove(poolMntPoint)
			if err != nil {
				return err
			}
		}

		err = os.MkdirAll(poolMntPoint, 0711)
		if err != nil {
			return err
		}

		mountSource := cleanSource
		mountFlags := unix.MS_BIND

		err = unix.Mount(mountSource, poolMntPoint, "", uintptr(mountFlags), "")
		if err != nil {
			logger.Errorf(`Failed to mount DIR storage pool "%s" onto "%s": %s`, mountSource, poolMntPoint, err)
			return err
		}

	}

	return nil
}

func patchFixUploadedAt(name string, d *Daemon) error {
	images, err := d.cluster.GetImagesFingerprints("default", false)
	if err != nil {
		return err
	}

	for _, fingerprint := range images {
		id, image, err := d.cluster.GetImage("default", fingerprint, false)
		if err != nil {
			return err
		}

		err = d.cluster.UpdateImageUploadDate(id, image.UploadedAt)
		if err != nil {
			return err
		}
	}

	return nil
}

func patchStorageApiCephSizeRemove(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil && err == db.ErrNoSuchObject {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		_, pool, _, err := d.cluster.GetStoragePoolInAnyState(poolName)
		if err != nil {
			logger.Errorf("Failed to query database: %s", err)
			return err
		}

		// We only care about zfs and lvm.
		if pool.Driver != "ceph" {
			continue
		}

		// The "size" property does not make sense for ceph osd storage pools.
		if pool.Config["size"] != "" {
			pool.Config["size"] = ""
		}

		// Update the config in the database.
		err = d.cluster.UpdateStoragePool(poolName, pool.Description,
			pool.Config)
		if err != nil {
			return err
		}
	}

	return nil
}

func patchDevicesNewNamingScheme(name string, d *Daemon) error {
	cts, err := d.cluster.LegacyContainersList()
	if err != nil {
		logger.Errorf("Failed to retrieve containers from database")
		return err
	}

	for _, ct := range cts {
		devicesPath := shared.VarPath("devices", ct)
		devDir, err := os.Open(devicesPath)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Errorf("Failed to open \"%s\": %s", devicesPath, err)
				return err
			}
			logger.Debugf("Container \"%s\" does not have on-disk devices", ct)
			continue
		}

		onDiskDevices, err := devDir.Readdirnames(-1)
		if err != nil {
			logger.Errorf("Failed to read directory entries from \"%s\": %s", devicesPath, err)
			return err
		}

		// nothing to do
		if len(onDiskDevices) == 0 {
			logger.Debugf("Devices directory \"%s\" is empty", devicesPath)
			continue
		}

		hasDeviceEntry := map[string]bool{}
		for _, v := range onDiskDevices {
			key := fmt.Sprintf("%s/%s", devicesPath, v)
			hasDeviceEntry[key] = false
		}

		// Load the container from the database.
		c, err := instance.LoadByProjectAndName(d.State(), "default", ct)
		if err != nil {
			logger.Errorf("Failed to load container %s: %s", ct, err)
			return err
		}

		if !c.IsRunning() {
			for wipe := range hasDeviceEntry {
				unix.Unmount(wipe, unix.MNT_DETACH)
				err := os.Remove(wipe)
				if err != nil {
					logger.Errorf("Failed to remove device \"%s\": %s", wipe, err)
					return err
				}
			}

			continue
		}

		// go through all devices for each container
		expandedDevices := c.ExpandedDevices()
		for _, dev := range expandedDevices.Sorted() {
			// We only care about unix-{char,block} and disk devices
			// since other devices don't create on-disk files.
			if !shared.StringInSlice(dev.Config["type"], []string{"disk", "unix-char", "unix-block"}) {
				continue
			}

			// Handle disks
			if dev.Config["type"] == "disk" {
				relativeDestPath := strings.TrimPrefix(dev.Config["path"], "/")
				hyphenatedDevName := strings.Replace(relativeDestPath, "/", "-", -1)
				devNameLegacy := fmt.Sprintf("disk.%s", hyphenatedDevName)
				devPathLegacy := filepath.Join(devicesPath, devNameLegacy)

				if !shared.PathExists(devPathLegacy) {
					logger.Debugf("Device \"%s\" does not exist", devPathLegacy)
					continue
				}

				hasDeviceEntry[devPathLegacy] = true

				// Try to unmount disk devices otherwise we get
				// EBUSY when we try to rename block devices.
				// But don't error out.
				unix.Unmount(devPathLegacy, unix.MNT_DETACH)

				// Switch device to new device naming scheme.
				devPathNew := filepath.Join(devicesPath, fmt.Sprintf("disk.%s.%s", strings.Replace(name, "/", "-", -1), hyphenatedDevName))
				err = os.Rename(devPathLegacy, devPathNew)
				if err != nil {
					logger.Errorf("Failed to rename device from \"%s\" to \"%s\": %s", devPathLegacy, devPathNew, err)
					return err
				}

				continue
			}

			// Handle unix devices
			srcPath := dev.Config["source"]
			if srcPath == "" {
				srcPath = dev.Config["path"]
			}

			relativeSrcPathLegacy := strings.TrimPrefix(srcPath, "/")
			hyphenatedDevNameLegacy := strings.Replace(relativeSrcPathLegacy, "/", "-", -1)
			devNameLegacy := fmt.Sprintf("unix.%s", hyphenatedDevNameLegacy)
			devPathLegacy := filepath.Join(devicesPath, devNameLegacy)

			if !shared.PathExists(devPathLegacy) {
				logger.Debugf("Device \"%s\" does not exist", devPathLegacy)
				continue
			}

			hasDeviceEntry[devPathLegacy] = true

			srcPath = dev.Config["path"]
			if srcPath == "" {
				srcPath = dev.Config["source"]
			}

			relativeSrcPathNew := strings.TrimPrefix(srcPath, "/")
			hyphenatedDevNameNew := strings.Replace(relativeSrcPathNew, "/", "-", -1)
			devPathNew := filepath.Join(devicesPath, fmt.Sprintf("unix.%s.%s", strings.Replace(name, "/", "-", -1), hyphenatedDevNameNew))
			// Switch device to new device naming scheme.
			err = os.Rename(devPathLegacy, devPathNew)
			if err != nil {
				logger.Errorf("Failed to rename device from \"%s\" to \"%s\": %s", devPathLegacy, devPathNew, err)
				return err
			}
		}

		// Wipe any devices not associated with a device entry.
		for k, v := range hasDeviceEntry {
			// This device is associated with a device entry.
			if v {
				continue
			}

			// This device is not associated with a device entry, so
			// wipe it.
			unix.Unmount(k, unix.MNT_DETACH)
			err := os.Remove(k)
			if err != nil {
				logger.Errorf("Failed to remove device \"%s\": %s", k, err)
				return err
			}
		}
	}

	return nil
}

func patchStorageApiPermissions(name string, d *Daemon) error {
	storagePoolsPath := shared.VarPath("storage-pools")
	err := os.Chmod(storagePoolsPath, 0711)
	if err != nil {
		return err
	}

	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil && err == db.ErrNoSuchObject {
		// No pool was configured in the previous update. So we're on a
		// pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	for _, poolName := range pools {
		pool, err := storagePools.GetPoolByName(d.State(), poolName)
		if err != nil {
			return err
		}

		ourMount, err := pool.Mount()
		if err != nil {
			return err
		}

		if ourMount {
			defer pool.Unmount()
		}

		// chmod storage pool directory
		storagePoolDir := shared.VarPath("storage-pools", poolName)
		err = os.Chmod(storagePoolDir, 0711)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		// chmod containers directory
		containersDir := shared.VarPath("storage-pools", poolName, "containers")
		err = os.Chmod(containersDir, 0711)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		// chmod custom subdir
		customDir := shared.VarPath("storage-pools", poolName, "custom")
		err = os.Chmod(customDir, 0711)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		// chmod images subdir
		imagesDir := shared.VarPath("storage-pools", poolName, "images")
		err = os.Chmod(imagesDir, 0700)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		// chmod snapshots subdir
		snapshotsDir := shared.VarPath("storage-pools", poolName, "snapshots")
		err = os.Chmod(snapshotsDir, 0700)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		// Retrieve ID of the storage pool (and check if the storage pool
		// exists).
		poolID, err := d.cluster.GetStoragePoolID(poolName)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		volumes, err := d.cluster.GetLocalStoragePoolVolumesWithType(project.Default, db.StoragePoolVolumeTypeCustom, poolID)
		if err != nil && err != db.ErrNoSuchObject {
			return err
		}

		for _, vol := range volumes {
			pool, err := storagePools.GetPoolByName(d.State(), poolName)
			if err != nil {
				return err
			}

			// Run task in anonymous function so as not to stack up defers.
			err = func() error {
				err = pool.MountCustomVolume(project.Default, vol, nil)
				if err != nil {
					return err
				}
				defer pool.UnmountCustomVolume(project.Default, vol, nil)

				cuMntPoint := storageDrivers.GetVolumeMountPath(poolName, storageDrivers.VolumeTypeCustom, vol)
				err = os.Chmod(cuMntPoint, 0711)
				if err != nil && !os.IsNotExist(err) {
					return err
				}

				return nil
			}()
			if err != nil {
				return err
			}
		}
	}

	cRegular, err := d.cluster.LegacyContainersList()
	if err != nil {
		return err
	}

	for _, ct := range cRegular {
		// load the container from the database
		inst, err := instance.LoadByProjectAndName(d.State(), project.Default, ct)
		if err != nil {
			return err
		}

		// Start the storage if needed
		pool, err := storagePools.GetPoolByInstance(d.State(), inst)
		if err != nil {
			return err
		}

		_, err = storagePools.InstanceMount(pool, inst, nil)
		if err != nil {
			return err
		}

		if inst.IsPrivileged() {
			err = os.Chmod(inst.Path(), 0700)
		} else {
			err = os.Chmod(inst.Path(), 0711)
		}

		storagePools.InstanceUnmount(pool, inst, nil)

		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}

func patchCandidConfigKey(name string, d *Daemon) error {
	return d.cluster.Transaction(func(tx *db.ClusterTx) error {
		config, err := tx.Config()
		if err != nil {
			return err
		}

		value, ok := config["core.macaroon.endpoint"]
		if !ok {
			// Nothing to do
			return nil
		}

		return tx.UpdateConfig(map[string]string{
			"core.macaroon.endpoint": "",
			"candid.api.url":         value,
		})
	})
}

func patchMoveBackups(name string, d *Daemon) error {
	// Get all storage pools
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil {
		if err == db.ErrNoSuchObject {
			return nil
		}

		return err
	}

	// Get all containers
	containers, err := d.cluster.LegacyContainersList()
	if err != nil {
		if err != db.ErrNoSuchObject {
			return err
		}

		containers = []string{}
	}

	// Convert the backups
	for _, pool := range pools {
		poolBackupPath := shared.VarPath("storage-pools", pool, "backups")

		// Check if we have any backup
		if !shared.PathExists(poolBackupPath) {
			continue
		}

		// Look at the list of backups
		cts, err := ioutil.ReadDir(poolBackupPath)
		if err != nil {
			return err
		}

		for _, ct := range cts {
			if !shared.StringInSlice(ct.Name(), containers) {
				// Backups for a deleted container, remove it
				err = os.RemoveAll(filepath.Join(poolBackupPath, ct.Name()))
				if err != nil {
					return err
				}

				continue
			}

			backups, err := ioutil.ReadDir(filepath.Join(poolBackupPath, ct.Name()))
			if err != nil {
				return err
			}

			if len(backups) > 0 {
				// Create the target path if needed
				backupsPath := shared.VarPath("backups", ct.Name())
				if !shared.PathExists(backupsPath) {
					err := os.MkdirAll(backupsPath, 0700)
					if err != nil {
						return err
					}
				}
			}

			for _, backup := range backups {
				// Create the tarball
				backupPath := shared.VarPath("backups", ct.Name(), backup.Name())
				path := filepath.Join(poolBackupPath, ct.Name(), backup.Name())
				args := []string{"-cf", backupPath, "--xattrs", "-C", path, "--transform", "s,^./,backup/,", "."}
				_, err = shared.RunCommand("tar", args...)
				if err != nil {
					return err
				}

				// Compress it
				infile, err := os.Open(backupPath)
				if err != nil {
					return err
				}
				defer infile.Close()

				compressed, err := os.Create(backupPath + ".compressed")
				if err != nil {
					return err
				}
				defer compressed.Close()

				err = compressFile("xz", infile, compressed)
				if err != nil {
					return err
				}

				err = os.Remove(backupPath)
				if err != nil {
					return err
				}

				err = os.Rename(compressed.Name(), backupPath)
				if err != nil {
					return err
				}

				// Set permissions
				err = os.Chmod(backupPath, 0600)
				if err != nil {
					return err
				}
			}
		}

		// Wipe the backup directory
		err = os.RemoveAll(poolBackupPath)
		if err != nil {
			return err
		}
	}

	return nil
}

func patchStorageApiRenameContainerSnapshotsDir(name string, d *Daemon) error {
	pools, err := d.cluster.GetStoragePoolNames()
	if err != nil && err == db.ErrNoSuchObject {
		// No pool was configured so we're on a pristine LXD instance.
		return nil
	} else if err != nil {
		// Database is screwed.
		logger.Errorf("Failed to query database: %s", err)
		return err
	}

	// Iterate through all configured pools
	for _, poolName := range pools {
		// Make sure the pool is mounted
		pool, err := storagePools.GetPoolByName(d.State(), poolName)
		if err != nil {
			return err
		}

		ourMount, err := pool.Mount()
		if err != nil {
			return err
		}

		if ourMount {
			defer pool.Unmount()
		}

		// Figure out source/target path
		containerSnapshotDirOld := shared.VarPath("storage-pools", poolName, "snapshots")
		containerSnapshotDirNew := shared.VarPath("storage-pools", poolName, "containers-snapshots")
		if !shared.PathExists(containerSnapshotDirOld) {
			continue
		}

		if !shared.PathExists(containerSnapshotDirNew) {
			// Simple and easy rename (common path)
			err = os.Rename(containerSnapshotDirOld, containerSnapshotDirNew)
			if err != nil {
				return err
			}
		} else {
			// Check if btrfs might have been used
			hasBtrfs := false
			_, err = exec.LookPath("btrfs")
			if err == nil {
				hasBtrfs = true
			}

			// Get all containers
			containersDir, err := os.Open(shared.VarPath("storage-pools", poolName, "snapshots"))
			if err != nil {
				return err
			}
			defer containersDir.Close()

			entries, err := containersDir.Readdirnames(-1)
			if err != nil {
				return err
			}

			for _, entry := range entries {
				// Create the target (straight rename won't work with btrfs)
				if !shared.PathExists(filepath.Join(containerSnapshotDirNew, entry)) {
					err = os.Mkdir(filepath.Join(containerSnapshotDirNew, entry), 0700)
					if err != nil {
						return err
					}
				}

				// Get all snapshots
				snapshotsDir, err := os.Open(shared.VarPath("storage-pools", poolName, "snapshots", entry))
				if err != nil {
					return err
				}
				defer snapshotsDir.Close()

				snaps, err := snapshotsDir.Readdirnames(-1)
				if err != nil {
					return err
				}

				// Disable the read-only properties
				if hasBtrfs {
					path := snapshotsDir.Name()
					subvols, _ := storageDrivers.BTRFSSubVolumesGet(path)
					for _, subvol := range subvols {
						subvol = filepath.Join(path, subvol)
						newSubvol := filepath.Join(shared.VarPath("storage-pools", poolName, "containers-snapshots", entry), subvol)

						if !storageDrivers.BTRFSSubVolumeIsRo(subvol) {
							continue
						}

						storageDrivers.BTRFSSubVolumeMakeRw(subvol)
						defer storageDrivers.BTRFSSubVolumeMakeRo(newSubvol)
					}
				}

				// Rename the snapshots
				for _, snap := range snaps {
					err = os.Rename(filepath.Join(containerSnapshotDirOld, entry, snap), filepath.Join(containerSnapshotDirNew, entry, snap))
					if err != nil {
						return err
					}
				}

				// Cleanup
				err = os.Remove(snapshotsDir.Name())
				if err != nil {
					if hasBtrfs {
						err1 := btrfsSubVolumeDelete(snapshotsDir.Name())
						if err1 != nil {
							return err
						}
					} else {
						return err
					}
				}
			}

			// Cleanup
			err = os.Remove(containersDir.Name())
			if err != nil {
				if hasBtrfs {
					err1 := btrfsSubVolumeDelete(containersDir.Name())
					if err1 != nil {
						return err
					}
				} else {
					return err
				}
			}
		}
	}

	return nil
}

func patchStorageApiUpdateContainerSnapshots(name string, d *Daemon) error {
	snapshotLinksDir, err := os.Open(shared.VarPath("snapshots"))
	if err != nil {
		return err
	}
	defer snapshotLinksDir.Close()

	// Get a list of all symlinks
	snapshotLinks, err := snapshotLinksDir.Readdirnames(-1)
	snapshotLinksDir.Close()
	if err != nil {
		return err
	}

	for _, linkName := range snapshotLinks {
		targetName, err := os.Readlink(shared.VarPath("snapshots", linkName))
		if err != nil {
			return err
		}

		targetFields := strings.Split(targetName, "/")

		if len(targetFields) < 4 {
			continue
		}

		if targetFields[len(targetFields)-2] != "snapshots" {
			continue
		}

		targetFields[len(targetFields)-2] = "containers-snapshots"
		newTargetName := strings.Join(targetFields, "/")

		err = os.Remove(shared.VarPath("snapshots", linkName))
		if err != nil {
			return err
		}

		err = os.Symlink(newTargetName, shared.VarPath("snapshots", linkName))
		if err != nil {
			return err
		}
	}

	return nil
}

func patchClusteringAddRoles(name string, d *Daemon) error {
	return nil
}

func patchClusteringDropDatabaseRole(name string, d *Daemon) error {
	return d.State().Cluster.Transaction(func(tx *db.ClusterTx) error {
		nodes, err := tx.GetNodes()
		if err != nil {
			return err
		}
		for _, node := range nodes {
			err := tx.UpdateNodeRoles(node.ID, nil)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// patchNetworkClearBridgeVolatileHwaddr removes the unsupported `volatile.bridge.hwaddr` config key from networks.
func patchNetworkClearBridgeVolatileHwaddr(name string, d *Daemon) error {
	// Use project.Default, as bridge networks don't support projects.
	projectName := project.Default

	// Get the list of networks.
	networks, err := d.cluster.GetNetworks(projectName)
	if err != nil {
		return errors.Wrapf(err, "Failed loading networks for network_clear_bridge_volatile_hwaddr patch")
	}

	for _, networkName := range networks {
		_, net, _, err := d.cluster.GetNetworkInAnyState(projectName, networkName)
		if err != nil {
			return errors.Wrapf(err, "Failed loading network %q for network_clear_bridge_volatile_hwaddr patch", networkName)
		}

		if net.Config["volatile.bridge.hwaddr"] != "" {
			delete(net.Config, "volatile.bridge.hwaddr")
			err = d.cluster.UpdateNetwork(projectName, net.Name, net.Description, net.Config)
			if err != nil {
				return errors.Wrapf(err, "Failed updating network %q for network_clear_bridge_volatile_hwaddr patch", networkName)
			}
		}
	}

	return nil
}

// Patches end here

// Here are a couple of legacy patches that were originally in
// db_updates.go and were written before the new patch mechanism
// above. To preserve exactly their semantics we treat them
// differently and still apply them during the database upgrade. In
// principle they could be converted to regular patches like the ones
// above, however that seems an unnecessary risk at this moment. See
// also PR #3322.
//
// NOTE: don't add any legacy patch here, instead use the patches
// mechanism above.
var legacyPatches = map[int](func(tx *sql.Tx) error){
	11: patchUpdateFromV10,
	12: patchUpdateFromV11,
	16: patchUpdateFromV15,
	30: patchUpdateFromV29,
	31: patchUpdateFromV30,
}

func patchUpdateFromV10(_ *sql.Tx) error {
	// Logic was moved to Daemon.init().
	return nil
}

func patchUpdateFromV11(_ *sql.Tx) error {
	containers, err := instancesOnDisk()
	if err != nil {
		return err
	}

	errors := 0

	cNames := containers["default"]

	for _, cName := range cNames {
		snapParentName, snapOnlyName, _ := shared.InstanceGetParentAndSnapshotName(cName)
		oldPath := shared.VarPath("containers", snapParentName, "snapshots", snapOnlyName)
		newPath := shared.VarPath("snapshots", snapParentName, snapOnlyName)
		if shared.PathExists(oldPath) && !shared.PathExists(newPath) {
			logger.Info(
				"Moving snapshot",
				log.Ctx{
					"snapshot": cName,
					"oldPath":  oldPath,
					"newPath":  newPath})

			// Rsync
			// containers/<container>/snapshots/<snap0>
			// to
			// snapshots/<container>/<snap0>
			output, err := rsync.LocalCopy(oldPath, newPath, "", true)
			if err != nil {
				logger.Error(
					"Failed rsync snapshot",
					log.Ctx{
						"snapshot": cName,
						"output":   string(output),
						"err":      err})
				errors++
				continue
			}

			// Remove containers/<container>/snapshots/<snap0>
			if err := os.RemoveAll(oldPath); err != nil {
				logger.Error(
					"Failed to remove the old snapshot path",
					log.Ctx{
						"snapshot": cName,
						"oldPath":  oldPath,
						"err":      err})

				// Ignore this error.
				// errors++
				// continue
			}

			// Remove /var/lib/lxd/containers/<container>/snapshots
			// if its empty.
			cPathParent := filepath.Dir(oldPath)
			if ok, _ := shared.PathIsEmpty(cPathParent); ok {
				os.Remove(cPathParent)
			}

		} // if shared.PathExists(oldPath) && !shared.PathExists(newPath) {
	} // for _, cName := range cNames {

	// Refuse to start lxd if a rsync failed.
	if errors > 0 {
		return fmt.Errorf("Got errors while moving snapshots, see the log output")
	}

	return nil
}

func patchUpdateFromV15(tx *sql.Tx) error {
	// munge all LVM-backed containers' LV names to match what is
	// required for snapshot support

	containers, err := instancesOnDisk()
	if err != nil {
		return err
	}
	cNames := containers["default"]

	vgName := ""
	config, err := query.SelectConfig(tx, "config", "")
	if err != nil {
		return err
	}
	vgName = config["storage.lvm_vg_name"]

	for _, cName := range cNames {
		var lvLinkPath string
		if strings.Contains(cName, shared.SnapshotDelimiter) {
			lvLinkPath = shared.VarPath("snapshots", fmt.Sprintf("%s.lv", cName))
		} else {
			lvLinkPath = shared.VarPath("containers", fmt.Sprintf("%s.lv", cName))
		}

		if !shared.PathExists(lvLinkPath) {
			continue
		}

		newLVName := strings.Replace(cName, "-", "--", -1)
		newLVName = strings.Replace(newLVName, shared.SnapshotDelimiter, "-", -1)

		if cName == newLVName {
			logger.Debug("No need to rename, skipping", log.Ctx{"cName": cName, "newLVName": newLVName})
			continue
		}

		logger.Debug("About to rename cName in lv upgrade", log.Ctx{"lvLinkPath": lvLinkPath, "cName": cName, "newLVName": newLVName})

		_, err := shared.RunCommand("lvrename", vgName, cName, newLVName)
		if err != nil {
			return fmt.Errorf("Could not rename LV '%s' to '%s': %v", cName, newLVName, err)
		}

		if err := os.Remove(lvLinkPath); err != nil {
			return fmt.Errorf("Couldn't remove lvLinkPath '%s'", lvLinkPath)
		}
		newLinkDest := fmt.Sprintf("/dev/%s/%s", vgName, newLVName)
		if err := os.Symlink(newLinkDest, lvLinkPath); err != nil {
			return fmt.Errorf("Couldn't recreate symlink '%s' to '%s'", lvLinkPath, newLinkDest)
		}
	}

	return nil
}

func patchUpdateFromV29(_ *sql.Tx) error {
	if shared.PathExists(shared.VarPath("zfs.img")) {
		err := os.Chmod(shared.VarPath("zfs.img"), 0600)
		if err != nil {
			return err
		}
	}

	return nil
}

func patchUpdateFromV30(_ *sql.Tx) error {
	entries, err := ioutil.ReadDir(shared.VarPath("containers"))
	if err != nil {
		/* If the directory didn't exist before, the user had never
		 * started containers, so we don't need to fix up permissions
		 * on anything.
		 */
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !shared.IsDir(shared.VarPath("containers", entry.Name(), "rootfs")) {
			continue
		}

		info, err := os.Stat(shared.VarPath("containers", entry.Name(), "rootfs"))
		if err != nil {
			return err
		}

		if int(info.Sys().(*syscall.Stat_t).Uid) == 0 {
			err := os.Chmod(shared.VarPath("containers", entry.Name()), 0700)
			if err != nil {
				return err
			}

			err = os.Chown(shared.VarPath("containers", entry.Name()), 0, 0)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
