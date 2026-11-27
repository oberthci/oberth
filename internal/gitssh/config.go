package gitssh

import (
	"bufio"
	"bytes"
	"context"
	"github.com/oberthci/oberth/internal/publicerr"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ConfigEnvironment = "OBERTH_GIT_SSH_CONFIG"
const reasonConfig = "Git SSH configuration is invalid"
const reasonTransport = "Git SSH transport is unavailable"
const reasonPins = "no usable Git host key pin"

type Config struct {
	Host, User, Port, Command, Identity, KnownHosts, Scratch string
	Now                                                      func() time.Time
	Dial                                                     func(context.Context, string) (net.Conn, error)
	// ReadFile supplies the owning server's custody rules; nil uses personal files.
	ReadFile func(string, bool) ([]byte, error)
}

// Parse accepts Git's simple SSH invocation; it never evaluates a shell command.
func Parse(args []string) (Config, error) {
	var c Config
	if len(args) >= 2 && args[0] == "-p" {
		c.Port = args[1]
		args = args[2:]
	}
	if len(args) != 2 {
		return c, failure(reasonConfig)
	}
	c.Host = args[0]
	c.Command = args[1]
	if user, host, ok := strings.Cut(c.Host, "@"); ok {
		c.User = user
		c.Host = host
	}
	if c.Host == "" || strings.HasPrefix(c.Host, "-") || strings.ContainsAny(c.Host, " /\\\x00\r\n") {
		return c, failure(reasonConfig)
	}
	if c.Command == "" || len(c.Command) > 16384 || strings.ContainsAny(c.Command, "\x00\r\n") {
		return c, failure(reasonConfig)
	}
	return c, nil
}

// Configure reads a bounded explicit directive set. No include, substitution,
// ambient identity, wildcard host selection or executable directive is accepted.
func Configure(path string, c Config) (configured Config, result error) {
	defer func() { result = pushBoundary(c.Command, false, result) }()
	data, err := c.readFile(path, false)
	if err != nil {
		return c, err
	}
	fields, err := directives(data, c.Host)
	if err != nil {
		return c, err
	}
	for k, v := range map[string]string{"identitiesonly": "yes", "batchmode": "yes", "stricthostkeychecking": "yes", "globalknownhostsfile": "/dev/null"} {
		if fields[k] != v {
			return c, failure(reasonConfig)
		}
	}
	c.Identity = fields["identityfile"]
	c.KnownHosts = fields["userknownhostsfile"]
	c.Host = first(fields["hostname"], c.Host)
	c.User = first(c.User, first(fields["user"], "git"))
	c.Port = first(c.Port, first(fields["port"], "22"))
	n, err := strconv.Atoi(c.Port)
	if err != nil || n < 1 || n > 65535 || c.Identity == "" || c.KnownHosts == "" {
		return c, failure(reasonConfig)
	}
	return c, nil
}
func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func directives(data []byte, host string) (map[string]string, error) {
	fields := make(map[string]string)
	active := true
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), (1<<20)+1)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v := directive(line)
		if !allowedDirective(k) || v == "" {
			return nil, failure(reasonConfig)
		}
		if k == "host" {
			active = v == host
			continue
		}
		if active && fields[k] == "" {
			fields[k] = v
		}
	}
	if scanner.Err() != nil {
		return nil, failure(reasonConfig)
	}
	return fields, nil
}
func directive(line string) (string, string) {
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return line, ""
	}
	return strings.ToLower(line[:i]), strings.Trim(strings.TrimSpace(strings.TrimLeft(line[i:], " \t=")), "\"")
}

func allowedDirective(k string) bool {
	switch k {
	case "host", "hostname", "user", "port", "identityfile", "identitiesonly", "batchmode", "stricthostkeychecking", "globalknownhostsfile", "userknownhostsfile":
		return true
	}
	return false
}
func readConfined(path string, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, failure(reasonConfig)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, failure(reasonConfig)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, failure(reasonConfig)
	}
	defer func() { _ = f.Close() }()
	return readRegular(f, private)
}
func readRegular(f *os.File, private bool) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, failure(reasonConfig)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || info.Size() < 1 {
		return nil, failure(reasonConfig)
	}
	if info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0037 != 0 {
		return nil, publicerr.New(publicerr.Forbidden, "Git SSH file permissions are unsafe.", "Remove group and other write access; keep the private identity owner-readable only.", nil)
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) != int(info.Size()) {
		return nil, failure(reasonConfig)
	}
	return data, nil
}

func (c Config) address() string { return net.JoinHostPort(strings.Trim(c.Host, "[]"), c.Port) }

func (c Config) readFile(path string, private bool) ([]byte, error) {
	if c.ReadFile != nil {
		return c.ReadFile(path, private)
	}
	return readConfined(path, private)
}
