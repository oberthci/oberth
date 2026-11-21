package apicontract

import "slices"

// grantSelectors are the access arguments that select a grant, in the order a
// refusal names them.
func grantSelectors() []string {
	return []string{"repo", "workflow", "step", "secret", "audience", "claim", "tier"}
}

// GrantArguments is the one table of the selectors each grant kind takes: all
// of them are required and every other selector is refused. An omitted kind
// is store_path; an unknown kind has no row.
func GrantArguments(kind string) ([]string, bool) {
	switch kind {
	case "", "store_path", "secret", "configmap", "image_pull_secret", "device":
		return []string{"repo", "workflow", "step", "secret"}, true
	case "identity":
		return []string{"repo", "workflow", "step", "secret", "audience"}, true
	case "pvc":
		return []string{"repo", "claim", "tier"}, true
	}
	return nil, false
}

// GrantArgumentProblem names the first selector the kind needs and given
// lacks (missing true), or the first given selector the kind does not take
// (missing false). It returns "" when the selectors fit or the kind is unknown.
func GrantArgumentProblem(kind string, given func(string) bool) (name string, missing bool) {
	takes, known := GrantArguments(kind)
	if !known {
		return "", false
	}
	for _, selector := range grantSelectors() {
		switch wanted := slices.Contains(takes, selector); {
		case wanted && !given(selector):
			return selector, true
		case !wanted && given(selector):
			return selector, false
		}
	}
	return "", false
}
