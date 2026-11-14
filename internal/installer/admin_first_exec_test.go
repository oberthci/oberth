package installer

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"
)

func TestFirstAdminDaemonStartupRetry(t *testing.T) {
	t.Run("SocketAppears", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := 0
			deps := Deps{PollInterval: time.Second, RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					return []byte("request daemon audit mutation gate: dial unix /data/admin.sock: connect: no such file or directory"), errors.New("exec failed")
				}
				return []byte("public response"), nil
			}}
			raw, err := runFirstAdminExec(t.Context(), deps, []byte("public request"), []string{"admin", "create-first-admin"})
			if err != nil || calls != 2 || string(raw) != "public response" {
				t.Fatal("startup socket was not retried", err, calls)
			}
		})
	})
	for _, failure := range []error{io.ErrUnexpectedEOF, errors.New("different key refused")} {
		t.Run(failure.Error(), func(t *testing.T) {
			calls := 0
			deps := Deps{RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) { calls++; return nil, failure }}
			if _, err := runFirstAdminExec(t.Context(), deps, nil, nil); err == nil || calls != 1 {
				t.Fatal("credential refusal or lost response retried", err, calls)
			}
		})
	}
}
