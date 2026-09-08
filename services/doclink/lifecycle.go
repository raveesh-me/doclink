package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"github.com/raveesh/doclink/internal/docref"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dispatcher owns lifecycle fan-out.
//
// The anchor publishes one event and learns nothing about who received it. The
// registry is the only component that knows the subscriber set, and it knows it
// as data (rows in link_types) rather than as configuration.
type dispatcher struct {
	store    *store
	resolver *resolver
	log      *slog.Logger
	client   *http.Client
	interval time.Duration
	maxTries int
}

func newDispatcher(st *store, res *resolver, log *slog.Logger) *dispatcher {
	return &dispatcher{
		store:    st,
		resolver: res,
		log:      log.With("component", "lifecycle"),
		client:   &http.Client{Timeout: 10 * time.Second},
		interval: 500 * time.Millisecond,
		maxTries: 10,
	}
}

// Publish records one delivery row per interested satellite, inside a single
// transaction, and returns immediately. Actual delivery is the worker's job, so
// a slow or dead satellite cannot make DeleteItem slow in PIM.
func (d *dispatcher) Publish(ctx context.Context, ev *doclinkv1.LifecycleEvent) (int, error) {
	anchorKey := docref.TypeKey(ev.Subject)
	plan, err := d.store.linkTypesForAnchor(ctx, anchorKey)
	if err != nil {
		return 0, err
	}

	tx, err := d.store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	queued := 0
	for _, lt := range plan {
		if !lt.lifecycle {
			continue
		}
		// ON CONFLICT DO NOTHING: PIM's relay redelivers whenever it fails to
		// mark an event published, and a redelivery must not duplicate work.
		_, err := tx.Exec(ctx,
			`INSERT INTO doclink.deliveries
			   (event_id, namespace, endpoint, subject_ref, kind, sequence)
			 VALUES ($1,$2,$3,$4,$5,$6)
			 ON CONFLICT (event_id, namespace) DO NOTHING`,
			ev.EventId, lt.namespace, lt.endpoint, docref.String(ev.Subject),
			kindName(ev.Kind), ev.Sequence)
		if err != nil {
			return 0, err
		}
		queued++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	// The registry's own index is its cache, so it cleans up synchronously
	// rather than waiting for the delivery worker. Satellites' real rows are
	// their own problem and are handled asynchronously below.
	if ev.Kind == doclinkv1.LifecycleKind_LIFECYCLE_KIND_DELETED {
		if err := d.store.deleteIndexForAnchor(ctx, docref.String(ev.Subject)); err != nil {
			d.log.Warn("failed to purge link index", "subject", docref.String(ev.Subject), "err", err)
		}
	}

	return queued, nil
}

func (d *dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.deliverBatch(ctx); err != nil {
				d.log.Warn("delivery batch failed", "err", err)
			}
		}
	}
}

type deliveryRow struct {
	eventID    string
	namespace  string
	endpoint   string
	subjectRef string
	kind       string
	sequence   int64
	attempts   int
}

func (d *dispatcher) deliverBatch(ctx context.Context) error {
	// Claimed the same way as PIM's outbox, and for the same reason: the
	// publish is a network call and must not happen while holding row locks.
	rows, err := d.store.pool.Query(ctx,
		`UPDATE doclink.deliveries SET claimed_at = now()
		 WHERE (event_id, namespace) IN (
		   SELECT event_id, namespace FROM doclink.deliveries
		   WHERE delivered_at IS NULL AND attempts < $1
		     AND (claimed_at IS NULL OR claimed_at < now() - interval '30 seconds')
		   ORDER BY created_at
		   LIMIT 64
		   FOR UPDATE SKIP LOCKED
		 )
		 RETURNING event_id, namespace, endpoint, subject_ref, kind, sequence, attempts`,
		d.maxTries)
	if err != nil {
		return err
	}
	var batch []deliveryRow
	for rows.Next() {
		var r deliveryRow
		if err := rows.Scan(&r.eventID, &r.namespace, &r.endpoint,
			&r.subjectRef, &r.kind, &r.sequence, &r.attempts); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range batch {
		subject, err := docref.Parse(r.subjectRef)
		if err != nil {
			d.markDelivered(ctx, r, "malformed subject ref")
			continue
		}
		resp, err := d.resolver.linkClient(r.endpoint).HandleLifecycle(ctx, connect.NewRequest(
			&doclinkv1.HandleLifecycleRequest{
				Event: &doclinkv1.LifecycleEvent{
					EventId:    r.eventID,
					Subject:    subject,
					Kind:       kindValue(r.kind),
					Sequence:   r.sequence,
					OccurredAt: timestamppb.Now(),
				},
			}))
		if err != nil {
			_, _ = d.store.pool.Exec(ctx,
				`UPDATE doclink.deliveries
				 SET attempts = attempts + 1, last_error = $3
				 WHERE event_id = $1 AND namespace = $2`,
				r.eventID, r.namespace, err.Error())
			if r.attempts+1 >= d.maxTries {
				// Giving up loudly. In production this is a page: a satellite
				// is now holding rows that point at a document that is gone,
				// and nothing else will ever tell it.
				d.log.Error("giving up on lifecycle delivery; satellite has orphans",
					"namespace", r.namespace, "subject", r.subjectRef,
					"event_id", r.eventID, "attempts", r.attempts+1)
			}
			continue
		}
		d.markDelivered(ctx, r, "")
		d.log.Info("lifecycle delivered",
			"namespace", r.namespace, "kind", r.kind,
			"subject", r.subjectRef, "rows_reconciled", resp.Msg.RowsAffected)
	}
	return nil
}

func (d *dispatcher) markDelivered(ctx context.Context, r deliveryRow, note string) {
	_, _ = d.store.pool.Exec(ctx,
		`UPDATE doclink.deliveries
		 SET delivered_at = now(), last_error = $3
		 WHERE event_id = $1 AND namespace = $2`, r.eventID, r.namespace, note)
}

func kindName(k doclinkv1.LifecycleKind) string {
	switch k {
	case doclinkv1.LifecycleKind_LIFECYCLE_KIND_CREATED:
		return "CREATED"
	case doclinkv1.LifecycleKind_LIFECYCLE_KIND_UPDATED:
		return "UPDATED"
	case doclinkv1.LifecycleKind_LIFECYCLE_KIND_DELETED:
		return "DELETED"
	default:
		return "UNSPECIFIED"
	}
}

func kindValue(s string) doclinkv1.LifecycleKind {
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
