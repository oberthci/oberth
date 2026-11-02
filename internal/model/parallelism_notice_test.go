package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParallelismNoticeRetainedAndValidated(t *testing.T) {
	for _, test := range []struct {
		name, demand, want string
		valid              bool
	}{
		{"static", `{"cpu_milli":8000,"memory_bytes":25769803776,"pods":4}`, "effective concurrency: 4 pods", true},
		{"dynamic", `{"cpu_milli":8000,"memory_bytes":25769803776,"pods":4,"assumptions":1}`, "item count is unknown", true},
		{"nested", `{"cpu_milli":8000,"memory_bytes":25769803776,"pods":8,"assumptions":2}`, "full inner fan-out demand", true},
		{"graph", `{"cpu_milli":8000,"memory_bytes":25769803776,"pods":4,"assumptions":4}`, "largest declared pod", true},
		{"invalid", `{"cpu_milli":8000,"memory_bytes":25769803776,"pods":0}`, "", false},
		{"unknown-reason", `{"cpu_milli":8000,"memory_bytes":25769803776,"pods":4,"assumptions":128}`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var c ResourceCoverage
			if err := json.Unmarshal([]byte(`{"declared":1,"pod_demand":`+test.demand+`}`), &c); err != nil {
				t.Fatal(err)
			}
			message := c.Message(strings.Repeat("a", 32))
			if c.Valid() != test.valid || (test.valid && !strings.Contains(message, test.want)) || (!test.valid && message != "") {
				t.Fatalf("valid=%v message=%q", c.Valid(), message)
			}
		})
	}
}
