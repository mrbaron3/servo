package main

import (
	"context"
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
		{Status: 0, Stdout: "status running\napiserver 1.1.0\nappRoot " + appRoot + "\n"},
		{Status: 0, Stdout: "status running\napiserver 1.1.0\nappRoot " + appRoot + "\n"},
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
