package service_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestTimeoutReauthorizesAfterFinalSnapshot(t *testing.T) {
	for _, change := range []string{"revoke", "hide-run", "narrow-scope"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := setup(t)
				f.seed("running")
				reads := 0
				f.c.Runs = stateReadHook{RunStore: f.store, read: func(ctx context.Context, id string) (stateport.RunState, error) {
					value, err := f.store.RunState(ctx, id)
					reads++
					if reads == 2 {
						f.store.mu.Lock()
						a := f.store.Actors[caller().Fingerprint]
						switch change {
						case "revoke":
							delete(f.store.Actors, a.Fingerprint)
						case "hide-run":
							a.Scope = []string{"private"}
							f.store.Actors[a.Fingerprint] = a
						case "narrow-scope":
							a.Scope = []string{"origin"}
							f.store.Actors[a.Fingerprint] = a
						}
						f.store.mu.Unlock()
						if err := f.store.native.RecordAction(ctx, model.AuditAction{Actor: a.Identity, Action: "scope.changed", ResourceType: "key", ResourceID: a.Fingerprint}); err != nil {
							t.Fatal(err)
						}
					}
					return value, err
				}}
				f.restart(t)
				result, err := f.s.RunWait(t.Context(), caller(), viewQuery{ID: runID}, time.Second)
				if reads != 2 {
					t.Fatalf("expected initial and timeout reads, got %d", reads)
				}
				if err == nil || result.Run != nil {
					t.Fatalf("authority change delivered timeout snapshot: %+v, err=%v", result, err)
				}
			})
		})
	}
}
