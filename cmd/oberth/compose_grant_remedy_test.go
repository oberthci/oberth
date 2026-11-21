//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestComposedIdentityRemedyAdmitsNextPush(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	document := strings.Replace(composedWorkflow, "    container:\n", `    volumes:
    - name: identity
      projected:
        sources:
        - serviceAccountToken: {audience: https://identity.example.test, expirationSeconds: 600, path: token}
    container:
      volumeMounts: [{name: identity, mountPath: /run/identity, readOnly: true}]
`, 1)
	f.git.Write(t, ".oberth/build.yaml", []byte(document))
	f.git.CommitFile(t, "projected identity")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	pushed, err := f.pushResult(t, "refs/heads/identity", "-o", "oberth.run=1")
	if err == nil {
		t.Fatal("ungranted push admitted", pushed)
	}
	var command string
	for _, line := range strings.Split(pushed, "\n") {
		if start := strings.Index(line, "oberth access allow "); start >= 0 {
			command = strings.TrimSpace(line[start:])
		}
	}
	if command == "" {
		t.Fatal("push omitted grant remedy", pushed)
	}
	app, _, diagnostic := f.revokeApplication(t)
	if code := app.run(t.Context(), remedyWords(t, command)[1:]); code != 0 {
		t.Fatal("printed grant failed", command, code, diagnostic.String())
	}
	if pushed, err = f.pushResult(t, "refs/heads/identity", "-o", "oberth.run=1"); err != nil {
		t.Fatal("granted push refused", pushed, err)
	}
	f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
}
