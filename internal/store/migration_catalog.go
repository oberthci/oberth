package store

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var (
	migrationIDPattern                   = regexp.MustCompile(`^[0-9]{14}-[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	migrations, migrationDefinitionError = loadMigrations(migrationFiles)
	latestMigrationVersion               = len(migrations)
)

func (m migration) ID() string {
	if m.id != "" {
		return m.id
	}
	return strconv.Itoa(m.version)
}

func validTimestampMigrationID(id string) bool {
	if len(id) > 96 || !migrationIDPattern.MatchString(id) {
		return false
	}
	_, err := time.Parse("20060102150405", id[:14])
	return err == nil
}

func loadMigrations(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, fmt.Errorf("%w: list migrations: %w", ErrSchemaIncompatible, err)
	}
	items := slices.Clone(legacyMigrations)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			return nil, fmt.Errorf("%w: unexpected migration file %q", ErrSchemaIncompatible, entry.Name())
		}
		id := strings.TrimSuffix(entry.Name(), ".sql")
		body, err := fs.ReadFile(files, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("%w: read migration %q: %w", ErrSchemaIncompatible, id, err)
		}
		items = append(items, migration{id: id, sql: string(body)})
	}
	return orderMigrations(items)
}

// Catalog positions are only a compatibility summary and a legacy fixture aid.
// Timestamped migrations are always matched to database receipts by their ID.
func orderMigrations(items []migration) ([]migration, error) {
	items = slices.Clone(items)
	slices.SortFunc(items, func(a, b migration) int {
		switch {
		case a.id == "" && b.id == "":
			return a.version - b.version
		case a.id == "":
			return -1
		case b.id == "":
			return 1
		default:
			return strings.Compare(a.id, b.id)
		}
	})
	for i := range items {
		if items[i].id != "" {
			items[i].version = i + 1
		}
	}
	return items, validateMigrations(items, len(items))
}

// Validate the binary independently of the database, even if fully upgraded.
func validateMigrations(items []migration, latest int) error {
	seen := make(map[string]bool, len(items))
	previousID := ""
	for index, item := range items {
		id := item.ID()
		if seen[id] {
			return fmt.Errorf("%w: duplicate migration ID %q in binary", ErrSchemaIncompatible, id)
		}
		seen[id] = true
		if item.id == "" {
			if item.version < 1 || item.version > lastIntegerMigrationVersion || item.version != index+1 {
				return fmt.Errorf("%w: invalid integer migration %d at position %d; integers are frozen at %d", ErrSchemaIncompatible, item.version, index, lastIntegerMigrationVersion)
			}
		} else {
			if index < lastIntegerMigrationVersion || !validTimestampMigrationID(id) || id <= previousID || item.version != index+1 {
				return fmt.Errorf("%w: invalid or unordered migration ID %q at position %d", ErrSchemaIncompatible, id, index)
			}
			previousID = id
		}
		methods := 0
		if strings.TrimSpace(item.sql) != "" {
			methods++
		}
		if item.apply != nil {
			methods++
		}
		if item.rawApply != nil {
			methods++
		}
		if methods != 1 || (item.id != "" && item.sql == "") {
			return fmt.Errorf("%w: migration %q must define exactly one application method; timestamped migrations require SQL", ErrSchemaIncompatible, id)
		}
	}
	if len(items) != latest || len(items) < lastIntegerMigrationVersion {
		return fmt.Errorf("%w: binary has %d migrations, expected %d (at least %d legacy)", ErrSchemaIncompatible, len(items), latest, lastIntegerMigrationVersion)
	}
	return nil
}
