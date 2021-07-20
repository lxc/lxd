package device

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/mdlayher/netx/eui64"
	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/dnsmasq"
	"github.com/lxc/lxd/lxd/dnsmasq/dhcpalloc"
	firewallDrivers "github.com/lxc/lxd/lxd/firewall/drivers"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/network/openvswitch"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/resources"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/validate"
)

type nicBridged struct {
	deviceCommon
}

// CanHotPlug returns whether the device can be managed whilst the instance is running. Returns true.
func (d *nicBridged) CanHotPlug() bool {
	return true
}

// validateConfig checks the supplied config for correctness.
func (d *nicBridged) validateConfig(instConf instance.ConfigReader) error {
	if !instanceSupported(instConf.Type(), instancetype.Container, instancetype.VM) {
		return ErrUnsupportedDevType
	}

	var requiredFields []string
	optionalFields := []string{
		"name",
		"network",
		"parent",
		"mtu",
		"hwaddr",
		"host_name",
		"limits.ingress",
		"limits.egress",
		"limits.max",
		"ipv4.address",
		"ipv6.address",
		"ipv4.routes",
		"ipv6.routes",
		"security.mac_filtering",
		"security.ipv4_filtering",
		"security.ipv6_filtering",
		"security.port_isolation",
		"maas.subnet.ipv4",
		"maas.subnet.ipv6",
		"boot.priority",
		"vlan",
	}

	// checkWithManagedNetwork validates the device's settings against the managed network.
	checkWithManagedNetwork := func(n network.Network) error {
		if n.Status() != api.NetworkStatusCreated {
			return fmt.Errorf("Specified network is not fully created")
		}

		if n.Type() != "bridge" {
			return fmt.Errorf("Specified network must be of type bridge")
		}

		netConfig := n.Config()

		if d.config["ipv4.address"] != "" {
			dhcpv4Subnet := n.DHCPv4Subnet()

			// Check that DHCPv4 is enabled on parent network (needed to use static assigned IPs) when
			// IP filtering isn't enabled (if it is we allow the use of static IPs for this purpose).
			if dhcpv4Subnet == nil && !shared.IsTrue(d.config["security.ipv4_filtering"]) {
				return fmt.Errorf(`Cannot specify "ipv4.address" when DHCP is disabled (unless using security.ipv4_filtering) on network %q`, n.Name())
			}

			// Check the static IP supplied is valid for the linked network. It should be part of the
			// network's subnet, but not necessarily part of the dynamic allocation ranges.
			if dhcpv4Subnet != nil && !dhcpalloc.DHCPValidIP(dhcpv4Subnet, nil, net.ParseIP(d.config["ipv4.address"])) {
				return fmt.Errorf("Device IP address %q not within network %q subnet", d.config["ipv4.address"], n.Name())
			}
		}

		if d.config["ipv6.address"] != "" {
			dhcpv6Subnet := n.DHCPv6Subnet()

			// Check that DHCPv6 is enabled on parent network (needed to use static assigned IPs) when
			// IP filtering isn't enabled (if it is we allow the use of static IPs for this purpose).
			if (dhcpv6Subnet == nil || !shared.IsTrue(netConfig["ipv6.dhcp.stateful"])) && !shared.IsTrue(d.config["security.ipv6_filtering"]) {
				return fmt.Errorf(`Cannot specify "ipv6.address" when DHCP or "ipv6.dhcp.stateful" are disabled (unless using security.ipv6_filtering) on network %q`, n.Name())
			}

			// Check the static IP supplied is valid for the linked network. It should be part of the
			// network's subnet, but not necessarily part of the dynamic allocation ranges.
			if dhcpv6Subnet != nil && !dhcpalloc.DHCPValidIP(dhcpv6Subnet, nil, net.ParseIP(d.config["ipv6.address"])) {
				return fmt.Errorf("Device IP address %q not within network %q subnet", d.config["ipv6.address"], n.Name())
			}
		}

		// When we know the parent network is managed, we can validate the NIC's VLAN settings based on
		// on the bridge driver type.
		if shared.StringInSlice(netConfig["bridge.driver"], []string{"", "native"}) {
			// Check VLAN 0 isn't set when using a native Linux managed bridge, as not supported.
			if d.config["vlan"] == "0" {
				return fmt.Errorf("VLAN ID 0 is not allowed for native Linux bridges")
			}

			// Check that none of the supplied VLAN IDs are VLAN 0 when using a native Linux managed
			// bridge, as not supported.
			for _, vlanID := range util.SplitNTrimSpace(d.config["vlan.tagged"], ",", -1, true) {
				if vlanID == "0" {
					return fmt.Errorf("VLAN tagged ID 0 is not allowed for native Linux bridges")
				}
			}
		}

		return nil
	}

	// Managed network if specified in config (either via parent or network keys), left nil if not.
	var n network.Network

	// Check that if network proeperty is set that conflicting keys are not present.
	if d.config["network"] != "" {
		requiredFields = append(requiredFields, "network")

		bannedKeys := []string{"nictype", "parent", "mtu", "maas.subnet.ipv4", "maas.subnet.ipv6"}
		for _, bannedKey := range bannedKeys {
			if d.config[bannedKey] != "" {
				return fmt.Errorf("Cannot use %q property in conjunction with %q property", bannedKey, "network")
			}
		}

		// Load managed network. project.Default is used here as bridge networks don't support projects.
		var err error
		n, err = network.LoadByName(d.state, project.Default, d.config["network"])
		if err != nil {
			return errors.Wrapf(err, "Error loading network config for %q", d.config["network"])
		}

		// Validate NIC settings with managed network.
		err = checkWithManagedNetwork(n)
		if err != nil {
			return err
		}

		// Apply network settings to NIC.
		netConfig := n.Config()

		// Link device to network bridge.
		d.config["parent"] = d.config["network"]

		// Apply network level config options to device config before validation.
		if netConfig["bridge.mtu"] != "" {
			d.config["mtu"] = netConfig["bridge.mtu"]
		}

		// Copy certain keys verbatim from the network's settings.
		inheritKeys := []string{"maas.subnet.ipv4", "maas.subnet.ipv6"}
		for _, inheritKey := range inheritKeys {
			if _, found := netConfig[inheritKey]; found {
				d.config[inheritKey] = netConfig[inheritKey]
			}
		}
	} else {
		// If no network property supplied, then parent property is required.
		requiredFields = append(requiredFields, "parent")

		// Check if parent is a managed network.
		// project.Default is used here as bridge networks don't support projects.
		n, _ = network.LoadByName(d.state, project.Default, d.config["parent"])
		if n != nil {
			// Validate NIC settings with managed network.
			err := checkWithManagedNetwork(n)
			if err != nil {
				return err
			}
		} else {
			// Check that static IPs are defined when using IP filtering on an unmanaged bridge.
			if shared.IsTrue(d.config["security.ipv4_filtering"]) && d.config["ipv4.address"] == "" {
				return fmt.Errorf("IPv4 filtering requires a manually specified ipv4.address when using an unmanaged parent bridge")
			}

			if shared.IsTrue(d.config["security.ipv6_filtering"]) && d.config["ipv6.address"] == "" {
				return fmt.Errorf("IPv6 filtering requires a manually specified ipv6.address when using an unmanaged parent bridge")
			}
		}
	}

	// Check that IP filtering isn't being used with VLAN filtering.
	if shared.IsTrue(d.config["security.ipv4_filtering"]) || shared.IsTrue(d.config["security.ipv6_filtering"]) {
		if d.config["vlan"] != "" || d.config["vlan.tagged"] != "" {
			return fmt.Errorf("IP filtering cannot be used with VLAN filtering")
		}
	}

	// Check there isn't another NIC with any of the same addresses specified on the same cluster member.
	// Can only validate this when the instance is supplied (and not doing profile validation).
	if d.inst != nil {
		filter := db.InstanceFilter{
			Project: "",                // All projects.
			Node:    d.inst.Location(), // Managed bridge networks have a per-server DHCP daemon.
			Type:    instancetype.Any,
		}

		ourNICIPs := make(map[string]net.IP, 2)
		ourNICIPs["ipv4.address"] = net.ParseIP(d.config["ipv4.address"])
		ourNICIPs["ipv6.address"] = net.ParseIP(d.config["ipv6.address"])

		ourNICMAC, _ := net.ParseMAC(d.config["hwaddr"])
		if ourNICMAC == nil {
			v := d.volatileGet()
			ourNICMAC, _ = net.ParseMAC(v["hwaddr"])
		}

		err := d.state.Cluster.InstanceList(&filter, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
			// Get the instance's effective network project name.
			instNetworkProject := project.NetworkProjectFromRecord(&p)

			if instNetworkProject != project.Default {
				return nil // Managed bridge networks can only exist in default project.
			}

			devices := db.ExpandInstanceDevices(deviceConfig.NewDevices(inst.Devices), profiles)

			// Iterate through each of the instance's devices, looking for NICs that are linked to
			// the same network, on the same cluster member as this NIC and have matching static IPs.
			for devName, devConfig := range devices {
				if devConfig["type"] != "nic" {
					continue
				}

				// Skip NICs that specify a NIC type that is not the same as our own.
				if !shared.StringInSlice(devConfig["nictype"], []string{"", "bridged"}) {
					continue
				}

				// Skip our own device.
				if inst.Name == d.inst.Name() && inst.Project == d.inst.Project() && d.Name() == devName {
					continue
				}

				// Skip NICs not connected to our NIC's managed network.
				// If our NIC is connected to a managed network (either via network or parent keys)
				// but the other NIC doesn't reference the same network name via either its network
				// or parent keys then we can say it is connected to a different network, so the
				// duplicate checks can be skipped.
				if n != nil && n.Name() != devConfig["network"] && n.Name() != devConfig["parent"] {
					continue
				}

				// Skip NICs that are connected to a managed network or different unmanaged parent
				// when we are not connected to a managed network.
				if n == nil && (devConfig["network"] != "" || d.config["parent"] != devConfig["parent"]) {
					continue
				}

				// Skip NICs connected to other VLANs (not perfect though as one NIC could
				// explicitly specify the default untagged VLAN and these would be connected to
				// same L2 even though the values are different, and there is a different default
				// value for native and openvswith parent bridges).
				if d.config["vlan"] != devConfig["vlan"] {
					continue
				}

				// Check NIC's MAC address doesn't match this NIC's MAC address.
				devNICMAC, _ := net.ParseMAC(devConfig["hwaddr"])
				if devNICMAC == nil {
					devNICMAC, _ = net.ParseMAC(inst.Config[fmt.Sprintf("volatile.%s.hwaddr", devName)])
				}

				if ourNICMAC != nil && devNICMAC != nil && bytes.Compare(ourNICMAC, devNICMAC) == 0 {
					return fmt.Errorf("MAC address %q already defined on another NIC", devNICMAC.String())
				}

				// Check NIC's static IPs don't match this NIC's static IPs.
				for _, key := range []string{"ipv4.address", "ipv6.address"} {
					if d.config[key] == "" {
						continue // No static IP specified on this NIC.
					}

					// Parse IPs to avoid being tripped up by presentation differences.
					devNICIP := net.ParseIP(devConfig[key])

					if ourNICIPs[key] != nil && devNICIP != nil && ourNICIPs[key].Equal(devNICIP) {
						return fmt.Errorf("IP address %q already defined on another NIC", devNICIP.String())
					}
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	rules := nicValidationRules(requiredFields, optionalFields, instConf)

	// Add bridge specific vlan validation.
	rules["vlan"] = func(value string) error {
		if value == "" || value == "none" {
			return nil
		}

		return validate.IsNetworkVLAN(value)
	}

	// Add bridge specific vlan.tagged validation.
	rules["vlan.tagged"] = func(value string) error {
		if value == "" {
			return nil
		}

		err := networkValidVLANList(value)
		if err != nil {
			return err
		}

		// Check that none of the supplied VLAN IDs are the same as the untagged VLAN ID.
		for _, vlanID := range util.SplitNTrimSpace(value, ",", -1, true) {
			if vlanID == d.config["vlan"] {
				return fmt.Errorf("Tagged VLAN ID %q cannot be the same as untagged VLAN ID", vlanID)
			}
		}

		return nil
	}

	// Now run normal validation.
	err := d.config.Validate(rules)
	if err != nil {
		return err
	}

	return nil
}

// validateEnvironment checks the runtime environment for correctness.
func (d *nicBridged) validateEnvironment() error {
	if d.inst.Type() == instancetype.Container && d.config["name"] == "" {
		return fmt.Errorf("Requires name property to start")
	}

	if !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", d.config["parent"])) {
		return fmt.Errorf("Parent device %q doesn't exist", d.config["parent"])
	}

	return nil
}

// UpdatableFields returns a list of fields that can be updated without triggering a device remove & add.
func (d *nicBridged) UpdatableFields(oldDevice Type) []string {
	// Check old and new device types match.
	_, match := oldDevice.(*nicBridged)
	if !match {
		return []string{}
	}

	return []string{"limits.ingress", "limits.egress", "limits.max", "ipv4.routes", "ipv6.routes", "ipv4.address", "ipv6.address", "security.mac_filtering", "security.ipv4_filtering", "security.ipv6_filtering"}
}

// Add is run when a device is added to a non-snapshot instance whether or not the instance is running.
func (d *nicBridged) Add() error {
	// Rebuild dnsmasq entry if needed and reload.
	err := d.rebuildDnsmasqEntry()
	if err != nil {
		return err
	}

	return nil
}

// Start is run when the device is added to a running instance or instance is starting up.
func (d *nicBridged) Start() (*deviceConfig.RunConfig, error) {
	err := d.validateEnvironment()
	if err != nil {
		return nil, err
	}

	revert := revert.New()
	defer revert.Fail()

	saveData := make(map[string]string)
	saveData["host_name"] = d.config["host_name"]

	var peerName string

	// Create veth pair and configure the peer end with custom hwaddr and mtu if supplied.
	if d.inst.Type() == instancetype.Container {
		if saveData["host_name"] == "" {
			saveData["host_name"] = network.RandomDevName("veth")
		}
		peerName, err = networkCreateVethPair(saveData["host_name"], d.config)
	} else if d.inst.Type() == instancetype.VM {
		if saveData["host_name"] == "" {
			saveData["host_name"] = network.RandomDevName("tap")
		}
		peerName = saveData["host_name"] // VMs use the host_name to link to the TAP FD.
		err = networkCreateTap(saveData["host_name"], d.config)
	}

	if err != nil {
		return nil, err
	}

	revert.Add(func() { network.InterfaceRemove(saveData["host_name"]) })

	// Populate device config with volatile fields if needed.
	networkVethFillFromVolatile(d.config, saveData)

	// Apply host-side routes to bridge interface.
	err = networkNICRouteAdd(d.config["parent"], append(util.SplitNTrimSpace(d.config["ipv4.routes"], ",", -1, true), util.SplitNTrimSpace(d.config["ipv6.routes"], ",", -1, true)...)...)
	if err != nil {
		return nil, err
	}

	// Apply host-side limits.
	err = networkSetupHostVethLimits(d.config)
	if err != nil {
		return nil, err
	}

	// Disable IPv6 on host-side veth interface (prevents host-side interface getting link-local address)
	// which isn't needed because the host-side interface is connected to a bridge.
	err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", saveData["host_name"]), "1")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Apply and host-side network filters (uses enriched host_name from networkVethFillFromVolatile).
	err = d.setupHostFilters(nil)
	if err != nil {
		return nil, err
	}
	revert.Add(func() { d.removeFilters(d.config) })

	// Attach host side veth interface to bridge.
	err = network.AttachInterface(d.config["parent"], saveData["host_name"])
	if err != nil {
		return nil, err
	}
	revert.Add(func() { network.DetachInterface(d.config["parent"], saveData["host_name"]) })

	// Attempt to disable router advertisement acceptance.
	err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", saveData["host_name"]), "0")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Attempt to enable port isolation
	if shared.IsTrue(d.config["security.port_isolation"]) {
		link := &ip.Link{Name: saveData["host_name"]}
		err = link.BridgeLinkSetIsolated(true)
		if err != nil {
			return nil, err
		}
	}

	// Detect bridge type and setup VLAN settings on bridge port.
	if network.IsNativeBridge(d.config["parent"]) {
		err = d.setupNativeBridgePortVLANs(saveData["host_name"])
	} else {
		err = d.setupOVSBridgePortVLANs(saveData["host_name"])
	}

	if err != nil {
		return nil, err
	}

	err = d.volatileSet(saveData)
	if err != nil {
		return nil, err
	}

	runConf := deviceConfig.RunConfig{}
	runConf.NetworkInterface = []deviceConfig.RunConfigItem{
		{Key: "type", Value: "phys"},
		{Key: "name", Value: d.config["name"]},
		{Key: "flags", Value: "up"},
		{Key: "link", Value: peerName},
	}

	if d.inst.Type() == instancetype.VM {
		runConf.NetworkInterface = append(runConf.NetworkInterface,
			[]deviceConfig.RunConfigItem{
				{Key: "devName", Value: d.name},
				{Key: "hwaddr", Value: d.config["hwaddr"]},
			}...)
	}

	revert.Success()
	return &runConf, nil
}

// Update applies configuration changes to a started device.
func (d *nicBridged) Update(oldDevices deviceConfig.Devices, isRunning bool) error {
	oldConfig := oldDevices[d.name]
	v := d.volatileGet()

	// Populate device config with volatile fields if needed.
	networkVethFillFromVolatile(d.config, v)
	networkVethFillFromVolatile(oldConfig, v)

	// If an IPv6 address has changed, flush all existing IPv6 leases for instance so instance
	// isn't allocated old IP. This is important with IPv6 because DHCPv6 supports multiple IP
	// address allocation and would result in instance having leases for both old and new IPs.
	if d.config["hwaddr"] != "" && d.config["ipv6.address"] != oldConfig["ipv6.address"] {
		err := d.networkClearLease(d.inst.Name(), d.config["parent"], d.config["hwaddr"], clearLeaseIPv6Only)
		if err != nil {
			return err
		}
	}

	// If instance is running, apply host side limits and filters first before rebuilding
	// dnsmasq config below so that existing config can be used as part of the filter removal.
	if isRunning {
		err := d.validateEnvironment()
		if err != nil {
			return err
		}

		// Validate old config so that it is enriched with network parent config needed for route removal.
		err = Validate(d.inst, d.state, d.name, oldConfig)
		if err != nil {
			return err
		}

		// Remove old host-side routes from bridge interface.
		networkNICRouteDelete(oldConfig["parent"], append(util.SplitNTrimSpace(oldConfig["ipv4.routes"], ",", -1, true), util.SplitNTrimSpace(oldConfig["ipv6.routes"], ",", -1, true)...)...)

		// Apply host-side routes to bridge interface.
		err = networkNICRouteAdd(d.config["parent"], append(util.SplitNTrimSpace(d.config["ipv4.routes"], ",", -1, true), util.SplitNTrimSpace(d.config["ipv6.routes"], ",", -1, true)...)...)
		if err != nil {
			return err
		}

		// Apply host-side limits.
		err = networkSetupHostVethLimits(d.config)
		if err != nil {
			return err
		}

		// Apply and host-side network filters (uses enriched host_name from networkVethFillFromVolatile).
		err = d.setupHostFilters(oldConfig)
		if err != nil {
			return err
		}
	}

	// Rebuild dnsmasq entry if needed and reload.
	err := d.rebuildDnsmasqEntry()
	if err != nil {
		return err
	}

	// If an IPv6 address has changed, if the instance is running we should bounce the host-side
	// veth interface to give the instance a chance to detect the change and re-apply for an
	// updated lease with new IP address.
	if d.config["ipv6.address"] != oldConfig["ipv6.address"] && d.config["host_name"] != "" && shared.PathExists(fmt.Sprintf("/sys/class/net/%s", d.config["host_name"])) {
		link := &ip.Link{Name: d.config["host_name"]}
		err := link.SetDown()
		if err != nil {
			return err
		}
		err = link.SetUp()
		if err != nil {
			return err
		}
	}

	return nil
}

// Stop is run when the device is removed from the instance.
func (d *nicBridged) Stop() (*deviceConfig.RunConfig, error) {
	runConf := deviceConfig.RunConfig{
		PostHooks: []func() error{d.postStop},
	}

	return &runConf, nil
}

// postStop is run after the device is removed from the instance.
func (d *nicBridged) postStop() error {
	defer d.volatileSet(map[string]string{
		"host_name": "",
	})

	v := d.volatileGet()

	networkVethFillFromVolatile(d.config, v)

	if d.config["host_name"] != "" && network.InterfaceExists(d.config["host_name"]) {
		// Detach host-side end of veth pair from bridge (required for openvswitch particularly).
		err := network.DetachInterface(d.config["parent"], d.config["host_name"])
		if err != nil {
			return errors.Wrapf(err, "Failed to detach interface %q from %q", d.config["host_name"], d.config["parent"])
		}

		// Removing host-side end of veth pair will delete the peer end too.
		err = network.InterfaceRemove(d.config["host_name"])
		if err != nil {
			return errors.Wrapf(err, "Failed to remove interface %q", d.config["host_name"])
		}
	}

	// Remove host-side routes from bridge interface.
	networkNICRouteDelete(d.config["parent"], append(util.SplitNTrimSpace(d.config["ipv4.routes"], ",", -1, true), util.SplitNTrimSpace(d.config["ipv6.routes"], ",", -1, true)...)...)
	d.removeFilters(d.config)

	return nil
}

// Remove is run when the device is removed from the instance or the instance is deleted.
func (d *nicBridged) Remove() error {
	if d.config["parent"] != "" {
		dnsmasq.ConfigMutex.Lock()
		defer dnsmasq.ConfigMutex.Unlock()

		if network.InterfaceExists(d.config["parent"]) {
			err := d.networkClearLease(d.inst.Name(), d.config["parent"], d.config["hwaddr"], clearLeaseAll)
			if err != nil {
				return errors.Wrapf(err, "Failed clearing leases")
			}
		}

		// Remove dnsmasq config if it exists (doesn't return error if file is missing).
		err := dnsmasq.RemoveStaticEntry(d.config["parent"], d.inst.Project(), d.inst.Name())
		if err != nil {
			return err
		}

		// Reload dnsmasq to apply new settings if dnsmasq is running.
		if shared.PathExists(shared.VarPath("networks", d.config["parent"], "dnsmasq.pid")) {
			err = dnsmasq.Kill(d.config["parent"], true)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// rebuildDnsmasqEntry rebuilds the dnsmasq host entry if connected to a LXD managed network and reloads dnsmasq.
func (d *nicBridged) rebuildDnsmasqEntry() error {
	// Rebuild dnsmasq config if a bridged device has changed and parent is a managed network.
	if !shared.PathExists(shared.VarPath("networks", d.config["parent"], "dnsmasq.pid")) {
		return nil
	}

	dnsmasq.ConfigMutex.Lock()
	defer dnsmasq.ConfigMutex.Unlock()

	// Use project.Default here as bridge networks don't support projects.
	_, dbInfo, _, err := d.state.Cluster.GetNetworkInAnyState(project.Default, d.config["parent"])
	if err != nil {
		return err
	}

	netConfig := dbInfo.Config
	ipv4Address := d.config["ipv4.address"]
	ipv6Address := d.config["ipv6.address"]

	// If IP filtering is enabled, and no static IP in config, check if there is already a
	// dynamically assigned static IP in dnsmasq config and write that back out in new config.
	if (shared.IsTrue(d.config["security.ipv4_filtering"]) && ipv4Address == "") || (shared.IsTrue(d.config["security.ipv6_filtering"]) && ipv6Address == "") {
		_, curIPv4, curIPv6, err := dnsmasq.DHCPStaticAllocation(d.config["parent"], d.inst.Project(), d.inst.Name())
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		if ipv4Address == "" && curIPv4.IP != nil {
			ipv4Address = curIPv4.IP.String()
		}

		if ipv6Address == "" && curIPv6.IP != nil {
			ipv6Address = curIPv6.IP.String()
		}
	}

	err = dnsmasq.UpdateStaticEntry(d.config["parent"], d.inst.Project(), d.inst.Name(), netConfig, d.config["hwaddr"], ipv4Address, ipv6Address)
	if err != nil {
		return err
	}

	// Reload dnsmasq to apply new settings.
	err = dnsmasq.Kill(d.config["parent"], true)
	if err != nil {
		return err
	}

	return nil
}

// setupHostFilters applies any host side network filters.
func (d *nicBridged) setupHostFilters(oldConfig deviceConfig.Device) error {
	// Check br_netfilter kernel module is loaded and enabled for IPv6 before clearing existing rules.
	// We won't try to load it as its default mode can cause unwanted traffic blocking.
	if shared.IsTrue(d.config["security.ipv6_filtering"]) {
		sysctlPath := "net/bridge/bridge-nf-call-ip6tables"
		sysctlVal, err := util.SysctlGet(sysctlPath)
		if err != nil {
			return errors.Wrapf(err, "security.ipv6_filtering requires br_netfilter be loaded")
		}

		if sysctlVal != "1\n" {
			return fmt.Errorf("security.ipv6_filtering requires br_netfilter sysctl net.bridge.bridge-nf-call-ip6tables=1")
		}
	}

	// Remove any old network filters if non-empty oldConfig supplied as part of update.
	if oldConfig != nil && (shared.IsTrue(oldConfig["security.mac_filtering"]) || shared.IsTrue(oldConfig["security.ipv4_filtering"]) || shared.IsTrue(oldConfig["security.ipv6_filtering"])) {
		d.removeFilters(oldConfig)
	}

	// Setup network filters.
	if shared.IsTrue(d.config["security.mac_filtering"]) || shared.IsTrue(d.config["security.ipv4_filtering"]) || shared.IsTrue(d.config["security.ipv6_filtering"]) {
		err := d.setFilters()
		if err != nil {
			return err
		}
	}

	return nil
}

// removeFilters removes any network level filters defined for the instance.
func (d *nicBridged) removeFilters(m deviceConfig.Device) {
	if m["hwaddr"] == "" {
		logger.Errorf("Failed to remove network filters for %q: hwaddr not defined", d.name)
		return
	}

	if m["host_name"] == "" {
		logger.Errorf("Failed to remove network filters for %q: host_name not defined", d.name)
		return
	}

	var IPv4, IPv6 net.IP

	if m["ipv4.address"] != "" {
		IPv4 = net.ParseIP(m["ipv4.address"])
	}

	if m["ipv6.address"] != "" {
		IPv6 = net.ParseIP(m["ipv6.address"])
	}

	// If no static IPv4 assigned, try removing the filter all rule in case it was setup.
	if IPv4 == nil {
		IPv4 = net.ParseIP(firewallDrivers.FilterIPv4All)
	}

	// If no static IPv6 assigned, try removing the filter all rule in case it was setup.
	if IPv6 == nil {
		IPv6 = net.ParseIP(firewallDrivers.FilterIPv6All)
	}

	// Remove filters for static MAC and IPs (if specified above).
	// This covers the case when filtering is used with an unmanaged bridge.
	logger.Debug("Clearing instance firewall static filters", log.Ctx{"project": d.inst.Project(), "instance": d.inst.Name(), "parent": m["parent"], "dev": d.name, "host_name": m["host_name"], "hwaddr": m["hwaddr"], "ipv4": IPv4, "ipv6": IPv6})
	err := d.state.Firewall.InstanceClearBridgeFilter(d.inst.Project(), d.inst.Name(), d.name, m["parent"], m["host_name"], m["hwaddr"], IPv4, IPv6)
	if err != nil {
		logger.Errorf("Failed to remove static IP network filters for %q: %v", d.name, err)
	}

	// Read current static DHCP IP allocation configured from dnsmasq host config (if exists).
	// This covers the case when IPs are not defined in config, but have been assigned in managed DHCP.
	_, IPv4Alloc, IPv6Alloc, err := dnsmasq.DHCPStaticAllocation(m["parent"], d.inst.Project(), d.inst.Name())
	if err != nil {
		if os.IsNotExist(err) {
			return
		}

		logger.Errorf("Failed to get static IP allocations for filter removal from %q: %v", d.name, err)
		return
	}

	logger.Debug("Clearing instance firewall dynamic filters", log.Ctx{"project": d.inst.Project(), "instance": d.inst.Name(), "parent": m["parent"], "dev": d.name, "host_name": m["host_name"], "hwaddr": m["hwaddr"], "ipv4": IPv4Alloc.IP, "ipv6": IPv6Alloc.IP})
	err = d.state.Firewall.InstanceClearBridgeFilter(d.inst.Project(), d.inst.Name(), d.name, m["parent"], m["host_name"], m["hwaddr"], IPv4Alloc.IP, IPv6Alloc.IP)
	if err != nil {
		logger.Errorf("Failed to remove DHCP network assigned filters  for %q: %v", d.name, err)
	}
}

// setFilters sets up any network level filters defined for the instance.
// These are controlled by the security.mac_filtering, security.ipv4_Filtering and security.ipv6_filtering config keys.
func (d *nicBridged) setFilters() (err error) {
	if d.config["hwaddr"] == "" {
		return fmt.Errorf("Failed to set network filters: require hwaddr defined")
	}

	if d.config["host_name"] == "" {
		return fmt.Errorf("Failed to set network filters: require host_name defined")
	}

	if d.config["parent"] == "" {
		return fmt.Errorf("Failed to set network filters: require parent defined")
	}

	// Parse device config.
	mac, err := net.ParseMAC(d.config["hwaddr"])
	if err != nil {
		return errors.Wrapf(err, "Invalid hwaddr")
	}

	// Parse static IPs, relies on invalid IPs being set to nil.
	IPv4 := net.ParseIP(d.config["ipv4.address"])
	IPv6 := net.ParseIP(d.config["ipv6.address"])

	// Check if the parent is managed and load config. If parent is unmanaged continue anyway.
	// project.Default is used here as bridge networks don't suppprt projects.
	n, err := network.LoadByName(d.state, project.Default, d.config["parent"])
	if err != nil && err != db.ErrNoSuchObject {
		return err
	}

	// If parent bridge is unmanaged check that a manually specified IP is available if IP filtering enabled.
	if err == db.ErrNoSuchObject || n == nil {
		if shared.IsTrue(d.config["security.ipv4_filtering"]) && d.config["ipv4.address"] == "" {
			return fmt.Errorf("IPv4 filtering requires a manually specified ipv4.address when using an unmanaged parent bridge")
		}

		if shared.IsTrue(d.config["security.ipv6_filtering"]) && d.config["ipv6.address"] == "" {
			return fmt.Errorf("IPv6 filtering requires a manually specified ipv6.address when using an unmanaged parent bridge")
		}
	}

	// If parent bridge is managed, allocate the static IPs (if needed).
	if n != nil && (IPv4 == nil || IPv6 == nil) {
		opts := &dhcpalloc.Options{
			ProjectName: d.inst.Project(),
			HostName:    d.inst.Name(),
			HostMAC:     mac,
			Network:     n,
		}

		err = dhcpalloc.AllocateTask(opts, func(t *dhcpalloc.Transaction) error {
			if shared.IsTrue(d.config["security.ipv4_filtering"]) && IPv4 == nil {
				IPv4, err = t.AllocateIPv4()

				// If DHCP not supported, skip error, and will result in total protocol filter.
				if err != nil && err != dhcpalloc.ErrDHCPNotSupported {
					return err
				}
			}

			if shared.IsTrue(d.config["security.ipv6_filtering"]) && IPv6 == nil {
				IPv6, err = t.AllocateIPv6()

				// If DHCP not supported, skip error, and will result in total protocol filter.
				if err != nil && err != dhcpalloc.ErrDHCPNotSupported {
					return err
				}
			}

			return nil
		})
		if err != nil && err != dhcpalloc.ErrDHCPNotSupported {
			return err
		}
	}

	// If anything goes wrong, clean up so we don't leave orphaned rules.
	revert := revert.New()
	defer revert.Fail()
	revert.Add(func() { d.removeFilters(d.config) })

	// If no allocated IPv4 address for filtering and filtering enabled, then block all IPv4 traffic.
	if shared.IsTrue(d.config["security.ipv4_filtering"]) && IPv4 == nil {
		IPv4 = net.ParseIP(firewallDrivers.FilterIPv4All)
	}

	// If no allocated IPv6 address for filtering and filtering enabled, then block all IPv6 traffic.
	if shared.IsTrue(d.config["security.ipv6_filtering"]) && IPv6 == nil {
		IPv6 = net.ParseIP(firewallDrivers.FilterIPv6All)
	}

	err = d.state.Firewall.InstanceSetupBridgeFilter(d.inst.Project(), d.inst.Name(), d.name, d.config["parent"], d.config["host_name"], d.config["hwaddr"], IPv4, IPv6, n != nil)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

const (
	clearLeaseAll = iota
	clearLeaseIPv4Only
	clearLeaseIPv6Only
)

// networkClearLease clears leases from a running dnsmasq process.
func (d *nicBridged) networkClearLease(name string, network string, hwaddr string, mode int) error {
	leaseFile := shared.VarPath("networks", network, "dnsmasq.leases")

	// Check that we are in fact running a dnsmasq for the network
	if !shared.PathExists(leaseFile) {
		return nil
	}

	// Convert MAC string to bytes to avoid any case comparison issues later.
	srcMAC, err := net.ParseMAC(hwaddr)
	if err != nil {
		return err
	}

	iface, err := net.InterfaceByName(network)
	if err != nil {
		return errors.Wrapf(err, "Failed getting bridge interface state for %q", network)
	}

	// Get IPv4 and IPv6 address of interface running dnsmasq on host.
	addrs, err := iface.Addrs()
	if err != nil {
		return errors.Wrapf(err, "Failed getting bridge interface addresses for %q", network)
	}

	var dstIPv4, dstIPv6 net.IP
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			return err
		}
		if !ip.IsGlobalUnicast() {
			continue
		}
		if ip.To4() == nil {
			dstIPv6 = ip
		} else {
			dstIPv4 = ip
		}
	}

	// Iterate the dnsmasq leases file looking for matching leases for this instance to release.
	file, err := os.Open(leaseFile)
	if err != nil {
		return err
	}
	defer file.Close()

	var dstDUID string
	errs := []error{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		fieldsLen := len(fields)

		// Handle lease lines
		if fieldsLen == 5 {
			if (mode == clearLeaseAll || mode == clearLeaseIPv4Only) && srcMAC.String() == fields[1] { // Handle IPv4 leases by matching MAC address to lease.
				srcIP := net.ParseIP(fields[2])

				if dstIPv4 == nil {
					logger.Warnf("Failed to release DHCPv4 lease for instance %q, IP %q, MAC %q, %v", name, srcIP, srcMAC, "No server address found")
					continue // Cant send release packet if no dstIP found.
				}

				err = d.networkDHCPv4Release(srcMAC, srcIP, dstIPv4)
				if err != nil {
					errs = append(errs, fmt.Errorf("Failed to release DHCPv4 lease for instance %q, IP %q, MAC %q, %v", name, srcIP, srcMAC, err))
				}
			} else if (mode == clearLeaseAll || mode == clearLeaseIPv6Only) && name == fields[3] { // Handle IPv6 addresses by matching hostname to lease.
				IAID := fields[1]
				srcIP := net.ParseIP(fields[2])
				DUID := fields[4]

				// Skip IPv4 addresses.
				if srcIP.To4() != nil {
					continue
				}

				if dstIPv6 == nil {
					logger.Warn("Failed to release DHCPv6 lease for instance %q, IP %q, DUID %q, IAID %q: %q", name, srcIP, DUID, IAID, "No server address found")
					continue // Cant send release packet if no dstIP found.
				}

				if dstDUID == "" {
					errs = append(errs, fmt.Errorf("Failed to release DHCPv6 lease for instance %q, IP %q, DUID %q, IAID %q: %s", name, srcIP, DUID, IAID, "No server DUID found"))
					continue // Cant send release packet if no dstDUID found.
				}

				err = d.networkDHCPv6Release(DUID, IAID, srcIP, dstIPv6, dstDUID)
				if err != nil {
					errs = append(errs, fmt.Errorf("Failed to release DHCPv6 lease for instance %q, IP %q, DUID %q, IAID %q: %v", name, srcIP, DUID, IAID, err))
				}
			}
		} else if fieldsLen == 2 && fields[0] == "duid" {
			// Handle server DUID line needed for releasing IPv6 leases.
			// This should come before the IPv6 leases in the lease file.
			dstDUID = fields[1]
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%v", errs)
	}

	err = scanner.Err()
	if err != nil {
		return err
	}

	return nil
}

// networkDHCPv4Release sends a DHCPv4 release packet to a DHCP server.
func (d *nicBridged) networkDHCPv4Release(srcMAC net.HardwareAddr, srcIP net.IP, dstIP net.IP) error {
	dstAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:67", dstIP.String()))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, dstAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	//Random DHCP transaction ID
	xid := rand.Uint32()

	// Construct a DHCP packet pretending to be from the source IP and MAC supplied.
	dhcp := layers.DHCPv4{
		Operation:    layers.DHCPOpRequest,
		HardwareType: layers.LinkTypeEthernet,
		ClientHWAddr: srcMAC,
		ClientIP:     srcIP,
		Xid:          xid,
	}

	// Add options to DHCP release packet.
	dhcp.Options = append(dhcp.Options,
		layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(layers.DHCPMsgTypeRelease)}),
		layers.NewDHCPOption(layers.DHCPOptServerID, dstIP.To4()),
	)

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	err = gopacket.SerializeLayers(buf, opts, &dhcp)
	if err != nil {
		return err
	}

	_, err = conn.Write(buf.Bytes())
	return err
}

// networkDHCPv6Release sends a DHCPv6 release packet to a DHCP server.
func (d *nicBridged) networkDHCPv6Release(srcDUID string, srcIAID string, srcIP net.IP, dstIP net.IP, dstDUID string) error {
	dstAddr, err := net.ResolveUDPAddr("udp6", fmt.Sprintf("[%s]:547", dstIP.String()))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp6", nil, dstAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Construct a DHCPv6 packet pretending to be from the source IP and MAC supplied.
	dhcp := layers.DHCPv6{
		MsgType: layers.DHCPv6MsgTypeRelease,
	}

	// Convert Server DUID from string to byte array
	dstDUIDRaw, err := hex.DecodeString(strings.Replace(dstDUID, ":", "", -1))
	if err != nil {
		return err
	}

	// Convert DUID from string to byte array
	srcDUIDRaw, err := hex.DecodeString(strings.Replace(srcDUID, ":", "", -1))
	if err != nil {
		return err
	}

	// Convert IAID string to int
	srcIAIDRaw, err := strconv.ParseUint(srcIAID, 10, 32)
	if err != nil {
		return err
	}
	srcIAIDRaw32 := uint32(srcIAIDRaw)

	// Build the Identity Association details option manually (as not provided by gopacket).
	iaAddr := d.networkDHCPv6CreateIAAddress(srcIP)
	ianaRaw := d.networkDHCPv6CreateIANA(srcIAIDRaw32, iaAddr)

	// Add options to DHCP release packet.
	dhcp.Options = append(dhcp.Options,
		layers.NewDHCPv6Option(layers.DHCPv6OptServerID, dstDUIDRaw),
		layers.NewDHCPv6Option(layers.DHCPv6OptClientID, srcDUIDRaw),
		layers.NewDHCPv6Option(layers.DHCPv6OptIANA, ianaRaw),
	)

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	err = gopacket.SerializeLayers(buf, opts, &dhcp)
	if err != nil {
		return err
	}

	_, err = conn.Write(buf.Bytes())
	return err
}

// networkDHCPv6CreateIANA creates a DHCPv6 Identity Association for Non-temporary Address (rfc3315 IA_NA) option.
func (d *nicBridged) networkDHCPv6CreateIANA(IAID uint32, IAAddr []byte) []byte {
	data := make([]byte, 12)
	binary.BigEndian.PutUint32(data[0:4], IAID)       // Identity Association Identifier
	binary.BigEndian.PutUint32(data[4:8], uint32(0))  // T1
	binary.BigEndian.PutUint32(data[8:12], uint32(0)) // T2
	data = append(data, IAAddr...)                    // Append the IA Address details
	return data
}

// networkDHCPv6CreateIAAddress creates a DHCPv6 Identity Association Address (rfc3315) option.
func (d *nicBridged) networkDHCPv6CreateIAAddress(IP net.IP) []byte {
	data := make([]byte, 28)
	binary.BigEndian.PutUint16(data[0:2], uint16(layers.DHCPv6OptIAAddr)) // Sub-Option type
	binary.BigEndian.PutUint16(data[2:4], uint16(24))                     // Length (fixed at 24 bytes)
	copy(data[4:20], IP)                                                  // IPv6 address to be released
	binary.BigEndian.PutUint32(data[20:24], uint32(0))                    // Preferred liftetime
	binary.BigEndian.PutUint32(data[24:28], uint32(0))                    // Valid lifetime
	return data
}

// setupNativeBridgePortVLANs configures the bridge port with the specified VLAN settings on the native bridge.
func (d *nicBridged) setupNativeBridgePortVLANs(hostName string) error {
	link := &ip.Link{Name: hostName}

	// Check vlan_filtering is enabled on bridge if needed.
	if d.config["vlan"] != "" || d.config["vlan.tagged"] != "" {
		vlanFilteringStatus, err := network.BridgeVLANFilteringStatus(d.config["parent"])
		if err != nil {
			return err
		}

		if vlanFilteringStatus != "1" {
			return fmt.Errorf("VLAN filtering is not enabled in parent bridge %q", d.config["parent"])
		}
	}

	// Set port on bridge to specified untagged PVID.
	if d.config["vlan"] != "" {
		// Reject VLAN ID 0 if specified (as validation allows VLAN ID 0 on unmanaged bridges for OVS).
		if d.config["vlan"] == "0" {
			return fmt.Errorf("VLAN ID 0 is not allowed for native Linux bridges")
		}

		// Get default PVID membership on port.
		defaultPVID, err := network.BridgeVLANDefaultPVID(d.config["parent"])
		if err != nil {
			return err
		}

		// If the default is different to the specified untagged VLAN or if tagged VLAN is set to "none"
		// then remove the default untagged membership.
		if defaultPVID != d.config["vlan"] || d.config["vlan"] == "none" {
			err = link.BridgeVLANDelete(defaultPVID, false, false)
			if err != nil {
				return err
			}
		}

		// Configure the untagged membership settings of the port if VLAN ID specified.
		if d.config["vlan"] != "none" {
			err = link.BridgeVLANAdd(d.config["vlan"], true, true, false, true)
			if err != nil {
				return err
			}
		}
	}

	// Add any tagged VLAN memberships.
	if d.config["vlan.tagged"] != "" {
		for _, vlanID := range util.SplitNTrimSpace(d.config["vlan.tagged"], ",", -1, true) {
			// Reject VLAN ID 0 if specified (as validation allows VLAN ID 0 on unmanaged bridges for OVS).
			if vlanID == "0" {
				return fmt.Errorf("VLAN tagged ID 0 is not allowed for native Linux bridges")
			}

			err := link.BridgeVLANAdd(vlanID, false, false, false, false)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// setupOVSBridgePortVLANs configures the bridge port with the specified VLAN settings on the openvswitch bridge.
func (d *nicBridged) setupOVSBridgePortVLANs(hostName string) error {
	ovs := openvswitch.NewOVS()

	// Set port on bridge to specified untagged PVID.
	if d.config["vlan"] != "" {
		if d.config["vlan"] == "none" && d.config["vlan.tagged"] == "" {
			return fmt.Errorf("vlan=none is not supported with openvswitch bridges when not using vlan.tagged")
		}

		// Configure the untagged 'native' membership settings of the port if VLAN ID specified.
		// Also set the vlan_mode=access, which will drop any tagged frames.
		// Order is important here, as vlan_mode is set to "access", assuming that vlan.tagged is not used.
		// If vlan.tagged is specified, then we expect it to also change the vlan_mode as needed.
		if d.config["vlan"] != "none" {
			err := ovs.BridgePortSet(hostName, "vlan_mode=access", fmt.Sprintf("tag=%s", d.config["vlan"]))
			if err != nil {
				return err
			}
		}
	}

	// Add any tagged VLAN memberships.
	if d.config["vlan.tagged"] != "" {
		vlanIDs := util.SplitNTrimSpace(d.config["vlan.tagged"], ",", -1, true)
		vlanMode := "trunk" // Default to only allowing tagged frames (drop untagged frames).
		if d.config["vlan"] != "none" {
			// If untagged vlan mode isn't "none" then allow untagged frames for port's 'native' VLAN.
			vlanMode = "native-untagged"
		}

		// Configure the tagged membership settings of the port if VLAN ID specified.
		// Also set the vlan_mode as needed from above.
		// Must come after the PortSet command used for setting "vlan" mode above so that the correct
		// vlan_mode is retained.
		err := ovs.BridgePortSet(hostName, fmt.Sprintf("vlan_mode=%s", vlanMode), fmt.Sprintf("trunks=%s", strings.Join(vlanIDs, ",")))
		if err != nil {
			return err
		}
	}

	return nil
}

// State gets the state of a bridged NIC by parsing the local DHCP server leases file.
func (d *nicBridged) State() (*api.InstanceStateNetwork, error) {
	v := d.volatileGet()

	// Populate device config with volatile fields if needed.
	networkVethFillFromVolatile(d.config, v)

	ips := []net.IP{}
	var v4mask string
	var v6mask string

	// ipStore appends an IP to ips if not already stored.
	ipStore := func(newIP net.IP) {
		for _, ip := range ips {
			if ip.Equal(newIP) {
				return
			}
		}

		ips = append(ips, newIP)
	}

	// Check if parent is managed network and load config.
	// Pass project.Default here, as currently dnsmasq (bridged) networks do not support projects.
	n, err := network.LoadByName(d.state, project.Default, d.config["parent"])
	if err == nil {
		// Extract subnet sizes from bridge addresses if available.
		netConfig := n.Config()
		_, v4subnet, _ := net.ParseCIDR(netConfig["ipv4.address"])
		_, v6subnet, _ := net.ParseCIDR(netConfig["ipv6.address"])

		if v4subnet != nil {
			mask, _ := v4subnet.Mask.Size()
			v4mask = fmt.Sprintf("%d", mask)
		}

		if v6subnet != nil {
			mask, _ := v6subnet.Mask.Size()
			v6mask = fmt.Sprintf("%d", mask)
		}

		if d.config["hwaddr"] != "" {
			// Parse the leases file if parent network is managed.
			leaseIPs, err := network.GetLeaseAddresses(n.Name(), d.config["hwaddr"])
			if err == nil {
				for _, leaseIP := range leaseIPs {
					ipStore(leaseIP)
				}
			}

			if !shared.IsTrue(n.Config()["ipv6.dhcp.stateful"]) && v6subnet != nil {
				// If stateful DHCPv6 is disabled, and IPv6 is enabled on the bridge, the the NIC
				// is likely to use its MAC and SLAAC to configure its address.
				hwAddr, err := net.ParseMAC(d.config["hwaddr"])
				if err == nil {
					ip, err := eui64.ParseMAC(v6subnet.IP, hwAddr)
					if err == nil {
						ipStore(ip)
					}
				}
			}
		}
	}

	// Get IP addresses from IP neighbour cache if present.
	neighIPs, err := network.GetNeighbourIPs(d.config["parent"], d.config["hwaddr"])
	if err == nil {
		validStates := []string{
			string(network.NeighbourIPStatePermanent),
			string(network.NeighbourIPStateNoARP),
			string(network.NeighbourIPStateReachable),
		}

		// Add any valid-state neighbour IP entries first.
		for _, neighIP := range neighIPs {
			if shared.StringInSlice(string(neighIP.State), validStates) {
				ipStore(neighIP.IP)
			}
		}

		// Add any non-failed-state entries.
		for _, neighIP := range neighIPs {
			if neighIP.State != network.NeighbourIPStateFailed && !shared.StringInSlice(string(neighIP.State), validStates) {
				ipStore(neighIP.IP)
			}
		}
	}

	// Convert IPs to InstanceStateNetworkAddresses.
	addresses := []api.InstanceStateNetworkAddress{}
	for _, ip := range ips {
		addr := api.InstanceStateNetworkAddress{}
		addr.Address = ip.String()
		addr.Family = "inet"
		addr.Netmask = v4mask

		if ip.To4() == nil {
			addr.Family = "inet6"
			addr.Netmask = v6mask
		}

		if ip.IsLinkLocalUnicast() {
			addr.Scope = "link"

			if addr.Family == "inet6" {
				addr.Netmask = "64" // Link-local IPv6 addresses are /64.
			} else {
				addr.Netmask = "16" // Link-local IPv4 addresses are /16.
			}
		} else {
			addr.Scope = "global"
		}

		addresses = append(addresses, addr)
	}

	// Get MTU of host interface if exists.
	iface, err := net.InterfaceByName(d.config["host_name"])
	if err != nil {
		d.logger.Warn("Failed getting host interface state for MTU", log.Ctx{"host_name": d.config["host_name"], "err": err})
	}

	mtu := -1
	if iface != nil {
		mtu = iface.MTU
	}

	// Retrieve the host counters, as we report the values from the instance's point of view,
	// those counters need to be reversed below.
	hostCounters, err := resources.GetNetworkCounters(d.config["host_name"])
	if err != nil {
		return nil, errors.Wrapf(err, "Failed getting network interface counters")
	}

	network := api.InstanceStateNetwork{
		Addresses: addresses,
		Counters: api.InstanceStateNetworkCounters{
			BytesReceived:   hostCounters.BytesSent,
			BytesSent:       hostCounters.BytesReceived,
			PacketsReceived: hostCounters.PacketsSent,
			PacketsSent:     hostCounters.PacketsReceived,
		},
		Hwaddr:   d.config["hwaddr"],
		HostName: d.config["host_name"],
		Mtu:      mtu,
		State:    "up",
		Type:     "broadcast",
	}

	return &network, nil
}
