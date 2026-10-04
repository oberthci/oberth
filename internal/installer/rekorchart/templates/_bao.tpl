{{/* OpenBao identities are projected only into the authenticating sidecar. */}}
{{- define "oberth.bao.init" -}}
- name: prepare-runtime
  image: busybox:1.37.0-musl@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092
  command: [sh, -ec]
  args:
    - cp /bin/busybox /tools/busybox; chmod 0555 /tools/busybox; chown {{ .uid }}:{{ .uid }} /credentials; chmod 0700 /credentials
  securityContext:
    runAsUser: 0
    runAsNonRoot: false
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities: {drop: [ALL], add: [CHOWN, FOWNER]}
  volumeMounts:
    - {name: bao-credentials, mountPath: /credentials}
    - {name: runtime-tools, mountPath: /tools}
- name: openbao-agent
  restartPolicy: Always
  image: quay.io/openbao/openbao:2.6.1@sha256:5b2486ab0fb90bbc788cc345b0a08616dfb375873ee8be5df3a2fd4d378a67e0
  command: [bao, agent, -config=/etc/bao/agent.hcl]
  env:
    - {name: HOME, value: /credentials}
  securityContext:
    runAsUser: {{ .uid }}
    runAsGroup: {{ .uid }}
    runAsNonRoot: true
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities: {drop: [ALL]}
  resources:
    requests: {cpu: 10m, memory: 32Mi}
    limits: {memory: 128Mi}
  startupProbe:
    exec: {command: [sh, -ec, {{ if eq .uid 999 }}"test -s /credentials/password && test -s /credentials/root-password"{{ else }}"test -s /credentials/password"{{ end }}]}
    periodSeconds: 2
    failureThreshold: 150
  volumeMounts:
    - {name: bao-credentials, mountPath: /credentials}
    - {name: bao-config, mountPath: /etc/bao, readOnly: true}
    - {name: bao-login, mountPath: /var/run/bao, readOnly: true}
{{- end }}
{{- define "oberth.bao.mounts" -}}
- {name: bao-credentials, mountPath: /credentials, readOnly: true}
- {name: runtime-tools, mountPath: /tools, readOnly: true}
{{- end }}
{{- define "oberth.bao.volumes" -}}
- name: bao-credentials
  emptyDir: {medium: Memory, sizeLimit: 1Mi}
- name: runtime-tools
  emptyDir: {medium: Memory, sizeLimit: 8Mi}
- name: bao-config
  configMap: {name: "rekor-bao-{{ . }}"}
- name: bao-login
  projected:
    defaultMode: 0444
    sources:
      - serviceAccountToken: {path: token, expirationSeconds: 600}
{{- end }}
