// Command doclink is the registry: the only component that knows which services
// link to which, and it knows it as rows rather than as code.
package main

import (
	"context"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raveesh/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh/doclink/internal/pg"
	"github.com/raveesh/doclink/internal/serve"
)

func main() {
	log := serve.Logger("doclink")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pg.Connect(ctx, pg.DSNFromEnv("DOCLINK_DSN",
		"postgres://svc_doclink:doclink-dev@localhost:5432/doclink?sslmode=disable"))
	if err != nil {
		log.Error("database connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// A second pool as svc_bench, used only by the MONOLITH strategy. It is
	// optional: without it the registry simply refuses that strategy, and the
	// real design is unaffected.
	var benchPool *pgxpool.Pool
	if dsn := os.Getenv("BENCH_DSN"); dsn != "" {
		benchPool, err = pg.Connect(ctx, dsn)
		if err != nil {
			log.Warn("bench pool unavailable; MONOLITH strategy disabled", "err", err)
		} else {
			defer benchPool.Close()
			log.Info("monolith baseline enabled")
		}
	}

	st := &store{pool: pool}
	res := newResolver(st, benchPool, log)
	disp := newDispatcher(st, res, log)

	mux := http.NewServeMux()
	mux.Handle(doclinkv1connect.NewRegistryServiceHandler(&registryService{
		store: st, resolver: res, dispatcher: disp,
	}))

	go disp.Run(ctx)

	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	if err := serve.Run(addr, mux, log); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}
