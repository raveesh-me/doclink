package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	doclinkv1 "github.com/raveesh-me/doclink/gen/doclink/v1"
	"github.com/raveesh-me/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh-me/doclink/internal/docref"
)

// resolver answers GetLinks. The three strategies below are semantically
// equivalent and differ only in where the work happens, which is what makes
// them comparable in the benchmark.
type resolver struct {
	store *store
	log   *slog.Logger

	// benchPool connects as svc_bench, the only role permitted to read the
	// monolith schema. The registry's own role cannot, which keeps the
	// straw-man baseline from quietly becoming a capability of the real design.
	benchPool *pgxpool.Pool

	httpClient *http.Client

	linkClients sync.Map // endpoint -> LinkResolverClient
	docClients  sync.Map // endpoint -> DocumentResolverClient
}

func newResolver(st *store, benchPool *pgxpool.Pool, log *slog.Logger) *resolver {
	return &resolver{
		store:     st,
		log:       log,
		benchPool: benchPool,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				// The federated strategy makes N simultaneous calls to N hosts.
				// The Go default of 2 idle connections per host would serialize
				// repeated benchmark iterations and misattribute connection
				// setup to fan-out cost.
				MaxIdleConnsPerHost: 64,
				MaxIdleConns:        256,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (r *resolver) linkClient(endpoint string) doclinkv1connect.LinkResolverClient {
	if c, ok := r.linkClients.Load(endpoint); ok {
		return c.(doclinkv1connect.LinkResolverClient)
	}
	c := doclinkv1connect.NewLinkResolverClient(r.httpClient, endpoint)
	actual, _ := r.linkClients.LoadOrStore(endpoint, c)
	return actual.(doclinkv1connect.LinkResolverClient)
}

func (r *resolver) docClient(endpoint string) doclinkv1connect.DocumentResolverClient {
	if c, ok := r.docClients.Load(endpoint); ok {
		return c.(doclinkv1connect.DocumentResolverClient)
	}
	c := doclinkv1connect.NewDocumentResolverClient(r.httpClient, endpoint)
	actual, _ := r.docClients.LoadOrStore(endpoint, c)
	return actual.(doclinkv1connect.DocumentResolverClient)
}

func (r *resolver) GetLinks(ctx context.Context, anchor *doclinkv1.DocRef,
	strategy doclinkv1.ResolutionStrategy, refsOnly bool) (*doclinkv1.GetLinksResponse, error) {

	if strategy == doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_UNSPECIFIED {
		strategy = doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_FEDERATED
	}

	start := time.Now()
	var groups []*doclinkv1.LinkGroup
	var rpcs int
	var err error

	switch strategy {
	case doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_FEDERATED:
		groups, rpcs, err = r.federated(ctx, anchor, refsOnly)
	case doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_INDEXED:
		groups, rpcs, err = r.indexed(ctx, anchor, refsOnly)
	case doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_MONOLITH:
		groups, err = r.monolith(ctx, anchor)
	default:
		return nil, fmt.Errorf("unknown strategy %v", strategy)
	}
	if err != nil {
		return nil, err
	}

	// Stable ordering so benchmark diffs and UI card order do not flap with
	// goroutine scheduling.
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Namespace != groups[j].Namespace {
			return groups[i].Namespace < groups[j].Namespace
		}
		return groups[i].Predicate < groups[j].Predicate
	})

	return &doclinkv1.GetLinksResponse{
		Anchor:      anchor,
		Groups:      groups,
		Strategy:    strategy,
		TotalMicros: time.Since(start).Microseconds(),
		FanoutRpcs:  int32(rpcs),
	}, nil
}

// federated asks every service that declared an edge into this anchor type.
//
// The registry does not know what subscriptions or shipping are; it knows only
// that two rows in link_types name endpoints. Adding a third satellite adds a
// row and therefore a goroutine, with no code change anywhere.
func (r *resolver) federated(ctx context.Context, anchor *doclinkv1.DocRef, refsOnly bool) (
	[]*doclinkv1.LinkGroup, int, error) {

	plan, err := r.store.linkTypesForAnchor(ctx, docref.TypeKey(anchor))
	if err != nil {
		return nil, 0, fmt.Errorf("load fan-out plan: %w", err)
	}
	if len(plan) == 0 {
		return nil, 0, nil
	}

	groups := make([]*doclinkv1.LinkGroup, len(plan))
	var wg sync.WaitGroup
	for i, lt := range plan {
		wg.Add(1)
		go func(i int, lt linkTypeRow) {
			defer wg.Done()
			began := time.Now()
			g := &doclinkv1.LinkGroup{
				Namespace:   lt.namespace,
				Predicate:   lt.predicate,
				Cardinality: lt.cardinality,
			}
			resp, err := r.linkClient(lt.endpoint).ResolveLinks(ctx, connect.NewRequest(
				&doclinkv1.ResolveLinksRequest{Anchor: anchor, RefsOnly: refsOnly}))
			if err != nil {
				// One satellite being down must degrade one card, not blank the
				// item screen. The error travels to the UI inside the group so
				// the card can render its own failure state.
				g.Error = err.Error()
				r.log.Warn("satellite resolve failed",
					"namespace", lt.namespace, "endpoint", lt.endpoint, "err", err)
			} else {
				g.Documents = resp.Msg.Documents
				if resp.Msg.Predicate != "" {
					g.Predicate = resp.Msg.Predicate
				}
			}
			g.ResolveMicros = time.Since(began).Microseconds()
			groups[i] = g
		}(i, lt)
	}
	wg.Wait()

	return groups, len(plan), nil
}

// indexed reads the registry's own materialized index.
//
// The headline is one local query instead of N network calls. The asterisk,
// which the benchmark is designed to expose, is that the index stores refs and
// not summaries: unless the caller asks for refs_only, the registry still has to
// hydrate them through each owner's DocumentResolver. INDEXED is therefore only
// unambiguously cheaper when the caller can work with bare references.
func (r *resolver) indexed(ctx context.Context, anchor *doclinkv1.DocRef, refsOnly bool) (
	[]*doclinkv1.LinkGroup, int, error) {

	rows, err := r.store.indexLookup(ctx, docref.String(anchor))
	if err != nil {
		return nil, 0, fmt.Errorf("index lookup: %w", err)
	}
	if len(rows) == 0 {
		return nil, 0, nil
	}

	type key struct{ ns, predicate string }
	byGroup := map[key]*doclinkv1.LinkGroup{}
	var order []key

	for _, row := range rows {
		ref, err := docref.Parse(row.sourceRef)
		if err != nil {
			r.log.Warn("skipping malformed index entry", "source_ref", row.sourceRef)
			continue
		}
		k := key{row.namespace, row.predicate}
		g, ok := byGroup[k]
		if !ok {
			g = &doclinkv1.LinkGroup{
				Namespace:   row.namespace,
				Predicate:   row.predicate,
				Cardinality: row.cardinality,
			}
			byGroup[k] = g
			order = append(order, k)
		}
		g.Documents = append(g.Documents, &doclinkv1.DocSummary{Ref: ref})
	}

	out := make([]*doclinkv1.LinkGroup, 0, len(order))
	for _, k := range order {
		out = append(out, byGroup[k])
	}

	rpcs := 0
	if !refsOnly {
		began := time.Now()
		n, err := r.hydrateGroups(ctx, out)
		if err != nil {
			return nil, 0, err
		}
		rpcs = n
		for _, g := range out {
			g.ResolveMicros = time.Since(began).Microseconds()
		}
	}
	return out, rpcs, nil
}

// hydrateGroups turns bare refs into summaries by asking each owning service.
// Refs are batched per owner, so the cost is one call per distinct namespace
// rather than one per document.
func (r *resolver) hydrateGroups(ctx context.Context, groups []*doclinkv1.LinkGroup) (int, error) {
	endpoints, err := r.store.resolverEndpoints(ctx)
	if err != nil {
		return 0, fmt.Errorf("load resolver endpoints: %w", err)
	}

	byEndpoint := map[string][]*doclinkv1.DocSummary{}
	for _, g := range groups {
		for _, d := range g.Documents {
			ep, ok := endpoints[docref.TypeKey(d.Ref)]
			if !ok {
				d.Title = docref.String(d.Ref)
				d.Subtitle = "unregistered type"
				continue
			}
			byEndpoint[ep] = append(byEndpoint[ep], d)
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for ep, targets := range byEndpoint {
		wg.Add(1)
		go func(ep string, targets []*doclinkv1.DocSummary) {
			defer wg.Done()
			refs := make([]*doclinkv1.DocRef, len(targets))
			for i, t := range targets {
				refs[i] = t.Ref
			}
			resp, err := r.docClient(ep).Describe(ctx, connect.NewRequest(
				&doclinkv1.DescribeRequest{Refs: refs}))
			if err != nil {
				r.log.Warn("hydration failed", "endpoint", ep, "err", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			// Describe is specified to return summaries positionally.
			for i, s := range resp.Msg.Summaries {
				if i < len(targets) && s != nil {
					targets[i].Title = s.Title
					targets[i].Subtitle = s.Subtitle
					targets[i].Badges = s.Badges
					targets[i].Href = s.Href
					targets[i].Missing = s.Missing
				}
			}
		}(ep, targets)
	}
	wg.Wait()
	return len(byEndpoint), nil
}

// monolith is the straw-man: every satellite's data folded into the anchor's
// own schema, answered by one query with no network hops at all.
//
// It is hard-coded against subscriptions and shipping, and that is the point.
// This function is what the item team would have to edit, and redeploy, every
// time a new module wanted to attach something to an item.
func (r *resolver) monolith(ctx context.Context, anchor *doclinkv1.DocRef) (
	[]*doclinkv1.LinkGroup, error) {

	if r.benchPool == nil {
		return nil, fmt.Errorf("monolith strategy unavailable: no bench pool configured")
	}

	subs := &doclinkv1.LinkGroup{
		Namespace:   "subscriptions",
		Predicate:   "covers",
		Cardinality: doclinkv1.Cardinality_CARDINALITY_MANY_TO_MANY,
	}
	ship := &doclinkv1.LinkGroup{
		Namespace:   "shipping",
		Predicate:   "ships_via",
		Cardinality: doclinkv1.Cardinality_CARDINALITY_MANY_TO_ONE,
	}

	began := time.Now()
	// The link-level attributes (max quantity, prepaid) are selected here even
	// though they belong to subscriptions, because in the monolith they are
	// columns on a table in the anchor's schema. Omitting them would make the
	// baseline look artificially cheap next to a federated call that returns
	// them.
	rows, err := r.benchPool.Query(ctx,
		`SELECT p.id, p.name, p.interval, p.discount_bps,
		        ip.max_quantity_per_cycle, ip.prepaid_only
		 FROM monolith.item_plans ip
		 JOIN monolith.plans p ON p.id = ip.plan_id
		 WHERE ip.item_id = $1
		 ORDER BY p.name`, anchor.Id)
	if err != nil {
		return nil, fmt.Errorf("monolith plans: %w", err)
	}
	for rows.Next() {
		var id, name, interval string
		var bps, maxQty int32
		var prepaid bool
		if err := rows.Scan(&id, &name, &interval, &bps, &maxQty, &prepaid); err != nil {
			rows.Close()
			return nil, err
		}
		subs.Documents = append(subs.Documents, &doclinkv1.DocSummary{
			Ref:      docref.New("subscriptions", "plan", id),
			Title:    name,
			Subtitle: interval,
			Badges: map[string]string{
				"discount": fmt.Sprintf("%.2f%%", float64(bps)/100),
				"max_qty":  fmt.Sprintf("%d", maxQty),
				"prepaid":  fmt.Sprintf("%t", prepaid),
			},
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var profID, profName, carrier *string
	var handling *int32
	var weightG int32
	var hazmat bool
	err = r.benchPool.QueryRow(ctx,
		`SELECT pr.id, pr.name, pr.carrier, pr.handling_days,
		        i.shipping_weight_grams, i.shipping_hazmat
		 FROM monolith.items i
		 LEFT JOIN monolith.profiles pr ON pr.id = i.shipping_profile_id
		 WHERE i.id = $1`, anchor.Id).
		Scan(&profID, &profName, &carrier, &handling, &weightG, &hazmat)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("monolith shipping: %w", err)
	}
	if profID != nil {
		ship.Documents = append(ship.Documents, &doclinkv1.DocSummary{
			Ref:      docref.New("shipping", "profile", *profID),
			Title:    *profName,
			Subtitle: *carrier,
			Badges: map[string]string{
				"handling_days": fmt.Sprintf("%d", *handling),
				"weight_g":      fmt.Sprintf("%d", weightG),
				"hazmat":        fmt.Sprintf("%t", hazmat),
			},
		})
	}

	elapsed := time.Since(began).Microseconds()
	subs.ResolveMicros = elapsed
	ship.ResolveMicros = elapsed
	return []*doclinkv1.LinkGroup{subs, ship}, nil
}

// ResolveDocuments hydrates arbitrary refs on behalf of a caller that holds
// references to documents it does not own. This is how the subscriptions card
// renders an item title without importing PIM's schema.
func (r *resolver) ResolveDocuments(ctx context.Context, refs []*doclinkv1.DocRef) (
	[]*doclinkv1.DocSummary, error) {

	summaries := make([]*doclinkv1.DocSummary, len(refs))
	group := &doclinkv1.LinkGroup{}
	for i, ref := range refs {
		summaries[i] = &doclinkv1.DocSummary{Ref: ref}
		group.Documents = append(group.Documents, summaries[i])
	}
	if _, err := r.hydrateGroups(ctx, []*doclinkv1.LinkGroup{group}); err != nil {
		return nil, err
	}
	return summaries, nil
}
