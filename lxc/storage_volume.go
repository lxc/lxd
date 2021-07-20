package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxc/utils"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	cli "github.com/lxc/lxd/shared/cmd"
	"github.com/lxc/lxd/shared/i18n"
	"github.com/lxc/lxd/shared/ioprogress"
	"github.com/lxc/lxd/shared/termios"
	"github.com/lxc/lxd/shared/units"
)

type volumeColumn struct {
	Name       string
	Data       func(api.StorageVolume, api.StorageVolumeState) string
	NeedsState bool
}

type cmdStorageVolume struct {
	global  *cmdGlobal
	storage *cmdStorage
}

func (c *cmdStorageVolume) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("volume")
	cmd.Short = i18n.G("Manage storage volumes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Manage storage volumes

Unless specified through a prefix, all volume operations affect "custom" (user created) volumes.`))

	// Attach
	storageVolumeAttachCmd := cmdStorageVolumeAttach{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeAttachCmd.Command())

	// Attach profile
	storageVolumeAttachProfileCmd := cmdStorageVolumeAttachProfile{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeAttachProfileCmd.Command())

	// Copy
	storageVolumeCopyCmd := cmdStorageVolumeCopy{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeCopyCmd.Command())

	// Create
	storageVolumeCreateCmd := cmdStorageVolumeCreate{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeCreateCmd.Command())

	// Delete
	storageVolumeDeleteCmd := cmdStorageVolumeDelete{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeDeleteCmd.Command())

	// Detach
	storageVolumeDetachCmd := cmdStorageVolumeDetach{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeDetachCmd.Command())

	// Detach profile
	storageVolumeDetachProfileCmd := cmdStorageVolumeDetachProfile{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeDetachProfileCmd.Command())

	// Edit
	storageVolumeEditCmd := cmdStorageVolumeEdit{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeEditCmd.Command())

	// Export
	storageVolumeExportCmd := cmdStorageVolumeExport{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeExportCmd.Command())

	// Get
	storageVolumeGetCmd := cmdStorageVolumeGet{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeGetCmd.Command())

	// Import
	storageVolumeImportCmd := cmdStorageVolumeImport{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeImportCmd.Command())

	// Info
	storageVolumeInfoCmd := cmdStorageVolumeInfo{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeInfoCmd.Command())

	// List
	storageVolumeListCmd := cmdStorageVolumeList{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeListCmd.Command())

	// Move
	storageVolumeMoveCmd := cmdStorageVolumeMove{global: c.global, storage: c.storage, storageVolume: c, storageVolumeCopy: &storageVolumeCopyCmd}
	cmd.AddCommand(storageVolumeMoveCmd.Command())

	// Rename
	storageVolumeRenameCmd := cmdStorageVolumeRename{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeRenameCmd.Command())

	// Set
	storageVolumeSetCmd := cmdStorageVolumeSet{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeSetCmd.Command())

	// Show
	storageVolumeShowCmd := cmdStorageVolumeShow{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeShowCmd.Command())

	// Snapshot
	storageVolumeSnapshotCmd := cmdStorageVolumeSnapshot{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeSnapshotCmd.Command())

	// Restore
	storageVolumeRestoreCmd := cmdStorageVolumeRestore{global: c.global, storage: c.storage, storageVolume: c}
	cmd.AddCommand(storageVolumeRestoreCmd.Command())

	// Unset
	storageVolumeUnsetCmd := cmdStorageVolumeUnset{global: c.global, storage: c.storage, storageVolume: c, storageVolumeSet: &storageVolumeSetCmd}
	cmd.AddCommand(storageVolumeUnsetCmd.Command())

	// Workaround for subcommand usage errors. See: https://github.com/spf13/cobra/issues/706
	cmd.Args = cobra.NoArgs
	cmd.Run = func(cmd *cobra.Command, args []string) { cmd.Usage() }
	return cmd
}

func (c *cmdStorageVolume) parseVolume(defaultType string, name string) (string, string) {
	fields := strings.SplitN(name, "/", 2)
	if len(fields) == 1 {
		return fields[0], defaultType
	} else if len(fields) == 2 && !shared.StringInSlice(fields[0], []string{"custom", "image", "container", "virtual-machine"}) {
		return name, defaultType
	}

	return fields[1], fields[0]
}

func (c *cmdStorageVolume) parseVolumeWithPool(name string) (string, string) {
	fields := strings.SplitN(name, "/", 2)
	if len(fields) == 1 {
		return fields[0], ""
	}

	return fields[1], fields[0]
}

// Attach
type cmdStorageVolumeAttach struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeAttach) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("attach", i18n.G("[<remote>:]<pool> <volume> <instance> [<device name>] <path>"))
	cmd.Short = i18n.G("Attach new storage volumes to instances")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Attach new storage volumes to instances`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeAttach) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 4, 5)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	// Attach the volume
	devPath := ""
	devName := ""
	if len(args) == 4 {
		// Only the path has been given to us.
		devPath = args[3]
		devName = args[1]
	} else if len(args) == 5 {
		// Path and device name have been given to us.
		devName = args[3]
		devPath = args[4]
	}

	volName, volType := c.storageVolume.parseVolume("custom", args[1])
	if volType != "custom" {
		return fmt.Errorf(i18n.G("Only \"custom\" volumes can be attached to instances"))
	}

	// Prepare the instance's device entry
	device := map[string]string{
		"type":   "disk",
		"pool":   resource.name,
		"source": volName,
		"path":   devPath,
	}

	// Add the device to the instance
	err = instanceDeviceAdd(resource.server, args[2], devName, device)
	if err != nil {
		return err
	}

	return nil
}

// Attach profile
type cmdStorageVolumeAttachProfile struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeAttachProfile) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("attach-profile", i18n.G("[<remote:>]<pool> <volume> <profile> [<device name>] <path>"))
	cmd.Short = i18n.G("Attach new storage volumes to profiles")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Attach new storage volumes to profiles`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeAttachProfile) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 4, 5)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	// Attach the volume
	devPath := ""
	devName := ""
	if len(args) == 4 {
		// Only the path has been given to us.
		devPath = args[3]
		devName = args[1]
	} else if len(args) == 5 {
		// Path and device name have been given to us.
		devName = args[3]
		devPath = args[4]
	}

	volName, volType := c.storageVolume.parseVolume("custom", args[1])
	if volType != "custom" {
		return fmt.Errorf(i18n.G("Only \"custom\" volumes can be attached to instances"))
	}

	// Check if the requested storage volume actually exists
	vol, _, err := resource.server.GetStoragePoolVolume(resource.name, volType, volName)
	if err != nil {
		return err
	}

	// Prepare the instance's device entry
	device := map[string]string{
		"type":   "disk",
		"pool":   resource.name,
		"source": vol.Name,
	}

	// Ignore path for block volumes
	if vol.ContentType != "block" {
		device["path"] = devPath
	}

	// Add the device to the instance
	err = profileDeviceAdd(resource.server, args[2], devName, device)
	if err != nil {
		return err
	}

	return nil
}

// Copy
type cmdStorageVolumeCopy struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume

	flagMode          string
	flagVolumeOnly    bool
	flagTargetProject string
	flagRefresh       bool
}

func (c *cmdStorageVolumeCopy) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("copy", i18n.G("<pool>/<volume>[/<snapshot>] <pool>/<volume>"))
	cmd.Aliases = []string{"cp"}
	cmd.Short = i18n.G("Copy storage volumes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Copy storage volumes`))

	cmd.Flags().StringVar(&c.flagMode, "mode", "pull", i18n.G("Transfer mode. One of pull (default), push or relay.")+"``")
	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.Flags().BoolVar(&c.flagVolumeOnly, "volume-only", false, i18n.G("Copy the volume without its snapshots"))
	cmd.Flags().StringVar(&c.flagTargetProject, "target-project", "", i18n.G("Copy to a project different from the source")+"``")
	cmd.Flags().BoolVar(&c.flagRefresh, "refresh", false, i18n.G("Refresh and update the existing storage volume copies"))
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeCopy) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0], args[1])
	if err != nil {
		return err
	}

	// Source
	srcResource := resources[0]
	if srcResource.name == "" {
		return fmt.Errorf(i18n.G("Missing source volume name"))
	}

	srcServer := srcResource.server
	srcPath := srcResource.name

	// Destination
	dstResource := resources[1]
	dstServer := dstResource.server
	dstPath := dstResource.name

	// Get source pool and volume name
	srcVolName, srcVolPool := c.storageVolume.parseVolumeWithPool(srcPath)
	if srcVolPool == "" {
		return fmt.Errorf(i18n.G("No storage pool for source volume specified"))
	}

	// Get destination pool and volume name
	// TODO: Make is possible to run lxc storage volume copy pool/vol/snap new-pool/new-vol/new-snap
	dstVolName, dstVolPool := c.storageVolume.parseVolumeWithPool(dstPath)
	if dstVolPool == "" {
		return fmt.Errorf(i18n.G("No storage pool for target volume specified"))
	}

	// Parse the mode
	mode := "pull"
	if c.flagMode != "" {
		mode = c.flagMode
	}

	if c.flagTargetProject != "" {
		dstServer = dstServer.UseProject(c.flagTargetProject)
	}

	var op lxd.RemoteOperation

	// Messages
	opMsg := i18n.G("Copying the storage volume: %s")
	finalMsg := i18n.G("Storage volume copied successfully!")

	if cmd.Name() == "move" {
		opMsg = i18n.G("Moving the storage volume: %s")
		finalMsg = i18n.G("Storage volume moved successfully!")
	}

	var srcVol *api.StorageVolume

	// Check if requested storage volume exists
	isSnapshot := shared.IsSnapshot(srcVolName)

	if isSnapshot {
		fields := strings.SplitN(srcVolName, "/", 2)
		_, _, err = srcServer.GetStoragePoolVolumeSnapshot(srcVolPool,
			"custom", fields[0], fields[1])
		if err != nil {
			return err
		}

		srcVol, _, err = srcServer.GetStoragePoolVolume(srcVolPool, "custom", fields[0])
	} else {
		srcVol, _, err = srcServer.GetStoragePoolVolume(srcVolPool, "custom", srcVolName)
	}
	if err != nil {
		return err
	}

	if cmd.Name() == "move" && srcServer == dstServer {
		args := &lxd.StoragePoolVolumeMoveArgs{}
		args.Name = dstVolName
		args.Mode = mode
		args.VolumeOnly = false

		if isSnapshot {
			srcVol.Name = srcVolName
		}

		op, err = dstServer.MoveStoragePoolVolume(dstVolPool, srcServer, srcVolPool, *srcVol, args)
		if err != nil {
			return err
		}
	} else {
		args := &lxd.StoragePoolVolumeCopyArgs{}
		args.Name = dstVolName
		args.Mode = mode
		args.VolumeOnly = c.flagVolumeOnly
		args.Refresh = c.flagRefresh

		if isSnapshot {
			srcVol.Name = srcVolName
		}

		op, err = dstServer.CopyStoragePoolVolume(dstVolPool, srcServer, srcVolPool, *srcVol, args)
		if err != nil {
			return err
		}
	}

	// Register progress handler
	progress := utils.ProgressRenderer{
		Format: opMsg,
		Quiet:  c.global.flagQuiet,
	}

	_, err = op.AddHandler(progress.UpdateOp)
	if err != nil {
		progress.Done("")
		return err
	}

	// Wait for operation to finish
	err = utils.CancelableWait(op, &progress)
	if err != nil {
		progress.Done("")
		return err
	}

	if cmd.Name() == "move" && srcServer != dstServer {
		err := srcServer.DeleteStoragePoolVolume(srcVolPool, srcVol.Type, srcVolName)
		if err != nil {
			progress.Done("")
			return err
		}
	}
	progress.Done(finalMsg)

	return nil
}

// Create
type cmdStorageVolumeCreate struct {
	global          *cmdGlobal
	storage         *cmdStorage
	storageVolume   *cmdStorageVolume
	flagContentType string
}

func (c *cmdStorageVolumeCreate) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("create", i18n.G("[<remote>:]<pool> <volume> [key=value...]"))
	cmd.Short = i18n.G("Create new custom storage volumes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Create new custom storage volumes`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.Flags().StringVar(&c.flagContentType, "type", "filesystem", i18n.G("Content type, block or filesystem")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeCreate) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, -1)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	// Create the storage volume entry
	vol := api.StorageVolumesPost{}
	vol.Name = volName
	vol.Type = volType
	vol.ContentType = c.flagContentType
	vol.Config = map[string]string{}

	for i := 2; i < len(args); i++ {
		entry := strings.SplitN(args[i], "=", 2)
		if len(entry) < 2 {
			return fmt.Errorf(i18n.G("Bad key=value pair: %s"), entry)
		}

		vol.Config[entry[0]] = entry[1]
	}

	// If a target was specified, create the volume on the given member.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	err = client.CreateStoragePoolVolume(resource.name, vol)
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Storage volume %s created")+"\n", args[1])
	}

	return nil
}

// Delete
type cmdStorageVolumeDelete struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeDelete) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("delete", i18n.G("[<remote>:]<pool> <volume>[/<snapshot>]"))
	cmd.Aliases = []string{"rm"}
	cmd.Short = i18n.G("Delete storage volumes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Delete storage volumes`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeDelete) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]
	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	// If a target was specified, create the volume on the given member.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	fields := strings.SplitN(volName, "/", 2)
	if len(fields) == 2 {
		// Delete the snapshot
		op, err := client.DeleteStoragePoolVolumeSnapshot(resource.name, volType, fields[0], fields[1])
		if err != nil {
			return err
		}

		err = op.Wait()
		if err != nil {
			return err
		}

	} else {
		// Delete the volume
		err := client.DeleteStoragePoolVolume(resource.name, volType, volName)
		if err != nil {
			return err
		}
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Storage volume %s deleted")+"\n", args[1])
	}

	return nil
}

// Detach
type cmdStorageVolumeDetach struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeDetach) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("detach", i18n.G("[<remote>:]<pool> <volume> <instance> [<device name>]"))
	cmd.Short = i18n.G("Detach storage volumes from instances")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Detach storage volumes from instances`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeDetach) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 3, 4)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	// Detach storage volumes
	devName := ""
	if len(args) == 4 {
		devName = args[3]
	}

	// Get the instance entry
	inst, etag, err := resource.server.GetInstance(args[2])
	if err != nil {
		return err
	}

	// Find the device
	if devName == "" {
		for n, d := range inst.Devices {
			if d["type"] == "disk" && d["pool"] == resource.name && d["source"] == args[1] {
				if devName != "" {
					return fmt.Errorf(i18n.G("More than one device matches, specify the device name"))
				}

				devName = n
			}
		}
	}

	if devName == "" {
		return fmt.Errorf(i18n.G("No device found for this storage volume"))
	}

	_, ok := inst.Devices[devName]
	if !ok {
		return fmt.Errorf(i18n.G("The specified device doesn't exist"))
	}

	// Remove the device
	delete(inst.Devices, devName)
	op, err := resource.server.UpdateInstance(args[2], inst.Writable(), etag)
	if err != nil {
		return err
	}

	return op.Wait()
}

// Detach profile
type cmdStorageVolumeDetachProfile struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeDetachProfile) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("detach-profile", i18n.G("[<remote:>]<pool> <volume> <profile> [<device name>]"))
	cmd.Short = i18n.G("Detach storage volumes from profiles")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Detach storage volumes from profiles`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeDetachProfile) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 3, 4)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	devName := ""
	if len(args) > 3 {
		devName = args[3]
	}

	// Get the profile entry
	profile, etag, err := resource.server.GetProfile(args[2])
	if err != nil {
		return err
	}

	// Find the device
	if devName == "" {
		for n, d := range profile.Devices {
			if d["type"] == "disk" && d["pool"] == resource.name && d["source"] == args[1] {
				if devName != "" {
					return fmt.Errorf(i18n.G("More than one device matches, specify the device name"))
				}

				devName = n
			}
		}
	}

	if devName == "" {
		return fmt.Errorf(i18n.G("No device found for this storage volume"))
	}

	_, ok := profile.Devices[devName]
	if !ok {
		return fmt.Errorf(i18n.G("The specified device doesn't exist"))
	}

	// Remove the device
	delete(profile.Devices, devName)
	err = resource.server.UpdateProfile(args[2], profile.Writable(), etag)
	if err != nil {
		return err
	}

	return nil
}

// Edit
type cmdStorageVolumeEdit struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeEdit) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("edit", i18n.G("[<remote>:]<pool> <volume>[/<snapshot>]"))
	cmd.Short = i18n.G("Edit storage volume configurations as YAML")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Edit storage volume configurations as YAML`))
	cmd.Example = cli.FormatSection("", i18n.G(
		`lxc storage volume edit [<remote>:]<pool> <volume> < volume.yaml
    Update a storage volume using the content of pool.yaml.`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeEdit) helpTemplate() string {
	return i18n.G(
		`### This is a YAML representation of a storage volume.
### Any line starting with a '# will be ignored.
###
### A storage volume consists of a set of configuration items.
###
### name: vol1
### type: custom
### used_by: []
### config:
###   size: "61203283968"`)
}

func (c *cmdStorageVolumeEdit) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	isSnapshot := false
	fields := strings.Split(volName, "/")
	if len(fields) > 2 {
		return fmt.Errorf("Invalid snapshot name")
	} else if len(fields) > 1 {
		isSnapshot = true
	}

	// If stdin isn't a terminal, read text from it
	if !termios.IsTerminal(getStdinFd()) {
		contents, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return err
		}

		if isSnapshot {
			newdata := api.StorageVolumeSnapshotPut{}
			err = yaml.Unmarshal(contents, &newdata)
			if err != nil {
				return err
			}

			err := client.UpdateStoragePoolVolumeSnapshot(resource.name, volType, fields[0], fields[1], newdata, "")
			if err != nil {
				return err
			}

			return nil
		}

		newdata := api.StorageVolumePut{}
		err = yaml.Unmarshal(contents, &newdata)
		if err != nil {
			return err
		}

		return client.UpdateStoragePoolVolume(resource.name, volType, volName, newdata, "")
	}

	// If a target was specified, create the volume on the given member.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	var data []byte
	var snapVol *api.StorageVolumeSnapshot
	var vol *api.StorageVolume
	etag := ""
	if isSnapshot {
		// Extract the current value
		snapVol, etag, err = client.GetStoragePoolVolumeSnapshot(resource.name, volType, fields[0], fields[1])
		if err != nil {
			return err
		}

		data, err = yaml.Marshal(&snapVol)
		if err != nil {
			return err
		}
	} else {
		// Extract the current value
		vol, etag, err = client.GetStoragePoolVolume(resource.name, volType, volName)
		if err != nil {
			return err
		}

		data, err = yaml.Marshal(&vol)
		if err != nil {
			return err
		}
	}

	// Spawn the editor
	content, err := shared.TextEditor("", []byte(c.helpTemplate()+"\n\n"+string(data)))
	if err != nil {
		return err
	}

	if isSnapshot {
		for {
			// Parse the text received from the editor
			newdata := api.StorageVolumeSnapshotPut{}
			err = yaml.Unmarshal(content, &newdata)
			if err == nil {
				err = client.UpdateStoragePoolVolumeSnapshot(resource.name, volType, fields[0], fields[1], newdata, etag)
			}

			// Respawn the editor
			if err != nil {
				fmt.Fprintf(os.Stderr, i18n.G("Config parsing error: %s")+"\n", err)
				fmt.Println(i18n.G("Press enter to open the editor again or ctrl+c to abort change"))

				_, err := os.Stdin.Read(make([]byte, 1))
				if err != nil {
					return err
				}

				content, err = shared.TextEditor("", content)
				if err != nil {
					return err
				}
				continue
			}
			break
		}

		return nil
	}

	for {
		// Parse the text received from the editor
		newdata := api.StorageVolume{}
		err = yaml.Unmarshal(content, &newdata)
		if err == nil {
			err = client.UpdateStoragePoolVolume(resource.name, volType, volName, newdata.Writable(), etag)
		}

		// Respawn the editor
		if err != nil {
			fmt.Fprintf(os.Stderr, i18n.G("Config parsing error: %s")+"\n", err)
			fmt.Println(i18n.G("Press enter to open the editor again or ctrl+c to abort change"))

			_, err := os.Stdin.Read(make([]byte, 1))
			if err != nil {
				return err
			}

			content, err = shared.TextEditor("", content)
			if err != nil {
				return err
			}
			continue
		}
		break
	}

	return nil
}

// Get
type cmdStorageVolumeGet struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeGet) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("get", i18n.G("[<remote>:]<pool> <volume>[/<snapshot>] <key>"))
	cmd.Short = i18n.G("Get values for storage volume configuration keys")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Get values for storage volume configuration keys`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeGet) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 3, 3)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	isSnapshot := false
	fields := strings.Split(volName, "/")
	if len(fields) > 2 {
		return fmt.Errorf("Invalid snapshot name")
	} else if len(fields) > 1 {
		isSnapshot = true
	}

	// If a target was specified, create the volume on the given member.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	if isSnapshot {
		// Get the storage volume snapshot entry
		resp, _, err := client.GetStoragePoolVolumeSnapshot(resource.name, volType, fields[0], fields[1])
		if err != nil {
			return err
		}

		for k, v := range resp.Config {
			if k == args[2] {
				fmt.Printf("%s\n", v)
			}
		}

		return nil
	}

	// Get the storage volume entry
	resp, _, err := client.GetStoragePoolVolume(resource.name, volType, volName)
	if err != nil {
		return err
	}

	for k, v := range resp.Config {
		if k == args[2] {
			fmt.Printf("%s\n", v)
		}
	}

	return nil
}

// Info
type cmdStorageVolumeInfo struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeInfo) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("info", i18n.G("[<remote>:]<pool> <volume>"))
	cmd.Short = i18n.G("Show storage volume state information")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Show storage volume state information`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeInfo) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	isSnapshot := false
	fields := strings.Split(volName, "/")
	if len(fields) > 2 {
		return fmt.Errorf("Invalid snapshot name")
	} else if len(fields) > 1 {
		isSnapshot = true
	}

	// Check if syntax matches a snapshot
	if isSnapshot || volType == "image" {
		return fmt.Errorf(i18n.G("Only instance or custom volumes are supported"))
	}

	// If a target member was specified, get the volume with the matching
	// name on that member, if any.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	state, err := client.GetStoragePoolVolumeState(resource.name, volType, volName)
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(&state)
	if err != nil {
		return err
	}

	fmt.Printf("%s", data)

	return nil
}

// List
type cmdStorageVolumeList struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume

	flagFormat  string
	flagColumns string

	defaultColumns string
}

func (c *cmdStorageVolumeList) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("list", i18n.G("[<remote>:]<pool>"))
	cmd.Aliases = []string{"ls"}
	cmd.Short = i18n.G("List storage volumes")

	c.defaultColumns = "tndcuL"
	cmd.Flags().StringVarP(&c.flagColumns, "columns", "c", c.defaultColumns, i18n.G("Columns")+"``")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`List storage volumes

The -c option takes a (optionally comma-separated) list of arguments
that control which image attributes to output when displaying in table
or csv format.

Default column layout is: lfpdasu

Column shorthand chars:

    t - Type of volume (custom, image, container or virtual-machine)
    n - Name
    d - Description
    c - Content type (filesystem or block)
    u - Number of references (used by)
    L - Location of the instance (e.g. its cluster member)
    U - Current disk usage`))
	cmd.Flags().StringVarP(&c.flagFormat, "format", "f", "table", i18n.G("Format (csv|json|table|yaml)")+"``")

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeList) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	volumes, err := resource.server.GetStoragePoolVolumes(resource.name)
	if err != nil {
		return err
	}

	// Process the columns
	columns, err := c.parseColumns(resource.server.IsClustered())
	if err != nil {
		return err
	}

	// Render the table
	data := [][]string{}
	for _, vol := range volumes {
		row := []string{}
		for _, column := range columns {
			if column.NeedsState && !shared.IsSnapshot(vol.Name) && vol.Type != "image" {
				state, err := resource.server.GetStoragePoolVolumeState(resource.name, vol.Type, vol.Name)
				if err != nil {
					return err
				}

				row = append(row, column.Data(vol, *state))
			} else {
				row = append(row, column.Data(vol, api.StorageVolumeState{}))
			}
		}
		data = append(data, row)
	}
	if len(columns) >= 2 {
		sort.Sort(byNameAndType(data))
	}

	rawData := make([]*api.StorageVolume, len(volumes))
	for i := range volumes {
		rawData[i] = &volumes[i]
	}

	headers := []string{}
	for _, column := range columns {
		headers = append(headers, column.Name)
	}

	return utils.RenderTable(c.flagFormat, headers, data, rawData)
}

func (c *cmdStorageVolumeList) parseColumns(clustered bool) ([]volumeColumn, error) {
	columnsShorthandMap := map[rune]volumeColumn{
		't': {Name: i18n.G("TYPE"), Data: c.typeColumnData},
		'n': {Name: i18n.G("NAME"), Data: c.nameColumnData},
		'd': {Name: i18n.G("DESCRIPTION"), Data: c.descriptionColumnData},
		'c': {Name: i18n.G("CONTENT-TYPE"), Data: c.contentTypeColumnData},
		'u': {Name: i18n.G("USED BY"), Data: c.usedByColumnData},
		'U': {Name: i18n.G("USAGE"), Data: c.usageColumnData, NeedsState: true},
	}

	if clustered {
		columnsShorthandMap['L'] = volumeColumn{Name: i18n.G("LOCATION"), Data: c.locationColumnData}
	} else {
		if c.flagColumns != c.defaultColumns {
			if strings.ContainsAny(c.flagColumns, "L") {
				return nil, fmt.Errorf(i18n.G("Can't specify column L when not clustered"))
			}
		}
		c.flagColumns = strings.Replace(c.flagColumns, "L", "", -1)
	}

	columnList := strings.Split(c.flagColumns, ",")

	columns := []volumeColumn{}
	for _, columnEntry := range columnList {
		if columnEntry == "" {
			return nil, fmt.Errorf(i18n.G("Empty column entry (redundant, leading or trailing command) in '%s'"), c.flagColumns)
		}

		for _, columnRune := range columnEntry {
			if column, ok := columnsShorthandMap[columnRune]; ok {
				columns = append(columns, column)
			} else {
				return nil, fmt.Errorf(i18n.G("Unknown column shorthand char '%c' in '%s'"), columnRune, columnEntry)
			}
		}
	}

	return columns, nil
}

func (c *cmdStorageVolumeList) typeColumnData(vol api.StorageVolume, state api.StorageVolumeState) string {
	if shared.IsSnapshot(vol.Name) {
		return fmt.Sprintf("%s (snapshot)", vol.Type)
	}

	return vol.Type
}

func (c *cmdStorageVolumeList) nameColumnData(vol api.StorageVolume, state api.StorageVolumeState) string {
	return vol.Name
}

func (c *cmdStorageVolumeList) descriptionColumnData(vol api.StorageVolume, state api.StorageVolumeState) string {
	return vol.Description
}

func (c *cmdStorageVolumeList) contentTypeColumnData(vol api.StorageVolume, state api.StorageVolumeState) string {
	if vol.ContentType == "" {
		return "filesystem"
	}

	return vol.ContentType
}

func (c *cmdStorageVolumeList) usedByColumnData(vol api.StorageVolume, state api.StorageVolumeState) string {
	return strconv.Itoa(len(vol.UsedBy))
}

func (c *cmdStorageVolumeList) locationColumnData(vol api.StorageVolume, state api.StorageVolumeState) string {
	return vol.Location
}

func (c *cmdStorageVolumeList) usageColumnData(vol api.StorageVolume, state api.StorageVolumeState) string {
	if state.Usage != nil {
		return units.GetByteSizeString(int64(state.Usage.Used), 2)
	}

	return ""
}

// Move
type cmdStorageVolumeMove struct {
	global            *cmdGlobal
	storage           *cmdStorage
	storageVolume     *cmdStorageVolume
	storageVolumeCopy *cmdStorageVolumeCopy
}

func (c *cmdStorageVolumeMove) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("move", i18n.G("<pool>/<volume> <pool>/<volume>"))
	cmd.Aliases = []string{"mv"}
	cmd.Short = i18n.G("Move storage volumes between pools")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Move storage volumes between pools`))

	cmd.Flags().StringVar(&c.storageVolumeCopy.flagMode, "mode", "pull", i18n.G("Transfer mode, one of pull (default), push or relay")+"``")
	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.Flags().StringVar(&c.storageVolumeCopy.flagTargetProject, "target-project", "", i18n.G("Move to a project different from the source")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeMove) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	return c.storageVolumeCopy.Run(cmd, args)
}

// Rename
type cmdStorageVolumeRename struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeRename) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("rename", i18n.G("[<remote>:]<pool> <old name>[/<old snapshot name>] <new name>[/<new snapshot name>]"))
	cmd.Short = i18n.G("Rename storage volumes and storage volume snapshots")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Rename storage volumes`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeRename) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 3, 3)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	isSnapshot := false
	fields := strings.Split(volName, "/")
	if len(fields) > 2 {
		return fmt.Errorf("Invalid snapshot name")
	} else if len(fields) > 1 {
		isSnapshot = true
	}

	if isSnapshot {
		// Create the storage volume entry
		vol := api.StorageVolumeSnapshotPost{}
		dstParentName, dstSnapName, dstIsSnap := shared.InstanceGetParentAndSnapshotName(args[2])

		if dstParentName != fields[0] {
			return fmt.Errorf("Invalid new snapshot name, parent volume must be the same as source")
		}

		if !dstIsSnap {
			return fmt.Errorf("Invalid new snapshot name")
		}

		vol.Name = dstSnapName

		// If a target member was specified, get the volume with the matching
		// name on that member, if any.
		if c.storage.flagTarget != "" {
			client = client.UseTarget(c.storage.flagTarget)
		}

		if len(fields) != 2 {
			return fmt.Errorf("Not a snapshot name")
		}

		op, err := client.RenameStoragePoolVolumeSnapshot(resource.name, volType, fields[0], fields[1], vol)
		if err != nil {
			return err
		}

		err = op.Wait()
		if err != nil {
			return err
		}

		fmt.Printf(i18n.G(`Renamed storage volume from "%s" to "%s"`)+"\n", volName, vol.Name)
		return nil
	}

	// Create the storage volume entry
	vol := api.StorageVolumePost{}
	vol.Name = args[2]

	// If a target member was specified, get the volume with the matching
	// name on that member, if any.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	err = client.RenameStoragePoolVolume(resource.name, volType, volName, vol)
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G(`Renamed storage volume from "%s" to "%s"`)+"\n", volName, vol.Name)
	}

	return nil
}

// Set
type cmdStorageVolumeSet struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeSet) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("set", i18n.G("[<remote>:]<pool> <volume> <key>=<value>..."))
	cmd.Short = i18n.G("Set storage volume configuration keys")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Set storage volume configuration keys

For backward compatibility, a single configuration key may still be set with:
    lxc storage volume set [<remote>:]<pool> <volume> <key> <value>`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeSet) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 3, -1)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]
	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	// If a target was specified, create the volume on the given member.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	// Get the storage volume entry
	vol, etag, err := client.GetStoragePoolVolume(resource.name, volType, volName)
	if err != nil {
		return err
	}

	// Get the values
	keys, err := getConfig(args[2:]...)
	if err != nil {
		return err
	}

	// Update the volume
	for k, v := range keys {
		vol.Config[k] = v
	}

	err = client.UpdateStoragePoolVolume(resource.name, vol.Type, vol.Name, vol.Writable(), etag)
	if err != nil {
		return err
	}

	return nil
}

// Show
type cmdStorageVolumeShow struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeShow) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("show", i18n.G("[<remote>:]<pool> <volume>[/<snapshot>]"))
	cmd.Short = i18n.G("Show storage volume configurations")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Show storage volume configurations`))
	cmd.Example = cli.FormatSection("", i18n.G(
		`lxc storage volume show default data
    Will show the properties of a custom volume called "data" in the "default" pool.

lxc storage volume show default container/data
    Will show the properties of the filesystem for a container called "data" in the "default" pool.`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeShow) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])

	isSnapshot := false
	fields := strings.Split(volName, "/")
	if len(fields) > 2 {
		return fmt.Errorf("Invalid snapshot name")
	} else if len(fields) > 1 {
		isSnapshot = true
	}

	// If a target member was specified, get the volume with the matching
	// name on that member, if any.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	// Get the storage volume entry
	if isSnapshot {
		vol, _, err := client.GetStoragePoolVolumeSnapshot(resource.name, volType, fields[0], fields[1])
		if err != nil {
			return err
		}

		data, err := yaml.Marshal(&vol)
		if err != nil {
			return err
		}

		fmt.Printf("%s", data)

		return nil
	}

	vol, _, err := client.GetStoragePoolVolume(resource.name, volType, volName)
	if err != nil {
		return err
	}

	sort.Strings(vol.UsedBy)

	data, err := yaml.Marshal(&vol)
	if err != nil {
		return err
	}

	fmt.Printf("%s", data)

	return nil
}

// Unset
type cmdStorageVolumeUnset struct {
	global           *cmdGlobal
	storage          *cmdStorage
	storageVolume    *cmdStorageVolume
	storageVolumeSet *cmdStorageVolumeSet
}

func (c *cmdStorageVolumeUnset) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("unset", i18n.G("[<remote>:]<pool> <volume> <key>"))
	cmd.Short = i18n.G("Unset storage volume configuration keys")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Unset storage volume configuration keys`))

	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeUnset) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 3, 3)
	if exit {
		return err
	}

	args = append(args, "")
	return c.storageVolumeSet.Run(cmd, args)
}

// Snapshot
type cmdStorageVolumeSnapshot struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume

	flagNoExpiry bool
	flagReuse    bool
}

func (c *cmdStorageVolumeSnapshot) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("snapshot", i18n.G("[<remote>:]<pool> <volume> [<snapshot>]"))
	cmd.Short = i18n.G("Snapshot storage volumes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Snapshot storage volumes`))

	cmd.RunE = c.Run
	cmd.Flags().BoolVar(&c.flagNoExpiry, "no-expiry", false, i18n.G("Ignore any configured auto-expiry for the storage volume"))
	cmd.Flags().BoolVar(&c.flagReuse, "reuse", false, i18n.G("If the snapshot name already exists, delete and create a new one"))
	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")

	return cmd
}

func (c *cmdStorageVolumeSnapshot) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 3)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]
	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Use the provided target.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	// Parse the input
	volName, volType := c.storageVolume.parseVolume("custom", args[1])
	if volType != "custom" {
		return fmt.Errorf(i18n.G("Only \"custom\" volumes can be snapshotted"))
	}

	// Check if the requested storage volume actually exists
	_, _, err = client.GetStoragePoolVolume(resource.name, volType, volName)
	if err != nil {
		return err
	}

	var snapname string
	if len(args) < 3 {
		snapname = ""
	} else {
		snapname = args[2]
	}

	req := api.StorageVolumeSnapshotsPost{
		Name: snapname,
	}

	if c.flagNoExpiry {
		req.ExpiresAt = &time.Time{}
	}

	if c.flagReuse && snapname != "" {
		snap, _, _ := client.GetStoragePoolVolumeSnapshot(resource.name, volType, volName, snapname)
		if snap != nil {
			op, err := client.DeleteStoragePoolVolumeSnapshot(resource.name, volType, volName, snapname)
			if err != nil {
				return err
			}

			err = op.Wait()
			if err != nil {
				return err
			}
		}
	}

	op, err := client.CreateStoragePoolVolumeSnapshot(resource.name, volType, volName, req)
	if err != nil {
		return err
	}

	return op.Wait()

}

// Restore
type cmdStorageVolumeRestore struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeRestore) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("restore", i18n.G("[<remote>:]<pool> <volume> <snapshot>"))
	cmd.Short = i18n.G("Restore storage volume snapshots")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Restore storage volume snapshots`))
	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeRestore) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 3, 3)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]
	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing pool name"))
	}

	client := resource.server

	// Use the provided target.
	if c.storage.flagTarget != "" {
		client = client.UseTarget(c.storage.flagTarget)
	}

	// Check if the requested storage volume actually exists
	_, _, err = client.GetStoragePoolVolume(resource.name, "custom", args[1])
	if err != nil {
		return err
	}

	req := api.StorageVolumePut{
		Restore: args[2],
	}

	_, etag, err := client.GetStoragePoolVolume(resource.name, "custom", args[1])
	if err != nil {
		return err
	}

	return client.UpdateStoragePoolVolume(resource.name, "custom", args[1], req, etag)
}

// Export
type cmdStorageVolumeExport struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume

	flagVolumeOnly           bool
	flagOptimizedStorage     bool
	flagCompressionAlgorithm string
}

func (c *cmdStorageVolumeExport) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("export", i18n.G("[<remote>:]<pool> <volume> [<path>]"))
	cmd.Short = i18n.G("Export custom storage volume")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Export custom storage volume`))

	cmd.Flags().BoolVar(&c.flagVolumeOnly, "volume-only", false, i18n.G("Export the volume without its snapshots"))
	cmd.Flags().BoolVar(&c.flagOptimizedStorage, "optimized-storage", false,
		i18n.G("Use storage driver optimized format (can only be restored on a similar pool)"))
	cmd.Flags().StringVar(&c.flagCompressionAlgorithm, "compression", "", i18n.G("Define a compression algorithm: for backup or none")+"``")
	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeExport) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 3)
	if exit {
		return err
	}

	// Connect to LXD
	remote, name, err := conf.ParseRemote(args[0])
	if err != nil {
		return err
	}

	d, err := conf.GetInstanceServer(remote)
	if err != nil {
		return err
	}

	// Use the provided target.
	if c.storage.flagTarget != "" {
		d = d.UseTarget(c.storage.flagTarget)
	}

	volumeOnly := c.flagVolumeOnly

	volName, volType := c.storageVolume.parseVolume("custom", args[1])
	if volType != "custom" {
		return fmt.Errorf(i18n.G("Only \"custom\" volumes can be exported"))
	}

	req := api.StoragePoolVolumeBackupsPost{
		Name:                 "",
		ExpiresAt:            time.Now().Add(24 * time.Hour),
		VolumeOnly:           volumeOnly,
		OptimizedStorage:     c.flagOptimizedStorage,
		CompressionAlgorithm: c.flagCompressionAlgorithm,
	}

	op, err := d.CreateStoragePoolVolumeBackup(name, volName, req)
	if err != nil {
		return errors.Wrap(err, "Failed to create storage volume backup")
	}

	// Watch the background operation
	progress := utils.ProgressRenderer{
		Format: i18n.G("Backing up storage volume: %s"),
		Quiet:  c.global.flagQuiet,
	}

	_, err = op.AddHandler(progress.UpdateOp)
	if err != nil {
		progress.Done("")
		return err
	}

	// Wait until backup is done
	err = utils.CancelableWait(op, &progress)
	if err != nil {
		progress.Done("")
		return err
	}
	progress.Done("")

	err = op.Wait()
	if err != nil {
		return err
	}

	// Get name of backup
	backupName := strings.TrimPrefix(op.Get().Resources["backups"][0],
		"/1.0/backups/")

	defer func() {
		// Delete backup after we're done
		op, err = d.DeleteStoragePoolVolumeBackup(name, volName, backupName)
		if err == nil {
			op.Wait()
		}
	}()

	var targetName string
	if len(args) > 2 {
		targetName = args[2]
	} else {
		targetName = "backup.tar.gz"
	}

	target, err := os.Create(shared.HostPathFollow(targetName))
	if err != nil {
		return err
	}
	defer target.Close()

	// Prepare the download request
	progress = utils.ProgressRenderer{
		Format: i18n.G("Exporting the backup: %s"),
		Quiet:  c.global.flagQuiet,
	}
	backupFileRequest := lxd.BackupFileRequest{
		BackupFile:      io.WriteSeeker(target),
		ProgressHandler: progress.UpdateProgress,
	}

	// Export tarball
	_, err = d.GetStoragePoolVolumeBackupFile(name, volName, backupName, &backupFileRequest)
	if err != nil {
		os.Remove(targetName)
		progress.Done("")
		return errors.Wrap(err, "Fetch storage volume backup file")
	}

	progress.Done(i18n.G("Backup exported successfully!"))
	return nil
}

// Import
type cmdStorageVolumeImport struct {
	global        *cmdGlobal
	storage       *cmdStorage
	storageVolume *cmdStorageVolume
}

func (c *cmdStorageVolumeImport) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("import", i18n.G("[<remote>:]<pool> <backup file> [<volume name>]"))
	cmd.Short = i18n.G("Import custom storage volumes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Import backups of custom volumes including their snapshots.`))
	cmd.Example = cli.FormatSection("", i18n.G(
		`lxc storage volume import default backup0.tar.gz
		Create a new custom volume using backup0.tar.gz as the source.`))
	cmd.Flags().StringVar(&c.storage.flagTarget, "target", "", i18n.G("Cluster member name")+"``")
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdStorageVolumeImport) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 3)
	if exit {
		return err
	}

	// Connect to LXD.
	remote, pool, err := conf.ParseRemote(args[0])
	if err != nil {
		return err
	}

	d, err := conf.GetInstanceServer(remote)
	if err != nil {
		return err
	}

	// Use the provided target.
	if c.storage.flagTarget != "" {
		d = d.UseTarget(c.storage.flagTarget)
	}

	file, err := os.Open(shared.HostPathFollow(args[1]))
	if err != nil {
		return err
	}
	defer file.Close()

	fstat, err := file.Stat()
	if err != nil {
		return err
	}

	volName := ""
	if len(args) >= 3 {
		volName = args[2]
	}

	progress := utils.ProgressRenderer{
		Format: i18n.G("Importing custom volume: %s"),
		Quiet:  c.global.flagQuiet,
	}

	createArgs := lxd.StoragePoolVolumeBackupArgs{
		BackupFile: &ioprogress.ProgressReader{
			ReadCloser: file,
			Tracker: &ioprogress.ProgressTracker{
				Length: fstat.Size(),
				Handler: func(percent int64, speed int64) {
					progress.UpdateProgress(ioprogress.ProgressData{Text: fmt.Sprintf("%d%% (%s/s)", percent, units.GetByteSizeString(speed, 2))})
				},
			},
		},
		Name: volName,
	}

	op, err := d.CreateStoragePoolVolumeFromBackup(pool, createArgs)
	if err != nil {
		return err
	}

	// Wait for operation to finish.
	err = utils.CancelableWait(op, &progress)
	if err != nil {
		progress.Done("")
		return err
	}

	progress.Done("")

	return nil
}
