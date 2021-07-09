package main

import (
	"fmt"
	"strings"

	"github.com/lxc/lxd/lxd/backup"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	storagePools "github.com/lxc/lxd/lxd/storage"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/version"
)

var supportedVolumeTypes = []int{db.StoragePoolVolumeTypeContainer, db.StoragePoolVolumeTypeVM, db.StoragePoolVolumeTypeCustom, db.StoragePoolVolumeTypeImage}
var supportedVolumeTypesInstances = []int{db.StoragePoolVolumeTypeContainer, db.StoragePoolVolumeTypeVM}

func storagePoolVolumeUpdateUsers(d *Daemon, projectName string, oldPoolName string, oldVol *api.StorageVolume, newPoolName string, newVol *api.StorageVolume) error {
	s := d.State()

	// Update all instances that are using the volume with a local (non-expanded) device.
	err := storagePools.VolumeUsedByInstanceDevices(s, oldPoolName, projectName, oldVol, false, func(dbInst db.Instance, project api.Project, profiles []api.Profile, usedByDevices []string) error {
		inst, err := instance.Load(s, db.InstanceToArgs(&dbInst), profiles)
		if err != nil {
			return err
		}

		localDevices := inst.LocalDevices()
		for _, devName := range usedByDevices {
			if _, exists := localDevices[devName]; exists {
				localDevices[devName]["pool"] = newPoolName
				localDevices[devName]["source"] = newVol.Name
			}
		}

		args := db.InstanceArgs{
			Architecture: inst.Architecture(),
			Description:  inst.Description(),
			Config:       inst.LocalConfig(),
			Devices:      localDevices,
			Ephemeral:    inst.IsEphemeral(),
			Profiles:     inst.Profiles(),
			Project:      inst.Project(),
			Type:         inst.Type(),
			Snapshot:     inst.IsSnapshot(),
		}

		err = inst.Update(args, false)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Update all profiles that are using the volume with a device.
	err = storagePools.VolumeUsedByProfileDevices(s, oldPoolName, projectName, oldVol, func(profile db.Profile, p api.Project, usedByDevices []string) error {
		for _, devName := range usedByDevices {
			if _, exists := profile.Devices[devName]; exists {
				profile.Devices[devName]["pool"] = newPoolName
				profile.Devices[devName]["source"] = newVol.Name
			}
		}

		pUpdate := api.ProfilePut{}
		pUpdate.Config = profile.Config
		pUpdate.Description = profile.Description
		pUpdate.Devices = profile.Devices
		apiProfile := db.ProfileToAPI(&profile)
		err = doProfileUpdate(d, profile.Project, profile.Name, apiProfile, pUpdate)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// volumeUsedBy = append(volumeUsedBy, fmt.Sprintf("/%s/containers/%s", version.APIVersion, ct))
func storagePoolVolumeUsedByGet(s *state.State, projectName string, poolName string, vol *api.StorageVolume) ([]string, error) {
	// Handle instance volumes.
	if vol.Type == db.StoragePoolVolumeTypeNameContainer || vol.Type == db.StoragePoolVolumeTypeNameVM {
		cName, sName, snap := shared.InstanceGetParentAndSnapshotName(vol.Name)
		if snap {
			if projectName == project.Default {
				return []string{fmt.Sprintf("/%s/instances/%s/snapshots/%s", version.APIVersion, cName, sName)}, nil
			}

			return []string{fmt.Sprintf("/%s/instances/%s/snapshots/%s?project=%s", version.APIVersion, cName, sName, projectName)}, nil
		}

		if projectName == project.Default {
			return []string{fmt.Sprintf("/%s/instances/%s", version.APIVersion, cName)}, nil
		}

		return []string{fmt.Sprintf("/%s/instances/%s?project=%s", version.APIVersion, cName, projectName)}, nil
	}

	// Handle image volumes.
	if vol.Type == db.StoragePoolVolumeTypeNameImage {
		if projectName == project.Default {
			return []string{fmt.Sprintf("/%s/images/%s", version.APIVersion, vol.Name)}, nil
		}

		return []string{fmt.Sprintf("/%s/images/%s?project=%s", version.APIVersion, vol.Name, projectName)}, nil
	}

	// Check if the daemon itself is using it.
	used, err := storagePools.VolumeUsedByDaemon(s, poolName, vol.Name)
	if err != nil {
		return []string{}, err
	}

	if used {
		return []string{fmt.Sprintf("/%s", version.APIVersion)}, nil
	}

	// Look for instances using this volume.
	volumeUsedBy := []string{}

	// Pass false to expandDevices, as we only want to see instances directly using a volume, rather than their
	// profiles using a volume.
	err = storagePools.VolumeUsedByInstanceDevices(s, poolName, projectName, vol, false, func(inst db.Instance, p api.Project, profiles []api.Profile, usedByDevices []string) error {
		if inst.Project == project.Default {
			volumeUsedBy = append(volumeUsedBy, fmt.Sprintf("/%s/instances/%s", version.APIVersion, inst.Name))
		} else {
			volumeUsedBy = append(volumeUsedBy, fmt.Sprintf("/%s/instances/%s?project=%s", version.APIVersion, inst.Name, inst.Project))
		}

		return nil
	})
	if err != nil {
		return []string{}, err
	}

	err = storagePools.VolumeUsedByProfileDevices(s, poolName, projectName, vol, func(profile db.Profile, p api.Project, usedByDevices []string) error {
		if profile.Project == project.Default {
			volumeUsedBy = append(volumeUsedBy, fmt.Sprintf("/%s/profiles/%s", version.APIVersion, profile.Name))
		} else {
			volumeUsedBy = append(volumeUsedBy, fmt.Sprintf("/%s/profiles/%s?project=%s", version.APIVersion, profile.Name, profile.Project))
		}

		return nil
	})
	if err != nil {
		return []string{}, err
	}

	return volumeUsedBy, nil
}

func storagePoolVolumeBackupLoadByName(s *state.State, projectName, poolName, backupName string) (*backup.VolumeBackup, error) {
	b, err := s.Cluster.GetStoragePoolVolumeBackup(projectName, poolName, backupName)
	if err != nil {
		return nil, err
	}

	volumeName := strings.Split(backupName, "/")[0]
	backup := backup.NewVolumeBackup(s, projectName, poolName, volumeName, b.ID, b.Name, b.CreationDate, b.ExpiryDate, b.VolumeOnly, b.OptimizedStorage)

	return backup, nil
}
