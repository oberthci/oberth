package installer

import "strings"

// installerManagedValues are the oberth chart value paths `oberth install`
// and `oberth upgrade` set themselves (see helmUpgradeArgs and the Argo
// values): each install derives them from its own cluster, image, OpenBao
// and grants. TestInstallerManagedValuesCoverEverySet keeps this list in
// step with the installer's --set flags. Entries are as narrow as what the
// installer sets, so an operator's own settings next to them still count. A restore leaves them to the target install, and never tells the
// operator to re-apply them from a backup.
var installerManagedValues = []string{
	"image.ref",
	"pushBannerURL",
	"service.type",
	"podAnnotations.oberth.ci/server-certificate-sha256",
	"secretstore",
	"tls.pki",
	"watchTunnel.image",
	"watchTunnel.openbaoImage",
	"watchTunnel.originCACert",
	"argo.namespace",
	"argo.controllerProfile",
	"argo.vault",
	"argo.ciSecrets",
	"argo.goProxy.port",
	"argo.perRepoIdentities",
	"argo.perRepoCIIdentities",
	"argo.perStepIdentities",
	"auditAnchor.rekorURL",
	"auditAnchor.rekorInsecureHTTP",
	"auditAnchor.rekorPublicKey",
	"auditAnchor.acceptWitnessGenesis",
	"networkPolicy.enabled",
	"promotion.grantMaxLifetime",
}

// installerManagedValue reports whether a dotted value path is, or lies
// under, one the installer sets.
func installerManagedValue(path string) bool {
	for _, managed := range installerManagedValues {
		if path == managed || strings.HasPrefix(path, managed+".") {
			return true
		}
	}
	return false
}
