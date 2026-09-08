// Package serve holds the HTTP wiring every service repeats: an h2c server so
// ConnectRPC can use HTTP/2 without TLS inside the cluster, permissive CORS for
// the browser-side clients, health endpoints for kubelet probes, and graceful
// shutdown.
package serve

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// CORS wraps a handler so browser clients on other origins can call it.
//
// The embeds are cross-origin by design, so this is load-bearing rather than
// incidental: a subscriptions card served from subs.doclink.localhost calls the
// subscriptions API on the same host, but the PIM shell also calls the registry
// across origins. Wide open is correct for a POC and wrong for production,
// where these would be an explicit allowlist read from the registry.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers",
			"Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, "+
				"Grpc-Timeout, X-Grpc-Web, X-User-Agent")
		h.Set("Access-Control-Expose-Headers",
			"Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Run starts the server and blocks until SIGINT/SIGTERM, then drains.
func Run(addr string, mux *http.ServeMux, log *slog.Logger) error {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           h2c.NewHandler(CORS(mux), &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		log.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// Logger returns the standard structured logger for a service.
func Logger(service string) *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("DEBUG") != "" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("service", service)
}
