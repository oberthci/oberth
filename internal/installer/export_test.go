package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// IsUncheckableKey reports whether err only means the key could not be
// tested before decryption.
func IsUncheckableKey(err error) bool { return errors.Is(err, errKeyUncheckable) }

// componentTable prints a 3-column Unicode box-drawing table:
//
//	┌──────────────────────────┬──────────────────────────┬────────────┐
//	│ Component                │ Detail                   │ Status     │
//	├──────────────────────────┼──────────────────────────┼────────────┤
//	│ Kubernetes auth          │ secret store             │ ready      │
//	│ Oberth                   │ v0.12.43                 │ deployed   │
//	└──────────────────────────┴──────────────────────────┴────────────┘
//
// Column widths auto-size to fit content, with minimum floors so the
// table never shrinks below 80 display columns. Borders are single-weight
// box-drawing characters. When color is true, borders are dim, the header
// is bold, sensitive rows are highlighted in yellow, and status symbols
// are colorized.
func componentTable(w io.Writer, rows []tableRow, color bool) {
	// Start from minimum widths (header labels + padding already fit).
	cW := tableMinComponent
	dW := tableMinDetail
	sW := tableMinStatus

	// Expand to fit content.
	for _, row := range rows {
		if w := textWidth(row.Name) + 2; w > cW {
			cW = w
		}
		if w := textWidth(row.Detail) + 2; w > dW {
			dW = w
		}
		if w := plainStatusLen(row.Status) + 2; w > sW {
			sW = w
		}
	}

	dim, bold, yellow, reset := "", "", "", ""
	if color {
		dim = "\033[2m"
		bold = "\033[1m"
		yellow = "\033[33m"
		reset = "\033[0m"
	}

	// Border helpers.
	hC := strings.Repeat("─", cW) // ─ repeated
	hD := strings.Repeat("─", dW)
	hS := strings.Repeat("─", sW)

	// Top border: ┌────┬────┬────┐
	_, _ = fmt.Fprintf(w, "%s┌%s┬%s┬%s┐%s\n", dim, hC, hD, hS, reset)

	// Header row — %-*s width is cW-1 (1 leading space + cW-1 content = cW).
	_, _ = fmt.Fprintf(w, "%s│%s%s %-*s%s%s│%s%s %-*s%s%s│%s%s %-*s%s%s│%s\n",
		dim, reset, bold, cW-1, "Component", reset, dim, reset, bold, dW-1, "Detail", reset, dim, reset, bold, sW-1, "Status", reset, dim, reset)

	// Header separator: ├────┼────┼────┤
	_, _ = fmt.Fprintf(w, "%s├%s┼%s┼%s┤%s\n", dim, hC, hD, hS, reset)

	// Data rows.
	for _, row := range rows {
		name := row.Name
		nameDisplay := name

		namePad := cW - 2 - textWidth(name)
		if namePad < 0 {
			namePad = 0
		}

		detailPad := dW - 2 - textWidth(row.Detail)
		if detailPad < 0 {
			detailPad = 0
		}

		statusText := formatStatusText(row.Status, color)
		statusPlainLen := plainStatusLen(row.Status)
		statusPad := sW - 2 - statusPlainLen
		if statusPad < 0 {
			statusPad = 0
		}

		if color && row.Sensitive {
			_, _ = fmt.Fprintf(w, "%s│%s %s%s%s%s %s│%s %s%s%s%s %s│%s %s%s %s│%s\n",
				dim, reset,
				yellow, nameDisplay, reset, strings.Repeat(" ", namePad),
				dim, reset,
				yellow, row.Detail, reset, strings.Repeat(" ", detailPad),
				dim, reset,
				statusText, strings.Repeat(" ", statusPad),
				dim, reset)
		} else {
			_, _ = fmt.Fprintf(w, "%s│%s %s%s %s│%s %s%s %s│%s %s%s %s│%s\n",
				dim, reset,
				nameDisplay, strings.Repeat(" ", namePad),
				dim, reset,
				row.Detail, strings.Repeat(" ", detailPad),
				dim, reset,
				statusText, strings.Repeat(" ", statusPad),
				dim, reset)
		}
	}

	// Bottom border: └────┴────┴────┘
	_, _ = fmt.Fprintf(w, "%s└%s┴%s┴%s┘%s\n", dim, hC, hD, hS, reset)
}

// StartPromptRow prints a partial row (│ component │ promptText) without a
// newline. The caller reads user input, then calls CompleteRow to overwrite
// the partial line with the final completed row.
func (tw *tableWriter) StartPromptRow(component, promptText string) {
	namePad := tw.cW - 2 - textWidth(component)
	if namePad < 0 {
		namePad = 0
	}
	_, _ = fmt.Fprintf(tw.w, "%s│%s %s%s %s│%s %s",
		tw.dim, tw.reset,
		component, strings.Repeat(" ", namePad),
		tw.dim, tw.reset,
		promptText)
}

// CompleteRow replaces the current prompt line with a completed data row.
// When color is enabled, ANSI escapes move the cursor up one line and clear
// it before printing the final row. When color is off (tests, pipes), the
// completed row is printed on the next line.
func (tw *tableWriter) CompleteRow(component, detail, status string, sensitive bool) {
	if tw.color {
		// The user pressed Enter after the prompt, moving the cursor down.
		// Go back up, clear the prompt line, and overwrite it.
		_, _ = fmt.Fprint(tw.w, "\033[1A\r\033[2K")
	}
	tw.writeDataRow(component, detail, status, sensitive)
}

// SetupProductionSecretStore initializes and unseals the standalone OpenBao
// through kubectl exec, prints the root token and unseal key exactly once,
// and configures the secret store with the captured root token. Re-runs
// against an already-initialized store skip init and use BAO_TOKEN when the
// operator supplies it.
func SetupProductionSecretStore(ctx context.Context, cfg Config, deps Deps, openbao OpenBaoResult) (SecretStoreResult, error) {
	color := isColor(deps)

	client, status, err := waitForOpenBaoStatus(ctx, cfg, deps, openbao.Namespace)
	if err != nil {
		return SecretStoreResult{}, err
	}
	if isInMemoryStorage(status) {
		return SecretStoreResult{}, fmt.Errorf("the OpenBao release in namespace %s runs in dev mode (in-memory storage); production mode cannot adopt it. "+
			"Re-run with --upgrade to reinstall it in production mode, or uninstall it first (helm uninstall openbao -n %s). "+
			"(Right after a mode-switch upgrade the dev pod may still be terminating — re-run in a moment.)",
			openbao.Namespace, openbao.Namespace)
	}

	var rootToken string
	switch {
	case !status.Initialized:
		initResult, err := client.operatorInit(ctx)
		if err != nil {
			return SecretStoreResult{}, err
		}
		// Print the credentials BEFORE anything that can still fail: after a
		// successful init they exist nowhere but here, and losing them locks
		// the store permanently.
		printProductionCredentials(deps.Output, initResult, color)
		rootToken = initResult.RootToken
		unsealStatus, err := client.unseal(ctx, initResult.UnsealKeysB64[0])
		if err != nil {
			return SecretStoreResult{}, err
		}
		if unsealStatus.Sealed {
			return SecretStoreResult{}, errors.New("OpenBao is still sealed after submitting the unseal key")
		}
	case status.Sealed:
		return SecretStoreResult{}, fmt.Errorf("OpenBao in namespace %s is initialized but sealed. Run /oberth:unseal or oberth unseal --context <chosen-context> --secrets-file <saved-file> --openbao-namespace %s, then re-run oberth install. See deploy/systemd/README.md for optional TPM2 host-bound recovery", openbao.Namespace, openbao.Namespace)
	default:
		_, _ = fmt.Fprintln(deps.Output, "OpenBao is already initialized and unsealed; skipping operator init (credentials were printed once at first initialization).")
		rootToken = os.Getenv(baoTokenEnvVar)
		if rootToken == "" && isInteractive(deps) && deps.ReadPassword != nil {
			_, _ = fmt.Fprint(deps.Output, "Root token (echo disabled): ")
			tokenBytes, err := deps.ReadPassword()
			_, _ = fmt.Fprintln(deps.Output) // newline after the silent read
			if err != nil {
				return SecretStoreResult{}, fmt.Errorf("read root token: %w", err)
			}
			rootToken = strings.TrimSpace(string(tokenBytes))
		}
		if rootToken == "" {
			_, _ = fmt.Fprintln(deps.Output, "Cannot verify the server PKI configuration: a re-run has no root token available.")
			_, _ = fmt.Fprintln(deps.Output, "To verify or repair the configuration, supply the root token without exposing it in shell history:")
			_, _ = fmt.Fprintln(deps.Output, "")
			_, _ = fmt.Fprintln(deps.Output, "    read -rs BAO_TOKEN && export BAO_TOKEN")
			_, _ = fmt.Fprintln(deps.Output, "    oberth install --install-secretstore")
			_, _ = fmt.Fprintln(deps.Output, "")
			_, _ = fmt.Fprintln(deps.Output, "Keep every install flag unchanged, especially --namespace, --openbao-namespace, --chart-version and --openbao-chart-version.")
			_, _ = fmt.Fprintln(deps.Output, "Or run scripts/setup-secretstore.sh from an already-authenticated bao CLI session.")
			return SecretStoreResult{}, fmt.Errorf("%s is required to verify production server PKI provisioning before Oberth can be enabled", baoTokenEnvVar)
		}
		_, _ = fmt.Fprintf(deps.Output, "Using the root token to verify the secret-store configuration.\n")
	}

	if err := waitForPodReady(ctx, deps, openbao.Namespace, openBaoPodSelector, cfg.Timeout); err != nil {
		return SecretStoreResult{}, fmt.Errorf("wait for OpenBao: %w", err)
	}

	configured, err := ConfigureSecretStore(ctx, cfg, deps, client, rootToken)
	if err != nil {
		return SecretStoreResult{}, fmt.Errorf("configure secret store: %w", err)
	}

	return configured, nil
}

// printProductionCredentials prints the once-only init credentials inside a
// titled credential box. They are deliberately never persisted by the installer.
func printProductionCredentials(w io.Writer, initResult baoInitResult, color bool) {
	_, _ = fmt.Fprintln(w)
	rows := []credentialRow{
		{Label: "Root token", Value: initResult.RootToken},
	}
	for i, key := range initResult.UnsealKeysB64 {
		label := "Unseal key"
		if len(initResult.UnsealKeysB64) > 1 {
			label = fmt.Sprintf("Unseal key %d", i+1)
		}
		rows = append(rows, credentialRow{Label: label, Value: key})
	}
	credentialBox(w,
		"OpenBao credentials (save now, shown once)",
		"",
		rows, color)
	_, _ = fmt.Fprintln(w)
}

// checkValuesPinnedConflicts checks parsed YAML data for leaf keys that
// collide with a pinned upgrade key.
func checkValuesPinnedConflicts(data []byte) error {
	return checkUpgradeValuesPinnedConflicts(data, upgradePinnedKeys)
}
