package registry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	extv1 "github.com/raveesh-me/doclink/gen/ext/v1"
	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	registryv1 "github.com/raveesh-me/doclink/gen/registry/v1"
	"github.com/raveesh-me/doclink/internal/sqlite"
)

const objSchema = `{"type":"object","properties":{"a":{"type":"string"}}}`

// describer is a module that only answers Describe.
type describer struct {
	extv1connect.UnimplementedModuleExtensionHandler
	resp *extv1.DescribeResponse
}

func (d *describer) Describe(context.Context, *connect.Request[extv1.DescribeRequest]) (*connect.Response[extv1.DescribeResponse], error) {
	return connect.NewResponse(d.resp), nil
}

func serveModule(t *testing.T, resp *extv1.DescribeResponse) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(extv1connect.NewModuleExtensionHandler(&describer{resp: resp}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newService(t *testing.T) *Service {
	t.Helper()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{
		Store:      store,
		Schemas:    NewSchemas(),
		HTTPClient: http.DefaultClient,
		DrainGrace: 50 * time.Millisecond,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func register(s *Service, tenant, url string) (*connect.Response[registryv1.RegisterModuleResponse], error) {
	return s.RegisterModule(context.Background(), connect.NewRequest(&registryv1.RegisterModuleRequest{TenantId: tenant, BaseUrl: url}))
}

func TestRegisterPersistsOneRowPerPoint(t *testing.T) {
	s := newService(t)
	url := serveModule(t, &extv1.DescribeResponse{ModuleId: "taxes", Version: "1", Points: []*extv1.ExtensionPoint{
		{Name: "product.enrich", SchemaRef: "x/v1", JsonSchema: objSchema, CacheTtlSeconds: 300},
		{Name: "product.enrich.contextual", SchemaRef: "y/v1", JsonSchema: `{"type":"object","properties":{"b":{}}}`, ContextKeys: []string{"buyer_state"}},
	}})
	if _, err := register(s, "t1", url+"/"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Store.List(context.Background(), "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if r.State != registryv1.ModuleState_ACTIVE || r.FailureMode != registryv1.FailureMode_FAIL_OPEN || r.TimeoutMs != 500 || r.BaseUrl != url {
			t.Errorf("unexpected row %v", r)
		}
	}
	if got := rows[1].ContextKeys; len(got) != 1 || got[0] != "buyer_state" {
		t.Errorf("context_keys not persisted: %v", got)
	}

	mods, err := s.Store.ActiveModules(context.Background(), "t1", "product.enrich")
	if err != nil || len(mods) != 1 || len(mods[0].Points) != 2 {
		t.Fatalf("ActiveModules = %v, %v", mods, err)
	}
}

func TestRegisterRejections(t *testing.T) {
	point := func(schema string) []*extv1.ExtensionPoint {
		return []*extv1.ExtensionPoint{{Name: "product.enrich", SchemaRef: "x/v1", JsonSchema: schema}}
	}
	cases := []struct {
		name string
		resp *extv1.DescribeResponse
		code connect.Code
		msg  string
	}{
		{"schema does not compile", &extv1.DescribeResponse{ModuleId: "m", Version: "1", Points: []*extv1.ExtensionPoint{
			{Name: "product.enrich", SchemaRef: "ok/v1", JsonSchema: objSchema},
			{Name: "product.enrich.other", SchemaRef: "bad/v1", JsonSchema: `{"type":"object","properties":{"z":{"type":"strnig"}}}`},
		}}, connect.CodeInvalidArgument, "does not compile"},
		{"schema is not JSON", &extv1.DescribeResponse{ModuleId: "m", Version: "1", Points: point(`{nope`)}, connect.CodeInvalidArgument, "does not compile"},
		{"external ref", &extv1.DescribeResponse{ModuleId: "m", Version: "1", Points: point(`{"type":"object","properties":{"a":{"$ref":"file:///etc/passwd"}}}`)}, connect.CodeInvalidArgument, "does not compile"},
		{"not an object", &extv1.DescribeResponse{ModuleId: "m", Version: "1", Points: point(`{"type":"string"}`)}, connect.CodeInvalidArgument, "type"},
		{"reserved core", &extv1.DescribeResponse{ModuleId: "core", Version: "1", Points: point(objSchema)}, connect.CodeInvalidArgument, "reserved"},
		{"reserved pim", &extv1.DescribeResponse{ModuleId: "pim", Version: "1", Points: point(objSchema)}, connect.CodeInvalidArgument, "reserved"},
		{"reserved _meta", &extv1.DescribeResponse{ModuleId: "_meta", Version: "1", Points: point(objSchema)}, connect.CodeInvalidArgument, "reserved"},
		{"uppercase id", &extv1.DescribeResponse{ModuleId: "Taxes", Version: "1", Points: point(objSchema)}, connect.CodeInvalidArgument, "lowercase"},
		{"overlapping properties", &extv1.DescribeResponse{ModuleId: "m", Version: "1", Points: []*extv1.ExtensionPoint{
			{Name: "product.enrich", SchemaRef: "a/v1", JsonSchema: objSchema},
			{Name: "product.enrich.b", SchemaRef: "b/v1", JsonSchema: objSchema},
		}}, connect.CodeInvalidArgument, `property "a"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t)
			_, err := register(s, "t1", serveModule(t, tc.resp))
			if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("want %v containing %q, got %v", tc.code, tc.msg, err)
			}
			rows, _ := s.Store.List(context.Background(), "t1", "")
			if len(rows) != 0 {
				t.Fatalf("rejected registration persisted %d rows", len(rows))
			}
		})
	}
}

func TestRegisterUnreachable(t *testing.T) {
	s := newService(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := register(s, "t1", url); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
}

func TestRegisterCollision(t *testing.T) {
	s := newService(t)
	resp := &extv1.DescribeResponse{ModuleId: "saas", Version: "1", Points: []*extv1.ExtensionPoint{
		{Name: "product.enrich", SchemaRef: "s/v1", JsonSchema: objSchema},
	}}
	if _, err := register(s, "t1", serveModule(t, resp)); err != nil {
		t.Fatal(err)
	}
	// Same id from a different URL collides; another tenant does not.
	if _, err := register(s, "t1", serveModule(t, resp)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("want AlreadyExists, got %v", err)
	}
	if _, err := register(s, "t2", serveModule(t, resp)); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
}

func TestUnregisterDrainsThenDeletes(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	url := serveModule(t, &extv1.DescribeResponse{ModuleId: "saas", Version: "1", Points: []*extv1.ExtensionPoint{
		{Name: "product.enrich", SchemaRef: "s/v1", JsonSchema: objSchema},
	}})
	if _, err := register(s, "t1", url); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UnregisterModule(ctx, connect.NewRequest(&registryv1.UnregisterModuleRequest{TenantId: "t1", ModuleId: "saas"})); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Store.List(ctx, "t1", "")
	if len(rows) != 1 || rows[0].State != registryv1.ModuleState_DRAINING {
		t.Fatalf("want one DRAINING row, got %v", rows)
	}
	if mods, _ := s.Store.ActiveModules(ctx, "t1", "product.enrich"); len(mods) != 0 {
		t.Fatalf("draining module still in fan-out: %v", mods)
	}
	// Still installed while draining.
	if _, err := register(s, "t1", url); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("re-register during drain: want AlreadyExists, got %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rows, _ := s.Store.List(ctx, "t1", ""); len(rows) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rows not deleted after grace period")
}
