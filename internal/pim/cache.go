package pim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/raveesh-me/doclink/internal/registry"
)

const defaultMaxStale = 10 * time.Minute

// Cache holds fragments PIM has already pulled from modules.
//
// It is a cache, not a record. The module stays the source of truth; every
// entry is evictable and reconstructible by calling the module again; and
// nothing ever queries it — the engine reads it by exact key, and there is no
// way to ask "which products have tax class reduced". A table that could answer
// that would be a second source of truth wearing a cache's clothing.
//
// Keyed (tenant, module, extension point, product, context hash). The context
// hash covers only the keys the point declared in Describe, so an undeclared
// key such as channel does not split the cache.
type Cache struct {
	// MaxStale is how long past expiry an entry is kept for stale-on-failure.
	MaxStale time.Duration
	Now      func() time.Time

	mu      sync.Mutex
	modules map[cacheModuleKey]*moduleEntries
}

type cacheModuleKey struct{ tenant, module string }

type moduleEntries struct {
	// generation increments on every invalidation, so a read that was already
	// in flight cannot write back what the module just said is out of date.
	generation uint64
	products   map[string]map[pointKey]cacheEntry
}

type pointKey struct{ point, contextHash string }

type cacheEntry struct {
	// data is nil when the module returned no fragment for the point; absence
	// is an answer and is cached like one.
	data    *structpb.Struct
	expires time.Time
}

// pointData is one product's fragments from one module, by point name.
type pointData map[string]*structpb.Struct

var _ registry.Invalidator = (*Cache)(nil)

func NewCache() *Cache {
	return &Cache{MaxStale: defaultMaxStale, Now: time.Now}
}

// contextHashes returns each point's context hash for a request.
func contextHashes(m registry.Module, reqCtx map[string]string) map[string]string {
	out := make(map[string]string, len(m.Points))
	for _, p := range m.Points {
		out[p.Name] = contextHash(p.ContextKeys, reqCtx)
	}
	return out
}

// contextHash is a stable hash of the declared keys only. A missing key and a
// key set to "" hash differently; key order does not matter.
func contextHash(keys []string, reqCtx map[string]string) string {
	if len(keys) == 0 {
		return ""
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, k := range sorted {
		v, ok := reqCtx[k]
		fmt.Fprintf(h, "%d:%s|%t|%d:%s;", len(k), k, ok, len(v), v)
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func (c *Cache) entries(tenant, module string, create bool) *moduleEntries {
	k := cacheModuleKey{tenant, module}
	me := c.modules[k]
	if me == nil && create {
		if c.modules == nil {
			c.modules = map[cacheModuleKey]*moduleEntries{}
		}
		me = &moduleEntries{products: map[string]map[pointKey]cacheEntry{}}
		c.modules[k] = me
	}
	return me
}

// Generation is read before a module call and handed back to Put.
func (c *Cache) Generation(tenant, module string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if me := c.entries(tenant, module, false); me != nil {
		return me.generation
	}
	return 0
}

// Fresh returns a product's fragments if every point of the module has an
// unexpired entry. A point with cache_ttl_seconds 0 is never stored, so a
// module with one is never a hit.
func (c *Cache) Fresh(tenant string, m registry.Module, productID string, hashes map[string]string) (pointData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	me := c.entries(tenant, m.ModuleID, false)
	if me == nil {
		return nil, false
	}
	now := c.Now()
	byPoint := me.products[productID]
	out := pointData{}
	for _, p := range m.Points {
		e, ok := byPoint[pointKey{p.Name, hashes[p.Name]}]
		if !ok || !now.Before(e.expires) {
			return nil, false
		}
		if e.data != nil {
			out[p.Name] = e.data
		}
	}
	return out, true
}

// Stale returns whatever entries a product still has within MaxStale of expiry,
// fresh ones included. Used only when the module has just failed a FAIL_OPEN
// read.
func (c *Cache) Stale(tenant string, m registry.Module, productID string, hashes map[string]string) (pointData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	me := c.entries(tenant, m.ModuleID, false)
	if me == nil {
		return nil, false
	}
	now := c.Now()
	byPoint := me.products[productID]
	out, found := pointData{}, false
	for _, p := range m.Points {
		e, ok := byPoint[pointKey{p.Name, hashes[p.Name]}]
		if !ok || !now.Before(e.expires.Add(c.MaxStale)) {
			continue
		}
		found = true
		if e.data != nil {
			out[p.Name] = e.data
		}
	}
	return out, found
}

// Put stores a product's freshly fetched fragments for every point with a
// positive TTL. It stores nothing if the module was invalidated since
// generation was read.
func (c *Cache) Put(tenant string, m registry.Module, productID string, hashes map[string]string, data pointData, generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	me := c.entries(tenant, m.ModuleID, true)
	if me.generation != generation {
		return false
	}
	now := c.Now()
	for _, p := range m.Points {
		if p.CacheTTL <= 0 {
			continue
		}
		byPoint := me.products[productID]
		if byPoint == nil {
			byPoint = map[pointKey]cacheEntry{}
			me.products[productID] = byPoint
		}
		byPoint[pointKey{p.Name, hashes[p.Name]}] = cacheEntry{data: data[p.Name], expires: now.Add(p.CacheTTL)}
	}
	return true
}

// Invalidate evicts a module's entries for the given products, or for every
// product when productIDs is empty, and returns how many entries went.
func (c *Cache) Invalidate(tenant, module string, productIDs []string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	me := c.entries(tenant, module, true)
	me.generation++
	n := 0
	if len(productIDs) == 0 {
		for _, byPoint := range me.products {
			n += len(byPoint)
		}
		me.products = map[string]map[pointKey]cacheEntry{}
		return n
	}
	for _, id := range productIDs {
		n += len(me.products[id])
		delete(me.products, id)
	}
	return n
}

// Sweep drops entries past MaxStale.
func (c *Cache) Sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, n := c.Now(), 0
	for _, me := range c.modules {
		for id, byPoint := range me.products {
			for k, e := range byPoint {
				if !now.Before(e.expires.Add(c.MaxStale)) {
					delete(byPoint, k)
					n++
				}
			}
			if len(byPoint) == 0 {
				delete(me.products, id)
			}
		}
	}
	return n
}
