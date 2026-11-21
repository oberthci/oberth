package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
	chartfake "github.com/oberthci/oberth/internal/installer/fake"
	apps "k8s.io/api/apps/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInstallCommandCannotTakeOverDefaultsFromFreshHome(t *testing.T) {
	for _, yes := range []bool{false, true} {
		for _, ports := range []bool{false, true} {
			t.Run(map[bool]string{false: "interactive", true: "yes"}[yes]+map[bool]string{true: "/ports"}[ports], func(t *testing.T) { installTakeoverCommand(t, yes, ports) })
		}
	}
}

func installTakeoverCommand(t *testing.T, yes, ports bool) {
	engine, charts := installedOwnerEngine(t)
	before := ownerCommandSnapshot(t, engine, charts)
	home := filepath.Join(t.TempDir(), "fresh-home")
	t.Setenv("HOME", home)
	a, out, diagnostic := fixture(t, fake.Bindings())
	a.base = filepath.Join(home, ".config", "oberth")
	a.install = func(ctx context.Context, r installationRequest, in io.Reader, out io.Writer) error {
		s, err := a.openInstallation(r)
		if err != nil {
			return err
		}
		credentials, err := installer.OpenConfigDir(filepath.Join(a.base, s.name))
		if err != nil {
			return err
		}
		defer credentials.Close()
		copied := *engine
		copied.Credentials = credentials
		s.engine, s.input = &copied, bufio.NewScanner(in)
		return s.dispatch(ctx, in, out)
	}
	args := []string{"install", "--context", "chosen", "--chart", "../../charts/oberth"}
	if yes {
		args = append(args, "--yes")
	}
	if ports {
		args = append(args, "--namespace", "oberth", "--https-node-port", "31443", "--ssh-node-port", "31022")
	}
	if code := a.run(t.Context(), args); code != 1 || !strings.Contains(diagnostic.String(), "does not own") || strings.Contains(out.String(), "Apply?") {
		t.Fatal("takeover command was reviewed/applied", code, out.String(), diagnostic.String())
	}
	if !strings.Contains(strings.Join(strings.Fields(diagnostic.String()), " "), "choose another --namespace and unused --https-node-port and --ssh-node-port") {
		t.Fatal("takeover remedy repeats supplied flags instead of changing values", diagnostic.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("takeover refusal created HOME/profile/key", err)
	}
	if !bytes.Equal(before, ownerCommandSnapshot(t, engine, charts)) {
		t.Fatal("takeover command changed first installation")
	}
}

func installedOwnerEngine(t *testing.T) (*installer.Engine, *chartfake.Chart) {
	t.Helper()
	engine, charts := offlineInstallEngine(t, t.TempDir())
	release, err := installer.LocalRelease("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.Prepare(t.Context(), release, installer.Choices{Context: "chosen"})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if _, err := engine.Apply(t.Context(), plan, plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Cluster.Client.AppsV1().Deployments("oberth").Create(t.Context(), &apps.Deployment{ObjectMeta: meta.ObjectMeta{Name: "oberth", Namespace: "oberth", Generation: 1}}, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return engine, charts
}

func ownerCommandSnapshot(t *testing.T, e *installer.Engine, charts *chartfake.Chart) []byte {
	t.Helper()
	secret, err := e.Cluster.Client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := e.Cluster.Client.AppsV1().Deployments("oberth").Get(t.Context(), "oberth", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal([]any{charts.History, secret, deployment})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
