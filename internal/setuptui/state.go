package setuptui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

// stepID names one entry of the main menu. The order is the order a first
// run walks them, and the number shown as "N of M" (#047).
type stepID int

const (
	stepCluster stepID = iota
	stepStore
	stepTLS
	stepForge
	stepUplink
	stepClient
	stepReview
	stepInstall

	stepCount = int(stepInstall) + 1
)

var stepNames = [stepCount]string{
	"Cluster",
	"Secret store",
	"TLS",
	"Forge & upstream key",
	"Uplink",
	"Client access",
	"Review",
	"Install",
}

func (s stepID) name() string { return stepNames[s] }

// stepTitle is the frame title: "<step> (N of M)".
func (s stepID) title() string {
	return fmt.Sprintf("%s (%d of %d)", s.name(), int(s)+1, stepCount)
}

// Install outcomes recorded in the state file. A pending upstream stays
// pending across runs until Verify sees the server Ready (#002).
const (
	installNone           = ""
	installComplete       = "complete"
	installPendingForge   = "pending-upstream"
	installPartial        = "partial"
	installFailed         = "failed"
	clusterChoiceFile     = "@file"
	clusterChoiceKind     = "@kind"
	uplinkKeyOther        = "@other"
	upstreamKeyOther      = "@other"
	upstreamKeyRescan     = "@rescan"
	forgeAuthGenerate     = "deploy-key"
	forgeAuthExisting     = "existing"
	storeInstallProd      = "install-prod"
	storeInstallDev       = "install-dev"
	storeConnect          = "connect"
	defaultClientAccess   = "both"
	setupStateFileName    = "setup.json"
	setupStateFileVersion = 1
)

// installRecord is what the last install attempt left behind.
type installRecord struct {
	Status string    `json:"status,omitempty"`
	Phase  string    `json:"phase,omitempty"`
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

// WizardState holds every answer. It wraps installer.Config — the wizard is
// a skin over `oberth install` — plus the choices that only the wizard
// needs. No secret is ever held here: credentials live in the ceremony.
type WizardState struct {
	Config installer.Config

	ClusterChoice  string // a context name, clusterChoiceFile or clusterChoiceKind
	ClusterInfo    clusterInfoMsg
	SSHKeyPath     string
	UplinkKey      string // a scanned key path or uplinkKeyOther
	UplinkIdentity string
	ForgeType      string
	ForgeOrg       string
	ForgeAuth      string
	// UpstreamKeyPick is the private key chosen from ~/.ssh, or
	// upstreamKeyOther when a path is typed (#057).
	UpstreamKeyPick string
	StoreMode       string
	StoreAddress    string
	ClientAccess    string

	done    [stepCount]bool
	Install installRecord
}

func newWizardState(opts Options) *WizardState {
	st := &WizardState{
		Config: installer.Config{
			Dev:           true,
			Namespace:     installer.DefaultNamespace,
			ChartPath:     opts.ChartPath,
			ImageRef:      opts.ImageRef,
			BinaryVersion: opts.BinaryVersion,
		},
		StoreMode:    storeInstallProd,
		ForgeType:    "github",
		ForgeAuth:    forgeAuthGenerate, // a dedicated, generated key (#017)
		ClientAccess: defaultClientAccess,
	}
	st.Config.InstallSecretStore = true
	return st
}

// contextForCommands is the kubeconfig context every kubectl the wizard runs
// or prints targets.
func (st *WizardState) contextForCommands() string {
	if st.Config.CreateKind {
		return installer.KindContextName
	}
	return st.Config.ContextName
}

func (st *WizardState) namespace() string {
	if st.Config.Namespace == "" {
		return installer.DefaultNamespace
	}
	return st.Config.Namespace
}

// savedSetup is the on-disk form of the answers: everything a rerun needs
// to resume, nothing that is a credential. Paths to key files are kept;
// key material never is.
type savedSetup struct {
	Version int `json:"version"`

	ClusterChoice  string `json:"clusterChoice,omitempty"`
	KubeconfigPath string `json:"kubeconfig,omitempty"`
	ContextName    string `json:"context,omitempty"`
	CreateKind     bool   `json:"createKind,omitempty"`
	// What the cluster probe found, so a resumed finish screen, Verify and
	// the equivalent command do not fall back to localhost (non-secret).
	ClusterServer string `json:"clusterServer,omitempty"`
	ClusterLocal  bool   `json:"clusterLocal,omitempty"`
	NodeName      string `json:"nodeName,omitempty"`
	NodeIP        string `json:"nodeIP,omitempty"`

	Namespace        string `json:"namespace,omitempty"`
	ArgoNamespace    string `json:"argoNamespace,omitempty"`
	OpenBaoNamespace string `json:"openbaoNamespace,omitempty"`
	NetworkPolicy    string `json:"networkPolicy,omitempty"`

	StoreMode    string `json:"storeMode,omitempty"`
	StoreAddress string `json:"storeAddress,omitempty"`
	InstallRekor bool   `json:"installRekor,omitempty"`

	TLSExtraDNSNames []string `json:"tlsExtraDNSNames,omitempty"`
	TLSExtraIPs      []string `json:"tlsExtraIPs,omitempty"`

	ForgeType          string   `json:"forge,omitempty"`
	ForgeOrg           string   `json:"organization,omitempty"`
	ForgeAuth          string   `json:"upstreamKey,omitempty"`
	UpstreamPrivateKey string   `json:"upstreamKeyFile,omitempty"`
	UpstreamKeyPick    string   `json:"upstreamKeyPick,omitempty"`
	AlsoUpstreams      []string `json:"alsoUpstreams,omitempty"`
	ClientHost         string   `json:"clientHost,omitempty"`

	UplinkIdentity string `json:"uplinkIdentity,omitempty"`
	UplinkKey      string `json:"uplinkKey,omitempty"`
	SSHKeyPath     string `json:"sshPublicKey,omitempty"`

	ClientAccess string `json:"clientAccess,omitempty"`

	Done    []string      `json:"done,omitempty"`
	Install installRecord `json:"install,omitzero"`
}

// setupStatePath is ~/.config/oberth/setup.json, honouring XDG_CONFIG_HOME
// like the client files beside it. Tests replace it.
var setupStatePath = func() (string, error) {
	dir, err := installer.ClientConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, setupStateFileName), nil
}

func (st *WizardState) toSaved() savedSetup {
	c := st.Config
	s := savedSetup{
		Version:            setupStateFileVersion,
		ClusterChoice:      st.ClusterChoice,
		KubeconfigPath:     c.KubeconfigPath,
		ContextName:        c.ContextName,
		CreateKind:         c.CreateKind,
		ClusterServer:      st.ClusterInfo.server,
		ClusterLocal:       st.ClusterInfo.isLocal,
		NodeName:           st.ClusterInfo.nodeName,
		NodeIP:             st.ClusterInfo.nodeIP,
		Namespace:          c.Namespace,
		ArgoNamespace:      c.ArgoNamespace,
		OpenBaoNamespace:   c.OpenBaoNamespace,
		NetworkPolicy:      c.NetworkPolicy,
		StoreMode:          st.StoreMode,
		StoreAddress:       st.StoreAddress,
		InstallRekor:       c.InstallRekor,
		TLSExtraDNSNames:   c.TLSExtraDNSNames,
		TLSExtraIPs:        c.TLSExtraIPs,
		ForgeType:          st.ForgeType,
		ForgeOrg:           st.ForgeOrg,
		ForgeAuth:          st.ForgeAuth,
		UpstreamPrivateKey: c.UpstreamPrivateKeyPath,
		UpstreamKeyPick:    st.UpstreamKeyPick,
		AlsoUpstreams:      c.AlsoUpstreams,
		ClientHost:         c.ClientHost,
		UplinkIdentity:     st.UplinkIdentity,
		UplinkKey:          st.UplinkKey,
		SSHKeyPath:         st.SSHKeyPath,
		ClientAccess:       st.ClientAccess,
		Install:            st.Install,
	}
	for i, done := range st.done {
		if done {
			s.Done = append(s.Done, stepID(i).key())
		}
	}
	return s
}

func (st *WizardState) fromSaved(s savedSetup) {
	c := &st.Config
	st.ClusterChoice = s.ClusterChoice
	c.KubeconfigPath = s.KubeconfigPath
	c.ContextName = s.ContextName
	c.CreateKind = s.CreateKind
	if st.ClusterInfo.server == "" && s.ClusterServer != "" {
		st.ClusterInfo.context = s.ContextName
		st.ClusterInfo.server = s.ClusterServer
		st.ClusterInfo.isLocal = s.ClusterLocal
		st.ClusterInfo.nodeName = s.NodeName
		st.ClusterInfo.nodeIP = s.NodeIP
	}
	if s.Namespace != "" {
		c.Namespace = s.Namespace
	}
	c.ArgoNamespace = s.ArgoNamespace
	c.OpenBaoNamespace = s.OpenBaoNamespace
	c.NetworkPolicy = s.NetworkPolicy
	if s.StoreMode != "" {
		st.setStoreMode(s.StoreMode)
	}
	st.StoreAddress = s.StoreAddress
	c.ArgoVaultAddress = ""
	if st.StoreMode == storeConnect {
		c.ArgoVaultAddress = s.StoreAddress
	}
	c.InstallRekor = s.InstallRekor && st.StoreMode == storeInstallProd
	c.TLSExtraDNSNames = s.TLSExtraDNSNames
	c.TLSExtraIPs = s.TLSExtraIPs
	if s.ForgeType != "" {
		st.ForgeType = s.ForgeType
	}
	st.ForgeOrg = s.ForgeOrg
	if s.ForgeAuth != "" {
		st.ForgeAuth = s.ForgeAuth
	}
	if st.ForgeAuth == forgeAuthExisting {
		c.UpstreamPrivateKeyPath = s.UpstreamPrivateKey
		st.UpstreamKeyPick = s.UpstreamKeyPick
	}
	c.AlsoUpstreams = s.AlsoUpstreams
	c.ClientHost = s.ClientHost
	st.UplinkIdentity = s.UplinkIdentity
	st.UplinkKey = s.UplinkKey
	st.SSHKeyPath = s.SSHKeyPath
	if s.ClientAccess != "" {
		st.ClientAccess = s.ClientAccess
	}
	for _, key := range s.Done {
		if id, ok := stepByKey(key); ok {
			st.done[id] = true
		}
	}
	st.Install = s.Install
}

// key is the stable name of a step in the state file.
func (s stepID) key() string {
	return [stepCount]string{"cluster", "store", "tls", "forge", "uplink", "client", "review", "install"}[s]
}

func stepByKey(key string) (stepID, bool) {
	for i := 0; i < stepCount; i++ {
		if stepID(i).key() == key {
			return stepID(i), true
		}
	}
	return 0, false
}

// loadSetupState reads the previous run's answers. A missing file is a fresh
// start; an unreadable one is reported and ignored, never fatal.
func loadSetupState(st *WizardState) (bool, error) {
	path, err := setupStatePath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the wizard's own state file
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var saved savedSetup
	if err := json.Unmarshal(data, &saved); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if saved.Version != setupStateFileVersion {
		return false, fmt.Errorf("%s: unsupported version %d", path, saved.Version)
	}
	st.fromSaved(saved)
	return true, nil
}

// saveSetupState writes the answers atomically with mode 0600 (#047).
func saveSetupState(st *WizardState) error {
	path, err := setupStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st.toSaved(), "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".setup-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// setStoreMode applies a secret-store choice to the installer flags.
func (st *WizardState) setStoreMode(mode string) {
	st.StoreMode = mode
	c := &st.Config
	c.InstallSecretStore = mode == storeInstallProd
	c.InstallSecretStoreDev = mode == storeInstallDev
	c.SecretStoreUndecided = false
	if mode != storeConnect {
		c.ArgoVaultAddress = ""
	}
	if mode != storeInstallProd {
		// Rekor needs persistent production OpenBao (installer.Validate).
		c.InstallRekor = false
	}
}

// displayHome shortens a path under the home directory to ~/...
func displayHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}
