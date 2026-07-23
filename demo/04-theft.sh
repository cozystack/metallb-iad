#!/usr/bin/env bash
# Act 4 — reservations are enforced by the backend, not by good manners.
cd "$(dirname "$0")" && . ./lib.sh

banner "Act 4: try to steal the address"

WEBIP=$(claimip web)

note "a thief hand-writes MetalLB's own pin annotation, bypassing claims entirely:"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: thief
  namespace: $NS
  annotations: {metallb.io/loadBalancerIPs: "$WEBIP"}
spec:
  type: LoadBalancer
  selector: {app: web}
  ports: [{name: http, port: 80, targetPort: 8080, protocol: TCP}]
EOF

sleep 5
[ -z "$(kubectl -n "$NS" get svc thief -o jsonpath='{.status.loadBalancer.ingress}')" ] \
  || { echo "the thief GOT the address — reservation broken"; exit 1; }
note "MetalLB itself refuses — the address is already assigned in its books,"
note "and the refusal names the current holder:"
show "kubectl -n $NS get svc thief"
show "kubectl -n $NS get events --field-selector involvedObject.name=thief | tail -3"

note "that's the point of placeholder-held reservations: in steady state the guarantee"
note "is MetalLB's own allocator, not an admission webhook that might be missing."
note "(and if an address IS ever wrongfully held, the ledger flags it: phase Conflict,"
note "events on both the address and the offender — loud, never a silent rebinding.)"
pause "delete the thief and move on"

kubectl -n "$NS" delete svc thief >/dev/null
note "thief deleted."
