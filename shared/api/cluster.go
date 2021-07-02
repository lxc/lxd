package api

import (
	"encoding/base64"
	"encoding/json"
)

// Cluster represents high-level information about a LXD cluster.
//
// swagger:model
//
// API extension: clustering
type Cluster struct {
	// Name of the cluster member answering the request
	// Example: lxd01
	ServerName string `json:"server_name" yaml:"server_name"`

	// Whether clustering is enabled
	// Example: true
	Enabled bool `json:"enabled" yaml:"enabled"`

	// List of member configuration keys (used during join)
	// Example: []
	//
	// API extension: clustering_join
	MemberConfig []ClusterMemberConfigKey `json:"member_config" yaml:"member_config"`
}

// ClusterMemberConfigKey represents a single config key that a new member of
// the cluster is required to provide when joining.
//
// The Value field is empty when getting clustering information with GET
// /1.0/cluster, and should be filled by the joining node when performing a PUT
// /1.0/cluster join request.
//
// swagger:model
//
// API extension: clustering_join
type ClusterMemberConfigKey struct {
	// The kind of configuration key (network, storage-pool, ...)
	// Example: storage-pool
	Entity string `json:"entity" yaml:"entity"`

	// The name of the object requiring this key
	// Example: local
	Name string `json:"name" yaml:"name"`

	// The name of the key
	// Example: source
	Key string `json:"key" yaml:"key"`

	// The value on the answering cluster member
	// Example: /dev/sdb
	Value string `json:"value" yaml:"value"`

	// A human friendly description key
	// Example: "source" property for storage pool "local"
	Description string `json:"description" yaml:"description"`
}

// ClusterPut represents the fields required to bootstrap or join a LXD
// cluster.
//
// swagger:model
//
// API extension: clustering
type ClusterPut struct {
	Cluster `yaml:",inline"`

	// The address of the cluster you wish to join
	// Example: 10.0.0.1:8443
	ClusterAddress string `json:"cluster_address" yaml:"cluster_address"`

	// The expected certificate (X509 PEM encoded) for the cluster
	// Example: X509 PEM certificate
	ClusterCertificate string `json:"cluster_certificate" yaml:"cluster_certificate"`

	// The local address to use for cluster communication
	// Example: 10.0.0.2:8443
	//
	// API extension: clustering_join
	ServerAddress string `json:"server_address" yaml:"server_address"`

	// The trust password of the cluster you're trying to join
	// Example: blah
	//
	// API extension: clustering_join
	ClusterPassword string `json:"cluster_password" yaml:"cluster_password"`
}

// ClusterMembersPost represents the fields required to request a join token to add a member to the cluster.
//
// swagger:model
//
// API extension: clustering_join_token
type ClusterMembersPost struct {
	// The name of the new cluster member
	// Example: lxd02
	ServerName string `json:"server_name" yaml:"server_name"`
}

// ClusterMemberJoinToken represents the fields contained within an encoded cluster member join token.
//
// swagger:model
//
// API extension: clustering_join_token
type ClusterMemberJoinToken struct {
	// The name of the new cluster member
	// Example: lxd02
	ServerName string `json:"server_name" yaml:"server_name"`

	// The fingerprint of the network certificate
	// Example: 57bb0ff4340b5bb28517e062023101adf788c37846dc8b619eb2c3cb4ef29436
	Fingerprint string `json:"fingerprint" yaml:"fingerprint"`

	// The addresses of existing online cluster members
	// Example: ["10.98.30.229:8443"]
	Addresses []string `json:"addresses" yaml:"addresses"`

	// The random join secret.
	// Example: 2b2284d44db32675923fe0d2020477e0e9be11801ff70c435e032b97028c35cd
	Secret string `json:"secret" yaml:"secret"`
}

// String encodes the cluster member join token as JSON and then Base64.
func (t *ClusterMemberJoinToken) String() string {
	joinTokenJSON, err := json.Marshal(t)
	if err != nil {
		return ""
	}

	return base64.StdEncoding.EncodeToString(joinTokenJSON)
}

// ClusterMemberPost represents the fields required to rename a LXD node.
//
// swagger:model
//
// API extension: clustering
type ClusterMemberPost struct {
	// The new name of the cluster member
	// Example: lxd02
	ServerName string `json:"server_name" yaml:"server_name"`
}

// ClusterMember represents the a LXD node in the cluster.
//
// swagger:model
//
// API extension: clustering
type ClusterMember struct {
	ClusterMemberPut `yaml:",inline"`

	// Name of the cluster member
	// Example: lxd01
	ServerName string `json:"server_name" yaml:"server_name"`

	// URL at which the cluster member can be reached
	// Example: https://10.0.0.1:8443
	URL string `json:"url" yaml:"url"`

	// Whether the cluster member is a database server
	// Example: true
	Database bool `json:"database" yaml:"database"`

	// Current status
	// Example: Online
	Status string `json:"status" yaml:"status"`

	// Additional status information
	// Example: fully operational
	Message string `json:"message" yaml:"message"`

	// The primary architecture of the cluster member
	// Example: x86_64
	//
	// API extension: clustering_architecture
	Architecture string `json:"architecture" yaml:"architecture"`
}

// Writable converts a full Profile struct into a ProfilePut struct (filters read-only fields)
func (member *ClusterMember) Writable() ClusterMemberPut {
	return member.ClusterMemberPut
}

// ClusterMemberPut represents the the modifiable fields of a LXD cluster member
//
// swagger:model
//
// API extension: clustering_edit_roles
type ClusterMemberPut struct {
	// List of roles held by this cluster member
	// Example: ["database"]
	//
	// API extension: clustering_roles
	Roles []string `json:"roles" yaml:"roles"`

	// Name of the failure domain for this cluster member
	// Example: rack1
	//
	// API extension: clustering_failure_domains
	FailureDomain string `json:"failure_domain" yaml:"failure_domain"`

	// Cluster member description
	// Example: AMD Epyc 32c/64t
	//
	// API extension: clustering_description
	Description string `json:"description" yaml:"description"`
}

// ClusterCertificatePut represents the certificate and key pair for all members in a LXD Cluster
//
// swagger:model
//
// API extension: clustering_update_certs
type ClusterCertificatePut struct {
	// The new certificate (X509 PEM encoded) for the cluster
	// Example: X509 PEM certificate
	ClusterCertificate string `json:"cluster_certificate" yaml:"cluster_certificate"`

	// The new certificate key (X509 PEM encoded) for the cluster
	// Example: X509 PEM certificate key
	ClusterCertificateKey string `json:"cluster_certificate_key" yaml:"cluster_certificate_key"`
}

// ClusterMemberStatePost represents the fields required to evacuate a cluster member.
//
// swagger:model
//
// API extension: clustering_evacuation
type ClusterMemberStatePost struct {
	// The action to be performed. Valid actions are "evacuate" and "restore".
	// Example: evacuate
	Action string `json:"action" yaml:"action"`
}
