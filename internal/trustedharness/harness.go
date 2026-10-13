// Package trustedharness executes operator-approved host-side suite drivers.
// Guest output has no authority over the result: only the host driver's waitpid
// status contributes a case, and only this process writes the inventory receipt.
package trustedharness

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

const MaxGuestBytes = 256 << 20
const MaxArtifactBytes = 256 << 20
const DriverFile = "suite-driver"

// Fetch admits only public, credential-free HTTPS addresses from operator
// policy. Redirects are refused; neither credentials nor candidate URLs enter
// this transport. Complete bytes are hashed before archive interpretation.
func Fetch(ctx context.Context, raw, expected string, limit int64) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("invalid trusted bundle endpoint")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("trusted bundle redirects refused") }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("fetch trusted bundle failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trusted bundle HTTP status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(b) == 0 || int64(len(b)) > limit {
		return nil, errors.New("trusted bundle exceeds bounds")
	}
	if digest(b) != expected {
		return nil, errors.New("trusted bundle digest mismatch")
	}
	return b, nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// GuestFiles accepts only a complete pinned USTAR containing kernel and
// initramfs regular files. There are no links, paths, extensions or disk tools.
func GuestFiles(bundle []byte, expected string) (map[string][]byte, error) {
	if len(bundle) == 0 || len(bundle) > MaxGuestBytes || digest(bundle) != expected {
		return nil, errors.New("guest bundle digest or bounds mismatch")
	}
	input := bytes.NewReader(bundle)
	tr := tar.NewReader(input)
	files := map[string][]byte{}
	for {
		h, e := tr.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, e
		}
		if (h.Name != "kernel" && h.Name != "initramfs") || h.Typeflag != tar.TypeReg || h.Format != tar.FormatUSTAR || len(h.PAXRecords) != 0 || files[h.Name] != nil || h.Size <= 0 || h.Size > MaxGuestBytes {
			return nil, errors.New("guest bundle requires only regular kernel and initramfs")
		}
		b, e := io.ReadAll(tr)
		if e != nil {
			return nil, e
		}
		files[h.Name] = b
	}
	for input.Len() > 0 {
		b, _ := input.ReadByte()
		if b != 0 {
			return nil, errors.New("guest bundle trailing data")
		}
	}
	if len(files) != 2 {
		return nil, errors.New("guest bundle missing boot files")
	}
	return files, nil
}
func driverBytes(bundle []byte) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(bundle))
	for {
		h, e := tr.Next()
		if errors.Is(e, io.EOF) {
			return nil, errors.New("suite-driver missing")
		}
		if e != nil {
			return nil, e
		}
		if h.Name == DriverFile {
			b, e := io.ReadAll(tr)
			if e != nil {
				return nil, e
			}
			f, e := elf.NewFile(bytes.NewReader(b))
			if e != nil {
				return nil, errors.New("suite-driver requires static ELF")
			}
			defer func() { _ = f.Close() }()
			if f.Class != elf.ELFCLASS64 || f.Machine != elf.EM_X86_64 || f.Type != elf.ET_EXEC {
				return nil, errors.New("suite-driver requires static linux/amd64 executable")
			}
			for _, p := range f.Progs {
				if p.Type == elf.PT_INTERP {
					return nil, errors.New("suite-driver must not use host loader")
				}
			}
			return b, nil
		}
	}
}

type artifactHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// snapshot copies only admitted regular artifacts into a private data-only tar.
// os.Root confines path resolution even when a producer planted symlinks. The
// resulting digest describes bytes actually transported, never a mutable PVC.
func snapshot(rootPath, destination string, paths []string) (string, error) {
	root, e := os.OpenRoot(rootPath)
	if e != nil {
		return "", e
	}
	defer func() { _ = root.Close() }()
	// #nosec G304 -- destination is a fresh file in server-owned private scratch, never a candidate path.
	out, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return "", e
	}
	defer func() { _ = out.Close() }()
	tw := tar.NewWriter(out)
	defer func() { _ = tw.Close() }()
	var hashes []artifactHash
	remaining := int64(MaxArtifactBytes)
	for _, p := range paths {
		info, e := root.Lstat(p)
		if e != nil || !info.Mode().IsRegular() {
			return "", errors.New("artifact must be a confined regular file")
		}
		f, e := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return "", e
		}
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() || st.Size() > remaining {
			_ = f.Close()
			return "", errors.New("artifact exceeds bounds")
		}
		b, e := io.ReadAll(io.LimitReader(f, remaining+1))
		_ = f.Close()
		if e != nil || int64(len(b)) > remaining {
			return "", errors.New("artifact snapshot exceeds bounds")
		}
		remaining -= int64(len(b))
		if e = tw.WriteHeader(&tar.Header{Name: p, Mode: 0444, Size: int64(len(b)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); e != nil {
			return "", e
		}
		if _, e = tw.Write(b); e != nil {
			return "", e
		}
		hashes = append(hashes, artifactHash{p, digest(b), int64(len(b))})
	}
	if e = tw.Close(); e != nil {
		return "", e
	}
	if e = out.Sync(); e != nil {
		return "", e
	}
	if e = out.Close(); e != nil {
		return "", e
	}
	b, _ := json.Marshal(hashes)
	return digest(b), nil
}

// Run has no caller-selectable program or QEMU options. An administrator has
// authorized intent.Recipe before starting this pinned binary. Candidate bytes
// reach only fw_cfg's read-only data channel; no host directory is guest-shared.
func Run(ctx context.Context, intent vmrunner.KVMIntent, artifactRoot, privateRoot string) (vmrunner.KVMReport, error) {
	return run(ctx, intent, artifactRoot, privateRoot, harnessDependencies{fetch: Fetch, qemu: func(args ...string) *exec.Cmd {
		// #nosec G204 -- fixed binary and server-constructed closed QEMU argv; no candidate commands.
		return exec.CommandContext(ctx, "/usr/bin/qemu-system-x86_64", args...)
	}})
}

type harnessDependencies struct {
	fetch func(context.Context, string, string, int64) ([]byte, error)
	qemu  func(...string) *exec.Cmd
}

func run(ctx context.Context, intent vmrunner.KVMIntent, artifactRoot, privateRoot string, deps harnessDependencies) (vmrunner.KVMReport, error) {
	if err := intent.Validate(); err != nil {
		return vmrunner.KVMReport{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, intent.Deadline)
	defer cancel()
	suiteBytes, e := deps.fetch(ctx, intent.Recipe.SuiteURL, intent.Contract.SuiteSHA256, vmrunner.MaxSuiteBundleBytes)
	if e != nil {
		return vmrunner.KVMReport{}, e
	}
	suite, e := vmrunner.VerifyKVMSuiteBundle(intent.Contract, suiteBytes)
	if e != nil {
		return vmrunner.KVMReport{}, e
	}
	inventory, count := suite.Inventory()
	if inventory != intent.InventoryDigest || count != intent.CaseCount {
		return vmrunner.KVMReport{}, errors.New("admitted inventory changed")
	}
	driver, e := driverBytes(suiteBytes)
	if e != nil {
		return vmrunner.KVMReport{}, e
	}
	guest, e := deps.fetch(ctx, intent.Recipe.GuestURL, intent.Contract.GuestSHA256, MaxGuestBytes)
	if e != nil {
		return vmrunner.KVMReport{}, e
	}
	files, e := GuestFiles(guest, intent.Contract.GuestSHA256)
	if e != nil {
		return vmrunner.KVMReport{}, e
	}
	dir, e := os.MkdirTemp(privateRoot, "execution-")
	if e != nil {
		return vmrunner.KVMReport{}, e
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, b := range files {
		if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			return vmrunner.KVMReport{}, e
		}
	}
	executable := filepath.Join(dir, DriverFile)
	// #nosec G306 -- the independently approved, digest-verified static suite driver must be executable in private scratch.
	if e = os.WriteFile(executable, driver, 0700); e != nil {
		return vmrunner.KVMReport{}, e
	}
	input := filepath.Join(dir, "artifacts.tar")
	artifactDigest, e := snapshot(artifactRoot, input, intent.Contract.Artifacts)
	if e != nil {
		return vmrunner.KVMReport{}, e
	}
	qmp := filepath.Join(dir, "qmp.sock")
	control := filepath.Join(dir, "guest.sock")
	qemu := deps.qemu("-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny", "-nodefaults", "-no-user-config", "-display", "none", "-accel", "kvm", "-cpu", "host", "-m", "2048", "-smp", "2", "-nic", "none", "-kernel", filepath.Join(dir, "kernel"), "-initrd", filepath.Join(dir, "initramfs"), "-append", "console=ttyS0 panic=1", "-serial", "null", "-qmp", "unix:"+qmp+",server=on,wait=off", "-device", "virtio-serial-pci", "-chardev", "socket,id=control,path="+control+",server=on,wait=off", "-device", "virtserialport,chardev=control,name=org.oberth.probe", "-fw_cfg", "name=opt/oberth/artifacts,file="+input)
	qemu.Env = []string{"PATH=/usr/bin:/bin"}
	qemu.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = qemu.Start(); e != nil {
		return vmrunner.KVMReport{}, e
	}
	done := make(chan error, 1)
	go func() { done <- qemu.Wait() }()
	joined := false
	defer func() {
		_ = syscall.Kill(-qemu.Process.Pid, syscall.SIGKILL)
		if !joined {
			<-done
		}
	}()
	var results []vmrunner.KVMCaseResult
	for _, id := range suite.CaseIDs() {
		select {
		case <-ctx.Done():
			return vmrunner.KVMReport{}, ctx.Err()
		case <-done:
			joined = true
			return vmrunner.KVMReport{}, errors.New("guest exited before cases completed")
		default:
		}
		if e = runHostCase(ctx, executable, dir, id, qmp, control, intent.Deadline); e != nil {
			return vmrunner.KVMReport{}, e
		}
		results = append(results, vmrunner.KVMCaseResult{ID: id, Outcome: "passed"})
	}
	if e = suite.VerifyInventory(intent.Contract, results); e != nil {
		return vmrunner.KVMReport{}, e
	}
	// The host ends the guest through its private QMP channel and requires QEMU
	// to exit successfully. Timeout/error paths kill and reap its process group.
	if e = quitQEMU(ctx, qmp); e != nil {
		return vmrunner.KVMReport{}, e
	}
	select {
	case e = <-done:
		joined = true
		if e != nil {
			return vmrunner.KVMReport{}, e
		}
	case <-ctx.Done():
		return vmrunner.KVMReport{}, ctx.Err()
	}
	actual, _ := json.Marshal(results)
	return vmrunner.KVMReport{Version: 1, IntentDigest: vmrunner.KVMIntentDigest(intent), InventoryDigest: digest(actual), CaseCount: len(results), ArtifactSetDigest: artifactDigest}, nil
}
func quitQEMU(ctx context.Context, path string) error {
	conn, e := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if e != nil {
		return e
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if e = conn.SetDeadline(deadline); e != nil {
		return e
	}
	dec := json.NewDecoder(io.LimitReader(conn, 64<<10))
	var greeting map[string]json.RawMessage
	if e = dec.Decode(&greeting); e != nil || greeting["QMP"] == nil {
		return errors.New("invalid QMP greeting")
	}
	for _, command := range []string{"qmp_capabilities", "quit"} {
		if e = json.NewEncoder(conn).Encode(map[string]string{"execute": command}); e != nil {
			return e
		}
		for {
			var response map[string]json.RawMessage
			if e = dec.Decode(&response); e != nil {
				return e
			}
			if response["error"] != nil {
				return errors.New("QMP command rejected")
			}
			if response["return"] != nil {
				break
			}
		}
	}
	return nil
}

func runHostCase(ctx context.Context, executable, dir, id, qmp, control string, deadline time.Time) error {
	// #nosec G204 -- fixed-path static host driver from the complete operator-approved suite digest; never candidate code.
	cmd := exec.CommandContext(ctx, executable, "--case", id, "--qmp", qmp, "--guest-control", control, "--deadline", deadline.UTC().Format(time.RFC3339Nano))
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("trusted host case %s failed: %w", id, err)
	}
	return nil
}
