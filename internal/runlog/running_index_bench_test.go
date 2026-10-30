package runlog

import (
	"fmt"
	"strings"
	"testing"
)

// Uses only APIs present in the RC, so this exact fixture also runs there.
// Warm-up permits bounded 2MiB extensions; the baseline keeps rescanning.
func BenchmarkRunningIndex(b *testing.B) {
	for _, mib := range []int{8, 64} {
		for _, appendBytes := range []bool{false, true} {
			b.Run(fmt.Sprintf("%dMiB/append-%t", mib, appendBytes), func(b *testing.B) {
				store, err := Open(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				file, err := store.Create("running-bench")
				if err != nil {
					b.Fatal(err)
				}
				noise := "[other/step] " + strings.Repeat("x", 100) + "\n"
				line := "[wanted/step] match " + strings.Repeat("y", 100) + "\n"
				if _, err := file.WriteString(strings.Repeat(noise, (mib<<20)/len(noise)) + strings.Repeat(line, 1000)); err != nil {
					b.Fatal(err)
				}
				filter := Filter{Tail: true, Pattern: "match", Limit: 20}
				for range mib/2 + 3 {
					if _, _, err := store.ReadActiveFiltered("running-bench", "wanted", "step", filter); err != nil {
						b.Fatal(err)
					}
				}
				var discovery int64
				b.ReportAllocs()
				for b.Loop() {
					if appendBytes {
						if _, err := file.WriteString(line); err != nil {
							b.Fatal(err)
						}
					}
					body, meta, err := store.ReadActiveFiltered("running-bench", "wanted", "step", filter)
					if err != nil || len(body) == 0 {
						b.Fatalf("active read: %v", err)
					}
					discovery += meta.DiscoveryBytes
				}
				b.ReportMetric(float64(discovery)/float64(b.N), "discovery_B/op")
				if err := file.Close(); err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}
