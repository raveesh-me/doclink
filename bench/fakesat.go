package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"github.com/raveesh/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh/doclink/internal/docref"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// fakeSatellite is a synthetic module that registers a link type against
// pim/item and answers traversals from memory.
//
// It exists to answer the question the two real satellites cannot: what does the
// federated read cost when a company has sixteen modules attached to its item,
// rather than two. Its storage is a map, so the measurement isolates fan-out
// cost from database cost.
type fakeSatellite struct {
	namespace string
	// docsPerAnchor is how many documents this satellite returns per item,
	// modelling a many-to-many edge of a given width.
	docsPerAnchor int
	// latency is injected before every reply, standing in for the network and
	// query time of a satellite that is not on this machine. Fan-out cost is
	// dominated by the slowest satellite, so measuring with zero latency would
	// flatter the federated strategy badly.
	latency time.Duration

	srv      *http.Server
	listener net.Listener
	endpoint string
}

func newFakeSatellite(index, docsPerAnchor int, latency time.Duration) (*fakeSatellite, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	fs := &fakeSatellite{
		namespace:     fmt.Sprintf("synth%02d", index),
		docsPerAnchor: docsPerAnchor,
		latency:       latency,
		listener:      ln,
		endpoint:      "http://" + ln.Addr().String(),
	}

	mux := http.NewServeMux()
	mux.Handle(doclinkv1connect.NewLinkResolverHandler(fs))
	mux.Handle(doclinkv1connect.NewDocumentResolverHandler(fs))
	fs.srv = &http.Server{
		Handler:           h2c.NewHandler(mux, &http2.Server{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = fs.srv.Serve(ln)
	}()
	return fs, nil
}

func (f *fakeSatellite) typeName() string { return "record" }

func (f *fakeSatellite) docFor(anchor *doclinkv1.DocRef, i int) *doclinkv1.DocSummary {
	// Deterministic ids derived from the anchor, so the link index and the live
	// resolver agree without the harness having to coordinate them.
	id := fmt.Sprintf("%s-%d", anchor.Id, i)
	return &doclinkv1.DocSummary{
		Ref:      docref.New(f.namespace, f.typeName(), id),
		Title:    fmt.Sprintf("%s record %d", f.namespace, i),
		Subtitle: "synthetic",
		Href:     "/records/" + id,
		Badges:   map[string]string{"synthetic": "true"},
	}
}

func (f *fakeSatellite) ResolveLinks(_ context.Context,
	req *connect.Request[doclinkv1.ResolveLinksRequest],
) (*connect.Response[doclinkv1.ResolveLinksResponse], error) {
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	out := &doclinkv1.ResolveLinksResponse{
		Predicate:   "annotates",
		Cardinality: doclinkv1.Cardinality_CARDINALITY_MANY_TO_MANY,
	}
	for i := 0; i < f.docsPerAnchor; i++ {
		d := f.docFor(req.Msg.Anchor, i)
		if req.Msg.RefsOnly {
			d = &doclinkv1.DocSummary{Ref: d.Ref}
		}
		out.Documents = append(out.Documents, d)
	}
	return connect.NewResponse(out), nil
}

func (f *fakeSatellite) HandleLifecycle(_ context.Context,
	_ *connect.Request[doclinkv1.HandleLifecycleRequest],
) (*connect.Response[doclinkv1.HandleLifecycleResponse], error) {
	return connect.NewResponse(&doclinkv1.HandleLifecycleResponse{}), nil
}

// Describe is what makes the INDEXED strategy measurable at scale: the index
// stores bare refs, so hydration calls back here, once per namespace.
func (f *fakeSatellite) Describe(_ context.Context,
	req *connect.Request[doclinkv1.DescribeRequest],
) (*connect.Response[doclinkv1.DescribeResponse], error) {
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	out := &doclinkv1.DescribeResponse{}
	for _, ref := range req.Msg.Refs {
		out.Summaries = append(out.Summaries, &doclinkv1.DocSummary{
			Ref:      ref,
			Title:    f.namespace + " " + ref.Id,
			Subtitle: "synthetic",
		})
	}
	return connect.NewResponse(out), nil
}

func (f *fakeSatellite) linkTypeID() string { return "synthetic:" + f.namespace }

func (f *fakeSatellite) declaration() *doclinkv1.LinkTypeDecl {
	return &doclinkv1.LinkTypeDecl{
		Id:         f.linkTypeID(),
		AnchorType: &doclinkv1.DocRef{Namespace: "pim", Type: "item"},
		SourceType: &doclinkv1.DocRef{Namespace: f.namespace, Type: f.typeName()},
		Predicate:  "annotates",
		// Synthetic satellites do not subscribe to lifecycle events. They hold
		// no durable rows, so there would be nothing to reconcile, and enrolling
		// them would distort the delete-path measurement.
		Cardinality:           doclinkv1.Cardinality_CARDINALITY_MANY_TO_MANY,
		ResolverEndpoint:      f.endpoint,
		SubscribesToLifecycle: false,
	}
}

func (f *fakeSatellite) documentType() *doclinkv1.DocumentType {
	return &doclinkv1.DocumentType{
		Namespace:        f.namespace,
		Type:             f.typeName(),
		DisplayName:      f.namespace + " record",
		ResolverEndpoint: f.endpoint,
	}
}

func (f *fakeSatellite) stop(log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.srv.Shutdown(ctx); err != nil {
		log.Warn("fake satellite shutdown", "namespace", f.namespace, "err", err)
	}
}
