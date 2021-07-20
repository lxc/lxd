package project

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	deviceconfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/rbac"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/units"
)

// AllowInstanceCreation returns an error if any project-specific limit or
// restriction is violated when creating a new instance.
func AllowInstanceCreation(tx *db.ClusterTx, projectName string, req api.InstancesPost) error {
	info, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}

	if info == nil {
		return nil
	}

	var instanceType instancetype.Type
	switch req.Type {
	case api.InstanceTypeContainer:
		instanceType = instancetype.Container
	case api.InstanceTypeVM:
		instanceType = instancetype.VM
	default:
		return fmt.Errorf("Unexpected instance type %q", instanceType)
	}

	if req.Profiles == nil {
		req.Profiles = []string{"default"}
	}

	err = checkInstanceCountLimit(info, instanceType)
	if err != nil {
		return err
	}

	err = checkTotalInstanceCountLimit(info)
	if err != nil {
		return err
	}

	// Add the instance being created.
	info.Instances = append(info.Instances, db.Instance{
		Name:     req.Name,
		Profiles: req.Profiles,
		Config:   req.Config,
		Project:  projectName,
	})

	// Special case restriction checks on volatile.* keys.
	strip := false

	if shared.StringInSlice(req.Source.Type, []string{"copy", "migration"}) {
		// Allow stripping volatile keys if dealing with a copy or migration.
		strip = true
	}

	err = checkRestrictionsOnVolatileConfig(
		info.Project, instanceType, req.Name, req.Config, map[string]string{}, strip)
	if err != nil {
		return err
	}

	err = checkRestrictionsAndAggregateLimits(tx, info)
	if err != nil {
		return err
	}

	return nil
}

// Check that we have not exceeded the maximum total allotted number of instances for both containers and vms.
func checkTotalInstanceCountLimit(info *projectInfo) error {
	count, limit, err := getTotalInstanceCountLimit(info)
	if err != nil {
		return err
	}

	if limit >= 0 && count >= limit {
		return fmt.Errorf("Reached maximum number of instances in project %q", info.Project.Name)
	}

	return nil
}

func getTotalInstanceCountLimit(info *projectInfo) (int, int, error) {
	overallValue, ok := info.Project.Config["limits.instances"]
	if ok {
		limit, err := strconv.Atoi(overallValue)
		if err != nil {
			return -1, -1, err
		}

		return len(info.Instances), limit, nil
	}

	return len(info.Instances), -1, nil
}

// Check that we have not reached the maximum number of instances for this type.
func checkInstanceCountLimit(info *projectInfo, instanceType instancetype.Type) error {
	count, limit, err := getInstanceCountLimit(info, instanceType)
	if err != nil {
		return err
	}

	if limit >= 0 && count >= limit {
		return fmt.Errorf("Reached maximum number of instances of type %q in project %q", instanceType, info.Project.Name)
	}

	return nil
}

func getInstanceCountLimit(info *projectInfo, instanceType instancetype.Type) (int, int, error) {
	var key string
	switch instanceType {
	case instancetype.Container:
		key = "limits.containers"
	case instancetype.VM:
		key = "limits.virtual-machines"
	default:
		return -1, -1, fmt.Errorf("Unexpected instance type %q", instanceType)
	}

	instanceCount := 0
	for _, inst := range info.Instances {
		if inst.Type == instanceType {
			instanceCount++
		}
	}

	value, ok := info.Project.Config[key]
	if ok {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 0 {
			return -1, -1, fmt.Errorf("Unexpected %q value: %q", key, value)
		}

		return instanceCount, limit, nil
	}

	return instanceCount, -1, nil
}

// Check restrictions on setting volatile.* keys.
func checkRestrictionsOnVolatileConfig(project *db.Project, instanceType instancetype.Type, instanceName string, config, currentConfig map[string]string, strip bool) error {
	if project.Config["restrict"] == "false" {
		return nil
	}

	var restrictedLowLevel string
	switch instanceType {
	case instancetype.Container:
		restrictedLowLevel = "restricted.containers.lowlevel"
	case instancetype.VM:
		restrictedLowLevel = "restricted.virtual-machines.lowlevel"
	}

	if project.Config[restrictedLowLevel] == "allow" {
		return nil
	}

	// Checker for safe volatile keys.
	isSafeKey := func(key string) bool {
		if shared.StringInSlice(key, []string{"volatile.apply_template", "volatile.base_image", "volatile.last_state.power"}) {
			return true
		}

		if strings.HasPrefix(key, shared.ConfigVolatilePrefix) {
			if strings.HasSuffix(key, ".apply_quota") {
				return true
			}

			if strings.HasSuffix(key, ".hwaddr") {
				return true
			}
		}

		return false
	}

	for key, value := range config {
		if !strings.HasPrefix(key, shared.ConfigVolatilePrefix) {
			continue
		}

		// Allow given safe volatile keys to be set
		if isSafeKey(key) {
			continue
		}

		if strip {
			delete(config, key)
			continue
		}

		currentValue, ok := currentConfig[key]
		if !ok {
			return fmt.Errorf(
				"Setting %q on %s %q in project %q is forbidden",
				key, instanceType, instanceName, project.Name)
		}

		if currentValue != value {
			return fmt.Errorf(
				"Changing %q on %s %q in project %q is forbidden",
				key, instanceType, instanceName, project.Name)
		}
	}

	return nil
}

// AllowVolumeCreation returns an error if any project-specific limit or
// restriction is violated when creating a new custom volume in a project.
func AllowVolumeCreation(tx *db.ClusterTx, projectName string, req api.StorageVolumesPost) error {
	info, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}

	if info == nil {
		return nil
	}

	// If "limits.disk" is not set, there's nothing to do.
	if info.Project.Config["limits.disk"] == "" {
		return nil
	}

	// Add the volume being created.
	info.Volumes = append(info.Volumes, db.StorageVolumeArgs{
		Name:   req.Name,
		Config: req.Config,
	})

	err = checkRestrictionsAndAggregateLimits(tx, info)
	if err != nil {
		return err
	}

	return nil
}

// GetImageSpaceBudget returns how much disk space is left in the given project
// for writing images.
//
// If no limit is in place, return -1.
func GetImageSpaceBudget(tx *db.ClusterTx, projectName string) (int64, error) {
	info, err := fetchProject(tx, projectName, true)
	if err != nil {
		return -1, err
	}

	if info == nil {
		return -1, nil
	}

	// If "features.images" is not enabled, the budget is unlimited.
	if !shared.IsTrue(info.Project.Config["features.images"]) {
		return -1, nil
	}

	// If "limits.disk" is not set, the budget is unlimited.
	if info.Project.Config["limits.disk"] == "" {
		return -1, nil
	}

	parser := aggregateLimitConfigValueParsers["limits.disk"]
	quota, err := parser(info.Project.Config["limits.disk"])
	if err != nil {
		return -1, err
	}

	info.Instances = expandInstancesConfigAndDevices(info.Instances, info.Profiles)

	totals, err := getTotalsAcrossProjectEntities(info, []string{"limits.disk"}, false)
	if err != nil {
		return -1, err
	}

	if totals["limits.disk"] < quota {
		return quota - totals["limits.disk"], nil
	}

	return 0, nil
}

// Check that we would not violate the project limits or restrictions if we
// were to commit the given instances and profiles.
func checkRestrictionsAndAggregateLimits(tx *db.ClusterTx, info *projectInfo) error {
	// List of config keys for which we need to check aggregate values
	// across all project instances.
	aggregateKeys := []string{}
	isRestricted := false
	for key, value := range info.Project.Config {
		if shared.StringInSlice(key, allAggregateLimits) {
			aggregateKeys = append(aggregateKeys, key)
			continue
		}

		if key == "restricted" && shared.IsTrue(value) {
			isRestricted = true
			continue
		}
	}

	if len(aggregateKeys) == 0 && !isRestricted {
		return nil
	}

	info.Instances = expandInstancesConfigAndDevices(info.Instances, info.Profiles)

	err := checkAggregateLimits(info, aggregateKeys)
	if err != nil {
		return err
	}

	if isRestricted {
		err = checkRestrictions(info.Project, info.Instances, info.Profiles)
		if err != nil {
			return err
		}
	}

	return nil
}

func getAggregateLimits(info *projectInfo, aggregateKeys []string) (map[string]api.ProjectStateResource, error) {
	result := map[string]api.ProjectStateResource{}

	if len(aggregateKeys) == 0 {
		return result, nil
	}

	totals, err := getTotalsAcrossProjectEntities(info, aggregateKeys, true)
	if err != nil {
		return nil, err
	}

	for _, key := range aggregateKeys {
		max := int64(-1)
		limit := info.Project.Config[key]
		if limit != "" {
			parser := aggregateLimitConfigValueParsers[key]
			max, err = parser(info.Project.Config[key])
			if err != nil {
				return nil, err
			}
		}

		resource := api.ProjectStateResource{
			Usage: totals[key],
			Limit: max,
		}
		result[key] = resource
	}

	return result, nil
}

func checkAggregateLimits(info *projectInfo, aggregateKeys []string) error {
	if len(aggregateKeys) == 0 {
		return nil
	}

	totals, err := getTotalsAcrossProjectEntities(info, aggregateKeys, false)
	if err != nil {
		return err
	}

	for _, key := range aggregateKeys {
		parser := aggregateLimitConfigValueParsers[key]
		max, err := parser(info.Project.Config[key])
		if err != nil {
			return err
		}

		if totals[key] > max {
			return fmt.Errorf(
				"Reached maximum aggregate value %s for %q in project %s",
				info.Project.Config[key], key, info.Project.Name)
		}
	}
	return nil
}

// Check that the project's restrictions are not violated across the given
// instances and profiles.
func checkRestrictions(project *db.Project, instances []db.Instance, profiles []db.Profile) error {
	containerConfigChecks := map[string]func(value string) error{}
	devicesChecks := map[string]func(value map[string]string) error{}

	allowContainerLowLevel := false
	allowVMLowLevel := false

	for _, key := range AllRestrictions {
		// Check if this particular restriction is defined explicitly
		// in the project config. If not, use the default value.
		restrictionValue, ok := project.Config[key]
		if !ok {
			restrictionValue = defaultRestrictionsValues[key]
		}

		switch key {
		case "restricted.containers.nesting":
			containerConfigChecks["security.nesting"] = func(instanceValue string) error {
				if restrictionValue == "block" && shared.IsTrue(instanceValue) {
					return fmt.Errorf("Container nesting is forbidden")
				}

				return nil
			}
		case "restricted.containers.lowlevel":
			if restrictionValue == "allow" {
				allowContainerLowLevel = true
			}
		case "restricted.containers.privilege":
			containerConfigChecks["security.privileged"] = func(instanceValue string) error {
				if restrictionValue != "allow" && shared.IsTrue(instanceValue) {
					return fmt.Errorf("Privileged containers are forbidden")
				}

				return nil
			}
			containerConfigChecks["security.idmap.isolated"] = func(instanceValue string) error {
				if restrictionValue == "isolated" && !shared.IsTrue(instanceValue) {
					return fmt.Errorf("Non-isolated containers are forbidden")
				}

				return nil
			}
		case "restricted.virtual-machines.lowlevel":
			if restrictionValue == "allow" {
				allowVMLowLevel = true
			}
		case "restricted.devices.unix-char":
			devicesChecks["unix-char"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Unix character devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.unix-block":
			devicesChecks["unix-block"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Unix block devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.unix-hotplug":
			devicesChecks["unix-hotplug"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Unix hotplug devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.infiniband":
			devicesChecks["infiniband"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("Infiniband devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.gpu":
			devicesChecks["gpu"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("GPU devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.usb":
			devicesChecks["usb"] = func(device map[string]string) error {
				if restrictionValue != "allow" {
					return fmt.Errorf("USB devices are forbidden")
				}

				return nil
			}
		case "restricted.devices.nic":
			devicesChecks["nic"] = func(device map[string]string) error {
				switch restrictionValue {
				case "block":
					return fmt.Errorf("Network devices are forbidden")
				case "managed":
					if device["network"] == "" {
						return fmt.Errorf("Only managed network devices are allowed")
					}
				}
				return nil
			}
		case "restricted.devices.disk":
			devicesChecks["disk"] = func(device map[string]string) error {
				// The root device is always allowed.
				if device["path"] == "/" && device["pool"] != "" {
					return nil
				}

				// Always allow the cloud-init config drive.
				if device["path"] == "" && device["source"] == "cloud-init:config" {
					return nil
				}

				switch restrictionValue {
				case "block":
					return fmt.Errorf("Disk devices are forbidden")
				case "managed":
					if device["pool"] == "" {
						return fmt.Errorf("Attaching disks not backed by a pool is forbidden")
					}
				}
				return nil
			}
		}
	}

	// Common config check logic between instances and profiles.
	entityConfigChecker := func(entityType, entityName string, config map[string]string) error {
		isContainerOrProfile := shared.StringInSlice(entityType, []string{"container", "profile"})
		isVMOrProfile := shared.StringInSlice(entityType, []string{"virtual machine", "profile"})
		for key, value := range config {
			// First check if the key is a forbidden low-level one.
			if isContainerOrProfile && !allowContainerLowLevel && isContainerLowLevelOptionForbidden(key) {
				return fmt.Errorf("Use of low-level config %q on %s %q of project %q is forbidden",
					key, entityType, entityName, project.Name)
			}

			if isVMOrProfile && !allowVMLowLevel && isVMLowLevelOptionForbidden(key) {
				return fmt.Errorf("Use of low-level config %q on %s %q of project %q is forbidden",
					key, entityType, entityName, project.Name)
			}

			var checker func(value string) error
			if isContainerOrProfile {
				checker = containerConfigChecks[key]
			}

			if checker == nil {
				continue
			}
			err := checker(value)
			if err != nil {
				return errors.Wrapf(
					err,
					"Invalid value %q for config %q on %s %q of project %q",
					value, key, entityType, entityName, project.Name)
			}
		}
		return nil
	}

	// Common devices check logic between instances and profiles.
	entityDevicesChecker := func(entityType, entityName string, devices map[string]map[string]string) error {
		for name, device := range devices {
			check, ok := devicesChecks[device["type"]]
			if !ok {
				continue
			}

			err := check(device)
			if err != nil {
				return errors.Wrapf(
					err,
					"Invalid device %q on %s %q of project %q",
					name, entityType, entityName, project.Name)
			}
		}
		return nil
	}

	for _, instance := range instances {
		var err error
		switch instance.Type {
		case instancetype.Container:
			err = entityConfigChecker("container", instance.Name, instance.Config)
		case instancetype.VM:
			err = entityConfigChecker("virtual machine", instance.Name, instance.Config)
		}
		if err != nil {
			return err
		}

		err = entityDevicesChecker("instance", instance.Name, instance.Devices)
		if err != nil {
			return err
		}
	}

	for _, profile := range profiles {
		err := entityConfigChecker("profile", profile.Name, profile.Config)
		if err != nil {
			return err
		}

		err = entityDevicesChecker("profile", profile.Name, profile.Devices)
		if err != nil {
			return err
		}
	}

	return nil
}

var allAggregateLimits = []string{
	"limits.cpu",
	"limits.disk",
	"limits.memory",
	"limits.processes",
}

// AllRestrictions lists all available 'restrict.*' config keys.
var AllRestrictions = []string{
	"restricted.backups",
	"restricted.cluster.target",
	"restricted.containers.nesting",
	"restricted.containers.lowlevel",
	"restricted.containers.privilege",
	"restricted.virtual-machines.lowlevel",
	"restricted.devices.unix-char",
	"restricted.devices.unix-block",
	"restricted.devices.unix-hotplug",
	"restricted.devices.infiniband",
	"restricted.devices.gpu",
	"restricted.devices.usb",
	"restricted.devices.nic",
	"restricted.devices.disk",
	"restricted.snapshots",
}

var defaultRestrictionsValues = map[string]string{
	"restricted.backups":                   "block",
	"restricted.cluster.target":            "block",
	"restricted.containers.nesting":        "block",
	"restricted.containers.lowlevel":       "block",
	"restricted.containers.privilege":      "unprivileged",
	"restricted.virtual-machines.lowlevel": "block",
	"restricted.devices.unix-char":         "block",
	"restricted.devices.unix-block":        "block",
	"restricted.devices.unix-hotplug":      "block",
	"restricted.devices.infiniband":        "block",
	"restricted.devices.gpu":               "block",
	"restricted.devices.usb":               "block",
	"restricted.devices.nic":               "managed",
	"restricted.devices.disk":              "managed",
	"restricted.snapshots":                 "block",
}

// Return true if a low-level container option is forbidden.
func isContainerLowLevelOptionForbidden(key string) bool {
	if strings.HasPrefix(key, "security.syscalls.intercept") {
		return true
	}

	if shared.StringInSlice(key, []string{
		"boot.host_shutdown_timeout",
		"linux.kernel_modules",
		"raw.apparmor",
		"raw.idmap",
		"raw.lxc",
		"raw.seccomp",
		"security.devlxd.images",
		"security.idmap.base",
		"security.idmap.size",
	}) {
		return true
	}

	return false
}

// Return true if a low-level VM option is forbidden.
func isVMLowLevelOptionForbidden(key string) bool {
	if shared.StringInSlice(key, []string{
		"boot.host_shutdown_timeout",
		"limits.memory.hugepages",
		"raw.qemu",
	}) {
		return true
	}
	return false
}

// AllowInstanceUpdate returns an error if any project-specific limit or
// restriction is violated when updating an existing instance.
func AllowInstanceUpdate(tx *db.ClusterTx, projectName, instanceName string, req api.InstancePut, currentConfig map[string]string) error {
	var updatedInstance *db.Instance
	info, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}

	if info == nil {
		return nil
	}

	// Change the instance being updated.
	for i, instance := range info.Instances {
		if instance.Name != instanceName {
			continue
		}
		info.Instances[i].Profiles = req.Profiles
		info.Instances[i].Config = req.Config
		info.Instances[i].Devices = req.Devices
		updatedInstance = &info.Instances[i]
	}

	// Special case restriction checks on volatile.* keys, since we want to
	// detect if they were changed or added.
	err = checkRestrictionsOnVolatileConfig(
		info.Project, updatedInstance.Type, updatedInstance.Name, req.Config, currentConfig, false)
	if err != nil {
		return err
	}

	err = checkRestrictionsAndAggregateLimits(tx, info)
	if err != nil {
		return err
	}

	return nil
}

// AllowVolumeUpdate returns an error if any project-specific limit or
// restriction is violated when updating an existing custom volume.
func AllowVolumeUpdate(tx *db.ClusterTx, projectName, volumeName string, req api.StorageVolumePut, currentConfig map[string]string) error {
	info, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}

	if info == nil {
		return nil
	}

	// If "limits.disk" is not set, there's nothing to do.
	if info.Project.Config["limits.disk"] == "" {
		return nil
	}

	// Change the volume being updated.
	for i, volume := range info.Volumes {
		if volume.Name != volumeName {
			continue
		}
		info.Volumes[i].Config = req.Config
	}

	err = checkRestrictionsAndAggregateLimits(tx, info)
	if err != nil {
		return err
	}

	return nil
}

// AllowProfileUpdate checks that project limits and restrictions are not
// violated when changing a profile.
func AllowProfileUpdate(tx *db.ClusterTx, projectName, profileName string, req api.ProfilePut) error {
	info, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}
	if info == nil {
		return nil
	}

	// Change the profile being updated.
	for i, profile := range info.Profiles {
		if profile.Name != profileName {
			continue
		}
		info.Profiles[i].Config = req.Config
		info.Profiles[i].Devices = req.Devices
	}

	err = checkRestrictionsAndAggregateLimits(tx, info)
	if err != nil {
		return err
	}

	return nil
}

// AllowProjectUpdate checks the new config to be set on a project is valid.
func AllowProjectUpdate(tx *db.ClusterTx, projectName string, config map[string]string, changed []string) error {
	info, err := fetchProject(tx, projectName, false)
	if err != nil {
		return err
	}

	info.Instances = expandInstancesConfigAndDevices(info.Instances, info.Profiles)

	// List of keys that need to check aggregate values across all project
	// instances.
	aggregateKeys := []string{}

	for _, key := range changed {
		if strings.HasPrefix(key, "restricted.") {
			project := &db.Project{
				Name:   projectName,
				Config: config,
			}

			err := checkRestrictions(project, info.Instances, info.Profiles)
			if err != nil {
				return errors.Wrapf(err, "Conflict detected when changing %q in project %q", key, projectName)
			}

			continue
		}

		switch key {
		case "limits.instances":
			err := validateTotalInstanceCountLimit(info.Instances, config[key], projectName)
			if err != nil {
				return errors.Wrapf(err, "Can't change limits.instances in project %q", projectName)
			}
		case "limits.containers":
			fallthrough
		case "limits.virtual-machines":
			err := validateInstanceCountLimit(info.Instances, key, config[key], projectName)
			if err != nil {
				return errors.Wrapf(err, "Can't change %q in project %q", key, projectName)
			}
		case "limits.processes":
			fallthrough
		case "limits.cpu":
			fallthrough
		case "limits.memory":
			fallthrough
		case "limits.disk":
			aggregateKeys = append(aggregateKeys, key)

		}
	}

	if len(aggregateKeys) > 0 {
		totals, err := getTotalsAcrossProjectEntities(info, aggregateKeys, false)
		if err != nil {
			return err
		}
		for _, key := range aggregateKeys {
			err := validateAggregateLimit(totals, key, config[key])
			if err != nil {
				return err
			}
		}

	}

	return nil
}

// Check that limits.instances, i.e. the total limit of containers/virtual machines allocated
// to the user is equal to or above the current count
func validateTotalInstanceCountLimit(instances []db.Instance, value, project string) error {
	if value == "" {
		return nil
	}

	limit, err := strconv.Atoi(value)
	if err != nil {
		return err
	}

	count := len(instances)

	if limit < count {
		return fmt.Errorf("'limits.instances' is too low: there currently are %d total instances in project %s", count, project)
	}
	return nil
}

// Check that limits.containers or limits.virtual-machines is equal or above
// the current count.
func validateInstanceCountLimit(instances []db.Instance, key, value, project string) error {
	if value == "" {
		return nil
	}

	instanceType := countConfigInstanceType[key]
	limit, err := strconv.Atoi(value)
	if err != nil {
		return err
	}

	dbType, err := instancetype.New(string(instanceType))
	if err != nil {
		return err
	}

	count := 0
	for _, instance := range instances {
		if instance.Type == dbType {
			count++
		}
	}

	if limit < count {
		return fmt.Errorf(
			"'%s' is too low: there currently are %d instances of type %s in project %s",
			key, count, instanceType, project)
	}

	return nil
}

var countConfigInstanceType = map[string]api.InstanceType{
	"limits.containers":       api.InstanceTypeContainer,
	"limits.virtual-machines": api.InstanceTypeVM,
}

// Validates an aggregate limit, checking that the new value is not below the
// current total amount.
func validateAggregateLimit(totals map[string]int64, key, value string) error {
	if value == "" {
		return nil
	}

	parser := aggregateLimitConfigValueParsers[key]
	limit, err := parser(value)
	if err != nil {
		errors.Wrapf(err, "Invalid value '%s' for limit %s", value, key)
	}

	total := totals[key]
	if limit < total {
		printer := aggregateLimitConfigValuePrinters[key]
		return fmt.Errorf("'%s' is too low: current total is %s", key, printer(total))
	}

	return nil
}

// Return true if the project has some limits or restrictions set.
func projectHasLimitsOrRestrictions(project *db.Project) bool {
	for k, v := range project.Config {
		if strings.HasPrefix(k, "limits.") {
			return true
		}

		if k == "restricted" && shared.IsTrue(v) {
			return true
		}

	}
	return false
}

// Hold information associated with the project, such as profiles and
// instances.
type projectInfo struct {
	Project   *db.Project
	Profiles  []db.Profile
	Instances []db.Instance
	Volumes   []db.StorageVolumeArgs
}

// Fetch the given project from the database along with its profiles, instances
// and possibly custom volumes.
//
// If the skipIfNoLimits flag is true, then profiles, instances and volumes
// won't be loaded if the profile has no limits set on it, and nil will be
// returned.
func fetchProject(tx *db.ClusterTx, projectName string, skipIfNoLimits bool) (*projectInfo, error) {
	project, err := tx.GetProject(projectName)
	if err != nil {
		return nil, errors.Wrap(err, "Fetch project database object")
	}

	if skipIfNoLimits && !projectHasLimitsOrRestrictions(project) {
		return nil, nil
	}

	profilesFilter := db.ProfileFilter{}

	// If the project has the profiles feature enabled, we use its own
	// profiles to expand the instances configs, otherwise we use the
	// profiles from the default project.
	if projectName == Default || shared.IsTrue(project.Config["features.profiles"]) {
		profilesFilter.Project = projectName
	} else {
		profilesFilter.Project = Default
	}

	profiles, err := tx.GetProfiles(profilesFilter)
	if err != nil {
		return nil, errors.Wrap(err, "Fetch profiles from database")
	}

	instances, err := tx.GetInstances(db.InstanceFilter{
		Type:    instancetype.Any,
		Project: projectName,
	})
	if err != nil {
		return nil, errors.Wrap(err, "Fetch project instances from database")
	}

	volumes, err := tx.GetCustomVolumesInProject(projectName)
	if err != nil {
		return nil, errors.Wrap(err, "Fetch project custom volumes from database")
	}

	info := &projectInfo{
		Project:   project,
		Profiles:  profiles,
		Instances: instances,
		Volumes:   volumes,
	}

	return info, nil
}

// Expand the configuration and devices of the given instances, taking the give
// project profiles into account.
func expandInstancesConfigAndDevices(instances []db.Instance, profiles []db.Profile) []db.Instance {
	expandedInstances := make([]db.Instance, len(instances))

	// Index of all profiles by name.
	profilesByName := map[string]db.Profile{}
	for _, profile := range profiles {
		profilesByName[profile.Name] = profile
	}

	for i, instance := range instances {
		profiles := make([]api.Profile, len(instance.Profiles))

		for j, name := range instance.Profiles {
			profile := profilesByName[name]
			profiles[j] = *db.ProfileToAPI(&profile)
		}

		expandedInstances[i] = instance
		expandedInstances[i].Config = db.ExpandInstanceConfig(instance.Config, profiles)
		expandedInstances[i].Devices = db.ExpandInstanceDevices(
			deviceconfig.NewDevices(instance.Devices), profiles).CloneNative()
	}

	return expandedInstances
}

// Sum of the effective values for the given limits across all project
// enties (instances and custom volumes).
func getTotalsAcrossProjectEntities(info *projectInfo, keys []string, skipUnset bool) (map[string]int64, error) {
	totals := map[string]int64{}

	for _, key := range keys {
		totals[key] = 0
		if key == "limits.disk" {
			for _, volume := range info.Volumes {
				value, ok := volume.Config["size"]
				if !ok {
					if skipUnset {
						continue
					}

					return nil, fmt.Errorf(
						"Custom volume %s in project %s has no 'size' config set",
						volume.Name, info.Project.Name)
				}

				limit, err := units.ParseByteSizeString(value)
				if err != nil {
					return nil, errors.Wrapf(
						err, "Parse 'size' for custom volume %s in project %s",
						volume.Name, info.Project.Name)
				}
				totals[key] += limit
			}
		}
	}

	for _, instance := range info.Instances {
		limits, err := getInstanceLimits(instance, keys, skipUnset)
		if err != nil {
			return nil, err
		}

		for _, key := range keys {
			totals[key] += limits[key]
		}
	}

	return totals, nil
}

// Return the effective instance-level values for the limits with the given keys.
func getInstanceLimits(instance db.Instance, keys []string, skipUnset bool) (map[string]int64, error) {
	limits := map[string]int64{}

	for _, key := range keys {
		var value string
		var ok bool
		if key == "limits.disk" {
			_, device, err := shared.GetRootDiskDevice(instance.Devices)
			if err != nil {
				return nil, fmt.Errorf(
					"Instance %s in project %s has no root device",
					instance.Name, instance.Project)
			}

			value, ok = device["size"]
			if !ok || value == "" {
				if skipUnset {
					continue
				}

				return nil, fmt.Errorf(
					"Instance %s in project %s has no 'size' config set on the root device, "+
						"either directly or via a profile",
					instance.Name, instance.Project)
			}
		} else {
			value, ok = instance.Config[key]
			if !ok || value == "" {
				if skipUnset {
					continue
				}

				return nil, fmt.Errorf(
					"Instance %s in project %s has no '%s' config, "+
						"either directly or via a profile",
					instance.Name, instance.Project, key)
			}
		}

		parser := aggregateLimitConfigValueParsers[key]
		limit, err := parser(value)
		if err != nil {
			if skipUnset {
				continue
			}

			return nil, errors.Wrapf(
				err, "Parse '%s' for instance %s in project %s",
				key, instance.Name, instance.Project)
		}

		limits[key] = limit
	}

	return limits, nil
}

var aggregateLimitConfigValueParsers = map[string]func(string) (int64, error){
	"limits.memory": func(value string) (int64, error) {
		if strings.HasSuffix(value, "%") {
			return -1, fmt.Errorf("Value can't be a percentage")
		}
		return units.ParseByteSizeString(value)
	},
	"limits.processes": func(value string) (int64, error) {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return -1, err
		}
		return int64(limit), nil
	},
	"limits.cpu": func(value string) (int64, error) {
		if strings.Contains(value, ",") || strings.Contains(value, "-") {
			return -1, fmt.Errorf("CPUs can't be pinned if project limits are used")
		}

		limit, err := strconv.Atoi(value)
		if err != nil {
			return -1, err
		}

		return int64(limit), nil
	},
	"limits.disk": func(value string) (int64, error) {
		return units.ParseByteSizeString(value)
	},
}

var aggregateLimitConfigValuePrinters = map[string]func(int64) string{
	"limits.memory": func(limit int64) string {
		return units.GetByteSizeString(limit, 1)
	},
	"limits.processes": func(limit int64) string {
		return fmt.Sprintf("%d", limit)
	},
	"limits.cpu": func(limit int64) string {
		return fmt.Sprintf("%d", limit)
	},
	"limits.disk": func(limit int64) string {
		return units.GetByteSizeString(limit, 1)
	},
}

// FilterUsedBy filters a UsedBy list based on project access
func FilterUsedBy(r *http.Request, entries []string) []string {
	// Shortcut for admins and non-RBAC environments.
	if rbac.UserIsAdmin(r) {
		return entries
	}

	// Filter the entries.
	usedBy := []string{}
	for _, entry := range entries {
		projectName := Default

		// Try to parse the query part of the URL.
		u, err := url.Parse(entry)
		if err != nil {
			// Skip URLs we can't parse.
			continue
		}

		// Check if project= is specified in the URL.
		val := u.Query().Get("project")
		if val != "" {
			projectName = val
		}

		if !rbac.UserHasPermission(r, projectName, "view") {
			continue
		}

		usedBy = append(usedBy, entry)
	}

	return usedBy
}

// CheckClusterTargetRestriction check if user is allowed to use cluster member targeting
func CheckClusterTargetRestriction(tx *db.ClusterTx, r *http.Request, projectName string, targetFlag string) error {
	// Allow server administrators to move instances around even when restricted (node evacuation, ...)
	if rbac.UserIsAdmin(r) {
		return nil
	}

	project, err := tx.GetProject(projectName)
	if err != nil {
		return fmt.Errorf("Fetch project database object")
	}

	if !projectHasLimitsOrRestrictions(project) {
		return nil
	}

	key := "restricted.cluster.target"
	restrictionValue, ok := project.Config[key]
	if !ok {
		restrictionValue = defaultRestrictionsValues[key]
	}

	if restrictionValue == "block" && targetFlag != "" {
		return fmt.Errorf("This project doesn't allow cluster member targeting")
	}

	return nil
}

// Return true if particular restriction in project is violated
func projectHasRestriction(project *db.Project, restrictionKey string, blockValue string) bool {
	restricted := project.Config["restricted"]
	if !shared.IsTrue(restricted) {
		return false
	}

	restrictionValue, ok := project.Config[restrictionKey]
	if !ok {
		restrictionValue = defaultRestrictionsValues[restrictionKey]
	}

	if restrictionValue == blockValue {
		return true
	}

	return false
}

// AllowBackupCreation returns an error if any project-specific restriction is violated
// when creating a new backup in a project.
func AllowBackupCreation(tx *db.ClusterTx, projectName string) error {
	project, err := tx.GetProject(projectName)
	if err != nil {
		return err
	}

	if projectHasRestriction(project, "restricted.backups", "block") {
		return fmt.Errorf("Project %s doesn't allow for backup creation", projectName)
	}
	return nil
}

// AllowSnapshotCreation returns an error if any project-specific restriction is violated
// when creating a new snapshot in a project.
func AllowSnapshotCreation(tx *db.ClusterTx, projectName string) error {
	project, err := tx.GetProject(projectName)
	if err != nil {
		return err
	}

	if projectHasRestriction(project, "restricted.snapshots", "block") {
		return fmt.Errorf("Project %s doesn't allow for snapshot creation", projectName)
	}
	return nil
}
