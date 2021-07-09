package drivers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flosch/pongo2"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	liblxc "gopkg.in/lxc/go-lxc.v2"
	yaml "gopkg.in/yaml.v2"

	"github.com/lxc/lxd/lxd/apparmor"
	"github.com/lxc/lxd/lxd/cgroup"
	"github.com/lxc/lxd/lxd/daemon"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/device"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/device/nictype"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/instance/operationlock"
	"github.com/lxc/lxd/lxd/lifecycle"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/seccomp"
	"github.com/lxc/lxd/lxd/state"
	storagePools "github.com/lxc/lxd/lxd/storage"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/lxd/template"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/idmap"
	"github.com/lxc/lxd/shared/instancewriter"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
	"github.com/lxc/lxd/shared/netutils"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/units"
)

// Helper functions
func lxcSetConfigItem(c *liblxc.Container, key string, value string) error {
	if c == nil {
		return fmt.Errorf("Uninitialized go-lxc struct")
	}

	if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
		switch key {
		case "lxc.uts.name":
			key = "lxc.utsname"
		case "lxc.pty.max":
			key = "lxc.pts"
		case "lxc.tty.dir":
			key = "lxc.devttydir"
		case "lxc.tty.max":
			key = "lxc.tty"
		case "lxc.apparmor.profile":
			key = "lxc.aa_profile"
		case "lxc.apparmor.allow_incomplete":
			key = "lxc.aa_allow_incomplete"
		case "lxc.selinux.context":
			key = "lxc.se_context"
		case "lxc.mount.fstab":
			key = "lxc.mount"
		case "lxc.console.path":
			key = "lxc.console"
		case "lxc.seccomp.profile":
			key = "lxc.seccomp"
		case "lxc.signal.halt":
			key = "lxc.haltsignal"
		case "lxc.signal.reboot":
			key = "lxc.rebootsignal"
		case "lxc.signal.stop":
			key = "lxc.stopsignal"
		case "lxc.log.syslog":
			key = "lxc.syslog"
		case "lxc.log.level":
			key = "lxc.loglevel"
		case "lxc.log.file":
			key = "lxc.logfile"
		case "lxc.init.cmd":
			key = "lxc.init_cmd"
		case "lxc.init.uid":
			key = "lxc.init_uid"
		case "lxc.init.gid":
			key = "lxc.init_gid"
		case "lxc.idmap":
			key = "lxc.id_map"
		}
	}

	if strings.HasPrefix(key, "lxc.prlimit.") {
		if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
			return fmt.Errorf(`Process limits require liblxc >= 2.1`)
		}
	}

	err := c.SetConfigItem(key, value)
	if err != nil {
		return fmt.Errorf("Failed to set LXC config: %s=%s", key, value)
	}

	return nil
}

func lxcStatusCode(state liblxc.State) api.StatusCode {
	return map[int]api.StatusCode{
		1: api.Stopped,
		2: api.Starting,
		3: api.Running,
		4: api.Stopping,
		5: api.Aborting,
		6: api.Freezing,
		7: api.Frozen,
		8: api.Thawed,
		9: api.Error,
	}[int(state)]
}

// lxcCreate creates the DB storage records and sets up instance devices.
// Accepts a reverter that revert steps this function does will be added to. It is up to the caller to call the
// revert's Fail() or Success() function as needed.
func lxcCreate(s *state.State, args db.InstanceArgs, revert *revert.Reverter) (instance.Instance, error) {
	// Create the container struct
	d := &lxc{
		common: common{
			state: s,

			architecture: args.Architecture,
			creationDate: args.CreationDate,
			dbType:       args.Type,
			description:  args.Description,
			ephemeral:    args.Ephemeral,
			expiryDate:   args.ExpiryDate,
			id:           args.ID,
			lastUsedDate: args.LastUsedDate,
			localConfig:  args.Config,
			localDevices: args.Devices,
			logger:       logging.AddContext(logger.Log, log.Ctx{"instanceType": args.Type, "instance": args.Name, "project": args.Project}),
			name:         args.Name,
			node:         args.Node,
			profiles:     args.Profiles,
			project:      args.Project,
			snapshot:     args.Snapshot,
			stateful:     args.Stateful,
		},
	}

	// Cleanup the zero values
	if d.expiryDate.IsZero() {
		d.expiryDate = time.Time{}
	}

	if d.creationDate.IsZero() {
		d.creationDate = time.Time{}
	}

	if d.lastUsedDate.IsZero() {
		d.lastUsedDate = time.Time{}
	}

	d.logger.Info("Creating container", log.Ctx{"ephemeral": d.ephemeral})

	// Load the config.
	err := d.init()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to expand config")
	}

	// Validate expanded config.
	err = instance.ValidConfig(s.OS, d.expandedConfig, false, instancetype.Container)
	if err != nil {
		return nil, errors.Wrap(err, "Invalid config")
	}

	err = instance.ValidDevices(s, s.Cluster, d.Project(), d.Type(), d.expandedDevices, true)
	if err != nil {
		return nil, errors.Wrap(err, "Invalid devices")
	}

	// Retrieve the container's storage pool.
	var storageInstance instance.Instance
	if d.IsSnapshot() {
		parentName, _, _ := shared.InstanceGetParentAndSnapshotName(d.name)

		// Load the parent.
		storageInstance, err = instance.LoadByProjectAndName(d.state, d.project, parentName)
		if err != nil {
			return nil, errors.Wrap(err, "Invalid parent")
		}
	} else {
		storageInstance = d
	}

	_, rootDiskDevice, err := shared.GetRootDiskDevice(storageInstance.ExpandedDevices().CloneNative())
	if err != nil {
		return nil, err
	}

	if rootDiskDevice["pool"] == "" {
		return nil, fmt.Errorf("The container's root device is missing the pool property")
	}

	// Initialize the storage pool.
	d.storagePool, err = storagePools.GetPoolByName(d.state, rootDiskDevice["pool"])
	if err != nil {
		return nil, errors.Wrapf(err, "Failed loading storage pool")
	}

	volType, err := storagePools.InstanceTypeToVolumeType(d.Type())
	if err != nil {
		return nil, err
	}

	storagePoolSupported := false
	for _, supportedType := range d.storagePool.Driver().Info().VolumeTypes {
		if supportedType == volType {
			storagePoolSupported = true
			break
		}
	}

	if !storagePoolSupported {
		return nil, fmt.Errorf("Storage pool does not support instance type")
	}

	// Create a new storage volume database entry for the container's storage volume.
	if d.IsSnapshot() {
		// Copy volume config from parent.
		parentName, _, _ := shared.InstanceGetParentAndSnapshotName(args.Name)
		_, parentVol, err := s.Cluster.GetLocalStoragePoolVolume(args.Project, parentName, db.StoragePoolVolumeTypeContainer, d.storagePool.ID())
		if err != nil {
			return nil, errors.Wrapf(err, "Failed loading source volume for snapshot")
		}

		_, err = s.Cluster.CreateStorageVolumeSnapshot(args.Project, args.Name, "", db.StoragePoolVolumeTypeContainer, d.storagePool.ID(), parentVol.Config, time.Time{})
		if err != nil {
			return nil, errors.Wrapf(err, "Failed creating storage record for snapshot")
		}
	} else {
		// Fill default config for new instances.
		volumeConfig := map[string]string{}
		err = d.storagePool.FillInstanceConfig(d, volumeConfig)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed filling default config")
		}

		_, err = s.Cluster.CreateStoragePoolVolume(args.Project, args.Name, "", db.StoragePoolVolumeTypeContainer, d.storagePool.ID(), volumeConfig, db.StoragePoolVolumeContentTypeFS)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed creating storage record")
		}
	}

	revert.Add(func() {
		s.Cluster.RemoveStoragePoolVolume(args.Project, args.Name, db.StoragePoolVolumeTypeContainer, d.storagePool.ID())
	})

	// Setup initial idmap config
	var idmap *idmap.IdmapSet
	base := int64(0)
	if !d.IsPrivileged() {
		idmap, base, err = findIdmap(
			s,
			args.Name,
			d.expandedConfig["security.idmap.isolated"],
			d.expandedConfig["security.idmap.base"],
			d.expandedConfig["security.idmap.size"],
			d.expandedConfig["raw.idmap"],
		)

		if err != nil {
			return nil, err
		}
	}

	var jsonIdmap string
	if idmap != nil {
		idmapBytes, err := json.Marshal(idmap.Idmap)
		if err != nil {
			return nil, err
		}
		jsonIdmap = string(idmapBytes)
	} else {
		jsonIdmap = "[]"
	}

	err = d.VolatileSet(map[string]string{"volatile.idmap.next": jsonIdmap})
	if err != nil {
		return nil, err
	}

	err = d.VolatileSet(map[string]string{"volatile.idmap.base": fmt.Sprintf("%v", base)})
	if err != nil {
		return nil, err
	}

	// Invalid idmap cache.
	d.idmapset = nil

	// Set last_state if not currently set.
	if d.localConfig["volatile.last_state.idmap"] == "" {
		err = d.VolatileSet(map[string]string{"volatile.last_state.idmap": "[]"})
		if err != nil {
			return nil, err
		}
	}

	// Re-run init to update the idmap.
	err = d.init()
	if err != nil {
		return nil, err
	}

	if !d.IsSnapshot() {
		// Add devices to container.
		for k, m := range d.expandedDevices {
			devName := k
			devConfig := m
			err = d.deviceAdd(devName, devConfig, false)
			if err != nil && err != device.ErrUnsupportedDevType {
				return nil, errors.Wrapf(err, "Failed to add device %q", devName)
			}

			revert.Add(func() { d.deviceRemove(devName, devConfig, false) })
		}

		// Update MAAS (must run after the MAC addresses have been generated).
		err = d.maasUpdate(d, nil)
		if err != nil {
			return nil, err
		}

		revert.Add(func() { d.maasDelete(d) })
	}

	d.logger.Info("Created container", log.Ctx{"ephemeral": d.ephemeral})
	if d.snapshot {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotCreated.Event(d, nil))
	} else {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceCreated.Event(d, nil))
	}

	return d, nil
}

func lxcLoad(s *state.State, args db.InstanceArgs, profiles []api.Profile) (instance.Instance, error) {
	// Create the container struct
	d := lxcInstantiate(s, args, nil)

	// Setup finalizer
	runtime.SetFinalizer(d, lxcUnload)

	// Expand config and devices
	err := d.(*lxc).expandConfig(profiles)
	if err != nil {
		return nil, err
	}

	err = d.(*lxc).expandDevices(profiles)
	if err != nil {
		return nil, err
	}

	return d, nil
}

// Unload is called by the garbage collector
func lxcUnload(d *lxc) {
	runtime.SetFinalizer(d, nil)
	if d.c != nil {
		d.c.Release()
		d.c = nil
	}
}

// Create a container struct without initializing it.
func lxcInstantiate(s *state.State, args db.InstanceArgs, expandedDevices deviceConfig.Devices) instance.Instance {
	d := &lxc{
		common: common{
			state: s,

			architecture: args.Architecture,
			creationDate: args.CreationDate,
			dbType:       args.Type,
			description:  args.Description,
			ephemeral:    args.Ephemeral,
			expiryDate:   args.ExpiryDate,
			id:           args.ID,
			lastUsedDate: args.LastUsedDate,
			localConfig:  args.Config,
			localDevices: args.Devices,
			logger:       logging.AddContext(logger.Log, log.Ctx{"instanceType": args.Type, "instance": args.Name, "project": args.Project}),
			name:         args.Name,
			node:         args.Node,
			profiles:     args.Profiles,
			project:      args.Project,
			snapshot:     args.Snapshot,
			stateful:     args.Stateful,
		},
	}

	// Cleanup the zero values
	if d.expiryDate.IsZero() {
		d.expiryDate = time.Time{}
	}

	if d.creationDate.IsZero() {
		d.creationDate = time.Time{}
	}

	if d.lastUsedDate.IsZero() {
		d.lastUsedDate = time.Time{}
	}

	// This is passed during expanded config validation.
	if expandedDevices != nil {
		d.expandedDevices = expandedDevices
	}

	return d
}

// The LXC container driver.
type lxc struct {
	common

	// Config handling.
	fromHook bool

	// Cached handles.
	// Do not use these variables directly, instead use their associated get functions so they
	// will be initialised on demand.
	c           *liblxc.Container
	cConfig     bool
	idmapset    *idmap.IdmapSet
	storagePool storagePools.Pool
}

func idmapSize(state *state.State, isolatedStr string, size string) (int64, error) {
	isolated := false
	if shared.IsTrue(isolatedStr) {
		isolated = true
	}

	var idMapSize int64
	if size == "" || size == "auto" {
		if isolated {
			idMapSize = 65536
		} else {
			if len(state.OS.IdmapSet.Idmap) != 2 {
				return 0, fmt.Errorf("bad initial idmap: %v", state.OS.IdmapSet)
			}

			idMapSize = state.OS.IdmapSet.Idmap[0].Maprange
		}
	} else {
		size, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return 0, err
		}

		idMapSize = size
	}

	return idMapSize, nil
}

var idmapLock sync.Mutex

func findIdmap(state *state.State, cName string, isolatedStr string, configBase string, configSize string, rawIdmap string) (*idmap.IdmapSet, int64, error) {
	isolated := false
	if shared.IsTrue(isolatedStr) {
		isolated = true
	}

	rawMaps, err := instance.ParseRawIdmap(rawIdmap)
	if err != nil {
		return nil, 0, err
	}

	if !isolated {
		newIdmapset := idmap.IdmapSet{Idmap: make([]idmap.IdmapEntry, len(state.OS.IdmapSet.Idmap))}
		copy(newIdmapset.Idmap, state.OS.IdmapSet.Idmap)

		for _, ent := range rawMaps {
			err := newIdmapset.AddSafe(ent)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, 0, err
			}
		}

		return &newIdmapset, 0, nil
	}

	size, err := idmapSize(state, isolatedStr, configSize)
	if err != nil {
		return nil, 0, err
	}

	mkIdmap := func(offset int64, size int64) (*idmap.IdmapSet, error) {
		set := &idmap.IdmapSet{Idmap: []idmap.IdmapEntry{
			{Isuid: true, Nsid: 0, Hostid: offset, Maprange: size},
			{Isgid: true, Nsid: 0, Hostid: offset, Maprange: size},
		}}

		for _, ent := range rawMaps {
			err := set.AddSafe(ent)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, err
			}
		}

		return set, nil
	}

	if configBase != "" {
		offset, err := strconv.ParseInt(configBase, 10, 64)
		if err != nil {
			return nil, 0, err
		}

		set, err := mkIdmap(offset, size)
		if err != nil && err == idmap.ErrHostIdIsSubId {
			return nil, 0, err
		}

		return set, offset, nil
	}

	idmapLock.Lock()
	defer idmapLock.Unlock()

	cts, err := instance.LoadNodeAll(state, instancetype.Container)
	if err != nil {
		return nil, 0, err
	}

	offset := state.OS.IdmapSet.Idmap[0].Hostid + 65536

	mapentries := idmap.ByHostid{}
	for _, container := range cts {
		if container.Type() != instancetype.Container {
			continue
		}

		name := container.Name()

		/* Don't change our map Just Because. */
		if name == cName {
			continue
		}

		if container.IsPrivileged() {
			continue
		}

		if !shared.IsTrue(container.ExpandedConfig()["security.idmap.isolated"]) {
			continue
		}

		cBase := int64(0)
		if container.ExpandedConfig()["volatile.idmap.base"] != "" {
			cBase, err = strconv.ParseInt(container.ExpandedConfig()["volatile.idmap.base"], 10, 64)
			if err != nil {
				return nil, 0, err
			}
		}

		cSize, err := idmapSize(state, container.ExpandedConfig()["security.idmap.isolated"], container.ExpandedConfig()["security.idmap.size"])
		if err != nil {
			return nil, 0, err
		}

		mapentries = append(mapentries, &idmap.IdmapEntry{Hostid: int64(cBase), Maprange: cSize})
	}

	sort.Sort(mapentries)

	for i := range mapentries {
		if i == 0 {
			if mapentries[0].Hostid < offset+size {
				offset = mapentries[0].Hostid + mapentries[0].Maprange
				continue
			}

			set, err := mkIdmap(offset, size)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, 0, err
			}

			return set, offset, nil
		}

		if mapentries[i-1].Hostid+mapentries[i-1].Maprange > offset {
			offset = mapentries[i-1].Hostid + mapentries[i-1].Maprange
			continue
		}

		offset = mapentries[i-1].Hostid + mapentries[i-1].Maprange
		if offset+size < mapentries[i].Hostid {
			set, err := mkIdmap(offset, size)
			if err != nil && err == idmap.ErrHostIdIsSubId {
				return nil, 0, err
			}

			return set, offset, nil
		}
		offset = mapentries[i].Hostid + mapentries[i].Maprange
	}

	if offset+size < state.OS.IdmapSet.Idmap[0].Hostid+state.OS.IdmapSet.Idmap[0].Maprange {
		set, err := mkIdmap(offset, size)
		if err != nil && err == idmap.ErrHostIdIsSubId {
			return nil, 0, err
		}

		return set, offset, nil
	}

	return nil, 0, fmt.Errorf("Not enough uid/gid available for the container")
}

func (d *lxc) init() error {
	// Compute the expanded config and device list
	err := d.expandConfig(nil)
	if err != nil {
		return err
	}

	err = d.expandDevices(nil)
	if err != nil {
		return err
	}

	return nil
}

func (d *lxc) initLXC(config bool) error {
	// No need to go through all that for snapshots
	if d.IsSnapshot() {
		return nil
	}

	// Check if being called from a hook
	if d.fromHook {
		return fmt.Errorf("You can't use go-lxc from inside a LXC hook")
	}

	// Check if already initialized
	if d.c != nil {
		if !config || d.cConfig {
			return nil
		}
	}

	// Load the go-lxc struct
	cname := project.Instance(d.Project(), d.Name())
	cc, err := liblxc.NewContainer(cname, d.state.OS.LxcPath)
	if err != nil {
		return err
	}

	// Load cgroup abstraction
	cg, err := d.cgroup(cc)
	if err != nil {
		return err
	}

	freeContainer := true
	defer func() {
		if freeContainer {
			cc.Release()
		}
	}()

	// Setup logging
	logfile := d.LogFilePath()
	err = lxcSetConfigItem(cc, "lxc.log.file", logfile)
	if err != nil {
		return err
	}

	logLevel := "warn"
	if daemon.Debug {
		logLevel = "trace"
	} else if daemon.Verbose {
		logLevel = "info"
	}

	err = lxcSetConfigItem(cc, "lxc.log.level", logLevel)
	if err != nil {
		return err
	}

	if util.RuntimeLiblxcVersionAtLeast(3, 0, 0) {
		// Default size log buffer
		err = lxcSetConfigItem(cc, "lxc.console.buffer.size", "auto")
		if err != nil {
			return err
		}

		err = lxcSetConfigItem(cc, "lxc.console.size", "auto")
		if err != nil {
			return err
		}

		// File to dump ringbuffer contents to when requested or
		// container shutdown.
		consoleBufferLogFile := d.ConsoleBufferLogPath()
		err = lxcSetConfigItem(cc, "lxc.console.logfile", consoleBufferLogFile)
		if err != nil {
			return err
		}
	}

	// Allow for lightweight init
	d.cConfig = config
	if !config {
		if d.c != nil {
			d.c.Release()
		}

		d.c = cc
		freeContainer = false
		return nil
	}

	if d.IsPrivileged() {
		// Base config
		toDrop := "sys_time sys_module sys_rawio"
		if !d.state.OS.AppArmorStacking || d.state.OS.AppArmorStacked {
			toDrop = toDrop + " mac_admin mac_override"
		}

		err = lxcSetConfigItem(cc, "lxc.cap.drop", toDrop)
		if err != nil {
			return err
		}
	}

	// Set an appropriate /proc, /sys/ and /sys/fs/cgroup
	mounts := []string{}
	if d.IsPrivileged() && !d.state.OS.RunningInUserNS {
		mounts = append(mounts, "proc:mixed")
		mounts = append(mounts, "sys:mixed")
	} else {
		mounts = append(mounts, "proc:rw")
		mounts = append(mounts, "sys:rw")
	}

	cgInfo := cgroup.GetInfo()
	if cgInfo.Namespacing {
		if cgInfo.Layout == cgroup.CgroupsUnified {
			mounts = append(mounts, "cgroup:rw:force")
		} else {
			mounts = append(mounts, "cgroup:mixed")
		}
	} else {
		mounts = append(mounts, "cgroup:mixed")
	}

	err = lxcSetConfigItem(cc, "lxc.mount.auto", strings.Join(mounts, " "))
	if err != nil {
		return err
	}

	err = lxcSetConfigItem(cc, "lxc.autodev", "1")
	if err != nil {
		return err
	}

	err = lxcSetConfigItem(cc, "lxc.pty.max", "1024")
	if err != nil {
		return err
	}

	bindMounts := []string{
		"/dev/fuse",
		"/dev/net/tun",
		"/proc/sys/fs/binfmt_misc",
		"/sys/firmware/efi/efivars",
		"/sys/fs/fuse/connections",
		"/sys/fs/pstore",
		"/sys/kernel/config",
		"/sys/kernel/debug",
		"/sys/kernel/security",
		"/sys/kernel/tracing",
	}

	if d.IsPrivileged() && !d.state.OS.RunningInUserNS {
		err = lxcSetConfigItem(cc, "lxc.mount.entry", "mqueue dev/mqueue mqueue rw,relatime,create=dir,optional 0 0")
		if err != nil {
			return err
		}
	} else {
		bindMounts = append(bindMounts, "/dev/mqueue")
	}

	for _, mnt := range bindMounts {
		if !shared.PathExists(mnt) {
			continue
		}

		if shared.IsDir(mnt) {
			err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s %s none rbind,create=dir,optional 0 0", mnt, strings.TrimPrefix(mnt, "/")))
			if err != nil {
				return err
			}
		} else {
			err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s %s none bind,create=file,optional 0 0", mnt, strings.TrimPrefix(mnt, "/")))
			if err != nil {
				return err
			}
		}
	}

	// For lxcfs
	templateConfDir := os.Getenv("LXD_LXC_TEMPLATE_CONFIG")
	if templateConfDir == "" {
		templateConfDir = "/usr/share/lxc/config"
	}

	if shared.PathExists(fmt.Sprintf("%s/common.conf.d/", templateConfDir)) {
		err = lxcSetConfigItem(cc, "lxc.include", fmt.Sprintf("%s/common.conf.d/", templateConfDir))
		if err != nil {
			return err
		}
	}

	// Configure devices cgroup
	if d.IsPrivileged() && !d.state.OS.RunningInUserNS && d.state.OS.CGInfo.Supports(cgroup.Devices, cg) {
		if d.state.OS.CGInfo.Layout == cgroup.CgroupsUnified {
			err = lxcSetConfigItem(cc, "lxc.cgroup2.devices.deny", "a")
		} else {
			err = lxcSetConfigItem(cc, "lxc.cgroup.devices.deny", "a")
		}
		if err != nil {
			return err
		}

		devices := []string{
			"b *:* m",      // Allow mknod of block devices
			"c *:* m",      // Allow mknod of char devices
			"c 136:* rwm",  // /dev/pts devices
			"c 1:3 rwm",    // /dev/null
			"c 1:5 rwm",    // /dev/zero
			"c 1:7 rwm",    // /dev/full
			"c 1:8 rwm",    // /dev/random
			"c 1:9 rwm",    // /dev/urandom
			"c 5:0 rwm",    // /dev/tty
			"c 5:1 rwm",    // /dev/console
			"c 5:2 rwm",    // /dev/ptmx
			"c 10:229 rwm", // /dev/fuse
			"c 10:200 rwm", // /dev/net/tun
		}

		for _, dev := range devices {
			if d.state.OS.CGInfo.Layout == cgroup.CgroupsUnified {
				err = lxcSetConfigItem(cc, "lxc.cgroup2.devices.allow", dev)
			} else {
				err = lxcSetConfigItem(cc, "lxc.cgroup.devices.allow", dev)
			}
			if err != nil {
				return err
			}
		}
	}

	if d.IsNesting() {
		/*
		 * mount extra /proc and /sys to work around kernel
		 * restrictions on remounting them when covered
		 */
		err = lxcSetConfigItem(cc, "lxc.mount.entry", "proc dev/.lxc/proc proc create=dir,optional 0 0")
		if err != nil {
			return err
		}

		err = lxcSetConfigItem(cc, "lxc.mount.entry", "sys dev/.lxc/sys sysfs create=dir,optional 0 0")
		if err != nil {
			return err
		}
	}

	// Setup architecture
	personality, err := osarch.ArchitecturePersonality(d.architecture)
	if err != nil {
		personality, err = osarch.ArchitecturePersonality(d.state.OS.Architectures[0])
		if err != nil {
			return err
		}
	}

	err = lxcSetConfigItem(cc, "lxc.arch", personality)
	if err != nil {
		return err
	}

	// Setup the hooks
	err = lxcSetConfigItem(cc, "lxc.hook.version", "1")
	if err != nil {
		return err
	}

	// Call the onstart hook on start.
	err = lxcSetConfigItem(cc, "lxc.hook.pre-start", fmt.Sprintf("/proc/%d/exe callhook %s %s %s start", os.Getpid(), shared.VarPath(""), strconv.Quote(d.Project()), strconv.Quote(d.Name())))
	if err != nil {
		return err
	}

	// Call the onstopns hook on stop but before namespaces are unmounted.
	err = lxcSetConfigItem(cc, "lxc.hook.stop", fmt.Sprintf("%s callhook %s %s %s stopns", d.state.OS.ExecPath, shared.VarPath(""), strconv.Quote(d.Project()), strconv.Quote(d.Name())))
	if err != nil {
		return err
	}

	// Call the onstop hook on stop.
	err = lxcSetConfigItem(cc, "lxc.hook.post-stop", fmt.Sprintf("%s callhook %s %s %s stop", d.state.OS.ExecPath, shared.VarPath(""), strconv.Quote(d.Project()), strconv.Quote(d.Name())))
	if err != nil {
		return err
	}

	// Setup the console
	err = lxcSetConfigItem(cc, "lxc.tty.max", "0")
	if err != nil {
		return err
	}

	// Setup the hostname
	err = lxcSetConfigItem(cc, "lxc.uts.name", d.Name())
	if err != nil {
		return err
	}

	// Setup devlxd
	if d.expandedConfig["security.devlxd"] == "" || shared.IsTrue(d.expandedConfig["security.devlxd"]) {
		err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s dev/lxd none bind,create=dir 0 0", shared.VarPath("devlxd")))
		if err != nil {
			return err
		}
	}

	// Setup AppArmor
	if d.state.OS.AppArmorAvailable {
		if d.state.OS.AppArmorConfined || !d.state.OS.AppArmorAdmin {
			// If confined but otherwise able to use AppArmor, use our own profile
			curProfile := util.AppArmorProfile()
			curProfile = strings.TrimSuffix(curProfile, " (enforce)")
			err := lxcSetConfigItem(cc, "lxc.apparmor.profile", curProfile)
			if err != nil {
				return err
			}
		} else {
			// If not currently confined, use the container's profile
			profile := apparmor.InstanceProfileName(d)

			/* In the nesting case, we want to enable the inside
			 * LXD to load its profile. Unprivileged containers can
			 * load profiles, but privileged containers cannot, so
			 * let's not use a namespace so they can fall back to
			 * the old way of nesting, i.e. using the parent's
			 * profile.
			 */
			if d.state.OS.AppArmorStacking && !d.state.OS.AppArmorStacked {
				profile = fmt.Sprintf("%s//&:%s:", profile, apparmor.InstanceNamespaceName(d))
			}

			err := lxcSetConfigItem(cc, "lxc.apparmor.profile", profile)
			if err != nil {
				return err
			}
		}
	}

	// Setup Seccomp if necessary
	if seccomp.InstanceNeedsPolicy(d) {
		err = lxcSetConfigItem(cc, "lxc.seccomp.profile", seccomp.ProfilePath(d))
		if err != nil {
			return err
		}

		// Setup notification socket
		// System requirement errors are handled during policy generation instead of here
		ok, err := seccomp.InstanceNeedsIntercept(d.state, d)
		if err == nil && ok {
			err = lxcSetConfigItem(cc, "lxc.seccomp.notify.proxy", fmt.Sprintf("unix:%s", shared.VarPath("seccomp.socket")))
			if err != nil {
				return err
			}
		}
	}

	// Setup idmap
	idmapset, err := d.NextIdmap()
	if err != nil {
		return err
	}

	if idmapset != nil {
		lines := idmapset.ToLxcString()
		for _, line := range lines {
			err := lxcSetConfigItem(cc, "lxc.idmap", line)
			if err != nil {
				return err
			}
		}
	}

	// Setup environment
	for k, v := range d.expandedConfig {
		if strings.HasPrefix(k, "environment.") {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("%s=%s", strings.TrimPrefix(k, "environment."), v))
			if err != nil {
				return err
			}
		}
	}

	// Setup NVIDIA runtime
	if shared.IsTrue(d.expandedConfig["nvidia.runtime"]) {
		hookDir := os.Getenv("LXD_LXC_HOOK")
		if hookDir == "" {
			hookDir = "/usr/share/lxc/hooks"
		}

		hookPath := filepath.Join(hookDir, "nvidia")
		if !shared.PathExists(hookPath) {
			return fmt.Errorf("The NVIDIA LXC hook couldn't be found")
		}

		_, err := exec.LookPath("nvidia-container-cli")
		if err != nil {
			return fmt.Errorf("The NVIDIA container tools couldn't be found")
		}

		err = lxcSetConfigItem(cc, "lxc.environment", "NVIDIA_VISIBLE_DEVICES=none")
		if err != nil {
			return err
		}

		nvidiaDriver := d.expandedConfig["nvidia.driver.capabilities"]
		if nvidiaDriver == "" {
			err = lxcSetConfigItem(cc, "lxc.environment", "NVIDIA_DRIVER_CAPABILITIES=compute,utility")
			if err != nil {
				return err
			}
		} else {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("NVIDIA_DRIVER_CAPABILITIES=%s", nvidiaDriver))
			if err != nil {
				return err
			}
		}

		nvidiaRequireCuda := d.expandedConfig["nvidia.require.cuda"]
		if nvidiaRequireCuda == "" {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("NVIDIA_REQUIRE_CUDA=%s", nvidiaRequireCuda))
			if err != nil {
				return err
			}
		}

		nvidiaRequireDriver := d.expandedConfig["nvidia.require.driver"]
		if nvidiaRequireDriver == "" {
			err = lxcSetConfigItem(cc, "lxc.environment", fmt.Sprintf("NVIDIA_REQUIRE_DRIVER=%s", nvidiaRequireDriver))
			if err != nil {
				return err
			}
		}

		err = lxcSetConfigItem(cc, "lxc.hook.mount", hookPath)
		if err != nil {
			return err
		}
	}

	// Memory limits
	if d.state.OS.CGInfo.Supports(cgroup.Memory, cg) {
		memory := d.expandedConfig["limits.memory"]
		memoryEnforce := d.expandedConfig["limits.memory.enforce"]
		memorySwap := d.expandedConfig["limits.memory.swap"]
		memorySwapPriority := d.expandedConfig["limits.memory.swap.priority"]

		// Configure the memory limits
		if memory != "" {
			var valueInt int64
			if strings.HasSuffix(memory, "%") {
				percent, err := strconv.ParseInt(strings.TrimSuffix(memory, "%"), 10, 64)
				if err != nil {
					return err
				}

				memoryTotal, err := shared.DeviceTotalMemory()
				if err != nil {
					return err
				}

				valueInt = int64((memoryTotal / 100) * percent)
			} else {
				valueInt, err = units.ParseByteSizeString(memory)
				if err != nil {
					return err
				}
			}

			if memoryEnforce == "soft" {
				err = cg.SetMemorySoftLimit(valueInt)
				if err != nil {
					return err
				}
			} else {
				if d.state.OS.CGInfo.Supports(cgroup.MemorySwap, cg) && (memorySwap == "" || shared.IsTrue(memorySwap)) {
					err = cg.SetMemoryLimit(valueInt)
					if err != nil {
						return err
					}

					err = cg.SetMemorySwapLimit(0)
					if err != nil {
						return err
					}
				} else {
					err = cg.SetMemoryLimit(valueInt)
					if err != nil {
						return err
					}
				}

				// Set soft limit to value 10% less than hard limit
				err = cg.SetMemorySoftLimit(int64(float64(valueInt) * 0.9))
				if err != nil {
					return err
				}
			}
		}

		if d.state.OS.CGInfo.Supports(cgroup.MemorySwappiness, cg) {
			// Configure the swappiness
			if memorySwap != "" && !shared.IsTrue(memorySwap) {
				err = cg.SetMemorySwappiness(0)
				if err != nil {
					return err
				}
			} else if memorySwapPriority != "" {
				priority, err := strconv.Atoi(memorySwapPriority)
				if err != nil {
					return err
				}

				err = cg.SetMemorySwappiness(int64(60 - 10 + priority))
				if err != nil {
					return err
				}
			}
		}
	}

	// CPU limits
	cpuPriority := d.expandedConfig["limits.cpu.priority"]
	cpuAllowance := d.expandedConfig["limits.cpu.allowance"]

	if (cpuPriority != "" || cpuAllowance != "") && d.state.OS.CGInfo.Supports(cgroup.CPU, cg) {
		cpuShares, cpuCfsQuota, cpuCfsPeriod, err := cgroup.ParseCPU(cpuAllowance, cpuPriority)
		if err != nil {
			return err
		}

		if cpuShares != 1024 {
			err = cg.SetCPUShare(cpuShares)
			if err != nil {
				return err
			}
		}

		if cpuCfsPeriod != -1 && cpuCfsQuota != -1 {
			err = cg.SetCPUCfsLimit(cpuCfsPeriod, cpuCfsQuota)
			if err != nil {
				return err
			}
		}
	}

	// Disk priority limits.
	diskPriority := d.ExpandedConfig()["limits.disk.priority"]
	if diskPriority != "" {
		if d.state.OS.CGInfo.Supports(cgroup.BlkioWeight, nil) {
			priorityInt, err := strconv.Atoi(diskPriority)
			if err != nil {
				return err
			}

			priority := priorityInt * 100

			// Minimum valid value is 10
			if priority == 0 {
				priority = 10
			}

			err = cg.SetBlkioWeight(int64(priority))
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("Cannot apply limits.disk.priority as blkio.weight cgroup controller is missing")
		}
	}

	// Processes
	if d.state.OS.CGInfo.Supports(cgroup.Pids, cg) {
		processes := d.expandedConfig["limits.processes"]
		if processes != "" {
			valueInt, err := strconv.ParseInt(processes, 10, 64)
			if err != nil {
				return err
			}

			err = cg.SetMaxProcesses(valueInt)
			if err != nil {
				return err
			}
		}
	}

	// Hugepages
	if d.state.OS.CGInfo.Supports(cgroup.Hugetlb, cg) {
		for i, key := range shared.HugePageSizeKeys {
			value := d.expandedConfig[key]
			if value != "" {
				value, err := units.ParseByteSizeString(value)
				if err != nil {
					return err
				}

				err = cg.SetHugepagesLimit(shared.HugePageSizeSuffix[i], value)
				if err != nil {
					return err
				}
			}
		}
	}

	// Setup process limits
	for k, v := range d.expandedConfig {
		if strings.HasPrefix(k, "limits.kernel.") {
			prlimitSuffix := strings.TrimPrefix(k, "limits.kernel.")
			prlimitKey := fmt.Sprintf("lxc.prlimit.%s", prlimitSuffix)
			err = lxcSetConfigItem(cc, prlimitKey, v)
			if err != nil {
				return err
			}
		}
	}

	// Setup shmounts
	if d.state.OS.LXCFeatures["mount_injection_file"] {
		err = lxcSetConfigItem(cc, "lxc.mount.auto", fmt.Sprintf("shmounts:%s:/dev/.lxd-mounts", d.ShmountsPath()))
	} else {
		err = lxcSetConfigItem(cc, "lxc.mount.entry", fmt.Sprintf("%s dev/.lxd-mounts none bind,create=dir 0 0", d.ShmountsPath()))
	}
	if err != nil {
		return err
	}

	// Apply raw.lxc
	if lxcConfig, ok := d.expandedConfig["raw.lxc"]; ok {
		f, err := ioutil.TempFile("", "lxd_config_")
		if err != nil {
			return err
		}

		err = shared.WriteAll(f, []byte(lxcConfig))
		f.Close()
		defer os.Remove(f.Name())
		if err != nil {
			return err
		}

		if err := cc.LoadConfigFile(f.Name()); err != nil {
			return fmt.Errorf("Failed to load raw.lxc")
		}
	}

	if d.c != nil {
		d.c.Release()
	}
	d.c = cc
	freeContainer = false

	return nil
}

var idmappedStorageMap map[unix.Fsid]idmap.IdmapStorageType = map[unix.Fsid]idmap.IdmapStorageType{}
var idmappedStorageMapLock sync.Mutex

// IdmappedStorage determines if the container can use idmapped mounts or shiftfs
func (d *lxc) IdmappedStorage(path string) idmap.IdmapStorageType {
	var mode idmap.IdmapStorageType = idmap.IdmapStorageNone

	if d.state.OS.Shiftfs {
		// Fallback to shiftfs.
		mode = idmap.IdmapStorageShiftfs
	}

	if !d.state.OS.LXCFeatures["idmapped_mounts_v2"] {
		return mode
	}

	buf := &unix.Statfs_t{}
	err := unix.Statfs(path, buf)
	if err != nil {
		// Log error but fallback to shiftfs
		d.logger.Error("Failed to statfs", log.Ctx{"err": err})
		return mode
	}

	idmappedStorageMapLock.Lock()
	defer idmappedStorageMapLock.Unlock()

	val, ok := idmappedStorageMap[buf.Fsid]
	if ok {
		// Return recorded idmapping type.
		return val
	}

	if idmap.CanIdmapMount(path) {
		// Use idmapped mounts.
		mode = idmap.IdmapStorageIdmapped
	}
	idmappedStorageMap[buf.Fsid] = mode

	return mode
}

func (d *lxc) devlxdEventSend(eventType string, eventMessage interface{}) error {
	event := shared.Jmap{}
	event["type"] = eventType
	event["timestamp"] = time.Now()
	event["metadata"] = eventMessage

	return d.state.DevlxdEvents.Send(strconv.Itoa(d.ID()), eventType, eventMessage)
}

// RegisterDevices calls the Register() function on all of the instance's devices.
func (d *lxc) RegisterDevices() {
	devices := d.ExpandedDevices()
	for _, entry := range devices.Sorted() {
		dev, _, err := d.deviceLoad(entry.Name, entry.Config)
		if err == device.ErrUnsupportedDevType {
			continue
		}

		if err != nil {
			d.logger.Error("Failed to load device to register", log.Ctx{"err": err, "device": entry.Name})
			continue
		}

		// Check whether device wants to register for any events.
		err = dev.Register()
		if err != nil {
			d.logger.Error("Failed to register device", log.Ctx{"err": err, "device": entry.Name})
			continue
		}
	}
}

// deviceLoad instantiates and validates a new device and returns it along with enriched config.
func (d *lxc) deviceLoad(deviceName string, rawConfig deviceConfig.Device) (device.Device, deviceConfig.Device, error) {
	var configCopy deviceConfig.Device
	var err error

	// Create copy of config and load some fields from volatile if device is nic or infiniband.
	if shared.StringInSlice(rawConfig["type"], []string{"nic", "infiniband"}) {
		configCopy, err = d.FillNetworkDevice(deviceName, rawConfig)
		if err != nil {
			return nil, nil, err
		}
	} else {
		// Othewise copy the config so it cannot be modified by device.
		configCopy = rawConfig.Clone()
	}

	dev, err := device.New(d, d.state, deviceName, configCopy, d.deviceVolatileGetFunc(deviceName), d.deviceVolatileSetFunc(deviceName))

	// Return device and config copy even if error occurs as caller may still use device.
	return dev, configCopy, err
}

// deviceAdd loads a new device and calls its Add() function.
func (d *lxc) deviceAdd(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool) error {
	dev, _, err := d.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	if instanceRunning && !dev.CanHotPlug() {
		return fmt.Errorf("Device cannot be added when instance is running")
	}

	return dev.Add()
}

// deviceStart loads a new device and calls its Start() function.
func (d *lxc) deviceStart(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool) (*deviceConfig.RunConfig, error) {
	logger := logging.AddContext(d.logger, log.Ctx{"device": deviceName, "type": rawConfig["type"]})
	logger.Debug("Starting device")

	revert := revert.New()
	defer revert.Fail()

	dev, configCopy, err := d.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return nil, err
	}

	if instanceRunning && !dev.CanHotPlug() {
		return nil, fmt.Errorf("Device cannot be started when instance is running")
	}

	runConf, err := dev.Start()
	if err != nil {
		return nil, err
	}

	revert.Add(func() {
		runConf, _ := dev.Stop()
		if runConf != nil {
			d.runHooks(runConf.PostHooks)
		}
	})

	// If runConf supplied, perform any container specific setup of device.
	if runConf != nil {
		// Shift device file ownership if needed before mounting into container.
		// This needs to be done whether or not container is running.
		if len(runConf.Mounts) > 0 {
			err := d.deviceStaticShiftMounts(runConf.Mounts)
			if err != nil {
				return nil, err
			}
		}

		// If container is running and then live attach device.
		if instanceRunning {
			// Attach mounts if requested.
			if len(runConf.Mounts) > 0 {
				err = d.deviceHandleMounts(runConf.Mounts)
				if err != nil {
					return nil, err
				}
			}

			// Add cgroup rules if requested.
			if len(runConf.CGroups) > 0 {
				err = d.deviceAddCgroupRules(runConf.CGroups)
				if err != nil {
					return nil, err
				}
			}

			// Attach network interface if requested.
			if len(runConf.NetworkInterface) > 0 {
				err = d.deviceAttachNIC(configCopy, runConf.NetworkInterface)
				if err != nil {
					return nil, err
				}
			}

			// If running, run post start hooks now (if not running LXD will run them
			// once the instance is started).
			err = d.runHooks(runConf.PostHooks)
			if err != nil {
				return nil, err
			}
		}
	}

	revert.Success()
	return runConf, nil
}

// deviceStaticShiftMounts statically shift device mount files ownership to active idmap if needed.
func (d *lxc) deviceStaticShiftMounts(mounts []deviceConfig.MountEntryItem) error {
	idmapSet, err := d.CurrentIdmap()
	if err != nil {
		return fmt.Errorf("Failed to get idmap for device: %s", err)
	}

	// If there is an idmap being applied and LXD not running in a user namespace then shift the
	// device files before they are mounted.
	if idmapSet != nil && !d.state.OS.RunningInUserNS {
		for _, mount := range mounts {
			// Skip UID/GID shifting if OwnerShift mode is not static, or the host-side
			// DevPath is empty (meaning an unmount request that doesn't need shifting).
			if mount.OwnerShift != deviceConfig.MountOwnerShiftStatic || mount.DevPath == "" {
				continue
			}

			err := idmapSet.ShiftFile(mount.DevPath)
			if err != nil {
				// uidshift failing is weird, but not a big problem. Log and proceed.
				d.logger.Debug("Failed to uidshift device", log.Ctx{"mountDevPath": mount.DevPath, "err": err})
			}
		}
	}

	return nil
}

// deviceAddCgroupRules live adds cgroup rules to a container.
func (d *lxc) deviceAddCgroupRules(cgroups []deviceConfig.RunConfigItem) error {
	cg, err := d.cgroup(nil)
	if err != nil {
		return err
	}

	for _, rule := range cgroups {
		// Only apply devices cgroup rules if container is running privileged and host has devices cgroup controller.
		if strings.HasPrefix(rule.Key, "devices.") && (!d.isCurrentlyPrivileged() || d.state.OS.RunningInUserNS || !d.state.OS.CGInfo.Supports(cgroup.Devices, cg)) {
			continue
		}

		// Add the new device cgroup rule.
		err := d.CGroupSet(rule.Key, rule.Value)
		if err != nil {
			return fmt.Errorf("Failed to add cgroup rule for device")
		}
	}

	return nil
}

// deviceAttachNIC live attaches a NIC device to a container.
func (d *lxc) deviceAttachNIC(configCopy map[string]string, netIF []deviceConfig.RunConfigItem) error {
	devName := ""
	for _, dev := range netIF {
		if dev.Key == "link" {
			devName = dev.Value
			break
		}
	}

	if devName == "" {
		return fmt.Errorf("Device didn't provide a link property to use")
	}

	// Load the go-lxc struct.
	err := d.initLXC(false)
	if err != nil {
		return err
	}

	// Add the interface to the container.
	err = d.c.AttachInterface(devName, configCopy["name"])
	if err != nil {
		return fmt.Errorf("Failed to attach interface: %s to %s: %s", devName, configCopy["name"], err)
	}

	return nil
}

// deviceUpdate loads a new device and calls its Update() function.
func (d *lxc) deviceUpdate(deviceName string, rawConfig deviceConfig.Device, oldDevices deviceConfig.Devices, instanceRunning bool) error {
	dev, _, err := d.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	err = dev.Update(oldDevices, instanceRunning)
	if err != nil {
		return err
	}

	return nil
}

// deviceStop loads a new device and calls its Stop() function.
// Accepts a stopHookNetnsPath argument which is required when run from the onStopNS hook before the
// container's network namespace is unmounted (which is required for NIC device cleanup).
func (d *lxc) deviceStop(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool, stopHookNetnsPath string) error {
	logger := logging.AddContext(d.logger, log.Ctx{"device": deviceName, "type": rawConfig["type"]})
	logger.Debug("Stopping device")

	dev, configCopy, err := d.deviceLoad(deviceName, rawConfig)

	// If deviceLoad fails with unsupported device type then return.
	if err == device.ErrUnsupportedDevType {
		return err
	}

	// If deviceLoad fails for any other reason then just log the error and proceed, as in the
	// scenario that a new version of LXD has additional validation restrictions than older
	// versions we still need to allow previously valid devices to be stopped.
	if err != nil {
		// If there is no device returned, then we cannot proceed, so return as error.
		if dev == nil {
			return fmt.Errorf("Device stop validation failed for %q: %v", deviceName, err)
		}

		logger.Error("Device stop validation failed for", log.Ctx{"err": err})
	}

	if instanceRunning && !dev.CanHotPlug() {
		return fmt.Errorf("Device cannot be stopped when instance is running")
	}

	runConf, err := dev.Stop()
	if err != nil {
		return err
	}

	if runConf != nil {
		// If network interface settings returned, then detach NIC from container.
		if len(runConf.NetworkInterface) > 0 {
			err = d.deviceDetachNIC(configCopy, runConf.NetworkInterface, instanceRunning, stopHookNetnsPath)
			if err != nil {
				return err
			}
		}

		// Add cgroup rules if requested and container is running.
		if len(runConf.CGroups) > 0 && instanceRunning {
			err = d.deviceAddCgroupRules(runConf.CGroups)
			if err != nil {
				return err
			}
		}

		// Detach mounts if requested and container is running.
		if len(runConf.Mounts) > 0 && instanceRunning {
			err = d.deviceHandleMounts(runConf.Mounts)
			if err != nil {
				return err
			}
		}

		// Run post stop hooks irrespective of run state of instance.
		err = d.runHooks(runConf.PostHooks)
		if err != nil {
			return err
		}
	}

	return nil
}

// deviceDetachNIC detaches a NIC device from a container.
// Accepts a stopHookNetnsPath argument which is required when run from the onStopNS hook before the
// container's network namespace is unmounted (which is required for NIC device cleanup).
func (d *lxc) deviceDetachNIC(configCopy map[string]string, netIF []deviceConfig.RunConfigItem, instanceRunning bool, stopHookNetnsPath string) error {
	// Get requested device name to detach interface back to on the host.
	devName := ""
	for _, dev := range netIF {
		if dev.Key == "link" {
			devName = dev.Value
			break
		}
	}

	if devName == "" {
		return fmt.Errorf("Device didn't provide a link property to use")
	}

	// If container is running, perform live detach of interface back to host.
	if instanceRunning {
		// For some reason, having network config confuses detach, so get our own go-lxc struct.
		cname := project.Instance(d.Project(), d.Name())
		cc, err := liblxc.NewContainer(cname, d.state.OS.LxcPath)
		if err != nil {
			return err
		}
		defer cc.Release()

		// Get interfaces inside container.
		ifaces, err := cc.Interfaces()
		if err != nil {
			return fmt.Errorf("Failed to list network interfaces: %v", err)
		}

		// If interface doesn't exist inside container, cannot proceed.
		if !shared.StringInSlice(configCopy["name"], ifaces) {
			return nil
		}

		err = cc.DetachInterfaceRename(configCopy["name"], devName)
		if err != nil {
			return errors.Wrapf(err, "Failed to detach interface: %q to %q", configCopy["name"], devName)
		}
	} else {
		// Currently liblxc does not move devices back to the host on stop that were added
		// after the the container was started. For this reason we utilise the lxc.hook.stop
		// hook so that we can capture the netns path, enter the namespace and move the nics
		// back to the host and rename them if liblxc hasn't already done it.
		// We can only move back devices that have an expected host_name record and where
		// that device doesn't already exist on the host as if a device exists on the host
		// we can't know whether that is because liblxc has moved it back already or whether
		// it is a conflicting device.
		if !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", devName)) {
			if stopHookNetnsPath == "" {
				return fmt.Errorf("Cannot detach NIC device %q without stopHookNetnsPath being provided", devName)
			}

			err := d.detachInterfaceRename(stopHookNetnsPath, configCopy["name"], devName)
			if err != nil {
				return errors.Wrapf(err, "Failed to detach interface: %q to %q", configCopy["name"], devName)
			}
			d.logger.Debug("Detached NIC device interface", log.Ctx{"name": configCopy["name"], "devName": devName})
		}
	}

	return nil
}

// deviceHandleMounts live attaches or detaches mounts on a container.
// If the mount DevPath is empty the mount action is treated as unmount.
func (d *lxc) deviceHandleMounts(mounts []deviceConfig.MountEntryItem) error {
	for _, mount := range mounts {
		if mount.DevPath != "" {
			flags := 0

			// Convert options into flags.
			for _, opt := range mount.Opts {
				if opt == "bind" {
					flags |= unix.MS_BIND
				} else if opt == "rbind" {
					flags |= unix.MS_BIND | unix.MS_REC
				} else if opt == "ro" {
					flags |= unix.MS_RDONLY
				}
			}

			var idmapType idmap.IdmapStorageType = idmap.IdmapStorageNone
			if mount.OwnerShift == deviceConfig.MountOwnerShiftDynamic {
				idmapType = d.IdmappedStorage(mount.DevPath)
				if idmapType == idmap.IdmapStorageNone {
					return fmt.Errorf("Required idmapping abilities not available")
				}
			}

			// Mount it into the container.
			err := d.insertMount(mount.DevPath, mount.TargetPath, mount.FSType, flags, idmapType)
			if err != nil {
				return fmt.Errorf("Failed to add mount for device inside container: %s", err)
			}
		} else {
			relativeTargetPath := strings.TrimPrefix(mount.TargetPath, "/")
			if d.FileExists(relativeTargetPath) == nil {
				err := d.removeMount(mount.TargetPath)
				if err != nil {
					return fmt.Errorf("Error unmounting the device path inside container: %s", err)
				}

				err = d.FileRemove(relativeTargetPath)
				if err != nil {
					// Only warn here and don't fail as removing a directory
					// mount may fail if there was already files inside
					// directory before it was mouted over preventing delete.
					d.logger.Warn("Could not remove the device path inside container", log.Ctx{"err": err})
				}
			}
		}
	}

	return nil
}

// deviceRemove loads a new device and calls its Remove() function.
func (d *lxc) deviceRemove(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool) error {
	logger := logging.AddContext(d.logger, log.Ctx{"device": deviceName, "type": rawConfig["type"]})

	dev, _, err := d.deviceLoad(deviceName, rawConfig)

	// If deviceLoad fails with unsupported device type then return.
	if err == device.ErrUnsupportedDevType {
		return err
	}

	// If deviceLoad fails for any other reason then just log the error and proceed, as in the
	// scenario that a new version of LXD has additional validation restrictions than older
	// versions we still need to allow previously valid devices to be stopped.
	if err != nil {
		// If there is no device returned, then we cannot proceed, so return as error.
		if dev == nil {
			return fmt.Errorf("Device remove validation failed for %q: %v", deviceName, err)
		}

		logger.Error("Device remove validation failed", log.Ctx{"err": err})
	}

	if instanceRunning && !dev.CanHotPlug() {
		return fmt.Errorf("Device cannot be removed when instance is running")
	}

	return dev.Remove()
}

// DeviceEventHandler actions the results of a RunConfig after an event has occurred on a device.
func (d *lxc) DeviceEventHandler(runConf *deviceConfig.RunConfig) error {
	// Device events can only be processed when the container is running.
	if !d.IsRunning() {
		return nil
	}

	if runConf == nil {
		return nil
	}

	// Shift device file ownership if needed before mounting devices into container.
	if len(runConf.Mounts) > 0 {
		err := d.deviceStaticShiftMounts(runConf.Mounts)
		if err != nil {
			return err
		}

		err = d.deviceHandleMounts(runConf.Mounts)
		if err != nil {
			return err
		}
	}

	// Add cgroup rules if requested.
	if len(runConf.CGroups) > 0 {
		err := d.deviceAddCgroupRules(runConf.CGroups)
		if err != nil {
			return err
		}
	}

	// Run any post hooks requested.
	err := d.runHooks(runConf.PostHooks)
	if err != nil {
		return err
	}

	// Generate uevent inside container if requested.
	if len(runConf.Uevents) > 0 {

		pidFdNr, pidFd := d.inheritInitPidFd()
		if pidFdNr >= 0 {
			defer pidFd.Close()
		}

		for _, eventParts := range runConf.Uevents {
			ueventArray := make([]string, 6)
			ueventArray[0] = "forkuevent"
			ueventArray[2] = "inject"
			ueventArray[1] = "--"
			ueventArray[3] = fmt.Sprintf("%d", d.InitPID())
			ueventArray[4] = fmt.Sprintf("%d", pidFdNr)
			length := 0
			for _, part := range eventParts {
				length = length + len(part) + 1
			}
			ueventArray[5] = fmt.Sprintf("%d", length)
			ueventArray = append(ueventArray, eventParts...)
			_, _, err := shared.RunCommandSplit(nil, []*os.File{pidFd}, d.state.OS.ExecPath, ueventArray...)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (d *lxc) handleIdmappedStorage() (idmap.IdmapStorageType, *idmap.IdmapSet, error) {
	diskIdmap, err := d.DiskIdmap()
	if err != nil {
		return idmap.IdmapStorageNone, nil, errors.Wrap(err, "Set last ID map")
	}

	nextIdmap, err := d.NextIdmap()
	if err != nil {
		return idmap.IdmapStorageNone, nil, errors.Wrap(err, "Set ID map")
	}

	// Identical on-disk idmaps so no changes required.
	if nextIdmap.Equals(diskIdmap) {
		return idmap.IdmapStorageNone, nextIdmap, nil
	}

	// There's no on-disk idmap applied and the container can use idmapped
	// storage.
	idmapType := d.IdmappedStorage(d.RootfsPath())
	if diskIdmap == nil && idmapType != idmap.IdmapStorageNone {
		return idmapType, nextIdmap, nil
	}

	// We need to change the on-disk idmap but the container is protected
	// against idmap changes.
	if shared.IsTrue(d.expandedConfig["security.protection.shift"]) {
		return idmap.IdmapStorageNone, nil, fmt.Errorf("Container is protected against filesystem shifting")
	}

	d.logger.Debug("Container idmap changed, remapping")
	d.updateProgress("Remapping container filesystem")

	storageType, err := d.getStorageType()
	if err != nil {
		return idmap.IdmapStorageNone, nil, errors.Wrap(err, "Storage type")
	}

	// Revert the currently applied on-disk idmap.
	if diskIdmap != nil {
		if storageType == "zfs" {
			err = diskIdmap.UnshiftRootfs(d.RootfsPath(), storageDrivers.ShiftZFSSkipper)
		} else if storageType == "btrfs" {
			err = storageDrivers.UnshiftBtrfsRootfs(d.RootfsPath(), diskIdmap)
		} else {
			err = diskIdmap.UnshiftRootfs(d.RootfsPath(), nil)
		}
		if err != nil {
			return idmap.IdmapStorageNone, nil, err
		}
	}

	jsonDiskIdmap := "[]"

	// If the container can't use idmapped storage apply the new on-disk
	// idmap of the container now. Otherwise we will later instruct LXC to
	// make use of idmapped storage.
	if nextIdmap != nil && idmapType == idmap.IdmapStorageNone {
		if storageType == "zfs" {
			err = nextIdmap.ShiftRootfs(d.RootfsPath(), storageDrivers.ShiftZFSSkipper)
		} else if storageType == "btrfs" {
			err = storageDrivers.ShiftBtrfsRootfs(d.RootfsPath(), nextIdmap)
		} else {
			err = nextIdmap.ShiftRootfs(d.RootfsPath(), nil)
		}
		if err != nil {
			return idmap.IdmapStorageNone, nil, err
		}

		idmapBytes, err := json.Marshal(nextIdmap.Idmap)
		if err != nil {
			return idmap.IdmapStorageNone, nil, err
		}
		jsonDiskIdmap = string(idmapBytes)
	}

	err = d.VolatileSet(map[string]string{"volatile.last_state.idmap": jsonDiskIdmap})
	if err != nil {
		return idmap.IdmapStorageNone, nextIdmap, errors.Wrapf(err, "Set volatile.last_state.idmap config key on container %q (id %d)", d.name, d.id)
	}

	d.updateProgress("")
	return idmapType, nextIdmap, nil
}

// Start functions
func (d *lxc) startCommon() (string, []func() error, error) {
	revert := revert.New()
	defer revert.Fail()

	// Load the go-lxc struct
	err := d.initLXC(true)
	if err != nil {
		return "", nil, errors.Wrap(err, "Load go-lxc struct")
	}

	// Load any required kernel modules
	kernelModules := d.expandedConfig["linux.kernel_modules"]
	if kernelModules != "" {
		for _, module := range strings.Split(kernelModules, ",") {
			module = strings.TrimPrefix(module, " ")
			err := util.LoadModule(module)
			if err != nil {
				return "", nil, fmt.Errorf("Failed to load kernel module '%s': %s", module, err)
			}
		}
	}

	// Rotate the log file.
	logfile := d.LogFilePath()
	if shared.PathExists(logfile) {
		os.Remove(logfile + ".old")
		err := os.Rename(logfile, logfile+".old")
		if err != nil {
			return "", nil, err
		}
	}

	// Mount instance root volume.
	_, err = d.mount()
	if err != nil {
		return "", nil, err
	}
	revert.Add(func() { d.unmount() })

	idmapType, nextIdmap, err := d.handleIdmappedStorage()
	if err != nil {
		return "", nil, errors.Wrap(err, "Failed to handle idmappe storage")
	}

	var idmapBytes []byte
	if nextIdmap == nil {
		idmapBytes = []byte("[]")
	} else {
		idmapBytes, err = json.Marshal(nextIdmap.Idmap)
		if err != nil {
			return "", nil, err
		}
	}

	if d.localConfig["volatile.idmap.current"] != string(idmapBytes) {
		err = d.VolatileSet(map[string]string{"volatile.idmap.current": string(idmapBytes)})
		if err != nil {
			return "", nil, errors.Wrapf(err, "Set volatile.idmap.current config key on container %q (id %d)", d.name, d.id)
		}
	}

	// Generate the Seccomp profile
	if err := seccomp.CreateProfile(d.state, d); err != nil {
		return "", nil, err
	}

	// Cleanup any existing leftover devices
	d.removeUnixDevices()
	d.removeDiskDevices()

	// Create any missing directories.
	err = os.MkdirAll(d.LogPath(), 0700)
	if err != nil {
		return "", nil, err
	}

	err = os.MkdirAll(d.DevicesPath(), 0711)
	if err != nil {
		return "", nil, err
	}

	err = os.MkdirAll(d.ShmountsPath(), 0711)
	if err != nil {
		return "", nil, err
	}

	// Generate UUID if not present.
	instUUID := d.localConfig["volatile.uuid"]
	if instUUID == "" {
		instUUID = uuid.New()
		d.VolatileSet(map[string]string{"volatile.uuid": instUUID})
	}

	// Create the devices
	postStartHooks := []func() error{}
	nicID := -1
	nvidiaDevices := []string{}

	// Setup devices in sorted order, this ensures that device mounts are added in path order.
	for _, entry := range d.expandedDevices.Sorted() {
		dev := entry // Ensure device variable has local scope for revert.

		// Start the device.
		runConf, err := d.deviceStart(dev.Name, dev.Config, false)
		if err != nil {
			return "", nil, errors.Wrapf(err, "Failed to start device %q", dev.Name)
		}

		// Stop device on failure to setup container.
		revert.Add(func() {
			err := d.deviceStop(dev.Name, dev.Config, false, "")
			if err != nil {
				d.logger.Error("Failed to cleanup device", log.Ctx{"devName": dev.Name, "err": err})
			}
		})
		if runConf == nil {
			continue
		}

		// Process rootfs setup.
		if runConf.RootFS.Path != "" {
			if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
				// Set the rootfs backend type if supported (must happen before any other lxc.rootfs)
				err := lxcSetConfigItem(d.c, "lxc.rootfs.backend", "dir")
				if err == nil {
					value := d.c.ConfigItem("lxc.rootfs.backend")
					if len(value) == 0 || value[0] != "dir" {
						lxcSetConfigItem(d.c, "lxc.rootfs.backend", "")
					}
				}
			}

			if util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
				rootfsPath := fmt.Sprintf("dir:%s", runConf.RootFS.Path)
				err = lxcSetConfigItem(d.c, "lxc.rootfs.path", rootfsPath)
			} else {
				err = lxcSetConfigItem(d.c, "lxc.rootfs", runConf.RootFS.Path)
			}

			if err != nil {
				return "", nil, errors.Wrapf(err, "Failed to setup device rootfs '%s'", dev.Name)
			}

			if len(runConf.RootFS.Opts) > 0 {
				err = lxcSetConfigItem(d.c, "lxc.rootfs.options", strings.Join(runConf.RootFS.Opts, ","))
				if err != nil {
					return "", nil, errors.Wrapf(err, "Failed to setup device rootfs '%s'", dev.Name)
				}
			}

			if !d.IsPrivileged() {
				if idmapType == idmap.IdmapStorageIdmapped {
					err = lxcSetConfigItem(d.c, "lxc.rootfs.options", "idmap=container")
					if err != nil {
						return "", nil, errors.Wrapf(err, "Failed to set \"idmap=container\" rootfs option")
					}
				} else if idmapType == idmap.IdmapStorageShiftfs {
					// Host side mark mount.
					err = lxcSetConfigItem(d.c, "lxc.hook.pre-start", fmt.Sprintf("/bin/mount -t shiftfs -o mark,passthrough=3 %s %s", strconv.Quote(d.RootfsPath()), strconv.Quote(d.RootfsPath())))
					if err != nil {
						return "", nil, errors.Wrapf(err, "Failed to setup device mount shiftfs '%s'", dev.Name)
					}

					// Container side shift mount.
					err = lxcSetConfigItem(d.c, "lxc.hook.pre-mount", fmt.Sprintf("/bin/mount -t shiftfs -o passthrough=3 %s %s", strconv.Quote(d.RootfsPath()), strconv.Quote(d.RootfsPath())))
					if err != nil {
						return "", nil, errors.Wrapf(err, "Failed to setup device mount shiftfs '%s'", dev.Name)
					}

					// Host side umount of mark mount.
					err = lxcSetConfigItem(d.c, "lxc.hook.start-host", fmt.Sprintf("/bin/umount -l %s", strconv.Quote(d.RootfsPath())))
					if err != nil {
						return "", nil, errors.Wrapf(err, "Failed to setup device mount shiftfs '%s'", dev.Name)
					}
				}
			}
		}

		// Pass any cgroups rules into LXC.
		if len(runConf.CGroups) > 0 {
			for _, rule := range runConf.CGroups {
				if strings.HasPrefix(rule.Key, "devices.") && (!d.isCurrentlyPrivileged() || d.state.OS.RunningInUserNS) {
					continue
				}
				if d.state.OS.CGInfo.Layout == cgroup.CgroupsUnified {
					err = lxcSetConfigItem(d.c, fmt.Sprintf("lxc.cgroup2.%s", rule.Key), rule.Value)
				} else {
					err = lxcSetConfigItem(d.c, fmt.Sprintf("lxc.cgroup.%s", rule.Key), rule.Value)
				}
				if err != nil {
					return "", nil, errors.Wrapf(err, "Failed to setup device cgroup '%s'", dev.Name)
				}
			}
		}

		// Pass any mounts into LXC.
		if len(runConf.Mounts) > 0 {
			for _, mount := range runConf.Mounts {
				if shared.StringInSlice("propagation", mount.Opts) && !util.RuntimeLiblxcVersionAtLeast(3, 0, 0) {
					return "", nil, errors.Wrapf(fmt.Errorf("liblxc 3.0 is required for mount propagation configuration"), "Failed to setup device mount '%s'", dev.Name)
				}

				mntOptions := strings.Join(mount.Opts, ",")

				if mount.OwnerShift == deviceConfig.MountOwnerShiftDynamic && !d.IsPrivileged() {
					switch d.IdmappedStorage(mount.DevPath) {
					case idmap.IdmapStorageIdmapped:
						mntOptions = strings.Join([]string{mntOptions, "idmap=container"}, ",")
					case idmap.IdmapStorageShiftfs:
						err = lxcSetConfigItem(d.c, "lxc.hook.pre-start", fmt.Sprintf("/bin/mount -t shiftfs -o mark,passthrough=3 %s %s", strconv.Quote(mount.DevPath), strconv.Quote(mount.DevPath)))
						if err != nil {
							return "", nil, errors.Wrapf(err, "Failed to setup device mount shiftfs '%s'", dev.Name)
						}

						err = lxcSetConfigItem(d.c, "lxc.hook.pre-mount", fmt.Sprintf("/bin/mount -t shiftfs -o passthrough=3 %s %s", strconv.Quote(mount.DevPath), strconv.Quote(mount.DevPath)))
						if err != nil {
							return "", nil, errors.Wrapf(err, "Failed to setup device mount shiftfs '%s'", dev.Name)
						}

						err = lxcSetConfigItem(d.c, "lxc.hook.start-host", fmt.Sprintf("/bin/umount -l %s", strconv.Quote(mount.DevPath)))
						if err != nil {
							return "", nil, errors.Wrapf(err, "Failed to setup device mount shiftfs '%s'", dev.Name)
						}
					case idmap.IdmapStorageNone:
						return "", nil, errors.Wrapf(fmt.Errorf("idmapping abilities are required but aren't supported on system"), "Failed to setup device mount '%s'", dev.Name)
					}
				}

				mntVal := fmt.Sprintf("%s %s %s %s %d %d", shared.EscapePathFstab(mount.DevPath), shared.EscapePathFstab(mount.TargetPath), mount.FSType, mntOptions, mount.Freq, mount.PassNo)
				err = lxcSetConfigItem(d.c, "lxc.mount.entry", mntVal)
				if err != nil {
					return "", nil, errors.Wrapf(err, "Failed to setup device mount '%s'", dev.Name)
				}
			}
		}

		// Pass any network setup config into LXC.
		if len(runConf.NetworkInterface) > 0 {
			// Increment nicID so that LXC network index is unique per device.
			nicID++

			networkKeyPrefix := "lxc.net"
			if !util.RuntimeLiblxcVersionAtLeast(2, 1, 0) {
				networkKeyPrefix = "lxc.network"
			}

			for _, nicItem := range runConf.NetworkInterface {
				err = lxcSetConfigItem(d.c, fmt.Sprintf("%s.%d.%s", networkKeyPrefix, nicID, nicItem.Key), nicItem.Value)
				if err != nil {
					return "", nil, errors.Wrapf(err, "Failed to setup device network interface '%s'", dev.Name)
				}
			}
		}

		// Add any post start hooks.
		if len(runConf.PostHooks) > 0 {
			postStartHooks = append(postStartHooks, runConf.PostHooks...)
		}

		// Build list of NVIDIA GPUs (used for MIG).
		if len(runConf.GPUDevice) > 0 {
			for _, entry := range runConf.GPUDevice {
				if entry.Key == device.GPUNvidiaDeviceKey {
					nvidiaDevices = append(nvidiaDevices, entry.Value)
				}
			}
		}
	}

	// Override NVIDIA_VISIBLE_DEVICES if we have devices that need it.
	if len(nvidiaDevices) > 0 {
		err = lxcSetConfigItem(d.c, "lxc.environment", fmt.Sprintf("NVIDIA_VISIBLE_DEVICES=%s", strings.Join(nvidiaDevices, ",")))
		if err != nil {
			return "", nil, errors.Wrapf(err, "Unable to set NVIDIA_VISIBLE_DEVICES in LXC environment")
		}
	}

	// Generate the LXC config
	configPath := filepath.Join(d.LogPath(), "lxc.conf")
	err = d.c.SaveConfigFile(configPath)
	if err != nil {
		os.Remove(configPath)
		return "", nil, err
	}

	// Set ownership to match container root
	currentIdmapset, err := d.CurrentIdmap()
	if err != nil {
		return "", nil, err
	}

	uid := int64(0)
	if currentIdmapset != nil {
		uid, _ = currentIdmapset.ShiftFromNs(0, 0)
	}

	err = os.Chown(d.Path(), int(uid), 0)
	if err != nil {
		return "", nil, err
	}

	// We only need traversal by root in the container
	err = os.Chmod(d.Path(), 0100)
	if err != nil {
		return "", nil, err
	}

	// Update the backup.yaml file
	err = d.UpdateBackupFile()
	if err != nil {
		return "", nil, err
	}

	// If starting stateless, wipe state
	if !d.IsStateful() && shared.PathExists(d.StatePath()) {
		os.RemoveAll(d.StatePath())
	}

	// Unmount any previously mounted shiftfs
	unix.Unmount(d.RootfsPath(), unix.MNT_DETACH)

	// Snapshot if needed.
	err = d.startupSnapshot(d)
	if err != nil {
		return "", nil, err
	}

	revert.Success()
	return configPath, postStartHooks, nil
}

// detachInterfaceRename enters the container's network namespace and moves the named interface
// in ifName back to the network namespace of the running process as the name specified in hostName.
func (d *lxc) detachInterfaceRename(netns string, ifName string, hostName string) error {
	lxdPID := os.Getpid()

	// Run forknet detach
	_, err := shared.RunCommand(
		d.state.OS.ExecPath,
		"forknet",
		"detach",
		"--",
		netns,
		fmt.Sprintf("%d", lxdPID),
		ifName,
		hostName,
	)

	// Process forknet detach response
	if err != nil {
		return err
	}

	return nil
}

// Start starts the instance.
func (d *lxc) Start(stateful bool) error {
	d.logger.Debug("Start started", log.Ctx{"stateful": stateful})
	defer d.logger.Debug("Start finished", log.Ctx{"stateful": stateful})

	// Check that we are startable before creating an operation lock, so if the instance is in the
	// process of stopping we don't prevent the stop hooks from running due to our start operation lock.
	err := d.isStartableStatusCode(d.statusCode())
	if err != nil {
		return err
	}

	var ctxMap log.Ctx

	// Setup a new operation
	exists, op, err := operationlock.CreateWaitGet(d.id, "start", []string{"restart", "restore"}, false, false)
	if err != nil {
		return errors.Wrap(err, "Create container start operation")
	}
	if exists {
		// An existing matching operation has now succeeded, return.
		return nil
	}
	defer op.Done(nil)

	if !daemon.SharedMountsSetup {
		err = fmt.Errorf("Daemon failed to setup shared mounts base. Does security.nesting need to be turned on?")
		op.Done(err)
		return err
	}

	// Run the shared start code
	configPath, postStartHooks, err := d.startCommon()
	if err != nil {
		op.Done(err)
		return errors.Wrap(err, "Failed preparing container for start")
	}

	ctxMap = log.Ctx{
		"action":    op.Action(),
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate,
		"stateful":  stateful}

	if op.Action() == "start" {
		d.logger.Info("Starting container", ctxMap)
	}

	// If stateful, restore now
	if stateful {
		if !d.stateful {
			err = fmt.Errorf("Container has no existing state to restore")
			op.Done(err)
			return err
		}

		criuMigrationArgs := instance.CriuMigrationArgs{
			Cmd:          liblxc.MIGRATE_RESTORE,
			StateDir:     d.StatePath(),
			Function:     "snapshot",
			Stop:         false,
			ActionScript: false,
			DumpDir:      "",
			PreDumpDir:   "",
		}

		err := d.Migrate(&criuMigrationArgs)
		if err != nil && !d.IsRunning() {
			op.Done(err)
			return errors.Wrap(err, "Migrate")
		}

		os.RemoveAll(d.StatePath())
		d.stateful = false

		err = d.state.Cluster.UpdateInstanceStatefulFlag(d.id, false)
		if err != nil {
			op.Done(err)
			return errors.Wrap(err, "Start container")
		}

		// Run any post start hooks.
		err = d.runHooks(postStartHooks)
		if err != nil {
			op.Done(err) // Must come before Stop() otherwise stop will not proceed.

			// Attempt to stop container.
			d.Stop(false)

			return err
		}

		if op.Action() == "start" {
			d.logger.Info("Started container", ctxMap)
			d.state.Events.SendLifecycle(d.project, lifecycle.InstanceStarted.Event(d, nil))
		}
		return nil
	} else if d.stateful {
		/* stateless start required when we have state, let's delete it */
		err := os.RemoveAll(d.StatePath())
		if err != nil {
			op.Done(err)
			return err
		}

		d.stateful = false
		err = d.state.Cluster.UpdateInstanceStatefulFlag(d.id, false)
		if err != nil {
			op.Done(err)
			return errors.Wrap(err, "Persist stateful flag")
		}
	}

	name := project.Instance(d.Project(), d.name)

	// Start the LXC container
	_, err = shared.RunCommand(
		d.state.OS.ExecPath,
		"forkstart",
		name,
		d.state.OS.LxcPath,
		configPath)
	if err != nil && !d.IsRunning() {
		// Attempt to extract the LXC errors
		lxcLog := ""
		logPath := filepath.Join(d.LogPath(), "lxc.log")
		if shared.PathExists(logPath) {
			logContent, err := ioutil.ReadFile(logPath)
			if err == nil {
				for _, line := range strings.Split(string(logContent), "\n") {
					fields := strings.Fields(line)
					if len(fields) < 4 {
						continue
					}

					// We only care about errors
					if fields[2] != "ERROR" {
						continue
					}

					// Prepend the line break
					if len(lxcLog) == 0 {
						lxcLog += "\n"
					}

					lxcLog += fmt.Sprintf("  %s\n", strings.Join(fields[0:], " "))
				}
			}
		}

		d.logger.Error("Failed starting container", ctxMap)

		// Return the actual error
		op.Done(err)
		return err
	}

	// Run any post start hooks.
	err = d.runHooks(postStartHooks)
	if err != nil {
		op.Done(err) // Must come before Stop() otherwise stop will not proceed.

		// Attempt to stop container.
		d.Stop(false)

		return err
	}

	if op.Action() == "start" {
		d.logger.Info("Started container", ctxMap)
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceStarted.Event(d, nil))
	}

	return nil
}

// OnHook is the top-level hook handler.
func (d *lxc) OnHook(hookName string, args map[string]string) error {
	switch hookName {
	case instance.HookStart:
		return d.onStart(args)
	case instance.HookStopNS:
		return d.onStopNS(args)
	case instance.HookStop:
		return d.onStop(args)
	default:
		return instance.ErrNotImplemented
	}
}

// onStart implements the start hook.
func (d *lxc) onStart(_ map[string]string) error {
	// Make sure we can't call go-lxc functions by mistake
	d.fromHook = true

	// Load the container AppArmor profile
	err := apparmor.InstanceLoad(d.state, d)
	if err != nil {
		return err
	}

	// Template anything that needs templating
	key := "volatile.apply_template"
	if d.localConfig[key] != "" {
		// Run any template that needs running
		err = d.templateApplyNow(instance.TemplateTrigger(d.localConfig[key]))
		if err != nil {
			apparmor.InstanceUnload(d.state, d)
			return err
		}

		// Remove the volatile key from the DB
		err := d.state.Cluster.DeleteInstanceConfigKey(d.id, key)
		if err != nil {
			apparmor.InstanceUnload(d.state, d)
			return err
		}
	}

	err = d.templateApplyNow("start")
	if err != nil {
		apparmor.InstanceUnload(d.state, d)
		return err
	}

	// Trigger a rebalance
	cgroup.TaskSchedulerTrigger("container", d.name, "started")

	// Apply network priority
	if d.expandedConfig["limits.network.priority"] != "" {
		go func(d *lxc) {
			d.fromHook = false
			err := d.setNetworkPriority()
			if err != nil {
				d.logger.Error("Failed to apply network priority", log.Ctx{"err": err})
			}
		}(d)
	}

	// Database updates
	err = d.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Record current state
		err = tx.UpdateInstancePowerState(d.id, "RUNNING")
		if err != nil {
			return errors.Wrap(err, "Error updating container state")
		}

		// Update time container last started time
		err = tx.UpdateInstanceLastUsedDate(d.id, time.Now().UTC())
		if err != nil {
			return errors.Wrap(err, "Error updating last used")
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// Stop functions
func (d *lxc) Stop(stateful bool) error {
	d.logger.Debug("Stop started", log.Ctx{"stateful": stateful})
	defer d.logger.Debug("Stop finished", log.Ctx{"stateful": stateful})

	// Must be run prior to creating the operation lock.
	if !d.IsRunning() {
		return fmt.Errorf("The instance is already stopped")
	}

	// Setup a new operation
	exists, op, err := operationlock.CreateWaitGet(d.id, "stop", []string{"restart", "restore"}, false, true)
	if err != nil {
		return err
	}
	if exists {
		// An existing matching operation has now succeeded, return.
		return nil
	}

	ctxMap := log.Ctx{
		"action":    op.Action(),
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate,
		"stateful":  stateful}

	if op.Action() == "stop" {
		d.logger.Info("Stopping container", ctxMap)
	}

	// Handle stateful stop
	if stateful {
		// Cleanup any existing state
		stateDir := d.StatePath()
		os.RemoveAll(stateDir)

		err := os.MkdirAll(stateDir, 0700)
		if err != nil {
			op.Done(err)
			return err
		}

		criuMigrationArgs := instance.CriuMigrationArgs{
			Cmd:          liblxc.MIGRATE_DUMP,
			StateDir:     stateDir,
			Function:     "snapshot",
			Stop:         true,
			ActionScript: false,
			DumpDir:      "",
			PreDumpDir:   "",
		}

		// Checkpoint
		err = d.Migrate(&criuMigrationArgs)
		if err != nil {
			op.Done(err)
			return err
		}

		err = op.Wait()
		if err != nil && d.IsRunning() {
			return err
		}

		d.stateful = true
		err = d.state.Cluster.UpdateInstanceStatefulFlag(d.id, true)
		if err != nil {
			d.logger.Error("Failed stopping container", ctxMap)
			return err
		}

		d.logger.Info("Stopped container", ctxMap)
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceStopped.Event(d, nil))

		return nil
	} else if shared.PathExists(d.StatePath()) {
		os.RemoveAll(d.StatePath())
	}

	// Load the go-lxc struct
	if d.expandedConfig["raw.lxc"] != "" {
		err = d.initLXC(true)
		if err != nil {
			op.Done(err)
			return err
		}
	} else {
		err = d.initLXC(false)
		if err != nil {
			op.Done(err)
			return err
		}
	}

	// Load cgroup abstraction
	cg, err := d.cgroup(nil)
	if err != nil {
		op.Done(err)
		return err
	}

	// Fork-bomb mitigation, prevent forking from this point on
	if d.state.OS.CGInfo.Supports(cgroup.Pids, cg) {
		// Attempt to disable forking new processes
		cg.SetMaxProcesses(0)
	} else if d.state.OS.CGInfo.Supports(cgroup.Freezer, cg) {
		// Attempt to freeze the container
		freezer := make(chan bool, 1)
		go func() {
			d.Freeze()
			freezer <- true
		}()

		select {
		case <-freezer:
		case <-time.After(time.Second * 5):
			d.Unfreeze()
		}
	}

	err = d.c.Stop()
	if err != nil {
		op.Done(err)
		return err
	}

	err = op.Wait()
	if err != nil && d.IsRunning() {
		return err
	}

	if op.Action() == "stop" {
		d.logger.Info("Stopped container", ctxMap)
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceStopped.Event(d, nil))
	}

	return nil
}

// Shutdown stops the instance.
func (d *lxc) Shutdown(timeout time.Duration) error {
	d.logger.Debug("Shutdown started", log.Ctx{"timeout": timeout})
	defer d.logger.Debug("Shutdown finished", log.Ctx{"timeout": timeout})

	// Must be run prior to creating the operation lock.
	statusCode := d.statusCode()
	if !d.isRunningStatusCode(statusCode) {
		if statusCode == api.Error {
			return fmt.Errorf("The instance cannot be cleanly shutdown as in %s status", statusCode)
		}

		return fmt.Errorf("The instance is already stopped")
	}

	// Setup a new operation
	exists, op, err := operationlock.CreateWaitGet(d.id, "stop", []string{"restart"}, true, false)
	if err != nil {
		return err
	}
	if exists {
		// An existing matching operation has now succeeded, return.
		return nil
	}

	// If frozen, resume so the signal can be handled.
	if d.IsFrozen() {
		err := d.Unfreeze()
		if err != nil {
			return err
		}
	}

	ctxMap := log.Ctx{
		"action":    "shutdown",
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate,
		"timeout":   timeout}

	if op.Action() == "stop" {
		d.logger.Info("Shutting down container", ctxMap)
	}

	// Load the go-lxc struct
	if d.expandedConfig["raw.lxc"] != "" {
		err = d.initLXC(true)
		if err != nil {
			op.Done(err)
			return err
		}
	} else {
		err = d.initLXC(false)
		if err != nil {
			op.Done(err)
			return err
		}
	}

	err = d.c.Shutdown(timeout)
	if err != nil {
		op.Done(err)
		return err
	}

	err = op.Wait()
	if err != nil && d.IsRunning() {
		return err
	}

	if op.Action() == "stop" {
		d.logger.Info("Shut down container", ctxMap)
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceShutdown.Event(d, nil))
	}

	return nil
}

// Restart restart the instance.
func (d *lxc) Restart(timeout time.Duration) error {
	ctxMap := log.Ctx{
		"action":    "shutdown",
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate,
		"timeout":   timeout}

	d.logger.Info("Restarting container", ctxMap)

	err := d.restartCommon(d, timeout)
	if err != nil {
		return err
	}

	d.logger.Info("Restarted container", ctxMap)
	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRestarted.Event(d, nil))

	return nil
}

// onStopNS is triggered by LXC's stop hook once a container is shutdown but before the container's
// namespaces have been closed. The netns path of the stopped container is provided.
func (d *lxc) onStopNS(args map[string]string) error {
	target := args["target"]
	netns := args["netns"]

	// Validate target.
	if !shared.StringInSlice(target, []string{"stop", "reboot"}) {
		d.logger.Error("Container sent invalid target to OnStopNS", log.Ctx{"target": target})
		return fmt.Errorf("Invalid stop target %q", target)
	}

	// Create/pick up operation, but don't complete it as we leave operation running for the onStop hook below.
	_, _, err := d.onStopOperationSetup(target)
	if err != nil {
		return err
	}

	// Clean up devices.
	d.cleanupDevices(false, netns)

	return nil
}

// onStop is triggered by LXC's post-stop hook once a container is shutdown and after the
// container's namespaces have been closed.
func (d *lxc) onStop(args map[string]string) error {
	target := args["target"]

	// Validate target
	if !shared.StringInSlice(target, []string{"stop", "reboot"}) {
		d.logger.Error("Container sent invalid target to OnStop", log.Ctx{"target": target})
		return fmt.Errorf("Invalid stop target: %s", target)
	}

	// Create/pick up operation.
	op, instanceInitiated, err := d.onStopOperationSetup(target)
	if err != nil {
		return err
	}

	// Make sure we can't call go-lxc functions by mistake
	d.fromHook = true

	// Record power state
	err = d.state.Cluster.UpdateInstancePowerState(d.id, "STOPPED")
	if err != nil {
		err = errors.Wrap(err, "Failed to set container state")
		op.Done(err)
		return err
	}

	go func(d *lxc, target string, op *operationlock.InstanceOperation) {
		d.fromHook = false
		err = nil

		// Unlock on return
		defer op.Done(nil)

		// Wait for other post-stop actions to be done and the container actually stopping.
		d.IsRunning()
		d.logger.Debug("Container stopped, cleaning up")

		// Clean up devices.
		d.cleanupDevices(false, "")

		// Remove directory ownership (to avoid issue if uidmap is re-used)
		err := os.Chown(d.Path(), 0, 0)
		if err != nil {
			op.Done(errors.Wrap(err, "Failed clearing ownership"))
			return
		}

		err = os.Chmod(d.Path(), 0100)
		if err != nil {
			op.Done(errors.Wrap(err, "Failed clearing permissions"))
			return
		}

		// Stop the storage for this container
		_, err = d.unmount()
		if err != nil {
			op.Done(errors.Wrap(err, "Failed unmounting container"))
			return
		}

		// Unload the apparmor profile
		err = apparmor.InstanceUnload(d.state, d)
		if err != nil {
			op.Done(errors.Wrap(err, "Failed to destroy apparmor namespace"))
			return
		}

		// Clean all the unix devices
		err = d.removeUnixDevices()
		if err != nil {
			op.Done(errors.Wrap(err, "Failed to remove unix devices"))
			return
		}

		// Clean all the disk devices
		err = d.removeDiskDevices()
		if err != nil {
			op.Done(errors.Wrap(err, "Failed to remove disk devices"))
			return
		}

		// Log and emit lifecycle if not user triggered
		if instanceInitiated {
			ctxMap := log.Ctx{
				"action":    target,
				"created":   d.creationDate,
				"ephemeral": d.ephemeral,
				"used":      d.lastUsedDate,
				"stateful":  false,
			}

			d.logger.Info("Shut down container", ctxMap)
			d.state.Events.SendLifecycle(d.project, lifecycle.InstanceShutdown.Event(d, nil))
		}

		// Reboot the container
		if target == "reboot" {
			// Start the container again
			err = d.Start(false)
			if err != nil {
				op.Done(errors.Wrap(err, "Failed restarting container"))
				return
			}
			d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRestarted.Event(d, nil))

			return
		}

		// Trigger a rebalance
		cgroup.TaskSchedulerTrigger("container", d.name, "stopped")

		// Destroy ephemeral containers
		if d.ephemeral {
			err = d.Delete(true)
			if err != nil {
				op.Done(errors.Wrap(err, "Failed deleting ephemeral container"))
				return
			}
		}
	}(d, target, op)

	return nil
}

// cleanupDevices performs any needed device cleanup steps when container is stopped.
// Accepts a stopHookNetnsPath argument which is required when run from the onStopNS hook before the
// container's network namespace is unmounted (which is required for NIC device cleanup).
func (d *lxc) cleanupDevices(instanceRunning bool, stopHookNetnsPath string) {
	for _, dev := range d.expandedDevices.Reversed() {
		// Only stop NIC devices when run from the onStopNS hook, and stop all other devices when run from
		// the onStop hook. This way disk devices are stopped after the instance has been fully stopped.
		if (stopHookNetnsPath != "" && dev.Config["type"] != "nic") || (stopHookNetnsPath == "" && dev.Config["type"] == "nic") {
			continue
		}

		// Use the device interface if device supports it.
		err := d.deviceStop(dev.Name, dev.Config, instanceRunning, stopHookNetnsPath)
		if err == device.ErrUnsupportedDevType {
			continue
		} else if err != nil {
			d.logger.Error("Failed to stop device", log.Ctx{"devName": dev.Name, "err": err})
		}
	}
}

// Freeze functions.
func (d *lxc) Freeze() error {
	ctxMap := log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate}

	// Check that we're running
	if !d.IsRunning() {
		return fmt.Errorf("The container isn't running")
	}

	cg, err := d.cgroup(nil)
	if err != nil {
		return err
	}

	// Check if the CGroup is available
	if !d.state.OS.CGInfo.Supports(cgroup.Freezer, cg) {
		d.logger.Info("Unable to freeze container (lack of kernel support)", ctxMap)
		return nil
	}

	// Check that we're not already frozen
	if d.IsFrozen() {
		return fmt.Errorf("The container is already frozen")
	}

	d.logger.Info("Freezing container", ctxMap)

	// Load the go-lxc struct
	err = d.initLXC(false)
	if err != nil {
		ctxMap["err"] = err
		d.logger.Error("Failed freezing container", ctxMap)
		return err
	}

	err = d.c.Freeze()
	if err != nil {
		ctxMap["err"] = err
		d.logger.Error("Failed freezing container", ctxMap)
		return err
	}

	d.logger.Info("Froze container", ctxMap)
	d.state.Events.SendLifecycle(d.project, lifecycle.InstancePaused.Event(d, nil))

	return err
}

// Unfreeze unfreezes the instance.
func (d *lxc) Unfreeze() error {
	ctxMap := log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate}

	// Check that we're running
	if !d.IsRunning() {
		return fmt.Errorf("The container isn't running")
	}

	cg, err := d.cgroup(nil)
	if err != nil {
		return err
	}

	// Check if the CGroup is available
	if !d.state.OS.CGInfo.Supports(cgroup.Freezer, cg) {
		d.logger.Info("Unable to unfreeze container (lack of kernel support)", ctxMap)
		return nil
	}

	// Check that we're frozen
	if !d.IsFrozen() {
		return fmt.Errorf("The container is already running")
	}

	d.logger.Info("Unfreezing container", ctxMap)

	// Load the go-lxc struct
	err = d.initLXC(false)
	if err != nil {
		d.logger.Error("Failed unfreezing container", ctxMap)
		return err
	}

	err = d.c.Unfreeze()
	if err != nil {
		d.logger.Error("Failed unfreezing container", ctxMap)
	}

	d.logger.Info("Unfroze container", ctxMap)
	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceResumed.Event(d, nil))

	return err
}

// Get lxc container state, with 1 second timeout.
// If we don't get a reply, assume the lxc monitor is unresponsive.
func (d *lxc) getLxcState() (liblxc.State, error) {
	if d.IsSnapshot() {
		return liblxc.StateMap["STOPPED"], nil
	}

	// Load the go-lxc struct
	err := d.initLXC(false)
	if err != nil {
		return liblxc.StateMap["STOPPED"], err
	}

	if d.c == nil {
		return liblxc.StateMap["STOPPED"], nil
	}

	monitor := make(chan liblxc.State, 1)

	go func(c *liblxc.Container) {
		monitor <- c.State()
	}(d.c)

	select {
	case state := <-monitor:
		return state, nil
	case <-time.After(5 * time.Second):
		return liblxc.StateMap["FROZEN"], fmt.Errorf("Monitor is unresponsive")
	}
}

// Render renders the state of the instance.
func (d *lxc) Render(options ...func(response interface{}) error) (interface{}, interface{}, error) {
	// Ignore err as the arch string on error is correct (unknown)
	architectureName, _ := osarch.ArchitectureName(d.architecture)

	if d.IsSnapshot() {
		// Prepare the ETag
		etag := []interface{}{d.expiryDate}

		snapState := api.InstanceSnapshot{
			CreatedAt:       d.creationDate,
			ExpandedConfig:  d.expandedConfig,
			ExpandedDevices: d.expandedDevices.CloneNative(),
			LastUsedAt:      d.lastUsedDate,
			Name:            strings.SplitN(d.name, "/", 2)[1],
			Stateful:        d.stateful,
			Size:            -1, // Default to uninitialised/error state (0 means no CoW usage).
		}
		snapState.Architecture = architectureName
		snapState.Config = d.localConfig
		snapState.Devices = d.localDevices.CloneNative()
		snapState.Ephemeral = d.ephemeral
		snapState.Profiles = d.profiles
		snapState.ExpiresAt = d.expiryDate

		for _, option := range options {
			err := option(&snapState)
			if err != nil {
				return nil, nil, err
			}
		}

		return &snapState, etag, nil
	}

	// Prepare the ETag
	etag := []interface{}{d.architecture, d.localConfig, d.localDevices, d.ephemeral, d.profiles}

	statusCode := d.statusCode()
	instState := api.Instance{
		ExpandedConfig:  d.expandedConfig,
		ExpandedDevices: d.expandedDevices.CloneNative(),
		Name:            d.name,
		Status:          statusCode.String(),
		StatusCode:      statusCode,
		Location:        d.node,
		Type:            d.Type().String(),
	}

	instState.Description = d.description
	instState.Architecture = architectureName
	instState.Config = d.localConfig
	instState.CreatedAt = d.creationDate
	instState.Devices = d.localDevices.CloneNative()
	instState.Ephemeral = d.ephemeral
	instState.LastUsedAt = d.lastUsedDate
	instState.Profiles = d.profiles
	instState.Stateful = d.stateful

	for _, option := range options {
		err := option(&instState)
		if err != nil {
			return nil, nil, err
		}
	}

	return &instState, etag, nil
}

// RenderFull renders the full state of the instance.
func (d *lxc) RenderFull() (*api.InstanceFull, interface{}, error) {
	if d.IsSnapshot() {
		return nil, nil, fmt.Errorf("RenderFull only works with containers")
	}

	// Get the Container struct
	base, etag, err := d.Render()
	if err != nil {
		return nil, nil, err
	}

	// Convert to ContainerFull
	ct := api.InstanceFull{Instance: *base.(*api.Instance)}

	// Add the ContainerState
	ct.State, err = d.renderState(ct.StatusCode)
	if err != nil {
		return nil, nil, err
	}

	// Add the ContainerSnapshots
	snaps, err := d.Snapshots()
	if err != nil {
		return nil, nil, err
	}

	for _, snap := range snaps {
		render, _, err := snap.Render()
		if err != nil {
			return nil, nil, err
		}

		if ct.Snapshots == nil {
			ct.Snapshots = []api.InstanceSnapshot{}
		}

		ct.Snapshots = append(ct.Snapshots, *render.(*api.InstanceSnapshot))
	}

	// Add the ContainerBackups
	backups, err := d.Backups()
	if err != nil {
		return nil, nil, err
	}

	for _, backup := range backups {
		render := backup.Render()

		if ct.Backups == nil {
			ct.Backups = []api.InstanceBackup{}
		}

		ct.Backups = append(ct.Backups, *render)
	}

	return &ct, etag, nil
}

// renderState renders just the running state of the instance.
func (d *lxc) renderState(statusCode api.StatusCode) (*api.InstanceState, error) {
	status := api.InstanceState{
		Status:     statusCode.String(),
		StatusCode: statusCode,
	}

	if d.isRunningStatusCode(statusCode) {
		pid := d.InitPID()
		status.CPU = d.cpuState()
		status.Memory = d.memoryState()
		status.Network = d.networkState()
		status.Pid = int64(pid)
		status.Processes = d.processesState()
	}

	status.Disk = d.diskState()

	return &status, nil
}

// RenderState renders just the running state of the instance.
func (d *lxc) RenderState() (*api.InstanceState, error) {
	return d.renderState(d.statusCode())
}

// Snapshot takes a new snapshot.
func (d *lxc) Snapshot(name string, expiry time.Time, stateful bool) error {
	// Deal with state.
	if stateful {
		// Quick checks.
		if !d.IsRunning() {
			return fmt.Errorf("Unable to create a stateful snapshot. The instance isn't running")
		}

		_, err := exec.LookPath("criu")
		if err != nil {
			return fmt.Errorf("Unable to create a stateful snapshot. CRIU isn't installed")
		}

		/* TODO: ideally we would freeze here and unfreeze below after
		 * we've copied the filesystem, to make sure there are no
		 * changes by the container while snapshotting. Unfortunately
		 * there is abug in CRIU where it doesn't leave the container
		 * in the same state it found it w.r.t. freezing, i.e. CRIU
		 * freezes too, and then /always/ thaws, even if the container
		 * was frozen. Until that's fixed, all calls to Unfreeze()
		 * after snapshotting will fail.
		 */
		criuMigrationArgs := instance.CriuMigrationArgs{
			Cmd:          liblxc.MIGRATE_DUMP,
			StateDir:     d.StatePath(),
			Function:     "snapshot",
			Stop:         false,
			ActionScript: false,
			DumpDir:      "",
			PreDumpDir:   "",
		}

		// Create the state path and Make sure we don't keep state around after the snapshot has been made.
		err = os.MkdirAll(d.StatePath(), 0700)
		if err != nil {
			return err
		}

		defer os.RemoveAll(d.StatePath())

		// Dump the state.
		err = d.Migrate(&criuMigrationArgs)
		if err != nil {
			return err
		}
	}

	return d.snapshotCommon(d, name, expiry, stateful)
}

// Restore restores a snapshot.
func (d *lxc) Restore(sourceContainer instance.Instance, stateful bool) error {
	var ctxMap log.Ctx

	op, err := operationlock.Create(d.id, "restore", false, false)
	if err != nil {
		return errors.Wrap(err, "Create restore operation")
	}
	defer op.Done(nil)

	// Stop the container.
	wasRunning := false
	if d.IsRunning() {
		wasRunning = true

		ephemeral := d.IsEphemeral()
		if ephemeral {
			// Unset ephemeral flag.
			args := db.InstanceArgs{
				Architecture: d.Architecture(),
				Config:       d.LocalConfig(),
				Description:  d.Description(),
				Devices:      d.LocalDevices(),
				Ephemeral:    false,
				Profiles:     d.Profiles(),
				Project:      d.Project(),
				Type:         d.Type(),
				Snapshot:     d.IsSnapshot(),
			}

			err := d.Update(args, false)
			if err != nil {
				op.Done(err)
				return err
			}

			// On function return, set the flag back on.
			defer func() {
				args.Ephemeral = ephemeral
				d.Update(args, false)
			}()
		}

		// This will unmount the container storage.
		err := d.Stop(false)
		if err != nil {
			op.Done(err)
			return err
		}

		// Refresh the operation as that one is now complete.
		op, err = operationlock.Create(d.id, "restore", false, false)
		if err != nil {
			return errors.Wrap(err, "Create restore operation")
		}
		defer op.Done(nil)
	}

	ctxMap = log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate,
		"source":    sourceContainer.Name()}

	d.logger.Info("Restoring container", ctxMap)

	// Initialize storage interface for the container and mount the rootfs for criu state check.
	pool, err := storagePools.GetPoolByInstance(d.state, d)
	if err != nil {
		op.Done(err)
		return err
	}

	d.logger.Debug("Mounting instance to check for CRIU state path existence")

	// Ensure that storage is mounted for state path checks and for backup.yaml updates.
	_, err = pool.MountInstance(d, nil)
	if err != nil {
		op.Done(err)
		return err
	}

	// Check for CRIU if necessary, before doing a bunch of filesystem manipulations.
	// Requires container be mounted to check StatePath exists.
	if shared.PathExists(d.StatePath()) {
		_, err := exec.LookPath("criu")
		if err != nil {
			err = fmt.Errorf("Failed to restore container state. CRIU isn't installed")
			op.Done(err)
			return err
		}
	}

	_, err = pool.UnmountInstance(d, nil)
	if err != nil {
		op.Done(err)
		return err
	}

	// Restore the rootfs.
	err = pool.RestoreInstanceSnapshot(d, sourceContainer, nil)
	if err != nil {
		op.Done(err)
		return err
	}

	// Restore the configuration.
	args := db.InstanceArgs{
		Architecture: sourceContainer.Architecture(),
		Config:       sourceContainer.LocalConfig(),
		Description:  sourceContainer.Description(),
		Devices:      sourceContainer.LocalDevices(),
		Ephemeral:    sourceContainer.IsEphemeral(),
		Profiles:     sourceContainer.Profiles(),
		Project:      sourceContainer.Project(),
		Type:         sourceContainer.Type(),
		Snapshot:     sourceContainer.IsSnapshot(),
	}

	// Don't pass as user-requested as there's no way to fix a bad config.
	// This will call d.UpdateBackupFile() to ensure snapshot list is up to date.
	err = d.Update(args, false)
	if err != nil {
		op.Done(err)
		return err
	}

	// If the container wasn't running but was stateful, should we restore it as running?
	if stateful == true {
		if !shared.PathExists(d.StatePath()) {
			err = fmt.Errorf("Stateful snapshot restore requested by snapshot is stateless")
			op.Done(err)
			return err
		}

		d.logger.Debug("Performing stateful restore", ctxMap)
		d.stateful = true

		criuMigrationArgs := instance.CriuMigrationArgs{
			Cmd:          liblxc.MIGRATE_RESTORE,
			StateDir:     d.StatePath(),
			Function:     "snapshot",
			Stop:         false,
			ActionScript: false,
			DumpDir:      "",
			PreDumpDir:   "",
		}

		// Checkpoint.
		err := d.Migrate(&criuMigrationArgs)
		if err != nil {
			op.Done(err)
			return err
		}

		// Remove the state from the parent container; we only keep this in snapshots.
		err2 := os.RemoveAll(d.StatePath())
		if err2 != nil && !os.IsNotExist(err) {
			op.Done(err)
			return err
		}

		if err != nil {
			op.Done(err)
			return err
		}

		d.logger.Debug("Performed stateful restore", ctxMap)
		d.logger.Info("Restored container", ctxMap)
		return nil
	}

	// Restart the container.
	if wasRunning {
		d.logger.Debug("Starting instance after snapshot restore")
		err = d.Start(false)
		if err != nil {
			op.Done(err)
			return err
		}
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRestored.Event(d, map[string]interface{}{"snapshot": sourceContainer.Name()}))
	d.logger.Info("Restored container", ctxMap)

	return nil
}

func (d *lxc) cleanup() {
	// Unmount any leftovers
	d.removeUnixDevices()
	d.removeDiskDevices()

	// Remove the security profiles
	apparmor.InstanceDelete(d.state, d)
	seccomp.DeleteProfile(d)

	// Remove the devices path
	os.Remove(d.DevicesPath())

	// Remove the shmounts path
	os.RemoveAll(d.ShmountsPath())
}

// Delete deletes the instance.
func (d *lxc) Delete(force bool) error {
	ctxMap := log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate}

	d.logger.Info("Deleting container", ctxMap)

	if !force && shared.IsTrue(d.expandedConfig["security.protection.delete"]) && !d.IsSnapshot() {
		err := fmt.Errorf("Container is protected")
		d.logger.Warn("Failed to delete container", log.Ctx{"err": err})
		return err
	}

	// Delete any persistent warnings for instance.
	err := d.warningsDelete()
	if err != nil {
		return err
	}

	isImport := false
	pool, err := storagePools.GetPoolByInstance(d.state, d)
	if err != nil && errors.Cause(err) != db.ErrNoSuchObject {
		return err
	} else if pool != nil {
		// Check if we're dealing with "lxd import".
		// "lxd import" is used for disaster recovery, where you already have a container
		// and snapshots on disk but no DB entry. As such if something has gone wrong during
		// the creation of the instance and we are now being asked to delete the instance,
		// we should not remove the storage volumes themselves as this would cause data loss.
		cName, _, _ := shared.InstanceGetParentAndSnapshotName(d.Name())
		importingFilePath := storagePools.InstanceImportingFilePath(d.Type(), pool.Name(), d.Project(), cName)
		if shared.PathExists(importingFilePath) {
			isImport = true
		}

		if d.IsSnapshot() {
			if !isImport {
				// Remove snapshot volume and database record.
				err = pool.DeleteInstanceSnapshot(d, nil)
				if err != nil {
					return err
				}
			} else {
				d.logger.Info("Skipping snapshot volume delete")
			}
		} else {
			// Remove all snapshots by initialising each snapshot as an Instance and
			// calling its Delete function.
			err := instance.DeleteSnapshots(d.state, d.Project(), d.Name())
			if err != nil {
				d.logger.Error("Failed to delete instance snapshots", log.Ctx{"err": err})
				return err
			}

			if !isImport {
				// Remove the storage volume, snapshot volumes and database records.
				err = pool.DeleteInstance(d, nil)
				if err != nil {
					return err
				}
			} else {
				d.logger.Info("Skipping instance volume delete")
			}
		}
	}

	// Perform other cleanup steps if not snapshot.
	if !d.IsSnapshot() {
		// Remove all backups.
		backups, err := d.Backups()
		if err != nil {
			return err
		}

		for _, backup := range backups {
			err = backup.Delete()
			if err != nil {
				return err
			}
		}

		// Delete the MAAS entry.
		err = d.maasDelete(d)
		if err != nil {
			d.logger.Error("Failed deleting container MAAS record", log.Ctx{"err": err})
			return err
		}

		// Remove devices from container.
		for k, m := range d.expandedDevices {
			err = d.deviceRemove(k, m, false)
			if err != nil && err != device.ErrUnsupportedDevType {
				return errors.Wrapf(err, "Failed to remove device %q", k)
			}
		}

		// Clean things up.
		d.cleanup()
	}

	// Remove the database record of the instance or snapshot instance.
	if err := d.state.Cluster.DeleteInstance(d.project, d.Name()); err != nil {
		d.logger.Error("Failed deleting container entry", log.Ctx{"err": err})
		return err
	}

	// If dealing with a snapshot, refresh the backup file on the parent.
	if d.IsSnapshot() && !isImport {
		parentName, _, _ := shared.InstanceGetParentAndSnapshotName(d.name)

		// Load the parent.
		parent, err := instance.LoadByProjectAndName(d.state, d.project, parentName)
		if err != nil {
			return errors.Wrap(err, "Invalid parent")
		}

		// Update the backup file.
		err = parent.UpdateBackupFile()
		if err != nil {
			return err
		}
	}

	d.logger.Info("Deleted container", ctxMap)
	if d.snapshot {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotDeleted.Event(d, nil))
	} else {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceDeleted.Event(d, nil))
	}

	return nil
}

// Rename renames the instance. Accepts an argument to enable applying deferred TemplateTriggerRename.
func (d *lxc) Rename(newName string, applyTemplateTrigger bool) error {
	oldName := d.Name()
	ctxMap := log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate,
		"newname":   newName}

	d.logger.Info("Renaming container", ctxMap)

	// Quick checks.
	err := instance.ValidName(newName, d.IsSnapshot())
	if err != nil {
		return err
	}

	if d.IsRunning() {
		return fmt.Errorf("Renaming of running container not allowed")
	}

	// Clean things up.
	d.cleanup()

	pool, err := storagePools.GetPoolByInstance(d.state, d)
	if err != nil {
		return errors.Wrap(err, "Failed loading instance storage pool")
	}

	if d.IsSnapshot() {
		_, newSnapName, _ := shared.InstanceGetParentAndSnapshotName(newName)
		err = pool.RenameInstanceSnapshot(d, newSnapName, nil)
		if err != nil {
			return errors.Wrap(err, "Rename instance snapshot")
		}
	} else {
		err = pool.RenameInstance(d, newName, nil)
		if err != nil {
			return errors.Wrap(err, "Rename instance")
		}

		if applyTemplateTrigger {
			err = d.DeferTemplateApply(instance.TemplateTriggerRename)
			if err != nil {
				return err
			}
		}
	}

	if !d.IsSnapshot() {
		// Rename all the instance snapshot database entries.
		results, err := d.state.Cluster.GetInstanceSnapshotsNames(d.project, oldName)
		if err != nil {
			d.logger.Error("Failed to get container snapshots", ctxMap)
			return errors.Wrapf(err, "Failed to get container snapshots")
		}

		for _, sname := range results {
			// Rename the snapshot.
			oldSnapName := strings.SplitN(sname, shared.SnapshotDelimiter, 2)[1]
			baseSnapName := filepath.Base(sname)
			err := d.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
				return tx.RenameInstanceSnapshot(d.project, oldName, oldSnapName, baseSnapName)
			})
			if err != nil {
				d.logger.Error("Failed renaming snapshot", ctxMap)
				return errors.Wrapf(err, "Failed renaming snapshot")
			}
		}
	}

	// Rename the instance database entry.
	err = d.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		if d.IsSnapshot() {
			oldParts := strings.SplitN(oldName, shared.SnapshotDelimiter, 2)
			newParts := strings.SplitN(newName, shared.SnapshotDelimiter, 2)
			return tx.RenameInstanceSnapshot(d.project, oldParts[0], oldParts[1], newParts[1])
		}

		return tx.RenameInstance(d.project, oldName, newName)
	})
	if err != nil {
		d.logger.Error("Failed renaming container", ctxMap)
		return errors.Wrapf(err, "Failed renaming container")
	}

	// Rename the logging path.
	newFullName := project.Instance(d.Project(), d.Name())
	os.RemoveAll(shared.LogPath(newFullName))
	if shared.PathExists(d.LogPath()) {
		err := os.Rename(d.LogPath(), shared.LogPath(newFullName))
		if err != nil {
			d.logger.Error("Failed renaming container", ctxMap)
			return errors.Wrapf(err, "Failed renaming container")
		}
	}

	// Rename the MAAS entry.
	if !d.IsSnapshot() {
		err = d.maasRename(d, newName)
		if err != nil {
			return err
		}
	}

	revert := revert.New()
	defer revert.Fail()

	// Set the new name in the struct.
	d.name = newName
	revert.Add(func() { d.name = oldName })

	// Rename the backups.
	backups, err := d.Backups()
	if err != nil {
		return err
	}

	for _, backup := range backups {
		b := backup
		oldName := b.Name()
		backupName := strings.Split(oldName, "/")[1]
		newName := fmt.Sprintf("%s/%s", newName, backupName)

		err = b.Rename(newName)
		if err != nil {
			return err
		}

		revert.Add(func() { b.Rename(oldName) })
	}

	// Invalidate the go-lxc cache.
	if d.c != nil {
		d.c.Release()
		d.c = nil
	}

	d.cConfig = false

	// Update lease files.
	network.UpdateDNSMasqStatic(d.state, "")

	err = d.UpdateBackupFile()
	if err != nil {
		return err
	}

	d.logger.Info("Renamed container", ctxMap)
	if d.snapshot {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotRenamed.Event(d, map[string]interface{}{"old_name": oldName}))
	} else {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRenamed.Event(d, map[string]interface{}{"old_name": oldName}))
	}

	revert.Success()
	return nil
}

// CGroupSet sets a cgroup value for the instance.
func (d *lxc) CGroupSet(key string, value string) error {
	// Load the go-lxc struct
	err := d.initLXC(false)
	if err != nil {
		return err
	}

	// Make sure the container is running
	if !d.IsRunning() {
		return fmt.Errorf("Can't set cgroups on a stopped container")
	}

	err = d.c.SetCgroupItem(key, value)
	if err != nil {
		return fmt.Errorf("Failed to set cgroup %s=\"%s\": %s", key, value, err)
	}

	return nil
}

// Update applies updated config.
func (d *lxc) Update(args db.InstanceArgs, userRequested bool) error {
	// Set sane defaults for unset keys
	if args.Project == "" {
		args.Project = project.Default
	}

	if args.Architecture == 0 {
		args.Architecture = d.architecture
	}

	if args.Config == nil {
		args.Config = map[string]string{}
	}

	if args.Devices == nil {
		args.Devices = deviceConfig.Devices{}
	}

	if args.Profiles == nil {
		args.Profiles = []string{}
	}

	if userRequested {
		// Validate the new config
		err := instance.ValidConfig(d.state.OS, args.Config, false, instancetype.Container)
		if err != nil {
			return errors.Wrap(err, "Invalid config")
		}

		// Validate the new devices without using expanded devices validation (expensive checks disabled).
		err = instance.ValidDevices(d.state, d.state.Cluster, d.Project(), d.Type(), args.Devices, false)
		if err != nil {
			return errors.Wrap(err, "Invalid devices")
		}
	}

	// Validate the new profiles
	profiles, err := d.state.Cluster.GetProfileNames(args.Project)
	if err != nil {
		return errors.Wrap(err, "Failed to get profiles")
	}

	checkedProfiles := []string{}
	for _, profile := range args.Profiles {
		if !shared.StringInSlice(profile, profiles) {
			return fmt.Errorf("Requested profile '%s' doesn't exist", profile)
		}

		if shared.StringInSlice(profile, checkedProfiles) {
			return fmt.Errorf("Duplicate profile found in request")
		}

		checkedProfiles = append(checkedProfiles, profile)
	}

	// Validate the new architecture
	if args.Architecture != 0 {
		_, err = osarch.ArchitectureName(args.Architecture)
		if err != nil {
			return fmt.Errorf("Invalid architecture id: %s", err)
		}
	}

	// Get a copy of the old configuration
	oldDescription := d.Description()
	oldArchitecture := 0
	err = shared.DeepCopy(&d.architecture, &oldArchitecture)
	if err != nil {
		return err
	}

	oldEphemeral := false
	err = shared.DeepCopy(&d.ephemeral, &oldEphemeral)
	if err != nil {
		return err
	}

	oldExpandedDevices := deviceConfig.Devices{}
	err = shared.DeepCopy(&d.expandedDevices, &oldExpandedDevices)
	if err != nil {
		return err
	}

	oldExpandedConfig := map[string]string{}
	err = shared.DeepCopy(&d.expandedConfig, &oldExpandedConfig)
	if err != nil {
		return err
	}

	oldLocalDevices := deviceConfig.Devices{}
	err = shared.DeepCopy(&d.localDevices, &oldLocalDevices)
	if err != nil {
		return err
	}

	oldLocalConfig := map[string]string{}
	err = shared.DeepCopy(&d.localConfig, &oldLocalConfig)
	if err != nil {
		return err
	}

	oldProfiles := []string{}
	err = shared.DeepCopy(&d.profiles, &oldProfiles)
	if err != nil {
		return err
	}

	oldExpiryDate := d.expiryDate

	// Define a function which reverts everything.  Defer this function
	// so that it doesn't need to be explicitly called in every failing
	// return path.  Track whether or not we want to undo the changes
	// using a closure.
	undoChanges := true
	defer func() {
		if undoChanges {
			d.description = oldDescription
			d.architecture = oldArchitecture
			d.ephemeral = oldEphemeral
			d.expandedConfig = oldExpandedConfig
			d.expandedDevices = oldExpandedDevices
			d.localConfig = oldLocalConfig
			d.localDevices = oldLocalDevices
			d.profiles = oldProfiles
			d.expiryDate = oldExpiryDate
			if d.c != nil {
				d.c.Release()
				d.c = nil
			}
			d.cConfig = false
			d.initLXC(true)
			cgroup.TaskSchedulerTrigger("container", d.name, "changed")
		}
	}()

	// Apply the various changes
	d.description = args.Description
	d.architecture = args.Architecture
	d.ephemeral = args.Ephemeral
	d.localConfig = args.Config
	d.localDevices = args.Devices
	d.profiles = args.Profiles
	d.expiryDate = args.ExpiryDate

	// Expand the config and refresh the LXC config
	err = d.expandConfig(nil)
	if err != nil {
		return errors.Wrap(err, "Expand config")
	}

	err = d.expandDevices(nil)
	if err != nil {
		return errors.Wrap(err, "Expand devices")
	}

	// Diff the configurations
	changedConfig := []string{}
	for key := range oldExpandedConfig {
		if oldExpandedConfig[key] != d.expandedConfig[key] {
			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	for key := range d.expandedConfig {
		if oldExpandedConfig[key] != d.expandedConfig[key] {
			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	// Diff the devices
	removeDevices, addDevices, updateDevices, allUpdatedKeys := oldExpandedDevices.Update(d.expandedDevices, func(oldDevice deviceConfig.Device, newDevice deviceConfig.Device) []string {
		// This function needs to return a list of fields that are excluded from differences
		// between oldDevice and newDevice. The result of this is that as long as the
		// devices are otherwise identical except for the fields returned here, then the
		// device is considered to be being "updated" rather than "added & removed".
		oldDevType, err := device.LoadByType(d.state, d.Project(), oldDevice)
		if err != nil {
			return []string{} // Couldn't create Device, so this cannot be an update.
		}

		newDevType, err := device.LoadByType(d.state, d.Project(), newDevice)
		if err != nil {
			return []string{} // Couldn't create Device, so this cannot be an update.
		}

		return newDevType.UpdatableFields(oldDevType)
	})

	if userRequested {
		// Do some validation of the config diff
		err = instance.ValidConfig(d.state.OS, d.expandedConfig, false, instancetype.Container)
		if err != nil {
			return errors.Wrap(err, "Invalid expanded config")
		}

		// Do full expanded validation of the devices diff.
		err = instance.ValidDevices(d.state, d.state.Cluster, d.Project(), d.Type(), d.expandedDevices, true)
		if err != nil {
			return errors.Wrap(err, "Invalid expanded devices")
		}
	}

	// Run through initLXC to catch anything we missed
	if userRequested {
		if d.c != nil {
			d.c.Release()
			d.c = nil
		}

		d.cConfig = false
		err = d.initLXC(true)
		if err != nil {
			return errors.Wrap(err, "Initialize LXC")
		}
	}

	cg, err := d.cgroup(nil)
	if err != nil {
		return err
	}

	// If apparmor changed, re-validate the apparmor profile (even if not running).
	if shared.StringInSlice("raw.apparmor", changedConfig) || shared.StringInSlice("security.nesting", changedConfig) {
		err = apparmor.InstanceValidate(d.state, d)
		if err != nil {
			return errors.Wrap(err, "Parse AppArmor profile")
		}
	}

	if shared.StringInSlice("security.idmap.isolated", changedConfig) || shared.StringInSlice("security.idmap.base", changedConfig) || shared.StringInSlice("security.idmap.size", changedConfig) || shared.StringInSlice("raw.idmap", changedConfig) || shared.StringInSlice("security.privileged", changedConfig) {
		var idmap *idmap.IdmapSet
		base := int64(0)
		if !d.IsPrivileged() {
			// update the idmap
			idmap, base, err = findIdmap(
				d.state,
				d.Name(),
				d.expandedConfig["security.idmap.isolated"],
				d.expandedConfig["security.idmap.base"],
				d.expandedConfig["security.idmap.size"],
				d.expandedConfig["raw.idmap"],
			)
			if err != nil {
				return errors.Wrap(err, "Failed to get ID map")
			}
		}

		var jsonIdmap string
		if idmap != nil {
			idmapBytes, err := json.Marshal(idmap.Idmap)
			if err != nil {
				return err
			}
			jsonIdmap = string(idmapBytes)
		} else {
			jsonIdmap = "[]"
		}
		d.localConfig["volatile.idmap.next"] = jsonIdmap
		d.localConfig["volatile.idmap.base"] = fmt.Sprintf("%v", base)

		// Invalid idmap cache
		d.idmapset = nil
	}

	isRunning := d.IsRunning()

	// Use the device interface to apply update changes.
	err = d.updateDevices(removeDevices, addDevices, updateDevices, oldExpandedDevices, isRunning, userRequested)
	if err != nil {
		return err
	}

	// Update MAAS (must run after the MAC addresses have been generated).
	updateMAAS := false
	for _, key := range []string{"maas.subnet.ipv4", "maas.subnet.ipv6", "ipv4.address", "ipv6.address"} {
		if shared.StringInSlice(key, allUpdatedKeys) {
			updateMAAS = true
			break
		}
	}

	if !d.IsSnapshot() && updateMAAS {
		err = d.maasUpdate(d, oldExpandedDevices.CloneNative())
		if err != nil {
			return err
		}
	}

	// Apply the live changes
	if isRunning {
		// Live update the container config
		for _, key := range changedConfig {
			value := d.expandedConfig[key]

			if key == "raw.apparmor" || key == "security.nesting" {
				// Update the AppArmor profile
				err = apparmor.InstanceLoad(d.state, d)
				if err != nil {
					return err
				}
			} else if key == "security.devlxd" {
				if value == "" || shared.IsTrue(value) {
					err = d.insertMount(shared.VarPath("devlxd"), "/dev/lxd", "none", unix.MS_BIND, idmap.IdmapStorageNone)
					if err != nil {
						return err
					}
				} else if d.FileExists("/dev/lxd") == nil {
					err = d.removeMount("/dev/lxd")
					if err != nil {
						return err
					}

					err = d.FileRemove("/dev/lxd")
					if err != nil {
						return err
					}
				}
			} else if key == "linux.kernel_modules" && value != "" {
				for _, module := range strings.Split(value, ",") {
					module = strings.TrimPrefix(module, " ")
					err := util.LoadModule(module)
					if err != nil {
						return fmt.Errorf("Failed to load kernel module '%s': %s", module, err)
					}
				}
			} else if key == "limits.disk.priority" {
				if !d.state.OS.CGInfo.Supports(cgroup.Blkio, cg) {
					continue
				}

				priorityInt := 5
				diskPriority := d.expandedConfig["limits.disk.priority"]
				if diskPriority != "" {
					priorityInt, err = strconv.Atoi(diskPriority)
					if err != nil {
						return err
					}
				}

				// Minimum valid value is 10
				priority := int64(priorityInt * 100)
				if priority == 0 {
					priority = 10
				}

				cg.SetBlkioWeight(priority)
				if err != nil {
					return err
				}
			} else if key == "limits.memory" || strings.HasPrefix(key, "limits.memory.") {
				// Skip if no memory CGroup
				if !d.state.OS.CGInfo.Supports(cgroup.Memory, cg) {
					continue
				}

				// Set the new memory limit
				memory := d.expandedConfig["limits.memory"]
				memoryEnforce := d.expandedConfig["limits.memory.enforce"]
				memorySwap := d.expandedConfig["limits.memory.swap"]
				var memoryInt int64

				// Parse memory
				if memory == "" {
					memoryInt = -1
				} else if strings.HasSuffix(memory, "%") {
					percent, err := strconv.ParseInt(strings.TrimSuffix(memory, "%"), 10, 64)
					if err != nil {
						return err
					}

					memoryTotal, err := shared.DeviceTotalMemory()
					if err != nil {
						return err
					}

					memoryInt = int64((memoryTotal / 100) * percent)
				} else {
					memoryInt, err = units.ParseByteSizeString(memory)
					if err != nil {
						return err
					}
				}

				// Store the old values for revert
				oldMemswLimit := int64(-1)
				if d.state.OS.CGInfo.Supports(cgroup.MemorySwap, cg) {
					oldMemswLimit, err = cg.GetMemorySwapLimit()
					if err != nil {
						oldMemswLimit = -1
					}
				}
				oldLimit, err := cg.GetMemoryLimit()
				if err != nil {
					oldLimit = -1
				}

				oldSoftLimit, err := cg.GetMemorySoftLimit()
				if err != nil {
					oldSoftLimit = -1
				}

				revertMemory := func() {
					if oldSoftLimit != -1 {
						cg.SetMemorySoftLimit(oldSoftLimit)
					}

					if oldLimit != -1 {
						cg.SetMemoryLimit(oldLimit)
					}

					if oldMemswLimit != -1 {
						cg.SetMemorySwapLimit(oldMemswLimit)
					}
				}

				// Reset everything
				if d.state.OS.CGInfo.Supports(cgroup.MemorySwap, cg) {
					err = cg.SetMemorySwapLimit(-1)
					if err != nil {
						revertMemory()
						return err
					}
				}

				err = cg.SetMemoryLimit(-1)
				if err != nil {
					revertMemory()
					return err
				}

				err = cg.SetMemorySoftLimit(-1)
				if err != nil {
					revertMemory()
					return err
				}

				// Set the new values
				if memoryEnforce == "soft" {
					// Set new limit
					err = cg.SetMemorySoftLimit(memoryInt)
					if err != nil {
						revertMemory()
						return err
					}
				} else {
					if d.state.OS.CGInfo.Supports(cgroup.MemorySwap, cg) && (memorySwap == "" || shared.IsTrue(memorySwap)) {
						err = cg.SetMemoryLimit(memoryInt)
						if err != nil {
							revertMemory()
							return err
						}

						err = cg.SetMemorySwapLimit(0)
						if err != nil {
							revertMemory()
							return err
						}
					} else {
						err = cg.SetMemoryLimit(memoryInt)
						if err != nil {
							revertMemory()
							return err
						}
					}

					// Set soft limit to value 10% less than hard limit
					err = cg.SetMemorySoftLimit(int64(float64(memoryInt) * 0.9))
					if err != nil {
						revertMemory()
						return err
					}
				}

				if !d.state.OS.CGInfo.Supports(cgroup.MemorySwappiness, cg) {
					continue
				}

				// Configure the swappiness
				if key == "limits.memory.swap" || key == "limits.memory.swap.priority" {
					memorySwap := d.expandedConfig["limits.memory.swap"]
					memorySwapPriority := d.expandedConfig["limits.memory.swap.priority"]
					if memorySwap != "" && !shared.IsTrue(memorySwap) {
						err = cg.SetMemorySwappiness(0)
						if err != nil {
							return err
						}
					} else {
						priority := 0
						if memorySwapPriority != "" {
							priority, err = strconv.Atoi(memorySwapPriority)
							if err != nil {
								return err
							}
						}

						err = cg.SetMemorySwappiness(int64(60 - 10 + priority))
						if err != nil {
							return err
						}
					}
				}
			} else if key == "limits.network.priority" {
				err := d.setNetworkPriority()
				if err != nil {
					return err
				}
			} else if key == "limits.cpu" {
				// Trigger a scheduler re-run
				cgroup.TaskSchedulerTrigger("container", d.name, "changed")
			} else if key == "limits.cpu.priority" || key == "limits.cpu.allowance" {
				// Skip if no cpu CGroup
				if !d.state.OS.CGInfo.Supports(cgroup.CPU, cg) {
					continue
				}

				// Apply new CPU limits
				cpuShares, cpuCfsQuota, cpuCfsPeriod, err := cgroup.ParseCPU(d.expandedConfig["limits.cpu.allowance"], d.expandedConfig["limits.cpu.priority"])
				if err != nil {
					return err
				}

				err = cg.SetCPUShare(cpuShares)
				if err != nil {
					return err
				}

				err = cg.SetCPUCfsLimit(cpuCfsPeriod, cpuCfsQuota)
				if err != nil {
					return err
				}
			} else if key == "limits.processes" {
				if !d.state.OS.CGInfo.Supports(cgroup.Pids, cg) {
					continue
				}

				if value == "" {
					err = cg.SetMaxProcesses(-1)
					if err != nil {
						return err
					}
				} else {
					valueInt, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						return err
					}

					err = cg.SetMaxProcesses(valueInt)
					if err != nil {
						return err
					}
				}
			} else if strings.HasPrefix(key, "limits.hugepages.") {
				if !d.state.OS.CGInfo.Supports(cgroup.Hugetlb, cg) {
					continue
				}

				pageType := ""

				switch key {
				case "limits.hugepages.64KB":
					pageType = "64KB"
				case "limits.hugepages.1MB":
					pageType = "1MB"
				case "limits.hugepages.2MB":
					pageType = "2MB"
				case "limits.hugepages.1GB":
					pageType = "1GB"
				}

				valueInt := int64(-1)
				if value != "" {
					valueInt, err = units.ParseByteSizeString(value)
					if err != nil {
						return err
					}
				}

				err = cg.SetHugepagesLimit(pageType, valueInt)
				if err != nil {
					return err
				}
			}
		}
	}

	// Finally, apply the changes to the database
	err = d.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Snapshots should update only their descriptions and expiry date.
		if d.IsSnapshot() {
			return tx.UpdateInstanceSnapshot(d.id, d.description, d.expiryDate)
		}

		object, err := tx.GetInstance(d.project, d.name)
		if err != nil {
			return err
		}

		object.Description = d.description
		object.Architecture = d.architecture
		object.Ephemeral = d.ephemeral
		object.ExpiryDate = d.expiryDate
		object.Config = d.localConfig
		object.Profiles = d.profiles
		object.Devices = d.localDevices.CloneNative()

		return tx.UpdateInstance(d.project, d.name, *object)
	})
	if err != nil {
		return errors.Wrap(err, "Failed to update database")
	}

	err = d.UpdateBackupFile()
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "Failed to write backup file")
	}

	// Send devlxd notifications
	if isRunning {
		// Config changes (only for user.* keys
		for _, key := range changedConfig {
			if !strings.HasPrefix(key, "user.") {
				continue
			}

			msg := map[string]string{
				"key":       key,
				"old_value": oldExpandedConfig[key],
				"value":     d.expandedConfig[key],
			}

			err = d.devlxdEventSend("config", msg)
			if err != nil {
				return err
			}
		}

		// Device changes
		for k, m := range removeDevices {
			msg := map[string]interface{}{
				"action": "removed",
				"name":   k,
				"config": m,
			}

			err = d.devlxdEventSend("device", msg)
			if err != nil {
				return err
			}
		}

		for k, m := range updateDevices {
			msg := map[string]interface{}{
				"action": "updated",
				"name":   k,
				"config": m,
			}

			err = d.devlxdEventSend("device", msg)
			if err != nil {
				return err
			}
		}

		for k, m := range addDevices {
			msg := map[string]interface{}{
				"action": "added",
				"name":   k,
				"config": m,
			}

			err = d.devlxdEventSend("device", msg)
			if err != nil {
				return err
			}
		}
	}

	// Success, update the closure to mark that the changes should be kept.
	undoChanges = false

	if userRequested {
		if d.snapshot {
			d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotUpdated.Event(d, nil))
		} else {
			d.state.Events.SendLifecycle(d.project, lifecycle.InstanceUpdated.Event(d, nil))
		}
	}

	return nil
}

func (d *lxc) updateDevices(removeDevices deviceConfig.Devices, addDevices deviceConfig.Devices, updateDevices deviceConfig.Devices, oldExpandedDevices deviceConfig.Devices, instanceRunning bool, userRequested bool) error {
	revert := revert.New()
	defer revert.Fail()

	// Remove devices in reverse order to how they were added.
	for _, dev := range removeDevices.Reversed() {
		if instanceRunning {
			err := d.deviceStop(dev.Name, dev.Config, instanceRunning, "")
			if err == device.ErrUnsupportedDevType {
				continue // No point in trying to remove device below.
			} else if err != nil {
				return errors.Wrapf(err, "Failed to stop device %q", dev.Name)
			}
		}

		err := d.deviceRemove(dev.Name, dev.Config, instanceRunning)
		if err != nil && err != device.ErrUnsupportedDevType {
			return errors.Wrapf(err, "Failed to remove device %q", dev.Name)
		}

		// Check whether we are about to add the same device back with updated config and
		// if not, or if the device type has changed, then remove all volatile keys for
		// this device (as its an actual removal or a device type change).
		err = d.deviceVolatileReset(dev.Name, dev.Config, addDevices[dev.Name])
		if err != nil {
			return errors.Wrapf(err, "Failed to reset volatile data for device %q", dev.Name)
		}
	}

	// Add devices in sorted order, this ensures that device mounts are added in path order.
	for _, dd := range addDevices.Sorted() {
		dev := dd // Local var for loop revert.
		err := d.deviceAdd(dev.Name, dev.Config, instanceRunning)
		if err == device.ErrUnsupportedDevType {
			continue // No point in trying to start device below.
		} else if err != nil {
			if userRequested {
				return errors.Wrapf(err, "Failed to add device %q", dev.Name)
			}

			// If update is non-user requested (i.e from a snapshot restore), there's nothing we can
			// do to fix the config and we don't want to prevent the snapshot restore so log and allow.
			d.logger.Error("Failed to add device, skipping as non-user requested", log.Ctx{"device": dev.Name, "err": err})
			continue
		}

		revert.Add(func() { d.deviceRemove(dev.Name, dev.Config, instanceRunning) })

		if instanceRunning {
			_, err := d.deviceStart(dev.Name, dev.Config, instanceRunning)
			if err != nil && err != device.ErrUnsupportedDevType {
				return errors.Wrapf(err, "Failed to start device %q", dev.Name)
			}

			revert.Add(func() { d.deviceStop(dev.Name, dev.Config, instanceRunning, "") })
		}
	}

	for _, dev := range updateDevices.Sorted() {
		err := d.deviceUpdate(dev.Name, dev.Config, oldExpandedDevices, instanceRunning)
		if err != nil && err != device.ErrUnsupportedDevType {
			return errors.Wrapf(err, "Failed to update device %q", dev.Name)
		}
	}

	revert.Success()
	return nil
}

// Export backs up the instance.
func (d *lxc) Export(w io.Writer, properties map[string]string, expiration time.Time) (api.ImageMetadata, error) {
	ctxMap := log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate}

	meta := api.ImageMetadata{}

	if d.IsRunning() {
		return meta, fmt.Errorf("Cannot export a running instance as an image")
	}

	d.logger.Info("Exporting instance", ctxMap)

	// Start the storage.
	_, err := d.mount()
	if err != nil {
		d.logger.Error("Failed exporting instance", ctxMap)
		return meta, err
	}
	defer d.unmount()

	// Get IDMap to unshift container as the tarball is created.
	idmap, err := d.DiskIdmap()
	if err != nil {
		d.logger.Error("Failed exporting instance", ctxMap)
		return meta, err
	}

	// Create the tarball.
	tarWriter := instancewriter.NewInstanceTarWriter(w, idmap)

	// Keep track of the first path we saw for each path with nlink>1.
	cDir := d.Path()

	// Path inside the tar image is the pathname starting after cDir.
	offset := len(cDir) + 1

	writeToTar := func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		err = tarWriter.WriteFile(path[offset:], path, fi, false)
		if err != nil {
			d.logger.Debug("Error tarring up", log.Ctx{"path": path, "err": err})
			return err
		}
		return nil
	}

	// Look for metadata.yaml.
	fnam := filepath.Join(cDir, "metadata.yaml")
	if !shared.PathExists(fnam) {
		// Generate a new metadata.yaml.
		tempDir, err := ioutil.TempDir("", "lxd_lxd_metadata_")
		if err != nil {
			tarWriter.Close()
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}
		defer os.RemoveAll(tempDir)

		// Get the instance's architecture.
		var arch string
		if d.IsSnapshot() {
			parentName, _, _ := shared.InstanceGetParentAndSnapshotName(d.name)
			parent, err := instance.LoadByProjectAndName(d.state, d.project, parentName)
			if err != nil {
				tarWriter.Close()
				d.logger.Error("Failed exporting instance", ctxMap)
				return meta, err
			}

			arch, _ = osarch.ArchitectureName(parent.Architecture())
		} else {
			arch, _ = osarch.ArchitectureName(d.architecture)
		}

		if arch == "" {
			arch, err = osarch.ArchitectureName(d.state.OS.Architectures[0])
			if err != nil {
				d.logger.Error("Failed exporting instance", ctxMap)
				return meta, err
			}
		}

		// Fill in the metadata.
		meta.Architecture = arch
		meta.CreationDate = time.Now().UTC().Unix()
		meta.Properties = properties
		if !expiration.IsZero() {
			meta.ExpiryDate = expiration.UTC().Unix()
		}

		data, err := yaml.Marshal(&meta)
		if err != nil {
			tarWriter.Close()
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}

		// Write the actual file.
		fnam = filepath.Join(tempDir, "metadata.yaml")
		err = ioutil.WriteFile(fnam, data, 0644)
		if err != nil {
			tarWriter.Close()
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}

		fi, err := os.Lstat(fnam)
		if err != nil {
			tarWriter.Close()
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}

		tmpOffset := len(path.Dir(fnam)) + 1
		if err := tarWriter.WriteFile(fnam[tmpOffset:], fnam, fi, false); err != nil {
			tarWriter.Close()
			d.logger.Debug("Error writing to tarfile", log.Ctx{"err": err})
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}
	} else {
		// Parse the metadata.
		content, err := ioutil.ReadFile(fnam)
		if err != nil {
			tarWriter.Close()
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}

		err = yaml.Unmarshal(content, &meta)
		if err != nil {
			tarWriter.Close()
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}

		if !expiration.IsZero() {
			meta.ExpiryDate = expiration.UTC().Unix()
		}

		if properties != nil {
			meta.Properties = properties
		}

		if properties != nil || !expiration.IsZero() {
			// Generate a new metadata.yaml.
			tempDir, err := ioutil.TempDir("", "lxd_lxd_metadata_")
			if err != nil {
				tarWriter.Close()
				d.logger.Error("Failed exporting instance", ctxMap)
				return meta, err
			}
			defer os.RemoveAll(tempDir)

			data, err := yaml.Marshal(&meta)
			if err != nil {
				tarWriter.Close()
				d.logger.Error("Failed exporting instance", ctxMap)
				return meta, err
			}

			// Write the actual file.
			fnam = filepath.Join(tempDir, "metadata.yaml")
			err = ioutil.WriteFile(fnam, data, 0644)
			if err != nil {
				tarWriter.Close()
				d.logger.Error("Failed exporting instance", ctxMap)
				return meta, err
			}
		}

		// Include metadata.yaml in the tarball.
		fi, err := os.Lstat(fnam)
		if err != nil {
			tarWriter.Close()
			d.logger.Debug("Error statting during export", log.Ctx{"fileName": fnam})
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}

		if properties != nil || !expiration.IsZero() {
			tmpOffset := len(path.Dir(fnam)) + 1
			err = tarWriter.WriteFile(fnam[tmpOffset:], fnam, fi, false)
		} else {
			err = tarWriter.WriteFile(fnam[offset:], fnam, fi, false)
		}
		if err != nil {
			tarWriter.Close()
			d.logger.Debug("Error writing to tarfile", log.Ctx{"err": err})
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}
	}

	// Include all the rootfs files.
	fnam = d.RootfsPath()
	err = filepath.Walk(fnam, writeToTar)
	if err != nil {
		d.logger.Error("Failed exporting instance", ctxMap)
		return meta, err
	}

	// Include all the templates.
	fnam = d.TemplatesPath()
	if shared.PathExists(fnam) {
		err = filepath.Walk(fnam, writeToTar)
		if err != nil {
			d.logger.Error("Failed exporting instance", ctxMap)
			return meta, err
		}
	}

	err = tarWriter.Close()
	if err != nil {
		d.logger.Error("Failed exporting instance", ctxMap)
		return meta, err
	}

	d.logger.Info("Exported instance", ctxMap)
	return meta, nil
}

func collectCRIULogFile(d instance.Instance, imagesDir string, function string, method string) error {
	t := time.Now().Format(time.RFC3339)
	newPath := filepath.Join(d.LogPath(), fmt.Sprintf("%s_%s_%s.log", function, method, t))
	return shared.FileCopy(filepath.Join(imagesDir, fmt.Sprintf("%s.log", method)), newPath)
}

func getCRIULogErrors(imagesDir string, method string) (string, error) {
	f, err := os.Open(path.Join(imagesDir, fmt.Sprintf("%s.log", method)))
	if err != nil {
		return "", err
	}

	defer f.Close()

	scanner := bufio.NewScanner(f)
	ret := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "Error") || strings.Contains(line, "Warn") {
			ret = append(ret, scanner.Text())
		}
	}

	return strings.Join(ret, "\n"), nil
}

// Migrate migrates the instance to another node.
func (d *lxc) Migrate(args *instance.CriuMigrationArgs) error {
	ctxMap := log.Ctx{
		"created":      d.creationDate,
		"ephemeral":    d.ephemeral,
		"used":         d.lastUsedDate,
		"statedir":     args.StateDir,
		"actionscript": args.ActionScript,
		"predumpdir":   args.PreDumpDir,
		"features":     args.Features,
		"stop":         args.Stop}

	_, err := exec.LookPath("criu")
	if err != nil {
		return fmt.Errorf("Unable to perform container live migration. CRIU isn't installed")
	}

	d.logger.Info("Migrating container", ctxMap)

	prettyCmd := ""
	switch args.Cmd {
	case liblxc.MIGRATE_PRE_DUMP:
		prettyCmd = "pre-dump"
	case liblxc.MIGRATE_DUMP:
		prettyCmd = "dump"
	case liblxc.MIGRATE_RESTORE:
		prettyCmd = "restore"
	case liblxc.MIGRATE_FEATURE_CHECK:
		prettyCmd = "feature-check"
	default:
		prettyCmd = "unknown"
		d.logger.Warn("Unknown migrate call", log.Ctx{"cmd": args.Cmd})
	}

	pool, err := d.getStoragePool()
	if err != nil {
		return err
	}

	preservesInodes := pool.Driver().Info().PreservesInodes

	/* This feature was only added in 2.0.1, let's not ask for it
	 * before then or migrations will fail.
	 */
	if !util.RuntimeLiblxcVersionAtLeast(2, 0, 1) {
		preservesInodes = false
	}

	finalStateDir := args.StateDir
	var migrateErr error

	/* For restore, we need an extra fork so that we daemonize monitor
	 * instead of having it be a child of LXD, so let's hijack the command
	 * here and do the extra fork.
	 */
	if args.Cmd == liblxc.MIGRATE_RESTORE {
		// Check that we're not already running.
		if d.IsRunning() {
			return fmt.Errorf("The container is already running")
		}

		// Run the shared start
		_, postStartHooks, err := d.startCommon()
		if err != nil {
			return errors.Wrap(err, "Failed preparing container for start")
		}

		/*
		 * For unprivileged containers we need to shift the
		 * perms on the images images so that they can be
		 * opened by the process after it is in its user
		 * namespace.
		 */
		idmapset, err := d.CurrentIdmap()
		if err != nil {
			return err
		}

		if idmapset != nil {
			storageType, err := d.getStorageType()
			if err != nil {
				return errors.Wrap(err, "Storage type")
			}

			if storageType == "zfs" {
				err = idmapset.ShiftRootfs(args.StateDir, storageDrivers.ShiftZFSSkipper)
			} else if storageType == "btrfs" {
				err = storageDrivers.ShiftBtrfsRootfs(args.StateDir, idmapset)
			} else {
				err = idmapset.ShiftRootfs(args.StateDir, nil)
			}
			if err != nil {
				return err
			}
		}

		configPath := filepath.Join(d.LogPath(), "lxc.conf")

		if args.DumpDir != "" {
			finalStateDir = fmt.Sprintf("%s/%s", args.StateDir, args.DumpDir)
		}

		_, migrateErr = shared.RunCommand(
			d.state.OS.ExecPath,
			"forkmigrate",
			d.name,
			d.state.OS.LxcPath,
			configPath,
			finalStateDir,
			fmt.Sprintf("%v", preservesInodes))

		if migrateErr == nil {
			// Run any post start hooks.
			err := d.runHooks(postStartHooks)
			if err != nil {
				// Attempt to stop container.
				d.Stop(false)
				return err
			}
		}
	} else if args.Cmd == liblxc.MIGRATE_FEATURE_CHECK {
		err := d.initLXC(true)
		if err != nil {
			return err
		}

		opts := liblxc.MigrateOptions{
			FeaturesToCheck: args.Features,
		}
		migrateErr = d.c.Migrate(args.Cmd, opts)
		if migrateErr != nil {
			d.logger.Info("CRIU feature check failed", ctxMap)
			return migrateErr
		}
		return nil
	} else {
		err := d.initLXC(true)
		if err != nil {
			return err
		}

		script := ""
		if args.ActionScript {
			script = filepath.Join(args.StateDir, "action.sh")
		}

		if args.DumpDir != "" {
			finalStateDir = fmt.Sprintf("%s/%s", args.StateDir, args.DumpDir)
		}

		// TODO: make this configurable? Ultimately I think we don't
		// want to do that; what we really want to do is have "modes"
		// of criu operation where one is "make this succeed" and the
		// other is "make this fast". Anyway, for now, let's choose a
		// really big size so it almost always succeeds, even if it is
		// slow.
		ghostLimit := uint64(256 * 1024 * 1024)

		opts := liblxc.MigrateOptions{
			Stop:            args.Stop,
			Directory:       finalStateDir,
			Verbose:         true,
			PreservesInodes: preservesInodes,
			ActionScript:    script,
			GhostLimit:      ghostLimit,
		}
		if args.PreDumpDir != "" {
			opts.PredumpDir = fmt.Sprintf("../%s", args.PreDumpDir)
		}

		if !d.IsRunning() {
			// otherwise the migration will needlessly fail
			args.Stop = false
		}

		migrateErr = d.c.Migrate(args.Cmd, opts)
	}

	collectErr := collectCRIULogFile(d, finalStateDir, args.Function, prettyCmd)
	if collectErr != nil {
		d.logger.Error("Error collecting checkpoint log file", log.Ctx{"err": collectErr})
	}

	if migrateErr != nil {
		log, err2 := getCRIULogErrors(finalStateDir, prettyCmd)
		if err2 == nil {
			d.logger.Info("Failed migrating container", ctxMap)
			migrateErr = fmt.Errorf("%s %s failed\n%s", args.Function, prettyCmd, log)
		}

		return migrateErr
	}

	d.logger.Info("Migrated container", ctxMap)

	return nil
}

func (d *lxc) templateApplyNow(trigger instance.TemplateTrigger) error {
	// If there's no metadata, just return
	fname := filepath.Join(d.Path(), "metadata.yaml")
	if !shared.PathExists(fname) {
		return nil
	}

	// Parse the metadata
	content, err := ioutil.ReadFile(fname)
	if err != nil {
		return errors.Wrap(err, "Failed to read metadata")
	}

	metadata := new(api.ImageMetadata)
	err = yaml.Unmarshal(content, &metadata)

	if err != nil {
		return errors.Wrapf(err, "Could not parse %s", fname)
	}

	// Find rootUID and rootGID
	idmapset, err := d.DiskIdmap()
	if err != nil {
		return errors.Wrap(err, "Failed to set ID map")
	}

	rootUID := int64(0)
	rootGID := int64(0)

	// Get the right uid and gid for the container
	if idmapset != nil {
		rootUID, rootGID = idmapset.ShiftIntoNs(0, 0)
	}

	// Figure out the container architecture
	arch, err := osarch.ArchitectureName(d.architecture)
	if err != nil {
		arch, err = osarch.ArchitectureName(d.state.OS.Architectures[0])
		if err != nil {
			return errors.Wrap(err, "Failed to detect system architecture")
		}
	}

	// Generate the container metadata
	containerMeta := make(map[string]string)
	containerMeta["name"] = d.name
	containerMeta["type"] = "container"
	containerMeta["architecture"] = arch

	if d.ephemeral {
		containerMeta["ephemeral"] = "true"
	} else {
		containerMeta["ephemeral"] = "false"
	}

	if d.IsPrivileged() {
		containerMeta["privileged"] = "true"
	} else {
		containerMeta["privileged"] = "false"
	}

	// Go through the templates
	for tplPath, tpl := range metadata.Templates {
		err = func(tplPath string, tpl *api.ImageMetadataTemplate) error {
			var w *os.File

			// Check if the template should be applied now
			found := false
			for _, tplTrigger := range tpl.When {
				if tplTrigger == string(trigger) {
					found = true
					break
				}
			}

			if !found {
				return nil
			}

			// Open the file to template, create if needed
			fullpath := filepath.Join(d.RootfsPath(), strings.TrimLeft(tplPath, "/"))
			if shared.PathExists(fullpath) {
				if tpl.CreateOnly {
					return nil
				}

				// Open the existing file
				w, err = os.Create(fullpath)
				if err != nil {
					return errors.Wrap(err, "Failed to create template file")
				}
			} else {
				// Create the directories leading to the file
				shared.MkdirAllOwner(path.Dir(fullpath), 0755, int(rootUID), int(rootGID))

				// Create the file itself
				w, err = os.Create(fullpath)
				if err != nil {
					return err
				}

				// Fix ownership and mode
				w.Chown(int(rootUID), int(rootGID))
				w.Chmod(0644)
			}
			defer w.Close()

			// Read the template
			tplString, err := ioutil.ReadFile(filepath.Join(d.TemplatesPath(), tpl.Template))
			if err != nil {
				return errors.Wrap(err, "Failed to read template file")
			}

			// Restrict filesystem access to within the container's rootfs
			tplSet := pongo2.NewSet(fmt.Sprintf("%s-%s", d.name, tpl.Template), template.ChrootLoader{Path: d.RootfsPath()})

			tplRender, err := tplSet.FromString("{% autoescape off %}" + string(tplString) + "{% endautoescape %}")
			if err != nil {
				return errors.Wrap(err, "Failed to render template")
			}

			configGet := func(confKey, confDefault *pongo2.Value) *pongo2.Value {
				val, ok := d.expandedConfig[confKey.String()]
				if !ok {
					return confDefault
				}

				return pongo2.AsValue(strings.TrimRight(val, "\r\n"))
			}

			// Render the template
			tplRender.ExecuteWriter(pongo2.Context{"trigger": trigger,
				"path":       tplPath,
				"container":  containerMeta,
				"instance":   containerMeta,
				"config":     d.expandedConfig,
				"devices":    d.expandedDevices,
				"properties": tpl.Properties,
				"config_get": configGet}, w)

			return nil

		}(tplPath, tpl)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *lxc) inheritInitPidFd() (int, *os.File) {
	if d.state.OS.PidFds {
		pidFdFile, err := d.InitPidFd()
		if err != nil {
			return -1, nil
		}

		return 3, pidFdFile
	}

	return -1, nil
}

// FileExists returns whether file exists inside instance.
func (d *lxc) FileExists(path string) error {
	// Check for ongoing operations (that may involve shifting).
	operationlock.Get(d.id).Wait()

	// Setup container storage if needed
	_, err := d.mount()
	if err != nil {
		return err
	}
	defer d.unmount()

	pidFdNr, pidFd := d.inheritInitPidFd()
	if pidFdNr >= 0 {
		defer pidFd.Close()
	}

	// Check if the file exists in the container
	_, stderr, err := shared.RunCommandSplit(
		nil,
		[]*os.File{pidFd},
		d.state.OS.ExecPath,
		"forkfile",
		"exists",
		d.RootfsPath(),
		fmt.Sprintf("%d", d.InitPID()),
		fmt.Sprintf("%d", pidFdNr),
		path,
	)

	// Process forkcheckfile response
	if stderr != "" {
		if strings.HasPrefix(stderr, "error:") {
			return fmt.Errorf(strings.TrimPrefix(strings.TrimSuffix(stderr, "\n"), "error: "))
		}

		for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
			d.logger.Debug("forkcheckfile", log.Ctx{"line": line})
		}
	}

	if err != nil {
		return err
	}

	return nil
}

// FilePull gets a file from the instance.
func (d *lxc) FilePull(srcpath string, dstpath string) (int64, int64, os.FileMode, string, []string, error) {
	// Check for ongoing operations (that may involve shifting).
	operationlock.Get(d.id).Wait()

	// Setup container storage if needed
	_, err := d.mount()
	if err != nil {
		return -1, -1, 0, "", nil, err
	}
	defer d.unmount()

	pidFdNr, pidFd := d.inheritInitPidFd()
	if pidFdNr >= 0 {
		defer pidFd.Close()
	}

	// Get the file from the container
	_, stderr, err := shared.RunCommandSplit(
		nil,
		[]*os.File{pidFd},
		d.state.OS.ExecPath,
		"forkfile",
		"pull",
		d.RootfsPath(),
		fmt.Sprintf("%d", d.InitPID()),
		fmt.Sprintf("%d", pidFdNr),
		srcpath,
		dstpath,
	)

	uid := int64(-1)
	gid := int64(-1)
	mode := -1
	fileType := "unknown"
	var dirEnts []string
	var errStr string

	// Process forkgetfile response
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if line == "" {
			continue
		}

		// Extract errors
		if strings.HasPrefix(line, "error: ") {
			errStr = strings.TrimPrefix(line, "error: ")
			continue
		}

		if strings.HasPrefix(line, "errno: ") {
			errno := strings.TrimPrefix(line, "errno: ")
			if errno == "2" {
				return -1, -1, 0, "", nil, os.ErrNotExist
			}

			return -1, -1, 0, "", nil, fmt.Errorf(errStr)
		}

		// Extract the uid
		if strings.HasPrefix(line, "uid: ") {
			uid, err = strconv.ParseInt(strings.TrimPrefix(line, "uid: "), 10, 64)
			if err != nil {
				return -1, -1, 0, "", nil, err
			}

			continue
		}

		// Extract the gid
		if strings.HasPrefix(line, "gid: ") {
			gid, err = strconv.ParseInt(strings.TrimPrefix(line, "gid: "), 10, 64)
			if err != nil {
				return -1, -1, 0, "", nil, err
			}

			continue
		}

		// Extract the mode
		if strings.HasPrefix(line, "mode: ") {
			mode, err = strconv.Atoi(strings.TrimPrefix(line, "mode: "))
			if err != nil {
				return -1, -1, 0, "", nil, err
			}

			continue
		}

		if strings.HasPrefix(line, "type: ") {
			fileType = strings.TrimPrefix(line, "type: ")
			continue
		}

		if strings.HasPrefix(line, "entry: ") {
			ent := strings.TrimPrefix(line, "entry: ")
			ent = strings.Replace(ent, "\x00", "\n", -1)
			dirEnts = append(dirEnts, ent)
			continue
		}

		d.logger.Debug("forkgetfile", log.Ctx{"line": line})
	}

	if err != nil {
		return -1, -1, 0, "", nil, err
	}

	// Unmap uid and gid if needed
	if !d.IsRunning() {
		idmapset, err := d.DiskIdmap()
		if err != nil {
			return -1, -1, 0, "", nil, err
		}

		if idmapset != nil {
			uid, gid = idmapset.ShiftFromNs(uid, gid)
		}
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceFileRetrieved.Event(d, log.Ctx{"file-source": srcpath, "file-destination": dstpath}))

	return uid, gid, os.FileMode(mode), fileType, dirEnts, nil
}

// FilePush sends a file into the instance.
func (d *lxc) FilePush(fileType string, srcpath string, dstpath string, uid int64, gid int64, mode int, write string) error {
	// Check for ongoing operations (that may involve shifting).
	operationlock.Get(d.id).Wait()

	var rootUID int64
	var rootGID int64
	var errStr string

	// Map uid and gid if needed
	if !d.IsRunning() {
		idmapset, err := d.DiskIdmap()
		if err != nil {
			return err
		}

		if idmapset != nil {
			uid, gid = idmapset.ShiftIntoNs(uid, gid)
			rootUID, rootGID = idmapset.ShiftIntoNs(0, 0)
		}
	}

	// Setup container storage if needed
	_, err := d.mount()
	if err != nil {
		return err
	}
	defer d.unmount()

	defaultMode := 0640
	if fileType == "directory" {
		defaultMode = 0750
	}

	pidFdNr, pidFd := d.inheritInitPidFd()
	if pidFdNr >= 0 {
		defer pidFd.Close()
	}

	// Push the file to the container
	_, stderr, err := shared.RunCommandSplit(
		nil,
		[]*os.File{pidFd},
		d.state.OS.ExecPath,
		"forkfile",
		"push",
		d.RootfsPath(),
		fmt.Sprintf("%d", d.InitPID()),
		fmt.Sprintf("%d", pidFdNr),
		srcpath,
		dstpath,
		fileType,
		fmt.Sprintf("%d", uid),
		fmt.Sprintf("%d", gid),
		fmt.Sprintf("%d", mode),
		fmt.Sprintf("%d", rootUID),
		fmt.Sprintf("%d", rootGID),
		fmt.Sprintf("%d", int(os.FileMode(defaultMode)&os.ModePerm)),
		write,
	)

	// Process forkgetfile response
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if line == "" {
			continue
		}

		// Extract errors
		if strings.HasPrefix(line, "error: ") {
			errStr = strings.TrimPrefix(line, "error: ")
			continue
		}

		if strings.HasPrefix(line, "errno: ") {
			errno := strings.TrimPrefix(line, "errno: ")
			if errno == "2" {
				return os.ErrNotExist
			}

			return fmt.Errorf(errStr)
		}
	}

	if err != nil {
		return err
	}

	ctx := log.Ctx{"file-source": srcpath, "file-destination": dstpath, "gid": gid, "mode": mode, "file-type": fileType, "uid": uid, "write-mode": write}
	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceFilePushed.Event(d, ctx))

	return nil
}

// FileRemove removes a file inside the instance.
func (d *lxc) FileRemove(path string) error {
	// Check for ongoing operations (that may involve shifting).
	operationlock.Get(d.id).Wait()

	var errStr string

	// Setup container storage if needed
	_, err := d.mount()
	if err != nil {
		return err
	}
	defer d.unmount()

	pidFdNr, pidFd := d.inheritInitPidFd()
	if pidFdNr >= 0 {
		defer pidFd.Close()
	}

	// Remove the file from the container
	_, stderr, err := shared.RunCommandSplit(
		nil,
		[]*os.File{pidFd},
		d.state.OS.ExecPath,
		"forkfile",
		"remove",
		d.RootfsPath(),
		fmt.Sprintf("%d", d.InitPID()),
		fmt.Sprintf("%d", pidFdNr),
		path,
	)

	// Process forkremovefile response
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if line == "" {
			continue
		}

		// Extract errors
		if strings.HasPrefix(line, "error: ") {
			errStr = strings.TrimPrefix(line, "error: ")
			continue
		}

		if strings.HasPrefix(line, "errno: ") {
			errno := strings.TrimPrefix(line, "errno: ")
			if errno == "2" {
				return os.ErrNotExist
			}

			return fmt.Errorf(errStr)
		}
	}

	if err != nil {
		return err
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceFileDeleted.Event(d, log.Ctx{"file": path}))
	return nil
}

// Console attaches to the instance console.
func (d *lxc) Console(protocol string) (*os.File, chan error, error) {
	if protocol != instance.ConsoleTypeConsole {
		return nil, nil, fmt.Errorf("Container instances don't support %q output", protocol)
	}

	chDisconnect := make(chan error, 1)

	args := []string{
		d.state.OS.ExecPath,
		"forkconsole",
		project.Instance(d.Project(), d.Name()),
		d.state.OS.LxcPath,
		filepath.Join(d.LogPath(), "lxc.conf"),
		"tty=0",
		"escape=-1"}

	idmapset, err := d.CurrentIdmap()
	if err != nil {
		return nil, nil, err
	}

	var rootUID, rootGID int64
	if idmapset != nil {
		rootUID, rootGID = idmapset.ShiftIntoNs(0, 0)
	}

	ptx, pty, err := shared.OpenPty(rootUID, rootGID)
	if err != nil {
		return nil, nil, err
	}

	cmd := exec.Cmd{}
	cmd.Path = d.state.OS.ExecPath
	cmd.Args = args
	cmd.Stdin = pty
	cmd.Stdout = pty
	cmd.Stderr = pty

	err = cmd.Start()
	if err != nil {
		return nil, nil, err
	}

	go func() {
		err = cmd.Wait()
		ptx.Close()
		pty.Close()
	}()

	go func() {
		<-chDisconnect
		cmd.Process.Kill()
	}()

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceConsole.Event(d, log.Ctx{"type": instance.ConsoleTypeConsole}))

	return ptx, chDisconnect, nil
}

// ConsoleLog returns console log.
func (d *lxc) ConsoleLog(opts liblxc.ConsoleLogOptions) (string, error) {
	msg, err := d.c.ConsoleLog(opts)
	if err != nil {
		return "", err
	}

	if opts.ClearLog {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceConsoleReset.Event(d, nil))
	} else if opts.ReadLog && opts.WriteToLogFile {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceConsoleRetrieved.Event(d, nil))
	}

	return string(msg), nil
}

// Exec executes a command inside the instance.
func (d *lxc) Exec(req api.InstanceExecPost, stdin *os.File, stdout *os.File, stderr *os.File) (instance.Cmd, error) {
	// Prepare the environment
	envSlice := []string{}

	for k, v := range req.Environment {
		envSlice = append(envSlice, fmt.Sprintf("%s=%s", k, v))
	}

	// Setup logfile
	logPath := filepath.Join(d.LogPath(), "forkexec.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_SYNC, 0644)
	if err != nil {
		return nil, err
	}

	// Prepare the subcommand
	cname := project.Instance(d.Project(), d.Name())
	args := []string{
		d.state.OS.ExecPath,
		"forkexec",
		cname,
		d.state.OS.LxcPath,
		filepath.Join(d.LogPath(), "lxc.conf"),
		req.Cwd,
		fmt.Sprintf("%d", req.User),
		fmt.Sprintf("%d", req.Group),
	}

	args = append(args, "--")
	args = append(args, "env")
	args = append(args, envSlice...)

	args = append(args, "--")
	args = append(args, "cmd")
	args = append(args, req.Command...)

	cmd := exec.Cmd{}
	cmd.Path = d.state.OS.ExecPath
	cmd.Args = args

	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Mitigation for CVE-2019-5736
	useRexec := false
	if d.expandedConfig["raw.idmap"] != "" {
		err := instance.AllowedUnprivilegedOnlyMap(d.expandedConfig["raw.idmap"])
		if err != nil {
			useRexec = true
		}
	}

	if shared.IsTrue(d.expandedConfig["security.privileged"]) {
		useRexec = true
	}

	if useRexec {
		cmd.Env = append(os.Environ(), "LXC_MEMFD_REXEC=1")
	}

	// Setup communication PIPE
	rStatus, wStatus, err := os.Pipe()
	defer rStatus.Close()
	if err != nil {
		return nil, err
	}

	cmd.ExtraFiles = []*os.File{stdin, stdout, stderr, wStatus}
	err = cmd.Start()
	wStatus.Close()
	if err != nil {
		return nil, err
	}

	attachedPid := shared.ReadPid(rStatus)
	if attachedPid <= 0 {
		cmd.Wait()
		d.logger.Error("Failed to retrieve PID of executing child process")
		return nil, fmt.Errorf("Failed to retrieve PID of executing child process")
	}
	d.logger.Debug("Retrieved PID of executing child process", log.Ctx{"attachedPid": attachedPid})

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceExec.Event(d, log.Ctx{"command": req.Command}))

	instCmd := &lxcCmd{
		cmd:              &cmd,
		attachedChildPid: int(attachedPid),
	}

	return instCmd, nil
}

func (d *lxc) cpuState() api.InstanceStateCPU {
	cpu := api.InstanceStateCPU{}

	// CPU usage in seconds
	cg, err := d.cgroup(nil)
	if err != nil {
		return cpu
	}

	if !d.state.OS.CGInfo.Supports(cgroup.CPUAcct, cg) {
		return cpu
	}

	value, err := cg.GetCPUAcctUsage()
	if err != nil {
		cpu.Usage = -1
		return cpu
	}

	cpu.Usage = value

	return cpu
}

func (d *lxc) diskState() map[string]api.InstanceStateDisk {
	disk := map[string]api.InstanceStateDisk{}

	for _, dev := range d.expandedDevices.Sorted() {
		if dev.Config["type"] != "disk" {
			continue
		}

		var usage int64

		if dev.Config["path"] == "/" {
			pool, err := storagePools.GetPoolByInstance(d.state, d)
			if err != nil {
				d.logger.Error("Error loading storage pool", log.Ctx{"err": err})
				continue
			}

			usage, err = pool.GetInstanceUsage(d)
			if err != nil {
				if errors.Cause(err) != storageDrivers.ErrNotSupported {
					d.logger.Error("Error getting disk usage", log.Ctx{"err": err})
				}
				continue
			}
		} else if dev.Config["pool"] != "" {
			pool, err := storagePools.GetPoolByName(d.state, dev.Config["pool"])
			if err != nil {
				d.logger.Error("Error loading storage pool", log.Ctx{"poolName": dev.Config["pool"], "err": err})
				continue
			}

			usage, err = pool.GetCustomVolumeUsage(d.Project(), dev.Config["source"])
			if err != nil {
				if errors.Cause(err) != storageDrivers.ErrNotSupported {
					d.logger.Error("Error getting volume usage", log.Ctx{"volume": dev.Config["source"], "err": err})
				}
				continue
			}
		} else {
			continue
		}

		disk[dev.Name] = api.InstanceStateDisk{Usage: usage}
	}

	return disk
}

func (d *lxc) memoryState() api.InstanceStateMemory {
	memory := api.InstanceStateMemory{}
	cg, err := d.cgroup(nil)
	if err != nil {
		return memory
	}

	if !d.state.OS.CGInfo.Supports(cgroup.Memory, cg) {
		return memory
	}

	// Memory in bytes
	value, err := cg.GetMemoryUsage()
	if err == nil {
		memory.Usage = value
	}

	// Memory peak in bytes
	if d.state.OS.CGInfo.Supports(cgroup.MemoryMaxUsage, cg) {
		value, err = cg.GetMemoryMaxUsage()
		if err == nil {
			memory.UsagePeak = value
		}
	}

	if d.state.OS.CGInfo.Supports(cgroup.MemorySwapUsage, cg) {
		// Swap in bytes
		if memory.Usage > 0 {
			value, err := cg.GetMemorySwapUsage()
			if err == nil {
				memory.SwapUsage = value
			}
		}

		// Swap peak in bytes
		if memory.UsagePeak > 0 {
			value, err = cg.GetMemorySwapMaxUsage()
			if err == nil {
				memory.SwapUsagePeak = value
			}
		}
	}

	return memory
}

func (d *lxc) networkState() map[string]api.InstanceStateNetwork {
	result := map[string]api.InstanceStateNetwork{}

	pid := d.InitPID()
	if pid < 1 {
		return result
	}

	couldUseNetnsGetifaddrs := d.state.OS.NetnsGetifaddrs
	if couldUseNetnsGetifaddrs {
		nw, err := netutils.NetnsGetifaddrs(int32(pid))
		if err != nil {
			couldUseNetnsGetifaddrs = false
			d.logger.Error("Failed to retrieve network information via netlink", log.Ctx{"pid": pid})
		} else {
			result = nw
		}
	}

	if !couldUseNetnsGetifaddrs {
		pidFdNr, pidFd := d.inheritInitPidFd()
		if pidFdNr >= 0 {
			defer pidFd.Close()
		}

		// Get the network state from the container
		out, _, err := shared.RunCommandSplit(
			nil,
			[]*os.File{pidFd},
			d.state.OS.ExecPath,
			"forknet",
			"info",
			"--",
			fmt.Sprintf("%d", pid),
			fmt.Sprintf("%d", pidFdNr))

		// Process forkgetnet response
		if err != nil {
			d.logger.Error("Error calling 'lxd forknet", log.Ctx{"err": err, "pid": pid})
			return result
		}

		// If we can use netns_getifaddrs() but it failed and the setns() +
		// netns_getifaddrs() succeeded we should just always fallback to the
		// setns() + netns_getifaddrs() style retrieval.
		d.state.OS.NetnsGetifaddrs = false

		nw := map[string]api.InstanceStateNetwork{}
		err = json.Unmarshal([]byte(out), &nw)
		if err != nil {
			d.logger.Error("Failure to read forknet json", log.Ctx{"err": err})
			return result
		}
		result = nw
	}

	// Get host_name from volatile data if not set already.
	for name, dev := range result {
		if dev.HostName == "" {
			dev.HostName = d.localConfig[fmt.Sprintf("volatile.%s.host_name", name)]
			result[name] = dev
		}
	}

	return result
}

func (d *lxc) processesState() int64 {
	// Return 0 if not running
	pid := d.InitPID()
	if pid == -1 {
		return 0
	}

	cg, err := d.cgroup(nil)
	if err != nil {
		return 0
	}

	if d.state.OS.CGInfo.Supports(cgroup.Pids, cg) {
		value, err := cg.GetProcessesUsage()
		if err != nil {
			return -1
		}

		return value
	}

	pids := []int64{int64(pid)}

	// Go through the pid list, adding new pids at the end so we go through them all
	for i := 0; i < len(pids); i++ {
		fname := fmt.Sprintf("/proc/%d/task/%d/children", pids[i], pids[i])
		fcont, err := ioutil.ReadFile(fname)
		if err != nil {
			// the process terminated during execution of this loop
			continue
		}

		content := strings.Split(string(fcont), " ")
		for j := 0; j < len(content); j++ {
			pid, err := strconv.ParseInt(content[j], 10, 64)
			if err == nil {
				pids = append(pids, pid)
			}
		}
	}

	return int64(len(pids))
}

// getStoragePool returns the current storage pool handle. To avoid a DB lookup each time this
// function is called, the handle is cached internally in the lxc struct.
func (d *lxc) getStoragePool() (storagePools.Pool, error) {
	if d.storagePool != nil {
		return d.storagePool, nil
	}

	pool, err := storagePools.GetPoolByInstance(d.state, d)
	if err != nil {
		return nil, err
	}
	d.storagePool = pool

	return d.storagePool, nil
}

// getStorageType returns the storage type of the instance's storage pool.
func (d *lxc) getStorageType() (string, error) {
	pool, err := d.getStoragePool()
	if err != nil {
		return "", err
	}

	return pool.Driver().Info().Name, nil
}

// mount the instance's rootfs volume if needed.
func (d *lxc) mount() (*storagePools.MountInfo, error) {
	pool, err := d.getStoragePool()
	if err != nil {
		return nil, err
	}

	if d.IsSnapshot() {
		mountInfo, err := pool.MountInstanceSnapshot(d, nil)
		if err != nil {
			return nil, err
		}

		return mountInfo, nil
	}

	mountInfo, err := pool.MountInstance(d, nil)
	if err != nil {
		return nil, err
	}

	return mountInfo, nil
}

// unmount the instance's rootfs volume if needed.
func (d *lxc) unmount() (bool, error) {
	pool, err := d.getStoragePool()
	if err != nil {
		return false, err
	}

	if d.IsSnapshot() {
		unmounted, err := pool.UnmountInstanceSnapshot(d, nil)
		if err != nil {
			return false, err
		}

		return unmounted, nil
	}

	// Workaround for liblxc failures on startup when shiftfs is used.
	diskIdmap, err := d.DiskIdmap()
	if err != nil {
		return false, err
	}

	if d.IdmappedStorage(d.RootfsPath()) == idmap.IdmapStorageShiftfs && !d.IsPrivileged() && diskIdmap == nil {
		unix.Unmount(d.RootfsPath(), unix.MNT_DETACH)
	}

	unmounted, err := pool.UnmountInstance(d, nil)
	if err != nil {
		return false, err
	}

	return unmounted, nil
}

// insertMountLXD inserts a mount into a LXD container.
// This function is used for the seccomp notifier and so cannot call any
// functions that would cause LXC to talk to the container's monitor. Otherwise
// we'll have a deadlock (with a timeout but still). The InitPID() call here is
// the exception since the seccomp notifier will make sure to always pass a
// valid PID.
func (d *lxc) insertMountLXD(source, target, fstype string, flags int, mntnsPID int, idmapType idmap.IdmapStorageType) error {
	pid := mntnsPID
	if pid <= 0 {
		// Get the init PID
		pid = d.InitPID()
		if pid == -1 {
			// Container isn't running
			return fmt.Errorf("Can't insert mount into stopped container")
		}
	}

	// Create the temporary mount target
	var tmpMount string
	var err error
	if shared.IsDir(source) {
		tmpMount, err = ioutil.TempDir(d.ShmountsPath(), "lxdmount_")
		if err != nil {
			return fmt.Errorf("Failed to create shmounts path: %s", err)
		}
	} else {
		f, err := ioutil.TempFile(d.ShmountsPath(), "lxdmount_")
		if err != nil {
			return fmt.Errorf("Failed to create shmounts path: %s", err)
		}

		tmpMount = f.Name()
		f.Close()
	}
	defer os.Remove(tmpMount)

	// Mount the filesystem
	err = unix.Mount(source, tmpMount, fstype, uintptr(flags), "")
	if err != nil {
		return fmt.Errorf("Failed to setup temporary mount: %s", err)
	}
	defer unix.Unmount(tmpMount, unix.MNT_DETACH)

	// Ensure that only flags modifying mount _properties_ make it through.
	// Strip things such as MS_BIND which would cause the creation of a
	// shiftfs mount to be skipped.
	// (Fyi, this is just one of the reasons why multiplexers are bad;
	// specifically when they do heinous things such as confusing flags
	// with commands.)

	// This is why multiplexers are bad
	shiftfsFlags := (flags & (unix.MS_RDONLY |
		unix.MS_NOSUID |
		unix.MS_NODEV |
		unix.MS_NOEXEC |
		unix.MS_DIRSYNC |
		unix.MS_NOATIME |
		unix.MS_NODIRATIME))

	// Setup host side shiftfs as needed
	switch idmapType {
	case idmap.IdmapStorageShiftfs:
		err = unix.Mount(tmpMount, tmpMount, "shiftfs", uintptr(shiftfsFlags), "mark,passthrough=3")
		if err != nil {
			return fmt.Errorf("Failed to setup host side shiftfs mount: %s", err)
		}
		defer unix.Unmount(tmpMount, unix.MNT_DETACH)
	case idmap.IdmapStorageIdmapped:
	case idmap.IdmapStorageNone:
	default:
		return fmt.Errorf("Invalid idmap value specified")
	}

	// Move the mount inside the container
	mntsrc := filepath.Join("/dev/.lxd-mounts", filepath.Base(tmpMount))
	pidStr := fmt.Sprintf("%d", pid)

	pidFdNr, pidFd := seccomp.MakePidFd(pid, d.state)
	if pidFdNr >= 0 {
		defer pidFd.Close()
	}

	if !strings.HasPrefix(target, "/") {
		target = "/" + target
	}

	_, err = shared.RunCommandInheritFds(
		[]*os.File{pidFd},
		d.state.OS.ExecPath,
		"forkmount",
		"lxd-mount",
		"--",
		pidStr,
		fmt.Sprintf("%d", pidFdNr),
		mntsrc,
		target,
		string(idmapType),
		fmt.Sprintf("%d", shiftfsFlags))
	if err != nil {
		return err
	}

	return nil
}

func (d *lxc) insertMountLXC(source, target, fstype string, flags int) error {
	cname := project.Instance(d.Project(), d.Name())
	configPath := filepath.Join(d.LogPath(), "lxc.conf")
	if fstype == "" {
		fstype = "none"
	}

	if !strings.HasPrefix(target, "/") {
		target = "/" + target
	}

	_, err := shared.RunCommand(
		d.state.OS.ExecPath,
		"forkmount",
		"lxc-mount",
		"--",
		cname,
		d.state.OS.LxcPath,
		configPath,
		source,
		target,
		fstype,
		fmt.Sprintf("%d", flags))
	if err != nil {
		return err
	}

	return nil
}

func (d *lxc) insertMount(source, target, fstype string, flags int, idmapType idmap.IdmapStorageType) error {
	if d.state.OS.LXCFeatures["mount_injection_file"] && idmapType == idmap.IdmapStorageNone {
		return d.insertMountLXC(source, target, fstype, flags)
	}

	return d.insertMountLXD(source, target, fstype, flags, -1, idmapType)
}

func (d *lxc) removeMount(mount string) error {
	// Get the init PID
	pid := d.InitPID()
	if pid == -1 {
		// Container isn't running
		return fmt.Errorf("Can't remove mount from stopped container")
	}

	if d.state.OS.LXCFeatures["mount_injection_file"] {
		configPath := filepath.Join(d.LogPath(), "lxc.conf")
		cname := project.Instance(d.Project(), d.Name())

		if !strings.HasPrefix(mount, "/") {
			mount = "/" + mount
		}

		_, err := shared.RunCommand(
			d.state.OS.ExecPath,
			"forkmount",
			"lxc-umount",
			"--",
			cname,
			d.state.OS.LxcPath,
			configPath,
			mount)
		if err != nil {
			return err
		}
	} else {
		// Remove the mount from the container
		pidFdNr, pidFd := d.inheritInitPidFd()
		if pidFdNr >= 0 {
			defer pidFd.Close()
		}

		_, err := shared.RunCommandInheritFds(
			[]*os.File{pidFd},
			d.state.OS.ExecPath,
			"forkmount",
			"lxd-umount",
			"--",
			fmt.Sprintf("%d", pid),
			fmt.Sprintf("%d", pidFdNr),
			mount)
		if err != nil {
			return err
		}
	}

	return nil
}

// InsertSeccompUnixDevice inserts a seccomp device.
func (d *lxc) InsertSeccompUnixDevice(prefix string, m deviceConfig.Device, pid int) error {
	if pid < 0 {
		return fmt.Errorf("Invalid request PID specified")
	}

	rootLink := fmt.Sprintf("/proc/%d/root", pid)
	rootPath, err := os.Readlink(rootLink)
	if err != nil {
		return err
	}

	uid, gid, _, _, err := seccomp.TaskIDs(pid)
	if err != nil {
		return err
	}

	idmapset, err := d.CurrentIdmap()
	if err != nil {
		return err
	}

	nsuid, nsgid := idmapset.ShiftFromNs(uid, gid)
	m["uid"] = fmt.Sprintf("%d", nsuid)
	m["gid"] = fmt.Sprintf("%d", nsgid)

	if !path.IsAbs(m["path"]) {
		cwdLink := fmt.Sprintf("/proc/%d/cwd", pid)
		prefixPath, err := os.Readlink(cwdLink)
		if err != nil {
			return err
		}

		prefixPath = strings.TrimPrefix(prefixPath, rootPath)
		m["path"] = filepath.Join(rootPath, prefixPath, m["path"])
	} else {
		m["path"] = filepath.Join(rootPath, m["path"])
	}

	idmapSet, err := d.CurrentIdmap()
	if err != nil {
		return err
	}

	dev, err := device.UnixDeviceCreate(d.state, idmapSet, d.DevicesPath(), prefix, m, true)
	if err != nil {
		return fmt.Errorf("Failed to setup device: %s", err)
	}

	devPath := dev.HostPath
	tgtPath := dev.RelativePath

	// Bind-mount it into the container
	defer os.Remove(devPath)
	return d.insertMountLXD(devPath, tgtPath, "none", unix.MS_BIND, pid, idmap.IdmapStorageNone)
}

func (d *lxc) removeUnixDevices() error {
	// Check that we indeed have devices to remove
	if !shared.PathExists(d.DevicesPath()) {
		return nil
	}

	// Load the directory listing
	dents, err := ioutil.ReadDir(d.DevicesPath())
	if err != nil {
		return err
	}

	// Go through all the unix devices
	for _, f := range dents {
		// Skip non-Unix devices
		if !strings.HasPrefix(f.Name(), "forkmknod.unix.") && !strings.HasPrefix(f.Name(), "unix.") && !strings.HasPrefix(f.Name(), "infiniband.unix.") {
			continue
		}

		// Remove the entry
		devicePath := filepath.Join(d.DevicesPath(), f.Name())
		err := os.Remove(devicePath)
		if err != nil {
			d.logger.Error("Failed removing unix device", log.Ctx{"err": err, "path": devicePath})
		}
	}

	return nil
}

// FillNetworkDevice takes a nic or infiniband device type and enriches it with automatically
// generated name and hwaddr properties if these are missing from the device.
func (d *lxc) FillNetworkDevice(name string, m deviceConfig.Device) (deviceConfig.Device, error) {
	var err error
	newDevice := m.Clone()

	// Function to try and guess an available name
	nextInterfaceName := func() (string, error) {
		devNames := []string{}

		// Include all static interface names
		for _, dev := range d.expandedDevices.Sorted() {
			if dev.Config["name"] != "" && !shared.StringInSlice(dev.Config["name"], devNames) {
				devNames = append(devNames, dev.Config["name"])
			}
		}

		// Include all currently allocated interface names
		for k, v := range d.expandedConfig {
			if !strings.HasPrefix(k, shared.ConfigVolatilePrefix) {
				continue
			}

			fields := strings.SplitN(k, ".", 3)
			if len(fields) != 3 {
				continue
			}

			if fields[2] != "name" || shared.StringInSlice(v, devNames) {
				continue
			}

			devNames = append(devNames, v)
		}

		// Attempt to include all existing interfaces
		cname := project.Instance(d.Project(), d.Name())
		cc, err := liblxc.NewContainer(cname, d.state.OS.LxcPath)
		if err == nil {
			defer cc.Release()

			interfaces, err := cc.Interfaces()
			if err == nil {
				for _, name := range interfaces {
					if shared.StringInSlice(name, devNames) {
						continue
					}

					devNames = append(devNames, name)
				}
			}
		}

		i := 0
		name := ""
		for {
			if m["type"] == "infiniband" {
				name = fmt.Sprintf("ib%d", i)
			} else {
				name = fmt.Sprintf("eth%d", i)
			}

			// Find a free device name
			if !shared.StringInSlice(name, devNames) {
				return name, nil
			}

			i++
		}
	}

	nicType, err := nictype.NICType(d.state, d.Project(), m)
	if err != nil {
		return nil, err
	}

	// Fill in the MAC address.
	if !shared.StringInSlice(nicType, []string{"physical", "ipvlan", "sriov"}) && m["hwaddr"] == "" {
		configKey := fmt.Sprintf("volatile.%s.hwaddr", name)
		volatileHwaddr := d.localConfig[configKey]
		if volatileHwaddr == "" {
			// Generate a new MAC address.
			volatileHwaddr, err = instance.DeviceNextInterfaceHWAddr()
			if err != nil || volatileHwaddr == "" {
				return nil, errors.Wrapf(err, "Failed generating %q", configKey)
			}

			// Update the database and update volatileHwaddr with stored value.
			volatileHwaddr, err = d.insertConfigkey(configKey, volatileHwaddr)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed storing generated config key %q", configKey)
			}

			// Set stored value into current instance config.
			d.localConfig[configKey] = volatileHwaddr
			d.expandedConfig[configKey] = volatileHwaddr
		}

		if volatileHwaddr == "" {
			return nil, fmt.Errorf("Failed getting %q", configKey)
		}

		newDevice["hwaddr"] = volatileHwaddr
	}

	// Fill in the interface name.
	if m["name"] == "" {
		configKey := fmt.Sprintf("volatile.%s.name", name)
		volatileName := d.localConfig[configKey]
		if volatileName == "" {
			// Generate a new interface name.
			volatileName, err = nextInterfaceName()
			if err != nil || volatileName == "" {
				return nil, errors.Wrapf(err, "Failed generating %q", configKey)
			}

			// Update the database and update volatileName with stored value.
			volatileName, err = d.insertConfigkey(configKey, volatileName)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed storing generated config key %q", configKey)
			}

			// Set stored value into current instance config.
			d.localConfig[configKey] = volatileName
			d.expandedConfig[configKey] = volatileName
		}

		if volatileName == "" {
			return nil, fmt.Errorf("Failed getting %q", configKey)
		}

		newDevice["name"] = volatileName
	}

	return newDevice, nil
}

func (d *lxc) removeDiskDevices() error {
	// Check that we indeed have devices to remove
	if !shared.PathExists(d.DevicesPath()) {
		return nil
	}

	// Load the directory listing
	dents, err := ioutil.ReadDir(d.DevicesPath())
	if err != nil {
		return err
	}

	// Go through all the unix devices
	for _, f := range dents {
		// Skip non-disk devices
		if !strings.HasPrefix(f.Name(), "disk.") {
			continue
		}

		// Always try to unmount the host side
		_ = unix.Unmount(filepath.Join(d.DevicesPath(), f.Name()), unix.MNT_DETACH)

		// Remove the entry
		diskPath := filepath.Join(d.DevicesPath(), f.Name())
		err := os.Remove(diskPath)
		if err != nil {
			d.logger.Error("Failed to remove disk device path", log.Ctx{"err": err, "path": diskPath})
		}
	}

	return nil
}

// Network I/O limits
func (d *lxc) setNetworkPriority() error {
	// Load the go-lxc struct.
	err := d.initLXC(false)
	if err != nil {
		return err
	}

	// Load the cgroup struct.
	cg, err := d.cgroup(nil)
	if err != nil {
		return err
	}

	// Check that the container is running
	if !d.IsRunning() {
		return fmt.Errorf("Can't set network priority on stopped container")
	}

	// Don't bother if the cgroup controller doesn't exist
	if !d.state.OS.CGInfo.Supports(cgroup.NetPrio, cg) {
		return nil
	}

	// Extract the current priority
	networkPriority := d.expandedConfig["limits.network.priority"]
	if networkPriority == "" {
		networkPriority = "0"
	}

	networkInt, err := strconv.Atoi(networkPriority)
	if err != nil {
		return err
	}

	// Get all the interfaces
	netifs, err := net.Interfaces()
	if err != nil {
		return err
	}

	// Check that we at least succeeded to set an entry
	success := false
	var lastError error
	for _, netif := range netifs {
		err = cg.SetNetIfPrio(fmt.Sprintf("%s %d", netif.Name, networkInt))
		if err == nil {
			success = true
		} else {
			lastError = err
		}
	}

	if !success {
		return fmt.Errorf("Failed to set network device priority: %s", lastError)
	}

	return nil
}

// IsFrozen returns if instance is frozen.
func (d *lxc) IsFrozen() bool {
	return d.statusCode() == api.Frozen
}

// IsNesting returns if instance is nested.
func (d *lxc) IsNesting() bool {
	return shared.IsTrue(d.expandedConfig["security.nesting"])
}

func (d *lxc) isCurrentlyPrivileged() bool {
	if !d.IsRunning() {
		return d.IsPrivileged()
	}

	idmap, err := d.CurrentIdmap()
	if err != nil {
		return d.IsPrivileged()
	}

	return idmap == nil
}

// IsPrivileged returns if instance is privileged.
func (d *lxc) IsPrivileged() bool {
	return shared.IsTrue(d.expandedConfig["security.privileged"])
}

// IsRunning returns if instance is running.
func (d *lxc) IsRunning() bool {
	return d.isRunningStatusCode(d.statusCode())
}

// InitPID returns PID of init process.
func (d *lxc) InitPID() int {
	// Load the go-lxc struct
	err := d.initLXC(false)
	if err != nil {
		return -1
	}

	return d.c.InitPid()
}

// InitPidFd returns pidfd of init process.
func (d *lxc) InitPidFd() (*os.File, error) {
	// Load the go-lxc struct
	err := d.initLXC(false)
	if err != nil {
		return nil, err
	}

	return d.c.InitPidFd()
}

// DevptsFd returns dirfd of devpts mount.
func (d *lxc) DevptsFd() (*os.File, error) {
	// Load the go-lxc struct
	err := d.initLXC(false)
	if err != nil {
		return nil, err
	}

	if !liblxc.HasApiExtension("devpts_fd") {
		return nil, fmt.Errorf("Missing devpts_fd extension")
	}

	return d.c.DevptsFd()
}

// CurrentIdmap returns current IDMAP.
func (d *lxc) CurrentIdmap() (*idmap.IdmapSet, error) {
	jsonIdmap, ok := d.LocalConfig()["volatile.idmap.current"]
	if !ok {
		return d.DiskIdmap()
	}

	return idmap.JSONUnmarshal(jsonIdmap)
}

// DiskIdmap returns DISK IDMAP.
func (d *lxc) DiskIdmap() (*idmap.IdmapSet, error) {
	jsonIdmap, ok := d.LocalConfig()["volatile.last_state.idmap"]
	if !ok {
		return nil, nil
	}

	return idmap.JSONUnmarshal(jsonIdmap)
}

// NextIdmap returns next IDMAP.
func (d *lxc) NextIdmap() (*idmap.IdmapSet, error) {
	jsonIdmap, ok := d.LocalConfig()["volatile.idmap.next"]
	if !ok {
		return d.CurrentIdmap()
	}

	return idmap.JSONUnmarshal(jsonIdmap)
}

// statusCode returns instance status code.
func (d *lxc) statusCode() api.StatusCode {
	state, err := d.getLxcState()
	if err != nil {
		return api.Error
	}

	return lxcStatusCode(state)
}

// State returns instance state.
func (d *lxc) State() string {
	return strings.ToUpper(d.statusCode().String())
}

// LogFilePath log file path.
func (d *lxc) LogFilePath() string {
	return filepath.Join(d.LogPath(), "lxc.log")
}

// StoragePool storage pool name.
func (d *lxc) StoragePool() (string, error) {
	poolName, err := d.state.Cluster.GetInstancePool(d.Project(), d.Name())
	if err != nil {
		return "", err
	}

	return poolName, nil
}

func (d *lxc) CGroup() (*cgroup.CGroup, error) {
	// Load the go-lxc struct
	err := d.initLXC(false)
	if err != nil {
		return nil, err
	}

	return d.cgroup(nil)
}

func (d *lxc) cgroup(cc *liblxc.Container) (*cgroup.CGroup, error) {
	rw := lxcCgroupReadWriter{}
	if cc != nil {
		rw.cc = cc
		rw.conf = true
	} else {
		rw.cc = d.c
	}

	cg, err := cgroup.New(&rw)
	if err != nil {
		return nil, err
	}

	cg.UnifiedCapable = liblxc.HasApiExtension("cgroup2")
	return cg, nil
}

type lxcCgroupReadWriter struct {
	cc   *liblxc.Container
	conf bool
}

func (rw *lxcCgroupReadWriter) Get(version cgroup.Backend, controller string, key string) (string, error) {
	if rw.conf {
		lxcKey := fmt.Sprintf("lxc.cgroup.%s", key)

		if version == cgroup.V2 {
			lxcKey = fmt.Sprintf("lxc.cgroup2.%s", key)
		}

		return strings.Join(rw.cc.ConfigItem(lxcKey), "\n"), nil
	}

	return strings.Join(rw.cc.CgroupItem(key), "\n"), nil
}

func (rw *lxcCgroupReadWriter) Set(version cgroup.Backend, controller string, key string, value string) error {
	if rw.conf {
		if version == cgroup.V1 {
			return lxcSetConfigItem(rw.cc, fmt.Sprintf("lxc.cgroup.%s", key), value)
		}

		return lxcSetConfigItem(rw.cc, fmt.Sprintf("lxc.cgroup2.%s", key), value)
	}

	return rw.cc.SetCgroupItem(key, value)
}

// UpdateBackupFile writes the instance's backup.yaml file to storage.
func (d *lxc) UpdateBackupFile() error {
	pool, err := d.getStoragePool()
	if err != nil {
		return err
	}

	return pool.UpdateInstanceBackupFile(d, nil)
}

// SaveConfigFile generates the LXC config file on disk.
func (d *lxc) SaveConfigFile() error {
	err := d.initLXC(true)
	if err != nil {
		return errors.Wrapf(err, "Failed to generate LXC config")
	}

	// Generate the LXC config.
	configPath := filepath.Join(d.LogPath(), "lxc.conf")
	err = d.c.SaveConfigFile(configPath)
	if err != nil {
		os.Remove(configPath)
		return errors.Wrapf(err, "Failed to save LXC config to file %q", configPath)
	}

	return nil
}

// Info returns "lxc" and the currently loaded version of LXC
func (d *lxc) Info() instance.Info {
	return instance.Info{
		Name:    "lxc",
		Version: liblxc.Version(),
		Type:    instancetype.Container,
		Error:   nil,
	}
}
