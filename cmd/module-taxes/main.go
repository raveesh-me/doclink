// Command module-taxes serves the taxes module on :8081.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	"github.com/raveesh-me/doclink/gen/registry/v1/registryv1connect"
	"github.com/raveesh-me/doclink/internal/modules/modkit"
	"github.com/raveesh-me/doclink/internal/modules/taxes"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	registry := flag.String("registry", "http://localhost:8080", "registry base URL, for InvalidateFragments")
	sleep := flag.Duration("sleep", 40*time.Millisecond, "deliberate EnrichProducts latency")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m := &taxes.Module{
		Sleep:    *sleep,
		Registry: registryv1connect.NewRegistryClient(http.DefaultClient, *registry),
		Log:      log,
	}

	mux := http.NewServeMux()
	mux.Handle(extv1connect.NewModuleExtensionHandler(m))
	m.Stats.Mount(mux)
	m.MountAdmin(mux)

	if err := modkit.Serve(*addr, mux, log); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
