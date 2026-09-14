package pim

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/raveesh-me/doclink/internal/registry"
)

func TestContextHashCoversDeclaredKeysOnly(t *testing.T) {
	keys := []string{"buyer_state"}
	base := contextHash(keys, map[string]string{"buyer_state": "KA"})
	if base == "" {
		t.Fatal("empty hash for a point with context keys")
	}
	if contextHash(keys, map[string]string{"buyer_state": "KA", "channel": "web", "locale": "hi"}) != base {
		t.Error("an undeclared key changed the hash")
	}
	if contextHash(keys, map[string]string{"buyer_state": "MH"}) == base {
		t.Error("a declared key's value did not change the hash")
	}
	if contextHash(keys, map[string]string{}) == contextHash(keys, map[string]string{"buyer_state": ""}) {
		t.Error("missing and empty hash the same")
	}
	if contextHash([]string{"a", "b"}, map[string]string{"a": "1", "b": "2"}) != contextHash([]string{"b", "a"}, map[string]string{"a": "1", "b": "2"}) {
		t.Error("key order changed the hash")
	}
	if contextHash([]string{"a", "b"}, map[string]string{"a": "1|b", "b": ""}) == contextHash([]string{"a", "b"}, map[string]string{"a": "1", "b": "b"}) {
		t.Error("values can be shifted across keys without changing the hash")
	}
	if contextHash(nil, map[string]string{"buyer_state": "KA"}) != "" {
		t.Error("a point with no context keys should hash to a constant")
	}
}

func TestCacheLifecycle(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	c := &Cache{MaxStale: time.Minute, Now: clock.now}
	m := registry.Module{ModuleID: "taxes", Points: []registry.Point{
		{Name: "product.enrich", CacheTTL: 300 * time.Second},
		{Name: "product.enrich.contextual", CacheTTL: 30 * time.Second, ContextKeys: []string{"buyer_state"}},
	}}
	hashes := contextHashes(m, map[string]string{"buyer_state": "KA"})
	data, _ := structpb.NewStruct(map[string]any{"hsn": "8471"})
	pts := pointData{"product.enrich": data} // contextual returned nothing: cached as absence

	if _, ok := c.Fresh("t1", m, "p1", hashes); ok {
		t.Fatal("hit on an empty cache")
	}
	if !c.Put("t1", m, "p1", hashes, pts, c.Generation("t1", "taxes")) {
		t.Fatal("put refused")
	}
	got, ok := c.Fresh("t1", m, "p1", hashes)
	if !ok || got["product.enrich"] == nil || got["product.enrich.contextual"] != nil {
		t.Fatalf("fresh = %v, %v", got, ok)
	}
	if _, ok := c.Fresh("t2", m, "p1", hashes); ok {
		t.Fatal("tenants share cache entries")
	}

	// The shortest TTL decides freshness for the module.
	clock.advance(31 * time.Second)
	if _, ok := c.Fresh("t1", m, "p1", hashes); ok {
		t.Fatal("hit after the contextual point expired")
	}
	if got, ok := c.Stale("t1", m, "p1", hashes); !ok || got["product.enrich"] == nil {
		t.Fatal("stale lookup lost an entry within MaxStale")
	}

	// Past expiry + MaxStale, the contextual entry is gone even for stale use.
	clock.advance(time.Minute)
	if n := c.Sweep(); n != 1 {
		t.Fatalf("swept %d entries, want 1", n)
	}

	// An invalidation between Generation and Put drops the write.
	gen := c.Generation("t1", "taxes")
	if n := c.Invalidate("t1", "taxes", []string{"p1"}); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if c.Put("t1", m, "p1", hashes, pts, gen) {
		t.Fatal("put accepted a write from before an invalidation")
	}
}

func TestCacheTTLZeroIsNeverStored(t *testing.T) {
	c := NewCache()
	m := registry.Module{ModuleID: "m", Points: []registry.Point{{Name: "product.enrich", CacheTTL: 0}}}
	hashes := contextHashes(m, nil)
	c.Put("t1", m, "p1", hashes, pointData{}, 0)
	if _, ok := c.Fresh("t1", m, "p1", hashes); ok {
		t.Fatal("TTL 0 point was cached")
	}
	if _, ok := c.Stale("t1", m, "p1", hashes); ok {
		t.Fatal("TTL 0 point is available as stale")
	}
}
