package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mrbaron3/servo/apps/control-plane/internal/lifecycle"
)

const (
	ControlSchemaVersion = 27
	migrationLockKey     = int64(0x4349534f02)
)

type Store struct {
	pool     *pgxpool.Pool
	topology RuntimeTopology
}

// RuntimeTopology is the immutable authority for the components managed by one
// control-plane process. Its zero value manages no components so omission
// cannot silently activate the credential-bearing legacy forwarder.
type RuntimeTopology string

const (
	RuntimeTopologyLegacyCLIForwarder   RuntimeTopology = "legacy-cli-forwarder"
	RuntimeTopologySignedWebhookIngress RuntimeTopology = "signed-webhook-ingress"
)

// ManagesComponent reports whether this topology owns a runtime component.
func (topology RuntimeTopology) ManagesComponent(component string) bool {
	switch component {
	case ComponentIssueMonitor, ComponentPRMonitor, ComponentExecution, ComponentQueue:
		return topology == RuntimeTopologyLegacyCLIForwarder ||
			topology == RuntimeTopologySignedWebhookIngress
	case ComponentForwarder:
		return topology == RuntimeTopologyLegacyCLIForwarder
	default:
		return false
	}
}

// ManagesComponent reports whether this control-store instance exposes a
// component in its explicitly selected runtime topology.
func (store *Store) ManagesComponent(component string) bool {
	return store.topology.ManagesComponent(component)
}

func OpenStore(ctx context.Context, databaseURL, migrationRoot string) (*Store, error) {
	return nil, fmt.Errorf(
		"runtime topology is required; use OpenStoreWithTopology or " +
			"OpenLegacyCLIForwarderStore explicitly",
	)
}

// OpenLegacyCLIForwarderStore is the explicit compatibility-oracle boundary.
// Standard production must use RuntimeTopologySignedWebhookIngress instead.
func OpenLegacyCLIForwarderStore(
	ctx context.Context,
	databaseURL, migrationRoot string,
) (*Store, error) {
	return OpenStoreWithTopology(
		ctx,
		databaseURL,
		migrationRoot,
		RuntimeTopologyLegacyCLIForwarder,
	)
}

// OpenStoreWithTopology binds one immutable runtime topology when the Store is
// created so supervision, execution, and status projection share one authority.
func OpenStoreWithTopology(
	ctx context.Context,
	databaseURL, migrationRoot string,
	topology RuntimeTopology,
) (*Store, error) {
	if topology != RuntimeTopologyLegacyCLIForwarder &&
		topology != RuntimeTopologySignedWebhookIngress {
		return nil, fmt.Errorf("unsupported runtime topology %q", topology)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse database URL: %v", ErrStoreUnavailable, err)
	}
	config.ConnConfig.ConnectTimeout = 3 * time.Second
	config.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("%w: connect: %v", ErrStoreUnavailable, err)
	}
	store := &Store{pool: pool, topology: topology}
	if err := store.VerifySchema(ctx, migrationRoot); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

// MigrateSchema is the explicit owner-only migration path used by the
// short-lived agentopsctl bootstrap container. Normal control and runner
// startup still call verify-only paths and never mutate DDL.
func MigrateSchema(ctx context.Context, databaseURL, migrationRoot string) error {
	expected, err := expectedMigrations(migrationRoot)
	if err != nil {
		return err
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("%w: parse database URL: %v", ErrStoreUnavailable, err)
	}
	config.ConnConfig.ConnectTimeout = 3 * time.Second
	config.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("%w: connect for migration: %v", ErrStoreUnavailable, err)
	}
	defer pool.Close()
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: acquire for migration: %v", ErrStoreUnavailable, err)
	}
	defer connection.Release()
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin migration: %v", ErrStoreUnavailable, err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("%w: lock migration: %v", ErrStoreUnavailable, err)
	}
	var trackingPresent bool
	if err := transaction.QueryRow(ctx,
		`SELECT to_regclass('agentops_control.schema_migrations') IS NOT NULL`,
	).Scan(&trackingPresent); err != nil {
		return fmt.Errorf("%w: inspect migration history: %v", ErrStoreUnavailable, err)
	}
	var objectCount int
	if err := transaction.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'agentops_control'
		   AND c.relkind IN ('r', 'p', 'v', 'm', 'S')
	`).Scan(&objectCount); err != nil {
		return fmt.Errorf("%w: inspect control schema: %v", ErrStoreUnavailable, err)
	}
	if !trackingPresent && objectCount > 0 {
		return fmt.Errorf(
			"%w: partial control schema exists without migration history",
			ErrStoreUnavailable,
		)
	}
	if !trackingPresent {
		if _, err := transaction.Exec(ctx, `
			CREATE SCHEMA IF NOT EXISTS agentops_control;
			CREATE TABLE agentops_control.schema_migrations (
			  version integer PRIMARY KEY CHECK (version > 0),
			  name text NOT NULL UNIQUE,
			  checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
			  installed_at timestamptz NOT NULL DEFAULT clock_timestamp()
			)
		`); err != nil {
			return fmt.Errorf("%w: create migration history: %v", ErrStoreUnavailable, err)
		}
	}
	rows, err := transaction.Query(ctx, `
		SELECT version, name, checksum
		  FROM agentops_control.schema_migrations
		 ORDER BY version
	`)
	if err != nil {
		return fmt.Errorf("%w: read migration history: %v", ErrStoreUnavailable, err)
	}
	installed := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("%w: read migration row: %v", ErrStoreUnavailable, err)
		}
		installed++
		item, present := expected[version]
		if !present || version != installed ||
			item.name != name || item.checksum != checksum {
			rows.Close()
			return fmt.Errorf(
				"%w: migration %d is unknown, non-contiguous, or checksum-mismatched",
				ErrStoreUnavailable,
				version,
			)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("%w: read migration history: %v", ErrStoreUnavailable, err)
	}
	rows.Close()
	for version := installed + 1; version <= ControlSchemaVersion; version++ {
		item := expected[version]
		body, err := os.ReadFile(filepath.Join(
			migrationRoot,
			"db",
			"control-store",
			"migrations",
			item.name,
		))
		if err != nil {
			return fmt.Errorf("%w: read migration %s: %v", ErrStoreUnavailable, item.name, err)
		}
		if _, err := transaction.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("%w: apply migration %s: %v", ErrStoreUnavailable, item.name, err)
		}
		if _, err := transaction.Exec(ctx, `
			INSERT INTO agentops_control.schema_migrations(version, name, checksum)
			VALUES ($1, $2, $3)
		`, version, item.name, item.checksum); err != nil {
			return fmt.Errorf("%w: record migration %s: %v", ErrStoreUnavailable, item.name, err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit migration: %v", ErrStoreUnavailable, err)
	}
	return nil
}

func (store *Store) Close() {
	store.pool.Close()
}

func (store *Store) Ping(ctx context.Context) error {
	if err := store.pool.Ping(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrStoreUnavailable, err)
	}
	return nil
}

func (store *Store) VerifySchema(ctx context.Context, root string) error {
	expected, err := expectedMigrations(root)
	if err != nil {
		return err
	}
	connection, err := store.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: acquire for schema verification: %v", ErrStoreUnavailable, err)
	}
	defer connection.Release()
	transaction, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin schema verification: %v", ErrStoreUnavailable, err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("%w: lock schema verification: %v", ErrStoreUnavailable, err)
	}
	rows, err := transaction.Query(ctx,
		`SELECT version, name, checksum
		   FROM agentops_control.schema_migrations
		  ORDER BY version`,
	)
	if err != nil {
		return fmt.Errorf("%w: control schema is unavailable: %v", ErrStoreUnavailable, err)
	}
	defer rows.Close()
	version := 0
	for rows.Next() {
		var installedVersion int
		var name, checksum string
		if err := rows.Scan(&installedVersion, &name, &checksum); err != nil {
			return fmt.Errorf("%w: read migration history: %v", ErrStoreUnavailable, err)
		}
		version++
		item, present := expected[installedVersion]
		if !present || installedVersion != version || item.name != name || item.checksum != checksum {
			return fmt.Errorf(
				"%w: migration %d is unknown, non-contiguous, or checksum-mismatched",
				ErrStoreUnavailable,
				installedVersion,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: read migration history: %v", ErrStoreUnavailable, err)
	}
	if version != ControlSchemaVersion {
		return fmt.Errorf(
			"%w: schema version %d is not supported version %d",
			ErrStoreUnavailable,
			version,
			ControlSchemaVersion,
		)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("%w: complete schema verification: %v", ErrStoreUnavailable, err)
	}
	return nil
}

type migrationIdentity struct {
	name     string
	checksum string
}

func expectedMigrations(root string) (map[int]migrationIdentity, error) {
	directory := filepath.Join(root, "db", "control-store", "migrations")
	names, err := filepath.Glob(filepath.Join(directory, "[0-9][0-9][0-9][0-9]_*.sql"))
	if err != nil {
		return nil, fmt.Errorf("%w: migration discovery failed: %v", ErrStoreUnavailable, err)
	}
	sort.Strings(names)
	if len(names) != ControlSchemaVersion {
		return nil, fmt.Errorf(
			"%w: migration set must contain exactly %d versions",
			ErrStoreUnavailable,
			ControlSchemaVersion,
		)
	}
	result := make(map[int]migrationIdentity, len(names))
	for index, path := range names {
		version := index + 1
		name := filepath.Base(path)
		if !strings.HasPrefix(name, fmt.Sprintf("%04d_", version)) {
			return nil, fmt.Errorf("%w: migration set is not contiguous at %s", ErrStoreUnavailable, name)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%w: read migration %s: %v", ErrStoreUnavailable, name, err)
		}
		digest := sha256.Sum256(body)
		result[version] = migrationIdentity{name: name, checksum: hex.EncodeToString(digest[:])}
	}
	return result, nil
}

func (store *Store) ListRegistrations(ctx context.Context) ([]Registration, error) {
	rows, err := store.pool.Query(ctx, registrationSelect+` ORDER BY repository`)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var registrations []Registration
	for rows.Next() {
		registration, err := scanRegistration(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		registrations = append(registrations, registration)
	}
	return registrations, unavailable(rows.Err())
}

func (store *Store) RegistrationByRepository(
	ctx context.Context,
	repository string,
) (Registration, error) {
	row := store.pool.QueryRow(ctx, registrationSelect+` WHERE repository = lower($1)`, repository)
	registration, err := scanRegistration(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registration{}, ErrNotFound
	}
	return registration, unavailable(err)
}

func (store *Store) RegistrationByID(ctx context.Context, id string) (Registration, error) {
	row := store.pool.QueryRow(ctx, registrationSelect+` WHERE id = $1`, id)
	registration, err := scanRegistration(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Registration{}, ErrNotFound
	}
	return registration, unavailable(err)
}

func (store *Store) CreateRegistration(
	ctx context.Context,
	input CreateRegistration,
	idempotencyKey, actorID string,
) (Registration, bool, error) {
	validated, err := input.Validated()
	if err != nil {
		return Registration{}, false, err
	}
	requestBody, _ := json.Marshal(validated)
	requestHash := sha256Hex(requestBody)
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Registration{}, false, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	scope := "registration:create"
	if _, err := transaction.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		advisoryRequestKey(scope, idempotencyKey),
	); err != nil {
		return Registration{}, false, unavailable(err)
	}
	var storedHash string
	var storedResponse []byte
	err = transaction.QueryRow(ctx,
		`SELECT request_hash, response
		   FROM agentops_control.control_api_requests
		  WHERE scope = $1 AND idempotency_key = $2
		  FOR UPDATE`,
		scope,
		idempotencyKey,
	).Scan(&storedHash, &storedResponse)
	if err == nil {
		if storedHash != requestHash {
			return Registration{}, false, ErrIdempotencyConflict
		}
		var registration Registration
		if err := json.Unmarshal(storedResponse, &registration); err != nil {
			return Registration{}, false, unavailable(err)
		}
		if err := transaction.Commit(ctx); err != nil {
			return Registration{}, false, unavailable(err)
		}
		return registration, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Registration{}, false, unavailable(err)
	}
	id, err := randomUUID()
	if err != nil {
		return Registration{}, false, err
	}
	registration, err := scanRegistration(transaction.QueryRow(ctx,
		`INSERT INTO agentops_control.repository_registrations(
		   id, repository, enabled, issue_monitor_enabled, pr_monitor_enabled,
		   execution_enabled, configuration
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (repository) DO NOTHING
		 RETURNING id, repository, enabled, issue_monitor_enabled,
		           pr_monitor_enabled, execution_enabled, configuration,
		           version, created_at, updated_at`,
		id,
		validated.Repository,
		validated.Enabled,
		validated.IssueMonitorEnabled,
		validated.PRMonitorEnabled,
		validated.ExecutionEnabled,
		validated.Configuration,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, lookupErr := scanRegistration(transaction.QueryRow(
			ctx,
			registrationSelect+` WHERE repository = $1`,
			validated.Repository,
		))
		if lookupErr != nil {
			return Registration{}, false, unavailable(lookupErr)
		}
		if err := transaction.Commit(ctx); err != nil {
			return Registration{}, false, unavailable(err)
		}
		return existing, false, ErrConflict
	}
	if err != nil {
		return Registration{}, false, classifyPostgres(err)
	}
	response, _ := json.Marshal(registration)
	if _, err := transaction.Exec(ctx,
		`INSERT INTO agentops_control.control_api_requests(
		   scope, idempotency_key, request_hash, status_code, response, actor_id
		 ) VALUES ($1, $2, $3, 201, $4, $5)`,
		scope,
		idempotencyKey,
		requestHash,
		response,
		actorID,
	); err != nil {
		return Registration{}, false, classifyPostgres(err)
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		actorID,
		"registration.created",
		&registration.ID,
		nil,
		map[string]any{
			"repository":            registration.Repository,
			"version":               registration.Version,
			"desiredState":          registrationDesiredAudit(registration),
			"outcome":               "applied",
			"commandIdentityDigest": "sha256:" + sha256Hex([]byte(idempotencyKey)),
		},
	); err != nil {
		return Registration{}, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return Registration{}, false, unavailable(err)
	}
	return registration, false, nil
}

func (store *Store) UpdateRegistration(
	ctx context.Context,
	id string,
	expectedVersion int64,
	patch RegistrationPatch,
	actorID, eventType string,
) (Registration, error) {
	if err := patch.Validate(); err != nil {
		return Registration{}, err
	}
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Registration{}, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	var configuration any
	if len(patch.Configuration) > 0 {
		configuration = patch.Configuration
	}
	registration, err := scanRegistration(transaction.QueryRow(ctx,
		`UPDATE agentops_control.repository_registrations
		    SET enabled = COALESCE($3, enabled),
		        issue_monitor_enabled = COALESCE($4, issue_monitor_enabled),
		        pr_monitor_enabled = COALESCE($5, pr_monitor_enabled),
		        execution_enabled = COALESCE($6, execution_enabled),
		        configuration = COALESCE($7::jsonb, configuration),
		        version = version + 1,
		        updated_at = clock_timestamp()
		  WHERE id = $1 AND version = $2
		  RETURNING id, repository, enabled, issue_monitor_enabled,
		            pr_monitor_enabled, execution_enabled, configuration,
		            version, created_at, updated_at`,
		id,
		expectedVersion,
		patch.Enabled,
		patch.IssueMonitorEnabled,
		patch.PRMonitorEnabled,
		patch.ExecutionEnabled,
		configuration,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		var present bool
		if lookupErr := transaction.QueryRow(
			ctx,
			`SELECT EXISTS(
			   SELECT 1 FROM agentops_control.repository_registrations WHERE id = $1
			 )`,
			id,
		).Scan(&present); lookupErr != nil {
			return Registration{}, unavailable(lookupErr)
		}
		if !present {
			return Registration{}, ErrNotFound
		}
		return Registration{}, ErrConflict
	}
	if err != nil {
		return Registration{}, classifyPostgres(err)
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		actorID,
		eventType,
		&registration.ID,
		nil,
		map[string]any{
			"previousVersion": expectedVersion,
			"version":         registration.Version,
			"enabled":         registration.Enabled,
		},
	); err != nil {
		return Registration{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return Registration{}, unavailable(err)
	}
	return registration, nil
}

func (store *Store) UpdateRegistrationCommand(
	ctx context.Context,
	id string,
	expectedVersion int64,
	patch RegistrationPatch,
	idempotencyKey, actorID, eventType string,
) (Registration, bool, error) {
	if err := patch.Validate(); err != nil {
		return Registration{}, false, err
	}
	requestBody, _ := json.Marshal(map[string]any{
		"id":              id,
		"expectedVersion": expectedVersion,
		"patch":           patch,
		"eventType":       eventType,
	})
	requestHash := sha256Hex(requestBody)
	scope := "registration:command:" + id
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Registration{}, false, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		advisoryRequestKey(scope, idempotencyKey),
	); err != nil {
		return Registration{}, false, unavailable(err)
	}
	var storedHash string
	var storedResponse []byte
	err = transaction.QueryRow(ctx,
		`SELECT request_hash, response
		   FROM agentops_control.control_api_requests
		  WHERE scope = $1 AND idempotency_key = $2
		  FOR UPDATE`,
		scope,
		idempotencyKey,
	).Scan(&storedHash, &storedResponse)
	if err == nil {
		if storedHash != requestHash {
			return Registration{}, false, ErrIdempotencyConflict
		}
		var stored registrationCommandRecord
		if err := json.Unmarshal(storedResponse, &stored); err != nil {
			return Registration{}, false, unavailable(err)
		}
		if err := transaction.Commit(ctx); err != nil {
			return Registration{}, false, unavailable(err)
		}
		if stored.ErrorReason != "" {
			return stored.Registration, true, registrationCommandRejection(
				stored.ErrorReason,
				stored.RecordedAt,
			)
		}
		return stored.Registration, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Registration{}, false, unavailable(err)
	}
	previous, err := scanRegistration(transaction.QueryRow(
		ctx,
		registrationSelect+` WHERE id = $1 FOR UPDATE`,
		id,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return persistRegistrationCommandRejection(
			ctx,
			transaction,
			scope,
			idempotencyKey,
			requestHash,
			actorID,
			eventType,
			id,
			expectedVersion,
			Registration{},
			"registration_not_found",
			404,
		)
	}
	if err != nil {
		return Registration{}, false, unavailable(err)
	}
	if previous.Version != expectedVersion {
		return persistRegistrationCommandRejection(
			ctx,
			transaction,
			scope,
			idempotencyKey,
			requestHash,
			actorID,
			eventType,
			id,
			expectedVersion,
			previous,
			"registration_version_mismatch",
			409,
		)
	}
	if !registrationPatchChanges(patch, previous) {
		return persistRegistrationCommandRejection(
			ctx,
			transaction,
			scope,
			idempotencyKey,
			requestHash,
			actorID,
			eventType,
			id,
			expectedVersion,
			previous,
			"registration_patch_has_no_change",
			400,
		)
	}
	var configuration any
	if len(patch.Configuration) > 0 {
		configuration = patch.Configuration
	}
	registration, err := scanRegistration(transaction.QueryRow(ctx,
		`UPDATE agentops_control.repository_registrations
		    SET enabled = COALESCE($3, enabled),
		        issue_monitor_enabled = COALESCE($4, issue_monitor_enabled),
		        pr_monitor_enabled = COALESCE($5, pr_monitor_enabled),
		        execution_enabled = COALESCE($6, execution_enabled),
		        configuration = COALESCE($7::jsonb, configuration),
		        version = version + 1,
		        updated_at = clock_timestamp()
		  WHERE id = $1 AND version = $2
		  RETURNING id, repository, enabled, issue_monitor_enabled,
		            pr_monitor_enabled, execution_enabled, configuration,
		            version, created_at, updated_at`,
		id,
		expectedVersion,
		patch.Enabled,
		patch.IssueMonitorEnabled,
		patch.PRMonitorEnabled,
		patch.ExecutionEnabled,
		configuration,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return previous, false, ErrConflict
	}
	if err != nil {
		return Registration{}, false, classifyPostgres(err)
	}
	response, _ := json.Marshal(registrationCommandRecord{
		Registration: registration,
		RecordedAt:   registration.UpdatedAt,
	})
	if _, err := transaction.Exec(ctx,
		`INSERT INTO agentops_control.control_api_requests(
		   scope, idempotency_key, request_hash, status_code, response, actor_id
		 ) VALUES ($1, $2, $3, 200, $4, $5)`,
		scope,
		idempotencyKey,
		requestHash,
		response,
		actorID,
	); err != nil {
		return Registration{}, false, classifyPostgres(err)
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		actorID,
		eventType,
		&registration.ID,
		nil,
		map[string]any{
			"previousVersion":       expectedVersion,
			"version":               registration.Version,
			"previousDesiredState":  registrationDesiredAudit(previous),
			"desiredState":          registrationDesiredAudit(registration),
			"changedFields":         registrationChangedFields(patch, previous, registration),
			"outcome":               "applied",
			"commandIdentityDigest": "sha256:" + sha256Hex([]byte(idempotencyKey)),
		},
	); err != nil {
		return Registration{}, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return Registration{}, false, unavailable(err)
	}
	return registration, false, nil
}

type registrationCommandRecord struct {
	Registration Registration `json:"registration"`
	ErrorReason  string       `json:"errorReason,omitempty"`
	RecordedAt   time.Time    `json:"recordedAt"`
}

func registrationCommandRejection(
	reason string,
	recordedAt time.Time,
) *RegistrationCommandRejection {
	cause := ErrConflict
	switch reason {
	case "registration_not_found":
		cause = ErrNotFound
	case "registration_patch_has_no_change":
		cause = ErrNoChange
	}
	return &RegistrationCommandRejection{
		Cause:      cause,
		Reason:     reason,
		RecordedAt: recordedAt,
	}
}

func persistRegistrationCommandRejection(
	ctx context.Context,
	transaction pgx.Tx,
	scope, idempotencyKey, requestHash, actorID, eventType string,
	registrationID string,
	expectedVersion int64,
	current Registration,
	reason string,
	statusCode int,
) (Registration, bool, error) {
	var recordedAt time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&recordedAt); err != nil {
		return Registration{}, false, unavailable(err)
	}
	record := registrationCommandRecord{
		Registration: current,
		ErrorReason:  reason,
		RecordedAt:   recordedAt,
	}
	response, _ := json.Marshal(record)
	if _, err := transaction.Exec(ctx,
		`INSERT INTO agentops_control.control_api_requests(
		   scope, idempotency_key, request_hash, status_code, response, actor_id
		 ) VALUES ($1, $2, $3, $4, $5, $6)`,
		scope,
		idempotencyKey,
		requestHash,
		statusCode,
		response,
		actorID,
	); err != nil {
		return Registration{}, false, classifyPostgres(err)
	}
	var auditRegistrationID *string
	var currentVersion *int64
	if current.ID != "" {
		auditRegistrationID = &current.ID
		currentVersion = &current.Version
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		actorID,
		eventType,
		auditRegistrationID,
		nil,
		map[string]any{
			"registrationId":        registrationID,
			"expectedVersion":       expectedVersion,
			"currentVersion":        currentVersion,
			"outcome":               map[bool]string{true: "version_conflict", false: "rejected"}[reason == "registration_version_mismatch"],
			"reason":                reason,
			"commandIdentityDigest": "sha256:" + sha256Hex([]byte(idempotencyKey)),
			"recordedAt":            recordedAt,
		},
	); err != nil {
		return Registration{}, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return Registration{}, false, unavailable(err)
	}
	return current, false, registrationCommandRejection(reason, recordedAt)
}

func registrationDesiredAudit(registration Registration) map[string]bool {
	return map[string]bool{
		"enabled":             registration.Enabled,
		"issueMonitorEnabled": registration.IssueMonitorEnabled,
		"prMonitorEnabled":    registration.PRMonitorEnabled,
		"executionEnabled":    registration.ExecutionEnabled,
	}
}

func registrationChangedFields(
	patch RegistrationPatch,
	previous, current Registration,
) map[string]map[string]bool {
	changed := make(map[string]map[string]bool)
	if patch.Enabled != nil {
		changed["enabled"] = map[string]bool{
			"previous": previous.Enabled,
			"current":  current.Enabled,
		}
	}
	if patch.IssueMonitorEnabled != nil {
		changed["issueMonitorEnabled"] = map[string]bool{
			"previous": previous.IssueMonitorEnabled,
			"current":  current.IssueMonitorEnabled,
		}
	}
	if patch.PRMonitorEnabled != nil {
		changed["prMonitorEnabled"] = map[string]bool{
			"previous": previous.PRMonitorEnabled,
			"current":  current.PRMonitorEnabled,
		}
	}
	if patch.ExecutionEnabled != nil {
		changed["executionEnabled"] = map[string]bool{
			"previous": previous.ExecutionEnabled,
			"current":  current.ExecutionEnabled,
		}
	}
	return changed
}

func registrationPatchChanges(patch RegistrationPatch, previous Registration) bool {
	return (patch.Enabled != nil && *patch.Enabled != previous.Enabled) ||
		(patch.IssueMonitorEnabled != nil &&
			*patch.IssueMonitorEnabled != previous.IssueMonitorEnabled) ||
		(patch.PRMonitorEnabled != nil &&
			*patch.PRMonitorEnabled != previous.PRMonitorEnabled) ||
		(patch.ExecutionEnabled != nil &&
			*patch.ExecutionEnabled != previous.ExecutionEnabled) ||
		len(patch.Configuration) > 0
}

func (store *Store) UpsertActualState(
	ctx context.Context,
	registration Registration,
	component, state, supervisorID string,
	lastError error,
) error {
	var errorMessage *string
	if lastError != nil {
		message := lastError.Error()
		errorMessage = &message
	}
	_, err := store.pool.Exec(ctx,
		`INSERT INTO agentops_control.monitor_actual_states(
		   registration_id, component, registration_version, state,
		   supervisor_id, observed_at, last_healthy_at, last_error
		 ) VALUES (
		   $1, $2, $3, $4, $5, clock_timestamp(),
		   CASE WHEN $4 = 'running' THEN clock_timestamp() ELSE NULL END, $6
		 )
		 ON CONFLICT (registration_id, component) DO UPDATE
		   SET registration_version = EXCLUDED.registration_version,
		       state = EXCLUDED.state,
		       supervisor_id = EXCLUDED.supervisor_id,
		       observed_at = EXCLUDED.observed_at,
		       last_healthy_at = CASE
		         WHEN EXCLUDED.state = 'running' THEN EXCLUDED.observed_at
		         ELSE agentops_control.monitor_actual_states.last_healthy_at
		       END,
		       last_error = EXCLUDED.last_error
		 WHERE agentops_control.monitor_actual_states.registration_version
		       <= EXCLUDED.registration_version`,
		registration.ID,
		component,
		registration.Version,
		state,
		supervisorID,
		errorMessage,
	)
	return unavailable(err)
}

func (store *Store) SaveMonitorCursor(
	ctx context.Context,
	registrationID, kind string,
	cursor map[string]any,
	observedAt time.Time,
) error {
	_, err := store.pool.Exec(ctx,
		`INSERT INTO agentops_control.monitor_cursors(
		   registration_id, monitor_kind, cursor, observed_at
		 ) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (registration_id, monitor_kind) DO UPDATE
		   SET cursor = EXCLUDED.cursor,
		       observed_at = EXCLUDED.observed_at,
		       updated_at = clock_timestamp()
		 WHERE agentops_control.monitor_cursors.observed_at < EXCLUDED.observed_at`,
		registrationID,
		kind,
		cursor,
		observedAt,
	)
	return unavailable(err)
}

func (store *Store) MonitorCursor(
	ctx context.Context,
	registrationID, kind string,
) (map[string]any, error) {
	var cursor map[string]any
	err := store.pool.QueryRow(ctx,
		`SELECT cursor
		   FROM agentops_control.monitor_cursors
		  WHERE registration_id = $1 AND monitor_kind = $2`,
		registrationID,
		kind,
	).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{}, nil
	}
	return cursor, unavailable(err)
}

func (store *Store) EnqueueWork(
	ctx context.Context,
	registration Registration,
	sourceKind, sourceKey string,
	item WorkItem,
) (string, bool, error) {
	if !registration.Enabled || !registration.ExecutionEnabled {
		return "", false, ErrStaleRegistration
	}
	jobType, payload, err := item.QueuedJob(sourceKind, registration.MergeMethod())
	if err != nil {
		return "", false, err
	}
	idempotencyKey := item.IdempotencyKey()
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", false, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		advisoryRequestKey(registration.ID, idempotencyKey),
	); err != nil {
		return "", false, unavailable(err)
	}
	var current Registration
	current, err = scanRegistration(transaction.QueryRow(
		ctx,
		registrationSelect+` WHERE id = $1 FOR SHARE`,
		registration.ID,
	))
	if err != nil {
		return "", false, unavailable(err)
	}
	if !current.Enabled || !current.ExecutionEnabled || current.Version != registration.Version {
		return "", false, ErrStaleRegistration
	}
	var lifecycleMode string
	if err := transaction.QueryRow(ctx, `
		SELECT mode
		  FROM agentops_control.lifecycle_state
		 WHERE singleton
		 FOR SHARE
	`).Scan(&lifecycleMode); err != nil {
		return "", false, unavailable(err)
	}
	if lifecycleMode != string(lifecycle.ModeActive) {
		return "", false, ErrOperatingMode
	}
	var existingID, existingStatus string
	var existingVersion int64
	var existingLastError *string
	var sameType, samePayload bool
	err = transaction.QueryRow(ctx,
		`SELECT id, registration_version, status, last_error,
		        job_type = $3, payload = $4::jsonb
		   FROM agentops_control.jobs
		  WHERE registration_id = $1 AND idempotency_key = $2`,
		registration.ID,
		idempotencyKey,
		jobType,
		payload,
	).Scan(
		&existingID,
		&existingVersion,
		&existingStatus,
		&existingLastError,
		&sameType,
		&samePayload,
	)
	if err == nil {
		if !sameType || !samePayload {
			return "", false, ErrIdempotencyConflict
		}
		requeued, err := requeueAfterRegistrationChange(
			ctx,
			transaction,
			registration,
			existingID,
			existingVersion,
			existingStatus,
			existingLastError,
			sourceKind,
			sourceKey,
			idempotencyKey,
		)
		if err != nil {
			return "", false, err
		}
		if err := transaction.Commit(ctx); err != nil {
			return "", false, unavailable(err)
		}
		return existingID, !requeued, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, unavailable(err)
	}
	id, err := randomUUID()
	if err != nil {
		return "", false, err
	}
	tag, err := transaction.Exec(ctx,
		`INSERT INTO agentops_control.jobs(
		   id, registration_id, registration_version, source_kind, source_key,
		   idempotency_key, job_type, payload
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (registration_id, idempotency_key) DO NOTHING`,
		id,
		registration.ID,
		registration.Version,
		sourceKind,
		sourceKey,
		idempotencyKey,
		jobType,
		payload,
	)
	if err != nil {
		return "", false, classifyPostgres(err)
	}
	if tag.RowsAffected() == 0 {
		if err := transaction.QueryRow(ctx,
			`SELECT id, registration_version, status, last_error,
			        job_type = $3, payload = $4::jsonb
			   FROM agentops_control.jobs
			  WHERE registration_id = $1 AND idempotency_key = $2`,
			registration.ID,
			idempotencyKey,
			jobType,
			payload,
		).Scan(
			&existingID,
			&existingVersion,
			&existingStatus,
			&existingLastError,
			&sameType,
			&samePayload,
		); err != nil {
			return "", false, unavailable(err)
		}
		if !sameType || !samePayload {
			return "", false, ErrIdempotencyConflict
		}
		requeued, err := requeueAfterRegistrationChange(
			ctx,
			transaction,
			registration,
			existingID,
			existingVersion,
			existingStatus,
			existingLastError,
			sourceKind,
			sourceKey,
			idempotencyKey,
		)
		if err != nil {
			return "", false, err
		}
		if err := transaction.Commit(ctx); err != nil {
			return "", false, unavailable(err)
		}
		return existingID, !requeued, nil
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		"agentops-control",
		"job.enqueued",
		&registration.ID,
		&id,
		map[string]any{
			"sourceKind":     sourceKind,
			"sourceKey":      sourceKey,
			"idempotencyKey": idempotencyKey,
			"jobType":        jobType,
		},
	); err != nil {
		return "", false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return "", false, unavailable(err)
	}
	return id, false, nil
}

const registrationChangedBeforeLease = "registration changed before lease acquisition"

func requeueAfterRegistrationChange(
	ctx context.Context,
	transaction pgx.Tx,
	registration Registration,
	jobID string,
	existingVersion int64,
	existingStatus string,
	existingLastError *string,
	sourceKind, sourceKey, idempotencyKey string,
) (bool, error) {
	if existingVersion == registration.Version ||
		existingStatus != "rejected" ||
		existingLastError == nil ||
		*existingLastError != registrationChangedBeforeLease {
		return false, nil
	}
	tag, err := transaction.Exec(ctx,
		`UPDATE agentops_control.jobs
		    SET registration_version = $2,
		        status = 'queued',
		        available_at = clock_timestamp(),
		        finished_at = NULL,
		        last_error = NULL,
		        updated_at = clock_timestamp()
		  WHERE id = $1
		    AND registration_version = $3
		    AND status = 'rejected'
		    AND last_error = $4`,
		jobID,
		registration.Version,
		existingVersion,
		registrationChangedBeforeLease,
	)
	if err != nil {
		return false, classifyPostgres(err)
	}
	if tag.RowsAffected() != 1 {
		return false, ErrConflict
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		"agentops-control",
		"job.requeued_after_registration_change",
		&registration.ID,
		&jobID,
		map[string]any{
			"fromRegistrationVersion": existingVersion,
			"toRegistrationVersion":   registration.Version,
			"sourceKind":              sourceKind,
			"sourceKey":               sourceKey,
			"idempotencyKey":          idempotencyKey,
		},
	); err != nil {
		return false, err
	}
	return true, nil
}

func (store *Store) ReceiveWebhook(
	ctx context.Context,
	deliveryKey, repository, event string,
	action *string,
	headers map[string]string,
	payload map[string]any,
) (WebhookReceipt, error) {
	if strings.TrimSpace(deliveryKey) == "" {
		return WebhookReceipt{}, fmt.Errorf("X-GitHub-Delivery is required")
	}
	repository = strings.ToLower(strings.TrimSpace(repository))
	if !safeRepositoryIdentity(repository) {
		return WebhookReceipt{}, fmt.Errorf("payload repository.full_name is invalid")
	}
	if !supportedWebhookEvent(event) {
		return WebhookReceipt{}, fmt.Errorf("unsupported GitHub webhook event: %s", event)
	}
	id, err := randomUUID()
	if err != nil {
		return WebhookReceipt{}, err
	}
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WebhookReceipt{}, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	var insertedID, status string
	err = transaction.QueryRow(ctx,
		`INSERT INTO agentops_control.webhook_deliveries(
		   id, delivery_key, repository, event, action, headers, payload
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (delivery_key) DO NOTHING
		 RETURNING id, status`,
		id,
		deliveryKey,
		repository,
		event,
		action,
		durableHeaders(headers),
		payload,
	).Scan(&insertedID, &status)
	if err == nil {
		if err := transaction.Commit(ctx); err != nil {
			return WebhookReceipt{}, unavailable(err)
		}
		return WebhookReceipt{DeliveryID: insertedID, Status: status}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return WebhookReceipt{}, classifyPostgres(err)
	}
	var sameRepository, sameEvent, sameAction, samePayload bool
	if err := transaction.QueryRow(ctx,
		`SELECT id, status, repository = $2, event = $3,
		        action IS NOT DISTINCT FROM $4, payload = $5::jsonb
		   FROM agentops_control.webhook_deliveries
		  WHERE delivery_key = $1
		  FOR UPDATE`,
		deliveryKey,
		repository,
		event,
		action,
		payload,
	).Scan(
		&insertedID,
		&status,
		&sameRepository,
		&sameEvent,
		&sameAction,
		&samePayload,
	); err != nil {
		return WebhookReceipt{}, unavailable(err)
	}
	if !sameRepository || !sameEvent || !sameAction || !samePayload {
		return WebhookReceipt{}, ErrIdempotencyConflict
	}
	if err := transaction.Commit(ctx); err != nil {
		return WebhookReceipt{}, unavailable(err)
	}
	return WebhookReceipt{DeliveryID: insertedID, Duplicate: true, Status: status}, nil
}

func (store *Store) ClaimWebhook(
	ctx context.Context,
	leaseDuration time.Duration,
) (*ClaimedDelivery, error) {
	token, err := randomUUID()
	if err != nil {
		return nil, err
	}
	row := store.pool.QueryRow(ctx,
		`WITH lifecycle AS (
		   SELECT mode
		     FROM agentops_control.lifecycle_state
		    WHERE singleton
		    FOR SHARE
		 ),
		 candidate AS (
		   SELECT id
		     FROM agentops_control.webhook_deliveries
		    WHERE EXISTS (
		            SELECT 1 FROM lifecycle
		             WHERE mode IN ('MONITOR_ONLY', 'ACTIVE')
		          )
		      AND (
		            status = 'pending'
		         OR (status = 'failed' AND next_retry_at <= clock_timestamp())
		          )
		    ORDER BY received_at, id
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1
		 )
		 UPDATE agentops_control.webhook_deliveries delivery
		    SET status = 'processing',
		        processing_token = $1,
		        processing_expires_at = clock_timestamp() + $2::interval,
		        route_attempts = route_attempts + 1,
		        next_retry_at = NULL,
		        last_error = NULL,
		        updated_at = clock_timestamp()
		   FROM candidate
		  WHERE delivery.id = candidate.id
		  RETURNING delivery.id, delivery.delivery_key, delivery.repository,
		            delivery.event, delivery.action, delivery.payload,
		            delivery.route_attempts, delivery.registration_id,
		            delivery.registration_version`,
		token,
		intervalLiteral(leaseDuration),
	)
	var claimed ClaimedDelivery
	err = row.Scan(
		&claimed.ID,
		&claimed.DeliveryKey,
		&claimed.Repository,
		&claimed.Event,
		&claimed.Action,
		&claimed.Payload,
		&claimed.RouteAttempts,
		&claimed.RegistrationID,
		&claimed.RegistrationVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable(err)
	}
	claimed.Token = token
	return &claimed, nil
}

func (store *Store) BindWebhook(
	ctx context.Context,
	claim ClaimedDelivery,
	registration Registration,
) error {
	tag, err := store.pool.Exec(ctx,
		`UPDATE agentops_control.webhook_deliveries
		    SET registration_id = $3,
		        registration_version = COALESCE(registration_version, $4),
		        updated_at = clock_timestamp()
		  WHERE id = $1 AND processing_token = $2
		    AND status = 'processing'
		    AND processing_expires_at > clock_timestamp()
		    AND (registration_id IS NULL OR registration_id = $3)
		    AND (registration_version IS NULL OR registration_version = $4)`,
		claim.ID,
		claim.Token,
		registration.ID,
		registration.Version,
	)
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleRegistration
	}
	return nil
}

func (store *Store) FinishWebhook(
	ctx context.Context,
	claim ClaimedDelivery,
	status, reason string,
) error {
	if status != "processed" && status != "ignored" && status != "failed" {
		return fmt.Errorf("invalid webhook outcome %s", status)
	}
	retryDelay := time.Duration(0)
	if status == "failed" {
		retryDelay = time.Second * time.Duration(1<<min(claim.RouteAttempts-1, 6))
	}
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	var registrationID *string
	err = transaction.QueryRow(ctx,
		`UPDATE agentops_control.webhook_deliveries
		    SET status = $3,
		        ignored_reason = CASE WHEN $3 = 'ignored' THEN $4 ELSE NULL END,
		        last_error = CASE WHEN $3 = 'failed' THEN $4 ELSE NULL END,
		        next_retry_at = CASE
		          WHEN $3 = 'failed' THEN clock_timestamp() + $5::interval
		          ELSE NULL
		        END,
		        processing_token = NULL,
		        processing_expires_at = NULL,
		        updated_at = clock_timestamp()
		  WHERE id = $1 AND processing_token = $2
		    AND status = 'processing'
		    AND processing_expires_at > clock_timestamp()
		  RETURNING registration_id`,
		claim.ID,
		claim.Token,
		status,
		nullIfEmpty(reason),
		intervalLiteral(retryDelay),
	).Scan(&registrationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return unavailable(err)
	}
	eventType := "webhook." + status
	if err := appendAuditTx(
		ctx,
		transaction,
		"agentops-control",
		eventType,
		registrationID,
		nil,
		map[string]any{
			"deliveryId":    claim.ID,
			"deliveryKey":   claim.DeliveryKey,
			"reason":        reason,
			"routeAttempts": claim.RouteAttempts,
		},
	); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

func (store *Store) RecoverInterruptedWebhooks(ctx context.Context) (int64, error) {
	tag, err := store.pool.Exec(ctx,
		`UPDATE agentops_control.webhook_deliveries
		    SET status = 'failed',
		        last_error = 'processing ownership expired',
		        next_retry_at = clock_timestamp(),
		        processing_token = NULL,
		        processing_expires_at = NULL,
		        updated_at = clock_timestamp()
		  WHERE status = 'processing'
		    AND processing_expires_at <= clock_timestamp()`,
	)
	if err != nil {
		return 0, unavailable(err)
	}
	return tag.RowsAffected(), nil
}

func (store *Store) DeliveryStatus(
	ctx context.Context,
	deliveryID string,
) (DeliveryStatus, error) {
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return DeliveryStatus{}, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	var status DeliveryStatus
	err = transaction.QueryRow(ctx,
		`SELECT id, delivery_key, repository, event, action, status,
		        ignored_reason, last_error, route_attempts, registration_id,
		        registration_version, received_at, updated_at
		   FROM agentops_control.webhook_deliveries
		  WHERE id = $1`,
		deliveryID,
	).Scan(
		&status.ID,
		&status.DeliveryKey,
		&status.Repository,
		&status.Event,
		&status.Action,
		&status.Status,
		&status.IgnoredReason,
		&status.LastError,
		&status.RouteAttempts,
		&status.RegistrationID,
		&status.RegistrationVersion,
		&status.ReceivedAt,
		&status.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryStatus{}, ErrNotFound
	}
	if err != nil {
		return DeliveryStatus{}, unavailable(err)
	}
	status.RetryAttempts = make([]DeliveryRetryAttempt, 0)
	rows, err := transaction.Query(ctx,
		`SELECT id, status, reason, observed_route_attempts, created_at
		   FROM agentops_control.delivery_retry_attempts
		  WHERE delivery_id = $1
		  ORDER BY created_at, id`,
		deliveryID,
	)
	if err != nil {
		return DeliveryStatus{}, unavailable(err)
	}
	for rows.Next() {
		var attempt DeliveryRetryAttempt
		if err := rows.Scan(
			&attempt.AttemptID,
			&attempt.Status,
			&attempt.Reason,
			&attempt.ObservedRouteAttempts,
			&attempt.CreatedAt,
		); err != nil {
			rows.Close()
			return DeliveryStatus{}, unavailable(err)
		}
		status.RetryAttempts = append(status.RetryAttempts, attempt)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DeliveryStatus{}, unavailable(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return DeliveryStatus{}, unavailable(err)
	}
	return status, nil
}

func (store *Store) RetryWebhook(
	ctx context.Context,
	deliveryID, idempotencyKey, actorID string,
	observedAttempts int,
	expectedRegistrationID string,
	expectedRegistrationVersion int64,
) (RetryResult, bool, error) {
	requestBody, _ := json.Marshal(map[string]any{
		"deliveryId":                  deliveryID,
		"expectedRegistrationId":      expectedRegistrationID,
		"expectedRegistrationVersion": expectedRegistrationVersion,
		"observedAttempts":            observedAttempts,
	})
	requestHash := sha256Hex(requestBody)
	scope := "delivery:retry"
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RetryResult{}, false, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if _, err := transaction.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		advisoryRequestKey(scope, idempotencyKey),
	); err != nil {
		return RetryResult{}, false, unavailable(err)
	}
	var storedHash string
	var storedResponse []byte
	var storedStatus int
	err = transaction.QueryRow(ctx,
		`SELECT request_hash, status_code, response
		   FROM agentops_control.control_api_requests
		  WHERE scope = $1 AND idempotency_key = $2
		  FOR UPDATE`,
		scope,
		idempotencyKey,
	).Scan(&storedHash, &storedStatus, &storedResponse)
	if err == nil {
		if storedHash != requestHash {
			return RetryResult{}, false, ErrIdempotencyConflict
		}
		if storedStatus == 409 {
			var conflict DeliveryRetryConflict
			if err := json.Unmarshal(storedResponse, &conflict); err != nil {
				return RetryResult{}, false, unavailable(err)
			}
			if err := transaction.Commit(ctx); err != nil {
				return RetryResult{}, false, unavailable(err)
			}
			return RetryResult{}, true, &conflict
		}
		var result RetryResult
		if err := json.Unmarshal(storedResponse, &result); err != nil {
			return RetryResult{}, false, unavailable(err)
		}
		if err := transaction.Commit(ctx); err != nil {
			return RetryResult{}, false, unavailable(err)
		}
		return result, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RetryResult{}, false, unavailable(err)
	}
	var status string
	var routeAttempts int
	var registrationID *string
	var registrationVersion *int64
	if err := transaction.QueryRow(ctx,
		`SELECT status, route_attempts, registration_id, registration_version
		   FROM agentops_control.webhook_deliveries
		  WHERE id = $1 FOR UPDATE`,
		deliveryID,
	).Scan(&status, &routeAttempts, &registrationID, &registrationVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RetryResult{}, false, ErrNotFound
		}
		return RetryResult{}, false, unavailable(err)
	}
	if registrationID == nil || registrationVersion == nil ||
		*registrationID != expectedRegistrationID ||
		*registrationVersion != expectedRegistrationVersion {
		conflict := &DeliveryRetryConflict{
			Reason: "registration_fence_mismatch", State: status, RouteAttempts: routeAttempts,
		}
		if registrationID != nil {
			conflict.RegistrationID = *registrationID
		}
		if registrationVersion != nil {
			conflict.RegistrationVersion = *registrationVersion
		}
		return persistRetryRejection(
			ctx,
			transaction,
			deliveryID,
			idempotencyKey,
			actorID,
			observedAttempts,
			requestHash,
			registrationID,
			conflict,
		)
	}
	if status != "failed" || routeAttempts != observedAttempts {
		reason := "observed_attempts_stale"
		if status != "failed" {
			reason = "delivery_not_retryable"
		}
		conflict := &DeliveryRetryConflict{
			Reason: reason, State: status, RouteAttempts: routeAttempts,
		}
		if registrationID != nil {
			conflict.RegistrationID = *registrationID
		}
		if registrationVersion != nil {
			conflict.RegistrationVersion = *registrationVersion
		}
		return persistRetryRejection(
			ctx,
			transaction,
			deliveryID,
			idempotencyKey,
			actorID,
			observedAttempts,
			requestHash,
			registrationID,
			conflict,
		)
	}
	if registrationID != nil {
		var enabled bool
		var executionEnabled bool
		var currentVersion int64
		if err := transaction.QueryRow(ctx,
			`SELECT enabled, execution_enabled, version
				   FROM agentops_control.repository_registrations
				  WHERE id = $1
				  FOR SHARE`,
			*registrationID,
		).Scan(&enabled, &executionEnabled, &currentVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				conflict := &DeliveryRetryConflict{
					Reason: "registration_missing", State: status, RouteAttempts: routeAttempts,
					RegistrationID:      *registrationID,
					RegistrationVersion: *registrationVersion,
				}
				return persistRetryRejection(
					ctx, transaction, deliveryID, idempotencyKey, actorID,
					observedAttempts, requestHash, registrationID, conflict,
				)
			}
			return RetryResult{}, false, unavailable(err)
		}
		if !enabled {
			conflict := &DeliveryRetryConflict{
				Reason: "registration_disabled", State: status, RouteAttempts: routeAttempts,
				RegistrationID:      *registrationID,
				RegistrationVersion: currentVersion,
			}
			return persistRetryRejection(
				ctx, transaction, deliveryID, idempotencyKey, actorID,
				observedAttempts, requestHash, registrationID, conflict,
			)
		}
		if !executionEnabled {
			conflict := &DeliveryRetryConflict{
				Reason: "execution_disabled", State: status, RouteAttempts: routeAttempts,
				RegistrationID:      *registrationID,
				RegistrationVersion: currentVersion,
			}
			return persistRetryRejection(
				ctx, transaction, deliveryID, idempotencyKey, actorID,
				observedAttempts, requestHash, registrationID, conflict,
			)
		}
		if registrationVersion == nil || currentVersion != *registrationVersion {
			conflict := &DeliveryRetryConflict{
				Reason: "registration_stale", State: status, RouteAttempts: routeAttempts,
				RegistrationID:      *registrationID,
				RegistrationVersion: currentVersion,
			}
			return persistRetryRejection(
				ctx, transaction, deliveryID, idempotencyKey, actorID,
				observedAttempts, requestHash, registrationID, conflict,
			)
		}
	}
	attemptID, err := randomUUID()
	if err != nil {
		return RetryResult{}, false, err
	}
	var recordedAt time.Time
	if err := transaction.QueryRow(ctx,
		`INSERT INTO agentops_control.delivery_retry_attempts(
		   id, delivery_id, idempotency_key, observed_route_attempts, actor_id, status
		 ) VALUES ($1, $2, $3, $4, $5, 'accepted')
		 RETURNING created_at`,
		attemptID,
		deliveryID,
		idempotencyKey,
		observedAttempts,
		actorID,
	).Scan(&recordedAt); err != nil {
		return RetryResult{}, false, classifyPostgres(err)
	}
	if _, err := transaction.Exec(ctx,
		`UPDATE agentops_control.webhook_deliveries
		    SET status = 'pending', next_retry_at = NULL, last_error = NULL,
		        updated_at = clock_timestamp()
		  WHERE id = $1`,
		deliveryID,
	); err != nil {
		return RetryResult{}, false, unavailable(err)
	}
	result := RetryResult{
		AttemptID: attemptID, DeliveryID: deliveryID, State: "pending", Cancellable: false,
		RecordedAt: recordedAt,
	}
	response, _ := json.Marshal(result)
	if _, err := transaction.Exec(ctx,
		`INSERT INTO agentops_control.control_api_requests(
		   scope, idempotency_key, request_hash, status_code, response, actor_id
		 ) VALUES ($1, $2, $3, 202, $4, $5)`,
		scope,
		idempotencyKey,
		requestHash,
		response,
		actorID,
	); err != nil {
		return RetryResult{}, false, classifyPostgres(err)
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		actorID,
		"webhook.retry.accepted",
		registrationID,
		nil,
		map[string]any{
			"deliveryId":                  deliveryID,
			"attemptId":                   attemptID,
			"observedRouteAttempts":       observedAttempts,
			"expectedRegistrationId":      expectedRegistrationID,
			"expectedRegistrationVersion": expectedRegistrationVersion,
		},
	); err != nil {
		return RetryResult{}, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return RetryResult{}, false, unavailable(err)
	}
	return result, false, nil
}

func persistRetryRejection(
	ctx context.Context,
	transaction pgx.Tx,
	deliveryID, idempotencyKey, actorID string,
	observedAttempts int,
	requestHash string,
	registrationID *string,
	conflict *DeliveryRetryConflict,
) (RetryResult, bool, error) {
	attemptID, err := randomUUID()
	if err != nil {
		return RetryResult{}, false, err
	}
	conflict.AttemptID = attemptID
	if err := transaction.QueryRow(ctx,
		`INSERT INTO agentops_control.delivery_retry_attempts(
		   id, delivery_id, idempotency_key, observed_route_attempts,
		   actor_id, status, reason
		 ) VALUES ($1, $2, $3, $4, $5, 'rejected', $6)
		 RETURNING created_at`,
		attemptID,
		deliveryID,
		idempotencyKey,
		observedAttempts,
		actorID,
		conflict.Reason,
	).Scan(&conflict.RecordedAt); err != nil {
		return RetryResult{}, false, classifyPostgres(err)
	}
	response, _ := json.Marshal(conflict)
	if _, err := transaction.Exec(ctx,
		`INSERT INTO agentops_control.control_api_requests(
		   scope, idempotency_key, request_hash, status_code, response, actor_id
		 ) VALUES ($1, $2, $3, 409, $4, $5)`,
		"delivery:retry",
		idempotencyKey,
		requestHash,
		response,
		actorID,
	); err != nil {
		return RetryResult{}, false, classifyPostgres(err)
	}
	if err := appendAuditTx(
		ctx,
		transaction,
		actorID,
		"webhook.retry.rejected",
		registrationID,
		nil,
		map[string]any{
			"deliveryId":            deliveryID,
			"attemptId":             attemptID,
			"observedRouteAttempts": observedAttempts,
			"reason":                conflict.Reason,
			"state":                 conflict.State,
			"routeAttempts":         conflict.RouteAttempts,
		},
	); err != nil {
		return RetryResult{}, false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return RetryResult{}, false, unavailable(err)
	}
	return RetryResult{}, false, conflict
}

func (store *Store) AppendAudit(
	ctx context.Context,
	actorID, eventType string,
	registrationID, jobID *string,
	details map[string]any,
) error {
	_, err := store.pool.Exec(ctx,
		`INSERT INTO agentops_control.runtime_audit(
		   actor_type, actor_id, event_type, registration_id, job_id, details
		 ) VALUES ('control', $1, $2, $3, $4, $5)`,
		actorID,
		eventType,
		registrationID,
		jobID,
		details,
	)
	return unavailable(err)
}

// ReconcileGateEscalations resolves stale observations and creates at most one
// escalation per job/gate/head. The caller supplies the observation time so
// tests can exercise the SLA boundary without sleeping.
func (store *Store) ReconcileGateEscalations(
	ctx context.Context,
	now time.Time,
) (int64, error) {
	var inserted int64
	err := store.pool.QueryRow(ctx, `
		WITH latest AS (
		  SELECT DISTINCT ON (progress.job_id)
		         progress.*, job.status AS job_status,
		         registration.enabled AS registration_enabled,
		         registration.configuration,
		         release.status AS release_status,
		         release.final_head AS release_final_head
		    FROM agentops_control.development_progress_events progress
		    JOIN agentops_control.jobs job ON job.id = progress.job_id
		    JOIN agentops_control.repository_registrations registration
		      ON registration.id = progress.registration_id
		    LEFT JOIN agentops_control.releases release
		      ON release.id = progress.release_id
		   ORDER BY progress.job_id, progress.occurred_at DESC, progress.id DESC
		), resolved AS (
		  UPDATE agentops_control.human_escalations escalation
		     SET resolved_at = $1
		    FROM latest
		   WHERE escalation.job_id = latest.job_id
		     AND escalation.resolved_at IS NULL
		     AND (
		       latest.job_status IN ('succeeded', 'failed', 'cancelled', 'rejected')
		       OR latest.release_status = 'merged'
		       OR latest.gate_key IS DISTINCT FROM escalation.gate_key
		       OR latest.state NOT IN ('running', 'waiting')
		     )
		  RETURNING escalation.id
		), configured AS (
		  SELECT latest.*,
		         GREATEST(60, LEAST(2592000, COALESCE(
		           CASE
		             WHEN jsonb_typeof(
		               latest.configuration->'gateTimeoutSeconds'->latest.gate_key
		             ) = 'number'
		             AND pg_input_is_valid(
		               latest.configuration->'gateTimeoutSeconds'->>latest.gate_key,
		               'integer'
		             )
		             THEN (
		               latest.configuration->'gateTimeoutSeconds'->>latest.gate_key
		             )::integer
		           END,
		           CASE
		             WHEN jsonb_typeof(
		               latest.configuration->'gateTimeoutSeconds'->'default'
		             ) = 'number'
		             AND pg_input_is_valid(
		               latest.configuration->'gateTimeoutSeconds'->>'default',
		               'integer'
		             )
		             THEN (
		               latest.configuration->'gateTimeoutSeconds'->>'default'
		             )::integer
		           END,
		           3600
		         ))) AS timeout_seconds
		    FROM latest
		   WHERE latest.registration_enabled
		     AND latest.job_status = 'leased'
		     AND latest.release_status IS DISTINCT FROM 'merged'
		     AND latest.gate_key IS NOT NULL
		     AND latest.state IN ('running', 'waiting')
		), inserted AS (
		  INSERT INTO agentops_control.human_escalations(
		    registration_id, registration_version, job_id, attempt_id, release_id,
		    repository, subject_kind, subject_number, gate_key, target_sha,
		    reason, evidence, human_action, gate_entered_at, escalated_at
		  )
		  SELECT configured.registration_id, configured.registration_version,
		         configured.job_id, configured.attempt_id, configured.release_id,
		         configured.repository, configured.subject_kind,
		         configured.subject_number, configured.gate_key,
		         COALESCE(configured.head_sha, configured.release_final_head),
		         format(
		           'gate %s exceeded its %s second SLA',
		           configured.gate_key, configured.timeout_seconds
		         ),
		         jsonb_build_object(
		           'progressEventId', configured.id,
		           'phase', configured.phase,
		           'step', configured.step,
		           'state', configured.state,
		           'timeoutSeconds', configured.timeout_seconds,
		           'observedAt', $1,
		           'targetSha', COALESCE(
		             configured.head_sha, configured.release_final_head
		           )
		         ),
		         CASE configured.gate_key
		           WHEN 'planning' THEN 'inspect planning output and update the Issue requirements or resume the retained session'
		           WHEN 'design' THEN 'record the required design decision or repair the design provider configuration'
		           WHEN 'repository-graders' THEN 'inspect the retained worktree and repair or restart the failing grader'
		           WHEN 'review' THEN 'inspect perspective evidence and push a corrected current head or approve the documented exception'
		           WHEN 'merge' THEN 'resolve current-head GitHub checks, reviews, threads, or mergeability blockers'
		           ELSE 'inspect lease recovery and safely retry or reapply the ready label'
		         END,
		         configured.occurred_at, $1
		    FROM configured
		   WHERE $1 - configured.occurred_at
		         >= make_interval(secs => configured.timeout_seconds)
		  ON CONFLICT (job_id, gate_key, COALESCE(target_sha, '')) DO NOTHING
		  RETURNING id
		)
		SELECT count(*) FROM inserted`, now.UTC()).Scan(&inserted)
	if err != nil {
		return 0, unavailable(err)
	}
	return inserted, nil
}

const (
	// Per-card orientation bounds. The full history stays queryable through
	// `agentopsctl progress`, which is paged by its own explicit --limit.
	developmentProgressIssueLimit   = 20
	developmentProgressHistoryLimit = 12
)

const developmentProgressSelect = `SELECT
       progress.id, progress.registration_id, progress.registration_version,
       progress.job_id, progress.attempt_id, attempt.attempt_number,
       progress.release_id, progress.repository, progress.subject_kind,
       progress.subject_number, progress.parent_issue_number,
       progress.worker_id, progress.event_key,
       progress.phase, progress.step, progress.state, progress.summary,
       progress.next_gate, progress.blocker, progress.session_name,
       progress.worktree_path, progress.branch, progress.pull_request_number,
	   progress.head_sha, progress.review_round, progress.review_outcome,
	   progress.gate_key, progress.human_action,
	   progress.occurred_at, job.status, job.last_error, lease.heartbeat_at,
	   job.job_type,
	   job.updated_at, job.available_at, job.finished_at, attempt.status,
	   lease.expires_at, release.status, release.final_head,
	   job.result->>'outcome', job.failure->>'code',
	   CASE WHEN job.failure ? 'retryable'
	        THEN (job.failure->>'retryable')::boolean END,
	   escalation.id, escalation.reason,
	   COALESCE(escalation.evidence, '{}'::jsonb),
	   escalation.human_action, escalation.gate_entered_at,
	   escalation.escalated_at, escalation.target_sha,
	   COALESCE(review_evidence.perspectives, '[]'::jsonb),
	   COALESCE(lineage.details, '{}'::jsonb)
  FROM agentops_control.development_progress_events progress
  JOIN agentops_control.jobs job ON job.id = progress.job_id
  JOIN agentops_control.job_attempts attempt ON attempt.id = progress.attempt_id
  LEFT JOIN agentops_control.job_leases lease
	ON lease.attempt_id = progress.attempt_id AND lease.status = 'active'
  LEFT JOIN agentops_control.releases release ON release.id = progress.release_id
  LEFT JOIN LATERAL (
	SELECT current_escalation.*
	  FROM agentops_control.human_escalations current_escalation
	 WHERE current_escalation.job_id = progress.job_id
	   AND current_escalation.resolved_at IS NULL
	 ORDER BY current_escalation.escalated_at DESC, current_escalation.id DESC
	 LIMIT 1
  ) escalation ON TRUE
  LEFT JOIN LATERAL (
	SELECT jsonb_agg(
	         jsonb_build_object(
	           'perspective', perspective.perspective,
	           'verdict', perspective.verdict,
	           'findingCount', perspective.finding_count,
	           'findings', perspective.findings,
	           'completedAt', perspective.completed_at
	         ) ORDER BY perspective.perspective
	       ) AS perspectives
	  FROM agentops_control.development_review_perspectives perspective
	 WHERE perspective.review_round_id = (
	   SELECT review_round.id
	     FROM agentops_control.development_review_rounds review_round
	    WHERE review_round.job_id = progress.job_id
	      AND (
	        progress.review_round IS NULL
	        OR review_round.round = progress.review_round
	      )
	      AND (
	        progress.head_sha IS NULL
	        OR review_round.head_sha = progress.head_sha
	      )
	    ORDER BY review_round.updated_at DESC, review_round.id DESC
	    LIMIT 1
	 )
  ) review_evidence ON TRUE
  LEFT JOIN LATERAL (
	SELECT jsonb_build_object(
	         'nodeId', node.id,
	         'status', node.status,
	         'parentNodeId', node.parent_node_id,
	         'parentIssueNumber', parent.issue_number,
	         'parentBranch', node.parent_branch,
	         'parentHeadSha', node.parent_head_sha,
	         'pullRequestNumber', node.pull_request_number,
	         'headSha', node.head_sha,
	         'integratedHeadSha', node.integrated_head_sha,
	         'children', COALESCE((
	           SELECT jsonb_agg(
	             jsonb_build_object(
	               'nodeId', child.id,
	               'issueNumber', child.issue_number,
	               'status', child.status,
	               'parentHeadSha', child.parent_head_sha,
	               'pullRequestNumber', child.pull_request_number,
	               'headSha', child.head_sha,
	               'integratedHeadSha', child.integrated_head_sha
	             ) ORDER BY child.created_at, child.id
	           )
	             FROM agentops_control.development_lineage_nodes child
	            WHERE child.parent_node_id = node.id
	         ), '[]'::jsonb)
	       ) AS details
	  FROM agentops_control.development_lineage_nodes node
	  LEFT JOIN agentops_control.development_lineage_nodes parent
	    ON parent.id = node.parent_node_id
	 WHERE node.registration_id = progress.registration_id
	   AND node.repository = progress.repository
	   AND node.issue_number = progress.subject_number
	 LIMIT 1
  ) lineage ON TRUE`

func scanDevelopmentProgress(row rowScanner) (DevelopmentProgressEvent, error) {
	var event DevelopmentProgressEvent
	err := row.Scan(
		&event.ID,
		&event.RegistrationID,
		&event.RegistrationVersion,
		&event.JobID,
		&event.AttemptID,
		&event.AttemptNumber,
		&event.ReleaseID,
		&event.Repository,
		&event.SubjectKind,
		&event.SubjectNumber,
		&event.ParentIssueNumber,
		&event.WorkerID,
		&event.EventKey,
		&event.Phase,
		&event.Step,
		&event.State,
		&event.Summary,
		&event.NextGate,
		&event.Blocker,
		&event.SessionName,
		&event.WorktreePath,
		&event.Branch,
		&event.PullRequestNumber,
		&event.HeadSHA,
		&event.ReviewRound,
		&event.ReviewOutcome,
		&event.GateKey,
		&event.HumanAction,
		&event.OccurredAt,
		&event.JobStatus,
		&event.JobLastError,
		&event.LeaseHeartbeatAt,
		&event.JobType,
		&event.JobUpdatedAt,
		&event.JobAvailableAt,
		&event.JobFinishedAt,
		&event.AttemptStatus,
		&event.LeaseExpiresAt,
		&event.ReleaseStatus,
		&event.ReleaseFinalHead,
		&event.JobResultOutcome,
		&event.JobFailureCode,
		&event.JobFailureRetryable,
		&event.EscalationID,
		&event.EscalationReason,
		&event.EscalationEvidence,
		&event.EscalationHumanAction,
		&event.EscalationGateEntered,
		&event.EscalatedAt,
		&event.EscalationTargetSHA,
		&event.ReviewPerspectives,
		&event.BranchLineage,
	)
	return event, err
}

func developmentProgressActivity(event DevelopmentProgressEvent) time.Time {
	activity := event.OccurredAt
	if event.LeaseHeartbeatAt != nil && event.LeaseHeartbeatAt.After(activity) {
		activity = *event.LeaseHeartbeatAt
	}
	return activity
}

type developmentProgressQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

const developmentJobsWithoutProgressSelect = `SELECT
       job.id, job.registration_id, job.registration_version, job.release_id,
       registration.repository, job.job_type, job.status, job.last_error,
       job.created_at, job.updated_at, job.available_at,
       COALESCE(
         release.issue_number,
         CASE WHEN pg_input_is_valid(job.payload->'event'->>'number', 'bigint')
           THEN (job.payload->'event'->>'number')::bigint END,
         CASE WHEN pg_input_is_valid(job.payload->'issue'->>'number', 'bigint')
           THEN (job.payload->'issue'->>'number')::bigint END
       ) AS issue_number,
       attempt.id, attempt.attempt_number, attempt.status, attempt.worker_id,
       lease.heartbeat_at, lease.expires_at,
       release.status, release.final_head,
       job.result->>'outcome', job.failure->>'code',
       CASE WHEN job.failure ? 'retryable'
            THEN (job.failure->>'retryable')::boolean END
  FROM agentops_control.jobs job
  JOIN agentops_control.repository_registrations registration
    ON registration.id = job.registration_id
  LEFT JOIN agentops_control.releases release ON release.id = job.release_id
  LEFT JOIN LATERAL (
    SELECT current_attempt.*
      FROM agentops_control.job_attempts current_attempt
     WHERE current_attempt.job_id = job.id
     ORDER BY current_attempt.attempt_number DESC, current_attempt.id DESC
     LIMIT 1
  ) attempt ON TRUE
  LEFT JOIN agentops_control.job_leases lease
    ON lease.attempt_id = attempt.id AND lease.status = 'active'
 WHERE job.job_type IN ('agentops.triage', 'agentops.runner')
   AND NOT EXISTS (
     SELECT 1 FROM agentops_control.development_progress_events progress
      WHERE progress.job_id = job.id
   )
   AND COALESCE(
     release.issue_number,
     CASE WHEN pg_input_is_valid(job.payload->'event'->>'number', 'bigint')
       THEN (job.payload->'event'->>'number')::bigint END,
     CASE WHEN pg_input_is_valid(job.payload->'issue'->>'number', 'bigint')
       THEN (job.payload->'issue'->>'number')::bigint END
   ) IS NOT NULL`

func developmentJobsWithoutProgress(
	ctx context.Context,
	querier developmentProgressQuerier,
	where string,
	arguments ...any,
) ([]DevelopmentProgressEvent, error) {
	rows, err := querier.Query(ctx,
		developmentJobsWithoutProgressSelect+where,
		arguments...,
	)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	events := make([]DevelopmentProgressEvent, 0)
	for rows.Next() {
		var event DevelopmentProgressEvent
		var attemptID, attemptStatus, workerID *string
		var attemptNumber *int
		if err := rows.Scan(
			&event.JobID,
			&event.RegistrationID,
			&event.RegistrationVersion,
			&event.ReleaseID,
			&event.Repository,
			&event.JobType,
			&event.JobStatus,
			&event.JobLastError,
			&event.OccurredAt,
			&event.JobUpdatedAt,
			&event.JobAvailableAt,
			&event.SubjectNumber,
			&attemptID,
			&attemptNumber,
			&attemptStatus,
			&workerID,
			&event.LeaseHeartbeatAt,
			&event.LeaseExpiresAt,
			&event.ReleaseStatus,
			&event.ReleaseFinalHead,
			&event.JobResultOutcome,
			&event.JobFailureCode,
			&event.JobFailureRetryable,
		); err != nil {
			return nil, unavailable(err)
		}
		event.SubjectKind = "issue"
		event.EventKey = "job:" + event.JobStatus
		event.Phase = "intake"
		event.Step = "durable work awaiting first progress report"
		event.State = "pending"
		event.WorkerID = "unassigned"
		if attemptID != nil {
			event.AttemptID = *attemptID
		}
		if attemptNumber != nil {
			event.AttemptNumber = *attemptNumber
		}
		if attemptStatus != nil {
			event.AttemptStatus = *attemptStatus
		}
		if workerID != nil {
			event.WorkerID = *workerID
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return events, nil
}

func textPointer(value string) *string {
	return &value
}

// canonicalDevelopmentProgress folds immutable progress history with the
// authoritative job/lease/release state. It is deliberately pure so fake-clock
// tests can pin recovery and stale-display boundaries without PostgreSQL time.
func canonicalDevelopmentProgress(
	event DevelopmentProgressEvent,
	now time.Time,
) DevelopmentProgressEvent {
	projected := event
	projected.Terminal = false
	if event.ReleaseFinalHead != nil {
		projected.HeadSHA = event.ReleaseFinalHead
	}
	if projected.HumanAction == nil && projected.State == "blocked" {
		projected.HumanAction = event.NextGate
	}
	projected.GateEnteredAt = nil
	projected.GateWaitSeconds = 0
	if projected.EscalationID == nil {
		projected.EscalationEvidence = nil
	}

	if event.ReleaseStatus != nil && *event.ReleaseStatus == "merged" {
		projected.KanbanLane = "released"
		projected.Phase = "completed"
		projected.Step = "implementation released"
		projected.State = "succeeded"
		projected.Summary = textPointer("GitHub merge and release receipt are durable")
		projected.NextGate = nil
		projected.Blocker = nil
		projected.HumanAction = nil
		projected.Terminal = true
		return projected
	}

	if event.JobStatus == "failed" || event.JobStatus == "cancelled" ||
		event.JobStatus == "rejected" {
		projected.KanbanLane = "failed"
		projected.Phase = "failed"
		projected.Step = "runner job " + event.JobStatus
		projected.State = "failed"
		projected.NextGate = nil
		projected.HumanAction = textPointer(
			"inspect the failure, correct the cause, then reapply the ready label or retry the delivery",
		)
		if event.JobLastError != nil {
			projected.Blocker = event.JobLastError
		} else {
			projected.Blocker = textPointer(event.JobStatus)
		}
		projected.Terminal = true
		return projected
	}

	if (event.ReleaseStatus != nil && *event.ReleaseStatus == "abandoned") ||
		(event.JobResultOutcome != nil && *event.JobResultOutcome == "needs-human-review") {
		projected.KanbanLane = "human-escalated"
		projected.Phase = "human-review"
		projected.State = "blocked"
		projected.Terminal = true
		if projected.Blocker == nil {
			projected.Blocker = textPointer("human judgment is required")
		}
		if projected.HumanAction == nil {
			projected.HumanAction = textPointer(
				"update the Issue requirements and reapply the ready label",
			)
		}
		return projected
	}

	if event.EscalationID != nil {
		projected.KanbanLane = "human-escalated"
		projected.Phase = "human-review"
		projected.State = "blocked"
		projected.Blocker = event.EscalationReason
		projected.HumanAction = event.EscalationHumanAction
		projected.GateEnteredAt = event.EscalationGateEntered
		if event.EscalationTargetSHA != nil {
			projected.HeadSHA = event.EscalationTargetSHA
		}
		if event.GateKey != nil && *event.GateKey == "review" {
			projected.ReviewOutcome = textPointer("escalated")
		}
		if projected.GateEnteredAt != nil && now.After(*projected.GateEnteredAt) {
			projected.GateWaitSeconds = int64(now.Sub(*projected.GateEnteredAt) / time.Second)
		}
		return projected
	}

	if event.JobStatus == "queued" {
		if event.AttemptNumber == 0 && event.JobType == "agentops.triage" {
			projected.KanbanLane = "ready"
			projected.Phase = "intake"
			projected.State = "pending"
			projected.Step = "ready Issue queued for intake"
			projected.Summary = textPointer("Durable triage work is waiting for its first lease")
			projected.NextGate = textPointer("triage claim")
			projected.HumanAction = nil
			return projected
		}
		if event.AttemptNumber == 0 {
			projected.KanbanLane = "intake-planning"
			projected.Phase = "intake"
			projected.State = "waiting"
			projected.Step = "development intake queued"
			projected.Summary = textPointer("Promoted work is waiting for its first runner lease")
			projected.NextGate = textPointer("runner claim")
			projected.HumanAction = nil
			return projected
		}
		projected.KanbanLane = "gate-wait"
		projected.State = "waiting"
		projected.Step = "runner recovery queued"
		projected.Summary = textPointer("The previous attempt ended; a bounded retry is scheduled")
		projected.NextGate = textPointer("next runner lease")
		projected.HumanAction = nil
		return projected
	}

	if event.JobStatus == "leased" &&
		(event.LeaseExpiresAt == nil || !event.LeaseExpiresAt.After(now)) {
		projected.KanbanLane = "gate-wait"
		projected.State = "waiting"
		projected.Step = "expired lease recovery pending"
		projected.Summary = textPointer("The claimed attempt has no live lease; reconciliation will reclaim it")
		projected.NextGate = textPointer("lease reconciliation")
		projected.HumanAction = nil
		return projected
	}
	if event.JobStatus == "leased" && event.EventKey == "job:leased" {
		projected.KanbanLane = "intake-planning"
		projected.Phase = "intake"
		projected.State = "running"
		projected.Step = "claimed work starting"
		projected.Summary = textPointer("The live lease bounds claim ownership while the worker starts")
		projected.NextGate = textPointer("first durable phase report")
		return projected
	}

	projected.KanbanLane = kanbanLaneFor(event)
	if projected.State == "waiting" || projected.State == "blocked" {
		entered := projected.OccurredAt
		projected.GateEnteredAt = &entered
		if now.After(entered) {
			projected.GateWaitSeconds = int64(now.Sub(entered) / time.Second)
		}
	}
	return projected
}

func kanbanLaneFor(event DevelopmentProgressEvent) string {
	if event.State == "failed" || event.Phase == "failed" {
		return "failed"
	}
	if event.Phase == "human-review" || event.State == "blocked" {
		return "human-escalated"
	}
	if event.State == "waiting" {
		return "gate-wait"
	}
	switch event.Phase {
	case "intake", "planning":
		return "intake-planning"
	case "design":
		return "design"
	case "generation", "validation", "pull-request":
		return "implementation"
	case "review":
		return "review"
	case "repair":
		return "repair"
	case "merge":
		return "merge-ready"
	case "completed":
		return "released"
	default:
		return "ready"
	}
}

func projectCurrentProgressEvents(
	events []DevelopmentProgressEvent,
	now time.Time,
) {
	seen := make(map[string]struct{})
	for index := range events {
		event := events[index]
		number := int64(0)
		if event.SubjectNumber != nil {
			number = *event.SubjectNumber
		}
		key := fmt.Sprintf("%s:%d", event.SubjectKind, number)
		if _, present := seen[key]; present {
			continue
		}
		seen[key] = struct{}{}
		events[index] = canonicalDevelopmentProgress(event, now)
	}
}

// DevelopmentProgress returns newest-first durable progress. An absent Issue
// filter lists recent work across the repository for operator orientation.
func (store *Store) DevelopmentProgress(
	ctx context.Context,
	repository string,
	issueNumber *int64,
	limit int,
) ([]DevelopmentProgressEvent, error) {
	repository = strings.ToLower(strings.TrimSpace(repository))
	if !safeRepositoryIdentity(repository) {
		return nil, fmt.Errorf("%w: invalid repository", ErrConflict)
	}
	if issueNumber != nil && *issueNumber < 1 {
		return nil, fmt.Errorf("%w: invalid issue number", ErrConflict)
	}
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("%w: invalid progress limit", ErrConflict)
	}
	rows, err := store.pool.Query(ctx, `
	WITH ranked_progress AS (
	  SELECT progress.id, progress.occurred_at,
	         row_number() OVER (
	           PARTITION BY progress.subject_kind, progress.subject_number
	           ORDER BY progress.occurred_at DESC, progress.id DESC
	         ) AS subject_history_position
	    FROM agentops_control.development_progress_events progress
	   WHERE progress.repository = $1
	     AND ($2::bigint IS NULL OR (
	       progress.subject_kind = 'issue'
	       AND (
	         progress.subject_number = $2
	         OR progress.parent_issue_number = $2
	       )
	     ))
	), selected_progress AS (
	  SELECT id
	    FROM ranked_progress
	   ORDER BY subject_history_position, occurred_at DESC, id DESC
	   LIMIT $3
	)
	`+developmentProgressSelect+`
	 JOIN selected_progress selected ON selected.id = progress.id
	 ORDER BY progress.occurred_at DESC, progress.id DESC
`, repository, issueNumber, limit)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	events := make([]DevelopmentProgressEvent, 0)
	for rows.Next() {
		event, err := scanDevelopmentProgress(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	synthetic, err := developmentJobsWithoutProgress(
		ctx,
		store.pool,
		` AND registration.repository = $1
		  AND ($2::bigint IS NULL OR COALESCE(
		    release.issue_number,
		    CASE WHEN pg_input_is_valid(job.payload->'event'->>'number', 'bigint')
		      THEN (job.payload->'event'->>'number')::bigint END,
		    CASE WHEN pg_input_is_valid(job.payload->'issue'->>'number', 'bigint')
		      THEN (job.payload->'issue'->>'number')::bigint END
		  ) = $2)
		  ORDER BY job.created_at DESC, job.id DESC LIMIT $3`,
		repository,
		issueNumber,
		limit,
	)
	if err != nil {
		return nil, err
	}
	events = append(events, synthetic...)
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].OccurredAt.After(events[j].OccurredAt)
		}
		return events[i].ID > events[j].ID
	})
	projectCurrentProgressEvents(events, time.Now().UTC())
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (store *Store) LifecycleMode(ctx context.Context) (lifecycle.Mode, error) {
	var mode lifecycle.Mode
	if err := store.pool.QueryRow(ctx, `
		SELECT mode
		  FROM agentops_control.lifecycle_state
		 WHERE singleton
	`).Scan(&mode); err != nil {
		return "", unavailable(err)
	}
	switch mode {
	case lifecycle.ModeOff, lifecycle.ModeMonitorOnly, lifecycle.ModeActive, lifecycle.ModeDraining:
		return mode, nil
	default:
		return "", unavailable(fmt.Errorf("unsupported lifecycle mode %q", mode))
	}
}

func (store *Store) Projections(
	ctx context.Context,
	_ time.Duration,
) ([]RegistrationProjection, error) {
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	var databaseNow time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return nil, unavailable(err)
	}
	var lifecycleMode lifecycle.Mode
	if err := transaction.QueryRow(ctx, `
		SELECT mode
		  FROM agentops_control.lifecycle_state
		 WHERE singleton
	`).Scan(&lifecycleMode); err != nil {
		return nil, unavailable(err)
	}
	switch lifecycleMode {
	case lifecycle.ModeOff, lifecycle.ModeMonitorOnly, lifecycle.ModeActive, lifecycle.ModeDraining:
	default:
		return nil, unavailable(fmt.Errorf("unsupported lifecycle mode %q", lifecycleMode))
	}
	registrationRows, err := transaction.Query(ctx, registrationSelect+` ORDER BY repository`)
	if err != nil {
		return nil, unavailable(err)
	}
	var registrations []Registration
	for registrationRows.Next() {
		registration, err := scanRegistration(registrationRows)
		if err != nil {
			registrationRows.Close()
			return nil, unavailable(err)
		}
		registrations = append(registrations, registration)
	}
	registrationRows.Close()
	if err := registrationRows.Err(); err != nil {
		return nil, unavailable(err)
	}
	actualRows, err := transaction.Query(ctx,
		`SELECT registration_id, component, registration_version, state,
		        supervisor_id, observed_at, last_healthy_at, last_error
		   FROM agentops_control.monitor_actual_states`,
	)
	if err != nil {
		return nil, unavailable(err)
	}
	actual := make(map[string]map[string]ActualState)
	for actualRows.Next() {
		var registrationID string
		var state ActualState
		if err := actualRows.Scan(
			&registrationID,
			&state.Component,
			&state.RegistrationVersion,
			&state.State,
			&state.SupervisorID,
			&state.ObservedAt,
			&state.LastHealthyAt,
			&state.LastError,
		); err != nil {
			actualRows.Close()
			return nil, unavailable(err)
		}
		if actual[registrationID] == nil {
			actual[registrationID] = make(map[string]ActualState)
		}
		actual[registrationID][state.Component] = state
	}
	actualRows.Close()
	if err := actualRows.Err(); err != nil {
		return nil, unavailable(err)
	}
	projections := make([]RegistrationProjection, 0, len(registrations))
	for _, registration := range registrations {
		cursorObservedAt := map[string]*time.Time{"issue": nil, "pull_request": nil}
		projection := RegistrationProjection{
			Registration:           registration,
			Mode:                   lifecycleMode,
			Components:             make(map[string]ComponentProjection),
			CursorObservedAt:       cursorObservedAt,
			LastPoll:               cursorObservedAt,
			RecentDeliveryFailures: make([]DeliveryFailureProjection, 0),
			DevelopmentProgress:    make([]DevelopmentIssueProgress, 0),
		}
		for _, component := range []string{
			ComponentIssueMonitor,
			ComponentPRMonitor,
			ComponentForwarder,
		} {
			if !store.ManagesComponent(component) {
				continue
			}
			observed, present := actual[registration.ID][component]
			componentProjection := ComponentProjection{
				Desired:       registration.Desired(component),
				Actual:        "unknown",
				Freshness:     "unknown",
				RecoveryState: "unknown",
			}
			if present {
				componentProjection.Actual = observed.State
				componentProjection.ObservedAt = &observed.ObservedAt
				componentProjection.LastGoodAt = observed.LastHealthyAt
				componentProjection.LastError = observed.LastError
				stale :=
					observed.RegistrationVersion != registration.Version ||
						databaseNow.Sub(observed.ObservedAt) >
							componentFreshnessBudget(component)
				if stale {
					componentProjection.Freshness = "stale"
					reason := "freshness_budget_exceeded"
					if observed.RegistrationVersion != registration.Version {
						reason = "registration_version_mismatch"
					}
					componentProjection.StaleReason = &reason
					componentProjection.RecoveryState = "blocked"
				} else {
					componentProjection.Freshness = "fresh"
					if observed.State == "failed" {
						componentProjection.RecoveryState = "scheduled"
					} else {
						componentProjection.RecoveryState = "none"
					}
				}
			}
			projection.Components[component] = componentProjection
		}
		for _, kind := range []string{"issue", "pull_request"} {
			var observed time.Time
			err := transaction.QueryRow(ctx,
				`SELECT observed_at
				   FROM agentops_control.monitor_cursors
				  WHERE registration_id = $1 AND monitor_kind = $2`,
				registration.ID,
				kind,
			).Scan(&observed)
			if err == nil {
				projection.CursorObservedAt[kind] = &observed
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, unavailable(err)
			}
		}
		var lastDelivery *time.Time
		err := transaction.QueryRow(ctx,
			`SELECT max(received_at)
			   FROM agentops_control.webhook_deliveries
			  WHERE registration_id = $1`,
			registration.ID,
		).Scan(&lastDelivery)
		if err == nil {
			projection.LastDelivery = lastDelivery
		} else {
			return nil, unavailable(err)
		}
		var activeJobID *string
		var activeJobStatus *string
		var activeJobRegistrationVersion *int64
		var activeJobUpdatedAt *time.Time
		if err := transaction.QueryRow(ctx,
			`SELECT
				   count(*) FILTER (WHERE job.status = 'queued'),
				   (array_agg(
				      job.id::text
				      ORDER BY COALESCE(lease.heartbeat_at, job.updated_at) DESC, job.id DESC
				    ) FILTER (WHERE job.status = 'leased'))[1],
				   (array_agg(
				      job.status
				      ORDER BY COALESCE(lease.heartbeat_at, job.updated_at) DESC, job.id DESC
				    ) FILTER (WHERE job.status IN ('leased', 'queued')))[1],
				   (array_agg(
				      job.registration_version
				      ORDER BY COALESCE(lease.heartbeat_at, job.updated_at) DESC, job.id DESC
				    ) FILTER (WHERE job.status IN ('leased', 'queued')))[1],
				   (array_agg(
				      COALESCE(lease.heartbeat_at, job.updated_at)
				      ORDER BY COALESCE(lease.heartbeat_at, job.updated_at) DESC, job.id DESC
				    ) FILTER (WHERE job.status IN ('leased', 'queued')))[1]
				 FROM agentops_control.jobs AS job
				 LEFT JOIN agentops_control.job_leases AS lease
				   ON lease.job_id = job.id AND lease.status = 'active'
				WHERE job.registration_id = $1`,
			registration.ID,
		).Scan(
			&projection.QueueDepth,
			&activeJobID,
			&activeJobStatus,
			&activeJobRegistrationVersion,
			&activeJobUpdatedAt,
		); err != nil {
			return nil, unavailable(err)
		}
		projection.ActiveJobID = activeJobID
		projection.ActiveJobState = activeJobStatus
		projection.ActiveJobRegistrationVersion = activeJobRegistrationVersion
		var jobFailure JobFailureProjection
		err = transaction.QueryRow(ctx,
			`SELECT id, registration_version, job_type, status, last_error, updated_at
			   FROM agentops_control.jobs
			  WHERE registration_id = $1
			    AND status IN ('failed', 'cancelled', 'rejected')
			  ORDER BY updated_at DESC, id DESC
			  LIMIT 1`,
			registration.ID,
		).Scan(
			&jobFailure.ID,
			&jobFailure.RegistrationVersion,
			&jobFailure.JobType,
			&jobFailure.Status,
			&jobFailure.LastError,
			&jobFailure.UpdatedAt,
		)
		if err == nil {
			projection.LastJobFailure = &jobFailure
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, unavailable(err)
		}
		var latestJobStatus *string
		var latestJobVersion *int64
		var latestJobUpdatedAt *time.Time
		var latestJobError *string
		err = transaction.QueryRow(ctx,
			`SELECT status, registration_version, updated_at, last_error
			   FROM agentops_control.jobs
			  WHERE registration_id = $1
			  ORDER BY updated_at DESC, id DESC
			  LIMIT 1`,
			registration.ID,
		).Scan(
			&latestJobStatus,
			&latestJobVersion,
			&latestJobUpdatedAt,
			&latestJobError,
		)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, unavailable(err)
		}
		execution := ComponentProjection{
			Desired:       registration.Desired(ComponentExecution),
			Actual:        "unknown",
			Freshness:     "unknown",
			RecoveryState: "unknown",
		}
		queue := ComponentProjection{
			Desired:       registration.Desired(ComponentQueue),
			Actual:        "idle",
			ObservedAt:    &databaseNow,
			Freshness:     "fresh",
			RecoveryState: "none",
		}
		if activeJobStatus == nil &&
			latestJobVersion != nil &&
			*latestJobVersion != registration.Version {
			reason := "registration_version_mismatch"
			execution.Actual = "stale"
			execution.ObservedAt = latestJobUpdatedAt
			execution.Freshness = "stale"
			execution.StaleReason = &reason
			execution.LastError = &reason
			execution.RecoveryState = "blocked"
			queue.Actual = "blocked_by_mode"
			queue.ObservedAt = latestJobUpdatedAt
			queue.Freshness = "stale"
			queue.StaleReason = &reason
			queue.LastError = &reason
			queue.RecoveryState = "blocked"
		} else if activeJobStatus != nil {
			execution.ObservedAt = activeJobUpdatedAt
			if activeJobRegistrationVersion == nil ||
				*activeJobRegistrationVersion != registration.Version {
				reason := "registration_version_mismatch"
				execution.Actual = "stale"
				execution.Freshness = "stale"
				execution.StaleReason = &reason
				execution.LastError = &reason
				execution.RecoveryState = "blocked"
				queue.StaleReason = &reason
				queue.LastError = &reason
				queue.Freshness = "stale"
				queue.RecoveryState = "blocked"
			} else if activeJobUpdatedAt != nil &&
				databaseNow.Sub(*activeJobUpdatedAt) > 30*time.Second {
				reason := "freshness_budget_exceeded"
				execution.Freshness = "stale"
				execution.StaleReason = &reason
				execution.RecoveryState = "blocked"
				queue.Freshness = "stale"
				queue.StaleReason = &reason
				queue.RecoveryState = "blocked"
			} else {
				execution.Freshness = "fresh"
			}
			if *activeJobStatus == "leased" {
				if execution.Actual != "stale" {
					execution.Actual = "running"
					if execution.RecoveryState != "blocked" {
						execution.RecoveryState = "in_progress"
					}
					execution.LastGoodAt = activeJobUpdatedAt
				}
				queue.Actual = "leased"
				if queue.RecoveryState != "blocked" {
					queue.RecoveryState = "in_progress"
				}
			} else {
				if execution.Actual != "stale" {
					execution.Actual = "waiting"
					if execution.RecoveryState != "blocked" {
						execution.RecoveryState = "scheduled"
					}
				}
				queue.Actual = "queued"
				if queue.RecoveryState != "blocked" {
					queue.RecoveryState = "scheduled"
				}
			}
			queue.ObservedAt = activeJobUpdatedAt
			queue.LastGoodAt = activeJobUpdatedAt
			if activeJobUpdatedAt != nil &&
				databaseNow.Sub(*activeJobUpdatedAt) > 15*time.Second {
				reason := "freshness_budget_exceeded"
				queue.Freshness = "stale"
				queue.StaleReason = &reason
			}
		} else if latestJobStatus != nil &&
			latestJobVersion != nil &&
			*latestJobVersion == registration.Version {
			execution.ObservedAt = latestJobUpdatedAt
			if latestJobUpdatedAt != nil &&
				databaseNow.Sub(*latestJobUpdatedAt) > 30*time.Second {
				reason := "freshness_budget_exceeded"
				execution.Freshness = "stale"
				execution.StaleReason = &reason
			} else {
				execution.Freshness = "fresh"
			}
			if *latestJobStatus == "succeeded" {
				execution.Actual = "idle"
				execution.LastGoodAt = latestJobUpdatedAt
				execution.RecoveryState = "recovered"
			} else if *latestJobStatus == "failed" ||
				*latestJobStatus == "cancelled" ||
				*latestJobStatus == "rejected" {
				execution.Actual = "failed"
				execution.LastError = latestJobError
				execution.RecoveryState = "scheduled"
				queue.Actual = "failed"
				queue.ObservedAt = latestJobUpdatedAt
				queue.LastError = latestJobError
				queue.RecoveryState = "scheduled"
				if latestJobUpdatedAt != nil &&
					databaseNow.Sub(*latestJobUpdatedAt) > 15*time.Second {
					reason := "freshness_budget_exceeded"
					queue.Freshness = "stale"
					queue.StaleReason = &reason
				}
			}
		}
		projection.Components[ComponentExecution] = execution
		projection.Components[ComponentQueue] = queue
		failureRows, err := transaction.Query(ctx,
			`SELECT id, delivery_key, event, action, status, ignored_reason,
			        last_error, route_attempts, registration_version, updated_at
			   FROM agentops_control.webhook_deliveries
			  WHERE registration_id = $1
			    AND status IN ('failed', 'ignored')
			  ORDER BY updated_at DESC, id DESC
			  LIMIT 20`,
			registration.ID,
		)
		if err != nil {
			return nil, unavailable(err)
		}
		for failureRows.Next() {
			var failure DeliveryFailureProjection
			if err := failureRows.Scan(
				&failure.ID,
				&failure.DeliveryKey,
				&failure.Event,
				&failure.Action,
				&failure.Status,
				&failure.IgnoredReason,
				&failure.LastError,
				&failure.RouteAttempts,
				&failure.RegistrationVersion,
				&failure.UpdatedAt,
			); err != nil {
				failureRows.Close()
				return nil, unavailable(err)
			}
			projection.RecentDeliveryFailures = append(
				projection.RecentDeliveryFailures,
				failure,
			)
		}
		failureRows.Close()
		if err := failureRows.Err(); err != nil {
			return nil, unavailable(err)
		}
		// One registration can accumulate unboundedly many Issues. The card is
		// an orientation surface, not an archive: keep the most recently active
		// Issues only and let `agentopsctl progress` serve the full history.
		// One extra Issue is requested purely to report truncation truthfully.
		progressRows, err := transaction.Query(ctx, `
			WITH recent_issues AS (
			  SELECT progress.subject_number,
			         max(progress.occurred_at) AS last_occurred_at
			    FROM agentops_control.development_progress_events progress
			   WHERE progress.registration_id = $1
			     AND progress.subject_kind = 'issue'
			     AND progress.subject_number IS NOT NULL
			   GROUP BY progress.subject_number
			   ORDER BY last_occurred_at DESC, progress.subject_number DESC
			   LIMIT $2
			), ranked_progress AS (
			  SELECT progress.id,
			         row_number() OVER (
			           PARTITION BY progress.subject_number
			           ORDER BY progress.occurred_at DESC, progress.id DESC
			         ) AS history_position
			    FROM agentops_control.development_progress_events progress
			    JOIN recent_issues
			      ON recent_issues.subject_number = progress.subject_number
			   WHERE progress.registration_id = $1
			     AND progress.subject_kind = 'issue'
			), selected_progress AS (
			  SELECT id FROM ranked_progress WHERE history_position <= $3
			)
			`+developmentProgressSelect+`
			 JOIN selected_progress selected ON selected.id = progress.id
			 ORDER BY progress.subject_number, progress.occurred_at DESC, progress.id DESC`,
			registration.ID,
			developmentProgressIssueLimit+1,
			developmentProgressHistoryLimit,
		)
		if err != nil {
			return nil, unavailable(err)
		}
		issueIndexes := make(map[int64]int)
		for progressRows.Next() {
			event, err := scanDevelopmentProgress(progressRows)
			if err != nil {
				progressRows.Close()
				return nil, unavailable(err)
			}
			if event.SubjectNumber == nil {
				progressRows.Close()
				return nil, unavailable(errors.New("issue progress has no issue number"))
			}
			issueNumber := *event.SubjectNumber
			index, present := issueIndexes[issueNumber]
			if !present {
				index = len(projection.DevelopmentProgress)
				issueIndexes[issueNumber] = index
				projection.DevelopmentProgress = append(
					projection.DevelopmentProgress,
					DevelopmentIssueProgress{
						Repository:   event.Repository,
						IssueNumber:  issueNumber,
						Current:      event,
						History:      make([]DevelopmentProgressEvent, 0, developmentProgressHistoryLimit),
						StartedAt:    event.OccurredAt,
						LastActivity: developmentProgressActivity(event),
					},
				)
			}
			issueProgress := &projection.DevelopmentProgress[index]
			issueProgress.History = append(issueProgress.History, event)
			if event.OccurredAt.Before(issueProgress.StartedAt) {
				issueProgress.StartedAt = event.OccurredAt
			}
			if activity := developmentProgressActivity(event); activity.After(issueProgress.LastActivity) {
				issueProgress.LastActivity = activity
			}
		}
		progressRows.Close()
		if err := progressRows.Err(); err != nil {
			return nil, unavailable(err)
		}
		syntheticProgress, err := developmentJobsWithoutProgress(
			ctx,
			transaction,
			` AND job.registration_id = $1
			  ORDER BY job.created_at DESC, job.id DESC LIMIT $2`,
			registration.ID,
			developmentProgressIssueLimit+1,
		)
		if err != nil {
			return nil, err
		}
		for _, event := range syntheticProgress {
			if event.SubjectNumber == nil {
				return nil, unavailable(errors.New("queued issue work has no issue number"))
			}
			issueNumber := *event.SubjectNumber
			index, present := issueIndexes[issueNumber]
			if !present {
				index = len(projection.DevelopmentProgress)
				issueIndexes[issueNumber] = index
				projection.DevelopmentProgress = append(
					projection.DevelopmentProgress,
					DevelopmentIssueProgress{
						Repository:   event.Repository,
						IssueNumber:  issueNumber,
						Current:      event,
						History:      []DevelopmentProgressEvent{event},
						StartedAt:    event.OccurredAt,
						LastActivity: developmentProgressActivity(event),
					},
				)
				continue
			}
			issueProgress := &projection.DevelopmentProgress[index]
			issueProgress.History = append(issueProgress.History, event)
			if event.OccurredAt.After(issueProgress.Current.OccurredAt) {
				issueProgress.Current = event
			}
			if event.OccurredAt.Before(issueProgress.StartedAt) {
				issueProgress.StartedAt = event.OccurredAt
			}
			if activity := developmentProgressActivity(event); activity.After(issueProgress.LastActivity) {
				issueProgress.LastActivity = activity
			}
		}
		for index := range projection.DevelopmentProgress {
			projection.DevelopmentProgress[index].Current =
				canonicalDevelopmentProgress(
					projection.DevelopmentProgress[index].Current,
					databaseNow,
				)
		}
		sort.SliceStable(projection.DevelopmentProgress, func(i, j int) bool {
			left := projection.DevelopmentProgress[i]
			right := projection.DevelopmentProgress[j]
			if !left.LastActivity.Equal(right.LastActivity) {
				return left.LastActivity.After(right.LastActivity)
			}
			return left.IssueNumber < right.IssueNumber
		})
		if len(projection.DevelopmentProgress) > developmentProgressIssueLimit {
			projection.DevelopmentProgress =
				projection.DevelopmentProgress[:developmentProgressIssueLimit]
			projection.DevelopmentProgressTruncated = true
		}
		projections = append(projections, projection)
	}
	sort.Slice(projections, func(i, j int) bool {
		leftAnomaly := projectionHasAnomaly(projections[i])
		rightAnomaly := projectionHasAnomaly(projections[j])
		if leftAnomaly != rightAnomaly {
			return leftAnomaly
		}
		return projections[i].Registration.Repository < projections[j].Registration.Repository
	})
	if err := transaction.Commit(ctx); err != nil {
		return nil, unavailable(err)
	}
	return projections, nil
}

func (store *Store) Listen(ctx context.Context, channel string, wake func()) error {
	if channel != "agentops_registration_wake" &&
		channel != "agentops_webhook_wake" &&
		channel != "agentops_job_wake" {
		return fmt.Errorf("invalid LISTEN channel")
	}
	connection, err := store.pool.Acquire(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, "LISTEN "+channel); err != nil {
		return unavailable(err)
	}
	for {
		if _, err := connection.Conn().WaitForNotification(ctx); err != nil {
			return unavailable(err)
		}
		wake()
	}
}

const registrationSelect = `SELECT id, repository, enabled, issue_monitor_enabled,
       pr_monitor_enabled, execution_enabled, configuration, version,
       created_at, updated_at
  FROM agentops_control.repository_registrations`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRegistration(row rowScanner) (Registration, error) {
	var registration Registration
	err := row.Scan(
		&registration.ID,
		&registration.Repository,
		&registration.Enabled,
		&registration.IssueMonitorEnabled,
		&registration.PRMonitorEnabled,
		&registration.ExecutionEnabled,
		&registration.Configuration,
		&registration.Version,
		&registration.CreatedAt,
		&registration.UpdatedAt,
	)
	return registration, err
}

func appendAuditTx(
	ctx context.Context,
	transaction pgx.Tx,
	actorID, eventType string,
	registrationID, jobID *string,
	details map[string]any,
) error {
	_, err := transaction.Exec(ctx,
		`INSERT INTO agentops_control.runtime_audit(
		   actor_type, actor_id, event_type, registration_id, job_id, details
		 ) VALUES ('control', $1, $2, $3, $4, $5)`,
		actorID,
		eventType,
		registrationID,
		jobID,
		details,
	)
	return unavailable(err)
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4],
		value[4:6],
		value[6:8],
		value[8:10],
		value[10:16],
	), nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func advisoryRequestKey(scope, idempotencyKey string) string {
	return fmt.Sprintf("%d:%s:%d:%s", len(scope), scope, len(idempotencyKey), idempotencyKey)
}

func unavailable(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrRepositoryBusy) ||
		errors.Is(err, ErrOperatingMode) ||
		errors.Is(err, ErrStaleRegistration) || errors.Is(err, ErrIdempotencyConflict) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStoreUnavailable, err)
}

func classifyPostgres(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			switch postgresError.ConstraintName {
			case "jobs_one_active_per_repository":
				return fmt.Errorf("%w: %s", ErrRepositoryBusy, postgresError.ConstraintName)
			case "jobs_registration_idempotency_key", "jobs_registration_source_key":
				return fmt.Errorf("%w: %s", ErrIdempotencyConflict, postgresError.ConstraintName)
			}
			return fmt.Errorf("%w: %s", ErrConflict, postgresError.ConstraintName)
		case "23514", "22P02", "23503":
			return fmt.Errorf("%w: %s", ErrConflict, postgresError.Message)
		case "55000":
			return fmt.Errorf("%w: %s", ErrOperatingMode, postgresError.Message)
		}
	}
	return unavailable(err)
}

func jsonEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func durableHeaders(headers map[string]string) map[string]string {
	allowed := map[string]bool{
		"content-type":                           true,
		"user-agent":                             true,
		"x-github-delivery":                      true,
		"x-github-event":                         true,
		"x-github-hook-id":                       true,
		"x-github-hook-installation-target-id":   true,
		"x-github-hook-installation-target-type": true,
	}
	result := make(map[string]string)
	for key, value := range headers {
		name := strings.ToLower(key)
		if allowed[name] {
			result[name] = value
		}
	}
	return result
}

func supportedWebhookEvent(event string) bool {
	switch event {
	case "issues", "pull_request", "pull_request_review",
		"pull_request_review_comment", "check_run", "check_suite",
		"push", "issue_comment":
		return true
	default:
		return false
	}
}

func intervalLiteral(duration time.Duration) string {
	return fmt.Sprintf("%d milliseconds", duration.Milliseconds())
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func projectionHasAnomaly(projection RegistrationProjection) bool {
	for name, component := range projection.Components {
		if component.Freshness != "fresh" ||
			component.Actual == "failed" ||
			component.Actual == "disconnected" ||
			component.Actual == "unknown" ||
			componentProjectionDiverges(name, component) {
			return true
		}
	}
	return false
}

func componentProjectionDiverges(name string, component ComponentProjection) bool {
	var allowed []string
	if component.Desired {
		switch name {
		case ComponentIssueMonitor, ComponentPRMonitor, ComponentForwarder:
			allowed = []string{"starting", "running"}
		case ComponentExecution:
			allowed = []string{"running", "waiting", "idle", "paused_by_mode"}
		case ComponentQueue:
			allowed = []string{"idle", "queued", "leased", "blocked_by_mode"}
		}
	} else {
		switch name {
		case ComponentIssueMonitor, ComponentPRMonitor, ComponentForwarder:
			allowed = []string{"stopped"}
		case ComponentExecution:
			allowed = []string{"stopped", "idle"}
		case ComponentQueue:
			allowed = []string{"idle"}
		}
	}
	for _, state := range allowed {
		if component.Actual == state {
			return false
		}
	}
	return true
}

func componentFreshnessBudget(component string) time.Duration {
	switch component {
	case ComponentIssueMonitor, ComponentPRMonitor:
		return 300 * time.Second
	case ComponentForwarder:
		return 60 * time.Second
	case ComponentExecution:
		return 30 * time.Second
	case ComponentQueue:
		return 15 * time.Second
	default:
		return 15 * time.Second
	}
}
