package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mattn/go-isatty"

	"github.com/oberthci/oberth/internal/setuptui"
)

func runSetup(ctx context.Context, arguments []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var opts setuptui.Options
	flags.BoolVar(&opts.DryMode, "dry-mode", false,
		"run the wizard fully, print the equivalent oberth install command, exit without applying")
	flags.BoolVar(&opts.Plain, "plain", false,
		"no TUI, sequential prompts (auto-selected for non-TTY, TERM=dumb, NO_COLOR)")
	flags.BoolVar(&opts.Accessible, "accessible", false,
		"screen reader mode (sequential prompts, no cursor addressing)")

	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: setup accepts flags only, no positional arguments", errUsage)
	}

	// Auto-select plain mode when the terminal is not interactive.
	if !opts.Plain && !opts.Accessible {
		if !isTTY(output) {
			opts.Plain = true
		} else if os.Getenv("TERM") == "dumb" {
			opts.Plain = true
			_, _ = fmt.Fprintln(output, "TERM=dumb — using sequential prompts")
		} else if os.Getenv("NO_COLOR") != "" {
			opts.Plain = true
			_, _ = fmt.Fprintln(output, "NO_COLOR set — using sequential prompts (unset NO_COLOR to use the full TUI)")
		}
	}

	// Thread the binary version so an apply resolves the same chart
	// version `oberth install` would (never a hardcoded placeholder).
	opts.BinaryVersion = version

	return setuptui.Run(ctx, opts, input, output)
}

// isTTY reports whether a writer is connected to a terminal.
func isTTY(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		return isatty.IsTerminal(f.Fd())
	}
	return false
}
