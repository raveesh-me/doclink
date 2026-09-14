// Package modkit is the small amount of plumbing the two example modules share.
// It lives under internal/modules, so PIM may not import it.
package modkit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// ToStruct is the module's boundary: a strongly typed, module-owned message goes
// in, the universal wire type comes out.
//
// The conversion is a protojson round-trip, so the Struct has exactly the shape
// proto3 JSON gives the message — lowerCamel field names, enums as names, int64
// as decimal strings — which is also the shape the module's JSON Schema must
// describe. EmitUnpopulated keeps zero values (a 0% rate, an empty IGST
// component) present rather than silently absent.
//
// PIM never sees the typed message. It receives, validates and serves a Struct.
func ToStruct(m proto.Message) (*structpb.Struct, error) {
	b, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(b, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Stats counts calls so the demo can assert that fan-out is batched.
type Stats struct {
	enrichCalls    atomic.Int64
	enrichProducts atomic.Int64
}

func (s *Stats) RecordEnrich(products int) {
	s.enrichCalls.Add(1)
	s.enrichProducts.Add(int64(products))
}

type StatsSnapshot struct {
	EnrichCalls    int64 `json:"enrich_calls"`
	EnrichProducts int64 `json:"enrich_products"`
}

func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{EnrichCalls: s.enrichCalls.Load(), EnrichProducts: s.enrichProducts.Load()}
}

// Mount adds GET /healthz, GET /stats and POST /stats/reset.
func (s *Stats) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, s.Snapshot())
	})
	mux.HandleFunc("POST /stats/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.enrichCalls.Store(0)
		s.enrichProducts.Store(0)
		WriteJSON(w, s.Snapshot())
	})
}

func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Serve runs h on addr until SIGINT or SIGTERM.
func Serve(addr string, h http.Handler, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", addr)

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
