package fake_test

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/cronsync/contracttest"
	"github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestNativeContract(t *testing.T) {
	contracttest.RunNative(t, func(*testing.T) contracttest.NativeFixture {
		return contracttest.NativeFixture{Port: fake.NewNative("ci", func() time.Time { return time.Unix(123, 0) }, nil), Namespace: "ci"}
	})
}
func TestGitContract(t *testing.T) {
	contracttest.RunGit(t, func(*testing.T) contracttest.GitFixture {
		ref := gitport.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("a", 40)}
		revision := gitport.Revision{Repository: "origin/team/widget", Commit: ref.CommitSHA}
		file := gitport.File{Path: ".oberth/schedule.yaml", Mode: 0100644, Blob: strings.Repeat("b", 40), Bytes: []byte("schedules: []")}
		git := fake.NewGit(nil)
		git.PutRef(revision.Repository, ref)
		git.PutFile(revision, file)
		return contracttest.GitFixture{Port: git, Revision: revision, Ref: ref, File: file}
	})
}
