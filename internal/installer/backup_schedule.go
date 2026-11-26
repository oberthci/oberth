package installer

// BackupSchedule is the operator-applied offline example; an installed
// profile prints it with its running server image (InstalledBackupSchedule).
const BackupSchedule = `# Apply in the installed server namespace, whose Pod Security policy is Restricted.
# Edit the namespace, pinned image, schedule, backup PVC and memory bound before applying.
# Keep operator profiles, decryption keys and unseal files off the cluster.
# The chart owns ServiceAccount oberth-backup; kubectl delete -f leaves it in place.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: oberth-backup, namespace: oberth}
rules:
  - apiGroups: ['']
    resources: [secrets]
    resourceNames:
      - oberth-admin
      - oberth-ca
      - oberth-ssh-host
      - oberth-forge-key
      - oberth-store-tls
      - oberth-backup-signing
    verbs: [get]
  - apiGroups: ['']
    resources: [configmaps]
    resourceNames: [oberth-backup]
    verbs: [get]
  - apiGroups: ['']
    resources: [serviceaccounts/token]
    resourceNames: [oberth-backup]
    verbs: [create]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: oberth-backup, namespace: oberth}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: oberth-backup}
subjects:
  - {kind: ServiceAccount, name: oberth-backup, namespace: oberth}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: oberth-backup, namespace: oberth}
spec:
  schedule: '0 2 * * *'
  timeZone: Etc/UTC
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 1
  failedJobsHistoryLimit: 1
  jobTemplate:
    spec:
      backoffLimit: 2
      activeDeadlineSeconds: 900
      template:
        spec:
          serviceAccountName: oberth-backup
          automountServiceAccountToken: false
          restartPolicy: Never
          enableServiceLinks: false
          securityContext:
            runAsNonRoot: true
            runAsUser: 65534
            runAsGroup: 65534
            fsGroup: 65534
            seccompProfile: {type: RuntimeDefault}
          containers:
            - name: backup
              image: REPLACE_WITH_PINNED_OBERTH_IMAGE
              command: [/usr/local/bin/oberth]
              args:
                - backup
                - --in-cluster
                - --namespace
                - $(SERVER_NAMESPACE)
                - --directory
                - /backups
                - --keep
                - '7'
              env:
                - name: SERVER_NAMESPACE
                  valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
                - {name: TMPDIR, value: /work}
              securityContext:
                allowPrivilegeEscalation: false
                readOnlyRootFilesystem: true
                capabilities: {drop: [ALL]}
              resources:
                requests: {cpu: 250m, memory: 512Mi}
                limits: {memory: 4Gi}
              volumeMounts:
                - {name: backups, mountPath: /backups}
                - {name: work, mountPath: /work}
                - name: api-access
                  mountPath: /var/run/secrets/kubernetes.io/serviceaccount
                  readOnly: true
          volumes:
            - name: backups
              persistentVolumeClaim: {claimName: oberth-backups}
            - name: work
              emptyDir: {medium: Memory, sizeLimit: 2Gi}
            - name: api-access
              projected:
                defaultMode: 0440
                sources:
                  - serviceAccountToken: {path: token, expirationSeconds: 600}
                  - configMap:
                      name: kube-root-ca.crt
                      items: [{key: ca.crt, path: ca.crt}]
`
