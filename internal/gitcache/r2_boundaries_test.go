package gitcache

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"k8s.io/utils/clock"
	"strings"
	"testing"
	"time"
)

func TestBatchSizeAndEndFramingRejectMalformedNativeResponses(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	for _, test := range []struct {
		name, header, body string
		ok                 bool
	}{
		{"exact", "2", "xy\n", true}, {"size", "3", "xyz\n", false}, {"framing", "2", "xy!", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := batchObjects{input: io.Discard,
				output: bufio.NewReader(strings.NewReader(fmt.Sprintf("%s blob %s\n%s",
					oid,
					test.header,
					test.body)))}
			err := reader.copy(t.Context(), treeEntry{oid: oid, size: 2}, io.Discard)
			if (err == nil) != test.ok {
				t.Fatal("malformed batch response accepted", err)
			}
		})
	}
}

type resetCounter struct{ count int }

func (*resetCounter) Stop() bool                 { return true }
func (c *resetCounter) Reset(time.Duration) bool { c.count++; return true }
func TestZeroByteProgressNeverExtendsIdleDeadline(t *testing.T) {
	t.Parallel()
	timer := &resetCounter{}
	p := &commandProgress{now: (clock.RealClock{}).Now, timer: timer, idle: time.Hour}
	p.touch(0)
	if timer.count != 0 {
		t.Fatal("zero bytes reset idle deadline")
	}
	p.touch(1)
	if timer.count != 1 {
		t.Fatal("positive progress did not reset deadline")
	}
}
func TestPushUsesOneHourTransferBudgetAndLocalWorkHasNoIdle(t *testing.T) {
	t.Parallel()
	if !transferCommand([]string{"push"}) ||
		transferCommand([]string{"gc"}) ||
		transferCommand([]string{"repack"}) ||
		transferCommand([]string{"prune"}) {
		t.Fatal("wrong command budget class")
	}
	b := defaultGitBudgets()
	if b.transfer != time.Hour || b.local != 10*time.Minute {
		t.Fatal(b)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	p := watchProgress(cancel, 0)
	defer p.close()
	if p.timer != nil || ctx.Err() != nil {
		t.Fatal("silent local operation received idle timer")
	}
}
