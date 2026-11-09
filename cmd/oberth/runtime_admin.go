package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Runtime administration (#1497): both commands call audited MCP tools that
// write server-owned overrides, so neither needs a Helm upgrade or restart.

const admissionCapacityUsage = "Usage: oberth admission capacity <0..64>|clear --reason <reason> [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"Sets the server-owned runtime run ceiling (0 is no ceiling); clear restores the mounted file or boot flag."

func runAdmissionCapacity(ctx context.Context, args []string, output io.Writer) error {
	if wantsHelp(args) {
		_, err := fmt.Fprintln(output, admissionCapacityUsage)
		return err
	}
	flags := flag.NewFlagSet("admission capacity", flag.ContinueOnError)
	reason := flags.String("reason", "", "reason for the capacity change")
	asJSON := flags.Bool("json", false, "emit JSON")
	transport := mcpTransportFlags(flags)
	positional, _, err := parseOperatorFlags(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("%w: %s", errUsage, admissionCapacityUsage)
	}
	arguments := map[string]any{"reason": *reason}
	if positional[0] == "clear" {
		arguments["clear"] = true
	} else {
		jobs, err := strconv.Atoi(positional[0])
		if err != nil || jobs < 0 || jobs > 64 {
			return fmt.Errorf("%w: %s", errUsage, admissionCapacityUsage)
		}
		arguments["max_concurrent_jobs"] = jobs
	}
	config, err := parseMCPFlags("admission capacity", transport(), 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "admission_capacity_config", arguments)
	if err != nil {
		return err
	}
	var result struct {
		Configuration struct {
			MaxConcurrentJobs int    `json:"max_concurrent_jobs"`
			Source            string `json:"source"`
			Error             string `json:"error"`
		} `json:"configuration"`
	}
	if *asJSON || json.Unmarshal(raw, &result) != nil || result.Configuration.Source == "" {
		_, err = fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	line := fmt.Sprintf("admission capacity: %d concurrent workflows, source=%s", result.Configuration.MaxConcurrentJobs, result.Configuration.Source)
	if result.Configuration.Error != "" {
		line += "; last reload error: " + result.Configuration.Error
	}
	_, err = fmt.Fprintln(output, line)
	return err
}

const releaseWIFUsage = "Usage: oberth release-wif list [--json]\n" +
	"       oberth release-wif grant <repo> --service-account <name> --template <leaf>=<role>[,...] --reason <reason> [--json]\n" +
	"       oberth release-wif revoke|reset <repo> --reason <reason> [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"Runtime overrides layered on the Helm argo.releaseWIF default; roles stay Helm configuration."

func runReleaseWIF(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: %s", errUsage, releaseWIFUsage)
	}
	action := args[0]
	if action != "list" && action != "grant" && action != "revoke" && action != "reset" {
		return fmt.Errorf("%w: %s", errUsage, releaseWIFUsage)
	}
	if wantsHelp(args[1:]) {
		_, err := fmt.Fprintln(output, releaseWIFUsage)
		return err
	}
	flags := flag.NewFlagSet("release-wif "+action, flag.ContinueOnError)
	reason := flags.String("reason", "", "reason for the change")
	account := flags.String("service-account", "", "the repository's release ServiceAccount (grant)")
	templates := flags.String("template", "", "comma-separated leaf=role mappings (grant)")
	asJSON := flags.Bool("json", false, "emit JSON")
	transport := mcpTransportFlags(flags)
	positional, _, err := parseOperatorFlags(flags, args[1:])
	if err != nil {
		return err
	}
	arguments := map[string]any{}
	switch {
	case action == "list" && len(positional) == 0 && *reason == "" && *account == "" && *templates == "":
	case action != "list" && len(positional) == 1 && strings.TrimSpace(*reason) != "":
		arguments["repo"], arguments["reason"] = positional[0], *reason
		if action == "grant" {
			mapping := map[string]string{}
			for _, pair := range strings.Split(*templates, ",") {
				leaf, role, ok := strings.Cut(pair, "=")
				if !ok || leaf == "" || role == "" || mapping[leaf] != "" {
					return fmt.Errorf("%w: %s", errUsage, releaseWIFUsage)
				}
				mapping[leaf] = role
			}
			if *account == "" {
				return fmt.Errorf("%w: %s", errUsage, releaseWIFUsage)
			}
			arguments["service_account_name"], arguments["templates"] = *account, mapping
		} else if *account != "" || *templates != "" {
			return fmt.Errorf("%w: %s", errUsage, releaseWIFUsage)
		}
	default:
		return fmt.Errorf("%w: %s", errUsage, releaseWIFUsage)
	}
	config, err := parseMCPFlags("release-wif "+action, transport(), 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "release_wif_"+action, arguments)
	if err != nil {
		return err
	}
	var view struct {
		Enabled   bool                       `json:"enabled"`
		Effective map[string]json.RawMessage `json:"effective"`
		Overrides []struct {
			Repo    string `json:"repo"`
			Revoked bool   `json:"revoked"`
			Actor   string `json:"actor"`
			Reason  string `json:"reason"`
		} `json:"overrides"`
		Inactive []struct {
			Repo  string `json:"repo"`
			Error string `json:"error"`
		} `json:"inactive"`
	}
	if *asJSON || json.Unmarshal(raw, &view) != nil {
		_, err = fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	if !view.Enabled {
		_, err = fmt.Fprintln(output, "release WIF is not configured on this server")
		return err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "effective repositories: %d\n", len(view.Effective))
	for _, override := range view.Overrides {
		state := "granted"
		if override.Revoked {
			state = "revoked"
		}
		fmt.Fprintf(&out, "override %s: %s by %s (%s)\n", override.Repo, state, override.Actor, override.Reason)
	}
	for _, inactive := range view.Inactive {
		fmt.Fprintf(&out, "inactive %s: %s\n", inactive.Repo, inactive.Error)
	}
	_, err = io.WriteString(output, out.String())
	return err
}
