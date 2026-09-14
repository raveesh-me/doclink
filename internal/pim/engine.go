package pim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	extv1 "github.com/raveesh-me/doclink/gen/ext/v1"
	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	pimv1 "github.com/raveesh-me/doclink/gen/pim/v1"
	registryv1 "github.com/raveesh-me/doclink/gen/registry/v1"
	"github.com/raveesh-me/doclink/internal/registry"
)

const (
	enrichFamily     = "product.enrich"
	maxFragmentBytes = 64 << 10
)

// ModuleSource is the slice of the registry the engine reads.
type ModuleSource interface {
	ActiveModules(ctx context.Context, tenantID, family string) ([]registry.Module, error)
}

// Engine composes products from core rows plus whatever the tenant's modules
// return. It knows modules only as registry rows: a base URL, a policy, and a
// JSON Schema per extension point. It has one client type, for the contract
// PIM owns, and points it at N URLs.
type Engine struct {
	Modules    ModuleSource
	Schemas    *registry.Schemas
	HTTPClient connect.HTTPClient
	Log        *slog.Logger
	Breakers   *Breakers // optional
	Cache      *Cache    // optional

	clientsMu sync.Mutex
	clients   map[string]extv1connect.ModuleExtensionClient
}

var _ Composer = (*Engine)(nil)

// outcome is what one module contributed to one read.
type outcome struct {
	module registry.Module
	// fields is the extension object to serve per product. It can be set for a
	// degraded product, when a stale cache entry stands in.
	fields map[string]map[string]*structpb.Value
	// degraded holds, per product, why this module failed it.
	degraded map[string]error
}

// reason picks one failure to report, deterministically.
func (o *outcome) reason() error {
	ids := make([]string, 0, len(o.degraded))
	for id := range o.degraded {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return o.degraded[ids[0]]
}

// fetch is the result of one EnrichProducts call.
type fetch struct {
	// err is set when the whole call failed: transport, timeout, a response
	// naming products that were not asked for.
	err      error
	points   map[string]pointData
	rejected map[string]error
}

func (f *fetch) failed() bool { return f.err != nil || len(f.rejected) > 0 }

func (e *Engine) Compose(ctx context.Context, tenantID string, cores []*pimv1.ProductCore, reqCtx map[string]string) ([]*pimv1.Product, error) {
	products := make([]*pimv1.Product, len(cores))
	for i, c := range cores {
		products[i] = &pimv1.Product{Core: c}
	}

	modules, err := e.Modules.ActiveModules(ctx, tenantID, enrichFamily)
	if err != nil {
		e.Log.Error("load modules", "tenant", tenantID, "err", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("load modules failed"))
	}
	if len(modules) == 0 {
		return products, nil
	}

	// One goroutine per module, at most one batched call per goroutine. A
	// FAIL_CLOSED failure returns an error, which cancels the others: the read
	// is lost anyway.
	outcomes := make([]*outcome, len(modules))
	g, gctx := errgroup.WithContext(ctx)
	for i, m := range modules {
		g.Go(func() error {
			o := e.resolve(gctx, m, tenantID, cores, reqCtx)
			outcomes[i] = o
			if len(o.degraded) > 0 && m.FailureMode == registryv1.FailureMode_FAIL_CLOSED {
				return connect.NewError(connect.CodeUnavailable,
					fmt.Errorf("module %q (FAIL_CLOSED) failed: %v", m.ModuleID, o.reason()))
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Modules arrive sorted by module_id, so degraded is sorted too.
	for _, p := range products {
		id := p.Core.Id
		for _, o := range outcomes {
			if o.degraded[id] != nil {
				p.Degraded = append(p.Degraded, o.module.ModuleID)
			}
			if f := o.fields[id]; len(f) > 0 {
				if p.Extensions == nil {
					p.Extensions = map[string]*structpb.Struct{}
				}
				// The key comes from the registry row. Nothing the module
				// sent can choose it.
				p.Extensions[o.module.ModuleID] = &structpb.Struct{Fields: f}
			}
		}
	}
	return products, nil
}

// resolve serves a module's contribution from the cache where it can, and
// makes one batched call for the products it cannot.
func (e *Engine) resolve(ctx context.Context, m registry.Module, tenantID string, cores []*pimv1.ProductCore, reqCtx map[string]string) *outcome {
	o := &outcome{module: m, fields: map[string]map[string]*structpb.Value{}, degraded: map[string]error{}}
	log := e.Log.With("tenant", tenantID, "module", m.ModuleID)

	var hashes map[string]string
	var generation uint64
	misses := cores
	if e.Cache != nil {
		hashes = contextHashes(m, reqCtx)
		generation = e.Cache.Generation(tenantID, m.ModuleID)
		misses = nil
		for _, c := range cores {
			if pts, ok := e.Cache.Fresh(tenantID, m, c.Id, hashes); ok {
				o.set(c.Id, m, pts)
				continue
			}
			misses = append(misses, c)
		}
		log.Debug("cache", "hits", len(cores)-len(misses), "misses", len(misses))
	}
	if len(misses) == 0 {
		return o
	}

	f := e.call(ctx, m, tenantID, misses, reqCtx)
	for _, c := range misses {
		err := f.err
		if err == nil {
			err = f.rejected[c.Id]
		}
		if err == nil {
			pts := f.points[c.Id]
			if e.Cache != nil {
				e.Cache.Put(tenantID, m, c.Id, hashes, pts, generation)
			}
			o.set(c.Id, m, pts)
			continue
		}
		o.degraded[c.Id] = err
		// A slightly old tax class beats no tax class on a catalogue browse.
		// Still degraded, so the consumer knows. Never under FAIL_CLOSED: that
		// tenant has said it wants no answer rather than a possibly wrong one.
		if e.Cache != nil && m.FailureMode == registryv1.FailureMode_FAIL_OPEN {
			if pts, ok := e.Cache.Stale(tenantID, m, c.Id, hashes); ok {
				o.set(c.Id, m, pts)
				log.Warn("serving stale fragments", "product", c.Id)
			}
		}
	}
	return o
}

// set merges a product's per-point fragments into one extension object.
// Registration guarantees no two points declare the same property.
func (o *outcome) set(productID string, m registry.Module, pts pointData) {
	merged := map[string]*structpb.Value{}
	for _, p := range m.Points {
		if data := pts[p.Name]; data != nil {
			for k, v := range data.Fields {
				merged[k] = v
			}
		}
	}
	if len(merged) > 0 {
		o.fields[productID] = merged
	}
}

// call consults the module's breaker around enrich.
func (e *Engine) call(ctx context.Context, m registry.Module, tenantID string, cores []*pimv1.ProductCore, reqCtx map[string]string) *fetch {
	if e.Breakers == nil {
		return e.enrich(ctx, m, tenantID, cores, reqCtx)
	}
	if !e.Breakers.Allow(tenantID, m.ModuleID) {
		return &fetch{err: ErrCircuitOpen}
	}

	// Whether the read's remaining budget, not the module's own timeout, is the
	// binding deadline. Decided before the call: afterwards, the module's timer
	// and the parent's can fire in either order, so ctx.Err() alone races.
	dl, hasDeadline := ctx.Deadline()
	capped := hasDeadline && time.Until(dl) <= m.Timeout

	f := e.enrich(ctx, m, tenantID, cores, reqCtx)
	timedOut := connect.CodeOf(f.err) == connect.CodeDeadlineExceeded
	if ctx.Err() != nil || (capped && timedOut) {
		// The read itself was cancelled or out of budget, or a FAIL_CLOSED
		// sibling failed. Counting that against this module would let a
		// caller with a 1ms deadline trip every breaker a tenant has.
		e.Breakers.Abandon(tenantID, m.ModuleID)
	} else {
		e.Breakers.Record(tenantID, m.ModuleID, !f.failed())
	}
	return f
}

// enrich makes the one batched EnrichProducts call for a module and vets the
// response.
func (e *Engine) enrich(ctx context.Context, m registry.Module, tenantID string, cores []*pimv1.ProductCore, reqCtx map[string]string) *fetch {
	log := e.Log.With("tenant", tenantID, "module", m.ModuleID)

	// Derived from the parent: if the read has 200ms left and the module is
	// allowed 500ms, it gets 200ms. Connect forwards the deadline to the module.
	cctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()

	start := time.Now()
	res, err := e.client(m.BaseURL).EnrichProducts(cctx, connect.NewRequest(&extv1.EnrichProductsRequest{
		TenantId: tenantID,
		Products: cores,
		Context:  reqCtx,
	}))
	elapsed := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			// Not this module's doing; say why rather than blame it.
			log.Info("module call abandoned", "elapsed", elapsed, "cause", context.Cause(ctx))
		} else {
			log.Warn("module call failed", "products", len(cores), "elapsed", elapsed, "err", err)
		}
		return &fetch{err: fmt.Errorf("EnrichProducts: %w", err)}
	}
	log.Debug("module call", "products", len(cores), "elapsed", elapsed)

	f := e.vet(m, cores, res.Msg)
	if f.err != nil {
		log.Warn("module response rejected", "err", f.err)
	}
	for pid, reason := range f.rejected {
		log.Warn("fragments dropped", "product", pid, "reason", reason)
	}
	return f
}

// vet enforces everything PIM does not take on trust: that each fragment is
// from the module it is registered as, names a registered point, fits the size
// cap, and validates against the registered schema. A product with any bad
// fragment loses the module's whole extension, rather than serving half an
// object that matches no schema the consumer was given.
func (e *Engine) vet(m registry.Module, cores []*pimv1.ProductCore, res *extv1.EnrichProductsResponse) *fetch {
	byRef := make(map[string]registry.Point, len(m.Points))
	for _, p := range m.Points {
		byRef[p.SchemaRef] = p
	}
	requested := make(map[string]bool, len(cores))
	for _, c := range cores {
		requested[c.Id] = true
	}
	for pid := range res.ByProductId {
		if !requested[pid] {
			return &fetch{err: fmt.Errorf("response includes product %q, which was not requested", pid)}
		}
	}

	f := &fetch{points: map[string]pointData{}, rejected: map[string]error{}}
	for _, c := range cores {
		set := res.ByProductId[c.Id]
		if set == nil {
			continue // the module has nothing for this product
		}
		pts, err := e.vetProduct(m, byRef, set)
		if err != nil {
			f.rejected[c.Id] = err
			continue
		}
		f.points[c.Id] = pts
	}
	return f
}

func (e *Engine) vetProduct(m registry.Module, byRef map[string]registry.Point, set *extv1.FragmentSet) (pointData, error) {
	pts := pointData{}
	props := map[string]bool{}
	for _, f := range set.Fragments {
		if f.ModuleId != m.ModuleID {
			return nil, fmt.Errorf("fragment claims module_id %q but the module is registered as %q", f.ModuleId, m.ModuleID)
		}
		point, ok := byRef[f.SchemaRef]
		if !ok {
			return nil, fmt.Errorf("fragment schema_ref %q is not a registered point of %q", f.SchemaRef, m.ModuleID)
		}
		if pts[point.Name] != nil {
			return nil, fmt.Errorf("more than one fragment for schema_ref %q", f.SchemaRef)
		}
		if f.Data == nil {
			return nil, fmt.Errorf("fragment %q has no data", f.SchemaRef)
		}
		if n := proto.Size(f.Data); n > maxFragmentBytes {
			return nil, fmt.Errorf("fragment %q is %d bytes, over the %d byte cap", f.SchemaRef, n, maxFragmentBytes)
		}
		sch, err := e.Schemas.Compile(point.JSONSchema)
		if err != nil {
			return nil, fmt.Errorf("registered schema %q no longer compiles: %w", f.SchemaRef, err)
		}
		if err := sch.Validate(f.Data.AsMap()); err != nil {
			return nil, fmt.Errorf("fragment violates %s: %w", f.SchemaRef, err)
		}
		for k := range f.Data.Fields {
			if props[k] {
				return nil, fmt.Errorf("property %q is set by more than one fragment", k)
			}
			props[k] = true
		}
		pts[point.Name] = f.Data
	}
	return pts, nil
}

func (e *Engine) client(baseURL string) extv1connect.ModuleExtensionClient {
	e.clientsMu.Lock()
	defer e.clientsMu.Unlock()
	if c, ok := e.clients[baseURL]; ok {
		return c
	}
	if e.clients == nil {
		e.clients = map[string]extv1connect.ModuleExtensionClient{}
	}
	c := extv1connect.NewModuleExtensionClient(e.HTTPClient, baseURL)
	e.clients[baseURL] = c
	return c
}
