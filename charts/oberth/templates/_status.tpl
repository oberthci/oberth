{{/* Public scalar facts only. Never include raw chart values or runtime defaults. */}}
{{- define "oberth.statusSettings" -}}
{{- $controller := .Values.argoNative.controller.image -}}
{{- $store := .Values.storeNative.server.image -}}
{{- dict "pod_defaults" .Values.policies.floor "link_local_filter" .Values.policies.egress "pod_security" (default "baseline" .Values.statusPodSecurity) "bundled_store" .Values.secretStore.bundled "https_node_port" .Values.nodePorts.https "ssh_node_port" .Values.nodePorts.ssh "source_image" (include "oberth.sourceImage" .) "controller_image" (printf "%s/%s:%s" $controller.registry $controller.repository $controller.tag) "executor_image" (include "oberth.executorImage" .) "store_image" (printf "%s/%s:%s" $store.registry $store.repository $store.tag) "data_size" .Values.storage.size "tmp_size" .Values.storage.tmp "origins" (omit (default (dict) .Values.statusOrigins) "parallel runs") | toJson -}}
{{- end -}}
