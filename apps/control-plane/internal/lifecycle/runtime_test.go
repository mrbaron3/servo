package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRuntimeRunner struct {
	results []CommandResult
	args    [][]string
}

func (runner *fakeRuntimeRunner) Run(
	_ context.Context,
	args []string,
) CommandResult {
	runner.args = append(runner.args, append([]string(nil), args...))
	if len(runner.results) == 0 {
		return CommandResult{}
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	result.Args = append([]string(nil), args...)
	return result
}

func (runner *fakeRuntimeRunner) RunWithStdin(
	ctx context.Context,
	args []string,
	_ io.Reader,
) CommandResult {
	return runner.Run(ctx, args)
}

func TestBuildContainerArgsEnforcesPublicationAndNamedMounts(t *testing.T) {
	base := ContainerSpec{
		Name: "agentops-runner", Role: "runner", Image: "runner:test",
		Networks: []string{"agentops-internal"}, Detach: true,
		ReadOnly: true, CapDropAll: true,
		Mounts: []Mount{{Volume: "agentops-runner-data", Target: "/workspace"}},
	}
	if _, _, err := buildContainerArgs(base); err != nil {
		t.Fatal(err)
	}
	published := base
	published.Publish = []Publication{{
		HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 8080,
	}}
	if _, _, err := buildContainerArgs(published); err == nil {
		t.Fatal("runner host publication was accepted")
	}
	bind := base
	bind.Mounts[0].Volume = "/Users/operator"
	if _, _, err := buildContainerArgs(bind); err == nil {
		t.Fatal("host bind mount was accepted")
	}
}

func TestContainerSpecDigestBindsNonSecretEnvironmentWithoutCredentialFingerprint(t *testing.T) {
	spec := ContainerSpec{
		Name: "agentops-control", Role: "control", Image: "control:test",
		Networks: []string{"default", "agentops-internal"},
		Environment: map[string]string{
			"AGENTOPS_OPERATING_MODE": "ACTIVE",
			"SECRET":                  "first",
		},
		Init: true, ReadOnly: true, CapDropAll: true, Detach: true,
		Publish: []Publication{{
			HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 8080,
		}},
	}
	digest, err := SpecDigest(
		spec,
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	if err != nil {
		t.Fatal(err)
	}
	spec.SpecDigest = digest
	args, _, err := buildContainerArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		strings.Join(args, " "),
		"--label com.mrbaron3.servo.spec-sha256="+digest,
	) {
		t.Fatalf("spec digest label is absent: %v", args)
	}
	rotated := spec
	rotated.Environment = map[string]string{
		"AGENTOPS_OPERATING_MODE": "ACTIVE",
		"SECRET":                  "second",
	}
	rotatedDigest, err := SpecDigest(
		rotated,
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	if err != nil {
		t.Fatal(err)
	}
	if rotatedDigest != digest {
		t.Fatal("credential rotation created a durable credential fingerprint")
	}
	rotated.Environment["AGENTOPS_OPERATING_MODE"] = "MONITOR_ONLY"
	nonSecretDigest, err := SpecDigest(
		rotated,
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	if err != nil {
		t.Fatal(err)
	}
	if nonSecretDigest == digest {
		t.Fatal("non-secret environment drift did not change the canonical digest")
	}
	imageDigest, err := SpecDigest(
		spec,
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	)
	if err != nil {
		t.Fatal(err)
	}
	if imageDigest == digest {
		t.Fatal("immutable image rotation did not change the canonical digest")
	}
}

func TestImageDigestParsesImmutableDescriptor(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `{"configuration":{"descriptor":{"digest":"sha256:` +
			strings.Repeat("a", 64) + `"}}}`,
	}}}
	runtime := NewAppleRuntimeForTest(fake)
	digest, err := runtime.ImageDigest(context.Background(), "control:test")
	if err != nil || digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("ImageDigest() = %q, %v", digest, err)
	}
}

// The environment subtraction is only as good as this parser, and it returns an
// empty set on any shape it does not recognize — which would silently disable
// the subtraction rather than fail. The payload here is the shape Apple
// Container 1.1.0 actually emits.
func TestImageEnvironmentParsesTheRealInspectShape(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-control:dev","variants":[{"config":{"config":{
			"Env":["PATH=/usr/local/bin:/usr/bin","HOME=/home/nonroot",
			"AGENTOPS_APP_ROOT=/app"],
			"Entrypoint":["node","dist/src/runner/cli.js"],
			"WorkingDir":"/app","User":"agentops"}}}]}]`,
	}}}
	runtime := NewAppleRuntimeForTest(fake)
	image, err := runtime.ImageConfiguration(
		context.Background(), "agentops-control:dev",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(image.Environment) != 3 ||
		image.Environment[0] != "PATH=/usr/local/bin:/usr/bin" ||
		image.Environment[2] != "AGENTOPS_APP_ROOT=/app" {
		t.Fatalf("image environment = %#v", image.Environment)
	}
	if image.WorkingDir != "/app" || image.User != "agentops" {
		t.Fatalf("image process context = %#v", image)
	}
	// Entrypoint plus Cmd is what the container actually runs.
	if process := image.Process(); len(process) != 2 ||
		process[0] != "node" || process[1] != "dist/src/runner/cli.js" {
		t.Fatalf("image process = %#v", image.Process())
	}
}

// Guessing between variants that declare different environments would silently
// change the replacement's environment.
func TestImageEnvironmentRefusesDisagreeingVariants(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"variants":[
			{"config":{"config":{"Env":["PATH=/a"]}}},
			{"config":{"config":{"Env":["PATH=/b"]}}}]}]`,
	}}}
	runtime := NewAppleRuntimeForTest(fake)
	if _, err := runtime.ImageConfiguration(
		context.Background(), "agentops-control:dev",
	); err == nil {
		t.Fatal("disagreeing variants were silently reconciled")
	}
}

// Teardown wants deleting an absent container to be success. The label sweep,
// which has just proven the container exists and owns it, needs the opposite:
// absence there means another actor is mutating the same reusable name.
func TestDeleteDistinguishesAbsenceFromSuccess(t *testing.T) {
	absent := func() *AppleRuntime {
		return NewAppleRuntimeForTest(&fakeRuntimeRunner{
			results: []CommandResult{{Status: 0, Stdout: `[]`}},
		})
	}
	err := absent().DeleteExisting(context.Background(), "agentops-runner")
	if !errors.Is(err, ErrContainerAbsent) {
		t.Fatalf("DeleteExisting() on an absent container = %v", err)
	}
	if err := absent().Delete(
		context.Background(), "agentops-runner",
	); err != nil {
		t.Fatalf("Delete() stopped being idempotent: %v", err)
	}
}

func TestBuildControlArgsAreExactLoopbackAndSecretsRedact(t *testing.T) {
	spec := ContainerSpec{
		Name: "agentops-control", Role: "control", Image: "control:test",
		Networks: []string{"agentops-internal", "default"}, Detach: true,
		ReadOnly: true, CapDropAll: true,
		Publish: []Publication{{
			HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 8080,
		}},
		Environment: map[string]string{"SECRET": "do-not-leak"},
	}
	args, _, err := buildContainerArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	rendered := strings.Join(args, " ")
	if !strings.Contains(rendered, "--publish 127.0.0.1:8080:8080") ||
		!strings.Contains(rendered, "--network agentops-internal --network default") {
		t.Fatalf("unexpected argv: %s", rendered)
	}
	if strings.Contains(rendered, "do-not-leak") ||
		!strings.Contains(rendered, "--env SECRET") {
		t.Fatalf("environment value leaked into argv: %s", rendered)
	}
	redacted := strings.Join(redactArgs(args), " ")
	if strings.Contains(redacted, "do-not-leak") {
		t.Fatalf("secret was not redacted: %s", redacted)
	}
}

func TestEnsureNetworkRejectsForeignResource(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-internal","configuration":{"mode":"host","labels":{}}}]`,
	}}}
	runtime := NewAppleRuntimeForTest(fake)
	if err := runtime.EnsureNetwork(context.Background(), "agentops-internal"); err == nil {
		t.Fatal("foreign network was accepted")
	}
}

func TestVolumeInitCanAddOnlyChownCapability(t *testing.T) {
	args, _, err := buildContainerArgs(ContainerSpec{
		Name:       "agentops-volume-init",
		Role:       "volume-init",
		Image:      "runner:test",
		Networks:   []string{"agentops-internal"},
		CapDropAll: true,
		CapAdd:     []string{"CAP_CHOWN"},
		Remove:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		strings.Join(args, " "),
		"--cap-drop ALL --cap-add CAP_CHOWN",
	) {
		t.Fatalf("unexpected argv: %s", strings.Join(args, " "))
	}
}

func TestEnsureNetworkAcceptsOwnedHostOnlyResource(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 0,
		Stdout: `[{"id":"agentops-internal","configuration":{"mode":"hostOnly","labels":{"com.mrbaron3.workflow.agentopsctl":"v1"}}}]`,
	}}}
	runtime := NewAppleRuntimeForTest(fake)
	if err := runtime.EnsureNetwork(context.Background(), "agentops-internal"); err != nil {
		t.Fatalf("owned host-only network was rejected: %v", err)
	}
}

func TestRuntimeErrorsRedactEnvironmentValues(t *testing.T) {
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 1, Stderr: "credential secret-value rejected",
	}}}
	runtime := NewAppleRuntimeForTest(fake)
	_, err := runtime.RunContainer(context.Background(), ContainerSpec{
		Name: "agentops-control", Role: "control", Image: "control:test",
		Networks: []string{"agentops-internal"}, Detach: true,
		Publish: []Publication{{
			HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 8080,
		}},
		Environment: map[string]string{"TOKEN": "secret-value"},
	})
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error was not safely redacted: %v", err)
	}
}

func TestCopyFileToContainerRedactsHostCredentialPathOnFailure(t *testing.T) {
	source := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(source, []byte("private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRuntimeRunner{results: []CommandResult{{
		Status: 1, Stderr: "copy rejected",
	}}}
	runtime := NewAppleRuntimeForTest(fake)
	err := runtime.CopyFileToContainer(
		context.Background(),
		"agentops-credential-init",
		source,
		"/credentials/codex/auth.json",
	)
	if err == nil || strings.Contains(err.Error(), source) {
		t.Fatalf("credential source path leaked: %v", err)
	}
	if len(fake.args) != 1 ||
		fake.args[0][0] != "exec" ||
		strings.Contains(strings.Join(fake.args[0], " "), source) {
		t.Fatalf("private stdin copy exposed the host source: %#v", fake.args)
	}
}
