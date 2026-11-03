package app

import "testing"

func TestDatabaseDeadlineRetryCountInUnscopedStatus(t *testing.T) {
	h := Health{DatabaseRetries: func() uint64 { return 17 }}
	value, err := h.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if value.(HealthStatus).DatabaseRetries != 17 {
		t.Fatal("background retries missing from status", value)
	}
}
