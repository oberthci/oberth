package main

import (
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"net/url"
	"os/exec"
	"slices"
	"strings"
)

func gitBindingAddress(repo string) string {
	return (&url.URL{Scheme: "file", Path: "/" + repo + ".git"}).String()
}

func (s *flowSession) checkGitBinding(repo, command string) error {
	address := gitBindingAddress(repo)
	if err := s.checkGitBindingScope(command); err != nil {
		return err
	}
	options := []string{"-c", "remote.oberth-binding-preflight.url=" + address}
	effective, err := s.git(append(slices.Clone(options), "remote", "-v")...)
	if err != nil {
		return err
	}
	urls := bindingURLs(effective)
	if len(urls) != 2 {
		return publicerr.New(publicerr.InvalidInput, "Git remote addresses are unavailable.", "Unset remote.oberth-binding-preflight.url and remote.oberth-binding-preflight.pushurl, then rerun "+command+".", nil)
	}
	for i, effective := range urls {
		if effective != address {
			setting, direction := "url.*.insteadOf", "fetch"
			if i == 1 {
				setting, direction = "url.*.pushInsteadOf", "push"
			}
			return publicerr.New(publicerr.InvalidInput, "Git rewrites the Oberth "+direction+" address.", "Keep "+setting+" compatible with the Oberth file address, then rerun "+command+".", nil)
		}
	}
	return s.checkGitFilePolicy(options, command)
}

func (s *flowSession) checkGitBindingScope(command string) error {
	output, err := s.git("config", "--null", "--show-scope", "--get-regexp", `^remote\.oberth\.(url|pushurl)$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		return err
	}
	fields := strings.Split(output, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] != "local" {
			return publicerr.New(publicerr.InvalidInput, "Git inherits Oberth remote addresses.", "Unset remote.oberth.url and remote.oberth.pushurl outside this checkout, then rerun "+command+".", nil)
		}
	}
	return nil
}

func (s *flowSession) checkGitFilePolicy(options []string, command string) error {
	directory, err := s.git("rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	// Git reads only this checkout's refs; the configured installation helper is not run.
	pack := "sh -c " + shellArgument("git upload-pack --strict "+shellArgument(directory)) + " --"
	query := []string{"ls-remote", "--upload-pack=" + pack, "oberth-binding-preflight", "refs/oberth-binding-preflight"}
	if _, err = s.git(append(slices.Clone(options), query...)...); err != nil {
		return publicerr.New(publicerr.InvalidInput, "Git blocks the Oberth file transport.", "Allow file; check protocol.file.allow, protocol.allow, GIT_ALLOW_PROTOCOL, GIT_PROTOCOL_FROM_USER; then rerun "+command+".", err)
	}
	return nil
}

// Git lists its resolved fetch address first, then its push addresses.
func bindingURLs(output string) []string {
	var urls []string
	for _, line := range strings.Split(output, "\n") {
		name, value, ok := strings.Cut(line, "\t")
		if !ok || name != "oberth-binding-preflight" {
			continue
		}
		end := strings.LastIndex(value, " (")
		if end < 0 {
			return nil
		}
		urls = append(urls, value[:end])
	}
	return urls
}
