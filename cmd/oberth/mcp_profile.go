package main

import (
	"context"
	"flag"
	"io"

	"github.com/oberthci/oberth/internal/client"
)

func mcpFlags() *flag.FlagSet {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.String("profile", "", "Joined uplink profile; defaults only to the checkout binding.")
	return flags
}
func (a application) mcpProfile(ctx context.Context, args []string) (string, error) {
	flags := mcpFlags()
	if err := flags.Parse(args); err != nil {
		return "", usageProblems{"Use oberth mcp [--profile <uplink-profile>]."}
	}
	if flags.NArg() != 0 {
		return "", usageProblems{"mcp accepts only --profile <uplink-profile>."}
	}
	profile := flags.Lookup("profile").Value.String()
	explicit := false
	flags.Visit(func(*flag.Flag) { explicit = true })
	if explicit {
		return profile, nil
	}
	session := flowSession{ctx: ctx, app: a}
	profile, err := session.git("config", "--local", "--get", "oberth.uplinkProfile")
	if err != nil {
		return "", nil
	}
	return profile, nil
}
func (a application) runMCP(ctx context.Context, profile string) int {
	closePipes, err := a.bridgePipes()
	if err != nil {
		return a.failed(err)
	}
	defer closePipes()
	err = client.Bridge(ctx, a.in, a.out, client.BridgeConfig{Config: a.config, Base: a.base, Profile: profile, Display: Display, Diagnostic: a.diagnostic})
	if err != nil {
		return a.failed(err)
	}
	return 0
}
