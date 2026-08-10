package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type SupervisionStore interface {
	ListRegistrations(context.Context) ([]Registration, error)
	ManagesComponent(string) bool
	UpsertActualState(
		context.Context,
		Registration,
		string,
		string,
		string,
		error,
	) error
}

type gateEscalationStore interface {
	ReconcileGateEscalations(context.Context, time.Time) (int64, error)
}

type ComponentRunner interface {
	Run(context.Context, Registration, string) error
}

type runningComponent struct {
	registration Registration
	component    string
	cancel       context.CancelFunc
	generation   uint64
}

type Supervisor struct {
	store       SupervisionStore
	runner      ComponentRunner
	id          string
	interval    time.Duration
	wake        chan struct{}
	log         *slog.Logger
	mu          sync.Mutex
	running     map[string]runningComponent
	reconciling sync.Mutex
	generation  uint64
	now         func() time.Time
}

func NewSupervisor(
	store SupervisionStore,
	runner ComponentRunner,
	id string,
	interval time.Duration,
	log *slog.Logger,
) *Supervisor {
	return &Supervisor{
		store: store, runner: runner, id: id, interval: interval,
		wake: make(chan struct{}, 1), log: log, running: make(map[string]runningComponent),
		now: time.Now,
	}
}

func (supervisor *Supervisor) Wake() {
	select {
	case supervisor.wake <- struct{}{}:
	default:
	}
}

func (supervisor *Supervisor) Run(ctx context.Context) error {
	ticker := time.NewTicker(supervisor.interval)
	defer ticker.Stop()
	defer supervisor.stopAll()
	for {
		if err := supervisor.Reconcile(ctx); err != nil {
			supervisor.log.Error("registration reconciliation failed closed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-supervisor.wake:
		}
	}
}

func (supervisor *Supervisor) Reconcile(ctx context.Context) error {
	supervisor.reconciling.Lock()
	defer supervisor.reconciling.Unlock()
	if escalations, supported := supervisor.store.(gateEscalationStore); supported {
		if _, err := escalations.ReconcileGateEscalations(ctx, supervisor.now()); err != nil {
			supervisor.stopAll()
			return err
		}
	}
	registrations, err := supervisor.store.ListRegistrations(ctx)
	if err != nil {
		supervisor.stopAll()
		return err
	}
	desired := make(map[string]runningComponent)
	for _, registration := range registrations {
		for _, component := range []string{
			ComponentIssueMonitor,
			ComponentPRMonitor,
			ComponentForwarder,
		} {
			if !supervisor.store.ManagesComponent(component) {
				continue
			}
			if registration.Desired(component) {
				key := componentKey(registration.ID, component)
				desired[key] = runningComponent{
					registration: registration,
					component:    component,
				}
			} else {
				if err := supervisor.store.UpsertActualState(
					ctx,
					registration,
					component,
					"stopped",
					supervisor.id,
					nil,
				); err != nil {
					supervisor.stopAll()
					return err
				}
			}
		}
	}

	supervisor.mu.Lock()
	for key, running := range supervisor.running {
		next, present := desired[key]
		if !present || next.registration.Version != running.registration.Version {
			delete(supervisor.running, key)
			running.cancel()
		}
	}
	toStart := make([]runningComponent, 0)
	for key, candidate := range desired {
		if _, present := supervisor.running[key]; present {
			continue
		}
		supervisor.generation++
		candidate.generation = supervisor.generation
		candidate.cancel = func() {}
		supervisor.running[key] = candidate
		toStart = append(toStart, candidate)
	}
	supervisor.mu.Unlock()

	for _, component := range toStart {
		if err := supervisor.store.UpsertActualState(
			ctx,
			component.registration,
			component.component,
			"starting",
			supervisor.id,
			nil,
		); err != nil {
			supervisor.stopAll()
			return err
		}
		childContext, cancel := context.WithCancel(ctx)
		supervisor.mu.Lock()
		key := componentKey(component.registration.ID, component.component)
		current, present := supervisor.running[key]
		if present &&
			current.registration.Version == component.registration.Version &&
			current.component == component.component &&
			current.generation == component.generation {
			current.cancel = cancel
			supervisor.running[key] = current
			supervisor.mu.Unlock()
			go supervisor.runComponent(childContext, key, current)
		} else {
			supervisor.mu.Unlock()
			cancel()
		}
	}
	return nil
}

func (supervisor *Supervisor) runComponent(
	ctx context.Context,
	key string,
	component runningComponent,
) {
	err := supervisor.runner.Run(ctx, component.registration, component.component)
	state := "failed"
	if ctx.Err() != nil {
		state = "stopped"
		err = nil
	}
	supervisor.mu.Lock()
	current, ownsSlot := supervisor.running[key]
	ownsSlot = ownsSlot &&
		current.registration.Version == component.registration.Version &&
		current.component == component.component &&
		current.generation == component.generation
	supervisor.mu.Unlock()
	if !ownsSlot {
		return
	}
	updateContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if updateErr := supervisor.store.UpsertActualState(
		updateContext,
		component.registration,
		component.component,
		state,
		supervisor.id,
		err,
	); updateErr != nil {
		supervisor.log.Error(
			"component terminal status was not persisted",
			"registration", component.registration.Repository,
			"component", component.component,
			"error", updateErr,
		)
	}
	supervisor.mu.Lock()
	if current, present := supervisor.running[key]; present &&
		current.registration.Version == component.registration.Version &&
		current.component == component.component &&
		current.generation == component.generation {
		delete(supervisor.running, key)
	}
	supervisor.mu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		supervisor.log.Error(
			"supervised component exited",
			"registration", component.registration.Repository,
			"component", component.component,
			"error", err,
		)
	}
}

func (supervisor *Supervisor) stopAll() {
	supervisor.mu.Lock()
	running := supervisor.running
	supervisor.running = make(map[string]runningComponent)
	supervisor.mu.Unlock()
	for _, component := range running {
		component.cancel()
	}
}

func componentKey(registrationID, component string) string {
	return fmt.Sprintf("%s:%s", registrationID, component)
}
