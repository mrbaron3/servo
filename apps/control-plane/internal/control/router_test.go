package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

type fakeRouterStore struct {
	registration Registration
	lookupError  error
	finished     []string
	enqueued     []WorkItem
	bound        int
	bindError    error
	enqueueError error
	currentMode  lifecycle.Mode
	lifecycleErr error
}

func (store *fakeRouterStore) LifecycleMode(context.Context) (lifecycle.Mode, error) {
	if store.lifecycleErr != nil {
		return "", store.lifecycleErr
	}
	if store.currentMode == "" {
		return lifecycle.ModeActive, nil
	}
	return store.currentMode, nil
}

func (store *fakeRouterStore) ClaimWebhook(context.Context, time.Duration) (*ClaimedDelivery, error) {
	return nil, nil
}

func (store *fakeRouterStore) RegistrationByRepository(
	context.Context,
	string,
) (Registration, error) {
	return store.registration, store.lookupError
}

func (store *fakeRouterStore) BindWebhook(
	context.Context,
	ClaimedDelivery,
	Registration,
) error {
	store.bound++
	return store.bindError
}

func (store *fakeRouterStore) EnqueueWork(
	_ context.Context,
	_ Registration,
	_, _ string,
	item WorkItem,
) (string, bool, error) {
	store.enqueued = append(store.enqueued, item)
	return "job-1", false, store.enqueueError
}

func (store *fakeRouterStore) FinishWebhook(
	_ context.Context,
	_ ClaimedDelivery,
	status, reason string,
) error {
	store.finished = append(store.finished, status+":"+reason)
	return nil
}

func (store *fakeRouterStore) RecoverInterruptedWebhooks(context.Context) (int64, error) {
	return 0, nil
}

func TestRouterFailsClosedForUnknownDisabledAndStaleRegistration(t *testing.T) {
	base := ClaimedDelivery{
		ID: "delivery-1", Repository: "owner/repo", Event: "issues",
		Payload: issuePayload(1, "2026-07-25T00:00:00Z"),
	}
	tests := []struct {
		name         string
		store        fakeRouterStore
		claim        ClaimedDelivery
		wantFinished string
	}{
		{
			name:         "unregistered",
			store:        fakeRouterStore{lookupError: ErrNotFound},
			claim:        base,
			wantFinished: "ignored:unregistered_repository",
		},
		{
			name: "disabled",
			store: fakeRouterStore{registration: Registration{
				ID: "registration-1", Repository: "owner/repo", Version: 1,
			}},
			claim:        base,
			wantFinished: "ignored:disabled_repository",
		},
		{
			name: "stale",
			store: fakeRouterStore{registration: Registration{
				ID: "registration-1", Repository: "owner/repo", Enabled: true,
				IssueMonitorEnabled: true, ExecutionEnabled: true, Version: 2,
			}},
			claim: func() ClaimedDelivery {
				claim := base
				version := int64(1)
				claim.RegistrationVersion = &version
				return claim
			}(),
			wantFinished: "ignored:stale_registration_version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := Router{Store: &test.store}
			if err := router.route(context.Background(), test.claim); err != nil {
				t.Fatal(err)
			}
			if len(test.store.enqueued) != 0 {
				t.Fatal("fail-closed delivery created a job")
			}
			if len(test.store.finished) != 1 || test.store.finished[0] != test.wantFinished {
				t.Fatalf("finished = %#v", test.store.finished)
			}
		})
	}
}

func TestWebhookAndPollUseSameJobIdentity(t *testing.T) {
	claim := ClaimedDelivery{
		ID: "delivery-1", DeliveryKey: "github-delivery",
		Repository: "owner/repo", Event: "pull_request",
		Payload: map[string]any{
			"pull_request": map[string]any{
				"number":     42.0,
				"updated_at": "2026-07-25T00:00:00Z",
			},
		},
	}
	webhook, ok := workItemFromWebhook(claim)
	if !ok {
		t.Fatal("workItemFromWebhook() rejected valid PR")
	}
	poll := WorkItem{
		Repository: "owner/repo", Kind: WorkItemKindPullRequest, Number: 42,
		UpdatedAt: time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC),
	}
	if webhook.IdempotencyKey() != poll.IdempotencyKey() {
		t.Fatalf("webhook key %q != poll key %q", webhook.IdempotencyKey(), poll.IdempotencyKey())
	}
	if webhook.Kind != WorkItemKindPullRequest || webhook.SourceEvent != "pull_request" {
		t.Fatalf("webhook item did not separate entity and event: %#v", webhook)
	}
	webhookPayload := webhook.CanonicalPayload()
	delete(webhookPayload, "sourceEvent")
	if !jsonEqual(webhookPayload, poll.CanonicalPayload()) {
		t.Fatal("webhook and poll payloads did not converge")
	}
}

func TestRouterLeavesFailedWorkForDurableRetry(t *testing.T) {
	store := &fakeRouterStore{
		registration: Registration{
			ID: "registration-1", Repository: "owner/repo", Enabled: true,
			IssueMonitorEnabled: true, ExecutionEnabled: true, Version: 1,
		},
		enqueueError: errors.New("single-flight busy"),
	}
	router := Router{Store: store}
	claim := ClaimedDelivery{
		ID: "delivery-1", DeliveryKey: "delivery-key", Token: "token",
		Repository: "owner/repo", Event: "issues",
		Payload: issuePayload(1, "2026-07-25T00:00:00Z"),
	}
	if err := router.route(context.Background(), claim); err == nil {
		t.Fatal("route() unexpectedly hid enqueue error")
	}
}

func TestRouterPreservesInactiveLifecycleModeWithoutExecutableWork(t *testing.T) {
	tests := []struct {
		mode   lifecycle.Mode
		reason string
	}{
		{mode: lifecycle.ModeMonitorOnly, reason: "lifecycle_monitor_only"},
		{mode: lifecycle.ModeDraining, reason: "lifecycle_draining"},
		{mode: lifecycle.ModeOff, reason: "lifecycle_off"},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			store := &fakeRouterStore{registration: Registration{
				ID: "registration-1", Repository: "owner/repo", Enabled: true,
				IssueMonitorEnabled: true, ExecutionEnabled: true, Version: 1,
			}, currentMode: test.mode}
			router := Router{Store: store}
			claim := ClaimedDelivery{
				ID: "delivery-1", DeliveryKey: "delivery-key", Token: "token",
				Repository: "owner/repo", Event: "issues",
				Payload: issuePayload(1, "2026-07-25T00:00:00Z"),
			}
			if err := router.route(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			if len(store.enqueued) != 0 ||
				store.bound != 1 ||
				len(store.finished) != 1 ||
				store.finished[0] != "ignored:"+test.reason {
				t.Fatalf(
					"%s route enqueued=%#v finished=%#v",
					test.mode,
					store.enqueued,
					store.finished,
				)
			}
		})
	}
}

func TestRouterFailsClosedWhenLifecycleAuthorityIsUnavailable(t *testing.T) {
	store := &fakeRouterStore{registration: Registration{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, ExecutionEnabled: true, Version: 1,
	}, lifecycleErr: ErrStoreUnavailable}
	router := Router{Store: store}
	claim := ClaimedDelivery{
		ID: "delivery-1", DeliveryKey: "delivery-key", Token: "token",
		Repository: "owner/repo", Event: "issues",
		Payload: issuePayload(1, "2026-07-25T00:00:00Z"),
	}
	if err := router.route(context.Background(), claim); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("route() error = %v, want store unavailable", err)
	}
	if store.bound != 1 || len(store.enqueued) != 0 || len(store.finished) != 0 {
		t.Fatalf(
			"unavailable lifecycle bound=%d enqueued=%#v finished=%#v",
			store.bound,
			store.enqueued,
			store.finished,
		)
	}
}

func TestRouterUsesAuthoritativeActiveLifecycle(t *testing.T) {
	store := &fakeRouterStore{registration: Registration{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, ExecutionEnabled: true, Version: 1,
	}, currentMode: lifecycle.ModeActive}
	router := Router{Store: store}
	claim := ClaimedDelivery{
		ID: "delivery-1", DeliveryKey: "delivery-key", Token: "token",
		Repository: "owner/repo", Event: "issues",
		Payload: issuePayload(1, "2026-07-25T00:00:00Z"),
	}
	if err := router.route(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if len(store.enqueued) != 1 || len(store.finished) != 1 ||
		store.finished[0] != "processed:" {
		t.Fatalf("ACTIVE lifecycle enqueued=%#v finished=%#v", store.enqueued, store.finished)
	}
}

func TestCheckAndPushEventsBecomeDurableTypedWork(t *testing.T) {
	store := &fakeRouterStore{registration: Registration{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		PRMonitorEnabled: true, ExecutionEnabled: true, Version: 1,
	}}
	router := Router{Store: store}
	action := "completed"
	events := []ClaimedDelivery{
		{
			ID: "delivery-check", DeliveryKey: "check-key", Token: "token",
			Repository: "owner/repo", Event: "check_run", Action: &action,
			Payload: map[string]any{
				"check_run": map[string]any{
					"id":         123.0,
					"updated_at": "2026-07-25T00:00:00Z",
				},
			},
		},
		{
			ID: "delivery-push", DeliveryKey: "push-key", Token: "token",
			Repository: "owner/repo", Event: "push",
			Payload: map[string]any{
				"after":   strings.Repeat("a", 40),
				"ref":     "refs/heads/main",
				"deleted": false,
				"head_commit": map[string]any{
					"timestamp": "2026-07-25T00:01:00Z",
				},
			},
		},
	}
	for _, claim := range events {
		if err := router.route(context.Background(), claim); err != nil {
			t.Fatalf("%s route: %v", claim.Event, err)
		}
	}
	if len(store.enqueued) != 2 ||
		store.enqueued[0].Kind != WorkItemKindRepositoryHead ||
		store.enqueued[0].SourceEvent != "check_run" ||
		store.enqueued[0].Identity != "123" ||
		store.enqueued[1].Kind != WorkItemKindRepositoryHead ||
		store.enqueued[1].SourceEvent != "push" ||
		store.enqueued[1].Identity !=
			"15:refs/heads/main:"+strings.Repeat("a", 40) {
		t.Fatalf("typed work = %#v", store.enqueued)
	}
	if len(store.finished) != 2 ||
		store.finished[0] != "processed:" ||
		store.finished[1] != "processed:" {
		t.Fatalf("finished = %#v", store.finished)
	}
}

func TestPushIdentityIncludesBranchRef(t *testing.T) {
	sha := strings.Repeat("a", 40)
	keys := make([]string, 0, 2)
	for _, ref := range []string{"refs/heads/main", "refs/heads/release"} {
		item, ok := workItemFromWebhook(ClaimedDelivery{
			Repository: "owner/repo",
			Event:      "push",
			Payload: map[string]any{
				"after": sha, "ref": ref, "deleted": false,
			},
		})
		if !ok {
			t.Fatalf("push for %s was rejected", ref)
		}
		keys = append(keys, item.IdempotencyKey())
	}
	if keys[0] == keys[1] {
		t.Fatalf("different branch pushes aliased: %q", keys[0])
	}
}

func issuePayload(number float64, updatedAt string) map[string]any {
	return map[string]any{
		"issue": map[string]any{"number": number, "updated_at": updatedAt},
	}
}
