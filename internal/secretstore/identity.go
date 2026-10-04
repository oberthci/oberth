package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

var identitySegment = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)

// IdentityPath is separate from all repository-controlled release paths.
func IdentityPath(namespace, name string) (string, error) {
	for _, part := range []string{namespace, name} {
		if !identitySegment.MatchString(part) || len(part) > 253 {
			return "", errors.New("invalid identity store namespace or name")
		}
	}
	return "oberth/data/identities/" + namespace + "/" + name, nil
}

// ReadIdentity returns nil for a never-created identity. Deleted versions are
// errors: loss of identity must never silently rotate a key.
func (client *Client) ReadIdentity(ctx context.Context, namespace, name string) (map[string][]byte, int, error) {
	path, err := IdentityPath(namespace, name)
	if err != nil {
		return nil, 0, err
	}
	session, err := client.login(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer client.logout(ctx, session)
	response, err := session.Logical().ReadWithContext(ctx, path)
	if err != nil {
		return nil, 0, sanitizeSecretStoreHTTPError("identity read", err)
	}
	if response == nil {
		return nil, 0, nil
	}
	data, ok := response.Data["data"].(map[string]any)
	if !ok || len(data) == 0 {
		return nil, 0, errors.New("identity is deleted or invalid; restore it explicitly")
	}
	metadata, ok := response.Data["metadata"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("identity version metadata is missing")
	}
	version, err := strconv.Atoi(fmt.Sprint(metadata["version"]))
	if err != nil || version < 1 {
		return nil, 0, errors.New("invalid identity version")
	}
	values := make(map[string][]byte, len(data))
	total := 0
	for key, raw := range data {
		value, ok := raw.(string)
		total += len(value)
		if !ok || len(data) > 64 || total > 1<<20 {
			clearKVValues(values)
			return nil, 0, errors.New("invalid or oversized identity data")
		}
		values[key] = []byte(value)
	}
	return values, version, nil
}

// WriteIdentity requires an exact version, including zero for first creation.
// Concurrent installers cannot overwrite another installer's identity.
func (client *Client) WriteIdentity(ctx context.Context, namespace, name string, version int, values map[string][]byte) error {
	path, err := IdentityPath(namespace, name)
	if err != nil {
		return err
	}
	if version < 0 || len(values) == 0 || len(values) > 64 {
		return errors.New("invalid identity write")
	}
	data := make(map[string]string, len(values))
	total := 0
	for key, value := range values {
		total += len(value)
		data[key] = string(value)
	}
	if total > 1<<20 {
		return errors.New("identity data exceeds size limit")
	}
	session, err := client.login(ctx)
	if err != nil {
		return err
	}
	defer client.logout(ctx, session)
	_, err = session.Logical().WriteWithContext(ctx, path, map[string]any{
		"data": data, "options": map[string]any{"cas": json.Number(strconv.Itoa(version))},
	})
	if err != nil {
		return sanitizeSecretStoreHTTPError("identity write", err)
	}
	return nil
}
