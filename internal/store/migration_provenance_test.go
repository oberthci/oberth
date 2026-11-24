package store

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func (f migrationFixture) verifySource(ctx context.Context) error {
	body, err := exec.CommandContext(ctx, "git", "show", f.SourceCommit+":internal/store/schema.go").Output()
	if err != nil {
		return fmt.Errorf("read schema %d fixture source: %w", f.Schema, err)
	}
	declaration := regexp.MustCompile(fmt.Sprintf(`(?m)^const Schema = %d$`, f.Schema))
	if !declaration.Match(body) {
		return fmt.Errorf("fixture source does not declare schema %d", f.Schema)
	}
	return nil
}

func TestMigrationFixtureSourceDeclaresRecordedSchema(t *testing.T) {
	body, fixture, err := readMigrationFixture("testdata/migrations", 6)
	requireOK(t, err)
	requireOK(t, fixture.verifySource(t.Context()))
	wrongSchema := fixture
	wrongSchema.Schema++
	if wrongSchema.verifySource(t.Context()) == nil {
		t.Fatal("fixture source schema mismatch was accepted")
	}
	wrongCommit := fixture
	wrongCommit.SourceCommit = strings.Repeat("0", 40)
	if wrongCommit.verifySource(t.Context()) == nil {
		t.Fatal("missing fixture source revision was accepted")
	}
	fixture.SourceCommit = strings.Repeat("x", 40)
	if fixture.validate(6, body) == nil {
		t.Fatal("non-hex fixture revision was accepted")
	}
}
