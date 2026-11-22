package gitcache_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retag replaces the pushed v1.2.3 with another tag object on the same commit.
func (p *releaseTagPush) retag(t *testing.T, args ...string) {
	t.Helper()
	p.f.Git(t, p.f.Work, append([]string{"tag", "-f", "-a", "v1.2.3"}, args...)...)
	p.tag.ObjectSHA = p.f.Git(t, p.f.Work, "rev-parse", p.tag.Name)
}

func TestReleaseTagMustBeOneTagObjectOnItsCommit(t *testing.T) {
	t.Parallel()
	for name, shape := range map[string]func(*testing.T, *releaseTagPush){
		"tag of a tag": func(t *testing.T, p *releaseTagPush) {
			p.f.Git(t, p.f.Work, "tag", "-a", "inner", "-m", "inner", p.tag.CommitSHA)
			p.retag(t, "-m", "outer", "inner")
		},
		"tag of a tag naming the commit in later headers": func(t *testing.T, p *releaseTagPush) {
			p.f.Git(t, p.f.Work, "tag", "-a", "inner", "-m", "inner", p.tag.CommitSHA)
			inner := p.f.Git(t, p.f.Work, "rev-parse", "refs/tags/inner")
			body := "object " + inner + "\ntype tag\ntag v1.2.3\ntagger Oberth <ci@oberth.ci> 946684800 +0000\n" +
				"object " + p.tag.CommitSHA + "\ntype commit\n\nouter\n"
			crafted := filepath.Join(p.f.Root, "crafted")
			if err := os.WriteFile(crafted, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			p.tag.ObjectSHA = p.f.Git(t, p.f.Work, "hash-object", "-t", "tag", "-w", "--literally", crafted)
			p.f.Git(t, p.f.Work, "update-ref", p.tag.Name, p.tag.ObjectSHA)
		},
		"tag object above 64 KiB": func(t *testing.T, p *releaseTagPush) {
			message := filepath.Join(p.f.Root, "message")
			if err := os.WriteFile(message, []byte(strings.Repeat("x", 70<<10)), 0600); err != nil {
				t.Fatal(err)
			}
			p.retag(t, "-F", message, p.tag.CommitSHA)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newReleaseTagPush(t)
			shape(t, p)
			delivery, err := p.receive(t, "", p.tag)
			if err == nil || delivery == nil || !strings.Contains(delivery.Error(), "release tag must point at its commit") {
				t.Fatalf("delivery %v, receive %v", delivery, err)
			}
			if p.delivered != 0 || p.forgeTag() != "" {
				t.Fatal("a refused tag shape reached the forge or a run")
			}
		})
	}
}
