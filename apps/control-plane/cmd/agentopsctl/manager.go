package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/control"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

type manager struct {
	config             config
	runtime            *lifecycle.AppleRuntime
	dashboardReachable func(string, int, time.Duration) bool
	openDashboard      func(context.Context, string) error
}

const (
	ProviderProbeTimeout          = 45 * time.Second
	CredentialSeedReadyTimeout    = 20 * time.Second
	ContainerReadyPollInterval    = 250 * time.Millisecond
	CredentialInitializerLifetime = 10 * time.Minute
	RunnerReadinessLogLines       = 100
	DashboardBootstrapLogLines    = 500
)

type mutationReceipt struct {
	Mutated bool
}

func newManager(config config, runtime *lifecycle.AppleRuntime) *manager {
	return &manager{
		config:             config,
		runtime:            runtime,
		dashboardReachable: tcpReachable,
		openDashboard: func(ctx context.Context, target string) error {
			return exec.CommandContext(ctx, "open", target).Run()
		},
	}
}

func (manager *manager) Start(
	ctx context.Context,
	mode lifecycle.Mode,
	build bool,
	requestID string,
) (resultErr error) {
	if err := manager.prepareReleaseProvenance(ctx); err != nil {
		return err
	}
	if err := manager.config.validateStart(mode); err != nil {
		return err
	}
	if !build {
		return fmt.Errorf(
			"start requires --build so the image is bound to the exact clean source HEAD",
		)
	}
	if err := manager.ensureRuntime(ctx); err != nil {
		return err
	}
	if manager.runtime.ImageExists(ctx, manager.config.ControlImage) {
		postgres, err := manager.runtime.Container(
			ctx,
			manager.config.PostgresContainer,
		)
		if err != nil {
			return err
		}
		if postgres != nil && postgres.Status.State == "running" {
			current, statusErr := manager.databaseStatus(ctx)
			if statusErr == nil && (current.State.Mode == lifecycle.ModeActive ||
				(current.State.Mode == lifecycle.ModeDraining &&
					(current.ActiveLeases > 0 || current.InFlightAttempts > 0))) {
				return fmt.Errorf(
					"running work must be drained before image build; run agentopsctl drain first",
				)
			}
		}
	}
	if build || !manager.runtime.ImageExists(ctx, manager.config.PostgresImage) {
		if err := manager.runtime.BuildImage(
			ctx,
			manager.config.PostgresImage,
			"postgres",
			filepath.Join(manager.config.ProjectRoot, "deploy", "Containerfile"),
			manager.config.ProjectRoot,
		); err != nil {
			return err
		}
	}
	if build || !manager.runtime.ImageExists(ctx, manager.config.ControlImage) {
		if err := manager.runtime.BuildImage(
			ctx,
			manager.config.ControlImage,
			"control",
			filepath.Join(manager.config.ProjectRoot, "deploy", "Containerfile"),
			manager.config.ProjectRoot,
		); err != nil {
			return err
		}
	}
	if build || !manager.runtime.ImageExists(
		ctx,
		manager.config.GitHubBrokerImage,
	) {
		if err := manager.runtime.BuildImage(
			ctx,
			manager.config.GitHubBrokerImage,
			"github-broker",
			filepath.Join(manager.config.ProjectRoot, "deploy", "Containerfile"),
			manager.config.ProjectRoot,
		); err != nil {
			return err
		}
	}
	if build || !manager.runtime.ImageExists(ctx, manager.config.TriageImage) {
		if err := manager.runtime.BuildImage(
			ctx,
			manager.config.TriageImage,
			"triage",
			filepath.Join(manager.config.ProjectRoot, "deploy", "Containerfile"),
			manager.config.ProjectRoot,
		); err != nil {
			return err
		}
	}
	if build || !manager.runtime.ImageExists(ctx, manager.config.RunnerImage) {
		if err := manager.runtime.BuildImage(
			ctx,
			manager.config.RunnerImage,
			"runner",
			filepath.Join(manager.config.ProjectRoot, "deploy", "Containerfile"),
			manager.config.ProjectRoot,
		); err != nil {
			return err
		}
	}
	if err := manager.prepareBuiltReleaseProvenance(ctx); err != nil {
		return err
	}
	if err := manager.runtime.EnsureNetwork(ctx, manager.config.Network); err != nil {
		return err
	}
	if err := manager.runtime.EnsureVolume(ctx, manager.config.PostgresVolume); err != nil {
		return err
	}
	if err := manager.runtime.EnsureVolume(ctx, manager.config.RunnerVolume); err != nil {
		return err
	}
	if err := manager.runtime.EnsureVolume(
		ctx,
		manager.config.GitHubAppKeyVolume,
	); err != nil {
		return err
	}
	if manager.config.usesCodexAuthFileFor(mode) {
		if err := manager.runtime.EnsureVolume(
			ctx,
			manager.config.TriageCredentialVolume,
		); err != nil {
			return err
		}
		if err := manager.runtime.EnsureVolume(
			ctx,
			manager.config.RunnerCredentialVolume,
		); err != nil {
			return err
		}
	}
	postgresStarted := false
	controlChanged := false
	githubBrokerChanged := false
	triageChanged := false
	runnerChanged := false
	var initialMode *lifecycle.Mode
	defer func() {
		if resultErr != nil {
			_ = manager.recordFailure(
				context.Background(),
				"start",
				resultErr.Error(),
				false,
			)
			_ = manager.compensateStart(
				context.Background(),
				postgresStarted,
				controlChanged,
				githubBrokerChanged,
				triageChanged,
				runnerChanged,
				initialMode,
				requestID,
			)
		}
	}()
	started, err := manager.ensurePostgres(ctx)
	if err != nil {
		return err
	}
	postgresStarted = started
	if err := manager.migrateAndBootstrap(ctx); err != nil {
		return err
	}
	persisted, err := manager.databaseStatus(ctx)
	if err != nil {
		return err
	}
	startingMode := persisted.State.Mode
	initialMode = &startingMode
	if err := manager.validateExistingTopology(ctx); err != nil {
		return err
	}
	if persisted.State.Mode == lifecycle.ModeActive {
		actualControl, err := manager.runtime.Container(ctx, manager.config.ControlContainer)
		if err != nil {
			return err
		}
		actualRunner, err := manager.runtime.Container(ctx, manager.config.RunnerContainer)
		if err != nil {
			return err
		}
		actualTriage, err := manager.runtime.Container(ctx, manager.config.TriageContainer)
		if err != nil {
			return err
		}
		actualGitHubBroker, err := manager.runtime.Container(
			ctx,
			manager.config.GitHubBrokerContainer,
		)
		if err != nil {
			return err
		}
		if mode == lifecycle.ModeActive &&
			!build &&
			actualControl != nil && actualControl.Status.State == "running" &&
			actualGitHubBroker != nil &&
			actualGitHubBroker.Status.State == "running" &&
			actualTriage != nil && actualTriage.Status.State == "running" &&
			actualRunner != nil && actualRunner.Status.State == "running" {
			if err := manager.verifyPublishedSurface(
				ctx,
				true,
				true,
				true,
				lifecycle.ModeActive,
			); err == nil {
				return nil
			}
			// A mutable tag or credential/configuration rotation invalidates
			// the canonical spec label. Enter the durable drain path before
			// replacing either process.
		}
		if _, err := manager.transition(
			ctx,
			lifecycle.ModeDraining,
			requestID+":recover-draining",
			time.Now().UTC().Add(10*time.Minute),
		); err != nil {
			return fmt.Errorf("recover persisted ACTIVE mode: %w", err)
		}
		persisted, err = manager.databaseStatus(ctx)
		if err != nil {
			return err
		}
	}
	if persisted.State.Mode == lifecycle.ModeDraining {
		persisted, err = manager.recoverDraining(ctx, persisted)
		if err != nil {
			return err
		}
		if _, err := manager.transition(
			ctx,
			lifecycle.ModeMonitorOnly,
			requestID+":recover-monitor",
			time.Time{},
		); err != nil {
			return err
		}
		persisted, err = manager.databaseStatus(ctx)
		if err != nil {
			return err
		}
	}
	if persisted.State.Mode == lifecycle.ModeActive && mode == lifecycle.ModeMonitorOnly {
		return fmt.Errorf("ACTIVE must drain before MONITOR_ONLY")
	}
	if persisted.State.Mode == lifecycle.ModeOff ||
		persisted.State.Mode == lifecycle.ModeMonitorOnly {
		receipt, err := manager.replaceControl(ctx, mode)
		controlChanged = controlChanged || receipt.Mutated
		if err != nil {
			return err
		}
		if persisted.State.Mode == lifecycle.ModeOff {
			if _, err := manager.transition(
				ctx,
				lifecycle.ModeMonitorOnly,
				requestID+":monitor",
				time.Time{},
			); err != nil {
				return err
			}
		}
	}
	receipt, err := manager.replaceGitHubBroker(ctx, mode)
	githubBrokerChanged = githubBrokerChanged || receipt.Mutated
	if err != nil {
		return err
	}
	receipt, err = manager.replaceTriage(ctx, mode)
	triageChanged = triageChanged || receipt.Mutated
	if err != nil {
		return err
	}
	if mode == lifecycle.ModeActive {
		receipt, err = manager.replaceRunner(ctx, mode)
		runnerChanged = runnerChanged || receipt.Mutated
		if err != nil {
			return err
		}
	} else {
		receipt, err = manager.removeRunner(ctx)
		runnerChanged = runnerChanged || receipt.Mutated
		if err != nil {
			return err
		}
	}
	if mode == lifecycle.ModeActive {
		current, err := manager.databaseStatus(ctx)
		if err != nil {
			return err
		}
		if current.State.Mode == lifecycle.ModeMonitorOnly {
			if _, err := manager.transition(
				ctx,
				lifecycle.ModeActive,
				requestID+":active",
				time.Time{},
			); err != nil {
				return err
			}
		}
	}
	return manager.verifyPublishedSurface(
		ctx,
		true,
		true,
		mode == lifecycle.ModeActive,
		mode,
	)
}

// DeployActive is the self-update boundary. It validates the exact clean
// source before touching lifecycle state, lets every current lease finish, and
// only then builds/migrates/replaces the topology from that revision.
func (manager *manager) DeployActive(
	ctx context.Context,
	timeout time.Duration,
	requestID string,
) error {
	if err := manager.prepareReleaseProvenance(ctx); err != nil {
		return err
	}
	if err := manager.config.validateStart(lifecycle.ModeActive); err != nil {
		return err
	}
	if err := manager.ensureRuntime(ctx); err != nil {
		return err
	}
	postgres, err := manager.runtime.Container(ctx, manager.config.PostgresContainer)
	if err != nil {
		return err
	}
	if postgres != nil && postgres.Status.State == "running" &&
		manager.runtime.ImageExists(ctx, manager.config.ControlImage) {
		status, err := manager.databaseStatus(ctx)
		if err != nil {
			return err
		}
		if status.State.Mode == lifecycle.ModeActive ||
			status.State.Mode == lifecycle.ModeDraining {
			if err := manager.Drain(ctx, timeout, requestID+":drain"); err != nil {
				return err
			}
		}
	}
	return manager.Start(ctx, lifecycle.ModeActive, true, requestID+":promote")
}

func (manager *manager) prepareReleaseProvenance(ctx context.Context) error {
	git := func(args ...string) (string, error) {
		command := exec.CommandContext(
			ctx,
			"git",
			append([]string{"-C", manager.config.ProjectRoot}, args...)...,
		)
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("read deployment source identity: %w", err)
		}
		return strings.TrimSpace(string(output)), nil
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(head) {
		return fmt.Errorf("deployment source HEAD is unavailable or invalid")
	}
	dirty, err := git("status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if dirty != "" {
		return fmt.Errorf(
			"deployment source has uncommitted changes; commit the exact release before building",
		)
	}
	remote, err := git("remote", "get-url", "origin")
	if err != nil {
		return err
	}
	repository, err := githubRepositoryFromRemote(remote)
	if err != nil {
		return err
	}
	if repository != "mrbaron3/servo" {
		return fmt.Errorf(
			"deployment source repository must be mrbaron3/servo, got %s",
			repository,
		)
	}
	if configured := manager.config.ReleaseConsumerRepository; configured != "" && configured != repository {
		return fmt.Errorf(
			"AGENTOPS_RELEASE_CONSUMER_REPOSITORY does not match origin",
		)
	}
	if configured := manager.config.ReleaseConsumerRevision; configured != "" && configured != head {
		return fmt.Errorf(
			"AGENTOPS_RELEASE_CONSUMER_REVISION does not match deployment source HEAD",
		)
	}
	manager.config.ReleaseConsumerRepository = repository
	manager.config.ReleaseConsumerRevision = head
	return nil
}

func (manager *manager) prepareBuiltReleaseProvenance(ctx context.Context) error {
	digest, err := manager.runtime.ImageDigest(ctx, manager.config.RunnerImage)
	if err != nil {
		return fmt.Errorf("read built runner image provenance: %w", err)
	}
	providerDefaults, err := builtProviderDefaults(
		manager.config.ProjectRoot,
		manager.config.Provider,
	)
	if err != nil {
		return err
	}
	revision := manager.config.ReleaseConsumerRevision
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(revision) {
		return fmt.Errorf("built release provenance requires an exact consumer revision")
	}
	manager.config.ReleaseEnvironmentKind = "container"
	manager.config.ReleaseEnvironmentReference = fmt.Sprintf(
		"%s@source-%s",
		manager.config.RunnerImage,
		revision[:12],
	)
	manager.config.ReleaseEnvironmentDigest = digest
	manager.config.ReleaseProviderDefaults = providerDefaults
	return nil
}

func builtProviderDefaults(projectRoot, provider string) (string, error) {
	providerRoot := filepath.Join(projectRoot, "deploy", "tools", "provider-cli")
	manifestPath := filepath.Join(providerRoot, "package.json")
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read provider manifest: %w", err)
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		return "", fmt.Errorf("parse provider manifest: %w", err)
	}
	packageName := ""
	referencePrefix := ""
	switch provider {
	case "codex":
		packageName = "@openai/codex"
		referencePrefix = "codex-cli-"
	case "claude":
		packageName = "@anthropic-ai/claude-code"
		referencePrefix = "claude-code-"
	default:
		return "", fmt.Errorf("unsupported provider provenance %q", provider)
	}
	version := strings.TrimSpace(manifest.Dependencies[packageName])
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`).MatchString(version) {
		return "", fmt.Errorf("provider manifest must pin %s to an exact version", packageName)
	}
	lockBody, err := os.ReadFile(
		filepath.Join(providerRoot, "package-lock.json"),
	)
	if err != nil {
		return "", fmt.Errorf("read provider lockfile: %w", err)
	}
	lockDigest := sha256.Sum256(lockBody)
	value := []map[string]string{{
		"provider":       provider,
		"reference":      referencePrefix + version + ":provider-default",
		"resolverDigest": fmt.Sprintf("sha256:%x", lockDigest),
	}}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode provider provenance: %w", err)
	}
	return string(encoded), nil
}

func githubRepositoryFromRemote(remote string) (string, error) {
	value := strings.TrimSpace(remote)
	for _, prefix := range []string{
		"git@github.com:", "ssh://git@github.com/", "https://github.com/",
	} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			value = strings.TrimSuffix(value, ".git")
			value = strings.ToLower(strings.Trim(value, "/"))
			if regexp.MustCompile(
				`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9_.-]{1,100}$`,
			).MatchString(value) {
				return value, nil
			}
		}
	}
	return "", fmt.Errorf("origin must be a canonical github.com repository URL")
}

func (manager *manager) Drain(
	ctx context.Context,
	timeout time.Duration,
	requestID string,
) error {
	if err := manager.ensureRuntime(ctx); err != nil {
		return err
	}
	status, err := manager.databaseStatus(ctx)
	if err != nil {
		return err
	}
	switch status.State.Mode {
	case lifecycle.ModeOff:
		return nil
	case lifecycle.ModeMonitorOnly:
		return fmt.Errorf("MONITOR_ONLY has no execution to drain; use stop for OFF")
	case lifecycle.ModeActive, lifecycle.ModeDraining:
	default:
		return fmt.Errorf("unsupported persisted mode %s", status.State.Mode)
	}
	deadline := status.DatabaseTime.UTC().Add(timeout)
	if status.State.Mode == lifecycle.ModeDraining &&
		status.State.DrainDeadlineAt != nil {
		// DRAINING is already authoritative. Preserve its original absolute
		// deadline so a same-key retry is semantically identical and a new
		// key cannot extend the audited drain window.
		deadline = status.State.DrainDeadlineAt.UTC()
	}
	drainState, err := manager.transition(
		ctx,
		lifecycle.ModeDraining,
		requestID,
		deadline,
	)
	if err != nil {
		return err
	}
	if drainState.DrainDeadlineAt == nil {
		return fmt.Errorf("persisted DRAINING state has no drain deadline")
	}
	deadline = drainState.DrainDeadlineAt.UTC()
	// Keep the existing control process and its current internal address alive.
	// PostgreSQL fences routing/enqueue/lease after the DRAINING commit, while
	// the stable CONNECT proxy lets the current attempt reach a natural stop.
	// Drain reaches a worker by configured name, so ownership has to be proven
	// here too: signalling a foreign or partially migrated container would stop
	// somebody else's process, or half-migrate our own while its named volume is
	// still exclusively attached. Refusing one worker must not leave a second,
	// provably owned one running — DRAINING is already committed, and returning
	// early would make that worker unstoppable through agentopsctl.
	var refused []error
	for _, name := range []string{
		manager.config.TriageContainer,
		manager.config.RunnerContainer,
	} {
		worker, err := manager.runtime.Container(ctx, name)
		if err != nil {
			return err
		}
		if worker == nil || worker.Status.State != "running" {
			continue
		}
		if err := lifecycle.RequireManaged(
			"container "+name,
			worker.Configuration.Labels,
		); err != nil {
			refused = append(refused, err)
			continue
		}
		if err := manager.runtime.SignalTerm(ctx, name); err != nil {
			return err
		}
	}
	if len(refused) != 0 {
		return errors.Join(refused...)
	}
	if err := manager.reconcileExpiredRunnerWork(ctx); err != nil {
		return err
	}
	active, attempts, err := manager.inFlight(ctx)
	if err == nil && active == 0 && attempts == 0 {
		stopped, runtimeErr := manager.workersStopped(ctx)
		if runtimeErr != nil {
			return runtimeErr
		}
		if stopped {
			return nil
		}
	}
	drainContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := manager.reconcileExpiredRunnerWork(drainContext); err != nil {
			return err
		}
		active, attempts, err := manager.inFlight(drainContext)
		if err == nil && active == 0 && attempts == 0 {
			stopped, runtimeErr := manager.workersStopped(drainContext)
			if runtimeErr != nil {
				return runtimeErr
			}
			if stopped {
				return nil
			}
		}
		select {
		case <-drainContext.Done():
			message := fmt.Sprintf(
				"drain deadline reached; active leases and attempts remain unresolved",
			)
			_ = manager.recordFailure(
				context.Background(),
				"drain",
				message,
				true,
			)
			return fmt.Errorf("%s; workers were not force-killed", message)
		case <-ticker.C:
		}
	}
}

func (manager *manager) reconcileExpiredRunnerWork(ctx context.Context) error {
	_, err := manager.admin(
		ctx,
		[]string{
			"lifecycle",
			"reconcile-expired",
			"--max-attempts",
			strconv.Itoa(lifecycle.DefaultReconcileMaxAttempts),
			"--retry-base", lifecycle.DefaultReconcileRetryBase.String(),
		},
		nil,
	)
	if err != nil {
		return fmt.Errorf("reconcile expired runner work: %w", err)
	}
	return nil
}

func (manager *manager) Stop(
	ctx context.Context,
	timeout time.Duration,
	requestID string,
) error {
	if err := manager.ensureRuntime(ctx); err != nil {
		return err
	}
	status, err := manager.databaseStatus(ctx)
	if err != nil {
		containers, listErr := manager.runtime.Containers(ctx)
		if listErr == nil && !hasManagedService(containers, manager.config) {
			return nil
		}
		return err
	}
	if status.State.Mode == lifecycle.ModeActive ||
		status.State.Mode == lifecycle.ModeDraining {
		if err := manager.Drain(ctx, timeout, requestID+":drain"); err != nil {
			return err
		}
		status, err = manager.databaseStatus(ctx)
		if err != nil {
			return err
		}
	}
	if status.ActiveLeases != 0 || status.InFlightAttempts != 0 {
		return fmt.Errorf("refusing OFF with active leases or in-flight attempts")
	}
	for _, name := range []string{
		manager.config.RunnerContainer,
		manager.config.TriageContainer,
		manager.config.GitHubBrokerContainer,
		manager.config.ControlContainer,
	} {
		if err := manager.gracefulStop(ctx, name, 30*time.Second); err != nil {
			_ = manager.recordFailure(ctx, "stop."+name, err.Error(), false)
			return err
		}
		if err := manager.runtime.Delete(ctx, name); err != nil {
			return err
		}
	}
	current, err := manager.databaseStatus(ctx)
	if err != nil {
		return err
	}
	if current.State.Mode != lifecycle.ModeOff {
		if _, err := manager.transition(
			ctx,
			lifecycle.ModeOff,
			requestID+":off",
			time.Time{},
		); err != nil {
			return err
		}
	}
	if err := manager.gracefulStop(
		ctx,
		manager.config.PostgresContainer,
		30*time.Second,
	); err != nil {
		_ = manager.recordFailure(ctx, "stop.postgres", err.Error(), false)
		return err
	}
	if err := manager.runtime.Delete(ctx, manager.config.PostgresContainer); err != nil {
		return err
	}
	return manager.verifyPublishedSurface(
		ctx,
		false,
		false,
		false,
		lifecycle.ModeOff,
	)
}

type combinedStatus struct {
	Persisted            *lifecycle.Status                     `json:"persisted"`
	PersistedError       *string                               `json:"persistedError"`
	Containers           map[string]*lifecycle.ContainerActual `json:"containers"`
	ControlReachable     bool                                  `json:"controlReachable"`
	OffLoopbackReachable bool                                  `json:"offLoopbackReachable"`
	LoopbackOnly         bool                                  `json:"loopbackOnly"`
	Provenance           deploymentProvenanceStatus            `json:"provenance"`
}

type deploymentProvenanceStatus struct {
	Repository      string `json:"repository"`
	ControlRevision string `json:"controlRevision"`
	RunnerRevision  string `json:"runnerRevision"`
	Consistent      bool   `json:"consistent"`
}

type progressReport struct {
	Items      []control.DevelopmentProgressEvent `json:"items"`
	ObservedAt time.Time                          `json:"observedAt"`
}

func (manager *manager) Status(ctx context.Context) (combinedStatus, error) {
	if err := manager.ensureRuntime(ctx); err != nil {
		return combinedStatus{}, err
	}
	result := combinedStatus{
		Containers: make(map[string]*lifecycle.ContainerActual),
	}
	for role, name := range map[string]string{
		"control":       manager.config.ControlContainer,
		"github-broker": manager.config.GitHubBrokerContainer,
		"triage":        manager.config.TriageContainer,
		"runner":        manager.config.RunnerContainer,
		"postgres":      manager.config.PostgresContainer,
	} {
		actual, err := manager.runtime.Container(ctx, name)
		if err != nil {
			return combinedStatus{}, err
		}
		result.Containers[role] = redactContainerStatus(actual)
	}
	if result.Containers["postgres"] != nil &&
		result.Containers["postgres"].Status.State == "running" &&
		manager.runtime.ImageExists(ctx, manager.config.ControlImage) {
		persisted, err := manager.databaseStatus(ctx)
		if err != nil {
			message := redactedError(err, manager.config)
			result.PersistedError = &message
		} else {
			result.Persisted = &persisted
		}
	} else {
		message := "PostgreSQL is stopped; persisted lifecycle state is not mutated or inferred"
		result.PersistedError = &message
	}
	result.ControlReachable = tcpReachable(
		"127.0.0.1",
		manager.config.ControlHostPort,
		300*time.Millisecond,
	)
	result.OffLoopbackReachable = manager.offLoopbackReachable()
	control := result.Containers["control"]
	result.LoopbackOnly = control != nil &&
		exactLoopbackPublication(control, manager.config.ControlHostPort) &&
		result.ControlReachable &&
		!result.OffLoopbackReachable
	if control == nil {
		result.LoopbackOnly = !result.ControlReachable && !result.OffLoopbackReachable
	}
	controlRepository := containerEnvironment(
		result.Containers["control"],
		"AGENTOPS_RELEASE_CONSUMER_REPOSITORY",
	)
	runnerRepository := containerEnvironment(
		result.Containers["runner"],
		"AGENTOPS_RELEASE_CONSUMER_REPOSITORY",
	)
	result.Provenance = deploymentProvenanceStatus{
		Repository: controlRepository,
		ControlRevision: containerEnvironment(
			result.Containers["control"],
			"AGENTOPS_RELEASE_CONSUMER_REVISION",
		),
		RunnerRevision: containerEnvironment(
			result.Containers["runner"],
			"AGENTOPS_RELEASE_CONSUMER_REVISION",
		),
	}
	result.Provenance.Consistent = result.Provenance.Repository != "" &&
		result.Provenance.ControlRevision != "" &&
		(result.Containers["runner"] == nil ||
			(runnerRepository == result.Provenance.Repository &&
				result.Provenance.RunnerRevision == result.Provenance.ControlRevision))
	return result, nil
}

func containerEnvironment(actual *lifecycle.ContainerActual, key string) string {
	if actual == nil {
		return ""
	}
	for _, entry := range actual.Configuration.InitProcess.Environment {
		name, value, present := strings.Cut(entry, "=")
		if present && name == key {
			return value
		}
	}
	return ""
}

func redactContainerStatus(
	actual *lifecycle.ContainerActual,
) *lifecycle.ContainerActual {
	if actual == nil {
		return nil
	}
	redacted := *actual
	redacted.Configuration = actual.Configuration
	redacted.Configuration.InitProcess = actual.Configuration.InitProcess
	redacted.Configuration.InitProcess.Environment = append(
		[]string(nil),
		actual.Configuration.InitProcess.Environment...,
	)
	for index, entry := range redacted.Configuration.InitProcess.Environment {
		key, _, present := strings.Cut(entry, "=")
		if present && lifecycle.CredentialEnvironmentKey(key) {
			redacted.Configuration.InitProcess.Environment[index] = key + "=***"
		}
	}
	return &redacted
}

func printStatus(status combinedStatus) {
	mode := "unavailable"
	if status.Persisted != nil {
		mode = string(status.Persisted.State.Mode)
	}
	fmt.Printf("mode: %s\n", mode)
	for _, role := range []string{
		"control",
		"github-broker",
		"triage",
		"runner",
		"postgres",
	} {
		state := "absent"
		if status.Containers[role] != nil {
			state = status.Containers[role].Status.State
		}
		fmt.Printf("%s: %s\n", role, state)
	}
	fmt.Printf(
		"control loopback reachable: %t (off-loopback: %t)\n",
		status.ControlReachable,
		status.OffLoopbackReachable,
	)
	fmt.Printf(
		"provenance: %s control=%s runner=%s consistent=%t\n",
		status.Provenance.Repository,
		status.Provenance.ControlRevision,
		status.Provenance.RunnerRevision,
		status.Provenance.Consistent,
	)
	if status.Persisted != nil {
		fmt.Printf(
			"queued=%d active-leases=%d in-flight-attempts=%d drain-timeout=%t\n",
			status.Persisted.QueuedJobs,
			status.Persisted.ActiveLeases,
			status.Persisted.InFlightAttempts,
			status.Persisted.State.DrainTimedOut,
		)
		if status.Persisted.State.LastError != nil {
			fmt.Printf("last error: %s\n", *status.Persisted.State.LastError)
		}
	} else if status.PersistedError != nil {
		fmt.Printf("persisted status: %s\n", *status.PersistedError)
	}
}

func (manager *manager) Progress(
	ctx context.Context,
	repository string,
	issueNumber int64,
	limit int,
) (progressReport, error) {
	if err := manager.ensureRuntime(ctx); err != nil {
		return progressReport{}, err
	}
	output, err := manager.admin(ctx, []string{
		"progress",
		"--repository", repository,
		"--issue", strconv.FormatInt(issueNumber, 10),
		"--limit", strconv.Itoa(limit),
	}, nil)
	if err != nil {
		return progressReport{}, err
	}
	var report progressReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		return progressReport{}, fmt.Errorf("parse development progress: %w", err)
	}
	if report.Items == nil {
		report.Items = make([]control.DevelopmentProgressEvent, 0)
	}
	return report, nil
}

func printProgress(report progressReport, repository string, issueNumber int64) {
	fmt.Printf("issue: %s#%d\n", repository, issueNumber)
	if len(report.Items) == 0 {
		fmt.Println("progress: no durable events")
		return
	}
	children := make(map[int64]control.DevelopmentProgressEvent)
	for _, event := range report.Items {
		if event.ParentIssueNumber == nil || *event.ParentIssueNumber != issueNumber ||
			event.SubjectNumber == nil || *event.SubjectNumber == issueNumber {
			continue
		}
		if _, present := children[*event.SubjectNumber]; !present {
			children[*event.SubjectNumber] = event
		}
	}
	if len(children) > 0 {
		childNumbers := make([]int64, 0, len(children))
		for childNumber := range children {
			childNumbers = append(childNumbers, childNumber)
		}
		sort.Slice(childNumbers, func(left, right int) bool {
			return childNumbers[left] < childNumbers[right]
		})
		fmt.Printf("epic children with durable progress: %d\n", len(childNumbers))
		for _, childNumber := range childNumbers {
			event := children[childNumber]
			fmt.Printf(
				"  #%d  %-14s %-9s %s\n",
				childNumber,
				event.Phase,
				event.State,
				event.Step,
			)
			if event.WorktreePath != nil {
				fmt.Printf("       worktree: %s\n", *event.WorktreePath)
			}
			if event.Blocker != nil {
				fmt.Printf("       blocker: %s\n", *event.Blocker)
			}
			if event.NextGate != nil {
				fmt.Printf("       next gate: %s\n", *event.NextGate)
			}
		}
		fmt.Println("latest child activity:")
	}
	current := report.Items[0]
	if current.SubjectNumber != nil && *current.SubjectNumber != issueNumber {
		fmt.Printf("subject: #%d (child of #%d)\n", *current.SubjectNumber, issueNumber)
	}
	state := current.State
	fmt.Printf("kanban lane: %s\n", current.KanbanLane)
	fmt.Printf("phase: %s\n", current.Phase)
	fmt.Printf("step: %s\n", current.Step)
	fmt.Printf("state: %s\n", state)
	fmt.Printf("since: %s\n", current.OccurredAt.Local().Format(time.RFC3339))
	lastActivity := current.OccurredAt
	if current.LeaseHeartbeatAt != nil && current.LeaseHeartbeatAt.After(lastActivity) {
		lastActivity = *current.LeaseHeartbeatAt
	}
	fmt.Printf("last activity: %s\n", lastActivity.Local().Format(time.RFC3339))
	fmt.Printf("job: %s attempt %d (%s)\n", current.JobID, current.AttemptNumber, current.JobStatus)
	printProgressValue("worker", &current.WorkerID)
	printProgressValue("session", current.SessionName)
	printProgressValue("worktree", current.WorktreePath)
	printProgressValue("branch", current.Branch)
	printProgressValue("head SHA", current.HeadSHA)
	if current.PullRequestNumber != nil {
		fmt.Printf("pull request: #%d\n", *current.PullRequestNumber)
	} else {
		fmt.Println("pull request: —")
	}
	printProgressValue("summary", current.Summary)
	printProgressValue("next gate", current.NextGate)
	printProgressValue("gate", current.GateKey)
	if current.GateEnteredAt != nil {
		fmt.Printf("gate wait: %s\n", (time.Duration(current.GateWaitSeconds) * time.Second).String())
	} else {
		fmt.Println("gate wait: —")
	}
	if current.ReviewRound != nil && *current.ReviewRound > 0 {
		outcome := "in progress"
		if current.ReviewOutcome != nil {
			outcome = *current.ReviewOutcome
		}
		fmt.Printf("review: round %d (%s)\n", *current.ReviewRound, outcome)
	} else {
		fmt.Println("review: —")
	}
	if len(current.ReviewPerspectives) > 0 && string(current.ReviewPerspectives) != "[]" {
		fmt.Printf("review perspectives: %s\n", string(current.ReviewPerspectives))
	} else {
		fmt.Println("review perspectives: —")
	}
	if len(current.BranchLineage) > 0 && string(current.BranchLineage) != "{}" {
		fmt.Printf("branch lineage: %s\n", string(current.BranchLineage))
	} else {
		fmt.Println("branch lineage: —")
	}
	printProgressValue("human action", current.HumanAction)
	if current.EscalationID != nil {
		escalatedAt := "unknown time"
		if current.EscalatedAt != nil {
			escalatedAt = current.EscalatedAt.Local().Format(time.RFC3339)
		}
		fmt.Printf("escalation: #%d at %s\n", *current.EscalationID, escalatedAt)
		fmt.Printf("escalation evidence: %s\n", string(current.EscalationEvidence))
	} else {
		fmt.Println("escalation: —")
	}
	blocker := current.Blocker
	if blocker == nil {
		blocker = current.JobLastError
	}
	printProgressValue("blocker", blocker)
	fmt.Println("history:")
	for index := len(report.Items) - 1; index >= 0; index-- {
		event := report.Items[index]
		fmt.Printf(
			"  %s  #%d %-14s %-9s %s\n",
			event.OccurredAt.Local().Format("2006-01-02 15:04:05"),
			progressSubjectNumber(event),
			event.Phase,
			event.State,
			event.Step,
		)
	}
}

func progressSubjectNumber(event control.DevelopmentProgressEvent) int64 {
	if event.SubjectNumber == nil {
		return 0
	}
	return *event.SubjectNumber
}

func printProgressValue(label string, value *string) {
	if value == nil || strings.TrimSpace(*value) == "" {
		fmt.Printf("%s: —\n", label)
		return
	}
	fmt.Printf("%s: %s\n", label, *value)
}

var retainedWorktreePath = regexp.MustCompile(
	`^/workspace/registrations/[0-9a-f-]{36}/jobs/[0-9a-f-]{36}/(?:attempt-[1-9][0-9]*/(?:worktree|harness/\.harness/worktrees/[A-Za-z0-9._-]+)|state/\.harness/worktrees/[A-Za-z0-9._-]+)$`,
)

// Worktree inspects the exact isolated attempt projected by durable progress.
// Arguments are passed without a shell; a forged path cannot become a command.
func (manager *manager) Worktree(
	ctx context.Context,
	repository string,
	issueNumber int64,
	showDiff bool,
	openShell bool,
) error {
	report, err := manager.Progress(ctx, repository, issueNumber, 50)
	if err != nil {
		return err
	}
	childIssues := make(map[int64]bool)
	for _, event := range report.Items {
		if event.ParentIssueNumber != nil && *event.ParentIssueNumber == issueNumber &&
			event.SubjectNumber != nil && *event.SubjectNumber != issueNumber {
			childIssues[*event.SubjectNumber] = true
		}
	}
	if len(childIssues) > 0 {
		numbers := make([]int64, 0, len(childIssues))
		for child := range childIssues {
			numbers = append(numbers, child)
		}
		sort.Slice(numbers, func(left, right int) bool { return numbers[left] < numbers[right] })
		labels := make([]string, 0, len(numbers))
		for _, number := range numbers {
			labels = append(labels, fmt.Sprintf("#%d", number))
		}
		return fmt.Errorf(
			"%s#%d is an Epic; choose an explicit child Issue (%s)",
			repository,
			issueNumber,
			strings.Join(labels, ", "),
		)
	}
	worktree := ""
	selectedIndex := -1
	rejectedPaths := 0
	var status lifecycle.CommandResult
	for index, item := range report.Items {
		if item.WorktreePath == nil {
			continue
		}
		candidate := *item.WorktreePath
		// A path this command cannot prove is inside the runner volume is never
		// inspected, but it is also not authority to abandon the search: one
		// legacy or foreign event must not hide every usable retained attempt.
		if !retainedWorktreePath.MatchString(candidate) {
			rejectedPaths++
			continue
		}
		candidateStatus := manager.runtime.Exec(
			ctx,
			manager.config.RunnerContainer,
			"git", "-C", candidate, "status", "--short", "--branch",
		)
		if candidateStatus.Status != 0 {
			continue
		}
		worktree = candidate
		selectedIndex = index
		status = candidateStatus
		break
	}
	if worktree == "" {
		if rejectedPaths > 0 {
			return fmt.Errorf(
				"no available isolated worktree is recorded for %s#%d "+
					"(%d recorded path(s) were outside the runner volume and skipped)",
				repository,
				issueNumber,
				rejectedPaths,
			)
		}
		return fmt.Errorf("no available isolated worktree is recorded for %s#%d", repository, issueNumber)
	}
	fmt.Printf("worktree: %s\n", worktree)
	if rejectedPaths > 0 {
		fmt.Printf("skipped: %d recorded path(s) outside the runner volume\n", rejectedPaths)
	}
	if selectedIndex > 0 {
		fmt.Println("preserved: yes (superseded by newer progress)")
	}
	fmt.Print(status.Stdout)
	if !strings.HasSuffix(status.Stdout, "\n") {
		fmt.Println()
	}
	if !showDiff {
		if !openShell {
			return nil
		}
		return manager.runtime.InteractiveExec(
			ctx, manager.config.RunnerContainer, worktree, "/bin/sh",
		)
	}
	head := manager.runtime.Exec(
		ctx,
		manager.config.RunnerContainer,
		"git", "-C", worktree, "log", "-1", "--oneline", "--decorate",
	)
	if head.Status != 0 {
		return fmt.Errorf("retained worktree HEAD is unavailable")
	}
	fmt.Println("HEAD:")
	fmt.Print(head.Stdout)
	if !strings.HasSuffix(head.Stdout, "\n") {
		fmt.Println()
	}
	diff := manager.runtime.Exec(
		ctx,
		manager.config.RunnerContainer,
		"git", "-C", worktree, "diff", "--stat", "--", ".",
	)
	if diff.Status != 0 {
		return fmt.Errorf("retained worktree diff is unavailable")
	}
	fmt.Println("working tree diff:")
	if strings.TrimSpace(diff.Stdout) == "" {
		fmt.Println("  (clean; implementation may already be committed in HEAD)")
	} else {
		fmt.Print(diff.Stdout)
	}
	if diff.Stdout != "" && !strings.HasSuffix(diff.Stdout, "\n") {
		fmt.Println()
	}
	commit := manager.runtime.Exec(
		ctx,
		manager.config.RunnerContainer,
		"git", "-C", worktree, "show", "--stat", "--oneline", "--summary",
		"--no-renames", "HEAD", "--", ".",
	)
	if commit.Status != 0 {
		return fmt.Errorf("retained worktree commit summary is unavailable")
	}
	fmt.Println("HEAD commit stat:")
	fmt.Print(commit.Stdout)
	if !strings.HasSuffix(commit.Stdout, "\n") {
		fmt.Println()
	}
	if openShell {
		return manager.runtime.InteractiveExec(
			ctx, manager.config.RunnerContainer, worktree, "/bin/sh",
		)
	}
	return nil
}

func (manager *manager) Logs(
	ctx context.Context,
	component string,
	lines int,
	follow bool,
) error {
	name, present := map[string]string{
		"control":       manager.config.ControlContainer,
		"github-broker": manager.config.GitHubBrokerContainer,
		"triage":        manager.config.TriageContainer,
		"runner":        manager.config.RunnerContainer,
		"postgres":      manager.config.PostgresContainer,
	}[strings.ToLower(strings.TrimSpace(component))]
	if !present {
		return fmt.Errorf(
			"component must be control, github-broker, triage, runner, or postgres",
		)
	}
	actual, err := manager.runtime.Container(ctx, name)
	if err != nil {
		return err
	}
	if actual == nil {
		return fmt.Errorf("%s container is absent", component)
	}
	return manager.runtime.FollowLogs(ctx, name, lines, follow)
}

func (manager *manager) Open(ctx context.Context) error {
	control, err := manager.runtime.Container(
		ctx,
		manager.config.ControlContainer,
	)
	if err != nil {
		return err
	}
	if err := managedDashboardControl(control, manager.config); err != nil {
		return err
	}
	if !manager.dashboardReachable(
		"127.0.0.1",
		manager.config.ControlHostPort,
		time.Second,
	) {
		return fmt.Errorf("Control API is not reachable on loopback")
	}
	logs := manager.runtime.RecentLogs(
		ctx,
		manager.config.ControlContainer,
		DashboardBootstrapLogLines,
	)
	if logs.Status != 0 {
		return fmt.Errorf("dashboard bootstrap URL is unavailable from control logs")
	}
	dashboardURL, err := latestDashboardBootstrapURL(
		logs.Stdout+"\n"+logs.Stderr,
		manager.config.ControlHostPort,
	)
	if err != nil {
		return err
	}
	if err := manager.openDashboard(ctx, dashboardURL); err != nil {
		return redactedDashboardOpenError()
	}
	return nil
}

// managedDashboardControl reports why the running Control API cannot be opened.
// It returns the ownership failure rather than a bare bool: a partially
// migrated control container is reachable and healthy, and reporting it as "not
// running" sends the operator looking for a crash instead of at the labels.
func managedDashboardControl(
	actual *lifecycle.ContainerActual,
	config config,
) error {
	notRunning := fmt.Errorf(
		"managed Control API is not running on the expected loopback publication",
	)
	if actual == nil ||
		actual.ID != config.ControlContainer ||
		actual.Status.State != "running" {
		return notRunning
	}
	if err := lifecycle.RequireOwned(
		"container "+config.ControlContainer,
		actual.Configuration.Labels,
	); err != nil {
		return err
	}
	if err := lifecycle.RequireRole(
		"container "+config.ControlContainer,
		"control",
		actual.Configuration.Labels,
	); err != nil {
		return err
	}
	if !exactLoopbackPublication(actual, config.ControlHostPort) {
		return notRunning
	}
	return nil
}

func latestDashboardBootstrapURL(logOutput string, port int) (string, error) {
	expectedHost := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	latest := ""
	for _, line := range strings.Split(logOutput, "\n") {
		start := strings.IndexByte(line, '{')
		if start < 0 {
			continue
		}
		var entry struct {
			DashboardBootstrapURL string `json:"dashboardBootstrapUrl"`
		}
		if err := json.NewDecoder(
			strings.NewReader(line[start:]),
		).Decode(&entry); err != nil || entry.DashboardBootstrapURL == "" {
			continue
		}
		if validDashboardBootstrapURL(
			entry.DashboardBootstrapURL,
			expectedHost,
		) {
			latest = entry.DashboardBootstrapURL
		}
	}
	if latest == "" {
		return "", fmt.Errorf(
			"valid dashboard bootstrap URL was not found in recent control logs",
		)
	}
	return latest, nil
}

func validDashboardBootstrapURL(raw, expectedHost string) bool {
	parsed, err := url.Parse(raw)
	if err != nil ||
		parsed.Scheme != "http" ||
		parsed.Host != expectedHost ||
		parsed.User != nil ||
		parsed.Path != "/dashboard/bootstrap" ||
		parsed.RawPath != "" ||
		parsed.Fragment != "" ||
		parsed.Opaque != "" {
		return false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query) != 1 || len(query["token"]) != 1 {
		return false
	}
	token := query["token"][0]
	return len(token) >= 32 && len(token) <= 512
}

func redactedDashboardOpenError() error {
	return fmt.Errorf("open Dashboard: browser launcher failed")
}

func (manager *manager) RotatePostgresAdmin(
	ctx context.Context,
	requestID string,
) error {
	if err := manager.config.validatePostgresRotation(); err != nil {
		return err
	}
	if err := manager.ensureRuntime(ctx); err != nil {
		return err
	}
	_, err := manager.admin(
		ctx,
		[]string{"rotate-postgres-admin", "--request-id", requestID},
		map[string]string{
			"AGENTOPS_NEXT_POSTGRES_PASSWORD": manager.config.NextPostgresPassword,
		},
	)
	return err
}

func (manager *manager) ensureRuntime(ctx context.Context) error {
	capability := manager.runtime.Capability(ctx)
	if !capability.Available {
		return fmt.Errorf("Apple Container CLI is not available")
	}
	if !capability.ServiceRunning {
		if err := manager.runtime.StartSystem(ctx); err != nil {
			return fmt.Errorf("start Apple Container system: %w", err)
		}
		capability = manager.runtime.Capability(ctx)
		if !capability.ServiceRunning {
			return fmt.Errorf("Apple Container system did not become ready")
		}
	}
	return nil
}

func (manager *manager) ensurePostgres(ctx context.Context) (bool, error) {
	spec := manager.postgresSpec()
	if err := manager.sealSpec(ctx, &spec); err != nil {
		return false, err
	}
	actual, err := manager.runtime.Container(ctx, manager.config.PostgresContainer)
	if err != nil {
		return false, err
	}
	if actual != nil {
		if err := lifecycle.RequireOwned(
			"container "+manager.config.PostgresContainer,
			actual.Configuration.Labels,
		); err != nil {
			return false, err
		}
		if err := validatePostgresMajorBoundary(actual); err != nil {
			return false, err
		}
		if actual.Status.State == "running" {
			if err := validatePostgresActual(actual, manager.config); err != nil {
				return false, err
			}
			if err := validateSpecActual(actual, spec); err != nil {
				// Drift is resolved by draining, stopping, and restarting onto
				// the preserved volume. Incomplete ownership labels are resolved
				// by fixing the labels — following the drift remediation would
				// delete and recreate a container that is not drifting at all,
				// and this is the PostgreSQL container, whose named volume the
				// recreation would have to detach and re-attach.
				if errors.Is(err, lifecycle.ErrMalformedOwnershipLabels) {
					return false, err
				}
				return false, fmt.Errorf(
					"PostgreSQL image/spec drift requires DRAINING, stop, and volume-preserving restart: %w",
					err,
				)
			}
			return false, manager.waitPostgres(ctx)
		}
		if err := manager.runtime.Delete(ctx, manager.config.PostgresContainer); err != nil {
			return false, err
		}
	}
	if err := manager.validatePostgresVolumeLayout(ctx); err != nil {
		return false, err
	}
	_, err = manager.runtime.RunContainer(ctx, spec)
	if err != nil {
		return false, err
	}
	return true, manager.waitPostgres(ctx)
}

const (
	managedPostgresMajor   = "18"
	managedPostgresDataDir = "/var/lib/postgresql/18/docker"
)

func validatePostgresMajorBoundary(actual *lifecycle.ContainerActual) error {
	major := ""
	dataDir := ""
	for _, entry := range actual.Configuration.InitProcess.Environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case "PG_MAJOR":
			major = value
		case "PGDATA":
			dataDir = value
		}
	}
	if (major != "" && major != managedPostgresMajor) ||
		(dataDir != "" && dataDir != managedPostgresDataDir) {
		return fmt.Errorf(
			"PostgreSQL major upgrade to %s is required; the existing data volume is preserved and must be migrated before restart",
			managedPostgresMajor,
		)
	}
	return nil
}

func (manager *manager) validatePostgresVolumeLayout(ctx context.Context) error {
	output, err := manager.runtime.RunContainer(ctx, lifecycle.ContainerSpec{
		Name:  manager.config.PostgresContainer + "-volume-probe",
		Role:  "postgres-volume-probe",
		Image: manager.config.PostgresImage,
		Mounts: []lifecycle.Mount{{
			Volume:   manager.config.PostgresVolume,
			Target:   "/var/lib/postgresql",
			ReadOnly: true,
		}},
		ReadOnly:   true,
		CapDropAll: true,
		Entrypoint: "/bin/sh",
		Command: []string{"-ceu", `
current=""
legacy=""
if [ -f /var/lib/postgresql/18/docker/PG_VERSION ]; then
  current="$(cat /var/lib/postgresql/18/docker/PG_VERSION)"
fi
if [ -f /var/lib/postgresql/data/PG_VERSION ]; then
  legacy="$(cat /var/lib/postgresql/data/PG_VERSION)"
fi
printf 'current=%s\nlegacy=%s\n' "$current" "$legacy"
`},
		Remove: true,
	})
	if err != nil {
		return fmt.Errorf("inspect PostgreSQL data volume: %w", err)
	}
	return validatePostgresVolumeLayout(output)
}

func validatePostgresVolumeLayout(output string) error {
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "current" && key != "legacy") {
			return fmt.Errorf("PostgreSQL data-volume probe returned an invalid result")
		}
		if _, duplicated := values[key]; duplicated {
			return fmt.Errorf("PostgreSQL data-volume probe returned a duplicated field")
		}
		value = strings.TrimSpace(value)
		if value != "" && !regexp.MustCompile(`^[0-9]+$`).MatchString(value) {
			return fmt.Errorf("PostgreSQL data-volume probe returned an invalid major version")
		}
		values[key] = value
	}
	if _, ok := values["current"]; !ok {
		return fmt.Errorf("PostgreSQL data-volume probe omitted the current layout")
	}
	if _, ok := values["legacy"]; !ok {
		return fmt.Errorf("PostgreSQL data-volume probe omitted the legacy layout")
	}
	if values["legacy"] != "" {
		return fmt.Errorf(
			"PostgreSQL %s data was found in the legacy layout; the volume is preserved and must be migrated to PostgreSQL %s before restart",
			values["legacy"], managedPostgresMajor,
		)
	}
	if values["current"] != "" && values["current"] != managedPostgresMajor {
		return fmt.Errorf(
			"PostgreSQL %s data was found in the current layout; expected major %s and refusing to start",
			values["current"], managedPostgresMajor,
		)
	}
	return nil
}

func (manager *manager) postgresSpec() lifecycle.ContainerSpec {
	return lifecycle.ContainerSpec{
		Name:     manager.config.PostgresContainer,
		Role:     "postgres",
		Image:    manager.config.PostgresImage,
		Networks: []string{manager.config.Network},
		Environment: map[string]string{
			"POSTGRES_PASSWORD": manager.config.PostgresPassword,
			"POSTGRES_DB":       "agentops",
			"PGDATA":            managedPostgresDataDir,
		},
		Mounts: []lifecycle.Mount{{
			Volume: manager.config.PostgresVolume,
			Target: "/var/lib/postgresql",
		}},
		Tmpfs: []string{"/tmp", "/run/postgresql"},
		Init:  true, Detach: true,
	}
}

func (manager *manager) waitPostgres(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		result := manager.runtime.Exec(
			wait,
			manager.config.PostgresContainer,
			"pg_isready", "-U", "postgres", "-d", "agentops",
		)
		if result.Status == 0 {
			return nil
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("PostgreSQL readiness timeout: %w", wait.Err())
		case <-ticker.C:
		}
	}
}

func (manager *manager) migrateAndBootstrap(ctx context.Context) error {
	if _, err := manager.admin(ctx, []string{"migrate"}, nil); err != nil {
		return err
	}
	if _, err := manager.admin(
		ctx,
		[]string{"bootstrap-roles"},
		map[string]string{
			"AGENTOPS_CONTROL_DB_PASSWORD": manager.config.ControlDBPassword,
			"AGENTOPS_TRIAGE_DB_PASSWORD":  manager.config.TriageDBPassword,
			"AGENTOPS_RUNNER_DB_PASSWORD":  manager.config.RunnerDBPassword,
		},
	); err != nil {
		return err
	}
	return nil
}

func (manager *manager) admin(
	ctx context.Context,
	command []string,
	extra map[string]string,
) (string, error) {
	databaseHost, err := manager.databaseHost(ctx)
	if err != nil {
		return "", err
	}
	environment := map[string]string{
		"AGENTOPS_DATABASE_URL": manager.config.adminDatabaseURL(databaseHost),
		"AGENTOPS_APP_ROOT":     "/app",
	}
	for key, value := range extra {
		environment[key] = value
	}
	name := fmt.Sprintf(
		"%s-admin-%d-%d",
		manager.config.Prefix,
		os.Getpid(),
		time.Now().UTC().UnixNano()%1_000_000,
	)
	return manager.runtime.RunContainer(ctx, lifecycle.ContainerSpec{
		Name:        name,
		Role:        "admin",
		Image:       manager.config.ControlImage,
		Networks:    []string{manager.config.Network},
		Environment: environment,
		Tmpfs:       []string{"/tmp"},
		ReadOnly:    true,
		CapDropAll:  true,
		Remove:      true,
		Command:     command,
	})
}

func (manager *manager) databaseStatus(ctx context.Context) (lifecycle.Status, error) {
	output, err := manager.admin(ctx, []string{"lifecycle", "status"}, nil)
	if err != nil {
		return lifecycle.Status{}, err
	}
	var status lifecycle.Status
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		return lifecycle.Status{}, fmt.Errorf("parse lifecycle status: %w", err)
	}
	return status, nil
}

func (manager *manager) recoverDraining(
	ctx context.Context,
	status lifecycle.Status,
) (lifecycle.Status, error) {
	deadline := time.Now().UTC().Add(10 * time.Minute)
	if status.State.DrainDeadlineAt != nil {
		deadline = *status.State.DrainDeadlineAt
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := manager.reconcileExpiredRunnerWork(ctx); err != nil {
			return lifecycle.Status{}, err
		}
		current, err := manager.databaseStatus(ctx)
		if err != nil {
			return lifecycle.Status{}, err
		}
		if current.ActiveLeases == 0 && current.InFlightAttempts == 0 {
			return current, nil
		}
		if !time.Now().UTC().Before(deadline) {
			message := fmt.Sprintf(
				"recovery drain deadline reached with %d active leases and %d in-flight attempts",
				current.ActiveLeases,
				current.InFlightAttempts,
			)
			_ = manager.recordFailure(
				context.Background(),
				"start.recovery",
				message,
				true,
			)
			return lifecycle.Status{}, fmt.Errorf("%s", message)
		}
		select {
		case <-ctx.Done():
			return lifecycle.Status{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (manager *manager) transition(
	ctx context.Context,
	to lifecycle.Mode,
	requestID string,
	deadline time.Time,
) (lifecycle.State, error) {
	command := []string{
		"lifecycle", "transition",
		"--to", string(to),
		"--idempotency-key", requestID,
		"--actor", "agentopsctl",
	}
	if !deadline.IsZero() {
		command = append(
			command,
			"--drain-deadline",
			deadline.UTC().Format(time.RFC3339Nano),
		)
	}
	output, err := manager.admin(ctx, command, nil)
	if err != nil {
		return lifecycle.State{}, err
	}
	var result struct {
		State lifecycle.State `json:"state"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return lifecycle.State{}, fmt.Errorf("parse lifecycle transition: %w", err)
	}
	if result.State.Mode != to {
		return result.State, fmt.Errorf(
			"lifecycle transition did not establish %s (current=%s)",
			to,
			result.State.Mode,
		)
	}
	return result.State, nil
}

func (manager *manager) recordFailure(
	ctx context.Context,
	operation, message string,
	drainTimeout bool,
) error {
	message = redactedError(errors.New(message), manager.config)
	command := []string{
		"lifecycle", "failure",
		"--operation", operation,
		"--message", message,
	}
	if drainTimeout {
		command = append(command, "--drain-timeout")
	}
	_, err := manager.admin(ctx, command, nil)
	return err
}

func (manager *manager) replaceControl(
	ctx context.Context,
	mode lifecycle.Mode,
) (mutationReceipt, error) {
	databaseHost, err := manager.databaseHost(ctx)
	if err != nil {
		return mutationReceipt{}, err
	}
	spec := manager.controlSpec(mode, databaseHost)
	if err := manager.sealSpec(ctx, &spec); err != nil {
		return mutationReceipt{}, err
	}
	receipt := mutationReceipt{Mutated: true}
	if err := manager.gracefulStop(
		ctx,
		manager.config.ControlContainer,
		20*time.Second,
	); err != nil {
		return receipt, err
	}
	if err := manager.runtime.Delete(ctx, manager.config.ControlContainer); err != nil {
		return receipt, err
	}
	_, err = manager.runtime.RunContainer(ctx, spec)
	if err != nil {
		return receipt, err
	}
	health, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(
			health,
			http.MethodGet,
			fmt.Sprintf(
				"http://127.0.0.1:%d/healthz",
				manager.config.ControlHostPort,
			),
			nil,
		)
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return receipt, nil
			}
		}
		select {
		case <-health.Done():
			return receipt, fmt.Errorf("Control API readiness timeout: %w", health.Err())
		case <-ticker.C:
		}
	}
}

func (manager *manager) controlSpec(
	mode lifecycle.Mode,
	databaseHost string,
) lifecycle.ContainerSpec {
	environment := map[string]string{
		"AGENTOPS_DATABASE_URL":              manager.config.controlDatabaseURL(databaseHost),
		"AGENTOPS_CONTROL_TOKEN":             manager.config.ControlToken,
		"AGENTOPS_DASHBOARD_BOOTSTRAP_TOKEN": manager.config.DashboardToken,
		"AGENTOPS_GITHUB_WEBHOOK_SECRET":     manager.config.WebhookSecret,
		"AGENTOPS_OPERATING_MODE":            string(mode),
		"AGENTOPS_DASHBOARD_ORIGIN": fmt.Sprintf(
			"http://127.0.0.1:%d",
			manager.config.ControlHostPort,
		),
		"AGENTOPS_CONTROL_LISTEN":                "127.0.0.1:8081",
		"AGENTOPS_CONTROL_PROXY_LISTEN":          "0.0.0.0:8080",
		"AGENTOPS_RUNNER_EGRESS_PROXY_LISTEN":    "0.0.0.0:8082",
		"AGENTOPS_RUNNER_PROVIDER":               manager.config.Provider,
		"AGENTOPS_RUNNER_PROVIDER_AUTH":          manager.config.providerAuth(mode),
		"AGENTOPS_GITHUB_MONITOR_BROKER_ENABLED": "true",
		"AGENTOPS_APP_ROOT":                      "/app",
	}
	manager.config.addReleaseProvenance(environment)
	return lifecycle.ContainerSpec{
		Name:  manager.config.ControlContainer,
		Role:  "control",
		Image: manager.config.ControlImage,
		// Apple Container assigns the default route to the first network.
		// Keep public egress on default while retaining the host-only network
		// solely for runner/PostgreSQL connectivity.
		Networks:    []string{"default", manager.config.Network},
		Environment: environment,
		Publish: []lifecycle.Publication{{
			HostIP:        "127.0.0.1",
			HostPort:      manager.config.ControlHostPort,
			ContainerPort: 8080,
		}},
		Tmpfs:      []string{"/tmp"},
		ReadOnly:   true,
		CapDropAll: true,
		Init:       true,
		Detach:     true,
	}
}

func (manager *manager) ensureRunnerVolumeOwner(ctx context.Context) error {
	_, err := manager.runtime.RunContainer(ctx, lifecycle.ContainerSpec{
		Name: fmt.Sprintf(
			"%s-volume-init-%d",
			manager.config.Prefix,
			os.Getpid(),
		),
		Role:  "volume-init",
		Image: manager.config.RunnerImage,
		Networks: []string{
			manager.config.Network,
		},
		Mounts: []lifecycle.Mount{{
			Volume: manager.config.RunnerVolume,
			Target: "/workspace",
		}},
		User:       "root",
		Entrypoint: "/bin/sh",
		Command: []string{
			"-c",
			"mkdir -p /workspace/registrations /workspace/store && " +
				"chown 65532:65532 /workspace/registrations /workspace/store",
		},
		CapDropAll: true,
		CapAdd:     []string{"CAP_CHOWN"},
		Remove:     true,
	})
	return err
}

func (manager *manager) seedCodexCredentialVolume(
	ctx context.Context,
	mode lifecycle.Mode,
	role string,
	volume string,
) error {
	if !manager.config.usesCodexAuthFileFor(mode) {
		return nil
	}
	if err := validateCodexAuthSource(manager.config.CodexAuthPath); err != nil {
		return err
	}
	// role別に一意な名前にする。同一プロセスがtriage/runnerの2回seedingを行うため、
	// PIDだけでは同名になり、先行init VMの非同期teardownとattachが競合する。
	name := fmt.Sprintf(
		"%s-credential-init-%s-%d",
		manager.config.Prefix,
		role,
		os.Getpid(),
	)
	_, err := manager.runtime.RunContainer(ctx, lifecycle.ContainerSpec{
		Name:     name,
		Role:     "volume-init",
		Image:    manager.config.RunnerImage,
		Networks: []string{manager.config.Network},
		Mounts: []lifecycle.Mount{{
			Volume: volume,
			Target: "/credentials",
		}},
		User:       "root",
		Entrypoint: "/bin/sh",
		Command: []string{
			"-c",
			"mkdir -p /credentials/codex && " +
				"chown 0:0 /credentials/codex && " +
				"chmod 0700 /credentials/codex && " +
				"chown 65532:65532 /credentials/codex && sleep " +
				strconv.FormatInt(
					int64(CredentialInitializerLifetime/time.Second),
					10,
				),
		},
		CapDropAll: true,
		CapAdd:     []string{"CAP_CHOWN"},
		Detach:     true,
		Remove:     true,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = manager.runtime.Stop(context.Background(), name, 5)
		_ = manager.runtime.Delete(context.Background(), name)
	}()
	ready, cancel := context.WithTimeout(ctx, CredentialSeedReadyTimeout)
	defer cancel()
	if err := manager.runtime.WaitState(
		ready,
		name,
		"running",
		ContainerReadyPollInterval,
	); err != nil {
		return err
	}
	if err := manager.runtime.CopyFileToContainer(
		ctx,
		name,
		manager.config.CodexAuthPath,
		"/credentials/codex/auth.json",
	); err != nil {
		return err
	}
	return nil
}

func (manager *manager) seedGitHubAppKeyVolume(
	ctx context.Context,
) error {
	if err := validateGitHubAppKeySource(
		manager.config.GitHubAppKeyPath,
	); err != nil {
		return err
	}
	name := fmt.Sprintf(
		"%s-github-key-init-%d",
		manager.config.Prefix,
		os.Getpid(),
	)
	_, err := manager.runtime.RunContainer(ctx, lifecycle.ContainerSpec{
		Name:     name,
		Role:     "volume-init",
		Image:    manager.config.GitHubBrokerImage,
		Networks: []string{manager.config.Network},
		Mounts: []lifecycle.Mount{{
			Volume: manager.config.GitHubAppKeyVolume,
			Target: "/run/agentops-github-app",
		}},
		User:       "root",
		Entrypoint: "/usr/local/bin/agentops-github-credential-helper",
		Command:    []string{"seed-wait"},
		CapDropAll: true,
		CapAdd:     []string{"CAP_CHOWN"},
		Detach:     true,
		Remove:     true,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = manager.runtime.Stop(context.Background(), name, 5)
		_ = manager.runtime.Delete(context.Background(), name)
	}()
	ready, cancel := context.WithTimeout(ctx, CredentialSeedReadyTimeout)
	defer cancel()
	if err := manager.runtime.WaitState(
		ready,
		name,
		"running",
		ContainerReadyPollInterval,
	); err != nil {
		return err
	}
	if err := manager.runtime.CopyPrivateFileWithHelper(
		ctx,
		name,
		manager.config.GitHubAppKeyPath,
		"/usr/local/bin/agentops-github-credential-helper",
		"seed",
	); err != nil {
		return err
	}
	return nil
}

func (manager *manager) replaceGitHubBroker(
	ctx context.Context,
	mode lifecycle.Mode,
) (mutationReceipt, error) {
	spec := manager.githubBrokerSpec(mode)
	if err := manager.sealSpec(ctx, &spec); err != nil {
		return mutationReceipt{}, err
	}
	receipt := mutationReceipt{Mutated: true}
	if err := manager.gracefulStop(
		ctx,
		manager.config.GitHubBrokerContainer,
		20*time.Second,
	); err != nil {
		return receipt, err
	}
	if err := manager.runtime.Delete(
		ctx,
		manager.config.GitHubBrokerContainer,
	); err != nil {
		return receipt, err
	}
	if err := manager.seedGitHubAppKeyVolume(ctx); err != nil {
		return receipt, err
	}
	if _, err := manager.runtime.RunContainer(ctx, spec); err != nil {
		return receipt, err
	}
	ready, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(ContainerReadyPollInterval)
	defer ticker.Stop()
	for {
		result := manager.runtime.Exec(
			ready,
			manager.config.GitHubBrokerContainer,
			"/usr/local/bin/agentops-github-credential-helper",
			"health",
		)
		if result.Status == 0 {
			return receipt, nil
		}
		select {
		case <-ready.Done():
			return receipt, fmt.Errorf(
				"GitHub credential broker readiness timeout: %w",
				ready.Err(),
			)
		case <-ticker.C:
		}
	}
}

func (manager *manager) githubBrokerSpec(
	mode lifecycle.Mode,
) lifecycle.ContainerSpec {
	environment := map[string]string{
		"AGENTOPS_GITHUB_APP_ID": strconv.FormatInt(
			manager.config.GitHubAppID,
			10,
		),
		"AGENTOPS_GITHUB_APP_INSTALLATION_ID": strconv.FormatInt(
			manager.config.GitHubInstallationID,
			10,
		),
		"AGENTOPS_GITHUB_APP_SLUG":                 manager.config.GitHubAppSlug,
		"AGENTOPS_GITHUB_APP_OWNER":                manager.config.GitHubAppOwner,
		"AGENTOPS_OPERATING_MODE":                  string(mode),
		"AGENTOPS_GITHUB_BROKER_LISTEN":            "0.0.0.0:8083",
		"AGENTOPS_GITHUB_BROKER_TRIAGE_CAPABILITY": manager.config.githubBrokerCapability("triage"),
		// The readiness probe calls /healthz, which is unauthenticated, so the
		// broker's own container never receives a role capability to hold.
		"AGENTOPS_GITHUB_BROKER_URL": "http://127.0.0.1:8083",
	}
	// DRAINING carries ACTIVE's development scope: compensation restores the
	// broker while draining, and the runner it is draining still needs to close
	// its attempt. Withholding the runner policy here would start a broker the
	// draining runner cannot use.
	if mode == lifecycle.ModeActive || mode == lifecycle.ModeDraining {
		environment["AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY"] =
			manager.config.githubBrokerCapability("runner")
	}
	return lifecycle.ContainerSpec{
		Name:        manager.config.GitHubBrokerContainer,
		Role:        "github-broker",
		Image:       manager.config.GitHubBrokerImage,
		Networks:    []string{"default", manager.config.Network},
		Environment: environment,
		Mounts: []lifecycle.Mount{{
			Volume:   manager.config.GitHubAppKeyVolume,
			Target:   "/run/agentops-github-app",
			ReadOnly: true,
		}},
		Tmpfs:      []string{"/tmp"},
		ReadOnly:   true,
		CapDropAll: true,
		Init:       true,
		Detach:     true,
	}
}

func (manager *manager) replaceTriage(
	ctx context.Context,
	mode lifecycle.Mode,
) (mutationReceipt, error) {
	databaseHost, err := manager.databaseHost(ctx)
	if err != nil {
		return mutationReceipt{}, err
	}
	controlHost, err := manager.networkHost(ctx, manager.config.ControlContainer)
	if err != nil {
		return mutationReceipt{}, err
	}
	githubBrokerHost, err := manager.networkHost(
		ctx,
		manager.config.GitHubBrokerContainer,
	)
	if err != nil {
		return mutationReceipt{}, err
	}
	spec := manager.triageSpec(
		mode,
		databaseHost,
		controlHost,
		githubBrokerHost,
	)
	if err := manager.sealSpec(ctx, &spec); err != nil {
		return mutationReceipt{}, err
	}
	receipt := mutationReceipt{Mutated: true}
	if err := manager.gracefulStop(
		ctx,
		manager.config.TriageContainer,
		20*time.Second,
	); err != nil {
		return receipt, err
	}
	if err := manager.runtime.Delete(ctx, manager.config.TriageContainer); err != nil {
		return receipt, err
	}
	if err := manager.seedCodexCredentialVolume(
		ctx,
		mode,
		"triage",
		manager.config.TriageCredentialVolume,
	); err != nil {
		return receipt, err
	}
	if _, err := manager.runtime.RunContainer(ctx, spec); err != nil {
		return receipt, err
	}
	if err := manager.waitWorkerStable(
		ctx,
		manager.config.TriageContainer,
		"triage",
	); err != nil {
		return receipt, err
	}
	if mode == lifecycle.ModeActive {
		if err := manager.probeProvider(
			ctx,
			manager.config.TriageContainer,
			"triage",
		); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}

func (manager *manager) triageSpec(
	mode lifecycle.Mode,
	databaseHost, controlHost, githubBrokerHost string,
) lifecycle.ContainerSpec {
	outbound := []map[string]any{
		{"host": databaseHost, "port": 5432},
		{"host": githubBrokerHost, "port": 8083},
		{"host": "api.github.com", "port": 443},
	}
	providerAuth := manager.config.providerAuth(mode)
	if mode == lifecycle.ModeActive {
		if manager.config.Provider == "codex" && providerAuth == "codex-login" {
			outbound = append(
				outbound,
				map[string]any{"host": "chatgpt.com", "port": 443},
				map[string]any{"host": "auth.openai.com", "port": 443},
			)
		} else if manager.config.Provider == "codex" {
			outbound = append(
				outbound,
				map[string]any{"host": "api.openai.com", "port": 443},
			)
		} else {
			outbound = append(
				outbound,
				map[string]any{"host": "api.anthropic.com", "port": 443},
			)
		}
	}
	outboundJSON, _ := json.Marshal(outbound)
	mounts := []map[string]any{}
	runtimeMounts := []lifecycle.Mount{}
	if manager.config.usesCodexAuthFileFor(mode) {
		mounts = append(mounts, map[string]any{
			"source":   manager.config.TriageCredentialVolume,
			"target":   "/run/agentops-credentials",
			"readOnly": true,
		})
		runtimeMounts = append(runtimeMounts, lifecycle.Mount{
			Volume:   manager.config.TriageCredentialVolume,
			Target:   "/run/agentops-credentials",
			ReadOnly: true,
		})
	}
	mountJSON, _ := json.Marshal(mounts)
	labels := manager.config.triagePolicyLabels()
	environment := map[string]string{
		"AGENTOPS_TRIAGE_DATABASE_URL":         manager.config.triageDatabaseURL(databaseHost),
		"AGENTOPS_TRIAGE_WORKER_ID":            manager.config.TriageContainer,
		"AGENTOPS_TRIAGE_PROVIDER":             manager.config.Provider,
		"AGENTOPS_TRIAGE_PROVIDER_AUTH":        providerAuth,
		"AGENTOPS_OPERATING_MODE":              string(mode),
		"AGENTOPS_TRIAGE_MOUNTS_JSON":          string(mountJSON),
		"AGENTOPS_TRIAGE_PUBLISHED_PORTS_JSON": "[]",
		"AGENTOPS_TRIAGE_OUTBOUND_JSON":        string(outboundJSON),
		"AGENTOPS_GITHUB_BROKER_URL":           "http://" + githubBrokerHost + ":8083",
		"AGENTOPS_GITHUB_BROKER_CAPABILITY":    manager.config.githubBrokerCapability("triage"),
		"AGENTOPS_GITHUB_BROKER_ROLE":          "triage",
		"AGENTOPS_TRIAGE_READY_LABEL":          labels[0],
		"AGENTOPS_TRIAGE_CLAIMED_LABEL":        labels[1],
		"AGENTOPS_TRIAGE_CANDIDATE_LABEL":      labels[2],
		"AGENTOPS_TRIAGE_BLOCKED_LABEL":        labels[3],
		"AGENTOPS_TRIAGE_NEEDS_INFO_LABEL":     labels[4],
		"AGENTOPS_APP_ROOT":                    "/app",
		"HTTPS_PROXY":                          "http://" + controlHost + ":8082",
		"HTTP_PROXY":                           "http://" + controlHost + ":8082",
		"NO_PROXY": databaseHost + "," + githubBrokerHost +
			",127.0.0.1,localhost",
	}
	if manager.config.TriageContextPaths != "" {
		environment["AGENTOPS_TRIAGE_CONTEXT_PATHS_JSON"] =
			manager.config.TriageContextPaths
	}
	if manager.config.TriageModel != "" {
		environment["AGENTOPS_TRIAGE_MODEL"] = manager.config.TriageModel
	}
	manager.config.addReleaseProvenance(environment)
	if mode == lifecycle.ModeActive && manager.config.Provider == "codex" {
		if manager.config.usesCodexAuthFileFor(mode) {
			environment["CODEX_HOME"] = "/run/agentops-credentials/codex"
		} else {
			environment["OPENAI_API_KEY"] = manager.config.ProviderToken
		}
	} else if mode == lifecycle.ModeActive {
		environment["ANTHROPIC_API_KEY"] = manager.config.ProviderToken
	}
	return lifecycle.ContainerSpec{
		Name:        manager.config.TriageContainer,
		Role:        "triage",
		Image:       manager.config.TriageImage,
		Networks:    []string{manager.config.Network},
		Environment: environment,
		Mounts:      runtimeMounts,
		Tmpfs:       []string{"/tmp", "/home/agentops"},
		ReadOnly:    true,
		CapDropAll:  true,
		Init:        true,
		Detach:      true,
	}
}

func (manager *manager) replaceRunner(
	ctx context.Context,
	mode lifecycle.Mode,
) (mutationReceipt, error) {
	databaseHost, err := manager.databaseHost(ctx)
	if err != nil {
		return mutationReceipt{}, err
	}
	controlHost, err := manager.networkHost(ctx, manager.config.ControlContainer)
	if err != nil {
		return mutationReceipt{}, err
	}
	githubBrokerHost, err := manager.networkHost(
		ctx,
		manager.config.GitHubBrokerContainer,
	)
	if err != nil {
		return mutationReceipt{}, err
	}
	spec := manager.runnerSpec(
		mode,
		databaseHost,
		controlHost,
		githubBrokerHost,
	)
	if err := manager.sealSpec(ctx, &spec); err != nil {
		return mutationReceipt{}, err
	}
	receipt := mutationReceipt{Mutated: true}
	if err := manager.gracefulStop(
		ctx,
		manager.config.RunnerContainer,
		20*time.Second,
	); err != nil {
		return receipt, err
	}
	if err := manager.runtime.Delete(ctx, manager.config.RunnerContainer); err != nil {
		return receipt, err
	}
	if err := manager.ensureRunnerVolumeOwner(ctx); err != nil {
		return receipt, err
	}
	if err := manager.seedCodexCredentialVolume(
		ctx,
		mode,
		"runner",
		manager.config.RunnerCredentialVolume,
	); err != nil {
		return receipt, err
	}
	_, err = manager.runtime.RunContainer(ctx, spec)
	if err != nil {
		return receipt, err
	}
	if err := manager.waitWorkerStable(
		ctx,
		manager.config.RunnerContainer,
		"runner",
	); err != nil {
		return receipt, err
	}
	if mode == lifecycle.ModeActive {
		if err := manager.probeRunnerProvider(ctx); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}

func (manager *manager) removeRunner(
	ctx context.Context,
) (mutationReceipt, error) {
	actual, err := manager.runtime.Container(ctx, manager.config.RunnerContainer)
	if err != nil {
		return mutationReceipt{}, err
	}
	if actual == nil {
		return mutationReceipt{}, nil
	}
	if err := lifecycle.RequireManaged(
		"container "+manager.config.RunnerContainer,
		actual.Configuration.Labels,
	); err != nil {
		return mutationReceipt{}, err
	}
	receipt := mutationReceipt{Mutated: true}
	if err := manager.gracefulStop(
		ctx,
		manager.config.RunnerContainer,
		20*time.Second,
	); err != nil {
		return receipt, err
	}
	if err := manager.runtime.Delete(ctx, manager.config.RunnerContainer); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (manager *manager) waitWorkerStable(
	ctx context.Context,
	container, role string,
) error {
	ready, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := manager.runtime.WaitState(
		ready,
		container,
		"running",
		500*time.Millisecond,
	); err != nil {
		return err
	}
	select {
	case <-ready.Done():
		return ready.Err()
	case <-time.After(2 * time.Second):
	}
	actual, err := manager.runtime.Container(ctx, container)
	if err != nil {
		return err
	}
	if actual == nil || actual.Status.State != "running" {
		logs := manager.runtime.RecentLogs(ctx, container, RunnerReadinessLogLines)
		detail := strings.TrimSpace(logs.Stdout + logs.Stderr)
		if detail == "" {
			detail = "no worker log output"
		}
		return fmt.Errorf(
			"%s exited during readiness stabilization: %s",
			role,
			redactedError(errors.New(detail), manager.config),
		)
	}
	return nil
}

func (manager *manager) probeRunnerProvider(ctx context.Context) error {
	return manager.probeProvider(
		ctx,
		manager.config.RunnerContainer,
		"runner",
	)
}

func (manager *manager) probeProvider(
	ctx context.Context,
	container, role string,
) error {
	probeContext, cancel := context.WithTimeout(ctx, ProviderProbeTimeout)
	defer cancel()
	var command []string
	switch manager.config.Provider {
	case "codex":
		// The catalog request exercises the selected login/API credential and
		// its exact egress path without running an agent or persisting output.
		command = []string{"codex", "debug", "models"}
	case "claude":
		// Claude's status command validates the CLI credential boundary. The
		// CISO-07 grounded execution provider is Codex; Claude remains review-only.
		command = []string{"claude", "auth", "status", "--json"}
	default:
		return fmt.Errorf("runner provider readiness is unsupported")
	}
	result := manager.runtime.Exec(
		probeContext,
		container,
		command...,
	)
	if probeContext.Err() != nil || result.Status != 0 {
		return fmt.Errorf("%s provider authentication readiness failed", role)
	}
	return nil
}

func (manager *manager) runnerSpec(
	mode lifecycle.Mode,
	databaseHost, controlHost, githubBrokerHost string,
) lifecycle.ContainerSpec {
	outbound := []map[string]any{
		{"host": databaseHost, "port": 5432},
		{"host": githubBrokerHost, "port": 8083},
		{"host": "github.com", "port": 443},
		{"host": "api.github.com", "port": 443},
	}
	providerAuth := manager.config.providerAuth(mode)
	if mode == lifecycle.ModeActive {
		if manager.config.Provider == "codex" && providerAuth == "codex-login" {
			outbound = append(
				outbound,
				map[string]any{"host": "chatgpt.com", "port": 443},
				map[string]any{"host": "auth.openai.com", "port": 443},
			)
		} else if manager.config.Provider == "codex" {
			outbound = append(
				outbound,
				map[string]any{"host": "api.openai.com", "port": 443},
			)
		} else {
			outbound = append(
				outbound,
				map[string]any{"host": "api.anthropic.com", "port": 443},
			)
		}
	}
	outboundJSON, _ := json.Marshal(outbound)
	mounts := []map[string]any{{
		"source":   manager.config.RunnerVolume,
		"target":   "/workspace",
		"readOnly": false,
	}}
	runtimeMounts := []lifecycle.Mount{{
		Volume: manager.config.RunnerVolume,
		Target: "/workspace",
	}}
	if manager.config.usesCodexAuthFileFor(mode) {
		mounts = append(mounts, map[string]any{
			"source":   manager.config.RunnerCredentialVolume,
			"target":   "/run/agentops-credentials",
			"readOnly": true,
		})
		runtimeMounts = append(runtimeMounts, lifecycle.Mount{
			Volume:   manager.config.RunnerCredentialVolume,
			Target:   "/run/agentops-credentials",
			ReadOnly: true,
		})
	}
	mountJSON, _ := json.Marshal(mounts)
	environment := map[string]string{
		"AGENTOPS_RUNNER_DATABASE_URL":         manager.config.runnerDatabaseURL(databaseHost),
		"AGENTOPS_RUNNER_WORKER_ID":            manager.config.RunnerContainer,
		"AGENTOPS_RUNNER_PROVIDER":             manager.config.Provider,
		"AGENTOPS_RUNNER_PROVIDER_AUTH":        providerAuth,
		"AGENTOPS_OPERATING_MODE":              string(mode),
		"AGENTOPS_RUNNER_MOUNTS_JSON":          string(mountJSON),
		"AGENTOPS_RUNNER_PUBLISHED_PORTS_JSON": "[]",
		"AGENTOPS_RUNNER_OUTBOUND_JSON":        string(outboundJSON),
		"AGENTOPS_GITHUB_BROKER_URL":           "http://" + githubBrokerHost + ":8083",
		"AGENTOPS_GITHUB_BROKER_CAPABILITY":    manager.config.githubBrokerCapability("runner"),
		"AGENTOPS_GITHUB_BROKER_ROLE":          "runner",
		"HTTPS_PROXY":                          "http://" + controlHost + ":8082",
		"HTTP_PROXY":                           "http://" + controlHost + ":8082",
		"NO_PROXY": databaseHost + "," + githubBrokerHost +
			",127.0.0.1,localhost",
	}
	manager.config.addReleaseProvenance(environment)
	if mode == lifecycle.ModeActive && manager.config.Provider == "codex" {
		if manager.config.usesCodexAuthFileFor(mode) {
			environment["CODEX_HOME"] = "/run/agentops-credentials/codex"
		} else {
			environment["OPENAI_API_KEY"] = manager.config.ProviderToken
		}
	} else if mode == lifecycle.ModeActive {
		environment["ANTHROPIC_API_KEY"] = manager.config.ProviderToken
	}
	return lifecycle.ContainerSpec{
		Name:        manager.config.RunnerContainer,
		Role:        "runner",
		Image:       manager.config.RunnerImage,
		Networks:    []string{manager.config.Network},
		Environment: environment,
		Mounts:      runtimeMounts,
		Tmpfs:       []string{"/tmp", "/home/agentops"},
		ReadOnly:    true,
		CapDropAll:  true,
		Init:        true,
		Detach:      true,
	}
}

func (manager *manager) sealSpec(
	ctx context.Context,
	spec *lifecycle.ContainerSpec,
) error {
	imageDigest, err := manager.runtime.ImageDigest(ctx, spec.Image)
	if err != nil {
		return err
	}
	specDigest, err := lifecycle.SpecDigest(*spec, imageDigest)
	if err != nil {
		return err
	}
	spec.SpecDigest = specDigest
	return nil
}

func (manager *manager) databaseHost(ctx context.Context) (string, error) {
	return manager.networkHost(ctx, manager.config.PostgresContainer)
}

func (manager *manager) networkHost(
	ctx context.Context,
	containerName string,
) (string, error) {
	actual, err := manager.runtime.Container(ctx, containerName)
	if err != nil {
		return "", err
	}
	if actual == nil || actual.Status.State != "running" {
		return "", fmt.Errorf("container %s is not running", containerName)
	}
	for _, network := range actual.Status.Networks {
		if network.Network != manager.config.Network {
			continue
		}
		host, _, _ := strings.Cut(network.IPv4Address, "/")
		if net.ParseIP(host) == nil {
			continue
		}
		return host, nil
	}
	return "", fmt.Errorf(
		"container %s has no IPv4 address on network %s",
		containerName,
		manager.config.Network,
	)
}

func (manager *manager) gracefulStop(
	ctx context.Context,
	name string,
	timeout time.Duration,
) error {
	actual, err := manager.runtime.Container(ctx, name)
	if err != nil || actual == nil || actual.Status.State != "running" {
		return err
	}
	if err := lifecycle.RequireManaged(
		"container "+name,
		actual.Configuration.Labels,
	); err != nil {
		return err
	}
	if err := manager.runtime.SignalTerm(ctx, name); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return manager.runtime.WaitState(wait, name, "stopped", 500*time.Millisecond)
}

func (manager *manager) workersStopped(ctx context.Context) (bool, error) {
	for _, name := range []string{
		manager.config.TriageContainer,
		manager.config.RunnerContainer,
	} {
		actual, err := manager.runtime.Container(ctx, name)
		if err != nil {
			return false, err
		}
		if actual != nil && actual.Status.State == "running" {
			return false, nil
		}
	}
	return true, nil
}

func (manager *manager) inFlight(ctx context.Context) (int64, int64, error) {
	result := manager.runtime.Exec(
		ctx,
		manager.config.PostgresContainer,
		"psql", "-U", "postgres", "-d", "agentops", "-Atqc",
		`SELECT
		   count(DISTINCT l.id) FILTER (
		     WHERE l.status = 'active' AND l.expires_at > clock_timestamp()
		   )::text || ':' ||
		   count(DISTINCT a.id) FILTER (WHERE a.status = 'running')::text
		 FROM agentops_control.job_leases l
		 FULL JOIN agentops_control.job_attempts a ON a.id = l.attempt_id`,
	)
	if result.Status != 0 {
		return 0, 0, fmt.Errorf("query in-flight state failed")
	}
	parts := strings.Split(strings.TrimSpace(result.Stdout), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid in-flight state response")
	}
	active, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	attempts, err := strconv.ParseInt(parts[1], 10, 64)
	return active, attempts, err
}

func (manager *manager) compensateStart(
	ctx context.Context,
	postgresStarted, controlChanged, githubBrokerChanged bool,
	triageChanged, runnerChanged bool,
	initialMode *lifecycle.Mode,
	requestID string,
) error {
	target := lifecycle.ModeOff
	if initialMode != nil {
		target = *initialMode
	}
	current, currentErr := manager.databaseStatus(ctx)
	if currentErr == nil &&
		(current.State.Mode == lifecycle.ModeActive ||
			current.State.Mode == lifecycle.ModeDraining) {
		// Never skip a drain merely to restore the pre-command mode: a runner
		// can acquire work between the ACTIVE commit and a later failure.
		target = lifecycle.ModeDraining
	} else if target == lifecycle.ModeActive || target == lifecycle.ModeDraining {
		// A partially replaced execution topology must never be advertised as
		// ACTIVE. Preserve DRAINING until work is reconciled; if recovery
		// already reached MONITOR_ONLY, retain that later safe boundary.
		if currentErr == nil &&
			(current.State.Mode == lifecycle.ModeMonitorOnly ||
				current.State.Mode == lifecycle.ModeOff) {
			target = current.State.Mode
		} else {
			target = lifecycle.ModeDraining
		}
	}
	if err := manager.rollbackLifecycle(
		ctx,
		target,
		requestID+":compensate",
	); err != nil {
		_ = manager.recordFailure(
			ctx,
			"start.compensation.lifecycle",
			err.Error(),
			false,
		)
	}

	names, restoreControl, restoreGitHubBroker, restoreTriage, restoreRunner :=
		compensationTopology(
			target,
			controlChanged,
			githubBrokerChanged,
			triageChanged,
			runnerChanged,
			manager.config.ControlContainer,
			manager.config.GitHubBrokerContainer,
			manager.config.TriageContainer,
			manager.config.RunnerContainer,
		)
	for _, name := range names {
		_ = manager.gracefulStop(ctx, name, 10*time.Second)
		_ = manager.runtime.Delete(ctx, name)
	}
	if restoreControl {
		// Recreate the control plane from current credentials and the safe
		// durable mode; this restores a pre-existing MONITOR_ONLY topology and
		// retains the recovery proxy for DRAINING.
		_, _ = manager.replaceControl(ctx, target)
	}
	if restoreGitHubBroker {
		_, _ = manager.replaceGitHubBroker(ctx, target)
	}
	if restoreRunner {
		_, _ = manager.replaceRunner(ctx, target)
	}
	if restoreTriage {
		// MONITOR_ONLY retains only the capability-limited Issue monitor and
		// triager; the development runner remains absent.
		_, _ = manager.replaceTriage(ctx, target)
	}
	if postgresStarted {
		if target == lifecycle.ModeOff {
			_ = manager.gracefulStop(ctx, manager.config.PostgresContainer, 15*time.Second)
			_ = manager.runtime.Delete(ctx, manager.config.PostgresContainer)
		}
	}
	return nil
}

func compensationTopology(
	target lifecycle.Mode,
	controlChanged, githubBrokerChanged, triageChanged, runnerChanged bool,
	controlContainer, githubBrokerContainer, triageContainer,
	runnerContainer string,
) (
	stopNames []string,
	restoreControl, restoreGitHubBroker, restoreTriage, restoreRunner bool,
) {
	if runnerChanged {
		stopNames = append(stopNames, runnerContainer)
	}
	if triageChanged {
		stopNames = append(stopNames, triageContainer)
	}
	if githubBrokerChanged {
		stopNames = append(stopNames, githubBrokerContainer)
	}
	if controlChanged && target == lifecycle.ModeOff {
		stopNames = append(stopNames, controlContainer)
	}
	return stopNames,
		controlChanged && target != lifecycle.ModeOff,
		githubBrokerChanged && target != lifecycle.ModeOff,
		triageChanged && target == lifecycle.ModeMonitorOnly,
		runnerChanged && target == lifecycle.ModeActive
}

func (manager *manager) rollbackLifecycle(
	ctx context.Context,
	target lifecycle.Mode,
	requestID string,
) error {
	status, err := manager.databaseStatus(ctx)
	if err != nil {
		return err
	}
	current := status.State.Mode
	if current == target {
		return nil
	}
	if current == lifecycle.ModeActive {
		state, err := manager.transition(
			ctx,
			lifecycle.ModeDraining,
			requestID+":draining",
			time.Now().UTC().Add(10*time.Minute),
		)
		if err != nil {
			return err
		}
		current = state.Mode
	}
	if target == lifecycle.ModeDraining {
		if current != lifecycle.ModeDraining {
			return fmt.Errorf("cannot compensate %s to DRAINING", current)
		}
		return nil
	}
	if current == lifecycle.ModeDraining {
		state, err := manager.transition(
			ctx,
			target,
			requestID+":"+strings.ToLower(string(target)),
			time.Time{},
		)
		if err != nil {
			return err
		}
		current = state.Mode
	}
	if current == lifecycle.ModeOff && target == lifecycle.ModeMonitorOnly {
		state, err := manager.transition(
			ctx,
			target,
			requestID+":monitor",
			time.Time{},
		)
		if err != nil {
			return err
		}
		current = state.Mode
	}
	if current == lifecycle.ModeMonitorOnly && target == lifecycle.ModeOff {
		state, err := manager.transition(
			ctx,
			target,
			requestID+":off",
			time.Time{},
		)
		if err != nil {
			return err
		}
		current = state.Mode
	}
	if current != target {
		return fmt.Errorf("compensation established %s instead of %s", current, target)
	}
	return nil
}

func (manager *manager) verifyPublishedSurface(
	ctx context.Context,
	controlExpected, triageExpected, runnerExpected bool,
	mode lifecycle.Mode,
) error {
	reachable := tcpReachable(
		"127.0.0.1",
		manager.config.ControlHostPort,
		time.Second,
	)
	if reachable != controlExpected {
		return fmt.Errorf(
			"control loopback publication expected=%t observed=%t",
			controlExpected,
			reachable,
		)
	}
	if tcpReachable("127.0.0.1", 5432, 300*time.Millisecond) {
		return fmt.Errorf("PostgreSQL 5432 is unexpectedly reachable on host loopback")
	}
	if controlExpected && manager.offLoopbackReachable() {
		return fmt.Errorf("control port is reachable off loopback")
	}
	control, err := manager.runtime.Container(ctx, manager.config.ControlContainer)
	if err != nil {
		return err
	}
	if controlExpected && !exactLoopbackPublication(
		control,
		manager.config.ControlHostPort,
	) {
		return fmt.Errorf("control does not have exactly one loopback-only publication")
	}
	if controlExpected {
		if err := manager.verifyManagedTopology(
			ctx,
			triageExpected,
			runnerExpected,
			mode,
		); err != nil {
			return err
		}
	}
	for _, role := range []string{
		manager.config.GitHubBrokerContainer,
		manager.config.TriageContainer,
		manager.config.RunnerContainer,
		manager.config.PostgresContainer,
	} {
		actual, err := manager.runtime.Container(ctx, role)
		if err != nil {
			return err
		}
		if actual != nil &&
			(len(actual.Configuration.PublishedPorts) != 0 ||
				len(actual.Configuration.PublishedSock) != 0) {
			return fmt.Errorf("%s exposes a host port or socket", role)
		}
	}
	return nil
}

func (manager *manager) verifyManagedTopology(
	ctx context.Context,
	triageExpected bool,
	runnerExpected bool,
	mode lifecycle.Mode,
) error {
	control, err := manager.runtime.Container(ctx, manager.config.ControlContainer)
	if err != nil {
		return err
	}
	postgres, err := manager.runtime.Container(ctx, manager.config.PostgresContainer)
	if err != nil {
		return err
	}
	runner, err := manager.runtime.Container(ctx, manager.config.RunnerContainer)
	if err != nil {
		return err
	}
	triage, err := manager.runtime.Container(ctx, manager.config.TriageContainer)
	if err != nil {
		return err
	}
	githubBroker, err := manager.runtime.Container(
		ctx,
		manager.config.GitHubBrokerContainer,
	)
	if err != nil {
		return err
	}
	if err := validateControlActual(control, manager.config); err != nil {
		return err
	}
	databaseHost, err := manager.databaseHost(ctx)
	if err != nil {
		return err
	}
	controlSpec := manager.controlSpec(mode, databaseHost)
	if err := manager.sealSpec(ctx, &controlSpec); err != nil {
		return err
	}
	if err := validateSpecActual(control, controlSpec); err != nil {
		return err
	}
	if err := validatePostgresActual(postgres, manager.config); err != nil {
		return err
	}
	postgresSpec := manager.postgresSpec()
	if err := manager.sealSpec(ctx, &postgresSpec); err != nil {
		return err
	}
	if err := validateSpecActual(postgres, postgresSpec); err != nil {
		return err
	}
	if err := validateGitHubBrokerActual(
		githubBroker,
		manager.config,
	); err != nil {
		return err
	}
	githubBrokerSpec := manager.githubBrokerSpec(mode)
	if err := manager.sealSpec(ctx, &githubBrokerSpec); err != nil {
		return err
	}
	if err := validateSpecActual(
		githubBroker,
		githubBrokerSpec,
	); err != nil {
		return err
	}
	controlHost, err := manager.networkHost(
		ctx,
		manager.config.ControlContainer,
	)
	if err != nil {
		return err
	}
	githubBrokerHost, err := manager.networkHost(
		ctx,
		manager.config.GitHubBrokerContainer,
	)
	if err != nil {
		return err
	}
	if triageExpected {
		if err := validateTriageActual(triage, manager.config, mode); err != nil {
			return err
		}
		triageSpec := manager.triageSpec(
			mode,
			databaseHost,
			controlHost,
			githubBrokerHost,
		)
		if err := manager.sealSpec(ctx, &triageSpec); err != nil {
			return err
		}
		if err := validateSpecActual(triage, triageSpec); err != nil {
			return err
		}
	} else if triage != nil && triage.Status.State == "running" {
		return fmt.Errorf("triage container is unexpectedly running")
	}
	if runnerExpected {
		if err := validateRunnerActual(runner, manager.config, mode); err != nil {
			return err
		}
		runnerSpec := manager.runnerSpec(
			mode,
			databaseHost,
			controlHost,
			githubBrokerHost,
		)
		if err := manager.sealSpec(ctx, &runnerSpec); err != nil {
			return err
		}
		if err := validateSpecActual(runner, runnerSpec); err != nil {
			return err
		}
	} else if runner != nil && runner.Status.State == "running" {
		return fmt.Errorf("runner container is unexpectedly running")
	}
	return nil
}

func validateSpecActual(
	actual *lifecycle.ContainerActual,
	expected lifecycle.ContainerSpec,
) error {
	if actual == nil {
		return fmt.Errorf("%s container is absent", expected.Name)
	}
	imageDigest := actual.Configuration.Image.Descriptor.Digest
	if !strings.HasPrefix(imageDigest, "sha256:") {
		return fmt.Errorf("%s has no immutable image descriptor", expected.Name)
	}
	expectedDigest, err := lifecycle.SpecDigest(expected, imageDigest)
	if err != nil {
		return err
	}
	if expected.SpecDigest != expectedDigest {
		return fmt.Errorf("%s desired specification digest is inconsistent", expected.Name)
	}
	if err := lifecycle.RequireSpecDigest(
		expected.Name,
		expected.SpecDigest,
		actual.Configuration.Labels,
	); err != nil {
		return err
	}
	actualEnvironment := make(map[string]string)
	for _, entry := range actual.Configuration.InitProcess.Environment {
		key, value, present := strings.Cut(entry, "=")
		if !present || key == "" {
			return fmt.Errorf("%s actual environment is malformed", expected.Name)
		}
		if _, duplicate := actualEnvironment[key]; duplicate {
			return fmt.Errorf("%s actual environment repeats %s", expected.Name, key)
		}
		actualEnvironment[key] = value
	}
	for key, desired := range expected.Environment {
		if observed, present := actualEnvironment[key]; !present || observed != desired {
			return fmt.Errorf("%s environment drifted at %s", expected.Name, key)
		}
	}
	for key := range actualEnvironment {
		if lifecycle.CredentialEnvironmentKey(key) {
			if _, expectedCredential := expected.Environment[key]; !expectedCredential {
				return fmt.Errorf(
					"%s has an unexpected credential environment key %s",
					expected.Name,
					key,
				)
			}
		}
	}
	return nil
}

func (manager *manager) validateExistingTopology(
	ctx context.Context,
) error {
	for _, name := range []string{
		manager.config.ControlContainer,
		manager.config.GitHubBrokerContainer,
		manager.config.TriageContainer,
		manager.config.RunnerContainer,
		manager.config.PostgresContainer,
	} {
		actual, err := manager.runtime.Container(ctx, name)
		if err != nil {
			return err
		}
		if actual != nil && actual.Status.State == "running" {
			var validateErr error
			switch name {
			case manager.config.ControlContainer:
				validateErr = validateControlActual(actual, manager.config)
			case manager.config.GitHubBrokerContainer:
				validateErr = validateGitHubBrokerActual(actual, manager.config)
			case manager.config.TriageContainer:
				workerMode, modeErr := existingWorkerMode(actual, false)
				if modeErr != nil {
					return modeErr
				}
				validateErr = validateTriageActual(
					actual,
					manager.config,
					workerMode,
				)
			case manager.config.RunnerContainer:
				workerMode, modeErr := existingWorkerMode(actual, true)
				if modeErr != nil {
					return modeErr
				}
				validateErr = validateRunnerActual(
					actual,
					manager.config,
					workerMode,
				)
			case manager.config.PostgresContainer:
				validateErr = validatePostgresActual(actual, manager.config)
			}
			if validateErr != nil {
				return validateErr
			}
		}
	}
	return nil
}

// existingWorkerMode reads the lifecycle boundary the running worker was
// created with. A failed start can durably compensate to MONITOR_ONLY before
// it has replaced the prior ACTIVE containers. Validating those containers as
// MONITOR_ONLY would reject their expected read-only credential mount and make
// the next start unable to repair the mixed topology. The worker's own mode is
// safe to use for this structural preflight; validateSpecActual later requires
// the exact desired-mode spec before the topology can be published.
func existingWorkerMode(
	actual *lifecycle.ContainerActual,
	runner bool,
) (lifecycle.Mode, error) {
	var raw string
	found := false
	for _, entry := range actual.Configuration.InitProcess.Environment {
		key, value, present := strings.Cut(entry, "=")
		if !present || key != "AGENTOPS_OPERATING_MODE" {
			continue
		}
		if found {
			return "", fmt.Errorf("worker operating mode is duplicated")
		}
		raw = value
		found = true
	}
	mode, err := lifecycle.ParseMode(raw)
	if !found || err != nil || mode == lifecycle.ModeOff ||
		(runner && mode != lifecycle.ModeActive && mode != lifecycle.ModeDraining) {
		return "", fmt.Errorf("worker operating mode is invalid")
	}
	return mode, nil
}

func validateGitHubBrokerActual(
	actual *lifecycle.ContainerActual,
	config config,
) error {
	if err := validateManagedActual(
		actual,
		config.GitHubBrokerContainer,
		"github-broker",
		config.GitHubBrokerImage,
		[]string{"default", config.Network},
		true,
	); err != nil {
		return err
	}
	if len(actual.Configuration.PublishedPorts) != 0 ||
		len(actual.Configuration.PublishedSock) != 0 {
		return fmt.Errorf("GitHub credential broker exposes a host port or socket")
	}
	if !exactMounts(actual, map[string]string{
		"/tmp":                     "tmpfs",
		"/run/agentops-github-app": config.GitHubAppKeyVolume,
	}) {
		return fmt.Errorf(
			"GitHub credential broker mounts do not match the hardened topology",
		)
	}
	return nil
}

func validateControlActual(
	actual *lifecycle.ContainerActual,
	config config,
) error {
	if err := validateManagedActual(
		actual,
		config.ControlContainer,
		"control",
		config.ControlImage,
		[]string{"default", config.Network},
		true,
	); err != nil {
		return err
	}
	if !exactLoopbackPublication(actual, config.ControlHostPort) {
		return fmt.Errorf("control publication does not match the expected loopback port")
	}
	if !exactMounts(actual, map[string]string{"/tmp": "tmpfs"}) {
		return fmt.Errorf("control mounts do not match the hardened topology")
	}
	return nil
}

func validateTriageActual(
	actual *lifecycle.ContainerActual,
	config config,
	mode lifecycle.Mode,
) error {
	if err := validateManagedActual(
		actual,
		config.TriageContainer,
		"triage",
		config.TriageImage,
		[]string{config.Network},
		true,
	); err != nil {
		return err
	}
	if len(actual.Configuration.PublishedPorts) != 0 ||
		len(actual.Configuration.PublishedSock) != 0 {
		return fmt.Errorf("triage exposes a host port or socket")
	}
	expectedMounts := map[string]string{
		"/tmp":           "tmpfs",
		"/home/agentops": "tmpfs",
	}
	if config.usesCodexAuthFileFor(workerCredentialMode(mode)) {
		expectedMounts["/run/agentops-credentials"] = config.TriageCredentialVolume
	}
	if !exactMounts(actual, expectedMounts) {
		return fmt.Errorf("triage mounts do not match the hardened topology")
	}
	return nil
}

func validateRunnerActual(
	actual *lifecycle.ContainerActual,
	config config,
	mode lifecycle.Mode,
) error {
	if err := validateManagedActual(
		actual,
		config.RunnerContainer,
		"runner",
		config.RunnerImage,
		[]string{config.Network},
		true,
	); err != nil {
		return err
	}
	if len(actual.Configuration.PublishedPorts) != 0 ||
		len(actual.Configuration.PublishedSock) != 0 {
		return fmt.Errorf("runner exposes a host port or socket")
	}
	expectedMounts := map[string]string{
		"/tmp":           "tmpfs",
		"/home/agentops": "tmpfs",
		"/workspace":     config.RunnerVolume,
	}
	if config.usesCodexAuthFileFor(workerCredentialMode(mode)) {
		expectedMounts["/run/agentops-credentials"] = config.RunnerCredentialVolume
	}
	if !exactMounts(actual, expectedMounts) {
		return fmt.Errorf("runner mounts do not match the hardened topology")
	}
	return nil
}

// DRAINING retains the already-running ACTIVE workers until their leases and
// attempts reach zero. Validate those workers against the credential boundary
// they were started with; no DRAINING worker replacement is performed.
func workerCredentialMode(mode lifecycle.Mode) lifecycle.Mode {
	if mode == lifecycle.ModeDraining {
		return lifecycle.ModeActive
	}
	return mode
}

func validatePostgresActual(
	actual *lifecycle.ContainerActual,
	config config,
) error {
	if err := validateManagedActual(
		actual,
		config.PostgresContainer,
		"postgres",
		config.PostgresImage,
		[]string{config.Network},
		false,
	); err != nil {
		return err
	}
	if len(actual.Configuration.PublishedPorts) != 0 ||
		len(actual.Configuration.PublishedSock) != 0 {
		return fmt.Errorf("PostgreSQL exposes a host port or socket")
	}
	if !exactMounts(actual, map[string]string{
		"/tmp":                "tmpfs",
		"/run/postgresql":     "tmpfs",
		"/var/lib/postgresql": config.PostgresVolume,
	}) {
		return fmt.Errorf("PostgreSQL mounts do not match the managed topology")
	}
	return nil
}

func validateManagedActual(
	actual *lifecycle.ContainerActual,
	name, role, image string,
	networks []string,
	hardened bool,
) error {
	if actual == nil || actual.Status.State != "running" {
		return fmt.Errorf("%s container is not running", name)
	}
	if actual.ID != name {
		return fmt.Errorf("%s ownership or role label does not match", name)
	}
	subject := "container " + name
	if err := lifecycle.RequireOwned(subject, actual.Configuration.Labels); err != nil {
		return err
	}
	if err := lifecycle.RequireRole(subject, role, actual.Configuration.Labels); err != nil {
		return err
	}
	if !imageReferenceMatches(actual.Configuration.Image.Reference, image) {
		return fmt.Errorf("%s image does not match %s", name, image)
	}
	actualNetworks := make([]string, 0, len(actual.Configuration.Networks))
	for _, network := range actual.Configuration.Networks {
		actualNetworks = append(actualNetworks, network.Network)
	}
	if strings.Join(actualNetworks, "\x00") != strings.Join(networks, "\x00") {
		return fmt.Errorf("%s networks do not match the managed topology", name)
	}
	if hardened {
		if !actual.Configuration.ReadOnly ||
			!containsString(actual.Configuration.CapDrop, "ALL") ||
			len(actual.Configuration.CapAdd) != 0 ||
			!managedNonRootUser(
				actual.Configuration.InitProcess.User.ID.UID,
				actual.Configuration.InitProcess.User.Raw.UserString,
			) {
			return fmt.Errorf("%s runtime hardening does not match", name)
		}
	}
	return nil
}

func managedNonRootUser(uid int, raw string) bool {
	raw = strings.TrimSpace(raw)
	if uid != 0 && uid != 65532 {
		return false
	}
	switch raw {
	case "":
		return uid == 65532
	case "agentops", "65532", "65532:65532":
		return uid == 0 || uid == 65532
	default:
		return false
	}
}

func imageReferenceMatches(actual, expected string) bool {
	return actual == expected || strings.HasSuffix(actual, "/"+expected)
}

func exactMounts(
	actual *lifecycle.ContainerActual,
	expected map[string]string,
) bool {
	if actual == nil || len(actual.Configuration.Mounts) != len(expected) {
		return false
	}
	for _, mount := range actual.Configuration.Mounts {
		kind, present := expected[mount.Destination]
		if !present {
			return false
		}
		if kind == "tmpfs" {
			if _, present := mount.Type["tmpfs"]; !present {
				return false
			}
			continue
		}
		volume, present := mount.Type["volume"].(map[string]any)
		if !present || volume["name"] != kind {
			return false
		}
	}
	return true
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func exactLoopbackPublication(
	actual *lifecycle.ContainerActual,
	hostPort int,
) bool {
	if actual == nil ||
		len(actual.Configuration.PublishedPorts) != 1 ||
		len(actual.Configuration.PublishedSock) != 0 {
		return false
	}
	publication := actual.Configuration.PublishedPorts[0]
	return publication["hostAddress"] == "127.0.0.1" &&
		numericJSONValue(publication["hostPort"]) == hostPort &&
		numericJSONValue(publication["containerPort"]) == 8080 &&
		numericJSONValue(publication["count"]) == 1 &&
		publication["proto"] == "tcp"
}

func numericJSONValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return -1
	}
}

func (manager *manager) offLoopbackReachable() bool {
	interfaces, _ := net.Interfaces()
	for _, item := range interfaces {
		if item.Flags&net.FlagUp == 0 || item.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := item.Addrs()
		for _, address := range addresses {
			host, _, err := net.ParseCIDR(address.String())
			if err != nil || host.IsLoopback() || host.To4() == nil {
				continue
			}
			if tcpReachable(host.String(), manager.config.ControlHostPort, 300*time.Millisecond) {
				return true
			}
		}
	}
	return false
}

func tcpReachable(host string, port int, timeout time.Duration) bool {
	connection, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort(host, strconv.Itoa(port)),
		timeout,
	)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func redactedError(err error, config config) string {
	message := err.Error()
	for _, secret := range []string{
		config.PostgresPassword,
		config.NextPostgresPassword,
		config.ControlDBPassword,
		config.TriageDBPassword,
		config.RunnerDBPassword,
		config.ControlToken,
		config.DashboardToken,
		config.WebhookSecret,
		config.ControlGitHubToken,
		config.TriageGitHubToken,
		config.RunnerGitHubToken,
		config.githubBrokerCapability("triage"),
		config.githubBrokerCapability("runner"),
		config.ProviderToken,
	} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "***")
		}
	}
	return message
}

func hasManagedService(
	containers []lifecycle.ContainerActual,
	config config,
) bool {
	names := map[string]bool{
		config.ControlContainer:      true,
		config.GitHubBrokerContainer: true,
		config.TriageContainer:       true,
		config.RunnerContainer:       true,
		config.PostgresContainer:     true,
	}
	for _, container := range containers {
		if names[container.ID] {
			return true
		}
	}
	return false
}
