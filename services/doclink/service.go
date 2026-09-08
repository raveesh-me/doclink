package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"github.com/raveesh/doclink/internal/docref"
	"github.com/raveesh/doclink/internal/ids"
)

// registryService implements doclink.v1.RegistryService.
//
// Every method here operates on declarations or on data supplied by callers.
// The service has no compiled-in knowledge of pim, subscriptions, or shipping,
// and adding a fourth module requires no change to this file.
type registryService struct {
	store      *store
	resolver   *resolver
	dispatcher *dispatcher
}

func (s *registryService) RegisterDocumentType(ctx context.Context,
	req *connect.Request[doclinkv1.RegisterDocumentTypeRequest],
) (*connect.Response[doclinkv1.RegisterDocumentTypeResponse], error) {
	dt := req.Msg.DocumentType
	if dt == nil || dt.Namespace == "" || dt.Type == "" || dt.ResolverEndpoint == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("namespace, type and resolver_endpoint are required"))
	}
	if err := s.store.putDocumentType(ctx, dt); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.RegisterDocumentTypeResponse{DocumentType: dt}), nil
}

func (s *registryService) RegisterExtensionPoint(ctx context.Context,
	req *connect.Request[doclinkv1.RegisterExtensionPointRequest],
) (*connect.Response[doclinkv1.RegisterExtensionPointResponse], error) {
	ep := req.Msg.ExtensionPoint
	if ep == nil || ep.Id == "" || ep.AnchorType == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("id and anchor_type are required"))
	}
	if err := s.store.putExtensionPoint(ctx, ep); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.RegisterExtensionPointResponse{ExtensionPoint: ep}), nil
}

// RegisterContribution deliberately does not verify that the extension point
// exists. A satellite may boot before the host that owns the slot, and refusing
// its registration would make startup order significant. Contributions to an
// unknown slot are simply never listed.
func (s *registryService) RegisterContribution(ctx context.Context,
	req *connect.Request[doclinkv1.RegisterContributionRequest],
) (*connect.Response[doclinkv1.RegisterContributionResponse], error) {
	c := req.Msg.Contribution
	if c == nil || c.ExtensionPointId == "" || c.Namespace == "" || c.EmbedUrl == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("extension_point_id, namespace and embed_url are required"))
	}
	if c.Id == "" {
		c.Id = ids.Prefixed("con")
	}
	if err := s.store.putContribution(ctx, c); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.RegisterContributionResponse{Contribution: c}), nil
}

func (s *registryService) RegisterLinkType(ctx context.Context,
	req *connect.Request[doclinkv1.RegisterLinkTypeRequest],
) (*connect.Response[doclinkv1.RegisterLinkTypeResponse], error) {
	lt := req.Msg.LinkType
	if lt == nil || lt.AnchorType == nil || lt.SourceType == nil ||
		lt.Predicate == "" || lt.ResolverEndpoint == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("anchor_type, source_type, predicate and resolver_endpoint are required"))
	}
	if lt.Id == "" {
		lt.Id = ids.Prefixed("lnk")
	}
	if err := s.store.putLinkType(ctx, lt); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.RegisterLinkTypeResponse{LinkType: lt}), nil
}

// ListContributions is the call the item screen makes to discover its own
// cards. It is the runtime replacement for an import statement.
func (s *registryService) ListContributions(ctx context.Context,
	req *connect.Request[doclinkv1.ListContributionsRequest],
) (*connect.Response[doclinkv1.ListContributionsResponse], error) {
	cons, err := s.store.listContributions(ctx, req.Msg.ExtensionPointId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.ListContributionsResponse{Contributions: cons}), nil
}

func (s *registryService) ListLinkTypes(ctx context.Context,
	req *connect.Request[doclinkv1.ListLinkTypesRequest],
) (*connect.Response[doclinkv1.ListLinkTypesResponse], error) {
	key := ""
	if a := req.Msg.AnchorType; a != nil && a.Namespace != "" {
		key = a.Namespace + "/" + a.Type
	}
	rows, err := s.store.linkTypesForAnchor(ctx, key)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*doclinkv1.LinkTypeDecl, 0, len(rows))
	for _, r := range rows {
		ans, ant := splitTypeKey(r.anchorKey)
		sns, st := splitTypeKey(r.sourceKey)
		out = append(out, &doclinkv1.LinkTypeDecl{
			Id:                    r.id,
			AnchorType:            &doclinkv1.DocRef{Namespace: ans, Type: ant},
			SourceType:            &doclinkv1.DocRef{Namespace: sns, Type: st},
			Predicate:             r.predicate,
			Cardinality:           r.cardinality,
			ResolverEndpoint:      r.endpoint,
			SubscribesToLifecycle: r.lifecycle,
		})
	}
	return connect.NewResponse(&doclinkv1.ListLinkTypesResponse{LinkTypes: out}), nil
}

func (s *registryService) ResolveDocuments(ctx context.Context,
	req *connect.Request[doclinkv1.ResolveDocumentsRequest],
) (*connect.Response[doclinkv1.ResolveDocumentsResponse], error) {
	summaries, err := s.resolver.ResolveDocuments(ctx, req.Msg.Refs)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.ResolveDocumentsResponse{Summaries: summaries}), nil
}

func (s *registryService) GetLinks(ctx context.Context,
	req *connect.Request[doclinkv1.GetLinksRequest],
) (*connect.Response[doclinkv1.GetLinksResponse], error) {
	if req.Msg.Anchor == nil || req.Msg.Anchor.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("anchor is required"))
	}
	resp, err := s.resolver.GetLinks(ctx, req.Msg.Anchor, req.Msg.Strategy, req.Msg.RefsOnly)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

func (s *registryService) UpsertIndexEntry(ctx context.Context,
	req *connect.Request[doclinkv1.UpsertIndexEntryRequest],
) (*connect.Response[doclinkv1.UpsertIndexEntryResponse], error) {
	m := req.Msg
	if m.Anchor == nil || m.Source == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("anchor and source are required"))
	}
	seq, err := s.store.upsertIndex(ctx,
		docref.String(m.Anchor), docref.String(m.Source), m.Predicate,
		cardinalityName(m.Cardinality), m.Source.Namespace, m.Deleted)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.UpsertIndexEntryResponse{Sequence: seq}), nil
}

func (s *registryService) PublishLifecycle(ctx context.Context,
	req *connect.Request[doclinkv1.PublishLifecycleRequest],
) (*connect.Response[doclinkv1.PublishLifecycleResponse], error) {
	ev := req.Msg.Event
	if ev == nil || ev.Subject == nil || ev.EventId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("event_id and subject are required"))
	}
	n, err := s.dispatcher.Publish(ctx, ev)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.PublishLifecycleResponse{Subscribers: int32(n)}), nil
}

// Deregister removes a service's declarations. Not authenticated in this POC,
// which is fine for a benchmark harness and obviously not fine for production:
// anything that can call this can make a satellite disappear from every item
// screen at once.
func (s *registryService) Deregister(ctx context.Context,
	req *connect.Request[doclinkv1.DeregisterRequest],
) (*connect.Response[doclinkv1.DeregisterResponse], error) {
	n, err := s.store.deregister(ctx,
		req.Msg.LinkTypeId, req.Msg.ContributionId, req.Msg.DocumentTypeKey)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&doclinkv1.DeregisterResponse{Removed: int32(n)}), nil
}
