package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
)

func (a application) mcpEntry(ctx context.Context, args []string) (int, bool) {
	if len(args) == 0 || args[0] != "mcp" {
		return 0, false
	}
	profile, err := a.mcpProfile(ctx, args[1:])
	if err != nil {
		var problems usageProblems
		if errors.As(err, &problems) {
			return a.usage(problems), true
		}
		return a.failed(err), true
	}
	return a.runMCP(ctx, profile), true
}

// Display uses the CLI's renderer, retaining detail as separate named fields.
func Display(tool string, structured json.RawMessage) (string, bool) {
	switch tool {
	case "run_list", "run_get", "run_wait", "status", "wait", "events_wait":
		var out bytes.Buffer
		a := application{out: &out}
		options := commonOptions{detail: true}
		for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
			if d.Name == tool {
				options.resultType = d.ResultType
				break
			}
		}
		if a.render(structured, false, options) != nil {
			return "", false
		}
		return safeDisplay(out.String()), true
	default:
		return "", false
	}
}

func (a application) agentTools(profile string) error {
	if profile == "" {
		return writeWrapped(a.out, "agent tools: run oberth join <file> to create an uplink connection.")
	}
	path, err := a.clientExecutable()
	if err != nil {
		return flowError("agent", err)
	}
	if strings.ContainsAny(path, " \t\r\n\"") {
		path = strconv.Quote(path)
	}
	_, err = fmt.Fprintf(a.out, "agent tools: stdio MCP server \"oberth\" = %s mcp --profile %s\n", path, commandWord(profile))
	return err
}
