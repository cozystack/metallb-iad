#!/usr/bin/env bash
# Tear the whole demo down. Safe to run at any point.
cd "$(dirname "$0")" && . ./lib.sh

banner "Teardown"

# Deleting the namespace deletes the claims; Delete-class addresses follow
# their claims, and the driver's finalizer releases each MetalLB allocation.
kubectl delete ns "$NS" --ignore-not-found --wait=true

# Retain-class addresses outlive their claims by design — delete the ledger
# entries explicitly (same finalizer path releases the reservations).
for a in $(kubectl get "$IP" -o jsonpath="{range .items[?(@.spec.className=='$RETAIN_CLASS')]}{.metadata.name}{'\n'}{end}"); do
  kubectl delete "$IP" "$a" --wait=true
done

kubectl delete "$IPCLASS" "$CLASS" "$RETAIN_CLASS" --ignore-not-found >/dev/null
note "classes gone; the MetalLB pools and advertisements follow by ownerRef."
note "nothing should remain — no placeholders, no ledger entries:"
show "kubectl -n $IAD_NS get svc"
show "kubectl get $IP"
