// Package registry is the per-tenant module registry: which modules a tenant has
// installed, where they live, what their fragments look like, and the tenant's
// policy for each.
package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	registryv1 "github.com/raveesh-me/doclink/gen/registry/v1"
)

var ErrInstalled = errors.New("module already installed for tenant")

// Store persists one row per (tenant, module, extension point).
type Store struct {
	db *sql.DB
}

const registryDDL = `
CREATE TABLE IF NOT EXISTS registry_modules (
	tenant_id         TEXT    NOT NULL,
	module_id         TEXT    NOT NULL,
	version           TEXT    NOT NULL,
	base_url          TEXT    NOT NULL,
	extension_point   TEXT    NOT NULL,
	cache_ttl_seconds INTEGER NOT NULL,
	context_keys      TEXT    NOT NULL, -- JSON array
	schema_ref        TEXT    NOT NULL,
	json_schema       TEXT    NOT NULL,
	timeout_ms        INTEGER NOT NULL,
	failure_mode      TEXT    NOT NULL CHECK (failure_mode IN ('FAIL_OPEN', 'FAIL_CLOSED')),
	state             TEXT    NOT NULL CHECK (state IN ('ACTIVE', 'DRAINING', 'DEAD')),
	registered_at     TEXT    NOT NULL,
	last_verified_at  TEXT    NOT NULL,
	PRIMARY KEY (tenant_id, module_id, extension_point)
)`

func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if _, err := db.ExecContext(ctx, registryDDL); err != nil {
		return nil, fmt.Errorf("migrate registry_modules: %w", err)
	}
	return &Store{db: db}, nil
}

const rowCols = `tenant_id, module_id, version, base_url, extension_point, cache_ttl_seconds,
	context_keys, schema_ref, json_schema, timeout_ms, failure_mode, state, registered_at, last_verified_at`

// Insert persists every row of one registration, or none of them.
func (s *Store) Insert(ctx context.Context, rows []*registryv1.ModuleRegistration) error {
	if len(rows) == 0 {
		return errors.New("no rows")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM registry_modules WHERE tenant_id = ? AND module_id = ?`,
		rows[0].TenantId, rows[0].ModuleId).Scan(&exists)
	if err != nil {
		return err
	}
	if exists > 0 {
		return ErrInstalled
	}
	for _, r := range rows {
		keys, _ := json.Marshal(nonNil(r.ContextKeys))
		_, err := tx.ExecContext(ctx, `INSERT INTO registry_modules (`+rowCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.TenantId, r.ModuleId, r.Version, r.BaseUrl, r.ExtensionPoint, r.CacheTtlSeconds,
			string(keys), r.SchemaRef, r.JsonSchema, r.TimeoutMs, r.FailureMode.String(), r.State.String(),
			formatTime(r.RegisteredAt), formatTime(r.LastVerifiedAt))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// List returns every row for a tenant, in every state. An empty moduleID means
// all modules.
func (s *Store) List(ctx context.Context, tenantID, moduleID string) ([]*registryv1.ModuleRegistration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+rowCols+` FROM registry_modules
		WHERE tenant_id = ? AND (? = '' OR module_id = ?)
		ORDER BY module_id, extension_point`, tenantID, moduleID, moduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*registryv1.ModuleRegistration
	for rows.Next() {
		var (
			r                        registryv1.ModuleRegistration
			keys, mode, state        string
			registered, lastVerified string
		)
		err := rows.Scan(&r.TenantId, &r.ModuleId, &r.Version, &r.BaseUrl, &r.ExtensionPoint,
			&r.CacheTtlSeconds, &keys, &r.SchemaRef, &r.JsonSchema, &r.TimeoutMs, &mode, &state,
			&registered, &lastVerified)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(keys), &r.ContextKeys); err != nil {
			return nil, fmt.Errorf("context_keys: %w", err)
		}
		r.FailureMode = registryv1.FailureMode(registryv1.FailureMode_value[mode])
		r.State = registryv1.ModuleState(registryv1.ModuleState_value[state])
		r.RegisteredAt = parseTime(registered)
		r.LastVerifiedAt = parseTime(lastVerified)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// Module is a registered module as the composition engine sees it: one base
// URL, one tenant policy, several extension points.
type Module struct {
	TenantID    string
	ModuleID    string
	Version     string
	BaseURL     string
	Timeout     time.Duration
	FailureMode registryv1.FailureMode
	Points      []Point
}

type Point struct {
	Name        string
	SchemaRef   string
	JSONSchema  string
	CacheTTL    time.Duration
	ContextKeys []string
}

// ActiveModules returns the tenant's ACTIVE modules with their points in the
// given family: "product.enrich" matches product.enrich and
// product.enrich.contextual.
func (s *Store) ActiveModules(ctx context.Context, tenantID, family string) ([]Module, error) {
	rows, err := s.List(ctx, tenantID, "")
	if err != nil {
		return nil, err
	}
	var out []Module
	for _, r := range rows {
		if r.State != registryv1.ModuleState_ACTIVE || !inFamily(r.ExtensionPoint, family) {
			continue
		}
		if n := len(out); n == 0 || out[n-1].ModuleID != r.ModuleId {
			out = append(out, Module{
				TenantID:    r.TenantId,
				ModuleID:    r.ModuleId,
				Version:     r.Version,
				BaseURL:     r.BaseUrl,
				Timeout:     time.Duration(r.TimeoutMs) * time.Millisecond,
				FailureMode: r.FailureMode,
			})
		}
		m := &out[len(out)-1]
		m.Points = append(m.Points, Point{
			Name:        r.ExtensionPoint,
			SchemaRef:   r.SchemaRef,
			JSONSchema:  r.JsonSchema,
			CacheTTL:    time.Duration(r.CacheTtlSeconds) * time.Second,
			ContextKeys: r.ContextKeys,
		})
	}
	return out, nil
}

func inFamily(point, family string) bool {
	return point == family || (len(point) > len(family) && point[:len(family)+1] == family+".")
}

// SetState moves every row of a module to state, returning the rows changed.
func (s *Store) SetState(ctx context.Context, tenantID, moduleID string, state registryv1.ModuleState) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE registry_modules SET state = ? WHERE tenant_id = ? AND module_id = ? AND state != ?`,
		state.String(), tenantID, moduleID, state.String())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteDraining removes a module's rows, but only if they are still DRAINING.
func (s *Store) DeleteDraining(ctx context.Context, tenantID, moduleID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM registry_modules WHERE tenant_id = ? AND module_id = ? AND state = 'DRAINING'`,
		tenantID, moduleID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeDraining deletes every DRAINING row. Only safe at startup, when no read
// can be in flight.
func (s *Store) PurgeDraining(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM registry_modules WHERE state = 'DRAINING'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdatePolicy sets timeout and/or failure mode; zero values leave a field as is.
func (s *Store) UpdatePolicy(ctx context.Context, tenantID, moduleID string, timeoutMS int32, mode registryv1.FailureMode) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE registry_modules SET
			timeout_ms   = CASE WHEN ? > 0 THEN ? ELSE timeout_ms END,
			failure_mode = CASE WHEN ? != '' THEN ? ELSE failure_mode END
		WHERE tenant_id = ? AND module_id = ?`,
		timeoutMS, timeoutMS, modeText(mode), modeText(mode), tenantID, moduleID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func modeText(m registryv1.FailureMode) string {
	if m == registryv1.FailureMode_FAILURE_MODE_UNSPECIFIED {
		return ""
	}
	return m.String()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func formatTime(t *timestamppb.Timestamp) string {
	return t.AsTime().UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) *timestamppb.Timestamp {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return timestamppb.New(t)
}
