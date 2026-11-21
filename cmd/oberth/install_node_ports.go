package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/installer"
)

func (s *installationSession) prepareInstallation(ctx context.Context, release installer.Release, out io.Writer) (*installer.Plan, error) {
	for {
		plan, err := s.engine.Prepare(ctx, release, s.choices)
		var problem *installer.Problem
		if !errors.As(err, &problem) || (problem.Code != "https-node-port" && problem.Code != "ssh-node-port") || s.request.enabled("yes") || s.request.enabled("dry-run") {
			return plan, err
		}
		if err = s.askNodePort(ctx, out, problem); err != nil {
			return nil, err
		}
	}
}

func (s *installationSession) askNodePort(ctx context.Context, out io.Writer, problem *installer.Problem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.input == nil || !s.interactivePrompt {
		return problem
	}
	if _, err := fmt.Fprintf(out, "%s\n%s\n%s\nChoose --%s: ", problem.Reason, problem.Remedy, problem.Detail, problem.Code); err != nil {
		return err
	}
	if !s.input.Scan() {
		return errors.Join(problem, s.input.Err())
	}
	port, err := strconv.Atoi(strings.TrimSpace(s.input.Text()))
	if err != nil || port < 1 || port > 65535 {
		return problem
	}
	if problem.Code == "https-node-port" {
		s.choices.HTTPSNodePort = port
	} else {
		s.choices.SSHNodePort = port
	}
	return nil
}
