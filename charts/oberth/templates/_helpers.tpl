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
