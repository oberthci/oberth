package verifytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Go consumes build flags before launching the test binary. Recover its overlay
// from the invoking process, including paths relative to that process's directory.
func callerOverlay(ctx context.Context, goFlags string, parentArgs []string) (map[string]string, error) {
	defaults, err := flagWords(goFlags)
	if err != nil {
		return nil, err
	}
	path, err := overlayPath(append(defaults, parentArgs...))
	if err != nil {
		return nil, err
	}
	if path == "" {
		return map[string]string{}, nil
	}
	directory, err := parentDirectory(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve invoking Go directory: %w", err)
	}
	return overlayFiles(directory, path)
}

func parentTestArguments() ([]string, error) {
	args, err := parentArguments()
	if err != nil {
		return nil, fmt.Errorf("read invoking Go arguments: %w", err)
	}
	for i := 1; i < len(args); i++ {
		if args[i] == "-C" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-C=") {
			continue
		}
		if args[i] == "test" {
			return args[i+1:], nil
		}
		break
	}
	return nil, errors.New("fixture rebuild requires direct go test; compiled test binaries do not retain command-line overlays")
}

func overlayPath(args []string) (string, error) {
	var path string
	for i := 0; i < len(args); i++ {
		name, value, equals := strings.Cut(args[i], "=")
		if name == "-args" || name == "--args" || name == "--" {
			break
		}
		if name != "-overlay" && name != "--overlay" {
			continue
		}
		if !equals {
			i++
			if i == len(args) {
				return "", errors.New("overlay path is missing")
			}
			value = args[i]
		}
		path = value
	}
	return path, nil
}

func overlayFiles(directory, path string) (map[string]string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var overlay struct{ Replace map[string]string }
	if err := json.Unmarshal(raw, &overlay); err != nil {
		return nil, err
	}
	replacements := map[string]string{}
	for source, target := range overlay.Replace {
		if !filepath.IsAbs(source) {
			source = filepath.Join(directory, source)
		}
		if target != "" && !filepath.IsAbs(target) {
			target = filepath.Join(directory, target)
		}
		replacements[source] = target
	}
	return replacements, nil
}

// GOFLAGS uses whole-word quotes without shell escaping.
func flagWords(flags string) ([]string, error) {
	var words []string
	for flags = strings.TrimLeft(flags, " \t\r\n"); flags != ""; flags = strings.TrimLeft(flags, " \t\r\n") {
		if flags[0] == '\'' || flags[0] == '"' {
			end := strings.IndexByte(flags[1:], flags[0])
			if end < 0 {
				return nil, errors.New("unterminated GOFLAGS quote")
			}
			words = append(words, flags[1:end+1])
			flags = flags[end+2:]
		} else {
			end := strings.IndexAny(flags, " \t\r\n")
			if end < 0 {
				end = len(flags)
			}
			words = append(words, flags[:end])
			flags = flags[end:]
		}
	}
	return words, nil
}

func overlayFlag(path string) (string, error) {
	flag := "-overlay=" + path
	if !strings.ContainsAny(flag, " \t\r\n") {
		return flag, nil
	}
	for _, quote := range []string{"'", "\""} {
		if !strings.Contains(flag, quote) {
			return quote + flag + quote, nil
		}
	}
	return "", errors.New("overlay path cannot be quoted in GOFLAGS")
}
