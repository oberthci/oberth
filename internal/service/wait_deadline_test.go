package service

import (
	"errors"
	"testing"
	"time"
)

func TestWaitDurationRejectsOversizedAndOverflowingSeconds(t *testing.T) {
	t.Parallel()
	service := &API{maximumWait: DefaultMaximumWait}
	for _, seconds := range []int{-1, 601, int(^uint(0) >> 1)} {
		if _, err := service.waitDuration(seconds); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("timeout %d accepted: %v", seconds, err)
		}
	}
	for _, seconds := range []int{0, 1, 600} {
		got, err := service.waitDuration(seconds)
		want := time.Duration(seconds) * time.Second
		if seconds == 0 {
			want = DefaultMaximumWait
		}
		if err != nil || got != want {
			t.Fatalf("timeout %d = %s, %v; want %s", seconds, got, err, want)
		}
	}
	service.maximumWait = 500 * time.Millisecond
	if _, err := service.waitDuration(1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("custom ceiling ignored: %v", err)
	}
}
