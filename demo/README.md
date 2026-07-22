# metallb-iad live demo

Five acts, one script per act, run in order. Every script acts on **the
cluster kubectl currently points at** (`KUBECONFIG` + current-context) and
prints presenter notes: what exists, the exact commands for *you* to type
(highlighted in yellow), and the one-liner takeaways to say out loud.

| Act | Script | Shows |
|-----|--------|-------|
| 1 | `01-claim.sh` | A class renders a MetalLB pool; a claim (default-class resolution) becomes a **backend-enforced reservation**: MetalLB assigns the address to a placeholder Service, the `IPAddress` ledger entry goes `Bound` — with no workload anywhere. |
| 2 | `02-attach.sh` | One annotation attaches the address to a real Service: the gap handoff pins the workload to exactly the claimed IP, the placeholder retires, nginx serves on it. |
| 3 | `03-lifecycle.sh` | The flagship: **delete the Service** — the address stays Bound, the hold comes back; a *new* Service with the same annotation gets the *same* IP. Workloads are cattle, the address is infrastructure. |
| 4 | `04-theft.sh` | A thief hand-writes MetalLB's own pin annotation — and MetalLB itself refuses, because the address is assigned in its books. The reservation is not an admission-webhook promise. |
| 5 | `05-retain.sh` | `reclaimPolicy: Retain`: delete the claim → `Released`, reservation still held; admin clears `claimRef` → a new claim adopts the same address by name. The whole PV lifecycle, for an IP. |
| — | `99-teardown.sh` | Deletes everything the demo created, including Retain leftovers. |

## Prerequisites

- The [address-controller](https://github.com/lllamnyp/address-controller) and
  metallb-iad charts installed; MetalLB running (`METALLB_NS`, default
  `cozy-metallb`; driver namespace `IAD_NS`, default `cozy-metallb-iad`).
- The demo ranges `10.242.42.0/24` / `10.242.43.0/24` need not be routed
  anywhere — everything is shown from inside the cluster (act 2's curl runs
  in a pod). Override them in `lib.sh` if they collide with something real.
- First run of act 2's curl pulls `nicolaka/netshoot` — do a dry run before
  the audience so images are warm.

## Notes

- The scripts are idempotent (`kubectl apply`); re-running an act is safe.
- `lib.sh` uses fully-qualified resource names throughout: on a modern
  cluster bare `ipaddresses` is a three-way ambiguity (the stock
  `networking.k8s.io` ClusterIP ledger, CAPI's `ipam.cluster.x-k8s.io`, and
  `local.sdn.cozystack.io`) — and the stock group wins.
- Act 1's claim names no class: it exercises the default-class annotation.
  The demo marks its own class as the cluster default; don't run it on a
  cluster that already has one.
- Names to keep in your head: namespace `demo-iad`, classes `demo`
  (10.242.42.0/24, Delete) / `demo-retain` (10.242.43.0/24, Retain),
  claims `web`, `keeper`, `keeper-2`, Services `web`, `web2`, `thief`.
