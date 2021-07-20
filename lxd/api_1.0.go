package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/config"
	"github.com/lxc/lxd/lxd/db"
	instanceDrivers "github.com/lxc/lxd/lxd/instance/drivers"
	"github.com/lxc/lxd/lxd/lifecycle"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/rbac"
	"github.com/lxc/lxd/lxd/request"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/version"
	"github.com/pkg/errors"
)

var api10Cmd = APIEndpoint{
	Get:   APIEndpointAction{Handler: api10Get, AllowUntrusted: true},
	Patch: APIEndpointAction{Handler: api10Patch},
	Put:   APIEndpointAction{Handler: api10Put},
}

var api10 = []APIEndpoint{
	api10Cmd,
	api10ResourcesCmd,
	certificateCmd,
	certificatesCmd,
	clusterCmd,
	clusterNodeCmd,
	clusterNodeStateCmd,
	clusterNodesCmd,
	clusterCertificateCmd,
	instanceBackupCmd,
	instanceBackupExportCmd,
	instanceBackupsCmd,
	instanceCmd,
	instanceConsoleCmd,
	instanceExecCmd,
	instanceFileCmd,
	instanceLogCmd,
	instanceLogsCmd,
	instanceMetadataCmd,
	instanceMetadataTemplatesCmd,
	instancesCmd,
	instanceSnapshotCmd,
	instanceSnapshotsCmd,
	instanceStateCmd,
	eventsCmd,
	imageAliasCmd,
	imageAliasesCmd,
	imageCmd,
	imageExportCmd,
	imageRefreshCmd,
	imagesCmd,
	imageSecretCmd,
	networkCmd,
	networkLeasesCmd,
	networksCmd,
	networkStateCmd,
	networkACLCmd,
	networkACLsCmd,
	operationCmd,
	operationsCmd,
	operationWait,
	operationWebsocket,
	profileCmd,
	profilesCmd,
	projectCmd,
	projectsCmd,
	projectStateCmd,
	storagePoolCmd,
	storagePoolResourcesCmd,
	storagePoolsCmd,
	storagePoolVolumesCmd,
	storagePoolVolumeSnapshotsTypeCmd,
	storagePoolVolumeSnapshotTypeCmd,
	storagePoolVolumesTypeCmd,
	storagePoolVolumeTypeCmd,
	storagePoolVolumeTypeCustomBackupsCmd,
	storagePoolVolumeTypeCustomBackupCmd,
	storagePoolVolumeTypeCustomBackupExportCmd,
	storagePoolVolumeTypeStateCmd,
	warningsCmd,
	warningCmd,
}

// swagger:operation GET /1.0?public server server_get_untrusted
//
// Get the server environment
//
// Shows a small subset of the server environment and configuration
// which is required by untrusted clients to reach a server.
//
// The `?public` part of the URL isn't required, it's simply used to
// separate the two behaviors of this endpoint.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: Server environment and configuration
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/ServerUntrusted"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0 server server_get
//
// Get the server environment and configuration
//
// Shows the full server environment and configuration.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
// responses:
//   "200":
//     description: Server environment and configuration
//     schema:
//       type: object
//       description: Sync response
//       properties:
//         type:
//           type: string
//           description: Response type
//           example: sync
//         status:
//           type: string
//           description: Status description
//           example: Success
//         status_code:
//           type: integer
//           description: Status code
//           example: 200
//         metadata:
//           $ref: "#/definitions/Server"
//   "500":
//     $ref: "#/responses/InternalServerError"
func api10Get(d *Daemon, r *http.Request) response.Response {
	authMethods := []string{"tls"}
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		config, err := cluster.ConfigLoad(tx)
		if err != nil {
			return err
		}

		candidURL, _, _, _ := config.CandidServer()
		rbacURL, _, _, _, _, _, _ := config.RBACServer()
		if candidURL != "" || rbacURL != "" {
			authMethods = append(authMethods, "candid")
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}
	srv := api.ServerUntrusted{
		APIExtensions: version.APIExtensions,
		APIStatus:     "stable",
		APIVersion:    version.APIVersion,
		Public:        false,
		Auth:          "untrusted",
		AuthMethods:   authMethods,
	}

	// If untrusted, return now
	if d.checkTrustedClient(r) != nil {
		return response.SyncResponseETag(true, srv, nil)
	}

	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	srv.Auth = "trusted"

	uname, err := shared.Uname()
	if err != nil {
		return response.InternalError(err)
	}

	address, err := node.HTTPSAddress(d.db)
	if err != nil {
		return response.InternalError(err)
	}
	addresses, err := util.ListenAddresses(address)
	if err != nil {
		return response.InternalError(err)
	}

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	// When clustered, use the node name, otherwise use the hostname.
	var serverName string
	if clustered {
		err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
			serverName, err = tx.GetLocalNodeName()
			return err
		})
		if err != nil {
			return response.SmartError(err)
		}
	} else {
		hostname, err := os.Hostname()
		if err != nil {
			return response.SmartError(err)
		}
		serverName = hostname
	}

	certificate := string(d.endpoints.NetworkPublicKey())
	var certificateFingerprint string
	if certificate != "" {
		certificateFingerprint, err = shared.CertFingerprintStr(certificate)
		if err != nil {
			return response.InternalError(err)
		}
	}

	architectures := []string{}

	for _, architecture := range d.os.Architectures {
		architectureName, err := osarch.ArchitectureName(architecture)
		if err != nil {
			return response.InternalError(err)
		}
		architectures = append(architectures, architectureName)
	}

	projectName := r.FormValue("project")
	if projectName == "" {
		projectName = project.Default
	}

	osInfo, err := osarch.GetLSBRelease()
	if err != nil {
		return response.InternalError(err)
	}

	env := api.ServerEnvironment{
		Addresses:              addresses,
		Architectures:          architectures,
		Certificate:            certificate,
		CertificateFingerprint: certificateFingerprint,
		Kernel:                 uname.Sysname,
		KernelArchitecture:     uname.Machine,
		KernelVersion:          uname.Release,
		OSName:                 osInfo["NAME"],
		OSVersion:              osInfo["VERSION_ID"],
		Project:                projectName,
		Server:                 "lxd",
		ServerPid:              os.Getpid(),
		ServerVersion:          version.Version,
		ServerClustered:        clustered,
		ServerName:             serverName,
		Firewall:               fmt.Sprintf("%s", d.firewall),
	}

	env.KernelFeatures = map[string]string{
		"netnsid_getifaddrs":        fmt.Sprintf("%v", d.os.NetnsGetifaddrs),
		"uevent_injection":          fmt.Sprintf("%v", d.os.UeventInjection),
		"unpriv_fscaps":             fmt.Sprintf("%v", d.os.VFS3Fscaps),
		"seccomp_listener":          fmt.Sprintf("%v", d.os.SeccompListener),
		"seccomp_listener_continue": fmt.Sprintf("%v", d.os.SeccompListenerContinue),
		"shiftfs":                   fmt.Sprintf("%v", d.os.Shiftfs),
	}

	for _, driverInfo := range instanceDrivers.SupportedInstanceTypes() {
		if env.Driver != "" {
			env.Driver = env.Driver + " | " + driverInfo.Name
		} else {
			env.Driver = driverInfo.Name
		}

		// Get the version of the instance drivers in use.
		if env.DriverVersion != "" {
			env.DriverVersion = env.DriverVersion + " | " + driverInfo.Version
		} else {
			env.DriverVersion = driverInfo.Version
		}
	}

	if d.os.LXCFeatures != nil {
		env.LXCFeatures = map[string]string{}
		for k, v := range d.os.LXCFeatures {
			env.LXCFeatures[k] = fmt.Sprintf("%v", v)
		}
	}

	supportedStorageDrivers, usedStorageDrivers := readStoragePoolDriversCache()
	for driver, version := range usedStorageDrivers {
		if env.Storage != "" {
			env.Storage = env.Storage + " | " + driver
		} else {
			env.Storage = driver
		}

		// Get the version of the storage drivers in use.
		if env.StorageVersion != "" {
			env.StorageVersion = env.StorageVersion + " | " + version
		} else {
			env.StorageVersion = version
		}
	}

	env.StorageSupportedDrivers = supportedStorageDrivers

	fullSrv := api.Server{ServerUntrusted: srv}
	fullSrv.Environment = env

	if rbac.UserIsAdmin(r) {
		fullSrv.Config, err = daemonConfigRender(d.State())
		if err != nil {
			return response.InternalError(err)
		}
	}

	return response.SyncResponseETag(true, fullSrv, fullSrv.Config)
}

// swagger:operation PUT /1.0 server server_put
//
// Update the server configuration
//
// Updates the entire server configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
//   - in: body
//     name: server
//     description: Server configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ServerPut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "412":
//     $ref: "#/responses/PreconditionFailed"
//   "500":
//     $ref: "#/responses/InternalServerError"
func api10Put(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	req := api.ServerPut{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// If this is a notification from a cluster node, just run the triggers
	// for reacting to the values that changed.
	if isClusterNotification(r) {
		logger.Debugf("Handling config changed notification")
		changed := make(map[string]string)
		for key, value := range req.Config {
			changed[key] = value.(string)
		}
		var config *cluster.Config
		err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
			var err error
			config, err = cluster.ConfigLoad(tx)
			return err
		})
		if err != nil {
			return response.SmartError(err)
		}
		err = doApi10UpdateTriggers(d, nil, changed, nil, config)
		if err != nil {
			return response.SmartError(err)
		}
		return response.EmptySyncResponse
	}

	render, err := daemonConfigRender(d.State())
	if err != nil {
		return response.SmartError(err)
	}
	err = util.EtagCheck(r, render)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	return doApi10Update(d, r, req, false)
}

// swagger:operation PATCH /1.0 server server_patch
//
// Partially update the server configuration
//
// Updates a subset of the server configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: target
//     description: Cluster member name
//     type: string
//     example: lxd01
//   - in: body
//     name: server
//     description: Server configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ServerPut"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "412":
//     $ref: "#/responses/PreconditionFailed"
//   "500":
//     $ref: "#/responses/InternalServerError"
func api10Patch(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := forwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	render, err := daemonConfigRender(d.State())
	if err != nil {
		return response.InternalError(err)
	}
	err = util.EtagCheck(r, render)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	req := api.ServerPut{}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	if req.Config == nil {
		return response.EmptySyncResponse
	}

	return doApi10Update(d, r, req, true)
}

func doApi10Update(d *Daemon, r *http.Request, req api.ServerPut, patch bool) response.Response {
	s := d.State()

	// First deal with config specific to the local daemon
	nodeValues := map[string]interface{}{}

	for key := range node.ConfigSchema {
		value, ok := req.Config[key]
		if ok {
			nodeValues[key] = value
			delete(req.Config, key)
		}
	}

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.InternalError(errors.Wrap(err, "Failed to check for cluster state"))
	}

	nodeChanged := map[string]string{}
	var newNodeConfig *node.Config
	err = d.db.Transaction(func(tx *db.NodeTx) error {
		var err error
		newNodeConfig, err = node.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "Failed to load node config")
		}

		// We currently don't allow changing the cluster.https_address
		// once it's set.
		curClusterAddress := newNodeConfig.ClusterAddress()
		newClusterAddress, ok := nodeValues["cluster.https_address"]

		if clustered && ok && curClusterAddress != "" && !util.IsAddressCovered(newClusterAddress.(string), curClusterAddress) {
			return fmt.Errorf("Changing cluster.https_address is currently not supported")
		}

		// Validate the storage volumes
		if nodeValues["storage.backups_volume"] != nil && nodeValues["storage.backups_volume"] != newNodeConfig.StorageBackupsVolume() {
			err := daemonStorageValidate(s, nodeValues["storage.backups_volume"].(string))
			if err != nil {
				return err
			}
		}

		if nodeValues["storage.images_volume"] != nil && nodeValues["storage.images_volume"] != newNodeConfig.StorageImagesVolume() {
			err := daemonStorageValidate(s, nodeValues["storage.images_volume"].(string))
			if err != nil {
				return err
			}
		}

		if patch {
			nodeChanged, err = newNodeConfig.Patch(nodeValues)
		} else {
			nodeChanged, err = newNodeConfig.Replace(nodeValues)
		}
		return err
	})
	if err != nil {
		switch err.(type) {
		case config.ErrorList:
			return response.BadRequest(err)
		default:
			return response.SmartError(err)
		}
	}

	// Validate global configuration
	hasRBAC := false
	hasCandid := false
	for k, v := range req.Config {
		if v == "" {
			continue
		}

		if strings.HasPrefix(k, "candid.") {
			hasCandid = true
		} else if strings.HasPrefix(k, "rbac.") {
			hasRBAC = true
		}

		if hasCandid && hasRBAC {
			return response.BadRequest(fmt.Errorf("RBAC and Candid are mutually exclusive"))
		}
	}

	// Then deal with cluster wide configuration
	var clusterChanged map[string]string
	var newClusterConfig *cluster.Config
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		newClusterConfig, err = cluster.ConfigLoad(tx)
		if err != nil {
			return errors.Wrap(err, "Failed to load cluster config")
		}

		if patch {
			clusterChanged, err = newClusterConfig.Patch(req.Config)
		} else {
			clusterChanged, err = newClusterConfig.Replace(req.Config)
		}
		return err
	})
	if err != nil {
		switch err.(type) {
		case config.ErrorList:
			return response.BadRequest(err)
		default:
			return response.SmartError(err)
		}
	}

	// Notify the other nodes about changes
	notifier, err := cluster.NewNotifier(s, d.endpoints.NetworkCert(), d.serverCert(), cluster.NotifyAlive)
	if err != nil {
		return response.SmartError(err)
	}
	err = notifier(func(client lxd.InstanceServer) error {
		server, etag, err := client.GetServer()
		if err != nil {
			return err
		}
		serverPut := server.Writable()
		serverPut.Config = make(map[string]interface{})
		// Only propagated cluster-wide changes
		for key, value := range clusterChanged {
			serverPut.Config[key] = value
		}
		return client.UpdateServer(serverPut, etag)
	})
	if err != nil {
		logger.Debugf("Failed to notify other nodes about config change: %v", err)
		return response.SmartError(err)
	}

	err = doApi10UpdateTriggers(d, nodeChanged, clusterChanged, newNodeConfig, newClusterConfig)
	if err != nil {
		return response.SmartError(err)
	}

	d.State().Events.SendLifecycle(project.Default, lifecycle.ConfigUpdated.Event(request.CreateRequestor(r), nil))

	return response.EmptySyncResponse
}

func doApi10UpdateTriggers(d *Daemon, nodeChanged, clusterChanged map[string]string, nodeConfig *node.Config, clusterConfig *cluster.Config) error {
	// Don't apply changes to settings until daemon is full started.
	<-d.readyChan

	s := d.State()

	maasChanged := false
	candidChanged := false
	rbacChanged := false

	for key := range clusterChanged {
		switch key {
		case "core.https_trusted_proxy":
			d.endpoints.NetworkUpdateTrustedProxy(clusterChanged[key])
		case "core.proxy_http":
			fallthrough
		case "core.proxy_https":
			fallthrough
		case "core.proxy_ignore_hosts":
			daemonConfigSetProxy(d, clusterConfig)
		case "maas.api.url":
			fallthrough
		case "maas.api.key":
			maasChanged = true
		case "candid.domains":
			fallthrough
		case "candid.expiry":
			fallthrough
		case "candid.api.key":
			fallthrough
		case "candid.api.url":
			candidChanged = true
		case "cluster.images_minimal_replica":
			autoSyncImages(d.ctx, d)
		case "images.auto_update_interval":
			fallthrough
		case "images.remote_cache_expiry":
			if !d.os.MockMode {
				d.taskPruneImages.Reset()
			}
		case "rbac.agent.url":
			fallthrough
		case "rbac.agent.username":
			fallthrough
		case "rbac.agent.private_key":
			fallthrough
		case "rbac.agent.public_key":
			fallthrough
		case "rbac.api.url":
			fallthrough
		case "rbac.api.key":
			fallthrough
		case "rbac.expiry":
			rbacChanged = true
		}
	}

	// Look for changed values. We do it sequentially because some keys are
	// correlated with others, and need to be processed first (for example
	// core.https_address need to be processed before
	// cluster.https_address).

	_, ok := nodeChanged["maas.machine"]
	if ok {
		maasChanged = true
	}

	value, ok := nodeChanged["core.https_address"]
	if ok {
		err := d.endpoints.NetworkUpdateAddress(value)
		if err != nil {
			return err
		}
		d.endpoints.NetworkUpdateTrustedProxy(clusterConfig.HTTPSTrustedProxy())
	}

	value, ok = nodeChanged["cluster.https_address"]
	if ok {
		err := d.endpoints.ClusterUpdateAddress(value)
		if err != nil {
			return err
		}
		d.endpoints.NetworkUpdateTrustedProxy(clusterConfig.HTTPSTrustedProxy())
	}

	value, ok = nodeChanged["core.debug_address"]
	if ok {
		err := d.endpoints.PprofUpdateAddress(value)
		if err != nil {
			return err
		}
	}

	value, ok = nodeChanged["storage.backups_volume"]
	if ok {
		err := daemonStorageMove(s, "backups", value)
		if err != nil {
			return err
		}
	}

	value, ok = nodeChanged["storage.images_volume"]
	if ok {
		err := daemonStorageMove(s, "images", value)
		if err != nil {
			return err
		}
	}

	if maasChanged {
		url, key := clusterConfig.MAASController()
		machine := nodeConfig.MAASMachine()
		err := d.setupMAASController(url, key, machine)
		if err != nil {
			return err
		}
	}

	if candidChanged {
		apiURL, apiKey, expiry, domains := clusterConfig.CandidServer()
		err := d.setupExternalAuthentication(apiURL, apiKey, expiry, domains)
		if err != nil {
			return err
		}
	}

	if rbacChanged {
		apiURL, apiKey, apiExpiry, agentURL, agentUsername, agentPrivateKey, agentPublicKey := clusterConfig.RBACServer()

		// Since RBAC seems to have been set up already, we need to disable it temporarily
		if d.rbac != nil {
			err := d.setupExternalAuthentication("", "", 0, "")
			if err != nil {
				return err
			}

			d.rbac.StopStatusCheck()
			d.rbac = nil
		}

		err := d.setupRBACServer(apiURL, apiKey, apiExpiry, agentURL, agentUsername, agentPrivateKey, agentPublicKey)
		if err != nil {
			return err
		}
	}

	_, ok = clusterChanged["cluster.offline_threshold"]
	if ok {
		d.gateway.HeartbeatOfflineThreshold = clusterConfig.OfflineThreshold()
		d.taskClusterHeartbeat.Reset()
	}

	return nil
}
