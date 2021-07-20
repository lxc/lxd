package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/lifecycle"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/project"
	projecthelpers "github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/rbac"
	"github.com/lxc/lxd/lxd/request"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/validate"
	"github.com/lxc/lxd/shared/version"
)

// projectFeatures are the features available to projects.
var projectFeatures = []string{"features.images", "features.profiles", "features.storage.volumes", "features.networks"}

// projectFeaturesDefaults are the features enabled by default on new projects.
// The features.networks won't be enabled by default until it becomes clear whether it is practical to run OVN on
// every system.
var projectFeaturesDefaults = []string{"features.images", "features.profiles", "features.storage.volumes"}

var projectsCmd = APIEndpoint{
	Path: "projects",

	Get:  APIEndpointAction{Handler: projectsGet, AccessHandler: allowAuthenticated},
	Post: APIEndpointAction{Handler: projectsPost},
}

var projectCmd = APIEndpoint{
	Path: "projects/{name}",

	Delete: APIEndpointAction{Handler: projectDelete},
	Get:    APIEndpointAction{Handler: projectGet, AccessHandler: allowAuthenticated},
	Patch:  APIEndpointAction{Handler: projectPatch, AccessHandler: allowAuthenticated},
	Post:   APIEndpointAction{Handler: projectPost},
	Put:    APIEndpointAction{Handler: projectPut, AccessHandler: allowAuthenticated},
}

var projectStateCmd = APIEndpoint{
	Path: "projects/{name}/state",

	Get: APIEndpointAction{Handler: projectStateGet, AccessHandler: allowAuthenticated},
}

// swagger:operation GET /1.0/projects projects projects_get
//
// Get the projects
//
// Returns a list of projects (URLs).
//
// ---
// produces:
//   - application/json
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
//               "/1.0/projects/default",
//               "/1.0/projects/foo"
//             ]
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/projects?recursion=1 projects projects_get_recursion1
//
// Get the projects
//
// Returns a list of projects (structs).
//
// ---
// produces:
//   - application/json
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
//           description: List of projects
//           items:
//             $ref: "#/definitions/Project"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectsGet(d *Daemon, r *http.Request) response.Response {
	recursion := util.IsRecursionRequest(r)

	var result interface{}
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		filter := db.ProjectFilter{}
		if recursion {
			projects, err := tx.GetProjects(filter)
			if err != nil {
				return err
			}

			filtered := []db.Project{}
			for _, project := range projects {
				if !rbac.UserHasPermission(r, project.Name, "view") {
					continue
				}

				filtered = append(filtered, project)
			}

			result = filtered
		} else {
			uris, err := tx.GetProjectURIs(filter)
			if err != nil {
				return err
			}

			filtered := []string{}
			for _, uri := range uris {
				name := strings.Split(uri, "/1.0/projects/")[1]

				if !rbac.UserHasPermission(r, name, "view") {
					continue
				}

				filtered = append(filtered, uri)
			}

			result = filtered
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, result)
}

// swagger:operation POST /1.0/projects projects projects_post
//
// Add a project
//
// Creates a new project.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectsPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectsPost(d *Daemon, r *http.Request) response.Response {
	// Parse the request.
	project := db.Project{}

	// Set default features.
	if project.Config == nil {
		project.Config = map[string]string{}
	}
	for _, feature := range projectFeaturesDefaults {
		_, ok := project.Config[feature]
		if !ok {
			project.Config[feature] = "true"
		}
	}

	err := json.NewDecoder(r.Body).Decode(&project)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	err = projectValidateName(project.Name)
	if err != nil {
		return response.BadRequest(err)
	}

	// Validate the configuration.
	err = projectValidateConfig(d.State(), project.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	var id int64
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		id, err = tx.CreateProject(project)
		if err != nil {
			return errors.Wrap(err, "Failed adding database record")
		}

		if shared.IsTrue(project.Config["features.profiles"]) {
			err = projectCreateDefaultProfile(tx, project.Name)
			if err != nil {
				return err
			}

			if project.Config["features.images"] == "false" {
				err = tx.InitProjectWithoutImages(project.Name)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
	if err != nil {
		return response.SmartError(errors.Wrapf(err, "Failed creating project %q", project.Name))
	}

	if d.rbac != nil {
		err = d.rbac.AddProject(id, project.Name)
		if err != nil {
			return response.SmartError(err)
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Name, lifecycle.ProjectCreated.Event(project.Name, requestor, nil))

	return response.SyncResponseLocation(true, nil, fmt.Sprintf("/%s/projects/%s", version.APIVersion, project.Name))
}

// Create the default profile of a project.
func projectCreateDefaultProfile(tx *db.ClusterTx, project string) error {
	// Create a default profile
	profile := db.Profile{}
	profile.Project = project
	profile.Name = projecthelpers.Default
	profile.Description = fmt.Sprintf("Default LXD profile for project %s", project)

	_, err := tx.CreateProfile(profile)
	if err != nil {
		return errors.Wrap(err, "Add default profile to database")
	}
	return nil
}

// swagger:operation GET /1.0/projects/{name} projects project_get
//
// Get the project
//
// Gets a specific project.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: Project
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
//           $ref: "#/definitions/Project"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectGet(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Check user permissions
	if !rbac.UserHasPermission(r, name, "view") {
		return response.Forbidden(nil)
	}

	// Get the database entry
	project, err := d.cluster.GetProject(name)
	if err != nil {
		return response.SmartError(err)
	}

	etag := []interface{}{
		project.Description,
		project.Config,
	}

	return response.SyncResponseETag(true, project, etag)
}

// swagger:operation PUT /1.0/projects/{name} projects project_put
//
// Update the project
//
// Updates the entire project configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectPut"
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
func projectPut(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Check user permissions
	if !rbac.UserHasPermission(r, name, "manage-projects") {
		return response.Forbidden(nil)
	}

	// Get the current data
	project, err := d.cluster.GetProject(name)
	if err != nil {
		return response.SmartError(err)
	}

	// Validate ETag
	etag := []interface{}{
		project.Description,
		project.Config,
	}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Parse the request
	req := api.ProjectPut{}

	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Name, lifecycle.ProjectUpdated.Event(project.Name, requestor, nil))

	return projectChange(d, project, req)
}

// swagger:operation PATCH /1.0/projects/{name} projects project_patch
//
// Partially update the project
//
// Updates a subset of the project configuration.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project configuration
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectPut"
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
func projectPatch(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Check user permissions
	if !rbac.UserHasPermission(r, name, "manage-projects") {
		return response.Forbidden(nil)
	}

	// Get the current data
	project, err := d.cluster.GetProject(name)
	if err != nil {
		return response.SmartError(err)
	}

	// Validate ETag
	etag := []interface{}{
		project.Description,
		project.Config,
	}
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

	req := api.ProjectPut{}
	if err := json.NewDecoder(rdr2).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	// Check what was actually set in the query
	_, err = reqRaw.GetString("description")
	if err != nil {
		req.Description = project.Description
	}

	config, err := reqRaw.GetMap("config")
	if err != nil {
		req.Config = project.Config
	} else {
		for k, v := range project.Config {
			_, ok := config[k]
			if !ok {
				config[k] = v
			}
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(project.Name, lifecycle.ProjectUpdated.Event(project.Name, requestor, nil))

	return projectChange(d, project, req)
}

// Common logic between PUT and PATCH.
func projectChange(d *Daemon, project *db.Project, req api.ProjectPut) response.Response {
	// Make a list of config keys that have changed.
	configChanged := []string{}
	for key := range project.Config {
		if req.Config[key] != project.Config[key] {
			configChanged = append(configChanged, key)
		}
	}

	for key := range req.Config {
		_, ok := project.Config[key]
		if !ok {
			configChanged = append(configChanged, key)
		}
	}

	// Flag indicating if any feature has changed.
	featuresChanged := false
	for _, featureKey := range projectFeatures {
		if shared.StringInSlice(featureKey, configChanged) {
			featuresChanged = true
			break
		}
	}

	// Quick checks.
	if project.Name == projecthelpers.Default && featuresChanged {
		return response.BadRequest(fmt.Errorf("You can't change the features of the default project"))
	}

	if !projectIsEmpty(project) && featuresChanged {
		return response.BadRequest(fmt.Errorf("Features can only be changed on empty projects"))
	}

	// Validate the configuration.
	err := projectValidateConfig(d.State(), req.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	// Update the database entry.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		err := projecthelpers.AllowProjectUpdate(tx, project.Name, req.Config, configChanged)
		if err != nil {
			return err
		}

		err = tx.UpdateProject(project.Name, req)
		if err != nil {
			return errors.Wrap(err, "Persist profile changes")
		}

		if shared.StringInSlice("features.profiles", configChanged) {
			if shared.IsTrue(req.Config["features.profiles"]) {
				err = projectCreateDefaultProfile(tx, project.Name)
				if err != nil {
					return err
				}
			} else {
				// Delete the project-specific default profile.
				filter := db.ProfileFilter{Project: project.Name, Name: projecthelpers.Default}
				err = tx.DeleteProfile(filter)
				if err != nil {
					return errors.Wrap(err, "Delete project default profile")
				}
			}
		}

		return nil
	})

	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

// swagger:operation POST /1.0/projects/{name} projects project_post
//
// Rename the project
//
// Renames an existing project.
//
// ---
// consumes:
//   - application/json
// produces:
//   - application/json
// parameters:
//   - in: body
//     name: project
//     description: Project rename request
//     required: true
//     schema:
//       $ref: "#/definitions/ProjectPost"
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectPost(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Parse the request.
	req := api.ProjectPost{}

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Quick checks.
	if name == projecthelpers.Default {
		return response.Forbidden(fmt.Errorf("The 'default' project cannot be renamed"))
	}

	// Perform the rename.
	run := func(op *operations.Operation) error {
		var id int64
		err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
			project, err := tx.GetProject(req.Name)
			if err != nil && err != db.ErrNoSuchObject {
				return errors.Wrapf(err, "Failed checking if project %q exists", req.Name)
			}

			if project != nil {
				return fmt.Errorf("A project named %q already exists", req.Name)
			}

			project, err = tx.GetProject(name)
			if err != nil {
				return errors.Wrapf(err, "Failed loading project %q", name)
			}

			if !projectIsEmpty(project) {
				return fmt.Errorf("Only empty projects can be renamed")
			}

			id, err = tx.GetProjectID(name)
			if err != nil {
				return errors.Wrapf(err, "Failed getting project ID for project %q", name)
			}

			err = projectValidateName(req.Name)
			if err != nil {
				return err
			}

			return tx.RenameProject(name, req.Name)
		})
		if err != nil {
			return err
		}

		if d.rbac != nil {
			err = d.rbac.RenameProject(id, req.Name)
			if err != nil {
				return err
			}
		}

		requestor := request.CreateRequestor(r)
		d.State().Events.SendLifecycle(req.Name, lifecycle.ProjectRenamed.Event(req.Name, requestor, log.Ctx{"old_name": name}))

		return nil
	}

	op, err := operations.OperationCreate(d.State(), "", operations.OperationClassTask, db.OperationProjectRename, nil, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

// swagger:operation DELETE /1.0/projects/{name} projects project_delete
//
// Delete the project
//
// Removes the project.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     $ref: "#/responses/EmptySyncResponse"
//   "400":
//     $ref: "#/responses/BadRequest"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectDelete(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Quick checks.
	if name == projecthelpers.Default {
		return response.Forbidden(fmt.Errorf("The 'default' project cannot be deleted"))
	}

	var id int64
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		project, err := tx.GetProject(name)
		if err != nil {
			return errors.Wrapf(err, "Fetch project %q", name)
		}
		if !projectIsEmpty(project) {
			return fmt.Errorf("Only empty projects can be removed")
		}

		id, err = tx.GetProjectID(name)
		if err != nil {
			return errors.Wrapf(err, "Fetch project id %q", name)
		}

		return tx.DeleteProject(db.ProjectFilter{Name: name})
	})

	if err != nil {
		return response.SmartError(err)
	}

	if d.rbac != nil {
		err = d.rbac.DeleteProject(id)
		if err != nil {
			return response.SmartError(err)
		}
	}

	requestor := request.CreateRequestor(r)
	d.State().Events.SendLifecycle(name, lifecycle.ProjectDeleted.Event(name, requestor, nil))

	return response.EmptySyncResponse
}

// swagger:operation GET /1.0/projects/{name}/state projects project_state_get
//
// Get the project state
//
// Gets a specific project resource consumption information.
//
// ---
// produces:
//   - application/json
// responses:
//   "200":
//     description: Project state
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
//           $ref: "#/definitions/ProjectState"
//   "403":
//     $ref: "#/responses/Forbidden"
//   "500":
//     $ref: "#/responses/InternalServerError"
func projectStateGet(d *Daemon, r *http.Request) response.Response {
	name := mux.Vars(r)["name"]

	// Check user permissions.
	if !rbac.UserHasPermission(r, name, "view") {
		return response.Forbidden(nil)
	}

	// Setup the state struct.
	state := api.ProjectState{}

	// Get current limits and usage.
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		result, err := projecthelpers.GetCurrentAllocations(tx, name)
		if err != nil {
			return err
		}
		state.Resources = result

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, &state)
}

// Check if a project is empty.
func projectIsEmpty(project *db.Project) bool {
	if len(project.UsedBy) > 0 {
		// Check if the only entity is the default profile.
		if len(project.UsedBy) == 1 && strings.Contains(project.UsedBy[0], "/profiles/default") {
			return true
		}
		return false
	}
	return true
}

func isEitherAllowOrBlock(value string) error {
	return validate.Optional(func(value string) error {
		return validate.IsOneOf(value, []string{"block", "allow"})
	})(value)
}

func isEitherAllowOrBlockOrManaged(value string) error {
	return validate.Optional(func(value string) error {
		return validate.IsOneOf(value, []string{"block", "allow", "managed"})
	})(value)
}

func projectValidateConfig(s *state.State, config map[string]string) error {
	// Validate the project configuration.
	projectConfigKeys := map[string]func(value string) error{
		"backups.compression_algorithm":  validate.IsCompressionAlgorithm,
		"features.profiles":              validate.Optional(validate.IsBool),
		"features.images":                validate.Optional(validate.IsBool),
		"features.storage.volumes":       validate.Optional(validate.IsBool),
		"features.networks":              validate.Optional(validate.IsBool),
		"images.auto_update_cached":      validate.Optional(validate.IsBool),
		"images.auto_update_interval":    validate.Optional(validate.IsInt64),
		"images.compression_algorithm":   validate.IsCompressionAlgorithm,
		"images.default_architecture":    validate.Optional(validate.IsArchitecture),
		"images.remote_cache_expiry":     validate.Optional(validate.IsInt64),
		"limits.instances":               validate.Optional(validate.IsUint32),
		"limits.containers":              validate.Optional(validate.IsUint32),
		"limits.virtual-machines":        validate.Optional(validate.IsUint32),
		"limits.memory":                  validate.Optional(validate.IsSize),
		"limits.processes":               validate.Optional(validate.IsUint32),
		"limits.cpu":                     validate.Optional(validate.IsUint32),
		"limits.disk":                    validate.Optional(validate.IsSize),
		"limits.networks":                validate.Optional(validate.IsUint32),
		"restricted":                     validate.Optional(validate.IsBool),
		"restricted.backups":             isEitherAllowOrBlock,
		"restricted.cluster.target":      isEitherAllowOrBlock,
		"restricted.containers.nesting":  isEitherAllowOrBlock,
		"restricted.containers.lowlevel": isEitherAllowOrBlock,
		"restricted.containers.privilege": validate.Optional(func(value string) error {
			return validate.IsOneOf(value, []string{"allow", "unprivileged", "isolated"})
		}),
		"restricted.virtual-machines.lowlevel": isEitherAllowOrBlock,
		"restricted.devices.unix-char":         isEitherAllowOrBlock,
		"restricted.devices.unix-block":        isEitherAllowOrBlock,
		"restricted.devices.unix-hotplug":      isEitherAllowOrBlock,
		"restricted.devices.infiniband":        isEitherAllowOrBlock,
		"restricted.devices.gpu":               isEitherAllowOrBlock,
		"restricted.devices.usb":               isEitherAllowOrBlock,
		"restricted.devices.nic":               isEitherAllowOrBlockOrManaged,
		"restricted.devices.disk":              isEitherAllowOrBlockOrManaged,
		"restricted.networks.uplinks":          validate.IsAny,
		"restricted.networks.subnets": validate.Optional(func(value string) error {
			return projectValidateRestrictedSubnets(s, value)
		}),
		"restricted.snapshots": isEitherAllowOrBlock,
	}

	for k, v := range config {
		key := k

		// User keys are free for all.
		if strings.HasPrefix(key, "user.") {
			continue
		}

		// Then validate.
		validator, ok := projectConfigKeys[key]
		if !ok {
			return fmt.Errorf("Invalid project configuration key %q", k)
		}

		err := validator(v)
		if err != nil {
			return errors.Wrapf(err, "Invalid project configuration key %q value", k)
		}
	}

	return nil
}

func projectValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("No name provided")
	}

	if strings.Contains(name, "/") {
		return fmt.Errorf("Project names may not contain slashes")
	}

	if strings.Contains(name, " ") {
		return fmt.Errorf("Project names may not contain spaces")
	}

	if strings.Contains(name, "_") {
		return fmt.Errorf("Project names may not contain underscores")
	}

	if strings.Contains(name, "'") || strings.Contains(name, `"`) {
		return fmt.Errorf("Project names may not contain quotes")
	}

	if name == "*" {
		return fmt.Errorf("Reserved project name")
	}

	if shared.StringInSlice(name, []string{".", ".."}) {
		return fmt.Errorf("Invalid project name %q", name)
	}

	return nil
}

// projectValidateRestrictedSubnets checks that the project's restricted.networks.subnets are properly formatted
// and are within the specified uplink network's routes.
func projectValidateRestrictedSubnets(s *state.State, value string) error {
	for _, subnetRaw := range util.SplitNTrimSpace(value, ",", -1, false) {
		subnetParts := strings.SplitN(subnetRaw, ":", 2)
		if len(subnetParts) != 2 {
			return fmt.Errorf(`Subnet %q invalid, must be in the format of "<uplink network>:<subnet>"`, subnetRaw)
		}

		uplinkName := subnetParts[0]
		subnetStr := subnetParts[1]

		restrictedSubnetIP, restrictedSubnet, err := net.ParseCIDR(subnetStr)
		if err != nil {
			return err
		}

		if restrictedSubnetIP.String() != restrictedSubnet.IP.String() {
			return fmt.Errorf("Not an IP network address %q", subnetStr)
		}

		// Check uplink exists and load config to compare subnets.
		_, uplink, _, err := s.Cluster.GetNetworkInAnyState(project.Default, uplinkName)
		if err != nil {
			return errors.Wrapf(err, "Invalid uplink network %q", uplinkName)
		}

		// Parse uplink route subnets.
		var uplinkRoutes []*net.IPNet
		for _, k := range []string{"ipv4.routes", "ipv6.routes"} {
			if uplink.Config[k] == "" {
				continue
			}

			uplinkRoutes, err = network.SubnetParseAppend(uplinkRoutes, util.SplitNTrimSpace(uplink.Config[k], ",", -1, false)...)
			if err != nil {
				return err
			}
		}

		foundMatch := false
		// Check that the restricted subnet is within one of the uplink's routes.
		for _, uplinkRoute := range uplinkRoutes {
			if network.SubnetContains(uplinkRoute, restrictedSubnet) {
				foundMatch = true
				break
			}
		}

		if !foundMatch {
			return fmt.Errorf("Uplink network %q doesn't contain %q in its routes", uplinkName, restrictedSubnet.String())
		}
	}

	return nil
}
