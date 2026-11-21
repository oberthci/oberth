package main

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallationAddressAcceptsSSHPortBounds(t *testing.T) {
	for _, port := range []int{1, 65535} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			s := installationSession{input: bufio.NewScanner(strings.NewReader("https://ci.example\n" + strconv.Itoa(port) + "\napproval\n"))}
			var out bytes.Buffer
			if err := s.askAddress(t.Context(), &out); err != nil || s.choices.SSHPort != port || s.choices.URL != "https://ci.example" {
				t.Fatal("valid reachable SSH port refused", s.choices, err)
			}
			if !s.input.Scan() || s.input.Text() != "approval" {
				t.Fatal("port prompt consumed approval")
			}
		})
	}
}

func TestNodePortPromptAcceptsBoundsWithoutChangingOtherPort(t *testing.T) {
	for _, flag := range []string{"https-node-port", "ssh-node-port"} {
		for _, port := range []int{0, 1, 65535, 65536} {
			t.Run(flag+"/"+strconv.Itoa(port), func(t *testing.T) {
				nodePortAnswerCase(t, flag, port)
			})
		}
	}
}

func nodePortAnswerCase(t *testing.T, flag string, port int) {
	t.Helper()
	before := installer.Choices{HTTPSNodePort: 31443, SSHNodePort: 31022}
	s := installationSession{choices: before, interactivePrompt: true, input: bufio.NewScanner(strings.NewReader(strconv.Itoa(port) + "\napproval\n"))}
	problem := installer.NewProblem(flag, nil, "Service: existing/front")
	var out bytes.Buffer
	err := s.askNodePort(t.Context(), &out, problem)
	want := before
	if port == 1 || port == 65535 {
		if err != nil {
			t.Fatal("valid port refused", err)
		}
		if flag == "https-node-port" {
			want.HTTPSNodePort = port
		} else {
			want.SSHNodePort = port
		}
	} else if !errors.Is(err, problem) {
		t.Fatal("invalid port lost the original conflict", err)
	}
	if s.choices.HTTPSNodePort != want.HTTPSNodePort || s.choices.SSHNodePort != want.SSHNodePort {
		t.Fatal("prompt changed the wrong port", s.choices)
	}
	if !s.input.Scan() || s.input.Text() != "approval" {
		t.Fatal("port prompt consumed approval")
	}
}
