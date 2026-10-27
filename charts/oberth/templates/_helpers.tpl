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

{{- define "oberth.pvcName" -}}
{{- if .Values.persistence.existingClaim -}}
{{ .Values.persistence.existingClaim }}
{{- else -}}
{{ include "oberth.fullname" . }}-data
{{- end -}}
{{- end }}

{{- define "oberth.tlsSecretName" -}}
{{- if .Values.tls.existingSecret -}}
{{ .Values.tls.existingSecret }}
{{- else -}}
{{ include "oberth.fullname" . }}-tls
{{- end -}}
{{- end }}

{{- define "oberth.hostKeySecretName" -}}
{{- if .Values.ssh.hostKey.existingSecret -}}
{{ .Values.ssh.hostKey.existingSecret }}
{{- else -}}
{{ include "oberth.fullname" . }}-ssh-host-key
{{- end -}}
{{- end }}

{{- /* --reuse-values may contain no utilityImage on pre-scratch releases.
Keep this fallback in sync with values.yaml and containerimage.Utility. */ -}}
{{- define "oberth.utilityImage" -}}
{{- .Values.utilityImage | default "busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74" -}}
{{- end -}}
