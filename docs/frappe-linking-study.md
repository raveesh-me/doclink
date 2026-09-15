# How Frappe links enrichment and documents — and what to copy

A study of `frappe/frappe` and `frappe/erpnext` (cloned into `reference/`,
shallow, at 2026-09-15), written to answer one question: the Item screen shows
*Sales / Tax / Quality / Manufacturing* as tabs, and *Quotation / Sales Order /
BOM / Batch* as connections. Are those the same abstraction wearing two hats, or
two different mechanisms?

**They are two different mechanisms.** They are not even in the same plane. One
is schema composition on the anchor row; the other is a curated reverse-link
projection that stores no edges at all. Frappe also has two *more* link
abstractions that are easy to mistake for the second one. All four are described
below, then mapped onto this repo.

---

## 1. The field plane — tabs

### What the tabs actually are

There is no "Sales module contributing to Item" in ERPNext. `Sales`, `Tax`,
`Quality`, `Manufacturing`, `Accounting` are `Tab Break` fields in a single
135-field DocType JSON owned by one module:

`reference/erpnext/erpnext/stock/doctype/item/item.json`

```
Tab Break  sales_details          Sales
Tab Break  item_tax_section_break Tax
Tab Break  quality_tab            Quality
Tab Break  manufacturing          Manufacturing
Tab Break  dashboard_tab          Connections     <- the other plane, see §2
```

and the fields under them are plain columns and child tables on `tabItem`:

| tab | fields |
|---|---|
| Sales | `sales_uom` (Link UOM), `grant_commission`, `max_discount`, `customer_items` (Table `Item Customer Detail`) |
| Tax | `taxes` (Table `Item Tax`), `purchase_tax_withholding_category`, `sales_tax_withholding_category` |
| Quality | `inspection_required_before_purchase`, `inspection_required_before_delivery`, `quality_inspection_template` |
| Manufacturing | `include_item_in_manufacturing`, `is_sub_contracted_item`, `default_bom`, `production_capacity`, `default_item_manufacturer`, … |

The giveaway is module ownership. `Item` is `module=Stock`. So are all three
child tables:

```
Item Tax              module=Stock  istable=1
Item Default          module=Stock  istable=1
Item Customer Detail  module=Stock  istable=1
Item                  module=Stock  istable=0
```

`Item Tax` is tax semantics, `Item Default` is accounting semantics,
`Item Customer Detail` is selling semantics — and all three live in the Stock
module, because in Frappe a child table is physically a `parent`/`parenttype`
row set and must be declared as a `Table` field on the parent. **The child table
cannot live in the module that owns its meaning.** The Accounts team's schema is
in the Stock team's file.

This is exactly the "columns on the anchor" failure mode in
[`architecture.md`](architecture.md), shipped at scale and mostly working. It
works because ERPNext is one repository, one release train, one team of
maintainers. It is not a decoupling mechanism, and it was never trying to be.

### The escape hatch for third-party apps

An app that is *not* erpnext enriches Item through `Custom Field` +
`Property Setter`, shipped as fixtures:

- `frappe/custom/doctype/custom_field/custom_field.py:218` — `on_update()` calls
  `frappe.clear_cache(doctype=self.dt)` then `frappe.db.updatedb(self.dt)`. A
  custom field is **real DDL**: `ALTER TABLE tabItem ADD COLUMN`.
- `frappe/utils/fixtures.py` — `sync_fixtures()` imports an app's
  `fixtures/*.json` on install/migrate, which is how India Compliance et al. add
  their GST fields to Item without touching erpnext.
- `frappe/model/meta.py:165` — `Meta.process()` is the assembly order that makes
  this coherent:

  ```python
  self.add_custom_fields()        # :408  merge tabCustom Field rows into .fields
  self.apply_property_setters()   # :428  override label/hidden/reqd/options
  self.init_field_caches()
  self.sort_fields()              # honours insert_after
  self.get_valid_columns()
  self.set_custom_permissions()
  self.add_custom_links_and_actions()   # :477  see §2
  self.check_if_large_table()
  ```

- `frappe/model/meta.py` `is_field_hidden_by_app()` — a field from a disabled
  app disappears, and so does a `Link`/`Table` field *pointing at* a doctype
  owned by a disabled app. That is the marketplace-uninstall story for the field
  plane, and it is a display-layer filter, not a data migration.

So the field plane has two tiers: **compile-time** (fields in the doctype JSON,
one repo) and **install-time** (Custom Field rows, per site, real DDL). There is
no runtime tier. Nothing discovers a field at page load.

---

## 2. The link plane — connections

### The declaration

`reference/erpnext/erpnext/stock/doctype/item/item_dashboard.py` is the entire
source of the second screenshot:

```python
def get_data():
    return {
        "heatmap": True,
        "fieldname": "item_code",                    # default reverse-link column
        "non_standard_fieldnames": {                 # per-doctype override
            "Work Order": "production_item",
            "Product Bundle": "new_item_code",
            "BOM": "item",
            "Batch": "item",
        },
        "transactions": [
            {"label": _("Groups"),    "items": ["BOM", "Product Bundle", "Item Alternative"]},
            {"label": _("Pricing"),   "items": ["Item Price", "Pricing Rule"]},
            {"label": _("Sell"),      "items": ["Quotation", "Sales Order", "Delivery Note", "Sales Invoice"]},
            ...
        ],
    }
```

Read what this is and is not.

- It stores **no edges.** The edge is `tabSales Order Item.item_code` — a column
  on the satellite. The anchor holds nothing, which is the same inversion this
  repo argues for.
- `fieldname` is a *convention*: "assume every linked doctype refers to me
  through a column called `item_code`." `non_standard_fieldnames` is the patch
  list for the ones that do not.
- `transactions` is pure **curation** — grouping and ordering for the UI. It is
  not the set of things that link to Item; §3 computes that automatically and it
  is much larger.

### Assembly, and how another app extends it

`frappe/model/meta.py:768`:

```python
def get_dashboard_data(self):
    data = frappe._dict()
    if not self.custom:
        module = load_doctype_module(self.name, suffix="_dashboard")   # item_dashboard.py
        if hasattr(module, "get_data"):
            data = frappe._dict(module.get_data())

    self.add_doctype_links(data)                                       # :792

    if not self.custom:
        for hook in frappe.get_hooks("override_doctype_dashboards", {}).get(self.name, []):
            data = frappe._dict(frappe.get_attr(hook)(data=data))      # chained, app order
    return data
```

Three contribution channels, in order:

1. **File convention** — `<doctype>_dashboard.py` in the owning app. Only the
   owner can write this.
2. **`DocType Link` child rows** (`frappe/core/doctype/doctype_link/doctype_link.json`)
   — `link_doctype`, `link_fieldname`, `group`, `parent_doctype`,
   `table_fieldname`, `is_child_table`, `hidden`, `custom`. Rows with `custom=1`
   are merged in by `add_custom_links_and_actions()` (`meta.py:477`) from the
   database, so **any installed app, or a site admin through Customize Form, can
   add a connection group without editing the anchor's source.** `add_doctype_links()`
   folds each row into an existing group by label or creates a new one, and
   back-fills `non_standard_fieldnames` / `internal_links` from it.
3. **`override_doctype_dashboards` hook** — a function per app, chained, each
   receiving the accumulated `data` and returning a new one. Full mutation
   rights, including deletion.

Channel 2 is the closest thing Frappe has to `Contribution` in this repo: a
declarative row, written by a satellite, discovered by the host at meta-build
time, with the host never naming the satellite.

### Resolution at runtime

`frappe/desk/notifications.py:242`:

```python
@frappe.whitelist()
def get_open_count(doctype, name, items=None):
    if frappe.flags.in_migrate or frappe.flags.in_install:
        return {"count": []}
    frappe.db.set_execution_timeout(1)          # 1s per count query, hard
    try:
        return _get_linked_document_counts(doctype, name, items)
    except Exception as e:
        if frappe.db.is_statement_timeout(e):
            return {"count": []}                # fail open, badges just vanish
        raise
```

`_get_linked_document_counts` (`:263`) loads the doc with
`check_permission=True`, gets the dashboard data, and for each doctype in the
groups picks one of two shapes:

- **external link** (`:359 get_external_links`) — the satellite carries the
  column. `SELECT count(*) FROM tabSales Order Item WHERE item_code = ?`, plus a
  second count with the doctype's "open" filters for the orange badge. If the
  fieldname sits on several child tables of the same parent, `:380` switches to
  an `or_filters` name list capped at 100, because a filter on an ambiguous
  fieldname silently resolves to whichever child table the query builder joins
  first.
- **internal link** (`:313 get_internal_links`) — the *anchor* carries the
  value, either directly (`data.internal_links[dt] = "fieldname"`) or through
  one of its own child tables (`[table_fieldname, link_fieldname]`). No query at
  all: read the loaded doc, collect distinct values, count them.
- and since pairs can be created in either direction (a Sales Invoice made
  *from* a Delivery Note vs one a Delivery Note was made from), `:290` probes
  the external direction when the internal one comes back empty.

`dynamic_links` (`:418`) adds the discriminator for polymorphic targets:
`{"fieldname": ["Customer", "party_type"]}` becomes an extra
`party_type = "Customer"` filter.

### Client

`frappe/public/js/frappe/form/dashboard.js`

- `:295 filter_permissions()` drops doctypes the user cannot read — cosmetic;
  the server already enforced via `get_lazy_doc(..., check_permission=True)`.
- `:317 render_links()` renders `form_links` template from the same `data` blob.
  Note `this.data.frm = this.frm` and `data_rendered` — rendered once per form.
- `:417 set_open_count()` fires the single `get_open_count` call and paints
  badges.
- The `+` button routes to `frm.make_new(doctype, fieldname)`
  (`frappe/public/js/frappe/form/form.js:2371`), which prefers
  `make_methods` / `custom_make_buttons` if the app defined one, else opens
  quick-entry with the link field pre-filled by scanning the target's meta for a
  `Link` whose `options` is the current doctype.
- The whole block is mounted into a `Connections` section
  (`dashboard.js:54`) which ERPNext parks under the `dashboard_tab` Tab Break —
  which is why the two planes look like peers in the tab strip while sharing
  nothing underneath.

---

## 3. The third abstraction: the derived link graph

`frappe/desk/form/linked_with.py:937`:

```python
def _get_linked_doctypes(doctype, ...):
    ret.update(get_linked_fields(doctype, ...))          # :978
    ret.update(get_dynamic_linked_fields(doctype, ...))  # :1030
    # + any doctype with a Table field whose options == doctype
    links  = frappe.get_all("DocField",     fields=["parent as dt"], filters=...)
    links += frappe.get_all("Custom Field", fields=["dt"],           filters=...)
```

and `get_linked_fields`:

```python
filters = [["fieldtype", "=", "Link"], ["options", "=", doctype]]
links  = frappe.get_all("DocField",     fields=["parent", "fieldname"], filters=filters)
links += frappe.get_all("Custom Field", fields=["dt as parent", "fieldname"], filters=filters)
```

**This is the key architectural fact about Frappe.** The schema is stored as
documents — `tabDocType`, `tabDocField`, `tabCustom Field`, `tabDocType Link` are
ordinary tables in the same database as the data. So "who links to Item?" is a
`SELECT`, not a registry anyone maintains. The complete inbound edge set is
derivable at runtime, for free, including edges added by third-party apps five
minutes ago.

That derived graph powers deletion and cancellation cascades
(`get_submitted_linked_docs`, `collect_deletion_blockers`,
`SubmittableDocumentTree`), the *Links* button, and link integrity on delete. An
app can opt a doctype out with `exclude_from_linked_with = True` in its module.

So the relationship between §2 and §3 is: **§3 is the truth, §2 is the
curation.** The dashboard exists because the derived graph is too big and
unordered to put on a screen, not because the derived graph is missing anything.

---

## 4. The fourth: `Dynamic Link` — the polymorphic satellite

`frappe/core/doctype/dynamic_link/dynamic_link.json`:

```
Dynamic Link (istable=1)
  link_doctype  Link         -> DocType
  link_name     Dynamic Link -> link_doctype
  link_title    Read Only
```

Contact and Address hold a child table of these. One Address row attaches to a
Customer, a Supplier, and a Lead simultaneously, with zero columns on any of
them and zero foreign keys anywhere. This is the row-level version of the same
inversion: **the satellite holds the reference, typed at runtime.**

The cost is in `frappe/model/dynamic_links.py`:

```python
def fetch_distinct_link_doctypes(doctype, fieldname):
    doctypes = frappe.cache.get_value(key)
    if doctypes is None:
        doctypes = frappe.db.sql(f"select distinct `{fieldname}` from `tab{doctype}`", pluck=True)
        frappe.cache.set_value(key, doctypes, expires_in_sec=12 * 60 * 60)
```

To know which doctypes a dynamic link *actually* points at, it runs
`SELECT DISTINCT` over the whole satellite table, caches for 12 hours, and
admits in its own docstring that the results "can possibly be outdated" and a
cache miss "can often be VERY expensive on large table". Raw SQL writes never
invalidate it. Polymorphism without a declaration means the type set has to be
discovered by scanning data — which is precisely the thing `link_types` in this
repo exists to avoid.

---

## 5. Why Frappe can afford all of this

Three properties, all of which we do not have:

1. **Metadata is data, in the same database.** Reverse-link discovery is a join.
   A registry lookup is a local query, not a network hop.
2. **One database per tenant.** A Frappe "site" is a database. `Custom Field`
   rows, `DocType Link` rows, `Property Setter` rows are all per-site. Per-tenant
   schema divergence is achieved with per-tenant `ALTER TABLE`, which is only
   sane because tenant count is in the hundreds, not the hundreds of thousands.
3. **Apps are code on the bench, activated per site.**
   `frappe/__init__.py:1018 _load_app_hooks()` iterates `get_active_apps()` and
   imports `<app>.hooks`; `frappe/app_state.py:32/58` derives disabled modules
   and doctypes from `disabled_apps`, and every meta build, list query, and
   permission query filters against them.

That last point is worth stating precisely, because it is the marketplace
question: **Frappe's marketplace is per-tenant activation of code that is already
deployed.** Installing an app writes rows and runs DDL on one tenant's database.
It does not load code at runtime. A tenant cannot have an app the bench does not
have.

That is the same shape as "some modules hardcoded at code time" — Frappe simply
makes every module that shape and varies only the activation.

---

## 6. Mapping onto this repo

| Frappe | plane | doclink equivalent | gap |
|---|---|---|---|
| doctype JSON fields, `Tab Break` | field | — (deliberately absent) | see §7.4 |
| `Custom Field` + `Property Setter` fixtures | field | — | per-tenant DDL; do not port |
| `<doctype>_dashboard.py` `transactions` | link (curation) | `ExtensionPoint` + `Contribution` | ours is runtime-discovered, theirs is meta-build-time |
| `DocType Link` rows with `custom=1` | link (curation) | `Contribution` | ours has no `group`/ordering beyond `weight` |
| `override_doctype_dashboards` | link (curation) | — | no post-processing hook; probably correct to omit |
| `get_open_count` external/internal | link (resolution) | `LinkResolver` fan-out | ours has no count-only call; see §7.5 |
| `_get_linked_doctypes` derived graph | link (truth) | `link_types` declarations | ours must be declared because schema is not shared — this is the real trade |
| `Dynamic Link` child table | link (storage) | satellite linkage table + `anchor_ref` | same inversion, ours is typed by declaration not by scan |
| app install / `disabled_apps` | activation | **missing** | §7.1 |

The important agreement: `Contribution` and `LinkTypeDecl` are already two
separate declarations in `proto/doclink/v1/registry.proto`. Frappe's `DocType
Link` row does triple duty — it renders the card, defines the count query, *and*
seeds the new-document prefill — which is why `add_doctype_links` has to
back-fill three different dictionaries from one row. Keep them split.

---

## 7. What to change here

### 7.1 Tenancy is the missing dimension, and it is a security boundary

Today nothing in `proto/doclink/v1/*` or `internal/pg/migrations/0005_doclink.sql`
carries a tenant. `contributions` is `UNIQUE (extension_point_id, namespace)` —
globally. That means the first tenant-aware feature request breaks the schema.

Model it as two tables, not one:

```sql
-- code-time identity. One row per module that exists in the world.
CREATE TABLE doclink.modules (
    module_key    TEXT PRIMARY KEY,        -- "subscriptions"
    source        TEXT NOT NULL,           -- 'builtin' | 'marketplace'
    publisher     TEXT NOT NULL,
    ...
);

-- per-tenant activation. This is the Frappe "installed app" row.
CREATE TABLE doclink.module_installations (
    tenant_id     TEXT NOT NULL,
    module_key    TEXT NOT NULL REFERENCES doclink.modules,
    state         TEXT NOT NULL,           -- 'active' | 'suspended' | 'uninstalling'
    installed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, module_key)
);
```

`contributions` and `link_types` stay **tenant-free** — they are declarations
about code, registered at module boot, exactly as now. The tenant filter is a
join against `module_installations` on the read path:

```sql
SELECT c.* FROM doclink.contributions c
JOIN doclink.module_installations i
  ON i.module_key = c.namespace AND i.tenant_id = $1 AND i.state = 'active'
WHERE c.extension_point_id = $2
ORDER BY c.weight DESC, c.title;
```

This keeps the number of declaration rows proportional to modules, not to
tenants × modules, and it makes install/uninstall a single row write with no
registry re-registration.

`tenant_id` **must come from the request's auth context in the registry, never
from a field the caller sets.** `ListContributions` taking a client-supplied
tenant id is a cross-tenant enumeration of who has installed what. Same rule for
`GetLinks` — the fan-out plan is derived from the tenant, and a satellite that is
not installed for that tenant must not be called at all, not called and filtered.

### 7.2 `builtin` vs `marketplace` is metadata, never dispatch

A module compiled into the binary should register through the same
`RegisterContribution` / `RegisterLinkType` calls at boot that a third-party
module makes over the network. The `source` column exists so you can refuse to
uninstall a builtin and so support can answer "where did this card come from" —
not so any code path branches on it. The moment the shell has
`if (contribution.source === 'builtin')` the property `make verify` asserts is
gone.

The Frappe precedent is exact: `erpnext` is an app on the bench with the same
lifecycle as any marketplace app, and `frappe` core reads its hooks through the
same `_load_app_hooks` loop.

### 7.3 CSP and lifecycle must be derived from the *installed* set

`services/webhost` computes `frame-src` as `registry contributions ∩
ALLOWED_EMBED_ORIGINS`. Once tenancy exists, that first term becomes *this
tenant's installed contributions*, and a suspended module must drop out of the
header on the next page load. Likewise the lifecycle fan-out: today
`link_types … WHERE subscribes_to_lifecycle` is the subscriber set; it becomes
that joined to active installations, or a satellite keeps receiving deletes for a
tenant that offboarded it.

### 7.4 Do not build the field plane. Build a slot for it.

The Sales/Tax/Quality tabs are the pattern this repo exists to refuse. If a
satellite needs to *appear as a tab* rather than a card, that is a second
extension point — `pim/item:detail.tab` — with the same contract as
`detail.card`: anchor DocRef in, iframe out, satellite owns storage. Different
slot, same plane, no anchor schema change.

The only fields that belong on `pim.items` are fields the PIM team would defend
in a design review as intrinsic to an item. If tenants genuinely need
tenant-specific *anchor-owned* attributes, that is a third thing: a
tenant-scoped, declared, validated attribute schema owned by PIM
(`pim.item_attribute_defs` + a typed value table), versioned and migrated by PIM.
It is not an open JSON bag and it is not per-tenant DDL. Frappe's `Custom Field`
gets away with DDL only because of one-database-per-tenant.

### 7.5 Give `LinkResolver` a count-only call, with a deadline

Frappe's badge counts are the operational weak point — an unbounded per-doctype
`count(*)` per connection, defended by `set_execution_timeout(1)` and a
fail-open `return {"count": []}`. That is a reasonable design under one database.
Over a network fan-out it is worse: the card menu needs "is there anything here",
and today the only way to answer is a full `ResolveLinks`.

Add `CountLinks(anchor_ref) -> {count, open_count}` to the `LinkResolver`
contract, hold the per-satellite budget well under the existing 5s client
timeout, and render an unknown badge on timeout rather than blocking the menu.
The benchmark in `architecture.md` already says why: with `refs_only`, INDEXED
goes flat at 4.3 ms for eighteen satellites. A count is the degenerate case of
refs-only and should be served from `link_index` keyed
`(tenant_id, anchor_ref)`, with the federated path as the correctness fallback.

### 7.6 Borrow the internal/external distinction

`get_internal_links` vs `get_external_links` is a real distinction we do not
model. Some links the anchor *does* hold — `Item.default_bom` is a column on the
item, and Frappe counts it without a query because the value is already loaded.
In our terms: when the anchor's own payload already contains a DocRef into
another namespace, the answer needs a resolve (for the display summary) but not a
traversal (to find the edge). Worth a flag on `LinkTypeDecl` so `GetLinks` can
skip a fan-out hop it does not need.

---

## 8. The one-line answer

Frappe has one abstraction for **enriching a document** (fields on the anchor's
own table, composed at build time from the owning app and at install time from
`Custom Field` DDL) and a separate one for **relating documents** (columns on the
satellite, discovered automatically by querying the schema-as-data, and curated
for display by `<doctype>_dashboard.py` plus `DocType Link` rows). The tabs and
the Connections panel sit next to each other in the UI and share no code.

This repo already picked the second abstraction and refused the first. The work
left is not to add the first one — it is to add the **activation dimension**
(tenant × module) that Frappe gets for free by giving every tenant its own
database, and to make every read path in the registry derive its fan-out from
that, from trusted context, on the server.
