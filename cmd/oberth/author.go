package main

import (
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"errors"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/containerimage"
)

func initialize(s *flowSession, args []string) error {
	f := flowFlags("init")
	rest, err := parseFlow(f, args)
	force := flagValue[bool](f, "force")
	release := flagValue[bool](f, "release")
	if err != nil {
		return err
	}
	if !validFlowArgs("init", rest) {
		return usageProblems{"init accepts flags only"}
	}
	root, err := s.git("rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	dir := filepath.Join(root, ".oberth")
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return flowError("exists", nil)
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	name := "build.yaml"
	if *release {
		name = "release.yaml"
	}
	if err = writeStarter(dir, name, *force); err != nil {
		return err
	}
	return s.fact("wrote .oberth/%s (1 step)\nnext: git push oberth HEAD -o oberth.run=1", name)
}
func starterWorkflow() string {
	return "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nmetadata:\n  generateName: build-\nspec:\n  entrypoint: build\n  templates:\n    - name: build\n      container:\n        image: " + containerimage.Utility + "\n        command: [sh, -c]\n        args: [\"echo build passed\"]\n"
}

func readAuthored(root, path string) ([]byte, error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, authoredFailure(path, "cannot open the checkout", err)
	}
	defer opened.Close()
	path = filepath.Clean(path)
	if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return nil, authoredFailure(path, "outside the checkout", nil)
	}
	if err = authoredPath(opened, path); err != nil {
		return nil, err
	}
	file, err := opened.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, authoredFailure(path, "cannot read a regular file", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, authoredFailure(path, "cannot read a regular file", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, authoredFailure(path, "cannot read a file of at most 1 MiB", err)
	}
	return data, nil
}
func validate(s *flowSession, args []string) error {
	if !validFlowArgs("validate", args) {
		return usageProblems{"validate accepts one pipeline path"}
	}
	path := ".oberth/build.yaml"
	if len(args) == 1 {
		path = args[0]
	}
	root := s.app.directory
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	data, err := readAuthored(root, path)
	if err != nil {
		return err
	}
	return s.validateDocument(path, data)
}

func check(s *flowSession, args []string) error {
	f := flowFlags("check")
	rest, err := parseFlow(f, args)
	repo := flagValue[string](f, "repo")
	local := flagValue[string](f, "local")
	if err != nil {
		return err
	}
	if !validFlowArgs("check", rest) {
		return usageProblems{"check accepts one reference"}
	}
	if *repo == "" {
		*repo, err = s.repository()
		if err != nil {
			return err
		}
	}
	request := apicontract.RunCheckArgs{Repo: *repo}
	if len(rest) == 1 {
		request.Ref = rest[0]
	}
	if *local != "" {
		root, err := s.git("rev-parse", "--show-toplevel")
		if err != nil {
			return err
		}
		data, err := readAuthored(root, *local)
		if err != nil {
			return err
		}
		request.Files = []apicontract.RunCheckArgsFilesItem{{Path: *local, Content: string(data)}}
	}
	var result apicontract.RunCheckResult
	if err = s.invoke("run_check", request, &result); err != nil {
		return err
	}
	if !s.options.json {
		if err = s.app.render(s.last, false, s.options); err != nil {
			return err
		}
	}
	return s.fact("next: git push oberth HEAD -o oberth.run=1")
}

func writeStarter(dir, name string, force bool) error {
	opened, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer opened.Close()
	target, mode := name, os.FileMode(0644)
	if force {
		mode, err = starterMode(opened, name)
		if err != nil {
			return err
		}
		// Replace this directory entry without truncating an aliased file.
		target = "." + name + "." + rand.Text()
	}
	file, err := opened.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return flowError("exists", err)
	}
	if force {
		// Cleanup is best effort; preserve the write or rename result.
		defer func() { _ = opened.Remove(target) }()
	}
	_, writeErr := io.WriteString(file, starterWorkflow())
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if force {
		return opened.Rename(target, name)
	}
	return nil
}

func starterMode(root *os.Root, name string) (os.FileMode, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return 0644, nil
	}
	if err != nil {
		return 0, flowError("exists", err)
	}
	if !info.Mode().IsRegular() {
		problem := flowError("starter_kind", nil)
		problem.Context = []string{"File: " + filepath.Join(root.Name(), name)}
		return 0, problem
	}
	return info.Mode().Perm(), nil
}

func authoredPath(opened *os.Root, path string) error {
	parts := strings.Split(path, string(filepath.Separator))
	for i := range parts {
		info, err := opened.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return authoredFailure(path, "cannot read without symbolic links", err)
		}
	}
	return nil
}

func authoredFailure(path, reason string, cause error) error {
	if errors.Is(cause, os.ErrNotExist) {
		reason = "not found"
	}
	refusal := flowError("input", cause)
	refusal.Context = []string{"File: " + path + ": " + reason}
	return refusal
}
