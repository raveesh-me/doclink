// Package pim is the PIM core: product CRUD over SQLite, and the composition
// engine that fills Product.extensions from modules found in the registry.
//
// Nothing under this package may import internal/modules. That is enforced by
// no_module_imports_test.go, not by convention.
package pim

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"

	"connectrpc.com/connect"

	pimv1 "github.com/raveesh-me/doclink/gen/pim/v1"
	"github.com/raveesh-me/doclink/gen/pim/v1/pimv1connect"
)

// Composer turns core products into full products. The composition engine is
// the real implementation; PlainComposer serves core only.
type Composer interface {
	Compose(ctx context.Context, tenantID string, cores []*pimv1.ProductCore, reqCtx map[string]string) ([]*pimv1.Product, error)
}

// PlainComposer returns products with no extensions.
type PlainComposer struct{}

func (PlainComposer) Compose(_ context.Context, _ string, cores []*pimv1.ProductCore, _ map[string]string) ([]*pimv1.Product, error) {
	out := make([]*pimv1.Product, len(cores))
	for i, c := range cores {
		out[i] = &pimv1.Product{Core: c}
	}
	return out, nil
}

type Service struct {
	Store    *Store
	Composer Composer
	Log      *slog.Logger
}

var _ pimv1connect.PIMHandler = (*Service)(nil)

const (
	defaultPageSize = 50
	maxPageSize     = 500
)

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

func (s *Service) GetProduct(ctx context.Context, req *connect.Request[pimv1.GetProductRequest]) (*connect.Response[pimv1.GetProductResponse], error) {
	m := req.Msg
	if m.TenantId == "" || m.Id == "" {
		return nil, invalid("tenant_id and id are required")
	}
	core, err := s.Store.Get(ctx, m.TenantId, m.Id)
	if errors.Is(err, ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("product not found"))
	}
	if err != nil {
		return nil, s.internal("get product", err)
	}
	products, err := s.Composer.Compose(ctx, m.TenantId, []*pimv1.ProductCore{core}, m.Context)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pimv1.GetProductResponse{Product: products[0]}), nil
}

func (s *Service) ListProducts(ctx context.Context, req *connect.Request[pimv1.ListProductsRequest]) (*connect.Response[pimv1.ListProductsResponse], error) {
	m := req.Msg
	if m.TenantId == "" {
		return nil, invalid("tenant_id is required")
	}
	size := int(m.PageSize)
	switch {
	case size <= 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	after := ""
	if m.PageToken != "" {
		b, err := base64.RawURLEncoding.DecodeString(m.PageToken)
		if err != nil {
			return nil, invalid("malformed page_token")
		}
		after = string(b)
	}

	// Pagination is on core columns only. Module data cannot appear in a
	// predicate or an ordering here: it is not stored, so it cannot be queried.
	cores, err := s.Store.List(ctx, m.TenantId, after, size+1)
	if err != nil {
		return nil, s.internal("list products", err)
	}
	res := &pimv1.ListProductsResponse{}
	if len(cores) > size {
		cores = cores[:size]
		res.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(cores[size-1].Id))
	}
	if len(cores) == 0 {
		return connect.NewResponse(res), nil
	}
	res.Products, err = s.Composer.Compose(ctx, m.TenantId, cores, m.Context)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (s *Service) UpsertProduct(ctx context.Context, req *connect.Request[pimv1.UpsertProductRequest]) (*connect.Response[pimv1.UpsertProductResponse], error) {
	p := req.Msg.Product
	switch {
	case p == nil:
		return nil, invalid("product is required")
	case p.TenantId == "":
		return nil, invalid("product.tenant_id is required")
	case p.Sku == "" || p.Title == "":
		return nil, invalid("product.sku and product.title are required")
	case p.PriceMinor < 0:
		return nil, invalid("product.price_minor must not be negative")
	case !currencyRE.MatchString(p.Currency):
		return nil, invalid("product.currency must be an ISO 4217 code")
	}
	if p.Id == "" {
		p.Id = newID()
	}
	err := s.Store.Upsert(ctx, p)
	if errors.Is(err, ErrDuplicate) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if err != nil {
		return nil, s.internal("upsert product", err)
	}
	return connect.NewResponse(&pimv1.UpsertProductResponse{Product: p}), nil
}

func (s *Service) internal(op string, err error) error {
	s.Log.Error(op, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New(op+" failed"))
}

func invalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "p_" + hex.EncodeToString(b)
}
