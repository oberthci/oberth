package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Parse once at test-binary startup: bad CI configuration fails even when a
// selected test never waits. Unset means 1; an explicitly empty value is invalid.
var serviceTestTimeoutScale = func() time.Duration {
	raw, set := os.LookupEnv("OBERTH_TEST_TIMEOUT_SCALE")
	if !set {
		return 1
	}
	scale, err := strconv.Atoi(raw)
	if err != nil || scale < 1 || scale > 100 {
		panic(fmt.Sprintf("OBERTH_TEST_TIMEOUT_SCALE must be an integer in 1..100, got %q", raw))
	}
	return time.Duration(scale)
}()

// testTimeout scales deliberately elapsed fixture timers, observation windows
// and polling intervals. Product timeout contracts and fake time stay literal.
func testTimeout(base time.Duration) time.Duration {
	return base * serviceTestTimeoutScale
}

// testStartTimeout bounds expected asynchronous progress, including startup,
// acquisition, completion and shutdown. These guards fail on expiry rather than
// deliberately waiting it out; allow at least 30s for a loaded host to schedule
// the worker. The floor adds no delay when the expected event arrives promptly.
func testStartTimeout(base time.Duration) time.Duration {
	return max(testTimeout(base), 30*time.Second)
}

func TestServiceTestTimeoutScale(t *testing.T) {
	if os.Getenv("OBERTH_TIMEOUT_SCALE_PROBE") == "1" {
		fmt.Printf("scaled=%d start=%d longer=%d\n", testTimeout(3*time.Millisecond), testStartTimeout(time.Second), testStartTimeout(20*time.Second))
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, raw string
		scale     int
	}{
		{"unset", "", 1}, {"one", "1", 1}, {"ci", "4", 4}, {"maximum", "100", 100},
		{"empty", "", 0}, {"zero", "0", 0}, {"negative", "-1", 0},
		{"too large", "101", 0}, {"fraction", "1.5", 0}, {"text", "slow", 0},
		{"whitespace", " 4", 0}, {"overflow", "999999999999999999999999", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(10*time.Second))
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestServiceTestTimeoutScale$", "-test.count=1")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "OBERTH_TEST_TIMEOUT_SCALE=") && !strings.HasPrefix(entry, "OBERTH_TIMEOUT_SCALE_PROBE=") {
					command.Env = append(command.Env, entry)
				}
			}
			command.Env = append(command.Env, "OBERTH_TIMEOUT_SCALE_PROBE=1")
			if test.name != "unset" {
				command.Env = append(command.Env, "OBERTH_TEST_TIMEOUT_SCALE="+test.raw)
			}
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("scale validation stalled:", ctx.Err())
			}
			if test.scale == 0 {
				if err == nil || !strings.Contains(string(output), "OBERTH_TEST_TIMEOUT_SCALE must be an integer in 1..100") {
					t.Fatalf("invalid scale %q: %v, %s", test.raw, err, output)
				}
			} else if err != nil || !strings.Contains(string(output), fmt.Sprintf("scaled=%d start=%d longer=%d\n", time.Duration(test.scale)*3*time.Millisecond, max(time.Duration(test.scale)*time.Second, 30*time.Second), max(time.Duration(test.scale)*20*time.Second, 30*time.Second))) {
				t.Fatalf("scale %q: %v, %s", test.raw, err, output)
			}
		})
	}
}
