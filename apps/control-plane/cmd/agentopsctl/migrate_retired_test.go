package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// refusalRuntime fails loudly if the retired path asks the runtime anything.
// The subcommand's --apply branch may start Apple Container's services, so the
// refusal has to land before the first call rather than after an inventory.
type refusalRuntime struct {
	t      *testing.T
	called bool
}

func (runtime *refusalRuntime) Run(
	_ context.Context, args []string,
) lifecycle.CommandResult {
	runtime.called = true
	runtime.t.Errorf("the retired sweep invoked the runtime: %v", args)
	return lifecycle.CommandResult{Status: 1}
}

func TestMigrateLabelsApplyIsRetiredBeforeTouchingTheRuntime(t *testing.T) {
	runner := &refusalRuntime{t: t}
	manager := newManager(
		config{ProjectRoot: t.TempDir()},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	err := manager.MigrateLabels(
		context.Background(), true, "", []string{"agentops-runner"},
	)
	if err == nil {
		t.Fatal("migrate-labels --apply still applies")
	}
	if runner.called {
		t.Fatal("the refusal came after the runtime was queried")
	}
	// The message has to say what replaced it and what replaced that. Through
	// Phase 3A it named the staged `migrate-label-metadata` commands; Phase 3B
	// retires those too, so what an operator needs now is to know that no
	// forward container label migration remains anywhere in this binary.
	for _, expected := range []string{
		"retired", "migrate-label-metadata", "Phase 3B",
		"com.mrbaron3.servo", "read-only inventory",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("refusal does not mention %q: %v", expected, err)
		}
	}
}

func TestMigrateLabelsOnlyIsRejectedWithoutApply(t *testing.T) {
	runner := &refusalRuntime{t: t}
	manager := newManager(
		config{ProjectRoot: t.TempDir()},
		lifecycle.NewAppleRuntimeForTest(runner),
	)
	if err := manager.MigrateLabels(
		context.Background(), false, "", []string{"agentops-runner"},
	); err == nil {
		t.Fatal("--only was accepted by the inventory")
	}
	if runner.called {
		t.Fatal("the refusal came after the runtime was queried")
	}
}

// TestRestartContextSurvivesCancellation pins the P1 the automated review
// found. main wires the command context to signal.NotifyContext, so a SIGINT
// arriving inside the stopped window cancels it — and exec.CommandContext
// refuses to launch with a cancelled context. Without a detached context the
// mandatory restart becomes the one operation cancellation disables, and the
// operator is left with no container runtime.
func TestRestartContextSurvivesCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	restart, cancelRestart := context.WithTimeout(
		context.WithoutCancel(parent), serviceRestartTimeout,
	)
	defer cancelRestart()
	cancel()
	if parent.Err() == nil {
		t.Fatal("the parent context should be cancelled")
	}
	if err := restart.Err(); err != nil {
		t.Fatalf("the restart context was cancelled with its parent: %v", err)
	}
	// It must still be bounded, or a hung restart would wedge the command.
	deadline, ok := restart.Deadline()
	if !ok {
		t.Fatal("the restart context has no deadline")
	}
	if time.Until(deadline) > serviceRestartTimeout+time.Second {
		t.Fatalf("restart deadline is further out than intended: %v", deadline)
	}
}
