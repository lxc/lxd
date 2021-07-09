package main

import (
	"fmt"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
)

func doProfileUpdate(d *Daemon, projectName string, name string, profile *api.Profile, req api.ProfilePut) error {
	// Check project limits.
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		return project.AllowProfileUpdate(tx, projectName, name, req)
	})
	if err != nil {
		return err
	}

	// Quick checks.
	err = validateProfileConfig(d, req.Config)
	if err != nil {
		return err
	}

	// Profiles can be applied to any instance type, so just use instancetype.Any type for validation so that
	// instance type specific validation checks are not performed.
	err = instance.ValidDevices(d.State(), d.cluster, projectName, instancetype.Any, deviceConfig.NewDevices(req.Devices), false)
	if err != nil {
		return err
	}

	insts, err := getProfileInstancesInfo(d.cluster, projectName, name)
	if err != nil {
		return errors.Wrapf(err, "Failed to query instances associated with profile %q", name)
	}

	// Check if the root disk device's pool is supposed to be changed or removed and prevent that if there are
	// instances using that root disk device.
	oldProfileRootDiskDeviceKey, oldProfileRootDiskDevice, _ := shared.GetRootDiskDevice(profile.Devices)
	_, newProfileRootDiskDevice, _ := shared.GetRootDiskDevice(req.Devices)
	if len(insts) > 0 && oldProfileRootDiskDevice["pool"] != "" && newProfileRootDiskDevice["pool"] == "" || (oldProfileRootDiskDevice["pool"] != newProfileRootDiskDevice["pool"]) {
		// Check for instances using the device.
		for _, inst := range insts {
			// Check if the device is locally overridden.
			k, v, _ := shared.GetRootDiskDevice(inst.Devices.CloneNative())
			if k != "" && v["pool"] != "" {
				continue
			}

			// Check what profile the device comes from by working backwards along the profiles list.
			for i := len(inst.Profiles) - 1; i >= 0; i-- {
				_, profile, err := d.cluster.GetProfile(projectName, inst.Profiles[i])
				if err != nil {
					return err
				}

				// Check if we find a match for the device.
				_, ok := profile.Devices[oldProfileRootDiskDeviceKey]
				if ok {
					// Found the profile.
					if inst.Profiles[i] == name {
						// If it's the current profile, then we can't modify that root device.
						return fmt.Errorf("At least one instance relies on this profile's root disk device")
					}

					// If it's not, then move on to the next instance.
					break
				}
			}
		}
	}

	// Update the database.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.UpdateProfile(projectName, name, db.Profile{
			Project:     projectName,
			Name:        name,
			Description: req.Description,
			Config:      req.Config,
			Devices:     req.Devices,
		})
	})
	if err != nil {
		return err
	}

	// Update all the instances on this node using the profile. Must be done after db.TxCommit due to DB lock.
	nodeName := ""
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		nodeName, err = tx.GetLocalNodeName()
		return err
	})
	if err != nil {
		return errors.Wrap(err, "failed to query local node name")
	}

	failures := map[*db.InstanceArgs]error{}
	for _, it := range insts {
		inst := it // Local var for instance pointer.
		err := doProfileUpdateInstance(d, name, profile.ProfilePut, nodeName, inst)
		if err != nil {
			failures[&inst] = err
		}
	}

	if len(failures) != 0 {
		msg := "The following instances failed to update (profile change still saved):\n"
		for inst, err := range failures {
			msg += fmt.Sprintf(" - Project: %s, Instance: %s: %v\n", inst.Project, inst.Name, err)
		}

		return fmt.Errorf("%s", msg)
	}

	return nil
}

// Like doProfileUpdate but does not update the database, since it was already
// updated by doProfileUpdate itself, called on the notifying node.
func doProfileUpdateCluster(d *Daemon, projectName string, name string, old api.ProfilePut) error {
	nodeName := ""
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error
		nodeName, err = tx.GetLocalNodeName()
		return err
	})
	if err != nil {
		return errors.Wrap(err, "Failed to query local node name")
	}

	insts, err := getProfileInstancesInfo(d.cluster, projectName, name)
	if err != nil {
		return errors.Wrapf(err, "Failed to query instances associated with profile %q", name)
	}

	failures := map[*db.InstanceArgs]error{}
	for _, it := range insts {
		inst := it // Local var for instance pointer.
		err := doProfileUpdateInstance(d, name, old, nodeName, inst)
		if err != nil {
			failures[&inst] = err
		}
	}

	if len(failures) != 0 {
		msg := "The following instances failed to update (profile change still saved):\n"
		for inst, err := range failures {
			msg += fmt.Sprintf(" - Project: %s, Instance: %s: %v\n", inst.Project, inst.Name, err)
		}

		return fmt.Errorf("%s", msg)
	}

	return nil
}

// Profile update of a single instance.
func doProfileUpdateInstance(d *Daemon, name string, old api.ProfilePut, nodeName string, args db.InstanceArgs) error {
	if args.Node != "" && args.Node != nodeName {
		// No-op, this instance does not belong to this node.
		return nil
	}

	profiles, err := d.cluster.GetProfiles(args.Project, args.Profiles)
	if err != nil {
		return err
	}

	for i, profileName := range args.Profiles {
		if profileName == name {
			// Overwrite the new config from the database with the old config and devices.
			profiles[i].Config = old.Config
			profiles[i].Devices = old.Devices
			break
		}
	}

	// Load the instance using the old profile config.
	inst, err := instance.Load(d.State(), args, profiles)
	if err != nil {
		return err
	}

	// Update will internally load the new profile configs and detect the changes to apply.
	return inst.Update(db.InstanceArgs{
		Architecture: inst.Architecture(),
		Config:       inst.LocalConfig(),
		Description:  inst.Description(),
		Devices:      inst.LocalDevices(),
		Ephemeral:    inst.IsEphemeral(),
		Profiles:     inst.Profiles(), // List of profile names to load from DB.
		Project:      inst.Project(),
		Type:         inst.Type(),
		Snapshot:     inst.IsSnapshot(),
	}, true)
}

// Query the db for information about instances associated with the given profile.
func getProfileInstancesInfo(cluster *db.Cluster, projectName string, profileName string) ([]db.InstanceArgs, error) {
	// Query the db for information about instances associated with the given profile.
	projectInstNames, err := cluster.GetInstancesWithProfile(projectName, profileName)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to query instances with profile %q", profileName)
	}

	instances := []db.InstanceArgs{}
	err = cluster.Transaction(func(tx *db.ClusterTx) error {
		for instProject, instNames := range projectInstNames {
			for _, instName := range instNames {
				inst, err := tx.GetInstance(instProject, instName)
				if err != nil {
					return err
				}

				instances = append(instances, db.InstanceToArgs(inst))
			}
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to fetch instances")
	}

	return instances, nil
}

func validateProfileConfig(d *Daemon, config map[string]string) error {
	err := instance.ValidConfig(d.os, config, true, instancetype.Any)
	if err != nil {
		return err
	}

	var profileKeyType = instancetype.Any
	for k := range config {
		_, t := shared.FindValidatorAndType(k)
		if profileKeyType != instancetype.Any && t != instancetype.Any && profileKeyType != t {
			return errors.New("Profiles with mixed instance specific configurations not allowed")
		}

		if t != instancetype.Any {
			profileKeyType = t
		}
	}

	return nil
}
