package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/boundedio"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func (c *Cache) installHook(p string) error {
	if err := os.MkdirAll(filepath.Join(p, "hooks"), 0700); err != nil {
		return err
	}
	for _, name := range []string{"pre-receive", "reference-transaction"} {
		hook := filepath.Join(p, "hooks", name)
		if err := os.Remove(hook); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(c.config.HookBinary, hook); err != nil {
			return err
		}
	}
	return nil
}

// receiveHookInvocation is dispatched by the composition command before its normal
// startup. Git invokes it in the receive process group; it never detaches.
func receiveHookInvocation(ctx context.Context, input io.Reader) error {
	p, err := os.Getwd()
	if err != nil {
		return err
	}
	record, err := readReservation(p)
	if err != nil {
		return err
	}
	err = receiveHook(ctx, input, p, &record)
	if err != nil {
		return errors.Join(err, markReceiveRejected(p, &record))
	}
	return nil
}
func receiveHook(ctx context.Context, input io.Reader, p string, record *Reservation) error {
	var err error
	if record.State != "reserved" {
		return refuse("receive state")
	}
	record.Event.Options, err = hookOptions()
	if err != nil {
		return err
	}
	updates, err := readHookUpdates(input)
	if err != nil {
		return err
	}
	if err = validateRecordUpdates(updates); err != nil {
		return err
	}
	if len(updates) > MaxUpdates {
		return refuse("receive update count")
	}
	record.Event.Updates = updates
	for _, u := range updates {
		if u.Ref == record.Event.Admission.DefaultBranch && u.NewSHA != "" && !record.AllowDefault {
			return &RefusalError{reason: reasonDefault, remedy: remedyDefault, Field: record.Event.Admission.DefaultBranch}
		}
		if err = validateUpdate(ctx, p, record, u); err != nil {
			return err
		}
	}
	return finishReceiveHook(ctx, p, record)
}
func finishReceiveHook(ctx context.Context, p string, record *Reservation) error {
	var err error
	if err = validatePublicRefCount(ctx, p, record.Event.Updates); err != nil {
		return err
	}
	if err = writeReservation(p, *record); err != nil {
		return err
	}
	if record.AlwaysRun || record.Event.Options.Run || slices.ContainsFunc(record.Event.Updates, func(u RefUpdate) bool {
		return u.NewSHA != "" && strings.HasPrefix(u.Ref, "refs/tags/")
	}) {
		if err = callGate(ctx); err != nil {
			return err
		}
	}
	record.State = "ready"
	return writeReservation(p, *record)
}
func zeroEmpty(s string) string {
	if strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}
func hookOptions() (PushOptions, error) {
	value := os.Getenv("GIT_PUSH_OPTION_COUNT")
	if value == "" {
		value = "0"
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return PushOptions{}, refuseOptions()
	}
	if count > 3 {
		return PushOptions{}, &RefusalError{reason: reasonOptionCount, remedy: remedyOptions}
	}
	options := make([]string, count)
	for i := range options {
		options[i] = os.Getenv("GIT_PUSH_OPTION_" + strconv.Itoa(i))
	}
	return ParsePushOptions(options)
}
func validateUpdate(ctx context.Context, p string, record *Reservation, u RefUpdate) error {
	a := record.Event.Admission
	if err := validateUpdateNames(record, u); err != nil {
		return err
	}
	if u.Ref == a.DefaultBranch && u.OldSHA != "" && u.NewSHA != "" {
		cmd := exec.CommandContext(ctx, os.Getenv("OBERTH_GIT_BINARY"), "merge-base", "--is-ancestor", u.OldSHA, u.NewSHA)
		cmd.Dir = p
		if err := cmd.Run(); err != nil {
			return refuse("default fast-forward")
		}
	}
	return nil
}
func validateUpdateNames(record *Reservation, u RefUpdate) error {
	if err := sourcecontract.ValidatePublicRef(u.Ref); err != nil {
		return err
	}
	if !optionalObject(u.OldSHA) || !optionalObject(u.NewSHA) {
		return refuse("receive object")
	}
	if strings.HasPrefix(u.Ref, "refs/tags/") && u.OldSHA != "" {
		return refuse("immutable tag")
	}
	if u.NewSHA == "" && protectedBranch(record.Event.Admission.DefaultBranch, u.Ref, record.ReservedBranches) {
		return refuse("protected branch")
	}
	return nil
}
func optionalObject(s string) bool { return s == "" || gitoid.Valid(s) }
func protectedBranch(defaultRef, ref string, patterns []string) bool {
	if ref == defaultRef {
		return true
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, branch); matched {
			return true
		}
	}
	return false
}

func validatePublicRefCount(ctx context.Context, p string, updates []RefUpdate) error {
	deletionOnly := len(updates) > 0
	for _, u := range updates {
		if u.NewSHA != "" {
			deletionOnly = false
		}
	}
	if deletionOnly {
		return nil
	}

	cmd := exec.CommandContext(
		ctx,
		os.Getenv("OBERTH_GIT_BINARY"),
		"for-each-ref",
		fmt.Sprintf("--count=%d", sourcecontract.MaxRefs+1),
		"--format=%(refname)",
		"refs/heads/",
		"refs/tags/",
	)
	cmd.Dir = p
	buffer := boundedio.New(2 << 20)
	cmd.Stdout = buffer
	if err := cmd.Run(); err != nil {
		return err
	}
	if err := buffer.Err(); err != nil {
		return err
	}
	refs := make(map[string]bool)
	for _, ref := range strings.Fields(string(buffer.Bytes())) {
		refs[ref] = true
	}
	if len(refs) > sourcecontract.MaxRefs {
		return refCountError()
	}
	for _, u := range updates {
		if u.NewSHA == "" {
			delete(refs, u.Ref)
		} else {
			refs[u.Ref] = true
		}
	}
	if len(refs) > sourcecontract.MaxRefs {
		return refCountError()
	}
	return nil
}

// ReceiveHookInvocation is dispatched by the composition executable before its
// normal startup. Git executes the owned symlink directly, without a shell.
func ReceiveHookInvocation(ctx context.Context, args []string, input io.Reader, diagnostics io.Writer) (int, bool) {
	if len(args) > 0 && filepath.Base(args[0]) == "reference-transaction" {
		if len(args) != 2 {
			return 1, true
		}
		if err := callTransactionGate(ctx, args[1], input); err != nil {
			reportHookRefusal(ctx, diagnostics, err)
			return 1, true
		}
		return 0, true
	}
	if len(args) == 0 || filepath.Base(args[0]) != "pre-receive" {
		return 0, false
	}
	if len(args) != 1 {
		_, _ = io.WriteString(diagnostics, reasonInvalid+". "+remedyInvalid+"\n")
		return 1, true
	}
	if err := receiveHookInvocation(ctx, input); err != nil {
		reportHookRefusal(ctx, diagnostics, err)
		return 1, true
	}
	return 0, true
}

// Product refusals travel through the gate; native stderr stays in server logs.
func reportHookRefusal(ctx context.Context, diagnostics io.Writer, err error) {
	if gateReports(err) {
		return
	}
	problem := publicerr.Classify(err)
	message := problem.Reason() + ". " + problem.Remedy()
	if reported := callGateRequest(ctx, gateRequest{Phase: "refused", Refusal: message}); !gateReports(reported) {
		_, _ = fmt.Fprintln(diagnostics, message)
	}
}

func gateReports(err error) bool {
	var gate *gateError
	return errors.As(err, &gate) && gate.reported
}
