package model

import "testing"

func TestNativeUID(t *testing.T) {
	for _, tc := range []struct {
		origin RunOrigin
		want   string
	}{
		{RunOrigin{WorkflowUID: "original"}, "original"},
		{RunOrigin{WorkflowUID: "original", ResumedFrom: "r1", ResumeUID: "resumed"}, "resumed"},
		{RunOrigin{WorkflowUID: "original", ResumedFrom: "r1"}, ""},
	} {
		if got := tc.origin.NativeUID(); got != tc.want {
			t.Fatalf("NativeUID = %q, want %q", got, tc.want)
		}
	}
}
