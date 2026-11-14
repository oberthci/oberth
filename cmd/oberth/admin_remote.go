package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func adminCLIFlags(name string, args []string) (*flag.FlagSet, *client.ConnectionFlags, func() ([]string, bool, error)) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	connection := registerTransportFlags(flags)
	return flags, connection, func() ([]string, bool, error) { return parseOperatorFlags(flags, args) }
}
func writeAdminResult(output io.Writer, value any) error {
	return json.NewEncoder(output).Encode(value)
}
func callAdminCLI(ctx context.Context, connection *client.ConnectionFlags, action string, args any, output io.Writer) error {
	remote, err := remoteClient(ctx, connection)
	if err != nil {
		return err
	}
	var result any
	if err := remote.Admin(ctx, action, args, &result); err != nil {
		return err
	}
	return writeAdminResult(output, result)
}
func existingAdminTool(ctx context.Context, connection *client.ConnectionFlags, name string, args any) (json.RawMessage, error) {
	resolved, err := client.ResolveConnection(*connection)
	if err != nil {
		return nil, err
	}
	cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: resolved.MCPURL, CAFile: resolved.CACert, ClientCert: resolved.ClientCert, ClientKey: resolved.ClientKey, Token: resolved.Token, TokenCommand: resolved.TokenCommand}}
	return (remoteRunCanceller{config: cfg}).call(ctx, name, args)
}
func callExistingAdminTool(ctx context.Context, connection *client.ConnectionFlags, name string, args any, output io.Writer) error {
	raw, err := existingAdminTool(ctx, connection, name, args)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, strings.TrimSpace(string(raw)))
	return err
}

func runUpstream(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(output, "Usage: oberth upstream add|list|remove|provide-key")
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("%w: upstream add|list|remove|provide-key", errUsage)
	}
	action := args[0]
	flags, connection, parse := adminCLIFlags("upstream "+action, args[1:])
	var request api.UpstreamAddRequest
	flags.BoolVar(&request.DedicatedKey, "dedicated-key", false, "use a dedicated upstream SSH identity")
	flags.BoolVar(&request.NoWait, "no-wait", false, "return the deploy key without waiting for forge authorization")
	flags.StringVar(&request.ExpectedHostFingerprint, "expected-host-fingerprint", "", "authoritative SHA256 SSH host fingerprint")
	flags.BoolVar(&request.Repin, "repin", false, "replace this endpoint's pins using the same trust rules")
	flags.StringVar(&request.CheckRepository, "check-repo", "", "repository to probe through the upstream")
	yes := flags.Bool("yes", false, "approve generation of the upstream identity")
	check := flags.Bool("check", false, "probe upstream authentication and repository access")
	asJSON := flags.Bool("json", false, "emit structured JSON")
	keyFile := flags.String("key-file", "", "owner-only local upstream private key to import")
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth upstream "+action+" [flags]")
	}
	switch action {
	case "add":
		if len(positional) != 2 {
			return fmt.Errorf("%w: upstream add <name> <ssh-url>", errUsage)
		}
		request.Name, request.BaseURL = positional[0], strings.TrimSuffix(positional[1], "/")
		if err := gitcache.ValidateUpstreamName(request.Name); err != nil {
			return err
		}
		if !*yes && *keyFile == "" {
			return errors.New("review the upstream and pass --yes, or --key-file for an existing identity")
		}
		remote, err := remoteClient(ctx, connection)
		if err != nil {
			return err
		}
		if *keyFile != "" {
			material, err := readAdminLocalKey(*keyFile)
			if err != nil {
				return err
			}
			defer clear(material)
			if err := remote.Admin(ctx, "upstream-provide-key", api.UpstreamKeyRequest{Name: request.Name, DedicatedKey: request.DedicatedKey, PrivateKey: string(material)}, nil); err != nil {
				return err
			}
		}
		var result struct {
			Identity api.UpstreamIdentity `json:"identity"`
			Upstream model.Upstream       `json:"upstream"`
		}
		if err := remote.Admin(ctx, "upstream-add", request, &result); err != nil {
			return err
		}
		if *asJSON {
			return writeAdminResult(output, result)
		}
		if result.Identity.PublicKey != "" {
			if _, err := fmt.Fprintln(output, result.Identity.PublicKey); err != nil {
				return err
			}
		}
		state := "registered"
		if result.Identity.Pending {
			state = "registered; deploy key pending"
		}
		if result.Identity.RestartRequired {
			if _, err := fmt.Fprintln(output, "Restart the Oberth server Deployment to activate the dedicated upstream key."); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(output, "Upstream %s %s.\n", request.Name, state)
		return err
	case "provide-key":
		if len(positional) != 1 {
			return fmt.Errorf("%w: upstream provide-key <name> (key on stdin)", errUsage)
		}
		body, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
		if err != nil || len(body) > 64<<10 {
			return errors.New("cannot read bounded upstream key")
		}
		defer clear(body)
		return callAdminCLI(ctx, connection, "upstream-provide-key", api.UpstreamKeyRequest{Name: positional[0], DedicatedKey: request.DedicatedKey, PrivateKey: string(body)}, output)
	case "list":
		if len(positional) != 0 {
			return fmt.Errorf("%w: upstream list takes no arguments", errUsage)
		}
		remote, err := remoteClient(ctx, connection)
		if err != nil {
			return err
		}
		var result struct {
			Upstreams []struct {
				Upstream model.Upstream `json:"upstream"`
				Auth     string         `json:"auth"`
			} `json:"upstreams"`
		}
		if err := remote.Admin(ctx, "upstream-list", map[string]bool{"check": *check}, &result); err != nil {
			return err
		}
		if *asJSON {
			return writeAdminResult(output, result)
		}
		writer := tabwriter.NewWriter(output, 0, 0, 3, ' ', 0)
		if _, err := fmt.Fprintln(writer, "NAME\tKIND\tURL\tKEY NAME\tAUTH"); err != nil {
			return err
		}
		for _, row := range result.Upstreams {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", row.Upstream.Name, row.Upstream.Kind, row.Upstream.BaseURL, row.Upstream.KeyName, row.Auth); err != nil {
				return err
			}
		}
		return writer.Flush()
	case "remove":
		if len(positional) != 1 || !*yes {
			return fmt.Errorf("%w: upstream remove --yes <name>", errUsage)
		}
		return callAdminCLI(ctx, connection, "upstream-remove", map[string]string{"name": positional[0]}, output)
	default:
		return fmt.Errorf("%w: upstream add|list|remove|provide-key", errUsage)
	}
}
func readAdminLocalKey(path string) ([]byte, error) {
	info, err := os.Lstat(path) //nolint:gosec // G703: operator-selected local private key; owner-only regular-file check below.
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("upstream key must be an owner-only regular file")
	}
	return readBoundedFile(path, 64<<10)
}
func runRepo(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(output, "Usage: oberth repo add|verify|remove|archive|unarchive")
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("%w: repo add|verify|remove|archive|unarchive", errUsage)
	}
	if args[0] == "archive" || args[0] == "unarchive" {
		return runRepoArchive(ctx, args[0], args[1:], output)
	}
	flags, connection, parse := adminCLIFlags("repo "+args[0], args[1:])
	branch := flags.String("default-branch", "main", "initial default branch; the first push confirms it")
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth repo "+args[0]+" [flags]")
	}
	switch args[0] {
	case "add":
		if len(positional) != 2 {
			return fmt.Errorf("%w: repo add <repository> <upstream>", errUsage)
		}
		return callAdminCLI(ctx, connection, "repo-add", api.RepoAddRequest{Repo: positional[0], Upstream: positional[1], DefaultBranch: *branch}, output)
	case "verify":
		if len(positional) != 1 {
			return fmt.Errorf("%w: repo verify <repository>", errUsage)
		}
		return callAdminCLI(ctx, connection, "repo-verify", map[string]string{"repo": positional[0]}, output)
	case "remove":
		if len(positional) != 1 {
			return fmt.Errorf("%w: repo remove <repository>", errUsage)
		}
		return callExistingAdminTool(ctx, connection, "repo_remove", map[string]string{"repo": positional[0]}, output)
	default:
		return fmt.Errorf("%w: repo add|verify|remove|archive|unarchive", errUsage)
	}
}
func runAccess(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(output, "Usage: oberth access list|allow|revoke")
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("%w: access list|allow|revoke", errUsage)
	}
	flags, connection, parse := adminCLIFlags("access "+args[0], args[1:])
	repo := flags.String("repo", "", "repository filter")
	revoked := flags.Bool("revoked", false, "include revoked grants")
	flags.Bool("json", false, "emit structured JSON")
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth access "+args[0]+" [flags]")
	}
	switch args[0] {
	case "list":
		if len(positional) != 0 {
			return fmt.Errorf("%w: access list takes no positional arguments", errUsage)
		}
		return callExistingAdminTool(ctx, connection, "access_list", map[string]any{"repo": *repo, "revoked": *revoked}, output)
	case "allow", "revoke":
		if len(positional) != 4 {
			return fmt.Errorf("%w: access %s <repo> <workflow-file> <step> <secret>", errUsage, args[0])
		}
		return callExistingAdminTool(ctx, connection, "access_"+args[0], map[string]string{"repo": positional[0], "workflow": positional[1], "step": positional[2], "secret": positional[3]}, output)
	default:
		return fmt.Errorf("%w: access list|allow|revoke", errUsage)
	}
}
func runUplink(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(output, "Usage: oberth uplink add|invite|cert|list|remove|rotate|scope")
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("%w: uplink add|invite|cert|list|remove|rotate|scope", errUsage)
	}
	if args[0] == "invite" {
		return runUplinkInvite(ctx, args[1:], output)
	}
	if args[0] == "cert" {
		return runUplinkCert(ctx, args[1:], input, output)
	}
	flags, connection, parse := adminCLIFlags("uplink "+args[0], args[1:])
	var enrollment api.UplinkAddRequest
	flags.BoolVar(&enrollment.Admin, "admin", false, "grant administrative authority")
	flags.BoolVar(&enrollment.Bearer, "bearer", false, "issue a bearer credential alongside a certificate")
	flags.StringVar(&enrollment.TTL, "ttl", "", "credential lifetime within the server policy")
	csrFile := flags.String("csr", "", "local public certificate signing request")
	flags.Func("scope", "upstream scope (repeatable or comma-separated)", func(value string) error {
		enrollment.Scope = append(enrollment.Scope, splitScope(value)...)
		return nil
	})

	fingerprint := flags.String("fingerprint", "", "SSH fingerprint to disambiguate an identity")
	replacement := flags.String("replacement-fingerprint", "", "already-enrolled replacement SSH fingerprint")
	clearScope := flags.Bool("clear", false, "clear the repository scope")
	var scopes []string
	flags.Func("set", "upstream scope (repeatable or comma-separated)", func(value string) error { scopes = append(scopes, splitScope(value)...); return nil })
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth uplink "+args[0]+" [flags]")
	}
	switch args[0] {
	case "add":
		if len(positional) != 2 {
			return fmt.Errorf("%w: uplink add [flags] <public-key-file|-> <identity>", errUsage)
		}
		var public []byte
		if positional[0] == "-" {
			public, err = io.ReadAll(io.LimitReader(input, (64<<10)+1))
		} else {
			public, err = readBoundedFile(positional[0], 64<<10)
		}
		if err != nil {
			return err
		}
		if len(public) > 64<<10 {
			return errors.New("public key exceeds 64 KiB")
		}
		enrollment.Identity = positional[1]
		enrollment.SSHPublicKey = string(public)
		if *csrFile != "" {
			raw, e := readBoundedFile(*csrFile, 64<<10)
			if e != nil {
				return e
			}
			if len(raw) > 64<<10 {
				return errors.New("CSR exceeds 64 KiB")
			}
			enrollment.CSR = string(raw)
		}
		return callAdminCLI(ctx, connection, "uplink-add", enrollment, output)
	case "list":
		if len(positional) != 0 {
			return fmt.Errorf("%w: uplink list takes no arguments", errUsage)
		}
		return callExistingAdminTool(ctx, connection, "uplink_list", map[string]any{}, output)
	case "remove", "rotate":
		if len(positional) != 1 {
			return fmt.Errorf("%w: uplink %s <identity>", errUsage, args[0])
		}
		name := "uplink_revoke"
		arguments := map[string]string{"identity": positional[0], "fingerprint": *fingerprint}
		if args[0] == "rotate" {
			name = "uplink_rotate"
			arguments["replacement_fingerprint"] = *replacement
		}
		return callExistingAdminTool(ctx, connection, name, arguments, output)
	case "scope":
		if len(positional) != 1 || (*clearScope == (len(scopes) > 0)) {
			return fmt.Errorf("%w: uplink scope (--set UPSTREAM | --clear) <identity>", errUsage)
		}
		if *clearScope {
			scopes = []string{}
		}
		return callAdminCLI(ctx, connection, "uplink-scope", map[string]any{"identity": positional[0], "fingerprint": *fingerprint, "scope": scopes}, output)
	default:
		return fmt.Errorf("%w: uplink add|invite|cert|list|remove|rotate|scope; installation alone creates the first admin", errUsage)
	}
}

func runRemoteSecretStoreVerify(ctx context.Context, args []string, output io.Writer) error {
	flags, connection, parse := adminCLIFlags("secretstore verify", args)
	release := flags.Bool("release-tier", false, "verify the release ServiceAccount trust chain")
	repo := flags.String("repo", "", "canonical repository whose identity to verify")
	tier := flags.String("tier", "release", "repository identity tier: ci or release")
	timeout := flags.Duration("timeout", 45*time.Second, "overall verification deadline")
	keys := flags.Bool("keys", false, "report field names, never values")
	var paths, expects stringList
	flags.Var(&paths, "path", "secret path to verify (repeatable)")
	flags.Var(&expects, "expect", "expected field names (repeatable)")
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth secretstore verify [flags] [paths]")
	}
	if *timeout <= 0 || *timeout > 2*time.Minute {
		return fmt.Errorf("%w: verification timeout must be positive and at most two minutes", errUsage)
	}
	paths = append(paths, positional...)
	return callExistingAdminTool(ctx, connection, "secretstore_verify", map[string]any{"release_tier": *release, "repo": *repo, "tier": *tier, "timeout_s": int((*timeout + time.Second - 1) / time.Second), "keys": *keys, "paths": []string(paths), "expect": []string(expects)}, output)
}
