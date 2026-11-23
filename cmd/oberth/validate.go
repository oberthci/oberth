package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type validationResult struct {
	OK       bool                        `json:"ok"`
	File     string                      `json:"file"`
	Plan     *admissioncontract.Plan     `json:"plan,omitempty"`
	Refusals []admissioncontract.Finding `json:"refusals"`
	Needs    []admissioncontract.Finding `json:"needs"`
	Detail   string                      `json:"detail,omitempty"`
}

func (s *flowSession) validateDocument(path string, document []byte) error {
	selected := validationSelectedFile(path)
	_, release, err := workflowpolicy.Select(nil, workflowpolicy.SelectionRequest{File: selected})
	if err != nil {
		return err
	}
	r := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, SelectedFile: selected, Document: document, Trigger: model.TriggerBranch}
	if release {
		r.Trigger = model.TriggerTag
	}
	prepared, err := argoworkflow.Prepare(s.ctx, r, argoworkflow.Policy{Release: release}, admissioncontract.PrepareHooks{OfflineValidation: true})
	result := validationResult{OK: err == nil, File: path, Refusals: []admissioncontract.Finding{}, Needs: []admissioncontract.Finding{}}
	if err != nil {
		var refused *argoworkflow.AdmissionError
		if !errors.As(err, &refused) {
			return err
		}
		result.Refusals = refused.Findings
		result.Needs = append(result.Needs, refused.Needs...)
		result.Detail = refused.Detail
	} else {
		view := prepared.View()
		result.Plan = &view.Plan
		result.Needs = append(result.Needs, view.Findings...)
	}
	for i := range result.Needs {
		if result.Needs[i].Code == "live" {
			result.Needs[i].Reason = "not checked offline"
			result.Needs[i].Remedy = "Run oberth check to verify live identity, templates and grants."
		}
	}
	if outputErr := s.validationOutput(result); outputErr != nil {
		return outputErr
	}
	return err
}

// Local checkout prefixes are not part of a run's selected repository file.
func validationSelectedFile(path string) string {
	selected := filepath.ToSlash(filepath.Clean(path))
	if at := strings.LastIndex(selected, "/.oberth/"); at >= 0 {
		selected = selected[at+1:]
	}
	if workflowpolicy.ValidateFile(selected) != nil {
		return "workflow.yaml"
	}
	return selected
}

func (s *flowSession) validationOutput(result validationResult) error {
	if s.options.json {
		return json.NewEncoder(s.app.out).Encode(result)
	}
	for _, f := range result.Refusals {
		if err := s.validationFinding(s.app.diagnostic, result.File, f); err != nil {
			return err
		}
	}
	if result.Plan != nil {
		for _, entry := range result.Plan.Entries {
			if err := s.fact("%s", entry.Name); err != nil {
				return err
			}
		}
	}
	if len(result.Needs) > 0 {
		if err := s.fact("Not checked offline:"); err != nil {
			return err
		}
		for _, f := range result.Needs {
			if err := s.validationFinding(s.app.out, result.File, f); err != nil {
				return err
			}
		}
	}
	if result.OK {
		return s.fact("%s valid offline; run oberth check for live checks", result.File)
	}
	return nil
}

func (s *flowSession) validationFinding(out io.Writer, file string, f admissioncontract.Finding) error {
	if err := writeWrapped(out, safeDisplay(fmt.Sprintf("%s %s: %s; %s", file, f.Path, f.Reason, f.Remedy))); err != nil {
		return err
	}
	if f.Command != "" {
		_, err := fmt.Fprintln(out, "  "+safeDisplay(f.Command))
		return err
	}
	return nil
}
