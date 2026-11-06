package protectedtests

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// PlacementAdmission retains public configuration for original-owner cleanup.
// It contains no transport credentials and grants no permission by itself.
// The controller validates the entire closed placement and configuration; the
// store separately joins this projection to the canonical run and audit chain.
type PlacementAdmission struct {
	Sources         *PlacementSources `json:"sources,omitempty"`
	Version         int               `json:"version"`
	Placement       json.RawMessage   `json:"placement"`
	Config          json.RawMessage   `json:"config"`
	PlacementSHA256 string            `json:"placement_sha256"`
	ConfigSHA256    string            `json:"config_sha256"`
	RuntimeSHA256   string            `json:"runtime_sha256"`
	IntentSHA256    string            `json:"intent_sha256"`
}

// PlacementScope is only a projection of a retained authority document. It
// cannot construct an executor's immutable ExecutionPlacement capability.
type PlacementScope struct {
	GraphDigest        string              `json:"graph_digest,omitempty"`
	FragmentLockDigest string              `json:"fragment_lock_digest,omitempty"`
	Fragments          []PlacementFragment `json:"fragments,omitempty"`
	Schema             string              `json:"schema"`
	RunID              string              `json:"parent_run_id"`
	Repository         string              `json:"repository"`
	RepositoryID       int64               `json:"repository_id"`
	Ref                string              `json:"ref"`
	SHA                string              `json:"sha"`
	SourceDigest       string              `json:"source_digest"`
	PipelineFile       string              `json:"pipeline_file"`
	Namespace          string              `json:"namespace"`
	ServiceAccount     string              `json:"service_account"`
	NormalWorkflow     string              `json:"normal_workflow"`
	PreflightWorkflow  string              `json:"preflight_workflow"`
	Expires            time.Time           `json:"expires"`
	Policy             PlacementPolicy     `json:"protected_policy"`
}

func NewPlacementAdmission(placement, config []byte, runtime GenericAdmission) (PlacementAdmission, error) {
	a := PlacementAdmission{Version: 1, Placement: bytes.Clone(placement), Config: bytes.Clone(config), PlacementSHA256: placementBodyDigest(placement), ConfigSHA256: placementBodyDigest(config), RuntimeSHA256: runtime.Digest(), IntentSHA256: IntentDigest(runtime.Intent)}
	return a, a.Validate(runtime)
}
func (a PlacementAdmission) Scope() (PlacementScope, error) {
	var scope PlacementScope
	err := json.Unmarshal(a.Placement, &scope)
	return scope, err
}
func (a PlacementAdmission) Validate(runtime GenericAdmission) error {
	if ((a.Version != 1 || a.Sources != nil) && (a.Version != 2 || a.Sources == nil)) || runtime.Validate() != nil || len(a.Placement) == 0 || len(a.Placement) > 65536 || len(a.Config) == 0 || len(a.Config) > 256<<10 || !json.Valid(a.Placement) || !json.Valid(a.Config) || a.PlacementSHA256 != placementBodyDigest(a.Placement) || a.ConfigSHA256 != placementBodyDigest(a.Config) || a.RuntimeSHA256 != runtime.Digest() || a.IntentSHA256 != IntentDigest(runtime.Intent) {
		return errors.New("invalid protected placement custody")
	}
	var config struct {
		ExecutionPlacement     json.RawMessage
		PipelineServiceAccount string
	}
	if json.Unmarshal(a.Config, &config) != nil || !bytes.Equal(config.ExecutionPlacement, a.Placement) {
		return errors.New("protected placement differs from retained executor configuration")
	}
	s, err := a.Scope()
	i := runtime.Intent
	if err != nil || s.Schema != "oberth.execution-placement/protected-v2" || s.RunID != i.RunID || s.Repository != i.Repository || s.RepositoryID != i.RepoID || s.SHA != i.CandidateSHA || s.SourceDigest != i.WorkflowSpecDigest || s.PipelineFile != ".oberth/build.yaml" || s.Ref == "" || s.Namespace != i.Namespace || s.ServiceAccount == "" || s.ServiceAccount != config.PipelineServiceAccount || s.NormalWorkflow == "" || s.PreflightWorkflow != s.NormalWorkflow+"-trusted" || s.PreflightWorkflow != i.WorkflowName || s.Expires.IsZero() || i.Deadline.After(s.Expires) || s.Policy.Match(runtime) != nil {
		return errors.New("protected placement custody differs from admitted host intent")
	}
	return a.validateSources(s)
}
func (a PlacementAdmission) Digest() string {
	body, _ := json.Marshal(a)
	return placementBodyDigest(body)
}
func placementBodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
