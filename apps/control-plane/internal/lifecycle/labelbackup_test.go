package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Evidence and the rollback plan are deliberately different artifacts, and these
// cases pin the difference. The evidence is committed to the repository, so an
// absolute path in it leaks the operator's home directory and a backup path in
// it points a reader at files containing environment values. The plan is not
// committed, and it has to carry exactly what the evidence omits, because
// nothing else can drive a recovery.
//
// Phase 3B removes the code that chose where a forward run would write its
// backups, along with the forward run itself. The separation these cases
// describe survives it: a retained plan is still read from a private root, and
// the record it carries still has to serialise without host paths.

func TestCommittedEvidenceCarriesNoHostPath(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	record := phase3ARecord(
		t, MetadataKindContainer, "ctr-started", root, backups, legacyTriple(),
	)
	encoded, err := json.Marshal(record)
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
	// A foreign label's value is arbitrary third-party text and never belongs in
	// a committed artifact, so the complete maps stay in the private plan.
	if strings.Contains(string(encoded), "keep-me") {
		t.Fatalf("committed evidence carries a foreign label value:\n%s", encoded)
	}
}

// TestRollbackPlanRoundTripsTheAbsolutePaths is the other half: the evidence
// cannot drive a rollback, so the private plan must.
func TestRollbackPlanRoundTripsTheAbsolutePaths(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	before := legacyTriple()
	record := phase3ARecord(
		t, MetadataKindVolume, "vol-a", root, backups, before,
	)
	encoded, err := json.Marshal(&RollbackPlan{
		Stage:   "retire",
		Applied: []*MetadataApplication{record},
		Locations: [][]RollbackLocation{{{
			Path:       record.Files[0].Path,
			BackupPath: record.Files[0].BackupPath,
		}}},
		Labels: []RollbackLabels{{
			Before: before, After: record.AfterLabels,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParseRollbackPlan(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := plan.Applied[0].Files[0].Path; got != record.Files[0].Path {
		t.Fatalf("plan lost the document path: %q", got)
	}
	if got := plan.Applied[0].Files[0].BackupPath; got != record.Files[0].BackupPath {
		t.Fatalf("plan lost the backup path: %q", got)
	}
	// The stage name is carried verbatim even though this binary no longer
	// defines it: the plan is an opaque restoration record.
	if plan.Applied[0].Stage != "retire" ||
		plan.Applied[0].ClassBefore != "legacy-only" {
		t.Fatalf("the plan lost the Phase 3A record it carries: %#v", plan.Applied[0])
	}
	if err := RollbackMetadataSweep(plan); err != nil {
		t.Fatalf("rollback from the parsed plan: %v", err)
	}
	restored := labelsOnDisk(
		t, record.Files[0].Path, record.Files[0].LabelPath,
	)
	if _, present := restored[CurrentManagedLabelKey]; present {
		t.Fatalf("rollback did not remove the migrated label: %v", restored)
	}
	if !sameLabels(restored, before) {
		t.Fatalf("rollback restored %v, want %v", restored, before)
	}
}

func TestParseRollbackPlanRefusesAMismatchedPlan(t *testing.T) {
	for name, body := range map[string]string{
		"no applied entries": `{"stage":"retire","applied":[],"locations":[]}`,
		"location count mismatch": `{"stage":"retire","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[]}`,
		"document count mismatch": `{"stage":"retire","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[[]],"labels":[{"before":{"a":"b"},"after":{"c":"d"}}]}`,
		"no label maps": `{"stage":"retire","applied":[` +
			`{"kind":"volume","id":"v","files":[{"document":"a"}]}],` +
			`"locations":[[{"path":"/tmp/a","backupPath":"/tmp/b"}]],` +
			`"labels":[{"before":{},"after":{}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRollbackPlan([]byte(body)); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}

// The plan's paths are absolute locations read verbatim out of a file, so
// binding them to the resolved host is the trust boundary of the whole retained
// path. Each case below is a plan that parses cleanly and is internally
// consistent — ParseRollbackPlan accepts every one of them — and must still be
// refused before anything is stopped or written.

func bindablePlan(t *testing.T, root, backups string) *RollbackPlan {
	t.Helper()
	record := phase3ARecord(
		t, MetadataKindVolume, "vol-a", root, backups, legacyTriple(),
	)
	return &RollbackPlan{
		Stage:   "retire",
		Applied: []*MetadataApplication{record},
		Locations: [][]RollbackLocation{{{
			Path:       record.Files[0].Path,
			BackupPath: record.Files[0].BackupPath,
		}}},
		Labels: []RollbackLabels{{
			Before: legacyTriple(), After: record.AfterLabels,
		}},
	}
}

func TestBindToHostAcceptsAPlanThatDescribesThisHost(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	plan := bindablePlan(t, root, backups)
	if err := plan.BindToHost(&MetadataHost{
		AppRoot: root, CLIVersion: "1.1.0",
	}); err != nil {
		t.Fatalf("a plan describing this host was refused: %v", err)
	}
}

func TestBindToHostRefusesAPlanThatDoesNotDescribeThisHost(t *testing.T) {
	for name, mutate := range map[string]func(*RollbackPlan, string){
		"a document outside the application root": func(p *RollbackPlan, root string) {
			p.Applied[0].Files[0].Path = filepath.Join(
				filepath.Dir(root), "elsewhere", "entity.json",
			)
		},
		"a document under a different resource": func(p *RollbackPlan, root string) {
			p.Applied[0].Files[0].Path = filepath.Join(
				root, "volumes", "vol-b", "entity.json",
			)
		},
		"a document the layout does not define": func(p *RollbackPlan, root string) {
			p.Applied[0].Files[0].Path = filepath.Join(
				root, "volumes", "vol-a", "volume.img",
			)
		},
		"an identity that escapes its directory": func(p *RollbackPlan, root string) {
			p.Applied[0].ID = "../networks/net-a"
		},
		"a kind this migration does not know": func(p *RollbackPlan, root string) {
			p.Applied[0].Kind = MetadataResourceKind("image")
		},
		// The label path decides which JSON field is overwritten. A forged one
		// would let a plan rewrite something that is not a labels object.
		"a forged label path": func(p *RollbackPlan, root string) {
			p.Applied[0].Files[0].LabelPath = []string{"name"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := seedAppRoot(t)
			backups := filepath.Join(t.TempDir(), "private-backups")
			plan := bindablePlan(t, root, backups)
			mutate(plan, root)
			if err := plan.BindToHost(&MetadataHost{
				AppRoot: root, CLIVersion: "1.1.0",
			}); err == nil {
				t.Fatalf("a plan with %s was bound to this host", name)
			}
		})
	}
}

func TestBindToHostRequiresAResolvedHost(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	plan := bindablePlan(t, root, backups)
	if err := plan.BindToHost(nil); err == nil {
		t.Fatal("a plan was bound with no host at all")
	}
	if err := plan.BindToHost(&MetadataHost{AppRoot: "  "}); err == nil {
		t.Fatal("a plan was bound to a host with no application root")
	}
}

// TestBindToHostRefusesABackupRootInsideAGitWorkTree restores a property Phase
// 3A enforced when it chose the backup root. Rollback writes new backups of its
// own — a document created since the migration is copied before it is brought
// back in line — and a backup is a verbatim copy of a container's environment.
func TestBindToHostRefusesABackupRootInsideAGitWorkTree(t *testing.T) {
	root := seedAppRoot(t)
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(checkout, "private-backups")
	plan := bindablePlan(t, root, backups)
	err := plan.BindToHost(&MetadataHost{AppRoot: root, CLIVersion: "1.1.0"})
	if err == nil || !strings.Contains(err.Error(), "git work tree") {
		t.Fatalf("a backup root inside a checkout was accepted: %v", err)
	}
}

// A symlinked ancestor is the case a lexical walk misses: ~/.local/state
// symlinked into a dotfiles repository is an ordinary stow arrangement.
func TestBindToHostFollowsASymlinkedAncestorIntoAWorkTree(t *testing.T) {
	root := seedAppRoot(t)
	outer := t.TempDir()
	checkout := filepath.Join(outer, "dotfiles")
	if err := os.MkdirAll(filepath.Join(checkout, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outer, "state")
	if err := os.Symlink(filepath.Join(checkout, "state"), link); err != nil {
		t.Fatal(err)
	}
	plan := bindablePlan(t, root, filepath.Join(link, "agentops", "backups"))
	err := plan.BindToHost(&MetadataHost{AppRoot: root, CLIVersion: "1.1.0"})
	if err == nil || !strings.Contains(err.Error(), "git work tree") {
		t.Fatalf("a symlinked ancestor was not resolved and refused: %v", err)
	}
}

// TestBindToHostRefusesASymlinkedIntermediateDirectory covers the one case
// openDirectory's O_NOFOLLOW cannot: it protects the resource directory and the
// document, but not an intermediate like <appRoot>/volumes. requireNoSymlinkInPath
// is the only guard that walks the whole path, and this is what pins it.
func TestBindToHostRefusesASymlinkedIntermediateDirectory(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	plan := bindablePlan(t, root, backups)
	// Move volumes/ aside and put a symlink in its place, so every path below it
	// still resolves to the same files.
	elsewhere := filepath.Join(t.TempDir(), "volumes")
	if err := os.Rename(filepath.Join(root, "volumes"), elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "volumes")); err != nil {
		t.Fatal(err)
	}
	err := plan.BindToHost(&MetadataHost{AppRoot: root, CLIVersion: "1.1.0"})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked intermediate directory was accepted: %v", err)
	}
}

// The plan supplies both the backup and the digests it should match, so those
// digests agree with themselves by construction. What makes them mean anything
// is re-deriving the migrated form from the backup and requiring it to equal the
// digest the plan recorded for the live document. These cases pin that.
func TestBindToHostVerifiesTheBackupAgainstThePlansOwnClaims(t *testing.T) {
	for name, corrupt := range map[string]func(*testing.T, *RollbackPlan){
		"a backup that is not what the plan hashed": func(t *testing.T, p *RollbackPlan) {
			if err := os.WriteFile(
				p.Applied[0].Files[0].BackupPath,
				[]byte(`{"name":"vol-a","labels":{"com.example.other":"x"}}`),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
		},
		"a backup that does not carry the labels it restores": func(t *testing.T, p *RollbackPlan) {
			// Self-consistent digests, but the labels the plan claims to restore
			// are not the ones inside the backup it points at.
			p.Applied[0].BeforeLabels = map[string]string{"com.example.other": "x"}
			p.Applied[0].Files[0].BeforeLabels = p.Applied[0].BeforeLabels
		},
		"a migration that cannot be reproduced from the backup": func(t *testing.T, p *RollbackPlan) {
			p.Applied[0].Files[0].AfterSHA256 = digestOf([]byte("not this document"))
		},
		"a backup readable by other accounts": func(t *testing.T, p *RollbackPlan) {
			if err := os.Chmod(p.Applied[0].Files[0].BackupPath, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := seedAppRoot(t)
			backups := filepath.Join(t.TempDir(), "private-backups")
			plan := bindablePlan(t, root, backups)
			corrupt(t, plan)
			if err := plan.BindToHost(&MetadataHost{
				AppRoot: root, CLIVersion: "1.1.0",
			}); err == nil {
				t.Fatalf("a plan with %s was bound to this host", name)
			}
		})
	}
}

func TestBindToHostRefusesAScatteredOrWideOpenBackupRoot(t *testing.T) {
	t.Run("backups spread across two roots", func(t *testing.T) {
		root := seedAppRoot(t)
		backups := filepath.Join(t.TempDir(), "private-backups")
		plan := bindablePlan(t, root, backups)
		second := phase3ARecord(
			t, MetadataKindNetwork, "net-a", root,
			filepath.Join(t.TempDir(), "other-backups"), legacyTriple(),
		)
		plan.Applied = append(plan.Applied, second)
		if err := plan.BindToHost(&MetadataHost{
			AppRoot: root, CLIVersion: "1.1.0",
		}); err == nil {
			t.Fatal("a plan naming two backup roots was accepted")
		}
	})
	t.Run("a backup root other accounts can read", func(t *testing.T) {
		root := seedAppRoot(t)
		backups := filepath.Join(t.TempDir(), "private-backups")
		plan := bindablePlan(t, root, backups)
		if err := os.Chmod(backups, 0o755); err != nil {
			t.Fatal(err)
		}
		err := plan.BindToHost(&MetadataHost{AppRoot: root, CLIVersion: "1.1.0"})
		if err == nil || !strings.Contains(err.Error(), "0700") {
			t.Fatalf("a world-readable backup root was accepted: %v", err)
		}
	})
	t.Run("the same resource named twice", func(t *testing.T) {
		root := seedAppRoot(t)
		backups := filepath.Join(t.TempDir(), "private-backups")
		plan := bindablePlan(t, root, backups)
		plan.Applied = append(plan.Applied, plan.Applied[0])
		if err := plan.BindToHost(&MetadataHost{
			AppRoot: root, CLIVersion: "1.1.0",
		}); err == nil {
			t.Fatal("a plan naming one resource twice was accepted")
		}
	})
}

// TestBindToHostRefusesASymlinkedBackupIntermediate covers the backup path's
// own components. Checking only the root leaves <root>/<kind> swappable, and
// rollback both reads a recorded backup through that path and writes a new one
// down it for a document that appeared since the migration.
func TestBindToHostRefusesASymlinkedBackupIntermediate(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	plan := bindablePlan(t, root, backups)
	// Move <root>/volume aside and symlink it, so every backup below still
	// resolves to the same bytes and only the path shape changed.
	kindDirectory := filepath.Join(backups, string(MetadataKindVolume))
	elsewhere := filepath.Join(t.TempDir(), "volume")
	if err := os.Rename(kindDirectory, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, kindDirectory); err != nil {
		t.Fatal(err)
	}
	err := plan.BindToHost(&MetadataHost{AppRoot: root, CLIVersion: "1.1.0"})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked backup intermediate was accepted: %v", err)
	}
}

// TestBindToHostRefusesATargetItCannotRestore proves the live document is
// verified before the runtime is stopped rather than only inside the restore.
// TestBindToHostRefusesPlanControlledOutputStrings pins the two fields a forged
// plan could otherwise write straight to the operator's terminal. Document and
// Backup are printed by RestoreOutcomes and embedded in every preflight error,
// and unlike Path and BackupPath they were never reconstructed and compared.
func TestBindToHostRefusesPlanControlledOutputStrings(t *testing.T) {
	for name, corrupt := range map[string]func(*metadataFileApplication){
		"document": func(file *metadataFileApplication) {
			file.Document = "entity.json\x1b[2K\rrestored 32 resource(s) to " +
				"their pre-migration labels"
		},
		"backup": func(file *metadataFileApplication) {
			file.Backup = "../../elsewhere/entity.json"
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := seedAppRoot(t)
			backups := filepath.Join(t.TempDir(), "private-backups")
			plan := bindablePlan(t, root, backups)
			corrupt(plan.Applied[0].Files[0])
			err := plan.BindToHost(&MetadataHost{
				AppRoot: root, CLIVersion: "1.1.0",
			})
			if err == nil {
				t.Fatalf("a plan-controlled %s string was bound to this host", name)
			}
			// Precise, so the test cannot pass because binding refused for some
			// unrelated reason that happens to mention the same word.
			if !strings.Contains(
				err.Error(), "recorded "+name+" location",
			) {
				t.Fatalf("the refusal does not name the %s field: %v", name, err)
			}
		})
	}
}

// TestBindToHostRefusesAnUnrecognisedFileBeforeStopping proves the layout
// refusal happens at bind time. It used to live only on the post-stop path, so
// a `.DS_Store` — ordinary in ~/Library/Application Support once Finder has
// opened the directory — cost the operator a full runtime stop/start cycle for
// a rollback that was always going to refuse.
func TestBindToHostRefusesAnUnrecognisedFileBeforeStopping(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	plan := bindablePlan(t, root, backups)
	stray := filepath.Join(
		filepath.Dir(plan.Applied[0].Files[0].Path), ".DS_Store",
	)
	if err := os.WriteFile(stray, []byte("finder"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := plan.BindToHost(&MetadataHost{AppRoot: root, CLIVersion: "1.1.0"})
	if err == nil {
		t.Fatal("an unrecognised file did not refuse the plan before the stop")
	}
	if !strings.Contains(err.Error(), ".DS_Store") {
		t.Fatalf("the refusal does not name the unrecognised file: %v", err)
	}
}

func TestBindToHostRefusesATargetItCannotRestore(t *testing.T) {
	root := seedAppRoot(t)
	backups := filepath.Join(t.TempDir(), "private-backups")
	plan := bindablePlan(t, root, backups)
	// A non-label field changed since the migration, so the document is neither
	// recorded shape and is not an acceptable re-serialisation either.
	if err := os.WriteFile(
		plan.Applied[0].Files[0].Path,
		[]byte(`{"name":"vol-renamed","labels":{"`+CurrentManagedLabelKey+`":"v1"}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := plan.BindToHost(&MetadataHost{
		AppRoot: root, CLIVersion: "1.1.0",
	}); err == nil {
		t.Fatal("a plan whose target cannot be restored was bound to this host")
	}
}
