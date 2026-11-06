# metallb-iad

MetalLB IP Allocation Driver — the reference per-class driver for the
[address-controller](https://github.com/cozystack/address-controller) core
(IP addresses as a first-class resource,
[cozystack/community#35](https://github.com/cozystack/community/pull/35)).

Like a CSI driver for a storage provider, this is the driver for one address
backend: it reserves and attaches IPs that are ultimately announced by
MetalLB. The core controller owns the generic claim–address lifecycle; this
driver owns everything MetalLB-shaped.

**The high-level algorithms — provisioning, the backend reference, association,
conflict handling — are recorded in [docs/design.md](docs/design.md)**,
written against the core's contract
([address-controller docs/design.md](https://github.com/cozystack/address-controller/blob/feat/core-controller/docs/design.md)).

## What it does

An `IPAddressClass` opts into this driver by naming it:

```yaml
apiVersion: local.sdn.cozystack.io/v1alpha1
kind: IPAddressClass
metadata:
  name: public
spec:
  provisioner: metallb.drivers.local.sdn.cozystack.io
  reclaimPolicy: Retain
  parameters:
    pool: public    # an existing MetalLB IPAddressPool with autoAssign: false
```

The class names MetalLB configuration and does not restate it: the admin
creates the pool and its advertisement in MetalLB's own terms, and the driver
only reads them.
A pool that sets `serviceAllocation` must admit the placeholder namespace and
every namespace whose Services name claims of the class; the driver does not
check this yet.

MetalLB has no native reservation concept, so the driver manufactures
one: **a reservation is held by a placeholder Service** — selectorless,
`type: LoadBalancer`, living in a driver-owned namespace — which MetalLB
assigns an address to but, having no endpoints, never announces. Held in
MetalLB's own books, silent on the wire.

Four controllers implement the driver side of the contract:

- **Claim** — for each family a stamped claim misses: create an
  allocation placeholder drawing from the class's pool, let MetalLB (the
  allocator of record) assign, observe the address, and record it as a
  pre-bound `IPAddress` — with the class's reclaim policy, a
  `source.fromClass` marker, and the driver's teardown finalizer. The
  driver implements **Allocate + Pin**, not Adopt.
- **Class** — checks each served class against the pool it names and
  reports, as `PoolRefused` events, a pool that is missing, auto-assigns,
  or is named by another class. Claims of a refused class get no new
  reservations.
- **Service** — the association layer, a separate and reversible act. A
  tenant annotates a Service with
  `local.sdn.cozystack.io/ip-address-claim: <claim>` (same namespace, by
  construction); the driver performs the **gap handoff**: record the
  association, release the hold placeholder, pin the workload with
  MetalLB's `metallb.io/loadBalancerIPs`. A second Service referencing an
  associated claim is rejected loudly. Removing the annotation withdraws
  the pin (only pins the driver wrote itself, never hand-written ones) and
  the hold placeholder comes back — the address stays bound: reserved,
  attached to nothing.
- **IPAddress** — enforces the holding invariant (a placeholder pinned to
  the address exists exactly while the address exists and is not
  associated to a live workload), clears associations whose Service is
  gone, tears down on deletion (delete the placeholder — releasing the
  MetalLB allocation — and withdraw any pin), and recovers `Conflict`
  once no Service wrongfully holds the address.

## Installing

Packaged as a Helm chart at [`chart/metallb-iad`](chart/metallb-iad) (no
kustomize). The CRDs belong to the
[address-controller](https://github.com/cozystack/address-controller) chart —
install that first. The driver's ClusterRole
(`chart/metallb-iad/templates/role.yaml`) is controller-gen output written by
`make manifests`, never edited by hand.

```sh
helm upgrade --install metallb-iad chart/metallb-iad \
  --namespace metallb-iad-system --create-namespace
```

Set `metallbNamespace` if MetalLB does not live in `metallb-system`, and
`loadBalancerClass` to the `--lb-class` of the MetalLB instance the driver
fronts if that instance is not the cluster default.
Reservation placeholders live in the release namespace by default; set
`placeholderNamespace` to put them elsewhere (the chart then creates that
namespace). Tenants must have no write access to it — a Service carrying the
placeholder label is exempt from conflict detection only inside that
namespace.

The driver image is published as `ghcr.io/cozystack/metallb-iad:main` (plus
`main-<sha>` and semver tags) by the release workflow on every push to main.

### Reserving part of an existing pool

A class cannot name a pool with `autoAssign: true`, and MetalLB rejects
pools whose ranges overlap. To reserve part of a range that a shared pool
auto-assigns today, split it:

1. Shrink the shared pool so it no longer covers the part to reserve.
   MetalLB withdraws any assignment that falls outside its pool, so first
   check `kubectl get svc -A` for Services holding addresses in that part.
   Leave those addresses in the shared pool, or adopt them first: create
   an `IPAddress` pre-bound to a claim (`spec.claimRef`) and name the claim
   on the Service.
2. Create a pool for the reserved part with `autoAssign: false`, and an
   `L2Advertisement` or `BGPAdvertisement` selecting it.
3. Create the class with `parameters.pool` naming the new pool.

### Migrating classes that listed addresses

Earlier releases took `parameters.addresses` and rendered an
`IPAddressPool` and `L2Advertisement` named `iad-<class>`, owned by the
class. This release refuses that format. Keep the rendered objects as the
admin's and point the class at them:

```sh
kubectl -n <metallb-namespace> patch ipaddresspool iad-<class> --type=json \
  -p '[{"op":"remove","path":"/metadata/ownerReferences"}]'
kubectl -n <metallb-namespace> patch l2advertisement iad-<class> --type=json \
  -p '[{"op":"remove","path":"/metadata/ownerReferences"}]'
kubectl patch ipaddressclass <class> --type=merge \
  -p '{"spec":{"parameters":{"addresses":null,"pool":"iad-<class>"}}}'
```

Remove the owner references first: otherwise deleting the class later
deletes the pool and withdraws its addresses.

## Building

```sh
make build                      # vet + build the driver binary
go test ./internal/...
make docker-build docker-push   # publish ghcr.io/cozystack/metallb-iad:<git-sha>
make helm-package               # lint and package the chart into dist/
```

## Not implemented (deliberately, for now)

- The **shared handoff**: closing the brief unheld window during
  (dis)association with a sequence of `metallb.io/allow-shared-ip`
  Service mutations, where the workload's shape permits it. Planned as an
  opportunistic upgrade over the gap handoff (design doc §5).
- The `ValidatingAdmissionPolicy` that forbids tenants writing raw MetalLB
  pool/pin annotations (design §Security layer 1). It belongs in the same
  release as any real deployment of this driver, but is deployment
  configuration, not controller code.
- Backwards inventory reconciliation (design §6): importing addresses an
  eager MetalLB handed to plain Services into `IPAddress` objects after the
  fact.
