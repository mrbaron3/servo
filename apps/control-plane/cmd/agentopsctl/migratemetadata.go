package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// `migrate-label-metadata` was the Phase 3A operator surface. Apple Container
// 1.1.0 has no route that changes an existing resource's labels — the complete
// XPC surface is create, delete, inspect, list, and prune — so a volume's labels
// could only be corrected by rewriting the runtime's own metadata while it was
// stopped. Recreating the volume instead would destroy the PostgreSQL data it
// holds, which is why that option never appeared here at all.
//
// Phase 3B retires the forward stages. `--stage prepare` added the current label
// pair beside the legacy one and `--stage retire` removed the legacy pair; both
// decided what to write by comparing the two namespaces, and this binary now
// reads one. The flags are still declared so that an operator who reaches for
// them gets the reason rather than "flag provided but not defined", and the
// refusal happens before the runtime is touched — nothing on that path can stop
// Apple Container as a side effect of being told no.
//
// What remains is `--rollback`, which restores the documents a Phase 3A run
// recorded, to the exact bytes it recorded for them. It is the one-way boundary
// made operable: this binary can undo Phase 3A, it cannot redo it, and a host
// returned to its pre-Phase-3A labels has to be operated by a pre-Phase-3B
// binary afterwards.

// serviceRestartTimeout bounds the mandatory restart. It is generous: bringing
// the runtime back matters more than finishing quickly, and the alternative to
// waiting is leaving the operator's machine without a container runtime.
const serviceRestartTimeout = 3 * time.Minute

// errForwardMetadataSweepRetired explains why no forward stage remains. It names
// the rollback path, because an operator who typed `--stage` is mid-incident and
// the next thing they need to know is what this command can still do.
var errForwardMetadataSweepRetired = fmt.Errorf(
	"migrate-label-metadata --stage is retired as of Phase 3B of Issue #123.\n"+
		"Both stages existed to move ownership labels between two namespaces, "+
		"and this binary now reads %s.* alone — there is no legacy namespace "+
		"left to migrate from or to.\n"+
		"What remains is recovery:\n"+
		"  agentopsctl migrate-label-metadata --rollback "+
		"<backup-root>/<stage>-<stamp>/rollback-plan.json\n"+
		"That restores the documents a Phase 3A run rewrote to their exact "+
		"recorded bytes. Operating the host after a rollback requires "+
		"deliberately running a pre-Phase-3B binary, which reads both "+
		"namespaces; this one will not see the restored labels.",
	lifecycle.CurrentLabelNamespace,
)

func runMigrateLabelMetadata(ctx context.Context, args []string) error {
	return migrateLabelMetadata(ctx, args, lifecycle.NewAppleRuntime())
}

// migrateLabelMetadata takes the runtime as a parameter so a test can prove the
// central claim of this command: that every retired flag is refused before
// anything reaches Apple Container. A constructor called inside the function
// would make that claim untestable, and it is the claim most worth pinning —
// the failure it guards against is stopping an operator's container runtime on
// the way to printing "retired".
func migrateLabelMetadata(
	ctx context.Context,
	args []string,
	runtime *lifecycle.AppleRuntime,
) error {
	flags := flag.NewFlagSet("migrate-label-metadata", flag.ContinueOnError)
	// The retired flags stay declared, with help text that says so, so the
	// refusal below is what an operator reads rather than a parse error.
	stage := flags.String(
		"stage", "",
		"retired in Phase 3B; no forward label migration remains",
	)
	apply := flags.Bool(
		"apply", false,
		"retired in Phase 3B with --stage; --rollback is the remaining "+
			"mutating path",
	)
	only := flags.String(
		"only", "",
		"retired in Phase 3B with --stage",
	)
	evidenceDir := flags.String(
		"evidence-dir", "",
		"retired in Phase 3B with --stage; a rollback records its outcome in "+
			"the private backup root it restores from",
	)
	backupDir := flags.String(
		"backup-dir", "",
		"retired in Phase 3B with --stage; --rollback reads the absolute "+
			"locations out of the plan it is given",
	)
	rollback := flags.String(
		"rollback", "",
		"path to a rollback plan written by a Phase 3A --apply run; restores "+
			"every document it rewrote to its exact original bytes",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError()
	}
	// The refusal precedes every runtime call, including the host resolution the
	// rollback path performs. A retired flag must not be able to reach anything
	// that inspects, stops, or starts Apple Container.
	for _, retired := range []struct {
		name string
		set  bool
	}{
		{"--stage", strings.TrimSpace(*stage) != ""},
		{"--apply", *apply},
		{"--only", strings.TrimSpace(*only) != ""},
		{"--evidence-dir", strings.TrimSpace(*evidenceDir) != ""},
		{"--backup-dir", strings.TrimSpace(*backupDir) != ""},
	} {
		if retired.set {
			return fmt.Errorf("%s: %w", retired.name, errForwardMetadataSweepRetired)
		}
	}
	if *rollback != "" && strings.TrimSpace(*rollback) == "" {
		// A path of only whitespace is a mistake worth naming, not the same
		// thing as asking for the retired forward stage.
		return fmt.Errorf("--rollback needs the path to a rollback plan")
	}
	if *rollback == "" {
		// With no forward stage there is no useful default action, and defaulting
		// to a mutating one would be the opposite of this command's convention
		// that the safe spelling is the short one.
		return errForwardMetadataSweepRetired
	}

	// Rollback writes to Apple Container's own documents, so it clears the same
	// gates the forward path did: the exact version allowlist that makes this
	// layout knowable at all, and the not-running check, which cannot be made
	// once the services are stopped.
	host, err := runtime.ResolveMetadataHost(ctx)
	if err != nil {
		return err
	}
	return runMetadataRollback(ctx, runtime, host, strings.TrimSpace(*rollback))
}

// runMetadataRollback restores every document a recorded run rewrote.
func runMetadataRollback(
	ctx context.Context,
	runtime *lifecycle.AppleRuntime,
	host *lifecycle.MetadataHost,
	reportPath string,
) error {
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Errorf("read report: %w", err)
	}
	plan, err := lifecycle.ParseRollbackPlan(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", reportPath, err)
	}
	// Every path in the plan is an absolute location read verbatim out of a
	// file. Binding them to the resolved host before the runtime is stopped is
	// what keeps a stale or substituted plan from taking Apple Container down and
	// then rewriting a document it has no business touching.
	if err := plan.BindToHost(host); err != nil {
		return fmt.Errorf("%s: %w", reportPath, err)
	}
	// The not-running gate is taken after the plan is known, so it can name the
	// plan's own targets as well as the containers this binary can classify. A
	// resumed rollback's earlier resources are already back on the retired
	// namespace and unclassifiable, and they are exactly the ones about to be
	// rewritten again.
	inventory, err := lifecycle.TakeOwnershipInventory(ctx, runtime)
	if err != nil {
		return err
	}
	running := append(
		inventory.RunningIdentifiableContainers(),
		inventory.RunningNamed(plan.ContainerTargets())...,
	)
	if unique := distinctSorted(running); len(unique) > 0 {
		return fmt.Errorf(
			"%d container(s) are not stopped (%s); stop them before rolling "+
				"back runtime metadata",
			len(unique), strings.Join(unique, ", "),
		)
	}
	fmt.Printf("stopping Apple Container services\n")
	if err := runtime.StopSystem(ctx); err != nil {
		return fmt.Errorf("stop Apple Container: %w", err)
	}
	defer func() {
		// A cancelled context must not be able to leave Apple Container stopped.
		// main wires ctx to signal.NotifyContext, so a SIGINT arriving inside the
		// stopped window would otherwise disable exactly the one operation that
		// must always run. Cancellation aborts the rollback; it must not abort
		// the recovery.
		restartCtx, cancelRestart := context.WithTimeout(
			context.WithoutCancel(ctx), serviceRestartTimeout,
		)
		defer cancelRestart()
		if err := runtime.StartSystem(restartCtx); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"WARNING: Apple Container did not start again: %v\n", err,
			)
		}
	}()
	if err := runtime.RequireServicesStopped(ctx); err != nil {
		return err
	}
	if err := lifecycle.RollbackMetadataSweep(plan); err != nil {
		return err
	}
	for _, application := range plan.Applied {
		for _, file := range application.RestoreOutcomes() {
			fmt.Printf("  %-10s %-46s %s\n", application.Kind, application.ID, file)
		}
	}
	fmt.Printf(
		"restored %d resource(s) to their pre-migration labels\n",
		len(plan.Applied),
	)
	// The deferred restart above reports a failure on stderr but cannot change
	// the exit status from inside a defer. Re-proving the runtime is up here is
	// what keeps "the rollback succeeded" from being printed by a process that
	// exits 0 with the operator's container runtime down.
	if capability := runtime.Capability(ctx); !capability.ServiceRunning {
		return fmt.Errorf(
			"the rollback completed but Apple Container is not running again; " +
				"start it with `container system start`",
		)
	}
	// The restored labels may be in the namespace this binary no longer reads,
	// in which case it can no longer see the resources it just restored. Saying
	// so here is the difference between a deliberate one-way boundary and an
	// operator discovering it at the next `agentopsctl status`.
	fmt.Printf(
		"NOTE: this binary reads %s.* only. If the restored labels predate "+
			"Phase 3A, operate this host with a pre-Phase-3B binary.\n",
		lifecycle.CurrentLabelNamespace,
	)
	return nil
}

// distinctSorted removes the overlap between the two not-running checks, which
// deliberately cover intersecting sets.
func distinctSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, already := seen[value]; already {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}
