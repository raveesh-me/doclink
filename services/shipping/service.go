package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	shippingv1 "github.com/raveesh/doclink/gen/shipping/v1"
	"github.com/raveesh/doclink/internal/docref"
	"github.com/raveesh/doclink/internal/registryclient"
)

const (
	predicate = "ships_via"
	cardinal  = doclinkv1.Cardinality_CARDINALITY_MANY_TO_ONE
)

type shippingService struct {
	store    *store
	registry *registryclient.Client
	log      *slog.Logger
}

func (s *shippingService) ListProfiles(ctx context.Context,
	_ *connect.Request[shippingv1.ListProfilesRequest],
) (*connect.Response[shippingv1.ListProfilesResponse], error) {
	profiles, err := s.store.listProfiles(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&shippingv1.ListProfilesResponse{Profiles: profiles}), nil
}

func (s *shippingService) GetAssignmentForAnchor(ctx context.Context,
	req *connect.Request[shippingv1.GetAssignmentForAnchorRequest],
) (*connect.Response[shippingv1.GetAssignmentForAnchorResponse], error) {

	if _, err := docref.Parse(req.Msg.AnchorRef); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	profiles, err := s.store.listProfiles(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &shippingv1.GetAssignmentForAnchorResponse{AvailableProfiles: profiles}

	a, err := s.store.assignmentForAnchor(ctx, req.Msg.AnchorRef)
	if errors.Is(err, errNoAssignment) {
		// Not an error: an unassigned item is the normal starting state, and
		// the card renders its empty form.
		return connect.NewResponse(out), nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out.Assignment = a
	for _, p := range profiles {
		if p.Id == a.ProfileId {
			out.Profile = p
			break
		}
	}
	return connect.NewResponse(out), nil
}

func (s *shippingService) AssignProfile(ctx context.Context,
	req *connect.Request[shippingv1.AssignProfileRequest],
) (*connect.Response[shippingv1.AssignProfileResponse], error) {

	anchor, err := docref.Parse(req.Msg.AnchorRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if req.Msg.ProfileId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("profile_id is required"))
	}

	// Reassignment replaces the previous row, so the old index entry has to go
	// too. Read before write, because the UNIQUE constraint means the upsert
	// destroys the evidence.
	var previous string
	if prev, err := s.store.assignmentForAnchor(ctx, req.Msg.AnchorRef); err == nil {
		previous = prev.ProfileId
	}

	a, err := s.store.assign(ctx, req.Msg.AnchorRef, req.Msg.ProfileId,
		req.Msg.WeightGrams, req.Msg.DimensionsCm, req.Msg.Hazmat)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if previous != "" && previous != a.ProfileId {
		s.registry.IndexEntry(ctx, anchor,
			docref.New("shipping", "profile", previous), predicate, cardinal, true)
	}
	s.registry.IndexEntry(ctx, anchor,
		docref.New("shipping", "profile", a.ProfileId), predicate, cardinal, false)

	return connect.NewResponse(&shippingv1.AssignProfileResponse{Assignment: a}), nil
}

func (s *shippingService) UnassignProfile(ctx context.Context,
	req *connect.Request[shippingv1.UnassignProfileRequest],
) (*connect.Response[shippingv1.UnassignProfileResponse], error) {

	profileID, err := s.store.unassign(ctx, req.Msg.AnchorRef)
	if errors.Is(err, errNoAssignment) {
		return connect.NewResponse(&shippingv1.UnassignProfileResponse{}), nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if anchor, err := docref.Parse(req.Msg.AnchorRef); err == nil {
		s.registry.IndexEntry(ctx, anchor,
			docref.New("shipping", "profile", profileID), predicate, cardinal, true)
	}
	return connect.NewResponse(&shippingv1.UnassignProfileResponse{}), nil
}

// ---------------------------------------------------------------------------
// doclink.v1.LinkResolver
// ---------------------------------------------------------------------------

type linkResolver struct {
	store *store
	log   *slog.Logger
}

func (r *linkResolver) ResolveLinks(ctx context.Context,
	req *connect.Request[doclinkv1.ResolveLinksRequest],
) (*connect.Response[doclinkv1.ResolveLinksResponse], error) {

	out := &doclinkv1.ResolveLinksResponse{Predicate: predicate, Cardinality: cardinal}

	a, err := r.store.assignmentForAnchor(ctx, docref.String(req.Msg.Anchor))
	if errors.Is(err, errNoAssignment) {
		return connect.NewResponse(out), nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	ref := docref.New("shipping", "profile", a.ProfileId)
	if req.Msg.RefsOnly {
		out.Documents = []*doclinkv1.DocSummary{{Ref: ref}}
		return connect.NewResponse(out), nil
	}

	profiles, err := r.store.profilesByID(ctx, []string{a.ProfileId})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	p, ok := profiles[a.ProfileId]
	if !ok {
		return connect.NewResponse(out), nil
	}
	out.Documents = []*doclinkv1.DocSummary{{
		Ref:      ref,
		Title:    p.Name,
		Subtitle: p.Carrier,
		Href:     "/profiles/" + p.Id,
		Badges: map[string]string{
			"handling_days": fmt.Sprintf("%d", p.HandlingDays),
			"weight_g":      fmt.Sprintf("%d", a.WeightGrams),
			"hazmat":        fmt.Sprintf("%t", a.Hazmat),
		},
	}}
	return connect.NewResponse(out), nil
}

func (r *linkResolver) HandleLifecycle(ctx context.Context,
	req *connect.Request[doclinkv1.HandleLifecycleRequest],
) (*connect.Response[doclinkv1.HandleLifecycleResponse], error) {

	if req.Msg.Event.Kind != doclinkv1.LifecycleKind_LIFECYCLE_KIND_DELETED {
		return connect.NewResponse(&doclinkv1.HandleLifecycleResponse{}), nil
	}
	anchorRef := docref.String(req.Msg.Event.Subject)
	n, err := r.store.purgeAnchor(ctx, anchorRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if n > 0 {
		r.log.Info("reconciled orphaned assignment", "anchor", anchorRef, "rows", n)
	}
	return connect.NewResponse(&doclinkv1.HandleLifecycleResponse{RowsAffected: int32(n)}), nil
}

// ---------------------------------------------------------------------------
// doclink.v1.DocumentResolver
// ---------------------------------------------------------------------------

type documentResolver struct{ store *store }

func (d *documentResolver) Describe(ctx context.Context,
	req *connect.Request[doclinkv1.DescribeRequest],
) (*connect.Response[doclinkv1.DescribeResponse], error) {

	idList := make([]string, 0, len(req.Msg.Refs))
	for _, ref := range req.Msg.Refs {
		if ref.Namespace != "shipping" || ref.Type != "profile" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("shipping does not own %s/%s", ref.Namespace, ref.Type))
		}
		idList = append(idList, ref.Id)
	}
	profiles, err := d.store.profilesByID(ctx, idList)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	summaries := make([]*doclinkv1.DocSummary, 0, len(req.Msg.Refs))
	for _, ref := range req.Msg.Refs {
		p, ok := profiles[ref.Id]
		if !ok {
			summaries = append(summaries, &doclinkv1.DocSummary{
				Ref: ref, Title: "(deleted profile)", Missing: true,
			})
			continue
		}
		summaries = append(summaries, &doclinkv1.DocSummary{
			Ref: ref, Title: p.Name, Subtitle: p.Carrier, Href: "/profiles/" + p.Id,
			Badges: map[string]string{"handling_days": fmt.Sprintf("%d", p.HandlingDays)},
		})
	}
	return connect.NewResponse(&doclinkv1.DescribeResponse{Summaries: summaries}), nil
}
