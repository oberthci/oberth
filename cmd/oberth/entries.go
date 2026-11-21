package main

import (
	"context"
	"slices"
)

func (a application) entry(ctx context.Context, args []string) (int, bool) {
	if len(args) > 0 && args[0] == "git-ssh" {
		return a.gitSSH(ctx, args[1:]), true
	}
	if code, ok := a.helpEntry(args); ok {
		return code, true
	}

	if code, ok := a.versionEntry(args); ok {
		return code, true
	}
	if code, ok := a.mcpEntry(ctx, args); ok {
		return code, true
	}
	if code, ok := a.installEntry(ctx, args); ok {
		return code, true
	}
	if len(args) > 0 && args[0] == "serve" {
		return a.serve(ctx, args[1:]), true
	}
	return 0, false
}

func (a application) helpEntry(args []string) (int, bool) {
	helpArgs := args
	if end := slices.Index(args, "--"); end >= 0 {
		helpArgs = args[:end]
	}
	options, rest, problems := parseCommon(helpArgs)
	if catalogueRequested(options, rest, problems) {
		return a.catalogue(), true
	}
	if helpRequested(options, rest) {
		return a.help(commands(), rest, problems), true
	}

	return 0, false
}
