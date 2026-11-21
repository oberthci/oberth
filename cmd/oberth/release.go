package main

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/oberthci/oberth/internal/apicontract"
)

func (s *flowSession) releasePreflight(repo, sha, tag string) error {
	var result apicontract.RunCheckResult
	if err := s.invoke("run_check", apicontract.RunCheckArgs{Repo: repo, Ref: sha, ReleaseTag: tag}, &result); err != nil {
		return err
	}
	if !result.OK {
		for _, finding := range result.Findings {
			if _, err := fmt.Fprintln(s.app.diagnostic, finding.Reason+"\n"+finding.Remedy); err != nil {
				return err
			}
			if finding.Command != "" {
				if _, err := fmt.Fprintln(s.app.diagnostic, finding.Command); err != nil {
					return err
				}
			}
		}
		return flowError("release_checks", nil)
	}
	if result.ReleaseTag != tag || result.CommitSHA == nil || *result.CommitSHA != sha {
		return flowError("contract", nil)
	}
	return nil
}
func (s *flowSession) ensureReleaseTag(tag, sha string) error {
	ref := "refs/tags/" + tag
	current, err := s.git("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err == nil {
		if current != sha {
			return flowError("release_tag", nil)
		}
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return err
	}
	_, err = s.git("tag", "-a", "-m", tag, "--", tag, sha)
	return err
}
