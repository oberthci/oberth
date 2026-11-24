package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func addressSession(t *testing.T, host string) (installationSession, installer.Release) {
	t.Helper()
	engine, _ := offlineInstallEngine(t, t.TempDir())
	engine.Cluster.Config.Host = "https://" + host + ":6443"
	release, err := installer.LocalRelease("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	s := installationSession{engine: engine, choices: installer.Choices{Context: "chosen"}, request: installationRequest{name: "install", flags: installationFlags("install")}}
	return s, release
}

func TestInstallationAddressRefusesUnspecifiedWithoutTerminal(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "[::]"} {
		s, release := addressSession(t, host)
		var out bytes.Buffer
		plan, err := s.prepareReachableInstallation(t.Context(), release, &out)
		if err == nil || !strings.Contains(err.Error(), "Run oberth install with --url and --ssh-port clients can reach.") || out.Len() != 0 || plan != nil {
			t.Fatal("unspecified host did not refuse before review", err, out.String())
		}
	}
}

func TestInstallationAddressPromptPreservesApproval(t *testing.T) {
	s, release := addressSession(t, "[::]")
	s.input = bufio.NewScanner(strings.NewReader("https://ci.example:34443\n34022\napproval\n"))
	var out bytes.Buffer
	if err := s.askAddress(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	plan, err := s.prepareInstallation(t.Context(), release, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if !strings.Contains(strings.Join(plan.Review(), "\n"), "https://ci.example:34443") || s.choices.SSHPort != 34022 || !s.input.Scan() || s.input.Text() != "approval" {
		t.Fatal("address prompt lost choice or approval")
	}
}

func TestInstallationAddressReviewKeepsLoopback(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "[::1]", "[::1%25lo]", "localhost", "localhost.", "LOCALHOST."} {
		s, release := addressSession(t, host)
		s.choices.URL = "https://" + host + ":30443"
		var out bytes.Buffer
		plan, err := s.prepareReachableInstallation(t.Context(), release, &out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(plan.Review(), "\n"), "Loopback address: only this machine can reach it.") {
			t.Fatal(plan.Review())
		}
		plan.Close()
	}
}

func TestInstallationAddressNoninteractiveModesNeverPrompt(t *testing.T) {
	for _, args := range [][]string{{"--yes"}, {"--dry-run"}, {"--output=json"}, {"--dry-run", "--output=json"}} {
		s, release := addressSession(t, "0.0.0.0")
		s.interactivePrompt = true
		s.input = bufio.NewScanner(strings.NewReader("https://ci.example\n34022\ny\n"))
		if err := s.request.flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		plan, err := s.prepareReachableInstallation(t.Context(), release, &out)
		if plan != nil || err == nil || !strings.Contains(err.Error(), "Run oberth install with --url and --ssh-port clients can reach.") || out.Len() != 0 {
			t.Fatal(args, err, out.String())
		}
		if !s.input.Scan() || s.input.Text() != "https://ci.example" {
			t.Fatal("unattended mode consumed input", args)
		}
	}
}

func TestInstallationAddressInteractiveRepairIncludesSSH(t *testing.T) {
	s, release := addressSession(t, "0x0")
	s.interactivePrompt = true
	s.input = bufio.NewScanner(strings.NewReader("https://ci.example:36443\n36022\ny\n"))
	var out bytes.Buffer
	plan, err := s.prepareReachableInstallation(t.Context(), release, &out)
	if err != nil {
		t.Fatal(err, out.String())
	}
	defer plan.Close()
	review := strings.Join(plan.Review(), "\n")
	if !strings.Contains(review, "https://ci.example:36443") || !strings.Contains(review, "ci.example:36022") || !strings.Contains(out.String(), "SSH port clients can reach [node port]:") {
		t.Fatal(review, out.String())
	}
	if !s.input.Scan() || s.input.Text() != "y" {
		t.Fatal("repair consumed approval")
	}
}

func TestInstallationAddressSSHPromptDefaultAndRefusals(t *testing.T) {
	for _, answer := range []string{"", "n", "0", "65536"} {
		s, _ := addressSession(t, "localhost")
		s.choices.SSHNodePort = 31022
		s.input = bufio.NewScanner(strings.NewReader("https://ci.example\n" + answer + "\ny\n"))
		var out bytes.Buffer
		err := s.askAddress(t.Context(), &out)
		if answer == "" {
			if err != nil || s.choices.SSHPort != 0 {
				t.Fatal("default SSH port lost", err, s.choices.SSHPort)
			}
		} else if err == nil || !strings.Contains(err.Error(), "--ssh-port") || strings.Contains(err.Error(), "strconv") {
			t.Fatal("invalid SSH port accepted or parser exposed", answer, err)
		}
		if !s.input.Scan() || s.input.Text() != "y" {
			t.Fatal("SSH prompt consumed approval", answer)
		}
	}
}

func TestInstallationAddressBlankSSHFollowsRepairedNodePort(t *testing.T) {
	s, release := nodePortSession(t, false)
	s.engine.Cluster.Config.Host = "https://0.0.0.0:6443"
	s.input = bufio.NewScanner(strings.NewReader("https://ci.example:31443\n\n31443\n31022\napproval\n"))
	var out bytes.Buffer
	plan, err := s.prepareReachableInstallation(t.Context(), release, &out)
	if err != nil {
		t.Fatal(err, out.String())
	}
	defer plan.Close()
	if s.choices.SSHPort != 0 || !strings.Contains(strings.Join(plan.Review(), "\n"), "SSH: ci.example:31022 -> node port 31022") {
		t.Fatal("blank SSH answer did not follow repaired node port", plan.Review())
	}
	for _, prompt := range []string{"SSH port clients can reach [node port]:", "Choose --https-node-port", "Choose --ssh-node-port"} {
		if !strings.Contains(out.String(), prompt) {
			t.Fatal("missing repair prompt", prompt, out.String())
		}
	}
	if !s.input.Scan() || s.input.Text() != "approval" {
		t.Fatal("repair consumed approval")
	}
}

func TestInstallationAddressBlankSSHKeepsExplicitPort(t *testing.T) {
	s, _ := addressSession(t, "localhost")
	s.choices.SSHPort = 32022
	s.input = bufio.NewScanner(strings.NewReader("\n"))
	var out bytes.Buffer
	if err := s.askReachableSSHPort(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if s.choices.SSHPort != 32022 || !strings.Contains(out.String(), "[32022]") {
		t.Fatal("blank SSH answer changed explicit port", s.choices.SSHPort, out.String())
	}
}
