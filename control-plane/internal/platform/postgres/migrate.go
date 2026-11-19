package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/systera/control-plane/migrations"
)

// migrationLockKey serializes concurrent migrators (e.g. several API replicas booting).
const migrationLockKey int64 = 0x5359535445524100 // "SYSTERA\0"

// Migration is one versioned schema change.
type Migration struct {
	Version int64
	Name    string
	Up      string
	Down    string
}

// LoadMigrations reads the embedded migration files in version order.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, err
	}
	byVersion := map[int64]*Migration{}
	for _, e := range entries {
		name := e.Name()
		var direction string
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			direction = "up"
		case strings.HasSuffix(name, ".down.sql"):
			direction = "down"
		default:
			continue
		}
		prefix, rest, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: expected NNNNNN_name.{up,down}.sql", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", name, err)
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, err
		}
		m := byVersion[version]
		if m == nil {
			m = &Migration{Version: version, Name: strings.TrimSuffix(strings.TrimSuffix(rest, ".up.sql"), ".down.sql")}
			byVersion[version] = m
		}
		if direction == "up" {
			m.Up = string(body)
		} else {
			m.Down = string(body)
		}
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" {
			return nil, fmt.Errorf("migration %d has no up file", m.Version)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// LatestVersion returns the highest embedded migration version.
func LatestVersion() (int64, error) {
	ms, err := LoadMigrations()
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 {
		return 0, nil
	}
	return ms[len(ms)-1].Version, nil
}

// CurrentVersion returns the highest applied migration version (0 if none).
func CurrentVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v int64
	err := pool.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return 0, nil
	}
	return v, err
}

// MigrateUp applies all pending migrations. Each migration and its bookkeeping row commit
// atomically. It returns the versions applied.
func MigrateUp(ctx context.Context, pool *pgxpool.Pool) ([]int64, error) {
	ms, err := LoadMigrations()
	if err != nil {
		return nil, err
	}
	var applied []int64
	err = withMigrationLock(ctx, pool, func(conn *pgxpool.Conn) error {
		done, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if done[m.Version] {
				continue
			}
			if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, m.Up); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name)
				return err
			}); err != nil {
				return fmt.Errorf("apply migration %06d_%s: %w", m.Version, m.Name, err)
			}
			applied = append(applied, m.Version)
		}
		return nil
	})
	return applied, err
}

// MigrateDown reverts the most recent `steps` applied migrations.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, steps int) ([]int64, error) {
	ms, err := LoadMigrations()
	if err != nil {
		return nil, err
	}
	byVersion := map[int64]Migration{}
	for _, m := range ms {
		byVersion[m.Version] = m
	}
	var reverted []int64
	err = withMigrationLock(ctx, pool, func(conn *pgxpool.Conn) error {
		rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT $1", steps)
		if err != nil {
			return err
		}
		versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, v := range versions {
			m, ok := byVersion[v]
			if !ok || m.Down == "" {
				return fmt.Errorf("migration %d has no down file", v)
			}
			if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, m.Down); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", v)
				return err
			}); err != nil {
				return fmt.Errorf("revert migration %06d_%s: %w", m.Version, m.Name, err)
			}
			reverted = append(reverted, v)
		}
		return nil
	})
	return reverted, err
}

func withMigrationLock(ctx context.Context, pool *pgxpool.Pool, fn func(*pgxpool.Conn) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey) }()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    BIGINT PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return fn(conn)
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int64]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(versions))
	for _, v := range versions {
		out[v] = true
	}
	return out, nil
}
