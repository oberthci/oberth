package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	"go.yaml.in/yaml/v3"
)

//go:embed skills/oberth-ci/SKILL.md skills/oberth-ci/references/*.md
var ciSkillFiles embed.FS

type skillFile struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}

type skillEntry struct {
	URI         string         `json:"uri"`
	Frontmatter map[string]any `json:"frontmatter"`
	Resources   []skillFile    `json:"resources"`
}

type ciSkill struct {
	entry skillEntry
	files map[string]string
}

func loadCISkill(version string) (ciSkill, error) {
	root := "skill://oberth/" + url.PathEscape(version) + "/oberth-ci/"
	out := ciSkill{entry: skillEntry{URI: root + "SKILL.md"}, files: map[string]string{}}
	for _, file := range []string{"SKILL.md", "references/fix-resume.md", "references/display.md"} {
		raw, err := ciSkillFiles.ReadFile("skills/oberth-ci/" + file)
		if err != nil {
			return ciSkill{}, err
		}
		text := strings.ReplaceAll(string(raw), "{{server-version}}", strconv.Quote(version))
		uri := root + file
		out.files[uri] = text
		digest := sha256.Sum256([]byte(text))
		out.entry.Resources = append(out.entry.Resources, skillFile{uri, "sha256:" + hex.EncodeToString(digest[:]), len(text)})
		if file == "SKILL.md" {
			_, rest, ok := strings.Cut(text, "---\n")
			front, _, closed := strings.Cut(rest, "\n---\n")
			if !ok || !closed {
				return ciSkill{}, refusal("registry")
			}
			if err := yaml.Unmarshal([]byte(front), &out.entry.Frontmatter); err != nil {
				return ciSkill{}, err
			}
		}
	}
	return out, nil
}

func (s *Server) skillRPC(w http.ResponseWriter, r *http.Request, x rpcExchange) {
	var params struct {
		apicontract.MCPParams
		URI    string `json:"uri,omitempty"`
		Cursor string `json:"cursor,omitempty"`
	}
	resp := x.resp
	if decode(x.req.Params, &params, apicontract.RequestBytes) != nil || params.Cursor != "" {
		resp.Error = &rpcError{-32602, refusal("input").Reason(), refusal("input")}
	} else if x.req.Method == "skills/list" {
		resp.Result = map[string]any{"skills": []skillEntry{s.skill.entry}, "ttlMs": 0, "cacheScope": "public"}
	} else if params.URI == s.skill.entry.URI {
		resp.Result = map[string]any{"skill": s.skill.entry, "ttlMs": 0, "cacheScope": "public"}
	} else {
		resp.Error = &rpcError{-32602, refusal("skill").Reason(), refusal("skill")}
	}
	s.writeRPC(w, r, resp)
}

func (s *Server) skillResources() []any {
	out := make([]any, 0, len(s.skill.entry.Resources))
	for _, file := range s.skill.entry.Resources {
		name := strings.TrimPrefix(file.URI, strings.TrimSuffix(s.skill.entry.URI, "SKILL.md"))
		if file.URI == s.skill.entry.URI {
			name = "oberth-ci"
		}
		out = append(out, map[string]any{"uri": file.URI, "name": name, "mimeType": "text/markdown"})
	}
	return out
}

func (s *Server) readSkill(w http.ResponseWriter, r *http.Request, x rpcExchange, uri string) bool {
	text, ok := s.skill.files[uri]
	if !ok && !strings.HasPrefix(uri, "skill:") {
		return false
	}
	resp := x.resp
	if ok {
		resp.Result = map[string]any{"contents": []any{map[string]any{"uri": uri, "mimeType": "text/markdown", "text": text}}, "ttlMs": 0, "cacheScope": "public"}
	} else {
		resp.Error = &rpcError{-32602, refusal("skill").Reason(), refusal("skill")}
	}
	s.writeRPC(w, r, resp)
	return true
}
