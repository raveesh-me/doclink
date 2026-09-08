// Command webhost serves the PIM shell's static build and attaches a
// Content-Security-Policy whose frame-src is derived from the registry.
//
// It exists because a CSP has to be an HTTP response header. A
// <meta http-equiv> tag cannot work here: the list of allowed origins is only
// known after asking the registry, and a CSP meta tag inserted after the
// document is parsed is ignored by browsers. So something dynamic has to sit in
// front of the static files, and this is the smallest thing that can.
//
// It is deliberately not a proxy and holds no application logic. It serves a
// directory and computes one header.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/raveesh-me/doclink/internal/serve"
)

func main() {
	log := serve.Logger("webhost")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := envOr("STATIC_DIR", "./dist")
	registryURL := envOr("DOCLINK_ENDPOINT", "http://localhost:8080")
	slot := envOr("EXTENSION_POINT", "pim/item:detail.card")
	// PIM's own API origins. Known at deploy time because they are PIM's own
	// dependencies, unlike the satellites'.
	connectOrigins := strings.Split(envOr("CONNECT_ORIGINS", ""), ",")

	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		log.Error("no index.html in static dir", "dir", dir, "err", err)
		os.Exit(1)
	}

	allowedEmbeds := strings.Split(envOr("ALLOWED_EMBED_ORIGINS", ""), ",")
	pol := newPolicy(registryURL, slot, connectOrigins, allowedEmbeds, log)

	// One synchronous attempt so the very first response already carries the
	// right origins. Failure is not fatal: the refresher will keep trying, and
	// until it succeeds no cards are frameable — which is the same state the
	// shell would be in anyway, since it discovers cards from this same
	// registry.
	warmCtx, warmCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := pol.Refresh(warmCtx); err != nil {
		log.Warn("initial frame-src load failed; cards blocked until the registry answers", "err", err)
	}
	warmCancel()

	go pol.Run(ctx, 30*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/", withPolicy(pol, spaHandler(dir)))
	mux.HandleFunc("/csp-status", func(w http.ResponseWriter, _ *http.Request) {
		origins, age, loaded := pol.Status()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "loaded: %t\nage: %s\nframe-src: %s\n",
			loaded, age.Round(time.Second), strings.Join(origins, " "))
	})

	if err := serve.Run(envOr("ADDR", ":8080"), mux, log); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}

func withPolicy(pol *policy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", pol.Header())
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// spaHandler serves files, falling back to index.html so /items/<id> survives a
// refresh. Requests under /assets/ are never rewritten: a miss there is a real
// 404, and answering it with HTML would turn a missing bundle into a confusing
// MIME error in the console.
func spaHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean(r.URL.Path)
		if strings.HasPrefix(clean, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}
		if clean == "/config.js" {
			// Mounted from a ConfigMap; a cached copy would survive a cluster
			// reconfiguration and point the app at the wrong backends.
			w.Header().Set("Cache-Control", "no-store")
			files.ServeHTTP(w, r)
			return
		}

		if _, err := os.Stat(filepath.Join(dir, clean)); err == nil && clean != "/" {
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
