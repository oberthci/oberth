package installer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
)

// secretLocation names one credential in the host's secret store. The three
// fields are the same identity expressed for the three stores this file knows
// how to drive, so a caller names the credential once and every platform
// branch below knows where to put it and how to read it back.
type secretLocation struct {
	// service is the Keychain service name on macOS and the `service`
	// attribute for secret-tool elsewhere. It is also what the retrieval
	// command the installer prints contains, so it is part of the interface.
	service string
	// label is the human-readable description a store that carries one shows.
	label string
	// passPath is the entry name inside `pass`.
	passPath string
}

var (
	// openBaoRootTokenLocation and openBaoUnsealKeyLocation are the two
	// credentials `bao operator init` produces exactly once.
	//
	// They were printed and never stored, on the theory that an installer
	// should not decide where an operator keeps a root token. In practice the
	// print scrolls off and the store is lost: an initialized OpenBao whose
	// unseal key nobody has is a permanently sealed OpenBao. The
	// install writes every other configuration file it needs, so this is the
	// same custody decision applied to the credentials that cannot be
	// reissued at all.
	openBaoRootTokenLocation = secretLocation{ // #nosec G101 -- the NAME of a secret-store entry, not credential material.
		service:  "oberth-openbao-root",
		label:    "Oberth OpenBao root token",
		passPath: "oberth/openbao-root",
	}
	openBaoUnsealKeyLocation = secretLocation{ // #nosec G101 -- the NAME of a secret-store entry, not credential material.
		service:  "oberth-openbao-unseal",
		label:    "Oberth OpenBao unseal key",
		passPath: "oberth/openbao-unseal",
	}
)

// errNoSecretStore is returned when the host offers no supported store. The
// caller falls back to printing the credential, which is the old behavior.
var errNoSecretStore = errors.New("no supported secret store found (install secret-tool or pass)")

// errSecretNotStored is returned by readStoredSecret when the store answers
// that it holds no such entry, as opposed to failing to answer at all.
var errSecretNotStored = errors.New("no such entry in the secret store")

// storeSecret writes one credential to the host's secret store.
//
// The value is written to the store's standard input wherever the store reads
// standard input; the macOS branch is the documented exception below.
func storeSecret(ctx context.Context, deps Deps, where secretLocation, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("no value to store")
	}
	lookPath := deps.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	switch runtime.GOOS {
	case "darwin":
		// The account name comes from the user database rather than $USER:
		// the environment variable is caller-controlled, and it is about to
		// become an argument to a command.
		account, err := currentAccount()
		if err != nil {
			return err
		}
		// The value is inline rather than on standard input.
		//
		// `security -w` with no value reads from the controlling terminal, not
		// from stdin, so piping the secret to it leaves the installer sitting
		// at a Keychain prompt asking the operator for a password the install
		// already holds -- which is worse than the problem being solved.
		//
		// The cost is that the value is in this command's argv while it runs,
		// readable by another process of the same user. It is a bounded window
		// on a machine where that user already holds the Keychain.
		//
		// -U replaces an existing item rather than adding a second one under
		// the same service, which is what makes a reinstall answer the read
		// with this deployment's credential instead of an older one.
		return runWithSecret(ctx, deps, "",
			"security", "add-generic-password", "-s", where.service, "-a", account, "-U", "-w", value)
	default:
		if _, err := lookPath("secret-tool"); err == nil {
			return runWithSecret(ctx, deps, value,
				"secret-tool", "store", "--label="+where.label, "service", where.service)
		}
		if _, err := lookPath("pass"); err == nil {
			// --echo takes the value from stdin instead of prompting twice.
			return runWithSecret(ctx, deps, value+"\n", "pass", "insert", "--echo", where.passPath)
		}
		return errNoSecretStore
	}
}

// readStoredSecret reads one credential back out of the host's secret store.
func readStoredSecret(ctx context.Context, deps Deps, where secretLocation) (string, error) {
	lookPath := deps.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}

	switch runtime.GOOS {
	case "darwin":
		account, err := currentAccount()
		if err != nil {
			return "", err
		}
		// The account-scoped read first, because that is what storeSecret
		// wrote. The service-only read second, because an entry an operator
		// created by hand before this code existed carries whatever account
		// they typed, and refusing to find it would make the installer's own
		// convention the only one that works.
		out, err := run(ctx, nil, "security", "find-generic-password", "-s", where.service, "-a", account, "-w")
		if err != nil {
			out, err = run(ctx, nil, "security", "find-generic-password", "-s", where.service, "-w")
		}
		if err != nil {
			return "", fmt.Errorf("%w: Keychain service %s", errSecretNotStored, where.service)
		}
		return strings.TrimSpace(string(out)), nil
	default:
		if _, err := lookPath("secret-tool"); err == nil {
			out, err := run(ctx, nil, "secret-tool", "lookup", "service", where.service)
			if err != nil || strings.TrimSpace(string(out)) == "" {
				return "", fmt.Errorf("%w: secret-tool service %s", errSecretNotStored, where.service)
			}
			return strings.TrimSpace(string(out)), nil
		}
		if _, err := lookPath("pass"); err == nil {
			out, err := run(ctx, nil, "pass", "show", where.passPath)
			if err != nil {
				return "", fmt.Errorf("%w: pass entry %s", errSecretNotStored, where.passPath)
			}
			return strings.TrimSpace(firstLine(string(out))), nil
		}
		return "", errNoSecretStore
	}
}

// retrievalCommand is the command an operator runs to read the credential
// back. It is printed instead of the credential itself.
func retrievalCommand(deps Deps, where secretLocation) string {
	lookPath := deps.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	switch runtime.GOOS {
	case "darwin":
		return "security find-generic-password -s " + where.service + " -w"
	default:
		if _, err := lookPath("secret-tool"); err == nil {
			return "secret-tool lookup service " + where.service
		}
		return "pass show " + where.passPath
	}
}

// currentAccount resolves the account name credentials are filed under.
func currentAccount() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve the current user: %w", err)
	}
	return account.Username, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// runWithSecret feeds a credential to a command's standard input.
func runWithSecret(ctx context.Context, deps Deps, secret, name string, args ...string) error {
	run := deps.RunCommand
	if run == nil {
		run = DefaultRunCommand
	}
	// The error names the command but never its arguments or output: on the
	// macOS path the credential is one of those arguments, and a store that
	// fails must not be the thing that prints it.
	if _, err := run(ctx, []byte(secret), name, args...); err != nil {
		return fmt.Errorf("%s could not store the credential", name)
	}
	return nil
}
