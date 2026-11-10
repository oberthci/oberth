package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// parseOperatorFlags preserves --, accepts flags around positionals, and never
// prints parse errors into a command's data output.
func parseOperatorFlags(flags *flag.FlagSet, arguments []string) ([]string, bool, error) {
	flags.SetOutput(io.Discard)
	var optionArgs, positional []string
	for i := 0; i < len(arguments); i++ {
		arg := arguments[i]
		if arg == "--" {
			positional = append(positional, arguments[i+1:]...)
			break
		}
		if arg == "--help" || arg == "-help" || arg == "-h" {
			return nil, true, nil
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := flags.Lookup(name)
		if f == nil {
			return nil, false, fmt.Errorf("%w: unknown flag %q", errUsage, arg)
		}
		optionArgs = append(optionArgs, arg)
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !inline && (!ok || !boolean.IsBoolFlag()) {
			if i+1 >= len(arguments) {
				return nil, false, fmt.Errorf("%w: %s requires a value", errUsage, flagSpelling(name))
			}
			if arguments[i+1] == "--help" || arguments[i+1] == "-h" || arguments[i+1] == "-help" {
				return nil, true, nil
			}
			i++
			if arguments[i] != "-" && strings.HasPrefix(arguments[i], "-") {
				return nil, false, fmt.Errorf("%w: %s needs a value, but got %q, which looks like another flag", errUsage, flagSpelling(name), arguments[i])
			}
			optionArgs = append(optionArgs, arguments[i])
		}
	}
	if err := flags.Parse(append(append(optionArgs, "--"), positional...)); err != nil {
		return nil, false, fmt.Errorf("%w: %s", errUsage, doubleDashFlags(err.Error()))
	}
	return flags.Args(), false, nil
}
func parseCommandFlags(flags *flag.FlagSet, args []string) error {
	_, help, err := parseOperatorFlags(flags, args)
	if help {
		return flag.ErrHelp
	}
	return err
}

type commandHelpError struct{ usage string }

func (e commandHelpError) Error() string        { return e.usage }
func (e commandHelpError) Is(target error) bool { return target == flag.ErrHelp }

// usageError marks a flag-parsing failure as a usage error exactly once;
// parseOperatorFlags already returns errUsage-wrapped errors.
func usageError(err error) error {
	if errors.Is(err, errUsage) {
		return err
	}
	return fmt.Errorf("%w: %w", errUsage, err)
}

// verbLikeWords are subcommand spellings people try that no command takes.
// Sent to the server as a run ID they would come back as an opaque 400.
var verbLikeWords = map[string]bool{
	"show": true, "list": true, "ls": true, "get": true, "view": true, "describe": true, "info": true,
	"status": true, "logs": true, "log": true, "delete": true, "remove": true, "rm": true, "help": true,
}

// refuseVerbAsRunID rejects a verb in the run-ID position and names the forms
// the command accepts.
func refuseVerbAsRunID(command, given, forms string) error {
	if !verbLikeWords[strings.ToLower(given)] {
		return nil
	}
	return fmt.Errorf("%w: %q is not a run ID; oberth %s takes %s", errUsage, given, command, forms)
}
