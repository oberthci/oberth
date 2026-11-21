package runtimeconfig

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSnapshotCloneIsIndependentAndPreservesNil(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, {}, []byte(`{"spec":{"entrypoint":"main"}}`)} {
		original := WorkflowDefaultsSnapshot{WorkflowDefaults: raw, Digest: "frozen"}
		copy := original.Clone()
		if !reflect.DeepEqual(original, copy) {
			t.Fatal("clone changed value")
		}
		if len(copy.WorkflowDefaults) > 0 {
			copy.WorkflowDefaults[0] = '['
			if original.WorkflowDefaults[0] != '{' {
				t.Fatal("snapshot shares bytes")
			}
		}
	}
}
