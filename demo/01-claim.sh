#!/usr/bin/env bash
# Act 1 — a claim becomes a reservation in MetalLB's own books.
cd "$(dirname "$0")" && . ./lib.sh

banner "Act 1: claim an IP — nothing is running yet"

kubectl apply -f - >/dev/null <<EOF
apiVersion: local.sdn.cozystack.io/v1alpha1
kind: IPAddressClass
metadata:
  name: $CLASS
  annotations: {ipaddressclass.local.sdn.cozystack.io/is-default-class: "true"}
spec:
  provisioner: metallb.drivers.local.sdn.cozystack.io
  reclaimPolicy: Delete
  parameters:
    addresses: ["$RANGE"]
EOF

note "one class = one MetalLB pool (autoAssign: false — nobody draws from it by accident):"
try "kubectl -n $METALLB_NS get ipaddresspools,l2advertisements"

kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: local.sdn.cozystack.io/v1alpha1
kind: IPAddressClaim
metadata: {name: web, namespace: $NS}
spec: {family: IPv4}
EOF

note "the claim names no class — the default-class annotation resolves it."
wait_bound web
WEBIP=$(claimip web)

note "what exists now:"
plain "claim  $NS/web        Bound   $WEBIP"
plain "ledger $(addrname web)   Bound to $NS/web, reclaim Delete"
try "kubectl -n $NS get $IPC"
try "kubectl get $IP"

note "the reservation is REAL in the backend — MetalLB assigned it to a placeholder:"
try "kubectl -n $IAD_NS get svc"
note "selectorless, no endpoints: held in MetalLB's books, silent on the wire."
note "no pod, no Service of yours, no traffic — and $WEBIP can already go into DNS."
