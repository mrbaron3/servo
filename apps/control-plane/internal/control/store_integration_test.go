package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	lifecyclestore "github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
	"github.com/mrbaron3/servo/apps/control-plane/internal/reporoot"
)

func TestExpectedMigrationsMatchControlSchemaVersion(t *testing.T) {
	root := testRepositoryRoot(t)
	migrations, err := expectedMigrations(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != ControlSchemaVersion {
		t.Fatalf(
			"migration count=%d control schema version=%d",
			len(migrations),
			ControlSchemaVersion,
		)
	}
}

func TestBootstrapRolesMatchesCurrentMonitorAndReviewCapabilities(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	if err := lifecyclestore.BootstrapRoles(
		ctx,
		databaseURL,
		strings.Repeat("c", 32),
		strings.Repeat("t", 32),
		strings.Repeat("r", 32),
	); err != nil {
		t.Fatal(err)
	}
	var triageClaim, runnerClaim, runnerReview, runnerLineage, oldClaimAbsent bool
	if err := pool.QueryRow(ctx, `
		SELECT
		  has_function_privilege(
		    'agentops_triage',
		    'agentops_control.claim_monitor_broker_request(text,uuid,integer)',
		    'EXECUTE'
		  ),
		  has_function_privilege(
		    'agentops_runner',
		    'agentops_control.claim_monitor_broker_request(text,uuid,integer)',
		    'EXECUTE'
		  ),
		  has_function_privilege(
		    'agentops_runner',
		    'agentops_control.record_development_review_round(uuid,text,jsonb)',
		    'EXECUTE'
		  ),
		  has_table_privilege(
		    'agentops_runner',
		    'agentops_control.development_lineage_nodes',
		    'SELECT'
		  ),
		  to_regprocedure(
		    'agentops_control.claim_monitor_broker_request(text,text[],uuid,integer)'
		  ) IS NULL`).Scan(
		&triageClaim,
		&runnerClaim,
		&runnerReview,
		&runnerLineage,
		&oldClaimAbsent,
	); err != nil {
		t.Fatal(err)
	}
	if !triageClaim || runnerClaim || !runnerReview || !runnerLineage ||
		!oldClaimAbsent {
		t.Fatalf(
			"role boundary triageClaim=%t runnerClaim=%t runnerReview=%t runnerLineage=%t oldClaimAbsent=%t",
			triageClaim,
			runnerClaim,
			runnerReview,
			runnerLineage,
			oldClaimAbsent,
		)
	}
}

func TestGateEscalationIsDurableOneShotAndResolvesOnAdvance(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	store := &Store{pool: pool}
	configuration := json.RawMessage(`{"gateTimeoutSeconds":{"review":60}}`)
	registrations, err := store.ListRegistrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations) != 1 || registrations[0].Repository != "mrbaron3/servo" {
		t.Fatalf("fresh registrations = %#v, want only mrbaron3/servo", registrations)
	}
	if !strings.Contains(string(registrations[0].Configuration), `"releaseEvidence"`) {
		t.Fatalf("fresh Servo registration lacks release evidence policy: %s", registrations[0].Configuration)
	}
	registration, err := store.UpdateRegistration(
		ctx,
		registrations[0].ID,
		registrations[0].Version,
		RegistrationPatch{Configuration: configuration},
		"gate-escalation-registration",
		"integration",
	)
	if err != nil {
		t.Fatal(err)
	}
	entered := time.Date(2026, 8, 4, 3, 0, 0, 0, time.UTC)
	head := "0123456789012345678901234567890123456789"
	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = setup.Rollback(ctx) }()
	if _, err := setup.Exec(ctx, `
		INSERT INTO agentops_control.jobs(
		  id, registration_id, registration_version, source_kind, source_key,
		  idempotency_key, job_type, payload, status
		) VALUES (
		  '20000000-0000-4000-8000-000000000019', $1, $2, 'manual',
		  'servo-review', 'servo-review', 'agentops.runner', '{}', 'leased'
		)`, registration.ID, registration.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `
		INSERT INTO agentops_control.job_attempts(
		  id, job_id, attempt_number, worker_id, status, started_at
		) VALUES (
		  '30000000-0000-4000-8000-000000000019',
		  '20000000-0000-4000-8000-000000000019', 1, 'servo-runner',
		  'running', $1
		)`, entered); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `
		INSERT INTO agentops_control.job_leases(
		  id, job_id, attempt_id, lease_token, worker_id, status,
		  acquired_at, heartbeat_at, expires_at
		) VALUES (
		  '40000000-0000-4000-8000-000000000019',
		  '20000000-0000-4000-8000-000000000019',
		  '30000000-0000-4000-8000-000000000019',
		  '50000000-0000-4000-8000-000000000019', 'servo-runner', 'active',
		  $1::timestamptz, $1::timestamptz,
		  $1::timestamptz + interval '3 hours'
		)`, entered); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `
		INSERT INTO agentops_control.development_progress_events(
		  registration_id, registration_version, job_id, attempt_id,
		  repository, subject_kind, subject_number, worker_id, event_key,
		  phase, step, state, next_gate, head_sha, review_round,
		  review_outcome, gate_key, occurred_at
		) VALUES (
		  $1, $2, '20000000-0000-4000-8000-000000000019',
		  '30000000-0000-4000-8000-000000000019', 'mrbaron3/servo',
		  'issue', 19, 'servo-runner', 'review:round-1:start', 'review',
		  'perspective review panel', 'running', 'panel verdict', $4, 1,
		  'running', 'review', $3
		)`, registration.ID, registration.Version, entered, head); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if count, err := store.ReconcileGateEscalations(
		ctx, entered.Add(59*time.Second),
	); err != nil || count != 0 {
		t.Fatalf("pre-SLA reconciliation count=%d err=%v", count, err)
	}
	if count, err := store.ReconcileGateEscalations(
		ctx, entered.Add(60*time.Second),
	); err != nil || count != 1 {
		t.Fatalf("SLA reconciliation count=%d err=%v", count, err)
	}
	if count, err := store.ReconcileGateEscalations(
		ctx, entered.Add(2*time.Hour),
	); err != nil || count != 0 {
		t.Fatalf("one-shot reconciliation count=%d err=%v", count, err)
	}
	issue := int64(19)
	progress, err := store.DevelopmentProgress(ctx, "mrbaron3/servo", &issue, 10)
	if err != nil || len(progress) != 1 ||
		progress[0].KanbanLane != "human-escalated" ||
		progress[0].EscalationID == nil || progress[0].HeadSHA == nil ||
		*progress[0].HeadSHA != head || progress[0].HumanAction == nil ||
		len(progress[0].EscalationEvidence) == 0 {
		t.Fatalf("escalated progress=%#v err=%v", progress, err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO agentops_control.development_progress_events(
		  registration_id, registration_version, job_id, attempt_id,
		  repository, subject_kind, subject_number, worker_id, event_key,
		  phase, step, state, head_sha, review_round, review_outcome, occurred_at
		) VALUES (
		  $1, $2, '20000000-0000-4000-8000-000000000019',
		  '30000000-0000-4000-8000-000000000019', 'mrbaron3/servo',
		  'issue', 19, 'servo-runner', 'review:round-1:passed', 'review',
		  'all perspectives passed', 'succeeded', $3, 1, 'approve', $4
		)`, registration.ID, registration.Version, head, entered.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileGateEscalations(
		ctx, entered.Add(62*time.Second),
	); err != nil || count != 0 {
		t.Fatalf("advance reconciliation count=%d err=%v", count, err)
	}
	var unresolved int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM agentops_control.human_escalations
		 WHERE resolved_at IS NULL
	`).Scan(&unresolved); err != nil || unresolved != 0 {
		t.Fatalf("unresolved escalation count=%d err=%v", unresolved, err)
	}
}

func TestQueuedReadyIssueProjectsBeforeTheFirstWorkerEvent(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	store := &Store{pool: pool}
	registrations, err := store.ListRegistrations(ctx)
	if err != nil || len(registrations) != 1 {
		t.Fatalf("registrations = %#v, %v", registrations, err)
	}
	registration := registrations[0]
	if _, err := pool.Exec(ctx, `
		UPDATE agentops_control.lifecycle_state
		   SET mode = 'ACTIVE', generation = generation + 1,
		       updated_at = clock_timestamp()
		 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agentops_control.jobs(
		  id, registration_id, registration_version, source_kind, source_key,
		  idempotency_key, job_type, payload, status
		) VALUES (
		  '20000000-0000-4000-8000-000000000090', $1, $2, 'poll',
		  'servo-ready-90', 'servo-ready-90', 'agentops.triage',
		  '{"schemaVersion":1,"repository":{"owner":"mrbaron3","name":"servo"},"issue":{"number":90,"observedUpdatedAt":"2026-08-04T00:00:00Z"}}',
		  'queued'
		)`, registration.ID, registration.Version); err != nil {
		t.Fatal(err)
	}
	issue := int64(90)
	progress, err := store.DevelopmentProgress(ctx, registration.Repository, &issue, 10)
	if err != nil || len(progress) != 1 || progress[0].KanbanLane != "ready" ||
		progress[0].AttemptID != "" || progress[0].WorkerID != "unassigned" {
		t.Fatalf("pre-event CLI progress = %#v, %v", progress, err)
	}
	projections, err := store.Projections(ctx, time.Minute)
	if err != nil || len(projections) != 1 ||
		projections[0].Mode != lifecyclestore.ModeActive ||
		len(projections[0].DevelopmentProgress) != 1 ||
		projections[0].DevelopmentProgress[0].Current.KanbanLane != "ready" {
		t.Fatalf("pre-event dashboard projection = %#v, %v", projections, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE agentops_control.lifecycle_state
		   SET mode = 'DRAINING', generation = generation + 1,
		       updated_at = clock_timestamp()
		 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	standardStore := &Store{
		pool: pool, topology: RuntimeTopologySignedWebhookIngress,
	}
	projections, err = standardStore.Projections(ctx, time.Minute)
	if err != nil || len(projections) != 1 ||
		projections[0].Mode != lifecyclestore.ModeDraining {
		t.Fatalf("draining dashboard projection = %#v, %v", projections, err)
	}
	if _, present := projections[0].Components[ComponentForwarder]; present {
		t.Fatalf("standard signed-ingress projection exposed legacy forwarder: %#v", projections[0])
	}
}

func TestPostgresLifecycleTransitionIntegration(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	store, err := lifecyclestore.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deadline := time.Now().UTC().Add(time.Minute).Round(time.Microsecond)
	details := map[string]any{"test": true}
	type transitionResult struct {
		transition lifecyclestore.Transition
		state      lifecyclestore.State
		err        error
	}
	results := make([]transitionResult, 2)
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index].transition, results[index].state, results[index].err =
				store.Transition(
					ctx,
					"integration",
					"active-to-draining",
					lifecyclestore.ModeDraining,
					&deadline,
					details,
				)
		}(index)
	}
	wait.Wait()
	transition := results[0].transition
	if transition.Replayed {
		transition = results[1].transition
	}
	for index, result := range results {
		if result.err != nil ||
			result.transition.ID != transition.ID ||
			result.state.Mode != lifecyclestore.ModeDraining {
			t.Fatalf("concurrent transition[%d]=%#v state=%#v err=%v",
				index, result.transition, result.state, result.err)
		}
	}
	if results[0].transition.Replayed == results[1].transition.Replayed ||
		transition.Status != "applied" {
		t.Fatalf("same-key callers did not converge: %#v", results)
	}
	replay, replayState, err := store.Transition(
		ctx,
		"integration",
		"active-to-draining",
		lifecyclestore.ModeDraining,
		&deadline,
		details,
	)
	if err != nil || !replay.Replayed || replay.ID != transition.ID ||
		replayState.DrainDeadlineAt == nil ||
		!replayState.DrainDeadlineAt.Equal(deadline) {
		t.Fatalf("transition replay=%#v state=%#v err=%v", replay, replayState, err)
	}
	conflictingDeadline := deadline.Add(time.Second)
	for name, candidate := range map[string]struct {
		actor    string
		deadline *time.Time
		details  map[string]any
	}{
		"actor":    {"other-actor", &deadline, details},
		"deadline": {"integration", &conflictingDeadline, details},
		"details":  {"integration", &deadline, map[string]any{"test": false}},
	} {
		if _, _, err := store.Transition(
			ctx,
			candidate.actor,
			"active-to-draining",
			lifecyclestore.ModeDraining,
			candidate.deadline,
			candidate.details,
		); !errors.Is(err, lifecyclestore.ErrIdempotencyConflict) {
			t.Fatalf("%s idempotency conflict = %v", name, err)
		}
	}
	if _, current, err := store.Transition(
		ctx,
		"integration",
		"invalid-draining-active",
		lifecyclestore.ModeActive,
		nil,
		nil,
	); err == nil || current.Mode != lifecyclestore.ModeDraining {
		t.Fatalf("invalid transition state=%#v err=%v", current, err)
	}
	if _, _, err := store.Transition(
		ctx,
		"integration",
		"draining-monitor",
		lifecyclestore.ModeMonitorOnly,
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Transition(
		ctx,
		"integration",
		"active-to-draining",
		lifecyclestore.ModeDraining,
		&deadline,
		details,
	); !errors.Is(err, lifecyclestore.ErrStaleReplay) {
		t.Fatalf("old transition replay was not rejected as stale: %v", err)
	}
	if _, _, err := store.Transition(
		ctx,
		"integration",
		"monitor-active",
		lifecyclestore.ModeActive,
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Transition(
		ctx,
		"integration",
		"invalid-draining-active",
		lifecyclestore.ModeActive,
		nil,
		nil,
	); err == nil {
		t.Fatal("rejected transition replay unexpectedly succeeded")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agentops_control.repository_registrations(id, repository)
		VALUES ('10000000-0000-4000-8000-000000000001', 'owner/recovery');
		INSERT INTO agentops_control.jobs(
		  id, registration_id, registration_version, source_kind, source_key,
		  idempotency_key, job_type, payload, status
		) VALUES (
		  '20000000-0000-4000-8000-000000000001',
		  '10000000-0000-4000-8000-000000000001',
		  1, 'recovery', 'expired-attempt', 'expired-attempt',
		  'agentops.runner', '{}', 'leased'
		);
		INSERT INTO agentops_control.job_attempts(
		  id, job_id, attempt_number, worker_id, status
		) VALUES (
		  '30000000-0000-4000-8000-000000000001',
		  '20000000-0000-4000-8000-000000000001',
		  1, 'crashed-runner', 'running'
		);
		INSERT INTO agentops_control.job_leases(
		  id, job_id, attempt_id, lease_token, worker_id, status,
		  acquired_at, heartbeat_at, expires_at
		) VALUES (
		  '40000000-0000-4000-8000-000000000001',
		  '20000000-0000-4000-8000-000000000001',
		  '30000000-0000-4000-8000-000000000001',
		  '50000000-0000-4000-8000-000000000001',
		  'crashed-runner', 'active',
		  clock_timestamp() - interval '2 minutes',
		  clock_timestamp() - interval '2 minutes',
		  clock_timestamp() - interval '1 minute'
		)
	`); err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.ReconcileExpiredRunnerWork(
		ctx,
		3,
		5*time.Second,
	)
	if err != nil || reconciled != 1 {
		t.Fatalf("expired recovery reconciled=%d err=%v", reconciled, err)
	}
	var jobStatus, attemptStatus, leaseStatus string
	if err := pool.QueryRow(ctx, `
		SELECT j.status, a.status, l.status
		  FROM agentops_control.jobs j
		  JOIN agentops_control.job_attempts a ON a.job_id = j.id
		  JOIN agentops_control.job_leases l ON l.attempt_id = a.id
		 WHERE j.id = '20000000-0000-4000-8000-000000000001'
	`).Scan(&jobStatus, &attemptStatus, &leaseStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "queued" ||
		attemptStatus != "timed_out" ||
		leaseStatus != "expired" {
		t.Fatalf(
			"recovered states job=%s attempt=%s lease=%s",
			jobStatus,
			attemptStatus,
			leaseStatus,
		)
	}
	if err := store.RecordFailure(
		ctx,
		"integration",
		"drain",
		"deadline reached",
		true,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(ctx)
	if err != nil || !status.State.DrainTimedOut ||
		status.State.LastError == nil || len(status.RecentTransitions) < 4 {
		t.Fatalf("lifecycle status=%#v err=%v", status, err)
	}
}

func TestMonitorBrokerRequestAndOriginAuditAreAtomic(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	const registrationID = "10000000-0000-4000-8000-000000000017"
	if _, err := pool.Exec(ctx, `
		INSERT INTO agentops_control.repository_registrations(
		  id, repository, enabled, issue_monitor_enabled,
		  pr_monitor_enabled, execution_enabled
		) VALUES ($1, 'acme/widgets', true, true, true, true)
	`, registrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION agentops_control.reject_monitor_request_audit()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.event_type = 'monitor.broker.requested' THEN
		    RAISE EXCEPTION 'injected origin audit failure';
		  END IF;
		  RETURN NEW;
		END $$
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER reject_monitor_request_audit
		  BEFORE INSERT ON agentops_control.runtime_audit
		  FOR EACH ROW EXECUTE FUNCTION
		    agentops_control.reject_monitor_request_audit()
	`); err != nil {
		t.Fatal(err)
	}
	source := BrokeredGitHubSource{
		Store:   &Store{pool: pool},
		Timeout: time.Second,
	}
	_, _, _, err = source.Poll(ctx, Registration{
		ID:                  registrationID,
		Repository:          "acme/widgets",
		Enabled:             true,
		IssueMonitorEnabled: true,
		PRMonitorEnabled:    true,
		ExecutionEnabled:    true,
		Version:             1,
	}, "issue", nil)
	if err == nil || !strings.Contains(err.Error(), "injected origin audit failure") {
		t.Fatalf("atomic broker request did not surface audit failure: %v", err)
	}
	var requests int
	if err := pool.QueryRow(
		ctx,
		`SELECT count(*) FROM agentops_control.monitor_broker_requests`,
	).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("broker request survived without its origin audit: %d", requests)
	}
}

func TestPostgresAdministratorRotationFencesAndInvalidatesOldCredential(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.User == nil || parsed.User.Username() != "postgres" {
		t.Skip("integration database is not the postgres administrator login")
	}
	currentPassword, present := parsed.User.Password()
	if !present {
		t.Skip("integration database URL has no password")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	if _, err := pool.Exec(ctx, `
		UPDATE agentops_control.lifecycle_state
		   SET mode = 'DRAINING', generation = generation + 1,
		       updated_at = clock_timestamp()
		 WHERE singleton
	`); err != nil {
		t.Fatal(err)
	}
	nextPassword := strings.Repeat("ciso07-next-admin-", 3)
	nextURL := *parsed
	nextURL.User = url.UserPassword("postgres", nextPassword)
	rotated := false
	defer func() {
		if !rotated {
			return
		}
		cleanup, cleanupErr := pgxpool.New(ctx, nextURL.String())
		if cleanupErr == nil {
			escaped := strings.ReplaceAll(currentPassword, "'", "''")
			_, cleanupErr = cleanup.Exec(
				ctx,
				`ALTER ROLE postgres PASSWORD '`+escaped+`'`,
			)
			cleanup.Close()
		}
		if cleanupErr != nil {
			t.Errorf("restore integration postgres credential: %v", cleanupErr)
		}
	}()
	if err := lifecyclestore.RotatePostgresAdmin(
		ctx,
		databaseURL,
		nextPassword,
		"ciso07-integration-rotation",
	); err != nil {
		t.Fatal(err)
	}
	rotated = true
	nextStore, err := lifecyclestore.Open(ctx, nextURL.String())
	if err != nil {
		t.Fatalf("new PostgreSQL administrator credential failed: %v", err)
	}
	nextStore.Close()
	if oldStore, oldErr := lifecyclestore.Open(ctx, databaseURL); oldErr == nil {
		oldStore.Close()
		t.Fatal("old PostgreSQL administrator credential still authenticated")
	}
}

func TestPostgresDirectInsertLifecycleFenceIntegration(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	store, err := lifecyclestore.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agentops_control.repository_registrations(id, repository)
		VALUES ('11000000-0000-4000-8000-000000000001', 'owner/fence')
	`); err != nil {
		t.Fatal(err)
	}
	insertTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = insertTx.Rollback(context.Background()) }()
	if _, err := insertTx.Exec(ctx, `
		INSERT INTO agentops_control.jobs(
		  id, registration_id, registration_version, source_kind, source_key,
		  idempotency_key, job_type, payload, status
		) VALUES (
		  '21000000-0000-4000-8000-000000000001',
		  '11000000-0000-4000-8000-000000000001',
		  1, 'manual', 'before-drain', 'before-drain',
		  'agentops.runner', '{}', 'queued'
		)
	`); err != nil {
		t.Fatal(err)
	}
	drainDone := make(chan error, 1)
	deadline := time.Now().UTC().Add(time.Minute).Round(time.Microsecond)
	go func() {
		_, _, transitionErr := store.Transition(
			ctx,
			"integration",
			"direct-insert-drain",
			lifecyclestore.ModeDraining,
			&deadline,
			map[string]any{"test": "direct-insert-fence"},
		)
		drainDone <- transitionErr
	}()
	select {
	case err := <-drainDone:
		t.Fatalf("drain crossed the uncommitted insert fence: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := insertTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agentops_control.jobs(
		  id, registration_id, registration_version, source_kind, source_key,
		  idempotency_key, job_type, payload, status
		) VALUES (
		  '21000000-0000-4000-8000-000000000002',
		  '11000000-0000-4000-8000-000000000001',
		  1, 'manual', 'after-drain', 'after-drain',
		  'agentops.runner', '{}', 'queued'
		)
	`); err == nil {
		t.Fatal("direct insert committed after DRAINING")
	}

	if _, _, err := store.Transition(
		ctx,
		"integration",
		"fence-draining-monitor",
		lifecyclestore.ModeMonitorOnly,
		nil,
		map[string]any{"test": "direct-insert-fence"},
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Transition(
		ctx,
		"integration",
		"fence-monitor-active",
		lifecyclestore.ModeActive,
		nil,
		map[string]any{"test": "direct-insert-fence"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE agentops_control.jobs
		   SET status = 'succeeded', finished_at = clock_timestamp(),
		       updated_at = clock_timestamp()
		 WHERE id = '21000000-0000-4000-8000-000000000001'
	`); err != nil {
		t.Fatal(err)
	}
	drainTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drainTx.Rollback(context.Background()) }()
	if _, err := drainTx.Exec(ctx, `
		UPDATE agentops_control.lifecycle_state
		   SET mode = 'DRAINING', generation = generation + 1,
		       updated_at = clock_timestamp()
		 WHERE singleton
	`); err != nil {
		t.Fatal(err)
	}
	insertDone := make(chan error, 1)
	go func() {
		_, insertErr := pool.Exec(ctx, `
			INSERT INTO agentops_control.jobs(
			  id, registration_id, registration_version, source_kind, source_key,
			  idempotency_key, job_type, payload, status
			) VALUES (
			  '21000000-0000-4000-8000-000000000003',
			  '11000000-0000-4000-8000-000000000001',
			  1, 'manual', 'racing-drain', 'racing-drain',
			  'agentops.runner', '{}', 'queued'
			)
		`)
		insertDone <- insertErr
	}()
	select {
	case err := <-insertDone:
		t.Fatalf("insert crossed the uncommitted drain fence: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := drainTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-insertDone; err == nil {
		t.Fatal("racing direct insert committed after drain")
	}
}

func TestPostgresLegacyWorkItemReplayConvergesOnCanonicalJobs(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	store, err := OpenStoreWithTopology(
		ctx,
		databaseURL,
		root,
		RuntimeTopologySignedWebhookIngress,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registration, duplicate, err := store.CreateRegistration(
		ctx,
		CreateRegistration{Repository: "owner/legacy-replay"},
		"create-legacy-replay-registration",
		"operator",
	)
	if err != nil || duplicate {
		t.Fatalf("CreateRegistration() = %#v, %v, %v", registration, duplicate, err)
	}
	updatedAt := time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		legacy    WorkItem
		canonical WorkItem
	}{
		{
			name: "pull request",
			legacy: WorkItem{
				Repository: registration.Repository, Kind: "pull_request", Number: 42,
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: registration.Repository, Kind: WorkItemKindPullRequest,
				Number: 42, UpdatedAt: updatedAt,
			},
		},
		{
			name: "push",
			legacy: WorkItem{
				Repository: registration.Repository, Kind: "push", Identity: "head-1",
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: registration.Repository, Kind: WorkItemKindRepositoryHead,
				SourceEvent: "push", Identity: "head-1", UpdatedAt: updatedAt,
			},
		},
		{
			name: "check run",
			legacy: WorkItem{
				Repository: registration.Repository, Kind: "check_run", Identity: "123",
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: registration.Repository, Kind: WorkItemKindRepositoryHead,
				SourceEvent: "check_run", Identity: "123", UpdatedAt: updatedAt,
			},
		},
		{
			name: "check suite",
			legacy: WorkItem{
				Repository: registration.Repository, Kind: "check_suite", Identity: "456",
				UpdatedAt: updatedAt,
			},
			canonical: WorkItem{
				Repository: registration.Repository, Kind: WorkItemKindRepositoryHead,
				SourceEvent: "check_suite", Identity: "456", UpdatedAt: updatedAt,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacyID, duplicate, err := store.EnqueueWork(
				ctx, registration, "webhook", "legacy-"+test.name, test.legacy,
			)
			if err != nil || duplicate {
				t.Fatalf("legacy EnqueueWork() = %s, %v, %v", legacyID, duplicate, err)
			}
			canonicalID, duplicate, err := store.EnqueueWork(
				ctx, registration, "poll", "canonical-"+test.name, test.canonical,
			)
			if err != nil || !duplicate || canonicalID != legacyID {
				t.Fatalf(
					"canonical replay = %s, %v, %v; legacy id = %s",
					canonicalID,
					duplicate,
					err,
					legacyID,
				)
			}
			if _, err := pool.Exec(ctx, `
				UPDATE agentops_control.jobs
				   SET status = 'succeeded', finished_at = clock_timestamp(),
				       updated_at = clock_timestamp()
				 WHERE id = $1`, legacyID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPostgresRegistrationControlIntegration(t *testing.T) {
	databaseURL := os.Getenv("AGENTOPS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOPS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	root := testRepositoryRoot(t)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	resetAndMigrate(t, ctx, pool, root)
	store, err := OpenLegacyCLIForwarderStore(ctx, databaseURL, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	created, duplicate, err := store.CreateRegistration(
		ctx,
		CreateRegistration{Repository: "Owner/Repo"},
		"create-registration",
		"operator",
	)
	if err != nil || duplicate {
		t.Fatalf("CreateRegistration() = %#v, %v, %v", created, duplicate, err)
	}
	replayed, duplicate, err := store.CreateRegistration(
		ctx,
		CreateRegistration{Repository: "Owner/Repo"},
		"create-registration",
		"operator",
	)
	if err != nil || !duplicate || replayed.ID != created.ID {
		t.Fatalf("idempotent replay = %#v, %v, %v", replayed, duplicate, err)
	}
	existing, duplicate, err := store.CreateRegistration(
		ctx,
		CreateRegistration{Repository: "owner/repo"},
		"create-existing-registration",
		"operator",
	)
	if !errors.Is(err, ErrConflict) || duplicate ||
		existing.ID != created.ID || existing.Version != created.Version {
		t.Fatalf("existing registration conflict = %#v, %v, %v", existing, duplicate, err)
	}
	if _, _, err := store.CreateRegistration(
		ctx,
		CreateRegistration{Repository: "owner/other"},
		"create-registration",
		"operator",
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	if key := advisoryRequestKey("a:b", "c"); key == advisoryRequestKey("a", "b:c") {
		t.Fatal("advisory request key framing is ambiguous")
	}
	configured, err := store.UpdateRegistration(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{Configuration: json.RawMessage(`{}`)},
		"operator",
		"registration.updated",
	)
	if err != nil || configured.Version != created.Version+1 {
		t.Fatalf("configuration update = %#v, %v", configured, err)
	}
	created = configured
	prEnabled := false
	commandUpdated, replay, err := store.UpdateRegistrationCommand(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{PRMonitorEnabled: &prEnabled},
		"update-command-1",
		"operator",
		"registration.updated",
	)
	if err != nil || replay || commandUpdated.Version != created.Version+1 {
		t.Fatalf("UpdateRegistrationCommand() = %#v, %v, %v", commandUpdated, replay, err)
	}
	var auditedPrevious, auditedCurrent bool
	var auditedOutcome string
	if err := store.pool.QueryRow(ctx,
		`SELECT
		   (details->'changedFields'->'prMonitorEnabled'->>'previous')::boolean,
		   (details->'changedFields'->'prMonitorEnabled'->>'current')::boolean,
		   details->>'outcome'
		 FROM agentops_control.runtime_audit
		WHERE event_type = 'registration.updated'
		  AND details->>'commandIdentityDigest' IS NOT NULL
		ORDER BY occurred_at DESC
		LIMIT 1`,
	).Scan(&auditedPrevious, &auditedCurrent, &auditedOutcome); err != nil {
		t.Fatal(err)
	}
	if !auditedPrevious || auditedCurrent || auditedOutcome != "applied" {
		t.Fatalf(
			"update audit previous=%v current=%v outcome=%s",
			auditedPrevious,
			auditedCurrent,
			auditedOutcome,
		)
	}
	commandReplay, replay, err := store.UpdateRegistrationCommand(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{PRMonitorEnabled: &prEnabled},
		"update-command-1",
		"operator",
		"registration.updated",
	)
	if err != nil || !replay || commandReplay.Version != commandUpdated.Version {
		t.Fatalf("update command replay = %#v, %v, %v", commandReplay, replay, err)
	}
	differentPREnabled := true
	if _, _, err := store.UpdateRegistrationCommand(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{PRMonitorEnabled: &differentPREnabled},
		"update-command-1",
		"operator",
		"registration.updated",
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("update command identity conflict = %v", err)
	}
	_, replay, err = store.UpdateRegistrationCommand(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{PRMonitorEnabled: &differentPREnabled},
		"update-command-rejected",
		"operator",
		"registration.updated",
	)
	var rejectedCommand *RegistrationCommandRejection
	if replay || !errors.As(err, &rejectedCommand) ||
		rejectedCommand.Reason != "registration_version_mismatch" ||
		rejectedCommand.RecordedAt.IsZero() {
		t.Fatalf("rejected update = replay=%v rejection=%#v err=%v", replay, rejectedCommand, err)
	}
	updateRejectedRecordedAt := rejectedCommand.RecordedAt
	_, replay, err = store.UpdateRegistrationCommand(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{PRMonitorEnabled: &differentPREnabled},
		"update-command-rejected",
		"operator",
		"registration.updated",
	)
	if !replay || !errors.As(err, &rejectedCommand) ||
		!rejectedCommand.RecordedAt.Equal(updateRejectedRecordedAt) {
		t.Fatalf("rejected update replay = replay=%v rejection=%#v err=%v", replay, rejectedCommand, err)
	}
	enabled := false
	if _, _, err := store.UpdateRegistrationCommand(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{Enabled: &enabled},
		"update-command-rejected",
		"operator",
		"registration.updated",
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("rejected update identity reuse = %v", err)
	}
	_, replay, err = store.UpdateRegistrationCommand(
		ctx,
		commandUpdated.ID,
		commandUpdated.Version,
		RegistrationPatch{PRMonitorEnabled: &prEnabled},
		"update-command-no-change",
		"operator",
		"registration.updated",
	)
	if replay || !errors.As(err, &rejectedCommand) ||
		rejectedCommand.Reason != "registration_patch_has_no_change" {
		t.Fatalf("no-change update = replay=%v rejection=%#v err=%v", replay, rejectedCommand, err)
	}
	_, replay, err = store.UpdateRegistrationCommand(
		ctx,
		commandUpdated.ID,
		commandUpdated.Version,
		RegistrationPatch{PRMonitorEnabled: &prEnabled},
		"update-command-no-change",
		"operator",
		"registration.updated",
	)
	if !replay || !errors.As(err, &rejectedCommand) ||
		rejectedCommand.Reason != "registration_patch_has_no_change" {
		t.Fatalf("no-change replay = replay=%v rejection=%#v err=%v", replay, rejectedCommand, err)
	}
	created = commandUpdated
	disabledDesired := false
	noEvidence, duplicate, err := store.CreateRegistration(
		ctx,
		CreateRegistration{
			Repository:          "owner/no-evidence",
			Enabled:             &disabledDesired,
			IssueMonitorEnabled: &disabledDesired,
			PRMonitorEnabled:    &disabledDesired,
			ExecutionEnabled:    &disabledDesired,
		},
		"create-no-evidence-registration",
		"operator",
	)
	if err != nil || duplicate {
		t.Fatalf("no-evidence registration = %#v, %v, %v", noEvidence, duplicate, err)
	}
	noEvidenceProjections, err := store.Projections(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	foundNoEvidence := false
	for _, projection := range noEvidenceProjections {
		if projection.Registration.ID != noEvidence.ID {
			continue
		}
		foundNoEvidence = true
		for _, componentName := range []string{
			ComponentIssueMonitor,
			ComponentPRMonitor,
			ComponentForwarder,
			ComponentExecution,
		} {
			component := projection.Components[componentName]
			if component.Actual != "unknown" ||
				component.Freshness != "unknown" ||
				component.ObservedAt != nil ||
				component.LastGoodAt != nil {
				t.Fatalf("missing %s evidence was fabricated: %#v", componentName, component)
			}
		}
	}
	if !foundNoEvidence {
		t.Fatal("no-evidence registration was absent from projections")
	}
	if err := store.UpsertActualState(
		ctx,
		created,
		ComponentIssueMonitor,
		"running",
		"current-supervisor",
		nil,
	); err != nil {
		t.Fatal(err)
	}
	staleRegistration := created
	staleRegistration.Version--
	if err := store.UpsertActualState(
		ctx,
		staleRegistration,
		ComponentIssueMonitor,
		"stopped",
		"stale-supervisor",
		nil,
	); err != nil {
		t.Fatal(err)
	}
	var actualVersion int64
	var actualState string
	if err := store.pool.QueryRow(ctx,
		`SELECT registration_version, state
		   FROM agentops_control.monitor_actual_states
		  WHERE registration_id = $1 AND component = $2`,
		created.ID,
		ComponentIssueMonitor,
	).Scan(&actualVersion, &actualState); err != nil {
		t.Fatal(err)
	}
	if actualVersion != created.Version || actualState != "running" {
		t.Fatalf("stale actual state overwrote current version: version=%d state=%s", actualVersion, actualState)
	}

	issueItem := WorkItem{
		Repository: created.Repository,
		Kind:       "issue",
		Number:     13,
		UpdatedAt:  time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC),
	}
	jobID, duplicate, err := store.EnqueueWork(
		ctx,
		created,
		"webhook",
		"delivery-source",
		issueItem,
	)
	if err != nil || duplicate {
		t.Fatalf("EnqueueWork() = %s, %v, %v", jobID, duplicate, err)
	}
	var jobType string
	var triagePayload map[string]any
	if err := store.pool.QueryRow(ctx,
		`SELECT job_type, payload FROM agentops_control.jobs WHERE id = $1`,
		jobID,
	).Scan(&jobType, &triagePayload); err != nil {
		t.Fatal(err)
	}
	repository, _ := triagePayload["repository"].(map[string]any)
	issue, _ := triagePayload["issue"].(map[string]any)
	if jobType != "agentops.triage" ||
		triagePayload["schemaVersion"] != float64(1) ||
		repository["owner"] != "owner" ||
		issue["number"] != float64(13) {
		t.Fatalf("triage contract projection = %s %#v", jobType, triagePayload)
	}
	pollJobID, duplicate, err := store.EnqueueWork(
		ctx,
		created,
		"poll",
		issueItem.IdempotencyKey(),
		issueItem,
	)
	if err != nil || !duplicate || pollJobID != jobID {
		t.Fatalf("webhook/poll convergence = %s, %v, %v", pollJobID, duplicate, err)
	}
	monitorDisabled := false
	raceRegistration, duplicate, err := store.CreateRegistration(
		ctx,
		CreateRegistration{
			Repository:          "owner/race",
			IssueMonitorEnabled: &monitorDisabled,
			PRMonitorEnabled:    &monitorDisabled,
		},
		"create-race-registration",
		"operator",
	)
	if err != nil || duplicate {
		t.Fatalf("race registration = %#v, %v, %v", raceRegistration, duplicate, err)
	}
	raceItem := issueItem
	raceItem.Repository = raceRegistration.Repository
	raceItem.Number = 77
	type enqueueResult struct {
		id        string
		duplicate bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan enqueueResult, 2)
	var enqueueGroup sync.WaitGroup
	for _, sourceKind := range []string{"webhook", "poll"} {
		enqueueGroup.Add(1)
		go func(kind string) {
			defer enqueueGroup.Done()
			<-start
			id, duplicate, err := store.EnqueueWork(
				ctx,
				raceRegistration,
				kind,
				kind+"-source",
				raceItem,
			)
			results <- enqueueResult{id: id, duplicate: duplicate, err: err}
		}(sourceKind)
	}
	close(start)
	enqueueGroup.Wait()
	close(results)
	var raceResults []enqueueResult
	for result := range results {
		raceResults = append(raceResults, result)
	}
	if len(raceResults) != 2 || raceResults[0].err != nil || raceResults[1].err != nil ||
		raceResults[0].id != raceResults[1].id ||
		raceResults[0].duplicate == raceResults[1].duplicate {
		t.Fatalf("concurrent webhook/poll convergence = %#v", raceResults)
	}
	requeueRegistration, duplicate, err := store.CreateRegistration(
		ctx,
		CreateRegistration{
			Repository:          "owner/requeue",
			IssueMonitorEnabled: &monitorDisabled,
			PRMonitorEnabled:    &monitorDisabled,
		},
		"create-requeue-registration",
		"operator",
	)
	if err != nil || duplicate {
		t.Fatalf("requeue registration = %#v, %v, %v", requeueRegistration, duplicate, err)
	}
	requeueItem := issueItem
	requeueItem.Repository = requeueRegistration.Repository
	requeueItem.Number = 88
	requeueJobID, duplicate, err := store.EnqueueWork(
		ctx,
		requeueRegistration,
		"webhook",
		"requeue-source",
		requeueItem,
	)
	if err != nil || duplicate {
		t.Fatalf("initial requeue job = %s, %v, %v", requeueJobID, duplicate, err)
	}
	requeueRegistration, err = store.UpdateRegistration(
		ctx,
		requeueRegistration.ID,
		requeueRegistration.Version,
		RegistrationPatch{Configuration: json.RawMessage(`{}`)},
		"operator",
		"registration.updated",
	)
	if err != nil {
		t.Fatal(err)
	}
	recoveredJobID, duplicate, err := store.EnqueueWork(
		ctx,
		requeueRegistration,
		"poll",
		requeueItem.IdempotencyKey(),
		requeueItem,
	)
	if err != nil || duplicate || recoveredJobID != requeueJobID {
		t.Fatalf(
			"registration-change requeue = %s, %v, %v",
			recoveredJobID,
			duplicate,
			err,
		)
	}
	var recoveredVersion int64
	var recoveredStatus string
	var requeueAudits int
	if err := store.pool.QueryRow(ctx,
		`SELECT registration_version, status
		   FROM agentops_control.jobs WHERE id = $1`,
		requeueJobID,
	).Scan(&recoveredVersion, &recoveredStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx,
		`SELECT count(*) FROM agentops_control.runtime_audit
		  WHERE job_id = $1
		    AND event_type = 'job.requeued_after_registration_change'`,
		requeueJobID,
	).Scan(&requeueAudits); err != nil {
		t.Fatal(err)
	}
	if recoveredVersion != requeueRegistration.Version ||
		recoveredStatus != "queued" || requeueAudits != 1 {
		t.Fatalf(
			"recovered version=%d status=%s audits=%d",
			recoveredVersion,
			recoveredStatus,
			requeueAudits,
		)
	}
	otherItem := issueItem
	otherItem.Number = 14
	if _, _, err := store.EnqueueWork(
		ctx,
		created,
		"poll",
		otherItem.IdempotencyKey(),
		otherItem,
	); !errors.Is(err, ErrRepositoryBusy) {
		t.Fatalf("single-flight error = %v", err)
	}

	payload := map[string]any{
		"action":     "opened",
		"repository": map[string]any{"full_name": created.Repository},
		"issue": map[string]any{
			"number":     13,
			"updated_at": "2026-07-25T00:00:00Z",
		},
	}
	receipt, err := store.ReceiveWebhook(
		ctx,
		"github-delivery-1",
		created.Repository,
		"issues",
		stringPointer("opened"),
		map[string]string{
			"Authorization":       "Bearer secret",
			"X-GitHub-Delivery":   "github-delivery-1",
			"X-Hub-Signature-256": "sha256=secret",
		},
		payload,
	)
	if err != nil {
		t.Fatal(err)
	}
	replayedReceipt, err := store.ReceiveWebhook(
		ctx,
		"github-delivery-1",
		created.Repository,
		"issues",
		stringPointer("opened"),
		map[string]string{},
		payload,
	)
	if err != nil || !replayedReceipt.Duplicate || replayedReceipt.DeliveryID != receipt.DeliveryID {
		t.Fatalf("delivery replay = %#v, %v", replayedReceipt, err)
	}
	conflictingPayload := map[string]any{
		"repository": map[string]any{"full_name": created.Repository},
		"issue":      map[string]any{"number": 99},
	}
	if _, err := store.ReceiveWebhook(
		ctx,
		"github-delivery-1",
		created.Repository,
		"issues",
		nil,
		map[string]string{},
		conflictingPayload,
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("delivery conflict error = %v", err)
	}
	var storedHeaders map[string]string
	if err := store.pool.QueryRow(ctx,
		`SELECT headers FROM agentops_control.webhook_deliveries WHERE id = $1`,
		receipt.DeliveryID,
	).Scan(&storedHeaders); err != nil {
		t.Fatal(err)
	}
	if len(storedHeaders) != 1 || storedHeaders["x-github-delivery"] != "github-delivery-1" {
		t.Fatalf("durable headers = %#v", storedHeaders)
	}

	unknownReceipt, err := store.ReceiveWebhook(
		ctx,
		"unknown-delivery",
		"unknown/repository",
		"issues",
		nil,
		map[string]string{},
		map[string]any{
			"repository": map[string]any{"full_name": "unknown/repository"},
			"issue": map[string]any{
				"number":     1,
				"updated_at": "2026-07-25T00:00:00Z",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimWebhook(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil {
		t.Fatal("expected a pending delivery")
	}
	// The first claim can be the registered delivery. Process claims until the unknown one is ignored.
	router := &Router{Store: store}
	for claim != nil {
		if err := router.route(ctx, *claim); err != nil {
			_ = store.FinishWebhook(ctx, *claim, "failed", err.Error())
		}
		claim, err = store.ClaimWebhook(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}
	var unknownStatus, ignoredReason string
	if err := store.pool.QueryRow(ctx,
		`SELECT status, ignored_reason
		   FROM agentops_control.webhook_deliveries
		  WHERE id = $1`,
		unknownReceipt.DeliveryID,
	).Scan(&unknownStatus, &ignoredReason); err != nil {
		t.Fatal(err)
	}
	if unknownStatus != "ignored" || ignoredReason != "unregistered_repository" {
		t.Fatalf("unknown delivery status=%s reason=%s", unknownStatus, ignoredReason)
	}

	failedReceipt, err := store.ReceiveWebhook(
		ctx,
		"failed-delivery",
		created.Repository,
		"issues",
		nil,
		map[string]string{},
		payload,
	)
	if err != nil {
		t.Fatal(err)
	}
	failedClaim, err := store.ClaimWebhook(ctx, time.Minute)
	if err != nil || failedClaim == nil {
		t.Fatalf("ClaimWebhook() = %#v, %v", failedClaim, err)
	}
	if err := store.BindWebhook(ctx, *failedClaim, created); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishWebhook(ctx, *failedClaim, "failed", "transient"); err != nil {
		t.Fatal(err)
	}
	projections, err := store.Projections(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var createdProjection *RegistrationProjection
	for index := range projections {
		if projections[index].Registration.ID == created.ID {
			createdProjection = &projections[index]
			break
		}
	}
	if createdProjection == nil ||
		len(createdProjection.RecentDeliveryFailures) != 1 ||
		createdProjection.RecentDeliveryFailures[0].ID != failedReceipt.DeliveryID ||
		createdProjection.RecentDeliveryFailures[0].LastError == nil {
		t.Fatalf("failed delivery projection = %#v", createdProjection)
	}
	if createdProjection.Components[ComponentExecution].Actual != "waiting" ||
		createdProjection.Components[ComponentQueue].Actual != "queued" ||
		createdProjection.ActiveJobState == nil ||
		*createdProjection.ActiveJobState != "queued" ||
		createdProjection.ActiveJobRegistrationVersion == nil ||
		*createdProjection.ActiveJobRegistrationVersion != created.Version {
		t.Fatalf("active work projection = %#v", createdProjection)
	}
	attemptID, _ := randomUUID()
	leaseID, _ := randomUUID()
	leaseToken, _ := randomUUID()
	if _, err := store.pool.Exec(ctx,
		`UPDATE agentops_control.jobs
		    SET status = 'leased', updated_at = clock_timestamp() - interval '2 minutes'
		  WHERE id = $1`,
		jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO agentops_control.job_attempts(
		   id, job_id, attempt_number, worker_id, status, started_at
		 ) VALUES ($1, $2, 1, 'heartbeat-worker', 'running', clock_timestamp() - interval '2 minutes')`,
		attemptID,
		jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO agentops_control.job_leases(
		   id, job_id, attempt_id, lease_token, worker_id,
		   acquired_at, heartbeat_at, expires_at
		 ) VALUES (
		   $1, $2, $3, $4, 'heartbeat-worker',
		   clock_timestamp() - interval '2 minutes',
		   clock_timestamp(),
		   clock_timestamp() + interval '1 minute'
		 )`,
		leaseID,
		jobID,
		attemptID,
		leaseToken,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO agentops_control.development_progress_events(
		   registration_id, registration_version, job_id, attempt_id,
		   repository, subject_kind, subject_number, parent_issue_number,
		   worker_id, event_key,
		   phase, step, state, next_gate, session_name, worktree_path, branch,
		   occurred_at
		 ) VALUES (
		   $1, $2, $3, $4, $5, 'issue', 13, 1, 'heartbeat-worker',
		   'generation:source:a1:start', 'generation', 'generator session',
		   'running', 'repository graders', 'ao-source-s0',
		   '/workspace/jobs/source/worktree', 'agent/source-s0',
		   clock_timestamp() - interval '1 minute'
		 )`,
		created.ID,
		created.Version,
		jobID,
		attemptID,
		created.Repository,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO agentops_control.development_progress_events(
		   registration_id, registration_version, job_id, attempt_id,
		   repository, subject_kind, subject_number, parent_issue_number,
		   worker_id, event_key,
		   phase, step, state, occurred_at
		 )
		 SELECT $1, $2, $3, $4, $5, 'issue', 14, 1, 'heartbeat-worker',
		        'review:test:' || sequence, 'review',
		        'review event ' || sequence,
		        CASE WHEN sequence = 13 THEN 'blocked' ELSE 'running' END,
		        clock_timestamp() + sequence * interval '1 millisecond'
		   FROM generate_series(1, 13) sequence`,
		created.ID,
		created.Version,
		jobID,
		attemptID,
		created.Repository,
	); err != nil {
		t.Fatal(err)
	}
	issueNumber := int64(13)
	progress, err := store.DevelopmentProgress(
		ctx,
		created.Repository,
		&issueNumber,
		20,
	)
	if err != nil || len(progress) != 1 ||
		progress[0].Phase != "generation" ||
		progress[0].SessionName == nil ||
		*progress[0].SessionName != "ao-source-s0" ||
		progress[0].WorktreePath == nil ||
		*progress[0].WorktreePath != "/workspace/jobs/source/worktree" ||
		progress[0].LeaseHeartbeatAt == nil {
		t.Fatalf("DevelopmentProgress() = %#v, %v", progress, err)
	}
	parentIssueNumber := int64(1)
	parentProgress, err := store.DevelopmentProgress(
		ctx,
		created.Repository,
		&parentIssueNumber,
		2,
	)
	parentSubjects := make(map[int64]bool)
	for _, event := range parentProgress {
		if event.SubjectNumber != nil {
			parentSubjects[*event.SubjectNumber] = true
		}
	}
	if err != nil || len(parentProgress) != 2 ||
		!parentSubjects[13] || !parentSubjects[14] ||
		parentProgress[0].ParentIssueNumber == nil ||
		*parentProgress[0].ParentIssueNumber != 1 {
		t.Fatalf("parent DevelopmentProgress() = %#v, %v", parentProgress, err)
	}
	projections, err = store.Projections(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for index := range projections {
		if projections[index].Registration.ID == created.ID {
			createdProjection = &projections[index]
			break
		}
	}
	executionProjection := createdProjection.Components[ComponentExecution]
	queueProjection := createdProjection.Components[ComponentQueue]
	if len(createdProjection.DevelopmentProgress) != 2 {
		t.Fatalf("development projection = %#v", createdProjection.DevelopmentProgress)
	}
	progressByIssue := make(map[int64]DevelopmentIssueProgress)
	for _, issueProgress := range createdProjection.DevelopmentProgress {
		progressByIssue[issueProgress.IssueNumber] = issueProgress
	}
	runningProgress, runningPresent := progressByIssue[13]
	blockedProgress, blockedPresent := progressByIssue[14]
	if !runningPresent || !blockedPresent ||
		runningProgress.Current.JobID != jobID ||
		runningProgress.Current.State != "running" ||
		len(runningProgress.History) != 1 ||
		runningProgress.Current.LeaseHeartbeatAt == nil ||
		!runningProgress.LastActivity.Equal(*runningProgress.Current.LeaseHeartbeatAt) ||
		blockedProgress.Current.EventKey != "review:test:13" ||
		blockedProgress.Current.State != "blocked" ||
		len(blockedProgress.History) != 12 ||
		!blockedProgress.LastActivity.Equal(blockedProgress.Current.OccurredAt) {
		t.Fatalf("grouped development projection = %#v", createdProjection.DevelopmentProgress)
	}
	for _, event := range blockedProgress.History {
		if event.SubjectNumber == nil || *event.SubjectNumber != 14 {
			t.Fatalf("Issue 14 history crossed coordinates: %#v", blockedProgress.History)
		}
	}
	if executionProjection.Actual != "running" ||
		executionProjection.Freshness != "fresh" ||
		queueProjection.Actual != "leased" ||
		queueProjection.Freshness != "fresh" ||
		executionProjection.ObservedAt == nil ||
		time.Since(*executionProjection.ObservedAt) > 10*time.Second {
		t.Fatalf(
			"lease heartbeat projection execution=%#v queue=%#v",
			executionProjection,
			queueProjection,
		)
	}
	if _, err := store.pool.Exec(ctx,
		`UPDATE agentops_control.job_leases
		    SET status = 'released', released_at = clock_timestamp()
		  WHERE id = $1`,
		leaseID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx,
		`DELETE FROM agentops_control.job_attempts WHERE id = $1`,
		attemptID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx,
		`UPDATE agentops_control.jobs
		    SET status = 'queued', updated_at = clock_timestamp()
		  WHERE id = $1`,
		jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx,
		`UPDATE agentops_control.repository_registrations
		    SET execution_enabled = false
		  WHERE id = $1`,
		created.ID,
	); err != nil {
		t.Fatal(err)
	}
	_, replay, err = store.RetryWebhook(
		ctx,
		failedReceipt.DeliveryID,
		"retry-execution-disabled",
		"operator",
		failedClaim.RouteAttempts,
		created.ID,
		created.Version,
	)
	var executionDisabledConflict *DeliveryRetryConflict
	if replay || !errors.As(err, &executionDisabledConflict) ||
		executionDisabledConflict.Reason != "execution_disabled" ||
		executionDisabledConflict.AttemptID == "" {
		t.Fatalf("execution-disabled retry = replay=%v conflict=%#v err=%v", replay, executionDisabledConflict, err)
	}
	executionDisabledAttemptID := executionDisabledConflict.AttemptID
	_, replay, err = store.RetryWebhook(
		ctx,
		failedReceipt.DeliveryID,
		"retry-execution-disabled",
		"operator",
		failedClaim.RouteAttempts,
		created.ID,
		created.Version,
	)
	if !replay || !errors.As(err, &executionDisabledConflict) ||
		executionDisabledConflict.AttemptID != executionDisabledAttemptID {
		t.Fatalf("execution-disabled retry replay = replay=%v conflict=%#v err=%v", replay, executionDisabledConflict, err)
	}
	if _, err := store.pool.Exec(ctx,
		`UPDATE agentops_control.repository_registrations
		    SET execution_enabled = true
		  WHERE id = $1`,
		created.ID,
	); err != nil {
		t.Fatal(err)
	}
	retry, replay, err := store.RetryWebhook(
		ctx,
		failedReceipt.DeliveryID,
		"retry-key",
		"operator",
		failedClaim.RouteAttempts,
		created.ID,
		created.Version,
	)
	if err != nil || replay || retry.State != "pending" || retry.Cancellable ||
		retry.RecordedAt.IsZero() {
		t.Fatalf("RetryWebhook() = %#v, %v, %v", retry, replay, err)
	}
	deliveryStatus, err := store.DeliveryStatus(ctx, failedReceipt.DeliveryID)
	acceptedRetryPresent := false
	for _, attempt := range deliveryStatus.RetryAttempts {
		if attempt.AttemptID == retry.AttemptID &&
			attempt.Status == "accepted" &&
			attempt.ObservedRouteAttempts == failedClaim.RouteAttempts {
			acceptedRetryPresent = true
		}
	}
	if err != nil ||
		deliveryStatus.Status != "pending" ||
		!acceptedRetryPresent {
		t.Fatalf("DeliveryStatus() = %#v, %v", deliveryStatus, err)
	}
	retryReplay, replay, err := store.RetryWebhook(
		ctx,
		failedReceipt.DeliveryID,
		"retry-key",
		"operator",
		failedClaim.RouteAttempts,
		created.ID,
		created.Version,
	)
	if err != nil || !replay || retryReplay.AttemptID != retry.AttemptID ||
		!retryReplay.RecordedAt.Equal(retry.RecordedAt) {
		t.Fatalf("retry replay = %#v, %v, %v", retryReplay, replay, err)
	}
	if _, _, err := store.RetryWebhook(
		ctx,
		unknownReceipt.DeliveryID,
		"retry-key",
		"operator",
		0,
		created.ID,
		created.Version,
	); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross-delivery idempotency key reuse = %v", err)
	}
	_, replay, err = store.RetryWebhook(
		ctx,
		failedReceipt.DeliveryID,
		"retry-rejected",
		"operator",
		failedClaim.RouteAttempts,
		created.ID,
		created.Version,
	)
	var retryConflict *DeliveryRetryConflict
	if replay || !errors.As(err, &retryConflict) || retryConflict.AttemptID == "" ||
		retryConflict.Reason != "delivery_not_retryable" {
		t.Fatalf("rejected retry = replay=%v conflict=%#v err=%v", replay, retryConflict, err)
	}
	rejectedAttemptID := retryConflict.AttemptID
	rejectedRecordedAt := retryConflict.RecordedAt
	_, replay, err = store.RetryWebhook(
		ctx,
		failedReceipt.DeliveryID,
		"retry-rejected",
		"operator",
		failedClaim.RouteAttempts,
		created.ID,
		created.Version,
	)
	if !replay || !errors.As(err, &retryConflict) ||
		retryConflict.AttemptID != rejectedAttemptID ||
		!retryConflict.RecordedAt.Equal(rejectedRecordedAt) {
		t.Fatalf("rejected retry replay = replay=%v conflict=%#v err=%v", replay, retryConflict, err)
	}
	var rejectedAttempts, rejectedAudits int
	if err := store.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM agentops_control.delivery_retry_attempts
		  WHERE delivery_id = $1 AND status = 'rejected' AND id = $2`,
		failedReceipt.DeliveryID,
		rejectedAttemptID,
	).Scan(&rejectedAttempts); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM agentops_control.runtime_audit
		  WHERE event_type = 'webhook.retry.rejected'
		    AND details->>'attemptId' = $1`,
		rejectedAttemptID,
	).Scan(&rejectedAudits); err != nil {
		t.Fatal(err)
	}
	if rejectedAttempts != 1 || rejectedAudits != 1 {
		t.Fatalf("rejected attempts=%d audits=%d", rejectedAttempts, rejectedAudits)
	}

	disabled := false
	updated, err := store.UpdateRegistration(
		ctx,
		created.ID,
		created.Version,
		RegistrationPatch{Enabled: &disabled},
		"operator",
		"registration.disabled",
	)
	if err != nil || updated.Enabled || updated.Version != created.Version+1 {
		t.Fatalf("UpdateRegistration() = %#v, %v", updated, err)
	}
	var staleJobStatus string
	if err := store.pool.QueryRow(ctx,
		`SELECT status FROM agentops_control.jobs WHERE id = $1`,
		jobID,
	).Scan(&staleJobStatus); err != nil {
		t.Fatal(err)
	}
	if staleJobStatus != "rejected" {
		t.Fatalf("stale queued job status = %s", staleJobStatus)
	}
	projections, err = store.Projections(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for index := range projections {
		if projections[index].Registration.ID == created.ID {
			createdProjection = &projections[index]
			break
		}
	}
	if createdProjection == nil || createdProjection.LastJobFailure == nil ||
		createdProjection.LastJobFailure.ID != jobID ||
		createdProjection.LastJobFailure.Status != "rejected" {
		t.Fatalf("job failure projection = %#v", createdProjection)
	}
	if createdProjection.Components[ComponentExecution].Actual != "stale" ||
		createdProjection.Components[ComponentExecution].Freshness != "stale" ||
		createdProjection.Components[ComponentExecution].RecoveryState != "blocked" {
		t.Fatalf("stale execution projection = %#v", createdProjection.Components[ComponentExecution])
	}
	if _, _, err := store.EnqueueWork(
		ctx,
		created,
		"poll",
		"disabled-work",
		otherItem,
	); !errors.Is(err, ErrStaleRegistration) {
		t.Fatalf("disabled enqueue error = %v", err)
	}
}

func testRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := reporoot.Find(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func resetAndMigrate(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	root string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS agentops_control CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA agentops_control"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE agentops_control.schema_migrations (
		  version integer PRIMARY KEY CHECK (version > 0),
		  name text NOT NULL UNIQUE,
		  checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
		  installed_at timestamptz NOT NULL DEFAULT clock_timestamp()
		)
	`); err != nil {
		t.Fatal(err)
	}
	for version, name := range []string{
		"0001_control_store.sql",
		"0002_registration_control.sql",
		"0003_isolated_runner.sql",
		"0004_agentops_lifecycle.sql",
		"0005_private_monitor_broker.sql",
		"0006_monitor_broker_capability_functions.sql",
		"0007_multi_repository_triage.sql",
		"0008_release_receipt_outbox.sql",
		"0009_release_constraint_capabilities.sql",
		"0010_release_completion_capability.sql",
		"0011_release_pull_request_binding.sql",
		"0012_runner_release_review_capabilities.sql",
		"0013_release_human_review_abandonment.sql",
		"0014_development_progress.sql",
		"0015_development_progress_backfill.sql",
		"0016_reuse_open_release_promotion.sql",
		"0017_freeze_source_issue_snapshot.sql",
		"0018_requirements_upgrade_pr_recovery.sql",
		"0019_durable_kanban_gates.sql",
		"0020_registration_repository_authority.sql",
		"0021_review_rounds_and_branch_dag.sql",
		"0022_review_finding_lineage_validation.sql",
	} {
		path := filepath.Join(root, "db", "control-store", "migrations", name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		digest := sha256.Sum256(body)
		if _, err := pool.Exec(
			ctx,
			`INSERT INTO agentops_control.schema_migrations(version, name, checksum)
			 VALUES ($1, $2, $3)`,
			version+1,
			name,
			hex.EncodeToString(digest[:]),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE agentops_control.lifecycle_state
		   SET mode = 'ACTIVE', generation = generation + 1,
		       updated_at = clock_timestamp()
		 WHERE singleton
	`); err != nil {
		t.Fatal(err)
	}
}

func stringPointer(value string) *string {
	return &value
}
