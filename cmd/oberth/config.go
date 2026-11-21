package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

type serverFiles struct {
	SecretStoreCA    string `json:"secret_store_ca" format:"absolute-path"`
	SecretStoreToken string `json:"secret_store_token" format:"absolute-path"`
	CACertificate    string `json:"ca_certificate" format:"absolute-path"`
	CAPrivateKey     string `json:"ca_private_key" format:"absolute-path"`
	AdminPublicKey   string `json:"admin_public_key" format:"absolute-path"`
	AdminPushKey     string `json:"admin_push_key" format:"absolute-path"`
	SSHHostKey       string `json:"ssh_host_key" format:"absolute-path"`
	ForgeKey         string `json:"forge_key" format:"absolute-path"`
}

type serverConfig struct {
	Files             serverFiles `json:"files"`
	Version           int         `json:"version"`
	Namespace         string      `json:"namespace"`
	ArgoNamespace     string      `json:"argo_namespace"`
	PipelineNamespace string      `json:"pipeline_namespace"`
	Public            struct {
		URL     string `json:"url"`
		SSHPort int    `json:"ssh_port"`
	} `json:"public"`
	Listen struct {
		HTTPS  string `json:"https"`
		SSH    string `json:"ssh"`
		Source string `json:"source"`
		Health string `json:"health"`
	} `json:"listen"`
	DatabaseFile string             `json:"database_file"`
	Protocol     stateport.Protocol `json:"protocol"`
	SecretStore  struct {
		Address    string `json:"address"`
		Audience   string `json:"audience"`
		KVMount    string `json:"kv_mount"`
		AuthMount  string `json:"auth_mount"`
		WriterRole string `json:"writer_role"`
	} `json:"secret_store"`
	Runs struct {
		Parallel int `json:"parallel"`
	} `json:"runs"`
	Logs struct {
		RunLimitBytes int64 `json:"run_limit_bytes"`
	} `json:"logs"`
	Schedules struct {
		MaxEntries int `json:"max_entries"`
	} `json:"schedules"`
	Assist struct {
		TTLSeconds int `json:"ttl_seconds"`
	} `json:"assist"`
	Runtime json.RawMessage `json:"runtime"`
}

func readConfig(path string) (serverConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return serverConfig{}, serverProblem("config", path+": "+fileReason(err), err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return serverConfig{}, serverProblem("config", path+": "+fileReason(err), err)
	}
	cfg, err := decodeConfig(data)
	var public *publicerr.Error
	if errors.As(err, &public) {
		public.Detail = path + ": " + public.Detail
	}
	return cfg, err
}
func decodeConfig(data []byte) (serverConfig, error) {
	var cfg serverConfig
	if len(data) > 1<<20 {
		return cfg, serverProblem("config", "config: exceeds 1048576 bytes", nil)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, serverProblem("config", jsonProblem(err), err)
	}
	if body := bytes.TrimSpace(data); len(body) == 0 || body[0] != '{' {
		return cfg, serverProblem("config", "config: expected object", nil)
	}
	problems := configFields(data, reflect.TypeFor[serverConfig](), "")
	for _, problem := range cfg.problems() {
		key, _, _ := strings.Cut(problem, ":")
		if !missingConfigKey(problems, key) {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return cfg, serverProblem("config", strings.Join(problems, "\n"), nil)
	}
	return cfg, publicURLHost(cfg.Public.URL)
}
func configFields(raw []byte, t reflect.Type, path string) []string {
	object, problems := configObject(raw, path)
	if object == nil {
		return problems
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		child, ok := object[name]
		delete(object, name)
		key := strings.TrimPrefix(path+"."+name, ".")
		if !ok || string(child) == "null" {
			if !strings.Contains(f.Tag.Get("json"), "omitempty") {
				problems = append(problems, key+": required")
			}
			continue
		}
		if f.Tag.Get("format") == "absolute-path" {
			var value string
			_ = json.Unmarshal(child, &value)
			if !filepath.IsAbs(value) {
				problems = append(problems, key+": expected absolute file path")
			}
		}
		if f.Type.Kind() == reflect.Struct {
			problems = append(problems, configFields(child, f.Type, key)...)
		}
		if f.Type == reflect.TypeFor[json.RawMessage]() {
			problems = append(problems, configDuplicates(child, key)...)
		}
	}
	for key := range object {
		problems = append(problems, strings.TrimPrefix(path+"."+key, ".")+": unknown field")
	}
	return problems
}
func configObject(raw []byte, path string) (map[string]json.RawMessage, []string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, []string{path + ": expected object"}
	}
	object := map[string]json.RawMessage{}
	var problems []string
	for decoder.More() {
		token, _ = decoder.Token()
		key := token.(string)
		if _, found := object[key]; found {
			problems = append(problems, strings.TrimPrefix(path+"."+key, ".")+": duplicate key")
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, []string{jsonProblem(err)}
		}
		object[key] = value
	}
	return object, problems
}
func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("config: not JSON at byte %d", syntax.Offset)
	}
	var kind *json.UnmarshalTypeError
	if errors.As(err, &kind) {
		expected := kind.Type.Kind().String()
		if expected == "int" || expected == "int64" {
			expected = "integer"
		}
		field := kind.Field
		if field == "" {
			field = "config"
		}
		if expected == "struct" {
			expected = "object"
		}
		return field + ": expected " + expected
	}
	return "config: expected one JSON object"
}
func fileReason(err error) string {
	if os.IsNotExist(err) {
		return "not found"
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission denied"
	}
	return "cannot read file"
}

func (c serverConfig) problems() []string {
	var p []string
	for key, valid := range map[string]bool{
		"argo_namespace": c.ArgoNamespace != "", "version": c.Version == 1, "namespace": c.Namespace != "", "pipeline_namespace": c.PipelineNamespace != "" && c.PipelineNamespace != c.Namespace,
		"public.url": httpsURL(c.Public.URL), "public.ssh_port": c.Public.SSHPort > 0 && c.Public.SSHPort <= 65535,
		"database_file": filepath.IsAbs(c.DatabaseFile),
		"protocol":      c.Protocol.Validate() == nil, "secret_store.address": httpsURL(c.SecretStore.Address),
		"secret_store.audience": strings.TrimSpace(c.SecretStore.Audience) != "" && strings.TrimSpace(c.SecretStore.Audience) == c.SecretStore.Audience,
		"secret_store.kv_mount": c.SecretStore.KVMount != "", "secret_store.auth_mount": c.SecretStore.AuthMount != "", "secret_store.writer_role": c.SecretStore.WriterRole != "",
		"runs.parallel": c.Runs.Parallel >= 0, "logs.run_limit_bytes": c.Logs.RunLimitBytes > 0,
		"schedules.max_entries": c.Schedules.MaxEntries >= 0 && c.Schedules.MaxEntries <= 256,
		"assist.ttl_seconds":    c.Assist.TTLSeconds > 0, "runtime": len(c.Runtime) > 0 && c.Runtime[0] == '{',
	} {
		if !valid {
			p = append(p, key+": invalid value")
		}
	}
	for key, address := range map[string]string{"listen.https": c.Listen.HTTPS, "listen.ssh": c.Listen.SSH, "listen.source": c.Listen.Source, "listen.health": c.Listen.Health} {
		if !listenAddress(address) {
			p = append(p, key+": invalid address")
		}
	}
	return p
}

func listenAddress(address string) bool {
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return false
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number <= 65535
}

func httpsURL(text string) bool {
	u, err := url.Parse(text)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func serverError(key string, cause error) *publicerr.Error {
	m := serverMessages()[key]
	return publicerr.New(publicerr.Unavailable, m[0], m[1], cause)
}
func missingPart(name string) error {
	e := serverError("part", nil)
	e.Detail = fmt.Sprintf("Required part: %s", name)
	return e
}
func serverMessages() map[string][2]string {
	return map[string][2]string{
		"public-host": {"Public URL host must use ASCII.", "Set public.url to the punycode address shown below and restart."},
		"config":      {"Server configuration is invalid.", "Fix each listed value in the config file and restart."},
		"part":        {"A required server part is missing.", "Use a build containing every configured component."},
		"shutdown":    {"Server shutdown exceeded a part's budget.", "Check the named part; its dependencies were left open."},
		"cache":       {"Git maintenance failed.", "The server will try Git maintenance again at the next hourly tick."},
		"retention":   {"Event retention failed.", "The server will try retention again at the next daily tick."},
		"stopped":     {"A server background part stopped.", "Check the server diagnostics and restart."},
		"starting":    {"Server is starting.", "Wait for startup to finish, then retry."},
		"unready":     {"Server is not ready.", "Check the database, certificate and background parts."},
		"tls":         {"Server CA is invalid.", "Check the mounted CA certificate and matching private key."},
		"admin":       {"Administrator key is invalid.", "Rerun oberth install to write the administrator public key Secret."},
	}
}

func serverProblem(key, detail string, cause error) *publicerr.Error {
	e := serverError(key, cause)
	e.Detail = detail
	return e
}

func configDuplicates(raw []byte, path string) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var problems []string
	switch raw[0] {
	case '{':
		object, issues := configObject(raw, path)
		problems = append(problems, issues...)
		for key, value := range object {
			problems = append(problems, configDuplicates(value, path+"."+key)...)
		}
	case '[':
		var values []json.RawMessage
		_ = json.Unmarshal(raw, &values)
		for i, value := range values {
			problems = append(problems, configDuplicates(value, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return problems
}
func missingConfigKey(problems []string, key string) bool {
	for _, problem := range problems {
		if !strings.HasSuffix(problem, ": required") {
			continue
		}
		missing := strings.TrimSuffix(problem, ": required")
		if key == missing || strings.HasPrefix(key, missing+".") {
			return true
		}
	}
	return false
}
