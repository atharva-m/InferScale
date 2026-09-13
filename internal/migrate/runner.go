// Package migrate applies the embedded, forward-only PostgreSQL schema used by
// InferScale. Production deployments run this command as a pre-deploy Job.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"

	"github.com/inferscale/inferscale/internal/storage/postgres/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	gooselock "github.com/pressly/goose/v3/lock"
)

// migrationLockID is the signed 64-bit ASCII prefix "InferSca". A dedicated
// ID avoids colliding with unrelated Goose users that share the database.
const migrationLockID int64 = 0x496e666572536361

func Run(ctx context.Context, databaseURL, command string, output io.Writer) error {
	if databaseURL == "" {
		return errors.New("INFERSCALE_DATABASE_URL is required")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}
	locker, err := gooselock.NewPostgresSessionLocker(gooselock.WithLockID(migrationLockID))
	if err != nil {
		return fmt.Errorf("configure PostgreSQL migration advisory lock: %w", err)
	}
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		database,
		migrations.FS,
		goose.WithSessionLocker(locker),
		goose.WithLogger(log.New(output, "", log.LstdFlags)),
	)
	if err != nil {
		return fmt.Errorf("initialize migrations: %w", err)
	}
	switch command {
	case "up":
		results, err := provider.Up(ctx)
		if err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		for _, result := range results {
			fmt.Fprintln(output, result.String())
		}
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return fmt.Errorf("migration status: %w", err)
		}
		for _, status := range statuses {
			appliedAt := "-"
			if !status.AppliedAt.IsZero() {
				appliedAt = status.AppliedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			fmt.Fprintf(output, "%06d %-7s %s %s\n", status.Source.Version, status.State, filepath.Base(status.Source.Path), appliedAt)
		}
	default:
		return fmt.Errorf("unsupported migration command %q (use up or status)", command)
	}
	return nil
}
