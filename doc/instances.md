# Instance configuration
## Properties
The following are direct instance properties and can't be part of a profile:

 - `name`
 - `architecture`

Name is the instance name and can only be changed by renaming the instance.

Valid instance names must:

 - Be between 1 and 63 characters long
 - Be made up exclusively of letters, numbers and dashes from the ASCII table
 - Not start with a digit or a dash
 - Not end with a dash

This requirement is so that the instance name may properly be used in
DNS records, on the filesystem, in various security profiles as well as
the hostname of the instance itself.

## Key/value configuration
The key/value configuration is namespaced with the following namespaces
currently supported:

 - `boot` (boot related options, timing, dependencies, ...)
 - `environment` (environment variables)
 - `image` (copy of the image properties at time of creation)
 - `limits` (resource limits)
 - `nvidia` (NVIDIA and CUDA configuration)
 - `raw` (raw instance configuration overrides)
 - `security` (security policies)
 - `user` (storage for user properties, searchable)
 - `volatile` (used internally by LXD to store internal data specific to an instance)

The currently supported keys are:

Key                                         | Type      | Default           | Live update   | Condition                 | Description
:--                                         | :---      | :------           | :----------   | :----------               | :----------
boot.autostart                              | boolean   | -                 | n/a           | -                         | Always start the instance when LXD starts (if not set, restore last state)
boot.autostart.delay                        | integer   | 0                 | n/a           | -                         | Number of seconds to wait after the instance started before starting the next one
boot.autostart.priority                     | integer   | 0                 | n/a           | -                         | What order to start the instances in (starting with highest)
boot.host\_shutdown\_timeout                | integer   | 30                | yes           | -                         | Seconds to wait for instance to shutdown before it is force stopped
boot.stop.priority                          | integer   | 0                 | n/a           | -                         | What order to shutdown the instances (starting with highest)
cluster.evacuate                            | string    | auto              | n/a           | -                         | What to do when evacuating the instance (auto, migrate, or stop)
environment.\*                              | string    | -                 | yes (exec)    | -                         | key/value environment variables to export to the instance and set on exec
limits.cpu                                  | string    | - (all)           | yes           | -                         | Number or range of CPUs to expose to the instance
limits.cpu.allowance                        | string    | 100%              | yes           | container                 | How much of the CPU can be used. Can be a percentage (e.g. 50%) for a soft limit or hard a chunk of time (25ms/100ms)
limits.cpu.priority                         | integer   | 10 (maximum)      | yes           | container                 | CPU scheduling priority compared to other instances sharing the same CPUs (overcommit) (integer between 0 and 10)
limits.disk.priority                        | integer   | 5 (medium)        | yes           | -                         | When under load, how much priority to give to the instance's I/O requests (integer between 0 and 10)
limits.hugepages.64KB                       | string    | -                 | yes           | container                 | Fixed value in bytes (various suffixes supported, see below) to limit number of 64 KB hugepages (Available hugepage sizes are architecture dependent.)
limits.hugepages.1MB                        | string    | -                 | yes           | container                 | Fixed value in bytes (various suffixes supported, see below) to limit number of 1 MB hugepages (Available hugepage sizes are architecture dependent.)
limits.hugepages.2MB                        | string    | -                 | yes           | container                 | Fixed value in bytes (various suffixes supported, see below) to limit number of 2 MB hugepages (Available hugepage sizes are architecture dependent.)
limits.hugepages.1GB                        | string    | -                 | yes           | container                 | Fixed value in bytes (various suffixes supported, see below) to limit number of 1 GB hugepages (Available hugepage sizes are architecture dependent.)
limits.kernel.\*                            | string    | -                 | no            | container                 | This limits kernel resources per instance (e.g. number of open files)
limits.memory                               | string    | - (all)           | yes           | -                         | Percentage of the host's memory or fixed value in bytes (various suffixes supported, see below)
limits.memory.enforce                       | string    | hard              | yes           | container                 | If hard, instance can't exceed its memory limit. If soft, the instance can exceed its memory limit when extra host memory is available
limits.memory.hugepages                     | boolean   | false             | no            | virtual-machine           | Controls whether to back the instance using hugepages rather than regular system memory
limits.memory.swap                          | boolean   | true              | yes           | container                 | Controls whether to encourage/discourage swapping less used pages for this instance
limits.memory.swap.priority                 | integer   | 10 (maximum)      | yes           | container                 | The higher this is set, the least likely the instance is to be swapped to disk (integer between 0 and 10)
limits.network.priority                     | integer   | 0 (minimum)       | yes           | -                         | When under load, how much priority to give to the instance's network requests (integer between 0 and 10)
limits.processes                            | integer   | - (max)           | yes           | container                 | Maximum number of processes that can run in the instance
linux.kernel\_modules                       | string    | -                 | yes           | container                 | Comma separated list of kernel modules to load before starting the instance
migration.incremental.memory                | boolean   | false             | yes           | container                 | Incremental memory transfer of the instance's memory to reduce downtime
migration.incremental.memory.goal           | integer   | 70                | yes           | container                 | Percentage of memory to have in sync before stopping the instance
migration.incremental.memory.iterations     | integer   | 10                | yes           | container                 | Maximum number of transfer operations to go through before stopping the instance
migration.stateful                          | boolean   | false             | no            | virtual-machine           | Allow for stateful stop/start and snapshots. This will prevent the use of some features that are incompatible with it
nvidia.driver.capabilities                  | string    | compute,utility   | no            | container                 | What driver capabilities the instance needs (sets libnvidia-container NVIDIA\_DRIVER\_CAPABILITIES)
nvidia.runtime                              | boolean   | false             | no            | container                 | Pass the host NVIDIA and CUDA runtime libraries into the instance
nvidia.require.cuda                         | string    | -                 | no            | container                 | Version expression for the required CUDA version (sets libnvidia-container NVIDIA\_REQUIRE\_CUDA)
nvidia.require.driver                       | string    | -                 | no            | container                 | Version expression for the required driver version (sets libnvidia-container NVIDIA\_REQUIRE\_DRIVER)
raw.apparmor                                | blob      | -                 | yes           | -                         | Apparmor profile entries to be appended to the generated profile
raw.idmap                                   | blob      | -                 | no            | unprivileged container    | Raw idmap configuration (e.g. "both 1000 1000")
raw.lxc                                     | blob      | -                 | no            | container                 | Raw LXC configuration to be appended to the generated one
raw.qemu                                    | blob      | -                 | no            | virtual-machine           | Raw Qemu configuration to be appended to the generated command line
raw.seccomp                                 | blob      | -                 | no            | container                 | Raw Seccomp configuration
security.devlxd                             | boolean   | true              | no            | container                 | Controls the presence of /dev/lxd in the instance
security.devlxd.images                      | boolean   | false             | no            | container                 | Controls the availability of the /1.0/images API over devlxd
security.idmap.base                         | integer   | -                 | no            | unprivileged container    | The base host ID to use for the allocation (overrides auto-detection)
security.idmap.isolated                     | boolean   | false             | no            | unprivileged container    | Use an idmap for this instance that is unique among instances with isolated set
security.idmap.size                         | integer   | -                 | no            | unprivileged container    | The size of the idmap to use
security.nesting                            | boolean   | false             | yes           | container                 | Support running lxd (nested) inside the instance
security.privileged                         | boolean   | false             | no            | container                 | Runs the instance in privileged mode
security.protection.delete                  | boolean   | false             | yes           | -                         | Prevents the instance from being deleted
security.protection.shift                   | boolean   | false             | yes           | container                 | Prevents the instance's filesystem from being uid/gid shifted on startup
security.secureboot                         | boolean   | true              | no            | virtual-machine           | Controls whether UEFI secure boot is enabled with the default Microsoft keys
security.syscalls.allow                     | string    | -                 | no            | container                 | A '\n' separated list of syscalls to allow (mutually exclusive with security.syscalls.deny\*)
security.syscalls.deny                      | string    | -                 | no            | container                 | A '\n' separated list of syscalls to deny
security.syscalls.deny\_compat              | boolean   | false             | no            | container                 | On x86\_64 this enables blocking of compat\_\* syscalls, it is a no-op on other arches
security.syscalls.deny\_default             | boolean   | true              | no            | container                 | Enables the default syscall deny
security.syscalls.intercept.bpf             | boolean   | false             | no            | container                 | Handles the `bpf` system call
security.syscalls.intercept.bpf.devices     | boolean   | false             | no            | container                 | Allows `bpf` programs for the devices cgroup in the unified hierarchy to be loaded.
security.syscalls.intercept.mknod           | boolean   | false             | no            | container                 | Handles the `mknod` and `mknodat` system calls (allows creation of a limited subset of char/block devices)
security.syscalls.intercept.mount           | boolean   | false             | no            | container                 | Handles the `mount` system call
security.syscalls.intercept.mount.allowed   | string    | -                 | yes           | container                 | Specify a comma-separated list of filesystems that are safe to mount for processes inside the instance
security.syscalls.intercept.mount.fuse      | string    | -                 | yes           | container                 | Whether to redirect mounts of a given filesystem to their fuse implemenation (e.g. ext4=fuse2fs)
security.syscalls.intercept.mount.shift     | boolean   | false             | yes           | container                 | Whether to mount shiftfs on top of filesystems handled through mount syscall interception
security.syscalls.intercept.setxattr        | boolean   | false             | no            | container                 | Handles the `setxattr` system call (allows setting a limited subset of restricted extended attributes)
snapshots.schedule                          | string    | -                 | no            | -                         | Cron expression (`<minute> <hour> <dom> <month> <dow>`), or a comma separated list of schedule aliases `<@hourly> <@daily> <@midnight> <@weekly> <@monthly> <@annually> <@yearly> <@startup>`
snapshots.schedule.stopped                  | bool      | false             | no            | -                         | Controls whether or not stopped instances are to be snapshoted automatically
snapshots.pattern                           | string    | snap%d            | no            | -                         | Pongo2 template string which represents the snapshot name (used for scheduled snapshots and unnamed snapshots)
snapshots.expiry                            | string    | -                 | no            | -                         | Controls when snapshots are to be deleted (expects expression like `1M 2H 3d 4w 5m 6y`)
user.\*                                     | string    | -                 | n/a           | -                         | Free form user key/value storage (can be used in search)

The following volatile keys are currently internally used by LXD:

Key                                         | Type      | Default       | Description
:--                                         | :---      | :------       | :----------
volatile.apply\_template                    | string    | -             | The name of a template hook which should be triggered upon next startup
volatile.base\_image                        | string    | -             | The hash of the image the instance was created from, if any
volatile.evacuate.origin                    | string    | -             | The origin (cluster member) of the evacuated instance
volatile.idmap.base                         | integer   | -             | The first id in the instance's primary idmap range
volatile.idmap.current                      | string    | -             | The idmap currently in use by the instance
volatile.idmap.next                         | string    | -             | The idmap to use next time the instance starts
volatile.last\_state.idmap                  | string    | -             | Serialized instance uid/gid map
volatile.last\_state.power                  | string    | -             | Instance state as of last host shutdown
volatile.vsock\_id                          | string    | -             | Instance vsock ID used as of last start
volatile.uuid                               | string    | -             | Instance UUID
volatile.\<name\>.apply\_quota              | string    | -             | Disk quota to be applied on next instance start
volatile.\<name\>.ceph\_rbd                 | string    | -             | RBD device path for Ceph disk devices
volatile.\<name\>.host\_name                | string    | -             | Network device name on the host
volatile.\<name\>.hwaddr                    | string    | -             | Network device MAC address (when no hwaddr property is set on the device itself)
volatile.\<name\>.last\_state.created       | string    | -             | Whether or not the network device physical device was created ("true" or "false")
volatile.\<name\>.last\_state.mtu           | string    | -             | Network device original MTU used when moving a physical device into an instance
volatile.\<name\>.last\_state.hwaddr        | string    | -             | Network device original MAC used when moving a physical device into an instance
volatile.\<name\>.last\_state.vf.id         | string    | -             | SR-IOV Virtual function ID used when moving a VF into an instance
volatile.\<name\>.last\_state.vf.hwaddr     | string    | -             | SR-IOV Virtual function original MAC used when moving a VF into an instance
volatile.\<name\>.last\_state.vf.vlan       | string    | -             | SR-IOV Virtual function original VLAN used when moving a VF into an instance
volatile.\<name\>.last\_state.vf.spoofcheck | string    | -             | SR-IOV Virtual function original spoof check setting used when moving a VF into an instance

Additionally, those user keys have become common with images (support isn't guaranteed):

Key                         | Type          | Default           | Description
:--                         | :---          | :------           | :----------
user.meta-data              | string        | -                 | Cloud-init meta-data, content is appended to seed value
user.network-config         | string        | DHCP on eth0      | Cloud-init network-config, content is used as seed value
user.network\_mode          | string        | dhcp              | One of "dhcp" or "link-local". Used to configure network in supported images
user.user-data              | string        | #!cloud-config    | Cloud-init user-data, content is used as seed value
user.vendor-data            | string        | #!cloud-config    | Cloud-init vendor-data, content is used as seed value

Note that while a type is defined above as a convenience, all values are
stored as strings and should be exported over the REST API as strings
(which makes it possible to support any extra values without breaking
backward compatibility).

Those keys can be set using the lxc tool with:

```bash
lxc config set <instance> <key> <value>
```

Volatile keys can't be set by the user and can only be set directly against an instance.

The raw keys allow direct interaction with the backend features that LXD
itself uses, setting those may very well break LXD in non-obvious ways
and should whenever possible be avoided.

### CPU limits
The CPU limits are implemented through a mix of the `cpuset` and `cpu` CGroup controllers.

`limits.cpu` results in CPU pinning through the `cpuset` controller.
A set of CPUs (e.g. `1,2,3`) or a CPU range (e.g. `0-3`) can be specified.

When a number of CPUs is specified instead (e.g. `4`), LXD will do
dynamic load-balancing of all instances that aren't pinned to specific
CPUs, trying to spread the load on the machine. Instances will then be
re-balanced every time an instance starts or stops as well as whenever a
CPU is added to the system.

To pin to a single CPU, you have to use the range syntax (e.g. `1-1`) to
differentiate it from a number of CPUs.

`limits.cpu.allowance` drives either the CFS scheduler quotas when
passed a time constraint, or the generic CPU shares mechanism when
passed a percentage value.

The time constraint (e.g. `20ms/50ms`) is relative to one CPU worth of
time, so to restrict to two CPUs worth of time, something like
100ms/50ms should be used.

When using a percentage value, the limit will only be applied when under
load and will be used to calculate the scheduler priority for the
instance, relative to any other instance which is using the same CPU(s).

`limits.cpu.priority` is another knob which is used to compute that
scheduler priority score when a number of instances sharing a set of
CPUs have the same percentage of CPU assigned to them.

# Devices configuration
LXD will always provide the instance with the basic devices which are required
for a standard POSIX system to work. These aren't visible in instance or
profile configuration and may not be overridden.

Those include:

 - `/dev/null` (character device)
 - `/dev/zero` (character device)
 - `/dev/full` (character device)
 - `/dev/console` (character device)
 - `/dev/tty` (character device)
 - `/dev/random` (character device)
 - `/dev/urandom` (character device)
 - `/dev/net/tun` (character device)
 - `/dev/fuse` (character device)
 - `lo` (network interface)

Anything else has to be defined in the instance configuration or in one of its
profiles. The default profile will typically contain a network interface to
become `eth0` in the instance.

To add extra devices to an instance, device entries can be added directly to an
instance, or to a profile.

Devices may be added or removed while the instance is running.

Every device entry is identified by a unique name. If the same name is used in
a subsequent profile or in the instance's own configuration, the whole entry
is overridden by the new definition.

Device entries are added to an instance through:

```bash
lxc config device add <instance> <name> <type> [key=value]...
```

or to a profile with:

```bash
lxc profile device add <profile> <name> <type> [key=value]...
```

## Device types
LXD supports the following device types:

ID (database)   | Name                               | Condition     | Description
:--             | :--                                | :--           | :--
0               | [none](#type-none)                 | -             | Inheritance blocker
1               | [nic](#type-nic)                   | -             | Network interface
2               | [disk](#type-disk)                 | -             | Mountpoint inside the instance
3               | [unix-char](#type-unix-char)       | container     | Unix character device
4               | [unix-block](#type-unix-block)     | container     | Unix block device
5               | [usb](#type-usb)                   | -             | USB device
6               | [gpu](#type-gpu)                   | -             | GPU device
7               | [infiniband](#type-infiniband)     | container     | Infiniband device
8               | [proxy](#type-proxy)               | container     | Proxy device
9               | [unix-hotplug](#type-unix-hotplug) | container     | Unix hotplug device
10              | [tpm](#type-tpm)                   | -             | TPM device
11              | [pci](#type-pci)                   | VM            | PCI device

### Type: none

Supported instance types: container, VM

A none type device doesn't have any property and doesn't create anything inside the instance.

It's only purpose it to stop inheritance of devices coming from profiles.

To do so, just add a none type device with the same name of the one you wish to skip inheriting.
It can be added in a profile being applied after the profile it originated from or directly on the instance.

### Type: nic
LXD supports several different kinds of network devices (referred to as Network Interface Controller or NIC).

When adding a network device to an instance, there are two ways to specify the type of device you want to add;
either by specifying the `nictype` property or using the `network` property.

#### Specifying a NIC using the `network` property

When specifying the `network` property, the NIC is linked to an existing managed network and the `nictype` is
automatically detected based on the network's type.

Some of the NICs properties are inherited from the network rather than being customisable for each NIC.

These are detailed in the "Managed" column in the NIC specific sections below.

#### NICs Available:

See the NIC's settings below for details about which properties are available.

The following NICs can be specified using the `nictype` or `network` properties:

 - [bridged](#nic-bridged): Uses an existing bridge on the host and creates a virtual device pair to connect the host bridge to the instance.
 - [macvlan](#nic-macvlan): Sets up a new network device based on an existing one but using a different MAC address.
 - [sriov](#nic-sriov): Passes a virtual function of an SR-IOV enabled physical network device into the instance.

The following NICs can be specified using only the `network` property:

 - [ovn](#nic-ovn): Uses an existing OVN network and creates a virtual device pair to connect the instance to it.

The following NICs can be specified using only the `nictype` property:

 - [physical](#nic-physical): Straight physical device passthrough from the host. The targeted device will vanish from the host and appear in the instance.
 - [ipvlan](#nic-ipvlan): Sets up a new network device based on an existing one using the same MAC address but a different IP.
 - [p2p](#nic-p2p): Creates a virtual device pair, putting one side in the instance and leaving the other side on the host.
 - [routed](#nic-routed): Creates a virtual device pair to connect the host to the instance and sets up static routes and proxy ARP/NDP entries to allow the instance to join the network of a designated parent interface.

#### nic: bridged

Supported instance types: container, VM

Selected using: `nictype`, `network`

Uses an existing bridge on the host and creates a virtual device pair to connect the host bridge to the instance.

Device configuration properties:

Key                      | Type    | Default           | Required | Managed | Description
:--                      | :--     | :--               | :--      | :--     | :--
parent                   | string  | -                 | yes      | yes     | The name of the host device
network                  | string  | -                 | yes      | no      | The LXD network to link device to (instead of parent)
name                     | string  | kernel assigned   | no       | no      | The name of the interface inside the instance
mtu                      | integer | parent MTU        | no       | yes     | The MTU of the new interface
hwaddr                   | string  | randomly assigned | no       | no      | The MAC address of the new interface
host\_name               | string  | randomly assigned | no       | no      | The name of the interface inside the host
limits.ingress           | string  | -                 | no       | no      | I/O limit in bit/s for incoming traffic (various suffixes supported, see below)
limits.egress            | string  | -                 | no       | no      | I/O limit in bit/s for outgoing traffic (various suffixes supported, see below)
limits.max               | string  | -                 | no       | no      | Same as modifying both limits.ingress and limits.egress
ipv4.address             | string  | -                 | no       | no      | An IPv4 address to assign to the instance through DHCP
ipv6.address             | string  | -                 | no       | no      | An IPv6 address to assign to the instance through DHCP
ipv4.routes              | string  | -                 | no       | no      | Comma delimited list of IPv4 static routes to add on host to nic
ipv6.routes              | string  | -                 | no       | no      | Comma delimited list of IPv6 static routes to add on host to nic
security.mac\_filtering  | boolean | false             | no       | no      | Prevent the instance from spoofing another's MAC address
security.ipv4\_filtering | boolean | false             | no       | no      | Prevent the instance from spoofing another's IPv4 address (enables mac\_filtering)
security.ipv6\_filtering | boolean | false             | no       | no      | Prevent the instance from spoofing another's IPv6 address (enables mac\_filtering)
maas.subnet.ipv4         | string  | -                 | no       | yes     | MAAS IPv4 subnet to register the instance in
maas.subnet.ipv6         | string  | -                 | no       | yes     | MAAS IPv6 subnet to register the instance in
boot.priority            | integer | -                 | no       | no      | Boot priority for VMs (higher boots first)
vlan                     | integer | -                 | no       | no      | The VLAN ID to use for untagged traffic (Can be `none` to remove port from default VLAN)
vlan.tagged              | integer | -                 | no       | no      | Comma delimited list of VLAN IDs to join for tagged traffic
security.port\_isolation | boolean | false             | no       | no      | Prevent the NIC from communicating with other NICs in the network that have port isolation enabled

#### nic: macvlan

Supported instance types: container, VM

Selected using: `nictype`, `network`

Sets up a new network device based on an existing one but using a different MAC address.

Device configuration properties:

Key                     | Type    | Default           | Required | Managed | Description
:--                     | :--     | :--               | :--      | :--     | :--
parent                  | string  | -                 | yes      | yes     | The name of the host device
network                 | string  | -                 | yes      | no      | The LXD network to link device to (instead of parent)
name                    | string  | kernel assigned   | no       | no      | The name of the interface inside the instance
mtu                     | integer | parent MTU        | no       | yes     | The MTU of the new interface
hwaddr                  | string  | randomly assigned | no       | no      | The MAC address of the new interface
vlan                    | integer | -                 | no       | no      | The VLAN ID to attach to
gvrp                    | boolean | false             | no       | no      | Register VLAN using GARP VLAN Registration Protocol
maas.subnet.ipv4        | string  | -                 | no       | yes     | MAAS IPv4 subnet to register the instance in
maas.subnet.ipv6        | string  | -                 | no       | yes     | MAAS IPv6 subnet to register the instance in
boot.priority           | integer | -                 | no       | no      | Boot priority for VMs (higher boots first)

#### nic: sriov

Supported instance types: container, VM

Selected using: `nictype`, `network`

Passes a virtual function of an SR-IOV enabled physical network device into the instance.

Device configuration properties:

Key                     | Type    | Default           | Required | Managed | Description
:--                     | :--     | :--               | :--      | :--     | :--
parent                  | string  | -                 | yes      | yes     | The name of the host device
network                 | string  | -                 | yes      | no      | The LXD network to link device to (instead of parent)
name                    | string  | kernel assigned   | no       | no      | The name of the interface inside the instance
mtu                     | integer | kernel assigned   | no       | yes     | The MTU of the new interface
hwaddr                  | string  | randomly assigned | no       | no      | The MAC address of the new interface
security.mac\_filtering | boolean | false             | no       | no      | Prevent the instance from spoofing another's MAC address
vlan                    | integer | -                 | no       | no      | The VLAN ID to attach to
maas.subnet.ipv4        | string  | -                 | no       | yes     | MAAS IPv4 subnet to register the instance in
maas.subnet.ipv6        | string  | -                 | no       | yes     | MAAS IPv6 subnet to register the instance in
boot.priority           | integer | -                 | no       | no      | Boot priority for VMs (higher boots first)

#### nic: ovn

Supported instance types: container, VM

Selected using: `network`

Uses an existing OVN network and creates a virtual device pair to connect the instance to it.

Device configuration properties:

Key                                  | Type    | Default           | Required | Managed | Description
:--                                  | :--     | :--               | :--      | :--     | :--
network                              | string  | -                 | yes      | yes     | The LXD network to link device to
name                                 | string  | kernel assigned   | no       | no      | The name of the interface inside the instance
host\_name                           | string  | randomly assigned | no       | no      | The name of the interface inside the host
hwaddr                               | string  | randomly assigned | no       | no      | The MAC address of the new interface
ipv4.address                         | string  | -                 | no       | no      | An IPv4 address to assign to the instance through DHCP
ipv6.address                         | string  | -                 | no       | no      | An IPv6 address to assign to the instance through DHCP
ipv4.routes                          | string  | -                 | no       | no      | Comma delimited list of IPv4 static routes to route to the NIC
ipv6.routes                          | string  | -                 | no       | no      | Comma delimited list of IPv6 static routes to route to the NIC
ipv4.routes.external                 | string  | -                 | no       | no      | Comma delimited list of IPv4 static routes to route to the NIC and publish on uplink network
ipv6.routes.external                 | string  | -                 | no       | no      | Comma delimited list of IPv6 static routes to route to the NIC and publish on uplink network
boot.priority                        | integer | -                 | no       | no      | Boot priority for VMs (higher boots first)
security.acls                        | string  | -                 | no       | no      | Comma separated list of Network ACLs to apply
security.acls.default.ingress.action | string  | reject            | no       | no      | Action to use for ingress traffic that doesn't match any ACL rule
security.acls.default.egress.action  | string  | reject            | no       | no      | Action to use for egress traffic that doesn't match any ACL rule
security.acls.default.ingress.logged | boolean | false             | no       | no      | Whether to log ingress traffic that doesn't match any ACL rule
security.acls.default.egress.logged  | boolean | false             | no       | no      | Whether to log egress traffic that doesn't match any ACL rule

#### nic: physical

Supported instance types: container, VM

Selected using: `nictype`

Straight physical device passthrough from the host. The targeted device will vanish from the host and appear in the instance.

Device configuration properties:

Key                     | Type    | Default           | Required | Description
:--                     | :--     | :--               | :--      | :--
parent                  | string  | -                 | yes      | The name of the host device
name                    | string  | kernel assigned   | no       | The name of the interface inside the instance
mtu                     | integer | parent MTU        | no       | The MTU of the new interface
hwaddr                  | string  | randomly assigned | no       | The MAC address of the new interface
vlan                    | integer | -                 | no       | The VLAN ID to attach to
gvrp                    | boolean | false             | no       | Register VLAN using GARP VLAN Registration Protocol
maas.subnet.ipv4        | string  | -                 | no       | MAAS IPv4 subnet to register the instance in
maas.subnet.ipv6        | string  | -                 | no       | MAAS IPv6 subnet to register the instance in
boot.priority           | integer | -                 | no       | Boot priority for VMs (higher boots first)

#### nic: ipvlan

Supported instance types: container

Selected using: `nictype`

Sets up a new network device based on an existing one using the same MAC address but a different IP.

LXD currently supports IPVLAN in L2 and L3S mode.

In this mode, the gateway is automatically set by LXD, however IP addresses must be manually specified using either one or both of `ipv4.address` and `ipv6.address` settings before instance is started.

For DNS, the nameservers need to be configured inside the instance, as these will not automatically be set.

It requires the following sysctls to be set:

If using IPv4 addresses:

```
net.ipv4.conf.<parent>.forwarding=1
```

If using IPv6 addresses:

```
net.ipv6.conf.<parent>.forwarding=1
net.ipv6.conf.<parent>.proxy_ndp=1
```

Device configuration properties:

Key                     | Type    | Default            | Required | Description
:--                     | :--     | :--                | :--      | :--
parent                  | string  | -                  | yes      | The name of the host device
name                    | string  | kernel assigned    | no       | The name of the interface inside the instance
mtu                     | integer | parent MTU         | no       | The MTU of the new interface
mode                    | string  | l3s                | no       | The IPVLAN mode (either `l2` or `l3s`)
hwaddr                  | string  | randomly assigned  | no       | The MAC address of the new interface
ipv4.address            | string  | -                  | no       | Comma delimited list of IPv4 static addresses to add to the instance. In `l2` mode these can be specified as CIDR values or singular addresses (if singular a subnet of /24 is used).
ipv4.gateway            | string  | auto               | no       | In `l3s` mode, whether to add an automatic default IPv4 gateway, can be `auto` or `none`. In `l2` mode specifies the IPv4 address of the gateway.
ipv4.host\_table        | integer | -                  | no       | The custom policy routing table ID to add IPv4 static routes to (in addition to main routing table).
ipv6.address            | string  | -                  | no       | Comma delimited list of IPv6 static addresses to add to the instance. In `l2` mode these can be specified as CIDR values or singular addresses (if singular a subnet of /64 is used).
ipv6.gateway            | string  | auto (l3s), - (l2) | no       | In `l3s` mode, whether to add an automatic default IPv6 gateway, can be `auto` or `none`. In `l2` mode specifies the IPv6 address of the gateway.
ipv6.host\_table        | integer | -                  | no       | The custom policy routing table ID to add IPv6 static routes to (in addition to main routing table).
vlan                    | integer | -                  | no       | The VLAN ID to attach to
gvrp                    | boolean | false              | no       | Register VLAN using GARP VLAN Registration Protocol

#### nic: p2p

Supported instance types: container, VM

Selected using: `nictype`

Creates a virtual device pair, putting one side in the instance and leaving the other side on the host.

Device configuration properties:

Key                     | Type    | Default           | Required | Description
:--                     | :--     | :--               | :--      | :--
name                    | string  | kernel assigned   | no       | The name of the interface inside the instance
mtu                     | integer | kernel assigned   | no       | The MTU of the new interface
hwaddr                  | string  | randomly assigned | no       | The MAC address of the new interface
host\_name              | string  | randomly assigned | no       | The name of the interface inside the host
limits.ingress          | string  | -                 | no       | I/O limit in bit/s for incoming traffic (various suffixes supported, see below)
limits.egress           | string  | -                 | no       | I/O limit in bit/s for outgoing traffic (various suffixes supported, see below)
limits.max              | string  | -                 | no       | Same as modifying both limits.ingress and limits.egress
ipv4.routes             | string  | -                 | no       | Comma delimited list of IPv4 static routes to add on host to nic
ipv6.routes             | string  | -                 | no       | Comma delimited list of IPv6 static routes to add on host to nic
boot.priority           | integer | -                 | no       | Boot priority for VMs (higher boots first)

#### nic: routed

Supported instance types: container

Selected using: `nictype`

This NIC type is similar in operation to IPVLAN, in that it allows an instance to join an external network without needing to configure a bridge and shares the host's MAC address.

However it differs from IPVLAN because it does not need IPVLAN support in the kernel and the host and instance can communicate with each other.

It will also respect netfilter rules on the host and will use the host's routing table to route packets which can be useful if the host is connected to multiple networks.

IP addresses must be manually specified using either one or both of `ipv4.address` and `ipv6.address` settings before the instance is started.

It sets up a veth pair between host and instance and then configures the following link-local gateway IPs on the host end which are then set as the default gateways in the instance:

  169.254.0.1
  fe80::1

It then configures static routes on the host pointing to the instance's veth interface for all of the instance's IPs.

This nic can operate with and without a `parent` network interface set.

With the `parent` network interface set proxy ARP/NDP entries of the instance's IPs are added to the parent interface allowing the instance to join the parent interface's network at layer 2.

For DNS, the nameservers need to be configured inside the instance, as these will not automatically be set.

It requires the following sysctls to be set:

If using IPv4 addresses:

```
net.ipv4.conf.<parent>.forwarding=1
```

If using IPv6 addresses:

```
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.<parent>.forwarding=1
net.ipv6.conf.all.proxy_ndp=1
net.ipv6.conf.<parent>.proxy_ndp=1
```

Each NIC device can have multiple IP addresses added to them. However it may be desirable to utilise multiple `routed` NIC interfaces.
In these cases one should set the `ipv4.gateway` and `ipv6.gateway` values to "none" on any subsequent interfaces to avoid default gateway conflicts.
It may also be useful to specify a different host-side address for these subsequent interfaces using `ipv4.host_address` and `ipv6.host_address` respectively.

Device configuration properties:

Key                     | Type    | Default           | Required | Description
:--                     | :--     | :--               | :--      | :--
parent                  | string  | -                 | no       | The name of the host device to join the instance to
name                    | string  | kernel assigned   | no       | The name of the interface inside the instance
host\_name              | string  | randomly assigned | no       | The name of the interface inside the host
mtu                     | integer | parent MTU        | no       | The MTU of the new interface
hwaddr                  | string  | randomly assigned | no       | The MAC address of the new interface
limits.ingress          | string  | -                 | no       | I/O limit in bit/s for incoming traffic (various suffixes supported, see below)
limits.egress           | string  | -                 | no       | I/O limit in bit/s for outgoing traffic (various suffixes supported, see below)
limits.max              | string  | -                 | no       | Same as modifying both limits.ingress and limits.egress
ipv4.address            | string  | -                 | no       | Comma delimited list of IPv4 static addresses to add to the instance
ipv4.gateway            | string  | auto              | no       | Whether to add an automatic default IPv4 gateway, can be "auto" or "none"
ipv4.host\_address      | string  | 169.254.0.1       | no       | The IPv4 address to add to the host-side veth interface.
ipv4.host\_table        | integer | -                 | no       | The custom policy routing table ID to add IPv4 static routes to (in addition to main routing table).
ipv6.address            | string  | -                 | no       | Comma delimited list of IPv6 static addresses to add to the instance
ipv6.gateway            | string  | auto              | no       | Whether to add an automatic default IPv6 gateway, can be "auto" or "none"
ipv6.host\_address      | string  | fe80::1           | no       | The IPv6 address to add to the host-side veth interface.
ipv6.host\_table        | integer | -                 | no       | The custom policy routing table ID to add IPv6 static routes to (in addition to main routing table).
vlan                    | integer | -                 | no       | The VLAN ID to attach to
gvrp                    | boolean | false             | no       | Register VLAN using GARP VLAN Registration Protocol

#### bridged, macvlan or ipvlan for connection to physical network

The `bridged`, `macvlan` and `ipvlan` interface types can be used to connect to an existing physical network.

`macvlan` effectively lets you fork your physical NIC, getting a second interface that's then used by the instance.
This saves you from creating a bridge device and veth pairs and usually offers better performance than a bridge.

The downside to this is that macvlan devices while able to communicate between themselves and to the outside, aren't able to talk to their parent device.
This means that you can't use macvlan if you ever need your instances to talk to the host itself.

In such case, a bridge is preferable. A bridge will also let you use mac filtering and I/O limits which cannot be applied to a macvlan device.

`ipvlan` is similar to `macvlan`, with the difference being that the forked device has IPs statically assigned to it and inherits the parent's MAC address on the network.

#### SR-IOV
The `sriov` interface type supports SR-IOV enabled network devices.
These devices associate a set of virtual functions (VFs) with the single physical function (PF) of the network device.
PFs are standard PCIe functions. VFs on the other hand are very lightweight PCIe functions that are optimized for data movement.
They come with a limited set of configuration capabilities to prevent changing properties of the PF.
Given that VFs appear as regular PCIe devices to the system they can be passed to instances just like a regular physical device.
The `sriov` interface type expects to be passed the name of an SR-IOV enabled network device on the system via the `parent` property.
LXD will then check for any available VFs on the system. By default LXD will allocate the first free VF it finds.
If it detects that either none are enabled or all currently enabled VFs are in use it will bump the number of supported VFs to the maximum value and use the first free VF.
If all possible VFs are in use or the kernel or card doesn't support incrementing the number of VFs LXD will return an error.

To create a `sriov` network device use:

```
lxc config device add <instance> <device-name> nic nictype=sriov parent=<sriov-enabled-device>
```

To tell LXD to use a specific unused VF add the `host_name` property and pass
it the name of the enabled VF.


#### MAAS integration
If you're using MAAS to manage the physical network under your LXD host
and want to attach your instances directly to a MAAS managed network,
LXD can be configured to interact with MAAS so that it can track your
instances.

At the daemon level, you must configure `maas.api.url` and
`maas.api.key`, then set the `maas.subnet.ipv4` and/or
`maas.subnet.ipv6` keys on the instance or profile's `nic` entry.

This will have LXD register all your instances with MAAS, giving them
proper DHCP leases and DNS records.

If you set the `ipv4.address` or `ipv6.address` keys on the nic, then
those will be registered as static assignments in MAAS too.

### Type: infiniband

Supported instance types: container

LXD supports two different kind of network types for infiniband devices:

 - `physical`: Straight physical device passthrough from the host. The targeted device will vanish from the host and appear in the instance.
 - `sriov`: Passes a virtual function of an SR-IOV enabled physical network device into the instance.

Different network interface types have different additional properties, the current list is:

Key                     | Type      | Default           | Required  | Used by         | Description
:--                     | :--       | :--               | :--       | :--             | :--
nictype                 | string    | -                 | yes       | all             | The device type, one of "physical", or "sriov"
name                    | string    | kernel assigned   | no        | all             | The name of the interface inside the instance
hwaddr                  | string    | randomly assigned | no        | all             | The MAC address of the new interface. Can be either full 20 byte variant or short 8 byte variant (which will only modify the last 8 bytes of the parent device)
mtu                     | integer   | parent MTU        | no        | all             | The MTU of the new interface
parent                  | string    | -                 | yes       | physical, sriov | The name of the host device or bridge

To create a `physical` `infiniband` device use:

```
lxc config device add <instance> <device-name> infiniband nictype=physical parent=<device>
```

#### SR-IOV with infiniband devices
Infiniband devices do support SR-IOV but in contrast to other SR-IOV enabled
devices infiniband does not support dynamic device creation in SR-IOV mode.
This means users need to pre-configure the number of virtual functions by
configuring the corresponding kernel module.

To create a `sriov` `infiniband` device use:

```
lxc config device add <instance> <device-name> infiniband nictype=sriov parent=<sriov-enabled-device>
```

### Type: disk

Supported instance types: container, VM

Disk entries are essentially mountpoints inside the instance. They can
either be a bind-mount of an existing file or directory on the host, or
if the source is a block device, a regular mount.

LXD supports the following additional source types:

- Ceph-rbd: Mount from existing ceph RBD device that is externally managed. LXD can use ceph to manage an internal file system for the instance, but in the event that a user has a previously existing ceph RBD that they would like use for this instance, they can use this command.
Example command
```
lxc config device add <instance> ceph-rbd1 disk source=ceph:<my_pool>/<my-volume> ceph.user_name=<username> ceph.cluster_name=<username> path=/ceph
```
- Ceph-fs: Mount from existing ceph FS device that is externally managed. LXD can use ceph to manage an internal file system for the instance, but in the event that a user has a previously existing ceph file sys that they would like use for this instancer, they can use this command.
Example command.
```
lxc config device add <instance> ceph-fs1 disk source=cephfs:<my-fs>/<some-path> ceph.user_name=<username> ceph.cluster_name=<username> path=/cephfs
```
- VM cloud-init: Generate a cloud-init config ISO from the user.vendor-data, user.user-data and user.meta-data config keys and attach to the VM so that cloud-init running inside the VM guest will detect the drive on boot and apply the config. Only applicable to virtual-machine instances.
Example command.
```
lxc config device add <instance> config disk source=cloud-init:config
```

Currently only the root disk (path=/) and config drive (source=cloud-init:config) are supported with virtual machines.


The following properties exist:

Key                 | Type      | Default   | Required  | Description
:--                 | :--       | :--       | :--       | :--
limits.read         | string    | -         | no        | I/O limit in byte/s (various suffixes supported, see below) or in iops (must be suffixed with "iops")
limits.write        | string    | -         | no        | I/O limit in byte/s (various suffixes supported, see below) or in iops (must be suffixed with "iops")
limits.max          | string    | -         | no        | Same as modifying both limits.read and limits.write
path                | string    | -         | yes       | Path inside the instance where the disk will be mounted (only for containers).
source              | string    | -         | yes       | Path on the host, either to a file/directory or to a block device
required            | boolean   | true      | no        | Controls whether to fail if the source doesn't exist
readonly            | boolean   | false     | no        | Controls whether to make the mount read-only
size                | string    | -         | no        | Disk size in bytes (various suffixes supported, see below). This is only supported for the rootfs (/)
size.state          | string    | -         | no        | Same as size above but applies to the filesystem volume used for saving runtime state in virtual machines.
recursive           | boolean   | false     | no        | Whether or not to recursively mount the source path
pool                | string    | -         | no        | The storage pool the disk device belongs to. This is only applicable for storage volumes managed by LXD
propagation         | string    | -         | no        | Controls how a bind-mount is shared between the instance and the host. (Can be one of `private`, the default, or `shared`, `slave`, `unbindable`,  `rshared`, `rslave`, `runbindable`,  `rprivate`. Please see the Linux Kernel [shared subtree](https://www.kernel.org/doc/Documentation/filesystems/sharedsubtree.txt) documentation for a full explanation)
shift               | boolean   | false     | no        | Setup a shifting overlay to translate the source uid/gid to match the instance
raw.mount.options   | string    | -         | no        | Filesystem specific mount options
ceph.user\_name     | string    | admin     | no        | If source is ceph or cephfs then ceph user\_name must be specified by user for proper mount
ceph.cluster\_name  | string    | ceph      | no        | If source is ceph or cephfs then ceph cluster\_name must be specified by user for proper mount
boot.priority       | integer   | -         | no        | Boot priority for VMs (higher boots first)

### Type: unix-char

Supported instance types: container

Unix character device entries simply make the requested character device
appear in the instance's `/dev` and allow read/write operations to it.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
source      | string    | -                 | no        | Path on the host
path        | string    | -                 | no        | Path inside the instance (one of "source" and "path" must be set)
major       | int       | device on host    | no        | Device major number
minor       | int       | device on host    | no        | Device minor number
uid         | int       | 0                 | no        | UID of the device owner in the instance
gid         | int       | 0                 | no        | GID of the device owner in the instance
mode        | int       | 0660              | no        | Mode of the device in the instance
required    | boolean   | true              | no        | Whether or not this device is required to start the instance

### Type: unix-block

Supported instance types: container

Unix block device entries simply make the requested block device
appear in the instance's `/dev` and allow read/write operations to it.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
source      | string    | -                 | no        | Path on the host
path        | string    | -                 | no        | Path inside the instance (one of "source" and "path" must be set)
major       | int       | device on host    | no        | Device major number
minor       | int       | device on host    | no        | Device minor number
uid         | int       | 0                 | no        | UID of the device owner in the instance
gid         | int       | 0                 | no        | GID of the device owner in the instance
mode        | int       | 0660              | no        | Mode of the device in the instance
required    | boolean   | true              | no        | Whether or not this device is required to start the instance

### Type: usb

Supported instance types: container, VM

USB device entries simply make the requested USB device appear in the
instance.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
vendorid    | string    | -                 | no        | The vendor id of the USB device
productid   | string    | -                 | no        | The product id of the USB device
uid         | int       | 0                 | no        | UID of the device owner in the instance
gid         | int       | 0                 | no        | GID of the device owner in the instance
mode        | int       | 0660              | no        | Mode of the device in the instance
required    | boolean   | false             | no        | Whether or not this device is required to start the instance. (The default is false, and all devices are hot-pluggable)

### Type: gpu

GPU device entries simply make the requested gpu device appear in the
instance.

#### GPUs Available:

The following GPUs can be specified using the `gputype` property:

 - [physical](#gpu-physical) Passes through an entire GPU. This is the default if `gputype` is unspecified.
 - [mdev](#gpu-mdev) Creates and passes through a virtual GPU into the instance.
 - [mig](#gpu-mig) Creates and passes through a MIG (Multi-Instance GPU) device into the instance.
 - [sriov](#gpu-sriov) Passes a virtual function of an SR-IOV enabled GPU into the instance.

#### gpu: physical

Supported instance types: container, VM

Passes through an entire GPU.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
vendorid    | string    | -                 | no        | The vendor id of the GPU device
productid   | string    | -                 | no        | The product id of the GPU device
id          | string    | -                 | no        | The card id of the GPU device
pci         | string    | -                 | no        | The pci address of the GPU device
uid         | int       | 0                 | no        | UID of the device owner in the instance (container only)
gid         | int       | 0                 | no        | GID of the device owner in the instance (container only)
mode        | int       | 0660              | no        | Mode of the device in the instance (container only)

#### gpu: mdev

Supported instance types: VM

Creates and passes through a virtual GPU into the instance. A list of available mdev profiles can be found by running `lxc info --resources`.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
vendorid    | string    | -                 | no        | The vendor id of the GPU device
productid   | string    | -                 | no        | The product id of the GPU device
id          | string    | -                 | no        | The card id of the GPU device
pci         | string    | -                 | no        | The pci address of the GPU device
mdev        | string    | -                 | yes       | The mdev profile to use (e.g. i915-GVTg\_V5\_4)

#### gpu: mig

Supported instance types: container

Creates and passes through a MIG compute instance. This currently requires NVIDIA MIG instances to be pre-created.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
vendorid    | string    | -                 | no        | The vendor id of the GPU device
productid   | string    | -                 | no        | The product id of the GPU device
id          | string    | -                 | no        | The card id of the GPU device
pci         | string    | -                 | no        | The pci address of the GPU device
mig.ci      | int       | -                 | yes       | Existing MIG compute instance ID
mig.gi      | int       | -                 | yes       | Existing MIG GPU instance ID

#### gpu: sriov

Supported instance types: VM

Passes a virtual function of an SR-IOV enabled GPU into the instance.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
vendorid    | string    | -                 | no        | The vendor id of the parent GPU device
productid   | string    | -                 | no        | The product id of the parent GPU device
id          | string    | -                 | no        | The card id of the parent GPU device
pci         | string    | -                 | no        | The pci address of the parent GPU device

### Type: proxy

Supported instance types: container (`nat` and non-`nat` modes), VM (`nat` mode only)

Proxy devices allow forwarding network connections between host and instance.
This makes it possible to forward traffic hitting one of the host's
addresses to an address inside the instance or to do the reverse and
have an address in the instance connect through the host.

The supported connection types are:
* `tcp <-> tcp`
* `udp <-> udp`
* `unix <-> unix`
* `tcp <-> unix`
* `unix <-> tcp`
* `udp <-> tcp`
* `tcp <-> udp`
* `udp <-> unix`
* `unix <-> udp`

The proxy device also supports a `nat` mode where packets are forwarded using NAT rather than being proxied through
a separate connection. This has benefit that the client address is maintained without the need for the target
destination to support the `PROXY` protocol (which is the only way to pass the client address through when using
the proxy device in non-nat mode).

When configuring a proxy device with `nat=true`, you will need to ensure that the target instance has a static IP
configured in LXD on its NIC device. E.g.

```
lxc config device set <instance> <nic> ipv4.address=<ipv4.address> ipv6.address=<ipv6.address>
```

In order to define a static IPv6 address, the parent managed network needs to have `ipv6.dhcp.stateful` enabled.

In NAT mode the supported connection types are:

* `tcp <-> tcp`
* `udp <-> udp`

When defining IPv6 addresses use square bracket notation, e.g.

```
connect=tcp:[2001:db8::1]:80
```

You can specify that the connect address should be the IP of the instance by setting the connect IP to the wildcard
address (`0.0.0.0` for IPv4 and `[::]` for IPv6).

The listen address can also use wildcard addresses when using non-NAT mode. However when using `nat` mode you must
specify an IP address on the LXD host.

Key             | Type      | Default       | Required  | Description
:--             | :--       | :--           | :--       | :--
listen          | string    | -             | yes       | The address and port to bind and listen (`<type>:<addr>:<port>[-<port>][,<port>]`)
connect         | string    | -             | yes       | The address and port to connect to (`<type>:<addr>:<port>[-<port>][,<port>]`)
bind            | string    | host          | no        | Which side to bind on (host/instance)
uid             | int       | 0             | no        | UID of the owner of the listening Unix socket
gid             | int       | 0             | no        | GID of the owner of the listening Unix socket
mode            | int       | 0644          | no        | Mode for the listening Unix socket
nat             | bool      | false         | no        | Whether to optimize proxying via NAT (requires instance NIC has static IP address)
proxy\_protocol | bool      | false         | no        | Whether to use the HAProxy PROXY protocol to transmit sender information
security.uid    | int       | 0             | no        | What UID to drop privilege to
security.gid    | int       | 0             | no        | What GID to drop privilege to

```
lxc config device add <instance> <device-name> proxy listen=<type>:<addr>:<port>[-<port>][,<port>] connect=<type>:<addr>:<port> bind=<host/instance>
```

### Type: unix-hotplug

Supported instance types: container

Unix hotplug device entries make the requested unix device appear in the
instance's `/dev` and allow read/write operations to it if the device exists on
the host system. Implementation depends on systemd-udev to be run on the host.

The following properties exist:

Key         | Type      | Default           | Required  | Description
:--         | :--       | :--               | :--       | :--
vendorid    | string    | -                 | no        | The vendor id of the unix device
productid   | string    | -                 | no        | The product id of the unix device
uid         | int       | 0                 | no        | UID of the device owner in the instance
gid         | int       | 0                 | no        | GID of the device owner in the instance
mode        | int       | 0660              | no        | Mode of the device in the instance
required    | boolean   | false             | no        | Whether or not this device is required to start the instance. (The default is false, and all devices are hot-pluggable)


### Type: tpm

Supported instance types: container, VM

TPM device entries enable access to a TPM emulator.

The following properties exist:

Key                 | Type      | Default   | Required  | Description
:--                 | :--       | :--       | :--       | :--
path                | string    | -         | yes       | Path inside the instance (only for containers).

### Type: pci

Supported instance types: VM

PCI device entries are used to pass raw PCI devices from the host into a virtual machine.

The following properties exist:

Key                 | Type      | Default   | Required  | Description
:--                 | :--       | :--       | :--       | :--
address             | string    | -         | yes       | PCI address of the device.


## Units for storage and network limits
Any value representing bytes or bits can make use of a number of useful
suffixes to make it easier to understand what a particular limit is.

Both decimal and binary (kibi) units are supported with the latter
mostly making sense for storage limits.

The full list of bit suffixes currently supported is:

 - bit (1)
 - kbit (1000)
 - Mbit (1000^2)
 - Gbit (1000^3)
 - Tbit (1000^4)
 - Pbit (1000^5)
 - Ebit (1000^6)
 - Kibit (1024)
 - Mibit (1024^2)
 - Gibit (1024^3)
 - Tibit (1024^4)
 - Pibit (1024^5)
 - Eibit (1024^6)

The full list of byte suffixes currently supported is:

 - B or bytes (1)
 - kB (1000)
 - MB (1000^2)
 - GB (1000^3)
 - TB (1000^4)
 - PB (1000^5)
 - EB (1000^6)
 - KiB (1024)
 - MiB (1024^2)
 - GiB (1024^3)
 - TiB (1024^4)
 - PiB (1024^5)
 - EiB (1024^6)

## Instance types
LXD supports simple instance types. Those are represented as a string
which can be passed at instance creation time.

There are three allowed syntaxes:

 - `<instance type>`
 - `<cloud>:<instance type>`
 - `c<CPU>-m<RAM in GB>`

For example, those 3 are equivalent:

 - t2.micro
 - aws:t2.micro
 - c1-m1

On the command line, this is passed like this:

```bash
lxc launch ubuntu:18.04 my-instance -t t2.micro
```

The list of supported clouds and instance types can be found here:

  https://github.com/dustinkirkland/instance-type

## Hugepage limits via `limits.hugepages.[size]`
LXD allows to limit the number of hugepages available to a container through
the `limits.hugepage.[size]` key. Limiting hugepages is done through the
hugetlb cgroup controller. This means the host system needs to expose the
hugetlb controller in the legacy or unified cgroup hierarchy for these limits
to apply.
Note that architectures often expose multiple hugepage sizes. In addition,
architectures may expose different hugepage sizes than other architectures.

Limiting hugepages is especially useful when LXD is configured to intercept the
mount syscall for the `hugetlbfs` filesystem in unprivileged containers. When
LXD intercepts a `hugetlbfs` mount  syscall, it will mount the `hugetlbfs`
filesystem for a container with correct `uid` and `gid` values as mount
options. This makes it possible to use hugepages from unprivileged containers.
However, it is recommended to limit the number of hugepages available to the
container through `limits.hugepages.[size]` to stop the container from being
able to exhaust the hugepages available to the host.

## Resource limits via `limits.kernel.[limit name]`
LXD exposes a generic namespaced key `limits.kernel.*` which can be used to set
resource limits for a given instance. It is generic in the sense that LXD will
not perform any validation on the resource that is specified following the
`limits.kernel.*` prefix. LXD cannot know about all the possible resources that
a given kernel supports. Instead, LXD will simply pass down the corresponding
resource key after the `limits.kernel.*` prefix and its value to the kernel.
The kernel will do the appropriate validation. This allows users to specify any
supported limit on their system. Some common limits are:

Key                      | Resource          | Description
:--                      | :---              | :----------
limits.kernel.as         | RLIMIT\_AS         | Maximum size of the process's virtual memory
limits.kernel.core       | RLIMIT\_CORE       | Maximum size of the process's coredump file
limits.kernel.cpu        | RLIMIT\_CPU        | Limit in seconds on the amount of cpu time the process can consume
limits.kernel.data       | RLIMIT\_DATA       | Maximum size of the process's data segment
limits.kernel.fsize      | RLIMIT\_FSIZE      | Maximum size of files the process may create
limits.kernel.locks      | RLIMIT\_LOCKS      | Limit on the number of file locks that this process may establish
limits.kernel.memlock    | RLIMIT\_MEMLOCK    | Limit on the number of bytes of memory that the process may lock in RAM
limits.kernel.nice       | RLIMIT\_NICE       | Maximum value to which the process's nice value can be raised
limits.kernel.nofile     | RLIMIT\_NOFILE     | Maximum number of open files for the process
limits.kernel.nproc      | RLIMIT\_NPROC      | Maximum number of processes that can be created for the user of the calling process
limits.kernel.rtprio     | RLIMIT\_RTPRIO     | Maximum value on the real-time-priority that maybe set for this process
limits.kernel.sigpending | RLIMIT\_SIGPENDING | Maximum number of signals that maybe queued for the user of the calling process

A full list of all available limits can be found in the manpages for the
`getrlimit(2)`/`setrlimit(2)` system calls. To specify a limit within the
`limits.kernel.*` namespace use the resource name in lowercase without the
`RLIMIT_` prefix, e.g.  `RLIMIT_NOFILE` should be specified as `nofile`.
A limit is specified as two colon separated values which are either numeric or
the word `unlimited` (e.g. `limits.kernel.nofile=1000:2000`). A single value can be
used as a shortcut to set both soft and hard limit (e.g.
`limits.kernel.nofile=3000`) to the same value. A resource with no explicitly
configured limitation will be inherited from the process starting up the
instance. Note that this inheritance is not enforced by LXD but by the kernel.

## Snapshot scheduling
LXD supports scheduled snapshots which can be created at most once every minute.
There are three configuration options. `snapshots.schedule` takes a shortened
cron expression: `<minute> <hour> <day-of-month> <month> <day-of-week>`. If this is
empty (default), no snapshots will be created. `snapshots.schedule.stopped`
controls whether or not stopped instance are to be automatically snapshotted.
It defaults to `false`. `snapshots.pattern` takes a pongo2 template string,
and the pongo2 context contains the `creation_date` variable. Be aware that you
should format the date (e.g. use `{{ creation_date|date:"2006-01-02_15-04-05" }}`)
in your template string to avoid forbidden characters in your snapshot name.
Another way to avoid name collisions is to use the placeholder `%d`. If a snapshot
with the same name (excluding the placeholder) already exists, all existing snapshot
names will be taken into account to find the highest number at the placeholders
position. This number will be incremented by one for the new name. The starting
number if no snapshot exists will be `0`.
