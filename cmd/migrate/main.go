// Command migrate applies the schema.
//
// Split out of the services because migration 0007 creates login roles, which
// requires a superuser connection, and no service should ever hold one. In the
// cluster this runs as a Job before the Deployments roll.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/raveesh/doclink/internal/pg"
)

func main() {
	ctx := context.Background()
	dsn := pg.DSNFromEnv("MIGRATION_DSN",
		"postgres://postgres:postgres@localhost:5432/doclink?sslmode=disable")

	pool, err := pg.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := pg.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	fmt.Println("migrations applied")
	os.Exit(0)
}
