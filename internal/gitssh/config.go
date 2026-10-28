// Package gitssh is Git's shell-free, pinned-key SSH transport.
package gitssh

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/kevinburke/ssh_config"
)

const ConfigEnvironment = "OBERTH_GIT_SSH_CONFIG"
const maximumFile = 1 << 20

type endpoint struct {
	host, user, port, command, identity, knownHosts string
}

func parse(arguments []string) (endpoint, error) {
	var value endpoint
	if len(arguments) >= 2 && arguments[0] == "-p" {
		value.port = arguments[1]
		arguments = arguments[2:]
		if !validPort(value.port) {
			return value, errors.New("invalid port")
		}
	}
	if len(arguments) != 2 {
		return value, errors.New("expected [-p port] host command")
	}
	value.host, value.command = arguments[0], arguments[1]
	if user, host, found := strings.Cut(value.host, "@"); found {
		if !validName(user) {
			return value, errors.New("invalid SSH user")
		}
		value.user, value.host = user, host
	}
	if strings.HasPrefix(value.host, "[") != strings.HasSuffix(value.host, "]") {
		return value, errors.New("invalid bracketed SSH host")
	}
	value.host = strings.TrimPrefix(strings.TrimSuffix(value.host, "]"), "[")
	if !validName(strings.ReplaceAll(value.host, ":", "")) || strings.Contains(value.host, "@") {
		return value, errors.New("invalid SSH host")
	}
	if value.command == "" || len(value.command) > 16384 || strings.ContainsAny(value.command, "\x00\r\n") {
		return value, errors.New("invalid Git command")
	}
	return value, nil
}

func validName(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validPort(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	port, err := strconv.Atoi(value)
	return err == nil && port > 0 && port <= 65535
}

func readFile(path string, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n$%\"\\") {
		return nil, errors.New("invalid configured path")
	}
	// Root confinement allows projected-volume symlinks within the configured
	// directory, but refuses a final symlink escaping it (including races).
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumFile || private && info.Mode().Perm()&0o037 != 0 {
		return nil, errors.New("invalid configured file or private-key permissions")
	}
	body, err := io.ReadAll(io.LimitReader(file, maximumFile+1))
	if err != nil || len(body) != int(info.Size()) {
		return nil, errors.New("configured file changed or exceeded bound")
	}
	return body, nil
}

func configured(path string, value endpoint) (endpoint, error) {
	body, err := readFile(path, false)
	if err != nil {
		return value, errors.New("SSH config unavailable")
	}
	// Reject Include before decoding: the parser may otherwise open other files.
	allowed := map[string]bool{"host": true, "hostname": true, "user": true, "port": true, "identityfile": true, "identitiesonly": true, "batchmode": true, "stricthostkeychecking": true, "globalknownhostsfile": true, "userknownhostsfile": true}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), maximumFile)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.FieldsFunc(line, func(c rune) bool { return unicode.IsSpace(c) || c == '=' })
		if len(fields) < 2 || !allowed[strings.ToLower(fields[0])] {
			return value, errors.New("unsupported SSH configuration")
		}
	}
	if scanner.Err() != nil {
		return value, errors.New("invalid SSH configuration")
	}
	config, err := ssh_config.Decode(bytes.NewReader(body))
	if err != nil {
		return value, errors.New("invalid SSH configuration")
	}
	alias := value.host
	get := func(key string) (string, error) {
		raw, err := config.Get(alias, key)
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = raw[1 : len(raw)-1]
		}
		return raw, err
	}
	for key, expected := range map[string]string{"IdentitiesOnly": "yes", "BatchMode": "yes", "StrictHostKeyChecking": "yes", "GlobalKnownHostsFile": "/dev/null"} {
		actual, err := get(key)
		if err != nil || actual != expected {
			return value, fmt.Errorf("SSH config must enforce %s", key)
		}
	}
	value.identity, err = get("IdentityFile")
	if err != nil || value.identity == "" {
		return value, errors.New("SSH identity absent")
	}
	value.knownHosts, err = get("UserKnownHostsFile")
	if err != nil || value.knownHosts == "" {
		return value, errors.New("pinned known_hosts absent")
	}
	if value.port == "" {
		value.port, err = get("Port")
		if err != nil {
			return value, err
		}
		if value.port == "" {
			value.port = "22"
		}
	}
	if !validPort(value.port) {
		return value, errors.New("invalid configured port")
	}
	if value.user == "" {
		value.user, err = get("User")
		if err != nil {
			return value, err
		}
		if value.user == "" {
			value.user = "git"
		}
	}
	if !validName(value.user) {
		return value, errors.New("invalid configured user")
	}
	host, err := get("HostName")
	if err != nil {
		return value, err
	}
	if host != "" {
		if !validName(strings.ReplaceAll(host, ":", "")) {
			return value, errors.New("invalid configured hostname")
		}
		value.host = host
	}
	return value, nil
}
