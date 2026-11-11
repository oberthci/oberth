package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/boundedio"
)

// ErrUpstreamRepositoryNotFound reports that the forge has no readable
// repository at the path Oberth derives from the upstream base URL.
var ErrUpstreamRepositoryNotFound = errors.New("app: upstream repository not found")

// UpstreamRepositoryPath is the repository path Git sends to an SSH forge for
// repositoryName under baseURL, as git itself derives it from an ssh:// URL:
// a leading "/~" names the login user's home and loses its slash.
func UpstreamRepositoryPath(baseURL, repositoryName string) (string, error) {
	parsed, err := url.Parse(joinUpstream(baseURL, repositoryName))
	if err != nil {
		return "", err
	}
	path := parsed.Path
	if strings.HasPrefix(path, "/~") {
		path = path[1:]
	}
	if path == "" || strings.ContainsAny(path, "'!\x00\r\n") {
		return "", fmt.Errorf("app: unsupported upstream repository path %q", path)
	}
	return path, nil
}

// UpstreamPathHint suggests the home-relative form when an absolute SSH
// repository path was not found: plain git-shell and gitolite-style forges
// keep repositories relative to the git user's home.
func UpstreamPathHint(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "ssh" || strings.HasPrefix(parsed.Path, "/~") {
		return ""
	}
	home := *parsed
	home.Path = "/~" + parsed.Path
	return fmt.Sprintf("if this forge keeps repositories relative to the git user's home, register the upstream as %s", home.String())
}

// ProbeSSHRepository proves that the forge serves repositoryName under the
// upstream base URL: it authenticates with the pinned host keys, starts
// git-upload-pack for the derived path and reads the first pkt-line of the
// ref advertisement, then ends the request with a flush packet. No objects
// are transferred. A forge that reports no such repository yields
// ErrUpstreamRepositoryNotFound naming the path tried.
func ProbeSSHRepository(ctx context.Context, baseURL, repositoryName string, privateKey, knownHosts []byte) error {
	path, err := UpstreamRepositoryPath(baseURL, repositoryName)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, defaultSSHProbeTimeout)
	defer cancel()
	client, err := dialPinnedSSH(probeCtx, baseURL, privateKey, knownHosts)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	stop := context.AfterFunc(probeCtx, func() { _ = client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open SSH session for repository probe: %w", err)
	}
	defer func() { _ = session.Close() }()
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	diagnostics := &boundedio.Buffer{Limit: 4096}
	session.Stderr = diagnostics
	if err := session.Start("git-upload-pack '" + path + "'"); err != nil {
		return fmt.Errorf("start git-upload-pack for %s: %w", path, err)
	}
	header := make([]byte, 4)
	_, readErr := io.ReadFull(stdout, header)
	refusal := ""
	if readErr == nil && isPktLineLength(header) {
		// A forge may refuse inside the protocol: an "ERR <reason>" pkt-line
		// in place of the advertisement is a failure, not a repository.
		refusal, readErr = errorPktLine(stdout, header)
		if readErr == nil && refusal == "" {
			// The advertisement started: the repository exists and is
			// readable. A flush packet ends the request before negotiation.
			_, _ = stdin.Write([]byte("0000"))
			_ = stdin.Close()
			return nil
		}
	}
	_ = stdin.Close()
	if refusal != "" {
		_ = session.Close()
	}
	waitErr := session.Wait()
	if probeCtx.Err() != nil {
		return fmt.Errorf("probe repository %s: %w", path, context.Cause(probeCtx))
	}
	reason := refusal
	if reason == "" {
		reason = firstLine(diagnostics.Bytes())
	}
	if reason == "" && waitErr != nil {
		reason = waitErr.Error()
	}
	if reason == "" && readErr != nil {
		reason = readErr.Error()
	}
	hint := ""
	if text := UpstreamPathHint(baseURL); text != "" {
		hint = "; " + text
	}
	return fmt.Errorf("%w: the forge serves no repository at %s (%s)%s", ErrUpstreamRepositoryNotFound, path, reason, hint)
}

// errorPktLine reads the start of the first pkt-line's payload and returns
// the reason when it is an "ERR " line (bounded), or "" for anything else.
func errorPktLine(stdout io.Reader, header []byte) (string, error) {
	length, err := strconv.ParseUint(string(header), 16, 16)
	if err != nil {
		return "", err
	}
	if length < 8 {
		return "", nil // A flush or a payload too short to be an ERR line.
	}
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(stdout, prefix); err != nil {
		return "", err
	}
	if string(prefix) != "ERR " {
		return "", nil
	}
	rest := make([]byte, min(int(length)-8, 1024))
	n, _ := io.ReadFull(stdout, rest)
	reason := firstLine(rest[:n])
	if reason == "" {
		reason = "the forge refused the repository"
	}
	return reason, nil
}

func isPktLineLength(header []byte) bool {
	for _, b := range header {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func firstLine(body []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(body), []byte("\n"))
	return strings.Join(strings.Fields(strings.ToValidUTF8(string(line), "?")), " ")
}
