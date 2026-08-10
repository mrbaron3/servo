package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// The `migrate-labels` subcommand is the operator surface for Phase 2 of Issue
// #123. It defaults to a read-only inventory because the mutating form deletes
// and recreates containers: the safe spelling has to be the short one.
//
// Evidence is written before the first mutation rather than after the last one.
// A sweep that crashes halfway is exactly the case where the snapshot matters,
// and a snapshot written at the end would not exist for it.

// labelMigrationEvidence is the durable record of one inventory or sweep. Every
// field is derived from already-redacted structures: no environment value, no
// credential, and no host path reaches this file.
type labelMigrationEvidence struct {
	GeneratedAt           string                         `json:"generatedAt"`
	Mode                  string                         `json:"mode"`
	AppleContainerVersion string                         `json:"appleContainerVersion"`
	Plan                  lifecycle.MigrationAudit       `json:"plan"`
	PlannedSpecs          []lifecycle.PlannedReplacement `json:"plannedSpecs,omitempty"`
	Sweep                 *lifecycle.SweepReport         `json:"sweep,omitempty"`
}

// splitIdentities turns a comma separated --only value into exact identities.
// Empty entries are dropped rather than passed through, because an empty
// identity would never match a container and the resulting failure would point
// at the wrong thing.
func splitIdentities(raw string) []string {
	identities := make([]string, 0)
	for _, candidate := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			identities = append(identities, trimmed)
		}
	}
	return identities
}

// MigrateLabels inventories managed containers and, when applying, migrates
// every selected old-only container to dual labels.
func (manager *manager) MigrateLabels(
	ctx context.Context,
	apply bool,
	evidenceDir string,
	only []string,
) error {
	// --apply must name its targets. "Every old-only container" is a broad
	// selector whose meaning depends on what else happens to be on the host,
	// and Issue #123 forbids mutating on one. The operator reads --only off the
	// inventory they just looked at, which also makes the sweep's blast radius
	// reviewable in the shell history.
	if apply && len(only) == 0 {
		return fmt.Errorf(
			"migrate-labels --apply requires --only with the exact container " +
				"identities to migrate; run without --apply to inventory them",
		)
	}
	// --only narrows a sweep, not an inventory. Accepting it here and ignoring
	// it would make the natural rehearsal for `--apply --only <id>` report
	// something other than what it appears to.
	if !apply && len(only) > 0 {
		return fmt.Errorf(
			"--only applies to --apply; the inventory always reports the whole " +
				"host so its counts can be read as a phase gate",
		)
	}
	// The inventory promises not to change the host, and starting the Apple
	// Container system service would break that promise before the operator has
	// chosen --apply. Only the mutating path may start the runtime.
	capability := manager.runtime.Capability(ctx)
	if !apply {
		if !capability.Available || !capability.ServiceRunning {
			return fmt.Errorf(
				"Apple Container is not running; start it with " +
					"`container system start` before taking inventory, which " +
					"never starts it for you",
			)
		}
	} else {
		if err := manager.ensureRuntime(ctx); err != nil {
			return err
		}
		capability = manager.runtime.Capability(ctx)
	}
	// A bare inventory writes nothing unless the operator asked for a durable
	// copy: it is run repeatedly, often from inside the repository, and
	// silently dropping files into a worktree somebody is about to commit from
	// is not what "read-only" should mean.
	writeInventory := strings.TrimSpace(evidenceDir) != ""
	if evidenceDir == "" {
		evidenceDir = filepath.Join(
			manager.config.ProjectRoot, "evidence", "label-p2",
		)
	}
	sweeper := lifecycle.NewLabelSweeper(manager.runtime)
	sweeper.Only = only
	// A random run id, not just the clock and pid: two runs can share a second,
	// and a pid is reused. Combined with O_EXCL on the write, a collision fails
	// the run instead of overwriting another run's evidence.
	runID, err := evidenceRunID()
	if err != nil {
		return err
	}
	stamp := fmt.Sprintf(
		"%s-%s", time.Now().UTC().Format("20060102T150405Z"), runID,
	)

	if !apply {
		audit, err := sweeper.Plan(ctx, "dry-run")
		if err != nil {
			return err
		}
		printLabelMigrationAudit(audit)
		if writeInventory {
			path, err := writeLabelMigrationEvidence(
				evidenceDir,
				fmt.Sprintf("inventory-%s.json", stamp),
				labelMigrationEvidence{
					GeneratedAt:           stamp,
					Mode:                  "dry-run",
					AppleContainerVersion: capability.Version,
					Plan:                  audit,
				},
			)
			if err != nil {
				return err
			}
			fmt.Printf("\nevidence: %s\n", path)
		}
		if audit.HasConflicts() {
			fmt.Println(
				"\nconflicting containers are present; resolve them before " +
					"running with --apply",
			)
		}
		return nil
	}

	// The snapshot is written from inside the sweep, against the exact audit it
	// is about to act on, and a write failure aborts before anything mutates.
	sweeper.SnapshotBeforeMutation = func(
		audit lifecycle.MigrationAudit,
		planned []lifecycle.PlannedReplacement,
	) error {
		path, err := writeLabelMigrationEvidence(
			evidenceDir,
			fmt.Sprintf("pre-mutation-%s.json", stamp),
			labelMigrationEvidence{
				GeneratedAt:           stamp,
				Mode:                  "pre-mutation",
				AppleContainerVersion: capability.Version,
				Plan:                  audit,
				PlannedSpecs:          planned,
			},
		)
		if err == nil {
			fmt.Printf("pre-mutation snapshot: %s\n", path)
		}
		return err
	}

	report, sweepErr := sweeper.Apply(ctx)
	// The sweep report is written whether or not the sweep succeeded. A halted
	// sweep is precisely when an operator needs to see which stage stopped it.
	path, writeErr := writeLabelMigrationEvidence(
		evidenceDir,
		fmt.Sprintf("sweep-%s.json", stamp),
		labelMigrationEvidence{
			GeneratedAt:           stamp,
			Mode:                  "apply",
			AppleContainerVersion: capability.Version,
			Plan:                  report.Before,
			Sweep:                 &report,
		},
	)
	printSweepReport(report)
	if writeErr != nil {
		if sweepErr != nil {
			return fmt.Errorf(
				"sweep failed (%v) and its evidence could not be written: %w",
				sweepErr, writeErr,
			)
		}
		return writeErr
	}
	fmt.Printf("\nevidence: %s\n", path)
	return sweepErr
}

// evidenceRunID returns a short collision-resistant identity for one run's
// evidence files.
func evidenceRunID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate evidence run identity: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func writeLabelMigrationEvidence(
	directory, name string,
	evidence labelMigrationEvidence,
) (string, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create evidence directory: %w", err)
	}
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode migration evidence: %w", err)
	}
	path := filepath.Join(directory, name)
	// O_EXCL rather than a truncating write: evidence is the record of an
	// irreversible operation, and a second run that happened to choose the same
	// name must fail loudly rather than silently replace the first run's
	// account of what it did.
	file, err := os.OpenFile(
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644,
	)
	if err != nil {
		return "", fmt.Errorf("write migration evidence: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return "", fmt.Errorf("write migration evidence: %w", err)
	}
	return path, nil
}

func printLabelMigrationAudit(audit lifecycle.MigrationAudit) {
	fmt.Printf("container ownership label inventory (%s)\n", audit.Phase)
	for _, record := range audit.Records {
		fmt.Printf(
			"  %-40s %-10s %-13s %-11s %s\n",
			record.ID,
			record.State,
			record.Ownership,
			record.Disposition,
			record.Reason,
		)
		for _, attachment := range record.NamedVolumes {
			readOnly := ""
			if attachment.ReadOnly {
				readOnly = " (read-only)"
			}
			fmt.Printf(
				"      volume %s -> %s%s\n",
				attachment.Name, attachment.Destination, readOnly,
			)
		}
	}
	fmt.Println("  ---")
	for _, disposition := range sortedDispositions(audit.Totals) {
		fmt.Printf("  %-12s %d\n", disposition, audit.Totals[disposition])
	}
}

func printSweepReport(report lifecycle.SweepReport) {
	fmt.Println("sweep steps")
	for _, step := range report.Steps {
		fmt.Printf(
			"  %-40s %-15s %-7s %s\n",
			step.Container, step.Stage, step.Outcome, step.Detail,
		)
	}
	if len(report.Volumes) > 0 {
		fmt.Println("named volume preservation")
		for _, record := range report.Volumes {
			fmt.Printf(
				"  %-50s before=%t after=%t\n",
				record.Name, record.PresentBefore, record.PresentAfter,
			)
		}
	}
	if report.Halted != "" {
		fmt.Printf("halted: %s\n", report.Halted)
		return
	}
	printLabelMigrationAudit(report.After)
}

func sortedDispositions(
	totals map[lifecycle.MigrationDisposition]int,
) []lifecycle.MigrationDisposition {
	names := make([]string, 0, len(totals))
	for disposition := range totals {
		names = append(names, string(disposition))
	}
	sort.Strings(names)
	ordered := make([]lifecycle.MigrationDisposition, 0, len(names))
	for _, name := range names {
		ordered = append(ordered, lifecycle.MigrationDisposition(name))
	}
	return ordered
}
