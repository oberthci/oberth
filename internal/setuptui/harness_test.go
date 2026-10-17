package setuptui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/installer"
)

// harness drives the wizard the way the Bubble Tea runtime would: every
// command is executed and its message fed back, synchronously.
type harness struct {
	t    *testing.T
	w    *wizard
	quit bool
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	queue := []tea.Msg{msg}
	for n := 0; len(queue) > 0; n++ {
		if n > 5000 {
			h.t.Fatal("message loop did not settle")
		}
		m := queue[0]
		queue = queue[1:]
		switch mm := m.(type) {
		case nil:
			continue
		case tea.BatchMsg:
			for _, c := range mm {
				if c != nil {
					queue = append(queue, c())
				}
			}
			continue
		case tea.QuitMsg:
			h.quit = true
			continue
		}
		_, cmd := h.w.Update(m)
		if cmd != nil {
			queue = append(queue, cmd())
		}
	}
}

func (h *harness) key(k tea.Key)     { h.t.Helper(); h.send(tea.KeyPressMsg(k)) }
func (h *harness) enter()            { h.t.Helper(); h.key(tea.Key{Code: tea.KeyEnter}) }
func (h *harness) esc()              { h.t.Helper(); h.key(tea.Key{Code: tea.KeyEscape}) }
func (h *harness) tab()              { h.t.Helper(); h.key(tea.Key{Code: tea.KeyTab}) }
func (h *harness) right()            { h.t.Helper(); h.key(tea.Key{Code: tea.KeyRight}) }
func (h *harness) down()             { h.t.Helper(); h.key(tea.Key{Code: tea.KeyDown}) }
func (h *harness) home()             { h.t.Helper(); h.key(tea.Key{Code: tea.KeyHome}) }
func (h *harness) letter(s string)   { h.t.Helper(); h.key(tea.Key{Code: []rune(s)[0], Text: s}) }
func (h *harness) typeText(s string) { h.t.Helper(); h.send(tea.PasteMsg{Content: s}) }

// clear empties the focused text field.
func (h *harness) clear() {
	h.t.Helper()
	h.key(tea.Key{Code: tea.KeyEnd})
	h.key(tea.Key{Code: 'u', Mod: tea.ModCtrl})
}

// view renders the current screen as plain text.
func (h *harness) view() string { return ansi.Strip(h.w.render()) }

func (h *harness) question() string {
	h.t.Helper()
	if q, ok := h.w.screen.(*questionScreen); ok {
		return q.q.id
	}
	return ""
}

func (h *harness) expectQuestion(id string) {
	h.t.Helper()
	if got := h.question(); got != id {
		h.t.Fatalf("on screen %q (%T), want question %q\n%s", got, h.w.screen, id, h.view())
	}
}

// testHost is an isolated home with a kubeconfig, one SSH public key and
// stubbed probes. kind is not installed, like the k3s host of issue #047.
type testHost struct {
	home       string
	kubeconfig string
	pubKey     string
}

func newTestHost(t *testing.T) *testHost {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("USER", "dima")
	kubeconfig := filepath.Join(home, "kubeconfig")
	writeKubeconfig(t, kubeconfig, "default", "default", "staging")
	t.Setenv("KUBECONFIG", kubeconfig)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	pubKey := filepath.Join(home, ".ssh", "id_ed25519.pub")
	if err := os.WriteFile(pubKey, ssh.MarshalAuthorizedKey(sshPub), 0o600); err != nil {
		t.Fatal(err)
	}

	stubKindTools(t, false)
	stubClusterProbe(t, nil)
	previousDial := dialPort
	dialPort = func(context.Context, string) error { return nil }
	t.Cleanup(func() { dialPort = previousDial })
	return &testHost{home: home, kubeconfig: kubeconfig, pubKey: pubKey}
}

func writeKubeconfig(t *testing.T, path, current string, contexts ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\ncurrent-context: " + current + "\nclusters:\n")
	for _, c := range contexts {
		b.WriteString("- name: " + c + "\n  cluster:\n    server: https://127.0.0.1:6443\n")
	}
	b.WriteString("contexts:\n")
	for _, c := range contexts {
		b.WriteString("- name: " + c + "\n  context:\n    cluster: " + c + "\n    user: u\n")
	}
	b.WriteString("users:\n- name: u\n  user:\n    token: x\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stubKindTools makes kind creation available or unavailable regardless of
// what the test host has installed.
func stubKindTools(t *testing.T, available bool) {
	t.Helper()
	previous := kindLookPath
	kindLookPath = func(name string) (string, error) {
		if available {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { kindLookPath = previous })
}

// stubClusterProbe answers every cluster probe like a reachable k3s node,
// or with err.
func stubClusterProbe(t *testing.T, err error) *[]string {
	t.Helper()
	var calls []string
	previous := clusterProbe
	clusterProbe = func(contextName, path string) clusterInfoMsg {
		calls = append(calls, contextName+"|"+path)
		if err != nil {
			return clusterInfoMsg{context: contextName, err: err}
		}
		return clusterInfoMsg{context: contextName, server: "https://127.0.0.1:6443", version: "v1.31.4+k3s1",
			engine: "k3s", isLocal: true, nodeCount: 1, cores: 8, nodeName: "Tuxbox", nodeIP: "192.168.86.193"}
	}
	t.Cleanup(func() { clusterProbe = previous })
	return &calls
}

// fakeInstaller records the Config it was given and plays a script.
type fakeInstaller struct {
	mu     sync.Mutex
	calls  int
	cfgs   []installer.Config
	script func(call int, deps installer.InstallDeps) error
}

func stubInstaller(t *testing.T, script func(call int, deps installer.InstallDeps) error) *fakeInstaller {
	t.Helper()
	f := &fakeInstaller{script: script}
	previous := executeInstaller
	executeInstaller = func(_ context.Context, cfg installer.Config, deps installer.InstallDeps) error {
		f.mu.Lock()
		f.calls++
		call := f.calls
		f.cfgs = append(f.cfgs, cfg)
		f.mu.Unlock()
		return f.script(call, deps)
	}
	t.Cleanup(func() { executeInstaller = previous })
	return f
}

// reportSteps reports the named installer steps done.
func reportSteps(deps installer.InstallDeps, steps ...string) {
	for _, s := range steps {
		deps.StepProgressSink(s, "done")
	}
}

// fakeKubectl answers the commands Verify runs.
type fakeKubectl struct {
	mu         sync.Mutex
	commands   []string
	registered bool
	ready      bool
	acceptAdd  bool
	pubKey     string
}

func stubKubectl(t *testing.T, k *fakeKubectl) *fakeKubectl {
	t.Helper()
	previous := runKubectl
	runKubectl = func(_ context.Context, args ...string) ([]byte, error) {
		k.mu.Lock()
		defer k.mu.Unlock()
		line := strings.Join(args, " ")
		k.commands = append(k.commands, line)
		switch {
		case strings.Contains(line, "upstream list"):
			if k.registered {
				return []byte(upstreamCheckTable("github", "ssh://git@github.com/acme", "ok")), nil
			}
			return []byte(upstreamCheckTable()), nil
		case strings.Contains(line, "upstream add"):
			if k.acceptAdd {
				k.registered, k.ready = true, true
				return []byte("ok\n"), nil
			}
			return []byte("ssh: unable to authenticate, attempted methods [none publickey]\n"), exec.ErrNotFound
		case strings.Contains(line, "id_ed25519.pub"):
			return []byte(k.pubKey), nil
		case strings.Contains(line, "get deployment"):
			if k.ready {
				return []byte("1/1 ghcr.io/oberthci/oberth:v0.17.28"), nil
			}
			return []byte("/1 ghcr.io/oberthci/oberth:v0.17.28"), nil
		}
		return nil, exec.ErrNotFound
	}
	t.Cleanup(func() { runKubectl = previous })
	return k
}

// upstreamCheckTable renders `oberth upstream list --check` output, aligned
// like the server's tabwriter: name, URL and AUTH triples.
func upstreamCheckTable(rows ...string) string {
	line := func(name, kind, url, keyName, fp, auth string) string {
		return fmt.Sprintf("%-10s %-6s %-36s %-10s %-20s %s\n", name, kind, url, keyName, fp, auth)
	}
	out := line("NAME", "KIND", "URL", "KEY NAME", "KEY FINGERPRINT", "AUTH")
	for i := 0; i+2 < len(rows); i += 3 {
		out += line(rows[i], "ssh", rows[i+1], "-", "SHA256:x", rows[i+2])
	}
	return out
}

func testPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(key))
}

func writeWorkKeyFixture(t *testing.T, passphrase string) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(private, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(private, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "work-key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newHarness starts a wizard the way Run does, at 80×24.
func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	st := newWizardState(opts)
	if _, err := loadSetupState(st); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, w: newWizard(opts, st)}
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	return h
}

// answerAllSteps walks steps 1–6 with defaults, organization "acme".
func (h *harness) answerAllSteps() {
	h.t.Helper()
	h.enter() // menu: open step 1
	h.expectQuestion("cluster.target")
	h.enter()
	h.expectQuestion("cluster.namespaces")
	h.enter()
	h.expectQuestion("cluster.network")
	h.enter()
	h.expectQuestion("store.mode")
	h.enter()
	h.expectQuestion("store.anchoring")
	h.enter()
	h.expectQuestion("tls.names")
	h.enter()
	h.expectQuestion("tls.client")
	h.enter()
	h.expectQuestion("forge.type")
	h.enter()
	h.expectQuestion("forge.org")
	h.typeText("acme")
	h.enter()
	h.expectQuestion("forge.key")
	h.enter()
	h.expectQuestion("forge.more")
	h.enter()
	h.expectQuestion("uplink.identity")
	h.enter()
	h.expectQuestion("uplink.key")
	h.enter()
	h.expectQuestion("client.access")
	h.enter()
	if _, ok := h.w.screen.(*reviewScreen); !ok {
		h.t.Fatalf("after the last question the review must open, got %T\n%s", h.w.screen, h.view())
	}
}

func ansiStrip(s string) string { return ansi.Strip(s) }
