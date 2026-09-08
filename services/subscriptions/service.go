package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	subscriptionsv1 "github.com/raveesh/doclink/gen/subscriptions/v1"
	"github.com/raveesh/doclink/internal/docref"
	"github.com/raveesh/doclink/internal/registryclient"
)

const (
	predicate = "covers"
	cardinal  = doclinkv1.Cardinality_CARDINALITY_MANY_TO_MANY
)

type subscriptionsService struct {
	store    *store
	registry *registryclient.Client
	log      *slog.Logger
}

func (s *subscriptionsService) ListPlans(ctx context.Context,
	_ *connect.Request[subscriptionsv1.ListPlansRequest],
) (*connect.Response[subscriptionsv1.ListPlansResponse], error) {
	plans, err := s.store.listPlans(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&subscriptionsv1.ListPlansResponse{Plans: plans}), nil
}

// ListCoverageForAnchor is what the embedded card calls. Its only input is the
// DocRef string the host handed the iframe. Everything the card displays is
// derived from that one value plus this service's own tables.
func (s *subscriptionsService) ListCoverageForAnchor(ctx context.Context,
	req *connect.Request[subscriptionsv1.ListCoverageForAnchorRequest],
) (*connect.Response[subscriptionsv1.ListCoverageForAnchorResponse], error) {

	if _, err := docref.Parse(req.Msg.AnchorRef); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	cov, err := s.store.coverageForAnchor(ctx, req.Msg.AnchorRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	plans, err := s.store.listPlans(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&subscriptionsv1.ListCoverageForAnchorResponse{
		Coverage: cov, Plans: plans,
	}), nil
}

func (s *subscriptionsService) AttachPlan(ctx context.Context,
	req *connect.Request[subscriptionsv1.AttachPlanRequest],
) (*connect.Response[subscriptionsv1.AttachPlanResponse], error) {

	anchor, err := docref.Parse(req.Msg.AnchorRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if req.Msg.PlanId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("plan_id is required"))
	}

	// Note what is NOT here: no call to PIM to check the item exists. Doing so
	// would make subscriptions depend on the anchor's availability at write
	// time, which is the coupling this design is removing. A link to a
	// nonexistent item is allowed, and shows up as a missing summary on read.
	cov, err := s.store.attach(ctx, req.Msg.AnchorRef, req.Msg.PlanId,
		req.Msg.MaxQuantityPerCycle, req.Msg.PrepaidOnly)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Mirror into the registry's index, after our own commit. Best effort by
	// construction: see registryclient.IndexEntry.
	s.registry.IndexEntry(ctx, anchor,
		docref.New("subscriptions", "plan", cov.PlanId), predicate, cardinal, false)

	return connect.NewResponse(&subscriptionsv1.AttachPlanResponse{Coverage: cov}), nil
}

func (s *subscriptionsService) DetachPlan(ctx context.Context,
	req *connect.Request[subscriptionsv1.DetachPlanRequest],
) (*connect.Response[subscriptionsv1.DetachPlanResponse], error) {

	cov, err := s.store.detach(ctx, req.Msg.CoverageId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if anchor, err := docref.Parse(cov.AnchorRef); err == nil {
		s.registry.IndexEntry(ctx, anchor,
			docref.New("subscriptions", "plan", cov.PlanId), predicate, cardinal, true)
	}
	return connect.NewResponse(&subscriptionsv1.DetachPlanResponse{}), nil
}

// ---------------------------------------------------------------------------
// doclink.v1.LinkResolver — the contract that makes this service discoverable
// ---------------------------------------------------------------------------

type linkResolver struct {
	store *store
	log   *slog.Logger
}

// ResolveLinks answers "what does subscriptions have for this item?" and is the
// call the registry fans out to. It returns fully-formed summaries rather than
// bare ids, so the registry never needs to understand what a plan is.
func (r *linkResolver) ResolveLinks(ctx context.Context,
	req *connect.Request[doclinkv1.ResolveLinksRequest],
) (*connect.Response[doclinkv1.ResolveLinksResponse], error) {

	anchorRef := docref.String(req.Msg.Anchor)
	cov, err := r.store.coverageForAnchor(ctx, anchorRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	out := &doclinkv1.ResolveLinksResponse{Predicate: predicate, Cardinality: cardinal}
	if len(cov) == 0 {
		return connect.NewResponse(out), nil
	}

	if req.Msg.RefsOnly {
		for _, c := range cov {
			out.Documents = append(out.Documents, &doclinkv1.DocSummary{
				Ref: docref.New("subscriptions", "plan", c.PlanId),
			})
		}
		return connect.NewResponse(out), nil
	}

	planIDs := make([]string, len(cov))
	for i, c := range cov {
		planIDs[i] = c.PlanId
	}
	plans, err := r.store.plansByID(ctx, planIDs)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	for _, c := range cov {
		p, ok := plans[c.PlanId]
		if !ok {
			continue
		}
		out.Documents = append(out.Documents, &doclinkv1.DocSummary{
			Ref:      docref.New("subscriptions", "plan", p.Id),
			Title:    p.Name,
			Subtitle: p.Interval,
			Href:     "/plans/" + p.Id,
			Badges: map[string]string{
				"discount": fmt.Sprintf("%.2f%%", float64(p.DiscountBps)/100),
				"max_qty":  fmt.Sprintf("%d", c.MaxQuantityPerCycle),
				"prepaid":  fmt.Sprintf("%t", c.PrepaidOnly),
			},
		})
	}
	return connect.NewResponse(out), nil
}

// HandleLifecycle is this service's answer to a foreign key it is not allowed
// to have. Idempotent: the registry retries, and a redelivered DELETE simply
// removes nothing the second time.
func (r *linkResolver) HandleLifecycle(ctx context.Context,
	req *connect.Request[doclinkv1.HandleLifecycleRequest],
) (*connect.Response[doclinkv1.HandleLifecycleResponse], error) {

	ev := req.Msg.Event
	if ev.Kind != doclinkv1.LifecycleKind_LIFECYCLE_KIND_DELETED {
		// CREATED and UPDATED carry no obligation for us: we hold no copy of
		// the item's data, only a reference to it.
		return connect.NewResponse(&doclinkv1.HandleLifecycleResponse{}), nil
	}

	anchorRef := docref.String(ev.Subject)
	n, err := r.store.purgeAnchor(ctx, anchorRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if n > 0 {
		r.log.Info("reconciled orphaned coverage", "anchor", anchorRef, "rows", n)
	}
	return connect.NewResponse(&doclinkv1.HandleLifecycleResponse{RowsAffected: int32(n)}), nil
}

// ---------------------------------------------------------------------------
// doclink.v1.DocumentResolver — so others can describe OUR documents
// ---------------------------------------------------------------------------

// documentResolver exists because the INDEXED strategy stores bare refs. When
// the registry reads a plan ref out of its index it has to ask someone what
// that plan is called, and this is who it asks.
type documentResolver struct{ store *store }

func (d *documentResolver) Describe(ctx context.Context,
	req *connect.Request[doclinkv1.DescribeRequest],
) (*connect.Response[doclinkv1.DescribeResponse], error) {

	idList := make([]string, 0, len(req.Msg.Refs))
	for _, ref := range req.Msg.Refs {
		if ref.Namespace != "subscriptions" || ref.Type != "plan" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("subscriptions does not own %s/%s", ref.Namespace, ref.Type))
		}
		idList = append(idList, ref.Id)
	}
	plans, err := d.store.plansByID(ctx, idList)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	summaries := make([]*doclinkv1.DocSummary, 0, len(req.Msg.Refs))
	for _, ref := range req.Msg.Refs {
		p, ok := plans[ref.Id]
		if !ok {
			summaries = append(summaries, &doclinkv1.DocSummary{
				Ref: ref, Title: "(deleted plan)", Missing: true,
			})
			continue
		}
		summaries = append(summaries, &doclinkv1.DocSummary{
			Ref:      ref,
			Title:    p.Name,
			Subtitle: p.Interval,
			Href:     "/plans/" + p.Id,
			Badges: map[string]string{
				"discount": fmt.Sprintf("%.2f%%", float64(p.DiscountBps)/100),
			},
		})
	}
	return connect.NewResponse(&doclinkv1.DescribeResponse{Summaries: summaries}), nil
}
