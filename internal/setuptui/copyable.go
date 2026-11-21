package setuptui

import "strings"

// CopyableLines separates a labelled path, key, address or command from prose.
// A nil result leaves prose wrapping to the caller.
func CopyableLines(text string) []string {
	label, value, labelled := strings.Cut(text, ": ")
	if !labelled {
		value = text
	}
	if !copyValue(value) {
		return nil
	}
	if labelled {
		return []string{label + ":", value}
	}
	return []string{value}
}

func copyValue(value string) bool {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return false
	}
	word := fields[0]
	if strings.Contains(word, "/") || len(fields) == 1 && strings.ContainsAny(strings.TrimRight(word, ".,;!?"), ".:") {
		return true
	}
	prefix, _, _ := strings.Cut(word, "-")
	switch prefix {
	case "ssh", "ecdsa", "sk":
		return true
	}
	switch word {
	case "oberth", "git", "kubectl":
		return len(fields) > 1 && fields[1] != "/"
	}
	return strings.HasPrefix(word, "SHA256:")
}
