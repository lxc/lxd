package storage

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/rsync"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/ioprogress"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
	"github.com/lxc/lxd/shared/units"
	"github.com/lxc/lxd/shared/validate"
)

// ValidName validates the provided name, and returns an error if it's not a valid storage name.
func ValidName(value string) error {
	if strings.Contains(value, "/") {
		return fmt.Errorf(`Storage name cannot contain "/"`)
	}

	return nil
}

// ConfigDiff returns a diff of the provided configs. Additionally, it returns whether or not
// only user properties have been changed.
func ConfigDiff(oldConfig map[string]string, newConfig map[string]string) ([]string, bool) {
	changedConfig := []string{}
	userOnly := true
	for key := range oldConfig {
		if oldConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	for key := range newConfig {
		if oldConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	// Skip on no change
	if len(changedConfig) == 0 {
		return nil, false
	}

	return changedConfig, userOnly
}

// VolumeTypeNameToDBType converts a volume type string to internal volume type DB code.
func VolumeTypeNameToDBType(volumeTypeName string) (int, error) {
	switch volumeTypeName {
	case db.StoragePoolVolumeTypeNameContainer:
		return db.StoragePoolVolumeTypeContainer, nil
	case db.StoragePoolVolumeTypeNameVM:
		return db.StoragePoolVolumeTypeVM, nil
	case db.StoragePoolVolumeTypeNameImage:
		return db.StoragePoolVolumeTypeImage, nil
	case db.StoragePoolVolumeTypeNameCustom:
		return db.StoragePoolVolumeTypeCustom, nil
	}

	return -1, fmt.Errorf("Invalid storage volume type name")
}

// VolumeTypeToDBType converts volume type to internal volume type DB code.
func VolumeTypeToDBType(volType drivers.VolumeType) (int, error) {
	switch volType {
	case drivers.VolumeTypeContainer:
		return db.StoragePoolVolumeTypeContainer, nil
	case drivers.VolumeTypeVM:
		return db.StoragePoolVolumeTypeVM, nil
	case drivers.VolumeTypeImage:
		return db.StoragePoolVolumeTypeImage, nil
	case drivers.VolumeTypeCustom:
		return db.StoragePoolVolumeTypeCustom, nil
	}

	return -1, fmt.Errorf("Invalid storage volume type")
}

// VolumeDBTypeToType converts internal volume type DB code to storage driver volume type.
func VolumeDBTypeToType(volDBType int) (drivers.VolumeType, error) {
	switch volDBType {
	case db.StoragePoolVolumeTypeContainer:
		return drivers.VolumeTypeContainer, nil
	case db.StoragePoolVolumeTypeVM:
		return drivers.VolumeTypeVM, nil
	case db.StoragePoolVolumeTypeImage:
		return drivers.VolumeTypeImage, nil
	case db.StoragePoolVolumeTypeCustom:
		return drivers.VolumeTypeCustom, nil
	}

	return "", fmt.Errorf("Invalid storage volume type")
}

// InstanceTypeToVolumeType converts instance type to storage driver volume type.
func InstanceTypeToVolumeType(instType instancetype.Type) (drivers.VolumeType, error) {
	switch instType {
	case instancetype.Container:
		return drivers.VolumeTypeContainer, nil
	case instancetype.VM:
		return drivers.VolumeTypeVM, nil
	}

	return "", fmt.Errorf("Invalid instance type")
}

// VolumeTypeToAPIInstanceType converts storage driver volume type to API instance type type.
func VolumeTypeToAPIInstanceType(volType drivers.VolumeType) (api.InstanceType, error) {
	switch volType {
	case drivers.VolumeTypeContainer:
		return api.InstanceTypeContainer, nil
	case drivers.VolumeTypeVM:
		return api.InstanceTypeVM, nil
	}

	return api.InstanceTypeAny, fmt.Errorf("Volume type doesn't have equivalent instance type")
}

// VolumeContentTypeToDBContentType converts volume type to internal code.
func VolumeContentTypeToDBContentType(contentType drivers.ContentType) (int, error) {
	switch contentType {
	case drivers.ContentTypeBlock:
		return db.StoragePoolVolumeContentTypeBlock, nil
	case drivers.ContentTypeFS:
		return db.StoragePoolVolumeContentTypeFS, nil
	}

	return -1, fmt.Errorf("Invalid volume content type")
}

// VolumeDBContentTypeToContentType converts internal content type DB code to driver representation.
func VolumeDBContentTypeToContentType(volDBType int) (drivers.ContentType, error) {
	switch volDBType {
	case db.StoragePoolVolumeContentTypeBlock:
		return drivers.ContentTypeBlock, nil
	case db.StoragePoolVolumeContentTypeFS:
		return drivers.ContentTypeFS, nil
	}

	return "", fmt.Errorf("Invalid volume content type")
}

// VolumeContentTypeNameToContentType converts volume content type string internal code.
func VolumeContentTypeNameToContentType(contentTypeName string) (int, error) {
	switch contentTypeName {
	case db.StoragePoolVolumeContentTypeNameFS:
		return db.StoragePoolVolumeContentTypeFS, nil
	case db.StoragePoolVolumeContentTypeNameBlock:
		return db.StoragePoolVolumeContentTypeBlock, nil
	}

	return -1, fmt.Errorf("Invalid volume content type name")
}

// VolumeDBCreate creates a volume in the database.
func VolumeDBCreate(s *state.State, pool Pool, projectName string, volumeName string, volumeDescription string, volumeType drivers.VolumeType, snapshot bool, volumeConfig map[string]string, expiryDate time.Time, contentType drivers.ContentType) error {
	// Convert the volume type to our internal integer representation.
	volDBType, err := VolumeTypeToDBType(volumeType)
	if err != nil {
		return err
	}

	volDBContentType, err := VolumeContentTypeToDBContentType(contentType)
	if err != nil {
		return err
	}

	// Check that a storage volume of the same storage volume type does not already exist.
	volumeID, _ := s.Cluster.GetStoragePoolNodeVolumeID(projectName, volumeName, volDBType, pool.ID())
	if volumeID > 0 {
		return fmt.Errorf("A storage volume of type %q already exists", volumeType)
	}

	// Make sure that we don't pass a nil to the next function.
	if volumeConfig == nil {
		volumeConfig = map[string]string{}
	}

	volType, err := VolumeDBTypeToType(volDBType)
	if err != nil {
		return err
	}

	vol := drivers.NewVolume(pool.Driver(), pool.Name(), volType, contentType, volumeName, volumeConfig, pool.Driver().Config())

	// Fill default config.
	err = pool.Driver().FillVolumeConfig(vol)
	if err != nil {
		return err
	}

	// Validate config.
	err = pool.Driver().ValidateVolume(vol, false)
	if err != nil {
		return err
	}

	// Create the database entry for the storage volume.
	if snapshot {
		_, err = s.Cluster.CreateStorageVolumeSnapshot(projectName, volumeName, volumeDescription, volDBType, pool.ID(), vol.Config(), expiryDate)
	} else {
		_, err = s.Cluster.CreateStoragePoolVolume(projectName, volumeName, volumeDescription, volDBType, pool.ID(), vol.Config(), volDBContentType)
	}
	if err != nil {
		return fmt.Errorf("Error inserting volume %q for project %q in pool %q of type %q into database %q", volumeName, projectName, pool.Name(), volumeType, err)
	}

	return nil
}

// SupportedPoolTypes the types of pools supported.
// Deprecated: this is being replaced with drivers.SupportedDrivers()
var SupportedPoolTypes = []string{"btrfs", "ceph", "cephfs", "dir", "lvm", "zfs"}

// StorageVolumeConfigKeys config validation for btrfs, ceph, cephfs, dir, lvm, zfs types.
// Deprecated: these are being moved to the per-storage-driver implementations.
var StorageVolumeConfigKeys = map[string]func(value string) ([]string, error){
	"block.filesystem": func(value string) ([]string, error) {
		err := validate.Optional(func(value string) error {
			return validate.IsOneOf(value, []string{"btrfs", "ext4", "xfs"})
		})(value)
		if err != nil {
			return nil, err
		}

		return []string{"ceph", "lvm"}, nil
	},
	"block.mount_options": func(value string) ([]string, error) {
		return []string{"ceph", "lvm"}, validate.IsAny(value)
	},
	"security.shifted": func(value string) ([]string, error) {
		return SupportedPoolTypes, validate.Optional(validate.IsBool)(value)
	},
	"security.unmapped": func(value string) ([]string, error) {
		return SupportedPoolTypes, validate.Optional(validate.IsBool)(value)
	},
	"size": func(value string) ([]string, error) {
		if value == "" {
			return SupportedPoolTypes, nil
		}

		_, err := units.ParseByteSizeString(value)
		if err != nil {
			return nil, err
		}

		return SupportedPoolTypes, nil
	},
	"volatile.idmap.last": func(value string) ([]string, error) {
		return SupportedPoolTypes, validate.IsAny(value)
	},
	"volatile.idmap.next": func(value string) ([]string, error) {
		return SupportedPoolTypes, validate.IsAny(value)
	},
	"zfs.remove_snapshots": func(value string) ([]string, error) {
		err := validate.Optional(validate.IsBool)(value)
		if err != nil {
			return nil, err
		}

		return []string{"zfs"}, nil
	},
	"zfs.use_refquota": func(value string) ([]string, error) {
		err := validate.Optional(validate.IsBool)(value)
		if err != nil {
			return nil, err
		}

		return []string{"zfs"}, nil
	},
}

// VolumeSnapshotsGet returns a list of snapshots of the form <volume>/<snapshot-name>.
func VolumeSnapshotsGet(s *state.State, projectName string, pool string, volume string, volType int) ([]db.StorageVolumeArgs, error) {
	poolID, err := s.Cluster.GetStoragePoolID(pool)
	if err != nil {
		return nil, err
	}

	snapshots, err := s.Cluster.GetLocalStoragePoolVolumeSnapshotsWithType(projectName, volume, volType, poolID)
	if err != nil {
		return nil, err
	}

	return snapshots, nil
}

// VolumePropertiesTranslate validates the supplied volume config and removes any keys that are not
// suitable for the volume's driver type.
func VolumePropertiesTranslate(targetConfig map[string]string, targetParentPoolDriver string) (map[string]string, error) {
	newConfig := make(map[string]string, len(targetConfig))
	for key, val := range targetConfig {
		// User keys are not validated.
		if strings.HasPrefix(key, "user.") {
			continue
		}

		// Validate storage volume config keys.
		validator, ok := StorageVolumeConfigKeys[key]
		if !ok {
			return nil, fmt.Errorf("Invalid storage volume configuration key: %s", key)
		}

		validStorageDrivers, err := validator(val)
		if err != nil {
			return nil, err
		}

		// Drop invalid keys.
		if !shared.StringInSlice(targetParentPoolDriver, validStorageDrivers) {
			continue
		}

		newConfig[key] = val
	}

	return newConfig, nil
}

// validatePoolCommonRules returns a map of pool config rules common to all drivers.
func validatePoolCommonRules() map[string]func(string) error {
	return map[string]func(string) error{
		"source":                  validate.IsAny,
		"volatile.initial_source": validate.IsAny,
		"volume.size":             validate.Optional(validate.IsSize),
		"size":                    validate.Optional(validate.IsSize),
		"rsync.bwlimit":           validate.IsAny,
		"rsync.compression":       validate.Optional(validate.IsBool),
	}
}

// validateVolumeCommonRules returns a map of volume config rules common to all drivers.
func validateVolumeCommonRules(vol drivers.Volume) map[string]func(string) error {
	rules := map[string]func(string) error{
		// Note: size should not be modifiable for non-custom volumes and should be checked
		// in the relevant volume update functions.
		"size": validate.Optional(validate.IsSize),
		"snapshots.expiry": func(value string) error {
			// Validate expression
			_, err := shared.GetSnapshotExpiry(time.Time{}, value)
			return err
		},
		"snapshots.schedule": validate.Optional(validate.IsCron([]string{"@hourly", "@daily", "@midnight", "@weekly", "@monthly", "@annually", "@yearly"})),
		"snapshots.pattern":  validate.IsAny,
	}

	// volatile.idmap settings only make sense for filesystem volumes.
	if vol.ContentType() == drivers.ContentTypeFS {
		rules["volatile.idmap.last"] = validate.IsAny
		rules["volatile.idmap.next"] = validate.IsAny
	}

	// block.mount_options and block.filesystem settings are only relevant for drivers that are block backed
	// and when there is a filesystem to actually mount. This includes filesystem volumes and VM Block volumes,
	// as they have an associated config filesystem volume that shares the config.
	if vol.IsBlockBacked() && (vol.ContentType() == drivers.ContentTypeFS || vol.IsVMBlock()) {
		rules["block.mount_options"] = validate.IsAny

		// Note: block.filesystem should not be modifiable after volume created.
		// This should be checked in the relevant volume update functions.
		rules["block.filesystem"] = validate.IsAny
	}

	// security.shifted and security.unmapped are only relevant for custom filesystem volumes.
	if vol.Type() == drivers.VolumeTypeCustom && vol.ContentType() == drivers.ContentTypeFS {
		rules["security.shifted"] = validate.Optional(validate.IsBool)
		rules["security.unmapped"] = validate.Optional(validate.IsBool)
	}

	// volatile.rootfs.size is only used for image volumes.
	if vol.Type() == drivers.VolumeTypeImage {
		rules["volatile.rootfs.size"] = validate.Optional(validate.IsInt64)
	}

	return rules
}

// ImageUnpack unpacks a filesystem image into the destination path.
// There are several formats that images can come in:
// Container Format A: Separate metadata tarball and root squashfs file.
// 	- Unpack metadata tarball into mountPath.
//	- Unpack root squashfs file into mountPath/rootfs.
// Container Format B: Combined tarball containing metadata files and root squashfs.
//	- Unpack combined tarball into mountPath.
// VM Format A: Separate metadata tarball and root qcow2 file.
// 	- Unpack metadata tarball into mountPath.
//	- Check rootBlockPath is a file and convert qcow2 file into raw format in rootBlockPath.
func ImageUnpack(imageFile string, vol drivers.Volume, destBlockFile string, blockBackend, runningInUserns bool, allowUnsafeResize bool, tracker *ioprogress.ProgressTracker) (int64, error) {
	logger := logging.AddContext(logger.Log, log.Ctx{"imageFile": imageFile, "vol": vol.Name()})

	// For all formats, first unpack the metadata (or combined) tarball into destPath.
	imageRootfsFile := imageFile + ".rootfs"
	destPath := vol.MountPath()

	// If no destBlockFile supplied then this is a container image unpack.
	if destBlockFile == "" {
		rootfsPath := filepath.Join(destPath, "rootfs")

		// Unpack the main image file.
		err := shared.Unpack(imageFile, destPath, blockBackend, runningInUserns, tracker)
		if err != nil {
			return -1, err
		}

		// Check for separate root file.
		if shared.PathExists(imageRootfsFile) {
			err = os.MkdirAll(rootfsPath, 0755)
			if err != nil {
				return -1, fmt.Errorf("Error creating rootfs directory")
			}

			err = shared.Unpack(imageRootfsFile, rootfsPath, blockBackend, runningInUserns, tracker)
			if err != nil {
				return -1, err
			}
		}

		// Check that the container image unpack has resulted in a rootfs dir.
		if !shared.PathExists(rootfsPath) {
			return -1, fmt.Errorf("Image is missing a rootfs: %s", imageFile)
		}

		// Done with this.
		return 0, nil
	}

	// If a rootBlockPath is supplied then this is a VM image unpack.

	// Validate the target.
	fileInfo, err := os.Stat(destBlockFile)
	if err != nil && !os.IsNotExist(err) {
		return -1, err
	}

	if fileInfo != nil && fileInfo.IsDir() {
		// If the dest block file exists, and it is a directory, fail.
		return -1, fmt.Errorf("Root block path isn't a file: %s", destBlockFile)
	}

	// convertBlockImage converts the qcow2 block image file into a raw block device. If needed it will attempt
	// to enlarge the destination volume to accommodate the unpacked qcow2 image file.
	convertBlockImage := func(v drivers.Volume, imgPath string, dstPath string) (int64, error) {
		// Get info about qcow2 file.
		imgJSON, err := shared.RunCommand("qemu-img", "info", "--output=json", imgPath)
		if err != nil {
			return -1, errors.Wrapf(err, "Failed reading image info %q", dstPath)
		}

		imgInfo := struct {
			Format      string `json:"format"`
			VirtualSize int64  `json:"virtual-size"`
		}{}

		err = json.Unmarshal([]byte(imgJSON), &imgInfo)
		if err != nil {
			return -1, err
		}

		if imgInfo.Format != "qcow2" {
			return -1, fmt.Errorf("Unexpected image format %q", imgInfo.Format)
		}

		// Check whether image is allowed to be unpacked into pool volume. Create a partial image volume
		// struct and then use it to check that target volume size can be set as needed.
		imgVolConfig := map[string]string{
			"volatile.rootfs.size": fmt.Sprintf("%d", imgInfo.VirtualSize),
		}
		imgVol := drivers.NewVolume(nil, "", drivers.VolumeTypeImage, drivers.ContentTypeBlock, "", imgVolConfig, nil)

		logger.Debug("Checking image unpack size")
		newVolSize, err := vol.ConfigSizeFromSource(imgVol)
		if err != nil {
			return -1, err
		}

		if shared.PathExists(dstPath) {
			volSizeBytes, err := drivers.BlockDiskSizeBytes(dstPath)
			if err != nil {
				return -1, errors.Wrapf(err, "Error getting current size of %q", dstPath)
			}

			// If the target volume's size is smaller than the image unpack size, then we need to
			// increase the target volume's size.
			if volSizeBytes < imgInfo.VirtualSize {
				logger.Debug("Increasing volume size", log.Ctx{"imgPath": imgPath, "dstPath": dstPath, "oldSize": volSizeBytes, "newSize": newVolSize})
				err = vol.SetQuota(newVolSize, allowUnsafeResize, nil)
				if err != nil {
					return -1, errors.Wrapf(err, "Error increasing volume size")
				}
			}
		}

		// Convert the qcow2 format to a raw block device using qemu's dd mode to avoid issues with
		// loop backed storage pools. Use the MinBlockBoundary block size to speed up conversion.
		logger.Debug("Converting qcow2 image to raw disk", log.Ctx{"imgPath": imgPath, "dstPath": dstPath})
		_, err = shared.RunCommand("qemu-img", "dd", "-f", "qcow2", "-O", "raw", fmt.Sprintf("bs=%d", drivers.MinBlockBoundary), fmt.Sprintf("if=%s", imgPath), fmt.Sprintf("of=%s", dstPath))
		if err != nil {
			return -1, errors.Wrapf(err, "Failed converting image to raw at %q", dstPath)
		}

		return imgInfo.VirtualSize, nil
	}

	var imgSize int64

	if shared.PathExists(imageRootfsFile) {
		// Unpack the main image file.
		err := shared.Unpack(imageFile, destPath, blockBackend, runningInUserns, tracker)
		if err != nil {
			return -1, err
		}

		// Convert the qcow2 format to a raw block device.
		imgSize, err = convertBlockImage(vol, imageRootfsFile, destBlockFile)
		if err != nil {
			return -1, err
		}
	} else {
		// Dealing with unified tarballs require an initial unpack to a temporary directory.
		tempDir, err := ioutil.TempDir(shared.VarPath("images"), "lxd_image_unpack_")
		if err != nil {
			return -1, err
		}
		defer os.RemoveAll(tempDir)

		// Unpack the whole image.
		err = shared.Unpack(imageFile, tempDir, blockBackend, runningInUserns, tracker)
		if err != nil {
			return -1, err
		}

		imgPath := filepath.Join(tempDir, "rootfs.img")

		// Convert the qcow2 format to a raw block device.
		imgSize, err = convertBlockImage(vol, imgPath, destBlockFile)
		if err != nil {
			return -1, err
		}

		// Delete the qcow2.
		err = os.Remove(imgPath)
		if err != nil {
			return -1, errors.Wrapf(err, "Failed to remove %q", imgPath)
		}

		// Transfer the content excluding the destBlockFile name so that we don't delete the block file
		// created above if the storage driver stores image files in the same directory as destPath.
		_, err = rsync.LocalCopy(tempDir, destPath, "", true, "--exclude", filepath.Base(destBlockFile))
		if err != nil {
			return -1, err
		}
	}

	return imgSize, nil
}

// InstanceContentType returns the instance's content type.
func InstanceContentType(inst instance.Instance) drivers.ContentType {
	contentType := drivers.ContentTypeFS
	if inst.Type() == instancetype.VM {
		contentType = drivers.ContentTypeBlock
	}

	return contentType
}

// VolumeUsedByProfileDevices finds profiles using a volume and passes them to profileFunc for evaluation.
// The profileFunc is provided with a profile config, project config and a list of device names that are using
// the volume.
func VolumeUsedByProfileDevices(s *state.State, poolName string, projectName string, vol *api.StorageVolume, profileFunc func(profile db.Profile, project api.Project, usedByDevices []string) error) error {
	// Convert the volume type name to our internal integer representation.
	volumeType, err := VolumeTypeNameToDBType(vol.Type)
	if err != nil {
		return err
	}

	projectMap := map[string]api.Project{}
	var profiles []db.Profile

	// Retrieve required info from the database in single transaction for performance.
	err = s.Cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error

		projects, err := tx.GetProjects(db.ProjectFilter{})
		if err != nil {
			return errors.Wrap(err, "Failed loading projects")
		}

		// Index of all projects by name.
		for i, project := range projects {
			projectMap[project.Name] = projects[i]
		}

		profiles, err = tx.GetProfiles(db.ProfileFilter{})
		if err != nil {
			return errors.Wrap(err, "Failed loading profiles")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Iterate all profiles, consider only those which belong to a project that has the same effective
	// storage project as volume.
	for _, profile := range profiles {
		p := projectMap[profile.Project]
		profileStorageProject := project.StorageVolumeProjectFromRecord(&p, volumeType)
		if err != nil {
			return err
		}

		// Check profile's storage project is the same as the volume's project.
		// If not then the volume names mentioned in the profile's config cannot be referring to volumes
		// in the volume's project we are trying to match, and this profile cannot possibly be using it.
		if projectName != profileStorageProject {
			continue
		}

		var usedByDevices []string

		// Iterate through each of the profiles's devices, looking for disks in the same pool as volume.
		// Then try and match the volume name against the profile device's "source" property.
		for devName, dev := range profile.Devices {
			if dev["type"] != "disk" {
				continue
			}

			if dev["pool"] != poolName {
				continue
			}

			if dev["source"] == vol.Name {
				usedByDevices = append(usedByDevices, devName)
			}
		}

		if len(usedByDevices) > 0 {
			err = profileFunc(profile, p, usedByDevices)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// VolumeUsedByInstanceDevices finds instances using a volume (either directly or via their expanded profiles if
// expandDevices is true) and passes them to instanceFunc for evaluation. If instanceFunc returns an error then it
// is returned immediately. The instanceFunc is executed during a DB transaction, so DB queries are not permitted.
// The instanceFunc is provided with a instance config, project config, instance's profiles and a list of device
// names that are using the volume.
func VolumeUsedByInstanceDevices(s *state.State, poolName string, projectName string, vol *api.StorageVolume, expandDevices bool, instanceFunc func(inst db.Instance, project api.Project, profiles []api.Profile, usedByDevices []string) error) error {
	// Convert the volume type name to our internal integer representation.
	volumeType, err := VolumeTypeNameToDBType(vol.Type)
	if err != nil {
		return err
	}

	return s.Cluster.InstanceList(nil, func(inst db.Instance, p api.Project, profiles []api.Profile) error {
		// If the volume has a specific cluster member which is different than the instance then skip as
		// instance cannot be using this volume.
		if vol.Location != "" && inst.Node != vol.Location {
			return nil
		}

		instStorageProject := project.StorageVolumeProjectFromRecord(&p, volumeType)
		if err != nil {
			return err
		}

		// Check instance's storage project is the same as the volume's project.
		// If not then the volume names mentioned in the instance's config cannot be referring to volumes
		// in the volume's project we are trying to match, and this instance cannot possibly be using it.
		if projectName != instStorageProject {
			return nil
		}

		// Use local devices for usage check by if expandDevices is false (but don't modify instance).
		devices := inst.Devices

		// Expand devices for usage check if expandDevices is true.
		if expandDevices {
			devices = db.ExpandInstanceDevices(deviceConfig.NewDevices(inst.Devices), profiles).CloneNative()
		}

		var usedByDevices []string

		// Iterate through each of the instance's devices, looking for disks in the same pool as volume.
		// Then try and match the volume name against the instance device's "source" property.
		for devName, dev := range devices {
			if dev["type"] != "disk" {
				continue
			}

			if dev["pool"] != poolName {
				continue
			}

			if dev["source"] == vol.Name {
				usedByDevices = append(usedByDevices, devName)
			}
		}

		if len(usedByDevices) > 0 {
			err = instanceFunc(inst, p, profiles, usedByDevices)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// VolumeUsedByExclusiveRemoteInstancesWithProfiles checks if custom volume is exclusively attached to a remote
// instance. Returns the remote instance that has the volume exclusively attached. Returns nil if volume available.
func VolumeUsedByExclusiveRemoteInstancesWithProfiles(s *state.State, poolName string, projectName string, vol *api.StorageVolume) (*db.Instance, error) {
	pool, err := GetPoolByName(s, poolName)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed loading storage pool %q", poolName)
	}

	info := pool.Driver().Info()

	// Always return nil if the storage driver supports mounting volumes on multiple nodes at once.
	if info.VolumeMultiNode {
		return nil, nil
	}

	// Get local node name so we can check if the volume is attached to a remote node.
	var localNode string
	err = s.Cluster.Transaction(func(tx *db.ClusterTx) error {
		localNode, err = tx.GetLocalNodeName()
		if err != nil {
			return errors.Wrapf(err, "Failed to get local node name")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Find if volume is attached to a remote instance.
	var remoteInstance *db.Instance
	err = VolumeUsedByInstanceDevices(s, poolName, projectName, vol, true, func(dbInst db.Instance, project api.Project, profiles []api.Profile, usedByDevices []string) error {
		if dbInst.Node != localNode {
			remoteInstance = &dbInst
			return db.ErrInstanceListStop // Stop the search, this volume is attached to a remote instance.
		}

		return nil
	})
	if err != nil && err != db.ErrInstanceListStop {
		return nil, err
	}

	return remoteInstance, nil
}

// VolumeUsedByDaemon indicates whether the volume is used by daemon storage.
func VolumeUsedByDaemon(s *state.State, poolName string, volumeName string) (bool, error) {
	var storageBackups string
	var storageImages string
	err := s.Node.Transaction(func(tx *db.NodeTx) error {
		nodeConfig, err := node.ConfigLoad(tx)
		if err != nil {
			return err
		}

		storageBackups = nodeConfig.StorageBackupsVolume()
		storageImages = nodeConfig.StorageImagesVolume()

		return nil
	})
	if err != nil {
		return false, err
	}

	fullName := fmt.Sprintf("%s/%s", poolName, volumeName)
	if storageBackups == fullName || storageImages == fullName {
		return true, nil
	}

	return false, nil
}

// FallbackMigrationType returns the fallback migration transport to use based on volume content type.
func FallbackMigrationType(contentType drivers.ContentType) migration.MigrationFSType {
	if contentType == drivers.ContentTypeBlock {
		return migration.MigrationFSType_BLOCK_AND_RSYNC
	}

	return migration.MigrationFSType_RSYNC
}

// RenderSnapshotUsage can be used as an optional argument to Instance.Render() to return snapshot usage.
// As this is a relatively expensive operation it is provided as an optional feature rather than on by default.
func RenderSnapshotUsage(s *state.State, snapInst instance.Instance) func(response interface{}) error {
	return func(response interface{}) error {
		apiRes, ok := response.(*api.InstanceSnapshot)
		if !ok {
			return nil
		}

		pool, err := GetPoolByInstance(s, snapInst)
		if err == nil {
			// It is important that the snapshot not be mounted here as mounting a snapshot can trigger a very
			// expensive filesystem UUID regeneration, so we rely on the driver implementation to get the info
			// we are requesting as cheaply as possible.
			apiRes.Size, _ = pool.GetInstanceUsage(snapInst)
		}

		return nil
	}
}

// InstanceMount mounts an instance's storage volume (if not already mounted).
// Please call InstanceUnmount when finished.
func InstanceMount(pool Pool, inst instance.Instance, op *operations.Operation) (*MountInfo, error) {
	var err error
	var mountInfo *MountInfo

	if inst.IsSnapshot() {
		mountInfo, err = pool.MountInstanceSnapshot(inst, op)
		if err != nil {
			return nil, err
		}
	} else {
		mountInfo, err = pool.MountInstance(inst, op)
		if err != nil {
			return nil, err
		}
	}

	return mountInfo, nil
}

// InstanceUnmount unmounts an instance's storage volume (if not in use). Returns if we unmounted the volume.
func InstanceUnmount(pool Pool, inst instance.Instance, op *operations.Operation) (bool, error) {
	var err error
	var ourUnmount bool

	if inst.IsSnapshot() {
		ourUnmount, err = pool.UnmountInstanceSnapshot(inst, op)
	} else {
		ourUnmount, err = pool.UnmountInstance(inst, op)
	}

	return ourUnmount, err
}

// InstanceDiskBlockSize returns the block device size for the instance's disk.
// This will mount the instance if not already mounted and will unmount at the end if needed.
func InstanceDiskBlockSize(pool Pool, inst instance.Instance, op *operations.Operation) (int64, error) {
	mountInfo, err := InstanceMount(pool, inst, op)
	if err != nil {
		return -1, err
	}
	defer InstanceUnmount(pool, inst, op)

	if mountInfo.DiskPath == "" {
		return -1, fmt.Errorf("No disk path available from mount")
	}

	blockDiskSize, err := drivers.BlockDiskSizeBytes(mountInfo.DiskPath)
	if err != nil {
		return -1, errors.Wrapf(err, "Error getting block disk size %q", mountInfo.DiskPath)
	}

	return blockDiskSize, nil
}
