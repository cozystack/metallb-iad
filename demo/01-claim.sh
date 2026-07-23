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
show "kubectl -n $METALLB_NS get ipaddresspools,l2advertisements"

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

note "how the reservation was made: the driver never picks an address itself."
note "it created a placeholder Service in $IAD_NS drawing from the class pool,"
note "MetalLB — the allocator of record — assigned one, and the driver recorded"
note "the observed result as a pre-bound ledger entry. Look at all three:"
show "kubectl -n $NS get $IPC"
show "kubectl get $IP"
show "kubectl -n $IAD_NS get svc"
note "the placeholder is selectorless with no endpoints: the address is assigned"
note "in MetalLB's own books — a backend-enforced reservation — but silent on the wire."
note "no pod, no Service of yours, no traffic — and $WEBIP can already go into DNS."
