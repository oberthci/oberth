package model

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var workflowFilePattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ValidateWorkflowFile requires the exact repository-relative workflow path.
// Grants never infer a file or authorize a wildcard file selector.
func ValidateWorkflowFile(file string) error {
	if !workflowFilePattern.MatchString(file) || len(file) > 1024 || path.IsAbs(file) || path.Clean(file) != file || file == "." || file == ".." || strings.HasPrefix(file, "../") || strings.ContainsAny(file, "\\\x00\r\n*?[]") {
		return fmt.Errorf("workflow must be an exact, clean repository-relative file path")
	}
	return nil
}
