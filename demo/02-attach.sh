#!/usr/bin/env bash
# Act 2 — attach the claimed address to a workload: annotate, don't allocate.
cd "$(dirname "$0")" && . ./lib.sh

banner "Act 2: attach the address to a Service"

WEBIP=$(claimip web)

kubectl apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: nginx
          image: nginxinc/nginx-unprivileged:alpine
          ports: [{containerPort: 8080}]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
EOF
lb_service web

note "one annotation on the Service is the whole tenant API:"
plain "local.sdn.cozystack.io/ip-address-claim: web"

wait_for 60 sh -c "[ \"\$(kubectl -n $NS get svc web -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)\" = '$WEBIP' ] && echo ok" >/dev/null \
  || { echo "service never got $WEBIP"; exit 1; }

note "the Service got exactly the claimed address — look at who wrote the pin:"
try "kubectl -n $NS get svc web -o jsonpath='{.metadata.annotations}' | jq"
try "kubectl -n $NS get events --field-selector involvedObject.name=web | tail"

note "the placeholder is gone — the workload holds the address in MetalLB's books now:"
try "kubectl -n $IAD_NS get svc"
try "kubectl get $IP   # ATTACHEDTO: web"

note "and it serves (from inside the cluster; the demo range isn't routed outside):"
try "kubectl -n $NS exec deploy/web -- wget -qO- http://$WEBIP | head -4"
