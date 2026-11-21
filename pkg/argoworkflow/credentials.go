package argoworkflow

import (
	"path"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	corev1 "k8s.io/api/core/v1"
)

// UsesCredentials reports declaration intent, never evidence that a secret was read.
func UsesCredentials(t *wfv1.Template) bool {
	if t == nil {
		return false
	}
	return declaredSecretPaths(t) != nil || projectsToken(t)
}
func projectsToken(t *wfv1.Template) bool {
	return slices.ContainsFunc(t.Volumes, tokenProjection)
}

// Only a main container can call the helper and read a delivered file.
func readsSecrets(t *wfv1.Template) bool {
	return slices.ContainsFunc(containers(t), func(c containerAt) bool { return c.main })
}
func secretCommand(t *wfv1.Template) bool {
	for _, c := range containers(t) {
		if !c.main || len(c.container.Command) == 0 {
			continue
		}
		cmd := c.container.Command
		args := append(slices.Clone(cmd[1:]), c.container.Args...)
		if path.Base(cmd[0]) == "oberth" && len(args) >= 2 && args[0] == "secretstore" && args[1] == "exec" {
			return true
		}
	}
	return false
}
func (g *graph) grantApplies(n *node, grant admissioncontract.GrantEvidence) bool {
	if n == nil || n.scope != "" {
		return false
	}
	step := n.template.Name
	if n.inlineDepth > 0 {
		step = "*"
	}
	key := workflowpolicy.GrantKey{Key: grant.Key(), Path: grant.Path, Audience: grant.Audience}
	want := identityname.Key{Repository: g.request.Repository, File: g.request.SelectedFile, Step: step}
	return key.Validate() == nil && key.Matches(want)
}
func (g *graph) granted(n *node, kind, value, audience string) bool {
	if kind == "secret" {
		return slices.ContainsFunc(g.secretGrants[n.key], func(grant admissioncontract.GrantEvidence) bool { return grant.Path == value })
	}
	for _, grant := range g.request.Grants {
		if g.grantApplies(n, grant) && evidenceMatches(grant, kind, value, audience) {
			return true
		}
	}
	return false
}
func (g *graph) nodeUsesCredentials(n *node) bool {
	if UsesCredentials(n.template) {
		return true
	}
	return len(g.secretGrants[n.key]) > 0
}
func (g *graph) credentialStep(n *node) string {
	if grants := g.secretGrants[n.key]; len(grants) > 0 {
		return grants[0].Step
	}
	for _, grant := range g.request.Grants {
		if grant.Kind == "identity" && grant.Step != "*" &&
			g.grantApplies(n, grant) && evidenceMatches(grant, grant.Kind, grant.Path, grant.Audience) {
			return grant.Step
		}
	}
	return "*"
}

// A step is a secret step exactly when its own template declares paths; each
// declared path needs its own grant. A template that starts no container is
// refused at its declaration and asks for no grant.
func (g *graph) credentials(n *node) {
	where := n.path + ".metadata.annotations[" + SecretPathsAnnotation + "]"
	paths := declaredSecretPaths(n.template)
	switch {
	case paths == nil:
		if secretCommand(n.template) {
			g.refuse(where, "secret_paths")
		}
		return
	case n.isDefault:
		return
	case !readsSecrets(n.template):
		g.refuse(where, "secret_form")
		return
	}
	for _, value := range g.secretDeclaration(n, paths) {
		if !g.granted(n, "secret", value, "") {
			g.missingGrant(n, "secret", value, n.path+".secrets["+value+"]")
		}
	}
	if g.loginAudience == "" {
		code := "store_config"
		if g.offlineValidation {
			code = "store_unverified"
		}
		f := finding(n.path, code)
		f.Installation = true
		f.Command = "oberth secretstore check"
		g.findings = append(g.findings, f)
	}
}
func (g *graph) token(v corev1.Volume, n *node, where string, shared bool) {
	if shared || n == nil || !singleMain(n.template) || len(v.Projected.Sources) != 1 {
		g.refuse(where, "token")
		return
	}
	source := v.Projected.Sources[0]
	if !tokenOnly(source) {
		g.refuse(where, "token")
		return
	}
	tok := source.ServiceAccountToken
	if tok.Audience == "" {
		g.refuse(where+".audience", "audience")
		return
	}
	if !g.granted(n, "identity", tok.Audience, tok.Audience) {
		g.missingGrant(n, "identity", tok.Audience, where+".audience["+tok.Audience+"]")
	}
	if !validToken(tok) {
		g.refuse(where, "token")
	}
	g.tokenMounts(v, n, where)
	for _, h := range append(slices.Clone(n.template.InitContainers), n.template.Sidecars...) {
		if h.MirrorVolumeMounts != nil && *h.MirrorVolumeMounts {
			g.refuse(where, "token")
		}
	}
}
func tokenOnly(s corev1.VolumeProjection) bool {
	return s.ServiceAccountToken != nil && s.Secret == nil && s.ConfigMap == nil && s.DownwardAPI == nil && s.ClusterTrustBundle == nil && s.PodCertificate == nil
}
func validToken(t *corev1.ServiceAccountTokenProjection) bool {
	return t.ExpirationSeconds != nil && *t.ExpirationSeconds >= 600 && *t.ExpirationSeconds <= 3600 && relativePath(t.Path)
}
func (g *graph) tokenMounts(v corev1.Volume, n *node, where string) {
	count := 0
	for _, c := range containers(n.template) {
		for _, m := range c.container.VolumeMounts {
			if m.Name != v.Name {
				continue
			}
			count++
			g.tokenMount(m, c.main, where)
		}
		for _, d := range c.container.VolumeDevices {
			if d.Name == v.Name {
				g.refuse(where, "token")
			}
		}
	}
	if count != 1 {
		g.refuse(where, "token")
	}
}
func (g *graph) tokenMount(m corev1.VolumeMount, main bool, where string) {
	if !main || !m.ReadOnly || m.SubPath != "" || m.SubPathExpr != "" {
		g.refuse(where, "token")
	}
}
func singleMain(t *wfv1.Template) bool {
	return t.ContainerSet == nil && (t.Container != nil) != (t.Script != nil) && t.DAG == nil && len(t.Steps) == 0
}
func relativePath(p string) bool {
	return literal(p) && p != "." && p != ".." && !path.IsAbs(p) && path.Clean(p) == p && !strings.HasPrefix(p, "../") && !strings.Contains(p, "\\")
}
func evidenceMatches(g admissioncontract.GrantEvidence, kind, value, audience string) bool {
	return g.Kind == kind && (kind == "identity" || g.Path == value) && g.Audience == audience && g.State == admissioncontract.StateVerified && g.Revision != ""
}

func (g *graph) missingGrant(n *node, kind, value, where string) {
	if n.scope != "" {
		g.refuse(where, "shared_grant")
		return
	}
	code := "read"
	switch kind {
	case "secret", "identity":
		code = "grant"
	case "pvc":
		code = "pvc"
	}
	f := finding(where, code)
	key := workflowpolicy.GrantKey{Key: identityname.Key{Repository: g.request.Repository, File: g.request.SelectedFile, Step: n.template.Name}, Path: value}
	if kind == "identity" {
		key.Path, key.Audience = "identity", value
	}
	if n.inlineDepth > 0 {
		key.Step = "*"
	}
	if grantableRequest(key, kind, value) {
		f.Installation = true
		key.File = g.commandFile()
		f.Command = grantCommand(key, kind, g.policy.Release)
	}
	g.findings = append(g.findings, f)
}

func releaseSelection(r admissioncontract.PrepareRequest, p Policy) error {
	if p.Release && r.Trigger != model.TriggerTag {
		return refusal("trigger", "grant")
	}
	return nil
}

func tokenProjection(v corev1.Volume) bool {
	if v.Projected == nil {
		return false
	}
	for _, s := range v.Projected.Sources {
		if s.ServiceAccountToken != nil {
			return true
		}
	}
	return false
}
