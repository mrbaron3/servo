package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/reporoot"
)

func TestRegistrationPublishedFixtureMatchesGoModel(t *testing.T) {
	root, err := reporoot.Find(".")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(
		root,
		"contracts",
		"control-store",
		"v1",
		"fixtures",
		"registration.valid.json",
	)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var registration Registration
	if err := json.Unmarshal(body, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.Repository != "mrbaron3/workflow" ||
		registration.Version != 1 ||
		!registration.Enabled ||
		!registration.IssueMonitorEnabled ||
		!registration.PRMonitorEnabled ||
		!registration.ExecutionEnabled {
		t.Fatalf("unexpected Registration fixture: %#v", registration)
	}
	if !validJSONObject(registration.Configuration) {
		t.Fatal("configuration did not remain a JSON object")
	}
	if registration.MergeMethod() != "rebase" ||
		(Registration{Configuration: json.RawMessage(`{}`)}).MergeMethod() != "squash" {
		t.Fatal("Registration integration strategy did not preserve explicit/default values")
	}
}

func TestWorkItemProjectsOnlyVersionedRunnerPayload(t *testing.T) {
	tests := []struct {
		name string
		item WorkItem
		kind string
		mode string
	}{
		{
			name: "pull request",
			item: WorkItem{
				Repository: "owner/repo", Kind: WorkItemKindPullRequest, Number: 38,
			},
			kind: "pull_request",
			mode: "pr_reconciliation",
		},
		{
			name: "repository event",
			item: WorkItem{
				Repository:  "owner/repo",
				Kind:        WorkItemKindRepositoryHead,
				SourceEvent: "push",
				Identity:    strings.Repeat("a", 40),
				Payload: map[string]any{
					"ref":              "refs/heads/main",
					"after":            strings.Repeat("a", 40),
					"untrustedCommand": "curl attacker.invalid",
				},
			},
			kind: "repository",
			mode: "pr_reconciliation",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := test.item.RunnerPayload("webhook", "rebase")
			if err != nil {
				t.Fatal(err)
			}
			event, _ := payload["event"].(map[string]any)
			execution, _ := payload["execution"].(map[string]any)
			if payload["schemaVersion"] != 1 ||
				event["kind"] != test.kind ||
				execution["mode"] != test.mode ||
				execution["readyLabel"] != "ready" ||
				execution["claimedLabel"] != "agent-claimed" ||
				execution["mergeMethod"] != "rebase" {
				t.Fatalf("unexpected runner payload: %#v", payload)
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{
				"untrustedCommand", "curl attacker.invalid", "cloneUrl", "hostPath",
			} {
				if strings.Contains(string(body), forbidden) {
					t.Fatalf("runner payload forwarded %q: %s", forbidden, body)
				}
			}
		})
	}
}

func TestWorkItemCanonicalKindsPreserveLegacyIdempotencyKeys(t *testing.T) {
	updatedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		legacy    WorkItem
		canonical WorkItem
	}{
		{
			name: "pull request",
			legacy: WorkItem{
				Repository: "owner/repo", Kind: "pull_request", Number: 42,
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: "owner/repo", Kind: WorkItemKindPullRequest, Number: 42,
				UpdatedAt: updatedAt,
			},
		},
		{
			name: "push",
			legacy: WorkItem{
				Repository: "owner/repo", Kind: "push", Identity: "head-identity",
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: "owner/repo", Kind: WorkItemKindRepositoryHead,
				SourceEvent: "push", Identity: "head-identity", UpdatedAt: updatedAt,
			},
		},
		{
			name: "check run",
			legacy: WorkItem{
				Repository: "owner/repo", Kind: "check_run", Identity: "123",
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: "owner/repo", Kind: WorkItemKindRepositoryHead,
				SourceEvent: "check_run", Identity: "123", UpdatedAt: updatedAt,
			},
		},
		{
			name: "check suite",
			legacy: WorkItem{
				Repository: "owner/repo", Kind: "check_suite", Identity: "456",
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: "owner/repo", Kind: WorkItemKindRepositoryHead,
				SourceEvent: "check_suite", Identity: "456", UpdatedAt: updatedAt,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.legacy.IdempotencyKey() != test.canonical.IdempotencyKey() {
				t.Fatalf(
					"legacy key %q != canonical key %q",
					test.legacy.IdempotencyKey(),
					test.canonical.IdempotencyKey(),
				)
			}
			legacyType, legacyPayload, legacyErr := test.legacy.QueuedJob("webhook", "squash")
			canonicalType, canonicalPayload, canonicalErr :=
				test.canonical.QueuedJob("webhook", "squash")
			if legacyErr != nil || canonicalErr != nil ||
				legacyType != canonicalType ||
				!jsonEqual(legacyPayload, canonicalPayload) {
				t.Fatalf(
					"legacy job = %s %#v %v; canonical job = %s %#v %v",
					legacyType,
					legacyPayload,
					legacyErr,
					canonicalType,
					canonicalPayload,
					canonicalErr,
				)
			}
		})
	}
}

func TestComponentProjectionDoesNotReintroduceLegacyAliases(t *testing.T) {
	projection := reflect.TypeOf(ComponentProjection{})
	for _, name := range []string{"State", "LastHealthyAt", "Stale"} {
		if _, present := projection.FieldByName(name); present {
			t.Fatalf("ComponentProjection reintroduced legacy field %s", name)
		}
	}
}

func TestIssueWorkProjectsOnlyVersionedTriagePayload(t *testing.T) {
	item := WorkItem{
		Repository: "owner/repo",
		Kind:       "issue",
		Number:     14,
		UpdatedAt:  time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
		Payload: map[string]any{
			"title":            "must not cross the queue",
			"untrustedCommand": "curl attacker.invalid",
		},
	}
	jobType, payload, err := item.QueuedJob("webhook", "merge")
	if err != nil {
		t.Fatal(err)
	}
	if jobType != "agentops.triage" || payload["schemaVersion"] != 1 {
		t.Fatalf("unexpected triage job: %s %#v", jobType, payload)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"title", "must not cross", "untrustedCommand", "curl attacker.invalid",
		"labels", "command", "cloneUrl", "hostPath",
	} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("triage payload forwarded %q: %s", forbidden, body)
		}
	}
}

func TestWorkItemRunnerPayloadFailsClosed(t *testing.T) {
	for _, item := range []WorkItem{
		{Repository: "Owner/repo", Kind: "issue", Number: 1},
		{Repository: "owner/repo", Kind: "issue", Number: 0},
		{Repository: "owner/repo", Kind: "pull_request", Number: 0},
		{Repository: "owner/repo", Kind: WorkItemKindRepositoryHead},
		{
			Repository: "owner/repo", Kind: WorkItemKindRepositoryHead,
			SourceEvent: "workflow_dispatch",
		},
		{Repository: "owner/repo", Kind: "workflow_dispatch"},
	} {
		if _, err := item.RunnerPayload("poll", "squash"); err == nil {
			t.Fatalf("RunnerPayload(%#v) unexpectedly succeeded", item)
		}
	}
	if _, err := (WorkItem{
		Repository: "owner/repo", Kind: WorkItemKindPullRequest, Number: 1,
	}).RunnerPayload("poll", "octopus"); err == nil {
		t.Fatal("RunnerPayload accepted an unsupported integration strategy")
	}
}

func TestWorkItemTriagePayloadFailsClosed(t *testing.T) {
	for _, item := range []WorkItem{
		{
			Repository: "Owner/repo", Kind: "issue", Number: 1,
			UpdatedAt: time.Now(),
		},
		{
			Repository: "owner/repo", Kind: "issue", Number: 0,
			UpdatedAt: time.Now(),
		},
		{Repository: "owner/repo", Kind: "issue", Number: 1},
		{
			Repository: "owner/repo", Kind: "pull_request", Number: 1,
			UpdatedAt: time.Now(),
		},
	} {
		if _, err := item.TriagePayload(); err == nil {
			t.Fatalf("TriagePayload(%#v) unexpectedly succeeded", item)
		}
	}
}
