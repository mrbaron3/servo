package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

func TestDatabaseURLsEscapeCredentials(t *testing.T) {
	value := config{
		PostgresContainer: "agentops-postgres",
		PostgresPassword:  "admin:/?#[]@ password",
	}
	url := value.adminDatabaseURL("agentops-postgres")
	if strings.Contains(url, " password") || !strings.Contains(url, "agentops-postgres:5432") {
		t.Fatalf("unsafe database URL = %q", url)
	}
}

func TestGitHubRepositoryFromRemoteAcceptsOnlyCanonicalGitHubCoordinates(t *testing.T) {
	for _, remote := range []string{
		"git@github.com:mrbaron3/servo.git",
		"ssh://git@github.com/mrbaron3/servo.git",
		"https://github.com/mrbaron3/servo",
	} {
		repository, err := githubRepositoryFromRemote(remote)
		if err != nil || repository != "mrbaron3/servo" {
			t.Fatalf("remote %q resolved to %q: %v", remote, repository, err)
		}
	}
	for _, remote := range []string{
		"https://example.com/mrbaron3/servo.git",
		"git@github.com:../servo.git",
		"github.com/mrbaron3/servo",
	} {
		if repository, err := githubRepositoryFromRemote(remote); err == nil {
			t.Fatalf("unsafe remote %q resolved to %q", remote, repository)
		}
	}
}

func TestExactLoopbackPublication(t *testing.T) {
	actual := &lifecycle.ContainerActual{}
	actual.Configuration.PublishedPorts = []map[string]any{{
		"hostAddress":   "127.0.0.1",
		"hostPort":      float64(8080),
		"containerPort": float64(8080),
		"count":         float64(1),
		"proto":         "tcp",
	}}
	if !exactLoopbackPublication(actual, 8080) {
		t.Fatal("exact loopback publication was rejected")
	}
	actual.Configuration.PublishedPorts[0]["hostAddress"] = "0.0.0.0"
	if exactLoopbackPublication(actual, 8080) {
		t.Fatal("wildcard publication was accepted")
	}
}

func TestValidateRunnerActualRequiresFullHardenedTopology(t *testing.T) {
	cfg := config{
		Network:                "agentops-internal",
		RunnerContainer:        "agentops-runner",
		RunnerImage:            "agentops-runner:dev",
		RunnerVolume:           "agentops-runner-workspace",
		RunnerCredentialVolume: "agentops-runner-credentials",
		Provider:               "codex",
		CodexAuthPath:          "/operator/.codex/auth.json",
	}
	actual := &lifecycle.ContainerActual{ID: cfg.RunnerContainer}
	actual.Status.State = "running"
	actual.Configuration.Labels = map[string]string{
		"com.mrbaron3.servo.agentopsctl": "v1",
		"com.mrbaron3.servo.role":        "runner",
	}
	actual.Configuration.Image.Reference = "docker.io/library/agentops-runner:dev"
	actual.Configuration.Networks = append(
		actual.Configuration.Networks,
		lifecycle.ContainerNetworkAttachment{Network: cfg.Network},
	)
	actual.Configuration.ReadOnly = true
	actual.Configuration.CapDrop = []string{"ALL"}
	actual.Configuration.InitProcess.User.ID.UID = 65532
	for destination, kind := range map[string]string{
		"/tmp":                      "tmpfs",
		"/home/agentops":            "tmpfs",
		"/workspace":                cfg.RunnerVolume,
		"/run/agentops-credentials": cfg.RunnerCredentialVolume,
	} {
		mount := lifecycle.ContainerMount{Destination: destination}
		if kind == "tmpfs" {
			mount.Type = map[string]any{"tmpfs": map[string]any{}}
		} else {
			mount.Type = map[string]any{
				"volume": map[string]any{"name": kind},
			}
		}
		actual.Configuration.Mounts = append(actual.Configuration.Mounts, mount)
	}
	if err := validateRunnerActual(actual, cfg, lifecycle.ModeActive); err != nil {
		t.Fatalf("valid runner topology rejected: %v", err)
	}
	if err := validateRunnerActual(actual, cfg, lifecycle.ModeDraining); err != nil {
		t.Fatalf("draining runner lost its active credential boundary: %v", err)
	}
	actual.Configuration.CapAdd = []string{"CAP_SYS_ADMIN"}
	if err := validateRunnerActual(actual, cfg, lifecycle.ModeActive); err == nil {
		t.Fatal("runner with added capability was accepted")
	}
}

func TestValidateTriageActualHasNoWorkspaceOrDevelopmentImage(t *testing.T) {
	cfg := config{
		Network:         "agentops-internal",
		TriageContainer: "agentops-triage",
		TriageImage:     "agentops-triage:dev",
	}
	actual := &lifecycle.ContainerActual{ID: cfg.TriageContainer}
	actual.Status.State = "running"
	actual.Configuration.Labels = map[string]string{
		"com.mrbaron3.servo.agentopsctl": "v1",
		"com.mrbaron3.servo.role":        "triage",
	}
	actual.Configuration.Image.Reference = "docker.io/library/agentops-triage:dev"
	actual.Configuration.Networks = append(
		actual.Configuration.Networks,
		lifecycle.ContainerNetworkAttachment{Network: cfg.Network},
	)
	actual.Configuration.ReadOnly = true
	actual.Configuration.CapDrop = []string{"ALL"}
	actual.Configuration.InitProcess.User.ID.UID = 65532
	for _, destination := range []string{"/tmp", "/home/agentops"} {
		actual.Configuration.Mounts = append(actual.Configuration.Mounts, lifecycle.ContainerMount{
			Destination: destination,
			Type:        map[string]any{"tmpfs": map[string]any{}},
		})
	}
	if err := validateTriageActual(
		actual,
		cfg,
		lifecycle.ModeMonitorOnly,
	); err != nil {
		t.Fatalf("valid triage topology rejected: %v", err)
	}
	actual.Configuration.Mounts = append(actual.Configuration.Mounts, lifecycle.ContainerMount{
		Destination: "/workspace",
		Type: map[string]any{
			"volume": map[string]any{"name": "development-workspace"},
		},
	})
	if err := validateTriageActual(
		actual,
		cfg,
		lifecycle.ModeMonitorOnly,
	); err == nil {
		t.Fatal("triage with a development workspace was accepted")
	}
}

func TestMonitorOnlyRequiresNoProviderCredentialOrCredentialMount(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "deploy", "Containerfile"),
		[]byte("FROM scratch\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	value := config{
		Prefix: "agentops", ProjectRoot: root,
		PostgresPassword:  strings.Repeat("a", 32),
		ControlDBPassword: strings.Repeat("b", 32),
		TriageDBPassword:  strings.Repeat("c", 32),
		RunnerDBPassword:  strings.Repeat("d", 32),
		ControlToken:      strings.Repeat("e", 32),
		DashboardToken:    strings.Repeat("f", 32),
		WebhookSecret:     strings.Repeat("g", 32),
		Provider:          "codex",
	}
	configureTestGitHubApp(t, &value, "acme")
	if err := value.validateStart(lifecycle.ModeMonitorOnly); err != nil {
		t.Fatalf("provider-free MONITOR_ONLY was rejected: %v", err)
	}
	if value.providerAuth(lifecycle.ModeMonitorOnly) != "none" ||
		value.usesCodexAuthFileFor(lifecycle.ModeMonitorOnly) {
		t.Fatal("MONITOR_ONLY selected a provider credential boundary")
	}
}

func TestStartRejectsEveryControlGitHubCredential(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "deploy", "Containerfile"),
		[]byte("FROM scratch\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	value := config{
		Prefix: "agentops", ProjectRoot: root,
		PostgresPassword:   strings.Repeat("a", 32),
		ControlDBPassword:  strings.Repeat("b", 32),
		TriageDBPassword:   strings.Repeat("c", 32),
		RunnerDBPassword:   strings.Repeat("d", 32),
		ControlToken:       strings.Repeat("e", 32),
		DashboardToken:     strings.Repeat("f", 32),
		WebhookSecret:      strings.Repeat("g", 32),
		ControlGitHubToken: strings.Repeat("h", 32),
		TriageGitHubToken:  strings.Repeat("i", 32),
		RunnerGitHubToken:  strings.Repeat("j", 32),
		Provider:           "codex", ProviderToken: strings.Repeat("k", 32),
	}
	if err := value.validateStart(lifecycle.ModeActive); err == nil ||
		!strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("control GitHub credential was accepted: %v", err)
	}
}

func TestActiveAllowsCredentialFreeControlWithPrivateBrokerAndCodexLogin(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "deploy", "Containerfile"),
		[]byte("FROM scratch\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(root, "auth.json")
	if err := os.WriteFile(auth, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := config{
		Prefix: "agentops", ProjectRoot: root,
		PostgresPassword:  strings.Repeat("a", 32),
		ControlDBPassword: strings.Repeat("b", 32),
		TriageDBPassword:  strings.Repeat("c", 32),
		RunnerDBPassword:  strings.Repeat("d", 32),
		ControlToken:      strings.Repeat("e", 32),
		DashboardToken:    strings.Repeat("f", 32),
		WebhookSecret:     strings.Repeat("g", 32),
		Provider:          "codex",
		CodexAuthPath:     auth,
	}
	configureTestGitHubApp(t, &value, "sample")
	if err := value.validateStart(lifecycle.ModeActive); err != nil {
		t.Fatalf("credential-free-control/private-runner boundary was rejected: %v", err)
	}
	if !value.usesCodexAuthFile() {
		t.Fatal("Codex login file mode was not selected")
	}
}

func TestCodexAuthSourceRejectsPermissiveOrNonCanonicalFile(t *testing.T) {
	root := t.TempDir()
	unsafe := filepath.Join(root, "auth.json")
	if err := os.WriteFile(unsafe, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateCodexAuthSource(unsafe); err == nil {
		t.Fatal("group/world-readable Codex auth was accepted")
	}
	safe := filepath.Join(root, "not-auth.json")
	if err := os.WriteFile(safe, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCodexAuthSource(safe); err == nil {
		t.Fatal("non-canonical Codex auth filename was accepted")
	}
	missing := filepath.Join(root, "missing", "auth.json")
	err := validateCodexAuthSource(missing)
	if err == nil || strings.Contains(err.Error(), root) {
		t.Fatalf("missing Codex auth leaked its host path: %v", err)
	}
}

func TestPostgresRotationRequiresDistinctCurrentAndNextCredentials(t *testing.T) {
	value := config{
		PostgresPassword:     strings.Repeat("a", 32),
		NextPostgresPassword: strings.Repeat("b", 32),
		ControlDBPassword:    strings.Repeat("c", 32),
		TriageDBPassword:     strings.Repeat("d", 32),
		RunnerDBPassword:     strings.Repeat("e", 32),
	}
	if err := value.validatePostgresRotation(); err != nil {
		t.Fatalf("valid PostgreSQL credential rotation rejected: %v", err)
	}
	value.NextPostgresPassword = value.ControlDBPassword
	if err := value.validatePostgresRotation(); err == nil {
		t.Fatal("database role credential reuse was accepted for admin rotation")
	}
}

func TestTriagePolicyConfigurationIsBoundedAndRepositoryRelative(t *testing.T) {
	if err := validateTriageLabels([]string{
		"human-approved",
		"automation-owned",
		"candidate",
		"dependency-blocked",
		"product-input",
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateTriageContextPaths(
		`["README.md","docs/ROADMAP.md","architecture/NORTH_STAR.md"]`,
	); err != nil {
		t.Fatal(err)
	}
	if err := validateTriageLabels([]string{
		"same", "same", "candidate", "blocked", "needs-info",
	}); err == nil {
		t.Fatal("duplicate triage labels were accepted")
	}
	for _, raw := range []string{
		`["../secret"]`,
		`["/absolute/path"]`,
		`{"path":"README.md"}`,
	} {
		if err := validateTriageContextPaths(raw); err == nil {
			t.Fatalf("unsafe context paths accepted: %s", raw)
		}
	}
}

func configureTestGitHubApp(
	t *testing.T,
	value *config,
	owner string,
) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "github-app.pem")
	contents := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	value.GitHubAppID = 42
	value.GitHubInstallationID = 99
	value.GitHubAppSlug = "agentops-test"
	value.GitHubAppOwner = owner
	value.GitHubAppKeyPath = path
	value.TriageBrokerCapability = strings.Repeat("t", 43)
	value.RunnerBrokerCapability = strings.Repeat("r", 43)
}

// A capability is the right to mint GitHub installation tokens for one role, so
// it may not be reachable from another credential's blast radius.
func TestBrokerCapabilitiesAreIndependentRevocableSecrets(t *testing.T) {
	base := func(t *testing.T) config {
		t.Helper()
		value := config{
			PostgresPassword:  strings.Repeat("a", 32),
			ControlDBPassword: strings.Repeat("b", 32),
			TriageDBPassword:  strings.Repeat("c", 32),
			RunnerDBPassword:  strings.Repeat("d", 32),
			ControlToken:      strings.Repeat("e", 32),
			DashboardToken:    strings.Repeat("f", 32),
			WebhookSecret:     strings.Repeat("g", 32),
		}
		configureTestGitHubApp(t, &value, "sample")
		return value
	}
	if err := base(t).validateBrokerCapabilities(
		lifecycle.ModeActive,
	); err != nil {
		t.Fatalf("independent capabilities were rejected: %v", err)
	}
	for name, mutate := range map[string]func(*config){
		"missing triage capability": func(value *config) {
			value.TriageBrokerCapability = ""
		},
		"short triage capability": func(value *config) {
			value.TriageBrokerCapability = strings.Repeat("t", 42)
		},
		"non-URL-safe triage capability": func(value *config) {
			value.TriageBrokerCapability = strings.Repeat("t", 42) + "="
		},
		"missing runner capability": func(value *config) {
			value.RunnerBrokerCapability = ""
		},
		"shared capability": func(value *config) {
			value.RunnerBrokerCapability = value.TriageBrokerCapability
		},
		"capability reused from a database credential": func(value *config) {
			value.RunnerDBPassword = value.RunnerBrokerCapability
		},
		"capability reused from the control token": func(value *config) {
			value.ControlToken = value.TriageBrokerCapability
		},
	} {
		value := base(t)
		mutate(&value)
		if err := value.validateBrokerCapabilities(
			lifecycle.ModeActive,
		); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
