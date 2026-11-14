package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/runlog"
	corev1 "k8s.io/api/core/v1"
)

type cancellingReservationStore struct {
	runlog.ContainerLogStore
	cancel context.CancelFunc
}

func (store cancellingReservationStore) ReserveContainerLog(runID string, entry runlog.ContainerLog) (bool, error) {
	claimed, err := store.ContainerLogStore.ReserveContainerLog(runID, entry)
	if claimed {
		store.cancel()
	}
	return claimed, err
}

func TestContainerLogCancellationBeforeOpenReleasesReservation(t *testing.T) {
	for _, when := range []string{"reserved", "verify", "open", "after-open"} {
		t.Run(when, func(t *testing.T) {
			c, store, wf, pod := nativeContainerLogFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opens, verifies := 0, 0
			c.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
				opens++
				if when == "open" && ctx.Err() == nil {
					cancel()
					return nil, ctx.Err()
				}
				return io.NopCloser(strings.NewReader("copied once\n")), nil
			}, func(context.Context, string, string) (*corev1.Pod, error) {
				verifies++
				if when == "verify" && verifies == 1 {
					cancel()
					return nil, ctx.Err()
				}
				if when == "after-open" && verifies == 2 {
					cancel()
					return nil, ctx.Err()
				}
				return pod.DeepCopy(), nil
			})
			if when == "reserved" {
				c.containerLogs = cancellingReservationStore{ContainerLogStore: store, cancel: cancel}
			}
			if err := c.ObserveContainerLogs(ctx, wf, "run", []*corev1.Pod{pod}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			entries, err := store.ContainerLogs(t.Context(), "run")
			if err != nil || len(entries) != 1 {
				t.Fatal(entries, err)
			}
			entry := entries[0]
			if when == "after-open" {
				if !entry.Captured {
					t.Fatal("opened source lost immutable receipt", entry)
				}
			} else if entry.Captured || entry.Pending || entry.Reason != "" {
				t.Fatal("cancellation sealed a source that never opened", entry)
			}
			c.containerLogs = store
			if err := c.ObserveContainerLogs(t.Context(), wf, "run", []*corev1.Pod{pod}); err != nil {
				t.Fatal(err)
			}
			body, _, entry, err := store.ReadContainerLog(t.Context(), "run", "node", "main", runlog.Filter{})
			if err != nil || !entry.Captured {
				t.Fatal(string(body), entry, err)
			}
			if when != "after-open" && string(body) != "copied once\n" {
				t.Fatal("resumed copy unavailable", string(body))
			}
			want := 1
			if when == "open" {
				want = 2
			}
			if opens != want {
				t.Fatal("source reopened or skipped", opens, want)
			}
		})
	}
}
