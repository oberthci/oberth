package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/store"
)

// promote tag_after (#1524). The tag is pushed in the same logical operation
// as the promotion: only after the promotion passed, only when the upstream
// target equals the promotion's result, on that result commit, and never
// with force. A transient upstream failure is retried on the publication
// retry schedule; every other outcome is recorded as failed with a reason.

var releaseVersionTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+`)

type annotatedTagPublisher interface {
	PublishAnnotatedTag(context.Context, string, string, string, string) (string, error)
}

type mirrorTagReader interface {
	TagSHA(context.Context, string, string) (string, error)
}

// checkTagAfter refuses tag_after before admission when it can never apply.
// It creates a new upstream ref, which no promotion grant or repository
// promotion policy covers: only an admin uplink may ask for it.
func (service *API) checkTagAfter(ctx context.Context, actor api.Actor, arguments promotionArguments) error {
	tag := arguments.TagAfter
	if !actor.Admin {
		return refuse(ErrForbidden, "tag_after requires an admin uplink: a promotion grant covers the promotion, not a new upstream tag")
	}
	if err := gitcache.ValidateTag(tag.Name); err != nil || strings.HasPrefix(tag.Name, "refs/") {
		return refuse(ErrInvalidInput, "tag_after name %q is not a valid tag name (no refs/tags/ prefix)", tag.Name)
	}
	if releaseVersionTag.MatchString(tag.Name) {
		return refuse(ErrInvalidInput, "tag_after refuses release-version names; push the release tag through Oberth to run release CI")
	}
	if strings.TrimSpace(tag.Message) == "" || len(tag.Message) > store.MaxPromotionTagMessage || strings.ContainsRune(tag.Message, 0) {
		return refuse(ErrInvalidInput, "tag_after message is required, at most %d bytes", store.MaxPromotionTagMessage)
	}
	if _, ok := service.git.(annotatedTagPublisher); !ok || service.agentEvents == nil {
		return refuse(ErrUnavailable, "tag_after")
	}
	sha := arguments.SHA
	if sha == "" && len(arguments.Sources) > 0 {
		sha = arguments.Sources[0].SHA
	}
	repository, _, err := service.resolvePromotionSource(ctx, arguments.Repo, sha)
	if err != nil {
		return err
	}
	if reader, ok := service.git.(mirrorTagReader); ok {
		if existing, err := reader.TagSHA(ctx, service.cacheInputFor(ctx, repository), tag.Name); err == nil && existing != "" {
			return refuse(ErrInvalidInput, "tag %s already exists at %s; tag_after never moves a tag", tag.Name, shortSHA(existing))
		}
	}
	return nil
}

// requestPromotionTag records the request for an admitted promotion.
func (service *API) requestPromotionTag(ctx context.Context, actor api.Actor, promotion model.Promotion, tag promotionTagArgument) string {
	if err := service.agentEvents.store.RequestPromotionTag(ctx, promotion.ID, actor.Identity, tag.Name, tag.Message); err != nil {
		log.Printf("WARN: tag_after request for promotion %s not recorded", promotion.ID)
		return "not recorded; no tag will be pushed"
	}
	service.signals.NotifyPromotion(promotion.ID)
	return model.PromotionTagPending
}

// settlePromotionTag decides one pending request. A transient failure is
// retried after pushretry.Delay, at most pushretry.MaxAttempts times.
func (service *API) settlePromotionTag(ctx context.Context, tags AgentEventStore, promotion model.Promotion, request model.PromotionTag, now time.Time) model.PromotionTag {
	if request.NextRetryAt != nil && now.Before(*request.NextRetryAt) {
		return request
	}
	status, reason, object, attempted, retry := service.decidePromotionTag(ctx, promotion, request)
	attempts := request.Attempts
	if attempted {
		attempts++
	}
	if retry && attempts < pushretry.MaxAttempts {
		updated, err := tags.RetryPromotionTag(ctx, promotion.ID, attempts, now.Add(max(pushretry.Delay(attempts), time.Minute)), reason)
		if err != nil {
			log.Printf("WARN: tag_after retry for promotion %s not recorded", promotion.ID)
			return request
		}
		return updated
	}
	if retry {
		status, reason = model.PromotionTagFailed, fmt.Sprintf("gave up after %d attempts: %s", attempts, reason)
	}
	settled, err := tags.FinishPromotionTag(ctx, promotion.ID, status, reason, object, attempts)
	if err != nil {
		log.Printf("WARN: tag_after outcome for promotion %s not recorded", promotion.ID)
		return request
	}
	return settled
}

func (service *API) decidePromotionTag(ctx context.Context, promotion model.Promotion, request model.PromotionTag) (status, reason, object string, attempted, retry bool) {
	failed := func(format string, values ...any) (string, string, string, bool, bool) {
		return model.PromotionTagFailed, fmt.Sprintf(format, values...), "", false, false
	}
	switch {
	case promotion.Status != model.PromotionPassed:
		return failed("promotion %s; tag %s not pushed", promotion.Status, request.Name)
	case promotion.ResultSHA == "":
		return failed("the promotion recorded no result; tag %s not pushed", request.Name)
	case service.repositories == nil:
		return failed("repositories are unavailable; tag %s not pushed", request.Name)
	}
	publisher, ok := service.git.(annotatedTagPublisher)
	remote, remoteOK := service.git.(interface {
		RemoteRef(context.Context, string, string) (string, bool, error)
	})
	if !ok || !remoteOK {
		return failed("tag publication is unavailable; tag %s not pushed", request.Name)
	}
	if err := service.requireMutation(ctx); err != nil {
		// The push is audited: wait for the audit gate, never bypass it.
		return model.PromotionTagPending, "audit integrity unavailable", "", false, true
	}
	repository, err := service.repositories.Repository(ctx, promotion.RepoID)
	if err != nil {
		return failed("repository unavailable; tag %s not pushed", request.Name)
	}
	input := service.cacheInputFor(ctx, repository)
	target := strings.TrimPrefix(promotion.TargetRef, "refs/heads/")
	upstream, exists, err := remote.RemoteRef(ctx, input, "refs/heads/"+target)
	if err != nil {
		return model.PromotionTagPending, "upstream " + target + " could not be read", "", true, true
	}
	if !exists || !sameOID(upstream, promotion.ResultSHA) {
		return failed("upstream %s is %s, not the promotion result %s; tag %s not pushed", target, shortSHA(upstream), shortSHA(promotion.ResultSHA), request.Name)
	}
	object, err = publisher.PublishAnnotatedTag(ctx, input, request.Name, promotion.ResultSHA, request.Message)
	switch {
	case err == nil:
		return model.PromotionTagPushed, fmt.Sprintf("pushed annotated tag %s (object %s) on %s after upstream %s equals the promotion result",
			request.Name, shortSHA(object), shortSHA(promotion.ResultSHA), target), object, true, false
	case errors.Is(err, gitcache.ErrTagExists):
		return model.PromotionTagFailed, fmt.Sprintf("tag %s already exists at another commit; never moved", request.Name), "", true, false
	case pushretry.Transient(err):
		return model.PromotionTagPending, "transient upstream failure: " + boundedText(err.Error(), agentEventDataErrorLength), "", true, true
	default:
		return model.PromotionTagFailed, "tag push refused: " + boundedText(err.Error(), agentEventDataErrorLength), "", true, false
	}
}
