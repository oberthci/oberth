package githubfake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cgi"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"message": message})
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var tail any
	if err := d.Decode(&tail); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func (f *Fake) control(w http.ResponseWriter, r *http.Request) {
	var value any
	var err error
	status := 400
	switch r.Method + " " + r.URL.Path {
	case "GET /_control/state":
		value, err = f.Snapshot(r.Context())
		status = 500
	case "POST /_control/seed":
		var seed Seed
		if err = decode(r, &seed); err == nil {
			err = f.ApplySeed(r.Context(), seed)
		}
		value = map[string]bool{"seeded": true}
	case "POST /_control/faults":
		var fault Fault
		if err = decode(r, &fault); err == nil {
			err = f.Inject(fault)
		}
		value = map[string]bool{"injected": true}
	default:
		f.record(r.Method, r.URL.Path, true)
		failure(w, 501, "UNEXPECTED control request")
		return
	}
	if err != nil {
		failure(w, status, err.Error())
		return
	}
	reply(w, 200, value)
}
func (f *Fake) createKey(w http.ResponseWriter, r *http.Request, name string) {
	if !authorized(w, r) {
		return
	}
	var in struct {
		Title    string `json:"title"`
		Key      string `json:"key"`
		ReadOnly bool   `json:"read_only"`
	}
	if err := decode(r, &in); err != nil {
		failure(w, 400, "invalid deploy-key JSON")
		return
	}
	key, err := parseKey(in.Key)
	if err != nil {
		failure(w, 422, "Validation Failed")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.repos[name]
	if !ok {
		failure(w, 404, "Not Found")
		return
	}
	for _, repo := range f.repos {
		for _, old := range repo {
			if old.Key == key {
				failure(w, 422, "key is already in use")
				return
			}
		}
	}
	value := DeployKey{ID: f.nextID, Key: key, Title: in.Title, ReadOnly: in.ReadOnly, Verified: true, CreatedAt: "2000-01-01T00:00:00Z", URL: fmt.Sprintf("/repos/%s/keys/%d", name, f.nextID)}
	f.nextID++
	f.repos[name] = append(f.repos[name], value)
	reply(w, 201, value)
}

var keyPath = regexp.MustCompile(`^/api/v3/repos/(` + repositoryPattern + `)/keys$`)
var gitPath = regexp.MustCompile(`^/(` + repositoryPattern + `)\.git/(info/refs|git-upload-pack)$`)

func validHTTPGit(r *http.Request) bool {
	match := gitPath.FindStringSubmatch(r.URL.Path)
	if match == nil || !validRepo(match[1]) {
		return false
	}
	switch r.Method + " " + match[2] {
	case "GET info/refs":
		return r.URL.RawQuery == "service=git-upload-pack"
	case "POST git-upload-pack":
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		return r.URL.RawQuery == "" && err == nil && strings.EqualFold(media, "application/x-git-upload-pack-request")
	}
	return false
}
func (f *Fake) httpGit(w http.ResponseWriter, r *http.Request) {
	name := gitPath.FindStringSubmatch(r.URL.Path)[1]
	f.mu.Lock()
	_, ok := f.repos[name]
	f.mu.Unlock()
	if !ok {
		failure(w, 404, "Not Found")
		return
	}
	path, err := f.git(r.Context(), "--exec-path").Output()
	if err != nil {
		failure(w, 500, "Git unavailable")
		return
	}
	if r.Method == "POST" {
		r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	}
	h := cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(path)), "git-http-backend"), Root: "/", Env: append(f.gitEnv(), "GIT_PROJECT_ROOT="+f.root, "GIT_HTTP_EXPORT_ALL=1"), Logger: f.logger}
	h.ServeHTTP(w, r)
}

// ControlHandler exposes fixture controls separately from upstream traffic.
func (f *Fake) ControlHandler() http.Handler { return http.HandlerFunc(f.control) }

func normalizeHTTPPath(path string) string {
	parts := strings.Split(path, "/")
	switch {
	case keyPath.MatchString(path):
		parts[4], parts[5] = strings.ToLower(parts[4]), strings.ToLower(parts[5])
	case len(parts) >= 4 && strings.HasSuffix(parts[2], ".git"), validAsset(path):
		parts[1], parts[2] = strings.ToLower(parts[1]), strings.ToLower(parts[2])
	}
	return strings.Join(parts, "/")
}
func authorized(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		return true
	}
	failure(w, 401, "Requires authentication")
	return false
}
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.Clone(r.Context())
	r.URL.Path = normalizeHTTPPath(r.URL.Path)
	handler := f.route(r)
	f.record(r.Method, r.URL.Path, handler == nil)
	if handler == nil {
		failure(w, 501, "UNEXPECTED request")
		return
	}
	if !f.httpFault(w, r) {
		handler(w, r)
	}
}
func (f *Fake) route(r *http.Request) http.HandlerFunc {
	switch {
	case validHTTPGit(r):
		return f.httpGit
	case r.URL.RawQuery != "":
		return nil
	case r.Method == "GET" && r.URL.Path == "/api/v3/":
		return func(w http.ResponseWriter, r *http.Request) {
			if authorized(w, r) {
				w.Header().Set("X-Oauth-Scopes", "repo, read:org, gist")
				reply(w, 200, map[string]any{})
			}
		}
	case r.Method == "POST" && keyPath.MatchString(r.URL.Path):
		name := keyPath.FindStringSubmatch(r.URL.Path)[1]
		if validRepo(name) {
			return func(w http.ResponseWriter, r *http.Request) { f.createKey(w, r, name) }
		}
	case r.Method == "GET" && validAsset(r.URL.Path):
		return f.asset
	}
	return nil
}
func (f *Fake) asset(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	body, ok := f.assets[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		failure(w, 404, "Not Found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
}

// Inject appends a finite fault. Attempts is a read-only consumption counter.
func (f *Fake) Inject(v Fault) error {
	if v.Attempts != 0 || v.Remaining < 1 || v.Path == "" || v.DelayMS < 0 || v.DelayMS > 60000 {
		return errors.New("fault requires path, positive remaining, zero attempts and delay_ms in 0..60000")
	}
	if err := validateFault(v); err != nil {
		return err
	}
	if v.Method == "GET" || v.Method == "POST" {
		v.Path = normalizeHTTPPath(v.Path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, v)
	return nil
}
func validateFault(v Fault) error {
	methods := map[string][]string{
		"rate_limit": {"GET", "POST"}, "secondary_rate_limit": {"GET", "POST"},
		"not_found":    {"GET", "POST", "git-upload-pack", "git-receive-pack"},
		"forbidden":    {"GET", "POST", "git-upload-pack", "git-receive-pack", "auth"},
		"server_error": {"GET", "POST", "git-upload-pack", "git-receive-pack"},
		"slow":         {"GET", "POST", "git-upload-pack", "git-receive-pack"},
		"drop":         {"GET", "POST", "git-upload-pack", "git-receive-pack"},
		"archived":     {"git-receive-pack"}, "protected": {"git-receive-pack"},
	}
	if !slices.Contains(methods[v.Kind], v.Method) || !validFaultStatus(v) || v.Method == "auth" && v.Path != "*" || v.Kind == "slow" && v.DelayMS == 0 {
		return errors.New("fault kind, method, status, auth path or slow delay is invalid")
	}
	return nil
}
func validFaultStatus(v Fault) bool {
	switch v.Kind {
	case "rate_limit", "secondary_rate_limit":
		return v.Status == 0 || v.Status == 403 || v.Status == 429
	case "server_error":
		return v.Status == 0 || v.Status >= 500 && v.Status <= 599
	default:
		return v.Status == 0
	}
}
func (f *Fake) takeFault(method, path string) *Fault {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.faults {
		v := &f.faults[i]
		if v.Remaining > 0 && v.Method == method && (v.Path == path || v.Path == "*") {
			v.Remaining--
			v.Attempts++
			return new(*v)
		}
	}
	return nil
}
func delay(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
func (f *Fake) httpFault(w http.ResponseWriter, r *http.Request) bool {
	v := f.takeFault(r.Method, r.URL.Path)
	if v == nil {
		return false
	}
	switch v.Kind {
	case "slow":
		return !f.delay(r.Context(), time.Duration(v.DelayMS)*time.Millisecond)
	case "drop":
		panic(http.ErrAbortHandler)
	case "rate_limit":
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
	case "secondary_rate_limit":
		w.Header().Set("Retry-After", "60")
	}
	response := map[string]struct {
		status  int
		message string
	}{
		"rate_limit": {403, "API rate limit exceeded"}, "secondary_rate_limit": {403, "You have exceeded a secondary rate limit"},
		"not_found": {404, "Not Found"}, "forbidden": {403, "Forbidden"}, "server_error": {503, "Service Unavailable"},
	}[v.Kind]
	if v.Status != 0 {
		response.status = v.Status
	}
	failure(w, response.status, response.message)
	return true
}
