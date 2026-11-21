package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestNodePortConflictRefusesWithoutTerminal(t *testing.T) {
	for _, flag := range []string{"https-node-port", "ssh-node-port"} {
		for _, pipe := range []bool{false, true} {
			s, release := nodePortSession(t, false)
			if flag == "ssh-node-port" {
				s.choices.HTTPSNodePort = 31443
			}
			input := nodePortInput(t, pipe)
			if err := s.prompt(t.Context(), input, io.Discard); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			plan, err := s.prepareReachableInstallation(t.Context(), release, &out)
			if err == nil || !strings.Contains(err.Error(), "--"+flag) || out.Len() != 0 || plan != nil {
				t.Fatal("nonterminal conflict prompted or succeeded", flag, pipe, err, out.String())
			}
			if !s.input.Scan() || s.input.Text() != "n" {
				t.Fatal("nonterminal conflict consumed input", flag, pipe)
			}
		}
	}
}

func nodePortInput(t *testing.T, pipe bool) io.Reader {
	t.Helper()
	if !pipe {
		return strings.NewReader("n\napproval\n")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	if _, err = write.WriteString("n\napproval\n"); err != nil {
		t.Fatal(err)
	}
	if err = write.Close(); err != nil {
		t.Fatal(err)
	}
	return read
}

func TestNodePortPromptKeepsFlagRemedyForInvalidInput(t *testing.T) {
	for _, flag := range []string{"https-node-port", "ssh-node-port"} {
		for _, answer := range []string{"n", "0", "65536", "999999999999999999999999"} {
			assertInvalidNodePortAnswer(t, flag, answer)
		}
	}
}

func assertInvalidNodePortAnswer(t *testing.T, flag, answer string) {
	t.Helper()
	s := installationSession{interactivePrompt: true, choices: installer.Defaults(), input: bufio.NewScanner(strings.NewReader(answer + "\napproval\n"))}
	before := s.choices
	conflict := installer.NewProblem(flag, nil, "Service: existing/front")
	var out bytes.Buffer
	err := s.askNodePort(t.Context(), &out, conflict)
	if !errors.Is(err, conflict) || s.choices.HTTPSNodePort != before.HTTPSNodePort || s.choices.SSHNodePort != before.SSHNodePort {
		t.Fatal("invalid answer lost flag remedy or changed choices", flag, answer, err)
	}
	if !strings.Contains(out.String(), "Choose --"+flag) || !s.input.Scan() || s.input.Text() != "approval" {
		t.Fatal("terminal replacement was not consumed exactly once", out.String())
	}
	diagnostic := &bytes.Buffer{}
	app := application{diagnostic: diagnostic}
	if app.failed(installationFailure(err)) != 1 {
		t.Fatal("invalid answer succeeded")
	}
	for _, required := range []string{"--" + flag, conflict.Remedy} {
		if !strings.Contains(diagnostic.String(), required) {
			t.Fatal("missing actionable refusal", diagnostic.String())
		}
	}
	if strings.Contains(diagnostic.String(), "strconv") || strings.Contains(diagnostic.String(), "parsing") {
		t.Fatal("parser text reached the operator", diagnostic.String())
	}
}

func TestInstallationResultNamesCredentialsOnce(t *testing.T) {
	for _, length := range []int{8, 80, 180} {
		dir := filepath.Join("/profiles", strings.Repeat("a", length))
		profile := installer.Profile{AdminCert: filepath.Join(dir, "admin.crt"), AdminKey: filepath.Join(dir, "admin.key")}
		var out bytes.Buffer
		if err := showInstallation(&out, installer.Result{Profile: profile}); err != nil {
			t.Fatal(err)
		}
		assertInstallationCredentialHeadings(t, out.String())
		whole := strings.Join(strings.Fields(out.String()), "")
		for _, path := range []string{profile.AdminCert, profile.AdminKey} {
			if strings.Count(whole, path) != 1 {
				t.Fatal("credential path lost or repeated", out.String())
			}
		}
	}
}

func assertInstallationCredentialHeadings(t *testing.T, output string) {
	t.Helper()
	for _, heading := range []string{"Administrator certificate:", "Administrator key:"} {
		if strings.Count(output, heading) != 1 {
			t.Fatal("credential heading lost or repeated", output)
		}
	}
}
