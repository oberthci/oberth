package sourceplugin

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRestartRetriesOnlyConnectionLossBeforeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"refused", syscall.ECONNREFUSED, true}, {"reset", syscall.ECONNRESET, true},
		{"closed", io.EOF, true}, {"truncated headers", io.ErrUnexpectedEOF, true},
		{"trust", x509.UnknownAuthorityError{}, false}, {"cancelled", context.Canceled, false},
		{"departed host", dialError(os.NewSyscallError("connect", syscall.EHOSTUNREACH)), true},
		{"dial timeout", dialError(os.ErrDeadlineExceeded), true},
		{"unknown host", dialError(&net.DNSError{Err: "no such host", Name: "source.invalid", IsNotFound: true}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			waits := 0
			c := &Client{tokenFile: identity(t), endpoint: "https://source.invalid", now: func() time.Time { return time.Unix(0, 0) }, jitter: func(time.Duration) time.Duration { return time.Nanosecond }, wait: func(context.Context, time.Duration) error { return oneRetryWait(&waits) }}
			c.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, tc.err
				}
				return &http.Response{StatusCode: 403, Body: io.NopCloser(http.NoBody)}, nil
			})}
			r, err := c.response(t.Context(), "{}")
			if (err == nil) != tc.retry {
				t.Fatalf("retry=%v error=%v", tc.retry, err)
			}
			if r != nil {
				_ = r.Body.Close()
			}
			want := 1
			if tc.retry {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d want %d", calls, want)
			}
		})
	}
}

func TestRestartWaitLogsAtThirtySecondsAndHonorsCancellation(t *testing.T) {
	now := time.Unix(0, 0)
	var logs []time.Time
	c := &Client{now: func() time.Time { return now }, wait: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }, log: func(message string) {
		if message != reasonWaiting {
			t.Error(message)
		}
		logs = append(logs, now)
	}}
	next := now.Add(30 * time.Second)
	if err := c.retryWait(t.Context(), 29*time.Second, &next); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatal("premature waiting message", logs)
	}
	if err := c.retryWait(t.Context(), 61*time.Second, &next); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatal(logs)
	}
	for i, at := range logs {
		if want := time.Unix(0, 0).Add(time.Duration(i+1) * 30 * time.Second); !at.Equal(want) {
			t.Fatalf("log at %s want %s", at, want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.wait = func(ctx context.Context, _ time.Duration) error { return context.Cause(ctx) }
	if err := c.retryWait(ctx, time.Second, &next); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestForbiddenResponseNeverWaitsForRetry(t *testing.T) {
	waits := 0
	client := &Client{tokenFile: identity(t), endpoint: "https://source.invalid", now: func() time.Time { return time.Unix(0, 0) }, jitter: func(time.Duration) time.Duration { return time.Nanosecond }, wait: func(context.Context, time.Duration) error { waits++; return context.Canceled }}
	client.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	response, err := client.response(t.Context(), "{}")
	if err != nil || response.StatusCode != 403 || waits != 0 {
		t.Fatal("forbidden response retried", err, waits)
	}
	_ = response.Body.Close()
}

func dialError(err error) error { return &net.OpError{Op: "dial", Net: "tcp", Err: err} }

func oneRetryWait(waits *int) error {
	*waits++
	if *waits > 1 {
		return errors.New("unexpected extra retry")
	}
	return nil
}
