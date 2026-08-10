package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

// The `migrate-labels` subcommand was the operator surface for Phase 2 of Issue
// #123. Phase 3A retires its mutating half and keeps the read-only inventory,
// which is still how an operator reads the phase gate.
//
// The sweep migrated a container by deleting it and recreating it from its own
// observed specification. That was safe while the writer emitted both
// namespaces, because a legacy-only container came back dual. It is not safe
// now: the same code would take a legacy-only container to current-only in one
// destructive step, which is the jump Issue #123 forbids. `--apply` therefore
// refuses before touching anything and names the staged replacement.

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

// runMigrateLabels parses and runs the subcommand without loading the full
// configuration. It resolves only the project root, which is the one thing the
// default evidence location needs, and nothing here writes to the host unless
// the operator asked for it.
func runMigrateLabels(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("migrate-labels", flag.ContinueOnError)
	// The read-only inventory is the default spelling because the mutating form
	// deletes and recreates containers.
	apply := flags.Bool(
		"apply",
		false,
		"retired in Phase 3A; no forward container label migration remains "+
			"anywhere in this binary",
	)
	evidenceDir := flags.String(
		"evidence-dir",
		"",
		"where to write the durable inventory",
	)
	only := flags.String(
		"only",
		"",
		"retired with --apply; `migrate-label-metadata --only` is retired too",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError()
	}
	root, err := resolveProjectRoot()
	if err != nil {
		return err
	}
	manager := newManager(
		config{ProjectRoot: root}, lifecycle.NewAppleRuntime(),
	)
	return manager.MigrateLabels(
		ctx, *apply, *evidenceDir, splitIdentities(*only),
	)
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

// MigrateLabels inventories managed containers. It never mutates the host.
func (manager *manager) MigrateLabels(
	ctx context.Context,
	apply bool,
	evidenceDir string,
	only []string,
) error {
	// Phase 3A retires this sweep's mutating half, and the refusal comes first —
	// before the capability probe, before any listing, before anything that
	// could start the runtime.
	//
	// The sweep migrated a container by deleting it and recreating it from its
	// own observed specification. That was correct while the writer emitted both
	// namespaces: a legacy-only container came back dual. From Phase 3A the
	// writer emits the current namespace alone, so the same code would take a
	// legacy-only container straight to current-only in one destructive step —
	// exactly the jump Issue #123 forbids, performed by a path whose review
	// predates the decision to forbid it. It would also delete the container
	// before discovering that its own verification, which still demands a dual
	// replacement, can no longer pass.
	//
	// Its replacement, `migrate-label-metadata --stage`, has since been retired
	// too: Phase 3B removed every path that reads the legacy namespace, and both
	// of that command's stages existed only to compare the two namespaces. No
	// forward container label migration remains in this binary. The read-only
	// inventory below is unaffected.
	if apply {
		return fmt.Errorf(
			"migrate-labels --apply is retired as of Phase 3A of Issue #123.\n" +
				"It migrated a container by deleting and recreating it, which " +
				"now produces a current-only replacement in one step and " +
				"skips the staged migration the epic requires.\n" +
				"Its staged replacement, `migrate-label-metadata --stage`, is " +
				"retired as of Phase 3B: this binary reads " +
				lifecycle.CurrentLabelNamespace + ".* only, so there is no " +
				"legacy namespace left to migrate from.\n" +
				"`agentopsctl migrate-labels` without --apply remains the " +
				"read-only inventory.",
		)
	}
	// --only narrowed a sweep, and there is no longer a sweep to narrow.
	if len(only) > 0 {
		return fmt.Errorf(
			"--only applied to the retired --apply path; the inventory always " +
				"reports the whole host so its counts can be read as a phase " +
				"gate",
		)
	}
	// The inventory promises not to change the host, and starting the Apple
	// Container system service would break that promise. Nothing on this path
	// starts the runtime.
	capability := manager.runtime.Capability(ctx)
	if !capability.Available || !capability.ServiceRunning {
		return fmt.Errorf(
			"Apple Container is not running; start it with " +
				"`container system start` before taking inventory, which " +
				"never starts it for you",
		)
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
	if audit.HasMalformed() {
		fmt.Println(
			"\ncontainers with incomplete ownership labels are present; " +
				"resolve them before acting on this host",
		)
	}
	return nil
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
