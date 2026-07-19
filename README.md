# metallb-iad

MetalLB IP Allocation Driver — the reference per-class driver for the
[address-controller](https://github.com/lllamnyp/address-controller) core
(IP addresses as a first-class resource,
[cozystack/community#35](https://github.com/cozystack/community/pull/35)).

Like a CSI driver for a storage provider, this is the driver for one address
backend: it reserves and attaches IPs that are ultimately announced by
MetalLB. The core controller owns the generic claim–address lifecycle; this
driver owns everything MetalLB-shaped.

**The high-level algorithms — provisioning, backend rendering, association,
conflict handling — are recorded in [docs/design.md](docs/design.md)**,
written against the core's contract
([address-controller docs/design.md](https://github.com/lllamnyp/address-controller/blob/feat/core-controller/docs/design.md)).

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
    addresses: ["203.0.113.0/24"]   # CIDRs this class carves from
```

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
- **Class** — renders the MetalLB configuration per served class: one
  `IPAddressPool` (`iad-<class>`) with `autoAssign: false` covering the
  class ranges, and one `L2Advertisement` selecting it, owner-referenced to
  the class. That is the entire per-cluster MetalLB setup — no pool per
  tenant, no `/32`s.
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

## Building

The address-controller dependency is a private repo, so:

```sh
GOPRIVATE=github.com/lllamnyp go build ./...
go test ./internal/...
```

Run with `--metallb-namespace` if MetalLB does not live in
`metallb-system`, and `--placeholder-namespace` to choose where
reservation placeholders live (default `metallb-iad-system`; tenants must
have no write access to it).

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
- MetalLB range syntax beyond CIDRs (e.g. `192.168.9.1-192.168.9.5`) in
  class parameters, and BGP advertisement rendering (L2 only).
