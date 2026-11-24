package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/setuptui"
	"golang.org/x/term"
)

func (s *installationSession) prepareReachableInstallation(ctx context.Context, release installer.Release, out io.Writer) (*installer.Plan, error) {
	plan, err := s.prepareInstallation(ctx, release, out)
	var problem *installer.Problem
	if !errors.As(err, &problem) || (problem.Code != "address" && problem.Code != "loopback-url") {
		return plan, err
	}
	if !s.interactivePrompt || s.request.enabled("yes") || s.request.enabled("dry-run") || s.request.text("output") == "json" {
		return nil, err
	}
	if err = s.askAddress(ctx, out); err != nil {
		return nil, err
	}
	return s.prepareInstallation(ctx, release, out)
}

func (s *installationSession) askAddress(ctx context.Context, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "Server URL clients can reach (https://host:port):"); err != nil {
		return err
	}
	if s.input == nil || !s.input.Scan() {
		return installer.NewProblem("address", nil, "")
	}
	s.choices.URL = strings.TrimSpace(s.input.Text())
	if s.choices.URL == "" {
		return installer.NewProblem("address", nil, "")
	}
	return s.askReachableSSHPort(ctx, out)
}

func installationTerminal(in io.Reader) bool {
	terminal, ok := in.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(terminal.Fd()))
}

// reviewTerminal answers the review on in and sizes its pages by in's
// terminal; a terminal whose size cannot be read counts as 24 rows of 80
// columns, and input that is not a terminal is not paged.
func reviewTerminal(in io.Reader, scanner *bufio.Scanner, out io.Writer) setuptui.Terminal {
	t := setuptui.Terminal{In: scanner, Out: out}
	terminal, ok := in.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(terminal.Fd())) {
		return t
	}
	columns, rows, err := term.GetSize(int(terminal.Fd()))
	if err != nil || rows <= 0 || columns <= 0 {
		columns, rows = 80, 24
	}
	t.Rows, t.Columns = rows, columns
	return t
}

func (s *installationSession) askReachableSSHPort(ctx context.Context, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	defaultPort := "node port"
	if s.choices.SSHPort != 0 {
		defaultPort = strconv.Itoa(s.choices.SSHPort)
	}
	if _, err := fmt.Fprintf(out, "SSH port clients can reach [%s]:\n", defaultPort); err != nil {
		return err
	}
	if !s.input.Scan() {
		return installer.NewProblem("ssh-port", nil, "")
	}
	answer := strings.TrimSpace(s.input.Text())
	if answer == "" {
		return nil
	}
	port, err := strconv.Atoi(answer)
	if err != nil || port < 1 || port > 65535 {
		return installer.NewProblem("ssh-port", nil, "")
	}
	s.choices.SSHPort = port
	return nil
}
