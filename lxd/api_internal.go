package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	runtimeDebug "runtime/debug"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/db/node"
	"github.com/lxc/lxd/lxd/db/query"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	storagePools "github.com/lxc/lxd/lxd/storage"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/units"
)

var apiInternal = []APIEndpoint{
	internalReadyCmd,
	internalShutdownCmd,
	internalContainerOnStartCmd,
	internalContainerOnStopNSCmd,
	internalContainerOnStopCmd,
	internalContainersCmd,
	internalSQLCmd,
	internalClusterAcceptCmd,
	internalClusterRebalanceCmd,
	internalClusterAssignCmd,
	internalClusterContainerMovedCmd,
	internalGarbageCollectorCmd,
	internalRAFTSnapshotCmd,
	internalClusterHandoverCmd,
	internalClusterRaftNodeCmd,
	internalImageRefreshCmd,
	internalImageOptimizeCmd,
	internalWarningCreateCmd,
	internalRecoverValidateCmd,
	internalRecoverImportCmd,
}

var internalShutdownCmd = APIEndpoint{
	Path: "shutdown",

	Put: APIEndpointAction{Handler: internalShutdown},
}

var internalReadyCmd = APIEndpoint{
	Path: "ready",

	Get: APIEndpointAction{Handler: internalWaitReady},
}

var internalContainerOnStartCmd = APIEndpoint{
	Path: "containers/{instanceRef}/onstart",

	Get: APIEndpointAction{Handler: internalContainerOnStart},
}

var internalContainerOnStopNSCmd = APIEndpoint{
	Path: "containers/{instanceRef}/onstopns",

	Get: APIEndpointAction{Handler: internalContainerOnStopNS},
}

var internalContainerOnStopCmd = APIEndpoint{
	Path: "containers/{instanceRef}/onstop",

	Get: APIEndpointAction{Handler: internalContainerOnStop},
}

var internalSQLCmd = APIEndpoint{
	Path: "sql",

	Get:  APIEndpointAction{Handler: internalSQLGet},
	Post: APIEndpointAction{Handler: internalSQLPost},
}

var internalContainersCmd = APIEndpoint{
	Path: "containers",

	Post: APIEndpointAction{Handler: internalImportFromRecovery},
}

var internalGarbageCollectorCmd = APIEndpoint{
	Path: "gc",

	Get: APIEndpointAction{Handler: internalGC},
}

var internalRAFTSnapshotCmd = APIEndpoint{
	Path: "raft-snapshot",

	Get: APIEndpointAction{Handler: internalRAFTSnapshot},
}

var internalImageRefreshCmd = APIEndpoint{
	Path: "testing/image-refresh",

	Get: APIEndpointAction{Handler: internalRefreshImage},
}

var internalImageOptimizeCmd = APIEndpoint{
	Path: "image-optimize",

	Post: APIEndpointAction{Handler: internalOptimizeImage},
}

var internalWarningCreateCmd = APIEndpoint{
	Path: "testing/warnings",

	Post: APIEndpointAction{Handler: internalCreateWarning},
}

var internalRecoverValidateCmd = APIEndpoint{
	Path: "recover/validate",

	Post: APIEndpointAction{Handler: internalRecoverValidate},
}

var internalRecoverImportCmd = APIEndpoint{
	Path: "recover/import",

	Post: APIEndpointAction{Handler: internalRecoverImport},
}

type internalImageOptimizePost struct {
	Image api.Image `json:"image" yaml:"image"`
	Pool  string    `json:"pool" yaml:"pool"`
}

type internalWarningCreatePost struct {
	Location       string `json:"location" yaml:"location"`
	Project        string `json:"project" yaml:"project"`
	EntityTypeCode int    `json:"entity_type_code" yaml:"entity_type_code"`
	EntityID       int    `json:"entity_id" yaml:"entity_id"`
	TypeCode       int    `json:"type_code" yaml:"type_code"`
	Message        string `json:"message" yaml:"message"`
}

// internalCreateWarning creates a warning, and is used for testing only.
func internalCreateWarning(d *Daemon, r *http.Request) response.Response {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return response.InternalError(err)
	}

	rdr1 := ioutil.NopCloser(bytes.NewBuffer(body))
	rdr2 := ioutil.NopCloser(bytes.NewBuffer(body))

	reqRaw := shared.Jmap{}
	err = json.NewDecoder(rdr1).Decode(&reqRaw)
	if err != nil {
		return response.BadRequest(err)
	}

	req := internalWarningCreatePost{}
	err = json.NewDecoder(rdr2).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	req.EntityTypeCode, _ = reqRaw.GetInt("entity_type_code")
	req.EntityID, _ = reqRaw.GetInt("entity_id")

	// Check if the entity exists, and fail if it doesn't.
	_, ok := cluster.EntityNames[req.EntityTypeCode]
	if req.EntityTypeCode != -1 && !ok {
		return response.SmartError(fmt.Errorf("Invalid entity type"))
	}

	err = d.cluster.UpsertWarning(req.Location, req.Project, req.EntityTypeCode, req.EntityID, db.WarningType(req.TypeCode), req.Message)
	if err != nil {
		return response.SmartError(errors.Wrap(err, "Failed to create warning"))
	}

	return response.EmptySyncResponse
}

func internalOptimizeImage(d *Daemon, r *http.Request) response.Response {
	req := &internalImageOptimizePost{}

	// Parse the request.
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	err = imageCreateInPool(d, &req.Image, req.Pool)
	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

func internalRefreshImage(d *Daemon, r *http.Request) response.Response {
	err := autoUpdateImages(d.ctx, d)
	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

func internalWaitReady(d *Daemon, r *http.Request) response.Response {
	// Check that we're not shutting down.
	var isClosing bool
	d.clusterMembershipMutex.RLock()
	isClosing = d.clusterMembershipClosing
	d.clusterMembershipMutex.RUnlock()
	if isClosing {
		return response.Unavailable(fmt.Errorf("LXD daemon is shutting down"))
	}

	select {
	case <-d.readyChan:
	default:
		return response.Unavailable(fmt.Errorf("LXD daemon not ready yet"))
	}

	return response.EmptySyncResponse
}

func internalShutdown(d *Daemon, r *http.Request) response.Response {
	d.shutdownChan <- struct{}{}

	force := queryParam(r, "force")

	if force == "true" {
		d.shutdownChan <- struct{}{}
	}

	return response.EmptySyncResponse
}

// internalContainerHookLoadFromRequestReference loads the container from the instance reference in the request.
// It detects whether the instance reference is an instance ID or instance name and loads instance accordingly.
func internalContainerHookLoadFromReference(s *state.State, r *http.Request) (instance.Instance, error) {
	var inst instance.Instance
	instanceRef := mux.Vars(r)["instanceRef"]
	projectName := projectParam(r)

	instanceID, err := strconv.Atoi(instanceRef)
	if err == nil {
		inst, err = instance.LoadByID(s, instanceID)
		if err != nil {
			return nil, err
		}
	} else {
		inst, err = instance.LoadByProjectAndName(s, projectName, instanceRef)
		if err != nil {
			return nil, err
		}
	}

	if inst.Type() != instancetype.Container {
		return nil, fmt.Errorf("Instance is not container type")
	}

	return inst, nil
}

func internalContainerOnStart(d *Daemon, r *http.Request) response.Response {
	inst, err := internalContainerHookLoadFromReference(d.State(), r)
	if err != nil {
		logger.Error("The start hook failed to load", log.Ctx{"err": err})
		return response.SmartError(err)
	}

	err = inst.OnHook(instance.HookStart, nil)
	if err != nil {
		logger.Error("The start hook failed", log.Ctx{"instance": inst.Name(), "err": err})
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

func internalContainerOnStopNS(d *Daemon, r *http.Request) response.Response {
	inst, err := internalContainerHookLoadFromReference(d.State(), r)
	if err != nil {
		logger.Error("The stopns hook failed to load", log.Ctx{"err": err})
		return response.SmartError(err)
	}

	target := queryParam(r, "target")
	if target == "" {
		target = "unknown"
	}
	netns := queryParam(r, "netns")

	args := map[string]string{
		"target": target,
		"netns":  netns,
	}

	err = inst.OnHook(instance.HookStopNS, args)
	if err != nil {
		logger.Error("The stopns hook failed", log.Ctx{"instance": inst.Name(), "err": err})
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

func internalContainerOnStop(d *Daemon, r *http.Request) response.Response {
	inst, err := internalContainerHookLoadFromReference(d.State(), r)
	if err != nil {
		logger.Error("The stop hook failed to load", log.Ctx{"err": err})
		return response.SmartError(err)
	}

	target := queryParam(r, "target")
	if target == "" {
		target = "unknown"
	}

	args := map[string]string{
		"target": target,
	}

	err = inst.OnHook(instance.HookStop, args)
	if err != nil {
		logger.Error("The stop hook failed", log.Ctx{"instance": inst.Name(), "err": err})
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

type internalSQLDump struct {
	Text string `json:"text" yaml:"text"`
}

type internalSQLQuery struct {
	Database string `json:"database" yaml:"database"`
	Query    string `json:"query" yaml:"query"`
}

type internalSQLBatch struct {
	Results []internalSQLResult
}

type internalSQLResult struct {
	Type         string          `json:"type" yaml:"type"`
	Columns      []string        `json:"columns" yaml:"columns"`
	Rows         [][]interface{} `json:"rows" yaml:"rows"`
	RowsAffected int64           `json:"rows_affected" yaml:"rows_affected"`
}

// Perform a database dump.
func internalSQLGet(d *Daemon, r *http.Request) response.Response {
	database := r.FormValue("database")

	if !shared.StringInSlice(database, []string{"local", "global"}) {
		return response.BadRequest(fmt.Errorf("Invalid database"))
	}

	schemaFormValue := r.FormValue("schema")
	schemaOnly, err := strconv.Atoi(schemaFormValue)
	if err != nil {
		schemaOnly = 0
	}

	var schema string
	var db *sql.DB
	if database == "global" {
		db = d.cluster.DB()
		schema = cluster.FreshSchema()
	} else {
		db = d.db.DB()
		schema = node.FreshSchema()
	}

	tx, err := db.Begin()
	if err != nil {
		return response.SmartError(errors.Wrap(err, "failed to start transaction"))
	}
	defer tx.Rollback()
	dump, err := query.Dump(tx, schema, schemaOnly == 1)
	if err != nil {
		return response.SmartError(errors.Wrapf(err, "failed dump database %s", database))
	}
	return response.SyncResponse(true, internalSQLDump{Text: dump})
}

// Execute queries.
func internalSQLPost(d *Daemon, r *http.Request) response.Response {
	req := &internalSQLQuery{}
	// Parse the request.
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	if !shared.StringInSlice(req.Database, []string{"local", "global"}) {
		return response.BadRequest(fmt.Errorf("Invalid database"))
	}

	if req.Query == "" {
		return response.BadRequest(fmt.Errorf("No query provided"))
	}

	var db *sql.DB
	if req.Database == "global" {
		db = d.cluster.DB()
	} else {
		db = d.db.DB()
	}

	batch := internalSQLBatch{}

	if req.Query == ".sync" {
		d.gateway.Sync()
		return response.SyncResponse(true, batch)
	}

	for _, query := range strings.Split(req.Query, ";") {
		query = strings.TrimLeft(query, " ")

		if query == "" {
			continue
		}

		result := internalSQLResult{}

		tx, err := db.Begin()
		if err != nil {
			return response.SmartError(err)
		}

		if strings.HasPrefix(strings.ToUpper(query), "SELECT") {
			err = internalSQLSelect(tx, query, &result)
			tx.Rollback()
		} else {
			err = internalSQLExec(tx, query, &result)
			if err != nil {
				tx.Rollback()
			} else {
				err = tx.Commit()
			}
		}
		if err != nil {
			return response.SmartError(err)
		}

		batch.Results = append(batch.Results, result)
	}

	return response.SyncResponse(true, batch)
}

func internalSQLSelect(tx *sql.Tx, query string, result *internalSQLResult) error {
	result.Type = "select"

	rows, err := tx.Query(query)
	if err != nil {
		return errors.Wrap(err, "Failed to execute query")
	}

	defer rows.Close()

	result.Columns, err = rows.Columns()
	if err != nil {
		return errors.Wrap(err, "Failed to fetch colume names")
	}

	for rows.Next() {
		row := make([]interface{}, len(result.Columns))
		rowPointers := make([]interface{}, len(result.Columns))
		for i := range row {
			rowPointers[i] = &row[i]
		}

		err := rows.Scan(rowPointers...)
		if err != nil {
			return errors.Wrap(err, "Failed to scan row")
		}

		for i, column := range row {
			// Convert bytes to string. This is safe as
			// long as we don't have any BLOB column type.
			data, ok := column.([]byte)
			if ok {
				row[i] = string(data)
			}
		}

		result.Rows = append(result.Rows, row)
	}

	err = rows.Err()
	if err != nil {
		return errors.Wrap(err, "Got a row error")
	}

	return nil
}

func internalSQLExec(tx *sql.Tx, query string, result *internalSQLResult) error {
	result.Type = "exec"
	r, err := tx.Exec(query)
	if err != nil {
		return errors.Wrapf(err, "Failed to exec query")
	}

	result.RowsAffected, err = r.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "Failed to fetch affected rows")
	}

	return nil
}

type internalImportPost struct {
	Name              string `json:"name" yaml:"name"`
	Force             bool   `json:"force" yaml:"force"`
	AllowNameOverride bool   `json:"allow_name_override" yaml:"allow_name_override"`
}

// internalImportFromRecovery allows recovery of an instance that is already on disk and mounted.
// If recovery is successful the instance is unmounted at the end.
func internalImportFromRecovery(d *Daemon, r *http.Request) response.Response {
	projectName := projectParam(r)

	// Parse the request.
	req := &internalImportPost{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	resp := internalImport(d, projectName, req, true)
	if resp.String() != "success" {
		return resp
	}

	inst, err := instance.LoadByProjectAndName(d.State(), projectName, req.Name)
	if err != nil {
		return response.SmartError(err)
	}

	pool, err := storagePools.GetPoolByInstance(d.State(), inst)
	if err != nil {
		return response.SmartError(err)
	}

	if inst.IsRunning() {
		// If the instance is running, then give the instance a chance to regenerate its config file, as
		// the internalImport function will have cleared its log directory (which contains the conf file).
		// This allows functionality that relies on a config file to continue after the recovery.
		err = inst.SaveConfigFile()
		if err != nil {
			return response.SmartError(errors.Wrapf(err, "Failed regenerating instance config file"))
		}
	} else {
		// If instance isn't running, then unmount instance volume to reset the mount and any left over
		// reference counters back its non-running state.
		_, err = pool.UnmountInstance(inst, nil)
		if err != nil {
			return response.SmartError(errors.Wrapf(err, "Failed unmounting instance"))
		}
	}

	// Reinitialise the instance's root disk quota even if no size specified (allows the storage driver the
	// opportunity to reinitialise the quota based on the new storage volume's DB ID).
	_, rootConfig, err := shared.GetRootDiskDevice(inst.ExpandedDevices().CloneNative())
	if err == nil {
		err = pool.SetInstanceQuota(inst, rootConfig["size"], rootConfig["size.state"], nil)
		if err != nil {
			return response.SmartError(errors.Wrapf(err, "Failed reinitializing root disk quota %q", rootConfig["size"]))
		}
	}

	return resp
}

// internalImport creates the instance and storage volume DB records.
// It expects the instance volume to be mounted so that the backup.yaml file is readable.
func internalImport(d *Daemon, projectName string, req *internalImportPost, recovery bool) response.Response {
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("The name of the instance is required"))
	}

	storagePoolsPath := shared.VarPath("storage-pools")
	storagePoolsDir, err := os.Open(storagePoolsPath)
	if err != nil {
		return response.InternalError(err)
	}

	// Get a list of all storage pools.
	storagePoolNames, err := storagePoolsDir.Readdirnames(-1)
	if err != nil {
		storagePoolsDir.Close()
		return response.InternalError(err)
	}
	storagePoolsDir.Close()

	// Check whether the instance exists on any of the storage pools as either a container or a VM.
	instanceMountPoints := []string{}
	instancePoolName := ""
	instanceType := instancetype.Container
	instanceVolType := storageDrivers.VolumeTypeContainer
	instanceDBVolType := db.StoragePoolVolumeTypeContainer

	for _, volType := range []storageDrivers.VolumeType{storageDrivers.VolumeTypeVM, storageDrivers.VolumeTypeContainer} {
		for _, poolName := range storagePoolNames {
			volStorageName := project.Instance(projectName, req.Name)
			instanceMntPoint := storageDrivers.GetVolumeMountPath(poolName, volType, volStorageName)

			if shared.PathExists(instanceMntPoint) {
				instanceMountPoints = append(instanceMountPoints, instanceMntPoint)
				instancePoolName = poolName
				instanceVolType = volType

				if volType == storageDrivers.VolumeTypeVM {
					instanceType = instancetype.VM
					instanceDBVolType = db.StoragePoolVolumeTypeVM
				} else {
					instanceType = instancetype.Container
					instanceDBVolType = db.StoragePoolVolumeTypeContainer
				}
			}
		}
	}

	// Quick checks.
	if len(instanceMountPoints) > 1 {
		return response.BadRequest(fmt.Errorf(`The instance %q seems to exist on multiple storage pools`, req.Name))
	} else if len(instanceMountPoints) != 1 {
		return response.BadRequest(fmt.Errorf(`The instance %q does not seem to exist on any storage pool`, req.Name))
	}

	// User needs to make sure that we can access the directory where backup.yaml lives.
	instanceMountPoint := instanceMountPoints[0]
	isEmpty, err := shared.PathIsEmpty(instanceMountPoint)
	if err != nil {
		return response.InternalError(err)
	}

	if isEmpty {
		return response.BadRequest(fmt.Errorf(`The instance's directory %q appears to be empty. Please ensure that the instance's storage volume is mounted`, instanceMountPoint))
	}

	// Read in the backup.yaml file.
	backupYamlPath := filepath.Join(instanceMountPoint, "backup.yaml")
	backupConf, err := backup.ParseConfigYamlFile(backupYamlPath)
	if err != nil {
		return response.SmartError(err)
	}

	if req.AllowNameOverride && req.Name != "" {
		backupConf.Container.Name = req.Name
	}

	if req.Name != backupConf.Container.Name {
		return response.InternalError(fmt.Errorf("Instance name in request %q doesn't match instance name in backup config %q", req.Name, backupConf.Container.Name))
	}

	// Update snapshot names to include instance name (if needed).
	for i, snap := range backupConf.Snapshots {
		if !strings.Contains(snap.Name, "/") {
			backupConf.Snapshots[i].Name = fmt.Sprintf("%s/%s", backupConf.Container.Name, snap.Name)
		}
	}

	if backupConf.Pool == nil {
		// We don't know what kind of storage type the pool is.
		return response.BadRequest(fmt.Errorf(`No storage pool struct in the backup file found. The storage pool needs to be recovered manually`))
	}

	// Try to retrieve the storage pool the instance supposedly lives on.
	pool, err := storagePools.GetPoolByName(d.State(), instancePoolName)
	if err == db.ErrNoSuchObject {
		// Create the storage pool db entry if it doesn't exist.
		_, err = storagePoolDBCreate(d.State(), instancePoolName, "", backupConf.Pool.Driver, backupConf.Pool.Config)
		if err != nil {
			return response.SmartError(errors.Wrap(err, "Create storage pool database entry"))
		}

		pool, err = storagePools.GetPoolByName(d.State(), instancePoolName)
		if err != nil {
			return response.SmartError(errors.Wrap(err, "Load storage pool database entry"))
		}
	} else if err != nil {
		return response.SmartError(errors.Wrap(err, "Find storage pool database entry"))
	}

	if backupConf.Pool.Name != instancePoolName {
		return response.BadRequest(fmt.Errorf(`The storage pool %q the instance was detected on does not match the storage pool %q specified in the backup file`, instancePoolName, backupConf.Pool.Name))
	}

	if backupConf.Pool.Driver != pool.Driver().Info().Name {
		return response.BadRequest(fmt.Errorf(`The storage pool's %q driver %q conflicts with the driver %q recorded in the instance's backup file`, instancePoolName, pool.Driver().Info().Name, backupConf.Pool.Driver))
	}

	// Check snapshots are consistent, and if not, if req.Force is true, then delete snapshots that do not exist in backup.yaml.
	existingSnapshots, err := pool.CheckInstanceBackupFileSnapshots(backupConf, projectName, req.Force, nil)
	if err != nil {
		if errors.Cause(err) == storagePools.ErrBackupSnapshotsMismatch {
			return response.InternalError(fmt.Errorf(`%s. Set "force" to discard non-existing snapshots`, err))
		}

		return response.InternalError(errors.Wrap(err, "Checking snapshots"))
	}

	// Check if a storage volume entry for the instance already exists.
	_, volume, ctVolErr := d.cluster.GetLocalStoragePoolVolume(projectName, req.Name, instanceDBVolType, pool.ID())
	if ctVolErr != nil {
		if ctVolErr != db.ErrNoSuchObject {
			return response.SmartError(ctVolErr)
		}
	}

	// If a storage volume entry exists only proceed if force was specified.
	if ctVolErr == nil && !req.Force {
		return response.BadRequest(fmt.Errorf(`Storage volume for instance %q already exists in the database. Set "force" to overwrite`, req.Name))
	}

	// Check if an entry for the instance already exists in the db.
	_, instanceErr := d.cluster.GetInstanceID(projectName, req.Name)
	if instanceErr != nil {
		if instanceErr != db.ErrNoSuchObject {
			return response.SmartError(instanceErr)
		}
	}

	// If a db entry exists only proceed if force was specified.
	if instanceErr == nil && !req.Force {
		return response.BadRequest(fmt.Errorf(`Entry for instance %q already exists in the database. Set "force" to overwrite`, req.Name))
	}

	if backupConf.Volume == nil {
		return response.BadRequest(fmt.Errorf(`No storage volume struct in the backup file found. The storage volume needs to be recovered manually`))
	}

	if ctVolErr == nil {
		if volume.Name != backupConf.Volume.Name {
			return response.BadRequest(fmt.Errorf(`The name %q of the storage volume is not identical to the instance's name "%s"`, volume.Name, req.Name))
		}

		if volume.Type != backupConf.Volume.Type {
			return response.BadRequest(fmt.Errorf(`The type %q of the storage volume is not identical to the instance's type %q`, volume.Type, backupConf.Volume.Type))
		}

		// Remove the storage volume db entry for the instance since force was specified.
		err := d.cluster.RemoveStoragePoolVolume(projectName, req.Name, instanceDBVolType, pool.ID())
		if err != nil {
			return response.SmartError(err)
		}
	}

	if instanceErr == nil {
		// Remove the storage volume db entry for the instance since force was specified.
		err := d.cluster.DeleteInstance(projectName, req.Name)
		if err != nil {
			return response.SmartError(err)
		}
	}

	// If recovering an on-disk instance, mark the filesystem as going through a recovery import, so that we
	// don't delete the on-disk files if an import error occurs.
	if recovery {
		importingFilePath := storagePools.InstanceImportingFilePath(instanceType, instancePoolName, projectName, req.Name)
		fd, err := os.Create(importingFilePath)
		if err != nil {
			return response.InternalError(err)
		}
		fd.Close()
		defer os.Remove(fd.Name())
	}

	baseImage := backupConf.Container.Config["volatile.base_image"]

	profiles, err := d.State().Cluster.GetProfiles(projectName, backupConf.Container.Profiles)
	if err != nil {
		return response.SmartError(errors.Wrapf(err, "Failed loading profiles for instance"))
	}

	// Add root device if needed.
	if backupConf.Container.Devices == nil {
		backupConf.Container.Devices = make(map[string]map[string]string, 0)
	}

	if backupConf.Container.ExpandedDevices == nil {
		backupConf.Container.ExpandedDevices = make(map[string]map[string]string, 0)
	}

	internalImportRootDevicePopulate(instancePoolName, backupConf.Container.Devices, backupConf.Container.ExpandedDevices, profiles)

	arch, err := osarch.ArchitectureId(backupConf.Container.Architecture)
	if err != nil {
		return response.SmartError(err)
	}

	revert := revert.New()
	defer revert.Fail()

	_, err = instance.CreateInternal(d.State(), db.InstanceArgs{
		Project:      projectName,
		Architecture: arch,
		BaseImage:    baseImage,
		Config:       backupConf.Container.Config,
		CreationDate: backupConf.Container.CreatedAt,
		Type:         instanceType,
		Description:  backupConf.Container.Description,
		Devices:      deviceConfig.NewDevices(backupConf.Container.Devices),
		Ephemeral:    backupConf.Container.Ephemeral,
		LastUsedDate: backupConf.Container.LastUsedAt,
		Name:         backupConf.Container.Name,
		Profiles:     backupConf.Container.Profiles,
		Stateful:     backupConf.Container.Stateful,
	}, true, revert)
	if err != nil {
		return response.SmartError(errors.Wrap(err, "Failed creating instance record"))
	}

	instancePath := storagePools.InstancePath(instanceType, projectName, req.Name, false)
	isPrivileged := false
	if backupConf.Container.Config["security.privileged"] == "" {
		isPrivileged = true
	}
	err = storagePools.CreateContainerMountpoint(instanceMountPoint, instancePath, isPrivileged)
	if err != nil {
		return response.InternalError(err)
	}

	for _, snap := range existingSnapshots {
		parts := strings.SplitN(snap.Name, shared.SnapshotDelimiter, 2)

		// Check if an entry for the snapshot already exists in the db.
		_, snapErr := d.cluster.GetInstanceSnapshotID(projectName, parts[0], parts[1])
		if snapErr != nil {
			if snapErr != db.ErrNoSuchObject {
				return response.SmartError(snapErr)
			}
		}

		// If a db entry exists only proceed if force was specified.
		if snapErr == nil && !req.Force {
			return response.BadRequest(fmt.Errorf(`Entry for snapshot %q already exists in the database. Set "force" to overwrite`, snap.Name))
		}

		// Check if a storage volume entry for the snapshot already exists.
		_, _, csVolErr := d.cluster.GetLocalStoragePoolVolume(projectName, snap.Name, instanceDBVolType, pool.ID())
		if csVolErr != nil {
			if csVolErr != db.ErrNoSuchObject {
				return response.SmartError(csVolErr)
			}
		}

		// If a storage volume entry exists only proceed if force was specified.
		if csVolErr == nil && !req.Force {
			return response.BadRequest(fmt.Errorf(`Storage volume for snapshot %q already exists in the database. Set "force" to overwrite`, snap.Name))
		}

		if snapErr == nil {
			err := d.cluster.DeleteInstance(projectName, snap.Name)
			if err != nil {
				return response.SmartError(err)
			}
		}

		if csVolErr == nil {
			err := d.cluster.RemoveStoragePoolVolume(projectName, snap.Name, instanceDBVolType, pool.ID())
			if err != nil {
				return response.SmartError(err)
			}
		}

		baseImage := snap.Config["volatile.base_image"]

		arch, err := osarch.ArchitectureId(snap.Architecture)
		if err != nil {
			return response.SmartError(err)
		}

		profiles, err := d.State().Cluster.GetProfiles(projectName, snap.Profiles)
		if err != nil {
			return response.SmartError(errors.Wrapf(err, "Failed loading profiles for instance snapshot %q", snap.Name))
		}

		// Add root device if needed.
		if snap.Devices == nil {
			snap.Devices = make(map[string]map[string]string, 0)
		}

		if snap.ExpandedDevices == nil {
			snap.ExpandedDevices = make(map[string]map[string]string, 0)
		}

		internalImportRootDevicePopulate(instancePoolName, snap.Devices, snap.ExpandedDevices, profiles)

		_, err = instance.CreateInternal(d.State(), db.InstanceArgs{
			Project:      projectName,
			Architecture: arch,
			BaseImage:    baseImage,
			Config:       snap.Config,
			CreationDate: snap.CreatedAt,
			Type:         instanceType,
			Snapshot:     true,
			Devices:      deviceConfig.NewDevices(snap.Devices),
			Ephemeral:    snap.Ephemeral,
			LastUsedDate: snap.LastUsedAt,
			Name:         snap.Name,
			Profiles:     snap.Profiles,
			Stateful:     snap.Stateful,
		}, true, revert)
		if err != nil {
			return response.SmartError(errors.Wrapf(err, "Failed creating instance snapshot record %q", snap.Name))
		}

		// Recreate missing mountpoints and symlinks.
		volStorageName := project.Instance(projectName, snap.Name)
		snapshotMountPoint := storageDrivers.GetVolumeMountPath(instancePoolName, instanceVolType, volStorageName)
		snapshotPath := storagePools.InstancePath(instanceType, projectName, req.Name, true)
		snapshotTargetPath := storageDrivers.GetVolumeSnapshotDir(instancePoolName, instanceVolType, volStorageName)

		err = storagePools.CreateSnapshotMountpoint(snapshotMountPoint, snapshotTargetPath, snapshotPath)
		if err != nil {
			return response.InternalError(err)
		}
	}

	revert.Success()
	return response.EmptySyncResponse
}

// internalImportRootDevicePopulate considers the local and expanded devices from backup.yaml as well as the
// expanded devices in the current profiles and if needed will populate localDevices with a new root disk config
// to attempt to maintain the same effective config as specified in backup.yaml. Where possible no new root disk
// device will be added, if the root disk config in the current profiles matches the effective backup.yaml config.
func internalImportRootDevicePopulate(instancePoolName string, localDevices map[string]map[string]string, expandedDevices map[string]map[string]string, profiles []api.Profile) {
	// First, check if localDevices from backup.yaml has a root disk.
	rootName, _, _ := shared.GetRootDiskDevice(localDevices)
	if rootName != "" {
		localDevices[rootName]["pool"] = instancePoolName

		return // Local root disk device has been set to target pool.
	}

	// Next check if expandedDevices from backup.yaml has a root disk.
	expandedRootName, expandedRootConfig, _ := shared.GetRootDiskDevice(expandedDevices)

	// Extract root disk from expanded profile devices.
	profileExpandedDevices := db.ExpandInstanceDevices(deviceConfig.NewDevices(localDevices), profiles)
	profileExpandedRootName, profileExpandedRootConfig, _ := shared.GetRootDiskDevice(profileExpandedDevices.CloneNative())

	// Record whether we need to add a new local disk device.
	addLocalDisk := false

	// We need to add a local root disk if the profiles don't have a root disk.
	if profileExpandedRootName == "" {
		addLocalDisk = true
	} else {
		// Check profile expanded root disk is in the correct pool
		if profileExpandedRootConfig["pool"] != instancePoolName {
			addLocalDisk = true
		} else {
			// Check profile expanded root disk config matches the old expanded disk in backup.yaml.
			// Excluding the "pool" property, which we ignore, as we have already checked the new
			// profile root disk matches the target pool name.
			if expandedRootName != "" {
				for k := range expandedRootConfig {
					if k == "pool" {
						continue // Ignore old pool name.
					}

					if expandedRootConfig[k] != profileExpandedRootConfig[k] {
						addLocalDisk = true
						break
					}
				}

				for k := range profileExpandedRootConfig {
					if k == "pool" {
						continue // Ignore old pool name.
					}

					if expandedRootConfig[k] != profileExpandedRootConfig[k] {
						addLocalDisk = true
						break
					}
				}
			}
		}
	}

	// Add local root disk entry if needed.
	if addLocalDisk {
		rootDev := map[string]string{
			"type": "disk",
			"path": "/",
			"pool": instancePoolName,
		}

		// Inherit any extra root disk config from the expanded root disk from backup.yaml.
		if expandedRootName != "" {
			for k, v := range expandedRootConfig {
				if _, found := rootDev[k]; !found {
					rootDev[k] = v
				}
			}
		}

		// If there is already a device called "root" in the instance's config, but it does not qualify as
		// a root disk, then try to find a free name for the new root disk device.
		rootDevName := "root"
		for i := 0; i < 100; i++ {
			if localDevices[rootDevName] == nil {
				break
			}

			rootDevName = fmt.Sprintf("root%d", i)
			continue
		}

		localDevices[rootDevName] = rootDev
	}
}

func internalGC(d *Daemon, r *http.Request) response.Response {
	logger.Infof("Started forced garbage collection run")
	runtime.GC()
	runtimeDebug.FreeOSMemory()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	logger.Infof("Heap allocated: %s", units.GetByteSizeString(int64(m.Alloc), 2))
	logger.Infof("Stack in use: %s", units.GetByteSizeString(int64(m.StackInuse), 2))
	logger.Infof("Requested from system: %s", units.GetByteSizeString(int64(m.Sys), 2))
	logger.Infof("Releasable to OS: %s", units.GetByteSizeString(int64(m.HeapIdle-m.HeapReleased), 2))

	logger.Infof("Completed forced garbage collection run")

	return response.EmptySyncResponse
}

func internalRAFTSnapshot(d *Daemon, r *http.Request) response.Response {
	logger.Infof("Started forced RAFT snapshot")
	err := d.gateway.Snapshot()
	if err != nil {
		logger.Errorf("Failed forced RAFT snapshot: %v", err)
		return response.InternalError(err)
	}

	logger.Infof("Completed forced RAFT snapshot")

	return response.EmptySyncResponse
}

// internalRecoverValidatePost is used to initiate a recovery validation scan.
type internalRecoverValidatePost struct {
	Pools []api.StoragePoolsPost `json:"pools" yaml:"pools"`
}

// internalRecoverValidateVolume provides info about a missing volume that the recovery validation scan found.
type internalRecoverValidateVolume struct {
	Name          string `json:"name" yaml:"name"`                   // Name of volume.
	Type          string `json:"type" yaml:"type"`                   // Same as Type from StorageVolumesPost (container, custom or virtual-machine).
	SnapshotCount int    `json:"snapshotCount" yaml:"snapshotCount"` // Count of snapshots found for volume.
	Project       string `json:"project" yaml:"project"`             // Project the volume belongs to.
	Pool          string `json:"pool" yaml:"pool"`                   // Pool the volume belongs to.
}

// internalRecoverValidateResult returns the result of the validation scan.
type internalRecoverValidateResult struct {
	UnknownVolumes   []internalRecoverValidateVolume // Volumes that could be imported.
	DependencyErrors []string                        // Errors that are preventing import from proceeding.
}

// internalRecoverImportPost is used to initiate a recovert import.
type internalRecoverImportPost struct {
	Pools []api.StoragePoolsPost `json:"pools" yaml:"pools"`
}

// internalRecoverScan provides the discovery and import functionality for both recovery validate and import steps.
func internalRecoverScan(d *Daemon, userPools []api.StoragePoolsPost, validateOnly bool) response.Response {
	var err error
	var projects map[string]*api.Project
	var projectProfiles map[string][]*api.Profile
	var projectNetworks map[string]map[int64]api.Network

	// Retrieve all project, profile and network info in a single transaction so we can use it for all
	// imported instances and volumes, and avoid repeatedly querying the same information.
	err = d.State().Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Load list of projects for validation.
		ps, err := tx.GetProjects(db.ProjectFilter{})
		if err != nil {
			return err
		}

		// Convert to map for lookups by name later.
		projects = make(map[string]*api.Project, len(ps))
		for i := range ps {
			projects[ps[i].Name] = &ps[i]
		}

		// Load list of project/profile names for validation.
		profiles, err := tx.GetProfiles(db.ProfileFilter{})
		if err != nil {
			return err
		}

		// Convert to map for lookups by project name later.
		projectProfiles = make(map[string][]*api.Profile)
		for _, profile := range profiles {
			if projectProfiles[profile.Project] == nil {
				projectProfiles[profile.Project] = []*api.Profile{db.ProfileToAPI(&profile)}
			} else {
				projectProfiles[profile.Project] = append(projectProfiles[profile.Project], db.ProfileToAPI(&profile))
			}
		}

		// Load list of project/network names for validation.
		projectNetworks, err = tx.GetCreatedNetworks()
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(errors.Wrapf(err, "Failed getting validate dependency check info"))
	}

	res := internalRecoverValidateResult{}

	// addDependencyError adds an error to the list of dependency errors if not already present in list.
	addDependencyError := func(err error) {
		errStr := err.Error()

		if !shared.StringInSlice(errStr, res.DependencyErrors) {
			res.DependencyErrors = append(res.DependencyErrors, errStr)
		}
	}

	// Used to store the unknown instance volumes for each pool & project.
	poolsProjectInsts := make(map[string]map[string][]*backup.Config)
	poolsProjectInstSnapshots := make(map[string]map[string]map[string][]*api.InstanceSnapshot)

	// Used to store a handle to each pool containing user supplied config.
	pools := make(map[string]storagePools.Pool)

	// Iterate the pools finding unknown volumes and perform validation.
	for _, p := range userPools {
		pool, err := storagePools.GetPoolByName(d.State(), p.Name)
		if err != nil {
			if errors.Cause(err) == db.ErrNoSuchObject {
				// If pool doesn't exist in DB, initialise a temporary pool instance using the
				// supplied pool info.
				poolInfo := api.StoragePool{
					Name:           p.Name,
					Driver:         p.Driver,
					StoragePoolPut: p.StoragePoolPut,
				}

				pool, err = storagePools.New(d.State(), &poolInfo)
				if err != nil {
					return response.SmartError(errors.Wrapf(err, "Failed to initialise unknown pool %q", p.Name))
				}

				err = pool.Driver().Validate(poolInfo.Config)
				if err != nil {
					return response.SmartError(errors.Wrapf(err, "Failed config validation for unknown pool %q", p.Name))
				}
			} else {
				return response.SmartError(errors.Wrapf(err, "Failed loading existing pool %q", p.Name))
			}
		}

		// Record this pool to be used during import stage, assuming validation passes.
		pools[p.Name] = pool

		// Try to mount the pool.
		_, err = pool.Mount()
		if err != nil {
			return response.SmartError(errors.Wrapf(err, "Failed mounting pool %q", pool.Name()))
		}

		// Unmount pool when done if not existing in DB after function has finished.
		// This way if we are dealing with an existing pool or have successfully created the DB record then
		// we won't unmount it. As we should leave successfully imported pools mounted.
		defer func() {
			cleanupPool := pools[pool.Name()]
			if cleanupPool != nil && cleanupPool.Status() == "" {
				cleanupPool.Unmount()
			}
		}()

		// Get list of instances on pool.
		poolProjectInsts, poolProjectInstSnapshots, err := pool.ListUnknownInstances(nil)
		if err != nil {
			if errors.Cause(err) == storageDrivers.ErrNotImplemented {
				logger.Error("Pool driver hasn't implemented recovery yet, skipping", log.Ctx{"pool": pool.Name(), "err": err})
			} else {
				return response.SmartError(errors.Wrapf(err, "Failed validating instances on pool %q", pool.Name()))
			}
		}

		// Store for consumption after validation scan to avoid needing to reprocess.
		poolsProjectInsts[p.Name] = poolProjectInsts
		poolsProjectInstSnapshots[p.Name] = poolProjectInstSnapshots

		// Check dependencies are met for each instance.
		for projectName, poolInsts := range poolProjectInsts {
			// Check project exists in database.
			instProject := projects[projectName]

			// Look up effective project names for profiles and networks.
			var profileProjectname string
			var networkProjectName string

			if instProject != nil {
				profileProjectname = project.ProfileProjectFromRecord(instProject)
				networkProjectName = project.NetworkProjectFromRecord(instProject)
			} else {
				addDependencyError(fmt.Errorf("Project %q", projectName))
			}

			for _, poolInst := range poolInsts {
				if instProject != nil {
					// Check that the instance's profile dependencies are met.
					for _, poolInstProfileName := range poolInst.Container.Profiles {
						foundProfile := false
						for _, profile := range projectProfiles[profileProjectname] {
							if profile.Name == poolInstProfileName {
								foundProfile = true
							}
						}

						if !foundProfile {
							addDependencyError(fmt.Errorf("Profile %q in project %q", poolInstProfileName, projectName))
						}
					}

					// Check that the instance's NIC network dependencies are met.
					for _, devConfig := range poolInst.Container.ExpandedDevices {
						if devConfig["type"] != "nic" {
							continue
						}

						if devConfig["network"] == "" {
							continue
						}

						foundNetwork := false
						for _, n := range projectNetworks[networkProjectName] {
							if n.Name == devConfig["network"] {
								foundNetwork = true
								break
							}
						}

						if !foundNetwork {
							addDependencyError(fmt.Errorf("Network %q in project %q", devConfig["network"], projectName))
						}
					}
				}
			}
		}
	}

	// If in validation mode or if there are dependency errors, return discovered unknown volumes, along with
	// any dependency errors.
	if validateOnly || len(res.DependencyErrors) > 0 {
		for poolName, poolProjectInsts := range poolsProjectInsts {
			for projectName, poolInsts := range poolProjectInsts {
				for _, poolInst := range poolInsts {
					res.UnknownVolumes = append(res.UnknownVolumes, internalRecoverValidateVolume{
						Type:          poolInst.Container.Type,
						Name:          poolInst.Container.Name,
						Pool:          poolName,
						Project:       projectName,
						SnapshotCount: len(poolInst.Snapshots),
					})
				}
			}
		}

		return response.SyncResponse(true, &res)
	}

	// If in import mode and no dependency errors, then re-create missing DB records.
	revert := revert.New()
	defer revert.Fail()

	// Create any missing instance and storage volume records.
	for _, pool := range pools {
		for projectName, poolInsts := range poolsProjectInsts[pool.Name()] {
			instProject := projects[projectName]

			if instProject == nil {
				// Shouldn't happen as we validated this above, but be sure for safety.
				return response.SmartError(fmt.Errorf("Project %q not found", projectName))
			}

			profileProjectname := project.ProfileProjectFromRecord(instProject)

			for _, poolInst := range poolInsts {
				// Create missing storage pool DB record if neeed.
				if pool.Status() == "" {
					if poolInst.Pool != nil {
						// Create storage pool DB record from config in the instance.
						logger.Info("Creating storage pool DB record from instance config", log.Ctx{"name": poolInst.Pool.Name, "description": poolInst.Pool.Description, "driver": poolInst.Pool.Driver, "config": poolInst.Pool.Config})
						_, err = dbStoragePoolCreateAndUpdateCache(d.State(), poolInst.Pool.Name, poolInst.Pool.Description, poolInst.Pool.Driver, poolInst.Pool.Config)
						if err != nil {
							return response.SmartError(errors.Wrapf(err, "Failed creating storage pool %q database entry", pool.Name()))
						}
					} else {
						// Create storage pool DB record from config supplied by user.
						poolDriverName := pool.Driver().Info().Name
						poolDriverConfig := pool.Driver().Config()
						logger.Info("Creating storage pool DB record from user config", log.Ctx{"name": pool.Name(), "driver": poolDriverName, "config": poolDriverConfig})
						_, err = dbStoragePoolCreateAndUpdateCache(d.State(), pool.Name(), "", poolDriverName, poolDriverConfig)
						if err != nil {
							return response.SmartError(errors.Wrapf(err, "Failed creating storage pool %q database entry", pool.Name()))
						}
					}

					revert.Add(func() {
						dbStoragePoolDeleteAndUpdateCache(d.State(), pool.Name())
					})

					newPool, err := storagePools.GetPoolByName(d.State(), pool.Name())
					if err != nil {
						return response.SmartError(errors.Wrapf(err, "Failed load created storage pool %q", pool.Name()))
					}

					// Record this newly created pool so that defer doesn't unmount on return.
					pools[pool.Name()] = newPool
					pool = newPool // Replace temporary pool handle with proper one from DB.

					revert.Add(func() {
						pool.Unmount() // Defer won't do it now, so unmount on failure.
					})
				}

				// Recover instance.
				profiles := make([]api.Profile, 0, len(poolInst.Container.Profiles))
				for _, profileName := range poolInst.Container.Profiles {
					for i := range projectProfiles[profileProjectname] {
						if projectProfiles[profileProjectname][i].Name == profileName {
							profiles = append(profiles, *projectProfiles[profileProjectname][i])
						}
					}
				}

				inst, err := internalRecoverImportInstance(d.State(), pool, projectName, poolInst, profiles, revert)
				if err != nil {
					return response.SmartError(errors.Wrapf(err, "Failed importing instance %q in project %q", poolInst.Container.Name, projectName))
				}

				// Recover instance snapshots.
				for _, poolInstSnap := range poolsProjectInstSnapshots[pool.Name()][projectName][poolInst.Container.Name] {
					profiles := make([]api.Profile, 0, len(poolInstSnap.Profiles))
					for _, profileName := range poolInstSnap.Profiles {
						for i := range projectProfiles[profileProjectname] {
							if projectProfiles[profileProjectname][i].Name == profileName {
								profiles = append(profiles, *projectProfiles[profileProjectname][i])
							}
						}
					}

					err = internalRecoverImportInstanceSnapshot(d.State(), pool, projectName, poolInst, poolInstSnap, profiles, revert)
					if err != nil {
						return response.SmartError(errors.Wrapf(err, "Failed importing instance %q snapshot %q in project %q", poolInst.Container.Name, poolInstSnap.Name, projectName))
					}
				}

				// Recreate instance mount path and symlinks (must come after snapshot recovery).
				err = pool.ImportInstance(inst, nil)
				if err != nil {
					return response.SmartError(errors.Wrap(err, "Failed importing instance"))
				}

				// Reinitialise the instance's root disk quota even if no size specified (allows the storage driver the
				// opportunity to reinitialise the quota based on the new storage volume's DB ID).
				_, rootConfig, err := shared.GetRootDiskDevice(inst.ExpandedDevices().CloneNative())
				if err == nil {
					err = pool.SetInstanceQuota(inst, rootConfig["size"], rootConfig["size.state"], nil)
					if err != nil {
						return response.SmartError(errors.Wrapf(err, "Failed reinitializing root disk quota %q", rootConfig["size"]))
					}
				}
			}
		}
	}

	revert.Success()
	return response.EmptySyncResponse
}

// internalRecoverImportInstance recreates the database records for an instance and returns the new instance.
func internalRecoverImportInstance(s *state.State, pool storagePools.Pool, projectName string, poolInst *backup.Config, profiles []api.Profile, revert *revert.Reverter) (instance.Instance, error) {
	// Add root device if needed.
	if poolInst.Container.Devices == nil {
		poolInst.Container.Devices = make(map[string]map[string]string, 0)
	}

	if poolInst.Container.ExpandedDevices == nil {
		poolInst.Container.ExpandedDevices = make(map[string]map[string]string, 0)
	}

	internalImportRootDevicePopulate(pool.Name(), poolInst.Container.Devices, poolInst.Container.ExpandedDevices, profiles)

	arch, err := osarch.ArchitectureId(poolInst.Container.Architecture)
	if err != nil {
		return nil, err
	}

	instanceType, err := instancetype.New(poolInst.Container.Type)
	if err != nil {
		return nil, err
	}

	inst, err := instance.CreateInternal(s, db.InstanceArgs{
		Project:      projectName,
		Architecture: arch,
		BaseImage:    poolInst.Container.Config["volatile.base_image"],
		Config:       poolInst.Container.Config,
		CreationDate: poolInst.Container.CreatedAt,
		Type:         instanceType,
		Description:  poolInst.Container.Description,
		Devices:      deviceConfig.NewDevices(poolInst.Container.Devices),
		Ephemeral:    poolInst.Container.Ephemeral,
		LastUsedDate: poolInst.Container.LastUsedAt,
		Name:         poolInst.Container.Name,
		Profiles:     poolInst.Container.Profiles,
		Stateful:     poolInst.Container.Stateful,
	}, false, revert)
	if err != nil {
		return nil, errors.Wrap(err, "Failed creating instance record")
	}

	return inst, err
}

// internalRecoverImportInstance recreates the database records for an instance snapshot.
func internalRecoverImportInstanceSnapshot(s *state.State, pool storagePools.Pool, projectName string, poolInst *backup.Config, snap *api.InstanceSnapshot, profiles []api.Profile, revert *revert.Reverter) error {
	// Add root device if needed.
	if snap.Devices == nil {
		snap.Devices = make(map[string]map[string]string, 0)
	}

	if snap.ExpandedDevices == nil {
		snap.ExpandedDevices = make(map[string]map[string]string, 0)
	}

	internalImportRootDevicePopulate(pool.Name(), snap.Devices, snap.ExpandedDevices, profiles)

	arch, err := osarch.ArchitectureId(snap.Architecture)
	if err != nil {
		return err
	}

	instanceType, err := instancetype.New(poolInst.Container.Type)
	if err != nil {
		return err
	}

	_, err = instance.CreateInternal(s, db.InstanceArgs{
		Project:      projectName,
		Architecture: arch,
		BaseImage:    snap.Config["volatile.base_image"],
		Config:       snap.Config,
		CreationDate: snap.CreatedAt,
		Type:         instanceType,
		Snapshot:     true,
		Devices:      deviceConfig.NewDevices(snap.Devices),
		Ephemeral:    snap.Ephemeral,
		LastUsedDate: snap.LastUsedAt,
		Name:         poolInst.Container.Name + shared.SnapshotDelimiter + snap.Name,
		Profiles:     snap.Profiles,
		Stateful:     snap.Stateful,
	}, false, revert)
	if err != nil {
		return errors.Wrapf(err, "Failed creating instance snapshot record %q", snap.Name)
	}

	return nil
}

// internalRecoverValidate validates the requested pools to be recovered.
func internalRecoverValidate(d *Daemon, r *http.Request) response.Response {
	// Parse the request.
	req := &internalRecoverValidatePost{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	return internalRecoverScan(d, req.Pools, true)
}

// internalRecoverImport performs the pool volume recovery.
func internalRecoverImport(d *Daemon, r *http.Request) response.Response {
	// Parse the request.
	req := &internalRecoverImportPost{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	return internalRecoverScan(d, req.Pools, false)
}
