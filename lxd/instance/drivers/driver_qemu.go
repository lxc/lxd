package drivers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flosch/pongo2"
	"github.com/gorilla/websocket"
	"github.com/kballard/go-shellquote"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v2"

	lxdClient "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/apparmor"
	"github.com/lxc/lxd/lxd/cgroup"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	dbCluster "github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/device"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/device/nictype"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/drivers/qmp"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/instance/operationlock"
	"github.com/lxc/lxd/lxd/lifecycle"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/resources"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	storagePools "github.com/lxc/lxd/lxd/storage"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	pongoTemplate "github.com/lxc/lxd/lxd/template"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/lxd/vsock"
	"github.com/lxc/lxd/lxd/warnings"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/instancewriter"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/subprocess"
	"github.com/lxc/lxd/shared/termios"
	"github.com/lxc/lxd/shared/units"
)

// qemuAsyncIO is used to indicate disk should use unsafe cache I/O.
const qemuUnsafeIO = "unsafeio"

// qemuSerialChardevName is used to communicate state via qmp between Qemu and LXD.
const qemuSerialChardevName = "qemu_serial-chardev"

// qemuDefaultMemSize is the default memory size for VMs if not limit specified.
const qemuDefaultMemSize = "1GiB"

// qemuPCIDeviceIDStart is the first PCI slot used for user configurable devices.
const qemuPCIDeviceIDStart = 4

// qemuDeviceIDPrefix used as part of the name given QEMU devices generated from user added devices.
const qemuDeviceIDPrefix = "dev-lxd_"

// qemuNetDevIDPrefix used as part of the name given QEMU netdevs generated from user added devices.
const qemuNetDevIDPrefix = "lxd_"

var errQemuAgentOffline = fmt.Errorf("LXD VM agent isn't currently running")

var vmConsole = map[int]bool{}
var vmConsoleLock sync.Mutex

type monitorHook func(m *qmp.Monitor) error

// qemuLoad creates a Qemu instance from the supplied InstanceArgs.
func qemuLoad(s *state.State, args db.InstanceArgs, profiles []api.Profile) (instance.Instance, error) {
	// Create the instance struct.
	d := qemuInstantiate(s, args, nil)

	// Expand config and devices.
	err := d.expandConfig(profiles)
	if err != nil {
		return nil, err
	}

	err = d.expandDevices(profiles)
	if err != nil {
		return nil, err
	}

	return d, nil
}

// qemuInstantiate creates a Qemu struct without expanding config. The expandedDevices argument is
// used during device config validation when the devices have already been expanded and we do not
// have access to the profiles used to do it. This can be safely passed as nil if not required.
func qemuInstantiate(s *state.State, args db.InstanceArgs, expandedDevices deviceConfig.Devices) *qemu {
	d := &qemu{
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

	// Get the architecture name.
	archName, err := osarch.ArchitectureName(d.architecture)
	if err == nil {
		d.architectureName = archName
	}

	// Cleanup the zero values.
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

// qemuCreate creates a new storage volume record and returns an initialised Instance.
// Accepts a reverter that revert steps this function does will be added to. It is up to the caller to call the
// revert's Fail() or Success() function as needed.
func qemuCreate(s *state.State, args db.InstanceArgs, revert *revert.Reverter) (instance.Instance, error) {
	// Create the instance struct.
	d := &qemu{
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

	// Get the architecture name.
	archName, err := osarch.ArchitectureName(d.architecture)
	if err == nil {
		d.architectureName = archName
	}

	// Cleanup the zero values.
	if d.expiryDate.IsZero() {
		d.expiryDate = time.Time{}
	}

	if d.creationDate.IsZero() {
		d.creationDate = time.Time{}
	}

	if d.lastUsedDate.IsZero() {
		d.lastUsedDate = time.Time{}
	}

	d.logger.Info("Creating instance", log.Ctx{"ephemeral": d.ephemeral})

	// Load the config.
	err = d.init()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to expand config")
	}

	// Validate expanded config.
	err = instance.ValidConfig(s.OS, d.expandedConfig, false, instancetype.VM)
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

	// Retrieve the instance's storage pool.
	_, rootDiskDevice, err := shared.GetRootDiskDevice(storageInstance.ExpandedDevices().CloneNative())
	if err != nil {
		return nil, err
	}

	if rootDiskDevice["pool"] == "" {
		return nil, fmt.Errorf("The instances's root device is missing the pool property")
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

	// Create a new database entry for the instance's storage volume.
	if d.IsSnapshot() {
		// Copy volume config from parent.
		parentName, _, _ := shared.InstanceGetParentAndSnapshotName(args.Name)
		_, parentVol, err := s.Cluster.GetLocalStoragePoolVolume(args.Project, parentName, db.StoragePoolVolumeTypeVM, d.storagePool.ID())
		if err != nil {
			return nil, errors.Wrapf(err, "Failed loading source volume for snapshot")
		}

		_, err = s.Cluster.CreateStorageVolumeSnapshot(args.Project, args.Name, "", db.StoragePoolVolumeTypeVM, d.storagePool.ID(), parentVol.Config, time.Time{})
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

		_, err = s.Cluster.CreateStoragePoolVolume(args.Project, args.Name, "", db.StoragePoolVolumeTypeVM, d.storagePool.ID(), volumeConfig, db.StoragePoolVolumeContentTypeBlock)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed creating storage record")
		}
	}

	revert.Add(func() {
		s.Cluster.RemoveStoragePoolVolume(args.Project, args.Name, db.StoragePoolVolumeTypeVM, d.storagePool.ID())
	})

	if !d.IsSnapshot() {
		// Add devices to instance.
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

	d.logger.Info("Created instance", log.Ctx{"ephemeral": d.ephemeral})

	if d.snapshot {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotCreated.Event(d, nil))
	} else {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceCreated.Event(d, nil))
	}

	return d, nil
}

// qemu is the QEMU virtual machine driver.
type qemu struct {
	common

	// Cached handles.
	// Do not use these variables directly, instead use their associated get functions so they
	// will be initialised on demand.
	agentClient      *http.Client
	architectureName string
	storagePool      storagePools.Pool
}

// getAgentClient returns the current agent client handle. To avoid TLS setup each time this
// function is called, the handle is cached internally in the Qemu struct.
func (d *qemu) getAgentClient() (*http.Client, error) {
	if d.agentClient != nil {
		return d.agentClient, nil
	}

	// The connection uses mutual authentication, so use the LXD server's key & cert for client.
	agentCert, _, clientCert, clientKey, err := d.generateAgentCert()
	if err != nil {
		return nil, err
	}

	agent, err := vsock.HTTPClient(d.vsockID(), clientCert, clientKey, agentCert)
	if err != nil {
		return nil, err
	}

	return agent, nil
}

// getStoragePool returns the current storage pool handle. To avoid a DB lookup each time this
// function is called, the handle is cached internally in the Qemu struct.
func (d *qemu) getStoragePool() (storagePools.Pool, error) {
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

func (d *qemu) getMonitorEventHandler() func(event string, data map[string]interface{}) {
	// Create local variables from instance properties we need so as not to keep references to instance around
	// after we have returned the callback function.
	projectName := d.Project()
	instanceName := d.Name()
	state := d.state
	logger := d.logger

	return func(event string, data map[string]interface{}) {
		if !shared.StringInSlice(event, []string{"SHUTDOWN", "RESET"}) {
			return // Don't bother loading the instance from DB if we aren't going to handle the event.
		}

		inst, err := instance.LoadByProjectAndName(state, projectName, instanceName)
		if err != nil {
			logger.Error("Failed to load instance", log.Ctx{"err": err})
			return
		}

		if event == "RESET" {
			// As we cannot start QEMU with the -no-reboot flag, because we have to issue a
			// system_reset QMP command to have the devices bootindex applied, then we need to handle
			// the RESET events triggered from a guest-reset operation and prevent QEMU internally
			// restarting the guest, and instead forcefully shutdown and restart the guest from LXD.
			entry, ok := data["reason"]
			if ok && entry == "guest-reset" {
				logger.Debug("Instance guest restart")
				err = inst.Restart(0) // Using 0 timeout will call inst.Stop() then inst.Start().
				if err != nil {
					logger.Error("Failed to restart instance", log.Ctx{"err": err})
					return
				}
			}
		} else if event == "SHUTDOWN" {
			logger.Debug("Instance stopped")

			target := "stop"
			entry, ok := data["reason"]
			if ok && entry == "guest-reset" {
				target = "reboot"
			}

			err = inst.(*qemu).onStop(target)
			if err != nil {
				logger.Error("Failed to cleanly stop instance", log.Ctx{"err": err})
				return
			}
		}
	}
}

// mount the instance's config volume if needed.
func (d *qemu) mount() (*storagePools.MountInfo, error) {
	var pool storagePools.Pool
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

// unmount the instance's config volume if needed.
func (d *qemu) unmount() (bool, error) {
	pool, err := d.getStoragePool()
	if err != nil {
		return false, err
	}

	unmounted, err := pool.UnmountInstance(d, nil)
	if err != nil {
		return false, err
	}

	return unmounted, nil
}

// generateAgentCert creates the necessary server key and certificate if needed.
func (d *qemu) generateAgentCert() (string, string, string, string, error) {
	// Mount the instance's config volume if needed.
	_, err := d.mount()
	if err != nil {
		return "", "", "", "", err
	}
	defer d.unmount()

	agentCertFile := filepath.Join(d.Path(), "agent.crt")
	agentKeyFile := filepath.Join(d.Path(), "agent.key")
	clientCertFile := filepath.Join(d.Path(), "agent-client.crt")
	clientKeyFile := filepath.Join(d.Path(), "agent-client.key")

	// Create server certificate.
	err = shared.FindOrGenCert(agentCertFile, agentKeyFile, false, false)
	if err != nil {
		return "", "", "", "", err
	}

	// Create client certificate.
	err = shared.FindOrGenCert(clientCertFile, clientKeyFile, true, false)
	if err != nil {
		return "", "", "", "", err
	}

	// Read all the files
	agentCert, err := ioutil.ReadFile(agentCertFile)
	if err != nil {
		return "", "", "", "", err
	}

	agentKey, err := ioutil.ReadFile(agentKeyFile)
	if err != nil {
		return "", "", "", "", err
	}

	clientCert, err := ioutil.ReadFile(clientCertFile)
	if err != nil {
		return "", "", "", "", err
	}

	clientKey, err := ioutil.ReadFile(clientKeyFile)
	if err != nil {
		return "", "", "", "", err
	}

	return string(agentCert), string(agentKey), string(clientCert), string(clientKey), nil
}

// Freeze freezes the instance.
func (d *qemu) Freeze() error {
	// Connect to the monitor.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		return err
	}

	// Send the stop command.
	err = monitor.Pause()
	if err != nil {
		return err
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstancePaused.Event(d, nil))
	return nil
}

// configDriveMountPath returns the path for the config drive bind mount.
func (d *qemu) configDriveMountPath() string {
	// Use instance path and config.mount directory rather than devices path to avoid conflicts with an
	// instance disk device mount of the same name.
	return filepath.Join(d.Path(), storageDrivers.VMConfigDriveMountDir)
}

// configDriveMountPathClear attempts to unmount the config drive bind mount and remove the directory.
func (d *qemu) configDriveMountPathClear() error {
	return device.DiskMountClear(d.configDriveMountPath())
}

// configVirtiofsdPaths returns the path for the socket and PID file to use with config drive virtiofsd process.
func (d *qemu) configVirtiofsdPaths() (string, string) {
	sockPath := filepath.Join(d.LogPath(), "virtio-fs.config.sock")
	pidPath := filepath.Join(d.LogPath(), "virtiofsd.pid")

	return sockPath, pidPath
}

// pidWait waits for the QEMU process to exit. Does this in a way that doesn't require the LXD process to be a
// parent of the QEMU process (in order to allow for LXD to be restarted after the VM was started).
// Returns true if process stopped, false if timeout was exceeded.
func (d *qemu) pidWait(timeout time.Duration, op *operationlock.InstanceOperation) bool {
	waitUntil := time.Now().Add(timeout)
	for {
		pid, _ := d.pid()
		if pid <= 0 {
			break
		}

		if time.Now().After(waitUntil) {
			return false
		}

		if op != nil {
			op.Reset() // Reset timeout to 30s.
		}

		time.Sleep(time.Millisecond * time.Duration(250))
	}

	return true
}

// onStop is run when the instance stops.
func (d *qemu) onStop(target string) error {
	d.logger.Debug("onStop hook started", log.Ctx{"target": target})
	defer d.logger.Debug("onStop hook finished", log.Ctx{"target": target})

	// Create/pick up operation.
	op, instanceInitiated, err := d.onStopOperationSetup(target)
	if err != nil {
		return err
	}

	// Wait for QEMU process to end (to avoiding racing start when restarting).
	// Wait up to 5 minutes to allow for flushing any pending data to disk.
	d.logger.Debug("Waiting for VM process to finish")
	waitTimeout := time.Minute * time.Duration(5)
	if d.pidWait(waitTimeout, op) {
		d.logger.Debug("VM process finished")
	} else {
		// Log a warning, but continue clean up as best we can.
		d.logger.Error("VM process failed to stop", log.Ctx{"timeout": waitTimeout})
	}

	// Reset timeout to 30s.
	op.Reset()

	// Cleanup.
	d.cleanupDevices() // Must be called before unmount.
	os.Remove(d.pidFilePath())
	os.Remove(d.monitorPath())
	d.unmount()

	// Record power state.
	err = d.state.Cluster.UpdateInstancePowerState(d.id, "STOPPED")
	if err != nil {
		op.Done(err)
		return err
	}

	// Unload the apparmor profile
	err = apparmor.InstanceUnload(d.state, d)
	if err != nil {
		op.Done(err)
		return err
	}

	if target == "reboot" {
		// Reset timeout to 30s.
		op.Reset()

		err = d.Start(false)
		if err != nil {
			op.Done(err)
			return err
		}

		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRestarted.Event(d, nil))
	} else if d.ephemeral {
		// Reset timeout to 30s.
		op.Reset()

		// Destroy ephemeral virtual machines
		err = d.Delete(true)
		if err != nil {
			op.Done(err)
			return err
		}
	}

	if instanceInitiated {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceShutdown.Event(d, nil))
	}

	op.Done(nil)
	return nil
}

// Shutdown shuts the instance down.
func (d *qemu) Shutdown(timeout time.Duration) error {
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

	// Connect to the monitor.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		op.Done(err)
		return err
	}

	// Get the wait channel.
	chDisconnect, err := monitor.Wait()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			op.Done(nil)
			return nil
		}

		op.Done(err)
		return err
	}

	// Send the system_powerdown command.
	err = monitor.Powerdown()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			op.Done(nil)
			return nil
		}

		op.Done(err)
		return err
	}

	// If timeout provided, block until the VM is not running or the timeout has elapsed.
	if timeout > 0 {
		select {
		case <-chDisconnect:
			break
		case <-time.After(timeout):
			err = fmt.Errorf("Instance was not shutdown after timeout")
			op.Done(err)
			return err
		}
	} else {
		<-chDisconnect // Block until VM is not running if no timeout provided.
	}

	// Wait for onStop.
	err = op.Wait()
	if err != nil && d.IsRunning() {
		return err
	}

	if op.Action() == "stop" {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceShutdown.Event(d, nil))
	}

	return nil
}

// Restart restart the instance.
func (d *qemu) Restart(timeout time.Duration) error {
	err := d.restartCommon(d, timeout)
	if err != nil {
		return err
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRestarted.Event(d, nil))

	return nil
}

func (d *qemu) ovmfPath() string {
	if os.Getenv("LXD_OVMF_PATH") != "" {
		return os.Getenv("LXD_OVMF_PATH")
	}

	return "/usr/share/OVMF"
}

// killQemuProcess kills specified process. Optimistically attempts to wait for the process to fully exit, but does
// not return an error if the Wait call fails. This is because this function is used in scenarios where LXD has
// been restarted after the VM has been started and is no longer the parent of the QEMU process.
// The caller should use another method to ensure that the QEMU process has fully exited instead.
// Returns an error if the Kill signal couldn't be sent to the process (for any other reason apart from the process
// not existing).
func (d *qemu) killQemuProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	err = proc.Kill()
	if err != nil {
		if strings.Contains(err.Error(), "process already finished") {
			return nil
		}

		return err
	}

	// Wait for process to exit, but don't return an error if this fails as it may be called when LXD isn't
	// the parent of the process, and we have still sent the kill signal as per the function's description.
	_, err = proc.Wait()
	if err != nil {
		d.logger.Warn("Failed to collect VM process exit status", log.Ctx{"pid": pid})
	}

	return nil
}

// restoreState restores the saved state of the VM.
func (d *qemu) restoreState(monitor *qmp.Monitor) error {
	stateFile, err := os.Open(d.StatePath())
	if err != nil {
		return err
	}

	uncompressedState, err := gzip.NewReader(stateFile)
	if err != nil {
		stateFile.Close()
		return err
	}

	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		uncompressedState.Close()
		stateFile.Close()
		return err
	}

	go func() {
		io.Copy(pipeWrite, uncompressedState)
		uncompressedState.Close()
		stateFile.Close()
		pipeWrite.Close()
		pipeRead.Close()
	}()

	err = monitor.SendFile("migration", pipeRead)
	if err != nil {
		return err
	}

	err = monitor.MigrateIncoming("fd:migration")
	if err != nil {
		return err
	}

	return nil
}

// saveState dumps the current VM state to disk.
// Once dumped, the VM is in a paused state and it's up to the caller to resume or kill it.
func (d *qemu) saveState(monitor *qmp.Monitor) error {
	os.Remove(d.StatePath())

	// Prepare the state file.
	stateFile, err := os.Create(d.StatePath())
	if err != nil {
		return err
	}

	compressedState, err := gzip.NewWriterLevel(stateFile, gzip.BestSpeed)
	if err != nil {
		stateFile.Close()
		return err
	}

	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		compressedState.Close()
		stateFile.Close()
		return err
	}
	defer pipeRead.Close()
	defer pipeWrite.Close()

	go io.Copy(compressedState, pipeRead)

	// Send the target file to qemu.
	err = monitor.SendFile("migration", pipeWrite)
	if err != nil {
		compressedState.Close()
		stateFile.Close()
		return err
	}

	// Issue the migration command.
	err = monitor.Migrate("fd:migration")
	if err != nil {
		compressedState.Close()
		stateFile.Close()
		return err
	}

	// Close the file to avoid unmount delays.
	compressedState.Close()
	stateFile.Close()

	return nil
}

// Start starts the instance.
func (d *qemu) Start(stateful bool) error {
	d.logger.Debug("Start started", log.Ctx{"stateful": stateful})
	defer d.logger.Debug("Start finished", log.Ctx{"stateful": stateful})

	// Check that we are startable before creating an operation lock, so if the instance is in the
	// process of stopping we don't prevent the stop hooks from running due to our start operation lock.
	err := d.isStartableStatusCode(d.statusCode())
	if err != nil {
		return err
	}

	// Check for stateful.
	if stateful && !shared.IsTrue(d.expandedConfig["migration.stateful"]) {
		return fmt.Errorf("Stateful start requires migration.stateful to be set to true")
	}

	// Setup a new operation.
	exists, op, err := operationlock.CreateWaitGet(d.id, "start", []string{"restart", "restore"}, false, false)
	if err != nil {
		return errors.Wrap(err, "Create instance start operation")
	}
	if exists {
		// An existing matching operation has now succeeded, return.
		return nil
	}
	defer op.Done(nil)

	// Ensure the correct vhost_vsock kernel module is loaded before establishing the vsock.
	err = util.LoadModule("vhost_vsock")
	if err != nil {
		op.Done(err)
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	// Start accumulating external device paths.
	d.devPaths = []string{}

	// Rotate the log file.
	logfile := d.LogFilePath()
	if shared.PathExists(logfile) {
		os.Remove(logfile + ".old")
		err := os.Rename(logfile, logfile+".old")
		if err != nil {
			op.Done(err)
			return err
		}
	}

	// Remove old pid file if needed.
	if shared.PathExists(d.pidFilePath()) {
		err = os.Remove(d.pidFilePath())
		if err != nil {
			op.Done(err)
			return errors.Wrapf(err, "Failed removing old PID file %q", d.pidFilePath())
		}
	}

	// Mount the instance's config volume.
	mountInfo, err := d.mount()
	if err != nil {
		op.Done(err)
		return err
	}

	revert.Add(func() { d.unmount() })

	// Update the backup.yaml file.
	err = d.UpdateBackupFile()
	if err != nil {
		return err
	}

	// Generate the config drive.
	err = d.generateConfigShare()
	if err != nil {
		op.Done(err)
		return err
	}

	// Create all needed paths.
	err = os.MkdirAll(d.LogPath(), 0700)
	if err != nil {
		op.Done(err)
		return err
	}

	err = os.MkdirAll(d.DevicesPath(), 0711)
	if err != nil {
		op.Done(err)
		return err
	}

	err = os.MkdirAll(d.ShmountsPath(), 0711)
	if err != nil {
		op.Done(err)
		return err
	}

	// Generate UUID if not present.
	instUUID := d.localConfig["volatile.uuid"]
	if instUUID == "" {
		instUUID = uuid.New()
		d.VolatileSet(map[string]string{"volatile.uuid": instUUID})
	}

	// Copy OVMF settings firmware to nvram file.
	// This firmware file can be modified by the VM so it must be copied from the defaults.
	if !shared.PathExists(d.nvramPath()) {
		err = d.setupNvram()
		if err != nil {
			op.Done(err)
			return err
		}
	}

	devConfs := make([]*deviceConfig.RunConfig, 0, len(d.expandedDevices))
	postStartHooks := []func() error{}

	// Setup devices in sorted order, this ensures that device mounts are added in path order.
	for _, entry := range d.expandedDevices.Sorted() {
		dev := entry // Ensure device variable has local scope for revert.

		// Start the device.
		runConf, err := d.deviceStart(dev.Name, dev.Config, false)
		if err != nil {
			op.Done(err)
			return errors.Wrapf(err, "Failed to start device %q", dev.Name)
		}

		if runConf == nil {
			continue
		}

		revert.Add(func() {
			err := d.deviceStop(dev.Name, dev.Config, false)
			if err != nil {
				d.logger.Error("Failed to cleanup device", log.Ctx{"devName": dev.Name, "err": err})
			}
		})

		// Add post-start hooks
		if len(runConf.PostHooks) > 0 {
			postStartHooks = append(postStartHooks, runConf.PostHooks...)
		}

		devConfs = append(devConfs, runConf)
	}

	// Setup the config drive readonly bind mount. Important that this come after the root disk device start.
	// in order to allow unmounts triggered by deferred resizes of the root volume.
	configMntPath := d.configDriveMountPath()
	err = d.configDriveMountPathClear()
	if err != nil {
		return errors.Wrapf(err, "Failed cleaning config drive mount path %q", configMntPath)
	}

	err = os.Mkdir(configMntPath, 0700)
	if err != nil {
		return errors.Wrapf(err, "Failed creating device mount path %q for config drive", configMntPath)
	}
	revert.Add(func() { d.configDriveMountPathClear() })

	// Mount the config drive device as readonly. This way it will be readonly irrespective of whether its
	// exported via 9p for virtio-fs.
	configSrcPath := filepath.Join(d.Path(), "config")
	err = device.DiskMount(configSrcPath, configMntPath, true, false, "", "", "none")
	if err != nil {
		return errors.Wrapf(err, "Failed mounting device mount path %q for config drive", configMntPath)
	}

	// Setup virtiofsd for the config drive mount path.
	// This is used by the lxd-agent in preference to 9p (due to its improved performance) and in scenarios
	// where 9p isn't available in the VM guest OS.
	configSockPath, configPIDPath := d.configVirtiofsdPaths()
	err = device.DiskVMVirtiofsdStart(d, configSockPath, configPIDPath, "", configMntPath)
	if err != nil {
		var errUnsupported device.UnsupportedError
		if errors.As(err, &errUnsupported) {
			d.logger.Warn("Unable to use virtio-fs for config drive, using 9p as a fallback", log.Ctx{"err": errUnsupported})

			if errUnsupported == device.ErrMissingVirtiofsd {
				// Create a warning if virtiofsd is missing
				d.state.Cluster.UpsertWarning(d.node, d.project, dbCluster.TypeInstance, d.ID(), db.WarningMissingVirtiofsd, "Using 9p as a fallback")
			} else {
				// Resolve previous warning
				warnings.ResolveWarningsByNodeAndProjectAndType(d.state.Cluster, d.node, d.project, db.WarningMissingVirtiofsd)
			}
		} else {
			// Resolve previous warning
			warnings.ResolveWarningsByNodeAndProjectAndType(d.state.Cluster, d.node, d.project, db.WarningMissingVirtiofsd)
			op.Done(err)
			return errors.Wrapf(err, "Failed to setup virtiofsd for config drive")
		}
	}
	revert.Add(func() { device.DiskVMVirtiofsdStop(configSockPath, configPIDPath) })

	// Get qemu configuration and check qemu is installed.
	qemuPath, qemuBus, err := d.qemuArchConfig(d.architecture)
	if err != nil {
		op.Done(err)
		return err
	}

	// Define a set of files to open and pass their file descriptors to qemu command.
	fdFiles := make([]string, 0)

	confFile, monHooks, err := d.generateQemuConfigFile(mountInfo, qemuBus, devConfs, &fdFiles)
	if err != nil {
		op.Done(err)
		return err
	}

	// Snapshot if needed.
	err = d.startupSnapshot(d)
	if err != nil {
		op.Done(err)
		return err
	}

	// Start QEMU.
	qemuCmd := []string{
		"--",
		qemuPath,
		"-S",
		"-name", d.Name(),
		"-uuid", instUUID,
		"-daemonize",
		"-cpu", "host",
		"-nographic",
		"-serial", "chardev:console",
		"-nodefaults",
		"-no-user-config",
		"-sandbox", "on,obsolete=deny,elevateprivileges=allow,spawn=deny,resourcecontrol=deny",
		"-readconfig", confFile,
		"-spice", d.spiceCmdlineConfig(),
		"-pidfile", d.pidFilePath(),
		"-D", d.LogFilePath(),
	}

	// If stateful, restore now.
	if stateful {
		if !d.stateful {
			err = fmt.Errorf("Instance has no existing state to restore")
			op.Done(err)
			return err
		}

		qemuCmd = append(qemuCmd, "-incoming", "defer")
	} else if d.stateful {
		// Stateless start requested but state is present, delete it.
		err := os.Remove(d.StatePath())
		if err != nil && !os.IsNotExist(err) {
			op.Done(err)
			return err
		}

		d.stateful = false
		err = d.state.Cluster.UpdateInstanceStatefulFlag(d.id, false)
		if err != nil {
			op.Done(err)
			return errors.Wrap(err, "Error updating instance stateful flag")
		}
	}

	// SMBIOS only on x86_64 and aarch64.
	if shared.IntInSlice(d.architecture, []int{osarch.ARCH_64BIT_INTEL_X86, osarch.ARCH_64BIT_ARMV8_LITTLE_ENDIAN}) {
		qemuCmd = append(qemuCmd, "-smbios", "type=2,manufacturer=Canonical Ltd.,product=LXD")
	}

	// Attempt to drop privileges (doesn't work when restoring state).
	if !stateful && d.state.OS.UnprivUser != "" {
		qemuCmd = append(qemuCmd, "-runas", d.state.OS.UnprivUser)

		// Change ownership of config directory files so they are accessible to the
		// unprivileged qemu process so that the 9p share can work.
		//
		// Security note: The 9P share will present the UID owner of these files on the host
		// to the VM. In order to ensure that non-root users in the VM cannot access these
		// files be sure to mount the 9P share in the VM with the "access=0" option to allow
		// only root user in VM to access the mounted share.
		err := filepath.Walk(filepath.Join(d.Path(), "config"),
			func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				err = os.Chown(path, int(d.state.OS.UnprivUID), -1)
				if err != nil {
					return err
				}

				return nil
			})
		if err != nil {
			op.Done(err)
			return err
		}
	}

	// Handle hugepages on architectures where we don't set NUMA nodes.
	if d.architecture != osarch.ARCH_64BIT_INTEL_X86 && shared.IsTrue(d.expandedConfig["limits.memory.hugepages"]) {
		hugetlb, err := util.HugepagesPath()
		if err != nil {
			op.Done(err)
			return err
		}

		qemuCmd = append(qemuCmd, "-mem-path", hugetlb, "-mem-prealloc")
	}

	if d.expandedConfig["raw.qemu"] != "" {
		fields, err := shellquote.Split(d.expandedConfig["raw.qemu"])
		if err != nil {
			op.Done(err)
			return err
		}
		qemuCmd = append(qemuCmd, fields...)
	}

	// Run the qemu command via forklimits so we can selectively increase ulimits.
	forkLimitsCmd := []string{
		"forklimits",
		"limit=memlock:unlimited:unlimited", // Required for PCI passthrough.
	}

	for i := range fdFiles {
		// Pass through any file descriptors as 3+i (as first 3 file descriptors are taken as standard).
		forkLimitsCmd = append(forkLimitsCmd, fmt.Sprintf("fd=%d", 3+i))
	}

	// Setup background process.
	p, err := subprocess.NewProcess(d.state.OS.ExecPath, append(forkLimitsCmd, qemuCmd...), d.EarlyLogFilePath(), d.EarlyLogFilePath())
	if err != nil {
		op.Done(err)
		return err
	}

	// Load the AppArmor profile
	err = apparmor.InstanceLoad(d.state, d)
	if err != nil {
		op.Done(err)
		return err
	}

	p.SetApparmor(apparmor.InstanceProfileName(d))

	// Open any extra files and pass their file handles to qemu command.
	files := []*os.File{}
	for _, file := range fdFiles {
		info, err := os.Stat(file)
		if err != nil {
			err = errors.Wrapf(err, "Error detecting file type %q", file)
			op.Done(err)
			return err
		}

		var f *os.File
		mode := info.Mode()
		if mode&os.ModeSocket != 0 {
			c, err := d.openUnixSocket(file)
			if err != nil {
				err = errors.Wrapf(err, "Error opening socket file %q", file)
				op.Done(err)
				return err
			}

			f, err = c.File()
			if err != nil {
				err = errors.Wrapf(err, "Error getting socket file descriptor %q", file)
				op.Done(err)
				return err
			}
			defer c.Close()
			defer f.Close() // Close file after qemu has started.
		} else {
			f, err = os.OpenFile(file, os.O_RDWR, 0)
			if err != nil {
				err = errors.Wrapf(err, "Error opening exta file %q", file)
				op.Done(err)
				return err
			}
			defer f.Close() // Close file after qemu has started.
		}

		files = append(files, f)
	}

	// Reset timeout to 30s.
	op.Reset()

	err = p.StartWithFiles(files)
	if err != nil {
		op.Done(err)
		return err
	}

	_, err = p.Wait(context.Background())
	if err != nil {
		stderr, _ := ioutil.ReadFile(d.EarlyLogFilePath())
		err = errors.Wrapf(err, "Failed to run: %s: %s", strings.Join(p.Args, " "), string(stderr))
		op.Done(err)
		return err
	}

	pid, err := d.pid()
	if err != nil || pid <= 0 {
		d.logger.Error("Failed to get VM process ID", log.Ctx{"err": err, "pid": pid})
		op.Done(err)
		return err
	}

	revert.Add(func() {
		d.killQemuProcess(pid)
	})

	// Start QMP monitoring.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		op.Done(err)
		return err
	}

	// Apply CPU pinning.
	cpuLimit, ok := d.expandedConfig["limits.cpu"]
	if ok && cpuLimit != "" {
		_, err := strconv.Atoi(cpuLimit)
		if err != nil {
			// Expand to a set of CPU identifiers and get the pinning map.
			_, _, _, pins, _, err := d.cpuTopology(cpuLimit)
			if err != nil {
				op.Done(err)
				return err
			}

			// Get the list of PIDs from the VM.
			pids, err := monitor.GetCPUs()
			if err != nil {
				op.Done(err)
				return err
			}

			// Confirm nothing weird is going on.
			if len(pins) != len(pids) {
				err = fmt.Errorf("QEMU has less vCPUs than configured")
				op.Done(err)
				return err
			}

			for i, pid := range pids {
				set := unix.CPUSet{}
				set.Set(int(pins[uint64(i)]))

				// Apply the pin.
				err := unix.SchedSetaffinity(pid, &set)
				if err != nil {
					op.Done(err)
					return err
				}
			}
		}
	}

	// Run monitor hooks from devices.
	for _, monHook := range monHooks {
		err = monHook(monitor)
		if err != nil {
			op.Done(err)
			return errors.Wrapf(err, "Failed setting up device via monitor")
		}
	}

	// Due to a bug in QEMU, devices added using QMP's device_add command do not have their bootindex option
	// respected (even if added before emuation is started). To workaround this we must reset the VM in order
	// for it to rebuild its boot config and to take into account the devices bootindex settings.
	// This also means we cannot start the QEMU process with the -no-reboot flag and have to handle restarting
	// the process from a guest initiated reset using the event handler returned from getMonitorEventHandler().
	monitor.Reset()

	// Reset timeout to 30s.
	op.Reset()

	// Restore the state.
	if stateful {
		err := d.restoreState(monitor)
		if err != nil {
			op.Done(err)
			return err
		}
	}

	// Start the VM.
	err = monitor.Start()
	if err != nil {
		op.Done(err)
		return err
	}

	// Finish handling stateful start.
	if stateful {
		// Cleanup state.
		os.Remove(d.StatePath())
		d.stateful = false

		err = d.state.Cluster.UpdateInstanceStatefulFlag(d.id, false)
		if err != nil {
			op.Done(err)
			return errors.Wrap(err, "Error updating instance stateful flag")
		}
	}

	// Database updates
	err = d.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Record current state
		err = tx.UpdateInstancePowerState(d.id, "RUNNING")
		if err != nil {
			err = errors.Wrap(err, "Error updating instance state")
			op.Done(err)
			return err
		}

		// Update time instance last started time
		err = tx.UpdateInstanceLastUsedDate(d.id, time.Now().UTC())
		if err != nil {
			err = errors.Wrap(err, "Error updating instance last used")
			op.Done(err)
			return err
		}

		return nil
	})
	if err != nil {
		op.Done(err)
		return err
	}

	revert.Success()

	// Run any post-start hooks.
	err = d.runHooks(postStartHooks)
	if err != nil {
		op.Done(err) // Must come before Stop() otherwise stop will not proceed.

		// Shut down the VM if hooks fail.
		d.Stop(false)
		return err
	}

	if op.Action() == "start" {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceStarted.Event(d, nil))
	}

	return nil
}

// openUnixSocket connects to a UNIX socket and returns the connection.
func (d *qemu) openUnixSocket(sockPath string) (*net.UnixConn, error) {
	addr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		return nil, err
	}

	c, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (d *qemu) setupNvram() error {
	// UEFI only on x86_64 and aarch64.
	if !shared.IntInSlice(d.architecture, []int{osarch.ARCH_64BIT_INTEL_X86, osarch.ARCH_64BIT_ARMV8_LITTLE_ENDIAN}) {
		return nil
	}

	// Mount the instance's config volume.
	_, err := d.mount()
	if err != nil {
		return err
	}
	defer d.unmount()

	srcOvmfFile := filepath.Join(d.ovmfPath(), "OVMF_VARS.fd")
	if d.expandedConfig["security.secureboot"] == "" || shared.IsTrue(d.expandedConfig["security.secureboot"]) {
		srcOvmfFile = filepath.Join(d.ovmfPath(), "OVMF_VARS.ms.fd")
	}

	missingEFIFirmwareErr := fmt.Errorf("Required EFI firmware settings file missing %q", srcOvmfFile)

	if !shared.PathExists(srcOvmfFile) {
		return missingEFIFirmwareErr
	}

	srcOvmfFile, err = filepath.EvalSymlinks(srcOvmfFile)
	if err != nil {
		return errors.Wrapf(err, "Failed resolving EFI firmware symlink %q", srcOvmfFile)
	}

	if !shared.PathExists(srcOvmfFile) {
		return missingEFIFirmwareErr
	}

	os.Remove(d.nvramPath())
	err = shared.FileCopy(srcOvmfFile, d.nvramPath())
	if err != nil {
		return err
	}

	return nil
}

func (d *qemu) qemuArchConfig(arch int) (string, string, error) {
	if arch == osarch.ARCH_64BIT_INTEL_X86 {
		path, err := exec.LookPath("qemu-system-x86_64")
		if err != nil {
			return "", "", err
		}

		return path, "pcie", nil
	} else if arch == osarch.ARCH_64BIT_ARMV8_LITTLE_ENDIAN {
		path, err := exec.LookPath("qemu-system-aarch64")
		if err != nil {
			return "", "", err
		}

		return path, "pcie", nil
	} else if arch == osarch.ARCH_64BIT_POWERPC_LITTLE_ENDIAN {
		path, err := exec.LookPath("qemu-system-ppc64")
		if err != nil {
			return "", "", err
		}

		return path, "pci", nil
	} else if arch == osarch.ARCH_64BIT_S390_BIG_ENDIAN {
		path, err := exec.LookPath("qemu-system-s390x")
		if err != nil {
			return "", "", err
		}

		return path, "ccw", nil
	}

	return "", "", fmt.Errorf("Architecture isn't supported for virtual machines")
}

// RegisterDevices calls the Register() function on all of the instance's devices.
func (d *qemu) RegisterDevices() {
	devices := d.ExpandedDevices()
	for _, entry := range devices.Sorted() {
		dev, _, err := d.deviceLoad(entry.Name, entry.Config)
		if err == device.ErrUnsupportedDevType {
			continue
		}

		if err != nil {
			d.logger.Error("Failed to load device to register", log.Ctx{"err": err, "instance": d.Name(), "device": entry.Name})
			continue
		}

		// Check whether device wants to register for any events.
		err = dev.Register()
		if err != nil {
			d.logger.Error("Failed to register device", log.Ctx{"err": err, "instance": d.Name(), "device": entry.Name})
			continue
		}
	}
}

// SaveConfigFile is not used by VMs because the Qemu config file is generated at start up and is not needed
// after that, so doesn't need to support being regenerated.
func (d *qemu) SaveConfigFile() error {
	return nil
}

// OnHook is the top-level hook handler.
func (d *qemu) OnHook(hookName string, args map[string]string) error {
	return instance.ErrNotImplemented
}

// deviceLoad instantiates and validates a new device and returns it along with enriched config.
func (d *qemu) deviceLoad(deviceName string, rawConfig deviceConfig.Device) (device.Device, deviceConfig.Device, error) {
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

// deviceStart loads a new device and calls its Start() function.
func (d *qemu) deviceStart(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool) (*deviceConfig.RunConfig, error) {
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

	// If runConf supplied, perform any instance specific setup of device.
	if runConf != nil {
		// If instance is running and then live attach device.
		if instanceRunning {
			// Attach network interface if requested.
			if len(runConf.NetworkInterface) > 0 {
				err = d.deviceAttachNIC(deviceName, configCopy, runConf.NetworkInterface)
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

// deviceAttachNIC live attaches a NIC device to the instance.
func (d *qemu) deviceAttachNIC(deviceName string, configCopy map[string]string, netIF []deviceConfig.RunConfigItem) error {
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

	_, qemuBus, err := d.qemuArchConfig(d.architecture)
	if err != nil {
		return err
	}

	// Check if the agent is running.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		return err
	}

	qemuDev := make(map[string]string)

	// PCIe and PCI require a port device name to hotplug the NIC into.
	if shared.StringInSlice(qemuBus, []string{"pcie", "pci"}) {
		pciDevID := qemuPCIDeviceIDStart

		// Iterate through all the instance devices in the same sorted order as is used when allocating the
		// boot time devices in order to find the PCI bus slot device we would have used at boot time.
		// Then attempt to use that same device, assuming it is available.
		for _, dev := range d.expandedDevices.Sorted() {
			if dev.Name == deviceName {
				break // Found our device.
			}

			pciDevID++
		}

		pciDeviceName := fmt.Sprintf("%s%d", busDevicePortPrefix, pciDevID)
		d.logger.Debug("Using PCI bus device to hotplug NIC into", log.Ctx{"device": deviceName, "port": pciDeviceName})
		qemuDev["bus"] = pciDeviceName
		qemuDev["addr"] = "00.0"
	}

	cpuCount, err := d.addCPUMemoryConfig(nil)
	if err != nil {
		return err
	}

	monHook, err := d.addNetDevConfig(cpuCount, qemuBus, qemuDev, nil, netIF)
	if err != nil {
		return err
	}

	err = monHook(monitor)
	if err != nil {
		return err
	}

	return nil
}

// deviceStop loads a new device and calls its Stop() function.
func (d *qemu) deviceStop(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool) error {
	logger := logging.AddContext(d.logger, log.Ctx{"device": deviceName, "type": rawConfig["type"]})
	logger.Debug("Stopping device")

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
			return fmt.Errorf("Device stop validation failed for %q: %v", deviceName, err)
		}

		logger.Error("Device stop validation failed", log.Ctx{"err": err})
	}

	if instanceRunning && !dev.CanHotPlug() {
		return fmt.Errorf("Device cannot be stopped when instance is running")
	}

	runConf, err := dev.Stop()
	if err != nil {
		return err
	}

	if runConf != nil {
		if runConf != nil {
			// Detach NIC from running instance.
			if rawConfig["type"] == "nic" && instanceRunning {
				err = d.deviceDetachNIC(deviceName)
				if err != nil {
					return err
				}
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

// deviceDetachNIC detaches a NIC device from a running instance.
func (d *qemu) deviceDetachNIC(deviceName string) error {
	// Check if the agent is running.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		return err
	}

	// pciDeviceExists checks if the deviceID exists as a bridged PCI device.
	pciDeviceExists := func(deviceID string) (bool, error) {
		pciDevs, err := monitor.QueryPCI()
		if err != nil {
			return false, err
		}

		for _, pciDev := range pciDevs {
			for _, bridgeDev := range pciDev.Bridge.Devices {
				if bridgeDev.DevID == deviceID {
					return true, nil
				}
			}
		}

		return false, nil
	}

	deviceID := fmt.Sprintf("%s%s", qemuDeviceIDPrefix, deviceName)
	netDevID := fmt.Sprintf("%s%s", qemuNetDevIDPrefix, deviceName)

	// Request removal of device.
	err = monitor.RemoveNIC(netDevID, deviceID)
	if err != nil {
		return err
	}

	_, qemuBus, err := d.qemuArchConfig(d.architecture)
	if err != nil {
		return err
	}

	if shared.StringInSlice(qemuBus, []string{"pcie", "pci"}) {
		// Wait until the device is actually removed (or we timeout waiting).
		waitDuration := time.Duration(time.Second * time.Duration(10))
		waitUntil := time.Now().Add(waitDuration)
		for {
			devExists, err := pciDeviceExists(deviceID)
			if err != nil {
				return errors.Wrapf(err, "Failed getting PCI devices to check for NIC detach")
			}

			if !devExists {
				break

			}

			if time.Now().After(waitUntil) {
				return errors.Wrapf(err, "Failed to detach NIC after %v", waitDuration)
			}

			d.logger.Debug("Waiting for NIC device to be detached", log.Ctx{"device": deviceName})
			time.Sleep(time.Second * time.Duration(2))
		}
	}

	return nil
}

func (d *qemu) monitorPath() string {
	return filepath.Join(d.LogPath(), "qemu.monitor")
}

func (d *qemu) nvramPath() string {
	return filepath.Join(d.Path(), "qemu.nvram")
}

func (d *qemu) spicePath() string {
	return filepath.Join(d.LogPath(), "qemu.spice")
}

func (d *qemu) spiceCmdlineConfig() string {
	return fmt.Sprintf("unix=on,disable-ticketing=on,addr=%s", d.spicePath())
}

// generateConfigShare generates the config share directory that will be exported to the VM via
// a 9P share. Due to the unknown size of templates inside the images this directory is created
// inside the VM's config volume so that it can be restricted by quota.
// Requires the instance be mounted before calling this function.
func (d *qemu) generateConfigShare() error {
	configDrivePath := filepath.Join(d.Path(), "config")

	// Create config drive dir if doesn't exist, if it does exist, leave it around so we don't regenerate all
	// files causing unnecessary config drive snapshot usage.
	err := os.MkdirAll(configDrivePath, 0500)
	if err != nil {
		return err
	}

	// Generate the cloud-init config.
	err = os.MkdirAll(filepath.Join(configDrivePath, "cloud-init"), 0500)
	if err != nil {
		return err
	}

	if d.ExpandedConfig()["user.user-data"] != "" {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "user-data"), []byte(d.ExpandedConfig()["user.user-data"]), 0400)
		if err != nil {
			return err
		}
	} else {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "user-data"), []byte("#cloud-config\n"), 0400)
		if err != nil {
			return err
		}
	}

	if d.ExpandedConfig()["user.vendor-data"] != "" {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "vendor-data"), []byte(d.ExpandedConfig()["user.vendor-data"]), 0400)
		if err != nil {
			return err
		}
	} else {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "vendor-data"), []byte("#cloud-config\n"), 0400)
		if err != nil {
			return err
		}
	}

	if d.ExpandedConfig()["user.network-config"] != "" {
		err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "network-config"), []byte(d.ExpandedConfig()["user.network-config"]), 0400)
		if err != nil {
			return err
		}
	} else {
		os.Remove(filepath.Join(configDrivePath, "cloud-init", "network-config"))
	}

	// Append any user.meta-data to our predefined meta-data config.
	err = ioutil.WriteFile(filepath.Join(configDrivePath, "cloud-init", "meta-data"), []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n%s\n", d.Name(), d.Name(), d.ExpandedConfig()["user.meta-data"])), 0400)
	if err != nil {
		return err
	}

	// Add the VM agent.
	lxdAgentSrcPath, err := exec.LookPath("lxd-agent")
	if err != nil {
		d.logger.Warn("lxd-agent not found, skipping its inclusion in the VM config drive", log.Ctx{"err": err})
	} else {
		// Install agent into config drive dir if found.
		lxdAgentSrcPath, err = filepath.EvalSymlinks(lxdAgentSrcPath)
		if err != nil {
			return err
		}

		lxdAgentSrcInfo, err := os.Stat(lxdAgentSrcPath)
		if err != nil {
			return errors.Wrapf(err, "Failed getting info for lxd-agent source %q", lxdAgentSrcPath)
		}

		lxdAgentInstallPath := filepath.Join(configDrivePath, "lxd-agent")
		lxdAgentNeedsInstall := true

		if shared.PathExists(lxdAgentInstallPath) {
			lxdAgentInstallInfo, err := os.Stat(lxdAgentInstallPath)
			if err != nil {
				return errors.Wrapf(err, "Failed getting info for existing lxd-agent install %q", lxdAgentInstallPath)
			}

			if lxdAgentInstallInfo.ModTime() == lxdAgentSrcInfo.ModTime() && lxdAgentInstallInfo.Size() == lxdAgentSrcInfo.Size() {
				lxdAgentNeedsInstall = false
			}
		}

		// Only install the lxd-agent into config drive if the existing one is different to the source one.
		// Otherwise we would end up copying it again and this can cause unnecessary snapshot usage.
		if lxdAgentNeedsInstall {
			d.logger.Debug("Installing lxd-agent", log.Ctx{"srcPath": lxdAgentSrcPath, "installPath": lxdAgentInstallPath})
			err = shared.FileCopy(lxdAgentSrcPath, lxdAgentInstallPath)
			if err != nil {
				return err
			}

			err = os.Chmod(lxdAgentInstallPath, 0500)
			if err != nil {
				return err
			}

			err = os.Chown(lxdAgentInstallPath, 0, 0)
			if err != nil {
				return err
			}

			// Ensure we copy the source file's timestamps so they can be used for comparison later.
			err = os.Chtimes(lxdAgentInstallPath, lxdAgentSrcInfo.ModTime(), lxdAgentSrcInfo.ModTime())
			if err != nil {
				return errors.Wrapf(err, "Failed setting lxd-agent timestamps")
			}
		} else {
			d.logger.Debug("Skipping lxd-agent install as unchanged", log.Ctx{"srcPath": lxdAgentSrcPath, "installPath": lxdAgentInstallPath})
		}
	}

	agentCert, agentKey, clientCert, _, err := d.generateAgentCert()
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "server.crt"), []byte(clientCert), 0400)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "agent.crt"), []byte(agentCert), 0400)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "agent.key"), []byte(agentKey), 0400)
	if err != nil {
		return err
	}

	// Systemd units.
	err = os.MkdirAll(filepath.Join(configDrivePath, "systemd"), 0500)
	if err != nil {
		return err
	}

	lxdAgentServiceUnit := `[Unit]
Description=LXD - agent
Documentation=https://linuxcontainers.org/lxd
ConditionPathExists=/dev/virtio-ports/org.linuxcontainers.lxd
Before=cloud-init.target cloud-init.service cloud-init-local.service
DefaultDependencies=no

[Service]
Type=notify
WorkingDirectory=-/run/lxd_agent
ExecStartPre=/lib/systemd/lxd-agent-setup
ExecStart=/run/lxd_agent/lxd-agent
Restart=on-failure
RestartSec=5s
StartLimitInterval=60
StartLimitBurst=10

[Install]
WantedBy=multi-user.target
`

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "systemd", "lxd-agent.service"), []byte(lxdAgentServiceUnit), 0400)
	if err != nil {
		return err
	}

	lxdAgentSetupScript := `#!/bin/sh
set -eu
PREFIX="/run/lxd_agent"

# Functions.
mount_virtiofs() {
    mount -t virtiofs config "${PREFIX}/.mnt" >/dev/null 2>&1
}

mount_9p() {
    /sbin/modprobe 9pnet_virtio >/dev/null 2>&1 || true
    /bin/mount -t 9p config "${PREFIX}/.mnt" -o access=0,trans=virtio >/dev/null 2>&1
}

fail() {
    umount -l "${PREFIX}" >/dev/null 2>&1 || true
    rmdir "${PREFIX}" >/dev/null 2>&1 || true
    echo "${1}"
    exit 1
}

# Setup the mount target.
umount -l "${PREFIX}" >/dev/null 2>&1 || true
mkdir -p "${PREFIX}"
mount -t tmpfs tmpfs "${PREFIX}" -o mode=0700,size=50M
mkdir -p "${PREFIX}/.mnt"

# Try virtiofs first.
mount_virtiofs || mount_9p || fail "Couldn't mount virtiofs or 9p, failing."

# Copy the data.
cp -Ra "${PREFIX}/.mnt/"* "${PREFIX}"

# Unmount the temporary mount.
umount "${PREFIX}/.mnt"
rmdir "${PREFIX}/.mnt"

# Fix up permissions.
chown -R root:root "${PREFIX}"
`

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "systemd", "lxd-agent-setup"), []byte(lxdAgentSetupScript), 0500)
	if err != nil {
		return err
	}

	// Udev rules
	err = os.MkdirAll(filepath.Join(configDrivePath, "udev"), 0500)
	if err != nil {
		return err
	}

	lxdAgentRules := `ACTION=="add", SYMLINK=="virtio-ports/org.linuxcontainers.lxd", TAG+="systemd", ACTION=="add", RUN+="/bin/systemctl start lxd-agent.service"`
	err = ioutil.WriteFile(filepath.Join(configDrivePath, "udev", "99-lxd-agent.rules"), []byte(lxdAgentRules), 0400)
	if err != nil {
		return err
	}

	// Install script for manual installs.
	lxdConfigShareInstall := `#!/bin/sh
if [ ! -e "systemd" ] || [ ! -e "lxd-agent" ]; then
    echo "This script must be run from within the 9p mount"
    exit 1
fi

if [ ! -e "/lib/systemd/system" ]; then
    echo "This script only works on systemd systems"
    exit 1
fi

# Cleanup former units.
rm -f /lib/systemd/system/lxd-agent-9p.service \
    /lib/systemd/system/lxd-agent-virtiofs.service \
    /etc/systemd/system/multi-user.target.wants/lxd-agent-9p.service \
    /etc/systemd/system/multi-user.target.wants/lxd-agent-virtiofs.service

# Install the units.
cp udev/99-lxd-agent.rules /lib/udev/rules.d/
cp systemd/lxd-agent.service /lib/systemd/system/
cp systemd/lxd-agent-setup /lib/systemd/
systemctl daemon-reload
systemctl enable lxd-agent.service

echo ""
echo "LXD agent has been installed, reboot to confirm setup."
echo "To start it now, unmount this filesystem and run: systemctl start lxd-agent"
`

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "install.sh"), []byte(lxdConfigShareInstall), 0700)
	if err != nil {
		return err
	}

	// Instance data for devlxd.
	err = d.writeInstanceData()
	if err != nil {
		return err
	}

	// Templated files.
	templateFilesPath := filepath.Join(configDrivePath, "files")

	// Clear path and recreate.
	os.RemoveAll(templateFilesPath)
	err = os.MkdirAll(templateFilesPath, 0500)
	if err != nil {
		return err
	}

	// Template anything that needs templating.
	key := "volatile.apply_template"
	if d.localConfig[key] != "" {
		// Run any template that needs running.
		err = d.templateApplyNow(instance.TemplateTrigger(d.localConfig[key]), templateFilesPath)
		if err != nil {
			return err
		}

		// Remove the volatile key from the DB.
		err := d.state.Cluster.DeleteInstanceConfigKey(d.id, key)
		if err != nil {
			return err
		}
	}

	err = d.templateApplyNow("start", templateFilesPath)
	if err != nil {
		return err
	}

	// Copy the template metadata itself too.
	metaPath := filepath.Join(d.Path(), "metadata.yaml")
	if shared.PathExists(metaPath) {
		err = shared.FileCopy(metaPath, filepath.Join(templateFilesPath, "metadata.yaml"))
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *qemu) templateApplyNow(trigger instance.TemplateTrigger, path string) error {
	// If there's no metadata, just return.
	fname := filepath.Join(d.Path(), "metadata.yaml")
	if !shared.PathExists(fname) {
		return nil
	}

	// Parse the metadata.
	content, err := ioutil.ReadFile(fname)
	if err != nil {
		return errors.Wrap(err, "Failed to read metadata")
	}

	metadata := new(api.ImageMetadata)
	err = yaml.Unmarshal(content, &metadata)
	if err != nil {
		return errors.Wrapf(err, "Could not parse %s", fname)
	}

	// Figure out the instance architecture.
	arch, err := osarch.ArchitectureName(d.architecture)
	if err != nil {
		arch, err = osarch.ArchitectureName(d.state.OS.Architectures[0])
		if err != nil {
			return errors.Wrap(err, "Failed to detect system architecture")
		}
	}

	// Generate the container metadata.
	instanceMeta := make(map[string]string)
	instanceMeta["name"] = d.name
	instanceMeta["type"] = "virtual-machine"
	instanceMeta["architecture"] = arch

	if d.ephemeral {
		instanceMeta["ephemeral"] = "true"
	} else {
		instanceMeta["ephemeral"] = "false"
	}

	// Go through the templates.
	for tplPath, tpl := range metadata.Templates {
		err = func(tplPath string, tpl *api.ImageMetadataTemplate) error {
			var w *os.File

			// Check if the template should be applied now.
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

			// Create the file itself.
			w, err = os.Create(filepath.Join(path, fmt.Sprintf("%s.out", tpl.Template)))
			if err != nil {
				return err
			}

			// Fix ownership and mode.
			w.Chmod(0644)
			defer w.Close()

			// Read the template.
			tplString, err := ioutil.ReadFile(filepath.Join(d.TemplatesPath(), tpl.Template))
			if err != nil {
				return errors.Wrap(err, "Failed to read template file")
			}

			// Restrict filesystem access to within the container's rootfs.
			tplSet := pongo2.NewSet(fmt.Sprintf("%s-%s", d.name, tpl.Template), pongoTemplate.ChrootLoader{Path: d.TemplatesPath()})
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

			// Render the template.
			tplRender.ExecuteWriter(pongo2.Context{"trigger": trigger,
				"path":       tplPath,
				"instance":   instanceMeta,
				"container":  instanceMeta, // FIXME: remove once most images have moved away.
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

// deviceBootPriorities returns a map keyed on device name containing the boot index to use.
// Qemu tries to boot devices in order of boot index (lowest first).
func (d *qemu) deviceBootPriorities() (map[string]int, error) {
	type devicePrios struct {
		Name     string
		BootPrio uint32
	}

	devices := []devicePrios{}

	for _, dev := range d.expandedDevices.Sorted() {
		if dev.Config["type"] != "disk" && dev.Config["type"] != "nic" {
			continue
		}

		bootPrio := uint32(0) // Default to lowest priority.
		if dev.Config["boot.priority"] != "" {
			prio, err := strconv.ParseInt(dev.Config["boot.priority"], 10, 32)
			if err != nil {
				return nil, errors.Wrapf(err, "Invalid boot.priority for device %q", dev.Name)
			}
			bootPrio = uint32(prio)
		} else if dev.Config["path"] == "/" {
			bootPrio = 1 // Set boot priority of root disk higher than any device without a boot prio.
		}

		devices = append(devices, devicePrios{Name: dev.Name, BootPrio: bootPrio})
	}

	// Sort devices by priority (use SliceStable so that devices with the same boot priority stay in the same
	// order each boot based on the device order provided by the d.expandedDevices.Sorted() function).
	// This is important because as well as providing a predicable boot index order, the boot index number can
	// also be used for other properties (such as disk SCSI ID) which can result in it being given different
	// device names inside the guest based on the device order.
	sort.SliceStable(devices, func(i, j int) bool { return devices[i].BootPrio > devices[j].BootPrio })

	sortedDevs := make(map[string]int, len(devices))
	for bootIndex, dev := range devices {
		sortedDevs[dev.Name] = bootIndex
	}

	return sortedDevs, nil
}

// generateQemuConfigFile writes the qemu config file and returns its location.
// It writes the config file inside the VM's log path.
func (d *qemu) generateQemuConfigFile(mountInfo *storagePools.MountInfo, busName string, devConfs []*deviceConfig.RunConfig, fdFiles *[]string) (string, []monitorHook, error) {
	var sb *strings.Builder = &strings.Builder{}
	var monHooks []monitorHook

	err := qemuBase.Execute(sb, map[string]interface{}{
		"architecture": d.architectureName,
	})
	if err != nil {
		return "", nil, err
	}

	cpuCount, err := d.addCPUMemoryConfig(sb)
	if err != nil {
		return "", nil, err
	}

	err = qemuDriveFirmware.Execute(sb, map[string]interface{}{
		"architecture": d.architectureName,
		"roPath":       filepath.Join(d.ovmfPath(), "OVMF_CODE.fd"),
		"nvramPath":    d.nvramPath(),
	})
	if err != nil {
		return "", nil, err
	}

	err = qemuControlSocket.Execute(sb, map[string]interface{}{
		"path": d.monitorPath(),
	})
	if err != nil {
		return "", nil, err
	}

	// Setup the bus allocator.
	bus := qemuNewBus(busName, sb)

	// Now add the fixed set of devices. The multi-function groups used for these fixed internal devices are
	// specifically chosen to ensure that we consume exactly 4 PCI bus ports (on PCIe bus). This ensures that
	// the first user device NIC added will use the 5th PCI bus port and will be consistently named enp5s0
	// on PCIe (which we need to maintain compatibility with network configuration in our existing VM images).
	// It's also meant to group all low-bandwidth internal devices onto a single address. PCIe bus allows a
	// total of 256 devices, but this assumes 32 chassis * 8 function. By using VFs for the internal fixed
	// devices we avoid consuming a chassis for each one. See also the qemuPCIDeviceIDStart constant.
	devBus, devAddr, multi := bus.allocate(busFunctionGroupGeneric)
	err = qemuBalloon.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,
	})
	if err != nil {
		return "", nil, err
	}

	devBus, devAddr, multi = bus.allocate(busFunctionGroupGeneric)
	err = qemuRNG.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,
	})
	if err != nil {
		return "", nil, err
	}

	devBus, devAddr, multi = bus.allocate(busFunctionGroupGeneric)
	err = qemuKeyboard.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,
	})
	if err != nil {
		return "", nil, err
	}

	devBus, devAddr, multi = bus.allocate(busFunctionGroupGeneric)
	err = qemuTablet.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,
	})
	if err != nil {
		return "", nil, err
	}

	devBus, devAddr, multi = bus.allocate(busFunctionGroupGeneric)
	err = qemuVsock.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,

		"vsockID": d.vsockID(),
	})
	if err != nil {
		return "", nil, err
	}

	devBus, devAddr, multi = bus.allocate(busFunctionGroupGeneric)
	err = qemuSerial.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,

		"chardevName":      qemuSerialChardevName,
		"ringbufSizeBytes": qmp.RingbufSize,
	})
	if err != nil {
		return "", nil, err
	}

	// s390x doesn't really have USB.
	if d.architecture != osarch.ARCH_64BIT_S390_BIG_ENDIAN {
		devBus, devAddr, multi = bus.allocate(busFunctionGroupGeneric)
		err = qemuUSB.Execute(sb, map[string]interface{}{
			"bus":           bus.name,
			"devBus":        devBus,
			"devAddr":       devAddr,
			"multifunction": multi,
		})
		if err != nil {
			return "", nil, err
		}
	}

	devBus, devAddr, multi = bus.allocate(busFunctionGroupNone)
	err = qemuSCSI.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,
	})
	if err != nil {
		return "", nil, err
	}

	// Always export the config directory as a 9p config drive, in case the host or VM guest doesn't support
	// virtio-fs.
	devBus, devAddr, multi = bus.allocate(busFunctionGroup9p)
	err = qemuDriveConfig.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,
		"protocol":      "9p",

		"path": d.configDriveMountPath(),
	})
	if err != nil {
		return "", nil, err
	}

	// If virtiofsd is running for the config directory then export the config drive via virtio-fs.
	// This is used by the lxd-agent in preference to 9p (due to its improved performance) and in scenarios
	// where 9p isn't available in the VM guest OS.
	configSockPath, _ := d.configVirtiofsdPaths()
	if shared.PathExists(configSockPath) {
		devBus, devAddr, multi = bus.allocate(busFunctionGroup9p)
		err = qemuDriveConfig.Execute(sb, map[string]interface{}{
			"bus":           bus.name,
			"devBus":        devBus,
			"devAddr":       devAddr,
			"multifunction": multi,
			"protocol":      "virtio-fs",

			"path": configSockPath,
		})
		if err != nil {
			return "", nil, err
		}
	}

	devBus, devAddr, multi = bus.allocate(busFunctionGroupNone)
	err = qemuGPU.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,

		"architecture": d.architectureName,
	})
	if err != nil {
		return "", nil, err
	}

	// Dynamic devices.
	bootIndexes, err := d.deviceBootPriorities()
	if err != nil {
		return "", nil, errors.Wrap(err, "Error calculating boot indexes")
	}

	// Record the mounts we are going to do inside the VM using the agent.
	agentMounts := []instancetype.VMAgentMount{}

	// These devices are sorted so that NICs are added first to ensure that the first NIC can use the 5th
	// PCIe bus port and will be consistently named enp5s0 for compatibility with network configuration in our
	// existing VM images. Even on non-PCIe busses having NICs first means that their names won't change when
	// other devices are added.
	for _, runConf := range devConfs {
		// Add drive devices.
		if len(runConf.Mounts) > 0 {
			for _, drive := range runConf.Mounts {
				if drive.TargetPath == "/" {
					err = d.addRootDriveConfig(sb, mountInfo, bootIndexes, drive)
				} else if drive.FSType == "9p" {
					err = d.addDriveDirConfig(sb, bus, fdFiles, &agentMounts, drive)
				} else {
					err = d.addDriveConfig(sb, bootIndexes, drive)
				}
				if err != nil {
					return "", nil, err
				}
			}
		}

		// Add network device.
		if len(runConf.NetworkInterface) > 0 {
			qemuDev := make(map[string]string)
			if shared.StringInSlice(bus.name, []string{"pcie", "pci"}) {
				// Allocate a PCI(e) port and write it to the config file so QMP can "hotplug" the
				// NIC into it later.
				devBus, devAddr, multi := bus.allocate(busFunctionGroupNone)

				// Populate the qemu device with port info.
				qemuDev["bus"] = devBus
				qemuDev["addr"] = devAddr

				if multi {
					qemuDev["multifunction"] = "on"
				}
			}

			monHook, err := d.addNetDevConfig(cpuCount, bus.name, qemuDev, bootIndexes, runConf.NetworkInterface)
			if err != nil {
				return "", nil, err
			}

			monHooks = append(monHooks, monHook)
		}

		// Add GPU device.
		if len(runConf.GPUDevice) > 0 {
			err = d.addGPUDevConfig(sb, bus, runConf.GPUDevice)
			if err != nil {
				return "", nil, err
			}
		}

		// Add PCI device.
		if len(runConf.PCIDevice) > 0 {
			err = d.addPCIDevConfig(sb, bus, runConf.PCIDevice)
			if err != nil {
				return "", nil, err
			}
		}

		// Add USB device.
		if len(runConf.USBDevice) > 0 {
			err = d.addUSBDeviceConfig(sb, bus, runConf.USBDevice)
			if err != nil {
				return "", nil, err
			}
		}

		// Add TPM device.
		if len(runConf.TPMDevice) > 0 {
			err = d.addTPMDeviceConfig(sb, runConf.TPMDevice)
			if err != nil {
				return "", nil, err
			}
		}

	}

	// Allocate 4 PCI slots for hotplug devices.
	for i := 0; i < 4; i++ {
		bus.allocate(busFunctionGroupNone)
	}

	// Write the agent mount config.
	agentMountJSON, err := json.Marshal(agentMounts)
	if err != nil {
		return "", nil, errors.Wrapf(err, "Failed marshalling agent mounts to JSON")
	}

	agentMountFile := filepath.Join(d.Path(), "config", "agent-mounts.json")
	err = ioutil.WriteFile(agentMountFile, agentMountJSON, 0400)
	if err != nil {
		return "", nil, errors.Wrapf(err, "Failed writing agent mounts file")
	}

	// Write the config file to disk.
	configPath := filepath.Join(d.LogPath(), "qemu.conf")
	return configPath, monHooks, ioutil.WriteFile(configPath, []byte(sb.String()), 0640)
}

// addCPUMemoryConfig adds the qemu config required for setting the number of virtualised CPUs and memory.
// If sb is nil then no config is written and instead just the CPU count is returned.
func (d *qemu) addCPUMemoryConfig(sb *strings.Builder) (int, error) {
	// Default to a single core.
	cpus := d.expandedConfig["limits.cpu"]
	if cpus == "" {
		cpus = "1"
	}

	ctx := map[string]interface{}{
		"architecture": d.architectureName,
	}

	cpuCount, err := strconv.Atoi(cpus)
	hostNodes := []uint64{}
	if err == nil {
		// If not pinning, default to exposing cores.
		ctx["cpuCount"] = cpuCount
		ctx["cpuSockets"] = 1
		ctx["cpuCores"] = cpuCount
		ctx["cpuThreads"] = 1
		hostNodes = []uint64{0}
	} else {
		// Expand to a set of CPU identifiers and get the pinning map.
		nrSockets, nrCores, nrThreads, vcpus, numaNodes, err := d.cpuTopology(cpus)
		if err != nil {
			return -1, err
		}

		// Figure out socket-id/core-id/thread-id for all vcpus.
		vcpuSocket := map[uint64]uint64{}
		vcpuCore := map[uint64]uint64{}
		vcpuThread := map[uint64]uint64{}
		vcpu := uint64(0)
		for i := 0; i < nrSockets; i++ {
			for j := 0; j < nrCores; j++ {
				for k := 0; k < nrThreads; k++ {
					vcpuSocket[vcpu] = uint64(i)
					vcpuCore[vcpu] = uint64(j)
					vcpuThread[vcpu] = uint64(k)
					vcpu++
				}
			}
		}

		// Prepare the NUMA map.
		numa := []map[string]uint64{}
		numaIDs := []uint64{}
		numaNode := uint64(0)
		for hostNode, entry := range numaNodes {
			hostNodes = append(hostNodes, hostNode)

			numaIDs = append(numaIDs, numaNode)
			for _, vcpu := range entry {
				numa = append(numa, map[string]uint64{
					"node":   numaNode,
					"socket": vcpuSocket[vcpu],
					"core":   vcpuCore[vcpu],
					"thread": vcpuThread[vcpu],
				})
			}

			numaNode++
		}

		// Prepare context.
		ctx["cpuCount"] = len(vcpus)
		ctx["cpuSockets"] = nrSockets
		ctx["cpuCores"] = nrCores
		ctx["cpuThreads"] = nrThreads
		ctx["cpuNumaNodes"] = numaIDs
		ctx["cpuNumaMapping"] = numa
		ctx["cpuNumaHostNodes"] = hostNodes
	}

	// Configure memory limit.
	memSize := d.expandedConfig["limits.memory"]
	if memSize == "" {
		memSize = qemuDefaultMemSize // Default if no memory limit specified.
	}

	memSizeBytes, err := units.ParseByteSizeString(memSize)
	if err != nil {
		return -1, fmt.Errorf("limits.memory invalid: %v", err)
	}

	ctx["hugepages"] = ""
	if shared.IsTrue(d.expandedConfig["limits.memory.hugepages"]) {
		hugetlb, err := util.HugepagesPath()
		if err != nil {
			return -1, err
		}

		ctx["hugepages"] = hugetlb
	}

	// Determine per-node memory limit.
	memSizeBytes = memSizeBytes / 1024 / 1024
	nodeMemory := int64(memSizeBytes / int64(len(hostNodes)))
	memSizeBytes = nodeMemory * int64(len(hostNodes))
	ctx["memory"] = nodeMemory

	if sb != nil {
		err = qemuMemory.Execute(sb, map[string]interface{}{
			"architecture": d.architectureName,
			"memSizeBytes": memSizeBytes,
		})

		if err != nil {
			return -1, err
		}

		err = qemuCPU.Execute(sb, ctx)
		if err != nil {
			return -1, err
		}
	}

	// Configure the CPU limit.
	return ctx["cpuCount"].(int), nil
}

// addFileDescriptor adds a file path to the list of files to open and pass file descriptor to qemu.
// Returns the file descriptor number that qemu will receive.
func (d *qemu) addFileDescriptor(fdFiles *[]string, filePath string) int {
	// Append the tap device file path to the list of files to be opened and passed to qemu.
	*fdFiles = append(*fdFiles, filePath)
	return 2 + len(*fdFiles) // Use 2+fdFiles count, as first user file descriptor is 3.
}

// addRootDriveConfig adds the qemu config required for adding the root drive.
func (d *qemu) addRootDriveConfig(sb *strings.Builder, mountInfo *storagePools.MountInfo, bootIndexes map[string]int, rootDriveConf deviceConfig.MountEntryItem) error {
	if rootDriveConf.TargetPath != "/" {
		return fmt.Errorf("Non-root drive config supplied")
	}

	pool, err := d.getStoragePool()
	if err != nil {
		return err
	}

	if mountInfo.DiskPath == "" {
		return fmt.Errorf("No disk path available from mount")
	}

	// Generate a new device config with the root device path expanded.
	driveConf := deviceConfig.MountEntryItem{
		DevName: rootDriveConf.DevName,
		DevPath: mountInfo.DiskPath,
	}

	// If the storage pool is on ZFS and backed by a loop file and we can't use DirectIO, then resort to
	// unsafe async I/O to avoid kernel lock up when running ZFS storage pools in an image file on another FS.
	driverInfo := pool.Driver().Info()
	driverConf := pool.Driver().Config()
	if driverInfo.Name == "zfs" && !driverInfo.DirectIO && shared.PathExists(driverConf["source"]) && !shared.IsBlockdevPath(driverConf["source"]) {
		driveConf.Opts = append(driveConf.Opts, qemuUnsafeIO)
	}

	return d.addDriveConfig(sb, bootIndexes, driveConf)
}

// addDriveDirConfig adds the qemu config required for adding a supplementary drive directory share.
func (d *qemu) addDriveDirConfig(sb *strings.Builder, bus *qemuBus, fdFiles *[]string, agentMounts *[]instancetype.VMAgentMount, driveConf deviceConfig.MountEntryItem) error {
	mountTag := fmt.Sprintf("lxd_%s", driveConf.DevName)

	agentMount := instancetype.VMAgentMount{
		Source: mountTag,
		Target: driveConf.TargetPath,
		FSType: driveConf.FSType,
	}

	// If mount type is 9p, we need to specify to use the virtio transport to support more VM guest OSes.
	if agentMount.FSType == "9p" {
		agentMount.Options = append(agentMount.Options, "trans=virtio")
	}

	readonly := shared.StringInSlice("ro", driveConf.Opts)

	// Indicate to agent to mount this readonly. Note: This is purely to indicate to VM guest that this is
	// readonly, it should *not* be used as a security measure, as the VM guest could remount it R/W.
	if readonly {
		agentMount.Options = append(agentMount.Options, "ro")
	}

	// Record the 9p mount for the agent.
	*agentMounts = append(*agentMounts, agentMount)

	// Check if the disk device has provided a virtiofsd socket path.
	var virtiofsdSockPath string
	for _, opt := range driveConf.Opts {
		if strings.HasPrefix(opt, fmt.Sprintf("%s=", device.DiskVirtiofsdSockMountOpt)) {
			parts := strings.SplitN(opt, "=", 2)
			virtiofsdSockPath = parts[1]
		}
	}

	// If there is a virtiofsd socket path setup the virtio-fs share.
	if virtiofsdSockPath != "" {
		if !shared.PathExists(virtiofsdSockPath) {
			return fmt.Errorf("virtiofsd socket path %q doesn't exist", virtiofsdSockPath)
		}

		devBus, devAddr, multi := bus.allocate(busFunctionGroup9p)

		// Add virtio-fs device as this will be preferred over 9p.
		err := qemuDriveDir.Execute(sb, map[string]interface{}{
			"bus":           bus.name,
			"devBus":        devBus,
			"devAddr":       devAddr,
			"multifunction": multi,

			"devName":  driveConf.DevName,
			"mountTag": mountTag,
			"path":     virtiofsdSockPath,
			"protocol": "virtio-fs",
		})
		if err != nil {
			return err
		}
	}

	// Add 9p share config.
	devBus, devAddr, multi := bus.allocate(busFunctionGroup9p)
	proxyFD := d.addFileDescriptor(fdFiles, driveConf.DevPath)

	return qemuDriveDir.Execute(sb, map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,

		"devName":  driveConf.DevName,
		"mountTag": mountTag,
		"proxyFD":  proxyFD, // Pass by file descriptor, so don't add to d.devPaths for apparmor access.
		"readonly": readonly,
		"protocol": "9p",
	})
}

// addDriveConfig adds the qemu config required for adding a supplementary drive.
func (d *qemu) addDriveConfig(sb *strings.Builder, bootIndexes map[string]int, driveConf deviceConfig.MountEntryItem) error {
	// Use native kernel async IO and O_DIRECT by default.
	aioMode := "native"
	cacheMode := "none" // Bypass host cache, use O_DIRECT semantics.
	media := "disk"

	readonly := shared.StringInSlice("ro", driveConf.Opts)

	// If drive config indicates we need to use unsafe I/O then use it.
	if shared.StringInSlice(qemuUnsafeIO, driveConf.Opts) {
		d.logger.Warn("Using unsafe cache I/O", log.Ctx{"DevPath": driveConf.DevPath})
		aioMode = "threads"
		cacheMode = "unsafe" // Use host cache, but ignore all sync requests from guest.
	} else if shared.PathExists(driveConf.DevPath) && !shared.IsBlockdevPath(driveConf.DevPath) {
		// Disk dev path is a file, check whether it is located on a ZFS filesystem.
		fsType, err := util.FilesystemDetect(driveConf.DevPath)
		if err != nil {
			return errors.Wrapf(err, "Failed detecting filesystem type of %q", driveConf.DevPath)
		}

		// If backing FS is ZFS or BTRFS, avoid using direct I/O and use host page cache only.
		// We've seen ZFS lock up and BTRFS checksum issues when using direct I/O on image files.
		if fsType == "zfs" || fsType == "btrfs" {
			if driveConf.FSType != "iso9660" {
				// Only warn about using writeback cache if the drive image is writable.
				d.logger.Warn("Using writeback cache I/O", log.Ctx{"DevPath": driveConf.DevPath, "fsType": fsType})
			}

			aioMode = "threads"
			cacheMode = "writeback" // Use host cache, with neither O_DSYNC nor O_DIRECT semantics.
		}

		// Special case ISO images as cdroms.
		if strings.HasSuffix(driveConf.DevPath, ".iso") {
			media = "cdrom"
		}
	}

	if !strings.HasPrefix(driveConf.DevPath, "rbd:") {
		// Add path to external devPaths. This way, the path will be included in the apparmor profile.
		d.devPaths = append(d.devPaths, driveConf.DevPath)
	}

	return qemuDrive.Execute(sb, map[string]interface{}{
		"devName":   driveConf.DevName,
		"devPath":   driveConf.DevPath,
		"bootIndex": bootIndexes[driveConf.DevName],
		"cacheMode": cacheMode,
		"aioMode":   aioMode,
		"media":     media,
		"shared":    driveConf.TargetPath != "/" && !strings.HasPrefix(driveConf.DevPath, "rbd:"),
		"readonly":  readonly,
	})
}

// addNetDevConfig adds the qemu config required for adding a network device.
// The qemuDev map is expected to be preconfigured with the settings for an existing port to use for the device.
func (d *qemu) addNetDevConfig(cpuCount int, busName string, qemuDev map[string]string, bootIndexes map[string]int, nicConfig []deviceConfig.RunConfigItem) (monitorHook, error) {
	revert := revert.New()
	defer revert.Fail()

	var devName, nicName, devHwaddr, pciSlotName, pciIOMMUGroup string
	for _, nicItem := range nicConfig {
		if nicItem.Key == "devName" {
			devName = nicItem.Value
		} else if nicItem.Key == "link" {
			nicName = nicItem.Value
		} else if nicItem.Key == "hwaddr" {
			devHwaddr = nicItem.Value
		} else if nicItem.Key == "pciSlotName" {
			pciSlotName = nicItem.Value
		} else if nicItem.Key == "pciIOMMUGroup" {
			pciIOMMUGroup = nicItem.Value
		}
	}

	var qemuNetDev map[string]interface{}
	qemuDev["id"] = fmt.Sprintf("%s%s", qemuDeviceIDPrefix, devName)

	if len(bootIndexes) > 0 {
		bootIndex, found := bootIndexes[devName]
		if found {
			qemuDev["bootindex"] = strconv.Itoa(bootIndex)
		}
	}

	// Detect MACVTAP interface types and figure out which tap device is being used.
	// This is so we can open a file handle to the tap device and pass it to the qemu process.
	if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/macvtap", nicName)) {
		content, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/ifindex", nicName))
		if err != nil {
			return nil, errors.Wrapf(err, "Error getting tap device ifindex")
		}

		ifindex, err := strconv.Atoi(strings.TrimSpace(string(content)))
		if err != nil {
			return nil, errors.Wrapf(err, "Error parsing tap device ifindex")
		}

		qemuNetDev = map[string]interface{}{
			"id":    fmt.Sprintf("%s%s", qemuNetDevIDPrefix, devName),
			"type":  "tap",
			"vhost": true,
			"fd":    fmt.Sprintf("/dev/tap%d", ifindex), // Indicates the file to open and the FD name.
		}

		if shared.StringInSlice(busName, []string{"pcie", "pci"}) {
			qemuDev["driver"] = "virtio-net-pci"
		} else if busName == "ccw" {
			qemuDev["driver"] = "virtio-net-ccw"
		}

		qemuDev["netdev"] = qemuNetDev["id"].(string)
		qemuDev["mac"] = devHwaddr
	} else if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/tun_flags", nicName)) {
		// Detect TAP (via TUN driver) device.
		qemuNetDev = map[string]interface{}{
			"id":         fmt.Sprintf("%s%s", qemuNetDevIDPrefix, devName),
			"type":       "tap",
			"vhost":      true,
			"script":     "no",
			"downscript": "no",
			"ifname":     nicName,
		}

		// Number of queues is the same as number of vCPUs. Run with a minimum of two queues.
		queueCount := cpuCount
		if queueCount < 2 {
			queueCount = 2
		}

		if queueCount > 0 {
			qemuNetDev["queues"] = queueCount
		}

		if shared.StringInSlice(busName, []string{"pcie", "pci"}) {
			qemuDev["driver"] = "virtio-net-pci"
		} else if busName == "ccw" {
			qemuDev["driver"] = "virtio-net-ccw"
		}

		// Number of vectors is number of vCPUs * 2 (RX/TX) + 2 (config/control MSI-X).
		vectors := 2*queueCount + 2
		if vectors > 0 {
			qemuDev["mq"] = "on"
			if shared.StringInSlice(busName, []string{"pcie", "pci"}) {
				qemuDev["vectors"] = strconv.Itoa(vectors)
			}
		}

		qemuDev["netdev"] = qemuNetDev["id"].(string)
		qemuDev["mac"] = devHwaddr
	} else if pciSlotName != "" {
		// Detect physical passthrough device.
		if shared.StringInSlice(busName, []string{"pcie", "pci"}) {
			qemuDev["driver"] = "vfio-pci"
		} else if busName == "ccw" {
			qemuDev["driver"] = "vfio-ccw"
		}

		qemuDev["host"] = pciSlotName

		if d.state.OS.UnprivUser != "" {
			if pciIOMMUGroup == "" {
				return nil, fmt.Errorf("No PCI IOMMU group supplied")
			}

			vfioGroupFile := fmt.Sprintf("/dev/vfio/%s", pciIOMMUGroup)
			err := os.Chown(vfioGroupFile, int(d.state.OS.UnprivUID), -1)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed to chown vfio group device %q", vfioGroupFile)
			}
			revert.Add(func() { os.Chown(vfioGroupFile, 0, -1) })
		}
	}

	if qemuDev["driver"] != "" {
		// Return a monitor hook to add the NIC via QMP before the VM is started.
		monHook := func(m *qmp.Monitor) error {
			if fd, found := qemuNetDev["fd"]; found {
				fileName := fd.(string)

				f, err := os.OpenFile(fileName, os.O_RDWR, 0)
				if err != nil {
					return errors.Wrapf(err, "Error opening exta file %q", fileName)
				}
				defer f.Close() // Close file after device has been added.

				err = m.SendFile(fileName, f)
				if err != nil {
					return errors.Wrapf(err, "Error sending exta file %q", fileName)
				}
			}

			err := m.AddNIC(qemuNetDev, qemuDev)
			if err != nil {
				return errors.Wrapf(err, "Failed setting up device %v", devName)
			}

			return nil
		}

		revert.Success()
		return monHook, nil
	}

	return nil, fmt.Errorf("Unrecognised device type")
}

// addPCIDevConfig adds the qemu config required for adding a raw PCI device.
func (d *qemu) addPCIDevConfig(sb *strings.Builder, bus *qemuBus, pciConfig []deviceConfig.RunConfigItem) error {
	var devName, pciSlotName string
	for _, pciItem := range pciConfig {
		if pciItem.Key == "devName" {
			devName = pciItem.Value
		} else if pciItem.Key == "pciSlotName" {
			pciSlotName = pciItem.Value
		}
	}

	devBus, devAddr, multi := bus.allocate(fmt.Sprintf("lxd_%s", devName))
	tplFields := map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,

		"devName":     devName,
		"pciSlotName": pciSlotName,
	}

	return qemuPCIPhysical.Execute(sb, tplFields)
}

// addGPUDevConfig adds the qemu config required for adding a GPU device.
func (d *qemu) addGPUDevConfig(sb *strings.Builder, bus *qemuBus, gpuConfig []deviceConfig.RunConfigItem) error {
	var devName, pciSlotName, vgpu string
	for _, gpuItem := range gpuConfig {
		if gpuItem.Key == "devName" {
			devName = gpuItem.Value
		} else if gpuItem.Key == "pciSlotName" {
			pciSlotName = gpuItem.Value
		} else if gpuItem.Key == "vgpu" {
			vgpu = gpuItem.Value
		}
	}

	vgaMode := func() bool {
		// No VGA mode on non-x86.
		if d.architecture != osarch.ARCH_64BIT_INTEL_X86 {
			return false
		}

		// Only enable if present on the card.
		if !shared.PathExists(filepath.Join("/sys/bus/pci/devices", pciSlotName, "boot_vga")) {
			return false
		}

		// Skip SRIOV VFs as those are shared with the host card.
		if shared.PathExists(filepath.Join("/sys/bus/pci/devices", pciSlotName, "physfn")) {
			return false
		}

		return true
	}()

	devBus, devAddr, multi := bus.allocate(fmt.Sprintf("lxd_%s", devName))
	tplFields := map[string]interface{}{
		"bus":           bus.name,
		"devBus":        devBus,
		"devAddr":       devAddr,
		"multifunction": multi,

		"devName":     devName,
		"pciSlotName": pciSlotName,
		"vga":         vgaMode,
		"vgpu":        vgpu,
	}

	// Add main GPU device in VGA mode to qemu config.
	err := qemuGPUDevPhysical.Execute(sb, tplFields)
	if err != nil {
		return err
	}

	var iommuGroupPath string

	if vgpu != "" {
		iommuGroupPath = filepath.Join("/sys/bus/mdev/devices", vgpu, "iommu_group", "devices")
	} else {
		// Add any other related IOMMU VFs as generic PCI devices.
		iommuGroupPath = filepath.Join("/sys/bus/pci/devices", pciSlotName, "iommu_group", "devices")
	}

	if shared.PathExists(iommuGroupPath) {
		// Extract parent slot name by removing any virtual function ID.
		parts := strings.SplitN(pciSlotName, ".", 2)
		prefix := parts[0]

		// Iterate the members of the IOMMU group and override any that match the parent slot name prefix.
		err := filepath.Walk(iommuGroupPath, func(path string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			iommuSlotName := filepath.Base(path) // Virtual function's address is dir name.

			// Match any VFs that are related to the GPU device (but not the GPU device itself).
			if strings.HasPrefix(iommuSlotName, prefix) && iommuSlotName != pciSlotName {
				// Add VF device without VGA mode to qemu config.
				devBus, devAddr, multi := bus.allocate(fmt.Sprintf("lxd_%s", devName))
				tplFields := map[string]interface{}{
					"bus":           bus.name,
					"devBus":        devBus,
					"devAddr":       devAddr,
					"multifunction": multi,

					// Generate associated device name by combining main device name and VF ID.
					"devName":     fmt.Sprintf("%s_%s", devName, devAddr),
					"pciSlotName": iommuSlotName,
					"vga":         false,
					"vgpu":        "",
				}

				err := qemuGPUDevPhysical.Execute(sb, tplFields)
				if err != nil {
					return err
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *qemu) addUSBDeviceConfig(sb *strings.Builder, bus *qemuBus, usbConfig []deviceConfig.RunConfigItem) error {
	var devName, hostDevice string

	for _, usbItem := range usbConfig {
		if usbItem.Key == "devName" {
			devName = usbItem.Value
		} else if usbItem.Key == "hostDevice" {
			hostDevice = usbItem.Value
		}
	}

	tplFields := map[string]interface{}{
		"hostDevice": hostDevice,
		"devName":    devName,
	}

	err := qemuUSBDev.Execute(sb, tplFields)
	if err != nil {
		return err
	}

	// Add path to external devPaths. This way, the path will be included in the apparmor profile.
	d.devPaths = append(d.devPaths, hostDevice)

	return nil
}

func (d *qemu) addTPMDeviceConfig(sb *strings.Builder, tpmConfig []deviceConfig.RunConfigItem) error {
	var devName, socketPath string

	for _, tpmItem := range tpmConfig {
		if tpmItem.Key == "path" {
			socketPath = tpmItem.Value
		} else if tpmItem.Key == "devName" {
			devName = tpmItem.Value
		}
	}

	tplFields := map[string]interface{}{
		"devName": devName,
		"path":    socketPath,
	}

	err := qemuTPM.Execute(sb, tplFields)
	if err != nil {
		return err
	}

	return nil
}

// pidFilePath returns the path where the qemu process should write its PID.
func (d *qemu) pidFilePath() string {
	return filepath.Join(d.LogPath(), "qemu.pid")
}

// pid gets the PID of the running qemu process. Returns 0 if PID file or process not found, and -1 if err non-nil.
func (d *qemu) pid() (int, error) {
	pidStr, err := ioutil.ReadFile(d.pidFilePath())
	if os.IsNotExist(err) {
		return 0, nil // PID file has gone.
	}

	if err != nil {
		return -1, err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidStr)))
	if err != nil {
		return -1, err
	}

	cmdLineProcFilePath := fmt.Sprintf("/proc/%d/cmdline", pid)
	cmdLine, err := ioutil.ReadFile(cmdLineProcFilePath)
	if err != nil {
		return 0, nil // Process has gone.
	}

	qemuSearchString := []byte("qemu-system")
	instUUID := []byte(d.localConfig["volatile.uuid"])
	if !bytes.Contains(cmdLine, qemuSearchString) || !bytes.Contains(cmdLine, instUUID) {
		return -1, fmt.Errorf("PID doesn't match the running process")
	}

	return pid, nil
}

// forceStop kills the QEMU prorcess if running, performs normal device & operation cleanup and sends stop
// lifecycle event.
func (d *qemu) forceStop() error {
	pid, _ := d.pid()
	if pid > 0 {
		err := d.killQemuProcess(pid)
		if err != nil {
			return errors.Wrapf(err, "Failed to stop VM process %d", pid)
		}

		// Wait for QEMU process to exit and perform device cleanup.
		err = d.onStop("stop")
		if err != nil {
			return err
		}

		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceStopped.Event(d, nil))
	}

	return nil
}

// Stop the VM.
func (d *qemu) Stop(stateful bool) error {
	d.logger.Debug("Stop started", log.Ctx{"stateful": stateful})
	defer d.logger.Debug("Stop finished", log.Ctx{"stateful": stateful})

	// Must be run prior to creating the operation lock.
	// Allow stop to proceed if statusCode is Error as we may need to forcefully kill the QEMU process.
	statusCode := d.statusCode()
	if !d.isRunningStatusCode(statusCode) && statusCode != api.Error {
		return fmt.Errorf("The instance is already stopped")
	}

	// Check for stateful.
	if stateful && !shared.IsTrue(d.expandedConfig["migration.stateful"]) {
		return fmt.Errorf("Stateful stop requires migration.stateful to be set to true")
	}

	// Setup a new operation.
	exists, op, err := operationlock.CreateWaitGet(d.id, "stop", []string{"restart", "restore"}, false, true)
	if err != nil {
		return err
	}
	if exists {
		// An existing matching operation has now succeeded, return.
		return nil
	}

	// Connect to the monitor.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		// If we fail to connect, it's most likely because the VM is already off, but it could also be
		// because the qemu process is not responding, check if process still exists and kill it if needed.
		return d.forceStop()
	}

	// Get the wait channel.
	chDisconnect, err := monitor.Wait()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			// If we fail to wait, it's most likely because the VM is already off, but it could also be
			// because the qemu process is not responding, check if process still exists and kill it if
			// needed.
			return d.forceStop()
		}

		op.Done(err)
		return err
	}

	// Handle stateful stop.
	if stateful {
		// Dump the state.
		err := d.saveState(monitor)
		if err != nil {
			op.Done(err)
			return err
		}

		// Reset the timer.
		op.Reset()

		// Mark the instance as having state.
		d.stateful = true
		err = d.state.Cluster.UpdateInstanceStatefulFlag(d.id, true)
		if err != nil {
			op.Done(err)
			return err
		}
	}

	// Send the quit command.
	err = monitor.Quit()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			op.Done(nil)
			return nil
		}

		op.Done(err)
		return err
	}

	// Wait for QEMU to exit (can take a while if pending I/O).
	<-chDisconnect

	// Wait for onStop.
	err = op.Wait()
	if err != nil && d.IsRunning() {
		return err
	}

	if op.Action() == "stop" {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceStopped.Event(d, nil))
	}

	return nil
}

// Unfreeze restores the instance to running.
func (d *qemu) Unfreeze() error {
	// Connect to the monitor.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		return err
	}

	// Send the cont command.
	err = monitor.Start()
	if err != nil {
		return err
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceResumed.Event(d, nil))
	return nil
}

// IsPrivileged does not apply to virtual machines. Always returns false.
func (d *qemu) IsPrivileged() bool {
	return false
}

// Snapshot takes a new snapshot.
func (d *qemu) Snapshot(name string, expiry time.Time, stateful bool) error {
	// Deal with state.
	if stateful {
		// Confirm the instance has stateful migration enabled.
		if !shared.IsTrue(d.expandedConfig["migration.stateful"]) {
			return fmt.Errorf("Stateful stop requires migration.stateful to be set to true")
		}

		// Quick checks.
		if !d.IsRunning() {
			return fmt.Errorf("Unable to create a stateful snapshot. The instance isn't running")
		}

		// Connect to the monitor.
		monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
		if err != nil {
			return err
		}

		// Dump the state.
		err = d.saveState(monitor)
		if err != nil {
			return err
		}

		// Resume the VM once the disk state has been saved.
		defer monitor.Start()

		// Remove the state from the main volume.
		defer os.Remove(d.StatePath())
	}

	return d.snapshotCommon(d, name, expiry, stateful)
}

// Restore restores an instance snapshot.
func (d *qemu) Restore(source instance.Instance, stateful bool) error {
	op, err := operationlock.Create(d.id, "restore", false, false)
	if err != nil {
		return errors.Wrap(err, "Create restore operation")
	}
	defer op.Done(nil)

	var ctxMap log.Ctx

	// Stop the instance.
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

		// This will unmount the instance storage.
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
		"source":    source.Name()}

	d.logger.Info("Restoring instance", ctxMap)

	// Load the storage driver.
	pool, err := storagePools.GetPoolByInstance(d.state, d)
	if err != nil {
		op.Done(err)
		return err
	}

	// Restore the rootfs.
	err = pool.RestoreInstanceSnapshot(d, source, nil)
	if err != nil {
		op.Done(err)
		return err
	}

	// Restore the configuration.
	args := db.InstanceArgs{
		Architecture: source.Architecture(),
		Config:       source.LocalConfig(),
		Description:  source.Description(),
		Devices:      source.LocalDevices(),
		Ephemeral:    source.IsEphemeral(),
		Profiles:     source.Profiles(),
		Project:      source.Project(),
		Type:         source.Type(),
		Snapshot:     source.IsSnapshot(),
	}

	// Don't pass as user-requested as there's no way to fix a bad config.
	// This will call d.UpdateBackupFile() to ensure snapshot list is up to date.
	err = d.Update(args, false)
	if err != nil {
		op.Done(err)
		return err
	}
	d.stateful = stateful

	// Restart the instance.
	if wasRunning || stateful {
		d.logger.Debug("Starting instance after snapshot restore")
		err := d.Start(stateful)
		if err != nil {
			op.Done(err)
			return err
		}
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRestored.Event(d, map[string]interface{}{"snapshot": source.Name()}))
	d.logger.Info("Restored instance", ctxMap)
	return nil
}

// Rename the instance. Accepts an argument to enable applying deferred TemplateTriggerRename.
func (d *qemu) Rename(newName string, applyTemplateTrigger bool) error {
	oldName := d.Name()
	ctxMap := log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate,
		"newname":   newName}

	d.logger.Info("Renaming instance", ctxMap)

	// Quick checks.
	err := instance.ValidName(newName, d.IsSnapshot())
	if err != nil {
		return err
	}

	if d.IsRunning() {
		return fmt.Errorf("Renaming of running instance not allowed")
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
			d.logger.Error("Failed to get instance snapshots", ctxMap)
			return errors.Wrapf(err, "Failed to get instance snapshots")
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
				return err
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
		d.logger.Error("Failed renaming instance", ctxMap)
		return err
	}

	// Rename the logging path.
	newFullName := project.Instance(d.Project(), d.Name())
	os.RemoveAll(shared.LogPath(newFullName))
	if shared.PathExists(d.LogPath()) {
		err := os.Rename(d.LogPath(), shared.LogPath(newFullName))
		if err != nil {
			d.logger.Error("Failed renaming instance", ctxMap)
			return err
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

	// Update lease files.
	network.UpdateDNSMasqStatic(d.state, "")

	err = d.UpdateBackupFile()
	if err != nil {
		return err
	}

	d.logger.Info("Renamed instance", ctxMap)

	if d.snapshot {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotRenamed.Event(d, map[string]interface{}{"old_name": oldName}))
	} else {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceRenamed.Event(d, map[string]interface{}{"old_name": oldName}))
	}

	revert.Success()
	return nil
}

// Update the instance config.
func (d *qemu) Update(args db.InstanceArgs, userRequested bool) error {
	revert := revert.New()
	defer revert.Fail()

	// Set sane defaults for unset keys.
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
		// Validate the new config.
		err := instance.ValidConfig(d.state.OS, args.Config, false, instancetype.VM)
		if err != nil {
			return errors.Wrap(err, "Invalid config")
		}

		// Validate the new devices without using expanded devices validation (expensive checks disabled).
		err = instance.ValidDevices(d.state, d.state.Cluster, d.Project(), d.Type(), args.Devices, false)
		if err != nil {
			return errors.Wrap(err, "Invalid devices")
		}
	}

	// Validate the new profiles.
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

	// Validate the new architecture.
	if args.Architecture != 0 {
		_, err = osarch.ArchitectureName(args.Architecture)
		if err != nil {
			return fmt.Errorf("Invalid architecture ID: %s", err)
		}
	}

	// Get a copy of the old configuration.
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

	// Revert local changes if update fails.
	revert.Add(func() {
		d.description = oldDescription
		d.architecture = oldArchitecture
		d.ephemeral = oldEphemeral
		d.expandedConfig = oldExpandedConfig
		d.expandedDevices = oldExpandedDevices
		d.localConfig = oldLocalConfig
		d.localDevices = oldLocalDevices
		d.profiles = oldProfiles
		d.expiryDate = oldExpiryDate
	})

	// Apply the various changes to local vars.
	d.description = args.Description
	d.architecture = args.Architecture
	d.ephemeral = args.Ephemeral
	d.localConfig = args.Config
	d.localDevices = args.Devices
	d.profiles = args.Profiles
	d.expiryDate = args.ExpiryDate

	// Expand the config.
	err = d.expandConfig(nil)
	if err != nil {
		return errors.Wrap(err, "Expand config")
	}

	err = d.expandDevices(nil)
	if err != nil {
		return errors.Wrap(err, "Expand devices")
	}

	// Diff the configurations.
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

	// Diff the devices.
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
		// Do some validation of the config diff.
		err = instance.ValidConfig(d.state.OS, d.expandedConfig, false, instancetype.VM)
		if err != nil {
			return errors.Wrap(err, "Invalid expanded config")
		}

		// Do full expanded validation of the devices diff.
		err = instance.ValidDevices(d.state, d.state.Cluster, d.Project(), d.Type(), d.expandedDevices, true)
		if err != nil {
			return errors.Wrap(err, "Invalid expanded devices")
		}
	}

	// If apparmor changed, re-validate the apparmor profile (even if not running).
	if shared.StringInSlice("raw.apparmor", changedConfig) {
		err = apparmor.InstanceValidate(d.state, d)
		if err != nil {
			return errors.Wrap(err, "Parse AppArmor profile")
		}
	}

	isRunning := d.IsRunning()

	// Use the device interface to apply update changes.
	err = d.updateDevices(removeDevices, addDevices, updateDevices, oldExpandedDevices, isRunning, userRequested)
	if err != nil {
		return err
	}

	if isRunning {
		// Only certain keys can be changed on a running VM.
		liveUpdateKeys := []string{"limits.memory"}

		// Check only keys that support live update have changed.
		for _, key := range changedConfig {
			if !strings.HasPrefix(key, "user.") && !shared.StringInSlice(key, liveUpdateKeys) {
				return fmt.Errorf("Key %q cannot be updated when VM is running", key)
			}
		}

		// Apply live update for each key.
		for _, key := range changedConfig {
			value := d.expandedConfig[key]

			if key == "limits.memory" {
				err = d.updateMemoryLimit(value)
				if err != nil {
					if err != nil {
						return errors.Wrapf(err, "Failed updating memory limit")
					}
				}
			}
		}
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

	if shared.StringInSlice("security.secureboot", changedConfig) {
		// Re-generate the NVRAM.
		err = d.setupNvram()
		if err != nil {
			return err
		}
	}

	// Finally, apply the changes to the database.
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

	// Changes have been applied and recorded, do not revert if an error occurs from here.
	revert.Success()

	if isRunning {
		err = d.writeInstanceData()
		if err != nil {
			return errors.Wrap(err, "Failed to write instance-data file")
		}

		// Send devlxd notifications only for user.* key changes
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
	}

	if userRequested {
		if d.snapshot {
			d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotUpdated.Event(d, nil))
		} else {
			d.state.Events.SendLifecycle(d.project, lifecycle.InstanceUpdated.Event(d, nil))
		}
	}

	return nil
}

// updateMemoryLimit live updates the VM's memory limit by reszing the balloon device.
func (d *qemu) updateMemoryLimit(newLimit string) error {
	if newLimit == "" {
		return nil
	}

	if shared.IsTrue(d.expandedConfig["limits.memory.hugepages"]) {
		return fmt.Errorf("Cannot live update memory limit when using huge pages")
	}

	// Check new size string is valid and convert to bytes.
	newSizeBytes, err := units.ParseByteSizeString(newLimit)
	if err != nil {
		return errors.Wrapf(err, "Invalid memory size")
	}
	newSizeMB := newSizeBytes / 1024 / 1024

	// Connect to the monitor.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		return err // The VM isn't running as no monitor socket available.
	}

	baseSizeBytes, err := monitor.GetMemorySizeBytes()
	if err != nil {
		return err
	}
	baseSizeMB := baseSizeBytes / 1024 / 1024

	curSizeBytes, err := monitor.GetMemoryBalloonSizeBytes()
	if err != nil {
		return err
	}
	curSizeMB := curSizeBytes / 1024 / 1024

	if curSizeMB == newSizeMB {
		return nil
	} else if baseSizeMB < newSizeMB {
		return fmt.Errorf("Cannot increase memory size beyond boot time size when VM is running (Boot time size %dMiB, new size %dMiB)", baseSizeMB, newSizeMB)
	}

	// Set effective memory size.
	err = monitor.SetMemoryBalloonSizeBytes(newSizeBytes)
	if err != nil {
		return err
	}

	// Changing the memory balloon can take time, so poll the effectice size to check it has shrunk within 1%
	// of the target size, which we then take as success (it may still continue to shrink closer to target).
	for i := 0; i < 10; i++ {
		curSizeBytes, err = monitor.GetMemoryBalloonSizeBytes()
		if err != nil {
			return err
		}
		curSizeMB = curSizeBytes / 1024 / 1024

		var diff int64
		if curSizeMB < newSizeMB {
			diff = newSizeMB - curSizeMB
		} else {
			diff = curSizeMB - newSizeMB
		}

		if diff <= (newSizeMB / 100) {
			return nil // We reached to within 1% of our target size.
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("Failed setting memory to %dMiB (currently %dMiB) as it was taking too long", newSizeMB, curSizeMB)
}

func (d *qemu) updateDevices(removeDevices deviceConfig.Devices, addDevices deviceConfig.Devices, updateDevices deviceConfig.Devices, oldExpandedDevices deviceConfig.Devices, instanceRunning bool, userRequested bool) error {
	revert := revert.New()
	defer revert.Fail()

	// Remove devices in reverse order to how they were added.
	for _, dev := range removeDevices.Reversed() {
		if instanceRunning {
			err := d.deviceStop(dev.Name, dev.Config, instanceRunning)
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

			revert.Add(func() { d.deviceStop(dev.Name, dev.Config, instanceRunning) })
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

// deviceUpdate loads a new device and calls its Update() function.
func (d *qemu) deviceUpdate(deviceName string, rawConfig deviceConfig.Device, oldDevices deviceConfig.Devices, instanceRunning bool) error {
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

func (d *qemu) removeUnixDevices() error {
	// Check that we indeed have devices to remove.
	if !shared.PathExists(d.DevicesPath()) {
		return nil
	}

	// Load the directory listing.
	dents, err := ioutil.ReadDir(d.DevicesPath())
	if err != nil {
		return err
	}

	for _, f := range dents {
		// Skip non-Unix devices.
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

func (d *qemu) removeDiskDevices() error {
	// Check that we indeed have devices to remove.
	if !shared.PathExists(d.DevicesPath()) {
		return nil
	}

	// Load the directory listing.
	dents, err := ioutil.ReadDir(d.DevicesPath())
	if err != nil {
		return err
	}

	for _, f := range dents {
		// Skip non-disk devices
		if !strings.HasPrefix(f.Name(), "disk.") {
			continue
		}

		// Always try to unmount the host side.
		_ = unix.Unmount(filepath.Join(d.DevicesPath(), f.Name()), unix.MNT_DETACH)

		// Remove the entry.
		diskPath := filepath.Join(d.DevicesPath(), f.Name())
		err := os.Remove(diskPath)
		if err != nil {
			d.logger.Error("Failed to remove disk device path", log.Ctx{"err": err, "path": diskPath})
		}
	}

	return nil
}

func (d *qemu) cleanup() {
	// Unmount any leftovers
	d.removeUnixDevices()
	d.removeDiskDevices()

	// Remove the security profiles
	apparmor.InstanceDelete(d.state, d)

	// Remove the devices path
	os.Remove(d.DevicesPath())

	// Remove the shmounts path
	os.RemoveAll(d.ShmountsPath())
}

// cleanupDevices performs any needed device cleanup steps when instance is stopped.
// Must be called before root volume is unmounted.
func (d *qemu) cleanupDevices() {
	// Clear up the config drive virtiofsd process.
	err := device.DiskVMVirtiofsdStop(d.configVirtiofsdPaths())
	if err != nil {
		d.logger.Warn("Failed cleaning up config drive virtiofsd", log.Ctx{"err": err})
	}

	// Clear up the config drive mount.
	err = d.configDriveMountPathClear()
	if err != nil {
		d.logger.Warn("Failed cleaning up config drive mount", log.Ctx{"err": err})
	}

	for _, dev := range d.expandedDevices.Reversed() {
		// Use the device interface if device supports it.
		err := d.deviceStop(dev.Name, dev.Config, false)
		if err == device.ErrUnsupportedDevType {
			continue
		} else if err != nil {
			d.logger.Error("Failed to stop device", log.Ctx{"devName": dev.Name, "err": err})
		}
	}
}

func (d *qemu) init() error {
	// Compute the expanded config and device list.
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

// Delete the instance.
func (d *qemu) Delete(force bool) error {
	ctxMap := log.Ctx{
		"created":   d.creationDate,
		"ephemeral": d.ephemeral,
		"used":      d.lastUsedDate}

	d.logger.Info("Deleting instance", ctxMap)

	// Check if instance is delete protected.
	if !force && shared.IsTrue(d.expandedConfig["security.protection.delete"]) && !d.IsSnapshot() {
		return fmt.Errorf("Instance is protected")
	}

	// Delete any persistent warnings for instance.
	err := d.warningsDelete()
	if err != nil {
		return err
	}

	// Check if we're dealing with "lxd import".
	// TODO consider lxd import detection for VMs.
	isImport := false

	// Attempt to initialize storage interface for the instance.
	pool, err := d.getStoragePool()
	if err != nil && errors.Cause(err) != db.ErrNoSuchObject {
		return err
	} else if pool != nil {
		if d.IsSnapshot() {
			if !isImport {
				// Remove snapshot volume and database record.
				err = pool.DeleteInstanceSnapshot(d, nil)
				if err != nil {
					return err
				}
			}
		} else {
			// Remove all snapshots by initialising each snapshot as an Instance and
			// calling its Delete function.
			err := instance.DeleteSnapshots(d.state, d.Project(), d.Name())
			if err != nil {
				return err
			}

			if !isImport {
				// Remove the storage volume, snapshot volumes and database records.
				err = pool.DeleteInstance(d, nil)
				if err != nil {
					return err
				}
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
			d.logger.Error("Failed deleting instance MAAS record", log.Ctx{"err": err})
			return err
		}

		// Run device removal function for each device.
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
	if err := d.state.Cluster.DeleteInstance(d.Project(), d.Name()); err != nil {
		d.logger.Error("Failed deleting instance entry", log.Ctx{"project": d.Project()})
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

	d.logger.Info("Deleted instance", ctxMap)

	if d.snapshot {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceSnapshotDeleted.Event(d, nil))
	} else {
		d.state.Events.SendLifecycle(d.project, lifecycle.InstanceDeleted.Event(d, nil))
	}

	return nil
}

func (d *qemu) deviceAdd(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool) error {
	dev, _, err := d.deviceLoad(deviceName, rawConfig)
	if err != nil {
		return err
	}

	if instanceRunning && !dev.CanHotPlug() {
		return fmt.Errorf("Device cannot be added when instance is running")
	}

	return dev.Add()
}

func (d *qemu) deviceRemove(deviceName string, rawConfig deviceConfig.Device, instanceRunning bool) error {
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

// Export publishes the instance.
func (d *qemu) Export(w io.Writer, properties map[string]string, expiration time.Time) (api.ImageMetadata, error) {
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
	mountInfo, err := d.mount()
	if err != nil {
		d.logger.Error("Failed exporting instance", ctxMap)
		return meta, err
	}
	defer d.unmount()

	// Create the tarball.
	tarWriter := instancewriter.NewInstanceTarWriter(w, nil)

	// Path inside the tar image is the pathname starting after cDir.
	cDir := d.Path()
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

		tmpOffset := len(filepath.Dir(fnam)) + 1
		if err := tarWriter.WriteFile(fnam[tmpOffset:], fnam, fi, false); err != nil {
			tarWriter.Close()
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
			tmpOffset := len(filepath.Dir(fnam)) + 1
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

	// Convert from raw to qcow2 and add to tarball.
	tmpPath, err := ioutil.TempDir(shared.VarPath("images"), "lxd_export_")
	if err != nil {
		return meta, err
	}
	defer os.RemoveAll(tmpPath)

	if mountInfo.DiskPath == "" {
		return meta, fmt.Errorf("No disk path available from mount")
	}

	fPath := fmt.Sprintf("%s/rootfs.img", tmpPath)
	_, err = shared.RunCommand("qemu-img", "convert", "-c", "-O", "qcow2", mountInfo.DiskPath, fPath)
	if err != nil {
		return meta, fmt.Errorf("Failed converting image to qcow2: %v", err)
	}

	fi, err := os.Lstat(fPath)
	if err != nil {
		return meta, err
	}

	imgOffset := len(tmpPath) + 1
	err = tarWriter.WriteFile(fPath[imgOffset:], fPath, fi, false)
	if err != nil {
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

// Migrate migrates the instance to another node.
func (d *qemu) Migrate(args *instance.CriuMigrationArgs) error {
	return instance.ErrNotImplemented
}

// CGroupSet is not implemented for VMs.
func (d *qemu) CGroup() (*cgroup.CGroup, error) {
	return nil, instance.ErrNotImplemented
}

// FileExists is not implemented for VMs.
func (d *qemu) FileExists(path string) error {
	return instance.ErrNotImplemented
}

// FilePull retrieves a file from the instance.
func (d *qemu) FilePull(srcPath string, dstPath string) (int64, int64, os.FileMode, string, []string, error) {
	client, err := d.getAgentClient()
	if err != nil {
		return 0, 0, 0, "", nil, err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		d.logger.Error("Failed to connect to lxd-agent", log.Ctx{"devName": d.Name(), "err": err})
		return 0, 0, 0, "", nil, fmt.Errorf("Failed to connect to lxd-agent")
	}
	defer agent.Disconnect()

	content, resp, err := agent.GetInstanceFile("", srcPath)
	if err != nil {
		return 0, 0, 0, "", nil, err
	}

	switch resp.Type {
	case "file", "symlink":
		data, err := ioutil.ReadAll(content)
		if err != nil {
			return 0, 0, 0, "", nil, err
		}

		err = ioutil.WriteFile(dstPath, data, os.FileMode(resp.Mode))
		if err != nil {
			return 0, 0, 0, "", nil, err
		}

		err = os.Lchown(dstPath, int(resp.UID), int(resp.GID))
		if err != nil {
			return 0, 0, 0, "", nil, err
		}

		return resp.UID, resp.GID, os.FileMode(resp.Mode), resp.Type, nil, nil
	case "directory":
		return resp.UID, resp.GID, os.FileMode(resp.Mode), resp.Type, resp.Entries, nil
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceFileRetrieved.Event(d, log.Ctx{"file-source": srcPath, "file-destination": dstPath}))

	return 0, 0, 0, "", nil, fmt.Errorf("bad file type %s", resp.Type)
}

// FilePush pushes a file into the instance.
func (d *qemu) FilePush(fileType string, srcPath string, dstPath string, uid int64, gid int64, mode int, write string) error {
	client, err := d.getAgentClient()
	if err != nil {
		return err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		d.logger.Error("Failed to connect to lxd-agent", log.Ctx{"err": err})
		return fmt.Errorf("Failed to connect to lxd-agent")
	}
	defer agent.Disconnect()

	args := lxdClient.InstanceFileArgs{
		GID:       gid,
		Mode:      mode,
		Type:      fileType,
		UID:       uid,
		WriteMode: write,
	}

	if fileType == "file" {
		f, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer f.Close()

		args.Content = f
	} else if fileType == "symlink" {
		symlinkTarget, err := os.Readlink(dstPath)
		if err != nil {
			return err
		}

		args.Content = bytes.NewReader([]byte(symlinkTarget))
	}

	err = agent.CreateInstanceFile("", dstPath, args)
	if err != nil {
		return err
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceFilePushed.Event(d, log.Ctx{"file-source": srcPath, "file-destination": dstPath, "info": args}))

	return nil
}

// FileRemove removes a file from the instance.
func (d *qemu) FileRemove(path string) error {
	// Connect to the agent.
	client, err := d.getAgentClient()
	if err != nil {
		return err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		return fmt.Errorf("Failed to connect to lxd-agent")
	}
	defer agent.Disconnect()

	// Delete instance file.
	err = agent.DeleteInstanceFile("", path)
	if err != nil {
		return err
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceFileDeleted.Event(d, log.Ctx{"file": path}))
	return nil
}

// Console gets access to the instance's console.
func (d *qemu) Console(protocol string) (*os.File, chan error, error) {
	switch protocol {
	case instance.ConsoleTypeConsole:
		return d.console()
	case instance.ConsoleTypeVGA:
		return d.vga()
	default:
		return nil, nil, fmt.Errorf("Unknown protocol %q", protocol)
	}
}

func (d *qemu) console() (*os.File, chan error, error) {
	chDisconnect := make(chan error, 1)

	// Avoid duplicate connects.
	vmConsoleLock.Lock()
	if vmConsole[d.id] {
		vmConsoleLock.Unlock()
		return nil, nil, fmt.Errorf("There is already an active console for this instance")
	}
	vmConsoleLock.Unlock()

	// Connect to the monitor.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		return nil, nil, err // The VM isn't running as no monitor socket available.
	}

	// Get the console.
	console, err := monitor.Console("console")
	if err != nil {
		return nil, nil, err
	}

	// Record the console is in use.
	vmConsoleLock.Lock()
	vmConsole[d.id] = true
	vmConsoleLock.Unlock()

	// Handle console disconnection.
	go func() {
		<-chDisconnect

		vmConsoleLock.Lock()
		delete(vmConsole, d.id)
		vmConsoleLock.Unlock()
	}()

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceConsole.Event(d, log.Ctx{"type": instance.ConsoleTypeConsole}))

	return console, chDisconnect, nil
}

func (d *qemu) vga() (*os.File, chan error, error) {
	// Open the spice socket
	conn, err := net.Dial("unix", d.spicePath())
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Connect to SPICE socket %q", d.spicePath())
	}

	file, err := (conn.(*net.UnixConn)).File()
	if err != nil {
		return nil, nil, errors.Wrap(err, "Get socket file")
	}
	conn.Close()

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceConsole.Event(d, log.Ctx{"type": instance.ConsoleTypeVGA}))

	return file, nil, nil
}

// Exec a command inside the instance.
func (d *qemu) Exec(req api.InstanceExecPost, stdin *os.File, stdout *os.File, stderr *os.File) (instance.Cmd, error) {
	revert := revert.New()
	defer revert.Fail()

	client, err := d.getAgentClient()
	if err != nil {
		return nil, err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		d.logger.Error("Failed to connect to lxd-agent", log.Ctx{"err": err})
		return nil, fmt.Errorf("Failed to connect to lxd-agent")
	}
	revert.Add(agent.Disconnect)

	req.WaitForWS = true
	if req.Interactive {
		// Set console to raw.
		oldttystate, err := termios.MakeRaw(int(stdin.Fd()))
		if err != nil {
			return nil, err
		}

		revert.Add(func() { termios.Restore(int(stdin.Fd()), oldttystate) })
	}

	dataDone := make(chan bool)
	controlSendCh := make(chan api.InstanceExecControl)
	controlResCh := make(chan error)

	// This is the signal control handler, it receives signals from lxc CLI and forwards them to the VM agent.
	controlHandler := func(control *websocket.Conn) {
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		defer control.WriteMessage(websocket.CloseMessage, closeMsg)

		for {
			select {
			case cmd := <-controlSendCh:
				controlResCh <- control.WriteJSON(cmd)
			case <-dataDone:
				return
			}
		}
	}

	args := lxdClient.InstanceExecArgs{
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
		DataDone: dataDone,
		Control:  controlHandler,
	}

	op, err := agent.ExecInstance("", req, &args)
	if err != nil {
		return nil, err
	}

	instCmd := &qemuCmd{
		cmd:              op,
		attachedChildPid: 0, // Process is not running on LXD host.
		dataDone:         args.DataDone,
		cleanupFunc:      revert.Clone().Fail, // Pass revert function clone as clean up function.
		controlSendCh:    controlSendCh,
		controlResCh:     controlResCh,
	}

	d.state.Events.SendLifecycle(d.project, lifecycle.InstanceExec.Event(d, log.Ctx{"command": req.Command}))

	revert.Success()
	return instCmd, nil
}

// Render returns info about the instance.
func (d *qemu) Render(options ...func(response interface{}) error) (interface{}, interface{}, error) {
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
		snapState.Architecture = d.architectureName
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
	instState.Architecture = d.architectureName
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

// RenderFull returns all info about the instance.
func (d *qemu) RenderFull() (*api.InstanceFull, interface{}, error) {
	if d.IsSnapshot() {
		return nil, nil, fmt.Errorf("RenderFull doesn't work with snapshots")
	}

	// Get the Instance struct.
	base, etag, err := d.Render()
	if err != nil {
		return nil, nil, err
	}

	// Convert to InstanceFull.
	vmState := api.InstanceFull{Instance: *base.(*api.Instance)}

	// Add the InstanceState.
	vmState.State, err = d.renderState(vmState.StatusCode)
	if err != nil {
		return nil, nil, err
	}

	// Add the InstanceSnapshots.
	snaps, err := d.Snapshots()
	if err != nil {
		return nil, nil, err
	}

	for _, snap := range snaps {
		render, _, err := snap.Render()
		if err != nil {
			return nil, nil, err
		}

		if vmState.Snapshots == nil {
			vmState.Snapshots = []api.InstanceSnapshot{}
		}

		vmState.Snapshots = append(vmState.Snapshots, *render.(*api.InstanceSnapshot))
	}

	// Add the InstanceBackups.
	backups, err := d.Backups()
	if err != nil {
		return nil, nil, err
	}

	for _, backup := range backups {
		render := backup.Render()

		if vmState.Backups == nil {
			vmState.Backups = []api.InstanceBackup{}
		}

		vmState.Backups = append(vmState.Backups, *render)
	}

	return &vmState, etag, nil
}

// renderState returns just state info about the instance.
func (d *qemu) renderState(statusCode api.StatusCode) (*api.InstanceState, error) {
	var err error

	status := &api.InstanceState{}
	pid, _ := d.pid()

	if d.isRunningStatusCode(statusCode) {
		// Try and get state info from agent.
		status, err = d.agentGetState()
		if err != nil {
			if err != errQemuAgentOffline {
				d.logger.Warn("Could not get VM state from agent", log.Ctx{"err": err})
			}

			// Fallback data if agent is not reachable.
			status = &api.InstanceState{}
			status.Processes = -1
			networks := map[string]api.InstanceStateNetwork{}
			for k, m := range d.ExpandedDevices() {
				if m["type"] != "nic" {
					continue
				}

				dev, _, err := d.deviceLoad(k, m)
				if err != nil {
					d.logger.Warn("Could not load device", log.Ctx{"device": k, "err": err})
					continue
				}

				// Only some NIC types support fallback state mechanisms when there is no agent.
				nic, ok := dev.(device.NICState)
				if !ok {
					continue
				}

				network, err := nic.State()
				if err != nil {
					return nil, errors.Wrapf(err, "Failed getting NIC state for %q", k)
				}

				if network != nil {
					networks[k] = *network
				}
			}

			status.Network = networks
		}

		// Populate host_name for network devices.
		for k, m := range d.ExpandedDevices() {
			// We only care about nics.
			if m["type"] != "nic" {
				continue
			}

			// Get hwaddr from static or volatile config.
			hwaddr := m["hwaddr"]
			if hwaddr == "" {
				hwaddr = d.localConfig[fmt.Sprintf("volatile.%s.hwaddr", k)]
			}

			// We have to match on hwaddr as device name can be different from the configured device
			// name when reported from the lxd-agent inside the VM (due to the guest OS choosing name).
			for netName, netStatus := range status.Network {
				if netStatus.Hwaddr == hwaddr {
					if netStatus.HostName == "" {
						netStatus.HostName = d.localConfig[fmt.Sprintf("volatile.%s.host_name", k)]
						status.Network[netName] = netStatus
					}
				}
			}
		}
	}

	status.Pid = int64(pid)
	status.Status = statusCode.String()
	status.StatusCode = statusCode
	status.Disk, err = d.diskState()
	if err != nil && errors.Cause(err) != storageDrivers.ErrNotSupported {
		d.logger.Warn("Error getting disk usage", log.Ctx{"err": err})
	}

	return status, nil
}

// RenderState returns just state info about the instance.
func (d *qemu) RenderState() (*api.InstanceState, error) {
	return d.renderState(d.statusCode())
}

// diskState gets disk usage info.
func (d *qemu) diskState() (map[string]api.InstanceStateDisk, error) {
	pool, err := d.getStoragePool()
	if err != nil {
		return nil, err
	}

	// Get the root disk device config.
	rootDiskName, _, err := shared.GetRootDiskDevice(d.ExpandedDevices().CloneNative())
	if err != nil {
		return nil, err
	}

	usage, err := pool.GetInstanceUsage(d)
	if err != nil {
		return nil, err
	}

	disk := map[string]api.InstanceStateDisk{}
	disk[rootDiskName] = api.InstanceStateDisk{Usage: usage}
	return disk, nil
}

// agentGetState connects to the agent inside of the VM and does
// an API call to get the current state.
func (d *qemu) agentGetState() (*api.InstanceState, error) {
	// Check if the agent is running.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		return nil, err
	}

	if !monitor.AgentReady() {
		return nil, errQemuAgentOffline
	}

	client, err := d.getAgentClient()
	if err != nil {
		return nil, err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		return nil, err
	}
	defer agent.Disconnect()

	status, _, err := agent.GetInstanceState("")
	if err != nil {
		return nil, err
	}

	return status, nil
}

// IsRunning returns whether or not the instance is running.
func (d *qemu) IsRunning() bool {
	return d.isRunningStatusCode(d.statusCode())
}

// IsFrozen returns whether the instance frozen or not.
func (d *qemu) IsFrozen() bool {
	return d.statusCode() == api.Frozen
}

// DeviceEventHandler handles events occurring on the instance's devices.
func (d *qemu) DeviceEventHandler(runConf *deviceConfig.RunConfig) error {
	return fmt.Errorf("DeviceEventHandler Not implemented")
}

// vsockID returns the vsock context ID, 3 being the first ID that can be used.
func (d *qemu) vsockID() int {
	return d.id + 3
}

// InitPID returns the instance's current process ID.
func (d *qemu) InitPID() int {
	pid, _ := d.pid()
	return pid
}

func (d *qemu) statusCode() api.StatusCode {
	// Shortcut to avoid spamming QMP during ongoing operations.
	op := operationlock.Get(d.id)
	if op != nil {
		if op.Action() == "start" {
			return api.Stopped
		}

		if op.Action() == "stop" {
			return api.Running
		}
	}

	// Connect to the monitor.
	monitor, err := qmp.Connect(d.monitorPath(), qemuSerialChardevName, d.getMonitorEventHandler())
	if err != nil {
		// If cannot connect to monitor, but qemu process in pid file still exists, then likely qemu
		// is unresponsive and this instance is in an error state.
		pid, _ := d.pid()
		if pid > 0 {
			return api.Error
		}

		// If we fail to connect, chances are the VM isn't running.
		return api.Stopped
	}

	status, err := monitor.Status()
	if err != nil {
		if err == qmp.ErrMonitorDisconnect {
			// If cannot connect to monitor, but qemu process in pid file still exists, then likely
			// qemu is unresponsive and this instance is in an error state.
			pid, _ := d.pid()
			if pid > 0 {
				return api.Error
			}

			return api.Stopped
		}

		return api.Error
	}

	if status == "running" {
		return api.Running
	} else if status == "paused" {
		return api.Frozen
	} else if status == "internal-error" {
		return api.Error
	}

	return api.Stopped
}

// State returns the instance's state code.
func (d *qemu) State() string {
	return strings.ToUpper(d.statusCode().String())
}

// EarlyLogFilePath returns the instance's early log path.
func (d *qemu) EarlyLogFilePath() string {
	return filepath.Join(d.LogPath(), "qemu.early.log")
}

// LogFilePath returns the instance's log path.
func (d *qemu) LogFilePath() string {
	return filepath.Join(d.LogPath(), "qemu.log")
}

// StoragePool returns the name of the instance's storage pool.
func (d *qemu) StoragePool() (string, error) {
	poolName, err := d.state.Cluster.GetInstancePool(d.Project(), d.Name())
	if err != nil {
		return "", err
	}

	return poolName, nil
}

// FillNetworkDevice takes a nic or infiniband device type and enriches it with automatically
// generated name and hwaddr properties if these are missing from the device.
func (d *qemu) FillNetworkDevice(name string, m deviceConfig.Device) (deviceConfig.Device, error) {
	var err error

	newDevice := m.Clone()

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

	return newDevice, nil
}

// UpdateBackupFile writes the instance's backup.yaml file to storage.
func (d *qemu) UpdateBackupFile() error {
	pool, err := d.getStoragePool()
	if err != nil {
		return err
	}

	return pool.UpdateInstanceBackupFile(d, nil)
}

// cpuTopology takes a user cpu range and returns the number of sockets, cores and threads to configure
// as well as a map of vcpu to threadid for pinning and a map of numa nodes to vcpus for NUMA layout.
func (d *qemu) cpuTopology(limit string) (int, int, int, map[uint64]uint64, map[uint64][]uint64, error) {
	// Get CPU topology.
	cpus, err := resources.GetCPU()
	if err != nil {
		return -1, -1, -1, nil, nil, err
	}

	// Expand the pins.
	pins, err := resources.ParseCpuset(limit)
	if err != nil {
		return -1, -1, -1, nil, nil, err
	}

	// Match tracking.
	vcpus := map[uint64]uint64{}
	sockets := map[uint64][]uint64{}
	cores := map[uint64][]uint64{}
	numaNodes := map[uint64][]uint64{}

	// Go through the physical CPUs looking for matches.
	i := uint64(0)
	for _, cpu := range cpus.Sockets {
		for _, core := range cpu.Cores {
			for _, thread := range core.Threads {
				for _, pin := range pins {
					if thread.ID == int64(pin) {
						// Found a matching CPU.
						vcpus[i] = uint64(pin)

						// Track cores per socket.
						_, ok := sockets[cpu.Socket]
						if !ok {
							sockets[cpu.Socket] = []uint64{}
						}
						if !shared.Uint64InSlice(core.Core, sockets[cpu.Socket]) {
							sockets[cpu.Socket] = append(sockets[cpu.Socket], core.Core)
						}

						// Track threads per core.
						_, ok = cores[core.Core]
						if !ok {
							cores[core.Core] = []uint64{}
						}
						if !shared.Uint64InSlice(thread.Thread, cores[core.Core]) {
							cores[core.Core] = append(cores[core.Core], thread.Thread)
						}

						// Record NUMA node for thread.
						_, ok = cores[core.Core]
						if !ok {
							numaNodes[thread.NUMANode] = []uint64{}
						}
						numaNodes[thread.NUMANode] = append(numaNodes[thread.NUMANode], i)

						i++
					}
				}
			}
		}
	}

	// Confirm we're getting the expected number of CPUs.
	if len(pins) != len(vcpus) {
		return -1, -1, -1, nil, nil, fmt.Errorf("Unavailable CPUs requested: %s", limit)
	}

	// Validate the topology.
	valid := true
	nrSockets := 0
	nrCores := 0
	nrThreads := 0

	// Confirm that there is no balancing inconsistencies.
	countCores := -1
	for _, cores := range sockets {
		if countCores != -1 && len(cores) != countCores {
			valid = false
			break
		}

		countCores = len(cores)
	}

	countThreads := -1
	for _, threads := range cores {
		if countThreads != -1 && len(threads) != countThreads {
			valid = false
			break
		}

		countThreads = len(threads)
	}

	// Check against double listing of CPU.
	if len(sockets)*countCores*countThreads != len(vcpus) {
		valid = false
	}

	// Build up the topology.
	if valid {
		// Valid topology.
		nrSockets = len(sockets)
		nrCores = countCores
		nrThreads = countThreads
	} else {
		d.logger.Warn("Instance uses a CPU pinning profile which doesn't match hardware layout")

		// Fallback on pretending everything are cores.
		nrSockets = 1
		nrCores = len(vcpus)
		nrThreads = 1
	}

	return nrSockets, nrCores, nrThreads, vcpus, numaNodes, nil
}

func (d *qemu) devlxdEventSend(eventType string, eventMessage interface{}) error {
	event := shared.Jmap{}
	event["type"] = eventType
	event["timestamp"] = time.Now()
	event["metadata"] = eventMessage

	client, err := d.getAgentClient()
	if err != nil {
		return err
	}

	agent, err := lxdClient.ConnectLXDHTTP(nil, client)
	if err != nil {
		d.logger.Error("Failed to connect to lxd-agent", log.Ctx{"err": err})
		return fmt.Errorf("Failed to connect to lxd-agent")
	}
	defer agent.Disconnect()

	_, _, err = agent.RawQuery("POST", "/1.0/events", &event, "")
	if err != nil {
		return err
	}

	return nil
}

func (d *qemu) writeInstanceData() error {
	// Only write instance-data file if security.devlxd is true.
	if !(d.expandedConfig["security.devlxd"] == "" || shared.IsTrue(d.expandedConfig["security.devlxd"])) {
		return nil
	}

	// Instance data for devlxd.
	configDrivePath := filepath.Join(d.Path(), "config")
	userConfig := make(map[string]string)

	for k, v := range d.ExpandedConfig() {
		if !strings.HasPrefix(k, "user.") {
			continue
		}

		userConfig[k] = v
	}

	location := "none"
	clustered, err := cluster.Enabled(d.state.Node)
	if err != nil {
		return err
	}
	if clustered {
		location = d.Location()
	}

	out, err := json.Marshal(struct {
		Name     string            `json:"name"`
		Location string            `json:"location"`
		Config   map[string]string `json:"config,omitempty"`
	}{d.Name(), location, userConfig})
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(configDrivePath, "instance-data"), out, 0600)
	if err != nil {
		return err
	}

	return nil
}

// Info returns "qemu" and the currently loaded qemu version.
func (d *qemu) Info() instance.Info {
	data := instance.Info{
		Name:  "qemu",
		Type:  instancetype.VM,
		Error: fmt.Errorf("Unknown error"),
	}

	if !shared.PathExists("/dev/kvm") {
		data.Error = fmt.Errorf("KVM support is missing")
		return data
	}

	err := util.LoadModule("vhost_vsock")
	if err != nil {
		data.Error = fmt.Errorf("vhost_vsock kernel module not loaded: %v", err)
		return data
	}

	hostArch, err := osarch.ArchitectureGetLocalID()
	if err != nil {
		data.Error = fmt.Errorf("Failed getting architecture")
		return data
	}

	qemuPath, _, err := d.qemuArchConfig(hostArch)
	if err != nil {
		data.Error = fmt.Errorf("QEMU command not available for architecture")
		return data
	}

	out, err := exec.Command(qemuPath, "--version").Output()
	if err != nil {
		data.Error = fmt.Errorf("Failed getting QEMU version")
		return data
	}

	qemuOutput := strings.Fields(string(out))
	if len(qemuOutput) >= 4 {
		qemuVersion := strings.Fields(string(out))[3]
		data.Version = qemuVersion
	} else {
		data.Version = "unknown" // Not necessarily an error that should prevent us using driver.
	}

	data.Error = nil

	return data
}
