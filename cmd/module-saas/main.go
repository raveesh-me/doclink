// Command module-saas serves the saas module on :8082.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	"github.com/raveesh-me/doclink/internal/modules/modkit"
	"github.com/raveesh-me/doclink/internal/modules/saas"
)

func main() {
	addr := flag.String("addr", ":8082", "listen address")
	ttl := flag.Int("cache-ttl", 60, "cache_ttl_seconds advertised in Describe")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m := &saas.Module{CacheTTLSeconds: int32(*ttl), Log: log}

	mux := http.NewServeMux()
	mux.Handle(extv1connect.NewModuleExtensionHandler(m))
	m.Stats.Mount(mux)
	m.MountChaos(mux)

	if err := modkit.Serve(*addr, mux, log); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
