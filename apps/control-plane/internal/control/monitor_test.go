package control

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

func TestMonitorBrokerProductionBoundaries(t *testing.T) {
	if MonitorBrokerTerminalRetention != 7*24*time.Hour {
		t.Fatalf("terminal retention = %s", MonitorBrokerTerminalRetention)
	}
	if MonitorBrokerResponseReuse != 2*time.Minute {
		t.Fatalf("response reuse = %s", MonitorBrokerResponseReuse)
	}
	if DefaultMonitorBrokerTimeout != 25*time.Second ||
		MaxMonitorBrokerTimeout != 30*time.Second ||
		MonitorBrokerResponsePoll != 100*time.Millisecond ||
		MaxMonitorBrokerTransientRetry != 1 {
		t.Fatalf(
			"broker timing = default %s max %s poll %s retries %d",
			DefaultMonitorBrokerTimeout,
			MaxMonitorBrokerTimeout,
			MonitorBrokerResponsePoll,
			MaxMonitorBrokerTransientRetry,
		)
	}
}

type fakeMonitorStore struct {
	mu           sync.Mutex
	enqueueError error
	enqueued     int
	savedCursors int
	saveHook     func()
	received     int
	actualStates int
	actualError  error
	failActualAt int
	currentMode  lifecycle.Mode
	lifecycleErr error
	topology     RuntimeTopology
}

func (store *fakeMonitorStore) ManagesComponent(component string) bool {
	return store.topology.ManagesComponent(component)
}

func (store *fakeMonitorStore) LifecycleMode(context.Context) (lifecycle.Mode, error) {
	if store.lifecycleErr != nil {
		return "", store.lifecycleErr
	}
	if store.currentMode == "" {
		return lifecycle.ModeActive, nil
	}
	return store.currentMode, nil
}

func (store *fakeMonitorStore) MonitorCursor(
	context.Context,
	string,
	string,
) (map[string]any, error) {
	return map[string]any{}, nil
}

func (store *fakeMonitorStore) SaveMonitorCursor(
	context.Context,
	string,
	string,
	map[string]any,
	time.Time,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.savedCursors++
	if store.saveHook != nil {
		store.saveHook()
	}
	return nil
}

func (store *fakeMonitorStore) EnqueueWork(
	context.Context,
	Registration,
	string,
	string,
	WorkItem,
) (string, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.enqueued++
	return "", false, store.enqueueError
}

func (store *fakeMonitorStore) ReceiveWebhook(
	context.Context,
	string,
	string,
	string,
	*string,
	map[string]string,
	map[string]any,
) (WebhookReceipt, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.received++
	return WebhookReceipt{DeliveryID: "delivery-1", Status: "pending"}, nil
}

func (store *fakeMonitorStore) UpsertActualState(
	context.Context,
	Registration,
	string,
	string,
	string,
	error,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.actualStates++
	if store.actualError != nil && store.actualStates >= store.failActualAt {
		return store.actualError
	}
	return nil
}

type fakeMonitorSource struct {
	items  []WorkItem
	calls  *int
	errors []error
}

func (source fakeMonitorSource) Poll(
	context.Context,
	Registration,
	string,
	map[string]any,
) ([]WorkItem, map[string]any, time.Time, error) {
	call := 0
	if source.calls != nil {
		(*source.calls)++
		call = *source.calls
	}
	if call > 0 && call <= len(source.errors) && source.errors[call-1] != nil {
		return nil, nil, time.Time{}, source.errors[call-1]
	}
	return source.items, map[string]any{"updatedAfter": "next"}, time.Now(), nil
}

func TestMonitorRetriesOneTransientBrokerProviderFailureWithoutAdvancingCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeMonitorStore{saveHook: cancel}
	polls := 0
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{
			calls:  &polls,
			errors: []error{ErrMonitorBrokerTransientProvider},
		},
		SupervisorID:   "test",
		PollInterval:   time.Hour,
		TransientRetry: time.Millisecond,
	}
	err := runner.runMonitor(ctx, Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		ExecutionEnabled:    true,
		Version:             1,
	}, "issue", ComponentIssueMonitor)
	if err != nil {
		t.Fatalf("runMonitor() error = %v", err)
	}
	if polls != 2 || store.savedCursors != 1 || store.actualStates != 2 {
		t.Fatalf(
			"polls=%d cursors=%d states=%d, want retry then one durable success",
			polls,
			store.savedCursors,
			store.actualStates,
		)
	}
}

func TestMonitorSurfacesRepeatedTransientBrokerProviderFailure(t *testing.T) {
	store := &fakeMonitorStore{}
	polls := 0
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{
			calls: &polls,
			errors: []error{
				ErrMonitorBrokerTransientProvider,
				ErrMonitorBrokerTransientProvider,
			},
		},
		SupervisorID:   "test",
		PollInterval:   time.Hour,
		TransientRetry: time.Millisecond,
	}
	err := runner.runMonitor(context.Background(), Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		ExecutionEnabled:    true,
		Version:             1,
	}, "issue", ComponentIssueMonitor)
	if !errors.Is(err, ErrMonitorBrokerTransientProvider) {
		t.Fatalf("runMonitor() error = %v, want transient provider failure", err)
	}
	if polls != 2 || store.savedCursors != 0 || store.actualStates != 1 {
		t.Fatalf(
			"polls=%d cursors=%d states=%d, want one bounded retry then failure",
			polls,
			store.savedCursors,
			store.actualStates,
		)
	}
}

func TestMonitorBrokerTransientProviderClassificationIsExact(t *testing.T) {
	for _, code := range []string{"provider_timeout", "provider_failure"} {
		failure := monitorBrokerFailure(
			code,
			"typed provider operation failed",
		)
		if !errors.Is(failure, ErrMonitorBrokerTransientProvider) {
			t.Fatalf("%s was not classified for bounded retry", code)
		}
	}
	other := monitorBrokerFailure("invalid_json", "GitHub response was invalid")
	if errors.Is(other, ErrMonitorBrokerTransientProvider) {
		t.Fatal("non-transient broker failure was classified as retryable")
	}
}

func TestMonitorDoesNotAdvanceCursorWhenSingleFlightIsBusy(t *testing.T) {
	store := &fakeMonitorStore{enqueueError: ErrRepositoryBusy}
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{items: []WorkItem{{
			Repository: "owner/repo",
			Kind:       "issue",
			Number:     1,
			UpdatedAt:  time.Now(),
		}}},
		SupervisorID: "test",
	}
	err := runner.pollOnce(context.Background(), Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		ExecutionEnabled:    true,
		Version:             1,
	}, "issue", ComponentIssueMonitor)
	if err != nil {
		t.Fatalf("pollOnce() error = %v", err)
	}
	if store.savedCursors != 0 {
		t.Fatal("busy poll advanced its cursor and could lose work")
	}
	if store.actualStates != 1 {
		t.Fatal("single-flight backpressure was projected as monitor failure")
	}
}

func TestPRIntentActiveStartupFenceDoesNotFailOrAdvanceMonitor(t *testing.T) {
	store := &fakeMonitorStore{enqueueError: ErrOperatingMode}
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{items: []WorkItem{{
			Repository: "owner/repo",
			Kind:       "pull_request",
			Number:     41,
			UpdatedAt:  time.Now(),
		}}},
		SupervisorID: "test",
	}
	err := runner.pollOnce(context.Background(), Registration{
		ID:               "registration-1",
		Repository:       "owner/repo",
		Enabled:          true,
		PRMonitorEnabled: true,
		ExecutionEnabled: true,
		Version:          1,
	}, "pull_request", ComponentPRMonitor)
	if err != nil {
		t.Fatalf("startup lifecycle fence was projected as monitor failure: %v", err)
	}
	if store.savedCursors != 0 {
		t.Fatal("startup lifecycle fence advanced the cursor before ACTIVE")
	}
	if store.actualStates != 1 {
		t.Fatal("startup lifecycle fence did not keep monitor health current")
	}
}

func TestExecutionDisabledMonitorPersistsObservationWithoutEnqueue(t *testing.T) {
	store := &fakeMonitorStore{}
	polls := 0
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{
			calls: &polls,
			items: []WorkItem{{
				Repository: "owner/repo",
				Kind:       "issue",
				Number:     1,
				UpdatedAt:  time.Now(),
			}},
		},
		SupervisorID: "test",
	}
	err := runner.pollOnce(context.Background(), Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		ExecutionEnabled:    false,
		Version:             1,
	}, "issue", ComponentIssueMonitor)
	if err != nil {
		t.Fatal(err)
	}
	if polls != 1 || store.enqueued != 0 || store.savedCursors != 0 ||
		store.actualStates != 1 {
		t.Fatalf(
			"polls=%d enqueued=%d cursors=%d actual=%d",
			polls,
			store.enqueued,
			store.savedCursors,
			store.actualStates,
		)
	}
}

func TestMonitorOnlyPersistsObservationWithoutEnqueue(t *testing.T) {
	store := &fakeMonitorStore{currentMode: lifecycle.ModeMonitorOnly}
	polls := 0
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{
			calls: &polls,
			items: []WorkItem{{
				Repository: "owner/repo",
				Kind:       "issue",
				Number:     1,
				UpdatedAt:  time.Now(),
			}},
		},
		SupervisorID: "test",
	}
	err := runner.pollOnce(context.Background(), Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		ExecutionEnabled:    true,
		Version:             1,
	}, "issue", ComponentIssueMonitor)
	if err != nil {
		t.Fatal(err)
	}
	if polls != 1 || store.enqueued != 0 || store.savedCursors != 0 ||
		store.actualStates != 1 {
		t.Fatalf(
			"polls=%d enqueued=%d cursors=%d actual=%d",
			polls,
			store.enqueued,
			store.savedCursors,
			store.actualStates,
		)
	}
}

func TestMonitorFailsClosedWhenLifecycleAuthorityIsUnavailable(t *testing.T) {
	store := &fakeMonitorStore{lifecycleErr: ErrStoreUnavailable}
	polls := 0
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{
			calls: &polls,
			items: []WorkItem{{
				Repository: "owner/repo", Kind: "issue", Number: 1,
				UpdatedAt: time.Now(),
			}},
		},
		SupervisorID: "test",
	}
	err := runner.pollOnce(context.Background(), Registration{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, ExecutionEnabled: true, Version: 1,
	}, "issue", ComponentIssueMonitor)
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("pollOnce() error = %v, want store unavailable", err)
	}
	if polls != 1 || store.enqueued != 0 || store.savedCursors != 0 ||
		store.actualStates != 0 {
		t.Fatalf(
			"polls=%d enqueued=%d cursors=%d actual=%d",
			polls,
			store.enqueued,
			store.savedCursors,
			store.actualStates,
		)
	}
}

func TestMonitorOnlyObservationIsReplayedAfterActiveCutover(t *testing.T) {
	store := &fakeMonitorStore{currentMode: lifecycle.ModeMonitorOnly}
	polls := 0
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{
			calls: &polls,
			items: []WorkItem{{
				Repository: "owner/repo",
				Kind:       "issue",
				Number:     7,
				UpdatedAt:  time.Now(),
			}},
		},
		SupervisorID: "test",
	}
	registration := Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		ExecutionEnabled:    true,
		Version:             1,
	}
	if err := runner.pollOnce(
		context.Background(),
		registration,
		"issue",
		ComponentIssueMonitor,
	); err != nil {
		t.Fatal(err)
	}
	store.currentMode = lifecycle.ModeActive
	if err := runner.pollOnce(
		context.Background(),
		registration,
		"issue",
		ComponentIssueMonitor,
	); err != nil {
		t.Fatal(err)
	}
	if polls != 2 || store.enqueued != 1 || store.savedCursors != 1 ||
		store.actualStates != 2 {
		t.Fatalf(
			"polls=%d enqueued=%d cursors=%d actual=%d",
			polls,
			store.enqueued,
			store.savedCursors,
			store.actualStates,
		)
	}
}

func TestDrainingLifecyclePausesActiveStartupWithoutAdvancingCursor(t *testing.T) {
	store := &fakeMonitorStore{currentMode: lifecycle.ModeDraining}
	runner := &ProductionRunner{
		Store: store,
		Source: fakeMonitorSource{items: []WorkItem{{
			Repository: "owner/repo",
			Kind:       "issue",
			Number:     9,
			UpdatedAt:  time.Now(),
		}}},
		SupervisorID: "test",
	}
	err := runner.pollOnce(context.Background(), Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		ExecutionEnabled:    true,
		Version:             1,
	}, "issue", ComponentIssueMonitor)
	if err != nil {
		t.Fatal(err)
	}
	if store.enqueued != 0 || store.savedCursors != 0 || store.actualStates != 1 {
		t.Fatalf(
			"DRAINING lifecycle enqueued=%d cursors=%d actual=%d",
			store.enqueued,
			store.savedCursors,
			store.actualStates,
		)
	}
}

func TestForwarderEnvironmentExcludesControlPlaneSecrets(t *testing.T) {
	environment := sanitizedForwarderEnvironment([]string{
		"PATH=/bin",
		"HOME=/home/agentops",
		"GH_TOKEN=github-token",
		"AGENTOPS_DATABASE_URL=postgres://secret",
		"AGENTOPS_CONTROL_TOKEN=control-secret",
		"AGENTOPS_GITHUB_WEBHOOK_SECRET=webhook-secret",
		"UNRELATED_SECRET=secret",
	})
	joined := strings.Join(environment, "\n")
	for _, expected := range []string{"PATH=/bin", "HOME=/home/agentops", "GH_TOKEN=github-token"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("sanitized environment omitted %s", expected)
		}
	}
	for _, forbidden := range []string{
		"AGENTOPS_DATABASE_URL",
		"AGENTOPS_CONTROL_TOKEN",
		"AGENTOPS_GITHUB_WEBHOOK_SECRET",
		"UNRELATED_SECRET",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sanitized environment leaked %s", forbidden)
		}
	}
}

func TestGitHubPollCursorUsesChronologicalNotLexicalTimestampOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(
			writer,
			`[{"number":13,"updated_at":"2026-07-25T00:00:00.500Z"}]`,
		)
	}))
	defer server.Close()
	source := GitHubSource{Client: server.Client(), BaseURL: server.URL}
	items, cursor, _, err := source.Poll(
		context.Background(),
		Registration{Repository: "owner/repo"},
		"issue",
		map[string]any{"updatedAfter": "2026-07-25T00:00:00Z"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || cursor["updatedAfter"] != "2026-07-25T00:00:00.5Z" {
		t.Fatalf("items=%#v cursor=%#v", items, cursor)
	}
}

type fakeCommand struct {
	stdout io.ReadCloser
	stderr io.ReadCloser
	wait   error
}

func (command *fakeCommand) StdoutPipe() (io.ReadCloser, error) { return command.stdout, nil }
func (command *fakeCommand) StderrPipe() (io.ReadCloser, error) { return command.stderr, nil }
func (command *fakeCommand) Start() error                       { return nil }
func (command *fakeCommand) Wait() error                        { return command.wait }

func TestForwarderRestartsAndPersistsBeforeContinuing(t *testing.T) {
	store := &fakeMonitorStore{topology: RuntimeTopologyLegacyCLIForwarder}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	commands := 0
	factory := func(context.Context, string, ...string) Command {
		commands++
		if commands == 2 {
			cancel()
		}
		payload := `{"action":"opened","repository":{"full_name":"owner/repo"},` +
			`"issue":{"number":1,"updated_at":"2026-07-25T00:00:00Z"}}` + "\n"
		return &fakeCommand{
			stdout: io.NopCloser(strings.NewReader(payload)),
			stderr: io.NopCloser(strings.NewReader("forwarder exited\n")),
			wait:   &fs.PathError{Op: "wait", Path: "gh", Err: errors.New("exit 1")},
		}
	}
	runner := &ProductionRunner{
		Store:          store,
		SupervisorID:   "test",
		ForwarderRetry: time.Millisecond,
		Command:        factory,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := runner.runForwarder(ctx, Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		Version:             1,
	})
	if err != nil {
		t.Fatalf("runForwarder() error = %v", err)
	}
	if commands != 2 {
		t.Fatalf("forwarder commands = %d, want restart", commands)
	}
	if store.received != 2 {
		t.Fatalf("persisted deliveries = %d, want 2", store.received)
	}
}

func TestForwarderRejectsRepositoryIdentityMismatch(t *testing.T) {
	store := &fakeMonitorStore{}
	runner := &ProductionRunner{
		Store: store,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := runner.scanForwarder(
		context.Background(),
		Registration{Repository: "owner/repo", Enabled: true, IssueMonitorEnabled: true},
		strings.NewReader(
			`{"repository":{"full_name":"attacker/repo"},"issue":{"number":1}}`+"\n",
		),
	)
	if err == nil {
		t.Fatal("scanForwarder() accepted mismatched repository")
	}
	if store.received != 0 {
		t.Fatal("mismatched payload was persisted")
	}
}

func TestForwarderHeartbeatKeepsActualStateFresh(t *testing.T) {
	store := &fakeMonitorStore{}
	runner := &ProductionRunner{
		Store:          store,
		SupervisorID:   "test",
		HealthInterval: time.Millisecond,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := runner.heartbeatForwarder(ctx, Registration{
		ID:         "registration-1",
		Repository: "owner/repo",
		Version:    1,
	}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	actualStates := store.actualStates
	store.mu.Unlock()
	if actualStates == 0 {
		t.Fatal("long-running forwarder did not refresh actual state")
	}
}

func TestPRIntentSignedWebhookIngressNeverExecutesControlSideGH(t *testing.T) {
	t.Run("PR-INTENT credential-free signed webhook ingress", func(t *testing.T) {
		store := &fakeMonitorStore{topology: RuntimeTopologySignedWebhookIngress}
		commands := 0
		runner := &ProductionRunner{
			Store:          store,
			SupervisorID:   "test",
			HealthInterval: time.Millisecond,
			Command: func(context.Context, string, ...string) Command {
				commands++
				return nil
			},
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := runner.runForwarder(ctx, Registration{
			ID:                  "registration-1",
			Repository:          "owner/repo",
			Enabled:             true,
			IssueMonitorEnabled: true,
			Version:             1,
		}); err != nil {
			t.Fatal(err)
		}
		if commands != 0 {
			t.Fatalf("credential-free control executed %d gh commands", commands)
		}
		store.mu.Lock()
		actualStates := store.actualStates
		store.mu.Unlock()
		if actualStates != 0 {
			t.Fatalf("signed webhook ingress fabricated %d forwarder health updates", actualStates)
		}
	})
}

func TestForwarderStopsWhenHeartbeatCannotBePersisted(t *testing.T) {
	persistError := errors.New("control store disconnected")
	store := &fakeMonitorStore{
		actualError:  persistError,
		failActualAt: 2,
		topology:     RuntimeTopologyLegacyCLIForwarder,
	}
	commandCancelled := make(chan struct{})
	factory := func(ctx context.Context, _ string, _ ...string) Command {
		reader, writer := io.Pipe()
		go func() {
			<-ctx.Done()
			close(commandCancelled)
			_ = writer.Close()
		}()
		return &fakeCommand{
			stdout: reader,
			stderr: io.NopCloser(strings.NewReader("")),
			wait:   context.Canceled,
		}
	}
	runner := &ProductionRunner{
		Store:          store,
		SupervisorID:   "test",
		HealthInterval: time.Millisecond,
		Command:        factory,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := runner.runForwarder(context.Background(), Registration{
		ID:                  "registration-1",
		Repository:          "owner/repo",
		Enabled:             true,
		IssueMonitorEnabled: true,
		Version:             1,
	})
	if !errors.Is(err, persistError) {
		t.Fatalf("runForwarder() error = %v, want %v", err, persistError)
	}
	select {
	case <-commandCancelled:
	default:
		t.Fatal("forwarder child was left running after heartbeat persistence failed")
	}
}
