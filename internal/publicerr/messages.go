package publicerr

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CheckMessage checks a complete display message without changing it. Components
// use limits 40 for status reasons, 80 for refusals and 120 for remedies. Empty
// status reasons are allowed; refusal constructors require nonempty messages.
func CheckMessage(text string, limit int) error {
	if limit <= 0 || !utf8.ValidString(text) {
		return fmt.Errorf("message needs valid UTF-8 and a positive character limit")
	}
	if n := utf8.RuneCountInString(text); n > limit {
		return fmt.Errorf("message has %d characters; limit is %d", n, limit)
	}
	if strings.ContainsAny(text, "\r\n") {
		return fmt.Errorf("message must fit one line")
	}
	text = strings.TrimSpace(text)
	if strings.HasSuffix(text, "...") || strings.HasSuffix(text, "…") {
		return fmt.Errorf("message ends in an ellipsis")
	}
	for i, r := range text {
		if !strings.ContainsRune(".!?", r) {
			continue
		}
		rest := text[i+utf8.RuneLen(r):]
		next, _ := utf8.DecodeRuneInString(rest)
		if unicode.IsSpace(next) && strings.TrimSpace(rest) != "" {
			return fmt.Errorf("message must be one sentence")
		}
	}
	return nil
}
