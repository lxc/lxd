package network

import (
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/netx/eui64"
	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/db"
	dbCluster "github.com/lxc/lxd/lxd/db/cluster"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/locking"
	"github.com/lxc/lxd/lxd/network/acl"
	"github.com/lxc/lxd/lxd/network/openvswitch"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/lxd/warnings"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/validate"
)

const ovnChassisPriorityMax = 32767
const ovnVolatileUplinkIPv4 = "volatile.network.ipv4.address"
const ovnVolatileUplinkIPv6 = "volatile.network.ipv6.address"

// ovnUplinkVars OVN object variables derived from uplink network.
type ovnUplinkVars struct {
	// Router.
	routerExtPortIPv4Net string
	routerExtPortIPv6Net string
	routerExtGwIPv4      net.IP
	routerExtGwIPv6      net.IP

	// External Switch.
	extSwitchProviderName string

	// DNS.
	dnsIPv6 []net.IP
	dnsIPv4 []net.IP
}

// ovnUplinkPortBridgeVars uplink bridge port variables used for start/stop.
type ovnUplinkPortBridgeVars struct {
	ovsBridge string
	uplinkEnd string
	ovsEnd    string
}

// OVNInstanceNICSetupOpts options for starting an OVN Instance NIC.
type OVNInstanceNICSetupOpts struct {
	InstanceUUID string
	DeviceName   string
	DeviceConfig deviceConfig.Device
	UplinkConfig map[string]string
	DNSName      string
}

// OVNInstanceNICStopOpts options for stopping an OVN Instance NIC.
type OVNInstanceNICStopOpts struct {
	InstanceUUID string
	DeviceName   string
	DeviceConfig deviceConfig.Device
}

// ovn represents a LXD OVN network.
type ovn struct {
	common
}

// Type returns the network type.
func (n *ovn) Type() string {
	return "ovn"
}

// DBType returns the network type DB ID.
func (n *ovn) DBType() db.NetworkType {
	return db.NetworkTypeOVN
}

// Config returns the network driver info.
func (n *ovn) Info() Info {
	return Info{
		Projects:           true,
		NodeSpecificConfig: false,
	}
}

// uplinkRoutes parses ipv4.routes and ipv6.routes settings for an uplink network into a slice of *net.IPNet.
func (n *ovn) uplinkRoutes(uplink *api.Network) ([]*net.IPNet, error) {
	var err error
	var uplinkRoutes []*net.IPNet
	for _, k := range []string{"ipv4.routes", "ipv6.routes"} {
		if uplink.Config[k] == "" {
			continue
		}

		uplinkRoutes, err = SubnetParseAppend(uplinkRoutes, util.SplitNTrimSpace(uplink.Config[k], ",", -1, false)...)
		if err != nil {
			return nil, err
		}
	}

	return uplinkRoutes, nil
}

// projectRestrictedSubnets parses the restrict.networks.subnets project setting and returns slice of *net.IPNet.
// Returns nil slice if no project restrictions, or empty slice if no allowed subnets.
func (n *ovn) projectRestrictedSubnets(p *db.Project, uplinkNetworkName string) ([]*net.IPNet, error) {
	// Parse project's restricted subnets.
	var projectRestrictedSubnets []*net.IPNet // Nil value indicates not restricted.
	if shared.IsTrue(p.Config["restricted"]) && p.Config["restricted.networks.subnets"] != "" {
		projectRestrictedSubnets = []*net.IPNet{} // Empty slice indicates no allowed subnets.

		for _, subnetRaw := range util.SplitNTrimSpace(p.Config["restricted.networks.subnets"], ",", -1, false) {
			subnetParts := strings.SplitN(subnetRaw, ":", 2)
			if len(subnetParts) != 2 {
				return nil, fmt.Errorf(`Project subnet %q invalid, must be in the format of "<uplink network>:<subnet>"`, subnetRaw)
			}

			subnetUplinkName := subnetParts[0]
			subnetStr := subnetParts[1]

			if subnetUplinkName != uplinkNetworkName {
				continue // Only include subnets for our uplink.
			}

			_, restrictedSubnet, err := net.ParseCIDR(subnetStr)
			if err != nil {
				return nil, err
			}

			projectRestrictedSubnets = append(projectRestrictedSubnets, restrictedSubnet)
		}
	}

	return projectRestrictedSubnets, nil
}

// validateExternalSubnet checks the supplied ipNet is allowed within the uplink routes and project
// restricted subnets. If projectRestrictedSubnets is nil, then it is not checked as this indicates project has
// no restrictions. Whereas if uplinkRoutes is nil/empty then this will always return an error.
func (n *ovn) validateExternalSubnet(uplinkRoutes []*net.IPNet, projectRestrictedSubnets []*net.IPNet, ipNet *net.IPNet) error {
	// Check that the IP network is within the project's restricted subnets if restricted.
	if projectRestrictedSubnets != nil {
		foundMatch := false
		for _, projectRestrictedSubnet := range projectRestrictedSubnets {
			if SubnetContains(projectRestrictedSubnet, ipNet) {
				foundMatch = true
				break
			}
		}

		if !foundMatch {
			return fmt.Errorf("Project doesn't contain %q in its restricted uplink subnets", ipNet.String())
		}
	}

	// Check that the IP network is within the uplink network's routes.
	foundMatch := false
	for _, uplinkRoute := range uplinkRoutes {
		if SubnetContains(uplinkRoute, ipNet) {
			foundMatch = true
			break
		}
	}

	if !foundMatch {
		return fmt.Errorf("Uplink network doesn't contain %q in its routes", ipNet.String())
	}

	return nil
}

// Validate network config.
func (n *ovn) Validate(config map[string]string) error {
	rules := map[string]func(value string) error{
		"network":       validate.IsAny,
		"bridge.hwaddr": validate.Optional(validate.IsNetworkMAC),
		"bridge.mtu":    validate.Optional(validate.IsNetworkMTU),
		"ipv4.address": validate.Optional(func(value string) error {
			if validate.IsOneOf(value, []string{"none", "auto"}) == nil {
				return nil
			}

			return validate.IsNetworkAddressCIDRV4(value)
		}),
		"ipv4.dhcp": validate.Optional(validate.IsBool),
		"ipv6.address": validate.Optional(func(value string) error {
			if validate.IsOneOf(value, []string{"none", "auto"}) == nil {
				return nil
			}

			return validate.IsNetworkAddressCIDRV6(value)
		}),
		"ipv6.dhcp":          validate.Optional(validate.IsBool),
		"ipv6.dhcp.stateful": validate.Optional(validate.IsBool),
		"ipv4.nat":           validate.Optional(validate.IsBool),
		"ipv6.nat":           validate.Optional(validate.IsBool),
		"dns.domain":         validate.IsAny,
		"dns.search":         validate.IsAny,
		"security.acls":      validate.IsAny,
		"security.acls.default.ingress.action": validate.Optional(func(value string) error {
			return validate.IsOneOf(value, acl.ValidActions)
		}),
		"security.acls.default.egress.action": validate.Optional(func(value string) error {
			return validate.IsOneOf(value, acl.ValidActions)
		}),
		"security.acls.default.ingress.logged": validate.Optional(validate.IsBool),
		"security.acls.default.egress.logged":  validate.Optional(validate.IsBool),

		// Volatile keys populated automatically as needed.
		ovnVolatileUplinkIPv4: validate.Optional(validate.IsNetworkAddressV4),
		ovnVolatileUplinkIPv6: validate.Optional(validate.IsNetworkAddressV6),
	}

	err := n.validate(config, rules)
	if err != nil {
		return err
	}

	// Check that if IPv6 enabled then the network size must be at least a /64 as both RA and DHCPv6
	// in OVN (as it generates addresses using EUI64) require at least a /64 subnet to operate.
	_, ipv6Net, _ := net.ParseCIDR(config["ipv6.address"])
	if ipv6Net != nil {
		ones, _ := ipv6Net.Mask.Size()
		if ones < 64 {
			return fmt.Errorf("IPv6 subnet must be at least a /64")
		}
	}

	// Load the project to get uplink network restrictions.
	p, err := n.state.Cluster.GetProject(n.project)
	if err != nil {
		return errors.Wrapf(err, "Failed to load network restrictions from project %q", n.project)
	}

	// Check uplink network is valid and allowed in project.
	uplinkNetworkName, err := n.validateUplinkNetwork(p, config["network"])
	if err != nil {
		return err
	}

	// Get uplink routes.
	_, uplink, _, err := n.state.Cluster.GetNetworkInAnyState(project.Default, uplinkNetworkName)
	if err != nil {
		return errors.Wrapf(err, "Failed to load uplink network %q", uplinkNetworkName)
	}

	uplinkRoutes, err := n.uplinkRoutes(uplink)
	if err != nil {
		return err
	}

	// Get project restricted routes.
	projectRestrictedSubnets, err := n.projectRestrictedSubnets(p, uplinkNetworkName)
	if err != nil {
		return err
	}

	// If NAT disabled, parse the external subnets that are being requested.
	var externalSubnets []*net.IPNet
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		addressKey := fmt.Sprintf("%s.address", keyPrefix)
		if !shared.IsTrue(config[fmt.Sprintf("%s.nat", keyPrefix)]) && validate.IsOneOf(config[addressKey], []string{"", "none", "auto"}) != nil {
			_, ipNet, err := net.ParseCIDR(config[addressKey])
			if err != nil {
				return errors.Wrapf(err, "Failed parsing %s", addressKey)
			}

			externalSubnets = append(externalSubnets, ipNet)
		}
	}

	if len(externalSubnets) > 0 {
		var projectNetworks map[string]map[int64]api.Network
		err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
			// Get all managed networks across all projects.
			projectNetworks, err = tx.GetCreatedNetworks()
			if err != nil {
				return errors.Wrapf(err, "Failed to load all networks")
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Get OVN networks that use the same uplink as us.
		ovnProjectNetworksWithOurUplink := n.ovnProjectNetworksWithUplink(config["network"], projectNetworks)

		// Get external subnets used by other OVN networks using our uplink.
		ovnNetworkExternalSubnets, err := n.ovnNetworkExternalSubnets(n.project, n.name, ovnProjectNetworksWithOurUplink, uplinkRoutes)
		if err != nil {
			return err
		}

		// Get external routes configured on OVN NICs using networks that use our uplink.
		ovnNICExternalRoutes, err := n.ovnNICExternalRoutes(nil, "", ovnProjectNetworksWithOurUplink)
		if err != nil {
			return err
		}

		// Check if uplink has routed ingress anycast mode enabled, as this relaxes the overlap checks.
		ipv4UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv4(uplink)
		ipv6UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv6(uplink)

		for _, externalSubnet := range externalSubnets {
			// Check the external subnet is allowed within both the uplink's external routes and any
			// project restricted subnets.
			err = n.validateExternalSubnet(uplinkRoutes, projectRestrictedSubnets, externalSubnet)
			if err != nil {
				return err
			}

			// Skip overlap checks if external subnet's protocol has anycast mode enabled on uplink.
			if externalSubnet.IP.To4() == nil {
				if ipv6UplinkAnycast == true {
					continue
				}
			} else if ipv4UplinkAnycast == true {
				continue
			}

			// Check the external subnet doesn't fall within any existing OVN network external subnets.
			for _, ovnNetworkExternalSubnet := range ovnNetworkExternalSubnets {
				if SubnetContains(ovnNetworkExternalSubnet, externalSubnet) || SubnetContains(externalSubnet, ovnNetworkExternalSubnet) {
					// This error is purposefully vague so that it doesn't reveal any names of
					// resources potentially outside of the network's project.
					return fmt.Errorf("External subnet %q overlaps with another OVN network's external subnet", externalSubnet.String())
				}
			}

			// Check the external subnet doesn't fall within any existing OVN NIC external routes.
			for _, ovnNICExternalRoute := range ovnNICExternalRoutes {
				if SubnetContains(ovnNICExternalRoute, externalSubnet) || SubnetContains(externalSubnet, ovnNICExternalRoute) {
					// This error is purposefully vague so that it doesn't reveal any names of
					// resources potentially outside of the networks's project.
					return fmt.Errorf("External subnet %q overlaps with another OVN NIC's external route", externalSubnet.String())
				}
			}
		}
	}

	// Check Security ACLs exist.
	if config["security.acls"] != "" {
		err = acl.Exists(n.state, n.project, util.SplitNTrimSpace(config["security.acls"], ",", -1, true)...)
		if err != nil {
			return err
		}
	}

	return nil
}

// getBridgeMTU returns MTU that should be used for the bridge and instance devices.
// Will also be used to configure the OVN DHCP and IPv6 RA options. Returns 0 if the bridge.mtu is not set/invalid.
func (n *ovn) getBridgeMTU() uint32 {
	if n.config["bridge.mtu"] != "" {
		mtu, err := strconv.ParseUint(n.config["bridge.mtu"], 10, 32)
		if err != nil {
			return 0
		}

		return uint32(mtu)
	}

	return 0
}

// getUnderlayInfo returns the MTU for the underlay network interface and the enscapsulation IP for OVN tunnels.
func (n *ovn) getUnderlayInfo() (uint32, net.IP, error) {
	// findMTUFromIP searches all interfaces on the host looking for one that has specified IP.
	findMTUFromIP := func(findIP net.IP) (uint32, error) {
		// Look for interface that has the OVN enscapsulation IP assigned.
		ifaces, err := net.Interfaces()
		if err != nil {
			return 0, errors.Wrapf(err, "Failed getting local network interfaces")
		}

		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}

			for _, addr := range addrs {
				ip, _, err := net.ParseCIDR(addr.String())
				if err != nil {
					continue
				}

				if ip.Equal(findIP) {
					underlayMTU, err := GetDevMTU(iface.Name)
					if err != nil {
						return 0, errors.Wrapf(err, "Failed getting MTU for %q", iface.Name)
					}

					return underlayMTU, nil // Found what we were looking for.
				}
			}
		}

		return 0, fmt.Errorf("No matching interface found for OVN enscapsulation IP %q", findIP.String())
	}

	ovs := openvswitch.NewOVS()
	encapIP, err := ovs.OVNEncapIP()
	if err != nil {
		return 0, nil, errors.Wrapf(err, "Failed getting OVN enscapsulation IP from OVS")
	}

	underlayMTU, err := findMTUFromIP(encapIP)
	if err != nil {
		return 0, nil, err
	}

	return underlayMTU, encapIP, nil
}

// getOptimalBridgeMTU returns the MTU that can be used for the bridge and instance devices based on the MTU value
// of the OVN underlay network interface. This assumes that the OVN tunnel mechanism used is geneve and that the
// same underlying network settings (MTU and encapsulation IP family) are used on all OVN nodes.
func (n *ovn) getOptimalBridgeMTU() (uint32, error) {
	// Get underlay MTU and encapsulation IP.
	underlayMTU, encapIP, err := n.getUnderlayInfo()
	if err != nil {
		return 0, errors.Wrapf(err, "Failed getting OVN underlay info")
	}

	// Encapsulation family is IPv6.
	if encapIP.To4() == nil {
		// If the underlay's MTU is large enough to accommodate a 1500 overlay MTU and the geneve tunnel
		// overhead of 78 bytes (when used with IPv6 encapsulation) then indicate 1500 MTU can be used.
		if underlayMTU >= 1578 {
			return 1500, nil
		}

		// Default to 1422 which can work with an underlay MTU of 1500.
		return 1422, nil
	}

	// If the underlay's MTU is large enough to accommodate a 1500 overlay MTU and the geneve tunnel
	// overhead of 58 bytes (when used with IPv4 encapsulation) then indicate 1500 MTU can be used.
	if underlayMTU >= 1558 {
		return 1500, nil
	}

	// Default to 1442 which can work with underlay MTU of 1500.
	return 1442, nil
}

// getNetworkPrefix returns OVN network prefix to use for object names.
func (n *ovn) getNetworkPrefix() string {
	return acl.OVNNetworkPrefix(n.id)
}

// getChassisGroup returns OVN chassis group name to use.
func (n *ovn) getChassisGroupName() openvswitch.OVNChassisGroup {
	return openvswitch.OVNChassisGroup(n.getNetworkPrefix())
}

// getRouterName returns OVN logical router name to use.
func (n *ovn) getRouterName() openvswitch.OVNRouter {
	return openvswitch.OVNRouter(fmt.Sprintf("%s-lr", n.getNetworkPrefix()))
}

// getRouterExtPortName returns OVN logical router external port name to use.
func (n *ovn) getRouterExtPortName() openvswitch.OVNRouterPort {
	return openvswitch.OVNRouterPort(fmt.Sprintf("%s-lrp-ext", n.getRouterName()))
}

// getRouterIntPortName returns OVN logical router internal port name to use.
func (n *ovn) getRouterIntPortName() openvswitch.OVNRouterPort {
	return openvswitch.OVNRouterPort(fmt.Sprintf("%s-lrp-int", n.getRouterName()))
}

// getRouterMAC returns OVN router MAC address to use for ports. Uses a stable seed to return stable random MAC.
func (n *ovn) getRouterMAC() (net.HardwareAddr, error) {
	hwAddr := n.config["bridge.hwaddr"]
	if hwAddr == "" {
		// Load server certificate. This is needs to be the same certificate for all nodes in a cluster.
		cert, err := util.LoadCert(n.state.OS.VarDir)
		if err != nil {
			return nil, err
		}

		// Generate the random seed, this uses the server certificate fingerprint (to ensure that multiple
		// standalone nodes on the same external network don't generate the same MAC for their networks).
		// It relies on the certificate being the same for all nodes in a cluster to allow the same MAC to
		// be generated on each bridge interface in the network.
		seed := fmt.Sprintf("%s.%d.%d", cert.Fingerprint(), 0, n.ID())
		r, err := util.GetStableRandomGenerator(seed)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed generating stable random router MAC")
		}

		hwAddr = randomHwaddr(r)
		n.logger.Debug("Stable MAC generated", log.Ctx{"seed": seed, "hwAddr": hwAddr})
	}

	mac, err := net.ParseMAC(hwAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed parsing router MAC address %q", mac)
	}

	return mac, nil
}

// getRouterIntPortIPv4Net returns OVN logical router internal port IPv4 address and subnet.
func (n *ovn) getRouterIntPortIPv4Net() string {
	return n.config["ipv4.address"]
}

// getRouterIntPortIPv4Net returns OVN logical router internal port IPv6 address and subnet.
func (n *ovn) getRouterIntPortIPv6Net() string {
	return n.config["ipv6.address"]
}

// getDomainName returns OVN DHCP domain name.
func (n *ovn) getDomainName() string {
	if n.config["dns.domain"] != "" {
		return n.config["dns.domain"]
	}

	return "lxd"
}

// getDNSSearchList returns OVN DHCP DNS search list. If no search list set returns getDomainName() as list.
func (n *ovn) getDNSSearchList() []string {
	if n.config["dns.search"] != "" {
		return util.SplitNTrimSpace(n.config["dns.search"], ",", -1, false)
	}

	return []string{n.getDomainName()}
}

// getExtSwitchName returns OVN  logical external switch name.
func (n *ovn) getExtSwitchName() openvswitch.OVNSwitch {
	return openvswitch.OVNSwitch(fmt.Sprintf("%s-ls-ext", n.getNetworkPrefix()))
}

// getExtSwitchRouterPortName returns OVN logical external switch router port name.
func (n *ovn) getExtSwitchRouterPortName() openvswitch.OVNSwitchPort {
	return openvswitch.OVNSwitchPort(fmt.Sprintf("%s-lsp-router", n.getExtSwitchName()))
}

// getExtSwitchProviderPortName returns OVN logical external switch provider port name.
func (n *ovn) getExtSwitchProviderPortName() openvswitch.OVNSwitchPort {
	return openvswitch.OVNSwitchPort(fmt.Sprintf("%s-lsp-provider", n.getExtSwitchName()))
}

// getIntSwitchName returns OVN logical internal switch name.
func (n *ovn) getIntSwitchName() openvswitch.OVNSwitch {
	return acl.OVNIntSwitchName(n.id)
}

// getIntSwitchRouterPortName returns OVN logical internal switch router port name.
func (n *ovn) getIntSwitchRouterPortName() openvswitch.OVNSwitchPort {
	return acl.OVNIntSwitchRouterPortName(n.id)
}

// getIntSwitchInstancePortPrefix returns OVN logical internal switch instance port name prefix.
func (n *ovn) getIntSwitchInstancePortPrefix() string {
	return fmt.Sprintf("%s-instance", n.getNetworkPrefix())
}

// setupUplinkPort initialises the uplink connection. Returns the derived ovnUplinkVars settings used
// during the initial creation of the logical network.
func (n *ovn) setupUplinkPort(routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	// Uplink network must be in default project.
	uplinkNet, err := LoadByName(n.state, project.Default, n.config["network"])
	if err != nil {
		return nil, errors.Wrapf(err, "Failed loading uplink network %q", n.config["network"])
	}

	switch uplinkNet.Type() {
	case "bridge":
		return n.setupUplinkPortBridge(uplinkNet, routerMAC)
	case "physical":
		return n.setupUplinkPortPhysical(uplinkNet, routerMAC)
	}

	return nil, fmt.Errorf("Failed setting up uplink port, network type %q unsupported as OVN uplink", uplinkNet.Type())
}

// setupUplinkPortBridge allocates external IPs on the uplink bridge.
// Returns the derived ovnUplinkVars settings.
func (n *ovn) setupUplinkPortBridge(uplinkNet Network, routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	bridgeNet, ok := uplinkNet.(*bridge)
	if !ok {
		return nil, fmt.Errorf("Network is not bridge type")
	}

	err := bridgeNet.checkClusterWideMACSafe(bridgeNet.config)
	if err != nil {
		return nil, errors.Wrapf(err, "Network %q is not suitable for use as OVN uplink", bridgeNet.name)
	}

	v, err := n.allocateUplinkPortIPs(uplinkNet, routerMAC)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed allocating uplink port IPs on network %q", uplinkNet.Name())
	}

	return v, nil
}

// setupUplinkPortPhysical allocates external IPs on the uplink network.
// Returns the derived ovnUplinkVars settings.
func (n *ovn) setupUplinkPortPhysical(uplinkNet Network, routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	v, err := n.allocateUplinkPortIPs(uplinkNet, routerMAC)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed allocating uplink port IPs on network %q", uplinkNet.Name())
	}

	return v, nil
}

// allocateUplinkPortIPs attempts to find a free IP in the uplink network's OVN ranges and then stores it in
// ovnVolatileUplinkIPv4 and ovnVolatileUplinkIPv6 config keys on this network. Returns ovnUplinkVars settings.
func (n *ovn) allocateUplinkPortIPs(uplinkNet Network, routerMAC net.HardwareAddr) (*ovnUplinkVars, error) {
	v := &ovnUplinkVars{}

	uplinkNetConf := uplinkNet.Config()

	// Uplink derived settings.
	v.extSwitchProviderName = uplinkNet.Name()

	// Detect uplink gateway setting.
	uplinkIPv4CIDR := uplinkNetConf["ipv4.address"]
	if uplinkIPv4CIDR == "" {
		uplinkIPv4CIDR = uplinkNetConf["ipv4.gateway"]
	}

	uplinkIPv6CIDR := uplinkNetConf["ipv6.address"]
	if uplinkIPv6CIDR == "" {
		uplinkIPv6CIDR = uplinkNetConf["ipv6.gateway"]
	}

	// Optional uplink values.
	uplinkIPv4, uplinkIPv4Net, err := net.ParseCIDR(uplinkIPv4CIDR)
	if err == nil {
		v.dnsIPv4 = []net.IP{uplinkIPv4}
		v.routerExtGwIPv4 = uplinkIPv4
	}

	uplinkIPv6, uplinkIPv6Net, err := net.ParseCIDR(uplinkIPv6CIDR)
	if err == nil {
		v.dnsIPv6 = []net.IP{uplinkIPv6}
		v.routerExtGwIPv6 = uplinkIPv6
	}

	// Detect optional DNS server list.
	if uplinkNetConf["dns.nameservers"] != "" {
		// Reset nameservers.
		v.dnsIPv4 = nil
		v.dnsIPv6 = nil

		nsList := util.SplitNTrimSpace(uplinkNetConf["dns.nameservers"], ",", -1, false)
		for _, ns := range nsList {
			nsIP := net.ParseIP(ns)
			if nsIP == nil {
				return nil, fmt.Errorf("Invalid uplink nameserver")
			}

			if nsIP.To4() == nil {
				v.dnsIPv6 = append(v.dnsIPv6, nsIP)
			} else {
				v.dnsIPv4 = append(v.dnsIPv4, nsIP)
			}
		}
	}

	// Parse existing allocated IPs for this network on the uplink network (if not set yet, will be nil).
	routerExtPortIPv4 := net.ParseIP(n.config[ovnVolatileUplinkIPv4])
	routerExtPortIPv6 := net.ParseIP(n.config[ovnVolatileUplinkIPv6])

	// Decide whether we need to allocate new IP(s) and go to the expense of retrieving all allocated IPs.
	if (uplinkIPv4Net != nil && routerExtPortIPv4 == nil) || (uplinkIPv6Net != nil && routerExtPortIPv6 == nil) {
		err := n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
			allAllocatedIPv4, allAllocatedIPv6, err := n.uplinkAllAllocatedIPs(tx, uplinkNet.Name())
			if err != nil {
				return errors.Wrapf(err, "Failed to get all allocated IPs for uplink")
			}

			if uplinkIPv4Net != nil && routerExtPortIPv4 == nil {
				if uplinkNetConf["ipv4.ovn.ranges"] == "" {
					return fmt.Errorf(`Missing required "ipv4.ovn.ranges" config key on uplink network`)
				}

				ipRanges, err := parseIPRanges(uplinkNetConf["ipv4.ovn.ranges"], uplinkNet.DHCPv4Subnet())
				if err != nil {
					return errors.Wrapf(err, "Failed to parse uplink IPv4 OVN ranges")
				}

				routerExtPortIPv4, err = n.uplinkAllocateIP(ipRanges, allAllocatedIPv4)
				if err != nil {
					return errors.Wrapf(err, "Failed to allocate uplink IPv4 address")
				}

				n.config[ovnVolatileUplinkIPv4] = routerExtPortIPv4.String()
			}

			if uplinkIPv6Net != nil && routerExtPortIPv6 == nil {
				// If IPv6 OVN ranges are specified by the uplink, allocate from them.
				if uplinkNetConf["ipv6.ovn.ranges"] != "" {
					ipRanges, err := parseIPRanges(uplinkNetConf["ipv6.ovn.ranges"], uplinkNet.DHCPv6Subnet())
					if err != nil {
						return errors.Wrapf(err, "Failed to parse uplink IPv6 OVN ranges")
					}

					routerExtPortIPv6, err = n.uplinkAllocateIP(ipRanges, allAllocatedIPv6)
					if err != nil {
						return errors.Wrapf(err, "Failed to allocate uplink IPv6 address")
					}

				} else {
					// Otherwise use EUI64 derived from MAC address.
					routerExtPortIPv6, err = eui64.ParseMAC(uplinkIPv6Net.IP, routerMAC)
					if err != nil {
						return err
					}
				}

				n.config[ovnVolatileUplinkIPv6] = routerExtPortIPv6.String()
			}

			err = tx.UpdateNetwork(n.id, n.description, n.config)
			if err != nil {
				return errors.Wrapf(err, "Failed saving allocated uplink network IPs")
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Configure variables needed to configure OVN router.
	if uplinkIPv4Net != nil && routerExtPortIPv4 != nil {
		routerExtPortIPv4Net := &net.IPNet{
			Mask: uplinkIPv4Net.Mask,
			IP:   routerExtPortIPv4,
		}
		v.routerExtPortIPv4Net = routerExtPortIPv4Net.String()
	}

	if uplinkIPv6Net != nil {
		routerExtPortIPv6Net := &net.IPNet{
			Mask: uplinkIPv6Net.Mask,
			IP:   routerExtPortIPv6,
		}
		v.routerExtPortIPv6Net = routerExtPortIPv6Net.String()
	}

	return v, nil
}

// uplinkAllAllocatedIPs gets a list of all IPv4 and IPv6 addresses allocated to OVN networks connected to uplink.
func (n *ovn) uplinkAllAllocatedIPs(tx *db.ClusterTx, uplinkNetName string) ([]net.IP, []net.IP, error) {
	// Get all managed networks across all projects.
	projectNetworks, err := tx.GetCreatedNetworks()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Failed to load all networks")
	}

	v4IPs := make([]net.IP, 0)
	v6IPs := make([]net.IP, 0)

	for _, networks := range projectNetworks {
		for _, netInfo := range networks {
			if netInfo.Type != "ovn" || netInfo.Config["network"] != uplinkNetName {
				continue
			}

			for _, k := range []string{ovnVolatileUplinkIPv4, ovnVolatileUplinkIPv6} {
				if netInfo.Config[k] != "" {
					ip := net.ParseIP(netInfo.Config[k])
					if ip != nil {
						if ip.To4() != nil {
							v4IPs = append(v4IPs, ip)
						} else {
							v6IPs = append(v6IPs, ip)
						}
					}
				}
			}
		}
	}

	return v4IPs, v6IPs, nil
}

// uplinkAllocateIP allocates a free IP from one of the IP ranges.
func (n *ovn) uplinkAllocateIP(ipRanges []*shared.IPRange, allAllocated []net.IP) (net.IP, error) {
	for _, ipRange := range ipRanges {
		inc := big.NewInt(1)

		// Convert IPs in range to native representations to allow incrementing and comparison.
		startIP := ipRange.Start.To4()
		if startIP == nil {
			startIP = ipRange.Start.To16()
		}

		endIP := ipRange.End.To4()
		if endIP == nil {
			endIP = ipRange.End.To16()
		}

		startBig := big.NewInt(0)
		startBig.SetBytes(startIP)
		endBig := big.NewInt(0)
		endBig.SetBytes(endIP)

		// Iterate through IPs in range, return the first unallocated one found.
		for {
			if startBig.Cmp(endBig) > 0 {
				break
			}

			ip := net.IP(startBig.Bytes())

			// Check IP is not already allocated.
			freeIP := true
			for _, allocatedIP := range allAllocated {
				if ip.Equal(allocatedIP) {
					freeIP = false
					break
				}

			}

			if !freeIP {
				startBig.Add(startBig, inc)
				continue
			}

			return ip, nil
		}
	}

	return nil, fmt.Errorf("No free IPs available")
}

// startUplinkPort performs any network start up logic needed to connect the uplink connection to OVN.
func (n *ovn) startUplinkPort() error {
	// Uplink network must be in default project.
	uplinkNet, err := LoadByName(n.state, project.Default, n.config["network"])
	if err != nil {
		return errors.Wrapf(err, "Failed loading uplink network %q", n.config["network"])
	}

	// Lock uplink network so that if multiple OVN networks are trying to connect to the same uplink we don't
	// race each other setting up the connection.
	unlock := locking.Lock(n.uplinkOperationLockName(uplinkNet))
	defer unlock()

	switch uplinkNet.Type() {
	case "bridge":
		return n.startUplinkPortBridge(uplinkNet)
	case "physical":
		return n.startUplinkPortPhysical(uplinkNet)
	}

	return fmt.Errorf("Failed starting uplink port, network type %q unsupported as OVN uplink", uplinkNet.Type())
}

// uplinkOperationLockName returns the lock name to use for operations on the uplink network.
func (n *ovn) uplinkOperationLockName(uplinkNet Network) string {
	return fmt.Sprintf("network.ovn.%s", uplinkNet.Name())
}

// uplinkPortBridgeVars returns the uplink port bridge variables needed for port start/stop.
func (n *ovn) uplinkPortBridgeVars(uplinkNet Network) *ovnUplinkPortBridgeVars {
	ovsBridge := fmt.Sprintf("lxdovn%d", uplinkNet.ID())

	return &ovnUplinkPortBridgeVars{
		ovsBridge: ovsBridge,
		uplinkEnd: fmt.Sprintf("%sa", ovsBridge),
		ovsEnd:    fmt.Sprintf("%sb", ovsBridge),
	}
}

// startUplinkPortBridge creates veth pair (if doesn't exist), creates OVS bridge (if doesn't exist) and
// connects veth pair to uplink bridge and OVS bridge.
func (n *ovn) startUplinkPortBridge(uplinkNet Network) error {
	if uplinkNet.Config()["bridge.driver"] != "openvswitch" {
		return n.startUplinkPortBridgeNative(uplinkNet, uplinkNet.Name())
	}

	return n.startUplinkPortBridgeOVS(uplinkNet, uplinkNet.Name())
}

// startUplinkPortBridgeNative connects an OVN logical router to an uplink native bridge.
func (n *ovn) startUplinkPortBridgeNative(uplinkNet Network, bridgeDevice string) error {
	// Do this after gaining lock so that on failure we revert before release locking.
	revert := revert.New()
	defer revert.Fail()

	// If uplink is a native bridge, then use a separate OVS bridge with veth pair connection to native bridge.
	vars := n.uplinkPortBridgeVars(uplinkNet)

	// Create veth pair if needed.
	if !InterfaceExists(vars.uplinkEnd) && !InterfaceExists(vars.ovsEnd) {
		veth := &ip.Veth{
			Link: ip.Link{
				Name: vars.uplinkEnd,
			},
			PeerName: vars.ovsEnd,
		}
		err := veth.Add()
		if err != nil {
			return errors.Wrapf(err, "Failed to create the uplink veth interfaces %q and %q", vars.uplinkEnd, vars.ovsEnd)
		}

		revert.Add(func() { veth.Delete() })
	}

	// Ensure that the veth interfaces inherit the uplink bridge's MTU (which the OVS bridge also inherits).
	uplinkNetConfig := uplinkNet.Config()
	if uplinkNetConfig["bridge.mtu"] != "" {
		uplinkEndLink := &ip.Link{Name: vars.uplinkEnd}
		err := uplinkEndLink.SetMTU(uplinkNetConfig["bridge.mtu"])
		if err != nil {
			return errors.Wrapf(err, "Failed setting MTU %q on %q", uplinkNetConfig["bridge.mtu"], uplinkEndLink.Name)
		}

		ovsEndLink := &ip.Link{Name: vars.ovsEnd}
		err = ovsEndLink.SetMTU(uplinkNetConfig["bridge.mtu"])
		if err != nil {
			return errors.Wrapf(err, "Failed setting MTU %q on %q", uplinkNetConfig["bridge.mtu"], ovsEndLink.Name)
		}
	}

	// Ensure correct sysctls are set on uplink veth interfaces to avoid getting IPv6 link-local addresses.
	err := util.SysctlSet(
		fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", vars.uplinkEnd), "1",
		fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", vars.ovsEnd), "1",
		fmt.Sprintf("net/ipv6/conf/%s/forwarding", vars.uplinkEnd), "0",
		fmt.Sprintf("net/ipv6/conf/%s/forwarding", vars.ovsEnd), "0",
	)
	if err != nil {
		return errors.Wrapf(err, "Failed to configure uplink veth interfaces %q and %q", vars.uplinkEnd, vars.ovsEnd)
	}

	// Connect uplink end of veth pair to uplink bridge and bring up.
	link := &ip.Link{Name: vars.uplinkEnd}
	err = link.SetMaster(bridgeDevice)
	if err != nil {
		return errors.Wrapf(err, "Failed to connect uplink veth interface %q to uplink bridge %q", vars.uplinkEnd, bridgeDevice)
	}

	link = &ip.Link{Name: vars.uplinkEnd}
	err = link.SetUp()
	if err != nil {
		return errors.Wrapf(err, "Failed to bring up uplink veth interface %q", vars.uplinkEnd)
	}

	// Ensure uplink OVS end veth interface is up.
	link = &ip.Link{Name: vars.ovsEnd}
	err = link.SetUp()
	if err != nil {
		return errors.Wrapf(err, "Failed to bring up uplink veth interface %q", vars.ovsEnd)
	}

	// Create uplink OVS bridge if needed.
	ovs := openvswitch.NewOVS()
	err = ovs.BridgeAdd(vars.ovsBridge, true)
	if err != nil {
		return errors.Wrapf(err, "Failed to create uplink OVS bridge %q", vars.ovsBridge)
	}

	// Connect OVS end veth interface to OVS bridge.
	err = ovs.BridgePortAdd(vars.ovsBridge, vars.ovsEnd, true)
	if err != nil {
		return errors.Wrapf(err, "Failed to connect uplink veth interface %q to uplink OVS bridge %q", vars.ovsEnd, vars.ovsBridge)
	}

	// Associate OVS bridge to logical OVN provider.
	err = ovs.OVNBridgeMappingAdd(vars.ovsBridge, uplinkNet.Name())
	if err != nil {
		return errors.Wrapf(err, "Failed to associate uplink OVS bridge %q to OVN provider %q", vars.ovsBridge, uplinkNet.Name())
	}

	// Attempt to learn uplink MAC.
	n.pingOVNRouterIPv6()

	revert.Success()
	return nil
}

// startUplinkPortBridgeOVS connects an OVN logical router to an uplink OVS bridge.
func (n *ovn) startUplinkPortBridgeOVS(uplinkNet Network, bridgeDevice string) error {
	// Do this after gaining lock so that on failure we revert before release locking.
	revert := revert.New()
	defer revert.Fail()

	// If uplink is an openvswitch bridge, have OVN logical provider connect directly to it.
	ovs := openvswitch.NewOVS()
	err := ovs.OVNBridgeMappingAdd(bridgeDevice, uplinkNet.Name())
	if err != nil {
		return errors.Wrapf(err, "Failed to associate uplink OVS bridge %q to OVN provider %q", bridgeDevice, uplinkNet.Name())
	}

	// Attempt to learn uplink MAC.
	n.pingOVNRouterIPv6()

	revert.Success()
	return nil
}

// pingOVNRouterIPv6 pings the OVN router's external IPv6 address to attempt to trigger MAC learning on uplink.
// This is to work around a bug in some versions of OVN.
func (n *ovn) pingOVNRouterIPv6() {
	routerExtPortIPv6 := net.ParseIP(n.config[ovnVolatileUplinkIPv6])
	if routerExtPortIPv6 != nil {
		// Now that the OVN router is connected to the uplink bridge, attempt to ping the OVN
		// router's external IPv6 from the LXD host running the uplink bridge in an attempt to trigger the
		// OVN router to learn the uplink gateway's MAC address. This is to work around a bug in
		// older versions of OVN that meant that the OVN router would not attempt to learn the external
		// uplink IPv6 gateway MAC address when using SNAT, meaning that external IPv6 connectivity
		// wouldn't work until the next router advertisement was sent (which could be several minutes).
		// By pinging the OVN router's external IP this will trigger an NDP request from the uplink bridge
		// which will cause the OVN router to learn its MAC address.
		go func() {
			// Try several attempts as it can take a few seconds for the network to come up.
			for i := 0; i < 5; i++ {
				if pingIP(routerExtPortIPv6) {
					n.logger.Debug("OVN router external IPv6 address reachable", log.Ctx{"ip": routerExtPortIPv6.String()})
					return
				}

				time.Sleep(time.Second)
			}

			// We would expect this on a chassis node that isn't the active router gateway, it doesn't
			// always indicate a problem.
			n.logger.Debug("OVN router external IPv6 address unreachable", log.Ctx{"ip": routerExtPortIPv6.String()})
		}()
	}
}

// startUplinkPortPhysical creates OVS bridge (if doesn't exist) and connects uplink interface to the OVS bridge.
func (n *ovn) startUplinkPortPhysical(uplinkNet Network) error {
	// Do this after gaining lock so that on failure we revert before release locking.
	revert := revert.New()
	defer revert.Fail()

	uplinkConfig := uplinkNet.Config()
	uplinkHostName := GetHostDevice(uplinkConfig["parent"], uplinkConfig["vlan"])

	if !InterfaceExists(uplinkHostName) {
		return fmt.Errorf("Uplink network %q is not started (interface %q is missing)", uplinkNet.Name(), uplinkHostName)
	}

	// Detect if uplink interface is a native bridge.
	if IsNativeBridge(uplinkHostName) {
		return n.startUplinkPortBridgeNative(uplinkNet, uplinkHostName)
	}

	// Detect if uplink interface is a OVS bridge.
	ovs := openvswitch.NewOVS()
	isOVSBridge, _ := ovs.BridgeExists(uplinkHostName)
	if isOVSBridge {
		return n.startUplinkPortBridgeOVS(uplinkNet, uplinkHostName)
	}

	// If uplink is a normal physical interface, then use a separate OVS bridge and connect uplink to it.
	vars := n.uplinkPortBridgeVars(uplinkNet)

	// Check no global unicast IPs defined on uplink, as that may indicate it is in use by another application.
	addresses, _, err := InterfaceStatus(uplinkHostName)
	if err != nil {
		return errors.Wrapf(err, "Failed getting interface status for %q", uplinkHostName)
	}

	if len(addresses) > 0 {
		return fmt.Errorf("Cannot start network as uplink network interface %q has one or more IP addresses configured on it", uplinkHostName)
	}

	// Ensure correct sysctls are set on uplink interface to avoid getting IPv6 link-local addresses.
	err = util.SysctlSet(
		fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", uplinkHostName), "1",
		fmt.Sprintf("net/ipv6/conf/%s/forwarding", uplinkHostName), "0",
	)
	if err != nil {
		return errors.Wrapf(err, "Failed to configure uplink interface %q", uplinkHostName)
	}

	// Create uplink OVS bridge if needed.
	err = ovs.BridgeAdd(vars.ovsBridge, true)
	if err != nil {
		return errors.Wrapf(err, "Failed to create uplink OVS bridge %q", vars.ovsBridge)
	}

	// Connect OVS end veth interface to OVS bridge.
	err = ovs.BridgePortAdd(vars.ovsBridge, uplinkHostName, true)
	if err != nil {
		return errors.Wrapf(err, "Failed to connect uplink interface %q to uplink OVS bridge %q", uplinkHostName, vars.ovsBridge)
	}

	// Associate OVS bridge to logical OVN provider.
	err = ovs.OVNBridgeMappingAdd(vars.ovsBridge, uplinkNet.Name())
	if err != nil {
		return errors.Wrapf(err, "Failed to associate uplink OVS bridge %q to OVN provider %q", vars.ovsBridge, uplinkNet.Name())
	}

	// Bring uplink interface up.
	link := &ip.Link{Name: uplinkHostName}
	err = link.SetUp()
	if err != nil {
		return errors.Wrapf(err, "Failed to bring up uplink interface %q", uplinkHostName)
	}

	revert.Success()
	return nil
}

// checkUplinkUse checks if uplink network is used by another OVN network.
func (n *ovn) checkUplinkUse() (bool, error) {
	// Get all managed networks across all projects.
	var err error
	var projectNetworks map[string]map[int64]api.Network

	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		projectNetworks, err = tx.GetCreatedNetworks()
		return err
	})
	if err != nil {
		return false, errors.Wrapf(err, "Failed to load all networks")
	}

	for projectName, networks := range projectNetworks {
		for _, network := range networks {
			if (projectName == n.project && network.Name == n.name) || network.Type != "ovn" {
				continue // Ignore our own DB record or non OVN networks.
			}

			// Check if another network is using our uplink.
			if network.Config["network"] == n.config["network"] {
				return true, nil
			}
		}
	}

	return false, nil
}

// deleteUplinkPort deletes the uplink connection.
func (n *ovn) deleteUplinkPort() error {
	// Uplink network must be in default project.
	if n.config["network"] != "" {
		uplinkNet, err := LoadByName(n.state, project.Default, n.config["network"])
		if err != nil {
			return errors.Wrapf(err, "Failed loading uplink network %q", n.config["network"])
		}

		// Lock uplink network so we don't race each other networks using the OVS uplink bridge.
		unlock := locking.Lock(n.uplinkOperationLockName(uplinkNet))
		defer unlock()

		switch uplinkNet.Type() {
		case "bridge":
			return n.deleteUplinkPortBridge(uplinkNet)
		case "physical":
			return n.deleteUplinkPortPhysical(uplinkNet)
		}

		return fmt.Errorf("Failed deleting uplink port, network type %q unsupported as OVN uplink", uplinkNet.Type())
	}

	return nil
}

// deleteUplinkPortBridge disconnects the uplink port from the bridge and performs any cleanup.
func (n *ovn) deleteUplinkPortBridge(uplinkNet Network) error {
	if uplinkNet.Config()["bridge.driver"] != "openvswitch" {
		return n.deleteUplinkPortBridgeNative(uplinkNet)
	}

	return n.deleteUplinkPortBridgeOVS(uplinkNet)
}

// deleteUplinkPortBridge deletes uplink OVS bridge, OVN bridge mappings and veth interfaces if not in use.
func (n *ovn) deleteUplinkPortBridgeNative(uplinkNet Network) error {
	// Check OVS uplink bridge exists, if it does, check whether the uplink network is in use.
	removeVeths := false
	vars := n.uplinkPortBridgeVars(uplinkNet)
	if InterfaceExists(vars.ovsBridge) {
		uplinkUsed, err := n.checkUplinkUse()
		if err != nil {
			return err
		}

		// Remove OVS bridge if the uplink network isn't used by any other OVN networks.
		if !uplinkUsed {
			removeVeths = true

			ovs := openvswitch.NewOVS()
			err = ovs.OVNBridgeMappingDelete(vars.ovsBridge, uplinkNet.Name())
			if err != nil {
				return err
			}

			err = ovs.BridgeDelete(vars.ovsBridge)
			if err != nil {
				return err
			}
		}
	} else {
		removeVeths = true // Remove the veths if OVS bridge already gone.
	}

	// Remove the veth interfaces if they exist.
	if removeVeths {
		if InterfaceExists(vars.uplinkEnd) {
			link := &ip.Link{Name: vars.uplinkEnd}
			err := link.Delete()
			if err != nil {
				return errors.Wrapf(err, "Failed to delete the uplink veth interface %q", vars.uplinkEnd)
			}
		}

		if InterfaceExists(vars.ovsEnd) {
			link := &ip.Link{Name: vars.ovsEnd}
			err := link.Delete()
			if err != nil {
				return errors.Wrapf(err, "Failed to delete the uplink veth interface %q", vars.ovsEnd)
			}
		}
	}

	return nil
}

// deleteUplinkPortBridge deletes OVN bridge mappings if not in use.
func (n *ovn) deleteUplinkPortBridgeOVS(uplinkNet Network) error {
	uplinkUsed, err := n.checkUplinkUse()
	if err != nil {
		return err
	}

	// Remove uplink OVS bridge mapping if not in use by other OVN networks.
	if !uplinkUsed {
		ovs := openvswitch.NewOVS()
		err = ovs.OVNBridgeMappingDelete(uplinkNet.Name(), uplinkNet.Name())
		if err != nil {
			return err
		}
	}

	return nil
}

// deleteUplinkPortPhysical deletes uplink OVS bridge and OVN bridge mappings if not in use.
func (n *ovn) deleteUplinkPortPhysical(uplinkNet Network) error {
	uplinkConfig := uplinkNet.Config()
	uplinkHostName := GetHostDevice(uplinkConfig["parent"], uplinkConfig["vlan"])

	// Detect if uplink interface is a native bridge.
	if IsNativeBridge(uplinkHostName) {
		return n.deleteUplinkPortBridgeNative(uplinkNet)
	}

	// Detect if uplink interface is a OVS bridge.
	ovs := openvswitch.NewOVS()
	isOVSBridge, _ := ovs.BridgeExists(uplinkHostName)
	if isOVSBridge {
		return n.deleteUplinkPortBridgeOVS(uplinkNet)
	}

	// Otherwise if uplink is normal physical interface, attempt cleanup of OVS bridge.

	// Check OVS uplink bridge exists, if it does, check whether the uplink network is in use.
	releaseIF := false
	vars := n.uplinkPortBridgeVars(uplinkNet)
	if InterfaceExists(vars.ovsBridge) {
		uplinkUsed, err := n.checkUplinkUse()
		if err != nil {
			return err
		}

		// Remove OVS bridge if the uplink network isn't used by any other OVN networks.
		if !uplinkUsed {
			releaseIF = true

			ovs := openvswitch.NewOVS()
			err = ovs.OVNBridgeMappingDelete(vars.ovsBridge, uplinkNet.Name())
			if err != nil {
				return err
			}

			err = ovs.BridgeDelete(vars.ovsBridge)
			if err != nil {
				return err
			}
		}
	} else {
		releaseIF = true // Bring uplink interface down if not needed.
	}

	// Bring down uplink interface if not used and exists.
	if releaseIF && InterfaceExists(uplinkHostName) {
		link := &ip.Link{Name: uplinkHostName}
		err := link.SetDown()
		if err != nil {
			return errors.Wrapf(err, "Failed to bring down uplink interface %q", uplinkHostName)
		}
	}

	return nil
}

// FillConfig fills requested config with any default values.
func (n *ovn) FillConfig(config map[string]string) error {
	if config["ipv4.address"] == "" {
		config["ipv4.address"] = "auto"
	}

	if config["ipv6.address"] == "" {
		content, err := ioutil.ReadFile("/proc/sys/net/ipv6/conf/default/disable_ipv6")
		if err == nil && string(content) == "0\n" {
			config["ipv6.address"] = "auto"
		}
	}

	// Now replace any "auto" keys with generated values.
	err := n.populateAutoConfig(config)
	if err != nil {
		return errors.Wrapf(err, "Failed generating auto config")
	}

	return nil
}

// populateAutoConfig replaces "auto" in config with generated values.
func (n *ovn) populateAutoConfig(config map[string]string) error {
	changedConfig := false

	if config["ipv4.address"] == "auto" {
		subnet, err := randomSubnetV4()
		if err != nil {
			return err
		}

		config["ipv4.address"] = subnet

		if config["ipv4.nat"] == "" {
			config["ipv4.nat"] = "true"
		}

		changedConfig = true
	}

	if config["ipv6.address"] == "auto" {
		subnet, err := randomSubnetV6()
		if err != nil {
			return err
		}

		config["ipv6.address"] = subnet

		if config["ipv6.nat"] == "" {
			config["ipv6.nat"] = "true"
		}

		changedConfig = true
	}

	// Re-validate config if changed.
	if changedConfig && n.state != nil {
		return n.Validate(config)
	}

	return nil
}

// Create sets up network in OVN Northbound database.
func (n *ovn) Create(clientType request.ClientType) error {
	n.logger.Debug("Create", log.Ctx{"clientType": clientType, "config": n.config})

	// We only need to setup the OVN Northbound database once, not on every clustered node.
	if clientType == request.ClientTypeNormal {
		err := n.setup(false)
		if err != nil {
			return err
		}
	}

	return nil
}

// allowedUplinkNetworks returns a list of allowed networks to use as uplinks based on project restrictions.
func (n *ovn) allowedUplinkNetworks(p *db.Project) ([]string, error) {
	// Uplink networks are always from the default project.
	networks, err := n.state.Cluster.GetNetworks(project.Default)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed getting uplink networks")
	}

	// Remove ourselves from the networks list if we are in the default project.
	if n.project == project.Default {
		allNets := networks
		networks = make([]string, 0, len(allNets)-1)
		for _, network := range allNets {
			if network == n.name {
				continue
			}

			networks = append(networks, network)
		}
	}

	// If project is not restricted, return full network list.
	if !shared.IsTrue(p.Config["restricted"]) {
		return networks, nil
	}

	allowedNetworks := []string{}

	// There are no allowed networks if restricted.networks.uplinks is not set.
	if p.Config["restricted.networks.uplinks"] == "" {
		return allowedNetworks, nil
	}

	// Parse the allowed uplinks and return any that are present in the actual defined networks.
	allowedRestrictedUplinks := util.SplitNTrimSpace(p.Config["restricted.networks.uplinks"], ",", -1, false)

	for _, allowedRestrictedUplink := range allowedRestrictedUplinks {
		if shared.StringInSlice(allowedRestrictedUplink, networks) {
			allowedNetworks = append(allowedNetworks, allowedRestrictedUplink)
		}
	}

	return allowedNetworks, nil
}

// validateUplinkNetwork checks if uplink network is allowed, and if empty string is supplied then tries to select
// an uplink network from the allowedUplinkNetworks() list if there is only one allowed network.
// Returns chosen uplink network name to use.
func (n *ovn) validateUplinkNetwork(p *db.Project, uplinkNetworkName string) (string, error) {
	allowedUplinkNetworks, err := n.allowedUplinkNetworks(p)
	if err != nil {
		return "", err
	}

	if uplinkNetworkName != "" {
		if !shared.StringInSlice(uplinkNetworkName, allowedUplinkNetworks) {
			return "", fmt.Errorf(`Option "network" value %q is not one of the allowed uplink networks in project`, uplinkNetworkName)
		}

		return uplinkNetworkName, nil
	}

	allowedNetworkCount := len(allowedUplinkNetworks)
	if allowedNetworkCount == 0 {
		return "", fmt.Errorf(`No allowed uplink networks in project`)
	} else if allowedNetworkCount == 1 {
		// If there is only one allowed uplink network then use it if not specified by user.
		return allowedUplinkNetworks[0], nil
	}

	return "", fmt.Errorf(`Option "network" is required`)
}

func (n *ovn) setup(update bool) error {
	// If we are in mock mode, just no-op.
	if n.state.OS.MockMode {
		return nil
	}

	n.logger.Debug("Setting up network")

	revert := revert.New()
	defer revert.Fail()

	client, err := openvswitch.NewOVN(n.state)
	if err != nil {
		return errors.Wrapf(err, "Failed to get OVN client")
	}

	var routerExtPortIPv4, routerIntPortIPv4, routerExtPortIPv6, routerIntPortIPv6 net.IP
	var routerExtPortIPv4Net, routerIntPortIPv4Net, routerExtPortIPv6Net, routerIntPortIPv6Net *net.IPNet

	// Record updated config so we can store back into DB and n.config variable.
	updatedConfig := make(map[string]string)

	// Load the project to get uplink network restrictions.
	p, err := n.state.Cluster.GetProject(n.project)
	if err != nil {
		return errors.Wrapf(err, "Failed to load network restrictions from project %q", n.project)
	}

	// Get project ID.
	var projectID int64
	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		projectID, err = tx.GetProjectID(n.project)
		return err
	})
	if err != nil {
		return errors.Wrapf(err, "Failed getting project ID for project %q", n.project)
	}

	// Check project restrictions and get uplink network to use.
	uplinkNetwork, err := n.validateUplinkNetwork(p, n.config["network"])
	if err != nil {
		return err
	}

	// Ensure automatically selected uplink network is saved into "network" key.
	if uplinkNetwork != n.config["network"] {
		updatedConfig["network"] = uplinkNetwork
	}

	// Get bridge MTU to use.
	bridgeMTU := n.getBridgeMTU()
	if bridgeMTU == 0 {
		// If no manual bridge MTU specified, derive it from the underlay network.
		bridgeMTU, err = n.getOptimalBridgeMTU()
		if err != nil {
			return errors.Wrapf(err, "Failed getting optimal bridge MTU")
		}

		// Save to config so the value can be read by instances connecting to network.
		updatedConfig["bridge.mtu"] = fmt.Sprintf("%d", bridgeMTU)
	}

	// Apply any config dynamically generated to the current config and store back to DB in single transaction.
	if len(updatedConfig) > 0 {
		for k, v := range updatedConfig {
			n.config[k] = v
		}

		err := n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
			err = tx.UpdateNetwork(n.id, n.description, n.config)
			if err != nil {
				return errors.Wrapf(err, "Failed saving updated network config")
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	// Get router MAC address.
	routerMAC, err := n.getRouterMAC()
	if err != nil {
		return err
	}

	// Setup uplink port (do this first to check uplink is suitable).
	uplinkNet, err := n.setupUplinkPort(routerMAC)
	if err != nil {
		return err
	}

	// Parse router IP config.
	if uplinkNet.routerExtPortIPv4Net != "" {
		routerExtPortIPv4, routerExtPortIPv4Net, err = net.ParseCIDR(uplinkNet.routerExtPortIPv4Net)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's external uplink port IPv4 Net")
		}
	}

	if uplinkNet.routerExtPortIPv6Net != "" {
		routerExtPortIPv6, routerExtPortIPv6Net, err = net.ParseCIDR(uplinkNet.routerExtPortIPv6Net)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's external uplink port IPv6 Net")
		}
	}

	if validate.IsOneOf(n.getRouterIntPortIPv4Net(), []string{"none", ""}) != nil {
		routerIntPortIPv4, routerIntPortIPv4Net, err = net.ParseCIDR(n.getRouterIntPortIPv4Net())
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's internal port IPv4 Net")
		}
	}

	if validate.IsOneOf(n.getRouterIntPortIPv6Net(), []string{"none", ""}) != nil {
		routerIntPortIPv6, routerIntPortIPv6Net, err = net.ParseCIDR(n.getRouterIntPortIPv6Net())
		if err != nil {
			return errors.Wrapf(err, "Failed parsing router's internal port IPv6 Net")
		}
	}

	// Create chassis group.
	err = client.ChassisGroupAdd(n.getChassisGroupName(), update)
	if err != nil {
		return err
	}

	if !update {
		revert.Add(func() { client.ChassisGroupDelete(n.getChassisGroupName()) })
	}

	// Create logical router.
	err = client.LogicalRouterAdd(n.getRouterName(), update)
	if err != nil {
		return errors.Wrapf(err, "Failed adding router")
	}

	if !update {
		revert.Add(func() { client.LogicalRouterDelete(n.getRouterName()) })
	}

	// Configure logical router.

	// Generate external router port IPs (in CIDR format).
	extRouterIPs := []*net.IPNet{}
	if routerExtPortIPv4Net != nil {
		extRouterIPs = append(extRouterIPs, &net.IPNet{
			IP:   routerExtPortIPv4,
			Mask: routerExtPortIPv4Net.Mask,
		})
	}

	if routerExtPortIPv6Net != nil {
		extRouterIPs = append(extRouterIPs, &net.IPNet{
			IP:   routerExtPortIPv6,
			Mask: routerExtPortIPv6Net.Mask,
		})
	}

	if len(extRouterIPs) > 0 {
		err = client.LogicalSwitchAdd(n.getExtSwitchName(), update)
		if err != nil {
			return errors.Wrapf(err, "Failed adding external switch")
		}

		if !update {
			revert.Add(func() { client.LogicalSwitchDelete(n.getExtSwitchName()) })
		}

		// Create external router port.
		err = client.LogicalRouterPortAdd(n.getRouterName(), n.getRouterExtPortName(), routerMAC, extRouterIPs, update)
		if err != nil {
			return errors.Wrapf(err, "Failed adding external router port")
		}

		if !update {
			revert.Add(func() { client.LogicalRouterPortDelete(n.getRouterExtPortName()) })
		}

		// Associate external router port to chassis group.
		err = client.LogicalRouterPortLinkChassisGroup(n.getRouterExtPortName(), n.getChassisGroupName())
		if err != nil {
			return errors.Wrapf(err, "Failed linking external router port to chassis group")
		}

		// Create external switch port and link to router port.
		err = client.LogicalSwitchPortAdd(n.getExtSwitchName(), n.getExtSwitchRouterPortName(), nil, update)
		if err != nil {
			return errors.Wrapf(err, "Failed adding external switch router port")
		}

		if !update {
			revert.Add(func() { client.LogicalSwitchPortDelete(n.getExtSwitchRouterPortName()) })
		}

		err = client.LogicalSwitchPortLinkRouter(n.getExtSwitchRouterPortName(), n.getRouterExtPortName())
		if err != nil {
			return errors.Wrapf(err, "Failed linking external router port to external switch port")
		}

		// Create external switch port and link to external provider network.
		err = client.LogicalSwitchPortAdd(n.getExtSwitchName(), n.getExtSwitchProviderPortName(), nil, update)
		if err != nil {
			return errors.Wrapf(err, "Failed adding external switch provider port")
		}

		if !update {
			revert.Add(func() { client.LogicalSwitchPortDelete(n.getExtSwitchProviderPortName()) })
		}

		err = client.LogicalSwitchPortLinkProviderNetwork(n.getExtSwitchProviderPortName(), uplinkNet.extSwitchProviderName)
		if err != nil {
			return errors.Wrapf(err, "Failed linking external switch provider port to external provider network")
		}

		// Remove any existing SNAT rules on update. As currently these are only defined from the network
		// config rather than from any instance NIC config, so we can re-create the active config below.
		if update {
			err = client.LogicalRouterSNATDeleteAll(n.getRouterName())
			if err != nil {
				return errors.Wrapf(err, "Failed removing existing router SNAT rules")
			}
		}

		// Add SNAT rules.
		if shared.IsTrue(n.config["ipv4.nat"]) && routerIntPortIPv4Net != nil && routerExtPortIPv4 != nil {
			err = client.LogicalRouterSNATAdd(n.getRouterName(), routerIntPortIPv4Net, routerExtPortIPv4, update)
			if err != nil {
				return errors.Wrapf(err, "Failed adding router IPv4 SNAT rule")
			}
		}

		if shared.IsTrue(n.config["ipv6.nat"]) && routerIntPortIPv6Net != nil && routerExtPortIPv6 != nil {
			err = client.LogicalRouterSNATAdd(n.getRouterName(), routerIntPortIPv6Net, routerExtPortIPv6, update)
			if err != nil {
				return errors.Wrapf(err, "Failed adding router IPv6 SNAT rule")
			}
		}

		// Add or remove default routes as config dictates.
		defaultIPv4Route := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
		if uplinkNet.routerExtGwIPv4 != nil {
			err = client.LogicalRouterRouteAdd(n.getRouterName(), defaultIPv4Route, uplinkNet.routerExtGwIPv4, update)
			if err != nil {
				return errors.Wrapf(err, "Failed adding IPv4 default route")
			}
		} else if update {
			err = client.LogicalRouterRouteDelete(n.getRouterName(), defaultIPv4Route)
			if err != nil {
				return errors.Wrapf(err, "Failed removing IPv4 default route")
			}
		}

		defaultIPv6Route := &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
		if uplinkNet.routerExtGwIPv6 != nil {
			err = client.LogicalRouterRouteAdd(n.getRouterName(), defaultIPv6Route, uplinkNet.routerExtGwIPv6, update)
			if err != nil {
				return errors.Wrapf(err, "Failed adding IPv6 default route")
			}
		} else if update {
			err = client.LogicalRouterRouteDelete(n.getRouterName(), defaultIPv6Route)
			if err != nil {
				return errors.Wrapf(err, "Failed removing IPv6 default route")
			}
		}
	}

	// Create internal logical switch if not updating.
	err = client.LogicalSwitchAdd(n.getIntSwitchName(), update)
	if err != nil {
		return errors.Wrapf(err, "Failed adding internal switch")
	}

	if !update {
		revert.Add(func() { client.LogicalSwitchDelete(n.getIntSwitchName()) })
	}

	var excludeIPV4 []shared.IPRange
	if routerIntPortIPv4 != nil {
		excludeIPV4 = []shared.IPRange{{Start: routerIntPortIPv4}}
	}

	// Setup IP allocation config on logical switch.
	err = client.LogicalSwitchSetIPAllocation(n.getIntSwitchName(), &openvswitch.OVNIPAllocationOpts{
		PrefixIPv4:  routerIntPortIPv4Net,
		PrefixIPv6:  routerIntPortIPv6Net,
		ExcludeIPv4: excludeIPV4,
	})
	if err != nil {
		return errors.Wrapf(err, "Failed setting IP allocation settings on internal switch")
	}

	// Gather internal router port IPs (in CIDR format).
	intRouterIPs := []*net.IPNet{}

	if routerIntPortIPv4Net != nil {
		intRouterIPs = append(intRouterIPs, &net.IPNet{
			IP:   routerIntPortIPv4,
			Mask: routerIntPortIPv4Net.Mask,
		})
	}

	if routerIntPortIPv6Net != nil {
		intRouterIPs = append(intRouterIPs, &net.IPNet{
			IP:   routerIntPortIPv6,
			Mask: routerIntPortIPv6Net.Mask,
		})
	}

	if len(intRouterIPs) <= 0 {
		return fmt.Errorf("No internal IPs defined for network router")
	}

	// Create internal router port.
	err = client.LogicalRouterPortAdd(n.getRouterName(), n.getRouterIntPortName(), routerMAC, intRouterIPs, update)
	if err != nil {
		return errors.Wrapf(err, "Failed adding internal router port")
	}

	if !update {
		revert.Add(func() { client.LogicalRouterPortDelete(n.getRouterIntPortName()) })
	}

	// Configure DHCP option sets.
	var dhcpv4UUID, dhcpv6UUID openvswitch.OVNDHCPOptionsUUID
	dhcpV4Subnet := n.DHCPv4Subnet()
	dhcpV6Subnet := n.DHCPv6Subnet()

	if update {
		// Find first existing DHCP options set for IPv4 and IPv6 and update them instead of adding sets.
		existingOpts, err := client.LogicalSwitchDHCPOptionsGet(n.getIntSwitchName())
		if err != nil {
			return errors.Wrapf(err, "Failed getting existing DHCP settings for internal switch")
		}

		// DHCP option records to delete if DHCP is being disabled.
		var deleteDHCPRecords []openvswitch.OVNDHCPOptionsUUID

		for _, existingOpt := range existingOpts {
			if existingOpt.CIDR.IP.To4() == nil {
				if dhcpv6UUID == "" {
					dhcpv6UUID = existingOpt.UUID

					if dhcpV6Subnet == nil {
						deleteDHCPRecords = append(deleteDHCPRecords, dhcpv6UUID)
					}
				}
			} else {
				if dhcpv4UUID == "" {
					dhcpv4UUID = existingOpt.UUID

					if dhcpV4Subnet == nil {
						deleteDHCPRecords = append(deleteDHCPRecords, dhcpv4UUID)
					}
				}
			}
		}

		if len(deleteDHCPRecords) > 0 {
			err = client.LogicalSwitchDHCPOptionsDelete(n.getIntSwitchName(), deleteDHCPRecords...)
			if err != nil {
				return errors.Wrapf(err, "Failed deleting existing DHCP settings for internal switch")
			}
		}
	}

	// Create DHCPv4 options for internal switch.
	if dhcpV4Subnet != nil {
		err = client.LogicalSwitchDHCPv4OptionsSet(n.getIntSwitchName(), dhcpv4UUID, dhcpV4Subnet, &openvswitch.OVNDHCPv4Opts{
			ServerID:           routerIntPortIPv4,
			ServerMAC:          routerMAC,
			Router:             routerIntPortIPv4,
			RecursiveDNSServer: uplinkNet.dnsIPv4,
			DomainName:         n.getDomainName(),
			LeaseTime:          time.Duration(time.Hour * 1),
			MTU:                bridgeMTU,
		})
		if err != nil {
			return errors.Wrapf(err, "Failed adding DHCPv4 settings for internal switch")
		}
	}

	// Create DHCPv6 options for internal switch.
	if dhcpV6Subnet != nil {
		err = client.LogicalSwitchDHCPv6OptionsSet(n.getIntSwitchName(), dhcpv6UUID, dhcpV6Subnet, &openvswitch.OVNDHCPv6Opts{
			ServerID:           routerMAC,
			RecursiveDNSServer: uplinkNet.dnsIPv6,
			DNSSearchList:      n.getDNSSearchList(),
		})
		if err != nil {
			return errors.Wrapf(err, "Failed adding DHCPv6 settings for internal switch")
		}
	}

	// Set IPv6 router advertisement settings.
	if routerIntPortIPv6Net != nil {
		adressMode := openvswitch.OVNIPv6AddressModeSLAAC
		if dhcpV6Subnet != nil {
			adressMode = openvswitch.OVNIPv6AddressModeDHCPStateless
			if shared.IsTrue(n.config["ipv6.dhcp.stateful"]) {
				adressMode = openvswitch.OVNIPv6AddressModeDHCPStateful
			}
		}

		var recursiveDNSServer net.IP
		if len(uplinkNet.dnsIPv6) > 0 {
			recursiveDNSServer = uplinkNet.dnsIPv6[0] // OVN only supports 1 RA DNS server.
		}

		err = client.LogicalRouterPortSetIPv6Advertisements(n.getRouterIntPortName(), &openvswitch.OVNIPv6RAOpts{
			AddressMode:        adressMode,
			SendPeriodic:       true,
			DNSSearchList:      n.getDNSSearchList(),
			RecursiveDNSServer: recursiveDNSServer,
			MTU:                bridgeMTU,

			// Keep these low until we support DNS search domains via DHCPv4, as otherwise RA DNSSL
			// won't take effect until advert after DHCPv4 has run on instance.
			MinInterval: time.Duration(time.Second * 30),
			MaxInterval: time.Duration(time.Minute * 1),
		})
		if err != nil {
			return errors.Wrapf(err, "Failed setting internal router port IPv6 advertisement settings")
		}
	} else {
		err = client.LogicalRouterPortDeleteIPv6Advertisements(n.getRouterIntPortName())
		if err != nil {
			return errors.Wrapf(err, "Failed removing internal router port IPv6 advertisement settings")
		}
	}

	// Create internal switch port and link to router port.
	err = client.LogicalSwitchPortAdd(n.getIntSwitchName(), n.getIntSwitchRouterPortName(), nil, update)
	if err != nil {
		return errors.Wrapf(err, "Failed adding internal switch router port")
	}

	if !update {
		revert.Add(func() { client.LogicalSwitchPortDelete(n.getIntSwitchRouterPortName()) })
	}

	err = client.LogicalSwitchPortLinkRouter(n.getIntSwitchRouterPortName(), n.getRouterIntPortName())
	if err != nil {
		return errors.Wrapf(err, "Failed linking internal router port to internal switch port")
	}

	// Apply baseline ACL rules to internal logical switch.
	err = acl.OVNApplyNetworkBaselineRules(client, n.getIntSwitchName(), n.getIntSwitchRouterPortName(), intRouterIPs, append(uplinkNet.dnsIPv4, uplinkNet.dnsIPv6...))
	if err != nil {
		return errors.Wrapf(err, "Failed applying baseline ACL rules to internal switch")
	}

	// Create network port group if needed.
	err = n.ensureNetworkPortGroup(projectID)
	if err != nil {
		return errors.Wrapf(err, "Failed to setup network port group")
	}

	// Ensure any network assigned security ACL port groups are created ready for instance NICs to use.
	securityACLS := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, true)
	if len(securityACLS) > 0 {
		// Get map of ACL names to DB IDs (used for generating OVN port group names).
		aclNameIDs, err := n.state.Cluster.GetNetworkACLIDsByNames(n.Project())
		if err != nil {
			return errors.Wrapf(err, "Failed getting network ACL IDs for security ACL setup")
		}

		// Request our network is setup with the specified ACLs.
		aclNets := map[string]acl.NetworkACLUsage{
			n.Name(): {Name: n.Name(), Type: n.Type(), ID: n.ID(), Config: n.Config()},
		}

		r, err := acl.OVNEnsureACLs(n.state, n.logger, client, n.Project(), aclNameIDs, aclNets, securityACLS, false)
		if err != nil {
			return errors.Wrapf(err, "Failed ensuring security ACLs are configured in OVN for network")
		}
		revert.Add(r.Fail)
	}

	revert.Success()
	return nil
}

// ensureNetworkPortGroup ensures that the network level port group (used for classifying NICs connected to this
// network as internal) exists.
func (n *ovn) ensureNetworkPortGroup(projectID int64) error {
	client, err := openvswitch.NewOVN(n.state)
	if err != nil {
		return errors.Wrapf(err, "Failed to get OVN client")
	}

	// Create port group (if needed) for NICs to classify as internal.
	intPortGroupName := acl.OVNIntSwitchPortGroupName(n.ID())
	intPortGroupUUID, _, err := client.PortGroupInfo(intPortGroupName)
	if err != nil {
		return errors.Wrapf(err, "Failed getting port group UUID for network %q setup", n.Name())
	}

	if intPortGroupUUID == "" {
		// Create internal port group and associate it with the logical switch, so that it will be
		// removed when the logical switch is removed.
		err = client.PortGroupAdd(projectID, intPortGroupName, "", n.getIntSwitchName())
		if err != nil {
			return errors.Wrapf(err, "Failed creating port group %q for network %q setup", intPortGroupName, n.Name())
		}
	}

	return nil
}

// addChassisGroupEntry adds an entry for the local OVS chassis to the OVN logical network's chassis group.
// The chassis priority value is a stable-random value derived from chassis group name and node ID. This is so we
// don't end up using the same chassis for the primary uplink chassis for all OVN networks in a cluster.
func (n *ovn) addChassisGroupEntry() error {
	client, err := openvswitch.NewOVN(n.state)
	if err != nil {
		return errors.Wrapf(err, "Failed to get OVN client")
	}

	// Get local chassis ID for chassis group.
	ovs := openvswitch.NewOVS()
	chassisID, err := ovs.ChassisID()
	if err != nil {
		return errors.Wrapf(err, "Failed getting OVS Chassis ID")
	}

	// Seed the stable random number generator with the chassis group name.
	// This way each OVN network will have its own random seed, so that we don't end up using the same chassis
	// for the primary uplink chassis for all OVN networks in a cluster.
	chassisGroupName := n.getChassisGroupName()
	r, err := util.GetStableRandomGenerator(string(chassisGroupName))
	if err != nil {
		return errors.Wrapf(err, "Failed generating stable random chassis group priority")
	}

	// Get all nodes in cluster.
	ourNodeID := int(n.state.Cluster.GetNodeID())
	var nodeIDs []int
	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		nodes, err := tx.GetNodes()
		if err != nil {
			return errors.Wrapf(err, "Failed getting node list for adding chassis group entry")
		}

		for _, node := range nodes {
			nodeIDs = append(nodeIDs, int(node.ID))
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Sort the nodes based on ID for stable priority generation.
	sort.Sort(sort.IntSlice(nodeIDs))

	// Generate a random priority from the seed for each node until we find a match for our node ID.
	// In this way the chassis priority for this node will be set to a per-node stable random value.
	var priority uint
	for _, nodeID := range nodeIDs {
		priority = uint(r.Intn(ovnChassisPriorityMax + 1))
		if nodeID == ourNodeID {
			break
		}
	}

	err = client.ChassisGroupChassisAdd(chassisGroupName, chassisID, priority)
	if err != nil {
		return errors.Wrapf(err, "Failed adding OVS chassis %q with priority %d to chassis group %q", chassisID, priority, chassisGroupName)
	}
	n.logger.Debug("Chassis group entry added", log.Ctx{"chassisGroup": chassisGroupName, "memberID": ourNodeID, "priority": priority})

	return nil
}

// deleteChassisGroupEntry deletes an entry for the local OVS chassis from the OVN logical network's chassis group.
func (n *ovn) deleteChassisGroupEntry() error {
	client, err := openvswitch.NewOVN(n.state)
	if err != nil {
		return errors.Wrapf(err, "Failed to get OVN client")
	}

	// Add local chassis to chassis group.
	ovs := openvswitch.NewOVS()
	chassisID, err := ovs.ChassisID()
	if err != nil {
		return errors.Wrapf(err, "Failed getting OVS Chassis ID")
	}

	err = client.ChassisGroupChassisDelete(n.getChassisGroupName(), chassisID)
	if err != nil {
		return errors.Wrapf(err, "Failed deleting OVS chassis %q from chassis group %q", chassisID, n.getChassisGroupName())
	}

	return nil
}

// Delete deletes a network.
func (n *ovn) Delete(clientType request.ClientType) error {
	n.logger.Debug("Delete", log.Ctx{"clientType": clientType})

	err := n.Stop()
	if err != nil {
		return err
	}

	if clientType == request.ClientTypeNormal {
		client, err := openvswitch.NewOVN(n.state)
		if err != nil {
			return errors.Wrapf(err, "Failed to get OVN client")
		}

		err = client.LogicalRouterDelete(n.getRouterName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchDelete(n.getExtSwitchName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchDelete(n.getIntSwitchName())
		if err != nil {
			return err
		}

		err = client.LogicalRouterPortDelete(n.getRouterExtPortName())
		if err != nil {
			return err
		}

		err = client.LogicalRouterPortDelete(n.getRouterIntPortName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchPortDelete(n.getExtSwitchRouterPortName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchPortDelete(n.getExtSwitchProviderPortName())
		if err != nil {
			return err
		}

		err = client.LogicalSwitchPortDelete(n.getIntSwitchRouterPortName())
		if err != nil {
			return err
		}

		// Must be done after logical router removal.
		err = client.ChassisGroupDelete(n.getChassisGroupName())
		if err != nil {
			return err
		}

		// Check for port groups that will become unused (and need deleting) as this network is deleted.
		securityACLs := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, true)
		if len(securityACLs) > 0 {
			err = acl.OVNPortGroupDeleteIfUnused(n.state, n.logger, client, n.project, &api.Network{Name: n.name}, "")
			if err != nil {
				return errors.Wrapf(err, "Failed removing unused OVN port groups")
			}
		}
	}

	return n.common.delete(clientType)
}

// Rename renames a network.
func (n *ovn) Rename(newName string) error {
	n.logger.Debug("Rename", log.Ctx{"newName": newName})

	// Rename common steps.
	err := n.common.rename(newName)
	if err != nil {
		return err
	}

	return nil
}

// Start starts adds the local OVS chassis ID to the OVN chass group and starts the local OVS uplink port.
func (n *ovn) Start() error {
	err := n.start()
	if err != nil {
		err := n.state.Cluster.UpsertWarningLocalNode(n.project, dbCluster.TypeNetwork, int(n.id), db.WarningNetworkStartupFailure, err.Error())
		if err != nil {
			n.logger.Warn("Failed to create warning", log.Ctx{"err": err})
		}
	} else {
		err := warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.Cluster, n.project, db.WarningNetworkStartupFailure, dbCluster.TypeNetwork, int(n.id))
		if err != nil {
			n.logger.Warn("Failed to resolve warning", log.Ctx{"err": err})
		}
	}

	return err
}

func (n *ovn) start() error {
	n.logger.Debug("Start")

	var err error
	var projectID int64
	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		projectID, err = tx.GetProjectID(n.project)
		return err
	})
	if err != nil {
		return errors.Wrapf(err, "Failed getting project ID for project %q", n.project)
	}

	// Ensure network level port group exists.
	err = n.ensureNetworkPortGroup(projectID)
	if err != nil {
		return err
	}

	// Add local node's OVS chassis ID to logical chassis group.
	err = n.addChassisGroupEntry()
	if err != nil {
		return err
	}

	err = n.startUplinkPort()
	if err != nil {
		return err
	}

	return nil
}

// Stop deletes the local OVS uplink port (if unused) and deletes the local OVS chassis ID from the
// OVN chass group
func (n *ovn) Stop() error {
	n.logger.Debug("Stop")

	// Delete local OVS chassis ID from logical OVN HA chassis group.
	err := n.deleteChassisGroupEntry()
	if err != nil {
		return err
	}

	// Delete local uplink port if not used by other OVN networks.
	err = n.deleteUplinkPort()
	if err != nil {
		return err
	}

	return nil
}

// Update updates the network. Accepts notification boolean indicating if this update request is coming from a
// cluster notification, in which case do not update the database, just apply local changes needed.
func (n *ovn) Update(newNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error {
	n.logger.Debug("Update", log.Ctx{"clientType": clientType, "newNetwork": newNetwork})

	err := n.populateAutoConfig(newNetwork.Config)
	if err != nil {
		return errors.Wrapf(err, "Failed generating auto config")
	}

	dbUpdateNeeeded, changedKeys, oldNetwork, err := n.common.configChanged(newNetwork)
	if err != nil {
		return err
	}

	if !dbUpdateNeeeded {
		return nil // Nothing changed.
	}

	// If the network as a whole has not had any previous creation attempts, or the node itself is still
	// pending, then don't apply the new settings to the node, just to the database record (ready for the
	// actual global create request to be initiated).
	if n.Status() == api.NetworkStatusPending || n.LocalStatus() == api.NetworkStatusPending {
		return n.common.update(newNetwork, targetNode, clientType)
	}

	revert := revert.New()
	defer revert.Fail()

	// Define a function which reverts everything.
	revert.Add(func() {
		// Reset changes to all nodes and database.
		n.common.update(oldNetwork, targetNode, clientType)

		// Reset any change that was made to logical network.
		if clientType == request.ClientTypeNormal {
			n.setup(true)
		}

		n.Start()
	})

	// Stop network before new config applied if uplink network is changing.
	if shared.StringInSlice("network", changedKeys) {
		err = n.Stop()
		if err != nil {
			return err
		}

		// Remove volatile keys associated with old network in new config.
		delete(newNetwork.Config, ovnVolatileUplinkIPv4)
		delete(newNetwork.Config, ovnVolatileUplinkIPv6)
	}

	// Apply changes to all nodes and databse.
	err = n.common.update(newNetwork, targetNode, clientType)
	if err != nil {
		return err
	}

	// Re-setup the logical network after config applied if needed.
	if len(changedKeys) > 0 && clientType == request.ClientTypeNormal {
		err = n.setup(true)
		if err != nil {
			return err
		}

		// Work out which ACLs have been added and removed.
		oldACLs := util.SplitNTrimSpace(oldNetwork.Config["security.acls"], ",", -1, true)
		newACLs := util.SplitNTrimSpace(newNetwork.Config["security.acls"], ",", -1, true)
		removedACLs := []string{}
		for _, oldACL := range oldACLs {
			if !shared.StringInSlice(oldACL, newACLs) {
				removedACLs = append(removedACLs, oldACL)
			}
		}

		addedACLs := []string{}
		for _, newACL := range newACLs {
			if !shared.StringInSlice(newACL, oldACLs) {
				addedACLs = append(addedACLs, newACL)
			}
		}

		// Detect if network default rule config has changed.
		defaultRuleKeys := []string{"security.acls.default.ingress.action", "security.acls.default.egress.action", "security.acls.default.ingress.logged", "security.acls.default.egress.logged"}
		changedDefaultRuleKeys := []string{}
		for _, k := range defaultRuleKeys {
			if shared.StringInSlice(k, changedKeys) {
				changedDefaultRuleKeys = append(changedDefaultRuleKeys, k)
			}
		}

		// Apply security ACL and default rule changes.
		if len(addedACLs) > 0 || len(removedACLs) > 0 || len(changedDefaultRuleKeys) > 0 {
			client, err := openvswitch.NewOVN(n.state)
			if err != nil {
				return errors.Wrapf(err, "Failed to get OVN client")
			}

			// Get map of ACL names to DB IDs (used for generating OVN port group names).
			aclNameIDs, err := n.state.Cluster.GetNetworkACLIDsByNames(n.Project())
			if err != nil {
				return errors.Wrapf(err, "Failed getting network ACL IDs for security ACL update")
			}

			addChangeSet := map[openvswitch.OVNPortGroup][]openvswitch.OVNSwitchPortUUID{}
			removeChangeSet := map[openvswitch.OVNPortGroup][]openvswitch.OVNSwitchPortUUID{}

			// Get list of active switch ports (avoids repeated querying of OVN NB).
			activePorts, err := client.LogicalSwitchPorts(n.getIntSwitchName())
			if err != nil {
				return errors.Wrapf(err, "Failed getting active ports")
			}

			// Apply ACL changes to running instance NICs that use this network.
			err = usedByInstanceDevices(n.state, n.project, n.name, func(inst db.Instance, nicName string, nicConfig map[string]string) error {
				nicACLs := util.SplitNTrimSpace(nicConfig["security.acls"], ",", -1, true)

				// Get logical port UUID and name.
				instancePortName := n.getInstanceDevicePortName(inst.Config["volatile.uuid"], nicName)

				portUUID, found := activePorts[instancePortName]
				if !found {
					return nil // No need to update a port that isn't started yet.
				}

				// Check whether we need to add any of the new ACLs to the NIC.
				for _, addedACL := range addedACLs {
					if shared.StringInSlice(addedACL, nicACLs) {
						continue // NIC already has this ACL applied directly, so no need to add.
					}

					aclID, found := aclNameIDs[addedACL]
					if !found {
						return fmt.Errorf("Cannot find security ACL ID for %q", addedACL)
					}

					// Add NIC port to ACL port group.
					portGroupName := acl.OVNACLPortGroupName(aclID)
					acl.OVNPortGroupInstanceNICSchedule(portUUID, addChangeSet, portGroupName)
					n.logger.Debug("Scheduled logical port for ACL port group addition", log.Ctx{"networkACL": addedACL, "portGroup": portGroupName, "port": instancePortName})
				}

				// Check whether we need to remove any of the removed ACLs from the NIC.
				for _, removedACL := range removedACLs {
					if shared.StringInSlice(removedACL, nicACLs) {
						continue // NIC still has this ACL applied directly, so don't remove.
					}

					aclID, found := aclNameIDs[removedACL]
					if !found {
						return fmt.Errorf("Cannot find security ACL ID for %q", removedACL)
					}

					// Remove NIC port from ACL port group.
					portGroupName := acl.OVNACLPortGroupName(aclID)
					acl.OVNPortGroupInstanceNICSchedule(portUUID, removeChangeSet, portGroupName)
					n.logger.Debug("Scheduled logical port for ACL port group removal", log.Ctx{"networkACL": removedACL, "portGroup": portGroupName, "port": instancePortName})
				}

				// If there are no ACLs being applied to the NIC (either from network or NIC) then
				// we should remove the default rule from the NIC.
				if len(newACLs) <= 0 && len(nicACLs) <= 0 {
					err = client.PortGroupPortClearACLRules(acl.OVNIntSwitchPortGroupName(n.ID()), instancePortName)
					if err != nil {
						return errors.Wrapf(err, "Failed clearing OVN default ACL rules for instance NIC")
					}

					n.logger.Debug("Cleared NIC default rules", log.Ctx{"port": instancePortName})
				} else {
					defaultRuleChange := false

					// If there are ACLs being applied, then decide if the default rule config
					// has changed materially for the NIC and update it if needed.
					for _, k := range changedDefaultRuleKeys {
						if _, found := nicConfig[k]; found {
							continue // Skip if changed key is overridden in NIC.
						}

						defaultRuleChange = true
						break
					}

					// If the default rule config has changed materially for this NIC or the
					// network previously didn't have any ACLs applied and now does, then add
					// the default rule to the NIC.
					if defaultRuleChange || len(oldACLs) <= 0 {
						// Set the automatic default ACL rule for the port.
						ingressAction, ingressLogged := n.instanceDeviceACLDefaults(nicConfig, "ingress")
						egressAction, egressLogged := n.instanceDeviceACLDefaults(nicConfig, "egress")

						logPrefix := fmt.Sprintf("%s-%s", inst.Config["volatile.uuid"], nicName)
						err = acl.OVNApplyInstanceNICDefaultRules(client, acl.OVNIntSwitchPortGroupName(n.ID()), logPrefix, instancePortName, ingressAction, ingressLogged, egressAction, egressLogged)
						if err != nil {
							return errors.Wrapf(err, "Failed applying OVN default ACL rules for instance NIC")
						}

						n.logger.Debug("Set NIC default rule", log.Ctx{"port": instancePortName, "ingressAction": ingressAction, "ingressLogged": ingressLogged, "egressAction": egressAction, "egressLogged": egressLogged})
					}
				}

				return nil
			})
			if err != nil {
				return err
			}

			// Apply add/remove changesets.
			if len(addChangeSet) > 0 || len(removeChangeSet) > 0 {
				n.logger.Debug("Applying ACL port group member change sets")
				err = client.PortGroupMemberChange(addChangeSet, removeChangeSet)
				if err != nil {
					return errors.Wrapf(err, "Failed applying OVN port group member change sets for instance NIC")
				}
			}

			// Check if any of the removed ACLs should have any unused port groups deleted.
			if len(removedACLs) > 0 {
				err = acl.OVNPortGroupDeleteIfUnused(n.state, n.logger, client, n.project, &api.Network{Name: n.name}, "", newACLs...)
				if err != nil {
					return errors.Wrapf(err, "Failed removing unused OVN port groups")
				}
			}
		}
	}

	// If uplink network is changing, start network after config applied.
	if shared.StringInSlice("network", changedKeys) {
		err = n.Start()
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// getInstanceDevicePortName returns the switch port name to use for an instance device.
func (n *ovn) getInstanceDevicePortName(instanceUUID string, deviceName string) openvswitch.OVNSwitchPort {
	return openvswitch.OVNSwitchPort(fmt.Sprintf("%s-%s-%s", n.getIntSwitchInstancePortPrefix(), instanceUUID, deviceName))
}

// instanceDevicePortRoutesParse parses the instance NIC device config for internal routes and external routes.
func (n *ovn) instanceDevicePortRoutesParse(deviceConfig map[string]string) ([]*net.IPNet, []*net.IPNet, error) {
	var err error

	internalRoutes := []*net.IPNet{}
	for _, key := range []string{"ipv4.routes", "ipv6.routes"} {
		if deviceConfig[key] == "" {
			continue
		}

		internalRoutes, err = SubnetParseAppend(internalRoutes, util.SplitNTrimSpace(deviceConfig[key], ",", -1, false)...)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "Invalid %q value", key)
		}
	}

	externalRoutes := []*net.IPNet{}
	for _, key := range []string{"ipv4.routes.external", "ipv6.routes.external"} {
		if deviceConfig[key] == "" {
			continue
		}

		externalRoutes, err = SubnetParseAppend(externalRoutes, util.SplitNTrimSpace(deviceConfig[key], ",", -1, false)...)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "Invalid %q value", key)
		}
	}

	return internalRoutes, externalRoutes, nil
}

// InstanceDevicePortValidateExternalRoutes validates the external routes for an OVN instance port.
func (n *ovn) InstanceDevicePortValidateExternalRoutes(deviceInstance instance.Instance, deviceName string, portExternalRoutes []*net.IPNet) error {
	var err error
	var p *db.Project
	var projectNetworks map[string]map[int64]api.Network

	// Get uplink routes.
	_, uplink, _, err := n.state.Cluster.GetNetworkInAnyState(project.Default, n.config["network"])
	if err != nil {
		return errors.Wrapf(err, "Failed to load uplink network %q", n.config["network"])
	}

	uplinkRoutes, err := n.uplinkRoutes(uplink)
	if err != nil {
		return err
	}

	// Check port's external routes are suffciently small when using l2proxy ingress mode on uplink.
	if shared.StringInSlice(uplink.Config["ovn.ingress_mode"], []string{"l2proxy", ""}) {
		for _, portExternalRoute := range portExternalRoutes {
			rOnes, rBits := portExternalRoute.Mask.Size()
			if rBits > 32 && rOnes < 122 {
				return fmt.Errorf("External route %q is too large. Maximum size for IPv6 external route is /122", portExternalRoute.String())
			} else if rOnes < 26 {
				return fmt.Errorf("External route %q is too large. Maximum size for IPv4 external route is /26", portExternalRoute.String())
			}
		}
	}

	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Load the project to get uplink network restrictions.
		p, err = tx.GetProject(n.project)
		if err != nil {
			return errors.Wrapf(err, "Failed to load network restrictions from project %q", n.project)
		}

		// Get all managed networks across all projects.
		projectNetworks, err = tx.GetCreatedNetworks()
		if err != nil {
			return errors.Wrapf(err, "Failed to load all networks")
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Get OVN networks that use the same uplink as us.
	ovnProjectNetworksWithOurUplink := n.ovnProjectNetworksWithUplink(n.config["network"], projectNetworks)

	// Get external subnets used by other OVN networks using our uplink.
	ovnNetworkExternalSubnets, err := n.ovnNetworkExternalSubnets("", "", ovnProjectNetworksWithOurUplink, uplinkRoutes)
	if err != nil {
		return err
	}

	// Get project restricted routes.
	projectRestrictedSubnets, err := n.projectRestrictedSubnets(p, n.config["network"])
	if err != nil {
		return err
	}

	// If validating with an instance, get external routes configured on OVN NICs (excluding ours) using
	// networks that use our uplink. If we are validating a profile and no instance is provided, skip
	// validating OVN NIC overlaps at this stage.
	var ovnNICExternalRoutes []*net.IPNet
	if deviceInstance != nil {
		ovnNICExternalRoutes, err = n.ovnNICExternalRoutes(deviceInstance, deviceName, ovnProjectNetworksWithOurUplink)
		if err != nil {
			return err
		}
	}

	// Check if uplink has routed ingress anycast mode enabled, as this relaxes the overlap checks.
	ipv4UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv4(uplink)
	ipv6UplinkAnycast := n.uplinkHasIngressRoutedAnycastIPv6(uplink)

	for _, portExternalRoute := range portExternalRoutes {
		// Check the external port route is allowed within both the uplink's external routes and any
		// project restricted subnets.
		err = n.validateExternalSubnet(uplinkRoutes, projectRestrictedSubnets, portExternalRoute)
		if err != nil {
			return err
		}

		// Skip overlap checks if the external route's protocol has anycast mode enabled on the uplink.
		if portExternalRoute.IP.To4() == nil {
			if ipv6UplinkAnycast == true {
				continue
			}
		} else if ipv4UplinkAnycast == true {
			continue
		}

		// Check the external port route doesn't fall within any existing OVN network external subnets.
		for _, ovnNetworkExternalSubnet := range ovnNetworkExternalSubnets {
			if SubnetContains(ovnNetworkExternalSubnet, portExternalRoute) || SubnetContains(portExternalRoute, ovnNetworkExternalSubnet) {
				// This error is purposefully vague so that it doesn't reveal any names of
				// resources potentially outside of the NIC's project.
				return fmt.Errorf("External route %q overlaps with another OVN network's external subnet", portExternalRoute.String())
			}
		}

		// Check the external port route doesn't fall within any existing OVN NIC external routes.
		for _, ovnNICExternalRoute := range ovnNICExternalRoutes {
			if SubnetContains(ovnNICExternalRoute, portExternalRoute) || SubnetContains(portExternalRoute, ovnNICExternalRoute) {
				// This error is purposefully vague so that it doesn't reveal any names of
				// resources potentially outside of the NIC's project.
				return fmt.Errorf("External route %q overlaps with another OVN NIC's external route", portExternalRoute.String())
			}
		}
	}

	return nil
}

// InstanceDevicePortAdd sets up an instance device port to the internal logical switch and returns the port name.
// Accepts a list of ACLs being removed from the NIC device (if called as part of a NIC update).
func (n *ovn) InstanceDevicePortSetup(opts *OVNInstanceNICSetupOpts, securityACLsRemove []string) (openvswitch.OVNSwitchPort, error) {
	if opts.InstanceUUID == "" {
		return "", fmt.Errorf("Instance UUID is required")
	}

	mac, err := net.ParseMAC(opts.DeviceConfig["hwaddr"])
	if err != nil {
		return "", err
	}

	ips := []net.IP{}
	for _, key := range []string{"ipv4.address", "ipv6.address"} {
		if opts.DeviceConfig[key] == "" {
			continue
		}

		ip := net.ParseIP(opts.DeviceConfig[key])
		if ip == nil {
			return "", fmt.Errorf("Invalid %s value %q", key, opts.DeviceConfig[key])
		}
		ips = append(ips, ip)
	}

	internalRoutes, externalRoutes, err := n.instanceDevicePortRoutesParse(opts.DeviceConfig)
	if err != nil {
		return "", errors.Wrapf(err, "Failed parsing NIC device routes")
	}

	revert := revert.New()
	defer revert.Fail()

	client, err := openvswitch.NewOVN(n.state)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to get OVN client")
	}

	dhcpv4Subnet := n.DHCPv4Subnet()
	dhcpv6Subnet := n.DHCPv6Subnet()
	var dhcpV4ID, dhcpv6ID openvswitch.OVNDHCPOptionsUUID

	// Get DHCP options IDs.
	if dhcpv4Subnet != nil || dhcpv6Subnet != nil {
		// Find existing DHCP options set for IPv4 and IPv6 and update them instead of adding sets.
		existingOpts, err := client.LogicalSwitchDHCPOptionsGet(n.getIntSwitchName())
		if err != nil {
			return "", errors.Wrapf(err, "Failed getting existing DHCP settings for internal switch")
		}

		if dhcpv4Subnet != nil {
			for _, existingOpt := range existingOpts {
				if existingOpt.CIDR.String() == dhcpv4Subnet.String() {
					if dhcpV4ID != "" {
						return "", fmt.Errorf("Multiple matching DHCP option sets found for switch %q and subnet %q", n.getIntSwitchName(), dhcpv4Subnet.String())
					}

					dhcpV4ID = existingOpt.UUID
				}
			}

			if dhcpV4ID == "" {
				return "", fmt.Errorf("Could not find DHCPv4 options for instance port for subnet %q", dhcpv4Subnet.String())
			}
		}

		if dhcpv6Subnet != nil {
			for _, existingOpt := range existingOpts {
				if existingOpt.CIDR.String() == dhcpv6Subnet.String() {
					if dhcpv6ID != "" {
						return "", fmt.Errorf("Multiple matching DHCP option sets found for switch %q and subnet %q", n.getIntSwitchName(), dhcpv6Subnet.String())
					}

					dhcpv6ID = existingOpt.UUID
				}
			}

			if dhcpv6ID == "" {
				return "", fmt.Errorf("Could not find DHCPv6 options for instance port for subnet %q", dhcpv6Subnet.String())
			}

			// If port isn't going to have fully dynamic IPs allocated by OVN, and instead only static IPv4
			// addresses have been added, then add an EUI64 static IPv6 address so that the switch port has an
			// IPv6 address that will be used to generate a DNS record. This works around a limitation in OVN
			// that prevents us requesting dynamic IPv6 address allocation when static IPv4 allocation is used.
			if len(ips) > 0 {
				hasIPv6 := false
				for _, ip := range ips {
					if ip.To4() == nil {
						hasIPv6 = true
						break
					}
				}

				if !hasIPv6 {
					eui64IP, err := eui64.ParseMAC(dhcpv6Subnet.IP, mac)
					if err != nil {
						return "", errors.Wrapf(err, "Failed generating EUI64 for instance port %q", mac.String())
					}

					// Add EUI64 to list of static IPs for instance port.
					ips = append(ips, eui64IP)
				}
			}
		}
	}

	instancePortName := n.getInstanceDevicePortName(opts.InstanceUUID, opts.DeviceName)

	// Add port with mayExist set to true, so that if instance port exists, we don't fail and continue below
	// to configure the port as needed. This is required in case the OVN northbound database was unavailable
	// when the instance NIC was stopped and was unable to remove the port on last stop, which would otherwise
	// prevent future NIC starts.
	err = client.LogicalSwitchPortAdd(n.getIntSwitchName(), instancePortName, &openvswitch.OVNSwitchPortOpts{
		DHCPv4OptsID: dhcpV4ID,
		DHCPv6OptsID: dhcpv6ID,
		MAC:          mac,
		IPs:          ips,
	}, true)
	if err != nil {
		return "", err
	}

	revert.Add(func() { client.LogicalSwitchPortDelete(instancePortName) })

	// Add DNS records for port's IPs, and retrieve the IP addresses used.
	dnsName := fmt.Sprintf("%s.%s", opts.DNSName, n.getDomainName())
	var dnsUUID openvswitch.OVNDNSUUID
	var dnsIPv4, dnsIPv6 net.IP

	// Retry a few times in case port has not yet allocated dynamic IPs.
	for i := 0; i < 5; i++ {
		dnsUUID, dnsIPv4, dnsIPv6, err = client.LogicalSwitchPortSetDNS(n.getIntSwitchName(), instancePortName, dnsName)
		if err == openvswitch.ErrOVNNoPortIPs {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		break
	}
	if err != nil {
		return "", errors.Wrapf(err, "Failed setting DNS for %q", dnsName)
	}

	revert.Add(func() { client.LogicalSwitchPortDeleteDNS(n.getIntSwitchName(), dnsUUID) })

	// Publish NIC's IPs on uplink network if NAT is disabled and using l2proxy ingress mode on uplink.
	if shared.StringInSlice(opts.UplinkConfig["ovn.ingress_mode"], []string{"l2proxy", ""}) {
		for _, k := range []string{"ipv4.nat", "ipv6.nat"} {
			if shared.IsTrue(n.config[k]) {
				continue
			}

			// Select the correct destination IP from the DNS records.
			var ip net.IP
			if k == "ipv4.nat" {
				ip = dnsIPv4
			} else if k == "ipv6.nat" {
				ip = dnsIPv6
			}

			if ip == nil {
				continue //No qualifying target IP from DNS records.
			}

			err = client.LogicalRouterDNATSNATAdd(n.getRouterName(), ip, ip, true, true)
			if err != nil {
				return "", err
			}

			revert.Add(func() { client.LogicalRouterDNATSNATDelete(n.getRouterName(), ip) })
		}
	}

	// Add each internal route (using the IPs set for DNS as target).
	for _, internalRoute := range internalRoutes {
		targetIP := dnsIPv4
		if internalRoute.IP.To4() == nil {
			targetIP = dnsIPv6
		}

		if targetIP == nil {
			return "", fmt.Errorf("Cannot add static route for %q as target IP is not set", internalRoute.String())
		}

		err = client.LogicalRouterRouteAdd(n.getRouterName(), internalRoute, targetIP, true)
		if err != nil {
			return "", err
		}

		revert.Add(func() { client.LogicalRouterRouteDelete(n.getRouterName(), internalRoute) })
	}

	// Add each external route (using the IPs set for DNS as target).
	for _, externalRoute := range externalRoutes {
		targetIP := dnsIPv4
		if externalRoute.IP.To4() == nil {
			targetIP = dnsIPv6
		}

		if targetIP == nil {
			return "", fmt.Errorf("Cannot add static route for %q as target IP is not set", externalRoute.String())
		}

		err = client.LogicalRouterRouteAdd(n.getRouterName(), externalRoute, targetIP, true)
		if err != nil {
			return "", err
		}

		revert.Add(func() { client.LogicalRouterRouteDelete(n.getRouterName(), externalRoute) })

		// When using l2proxy ingress mode on uplink, in order to advertise the external route to the
		// uplink network using proxy ARP/NDP we need to add a stateless dnat_and_snat rule (as to my
		// knowledge this is the only way to get the OVN router to respond to ARP/NDP requests for IPs that
		// it doesn't actually have). However we have to add each IP in the external route individually as
		// DNAT doesn't support whole subnets.
		if shared.StringInSlice(opts.UplinkConfig["ovn.ingress_mode"], []string{"l2proxy", ""}) {
			err = SubnetIterate(externalRoute, func(ip net.IP) error {
				err = client.LogicalRouterDNATSNATAdd(n.getRouterName(), ip, ip, true, true)
				if err != nil {
					return err
				}

				revert.Add(func() { client.LogicalRouterDNATSNATDelete(n.getRouterName(), ip) })

				return nil
			})
			if err != nil {
				return "", err
			}
		}
	}

	// Merge network and NIC assigned security ACL lists.
	netACLNames := util.SplitNTrimSpace(n.config["security.acls"], ",", -1, true)
	nicACLNames := util.SplitNTrimSpace(opts.DeviceConfig["security.acls"], ",", -1, true)

	for _, aclName := range netACLNames {
		if !shared.StringInSlice(aclName, nicACLNames) {
			nicACLNames = append(nicACLNames, aclName)
		}
	}

	// Apply Security ACL port group settings.
	addChangeSet := map[openvswitch.OVNPortGroup][]openvswitch.OVNSwitchPortUUID{}
	removeChangeSet := map[openvswitch.OVNPortGroup][]openvswitch.OVNSwitchPortUUID{}

	// Get logical port UUID.
	portUUID, err := client.LogicalSwitchPortUUID(instancePortName)
	if err != nil || portUUID == "" {
		return "", errors.Wrapf(err, "Failed getting logical port UUID for security ACL removal")
	}

	// Add NIC port to network port group (this includes the port in the @internal subject for ACL rules).
	acl.OVNPortGroupInstanceNICSchedule(portUUID, addChangeSet, acl.OVNIntSwitchPortGroupName(n.ID()))
	n.logger.Debug("Scheduled logical port for network port group addition", log.Ctx{"portGroup": acl.OVNIntSwitchPortGroupName(n.ID()), "port": instancePortName})

	if len(nicACLNames) > 0 || len(securityACLsRemove) > 0 {
		// Get map of ACL names to DB IDs (used for generating OVN port group names).
		aclNameIDs, err := n.state.Cluster.GetNetworkACLIDsByNames(n.Project())
		if err != nil {
			return "", errors.Wrapf(err, "Failed getting network ACL IDs for security ACL setup")
		}

		// Add port to ACLs requested.
		if len(nicACLNames) > 0 {
			// Request our network is setup with the specified ACLs.
			aclNets := map[string]acl.NetworkACLUsage{
				n.Name(): {Name: n.Name(), Type: n.Type(), ID: n.ID(), Config: n.Config()},
			}

			r, err := acl.OVNEnsureACLs(n.state, n.logger, client, n.Project(), aclNameIDs, aclNets, nicACLNames, false)
			if err != nil {
				return "", errors.Wrapf(err, "Failed ensuring security ACLs are configured in OVN for instance")
			}
			revert.Add(r.Fail)

			for _, aclName := range nicACLNames {
				aclID, found := aclNameIDs[aclName]
				if !found {
					return "", fmt.Errorf("Cannot find security ACL ID for %q", aclName)
				}

				// Add NIC port to ACL port group.
				portGroupName := acl.OVNACLPortGroupName(aclID)
				acl.OVNPortGroupInstanceNICSchedule(portUUID, addChangeSet, portGroupName)
				n.logger.Debug("Scheduled logical port for ACL port group addition", log.Ctx{"networkACL": aclName, "portGroup": portGroupName, "port": instancePortName})
			}
		}

		// Remove port fom ACLs requested.
		for _, aclName := range securityACLsRemove {
			// Don't remove ACLs that are in the add ACLs list (there are possibly added from
			// the network assigned ACLs).
			if shared.StringInSlice(aclName, nicACLNames) {
				continue
			}

			aclID, found := aclNameIDs[aclName]
			if !found {
				return "", fmt.Errorf("Cannot find security ACL ID for %q", aclName)
			}

			// Remove NIC port from ACL port group.
			portGroupName := acl.OVNACLPortGroupName(aclID)
			acl.OVNPortGroupInstanceNICSchedule(portUUID, removeChangeSet, portGroupName)
			n.logger.Debug("Scheduled logical port for ACL port group removal", log.Ctx{"networkACL": aclName, "portGroup": portGroupName, "port": instancePortName})
		}
	}

	// Add instance NIC switch port to port groups required. Always run this as the addChangeSet should always
	// be populated even if no ACLs being applied, because the NIC port needs to be added to the network level
	// port group.
	n.logger.Debug("Applying instance NIC port group member change sets")
	err = client.PortGroupMemberChange(addChangeSet, removeChangeSet)
	if err != nil {
		return "", errors.Wrapf(err, "Failed applying OVN port group member change sets for instance NIC")
	}

	// Set the automatic default ACL rule for the port.
	if len(nicACLNames) > 0 {
		ingressAction, ingressLogged := n.instanceDeviceACLDefaults(opts.DeviceConfig, "ingress")
		egressAction, egressLogged := n.instanceDeviceACLDefaults(opts.DeviceConfig, "egress")

		logPrefix := fmt.Sprintf("%s-%s", opts.InstanceUUID, opts.DeviceName)
		err = acl.OVNApplyInstanceNICDefaultRules(client, acl.OVNIntSwitchPortGroupName(n.ID()), logPrefix, instancePortName, ingressAction, ingressLogged, egressAction, egressLogged)
		if err != nil {
			return "", errors.Wrapf(err, "Failed applying OVN default ACL rules for instance NIC")
		}

		n.logger.Debug("Set NIC default rule", log.Ctx{"port": instancePortName, "ingressAction": ingressAction, "ingressLogged": ingressLogged, "egressAction": egressAction, "egressLogged": egressLogged})
	} else {
		err = client.PortGroupPortClearACLRules(acl.OVNIntSwitchPortGroupName(n.ID()), instancePortName)
		if err != nil {
			return "", errors.Wrapf(err, "Failed clearing OVN default ACL rules for instance NIC")
		}

		n.logger.Debug("Cleared NIC default rule", log.Ctx{"port": instancePortName})
	}

	revert.Success()
	return instancePortName, nil
}

// instanceDeviceACLDefaults returns the action and logging mode to use for the specified direction's default rule.
// If the security.acls.default.{in,e}gress.action or security.acls.default.{in,e}gress.logged settings are not
// specified in the NIC device config, then the settings on the network are used, and if not specified there then
// it returns "reject" and false respectively.
func (n *ovn) instanceDeviceACLDefaults(deviceConfig deviceConfig.Device, direction string) (string, bool) {
	defaults := map[string]string{
		fmt.Sprintf("security.acls.default.%s.action", direction): "reject",
		fmt.Sprintf("security.acls.default.%s.logged", direction): "false",
	}

	for k := range defaults {
		if deviceConfig[k] != "" {
			defaults[k] = deviceConfig[k]
		} else if n.config[k] != "" {
			defaults[k] = n.config[k]
		}
	}

	return defaults[fmt.Sprintf("security.acls.default.%s.action", direction)], shared.IsTrue(defaults[fmt.Sprintf("security.acls.default.%s.logged", direction)])
}

// InstanceDevicePortDynamicIPs returns the dynamically allocated IPs for a device port.
func (n *ovn) InstanceDevicePortDynamicIPs(instanceUUID string, deviceName string) ([]net.IP, error) {
	if instanceUUID == "" {
		return nil, fmt.Errorf("Instance UUID is required")
	}

	instancePortName := n.getInstanceDevicePortName(instanceUUID, deviceName)

	client, err := openvswitch.NewOVN(n.state)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get OVN client")
	}

	return client.LogicalSwitchPortDynamicIPs(instancePortName)
}

// InstanceDevicePortDelete deletes an instance device port from the internal logical switch.
func (n *ovn) InstanceDevicePortDelete(ovsExternalOVNPort openvswitch.OVNSwitchPort, opts *OVNInstanceNICStopOpts) error {
	// Decide whether to use OVS provided OVN port name or internally derived OVN port name.
	instancePortName := ovsExternalOVNPort
	source := "OVS"
	if ovsExternalOVNPort == "" {
		if opts.InstanceUUID == "" {
			return fmt.Errorf("Instance UUID is required")
		}

		instancePortName = n.getInstanceDevicePortName(opts.InstanceUUID, opts.DeviceName)
		source = "internal"
	}

	n.logger.Debug("Deleting instance port", log.Ctx{"port": instancePortName, "source": source})

	internalRoutes, externalRoutes, err := n.instanceDevicePortRoutesParse(opts.DeviceConfig)
	if err != nil {
		return errors.Wrapf(err, "Failed parsing NIC device routes")
	}

	client, err := openvswitch.NewOVN(n.state)
	if err != nil {
		return errors.Wrapf(err, "Failed to get OVN client")
	}

	// Load uplink network config.
	_, uplink, _, err := n.state.Cluster.GetNetworkInAnyState(project.Default, n.config["network"])
	if err != nil {
		return errors.Wrapf(err, "Failed to load uplink network %q", n.config["network"])
	}

	// Get DNS records.
	dnsUUID, _, dnsIPs, err := client.LogicalSwitchPortGetDNS(instancePortName)
	if err != nil {
		return err
	}

	// Cleanup logical switch port and associated config.
	err = client.LogicalSwitchPortCleanup(instancePortName, n.getIntSwitchName(), acl.OVNIntSwitchPortGroupName(n.ID()), dnsUUID)
	if err != nil {
		return err
	}

	removeRoutes := []*net.IPNet{}
	removeNATIPs := []net.IP{}

	// Delete any associated external IP DNAT rules for the DNS IPs.
	if len(dnsIPs) > 0 {
		removeNATIPs = append(removeNATIPs, dnsIPs...)
	}

	// Delete internal routes.
	if len(internalRoutes) > 0 {
		removeRoutes = append(removeRoutes, internalRoutes...)
	}

	// Delete external routes.
	for _, externalRoute := range externalRoutes {
		removeRoutes = append(removeRoutes, externalRoute)

		// Remove the DNAT rules when using l2proxy ingress mode on uplink.
		if shared.StringInSlice(uplink.Config["ovn.ingress_mode"], []string{"l2proxy", ""}) {
			err = SubnetIterate(externalRoute, func(ip net.IP) error {
				removeNATIPs = append(removeNATIPs, ip)

				return nil
			})
			if err != nil {
				return err
			}
		}
	}

	if len(removeRoutes) > 0 {
		err = client.LogicalRouterRouteDelete(n.getRouterName(), removeRoutes...)
		if err != nil {
			return err
		}
	}

	if len(removeNATIPs) > 0 {
		err = client.LogicalRouterDNATSNATDelete(n.getRouterName(), removeNATIPs...)
		if err != nil {
			return err
		}
	}

	return nil
}

// DHCPv4Subnet returns the DHCPv4 subnet (if DHCP is enabled on network).
func (n *ovn) DHCPv4Subnet() *net.IPNet {
	// DHCP is disabled on this network (an empty ipv4.dhcp setting indicates enabled by default).
	if n.config["ipv4.dhcp"] != "" && !shared.IsTrue(n.config["ipv4.dhcp"]) {
		return nil
	}

	_, subnet, err := net.ParseCIDR(n.getRouterIntPortIPv4Net())
	if err != nil {
		return nil
	}

	return subnet
}

// DHCPv6Subnet returns the DHCPv6 subnet (if DHCP or SLAAC is enabled on network).
func (n *ovn) DHCPv6Subnet() *net.IPNet {
	// DHCP is disabled on this network (an empty ipv6.dhcp setting indicates enabled by default).
	if n.config["ipv6.dhcp"] != "" && !shared.IsTrue(n.config["ipv6.dhcp"]) {
		return nil
	}

	_, subnet, err := net.ParseCIDR(n.getRouterIntPortIPv6Net())
	if err != nil {
		return nil
	}

	ones, _ := subnet.Mask.Size()
	if ones < 64 {
		return nil // OVN only supports DHCPv6 allocated using EUI64 (which requires at least a /64).
	}

	return subnet
}

// ovnNetworkExternalSubnets returns a list of external subnets used by OVN networks (optionally exluding our own
// if both ourProject and ourNetwork are non-empty) using the same uplink as this OVN network. OVN networks are
// considered to be using external subnets if their ipv4.address and/or ipv6.address are in the uplink's external
// routes and the associated NAT is disabled for the IP family.
func (n *ovn) ovnNetworkExternalSubnets(ourProject string, ourNetwork string, ovnProjectNetworksWithOurUplink map[string][]*api.Network, uplinkRoutes []*net.IPNet) ([]*net.IPNet, error) {
	externalSubnets := make([]*net.IPNet, 0)
	for netProject, networks := range ovnProjectNetworksWithOurUplink {
		for _, netInfo := range networks {
			if netProject == ourProject && netInfo.Name == ourNetwork {
				continue
			}

			for _, keyPrefix := range []string{"ipv4", "ipv6"} {
				if !shared.IsTrue(netInfo.Config[fmt.Sprintf("%s.nat", keyPrefix)]) {
					_, ipNet, _ := net.ParseCIDR(netInfo.Config[fmt.Sprintf("%s.address", keyPrefix)])
					if ipNet == nil {
						// If the network doesn't have a valid IP, skip it.
						continue
					}

					// Check the network's subnet is a valid external route on uplink.
					err := n.validateExternalSubnet(uplinkRoutes, nil, ipNet)
					if err != nil {
						return nil, errors.Wrapf(err, "Failed checking if OVN network external subnet %q is valid external route on uplink %q", ipNet.String(), n.config["network"])
					}

					externalSubnets = append(externalSubnets, ipNet)
				}
			}
		}
	}

	return externalSubnets, nil
}

// ovnNICExternalRoutes returns a list of external routes currently used by OVN NICs (optionally excluding our
// own if both ourDeviceInstance and ourDeviceName are non-empty) that are connected to OVN networks that share
// the same uplink as this network uses.
func (n *ovn) ovnNICExternalRoutes(ourDeviceInstance instance.Instance, ourDeviceName string, ovnProjectNetworksWithOurUplink map[string][]*api.Network) ([]*net.IPNet, error) {
	externalRoutes := make([]*net.IPNet, 0)

	// nicUsesNetwork returns true if the nicDev's "network" property matches one of the projectNetworks names
	// and the instNetworkProject matches the projectNetworks's project. As we only use network name and
	// project to match we rely on projectNetworks only including OVN networks that use our uplink.
	nicUsesNetwork := func(instNetworkProject string, nicDev map[string]string, projectNetworks map[string][]*api.Network) bool {
		for netProject, networks := range projectNetworks {
			for _, network := range networks {
				if netProject == instNetworkProject && network.Name == nicDev["network"] {
					return true
				}
			}
		}

		return false
	}

	err := n.state.Cluster.InstanceList(nil, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
		// Get the instance's effective network project name.
		instNetworkProject := project.NetworkProjectFromRecord(&p)
		devices := db.ExpandInstanceDevices(deviceConfig.NewDevices(inst.Devices), profiles)

		// Iterate through each of the instance's devices, looking for OVN NICs that are linked to networks
		// that use our uplink.
		for devName, devConfig := range devices {
			if devConfig["type"] != "nic" {
				continue
			}

			// Skip our own device (if instance and device name were supplied).
			if ourDeviceInstance != nil && ourDeviceName != "" && inst.Name == ourDeviceInstance.Name() && inst.Project == ourDeviceInstance.Project() && ourDeviceName == devName {
				continue
			}

			// Check whether the NIC device references one of the OVN networks supplied.
			if !nicUsesNetwork(instNetworkProject, devConfig, ovnProjectNetworksWithOurUplink) {
				continue
			}

			// For OVN NICs that are connected to networks that use the same uplink as we do, check
			// if they have any external routes configured, and if so add them to the list to return.
			for _, keyPrefix := range []string{"ipv4", "ipv6"} {
				_, ipNet, _ := net.ParseCIDR(devConfig[fmt.Sprintf("%s.routes.external", keyPrefix)])
				if ipNet == nil {
					// If the NIC device doesn't have a valid external route setting, skip.
					continue
				}

				externalRoutes = append(externalRoutes, ipNet)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return externalRoutes, nil
}

// ovnProjectNetworksWithUplink accepts a map of all networks in all projects and returns a filtered map of OVN
// networks that use the uplink specified.
func (n *ovn) ovnProjectNetworksWithUplink(uplink string, projectNetworks map[string]map[int64]api.Network) map[string][]*api.Network {
	ovnProjectNetworksWithOurUplink := make(map[string][]*api.Network)
	for netProject, networks := range projectNetworks {
		for _, ni := range networks {
			network := ni // Local var creating pointer to rather than iterator.

			// Skip non-OVN networks or those networks that don't use the uplink specified.
			if network.Type != "ovn" || network.Config["network"] != uplink {
				continue
			}

			if ovnProjectNetworksWithOurUplink[netProject] == nil {
				ovnProjectNetworksWithOurUplink[netProject] = []*api.Network{&network}
			} else {
				ovnProjectNetworksWithOurUplink[netProject] = append(ovnProjectNetworksWithOurUplink[netProject], &network)
			}
		}
	}

	return ovnProjectNetworksWithOurUplink
}

// uplinkHasIngressRoutedAnycastIPv4 returns true if the uplink network has IPv4 routed ingress anycast enabled.
func (n *ovn) uplinkHasIngressRoutedAnycastIPv4(uplink *api.Network) bool {
	return shared.IsTrue(uplink.Config["ipv4.routes.anycast"]) && uplink.Config["ovn.ingress_mode"] == "routed"
}

// uplinkHasIngressRoutedAnycastIPv6 returns true if the uplink network has routed IPv6 ingress anycast enabled.
func (n *ovn) uplinkHasIngressRoutedAnycastIPv6(uplink *api.Network) bool {
	return shared.IsTrue(uplink.Config["ipv6.routes.anycast"]) && uplink.Config["ovn.ingress_mode"] == "routed"
}

// handleDependencyChange applies changes from uplink network if specific watched keys have changed.
func (n *ovn) handleDependencyChange(uplinkName string, uplinkConfig map[string]string, changedKeys []string) error {
	// Detect changes that need to be applied to the network.
	for _, k := range []string{"dns.nameservers"} {
		if shared.StringInSlice(k, changedKeys) {
			n.logger.Debug("Applying changes from uplink network", log.Ctx{"uplink": uplinkName})

			// Re-setup logical network in order to apply uplink changes.
			err := n.setup(true)
			if err != nil {
				return err
			}

			break // Only run setup once per notification (all changes will be applied).
		}
	}

	// Add or remove the instance NIC l2proxy DNAT_AND_SNAT rules if uplink's ovn.ingress_mode has changed.
	if shared.StringInSlice("ovn.ingress_mode", changedKeys) {
		n.logger.Debug("Applying ingress mode changes from uplink network to instance NICs", log.Ctx{"uplink": uplinkName})

		client, err := openvswitch.NewOVN(n.state)
		if err != nil {
			return errors.Wrapf(err, "Failed to get OVN client")
		}

		if shared.StringInSlice(uplinkConfig["ovn.ingress_mode"], []string{"l2proxy", ""}) {
			// Get list of active switch ports (avoids repeated querying of OVN NB).
			activePorts, err := client.LogicalSwitchPorts(n.getIntSwitchName())
			if err != nil {
				return errors.Wrapf(err, "Failed getting active ports")
			}

			// Find all instance NICs that use this network, and re-add the logical OVN instance port.
			// This will restore the l2proxy DNAT_AND_SNAT rules.
			err = n.state.Cluster.InstanceList(nil, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
				// Get the instance's effective network project name.
				instNetworkProject := project.NetworkProjectFromRecord(&p)

				// Skip instances who's effective network project doesn't match this network's
				// project.
				if n.Project() != instNetworkProject {
					return nil
				}

				devices := db.ExpandInstanceDevices(deviceConfig.NewDevices(inst.Devices), profiles)

				// Iterate through each of the instance's devices, looking for NICs that are linked
				// this network.
				for devName, devConfig := range devices {
					if devConfig["type"] != "nic" || n.Name() != devConfig["network"] {
						continue
					}

					// Check if instance port exists, if not then we can skip.
					instanceUUID := inst.Config["volatile.uuid"]
					instancePortName := n.getInstanceDevicePortName(instanceUUID, devName)
					_, found := activePorts[instancePortName]
					if !found {
						continue // No need to update a port that isn't started yet.
					}

					if devConfig["hwaddr"] == "" {
						// Load volatile MAC if no static MAC specified.
						devConfig["hwaddr"] = inst.Config[fmt.Sprintf("volatile.%s.hwaddr", devName)]
					}

					// Re-add logical switch port to apply the l2proxy DNAT_AND_SNAT rules.
					n.logger.Debug("Re-adding instance OVN NIC port to apply ingress mode changes", log.Ctx{"project": inst.Project, "instance": inst.Name, "device": devName})
					_, err = n.InstanceDevicePortSetup(&OVNInstanceNICSetupOpts{
						InstanceUUID: instanceUUID,
						DNSName:      inst.Name,
						DeviceName:   devName,
						DeviceConfig: devConfig,
						UplinkConfig: uplinkConfig,
					}, nil)
					if err != nil {
						n.logger.Error("Failed re-adding instance OVN NIC port", log.Ctx{"project": inst.Project, "instance": inst.Name, "err": err})
						continue
					}
				}

				return nil
			})
			if err != nil {
				return errors.Wrapf(err, "Failed adding instance NIC ingress mode l2proxy rules")
			}
		} else {
			// Remove all DNAT_AND_SNAT rules if not using l2proxy ingress mode, as currently we only
			// use DNAT_AND_SNAT rules for this feature so it is safe to do.
			err = client.LogicalRouterDNATSNATDeleteAll(n.getRouterName())
			if err != nil {
				return errors.Wrapf(err, "Failed deleting instance NIC ingress mode l2proxy rules")
			}
		}
	}

	return nil
}
