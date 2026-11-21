package argoworkflow

import (
	"errors"
	"path"
	"strings"
)

func documentationShellParameters(raw []byte) error {
	wf, err := Decode(raw)
	if err != nil {
		return err
	}
	for _, tpl := range wf.Spec.Templates {
		if tpl.Script != nil && strings.Contains(tpl.Script.Source, "{{") {
			return errors.New("parameter interpolated into shell source")
		}
		if c := tpl.Container; c != nil {
			if err := documentationShellCommand(c.Command, c.Args); err != nil {
				return err
			}
		}
	}
	return nil
}

func documentationShell(command string) bool {
	switch path.Base(command) {
	case "sh", "bash", "ash", "dash":
		return true
	default:
		return false
	}
}

func documentationShellCommand(command, args []string) error {
	words := append(append([]string{}, command...), args...)
	if len(words) > 0 && path.Base(words[0]) == "env" {
		words = words[1:]
	}
	if len(words) == 0 || !documentationShell(words[0]) {
		return nil
	}
	words = words[1:]
	for i, word := range words {
		if strings.HasPrefix(word, "-") && strings.Contains(word, "c") && i+1 < len(words) {
			if strings.Contains(words[i+1], "{{") {
				return errors.New("parameter interpolated into shell command")
			}
		}
	}
	return nil
}
