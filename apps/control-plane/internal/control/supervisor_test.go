package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSupervisionStore struct {
	mu                   sync.Mutex
	registrations        []Registration
	listError            error
	states               []string
	upsertErrorComponent string
	upsertErrorState     string
}

type filteredSupervisionStore struct {
	*fakeSupervisionStore
	unmanaged map[string]bool
}

func (store *filteredSupervisionStore) ManagesComponent(component string) bool {
	return !store.unmanaged[component]
}

func (store *fakeSupervisionStore) ListRegistrations(context.Context) ([]Registration, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]Registration(nil), store.registrations...), store.listError
}

func (store *fakeSupervisionStore) ManagesComponent(string) bool { return true }

func (store *fakeSupervisionStore) UpsertActualState(
	_ context.Context,
	registration Registration,
	component, state, _ string,
	_ error,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.states = append(store.states, componentKey(registration.ID, component)+":"+state)
	if component == store.upsertErrorComponent && state == store.upsertErrorState {
		return errors.New("actual state unavailable")
	}
	return nil
}

func (store *fakeSupervisionStore) failUpsert(component, state string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.upsertErrorComponent = component
	store.upsertErrorState = state
}

func (store *fakeSupervisionStore) set(registrations []Registration, err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.registrations = registrations
	store.listError = err
}

type fakeComponentRunner struct {
	started chan string
	stopped chan string
}

func (runner *fakeComponentRunner) Run(
	ctx context.Context,
	registration Registration,
	component string,
) error {
	key := componentKey(registration.ID, component) + ":" + string(rune(registration.Version))
	runner.started <- key
	<-ctx.Done()
	runner.stopped <- key
	return nil
}

func TestSupervisorDynamicallyReconfiguresAndFailsClosed(t *testing.T) {
	store := &fakeSupervisionStore{}
	runner := &fakeComponentRunner{
		started: make(chan string, 20),
		stopped: make(chan string, 20),
	}
	supervisor := NewSupervisor(
		store,
		runner,
		"test",
		time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registration := Registration{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, Version: 1,
	}
	store.set([]Registration{registration}, nil)
	if err := supervisor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertStarts(t, runner.started, 2)

	registration.Version = 2
	registration.IssueMonitorEnabled = false
	registration.PRMonitorEnabled = true
	store.set([]Registration{registration}, nil)
	if err := supervisor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertStops(t, runner.stopped, 2)
	assertStarts(t, runner.started, 2)

	store.set(nil, errors.New("database disconnected"))
	if err := supervisor.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile() accepted disconnected database")
	}
	assertStops(t, runner.stopped, 2)

	store.set([]Registration{registration}, nil)
	if err := supervisor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertStarts(t, runner.started, 2)
	if err := supervisor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoStart(t, runner.started)

	registration.Version = 3
	registration.Enabled = false
	store.set([]Registration{registration}, nil)
	if err := supervisor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertStops(t, runner.stopped, 2)
}

func TestSupervisorExcludesComponentsOutsideRuntimeTopology(t *testing.T) {
	baseStore := &fakeSupervisionStore{registrations: []Registration{{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, Version: 1,
	}}}
	store := &filteredSupervisionStore{
		fakeSupervisionStore: baseStore,
		unmanaged:            map[string]bool{ComponentForwarder: true},
	}
	runner := &fakeComponentRunner{
		started: make(chan string, 20),
		stopped: make(chan string, 20),
	}
	supervisor := NewSupervisor(
		store,
		runner,
		"test",
		time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStarts(t, runner.started, 1)
	assertNoStart(t, runner.started)
	baseStore.mu.Lock()
	states := append([]string(nil), baseStore.states...)
	baseStore.mu.Unlock()
	for _, state := range states {
		if strings.Contains(state, ":"+ComponentForwarder+":") {
			t.Fatalf("unmanaged forwarder received actual-state projection: %s", state)
		}
	}
	supervisor.stopAll()
	assertStops(t, runner.stopped, 1)
}

func assertNoStart(t *testing.T, channel <-chan string) {
	t.Helper()
	select {
	case started := <-channel:
		t.Fatalf("stale component owner removed a reconstructed component: %s", started)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPeriodicReconciliationRecoversMissedNotification(t *testing.T) {
	store := &fakeSupervisionStore{}
	runner := &fakeComponentRunner{
		started: make(chan string, 20),
		stopped: make(chan string, 20),
	}
	supervisor := NewSupervisor(
		store,
		runner,
		"test",
		10*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	store.set([]Registration{{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, Version: 1,
	}}, nil)
	assertStarts(t, runner.started, 2)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Supervisor.Run did not stop")
	}
}

func TestSupervisorFailsClosedAndClearsPlaceholdersWhenActualStateWriteFails(t *testing.T) {
	store := &fakeSupervisionStore{registrations: []Registration{{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, Version: 1,
	}}}
	runner := &fakeComponentRunner{
		started: make(chan string, 20),
		stopped: make(chan string, 20),
	}
	supervisor := NewSupervisor(
		store,
		runner,
		"test",
		time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	store.failUpsert(ComponentForwarder, "starting")
	if err := supervisor.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() accepted a failed starting-state write")
	}
	supervisor.mu.Lock()
	running := len(supervisor.running)
	supervisor.mu.Unlock()
	if running != 0 {
		t.Fatalf("failed reconcile retained %d running placeholders", running)
	}
	for {
		select {
		case <-runner.started:
		default:
			goto drained
		}
	}
drained:
	store.failUpsert("", "")
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStarts(t, runner.started, 2)
	supervisor.stopAll()
}

func TestSupervisorStopsExistingComponentsWhenStoppedStateWriteFails(t *testing.T) {
	store := &fakeSupervisionStore{registrations: []Registration{{
		ID: "registration-1", Repository: "owner/repo", Enabled: true,
		IssueMonitorEnabled: true, Version: 1,
	}}}
	runner := &fakeComponentRunner{
		started: make(chan string, 20),
		stopped: make(chan string, 20),
	}
	supervisor := NewSupervisor(
		store,
		runner,
		"test",
		time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assertStarts(t, runner.started, 2)
	disabled := store.registrations[0]
	disabled.Enabled = false
	disabled.Version++
	store.set([]Registration{disabled}, nil)
	store.failUpsert(ComponentPRMonitor, "stopped")
	if err := supervisor.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile() accepted a failed stopped-state write")
	}
	assertStops(t, runner.stopped, 2)
}

func assertStarts(t *testing.T, channel <-chan string, count int) {
	t.Helper()
	for range count {
		select {
		case <-channel:
		case <-time.After(time.Second):
			t.Fatalf("wanted %d component starts", count)
		}
	}
}

func assertStops(t *testing.T, channel <-chan string, count int) {
	t.Helper()
	for range count {
		select {
		case <-channel:
		case <-time.After(time.Second):
			t.Fatalf("wanted %d component stops", count)
		}
	}
}
