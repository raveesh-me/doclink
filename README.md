# doc-link

A POC for linking documents across service boundaries **without the anchor
knowing its satellites**.

An item in a PIM is owned by one team. Subscriptions wants to attach plan
coverage to it; shipping wants weight and dimensions; a marketplace connector
that does not exist yet will want an external id. The usual answers — columns on
`items`, a JSON extension bag, foreign keys from the item outward — all make the
anchor's schema the union of everyone else's roadmap.

This repo inverts the arrow. The satellite holds the reference; the anchor holds
nothing. The item detail screen renders cards it has never heard of, discovered
from a registry at page load and handed exactly one thing: the item's DocRef.

**Read [`docs/architecture.md`](docs/architecture.md) for the reasoning, the
failure modes, and the benchmark results.**

---

## Quick start

```bash
make pg          # Postgres in podman on :5433, migrated
make build       # Go binaries
make seed        # 1000 items, identical data in all three representations
make dev         # 4 services + 3 frontends
```

Then open <http://localhost:5181/items> and click an item.

```bash
make bench       # the strategy comparison
make verify      # assert the architectural rules
make dev-stop    # stop everything
```

## CI

`.github/workflows/ci.yml` runs five jobs on every push and pull request:

| job | what it catches |
|---|---|
| `go` | gofmt, `go vet`, build, `go test -race` |
| `protos` | `buf lint`, and that committed generated code still matches the protos |
| `verify` | the architectural rules, against a real Postgres service container |
| `web` | typechecks all three frontends (`vite build` does not typecheck) and builds them |
| `images` | all nine container images build |

The `verify` job is the one worth keeping. Ten of its eleven assertions are
static, but the role-isolation check needs a live server to prove Postgres
actually refuses a cross-schema read — so the job runs one, migrates it, and
asserts the refusal.

### In a cluster

```bash
make cluster     # images + kind + CNPG + ingress, ~10 min from cold
```

Then <http://pim.doclink.localhost:18080/items>.

The cluster path is the honest one: it gives every component its own **origin**,
which is what makes the iframe isolation real rather than decorative. `make dev`
uses distinct ports as the closest available stand-in.

`*.localhost` resolves to loopback automatically in Chrome and Firefox. Safari
does not do this — add to `/etc/hosts`:

```
127.0.0.1 pim.doclink.localhost subs.doclink.localhost ship.doclink.localhost
127.0.0.1 doclink-api.doclink.localhost pim-api.doclink.localhost
127.0.0.1 subs-api.doclink.localhost ship-api.doclink.localhost
```

The cluster uses host ports **18080/18443**, not 80/443, so it does not collide
with anything already running.

```bash
make bench-cluster   # the same sweep, against the cluster
make kind-down       # tear it down
```

### Cluster troubleshooting

Everything below was hit while bringing this up on a laptop, so it is worth
knowing in advance.

**`make cluster` needs headroom.** A podman machine with the 3.6 GB default
cannot hold a kind cluster, a replicated Postgres, and whatever else you are
running. Before starting:

```bash
podman machine stop && podman machine set --memory 8192 --cpus 6 --disk-size 60
podman machine start
```

Symptoms of not having it: `no space left on device` during an image pull, or
pods stuck `Pending` with insufficient memory. Check with
`podman machine ssh 'df -h /; free -m'` and reclaim with `podman image prune -a`.

**Deploy sizing.** `deploy/kind/cluster.yaml` uses one worker and
`deploy/cnpg/cluster.yaml` uses `instances: 1`, both sized down from the intended
two. Both files say so, and raising them changes nothing else: services connect
to the `doclink-pg-rw` Service, which CNPG points at whichever pod is primary.

**Do not delete a CNPG bootstrap Job by hand.** It leaves the PVC annotated
`initializing` with no Job to finish it, and the cluster sits in
`Setting up primary` forever. Recover by deleting the `Cluster` and its PVC and
re-applying.

**Image names carry a `localhost/` prefix.** podman names locally built images
that way and kind loads them under the saved name, so the manifests say
`localhost/doclink/pim:dev`. Drop the prefix and Kubernetes resolves it to Docker
Hub and `ImagePullBackOff`es.

## What is here

```
proto/            the contracts — start with doclink/v1/core.proto
  doclink/v1/     DocRef, the registry, and the two interfaces satellites implement
services/
  doclink/        the registry: declarations, fan-out, lifecycle dispatch
  webhost/        serves the shell and computes its Content-Security-Policy
  pim/            the anchor: items, DocumentResolver, transactional outbox
  subscriptions/  a satellite: many-to-many, owns its linkage table
  shipping/       a satellite: many-to-one, owns its linkage table
web/
  host-sdk/       the postMessage protocol — read protocol.ts first
  pim-web/        the shell (Vue 3)
  subscriptions-web/  a card (Vue 3)
  shipping-web/   a card (plain TypeScript, no framework — deliberately)
bench/            the three-strategy comparison, with synthetic satellites
deploy/           kind, CNPG, and one hostname per component
scripts/verify-decoupling.sh   the architectural rules, as assertions
```

## The three things worth looking at

**`proto/doclink/v1/core.proto`** — the `DocRef` message. Three strings. That is
the entire shared vocabulary between services, and everything else follows from
keeping it that small.

**`web/host-sdk/src/protocol.ts`** — the host/guest contract. The host sends one
piece of domain data, the anchor's DocRef, and nothing else. The shipping card
proves this imposes no framework.

**`services/pim/store.go`, `mutate()`** — the item write and its lifecycle event
in one transaction. This is the replacement for `ON DELETE CASCADE` across a
database boundary, and it is where decoupled designs usually cheat.

## Ports

| | local (`make dev`) | cluster (`make cluster`) |
|---|---|---|
| PIM shell | :5181 | `pim.doclink.localhost:18080` |
| subscriptions card | :5182 | `subs.doclink.localhost:18080` |
| shipping card | :5183 | `ship.doclink.localhost:18080` |
| registry API | :8080 | `doclink-api.doclink.localhost:18080` |
| PIM API | :8081 | `pim-api.doclink.localhost:18080` |
| subscriptions API | :8082 | `subs-api.doclink.localhost:18080` |
| shipping API | :8083 | `ship-api.doclink.localhost:18080` |
| Postgres | :5433 (podman) | CNPG in-cluster |

## Caveats

This is a POC, and the following are knowingly absent:

- **No authentication or authorization anywhere.** In particular `Deregister` is
  unauthenticated, and anything that can call it can make a satellite vanish from
  every item screen at once.
- **CORS is wide open.** The CSP constrains what the shell will *frame*
  (`services/webhost`), but the APIs still accept any origin.
- **Credentials are checked in** (`deploy/manifests/01-secrets.yaml`) so that
  `make cluster` is one command and nothing about what services can reach is
  hidden.
- **One Postgres cluster with a schema and login role per service**, rather than
  a cluster per service. The boundary is still enforced by Postgres — see
  `make verify` — but it is a laptop concession.
- **The registry is a single point of failure** for card discovery and lifecycle
  fan-out. Its reads are cacheable and its writes are rare, so this is tractable,
  but it is not addressed here.
