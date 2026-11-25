package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"
)

type storeBootstrap struct {
	Keys  []string `json:"keys_base64"`
	Token string   `json:"root_token"`
}

func storeSettings(p *Plan) secretstore.Config {
	v, _ := inputs(p.release.chart, p.request.Values, p.choices.Namespace)
	store := v["secretStore"].(map[string]any)
	return secretstore.Config{
		Mount: fmt.Sprint(store["kv_mount"]), AuthMount: fmt.Sprint(store["auth_mount"]),
		WriterRole: fmt.Sprint(store["writer_role"]), StepRole: "oberth-step",
		ServerNamespace: p.choices.Namespace, PipelineNamespace: p.choices.PipelineNamespace,
		ServerAccount: "oberth", Audience: fmt.Sprint(store["audience"]),
	}
}
func (e *Engine) startStore(ctx context.Context, p *Plan, progress Progress) error {
	v, _ := inputs(p.release.chart, p.request.Values, p.choices.Namespace)
	if settingText(v, "secretStore", "bundled") != "on" {
		return nil
	}
	if err := emit(progress, StartingStore); err != nil {
		return err
	}
	cfg := storeSettings(p)
	if err := cfg.Validate(); err != nil {
		return err
	}
	client, close, err := e.storeClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer close()
	defer client.Close()
	return e.initializeStore(ctx, client, cfg)
}

// storeProbe bounds a review's read of the bundled store, whose Pod may not be running.
const storeProbe = 10 * time.Second

// upgradeStore refuses, in an ordinary upgrade's review, only a store state it
// read and the apply's store step would refuse. A store it cannot read within
// storeProbe, and a saved, resumable setup, are left to that step.
func (e *Engine) upgradeStore(ctx context.Context, p *Plan) error {
	pending, err := e.Credentials.read("store-init.json")
	clear(pending)
	if err == nil {
		return nil
	}
	if !isAbsent(err) {
		return described("custody", err, e.Credentials.pathFor("store-init.json"))
	}
	refusal, _ := e.storeState(ctx, p)
	return refusal
}

// storeReady holds a completion to a store it read within storeProbe and found
// ready: initialized, unsealed and matching its saved setup.
func (e *Engine) storeReady(ctx context.Context, p *Plan) error {
	refusal, unread := e.storeState(ctx, p)
	if refusal == nil && unread != nil {
		return problem("store-starting", unread)
	}
	return refusal
}

// storeState reads, without writing and within storeProbe, what an apply's store
// step would find. refusal is that step's refusal of what was read; unread is
// why the store could not be read. Both are nil without the bundled store.
func (e *Engine) storeState(ctx context.Context, p *Plan) (refusal, unread error) {
	v, _ := inputs(p.release.chart, p.request.Values, p.choices.Namespace)
	if settingText(v, "secretStore", "bundled") != "on" {
		return nil, nil
	}
	cfg := storeSettings(p)
	if err := cfg.Validate(); err != nil {
		return err, nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeProbe)
	defer cancel()
	client, close, err := e.storeClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer close()
	defer client.Close()
	status, err := readStoreStatus(ctx, client)
	if err != nil {
		return nil, err
	}
	if !*status.Initialized {
		return problem("store-init", nil), nil
	}
	return e.installedStore(*status.Sealed, cfg), nil
}

func (e *Engine) storeClient(ctx context.Context, cfg secretstore.Config) (*secretstore.Client, func(), error) {
	if e.Credentials == nil && e.OpenStore != nil { // The in-cluster backup account has no profile.
		return e.OpenStore(ctx, cfg)
	}
	ca, err := e.Credentials.profileCA()
	if err != nil {
		return nil, nil, err
	}
	if e.OpenStore != nil {
		return e.OpenStore(ctx, cfg)
	}
	return e.forwardStore(ctx, cfg, ca)
}

type storeStatus struct {
	Initialized *bool `json:"initialized"`
	Sealed      *bool `json:"sealed"`
}

func readStoreStatus(ctx context.Context, c *secretstore.Client) (storeStatus, error) {
	var status storeStatus
	if _, err := c.JSON(ctx, secretstore.Request{Method: "GET", Path: "/sys/seal-status"}, &status); err != nil {
		return status, err
	}
	if status.Initialized == nil || status.Sealed == nil {
		return status, problem("store-init", nil)
	}
	return status, nil
}

func (e *Engine) initializeStore(ctx context.Context, c *secretstore.Client, cfg secretstore.Config) error {
	status, err := readStoreStatus(ctx, c)
	if err != nil {
		return err
	}
	init, err := e.pendingStore(ctx, c, *status.Initialized)
	if err != nil {
		return err
	}
	if init == nil {
		return e.installedStore(*status.Sealed, cfg)
	}
	key, token := []byte(init.Keys[0]), []byte(init.Token)
	defer clear(key)
	defer clear(token)
	if err = e.Credentials.save("recovery", key); err != nil {
		return problem("custody", err)
	}
	if *status.Initialized && *status.Sealed {
		return problem("store-sealed", nil)
	}
	if !*status.Initialized {
		if err = e.unsealStore(ctx, c, key); err != nil {
			return err
		}
	}
	return e.configureStore(ctx, c, cfg, token)
}
func (e *Engine) installedStore(sealed bool, cfg secretstore.Config) error {
	if sealed {
		raw, err := e.Credentials.read("recovery")
		clear(raw)
		if isAbsent(err) {
			return problem("store-lost", nil)
		}
		if err != nil {
			return described("custody", err, e.Credentials.pathFor("recovery"))
		}
		return problem("store-sealed", nil)
	}
	receipt, err := e.Credentials.read("store-configured")
	if err != nil {
		return problem("store-init", err)
	}
	expected, _ := json.Marshal(cfg)
	if string(receipt) != publicDigest(expected) {
		return problem("store-drift", nil)
	}
	return nil
}
func (e *Engine) configureStore(ctx context.Context, c *secretstore.Client, cfg secretstore.Config, token []byte) error {
	if err := e.waitStoreActive(ctx, c); err != nil {
		return err
	}
	expected, _ := json.Marshal(cfg)
	digest := []byte(publicDigest(expected))
	receipt, err := e.Credentials.read("store-configured")
	if isAbsent(err) {
		if err = c.Setup(ctx, token, cfg); err != nil {
			return err
		}
		if err = e.Credentials.save("store-configured", digest); err != nil {
			return problem("custody", err)
		}
	} else if err != nil || string(receipt) != string(digest) {
		return problem("store-drift", err)
	}
	if err = revokeRoot(ctx, c, token); err != nil {
		return err
	}
	return errors.Join(e.Credentials.root.Remove("store-init.json"), e.Credentials.directory.Sync())
}

// A resumed bootstrap may have lost the successful revoke reply. Only an
// independently unauthorized lookup proves that its former root is unusable.
func revokeRoot(ctx context.Context, c *secretstore.Client, token []byte) error {
	status, err := c.JSON(ctx, secretstore.Request{Method: "POST", Path: "/auth/token/revoke-self", Token: token}, nil)
	if err == nil || status != 403 {
		return err
	}
	status, lookup := c.JSON(ctx, secretstore.Request{Method: "GET", Path: "/auth/token/lookup-self", Token: token, Accept: 403}, nil)
	if status == 403 && lookup == nil {
		return nil
	}
	return errors.Join(err, lookup)
}

func (e *Engine) unsealStore(ctx context.Context, c *secretstore.Client, key []byte) error {
	var result struct {
		Sealed *bool `json:"sealed"`
	}
	if status, err := c.JSON(ctx, secretstore.Request{Method: "POST", Path: "/sys/unseal", Body: map[string]string{"key": string(key)}}, &result); err != nil {
		if status == 400 {
			return described("unseal-rejected", nil, e.Credentials.pathFor("recovery"))
		}
		return err
	}
	if result.Sealed == nil || *result.Sealed {
		return problem("store-sealed", nil)
	}
	return nil
}

// UnsealStatus is the observed outcome of an explicit store-unseal action.
type UnsealStatus string

const (
	StoreUnsealed        UnsealStatus = "Secret store unsealed."
	StoreAlreadyUnsealed UnsealStatus = "Secret store was already unsealed."
)

// Unseal is an explicit operator action; normal reruns never open an installed sealed store.
func (e *Engine) Unseal(ctx context.Context) (UnsealStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	profile, err := e.Credentials.profile()
	if err != nil {
		return "", problem("custody", err)
	}
	uid, err := e.Cluster.UID(ctx)
	if err != nil {
		return "", problem("cluster", err)
	}
	if err = e.Credentials.checkTarget(e.Cluster.Context, uid, profile.Namespace); err != nil {
		return "", err
	}
	if profile.RecoveryKey == "" {
		return "", problem("store-external", nil)
	}
	key, err := e.Credentials.read("recovery")
	if err != nil {
		return "", described("unseal-key", err, e.Credentials.pathFor("recovery"))
	}
	defer clear(key)
	client, close, err := e.storeClient(ctx, secretstore.Config{ServerNamespace: profile.Namespace})
	if err != nil {
		return "", err
	}
	defer close()
	defer client.Close()
	status, err := readStoreStatus(ctx, client)
	if err != nil {
		return "", err
	}
	if !*status.Sealed {
		return StoreAlreadyUnsealed, nil
	}
	if err = e.unsealStore(ctx, client, key); err != nil {
		return "", err
	}
	return StoreUnsealed, nil
}

func (e *Engine) pendingStore(ctx context.Context, c *secretstore.Client, initialized bool) (*storeBootstrap, error) {
	raw, err := e.Credentials.read("store-init.json")
	defer clear(raw)
	if isAbsent(err) && initialized {
		return nil, nil
	}
	if err != nil && !isAbsent(err) {
		return nil, described("custody", err, e.Credentials.pathFor("store-init.json"))
	}
	var init storeBootstrap
	if isAbsent(err) {
		if _, err = c.JSON(ctx, secretstore.Request{Method: "POST", Path: "/sys/init", Body: map[string]int{"secret_shares": 1, "secret_threshold": 1}}, &init); err != nil {
			return nil, err
		}
		if !init.valid() {
			return nil, problem("store-init", nil)
		}
		raw, err = json.Marshal(init)
		if err != nil {
			return nil, err
		}
		defer clear(raw)
		if err = e.Credentials.save("store-init.json", raw); err != nil {
			return nil, problem("custody", err)
		}
	} else if json.Unmarshal(raw, &init) != nil || !init.valid() {
		return nil, problem("store-init", nil)
	}
	return &init, nil
}

func (b storeBootstrap) valid() bool { return len(b.Keys) == 1 && b.Keys[0] != "" && b.Token != "" }

func (e *Engine) reviewPendingStore(p *Plan) error {
	pending, err := e.Credentials.read("store-init.json")
	clear(pending)
	if err != nil && !isAbsent(err) {
		return described("custody", err, e.Credentials.pathFor("store-init.json"))
	}
	if err == nil {
		p.review = append(p.review, "Store setup resumes with the saved root token, which is then revoked.")
	}
	return nil
}
