// Command pim serves the PIM API and the module registry on :8080.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/raveesh-me/doclink/gen/pim/v1/pimv1connect"
	"github.com/raveesh-me/doclink/gen/registry/v1/registryv1connect"
	"github.com/raveesh-me/doclink/internal/pim"
	"github.com/raveesh-me/doclink/internal/registry"
	"github.com/raveesh-me/doclink/internal/sqlite"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "data/pim.db", "SQLite database path")
	seed := flag.Bool("seed", true, "seed two tenants and a handful of products into an empty database")
	drainGrace := flag.Duration("drain-grace", 5*time.Second, "how long an unregistered module stays DRAINING before deletion")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*addr, *dbPath, *seed, *drainGrace, log); err != nil {
		log.Error("pim", "err", err)
		os.Exit(1)
	}
}

func run(addr, dbPath string, seed bool, drainGrace time.Duration, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return err
		}
	}
	db, err := sqlite.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	store, err := pim.NewStore(ctx, db)
	if err != nil {
		return err
	}
	if seed {
		seeded, err := store.SeedIfEmpty(ctx)
		if err != nil {
			return err
		}
		if seeded {
			log.Info("seeded products", "tenants", "t1,t2")
		}
	}

	regStore, err := registry.NewStore(ctx, db)
	if err != nil {
		return err
	}
	// A restart cannot have reads in flight, so drains in progress can finish.
	if n, err := regStore.PurgeDraining(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Info("deleted registrations left draining by a previous run", "rows", n)
	}
	schemas := registry.NewSchemas()
	cache := pim.NewCache()
	reg := &registry.Service{
		Store:       regStore,
		Schemas:     schemas,
		HTTPClient:  registry.DefaultHTTPClient,
		DrainGrace:  drainGrace,
		Invalidator: cache,
		Log:         log,
	}
	breakers := pim.NewBreakers(log)
	engine := &pim.Engine{
		Modules:    regStore,
		Schemas:    schemas,
		HTTPClient: registry.DefaultHTTPClient,
		Log:        log,
		Breakers:   breakers,
		Cache:      cache,
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := cache.Sweep(); n > 0 {
					log.Debug("cache sweep", "evicted", n)
				}
			}
		}
	}()

	svc := &pim.Service{Store: store, Composer: engine, Log: log}

	mux := http.NewServeMux()
	mux.Handle(pimv1connect.NewPIMHandler(svc))
	mux.Handle(registryv1connect.NewRegistryHandler(reg))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /openapi/{file}", &pim.SpecHandler{Modules: regStore, Log: log})
	mux.HandleFunc("GET /debug/breakers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(breakers.Snapshot())
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", addr, "db", dbPath)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
