package service

import "fmt"

func fixtureAddGrant(repo, step, secret string) func([]SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
	return func(existing []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
		for _, entry := range existing {
			if entry.Repo == repo && entry.Step == step && entry.Secret == secret {
				return existing, nil // already present
			}
		}
		return append(existing, SecretAccessGrantEntry{Repo: repo, Step: step, Secret: secret}), nil
	}
}

func fixtureRemoveGrant(repo, step, secret string) func([]SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
	return func(existing []SecretAccessGrantEntry) ([]SecretAccessGrantEntry, error) {
		result := make([]SecretAccessGrantEntry, 0, len(existing))
		found := false
		for _, entry := range existing {
			if entry.Repo == repo && entry.Step == step && entry.Secret == secret {
				found = true
				continue
			}
			result = append(result, entry)
		}
		if !found {
			return nil, fmt.Errorf("no grant for %s/%s/%s in ConfigMap", repo, step, secret)
		}
		return result, nil
	}
}
