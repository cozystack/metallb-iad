# Design: MetalLB IP Allocation Driver (metallb-iad)

- **Component:** the reference per-class driver for
  [address-controller](https://github.com/lllamnyp/address-controller)
  (contract:
  [docs/design.md](https://github.com/lllamnyp/address-controller/blob/feat/core-controller/docs/design.md)
  in that repo; original proposal:
  [cozystack/community#35](https://github.com/cozystack/community/pull/35))
- **Provisioner name:** `metallb.drivers.local.sdn.cozystack.io`
- **Status:** implemented (alpha)

This document records how the driver discharges each obligation of the
core's contract, at the level of the algorithms rather than the code. The
core doc defines *what* a driver must do; this one defines *how this
driver does it* against MetalLB.

## 1. Positioning and capability profile

MetalLB is a **self-allocating backend with no reservation concept**:
nothing on the MetalLB side can hold an address for later. Two design
consequences:

- **The driver is the IPAM of record.** The set of `IPAddress` objects
  *is* the allocation state; there is no second ledger to reconcile
  against, and every address this driver creates carries
  `source.fromClass`. In the proposal's capability terms the driver
  implements **Allocate + Pin, not Adopt** — `providerRef` never appears
  here (that is the cloud-driver profile).
- **Reservation is a two-sided discipline, not a MetalLB feature.** The
  driver keeps MetalLB away from the reserved range (`autoAssign: false`,
  §3) and steers it onto specific addresses only through the pin
  annotation (§4). MetalLB itself remains entirely unaware that
  reservations exist.

The driver is four independent reconciliation loops — provisioning (§2),
backend rendering (§3), association (§4), and teardown/recovery (§5) —
each idempotent and each re-derivable from cluster state alone. The
driver holds no state of its own.

## 2. Provisioning (contract obligations 1–2)

**Trigger.** A live `IPAddressClaim` stamped with this driver's name in
the `local.sdn.cozystack.io/provisioner` annotation, whose class exists
and names this driver. Claims with `spec.addressName` set are skipped:
pre-binding to a specific existing address is the core's matching path,
not a provisioning request.

**Class parameters.** The driver defines the shape of the class's opaque
`parameters` as a list of CIDRs:

```yaml
parameters:
  addresses: ["203.0.113.0/24", "2001:db8::/64"]
```

Malformed parameters produce a warning event on the claim and no
provisioning — a misconfigured class must fail loudly, not allocate
garbage.

**Algorithm**, per reconcile of a triggering claim:

1. Scan the `IPAddress` ledger once, deriving two facts:
   - the **in-use set**: every IP represented by any `IPAddress` object,
     *regardless of class* — one IP must never be represented by two
     objects, so class boundaries do not partition the in-use check;
   - the **bound families**: which of this claim's requested families
     already have an address bound (`claimRef` names the claim, UID empty
     or matching, not deleting).
2. For each requested family still missing (`Dual` expands to v4 + v6),
   allocate **first-free**: walk the class CIDRs in their declared order,
   ascending within each CIDR, skipping IPv4 network and broadcast
   addresses (for prefixes wider than /31) and everything in the in-use
   set. The first free address wins.
3. Create the `IPAddress`: name derived deterministically from the IP
   (`ip-203-0-113-7`), `claimRef` pre-set with the claim's UID, reclaim
   policy copied from the class, `source.fromClass`, and the driver's
   teardown finalizer. Binding completion and claim status are the core's
   job — the driver never writes claim status.

**Why this is safe without an allocator lock.** Allocation is a pure
function of (class parameters, ledger); the object name is a pure function
of the IP. Two racing allocations of the same IP collide on the object
name, the API server rejects the second create, and the loser re-runs
against a ledger that now contains the winner. The create is the lock.

**Exhaustion** is surfaced as a warning event on the claim (the core
keeps it honestly `Pending` with `WaitingForProvisioning`). Retry is
event-driven — any ledger change requeues pending claims of this driver,
so freeing one address immediately unblocks the longest-waiting claim —
with a slow periodic retry as a safety net.

## 3. Backend rendering (per class)

For each `IPAddressClass` naming this driver, render exactly two MetalLB
objects, both owner-referenced to the class so garbage collection removes
them with it:

- an `IPAddressPool` (`iad-<class>`) covering the class CIDRs, with
  **`autoAssign: false`** — MetalLB must never *automatically* hand a
  reserved address to a plain LoadBalancer Service; the pool is reachable
  only via explicit pinning;
- an `L2Advertisement` selecting that pool — advertisement selects pools,
  not addresses, so one object covers every address the class will ever
  hold.

This is the proposal's §7 shape verbatim: one pool and one advertisement
per address source, for the life of the cluster. No pool per tenant, no
/32s, and the admin configures nothing per address.

`autoAssign: false` is a steering measure, not a security boundary: a
Service can still name the pool or pin an address explicitly. Closing
that path belongs to admission (§6).

## 4. Association (contract obligation 5)

Association is the separate, reversible act the model exists for. The
tenant-facing surface is one annotation on a Service:

```yaml
metadata:
  annotations:
    local.sdn.cozystack.io/ip-address-claim: web   # a claim in THIS namespace
```

**Forward algorithm** (annotation present):

1. Resolve the claim — in the Service's own namespace, by construction;
   cross-namespace sharing is unrepresentable. The claim must be stamped
   with this driver's name (another driver's claims are ignored) and be
   `Bound` (otherwise wait; binding progress requeues the Service).
2. **Enforce 1:1.** Inspect `status.associatedTo` on every address the
   claim reports. Each must be unassociated, already associated to this
   Service, or associated to a holder that **no longer exists** — a stale
   record whose deletion event was missed must not wedge a cutover. A
   *live* different holder rejects the association, loudly, with no
   partial pin: silently letting the second Service win is how an address
   goes quietly dead.
3. **Pin.** Write MetalLB's `metallb.io/loadBalancerIPs` with the claim's
   addresses (v4 before v6, matching dual-stack expectations), plus a
   driver-owned marker annotation recording that *this driver* wrote the
   pin. The marker is the authorship boundary: the driver will later
   remove only pins it marked, never a hand-written or GitOps-managed
   pin.
4. Record `status.associatedTo` on each bound address.

MetalLB then allocates exactly the pinned address from the
`autoAssign: false` pool and announces it. The driver never touches
announcement.

**Reverse algorithm** (annotation removed, or Service deleted): if the
marker annotation is present, remove the pin and the marker, and clear
`associatedTo` on the addresses that named this Service. The address
**stays `Bound` to its claim** — reserved, attached to nothing, which is
precisely what an unassociated elastic IP is. Releasing the address is
only ever done by deleting the *claim* (core reclaim flow).

**The flagship flow** — the integration test that is the proposal —
follows directly: claim → bound address → annotate Service A → pinned →
delete Service A → pin gone, address still `Bound` → annotate Service B →
the *same* address comes back on B. Step 2's stale-holder rule is what
makes the cutover robust even if the driver never observed A's deletion.

## 5. Conflict detection, recovery, and teardown (obligations 4, 6)

**Detection** (the proposal's "layer 2"). On every Service change, each
IP in the Service's live `status.loadBalancer.ingress` is looked up in
the ledger:

- ledger hit, and the address's binding does not authorize this Service
  (`associatedTo` is unset or names someone else) → the address is driven
  to phase `Conflict`, with events on both the address (naming the
  offender) and the Service (naming the rightful claim). The reservation
  is never silently rebound — a collision is a *visible fault*, whatever
  caused it: an explicit pool/pin annotation, a race, or a write under an
  impersonated identity.
- no ledger hit → a plain Service got an address from some other pool
  (the ordinary eager-allocator case). Not a conflict, not this driver's
  business.

**Recovery.** `Conflict` is driver-owned and sticky for the core, so the
driver also clears it: whenever Services change, conflicted addresses are
rechecked, and once no live Service wrongfully holds the address, the
phase is handed back to core bookkeeping (`Bound` if the binding stands,
`Available` if not). No operator action is needed beyond removing the
offending Service or its assignment.

**Teardown.** Deletion of a driver-created `IPAddress` passes through the
driver's finalizer: withdraw the pin from the associated Service if the
driver wrote one, then release the finalizer. There is deliberately
nothing else — MetalLB has no per-address backend object; the pool is
class-level and the ledger entry *was* the reservation. The finalizer
still exists because the contract requires teardown to be a driver
concern, and because a live pin left behind would keep MetalLB announcing
an address whose reservation is gone.

## 6. Security posture

The driver writes MetalLB's raw pin annotation with its own identity.
The model assumes the proposal's admission layer around it: a
`ValidatingAdmissionPolicy` that forbids non-allowlisted principals from
writing any pool-selecting or address-pinning backend annotation, leaving
the claim-reference annotation as the only tenant-reachable path to the
reserved range. That policy is deployment configuration and ships with an
installation, not with this controller; without it the model degrades to
detection-only (§5), which is exactly the proposal's stated fallback for
principals admission cannot bind anyway (cluster-admins, impersonators).

## 7. Known gaps (acknowledged, deferred)

- **The pre-association theft window** on self-allocating backends: an
  eager allocator can hand a plain Service a reserved-but-inert address
  if explicitly steered there. Detected (§5), not prevented — prevention
  requires the admission policy (§6) plus, ultimately, allocation being
  refused driver-side. §8 records a design that closes most of this gap
  by making the reservation visible to MetalLB itself.
- **No backwards inventory** (proposal §6): addresses MetalLB assigned to
  plain Services are not imported into the ledger after the fact. A
  related sharper edge: the first-free allocator (§2) consults only the
  ledger, so it can pick an IP that MetalLB assigned to a plain Service
  outside the model's knowledge; the collision then surfaces as a failed
  pin or a Conflict rather than being avoided. §8 eliminates this class
  of error.
- **L2 only, CIDRs only**: BGP advertisement rendering and MetalLB's
  `a.b.c.d-e.f.g.h` range syntax in class parameters are future
  parameters, not shapes the algorithm depends on.
- **Dual-stack pinning** assumes the Service's `ipFamilies` accept both
  pinned addresses; the driver orders v4 first but does not reconcile
  family policy mismatches.

## 8. Design alternative: holding reservations with placeholder Services

*Status: analyzed, judged superior for allocation and holding, not yet
implemented. Recorded here as the intended direction.*

The implemented driver keeps the reservation only in the ledger: MetalLB
has no idea a reserved-but-inert address is taken, which is why the whole
§6 posture leans on `autoAssign: false` plus admission, with §5 as the
backstop. The alternative: **the driver reserves an address by creating a
placeholder Service and letting MetalLB allocate to it** — manufacturing
the reservation primitive MetalLB lacks out of MetalLB's own allocation
unit.

**Reserve.** For each family a claim misses, the driver creates a
placeholder Service in its own namespace: `type: LoadBalancer`, no
selector (so no endpoints), one throwaway port,
`allocateLoadBalancerNodePorts: false`, drawing from the class pool by
explicit pool selection. MetalLB — the allocator of record — assigns an
IP; the driver observes it and only then creates the `IPAddress`,
recording the observed fact (the same allocate–observe–record flow a
cloud driver uses, with the placeholder playing the role of the provider
reservation object and its name the stable handle). A `Dual` claim uses
one dual-stack placeholder. The placeholder is owner-referenced to the
`IPAddress` and finalizer-protected; a recreate-and-repin loop restores
it if deleted, with theft during the outage surfacing as `Lost`/`Conflict`.

**Hold.** With no endpoints MetalLB does not announce the address —
reserved but inert, exactly the wanted semantics — yet it *is* assigned
in MetalLB's books, so MetalLB itself refuses to give it to any other
Service, pinned or not. The reservation is enforced by the backend, not
just recorded beside it. Under `Retain`, a `Released` address stays held
against MetalLB too. Teardown becomes real work at last: delete the
placeholder, releasing the allocation.

**What this buys** over §2's first-free allocator:

1. The pre-association theft window (§7) mostly closes without admission:
   MetalLB will not double-assign a held address.
2. No parallel IPAM: allocation consults MetalLB's live state, so the
   driver can never pick an IP some plain Service already holds — the
   split-brain class of errors disappears, along with the driver's
   reimplementation of range walking (dash syntax and avoid-lists come
   free).
3. Model consistency: MetalLB stops being a reservation-less backend and
   becomes a provider with a reservation object; `Retain`/`Delete` map
   onto keep/delete the placeholder.

**The cost concentrates in the handoff** — moving the address between
placeholder and real Service at (dis)association time:

- *Shared handoff* (both Services carry `metallb.io/allow-shared-ip` with
  a per-claim key; pin the real Service while the placeholder still
  holds; then release the placeholder) is gapless, but MetalLB's sharing
  rules require non-overlapping ports and `externalTrafficPolicy:
  Cluster` — which forbids source-IP preservation and collides with the
  whole-IP/1:1-NAT direction. Unacceptable as a universal requirement.
- *Gap handoff* (unpin the placeholder, then pin the real Service, and
  the mirror image on disassociation) opens a window in which the
  address is unheld in MetalLB's books and the protection degrades to
  exactly the implemented model: `autoAssign: false` + admission +
  detection. The window is bounded by reconcile latency and exists only
  during handoffs — versus permanently, today.

**Verdict.** Adopt the placeholder mechanism for *allocation and
holding*; keep the ledger authoritative and the association algorithm
(§4) as is; use gap handoff unconditionally, with shared handoff as a
best-effort optimization when the tenant Service happens to satisfy the
sharing constraints. Everything else in this document — the association
1:1 rules, conflict detection, reclaim semantics — is unchanged by the
switch; only §2's allocation step and §5's teardown step are replaced.
