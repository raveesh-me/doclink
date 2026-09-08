// Command shipping is the second satellite. Its embed is deliberately written
// without a frontend framework, to demonstrate that the host contract does not
// impose one.
package main

import (
	"context"
	"net/http"
	"os"

	doclinkv1 "github.com/raveesh-me/doclink/gen/doclink/v1"
	"github.com/raveesh-me/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh-me/doclink/gen/shipping/v1/shippingv1connect"
	"github.com/raveesh-me/doclink/internal/pg"
	"github.com/raveesh-me/doclink/internal/registryclient"
	"github.com/raveesh-me/doclink/internal/serve"
)

func main() {
	log := serve.Logger("shipping")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pg.Connect(ctx, pg.DSNFromEnv("SHIPPING_DSN",
		"postgres://svc_shipping:doclink-dev@localhost:5432/doclink?sslmode=disable"))
	if err != nil {
		log.Error("database connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	st := &store{pool: pool}
	registryEndpoint := envOr("DOCLINK_ENDPOINT", "http://localhost:8080")
	selfEndpoint := envOr("SELF_ENDPOINT", "http://localhost:8083")
	embedBase := envOr("EMBED_BASE_URL", "http://localhost:5183")

	rc := registryclient.New(registryEndpoint, log)

	mux := http.NewServeMux()
	mux.Handle(shippingv1connect.NewShippingServiceHandler(
		&shippingService{store: st, registry: rc, log: log}))
	mux.Handle(doclinkv1connect.NewLinkResolverHandler(&linkResolver{store: st, log: log}))
	mux.Handle(doclinkv1connect.NewDocumentResolverHandler(&documentResolver{store: st}))

	go rc.Announce(ctx, registryclient.Registration{
		DocumentTypes: []*doclinkv1.DocumentType{{
			Namespace:        "shipping",
			Type:             "profile",
			DisplayName:      "Shipping profile",
			ResolverEndpoint: selfEndpoint,
		}},
		Contributions: []*doclinkv1.Contribution{{
			Id:               "shipping:item-detail-card",
			ExtensionPointId: "pim/item:detail.card",
			Namespace:        "shipping",
			Title:            "Shipping",
			Icon:             "📦",
			EmbedKind:        doclinkv1.EmbedKind_EMBED_KIND_IFRAME,
			EmbedUrl:         embedBase + "/embed/item-card.html",
			ServiceEndpoint:  selfEndpoint,
			Weight:           90,
		}},
		LinkTypes: []*doclinkv1.LinkTypeDecl{{
			Id:                    "shipping:item-ships-via-profile",
			AnchorType:            &doclinkv1.DocRef{Namespace: "pim", Type: "item"},
			SourceType:            &doclinkv1.DocRef{Namespace: "shipping", Type: "profile"},
			Predicate:             "ships_via",
			Cardinality:           doclinkv1.Cardinality_CARDINALITY_MANY_TO_ONE,
			ResolverEndpoint:      selfEndpoint,
			SubscribesToLifecycle: true,
		}},
	})

	if err := serve.Run(envOr("ADDR", ":8083"), mux, log); err != nil {
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
