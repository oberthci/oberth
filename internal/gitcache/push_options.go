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
		reservation.Priority = "invalid"
		reservation.PriorityReason = "priority refused: too many push options"
	} else {
		options := make([]string, 0, count)
		for i := 0; i < count; i++ {
			options = append(options, os.Getenv("GIT_PUSH_OPTION_"+strconv.Itoa(i)))
		}
		parsed := parsePushOptions(options)
		reservation.Priority, reservation.PriorityReason, reservation.Supersede, reservation.Assist = parsed.priority, parsed.reason, parsed.supersede, parsed.assist
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

// maximumPriorityReason matches the stored bound of a priority reason.
const maximumPriorityReason = 512

// knownPushOptions is every oberth.* push option the server reads; the
// notice for anything else lists them.
const knownPushOptions = "oberth.priority=<low|standard|promotion|high>, oberth.priority_reason=<text>, oberth.supersede=true, oberth.assist=1"

type pushOptions struct {
	priority, reason  string
	supersede, assist bool
	// notices are one-line warnings for the pusher: unknown or malformed
	// oberth.* options are ignored, never a reason to refuse the push.
	notices []string
}

// parsePushOptions reads the oberth.* push options. oberth.assist=1 (or true)
// asks for agent-assisted retry holds on the branch run; the server still
// decides eligibility (branch CI, no credentials, policy enabled).
// oberth.priority_reason explains a priority request; the store still bounds
// and audits it. Options outside the oberth. namespace belong to other hooks.
func parsePushOptions(options []string) (parsed pushOptions) {
	seenPriority, seenReason := false, false
	reason := ""
	for _, option := range options {
		name, value, _ := strings.Cut(option, "=")
		switch {
		case name == "oberth.priority":
			if seenPriority {
				parsed.priority = "invalid"
				parsed.reason = "priority refused: duplicate priority push option"
				continue
			}
			seenPriority = true
			parsed.priority = value
			if parsed.priority == "" || len(parsed.priority) > 16 {
				parsed.priority = "invalid"
			}
		case name == "oberth.priority_reason":
			if seenReason {
				parsed.notices = append(parsed.notices, "ignored duplicate push option oberth.priority_reason")
				continue
			}
			seenReason = true
			trimmed := strings.TrimSpace(value)
			if trimmed == "" || len(trimmed) > maximumPriorityReason || strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
				parsed.notices = append(parsed.notices, fmt.Sprintf("ignored push option oberth.priority_reason: use 1..%d bytes without control characters", maximumPriorityReason))
				continue
			}
			reason = trimmed
		case option == "oberth.supersede=true":
			parsed.supersede = true
		case option == "oberth.supersede=false":
		case option == "oberth.assist=1" || option == "oberth.assist=true":
			parsed.assist = true
		case option == "oberth.assist=0" || option == "oberth.assist=false":
		case name == "oberth.supersede" || name == "oberth.assist":
			parsed.notices = append(parsed.notices, fmt.Sprintf("ignored push option %s: value %q is not recognised (known: %s)", name, boundedOptionText(value), knownPushOptions))
		case strings.HasPrefix(option, "oberth."):
			parsed.notices = append(parsed.notices, fmt.Sprintf("ignored unknown push option %s (known: %s)", boundedOptionText(name), knownPushOptions))
		}
	}
	if parsed.priority != "" && parsed.reason == "" {
		parsed.reason = "git push priority request"
		if reason != "" {
			parsed.reason = reason
		}
	}
	if reason != "" && parsed.priority == "" {
		parsed.notices = append(parsed.notices, "ignored push option oberth.priority_reason: it explains an oberth.priority request")
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
