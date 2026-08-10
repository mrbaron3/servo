package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

func TestPrepareReleaseProvenancePinsCleanServoHeadAndRejectsDrift(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q")
	git("config", "user.name", "AgentOps Test")
	git("config", "user.email", "agentops@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked")
	git("commit", "-qm", "fixture")
	git("remote", "add", "origin", "git@github.com:mrbaron3/servo.git")
	head := git("rev-parse", "HEAD")
	subject := newManager(config{ProjectRoot: root}, nil)
	if err := subject.prepareReleaseProvenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if subject.config.ReleaseConsumerRepository != "mrbaron3/servo" ||
		subject.config.ReleaseConsumerRevision != head {
		t.Fatalf("provenance = %#v", subject.config)
	}

	drifted := newManager(config{
		ProjectRoot: root, ReleaseConsumerRevision: strings.Repeat("a", 40),
	}, nil)
	if err := drifted.prepareReleaseProvenance(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("stale provenance was accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := subject.prepareReleaseProvenance(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty deployment source was accepted: %v", err)
	}
}

func TestBuiltProviderDefaultsUsesExactProviderAndLockDigest(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "deploy", "tools", "provider-cli")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "package.json"),
		[]byte(`{"dependencies":{"@openai/codex":"0.146.0","@anthropic-ai/claude-code":"2.1.221"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "package-lock.json"),
		[]byte("locked provider graph\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	actual, err := builtProviderDefaults(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if actual != `[{"provider":"codex","reference":"codex-cli-0.146.0:provider-default","resolverDigest":"sha256:faeddd4e8bf9e00d399a6c46cff411688f3418c955f0c43378d559d5d841637d"}]` {
		t.Fatalf("provider defaults = %s", actual)
	}

	if _, err := builtProviderDefaults(root, "gemini"); err == nil {
		t.Fatal("unsupported provider provenance was accepted")
	}
	if err := os.WriteFile(
		filepath.Join(directory, "package.json"),
		[]byte(`{"dependencies":{"@openai/codex":"^0.146.0"}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := builtProviderDefaults(root, "codex"); err == nil ||
		!strings.Contains(err.Error(), "exact version") {
		t.Fatalf("non-exact provider version was accepted: %v", err)
	}
}

func TestBuiltReleaseProvenanceOverridesStaleOperatorEnvironment(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "deploy", "tools", "provider-cli")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"package.json":      `{"dependencies":{"@openai/codex":"0.146.0"}}`,
		"package-lock.json": "locked provider graph\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{{
		Status: 0,
		Stdout: `{"configuration":{"descriptor":{"digest":"` + digest + `"}}}`,
	}}}
	subject := newManager(config{
		ProjectRoot:                 root,
		RunnerImage:                 "runner:test",
		Provider:                    "codex",
		ReleaseConsumerRevision:     strings.Repeat("b", 40),
		ReleaseEnvironmentReference: "stale",
		ReleaseEnvironmentDigest:    "sha256:" + strings.Repeat("c", 64),
		ReleaseProviderDefaults:     `[{"provider":"codex","reference":"stale"}]`,
	}, lifecycle.NewAppleRuntimeForTest(fake))
	if err := subject.prepareBuiltReleaseProvenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if subject.config.ReleaseEnvironmentKind != "container" ||
		subject.config.ReleaseEnvironmentReference !=
			"runner:test@source-bbbbbbbbbbbb" ||
		subject.config.ReleaseEnvironmentDigest != digest ||
		!strings.Contains(subject.config.ReleaseProviderDefaults, "codex-cli-0.146.0") {
		t.Fatalf("built provenance = %#v", subject.config)
	}
}

type managerRuntimeRunner struct {
	results          []lifecycle.CommandResult
	args             [][]string
	environments     []map[string]string
	interactive      [][]string
	interactiveError error
}

func (runner *managerRuntimeRunner) RunInteractive(
	_ context.Context,
	args []string,
) error {
	runner.interactive = append(runner.interactive, append([]string(nil), args...))
	return runner.interactiveError
}

func (runner *managerRuntimeRunner) RunWithEnvironment(
	ctx context.Context,
	args []string,
	environment map[string]string,
) lifecycle.CommandResult {
	copied := make(map[string]string, len(environment))
	for key, value := range environment {
		copied[key] = value
	}
	runner.environments = append(runner.environments, copied)
	return runner.Run(ctx, args)
}

func (runner *managerRuntimeRunner) Run(
	_ context.Context,
	args []string,
) lifecycle.CommandResult {
	runner.args = append(runner.args, append([]string(nil), args...))
	result := lifecycle.CommandResult{}
	if len(runner.results) != 0 {
		result = runner.results[0]
		runner.results = runner.results[1:]
	}
	result.Args = append([]string(nil), args...)
	return result
}

func dashboardControlResult() lifecycle.CommandResult {
	return lifecycle.CommandResult{
		Status: 0,
		Stdout: `[{"id":"agentops-control","configuration":{"labels":{` +
			`"com.mrbaron3.servo.agentopsctl":"v1",` +
			`"com.mrbaron3.servo.role":"control"},"publishedPorts":[{` +
			`"hostAddress":"127.0.0.1","hostPort":8080,` +
			`"containerPort":8080,"count":1,"proto":"tcp"}]},` +
			`"status":{"state":"running"}}]`,
	}
}

func TestOpenUsesLatestValidDashboardBootstrapURL(t *testing.T) {
	oldToken := strings.Repeat("a", 32)
	currentToken := strings.Repeat("b", 32)
	currentURL := "http://127.0.0.1:8080/dashboard/bootstrap?token=" +
		currentToken
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		dashboardControlResult(),
		{
			Status: 0,
			Stdout: `{"dashboardBootstrapUrl":"http://127.0.0.1:8080/dashboard/bootstrap?token=` +
				oldToken + `"}` + "\n" +
				`{"dashboardBootstrapUrl":"` + currentURL + `"}` + "\n" +
				`{"dashboardBootstrapUrl":"http://attacker.invalid/dashboard/bootstrap?token=` +
				strings.Repeat("c", 32) + `"}`,
		},
	}}
	subject := newManager(
		testManagerConfig(),
		lifecycle.NewAppleRuntimeForTest(fake),
	)
	subject.dashboardReachable = func(string, int, time.Duration) bool {
		return true
	}
	opened := ""
	subject.openDashboard = func(_ context.Context, target string) error {
		opened = target
		return nil
	}

	if err := subject.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if opened != currentURL {
		t.Fatalf("opened URL = %q", opened)
	}
	if len(fake.args) != 2 ||
		strings.Join(fake.args[0], " ") !=
			"list --all --format json" ||
		strings.Join(fake.args[1], " ") !=
			"logs -n 500 agentops-control" {
		t.Fatalf("control log lookup argv = %#v", fake.args)
	}
}

func TestOpenRejectsUnexpectedControlPublicationBeforeReadingLogs(t *testing.T) {
	control := dashboardControlResult()
	control.Stdout = strings.Replace(
		control.Stdout,
		`"hostAddress":"127.0.0.1"`,
		`"hostAddress":"0.0.0.0"`,
		1,
	)
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{control}}
	subject := newManager(
		testManagerConfig(),
		lifecycle.NewAppleRuntimeForTest(fake),
	)
	subject.dashboardReachable = func(string, int, time.Duration) bool {
		return true
	}
	opened := false
	subject.openDashboard = func(context.Context, string) error {
		opened = true
		return nil
	}

	if err := subject.Open(context.Background()); err == nil {
		t.Fatal("Open() accepted an unexpected control publication")
	}
	if opened {
		t.Fatal("Open() launched the Dashboard for an unexpected control publication")
	}
	if len(fake.args) != 1 ||
		strings.Join(fake.args[0], " ") != "list --all --format json" {
		t.Fatalf("unexpected control lookup argv = %#v", fake.args)
	}
}

func TestOpenErrorsDoNotExposeDashboardBootstrapToken(t *testing.T) {
	token := strings.Repeat("s", 32)
	encodedToken := "%73" + strings.Repeat("s", 31)
	dashboardURL := "http://127.0.0.1:8080/dashboard/bootstrap?token=" +
		encodedToken
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		dashboardControlResult(),
		{
			Status: 0,
			Stdout: `{"dashboardBootstrapUrl":"` + dashboardURL + `"}`,
		},
	}}
	subject := newManager(
		testManagerConfig(),
		lifecycle.NewAppleRuntimeForTest(fake),
	)
	subject.dashboardReachable = func(string, int, time.Duration) bool {
		return true
	}
	subject.openDashboard = func(_ context.Context, target string) error {
		return errors.New("launcher rejected token=" + encodedToken)
	}

	err := subject.Open(context.Background())
	if err == nil ||
		strings.Contains(err.Error(), token) ||
		strings.Contains(err.Error(), encodedToken) ||
		strings.Contains(err.Error(), dashboardURL) {
		t.Fatalf("Open() error exposed bootstrap credential: %v", err)
	}
}

func TestOpenDoesNotExposeControlLogsWhenBootstrapLookupFails(t *testing.T) {
	token := strings.Repeat("x", 32)
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		dashboardControlResult(),
		{
			Status: 1,
			Stdout: `{"dashboardBootstrapUrl":"http://127.0.0.1:8080/dashboard/bootstrap?token=` +
				token + `"}`,
			Stderr: "injected log lookup failure",
		},
	}}
	subject := newManager(
		testManagerConfig(),
		lifecycle.NewAppleRuntimeForTest(fake),
	)
	subject.dashboardReachable = func(string, int, time.Duration) bool {
		return true
	}

	err := subject.Open(context.Background())
	if err == nil ||
		strings.Contains(err.Error(), token) ||
		strings.Contains(err.Error(), "injected log lookup failure") {
		t.Fatalf("Open() exposed control log output: %v", err)
	}
}

func TestReplaceControlPreflightFailureHasNoMutationReceipt(t *testing.T) {
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{{
		Status: 1,
		Stderr: "injected list failure",
	}}}
	subject := newManager(testManagerConfig(), lifecycle.NewAppleRuntimeForTest(fake))
	receipt, err := subject.replaceControl(
		context.Background(),
		lifecycle.ModeMonitorOnly,
	)
	if err == nil || receipt.Mutated {
		t.Fatalf("replaceControl() receipt=%#v err=%v", receipt, err)
	}
	for _, args := range fake.args {
		if len(args) != 0 &&
			(args[0] == "kill" || args[0] == "delete" || args[0] == "run") {
			t.Fatalf("preflight failure mutated runtime with argv %v", args)
		}
	}
}

func TestReplaceControlPostDeleteFailureReturnsMutationReceipt(t *testing.T) {
	postgres := `[{"id":"agentops-postgres","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}}]`
	// The role label is not decoration here: the destructive gate requires it,
	// because a container carrying the ownership marker alone is one this binary
	// never wrote.
	stoppedControl := `[{"id":"agentops-control","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1",` +
		`"com.mrbaron3.servo.role":"control"}},"status":{"state":"stopped"}}]`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: postgres},
		{Status: 0, Stdout: `{"configuration":{"descriptor":{"digest":"sha256:` +
			strings.Repeat("a", 64) + `"}}}`},
		{Status: 0, Stdout: stoppedControl},
		{Status: 0, Stdout: stoppedControl},
		{Status: 0},
		{Status: 1, Stderr: "injected run failure"},
	}}
	subject := newManager(testManagerConfig(), lifecycle.NewAppleRuntimeForTest(fake))
	receipt, err := subject.replaceControl(
		context.Background(),
		lifecycle.ModeMonitorOnly,
	)
	if err == nil || !receipt.Mutated {
		t.Fatalf("replaceControl() receipt=%#v err=%v", receipt, err)
	}
	foundDelete := false
	for _, args := range fake.args {
		if len(args) != 0 && args[0] == "delete" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatal("injected post-delete failure did not exercise a mutation")
	}
}

func TestValidateSpecActualRejectsImageAndConfigurationDrift(t *testing.T) {
	spec := lifecycle.ContainerSpec{
		Name: "agentops-control", Role: "control", Image: "control:test",
		Networks:    []string{"default", "agentops-internal"},
		Environment: map[string]string{"SECRET": "first"},
		Init:        true, ReadOnly: true, CapDropAll: true, Detach: true,
	}
	image := "sha256:" + strings.Repeat("a", 64)
	digest, err := lifecycle.SpecDigest(spec, image)
	if err != nil {
		t.Fatal(err)
	}
	spec.SpecDigest = digest
	actual := &lifecycle.ContainerActual{}
	actual.ID = spec.Name
	actual.Configuration.Image.Descriptor.Digest = image
	actual.Configuration.Labels = map[string]string{
		"com.mrbaron3.servo.spec-sha256": digest,
	}
	actual.Configuration.InitProcess.Environment = []string{"SECRET=first"}
	if err := validateSpecActual(actual, spec); err != nil {
		t.Fatal(err)
	}
	actual.Configuration.Image.Descriptor.Digest =
		"sha256:" + strings.Repeat("b", 64)
	if err := validateSpecActual(actual, spec); err == nil {
		t.Fatal("mutable-tag image drift was accepted")
	}
	actual.Configuration.Image.Descriptor.Digest = image
	spec.Environment["SECRET"] = "rotated"
	if err := validateSpecActual(actual, spec); err == nil {
		t.Fatal("environment drift was accepted")
	}
}

func TestExistingWorkerModeUsesSealedContainerBoundaryDuringRecovery(t *testing.T) {
	actual := &lifecycle.ContainerActual{}
	actual.Configuration.InitProcess.Environment = []string{
		"AGENTOPS_OPERATING_MODE=ACTIVE",
	}
	mode, err := existingWorkerMode(actual, false)
	if err != nil || mode != lifecycle.ModeActive {
		t.Fatalf("ACTIVE triage recovery mode = %q, %v", mode, err)
	}
	mode, err = existingWorkerMode(actual, true)
	if err != nil || mode != lifecycle.ModeActive {
		t.Fatalf("ACTIVE runner recovery mode = %q, %v", mode, err)
	}

	actual.Configuration.InitProcess.Environment = []string{
		"AGENTOPS_OPERATING_MODE=MONITOR_ONLY",
	}
	if _, err := existingWorkerMode(actual, true); err == nil {
		t.Fatal("MONITOR_ONLY runner boundary was accepted")
	}
	actual.Configuration.InitProcess.Environment = []string{
		"AGENTOPS_OPERATING_MODE=ACTIVE",
		"AGENTOPS_OPERATING_MODE=DRAINING",
	}
	if _, err := existingWorkerMode(actual, false); err == nil {
		t.Fatal("duplicated worker mode was accepted")
	}
}

func TestRedactContainerStatusRemovesCredentialEnvironmentValues(t *testing.T) {
	actual := &lifecycle.ContainerActual{}
	actual.Configuration.InitProcess.Environment = []string{
		"AGENTOPS_DATABASE_URL=postgresql://role:database-secret@db/agentops",
		"AGENTOPS_RUNNER_GITHUB_TOKEN=github-secret",
		"AGENTOPS_GITHUB_BROKER_CAPABILITY=broker-capability-secret",
		"AGENTOPS_OPERATING_MODE=ACTIVE",
	}
	redacted := redactContainerStatus(actual)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(encoded)
	for _, secret := range []string{
		"database-secret",
		"github-secret",
		"broker-capability-secret",
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("status JSON leaked %q: %s", secret, rendered)
		}
	}
	if !strings.Contains(rendered, "AGENTOPS_DATABASE_URL=***") ||
		!strings.Contains(rendered, "AGENTOPS_RUNNER_GITHUB_TOKEN=***") ||
		!strings.Contains(
			rendered,
			"AGENTOPS_GITHUB_BROKER_CAPABILITY=***",
		) ||
		!strings.Contains(rendered, "AGENTOPS_OPERATING_MODE=ACTIVE") {
		t.Fatalf("unexpected redacted status JSON: %s", rendered)
	}
	if actual.Configuration.InitProcess.Environment[0] ==
		"AGENTOPS_DATABASE_URL=***" {
		t.Fatal("redaction mutated the runtime inspection source")
	}
}

func TestManagedNonRootUserAcceptsAppleNumericIdentityWithoutRoot(t *testing.T) {
	for _, user := range []struct {
		uid int
		raw string
	}{
		{uid: 65532},
		{raw: "agentops"},
		{raw: "65532"},
		{raw: "65532:65532"},
	} {
		if !managedNonRootUser(user.uid, user.raw) {
			t.Fatalf("managed identity was rejected: %#v", user)
		}
	}
	for _, user := range []struct {
		uid int
		raw string
	}{
		{},
		{raw: "root"},
		{raw: "0:0"},
		{raw: "65532:0"},
		{uid: 65532, raw: "root"},
		{uid: 65532, raw: "0:0"},
		{uid: 65532, raw: "65532:0"},
		{uid: 1234, raw: "agentops"},
	} {
		if managedNonRootUser(user.uid, user.raw) {
			t.Fatalf("unsafe identity was accepted: %#v", user)
		}
	}
}

func TestPostgresSpecRejectsMutableTagImageAndCredentialDrift(t *testing.T) {
	cfg := testManagerConfig()
	cfg.PostgresPassword = "postgres-password-first-value-0001"
	subject := newManager(cfg, nil)
	spec := subject.postgresSpec()
	image := "sha256:" + strings.Repeat("c", 64)
	digest, err := lifecycle.SpecDigest(spec, image)
	if err != nil {
		t.Fatal(err)
	}
	spec.SpecDigest = digest
	actual := &lifecycle.ContainerActual{}
	actual.ID = cfg.PostgresContainer
	actual.Configuration.Image.Descriptor.Digest = image
	actual.Configuration.Labels = map[string]string{
		"com.mrbaron3.servo.spec-sha256": digest,
	}
	actual.Configuration.InitProcess.Environment = []string{
		"POSTGRES_PASSWORD=postgres-password-first-value-0001",
		"POSTGRES_DB=agentops",
		"PGDATA=/var/lib/postgresql/18/docker",
	}
	if err := validateSpecActual(actual, spec); err != nil {
		t.Fatal(err)
	}
	actual.Configuration.Image.Descriptor.Digest =
		"sha256:" + strings.Repeat("d", 64)
	if err := validateSpecActual(actual, spec); err == nil {
		t.Fatal("PostgreSQL mutable-tag image drift was accepted")
	}
	actual.Configuration.Image.Descriptor.Digest = image
	spec.Environment["POSTGRES_PASSWORD"] = "postgres-password-rotated-value-0002"
	if err := validateSpecActual(actual, spec); err == nil {
		t.Fatal("PostgreSQL administrator credential drift was accepted")
	}
}

func TestPostgresMajorUpgradeBoundaryPreservesLegacyData(t *testing.T) {
	actual := &lifecycle.ContainerActual{}
	actual.Configuration.InitProcess.Environment = []string{
		"PG_MAJOR=16",
		"PGDATA=/var/lib/postgresql/data",
	}
	if err := validatePostgresMajorBoundary(actual); err == nil ||
		!strings.Contains(err.Error(), "preserved") {
		t.Fatalf("legacy PostgreSQL container was accepted: %v", err)
	}
	actual.Configuration.InitProcess.Environment = []string{
		"PG_MAJOR=18",
		"PGDATA=/var/lib/postgresql/18/docker",
	}
	if err := validatePostgresMajorBoundary(actual); err != nil {
		t.Fatalf("current PostgreSQL container was rejected: %v", err)
	}
}

func TestPostgresVolumeLayoutFailsClosedForLegacyOrAmbiguousData(t *testing.T) {
	for _, output := range []string{
		"current=\nlegacy=16\n",
		"current=18\nlegacy=16\n",
		"current=17\nlegacy=\n",
		"current=18\n",
		"unexpected=18\nlegacy=\n",
		"current=18\ncurrent=18\nlegacy=\n",
		"current=18-dev\nlegacy=\n",
	} {
		if err := validatePostgresVolumeLayout(output); err == nil {
			t.Fatalf("unsafe volume layout was accepted: %q", output)
		}
	}
	for _, output := range []string{
		"current=\nlegacy=\n",
		"current=18\nlegacy=\n",
	} {
		if err := validatePostgresVolumeLayout(output); err != nil {
			t.Fatalf("safe volume layout %q was rejected: %v", output, err)
		}
	}
}

func TestPostgresVolumeProbeIsReadOnlyAndAcceptsAnEmptyVolume(t *testing.T) {
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{{
		Status: 0,
		Stdout: "current=\nlegacy=\n",
	}}}
	subject := newManager(
		testManagerConfig(),
		lifecycle.NewAppleRuntimeForTest(fake),
	)
	if err := subject.validatePostgresVolumeLayout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.args) != 1 {
		t.Fatalf("volume probe commands = %#v", fake.args)
	}
	rendered := strings.Join(fake.args[0], " ")
	for _, expected := range []string{
		"--rm",
		"--read-only",
		"--cap-drop ALL",
		"--volume agentops-postgres-data:/var/lib/postgresql:ro",
		"--entrypoint /bin/sh",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("volume probe omitted %q: %s", expected, rendered)
		}
	}
}

func TestProbeRunnerProviderFailsClosedWithoutLeakingOutput(t *testing.T) {
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{{
		Status: 1,
		Stderr: "secret provider rejection response",
	}}}
	subject := newManager(testManagerConfig(), lifecycle.NewAppleRuntimeForTest(fake))
	err := subject.probeRunnerProvider(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret provider") {
		t.Fatalf("probeRunnerProvider() err=%v", err)
	}
	if len(fake.args) != 1 ||
		strings.Join(fake.args[0], " ") !=
			"exec agentops-runner codex debug models" {
		t.Fatalf("unexpected provider probe argv: %#v", fake.args)
	}
}

func TestCompensationTopologyRestoresTriageWithoutDevelopmentRunner(t *testing.T) {
	stops, restoreControl, restoreBroker, restoreTriage, restoreRunner :=
		compensationTopology(
			lifecycle.ModeMonitorOnly,
			true,
			true,
			true,
			true,
			"agentops-control",
			"agentops-github-broker",
			"agentops-triage",
			"agentops-runner",
		)
	if strings.Join(stops, ",") !=
		"agentops-runner,agentops-triage,agentops-github-broker" ||
		!restoreControl ||
		!restoreBroker ||
		!restoreTriage ||
		restoreRunner {
		t.Fatalf(
			"MONITOR_ONLY compensation stops=%v control=%t triage=%t runner=%t",
			stops,
			restoreControl,
			restoreTriage,
			restoreRunner,
		)
	}

	_, _, restoreDrainingBroker, restoreDrainingTriage,
		restoreDrainingRunner := compensationTopology(
		lifecycle.ModeDraining,
		true,
		true,
		true,
		true,
		"agentops-control",
		"agentops-github-broker",
		"agentops-triage",
		"agentops-runner",
	)
	if !restoreDrainingBroker ||
		restoreDrainingTriage ||
		restoreDrainingRunner {
		t.Fatal("DRAINING compensation recreated a worker")
	}
}

func TestPRIntentPinsCredentialAndProviderReadinessBoundaries(t *testing.T) {
	if ProviderProbeTimeout != 45*time.Second ||
		CredentialSeedReadyTimeout != 20*time.Second ||
		ContainerReadyPollInterval != 250*time.Millisecond ||
		CredentialInitializerLifetime != 10*time.Minute ||
		RunnerReadinessLogLines != 100 {
		t.Fatalf(
			"unexpected readiness boundaries: probe=%s seed=%s poll=%s initializer=%s logs=%d",
			ProviderProbeTimeout,
			CredentialSeedReadyTimeout,
			ContainerReadyPollInterval,
			CredentialInitializerLifetime,
			RunnerReadinessLogLines,
		)
	}
	if ProviderProbeTimeout <= control.DefaultMonitorBrokerTimeout ||
		CredentialSeedReadyTimeout >= CredentialInitializerLifetime {
		t.Fatal("provider/credential readiness relationships are unsafe")
	}
}

func TestCISO07IntegratedModeTopology(t *testing.T) {
	t.Run("mode-specific triage and development boundaries", func(t *testing.T) {
		cfg := testManagerConfig()
		cfg.TriageDBPassword = "triage-database-password-value-0001"
		cfg.RunnerDBPassword = "runner-database-password-value-0001"
		cfg.ControlDBPassword = "control-database-password-value-001"
		cfg.TriageReadyLabel = "human-approved"
		cfg.TriageClaimedLabel = "automation-owned"
		cfg.ReleaseConsumerRepository = "mrbaron3/servo"
		cfg.ReleaseConsumerRevision = strings.Repeat("a", 40)
		subject := newManager(cfg, nil)

		monitorControl := subject.controlSpec(lifecycle.ModeMonitorOnly, "192.0.2.10")
		monitorTriage := subject.triageSpec(
			lifecycle.ModeMonitorOnly,
			"192.0.2.10",
			"192.0.2.11",
			"192.0.2.12",
		)
		if monitorControl.Environment["AGENTOPS_GITHUB_MONITOR_BROKER_ENABLED"] != "true" ||
			monitorControl.Environment["AGENTOPS_RUNNER_EGRESS_PROXY_LISTEN"] !=
				"0.0.0.0:8082" {
			t.Fatalf("MONITOR_ONLY control broker boundary = %#v", monitorControl.Environment)
		}
		if monitorTriage.Environment["AGENTOPS_GITHUB_BROKER_CAPABILITY"] !=
			cfg.githubBrokerCapability("triage") ||
			monitorTriage.Environment["AGENTOPS_GITHUB_BROKER_ROLE"] !=
				"triage" ||
			monitorTriage.Environment["AGENTOPS_GITHUB_BROKER_URL"] !=
				"http://192.0.2.12:8083" ||
			monitorTriage.Environment["AGENTOPS_TRIAGE_READY_LABEL"] !=
				"human-approved" ||
			monitorTriage.Environment["AGENTOPS_TRIAGE_CLAIMED_LABEL"] !=
				"automation-owned" ||
			monitorTriage.Environment["HTTPS_PROXY"] !=
				"http://192.0.2.11:8082" {
			t.Fatal("MONITOR_ONLY triage did not receive its broker-only boundary")
		}
		for _, key := range []string{"CODEX_HOME", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
			if _, present := monitorTriage.Environment[key]; present {
				t.Fatalf("MONITOR_ONLY triage received provider credential %s", key)
			}
		}
		if len(monitorTriage.Mounts) != 0 {
			t.Fatalf("MONITOR_ONLY triage mounts = %#v", monitorTriage.Mounts)
		}
		var monitorOutbound []map[string]any
		if err := json.Unmarshal(
			[]byte(monitorTriage.Environment["AGENTOPS_TRIAGE_OUTBOUND_JSON"]),
			&monitorOutbound,
		); err != nil {
			t.Fatal(err)
		}
		if len(monitorOutbound) != 3 ||
			monitorOutbound[0]["host"] != "192.0.2.10" ||
			monitorOutbound[1]["host"] != "192.0.2.12" ||
			monitorOutbound[2]["host"] != "api.github.com" {
			t.Fatalf("MONITOR_ONLY outbound = %#v", monitorOutbound)
		}

		activeControl := subject.controlSpec(lifecycle.ModeActive, "192.0.2.10")
		activeBroker := subject.githubBrokerSpec(lifecycle.ModeActive)
		activeTriage := subject.triageSpec(
			lifecycle.ModeActive,
			"192.0.2.10",
			"192.0.2.11",
			"192.0.2.12",
		)
		activeRunner := subject.runnerSpec(
			lifecycle.ModeActive,
			"192.0.2.10",
			"192.0.2.11",
			"192.0.2.12",
		)
		if activeControl.Environment["AGENTOPS_RELEASE_CONSUMER_REVISION"] !=
			activeRunner.Environment["AGENTOPS_RELEASE_CONSUMER_REVISION"] ||
			activeControl.Environment["AGENTOPS_RELEASE_CONSUMER_REPOSITORY"] !=
				"mrbaron3/servo" {
			t.Fatal("control/dashboard and runner release provenance diverged")
		}
		if activeControl.Environment["AGENTOPS_GITHUB_MONITOR_BROKER_ENABLED"] != "true" ||
			activeControl.Environment["AGENTOPS_RUNNER_EGRESS_PROXY_LISTEN"] !=
				"0.0.0.0:8082" {
			t.Fatalf("ACTIVE control broker boundary = %#v", activeControl.Environment)
		}
		if activeBroker.Environment["AGENTOPS_GITHUB_BROKER_TRIAGE_CAPABILITY"] !=
			cfg.githubBrokerCapability("triage") ||
			activeBroker.Environment["AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY"] !=
				cfg.githubBrokerCapability("runner") ||
			activeBroker.Environment["AGENTOPS_GITHUB_BROKER_TRIAGE_CAPABILITY"] ==
				activeBroker.Environment["AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY"] {
			t.Fatal("ACTIVE broker did not isolate role capabilities")
		}
		// Compensation restores the broker with the DRAINING target, so that
		// spec has to be one the broker will actually serve: same development
		// scope as ACTIVE, so a draining runner can still close its attempt.
		drainingBroker := subject.githubBrokerSpec(lifecycle.ModeDraining)
		if drainingBroker.Environment["AGENTOPS_OPERATING_MODE"] != "DRAINING" ||
			drainingBroker.Environment["AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY"] !=
				cfg.githubBrokerCapability("runner") {
			t.Fatalf(
				"DRAINING broker lost its development scope = %#v",
				drainingBroker.Environment,
			)
		}
		// The broker verifies capabilities; it never presents one. Its readiness
		// probe reaches an unauthenticated /healthz, so holding a client-side
		// capability would only widen where that secret can be read.
		for _, key := range []string{
			"AGENTOPS_GITHUB_BROKER_CAPABILITY",
			"AGENTOPS_GITHUB_BROKER_ROLE",
			"AGENTOPS_GITHUB_APP_PRIVATE_KEY_FILE",
		} {
			if _, present := activeBroker.Environment[key]; present {
				t.Fatalf("broker container carries client configuration %s", key)
			}
		}
		if activeTriage.Environment["AGENTOPS_GITHUB_BROKER_CAPABILITY"] !=
			cfg.githubBrokerCapability("triage") ||
			activeTriage.Environment["AGENTOPS_RUNNER_GITHUB_TOKEN"] != "" ||
			activeTriage.Environment["CODEX_HOME"] !=
				"/run/agentops-credentials/codex" ||
			len(activeTriage.Mounts) != 1 ||
			activeTriage.Mounts[0].Target != "/run/agentops-credentials" ||
			activeTriage.Mounts[0].Volume != cfg.TriageCredentialVolume {
			t.Fatal("ACTIVE triage escaped its Issue-only capability boundary")
		}
		if activeRunner.Environment["AGENTOPS_GITHUB_BROKER_CAPABILITY"] !=
			cfg.githubBrokerCapability("runner") ||
			activeRunner.Environment["AGENTOPS_TRIAGE_GITHUB_TOKEN"] != "" ||
			activeRunner.Environment["AGENTOPS_MONITOR_REPOSITORIES"] != "" ||
			activeRunner.Environment["CODEX_HOME"] !=
				"/run/agentops-credentials/codex" {
			t.Fatal("ACTIVE runner did not receive its development-only boundary")
		}
		if len(activeRunner.Mounts) != 2 ||
			activeRunner.Mounts[1].Volume != cfg.RunnerCredentialVolume ||
			!activeRunner.Mounts[1].ReadOnly {
			t.Fatalf("ACTIVE credential mounts = %#v", activeRunner.Mounts)
		}
		// Apple Containerのnamed volumeは単一VMへの排他attach。triageとrunnerが
		// credential volumeを共有すると、後からattachする側が必ず起動に失敗する。
		if activeTriage.Mounts[0].Volume == activeRunner.Mounts[1].Volume {
			t.Fatal("triage and runner share a credential volume")
		}
	})
}

func TestPRIntentPostgresRotationUsesEnvironmentCapability(t *testing.T) {
	postgres := `[{"id":"agentops-postgres","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}}]`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: "container 0.12.0"},
		{Status: 0},
		{Status: 0, Stdout: postgres},
		{Status: 0, Stdout: `{"rotated":true}`},
	}}
	cfg := testManagerConfig()
	cfg.PostgresPassword = "postgres-current-password-value-0001"
	cfg.NextPostgresPassword = "postgres-next-password-value-000002"
	cfg.ControlDBPassword = "control-role-password-value-000003"
	cfg.RunnerDBPassword = "runner-role-password-value-0000004"
	subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))

	if err := subject.RotatePostgresAdmin(
		context.Background(),
		"rotation-request-001",
	); err != nil {
		t.Fatal(err)
	}
	if len(fake.environments) != 1 {
		t.Fatalf("admin environments = %#v", fake.environments)
	}
	environment := fake.environments[0]
	if environment["AGENTOPS_NEXT_POSTGRES_PASSWORD"] !=
		cfg.NextPostgresPassword {
		t.Fatal("next password was not forwarded through the environment")
	}
	argv := strings.Join(fake.args[len(fake.args)-1], " ")
	if strings.Contains(argv, cfg.NextPostgresPassword) ||
		!strings.Contains(argv, "rotate-postgres-admin --request-id rotation-request-001") {
		t.Fatalf("unsafe or incorrect rotation argv: %s", argv)
	}
}

func TestPRIntentPostgresRotationFailsClosedAndRedactsRuntimeFailure(t *testing.T) {
	cfg := testManagerConfig()
	cfg.PostgresPassword = "postgres-current-password-value-0001"
	cfg.ControlDBPassword = "control-role-password-value-000003"
	cfg.RunnerDBPassword = "runner-role-password-value-0000004"
	subject := newManager(cfg, lifecycle.NewAppleRuntimeForTest(
		&managerRuntimeRunner{},
	))
	if err := subject.RotatePostgresAdmin(
		context.Background(),
		"rotation-request-002",
	); err == nil {
		t.Fatal("missing next password was accepted")
	}

	cfg.NextPostgresPassword = "postgres-next-password-value-000002"
	postgres := `[{"id":"agentops-postgres","configuration":{"labels":` +
		`{"com.mrbaron3.servo.agentopsctl":"v1"}},"status":{"state":"running",` +
		`"networks":[{"network":"agentops-internal","ipv4Address":"192.0.2.10/24"}]}}]`
	fake := &managerRuntimeRunner{results: []lifecycle.CommandResult{
		{Status: 0, Stdout: "container 0.12.0"},
		{Status: 0},
		{Status: 0, Stdout: postgres},
		{Status: 1, Stderr: "failure " + cfg.NextPostgresPassword},
	}}
	subject = newManager(cfg, lifecycle.NewAppleRuntimeForTest(fake))
	err := subject.RotatePostgresAdmin(
		context.Background(),
		"rotation-request-003",
	)
	if err == nil || strings.Contains(err.Error(), cfg.NextPostgresPassword) {
		t.Fatalf("rotation failure was absent or leaked a secret: %v", err)
	}
}

func testManagerConfig() config {
	return config{
		Prefix:                 "agentops",
		Network:                "agentops-internal",
		PostgresVolume:         "agentops-postgres-data",
		RunnerVolume:           "agentops-runner-workspace",
		TriageCredentialVolume: "agentops-triage-credentials",
		RunnerCredentialVolume: "agentops-runner-credentials",
		GitHubAppKeyVolume:     "agentops-github-app-key",
		PostgresContainer:      "agentops-postgres",
		ControlContainer:       "agentops-control",
		GitHubBrokerContainer:  "agentops-github-broker",
		TriageContainer:        "agentops-triage",
		RunnerContainer:        "agentops-runner",
		PostgresImage:          "agentops-postgres:dev",
		ControlImage:           "control:test",
		GitHubBrokerImage:      "github-broker:test",
		TriageImage:            "triage:test",
		RunnerImage:            "runner:test",
		ControlHostPort:        8080,
		Provider:               "codex",
		TriageDBPassword:       "triage-database-password-value-0001",
		RunnerDBPassword:       "runner-database-password-value-0001",
		GitHubAppID:            42,
		GitHubInstallationID:   99,
		GitHubAppSlug:          "agentops-test",
		GitHubAppOwner:         "acme",
		TriageBrokerCapability: strings.Repeat("t", 43),
		RunnerBrokerCapability: strings.Repeat("r", 43),
	}
}
