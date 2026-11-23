package gitport

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestLostForgeHistoryRemedyRequiresRestoration(t *testing.T) {
	err := &ForgeHistoryLostError{}
	problem := publicerr.Classify(err)
	var empty *EmptyRepositoryError
	if errors.As(err, &empty) || problem.Code() != publicerr.InvalidInput {
		t.Fatal("lost history permits bootstrap or automatic retry", problem)
	}
	if !strings.HasPrefix(problem.Remedy(), "Restore ") || !strings.Contains(problem.Remedy(), "refs on the forge") || strings.Contains(problem.Remedy(), "first commit") {
		t.Fatal("lost-history remedy invites another bootstrap push", problem.Remedy())
	}
	if publicerr.CheckMessage(problem.Reason(), 40) != nil || publicerr.CheckMessage(problem.Remedy(), 120) != nil {
		t.Fatal("lost-history message exceeds its bounds", problem)
	}
	data, marshalErr := json.Marshal(problem)
	if marshalErr != nil || !strings.Contains(string(data), problem.Reason()) || !strings.Contains(string(data), problem.Remedy()) {
		t.Fatal("public JSON lost the restore remedy", string(data), marshalErr)
	}
}
