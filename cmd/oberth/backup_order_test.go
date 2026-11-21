package main

import (
	"context"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
)

func TestBackupStreamEOFWaitsForCompletionAudit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		allowAudit, recorded := make(chan struct{}), make(chan struct{})
		stream := backupStream(t.Context(), func(_ context.Context, out io.WriteCloser) error {
			if _, err := io.WriteString(out, "archive"); err != nil {
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			<-allowAudit
			close(recorded)
			return nil
		})
		defer stream.Close()
		released := false
		defer func() {
			if !released {
				close(allowAudit)
			}
		}()
		result := make(chan error, 1)
		go func() {
			data, err := io.ReadAll(stream)
			if err == nil && string(data) != "archive" {
				err = fmt.Errorf("archive bytes changed")
			}
			select {
			case <-recorded:
			default:
				err = fmt.Errorf("EOF arrived before the completion audit")
			}
			result <- err
		}()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatal("stream ended while completion audit was blocked", err)
		default:
		}
		close(allowAudit)
		released = true
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	})
}
