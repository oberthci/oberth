package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// Git supplies these variables only to pre-receive, after negotiating the
// push-options capability. Persist intent before the hook lets refs change.
func recordNativePushOptions(diagnostics io.Writer) error {
	root, repo := os.Getenv("OBERTH_RECEIVE_OUTBOX"), os.Getenv("OBERTH_RECEIVE_REPO")
	if root == "" || repo == "" {
		return nil
	}
	c := &Cache{outboxRoot: root}
	reservation, err := c.readReservation(repo)
	if err != nil {
		return err
	}
	count, _ := strconv.Atoi(os.Getenv("GIT_PUSH_OPTION_COUNT"))
	if count < 0 || count > maximumPushOptions {
		return errors.New("too many push options")
	} else {
		options := make([]string, 0, count)
		for i := 0; i < count; i++ {
			options = append(options, os.Getenv("GIT_PUSH_OPTION_"+strconv.Itoa(i)))
		}
		parsed := parsePushOptions(options)
		if parsed.err != nil {
			return parsed.err
		}
		reservation.Assist = parsed.assist
		// Git forwards hook stderr to the pusher as "remote: ..." lines.
		for _, notice := range parsed.notices {
			_, _ = fmt.Fprintf(diagnostics, "oberth: %s\n", notice)
		}
	}
	if reservation.State != receiveReserved {
		return errors.New("push options require a reserved receive")
	}
	return c.writeReservation(reservation)
}

const maximumPushOptions = 32

const knownPushOptions = "oberth.assist=1"

type pushOptions struct {
	assist  bool
	notices []string
	err     error
}

// parsePushOptions reads the assisted-run intent. Retired scheduling options refuse the receive.
func parsePushOptions(options []string) (parsed pushOptions) {
	for _, option := range options {
		name, value, _ := strings.Cut(option, "=")
		switch {
		case name == "oberth.priority" || name == "oberth.priority_reason" || name == "oberth.supersede":
			parsed.err = fmt.Errorf("push option %s was removed in v0.22.0; author Argo priority in Workflow YAML; branch pushes supersede older work", name)
		case option == "oberth.assist=1" || option == "oberth.assist=true":
			parsed.assist = true
		case option == "oberth.assist=0" || option == "oberth.assist=false":
		case name == "oberth.assist":
			parsed.notices = append(parsed.notices, fmt.Sprintf("ignored push option %s: value %q is not recognised (known: %s)", name, boundedOptionText(value), knownPushOptions))
		case strings.HasPrefix(option, "oberth."):
			parsed.notices = append(parsed.notices, fmt.Sprintf("ignored unknown push option %s (known: %s)", boundedOptionText(name), knownPushOptions))
		}
	}
	return parsed
}

// boundedOptionText keeps a pusher-supplied name printable and short.
func boundedOptionText(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	if runes := []rune(text); len(runes) > 64 {
		return string(runes[:63]) + "…"
	}
	return text
}

type receiveFingerprintKey struct{}

// WithReceiveFingerprint binds the authenticated SSH key to a receive. It is
// server metadata, never a Git push option or client environment variable.
func WithReceiveFingerprint(ctx context.Context, fingerprint string) context.Context {
	return context.WithValue(ctx, receiveFingerprintKey{}, fingerprint)
}

func ReceiveFingerprint(ctx context.Context) string {
	fingerprint, _ := ctx.Value(receiveFingerprintKey{}).(string)
	return fingerprint
}
