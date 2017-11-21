# Test helper for clustering

setup_clustering_bridge() {
  name="br$$"

  echo "==> Setup clustering bridge ${name}"

  brctl addbr "${name}"
  ip addr add 10.1.1.1/16 dev "${name}"
  ip link set dev "${name}" up

  iptables -t nat -A POSTROUTING -s 10.1.0.0/16 -d 0.0.0.0/0 -j MASQUERADE
  echo 1 > /proc/sys/net/ipv4/ip_forward
}

teardown_clustering_bridge() {
  name="br$$"

  if brctl show | grep -q "${name}" ; then
      echo "==> Teardown clustering bridge ${name}"
      echo 0 > /proc/sys/net/ipv4/ip_forward
      iptables -t nat -D POSTROUTING -s 10.1.0.0/16 -d 0.0.0.0/0 -j MASQUERADE
      ip link set dev "${name}" down
      ip addr del 10.1.1.1/16 dev "${name}"
      brctl delbr "${name}"
  fi
}

setup_clustering_netns() {
  id="${1}"
  shift

  prefix="lxd$$"
  ns="${prefix}${id}"

  echo "==> Setup clustering netns ${ns}"

  mkdir -p /run/netns
  touch "/run/netns/${ns}"

  unshare -n sh -c "mount --bind /proc/self/ns/net /run/netns/${ns}"

  veth1="v${ns}1"
  veth2="v${ns}2"

  ip link add "${veth1}" type veth peer name "${veth2}"
  ip link set "${veth2}" netns "${ns}"

  nsbridge="br$$"
  brctl addif "${nsbridge}" "${veth1}"

  ip link set "${veth1}" up

  (
    cat <<EOF
    ip link set dev lo up
    ip link set dev "${veth2}" name eth0
    ip link set eth0 up
    ip addr add "10.1.1.10${id}/16" dev eth0
    ip route add default via 10.1.1.1
EOF
  ) | nsenter --net="/run/netns/${ns}" sh
}

teardown_clustering_netns() {
  prefix="lxd$$"
  nsbridge="br$$"
  for ns in $(ip netns | grep "${prefix}" | cut -f 1 -d " ") ; do
      echo "==> Teardown clustering netns ${ns}"
      veth1="v${ns}1"
      veth2="v${ns}2"
      nsenter --net="/run/netns/${ns}" ip link set eth0 down
      nsenter --net="/run/netns/${ns}" ip link set lo down
      ip link set "${veth1}" down
      brctl delif "${nsbridge}" "${veth1}"
      ip link delete "${veth1}" type veth
      umount "/run/netns/${ns}"
      rm "/run/netns/${ns}"
  done
}

spawn_lxd_and_bootstrap_cluster() {
  set -e
  ns="${1}"
  bridge="${2}"
  LXD_DIR="${3}"
  LXD_NETNS="${ns}" spawn_lxd "${LXD_DIR}" false
  (
    set -e

    cat <<EOF | lxd init --preseed
config:
  core.trust_password: sekret
  core.https_address: 10.1.1.101:8443
  images.auto_update_interval: 15
storage_pools:
- name: data
  driver: dir
networks:
- name: $bridge
  type: bridge
  config:
    ipv4.address: none
    ipv6.address: none
profiles:
- name: default
  devices:
    root:
      path: /
      pool: data
      type: disk
cluster:
  name: node1
EOF
  )
}

spawn_lxd_and_join_cluster() {
  set -e
  ns="${1}"
  bridge="${2}"
  cert="${3}"
  index="${4}"
  target="${5}"
  LXD_DIR="${6}"

  LXD_NETNS="${ns}" spawn_lxd "${LXD_DIR}" false
  (
    set -e

    cat <<EOF | lxd init --preseed
config:
  core.https_address: 10.1.1.10${index}:8443
  images.auto_update_interval: 15
storage_pools:
- name: data
  driver: dir
networks:
- name: $bridge
  type: bridge
  config:
    ipv4.address: none
    ipv6.address: none
profiles:
- name: default
  devices:
    root:
      path: /
      pool: data
      type: disk
cluster:
  name: node${index}
  target_address: 10.1.1.10${target}:8443
  target_password: sekret
  target_cert: "$cert"
EOF
  )
}
