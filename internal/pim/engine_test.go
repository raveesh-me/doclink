package pim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	extv1 "github.com/raveesh-me/doclink/gen/ext/v1"
	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	pimv1 "github.com/raveesh-me/doclink/gen/pim/v1"
	registryv1 "github.com/raveesh-me/doclink/gen/registry/v1"
	"github.com/raveesh-me/doclink/internal/registry"
)

// These tests use fake modules defined here, speaking only the ext/v1 contract.
// They must not use the real modules: that would import internal/modules, and
// no_module_imports_test.go walks test files too.

type fakeModule struct {
	extv1connect.UnimplementedModuleExtensionHandler
	calls    atomic.Int64
	products atomic.Int64
	enrich   func(context.Context, *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error)
}

func (f *fakeModule) EnrichProducts(ctx context.Context, req *connect.Request[extv1.EnrichProductsRequest]) (*connect.Response[extv1.EnrichProductsResponse], error) {
	f.calls.Add(1)
	f.products.Add(int64(len(req.Msg.Products)))
	res, err := f.enrich(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (f *fakeModule) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(extv1connect.NewModuleExtensionHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// each builds a response by calling fn for every requested product.
func each(moduleID, ref string, fn func(*pimv1.ProductCore) map[string]any) func(context.Context, *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
	return func(_ context.Context, req *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		res := &extv1.EnrichProductsResponse{ByProductId: map[string]*extv1.FragmentSet{}}
		for _, p := range req.Products {
			data, err := structpb.NewStruct(fn(p))
			if err != nil {
				return nil, err
			}
			res.ByProductId[p.Id] = &extv1.FragmentSet{Fragments: []*extv1.Fragment{{ModuleId: moduleID, SchemaRef: ref, Data: data}}}
		}
		return res, nil
	}
}

type staticModules []registry.Module

func (s staticModules) ActiveModules(context.Context, string, string) ([]registry.Module, error) {
	return s, nil
}

const seatsSchema = `{"type":"object","additionalProperties":false,"required":["seats"],
	"properties":{"seats":{"type":"integer","minimum":1}}}`

const hsnSchema = `{"type":"object","additionalProperties":false,"required":["hsn"],
	"properties":{"hsn":{"type":"string"}}}`

func mod(id, url, ref, schema string, mode registryv1.FailureMode, timeout time.Duration) registry.Module {
	return registry.Module{
		TenantID: "t1", ModuleID: id, Version: "1", BaseURL: url, Timeout: timeout, FailureMode: mode,
		Points: []registry.Point{{Name: "product.enrich", SchemaRef: ref, JSONSchema: schema}},
	}
}

func newEngine(mods ...registry.Module) *Engine {
	return &Engine{
		Modules:    staticModules(mods),
		Schemas:    registry.NewSchemas(),
		HTTPClient: http.DefaultClient,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func cores(n int) []*pimv1.ProductCore {
	out := make([]*pimv1.ProductCore, n)
	for i := range out {
		out[i] = &pimv1.ProductCore{Id: fmt.Sprintf("p%02d", i), TenantId: "t1", Sku: fmt.Sprintf("SKU-%d", i), PriceMinor: 100}
	}
	return out
}

const open, closed = registryv1.FailureMode_FAIL_OPEN, registryv1.FailureMode_FAIL_CLOSED

func TestComposeBatchesOneCallPerModule(t *testing.T) {
	saas := &fakeModule{enrich: each("saas", "s/v1", func(*pimv1.ProductCore) map[string]any { return map[string]any{"seats": 5} })}
	taxes := &fakeModule{enrich: each("taxes", "t/v1", func(p *pimv1.ProductCore) map[string]any { return map[string]any{"hsn": p.Sku} })}
	e := newEngine(
		mod("saas", saas.serve(t), "s/v1", seatsSchema, open, time.Second),
		mod("taxes", taxes.serve(t), "t/v1", hsnSchema, open, time.Second),
	)

	products, err := e.Compose(context.Background(), "t1", cores(50), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]*fakeModule{"saas": saas, "taxes": taxes} {
		if c, p := m.calls.Load(), m.products.Load(); c != 1 || p != 50 {
			t.Errorf("%s: %d calls for %d products, want 1 call for 50", name, c, p)
		}
	}
	for _, p := range products {
		if len(p.Degraded) != 0 || p.Extensions["saas"].Fields["seats"].GetNumberValue() != 5 ||
			p.Extensions["taxes"].Fields["hsn"].GetStringValue() != p.Core.Sku {
			t.Fatalf("product %s composed wrong: %v", p.Core.Id, p)
		}
	}
}

func TestComposeFansOutConcurrently(t *testing.T) {
	slow := func(id, ref string, fields map[string]any) *fakeModule {
		inner := each(id, ref, func(*pimv1.ProductCore) map[string]any { return fields })
		return &fakeModule{enrich: func(ctx context.Context, req *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
			time.Sleep(100 * time.Millisecond)
			return inner(ctx, req)
		}}
	}
	a := slow("saas", "s/v1", map[string]any{"seats": 1})
	b := slow("taxes", "t/v1", map[string]any{"hsn": "1"})
	e := newEngine(
		mod("saas", a.serve(t), "s/v1", seatsSchema, open, time.Second),
		mod("taxes", b.serve(t), "t/v1", hsnSchema, open, time.Second),
	)
	start := time.Now()
	products, err := e.Compose(context.Background(), "t1", cores(1), nil)
	if err != nil || len(products[0].Extensions) != 2 {
		t.Fatalf("got %v, %v", products, err)
	}
	if elapsed := time.Since(start); elapsed > 180*time.Millisecond {
		t.Fatalf("two 100ms modules took %v; fan-out is sequential", elapsed)
	}
}

func TestComposeRejectsAtMerge(t *testing.T) {
	cases := map[string]func(*extv1.Fragment){
		"claims core":            func(f *extv1.Fragment) { f.ModuleId = "core" },
		"claims another module":  func(f *extv1.Fragment) { f.ModuleId = "taxes" },
		"unregistered point":     func(f *extv1.Fragment) { f.SchemaRef = "other/v1" },
		"violates schema":        func(f *extv1.Fragment) { f.Data.Fields["seats"] = structpb.NewStringValue("five") },
		"undeclared property":    func(f *extv1.Fragment) { f.Data.Fields["core"] = structpb.NewStringValue("x") },
		"over the 64KB size cap": func(f *extv1.Fragment) { f.Data.Fields["pad"] = structpb.NewStringValue(strings.Repeat("x", 70_000)) },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			good := each("saas", "s/v1", func(*pimv1.ProductCore) map[string]any { return map[string]any{"seats": 3} })
			saas := &fakeModule{enrich: func(ctx context.Context, req *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
				res, _ := good(ctx, req)
				corrupt(res.ByProductId["p01"].Fragments[0]) // only one product misbehaves
				return res, nil
			}}
			e := newEngine(mod("saas", saas.serve(t), "s/v1", seatsSchema, open, time.Second))

			products, err := e.Compose(context.Background(), "t1", cores(3), nil)
			if err != nil {
				t.Fatalf("FAIL_OPEN read failed: %v", err)
			}
			for _, p := range products {
				if len(p.Extensions) > 1 {
					t.Errorf("%s: extensions has keys other than saas: %v", p.Core.Id, p.Extensions)
				}
				if p.Core.Id == "p01" {
					if p.Extensions != nil || len(p.Degraded) != 1 || p.Degraded[0] != "saas" {
						t.Errorf("p01: want no extension and degraded [saas], got %v / %v", p.Extensions, p.Degraded)
					}
				} else if p.Extensions["saas"] == nil || len(p.Degraded) != 0 {
					t.Errorf("%s: a well-behaved product was affected: %v", p.Core.Id, p)
				}
			}
		})
	}
}

func TestComposeRejectsUnrequestedProducts(t *testing.T) {
	saas := &fakeModule{enrich: func(context.Context, *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		data, _ := structpb.NewStruct(map[string]any{"seats": 1})
		return &extv1.EnrichProductsResponse{ByProductId: map[string]*extv1.FragmentSet{
			"someone-elses-product": {Fragments: []*extv1.Fragment{{ModuleId: "saas", SchemaRef: "s/v1", Data: data}}},
		}}, nil
	}}
	e := newEngine(mod("saas", saas.serve(t), "s/v1", seatsSchema, open, time.Second))
	products, err := e.Compose(context.Background(), "t1", cores(1), nil)
	if err != nil || len(products[0].Degraded) != 1 {
		t.Fatalf("want degraded, got %v, %v", products, err)
	}
}

func TestComposeFailModes(t *testing.T) {
	down := &fakeModule{enrich: func(context.Context, *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("boom"))
	}}
	up := &fakeModule{enrich: each("taxes", "t/v1", func(p *pimv1.ProductCore) map[string]any { return map[string]any{"hsn": "1234"} })}
	downURL, upURL := down.serve(t), up.serve(t)

	t.Run("open", func(t *testing.T) {
		e := newEngine(mod("saas", downURL, "s/v1", seatsSchema, open, time.Second), mod("taxes", upURL, "t/v1", hsnSchema, open, time.Second))
		products, err := e.Compose(context.Background(), "t1", cores(2), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range products {
			if len(p.Degraded) != 1 || p.Degraded[0] != "saas" || p.Extensions["taxes"] == nil || p.Extensions["saas"] != nil {
				t.Errorf("want taxes present and degraded [saas], got %v", p)
			}
		}
	})
	t.Run("closed", func(t *testing.T) {
		e := newEngine(mod("saas", downURL, "s/v1", seatsSchema, closed, time.Second), mod("taxes", upURL, "t/v1", hsnSchema, open, time.Second))
		_, err := e.Compose(context.Background(), "t1", cores(2), nil)
		if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), `"saas"`) {
			t.Fatalf("want Unavailable naming saas, got %v", err)
		}
	})
}

func TestComposeCircuitBreaker(t *testing.T) {
	var up atomic.Bool
	flaky := &fakeModule{}
	good := each("saas", "s/v1", func(*pimv1.ProductCore) map[string]any { return map[string]any{"seats": 2} })
	flaky.enrich = func(ctx context.Context, req *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		if !up.Load() {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("down"))
		}
		time.Sleep(20 * time.Millisecond)
		return good(ctx, req)
	}
	e := newEngine(mod("saas", flaky.serve(t), "s/v1", seatsSchema, open, time.Second))
	clock := &fakeClock{t: time.Unix(0, 0)}
	e.Breakers = &Breakers{Threshold: 5, Cooldown: 10 * time.Second, Now: clock.now}
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		products, err := e.Compose(ctx, "t1", cores(1), nil)
		if err != nil || len(products[0].Degraded) != 1 {
			t.Fatalf("read %d: want degraded, got %v, %v", i, products, err)
		}
	}
	if n := flaky.calls.Load(); n != 5 {
		t.Fatalf("module received %d calls, want 5 (breaker should open after the 5th failure)", n)
	}

	// Module recovers; half-open admits one probe among concurrent reads.
	up.Store(true)
	clock.advance(10 * time.Second)
	done := make(chan []*pimv1.Product, 4)
	for i := 0; i < 4; i++ {
		go func() {
			products, _ := e.Compose(ctx, "t1", cores(1), nil)
			done <- products
		}()
	}
	served := 0
	for i := 0; i < 4; i++ {
		if p := <-done; p[0].Extensions["saas"] != nil {
			served++
		}
	}
	if n := flaky.calls.Load(); n != 6 || served != 1 {
		t.Fatalf("half-open: %d total calls and %d served reads, want 6 and 1", n, served)
	}
	products, _ := e.Compose(ctx, "t1", cores(1), nil)
	if products[0].Extensions["saas"] == nil {
		t.Fatal("breaker did not close after a successful probe")
	}
}

func TestComposeCancelledReadDoesNotTripBreaker(t *testing.T) {
	slow := &fakeModule{enrich: func(ctx context.Context, _ *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	e := newEngine(mod("saas", slow.serve(t), "s/v1", seatsSchema, open, time.Second))
	e.Breakers = NewBreakers(nil)
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, _ = e.Compose(ctx, "t1", cores(1), nil)
		cancel()
	}
	if s := e.Breakers.Snapshot(); len(s) != 1 || s[0].State != "closed" || s[0].Failures != 0 {
		t.Fatalf("caller deadlines tripped the breaker: %+v", s)
	}
}

func TestComposeTimeoutDerivedFromParent(t *testing.T) {
	var seen atomic.Int64 // remaining budget the module observed, in ms
	slow := &fakeModule{enrich: func(ctx context.Context, _ *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		if dl, ok := ctx.Deadline(); ok {
			seen.Store(time.Until(dl).Milliseconds())
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	// The module is allowed 5s; the read only has 150ms.
	e := newEngine(mod("saas", slow.serve(t), "s/v1", seatsSchema, open, 5*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	products, err := e.Compose(ctx, "t1", cores(1), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > time.Second {
		t.Fatalf("took %v; the module's 5s timeout was not capped by the parent deadline", elapsed)
	}
	if ms := seen.Load(); ms <= 0 || ms > 150 {
		t.Fatalf("module saw a %dms deadline, want <= 150ms", ms)
	}
	if len(products[0].Degraded) != 1 {
		t.Fatalf("want degraded, got %v", products[0])
	}
}

// cacheFixture is a taxes-shaped module (static point, TTL 300; contextual
// point keyed on buyer_state, TTL 30) and a saas-shaped one (TTL 60), with the
// cache on a fake clock.
type cacheFixture struct {
	e           *Engine
	clock       *fakeClock
	taxes, saas *fakeModule
	lastBatch   atomic.Int64
	saasDown    atomic.Bool
}

func newCacheFixture(t *testing.T, saasMode registryv1.FailureMode) *cacheFixture {
	fx := &cacheFixture{clock: &fakeClock{t: time.Unix(0, 0)}}
	fx.taxes = &fakeModule{enrich: func(_ context.Context, req *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		fx.lastBatch.Store(int64(len(req.Products)))
		res := &extv1.EnrichProductsResponse{ByProductId: map[string]*extv1.FragmentSet{}}
		for _, p := range req.Products {
			static, _ := structpb.NewStruct(map[string]any{"hsn": p.Sku})
			contextual, _ := structpb.NewStruct(map[string]any{"buyerState": req.Context["buyer_state"]})
			res.ByProductId[p.Id] = &extv1.FragmentSet{Fragments: []*extv1.Fragment{
				{ModuleId: "taxes", SchemaRef: "t/v1", Data: static},
				{ModuleId: "taxes", SchemaRef: "tc/v1", Data: contextual},
			}}
		}
		return res, nil
	}}
	up := each("saas", "s/v1", func(*pimv1.ProductCore) map[string]any { return map[string]any{"seats": 7} })
	fx.saas = &fakeModule{enrich: func(ctx context.Context, req *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		if fx.saasDown.Load() {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("down"))
		}
		return up(ctx, req)
	}}
	fx.e = newEngine(
		registry.Module{TenantID: "t1", ModuleID: "saas", BaseURL: fx.saas.serve(t), Timeout: time.Second, FailureMode: saasMode,
			Points: []registry.Point{{Name: "product.enrich", SchemaRef: "s/v1", JSONSchema: seatsSchema, CacheTTL: 60 * time.Second}}},
		registry.Module{TenantID: "t1", ModuleID: "taxes", BaseURL: fx.taxes.serve(t), Timeout: time.Second, FailureMode: open,
			Points: []registry.Point{
				{Name: "product.enrich", SchemaRef: "t/v1", JSONSchema: hsnSchema, CacheTTL: 300 * time.Second},
				{Name: "product.enrich.contextual", SchemaRef: "tc/v1", CacheTTL: 30 * time.Second, ContextKeys: []string{"buyer_state"},
					JSONSchema: `{"type":"object","properties":{"buyerState":{"type":"string"}}}`},
			}},
	)
	fx.e.Cache = &Cache{MaxStale: 10 * time.Minute, Now: fx.clock.now}
	return fx
}

func (fx *cacheFixture) read(t *testing.T, reqCtx map[string]string) []*pimv1.Product {
	t.Helper()
	products, err := fx.e.Compose(context.Background(), "t1", cores(3), reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	return products
}

func (fx *cacheFixture) wantCalls(t *testing.T, step string, taxes, saas int64) {
	t.Helper()
	if gt, gs := fx.taxes.calls.Load(), fx.saas.calls.Load(); gt != taxes || gs != saas {
		t.Fatalf("%s: taxes %d calls, saas %d calls; want %d and %d", step, gt, gs, taxes, saas)
	}
}

func TestComposeCacheHitsAndContextKeys(t *testing.T) {
	fx := newCacheFixture(t, open)

	fx.read(t, map[string]string{"buyer_state": "MH"})
	fx.wantCalls(t, "first read", 1, 1)

	fx.read(t, map[string]string{"buyer_state": "MH"})
	fx.wantCalls(t, "identical read", 1, 1)

	products := fx.read(t, map[string]string{"buyer_state": "KA"})
	fx.wantCalls(t, "declared key changed", 2, 1)
	if got := products[0].Extensions["taxes"].Fields["buyerState"].GetStringValue(); got != "KA" {
		t.Fatalf("served buyerState %q after changing buyer_state", got)
	}

	fx.read(t, map[string]string{"buyer_state": "KA", "channel": "web"})
	fx.wantCalls(t, "undeclared key changed", 2, 1)

	// Divergent expiry: the contextual point (30s) lapses before saas (60s).
	fx.clock.advance(31 * time.Second)
	fx.read(t, map[string]string{"buyer_state": "KA"})
	fx.wantCalls(t, "after 31s", 3, 1)

	// Invalidation of one product: back to the network, for that product only.
	fx.e.Cache.Invalidate("t1", "taxes", []string{"p01"})
	products = fx.read(t, map[string]string{"buyer_state": "KA"})
	fx.wantCalls(t, "after invalidating p01", 4, 1)
	if n := fx.lastBatch.Load(); n != 1 {
		t.Fatalf("re-fetch after invalidation sent %d products, want only p01", n)
	}
	for _, p := range products {
		if p.Extensions["taxes"].Fields["hsn"] == nil || len(p.Degraded) != 0 {
			t.Fatalf("%s composed wrong from mixed cache/network: %v", p.Core.Id, p)
		}
	}
}

func TestComposeServesStaleOnFailure(t *testing.T) {
	t.Run("fail open serves stale and still marks degraded", func(t *testing.T) {
		fx := newCacheFixture(t, open)
		fx.read(t, nil)
		fx.saasDown.Store(true)

		// Fresh entry: no call at all, so nothing to degrade.
		if p := fx.read(t, nil)[0]; len(p.Degraded) != 0 || p.Extensions["saas"] == nil {
			t.Fatalf("fresh hit with module down: %v", p)
		}
		fx.wantCalls(t, "fresh hit", 1, 1)

		// 61s expires saas (60s) and also taxes' contextual point (30s).
		fx.clock.advance(61 * time.Second)
		p := fx.read(t, nil)[0]
		fx.wantCalls(t, "expired", 2, 2)
		if len(p.Degraded) != 1 || p.Degraded[0] != "saas" {
			t.Fatalf("want degraded [saas], got %v", p.Degraded)
		}
		if p.Extensions["saas"].Fields["seats"].GetNumberValue() != 7 {
			t.Fatalf("stale fragment not served: %v", p.Extensions)
		}
	})
	t.Run("fail closed does not", func(t *testing.T) {
		fx := newCacheFixture(t, closed)
		fx.read(t, nil)
		fx.saasDown.Store(true)
		fx.clock.advance(61 * time.Second)
		_, err := fx.e.Compose(context.Background(), "t1", cores(3), nil)
		if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("want Unavailable, got %v", err)
		}
	})
}

func TestComposeTTLZeroAlwaysCalls(t *testing.T) {
	m := &fakeModule{enrich: each("saas", "s/v1", func(*pimv1.ProductCore) map[string]any { return map[string]any{"seats": 1} })}
	e := newEngine(mod("saas", m.serve(t), "s/v1", seatsSchema, open, time.Second)) // CacheTTL 0
	e.Cache = NewCache()
	for i := 0; i < 3; i++ {
		if _, err := e.Compose(context.Background(), "t1", cores(1), nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := m.calls.Load(); n != 3 {
		t.Fatalf("TTL 0: %d calls for 3 reads", n)
	}
}

func TestComposeInvalidationDuringFlightIsNotCached(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	inner := each("saas", "s/v1", func(*pimv1.ProductCore) map[string]any { return map[string]any{"seats": 1} })
	m := &fakeModule{}
	m.enrich = func(ctx context.Context, req *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		if m.calls.Load() == 1 {
			started <- struct{}{}
			<-release
		}
		return inner(ctx, req)
	}
	mm := mod("saas", m.serve(t), "s/v1", seatsSchema, open, 5*time.Second)
	mm.Points[0].CacheTTL = time.Minute
	e := newEngine(mm)
	e.Cache = NewCache()

	done := make(chan struct{})
	go func() {
		_, _ = e.Compose(context.Background(), "t1", cores(1), nil)
		close(done)
	}()
	<-started
	e.Cache.Invalidate("t1", "saas", nil) // the module's data changed mid-read
	close(release)
	<-done

	if _, err := e.Compose(context.Background(), "t1", cores(1), nil); err != nil {
		t.Fatal(err)
	}
	if n := m.calls.Load(); n != 2 {
		t.Fatalf("%d calls; the in-flight read cached data invalidated during the call", n)
	}
}

func TestComposeModuleTimeoutShorterThanParent(t *testing.T) {
	slow := &fakeModule{enrich: func(ctx context.Context, _ *extv1.EnrichProductsRequest) (*extv1.EnrichProductsResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	e := newEngine(mod("saas", slow.serve(t), "s/v1", seatsSchema, open, 30*time.Millisecond))
	start := time.Now()
	products, err := e.Compose(context.Background(), "t1", cores(1), nil)
	if err != nil || len(products[0].Degraded) != 1 {
		t.Fatalf("want degraded, got %v, %v", products, err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("took %v, want ~30ms", elapsed)
	}
}
