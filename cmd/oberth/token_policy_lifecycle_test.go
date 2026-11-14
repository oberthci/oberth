package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type tokenWarningBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *tokenWarningBuffer) Write(body []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(body)
}

func (buffer *tokenWarningBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func TestLegacyTokenWarningRepeatsHourlyAndStopsAfterRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output tokenWarningBuffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var count atomic.Int64
		count.Store(2)
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchNonExpiringUplinkTokens(ctx, func(context.Context) (int, error) { return int(count.Load()), nil }, log.New(&output, "", 0))
		}()
		synctest.Wait()
		if strings.Count(output.String(), "have no expiry") != 1 {
			t.Fatalf("startup warning: %s", &output)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if strings.Count(output.String(), "have no expiry") != 2 {
			t.Fatalf("missing periodic warning: %s", &output)
		}
		count.Store(0)
		time.Sleep(time.Hour)
		synctest.Wait()
		if strings.Count(output.String(), "have no expiry") != 2 {
			t.Fatalf("warning persisted after rotation: %s", &output)
		}
		cancel()
		<-done
	})
}

func TestLegacyTokenWarningReadErrorDoesNotExposeDetails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output tokenWarningBuffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchNonExpiringUplinkTokens(ctx, func(context.Context) (int, error) { return 0, errors.New("private diagnostic material") }, log.New(&output, "", 0))
		}()
		synctest.Wait()
		if !strings.Contains(output.String(), "unable to check") || strings.Contains(output.String(), "private diagnostic material") {
			t.Fatalf("unsafe or missing warning: %s", &output)
		}
		cancel()
		<-done
	})
}
