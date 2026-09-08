package main

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	pimv1 "github.com/raveesh/doclink/gen/pim/v1"
)

// itemService implements pim.v1.ItemService.
//
// Read it looking for a mention of subscriptions, shipping, or the registry.
// There isn't one. PIM's only concession to the architecture is the outbox and
// the DocumentResolver below, and neither names a satellite.
type itemService struct{ store *store }

func (s *itemService) GetItem(ctx context.Context, req *connect.Request[pimv1.GetItemRequest]) (
	*connect.Response[pimv1.GetItemResponse], error) {
	it, err := s.store.get(ctx, req.Msg.Id)
	if errors.Is(err, errNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pimv1.GetItemResponse{Item: it}), nil
}

func (s *itemService) ListItems(ctx context.Context, req *connect.Request[pimv1.ListItemsRequest]) (
	*connect.Response[pimv1.ListItemsResponse], error) {
	size := int(req.Msg.PageSize)
	if size <= 0 || size > 200 {
		size = 50
	}
	offset := 0
	if t := req.Msg.PageToken; t != "" {
		if _, err := fmt.Sscanf(t, "%d", &offset); err != nil || offset < 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("invalid page_token %q", t))
		}
	}

	items, total, err := s.store.list(ctx, req.Msg.Query, size, offset)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	next := ""
	if offset+len(items) < total {
		next = fmt.Sprintf("%d", offset+len(items))
	}
	return connect.NewResponse(&pimv1.ListItemsResponse{
		Items: items, NextPageToken: next, Total: int32(total),
	}), nil
}

func (s *itemService) CreateItem(ctx context.Context, req *connect.Request[pimv1.CreateItemRequest]) (
	*connect.Response[pimv1.CreateItemResponse], error) {
	if req.Msg.Item == nil || req.Msg.Item.Sku == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("item.sku is required"))
	}
	it, err := s.store.create(ctx, req.Msg.Item)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pimv1.CreateItemResponse{Item: it}), nil
}

func (s *itemService) UpdateItem(ctx context.Context, req *connect.Request[pimv1.UpdateItemRequest]) (
	*connect.Response[pimv1.UpdateItemResponse], error) {
	if req.Msg.Item == nil || req.Msg.Item.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("item.id is required"))
	}
	it, err := s.store.update(ctx, req.Msg.Item)
	if errors.Is(err, errNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pimv1.UpdateItemResponse{Item: it}), nil
}

// DeleteItem removes the item and queues a lifecycle event in the same
// transaction. It does NOT wait for satellites to clean up, and does not ask
// them for permission: the anchor is not allowed to know they exist. Orphan
// rows in subscriptions and shipping are legal, transiently, and are each
// service's own responsibility to reconcile.
func (s *itemService) DeleteItem(ctx context.Context, req *connect.Request[pimv1.DeleteItemRequest]) (
	*connect.Response[pimv1.DeleteItemResponse], error) {
	eventID, err := s.store.delete(ctx, req.Msg.Id)
	if errors.Is(err, errNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pimv1.DeleteItemResponse{EventId: eventID}), nil
}

// documentResolver implements doclink.v1.DocumentResolver.
//
// This is the entire obligation of being an anchor document type: given
// references to my documents, describe them. Fifty lines, written once, and
// never touched again as satellites come and go.
type documentResolver struct{ store *store }

func (r *documentResolver) Describe(ctx context.Context, req *connect.Request[doclinkv1.DescribeRequest]) (
	*connect.Response[doclinkv1.DescribeResponse], error) {

	idList := make([]string, 0, len(req.Msg.Refs))
	for _, ref := range req.Msg.Refs {
		if ref.Namespace != "pim" || ref.Type != "item" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("pim does not own %s/%s", ref.Namespace, ref.Type))
		}
		idList = append(idList, ref.Id)
	}

	found, err := r.store.getMany(ctx, idList)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	summaries := make([]*doclinkv1.DocSummary, 0, len(req.Msg.Refs))
	for _, ref := range req.Msg.Refs {
		it, ok := found[ref.Id]
		if !ok {
			// A reference we cannot resolve is not an error. It is a satellite
			// holding a link to something that no longer exists, and saying so
			// plainly is how dangling references become visible without a
			// foreign key.
			summaries = append(summaries, &doclinkv1.DocSummary{
				Ref: ref, Title: "(deleted item)", Missing: true,
			})
			continue
		}
		summaries = append(summaries, &doclinkv1.DocSummary{
			Ref:      ref,
			Title:    it.Title,
			Subtitle: it.Sku,
			Href:     "/items/" + it.Id,
			Badges: map[string]string{
				"status": it.Status,
				"price":  fmt.Sprintf("%s %.2f", it.Currency, float64(it.PriceCents)/100),
			},
		})
	}
	return connect.NewResponse(&doclinkv1.DescribeResponse{Summaries: summaries}), nil
}
