package main

import (
	"bufio"
	"bytes"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecondInstallationFlagsAndProfile(t *testing.T) {
	if defaultInstallationProfile("", "second") != "" {
		t.Fatal("profile name was chosen before the context prompt")
	}
	flags := installationFlags("install")
	if err := flags.Parse([]string{"--context=chosen", "--namespace=second", "--pipeline-namespace=jobs", "--https-node-port=31443", "--ssh-node-port=31022"}); err != nil {
		t.Fatal(err)
	}
	r := installationRequest{name: "install", flags: flags}
	c := r.choices()
	if c.Namespace != "second" || c.PipelineNamespace != "jobs" || c.HTTPSNodePort != 31443 || c.SSHNodePort != 31022 || installationProfileName(r) != "chosen-second" {
		t.Fatal(c, installationProfileName(r))
	}
	if err := flags.Set("profile", "explicit"); err != nil {
		t.Fatal(err)
	}
	if installationProfileName(r) != "explicit" {
		t.Fatal("explicit profile ignored")
	}
}

func TestInteractiveNodePortConflictAsksAndPreparesAgain(t *testing.T) {
	for _, unattended := range []bool{false, true} {
		t.Run(strconv.FormatBool(unattended), func(t *testing.T) { nodePortConflictCase(t, unattended) })
	}
}

func nodePortConflictCase(t *testing.T, unattended bool) {
	s, release := nodePortSession(t, unattended)
	var out bytes.Buffer
	plan, err := s.prepareInstallation(t.Context(), release, &out)
	if unattended {
		if err == nil || !strings.Contains(err.Error(), "--https-node-port") || out.Len() != 0 {
			t.Fatal("unattended conflict prompted or succeeded", err, out.String())
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if !strings.Contains(out.String(), "Choose --https-node-port") || !strings.Contains(out.String(), "Choose --ssh-node-port") || !s.input.Scan() || s.input.Text() != "approval" {
		t.Fatal("conflict retry lost approval input", out.String())
	}
	assertPortConflictDetails(t, out.String())
	if !strings.Contains(strings.Join(plan.Review(), "\n"), "31443 -> node port 31443") {
		t.Fatal("new public default did not follow node port", plan.Review())
	}
}

func nodePortSession(t *testing.T, unattended bool) (installationSession, installer.Release) {
	engine, _ := offlineInstallEngine(t, t.TempDir())
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "front", Namespace: "existing"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443, NodePort: 30443}, {Port: 22, NodePort: 30022}}}}
	if _, err := engine.Cluster.Client.CoreV1().Services("existing").Create(t.Context(), service, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	flags := installationFlags("install")
	if err := flags.Set("yes", strconv.FormatBool(unattended)); err != nil {
		t.Fatal(err)
	}
	release, err := installer.LocalRelease("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	s := installationSession{interactivePrompt: true, engine: engine, choices: installer.Choices{Context: "chosen", Namespace: "second"}, request: installationRequest{name: "install", flags: flags}, input: bufio.NewScanner(strings.NewReader("31443\n31022\napproval\n"))}
	return s, release
}

func assertPortConflictDetails(t *testing.T, output string) {
	t.Helper()
	for _, flag := range []string{"https-node-port", "ssh-node-port"} {
		prompt := strings.Index(output, "Choose --"+flag)
		before := output[:prompt]
		for _, detail := range []string{"Service: existing/front", "Choose an unused node port and review the installation again."} {
			if !strings.Contains(before, detail) {
				t.Fatal("conflict prompt omitted reason, remedy or holder", output)
			}
		}
	}
}

func TestNodePortConflictDoesNotPromptOnPipedInput(t *testing.T) {
	s, release := nodePortSession(t, false)
	s.interactivePrompt = false
	var out bytes.Buffer
	plan, err := s.prepareInstallation(t.Context(), release, &out)
	if plan != nil {
		plan.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "--https-node-port") || out.Len() != 0 {
		t.Fatal("piped input prompted", err, out.String())
	}
	if !s.input.Scan() || s.input.Text() != "31443" {
		t.Fatal("piped input consumed")
	}
}
func TestNodePortPromptRejectsTextWithoutParserInternals(t *testing.T) {
	s, release := nodePortSession(t, false)
	s.input = bufio.NewScanner(strings.NewReader("not-a-number\n"))
	var out bytes.Buffer
	plan, err := s.prepareInstallation(t.Context(), release, &out)
	if plan != nil {
		plan.Close()
	}
	if err == nil || strings.Contains(err.Error(), "strconv") || strings.Contains(err.Error(), "not-a-number") || !strings.Contains(err.Error(), "--https-node-port") {
		t.Fatal("unclear node port refusal", err)
	}
}
func TestInstallationCredentialHeadingsAppearOnceForLongPaths(t *testing.T) {
	profile := installer.Profile{AdminCert: "/private/" + strings.Repeat("directory/", 15) + "admin-long.crt", AdminKey: "/private/" + strings.Repeat("directory/", 15) + "admin-long.key"}
	var out bytes.Buffer
	if err := showInstallation(&out, installer.Result{Profile: profile}); err != nil {
		t.Fatal(err)
	}
	for _, heading := range []string{"Administrator certificate:", "Administrator key:"} {
		if strings.Count(out.String(), heading) != 1 {
			t.Fatal("repeated credential heading", out.String())
		}
	}
	values := []string{profile.AdminCert, profile.AdminKey, filepath.Join(filepath.Dir(profile.AdminCert), "installation.json")}
	for _, value := range values {
		if !strings.Contains(out.String(), "\n"+value+"\n") {
			t.Fatal("copyable path is not whole on its own line", value)
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if !slices.Contains(values, line) && len([]rune(line)) > 80 {
			t.Fatal("prose exceeded display width", line)
		}
	}
}
