package registry

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Schemas compiles module-supplied JSON Schemas, memoized by content hash so a
// read path that validates every fragment never recompiles.
type Schemas struct {
	mu       sync.Mutex
	compiled map[[32]byte]*jsonschema.Schema
}

func NewSchemas() *Schemas {
	return &Schemas{compiled: map[[32]byte]*jsonschema.Schema{}}
}

const resourceURL = "mem:///fragment.schema.json"

func (s *Schemas) Compile(text string) (*jsonschema.Schema, error) {
	key := sha256.Sum256([]byte(text))
	s.mu.Lock()
	defer s.mu.Unlock()
	if sch, ok := s.compiled[key]; ok {
		return sch, nil
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// No loaders: a module's schema must be self-contained. The default loader
	// would follow file:// $refs on the PIM host.
	c.UseLoader(jsonschema.SchemeURLLoader{})
	if err := c.AddResource(resourceURL, doc); err != nil {
		return nil, err
	}
	sch, err := c.Compile(resourceURL)
	if err != nil {
		return nil, err
	}
	s.compiled[key] = sch
	return sch, nil
}

// topLevelProperties returns the property names of an object schema.
//
// A module's fragments are merged into one object under extensions[module_id],
// so each point's schema must describe an object with declared properties, and
// no two points of one module may declare the same one. Checking this at
// registration turns a merge conflict at read time into a loud rejection now.
func topLevelProperties(text string) ([]string, error) {
	var doc struct {
		Type       any                        `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, err
	}
	if doc.Type != "object" {
		return nil, errors.New(`schema must have "type": "object" at the top level`)
	}
	if len(doc.Properties) == 0 {
		return nil, errors.New(`schema must declare "properties"`)
	}
	names := make([]string, 0, len(doc.Properties))
	for name := range doc.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
