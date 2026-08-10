package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func inactiveLifecycleReason(mode lifecycle.Mode) (string, error) {
	switch mode {
	case lifecycle.ModeMonitorOnly:
		return "lifecycle_monitor_only", nil
	case lifecycle.ModeDraining:
		return "lifecycle_draining", nil
	case lifecycle.ModeOff:
		return "lifecycle_off", nil
	default:
		return "", fmt.Errorf("unsupported lifecycle mode %q", mode)
	}
}

type RouterStore interface {
	LifecycleMode(context.Context) (lifecycle.Mode, error)
	ClaimWebhook(context.Context, time.Duration) (*ClaimedDelivery, error)
	RegistrationByRepository(context.Context, string) (Registration, error)
	BindWebhook(context.Context, ClaimedDelivery, Registration) error
	EnqueueWork(
		context.Context,
		Registration,
		string,
		string,
		WorkItem,
	) (string, bool, error)
	FinishWebhook(context.Context, ClaimedDelivery, string, string) error
	RecoverInterruptedWebhooks(context.Context) (int64, error)
}

type Router struct {
	Store    RouterStore
	Interval time.Duration
	Lease    time.Duration
	Wake     chan struct{}
	Log      *slog.Logger
}

func (router *Router) Signal() {
	select {
	case router.Wake <- struct{}{}:
	default:
	}
}

func (router *Router) Run(ctx context.Context) error {
	ticker := time.NewTicker(router.Interval)
	defer ticker.Stop()
	for {
		if _, err := router.Store.RecoverInterruptedWebhooks(ctx); err != nil {
			router.Log.Error("webhook recovery failed", "error", err)
		} else if err := router.drain(ctx); err != nil {
			router.Log.Error("webhook routing failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-router.Wake:
		}
	}
}

func (router *Router) drain(ctx context.Context) error {
	for {
		claim, err := router.Store.ClaimWebhook(ctx, router.Lease)
		if err != nil {
			return err
		}
		if claim == nil {
			return nil
		}
		if err := router.route(ctx, *claim); err != nil {
			if finishErr := router.Store.FinishWebhook(
				ctx,
				*claim,
				"failed",
				err.Error(),
			); finishErr != nil {
				return fmt.Errorf("route: %v; persist failure: %w", err, finishErr)
			}
		}
	}
}

func (router *Router) route(ctx context.Context, claim ClaimedDelivery) error {
	registration, err := router.Store.RegistrationByRepository(ctx, claim.Repository)
	if errors.Is(err, ErrNotFound) {
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			"unregistered_repository",
		)
	}
	if err != nil {
		return err
	}
	if !registration.Enabled {
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			"disabled_repository",
		)
	}
	if claim.RegistrationID != nil && *claim.RegistrationID != registration.ID {
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			"registration_identity_changed",
		)
	}
	if claim.RegistrationVersion != nil && *claim.RegistrationVersion != registration.Version {
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			"stale_registration_version",
		)
	}
	if !eventAllowed(registration, claim.Event) {
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			"monitor_capability_disabled",
		)
	}
	if err := router.Store.BindWebhook(ctx, claim, registration); err != nil {
		if errors.Is(err, ErrStaleRegistration) {
			return router.Store.FinishWebhook(
				ctx,
				claim,
				"ignored",
				"stale_registration_version",
			)
		}
		return err
	}
	if !registration.ExecutionEnabled {
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			"execution_disabled",
		)
	}
	mode, err := router.Store.LifecycleMode(ctx)
	if err != nil {
		return err
	}
	if mode != lifecycle.ModeActive {
		reason, err := inactiveLifecycleReason(mode)
		if err != nil {
			return err
		}
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			reason,
		)
	}
	item, ok := workItemFromWebhook(claim)
	if !ok {
		return router.Store.FinishWebhook(
			ctx,
			claim,
			"ignored",
			"event_not_job_addressable",
		)
	}
	if _, _, err := router.Store.EnqueueWork(
		ctx,
		registration,
		"webhook",
		claim.DeliveryKey,
		item,
	); err != nil {
		return err
	}
	return router.Store.FinishWebhook(ctx, claim, "processed", "")
}

func workItemFromWebhook(claim ClaimedDelivery) (WorkItem, bool) {
	kind := ""
	var entity map[string]any
	switch claim.Event {
	case "issues", "issue_comment":
		kind = WorkItemKindIssue
		entity, _ = claim.Payload["issue"].(map[string]any)
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		kind = WorkItemKindPullRequest
		entity, _ = claim.Payload["pull_request"].(map[string]any)
	case "check_run", "check_suite":
		entity, _ = claim.Payload[claim.Event].(map[string]any)
		if entity == nil {
			return WorkItem{}, false
		}
		identity, ok := jsonNumber(entity["id"])
		if !ok {
			return WorkItem{}, false
		}
		updatedRaw, _ := entity["updated_at"].(string)
		updatedAt, err := time.Parse(time.RFC3339, updatedRaw)
		if err != nil {
			return WorkItem{}, false
		}
		return WorkItem{
			Repository:  claim.Repository,
			Kind:        WorkItemKindRepositoryHead,
			SourceEvent: claim.Event,
			Identity:    strconv.FormatInt(identity, 10),
			UpdatedAt:   updatedAt,
			Payload: map[string]any{
				"action": optionalStringValue(claim.Action),
			},
		}, true
	case "push":
		after, _ := claim.Payload["after"].(string)
		after = strings.ToLower(strings.TrimSpace(after))
		ref, _ := claim.Payload["ref"].(string)
		deleted, _ := claim.Payload["deleted"].(bool)
		if deleted || !commitSHAPattern.MatchString(after) ||
			!strings.HasPrefix(ref, "refs/heads/") {
			return WorkItem{}, false
		}
		var updatedAt time.Time
		if headCommit, ok := claim.Payload["head_commit"].(map[string]any); ok {
			timestamp, _ := headCommit["timestamp"].(string)
			updatedAt, _ = time.Parse(time.RFC3339, timestamp)
		}
		return WorkItem{
			Repository:  claim.Repository,
			Kind:        WorkItemKindRepositoryHead,
			SourceEvent: "push",
			// GitHub can push the same commit to multiple branches. Include a
			// length-framed ref so each branch remains a distinct logical event.
			Identity:  fmt.Sprintf("%d:%s:%s", len(ref), ref, after),
			UpdatedAt: updatedAt,
			Payload: map[string]any{
				"ref": ref, "after": after, "deleted": false,
			},
		}, true
	default:
		return WorkItem{}, false
	}
	if entity == nil {
		return WorkItem{}, false
	}
	number, ok := jsonNumber(entity["number"])
	if !ok {
		return WorkItem{}, false
	}
	updatedRaw, _ := entity["updated_at"].(string)
	updatedAt, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return WorkItem{}, false
	}
	return WorkItem{
		Repository:  claim.Repository,
		Kind:        kind,
		SourceEvent: claim.Event,
		Number:      number,
		UpdatedAt:   updatedAt,
	}, true
}

func optionalStringValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
