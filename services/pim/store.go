package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pimv1 "github.com/raveesh/doclink/gen/pim/v1"
	"github.com/raveesh/doclink/internal/ids"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var errNotFound = errors.New("item not found")

type store struct{ pool *pgxpool.Pool }

const itemCols = `id, sku, title, description, price_cents, currency, status, created_at, updated_at`

func scanItem(row pgx.Row) (*pimv1.Item, error) {
	var it pimv1.Item
	var created, updated time.Time
	err := row.Scan(&it.Id, &it.Sku, &it.Title, &it.Description,
		&it.PriceCents, &it.Currency, &it.Status, &created, &updated)
	if err != nil {
		return nil, err
	}
	it.CreatedAt = timestamppb.New(created)
	it.UpdatedAt = timestamppb.New(updated)
	return &it, nil
}

func (s *store) get(ctx context.Context, id string) (*pimv1.Item, error) {
	it, err := scanItem(s.pool.QueryRow(ctx,
		`SELECT `+itemCols+` FROM pim.items WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return it, err
}

// getMany backs the DocumentResolver. It is a single round trip on purpose:
// resolving N references must not become N queries, or the registry's hydration
// step would dominate every measurement in the benchmark.
func (s *store) getMany(ctx context.Context, idList []string) (map[string]*pimv1.Item, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+itemCols+` FROM pim.items WHERE id = ANY($1)`, idList)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]*pimv1.Item, len(idList))
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out[it.Id] = it
	}
	return out, rows.Err()
}

func (s *store) list(ctx context.Context, query string, limit int, offset int) ([]*pimv1.Item, int, error) {
	pattern := "%" + query + "%"
	rows, err := s.pool.Query(ctx,
		`SELECT `+itemCols+` FROM pim.items
		 WHERE ($1 = '' OR title ILIKE $2 OR sku ILIKE $2)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3 OFFSET $4`, query, pattern, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var items []*pimv1.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var total int
	err = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pim.items
		 WHERE ($1 = '' OR title ILIKE $2 OR sku ILIKE $2)`, query, pattern).Scan(&total)
	return items, total, err
}

// mutate performs an item write and its lifecycle event in one transaction.
//
// This is the whole reason the outbox exists. There is no foreign key from any
// satellite to pim.items, so nothing in the database will tell subscriptions
// that an item disappeared. The guarantee we can still make is that an item is
// never deleted without an event being durably queued, because both happen under
// the same commit.
func (s *store) mutate(ctx context.Context, subjectID, kind string,
	body func(pgx.Tx) error) (string, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := body(tx); err != nil {
		return "", err
	}

	subjectRef := "pim/item/" + subjectID

	// Per-subject monotonic sequence. A satellite that receives event 7 after
	// event 9 can discard it without asking anyone what the current state is.
	var seq int64
	err = tx.QueryRow(ctx,
		`INSERT INTO pim.sequences (subject_ref, value) VALUES ($1, 1)
		 ON CONFLICT (subject_ref) DO UPDATE SET value = pim.sequences.value + 1
		 RETURNING value`, subjectRef).Scan(&seq)
	if err != nil {
		return "", fmt.Errorf("bump sequence: %w", err)
	}

	eventID := ids.New()
	_, err = tx.Exec(ctx,
		`INSERT INTO pim.outbox (event_id, subject_ref, kind, sequence)
		 VALUES ($1, $2, $3, $4)`, eventID, subjectRef, kind, seq)
	if err != nil {
		return "", fmt.Errorf("write outbox: %w", err)
	}

	return eventID, tx.Commit(ctx)
}

func (s *store) create(ctx context.Context, it *pimv1.Item) (*pimv1.Item, error) {
	if it.Id == "" {
		it.Id = ids.New()
	}
	_, err := s.mutate(ctx, it.Id, "CREATED", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO pim.items (id, sku, title, description, price_cents, currency, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			it.Id, it.Sku, it.Title, it.Description, it.PriceCents,
			orDefault(it.Currency, "USD"), orDefault(it.Status, "active"))
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.get(ctx, it.Id)
}

func (s *store) update(ctx context.Context, it *pimv1.Item) (*pimv1.Item, error) {
	_, err := s.mutate(ctx, it.Id, "UPDATED", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE pim.items
			 SET sku=$2, title=$3, description=$4, price_cents=$5,
			     currency=$6, status=$7, updated_at=now()
			 WHERE id=$1`,
			it.Id, it.Sku, it.Title, it.Description, it.PriceCents,
			orDefault(it.Currency, "USD"), orDefault(it.Status, "active"))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.get(ctx, it.Id)
}

func (s *store) delete(ctx context.Context, id string) (string, error) {
	return s.mutate(ctx, id, "DELETED", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM pim.items WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound
		}
		return nil
	})
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
