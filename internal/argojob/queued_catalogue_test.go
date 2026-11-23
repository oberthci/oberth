package argojob

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestQueuedReasonRanksMatchNativeCatalogue(t *testing.T) {
	for _, advanced := range []string{reasonStarting, reasonImage} {
		for _, waiting := range []string{reasonWaiting, reasonNode, reasonRunSlot, reasonLock, fmt.Sprintf(reasonSyncLimit, "heavy-go"), fmt.Sprintf(reasonMutex, "publish")} {
			status, reason := stepmodel.Progress(stepmodel.RunQueued, "", nil, advanced)
			status, reason = stepmodel.Progress(status, reason, nil, waiting)
			if status != stepmodel.RunQueued || reason != advanced {
				t.Fatalf("native catalogue wait %q replaced %q: %s / %q", waiting, advanced, status, reason)
			}
		}
	}
}
