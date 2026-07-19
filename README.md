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

Four controllers implement the driver side of the contract:

- **Claim** — watches `IPAddressClaim`s stamped with this driver's name in
  the `local.sdn.cozystack.io/provisioner` annotation and provisions an
  `IPAddress` per missing family (first-free from the class CIDRs, skipping
  IPv4 network/broadcast and anything already in the ledger). Addresses are
  pre-bound via `claimRef`, carry the class's reclaim policy, a
  `source.fromClass` marker, and the driver's teardown finalizer. MetalLB is
  a self-allocating backend with no reservation concept, so this driver is
  the IPAM of record: it implements **Allocate + Pin**, not Adopt.
- **Class** — renders the MetalLB configuration per served class: one
  `IPAddressPool` (`iad-<class>`) with `autoAssign: false` covering the
  class ranges, and one `L2Advertisement` selecting it, owner-referenced to
  the class. That is the entire per-cluster MetalLB setup — no pool per
  tenant, no `/32`s.
- **Service** — the association layer, a separate and reversible act. A
  tenant annotates a Service with
  `local.sdn.cozystack.io/ip-address-claim: <claim>` (same namespace, by
  construction); the driver resolves claim → bound addresses, writes
  MetalLB's `metallb.io/loadBalancerIPs` pin, and records
  `IPAddress.status.associatedTo`. A second Service referencing an
  associated claim is rejected loudly. Removing the annotation withdraws
  the pin — the address stays bound: reserved, attached to nothing. The
  driver only ever removes pins it wrote itself (tracked with a marker
  annotation), never hand-written ones.
- **IPAddress** — teardown and conflict recovery. On deletion it withdraws
  a live pin and drops the finalizer (MetalLB has no per-address backend
  object to deallocate). It also reconciles live Service assignments
  against the ledger: a Service holding an address whose binding does not
  authorize it drives the `IPAddress` to phase `Conflict` (detection, per
  the design's layer 2 — never silent theft), and clears it once the
  wrongful holder is gone.

## Building

The address-controller dependency is a private repo, so:

```sh
GOPRIVATE=github.com/lllamnyp go build ./...
go test ./internal/...
```

Run with `--metallb-namespace` if MetalLB does not live in `metallb-system`.

## Not implemented (deliberately, for now)

- The `ValidatingAdmissionPolicy` that forbids tenants writing raw MetalLB
  pool/pin annotations (design §Security layer 1). It belongs in the same
  release as any real deployment of this driver, but is deployment
  configuration, not controller code.
- Backwards inventory reconciliation (design §6): importing addresses an
  eager MetalLB handed to plain Services into `IPAddress` objects after the
  fact.
- MetalLB range syntax beyond CIDRs (e.g. `192.168.9.1-192.168.9.5`) in
  class parameters, and BGP advertisement rendering (L2 only).
