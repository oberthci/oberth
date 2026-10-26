package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

type fixedRepositoryLoader struct{ store storer.Storer }

func (l fixedRepositoryLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.store, nil }

func (b *goGitBackend) serve(ctx context.Context, r *git.Repository, op string, spec commandSpec) error {
	out := spec.stdout
	if out == nil {
		out = io.Discard
	}
	input := spec.stdin
	if input == nil {
		return errors.New("git service input required")
	}
	// An SSH channel can block inside Read/Write. Closing it at the command
	// deadline interrupts those calls; checking ctx only before Read cannot.
	if closer, ok := input.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	if op == "receive-pack" {
		return b.serveReceive(ctx, r, input, out, spec.stderr)
	}
	service := server.NewServer(fixedRepositoryLoader{store: r.Storer})
	session, err := service.NewUploadPackSession(&transport.Endpoint{}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	refs, err := session.AdvertisedReferencesContext(ctx)
	if err != nil {
		return err
	}
	for name := range refs.References {
		if hiddenUploadRef(name) {
			delete(refs.References, name)
		}
	}
	for name := range refs.Peeled {
		if hiddenUploadRef(name) {
			delete(refs.Peeled, name)
		}
	}
	// Offer protocol v0 only; a Git client asking for v2 correctly falls back
	// when the advertisement has no v2 greeting.
	if err := refs.Encode(out); err != nil {
		return err
	}
	return serveGoUpload(ctx, r, input, out, refs)
}
func hiddenUploadRef(name string) bool {
	return strings.HasPrefix(name, "refs/oberth/") || strings.HasPrefix(name, "refs/replace/")
}

func (b *goGitBackend) serveReceive(ctx context.Context, r *git.Repository, input io.Reader, out, stderr io.Writer) error {
	refs := packp.NewAdvRefs()
	_ = refs.Capabilities.Set(capability.ReportStatus)
	_ = refs.Capabilities.Set(capability.DeleteRefs)
	_ = refs.Capabilities.Set(capability.OFSDelta)
	iter, err := r.References()
	if err != nil {
		return err
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference && isGoPublicRef(ref.Name().String()) {
			refs.References[ref.Name().String()] = ref.Hash()
		}
		return nil
	})
	iter.Close()
	if err != nil {
		return err
	}
	if err := refs.Encode(out); err != nil {
		return err
	}
	// Bound command headers before decoding; pack bytes remain streaming. A
	// pkt-line is at most 64 KiB, and the command count has a separate cap.
	bounded, err := readReceiveCommands(ctx, input)
	if err != nil {
		return err
	}
	request := packp.NewReferenceUpdateRequest()
	if err := request.Decode(io.MultiReader(strings.NewReader(bounded), input)); err != nil {
		return err
	}

	report, receiveErr := b.receiveGoPack(ctx, r, request, stderr)
	if request.Capabilities.Supports(capability.ReportStatus) {
		if err := report.Encode(out); err != nil {
			return errors.Join(receiveErr, err)
		}
	}
	if report.UnpackStatus == "ok" {
		return nil
	}
	return receiveErr
}
func isGoPublicRef(name string) bool {
	return strings.HasPrefix(name, "refs/heads/") || strings.HasPrefix(name, "refs/tags/")
}
func readReceiveCommands(ctx context.Context, input io.Reader) (string, error) {
	var result strings.Builder
	scan := pktline.NewScanner(contextReader{ctx, input})
	count := 0
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			result.WriteString("0000")
			return result.String(), nil
		}
		count++
		if count > maximumReceiveRefUpdates+1 {
			return "", errors.New("receive exceeds ref update limit")
		}
		_, _ = fmt.Fprintf(&result, "%04x%s", len(line)+4, line)
	}
	if err := scan.Err(); err != nil {
		return "", err
	}
	return "", io.ErrUnexpectedEOF
}
func (b *goGitBackend) receiveGoPack(ctx context.Context, r *git.Repository, request *packp.ReferenceUpdateRequest, stderr io.Writer) (*packp.ReportStatus, error) {
	report := packp.NewReportStatus()
	report.UnpackStatus = "ok"
	known, knownErr := goKnownRoots(r)
	if knownErr != nil {
		report.UnpackStatus = knownErr.Error()
		return report, knownErr
	}
	needsPack := false
	for _, command := range request.Commands {
		if !command.New.IsZero() {
			needsPack = true
			break
		}
	}
	if request.Packfile != nil && needsPack {
		err := packfile.UpdateObjectStorage(r.Storer, contextReader{ctx, request.Packfile})
		closeErr := request.Packfile.Close()
		if err != nil || closeErr != nil {
			failure := errors.Join(err, closeErr)
			report.UnpackStatus = failure.Error()
			return report, failure
		}
	}
	failure := b.receiveBatchPolicy(ctx, r, request.Commands)
	roots := []plumbing.Hash{}
	for _, cmd := range request.Commands {
		if !cmd.New.IsZero() {
			roots = append(roots, cmd.New)
		}
	}
	if failure == nil {
		if err := validateGoGraph(ctx, r, roots, known); err != nil {
			report.UnpackStatus = err.Error()
			return report, err
		}
	}
	// Global admission checks are evaluated before the first mutation. Invalid
	// individual ref updates remain per-ref refusals, matching receive-pack.
	var first error
	for _, command := range request.Commands {
		err := failure
		if err == nil {
			err = b.receiveRefPolicy(ctx, r, command)
		}
		if err == nil {
			err = applyReceiveRef(r, command)
		}
		status := "ok"
		if err != nil {
			status = err.Error()
			if first == nil {
				first = err
			}
			if stderr != nil {
				_, _ = fmt.Fprintln(stderr, "oberth: rejected receive:", err)
			}
		}
		report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{ReferenceName: command.Name, Status: status})
	}
	return report, first
}
func applyReceiveRef(r *git.Repository, command *packp.Command) error {
	old := plumbing.NewHashReference(command.Name, command.Old)
	store, ok := r.Storer.(*atomicReferenceStore)
	if !ok {
		return errors.New("atomic ref store required")
	}
	if command.New.IsZero() {
		return store.deleteReference(command.Name, old)
	}
	return store.CheckAndSetReference(plumbing.NewHashReference(command.Name, command.New), old)
}
func (b *goGitBackend) receiveBatchPolicy(ctx context.Context, r *git.Repository, commands []*packp.Command) error {
	if len(commands) > maximumReceiveRefUpdates {
		return fmt.Errorf("receive exceeds %d ref updates", maximumReceiveRefUpdates)
	}
	refs, err := r.References()
	if err != nil {
		return err
	}
	defer refs.Close()
	current := map[string]plumbing.Hash{}
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if isGoPublicRef(ref.Name().String()) {
			current[ref.Name().String()] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return err
	}
	count := len(current)
	seen := map[plumbing.ReferenceName]bool{}
	for _, cmd := range commands {
		if seen[cmd.Name] {
			return errors.New("duplicate ref update")
		}
		seen[cmd.Name] = true
		name := cmd.Name.String()
		old, exists := current[name]
		if len(current) > maximumPublicRefs {
			if !strings.HasPrefix(name, "refs/heads/") || !cmd.New.IsZero() || !exists || old != cmd.Old {
				return fmt.Errorf("repository exceeds %d public refs; recovery receive may contain only exact-current branch deletions", maximumPublicRefs)
			}
		}
		if isGoPublicRef(name) {
			if cmd.New.IsZero() && strings.HasPrefix(name, "refs/heads/") && exists && old == cmd.Old {
				count--
			} else if cmd.Old.IsZero() && !exists {
				count++
			}
		}
	}
	if count > maximumPublicRefs {
		return fmt.Errorf("receive would leave %d public refs; limit is %d", count, maximumPublicRefs)
	}
	return nil
}
func (b *goGitBackend) receiveRefPolicy(ctx context.Context, r *git.Repository, command *packp.Command) error {
	name := command.Name.String()
	if !isGoPublicRef(name) {
		return errors.New("deny updating a hidden ref")
	}
	if err := command.Name.Validate(); err != nil {
		return fmt.Errorf("invalid ref: %w", err)
	}
	branch := strings.TrimPrefix(name, "refs/heads/")
	if strings.HasPrefix(name, "refs/heads/") {
		if err := ValidateBranch(branch); err != nil {
			return fmt.Errorf("invalid branch: %w", err)
		}
		if !command.New.IsZero() {
			if _, err := r.CommitObject(command.New); err != nil {
				return fmt.Errorf("branch does not select a commit: %w", err)
			}
		}
		head, err := r.Reference(plumbing.HEAD, false)
		if err != nil {
			return err
		}
		if head.Target() == command.Name && !command.Old.IsZero() && !command.New.IsZero() {
			yes, err := goIsAncestor(ctx, r, command.Old, command.New)
			if err != nil {
				return err
			}
			if !yes {
				return errors.New("non-fast-forward push to default branch")
			}
		}
		return nil
	}
	tag := strings.TrimPrefix(name, "refs/tags/")
	if err := ValidateTag(tag); err != nil {
		return fmt.Errorf("invalid tag: %w", err)
	}
	if command.Old == command.New {
		return nil
	}
	if !command.Old.IsZero() {
		return errors.New("release tags are immutable")
	}
	if command.New.IsZero() {
		return errors.New("release tags cannot be deleted")
	}
	if _, err := r.Reference(plumbing.ReferenceName(upstreamRefPrefix+"tags/"+tag), true); err == nil {
		return errors.New("tag already exists upstream")
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return err
	}
	head, err := r.Reference(plumbing.HEAD, false)
	if err != nil {
		return errors.New("default branch is unavailable")
	}
	defaultBranch := strings.TrimPrefix(head.Target().String(), "refs/heads/")
	if err := ValidateBranch(defaultBranch); err != nil {
		return errors.New("default branch is invalid")
	}
	admission, err := r.Reference(plumbing.ReferenceName(releaseAdmissionRefPrefix+defaultBranch), true)
	if err != nil {
		return errors.New("fresh upstream default branch is unavailable")
	}
	commit, err := resolveGoObject(r, command.New.String(), true)
	if err != nil {
		return errors.New("tag does not select a commit")
	}
	yes, err := goIsAncestor(ctx, r, commit, admission.Hash())
	if err != nil {
		return err
	}
	if !yes {
		return fmt.Errorf("tag commit is not reachable from the fresh upstream default branch — promote the commit to the default branch first (promote <sha> %s), then tag", defaultBranch)
	}
	return nil
}
