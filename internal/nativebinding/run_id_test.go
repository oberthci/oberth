package nativebinding

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestValidChildRunIDRequiresBoundedCanonicalChildren(t *testing.T) {
	parent := cronName(1, "daily")
	maxParent := cronName(math.MaxInt64, "daily")
	for _, id := range []string{parent + "-1", parent + "-1795012440", maxParent + "-" + strconv.FormatInt(math.MaxInt64, 10)} {
		if !ValidChildRunID(id) {
			t.Error("valid run ID refused", id)
		}
	}
	for _, id := range []string{"", "abcdef01", "abcdef0", "ABCDEF01", "not-a-run", parent, parent + "-", parent + "-0", parent + "-01", parent + "-+1", parent + "--1", parent + "-1-extra", parent + "-9223372036854775808", parent + "-1/other", strings.Replace(parent, "cron-1-", "cron-01-", 1) + "-1", strings.Replace(parent, "cron-1-", "cron-0-", 1) + "-1", strings.Replace(parent, "cron-1-", "cron-1Z-", 1) + "-1", strings.Replace(parent, "cron-1-", "cron-zzzzzzzzzzzzzz-", 1) + "-1", "oberth-cron-1-" + strings.Repeat("g", 24) + "-1", "oberth-cron-1-" + strings.Repeat("a", 23) + "-1", "oberth-cron-1-" + strings.Repeat("A", 24) + "-1", parent + "-" + strings.Repeat("1", 100)} {
		if ValidChildRunID(id) {
			t.Error("malformed run ID admitted", id)
		}
	}
}
