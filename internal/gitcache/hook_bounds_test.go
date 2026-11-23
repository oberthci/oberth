package gitcache

import (
	"fmt"
	"strings"
	"testing"
)

func TestReservedPatternAccepts255Refuses256(t *testing.T) {
	for _, n := range []int{255, 256} {
		c, err := New(t.Context(),
			Config{
				Root:             t.TempDir(),
				Scratch:          t.TempDir(),
				Binary:           "/usr/bin/git",
				ReservedBranches: []string{strings.Repeat("a", n)},
			},
		)
		if c != nil {
			_ = c.Close()
		}
		if (err == nil) != (n == 255) {
			t.Fatal(n, err)
		}
	}
}
func TestHookOptionsAcceptsThreeRefusesFour(t *testing.T) {
	t.Setenv("GIT_PUSH_OPTION_0", "oberth.assist=1")
	t.Setenv("GIT_PUSH_OPTION_1", "oberth.run=1")
	t.Setenv("GIT_PUSH_OPTION_2", "oberth.file=hack/walk-vm.yaml")
	t.Setenv("GIT_PUSH_OPTION_COUNT", "3")
	if _, err := hookOptions(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_PUSH_OPTION_COUNT", "4")
	if _, err := hookOptions(); err == nil || !strings.Contains(err.Error(), reasonOptionCount) {
		t.Fatal("fourth option accepted")
	}
}

func TestHookLineAccepts4096BytesRefuses4097(t *testing.T) {
	for _, size := range []int{4096, 4097} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			_, session, _ := gateFixture(t)
			record, err := readReservation(session.store)
			if err != nil {
				t.Fatal(err)
			}
			record.AlwaysRun = false
			line := strings.Repeat("0", 40) + " " + strings.Repeat("a", 40) + " refs/heads/topic"
			line += strings.Repeat(" ", size-len(line)-1) + "\n"
			t.Setenv("OBERTH_GIT_BINARY", "/usr/bin/git")
			t.Setenv("GIT_PUSH_OPTION_COUNT", "0")
			err = receiveHook(t.Context(), strings.NewReader(line), session.store, &record)
			if (err == nil) != (size == 4096) {
				t.Fatalf("hook line %d: %v", size, err)
			}
		})
	}
}
