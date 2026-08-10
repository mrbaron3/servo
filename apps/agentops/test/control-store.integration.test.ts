import fs from 'node:fs';
import { createHash, randomUUID } from 'node:crypto';
import os from 'node:os';
import path from 'node:path';
import { Pool, type PoolClient } from 'pg';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import {
  CONTROL_SCHEMA_VERSION,
  CONTROL_MIGRATION_LOCK_KEY,
  ControlSchemaError,
  IdempotencyConflictError,
  JobEnvelopeContract,
  LeaseRejectedError,
  OperatingModeError,
  PostgresControlStore,
  RepositoryBusyError,
  releaseSourceIssueSnapshotDigest,
  assertControlSchema,
  loadControlMigrations,
  migrateControlSchema,
} from '../src/control-store/index.js';
import type { AgentOpsRunnerAdapter } from '../src/runner/adapter.js';
import { IsolatedRunnerService } from '../src/runner/service.js';
import {
  RunnerWorkspaceManager,
  type WorkspaceCommandRunner,
} from '../src/runner/workspace.js';

// Each test in this file rebuilds the control schema against a real PostgreSQL
// before doing its own work, so the default 5s per-test budget leaves no room
// for a loaded CI runner: one test was observed timing out at 5009ms while its
// neighbours in the same run took 4-6x their local duration. The bound sits
// above the lock timeout in reset() so a genuine lock wait fails naming itself
// rather than as an expired test.
vi.setConfig({ testTimeout: 20_000 });

function frozenSourceIssue(repository: string, number: number, at: string) {
  const source = {
    repository,
    number,
    title: `Issue ${number}`,
    body: 'Frozen acceptance requirements.',
    url: `https://github.com/${repository}/issues/${number}`,
    labels: ['ready'],
    comments: [],
    state: 'open' as const,
    sourceUpdatedAt: at,
    capturedAt: at,
  };
  return { ...source, digest: releaseSourceIssueSnapshotDigest(source) };
}

const databaseUrl = process.env.AGENTOPS_TEST_DATABASE_URL;
const integration = databaseUrl ? describe.sequential : describe.skip;

integration('PostgreSQL control store', () => {
  let pool: Pool;

  beforeAll(() => {
    pool = new Pool({ connectionString: databaseUrl, max: 16 });
  });

  afterAll(async () => {
    await pool?.end();
  });

  // Every test rebuilds the schema, so this DROP is the one statement in the
  // suite that never means to wait: if it cannot take its locks, a previous
  // test left a connection holding them. An unbounded wait surfaces as an
  // opaque "test timed out", which says nothing about the cause — a bounded one
  // fails naming the lock. The bound is generous so ordinary CI slowness never
  // reaches it. The three statements travel as one simple query, which
  // PostgreSQL wraps in an implicit transaction, so the bound never outlives
  // this call on a pooled connection: the trailing reset restores it, and a
  // failing DROP rolls the SET back with everything else.
  async function reset(): Promise<void> {
    await pool.query(
      `SET lock_timeout = '10s';
       DROP SCHEMA IF EXISTS agentops_control CASCADE;
       SET lock_timeout = DEFAULT;`,
    );
  }

  async function migratedStore(): Promise<PostgresControlStore> {
    await reset();
    expect(await migrateControlSchema(pool)).toBe(CONTROL_SCHEMA_VERSION);
    await pool.query(
      `UPDATE agentops_control.lifecycle_state
          SET mode = 'ACTIVE', generation = generation + 1,
              updated_at = clock_timestamp()
        WHERE singleton`,
    );
    await assertControlSchema(pool);
    return new PostgresControlStore(pool);
  }

  async function registration(store: PostgresControlStore, suffix = '') {
    return store.createRegistration({
      repository: `mrbaron3/control-store${suffix}`,
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
  }

  async function enqueue(
    store: PostgresControlStore,
    registrationId: string,
    registrationVersion: number,
    key: string,
    sourceKind: 'webhook' | 'poll' = 'webhook',
  ) {
    return store.enqueueJob({
      registrationId,
      registrationVersion,
      source: { kind: sourceKind, key: `${sourceKind}:${key}` },
      idempotencyKey: key,
      jobType: 'github_issue_turn',
      payload: { issueNumber: 12 },
    });
  }

  async function enqueueRunner(
    store: PostgresControlStore,
    registrationId: string,
    registrationVersion: number,
    key: string,
  ) {
    return store.enqueueJob({
      registrationId,
      registrationVersion,
      source: { kind: 'manual', key },
      idempotencyKey: key,
      jobType: 'agentops.runner',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'mrbaron3', name: `control-store${key}` },
        event: { kind: 'issue', number: 14, action: 'labeled' },
        target: { baseRef: 'refs/heads/main' },
        execution: {
          mode: 'development_turn',
          requiredChecks: ['test'],
          mergeMethod: 'squash',
          readyLabel: 'ready',
          claimedLabel: 'agent-claimed',
        },
        artifacts: [],
      },
    });
  }

  async function insertMonitorBrokerRequest(input: {
    registrationId: string;
    registrationVersion: number;
    repository?: string;
    kind?: 'issue' | 'pull_request';
    cursor?: { updatedAfter: string };
  }): Promise<string> {
    const id = randomUUID();
    const cursor = input.cursor ?? { updatedAfter: '' };
    const digest = createHash('sha256')
      .update(JSON.stringify(cursor))
      .digest('hex');
    await pool.query(
      `INSERT INTO agentops_control.monitor_broker_requests(
         id, registration_id, registration_version, repository,
         monitor_kind, cursor, cursor_sha256
       ) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
      [
        id,
        input.registrationId,
        input.registrationVersion,
        input.repository ?? 'mrbaron3/workflow',
        input.kind ?? 'issue',
        cursor,
        digest,
      ],
    );
    return id;
  }

  it('keeps historical Registration grader aliases decode-only', async () => {
    const store = await migratedStore();
    const created = await registration(store, '-historical-policy');
    const historicalConfiguration = {
      releaseEvidence: {
        authority: 'human-ready-allowed',
        requiredGateSignals: [
          { source: 'repository-grader', name: 'contracts' },
          { source: 'repository-grader', name: 'contracts' },
        ],
        requiredReviewPerspectives: ['security', 'security'],
        minimumHeadEpochs: 1,
      },
    };
    await expect(store.createRegistration({
      repository: 'mrbaron3/control-store-historical-writer',
      configuration: historicalConfiguration,
    })).rejects.toThrow();

    await pool.query(
      `UPDATE agentops_control.repository_registrations
          SET configuration = $2
        WHERE id = $1`,
      [created.id, historicalConfiguration],
    );
    await expect(store.getRegistration(created.id)).resolves.toMatchObject({
      id: created.id,
      configuration: historicalConfiguration,
    });
    await expect(store.listRegistrations()).resolves.toContainEqual(
      expect.objectContaining({
        id: created.id,
        configuration: historicalConfiguration,
      }),
    );
    await enqueueRunner(store, created.id, created.version, '-historical-policy');
    const lease = await store.acquireLease({
      workerId: 'runner-historical-policy',
      durationMs: 10_000,
    });
    expect(lease).not.toBeNull();
    await expect(store.assertExecutionGuard({
      token: lease!.token,
      workerId: 'runner-historical-policy',
      boundary: 'provider',
    })).resolves.toMatchObject({
      ok: true,
      registration: { configuration: historicalConfiguration },
    });
  });

  it('atomically fences racing enqueue and lease acquisition when drain commits', async () => {
    const store = await migratedStore();
    const registered = await registration(store, '-mode-fence');
    await enqueueRunner(store, registered.id, registered.version, 'before-drain');
    let blocker: PoolClient | undefined;
    let blockerTransactionOpen = false;
    const pending: Promise<unknown>[] = [];
    try {
      blocker = await pool.connect();
      await blocker.query('BEGIN');
      blockerTransactionOpen = true;
      await blocker.query(
        `UPDATE agentops_control.lifecycle_state
            SET mode = 'DRAINING', generation = generation + 1,
                updated_at = clock_timestamp()
          WHERE singleton`,
      );
      let leaseSettled = false;
      let enqueueSettled = false;
      const lease = store.acquireLease({
        workerId: 'mode-fenced-runner',
        durationMs: 30_000,
        jobType: 'agentops.runner',
      }).finally(() => {
        leaseSettled = true;
      });
      // Mark an unexpected rejection handled immediately; the assertion below
      // still observes the original promise after the blocker commits.
      void lease.catch(() => undefined);
      pending.push(lease);
      const enqueue = enqueueRunner(
        store,
        registered.id,
        registered.version,
        'after-drain',
      ).then(
        () => null,
        (error: unknown) => error,
      ).finally(() => {
        enqueueSettled = true;
      });
      pending.push(enqueue);
      await new Promise((resolve) => setTimeout(resolve, 100));
      expect({ leaseSettled, enqueueSettled }).toEqual({
        leaseSettled: false,
        enqueueSettled: false,
      });
      await blocker.query('COMMIT');
      blockerTransactionOpen = false;
      await expect(lease).resolves.toBeNull();
      await expect(enqueue).resolves.toBeInstanceOf(OperatingModeError);
    } finally {
      let cleanupError: unknown;
      if (blocker) {
        if (blockerTransactionOpen) {
          try {
            await blocker.query('ROLLBACK');
          } catch (error) {
            cleanupError = error;
          }
        }
        blocker.release(cleanupError instanceof Error ? cleanupError : undefined);
      }
      await Promise.allSettled(pending);
      if (cleanupError) throw cleanupError;
    }
  }, 15_000);

  it('upgrades, verifies after restart, and rejects partial/unknown schema', async () => {
    await reset();
    await expect(assertControlSchema(pool)).rejects.toBeInstanceOf(ControlSchemaError);
    await expect(migrateControlSchema(pool)).resolves.toBe(CONTROL_SCHEMA_VERSION);
    await expect(assertControlSchema(pool)).resolves.toBeUndefined();

    const restartedPool = new Pool({ connectionString: databaseUrl });
    await expect(assertControlSchema(restartedPool)).resolves.toBeUndefined();
    await restartedPool.end();

    await pool.query(
      `UPDATE agentops_control.schema_migrations
          SET version = 99 WHERE version = $1`,
      [CONTROL_SCHEMA_VERSION],
    );
    await expect(assertControlSchema(pool)).rejects.toThrow(/unknown|non-contiguous/);

    await reset();
    await pool.query('CREATE SCHEMA agentops_control');
    await pool.query('CREATE TABLE agentops_control.partial_table(id integer)');
    await expect(migrateControlSchema(pool)).rejects.toThrow(/partial control schema/);
  });

  it('migrates Stage 3 legacy rows without inventing lost evidence', async () => {
    await reset();
    const migrations = loadControlMigrations();
    for (const migration of migrations.slice(0, 22)) {
      await pool.query(migration.sql);
    }
    await pool.query(
      `UPDATE agentops_control.lifecycle_state SET mode = 'ACTIVE'
        WHERE singleton`,
    );
    const registrationRow = await pool.query<{ id: string; version: string }>(
      `SELECT id, version FROM agentops_control.repository_registrations
        WHERE repository = 'mrbaron3/servo'`,
    );
    const registered = registrationRow.rows[0]!;
    const jobId = '30000000-0000-4000-8000-000000000001';
    const attemptId = '30000000-0000-4000-8000-000000000002';
    const releaseId = '30000000-0000-4000-8000-000000000003';
    const lineageId = '30000000-0000-4000-8000-000000000004';
    const buildId = '30000000-0000-4000-8000-000000000005';
    const head = 'a'.repeat(40);
    await pool.query(
      `INSERT INTO agentops_control.jobs(
         id, registration_id, registration_version, source_kind, source_key,
         idempotency_key, job_type, payload, status
       ) VALUES ($1, $2, $3, 'manual', 'stage3-upgrade', 'stage3-upgrade',
         'agentops.runner', '{}', 'succeeded')`,
      [jobId, registered.id, Number(registered.version)],
    );
    await pool.query(
      `INSERT INTO agentops_control.job_attempts(
         id, job_id, attempt_number, worker_id, status
       ) VALUES ($1, $2, 1, 'stage3-upgrade', 'succeeded')`,
      [attemptId, jobId],
    );
    await pool.query(
      `INSERT INTO agentops_control.releases(
         id, registration_id, release_key, repository, issue_number, policy
       ) VALUES ($1, $2, 'stage3-upgrade', 'mrbaron3/servo', 301,
         '{
           "authority":"human-ready-allowed",
           "requiredGateSignals":[{"source":"repository-grader","name":"build"}],
           "requiredReviewPerspectives":["security","codeQuality"],
           "minimumHeadEpochs":1
         }')`,
      [releaseId, registered.id],
    );
    await pool.query(
      `INSERT INTO agentops_control.development_review_rounds(
         registration_id, registration_version, job_id, attempt_id,
         repository, subject_kind, subject_number, round, head_sha, branch,
         outcome, started_at, completed_at
       ) VALUES ($1, $2, $3, $4, 'mrbaron3/servo', 'issue', 301, 1, $5,
         'agent/stage3-upgrade', 'request-changes', clock_timestamp(),
         clock_timestamp())`,
      [registered.id, Number(registered.version), jobId, attemptId, head],
    );
    await pool.query(
      `INSERT INTO agentops_control.development_lineage_nodes(
         id, registration_id, release_id, repository, issue_number, status,
         child_pull_request_number, child_head_sha
       ) VALUES ($1, $2, $3, 'mrbaron3/servo', 301, 'running', 77, $4)`,
      [lineageId, registered.id, releaseId, head],
    );
    await pool.query(
      `INSERT INTO agentops_control.released_builds(
         id, registration_id, issue_number, revision_id, head_sha,
         panel_approved, released_at
       ) VALUES ($1, $2, 301, 'legacy-revision', $3, true, clock_timestamp())`,
      [buildId, registered.id, head],
    );
    await pool.query(
      `INSERT INTO agentops_control.build_defects(
         id, build_id, defect_key, observation_stage, severity, summary,
         discovered_at
       ) VALUES ('30000000-0000-4000-8000-000000000006', $1,
         'legacy-high', 'release_escape', 'high', 'historical escape',
         clock_timestamp())`,
      [buildId],
    );
    const common = {
      releaseId,
      repository: 'mrbaron3/servo',
      issueNumber: 301,
      producer: {},
      causes: [],
      recordedAt: new Date(Date.now() + 1_000).toISOString(),
    };
    const legacyReceipts = [
      {
        ...common,
        receiptId: '30000000-0000-4000-8000-000000000010',
        receiptKey: 'runtime:legacy',
        kind: 'runtime-provenance',
        consumer: { repository: 'mrbaron3/consumer', revision: head },
        environment: { kind: 'container', reference: 'legacy', digest: `sha256:${'b'.repeat(64)}` },
        invocations: [{
          invocationId: 'invocation:legacy', role: 'reviewer', provider: 'codex',
          model: { kind: 'explicit', name: 'legacy-model' }, head,
        }],
      },
      {
        ...common,
        receiptId: '30000000-0000-4000-8000-000000000011',
        receiptKey: 'review:legacy',
        kind: 'review', head, headEpoch: 1, perspective: 'security',
        invocationId: 'invocation:legacy', verdict: 'findings', findings: [],
      },
      {
        ...common,
        receiptId: '30000000-0000-4000-8000-000000000012',
        receiptKey: 'merge:legacy',
        kind: 'merge', pullRequest: 77, expectedHead: head,
        observedPrHead: head, mergeSha: 'c'.repeat(40), actor: 'legacy',
        issueState: 'CLOSED', issueStateReason: 'COMPLETED',
        mergeReachableFromDefaultBranch: true,
        mergedAt: new Date(Date.now() + 2_000).toISOString(),
      },
    ];
    for (const receipt of legacyReceipts) {
      await pool.query(
        `INSERT INTO agentops_control.release_receipt_outbox(
           receipt_id, release_id, receipt_key, kind, repository, issue_number,
           head_sha, causes, payload, recorded_at
         ) VALUES ($1, $2, $3, $4, $5, $6, $7, '{}', $8::jsonb,
           ($8::jsonb->>'recordedAt')::timestamptz)`,
        [receipt.receiptId, releaseId, receipt.receiptKey, receipt.kind,
          receipt.repository, receipt.issueNumber,
          receipt.kind === 'runtime-provenance' ? null : head, receipt],
      );
    }
    for (const migration of migrations.slice(22)) {
      await pool.query(migration.sql);
    }

    await expect(pool.query(
      `SELECT outcome FROM agentops_control.development_review_rounds`,
    )).resolves.toMatchObject({ rows: [{ outcome: 'request_changes' }] });
    await expect(pool.query(
      `SELECT pull_request_number, head_sha
         FROM agentops_control.development_lineage_nodes`,
    )).resolves.toMatchObject({
      rows: [{ pull_request_number: '77', head_sha: head }],
    });
    const migratedReceipts = await pool.query<{ kind: string; payload: any }>(
      `SELECT kind, payload FROM agentops_control.release_receipt_outbox
        ORDER BY kind`,
    );
    expect(migratedReceipts.rows).toEqual(expect.arrayContaining([
      expect.objectContaining({
        kind: 'runtime-provenance',
        payload: expect.objectContaining({
          invocations: [expect.objectContaining({
            invocationKey: 'invocation:legacy',
            invocationRef: 'invocation:legacy',
          })],
        }),
      }),
      expect.objectContaining({
        kind: 'review',
        payload: expect.objectContaining({
          verdict: 'request_changes', hasFindings: false,
          invocationRef: 'invocation:legacy',
        }),
      }),
      expect.objectContaining({
        kind: 'merge',
        payload: expect.objectContaining({
          pullRequestNumber: 77, sourceIssueClosure: 'completed',
        }),
      }),
    ]));
    const retired = await pool.query<{ severity: string }>(
      `SELECT severity FROM agentops_control.retired_build_defects`,
    );
    expect(retired.rows).toEqual([{ severity: 'blocker' }]);
    const activeTables = await pool.query<{ active: boolean; retired: boolean }>(
      `SELECT to_regclass('agentops_control.released_builds') IS NOT NULL AS active,
              to_regclass('agentops_control.retired_released_builds') IS NOT NULL AS retired`,
    );
    expect(activeTables.rows).toEqual([{ active: false, retired: true }]);
  });

  it('upgrades the exact deployed version-16 schema without rewriting history', async () => {
    await reset();
    const migrations = loadControlMigrations();
    expect(migrations.slice(12, 16).map(({ version, checksum }) => ({ version, checksum })))
      .toEqual([
        { version: 13, checksum: 'c58e1668adf5eebf799af04b646b61aea6479f6ed6a419179ea938ec0f3af407' },
        { version: 14, checksum: 'e964d77251c3afbbf3729fcc627bfc25a3443859d0802529c542a59880d18407' },
        { version: 15, checksum: 'bc6016b0147bc37601b72d43125fdcaec9393699b511182e877038dc36b871f7' },
        { version: 16, checksum: 'e15d92b05cf16371f0c3f870876dc6e788a37df454f4d2e310c6e904f5afcb8d' },
      ]);
    await pool.query('CREATE SCHEMA agentops_control');
    await pool.query(`
      CREATE TABLE agentops_control.schema_migrations (
        version integer PRIMARY KEY CHECK (version > 0),
        name text NOT NULL UNIQUE,
        checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
        installed_at timestamptz NOT NULL DEFAULT clock_timestamp()
      )
    `);
    for (const migration of migrations.slice(0, 16)) {
      await pool.query(migration.sql);
      await pool.query(
        `INSERT INTO agentops_control.schema_migrations(version, name, checksum)
         VALUES ($1, $2, $3)`,
        [migration.version, migration.name, migration.checksum],
      );
    }
    const legacyRegistrationId = randomUUID();
    const legacyJobId = randomUUID();
    const legacyAttemptId = randomUUID();
    await pool.query(
      `UPDATE agentops_control.lifecycle_state
          SET mode = 'ACTIVE', generation = generation + 1,
              updated_at = clock_timestamp()
        WHERE singleton`,
    );
    await pool.query(
      `INSERT INTO agentops_control.repository_registrations(
         id, repository, configuration
       ) VALUES ($1, 'sample/legacy-needs-info', '{}'::jsonb)`,
      [legacyRegistrationId],
    );
    await pool.query(
      `INSERT INTO agentops_control.jobs(
         id, registration_id, registration_version, source_kind, source_key,
         idempotency_key, job_type, payload, status, result, finished_at
       ) VALUES (
         $1, $2, 1, 'poll', 'legacy-needs-info', 'legacy-needs-info',
         'agentops.triage', $3, 'succeeded', $4, clock_timestamp()
       )`,
      [
        legacyJobId,
        legacyRegistrationId,
        {
          schemaVersion: 1,
          repository: { owner: 'sample', name: 'legacy-needs-info' },
          issue: { number: 9, observedUpdatedAt: '2026-08-01T00:00:00Z' },
        },
        {
          decision: {
            readiness: 'needs_info',
            summary: 'Legacy issue needs detail.',
            missingInformation: Array.from({ length: 16 }, () => '不足'.repeat(250)),
          },
        },
      ],
    );
    await pool.query(
      `INSERT INTO agentops_control.job_attempts(
         id, job_id, attempt_number, worker_id, status, finished_at
       ) VALUES ($1, $2, 1, 'legacy-triage', 'succeeded', clock_timestamp())`,
      [legacyAttemptId, legacyJobId],
    );

    await expect(migrateControlSchema(pool)).resolves.toBe(CONTROL_SCHEMA_VERSION);
    await expect(assertControlSchema(pool)).resolves.toBeUndefined();
    const installed = await pool.query<{ version: number }>(
      `SELECT max(version)::integer AS version
         FROM agentops_control.schema_migrations`,
    );
    expect(installed.rows[0]?.version).toBe(CONTROL_SCHEMA_VERSION);
    const backfilled = await pool.query<{ blocker_length: number }>(
      `SELECT length(blocker)::integer AS blocker_length
         FROM agentops_control.development_progress_events
        WHERE job_id = $1 AND event_key = 'migration:triage-result'`,
      [legacyJobId],
    );
    expect(backfilled.rows).toEqual([{ blocker_length: 1000 }]);
  });

  it('fails closed when the database connection is unavailable', async () => {
    await expect(PostgresControlStore.open({
      connectionString: 'postgresql://postgres:unused@127.0.0.1:1/agentops',
      connectionTimeoutMillis: 200,
    })).rejects.toThrow(/failed closed/);
  });

  it('rolls back every DDL change when a migration fails', async () => {
    await reset();
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agentops-bad-migration-'));
    const directory = path.join(root, 'db', 'control-store', 'migrations');
    fs.mkdirSync(directory, { recursive: true });
    // Enumerated from the real migration set so adding a migration cannot make
    // this fixture silently non-contiguous instead of exercising the rollback.
    const migrationNames = fs
      .readdirSync(path.join(process.cwd(), 'db', 'control-store', 'migrations'))
      .filter((name) => name.endsWith('.sql'))
      .sort();
    expect(migrationNames).toHaveLength(CONTROL_SCHEMA_VERSION);
    for (const name of migrationNames) {
      const valid = fs.readFileSync(
        path.join(process.cwd(), 'db', 'control-store', 'migrations', name),
        'utf8',
      );
      fs.writeFileSync(
        path.join(directory, name),
        name.startsWith('0016_')
          ? `${valid}\nTHIS IS DELIBERATELY INVALID SQL;\n`
          : valid,
      );
    }
    await expect(migrateControlSchema(pool, { root })).rejects.toThrow(/failed closed/);
    const result = await pool.query<{ relation: string | null }>(
      `SELECT to_regclass('agentops_control.repository_registrations')::text AS relation`,
    );
    expect(result.rows[0]?.relation).toBeNull();
  });

  it('serializes fail-closed verification with schema migration ownership', async () => {
    await migratedStore();
    const blocker = await pool.connect();
    try {
      await blocker.query('SELECT pg_advisory_lock($1)', [CONTROL_MIGRATION_LOCK_KEY]);
      let settled = false;
      const verification = assertControlSchema(pool).then(() => {
        settled = true;
      });
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(settled).toBe(false);
      await blocker.query('SELECT pg_advisory_unlock($1)', [CONTROL_MIGRATION_LOCK_KEY]);
      await expect(verification).resolves.toBeUndefined();
      expect(settled).toBe(true);
    } finally {
      await blocker.query('SELECT pg_advisory_unlock($1)', [CONTROL_MIGRATION_LOCK_KEY]);
      blocker.release();
    }
  });

  it('deduplicates typed monitor work, fences stale leases, and rejects stale Registration', async () => {
    const store = await migratedStore();
    const repo = await store.createRegistration({
      repository: 'mrbaron3/workflow',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const requestId = await insertMonitorBrokerRequest({
      registrationId: repo.id,
      registrationVersion: repo.version,
      repository: repo.repository,
    });
    await expect(insertMonitorBrokerRequest({
      registrationId: repo.id,
      registrationVersion: repo.version,
    })).rejects.toMatchObject({ code: '23505' });

    const claims = await Promise.all([
      store.claimMonitorBrokerRequest({
        workerId: 'monitor-runner-a',
        leaseMs: 5_000,
      }),
      store.claimMonitorBrokerRequest({
        workerId: 'monitor-runner-b',
        leaseMs: 5_000,
      }),
    ]);
    const first = claims.find((claim) => claim !== null);
    expect(first).toMatchObject({
      id: requestId,
      registrationId: repo.id,
      registrationVersion: repo.version,
      repository: repo.repository,
      monitorKind: 'issue',
      cursor: { updatedAfter: '' },
    });
    expect(claims.filter((claim) => claim !== null)).toHaveLength(1);

    await pool.query(
      `UPDATE agentops_control.monitor_broker_requests
          SET lease_expires_at = clock_timestamp() - interval '1 second'
        WHERE id = $1`,
      [requestId],
    );
    await expect(store.failMonitorBrokerRequest({
      request: first!,
      workerId: claims[0] === first ? 'monitor-runner-a' : 'monitor-runner-b',
      code: 'late_provider_failure',
      message: 'must not overwrite an expired lease',
    })).rejects.toThrow(/stale or lost/);
    const stillLeased = await pool.query<{ status: string }>(
      `SELECT status
         FROM agentops_control.monitor_broker_requests
        WHERE id = $1`,
      [requestId],
    );
    expect(stillLeased.rows[0]?.status).toBe('leased');
    const recovered = await store.claimMonitorBrokerRequest({
      workerId: 'monitor-runner-recovery',
      leaseMs: 5_000,
    });
    expect(recovered).toMatchObject({ id: requestId });
    expect(recovered?.leaseToken).not.toBe(first?.leaseToken);
    const response = {
      items: [],
      nextCursor: { updatedAfter: '' },
      observedAt: new Date().toISOString(),
    };
    await expect(store.completeMonitorBrokerRequest({
      request: first!,
      workerId: claims[0] === first ? 'monitor-runner-a' : 'monitor-runner-b',
      response,
    })).rejects.toThrow(/stale or lost/);
    await store.completeMonitorBrokerRequest({
      request: recovered!,
      workerId: 'monitor-runner-recovery',
      response,
    });

    const staleId = await insertMonitorBrokerRequest({
      registrationId: repo.id,
      registrationVersion: repo.version,
      kind: 'pull_request',
    });
    await store.updateRegistration(repo.id, { enabled: false });
    await expect(store.claimMonitorBrokerRequest({
      workerId: 'monitor-runner-stale',
      leaseMs: 5_000,
    })).resolves.toBeNull();
    const stale = await pool.query<{
      status: string;
      error_code: string | null;
    }>(
      `SELECT status, error_code
         FROM agentops_control.monitor_broker_requests
        WHERE id = $1`,
      [staleId],
    );
    expect(stale.rows[0]).toMatchObject({
      status: 'failed',
      error_code: 'stale_registration',
    });
    const audit = await pool.query<{ event_type: string }>(
      `SELECT event_type
         FROM agentops_control.runtime_audit
        WHERE registration_id = $1
          AND event_type LIKE 'monitor.broker.%'
        ORDER BY id`,
      [repo.id],
    );
    expect(audit.rows.map((row) => row.event_type)).toEqual(expect.arrayContaining([
      'monitor.broker.claimed',
      'monitor.broker.completed',
      'monitor.broker.denied',
    ]));
  });

  it('grants broker capabilities only to the triage role', async () => {
    await pool.query(`
      DO $$ BEGIN
        IF NOT EXISTS (
          SELECT 1 FROM pg_roles WHERE rolname = 'agentops_triage'
        ) THEN
          CREATE ROLE agentops_triage NOLOGIN;
        END IF;
        IF NOT EXISTS (
          SELECT 1 FROM pg_roles WHERE rolname = 'agentops_runner'
        ) THEN
          CREATE ROLE agentops_runner NOLOGIN;
        END IF;
      END $$
    `);
    const store = await migratedStore();
    await pool.query(
      `GRANT USAGE ON SCHEMA agentops_control TO agentops_triage, agentops_runner`,
    );
    await pool.query(
      `GRANT SELECT ON agentops_control.monitor_broker_requests
         TO agentops_triage`,
    );
    const repo = await store.createRegistration({
      repository: 'acme/widgets',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const requestId = await insertMonitorBrokerRequest({
      registrationId: repo.id,
      registrationVersion: repo.version,
      repository: repo.repository,
    });
    const request = await store.claimMonitorBrokerRequest({
      workerId: 'capability-triage',
      leaseMs: 30_000,
    });
    expect(request).toMatchObject({ id: requestId });
    const validResponse = {
      items: [],
      nextCursor: { updatedAfter: '' },
      observedAt: new Date().toISOString(),
    };

    const triage = await pool.connect();
    try {
      await triage.query('SET ROLE agentops_triage');
      await expect(triage.query(
        `UPDATE agentops_control.monitor_broker_requests
            SET status = 'succeeded', response = '{}'::jsonb,
                completed_at = clock_timestamp()
          WHERE id = $1`,
        [requestId],
      )).rejects.toMatchObject({ code: '42501' });

      const wrongLease = await triage.query<{ registration_id: string | null }>(
        `SELECT agentops_control.complete_monitor_broker_request(
           $1, $2, $3, $4
         ) AS registration_id`,
        [requestId, randomUUID(), 'capability-triage', validResponse],
      );
      expect(wrongLease.rows[0]?.registration_id).toBeNull();

      await expect(triage.query(
        `SELECT agentops_control.complete_monitor_broker_request(
           $1, $2, $3, $4
         )`,
        [
          requestId,
          request!.leaseToken,
          'capability-triage',
          {
            items: [{
              repository: 'attacker/forged',
              kind: 'issue',
              number: 1,
              updatedAt: new Date().toISOString(),
            }],
            nextCursor: { updatedAfter: '' },
            observedAt: new Date().toISOString(),
          },
        ],
      )).rejects.toThrow(/invalid monitor broker response/);
    } finally {
      await triage.query('RESET ROLE');
      triage.release();
    }

    const runner = await pool.connect();
    try {
      await runner.query('SET ROLE agentops_runner');
      await expect(runner.query(
        `SELECT agentops_control.complete_monitor_broker_request(
           $1, $2, $3, $4
         )`,
        [requestId, request!.leaseToken, 'capability-runner', validResponse],
      )).rejects.toMatchObject({ code: '42501' });
    } finally {
      await runner.query('RESET ROLE');
      runner.release();
    }

    const durable = await pool.query<{
      status: string;
      response: Record<string, unknown> | null;
    }>(
      `SELECT status, response
         FROM agentops_control.monitor_broker_requests
        WHERE id = $1`,
      [requestId],
    );
    expect(durable.rows[0]).toEqual({ status: 'leased', response: null });
    await store.completeMonitorBrokerRequest({
      request: request!,
      workerId: 'capability-triage',
      response: validResponse,
    });
  });

  it('enforces triage and development job-type isolation in PostgreSQL', async () => {
    await pool.query(`
      DO $$ BEGIN
        IF NOT EXISTS (
          SELECT 1 FROM pg_roles WHERE rolname = 'agentops_triage'
        ) THEN
          CREATE ROLE agentops_triage NOLOGIN;
        END IF;
        IF NOT EXISTS (
          SELECT 1 FROM pg_roles WHERE rolname = 'agentops_runner'
        ) THEN
          CREATE ROLE agentops_runner NOLOGIN;
        END IF;
      END $$
    `);
    const store = await migratedStore();
    await pool.query(
      `GRANT USAGE ON SCHEMA agentops_control
         TO agentops_triage, agentops_runner`,
    );
    await pool.query(
      `GRANT SELECT, UPDATE ON agentops_control.jobs
         TO agentops_triage, agentops_runner`,
    );
    await pool.query(
      `GRANT SELECT, INSERT, UPDATE ON agentops_control.job_attempts,
         agentops_control.job_leases
         TO agentops_triage, agentops_runner`,
    );
    const triageRepo = await store.createRegistration({
      repository: 'sample/triage-target',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const developmentRepo = await store.createRegistration({
      repository: 'sample/development-target',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const triageJob = await store.enqueueJob({
      registrationId: triageRepo.id,
      registrationVersion: triageRepo.version,
      source: { kind: 'poll', key: 'triage-role-scope' },
      idempotencyKey: 'triage-role-scope',
      jobType: 'agentops.triage',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'triage-target' },
        issue: {
          number: 8,
          observedUpdatedAt: '2026-07-29T00:00:00.000Z',
        },
      },
    });
    const developmentJob = await store.enqueueJob({
      registrationId: developmentRepo.id,
      registrationVersion: developmentRepo.version,
      source: { kind: 'manual', key: 'development-role-scope' },
      idempotencyKey: 'development-role-scope',
      jobType: 'agentops.runner',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'development-target' },
        event: { kind: 'issue', number: 14, action: 'labeled' },
        target: { baseRef: 'refs/heads/main' },
        execution: {
          mode: 'development_turn',
          requiredChecks: ['test'],
          mergeMethod: 'squash',
          readyLabel: 'ready',
          claimedLabel: 'agent-claimed',
        },
        artifacts: [],
      },
    });

    for (const role of ['agentops_triage', 'agentops_runner'] as const) {
      const ownJob = role === 'agentops_triage'
        ? triageJob.job
        : developmentJob.job;
      const otherJob = role === 'agentops_triage'
        ? developmentJob.job
        : triageJob.job;
      const client = await pool.connect();
      try {
        await client.query(`SET ROLE ${role}`);
        const visible = await client.query<{ id: string; job_type: string }>(
          `SELECT id, job_type
             FROM agentops_control.jobs
            ORDER BY id`,
        );
        expect(visible.rows).toEqual([{
          id: ownJob.id,
          job_type: ownJob.jobType,
        }]);
        const allowed = await client.query<{ id: string }>(
          `UPDATE agentops_control.jobs
              SET last_error = NULL
            WHERE id = $1
            RETURNING id`,
          [ownJob.id],
        );
        expect(allowed.rows).toEqual([{ id: ownJob.id }]);
        await expect(client.query(
          `INSERT INTO agentops_control.job_attempts(
             id, job_id, attempt_number, worker_id, status
           ) VALUES ($1, $2, 1, 'own-role-check', 'running')`,
          [randomUUID(), ownJob.id],
        )).resolves.toMatchObject({ rowCount: 1 });
        const forged = await client.query<{ id: string }>(
          `UPDATE agentops_control.jobs
              SET last_error = 'cross-role-forgery'
            WHERE id = $1
            RETURNING id`,
          [otherJob.id],
        );
        expect(forged.rows).toEqual([]);
        await expect(client.query(
          `INSERT INTO agentops_control.job_attempts(
             id, job_id, attempt_number, worker_id, status
           ) VALUES ($1, $2, 1, 'cross-role-forgery', 'running')`,
          [randomUUID(), otherJob.id],
        )).rejects.toMatchObject({ code: '42501' });
      } finally {
        await client.query('RESET ROLE');
        client.release();
      }
    }
  });

  it('atomically promotes a triaged Issue with configured label semantics', async () => {
    const store = await migratedStore();
    const repo = await store.createRegistration({
      repository: 'sample/design-system',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: { mergeMethod: 'rebase' },
    });
    const queued = await store.enqueueJob({
      registrationId: repo.id,
      registrationVersion: repo.version,
      source: { kind: 'poll', key: 'issue:27' },
      idempotencyKey: 'issue:27',
      jobType: 'agentops.triage',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'design-system' },
        issue: {
          number: 27,
          observedUpdatedAt: '2026-07-29T00:00:00.000Z',
        },
      },
    });
    const lease = await store.acquireLease({
      workerId: 'triage-custom-labels',
      durationMs: 30_000,
      jobType: 'agentops.triage',
    });
    expect(lease?.job.id).toBe(queued.job.id);
    const promotedJobId = await store.promoteTriageLease({
      token: lease!.token,
      workerId: 'triage-custom-labels',
      readyLabel: 'human-approved',
      claimedLabel: 'automation-owned',
      authority: {
        actor: 'product-owner',
        readyAt: '2026-07-29T00:00:30.000Z',
        sourceIssue: frozenSourceIssue(
          repo.repository,
          27,
          '2026-07-29T00:00:29.000Z',
        ),
      },
      result: {
        schemaVersion: 1,
        status: 'succeeded',
        jobId: lease!.job.id,
        attemptNumber: lease!.attemptNumber,
        repository: repo.repository,
        issueNumber: 27,
        outcome: 'promoted',
        sourceDigest: null,
        decision: null,
        commentUrl: null,
        appliedLabels: [],
        promotedJobId: null,
        providerProvenance: null,
        completedAt: '2026-07-29T00:01:00.000Z',
      },
    });
    const jobs = await pool.query<{
      id: string;
      job_type: string;
      status: string;
      payload: Record<string, unknown>;
    }>(
      `SELECT id, job_type, status, payload
         FROM agentops_control.jobs
        WHERE id = ANY($1::uuid[])
        ORDER BY created_at, id`,
      [[queued.job.id, promotedJobId]],
    );
    expect(jobs.rows).toHaveLength(2);
    expect(jobs.rows.find((job) => job.id === queued.job.id)).toMatchObject({
      job_type: 'agentops.triage',
      status: 'succeeded',
    });
    expect(jobs.rows.find((job) => job.id === promotedJobId)).toMatchObject({
      job_type: 'agentops.runner',
      status: 'queued',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'design-system' },
        event: { kind: 'issue', number: 27, action: 'recovery' },
        execution: {
          mode: 'development_turn',
          mergeMethod: 'rebase',
          readyLabel: 'human-approved',
          claimedLabel: 'automation-owned',
        },
      },
    });
  });

  it('persists review perspectives and enforces exact-head child DAG integration', async () => {
    const store = await migratedStore();
    const registrations = await store.listRegistrations();
    const repo = registrations.find((candidate) =>
      candidate.repository === 'mrbaron3/servo')!;
    const policy = {
      authority: 'human-ready-allowed' as const,
      requiredGateSignals: [
        { source: 'repository-grader' as const, name: 'unit_tests' as const },
      ],
      requiredReviewPerspectives: ['security' as const, 'codeQuality' as const],
      minimumHeadEpochs: 1,
    };
    const parentRelease = (await store.createRelease({
      registrationId: repo.id,
      registrationVersion: repo.version,
      releaseKey: 'issue:71:review-dag',
      repository: repo.repository,
      issueNumber: 71,
      policy,
    })).release;
    const parentJob = await store.enqueueJob({
      registrationId: repo.id,
      registrationVersion: repo.version,
      source: { kind: 'manual', key: 'review-dag-parent' },
      idempotencyKey: 'review-dag-parent',
      jobType: 'agentops.runner',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'mrbaron3', name: 'servo' },
        event: { kind: 'issue', number: 71, action: 'recovery' },
        target: { baseRef: 'refs/heads/main' },
        execution: {
          mode: 'development_turn', requiredChecks: ['test'],
          mergeMethod: 'squash', readyLabel: 'ready',
          claimedLabel: 'agent-claimed',
        },
        artifacts: [],
      },
    });
    await store.linkJobToRelease({
      jobId: parentJob.job.id,
      releaseId: parentRelease.id,
    });
    const parentLease = await store.acquireLease({
      workerId: 'review-dag-parent', durationMs: 30_000,
      jobType: 'agentops.runner',
    });
    expect(parentLease?.job.id).toBe(parentJob.job.id);
    const parentHead = 'a'.repeat(40);
    const invalidLineageReview = (finding: Record<string, unknown>) => ({
      round: 1,
      headSha: parentHead,
      branch: 'agent/issue-71',
      pullRequestNumber: 81,
      outcome: 'request_changes',
      startedAt: '2026-08-04T00:58:00.000Z',
      completedAt: '2026-08-04T00:59:00.000Z',
      perspectives: [{
        perspective: 'security',
        verdict: 'request_changes',
        findings: [{
          criterionId: 'SEC-lineage', severity: 'major',
          expected: 'lineage reference pairing',
          observed: 'invalid pair',
          requiredFix: ['reject invalid pair'],
          disposition: 'in-change',
          ...finding,
        }],
      }],
    });
    await expect(pool.query(
      `SELECT agentops_control.record_development_review_round($1, $2, $3)`,
      [parentLease!.token, 'review-dag-parent', invalidLineageReview({
        lineage: 'persisted',
      })],
    )).rejects.toThrow(/development review finding is invalid/);
    await expect(pool.query(
      `SELECT agentops_control.record_development_review_round($1, $2, $3)`,
      [parentLease!.token, 'review-dag-parent', invalidLineageReview({
        lineage: 'new',
        lineageRef: `finding-origin-v1:${'f'.repeat(64)}`,
      })],
    )).rejects.toThrow(/development review finding is invalid/);
    await store.recordDevelopmentReviewRound({
      token: parentLease!.token,
      workerId: 'review-dag-parent',
      review: {
        round: 1,
        headSha: parentHead,
        branch: 'agent/issue-71',
        pullRequestNumber: 81,
        outcome: 'request_changes',
        startedAt: '2026-08-04T01:00:00.000Z',
        completedAt: '2026-08-04T01:01:00.000Z',
        perspectives: [{
          perspective: 'security',
          verdict: 'request_changes',
          findings: [{
            criterionId: 'SEC-child', severity: 'major',
            expected: 'isolated authorization boundary',
            observed: 'adjacent subsystem lacks its own guard',
            requiredFix: ['implement the adjacent authorization boundary'],
            disposition: 'separate-issue',
            separationReason: 'The subsystem is independently testable and outside parent scope.',
          }, {
            criterionId: 'QUALITY-child', severity: 'major',
            expected: 'independent migration coverage',
            observed: 'the adjacent migration lacks restart coverage',
            requiredFix: ['add isolated restart coverage'],
            disposition: 'separate-issue',
            separationReason: 'The migration is independent of the parent change.',
            lineage: 'new',
          }],
        }],
      },
    });
    await expect(store.recordDevelopmentReviewRound({
      token: parentLease!.token,
      workerId: 'review-dag-parent',
      review: {
        round: 1,
        headSha: parentHead,
        branch: 'agent/issue-71',
        pullRequestNumber: 81,
        outcome: 'approve',
        startedAt: '2026-08-04T01:00:00.000Z',
        completedAt: '2026-08-04T01:02:00.000Z',
        perspectives: [{
          perspective: 'security',
          verdict: 'approve',
          findings: [],
        }],
      },
    })).rejects.toThrow(/immutable/);
    const findingKey = `sha256:${'b'.repeat(64)}`;
    await expect(store.recordReviewChild({
      token: parentLease!.token,
      workerId: 'review-dag-parent',
      childIssueNumber: 99,
      childIssueUrl: 'https://github.com/mrbaron3/servo/issues/99',
      findingKey: `sha256:${'c'.repeat(64)}`,
      finding: {
        criterionId: 'SEC-fabricated', severity: 'major',
        expected: 'a durable finding',
        observed: 'no perspective recorded this finding',
        requiredFix: ['reject it'],
        disposition: 'separate-issue',
        separationReason: 'This fabricated record must not enter the DAG.',
      },
      reviewRound: 1,
      parentPullRequestNumber: 81,
      parentBranch: 'agent/issue-71',
      parentHeadSha: parentHead,
    })).rejects.toThrow(/durable review evidence/);
    const childNodeId = await store.recordReviewChild({
      token: parentLease!.token,
      workerId: 'review-dag-parent',
      childIssueNumber: 72,
      childIssueUrl: 'https://github.com/mrbaron3/servo/issues/72',
      findingKey,
      finding: {
        criterionId: 'SEC-child', severity: 'major',
        expected: 'isolated authorization boundary',
        observed: 'adjacent subsystem lacks its own guard',
        requiredFix: ['implement the adjacent authorization boundary'],
        disposition: 'separate-issue',
        separationReason: 'The subsystem is independently testable and outside parent scope.',
      },
      reviewRound: 1,
      parentPullRequestNumber: 81,
      parentBranch: 'agent/issue-71',
      parentHeadSha: parentHead,
    });
    await expect(store.recordReviewChild({
      token: parentLease!.token,
      workerId: 'review-dag-parent',
      childIssueNumber: 72,
      childIssueUrl: 'https://github.com/mrbaron3/servo/issues/72',
      findingKey,
      finding: {
        criterionId: 'SEC-child', severity: 'major',
        expected: 'isolated authorization boundary',
        observed: 'adjacent subsystem lacks its own guard',
        requiredFix: ['implement the adjacent authorization boundary'],
        disposition: 'separate-issue',
        separationReason: 'The subsystem is independently testable and outside parent scope.',
      },
      reviewRound: 1,
      parentPullRequestNumber: 81,
      parentBranch: 'agent/issue-71',
      parentHeadSha: parentHead,
    })).resolves.toBe(childNodeId);
    const siblingNodeId = await store.recordReviewChild({
      token: parentLease!.token,
      workerId: 'review-dag-parent',
      childIssueNumber: 73,
      childIssueUrl: 'https://github.com/mrbaron3/servo/issues/73',
      findingKey: `sha256:${'e'.repeat(64)}`,
      finding: {
        criterionId: 'QUALITY-child', severity: 'major',
        expected: 'independent migration coverage',
        observed: 'the adjacent migration lacks restart coverage',
        requiredFix: ['add isolated restart coverage'],
        disposition: 'separate-issue',
        separationReason: 'The migration is independent of the parent change.',
        lineage: 'new',
      },
      reviewRound: 1,
      parentPullRequestNumber: 81,
      parentBranch: 'agent/issue-71',
      parentHeadSha: parentHead,
    });
    await expect(store.reviewLineageGate(parentRelease.id)).resolves.toMatchObject({
      ready: false,
      pending: [
        { issueNumber: 72, status: 'pending' },
        { issueNumber: 73, status: 'pending' },
      ],
    });
    await expect(store.getReviewChildTarget(repo.repository, 72)).resolves.toMatchObject({
      nodeId: childNodeId,
      parentIssueNumber: 71,
      parentPullRequestNumber: 81,
      parentBranch: 'agent/issue-71',
      parentHeadSha: parentHead,
    });

    await pool.query(
      `UPDATE agentops_control.job_leases SET status = 'released'
        WHERE lease_token = $1`,
      [parentLease!.token],
    );
    await pool.query(
      `UPDATE agentops_control.job_attempts
          SET status = 'succeeded', finished_at = clock_timestamp()
        WHERE id = $1`,
      [parentLease!.attemptId],
    );
    await pool.query(
      `UPDATE agentops_control.jobs
          SET status = 'succeeded', finished_at = clock_timestamp()
        WHERE id = $1`,
      [parentJob.job.id],
    );
    const childRelease = (await store.createRelease({
      registrationId: repo.id,
      registrationVersion: repo.version,
      releaseKey: 'issue:72:review-dag',
      repository: repo.repository,
      issueNumber: 72,
      policy,
    })).release;
    const childJob = await store.enqueueJob({
      registrationId: repo.id,
      registrationVersion: repo.version,
      source: { kind: 'manual', key: 'review-dag-child' },
      idempotencyKey: 'review-dag-child',
      jobType: 'agentops.runner',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'mrbaron3', name: 'servo' },
        event: { kind: 'issue', number: 72, action: 'recovery' },
        target: {
          baseRef: 'refs/heads/agent/issue-71',
          headRef: parentHead,
        },
        execution: {
          mode: 'development_turn', requiredChecks: ['test'],
          mergeMethod: 'squash', readyLabel: 'ready',
          claimedLabel: 'agent-claimed',
        },
        artifacts: [],
      },
    });
    await store.linkJobToRelease({
      jobId: childJob.job.id,
      releaseId: childRelease.id,
    });
    const childLease = await store.acquireLease({
      workerId: 'review-dag-child', durationMs: 30_000,
      jobType: 'agentops.runner',
    });
    expect(childLease?.job.id).toBe(childJob.job.id);
    await expect(store.bindReviewChildRelease({
      token: childLease!.token,
      workerId: 'review-dag-child',
      releaseId: childRelease.id,
    })).resolves.toBe(childNodeId);
    const childHead = 'c'.repeat(40);
    const integratedHead = 'd'.repeat(40);
    await store.recordDevelopmentReviewRound({
      token: childLease!.token,
      workerId: 'review-dag-child',
      review: {
        round: 2,
        headSha: childHead,
        branch: 'agent/issue-72',
        pullRequestNumber: 82,
        outcome: 'request_changes',
        startedAt: '2026-08-04T02:00:00.000Z',
        completedAt: '2026-08-04T02:01:00.000Z',
        perspectives: [{
          perspective: 'codeQuality',
          verdict: 'request_changes',
          findings: [{
            criterionId: 'NESTED-child', severity: 'major',
            expected: 'nested work remains isolated',
            observed: 'a second independent boundary is missing',
            requiredFix: ['implement the nested boundary'],
            disposition: 'separate-issue',
            separationReason: 'The nested boundary has its own acceptance criteria.',
          }],
        }],
      },
    });
    const grandchildNodeId = await store.recordReviewChild({
      token: childLease!.token,
      workerId: 'review-dag-child',
      childIssueNumber: 74,
      childIssueUrl: 'https://github.com/mrbaron3/servo/issues/74',
      findingKey: `sha256:${'f'.repeat(64)}`,
      finding: {
        criterionId: 'NESTED-child', severity: 'major',
        expected: 'nested work remains isolated',
        observed: 'a second independent boundary is missing',
        requiredFix: ['implement the nested boundary'],
        disposition: 'separate-issue',
        separationReason: 'The nested boundary has its own acceptance criteria.',
      },
      reviewRound: 2,
      parentPullRequestNumber: 82,
      parentBranch: 'agent/issue-72',
      parentHeadSha: childHead,
    });
    await expect(store.reviewLineageGate(childRelease.id)).resolves.toMatchObject({
      ready: false,
      pending: [{ issueNumber: 74, status: 'pending' }],
    });
    await pool.query(
      `UPDATE agentops_control.releases
          SET status = 'merged', pull_request_number = 82,
              final_head = $2, merge_sha = $3, merge_actor = 'integration-test',
              completed_at = clock_timestamp(), updated_at = clock_timestamp()
        WHERE id = $1`,
      [childRelease.id, childHead, integratedHead],
    );
    await expect(store.markReviewChildIntegrated({
      token: childLease!.token,
      workerId: 'review-dag-child',
      releaseId: childRelease.id,
      pullRequestNumber: 82,
      headSha: childHead,
      integratedHeadSha: integratedHead,
    })).rejects.toThrow(/integration is invalid/);
    await pool.query(
      `UPDATE agentops_control.development_lineage_nodes
          SET status = 'integrated', integrated_head_sha = $2,
              integrated_at = clock_timestamp(), updated_at = clock_timestamp()
        WHERE id = $1`,
      [grandchildNodeId, '1'.repeat(40)],
    );
    await expect(store.markReviewChildIntegrated({
      token: childLease!.token,
      workerId: 'review-dag-child',
      releaseId: childRelease.id,
      pullRequestNumber: 82,
      headSha: childHead,
      integratedHeadSha: integratedHead,
    })).resolves.toBe(childNodeId);
    await pool.query(
      `UPDATE agentops_control.development_lineage_nodes
          SET status = 'integrated', integrated_head_sha = $2,
              integrated_at = clock_timestamp(), updated_at = clock_timestamp()
        WHERE id = $1`,
      [siblingNodeId, '2'.repeat(40)],
    );
    await expect(store.reviewLineageGate(parentRelease.id)).resolves.toEqual({
      ready: true,
      pending: [],
      integratedHeads: [integratedHead, '2'.repeat(40)],
    });

    const root = await pool.query<{ id: string }>(
      `SELECT id FROM agentops_control.development_lineage_nodes
        WHERE release_id = $1`,
      [parentRelease.id],
    );
    await expect(pool.query(
      `UPDATE agentops_control.development_lineage_nodes
          SET parent_node_id = $2 WHERE id = $1`,
      [root.rows[0]!.id, childNodeId],
    )).rejects.toThrow(/cycle/);
    const review = await pool.query<{
      outcome: string;
      perspective: string;
      finding_count: number;
    }>(
      `SELECT round.outcome, perspective.perspective, perspective.finding_count
         FROM agentops_control.development_review_rounds round
         JOIN agentops_control.development_review_perspectives perspective
           ON perspective.review_round_id = round.id
        WHERE round.job_id = $1`,
      [parentJob.job.id],
    );
    expect(review.rows).toEqual([{
      outcome: 'request_changes', perspective: 'security', finding_count: 2,
    }]);
  });

  it('deduplicates concurrent webhook and poll enqueue and duplicate deliveries', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    const [webhook, poll] = await Promise.all([
      enqueue(store, repo.id, repo.version, 'same-logical-event', 'webhook'),
      enqueue(store, repo.id, repo.version, 'same-logical-event', 'poll'),
    ]);
    expect(new Set([webhook.job.id, poll.job.id]).size).toBe(1);
    expect([webhook.duplicate, poll.duplicate].sort()).toEqual([false, true]);
    expect(JobEnvelopeContract.parse(webhook.job)).toEqual(webhook.job);
    const otherRepo = await registration(store, '-other');
    const other = await enqueue(
      store,
      otherRepo.id,
      otherRepo.version,
      'same-logical-event',
      'webhook',
    );
    expect(other.job.id).not.toBe(webhook.job.id);
    expect(other.job.registrationId).toBe(otherRepo.id);
    await expect(store.enqueueJob({
      registrationId: repo.id,
      registrationVersion: repo.version,
      source: { kind: 'manual', key: 'manual:mismatched-reuse' },
      idempotencyKey: 'same-logical-event',
      jobType: 'github_issue_turn',
      payload: { issueNumber: 13 },
    })).rejects.toBeInstanceOf(IdempotencyConflictError);

    const deliveries = await Promise.all(Array.from({ length: 8 }, () =>
      store.receiveWebhook({
        deliveryKey: 'github-delivery-1',
        repository: repo.repository,
        event: 'issues',
        headers: {
          authorization: 'Bearer secret',
          cookie: 'session=secret',
          'x-github-delivery': 'github-delivery-1',
          'x-hub-signature-256': 'sha256=secret',
        },
        payload: { action: 'labeled' },
      })));
    expect(new Set(deliveries.map((row) => row.deliveryId)).size).toBe(1);
    expect(deliveries.filter((row) => !row.duplicate)).toHaveLength(1);
    const stored = await pool.query<{ headers: Record<string, string> }>(
      'SELECT headers FROM agentops_control.webhook_deliveries WHERE id = $1',
      [deliveries[0]!.deliveryId],
    );
    expect(stored.rows[0]?.headers).toEqual({
      'x-github-delivery': 'github-delivery-1',
    });
  });

  it('requeues an observation rejected only by a Registration version change', async () => {
    const store = await migratedStore();
    const repo = await registration(store, '-requeue');
    const first = await enqueue(
      store,
      repo.id,
      repo.version,
      'versioned-observation',
      'webhook',
    );
    const updated = await store.updateRegistration(repo.id, { configuration: {} });
    const recovered = await enqueue(
      store,
      updated.id,
      updated.version,
      'versioned-observation',
      'poll',
    );
    expect(recovered.duplicate).toBe(false);
    expect(recovered.job).toMatchObject({
      id: first.job.id,
      registrationVersion: updated.version,
      status: 'queued',
    });
    const audit = await pool.query<{ count: string }>(
      `SELECT count(*) FROM agentops_control.runtime_audit
        WHERE job_id = $1
          AND event_type = 'job.requeued_after_registration_change'`,
      [first.job.id],
    );
    expect(Number(audit.rows[0]?.count)).toBe(1);
    await expect(pool.query(
      `UPDATE agentops_control.repository_registrations
          SET configuration = '{"command":"unsafe"}'::jsonb
        WHERE id = $1`,
      [repo.id],
    )).rejects.toThrow();
  });

  it('does not steal live webhook work and recovers only expired ownership after restart', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    const receipt = await store.receiveWebhook({
      deliveryKey: 'interrupted-delivery',
      repository: repo.repository,
      event: 'issues',
      headers: {},
      payload: { action: 'labeled' },
    });
    const first = await store.setWebhookConsumers(
      receipt.deliveryId,
      repo.id,
      ['agentops', 'audit'],
      80,
    );
    await store.completeWebhookConsumer(receipt.deliveryId, 'agentops', first.token);
    await expect(store.completeWebhookConsumer(
      receipt.deliveryId,
      'absent',
      first.token,
    )).rejects.toBeInstanceOf(LeaseRejectedError);

    const restarted = new PostgresControlStore(pool);
    expect(await restarted.recoverInterruptedWebhooks()).toBe(0);
    await restarted.heartbeatWebhookProcessing(first.token, 80);
    expect(await restarted.recoverInterruptedWebhooks()).toBe(0);
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(await restarted.recoverInterruptedWebhooks()).toBe(1);
    await expect(restarted.completeWebhookConsumer(
      receipt.deliveryId,
      'audit',
      first.token,
    )).rejects.toBeInstanceOf(LeaseRejectedError);
    const state = await pool.query<{ status: string }>(
      'SELECT status FROM agentops_control.webhook_deliveries WHERE id = $1',
      [receipt.deliveryId],
    );
    expect(state.rows[0]?.status).toBe('pending');

    const claimed = await Promise.all([
      restarted.claimPendingWebhook(),
      restarted.claimPendingWebhook(),
    ]);
    const second = claimed.find((row) => row !== null);
    expect(claimed.filter(Boolean)).toHaveLength(1);
    expect(second).toMatchObject({
      deliveryId: receipt.deliveryId,
      deliveryKey: 'interrupted-delivery',
      repository: repo.repository,
      payload: { action: 'labeled' },
    });
    await restarted.setClaimedWebhookConsumers(
      second!.token,
      repo.id,
      ['agentops', 'audit'],
    );
    await restarted.completeWebhookConsumer(receipt.deliveryId, 'audit', second!.token);
    await restarted.finishWebhookDelivery(
      receipt.deliveryId,
      { status: 'processed' },
      second!.token,
    );
    const completed = await pool.query<{ status: string }>(
      'SELECT status FROM agentops_control.webhook_deliveries WHERE id = $1',
      [receipt.deliveryId],
    );
    expect(completed.rows[0]?.status).toBe('processed');
  });

  it('keeps monitor cursors monotonic and releases stale queued single-flight work', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    const observedAt = new Date('2026-07-25T01:02:03.000Z');
    await expect(store.saveMonitorCursor({
      registrationId: repo.id,
      monitorKind: 'issue',
      cursor: { issueUpdatedAt: '2026-07-25T01:00:00Z', issueNumber: 12 },
      observedAt,
    })).resolves.toBe(true);
    await expect(store.saveMonitorCursor({
      registrationId: repo.id,
      monitorKind: 'issue',
      cursor: { issueUpdatedAt: '2026-07-24T23:00:00Z', issueNumber: 11 },
      observedAt: new Date('2026-07-25T00:00:00.000Z'),
    })).resolves.toBe(false);
    await expect(store.getMonitorCursor(repo.id, 'issue')).resolves.toMatchObject({
      cursor: { issueUpdatedAt: '2026-07-25T01:00:00Z', issueNumber: 12 },
      observedAt: observedAt.toISOString(),
    });
    const stale = await enqueue(store, repo.id, repo.version, 'stale-registration');
    const updated = await store.updateRegistration(repo.id, { issueMonitorEnabled: false });
    await expect(store.updateRegistration(repo.id, { enabled: undefined }))
      .rejects.toThrow(/patch is empty/);
    const staleStatus = await pool.query<{ status: string }>(
      'SELECT status FROM agentops_control.jobs WHERE id = $1',
      [stale.job.id],
    );
    expect(staleStatus.rows[0]?.status).toBe('rejected');
    await expect(enqueue(store, repo.id, repo.version, 'stale-registration'))
      .rejects.toThrow(/stale/);
    await expect(enqueue(store, repo.id, updated.version, 'replacement-registration'))
      .resolves.toMatchObject({ duplicate: false });
  });

  it('lets only one competing worker acquire a lease', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    await enqueue(store, repo.id, repo.version, 'lease-race');
    const leases = await Promise.all(
      Array.from({ length: 12 }, (_, index) =>
        store.acquireLease({ workerId: `worker-${index}`, durationMs: 10_000 })),
    );
    expect(leases.filter(Boolean)).toHaveLength(1);
  });

  it('scopes isolated-runner claim and reclaim to agentops.runner jobs', async () => {
    const store = await migratedStore();
    const legacyRegistration = await registration(store, '-legacy');
    const runnerRegistration = await registration(store, '-runner');
    await enqueue(
      store,
      legacyRegistration.id,
      legacyRegistration.version,
      'legacy-first',
    );
    const runnerJob = await enqueueRunner(
      store,
      runnerRegistration.id,
      runnerRegistration.version,
      '-runner',
    );

    const runnerLease = await store.acquireLease({
      workerId: 'isolated-runner',
      durationMs: 10_000,
      jobType: 'agentops.runner',
    });
    expect(runnerLease?.job.id).toBe(runnerJob.job.id);
    await store.finishLease(runnerLease!.token, { status: 'succeeded' });

    const legacyLease = await store.acquireLease({
      workerId: 'legacy-consumer',
      durationMs: 20,
    });
    expect(legacyLease?.job.jobType).toBe('github_issue_turn');
    await new Promise((resolve) => setTimeout(resolve, 40));
    expect(await store.reclaimExpiredLeases(3, {
      jobType: 'agentops.runner',
      retryBaseMs: 1_000,
    })).toBe(0);
    const legacyState = await pool.query<{ status: string }>(
      `SELECT status FROM agentops_control.job_leases WHERE id = $1`,
      [legacyLease!.id],
    );
    expect(legacyState.rows[0]?.status).toBe('active');
  });

  it('heartbeats, times out, reclaims, and preserves attempt history', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    await enqueue(store, repo.id, repo.version, 'reclaim');
    const first = await store.acquireLease({ workerId: 'worker-a', durationMs: 60 });
    expect(first).not.toBeNull();
    await store.heartbeatLease(first!.token, 120);
    await new Promise((resolve) => setTimeout(resolve, 150));
    await expect(store.heartbeatLease(first!.token, 100))
      .rejects.toBeInstanceOf(LeaseRejectedError);
    await expect(store.failOrRetryLease({
      token: first!.token,
      workerId: 'worker-a',
      failure: {
        schemaVersion: 1,
        status: 'failed',
        code: 'provider_failure',
        message: 'stale worker must not retry',
        retryable: true,
        boundary: 'provider',
        observedAt: new Date().toISOString(),
      },
      retryDelayMs: 0,
      maxAttempts: 2,
    })).rejects.toBeInstanceOf(LeaseRejectedError);
    expect(await store.reclaimExpiredLeases()).toBe(1);
    const second = await store.acquireLease({ workerId: 'worker-b', durationMs: 1_000 });
    expect(second?.job.id).toBe(first?.job.id);
    expect(second?.attemptNumber).toBe(2);
    const attempts = await pool.query<{
      status: string;
      failure: Record<string, unknown> | null;
    }>(
      `SELECT status, failure FROM agentops_control.job_attempts
        WHERE job_id = $1 ORDER BY attempt_number`,
      [first!.job.id],
    );
    expect(attempts.rows.map((row) => row.status)).toEqual(['timed_out', 'running']);
    expect(attempts.rows[0]?.failure).toMatchObject({
      schemaVersion: 1,
      status: 'failed',
      code: 'lease_lost',
      retryable: true,
      boundary: null,
    });
    const artifactId = await store.linkArtifact({
      jobId: second!.job.id,
      attemptId: second!.attemptId,
      kind: 'test-output',
      uri: `volume://registrations/${repo.id}/tests/attempt-2.log`,
      sha256: 'a'.repeat(64),
      sizeBytes: 1234,
    });
    await store.appendAudit({
      actorType: 'worker',
      actorId: 'worker-b',
      eventType: 'artifact_linked',
      registrationId: repo.id,
      jobId: second!.job.id,
      details: { artifactId },
    });
    const metadata = await pool.query<{ uri: string; size_bytes: string }>(
      'SELECT uri, size_bytes FROM agentops_control.artifact_links WHERE id = $1',
      [artifactId],
    );
    expect(metadata.rows[0]).toMatchObject({
      uri: `volume://registrations/${repo.id}/tests/attempt-2.log`,
      size_bytes: '1234',
    });
    await store.finishLease(second!.token, { status: 'succeeded' });
  });

  it('records idempotent Issue progress only for the live runner lease', async () => {
    await pool.query(`
      DO $$ BEGIN
        IF NOT EXISTS (
          SELECT 1 FROM pg_roles WHERE rolname = 'agentops_runner'
        ) THEN
          CREATE ROLE agentops_runner NOLOGIN;
        END IF;
      END $$
    `);
    const store = await migratedStore();
    await pool.query(
      'GRANT USAGE ON SCHEMA agentops_control TO agentops_runner',
    );
    const registered = await registration(store, '-progress');
    await enqueueRunner(store, registered.id, registered.version, '-progress');
    const lease = await store.acquireLease({
      workerId: 'progress-runner',
      durationMs: 30_000,
      jobType: 'agentops.runner',
    });
    expect(lease).not.toBeNull();

    const firstId = await store.recordDevelopmentProgress({
      token: lease!.token,
      workerId: lease!.workerId,
      event: {
        eventKey: 'generation:source:a1:start',
        phase: 'generation',
        step: 'generator session attempt 1/3',
        state: 'running',
        summary: 'Implementing in an isolated worktree',
        nextGate: 'repository graders',
        sessionName: 'ao-progress-source-s0',
        worktreePath: '/workspace/registrations/repo/jobs/job/attempt-1/worktree',
        branch: 'agent/progress-source-s0',
        parentIssueNumber: 1,
        headSha: '0123456789012345678901234567890123456789',
        reviewRound: 1,
        reviewOutcome: 'running',
        gateKey: 'review',
      },
    });
    const refreshedId = await store.recordDevelopmentProgress({
      token: lease!.token,
      workerId: lease!.workerId,
      event: {
        eventKey: 'generation:source:a1:start',
        phase: 'generation',
        step: 'generator session attempt 1/3',
        state: 'running',
        summary: 'Generator is still active in its isolated worktree',
        nextGate: 'repository graders',
        sessionName: 'ao-progress-source-s0',
        worktreePath: '/workspace/registrations/repo/jobs/job/attempt-1/worktree',
        branch: 'agent/progress-source-s0',
        parentIssueNumber: 1,
        headSha: '0123456789012345678901234567890123456789',
        reviewRound: 1,
        reviewOutcome: 'running',
        gateKey: 'review',
      },
    });
    expect(refreshedId).toBe(firstId);

    const runner = await pool.connect();
    try {
      await runner.query('SET ROLE agentops_runner');
      await expect(runner.query(
        'SELECT count(*) FROM agentops_control.development_progress_events',
      )).rejects.toMatchObject({ code: '42501' });
      const capability = await runner.query<{ id: string }>(
        `SELECT agentops_control.record_development_progress($1, $2, $3)
           AS id`,
        [
          lease!.token,
          lease!.workerId,
          {
            eventKey: 'generation:source:a1:start',
            phase: 'generation',
            step: 'generator session attempt 1/3',
            state: 'running',
            summary: 'Runner capability updated progress without table SELECT',
            nextGate: 'repository graders',
            sessionName: 'ao-progress-source-s0',
            worktreePath:
              '/workspace/registrations/repo/jobs/job/attempt-1/worktree',
            branch: 'agent/progress-source-s0',
            parentIssueNumber: 1,
            headSha: '0123456789012345678901234567890123456789',
            reviewRound: 1,
            reviewOutcome: 'running',
            gateKey: 'review',
          },
        ],
      );
      expect(capability.rows).toEqual([{ id: String(firstId) }]);
    } finally {
      await runner.query('RESET ROLE');
      runner.release();
    }

    const durable = await pool.query<{
      repository: string;
      subject_kind: string;
      subject_number: string;
      worker_id: string;
      summary: string;
      parent_issue_number: string;
      head_sha: string;
      review_round: number;
      review_outcome: string;
      gate_key: string;
      count: string;
    }>(
      `SELECT max(repository) AS repository,
              max(subject_kind) AS subject_kind,
              max(subject_number)::text AS subject_number,
              max(worker_id) AS worker_id,
              max(summary) AS summary,
              max(parent_issue_number)::text AS parent_issue_number,
              max(head_sha) AS head_sha,
              max(review_round) AS review_round,
              max(review_outcome) AS review_outcome,
              max(gate_key) AS gate_key,
              count(*)::text AS count
         FROM agentops_control.development_progress_events
        WHERE job_id = $1`,
      [lease!.job.id],
    );
    expect(durable.rows[0]).toMatchObject({
      repository: registered.repository,
      subject_kind: 'issue',
      subject_number: '14',
      worker_id: lease!.workerId,
      summary: 'Runner capability updated progress without table SELECT',
      parent_issue_number: '1',
      head_sha: '0123456789012345678901234567890123456789',
      review_round: 1,
      review_outcome: 'running',
      gate_key: 'review',
      count: '1',
    });

    await store.finishLease(lease!.token, {
      status: 'failed',
      error: 'synthetic terminal failure',
    });
    await expect(store.recordDevelopmentProgress({
      token: lease!.token,
      workerId: lease!.workerId,
      event: {
        eventKey: 'failed:late',
        phase: 'failed',
        step: 'late report',
        state: 'failed',
      },
    })).rejects.toThrow(/lease identity is invalid/);
  });

  it('backfills terminal pre-progress runner jobs with their blocker', async () => {
    const store = await migratedStore();
    const registered = await registration(store, '-progress-backfill');
    await enqueueRunner(
      store,
      registered.id,
      registered.version,
      '-progress-backfill',
    );
    const lease = await store.acquireLease({
      workerId: 'legacy-progress-runner',
      durationMs: 30_000,
      jobType: 'agentops.runner',
    });
    expect(lease).not.toBeNull();
    await store.finishLease(lease!.token, {
      status: 'failed',
      error: 'required checks are still pending',
    });
    const migration = fs.readFileSync(
      path.join(
        process.cwd(),
        'db',
        'control-store',
        'migrations',
        '0015_development_progress_backfill.sql',
      ),
      'utf8',
    );
    await pool.query(migration);
    const progress = await pool.query<{
      phase: string;
      state: string;
      blocker: string | null;
      subject_number: string;
      event_key: string;
    }>(
      `SELECT phase, state, blocker, subject_number::text, event_key
         FROM agentops_control.development_progress_events
        WHERE job_id = $1`,
      [lease!.job.id],
    );
    expect(progress.rows).toEqual([{
      phase: 'failed',
      state: 'failed',
      blocker: 'required checks are still pending',
      subject_number: '14',
      event_key: 'migration:terminal-job',
    }]);
  });

  it('rejects an expired leased job after its registration changes', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    await enqueue(store, repo.id, repo.version, 'leased-before-registration-change');
    const lease = await store.acquireLease({ workerId: 'worker-stale', durationMs: 60 });
    expect(lease).not.toBeNull();
    const updated = await store.updateRegistration(repo.id, { prMonitorEnabled: false });
    await new Promise((resolve) => setTimeout(resolve, 80));
    expect(await store.reclaimExpiredLeases()).toBe(1);
    const stale = await pool.query<{
      status: string;
      failure: Record<string, unknown>;
    }>(
      'SELECT status, failure FROM agentops_control.jobs WHERE id = $1',
      [lease!.job.id],
    );
    expect(stale.rows[0]?.status).toBe('rejected');
    expect(stale.rows[0]?.failure).toMatchObject({
      schemaVersion: 1,
      status: 'failed',
      code: 'registration_stale',
      retryable: false,
      boundary: 'claim',
    });
    await expect(enqueue(
      store,
      repo.id,
      updated.version,
      'replacement-after-stale-lease',
    )).resolves.toMatchObject({ duplicate: false });
  });

  it('terminates crash/expiry retries at the configured attempt ceiling', async () => {
    const store = await migratedStore();
    const repo = await registration(store, '-expiry-ceiling');
    await enqueue(store, repo.id, repo.version, 'expiry-ceiling');
    const lease = await store.acquireLease({
      workerId: 'worker-expiry-ceiling',
      durationMs: 40,
    });
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(await store.reclaimExpiredLeases(1)).toBe(1);
    await expect(store.acquireLease({
      workerId: 'worker-after-ceiling',
      durationMs: 1_000,
    })).resolves.toBeNull();
    const outcome = await pool.query<{
      status: string;
      failure: Record<string, unknown>;
    }>(
      'SELECT status, failure FROM agentops_control.jobs WHERE id = $1',
      [lease!.job.id],
    );
    expect(outcome.rows[0]).toMatchObject({
      status: 'failed',
      failure: {
        schemaVersion: 1,
        code: 'lease_lost',
        retryable: false,
      },
    });
  });

  it('serializes expired lease reclaim with a concurrent registration update', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    await enqueue(store, repo.id, repo.version, 'concurrent-reclaim');
    const lease = await store.acquireLease({
      workerId: 'worker-concurrent-reclaim',
      durationMs: 60,
    });
    expect(lease).not.toBeNull();
    await new Promise((resolve) => setTimeout(resolve, 80));

    const updater = await pool.connect();
    try {
      await updater.query('BEGIN');
      await updater.query(
        `SELECT id FROM agentops_control.repository_registrations
          WHERE id = $1 FOR UPDATE`,
        [repo.id],
      );
      await updater.query(
        `UPDATE agentops_control.repository_registrations
            SET version = version + 1, pr_monitor_enabled = false
          WHERE id = $1`,
        [repo.id],
      );
      const missed = await updater.query(
        `UPDATE agentops_control.jobs
            SET status = 'rejected'
          WHERE registration_id = $1 AND status = 'queued'`,
        [repo.id],
      );
      expect(missed.rowCount).toBe(0);
      await expect(store.reclaimExpiredLeases()).resolves.toBe(0);
      await updater.query('COMMIT');
    } catch (error) {
      await updater.query('ROLLBACK');
      throw error;
    } finally {
      updater.release();
    }

    await expect(store.reclaimExpiredLeases()).resolves.toBe(1);
    const stale = await pool.query<{ status: string }>(
      'SELECT status FROM agentops_control.jobs WHERE id = $1',
      [lease!.job.id],
    );
    expect(stale.rows[0]?.status).toBe('rejected');
    const updated = await store.getRegistration(repo.id);
    await expect(enqueue(
      store,
      repo.id,
      updated!.version,
      'replacement-after-concurrent-reclaim',
    )).resolves.toMatchObject({ duplicate: false });
  });

  it('enforces repository single-flight in parallel transactions and at runtime', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    const results = await Promise.allSettled([
      enqueue(store, repo.id, repo.version, 'parallel-a', 'webhook'),
      enqueue(store, repo.id, repo.version, 'parallel-b', 'poll'),
    ]);
    expect(results.filter((result) => result.status === 'fulfilled')).toHaveLength(1);
    const rejected = results.find((result) => result.status === 'rejected');
    expect(rejected?.status === 'rejected' && rejected.reason)
      .toBeInstanceOf(RepositoryBusyError);
    await expect(enqueue(store, repo.id, repo.version, 'runtime-reject'))
      .rejects.toBeInstanceOf(RepositoryBusyError);
  });

  it('reconciles durable work after a deliberately missed notification', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    const queued = await enqueue(store, repo.id, repo.version, 'missed-notify');
    // No LISTEN connection was active when enqueue committed.
    const work = await store.listReconciliationWork();
    expect(work).toEqual([
      expect.objectContaining({
        jobId: queued.job.id,
        registrationId: repo.id,
        repository: repo.repository,
        status: 'queued',
      }),
    ]);
  });

  it('wakes LISTEN clients and still leaves work for periodic reconciliation', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    const notified = new Promise<string>((resolve) => {
      void store.listen('agentops_job_wake', (notice) => {
        resolve(notice.payload ?? '');
      }).then(async (stop) => {
        await enqueue(store, repo.id, repo.version, 'listen-notify');
        await notified;
        await stop();
      });
    });
    await expect(notified).resolves.toContain('"table"');
    await expect(store.listReconciliationWork()).resolves.toHaveLength(1);
  });

  it('wakes webhook listeners and lets a restarted router claim pending work', async () => {
    const store = await migratedStore();
    const repo = await registration(store);
    let resolveNotice!: (payload: string) => void;
    const notified = new Promise<string>((resolve) => {
      resolveNotice = resolve;
    });
    const stop = await store.listen('agentops_webhook_wake', (notice) => {
      resolveNotice(notice.payload ?? '');
    });
    await store.receiveWebhook({
      deliveryKey: 'pending-across-restart',
      repository: repo.repository,
      event: 'issues',
      headers: {},
      payload: { action: 'opened' },
    });
    await expect(notified).resolves.toContain('webhook_deliveries');
    await stop();

    const restarted = new PostgresControlStore(pool);
    await expect(restarted.claimPendingWebhook()).resolves.toMatchObject({
      deliveryKey: 'pending-across-restart',
      repository: repo.repository,
      payload: { action: 'opened' },
    });
  });

  it('audits fail-closed Registration races at claim/provider/push/merge/release', async () => {
    const store = await migratedStore();
    const boundaries = ['claim', 'provider', 'push', 'merge', 'release'] as const;
    for (const boundary of boundaries) {
      const suffix = `-${boundary}`;
      const repo = await registration(store, suffix);
      await enqueueRunner(store, repo.id, repo.version, suffix);
      if (boundary === 'claim') {
        await store.updateRegistration(repo.id, { executionEnabled: false });
        await expect(store.acquireLease({
          workerId: `runner-${boundary}`,
          durationMs: 5_000,
        })).resolves.toBeNull();
      } else {
        const lease = await store.acquireLease({
          workerId: `runner-${boundary}`,
          durationMs: 5_000,
        });
        expect(lease).not.toBeNull();
        await store.updateRegistration(repo.id, { executionEnabled: false });
        await expect(store.assertExecutionGuard({
          token: lease!.token,
          workerId: `runner-${boundary}`,
          boundary,
        })).resolves.toMatchObject({
          ok: false,
          reason: 'registration_execution_disabled',
          jobId: lease!.job.id,
        });
      }
      const audit = await pool.query<{
        event_type: string;
        reason: string;
      }>(
        `SELECT event_type, details->>'reason' AS reason
           FROM agentops_control.runtime_audit
          WHERE registration_id = $1
            AND event_type = $2`,
        [repo.id, `runner.boundary.${boundary}.denied`],
      );
      expect(audit.rows).toEqual([{
        event_type: `runner.boundary.${boundary}.denied`,
        reason: 'registration_execution_disabled',
      }]);
    }
  });

  it('revalidates a live lease at every allowed critical boundary', async () => {
    const store = await migratedStore();
    const repo = await registration(store, '-allowed-boundaries');
    await enqueueRunner(store, repo.id, repo.version, '-allowed-boundaries');
    const lease = await store.acquireLease({
      workerId: 'runner-allowed',
      durationMs: 10_000,
    });
    expect(lease).not.toBeNull();
    for (const boundary of ['provider', 'push', 'merge', 'release'] as const) {
      await expect(store.assertExecutionGuard({
        token: lease!.token,
        workerId: 'runner-allowed',
        boundary,
      })).resolves.toMatchObject({
        ok: true,
        reason: null,
        jobId: lease!.job.id,
        registration: {
          id: repo.id,
          version: repo.version,
          enabled: true,
          executionEnabled: true,
        },
      });
    }
    const events = await pool.query<{ event_type: string }>(
      `SELECT event_type
         FROM agentops_control.runtime_audit
        WHERE job_id = $1 AND event_type LIKE 'runner.boundary.%.allowed'
        ORDER BY event_type`,
      [lease!.job.id],
    );
    expect(events.rows.map((row) => row.event_type)).toEqual([
      'runner.boundary.claim.allowed',
      'runner.boundary.merge.allowed',
      'runner.boundary.provider.allowed',
      'runner.boundary.push.allowed',
      'runner.boundary.release.allowed',
    ]);
  });

  it('records typed retry history and never retries stale Registration work', async () => {
    const store = await migratedStore();
    const repo = await registration(store, '-typed-retry');
    await enqueueRunner(store, repo.id, repo.version, '-typed-retry');
    const first = await store.acquireLease({
      workerId: 'runner-retry',
      durationMs: 10_000,
    });
    const failure = {
      schemaVersion: 1 as const,
      status: 'failed' as const,
      code: 'provider_failure' as const,
      message: 'provider temporarily unavailable',
      retryable: true,
      boundary: 'provider' as const,
      observedAt: new Date().toISOString(),
    };
    await expect(store.failOrRetryLease({
      token: first!.token,
      workerId: 'runner-retry',
      failure,
      retryDelayMs: 0,
      maxAttempts: 2,
    })).resolves.toBe('queued');
    const second = await store.acquireLease({
      workerId: 'runner-retry',
      durationMs: 10_000,
    });
    expect(second?.attemptNumber).toBe(2);
    await store.updateRegistration(repo.id, { executionEnabled: false });
    await expect(store.failOrRetryLease({
      token: second!.token,
      workerId: 'runner-retry',
      failure,
      retryDelayMs: 0,
      maxAttempts: 3,
    })).resolves.toBe('rejected');
    const persisted = await pool.query<{
      job_status: string;
      job_failure: Record<string, unknown>;
      attempt_status: string;
      attempt_failure: Record<string, unknown>;
    }>(
      `SELECT j.status AS job_status, j.failure AS job_failure,
              a.status AS attempt_status, a.failure AS attempt_failure
         FROM agentops_control.jobs j
         JOIN agentops_control.job_attempts a ON a.job_id = j.id
        WHERE j.id = $1
        ORDER BY a.attempt_number DESC
        LIMIT 1`,
      [first!.job.id],
    );
    expect(persisted.rows[0]).toMatchObject({
      job_status: 'rejected',
      job_failure: failure,
      attempt_status: 'failed',
      attempt_failure: failure,
    });
  });

  it('runs a safe existing-AgentOps-shaped path through all fences and stores only artifact metadata', async () => {
    const store = await migratedStore();
    const repo = await registration(store, '-service');
    const queued = await enqueueRunner(store, repo.id, repo.version, '-service');
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agentops-runner-service-'));
    const commandRunner: WorkspaceCommandRunner = (_command, args) => {
      if (args[0] === 'clone') fs.mkdirSync(String(args.at(-1)), { recursive: true });
      if (args.includes('add')) {
        const index = args.indexOf('add');
        fs.mkdirSync(String(args[index + 4]), { recursive: true });
      }
      if (args.includes('rev-parse')) {
        return { status: 0, stdout: `${'a'.repeat(40)}\n`, stderr: '' };
      }
      if (args.includes('get-url')) {
        return {
          status: 0,
          stdout: 'https://github.com/mrbaron3/control-store-service.git\n',
          stderr: '',
        };
      }
      return { status: 0, stdout: '', stderr: '' };
    };
    const adapter: AgentOpsRunnerAdapter = {
      async execute(input) {
        await input.fence.arm('push');
        input.fence.consume('push');
        await input.fence.arm('merge');
        input.fence.consume('merge');
        await input.fence.arm('release');
        input.fence.consume('release');
        return {
          outcome: 'completed',
          humanReview: null,
          headSha: 'b'.repeat(40),
          pullRequestNumber: 38,
          developmentTurn: {
            intake: [],
            enrichmentIds: [],
            driveResults: [],
          },
        };
      },
    };
    const service = new IsolatedRunnerService({
      operatingMode: 'ACTIVE',
      workerId: 'runner-service',
      workspaceRoot: root,
      provider: 'codex',
      leaseDurationMs: 10_000,
      heartbeatIntervalMs: 1_000,
      reconciliationIntervalMs: 250,
      maxAttempts: 2,
      retryBaseMs: 0,
      attemptTimeoutMs: 60_000,
    }, {
      store,
      workspace: new RunnerWorkspaceManager(root, {}, commandRunner),
      adapter,
    });
    await expect(service.runOnce()).resolves.toBe(true);
    const outcome = await pool.query<{
      status: string;
      result: Record<string, unknown>;
      payload: Record<string, unknown>;
      artifact_count: string;
      artifact_uri: string;
    }>(
      `SELECT j.status, j.result, j.payload,
              count(a.id)::text AS artifact_count,
              min(a.uri) AS artifact_uri
         FROM agentops_control.jobs j
         LEFT JOIN agentops_control.artifact_links a ON a.job_id = j.id
        WHERE j.id = $1
        GROUP BY j.id`,
      [queued.job.id],
    );
    expect(outcome.rows[0]).toMatchObject({
      status: 'succeeded',
      result: {
        schemaVersion: 1,
        status: 'succeeded',
        outcome: 'completed',
        jobId: queued.job.id,
        headSha: 'b'.repeat(40),
        pullRequestNumber: 38,
      },
      artifact_count: '1',
    });
    expect(outcome.rows[0]?.artifact_uri).toMatch(
      new RegExp(`^volume://registrations/${repo.id}/`),
    );
    expect(JSON.stringify(outcome.rows[0]?.payload)).not.toContain('runner-result');
    expect(await fs.promises.readFile(
      path.join(
        root,
        'registrations',
        repo.id,
        'jobs',
        queued.job.id,
        'attempt-1',
        'artifacts',
        'runner-result.json',
      ),
      'utf8',
    )).toContain('"developmentTurn"');
  });

  it('leaves durable final-suite evidence for every stale critical boundary', async () => {
    const store = await migratedStore();
    for (const boundary of ['claim', 'provider', 'push', 'merge', 'release'] as const) {
      const suffix = `-final-${boundary}`;
      const repo = await registration(store, suffix);
      await enqueueRunner(store, repo.id, repo.version, suffix);
      if (boundary === 'claim') {
        await store.updateRegistration(repo.id, { enabled: false });
      } else {
        const lease = await store.acquireLease({
          workerId: `runner-final-${boundary}`,
          durationMs: 60_000,
        });
        await store.updateRegistration(repo.id, { enabled: false });
        await store.assertExecutionGuard({
          token: lease!.token,
          workerId: `runner-final-${boundary}`,
          boundary,
        });
      }
    }
    const allowedRepo = await registration(store, '-final-allowed');
    await enqueueRunner(
      store,
      allowedRepo.id,
      allowedRepo.version,
      '-final-allowed',
    );
    const allowedLease = await store.acquireLease({
      workerId: 'runner-final-allowed',
      durationMs: 60_000,
    });
    for (const boundary of ['provider', 'push', 'merge', 'release'] as const) {
      await expect(store.assertExecutionGuard({
        token: allowedLease!.token,
        workerId: 'runner-final-allowed',
        boundary,
      })).resolves.toMatchObject({ ok: true, reason: null });
    }
    const durable = await pool.query<{ boundary: string; reason: string }>(
      `SELECT split_part(event_type, '.', 3) AS boundary,
              details->>'reason' AS reason
         FROM agentops_control.runtime_audit
        WHERE event_type LIKE 'runner.boundary.%.denied'
        ORDER BY boundary`,
    );
    expect(durable.rows).toEqual([
      { boundary: 'claim', reason: 'registration_disabled' },
      { boundary: 'merge', reason: 'registration_disabled' },
      { boundary: 'provider', reason: 'registration_disabled' },
      { boundary: 'push', reason: 'registration_disabled' },
      { boundary: 'release', reason: 'registration_disabled' },
    ]);
  });
});
