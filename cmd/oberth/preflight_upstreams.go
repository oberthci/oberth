package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/app"
)

// Consume the complete `oberth upstream list` metadata captured from the old
// server. No database, SSH identities, backup or network credentials are read.
func runPreflightUpstreams(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("preflight upstreams", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inventory := flags.String("inventory", "", "complete old-server upstream list metadata file")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if *inventory == "" || flags.NArg() != 0 {
		return fmt.Errorf("%w: preflight upstreams --inventory FILE", errUsage)
	}
	file, err := os.Open(*inventory) // #nosec G304 -- explicit coordinator-supplied metadata file, bounded below.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	names, err := httpsUpstreamInventory(file)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		for _, name := range names {
			if _, err := fmt.Fprintf(output, "HTTPS upstream: %s\n", name); err != nil {
				return err
			}
		}
		return errors.New("upgrade refused: HTTPS upstreams are unsupported by static Git (built without curl); migrate these registrations to pinned-key ssh:// before upgrading")
	}
	_, err = fmt.Fprintln(output, "upstream transport preflight passed: no registered HTTPS upstreams")
	return err
}

func httpsUpstreamInventory(input io.Reader) ([]string, error) {
	const maximum = 1 << 20
	limited := &io.LimitedReader{R: input, N: maximum + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	if !scanner.Scan() {
		return nil, errors.New("empty upstream inventory")
	}
	header := strings.Fields(scanner.Text())
	if len(header) < 3 || header[0] != "NAME" || header[1] != "KIND" || header[2] != "URL" {
		return nil, errors.New("unexpected upstream inventory header")
	}
	names := make([]string, 0)
	seen := make(map[string]bool)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || app.ValidateUpstreamName(fields[0]) != nil || seen[fields[0]] {
			return nil, errors.New("malformed or duplicate upstream metadata")
		}
		seen[fields[0]] = true
		endpoint, err := url.Parse(fields[2])
		if err != nil {
			return nil, errors.New("invalid upstream URL metadata")
		}
		switch endpoint.Scheme {
		case "https":
			names = append(names, fields[0])
		case "ssh":
			if endpoint.Hostname() == "" {
				return nil, errors.New("invalid SSH upstream metadata")
			}
		case "":
			if !strings.HasPrefix(fields[2], "/") {
				return nil, errors.New("unrecognized upstream transport")
			}
		default:
			return nil, errors.New("unrecognized upstream transport")
		}
	}
	if scanner.Err() != nil || limited.N <= 0 {
		return nil, errors.New("upstream inventory exceeded its bound or could not be read")
	}
	sort.Strings(names)
	return names, nil
}
