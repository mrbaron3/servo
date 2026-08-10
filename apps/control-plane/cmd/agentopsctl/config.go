package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
	"github.com/mrbaron3/servo/apps/control-plane/internal/githubapp"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
	"github.com/mrbaron3/servo/apps/control-plane/internal/reporoot"
)

type config struct {
	Prefix                      string
	Network                     string
	PostgresVolume              string
	RunnerVolume                string
	TriageCredentialVolume      string
	RunnerCredentialVolume      string
	GitHubAppKeyVolume          string
	PostgresContainer           string
	ControlContainer            string
	GitHubBrokerContainer       string
	TriageContainer             string
	RunnerContainer             string
	PostgresImage               string
	ControlImage                string
	GitHubBrokerImage           string
	TriageImage                 string
	RunnerImage                 string
	ProjectRoot                 string
	ControlHostPort             int
	PostgresPassword            string
	NextPostgresPassword        string
	ControlDBPassword           string
	TriageDBPassword            string
	RunnerDBPassword            string
	ControlToken                string
	DashboardToken              string
	WebhookSecret               string
	ControlGitHubToken          string
	TriageGitHubToken           string
	RunnerGitHubToken           string
	GitHubAppID                 int64
	GitHubInstallationID        int64
	GitHubAppSlug               string
	GitHubAppOwner              string
	GitHubAppKeyPath            string
	TriageBrokerCapability      string
	RunnerBrokerCapability      string
	Provider                    string
	ProviderToken               string
	CodexAuthPath               string
	TriageReadyLabel            string
	TriageClaimedLabel          string
	TriageCandidateLabel        string
	TriageBlockedLabel          string
	TriageNeedsInfoLabel        string
	TriageContextPaths          string
	TriageModel                 string
	ReleaseConsumerRepository   string
	ReleaseConsumerRevision     string
	ReleaseEnvironmentKind      string
	ReleaseEnvironmentReference string
	ReleaseEnvironmentDigest    string
	ReleaseProviderDefaults     string
}

// resolveProjectRoot locates the repository without touching anything. It is
// separate from loadConfig because loading the configuration resolves broker
// capabilities and persists them, and a read-only command must be able to find
// the project without creating state on the host.
func resolveProjectRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("AGENTOPSCTL_PROJECT_ROOT"))
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root, err = reporoot.Find(cwd)
		if err != nil {
			return "", err
		}
	}
	return filepath.Abs(root)
}

func loadConfig() (config, error) {
	root, err := resolveProjectRoot()
	if err != nil {
		return config{}, err
	}
	prefix := environmentValue("AGENTOPSCTL_NAME_PREFIX", "agentops")
	port, err := strconv.Atoi(environmentValue("AGENTOPSCTL_CONTROL_HOST_PORT", "8080"))
	if err != nil || port < 1 || port > 65535 {
		return config{}, fmt.Errorf("AGENTOPSCTL_CONTROL_HOST_PORT must be 1..65535")
	}
	githubAppID, err := optionalPositiveInt64("AGENTOPS_GITHUB_APP_ID")
	if err != nil {
		return config{}, err
	}
	githubInstallationID, err := optionalPositiveInt64(
		"AGENTOPS_GITHUB_APP_INSTALLATION_ID",
	)
	if err != nil {
		return config{}, err
	}
	provider := strings.ToLower(environmentValue("AGENTOPS_RUNNER_PROVIDER", "codex"))
	var providerToken string
	var codexAuthPath string
	switch provider {
	case "codex":
		providerToken = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		codexAuthPath = strings.TrimSpace(os.Getenv("AGENTOPS_RUNNER_CODEX_AUTH_FILE"))
		if providerToken == "" && codexAuthPath == "" {
			if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
				codexAuthPath = filepath.Join(codexHome, "auth.json")
			} else if userHome, homeErr := os.UserHomeDir(); homeErr == nil {
				codexAuthPath = filepath.Join(userHome, ".codex", "auth.json")
			}
		}
	case "claude":
		providerToken = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	default:
		return config{}, fmt.Errorf("AGENTOPS_RUNNER_PROVIDER must be codex or claude")
	}
	// Capabilities are resolved rather than merely read: with no operator value
	// agentopsctl generates one and keeps it in its own private state, so
	// bootstrap asks for no manual secret while the capability stays independent
	// of every other credential. Resolution belongs here, on the path every
	// subcommand takes, because status compares a running topology against the
	// same values a start injected.
	triageCapability, runnerCapability, err := resolveBrokerCapabilities(
		prefix,
		strings.TrimSpace(os.Getenv("AGENTOPS_GITHUB_BROKER_TRIAGE_CAPABILITY")),
		strings.TrimSpace(os.Getenv("AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY")),
	)
	if err != nil {
		return config{}, err
	}
	return config{
		Prefix:         prefix,
		Network:        prefix + "-internal",
		PostgresVolume: prefix + "-postgres-data",
		RunnerVolume:   prefix + "-runner-workspace",
		// Apple Containerのnamed volumeは単一VMへの排他attachのため、
		// credential volumeはmountするroleごとに分ける（共有すると2つ目のattachが
		// VZErrorDomain Code=2で必ず失敗する）。
		TriageCredentialVolume: prefix + "-triage-credentials",
		RunnerCredentialVolume: prefix + "-runner-credentials",
		GitHubAppKeyVolume:     prefix + "-github-app-key",
		PostgresContainer:      prefix + "-postgres",
		ControlContainer:       prefix + "-control",
		GitHubBrokerContainer:  prefix + "-github-broker",
		TriageContainer:        prefix + "-triage",
		RunnerContainer:        prefix + "-runner",
		PostgresImage: environmentValue(
			"AGENTOPSCTL_POSTGRES_IMAGE",
			"agentops-postgres:dev",
		),
		ControlImage: environmentValue(
			"AGENTOPSCTL_CONTROL_IMAGE",
			"agentops-control:dev",
		),
		GitHubBrokerImage: environmentValue(
			"AGENTOPSCTL_GITHUB_BROKER_IMAGE",
			"agentops-github-broker:dev",
		),
		TriageImage: environmentValue(
			"AGENTOPSCTL_TRIAGE_IMAGE",
			"agentops-triage:dev",
		),
		RunnerImage: environmentValue(
			"AGENTOPSCTL_RUNNER_IMAGE",
			"agentops-runner:dev",
		),
		ProjectRoot:      root,
		ControlHostPort:  port,
		PostgresPassword: strings.TrimSpace(os.Getenv("AGENTOPS_POSTGRES_PASSWORD")),
		NextPostgresPassword: strings.TrimSpace(
			os.Getenv("AGENTOPS_NEXT_POSTGRES_PASSWORD"),
		),
		ControlDBPassword:    strings.TrimSpace(os.Getenv("AGENTOPS_CONTROL_DB_PASSWORD")),
		TriageDBPassword:     strings.TrimSpace(os.Getenv("AGENTOPS_TRIAGE_DB_PASSWORD")),
		RunnerDBPassword:     strings.TrimSpace(os.Getenv("AGENTOPS_RUNNER_DB_PASSWORD")),
		ControlToken:         strings.TrimSpace(os.Getenv("AGENTOPS_CONTROL_TOKEN")),
		DashboardToken:       strings.TrimSpace(os.Getenv("AGENTOPS_DASHBOARD_BOOTSTRAP_TOKEN")),
		WebhookSecret:        strings.TrimSpace(os.Getenv("AGENTOPS_GITHUB_WEBHOOK_SECRET")),
		ControlGitHubToken:   strings.TrimSpace(os.Getenv("AGENTOPS_CONTROL_GITHUB_TOKEN")),
		TriageGitHubToken:    strings.TrimSpace(os.Getenv("AGENTOPS_TRIAGE_GITHUB_TOKEN")),
		RunnerGitHubToken:    strings.TrimSpace(os.Getenv("AGENTOPS_RUNNER_GITHUB_TOKEN")),
		GitHubAppID:          githubAppID,
		GitHubInstallationID: githubInstallationID,
		GitHubAppSlug: strings.TrimSpace(
			os.Getenv("AGENTOPS_GITHUB_APP_SLUG"),
		),
		GitHubAppOwner: strings.TrimSpace(
			os.Getenv("AGENTOPS_GITHUB_APP_OWNER"),
		),
		GitHubAppKeyPath: strings.TrimSpace(
			os.Getenv("AGENTOPS_GITHUB_APP_PRIVATE_KEY_FILE"),
		),
		TriageBrokerCapability: triageCapability,
		RunnerBrokerCapability: runnerCapability,
		Provider:               provider,
		ProviderToken:          providerToken,
		CodexAuthPath:          codexAuthPath,
		TriageReadyLabel: strings.TrimSpace(
			environmentValue("AGENTOPS_TRIAGE_READY_LABEL", "ready"),
		),
		TriageClaimedLabel: strings.TrimSpace(
			environmentValue("AGENTOPS_TRIAGE_CLAIMED_LABEL", "agent-claimed"),
		),
		TriageCandidateLabel: strings.TrimSpace(
			environmentValue("AGENTOPS_TRIAGE_CANDIDATE_LABEL", "ready-candidate"),
		),
		TriageBlockedLabel: strings.TrimSpace(
			environmentValue("AGENTOPS_TRIAGE_BLOCKED_LABEL", "blocked"),
		),
		TriageNeedsInfoLabel: strings.TrimSpace(
			environmentValue("AGENTOPS_TRIAGE_NEEDS_INFO_LABEL", "needs-info"),
		),
		TriageContextPaths: strings.TrimSpace(
			os.Getenv("AGENTOPS_TRIAGE_CONTEXT_PATHS_JSON"),
		),
		TriageModel: strings.TrimSpace(os.Getenv("AGENTOPS_TRIAGE_MODEL")),
		ReleaseConsumerRepository: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_CONSUMER_REPOSITORY"),
		),
		ReleaseConsumerRevision: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_CONSUMER_REVISION"),
		),
		ReleaseEnvironmentKind: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_ENVIRONMENT_KIND"),
		),
		ReleaseEnvironmentReference: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_ENVIRONMENT_REFERENCE"),
		),
		ReleaseEnvironmentDigest: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_ENVIRONMENT_DIGEST"),
		),
		ReleaseProviderDefaults: strings.TrimSpace(
			os.Getenv("AGENTOPS_RELEASE_PROVIDER_DEFAULTS_JSON"),
		),
	}, nil
}

func (value config) addReleaseProvenance(environment map[string]string) {
	values := map[string]string{
		"AGENTOPS_RELEASE_CONSUMER_REPOSITORY":    value.ReleaseConsumerRepository,
		"AGENTOPS_RELEASE_CONSUMER_REVISION":      value.ReleaseConsumerRevision,
		"AGENTOPS_RELEASE_ENVIRONMENT_KIND":       value.ReleaseEnvironmentKind,
		"AGENTOPS_RELEASE_ENVIRONMENT_REFERENCE":  value.ReleaseEnvironmentReference,
		"AGENTOPS_RELEASE_ENVIRONMENT_DIGEST":     value.ReleaseEnvironmentDigest,
		"AGENTOPS_RELEASE_PROVIDER_DEFAULTS_JSON": value.ReleaseProviderDefaults,
	}
	for key, item := range values {
		if item != "" {
			environment[key] = item
		}
	}
}

func (value config) validateStart(mode lifecycle.Mode) error {
	if !resourceName(value.Prefix) {
		return fmt.Errorf("AGENTOPSCTL_NAME_PREFIX must be a safe named-resource prefix")
	}
	if value.ProjectRoot == "/" || !filepath.IsAbs(value.ProjectRoot) {
		return fmt.Errorf("AGENTOPSCTL_PROJECT_ROOT must be a specific absolute repository path")
	}
	if _, err := os.Stat(filepath.Join(value.ProjectRoot, "deploy", "Containerfile")); err != nil {
		return fmt.Errorf("AGENTOPSCTL_PROJECT_ROOT has no deploy/Containerfile: %w", err)
	}
	secrets := map[string]string{
		"AGENTOPS_POSTGRES_PASSWORD":         value.PostgresPassword,
		"AGENTOPS_CONTROL_DB_PASSWORD":       value.ControlDBPassword,
		"AGENTOPS_TRIAGE_DB_PASSWORD":        value.TriageDBPassword,
		"AGENTOPS_RUNNER_DB_PASSWORD":        value.RunnerDBPassword,
		"AGENTOPS_CONTROL_TOKEN":             value.ControlToken,
		"AGENTOPS_DASHBOARD_BOOTSTRAP_TOKEN": value.DashboardToken,
		"AGENTOPS_GITHUB_WEBHOOK_SECRET":     value.WebhookSecret,
	}
	for name, secret := range secrets {
		if len(secret) < 32 {
			return fmt.Errorf("%s must be at least 32 bytes", name)
		}
	}
	databaseCredentials := []string{
		value.PostgresPassword,
		value.ControlDBPassword,
		value.TriageDBPassword,
		value.RunnerDBPassword,
	}
	if !allDistinct(databaseCredentials) {
		return fmt.Errorf(
			"PostgreSQL admin, control, triage, and runner credentials must be distinct",
		)
	}
	if value.ControlGitHubToken != "" ||
		value.TriageGitHubToken != "" ||
		value.RunnerGitHubToken != "" {
		return fmt.Errorf(
			"manual GitHub token environment is forbidden; use the managed GitHub App broker",
		)
	}
	if err := value.validateGitHubApp(mode); err != nil {
		return err
	}
	if err := validateTriageLabels(value.triagePolicyLabels()); err != nil {
		return err
	}
	if err := validateTriageContextPaths(value.TriageContextPaths); err != nil {
		return err
	}
	if mode == lifecycle.ModeActive {
		if value.Provider == "codex" && len(value.ProviderToken) < 20 {
			if err := validateCodexAuthSource(value.CodexAuthPath); err != nil {
				return err
			}
		} else if len(value.ProviderToken) < 20 {
			return fmt.Errorf("ANTHROPIC_API_KEY is required for the isolated runner")
		}
	}
	return nil
}

// brokerCapabilityPattern is the shape both the Go broker and the TypeScript
// workers accept, so a capability that starts agentopsctl can never be one a
// worker rejects at runtime.
var brokerCapabilityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

func (value config) validateBrokerCapabilities(mode lifecycle.Mode) error {
	if !brokerCapabilityPattern.MatchString(value.TriageBrokerCapability) {
		return fmt.Errorf(
			"AGENTOPS_GITHUB_BROKER_TRIAGE_CAPABILITY must be 43..128 URL-safe characters",
		)
	}
	if mode == lifecycle.ModeActive &&
		!brokerCapabilityPattern.MatchString(value.RunnerBrokerCapability) {
		return fmt.Errorf(
			"AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY must be 43..128 URL-safe characters in ACTIVE mode",
		)
	}
	if value.TriageBrokerCapability == value.RunnerBrokerCapability {
		return fmt.Errorf(
			"triage and development GitHub broker capabilities must be distinct",
		)
	}
	// Holding a capability is the right to mint GitHub installation tokens for
	// that role. Sharing a value with any other credential would put that right
	// inside a trust domain the broker exists to keep it out of.
	for _, existing := range []string{
		value.PostgresPassword,
		value.ControlDBPassword,
		value.TriageDBPassword,
		value.RunnerDBPassword,
		value.ControlToken,
		value.DashboardToken,
		value.WebhookSecret,
	} {
		if existing == "" {
			continue
		}
		if existing == value.TriageBrokerCapability ||
			existing == value.RunnerBrokerCapability {
			return fmt.Errorf(
				"GitHub broker capabilities must not repeat another credential",
			)
		}
	}
	return nil
}

func (value config) validateGitHubApp(mode lifecycle.Mode) error {
	if value.GitHubAppID <= 0 || value.GitHubInstallationID <= 0 {
		return fmt.Errorf(
			"AGENTOPS_GITHUB_APP_ID and AGENTOPS_GITHUB_APP_INSTALLATION_ID are required",
		)
	}
	if !githubapp.ValidAppSlug(value.GitHubAppSlug) ||
		!control.ValidRepositoryIdentity(value.GitHubAppOwner+"/repository") {
		return fmt.Errorf(
			"AGENTOPS_GITHUB_APP_SLUG and AGENTOPS_GITHUB_APP_OWNER must be canonical",
		)
	}
	if err := value.validateBrokerCapabilities(mode); err != nil {
		return err
	}
	return validateGitHubAppKeySource(value.GitHubAppKeyPath)
}

func validateGitHubAppKeySource(source string) error {
	absolute, err := filepath.Abs(source)
	if err != nil || source == "" || absolute != source ||
		filepath.Ext(absolute) != ".pem" {
		return fmt.Errorf(
			"AGENTOPS_GITHUB_APP_PRIVATE_KEY_FILE must be an absolute .pem file",
		)
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 ||
		info.Size() < 1 || info.Size() > 64*1024 {
		return fmt.Errorf(
			"GitHub App private key must be a private regular file of at most 64 KiB",
		)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return fmt.Errorf("GitHub App private key is unavailable")
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(contents) > 64*1024 {
		return fmt.Errorf("GitHub App private key is unavailable")
	}
	if _, err := githubapp.ParseRSAPrivateKeyPEM(contents); err != nil {
		return fmt.Errorf("GitHub App private key is invalid")
	}
	return nil
}

// githubBrokerCapability returns the role's own broker capability. It is a
// first-class secret rather than something derived from another credential:
// whoever holds it can mint GitHub installation tokens for that role, so it
// must be revocable — and its holders auditable — independently of PostgreSQL.
func (value config) githubBrokerCapability(role string) string {
	if role == "runner" {
		return value.RunnerBrokerCapability
	}
	return value.TriageBrokerCapability
}

func (value config) usesCodexAuthFile() bool {
	return value.Provider == "codex" && len(value.ProviderToken) < 20
}

func (value config) providerAuth(mode lifecycle.Mode) string {
	if mode != lifecycle.ModeActive {
		return "none"
	}
	if value.usesCodexAuthFile() {
		return "codex-login"
	}
	return "api-key"
}

func (value config) usesCodexAuthFileFor(mode lifecycle.Mode) bool {
	return mode == lifecycle.ModeActive && value.usesCodexAuthFile()
}

func (value config) validatePostgresRotation() error {
	if len(value.PostgresPassword) < 32 {
		return fmt.Errorf("AGENTOPS_POSTGRES_PASSWORD must be at least 32 bytes")
	}
	if len(value.NextPostgresPassword) < 32 {
		return fmt.Errorf("AGENTOPS_NEXT_POSTGRES_PASSWORD must be at least 32 bytes")
	}
	if value.NextPostgresPassword == value.PostgresPassword ||
		value.NextPostgresPassword == value.ControlDBPassword ||
		value.NextPostgresPassword == value.TriageDBPassword ||
		value.NextPostgresPassword == value.RunnerDBPassword {
		return fmt.Errorf(
			"next PostgreSQL administrator credential must be distinct from current database credentials",
		)
	}
	return nil
}

func validateCodexAuthSource(source string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf(
			"OPENAI_API_KEY or AGENTOPS_RUNNER_CODEX_AUTH_FILE is required for ACTIVE codex runner",
		)
	}
	absolute, err := filepath.Abs(source)
	if err != nil || absolute != source || filepath.Base(absolute) != "auth.json" {
		return fmt.Errorf("Codex auth source must be an absolute auth.json file")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("Codex auth source is unavailable")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Codex auth source must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("Codex auth source must not be group/world accessible")
	}
	return nil
}

func (value config) adminDatabaseURL(host string) string {
	return databaseURL("postgres", value.PostgresPassword, host)
}

func (value config) controlDatabaseURL(host string) string {
	return databaseURL(
		"agentops_control_app",
		value.ControlDBPassword,
		host,
	)
}

func (value config) runnerDatabaseURL(host string) string {
	return databaseURL("agentops_runner", value.RunnerDBPassword, host)
}

func (value config) triageDatabaseURL(host string) string {
	return databaseURL("agentops_triage", value.TriageDBPassword, host)
}

func (value config) triagePolicyLabels() []string {
	defaults := []string{
		"ready",
		"agent-claimed",
		"ready-candidate",
		"blocked",
		"needs-info",
	}
	configured := []string{
		value.TriageReadyLabel,
		value.TriageClaimedLabel,
		value.TriageCandidateLabel,
		value.TriageBlockedLabel,
		value.TriageNeedsInfoLabel,
	}
	for index := range configured {
		if configured[index] == "" {
			configured[index] = defaults[index]
		}
	}
	return configured
}

func databaseURL(user, password, host string) string {
	return (&url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(user, password),
		Host:   host + ":5432",
		Path:   "/agentops",
	}).String()
}

func environmentValue(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func optionalPositiveInt64(name string) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func resourceName(value string) bool {
	if value == "" || len(value) > 80 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			(index > 0 && (character == '-' || character == '_' || character == '.')) {
			continue
		}
		return false
	}
	return true
}

func allDistinct(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validateTriageLabels(labels []string) error {
	if len(labels) != 5 || !allDistinct(labels) {
		return fmt.Errorf("triage labels must be five distinct bounded plain-text values")
	}
	for _, label := range labels {
		if len(label) < 1 || len(label) > 50 {
			return fmt.Errorf("triage labels must be five distinct bounded plain-text values")
		}
		for index, character := range label {
			if (character >= 'a' && character <= 'z') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') ||
				(index > 0 && strings.ContainsRune("._:/ -", character)) {
				continue
			}
			return fmt.Errorf("triage labels must be five distinct bounded plain-text values")
		}
	}
	return nil
}

func validateTriageContextPaths(raw string) error {
	if raw == "" {
		return nil
	}
	var paths []string
	if err := json.Unmarshal([]byte(raw), &paths); err != nil ||
		len(paths) < 1 || len(paths) > 16 {
		return fmt.Errorf(
			"AGENTOPS_TRIAGE_CONTEXT_PATHS_JSON must contain 1..16 repository-relative paths",
		)
	}
	for _, candidate := range paths {
		if candidate == "" || len(candidate) > 200 ||
			strings.HasPrefix(candidate, "/") {
			return fmt.Errorf(
				"AGENTOPS_TRIAGE_CONTEXT_PATHS_JSON must contain 1..16 repository-relative paths",
			)
		}
		for _, segment := range strings.Split(candidate, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return fmt.Errorf(
					"AGENTOPS_TRIAGE_CONTEXT_PATHS_JSON must contain 1..16 repository-relative paths",
				)
			}
			for _, character := range segment {
				if (character >= 'a' && character <= 'z') ||
					(character >= 'A' && character <= 'Z') ||
					(character >= '0' && character <= '9') ||
					strings.ContainsRune("._-", character) {
					continue
				}
				return fmt.Errorf(
					"AGENTOPS_TRIAGE_CONTEXT_PATHS_JSON must contain 1..16 repository-relative paths",
				)
			}
		}
	}
	return nil
}
