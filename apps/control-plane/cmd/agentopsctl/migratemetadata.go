package main

import (
	"context"
	"errors"
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
) (err error) {
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
	// The restart is one closure with two callers, and it runs at most once.
	//
	// Recovery is ONE closure that both starts the runtime and proves it came
	// back, and it runs at most once.
	//
	// Starting and proving belong together because every path that leaves this
	// function has the same obligation. A deferred start alone is not enough:
	// the success path has to prove the runtime is up, and a check written after
	// the function body runs before any defer fires, so it would read a stopped
	// runtime on every successful rollback. An explicit start alone is not
	// enough either: a stop that fails partway, a cancelled run, and a failed
	// restore never reach it. And a proof that lives outside the closure is
	// worst of both — the fallback paths start the runtime and never check it,
	// so `StartSystem` returning nil while the services stay down is reported as
	// nothing at all.
	//
	// `restarted` is what keeps the two callers from starting the runtime twice.
	restarted := false
	restart := func() error {
		if restarted {
			return nil
		}
		restarted = true
		// A cancelled context must not be able to leave Apple Container stopped.
		// main wires ctx to signal.NotifyContext, so a SIGINT arriving inside the
		// stopped window would otherwise disable exactly the one operation that
		// must always run. Cancellation aborts the rollback; it must not abort
		// the recovery — including its proof, which is why the capability probe
		// below runs on this context too rather than on the caller's.
		restartCtx, cancelRestart := context.WithTimeout(
			context.WithoutCancel(ctx), serviceRestartTimeout,
		)
		defer cancelRestart()
		if startErr := runtime.StartSystem(restartCtx); startErr != nil {
			return fmt.Errorf("start Apple Container: %w", startErr)
		}
		// The runtime's own answer, not this process's belief about it. A start
		// that exits zero while the apiserver stays down is the case this exists
		// for, and it is invisible to StartSystem's return value.
		if capability := runtime.Capability(restartCtx); !capability.ServiceRunning {
			return fmt.Errorf(
				"Apple Container reports its services are still not running; " +
					"start them with `container system start`",
			)
		}
		return nil
	}

	fmt.Printf("stopping Apple Container services\n")
	// Registered BEFORE the stop, not after it succeeds: returning from a failed
	// stop without a registered restart is how an operator's machine is left
	// without a container runtime by a command that only meant to refuse.
	defer func() {
		if restarted {
			return
		}
		if restartErr := restart(); restartErr != nil {
			fmt.Fprintf(
				os.Stderr,
				"WARNING: Apple Container did not start again: %v\n", restartErr,
			)
			// Joined into the returned error rather than only printed. A defer
			// cannot change the exit status by printing, and "the rollback
			// failed AND the runtime is down" is not the same incident as
			// either one alone.
			err = errors.Join(err, fmt.Errorf(
				"Apple Container did not start again: %w", restartErr,
			))
		}
	}()
	if stopErr := runtime.StopSystem(ctx); stopErr != nil {
		return fmt.Errorf("stop Apple Container: %w", stopErr)
	}
	if stoppedErr := runtime.RequireServicesStopped(ctx); stoppedErr != nil {
		return stoppedErr
	}
	printRestoreOutcomes := func() {
		for _, application := range plan.Applied {
			for _, file := range application.RestoreOutcomes() {
				fmt.Printf("  %-10s %-46s %s\n", application.Kind, application.ID, file)
			}
		}
	}
	if rollbackErr := lifecycle.RollbackMetadataSweep(plan); rollbackErr != nil {
		// The rollback unwinds within one resource but not across them: it stops
		// at the first failing application and leaves every higher-indexed one it
		// already restored reverted. These outcomes are the only record of which
		// half of the host is in which state. They cannot be recomputed after the
		// process exits — a reverted resource carries pre-Phase-3A labels, so this
		// binary cannot see it, and `agentopsctl status` cannot tell the two apart
		// by design. Printing them only on success threw that record away in the
		// one case an operator needs it.
		printRestoreOutcomes()
		fmt.Fprintf(
			os.Stderr,
			"NOTE: the host is partially reverted. The outcomes above name every "+
				"document that landed; the rest still carry their post-migration "+
				"labels. Re-running this same plan is the supported recovery and "+
				"is idempotent.\n",
		)
		return rollbackErr
	}
	// Restarted and proved BEFORE any success output, so a rollback that could
	// not bring the runtime back does not print a success summary. The outcome
	// table is part of that summary: it reads as "this all worked".
	if restartErr := restart(); restartErr != nil {
		return restartErr
	}
	printRestoreOutcomes()
	fmt.Printf(
		"restored %d resource(s) to their pre-migration labels\n",
		len(plan.Applied),
	)
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
