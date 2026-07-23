# Shared plumbing for the live demo. Sourced, not executed.
# Every script acts on the cluster kubectl currently points at
# (KUBECONFIG + current-context) — nothing is hardcoded to a cluster.
set -euo pipefail

NS=demo-iad
CLASS=demo               RANGE=10.242.42.0/24     # default class, reclaimPolicy Delete
RETAIN_CLASS=demo-retain RETAIN_RANGE=10.242.43.0/24

# Where the driver keeps its reservation placeholders, and where MetalLB lives.
IAD_NS=${IAD_NS:-cozy-metallb-iad}
METALLB_NS=${METALLB_NS:-cozy-metallb}

# Always fully qualified: on a modern cluster bare "ipaddresses" is a three-way
# ambiguity (networking.k8s.io ClusterIP ledger, CAPI's ipam.cluster.x-k8s.io,
# and ours) and the stock group wins.
IP=ipaddresses.local.sdn.cozystack.io
IPC=ipaddressclaims.local.sdn.cozystack.io
IPCLASS=ipaddressclasses.local.sdn.cozystack.io

MAGENTA=$'\e[1;35m'; CYAN=$'\e[1;36m'; YELLOW=$'\e[1;33m'; OFF=$'\e[0m'

banner() { printf '\n%s== %s ==%s\n' "$MAGENTA" "$*" "$OFF"; }
note()   { printf '%s  » %s%s\n' "$CYAN" "$*" "$OFF"; }
plain()  { printf '        %s\n' "$*"; }
# Print a read-only command, run it, and show its output indented — the
# audience sees both. Takes ONE string; failures print, never kill the act.
show()   { printf '%s        $ %s%s\n' "$YELLOW" "$1" "$OFF"; { bash -c "$1" 2>&1 || true; } | sed 's/^/        /'; echo; }
# Hold the stage until the presenter is done looking; no-op when scripted.
pause()  { if [ -t 0 ]; then echo; read -r -p "        [Enter: ${*:-continue}]"; echo; fi; }

claimip()   { kubectl -n "$NS" get "$IPC" "$1" -o jsonpath='{.status.addresses[0].address}'; }
addrname()  { kubectl -n "$NS" get "$IPC" "$1" -o jsonpath='{.status.addresses[0].name}'; }
svcip()     { kubectl -n "$NS" get svc "$1" -o jsonpath='{.status.loadBalancer.ingress[0].ip}'; }

# Poll a command until its output is non-empty (prints it) or the timeout hits.
wait_for() { # wait_for <seconds> <cmd...>
  local t=$1 out; shift
  for _ in $(seq "$t"); do
    out=$("$@" 2>/dev/null || true)
    [ -n "$out" ] && { echo "$out"; return 0; }
    sleep 1
  done
  return 1
}

# A claim is Bound once every requested family has an address.
wait_bound() { # wait_bound <claim>
  kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Bound "$IPC/$1" --timeout=120s >/dev/null
}

# A LoadBalancer Service attached to the demo claim via the tenant annotation.
lb_service() { # lb_service <name>
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $1
  namespace: $NS
  annotations: {local.sdn.cozystack.io/ip-address-claim: web}
spec:
  type: LoadBalancer
  selector: {app: web}
  ports: [{name: http, port: 80, targetPort: 8080, protocol: TCP}]
EOF
}
