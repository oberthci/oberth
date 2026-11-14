package service

import "fmt"

// AddGrant returns a modify function that adds one grant entry, deduplicating
// against existing entries by literal string comparison.
func AddGrant(repo, workflow, step, secret string) func([]SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
	return func(existing []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
		for _, entry := range existing {
			if entry.Repo == repo && entry.Workflow == workflow && entry.Step == step && entry.Secret == secret {
				return existing, nil // already present
			}
		}
		return append(existing, SecretAccessGrantEntry{Repo: repo, Workflow: workflow, Step: step, Secret: secret}), nil
	}
}

// RemoveGrant returns a modify function that removes one grant entry by literal
// string comparison. Returns an error if the entry is not found.
func RemoveGrant(repo, workflow, step, secret string) func([]SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
	return func(existing []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
		result := make([]SecretAccessGrantEntry, 0, len(existing))
		found := false
		for _, entry := range existing {
			if entry.Repo == repo && entry.Workflow == workflow && entry.Step == step && entry.Secret == secret {
				found = true
				continue
			}
			result = append(result, entry)
		}
		if !found {
			return nil, fmt.Errorf("no grant for %s workflow %s step %s secret %s in ConfigMap", repo, workflow, step, secret)
		}
		return result, nil
	}
}
