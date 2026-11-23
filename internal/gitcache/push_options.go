package gitcache

import (
	"github.com/oberthci/oberth/internal/sourcecontract"
	"strings"
)

type PushOptions struct {
	Assist, Run bool
	File        string
}

func ParsePushOptions(options []string) (PushOptions, error) {
	var result PushOptions
	seen := make(map[string]bool)
	for _, option := range options {
		k, v, ok := strings.Cut(option, "=")
		if !ok || seen[k] {
			return result, refuseOptions()
		}
		seen[k] = true
		if k == "oberth.file" {
			if sourcecontract.ValidateFilePath(v) != nil {
				return result, refuseOptions()
			}
			result.File = v
			continue
		}
		if v != "0" && v != "1" {
			return result, refuseOptions()
		}
		switch k {
		case "oberth.assist":
			result.Assist = v == "1"
		case "oberth.run":
			result.Run = v == "1"
		default:
			return result, refuseOptions()
		}
	}
	if result.File != "" && !result.Run {
		return result, refuseOptions()
	}
	return result, nil
}
func refuseOptions() error { return &OptionsError{} }

type OptionsError struct{}

func (*OptionsError) Error() string {
	return reasonOptions + ". " + remedyOptions
}
