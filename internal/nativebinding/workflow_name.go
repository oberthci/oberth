package nativebinding

import (
	"crypto/sha256"
	"encoding/hex"
)

// WorkflowName is the fixed-length run- plus 32 lowercase hexadecimal digits
// used by wiring for both creation and unbound recovery. Distinct names cannot
// extend one another with a dash; task-result admission relies on that property.
func WorkflowName(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return "run-" + hex.EncodeToString(sum[:16])
}
