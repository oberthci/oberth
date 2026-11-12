package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/invite"
)

func runUplinkInvite(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("uplink invite", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	connectionFlags := registerTransportFlags(flags)
	out := flags.String("out", "", "new 0600 invite file outside a git worktree; no credential is printed")
	expires := flags.Duration("expires", invite.DefaultLifetime, "single-use invite lifetime (maximum 168h)")
	var scopes []string
	flags.Func("scope", "upstream name (repeatable or comma-separated)", func(value string) error { scopes = append(scopes, splitScope(value)...); return nil })
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			printFlagUsage(flags)
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 1 || *out == "" {
		return fmt.Errorf("%w: uplink invite <identity@host> --out <private-file> [--scope upstream] [--expires 24h]", errUsage)
	}
	if err := invite.ValidateIdentity(flags.Arg(0)); err != nil {
		return err
	}
	if *expires <= 0 || *expires > invite.MaximumLifetime {
		return errors.New("invite lifetime must be positive and at most 168h")
	}
	connection, err := client.ResolveConnection(*connectionFlags)
	if err != nil {
		return err
	}
	if err := invite.ValidateURL(connection.MCPURL); err != nil {
		return err
	}
	path, err := installer.CheckSecretsFilePath(*out)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: checked new operator-selected path outside a worktree.
	if err != nil {
		return errors.New("cannot reserve the private invite file")
	}
	defer func() { _ = file.Close() }()
	if err := file.Chmod(0600); err != nil {
		return errors.New("cannot restrict the private invite file")
	}
	secret, digest, err := invite.NewSecret()
	if err != nil {
		return err
	}
	response, err := mcpRequest(ctx, mcpFlagResult{mcpConfig: mcpConfig{URL: connection.MCPURL, CAFile: connection.CACert, ClientCert: connection.ClientCert, ClientKey: connection.ClientKey, Token: connection.Token, TokenCommand: connection.TokenCommand}}, "tools/call", toolsCallParams{Name: "uplink_invite", Arguments: mustInviteJSON(map[string]any{"identity": flags.Arg(0), "server_url": connection.MCPURL, "secret_digest": digest, "scope": scopes, "expires": expires.String()})})
	if err != nil {
		return errors.New("invite creation failed; the private output file was kept and must not be shared")
	}
	var reply struct {
		IsError bool           `json:"isError"`
		Binding invite.Binding `json:"structuredContent"`
	}
	if response.Error != nil || json.Unmarshal(response.Result, &reply) != nil || reply.IsError || reply.Binding.Identity != flags.Arg(0) || reply.Binding.ServerURL != connection.MCPURL || invite.ValidatePin(reply.Binding.CAPin) != nil || !reply.Binding.ExpiresAt.After(time.Now()) {
		return errors.New("invite creation was refused; the private output file was kept and must not be shared")
	}
	body, err := json.MarshalIndent(invite.File{Binding: reply.Binding, Secret: secret}, "", "  ") //nolint:gosec // G117: serialized only into the reserved owner-only handoff file, never a response or log.
	if err != nil {
		return err
	}
	defer clear(body)
	if _, err = file.Write(append(body, '\n')); err != nil {
		return errors.New("cannot save invite; the output file was kept")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot sync invite; the output file was kept")
	}
	_, err = fmt.Fprintf(output, "Single-use invite saved: %s (expires %s). Transfer this file privately; do not paste or read it in an agent session.\n", path, reply.Binding.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}
func mustInviteJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }
