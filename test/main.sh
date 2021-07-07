#!/bin/sh -eu
[ -n "${GOPATH:-}" ] && export "PATH=${GOPATH}/bin:${PATH}"

# Don't translate lxc output for parsing in it in tests.
export LC_ALL="C"

# Force UTC for consistency
export TZ="UTC"

if [ -n "${LXD_VERBOSE:-}" ] || [ -n "${LXD_DEBUG:-}" ]; then
  set -x
fi

export DEBUG=""
if [ -n "${LXD_VERBOSE:-}" ]; then
  DEBUG="--verbose"
fi

if [ -n "${LXD_DEBUG:-}" ]; then
  DEBUG="--debug"
fi

if [ -z "${LXD_BACKEND:-}" ]; then
    LXD_BACKEND="dir"
fi

# shellcheck disable=SC2034
LXD_NETNS=""

# Always ignore SC2230 ('which' is non-standard. Use builtin 'command -v' instead.)
export SHELLCHECK_OPTS="-e SC2230"

import_subdir_files() {
    test "$1"
    # shellcheck disable=SC2039
    local file
    for file in "$1"/*.sh; do
        # shellcheck disable=SC1090
        . "$file"
    done
}

import_subdir_files includes

echo "==> Checking for dependencies"
check_dependencies lxd lxc curl dnsmasq jq git xgettext sqlite3 msgmerge msgfmt shuf setfacl uuidgen socat dig

if [ "${USER:-'root'}" != "root" ]; then
  echo "The testsuite must be run as root." >&2
  exit 1
fi

if [ -n "${LXD_LOGS:-}" ] && [ ! -d "${LXD_LOGS}" ]; then
  echo "Your LXD_LOGS path doesn't exist: ${LXD_LOGS}"
  exit 1
fi

echo "==> Available storage backends: $(available_storage_backends | sort)"
if [ "$LXD_BACKEND" != "random" ] && ! storage_backend_available "$LXD_BACKEND"; then
  if [ "${LXD_BACKEND}" = "ceph" ] && [ -z "${LXD_CEPH_CLUSTER:-}" ]; then
    echo "Ceph storage backend requires that \"LXD_CEPH_CLUSTER\" be set."
    exit 1
  fi
  echo "Storage backend \"$LXD_BACKEND\" is not available"
  exit 1
fi
echo "==> Using storage backend ${LXD_BACKEND}"

import_storage_backends

cleanup() {
  # Allow for failures and stop tracing everything
  set +ex
  DEBUG=

  # Allow for inspection
  if [ -n "${LXD_INSPECT:-}" ]; then
    if [ "${TEST_RESULT}" != "success" ]; then
      echo "==> TEST DONE: ${TEST_CURRENT_DESCRIPTION}"
    fi
    echo "==> Test result: ${TEST_RESULT}"

    # shellcheck disable=SC2086
    printf "To poke around, use:\\n LXD_DIR=%s LXD_CONF=%s sudo -E %s/bin/lxc COMMAND\\n" "${LXD_DIR}" "${LXD_CONF}" ${GOPATH:-}
    echo "Tests Completed (${TEST_RESULT}): hit enter to continue"

    # shellcheck disable=SC2034
    read -r nothing
  fi

  echo "==> Cleaning up"

  kill_external_auth_daemon "$TEST_DIR"
  cleanup_lxds "$TEST_DIR"

  echo ""
  echo ""
  if [ "${TEST_RESULT}" != "success" ]; then
    echo "==> TEST DONE: ${TEST_CURRENT_DESCRIPTION}"
  fi
  echo "==> Test result: ${TEST_RESULT}"
}

# Must be set before cleanup()
TEST_CURRENT=setup
# shellcheck disable=SC2034
TEST_RESULT=failure

trap cleanup EXIT HUP INT TERM

# Import all the testsuites
import_subdir_files suites

# Setup test directory
TEST_DIR=$(mktemp -d -p "$(pwd)" tmp.XXX)
chmod +x "${TEST_DIR}"

if [ -n "${LXD_TMPFS:-}" ]; then
  mount -t tmpfs tmpfs "${TEST_DIR}" -o mode=0751
fi

LXD_CONF=$(mktemp -d -p "${TEST_DIR}" XXX)
export LXD_CONF

LXD_DIR=$(mktemp -d -p "${TEST_DIR}" XXX)
export LXD_DIR
chmod +x "${LXD_DIR}"
spawn_lxd "${LXD_DIR}" true
LXD_ADDR=$(cat "${LXD_DIR}/lxd.addr")
export LXD_ADDR

start_external_auth_daemon "${LXD_DIR}"

run_test() {
  TEST_CURRENT=${1}
  TEST_CURRENT_DESCRIPTION=${2:-${1}}

  echo "==> TEST BEGIN: ${TEST_CURRENT_DESCRIPTION}"
  START_TIME=$(date +%s)
  ${TEST_CURRENT}
  END_TIME=$(date +%s)

  echo "==> TEST DONE: ${TEST_CURRENT_DESCRIPTION} ($((END_TIME-START_TIME))s)"
}

# allow for running a specific set of tests
if [ "$#" -gt 0 ]; then
  run_test "test_${1}"
  # shellcheck disable=SC2034
  TEST_RESULT=success
  exit
fi

run_test test_check_deps "checking dependencies"
run_test test_static_analysis "static analysis"
run_test test_database_update "database schema updates"
run_test test_database_restore "database restore"
run_test test_database_no_disk_space "database out of disk space"
run_test test_sql "lxd sql"
run_test test_tls_restrictions "TLS restrictions"
run_test test_basic_usage "basic usage"
run_test test_remote_url "remote url handling"
run_test test_remote_admin "remote administration"
run_test test_remote_usage "remote usage"
run_test test_clustering_enable "clustering enable"
run_test test_clustering_membership "clustering membership"
run_test test_clustering_containers "clustering containers"
run_test test_clustering_storage "clustering storage"
run_test test_clustering_storage_single_node "clustering storage single node"
run_test test_clustering_network "clustering network"
run_test test_clustering_publish "clustering publish"
run_test test_clustering_profiles "clustering profiles"
run_test test_clustering_join_api "clustering join api"
run_test test_clustering_shutdown_nodes "clustering shutdown"
run_test test_clustering_projects "clustering projects"
run_test test_clustering_update_cert "clustering update cert"
run_test test_clustering_address "clustering address"
run_test test_clustering_image_replication "clustering image replication"
run_test test_clustering_dns "clustering DNS"
run_test test_clustering_recover "clustering recovery"
run_test test_clustering_handover "clustering handover"
run_test test_clustering_rebalance "clustering rebalance"
run_test test_clustering_remove_raft_node "clustering remove raft node"
run_test test_clustering_failure_domains "clustering failure domains"
run_test test_clustering_image_refresh "clustering image refresh"
run_test test_clustering_evacuation "clustering evacuation"
# run_test test_clustering_upgrade "clustering upgrade"
run_test test_projects_default "default project"
run_test test_projects_crud "projects CRUD operations"
run_test test_projects_containers "containers inside projects"
run_test test_projects_snapshots "snapshots inside projects"
run_test test_projects_backups "backups inside projects"
run_test test_projects_profiles "profiles inside projects"
run_test test_projects_profiles_default "profiles from the global default project"
run_test test_projects_images "images inside projects"
run_test test_projects_images_default "images from the global default project"
run_test test_projects_storage "projects and storage pools"
run_test test_projects_network "projects and networks"
run_test test_projects_limits "projects limits"
run_test test_projects_usage "projects usage"
run_test test_projects_restrictions "projects restrictions"
run_test test_container_devices_disk "container devices - disk"
run_test test_container_devices_nic_p2p "container devices - nic - p2p"
run_test test_container_devices_nic_bridged "container devices - nic - bridged"
run_test test_container_devices_nic_bridged_acl "container devices - nic - bridged - acl"
run_test test_container_devices_nic_bridged_filtering "container devices - nic - bridged - filtering"
run_test test_container_devices_nic_bridged_vlan "container devices - nic - bridged - vlan"
run_test test_container_devices_nic_physical "container devices - nic - physical"
run_test test_container_devices_nic_macvlan "container devices - nic - macvlan"
run_test test_container_devices_nic_ipvlan "container devices - nic - ipvlan"
run_test test_container_devices_nic_sriov "container devices - nic - sriov"
run_test test_container_devices_nic_routed "container devices - nic - routed"
run_test test_container_devices_infiniband_physical "container devices - infiniband - physical"
run_test test_container_devices_infiniband_sriov "container devices - infiniband - sriov"
run_test test_container_devices_proxy "container devices - proxy"
run_test test_container_devices_gpu "container devices - gpu"
run_test test_container_devices_unix_char "container devices - unix-char"
run_test test_container_devices_unix_block "container devices - unix-block"
run_test test_container_devices_tpm "container devices - tpm"
run_test test_security "security features"
run_test test_security_protection "container protection"
run_test test_image_expiry "image expiry"
run_test test_image_list_all_aliases "image list all aliases"
run_test test_image_auto_update "image auto-update"
run_test test_image_prefer_cached "image prefer cached"
run_test test_image_import_dir "import image from directory"
run_test test_concurrent_exec "concurrent exec"
run_test test_concurrent "concurrent startup"
run_test test_snapshots "container snapshots"
run_test test_snap_restore "snapshot restores"
run_test test_snap_expiry "snapshot expiry"
run_test test_snap_schedule "snapshot scheduling"
run_test test_config_profiles "profiles and configuration"
run_test test_config_edit "container configuration edit"
run_test test_config_edit_container_snapshot_pool_config "container and snapshot volume configuration edit"
run_test test_container_metadata "manage container metadata and templates"
run_test test_container_snapshot_config "container snapshot configuration"
run_test test_server_config "server configuration"
run_test test_filemanip "file manipulations"
run_test test_network "network management"
run_test test_network_acl "network ACL management"
run_test test_idmap "id mapping"
run_test test_template "file templating"
run_test test_pki "PKI mode"
run_test test_devlxd "/dev/lxd"
run_test test_fuidshift "fuidshift"
run_test test_migration "migration"
run_test test_fdleak "fd leak"
run_test test_storage "storage"
run_test test_storage_volume_snapshots "storage volume snapshots"
run_test test_init_auto "lxd init auto"
run_test test_init_interactive "lxd init interactive"
run_test test_init_preseed "lxd init preseed"
run_test test_storage_profiles "storage profiles"
run_test test_container_import "container import"
run_test test_storage_volume_attach "attaching storage volumes"
run_test test_storage_driver_btrfs "btrfs storage driver"
run_test test_storage_driver_ceph "ceph storage driver"
run_test test_storage_driver_cephfs "cephfs storage driver"
run_test test_resources "resources"
run_test test_kernel_limits "kernel limits"
run_test test_macaroon_auth "macaroon authentication"
run_test test_console "console"
run_test test_query "query"
run_test test_storage_local_volume_handling "storage local volume handling"
run_test test_backup_import "backup import"
run_test test_backup_export "backup export"
run_test test_backup_rename "backup rename"
run_test test_backup_volume_export "backup volume export"
run_test test_backup_volume_rename_delete "backup volume rename and delete"
run_test test_container_local_cross_pool_handling "container local cross pool handling"
run_test test_incremental_copy "incremental container copy"
run_test test_profiles_project_default "profiles in default project"
run_test test_profiles_project_images_profiles "profiles in project with images and profiles enabled"
run_test test_profiles_project_images "profiles in project with images enabled and profiles disabled"
run_test test_profiles_project_profiles "profiles in project with images disabled and profiles enabled"
run_test test_filtering "API filtering"
run_test test_warnings "Warnings"

# shellcheck disable=SC2034
TEST_RESULT=success
