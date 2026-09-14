package pim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	pimv1 "github.com/raveesh-me/doclink/gen/pim/v1"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("duplicate")
)

// Store holds ProductCore and nothing else. There is no fragments table: module
// data is never a record here.
type Store struct {
	db *sql.DB
}

const productsDDL = `
CREATE TABLE IF NOT EXISTS products (
	tenant_id   TEXT    NOT NULL,
	id          TEXT    NOT NULL,
	sku         TEXT    NOT NULL,
	title       TEXT    NOT NULL,
	price_minor INTEGER NOT NULL,
	currency    TEXT    NOT NULL,
	created_at  TEXT    NOT NULL,
	updated_at  TEXT    NOT NULL,
	PRIMARY KEY (tenant_id, id),
	UNIQUE (tenant_id, sku)
)`

func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if _, err := db.ExecContext(ctx, productsDDL); err != nil {
		return nil, fmt.Errorf("migrate products: %w", err)
	}
	return &Store{db: db}, nil
}

const productCols = `id, tenant_id, sku, title, price_minor, currency`

func scanProduct(row interface{ Scan(...any) error }) (*pimv1.ProductCore, error) {
	p := &pimv1.ProductCore{}
	if err := row.Scan(&p.Id, &p.TenantId, &p.Sku, &p.Title, &p.PriceMinor, &p.Currency); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) Get(ctx context.Context, tenantID, id string) (*pimv1.ProductCore, error) {
	p, err := scanProduct(s.db.QueryRowContext(ctx,
		`SELECT `+productCols+` FROM products WHERE tenant_id = ? AND id = ?`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// List returns up to limit products with id > after, ordered by id.
func (s *Store) List(ctx context.Context, tenantID, after string, limit int) ([]*pimv1.ProductCore, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+productCols+` FROM products WHERE tenant_id = ? AND id > ? ORDER BY id LIMIT ?`,
		tenantID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pimv1.ProductCore
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Upsert(ctx context.Context, p *pimv1.ProductCore) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO products (id, tenant_id, sku, title, price_minor, currency, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			sku = excluded.sku, title = excluded.title, price_minor = excluded.price_minor,
			currency = excluded.currency, updated_at = excluded.updated_at`,
		p.Id, p.TenantId, p.Sku, p.Title, p.PriceMinor, p.Currency, now, now)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: products.tenant_id, products.sku") {
		return fmt.Errorf("%w: sku %q already belongs to another product", ErrDuplicate, p.Sku)
	}
	return err
}

// SeedIfEmpty loads two tenants and a handful of products into an empty table.
func (s *Store) SeedIfEmpty(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM products`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	for _, p := range seed {
		if err := s.Upsert(ctx, p); err != nil {
			return false, fmt.Errorf("seed %s/%s: %w", p.TenantId, p.Id, err)
		}
	}
	return true, nil
}

var seed = []*pimv1.ProductCore{
	{TenantId: "t1", Id: "p1", Sku: "SKU-1", Title: "Widget", PriceMinor: 49900, Currency: "INR"},
	{TenantId: "t1", Id: "p2", Sku: "SKU-2", Title: "Field Guide to Birds", PriceMinor: 89900, Currency: "INR"},
	{TenantId: "t1", Id: "p3", Sku: "SKU-3", Title: "Cotton T-shirt", PriceMinor: 79900, Currency: "INR"},
	{TenantId: "t1", Id: "p4", Sku: "SKU-4", Title: "Silver Bangle", PriceMinor: 1249900, Currency: "INR"},
	{TenantId: "t1", Id: "p5", Sku: "SKU-5", Title: "Team Plan", PriceMinor: 199900, Currency: "INR"},
	{TenantId: "t2", Id: "p1", Sku: "SKU-1", Title: "Widget", PriceMinor: 52900, Currency: "INR"},
	{TenantId: "t2", Id: "p2", Sku: "SKU-3", Title: "Cotton T-shirt", PriceMinor: 69900, Currency: "INR"},
	{TenantId: "t2", Id: "p3", Sku: "SKU-6", Title: "Desk Lamp", PriceMinor: 249900, Currency: "INR"},
}
