package acl

import (
	"fmt"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
)

// LoadByName loads and initialises a Network ACL from the database by project and name.
func LoadByName(s *state.State, projectName string, name string) (NetworkACL, error) {
	id, aclInfo, err := s.Cluster.GetNetworkACL(projectName, name)
	if err != nil {
		return nil, err
	}

	var acl NetworkACL = &common{} // Only a single driver currently.
	acl.init(s, id, projectName, aclInfo)

	return acl, nil
}

// Create validates supplied record and creates new Network ACL record in the database.
func Create(s *state.State, projectName string, aclInfo *api.NetworkACLsPost) error {
	var acl NetworkACL = &common{} // Only a single driver currently.
	acl.init(s, -1, projectName, nil)

	err := acl.validateName(aclInfo.Name)
	if err != nil {
		return err
	}

	err = acl.validateConfig(&aclInfo.NetworkACLPut)
	if err != nil {
		return err
	}

	// Insert DB record.
	_, err = s.Cluster.CreateNetworkACL(projectName, aclInfo)
	if err != nil {
		return err
	}

	return nil
}

// Exists checks the ACL name(s) provided exists in the project.
// If multiple names are provided, also checks that duplicate names aren't specified in the list.
func Exists(s *state.State, projectName string, name ...string) error {
	existingACLNames, err := s.Cluster.GetNetworkACLs(projectName)
	if err != nil {
		return err
	}

	checkedACLNames := make(map[string]struct{}, len(name))

	for _, aclName := range name {
		if !shared.StringInSlice(aclName, existingACLNames) {
			return fmt.Errorf("Network ACL %q does not exist", aclName)
		}

		_, found := checkedACLNames[aclName]
		if found {
			return fmt.Errorf("Network ACL %q specified multiple times", aclName)
		}

		checkedACLNames[aclName] = struct{}{}
	}

	return nil
}

// UsedBy finds all networks, profiles and instance NICs that use any of the specified ACLs and executes usageFunc
// once for each resource using one or more of the ACLs with info about the resource and matched ACLs being used.
func UsedBy(s *state.State, aclProjectName string, usageFunc func(matchedACLNames []string, usageType interface{}, nicName string, nicConfig map[string]string) error, matchACLNames ...string) error {
	if len(matchACLNames) <= 0 {
		return nil
	}

	// Find networks using the ACLs. Cheapest to do.
	networkNames, err := s.Cluster.GetCreatedNetworks(aclProjectName)
	if err != nil && err != db.ErrNoSuchObject {
		return errors.Wrapf(err, "Failed loading networks for project %q", aclProjectName)
	}

	for _, networkName := range networkNames {
		_, network, _, err := s.Cluster.GetNetworkInAnyState(aclProjectName, networkName)
		if err != nil {
			return errors.Wrapf(err, "Failed to get network config for %q", networkName)
		}

		netACLNames := util.SplitNTrimSpace(network.Config["security.acls"], ",", -1, true)
		matchedACLNames := []string{}
		for _, netACLName := range netACLNames {
			if shared.StringInSlice(netACLName, matchACLNames) {
				matchedACLNames = append(matchedACLNames, netACLName)
			}
		}

		if len(matchedACLNames) > 0 {
			// Call usageFunc with a list of matched ACLs and info about the network.
			err := usageFunc(matchedACLNames, network, "", nil)
			if err != nil {
				return err
			}
		}
	}

	// Look for profiles. Next cheapest to do.
	var profiles []db.Profile
	err = s.Cluster.Transaction(func(tx *db.ClusterTx) error {
		profiles, err = tx.GetProfiles(db.ProfileFilter{})
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, profile := range profiles {
		// Get the profiles's effective network project name.
		profileNetworkProjectName, _, err := project.NetworkProject(s.Cluster, profile.Project)
		if err != nil {
			return err
		}

		// Skip profiles who's effective network project doesn't match this Network ACL's project.
		if profileNetworkProjectName != aclProjectName {
			continue
		}

		// Iterate through each of the instance's devices, looking for NICs that are using any of the ACLs.
		for devName, devConfig := range deviceConfig.NewDevices(profile.Devices) {
			matchedACLNames := isInUseByDevice(devConfig, matchACLNames...)
			if len(matchedACLNames) > 0 {
				// Call usageFunc with a list of matched ACLs and info about the instance NIC.
				err := usageFunc(matchedACLNames, profile, devName, devConfig)
				if err != nil {
					return err
				}
			}
		}
	}

	// Find ACLs that have rules that reference the ACLs.
	aclNames, err := s.Cluster.GetNetworkACLs(aclProjectName)
	if err != nil {
		return err
	}

	for _, aclName := range aclNames {
		_, aclInfo, err := s.Cluster.GetNetworkACL(aclProjectName, aclName)
		if err != nil {
			return err
		}

		matchedACLNames := []string{}

		// Ingress rules can specify ACL names in their Source subjects.
		for _, rule := range aclInfo.Ingress {
			for _, subject := range util.SplitNTrimSpace(rule.Source, ",", -1, true) {

				// Look for new matching ACLs, but ignore our own ACL reference in our own rules.
				if shared.StringInSlice(subject, matchACLNames) && !shared.StringInSlice(subject, matchedACLNames) && subject != aclInfo.Name {
					matchedACLNames = append(matchedACLNames, subject)
				}
			}
		}

		// Egress rules can specify ACL names in their Destination subjects.
		for _, rule := range aclInfo.Egress {
			for _, subject := range util.SplitNTrimSpace(rule.Destination, ",", -1, true) {

				// Look for new matching ACLs, but ignore our own ACL reference in our own rules.
				if shared.StringInSlice(subject, matchACLNames) && !shared.StringInSlice(subject, matchedACLNames) && subject != aclInfo.Name {
					matchedACLNames = append(matchedACLNames, subject)
				}
			}
		}

		if len(matchedACLNames) > 0 {
			// Call usageFunc with a list of matched ACLs and info about the ACL.
			err := usageFunc(matchedACLNames, aclInfo, "", nil)
			if err != nil {
				return err
			}
		}
	}

	// Find instances using the ACLs. Most expensive to do.
	err = s.Cluster.InstanceList(nil, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
		// Get the instance's effective network project name.
		instNetworkProject := project.NetworkProjectFromRecord(&p)

		// Skip instances who's effective network project doesn't match this Network ACL's project.
		if instNetworkProject != aclProjectName {
			return nil
		}

		devices := db.ExpandInstanceDevices(deviceConfig.NewDevices(inst.Devices), profiles)

		// Iterate through each of the instance's devices, looking for NICs that are using any of the ACLs.
		for devName, devConfig := range devices {
			matchedACLNames := isInUseByDevice(devConfig, matchACLNames...)
			if len(matchedACLNames) > 0 {
				// Call usageFunc with a list of matched ACLs and info about the instance NIC.
				err := usageFunc(matchedACLNames, inst, devName, devConfig)
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

// isInUseByDevice returns any of the supplied matching ACL names found referenced by the NIC device.
func isInUseByDevice(d deviceConfig.Device, matchACLNames ...string) []string {
	matchedACLNames := []string{}

	// Only NICs linked to managed networks can use network ACLs.
	if d["type"] != "nic" || d["network"] == "" {
		return matchedACLNames
	}

	for _, nicACLName := range util.SplitNTrimSpace(d["security.acls"], ",", -1, true) {
		if shared.StringInSlice(nicACLName, matchACLNames) {
			matchedACLNames = append(matchedACLNames, nicACLName)
		}
	}

	return matchedACLNames
}

// NetworkACLUsage info about a network and what ACL it uses.
type NetworkACLUsage struct {
	ID     int64
	Name   string
	Type   string
	Config map[string]string
}

// NetworkUsage populates the provided aclNets map with networks that are using any of the specified ACLs.
func NetworkUsage(s *state.State, aclProjectName string, aclNames []string, aclNets map[string]NetworkACLUsage) error {
	supportedNetTypes := []string{"bridge", "ovn"}

	// Find all networks and instance/profile NICs that use any of the specified Network ACLs.
	err := UsedBy(s, aclProjectName, func(matchedACLNames []string, usageType interface{}, _ string, nicConfig map[string]string) error {
		switch u := usageType.(type) {
		case db.Instance, db.Profile:
			networkID, network, _, err := s.Cluster.GetNetworkInAnyState(aclProjectName, nicConfig["network"])
			if err != nil {
				return errors.Wrapf(err, "Failed to load network %q", nicConfig["network"])
			}

			if shared.StringInSlice(network.Type, supportedNetTypes) {
				if _, found := aclNets[network.Name]; !found {
					aclNets[network.Name] = NetworkACLUsage{
						ID:     networkID,
						Name:   network.Name,
						Type:   network.Type,
						Config: network.Config,
					}
				}
			}
		case *api.Network:
			if shared.StringInSlice(u.Type, supportedNetTypes) {
				if _, found := aclNets[u.Name]; !found {
					networkID, network, _, err := s.Cluster.GetNetworkInAnyState(aclProjectName, u.Name)
					if err != nil {
						return errors.Wrapf(err, "Failed to load network %q", u.Name)
					}

					aclNets[u.Name] = NetworkACLUsage{
						ID:     networkID,
						Name:   network.Name,
						Type:   network.Type,
						Config: network.Config,
					}
				}
			}
		case *api.NetworkACL:
			return nil // Nothing to do for ACL rules referencing us.
		default:
			return fmt.Errorf("Unrecognised usage type %T", u)
		}

		return nil
	}, aclNames...)
	if err != nil {
		return err
	}

	return nil
}
