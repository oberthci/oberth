package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oberthci/oberth/internal/argojob"
)

// `oberth run hold ...` is the CLI form of the assisted-continuation MCP
// tools (run_hold_list, run_hold_exec, run_hold_patch, run_continue,
// run_hold_abandon). See docs/assisted-continuation.md.
const runHoldUsage = "Usage: oberth run hold list <run-id> [--json]\n" +
	"       oberth run hold exec <run-id> [--step <name>] [--stdin <path>] [--timeout <seconds>] [--json] -- <argv...>\n" +
	"       oberth run hold patch <run-id> [--step <name>] (--diff-file <path> | --diff-file -) [--json]\n" +
	"       oberth run hold continue <run-id> [--step <name>] [--json]\n" +
	"       oberth run hold abandon <run-id> [--step <name>] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

type runHoldOptions struct {
	step, stdinPath, diffFile string
	timeout                   int
	asJSON                    bool
	transport                 []string
	positional                []string
}

func parseRunHoldOptions(command string, arguments []string, withExec, withPatch bool) (runHoldOptions, error) {
	var options runHoldOptions
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.StringVar(&options.step, "step", "", "held step (Argo template name); optional with one hold")
	flags.BoolVar(&options.asJSON, "json", false, "emit JSON")
	if withExec {
		flags.StringVar(&options.stdinPath, "stdin", "", "file whose contents become the command's standard input")
		flags.IntVar(&options.timeout, "timeout", 0, "command deadline in seconds (at most the server's assist.execTimeout)")
	}
	if withPatch {
		flags.StringVar(&options.diffFile, "diff-file", "", "unified diff to apply; - reads standard input")
	}
	transport := mcpTransportFlags(flags)
	var help bool
	var err error
	options.positional, help, err = parseOperatorFlags(flags, arguments)
	if err != nil {
		return options, err
	}
	if help {
		return options, commandHelpError{usage: runHoldUsage}
	}
	options.transport = transport()
	return options, nil
}

func runRunHold(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 || wantsHelp(arguments[:1]) {
		_, err := fmt.Fprintln(output, runHoldUsage)
		return err
	}
	action, rest := arguments[0], arguments[1:]
	var argv []string
	if action == "exec" {
		if separator := indexOf(rest, "--"); separator >= 0 {
			argv = rest[separator+1:]
			rest = rest[:separator]
		}
	}
	tool := map[string]string{"list": "run_hold_list", "exec": "run_hold_exec", "patch": "run_hold_patch", "continue": "run_continue", "abandon": "run_hold_abandon"}[action]
	if tool == "" {
		return fmt.Errorf("%w: run hold list|exec|patch|continue|abandon", errUsage)
	}
	options, err := parseRunHoldOptions("run hold "+action, rest, action == "exec", action == "patch")
	if err != nil {
		return err
	}
	if len(options.positional) != 1 {
		return fmt.Errorf("%w: run hold %s <run-id>", errUsage, action)
	}
	config, err := parseMCPFlags("run hold "+action, options.transport, 0)
	if err != nil {
		return err
	}
	toolArguments := map[string]any{"id": options.positional[0]}
	if options.step != "" {
		toolArguments["step"] = options.step
	}
	switch action {
	case "exec":
		if len(argv) == 0 {
			return fmt.Errorf("%w: run hold exec <run-id> -- <argv...>", errUsage)
		}
		toolArguments["argv"] = argv
		if options.stdinPath != "" {
			content, err := os.ReadFile(options.stdinPath) // #nosec G304 -- operator-named local file.
			if err != nil {
				return err
			}
			toolArguments["stdin"] = string(content)
		}
		if options.timeout > 0 {
			toolArguments["timeout_seconds"] = options.timeout
		}
	case "patch":
		if options.diffFile == "" {
			return fmt.Errorf("%w: run hold patch needs --diff-file <path> (or - for stdin)", errUsage)
		}
		var content []byte
		if options.diffFile == "-" {
			content, err = readHoldDiff(os.Stdin, argojob.MaxAssistPatchMaxBytes)
		} else if err = holdDiffFileWithinLimit(options.diffFile, argojob.MaxAssistPatchMaxBytes); err == nil {
			content, err = os.ReadFile(options.diffFile) // #nosec G304 -- operator-named local file.
		}
		if err == nil {
			err = holdDiffWithinLimit(int64(len(content)), argojob.MaxAssistPatchMaxBytes)
		}
		if err != nil {
			return err
		}
		toolArguments["diff"] = string(content)
	}
	raw, err := remoteRunCanceller{config: config}.call(ctx, tool, toolArguments)
	if err != nil {
		return err
	}
	return writeRunHold(output, action, raw, options.asJSON)
}

// The run hold patch diff is bounded by limit (the largest
// assist.patchMaxBytes any server accepts) before it is read into memory
// and encoded, so a wrong file or an endless pipe fails fast on the client
// instead of in a request the server must refuse.

// readHoldDiff reads at most limit+1 bytes of a piped diff.
func readHoldDiff(stdin io.Reader, limit int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(stdin, limit+1))
	if err != nil {
		return nil, err
	}
	return content, holdDiffWithinLimit(int64(len(content)), limit)
}

// holdDiffFileWithinLimit refuses a diff file larger than limit, or one
// that is not a regular file (a device or FIFO has no size to check; pipe
// it through --diff-file - instead), before it is read.
func holdDiffFileWithinLimit(path string, limit int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: --diff-file %s is not a regular file; use --diff-file - to read a pipe", errUsage, path)
	}
	return holdDiffWithinLimit(info.Size(), limit)
}

func holdDiffWithinLimit(size, limit int64) error {
	if size > limit {
		return fmt.Errorf("%w: diff is larger than %d bytes, the largest assist.patchMaxBytes; push the change as a commit instead", errUsage, limit)
	}
	return nil
}

func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func writeRunHold(output io.Writer, action string, raw json.RawMessage, asJSON bool) error {
	if asJSON {
		_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	switch action {
	case "list":
		var response struct {
			RunID    string `json:"run_id"`
			Assisted bool   `json:"assisted"`
			Holds    []struct {
				Pod          string `json:"pod"`
				Step         string `json:"step"`
				Attempt      int    `json:"attempt"`
				Deadline     string `json:"deadline"`
				PatchCapable bool   `json:"patch_capable"`
				Released     bool   `json:"released"`
			} `json:"holds"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
			return err
		}
		if len(response.Holds) == 0 {
			_, err := fmt.Fprintf(output, "run %s holds no step (assisted=%v)\n", response.RunID, response.Assisted)
			return err
		}
		for _, hold := range response.Holds {
			state := "parked"
			if hold.Released {
				state = "released"
			}
			if _, err := fmt.Fprintf(output, "%s\tattempt %d\t%s\tpod %s\tdeadline %s\tpatch=%v\n", hold.Step, hold.Attempt, state, hold.Pod, hold.Deadline, hold.PatchCapable); err != nil {
				return err
			}
		}
		return nil
	case "exec":
		var response struct {
			Result struct {
				ExitCode  int    `json:"exit_code"`
				Stdout    string `json:"stdout"`
				Stderr    string `json:"stderr"`
				Truncated bool   `json:"truncated"`
				TimedOut  bool   `json:"timed_out"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
			return err
		}
		if _, err := io.WriteString(output, response.Result.Stdout); err != nil {
			return err
		}
		if response.Result.Stderr != "" {
			if _, err := fmt.Fprintf(output, "[stderr]\n%s", response.Result.Stderr); err != nil {
				return err
			}
		}
		suffix := ""
		if response.Result.Truncated {
			suffix = " (output truncated)"
		}
		if response.Result.TimedOut {
			suffix += " (timed out)"
		}
		_, err := fmt.Fprintf(output, "[exit %d]%s\n", response.Result.ExitCode, suffix)
		return err
	default:
		_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
}
