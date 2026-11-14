package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func TestRestorePreviewRespectsServerSnapshotSpaceRefusal(t *testing.T) {
	source := newFakeBackupCluster(t, sourceCompat)
	var output bytes.Buffer
	deps := source.deps(&output)
	calls := 0
	refusal := "backup space refused: volume /data has 10 bytes free, needs 268435456 bytes; grow/free the volume"
	deps.Admin = streamingFixtureAdmin{AdminAPI: deps.Admin, download: func(context.Context, int64, io.Writer) error { calls++; return errors.New(refusal) }}
	deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("preview must not bypass refusal with a utility Pod")
		return nil, nil
	}
	refreshFixtureAdmin(&deps)
	target := &restoreTarget{Pod: "oberth-1", Schema: store.LatestMigrationVersion()}
	target.Server.Compatibility = sourceCompat
	readTargetContents(t.Context(), RestoreConfig{Namespace: DefaultNamespace}, deps, target, t.TempDir())
	if calls != 1 || target.Contents != nil || !strings.Contains(target.ContentsNote, refusal) {
		t.Fatal("preview did not retain the space refusal", calls, target.Contents, target.ContentsNote)
	}
}
