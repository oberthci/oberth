package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/model"
)

const promotionCreateUsage = "Usage: oberth promotion create --branch <target> [--sha <sha> | --sources-file <json-file>] [--repo <repository>] [--skip-conflicting] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"Choose exactly one of --sha or --sources-file.\nsources-file contains 1..32 ordered {\"sha\":\"...\",\"ref\":\"feature/one\"} objects in a JSON array.\n" +
	"Each source must be an exactly tested green branch commit. A train merges in order and runs CI once before publishing.\n" +
	"Example sources.json (replace with your three tested SHAs):\n" +
	`[{"sha":"1111111111111111111111111111111111111111"},{"sha":"2222222222222222222222222222222222222222"},{"sha":"3333333333333333333333333333333333333333"}]` + "\n" +
	"oberth promotion create --repo codeberg/acme/widgets --branch main --sources-file sources.json"

func runPromotionCreate(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("promotion create", flag.ContinueOnError)
	repo := flags.String("repo", "", "repository")
	branch := flags.String("branch", "", "target branch")
	sha := flags.String("sha", "", "single source SHA")
	sourcesFile := flags.String("sources-file", "", "ordered source JSON array")
	skip := flags.Bool("skip-conflicting", false, "skip conflicting train sources")
	flags.Bool("json", false, "emit JSON")
	transport := mcpTransportFlags(flags)
	positional, help, err := parseOperatorFlags(flags, arguments)
	if help {
		_, err = fmt.Fprintln(output, promotionCreateUsage)
		return err
	}
	if err != nil {
		return err
	}
	if len(positional) != 0 || *branch == "" || (*sha == "") == (*sourcesFile == "") || *skip && *sourcesFile == "" {
		return fmt.Errorf("%w: %s", errUsage, promotionCreateUsage)
	}
	args := map[string]any{"branch": *branch}
	if *repo != "" {
		args["repo"] = *repo
	}
	if *sourcesFile != "" {
		file, err := os.Open(*sourcesFile)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("%w: sources file exceeds 1 MiB", errUsage)
		}
		var sources []struct {
			SHA string `json:"sha"`
			Ref string `json:"ref,omitempty"`
		}
		if err = json.Unmarshal(raw, &sources); err != nil {
			return fmt.Errorf("%w: sources file: %w", errUsage, err)
		}
		if len(sources) == 0 || len(sources) > model.MaxPromotionSources {
			return fmt.Errorf("%w: train requires 1..32 sources", errUsage)
		}
		// Forward the original objects so server-side strict decoding also rejects
		// unknown fields, rather than silently removing them here.
		args["sources"] = json.RawMessage(raw)
		if *skip {
			args["skip_conflicting"] = true
		}
	} else {
		args["sha"] = *sha
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return runMCPCall(ctx, append([]string{"promote", string(raw)}, transport()...), output)
}
