package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	subscriptionsv1 "github.com/raveesh/doclink/gen/subscriptions/v1"
	"github.com/raveesh/doclink/internal/ids"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type store struct{ pool *pgxpool.Pool }

func (s *store) listPlans(ctx context.Context) ([]*subscriptionsv1.Plan, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, interval, discount_bps, min_cycles, created_at
		 FROM subscriptions.plans ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*subscriptionsv1.Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *store) plansByID(ctx context.Context, idList []string) (map[string]*subscriptionsv1.Plan, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, interval, discount_bps, min_cycles, created_at
		 FROM subscriptions.plans WHERE id = ANY($1)`, idList)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]*subscriptionsv1.Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out[p.Id] = p
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPlan(row scanner) (*subscriptionsv1.Plan, error) {
	var p subscriptionsv1.Plan
	var created time.Time
	if err := row.Scan(&p.Id, &p.Name, &p.Interval, &p.DiscountBps, &p.MinCycles, &created); err != nil {
		return nil, err
	}
	p.CreatedAt = timestamppb.New(created)
	return &p, nil
}

// coverageForAnchor is the traversal that the whole architecture rests on.
//
// It is a single indexed lookup on a TEXT column holding a DocRef string. There
// is no join to the item, because the item is in another database. Everything
// this service knows about the item is what is in that string.
func (s *store) coverageForAnchor(ctx context.Context, anchorRef string) ([]*subscriptionsv1.Coverage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, plan_id, anchor_ref, max_quantity_per_cycle, prepaid_only, created_at
		 FROM subscriptions.coverage
		 WHERE anchor_ref = $1
		 ORDER BY created_at`, anchorRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*subscriptionsv1.Coverage
	for rows.Next() {
		var c subscriptionsv1.Coverage
		var created time.Time
		if err := rows.Scan(&c.Id, &c.PlanId, &c.AnchorRef,
			&c.MaxQuantityPerCycle, &c.PrepaidOnly, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = timestamppb.New(created)
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *store) attach(ctx context.Context, anchorRef, planID string,
	maxQty int32, prepaidOnly bool) (*subscriptionsv1.Coverage, error) {

	c := &subscriptionsv1.Coverage{
		Id:                  ids.New(),
		PlanId:              planID,
		AnchorRef:           anchorRef,
		MaxQuantityPerCycle: maxQty,
		PrepaidOnly:         prepaidOnly,
	}
	var created time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO subscriptions.coverage
		   (id, plan_id, anchor_ref, max_quantity_per_cycle, prepaid_only)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (plan_id, anchor_ref) DO UPDATE
		   SET max_quantity_per_cycle = EXCLUDED.max_quantity_per_cycle,
		       prepaid_only = EXCLUDED.prepaid_only
		 RETURNING id, created_at`,
		c.Id, planID, anchorRef, maxQty, prepaidOnly).Scan(&c.Id, &created)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = timestamppb.New(created)
	return c, nil
}

// detach returns the removed row so the caller can mirror the deletion into the
// registry's link index.
func (s *store) detach(ctx context.Context, coverageID string) (*subscriptionsv1.Coverage, error) {
	var c subscriptionsv1.Coverage
	var created time.Time
	err := s.pool.QueryRow(ctx,
		`DELETE FROM subscriptions.coverage WHERE id = $1
		 RETURNING id, plan_id, anchor_ref, max_quantity_per_cycle, prepaid_only, created_at`,
		coverageID).Scan(&c.Id, &c.PlanId, &c.AnchorRef,
		&c.MaxQuantityPerCycle, &c.PrepaidOnly, &created)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = timestamppb.New(created)
	return &c, nil
}

// purgeAnchor drops every coverage row pointing at a deleted item. This is the
// hand-written replacement for ON DELETE CASCADE, and it runs in this service,
// on this service's schedule, against this service's table. PIM neither knows
// nor waits for it.
func (s *store) purgeAnchor(ctx context.Context, anchorRef string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM subscriptions.coverage WHERE anchor_ref = $1`, anchorRef)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
