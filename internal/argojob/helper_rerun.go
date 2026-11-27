package argojob

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/nativebinding"
)

const maxRerunArchive = 1 << 30
const maxRerunTree = 512 << 20

// The manifest precedes C6's complete snapshot, including its Git metadata.
// The digest covers both, so removed paths cannot change independently.
type rerunManifest struct {
	RunID   string
	Commit  string
	Removed []string
	kept    string
}

func (r holdRuntime) checkoutRoot() string {
	if r.sourceRoot != "" {
		return r.sourceRoot
	}
	return "/work/src"
}

func validRerunDigest(d string) bool {
	b, err := hex.DecodeString(d)
	return err == nil && len(b) == sha256.Size && d == strings.ToLower(d)
}

func (r holdRuntime) stageRerun(ctx context.Context, input io.Reader, digest string) (string, error) {
	if input == nil || !validRerunDigest(digest) {
		return "", fmt.Errorf("invalid snapshot digest")
	}
	f, err := os.CreateTemp(r.root, ".oberth-resume-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(&resumeContextReader{ctx, input}, maxRerunArchive+1))
	if err != nil {
		return "", err
	}
	if n > maxRerunArchive || hex.EncodeToString(hash.Sum(nil)) != digest {
		return "", fmt.Errorf("snapshot digest mismatch")
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

type resumeContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *resumeContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Lstat the root itself: OpenRoot intentionally follows a root symlink.
func (runtime holdRuntime) rerunRoot(stage string) error {
	r, err := os.Lstat(runtime.checkoutRoot())
	if err != nil {
		return err
	}
	s, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	if !r.IsDir() || !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || filepath.Dir(stage) != runtime.root || !strings.HasPrefix(filepath.Base(stage), ".oberth-resume-") {
		return fmt.Errorf("source root or stage is unsafe")
	}
	sstat, sok := s.Sys().(*syscall.Stat_t)
	if runtime.sourceInfo == nil || !os.SameFile(runtime.sourceInfo, r) || !sok || sstat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("source root changed")
	}
	return nil
}

func snapshotPath(p string) bool {
	return p != "." && p != "" && filepath.IsLocal(p) && filepath.Clean(p) == p && !strings.Contains(p, "\\")
}

// Refuse symlink ancestors even inside os.Root. Leaf links are unlinked, never
// opened; hard links are also replaced with private newly created files.
func snapshotParents(root *os.Root, p string) error {
	parts := strings.Split(filepath.Dir(p), string(filepath.Separator))
	parent := ""
	for _, part := range parts {
		if part == "." {
			continue
		}
		parent = filepath.Join(parent, part)
		info, err := root.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			err = root.Mkdir(parent, 0700)
		} else if err == nil && !info.IsDir() {
			return fmt.Errorf("snapshot parent is not a directory")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r holdRuntime) applyRerunSnapshot(ctx context.Context, stage, digest string) (rerunManifest, error) {
	if err := r.rerunRoot(stage); err != nil {
		return rerunManifest{}, err
	}
	dir, err := os.MkdirTemp(r.root, ".oberth-resume-tree-*")
	if err != nil {
		return rerunManifest{}, err
	}
	defer os.RemoveAll(dir)
	manifest, files, err := verifyRerunSnapshot(ctx, stage, digest, dir)
	if err != nil {
		return rerunManifest{}, err
	}
	if err = r.rerunRoot(stage); err != nil {
		return rerunManifest{}, err
	}
	dest, err := openRerunRoot(r.checkoutRoot(), r.sourceInfo)
	if err != nil {
		return rerunManifest{}, err
	}
	defer dest.Close()
	if err = removeSnapshotPaths(dest, manifest.Removed); err != nil {
		return rerunManifest{}, err
	}
	for _, p := range files {
		if err = installSnapshotFile(dest, dir, p); err != nil {
			return rerunManifest{}, err
		}
	}
	manifest.kept = keptUntracked(dest, files)
	return manifest, ctx.Err()
}

// Verify the exact extracted bytes before changing any checkout path.
func verifyRerunSnapshot(ctx context.Context, stage, digest, dir string) (rerunManifest, []string, error) {
	var manifest rerunManifest
	f, err := os.OpenFile(stage, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return manifest, nil, err
	}
	defer f.Close()
	verified := sha256.New()
	input := io.TeeReader(io.LimitReader(f, maxRerunArchive+1), verified)
	if err = readHoldFrame(input, &manifest); err != nil || !validRerunManifest(manifest) {
		return manifest, nil, fmt.Errorf("invalid snapshot manifest")
	}
	files, err := unpackRerun(ctx, input, dir)
	if err != nil {
		return manifest, nil, err
	}
	if _, err = io.Copy(io.Discard, input); err != nil || hex.EncodeToString(verified.Sum(nil)) != digest {
		return manifest, nil, fmt.Errorf("snapshot digest mismatch")
	}
	head, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil || string(head) != manifest.Commit+"\n" {
		return manifest, nil, fmt.Errorf("snapshot HEAD differs from F")
	}
	return manifest, files, nil
}

func validRerunManifest(m rerunManifest) bool {
	return gitoid.Valid(m.Commit) && m.RunID != "" && len(m.RunID) <= 128 && !strings.ContainsAny(m.RunID, "\r\n\x00")
}

func openRerunRoot(root string, expected os.FileInfo) (*os.Root, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || expected == nil || !os.SameFile(expected, rootInfo) {
		return nil, fmt.Errorf("source root changed")
	}
	dest, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	opened, err := dest.Stat(".")
	if err != nil || !os.SameFile(rootInfo, opened) {
		_ = dest.Close()
		return nil, fmt.Errorf("source root changed while opening")
	}
	return dest, nil
}

func removeSnapshotPaths(dest *os.Root, paths []string) error {
	for _, p := range paths {
		if !snapshotPath(p) || p == ".git" || strings.HasPrefix(p, ".git/") {
			return fmt.Errorf("invalid removed path")
		}
		if err := snapshotParents(dest, p); err != nil {
			return err
		}
		if err := dest.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// No P0 refs, config, index, hooks or metadata survive the snapshot install.
	return dest.RemoveAll(".git")
}

func unpackRerun(ctx context.Context, input io.Reader, dir string) ([]string, error) {
	gz, err := gzip.NewReader(input)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(&resumeContextReader{ctx, gz}, maxRerunTree+(16<<20)))
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	seen, files, total := map[string]bool{}, []string{}, int64(0)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !validSnapshotEntry(h, seen, total) {
			return nil, fmt.Errorf("invalid snapshot entry")
		}
		seen[h.Name], total = true, total+h.Size
		if len(files) >= 100000 {
			return nil, fmt.Errorf("snapshot has too many files")
		}
		if err = writeStagedSnapshot(root, h, tr); err != nil {
			return nil, err
		}
		files = append(files, h.Name)
	}
	return files, finishSnapshotArchive(gz, seen)
}

func finishSnapshotArchive(gz *gzip.Reader, seen map[string]bool) error {
	if !seen[".git/HEAD"] || !seen[".git/index"] {
		return fmt.Errorf("snapshot lacks Git metadata")
	}
	// Consume the gzip trailer so checksum errors cannot be overlooked.
	if n, err := io.Copy(io.Discard, io.LimitReader(gz, 1)); err != nil || n != 0 {
		return fmt.Errorf("invalid snapshot trailer: %w", err)
	}
	return nil
}

func validSnapshotEntry(h *tar.Header, seen map[string]bool, total int64) bool {
	return snapshotPath(h.Name) && !seen[h.Name] && h.Typeflag == tar.TypeReg && h.Size >= 0 && h.Size <= maxRerunTree-total
}

func writeStagedSnapshot(root *os.Root, h *tar.Header, tr io.Reader) error {
	if err := snapshotParents(root, h.Name); err != nil {
		return err
	}
	f, err := root.OpenFile(h.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0755|0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, tr)
	return errors.Join(copyErr, f.Close())
}

func installSnapshotFile(dest *os.Root, stage, p string) error {
	if err := snapshotParents(dest, p); err != nil {
		return err
	}
	if err := dest.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(filepath.Join(stage, p))
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := dest.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

func (r holdRuntime) rerunCommand(ctx context.Context, request holdRequest, code int, diagnostic io.Writer) int {
	manifest, err := r.applyRerunSnapshot(ctx, request.Stage, request.Digest)
	if err != nil {
		return holdDiagnostic(diagnostic, code, fmt.Errorf("snapshot install failed: %w", err))
	}
	// End any partial command output before emitting the exact boundary line.
	_, _ = fmt.Fprintln(diagnostic, "\n"+nativebinding.RerunMarker(manifest.Commit, manifest.RunID))
	_, _ = fmt.Fprintln(diagnostic, "oberth-hold: kept untracked files: "+manifest.kept)
	if len(r.command) == 0 {
		return holdDiagnostic(diagnostic, code, fmt.Errorf("authored command is missing"))
	}
	cmd := exec.Command(r.command[0], r.command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.streams.In, r.streams.Out, r.streams.Err
	if err = cmd.Start(); err != nil {
		return holdDiagnostic(diagnostic, 127, err)
	}
	if r.pendingSignal != nil {
		_ = cmd.Process.Signal(r.pendingSignal)
	}
	result, _ := r.waitOriginal(ctx, cmd)
	return result
}
