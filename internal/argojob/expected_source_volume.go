package argojob

import "github.com/oberthci/oberth/pkg/pipeline"

func (controller *Controller) expectedSourceVolume(request Request) SourceVolume {
	volume := SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath}
	if request.Assist {
		volume.BinarySubPath = binarySubPath
	}
	if controller.config.GoProxyCACertPEM != "" {
		volume.GoProxyCASubPath = goProxyCASubPath + "/" + goProxyCACertFile
	}
	if request.Trigger == pipeline.TriggerRelease || pipelineDeclaresSecretPaths(request.Source) {
		volume.BinarySubPath = binarySubPath
		if controller.config.VaultCACertPEM != "" {
			volume.VaultCASubPath = vaultCASubPath
		}
	}
	return volume
}
