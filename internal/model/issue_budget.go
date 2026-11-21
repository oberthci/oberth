package model

import (
	"encoding/json"

	"github.com/oberthci/oberth/internal/publicerr"
)

// IssueBytes bounds the complete public issue, including retained source history.
const IssueBytes = 128 << 10

// CheckIssueBudget runs on the complete projection before an edit is committed.
func CheckIssueBudget(v Issue) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > IssueBytes {
		return publicerr.New(publicerr.InvalidInput, "The complete issue would exceed 131072 bytes.", "Shorten the title or body before retrying; retained source history is never removed.", nil)
	}
	return nil
}
