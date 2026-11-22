package gitcache_test

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitport"
)

// A read of the default head that is not an admission decision (a
// publication's predecessor, the promotion policy) keeps reporting a forge
// whose listing does not finish as an outage: a default branch gone upstream
// never reads as absent there.
func TestDefaultHeadReadOutsideAdmissionReportsAnUnfinishedListingAsAnOutage(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	f := x.pipeline
	f.Git(t, f.Work, "push", "-q", f.Bare, "main:refs/heads/other")
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/*")
	gitcache.SetRefreshWait(x.c, 300*time.Millisecond)
	_, err := x.c.RemoteRef(t.Context(), pinnedPipeline, "refs/heads/main")
	var absent *gitport.AbsentError
	var outage *gitport.UnavailableError
	if errors.As(err, &absent) || !errors.As(err, &outage) || outage.Op != "forge" {
		t.Fatal("an unfinished listing was not reported as a forge outage", err)
	}
}
