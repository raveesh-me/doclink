# PIM with runtime-discovered modules

A proof of concept for a multi-tenant PIM that composes product responses from
independently deployed modules it has no compile-time knowledge of, discovered
through a per-tenant registry, over Connect RPC.

**The thesis:** PIM never imports a module's types, yet API consumers still
receive typed, documented, plain JSON.

```bash
make demo    # build, start PIM + both modules, walk every acceptance criterion
```

```bash
make dev                 # PIM :8080, taxes :8081, saas :8082, output prefixed per service
make register            # in another terminal: t1 gets taxes + saas, t2 gets taxes
curl -X POST localhost:8080/pim.v1.PIM/GetProduct \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"t1","id":"p1","context":{"buyer_state":"KA"}}'
curl localhost:8080/openapi/t1.json
```

`make proto`, `make build` and `make test` do what they say. Requires Go 1.26
(the current `x/tools` and connect releases need it) and `buf` on the PATH;
protoc plugins are pinned as `go tool` dependencies in `go.mod`. `gen/` is not
committed, and every make target regenerates it first.

## Architecture

```
                         API consumer (plain JSON)
                    │                              ▲
   POST /pim.v1.PIM/GetProduct          GET /openapi/{tenant}.json
                    │                              │
┌───────────────────▼──────────────────────────────┴─────────────────────────┐
│ PIM process :8080                                                          │
│                                                                            │
│  PIM service ── ProductCore ──► SQLite products     (the only record)      │
│      │                                                                     │
│      ▼                                                                     │
│  Composition engine                        OpenAPI builder                 │
│   1. ACTIVE modules for tenant ◄──────┐      pim.v1 descriptors            │
│   2. fragment cache (hit = no call)   │      + registered json_schema      │
│   3. errgroup fan-out, 1 batched call │                                    │
│      per module, deadline ≤ parent    │                                    │
│   4. breaker ─ vet: module_id, point, │                                    │
│      64KB, JSON Schema                │                                    │
│   5. merge into extensions[module_id] │                                    │
│      (key from the registry row)      │                                    │
│      │                                │                                    │
│      │                     Registry service ── SQLite registry_modules     │
│      │                      Register = Describe handshake + schema compile │
│      │                      Unregister = DRAINING, delete after grace      │
│      │                      InvalidateFragments ─► cache                   │
│      │                      UpdateModulePolicy                             │
└──────┼────────────────────────────────▲─────────────────────────────────────┘
       │ ext.v1.ModuleExtension          │ Describe (at registration)
       │ EnrichProducts (Struct)         │ InvalidateFragments (IDs only)
       │ one generated client, N URLs    │
┌──────▼────────────────────┐   ┌────────┴──────────────────┐
│ module-taxes :8081        │   │ module-saas :8082         │
│ own types: taxes.v1.*     │   │ own types: saas.v1.*      │
│ → protojson → Struct      │   │ → protojson → Struct      │
└───────────────────────────┘   └───────────────────────────┘

 PIM ──✗── internal/modules/**, gen/modules/**   (no_module_imports_test.go)
```

PIM owns three contracts, and that is all it compiles:

| proto | who implements | purpose |
|---|---|---|
| `pim/v1/pim.proto` | PIM | the public API; `Product.extensions` is `map<string, Struct>` |
| `ext/v1/extension.proto` | every module | `Describe` and `EnrichProducts` — nothing else |
| `registry/v1/registry.proto` | PIM | install, drain, list, invalidate, set policy |

Each module also has its own proto package (`proto/modules/{taxes,saas}`)
that only the module compiles.

## Why `Struct`, not `Any`

`Fragment.data` is `google.protobuf.Struct`, and this is load-bearing.

protojson resolves an `Any`'s `type_url` against `protoregistry.GlobalTypes`,
which only contains types compiled into the binary. PIM does not compile module
types, so marshalling an `Any` to JSON would fail — at serialization time,
*after* the handler succeeded, and only for tenants that installed that module.
It is exactly the failure that passes every test written against a tenant with
no modules.

`Struct` is a well-known type present in every binary and marshals with the
default resolver, always. The same reasoning is a comment on the `Fragment`
message so it survives refactors.

Strong typing is not given up; it moves to each side of the boundary:

- **Inside the module**, data is a real proto message
  (`taxes.v1.TaxComputation`) converted to `Struct` at the module's edge with a
  protojson round-trip (`internal/modules/modkit.ToStruct`). A module cannot
  accidentally send `"seats": "five"` — the saas chaos mode has to bypass its
  own type to do it.
- **At PIM**, every fragment is validated against the JSON Schema the module
  registered, and dropped if it fails.
- **For consumers**, `GET /openapi/{tenant}.json` puts each module's schema back
  under `Product.extensions.{module_id}`. Tenant t1's spec describes `taxes` and
  `saas`; t2's describes only `taxes`. Neither was generated by importing
  either module. A test composes a real product and validates its wire JSON
  against the generated spec.

## Cache, not record

PIM is pull-only. The module is always the source of truth, and module data is
never a row in PIM. There is no `fragments` table.

PIM does hold an in-memory **cache**, which is a different thing:

- **evictable** — TTLs, `InvalidateFragments`, unregistration, a restart;
- **reconstructible** — any entry can be rebuilt by calling the module again;
- **never queried** — the engine reads it by exact key only. Nothing can ask it
  "which products have tax class reduced".

It is keyed `(tenant, module, extension point, product, context hash)`. The
context hash covers only the `context_keys` the point declared in `Describe`, so
`buyer_state` splits the cache and `channel` does not. TTL comes from each
point's `cache_ttl_seconds`; 0 means never cache, which is reserved for
fragments carrying per-request identity.

`InvalidateFragments(tenant, module, product_ids)` exists because TTL cannot
express "this classification rule changed and 3,000 products just moved" — only
the module knows that. It carries IDs, never values: PIM re-pulls. Each
invalidation bumps a per-module generation, so a read already in flight cannot
write back what the module just declared out of date.

On module failure under `FAIL_OPEN`, an expired entry (kept up to 10 minutes) is
served and the product is still marked `degraded`. A slightly old tax class
beats no tax class on a catalogue browse. `FAIL_CLOSED` never serves stale.

Milestones 1–6 ran with every read on the network, so the cache is measurably
an optimisation, not a load-bearing assumption. From the demo: a cold read of a
product with both modules is ~43ms (taxes sleeps 40ms deliberately); an
identical read is ~1ms and makes zero module calls.

## What pull-only costs

**PIM cannot filter, sort, or paginate on module data.** "List products with
tax class reduced, cheapest first" is not a slow query here; it is an
unanswerable one. Answering it would mean fetching every product, fanning out
for all of them, filtering in memory, then paginating. Anything that must appear
in a `WHERE` or `ORDER BY` has to live in `ProductCore`.

In exchange, ownership is unambiguous and a whole class of sync bugs cannot
exist: PIM never holds a copy of module data that could disagree with the
module.

When this bites, the escape hatches are:

1. **Promote the field into `ProductCore`**, if it is genuinely core.
2. **Project fragments into a separate read-model index** that is explicitly
   derived and explicitly rebuildable from the modules — not a second source of
   truth wearing a cache's clothing.

Nothing in the demo needed module data in a predicate, so nothing here works
around it.

## Failure behaviour

Every read depends on module availability, so the degraded path is the normal
behaviour of a partially healthy system, not error handling.

| situation | FAIL_OPEN | FAIL_CLOSED |
|---|---|---|
| module down, slow past its timeout, or breaker open | extension omitted (or stale served), `degraded: [id]` | `503 unavailable`, message names the module |
| fragment fails its schema, exceeds 64KB, names an unregistered point | that product's extension from that module dropped, logged, `degraded` | 503 |
| fragment claims another `module_id` (`core`, `taxes`, …) | rejected at merge, logged, `degraded` | 503 |
| response includes a product that was not requested | whole call rejected | 503 |

- **Timeouts** are derived from the parent: if the read has 200ms left and the
  module is allowed 500ms, it gets 200ms, and Connect forwards that deadline to
  the module.
- **Circuit breaker** per `(tenant, module)`, in memory: 5 consecutive failures
  open it for 10s, then half-open admits exactly one probe. A read cancelled by
  its caller or out of budget does *not* count against the module — otherwise a
  client sending 1ms deadlines could trip every breaker a tenant has.
- **Writes never touch modules.** `UpsertProduct` stores `ProductCore` and
  returns; the demo upserts a product while saas is dead.

## Registration is a handshake

`RegisterModule(tenant_id, base_url)` dials the URL, calls `Describe`, and
rejects the whole registration — reporting every problem at once — unless:

- every `json_schema` compiles (with no loaders: `$ref` to files or URLs fails);
- every schema is an object with declared `properties`, and no two points of one
  module declare the same property (they merge into one object);
- `module_id` is lowercase, not reserved (`core`, `pim`, `_meta`), and not
  already installed for the tenant;
- point names and `schema_ref`s are unique within the module.

It then writes one row per extension point, `ACTIVE`. A URL that cannot answer
is refused now rather than discovered later as a dead row degrading every read.

`UnregisterModule` sets `DRAINING` — new reads stop fanning out to it at once —
and deletes after a grace period (5s). Reads that had already resolved the
module finish normally; the demo unregisters taxes with 20 reads inside its
40ms sleep and all 20 succeed.

`UpdateModulePolicy` changes `timeout_ms` and `failure_mode` without a
handshake: policy is the tenant's choice, not the module's description, and must
be changeable while the module is down.

## The boundary, enforced

`internal/pim/no_module_imports_test.go` loads the full transitive import graph
of `internal/pim/...` and `cmd/pim` — test files included — with
`golang.org/x/tools/go/packages`, and fails on any path containing
`internal/modules` or `gen/modules`. The second pattern matters: importing a
module's generated types would register them in `GlobalTypes`, which is as much
a breach as importing its code. The test refuses to pass vacuously: it fails if
the graph did not load or if the walk never reached `gen/ext/v1`. It runs in
`make test`; a deliberately injected import fails with the chain
`cmd/pim -> internal/pim -> internal/modules/modkit`.

## Wire details worth knowing

- JSON field names are lowerCamel on output; requests accept either form.
- `int64` fields are **decimal strings** (`"priceMinor": "49900"`), per proto3
  JSON. The OpenAPI spec documents them as such.
- Empty repeated fields are omitted, so `degraded` is absent rather than `[]`
  when nothing failed.

## Layout

```
proto/pim/v1            public API
proto/ext/v1            the contract modules implement
proto/registry/v1       module registry
proto/modules/*         module-owned types (PIM never imports gen/modules)
cmd/pim                 PIM + registry, :8080
cmd/module-taxes        :8081 — HSN classification (TTL 300) and GST by buyer_state (TTL 30), 40ms sleep
cmd/module-saas         :8082 — seats and billing cycle (TTL 60); /chaos to misbehave on demand
cmd/demo                the acceptance walkthrough
internal/pim            product store, composition engine, breaker, cache, OpenAPI
internal/registry       registry store, handshake, schema compilation
internal/modules        module implementations
```

The SQLite database (`data/pim.db` under `make dev`) is seeded on first start
with tenants t1 (five products) and t2 (three).

## Knowingly absent or unfinished

- **No authentication anywhere.** In particular `InvalidateFragments` and
  `UnregisterModule` can be called by anyone who can reach PIM.
- **`DEAD` is never set.** The spec's row shape has it, but no behaviour for it.
  Runtime liveness is the breaker's job; a registry flag that dropped a module
  from fan-out would silently bypass `FAIL_CLOSED`, since a read that makes no
  call cannot fail. `last_verified_at` is the handshake time.
- **Cache and breakers are per process.** Several PIM replicas would each have
  their own, and `InvalidateFragments` would reach only one.
- **Point schemas using local `$ref`/`$defs`** do not survive the property-level
  merge into the OpenAPI spec.
- `module-saas` exposes `/chaos` and both modules expose `/stats`; they exist for
  the demo.
