package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/lifecycle"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/request"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/version"
)

var profilesCmd = APIEndpoint{
	Path: "profiles",

	Get:  APIEndpointAction{Handler: profilesGet, AccessHandler: allowProjectPermission("profiles", "view")},
	Post: APIEndpointAction{Handler: profilesPost, AccessHandler: allowProjectPermission("profiles", "manage-profiles")},
}

var profileCmd = APIEndpoint{
	Path: "profiles/{name}",

	Delete: APIEndpointAction{Handler: profileDelete, AccessHandler: allowProjectPermission("profiles", "manage-profiles")},
	Get:    APIEndpointAction{Handler: profileGet, AccessHandler: allowProjectPermission("profiles", "view")},
	Patch:  APIEndpointAction{Handler: profilePatch, AccessHandler: allowProjectPermission("profiles", "manage-profiles")},
	Post:   APIEndpointAction{Handler: profilePost, AccessHandler: allowProjectPermission("profiles", "manage-profiles")},
	Put:    APIEndpointAction{Handler: profilePut, AccessHandler: allowProjectPermission("profiles", "manage-profiles")},
}

// swagger:operation GET /1.0/profiles profiles profiles_get
//
// Get the profiles
//
// Returns a list of profiles (URLs).
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
// responses:
//   "200":
//     description: API endpoints
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
//           type: array
//           description: List of endpoints
//           items:
//             type: string
//           example: |-
//             [
//               "/1.0/profiles/default",
//               "/1.0/profiles/foo"
//             ]
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/profiles?recursion=1 profiles profiles_get_recursion1
//
// Get the profiles
//
// Returns a list of profiles (structs).
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
// responses:
//   "200":
//     description: API endpoints
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
//           type: array
//           description: List of profiles
//           items:
//             $ref: "#/definitions/Profile"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func profilesGet(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.ProfileProject(d.State().Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	recursion := util.IsRecursionRequest(r)

	var result interface{}
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		filter := db.ProfileFilter{
			Project: projectName,
		}
		if recursion {
			profiles, err := tx.GetProfiles(filter)
			if err != nil {
				return err
			}
			apiProfiles := make([]*api.Profile, len(profiles))
			for i, profile := range profiles {
				apiProfiles[i] = db.ProfileToAPI(&profile)
				apiProfiles[i].UsedBy = project.FilterUsedBy(r, apiProfiles[i].UsedBy)
			}

			result = apiProfiles
		} else {
			result, err = tx.GetProfileURIs(filter)
		}
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, result)
}

// swagger:operation POST /1.0/profiles profiles profiles_post
//
// Add a profile
//
// Creates a new profile.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: body
//     name: profile
//     description: Profile
//     required: true
//     schema:
//       $ref: "#/definitions/ProfilesPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func profilesPost(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.ProfileProject(d.State().Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	req := api.ProfilesPost{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("No name provided"))
	}

	if strings.Contains(req.Name, "/") {
		return response.BadRequest(fmt.Errorf("Profile names may not contain slashes"))
	}

	if shared.StringInSlice(req.Name, []string{".", ".."}) {
		return response.BadRequest(fmt.Errorf("Invalid profile name %q", req.Name))
	}

	err = validateProfileConfig(d, req.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	// At this point we don't know the instance type, so just use instancetype.Any type for validation.
	err = instance.ValidDevices(d.State(), d.cluster, projectName, instancetype.Any, deviceConfig.NewDevices(req.Devices), false)
	if err != nil {
		return response.BadRequest(err)
	}

	// Update DB entry.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		current, _ := tx.GetProfile(projectName, req.Name)
		if current != nil {
			return fmt.Errorf("The profile already exists")
		}

		profile := db.Profile{
			Project:     projectName,
			Name:        req.Name,
			Description: req.Description,
			Config:      req.Config,
			Devices:     req.Devices,
		}
		_, err = tx.CreateProfile(profile)
		return err
	})
	if err != nil {
		return response.SmartError(errors.Wrapf(err, "Error inserting %q into database", req.Name))
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectName, lifecycle.ProfileCreated.Event(req.Name, projectName, requestor, nil))

	return response.SyncResponseLocation(true, nil, fmt.Sprintf("/%s/profiles/%s", version.APIVersion, req.Name))
}

// swagger:operation GET /1.0/profiles/{name} profiles profile_get
//
// Get the profile
//
// Gets a specific profile.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
// responses:
//   "200":
//     description: Profile
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
//           $ref: "#/definitions/Profile"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func profileGet(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.ProfileProject(d.State().Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	name := mux.Vars(r)["name"]

	var resp *api.Profile

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		profile, err := tx.GetProfile(projectName, name)
		if err != nil {
			return errors.Wrap(err, "Fetch profile")
		}

		resp = db.ProfileToAPI(profile)
		resp.UsedBy = project.FilterUsedBy(r, resp.UsedBy)

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	etag := []interface{}{resp.Config, resp.Description, resp.Devices}
	return response.SyncResponseETag(true, resp, etag)
}

// swagger:operation PUT /1.0/profiles/{name} profiles profile_put
//
// Update the profile
//
// Updates the entire profile configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: body
//     name: profile
//     description: Profile configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ProfilePut"
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
func profilePut(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.ProfileProject(d.State().Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	name := mux.Vars(r)["name"]

	if isClusterNotification(r) {
		// In this case the ProfilePut request payload contains information about the old profile, since
		// the new one has already been saved in the database.
		old := api.ProfilePut{}
		err := json.NewDecoder(r.Body).Decode(&old)
		if err != nil {
			return response.BadRequest(err)
		}

		err = doProfileUpdateCluster(d, projectName, name, old)
		return response.SmartError(err)
	}

	var profile *api.Profile

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		current, err := tx.GetProfile(projectName, name)
		if err != nil {
			return errors.Wrapf(err, "Failed to retrieve profile %q", name)
		}

		profile = db.ProfileToAPI(current)

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Validate the ETag.
	etag := []interface{}{profile.Config, profile.Description, profile.Devices}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	req := api.ProfilePut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	err = doProfileUpdate(d, projectName, name, profile, req)

	if err == nil && !isClusterNotification(r) {
		// Notify all other nodes. If a node is down, it will be ignored.
		notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), d.serverCert(), cluster.NotifyAlive)
		if err != nil {
			return response.SmartError(err)
		}

		err = notifier(func(client lxd.InstanceServer) error {
			return client.UseProject(projectName).UpdateProfile(name, profile.ProfilePut, "")
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectName, lifecycle.ProfileUpdated.Event(name, projectName, requestor, nil))

	return response.SmartError(err)
}

// swagger:operation PATCH /1.0/profiles/{name} profiles profile_patch
//
// Partially update the profile
//
// Updates a subset of the profile configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: body
//     name: profile
//     description: Profile configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ProfilePut"
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
func profilePatch(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.ProfileProject(d.State().Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	name := mux.Vars(r)["name"]

	var profile *api.Profile

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		current, err := tx.GetProfile(projectName, name)
		if err != nil {
			return errors.Wrapf(err, "Failed to retrieve profile=%q", name)
		}

		profile = db.ProfileToAPI(current)

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Validate the ETag.
	etag := []interface{}{profile.Config, profile.Description, profile.Devices}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return response.InternalError(err)
	}

	rdr1 := ioutil.NopCloser(bytes.NewBuffer(body))
	rdr2 := ioutil.NopCloser(bytes.NewBuffer(body))

	reqRaw := shared.Jmap{}
	if err := json.NewDecoder(rdr1).Decode(&reqRaw); err != nil {
		return response.BadRequest(err)
	}

	req := api.ProfilePut{}
	if err := json.NewDecoder(rdr2).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	// Get Description.
	_, err = reqRaw.GetString("description")
	if err != nil {
		req.Description = profile.Description
	}

	// Get Config.
	if req.Config == nil {
		req.Config = profile.Config
	} else {
		for k, v := range profile.Config {
			_, ok := req.Config[k]
			if !ok {
				req.Config[k] = v
			}
		}
	}

	// Get Devices.
	if req.Devices == nil {
		req.Devices = profile.Devices
	} else {
		for k, v := range profile.Devices {
			_, ok := req.Devices[k]
			if !ok {
				req.Devices[k] = v
			}
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectName, lifecycle.ProfileUpdated.Event(name, projectName, requestor, nil))

	return response.SmartError(doProfileUpdate(d, projectName, name, profile, req))
}

// swagger:operation POST /1.0/profiles/{name} profiles profile_post
//
// Rename the profile
//
// Renames an existing profile.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
//   - in: body
//     name: profile
//     description: Profile rename request
//     required: true
//     schema:
//       $ref: "#/definitions/ProfilePost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func profilePost(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.ProfileProject(d.State().Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	name := mux.Vars(r)["name"]
	if name == "default" {
		return response.Forbidden(errors.New(`The "default" profile cannot be renamed`))
	}

	req := api.ProfilePost{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("No name provided"))
	}

	if strings.Contains(req.Name, "/") {
		return response.BadRequest(fmt.Errorf("Profile names may not contain slashes"))
	}

	if shared.StringInSlice(req.Name, []string{".", ".."}) {
		return response.BadRequest(fmt.Errorf("Invalid profile name %q", req.Name))
	}

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		// Check that the name isn't already in use.
		_, err = tx.GetProfile(projectName, req.Name)
		if err == nil {
			return fmt.Errorf("Name %q already in use", req.Name)
		}

		return tx.RenameProfile(projectName, name, req.Name)
	})
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectName, lifecycle.ProfileRenamed.Event(req.Name, projectName, requestor, log.Ctx{"old_name": name}))

	return response.SyncResponseLocation(true, nil, fmt.Sprintf("/%s/profiles/%s", version.APIVersion, req.Name))
}

// swagger:operation DELETE /1.0/profiles/{name} profiles profile_delete
//
// Delete the profile
//
// Removes the profile.
//
// ---
// produces:
//   - application/json
// parameters:
//   - in: query
//     name: project
//     description: Project name
//     type: string
//     example: default
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func profileDelete(d *Daemon, r *http.Request) response.Response {
	projectName, _, err := project.ProfileProject(d.State().Cluster, projectParam(r))
	if err != nil {
		return response.SmartError(err)
	}

	name := mux.Vars(r)["name"]
	if name == "default" {
		return response.Forbidden(errors.New(`The "default" profile cannot be deleted`))
	}

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		profile, err := tx.GetProfile(projectName, name)
		if err != nil {
			return err
		}
		if len(profile.UsedBy) > 0 {
			return fmt.Errorf("Profile is currently in use")
		}

		return tx.DeleteProfile(projectName, name)
	})
	if err != nil {
		return response.SmartError(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(projectName, lifecycle.ProfileDeleted.Event(name, projectName, requestor, nil))

	return response.EmptySyncResponse
}
