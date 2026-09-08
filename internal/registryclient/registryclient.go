// Package registryclient is the self-registration helper every service uses at
// startup.
//
// Registration is announced by the service itself, at boot, over the network.
// Nothing is declared in a shared config file and nothing is compiled into the
// host: that is what lets a satellite appear in the item screen without the PIM
// team deploying anything.
package registryclient

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"github.com/raveesh/doclink/gen/doclink/v1/doclinkv1connect"
)

type Client struct {
	rpc doclinkv1connect.RegistryServiceClient
	log *slog.Logger
}

func New(endpoint string, log *slog.Logger) *Client {
	return &Client{
		rpc: doclinkv1connect.NewRegistryServiceClient(
			&http.Client{Timeout: 10 * time.Second}, endpoint),
		log: log,
	}
}

// Registration is everything one service announces about itself. All fields are
// optional; a service that only anchors documents fills DocumentTypes and
// ExtensionPoints, a satellite fills Contributions and LinkTypes.
type Registration struct {
	DocumentTypes   []*doclinkv1.DocumentType
	ExtensionPoints []*doclinkv1.ExtensionPoint
	Contributions   []*doclinkv1.Contribution
	LinkTypes       []*doclinkv1.LinkTypeDecl
}

// Announce registers everything, retrying until the registry is reachable.
//
// Services must start successfully even when the registry is down; a satellite
// that cannot register is invisible in the UI, which is degraded but not broken.
// So this runs in the background and never blocks serving.
func (c *Client) Announce(ctx context.Context, reg Registration) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := c.announceOnce(ctx, reg)
		if err == nil {
			c.log.Info("registered with doclink",
				"document_types", len(reg.DocumentTypes),
				"extension_points", len(reg.ExtensionPoints),
				"contributions", len(reg.Contributions),
				"link_types", len(reg.LinkTypes))
			return
		}
		c.log.Warn("registration failed, retrying", "attempt", attempt, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) announceOnce(ctx context.Context, reg Registration) error {
	for _, dt := range reg.DocumentTypes {
		_, err := c.rpc.RegisterDocumentType(ctx, connect.NewRequest(
			&doclinkv1.RegisterDocumentTypeRequest{DocumentType: dt}))
		if err != nil {
			return fmt.Errorf("document type %s/%s: %w", dt.Namespace, dt.Type, err)
		}
	}
	for _, ep := range reg.ExtensionPoints {
		_, err := c.rpc.RegisterExtensionPoint(ctx, connect.NewRequest(
			&doclinkv1.RegisterExtensionPointRequest{ExtensionPoint: ep}))
		if err != nil {
			return fmt.Errorf("extension point %s: %w", ep.Id, err)
		}
	}
	for _, con := range reg.Contributions {
		_, err := c.rpc.RegisterContribution(ctx, connect.NewRequest(
			&doclinkv1.RegisterContributionRequest{Contribution: con}))
		if err != nil {
			return fmt.Errorf("contribution %s: %w", con.Id, err)
		}
	}
	for _, lt := range reg.LinkTypes {
		_, err := c.rpc.RegisterLinkType(ctx, connect.NewRequest(
			&doclinkv1.RegisterLinkTypeRequest{LinkType: lt}))
		if err != nil {
			return fmt.Errorf("link type %s: %w", lt.Id, err)
		}
	}
	return nil
}

// IndexEntry mirrors a satellite's linkage write into the registry's link_index,
// which backs the INDEXED strategy.
//
// Called after the satellite's own transaction commits, deliberately outside it:
// the index is a cache, and a failure here must not roll back a write the
// satellite considers durable. The cost of that choice is a staleness window,
// which the benchmark measures rather than hides.
func (c *Client) IndexEntry(ctx context.Context, anchor, source *doclinkv1.DocRef,
	predicate string, card doclinkv1.Cardinality, deleted bool) {
	_, err := c.rpc.UpsertIndexEntry(ctx, connect.NewRequest(
		&doclinkv1.UpsertIndexEntryRequest{
			Anchor:      anchor,
			Source:      source,
			Predicate:   predicate,
			Cardinality: card,
			Deleted:     deleted,
		}))
	if err != nil {
		c.log.Warn("link index update failed; INDEXED reads will be stale",
			"anchor", anchor.String(), "err", err)
	}
}
