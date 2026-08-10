package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// Phase 3B's claim about `migrate-label-metadata` is not that the forward stages
// return an error — it is that they return it *before anything reaches Apple
// Container*. That ordering is the whole property: the failure it guards against
// is taking an operator's container runtime down on the way to printing
// "retired".
//
// So every case here asserts two things together, and the second matters more
// than the first: the command failed, and the runtime was never called.

// refusingRuntimeRunner fails the test if it is ever asked to run anything. A
// counter would let a refusal that queried the host still pass with a later
// assertion; failing at the call site names the command that got through.
type refusingRuntimeRunner struct {
	t      *testing.T
	called bool
}

func (runner *refusingRuntimeRunner) Run(
	_ context.Context,
	args []string,
) lifecycle.CommandResult {
	runner.t.Helper()
	runner.called = true
	runner.t.Errorf(
		"a retired flag reached Apple Container: container %s",
		strings.Join(args, " "),
	)
	return lifecycle.CommandResult{Status: 1}
}

func TestMigrateLabelMetadataRefusesEveryRetiredFlagBeforeTouchingTheRuntime(t *testing.T) {
	for name, args := range map[string][]string{
		"--stage prepare":   {"--stage", "prepare"},
		"--stage retire":    {"--stage", "retire"},
		"--stage unknown":   {"--stage", "nonsense"},
		"--apply":           {"--apply"},
		"--only":            {"--only", "volume/agentops-postgres-data"},
		"--evidence-dir":    {"--evidence-dir", "evidence/label-p3a"},
		"--backup-dir":      {"--backup-dir", "/tmp/backups"},
		"stage with apply":  {"--stage", "retire", "--apply"},
		"no arguments":      {},
		"apply with a plan": {"--apply", "--rollback", "/tmp/plan.json"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &refusingRuntimeRunner{t: t}
			err := migrateLabelMetadata(
				context.Background(), args,
				lifecycle.NewAppleRuntimeForTest(runner),
			)
			if err == nil {
				t.Fatal("a retired invocation was accepted")
			}
			if !errors.Is(err, errForwardMetadataSweepRetired) {
				t.Fatalf("not reported as retired: %v", err)
			}
			if runner.called {
				t.Fatal("the refusal came after the runtime was queried")
			}
			// An operator who reaches this is mid-incident; the message has to
			// name what the command can still do.
			for _, expected := range []string{
				"Phase 3B", "--rollback", lifecycle.CurrentLabelNamespace,
				"pre-Phase-3B binary",
			} {
				if !strings.Contains(err.Error(), expected) {
					t.Errorf("refusal does not mention %q: %v", expected, err)
				}
			}
		})
	}
}

// TestMigrateLabelMetadataRollbackNeedsAResolvedHostBeforeStopping pins the
// other half of the ordering. A rollback must resolve and validate before it
// stops anything; with no usable runtime it has to fail on the host resolution,
// not on a half-stopped machine.
func TestMigrateLabelMetadataRollbackFailsClosedWithoutAUsableHost(t *testing.T) {
	runner := &recordingRuntimeRunner{
		results: []lifecycle.CommandResult{{Status: 1, Stderr: "not running"}},
	}
	err := migrateLabelMetadata(
		context.Background(),
		[]string{"--rollback", "/nonexistent/rollback-plan.json"},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err == nil {
		t.Fatal("a rollback ran without a resolved host")
	}
	for _, args := range runner.args {
		if len(args) >= 2 && args[0] == "system" && args[1] == "stop" {
			t.Fatalf("the runtime was stopped before the host was proven: %#v", runner.args)
		}
	}
}

// recordingRuntimeRunner returns canned results and remembers what it was asked
// to run, so a test can assert on ordering rather than only on the error.
type recordingRuntimeRunner struct {
	results []lifecycle.CommandResult
	args    [][]string
	index   int
}

func (runner *recordingRuntimeRunner) Run(
	_ context.Context,
	args []string,
) lifecycle.CommandResult {
	runner.args = append(runner.args, args)
	if runner.index < len(runner.results) {
		result := runner.results[runner.index]
		runner.index++
		return result
	}
	return lifecycle.CommandResult{Status: 1, Stderr: "no canned result"}
}

// TestRollbackNeverStopsTheRuntimeWhenThePlanDoesNotBind is the command-level
// half of the binding guarantee. The lifecycle tests prove BindToHost's
// verdicts; only this one proves the verdict arrives before Apple Container is
// taken down, which is the property an operator actually depends on.
func TestRollbackNeverStopsTheRuntimeWhenThePlanDoesNotBind(t *testing.T) {
	directory := t.TempDir()
	planPath := filepath.Join(directory, "rollback-plan.json")
	// A plan that parses and is internally consistent, but names a document
	// under an application root this host does not have.
	plan := `{"stage":"retire","applied":[{"kind":"volume","id":"vol-a",` +
		`"files":[{"document":"volumes/vol-a/entity.json","labelPath":["labels"]}]}],` +
		`"locations":[[{"path":"/nowhere/volumes/vol-a/entity.json",` +
		`"backupPath":"/nowhere/backups/volume/vol-a/entity.json"}]],` +
		`"labels":[{"before":{"a":"b"},"after":{"c":"d"}}]}`
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	appRoot := t.TempDir()
	runner := &recordingRuntimeRunner{results: []lifecycle.CommandResult{
		// ResolveMetadataHost: version probe and system status.
		{Status: 0, Stdout: "container CLI version 1.1.0"},
		{Status: 0, Stdout: "status running\n" +
			"apiserver.version  container-apiserver version 1.1.0\n" +
			"appRoot " + appRoot + "\n"},
	}}
	err := migrateLabelMetadata(
		context.Background(),
		[]string{"--rollback", planPath},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err == nil {
		t.Fatal("a plan that does not describe this host was accepted")
	}
	for _, args := range runner.args {
		if len(args) >= 2 && args[0] == "system" &&
			(args[1] == "stop" || args[1] == "start") {
			t.Fatalf(
				"the runtime was stopped or started despite a binding failure: %#v",
				runner.args,
			)
		}
	}
}

func TestRollbackRejectsABlankPlanPath(t *testing.T) {
	runner := &refusingRuntimeRunner{t: t}
	err := migrateLabelMetadata(
		context.Background(),
		[]string{"--rollback", "   "},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err == nil {
		t.Fatal("a blank rollback path was accepted")
	}
	// A whitespace-only path is a mistake worth naming, not the same thing as
	// asking for the retired forward stage.
	if errors.Is(err, errForwardMetadataSweepRetired) {
		t.Fatalf("a blank path was reported as the retired stage: %v", err)
	}
	if runner.called {
		t.Fatal("a blank rollback path reached the runtime")
	}
}

// rollbackFixture writes an application root, a private backup root, and a plan
// that binds cleanly against them. Every restart case below drives the same real
// rollback and differs only in what the runtime is made to do.
func rollbackFixture(t *testing.T) (planPath, appRoot string) {
	t.Helper()
	root := t.TempDir()
	appRoot = filepath.Join(root, "appRoot")
	backups := filepath.Join(root, "backups")
	document := filepath.Join(appRoot, "volumes", "vol-a", "entity.json")
	backup := filepath.Join(backups, "volume", "vol-a", "entity.json")
	for _, directory := range []string{filepath.Dir(document), filepath.Dir(backup)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	before := `{"name":"vol-a","labels":{"com.example.old":"v1"}}`
	after := `{"name":"vol-a","labels":{"com.mrbaron3.servo.agentopsctl":"v1"}}`
	if err := os.WriteFile(backup, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(document, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := func(body string) string {
		digest := sha256.Sum256([]byte(body))
		return hex.EncodeToString(digest[:])
	}
	plan := map[string]any{
		"stage": "retire",
		"applied": []map[string]any{{
			"kind": "volume", "id": "vol-a",
			"files": []map[string]any{{
				"document":     "volumes/vol-a/entity.json",
				"labelPath":    []string{"labels"},
				"beforeSha256": sum(before),
				"afterSha256":  sum(after),
				"backupSha256": sum(before),
			}},
		}},
		"locations": [][]map[string]string{{{"path": document, "backupPath": backup}}},
		"labels": []map[string]map[string]string{{
			"before": {"com.example.old": "v1"},
			"after":  {"com.mrbaron3.servo.agentopsctl": "v1"},
		}},
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath = filepath.Join(root, "rollback-plan.json")
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return planPath, appRoot
}

// stoppedStatus is what `container system status` reports once the services are
// down. The stopped-state proof requires this positive marker, not merely a
// nonzero exit: "the command failed" is not evidence that a service stopped.
const stoppedStatus = "apiserver is not running"

func runningStatus(appRoot string) string {
	return "status running\n" +
		"apiserver.version  container-apiserver version 1.1.0 (build: release)\n" +
		"appRoot " + appRoot + "\n"
}

// preStopResults are the calls every rollback makes before it stops anything:
// host resolution, then the inventory the not-running gate reads.
func preStopResults(appRoot string) []lifecycle.CommandResult {
	return []lifecycle.CommandResult{
		{Status: 0, Stdout: "container CLI version 1.1.0"},
		{Status: 0, Stdout: runningStatus(appRoot)},
		{Status: 0, Stdout: `[]`},
		{Status: 0, Stdout: `[]`},
		{Status: 0, Stdout: `[]`},
	}
}

// stoppedProofResults are what RequireServicesStopped needs: a usable CLI, a
// failing status that says the apiserver is not running, and a data-plane call
// that also fails.
func stoppedProofResults() []lifecycle.CommandResult {
	return []lifecycle.CommandResult{
		{Status: 0, Stdout: "container CLI version 1.1.0"},
		{Status: 1, Stderr: stoppedStatus},
		{Status: 1, Stderr: stoppedStatus},
	}
}

// recoveryResults are the calls one recovery makes: StartSystem, then the
// capability probe that proves the runtime actually came back.
func recoveryResults(appRoot string, up bool) []lifecycle.CommandResult {
	status := lifecycle.CommandResult{Status: 0, Stdout: runningStatus(appRoot)}
	if !up {
		status = lifecycle.CommandResult{Status: 1, Stderr: stoppedStatus}
	}
	return []lifecycle.CommandResult{
		{Status: 0},
		{Status: 0, Stdout: "container CLI version 1.1.0"},
		status,
	}
}

// proofsAfterFirstStart counts the capability probes that follow the first
// `system start`, which is how "no double proof" is asserted.
func proofsAfterFirstStart(args [][]string) int {
	started, proofs := false, 0
	for _, argv := range args {
		command := strings.Join(argv, " ")
		if command == "system start" {
			started = true
			continue
		}
		if started && command == "system status" {
			proofs++
		}
	}
	return proofs
}

func startCommands(args [][]string) int {
	count := 0
	for _, argv := range args {
		if len(argv) >= 2 && argv[0] == "system" && argv[1] == "start" {
			count++
		}
	}
	return count
}

func commandOrder(args [][]string) []string {
	order := make([]string, 0, len(args))
	for _, argv := range args {
		order = append(order, strings.Join(argv, " "))
	}
	return order
}

// TestRollbackSucceedsAndRestartsExactlyOnce is the case a deferred-only restart
// breaks: a check written after the function body runs BEFORE any defer fires,
// so verifying the runtime there reads a stopped runtime and reports a failure
// that did not happen. A successful rollback returns nil, starts the runtime
// once, and verifies it afterwards.
func TestRollbackSucceedsAndRestartsExactlyOnce(t *testing.T) {
	planPath, appRoot := rollbackFixture(t)
	results := preStopResults(appRoot)
	results = append(results, lifecycle.CommandResult{Status: 0})
	results = append(results, stoppedProofResults()...)
	results = append(results, recoveryResults(appRoot, true)...)
	runner := &recordingRuntimeRunner{results: results}
	if err := migrateLabelMetadata(
		context.Background(),
		[]string{"--rollback", planPath},
		lifecycle.NewAppleRuntimeForTest(runner),
	); err != nil {
		t.Fatalf("a successful rollback reported an error: %v\n%v",
			err, commandOrder(runner.args))
	}
	if starts := startCommands(runner.args); starts != 1 {
		t.Fatalf("system start ran %d times, want exactly 1: %v",
			starts, commandOrder(runner.args))
	}
	order := commandOrder(runner.args)
	stopIndex, startIndex, verifyIndex := -1, -1, -1
	for index, command := range order {
		switch {
		case command == "system stop":
			stopIndex = index
		case command == "system start":
			startIndex = index
		case command == "system status" && startIndex >= 0 && verifyIndex < 0:
			verifyIndex = index
		}
	}
	if stopIndex < 0 || startIndex < stopIndex || verifyIndex < startIndex {
		t.Fatalf("stop, start and verify are out of order: %v", order)
	}
	if proofs := proofsAfterFirstStart(runner.args); proofs != 1 {
		t.Fatalf("the runtime was proved %d times, want exactly 1: %v", proofs, order)
	}
}

// TestRollbackReportsAFailedRestartOnTheSuccessPath: the restore succeeded but
// the runtime did not come back, so the command must not exit 0 — and the
// deferred fallback must not retry a start the explicit call already made.
func TestRollbackReportsAFailedRestartOnTheSuccessPath(t *testing.T) {
	planPath, appRoot := rollbackFixture(t)
	results := preStopResults(appRoot)
	results = append(results, lifecycle.CommandResult{Status: 0})
	results = append(results, stoppedProofResults()...)
	results = append(results, lifecycle.CommandResult{Status: 1, Stderr: "start failed"})
	runner := &recordingRuntimeRunner{results: results}
	err := migrateLabelMetadata(
		context.Background(),
		[]string{"--rollback", planPath},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err == nil {
		t.Fatalf("a failed restart exited 0: %v", commandOrder(runner.args))
	}
	if starts := startCommands(runner.args); starts != 1 {
		t.Fatalf("system start ran %d times, want exactly 1: %v",
			starts, commandOrder(runner.args))
	}
}

// TestRollbackRestartsTheRuntimeEvenWhenTheStopFails pins the other half. A stop
// that fails partway has still taken services down, so the restart has to be
// registered before the stop rather than after it succeeds — and still run once.
func TestRollbackRestartsTheRuntimeEvenWhenTheStopFails(t *testing.T) {
	planPath, appRoot := rollbackFixture(t)
	results := preStopResults(appRoot)
	results = append(results, lifecycle.CommandResult{Status: 1, Stderr: "stop failed halfway"})
	results = append(results, recoveryResults(appRoot, true)...)
	runner := &recordingRuntimeRunner{results: results}
	err := migrateLabelMetadata(
		context.Background(),
		[]string{"--rollback", planPath},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err == nil {
		t.Fatal("a failed stop was reported as success")
	}
	if starts := startCommands(runner.args); starts != 1 {
		t.Fatalf("a failed stop produced %d starts, want exactly 1: %v",
			starts, commandOrder(runner.args))
	}
}

// TestRollbackRestartsUnderACancelledContext proves cancellation aborts the
// rollback without aborting the recovery. main wires ctx to signal.NotifyContext,
// so a SIGINT inside the stopped window would otherwise disable exactly the one
// operation that must always run.
func TestRollbackRestartsUnderACancelledContext(t *testing.T) {
	planPath, appRoot := rollbackFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	results := preStopResults(appRoot)
	results = append(results, lifecycle.CommandResult{Status: 0}) // system stop
	// The caller's context is cancelled on the stop. The run then fails for an
	// ordinary reason — the stopped-state proof sees a live apiserver — which is
	// what sends it down the deferred recovery path with a cancelled context in
	// hand. A fake runner ignores context, so cancellation has to be observed
	// where it matters: on the calls the recovery itself makes.
	results = append(results,
		lifecycle.CommandResult{Status: 0, Stdout: "container CLI version 1.1.0"},
		lifecycle.CommandResult{Status: 0, Stdout: runningStatus(appRoot)},
	)
	results = append(results, recoveryResults(appRoot, true)...)
	runner := &cancellingRuntimeRunner{
		recordingRuntimeRunner: recordingRuntimeRunner{results: results},
		cancelOn:               "system stop",
		cancel:                 cancel,
	}
	err := migrateLabelMetadata(
		ctx,
		[]string{"--rollback", planPath},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err == nil {
		t.Fatal("a cancelled rollback was reported as success")
	}
	if starts := startCommands(runner.args); starts != 1 {
		t.Fatalf("a cancelled rollback produced %d starts, want exactly 1: %v",
			starts, commandOrder(runner.args))
	}
	if !runner.sawStartAfterCancel {
		t.Fatalf("the restart did not survive cancellation: %v",
			commandOrder(runner.args))
	}
	// The proof runs on the same uncancelled context as the start, so a
	// cancelled caller cannot leave the recovery unverified.
	if proofs := proofsAfterFirstStart(runner.args); proofs != 1 {
		t.Fatalf("the runtime was proved %d times, want exactly 1: %v",
			proofs, commandOrder(runner.args))
	}
	if !runner.sawProofAfterCancel {
		t.Fatalf("the recovery proof did not survive cancellation: %v",
			commandOrder(runner.args))
	}
}

// cancellingRuntimeRunner cancels the caller's context partway through, so the
// restart has to survive it.
type cancellingRuntimeRunner struct {
	recordingRuntimeRunner
	cancelOn            string
	cancel              context.CancelFunc
	cancelled           bool
	started             bool
	sawStartAfterCancel bool
	sawProofAfterCancel bool
}

func (runner *cancellingRuntimeRunner) Run(
	ctx context.Context,
	args []string,
) lifecycle.CommandResult {
	command := strings.Join(args, " ")
	if runner.cancelled && command == "system start" && ctx.Err() == nil {
		// The restart context must be alive even though the caller's is not.
		runner.sawStartAfterCancel = true
		runner.started = true
	}
	if runner.started && command == "system status" && ctx.Err() == nil {
		runner.sawProofAfterCancel = true
	}
	result := runner.recordingRuntimeRunner.Run(ctx, args)
	if command == runner.cancelOn && !runner.cancelled {
		runner.cancelled = true
		runner.cancel()
	}
	return result
}

// TestRollbackJoinsAPrimaryFailureWithAFailedRecovery is the case the proof
// being outside the closure hid entirely. A fallback path started the runtime
// and never checked it, so StartSystem returning nil while the services stayed
// down produced only the primary error — the operator was told the rollback
// failed and nothing at all about the runtime being down.
func TestRollbackJoinsAPrimaryFailureWithAFailedRecovery(t *testing.T) {
	planPath, appRoot := rollbackFixture(t)
	results := preStopResults(appRoot)
	results = append(results, lifecycle.CommandResult{Status: 0}) // system stop
	// The stopped-state proof fails: `system status` still reports running, so
	// the rollback refuses to edit metadata underneath a live apiserver. That is
	// the PRIMARY failure, and it returns before the explicit recovery.
	results = append(results,
		lifecycle.CommandResult{Status: 0, Stdout: "container CLI version 1.1.0"},
		lifecycle.CommandResult{Status: 0, Stdout: runningStatus(appRoot)},
	)
	// The deferred recovery then starts the runtime successfully but cannot
	// prove it came back.
	results = append(results, recoveryResults(appRoot, false)...)
	runner := &recordingRuntimeRunner{results: results}
	err := migrateLabelMetadata(
		context.Background(),
		[]string{"--rollback", planPath},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err == nil {
		t.Fatalf("a failed rollback exited 0: %v", commandOrder(runner.args))
	}
	message := err.Error()
	// Both incidents, because they are not the same incident.
	if !strings.Contains(message, "underneath a live apiserver") {
		t.Fatalf("the primary failure was lost: %v", err)
	}
	if !strings.Contains(message, "did not start again") ||
		!strings.Contains(message, "still not running") {
		t.Fatalf("the recovery failure was lost: %v", err)
	}
	if starts := startCommands(runner.args); starts != 1 {
		t.Fatalf("system start ran %d times, want exactly 1: %v",
			starts, commandOrder(runner.args))
	}
	if proofs := proofsAfterFirstStart(runner.args); proofs != 1 {
		t.Fatalf("the runtime was proved %d times, want exactly 1: %v",
			proofs, commandOrder(runner.args))
	}
}
