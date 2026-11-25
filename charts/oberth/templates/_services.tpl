{{- define "oberth.serverService" -}}
apiVersion: v1
kind: Service
metadata:
  name: oberth
  namespace: {{ .Release.Namespace }}
spec:
  type: NodePort
  selector: {app: oberth}
  ports:
    - {name: https, port: {{ .Values.servicePort }}, targetPort: https, nodePort: {{ .Values.nodePorts.https }}}
    - {name: ssh, port: 2222, targetPort: ssh, nodePort: {{ .Values.nodePorts.ssh }}}
{{- end -}}

{{- define "oberth.sourceService" -}}
apiVersion: v1
kind: Service
metadata:
  name: oberth-source
  namespace: {{ .Release.Namespace }}
spec:
  selector: {app: oberth}
  ports:
    - {name: source, port: 8445, targetPort: source}
{{- end -}}
