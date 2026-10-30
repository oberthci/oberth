package runlog

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// This benchmark intentionally uses only the pre-fix public API, so the same
// fixture can be run unchanged on the baseline and the bounded implementation.
func BenchmarkRunLogRead(b *testing.B) {
	for _, mib := range []int{8, 64} {
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			store, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			line := []byte("[bench/step] match " + strings.Repeat("x", 100) + "\n")
			body := bytes.Repeat(line, (mib<<20)/len(line))
			file, err := store.Create("bench")
			if err != nil {
				b.Fatal(err)
			}
			if _, err := file.Write(body); err != nil {
				b.Fatal(err)
			}
			if err := file.Close(); err != nil {
				b.Fatal(err)
			}
			if _, err := store.BuildIndex("bench"); err != nil {
				b.Fatal(err)
			}
			for _, tc := range []struct {
				name   string
				active bool
				filter Filter
			}{
				{"terminal-head20", false, Filter{Pattern: "match", Limit: 20}},
				{"active-tail20", true, Filter{Pattern: "match", Tail: true, Limit: 20}},
				{"terminal-sparse", false, Filter{Pattern: "absent", Tail: true, Limit: 20}},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						var err error
						if tc.active {
							_, _, err = store.ReadActiveFiltered("bench", "bench", "step", tc.filter)
						} else {
							_, _, err = store.ReadFiltered("bench", "bench", "step", tc.filter)
						}
						if err != nil && !os.IsNotExist(err) {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
