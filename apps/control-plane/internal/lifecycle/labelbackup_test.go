package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A backup is a verbatim copy of a container's configuration document, and on
// this project's own topology that document carries initProcess.environment
// with values — POSTGRES_PASSWORD among them. These tests pin the two
// properties that follow: the backup root never sits where a checkout could
// commit it, and it is not readable by other accounts.

func TestResolveBackupRootRefusesAPathInsideAGitWorkTree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "evidence", "label-p3a", "backups")
	_, err := ResolveBackupRoot(nested)
	if err == nil || !strings.Contains(err.Error(), "git work tree") {
		t.Fatalf("expected a git work tree refusal, got %v", err)
	}
	if _, statErr := os.Stat(nested); statErr == nil {
		t.Fatal("the refused backup root was created anyway")
	}
}

func TestResolveBackupRootRefusesAWorktreeWhoseGitIsAFile(t *testing.T) {
	// A linked worktree records its git directory in a `.git` file rather than a
	// directory, and it is just as committable.
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveBackupRoot(
		filepath.Join(root, "backups"),
	); err == nil {
		t.Fatal("expected a linked worktree to be refused")
	}
}

func TestResolveBackupRootCreatesAPrivateDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "backups")
	resolved, err := ResolveBackupRoot(root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("backup root is mode %v, want 0700", info.Mode().Perm())
	}
}

func TestResolveBackupRootNarrowsAnExistingWideDirectory(t *testing.T) {
	// MkdirAll leaves an existing directory's mode alone, so a root created
	// wider by an earlier run has to be narrowed rather than trusted.
	root := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveBackupRoot(root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("backup root stayed at %v", info.Mode().Perm())
	}
}

func TestResolveBackupRootHonoursTheEnvironmentDefault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "from-env")
	t.Setenv(defaultBackupRootEnv, root)
	resolved, err := ResolveBackupRoot("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != root {
		t.Fatalf("resolved %q, want %q", resolved, root)
	}
}

// TestCommittedEvidenceCarriesNoHostPath is the regression test for the review
// finding that produced this file. The evidence is committed to the repository,
// so an absolute path in it is a leak of the operator's home directory, and a
// backup path in it points a reader at files containing environment values.
func TestCommittedEvidenceCarriesNoHostPath(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	target, err := resolveMetadataTarget(root, MetadataKindContainer, "ctr-started")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := applyMetadataStage(
		target, MetadataStagePrepare, backups, root,
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	report := &MetadataSweepReport{
		Stage:          MetadataStagePrepare,
		Host:           MetadataHost{AppRoot: root, CLIVersion: "1.1.0"},
		Applied:        []*MetadataApplication{applied},
		BackupRoot:     backups,
		BackupRootName: filepath.Base(backups),
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{root, backups} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf(
				"committed evidence carries the absolute path %q:\n%s",
				forbidden, encoded,
			)
		}
	}
	// The relative renderings must still be there, otherwise the evidence would
	// be sanitised into uselessness.
	if !strings.Contains(string(encoded), "containers/ctr-started/config.json") {
		t.Fatalf("evidence lost its relative document path:\n%s", encoded)
	}
}

// TestRollbackPlanCarriesTheAbsolutePathsRollbackNeeds is the other half: the
// evidence cannot drive a rollback, so the private plan must.
func TestRollbackPlanRoundTripsTheAbsolutePaths(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	target, err := resolveMetadataTarget(root, MetadataKindVolume, "vol-a")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := applyMetadataStage(
		target, MetadataStagePrepare, backups, root,
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	encoded, err := json.Marshal(BuildRollbackPlan(&MetadataSweepReport{
		Stage:   MetadataStagePrepare,
		Applied: []*MetadataApplication{applied},
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParseRollbackPlan(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := plan.Applied[0].Files[0].Path; got != applied.Files[0].Path {
		t.Fatalf("plan lost the document path: %q", got)
	}
	if err := RollbackMetadataSweep(plan); err != nil {
		t.Fatalf("rollback from the parsed plan: %v", err)
	}
	restored, err := os.ReadFile(applied.Files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restored), CurrentManagedLabelKey) {
		t.Fatalf("rollback did not remove the prepared label:\n%s", restored)
	}
}

func TestParseRollbackPlanRefusesAMismatchedPlan(t *testing.T) {
	for name, body := range map[string]string{
		"no applied entries": `{"stage":"prepare","applied":[],"locations":[]}`,
		"location count mismatch": `{"stage":"prepare","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[]}`,
		"document count mismatch": `{"stage":"prepare","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[[]]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRollbackPlan([]byte(body)); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}

func TestResolveBackupRootFollowsASymlinkedAncestorIntoAWorkTree(t *testing.T) {
	// ~/.local/state symlinked into a dotfiles repository is an ordinary stow
	// arrangement. A lexical walk over the un-resolved path would never see the
	// checkout, and the credential-bearing backups would land inside it.
	root := t.TempDir()
	checkout := filepath.Join(root, "dotfiles")
	if err := os.MkdirAll(filepath.Join(checkout, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state")
	if err := os.Symlink(filepath.Join(checkout, "state"), link); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveBackupRoot(filepath.Join(link, "agentops", "backups"))
	if err == nil || !strings.Contains(err.Error(), "git work tree") {
		t.Fatalf("expected a symlinked ancestor to be resolved and refused, got %v", err)
	}
}
