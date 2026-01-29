set -euo pipefail

node="${SKYFORGE_NODE_NAME:?SKYFORGE_NODE_NAME is required}"
echo "[clabernetes] vrnetlab iol bootstrap starting (node=${node} pid=${IOL_PID})"

IFS=',' read -r -a link_ifaces <<< "${SKYFORGE_IOL_LINK_IFACES:-}"
for ifn in "${link_ifaces[@]}"; do
  if [ -z "$ifn" ]; then
    continue
  fi
  for i in $(seq 1 90); do
    if [ -e "/sys/class/net/$ifn" ]; then
      break
    fi
    sleep 1
  done
done

mkdir -p /vrnetlab
touch "/vrnetlab/${SKYFORGE_IOL_NVRAM}"

# NETMAP: map ios ports to linux ifaces configured in iouyap.ini.
{
  echo "${IOL_PID}:0/0 513:0/0"
  idx=1
  for ifn in "${link_ifaces[@]}"; do
    if [ -z "$ifn" ]; then
      continue
    fi
    slot=$((idx / 4))
    port=$((idx % 4))
    echo "${IOL_PID}:${slot}/${port} 513:${slot}/${port}"
    idx=$((idx+1))
  done
} > /vrnetlab/NETMAP

# Build /iol/config.txt (IOS boot config) similar to containerlab's iol kind driver.
#
# Important: do NOT "steal" the Kubernetes pod IP (eth0) for IOS management.
# In CNI setups where pod IPs are /32 and routed, moving the pod IP
# into the VM breaks pod routing and makes the pod unreachable from other nodes.
#
# Instead, create an internal management veth pair:
# - host side: vrl-mgmt0 (169.254.100.1/30)
# - IOS side:  vrl-mgmt1 (attached to IOS Ethernet0/0 via iouyap)
# The clabernetes launcher will run a TCP proxy on podIP:22 -> 169.254.100.2:22.
MGMT_HOST_DEV="vrl-mgmt0"
MGMT_IOS_DEV="vrl-mgmt1"
MGMT_HOST_IP="169.254.100.1/30"
MGMT_IOS_IP="169.254.100.2"
MGMT_IOS_MASK="255.255.255.252"

if ! ip link show "${MGMT_HOST_DEV}" >/dev/null 2>&1; then
  ip link add "${MGMT_HOST_DEV}" type veth peer name "${MGMT_IOS_DEV}"
fi
ip link set "${MGMT_HOST_DEV}" up
ip link set "${MGMT_IOS_DEV}" up
ip addr replace "${MGMT_HOST_IP}" dev "${MGMT_HOST_DEV}"

# IOUYAP config mapping bay/unit ports to linux ifaces.
{
  echo "[default]"
  echo "base_port = 49000"
  echo "netmap = /iol/NETMAP"
  echo "[513:0/0]"
  # Management interface:
  echo "eth_dev = ${MGMT_IOS_DEV}"
  idx=1
  for ifn in "${link_ifaces[@]}"; do
    if [ -z "$ifn" ]; then
      continue
    fi
    slot=$((idx / 4))
    port=$((idx % 4))
    echo "[513:${slot}/${port}]"
    echo "eth_dev = $ifn"
    idx=$((idx+1))
  done
} > /vrnetlab/iouyap.ini

: > /vrnetlab/config.txt
cat >> /vrnetlab/config.txt <<CFGEOF
hostname ${node}
!
no aaa new-model
!
ip domain name lab
!
ip cef
!
ipv6 unicast-routing
!
no ip domain lookup
!
username admin privilege 15 secret admin
!
interface Ethernet0/0
 description clab-mgmt
 ip address ${MGMT_IOS_IP} ${MGMT_IOS_MASK}
 no cdp enable
 no lldp transmit
 no lldp receive
 no shutdown
!
ip forward-protocol nd
!
ip ssh version 2
crypto key generate rsa modulus 2048
!
line vty 0 4
 login local
 transport input ssh
!
CFGEOF

if [ -f /netlab/initial.cfg ]; then
  # netlab-generated initial.cfg may include its own "line vty" stanza. That can
  # unintentionally disable SSH access. Strip it and re-assert SSH on the vty lines below.
  #
  # It also commonly includes an "interface Ethernet0/0" stanza (management interface for IOL)
  # which can override the vrf/ip config we generate above. Strip that too.
  awk '
    BEGIN { in_vty=0; in_mgmt_if=0 }
    $0 == "line vty 0 4" { in_vty=1; next }
    $0 == "interface Ethernet0/0" { in_mgmt_if=1; next }
    in_vty {
      if ($0 == "!") { in_vty=0 }
      next
    }
    in_mgmt_if {
      if ($0 == "!") { in_mgmt_if=0 }
      next
    }
    # Strip netlab IOS SSH directives that bind SSH to a VRF/source-interface.
    $0 ~ /^ip ssh server vrf / { next }
    $0 ~ /^ip ssh source-interface / { next }
    { print }
  ' /netlab/initial.cfg >> /vrnetlab/config.txt
  echo "!" >> /vrnetlab/config.txt
fi

# netlab produces additional cfglets/snippets under /tmp/skyforge-c9s/<topology>/node_files/<node>/.
# For IOS/IOS-XE (IOL) we want those applied as part of the initial config load, so append them.
for f in /tmp/skyforge-c9s/*/node_files/${node}/*; do
  if [ ! -f "$f" ]; then
    continue
  fi
  bn="$(basename "$f")"
  if [ "$bn" = "initial" ] || [ "$bn" = "initial.cfg" ]; then
    continue
  fi
  awk '
    BEGIN { in_vty=0; in_mgmt_if=0 }
    $0 == "line vty 0 4" { in_vty=1; next }
    $0 == "interface Ethernet0/0" { in_mgmt_if=1; next }
    in_vty {
      if ($0 == "!") { in_vty=0 }
      next
    }
    in_mgmt_if {
      if ($0 == "!") { in_mgmt_if=0 }
      next
    }
    $0 ~ /^ip ssh server vrf / { next }
    $0 ~ /^ip ssh source-interface / { next }
    { print }
  ' "$f" >> /vrnetlab/config.txt
  echo "!" >> /vrnetlab/config.txt
done

cat >> /vrnetlab/config.txt <<CFGEOF
line vty 0 4
 login local
 transport input ssh
!
CFGEOF

echo "end" >> /vrnetlab/config.txt

# Symlink the runtime artifacts into /iol to match containerlab expectations.
ln -sf /vrnetlab/NETMAP /iol/NETMAP
ln -sf /vrnetlab/iouyap.ini /iol/iouyap.ini
ln -sf /vrnetlab/config.txt /iol/config.txt
ln -sf "/vrnetlab/${SKYFORGE_IOL_NVRAM}" "/iol/${SKYFORGE_IOL_NVRAM}"

# Start iouyap (background) + IOL.
/usr/bin/iouyap -f /iol/iouyap.ini 513 -q -d

ports=$(( ${#link_ifaces[@]} + 1 ))
slots=$(( (ports + 3) / 4 ))
echo "[clabernetes] starting iol.bin (slots=$slots ports=$ports mgmt=${MGMT_IOS_IP}/${MGMT_IOS_MASK})"
cd /iol
exec ./iol.bin "$IOL_PID" -e "$slots" -s 0 -c config.txt -n 1024

