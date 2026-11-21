package secretstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"net/http"
	"strings"
)

// Writer satisfies the service's Put/Delete capability; it cannot read secrets.
type Writer struct {
	Client *Client
	Config Config
	Token  func(context.Context) ([]byte, error)
}

func (c *Client) login(ctx context.Context, jwt []byte, mount, role string) ([]byte, error) {
	if len(jwt) == 0 || len(jwt) > 64<<10 || !validName(mount) || !validName(role) {
		return nil, refuse("token", nil)
	}
	var result struct {
		Auth struct {
			Token string `json:"client_token"`
		} `json:"auth"`
	}
	_, err := c.JSON(ctx, Request{Method: "POST", Path: "/auth/" + mount + "/login", Body: map[string]string{"jwt": string(jwt), "role": role}}, &result)
	if err == nil && result.Auth.Token == "" {
		err = refuse("response", nil)
	}
	return []byte(result.Auth.Token), err
}
func (c *Client) Revoke(ctx context.Context, token []byte) error {
	_, err := c.JSON(ctx, Request{Method: "POST", Path: "/auth/token/revoke-self", Token: token}, nil)
	return err
}
func (w Writer) Put(ctx context.Context, account, path string, value []byte) error {
	if len(value) > MaxValueBytes {
		return refuse("limit", nil)
	}
	return w.write(ctx, account, path, map[string]string{"value": base64.StdEncoding.EncodeToString(value)})
}
func (w Writer) Delete(ctx context.Context, account, path string) error {
	return w.write(ctx, account, path, nil)
}
func (w Writer) write(ctx context.Context, account, path string, value any) (err error) {
	if !validName(account) || !workflowpolicy.ValidSecretPath(path) || w.Client == nil || w.Token == nil {
		return refuse("path", nil)
	}
	if err = w.Config.Validate(); err != nil {
		return err
	}
	jwt, err := w.Token(ctx)
	if err != nil {
		return err
	}
	defer clear(jwt)
	token, err := w.Client.login(ctx, jwt, w.Config.AuthMount, w.Config.WriterRole)
	if err != nil {
		return err
	}
	defer clear(token)
	defer func() { _ = joinRevoke(ctx, w.Client, token, nil) }()
	method, accepted := "POST", 0
	if value == nil {
		method, accepted = "DELETE", http.StatusNotFound
	}
	_, err = w.Client.JSON(ctx, Request{Method: method, Path: "/" + w.Config.Mount + "/steps/" + account + "/" + path, Token: token, Body: value, Accept: accepted}, nil)
	return err
}
func account(jwt []byte, namespace string) (string, error) {
	parts := strings.Split(string(jwt), ".")
	if len(parts) != 3 {
		return "", refuse("token", nil)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", refuse("token", nil)
	}
	defer clear(raw)
	var claim struct {
		Subject string `json:"sub"`
	}
	if json.Unmarshal(raw, &claim) != nil {
		return "", refuse("token", nil)
	}
	prefix := "system:serviceaccount:" + namespace + ":"
	name := strings.TrimPrefix(claim.Subject, prefix)
	if name == claim.Subject || !validName(name) {
		return "", refuse("token", nil)
	}
	return name, nil
}
func (c *Client) Fetch(ctx context.Context, cfg Config, jwt []byte, paths []string) (values map[string][]byte, err error) {
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	name, err := account(jwt, cfg.PipelineNamespace)
	if err != nil {
		return nil, err
	}
	if err := checkPaths(paths); err != nil {
		return nil, err
	}
	values = map[string][]byte{}
	defer func() {
		if err != nil {
			for _, v := range values {
				clear(v)
			}
			values = nil
		}
	}()
	token, err := c.login(ctx, jwt, cfg.AuthMount, cfg.StepRole)
	if err != nil {
		return nil, err
	}
	defer clear(token)
	defer func() { err = joinRevoke(ctx, c, token, err) }()
	for _, path := range paths {
		value, e := c.fetchOne(ctx, token, "/"+cfg.Mount+"/steps/"+name+"/"+path)
		if e != nil {
			return values, e
		}
		values[path] = value
	}
	return values, nil
}

func (c *Client) fetchOne(ctx context.Context, token []byte, path string) ([]byte, error) {
	var r struct {
		Data struct {
			Value *string `json:"value"`
		} `json:"data"`
	}
	if _, err := c.JSON(ctx, Request{Method: "GET", Path: path, Token: token}, &r); err != nil {
		return nil, err
	}
	if r.Data.Value == nil {
		return nil, refuse("response", nil)
	}
	value, err := base64.StdEncoding.Strict().DecodeString(*r.Data.Value)
	if err != nil || len(value) > MaxValueBytes {
		clear(value)
		return nil, refuse("limit", err)
	}
	return value, nil
}

func checkPaths(paths []string) error {
	if len(paths) == 0 || len(paths) > 32 {
		return refuse("limit", nil)
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if !workflowpolicy.ValidSecretPath(path) || seen[path] {
			return refuse("path", nil)
		}
		seen[path] = true
	}
	return nil
}
