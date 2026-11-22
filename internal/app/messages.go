package app

import (
	"errors"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

const assistEnabled = false // Disabled until the held-step redesign lands.

type message struct{ reason, remedy string }

func messages() map[string]message {
	return map[string]message{
		"release_version": {"release tag is not a version", "Use a stable version tag such as v1.2.3."},
		"release_head":    {"release default head is missing", "Push the tag again after the default branch is readable."},
		"release_old":     {"release version is not newer", "Choose a higher version."},
		"release_build":   {"release build has not passed", "Run .oberth/build.yaml on the tagged commit and wait for it to pass, then push the tag again."},
		"release_rerun":   {"release rerun facts differ", "Rerun the original release with its unchanged tag, commit, workflow and parameters."},
		"release_used":    {"release version is already used", "Release the next version; a release tag on the forge is never moved."},
		"release_count":   {"push carries more than one release tag", "Push one release tag at a time."},
		"tag_delivery":    {"release tag not delivered to the forge", "Check the forge: if the tag is there, this version is used; release the next one."},
		"assist":          {"assist unavailable", "assist is not available in this version; running without it"},
		"push":            {"push refused; nothing ran", "Fix every reported finding, then push again."},
		"config":          {"submission wiring is incomplete", "Configure every submission dependency before serving."},
		"recovery":        {"recovery requires a fresh server", "Restart Oberth to recover accepted submissions before serving new work."},
		"request":         {"run identity is incomplete", "Use an attributed run with an exact repository, ref and commit."},
		"schedule_ref":    {"schedule ref contains template syntax", "Rename the branch without '{{'."},
		"schedule":        {"schedule field must be literal", "Move the expression into a template; schedules need literal spec-level fields."},
		"read":            {"pipeline could not be read", "Check the selected file and repository access, then retry."},
		"missing":         {"pipeline file is missing", "Add the selected pipeline file at the exact commit, then retry."},
		"create":          {"workflow creation refused", "Correct the reported cluster problem before submitting again."},
		"submission":      {"run submission is unavailable", "The run stays accepted and is picked up at the next server start."},
		"unavailable":     {"admission is unavailable", "Try the push again when the dependency is available."},
		"decision":        {"ref was refused", "Follow the reported remedy before pushing again."},
		"prepare":         {"pipeline admission failed", "Fix every reported finding, then push again."},
		"pin":             {"source producer is not allowed", "Ask an administrator to allow this consumer and producer pair."},
		"reachable":       {"source commit is not on default history", "Choose a commit reachable from the producer's default branch."},
		"policy":          {"source policy is invalid", "Use canonical repository pairs within the documented policy bounds."},
		"timeout":         {"admission timed out", "Try the push again."},
		"closed":          {"server is shutting down", "Try again after the server restarts."},
		"keep":            {"source retention refused", "Check source storage, then rerun."},
		"lost":            {"workflow absent after restart", "Rerun this accepted run."},
		"foreign":         {"Workflow owner differs", "Inspect the named Workflow and resolve its ownership before rerunning."},
		"trust":           {"host fingerprint needs confirmation", "Pass the fingerprints verified or confirmed from the offered list."},
		"pins":            {"host pins cannot be replaced safely", "Use separate exact host lines before re-pinning this address."},
		"probe":           {"repository check failed", "Check the upstream address, identity and host pins, then retry."},
		"ready":           {"repository is readable", ""},
		"failed":          {"run submission failed", "Read the run detail for the cause."},
		"started":         {"run started", ""},
		"accepted":        {"run accepted", ""},
		"parked":          {"branch parked; nothing ran", "Run it with git push -o oberth.run=1."},
		"deleted":         {"ref deleted; nothing ran", ""},
		"absent":          {"no pipeline; nothing ran", "Add the selected pipeline file to start CI."},
	}
}

type Refusal struct {
	Code, Ref, File, Detail string
	Problems                []admissioncontract.Finding
	cause                   error
}

func (e *Refusal) Reason() string { return messages()[e.Code].reason }
func (e *Refusal) Remedy() string { return messages()[e.Code].remedy }
func (e *Refusal) Error() string {
	lines := e.findings()
	if len(lines) == 0 {
		lines = append(lines, strings.TrimPrefix(join(e.Ref, e.File)+": "+join(sentence(e.Reason()), e.Remedy()), ": "))
	}
	if e.Detail != "" {
		lines = append(lines, "  detail: "+e.Detail)
	}
	return strings.Join(lines, "\n")
}
func (e *Refusal) Unwrap() error { return e.cause }

// findings renders each finding as a push prints it, in admission order. A
// command is labelled with who runs it: the live-evidence check is the
// pusher's own; every other printed command needs an administrator.
func (e *Refusal) findings() []string {
	var lines []string
	for _, f := range e.Problems {
		fix := f.Remedy
		switch {
		case f.Command != "" && f.Code == "live":
			fix = f.Command
		case f.Command != "":
			fix = publicerr.AdministratorLine(f.Command)
		}
		lines = append(lines, join(e.Ref, e.File, f.Path)+": "+sentence(f.Reason)+"\n  fix: "+fix)
	}
	return lines
}

// listFindings makes the public refusal carry the findings as one context
// entry, so a caller of the service reads what a pusher reads, in that order.
func (e *Refusal) listFindings() {
	var public *publicerr.Error
	if len(e.Problems) == 0 || !errors.As(e.cause, &public) {
		return
	}
	listed := *public
	listed.Context = []string{strings.Join(e.findings(), "\n")}
	e.cause = &listed
}

// withCommand attaches the administrator command that resolves this refusal
// to the line a push prints and to its public form; "" attaches none.
func (e *Refusal) withCommand(command string) *Refusal {
	if command == "" {
		return e
	}
	e.Problems = []admissioncontract.Finding{{Code: e.Code, Reason: e.Reason(), Remedy: e.Remedy(), Command: command}}
	var public *publicerr.Error
	if errors.As(e.cause, &public) {
		_ = public.WithCommand(command)
	}
	return e
}
func refusal(code string, err error) *Refusal {
	if code == "decision" && !publicerr.Permanent(err) {
		code = "unavailable"
	}
	e := &Refusal{Code: code}
	kind := publicerr.InvalidInput
	switch code {
	case "read", "unavailable", "timeout", "closed", "keep", "create", "submission", "probe":
		kind = publicerr.Unavailable
	}
	if err != nil {
		e.Detail = err.Error()
		var public *publicerr.Error
		if errors.As(err, &public) {
			e.Problems = []admissioncontract.Finding{{Code: code, Reason: public.Reason(), Remedy: public.Remedy(), Command: public.Command()}}
			e.Detail = public.Detail
		}
	}
	m := messages()[code]
	e.cause = publicerr.New(kind, m.reason, m.remedy, err)
	return e
}
func sentence(text string) string { return strings.TrimRight(text, ".") + "." }
func join(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), " ")
}
