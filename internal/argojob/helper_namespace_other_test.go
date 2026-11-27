//go:build !linux

package argojob

import "testing"

func TestInspectionCannotInvokeRerunEvenWithoutSecrets(t *testing.T) {
	t.Skip("held-command reruns require Linux PID namespaces")
}
