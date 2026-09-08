// Package ids generates the identifiers used across the POC.
package ids

import (
	"math/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a lexicographically sortable, time-ordered identifier.
//
// ULIDs rather than UUIDv4 because these ids end up as the id segment of a
// DocRef string and therefore appear in URLs, iframe query strings, and index
// keys; monotonic ordering keeps B-tree inserts on the linkage tables from
// scattering, which matters once the benchmark starts writing them in bulk.
func New() string {
	return ulid.Make().String()
}

// Prefixed is for seed data, where a readable id makes a failing benchmark far
// easier to interpret than an opaque one.
func Prefixed(prefix string) string {
	return prefix + "-" + strings.ToLower(ulid.Make().String()[10:])
}

func init() {
	// Keep the seeder's own randomness independent of ULID entropy.
	rand.New(rand.NewSource(time.Now().UnixNano()))
}
