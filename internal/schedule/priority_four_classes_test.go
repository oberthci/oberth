package schedule

import (
	"fmt"
	"testing"
	"time"
)

func TestFourPriorityScheduleNames(t *testing.T) {
	for input, want := range map[string]string{"low": "low", "standard": "standard", "promotion": "promotion", "high": "high"} {
		file, err := Parse([]byte(fmt.Sprintf("schedules:\n  - name: nightly\n    cron: '0 3 * * *'\n    priority: %s\n", input)), testLimits(), time.Now())
		if err != nil || len(file.Entries) != 1 {
			t.Errorf("%s refused: %v", input, err)
			continue
		}
		if got := string(file.Entries[0].Priority); got != want {
			t.Errorf("%s=%s want %s", input, got, want)
		}
	}
}

func TestRemovedPriorityAliasesAreRefused(t *testing.T) {
	for _, input := range []string{"normal", "highest"} {
		if _, err := Parse([]byte(fmt.Sprintf("schedules:\n  - name: nightly\n    cron: '0 3 * * *'\n    priority: %s\n", input)), testLimits(), time.Now()); err == nil {
			t.Errorf("%s accepted", input)
		}
	}
}
