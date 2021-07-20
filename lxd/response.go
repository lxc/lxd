package main

import (
	"net/http"

	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/response"
)

func forwardedResponseToNode(d *Daemon, r *http.Request, node string) response.Response {
	// Figure out the address of the target node (which is possibly
	// this very same node).
	address, err := cluster.ResolveTarget(d.cluster, node)
	if err != nil {
		return response.SmartError(err)
	}

	if address != "" {
		// Forward the response.
		client, err := cluster.Connect(address, d.endpoints.NetworkCert(), d.serverCert(), r, false)
		if err != nil {
			return response.SmartError(err)
		}
		return response.ForwardedResponse(client, r)
	}

	return nil
}

// forwardedResponseIfTargetIsRemote redirects a request to the request has a
// targetNode parameter pointing to a node which is not the local one.
func forwardedResponseIfTargetIsRemote(d *Daemon, r *http.Request) response.Response {
	targetNode := queryParam(r, "target")
	if targetNode == "" {
		return nil
	}

	return forwardedResponseToNode(d, r, targetNode)
}

// forwardedResponseIfInstanceIsRemote redirects a request to the node running
// the container with the given name. If the container is local, nothing gets
// done and nil is returned.
func forwardedResponseIfInstanceIsRemote(d *Daemon, r *http.Request, project, name string, instanceType instancetype.Type) (response.Response, error) {
	client, err := cluster.ConnectIfInstanceIsRemote(d.cluster, project, name, d.endpoints.NetworkCert(), d.serverCert(), r, instanceType)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	return response.ForwardedResponse(client, r), nil
}

// forwardedResponseIfVolumeIsRemote redirects a request to the node hosting
// the volume with the given pool ID, name and type. If the container is local,
// nothing gets done and nil is returned. If more than one node has a matching
// volume, an error is returned.
//
// This is used when no targetNode is specified, and saves users some typing
// when the volume name/type is unique to a node.
func forwardedResponseIfVolumeIsRemote(d *Daemon, r *http.Request, poolName string, projectName string, volumeName string, volumeType int) response.Response {
	if queryParam(r, "target") != "" {
		return nil
	}

	client, err := cluster.ConnectIfVolumeIsRemote(d.State(), poolName, projectName, volumeName, volumeType, d.endpoints.NetworkCert(), d.serverCert(), r)
	if err != nil {
		return response.SmartError(err)
	}

	if client == nil {
		return nil
	}

	return response.ForwardedResponse(client, r)
}
