# Helper functions related to storage backends.

# Whether a storage backend is available
storage_backend_available() {
    # shellcheck disable=2039
    local backends
    backends="$(available_storage_backends)"
    [ "${backends#*$1}" != "$backends" ]
}

# Choose a random available backend, excluding LXD_BACKEND
random_storage_backend() {
    # shellcheck disable=2046
    shuf -e $(available_storage_backends) | head -n 1
}

# Return the storage backend being used by a LXD instance
storage_backend() {
    cat "$1/lxd.backend"
}

# Return a list of available storage backends
available_storage_backends() {
    # shellcheck disable=2039
    local backend backends storage_backends

    backends="dir" # always available

    storage_backends="btrfs lvm zfs"
    if [ -n "${LXD_CEPH_CLUSTER:-}" ]; then
        storage_backends="${storage_backends} ceph"
    fi

    for backend in $storage_backends; do
        if which "$backend" >/dev/null 2>&1; then
            backends="$backends $backend"
        fi
    done

    echo "$backends"
}

configure_loop_device() {
    # shellcheck disable=SC2039
    local lv_loop_file pvloopdev

    lv_loop_file=$(mktemp -p "${TEST_DIR}" XXXX.img)
    truncate -s 10G "${lv_loop_file}"
    pvloopdev=$(losetup --show -f "${lv_loop_file}")
    if [ ! -e "${pvloopdev}" ]; then
        echo "failed to setup loop"
        false
    fi
    echo "${pvloopdev}" >> "${TEST_DIR}/loops"

    # The following code enables to return a value from a shell function by
    # calling the function as: fun VAR1

    # shellcheck disable=2039
    local __tmp1="${1}"
    # shellcheck disable=2039
    local res1="${lv_loop_file}"
    if [ "${__tmp1}" ]; then
        eval "${__tmp1}='${res1}'"
    fi

    # shellcheck disable=2039
    local __tmp2="${2}"
    # shellcheck disable=2039
    local res2="${pvloopdev}"
    if [ "${__tmp2}" ]; then
        eval "${__tmp2}='${res2}'"
    fi
}

deconfigure_loop_device() {
    # shellcheck disable=SC2039
    local lv_loop_file loopdev success
    lv_loop_file="${1}"
    loopdev="${2}"
    success=0
    # shellcheck disable=SC2034
    for i in $(seq 10); do
        if losetup -d "${loopdev}"; then
            success=1
            break
        fi

        sleep 0.5
    done

    if [ "${success}" = "0" ]; then
        echo "Failed to tear down loop device"
        return 1
    fi

    rm -f "${lv_loop_file}"
    sed -i "\|^${loopdev}|d" "${TEST_DIR}/loops"
}
