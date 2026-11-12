{{- define "oberth.name" -}}
oberth
{{- end }}

{{- define "oberth.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "oberth.labels" -}}
app.kubernetes.io/name: {{ include "oberth.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{ include "oberth.chartLabels" . }}
{{- end }}

{{- /* Shared chart identity excludes application identity: the connector has
       its own stable name and Deployment instance/selector. */ -}}
{{- define "oberth.chartLabels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "oberth.selectorLabels" -}}
app.kubernetes.io/name: {{ include "oberth.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "oberth.serviceAccountName" -}}
{{ include "oberth.fullname" . }}
{{- end }}

{{- /* Cluster-scoped names retain the namespace and release identity. */ -}}
{{- define "oberth.controlPlanePriorityClass" -}}
{{- printf "%s-%s-control-plane" .Release.Namespace (include "oberth.fullname" .) -}}
{{- end }}

{{- /* One execution owner per pipeline namespace, also on isolated targets. */ -}}
{{- define "oberth.pipelinePriorityClass" -}}
{{- printf "%s-oberth-pipeline" .Values.argo.namespace -}}
{{- end }}

{{- define "oberth.pvcName" -}}
{{- if .Values.persistence.existingClaim -}}
{{ .Values.persistence.existingClaim }}
{{- else -}}
{{ include "oberth.fullname" . }}-data
{{- end -}}
{{- end }}

{{- /* --reuse-values may contain no utilityImage on pre-scratch releases.
Keep this fallback in sync with values.yaml and containerimage.Utility. */ -}}
{{- define "oberth.utilityImage" -}}
{{- .Values.utilityImage | default "busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74" -}}
{{- end -}}
