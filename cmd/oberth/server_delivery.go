package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/strictjson"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *composition) stepDelivery() runtimeconfig.SecretDelivery {
	d := c.delivery
	_, port, _ := net.SplitHostPort(c.config.Listen.Source)
	d.BrokerAddress = "https://" + net.JoinHostPort("oberth-source."+c.config.Namespace+".svc", port)
	d.BrokerServerName = "oberth." + c.config.Namespace + ".svc"
	c.identity.mu.Lock()
	d.BrokerCAPEM = string(c.identity.certBytes)
	c.identity.mu.Unlock()
	return d
}

func (o *serverOwners) deliveryIdentity(ctx context.Context, caller sourcedelivery.Caller) (runlog.InfrastructureReceipt, error) {
	w, err := o.cluster.Argo.ArgoprojV1alpha1().Workflows(caller.Namespace).Get(ctx, caller.Workflow, meta.GetOptions{})
	if err != nil {
		return runlog.InfrastructureReceipt{}, err
	}
	if string(w.UID) != caller.UID || caller.Namespace != o.c.config.PipelineNamespace {
		return runlog.InfrastructureReceipt{}, &sourcedelivery.Failure{Code: "forbidden"}
	}
	return argojob.DeliveryIdentity(w, caller.Pod)
}

func (o *serverOwners) recordDelivery(ctx context.Context, caller sourcedelivery.Caller, result runlog.InfrastructureReceipt) error {
	finished := o.c.clock.Now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	r, err := o.deliveryIdentity(ctx, caller)
	if err != nil {
		return err
	}
	r.Kind = result.Kind
	r.StartedAt, r.FinishedAt, r.Delivered = result.StartedAt, finished, result.Delivered
	return o.persistDelivery(ctx, r)
}

func (o *serverOwners) persistDelivery(ctx context.Context, r runlog.InfrastructureReceipt) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return o.c.logs.RecordInfrastructure(ctx, r)
}

func (o *serverOwners) secretDeliveryHandler(authenticate func(context.Context, string) (sourcedelivery.Caller, error)) http.Handler {
	slots := make(chan struct{}, max(1, o.settings.SourceClientBurst/3))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(405)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.WriteHeader(502)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		jwt := r.Header.Get("X-Vault-Token")
		if len(jwt) == 0 || len(jwt) > 64<<10 {
			w.WriteHeader(401)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(o.c.clock.Now().Add(10 * time.Second))
		input, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		var request secretstore.BrokerRequest
		if err != nil || strictjson.DecodeStrictObject(input, &request, 4096) != nil || !workflowpolicy.ValidSecretPath(request.Path) {
			w.WriteHeader(400)
			return
		}
		caller, err := authenticate(ctx, jwt)
		if err != nil {
			w.WriteHeader(secretDeliveryStatus(err))
			return
		}
		value, err := o.fetchStepSecret(ctx, caller, jwt, request.Path)
		if err != nil {
			w.WriteHeader(secretDeliveryStatus(err))
			return
		}
		defer clear(value)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(secretstore.BrokerResponse{Value: value})
	})
}

func (o *serverOwners) fetchStepSecret(ctx context.Context, caller sourcedelivery.Caller, jwt, path string) ([]byte, error) {
	started := o.c.clock.Now()
	identity, err := o.deliveryIdentity(ctx, caller)
	if err != nil {
		return nil, err
	}
	run, err := o.deliveryRun(ctx, identity.RunID)
	if err == nil && (terminalRun(run.Status) || run.Origin.WorkflowUID != identity.WorkflowUID || run.Admission.Identity.Namespace != caller.Namespace) {
		return nil, &sourcedelivery.Failure{Code: "forbidden"}
	}
	var values map[string][]byte
	if err == nil {
		values, err = o.c.storeWriter.Client.Fetch(ctx, o.c.storeWriter.Config, []byte(jwt), []string{path})
	}
	// Identity was verified before the operation. A native failure arriving
	// during it must not erase the exact Pod's completed delivery evidence.
	identity.Kind = "secret"
	identity.StartedAt, identity.FinishedAt, identity.Delivered = started, o.c.clock.Now(), err == nil
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) && deliveryServerFailure(err) {
		err = errors.Join(err, o.persistDelivery(ctx, identity))
	}
	if err != nil {
		return nil, err
	}
	if err = o.persistDelivery(ctx, identity); err != nil {
		clear(values[path])
		return nil, err
	}
	return values[path], nil
}

// A transient database error does not terminate a delivery. Keep its read
// within this request's existing deadline, before recording an unrecoverable
// failure. This never starts or retries workflow execution.
func (o *serverOwners) deliveryRun(ctx context.Context, id string) (stateport.Run, error) {
	backoff := 100 * time.Millisecond
	for {
		r, err := o.c.store.Run(ctx, id)
		if err == nil || !deliveryServerFailure(err) {
			return r, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return r, err
		case <-timer.C:
		}
		backoff = min(backoff*2, time.Second)
	}
}

func deliveryServerFailure(err error) bool {
	switch publicerr.Classify(err).Code() {
	case publicerr.Unavailable, publicerr.Invariant, publicerr.SchemaRefused:
		return true
	default:
		return false
	}
}

func secretDeliveryStatus(err error) int {
	var failure *sourcedelivery.Failure
	if errors.As(err, &failure) && (failure.Code == "forbidden" || failure.Code == "unauthenticated") {
		return 403
	}
	p := publicerr.Classify(err)
	switch p.Code() {
	case publicerr.NotFound:
		return 404
	case publicerr.Unavailable, publicerr.Invariant, publicerr.SchemaRefused:
		if p.Reason() == "Secret store is sealed." {
			return 503
		}
		return 502
	default:
		return 403
	}
}
