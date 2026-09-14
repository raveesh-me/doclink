package pim

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/encoding/protojson"

	registryv1 "github.com/raveesh-me/doclink/gen/registry/v1"
	"github.com/raveesh-me/doclink/internal/registry"
)

// tenantModules serves a different module set per tenant.
type tenantModules map[string][]registry.Module

func (t tenantModules) ActiveModules(_ context.Context, tenantID, _ string) ([]registry.Module, error) {
	return t[tenantID], nil
}

func extensionsOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(doc)
	var round map[string]any
	_ = json.Unmarshal(b, &round)
	product := round["components"].(map[string]any)["schemas"].(map[string]any)["Product"].(map[string]any)
	return product["properties"].(map[string]any)["extensions"].(map[string]any)["properties"].(map[string]any)
}

func TestSpecDiffersPerTenant(t *testing.T) {
	fx := newCacheFixture(t, open)
	mods := fx.e.Modules.(staticModules)
	var saas, taxes registry.Module
	for _, m := range mods {
		if m.ModuleID == "saas" {
			saas = m
		} else {
			taxes = m
		}
	}
	src := tenantModules{"t1": {saas, taxes}, "t2": {taxes}}

	t1, err := BuildSpec(context.Background(), src, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := BuildSpec(context.Background(), src, "t2")
	if err != nil {
		t.Fatal(err)
	}
	e1, e2 := extensionsOf(t, t1), extensionsOf(t, t2)
	if keys(e1) != "saas,taxes" || keys(e2) != "taxes" {
		t.Fatalf("t1 extensions %s, t2 extensions %s", keys(e1), keys(e2))
	}
	if !reflect.DeepEqual(e1["taxes"], e2["taxes"]) {
		t.Fatal("the same module is described differently for two tenants")
	}

	taxesSchema := e1["taxes"].(map[string]any)
	if keys(taxesSchema["properties"].(map[string]any)) != "buyerState,hsn" {
		t.Fatalf("taxes points not merged: %v", taxesSchema["properties"])
	}
	empty, _ := BuildSpec(context.Background(), src, "t3")
	if len(extensionsOf(t, empty)) != 0 {
		t.Fatal("tenant with no modules got extensions")
	}
}

// The spec must describe what the wire actually returns: compose a real
// product through the engine and validate its JSON against the generated
// Product schema.
func TestSpecValidatesComposedProduct(t *testing.T) {
	fx := newCacheFixture(t, open)
	products := fx.read(t, map[string]string{"buyer_state": "KA"})

	doc, err := BuildSpec(context.Background(), fx.e.Modules, "t1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem:///openapi.json", parsed); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("mem:///openapi.json#/components/schemas/Product")
	if err != nil {
		t.Fatalf("generated Product schema does not compile: %v", err)
	}

	check := func(label string, wire []byte, wantValid bool) {
		t.Helper()
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(string(wire)))
		if err != nil {
			t.Fatal(err)
		}
		if err := sch.Validate(v); (err == nil) != wantValid {
			t.Fatalf("%s: valid=%v, want %v (%v)\n%s", label, err == nil, wantValid, err, wire)
		}
	}
	for _, p := range products {
		wire, _ := protojson.Marshal(p)
		check(p.Core.Id, wire, true)
	}
	check("unknown module key", []byte(`{"core":{"id":"p1"},"extensions":{"marketing":{}}}`), false)
	check("unknown degraded id", []byte(`{"core":{"id":"p1"},"degraded":["marketing"]}`), false)
	check("half a point", []byte(`{"core":{"id":"p1"},"extensions":{"saas":{}}}`), true)
	check("wrong type in module data", []byte(`{"core":{"id":"p1"},"extensions":{"saas":{"seats":"five"}}}`), false)

	// And the core half matches proto3 JSON, int64-as-string included.
	check("int64 as string", []byte(`{"core":{"id":"p1","priceMinor":"49900"}}`), true)
}

func TestModuleSchemaDependentRequired(t *testing.T) {
	m := registry.Module{ModuleID: "taxes", Version: "1", FailureMode: registryv1.FailureMode_FAIL_OPEN, Points: []registry.Point{
		{Name: "product.enrich", CacheTTL: time.Minute, JSONSchema: `{"type":"object","additionalProperties":false,"required":["hsn","taxClass"],"properties":{"hsn":{},"taxClass":{}}}`},
		{Name: "product.enrich.contextual", JSONSchema: `{"type":"object","additionalProperties":false,"required":["rate"],"properties":{"rate":{}}}`},
	}}
	s, err := moduleSchema(m)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"hsn": []string{"taxClass"}, "taxClass": []string{"hsn"}}
	if !reflect.DeepEqual(s["dependentRequired"], want) {
		t.Fatalf("dependentRequired = %v", s["dependentRequired"])
	}
	if s["additionalProperties"] != false {
		t.Fatal("closed point schemas should give a closed module schema")
	}
}

func keys(m map[string]any) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
