package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	doclinkv1 "github.com/raveesh/doclink/gen/doclink/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type store struct{ pool *pgxpool.Pool }

// ---------------------------------------------------------------------------
// Declaration writes. All upserts: services re-announce on every boot, so
// registration must be idempotent or a rolling restart would fail.
// ---------------------------------------------------------------------------

func (s *store) putDocumentType(ctx context.Context, dt *doclinkv1.DocumentType) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doclink.document_types
		   (type_key, namespace, type, display_name, resolver_endpoint)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (type_key) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       resolver_endpoint = EXCLUDED.resolver_endpoint`,
		dt.Namespace+"/"+dt.Type, dt.Namespace, dt.Type, dt.DisplayName, dt.ResolverEndpoint)
	return err
}

func (s *store) putExtensionPoint(ctx context.Context, ep *doclinkv1.ExtensionPoint) error {
	anchorKey := ep.AnchorType.GetNamespace() + "/" + ep.AnchorType.GetType()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doclink.extension_points (id, anchor_type_key, display_name, description)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (id) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       description = EXCLUDED.description`,
		ep.Id, anchorKey, ep.DisplayName, ep.Description)
	return err
}

func (s *store) putContribution(ctx context.Context, c *doclinkv1.Contribution) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doclink.contributions
		   (id, extension_point_id, namespace, title, icon, embed_kind,
		    embed_url, service_endpoint, weight)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (extension_point_id, namespace) DO UPDATE
		   SET title = EXCLUDED.title,
		       icon = EXCLUDED.icon,
		       embed_kind = EXCLUDED.embed_kind,
		       embed_url = EXCLUDED.embed_url,
		       service_endpoint = EXCLUDED.service_endpoint,
		       weight = EXCLUDED.weight`,
		c.Id, c.ExtensionPointId, c.Namespace, c.Title, c.Icon,
		embedKindName(c.EmbedKind), c.EmbedUrl, c.ServiceEndpoint, c.Weight)
	return err
}

func (s *store) putLinkType(ctx context.Context, lt *doclinkv1.LinkTypeDecl) error {
	anchorKey := lt.AnchorType.GetNamespace() + "/" + lt.AnchorType.GetType()
	sourceKey := lt.SourceType.GetNamespace() + "/" + lt.SourceType.GetType()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doclink.link_types
		   (id, anchor_type_key, source_type_key, predicate, cardinality,
		    resolver_endpoint, subscribes_to_lifecycle)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (anchor_type_key, source_type_key, predicate) DO UPDATE
		   SET cardinality = EXCLUDED.cardinality,
		       resolver_endpoint = EXCLUDED.resolver_endpoint,
		       subscribes_to_lifecycle = EXCLUDED.subscribes_to_lifecycle`,
		lt.Id, anchorKey, sourceKey, lt.Predicate, cardinalityName(lt.Cardinality),
		lt.ResolverEndpoint, lt.SubscribesToLifecycle)
	return err
}

// ---------------------------------------------------------------------------
// Declaration reads
// ---------------------------------------------------------------------------

func (s *store) listContributions(ctx context.Context, slot string) ([]*doclinkv1.Contribution, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, extension_point_id, namespace, title, icon, embed_kind,
		        embed_url, service_endpoint, weight, registered_at
		 FROM doclink.contributions
		 WHERE ($1 = '' OR extension_point_id = $1)
		 ORDER BY weight DESC, title ASC`, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*doclinkv1.Contribution
	for rows.Next() {
		var c doclinkv1.Contribution
		var kind string
		var at time.Time
		if err := rows.Scan(&c.Id, &c.ExtensionPointId, &c.Namespace, &c.Title, &c.Icon,
			&kind, &c.EmbedUrl, &c.ServiceEndpoint, &c.Weight, &at); err != nil {
			return nil, err
		}
		c.EmbedKind = embedKindValue(kind)
		c.RegisteredAt = timestamppb.New(at)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// linkTypeRow is the internal view; the fan-out planner needs the endpoint,
// which the proto also carries but which is easier to work with unwrapped.
type linkTypeRow struct {
	id          string
	anchorKey   string
	sourceKey   string
	namespace   string
	predicate   string
	cardinality doclinkv1.Cardinality
	endpoint    string
	lifecycle   bool
}

// linkTypesForAnchor is the fan-out plan: every service that has declared an
// edge into this anchor type. This single query is what replaces a compiled-in
// list of satellites.
func (s *store) linkTypesForAnchor(ctx context.Context, anchorKey string) ([]linkTypeRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, anchor_type_key, source_type_key, predicate, cardinality,
		        resolver_endpoint, subscribes_to_lifecycle
		 FROM doclink.link_types
		 WHERE ($1 = '' OR anchor_type_key = $1)
		 ORDER BY source_type_key`, anchorKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []linkTypeRow
	for rows.Next() {
		var r linkTypeRow
		var card string
		if err := rows.Scan(&r.id, &r.anchorKey, &r.sourceKey, &r.predicate,
			&card, &r.endpoint, &r.lifecycle); err != nil {
			return nil, err
		}
		r.cardinality = cardinalityValue(card)
		r.namespace, _ = splitTypeKey(r.sourceKey)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) resolverEndpoints(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT type_key, resolver_endpoint FROM doclink.document_types`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Link index (INDEXED strategy only)
// ---------------------------------------------------------------------------

func (s *store) upsertIndex(ctx context.Context, anchorRef, sourceRef, predicate,
	cardinality, namespace string, deleted bool) (int64, error) {
	if deleted {
		_, err := s.pool.Exec(ctx,
			`DELETE FROM doclink.link_index
			 WHERE anchor_ref = $1 AND source_ref = $2 AND predicate = $3`,
			anchorRef, sourceRef, predicate)
		return 0, err
	}
	var seq int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO doclink.link_index
		   (anchor_ref, source_ref, predicate, cardinality, namespace)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (anchor_ref, source_ref, predicate) DO UPDATE
		   SET written_at = now(), cardinality = EXCLUDED.cardinality
		 RETURNING sequence`,
		anchorRef, sourceRef, predicate, cardinality, namespace).Scan(&seq)
	return seq, err
}

type indexRow struct {
	sourceRef   string
	predicate   string
	namespace   string
	cardinality doclinkv1.Cardinality
}

func (s *store) indexLookup(ctx context.Context, anchorRef string) ([]indexRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source_ref, predicate, namespace, cardinality
		 FROM doclink.link_index
		 WHERE anchor_ref = $1
		 ORDER BY namespace, source_ref`, anchorRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []indexRow
	for rows.Next() {
		var r indexRow
		var card string
		if err := rows.Scan(&r.sourceRef, &r.predicate, &r.namespace, &card); err != nil {
			return nil, err
		}
		r.cardinality = cardinalityValue(card)
		out = append(out, r)
	}
	return out, rows.Err()
}

// deleteIndexForAnchor drops every index entry for a deleted anchor. The
// registry can do this unilaterally because the index is its own cache; the
// satellites' real rows are cleaned up by their own lifecycle handlers.
func (s *store) deleteIndexForAnchor(ctx context.Context, anchorRef string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM doclink.link_index WHERE anchor_ref = $1`, anchorRef)
	return err
}

// ---------------------------------------------------------------------------
// Deregistration
// ---------------------------------------------------------------------------

func (s *store) deregister(ctx context.Context, linkTypeID, contributionID, docTypeKey string) (int, error) {
	removed := 0
	if linkTypeID != "" {
		tag, err := s.pool.Exec(ctx, `DELETE FROM doclink.link_types WHERE id = $1`, linkTypeID)
		if err != nil {
			return removed, err
		}
		removed += int(tag.RowsAffected())
	}
	if contributionID != "" {
		tag, err := s.pool.Exec(ctx, `DELETE FROM doclink.contributions WHERE id = $1`, contributionID)
		if err != nil {
			return removed, err
		}
		removed += int(tag.RowsAffected())
	}
	if docTypeKey != "" {
		tag, err := s.pool.Exec(ctx, `DELETE FROM doclink.document_types WHERE type_key = $1`, docTypeKey)
		if err != nil {
			return removed, err
		}
		removed += int(tag.RowsAffected())
	}
	return removed, nil
}
