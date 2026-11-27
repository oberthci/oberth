//go:build !linux

package argojob

import (
	"errors"
	"testing"
)

func TestInspectionCannotInvokeRerunEvenWithoutSecrets(t *testing.T) {
	namespaceUnavailable(t, errors.New("held-command reruns require Linux PID namespaces"))
}
