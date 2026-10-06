# Design: MetalLB IP Allocation Driver (metallb-iad)

- **Component:** the reference per-class driver for
  [address-controller](https://github.com/cozystack/address-controller)
  (contract:
  [docs/design.md](https://github.com/cozystack/address-controller/blob/feat/core-controller/docs/design.md)
  in that repo; original proposal:
  [cozystack/community#35](https://github.com/cozystack/community/pull/35))
- **Provisioner name:** `metallb.drivers.local.sdn.cozystack.io`
- **Status:** implemented (alpha)

This document records how the driver discharges each obligation of the
core's contract, at the level of the algorithms rather than the code. The
core doc defines *what* a driver must do; this one defines *how this
driver does it* against MetalLB.

## 1. Positioning: manufacturing the missing reservation primitive

MetalLB is a **self-allocating backend with no reservation concept**:
nothing on the MetalLB side can natively hold an address for later. This
driver's central move is to *manufacture* the missing primitive out of
MetalLB's own allocation unit — the Service:

> **A reservation is held by a placeholder Service**: a selectorless
> `type: LoadBalancer` Service in the driver's own namespace, which
> MetalLB assigns an address to but — having no endpoints — never
> announces. Assigned but silent: reserved and inert, in MetalLB's own
> books.

Consequences that shape everything below:

- **MetalLB is the allocator of record; the ledger is the record of
  reservations.** The driver never computes a free address itself — it
  asks MetalLB (by creating a placeholder) and records what it observes.
  The `IPAddress` object is created only after the allocation is a fact,
  which is exactly the allocate–observe–record flow a cloud driver uses;
  the placeholder plays the role of the provider-side reservation object.
- **The reservation is enforced by the backend, not just recorded beside
  it.** A held address is *assigned* in MetalLB's accounting, so MetalLB
  itself refuses to give it to any other Service, pinned or not. The
  admission layer (§8) remains defense-in-depth rather than the primary
  guarantee — except during handoff gaps (§5).
- In the proposal's capability terms the driver implements **Allocate +
  Pin, not Adopt**; every address it creates carries `source.fromClass`
  (the pool the placeholder draws from belongs to the class; no
  externally-owned reservation is involved).

The driver is four independent, idempotent reconciliation loops —
reservation (§2), backend rendering (§3), holding (§4), and association
(§5) — plus teardown and conflict recovery (§6–§7). The driver holds no
state outside the cluster.

## 2. Reservation (provisioning)

**Trigger.** A live `IPAddressClaim` stamped with this driver's name in
the `local.sdn.cozystack.io/provisioner` annotation, whose class exists
and names this driver. Claims with `spec.addressName` set are skipped:
pre-binding to a specific existing address is the core's matching path.

**Algorithm**, per family the claim misses (`Dual` expands to v4 + v6,
one single-stack placeholder per family):

1. **Reserve.** Ensure the family's *allocation placeholder* exists: a
   placeholder Service named deterministically from the claim UID and
   family (so racing reconciles collide on create instead of
   double-allocating), carrying the claim's identity in annotations and
   drawing from the class's pool by explicit pool selection
   (`metallb.io/address-pool` — the sanctioned way past
   `autoAssign: false`). `allocateLoadBalancerNodePorts: false` keeps it
   from consuming node ports.
2. **Observe.** Wait for MetalLB to assign (`status.loadBalancer
   .ingress`). Pool exhaustion simply leaves the placeholder unassigned —
   MetalLB reports why on the placeholder, the claim stays honestly
   `Pending` under the core's `WaitingForProvisioning`, and the watch
   retriggers when capacity frees.
3. **Record**, in three idempotent steps that re-converge after a crash
   between any two:
   1. link and pin the placeholder: label it with the future `IPAddress`
      name and pin it (`metallb.io/loadBalancerIPs`) to its own observed
      address, so a MetalLB restart can never reshuffle the reservation;
   2. create the `IPAddress`: name derived from the IP, `spec.address`
      the observed fact, `claimRef` pre-set with the claim's UID, reclaim
      policy copied from the class, `source.fromClass`, and the driver's
      teardown finalizer;
   3. hand the placeholder's lifecycle to the address (owner reference).
      From here on the placeholder is a *hold* placeholder (§4) and the
      claim's part is done — binding completion is the core's.

   If the `IPAddress` name already exists bound to someone else, MetalLB
   and the ledger disagree about the IP; the driver surfaces a
   `LedgerMismatch` warning and does not fight over it.

**Orphan cleanup.** An allocation placeholder whose claim disappears
before an `IPAddress` materializes would hold an address forever; when a
claim is deleted or gone, such placeholders are deleted. Placeholders
backed by an existing `IPAddress` are exempt — their lifecycle belongs to
the address, and under `Retain` they keep holding after the claim is
gone.

## 3. Backend rendering (per class)

For each `IPAddressClass` naming this driver, render exactly two MetalLB
objects, both owner-referenced to the class so garbage collection removes
them with it:

- an `IPAddressPool` (`iad-<class>`) covering the class CIDRs
  (`parameters.addresses`), with **`autoAssign: false`** — plain Services
  never draw from the reserved range automatically; placeholders reach it
  by explicit pool selection, and workloads only ever via pins the driver
  writes;
- an `L2Advertisement` selecting that pool — advertisement selects pools,
  not addresses, so one object covers every address the class will ever
  hold.

This is the proposal's §7 shape verbatim: one pool and one advertisement
per address source, for the life of the cluster. No pool per tenant, no
/32s, and the admin configures nothing per address.

## 4. Holding: the placeholder invariant

Between provisioning and teardown, placeholder existence is *derived
state*, enforced level-triggered on every address reconcile:

> A placeholder pinned to the address exists **iff** the `IPAddress`
> exists, belongs to a class this driver serves, and is not currently
> associated to a live workload.

Concretely:

- **Unassociated** (`status.associatedTo` nil — covers freshly
  provisioned, disassociated, `Released` under `Retain`, and
  admin-recycled addresses): ensure a placeholder pinned to
  `spec.address` exists. The allocation placeholder, once linked (§2),
  already satisfies this; after a disassociation a fresh *hold*
  placeholder (named from the address) is created. A placeholder deleted
  out-of-band is therefore recreated; if the address was stolen during
  the outage, the pin fails and the theft surfaces through conflict
  detection rather than silently.
- **Associated:** delete any placeholder linked to the address — a
  lingering one would fight the workload's Service for the pin.
- **Stale association:** if the associated Service no longer exists (its
  deletion event may have been missed), the driver clears
  `status.associatedTo` — which re-arms the hold on the same pass. This
  is the mechanism that makes "delete the workload, the address is still
  held" true in MetalLB's books and not just in the ledger.

## 5. Association: the gap handoff

Association remains a separate, reversible act driven by one tenant-facing
annotation on a Service (`local.sdn.cozystack.io/ip-address-claim`,
naming a claim in the Service's own namespace). The 1:1 rule, the
stale-holder takeover, the rejection of a second Service, and the
never-touch-hand-written-pins rule are unchanged. What changes with
placeholders is the *transfer of the hold*.

**Forward (associate)** — three steps, ordered so that every prefix of
the sequence re-converges after a crash:

1. **Record the association in the ledger** (`status.associatedTo` on
   each bound address). This is what disarms the holding invariant (§4),
   so the address controller will not re-arm the hold mid-handoff.
2. **Release the hold**: delete the placeholders linked to the bound
   addresses, freeing the address in MetalLB's books.
3. **Pin the real Service** (`metallb.io/loadBalancerIPs`, v4 before v6,
   plus the driver's authorship marker); MetalLB assigns the address to
   the workload and announces it.

Between steps 2 and 3 the address is briefly unheld — **the gap**. During
it the protection degrades to exactly the non-placeholder model:
`autoAssign: false`, admission (§8), and conflict detection (§6). The gap
is bounded by one reconcile pass plus MetalLB's processing, and exists
only during handoffs. A crash after step 1 or 2 is re-driven by the
address watch (the associated Service is requeued until the pin lands).

**Reverse (disassociate)** — the mirror image, ordered the same way: the
pin is withdrawn (only if the driver's marker shows it wrote it), then
`status.associatedTo` is cleared — and the cleared association re-arms
the holding invariant, which recreates a placeholder pinned to the
address. The address stays `Bound` to its claim throughout: reserved,
attached to nothing.

One MetalLB semantic makes this mirror asymmetric in practice (observed
on a live cluster): withdrawing the pin does *not* make MetalLB revoke
the Service's existing assignment — assignments are sticky, and
`autoAssign: false` only gates *new* allocation. Until the ex-holder
Service is deleted (or mutated off the IP), it keeps announcing the
address, the recreated hold placeholder sits Pending, and conflict
detection (§6) flags the address `Conflict` with the ex-holder as the
offender. That is loud and safe — the address cannot be double-assigned,
and deleting the ex-holder clears the conflict and completes the re-hold
— but it means annotation removal alone does not return the address to
a *held* reservation; the Service's own lifecycle does.

**The flagship flow** — claim → bound address, held by a placeholder →
annotate Service A → handoff, A announces → delete A → stale association
cleared, placeholder re-holds → annotate Service B → handoff, the *same*
address comes back on B. At every point outside the two brief gaps, the
address is assigned to *something* in MetalLB's books and cannot be
handed to anyone else.

**Future optimization — shared handoff.** Where the tenant Service
happens to satisfy MetalLB's IP-sharing constraints (non-overlapping
ports, `externalTrafficPolicy: Cluster`), the gap can be closed entirely
by a sequence of Service mutations: stamp both placeholder and workload
with a per-claim `metallb.io/allow-shared-ip` key, pin the workload while
the placeholder still holds, then release the placeholder. Rejected as
the universal mechanism (the constraints forbid source-IP preservation
and collide with whole-IP use cases) but planned as an opportunistic
upgrade over the gap handoff.

## 6. Conflict detection and recovery

**Detection** (the proposal's "layer 2"). On every Service change, each
IP in the Service's live `status.loadBalancer.ingress` is looked up in
the ledger:

- ledger hit, and the address's binding does not authorize this Service
  (`associatedTo` is unset or names someone else) → the address is driven
  to phase `Conflict`, with events on both the address (naming the
  offender) and the Service (naming the rightful claim). A collision is a
  *visible fault*, never a silent rebinding — whatever caused it: an
  explicit pool/pin annotation, a handoff-gap race, or a write under an
  impersonated identity.
- no ledger hit → a plain Service got an address from some other pool
  (the ordinary eager-allocator case). Not a conflict, not this driver's
  business.

The driver's own placeholders are authorized holders, not offenders. They
are exempt from detection — but only when they live in the driver's
placeholder namespace, which tenants cannot write to; the placeholder
label alone is forgeable and is deliberately not trusted by itself.

**Recovery.** `Conflict` is driver-owned and sticky for the core, so the
driver also clears it: conflicted addresses are rechecked whenever
Services change, and once no Service wrongfully holds the address the
phase is handed back to core bookkeeping (`Bound` if the binding stands,
`Available` if not) — at which point the holding invariant (§4) re-arms
the placeholder.

## 7. Teardown

Deletion of a driver-created `IPAddress` passes through the teardown
finalizer, which now has real backend work:

1. delete the placeholders linked to the address — *this releases the
   MetalLB allocation*;
2. withdraw the pin from the associated Service, if the driver wrote one;
3. release the finalizer.

Under `Retain`, none of this runs at claim deletion: the address goes
`Released`, keeps its `claimRef`, and — per the holding invariant — keeps
its placeholder, so the reservation stays enforced against MetalLB until
an admin either recycles the address (clear `claimRef`) or actually
deletes the object.

## 8. Security posture

With placeholders, MetalLB itself refuses to double-assign a held
address, so the reservation guarantee is backend-enforced in steady
state. The proposal's admission layer — a `ValidatingAdmissionPolicy`
forbidding non-allowlisted principals from writing pool-selecting or
address-pinning annotations — remains part of any real deployment, for
two reasons: it is the only line of defense *during handoff gaps* (§5),
and it stops tenants from steering MetalLB at reserved addresses in ways
that would otherwise end as detected-but-noisy conflicts. The policy is
deployment configuration and ships with an installation, not with this
controller. Placeholders live in a driver-owned namespace tenants must
have no write access to; the `--placeholder-namespace` flag names it.

## 9. Known gaps (acknowledged, deferred)

- **The handoff gap** (§5): during association and disassociation the
  address is briefly unheld in MetalLB's books, protected only by
  `autoAssign: false` + admission + detection. Closable per-Service by
  the shared-handoff optimization; not closable in general.
- **No backwards inventory** (proposal §6): addresses MetalLB assigned to
  plain Services are not imported into the ledger after the fact.
- **L2 only, CIDRs only**: BGP advertisement rendering and MetalLB's
  `a.b.c.d-e.f.g.h` range syntax in class parameters are future
  parameters, not shapes the algorithm depends on.
- **Dual-stack pinning** assumes the Service's `ipFamilies` accept both
  pinned addresses; the driver orders v4 first but does not reconcile
  family policy mismatches.
- **Object sprawl**: one placeholder Service per reserved-and-unattached
  address, plus a ClusterIP each consumes from the service CIDR. Bounded
  by the reserved ranges' size; accepted.

## 10. Alternative considered: driver-side first-free allocation

The driver's first implementation allocated addresses itself: walk the
class CIDRs, skip IPv4 network/broadcast and every IP in the ledger, take
the first free, create the `IPAddress` — the ledger as the only allocator
state, create-collision on the object name as the lock. It was replaced
by the placeholder mechanism because a ledger-only reservation is
invisible to MetalLB (the entire anti-theft posture then rests on
admission), and because a driver-side allocator is a second IPAM that can
disagree with MetalLB's live state — it can pick an IP some plain Service
already holds, discovering the collision only at pin time. Both failure
classes disappear when MetalLB allocates and the driver observes. The
first-free design remains valid for backends with no allocation API at
all, but MetalLB is not such a backend — allocation-by-Service *is* its
API.
