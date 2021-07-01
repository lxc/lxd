package api

// StorageVolumesPost represents the fields of a new LXD storage pool volume
//
// swagger:model
//
// API extension: storage
type StorageVolumesPost struct {
	StorageVolumePut `yaml:",inline"`

	// Volume name
	// Example: foo
	Name string `json:"name" yaml:"name"`

	// Volume type (container, custom, image or virtual-machine)
	// Example: custom
	Type string `json:"type" yaml:"type"`

	// Migration source
	//
	// API extension: storage_api_local_volume_handling
	Source StorageVolumeSource `json:"source" yaml:"source"`

	// Volume content type (filesystem or block)
	// Example: filesystem
	//
	// API extension: custom_block_volumes
	ContentType string `json:"content_type" yaml:"content_type"`
}

// StorageVolumePost represents the fields required to rename a LXD storage pool volume
//
// swagger:model
//
// API extension: storage_api_volume_rename
type StorageVolumePost struct {
	// New volume name
	// Example: foo
	Name string `json:"name" yaml:"name"`

	// New storage pool
	// Example: remote
	//
	// API extension: storage_api_local_volume_handling
	Pool string `json:"pool,omitempty" yaml:"pool,omitempty"`

	// Initiate volume migration
	// Example: false
	//
	// API extension: storage_api_remote_volume_handling
	Migration bool `json:"migration" yaml:"migration"`

	// Migration target (for push mode)
	//
	// API extension: storage_api_remote_volume_handling
	Target *StorageVolumePostTarget `json:"target" yaml:"target"`

	// Whether snapshots should be discarded (migration only)
	// Example: false
	//
	// API extension: storage_api_remote_volume_snapshots
	VolumeOnly bool `json:"volume_only" yaml:"volume_only"`
}

// StorageVolumePostTarget represents the migration target host and operation
//
// swagger:model
//
// API extension: storage_api_remote_volume_handling
type StorageVolumePostTarget struct {
	// The certificate of the migration target
	// Example: X509 PEM certificate
	Certificate string `json:"certificate" yaml:"certificate"`

	// Remote operation URL (for migration)
	// Example: https://1.2.3.4:8443/1.0/operations/1721ae08-b6a8-416a-9614-3f89302466e1
	Operation string `json:"operation,omitempty" yaml:"operation,omitempty"`

	// Migration websockets credentials
	// Example: {"migration": "random-string"}
	Websockets map[string]string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

// StorageVolume represents the fields of a LXD storage volume.
//
// swagger:model
//
// API extension: storage
type StorageVolume struct {
	StorageVolumePut `yaml:",inline"`

	// Volume name
	// Example: foo
	Name string `json:"name" yaml:"name"`

	// Volume type
	// Example: custom
	Type string `json:"type" yaml:"type"`

	// List of URLs of objects using this storage volume
	// Example: ["/1.0/instances/blah"]
	UsedBy []string `json:"used_by" yaml:"used_by"`

	// What cluster member this record was found on
	// Example: lxd01
	//
	// API extension: clustering
	Location string `json:"location" yaml:"location"`

	// Volume content type (filesystem or block)
	// Example: filesystem
	//
	// API extension: custom_block_volumes
	ContentType string `json:"content_type" yaml:"content_type"`
}

// StorageVolumePut represents the modifiable fields of a LXD storage volume
//
// swagger:model
//
// API extension: storage
type StorageVolumePut struct {
	// Storage volume configuration map (refer to doc/storage.md)
	// Example: {"zfs.remove_snapshots": "true", "size": "50GiB"}
	Config map[string]string `json:"config" yaml:"config"`

	// Description of the storage volume
	// Example: My custom volume
	//
	// API extension: entity_description
	Description string `json:"description" yaml:"description"`

	// Name of a snapshot to restore
	// Example: snap0
	//
	// API extension: storage_api_volume_snapshots
	Restore string `json:"restore,omitempty" yaml:"restore,omitempty"`
}

// StorageVolumeSource represents the creation source for a new storage volume
//
// swagger:model
//
// API extension: storage_api_local_volume_handling
type StorageVolumeSource struct {
	// Source volume name (for copy)
	// Example: foo
	Name string `json:"name" yaml:"name"`

	// Source type (copy or migration)
	// Example: copy
	Type string `json:"type" yaml:"type"`

	// Source storage pool (for copy)
	// Example: local
	Pool string `json:"pool" yaml:"pool"`

	// Certificate (for migration)
	// Example: X509 PEM certificate
	//
	// API extension: storage_api_remote_volume_handling
	Certificate string `json:"certificate" yaml:"certificate"`

	// Whether to use pull or push mode (for migration)
	// Example: pull
	//
	// API extension: storage_api_remote_volume_handling
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`

	// Remote operation URL (for migration)
	// Example: https://1.2.3.4:8443/1.0/operations/1721ae08-b6a8-416a-9614-3f89302466e1
	//
	// API extension: storage_api_remote_volume_handling
	Operation string `json:"operation,omitempty" yaml:"operation,omitempty"`

	// Map of migration websockets (for migration)
	// Example: {"rsync": "RANDOM-STRING"}
	//
	// API extension: storage_api_remote_volume_handling
	Websockets map[string]string `json:"secrets,omitempty" yaml:"secrets,omitempty"`

	// Whether snapshots should be discarded (for migration)
	// Example: false
	//
	// API extension: storage_api_volume_snapshots
	VolumeOnly bool `json:"volume_only" yaml:"volume_only"`

	// Whether existing destination volume should be refreshed
	// Example: false
	//
	// API extension: custom_volume_refresh
	Refresh bool `json:"refresh" yaml:"refresh"`

	// Source project name
	// Example: foo
	//
	// API extension: storage_api_project
	Project string `json:"project,omitempty" yaml:"project,omitempty"`
}
