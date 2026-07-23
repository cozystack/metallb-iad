#!/usr/bin/env bash
# Act 5 — Retain: the PV parallel, end to end.
cd "$(dirname "$0")" && . ./lib.sh

banner "Act 5: reclaimPolicy Retain and re-adoption"

kubectl apply -f - >/dev/null <<EOF
apiVersion: local.sdn.cozystack.io/v1alpha1
kind: IPAddressClass
metadata: {name: $RETAIN_CLASS}
spec:
  provisioner: metallb.drivers.local.sdn.cozystack.io
  reclaimPolicy: Retain
  parameters:
    addresses: ["$RETAIN_RANGE"]
---
apiVersion: local.sdn.cozystack.io/v1alpha1
kind: IPAddressClaim
metadata: {name: keeper, namespace: $NS}
spec: {family: IPv4, className: $RETAIN_CLASS}
EOF

wait_bound keeper
KEEPIP=$(claimip keeper); KEEPADDR=$(addrname keeper)
note "a second class, reclaimPolicy Retain — and a claim, Bound as before:"
show "kubectl -n $NS get $IPC"
show "kubectl get $IP $KEEPADDR"
pause "delete the claim (this is the Retain moment)"

kubectl -n "$NS" delete "$IPC" keeper >/dev/null
wait_for 60 sh -c "[ \"\$(kubectl get $IP $KEEPADDR -o jsonpath='{.status.phase}' 2>/dev/null)\" = Released ] && echo ok" >/dev/null \
  || { echo "address never went Released"; exit 1; }

note "the claim object is gone:"
show "kubectl -n $NS get $IPC"
note "but under Retain the address is only Released, never freed — the ledger entry"
note "still names its dead claim, and the reservation is still held against MetalLB:"
show "kubectl get $IP $KEEPADDR"
show "kubectl -n $IAD_NS get svc"
note "nobody can be handed $KEEPIP by accident, exactly like a Released PV."
pause "act as the admin: clear the claimRef to allow reuse"

kubectl patch "$IP" "$KEEPADDR" --type=merge -p '{"spec":{"claimRef":null}}' >/dev/null
wait_for 60 sh -c "[ \"\$(kubectl get $IP $KEEPADDR -o jsonpath='{.status.phase}' 2>/dev/null)\" = Available ] && echo ok" >/dev/null \
  || { echo "address never went Available"; exit 1; }

note "claimRef cleared — Available now, and STILL held (no re-allocation race):"
show "kubectl get $IP $KEEPADDR"
pause "create a new claim in the same class — NO addressName, no pinning"

kubectl apply -f - >/dev/null <<EOF
apiVersion: local.sdn.cozystack.io/v1alpha1
kind: IPAddressClaim
metadata: {name: keeper-2, namespace: $NS}
spec: {family: IPv4, className: $RETAIN_CLASS}
EOF
wait_bound keeper-2
[ "$(claimip keeper-2)" = "$KEEPIP" ] \
  || { echo "keeper-2 got $(claimip keeper-2), not the Available $KEEPIP — binding raced provisioning"; exit 1; }

note "bind-before-provision: the core matched the existing Available address by"
note "class and family BEFORE the driver would allocate a fresh one — why mint a"
note "new IP when one is sitting there? Same IP, new owner:"
show "kubectl -n $NS get $IPC"
show "kubectl get $IP $KEEPADDR"
note "if that address had been waiting for something specific, the safeguards are:"
note "a claim can pin exactly one address by name (spec.addressName), and an admin"
note "who wants a Released address kept out of circulation simply leaves the dead"
note "claimRef in place — clearing it is precisely the 'free to reuse' signal."
note "allocation, release, adoption — the whole PV lifecycle, for an IP address."
