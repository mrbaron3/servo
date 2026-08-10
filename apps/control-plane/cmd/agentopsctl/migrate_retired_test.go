package main

import (
	"context"
	"strings"
	"testing"

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
	// The message has to name the replacement: an operator who reaches this is
	// mid-migration and needs the next command, not just a refusal.
	for _, expected := range []string{
		"retired", "migrate-label-metadata", "prepare", "retire",
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
