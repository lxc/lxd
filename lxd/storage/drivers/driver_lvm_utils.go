package drivers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/locking"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/shared"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/units"
	"github.com/lxc/lxd/shared/version"
)

// lvmBlockVolSuffix suffix used for block content type svolumes.
const lvmBlockVolSuffix = ".block"

// lvmSnapshotSeparator separator character used between volume name and snaphot name in logical volume names.
const lvmSnapshotSeparator = "-"

// lvmEscapedHyphen used to escape hyphens in volume names to avoid conflicts with lvmSnapshotSeparator.
const lvmEscapedHyphen = "--"

var errLVMNotFound = fmt.Errorf("Not found")

// lvmVolumeTypePrefixes maps the different volume types to LVM volume name prefixes.
var lvmVolumeTypePrefixes = map[VolumeType]string{
	VolumeTypeContainer: "containers",
	VolumeTypeVM:        "virtual-machines",
	VolumeTypeImage:     "images",
	VolumeTypeCustom:    "custom",
}

// usesThinpool indicates whether the config specifies to use a thin pool or not.
func (d *lvm) usesThinpool() bool {
	// Default is to use a thinpool.
	if d.config["lvm.use_thinpool"] == "" {
		return true
	}

	return shared.IsTrue(d.config["lvm.use_thinpool"])
}

// thinpoolName returns the thinpool volume to use.
func (d *lvm) thinpoolName() string {
	if d.config["lvm.thinpool_name"] != "" {
		return d.config["lvm.thinpool_name"]
	}

	return "LXDThinPool"
}

// openLoopFile opens a loopback file and disable auto detach.
func (d *lvm) openLoopFile(source string) (*os.File, error) {
	if source == "" {
		return nil, fmt.Errorf("No source property found for the storage pool")
	}

	if filepath.IsAbs(source) && !shared.IsBlockdevPath(source) {
		unlock := locking.Lock(OperationLockName("openLoopFile", d.name, "", "", ""))
		defer unlock()

		// Try to prepare new loop device.
		loopF, err := PrepareLoopDev(source, 0)
		if err != nil {
			return nil, err
		}

		// Make sure that LO_FLAGS_AUTOCLEAR is unset, so that the loopback device will not
		// autodestruct on last close.
		err = UnsetAutoclearOnLoopDev(int(loopF.Fd()))
		if err != nil {
			return nil, err
		}

		return loopF, nil
	}

	return nil, fmt.Errorf("Source is not loop file")
}

// isLVMNotFoundExitError checks whether the supplied error is an exit error from an LVM command
// meaning that the object was not found. Returns true if it is (exit status 5) false if not.
func (d *lvm) isLVMNotFoundExitError(err error) bool {
	runErr, ok := err.(shared.RunError)
	if ok {
		exitError, ok := runErr.Err.(*exec.ExitError)
		if ok {
			if exitError.ExitCode() == 5 {
				return true
			}
		}
	}

	return false
}

// pysicalVolumeExists checks if an LVM Physical Volume exists.
func (d *lvm) pysicalVolumeExists(pvName string) (bool, error) {
	_, err := shared.RunCommand("pvs", "--noheadings", "-o", "pv_name", pvName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil
		}

		return false, errors.Wrapf(err, "Error checking for LVM physical volume %q", pvName)
	}

	return true, nil
}

// volumeGroupExists checks if an LVM Volume Group exists and returns any tags on that volume group.
func (d *lvm) volumeGroupExists(vgName string) (bool, []string, error) {
	output, err := shared.RunCommand("vgs", "--noheadings", "-o", "vg_tags", vgName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil, nil
		}

		return false, nil, errors.Wrapf(err, "Error checking for LVM volume group %q", vgName)
	}

	output = strings.TrimSpace(output)
	tags := strings.SplitN(output, ",", -1)

	return true, tags, nil
}

// volumeGroupExtentSize gets the volume group's physical extent size in bytes.
func (d *lvm) volumeGroupExtentSize(vgName string) (int64, error) {
	output, err := shared.RunCommand("vgs", "--noheadings", "--nosuffix", "--units", "b", "-o", "vg_extent_size", vgName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, errLVMNotFound
		}

		return -1, err
	}

	output = strings.TrimSpace(output)
	return strconv.ParseInt(output, 10, 64)
}

// countLogicalVolumes gets the count of volumes (both normal and thin) in a volume group.
func (d *lvm) countLogicalVolumes(vgName string) (int, error) {
	output, err := shared.RunCommand("vgs", "--noheadings", "-o", "lv_count", vgName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, errLVMNotFound
		}

		return -1, errors.Wrapf(err, "Error counting logical volumes in LVM volume group %q", vgName)
	}

	output = strings.TrimSpace(output)
	return strconv.Atoi(output)
}

// countThinVolumes gets the count of thin volumes in a thin pool.
func (d *lvm) countThinVolumes(vgName, poolName string) (int, error) {
	output, err := shared.RunCommand("lvs", "--noheadings", "-o", "thin_count", fmt.Sprintf("%s/%s", vgName, poolName))
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, errLVMNotFound
		}

		return -1, errors.Wrapf(err, "Error counting thin volumes in LVM volume group %q", vgName)
	}

	output = strings.TrimSpace(output)
	return strconv.Atoi(output)
}

// thinpoolExists checks whether the specified thinpool exists in a volume group.
func (d *lvm) thinpoolExists(vgName string, poolName string) (bool, error) {
	output, err := shared.RunCommand("lvs", "--noheadings", "-o", "lv_attr", fmt.Sprintf("%s/%s", vgName, poolName))
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil
		}

		return false, errors.Wrapf(err, "Error checking for LVM thin pool %q", poolName)
	}

	// Found LV named poolname, check type:
	attrs := strings.TrimSpace(string(output[:]))
	if strings.HasPrefix(attrs, "t") {
		return true, nil
	}

	return false, fmt.Errorf("LVM volume named %q exists but is not a thin pool", poolName)
}

// logicalVolumeExists checks whether the specified logical volume exists.
func (d *lvm) logicalVolumeExists(volDevPath string) (bool, error) {
	_, err := shared.RunCommand("lvs", "--noheadings", "-o", "lv_name", volDevPath)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil
		}

		return false, errors.Wrapf(err, "Error checking for LVM logical volume %q", volDevPath)
	}

	return true, nil
}

// createDefaultThinPool creates the default thinpool as 100% the size of the volume group with a 1G
// meta data volume.
func (d *lvm) createDefaultThinPool(lvmVersion, vgName, thinPoolName string) error {
	isRecent, err := d.lvmVersionIsAtLeast(lvmVersion, "2.02.99")
	if err != nil {
		return errors.Wrapf(err, "Error checking LVM version")
	}

	lvmThinPool := fmt.Sprintf("%s/%s", vgName, thinPoolName)

	args := []string{
		"--yes",
		"--wipesignatures", "y",
		"--poolmetadatasize", "1G",
		"--thinpool", lvmThinPool,
	}

	if isRecent {
		args = append(args, "--extents", "100%FREE")
	} else {
		args = append(args, "--size", "1G")
	}

	// Because the thin pool is created as an LVM volume, if the volume stripes option is set we need to apply
	// it to the thin pool volume, as it cannot be applied to the thin volumes themselves.
	if d.config["volume.lvm.stripes"] != "" {
		args = append(args, "--stripes", d.config["volume.lvm.stripes"])

		if d.config["volume.lvm.stripes.size"] != "" {
			stripSizeBytes, err := d.roundedSizeBytesString(d.config["volume.lvm.stripes.size"])
			if err != nil {
				return errors.Wrapf(err, "Invalid volume stripe size %q", d.config["volume.lvm.stripes.size"])
			}

			args = append(args, "--stripesize", fmt.Sprintf("%db", stripSizeBytes))
		}
	}

	// Create the thin pool volume.
	_, err = shared.TryRunCommand("lvcreate", args...)
	if err != nil {
		return errors.Wrapf(err, "Error creating LVM thin pool named %q", thinPoolName)
	}

	if !isRecent {
		// Grow it to the maximum VG size (two step process required by old LVM).
		_, err = shared.TryRunCommand("lvextend", "--alloc", "anywhere", "-l", "100%FREE", lvmThinPool)
		if err != nil {
			return errors.Wrapf(err, "Error growing LVM thin pool named %q", thinPoolName)
		}
	}

	return nil
}

// lvmVersionIsAtLeast checks whether the installed version of LVM is at least the specific version.
func (d *lvm) lvmVersionIsAtLeast(sTypeVersion string, versionString string) (bool, error) {
	lvmVersionString := strings.Split(sTypeVersion, "/")[0]

	lvmVersion, err := version.Parse(lvmVersionString)
	if err != nil {
		return false, err
	}

	inVersion, err := version.Parse(versionString)
	if err != nil {
		return false, err
	}

	if lvmVersion.Compare(inVersion) < 0 {
		return false, nil
	}

	return true, nil
}

// roundedSizeString rounds the size to the nearest multiple of 512 bytes as the LVM tools require this.
func (d *lvm) roundedSizeBytesString(size string) (int64, error) {
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return 0, err
	}

	// LVM tools require sizes in multiples of 512 bytes.
	const minSizeBytes = 512
	if sizeBytes < minSizeBytes {
		sizeBytes = minSizeBytes
	}

	// Round the size to closest minSizeBytes bytes.
	sizeBytes = int64(sizeBytes/minSizeBytes) * minSizeBytes

	return sizeBytes, nil
}

// createLogicalVolume creates a logical volume.
func (d *lvm) createLogicalVolume(vgName, thinPoolName string, vol Volume, makeThinLv bool) error {
	var err error

	lvSizeBytes, err := d.roundedSizeBytesString(vol.ConfigSize())
	if err != nil {
		return err
	}

	lvFullName := d.lvmFullVolumeName(vol.volType, vol.contentType, vol.name)

	args := []string{
		"--name", lvFullName,
		"--yes",
		"--wipesignatures", "y",
	}

	if makeThinLv {
		targetVg := fmt.Sprintf("%s/%s", vgName, thinPoolName)
		args = append(args,
			"--thin",
			"--virtualsize", fmt.Sprintf("%db", lvSizeBytes),
			targetVg,
		)
	} else {
		args = append(args,
			"--size", fmt.Sprintf("%db", lvSizeBytes),
			vgName,
		)

		// As we are creating a normal logical volume we can apply stripes settings if specified.
		stripes := vol.ExpandedConfig("lvm.stripes")
		if stripes != "" {
			args = append(args, "--stripes", stripes)

			stripeSize := vol.ExpandedConfig("lvm.stripes.size")
			if stripeSize != "" {
				stripSizeBytes, err := d.roundedSizeBytesString(stripeSize)
				if err != nil {
					return errors.Wrapf(err, "Invalid volume stripe size %q", stripeSize)
				}

				args = append(args, "--stripesize", fmt.Sprintf("%db", stripSizeBytes))
			}
		}
	}

	_, err = shared.TryRunCommand("lvcreate", args...)
	if err != nil {
		return errors.Wrapf(err, "Error creating LVM logical volume %q", lvFullName)
	}

	volDevPath := d.lvmDevPath(vgName, vol.volType, vol.contentType, vol.name)

	if vol.contentType == ContentTypeFS {
		_, err = makeFSType(volDevPath, vol.ConfigBlockFilesystem(), nil)
		if err != nil {
			return errors.Wrapf(err, "Error making filesystem on LVM logical volume")
		}
	}

	isRecent, err := d.lvmVersionIsAtLeast(lvmVersion, "2.02.99")
	if err != nil {
		return errors.Wrapf(err, "Error checking LVM version")
	}

	if isRecent {
		// Disable auto activation of volume on LVM versions that support it.
		// Must be done after volume create so that zeroing and signature wiping can take place.
		_, err := shared.RunCommand("lvchange", "--setactivationskip", "y", volDevPath)
		if err != nil {
			return errors.Wrapf(err, "Failed to set activation skip on LVM logical volume %q", volDevPath)
		}
	}

	d.logger.Debug("Logical volume created", log.Ctx{"vg_name": vgName, "lv_name": lvFullName, "size": fmt.Sprintf("%db", lvSizeBytes), "fs": vol.ConfigBlockFilesystem()})
	return nil
}

// createLogicalVolumeSnapshot creates a snapshot of a logical volume.
func (d *lvm) createLogicalVolumeSnapshot(vgName string, srcVol Volume, snapVol Volume, readonly bool, makeThinLv bool) (string, error) {
	srcVolDevPath := d.lvmDevPath(vgName, srcVol.volType, srcVol.contentType, srcVol.name)
	isRecent, err := d.lvmVersionIsAtLeast(lvmVersion, "2.02.99")
	if err != nil {
		return "", errors.Wrapf(err, "Error checking LVM version")
	}

	snapLvName := d.lvmFullVolumeName(snapVol.volType, snapVol.contentType, snapVol.name)
	logCtx := log.Ctx{"vg_name": vgName, "lv_name": snapLvName, "src_dev": srcVolDevPath, "thin": makeThinLv}
	args := []string{"-n", snapLvName, "-s", srcVolDevPath}

	if isRecent {
		args = append(args, "--setactivationskip", "y")
	}

	// If the source is not a thin volume the size needs to be specified.
	// According to LVM tools 15-20% of the original volume should be sufficient.
	// However, let's not be stingy at first otherwise we might force users to fiddle around with lvextend.
	if !makeThinLv {
		lvSizeBytes, err := d.roundedSizeBytesString(snapVol.ConfigSize())
		if err != nil {
			return "", err
		}

		args = append(args, "--size", fmt.Sprintf("%db", lvSizeBytes))
		logCtx["size"] = fmt.Sprintf("%db", lvSizeBytes)
	}

	if readonly {
		args = append(args, "-pr")
	} else {
		args = append(args, "-prw")
	}

	revert := revert.New()
	defer revert.Fail()

	_, err = shared.TryRunCommand("lvcreate", args...)
	if err != nil {
		return "", err
	}
	d.logger.Debug("Logical volume snapshot created", logCtx)

	revert.Add(func() {
		d.removeLogicalVolume(d.lvmDevPath(vgName, snapVol.volType, snapVol.contentType, snapVol.name))
	})

	targetVolDevPath := d.lvmDevPath(vgName, snapVol.volType, snapVol.contentType, snapVol.name)

	revert.Success()
	return targetVolDevPath, nil
}

// removeLogicalVolume removes a logical volume.
func (d *lvm) removeLogicalVolume(volDevPath string) error {
	_, err := shared.TryRunCommand("lvremove", "-f", volDevPath)
	if err != nil {
		return err
	}
	d.logger.Debug("Logical volume removed", log.Ctx{"dev": volDevPath})

	return nil
}

// renameLogicalVolume renames a logical volume.
func (d *lvm) renameLogicalVolume(volDevPath string, newVolDevPath string) error {
	_, err := shared.TryRunCommand("lvrename", volDevPath, newVolDevPath)
	if err != nil {
		return err
	}
	d.logger.Debug("Logical volume renamed", log.Ctx{"dev": volDevPath, "new_dev": newVolDevPath})

	return nil
}

// lvmFullVolumeName returns the logical volume's full name with volume type prefix. It also converts the supplied
// volName to a name suitable for use as a logical volume using volNameToLVName(). If an empty volType is passed
// then just the volName is returned. If an invalid volType is passed then an empty string is returned.
// If a content type of ContentTypeBlock is supplied then the volume name is suffixed with lvmBlockVolSuffix.
func (d *lvm) lvmFullVolumeName(volType VolumeType, contentType ContentType, volName string) string {
	if volType == "" {
		return volName
	}

	volTypePrefix := lvmVolumeTypePrefixes[volType]

	// Invalid volume type supplied.
	if volTypePrefix == "" {
		return ""
	}

	contentTypeSuffix := ""
	if contentType == ContentTypeBlock {
		contentTypeSuffix = lvmBlockVolSuffix
	}

	// Escape the volume name to a name suitable for using as a logical volume.
	lvName := strings.Replace(strings.Replace(volName, "-", lvmEscapedHyphen, -1), shared.SnapshotDelimiter, lvmSnapshotSeparator, -1)

	return fmt.Sprintf("%s_%s%s", volTypePrefix, lvName, contentTypeSuffix)
}

// lvmDevPath returns the path to the LVM volume device. Empty string is returned if invalid volType supplied.
func (d *lvm) lvmDevPath(vgName string, volType VolumeType, contentType ContentType, volName string) string {
	fullVolName := d.lvmFullVolumeName(volType, contentType, volName)
	if fullVolName == "" {
		return "" // Invalid volType supplied.
	}

	return fmt.Sprintf("/dev/%s/%s", vgName, fullVolName)
}

// resizeLogicalVolume resizes an LVM logical volume. This function does not resize any filesystem inside the LV.
func (d *lvm) resizeLogicalVolume(lvPath string, sizeBytes int64) error {
	_, err := shared.TryRunCommand("lvresize", "-L", fmt.Sprintf("%db", sizeBytes), "-f", lvPath)
	if err != nil {
		return err
	}

	d.logger.Debug("Logical volume resized", log.Ctx{"dev": lvPath, "size": fmt.Sprintf("%db", sizeBytes)})
	return nil
}

// copyThinpoolVolume makes an optimised copy of a thinpool volume by using thinpool snapshots.
func (d *lvm) copyThinpoolVolume(vol, srcVol Volume, srcSnapshots []Volume, refresh bool) error {
	revert := revert.New()
	defer revert.Fail()

	removeVols := []string{}

	// If copying snapshots is indicated, check the source isn't itself a snapshot.
	if len(srcSnapshots) > 0 && !srcVol.IsSnapshot() {
		// Create the parent snapshot directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}

		for _, srcSnapshot := range srcSnapshots {
			_, snapName, _ := shared.InstanceGetParentAndSnapshotName(srcSnapshot.name)
			newFullSnapName := GetSnapshotVolumeName(vol.name, snapName)
			newSnapVol := NewVolume(d, d.Name(), vol.volType, vol.contentType, newFullSnapName, vol.config, vol.poolConfig)

			if d.HasVolume(newSnapVol) {
				return fmt.Errorf("LVM snapshot volume already exists %q", newSnapVol.name)
			}

			newSnapVolPath := newSnapVol.MountPath()
			err := newSnapVol.EnsureMountPath()
			if err != nil {
				return err
			}

			revert.Add(func() { os.RemoveAll(newSnapVolPath) })

			// We do not modify the original snapshot so as to avoid damaging if it is corrupted for
			// some reason. If the filesystem needs to have a unique UUID generated in order to mount
			// this will be done at restore time to be safe.
			_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], srcSnapshot, newSnapVol, true, d.usesThinpool())
			if err != nil {
				return errors.Wrapf(err, "Error creating LVM logical volume snapshot")
			}

			revert.Add(func() {
				d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], newSnapVol.volType, newSnapVol.contentType, newSnapVol.name))
			})
		}
	}

	// Handle copying the main volume.
	if d.HasVolume(vol) {
		if refresh {
			newVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
			tmpVolName := fmt.Sprintf("%s%s", vol.name, tmpVolSuffix)
			tmpVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, tmpVolName)

			// Rename existing volume to temporary new name so we can revert if needed.
			err := d.renameLogicalVolume(newVolDevPath, tmpVolDevPath)
			if err != nil {
				return errors.Wrapf(err, "Error temporarily renaming original LVM logical volume")
			}

			// Record this volume to be removed at the very end.
			removeVols = append(removeVols, tmpVolName)

			revert.Add(func() {
				// Rename the original volume back to the original name.
				d.renameLogicalVolume(tmpVolDevPath, newVolDevPath)
			})
		} else {
			return fmt.Errorf("LVM volume already exists %q", vol.name)
		}
	} else {
		volPath := vol.MountPath()
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}

		revert.Add(func() { os.RemoveAll(volPath) })
	}

	// Create snapshot of source volume as new volume.
	_, err := d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], srcVol, vol, false, d.usesThinpool())
	if err != nil {
		return errors.Wrapf(err, "Error creating LVM logical volume snapshot")
	}

	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)

	revert.Add(func() {
		d.removeLogicalVolume(volDevPath)
	})

	if vol.contentType == ContentTypeFS {
		// Generate a new filesystem UUID if needed (this is required because some filesystems won't allow
		// volumes with the same UUID to be mounted at the same time). This should be done before volume
		// resize as some filesystems will need to mount the filesystem to resize.
		if renegerateFilesystemUUIDNeeded(vol.ConfigBlockFilesystem()) {
			_, err = d.activateVolume(volDevPath)
			if err != nil {
				return err
			}

			d.logger.Debug("Regenerating filesystem UUID", log.Ctx{"dev": volDevPath, "fs": vol.ConfigBlockFilesystem()})
			err = regenerateFilesystemUUID(vol.ConfigBlockFilesystem(), volDevPath)
			if err != nil {
				return err
			}
		}

		// Mount the volume and ensure the permissions are set correctly inside the mounted volume.
		err = vol.MountTask(func(_ string, _ *operations.Operation) error {
			return vol.EnsureMountPath()
		}, nil)
		if err != nil {
			return err
		}
	}

	// Resize volume to the size specified. Only uses volume "size" property and does not use pool/defaults
	// to give the caller more control over the size being used.
	err = d.SetVolumeQuota(vol, vol.config["size"], false, nil)
	if err != nil {
		return err
	}

	// Finally clean up original volumes left that were renamed with a tmpVolSuffix suffix.
	for _, removeVolName := range removeVols {
		err := d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, removeVolName))
		if err != nil {
			return errors.Wrapf(err, "Error removing LVM volume %q", vol.name)
		}
	}

	revert.Success()
	return nil
}

// logicalVolumeSize gets the size in bytes of a logical volume.
func (d *lvm) logicalVolumeSize(volDevPath string) (int64, error) {
	output, err := shared.RunCommand("lvs", "--noheadings", "--nosuffix", "--units", "b", "-o", "lv_size", volDevPath)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, errLVMNotFound
		}

		return -1, errors.Wrapf(err, "Error getting size of LVM volume %q", volDevPath)
	}

	output = strings.TrimSpace(output)
	return strconv.ParseInt(output, 10, 64)
}

func (d *lvm) thinPoolVolumeUsage(volDevPath string) (uint64, uint64, error) {
	args := []string{
		volDevPath,
		"--noheadings",
		"--units", "b",
		"--nosuffix",
		"--separator", ",",
		"-o", "lv_size,data_percent,metadata_percent",
	}

	out, err := shared.RunCommand("lvs", args...)
	if err != nil {
		return 0, 0, err
	}

	parts := strings.Split(strings.TrimSpace(out), ",")
	if len(parts) < 3 {
		return 0, 0, fmt.Errorf("Unexpected output from lvs command")
	}

	total, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}

	totalSize := total

	dataPerc, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, 0, err
	}

	metaPerc := float64(0)

	// For thin volumes there is no meta data percentage. This is only for the thin pool volume itself.
	if parts[2] != "" {
		metaPerc, err = strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return 0, 0, err
		}
	}

	usedSize := uint64(float64(total) * ((dataPerc + metaPerc) / 100))

	return totalSize, usedSize, nil
}

// parseLogicalVolumeSnapshot parses a raw logical volume name (from lvs command) and checks whether it is a
// snapshot of the supplied parent volume. Returns unescaped parsed snapshot name if snapshot volume recognised,
// empty string if not. The parent is required due to limitations in the naming scheme that LXD has historically
// been used for naming logical volumes meaning that additional context of the parent is required to accurately
// recognise snapshot volumes that belong to the parent.
func (d *lvm) parseLogicalVolumeSnapshot(parent Volume, lvmVolName string) string {
	fullVolName := d.lvmFullVolumeName(parent.volType, parent.contentType, parent.name)

	// If block volume, remove the block suffix ready for comparison with LV list.
	if parent.IsVMBlock() || parent.volType == VolumeTypeCustom && parent.contentType == ContentTypeBlock {
		if !strings.HasSuffix(lvmVolName, lvmBlockVolSuffix) {
			return ""
		}

		// Remove the block suffix so that snapshot names can be compared and extracted without the suffix.
		fullVolName = strings.TrimSuffix(fullVolName, lvmBlockVolSuffix)
		lvmVolName = strings.TrimSuffix(lvmVolName, lvmBlockVolSuffix)
	}

	// Prefix we would expect for a snapshot of the parent volume.
	snapPrefix := fmt.Sprintf("%s%s", fullVolName, lvmSnapshotSeparator)

	// Prefix used when escaping "-" in volume names. Doesn't indicate a snapshot of parent.
	badPrefix := fmt.Sprintf("%s%s", fullVolName, lvmEscapedHyphen)

	// Check the volume matches the snapshot prefix, but doesn't match the prefix that indicates a similarly
	// named volume that just has escaped "-" characters in it.
	if strings.HasPrefix(lvmVolName, snapPrefix) && !strings.HasPrefix(lvmVolName, badPrefix) {
		// Remove volume name prefix (including snapshot delimiter) and unescape snapshot name.
		return strings.Replace(strings.TrimPrefix(lvmVolName, snapPrefix), lvmEscapedHyphen, "-", -1)
	}

	return ""
}

// activateVolume activates an LVM logical volume if not already present. Returns true if activated, false if not.
func (d *lvm) activateVolume(volDevPath string) (bool, error) {
	if !shared.PathExists(volDevPath) {
		_, err := shared.RunCommand("lvchange", "--activate", "y", "--ignoreactivationskip", volDevPath)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to activate LVM logical volume %q", volDevPath)
		}
		d.logger.Debug("Activated logical volume", log.Ctx{"dev": volDevPath})
		return true, nil
	}

	return false, nil
}

// deactivateVolume deactivates an LVM logical volume if present. Returns true if deactivated, false if not.
func (d *lvm) deactivateVolume(volDevPath string) (bool, error) {
	if shared.PathExists(volDevPath) {
		_, err := shared.RunCommand("lvchange", "--activate", "n", "--ignoreactivationskip", volDevPath)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to deactivate LVM logical volume %q", volDevPath)
		}
		d.logger.Debug("Deactivated logical volume", log.Ctx{"dev": volDevPath})
		return true, nil
	}

	return false, nil
}
