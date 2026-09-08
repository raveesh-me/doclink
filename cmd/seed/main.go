// Command seed populates every schema with the same logical dataset.
//
// It writes the federated world (pim + subscriptions + shipping), the registry's
// link index, and the monolith baseline, so all three resolution strategies are
// answering questions about identical data. Any latency difference the benchmark
// reports is therefore a property of the strategy and not of the data.
//
// The seeder connects as svc_bench, the only role allowed to see across schema
// boundaries. No production service could do what this program does.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raveesh/doclink/internal/ids"
	"github.com/raveesh/doclink/internal/pg"
)

var (
	itemCount    = flag.Int("items", 1000, "number of items to create")
	planCount    = flag.Int("plans", 8, "number of subscription plans")
	profileCount = flag.Int("profiles", 6, "number of shipping profiles")
	maxPlansPer  = flag.Int("max-plans-per-item", 3, "upper bound on plans linked to one item")
	shipRatio    = flag.Float64("shipping-ratio", 0.8, "fraction of items with a shipping profile")
	truncate     = flag.Bool("truncate", true, "wipe existing data first")
	seedValue    = flag.Int64("seed", 42, "PRNG seed, so runs are reproducible")
)

func main() {
	flag.Parse()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(*seedValue))

	dsn := pg.DSNFromEnv("BENCH_DSN",
		"postgres://svc_bench:doclink-dev@localhost:5432/doclink?sslmode=disable")
	pool, err := pg.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if *truncate {
		if err := wipe(ctx, pool); err != nil {
			log.Fatalf("truncate: %v", err)
		}
		fmt.Println("wiped existing data")
	}

	plans := makePlans(*planCount, rng)
	profiles := makeProfiles(*profileCount, rng)

	if err := insertPlans(ctx, pool, plans); err != nil {
		log.Fatalf("plans: %v", err)
	}
	if err := insertProfiles(ctx, pool, profiles); err != nil {
		log.Fatalf("profiles: %v", err)
	}
	fmt.Printf("seeded %d plans, %d shipping profiles\n", len(plans), len(profiles))

	start := time.Now()
	links := 0
	for i := 0; i < *itemCount; i++ {
		itemID := ids.New()
		if err := insertItem(ctx, pool, itemID, i, rng); err != nil {
			log.Fatalf("item %d: %v", i, err)
		}
		n, err := linkItem(ctx, pool, itemID, plans, profiles, rng)
		if err != nil {
			log.Fatalf("link item %d: %v", i, err)
		}
		links += n

		if (i+1)%500 == 0 {
			fmt.Printf("  %d/%d items\n", i+1, *itemCount)
		}
	}

	fmt.Printf("seeded %d items and %d links in %s\n", *itemCount, links, time.Since(start).Round(time.Millisecond))
	fmt.Println("\nAll three strategies now describe the same data:")
	fmt.Println("  FEDERATED -> subscriptions.coverage + shipping.assignments")
	fmt.Println("  INDEXED   -> doclink.link_index")
	fmt.Println("  MONOLITH  -> monolith.item_plans + monolith.items.shipping_profile_id")
	os.Exit(0)
}

func wipe(ctx context.Context, pool *pgxpool.Pool) error {
	// Order matters only within a schema; across schemas there are no foreign
	// keys, which is the entire premise.
	stmts := []string{
		`TRUNCATE monolith.item_plans, monolith.items, monolith.plans, monolith.profiles CASCADE`,
		`TRUNCATE subscriptions.coverage, subscriptions.plans CASCADE`,
		`TRUNCATE shipping.assignments, shipping.profiles CASCADE`,
		`TRUNCATE doclink.link_index`,
		`TRUNCATE pim.items, pim.outbox, pim.sequences CASCADE`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

type plan struct {
	id, name, interval string
	discountBps        int32
	minCycles          int32
}

type profile struct {
	id, name, carrier string
	handlingDays      int32
	requiresSignature bool
}

func makePlans(n int, rng *rand.Rand) []plan {
	intervals := []string{"weekly", "monthly", "quarterly"}
	out := make([]plan, n)
	for i := range out {
		iv := intervals[i%len(intervals)]
		out[i] = plan{
			id:          ids.Prefixed("plan"),
			name:        fmt.Sprintf("%s saver %d", iv, i+1),
			interval:    iv,
			discountBps: int32(250 + rng.Intn(1250)),
			minCycles:   int32(1 + rng.Intn(6)),
		}
	}
	return out
}

func makeProfiles(n int, rng *rand.Rand) []profile {
	carriers := []string{"UPS", "FedEx", "DHL", "USPS", "Royal Mail", "DPD"}
	out := make([]profile, n)
	for i := range out {
		c := carriers[i%len(carriers)]
		out[i] = profile{
			id:                ids.Prefixed("prof"),
			name:              fmt.Sprintf("%s standard", c),
			carrier:           c,
			handlingDays:      int32(1 + rng.Intn(4)),
			requiresSignature: rng.Intn(3) == 0,
		}
	}
	return out
}

func insertPlans(ctx context.Context, pool *pgxpool.Pool, plans []plan) error {
	for _, p := range plans {
		if _, err := pool.Exec(ctx,
			`INSERT INTO subscriptions.plans (id, name, interval, discount_bps, min_cycles)
			 VALUES ($1,$2,$3,$4,$5)`,
			p.id, p.name, p.interval, p.discountBps, p.minCycles); err != nil {
			return err
		}
		// Same plan, same id, in the straw-man schema.
		if _, err := pool.Exec(ctx,
			`INSERT INTO monolith.plans (id, name, interval, discount_bps, min_cycles)
			 VALUES ($1,$2,$3,$4,$5)`,
			p.id, p.name, p.interval, p.discountBps, p.minCycles); err != nil {
			return err
		}
	}
	return nil
}

func insertProfiles(ctx context.Context, pool *pgxpool.Pool, profiles []profile) error {
	for _, p := range profiles {
		if _, err := pool.Exec(ctx,
			`INSERT INTO shipping.profiles (id, name, carrier, handling_days, requires_signature)
			 VALUES ($1,$2,$3,$4,$5)`,
			p.id, p.name, p.carrier, p.handlingDays, p.requiresSignature); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO monolith.profiles (id, name, carrier, handling_days, requires_signature)
			 VALUES ($1,$2,$3,$4,$5)`,
			p.id, p.name, p.carrier, p.handlingDays, p.requiresSignature); err != nil {
			return err
		}
	}
	return nil
}

var adjectives = []string{"Merino", "Organic", "Recycled", "Heavyweight", "Featherlight",
	"Waxed", "Brushed", "Ripstop", "Selvedge", "Quilted"}
var nouns = []string{"crew tee", "field jacket", "chore coat", "hiking sock", "canvas tote",
	"beanie", "overshirt", "trail pant", "camp mug", "duffel"}

func insertItem(ctx context.Context, pool *pgxpool.Pool, id string, n int, rng *rand.Rand) error {
	title := fmt.Sprintf("%s %s", adjectives[rng.Intn(len(adjectives))], nouns[rng.Intn(len(nouns))])
	sku := fmt.Sprintf("SKU-%06d", n)
	price := int64(1500 + rng.Intn(28500))
	desc := "Seeded fixture for the doc-link benchmark."

	if _, err := pool.Exec(ctx,
		`INSERT INTO pim.items (id, sku, title, description, price_cents, currency, status)
		 VALUES ($1,$2,$3,$4,$5,'USD','active')`,
		id, sku, title, desc, price); err != nil {
		return err
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO monolith.items (id, sku, title, description, price_cents, currency, status)
		 VALUES ($1,$2,$3,$4,$5,'USD','active')`,
		id, sku, title, desc, price)
	return err
}

// linkItem writes the same logical links into all three representations.
func linkItem(ctx context.Context, pool *pgxpool.Pool, itemID string,
	plans []plan, profiles []profile, rng *rand.Rand) (int, error) {

	anchorRef := "pim/item/" + itemID
	count := 0

	// Subscriptions: many-to-many, zero or more plans per item.
	nPlans := rng.Intn(*maxPlansPer + 1)
	chosen := rng.Perm(len(plans))[:nPlans]
	for _, pi := range chosen {
		p := plans[pi]
		maxQty := int32(1 + rng.Intn(5))
		prepaid := rng.Intn(4) == 0

		if _, err := pool.Exec(ctx,
			`INSERT INTO subscriptions.coverage
			   (id, plan_id, anchor_ref, max_quantity_per_cycle, prepaid_only)
			 VALUES ($1,$2,$3,$4,$5)`,
			ids.New(), p.id, anchorRef, maxQty, prepaid); err != nil {
			return count, err
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO monolith.item_plans (item_id, plan_id, max_quantity_per_cycle, prepaid_only)
			 VALUES ($1,$2,$3,$4)`,
			itemID, p.id, maxQty, prepaid); err != nil {
			return count, err
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO doclink.link_index (anchor_ref, source_ref, predicate, cardinality, namespace)
			 VALUES ($1,$2,'covers','MANY_TO_MANY','subscriptions')
			 ON CONFLICT DO NOTHING`,
			anchorRef, "subscriptions/plan/"+p.id); err != nil {
			return count, err
		}
		count++
	}

	// Shipping: many-to-one, at most one profile per item.
	if rng.Float64() < *shipRatio {
		pr := profiles[rng.Intn(len(profiles))]
		weight := int32(50 + rng.Intn(4000))
		dims := fmt.Sprintf("%dx%dx%d", 10+rng.Intn(40), 10+rng.Intn(30), 2+rng.Intn(20))
		hazmat := rng.Intn(20) == 0

		if _, err := pool.Exec(ctx,
			`INSERT INTO shipping.assignments
			   (id, profile_id, anchor_ref, weight_grams, dimensions_cm, hazmat)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			ids.New(), pr.id, anchorRef, weight, dims, hazmat); err != nil {
			return count, err
		}
		if _, err := pool.Exec(ctx,
			`UPDATE monolith.items
			 SET shipping_profile_id = $2, shipping_weight_grams = $3,
			     shipping_dimensions_cm = $4, shipping_hazmat = $5
			 WHERE id = $1`,
			itemID, pr.id, weight, dims, hazmat); err != nil {
			return count, err
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO doclink.link_index (anchor_ref, source_ref, predicate, cardinality, namespace)
			 VALUES ($1,$2,'ships_via','MANY_TO_ONE','shipping')
			 ON CONFLICT DO NOTHING`,
			anchorRef, "shipping/profile/"+pr.id); err != nil {
			return count, err
		}
		count++
	}

	return count, nil
}
