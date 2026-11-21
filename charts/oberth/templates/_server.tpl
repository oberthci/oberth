{{/* Current CLI wire config: explicit fields resolved from chart inputs. */}}
{{- define "oberth.server" -}}
{{- $server := deepCopy .Values.server -}}
{{- $runtime := pick $server.runtime "git_binary" "source_timeout_seconds" "admission_timeout_seconds" "source_client_burst" "receive_max_input_bytes" "default_branch" "image_prefixes" "source_policy" "release_routes" "issue_stale_after_seconds" "issue_active_stale_after_seconds" -}}
{{- $_ := set $runtime "helper_image" (include "oberth.image" .) -}}
{{- $defaults := .Values.argoNative.controller.workflowDefaults -}}
{{- $_ := set $runtime "workflow_defaults" (dict "workflow_defaults" nil "digest" "") -}}
{{- if $defaults -}}
{{- $_ := set $runtime "workflow_defaults" (dict "workflow_defaults" $defaults "digest" ($defaults | toJson | sha256sum)) -}}
{{- end -}}
{{- $_ := set $server "argo_namespace" (include "oberth.argoNamespace" .) -}}
{{- $_ := set $server "runtime" $runtime -}}
{{- $_ := set $server "files" (dict "ca_certificate" "/etc/oberth/ca/tls.crt" "ca_private_key" "/etc/oberth/ca/tls.key" "admin_public_key" "/etc/oberth/admin/admin.pub" "admin_push_key" "/etc/oberth/admin/push.pub" "ssh_host_key" "/etc/oberth/ssh/key" "forge_key" "/etc/oberth/forge/key" "secret_store_ca" "/etc/oberth/store/ca.crt" "secret_store_token" "/var/run/secrets/secretstore/token") -}}
{{- $_ := set $server "namespace" .Release.Namespace -}}
{{- $_ := set $server "pipeline_namespace" .Values.pipeline.namespace -}}
{{- $_ := set $server "database_file" .Values.database.file -}}
{{- $_ := set $server "secret_store" (pick .Values.secretStore "address" "audience" "kv_mount" "auth_mount" "writer_role") -}}
{{- range $key := list "runs" "logs" "schedules" "assist" -}}
{{- $_ := set $server $key (index $.Values $key) -}}
{{- end -}}

{{- $json := $server | toJson -}}
{{- if gt (len $json) 1048576 -}}
{{- fail "server.json must be at most 1 MiB" -}}
{{- end -}}
{{- $json -}}
{{- end -}}

{{- define "oberth.argoNamespace" -}}
{{- default .Release.Namespace .Values.argoNative.namespaceOverride -}}
{{- end -}}

{{- define "oberth.image" -}}
{{- if eq .Values.imageDigest "unset" -}}
{{- printf "%s:dev" .Values.imageRepository -}}
{{- else -}}
{{- printf "%s@%s" .Values.imageRepository .Values.imageDigest -}}
{{- end -}}
{{- end -}}
{{- define "oberth.sourceImage" -}}
{{- if eq .Values.sourcePluginImageDigest "unset" -}}
{{- printf "%s:dev" .Values.sourcePluginImageRepository -}}
{{- else -}}
{{- printf "%s@%s" .Values.sourcePluginImageRepository .Values.sourcePluginImageDigest -}}
{{- end -}}
{{- end -}}

{{- define "oberth.executorImage" -}}
{{- $image := .Values.argoNative.executor.image -}}
{{- printf "%s/%s:%s" $image.registry $image.repository $image.tag -}}
{{- end -}}

{{- define "oberth.clusterName" -}}
{{- $root := index . 0 -}}
{{- if ne $root.Release.Namespace "oberth" -}}{{ $root.Release.Namespace }}-{{- end -}}
oberth-{{ index . 1 }}
{{- end -}}
