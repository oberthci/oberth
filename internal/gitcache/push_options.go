package gitcache

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
)

// Git supplies these variables only to pre-receive, after negotiating the
// push-options capability. Persist intent before the hook lets refs change.
func recordNativePushOptions() error {
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
	if count < 0 || count > 32 {
		reservation.Priority = "invalid"
		reservation.PriorityReason = "priority refused: too many push options"
	} else {
		options := make([]string, 0, count)
		for i := 0; i < count; i++ {
			options = append(options, os.Getenv("GIT_PUSH_OPTION_"+strconv.Itoa(i)))
		}
		reservation.Priority, reservation.PriorityReason, reservation.Supersede, reservation.Assist = parsePushOptions(options)
	}
	if reservation.State != receiveReserved {
		return errors.New("push options require a reserved receive")
	}
	return c.writeReservation(reservation)
}

// parsePushOptions reads the oberth.* push options. oberth.assist=1 (or true)
// asks for agent-assisted retry holds on the branch run; the server still
// decides eligibility (branch CI, no credentials, policy enabled).
func parsePushOptions(options []string) (priority, reason string, supersede, assist bool) {
	seenPriority := false
	for _, option := range options {
		switch {
		case strings.HasPrefix(option, "oberth.priority="):
			if seenPriority {
				priority = "invalid"
				reason = "priority refused: duplicate priority push option"
				continue
			}
			seenPriority = true
			priority = strings.TrimPrefix(option, "oberth.priority=")
			if priority == "" || len(priority) > 16 {
				priority = "invalid"
			}
		case option == "oberth.supersede=true":
			supersede = true
		case option == "oberth.assist=1" || option == "oberth.assist=true":
			assist = true
		}
	}
	if priority != "" && reason == "" {
		reason = "git push priority request"
	}
	return
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
