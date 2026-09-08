// Command bench measures what the decoupling costs.
//
// It asks the same question — "what is linked to this item?" — three ways:
//
//	FEDERATED  the registry fans out over ConnectRPC to every declared satellite
//	INDEXED    the registry reads its own materialized index, then hydrates
//	MONOLITH   one SQL query against a schema where satellites are columns
//
// and sweeps the number of attached satellites, because the interesting cost is
// not the two-module case that fits on a slide. Synthetic satellites are added
// on top of the two real ones so the sweep can reach sixteen modules without
// sixteen deployments.
//
// MONOLITH is measured only at the baseline satellite count. That is not an
// oversight: extending it would require a schema migration on the anchor's table
// per satellite, which is the cost the whole design exists to avoid, and which
// no benchmark harness can perform on the anchor team's behalf.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"github.com/raveesh/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh/doclink/internal/pg"
	"github.com/raveesh/doclink/internal/serve"
)

var (
	registryURL = flag.String("registry", "http://localhost:8080", "doclink registry base URL")
	duration    = flag.Duration("duration", 5*time.Second, "measurement window per configuration")
	warmup      = flag.Duration("warmup", 1*time.Second, "warmup per configuration, not recorded")
	concurrency = flag.Int("concurrency", 16, "concurrent in-flight requests")
	satCounts   = flag.String("satellites", "0,2,4,8,16", "synthetic satellites to add, comma separated")
	docsPerSat  = flag.Int("docs-per-satellite", 3, "documents each synthetic satellite returns per item")
	satLatency  = flag.Duration("satellite-latency", 2*time.Millisecond,
		"artificial latency injected into each synthetic satellite, modelling a non-local service")
	sampleItems = flag.Int("sample-items", 500, "items to draw requests from")
	refsOnly    = flag.Bool("refs-only", false, "skip summary hydration (changes the INDEXED result substantially)")
	outPath     = flag.String("out", "bench/results/latest.json", "where to write the JSON report")
)

type result struct {
	Strategy       string  `json:"strategy"`
	Satellites     int     `json:"satellites"`
	TotalSatellite int     `json:"total_satellites_in_fanout"`
	Requests       int64   `json:"requests"`
	Errors         int64   `json:"errors"`
	Throughput     float64 `json:"requests_per_second"`
	P50Micros      int64   `json:"p50_micros"`
	P95Micros      int64   `json:"p95_micros"`
	P99Micros      int64   `json:"p99_micros"`
	MaxMicros      int64   `json:"max_micros"`
	MeanFanoutRPCs float64 `json:"mean_fanout_rpcs"`
	MeanDocs       float64 `json:"mean_documents_returned"`
}

type report struct {
	RanAt           time.Time `json:"ran_at"`
	Duration        string    `json:"duration_per_config"`
	Concurrency     int       `json:"concurrency"`
	SatelliteMillis float64   `json:"synthetic_satellite_latency_ms"`
	DocsPerSat      int       `json:"docs_per_synthetic_satellite"`
	RefsOnly        bool      `json:"refs_only"`
	Results         []result  `json:"results"`
}

func main() {
	flag.Parse()
	log := serve.Logger("bench")
	ctx := context.Background()

	counts := parseCounts(*satCounts)
	if len(counts) == 0 {
		log.Error("no satellite counts given")
		os.Exit(1)
	}

	pool, err := pg.Connect(ctx, pg.DSNFromEnv("BENCH_DSN",
		"postgres://svc_bench:doclink-dev@localhost:5433/doclink?sslmode=disable"))
	if err != nil {
		log.Error("database connect failed (is the local Postgres up?)", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	items, err := loadItemIDs(ctx, pool, *sampleItems)
	if err != nil || len(items) == 0 {
		log.Error("could not load sample items; run cmd/seed first", "err", err, "found", len(items))
		os.Exit(1)
	}
	log.Info("loaded sample", "items", len(items))

	registry := doclinkv1connect.NewRegistryServiceClient(
		&http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 128, MaxIdleConns: 512},
		},
		*registryURL,
	)

	baseline, err := countFanout(ctx, registry)
	if err != nil {
		log.Error("registry unreachable", "err", err)
		os.Exit(1)
	}
	log.Info("baseline fan-out plan", "satellites", baseline)

	rep := report{
		RanAt:           time.Now(),
		Duration:        duration.String(),
		Concurrency:     *concurrency,
		SatelliteMillis: float64(satLatency.Microseconds()) / 1000,
		DocsPerSat:      *docsPerSat,
		RefsOnly:        *refsOnly,
	}

	for _, n := range counts {
		sats, err := startSatellites(ctx, registry, pool, items, n, log)
		if err != nil {
			log.Error("synthetic satellites unusable", "n", n, "err", err)
			stopSatellites(ctx, registry, pool, sats, log)
			os.Exit(1)
		}

		strategies := []doclinkv1.ResolutionStrategy{
			doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_FEDERATED,
			doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_INDEXED,
		}
		if n == 0 {
			// The only configuration where the straw-man is a like-for-like
			// comparison: exactly the satellites it was hand-written for.
			strategies = append(strategies,
				doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_MONOLITH)
		}

		for _, s := range strategies {
			r := run(ctx, registry, items, s, n, baseline+n)
			rep.Results = append(rep.Results, r)
			log.Info("measured", "strategy", r.Strategy, "synthetic_satellites", n,
				"p50_us", r.P50Micros, "p99_us", r.P99Micros, "rps", int(r.Throughput))
		}

		stopSatellites(ctx, registry, pool, sats, log)
	}

	printTable(rep)
	if err := writeReport(rep, *outPath); err != nil {
		log.Warn("could not write report", "path", *outPath, "err", err)
	} else {
		fmt.Printf("\nreport written to %s\n", *outPath)
	}
}

func parseCounts(s string) []int {
	var out []int
	for _, part := range splitComma(s) {
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err == nil && n >= 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r != ' ' {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func loadItemIDs(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT id FROM pim.items ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// countFanout asks how many satellites already answer for pim/item, so the
// report can distinguish "two real modules" from "two real plus fourteen
// synthetic".
func countFanout(ctx context.Context, registry doclinkv1connect.RegistryServiceClient) (int, error) {
	resp, err := registry.ListLinkTypes(ctx, connect.NewRequest(&doclinkv1.ListLinkTypesRequest{
		AnchorType: &doclinkv1.DocRef{Namespace: "pim", Type: "item"},
	}))
	if err != nil {
		return 0, err
	}
	return len(resp.Msg.LinkTypes), nil
}

func startSatellites(ctx context.Context, registry doclinkv1connect.RegistryServiceClient,
	pool *pgxpool.Pool, items []string, n int, log *slog.Logger) ([]*fakeSatellite, error) {

	var sats []*fakeSatellite
	for i := 0; i < n; i++ {
		fs, err := newFakeSatellite(i, *docsPerSat, *satLatency)
		if err != nil {
			return sats, err
		}
		if _, err := registry.RegisterDocumentType(ctx, connect.NewRequest(
			&doclinkv1.RegisterDocumentTypeRequest{DocumentType: fs.documentType()})); err != nil {
			return sats, err
		}
		if _, err := registry.RegisterLinkType(ctx, connect.NewRequest(
			&doclinkv1.RegisterLinkTypeRequest{LinkType: fs.declaration()})); err != nil {
			return sats, err
		}
		sats = append(sats, fs)
	}

	if n > 0 {
		// The INDEXED strategy reads rows, so the synthetic satellites' links
		// must exist in the index too, or INDEXED would be measured answering an
		// easier question than FEDERATED.
		if err := seedIndex(ctx, pool, items, sats); err != nil {
			return sats, err
		}
		if err := verifyReachable(ctx, registry, items[0], sats); err != nil {
			return sats, err
		}
		log.Info("synthetic satellites up", "count", n,
			"index_rows", len(items)*n*(*docsPerSat))
	}
	return sats, nil
}

// verifyReachable proves the registry can actually call the synthetic
// satellites before anything is measured.
//
// They listen on 127.0.0.1 in this process, so a registry running inside a
// cluster cannot reach them: the fan-out would fail per-group, GetLinks would
// dutifully return the remaining groups, and the harness would report a
// confidently wrong number. Failing loudly here is the difference between a
// benchmark and a decoration.
func verifyReachable(ctx context.Context, registry doclinkv1connect.RegistryServiceClient,
	sampleItem string, sats []*fakeSatellite) error {

	resp, err := registry.GetLinks(ctx, connect.NewRequest(&doclinkv1.GetLinksRequest{
		Anchor:   &doclinkv1.DocRef{Namespace: "pim", Type: "item", Id: sampleItem},
		Strategy: doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_FEDERATED,
	}))
	if err != nil {
		return fmt.Errorf("probe GetLinks: %w", err)
	}

	seen := map[string]int{}
	for _, g := range resp.Msg.Groups {
		if g.Error != "" {
			seen[g.Namespace] = -1
			continue
		}
		seen[g.Namespace] = len(g.Documents)
	}

	for _, fs := range sats {
		switch seen[fs.namespace] {
		case 0:
			return fmt.Errorf(
				"synthetic satellite %s registered but the registry got no documents from it.\n"+
					"  Synthetic satellites listen on 127.0.0.1 in the harness process, so a\n"+
					"  registry running inside a cluster cannot reach them. Run the sweep against\n"+
					"  a local registry (make bench), or use -satellites 0 against a cluster.",
				fs.namespace)
		case -1:
			return fmt.Errorf(
				"registry could not call synthetic satellite %s at %s (see its error group);\n"+
					"  same cause as above if the registry is not in this process's network namespace",
				fs.namespace, fs.endpoint)
		}
	}
	return nil
}

func seedIndex(ctx context.Context, pool *pgxpool.Pool, items []string, sats []*fakeSatellite) error {
	batch := 0
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, item := range items {
		anchorRef := "pim/item/" + item
		for _, fs := range sats {
			for i := 0; i < fs.docsPerAnchor; i++ {
				sourceRef := fmt.Sprintf("%s/%s/%s-%d", fs.namespace, fs.typeName(), item, i)
				_, err := tx.Exec(ctx,
					`INSERT INTO doclink.link_index
					   (anchor_ref, source_ref, predicate, cardinality, namespace)
					 VALUES ($1,$2,'annotates','MANY_TO_MANY',$3)
					 ON CONFLICT DO NOTHING`,
					anchorRef, sourceRef, fs.namespace)
				if err != nil {
					return err
				}
				batch++
			}
		}
	}
	return tx.Commit(ctx)
}

func stopSatellites(ctx context.Context, registry doclinkv1connect.RegistryServiceClient,
	pool *pgxpool.Pool, sats []*fakeSatellite, log *slog.Logger) {

	for _, fs := range sats {
		if _, err := registry.Deregister(ctx, connect.NewRequest(&doclinkv1.DeregisterRequest{
			LinkTypeId:      fs.linkTypeID(),
			DocumentTypeKey: fs.namespace + "/" + fs.typeName(),
		})); err != nil {
			log.Warn("deregister failed; the fan-out plan may be dirty",
				"namespace", fs.namespace, "err", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM doclink.link_index WHERE namespace = $1`, fs.namespace); err != nil {
			log.Warn("index cleanup failed", "namespace", fs.namespace, "err", err)
		}
		fs.stop(log)
	}
}

func run(ctx context.Context, registry doclinkv1connect.RegistryServiceClient,
	items []string, strategy doclinkv1.ResolutionStrategy, synthetic, totalFanout int) result {

	measure := func(window time.Duration, record bool) ([]int64, int64, int64, int64, int64) {
		deadline := time.Now().Add(window)
		var (
			mu       sync.Mutex
			samples  []int64
			requests atomic.Int64
			errs     atomic.Int64
			rpcs     atomic.Int64
			docs     atomic.Int64
			wg       sync.WaitGroup
		)

		for w := 0; w < *concurrency; w++ {
			wg.Add(1)
			go func(seed int64) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(seed))
				var local []int64
				for time.Now().Before(deadline) {
					id := items[rng.Intn(len(items))]
					began := time.Now()
					resp, err := registry.GetLinks(ctx, connect.NewRequest(&doclinkv1.GetLinksRequest{
						Anchor:   &doclinkv1.DocRef{Namespace: "pim", Type: "item", Id: id},
						Strategy: strategy,
						RefsOnly: *refsOnly,
					}))
					elapsed := time.Since(began).Microseconds()
					requests.Add(1)
					if err != nil {
						errs.Add(1)
						continue
					}
					rpcs.Add(int64(resp.Msg.FanoutRpcs))
					for _, g := range resp.Msg.Groups {
						docs.Add(int64(len(g.Documents)))
					}
					if record {
						local = append(local, elapsed)
					}
				}
				if record && len(local) > 0 {
					mu.Lock()
					samples = append(samples, local...)
					mu.Unlock()
				}
			}(time.Now().UnixNano() + int64(w))
		}
		wg.Wait()
		return samples, requests.Load(), errs.Load(), rpcs.Load(), docs.Load()
	}

	// Warmup fills connection pools and Postgres' cache. Without it the first
	// strategy measured would carry the cost of every other strategy's setup.
	_, _, _, _, _ = measure(*warmup, false)

	started := time.Now()
	samples, requests, errs, rpcs, docs := measure(*duration, true)
	wall := time.Since(started).Seconds()

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	r := result{
		Strategy:       strategyName(strategy),
		Satellites:     synthetic,
		TotalSatellite: totalFanout,
		Requests:       requests,
		Errors:         errs,
		Throughput:     float64(requests) / wall,
	}
	if n := len(samples); n > 0 {
		r.P50Micros = samples[percentileIndex(n, 0.50)]
		r.P95Micros = samples[percentileIndex(n, 0.95)]
		r.P99Micros = samples[percentileIndex(n, 0.99)]
		r.MaxMicros = samples[n-1]
	}
	if ok := requests - errs; ok > 0 {
		r.MeanFanoutRPCs = float64(rpcs) / float64(ok)
		r.MeanDocs = float64(docs) / float64(ok)
	}
	return r
}

func percentileIndex(n int, p float64) int {
	i := int(float64(n) * p)
	if i >= n {
		i = n - 1
	}
	return i
}

func strategyName(s doclinkv1.ResolutionStrategy) string {
	switch s {
	case doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_FEDERATED:
		return "FEDERATED"
	case doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_INDEXED:
		return "INDEXED"
	case doclinkv1.ResolutionStrategy_RESOLUTION_STRATEGY_MONOLITH:
		return "MONOLITH"
	default:
		return "UNSPECIFIED"
	}
}

func printTable(rep report) {
	fmt.Printf("\n%-10s %5s %6s %9s %9s %9s %9s %8s %7s\n",
		"STRATEGY", "SYNTH", "FANOUT", "p50 ms", "p95 ms", "p99 ms", "max ms", "req/s", "docs")
	fmt.Println("---------------------------------------------------------------------------------------")
	for _, r := range rep.Results {
		fmt.Printf("%-10s %5d %6d %9.2f %9.2f %9.2f %9.2f %8.0f %7.1f\n",
			r.Strategy, r.Satellites, r.TotalSatellite,
			float64(r.P50Micros)/1000, float64(r.P95Micros)/1000,
			float64(r.P99Micros)/1000, float64(r.MaxMicros)/1000,
			r.Throughput, r.MeanDocs)
		if r.Errors > 0 {
			fmt.Printf("%-10s   errors: %d of %d requests\n", "", r.Errors, r.Requests)
		}
	}
	fmt.Printf("\nsynthetic satellite latency: %.1f ms each · %d docs each · refs_only=%v\n",
		rep.SatelliteMillis, rep.DocsPerSat, rep.RefsOnly)
}

func writeReport(rep report, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}
