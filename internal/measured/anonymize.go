package measured

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Anonymizer replaces the names in a resource report with keyed hashes so
// usage data can be shared (#1524, oberth run resources --anonymize). Each
// export draws a fresh random salt that is never written out: the same name
// maps to the same token within one export, so steps stay comparable, while
// two exports cannot be joined and a dictionary of likely names cannot be
// checked against the tokens. Numbers, flags and Oberth's own fixed advice
// text are kept. The report names no container image; a step's image is
// identified only through its hashed step name.
type Anonymizer struct {
	salt []byte
}

// NewAnonymizer draws a 256-bit per-export salt.
func NewAnonymizer() (*Anonymizer, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("draw anonymization salt: %w", err)
	}
	return newAnonymizer(salt), nil
}

func newAnonymizer(salt []byte) *Anonymizer {
	return &Anonymizer{salt: salt}
}

// token is kind-<12 hex>: the kind keeps a repository and a step of the
// same spelling apart and says what the token stands for.
func (a *Anonymizer) token(kind, name string) string {
	if name == "" {
		return ""
	}
	mac := hmac.New(sha256.New, a.salt)
	_, _ = mac.Write([]byte(kind + "\x00" + name))
	return kind + "-" + hex.EncodeToString(mac.Sum(nil))[:12]
}

// Report returns an anonymized copy of report: repository, ref, burn and
// template (step) names, commit SHAs and run IDs become tokens.
func (a *Anonymizer) Report(report Report) Report {
	out := report
	out.Repository = a.token("repo", report.Repository)
	if report.Ref != "" && report.Ref != "any" && report.Ref != "default" {
		out.Ref = a.token("ref", report.Ref)
	}
	out.Run = a.token("run", report.Run)
	out.NewestSHA = a.token("sha", report.NewestSHA)
	out.SafetyRunIDs = a.tokens("run", report.SafetyRunIDs)
	out.Steps = make([]Step, len(report.Steps))
	for index, step := range report.Steps {
		anonymized := step
		anonymized.Stage = a.token("step", step.Stage)
		anonymized.Template = a.token("step", step.Template)
		anonymized.OwnerRepository = a.token("repo", step.OwnerRepository)
		anonymized.RunIDs = a.tokens("run", step.RunIDs)
		if step.Declared != nil {
			declared := *step.Declared
			declared.ResolvedTemplate = ""
			declared.Stage = a.token("step", step.Declared.Stage)
			declared.Template = a.token("step", step.Declared.Template)
			declared.OwnerRepository = a.token("repo", step.Declared.OwnerRepository)
			declared.SHA = a.token("sha", step.Declared.SHA)
			anonymized.Declared = &declared
		}
		out.Steps[index] = anonymized
	}
	return out
}

func (a *Anonymizer) tokens(kind string, names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, len(names))
	for index, name := range names {
		out[index] = a.token(kind, name)
	}
	return out
}
