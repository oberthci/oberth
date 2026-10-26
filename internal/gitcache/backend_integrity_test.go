package gitcache

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/storage/memory"
)

func rawGoObject(t *testing.T, r *git.Repository, kind plumbing.ObjectType, body []byte) plumbing.Hash {
	t.Helper()
	object := r.Storer.NewEncodedObject()
	object.SetType(kind)
	object.SetSize(int64(len(body)))
	writer, err := object.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	hash, err := r.Storer.SetEncodedObject(object)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
func rawTreeEntry(mode, name string, hash plumbing.Hash) []byte {
	return append([]byte(mode+" "+name+"\x00"), hash[:]...)
}
func testCommitBody(tree plumbing.Hash) string {
	return "tree " + tree.String() + "\nauthor Fixture <fixture@example.invalid> 1600000000 +0000\ncommitter Fixture <fixture@example.invalid> 1600000000 +0000\n\nfixture\n"
}

func TestGoGraphRejectsMalformedMetadata(t *testing.T) {
	for _, name := range []string{"valid", "tree order", "directory tree order", "missing author", "bad parent hash", "bad tree hash", "bad identity", "bad timestamp", "duplicate author", "unterminated headers", "bad tag type", "bad tag hash", "bad tag name", "bad tagger", "tag type mismatch", "gitmodules syntax", "gitmodules URL", "gitmodules duplicate URL", "gitmodules path", "gitmodules traversal", "gitmodules alias path", "gitmodules name", "gitmodules update", "gitmodules symlink", "valid modules", ".git", "GIT~1", ".git.", ".git ", ".git::$INDEX_ALLOCATION", "x\\.git", ".g\u200cit", "\ufeff.git"} {
		t.Run(name, func(t *testing.T) {
			r, err := git.Init(memory.NewStorage(), nil)
			if err != nil {
				t.Fatal(err)
			}
			blob := rawGoObject(t, r, plumbing.BlobObject, []byte("blob"))
			treeBody := rawTreeEntry("100644", "file", blob)
			switch name {
			case "tree order":
				treeBody = append(rawTreeEntry("100644", "z", blob), rawTreeEntry("100644", "a", blob)...)
			case "directory tree order":
				empty := rawGoObject(t, r, plumbing.TreeObject, nil)
				treeBody = append(rawTreeEntry("40000", "a", empty), rawTreeEntry("100644", "a.c", blob)...)
			default:
				if strings.HasPrefix(name, ".git") || strings.HasPrefix(name, "GIT~") || strings.Contains(name, "\\") || strings.ContainsAny(name, "\u200c\ufeff") {
					treeBody = rawTreeEntry("100644", name, blob)
				}
			}
			if strings.HasPrefix(name, "gitmodules") || name == "valid modules" {
				module := "[submodule \"lib\"]\n path = lib\n url = ../project.git\n"
				switch name {
				case "gitmodules syntax":
					module = "[submodule broken"
				case "gitmodules URL":
					module += " url = -uhoh\n"
				case "gitmodules duplicate URL":
					module += " url = ext::command\n url = https://example.invalid/safe\n"
				case "gitmodules path":
					module += " path = -option\n"
				case "gitmodules traversal":
					module += " path = ../outside\n"
				case "gitmodules alias path":
					module += " path = GIT~1/hooks\n"
				case "gitmodules name":
					module = "[submodule \"../evil\"]\n path = lib\n url = ../project.git\n"
				case "gitmodules update":
					module += " update = !command\n"
				}
				blob = rawGoObject(t, r, plumbing.BlobObject, []byte(module))
				mode := "100644"
				if name == "gitmodules symlink" {
					mode = "120000"
				}
				treeBody = rawTreeEntry(mode, ".gitmodules", blob)
			}
			tree := rawGoObject(t, r, plumbing.TreeObject, treeBody)
			body := testCommitBody(tree)
			switch name {
			case "missing author":
				body = strings.Replace(body, "author Fixture <fixture@example.invalid> 1600000000 +0000\n", "", 1)
			case "bad parent hash":
				body = strings.Replace(body, "\nauthor ", "\nparent malformed\nauthor ", 1)
			case "bad tree hash":
				body = strings.Replace(body, tree.String(), strings.Repeat("z", 40), 1)
			case "bad identity":
				body = strings.Replace(body, "Fixture <fixture@example.invalid> 1600000000 +0000", "malformed", 1)
			case "bad timestamp":
				body = strings.Replace(body, "1600000000", "not-a-timestamp", 1)
			case "duplicate author":
				body = strings.Replace(body, "\n\nfixture", "\nauthor Fixture <a@b> 1600000000 +0000\n\nfixture", 1)
			case "unterminated headers":
				body = strings.Split(body, "\n\n")[0]
			}
			root := tree
			if strings.Contains(name, "author") || strings.Contains(name, "hash") || strings.Contains(name, "identity") || strings.Contains(name, "timestamp") || strings.Contains(name, "headers") {
				root = rawGoObject(t, r, plumbing.CommitObject, []byte(body))
			}
			if strings.Contains(name, "tag") && !strings.HasPrefix(name, "gitmodules") {
				tag := "object " + blob.String() + "\ntype blob\ntag fixture\ntagger Fixture <a@b> 1600000000 +0000\n\nmessage\n"
				switch name {
				case "bad tag type":
					tag = strings.Replace(tag, "type blob", "type nonsense", 1)
				case "bad tag hash":
					tag = strings.Replace(tag, blob.String(), strings.Repeat("z", 40), 1)
				case "bad tag name":
					tag = strings.Replace(tag, "tag fixture", "tag bad..name", 1)
				case "bad tagger":
					tag = strings.Replace(tag, "tagger Fixture <a@b> 1600000000 +0000", "tagger bad", 1)
				case "tag type mismatch":
					tag = strings.Replace(tag, "type blob", "type commit", 1)
				}
				root = rawGoObject(t, r, plumbing.TagObject, []byte(tag))
			}
			err = validateGoGraph(t.Context(), r, []plumbing.Hash{root}, nil)
			valid := name == "valid" || name == "valid modules"
			if valid && err != nil {
				t.Fatal(err)
			}
			if !valid && err == nil {
				t.Fatal("malformed object graph accepted")
			}
		})
	}
}

func TestMissingSubtreeRefusesMergeAndArchive(t *testing.T) {
	repository := newTestRepository(t)
	if err := os.Mkdir(filepath.Join(repository.work, "foo"), 0o700); err != nil {
		t.Fatal(err)
	}
	repository.initialSHA = repository.commitFile(t, "nested baseline", "foo/file", "nested\n")
	source := repository.featureCommit(t, "source", "source\n")
	repository.checkout(t, "main")
	target := repository.commitFile(t, "target", "target", "target\n")
	subtree := runGit(t, repository.work, "rev-parse", "HEAD:foo")
	if err := os.Remove(filepath.Join(repository.work, ".git", "objects", subtree[:2], subtree[2:])); err != nil {
		t.Fatal(err)
	}
	r, closeStorage, err := openGoRepository(repository.work)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStorage()
	if _, err := flattenMergeTree(t.Context(), r, plumbing.NewHash(target)); err == nil {
		t.Fatal("missing subtree silently flattened")
	}
	var output bytes.Buffer
	if err := goArchive(t.Context(), r, "HEAD", &output); err == nil {
		t.Fatal("missing subtree silently archived")
	}
	cache := &Cache{timeout: time.Second}
	if err := cache.mergePromotionTrees(t.Context(), repository.work, source, "main"); err == nil {
		t.Fatal("missing subtree silently merged")
	}
	if head := runGit(t, repository.work, "rev-parse", "HEAD"); head != target {
		t.Fatal("failed merge moved HEAD")
	}
}

func TestReceiveQuarantineRejectsObjectsBeforePublication(t *testing.T) {
	for _, scenario := range []string{"graph", "policy", "truncated", "orphan metadata", "stale", "success"} {
		t.Run(scenario, func(t *testing.T) {
			repository := newTestRepository(t)
			cache := newTestCache(t, repository.upstream)
			if cache.backendName != BackendGoGit {
				t.Skip("Go object-store quarantine")
			}
			ready, err := cache.Ensure(t.Context(), "example")
			if err != nil {
				t.Fatal(err)
			}
			r, closeStorage, err := openGoRepository(ready.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeStorage()
			initial, err := r.CommitObject(plumbing.NewHash(repository.initialSHA))
			if err != nil {
				t.Fatal(err)
			}
			source, err := git.Init(memory.NewStorage(), nil)
			if err != nil {
				t.Fatal(err)
			}
			body := testCommitBody(initial.TreeHash)
			if scenario == "graph" {
				body = strings.Replace(body, "author Fixture <fixture@example.invalid> 1600000000 +0000\n", "", 1)
			}
			candidate := rawGoObject(t, source, plumbing.CommitObject, []byte(body))
			hashes := []plumbing.Hash{candidate}
			if scenario == "orphan metadata" {
				hashes = append(hashes, rawGoObject(t, source, plumbing.TagObject, []byte("malformed\n\n")))
			}
			var pack bytes.Buffer
			if _, err := packfile.NewEncoder(&pack, source.Storer, false).Encode(hashes, 0); err != nil {
				t.Fatal(err)
			}
			content := pack.Bytes()
			if scenario == "truncated" {
				content = content[:len(content)-8]
			}
			name := plumbing.ReferenceName("refs/heads/feature/quarantine")
			if scenario == "policy" {
				name = "refs/oberth/adversarial"
			}
			request := packp.NewReferenceUpdateRequest()
			request.Commands = []*packp.Command{{Name: name, New: candidate}}
			if scenario == "stale" {
				request.Commands[0].Old = plumbing.NewHash(repository.initialSHA)
			}
			request.Packfile = io.NopCloser(bytes.NewReader(content))
			report, err := cache.backend.(*goGitBackend).receiveGoPack(context.Background(), r, request, io.Discard)
			if scenario == "success" {
				if err != nil || report.UnpackStatus != "ok" {
					t.Fatalf("receive=%v %+v", err, report)
				}
				if err := r.Storer.HasEncodedObject(candidate); err != nil {
					t.Fatal("accepted object not promoted", err)
				}
				if ref, err := r.Reference(name, true); err != nil || ref.Hash() != candidate {
					t.Fatalf("accepted ref=%v %v", ref, err)
				}
			} else {
				if err == nil {
					t.Fatalf("rejected receive succeeded: %+v", report)
				}
				if err := r.Storer.HasEncodedObject(candidate); err == nil {
					t.Fatal("rejected object escaped quarantine")
				}
				if _, err := r.Reference(name, true); err == nil {
					t.Fatal("rejected ref published")
				}
			}
			files, err := os.ReadDir(filepath.Join(ready.Path, "objects"))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if strings.HasPrefix(file.Name(), "oberth-incoming-") {
					t.Fatal("quarantine not cleaned")
				}
			}
		})
	}
}
