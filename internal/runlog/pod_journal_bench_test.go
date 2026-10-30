package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Several chatty step writers share one run's serialized commit path. The
// legacy baseline adds its prefix/projection barriers around the synced WAL.
func BenchmarkPodJournalConcurrentChatty(b *testing.B) {
	for _, extraSync := range []bool{true, false} {
		name := "one-sync"
		if extraSync {
			name = "legacy-three-sync"
		}
		b.Run(name, func(b *testing.B) {
			f, err := os.OpenFile(filepath.Join(b.TempDir(), "run.log"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				b.Fatal(err)
			}
			defer f.Close()
			j, err := OpenPodLogJournal(f, 1<<30)
			if err != nil {
				b.Fatal(err)
			}
			defer j.Close()
			var mu sync.Mutex
			var workers atomic.Int64
			body := []byte(strings.Repeat("[test/unit] "+strings.Repeat("x", 112)+"\n", 32))
			b.SetBytes(int64(len(body)))
			b.SetParallelism(4)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				id := workers.Add(1)
				node, pod := fmt.Sprintf("node-%d", id), fmt.Sprintf("pod-%d", id)
				c := PodLogCursor{Key: "workflow/" + node + "/" + pod, WorkflowUID: "workflow", Node: node, Pod: pod, Container: "main", At: time.Now().UTC()}
				for pb.Next() {
					mu.Lock()
					c.Count++
					c.Bytes += int64(len(body))
					var err error
					if extraSync {
						err = f.Sync()
					}
					if err == nil {
						err = j.Commit(c, body, f.Write)
					}
					if err == nil && extraSync {
						err = f.Sync()
					}
					mu.Unlock()
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			syncs := 1.0
			if extraSync {
				syncs = 3
			}
			b.ReportMetric(syncs, "syncs/op")
		})
	}
}
