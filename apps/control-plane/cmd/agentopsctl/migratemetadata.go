package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// `migrate-label-metadata` is the Phase 3A operator surface. Apple Container
// 1.1.0 has no route that changes an existing resource's labels — the complete
// XPC surface is create, delete, inspect, list, and prune — so a volume's labels
// can only be corrected by rewriting the runtime's own metadata while it is
// stopped. Recreating the volume instead would destroy the PostgreSQL data it
// holds, which is why that option does not appear here at all.
//
// The default spelling is a read-only plan for the same reason `migrate-labels`
// defaults to an inventory: the mutating form takes the runtime down and edits
// files that belong to another program, and the safe word has to be the short
// one.

// metadataEvidence is the durable record of one plan, application, or rollback.
// Every path in it is relative to the application root, so a file that is
// committed to the repository never records where the operator's home
// directory is.
type metadataEvidence struct {
	GeneratedAt string                         `json:"generatedAt"`
	Mode        string                         `json:"mode"`
	Stage       lifecycle.MetadataStage        `json:"stage"`
	Host        lifecycle.MetadataHost         `json:"host"`
	Inventory   *lifecycle.OwnershipInventory  `json:"inventory,omitempty"`
	Plan        *lifecycle.MetadataSweepPlan   `json:"plan,omitempty"`
	Report      *lifecycle.MetadataSweepReport `json:"report,omitempty"`
	VerifiedBy  *lifecycle.OwnershipInventory  `json:"verifiedInventory,omitempty"`
}

func runMigrateLabelMetadata(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("migrate-label-metadata", flag.ContinueOnError)
	stage := flags.String(
		"stage",
		string(lifecycle.MetadataStagePrepare),
		"prepare (add the current pair beside the legacy one) or retire "+
			"(remove the legacy pair once the current one is proven)",
	)
	apply := flags.Bool(
		"apply",
		false,
		"stop Apple Container, rewrite the named resources' metadata, and "+
			"start it again; without this the command only plans",
	)
	only := flags.String(
		"only",
		"",
		"comma separated exact targets as kind/identity, for example "+
			"volume/agentops-postgres-data; required with --apply",
	)
	evidenceDir := flags.String(
		"evidence-dir", "", "where to write the durable plan and report",
	)
	backupDir := flags.String(
		"backup-dir", "",
		"private directory for the pre-migration copy of every rewritten "+
			"document; defaults to $XDG_STATE_HOME/agentops/label-metadata-"+
			"backups and must sit outside every git work tree",
	)
	rollback := flags.String(
		"rollback", "",
		"path to a report written by an earlier --apply; restores every "+
			"document it rewrote to its exact original bytes",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError()
	}
	migrationStage := lifecycle.MetadataStage(*stage)
	switch migrationStage {
	case lifecycle.MetadataStagePrepare, lifecycle.MetadataStageRetire:
	default:
		return fmt.Errorf(
			"--stage must be %q or %q",
			lifecycle.MetadataStagePrepare, lifecycle.MetadataStageRetire,
		)
	}
	runtime := lifecycle.NewAppleRuntime()
	if strings.TrimSpace(*rollback) != "" {
		// Rollback writes to the same documents as the forward path, so it has
		// to clear the same gates: the exact version allowlist that makes this
		// layout knowable at all, and the running-container check, which cannot
		// be made once the services are stopped.
		if _, err := runtime.ResolveMetadataHost(ctx); err != nil {
			return err
		}
		inventory, err := lifecycle.TakeOwnershipInventory(ctx, runtime)
		if err != nil {
			return err
		}
		if running := inventory.RunningManagedContainers(); len(running) > 0 {
			return fmt.Errorf(
				"%d managed container(s) are not stopped (%s); stop them before "+
					"rolling back runtime metadata",
				len(running), strings.Join(running, ", "),
			)
		}
		return runMetadataRollback(ctx, runtime, *rollback)
	}

	// The host has to be identified while the services are up: a stopped
	// apiserver reports neither its application root nor its version, and both
	// are gates this command refuses to run without.
	host, err := runtime.ResolveMetadataHost(ctx)
	if err != nil {
		return err
	}
	inventory, err := lifecycle.TakeOwnershipInventory(ctx, runtime)
	if err != nil {
		return err
	}
	if err := inventory.RequireConflictFree(); err != nil {
		return err
	}
	targets, err := parseMetadataTargets(*only)
	if err != nil {
		return err
	}
	if !*apply && len(targets) == 0 {
		// Planning the whole managed host is the rehearsal an operator reads
		// before choosing --only, so the default plan covers everything owned.
		targets = inventory.ManagedTargets()
	}
	if *apply && len(targets) == 0 {
		return fmt.Errorf(
			"--apply requires --only with the exact targets to migrate; run " +
				"without --apply to see the plan the host currently supports",
		)
	}
	// Retiring the legacy pair anywhere is only safe once it is redundant
	// everywhere, so the gate is evaluated against the whole host rather than
	// against the named targets.
	if migrationStage == lifecycle.MetadataStageRetire {
		if err := inventory.RequireCurrentOwnershipEverywhere(); err != nil {
			return err
		}
	}
	states := make(map[string]string, len(inventory.Records))
	for _, record := range inventory.Records {
		states[lifecycle.MetadataTargetRef{
			Kind: record.Kind, ID: record.ID,
		}.String()] = record.State
	}
	plan, err := lifecycle.PlanMetadataSweep(host, migrationStage, targets, states)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	printMetadataPlan(plan, inventory)

	if !*apply {
		if strings.TrimSpace(*evidenceDir) != "" {
			path, err := writeMetadataEvidence(
				*evidenceDir, fmt.Sprintf("plan-%s-%s.json", *stage, stamp),
				metadataEvidence{
					GeneratedAt: stamp, Mode: "plan", Stage: migrationStage,
					Host: *host, Inventory: inventory, Plan: plan,
				},
			)
			if err != nil {
				return err
			}
			fmt.Printf("\nevidence: %s\n", path)
		}
		return nil
	}

	// A backup is a verbatim copy of a container's configuration, which carries
	// initProcess.environment with values. The root is therefore resolved to a
	// private 0700 directory outside every git work tree, so a copy of a live
	// credential can never be staged from a checkout the sweep was run in.
	resolvedBackupRoot, err := lifecycle.ResolveBackupRoot(*backupDir)
	if err != nil {
		return err
	}
	// A stopped managed container can be rewritten safely because nothing is
	// reading its configuration. A running one cannot: the runtime holds its
	// own view of the labels, and the file would be overwritten underneath it.
	if running := inventory.RunningManagedContainers(); len(running) > 0 {
		return fmt.Errorf(
			"%d managed container(s) are not stopped (%s); stop them before "+
				"editing runtime metadata",
			len(running), strings.Join(running, ", "),
		)
	}
	if plan.ChangeCount == 0 {
		fmt.Println("\nevery named target already satisfies this stage")
		return nil
	}
	backupRoot := filepath.Join(
		resolvedBackupRoot, fmt.Sprintf("%s-%s", *stage, stamp),
	)

	fmt.Printf("\nstopping Apple Container services\n")
	if err := runtime.StopSystem(ctx); err != nil {
		return fmt.Errorf("stop Apple Container: %w", err)
	}
	// Whatever happens next, the runtime is brought back up. A host left with
	// its services down is a worse outcome than a failed migration. The flag
	// keeps the success path from starting it a second time: the redundant call
	// can fail merely because the services are already up, and printing
	// "Apple Container did not start again" after a run that succeeded and
	// verified would make the most alarming line of the output the least true.
	started := false
	defer func() {
		if started {
			return
		}
		if err := runtime.StartSystem(ctx); err != nil {
			fmt.Fprintf(
				os.Stderr,
				"WARNING: Apple Container did not start again: %v\n", err,
			)
		}
	}()
	report, applyErr := lifecycle.ApplyMetadataSweep(
		ctx, runtime, host, migrationStage, targets, backupRoot,
	)
	if applyErr != nil {
		// The report is written even when the sweep halted: which stage stopped
		// it is precisely what an operator needs in that case.
		if report != nil {
			if path, err := writeMetadataEvidence(
				evidenceOrDefault(*evidenceDir),
				fmt.Sprintf("halted-%s-%s.json", *stage, stamp),
				metadataEvidence{
					GeneratedAt: stamp, Mode: "halted", Stage: migrationStage,
					Host: *host, Plan: plan, Report: report,
				},
			); err == nil {
				fmt.Printf("halted report: %s\n", path)
			}
			if len(report.Applied) > 0 {
				fmt.Printf(
					"%d resource(s) were already rewritten. Undo them with:\n"+
						"  agentopsctl migrate-label-metadata --rollback %s\n",
					len(report.Applied), lifecycle.RollbackPlanPath(backupRoot),
				)
				if report.PlanStaleAfter != "" {
					fmt.Fprintf(
						os.Stderr,
						"WARNING: the rollback plan could NOT be updated for %s. "+
							"That resource is migrated and the plan does not "+
							"describe it; its backup is under %s and must be "+
							"restored by hand.\n",
						report.PlanStaleAfter, backupRoot,
					)
				}
			}
		}
		return applyErr
	}

	fmt.Printf("starting Apple Container services\n")
	if err := runtime.StartSystem(ctx); err != nil {
		return fmt.Errorf("start Apple Container: %w", err)
	}
	started = true
	if err := lifecycle.VerifyMetadataSweep(ctx, runtime, report); err != nil {
		return fmt.Errorf(
			"the sweep wrote its documents but the runtime does not report "+
				"the expected labels: %w", err,
		)
	}
	verified, err := lifecycle.TakeOwnershipInventory(ctx, runtime)
	if err != nil {
		return err
	}
	path, err := writeMetadataEvidence(
		evidenceOrDefault(*evidenceDir),
		fmt.Sprintf("applied-%s-%s.json", *stage, stamp),
		metadataEvidence{
			GeneratedAt: stamp, Mode: "applied", Stage: migrationStage,
			Host: *host, Plan: plan, Report: report, VerifiedBy: verified,
		},
	)
	if err != nil {
		return err
	}
	// The rollback plan was written inside the sweep as each resource landed, so
	// it already exists here and also exists on every failure path above.
	rollbackPath := lifecycle.RollbackPlanPath(backupRoot)
	printMetadataReport(report)
	fmt.Printf("\nevidence: %s\n", path)
	fmt.Printf("backups:  %s\n", backupRoot)
	fmt.Printf("rollback: agentopsctl migrate-label-metadata --rollback %s\n", rollbackPath)
	return nil
}

// runMetadataRollback restores every document a recorded run rewrote.
func runMetadataRollback(
	ctx context.Context,
	runtime *lifecycle.AppleRuntime,
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
	fmt.Printf("stopping Apple Container services\n")
	if err := runtime.StopSystem(ctx); err != nil {
		return fmt.Errorf("stop Apple Container: %w", err)
	}
	defer func() {
		if err := runtime.StartSystem(ctx); err != nil {
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
	return nil
}

func parseMetadataTargets(raw string) ([]lifecycle.MetadataTargetRef, error) {
	targets := make([]lifecycle.MetadataTargetRef, 0)
	for _, candidate := range strings.Split(raw, ",") {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		reference, err := lifecycle.ParseMetadataTargetRef(candidate)
		if err != nil {
			return nil, err
		}
		targets = append(targets, reference)
	}
	if len(targets) > 0 {
		if err := lifecycle.RequireDistinctTargets(targets); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func evidenceOrDefault(directory string) string {
	if strings.TrimSpace(directory) != "" {
		return directory
	}
	root, err := resolveProjectRoot()
	if err != nil {
		return "."
	}
	return filepath.Join(root, "evidence", "label-p3a")
}

func writeMetadataEvidence(
	directory, name string,
	evidence metadataEvidence,
) (string, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create evidence directory: %w", err)
	}
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode evidence: %w", err)
	}
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("write evidence: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return "", fmt.Errorf("write evidence: %w", err)
	}
	return path, nil
}

func printMetadataPlan(
	plan *lifecycle.MetadataSweepPlan,
	inventory *lifecycle.OwnershipInventory,
) {
	fmt.Printf(
		"Apple Container %s (apiserver %s)\n",
		plan.Host.CLIVersion, plan.Host.APIServerVersion,
	)
	fmt.Printf("ownership inventory\n")
	for class, count := range inventory.Totals {
		fmt.Printf("  %-14s %d\n", class, count)
	}
	fmt.Printf("\n%s plan\n", plan.Stage)
	for _, entry := range plan.Entries {
		action := "migrate"
		if entry.Satisfied {
			action = "satisfied"
		}
		fmt.Printf(
			"  %-10s %-46s %-13s -> %-13s %s\n",
			entry.Kind, entry.ID, entry.ClassBefore, entry.ClassAfter, action,
		)
		for _, document := range entry.Documents {
			fmt.Printf(
				"      %s  %s\n", document.BeforeSHA256[:12], document.RelativePath,
			)
		}
	}
	fmt.Printf("  ---\n  %d resource(s) would change\n", plan.ChangeCount)
}

func printMetadataReport(report *lifecycle.MetadataSweepReport) {
	fmt.Printf("\n%s applied\n", report.Stage)
	for _, application := range report.Applied {
		fmt.Printf(
			"  %-10s %-46s %-13s -> %-13s keys=%s\n",
			application.Kind, application.ID,
			application.ClassBefore, application.ClassAfter,
			strings.Join(application.ChangedKeys, ","),
		)
	}
	if len(report.Skipped) > 0 {
		fmt.Printf("  %d already satisfied\n", len(report.Skipped))
	}
	fmt.Printf("  runtime verified: %t\n", report.VerifiedByAPI)
}
