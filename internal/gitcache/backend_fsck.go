package gitcache

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	format "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const maxGitMetadataBytes = 1 << 20

// Validate the raw headers before go-git's tolerant object decoders run.
// Required order/identity grammar follows Git's fsck.c, not Decode's defaults.
func validateGoHeaders(ctx context.Context, encoded plumbing.EncodedObject) (result error) {
	reader, err := encoded.Reader()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, reader.Close()) }()
	scan := bufio.NewReader(io.LimitReader(contextReader{ctx, reader}, maxGitMetadataBytes+1))
	headers := []string{}
	total := 0
	for {
		line, err := scan.ReadString('\n')
		total += len(line)
		if total > maxGitMetadataBytes {
			return errors.New("git object headers exceed limit")
		}
		if err != nil {
			return fmt.Errorf("unterminated Git object headers: %w", err)
		}
		if strings.ContainsAny(line, "\x00\r") {
			return errors.New("invalid control in Git object header")
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break
		}
		headers = append(headers, line)
	}
	hashLine := func(line, prefix string) bool {
		if !strings.HasPrefix(line, prefix) || len(line) != len(prefix)+40 {
			return false
		}
		_, err := hex.DecodeString(line[len(prefix):])
		return err == nil
	}
	var extra []string
	if encoded.Type() == plumbing.CommitObject {
		if len(headers) < 3 || !hashLine(headers[0], "tree ") {
			return errors.New("invalid commit tree header")
		}
		i := 1
		for i < len(headers) && strings.HasPrefix(headers[i], "parent ") {
			if !hashLine(headers[i], "parent ") {
				return errors.New("invalid commit parent header")
			}
			i++
		}
		if i+1 >= len(headers) || !strings.HasPrefix(headers[i], "author ") || !validGitIdentity(strings.TrimPrefix(headers[i], "author ")) {
			return errors.New("invalid commit author header")
		}
		i++
		if !strings.HasPrefix(headers[i], "committer ") || !validGitIdentity(strings.TrimPrefix(headers[i], "committer ")) {
			return errors.New("invalid commit committer header")
		}
		extra = headers[i+1:]
	} else {
		if len(headers) < 3 || !hashLine(headers[0], "object ") {
			return errors.New("invalid tag object header")
		}
		if !strings.HasPrefix(headers[1], "type ") {
			return errors.New("missing tag type header")
		}
		kind, err := plumbing.ParseObjectType(strings.TrimPrefix(headers[1], "type "))
		if err != nil || kind < plumbing.CommitObject || kind > plumbing.TagObject {
			return errors.New("invalid tag type header")
		}
		if !strings.HasPrefix(headers[2], "tag ") || plumbing.ReferenceName("refs/tags/"+strings.TrimPrefix(headers[2], "tag ")).Validate() != nil {
			return errors.New("invalid tag name header")
		}
		i := 3
		if i < len(headers) && strings.HasPrefix(headers[i], "tagger ") {
			if !validGitIdentity(strings.TrimPrefix(headers[i], "tagger ")) {
				return errors.New("invalid tagger header")
			}
			i++
		}
		extra = headers[i:]
	}
	continuation := false
	for _, line := range extra {
		if strings.HasPrefix(line, " ") {
			if !continuation {
				return errors.New("orphan Git header continuation")
			}
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok || name == "" || value == "" {
			return errors.New("malformed optional Git header")
		}
		for _, c := range name {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				if c != '-' {
					return errors.New("invalid Git header name")
				}
			}
		}
		switch name {
		case "tree", "parent", "author", "committer", "object", "type", "tag", "tagger":
			return errors.New("duplicate or out-of-order Git header")
		}
		continuation = true
	}
	return nil
}
func validGitIdentity(value string) bool {
	left := strings.Index(value, " <")
	if left < 0 || strings.ContainsAny(value[:left], "<>") {
		return false
	}
	right := strings.Index(value[left+2:], ">")
	if right < 0 {
		return false
	}
	right += left + 2
	if strings.ContainsAny(value[left+2:right], "<>") {
		return false
	}
	fields := strings.Split(value[right+1:], " ")
	if len(fields) != 3 || fields[0] != "" {
		return false
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return false
	}
	zone := fields[2]
	if len(zone) != 5 || (zone[0] != '+' && zone[0] != '-') {
		return false
	}
	for _, c := range zone[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func portableGitComponent(name string) string {
	// HFS+ ignores these Unicode codepoints when resolving names. NTFS also
	// ignores trailing spaces/dots and treats ':' as an alternate-data stream.
	return strings.Map(func(c rune) rune {
		if (c >= 0x200c && c <= 0x200f) || (c >= 0x202a && c <= 0x202e) || (c >= 0x206a && c <= 0x206f) || c == 0xfeff {
			return -1
		}
		return c
	}, name)
}
func gitMetadataAlias(name string) bool {
	for _, part := range strings.Split(name, "\\") {
		hfs := portableGitComponent(part)
		if strings.EqualFold(hfs, ".git") {
			return true
		}
		base, _, _ := strings.Cut(part, ":")
		base = strings.TrimRight(base, " .")
		if strings.EqualFold(base, ".git") || strings.EqualFold(base, "git~1") {
			return true
		}
	}
	return false
}
func isGitmodulesName(name string) bool {
	base, _, _ := strings.Cut(portableGitComponent(name), ":")
	base = strings.TrimRight(base, " .")
	return strings.EqualFold(base, ".gitmodules") || strings.EqualFold(base, "gitmod~1")
}
func validateGoGitmodules(ctx context.Context, r *git.Repository, entry object.TreeEntry) (result error) {
	blob, err := r.BlobObject(entry.Hash)
	if err != nil {
		return err
	}
	if blob.Size > maxGitMetadataBytes {
		return errors.New(".gitmodules exceeds size limit")
	}
	reader, err := blob.Reader()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, reader.Close()) }()
	cfg := format.New()
	if err := format.NewDecoder(io.LimitReader(contextReader{ctx, reader}, maxGitMetadataBytes+1)).Decode(cfg); err != nil {
		return fmt.Errorf("invalid .gitmodules syntax: %w", err)
	}
	for _, section := range cfg.Sections {
		if !section.IsName("submodule") {
			continue
		}
		for _, sub := range section.Subsections {
			module := config.Submodule{Name: sub.Name, Path: "module", URL: "https://example.invalid/module"}
			if err := module.Validate(); err != nil {
				return fmt.Errorf("invalid .gitmodules name: %w", err)
			}
			for _, option := range sub.Options {
				value := option.Value
				switch strings.ToLower(option.Key) {
				case "path":
					if !safeSubmodulePath(value) {
						return errors.New("unsafe .gitmodules path")
					}
				case "url":
					decoded, err := url.PathUnescape(value)
					if err != nil || value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(decoded, "-") || strings.ContainsAny(decoded, "\x00\r\n") || strings.HasPrefix(strings.ToLower(decoded), "ext::") {
						return errors.New("unsafe .gitmodules URL")
					}
				case "update":
					if strings.HasPrefix(value, "!") {
						return errors.New("unsafe .gitmodules update command")
					}
				}
			}
		}
	}
	return nil
}
func safeSubmodulePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") || strings.ContainsAny(value, "\x00\r\n:") {
		return false
	}
	for _, part := range strings.FieldsFunc(value, func(c rune) bool { return c == '/' || c == '\\' }) {
		clean := strings.TrimRight(portableGitComponent(part), " ")
		if clean == "." || clean == ".." || gitMetadataAlias(part) {
			return false
		}
	}
	return true
}
