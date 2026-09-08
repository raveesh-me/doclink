// Command pim is the anchor service: it owns items, and knows nothing about
// anything that links to them.
package main

import (
	"context"
	"net/http"
	"os"

	doclinkv1 "github.com/raveesh-me/doclink/gen/doclink/v1"
	"github.com/raveesh-me/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh-me/doclink/gen/pim/v1/pimv1connect"
	"github.com/raveesh-me/doclink/internal/pg"
	"github.com/raveesh-me/doclink/internal/registryclient"
	"github.com/raveesh-me/doclink/internal/serve"
)

func main() {
	log := serve.Logger("pim")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsn := pg.DSNFromEnv("PIM_DSN",
		"postgres://svc_pim:doclink-dev@localhost:5432/doclink?sslmode=disable")
	pool, err := pg.Connect(ctx, dsn)
	if err != nil {
		log.Error("database connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Migrations are NOT run here. They create login roles, which needs a
	// superuser connection that no service should hold. See cmd/migrate, which
	// runs as a Job before these Deployments roll.

	st := &store{pool: pool}
	registryEndpoint := envOr("DOCLINK_ENDPOINT", "http://localhost:8080")
	selfEndpoint := envOr("SELF_ENDPOINT", "http://localhost:8081")

	mux := http.NewServeMux()
	mux.Handle(pimv1connect.NewItemServiceHandler(&itemService{store: st}))
	mux.Handle(doclinkv1connect.NewDocumentResolverHandler(&documentResolver{store: st}))

	// Announce what PIM owns and what it exposes. Note there is no list of
	// satellites here: PIM declares the slot, not who fills it.
	rc := registryclient.New(registryEndpoint, log)
	go rc.Announce(ctx, registryclient.Registration{
		DocumentTypes: []*doclinkv1.DocumentType{{
			Namespace:        "pim",
			Type:             "item",
			DisplayName:      "Item",
			ResolverEndpoint: selfEndpoint,
		}},
		ExtensionPoints: []*doclinkv1.ExtensionPoint{{
			Id:          "pim/item:detail.card",
			AnchorType:  &doclinkv1.DocRef{Namespace: "pim", Type: "item"},
			DisplayName: "Item detail cards",
			Description: "Cards rendered on the item detail screen. " +
				"The host passes exactly one thing: the item's DocRef.",
		}},
	})

	go newRelay(st, registryEndpoint, log).Run(ctx)

	if err := serve.Run(envOr("ADDR", ":8081"), mux, log); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
