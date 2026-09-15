# How Medusa links modules — and what it gets right that Frappe does not

Companion to [`frappe-linking-study.md`](frappe-linking-study.md). Source:
`medusajs/medusa` cloned shallow into `reference/medusa` on 2026-09-15.

Frappe's answer to "enrichment vs. linking" was **two unrelated mechanisms that
happen to share a tab strip**. Medusa's answer is the opposite: **one
declaration that emits both**, plus a deliberate third form for edges that
already exist, plus a materialized index with a declarative invalidation
contract.

Medusa is also the closer architectural cousin to this repo — modules are
supposed to be independently deployable, the anchor genuinely does not import
the satellite, and the edge is owned by a third party. Where it diverges from us
is worth as much as where it agrees.

---

## 1. One declaration, two planes

`defineLink` is the whole public surface:

```ts
// src/links/product-inventory.ts, in a Medusa app or plugin
import ProductModule from "@medusajs/medusa/product"
import InventoryModule from "@medusajs/medusa/inventory"

export default defineLink(
  ProductModule.linkable.productVariant,
  InventoryModule.linkable.inventoryItem,
  { database: { table: "product_variant_inventory_item",
                extraColumns: { required_quantity: { type: "decimal" } } } }
)
```

`packages/core/utils/src/modules-sdk/define-link.ts:200` compiles that into a
`ModuleJoinerConfig` with **two distinct sections**:

```ts
relationships: [                       // the EDGE — a real pivot table
  { serviceName: Modules.PRODUCT,   entity: "ProductVariant",
    primaryKey: "id", foreignKey: "variant_id",        alias: "variant" },
  { serviceName: Modules.INVENTORY, entity: "InventoryItem",
    primaryKey: "id", foreignKey: "inventory_item_id", alias: "inventory" },
],
extends: [                             // the ENRICHMENT — query-graph grafts
  { serviceName: Modules.PRODUCT, entity: "ProductVariant",
    fieldAlias: { inventory: "inventory_items.inventory" },
    relationship: { serviceName: LINKS.ProductVariantInventoryItem,
                    primaryKey: "variant_id", foreignKey: "id",
                    alias: "inventory_items", isList: true } },
  { serviceName: Modules.INVENTORY, entity: "InventoryItem",
    fieldAlias: { variants: { path: "variant_link.variant", isList: true } },
    relationship: { ... } },
],
```

Read the `extends` block carefully, because that is the answer to the original
question. **`ProductVariant.inventory` becomes a queryable field on an entity
owned by a module that has never heard of inventory.** No column was added to
`product_variant`. No code in the product module changed. The field exists only
in the query graph, and only because a third declaration said so.

That is Frappe's *Manufacturing tab* — `Item.default_bom` — achieved without
putting the column on `tabItem`. Same user-visible affordance, opposite storage
decision.

### What actually gets created

`packages/modules/link-modules/src/utils/generate-entity.ts` builds a MikroORM
`EntitySchema` at runtime from the joiner config: the two foreign keys as a
composite primary key, `extraFields` as real columns, plus
`created_at`/`updated_at`/`deleted_at` and a soft-delete filter. The table is
named by `databaseConfig.tableName` or composed from the two service names and
run through `compressName` (Postgres' 63-char identifier limit).

So a link is **a module in its own right** — its own service name, its own
table, its own migrations, its own id prefix. Neither side owns it. That is
`LinkTypeDecl` plus a storage layer, in one package.

---

## 2. Three kinds of link, and the second one is the interesting one

### 2a. Writable link — a pivot table

The default. `product_variant_inventory_item`, `product_sales_channel`,
`order_payment_collection`. The edge did not exist before, so a table is created
to hold it.

### 2b. Read-only link — zero storage

`packages/modules/link-modules/src/definitions/readonly/cart-customer.ts`:

```ts
export const CartCustomer: ModuleJoinerConfig = {
  isLink: true,
  isReadOnlyLink: true,
  extends: [
    { serviceName: Modules.CART, entity: "Cart",
      relationship: { serviceName: Modules.CUSTOMER, entity: "Customer",
                      primaryKey: "id", foreignKey: "customer_id",
                      alias: "customer" } },
    { serviceName: Modules.CUSTOMER, entity: "Customer",
      relationship: { serviceName: Modules.CART, entity: "Cart",
                      primaryKey: "customer_id", foreignKey: "id",
                      alias: "carts", isList: true } },
  ],
}
```

No `relationships`, no `databaseConfig`, no table. `cart.customer_id` is
**already a column on the cart**. The declaration does nothing but tell the query
planner that the column is a reference into another module's entity, so
`cart.customer.email` resolves.

This is the same distinction Frappe draws between `get_internal_links` (the
anchor already holds the value — no query) and `get_external_links` (the
satellite holds the column — count query), but Medusa makes it a first-class
declaration rather than a shape inferred at count time. Fifteen read-only links
ship in the box: `cart-product`, `order-region`, `store-currency`,
`line-item-adjustment-promotion`, `product-translation`.

`defineLink` enforces the asymmetry — a read-only link **must** name the field
and **may not** declare `filterable` fields:

```ts
if (!leftService.linkable || !leftService.field) {
  throw new Error(`ReadOnly link requires "linkable" and "field" ...`)
} else if (leftService.filterable || rightService.filterable) {
  throw new Error(`ReadOnly link does not support filterable fields.`)
}
```

### 2c. The Index module — materialized, event-invalidated

`packages/modules/index` is Medusa's answer to the exact fork
[`architecture.md`](architecture.md) identifies ("does the index store
denormalized summaries or only references?"). Medusa picked **both**, in two
tables:

```ts
const IndexData = model.define("IndexData", {
  id: model.text().primaryKey(),
  name: model.text().primaryKey(),      // entity type
  data: model.json().default({}),       // the denormalized summary
  staled_at: model.dateTime().nullable(),
})

const IndexRelation = model.define("IndexRelation", {
  id: model.autoincrement().primaryKey(),
  pivot: model.text(),
  parent_name: model.text(), parent_id: model.text(),
  child_name: model.text(),  child_id: model.text(),
  staled_at: model.dateTime().nullable(),
})
```

`IndexRelation` is our `link_index` almost column for column. `IndexData` is the
denormalized-summary half we declined to build.

The part worth stealing is **how it is configured**. Not by code, and not by
satellites pushing after commit — by a GraphQL schema with a `@Listeners`
directive (`packages/modules/index/src/utils/default-schema.ts`):

```graphql
type Product @Listeners(values: [
  "product.product.created", "product.product.updated",
  "product.product.deleted", "product.product.restored"
]) {
  id: ID
  title: String
  handle: String
  variants: [ProductVariant]
  sales_channels: [SalesChannel]
}

type ProductVariant @Listeners(values: [
  "product.product-variant.created", ...
]) {
  id: ID
  sku: String
  prices: [Price]
}
```

`IndexModuleService.registerListeners()` walks that schema, subscribes each
declared event to `storageProvider.consumeEvent(entityRepresentation)`, and —
critically — **only in worker mode** (`if (!this.#isWorkerMode) return`). The
shape of the index, the events that invalidate each part of it, and the
projection stored are one declaration. Rows are marked `staled_at` rather than
deleted, and a `dataSynchronizer` backfills entities whose metadata changed.

---

## 3. What a module must publish to be linkable

`Module.linkable` is generated, not written:
`buildLinkConfigFromModelObjects` (`joiner-config-builder.ts:547`) walks each DML
model's schema, and for every property that is a primary key or an id, emits:

```ts
{ linkable: "product_variant_id",   // the foreign key name a link will use
  primaryKey: "id",
  serviceName: "product",
  field: "productVariant",
  entity: "ProductVariant" }
```

So the set of link targets is derived from the module's own primary keys, and
`defineLink` validates against it at boot:

```ts
const serviceAKeyEntity = serviceAInfo.linkableKeys?.[serviceAObj.key]
if (!serviceAKeyEntity) {
  throw new Error(`Key ${serviceAObj.key} is not linkable on service ${serviceAObj.module}`)
}
...
if (!moduleAPrimaryKeys.includes(serviceAPrimaryKey)) {
  throw new Error(`Primary key ${serviceAPrimaryKey} is not defined on service ${serviceAObj.module}`)
}
```

A module also has to opt in to being queryable at all
(`definition: { isQueryable: true }` in `medusa-config`), and the error message
says so verbatim.

**This is a contract we do not have.** `RegisterLinkType` in this repo accepts
any `source_type`/`anchor_type` pair with no check that the anchor namespace has
declared the key as stable and referenceable.

---

## 4. Reading across: the graph planner

`packages/core/query/src/joiner/remote-joiner.ts` — a three-stage pipeline
documented in the class comment:

> `GraphCatalog` (index configs) → `compileQuery` (query → plan) →
> `executePlan` (fetch + join + shortcuts).

Data loading is behind `IRemoteDataFetcher`, so the planner does not care whether
a module is in-process or remote. `cross-module-joins/` carries the part most
federated designs skip: `rewrite-filters.ts` (410 lines) and
`residual-filters.ts` (400 lines) decide which filters can be pushed down to
which module and which must be applied after the join — because a filter on
`product.variants.inventory.location_id` spans three services and only some of it
can travel.

That is the piece our `GetLinks` fan-out does not have and will eventually need
if callers ever filter across a link rather than just listing.

---

## 5. Deletes: a cascade walk, best-effort

`packages/core/modules-sdk/src/link.ts`. The `Link` service builds a relations
map at boot from every loaded module's joiner config, skipping relationships that
did not opt in:

```ts
if (joinerConfig.isLink && !relationship.deleteCascade) {
  continue
}
```

`executeCascade` then walks that map breadth-first, calling `softDelete` or
`restore` on each related service, deduplicating by `serviceName-primaryKey`, and
**collecting failures into an array rather than aborting**:

```ts
} catch (error) {
  errors.push({ serviceName, method, args: cascadeDelKeys, error: ... })
  return
}
...
return [errors.length ? errors : null, result]
```

No distributed transaction, no retry, no durable record of what failed. A caller
that ignores the error tuple silently orphans rows.

**This is the one place where this repo is already stronger.** The transactional
outbox in `services/pim` plus `doclink.deliveries` with `attempts` and
`last_error` gives bounded retry and a durable record of giving up. Medusa's
cascade is a synchronous best-effort walk that happens to work because every
module is usually in the same process against the same database.

---

## 6. The write path: `additional_data` and workflow hooks

The question Frappe answers with `Custom Field` — *how does satellite data get in
when the user submits the anchor's form?* — Medusa answers without touching the
anchor's schema:

```ts
// packages/core/core-flows/src/product/workflows/create-products.ts:281
const productsCreated = createHook("productsCreated", {
  products: response,
  additional_data: input.additional_data,
})

return new WorkflowResponse(response, { hooks: [productsCreated] })
```

`additional_data` is an opaque bag the core workflow never reads — it validates
it against a plugin-supplied validator and forwards it. A plugin registers a
handler on `createProductsWorkflow.hooks.productsCreated`, receives the created
products *and* the bag, and writes its own module row plus the link. The product
module's API schema does not grow, and the product table does not grow.

That is the missing half of our design. `doclink` has a read plane
(`ListContributions` / `GetLinks`) and a lifecycle plane (outbox → deliveries),
but no write plane: nothing lets a satellite participate in the anchor's create
transaction or react to it with data the user supplied on the anchor's form.

---

## 7. The UI plane, and where Medusa is the anti-pattern for us

Admin extension points are **injection zones** — but they are a compile-time
TypeScript union, extended by declaration merging
(`packages/admin/admin-shared/src/extensions/widgets/types.ts`):

```ts
export type InjectionZone = keyof InjectionZoneRegistry

// a plugin, in index.d.ts:
declare module "@medusajs/admin-shared" {
  interface InjectionZoneRegistry {
    "my-plugin.product-page": true
    "my-plugin.product-page.side": true
  }
}
```

Core zones read `product.details`, `product.details.side`, `product.list`,
`order.details`, `customer.details` — the same taxonomy as
`pim/item:detail.card`. The comment in that file even records the migration away
from `.before`/`.after` suffixes because "the layout composer handles ordering
with drag&drop", which is our `weight` field learned the hard way.

Discovery is where it diverges completely. `admin-vite-plugin` **Babel-parses
every widget file at build time**, reads the `defineWidgetConfig` object
literal, and emits a virtual module:

```ts
export default { widgets: [
  { Component: WidgetComponent_0, zone: ["product.details"], widgetId: "..." },
] }
```

So: every widget is statically imported into the admin bundle, runs in the host's
JS realm, and the set of extensions is frozen at build time. A new plugin means a
rebuild and a redeploy of the admin.

**That is the design this repo deliberately refused** — `architecture.md`'s
argument for cross-origin iframes over custom elements, and for
`ListContributions` at page load over a build-time manifest. Medusa pays for the
convenience (shared theme, shared router, no postMessage, no height negotiation)
with a decoupling that is by etiquette and a deploy coupling between host and
satellite.

---

## 8. Registration, isolation, and tenancy

### Registration is a global import side effect

```ts
global.MedusaModule.setCustomLink(register)     // define-link.ts, module scope
```

and the loader's job is only to import the files
(`packages/core/framework/src/links/link-loader.ts`):

> Load links from the source paths, links are registering themselves, therefore
> we only need to import them

`packages/medusa/src/loaders/index.ts:189` builds the source paths as
`join(plugin.resolve, "links")` for every resolved plugin, so discovery is
filesystem convention over `node_modules` at boot. There is no registry table,
nothing queryable at runtime, and nothing a tenant can toggle.

### Isolation is opt-in and unenforced

`packages/core/utils/src/modules-sdk/load-module-database-config.ts`: each module
reads `PRODUCT_DATABASE_URL` → `MEDUSA_DATABASE_URL` → `DATABASE_URL`, and
`schema` defaults to `"public"`. So by default **every module shares one
connection, one database and one schema**, and module boundaries are convention.
Nothing stops a raw query joining `product` to `inventory_item`.

Compare `internal/pg/migrations/0007_roles.sql` here, where
`svc_subscriptions` has `USAGE` on its own schema only and a cross-schema join
fails with `ERROR: permission denied for schema pim`. That is the difference
between a boundary and a guideline.

### There is no tenancy at all

Modules are listed in `medusa-config.ts`; plugins are resolved from
`node_modules` at boot; links load from plugin directories. One deployment serves
one merchant. There is no per-tenant activation, no install state, no equivalent
of Frappe's `disabled_apps`. On the specific question of a multi-tenant
marketplace, **Frappe is the better reference and Medusa has nothing to say.**

---

## 9. Three systems, one table

| | Frappe | Medusa | doclink (today) |
|---|---|---|---|
| Enrichment of anchor | columns + child tables on `tabItem`; `Custom Field` DDL | `extends` field alias in the query graph; no column | iframe card in a declared slot |
| Edge storage | column on the satellite | pivot table owned by a link module | satellite's own linkage table |
| Edge declared or derived | derived (`SELECT` over `tabDocField`) | declared (`defineLink`) | declared (`RegisterLinkType`) |
| "Edge already exists" case | inferred at count time (internal vs. external) | **declared** (`isReadOnlyLink`) | not modeled |
| Properties on the edge | child-table row fields | `extraColumns` on the link table | not modeled |
| Linkable surface | any Link field, unvalidated | `Module.linkable`, validated at boot | not modeled |
| Read across | count query per doctype, 1s timeout, fail open | graph planner with filter push-down | fan-out, 5s client timeout |
| Materialized index | none | `IndexData` + `IndexRelation`, `@Listeners` | `link_index` (refs only) |
| Delete | derived graph, blocks the delete | best-effort cascade, errors collected | outbox → deliveries, bounded retry |
| Write path for satellite data | `Custom Field` on the anchor form | `additional_data` + workflow hook | **nothing** |
| UI extension discovery | meta-build time, server-rendered | build time, Babel scan, same JS realm | runtime, cross-origin iframe |
| Isolation | one DB per tenant | shared schema by default | per-schema roles, enforced |
| Tenancy / marketplace | per-site app install + `disabled_apps` | none | **nothing** |

---

## 10. What to take from Medusa

Five things, roughly in order of value.

### 10.1 Declare the "edge already exists" case — `isReadOnlyLink`

This is now the second independent confirmation. Frappe infers it at count time
(`get_internal_links` vs `get_external_links`); Medusa declares it up front and
stores nothing. We force every satellite to own a linkage table even when the
reference is already a field it holds for its own reasons.

Add a storage discriminator to `LinkTypeDecl`:

```proto
enum LinkStorage {
  LINK_STORAGE_UNSPECIFIED = 0;
  LINK_STORAGE_OWNED_TABLE  = 1;  // satellite keeps a linkage table (today's only mode)
  LINK_STORAGE_EXISTING_FIELD = 2; // the anchor ref is already a column the satellite holds
}
```

`GetLinks` can then skip a traversal hop for mode 2 and go straight to
resolution, and `ResolveLinks` implementations stop writing a redundant row.

### 10.2 Let the edge carry properties

`extraColumns: { required_quantity: { type: "decimal", defaultValue: "1" } }`.
"This variant needs 2 of that inventory item" is a fact about the *edge*, not
about either endpoint, and today we have nowhere to put it — a satellite either
smuggles it into its own row or invents a side table. Add a small typed
`edge_fields` declaration to `LinkTypeDecl` and return the values on
`DocSummary`-adjacent link rows.

### 10.3 Validate the linkable surface at registration

`defineLink` fails at boot with `Key X is not linkable on service Y` and
`Primary key X is not defined on service Y`. `RegisterLinkType` accepts anything
today. Extend `DocumentType` to publish the keys it considers stable and
referenceable, and reject a `LinkTypeDecl` whose `anchor_type` names a key the
owner has not published. Registration-time failure beats a dangling ref
discovered on the item screen.

### 10.4 Give the index a declarative invalidation contract

Our `link_index` is written by satellites after their own commit, with no
statement anywhere about what makes a row stale. Medusa's `@Listeners` names the
exact lifecycle events that invalidate each indexed entity, and marks rows
`staled_at` instead of deleting them.

Concretely: put the subscribing events on `LinkTypeDecl` next to
`subscribes_to_lifecycle`, add `staled_at` to `doclink.link_index`, and let the
registry stale rows on lifecycle delivery rather than waiting for a satellite to
push a correction. Keep storing refs only — the benchmark already says summaries
buy flatness at the cost of a much wider staleness surface, and Medusa's
`IndexData` is exactly that trade taken.

### 10.5 Build the write plane

`additional_data` plus a workflow hook is the cleanest answer to "user fills in
satellite data on the anchor's form" that any of the three systems has. Frappe's
answer is `ALTER TABLE`. Ours is nothing.

Minimum viable version: an opaque `additional_data` map on PIM's create/update
mutations that PIM never reads, validated against a schema each contributing
module registers, and forwarded verbatim on the lifecycle event. The satellite
reacts to `item.created` with the bag and writes its own row. The PIM API schema
does not grow, and no satellite is in PIM's write transaction.

### What not to take

- **Build-time extension discovery.** Babel-scanning widget files into the host
  bundle freezes the extension set at build and puts every satellite in the
  host's JS realm. We refused this deliberately; Medusa is the worked example of
  what it costs.
- **A global mutable registry.** `global.MedusaModule.setCustomLink` at import
  time is fine for one process and meaningless across services.
- **Shared-schema-by-default.** `schema: "public"` for every module makes the
  boundary a code-review rule. Keep the role grants.
- **Best-effort cascade.** Collecting errors into an array and returning them is
  weaker than the outbox. Keep ours.
