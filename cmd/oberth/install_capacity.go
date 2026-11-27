package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/installer"
)

func reviewCapacity(r installationRequest, in io.Reader, out io.Writer) error {
	if r.enabled("print-host-profile") {
		_, err := io.WriteString(out, installer.HostProfile())
		return err
	}
	input := &io.LimitedReader{R: in, N: 16<<20 + 1}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	var evidence installer.CapacityEvidence
	if err := decoder.Decode(&evidence); err != nil || decoder.Decode(new(any)) != io.EOF || input.N <= 0 {
		return installationProblem("Capacity evidence is invalid.", "Provide one frozen capacity evidence JSON object on standard input, at most 16 MiB.")
	}
	advice := installer.AdviseCapacity(evidence)
	if r.text("output") == "json" {
		return json.NewEncoder(out).Encode(struct {
			Advice installer.CapacityAdvice `json:"advice"`
			Host   []string                 `json:"host"`
		}{advice, installer.ReviewHost(evidence.Host)})
	}
	for _, line := range append(installer.CapacityLines(advice), installer.ReviewHost(evidence.Host)...) {
		if _, err := fmt.Fprintln(out, safeDisplay(line)); err != nil {
			return err
		}
	}
	return nil
}

func (s *installationSession) nativeChoices() error {
	for _, choice := range []struct {
		name   string
		decode func(string) (map[string]any, error)
	}{{"resources", installer.ResourceChoices}, {"run-policy", installer.RunPolicyChoices}, {"run-priorities", installer.RunPriorityChoices}} {
		if s.request.text(choice.name) == "" {
			continue
		}
		values, err := choice.decode(s.request.text(choice.name))
		if err != nil {
			return err
		}
		if s.choices.Values == nil {
			s.choices.Values = map[string]any{}
		}
		for name, value := range values {
			s.choices.Values[name] = value
		}
	}
	return nil
}
