#!/usr/bin/env bash
# Act 3 — the flagship: the address outlives the workload.
cd "$(dirname "$0")" && . ./lib.sh

banner "Act 3: delete the Service; the address survives"

WEBIP=$(claimip web)

kubectl -n "$NS" delete svc web >/dev/null
note "the Service that announced $WEBIP is GONE."

wait_for 60 kubectl -n "$IAD_NS" get svc \
  -l "metallb.drivers.local.sdn.cozystack.io/ip-address=$(addrname web)" -o name >/dev/null \
  || { echo "hold placeholder never came back"; exit 1; }

note "released, but not lost. deleting the holder freed the address in MetalLB's"
note "books — for a moment it belonged to nobody. The driver noticed the stale"
note "attachment, cleared it, and immediately re-manufactured the hold: a fresh"
note "placeholder pinned to $WEBIP. Back to a backend-enforced reservation:"
show "kubectl -n $NS get svc"
show "kubectl get $IP   # still Bound to its claim, ATTACHEDTO empty again"
show "kubectl -n $IAD_NS get svc"
note "deleting a workload NEVER releases the address — its lifetime belongs to the claim."
pause "bring up the successor Service (web2)"

lb_service web2
wait_for 60 sh -c "[ \"\$(kubectl -n $NS get svc web2 -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)\" = '$WEBIP' ] && echo ok" >/dev/null \
  || { echo "web2 never got $WEBIP"; exit 1; }

note "a NEW Service (web2) with the same annotation — and the SAME address comes back:"
show "kubectl -n $NS get svc web2"
show "kubectl get $IP"
note "this is what makes the IP DNS-safe: workloads are cattle, the address is infrastructure."
