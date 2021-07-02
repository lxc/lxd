# API extensions

The changes below were introduced to the LXD API after the 1.0 API was finalized.

They are all backward compatible and can be detected by client tools by
looking at the `api_extensions` field in `GET /1.0/`.


## storage\_zfs\_remove\_snapshots
A `storage.zfs_remove_snapshots` daemon configuration key was introduced.

It's a boolean that defaults to false and that when set to true instructs LXD
to remove any needed snapshot when attempting to restore another.

This is needed as ZFS will only let you restore the latest snapshot.

## container\_host\_shutdown\_timeout
A `boot.host_shutdown_timeout` container configuration key was introduced.

It's an integer which indicates how long LXD should wait for the container
to stop before killing it.

Its value is only used on clean LXD daemon shutdown. It defaults to 30s.

## container\_stop\_priority
A `boot.stop.priority` container configuration key was introduced.

It's an integer which indicates the priority of a container during shutdown.

Containers will shutdown starting with the highest priority level.

Containers with the same priority will shutdown in parallel.  It defaults to 0.

## container\_syscall\_filtering
A number of new syscalls related container configuration keys were introduced.

 * `security.syscalls.blacklist_default`
 * `security.syscalls.blacklist_compat`
 * `security.syscalls.blacklist`
 * `security.syscalls.whitelist`

See [configuration.md](configuration.md) for how to use them.

## auth\_pki
This indicates support for PKI authentication mode.

In this mode, the client and server both must use certificates issued by the same PKI.

See [security.md](security.md) for details.

## container\_last\_used\_at
A `last_used_at` field was added to the `GET /1.0/containers/<name>` endpoint.

It is a timestamp of the last time the container was started.

If a container has been created but not started yet, `last_used_at` field
will be `1970-01-01T00:00:00Z`

## etag
Add support for the ETag header on all relevant endpoints.

This adds the following HTTP header on answers to GET:

 - ETag (SHA-256 of user modifiable content)

And adds support for the following HTTP header on PUT requests:

 - If-Match (ETag value retrieved through previous GET)

This makes it possible to GET a LXD object, modify it and PUT it without
risking to hit a race condition where LXD or another client modified the
object in the meantime.

## patch
Add support for the HTTP PATCH method.

PATCH allows for partial update of an object in place of PUT.

## usb\_devices
Add support for USB hotplug.

## https\_allowed\_credentials
To use LXD API with all Web Browsers (via SPAs) you must send credentials
(certificate) with each XHR (in order for this to happen, you should set
["withCredentials=true"](https://developer.mozilla.org/en-US/docs/Web/API/XMLHttpRequest/withCredentials)
flag to each XHR Request).

Some browsers like Firefox and Safari can't accept server response without
`Access-Control-Allow-Credentials: true` header. To ensure that the server will
return a response with that header, set `core.https_allowed_credentials=true`.

## image\_compression\_algorithm
This adds support for a `compression_algorithm` property when creating an image (`POST /1.0/images`).

Setting this property overrides the server default value (`images.compression_algorithm`).

## directory\_manipulation
This allows for creating and listing directories via the LXD API, and exports
the file type via the X-LXD-type header, which can be either "file" or
"directory" right now.

## container\_cpu\_time
This adds support for retrieving cpu time for a running container.

## storage\_zfs\_use\_refquota
Introduces a new server property `storage.zfs_use_refquota` which instructs LXD
to set the "refquota" property instead of "quota" when setting a size limit
on a container. LXD will also then use "usedbydataset" in place of "used"
when being queried about disk utilization.

This effectively controls whether disk usage by snapshots should be
considered as part of the container's disk space usage.

## storage\_lvm\_mount\_options
Adds a new `storage.lvm_mount_options` daemon configuration option
which defaults to "discard" and allows the user to set addition mount
options for the filesystem used by the LVM LV.

## network
Network management API for LXD.

This includes:

 * Addition of the "managed" property on `/1.0/networks` entries
 * All the network configuration options (see [configuration.md](configuration.md) for details)
 * `POST /1.0/networks` (see [RESTful API](rest-api.md) for details)
 * `PUT /1.0/networks/<entry>` (see [RESTful API](rest-api.md) for details)
 * `PATCH /1.0/networks/<entry>` (see [RESTful API](rest-api.md) for details)
 * `DELETE /1.0/networks/<entry>` (see [RESTful API](rest-api.md) for details)
 * `ipv4.address` property on "nic" type devices (when nictype is "bridged")
 * `ipv6.address` property on "nic" type devices (when nictype is "bridged")
 * `security.mac_filtering` property on "nic" type devices (when nictype is "bridged")

## profile\_usedby
Adds a new used\_by field to profile entries listing the containers that are using it.

## container\_push
When a container is created in push mode, the client serves as a proxy between
the source and target server. This is useful in cases where the target server
is behind a NAT or firewall and cannot directly communicate with the source
server and operate in pull mode.

## container\_exec\_recording
Introduces a new boolean "record-output", parameter to
`/1.0/containers/<name>/exec` which when set to "true" and combined with
with "wait-for-websocket" set to false, will record stdout and stderr to
disk and make them available through the logs interface.

The URL to the recorded output is included in the operation metadata
once the command is done running.

That output will expire similarly to other log files, typically after 48 hours.

## certificate\_update
Adds the following to the REST API:

 * ETag header on GET of a certificate
 * PUT of certificate entries
 * PATCH of certificate entries

## container\_exec\_signal\_handling
Adds support `/1.0/containers/<name>/exec` for forwarding signals sent to the
client to the processes executing in the container. Currently SIGTERM and
SIGHUP are forwarded. Further signals that can be forwarded might be added
later.

## gpu\_devices
Enables adding GPUs to a container.

## container\_image\_properties
Introduces a new `image` config key space. Read-only, includes the properties of the parent image.

## migration\_progress
Transfer progress is now exported as part of the operation, on both sending and receiving ends.
This shows up as a "fs\_progress" attribute in the operation metadata.

## id\_map
Enables setting the `security.idmap.isolated` and `security.idmap.isolated`,
`security.idmap.size`, and `raw.id_map` fields.

## network\_firewall\_filtering
Add two new keys, `ipv4.firewall` and `ipv6.firewall` which if set to
false will turn off the generation of iptables FORWARDING rules. NAT
rules will still be added so long as the matching `ipv4.nat` or
`ipv6.nat` key is set to true.

Rules necessary for dnsmasq to work (DHCP/DNS) will always be applied if
dnsmasq is enabled on the bridge.

## network\_routes
Introduces `ipv4.routes` and `ipv6.routes` which allow routing additional subnets to a LXD bridge.

## storage
Storage management API for LXD.

This includes:

* `GET /1.0/storage-pools`
* `POST /1.0/storage-pools` (see [RESTful API](rest-api.md) for details)

* `GET /1.0/storage-pools/<name>` (see [RESTful API](rest-api.md) for details)
* `POST /1.0/storage-pools/<name>` (see [RESTful API](rest-api.md) for details)
* `PUT /1.0/storage-pools/<name>` (see [RESTful API](rest-api.md) for details)
* `PATCH /1.0/storage-pools/<name>` (see [RESTful API](rest-api.md) for details)
* `DELETE /1.0/storage-pools/<name>` (see [RESTful API](rest-api.md) for details)

* `GET /1.0/storage-pools/<name>/volumes` (see [RESTful API](rest-api.md) for details)

* `GET /1.0/storage-pools/<name>/volumes/<volume_type>` (see [RESTful API](rest-api.md) for details)
* `POST /1.0/storage-pools/<name>/volumes/<volume_type>` (see [RESTful API](rest-api.md) for details)

* `GET /1.0/storage-pools/<pool>/volumes/<volume_type>/<name>` (see [RESTful API](rest-api.md) for details)
* `POST /1.0/storage-pools/<pool>/volumes/<volume_type>/<name>` (see [RESTful API](rest-api.md) for details)
* `PUT /1.0/storage-pools/<pool>/volumes/<volume_type>/<name>` (see [RESTful API](rest-api.md) for details)
* `PATCH /1.0/storage-pools/<pool>/volumes/<volume_type>/<name>` (see [RESTful API](rest-api.md) for details)
* `DELETE /1.0/storage-pools/<pool>/volumes/<volume_type>/<name>` (see [RESTful API](rest-api.md) for details)

* All storage configuration options (see [configuration.md](configuration.md) for details)

## file\_delete
Implements `DELETE` in `/1.0/containers/<name>/files`

## file\_append
Implements the `X-LXD-write` header which can be one of `overwrite` or `append`.

## network\_dhcp\_expiry
Introduces `ipv4.dhcp.expiry` and `ipv6.dhcp.expiry` allowing to set the DHCP lease expiry time.

## storage\_lvm\_vg\_rename
Introduces the ability to rename a volume group by setting `storage.lvm.vg_name`.

## storage\_lvm\_thinpool\_rename
Introduces the ability to rename a thinpool name by setting `storage.thinpool_name`.

## network\_vlan
This adds a new `vlan` property to `macvlan` network devices.

When set, this will instruct LXD to attach to the specified VLAN. LXD
will look for an existing interface for that VLAN on the host. If one
can't be found it will create one itself and then use that as the
macvlan parent.

## image\_create\_aliases
Adds a new `aliases` field to `POST /1.0/images` allowing for aliases to
be set at image creation/import time.

## container\_stateless\_copy
This introduces a new `live` attribute in `POST /1.0/containers/<name>`.
Setting it to false tells LXD not to attempt running state transfer.

## container\_only\_migration
Introduces a new boolean `container_only` attribute. When set to true only the
container will be copied or moved.

## storage\_zfs\_clone\_copy
Introduces a new boolean `storage_zfs_clone_copy` property for ZFS storage
pools. When set to false copying a container will be done through zfs send and
receive. This will make the target container independent of its source
container thus avoiding the need to keep dependent snapshots in the ZFS pool
around. However, this also entails less efficient storage usage for the
affected pool.
The default value for this property is true, i.e. space-efficient snapshots
will be used unless explicitly set to "false".

## unix\_device\_rename
Introduces the ability to rename the unix-block/unix-char device inside container by setting `path`,
and the `source` attribute is added to specify the device on host.
If `source` is set without a `path`, we should assume that `path` will be the same as `source`.
If `path` is set without `source` and `major`/`minor` isn't set,
we should assume that `source` will be the same as `path`.
So at least one of them must be set.

## storage\_rsync\_bwlimit
When rsync has to be invoked to transfer storage entities setting `rsync.bwlimit`
places an upper limit on the amount of socket I/O allowed.

## network\_vxlan\_interface
This introduces a new `tunnel.NAME.interface` option for networks.

This key control what host network interface is used for a VXLAN tunnel.

## storage\_btrfs\_mount\_options
This introduces the `btrfs.mount_options` property for btrfs storage pools.

This key controls what mount options will be used for the btrfs storage pool.

## entity\_description
This adds descriptions to entities like containers, snapshots, networks, storage pools and volumes.

## image\_force\_refresh
This allows forcing a refresh for an existing image.

## storage\_lvm\_lv\_resizing
This introduces the ability to resize logical volumes by setting the `size`
property in the containers root disk device.

## id\_map\_base
This introduces a new `security.idmap.base` allowing the user to skip the
map auto-selection process for isolated containers and specify what host
uid/gid to use as the base.

## file\_symlinks
This adds support for transferring symlinks through the file API.
X-LXD-type can now be "symlink" with the request content being the target path.

## container\_push\_target
This adds the `target` field to `POST /1.0/containers/<name>` which can be
used to have the source LXD host connect to the target during migration.

## network\_vlan\_physical
Allows use of `vlan` property with `physical` network devices.

When set, this will instruct LXD to attach to the specified VLAN on the `parent` interface.
LXD will look for an existing interface for that `parent` and VLAN on the host.
If one can't be found it will create one itself.
Then, LXD will directly attach this interface to the container.

## storage\_images\_delete
This enabled the storage API to delete storage volumes for images from
a specific storage pool.

## container\_edit\_metadata
This adds support for editing a container metadata.yaml and related templates
via API, by accessing urls under `/1.0/containers/<name>/metadata`. It can be used
to edit a container before publishing an image from it.

## container\_snapshot\_stateful\_migration
This enables migrating stateful container snapshots to new containers.

## storage\_driver\_ceph
This adds a ceph storage driver.

## storage\_ceph\_user\_name
This adds the ability to specify the ceph user.

## instance\_types
This adds the `instance_type` field to the container creation request.
Its value is expanded to LXD resource limits.

## storage\_volatile\_initial\_source
This records the actual source passed to LXD during storage pool creation.

## storage\_ceph\_force\_osd\_reuse
This introduces the `ceph.osd.force_reuse` property for the ceph storage
driver. When set to `true` LXD will reuse a osd storage pool that is already in
use by another LXD instance.

## storage\_block\_filesystem\_btrfs
This adds support for btrfs as a storage volume filesystem, in addition to ext4
and xfs.

## resources
This adds support for querying a LXD daemon for the system resources it has
available.

## kernel\_limits
This adds support for setting process limits such as maximum number of open
files for the container via `nofile`. The format is `limits.kernel.[limit name]`.

## storage\_api\_volume\_rename
This adds support for renaming custom storage volumes.

## external\_authentication
This adds support for external authentication via Macaroons.

## network\_sriov
This adds support for SR-IOV enabled network devices.

## console
This adds support to interact with the container console device and console log.

## restrict\_devlxd
A new security.devlxd container configuration key was introduced.
The key controls whether the /dev/lxd interface is made available to the container.
If set to false, this effectively prevents the container from interacting with the LXD daemon.

## migration\_pre\_copy
This adds support for optimized memory transfer during live migration.

## infiniband
This adds support to use infiniband network devices.

## maas\_network
This adds support for MAAS network integration.

When configured at the daemon level, it's then possible to attach a "nic"
device to a particular MAAS subnet.

## devlxd\_events
This adds a websocket API to the devlxd socket.

When connecting to /1.0/events over the devlxd socket, you will now be
getting a stream of events over websocket.

## proxy
This adds a new `proxy` device type to containers, allowing forwarding
of connections between the host and container.

## network\_dhcp\_gateway
Introduces a new ipv4.dhcp.gateway network config key to set an alternate gateway.

## file\_get\_symlink
This makes it possible to retrieve symlinks using the file API.

## network\_leases
Adds a new /1.0/networks/NAME/leases API endpoint to query the lease database on
bridges which run a LXD-managed DHCP server.

## unix\_device\_hotplug
This adds support for the "required" property for unix devices.

## storage\_api\_local\_volume\_handling
This add the ability to copy and move custom storage volumes locally in the
same and between storage pools.

## operation\_description
Adds a "description" field to all operations.

## clustering
Clustering API for LXD.

This includes the following new endpoints (see [RESTful API](rest-api.md) for details):

* `GET /1.0/cluster`
* `UPDATE /1.0/cluster`

* `GET /1.0/cluster/members`

* `GET /1.0/cluster/members/<name>`
* `POST /1.0/cluster/members/<name>`
* `DELETE /1.0/cluster/members/<name>`

The following existing endpoints have been modified:

 * `POST /1.0/containers` accepts a new target query parameter
 * `POST /1.0/storage-pools` accepts a new target query parameter
 * `GET /1.0/storage-pool/<name>` accepts a new target query parameter
 * `POST /1.0/storage-pool/<pool>/volumes/<type>` accepts a new target query parameter
 * `GET /1.0/storage-pool/<pool>/volumes/<type>/<name>` accepts a new target query parameter
 * `POST /1.0/storage-pool/<pool>/volumes/<type>/<name>` accepts a new target query parameter
 * `PUT /1.0/storage-pool/<pool>/volumes/<type>/<name>` accepts a new target query parameter
 * `PATCH /1.0/storage-pool/<pool>/volumes/<type>/<name>` accepts a new target query parameter
 * `DELETE /1.0/storage-pool/<pool>/volumes/<type>/<name>` accepts a new target query parameter
 * `POST /1.0/networks` accepts a new target query parameter
 * `GET /1.0/networks/<name>` accepts a new target query parameter

## event\_lifecycle
This adds a new `lifecycle` message type to the events API.

## storage\_api\_remote\_volume\_handling
This adds the ability to copy and move custom storage volumes between remote.

## nvidia\_runtime
Adds a `nvidia_runtime` config option for containers, setting this to
true will have the NVIDIA runtime and CUDA libraries passed to the
container.

## container\_mount\_propagation
This adds a new "propagation" option to the disk device type, allowing
the configuration of kernel mount propagation.

## container\_backup
Add container backup support.

This includes the following new endpoints (see [RESTful API](rest-api.md) for details):

* `GET /1.0/containers/<name>/backups`
* `POST /1.0/containers/<name>/backups`

* `GET /1.0/containers/<name>/backups/<name>`
* `POST /1.0/containers/<name>/backups/<name>`
* `DELETE /1.0/containers/<name>/backups/<name>`

* `GET /1.0/containers/<name>/backups/<name>/export`

The following existing endpoint has been modified:

 * `POST /1.0/containers` accepts the new source type `backup`

## devlxd\_images
Adds a `security.devlxd.images` config option for containers which
controls the availability of a `/1.0/images/FINGERPRINT/export` API over
devlxd. This can be used by a container running nested LXD to access raw
images from the host.

## container\_local\_cross\_pool\_handling
This enables copying or moving containers between storage pools on the same LXD
instance.

## proxy\_unix
Add support for both unix sockets and abstract unix sockets in proxy devices.
They can be used by specifying the address as `unix:/path/to/unix.sock` (normal
socket) or `unix:@/tmp/unix.sock` (abstract socket).

Supported connections are now:

* `TCP <-> TCP`
* `UNIX <-> UNIX`
* `TCP <-> UNIX`
* `UNIX <-> TCP`

## proxy\_udp
Add support for udp in proxy devices.

Supported connections are now:

* `TCP <-> TCP`
* `UNIX <-> UNIX`
* `TCP <-> UNIX`
* `UNIX <-> TCP`
* `UDP <-> UDP`
* `TCP <-> UDP`
* `UNIX <-> UDP`

## clustering\_join
This makes GET /1.0/cluster return information about which storage pools and
networks are required to be created by joining nodes and which node-specific
configuration keys they are required to use when creating them. Likewise the PUT
/1.0/cluster endpoint now accepts the same format to pass information about
storage pools and networks to be automatically created before attempting to join
a cluster.

## proxy\_tcp\_udp\_multi\_port\_handling
Adds support for forwarding traffic for multiple ports. Forwarding is allowed
between a range of ports if the port range is equal for source and target
(for example `1.2.3.4 0-1000 -> 5.6.7.8 1000-2000`) and between a range of source
ports and a single target port (for example `1.2.3.4 0-1000 -> 5.6.7.8 1000`).

## network\_state
Adds support for retrieving a network's state.

This adds the following new endpoint (see [RESTful API](rest-api.md) for details):

* `GET /1.0/networks/<name>/state`

## proxy\_unix\_dac\_properties
This adds support for gid, uid, and mode properties for non-abstract unix
sockets.

## container\_protection\_delete
Enables setting the `security.protection.delete` field which prevents containers
from being deleted if set to true. Snapshots are not affected by this setting.

## proxy\_priv\_drop
Adds security.uid and security.gid for the proxy devices, allowing
privilege dropping and effectively changing the uid/gid used for
connections to Unix sockets too.

## pprof\_http
This adds a new core.debug\_address config option to start a debugging HTTP server.

That server currently includes a pprof API and replaces the old
cpu-profile, memory-profile and print-goroutines debug options.

## proxy\_haproxy\_protocol
Adds a proxy\_protocol key to the proxy device which controls the use of the HAProxy PROXY protocol header.

## network\_hwaddr
Adds a bridge.hwaddr key to control the MAC address of the bridge.

## proxy\_nat
This adds optimized UDP/TCP proxying. If the configuration allows, proxying
will be done via iptables instead of proxy devices.

## network\_nat\_order
This introduces the `ipv4.nat.order` and `ipv6.nat.order` configuration keys for LXD bridges.
Those keys control whether to put the LXD rules before or after any pre-existing rules in the chain.

## container\_full
This introduces a new recursion=2 mode for `GET /1.0/containers` which allows for the retrieval of
all container structs, including the state, snapshots and backup structs.

This effectively allows for "lxc list" to get all it needs in one query.

## candid\_authentication
This introduces the new candid.api.url config option and removes
core.macaroon.endpoint.

## backup\_compression
This introduces a new `backups.compression_algorithm` config key which
allows configuration of backup compression.

## candid\_config
This introduces the config keys `candid.domains` and `candid.expiry`. The
former allows specifying allowed/valid Candid domains, the latter makes the
macaroon's expiry configurable. The `lxc remote add` command now has a
`--domain` flag which allows specifying a Candid domain.

## nvidia\_runtime\_config
This introduces a few extra config keys when using nvidia.runtime and the libnvidia-container library.
Those keys translate pretty much directly to the matching nvidia-container environment variables:

 - nvidia.driver.capabilities => NVIDIA\_DRIVER\_CAPABILITIES
 - nvidia.require.cuda => NVIDIA\_REQUIRE\_CUDA
 - nvidia.require.driver => NVIDIA\_REQUIRE\_DRIVER

## storage\_api\_volume\_snapshots
Add support for storage volume snapshots. They work like container snapshots,
only for volumes.

This adds the following new endpoint (see [RESTful API](rest-api.md) for details):

* `GET /1.0/storage-pools/<pool>/volumes/<type>/<name>/snapshots`
* `POST /1.0/storage-pools/<pool>/volumes/<type>/<name>/snapshots`

* `GET /1.0/storage-pools/<pool>/volumes/<type>/<volume>/snapshots/<name>`
* `PUT /1.0/storage-pools/<pool>/volumes/<type>/<volume>/snapshots/<name>`
* `POST /1.0/storage-pools/<pool>/volumes/<type>/<volume>/snapshots/<name>`
* `DELETE /1.0/storage-pools/<pool>/volumes/<type>/<volume>/snapshots/<name>`

## storage\_unmapped
Introduces a new `security.unmapped` boolean on storage volumes.

Setting it to true will flush the current map on the volume and prevent
any further idmap tracking and remapping on the volume.

This can be used to share data between isolated containers after
attaching it to the container which requires write access.

## projects
Add a new project API, supporting creation, update and deletion of projects.

Projects can hold containers, profiles or images at this point and let
you get a separate view of your LXD resources by switching to it.

## candid\_config\_key
This introduces a new `candid.api.key` option which allows for setting
the expected public key for the endpoint, allowing for safe use of a
HTTP-only candid server.

## network\_vxlan\_ttl
This adds a new `tunnel.NAME.ttl` network configuration option which
makes it possible to raise the ttl on VXLAN tunnels.

## container\_incremental\_copy
This adds support for incremental container copy. When copying a container
using the `--refresh` flag, only the missing or outdated files will be
copied over. Should the target container not exist yet, a normal copy operation
is performed.

## usb\_optional\_vendorid
As the name implies, the `vendorid` field on USB devices attached to
containers has now been made optional, allowing for all USB devices to
be passed to a container (similar to what's done for GPUs).

## snapshot\_scheduling
This adds support for snapshot scheduling. It introduces three new
configuration keys: `snapshots.schedule`, `snapshots.schedule.stopped`, and
`snapshots.pattern`. Snapshots can be created automatically up to every minute.

## snapshots\_schedule\_aliases
Snapshot schedule can be configured by a comma separated list of schedule aliases.
Available aliases are `<@hourly> <@daily> <@midnight> <@weekly> <@monthly> <@annually> <@yearly> <@startup>` for instances,
and `<@hourly> <@daily> <@midnight> <@weekly> <@monthly> <@annually> <@yearly>` for storage volumes.

## container\_copy\_project
Introduces a `project` field to the container source dict, allowing for
copy/move of containers between projects.

## clustering\_server\_address
This adds support for configuring a server network address which differs from
the REST API client network address. When bootstrapping a new cluster, clients
can set the new ```cluster.https_address``` config key to specify the address of
the initial server. When joining a new server, clients can set the
```core.https_address``` config key of the joining server to the REST API
address the joining server should listen at, and set the ```server_address```
key in the ```PUT /1.0/cluster``` API to the address the joining server should
use for clustering traffic (the value of ```server_address``` will be
automatically copied to the ```cluster.https_address``` config key of the
joining server).

## clustering\_image\_replication
Enable image replication across the nodes in the cluster.
A new cluster.images_minimal_replica configuration key was introduced can be used
to specify to the minimal numbers of nodes for image replication.

## container\_protection\_shift
Enables setting the `security.protection.shift` option which prevents containers
from having their filesystem shifted.

## snapshot\_expiry
This adds support for snapshot expiration. The task is run minutely. The config
option `snapshots.expiry` takes an expression in the form of `1M 2H 3d 4w 5m
6y` (1 minute, 2 hours, 3 days, 4 weeks, 5 months, 6 years), however not all
parts have to be used.

Snapshots which are then created will be given an expiry date based on the
expression. This expiry date, defined by `expires_at`, can be manually edited
using the API or `lxc config edit`. Snapshots with a valid expiry date will be
removed when the task in run. Expiry can be disabled by setting `expires_at` to
an empty string or `0001-01-01T00:00:00Z` (zero time). This is the default if
`snapshots.expiry` is not set.

This adds the following new endpoint (see [RESTful API](rest-api.md) for details):

* `PUT /1.0/containers/<name>/snapshots/<name>`

## snapshot\_expiry\_creation
Adds `expires\_at` to container creation, allowing for override of a
snapshot's expiry at creation time.

## network\_leases\_location
Introductes a "Location" field in the leases list.
This is used when querying a cluster to show what node a particular
lease was found on.

## resources\_cpu\_socket
Add Socket field to CPU resources in case we get out of order socket information.

## resources\_gpu
Add a new GPU struct to the server resources, listing all usable GPUs on the system.

## resources\_numa
Shows the NUMA node for all CPUs and GPUs.

## kernel\_features
Exposes the state of optional kernel features through the server environment.

## id\_map\_current
This introduces a new internal `volatile.idmap.current` key which is
used to track the current mapping for the container.

This effectively gives us:

 - `volatile.last_state.idmap` => On-disk idmap
 - `volatile.idmap.current` => Current kernel map
 - `volatile.idmap.next` => Next on-disk idmap

This is required to implement environments where the on-disk map isn't
changed but the kernel map is (e.g. shiftfs).

## event\_location
Expose the location of the generation of API events.

## storage\_api\_remote\_volume\_snapshots
This allows migrating storage volumes including their snapshots.

## network\_nat\_address
This introduces the `ipv4.nat.address` and `ipv6.nat.address` configuration keys for LXD bridges.
Those keys control the source address used for outbound traffic from the bridge.

## container\_nic\_routes
This introduces the `ipv4.routes` and `ipv6.routes` properties on "nic" type devices.
This allows adding static routes on host to container's nic.

## rbac
Adds support for RBAC (role based access control). This introduces new config keys:

  * rbac.api.url
  * rbac.api.key
  * rbac.api.expiry
  * rbac.agent.url
  * rbac.agent.username
  * rbac.agent.private\_key
  * rbac.agent.public\_key

## cluster\_internal\_copy
This makes it possible to do a normal "POST /1.0/containers" to copy a
container between cluster nodes with LXD internally detecting whether a
migration is required.

## seccomp\_notify
If the kernel supports seccomp-based syscall interception LXD can be notified
by a container that a registered syscall has been performed. LXD can then
decide to trigger various actions.

## lxc\_features
This introduces the `lxc_features` section output from the `lxc info` command
via the `GET /1.0/` route. It outputs the result of checks for key features being present in the
underlying LXC library.

## container\_nic\_ipvlan
This introduces the `ipvlan` "nic" device type.

## network\_vlan\_sriov
This introduces VLAN (`vlan`) and MAC filtering (`security.mac_filtering`) support for SR-IOV devices.

## storage\_cephfs
Add support for CEPHFS as a storage pool driver. This can only be used
for custom volumes, images and containers should be on CEPH (RBD)
instead.

## container\_nic\_ipfilter
This introduces container IP filtering (`security.ipv4_filtering` and `security.ipv6_filtering`) support for `bridged` nic devices.

## resources\_v2
Rework the resources API at /1.0/resources, especially:

 - CPU
   - Fix reporting to track sockets, cores and threads
   - Track NUMA node per core
   - Track base and turbo frequency per socket
   - Track current frequency per core
   - Add CPU cache information
   - Export the CPU architecture
   - Show online/offline status of threads
 - Memory
   - Add hugepages tracking
   - Track memory consumption per NUMA node too
 - GPU
   - Split DRM information to separate struct
   - Export device names and nodes in DRM struct
   - Export device name and node in NVIDIA struct
   - Add SR-IOV VF tracking

## container\_exec\_user\_group\_cwd
Adds support for specifying User, Group and Cwd during `POST /1.0/containers/NAME/exec`.

## container\_syscall\_intercept
Adds the `security.syscalls.intercept.\*` configuration keys to control
what system calls will be interecepted by LXD and processed with
elevated permissions.

## container\_disk\_shift
Adds the `shift` property on `disk` devices which controls the use of the shiftfs overlay.

## storage\_shifted
Introduces a new `security.shifted` boolean on storage volumes.

Setting it to true will allow multiple isolated containers to attach the
same storage volume while keeping the filesystem writable from all of
them.

This makes use of shiftfs as an overlay filesystem.

## resources\_infiniband
Export infiniband character device information (issm, umad, uverb) as part of the resources API.

## daemon\_storage
This introduces two new configuration keys `storage.images_volume` and
`storage.backups_volume` to allow for a storage volume on an existing
pool be used for storing the daemon-wide images and backups artifacts.

## instances
This introduces the concept of instances, of which currently the only type is "container".

## image\_types
This introduces support for a new Type field on images, indicating what type of images they are.

## resources\_disk\_sata
Extends the disk resource API struct to include:

 - Proper detection of sata devices (type)
 - Device path
 - Drive RPM
 - Block size
 - Firmware version
 - Serial number

## clustering\_roles
This adds a new `roles` attribute to cluster entries, exposing a list of
roles that the member serves in the cluster.

## images\_expiry
This allows for editing of the expiry date on images.

## resources\_network\_firmware
Adds a FirmwareVersion field to network card entries.

## backup\_compression\_algorithm
This adds support for a `compression_algorithm` property when creating a backup (`POST /1.0/containers/<name>/backups`).

Setting this property overrides the server default value (`backups.compression_algorithm`).

## ceph\_data\_pool\_name
This adds support for an optional argument (`ceph.osd.data_pool_name`) when creating
storage pools using Ceph RBD, when this argument is used the pool will store it's
actual data in the pool specified with `data_pool_name` while keeping the metadata
in the pool specified by `pool_name`.

## container\_syscall\_intercept\_mount
Adds the `security.syscalls.intercept.mount`,
`security.syscalls.intercept.mount.allowed`, and
`security.syscalls.intercept.mount.shift` configuration keys to control whether
and how the mount system call will be interecepted by LXD and processed with
elevated permissions.

## compression\_squashfs
Adds support for importing/exporting of images/backups using SquashFS file system format.

## container\_raw\_mount
This adds support for passing in raw mount options for disk devices.

## container\_nic\_routed
This introduces the `routed` "nic" device type.

## container\_syscall\_intercept\_mount\_fuse
Adds the `security.syscalls.intercept.mount.fuse` key. It can be used to
redirect filesystem mounts to their fuse implementation. To this end, set e.g.
`security.syscalls.intercept.mount.fuse=ext4=fuse2fs`.

## container\_disk\_ceph
This allows for existing a CEPH RDB or FS to be directly connected to a LXD container.

## virtual\_machines
Add virtual machine support.

## image\_profiles
Allows a list of profiles to be applied to an image when launching a new container.

## clustering\_architecture
This adds a new `architecture` attribute to cluster members which indicates a cluster
member's architecture.

## resources\_disk\_id
Add a new device\_id field in the disk entries on the resources API.

## storage\_lvm\_stripes
This adds the ability to use LVM stripes on normal volumes and thin pool volumes.

## vm\_boot\_priority
Adds a `boot.priority` property on nic and disk devices to control the boot order.

## unix\_hotplug\_devices
Adds support for unix char and block device hotplugging.

## api\_filtering
Adds support for filtering the result of a GET request for instances and images.

## instance\_nic\_network
Adds support for the `network` property on a NIC device to allow a NIC to be linked to a managed network.
This allows it to inherit some of the network's settings and allows better validation of IP settings.

## clustering\_sizing
Support specifying a custom values for database voters and standbys.
The new `cluster.max_voters` and `cluster.max_standby` configuration keys were introduced
to specify to the ideal number of database voter and standbys.

## firewall\_driver
Adds the `Firewall` property to the ServerEnvironment struct indicating the firewall driver being used.

## storage\_lvm\_vg\_force\_reuse
Introduces the ability to create a storage pool from an existing non-empty volume group.
This option should be used with care, as LXD can then not guarantee that volume name conflicts won't occur
with non-LXD created volumes in the same volume group.
This could also potentially lead to LXD deleting a non-LXD volume should name conflicts occur.

## container\_syscall\_intercept\_hugetlbfs
When mount syscall interception is enabled and hugetlbfs is specified as an
allowed filesystem type LXD will mount a separate hugetlbfs instance for the
container with the uid and gid mount options set to the container's root uid
and gid. This ensures that processes in the container can use hugepages.

## limits\_hugepages
This allows to limit the number of hugepages a container can use through the
hugetlb cgroup. This means the hugetlb cgroup needs to be available. Note, that
limiting hugepages is recommended when intercepting the mount syscall for the
hugetlbfs filesystem to avoid allowing the container to exhaust the host's
hugepages resources.

## container\_nic\_routed\_gateway
This introduces the `ipv4.gateway` and `ipv6.gateway` NIC config keys that can take a value of either "auto" or
"none". The default value for the key if unspecified is "auto". This will cause the current behaviour of a default
gateway being added inside the container and the same gateway address being added to the host-side interface.
If the value is set to "none" then no default gateway nor will the address be added to the host-side interface.
This allows multiple routed NIC devices to be added to a container.

## projects\_restrictions
This introduces support for the `restricted` configuration key on project, which
can prevent the use of security-sensitive features in a project.

## custom\_volume\_snapshot\_expiry
This allows custom volume snapshots to expiry.
Expiry dates can be set individually, or by setting the `snapshots.expiry` config key on the parent custom volume which then automatically applies to all created snapshots.

## volume\_snapshot\_scheduling
This adds support for custom volume snapshot scheduling. It introduces two new
configuration keys: `snapshots.schedule` and
`snapshots.pattern`. Snapshots can be created automatically up to every minute.

## trust\_ca\_certificates
This allows for checking client certificates trusted by the provided CA (`server.ca`).
It can be enabled by setting `core.trust_ca_certificates` to true.
If enabled, it will perform the check, and bypass the trusted password if true.
An exception will be made if the connecting client certificate is in the provided CRL (`ca.crl`).
In this case, it will ask for the password.

## snapshot\_disk\_usage
This adds a new `size` field to the output of `/1.0/instances/<name>/snapshots/<snapshot>` which represents the disk usage of the snapshot.

## clustering\_edit\_roles
This adds a writable endpoint for cluster members, allowing the editing of their roles.

## container\_nic\_routed\_host\_address
This introduces the `ipv4.host_address` and `ipv6.host_address` NIC config keys that can be used to control the
host-side veth interface's IP addresses. This can be useful when using multiple routed NICs at the same time and
needing a predictable next-hop address to use.

This also alters the behaviour of `ipv4.gateway` and `ipv6.gateway` NIC config keys. When they are set to "auto"
the container will have its default gateway set to the value of `ipv4.host_address` or `ipv6.host_address` respectively.

The default values are:

`ipv4.host_address`: 169.254.0.1
`ipv6.host_address`: fe80::1

This is backward compatible with the previous default behaviour.

## container\_nic\_ipvlan\_gateway
This introduces the `ipv4.gateway` and `ipv6.gateway` NIC config keys that can take a value of either "auto" or
"none". The default value for the key if unspecified is "auto". This will cause the current behaviour of a default
gateway being added inside the container and the same gateway address being added to the host-side interface.
If the value is set to "none" then no default gateway nor will the address be added to the host-side interface.
This allows multiple ipvlan NIC devices to be added to a container.

## resources\_usb\_pci
This adds USB and PCI devices to the output of `/1.0/resources`.

## resources\_cpu\_threads\_numa
This indicates that the numa\_node field is now recorded per-thread
rather than per core as some hardware apparently puts threads in
different NUMA domains.

## resources\_cpu\_core\_die
Exposes the `die_id` information on each core.

## api\_os
This introduces two new fields in `/1.0`, `os` and `os_version`.

Those are taken from the os-release data on the system.

## container\_nic\_routed\_host\_table
This introduces the `ipv4.host_table` and `ipv6.host_table` NIC config keys that can be used to add static routes
for the instance's IPs to a custom policy routing table by ID.

## container\_nic\_ipvlan\_host\_table
This introduces the `ipv4.host_table` and `ipv6.host_table` NIC config keys that can be used to add static routes
for the instance's IPs to a custom policy routing table by ID.

## container\_nic\_ipvlan\_mode
This introduces the `mode` NIC config key that can be used to switch the `ipvlan` mode into either `l2` or `l3s`.
If not specified, the default value is `l3s` (which is the old behavior).

In `l2` mode the `ipv4.address` and `ipv6.address` keys will accept addresses in either CIDR or singular formats.
If singular format is used, the default subnet size is taken to be /24 and /64 for IPv4 and IPv6 respectively.

In `l2` mode the `ipv4.gateway` and `ipv6.gateway` keys accept only a singular IP address.

## resources\_system
This adds system information to the output of `/1.0/resources`.

## images\_push\_relay
This adds the push and relay modes to image copy.
It also introduces the following new endpoint:
 - `POST 1.0/images/<fingerprint>/export`

## network\_dns\_search
This introduces the `dns.search` config option on networks.

## container\_nic\_routed\_limits
This introduces `limits.ingress`, `limits.egress` and `limits.max` for routed NICs.

## instance\_nic\_bridged\_vlan
This introduces the `vlan` and `vlan.tagged` settings for `bridged` NICs.

`vlan` specifies the untagged VLAN to join, and `vlan.tagged` is a comma delimited list of tagged VLANs to join.

## network\_state\_bond\_bridge
This adds a "bridge" and "bond" section to the /1.0/networks/NAME/state API.

Those contain additional state information relevant to those particular types.

Bond:

 - Mode
 - Transmit hash
 - Up delay
 - Down delay
 - MII frequency
 - MII state
 - Lower devices

Bridge:

 - ID
 - Forward delay
 - STP mode
 - Default VLAN
 - VLAN filtering
 - Upper devices

## resources\_cpu\_isolated
Add an `Isolated` property on CPU threads to indicate if the thread is
physically `Online` but is configured not to accept tasks.

## usedby\_consistency
This extension indicates that UsedBy should now be consistent with
suitable ?project= and ?target= when appropriate.

The 5 entities that have UsedBy are:
 - Profiles
 - Projects
 - Networks
 - Storage pools
 - Storage volumes

## custom\_block\_volumes

This adds support for creating and attaching custom block volumes to instances.
It introduces the new `--type` flag when creating custom storage volumes, and accepts the values `fs` and `block`.

## clustering\_failure\_domains

This extension adds a new `failure\_domain` field to the `PUT /1.0/cluster/<node>` API,
which can be used to set the failure domain of a node.

## container\_syscall\_filtering\_allow\_deny\_syntax
A number of new syscalls related container configuration keys were updated.

 * `security.syscalls.deny_default`
 * `security.syscalls.deny_compat`
 * `security.syscalls.deny`
 * `security.syscalls.allow`

## resources\_gpu\_mdev
Expose available mediated device profiles and devices in /1.0/resources.

## console\_vga\_type

This extends the `/1.0/console` endpoint to take a `?type=` argument, which can
be set to `console` (default) or `vga` (the new type added by this extension).

When POST'ing to `/1.0/<instance name>/console?type=vga` the data websocket
returned by the operation in the metadata field will be a bidirectional proxy
attached to a SPICE unix socket of the target virtual machine.

## projects\_limits\_disk
Add `limits.disk` to the available project configuration keys. If set, it limits
the total amount of disk space that instances volumes, custom volumes and images
volumes can use in the project.

## network\_type\_macvlan
Adds support for additional network type `macvlan` and adds `parent` configuration key for this network type to
specify which parent interface should be used for creating NIC device interfaces on top of.

Also adds `network` configuration key support for `macvlan` NICs to allow them to specify the associated network of
the same type that they should use as the basis for the NIC device.

## network\_type\_sriov
Adds support for additional network type `sriov` and adds `parent` configuration key for this network type to
specify which parent interface should be used for creating NIC device interfaces on top of.

Also adds `network` configuration key support for `sriov` NICs to allow them to specify the associated network of
the same type that they should use as the basis for the NIC device.

## container\_syscall\_intercept\_bpf\_devices
This adds support to intercept the bpf syscall in containers. Specifically, it allows to manage device cgroup bpf programs.

## network\_type\_ovn
Adds support for additional network type `ovn` with the ability to specify a `bridge` type network as the `parent`.

Introduces a new NIC device type of `ovn` which allows the `network` configuration key to specify which `ovn`
type network they should connect to.

Also introduces two new global config keys that apply to all `ovn` networks and NIC devices:

 - network.ovn.integration\_bridge - the OVS integration bridge to use.
 - network.ovn.northbound\_connection - the OVN northbound database connection string.

## projects\_networks
Adds the `features.networks` config key to projects and the ability for a project to hold networks.

## projects\_networks\_restricted\_uplinks
Adds the `restricted.networks.uplinks` project config key to indicate (as a comma delimited list) which networks
the networks created inside the project can use as their uplink network.

## custom\_volume\_backup
Add custom volume backup support.

This includes the following new endpoints (see [RESTful API](rest-api.md) for details):

* `GET /1.0/storage-pools/<pool>/<type>/<volume>/backups`
* `POST /1.0/storage-pools/<pool>/<type>/<volume>/backups`

* `GET /1.0/storage-pools/<pool>/<type>/<volume>/backups/<name>`
* `POST /1.0/storage-pools/<pool>/<type>/<volume>/backups/<name>`
* `DELETE /1.0/storage-pools/<pool>/<type>/<volume>/backups/<name>`

* `GET /1.0/storage-pools/<pool>/<type>/<volume>/backups/<name>/export`

The following existing endpoint has been modified:

 * `POST /1.0/storage-pools/<pool>/<type>/<volume>` accepts the new source type `backup`

## backup\_override\_name
Adds `Name` field to `InstanceBackupArgs` to allow specifying a different instance name when restoring a backup.

Adds `Name` and `PoolName` fields to `StoragePoolVolumeBackupArgs` to allow specifying a different volume name
when restoring a custom volume backup.

## storage\_rsync\_compression
Adds `rsync.compression` config key to storage pools. This key can be used
to disable compression in rsync while migrating storage pools.

## network\_type\_physical
Adds support for additional network type `physical` that can be used as an uplink for `ovn` networks.

The interface specified by `parent` on the `physical` network will be connected to the `ovn` network's gateway.

## network\_ovn\_external\_subnets
Adds support for `ovn` networks to use external subnets from uplink networks.

Introduces the `ipv4.routes` and `ipv6.routes` setting on `physical` networks that defines the external routes
allowed to be used in child OVN networks in their `ipv4.routes.external` and `ipv6.routes.external` settings.

Introduces the `restricted.networks.subnets` project setting that specifies which external subnets are allowed to
be used by OVN networks inside the project (if not set then all routes defined on the uplink network are allowed).

## network\_ovn\_nat
Adds support for `ipv4.nat` and `ipv6.nat` settings on `ovn` networks.

When creating the network if these settings are unspecified, and an equivalent IP address is being generated for
the subnet, then the appropriate NAT setting will added set to `true`.

If the setting is missing then the value is taken as `false`.

## network\_ovn\_external\_routes\_remove
Removes the settings `ipv4.routes.external` and `ipv6.routes.external` from `ovn` networks.

The equivalent settings on the `ovn` NIC type can be used instead for this, rather than having to specify them
both at the network and NIC level.

## tpm\_device\_type
This introduces the `tpm` device type.

## storage\_zfs\_clone\_copy\_rebase
This introduces `rebase` as a value for zfs.clone\_copy causing LXD to
track down any "image" dataset in the ancestry line and then perform
send/receive on top of that.

## gpu\_mdev
This adds support for virtual GPUs. It introduces the `mdev` config key for GPU devices which takes
a supported mdev type, e.g. i915-GVTg\_V5\_4.

## resources\_pci\_iommu
This adds the IOMMUGroup field for PCI entries in the resources API.

## resources\_network\_usb
Adds the usb\_address field to the network card entries in the resources API.

## resources\_disk\_address
Adds the usb\_address and pci\_address fields to the disk entries in the resources API.

## network\_physical\_ovn\_ingress\_mode
Adds `ovn.ingress_mode` setting for `physical` networks.

Sets the method that OVN NIC external IPs will be advertised on uplink network.

Either `l2proxy` (proxy ARP/NDP) or `routed`.

## network\_ovn\_dhcp
Adds `ipv4.dhcp` and `ipv6.dhcp` settings for `ovn` networks.

Allows DHCP (and RA for IPv6) to be disabled. Defaults to on.

## network\_physical\_routes\_anycast
Adds `ipv4.routes.anycast` and `ipv6.routes.anycast` boolean settings for `physical` networks. Defaults to false.

Allows OVN networks using physical network as uplink to relax external subnet/route overlap detection when used
with `ovn.ingress_mode=routed`.

## projects\_limits\_instances
Adds `limits.instances` to the available project configuration keys. If set, it
limits the total number of instances (VMs and containers) that can be used in the project.

## network\_state\_vlan
This adds a "vlan" section to the /1.0/networks/NAME/state API.

Those contain additional state information relevant to VLAN interfaces:
 - lower\_device
 - vid

## instance\_nic\_bridged\_port\_isolation
This adds the `security.port_isolation` field for bridged NIC instances.

## instance\_bulk\_state\_change
Adds the following endpoint for bulk state change (see [RESTful API](rest-api.md) for details):

* `PUT /1.0/instances`

## network\_gvrp
This adds an optional `gvrp` property to `macvlan` and `physical` networks,
and to `ipvlan`, `macvlan`, `routed` and `physical` NIC devices.

When set, this specifies whether the VLAN should be registered using GARP VLAN
Registration Protocol. Defaults to false.

## instance\_pool\_move
This adds a `pool` field to the `POST /1.0/instances/NAME` API,
allowing for easy move of an instance root disk between pools.

## gpu\_sriov
This adds support for SR-IOV enabled GPUs.
It introduces the `sriov` gpu type property.

## pci\_device\_type
This introduces the `pci` device type.

## storage\_volume\_state
Add new `/1.0/storage-pools/POOL/volumes/VOLUME/state` API endpoint to get usage data on a volume.

## network\_acl
This adds the concept of network ACLs to API under the API endpoint prefix `/1.0/network-acls`.

## migration\_stateful
Add a new `migration.stateful` config key.

## disk\_state\_quota
This introduces the `size.state` device config key on `disk` devices.

## storage\_ceph\_features
Adds a new `ceph.rbd.features` config key on storage pools to control the RBD features used for new volumes.

## projects\_compression
Adds new `backups.compression_algorithm` and `images.compression_algorithm` config keys which
allows configuration of backup and image compression per-project.

## projects\_images\_remote\_cache\_expiry
Add new `images.remote_cache_expiry` config key to projects,
allowing for set number of days after which an unused cached remote image will be flushed.

## certificate\_project
Adds a new `restricted` property to certificates in the API as well as
`projects` holding a list of project names that the certificate has
access to.

## network\_ovn\_acl
Adds a new `security.acls` property to OVN networks and OVN NICs, allowing Network ACLs to be applied.

## projects\_images\_auto\_update
Adds new `images.auto_update_cached` and `images.auto_update_interval` config keys which
allows configuration of images auto update in projects

## projects\_restricted\_cluster\_target
Adds new `restricted.cluster.target` config key to project which prevent the user from using --target
to specify what cluster member to place a workload on or the ability to move a workload between members.

## images\_default\_architecture
Adds new `images.default_architecture` global config key and matching per-project key which lets user tell LXD
what architecture to go with when no specific one is specified as part of the image request.

## network\_ovn\_acl\_defaults
Adds new `security.acls.default.{in,e}gress.action` and `security.acls.default.{in,e}gress.logged` config keys for
OVN networks and NICs. This replaces the removed ACL `default.action` and `default.logged` keys.

## gpu\_mig
This adds support for NVIDIA MIG. It introduces the `mig` gputype and associaetd config keys.

## project\_usage
Adds an API endpoint to get current resource allocations in a project.
Accessible at API `GET /1.0/projects/<name>/state`.

## network\_bridge\_acl
Adds a new `security.acls` config key to `bridge` networks, allowing Network ACLs to be applied.

Also adds `security.acls.default.{in,e}gress.action` and `security.acls.default.{in,e}gress.logged` config keys for
specifying the default behaviour for unmatched traffic.

## warnings
Warning API for LXD.

This includes the following endpoints (see  [Restful API](rest-api.md) for details):

* `GET /1.0/warnings`

* `GET /1.0/warnings/<uuid>`
* `PUT /1.0/warnings/<uuid>`
* `DELETE /1.0/warnings/<uuid>`

## projects\_restricted\_backups\_and\_snapshots
Adds new `restricted.backups` and `restricted.snapshots` config keys to project which
prevents the user from creation of backups and snapshots.

## clustering\_join\_token
Adds `POST /1.0/cluster/members` API endpoint for requesting a join token used when adding new cluster members
without using the trust password.

## clustering\_description
Adds an editable description to the cluster members.

## server\_trusted\_proxy
This introduces support for `core.https_trusted_proxy` which has LXD
parse a HAProxy style connection header on such connections and if
present, will rewrite the request's source address to that provided by
the proxy server.

## clustering\_update\_cert
Adds `PUT /1.0/cluster/certificate` endpoint for updating the cluster
certificate across the whole cluster

## storage\_api\_project
This adds support for copy/move custom storage volumes between projects.

## server\_instance\_driver\_operational
This modifies the `driver` output for the `/1.0` endpoint to only include drivers which are actually supported and
operational on the server (as opposed to being included in LXD but not operational on the server).

## server\_supported\_storage\_drivers
This adds supported storage driver info to server environment info.

## event\_lifecycle\_requestor\_address
Adds a new address field to lifecycle requestor.

## clustering\_evacuation
Adds `POST /1.0/cluster/members/<name>/state` endpoint for evacuating and restoring cluster members.
It also adds the config keys `cluster.evacuate` and `volatile.evacuate.origin` for setting the evacuation method (`auto`, `stop` or `migrate`) and the origin of any migrated instance respectively.
