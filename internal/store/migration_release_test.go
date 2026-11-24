package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func migrationCoverage(target schemaDefinition) error {
	if target.number < 6 || len(target.migrations) != target.number-6 {
		return fmt.Errorf("schema %d requires every migration from schema 6", target.number)
	}
	for number := 6; number < target.number; number++ {
		step, ok := target.migrations[number]
		if !ok || step.apply == nil || step.baseline == "" {
			return fmt.Errorf("schema %d requires a %d to %d step", target.number, number, number+1)
		}
	}
	return nil
}

// Invoked by the release guard, including when the schema number alone changes.
func TestMigrationReleaseCoverage(t *testing.T) {
	target := currentSchema()
	requireOK(t, migrationCoverage(target))
	// The schema-6 image also proves the framework while there are no live steps.
	for number := 6; number == 6 || number < target.number; number++ {
		path, fixture := migrationFixturePath(t, number)
		requireOK(t, fixture.verifySource(t.Context()))
		s, err := Open(t.Context(), path, testOptions())
		requireOK(t, err)
		assertMigrationRecords(t, s, fixture)
		requireOK(t, s.Close())
	}
}

func TestMigrationCoverageRefusesMissingStepsAndFixtures(t *testing.T) {
	target := testMigrationTarget()
	requireOK(t, migrationCoverage(target))
	delete(target.migrations, 7)
	if migrationCoverage(target) == nil {
		t.Fatal("schema increase has no step")
	}
	target.migrations[7] = schemaMigration{}
	if migrationCoverage(target) == nil {
		t.Fatal("empty step accepted")
	}
	directory := t.TempDir()
	if _, _, err := readMigrationFixture(directory, 6); err == nil {
		t.Fatal("missing fixture accepted")
	}
	metadata, err := os.ReadFile("testdata/migrations/schema6.json")
	requireOK(t, err)
	requireOK(t, os.WriteFile(filepath.Join(directory, "schema6.json"), metadata, 0600))
	if _, _, err := readMigrationFixture(directory, 6); err == nil {
		t.Fatal("metadata without previous-code database accepted")
	}
}
