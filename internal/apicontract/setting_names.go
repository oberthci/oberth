package apicontract

const (
	SettingNamespace              = "namespace"
	SettingPipelineNamespace      = "pipeline namespace"
	SettingHTTPSNodePort          = "HTTPS node port"
	SettingSSHNodePort            = "SSH node port"
	SettingSSHPort                = "SSH port"
	SettingServerURL              = "server URL"
	SettingParallelRuns           = "parallel runs"
	SettingPodDefaults            = "Pod defaults"
	SettingLinkLocalFilter        = "link-local egress filter"
	SettingSourcePins             = "source pins"
	SettingStoreMode              = "store mode"
	SettingLogBytes               = "log bytes per run"
	SettingScheduleEntries        = "schedule entries"
	SettingAssistTTL              = "assist TTL seconds"
	SettingDataSize               = "data volume size"
	SettingTmpSize                = "temporary volume size"
	SettingDatabaseFile           = "database file"
	SettingRegisteredUpstreams    = "registered upstreams"
	SettingRegisteredRepositories = "registered repositories"
)

func InstallationChoiceNames() []string {
	return []string{
		SettingNamespace,
		SettingPipelineNamespace,
		SettingHTTPSNodePort,
		SettingSSHNodePort,
		SettingSSHPort,
		SettingServerURL,
		SettingParallelRuns,
		SettingPodDefaults,
		SettingLinkLocalFilter,
		SettingSourcePins,
		SettingStoreMode,
		SettingLogBytes,
		SettingScheduleEntries,
		SettingAssistTTL,
		SettingDataSize,
		SettingTmpSize,
		SettingDatabaseFile,
	}
}
