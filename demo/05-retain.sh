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
note "claimed $KEEPIP from the Retain class ($KEEPADDR)."

kubectl -n "$NS" delete "$IPC" keeper >/dev/null
wait_for 60 sh -c "[ \"\$(kubectl get $IP $KEEPADDR -o jsonpath='{.status.phase}' 2>/dev/null)\" = Released ] && echo ok" >/dev/null \
  || { echo "address never went Released"; exit 1; }

note "the claim is gone — but under Retain the address is only Released, never freed:"
try "kubectl get $IP $KEEPADDR   # phase Released, claimRef still recorded"
try "kubectl -n $IAD_NS get svc     # the reservation is STILL held against MetalLB"
note "nobody can be handed $KEEPIP by accident, exactly like a Released PV."

note "an admin decides it may be reused, by clearing the claimRef:"
plain "kubectl patch $IP $KEEPADDR --type=merge -p '{\"spec\":{\"claimRef\":null}}'"
kubectl patch "$IP" "$KEEPADDR" --type=merge -p '{"spec":{"claimRef":null}}' >/dev/null

kubectl apply -f - >/dev/null <<EOF
apiVersion: local.sdn.cozystack.io/v1alpha1
kind: IPAddressClaim
metadata: {name: keeper-2, namespace: $NS}
spec: {family: IPv4, className: $RETAIN_CLASS, addressName: $KEEPADDR}
EOF
wait_bound keeper-2

note "a new claim pinned the now-Available address by name — same IP, new owner:"
plain "keeper-2  Bound  $(claimip keeper-2)"
try "kubectl get $IP $KEEPADDR"
note "allocation, release, adoption — the whole PV lifecycle, for an IP address."
