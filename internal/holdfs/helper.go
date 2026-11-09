package holdfs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The Pod-side helper: `oberth internal-hold-fs <read|write|release> ...`.
// It runs through pods/exec inside a held attempt's main container with the
// server's own static executable, so no image shell or coreutils is needed.
// Every path is root-relative, cleaned by CleanPath, walked segment by
// segment with Lstat so no symlink is followed, and bounded in size.

// ReadRequest asks for files under the root.
type ReadRequest struct {
	Paths    []string `json:"paths"`
	MaxBytes int64    `json:"max_bytes"`
}

// FileState is one file as the helper saw it.
type FileState struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Content is the raw bytes when they fit MaxBytes; Truncated says they
	// did not and the server must refuse to patch this file.
	Content   []byte `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ReadResponse lists the files in request order.
type ReadResponse struct {
	Files []FileState `json:"files"`
}

// WriteRequest replaces, creates or deletes files atomically per file.
type WriteRequest struct {
	Files []FileWrite `json:"files"`
}

// FileWrite is one file change with its precondition.
type FileWrite struct {
	Path string `json:"path"`
	// ExpectSHA256 is the digest the server read; empty means the file must
	// not exist yet. A mismatch refuses the whole request before any write.
	ExpectSHA256 string `json:"expect_sha256"`
	Content      []byte `json:"content,omitempty"`
	Delete       bool   `json:"delete,omitempty"`
}

// WriteResponse reports the digests after the write.
type WriteResponse struct {
	Files []FileState `json:"files"`
}

// MaxReadBytes bounds any single read regardless of the request.
const MaxReadBytes = 16 << 20

// Run is the helper entrypoint; it returns the process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		_, _ = fmt.Fprintln(stderr, "usage: internal-hold-fs read|write --root <dir> | release --path <file>")
		return 2
	}
	var err error
	switch args[0] {
	case "read":
		err = runRead(args[1:], stdin, stdout)
	case "write":
		err = runWrite(args[1:], stdin, stdout)
	case "release":
		err = runRelease(args[1:])
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "internal-hold-fs:", err)
		return 1
	}
	return 0
}

func flagValue(args []string, name string) (string, error) {
	for index := 0; index+1 < len(args); index += 2 {
		if args[index] == name {
			return args[index+1], nil
		}
	}
	return "", fmt.Errorf("%s is required", name)
}

func runRead(args []string, stdin io.Reader, stdout io.Writer) error {
	root, err := flagValue(args, "--root")
	if err != nil {
		return err
	}
	var request ReadRequest
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&request); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if request.MaxBytes <= 0 || request.MaxBytes > MaxReadBytes {
		request.MaxBytes = MaxReadBytes
	}
	response := ReadResponse{Files: []FileState{}}
	for _, raw := range request.Paths {
		state, err := ReadFile(root, raw, request.MaxBytes)
		if err != nil {
			return err
		}
		response.Files = append(response.Files, state)
	}
	return json.NewEncoder(stdout).Encode(response)
}

// ReadFile reads one root-relative file without following symlinks.
func ReadFile(root, raw string, maxBytes int64) (FileState, error) {
	relative, err := CleanPath(raw)
	if err != nil {
		return FileState{}, err
	}
	state := FileState{Path: relative}
	full, err := resolveNoSymlink(root, relative, true)
	if err != nil {
		return FileState{}, err
	}
	info, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return FileState{}, err
	}
	if !info.Mode().IsRegular() {
		return FileState{}, fmt.Errorf("%w: %s is not a regular file", ErrPath, relative)
	}
	state.Exists, state.Size = true, info.Size()
	file, err := os.OpenFile(full, os.O_RDONLY|noFollow, 0) // #nosec G304 -- resolveNoSymlink confined full under root.
	if err != nil {
		return FileState{}, err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if state.Size > maxBytes {
		if _, err := io.Copy(digest, file); err != nil {
			return FileState{}, err
		}
		state.Truncated = true
	} else {
		content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
		if err != nil {
			return FileState{}, err
		}
		if int64(len(content)) > maxBytes {
			state.Truncated = true
		} else {
			state.Content = content
		}
		digest.Write(content)
	}
	state.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return state, nil
}

func runWrite(args []string, stdin io.Reader, stdout io.Writer) error {
	root, err := flagValue(args, "--root")
	if err != nil {
		return err
	}
	var request WriteRequest
	if err := json.NewDecoder(io.LimitReader(stdin, MaxReadBytes*2)).Decode(&request); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	response, err := WriteFiles(root, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(response)
}

// WriteFiles checks every precondition first, then writes each file through
// a same-directory temporary and rename. Directories are created as needed,
// never through a symlink.
func WriteFiles(root string, request WriteRequest) (WriteResponse, error) {
	type planned struct {
		full, relative string
		write          FileWrite
	}
	var plan []planned
	for _, write := range request.Files {
		relative, err := CleanPath(write.Path)
		if err != nil {
			return WriteResponse{}, err
		}
		full, err := resolveNoSymlink(root, relative, true)
		if err != nil {
			return WriteResponse{}, err
		}
		current, err := ReadFile(root, relative, MaxReadBytes)
		if err != nil {
			return WriteResponse{}, err
		}
		switch {
		case write.ExpectSHA256 == "" && current.Exists:
			return WriteResponse{}, fmt.Errorf("%w: %s exists but the patch creates it", ErrContext, relative)
		case write.ExpectSHA256 != "" && !current.Exists:
			return WriteResponse{}, fmt.Errorf("%w: %s vanished since it was read", ErrContext, relative)
		case write.ExpectSHA256 != "" && current.SHA256 != write.ExpectSHA256:
			return WriteResponse{}, fmt.Errorf("%w: %s changed since it was read", ErrContext, relative)
		case write.Delete && write.ExpectSHA256 == "":
			return WriteResponse{}, fmt.Errorf("%w: %s cannot be deleted before it exists", ErrContext, relative)
		}
		plan = append(plan, planned{full: full, relative: relative, write: write})
	}
	response := WriteResponse{Files: []FileState{}}
	for _, item := range plan {
		if item.write.Delete {
			if err := os.Remove(item.full); err != nil {
				return WriteResponse{}, err
			}
			response.Files = append(response.Files, FileState{Path: item.relative})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(item.full), 0o755); err != nil { // #nosec G301 -- a source tree directory the step itself must read.
			return WriteResponse{}, err
		}
		if err := writeAtomic(item.full, item.write.Content); err != nil {
			return WriteResponse{}, err
		}
		sum := sha256.Sum256(item.write.Content)
		response.Files = append(response.Files, FileState{Path: item.relative, Exists: true, Size: int64(len(item.write.Content)), SHA256: hex.EncodeToString(sum[:])})
	}
	return response, nil
}

func writeAtomic(full string, content []byte) error {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(full), ".oberth-hold-"+hex.EncodeToString(suffix[:]))
	mode := os.FileMode(0o644)
	if info, err := os.Lstat(full); err == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm()
	}
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, mode) // #nosec G304 -- temp sits beside the confined target.
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, full); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// resolveNoSymlink joins root and relative, refusing any existing symlink
// along the way. With allowMissing, absent segments are fine (a file or
// directory that does not exist yet, which a read reports and a write
// creates).
func resolveNoSymlink(root, relative string, allowMissing bool) (string, error) {
	root = filepath.Clean(root)
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: root %s is not a directory", ErrPath, root)
	}
	full := root
	segments := strings.Split(relative, "/")
	for index, segment := range segments {
		full = filepath.Join(full, segment)
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			if allowMissing || index == len(segments)-1 {
				continue
			}
			return "", fmt.Errorf("%w: %s does not exist", ErrPath, relative)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s is a symlink", ErrPath, strings.Join(segments[:index+1], "/"))
		}
		if index < len(segments)-1 && !info.IsDir() {
			return "", fmt.Errorf("%w: %s is not a directory", ErrPath, strings.Join(segments[:index+1], "/"))
		}
	}
	return full, nil
}

func runRelease(args []string) error {
	target, err := flagValue(args, "--path")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("%w: release path must be absolute", ErrPath)
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|noFollow, 0o600) // #nosec G304 -- the server names Argo's release marker path.
	if err != nil {
		return err
	}
	return file.Close()
}

// Digest is the hex SHA-256 of content.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
