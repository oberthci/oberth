package argojob

func (controller *Controller) expectedSourceVolume(request Request) SourceVolume {
	volume := SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath}
	if request.Assist {
		volume.BinarySubPath = binarySubPath
	}
	if requestNeedsSecretStore(request) {
		volume.BinarySubPath = binarySubPath
		if controller.config.VaultCACertPEM != "" {
			volume.VaultCASubPath = vaultCASubPath
		}
	}
	return volume
}
