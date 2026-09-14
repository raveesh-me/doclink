// Package saas is an example module: seat counts and billing cycles for
// subscription products.
//
// It also carries a chaos switch, because several acceptance criteria are about
// what PIM does when a module misbehaves, and the honest way to show that is a
// real module sending real bad responses over the wire.
package saas

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	extv1 "github.com/raveesh-me/doclink/gen/ext/v1"
	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	saasv1 "github.com/raveesh-me/doclink/gen/modules/saas/v1"
	pimv1 "github.com/raveesh-me/doclink/gen/pim/v1"
	"github.com/raveesh-me/doclink/internal/modules/modkit"
)

const (
	ModuleID  = "saas"
	Version   = "1.0.0"
	schemaRef = "acme.saas/v1"
)

const schema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Subscription terms",
  "type": "object",
  "additionalProperties": false,
  "required": ["seats", "billingCycle"],
  "properties": {
    "seats": {"type": "integer", "minimum": 1, "description": "Seats included in one unit."},
    "billingCycle": {"type": "string", "enum": ["MONTHLY", "ANNUAL"]}
  }
}`

// Chaos modes. Each one makes the module misbehave in a way PIM must contain.
const (
	ChaosOff = "off"
	// A fragment whose data violates the registered schema.
	ChaosBadSchema = "bad_schema"
	// A fragment claiming to be from module "core".
	ChaosSpoofCore = "spoof_core"
	// A fragment claiming to be from module "taxes".
	ChaosSpoofTaxes = "spoof_taxes"
	// A fragment larger than PIM's 64KB cap.
	ChaosOversize = "oversize"
)

var chaosModes = []string{ChaosOff, ChaosBadSchema, ChaosSpoofCore, ChaosSpoofTaxes, ChaosOversize}

type Module struct {
	Stats           modkit.Stats
	CacheTTLSeconds int32
	Log             *slog.Logger

	chaos atomic.Value // string
}

var _ extv1connect.ModuleExtensionHandler = (*Module)(nil)

func (m *Module) Describe(context.Context, *connect.Request[extv1.DescribeRequest]) (*connect.Response[extv1.DescribeResponse], error) {
	return connect.NewResponse(&extv1.DescribeResponse{
		ModuleId: ModuleID,
		Version:  Version,
		Points: []*extv1.ExtensionPoint{{
			Name:            "product.enrich",
			SchemaRef:       schemaRef,
			JsonSchema:      schema,
			CacheTtlSeconds: m.CacheTTLSeconds,
		}},
	}), nil
}

func (m *Module) EnrichProducts(_ context.Context, req *connect.Request[extv1.EnrichProductsRequest]) (*connect.Response[extv1.EnrichProductsResponse], error) {
	m.Stats.RecordEnrich(len(req.Msg.Products))
	chaos := m.Chaos()

	out := &extv1.EnrichProductsResponse{ByProductId: map[string]*extv1.FragmentSet{}}
	for _, p := range req.Msg.Products {
		// The module's own type, converted at its own boundary.
		data, err := modkit.ToStruct(terms(p))
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		f := &extv1.Fragment{ModuleId: ModuleID, SchemaRef: schemaRef, Data: data}

		switch chaos {
		case ChaosBadSchema:
			// Deliberately bypasses SubscriptionTerms: the typed message could
			// not express this, which is rather the point of having one.
			f.Data.Fields["seats"] = structpb.NewStringValue("five")
		case ChaosSpoofCore:
			f.ModuleId = "core"
		case ChaosSpoofTaxes:
			f.ModuleId = "taxes"
			f.SchemaRef = "acme.tax/v1"
			f.Data, _ = structpb.NewStruct(map[string]any{"hsn": "0000", "taxClass": "exempt"})
		case ChaosOversize:
			f.Data.Fields["padding"] = structpb.NewStringValue(strings.Repeat("x", 70_000))
		}
		out.ByProductId[p.Id] = &extv1.FragmentSet{Fragments: []*extv1.Fragment{f}}
	}
	return connect.NewResponse(out), nil
}

// terms derives stable, plausible terms from the SKU. The module's own data.
func terms(p *pimv1.ProductCore) *saasv1.SubscriptionTerms {
	if p.Sku == "SKU-1" {
		return &saasv1.SubscriptionTerms{Seats: 5, BillingCycle: saasv1.BillingCycle_MONTHLY}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(p.Sku))
	n := h.Sum32()
	cycle := saasv1.BillingCycle_MONTHLY
	if n%2 == 0 {
		cycle = saasv1.BillingCycle_ANNUAL
	}
	return &saasv1.SubscriptionTerms{Seats: int32(n%20) + 1, BillingCycle: cycle}
}

func (m *Module) Chaos() string {
	if v, ok := m.chaos.Load().(string); ok {
		return v
	}
	return ChaosOff
}

// MountChaos adds GET/POST /chaos.
func (m *Module) MountChaos(mux *http.ServeMux) {
	mux.HandleFunc("GET /chaos", func(w http.ResponseWriter, _ *http.Request) {
		modkit.WriteJSON(w, map[string]any{"mode": m.Chaos(), "modes": chaosModes})
	})
	mux.HandleFunc("POST /chaos", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode string `json:"mode"`
		}
		if err := modkit.DecodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		valid := false
		for _, mode := range chaosModes {
			valid = valid || mode == req.Mode
		}
		if !valid {
			http.Error(w, fmt.Sprintf("mode must be one of %v", chaosModes), http.StatusBadRequest)
			return
		}
		m.chaos.Store(req.Mode)
		m.Log.Warn("chaos mode set", "mode", req.Mode)
		modkit.WriteJSON(w, map[string]any{"mode": req.Mode})
	})
}
