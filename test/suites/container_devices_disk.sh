test_container_devices_disk() {
  ensure_import_testimage
  ensure_has_localhost_remote "${LXD_ADDR}"

  lxc launch testimage foo

  test_container_devices_disk_shift

  lxc delete -f foo
}

test_container_devices_disk_shift() {
  if ! grep -q shiftfs /proc/filesystems; then
    return
  fi

  # Test basic shiftfs
  mkdir -p "${TEST_DIR}/shift-source"
  touch "${TEST_DIR}/shift-source/a"
  chown 123:456 "${TEST_DIR}/shift-source/a"

  lxc config device add foo shiftfs disk source="${TEST_DIR}/shift-source" path=/mnt
  [ "$(lxc exec foo -- stat /mnt/a -c '%u:%g')" = "65534:65534" ] || false
  lxc config device remove foo shiftfs

  lxc config device add foo shiftfs disk source="${TEST_DIR}/shift-source" path=/mnt shift=true
  [ "$(lxc exec foo -- stat /mnt/a -c '%u:%g')" = "123:456" ] || false

  lxc stop foo -f
  lxc start foo
  [ "$(lxc exec foo -- stat /mnt/a -c '%u:%g')" = "123:456" ] || false
  lxc config device remove foo shiftfs
  lxc stop foo -f

  # Test shifted custom volumes
  POOL=$(lxc profile device get default root pool)
  lxc storage volume create "${POOL}" foo-shift security.shifted=true

  lxc start foo
  lxc launch testimage foo-priv -c security.privileged=true
  lxc launch testimage foo-isol1 -c security.idmap.isolated=true
  lxc launch testimage foo-isol2 -c security.idmap.isolated=true

  lxc config device add foo shifted disk pool="${POOL}" source=foo-shift path=/mnt
  lxc config device add foo-priv shifted disk pool="${POOL}" source=foo-shift path=/mnt
  lxc config device add foo-isol1 shifted disk pool="${POOL}" source=foo-shift path=/mnt
  lxc config device add foo-isol2 shifted disk pool="${POOL}" source=foo-shift path=/mnt

  lxc exec foo -- touch /mnt/a
  lxc exec foo -- chown 123:456 /mnt/a

  [ "$(lxc exec foo -- stat /mnt/a -c '%u:%g')" = "123:456" ] || false
  [ "$(lxc exec foo-priv -- stat /mnt/a -c '%u:%g')" = "123:456" ] || false
  [ "$(lxc exec foo-isol1 -- stat /mnt/a -c '%u:%g')" = "123:456" ] || false
  [ "$(lxc exec foo-isol2 -- stat /mnt/a -c '%u:%g')" = "123:456" ] || false

  lxc delete -f foo-priv foo-isol1 foo-isol2
  lxc config device remove foo shifted
  lxc storage volume delete "${POOL}" foo-shift
  lxc stop foo -f
}
#- Add a new "test_container_devices_disk_ceph" function
#- Get storage backend with: lxd_backend=$(storage_backend "$LXD_DIR")
#- If lxd_backend isn't ceph, return from the function
#- If it is ceph, then create a temporary rbd pool name, something like "lxdtest-$(basename "${LXD_DIR}")-disk" would do the trick
#- Create a pool with "ceph osd pool create $RBD_POOL_NAME 1"
#- Create the rbd volume with "rbd create --pool $RBD_POOL_NAME blah 50MB"
#- Map the rbd volume with "rbd map --pool $RBD_POOL_NAME blah"
#- Create a filesystem on it with "mkfs.ext4"
#- Unmap the volume with "rbd unmap /dev/rbdX"
#- Create a privileged container (easiest) with "lxc launch testimage ceph-disk -c security.privileged=true"
#- Attach the volume to the container with "lxc config device add ceph-disk rbd disk source=ceph:$RBD_POOL_NAME/blah ceph.user_name=admin ceph.cluster_name=ceph path=/ceph"
#- Confirm that it's visible in the container with something like "lxc exec ceph-disk -- stat /ceph/lost+found"
#- Restart the container to validate it works on startup "lxc restart ceph-disk"
#- Confirm that it's visible in the container with something like "lxc exec cephfs-disk -- stat /cephfs"
#- Delete the container "lxc delete -f ceph-disk"
#- Add a new "test_container_devices_disk_cephfs" function
#- Get storage backend with: lxd_backend=$(storage_backend "$LXD_DIR")
#- If lxd_backend isn't ceph, return from the function
#- If LXD_CEPH_CEPHFS is empty, return from the function
#- Create a privileged container (easiest) with "lxc launch testimage ceph-fs -c security.privileged=true"
#- Attach the volume to the container with "lxc config device add ceph-fs fs disk source=cephfs:$LXD_CEPH_CEPHFS/ ceph.user_name=admin ceph.cluster_name=ceph path=/cephfs"
#- Confirm that it's visible in the container with something like "lxc exec cephfs-disk -- stat /cephfs"
#- Restart the container to validate it works on startup "lxc restart cephfs-disk"
#- Confirm that it's visible in the container with something like "lxc exec cephfs-disk -- stat /cephfs"
#- Delete the container "lxc delete -f cephfs-disk"
#- Add both functions to test_container_devices_disk

test_container_devices_disk_ceph() {
  local LXD_BACKEND

  LXD_BACKEND=$(storage_backend "$LXD_DIR")
  if ! [ "${LXD_BACKEND}" = "ceph" ]; then
    return
  fi
  RBD_POOL_NAME=lxdtest-$(basename "${LXD_DIR}")-disk
  ceph osd pool create $RBD_POOL_NAME 1
  rbd create --pool $RBD_POOL_NAME --size 50MB
  rbd map --pool $RBD_POOL_NAME --name admin
  RBD_POOL_PATH="/dev/rbd/${RBD_POOL_NAME}"
  mkfs.ext4 -m0 $RBD_POOL_PATH
  rbd unmap $RBD_POOL_PATH
  lxc launch testimage ceph-disk -c security.privileged=true
  lxc config device add ceph-disk rbd disk source=ceph:$RBD_POOL_NAME/my-volume ceph.user_name=admin ceph.cluster_name=ceph path=/ceph
  lxc exec ceph-disk -- stat /ceph/lost+found
  lxc restart ceph-disk
  lxc exec cephfs-disk -- stat /cephfs
  lxc delete -f ceph-disklxc delete -f ceph-disk
}

test_container_devices_disk_cephfs() {
  local LXD_BACKEND

  LXD_BACKEND=$(storage_backend "$LXD_DIR")
  if ! [ "${LXD_BACKEND}" = "ceph" ]|| [ -z "${LXD_CEPH_CEPHFS:-}" ]; then
    return
  fi
#  ceph osd pool create cephfs_data
#  ceph osd pool create cephfs_metadata
#  ceph fs new $LXD_CEPH_CEPHFS cephfs_metadata cephfs_data
  lxc launch testimage ceph-fs -c security.privileged=true
  lxc config device add ceph-fs fs disk source=cephfs:$LXD_CEPH_CEPHFS/ ceph.user_name=admin ceph.cluster_name=ceph path=/cephfs
  lxc exec cephfs-disk -- stat /cephfs
  lxc restart cephfs-disk
  lxc exec cephfs-disk -- stat /cephfs
  lxc delete -f cephfs-disk
}