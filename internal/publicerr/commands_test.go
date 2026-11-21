package publicerr

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCommandsQuoteEveryValueAsOneShellWord(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{UnarchiveCommand("origin/team/it's"), `oberth repo unarchive 'origin/team/it'\''s'`},
		{PromoteCommand("origin/team/widget", "abc12345"), "oberth promote 'abc12345' --repo 'origin/team/widget'"},
		{IssueDeleteCommand(42), "oberth issue delete 42"},
		{ReceiveRetryCommand("origin/team/widget"), "oberth receive outbox retry --repo 'origin/team/widget'"},
		{RepoAddCommand("ssh://git@forge/team/widget.git", ""), "oberth repo add 'ssh://git@forge/team/widget.git'"},
		{RepoAddCommand("ssh://git@forge/team/widget.git", "origin"), "oberth repo add 'ssh://git@forge/team/widget.git' --upstream 'origin'"},
		{RepoAddTrustCommand("ssh://forge/w.git", "hosts", "origin", true), "oberth repo add 'ssh://forge/w.git' --known-hosts 'hosts' --expected-host-fingerprint VERIFIED_FINGERPRINT --upstream 'origin' --repin"},
		{CheckCommand("<repo>", ".oberth/build.yaml"), "oberth check --repo '<repo>' --local '.oberth/build.yaml'"},
		{SecretStoreCheckCommand(), "oberth secretstore check"},
		{AccessAllowCommand("", Grant{Repo: "r", File: "f", Step: "s", Path: "p"}), "oberth access allow 'r' 'f' 's' 'p'"},
		{AccessAllowCommand("identity", Grant{Repo: "r", File: "f", Step: "s", Path: "identity", Audience: "a"}), "oberth access allow --kind identity --audience 'a' 'r' 'f' 's' 'identity'"},
		{ClaimAllowCommand("r", "cache", true), "oberth access allow --kind pvc --tier release 'r' 'cache'"},
		{SecretPutCommand(Grant{Repo: "r", File: "f", Step: "s", Path: "p"}), "oberth secret put --repo 'r' --file 'f' --step 's' 'p' < <value-file>"},
		{CapabilityGrantCommand("r", `{"namespace":"ci"}`), `oberth workflow capability grant --repo 'r' --capability '{"namespace":"ci"}' --reason '<reason>'`},
		{SourcePinCommand("origin/team/widget", "origin/team/it's"), `oberth upgrade --source-pin 'origin/team/widget=origin/team/it'\''s'`},
	} {
		if tc.got != tc.want {
			t.Errorf("got  %s\nwant %s", tc.got, tc.want)
		}
	}
}

func TestNamesThatReadAsFlagsTakeTheirFlagFormOrPrintNothing(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{AccessAllowCommand("", Grant{Repo: "r", File: "f", Step: "--step=*", Path: "p"}), "oberth access allow --step='--step=*' 'r' 'f' 'p'"},
		{AccessAllowCommand("", Grant{Repo: "--repo=x", File: "--kind", Step: "pvc", Path: "-k"}), "oberth access allow --repo='--repo=x' --workflow='--kind' --secret='-k' 'pvc'"},
		{ClaimAllowCommand("r", "-c", false), "oberth access allow --kind pvc --tier ci --claim='-c' 'r'"},
		{SecretPutCommand(Grant{Repo: "r", File: "f", Step: "-s", Path: "p"}), "oberth secret put --repo 'r' --file 'f' --step='-s' 'p' < <value-file>"},
		{ReceiveRetryCommand("--json"), "oberth receive outbox retry --repo='--json'"},
		{SecretPutCommand(Grant{Repo: "r", File: "f", Step: "s", Path: "-p"}), ""},
		{UnarchiveCommand("-r"), ""},
		{PromoteCommand("r", "-abc"), ""},
		{IssueDeleteCommand(-1), ""},
		{RepoAddCommand("-u", "origin"), ""},
		{SourcePinCommand("-x/team/widget", "origin/team/source"), ""},
		{SourcePinCommand("origin/team/widget", "--source-pin=origin/team/widget"), ""},
		{SourcePinCommand("origin/team/widget=origin/team/other", "origin/team/source"), ""},
		{SourcePinCommand("origin/team/widget", "origin/team/a=b"), ""},
		{SourcePinCommand("", "origin/team/source"), ""},
	} {
		if tc.got != tc.want {
			t.Errorf("got  %q\nwant %q", tc.got, tc.want)
		}
	}
	if New(Forbidden, "Refused.", "Ask an administrator.", nil).WithCommand(UnarchiveCommand("-r")).Command() != "" {
		t.Fatal("a refusal carried an unsafe command")
	}
}

func TestAdministratorCommandTravelsAfterContextAndKeepsBounds(t *testing.T) {
	plain := New(Forbidden, "The repository is archived.", "Ask an administrator to unarchive the repository.", nil)
	raw, _ := json.Marshal(plain)
	if strings.Contains(string(raw), "Administrator") || strings.Contains(plain.Error(), "Administrator") || plain.Command() != "" {
		t.Fatal("a refusal without a command printed one", string(raw))
	}
	e := New(Forbidden, "The repository is archived.", "Ask an administrator to unarchive the repository.", nil)
	e.Context = []string{"Repository: origin/team/widget"}
	e = e.WithCommand(UnarchiveCommand("origin/team/widget"))
	var wire struct {
		Context        []string
		Reason, Remedy string
	}
	raw, _ = json.Marshal(e)
	if json.Unmarshal(raw, &wire) != nil || !slices.Equal(wire.Context, []string{"Repository: origin/team/widget", "Administrator: oberth repo unarchive 'origin/team/widget'"}) {
		t.Fatal("command not carried after the context", string(raw))
	}
	if wire.Reason != e.Reason() || wire.Remedy != e.Remedy() || len(e.Context) != 1 {
		t.Fatal("command changed the bounded reason, remedy or authored context", wire)
	}
	if !strings.HasSuffix(e.Error(), "\nAdministrator: oberth repo unarchive 'origin/team/widget'") {
		t.Fatal("error text lost the command", e.Error())
	}
	var none *Error
	if none.Command() != "" || none.publicContext() != nil {
		t.Fatal("nil refusal has a command")
	}
}

func TestClassifyKeepsOneCommandAndListsDifferentOnes(t *testing.T) {
	archived := func(repo string) *Error {
		return New(Forbidden, "The repository is archived.", "Ask an administrator to unarchive the repository.", nil).WithCommand(UnarchiveCommand(repo))
	}
	same := Classify(errors.Join(archived("origin/team/a"), archived("origin/team/a")))
	if same.Command() != UnarchiveCommand("origin/team/a") || len(same.Context) != 0 || !strings.Contains(same.Error(), "Administrator: ") {
		t.Fatal("one shared command was not kept", same.Command(), same.Context)
	}
	both := Classify(errors.Join(archived("origin/team/a"), archived("origin/team/b")))
	if both.Command() != "" || !slices.Equal(both.Context, []string{AdministratorLine(UnarchiveCommand("origin/team/a")), AdministratorLine(UnarchiveCommand("origin/team/b"))}) {
		t.Fatal("different commands were merged or lost", both.Command(), both.Context)
	}
	other := Classify(errors.Join(New(Unavailable, "Retry later.", "Retry later.", nil), archived("origin/team/a")))
	if other.Code() != Unavailable || other.Command() != "" {
		t.Fatal("a command of another code leaked into the classified refusal", other)
	}
}
