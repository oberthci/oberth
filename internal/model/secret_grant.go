package model

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SecretAccessKey identifies an exact repository, workflow, step and public path.
type SecretAccessKey struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
	Path     string `json:"secret"`
}

var grantKeyPathPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// Validate refuses characters which could escape an OpenBao policy path.
func (key SecretAccessKey) Validate() error {
	if strings.TrimSpace(key.Repo) == "" || strings.TrimSpace(key.Step) == "" || strings.TrimSpace(key.Path) == "" {
		return errors.New("repo, workflow, step and secret path are required")
	}
	if !grantKeyPathPattern.MatchString(key.Repo) {
		return fmt.Errorf("repo %q contains characters outside [A-Za-z0-9._/-]", key.Repo)
	}
	if key.Step != "*" && !grantKeyPathPattern.MatchString(key.Step) {
		return errors.New("step must be * or contain only [A-Za-z0-9._/-]")
	}
	if !grantKeyPathPattern.MatchString(key.Path) {
		return fmt.Errorf("secret path %q contains characters outside [A-Za-z0-9._/-]", key.Path)
	}
	for _, path := range []string{key.Repo, key.Path} {
		for _, segment := range strings.Split(path, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return errors.New("repository and secret paths require clean, non-reserved segments")
			}
		}
	}
	return ValidateWorkflowFile(key.Workflow)
}
