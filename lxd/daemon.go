package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	sqldriver "database/sql/driver"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/canonical/candid/candidclient"
	dqliteclient "github.com/canonical/go-dqlite/client"
	"github.com/canonical/go-dqlite/driver"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	liblxc "gopkg.in/lxc/go-lxc.v2"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	"gopkg.in/macaroon-bakery.v2/bakery/identchecker"
	"gopkg.in/macaroon-bakery.v2/httpbakery"

	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/daemon"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/device"
	"github.com/lxc/lxd/lxd/endpoints"
	"github.com/lxc/lxd/lxd/events"
	"github.com/lxc/lxd/lxd/firewall"
	"github.com/lxc/lxd/lxd/instance"
	instanceDrivers "github.com/lxc/lxd/lxd/instance/drivers"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/maas"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/rbac"
	"github.com/lxc/lxd/lxd/request"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/lxd/seccomp"
	"github.com/lxc/lxd/lxd/state"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/lxd/sys"
	"github.com/lxc/lxd/lxd/task"
	"github.com/lxc/lxd/lxd/ucred"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/lxd/warnings"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/idmap"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/version"

	log "github.com/lxc/lxd/shared/log15"
)

// A Daemon can respond to requests from a shared client.
type Daemon struct {
	clientCerts  *certificateCache
	os           *sys.OS
	db           *db.Node
	firewall     firewall.Firewall
	maas         *maas.Controller
	rbac         *rbac.Server
	cluster      *db.Cluster
	setupChan    chan struct{} // Closed when basic Daemon setup is completed
	readyChan    chan struct{} // Closed when LXD is fully ready
	shutdownChan chan struct{}

	// Event servers
	devlxdEvents *events.Server
	events       *events.Server

	// Tasks registry for long-running background tasks
	// Keep clustering tasks separate as they cause a lot of CPU wakeups
	tasks        task.Group
	clusterTasks task.Group

	// Indexes of tasks that need to be reset when their execution interval changes
	taskPruneImages      *task.Task
	taskClusterHeartbeat *task.Task

	// Stores startup time of daemon
	startTime time.Time

	config    *DaemonConfig
	endpoints *endpoints.Endpoints
	gateway   *cluster.Gateway
	seccomp   *seccomp.Server

	proxy func(req *http.Request) (*url.URL, error)

	externalAuth *externalAuth

	// Stores last heartbeat node information to detect node changes.
	lastNodeList *cluster.APIHeartbeat

	// Serialize changes to cluster membership (joins, leaves, role
	// changes).
	clusterMembershipMutex   sync.RWMutex
	clusterMembershipClosing bool // Prevent further rebalances

	serverCert    func() *shared.CertInfo
	serverCertInt *shared.CertInfo // Do not use this directly, use servertCert func.

	ctx    context.Context
	cancel context.CancelFunc
}

type externalAuth struct {
	endpoint string
	expiry   int64
	bakery   *identchecker.Bakery
}

// DaemonConfig holds configuration values for Daemon.
type DaemonConfig struct {
	Group              string        // Group name the local unix socket should be chown'ed to
	Trace              []string      // List of sub-systems to trace
	RaftLatency        float64       // Coarse grain measure of the cluster latency
	DqliteSetupTimeout time.Duration // How long to wait for the cluster database to be up
}

// IdentityClientWrapper is a wrapper around an IdentityClient.
type IdentityClientWrapper struct {
	client       identchecker.IdentityClient
	ValidDomains []string
}

func (m *IdentityClientWrapper) IdentityFromContext(ctx context.Context) (identchecker.Identity, []checkers.Caveat, error) {
	return m.client.IdentityFromContext(ctx)
}

func (m *IdentityClientWrapper) DeclaredIdentity(ctx context.Context, declared map[string]string) (identchecker.Identity, error) {
	// Extract the domain from the username
	fields := strings.SplitN(declared["username"], "@", 2)

	// Only validate domain if we have a list of valid domains
	if len(m.ValidDomains) > 0 {
		// If no domain was provided by candid, reject the request
		if len(fields) < 2 {
			logger.Warnf("Failed candid client authentication: no domain provided")
			return nil, fmt.Errorf("Missing domain in candid reply")
		}

		// Check that it was a valid domain
		if !shared.StringInSlice(fields[1], m.ValidDomains) {
			logger.Warnf("Failed candid client authentication: untrusted domain \"%s\"", fields[1])
			return nil, fmt.Errorf("Untrusted candid domain")
		}
	}

	return m.client.DeclaredIdentity(ctx, declared)
}

// newDaemon returns a new Daemon object with the given configuration.
func newDaemon(config *DaemonConfig, os *sys.OS) *Daemon {
	lxdEvents := events.NewServer(daemon.Debug, daemon.Verbose)
	devlxdEvents := events.NewServer(daemon.Debug, daemon.Verbose)
	ctx, cancel := context.WithCancel(context.Background())

	d := &Daemon{
		clientCerts:  &certificateCache{},
		config:       config,
		devlxdEvents: devlxdEvents,
		events:       lxdEvents,
		os:           os,
		setupChan:    make(chan struct{}),
		readyChan:    make(chan struct{}),
		shutdownChan: make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}

	d.serverCert = func() *shared.CertInfo { return d.serverCertInt }

	return d
}

// defaultDaemonConfig returns a DaemonConfig object with default values.
func defaultDaemonConfig() *DaemonConfig {
	return &DaemonConfig{
		RaftLatency:        3.0,
		DqliteSetupTimeout: 36 * time.Hour, // Account for snap refresh lag
	}
}

// defaultDaemon returns a new, un-initialized Daemon object with default values.
func defaultDaemon() *Daemon {
	config := defaultDaemonConfig()
	os := sys.DefaultOS()
	return newDaemon(config, os)
}

// APIEndpoint represents a URL in our API.
type APIEndpoint struct {
	Name    string             // Name for this endpoint.
	Path    string             // Path pattern for this endpoint.
	Aliases []APIEndpointAlias // Any aliases for this endpoint.
	Get     APIEndpointAction
	Put     APIEndpointAction
	Post    APIEndpointAction
	Delete  APIEndpointAction
	Patch   APIEndpointAction
}

// APIEndpointAlias represents an alias URL of and APIEndpoint in our API.
type APIEndpointAlias struct {
	Name string // Name for this alias.
	Path string // Path pattern for this alias.
}

// APIEndpointAction represents an action on an API endpoint.
type APIEndpointAction struct {
	Handler        func(d *Daemon, r *http.Request) response.Response
	AccessHandler  func(d *Daemon, r *http.Request) response.Response
	AllowUntrusted bool
}

// allowAuthenticated is an AccessHandler which allows all requests.
// This function doesn't do anything itself, except return the EmptySyncResponse that allows the request to
// proceed. However in order to access any API route you must be authenticated, unless the handler's AllowUntrusted
// property is set to true or you are an admin.
func allowAuthenticated(d *Daemon, r *http.Request) response.Response {
	return response.EmptySyncResponse
}

// allowProjectPermission is a wrapper to check access against the project, its features and RBAC permission
func allowProjectPermission(feature string, permission string) func(d *Daemon, r *http.Request) response.Response {
	return func(d *Daemon, r *http.Request) response.Response {
		// Shortcut for speed
		if rbac.UserIsAdmin(r) {
			return response.EmptySyncResponse
		}

		// Get the project
		projectName := projectParam(r)

		// Validate whether the user has the needed permission
		if !rbac.UserHasPermission(r, projectName, permission) {
			return response.Forbidden(nil)
		}

		return response.EmptySyncResponse
	}
}

// Convenience function around Authenticate
func (d *Daemon) checkTrustedClient(r *http.Request) error {
	trusted, _, _, err := d.Authenticate(nil, r)
	if !trusted || err != nil {
		if err != nil {
			return err
		}

		return fmt.Errorf("Not authorized")
	}

	return nil
}

// getTrustedCertificates returns trusted certificates key on DB type and fingerprint.
func (d *Daemon) getTrustedCertificates() map[db.CertificateType]map[string]x509.Certificate {
	d.clientCerts.Lock.Lock()
	defer d.clientCerts.Lock.Unlock()

	return d.clientCerts.Certificates
}

// Authenticate validates an incoming http Request
// It will check over what protocol it came, what type of request it is and
// will validate the TLS certificate or Macaroon.
//
// This does not perform authorization, only validates authentication.
func (d *Daemon) Authenticate(w http.ResponseWriter, r *http.Request) (bool, string, string, error) {
	trustedCerts := d.getTrustedCertificates()

	// Allow internal cluster traffic by checking against the trusted certfificates.
	if r.TLS != nil {
		for _, i := range r.TLS.PeerCertificates {
			trusted, _ := util.CheckTrustState(*i, trustedCerts[db.CertificateTypeServer], d.endpoints.NetworkCert(), false)
			if trusted {
				return true, "", "cluster", nil
			}
		}
	}

	// Local unix socket queries.
	if r.RemoteAddr == "@" {
		if w != nil {
			conn := extractUnderlyingConn(w)
			cred, err := ucred.GetCred(conn)
			if err != nil {
				return false, "", "", err
			}

			u, err := user.LookupId(fmt.Sprintf("%d", cred.Uid))
			if err != nil {
				return true, fmt.Sprintf("uid=%d", cred.Uid), "unix", nil
			}

			return true, u.Username, "unix", nil
		}

		return true, "", "unix", nil
	}

	// Devlxd unix socket credentials on main API.
	if r.RemoteAddr == "@devlxd" {
		return false, "", "", fmt.Errorf("Main API query can't come from /dev/lxd socket")
	}

	// Cluster notification with wrong certificate.
	if isClusterNotification(r) {
		return false, "", "", fmt.Errorf("Cluster notification isn't using trusted server certificate")
	}

	// Bad query, no TLS found.
	if r.TLS == nil {
		return false, "", "", fmt.Errorf("Bad/missing TLS on network query")
	}

	if d.externalAuth != nil && r.Header.Get(httpbakery.BakeryProtocolHeader) != "" {
		// Validate external authentication.
		ctx := httpbakery.ContextWithRequest(context.TODO(), r)
		authChecker := d.externalAuth.bakery.Checker.Auth(httpbakery.RequestMacaroons(r)...)

		ops := []bakery.Op{{
			Entity: r.URL.Path,
			Action: r.Method,
		}}

		info, err := authChecker.Allow(ctx, ops...)
		if err != nil {
			// Bad macaroon.
			return false, "", "", err
		}

		if info != nil && info.Identity != nil {
			// Valid identity macaroon found.
			return true, info.Identity.Id(), "candid", nil
		}

		// Valid macaroon with no identity information.
		return true, "", "candid", nil
	}

	// Validate normal TLS access.
	trustCACertificates, err := cluster.ConfigGetBool(d.cluster, "core.trust_ca_certificates")
	if err != nil {
		return false, "", "", err
	}

	for _, i := range r.TLS.PeerCertificates {
		trusted, username := util.CheckTrustState(*i, trustedCerts[db.CertificateTypeClient], d.endpoints.NetworkCert(), trustCACertificates)
		if trusted {
			return true, username, "tls", nil
		}
	}

	// Reject unauthorized.
	return false, "", "", nil
}

func writeMacaroonsRequiredResponse(b *identchecker.Bakery, r *http.Request, w http.ResponseWriter, derr *bakery.DischargeRequiredError, expiry int64) {
	ctx := httpbakery.ContextWithRequest(context.TODO(), r)
	caveats := append(derr.Caveats,
		checkers.TimeBeforeCaveat(time.Now().Add(time.Duration(expiry)*time.Second)))

	// Mint an appropriate macaroon and send it back to the client.
	m, err := b.Oven.NewMacaroon(
		ctx, httpbakery.RequestVersion(r), caveats, derr.Ops...)
	if err != nil {
		resp := response.ErrorResponse(http.StatusInternalServerError, err.Error())
		resp.Render(w)
		return
	}

	herr := httpbakery.NewDischargeRequiredError(
		httpbakery.DischargeRequiredErrorParams{
			Macaroon:      m,
			OriginalError: derr,
			Request:       r,
		})
	herr.(*httpbakery.Error).Info.CookieNameSuffix = "auth"
	httpbakery.WriteError(ctx, w, herr)
	return
}

// State creates a new State instance linked to our internal db and os.
func (d *Daemon) State() *state.State {
	// If the daemon is shutting down, the context will be cancelled.
	// This information will be available throughout the code, and can be used to prevent new
	// operations from starting during shutdown.

	// Build a list of supported instance types.
	supportedInstanceTypesInfo := instanceDrivers.SupportedInstanceTypes()
	supportedInstanceTypes := make(map[instancetype.Type]struct{}, len(supportedInstanceTypesInfo))
	for instanceType := range supportedInstanceTypesInfo {
		supportedInstanceTypes[instanceType] = struct{}{}
	}

	return &state.State{
		Context:                d.ctx,
		Node:                   d.db,
		Cluster:                d.cluster,
		MAAS:                   d.maas,
		OS:                     d.os,
		Endpoints:              d.endpoints,
		Events:                 d.events,
		DevlxdEvents:           d.devlxdEvents,
		Firewall:               d.firewall,
		Proxy:                  d.proxy,
		ServerCert:             d.serverCert,
		UpdateCertificateCache: func() { updateCertificateCache(d) },
		InstanceTypes:          supportedInstanceTypes,
	}
}

// UnixSocket returns the full path to the unix.socket file that this daemon is
// listening on. Used by tests.
func (d *Daemon) UnixSocket() string {
	path := os.Getenv("LXD_SOCKET")
	if path != "" {
		return path
	}

	return filepath.Join(d.os.VarDir, "unix.socket")
}

func (d *Daemon) createCmd(restAPI *mux.Router, version string, c APIEndpoint) {
	var uri string
	if c.Path == "" {
		uri = fmt.Sprintf("/%s", version)
	} else {
		uri = fmt.Sprintf("/%s/%s", version, c.Path)
	}

	route := restAPI.HandleFunc(uri, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if !(r.RemoteAddr == "@" && version == "internal") {
			// Block public API requests until we're done with basic
			// initialization tasks, such setting up the cluster database.
			select {
			case <-d.setupChan:
			default:
				response := response.Unavailable(fmt.Errorf("LXD daemon setup in progress"))
				response.Render(w)
				return
			}
		}

		// Authentication
		trusted, username, protocol, err := d.Authenticate(w, r)
		if err != nil {
			// If not a macaroon discharge request, return the error
			_, ok := err.(*bakery.DischargeRequiredError)
			if !ok {
				response.InternalError(err).Render(w)
				return
			}
		}

		// Reject internal queries to remote, non-cluster, clients
		if version == "internal" && !shared.StringInSlice(protocol, []string{"unix", "cluster"}) {
			// Except for the initial cluster accept request (done over trusted TLS)
			if !trusted || c.Path != "cluster/accept" || protocol != "tls" {
				logger.Warn("Rejecting remote internal API request", log.Ctx{"ip": r.RemoteAddr})
				response.Forbidden(nil).Render(w)
				return
			}
		}

		untrustedOk := (r.Method == "GET" && c.Get.AllowUntrusted) || (r.Method == "POST" && c.Post.AllowUntrusted)
		if trusted {
			logCtx := log.Ctx{"method": r.Method, "url": r.URL.RequestURI(), "ip": r.RemoteAddr, "username": username, "protocol": protocol}
			logger.Debug("Handling", logCtx)

			// Get user access data.
			userAccess, err := func() (*rbac.UserAccess, error) {
				ua := &rbac.UserAccess{}
				ua.Admin = true

				// Internal cluster communications.
				if protocol == "cluster" {
					return ua, nil
				}

				// Regular TLS clients.
				if protocol == "tls" {
					d.clientCerts.Lock.Lock()
					certProjects := d.clientCerts.Projects
					d.clientCerts.Lock.Unlock()

					// Check if we have restrictions on the key.
					if certProjects != nil {
						projects, ok := certProjects[username]
						if ok {
							ua.Admin = false
							ua.Projects = map[string][]string{}
							for _, projectName := range projects {
								ua.Projects[projectName] = []string{
									"view",
									"manage-containers",
									"manage-images",
									"manage-networks",
									"manage-profiles",
									"manage-storage-volumes",
									"operate-containers",
								}
							}
						}
					}

					return ua, nil
				}

				// If no external authentication configured, we're done now.
				if d.externalAuth == nil || d.rbac == nil || r.RemoteAddr == "@" {
					return ua, nil
				}

				// Validate RBAC permissions.
				ua, err = d.rbac.UserAccess(username)
				if err != nil {
					return nil, err
				}

				return ua, nil
			}()
			if err != nil {
				logCtx["err"] = err
				logger.Warn("Rejecting remote API request", logCtx)
				response.Forbidden(nil).Render(w)
				return
			}

			// Add authentication/authorization context data.
			ctx := context.WithValue(r.Context(), request.CtxUsername, username)
			ctx = context.WithValue(ctx, request.CtxProtocol, protocol)
			ctx = context.WithValue(ctx, request.CtxAccess, userAccess)

			// Add forwarded requestor data.
			if protocol == "cluster" {
				// Add authentication/authorization context data.
				ctx = context.WithValue(ctx, request.CtxForwardedAddress, r.Header.Get(request.HeaderForwardedAddress))
				ctx = context.WithValue(ctx, request.CtxForwardedUsername, r.Header.Get(request.HeaderForwardedUsername))
				ctx = context.WithValue(ctx, request.CtxForwardedProtocol, r.Header.Get(request.HeaderForwardedProtocol))
			}

			r = r.WithContext(ctx)
		} else if untrustedOk && r.Header.Get("X-LXD-authenticated") == "" {
			logger.Debug(fmt.Sprintf("Allowing untrusted %s", r.Method), log.Ctx{"url": r.URL.RequestURI(), "ip": r.RemoteAddr})
		} else if derr, ok := err.(*bakery.DischargeRequiredError); ok {
			writeMacaroonsRequiredResponse(d.externalAuth.bakery, r, w, derr, d.externalAuth.expiry)
			return
		} else {
			logger.Warn("Rejecting request from untrusted client", log.Ctx{"ip": r.RemoteAddr})
			response.Forbidden(nil).Render(w)
			return
		}

		// Dump full request JSON when in debug mode
		if daemon.Debug && r.Method != "GET" && util.IsJSONRequest(r) {
			newBody := &bytes.Buffer{}
			captured := &bytes.Buffer{}
			multiW := io.MultiWriter(newBody, captured)
			if _, err := io.Copy(multiW, r.Body); err != nil {
				response.InternalError(err).Render(w)
				return
			}

			r.Body = shared.BytesReadCloser{Buf: newBody}
			shared.DebugJson(captured)
		}

		// Actually process the request
		var resp response.Response
		resp = response.NotImplemented(nil)

		// Return Unavailable Error (503) if daemon is shutting down.
		// There are some exceptions:
		// - internal calls, e.g. lxd shutdown
		// - events endpoint as this is accessed when running `lxd shutdown`
		// - /1.0 endpoint
		// - /1.0/operations endpoints
		// - GET queries
		allowedDuringShutdown := func() bool {
			if version == "internal" {
				return true
			}

			if c.Path == "" || c.Path == "events" || c.Path == "operations" || strings.HasPrefix(c.Path, "operations/") {
				return true
			}

			if r.Method == "GET" {
				return true
			}

			return false
		}

		if d.ctx.Err() == context.Canceled && !allowedDuringShutdown() {
			response.Unavailable(fmt.Errorf("LXD is shutting down")).Render(w)
			return
		}

		handleRequest := func(action APIEndpointAction) response.Response {
			if action.Handler == nil {
				return response.NotImplemented(nil)
			}

			if action.AccessHandler != nil {
				// Defer access control to custom handler
				resp := action.AccessHandler(d, r)
				if resp != response.EmptySyncResponse {
					return resp
				}
			} else if !action.AllowUntrusted {
				// Require admin privileges
				if !rbac.UserIsAdmin(r) {
					return response.Forbidden(nil)
				}
			}

			return action.Handler(d, r)
		}

		switch r.Method {
		case "GET":
			resp = handleRequest(c.Get)
		case "PUT":
			resp = handleRequest(c.Put)
		case "POST":
			resp = handleRequest(c.Post)
		case "DELETE":
			resp = handleRequest(c.Delete)
		case "PATCH":
			resp = handleRequest(c.Patch)
		default:
			resp = response.NotFound(fmt.Errorf("Method '%s' not found", r.Method))
		}

		// Handle errors
		if err := resp.Render(w); err != nil {
			err := response.InternalError(err).Render(w)
			if err != nil {
				logger.Errorf("Failed writing error for error, giving up")
			}
		}
	})

	// If the endpoint has a canonical name then record it so it can be used to build URLS
	// and accessed in the context of the request by the handler function.
	if c.Name != "" {
		route.Name(c.Name)
	}
}

// have we setup shared mounts?
var sharedMountsLock sync.Mutex

// setupSharedMounts will mount any shared mounts needed, and set daemon.SharedMountsSetup to true.
func setupSharedMounts() error {
	// Check if we already went through this
	if daemon.SharedMountsSetup {
		return nil
	}

	// Get a lock to prevent races
	sharedMountsLock.Lock()
	defer sharedMountsLock.Unlock()

	// Check if already setup
	path := shared.VarPath("shmounts")
	if shared.IsMountPoint(path) {
		daemon.SharedMountsSetup = true
		return nil
	}

	// Mount a new tmpfs
	if err := unix.Mount("tmpfs", path, "tmpfs", 0, "size=100k,mode=0711"); err != nil {
		return err
	}

	// Mark as MS_SHARED and MS_REC
	var flags uintptr = unix.MS_SHARED | unix.MS_REC
	if err := unix.Mount(path, path, "none", flags, ""); err != nil {
		return err
	}

	daemon.SharedMountsSetup = true
	return nil
}

func (d *Daemon) Init() error {
	err := d.init()

	// If an error occurred synchronously while starting up, let's try to
	// cleanup any state we produced so far. Errors happening here will be
	// ignored.
	if err != nil {
		logger.Errorf("Failed to start the daemon: %v", err)
		d.Stop()
	}

	return err
}

func (d *Daemon) init() error {
	var dbWarnings []db.Warning

	// Setup logger
	events.LoggingServer = d.events

	// Lets check if there's an existing LXD running
	err := endpoints.CheckAlreadyRunning(d.UnixSocket())
	if err != nil {
		return err
	}

	/* Set the LVM environment */
	err = os.Setenv("LVM_SUPPRESS_FD_WARNINGS", "1")
	if err != nil {
		return err
	}

	/* Print welcome message */
	if d.os.MockMode {
		logger.Info(fmt.Sprintf("LXD %s is starting in mock mode", version.Version),
			log.Ctx{"path": shared.VarPath("")})
	} else {
		logger.Info(fmt.Sprintf("LXD %s is starting in normal mode", version.Version),
			log.Ctx{"path": shared.VarPath("")})
	}

	/* List of sub-systems to trace */
	trace := d.config.Trace

	/* Initialize the operating system facade */
	dbWarnings, err = d.os.Init()
	if err != nil {
		return err
	}

	// Bump some kernel limits to avoid issues
	for _, limit := range []int{unix.RLIMIT_NOFILE} {
		rLimit := unix.Rlimit{}
		err := unix.Getrlimit(limit, &rLimit)
		if err != nil {
			return err
		}

		rLimit.Cur = rLimit.Max

		err = unix.Setrlimit(limit, &rLimit)
		if err != nil {
			return err
		}
	}

	// Detect LXC features
	d.os.LXCFeatures = map[string]bool{}
	lxcExtensions := []string{
		"mount_injection_file",
		"seccomp_notify",
		"network_ipvlan",
		"network_l2proxy",
		"network_gateway_device_route",
		"network_phys_macvlan_mtu",
		"network_veth_router",
		"cgroup2",
		"pidfd",
		"seccomp_allow_deny_syntax",
		"devpts_fd",
		"seccomp_proxy_send_notify_fd",
		"idmapped_mounts_v2",
	}
	for _, extension := range lxcExtensions {
		d.os.LXCFeatures[extension] = liblxc.HasApiExtension(extension)
	}

	// Look for kernel features
	logger.Infof("Kernel features:")
	d.os.CloseRange = canUseCloseRange()
	if d.os.CloseRange {
		logger.Infof(" - closing multiple file descriptors efficiently: yes")
	} else {
		logger.Infof(" - closing multiple file descriptors efficiently: no")
	}

	d.os.NetnsGetifaddrs = canUseNetnsGetifaddrs()
	if d.os.NetnsGetifaddrs {
		logger.Infof(" - netnsid-based network retrieval: yes")
	} else {
		logger.Infof(" - netnsid-based network retrieval: no")
	}

	if canUsePidFds() && d.os.LXCFeatures["pidfd"] {
		d.os.PidFds = true
	}
	if d.os.PidFds {
		logger.Infof(" - pidfds: yes")
	} else {
		logger.Infof(" - pidfds: no")
	}

	d.os.UeventInjection = canUseUeventInjection()
	if d.os.UeventInjection {
		logger.Infof(" - uevent injection: yes")
	} else {
		logger.Infof(" - uevent injection: no")
	}

	d.os.SeccompListener = canUseSeccompListener()
	if d.os.SeccompListener {
		logger.Infof(" - seccomp listener: yes")
	} else {
		logger.Infof(" - seccomp listener: no")
	}

	d.os.SeccompListenerContinue = canUseSeccompListenerContinue()
	if d.os.SeccompListenerContinue {
		logger.Infof(" - seccomp listener continue syscalls: yes")
	} else {
		logger.Infof(" - seccomp listener continue syscalls: no")
	}

	if canUseSeccompListenerAddfd() && d.os.LXCFeatures["seccomp_proxy_send_notify_fd"] {
		d.os.SeccompListenerAddfd = true
		logger.Infof(" - seccomp listener add file descriptors: yes")
	} else {
		logger.Infof(" - seccomp listener add file descriptors: no")
	}

	d.os.PidFdSetns = canUsePidFdSetns()
	if d.os.PidFdSetns {
		logger.Infof(" - attach to namespaces via pidfds: yes")
	} else {
		logger.Infof(" - attach to namespaces via pidfds: no")
	}

	if d.os.LXCFeatures["devpts_fd"] && canUseNativeTerminals() {
		d.os.NativeTerminals = true
		logger.Infof(" - safe native terminal allocation : yes")
	} else {
		logger.Infof(" - safe native terminal allocation : no")
	}

	/*
	 * During daemon startup we're the only thread that touches VFS3Fscaps
	 * so we don't need to bother with atomic.StoreInt32() when touching
	 * VFS3Fscaps.
	 */
	d.os.VFS3Fscaps = idmap.SupportsVFS3Fscaps("")
	if d.os.VFS3Fscaps {
		idmap.VFS3Fscaps = idmap.VFS3FscapsSupported
		logger.Infof(" - unprivileged file capabilities: yes")
	} else {
		idmap.VFS3Fscaps = idmap.VFS3FscapsUnsupported
		logger.Infof(" - unprivileged file capabilities: no")
	}

	dbWarnings = append(dbWarnings, d.os.CGInfo.Warnings()...)

	logger.Infof(" - cgroup layout: %s", d.os.CGInfo.Mode())

	for _, w := range dbWarnings {
		logger.Warnf(" - %s, %s", db.WarningTypeNames[db.WarningType(w.TypeCode)], w.LastMessage)
	}

	// Detect shiftfs support.
	if shared.IsTrue(os.Getenv("LXD_SHIFTFS_DISABLE")) {
		logger.Infof(" - shiftfs support: disabled")
	} else {
		if canUseShiftfs() && (util.HasFilesystem("shiftfs") || util.LoadModule("shiftfs") == nil) {
			d.os.Shiftfs = true
			logger.Infof(" - shiftfs support: yes")
		} else {
			logger.Infof(" - shiftfs support: no")
		}
	}

	// Detect and cached available instance types from operational drivers.
	instanceDrivers.SupportedInstanceTypes()

	// Validate the devices storage.
	testDev := shared.VarPath("devices", ".test")
	testDevNum := int(unix.Mkdev(0, 0))
	os.Remove(testDev)
	err = unix.Mknod(testDev, 0600|unix.S_IFCHR, testDevNum)
	if err == nil {
		fd, err := os.Open(testDev)
		if err != nil && os.IsPermission(err) {
			logger.Warnf("Unable to access device nodes, LXD likely running on a nodev mount")
			d.os.Nodev = true
		}
		fd.Close()
		os.Remove(testDev)
	}

	/* Initialize the database */
	dump, err := initializeDbObject(d)
	if err != nil {
		return err
	}

	/* Setup network endpoint certificate */
	networkCert, err := util.LoadCert(d.os.VarDir)
	if err != nil {
		return err
	}

	/* Setup server certificate */
	serverCert, err := util.LoadServerCert(d.os.VarDir)
	if err != nil {
		return err
	}

	// Load cached local trusted certificates before starting listener and cluster database.
	err = updateCertificateCacheFromLocal(d)
	if err != nil {
		return err
	}

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return errors.Wrapf(err, "Failed checking if clustered")
	}

	// Detect if clustered, but not yet upgraded to per-server client certificates.
	if clustered && len(d.clientCerts.Certificates[db.CertificateTypeServer]) < 1 {
		// If the cluster has not yet upgraded to per-server client certificates (by running patch
		// patchClusteringServerCertTrust) then temporarily use the network (cluster) certificate as client
		// certificate, and cause us to trust it for use as client certificate from the other members.
		logger.Warnf("No local trusted server certificates found, falling back to trusting network certificate")
		logger.Infof("Set client certificate to network certificate %v", networkCert.Fingerprint())
		d.serverCertInt = networkCert

	} else {
		// If standalone or the local trusted certificates table is populated with server certificates then
		// use our local server certificate as client certificate for intra-cluster communication.
		logger.Infof("Set client certificate to server certificate %v", serverCert.Fingerprint())
		d.serverCertInt = serverCert
	}

	/* Setup dqlite */
	clusterLogLevel := "ERROR"
	if shared.StringInSlice("dqlite", trace) {
		clusterLogLevel = "TRACE"
	}
	d.gateway, err = cluster.NewGateway(
		d.db,
		networkCert,
		d.serverCert,
		cluster.Latency(d.config.RaftLatency),
		cluster.LogLevel(clusterLogLevel))
	if err != nil {
		return err
	}
	d.gateway.HeartbeatNodeHook = d.NodeRefreshTask

	/* Setup some mounts (nice to have) */
	if !d.os.MockMode {
		// Attempt to mount the shmounts tmpfs
		err := setupSharedMounts()
		if err != nil {
			logger.Warnf("Failed settting up shared mounts: %v", err)
		}

		// Attempt to Mount the devlxd tmpfs
		devlxd := filepath.Join(d.os.VarDir, "devlxd")
		if !shared.IsMountPoint(devlxd) {
			unix.Mount("tmpfs", devlxd, "tmpfs", 0, "size=100k,mode=0755")
		}
	}

	address, err := node.HTTPSAddress(d.db)
	if err != nil {
		return errors.Wrap(err, "Failed to fetch node address")
	}

	clusterAddress, err := node.ClusterAddress(d.db)
	if err != nil {
		return errors.Wrap(err, "Failed to fetch cluster address")
	}

	debugAddress, err := node.DebugAddress(d.db)
	if err != nil {
		return errors.Wrap(err, "Failed to fetch debug address")
	}

	/* Setup the web server */
	config := &endpoints.Config{
		Dir:                  d.os.VarDir,
		UnixSocket:           d.UnixSocket(),
		Cert:                 networkCert,
		RestServer:           restServer(d),
		DevLxdServer:         devLxdServer(d),
		LocalUnixSocketGroup: d.config.Group,
		NetworkAddress:       address,
		ClusterAddress:       clusterAddress,
		DebugAddress:         debugAddress,
	}
	d.endpoints, err = endpoints.Up(config)
	if err != nil {
		return err
	}

	// Have the db package determine remote storage drivers
	db.StorageRemoteDriverNames = storageDrivers.RemoteDriverNames

	/* Open the cluster database */
	for {
		logger.Info("Initializing global database")
		dir := filepath.Join(d.os.VarDir, "database")

		store := d.gateway.NodeStore()

		contextTimeout := 30 * time.Second
		if !clustered {
			// FIXME: this is a workaround for #5234. We set a very
			// high timeout when we're not clustered, since there's
			// actually no networking involved.
			contextTimeout = time.Minute
		}

		options := []driver.Option{
			driver.WithDialFunc(d.gateway.DialFunc()),
			driver.WithContext(d.gateway.Context()),
			driver.WithConnectionTimeout(10 * time.Second),
			driver.WithContextTimeout(contextTimeout),
			driver.WithLogFunc(cluster.DqliteLog),
		}

		if shared.StringInSlice("database", trace) {
			options = append(options, driver.WithTracing(dqliteclient.LogDebug))
		}

		d.cluster, err = db.OpenCluster(
			"db.bin", store, clusterAddress, dir,
			d.config.DqliteSetupTimeout, dump, options...,
		)
		if err == nil {
			break
		}
		// If some other nodes have schema or API versions less recent
		// than this node, we block until we receive a notification
		// from the last node being upgraded that everything should be
		// now fine, and then retry
		if err == db.ErrSomeNodesAreBehind {
			logger.Info("Wait for other cluster nodes to upgrade their versions")

			// The only thing we want to still do on this node is
			// to run the heartbeat task, in case we are the raft
			// leader.
			d.gateway.Cluster = d.cluster
			taskFunc, taskSchedule := cluster.HeartbeatTask(d.gateway)
			hbGroup := task.Group{}
			d.taskClusterHeartbeat = hbGroup.Add(taskFunc, taskSchedule)
			hbGroup.Start(d.ctx)
			d.gateway.WaitUpgradeNotification()
			hbGroup.Stop(time.Second)
			d.gateway.Cluster = nil

			d.cluster.Close()

			continue
		}
		return errors.Wrap(err, "failed to open cluster database")
	}

	d.firewall = firewall.New()
	logger.Infof("Firewall loaded driver %q", d.firewall)

	err = cluster.NotifyUpgradeCompleted(d.State(), networkCert, d.serverCert())
	if err != nil {
		// Ignore the error, since it's not fatal for this particular
		// node. In most cases it just means that some nodes are
		// offline.
		logger.Debugf("Could not notify all nodes of database upgrade: %v", err)
	}
	d.gateway.Cluster = d.cluster

	// This logic used to belong to patchUpdateFromV10, but has been moved
	// here because it needs database access.
	if shared.PathExists(shared.VarPath("lxc")) {
		err := os.Rename(shared.VarPath("lxc"), shared.VarPath("containers"))
		if err != nil {
			return err
		}

		logger.Debugf("Restarting all the containers following directory rename")
		s := d.State()
		instancesShutdown(s)
		instancesRestart(s)
	}

	// Setup the user-agent.
	if clustered {
		version.UserAgentFeatures([]string{"cluster"})
	}

	// Mount the storage pools.
	logger.Infof("Initializing storage pools")
	err = setupStorageDriver(d.State(), false)
	if err != nil {
		return err
	}

	// Apply all patches that need to be run before daemon storage is initialised.
	err = patchesApply(d, patchPreDaemonStorage)
	if err != nil {
		return err
	}

	// Mount any daemon storage volumes.
	logger.Infof("Initializing daemon storage mounts")
	err = daemonStorageMount(d.State())
	if err != nil {
		return err
	}

	// Create directories on daemon storage mounts.
	err = d.os.InitStorage()
	if err != nil {
		return err
	}

	// Apply all patches that need to be run after daemon storage is initialised.
	err = patchesApply(d, patchPostDaemonStorage)
	if err != nil {
		return err
	}

	// Setup the networks.
	logger.Infof("Initializing networks")
	err = networkStartup(d.State())
	if err != nil {
		return err
	}

	// Cleanup leftover images.
	pruneLeftoverImages(d)

	// Setup the proxy handler, external authentication and MAAS.
	candidAPIURL := ""
	candidAPIKey := ""
	candidDomains := ""
	candidExpiry := int64(0)

	rbacAPIURL := ""
	rbacAPIKey := ""
	rbacAgentURL := ""
	rbacAgentUsername := ""
	rbacAgentPrivateKey := ""
	rbacAgentPublicKey := ""
	rbacExpiry := int64(0)

	maasAPIURL := ""
	maasAPIKey := ""
	maasMachine := ""

	err = d.db.Transaction(func(tx *db.NodeTx) error {
		config, err := node.ConfigLoad(tx)
		if err != nil {
			return err
		}

		maasMachine = config.MAASMachine()
		return nil
	})
	if err != nil {
		return err
	}

	logger.Infof("Loading daemon configuration")
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		config, err := cluster.ConfigLoad(tx)
		if err != nil {
			return err
		}

		d.proxy = shared.ProxyFromConfig(
			config.ProxyHTTPS(), config.ProxyHTTP(), config.ProxyIgnoreHosts(),
		)

		candidAPIURL, candidAPIKey, candidExpiry, candidDomains = config.CandidServer()
		maasAPIURL, maasAPIKey = config.MAASController()
		rbacAPIURL, rbacAPIKey, rbacExpiry, rbacAgentURL, rbacAgentUsername, rbacAgentPrivateKey, rbacAgentPublicKey = config.RBACServer()
		d.gateway.HeartbeatOfflineThreshold = config.OfflineThreshold()

		d.endpoints.NetworkUpdateTrustedProxy(config.HTTPSTrustedProxy())

		return nil
	})
	if err != nil {
		return err
	}

	if rbacAPIURL != "" {
		err = d.setupRBACServer(rbacAPIURL, rbacAPIKey, rbacExpiry, rbacAgentURL, rbacAgentUsername, rbacAgentPrivateKey, rbacAgentPublicKey)
		if err != nil {
			return err
		}
	}

	if candidAPIURL != "" {
		err = d.setupExternalAuthentication(candidAPIURL, candidAPIKey, candidExpiry, candidDomains)
		if err != nil {
			return err
		}
	}

	if !d.os.MockMode {
		// Start the scheduler
		go deviceEventListener(d.State())

		// Setup inotify watches
		_, err := device.InotifyInit(d.State())
		if err != nil {
			return err
		}

		go device.InotifyHandler(d.State())

		// Register devices on running instances to receive events and reconnect to VM monitor sockets.
		// This should come after the event handler go routines have been started.
		devicesRegister(d.State())

		// Setup seccomp handler
		if d.os.SeccompListener {
			seccompServer, err := seccomp.NewSeccompServer(d.State(), shared.VarPath("seccomp.socket"), func(pid int32, state *state.State) (seccomp.Instance, error) {
				return findContainerForPid(pid, state)
			})
			if err != nil {
				return err
			}
			d.seccomp = seccompServer
			logger.Info("Started seccomp handler", log.Ctx{"path": shared.VarPath("seccomp.socket")})
		}

		// Read the trusted certificates
		updateCertificateCache(d)

		// Connect to MAAS
		if maasAPIURL != "" {
			go func() {
				warningAdded := false

				for {
					err = d.setupMAASController(maasAPIURL, maasAPIKey, maasMachine)
					if err == nil {
						logger.Info("Connected to MAAS controller", log.Ctx{"url": maasAPIURL})
						break
					}

					logger.Warn("Unable to connect to MAAS, trying again in a minute", log.Ctx{"url": maasAPIURL, "err": err})

					if !warningAdded {
						d.cluster.UpsertWarningLocalNode("", -1, -1, db.WarningUnableToConnectToMAAS, err.Error())

						warningAdded = true
					}

					time.Sleep(time.Minute)
				}

				// Resolve any previously created warning once connected
				if warningAdded {
					warnings.ResolveWarningsByLocalNodeAndType(d.cluster, db.WarningUnableToConnectToMAAS)
				}
			}()
		}
	}

	close(d.setupChan)

	// Create warnings that have been collected
	for _, w := range dbWarnings {
		err := d.cluster.UpsertWarningLocalNode("", -1, -1, db.WarningType(w.TypeCode), w.LastMessage)
		if err != nil {
			logger.Warn("Failed to create warning", log.Ctx{"err": err})
		}
	}

	// Resolve warnings; exclude those just created
	for i := range db.WarningTypeNames {
		resolveWarning := true

		for _, w := range dbWarnings {
			if i == w.TypeCode {
				// Do not resolve the warning as it's still valid
				resolveWarning = false
				break
			}
		}

		if !resolveWarning {
			continue
		}

		// Resolve warnings with the given type
		err := warnings.ResolveWarningsByLocalNodeAndType(d.cluster, i)
		if err != nil {
			logger.Warn("Failed to resolve warnings", log.Ctx{"err": err})
		}
	}

	// Run the post initialization actions
	err = d.Ready()
	if err != nil {
		return err
	}

	return nil
}

func (d *Daemon) startClusterTasks() {
	// Heartbeats
	d.taskClusterHeartbeat = d.clusterTasks.Add(cluster.HeartbeatTask(d.gateway))

	// Events
	d.clusterTasks.Add(cluster.Events(d.endpoints, d.cluster, d.serverCert, d.events.Forward))

	// Auto-sync images across the cluster (hourly)
	d.clusterTasks.Add(autoSyncImagesTask(d))

	// Start all background tasks
	d.clusterTasks.Start(d.ctx)
}

func (d *Daemon) stopClusterTasks() {
	d.clusterTasks.Stop(3 * time.Second)
	d.clusterTasks = task.Group{}
}

func (d *Daemon) Ready() error {
	// Check if clustered
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return err
	}

	if clustered {
		d.startClusterTasks()
	}

	d.startTime = time.Now()

	// FIXME: There's no hard reason for which we should not run these
	//        tasks in mock mode. However it requires that we tweak them so
	//        they exit gracefully without blocking (something we should do
	//        anyways) and they don't hit the internet or similar. Support
	//        for proper cancellation is something that has been started
	//        but has not been fully completed.
	if !d.os.MockMode {
		// Log expiry (daily)
		d.tasks.Add(expireLogsTask(d.State()))

		// Remove expired images (daily)
		d.taskPruneImages = d.tasks.Add(pruneExpiredImagesTask(d))

		// Auto-update images (every 6 hours, configurable)
		d.tasks.Add(autoUpdateImagesTask(d))

		// Auto-update instance types (daily)
		d.tasks.Add(instanceRefreshTypesTask(d))

		// Remove expired container backups (hourly)
		d.tasks.Add(pruneExpiredContainerBackupsTask(d))

		// Take snapshot of containers (minutely check of configurable cron expression)
		d.tasks.Add(autoCreateContainerSnapshotsTask(d))

		// Remove expired container snapshots (minutely)
		d.tasks.Add(pruneExpiredContainerSnapshotsTask(d))

		// Remove expired custom volume snapshots (minutely)
		d.tasks.Add(pruneExpireCustomVolumeSnapshotsTask(d))

		// Take snapshot of custom volumes (minutely check of configurable cron expression)
		d.tasks.Add(autoCreateCustomVolumeSnapshotsTask(d))

		// Remove resolved warnings (daily)
		d.tasks.Add(pruneResolvedWarningsTask(d))
	}

	// Start all background tasks
	d.tasks.Start(d.ctx)

	// Get daemon state struct
	s := d.State()

	// Restore containers
	instancesRestart(s)

	// Re-balance in case things changed while LXD was down
	deviceTaskBalance(s)

	// Unblock incoming requests
	close(d.readyChan)

	return nil
}

func (d *Daemon) numRunningContainers() (int, error) {
	results, err := instance.LoadNodeAll(d.State(), instancetype.Container)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, instance := range results {
		if instance.IsRunning() {
			count = count + 1
		}
	}

	return count, nil
}

// Kill signals the daemon that we want to shutdown, and that any work
// initiated from this point (e.g. database queries over gRPC) should not be
// retried in case of failure.
func (d *Daemon) Kill() {
	if d.gateway != nil {
		d.clusterMembershipMutex.Lock()
		d.clusterMembershipClosing = true
		d.clusterMembershipMutex.Unlock()
		err := handoverMemberRole(d)
		if err != nil {
			logger.Warnf("Could not handover member's responsibilities: %v", err)
		}
		d.gateway.Kill()
		d.cluster.Kill()
	}
}

// Stop stops the shared daemon.
func (d *Daemon) Stop() error {
	logger.Info("Starting shutdown sequence")
	errs := []error{}
	trackError := func(err error, desc string) {
		if err != nil {
			errs = append(errs, errors.Wrap(err, desc))
		}
	}

	trackError(d.tasks.Stop(3*time.Second), "Stop tasks")                // Give tasks a bit of time to cleanup.
	trackError(d.clusterTasks.Stop(3*time.Second), "Stop cluster tasks") // Give tasks a bit of time to cleanup.

	shouldUnmount := false
	if d.cluster != nil {
		// It might be that database nodes are all down, in that case
		// we don't want to wait too much.
		//
		// FIXME: it should be possible to provide a context or a
		//        timeout for database queries.
		ch := make(chan bool)
		go func() {
			n, err := d.numRunningContainers()
			if err != nil {
				logger.Warnf("Failed to get number of running containers: %v", err)
			}
			ch <- err != nil || n == 0
		}()
		select {
		case shouldUnmount = <-ch:
		case <-time.After(2 * time.Second):
			logger.Warnf("Give up waiting to get number of running containers")
			shouldUnmount = true
		}

		logger.Infof("Closing the database")
		err := d.cluster.Close()
		// If we got io.EOF the network connection was interrupted and
		// it's likely that the other node shutdown. Let's just log the
		// event and return cleanly.
		if errors.Cause(err) == sqldriver.ErrBadConn {
			logger.Debugf("Could not close remote database cleanly: %v", err)
		} else {
			trackError(err, "Close cluster database")
		}
	}
	if d.db != nil {
		trackError(d.db.Close(), "Close local database")
	}

	if d.gateway != nil {
		trackError(d.gateway.Shutdown(), "Shutdown dqlite")
	}

	if d.endpoints != nil {
		trackError(d.endpoints.Down(), "Shutdown endpoints")
	}

	if shouldUnmount {
		logger.Infof("Unmounting temporary filesystems")

		unix.Unmount(shared.VarPath("devlxd"), unix.MNT_DETACH)
		unix.Unmount(shared.VarPath("shmounts"), unix.MNT_DETACH)

		logger.Infof("Done unmounting temporary filesystems")
	} else {
		logger.Debugf(
			"Not unmounting temporary filesystems (containers are still running)")
	}

	if d.seccomp != nil {
		trackError(d.seccomp.Stop(), "Stop seccomp")
	}

	var err error
	if n := len(errs); n > 0 {
		format := "%v"
		if n > 1 {
			format += fmt.Sprintf(" (and %d more errors)", n)
		}
		err = fmt.Errorf(format, errs[0])
	}
	if err != nil {
		logger.Errorf("Failed to cleanly shutdown daemon: %v", err)
	}
	return err
}

// Setup external authentication
func (d *Daemon) setupExternalAuthentication(authEndpoint string, authPubkey string, expiry int64, domains string) error {
	// Parse the list of domains
	authDomains := []string{}
	for _, domain := range strings.Split(domains, ",") {
		if domain == "" {
			continue
		}

		authDomains = append(authDomains, strings.TrimSpace(domain))
	}

	// Allow disable external authentication
	if authEndpoint == "" {
		d.externalAuth = nil
		return nil
	}

	// Setup the candid client
	idmClient, err := candidclient.New(candidclient.NewParams{
		BaseURL: authEndpoint,
	})
	if err != nil {
		return err
	}

	idmClientWrapper := &IdentityClientWrapper{
		client:       idmClient,
		ValidDomains: authDomains,
	}

	// Generate an internal private key
	key, err := bakery.GenerateKey()
	if err != nil {
		return err
	}

	pkCache := bakery.NewThirdPartyStore()
	pkLocator := httpbakery.NewThirdPartyLocator(nil, pkCache)
	if authPubkey != "" {
		// Parse the public key
		pkKey := bakery.Key{}
		err := pkKey.UnmarshalText([]byte(authPubkey))
		if err != nil {
			return err
		}

		// Add the key information
		pkCache.AddInfo(authEndpoint, bakery.ThirdPartyInfo{
			PublicKey: bakery.PublicKey{Key: pkKey},
			Version:   3,
		})

		// Allow http URLs if we have a public key set
		if strings.HasPrefix(authEndpoint, "http://") {
			pkLocator.AllowInsecure()
		}
	}

	// Setup the bakery
	bakery := identchecker.NewBakery(identchecker.BakeryParams{
		Key:            key,
		Location:       authEndpoint,
		Locator:        pkLocator,
		Checker:        httpbakery.NewChecker(),
		IdentityClient: idmClientWrapper,
		Authorizer: identchecker.ACLAuthorizer{
			GetACL: func(ctx context.Context, op bakery.Op) ([]string, bool, error) {
				return []string{identchecker.Everyone}, false, nil
			},
		},
	})

	// Store our settings
	d.externalAuth = &externalAuth{
		endpoint: authEndpoint,
		expiry:   expiry,
		bakery:   bakery,
	}

	return nil
}

// Setup RBAC
func (d *Daemon) setupRBACServer(rbacURL string, rbacKey string, rbacExpiry int64, rbacAgentURL string, rbacAgentUsername string, rbacAgentPrivateKey string, rbacAgentPublicKey string) error {
	if d.rbac != nil || rbacURL == "" || rbacAgentURL == "" || rbacAgentUsername == "" || rbacAgentPrivateKey == "" || rbacAgentPublicKey == "" {
		return nil
	}

	// Get a new server struct
	server, err := rbac.NewServer(rbacURL, rbacKey, rbacAgentURL, rbacAgentUsername, rbacAgentPrivateKey, rbacAgentPublicKey)
	if err != nil {
		return err
	}

	// Set projects helper
	server.ProjectsFunc = func() (map[int64]string, error) {
		var result map[int64]string
		err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
			var err error
			result, err = tx.GetProjectIDsToNames()
			return err
		})

		return result, err
	}

	// Perform full sync when online
	go func() {
		for {
			err = server.SyncProjects()
			if err != nil {
				time.Sleep(time.Minute)
				continue
			}

			break
		}
	}()

	server.StartStatusCheck()

	d.rbac = server

	// Enable candid authentication
	err = d.setupExternalAuthentication(fmt.Sprintf("%s/auth", rbacURL), rbacKey, rbacExpiry, "")
	if err != nil {
		return err
	}

	return nil
}

// Setup MAAS
func (d *Daemon) setupMAASController(server string, key string, machine string) error {
	var err error
	d.maas = nil

	// Default the machine name to the hostname
	if machine == "" {
		machine, err = os.Hostname()
		if err != nil {
			return err
		}
	}

	// We need both URL and key, otherwise disable MAAS
	if server == "" || key == "" {
		return nil
	}

	// Get a new controller struct
	controller, err := maas.NewController(server, key, machine)
	if err != nil {
		d.maas = nil
		return err
	}

	d.maas = controller
	return nil
}

// Create a database connection and perform any updates needed.
func initializeDbObject(d *Daemon) (*db.Dump, error) {
	logger.Info("Initializing local database")
	// Rename the old database name if needed.
	if shared.PathExists(d.os.LegacyLocalDatabasePath()) {
		if shared.PathExists(d.os.LocalDatabasePath()) {
			return nil, fmt.Errorf("Both legacy and new local database files exists")
		}
		logger.Info("Renaming local database file from lxd.db to database/local.db")
		err := os.Rename(d.os.LegacyLocalDatabasePath(), d.os.LocalDatabasePath())
		if err != nil {
			return nil, errors.Wrap(err, "Failed to rename legacy local database file")
		}
	}

	// NOTE: we use the legacyPatches parameter to run a few
	// legacy non-db updates that were in place before the
	// patches mechanism was introduced in lxd/patches.go. The
	// rest of non-db patches will be applied separately via
	// patchesApplyAll. See PR #3322 for more details.
	legacy := map[int]*db.LegacyPatch{}
	for i, patch := range legacyPatches {
		legacy[i] = &db.LegacyPatch{
			Hook: func(tx *sql.Tx) error {
				return patch(tx)
			},
		}
	}

	// Hook to run when the local database is created from scratch. It will
	// create the default profile and mark all patches as applied.
	freshHook := func(db *db.Node) error {
		for _, patchName := range patchesGetNames() {
			err := db.MarkPatchAsApplied(patchName)
			if err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	var dump *db.Dump
	d.db, dump, err = db.OpenNode(filepath.Join(d.os.VarDir, "database"), freshHook, legacy)
	if err != nil {
		return nil, fmt.Errorf("Error creating database: %s", err)
	}

	return dump, nil
}

func (d *Daemon) hasNodeListChanged(heartbeatData *cluster.APIHeartbeat) bool {
	// No previous heartbeat data.
	if d.lastNodeList == nil {
		return true
	}

	// Member count has changed.
	if len(d.lastNodeList.Members) != len(heartbeatData.Members) {
		return true
	}

	// Check for node address changes.
	for lastMemberID, lastMember := range d.lastNodeList.Members {
		if heartbeatData.Members[lastMemberID].Address != lastMember.Address {
			return true
		}
	}

	return false
}

// NodeRefreshTask is run each time a fresh node is generated.
// This can be used to trigger actions when the node list changes.
func (d *Daemon) NodeRefreshTask(heartbeatData *cluster.APIHeartbeat) {
	// Don't process the heartbeat until we're fully online
	if d.cluster == nil || d.cluster.GetNodeID() == 0 {
		return
	}

	// If the max version of the cluster has changed, check whether we need to upgrade.
	if d.lastNodeList == nil || d.lastNodeList.Version.APIExtensions != heartbeatData.Version.APIExtensions || d.lastNodeList.Version.Schema != heartbeatData.Version.Schema {
		err := cluster.MaybeUpdate(d.State())
		if err != nil {
			logger.Error("Error updating", log.Ctx{"err": err})
			return
		}
	}

	isDegraded := false
	hasNodesNotPartOfRaft := false
	voters := 0
	standbys := 0

	// Only refresh forkdns peers if the full state list has been generated.
	if heartbeatData.FullStateList && len(heartbeatData.Members) > 0 {
		for i, node := range heartbeatData.Members {
			role := db.RaftRole(node.RaftRole)
			// Exclude nodes that the leader considers offline.
			// This is to avoid forkdns delaying results by querying an offline node.
			if !node.Online {
				if role != db.RaftSpare {
					isDegraded = true
				}
				logger.Warn("Excluding offline member from refresh", log.Ctx{"address": node.Address, "ID": node.ID, "raftID": node.RaftID, "lastHeartbeat": node.LastHeartbeat})
				delete(heartbeatData.Members, i)
			}
			switch role {
			case db.RaftVoter:
				voters++
			case db.RaftStandBy:
				standbys++
			}
			if node.RaftID == 0 {
				hasNodesNotPartOfRaft = true
			}

		}

		nodeListChanged := d.hasNodeListChanged(heartbeatData)
		if nodeListChanged {
			logger.Debug("Member list has changed")
			updateCertificateCache(d)

			err := networkUpdateForkdnsServersTask(d.State(), heartbeatData)
			if err != nil {
				logger.Error("Error refreshing forkdns", log.Ctx{"err": err})
				return
			}
		}
	}

	// Only update the node list if the task succeeded.
	// If it fails then it will get to run again next heartbeat.
	d.lastNodeList = heartbeatData

	// If there are offline members that have voter or stand-by database
	// roles, let's see if we can replace them with spare ones. Also, if we
	// don't have enough voters or standbys, let's see if we can upgrade
	// some member.
	if len(heartbeatData.Members) > 2 {
		var maxVoters int64
		var maxStandBy int64
		err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
			config, err := cluster.ConfigLoad(tx)
			if err != nil {
				return err
			}
			maxVoters = config.MaxVoters()
			maxStandBy = config.MaxStandBy()
			return nil
		})
		if err != nil {
			logger.Errorf("Error loading cluster configuration: %v", err)
			return
		}

		if isDegraded || voters < int(maxVoters) || standbys < int(maxStandBy) {
			go func() {
				// Wait a little bit, just to avoid spurious
				// attempts due to nodes being shut down.
				time.Sleep(5 * time.Second)
				d.clusterMembershipMutex.Lock()
				defer d.clusterMembershipMutex.Unlock()
				if d.clusterMembershipClosing {
					return
				}
				err := rebalanceMemberRoles(d, nil)
				if err != nil && errors.Cause(err) != cluster.ErrNotLeader {
					logger.Warnf("Could not rebalance cluster member roles: %v", err)
				}
			}()
		}
		if hasNodesNotPartOfRaft {
			go func() {
				d.clusterMembershipMutex.Lock()
				defer d.clusterMembershipMutex.Unlock()
				err := upgradeNodesWithoutRaftRole(d)
				if err != nil && errors.Cause(err) != cluster.ErrNotLeader {
					logger.Warnf("Failed upgrade raft roles: %v", err)
				}
			}()
		}
	}
}
