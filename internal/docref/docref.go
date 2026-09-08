// Package docref implements the one piece of vocabulary that crosses service
// boundaries: an opaque reference to a document owned by someone else.
package docref

import (
	"errors"
	"fmt"
	"strings"

	doclinkv1 "github.com/raveesh-me/doclink/gen/doclink/v1"
)

// ErrMalformed is returned for strings that are not "<namespace>/<type>/<id>".
var ErrMalformed = errors.New("docref: malformed reference")

// String renders the canonical wire form. This is what satellites store in
// their linkage tables and what the host SDK posts across the iframe boundary.
func String(r *doclinkv1.DocRef) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s", r.Namespace, r.Type, r.Id)
}

// TypeKey is the namespace/type pair without the id, used to look up owners and
// link declarations.
func TypeKey(r *doclinkv1.DocRef) string {
	if r == nil {
		return ""
	}
	return r.Namespace + "/" + r.Type
}

// Parse is the inverse of String. The id may itself contain slashes so that
// owners are free to use hierarchical identifiers; namespace and type may not.
func Parse(s string) (*doclinkv1.DocRef, error) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: %q", ErrMalformed, s)
	}
	for i, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("%w: empty segment %d in %q", ErrMalformed, i, s)
		}
	}
	return &doclinkv1.DocRef{Namespace: parts[0], Type: parts[1], Id: parts[2]}, nil
}

// MustParse is for wiring code and seed data where a malformed literal is a
// programming error.
func MustParse(s string) *doclinkv1.DocRef {
	r, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return r
}

// New builds a reference without going through the string form.
func New(namespace, typ, id string) *doclinkv1.DocRef {
	return &doclinkv1.DocRef{Namespace: namespace, Type: typ, Id: id}
}
