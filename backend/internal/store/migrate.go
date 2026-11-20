package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migration is one forward-only schema revision embedded at build time.
type migration struct {
	Version int
	Name    string
	SQL     string
}

// loadMigrations reads migrations/<version>_<name>.sql from fsys, sorted
// ascending by version. Filenames must start with a positive integer (e.g.
// 0001_initial); duplicate versions are rejected.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		prefix, name, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: expected <version>_<name>.sql", e.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q: invalid version prefix %q", e.Name(), prefix)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d (%s and %s)", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(fsys, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{Version: version, Name: name, SQL: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

// migrate creates the version table and applies every migration in fsys whose
// version is not yet recorded. Each migration runs in its own transaction, so a
// failure leaves the recorded version untouched and the database at the
// previous revision. Migrations are forward-only: an unapplied version below
// the highest recorded version is an error, never a gap-fill. It returns the
// highest applied version.
func migrate(db *sql.DB, fsys fs.FS) (int, error) {
	if _, err := db.Exec(schemaMigrationsDDL); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	migrations, err := loadMigrations(fsys)
	if err != nil {
		return 0, err
	}

	applied := make(map[int]bool, len(migrations))
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate applied migrations: %w", err)
	}

	// Forward-only: recorded versions are a lower bound. Applying an unapplied
	// migration whose version sits below the highest recorded version would
	// rewrite history out of order, so reject it instead of gap-filling.
	highest := 0
	for v := range applied {
		if v > highest {
			highest = v
		}
	}
	for _, m := range migrations {
		if !applied[m.Version] && m.Version < highest {
			return highest, fmt.Errorf(
				"migration %d (%s) is below already applied version %d; migrations are forward-only",
				m.Version, m.Name, highest,
			)
		}
	}

	latest := highest
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return latest, err
		}
		latest = m.Version
	}
	return latest, nil
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.Version, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.Exec(m.SQL); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.Version, m.Name,
	); err != nil {
		return fmt.Errorf("record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.Version, err)
	}
	return nil
}
