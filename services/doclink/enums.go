package main

import (
	"strings"

	doclinkv1 "github.com/raveesh-me/doclink/gen/doclink/v1"
)

// Enums are stored as short strings rather than integers so the tables stay
// readable in psql during a benchmark run, and so adding an enum value later
// does not silently reinterpret existing rows.

func cardinalityName(c doclinkv1.Cardinality) string {
	switch c {
	case doclinkv1.Cardinality_CARDINALITY_ONE_TO_ONE:
		return "ONE_TO_ONE"
	case doclinkv1.Cardinality_CARDINALITY_MANY_TO_ONE:
		return "MANY_TO_ONE"
	case doclinkv1.Cardinality_CARDINALITY_ONE_TO_MANY:
		return "ONE_TO_MANY"
	case doclinkv1.Cardinality_CARDINALITY_MANY_TO_MANY:
		return "MANY_TO_MANY"
	default:
		return "UNSPECIFIED"
	}
}

func cardinalityValue(s string) doclinkv1.Cardinality {
	switch s {
	case "ONE_TO_ONE":
		return doclinkv1.Cardinality_CARDINALITY_ONE_TO_ONE
	case "MANY_TO_ONE":
		return doclinkv1.Cardinality_CARDINALITY_MANY_TO_ONE
	case "ONE_TO_MANY":
		return doclinkv1.Cardinality_CARDINALITY_ONE_TO_MANY
	case "MANY_TO_MANY":
		return doclinkv1.Cardinality_CARDINALITY_MANY_TO_MANY
	default:
		return doclinkv1.Cardinality_CARDINALITY_UNSPECIFIED
	}
}

func embedKindName(k doclinkv1.EmbedKind) string {
	if k == doclinkv1.EmbedKind_EMBED_KIND_CUSTOM_ELEMENT {
		return "CUSTOM_ELEMENT"
	}
	return "IFRAME"
}

func embedKindValue(s string) doclinkv1.EmbedKind {
	if s == "CUSTOM_ELEMENT" {
		return doclinkv1.EmbedKind_EMBED_KIND_CUSTOM_ELEMENT
	}
	return doclinkv1.EmbedKind_EMBED_KIND_IFRAME
}

// splitTypeKey splits "pim/item" into ("pim", "item").
func splitTypeKey(key string) (namespace, typ string) {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}
