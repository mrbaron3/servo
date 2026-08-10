package main

import (
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

func TestParseProgressTarget(t *testing.T) {
	repository, issue, err := parseProgressTarget("mrbaron3/servo#8")
	if err != nil || repository != "mrbaron3/servo" || issue != 8 {
		t.Fatalf("parseProgressTarget() = %q, %d, %v", repository, issue, err)
	}
	for _, invalid := range []string{
		"mrbaron3/servo",
		"mrbaron3/servo#0",
		"mrbaron3/servo#not-a-number",
		"../servo#8",
		"mrbaron3/servo#8#9",
	} {
		if _, _, err := parseProgressTarget(invalid); err == nil {
			t.Fatalf("parseProgressTarget(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestRetainedWorktreePathAcceptsOnlyRunnerOwnedCheckouts(t *testing.T) {
	registration := "11111111-1111-4111-8111-111111111111"
	job := "22222222-2222-4222-8222-222222222222"
	for _, valid := range []string{
		"/workspace/registrations/" + registration + "/jobs/" + job + "/attempt-2/worktree",
		"/workspace/registrations/" + registration + "/jobs/" + job + "/attempt-2/harness/.harness/worktrees/ISSUE-0008-s0",
		"/workspace/registrations/" + registration + "/jobs/" + job + "/state/.harness/worktrees/ISSUE-0008-s0",
	} {
		if !retainedWorktreePath.MatchString(valid) {
			t.Fatalf("runner-owned worktree %q was rejected", valid)
		}
	}
	for _, invalid := range []string{
		"/workspace/registrations/" + registration + "/jobs/" + job + "/state/.harness/worktrees/../escape",
		"/workspace/registrations/" + registration + "/jobs/" + job + "/state/review-worktrees/security",
		"/workspace/registrations/" + registration + "/jobs/" + job + "/attempt-0/worktree",
		"/tmp/worktree",
	} {
		if retainedWorktreePath.MatchString(invalid) {
			t.Fatalf("unsafe worktree %q was accepted", invalid)
		}
	}
}

func TestWorktreeRejectsAmbiguousEpicAggregate(t *testing.T) {
	postgres := `[{"id":"agentops-postgres","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}}]`
	progress := `{"items":[{"subjectNumber":8,"parentIssueNumber":1,` +
		`"worktreePath":"/workspace/registrations/11111111-1111-4111-8111-111111111111/` +
		`jobs/22222222-2222-4222-8222-222222222222/attempt-2/worktree"}]}`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: "container 0.12.0"},
		{Status: 0},
		{Status: 0, Stdout: postgres},
		{Status: 0, Stdout: progress},
	}}
	subject := newManager(testManagerConfig(), lifecycle.NewAppleRuntimeForTest(fake))

	err := subject.Worktree(context.Background(), "mrbaron3/servo", 1, false, true)
	if err == nil || !strings.Contains(err.Error(), "choose an explicit child Issue (#8)") {
		t.Fatalf("Worktree() error = %v", err)
	}
	if len(fake.interactive) != 0 {
		t.Fatalf("Epic worktree unexpectedly opened: %#v", fake.interactive)
	}
}

func TestPrintProgressAggregatesEpicChildrenAndWorktree(t *testing.T) {
	parent := int64(1)
	child := int64(8)
	worktree := "/workspace/registrations/11111111-1111-4111-8111-111111111111/jobs/22222222-2222-4222-8222-222222222222/attempt-2/worktree"
	nextGate := "repository graders"
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	report := progressReport{Items: []control.DevelopmentProgressEvent{{
		SubjectNumber:     &child,
		ParentIssueNumber: &parent,
		Phase:             "generation",
		Step:              "generator session",
		State:             "running",
		OccurredAt:        now,
		JobID:             "job-8",
		AttemptNumber:     2,
		JobStatus:         "leased",
		WorkerID:          "runner-8",
		WorktreePath:      &worktree,
		NextGate:          &nextGate,
	}}}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = write
	printProgress(report, "mrbaron3/servo", 1)
	_ = write.Close()
	os.Stdout = previous
	output, err := io.ReadAll(read)
	_ = read.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"epic children with durable progress: 1",
		"#8  generation",
		"subject: #8 (child of #1)",
		"worktree: " + worktree,
		"next gate: repository graders",
	} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("progress output missing %q:\n%s", expected, output)
		}
	}
}

func TestWorktreeShellFindsPreservedSupersededCheckout(t *testing.T) {
	registration := "11111111-1111-4111-8111-111111111111"
	job := "22222222-2222-4222-8222-222222222222"
	stale := "/workspace/registrations/" + registration + "/jobs/" + job + "/attempt-1/worktree"
	live := "/workspace/registrations/" + registration + "/jobs/" + job + "/attempt-2/worktree"
	postgres := `[{"id":"agentops-postgres","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}}]`
	progress := `{"items":[` +
		`{"phase":"completed","step":"released","state":"succeeded"},` +
		`{"worktreePath":"` + stale + `"},` +
		`{"worktreePath":"` + live + `"}` +
		`]}`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: "container 0.12.0"},
		{Status: 0},
		{Status: 0, Stdout: postgres},
		{Status: 0, Stdout: progress},
		{Status: 1, Stderr: "stale"},
		{Status: 0, Stdout: "## runner/preserved\n"},
	}}
	subject := newManager(testManagerConfig(), lifecycle.NewAppleRuntimeForTest(fake))

	if err := subject.Worktree(
		context.Background(),
		"mrbaron3/servo",
		8,
		false,
		true,
	); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exec", "--interactive", "--tty", "--workdir", live,
		"agentops-runner", "/bin/sh",
	}
	if !reflect.DeepEqual(fake.interactive, [][]string{want}) {
		t.Fatalf("interactive argv = %#v, want %#v", fake.interactive, want)
	}
}

// A recorded path this command cannot prove is inside the runner volume is not
// authority to abandon the search: one legacy or foreign event must not hide
// every usable retained attempt.
func TestWorktreeSkipsPathsOutsideTheRunnerVolumeInsteadOfFailing(t *testing.T) {
	registration := "11111111-1111-4111-8111-111111111111"
	job := "22222222-2222-4222-8222-222222222222"
	live := "/workspace/registrations/" + registration + "/jobs/" + job + "/attempt-2/worktree"
	postgres := `[{"id":"agentops-postgres","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}}]`
	progress := `{"items":[` +
		`{"worktreePath":"/home/operator/local-checkout"},` +
		`{"worktreePath":"` + live + `"}` +
		`]}`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: "container 0.12.0"},
		{Status: 0},
		{Status: 0, Stdout: postgres},
		{Status: 0, Stdout: progress},
		{Status: 0, Stdout: "## runner/live\n"},
	}}
	subject := newManager(testManagerConfig(), lifecycle.NewAppleRuntimeForTest(fake))

	if err := subject.Worktree(
		context.Background(),
		"mrbaron3/servo",
		8,
		false,
		true,
	); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exec", "--interactive", "--tty", "--workdir", live,
		"agentops-runner", "/bin/sh",
	}
	if !reflect.DeepEqual(fake.interactive, [][]string{want}) {
		t.Fatalf("interactive argv = %#v, want %#v", fake.interactive, want)
	}
}

func TestWorktreeReportsSkippedPathsWhenNoneRemain(t *testing.T) {
	postgres := `[{"id":"agentops-postgres","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}}]`
	progress := `{"items":[{"worktreePath":"/home/operator/local-checkout"}]}`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: "container 0.12.0"},
		{Status: 0},
		{Status: 0, Stdout: postgres},
		{Status: 0, Stdout: progress},
	}}
	subject := newManager(testManagerConfig(), lifecycle.NewAppleRuntimeForTest(fake))

	err := subject.Worktree(context.Background(), "mrbaron3/servo", 8, false, true)
	if err == nil {
		t.Fatal("expected an error when every recorded path was skipped")
	}
	if !strings.Contains(err.Error(), "outside the runner volume") {
		t.Fatalf("error = %v, want it to report the skipped paths", err)
	}
	if len(fake.interactive) != 0 {
		t.Fatalf("interactive argv = %#v, want none", fake.interactive)
	}
}
