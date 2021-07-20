test_storage_local_volume_handling() {
  ensure_import_testimage

  # shellcheck disable=2039
  local LXD_STORAGE_DIR lxd_backend
  # shellcheck disable=SC2034
  lxd_backend=$(storage_backend "$LXD_DIR")
  LXD_STORAGE_DIR=$(mktemp -d -p "${TEST_DIR}" XXXXXXXXX)
  chmod +x "${LXD_STORAGE_DIR}"
  spawn_lxd "${LXD_STORAGE_DIR}" false

  ensure_import_testimage

  (
    set -e
    # shellcheck disable=2030
    LXD_DIR="${LXD_STORAGE_DIR}"

    if storage_backend_available "btrfs"; then
      lxc storage create "lxdtest-$(basename "${LXD_DIR}")-btrfs" btrfs size=100GB
    fi

    if storage_backend_available "ceph"; then
      lxc storage create "lxdtest-$(basename "${LXD_DIR}")-ceph" ceph volume.size=25MB ceph.osd.pg_num=1
      if [ -n "${LXD_CEPH_CEPHFS:-}" ]; then
        lxc storage create "lxdtest-$(basename "${LXD_DIR}")-cephfs" cephfs source="${LXD_CEPH_CEPHFS}/$(basename "${LXD_DIR}")-cephfs"
      fi
    fi

    lxc storage create "lxdtest-$(basename "${LXD_DIR}")-dir" dir

    if storage_backend_available "lvm"; then
      lxc storage create "lxdtest-$(basename "${LXD_DIR}")-lvm" lvm volume.size=25MB
    fi

    if storage_backend_available "zfs"; then
      lxc storage create "lxdtest-$(basename "${LXD_DIR}")-zfs" zfs size=100GB
    fi

    # Test all combinations of our storage drivers

    driver="${lxd_backend}"
    pool_opts=

    if [ "$driver" = "btrfs" ] || [ "$driver" = "zfs" ]; then
      pool_opts="size=100GB"
    fi

    if [ "$driver" = "ceph" ]; then
      pool_opts="volume.size=25MB ceph.osd.pg_num=1"
    fi

    if [ "$driver" = "lvm" ]; then
      pool_opts="volume.size=25MB"
    fi

    if [ -n "${pool_opts}" ]; then
      # shellcheck disable=SC2086
      lxc storage create "lxdtest-$(basename "${LXD_DIR}")-${driver}1" "${driver}" $pool_opts
    else
      lxc storage create "lxdtest-$(basename "${LXD_DIR}")-${driver}1" "${driver}"
    fi

    lxc storage volume create "lxdtest-$(basename "${LXD_DIR}")-${driver}" vol1
    # This will create the snapshot vol1/snap0
    lxc storage volume snapshot "lxdtest-$(basename "${LXD_DIR}")-${driver}" vol1
    # Copy volume with snapshots
    lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${driver}1/vol1"
    # Ensure the target snapshot is there
    lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${driver}1" vol1/snap0
    # Copy volume only
    lxc storage volume copy --volume-only "lxdtest-$(basename "${LXD_DIR}")-${driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${driver}1/vol2"
    # Copy snapshot to volume
    lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${driver}/vol1/snap0" "lxdtest-$(basename "${LXD_DIR}")-${driver}1/vol3"
    lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${driver}1" vol1
    lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${driver}1" vol2
    lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${driver}1" vol3
    lxc storage volume move "lxdtest-$(basename "${LXD_DIR}")-${driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${driver}1/vol1"
    ! lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${driver}" vol1 || false
    lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${driver}1" vol1
    lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${driver}1" vol1
    lxc storage delete "lxdtest-$(basename "${LXD_DIR}")-${driver}1"

    for source_driver in "btrfs" "ceph" "cephfs" "dir" "lvm" "zfs"; do
      for target_driver in "btrfs" "ceph" "cephfs" "dir" "lvm" "zfs"; do
        # FIXME: Skip copies across old and new backends for now
        storage_compatible "${source_driver}" "${target_driver}" || continue
        storage_compatible "${target_driver}" "${source_driver}" || continue

        # shellcheck disable=SC2235
        if [ "$source_driver" != "$target_driver" ] \
            && ([ "$lxd_backend" = "$source_driver" ] || ([ "$lxd_backend" = "ceph" ] && [ "$source_driver" = "cephfs" ] && [ -n "${LXD_CEPH_CEPHFS:-}" ])) \
            && storage_backend_available "$source_driver" && storage_backend_available "$target_driver"; then
          # source_driver -> target_driver
          lxc storage volume create "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1
          # This will create the snapshot vol1/snap0
          lxc storage volume snapshot "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1
          # Copy volume with snapshots
          lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol1"
          # Ensure the target snapshot is there
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1/snap0
          # Copy volume only
          lxc storage volume copy --volume-only "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol2"
          # Copy snapshot to volume
          lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol1/snap0" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol3"
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol2
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol3
          lxc storage volume move "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol1"
          ! lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1 || false
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1

          # target_driver -> source_driver
          lxc storage volume create "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1
          lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol1"
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1

          lxc storage volume move "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol1"
          ! lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1 || false
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1

          if [ "${source_driver}" = "cephfs" ] || [ "${target_driver}" = "cephfs" ]; then
            continue
          fi

          # create custom block volume without snapshots
          lxc storage volume create "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1 --type=block size=4194304
          lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol1" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol1"
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1 | grep -q 'content_type: block'

          # create custom block volume with a snapshot
          lxc storage volume create "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol2 --type=block size=4194304
          lxc storage volume snapshot "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol2
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol2/snap0 | grep -q 'content_type: block'

          # restore snapshot
          lxc storage volume restore "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol2 snap0
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol2 | grep -q 'content_type: block'

          # copy with snapshots
          lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol2" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol2"
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol2 | grep -q 'content_type: block'
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol2/snap0 | grep -q 'content_type: block'

          # copy without snapshots
          lxc storage volume copy "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol2" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol3" --volume-only
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol3 | grep -q 'content_type: block'
          ! lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol3/snap0 | grep -q 'content_type: block' || false

          # move images
          lxc storage volume move "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol2" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol4"
          ! lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol2 | grep -q 'content_type: block' || false
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol4 | grep -q 'content_type: block'
          lxc storage volume show "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol4/snap0 | grep -q 'content_type: block'

          # create volumes
          lxc storage volume create "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol5 --type=block size=4194304
          lxc storage volume create "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol6 --type=block size=4194304
          # copy to empty volume destination with refresh flag
          lxc storage volume copy --refresh "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol5" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol5"
          # incremental copy to existing volume destination with refresh flag
          lxc storage volume copy --refresh "lxdtest-$(basename "${LXD_DIR}")-${source_driver}/vol6" "lxdtest-$(basename "${LXD_DIR}")-${target_driver}/vol5"

          # clean up
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol1
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol1
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol2
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol3
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol4
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol5
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${target_driver}" vol5
          lxc storage volume delete "lxdtest-$(basename "${LXD_DIR}")-${source_driver}" vol6
        fi
      done
    done
  )

  # shellcheck disable=SC2031
  LXD_DIR="${LXD_DIR}"
  kill_lxd "${LXD_STORAGE_DIR}"
}
