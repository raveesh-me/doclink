package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"github.com/raveesh/doclink/gen/doclink/v1/doclinkv1connect"
	"github.com/raveesh/doclink/internal/docref"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// relay drains pim.outbox into the registry.
//
// Polling rather than LISTEN/NOTIFY: notifications are lost if no one is
// listening, so a relay restart would silently drop events, and the whole point
// of the outbox is that it cannot. Polling is slower and correct.
type relay struct {
	store    *store
	registry doclinkv1connect.RegistryServiceClient
	log      *slog.Logger
	interval time.Duration
	batch    int
}

func newRelay(st *store, registryEndpoint string, log *slog.Logger) *relay {
	return &relay{
		store: st,
		registry: doclinkv1connect.NewRegistryServiceClient(
			&http.Client{Timeout: 15 * time.Second}, registryEndpoint),
		log:      log.With("component", "outbox-relay"),
		interval: 500 * time.Millisecond,
		batch:    64,
	}
}

func (r *relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("relay stopped")
			return
		case <-ticker.C:
			if n, err := r.drainOnce(ctx); err != nil {
				r.log.Warn("drain failed", "err", err)
			} else if n > 0 {
				r.log.Debug("published events", "count", n)
			}
		}
	}
}

type pending struct {
	eventID    string
	subjectRef string
	kind       string
	sequence   int64
	occurredAt time.Time
}

func (r *relay) drainOnce(ctx context.Context) (int, error) {
	// Claim a batch atomically.
	//
	// The subselect's FOR UPDATE SKIP LOCKED only holds its locks for the
	// duration of this one statement, which is exactly long enough: the UPDATE
	// that wraps it stamps claimed_at under the same lock, so a second replica
	// polling concurrently skips these rows and picks up different ones. Doing
	// the SELECT and the publish inside one long transaction would be the
	// obvious alternative and is worse — it would hold row locks across network
	// I/O to the registry.
	rows, err := r.store.pool.Query(ctx,
		`UPDATE pim.outbox SET claimed_at = now()
		 WHERE event_id IN (
		   SELECT event_id FROM pim.outbox
		   WHERE published_at IS NULL
		     AND (claimed_at IS NULL OR claimed_at < now() - interval '30 seconds')
		   ORDER BY occurred_at
		   LIMIT $1
		   FOR UPDATE SKIP LOCKED
		 )
		 RETURNING event_id, subject_ref, kind, sequence, occurred_at`, r.batch)
	if err != nil {
		return 0, err
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.eventID, &p.subjectRef, &p.kind, &p.sequence, &p.occurredAt); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	published := 0
	for _, p := range batch {
		ref, err := docref.Parse(p.subjectRef)
		if err != nil {
			// Unparseable subject can never succeed; mark it done rather than
			// blocking the queue head forever.
			r.markFailedPermanently(ctx, p.eventID, err.Error())
			continue
		}
		_, err = r.registry.PublishLifecycle(ctx, connect.NewRequest(
			&doclinkv1.PublishLifecycleRequest{
				Event: &doclinkv1.LifecycleEvent{
					EventId:    p.eventID,
					Subject:    ref,
					Kind:       kindFromString(p.kind),
					OccurredAt: timestamppb.New(p.occurredAt),
					Sequence:   p.sequence,
				},
			}))
		if err != nil {
			r.recordAttempt(ctx, p.eventID, err.Error())
			continue
		}
		if _, err := r.store.pool.Exec(ctx,
			`UPDATE pim.outbox SET published_at = now() WHERE event_id = $1`, p.eventID); err != nil {
			// The registry has the event but we failed to record that. On the
			// next pass it is redelivered, which is exactly why satellite
			// lifecycle handlers are required to be idempotent.
			r.log.Warn("published but failed to mark", "event_id", p.eventID, "err", err)
		}
		published++
	}
	return published, nil
}

func (r *relay) recordAttempt(ctx context.Context, eventID, msg string) {
	_, _ = r.store.pool.Exec(ctx,
		`UPDATE pim.outbox SET attempts = attempts + 1, last_error = $2 WHERE event_id = $1`,
		eventID, msg)
}

func (r *relay) markFailedPermanently(ctx context.Context, eventID, msg string) {
	r.log.Error("dropping unpublishable event", "event_id", eventID, "err", msg)
	_, _ = r.store.pool.Exec(ctx,
		`UPDATE pim.outbox SET published_at = now(), last_error = $2 WHERE event_id = $1`,
		eventID, msg)
}

func kindFromString(s string) doclinkv1.LifecycleKind {
	switch s {
	case "CREATED":
		return doclinkv1.LifecycleKind_LIFECYCLE_KIND_CREATED
	case "UPDATED":
		return doclinkv1.LifecycleKind_LIFECYCLE_KIND_UPDATED
	case "DELETED":
		return doclinkv1.LifecycleKind_LIFECYCLE_KIND_DELETED
	default:
		return doclinkv1.LifecycleKind_LIFECYCLE_KIND_UNSPECIFIED
	}
}
