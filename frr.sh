#!/bin/bash
#set -euo pipefail

# Enable verbose shell output if FRR_SH_VERBOSE is set to 'true'
if [[ "${FRR_SH_VERBOSE:-}" == "true" ]]; then
  set -x
fi

# The argument to the command is the operation to be performed
# frr-node display display_env 
# a cmd must be provided, there is no default
cmd=${1:-""}

# The frr user id, by default it is going to be frr:frr
frr_user_id=${FRR_USER_ID:-""}

# frr options
frr_options=${FRR_OPTIONS:-""}

# This script is the entrypoint to the image.
# frr.sh version (update when API between daemonset and script changes - v.x.y)
frr_version="3"

# The daemonset version must be compatible with this script.
# The default when FRR_DAEMONSET_VERSION is not set is version 3
frr_daemonset_version=${FRR_DAEMONSET_VERSION:-"3"}

# hostname is the host's hostname when using host networking,
# This is useful on the master
# otherwise it is the container ID (useful for debugging).
frr_pod_host=${K8S_NODE:-$(hostname)}

# The ovs user id, by default it is going to be root:root
frr_user_id=${FRR_USER_ID:-""}

# frr options
frr_options=${FRR_OPTIONS:-""}

# frr.conf variables
ocp_asn=${OCPASN:-65000}
ocp_routerid=${OCPROUTERID:-"10.10.10.1"}
ocp_peer=${OCPPEER:-"10.10.10.1"}

FRR_ETCDIR=/etc/frr
FRR_RUNDIR=/var/run/frr
FRR_LOGDIR=/var/log/frr

# =========================================

setup_frr_permissions() {
    chown -R ${frr_user_id} ${FRR_RUNDIR}
    chown -R ${frr_user_id} ${FRR_LOGDIR}
    chown -R ${frr_user_id} ${FRR_ETCDIR}
}

# =========================================

display_version() {
  echo " =================== hostname: ${frr_pod_host}"
  echo " =================== daemonset version ${frr_daemonset_version}"
  if [[ -f /root/git_info ]]; then
    disp_ver=$(cat /root/git_info)
    return
  fi
}

display_env() {
  echo FRR_USER_ID ${frr_user_id}
  echo FRR_OPTIONS ${frr_options}
  echo frr.sh version ${frr_version}
  echo ocp_asn ${ocp_asn}
  echo ocp_routerid ${ocp_routerid}
  echo ocp_peer ${ocp_peer}
}

# frr-node - all nodes
frr-node() {
  trap 'kill $(jobs -p) ; exit 0' TERM
  rm -f ${FRR_RUNDIR}/frr.pid
  echo "=============== frr-node ========== update frr.conf"
  sed -i "s/OCPASN/$ocp_asn/" /etc/frr/frr.conf
  sed -i "s/OCPPEER/$ocp_peer/" /etc/frr/frr.conf
  sed -i "s/OCPROUTERID/$ocp_routerid/" /etc/frr/frr.conf

  #chown -R frr:frr /etc/frr
  chown -R frr:frr ${FRR_RUNDIR}
  echo "=============== frr-node ========== starting"
  # /usr/lib/frr/frrinit.sh start
  # bash -x /usr/lib/frr/frrinit.sh start
  bash -x 
  /usr/lib/frr/frrinit.sh start
  frrResult=$?
  echo "=============== frrinit result is ${frrResult} " 
 
  # Sleep forever
  exec tail -f /dev/null
}

echo "================== frr.sh --- version: ${frr_version} ================"

display_version

display_env

case ${cmd} in
"frr-node") 
  frr-node
  ;;
"display_env")
  display_env
  exit 0
  ;;
"display")
  display
  exit 0
  ;;
*)
  echo "invalid command ${cmd}"
  echo "valid v3 commands: frr-node display_env display " 
  exit 0
  ;;
esac

exit 0
