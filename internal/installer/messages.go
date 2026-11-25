package installer

import (
	"errors"
	"fmt"
	"strings"
)

// Phase is an engine-owned progress reason.
type Phase string

const (
	StartingStore  Phase = "starting the secret store"
	Checking       Phase = "checking the cluster"
	ReleaseChecked Phase = "verifying the release"
	Creating       Phase = "creating identities"
	Saving         Phase = "saving credentials"
	Installing     Phase = "installing the chart"
	Waiting        Phase = "waiting for server"
	Endpoint       Phase = "verified the endpoint"
	Installed      Phase = "installed"
)

func upgradeRunsReview(count int) string {
	return fmt.Sprintf("Runs: %d; unlabelled runs not adopted; finish before upgrade.", count)
}

// Problem separates a stable refusal from its complete diagnostic.
type Problem struct {
	Code, Reason, Remedy, Detail string
	cause                        error
}

func (p *Problem) Unwrap() error { return p.cause }
func (p *Problem) Error() string {
	text := p.Reason + "\n" + p.Remedy
	if p.Detail != "" {
		text += "\n" + p.Detail
	}
	return output(text)
}

// NewProblem presents an engine refusal to the plain frontend.
func NewProblem(code string, cause error, detail string) *Problem {
	p := problems()[code]
	if detail == "" && cause != nil {
		detail = cause.Error()
	}
	p.Code, p.cause, p.Detail = code, cause, detail
	return &p
}
func problem(code string, cause error) error                  { return NewProblem(code, cause, "") }
func described(code string, cause error, detail string) error { return NewProblem(code, cause, detail) }

func chartError(code string, err error) error {
	if err == nil {
		return nil
	}
	var refusal *Problem
	if errors.As(err, &refusal) {
		return refusal
	}
	return problem(code, err)
}

// output wraps at spaces and preserves a long path as one copyable token.
func output(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		current := ""
		for _, word := range strings.Fields(line) {
			if current != "" && len([]rune(current))+1+len([]rune(word)) > 120 {
				lines = append(lines, current)
				current = ""
			}
			if current != "" {
				current += " "
			}
			current += word
		}
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n")
}
func wrap(operation string, err error) error {
	return described("cluster", err, operation+": "+err.Error())
}
func problems() map[string]Problem {
	result := map[string]Problem{
		"service-create":         {Reason: "This account cannot create Services.", Remedy: "Grant permission to create Services in the named namespace, then retry."},
		"service-update":         {Reason: "This account cannot update Services.", Remedy: "Grant permission to update Services in the named namespace, then retry."},
		"namespace-reserved":     {Reason: "System namespaces cannot hold an installation.", Remedy: "Choose another namespace for the server and pipelines."},
		"local-release":          {Reason: "A local chart requires a fresh or local installation.", Remedy: "Use a released version, or remove the installation first."},
		"schema-downgrade":       {Reason: "The target requires an older database schema.", Remedy: "Run oberth upgrade --version <version> with a release that supports the live schema."},
		"release-provenance":     {Reason: "Installation release metadata is missing or inconsistent.", Remedy: "Inspect Helm release history; copy the data volume at storage level before repairing metadata by hand."},
		"history-delete":         {Reason: "Stored history would delete an object this release does not own.", Remedy: "Check the named object's namespace and release ownership; correct the history or object, then review and retry."},
		"upgrade-required":       {Reason: "Install can only resume the current release version.", Remedy: "Run oberth upgrade --version <version> to check the live database schema before changing versions."},
		"chart-state":            {Reason: "The latest chart revision has an unsupported state.", Remedy: "Inspect helm history oberth -n <namespace>; preserve the data volume before repairing the release by hand."},
		"chart-files":            {Reason: "Required chart dependency files are missing.", Remedy: "Run oberth upgrade --version <earlier-version> with the complete signed release."},
		"context-missing":        {Reason: "The selected context is not in kubeconfig.", Remedy: "Choose a context listed in the selected kubeconfig file."},
		"wiring":                 {Reason: "The installer is missing a required component.", Remedy: "Use a complete Oberth build with the install engine."},
		"version":                {Reason: "The cluster version is too old.", Remedy: "Use Kubernetes 1.30 or newer for Pod-bound tokens and task-result admission."},
		"namespaces":             {Reason: "Server and pipeline namespaces must differ.", Remedy: "Choose separate valid Kubernetes namespaces for the server and its runs."},
		"namespace":              {Reason: "A namespace is not a valid Kubernetes namespace name.", Remedy: "Use lowercase letters, digits and hyphens, with at most 63 characters."},
		"url":                    {Reason: "The public server URL is invalid.", Remedy: "Use an HTTPS origin without credentials, a path, query or fragment."},
		"ssh-port":               {Reason: "The SSH port is outside 1 through 65535.", Remedy: "Pass --ssh-port with a port between 1 and 65535."},
		"parallel":               {Reason: "Parallel runs cannot be negative.", Remedy: "Pass --parallel 0 for unlimited runs, or a positive number."},
		"source-pins-completion": {Reason: "An interrupted upgrade must be finished first.", Remedy: "Run oberth upgrade to finish it, then run it again with --source-pin or --no-source-pins."},
		"source-pins":            {Reason: "The source pin policy is invalid.", Remedy: "Pass --source-pin consumer=producer with canonical repositories within the policy bounds."},
		"store-url":              {Reason: "The external secret store URL is invalid.", Remedy: "Pass --external-store with an HTTPS address without credentials, query or fragment."},
		"cluster":                {Reason: "The cluster request failed.", Remedy: "Check the selected context, login and permissions, then retry."},
		"choices":                {Reason: "An installation setting is unsupported.", Remedy: "Use the documented install flags; generated chart inputs cannot be overridden."},
		"target":                 {Reason: "The cluster or release changed after review.", Remedy: "Rerun the same oberth command to review the changed release."},
		"release":                {Reason: "Release verification failed.", Remedy: "Download the signed release again and check its trusted signing key."},
		"identity-missing":       {Reason: "An established installation Secret is missing.", Remedy: "Apply a saved private copy of the named Secret; if none exists, preserve the data volume and profile."},
		"identity":               {Reason: "An installation identity is invalid.", Remedy: "Check its certificate and key; keep the profile before reinstalling a broken installation."},
		"profile-cluster":        {Reason: "This profile belongs to another cluster.", Remedy: "Move the profile directory aside, then run oberth install again."},
		"profile-ca":             {Reason: "The profile trust differs from this installation.", Remedy: "Move the old profile directory aside, then run oberth install again."},
		"profile-key":            {Reason: "The saved administrator key does not match the installation.", Remedy: "Use the matching profile; retain this directory before starting a fresh installation."},
		"custody":                {Reason: "A credential file or directory is unsafe or unreadable.", Remedy: "Use an owner-only profile directory (0700) and credential files (0600) outside the checkout."},
		"tls":                    {Reason: "The server failed TLS or status verification.", Remedy: "Check the saved trust, server certificate and reported diagnostic."},
		"starting":               {Reason: "The server is not ready yet.", Remedy: "Wait for server recovery to finish; installation retries within its deadline."},
		"connect":                {Reason: "The server cannot be reached yet.", Remedy: "Check --url, port mappings and server startup; installation retries within its deadline."},
		"timeout":                {Reason: "The server did not become ready before the deadline.", Remedy: "Check the server's diagnostics and public address, then rerun oberth install."},
		"version-timeout":        {Reason: "Server still serves another version.", Remedy: "Run oberth system status and inspect the server rollout; keep this profile for recovery."},
		"review":                 {Reason: "Installation requires review approval.", Remedy: "Review the prepared installation and explicitly approve Apply."},
		"terminal":               {Reason: "Installation needs explicit noninteractive approval.", Remedy: "Review --dry-run, then rerun with --yes."},
		"pending":                {Reason: "A chart operation started outside Oberth is still pending.", Remedy: "Finish or undo that operation with the tool that started it, then prepare again."},
		"chart":                  {Reason: "The chart operation failed.", Remedy: "Correct the reported values or permissions, then rerun oberth install."},
		"store-init":             {Reason: "The store was initialized without a usable local setup record.", Remedy: "Use its original private profile; recreate only an empty store if its initialization reply was lost."},
		"store-lost":             {Reason: "This store's keys were lost before they were saved.", Remedy: "Delete the store's data volume (data-oberth-store-0), then install again."},
		"unseal-rejected":        {Reason: "The unseal key does not match this store.", Remedy: "Use the recovery file of the installation that created this store; see the path in details."},
		"store-sealed":           {Reason: "Secret store is sealed.", Remedy: "An administrator runs oberth unseal, then retry."},
		"store-drift":            {Reason: "The saved store setup differs from these installation settings.", Remedy: "Use the matching profile and settings; reconcile store policy explicitly before changing them."},
		"unseal-key":             {Reason: "The local unseal key is missing or unreadable.", Remedy: "Restore the named private file from your saved installation profile, then run oberth unseal."},
		"store-external":         {Reason: "This installation uses an external secret store.", Remedy: "Use the external store operator tools to initialize or unseal it."},
		"store-pod":              {Reason: "The store Pod is not owned by the installed StatefulSet.", Remedy: "Check the release and its controller-owned store Pod, then retry."},
		"secret-input":           {Reason: "Secret input is missing, unreadable or larger than 1 MiB.", Remedy: "Read the value from standard input or a readable file; never pass it as a command argument."},
		"store-starting":         {Reason: "The bundled store Pod is not running yet.", Remedy: "Wait for the owned store Pod to start within the installation deadline."},
		"restore-exists":         {Reason: "The restore or backup destination already exists.", Remedy: "Choose a new file; inspect existing output before removing it."},
		"restore-proof":          {Reason: "The selected database or archive does not match its recovery proof.", Remedy: "Use the matching authenticated backup, protocol, schema and audit head."},
		"operation":              {Reason: "Another installation operation owns this profile.", Remedy: "Wait for that command to finish, then prepare and review again."},
		"chart-timeout":          {Reason: "The chart phase did not finish before its deadline.", Remedy: "Check image pulls, storage and permissions, then rerun oberth install to repair and retry."},
	}
	for key, value := range operationProblems() {
		result[key] = value
	}
	return backupProblems(result)
}

func operationProblems() map[string]Problem {
	return map[string]Problem{
		"loopback-url":           {Reason: "The API host is loopback; choose a reachable server URL.", Remedy: "Run oberth install with --url set to an HTTPS address clients can reach."},
		"address":                {Reason: "The server address is unspecified.", Remedy: "Run oberth install with --url and --ssh-port clients can reach."},
		"url-host":               {Reason: "Public URL host must use ASCII.", Remedy: "Set --url to the punycode address shown below and review again."},
		"store-health":           {Reason: "Secret store health is unexpected.", Remedy: "Check the store's health response, then rerun oberth install."},
		"store-timeout":          {Reason: "Secret store startup timed out.", Remedy: "Check the store Pod and leadership, then rerun oberth install."},
		"administrator-missing":  {Reason: "The installation's administrator Secret is missing.", Remedy: "Apply a saved private copy of oberth-admin; if none exists, preserve the data volume and original profile."},
		"administrator-retained": {Reason: "The namespace retains an installation's administrator Secret.", Remedy: "Update with its profile; otherwise choose another --namespace and unused --https-node-port and --ssh-node-port."},
		"credential-missing":     {Reason: "The profile certificate file is missing.", Remedy: "Restore the named CA file from this installation’s profile backup, then retry."},
		"installation-owner":     {Reason: "This profile does not own the existing installation.", Remedy: "Update with its profile; otherwise choose another --namespace and unused --https-node-port and --ssh-node-port."},
		"release-foreign":        {Reason: "The target release belongs to another product.", Remedy: "Choose another --namespace for this installation; keep the existing release."},
		"release-membership":     {Reason: "A Helm release in the cluster cannot be read.", Remedy: "Repair the release metadata in the named namespace, then review again."},
		"install-answers":        {Reason: "Installation answers differ from the local record.", Remedy: "Use the recorded answers. If a revision is pending, roll it back before changing them."},
		"namespace-installation": {Reason: "This namespace holds another installation.", Remedy: "Choose another --namespace and unused --https-node-port and --ssh-node-port."},
		"namespace-owner":        {Reason: "This namespace belongs to another Helm release.", Remedy: "Choose another namespace for this installation."},
		"pipeline-security":      {Reason: "Pipeline namespace lacks Pod Security enforcement.", Remedy: "Set pod-security.kubernetes.io/enforce=baseline or restricted; or let the installer create the namespace."},
		"pipeline-change":        {Reason: "The pipeline namespace of an existing installation cannot be changed.", Remedy: "Use its recorded pipeline namespace, or create a separate installation."},
		"profile-namespace":      {Reason: "This profile belongs to another namespace.", Remedy: "Choose a separate --profile for the second installation."},
		"node-port-api":          {Reason: "The cluster refused these node ports.", Remedy: "Choose available node ports allowed by the cluster, then review again."},
		"node-port":              {Reason: "Service node ports must be distinct and between 1 and 65535.", Remedy: "Set --https-node-port and --ssh-node-port to unused cluster node ports."},
		"service-port":           {Reason: "HTTPS Service port must be 1 to 65535.", Remedy: "Set --service-port to a valid in-cluster HTTPS port."},
		"service-port-chart":     {Reason: "This release has a fixed Service port.", Remedy: "Use its default port or select a release with --service-port support."},
		"service-port-ssh":       {Reason: "HTTPS and SSH Service ports must differ.", Remedy: "Choose --service-port other than 2222, which is the SSH Service port."},
		"https-node-port":        {Reason: "HTTPS node port is taken; use --https-node-port.", Remedy: "Choose an unused node port and review the installation again."},
		"ssh-node-port":          {Reason: "SSH node port is taken; use --ssh-node-port.", Remedy: "Choose an unused node port and review the installation again."},
		"definition-partial":     {Reason: "The cluster has an incomplete set of workflow definitions.", Remedy: "Install the missing definitions with their owner, then review again."},
		"definition-schema":      {Reason: "An existing workflow definition is incompatible.", Remedy: "Upgrade definitions with their owner, or install without borrowing in a cluster of its own."},
		"release-unowned":        {Reason: "Applied installer settings are unavailable in this release history.", Remedy: "Keep a storage-level copy of the data volume; inspect Helm history and repair the saved settings by hand."},
		"rollback-missing":       {Reason: "The local rollback record is missing.", Remedy: "Use the original installation profile, or run oberth upgrade to create a new rollback point."},
		"foreign-rollback":       {Reason: "Outside changes prevent use of this rollback point.", Remedy: "Run oberth upgrade for a new rollback point."},
		"rollback-superseded":    {Reason: "A later installer apply replaced this rollback point.", Remedy: "Run oberth upgrade --version <earlier-version> to return, or oberth upgrade for a new rollback point."},
		"foreign-removed":        {Reason: "Release removal is still in progress.", Remedy: "Finish removal with the tool that started it, then run oberth install to review the saved choices."},
		"upgrade-source":         {Reason: "There is no deployed release to upgrade.", Remedy: "Run oberth install before upgrading this namespace."},
		"upgrade-schema-failed":  {Reason: "Upgrade did not finish or pass readiness verification.", Remedy: "When oberth system status is ready, run oberth upgrade again; recovery of a stopped server is manual."},
		"controller-timeout":     {Reason: "The workflow controller did not finish restarting before the deadline.", Remedy: "Check the oberth-controller-workflow-controller Pods in the installation namespace, then retry."},
		"upgrade-failed":         {Reason: "Upgrade did not finish or pass readiness verification.", Remedy: "When oberth system status is ready, run oberth upgrade again, or run oberth rollback --yes to return."},
		"rollback":               {Reason: "The local rollback record is unreadable or invalid.", Remedy: "Recover the intact installation profile and rollback record from your private recovery copy."},
		"repair-release":         {Reason: "Upgrade repair requires the recorded release.", Remedy: "Retry oberth upgrade with its recorded version; oberth rollback --yes works only with the same schema."},
		"rollback-schema":        {Reason: "Rollback across a database schema change is unavailable.", Remedy: "Keep the backup, backup.agekey and rollback.json in this profile; recovery is manual in this version."},
		"repair-schema":          {Reason: "Upgrade repair across a schema change is unavailable.", Remedy: "When oberth system status is ready, retry oberth upgrade; otherwise keep this profile for manual recovery."},
		"ready-version":          {Reason: "The ready server does not serve the reviewed version.", Remedy: "Run oberth system status; inspect Helm history and keep the profile and recovery files for manual recovery."},
	}
}

func backupProblems(catalogue map[string]Problem) map[string]Problem {
	for code, entry := range map[string]Problem{
		"backup":                {Reason: "The encrypted database backup could not be completed.", Remedy: "Check the age key, archive, destination and reported cause; retry with complete input."},
		"backup-destination":    {Reason: "The backup destination directory is missing or cannot be used.", Remedy: "Create its directory or choose a destination in an existing writable directory."},
		"backup-staging":        {Reason: "The incomplete backup file is unavailable or has changed.", Remedy: "Inspect the destination and choose a new backup path."},
		"backup-publish":        {Reason: "The backup could not be published at its destination.", Remedy: "Check the reported cause and storage, then retry with a new backup path."},
		"backup-hardlink":       {Reason: "The destination filesystem cannot create hard links.", Remedy: "Write to a local disk, then copy the completed backup."},
		"backup-cleanup":        {Reason: "Backup is published, but its temporary file could not be removed.", Remedy: "Keep the backup at the shown path and check storage; do not retry there."},
		"backup-directory-sync": {Reason: "Backup is published, but its directory could not be synced.", Remedy: "Keep the backup at the shown path and check the storage; do not retry there."},
		"backup-limit":          {Reason: "The database archive exceeds its selected bound.", Remedy: "Use a positive logical limit up to 4 GiB, with enough staging space."},
		"backup-retention":      {Reason: "Backup is published, but older copies could not be removed.", Remedy: "Keep the completed archive; check directory permissions and remove older copies by hand."},
		"restore-input":         {Reason: "Installation restore input is incomplete or incompatible.", Remedy: "Use --yes, a fresh cluster, URL and profile, a compatible signed release, and the saved private recovery files."},
		"restore-occupied":      {Reason: "Restore target is already in use.", Remedy: "Use a fresh cluster and new profile; preserve the existing installation and its volume."},
		"restore-database":      {Reason: "Database restore did not finish.", Remedy: "The server remains stopped. Preserve its volume; inspect storage and retry in a fresh target."},
		"restore-store":         {Reason: "Bundled-store restore did not finish.", Remedy: "The server remains stopped. Keep the archive and unseal file; inspect the store before retrying."},
	} {
		catalogue[code] = entry
	}
	return catalogue
}
