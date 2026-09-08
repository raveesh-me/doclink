// Command subscriptions is a satellite. It attaches its own data to items it
// does not own, using nothing but an item's DocRef.
package main

import (
	"context"
	"net/http"
	"os"

	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"github.com/raveesh/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh/doclink/gen/subscriptions/v1/subscriptionsv1connect"
	"github.com/raveesh/doclink/internal/pg"
	"github.com/raveesh/doclink/internal/registryclient"
	"github.com/raveesh/doclink/internal/serve"
)

func main() {
	log := serve.Logger("subscriptions")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pg.Connect(ctx, pg.DSNFromEnv("SUBSCRIPTIONS_DSN",
		"postgres://svc_subscriptions:doclink-dev@localhost:5432/doclink?sslmode=disable"))
	if err != nil {
		log.Error("database connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	st := &store{pool: pool}
	registryEndpoint := envOr("DOCLINK_ENDPOINT", "http://localhost:8080")
	selfEndpoint := envOr("SELF_ENDPOINT", "http://localhost:8082")
	embedBase := envOr("EMBED_BASE_URL", "http://localhost:5182")

	rc := registryclient.New(registryEndpoint, log)

	mux := http.NewServeMux()
	mux.Handle(subscriptionsv1connect.NewSubscriptionsServiceHandler(
		&subscriptionsService{store: st, registry: rc, log: log}))
	mux.Handle(doclinkv1connect.NewLinkResolverHandler(&linkResolver{store: st, log: log}))
	mux.Handle(doclinkv1connect.NewDocumentResolverHandler(&documentResolver{store: st}))

	// Everything this service tells the world about itself. There is no
	// corresponding entry anywhere in PIM: the item screen learns about
	// subscriptions by asking the registry at page load, not at build time.
	go rc.Announce(ctx, registryclient.Registration{
		DocumentTypes: []*doclinkv1.DocumentType{{
			Namespace:        "subscriptions",
			Type:             "plan",
			DisplayName:      "Subscription plan",
			ResolverEndpoint: selfEndpoint,
		}},
		Contributions: []*doclinkv1.Contribution{{
			Id:               "subscriptions:item-detail-card",
			ExtensionPointId: "pim/item:detail.card",
			Namespace:        "subscriptions",
			Title:            "Subscriptions",
			Icon:             "🔁",
			EmbedKind:        doclinkv1.EmbedKind_EMBED_KIND_IFRAME,
			EmbedUrl:         embedBase + "/embed/item-card.html",
			ServiceEndpoint:  selfEndpoint,
			Weight:           100,
		}},
		LinkTypes: []*doclinkv1.LinkTypeDecl{{
			Id:                    "subscriptions:plan-covers-item",
			AnchorType:            &doclinkv1.DocRef{Namespace: "pim", Type: "item"},
			SourceType:            &doclinkv1.DocRef{Namespace: "subscriptions", Type: "plan"},
			Predicate:             "covers",
			Cardinality:           doclinkv1.Cardinality_CARDINALITY_MANY_TO_MANY,
			ResolverEndpoint:      selfEndpoint,
			SubscribesToLifecycle: true,
		}},
	})

	if err := serve.Run(envOr("ADDR", ":8082"), mux, log); err != nil {
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
