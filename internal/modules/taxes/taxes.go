// Package taxes is an example module. It owns HSN classification and GST
// computation; PIM knows neither exists.
package taxes

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	extv1 "github.com/raveesh-me/doclink/gen/ext/v1"
	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	taxesv1 "github.com/raveesh-me/doclink/gen/modules/taxes/v1"
	pimv1 "github.com/raveesh-me/doclink/gen/pim/v1"
	registryv1 "github.com/raveesh-me/doclink/gen/registry/v1"
	"github.com/raveesh-me/doclink/gen/registry/v1/registryv1connect"
	"github.com/raveesh-me/doclink/internal/modules/modkit"
)

const (
	ModuleID = "taxes"
	Version  = "1.0.0"

	staticRef     = "acme.tax/v1"
	contextualRef = "acme.tax.contextual/v1"

	// sellerState decides intra- vs inter-state supply.
	sellerState = "KA"
)

// rates is the GST-ish rate table, in percent. Not real tax logic.
var rates = map[string]float64{
	"exempt":   0,
	"reduced":  5,
	"standard": 18,
	"luxury":   28,
}

// defaultClasses is the module's own reference data, keyed by SKU.
var defaultClasses = map[string]*taxesv1.TaxClassification{
	"SKU-1": {Hsn: "8471", TaxClass: "standard"}, // computers
	"SKU-2": {Hsn: "4901", TaxClass: "exempt"},   // printed books
	"SKU-3": {Hsn: "6109", TaxClass: "reduced"},  // T-shirts
	"SKU-4": {Hsn: "7113", TaxClass: "luxury"},   // jewellery
}

const staticSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Tax classification",
  "type": "object",
  "additionalProperties": false,
  "required": ["hsn", "taxClass"],
  "properties": {
    "hsn": {"type": "string", "pattern": "^[0-9]{4,8}$", "description": "Harmonized System of Nomenclature code."},
    "taxClass": {"type": "string", "enum": ["exempt", "reduced", "standard", "luxury"]}
  }
}`

const contextualSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Tax computation for a buyer state",
  "type": "object",
  "additionalProperties": false,
  "required": ["buyerState", "supply", "rate", "amountMinor", "cgstMinor", "sgstMinor", "igstMinor"],
  "properties": {
    "buyerState": {"type": "string", "description": "The context[buyer_state] the computation used."},
    "supply": {"type": "string", "enum": ["INTRA_STATE", "INTER_STATE"]},
    "rate": {"type": "number", "minimum": 0, "maximum": 100, "description": "GST rate, percent."},
    "amountMinor": {"type": "string", "pattern": "^-?[0-9]+$", "format": "int64", "description": "Total tax in minor units. int64 as a decimal string, per proto3 JSON."},
    "cgstMinor": {"type": "string", "pattern": "^-?[0-9]+$", "format": "int64"},
    "sgstMinor": {"type": "string", "pattern": "^-?[0-9]+$", "format": "int64"},
    "igstMinor": {"type": "string", "pattern": "^-?[0-9]+$", "format": "int64"}
  }
}`

type Module struct {
	Stats    modkit.Stats
	Sleep    time.Duration
	Registry registryv1connect.RegistryClient
	Log      *slog.Logger

	mu        sync.RWMutex
	overrides map[string]*taxesv1.TaxClassification // tenant/sku
}

var _ extv1connect.ModuleExtensionHandler = (*Module)(nil)

func (m *Module) Describe(context.Context, *connect.Request[extv1.DescribeRequest]) (*connect.Response[extv1.DescribeResponse], error) {
	return connect.NewResponse(&extv1.DescribeResponse{
		ModuleId: ModuleID,
		Version:  Version,
		Points: []*extv1.ExtensionPoint{
			{
				Name:            "product.enrich",
				SchemaRef:       staticRef,
				JsonSchema:      staticSchema,
				CacheTtlSeconds: 300,
			},
			{
				Name:            "product.enrich.contextual",
				SchemaRef:       contextualRef,
				JsonSchema:      contextualSchema,
				CacheTtlSeconds: 30,
				ContextKeys:     []string{"buyer_state"},
			},
		},
	}), nil
}

func (m *Module) EnrichProducts(ctx context.Context, req *connect.Request[extv1.EnrichProductsRequest]) (*connect.Response[extv1.EnrichProductsResponse], error) {
	m.Stats.RecordEnrich(len(req.Msg.Products))

	// Deliberate latency, so fan-out concurrency is visible.
	select {
	case <-time.After(m.Sleep):
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	}

	buyerState := req.Msg.Context["buyer_state"]
	out := &extv1.EnrichProductsResponse{ByProductId: map[string]*extv1.FragmentSet{}}
	for _, p := range req.Msg.Products {
		class := m.classify(req.Msg.TenantId, p.Sku)
		set := &extv1.FragmentSet{}

		f, err := fragment(staticRef, class)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		set.Fragments = append(set.Fragments, f)

		// Without a buyer state there is nothing to compute. Returning no
		// contextual fragment is an answer, not a failure.
		if buyerState != "" {
			f, err := fragment(contextualRef, compute(p, class, buyerState))
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			set.Fragments = append(set.Fragments, f)
		}
		out.ByProductId[p.Id] = set
	}
	return connect.NewResponse(out), nil
}

// fragment is where the module's types leave the module.
func fragment(ref string, msg proto.Message) (*extv1.Fragment, error) {
	data, err := modkit.ToStruct(msg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	return &extv1.Fragment{ModuleId: ModuleID, SchemaRef: ref, Data: data}, nil
}

func (m *Module) classify(tenant, sku string) *taxesv1.TaxClassification {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.overrides[tenant+"/"+sku]; ok {
		return c
	}
	if c, ok := defaultClasses[sku]; ok {
		return c
	}
	return &taxesv1.TaxClassification{Hsn: "9983", TaxClass: "standard"}
}

func compute(p *pimv1.ProductCore, class *taxesv1.TaxClassification, buyerState string) *taxesv1.TaxComputation {
	rate := rates[class.TaxClass]
	amount := int64(math.Round(float64(p.PriceMinor) * rate / 100))
	c := &taxesv1.TaxComputation{BuyerState: buyerState, Rate: rate, AmountMinor: amount}
	if buyerState == sellerState {
		c.Supply = taxesv1.Supply_INTRA_STATE
		c.CgstMinor = amount / 2
		c.SgstMinor = amount - amount/2
	} else {
		c.Supply = taxesv1.Supply_INTER_STATE
		c.IgstMinor = amount
	}
	return c
}

type reclassifyRequest struct {
	TenantID   string   `json:"tenant_id"`
	SKU        string   `json:"sku"`
	HSN        string   `json:"hsn"`
	TaxClass   string   `json:"tax_class"`
	ProductIDs []string `json:"product_ids"`
}

// MountAdmin adds POST /admin/reclassify: the module's own data changes, and it
// tells PIM which products are affected. It does not tell PIM the new values.
func (m *Module) MountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/reclassify", func(w http.ResponseWriter, r *http.Request) {
		var req reclassifyRequest
		if err := modkit.DecodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, ok := rates[req.TaxClass]; !ok {
			http.Error(w, "unknown tax_class", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if m.overrides == nil {
			m.overrides = map[string]*taxesv1.TaxClassification{}
		}
		m.overrides[req.TenantID+"/"+req.SKU] = &taxesv1.TaxClassification{Hsn: req.HSN, TaxClass: req.TaxClass}
		m.mu.Unlock()

		res, err := m.Registry.InvalidateFragments(r.Context(), connect.NewRequest(&registryv1.InvalidateFragmentsRequest{
			TenantId:   req.TenantID,
			ModuleId:   ModuleID,
			ProductIds: req.ProductIDs,
		}))
		if err != nil {
			http.Error(w, "invalidate: "+err.Error(), http.StatusBadGateway)
			return
		}
		m.Log.Info("reclassified", "tenant", req.TenantID, "sku", req.SKU, "evicted", res.Msg.Evicted)
		modkit.WriteJSON(w, map[string]any{"evicted": res.Msg.Evicted})
	})
}
