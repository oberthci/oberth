package main

import (
	"encoding/json"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func observationExitCode(tool string, data json.RawMessage) int {
	if tool != "run_wait" {
		return 0
	}
	var result apicontract.WaitResult
	if json.Unmarshal(data, &result) != nil {
		return 1
	}
	if result.Run != nil && (result.Run.Status == stepmodel.RunFailed || result.Run.Status == stepmodel.RunCancelled) {
		return 1
	}
	return 0
}
