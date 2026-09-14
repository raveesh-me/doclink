package pim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	pimv1 "github.com/raveesh-me/doclink/gen/pim/v1"
	"github.com/raveesh-me/doclink/internal/registry"
)

// SpecHandler serves GET /openapi/{tenant_id}.json: an OpenAPI 3.1 document
// assembled at request time from PIM's own proto descriptors plus the JSON
// Schemas the tenant's modules registered.
//
// This is where the type safety the wire gave up comes back. The wire carries
// google.protobuf.Struct so that PIM never compiles a module's types; the spec
// puts each module's schema back under Product.extensions.{module_id}, so a
// consumer for one tenant gets a document describing exactly that tenant's
// modules — generated without PIM importing any of them.
type SpecHandler struct {
	Modules ModuleSource
	Log     *slog.Logger
}

func (h *SpecHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := strings.CutSuffix(r.PathValue("file"), ".json")
	if !ok || tenantID == "" {
		http.Error(w, "want /openapi/{tenant_id}.json", http.StatusNotFound)
		return
	}
	doc, err := BuildSpec(r.Context(), h.Modules, tenantID)
	if err != nil {
		h.Log.Error("build openapi", "tenant", tenantID, "err", err)
		http.Error(w, "could not build spec", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}

// BuildSpec returns the tenant's OpenAPI document. Maps marshal with sorted
// keys, so two builds of the same registry state are byte-identical.
func BuildSpec(ctx context.Context, modules ModuleSource, tenantID string) (map[string]any, error) {
	mods, err := modules.ActiveModules(ctx, tenantID, enrichFamily)
	if err != nil {
		return nil, err
	}

	schemas := map[string]any{}
	g := &schemaGen{defs: schemas}
	paths := map[string]any{}
	svc := pimv1.File_pim_v1_pim_proto.Services().ByName("PIM")
	for i := 0; i < svc.Methods().Len(); i++ {
		m := svc.Methods().Get(i)
		paths[fmt.Sprintf("/%s/%s", svc.FullName(), m.Name())] = map[string]any{
			"post": map[string]any{
				"operationId": string(m.Name()),
				"requestBody": map[string]any{
					"required": true,
					"content":  jsonContent(g.ref(m.Input())),
				},
				"responses": map[string]any{
					"200":     map[string]any{"description": "OK", "content": jsonContent(g.ref(m.Output()))},
					"default": map[string]any{"description": "Connect error", "content": jsonContent(map[string]any{"$ref": "#/components/schemas/ConnectError"})},
				},
			},
		}
	}
	schemas["ConnectError"] = map[string]any{
		"type":     "object",
		"required": []string{"code"},
		"properties": map[string]any{
			"code":    map[string]any{"type": "string", "examples": []string{"unavailable", "not_found", "invalid_argument"}},
			"message": map[string]any{"type": "string"},
		},
	}

	// Replace the generic Product.extensions and Product.degraded with this
	// tenant's modules.
	product := schemas["Product"].(map[string]any)
	props := product["properties"].(map[string]any)
	extProps := map[string]any{}
	ids := []string{}
	versions := []string{}
	for _, m := range mods {
		s, err := moduleSchema(m)
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", m.ModuleID, err)
		}
		extProps[m.ModuleID] = s
		ids = append(ids, m.ModuleID)
		versions = append(versions, m.ModuleID+"@"+m.Version)
	}
	props["extensions"] = map[string]any{
		"type": "object",
		"description": fmt.Sprintf("Module data for tenant %s, keyed by module_id. A key is absent when the module "+
			"has nothing for the product or failed; failures are listed in degraded.", tenantID),
		"additionalProperties": false,
		"properties":           extProps,
	}
	degradedItems := map[string]any{"type": "string"}
	if len(ids) > 0 {
		degradedItems["enum"] = ids
	}
	props["degraded"] = map[string]any{
		"type": "array",
		"description": "module_ids whose extension is missing, or served from a stale cache entry, for this product. " +
			"Omitted when empty (proto3 JSON).",
		"items": degradedItems,
	}

	moduleList := "none"
	if len(versions) > 0 {
		moduleList = strings.Join(versions, ", ")
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "PIM API for tenant " + tenantID,
			"version": "v1",
			"description": "Connect RPC over HTTP POST with JSON bodies. int64 fields are decimal strings (proto3 JSON). " +
				"Installed modules: " + moduleList + ".",
		},
		"paths":      paths,
		"components": map[string]any{"schemas": schemas},
	}, nil
}

func jsonContent(schema any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}

// moduleSchema merges a module's point schemas into the one object its
// fragments are merged into. Registration guarantees the points declare
// disjoint properties.
//
// Each point's fragment can be absent (the contextual point without a
// buyer_state; a partial stale entry), so no property is required outright.
// Instead dependentRequired says: if any of a point's required properties is
// present, all of them are.
//
// Limitation: point schemas are merged at the property level, so a schema that
// uses local $refs into its own $defs does not survive the merge.
func moduleSchema(m registry.Module) (map[string]any, error) {
	props := map[string]any{}
	dependent := map[string]any{}
	closed := true
	points := []any{}
	for _, p := range m.Points {
		var s map[string]any
		if err := json.Unmarshal([]byte(p.JSONSchema), &s); err != nil {
			return nil, fmt.Errorf("point %s: %w", p.Name, err)
		}
		pointProps, _ := s["properties"].(map[string]any)
		for k, v := range pointProps {
			props[k] = v
		}
		if ap, ok := s["additionalProperties"].(bool); !ok || ap {
			closed = false
		}
		var required []string
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if name, ok := r.(string); ok {
					required = append(required, name)
				}
			}
		}
		for _, r := range required {
			others := []string{}
			for _, o := range required {
				if o != r {
					others = append(others, o)
				}
			}
			if len(others) > 0 {
				dependent[r] = others
			}
		}
		point := map[string]any{"name": p.Name, "schemaRef": p.SchemaRef, "cacheTtlSeconds": int(p.CacheTTL.Seconds())}
		if len(p.ContextKeys) > 0 {
			point["contextKeys"] = p.ContextKeys
		}
		if title, ok := s["title"].(string); ok {
			point["title"] = title
		}
		points = append(points, point)
	}
	out := map[string]any{
		"type":         "object",
		"title":        m.ModuleID,
		"description":  fmt.Sprintf("Provided by module %s %s.", m.ModuleID, m.Version),
		"properties":   props,
		"x-pim-points": points,
	}
	if closed {
		out["additionalProperties"] = false
	}
	if len(dependent) > 0 {
		out["dependentRequired"] = dependent
	}
	return out, nil
}

// schemaGen turns proto message descriptors into JSON Schemas matching their
// proto3 JSON encoding, so the core half of the spec cannot drift from the
// wire.
type schemaGen struct {
	defs map[string]any
}

func (g *schemaGen) ref(md protoreflect.MessageDescriptor) map[string]any {
	name := string(md.Name())
	if _, done := g.defs[name]; !done {
		g.defs[name] = nil // reserve, for recursive messages
		g.defs[name] = g.message(md)
	}
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func (g *schemaGen) message(md protoreflect.MessageDescriptor) map[string]any {
	props := map[string]any{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		props[fd.JSONName()] = g.field(fd)
	}
	return map[string]any{"type": "object", "properties": props}
}

func (g *schemaGen) field(fd protoreflect.FieldDescriptor) map[string]any {
	if fd.IsMap() {
		return map[string]any{"type": "object", "additionalProperties": g.singular(fd.MapValue())}
	}
	if fd.IsList() {
		return map[string]any{"type": "array", "items": g.singular(fd)}
	}
	return g.singular(fd)
}

func (g *schemaGen) singular(fd protoreflect.FieldDescriptor) map[string]any {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return map[string]any{"type": "integer", "format": "int32"}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return map[string]any{"type": "integer", "format": "uint32", "minimum": 0}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// proto3 JSON writes 64-bit integers as strings, and accepts either.
		return map[string]any{"type": []string{"string", "integer"}, "format": "int64", "pattern": "^-?[0-9]+$"}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return map[string]any{"type": []string{"string", "integer"}, "format": "uint64", "pattern": "^[0-9]+$"}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"type": "number"}
	case protoreflect.BytesKind:
		return map[string]any{"type": "string", "contentEncoding": "base64"}
	case protoreflect.EnumKind:
		vals := fd.Enum().Values()
		names := make([]string, vals.Len())
		for i := range names {
			names[i] = string(vals.Get(i).Name())
		}
		return map[string]any{"type": "string", "enum": names}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		switch fd.Message().FullName() {
		case "google.protobuf.Struct":
			return map[string]any{"type": "object"}
		case "google.protobuf.Timestamp":
			return map[string]any{"type": "string", "format": "date-time"}
		}
		return g.ref(fd.Message())
	}
	return map[string]any{}
}
