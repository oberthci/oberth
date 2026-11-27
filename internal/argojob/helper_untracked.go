package argojob

import (
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Snapshot paths are tracked by F. Everything else in the retained checkout
// is workspace state. Quote names so a filename cannot forge a log line.
func keptUntracked(root *os.Root, files []string) string {
	list := retainedFiles{tracked: make(map[string]bool, len(files))}
	for _, name := range files {
		list.tracked[name] = true
	}
	err := fs.WalkDir(root.FS(), ".", list.visit)
	if err != nil {
		list.names = append(list.names, "(list unavailable)")
	} else if list.truncated {
		list.names = append(list.names, "(list truncated)")
	} else if len(list.names) == 0 {
		return "none"
	}
	return strings.Join(list.names, ", ")
}

type retainedFiles struct {
	tracked       map[string]bool
	names         []string
	size, visited int
	truncated     bool
}

func (list *retainedFiles) visit(name string, entry fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	if name == ".git" {
		return fs.SkipDir
	}
	list.visited++
	if list.visited > 100000 {
		list.truncated = true
		return fs.SkipAll
	}
	if entry.IsDir() || list.tracked[name] {
		return nil
	}
	quoted := strconv.Quote(name)
	if len(list.names) == 32 || list.size+len(quoted) > 4096 {
		list.truncated = true
		return fs.SkipAll
	}
	list.names, list.size = append(list.names, quoted), list.size+len(quoted)
	return nil
}
