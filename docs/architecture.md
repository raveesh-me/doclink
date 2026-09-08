# doc-link: linking documents without coupling their owners

## The problem

Take an item in a commerce system. The PIM team owns it: SKU, title, price,
status. Then subscriptions wants to attach "this item can be bought on a monthly
plan, max 3 per cycle, prepaid only". Then shipping wants "1.2kg, 40×20×8, needs
a signature". Then a marketplace connector wants its own external id, and then a
tax module wants a classification code.

There are three usual answers, and all three are bad in the same way.

**Columns on the item.** `items.subscription_plan_id`, `items.shipping_weight_g`.
The item table becomes the union of every module that ever wanted something, its
migration history becomes a record of other teams' roadmaps, and the PIM team
reviews schema changes for features it does not own. It also cannot express
many-to-many, so the first module that needs it forces a join table into the
anchor's schema anyway.

**A JSON extension bag.** `items.extensions jsonb`. Now the coupling is
invisible rather than absent: nothing is typed, nothing is indexed well, nothing
can be validated, and two modules will eventually pick the same key.

**Foreign keys from the item to each satellite.** Same as columns, plus the item
service now cannot be deployed without every satellite's database being
reachable.

All three share one property: **the anchor knows about the satellites.** Adding a
module means editing the busiest table in the company. And the anchor team cannot
possibly anticipate the list — a marketplace integration that does not exist yet
still needs to attach data to items.

## The inversion

The satellite knows about the anchor. The anchor knows nothing.

Concretely:

- `pim.items` has no column referring to any satellite, and never will.
- `subscriptions.coverage` has an `anchor_ref TEXT` column holding
  `"pim/item/01J8XYZ"`. It is not a foreign key. It cannot be: the item lives in
  a different database owned by a different team.
- The PIM item screen declares a slot, `pim/item:detail.card`, and passes into it
  exactly one thing: the item's DocRef string.
- Satellites register at runtime saying "I fill that slot, here is my URL". The
  shell discovers them by asking, at page load, not by importing them.

The only vocabulary shared across the boundary is the **DocRef**:
`(namespace, type, id)`, canonically `"pim/item/01J8XYZ"`. It carries identity
and nothing else — no schema, no fields, no hint about contents. A service that
receives a DocRef it does not own can do exactly two things with it: store it in
its own linkage table, or hand it back to the registry for resolution.

## The two planes

### Control plane: the registry (`doclink`)

Holds **declarations**, never link rows:

| Table | Who writes it | What it means |
|---|---|---|
| `document_types` | the owner | "`pim/item` exists; ask this endpoint to describe one" |
| `extension_points` | the host screen | "`pim/item:detail.card` exists; the anchor DocRef is all you get" |
| `contributions` | a satellite | "I fill that slot; my card is at this URL" |
| `link_types` | a satellite | "I hold `subscriptions/plan --covers--> pim/item`; ask me to traverse it" |

Note the asymmetry: the extension point does not list who fills it. It cannot.
That is the whole point.

### Data plane: the satellites

Each satellite owns its linkage table, its index on `anchor_ref`, and its own
answer to `ResolveLinks(anchor) -> []DocSummary`. The registry knows an endpoint;
it does not know what a plan or a shipping profile is.

## What breaks, and how it is handled

### A satellite that is slow rather than down

`GetLinks` fans out with a shared context and a 5-second client timeout, so one
slow satellite sets the floor for the whole response — the p99 column above is
mostly this. Nothing here does per-satellite deadline budgeting or hedging, and a
production version would: the registry knows the fan-out plan, so it is the right
place to enforce a per-group deadline and return partial results rather than let
the slowest module define the item screen's latency.

### Deleting an item

No foreign keys means no `ON DELETE CASCADE`. Nothing in the database will tell
subscriptions that an item is gone.

The replacement is a transactional outbox. PIM writes the delete and a lifecycle
event **in one transaction**, so an item can never be deleted without an event
being durably queued. A relay drains the outbox to the registry; the registry
knows the subscriber set (it is a query on `link_types`) and fans out; each
satellite reconciles its own orphans in its own database on its own schedule.

PIM does not wait for this, does not know who received it, and does not know
whether it succeeded. Orphan rows are legal, transiently. Delivery is retried
with a bounded attempt count, and giving up is logged loudly — because at that
point a satellite is holding rows pointing at a document that is gone and nothing
else will ever tell it.

### A satellite being down

`GetLinks` fans out in parallel and collects failures per group. One dead
satellite degrades one card. It does not blank the item screen, and the error
travels to the UI inside the group so the card can render its own failure state.

### Referential integrity on write

`AttachPlan` does **not** call PIM to check that the item exists. Doing so would
make subscriptions depend on the anchor's availability at write time, which is
the coupling being removed. A link to a nonexistent item is allowed and surfaces
on read as `DocSummary.missing = true`.

## Why the schema isolation is real

One CNPG cluster, one database, one schema per service, and one **login role**
per schema granted `USAGE` on its own schema only. This is a laptop concession
over a Postgres cluster per service, but the boundary it stands in for is
enforced by Postgres rather than by code review:

```
$ psql "postgres://svc_subscriptions:...@localhost/doclink" -c "SELECT * FROM pim.items"
ERROR:  permission denied for schema pim
```

A cross-schema join is not discouraged here. It fails.

## Why iframes for the embeds

The alternative — a custom element loaded as an ES module from the satellite's
origin — is lighter, themes trivially, and needs no height negotiation. It also
runs in the host's JavaScript realm, which means the decoupling is by etiquette:
a satellite can reach into the host's globals, break its router, or ship a
library version that clobbers the host's.

An iframe on a distinct origin makes the isolation the browser's problem instead
of a review comment. The costs are real and were paid deliberately: a
postMessage protocol, height negotiation, a theme handshake because CSS does not
cross the boundary, and focus/a11y friction.

`deploy/manifests/05-ingress.yaml` is what makes it true — seven distinct
hostnames. Serving an embed same-origin with the host would leave the `sandbox`
attribute as theatre, so `mountEmbed` checks for it and warns.

The demonstration that the contract holds is in the repo: the shell and the
subscriptions card are Vue, and the **shipping card is plain TypeScript with no
framework at all**. The host cannot tell the difference.

## What the shell knows, and the trust boundary

Grep the shell's source for satellite names and you find nothing outside
comments — `make verify` asserts it. The shell hardcodes three things: a client
for its own backend, a client for the registry, and the id of the slot *it
declares*. Everything else arrives from `ListContributions` at page load, and
`EmbedCard` mounts whatever URL comes back.

So the shell knows a *category* exists, not which members. Same relationship as
an interface to its implementations.

That leaves one real hole, which `services/webhost` closes. A CSP has to be a
response header — a `<meta http-equiv>` tag cannot work, because the origin list
is only known after asking the registry and a CSP meta tag inserted after parse
is ignored. So a small Go server sits in front of the shell's static build and
computes the header:

```
connect-src   'self' + PIM's own API origins    ← configuration; PIM's own deps
frame-src     registry contributions ∩ ALLOWED_EMBED_ORIGINS
```

The intersection is the part that matters. Deriving `frame-src` purely from the
registry would be theatre: a rogue registry row would add its own origin to the
policy meant to constrain it. So registry origins are filtered through a pattern
list supplied by deployment configuration, which the registry cannot write.

`http://*.doclink.localhost` covers every satellite behind the ingress with no
per-satellite entry, so the decoupling survives — a new module still needs zero
configuration anywhere. A row pointing somewhere else is dropped, logged at
ERROR, and the browser refuses to frame it:

```
Framing 'http://evil.example.com/' violates the following Content Security Policy
directive: "frame-src http://localhost:5182 http://localhost:5183".
```

When the registry has never answered, `frame-src` is `'none'` — fail closed. That
costs nothing, because the shell discovers its cards through the same call: a
registry that cannot answer already means there are no cards to frame.

## Read strategies, and what the benchmark found

`GetLinks` answers the same question three ways:

- **FEDERATED** — fan out over ConnectRPC to every service in `link_types`.
  Strongly consistent, N network hops.
- **INDEXED** — read `doclink.link_index`, maintained by satellite writes after
  their own commit. One local query, eventually consistent.
- **MONOLITH** — the straw-man: one SQL query against a schema where satellites
  are columns on the anchor's table.

Measured on an M-series laptop: 500 items, 16 concurrent clients, 6 s per
configuration, synthetic satellites injecting 2 ms each. Raw output in
`bench/results/latest.json`; reproduce with `make bench`.

| satellites in fan-out | FEDERATED p50 | INDEXED p50 | MONOLITH p50 |
|---|---|---|---|
| 2 | 12.1 ms | 10.3 ms | 4.9 ms |
| 4 | 15.6 ms | 17.6 ms | — |
| 6 | 20.9 ms | 17.1 ms | — |
| 10 | 23.1 ms | 20.8 ms | — |
| 18 | 34.1 ms | 33.1 ms | — |

Note the four-satellite row, where INDEXED is *slower* than FEDERATED. That is
not noise to be smoothed away — it is the same effect as the rest of the column,
just large enough to cross over: INDEXED pays a local query **and** a hydration
fan-out, so when the fan-out dominates it is strictly extra work.

Three things worth saying plainly.

**The monolith is 2.7× faster at the two-satellite case.** That is the real price
of the decoupling on the read path, and pretending otherwise would make the rest
of the argument untrustworthy. It is measured only at that point because
extending it would require a schema migration on the anchor's table per
satellite — which is the cost the design exists to avoid, and which no benchmark
harness can perform on the anchor team's behalf.

**The index barely helps.** 33.1 ms vs 34.1 ms at eighteen satellites, which is
nothing like what "one query instead of eighteen calls" would predict. The reason
is that the index stores *refs*, so hydrating them into displayable summaries is
itself a fan-out — one call per namespace. The index moves the fan-out; it does
not remove it.

**Unless the caller can work with bare refs.** With `refs_only`
(`make bench-refs`, `bench/results/refs-only.json`):

| satellites in fan-out | FEDERATED p50 | INDEXED p50 |
|---|---|---|
| 2 | 9.0 ms | 3.1 ms |
| 6 | 14.4 ms | 3.2 ms |
| 18 | 30.8 ms | 4.3 ms |

INDEXED goes **flat** — 4.3 ms at eighteen satellites, faster than the monolith's
4.9 ms at two — while FEDERATED grows linearly. Same code, same data; the only
change is whether the caller needs display strings.

So the design decision this benchmark actually informs is not "federated vs
indexed". It is **whether the index stores denormalized summaries or only
references**. Storing summaries would make INDEXED flat for the UI case too, at
the cost of a much wider staleness surface and a much harder invalidation
problem — every satellite would have to push a summary on every write, and a
plan rename would have to reach every index row mentioning it.

That is a real fork in the road, and the honest answer for the item screen is
probably neither: FEDERATED is strongly consistent, degrades per-card when a
satellite is down, and at the satellite counts a real product actually has (two
to six, not eighteen) costs 12–21 ms. The index earns its keep for bulk
questions — "which items have a subscription" — where the caller wants refs and
is querying thousands of anchors, not one.

## The number that matters

Adding a satellite to the item screen requires **zero** changes to the PIM
service and the PIM frontend. Not "one small config change" — zero. The check is
mechanical:

```
$ grep -ri 'subscription\|shipping' services/pim/ web/pim-web/src/
(no matches)
```

Neither the anchor service nor the shell contains the string. They cannot: PIM
declares a slot and publishes lifecycle events, and both of those are written in
terms of `pim/item` alone. A satellite registers itself at boot and appears in
the card menu on the next page load.

That is the deliverable. The latency table above is what it costs.
