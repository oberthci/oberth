package identityname

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

const (
	maxNameBytes  = 63
	hashHexDigits = 20
)

// Kind separates repository and step identities, including their CI counterparts.
type Kind string

const (
	Pipeline   Kind = "pipeline"
	PipelineCI Kind = "pipeline_ci"
	Step       Kind = "step"
	StepCI     Kind = "step_ci"
)

func (k Kind) Valid() bool {
	switch k {
	case Pipeline, PipelineCI, Step, StepCI:
		return true
	}
	return false
}

// Key identifies a canonical repository, exact workflow file and step. Repository
// identities leave File and Step empty. A grant may use Step "*" for its whole file.
// The tuple is shared by identity derivation, grant policy and admission evidence.
type Key struct {
	Repository string `json:"repo"`
	File       string `json:"file"`
	Step       string `json:"step"`
}

// InvalidKeyError identifies missing or inconsistent naming facts.
type InvalidKeyError struct{ reason string }

func (e *InvalidKeyError) Error() string { return "identity key: " + e.Reason() + ". " + e.Remedy() }

func (e *InvalidKeyError) Reason() string {
	if e.reason == "" {
		return "identity key is incomplete"
	}
	return e.reason
}

func (e *InvalidKeyError) Remedy() string {
	return "Provide a canonical repository and identity kind; step identities also need an exact file and step."
}

// Name derives the service-account and role name trusted by identity providers.
// Repository names hash the slash-joined repository; step names hash the NUL-joined
// upstream, organization, repository, file and step. CI changes only the prefix.
// Callers validate repository/file authority; deriving a name grants nothing.
func Name(key Key, kind Kind) (string, error) {
	if err := validateKey(key, kind); err != nil {
		return "", err
	}
	prefix, input, readable, fallback := "oberth-argo-", key.Repository, key.Repository, "repo"
	switch kind {
	case PipelineCI:
		prefix = "oberth-argo-ci-"
	case Step, StepCI:
		prefix = "oberth-step-"
		if kind == StepCI {
			prefix = "oberth-step-ci-"
		}
		input = strings.Replace(key.Repository, "/", "\x00", 2) + "\x00" + key.File + "\x00" + key.Step
		readable += "-" + key.File + "-" + key.Step
		fallback = "step"
	}
	return formatName(prefix, input, readable, fallback), nil
}

func formatName(prefix, input, readable, fallback string) string {
	digest := sha256.Sum256([]byte(input))
	suffix := hex.EncodeToString(digest[:])[:hashHexDigits]
	readable = strings.Trim(strings.Map(labelRune, strings.ToLower(readable)), "-")
	budget := maxNameBytes - len(prefix) - len(suffix) - 1
	if len(readable) > budget {
		readable = strings.TrimRight(readable[:budget], "-")
	}
	if readable == "" {
		readable = fallback
	}
	return prefix + readable + "-" + suffix
}

func validateKey(key Key, kind Kind) error {
	if !kind.Valid() || len(strings.SplitN(key.Repository, "/", 3)) != 3 {
		return &InvalidKeyError{reason: "kind and qualified repository are required"}
	}
	if !utf8.ValidString(key.Repository + key.File + key.Step) {
		return &InvalidKeyError{reason: "identity key must be valid UTF-8"}
	}
	isStep := kind == Step || kind == StepCI
	if isStep && (key.File == "" || key.Step == "") {
		return &InvalidKeyError{reason: "step identities require exact file and step"}
	}
	if !isStep && (key.File != "" || key.Step != "") {
		return &InvalidKeyError{reason: "repository identities require empty file and step"}
	}
	return nil
}

func labelRune(r rune) rune {
	if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
		return r
	}
	return '-'
}
