package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	shippingv1 "github.com/raveesh/doclink/gen/shipping/v1"
	"github.com/raveesh/doclink/internal/ids"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var errNoAssignment = errors.New("no shipping assignment for anchor")

type store struct{ pool *pgxpool.Pool }

type scanner interface{ Scan(dest ...any) error }

func scanProfile(row scanner) (*shippingv1.Profile, error) {
	var p shippingv1.Profile
	var created time.Time
	err := row.Scan(&p.Id, &p.Name, &p.Carrier, &p.HandlingDays, &p.RequiresSignature, &created)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = timestamppb.New(created)
	return &p, nil
}

const profileCols = `id, name, carrier, handling_days, requires_signature, created_at`

func (s *store) listProfiles(ctx context.Context) ([]*shippingv1.Profile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+profileCols+` FROM shipping.profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*shippingv1.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *store) profilesByID(ctx context.Context, idList []string) (map[string]*shippingv1.Profile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+profileCols+` FROM shipping.profiles WHERE id = ANY($1)`, idList)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]*shippingv1.Profile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out[p.Id] = p
	}
	return out, rows.Err()
}

// assignmentForAnchor is the many-to-one traversal: at most one row, enforced by
// a UNIQUE constraint on anchor_ref. The cardinality lives here, in the service
// that cares about it, rather than as a column shape in the anchor's table.
func (s *store) assignmentForAnchor(ctx context.Context, anchorRef string) (
	*shippingv1.Assignment, error) {

	var a shippingv1.Assignment
	var created time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, profile_id, anchor_ref, weight_grams, dimensions_cm, hazmat, created_at
		 FROM shipping.assignments WHERE anchor_ref = $1`, anchorRef).
		Scan(&a.Id, &a.ProfileId, &a.AnchorRef, &a.WeightGrams, &a.DimensionsCm, &a.Hazmat, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoAssignment
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = timestamppb.New(created)
	return &a, nil
}

func (s *store) assign(ctx context.Context, anchorRef, profileID string,
	weight int32, dims string, hazmat bool) (*shippingv1.Assignment, error) {

	a := &shippingv1.Assignment{
		Id: ids.New(), ProfileId: profileID, AnchorRef: anchorRef,
		WeightGrams: weight, DimensionsCm: dims, Hazmat: hazmat,
	}
	var created time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO shipping.assignments
		   (id, profile_id, anchor_ref, weight_grams, dimensions_cm, hazmat)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (anchor_ref) DO UPDATE
		   SET profile_id = EXCLUDED.profile_id,
		       weight_grams = EXCLUDED.weight_grams,
		       dimensions_cm = EXCLUDED.dimensions_cm,
		       hazmat = EXCLUDED.hazmat
		 RETURNING id, created_at`,
		a.Id, profileID, anchorRef, weight, dims, hazmat).Scan(&a.Id, &created)
	if err != nil {
		return nil, err
	}
	a.CreatedAt = timestamppb.New(created)
	return a, nil
}

func (s *store) unassign(ctx context.Context, anchorRef string) (string, error) {
	var profileID string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM shipping.assignments WHERE anchor_ref = $1 RETURNING profile_id`,
		anchorRef).Scan(&profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNoAssignment
	}
	return profileID, err
}

// purgeAnchor is shipping's own cascade, run in response to a lifecycle event.
func (s *store) purgeAnchor(ctx context.Context, anchorRef string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM shipping.assignments WHERE anchor_ref = $1`, anchorRef)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
