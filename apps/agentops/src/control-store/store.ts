import { createHash, randomUUID } from 'node:crypto';
import {
  Pool,
  type Notification,
  type PoolClient,
  type PoolConfig,
  type QueryResultRow,
} from 'pg';
import { z } from 'zod';
import { assertControlSchema, migrateControlSchema } from './migrations.js';
import {
  DevelopmentProgressUpdate,
  type DevelopmentProgressUpdate as DevelopmentProgressUpdateType,
} from '../domain/development-progress.js';
import {
  DevelopmentReviewFinding,
  DevelopmentReviewRound,
  type DevelopmentReviewRound as DevelopmentReviewRoundType,
} from '../domain/development-review.js';
import {
  DurableReleaseReceiptContract,
  HistoricalDurableReleaseReceiptContract,
  HistoricalReleasePolicyContract,
  LiveReleaseReceiptEvidenceContract,
  PersistedLiveReleaseReceiptEvidenceContract,
  ReleaseArtifactContract,
  ReleaseGradeReceiptContract,
  ReleaseMergeIntentReceiptContract,
  ReleaseMergeReceiptContract,
  ReleasePolicyContract,
  assertLiveReleaseReceiptEvidence,
  legacyLiveReleaseReceiptEvidenceWire,
  releasePreMergeSemanticErrors,
  type DurableReleaseReceipt,
  type HistoricalDurableReleaseReceipt,
  type PersistedLiveReleaseReceiptEvidence,
  type ReleaseArtifact,
  type ReleaseMergeIntentReceipt,
  type ReleaseMergeReceipt,
  type ReleasePolicy,
} from '../evidence/release-receipt.js';
import {
  CanonicalRepository,
  EnqueueJobInput,
  GitHubLabelNameContract,
  HistoricalRepositoryRegistrationConfigurationContract,
  IdempotencyConflictError,
  LeaseRejectedError,
  MonitorBrokerCursor,
  MonitorBrokerResponse,
  OperatingModeError,
  RepositoryBusyError,
  RepositoryRegistrationInput,
  RepositoryRegistrationPatch,
  ReleaseSourceIssueSnapshotContract,
  ReleaseCertificationError,
  ReleaseReceiptConflictError,
  RunnerCriticalBoundary,
  RunnerJobPayloadV1Contract,
  RunnerJobFailureV1Contract,
  RunnerJobResultV1Contract,
  StaleRegistrationError,
  TriageDecisionV1Contract,
  TriageJobResultV1Contract,
  TriageProviderProvenanceContract,
  type EnqueueResult,
  type ExecutionGuardVerdict,
  type JobEnvelope,
  type Lease,
  type MonitorBrokerRequest,
  type MonitorBrokerResponse as MonitorBrokerResponseType,
  type ReconciliationWork,
  type ReleaseRecord,
  type ReleaseSourceIssueSnapshot,
  type ReleaseReceiptOutboxEntry,
  type RepositoryRegistration,
  type RunnerJobFailureV1,
  type RunnerJobResultV1,
  type TriageDecisionV1,
  type TriageJobResultV1,
  type WebhookClaim,
} from './types.js';

interface RegistrationRow extends QueryResultRow {
  id: string;
  repository: string;
  enabled: boolean;
  issue_monitor_enabled: boolean;
  pr_monitor_enabled: boolean;
  execution_enabled: boolean;
  configuration: Record<string, unknown>;
  version: string;
  created_at: Date;
  updated_at: Date;
}

interface JobRow extends QueryResultRow {
  id: string;
  registration_id: string;
  registration_version: string;
  contract_version: number;
  source_kind: JobEnvelope['source']['kind'];
  source_key: string;
  idempotency_key: string;
  job_type: string;
  payload: Record<string, unknown>;
  status: JobEnvelope['status'];
  last_error: string | null;
  created_at: Date;
  release_id: string | null;
}

interface ReleaseRow extends QueryResultRow {
  id: string;
  registration_id: string;
  release_key: string;
  repository: string;
  issue_number: string;
  policy: ReleasePolicy;
  status: ReleaseRecord['status'];
  pull_request_number: string | null;
  final_head: string | null;
  merge_sha: string | null;
  merge_actor: string | null;
  created_at: Date;
  updated_at: Date;
  completed_at: Date | null;
}

interface ReleaseReceiptRow extends QueryResultRow {
  payload: DurableReleaseReceipt;
  published_at: Date | null;
}

interface ReleaseArtifactRow extends QueryResultRow {
  artifact_key: string;
  kind: string;
  uri: string;
  sha256: string;
  size_bytes: string;
  release_id: string;
  source_head: string;
  receipt_ids: string[];
}

interface WebhookDeliveryRow extends QueryResultRow {
  id: string;
  delivery_key: string;
  repository: string;
  event: string;
  action: string | null;
  headers: Record<string, string>;
  payload: Record<string, unknown>;
  received_at: Date;
}

interface MonitorBrokerRequestRow extends QueryResultRow {
  id: string;
  registration_id: string;
  registration_version: string;
  repository: string;
  monitor_kind: 'issue' | 'pull_request';
  cursor: { updatedAfter: string };
  lease_token: string;
}

function persistedRegistrationConfiguration(value: unknown) {
  return HistoricalRepositoryRegistrationConfigurationContract.parse(value);
}

function registration(row: RegistrationRow): RepositoryRegistration {
  return {
    id: row.id,
    repository: row.repository,
    enabled: row.enabled,
    issueMonitorEnabled: row.issue_monitor_enabled,
    prMonitorEnabled: row.pr_monitor_enabled,
    executionEnabled: row.execution_enabled,
    configuration: persistedRegistrationConfiguration(row.configuration),
    version: Number(row.version),
    createdAt: row.created_at.toISOString(),
    updatedAt: row.updated_at.toISOString(),
  };
}

function job(row: JobRow): JobEnvelope {
  return {
    contractVersion: row.contract_version as 1,
    id: row.id,
    registrationId: row.registration_id,
    registrationVersion: Number(row.registration_version),
    source: { kind: row.source_kind, key: row.source_key },
    idempotencyKey: row.idempotency_key,
    jobType: row.job_type,
    payload: row.payload,
    status: row.status,
    createdAt: row.created_at.toISOString(),
    ...(row.release_id ? { releaseId: row.release_id } : {}),
  };
}

function releaseRecordBase(row: ReleaseRow): Omit<ReleaseRecord, 'policy'> {
  return {
    id: row.id,
    registrationId: row.registration_id,
    releaseKey: row.release_key,
    repository: row.repository,
    issueNumber: Number(row.issue_number),
    status: row.status,
    pullRequest: row.pull_request_number === null
      ? null
      : Number(row.pull_request_number),
    finalHead: row.final_head,
    mergeSha: row.merge_sha,
    mergeActor: row.merge_actor,
    createdAt: row.created_at.toISOString(),
    updatedAt: row.updated_at.toISOString(),
    completedAt: row.completed_at?.toISOString() ?? null,
  };
}

function releaseRecord(row: ReleaseRow): ReleaseRecord {
  return {
    ...releaseRecordBase(row),
    // Persisted v2/v3 policies are immutable history. Every writer remains on
    // ReleasePolicyContract, while every row reader accepts the historical
    // repository-grader namespace so upgrades cannot strand active recovery.
    policy: HistoricalReleasePolicyContract.parse(row.policy),
  };
}

function advisoryRequestKey(scope: string, key: string): string {
  return `${scope.length}:${scope}:${key.length}:${key}`;
}

const DURABLE_WEBHOOK_HEADER_ALLOWLIST = new Set([
  'content-type',
  'user-agent',
  'x-github-delivery',
  'x-github-event',
  'x-github-hook-id',
  'x-github-hook-installation-target-id',
  'x-github-hook-installation-target-type',
]);

export function durableControlWebhookHeaders(
  headers: Record<string, string>,
): Record<string, string> {
  return Object.fromEntries(
    Object.entries(headers).filter(([name]) =>
      DURABLE_WEBHOOK_HEADER_ALLOWLIST.has(name.toLowerCase())),
  );
}

async function transaction<T>(
  pool: Pool,
  run: (client: PoolClient) => Promise<T>,
): Promise<T> {
  const client = await pool.connect();
  try {
    await client.query('BEGIN');
    const result = await run(client);
    await client.query('COMMIT');
    return result;
  } catch (error) {
    try {
      await client.query('ROLLBACK');
    } catch {
      // Preserve the transaction's original failure.
    }
    throw error;
  } finally {
    client.release();
  }
}

function receiptHead(receipt: DurableReleaseReceipt): string | null {
  switch (receipt.kind) {
    case 'build':
    case 'grade':
    case 'review':
      return receipt.head;
    case 'finding-resolution':
      return receipt.resolvedOnHead;
    case 'merge-intent':
    case 'merge':
      return receipt.expectedHead;
    default:
      return null;
  }
}

async function insertReleaseReceipt(
  client: PoolClient,
  receipt: DurableReleaseReceipt,
): Promise<ReleaseReceiptOutboxEntry> {
  try {
    const result = await client.query<ReleaseReceiptRow>(
      `INSERT INTO agentops_control.release_receipt_outbox(
         receipt_id, release_id, receipt_key, kind, repository,
         issue_number, head_sha, causes, payload, recorded_at
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
       ON CONFLICT (release_id, receipt_key) DO UPDATE
         SET receipt_key = EXCLUDED.receipt_key
       WHERE agentops_control.release_receipt_outbox.receipt_id = EXCLUDED.receipt_id
         AND agentops_control.release_receipt_outbox.payload = EXCLUDED.payload
       RETURNING payload, published_at`,
      [
        receipt.receiptId,
        receipt.releaseId,
        receipt.receiptKey,
        receipt.kind,
        receipt.repository,
        receipt.issueNumber,
        receiptHead(receipt),
        receipt.causes,
        receipt,
        receipt.recordedAt,
      ],
    );
    const row = result.rows[0];
    if (!row) {
      throw new ReleaseReceiptConflictError(
        `receipt key ${receipt.receiptKey} was reused with different evidence`,
      );
    }
    return {
      receipt: DurableReleaseReceiptContract.parse(row.payload),
      publishedAt: row.published_at?.toISOString() ?? null,
    };
  } catch (error) {
    if (
      error instanceof ReleaseReceiptConflictError
      || !(error && typeof error === 'object' && 'code' in error)
      || error.code !== '23505'
    ) {
      throw error;
    }
    throw new ReleaseReceiptConflictError(
      `receipt ${receipt.receiptId}/${receipt.receiptKey} conflicts with durable evidence`,
    );
  }
}

async function releaseReceipts(
  client: PoolClient,
  releaseId: string,
  includeMergeIntent: boolean,
): Promise<ReleaseReceiptOutboxEntry[]> {
  const result = await client.query<ReleaseReceiptRow>(
    `SELECT payload, published_at
       FROM agentops_control.release_receipt_outbox
      WHERE release_id = $1
        AND ($2::boolean OR kind <> 'merge-intent')
      ORDER BY recorded_at, receipt_id`,
    [releaseId, includeMergeIntent],
  );
  return result.rows.map((row) => ({
    // Historical grade aliases are accepted only while decoding durable rows.
    // recordReleaseReceipt below continues to enforce the canonical contract.
    receipt: HistoricalDurableReleaseReceiptContract.parse(row.payload),
    publishedAt: row.published_at?.toISOString() ?? null,
  }));
}

async function observeReleaseHead(
  client: PoolClient,
  input: { releaseId: string; head: string; parentHead: string | null },
): Promise<number> {
  const existing = await client.query<{
    head_epoch: number;
    parent_head: string | null;
  }>(
    `SELECT head_epoch, parent_head
       FROM agentops_control.release_heads
      WHERE release_id = $1 AND head_sha = $2`,
    [input.releaseId, input.head],
  );
  if (existing.rows[0]) {
    if (existing.rows[0].parent_head !== input.parentHead) {
      throw new ReleaseReceiptConflictError(
        `release head ${input.head} was reused with a different parent`,
      );
    }
    return existing.rows[0].head_epoch;
  }
  const next = await client.query<{ head_epoch: number }>(
    `INSERT INTO agentops_control.release_heads(
       release_id, head_sha, head_epoch, parent_head
     )
     SELECT $1, $2, COALESCE(max(head_epoch), 0) + 1, $3
       FROM agentops_control.release_heads
      WHERE release_id = $1
     RETURNING head_epoch`,
    [input.releaseId, input.head, input.parentHead],
  );
  return next.rows[0]!.head_epoch;
}

export interface OpenControlStoreOptions {
  migrate?: boolean;
  migrationRoot?: string;
}

/**
 * Transactional TypeScript adapter for the language-neutral SQL contract.
 * No method falls back to JSON or in-memory durable state.
 */
export class PostgresControlStore {
  readonly pool: Pool;

  constructor(config: PoolConfig | Pool) {
    this.pool = config instanceof Pool ? config : new Pool(config);
  }

  static async open(
    config: PoolConfig,
    options: OpenControlStoreOptions = {},
  ): Promise<PostgresControlStore> {
    const store = new PostgresControlStore(config);
    try {
      if (options.migrate) {
        await migrateControlSchema(store.pool, { root: options.migrationRoot });
      } else {
        await assertControlSchema(store.pool, { root: options.migrationRoot });
      }
      return store;
    } catch (error) {
      await store.close();
      throw error;
    }
  }

  async close(): Promise<void> {
    await this.pool.end();
  }

  async createRegistration(input: unknown): Promise<RepositoryRegistration> {
    const parsed = RepositoryRegistrationInput.parse(input);
    const result = await this.pool.query<RegistrationRow>(
      `INSERT INTO agentops_control.repository_registrations(
         id, repository, enabled, issue_monitor_enabled, pr_monitor_enabled,
         execution_enabled, configuration
       ) VALUES ($1, $2, $3, $4, $5, $6, $7)
       RETURNING *`,
      [
        randomUUID(),
        parsed.repository,
        parsed.enabled,
        parsed.issueMonitorEnabled,
        parsed.prMonitorEnabled,
        parsed.executionEnabled,
        parsed.configuration,
      ],
    );
    return registration(result.rows[0]!);
  }

  async updateRegistration(
    id: string,
    patch: Partial<Omit<RepositoryRegistrationInput, 'repository'>>,
  ): Promise<RepositoryRegistration> {
    const parsedPatch = RepositoryRegistrationPatch.parse(patch);
    const allowed = [
      ['enabled', 'enabled'],
      ['issueMonitorEnabled', 'issue_monitor_enabled'],
      ['prMonitorEnabled', 'pr_monitor_enabled'],
      ['executionEnabled', 'execution_enabled'],
      ['configuration', 'configuration'],
    ] as const;
    const changes = allowed.filter(([property]) => parsedPatch[property] !== undefined);
    if (changes.length === 0) throw new Error('registration patch is empty');
    const values = changes.map(([property]) => parsedPatch[property]);
    const assignments = changes.map(([, column], index) => `${column} = $${index + 2}`);
    return transaction(this.pool, async (client) => {
      const locked = await client.query<{ id: string }>(
        `SELECT id FROM agentops_control.repository_registrations
          WHERE id = $1 FOR UPDATE`,
        [id],
      );
      if (!locked.rows[0]) throw new Error(`no such repository registration: ${id}`);
      const result = await client.query<RegistrationRow>(
        `UPDATE agentops_control.repository_registrations
            SET ${assignments.join(', ')}, version = version + 1,
                updated_at = clock_timestamp()
          WHERE id = $1
          RETURNING *`,
        [id, ...values],
      );
      await client.query(
        `UPDATE agentops_control.jobs
            SET status = 'rejected', finished_at = clock_timestamp(),
                updated_at = clock_timestamp(),
                last_error = 'registration changed before lease acquisition'
          WHERE registration_id = $1 AND status = 'queued'`,
        [id],
      );
      return registration(result.rows[0]!);
    });
  }

  async getRegistration(id: string): Promise<RepositoryRegistration | null> {
    const result = await this.pool.query<RegistrationRow>(
      'SELECT * FROM agentops_control.repository_registrations WHERE id = $1',
      [id],
    );
    return result.rows[0] ? registration(result.rows[0]) : null;
  }

  async listRegistrations(): Promise<RepositoryRegistration[]> {
    const result = await this.pool.query<RegistrationRow>(
      'SELECT * FROM agentops_control.repository_registrations ORDER BY repository',
    );
    return result.rows.map(registration);
  }

  /**
   * Allocate one durable identity for all jobs and retries serving a release.
   * Reusing the key with identical coordinates is idempotent; changing policy
   * or target under the same key fails closed.
   */
  async createRelease(input: {
    registrationId: string;
    registrationVersion: number;
    releaseKey: string;
    repository: string;
    issueNumber: number;
    policy: ReleasePolicy;
  }): Promise<{ release: ReleaseRecord; duplicate: boolean }> {
    const repository = CanonicalRepository.parse(input.repository);
    const policy = ReleasePolicyContract.parse(input.policy);
    if (!/^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$/.test(input.releaseKey)) {
      throw new Error('releaseKey is invalid');
    }
    if (
      !Number.isInteger(input.issueNumber)
      || input.issueNumber < 1
      || input.issueNumber > 2_147_483_647
    ) {
      throw new Error('release issueNumber is invalid');
    }
    return transaction(this.pool, async (client) => {
      const registrationResult = await client.query<RegistrationRow>(
        `SELECT * FROM agentops_control.repository_registrations
          WHERE id = $1 FOR SHARE`,
        [input.registrationId],
      );
      const current = registrationResult.rows[0];
      if (
        !current
        || !current.enabled
        || !current.execution_enabled
        || Number(current.version) !== input.registrationVersion
        || current.repository !== repository
      ) {
        throw new StaleRegistrationError(
          `release does not match live Registration ${input.registrationId}`,
        );
      }
      await client.query(
        'SELECT pg_advisory_xact_lock(hashtextextended($1, 0))',
        [advisoryRequestKey(input.registrationId, `release:${input.releaseKey}`)],
      );
      const existing = await client.query<ReleaseRow>(
        `SELECT * FROM agentops_control.releases
          WHERE registration_id = $1 AND release_key = $2`,
        [input.registrationId, input.releaseKey],
      );
      if (existing.rows[0]) {
        const found = releaseRecord(existing.rows[0]);
        if (
          found.repository !== repository
          || found.issueNumber !== input.issueNumber
          || JSON.stringify(found.policy) !== JSON.stringify(policy)
        ) {
          throw new ReleaseReceiptConflictError(
            `release key ${input.releaseKey} was reused with different coordinates or policy`,
          );
        }
        return { release: found, duplicate: true };
      }
      try {
        const inserted = await client.query<ReleaseRow>(
          `INSERT INTO agentops_control.releases(
             id, registration_id, release_key, repository, issue_number, policy
           ) VALUES ($1, $2, $3, $4, $5, $6)
           RETURNING *`,
          [
            randomUUID(),
            input.registrationId,
            input.releaseKey,
            repository,
            input.issueNumber,
            policy,
          ],
        );
        return { release: releaseRecord(inserted.rows[0]!), duplicate: false };
      } catch (error) {
        if (
          error && typeof error === 'object' && 'code' in error
          && error.code === '23505'
        ) {
          throw new ReleaseReceiptConflictError(
            `${repository}#${input.issueNumber} already has an open release`,
          );
        }
        throw error;
      }
    });
  }

  async getRelease(id: string): Promise<ReleaseRecord | null> {
    const parsedId = z.string().uuid().parse(id);
    const result = await this.pool.query<ReleaseRow>(
      'SELECT * FROM agentops_control.releases WHERE id = $1',
      [parsedId],
    );
    return result.rows[0] ? releaseRecord(result.rows[0]) : null;
  }

  /** Resolve the immutable ready-time Source Issue attached to this release's promoted job. */
  async getReleaseSourceIssue(
    releaseId: string,
  ): Promise<ReleaseSourceIssueSnapshot | null> {
    const parsedId = z.string().uuid().parse(releaseId);
    const result = await this.pool.query<{ payload: unknown }>(
      `SELECT payload
         FROM agentops_control.jobs
        WHERE release_id = $1
          AND job_type = 'agentops.runner'
          AND payload ? 'sourceIssue'
        ORDER BY created_at, id`,
      [parsedId],
    );
    const snapshots = result.rows.map((row) => {
      const payload = RunnerJobPayloadV1Contract.parse(row.payload);
      if (!payload.sourceIssue) {
        throw new ReleaseReceiptConflictError(
          `release ${parsedId} has a malformed Source Issue binding`,
        );
      }
      return payload.sourceIssue;
    });
    if (snapshots.length === 0) return null;
    if (snapshots.some((snapshot) => snapshot.digest !== snapshots[0]!.digest)) {
      throw new ReleaseReceiptConflictError(
        `release ${parsedId} has conflicting Source Issue snapshots`,
      );
    }
    return snapshots[0]!;
  }

  /** Resolve a v2 recovery job back to its durable release identity. */
  async findReleaseForRunnerEvent(input: {
    registrationId: string;
    issueNumber?: number;
    pullRequest?: number;
    includeMerged?: boolean;
  }): Promise<ReleaseRecord | null> {
    const coordinates = z.object({
      registrationId: z.string().uuid(),
      issueNumber: z.number().int().positive().optional(),
      pullRequest: z.number().int().positive().optional(),
      includeMerged: z.boolean().optional(),
    }).strict().refine(
      (value) => value.issueNumber !== undefined || value.pullRequest !== undefined,
      'release recovery needs an issue or pull request coordinate',
    ).parse(input);
    const result = await this.pool.query<ReleaseRow>(
      `SELECT * FROM agentops_control.releases
        WHERE registration_id = $1
          AND (
            status IN ('collecting', 'merge-authorized')
            OR ($4::boolean AND status = 'merged')
          )
          AND ($2::bigint IS NULL OR issue_number = $2)
          AND ($3::bigint IS NULL OR pull_request_number = $3)
        ORDER BY created_at DESC
        LIMIT 2`,
      [
        coordinates.registrationId,
        coordinates.issueNumber ?? null,
        coordinates.pullRequest ?? null,
        coordinates.includeMerged ?? false,
      ],
    );
    if (result.rows.length > 1) {
      throw new ReleaseReceiptConflictError(
        'runner event matches more than one release identity',
      );
    }
    return result.rows[0] ? releaseRecord(result.rows[0]) : null;
  }

  /** Link an independently scheduled job to the stable release identity. */
  async linkJobToRelease(input: {
    jobId: string;
    releaseId: string;
  }): Promise<void> {
    await transaction(this.pool, async (client) => {
      const linked = await client.query<{ id: string }>(
        `UPDATE agentops_control.jobs job
            SET release_id = release.id, updated_at = clock_timestamp()
           FROM agentops_control.releases release
          WHERE job.id = $1
            AND release.id = $2
            AND job.registration_id = release.registration_id
            AND (job.release_id IS NULL OR job.release_id = release.id)
          RETURNING job.id`,
        [input.jobId, input.releaseId],
      );
      if (!linked.rows[0]) {
        throw new ReleaseReceiptConflictError(
          'job and release must share one Registration and stable identity',
        );
      }
    });
  }

  /** Persist the stable GitHub PR coordinate before review or recovery splits jobs. */
  async bindReleasePullRequest(input: {
    jobId: string;
    releaseId: string;
    pullRequestNumber: number;
  }): Promise<void> {
    const parsed = z.object({
      jobId: z.string().uuid(),
      releaseId: z.string().uuid(),
      pullRequestNumber: z.number().int().positive().max(2_147_483_647),
    }).strict().parse(input);
    const result = await this.pool.query<{ pull_request_number: string }>(
      `SELECT agentops_control.bind_release_pull_request($1, $2, $3)
         AS pull_request_number`,
      [parsed.jobId, parsed.releaseId, parsed.pullRequestNumber],
    );
    if (Number(result.rows[0]?.pull_request_number) !== parsed.pullRequestNumber) {
      throw new ReleaseReceiptConflictError(
        'release pull request binding did not converge',
      );
    }
  }

  /** Atomically bind the single PR preserved when v17 abandoned a legacy release. */
  async recoverRequirementsUpgradePullRequest(input: {
    jobId: string;
    releaseId: string;
  }): Promise<number | null> {
    const parsed = z.object({
      jobId: z.string().uuid(),
      releaseId: z.string().uuid(),
    }).strict().parse(input);
    const result = await this.pool.query<{ pull_request_number: string | null }>(
      `SELECT agentops_control.recover_requirements_upgrade_pull_request($1, $2)
         AS pull_request_number`,
      [parsed.jobId, parsed.releaseId],
    );
    const pullRequest = result.rows[0]?.pull_request_number;
    return pullRequest === null || pullRequest === undefined
      ? null
      : Number(pullRequest);
  }

  /** Allocate a stable release-scoped epoch for a head, independent of job/retry order. */
  async observeReleaseHead(input: {
    releaseId: string;
    head: string;
    parentHead: string | null;
  }): Promise<number> {
    const parsed = z.object({
      releaseId: z.string().uuid(),
      head: z.string().regex(/^[0-9a-f]{40}$/),
      parentHead: z.string().regex(/^[0-9a-f]{40}$/).nullable(),
    }).strict().parse(input);
    return transaction(this.pool, async (client) => {
      const existing = await client.query<{
        head_epoch: number;
        parent_head: string | null;
      }>(
        `SELECT head_epoch, parent_head
           FROM agentops_control.release_heads
          WHERE release_id = $1 AND head_sha = $2`,
        [parsed.releaseId, parsed.head],
      );
      if (existing.rows[0]) {
        if (existing.rows[0].parent_head !== parsed.parentHead) {
          throw new ReleaseReceiptConflictError(
            'release head was reused with a different parent',
          );
        }
        return Number(existing.rows[0].head_epoch);
      }
      const result = await client.query<{ head_epoch: number }>(
        `SELECT agentops_control.observe_release_head($1, $2, $3) AS head_epoch`,
        [parsed.releaseId, parsed.head, parsed.parentHead],
      );
      if (!result.rows[0]) {
        throw new ReleaseReceiptConflictError(
          'release head observation did not converge',
        );
      }
      return Number(result.rows[0].head_epoch);
    });
  }

  /** Persist an immutable semantic receipt into the transactional outbox. */
  async recordReleaseReceipt(input: unknown): Promise<ReleaseReceiptOutboxEntry> {
    const receipt = DurableReleaseReceiptContract.parse(input);
    if (receipt.kind === 'merge-intent' || receipt.kind === 'merge') {
      throw new Error('merge receipts must use the release transition methods');
    }
    const result = await this.pool.query<{ payload: unknown }>(
      `SELECT agentops_control.record_release_receipt($1) AS payload`,
      [receipt],
    );
    return {
      receipt: DurableReleaseReceiptContract.parse(result.rows[0]?.payload),
      publishedAt: null,
    };
  }

  async listReleaseReceipts(
    releaseId: string,
    options: { includeMergeIntent?: boolean } = {},
  ): Promise<ReleaseReceiptOutboxEntry[]> {
    return transaction(this.pool, (client) => releaseReceipts(
      client,
      z.string().uuid().parse(releaseId),
      options.includeMergeIntent ?? false,
    ));
  }

  /** Bind an artifact to release/head/receipt identities without copying bytes into PostgreSQL. */
  async recordReleaseArtifact(input: {
    artifactKey: string;
    artifact: ReleaseArtifact;
  }): Promise<ReleaseArtifact> {
    if (!/^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$/.test(input.artifactKey)) {
      throw new Error('release artifactKey is invalid');
    }
    const artifact = ReleaseArtifactContract.parse(input.artifact);
    const result = await this.pool.query<{ artifact: unknown }>(
      `SELECT agentops_control.record_release_artifact($1, $2) AS artifact`,
      [input.artifactKey, artifact],
    );
    return ReleaseArtifactContract.parse(result.rows[0]?.artifact);
  }

  /** Assemble and independently certify one completed release from durable records only. */
  async exportReleaseEvidence(
    releaseId: string,
  ): Promise<PersistedLiveReleaseReceiptEvidence> {
    const parsedId = z.string().uuid().parse(releaseId);
    return transaction(this.pool, async (client) => {
      await client.query('SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY');
      const selected = await client.query<ReleaseRow>(
        'SELECT * FROM agentops_control.releases WHERE id = $1',
        [parsedId],
      );
      const row = selected.rows[0];
      if (!row) throw new Error(`no such release: ${parsedId}`);
      const release = releaseRecord(row);
      if (
        release.status !== 'merged'
        || release.pullRequest === null
        || release.finalHead === null
        || release.mergeSha === null
        || release.completedAt === null
      ) {
        throw new ReleaseCertificationError('only a completed release can be exported');
      }
      const entries = await releaseReceipts(client, release.id, true);
      const receipts = entries.map((entry) => entry.receipt);
      const one = <K extends HistoricalDurableReleaseReceipt['kind']>(kind: K) =>
        receipts.find((receipt) => receipt.kind === kind);
      const requirementsAuthority = one('requirements-authority');
      const canonicalV4 = requirementsAuthority !== undefined
        && ReleasePolicyContract.safeParse(release.policy).success
        && receipts.filter((receipt) => receipt.kind === 'grade')
          .every((receipt) => ReleaseGradeReceiptContract.safeParse(receipt).success);
      const artifacts = await client.query<ReleaseArtifactRow>(
        `SELECT * FROM agentops_control.release_artifacts
          WHERE release_id = $1 ORDER BY recorded_at, id`,
        [release.id],
      );
      const evidence = LiveReleaseReceiptEvidenceContract.parse({
        // Merged releases from before schema 17 remain historical v2 evidence.
        // Historical v3 policies/grade aliases stay on their immutable wire;
        // every canonical release with frozen requirements exports v4.
        schemaVersion: canonicalV4 ? '4.0' : requirementsAuthority ? '3.0' : '2.0',
        release: {
          id: release.id,
          repository: release.repository,
          issueNumber: release.issueNumber,
          pullRequestNumber: release.pullRequest,
          finalHead: release.finalHead,
          mergeSha: release.mergeSha,
          createdAt: release.createdAt,
          completedAt: release.completedAt,
        },
        policy: release.policy,
        receipts: {
          authority: one('authority'),
          ...(requirementsAuthority ? { requirementsAuthority } : {}),
          runtime: receipts.filter((receipt) => receipt.kind === 'runtime-provenance'),
          builds: receipts.filter((receipt) => receipt.kind === 'build'),
          grades: receipts.filter((receipt) => receipt.kind === 'grade'),
          reviews: receipts.filter((receipt) => receipt.kind === 'review'),
          findingResolutions: receipts.filter(
            (receipt) => receipt.kind === 'finding-resolution',
          ),
          mergeIntent: one('merge-intent'),
          merge: one('merge'),
          interventions: receipts.filter((receipt) => receipt.kind === 'intervention'),
        },
        artifacts: artifacts.rows.map((artifact) => ({
          kind: artifact.kind,
          uri: artifact.uri,
          sha256: artifact.sha256,
          sizeBytes: Number(artifact.size_bytes),
          releaseId: artifact.release_id,
          sourceHead: artifact.source_head,
          receiptIds: artifact.receipt_ids,
        })),
        result: receipts.some((receipt) => receipt.kind === 'intervention')
          ? 'passed-with-interventions'
          : 'passed',
      });
      assertLiveReleaseReceiptEvidence(evidence);
      return canonicalV4
        ? PersistedLiveReleaseReceiptEvidenceContract.parse(evidence)
        : PersistedLiveReleaseReceiptEvidenceContract.parse(
          legacyLiveReleaseReceiptEvidenceWire(
            evidence,
            requirementsAuthority ? '3.0' : '2.0',
          ),
        );
    });
  }

  /**
   * Fail-closed pre-merge gate. The intent receipt and release state commit in
   * one transaction before the caller may invoke GitHub's merge mutation.
   */
  async authorizeReleaseMerge(input: {
    releaseId: string;
    intent: ReleaseMergeIntentReceipt;
  }): Promise<ReleaseRecord> {
    const intent = ReleaseMergeIntentReceiptContract.parse(input.intent);
    if (intent.releaseId !== input.releaseId) {
      throw new ReleaseReceiptConflictError('merge intent releaseId mismatch');
    }
    return transaction(this.pool, async (client) => {
      const selected = await client.query<ReleaseRow>(
        `SELECT * FROM agentops_control.releases WHERE id = $1`,
        [input.releaseId],
      );
      const row = selected.rows[0];
      if (!row) throw new Error(`no such release: ${input.releaseId}`);
      const release = releaseRecord(row);
      if (
        release.repository !== intent.repository
        || release.issueNumber !== intent.issueNumber
        || intent.expectedHead !== intent.observedPrHead
      ) {
        throw new ReleaseReceiptConflictError(
          'merge intent does not match release coordinates and current head',
        );
      }
      if (Date.parse(intent.recordedAt) < Date.parse(release.createdAt)) {
        throw new ReleaseReceiptConflictError(
          'merge intent recordedAt cannot precede release creation',
        );
      }
      if (release.status === 'merged') {
        throw new ReleaseReceiptConflictError('merged release cannot be re-authorized');
      }
      const mixedArtifacts = await client.query<{ present: boolean }>(
        `SELECT EXISTS(
           SELECT 1 FROM agentops_control.release_artifacts
            WHERE release_id = $1 AND source_head <> $2
         ) AS present`,
        [release.id, intent.expectedHead],
      );
      if (mixedArtifacts.rows[0]?.present === true) {
        throw new ReleaseCertificationError(
          'release artifacts contain a source head other than expectedHead',
        );
      }
      const receipts = await releaseReceipts(client, input.releaseId, false);
      const errors = releasePreMergeSemanticErrors({
        releaseId: release.id,
        repository: release.repository,
        issueNumber: release.issueNumber,
        pullRequestNumber: intent.pullRequestNumber,
        expectedHead: intent.expectedHead,
        policy: release.policy,
        receipts: receipts.map((entry) => entry.receipt),
      });
      if (errors.length > 0) {
        throw new ReleaseCertificationError(
          `release merge authorization failed: ${errors.join('; ')}`,
        );
      }
      await client.query(
        `SELECT agentops_control.authorize_release_merge($1)`,
        [intent],
      );
      const updated = await client.query<ReleaseRow>(
        `SELECT * FROM agentops_control.releases WHERE id = $1`,
        [release.id],
      );
      if (!updated.rows[0]) {
        throw new ReleaseReceiptConflictError(
          'release was already authorized for a different PR or head',
        );
      }
      return releaseRecord(updated.rows[0]);
    });
  }

  /**
   * Idempotently converge recovery on the merge GitHub already reports. A
   * completed release never transitions back to failed because a cleanup job
   * or receipt publisher is retried later.
   */
  async completeReleaseMerge(input: {
    releaseId: string;
    receipt: ReleaseMergeReceipt;
  }): Promise<ReleaseRecord> {
    const receipt = ReleaseMergeReceiptContract.parse(input.receipt);
    if (receipt.releaseId !== input.releaseId) {
      throw new ReleaseReceiptConflictError('merge receipt releaseId mismatch');
    }
    return transaction(this.pool, async (client) => {
      const selected = await client.query<ReleaseRow>(
        `SELECT * FROM agentops_control.releases WHERE id = $1`,
        [input.releaseId],
      );
      const row = selected.rows[0];
      if (!row) throw new Error(`no such release: ${input.releaseId}`);
      const release = releaseRecord(row);
      if (
        release.repository !== receipt.repository
        || release.issueNumber !== receipt.issueNumber
      ) {
        throw new ReleaseReceiptConflictError('merge receipt coordinates mismatch');
      }
      const intentResult = await client.query<ReleaseReceiptRow>(
        `SELECT payload, published_at
           FROM agentops_control.release_receipt_outbox
          WHERE release_id = $1 AND kind = 'merge-intent'`,
        [release.id],
      );
      const intent = intentResult.rows[0]
        ? ReleaseMergeIntentReceiptContract.parse(intentResult.rows[0].payload)
        : null;
      if (
        !intent
        || intent.pullRequestNumber !== receipt.pullRequestNumber
        || intent.expectedHead !== receipt.expectedHead
        || intent.observedPrHead !== receipt.observedPrHead
        || !receipt.causes.includes(intent.receiptId)
      ) {
        throw new ReleaseCertificationError(
          'merge receipt is not caused by its matching durable merge intent',
        );
      }
      if (release.status === 'merged') {
        if (
          release.pullRequest !== receipt.pullRequestNumber
          || release.finalHead !== receipt.expectedHead
          || receipt.observedPrHead !== receipt.expectedHead
          || release.mergeSha !== receipt.mergeSha
          || release.mergeActor !== receipt.actor
        ) {
          throw new ReleaseReceiptConflictError(
            'completed release conflicts with observed GitHub merge',
          );
        }
        const existingMerge = await client.query<{ present: boolean }>(
          `SELECT EXISTS(
             SELECT 1 FROM agentops_control.release_receipt_outbox
              WHERE release_id = $1 AND kind = 'merge'
           ) AS present`,
          [release.id],
        );
        if (existingMerge.rows[0]?.present !== true) {
          throw new ReleaseCertificationError(
            'merged release is missing its durable merge receipt',
          );
        }
        return release;
      }
      if (
        release.status !== 'merge-authorized'
        || release.pullRequest !== receipt.pullRequestNumber
        || release.finalHead !== receipt.expectedHead
        || receipt.observedPrHead !== receipt.expectedHead
      ) {
        throw new ReleaseCertificationError(
          'GitHub merge has no matching durable expected-head authorization',
        );
      }
      await client.query(
        `SELECT agentops_control.complete_release_merge($1)`,
        [receipt],
      );
      const updated = await client.query<ReleaseRow>(
        `SELECT * FROM agentops_control.releases WHERE id = $1`,
        [release.id],
      );
      if (!updated.rows[0]) {
        throw new ReleaseReceiptConflictError('release merge raced with another result');
      }
      return releaseRecord(updated.rows[0]);
    });
  }

  async markReleaseReceiptPublished(receiptId: string): Promise<void> {
    const result = await this.pool.query(
      `UPDATE agentops_control.release_receipt_outbox
          SET published_at = COALESCE(published_at, clock_timestamp())
        WHERE receipt_id = $1`,
      [z.string().uuid().parse(receiptId)],
    );
    if ((result.rowCount ?? 0) !== 1) {
      throw new Error(`no such release receipt: ${receiptId}`);
    }
  }

  async saveMonitorCursor(input: {
    registrationId: string;
    monitorKind: 'issue' | 'pull_request';
    cursor: Record<string, unknown>;
    observedAt: Date;
  }): Promise<boolean> {
    const result = await this.pool.query(
      `INSERT INTO agentops_control.monitor_cursors(
         registration_id, monitor_kind, cursor, observed_at
       ) VALUES ($1, $2, $3, $4)
       ON CONFLICT (registration_id, monitor_kind) DO UPDATE
         SET cursor = EXCLUDED.cursor,
             observed_at = EXCLUDED.observed_at,
             updated_at = clock_timestamp()
       WHERE agentops_control.monitor_cursors.observed_at < EXCLUDED.observed_at
       RETURNING registration_id`,
      [input.registrationId, input.monitorKind, input.cursor, input.observedAt],
    );
    return (result.rowCount ?? 0) > 0;
  }

  async getMonitorCursor(
    registrationId: string,
    monitorKind: 'issue' | 'pull_request',
  ): Promise<{
    cursor: Record<string, unknown>;
    observedAt: string;
    updatedAt: string;
  } | null> {
    const result = await this.pool.query<{
      cursor: Record<string, unknown>;
      observed_at: Date;
      updated_at: Date;
    }>(
      `SELECT cursor, observed_at, updated_at
         FROM agentops_control.monitor_cursors
        WHERE registration_id = $1 AND monitor_kind = $2`,
      [registrationId, monitorKind],
    );
    const row = result.rows[0];
    return row ? {
      cursor: row.cursor,
      observedAt: row.observed_at.toISOString(),
      updatedAt: row.updated_at.toISOString(),
    } : null;
  }

  async claimMonitorBrokerRequest(input: {
    workerId: string;
    leaseMs: number;
  }): Promise<MonitorBrokerRequest | null> {
    if (
      !Number.isInteger(input.leaseMs)
      || input.leaseMs < 5_000
      || input.leaseMs > 60_000
    ) {
      throw new Error('monitor broker lease must be 5000..60000ms');
    }
    return transaction(this.pool, async (client) => {
      const leaseToken = randomUUID();
      const result = await client.query<MonitorBrokerRequestRow>(
        `SELECT * FROM agentops_control.claim_monitor_broker_request($1, $2, $3)`,
        [input.workerId, leaseToken, input.leaseMs],
      );
      const row = result.rows[0];
      if (!row) return null;
      const cursor = MonitorBrokerCursor.parse(row.cursor);
      await client.query(
        `INSERT INTO agentops_control.runtime_audit(
           actor_type, actor_id, event_type, registration_id, details
         ) VALUES ('triage', $1, 'monitor.broker.claimed', $2, $3)`,
        [
          input.workerId,
          row.registration_id,
          {
            requestId: row.id,
            registrationVersion: Number(row.registration_version),
            repository: row.repository,
            monitorKind: row.monitor_kind,
          },
        ],
      );
      return {
        id: row.id,
        registrationId: row.registration_id,
        registrationVersion: Number(row.registration_version),
        repository: row.repository,
        monitorKind: row.monitor_kind,
        cursor,
        leaseToken: row.lease_token,
      };
    });
  }

  async completeMonitorBrokerRequest(input: {
    request: MonitorBrokerRequest;
    workerId: string;
    response: MonitorBrokerResponseType;
  }): Promise<void> {
    const response = MonitorBrokerResponse.parse(input.response);
    await transaction(this.pool, async (client) => {
      const completed = await client.query<{ registration_id: string | null }>(
        `SELECT agentops_control.complete_monitor_broker_request($1, $2, $3, $4)
           AS registration_id`,
        [
          input.request.id,
          input.request.leaseToken,
          input.workerId,
          response,
        ],
      );
      if (!completed.rows[0]?.registration_id) {
        throw new Error('monitor broker lease is stale or lost');
      }
      const responseDigest = createHash('sha256')
        .update(JSON.stringify(response))
        .digest('hex');
      await client.query(
        `INSERT INTO agentops_control.runtime_audit(
           actor_type, actor_id, event_type, registration_id, details
         ) VALUES ('triage', $1, 'monitor.broker.completed', $2, $3)`,
        [
          input.workerId,
          input.request.registrationId,
          {
            requestId: input.request.id,
            registrationVersion: input.request.registrationVersion,
            repository: input.request.repository,
            monitorKind: input.request.monitorKind,
            itemCount: response.items.length,
            responseSha256: responseDigest,
          },
        ],
      );
    });
  }

  async failMonitorBrokerRequest(input: {
    request: MonitorBrokerRequest;
    workerId: string;
    code: string;
    message: string;
  }): Promise<void> {
    const message = input.message.slice(0, 512);
    await transaction(this.pool, async (client) => {
      const failed = await client.query<{ registration_id: string | null }>(
        `SELECT agentops_control.fail_monitor_broker_request($1, $2, $3, $4, $5)
           AS registration_id`,
        [
          input.request.id,
          input.request.leaseToken,
          input.workerId,
          input.code,
          message,
        ],
      );
      if (!failed.rows[0]?.registration_id) {
        throw new Error('monitor broker lease is stale or lost');
      }
      await client.query(
        `INSERT INTO agentops_control.runtime_audit(
           actor_type, actor_id, event_type, registration_id, details
         ) VALUES ('triage', $1, 'monitor.broker.failed', $2, $3)`,
        [
          input.workerId,
          input.request.registrationId,
          {
            requestId: input.request.id,
            registrationVersion: input.request.registrationVersion,
            repository: input.request.repository,
            monitorKind: input.request.monitorKind,
            code: input.code,
          },
        ],
      );
    });
  }

  async receiveWebhook(input: {
    deliveryKey: string;
    repository: string;
    event: string;
    action?: string | null;
    headers: Record<string, string>;
    payload: Record<string, unknown>;
  }): Promise<{ deliveryId: string; duplicate: boolean; status: string }> {
    if (!input.deliveryKey.trim()) throw new Error('delivery key is required');
    return transaction(this.pool, async (client) => {
      const id = randomUUID();
      const inserted = await client.query<{ id: string; status: string }>(
        `INSERT INTO agentops_control.webhook_deliveries(
           id, delivery_key, repository, event, action, headers, payload
         ) VALUES ($1, $2, lower($3), $4, $5, $6, $7)
         ON CONFLICT (delivery_key) DO NOTHING
         RETURNING id, status`,
        [
          id,
          input.deliveryKey,
          input.repository,
          input.event,
          input.action ?? null,
          durableControlWebhookHeaders(input.headers),
          input.payload,
        ],
      );
      if (inserted.rows[0]) {
        return {
          deliveryId: inserted.rows[0].id,
          duplicate: false,
          status: inserted.rows[0].status,
        };
      }
      const existing = await client.query<{ id: string; status: string }>(
        `SELECT id, status FROM agentops_control.webhook_deliveries
          WHERE delivery_key = $1`,
        [input.deliveryKey],
      );
      return {
        deliveryId: existing.rows[0]!.id,
        duplicate: true,
        status: existing.rows[0]!.status,
      };
    });
  }

  async setWebhookConsumers(
    deliveryId: string,
    registrationId: string,
    consumers: readonly string[],
    durationMs = 5 * 60_000,
  ): Promise<{ token: string; expiresAt: string }> {
    if (!Number.isInteger(durationMs) || durationMs <= 0) {
      throw new Error('durationMs must be a positive integer');
    }
    return transaction(this.pool, async (client) => {
      const locked = await client.query<{ status: string }>(
        `SELECT status FROM agentops_control.webhook_deliveries
          WHERE id = $1 FOR UPDATE`,
        [deliveryId],
      );
      if (!locked.rows[0]) throw new Error(`no such webhook delivery: ${deliveryId}`);
      if (locked.rows[0].status !== 'pending') {
        throw new Error(`delivery ${deliveryId} is not pending`);
      }
      const token = randomUUID();
      const ownership = await client.query<{ processing_expires_at: Date }>(
        `UPDATE agentops_control.webhook_deliveries
            SET registration_id = $2, status = 'processing',
                processing_token = $3,
                processing_expires_at =
                  clock_timestamp() + ($4 * interval '1 millisecond'),
                route_attempts = route_attempts + 1, updated_at = clock_timestamp()
          WHERE id = $1
          RETURNING processing_expires_at`,
        [deliveryId, registrationId, token, durationMs],
      );
      for (const consumer of [...new Set(consumers)].sort()) {
        await client.query(
          `INSERT INTO agentops_control.webhook_consumers(delivery_id, consumer)
           VALUES ($1, $2)
           ON CONFLICT (delivery_id, consumer) DO NOTHING`,
          [deliveryId, consumer],
        );
      }
      return {
        token,
        expiresAt: ownership.rows[0]!.processing_expires_at.toISOString(),
      };
    });
  }

  async claimPendingWebhook(durationMs = 5 * 60_000): Promise<WebhookClaim | null> {
    if (!Number.isInteger(durationMs) || durationMs <= 0) {
      throw new Error('durationMs must be a positive integer');
    }
    return transaction(this.pool, async (client) => {
      const pending = await client.query<WebhookDeliveryRow>(
        `SELECT id, delivery_key, repository, event, action, headers, payload, received_at
           FROM agentops_control.webhook_deliveries
          WHERE status = 'pending'
          ORDER BY received_at, id
          FOR UPDATE SKIP LOCKED
          LIMIT 1`,
      );
      const row = pending.rows[0];
      if (!row) return null;
      const token = randomUUID();
      const ownership = await client.query<{ processing_expires_at: Date }>(
        `UPDATE agentops_control.webhook_deliveries
            SET status = 'processing', processing_token = $2,
                processing_expires_at =
                  clock_timestamp() + ($3 * interval '1 millisecond'),
                route_attempts = route_attempts + 1,
                updated_at = clock_timestamp()
          WHERE id = $1
          RETURNING processing_expires_at`,
        [row.id, token, durationMs],
      );
      return {
        deliveryId: row.id,
        deliveryKey: row.delivery_key,
        repository: row.repository,
        event: row.event,
        action: row.action,
        headers: row.headers,
        payload: row.payload,
        token,
        expiresAt: ownership.rows[0]!.processing_expires_at.toISOString(),
        receivedAt: row.received_at.toISOString(),
      };
    });
  }

  async setClaimedWebhookConsumers(
    processingToken: string,
    registrationId: string,
    consumers: readonly string[],
  ): Promise<string> {
    return transaction(this.pool, async (client) => {
      const claimed = await client.query<{ id: string; registration_id: string | null }>(
        `SELECT id, registration_id
           FROM agentops_control.webhook_deliveries
          WHERE processing_token = $1 AND status = 'processing'
            AND processing_expires_at > clock_timestamp()
          FOR UPDATE`,
        [processingToken],
      );
      const row = claimed.rows[0];
      if (!row || (row.registration_id && row.registration_id !== registrationId)) {
        throw new LeaseRejectedError(
          'webhook processing ownership is absent, expired, or already routed',
        );
      }
      await client.query(
        `UPDATE agentops_control.webhook_deliveries
            SET registration_id = $2, updated_at = clock_timestamp()
          WHERE id = $1`,
        [row.id, registrationId],
      );
      for (const consumer of [...new Set(consumers)].sort()) {
        await client.query(
          `INSERT INTO agentops_control.webhook_consumers(delivery_id, consumer)
           VALUES ($1, $2)
           ON CONFLICT (delivery_id, consumer) DO NOTHING`,
          [row.id, consumer],
        );
      }
      return row.id;
    });
  }

  async completeWebhookConsumer(
    deliveryId: string,
    consumer: string,
    processingToken: string,
    error?: string,
  ): Promise<void> {
    const result = await this.pool.query(
      `UPDATE agentops_control.webhook_consumers c
          SET status = $4, attempts = attempts + 1, last_error = $5,
              completed_at = CASE WHEN $4 = 'completed' THEN clock_timestamp() END,
              updated_at = clock_timestamp()
         FROM agentops_control.webhook_deliveries d
        WHERE c.delivery_id = $1 AND c.consumer = $2
          AND d.id = c.delivery_id
          AND d.status = 'processing'
          AND d.processing_token = $3
          AND d.processing_expires_at > clock_timestamp()`,
      [
        deliveryId,
        consumer,
        processingToken,
        error ? 'failed' : 'completed',
        error ?? null,
      ],
    );
    if ((result.rowCount ?? 0) !== 1) {
      throw new LeaseRejectedError(
        `webhook consumer ${consumer} is absent or processing ownership is invalid`,
      );
    }
  }

  async heartbeatWebhookProcessing(
    processingToken: string,
    durationMs: number,
  ): Promise<string> {
    if (!Number.isInteger(durationMs) || durationMs <= 0) {
      throw new Error('durationMs must be a positive integer');
    }
    const result = await this.pool.query<{ processing_expires_at: Date }>(
      `UPDATE agentops_control.webhook_deliveries
          SET processing_expires_at =
                clock_timestamp() + ($2 * interval '1 millisecond'),
              updated_at = clock_timestamp()
        WHERE processing_token = $1 AND status = 'processing'
          AND processing_expires_at > clock_timestamp()
        RETURNING processing_expires_at`,
      [processingToken, durationMs],
    );
    if (!result.rows[0]) {
      throw new LeaseRejectedError('webhook processing ownership is absent or expired');
    }
    return result.rows[0].processing_expires_at.toISOString();
  }

  async finishWebhookDelivery(
    deliveryId: string,
    outcome:
      | { status: 'processed' }
      | { status: 'ignored'; reason: string }
      | { status: 'failed'; error: string },
    processingToken?: string,
  ): Promise<void> {
    await transaction(this.pool, async (client) => {
      const delivery = await client.query<{
        status: string;
        registration_id: string | null;
        processing_token: string | null;
        ownership_active: boolean;
      }>(
        `SELECT status, registration_id, processing_token,
                processing_expires_at > clock_timestamp() AS ownership_active
           FROM agentops_control.webhook_deliveries
          WHERE id = $1 FOR UPDATE`,
        [deliveryId],
      );
      const row = delivery.rows[0];
      if (!row) throw new Error(`no such webhook delivery: ${deliveryId}`);
      if (row.status === 'processing') {
        if (
          !processingToken
          || row.processing_token !== processingToken
          || !row.ownership_active
        ) {
          throw new LeaseRejectedError(
            'webhook processing ownership is invalid or expired',
          );
        }
      } else if (outcome.status !== 'ignored' || row.status !== 'pending') {
        throw new Error(`delivery ${deliveryId} cannot transition from ${row.status}`);
      }
      if (outcome.status === 'processed' || outcome.status === 'failed') {
        if (row.status !== 'processing') {
          throw new Error(`delivery ${deliveryId} is not processing`);
        }
        if (outcome.status === 'processed') {
          const incomplete = await client.query<{ count: string }>(
            `SELECT count(*) AS count
               FROM agentops_control.webhook_consumers
              WHERE delivery_id = $1 AND status <> 'completed'`,
            [deliveryId],
          );
          if (Number(incomplete.rows[0]!.count) > 0) {
            throw new Error(`delivery ${deliveryId} has incomplete consumers`);
          }
        }
      } else if (outcome.status === 'ignored' && row.registration_id !== null) {
        throw new Error(`routed delivery ${deliveryId} cannot be ignored`);
      }
      await client.query(
        `UPDATE agentops_control.webhook_deliveries
            SET status = $2,
                ignored_reason = $3,
                last_error = $4,
                processing_token = NULL,
                processing_expires_at = NULL,
                updated_at = clock_timestamp()
          WHERE id = $1`,
        [
          deliveryId,
          outcome.status,
          outcome.status === 'ignored' ? outcome.reason : null,
          outcome.status === 'failed' ? outcome.error : null,
        ],
      );
    });
  }

  /** Crash recovery resets only unfinished delivery/consumer state. */
  async recoverInterruptedWebhooks(): Promise<number> {
    return transaction(this.pool, async (client) => {
      const interrupted = await client.query<{ id: string }>(
        `SELECT id
           FROM agentops_control.webhook_deliveries
          WHERE status = 'processing'
            AND processing_expires_at <= clock_timestamp()
          ORDER BY processing_expires_at
          FOR UPDATE SKIP LOCKED`,
      );
      if (interrupted.rows.length === 0) return 0;
      const ids = interrupted.rows.map((row) => row.id);
      await client.query(
        `UPDATE agentops_control.webhook_consumers
            SET status = 'pending', last_error = NULL,
                updated_at = clock_timestamp()
          WHERE delivery_id = ANY($1::uuid[]) AND status IN ('processing', 'failed')`,
        [ids],
      );
      await client.query(
        `UPDATE agentops_control.webhook_deliveries
            SET status = 'pending', last_error = NULL,
                processing_token = NULL, processing_expires_at = NULL,
                updated_at = clock_timestamp()
          WHERE id = ANY($1::uuid[])`,
        [ids],
      );
      return interrupted.rows.length;
    });
  }

  async enqueueJob(input: unknown): Promise<EnqueueResult> {
    const parsed = EnqueueJobInput.parse(input);
    return transaction(this.pool, async (client) => {
      const registrationResult = await client.query<RegistrationRow>(
        `SELECT * FROM agentops_control.repository_registrations
          WHERE id = $1 FOR SHARE`,
        [parsed.registrationId],
      );
      const current = registrationResult.rows[0];
      if (
        !current
        || !current.enabled
        || !current.execution_enabled
        || Number(current.version) !== parsed.registrationVersion
      ) {
        throw new StaleRegistrationError(
          `registration ${parsed.registrationId} is absent, disabled, or stale`,
        );
      }
      const lifecycleResult = await client.query<{ mode: string }>(
        `SELECT mode
           FROM agentops_control.lifecycle_state
          WHERE singleton
          FOR SHARE`,
      );
      if (lifecycleResult.rows[0]?.mode !== 'ACTIVE') {
        throw new OperatingModeError(
          `operating mode ${lifecycleResult.rows[0]?.mode ?? 'unknown'} does not permit enqueue`,
        );
      }
      // The idempotency-key lock makes same-logical webhook/poll races converge
      // before the repository-wide active-job constraint can reject the loser.
      await client.query(
        'SELECT pg_advisory_xact_lock(hashtextextended($1, 0))',
        [advisoryRequestKey(parsed.registrationId, parsed.idempotencyKey)],
      );
      const requeueAfterRegistrationChange = async (
        row: JobRow,
      ): Promise<EnqueueResult | null> => {
        if (
          Number(row.registration_version) === parsed.registrationVersion
          || row.status !== 'rejected'
          || row.last_error !== 'registration changed before lease acquisition'
        ) {
          return null;
        }
        const requeued = await client.query<JobRow>(
          `UPDATE agentops_control.jobs
              SET registration_version = $2, status = 'queued',
                  available_at = clock_timestamp(), finished_at = NULL,
                  last_error = NULL, updated_at = clock_timestamp()
            WHERE id = $1
              AND registration_version = $3
              AND status = 'rejected'
              AND last_error = 'registration changed before lease acquisition'
            RETURNING *`,
          [row.id, parsed.registrationVersion, row.registration_version],
        );
        if (!requeued.rows[0]) {
          throw new StaleRegistrationError(
            `job ${row.id} changed while recovering a stale registration observation`,
          );
        }
        await client.query(
          `INSERT INTO agentops_control.runtime_audit(
             actor_type, actor_id, event_type, registration_id, job_id, details
           ) VALUES ('control', 'control-store',
                     'job.requeued_after_registration_change', $1, $2, $3)`,
          [
            parsed.registrationId,
            row.id,
            {
              fromRegistrationVersion: Number(row.registration_version),
              toRegistrationVersion: parsed.registrationVersion,
              sourceKind: parsed.source.kind,
              sourceKey: parsed.source.key,
              idempotencyKey: parsed.idempotencyKey,
            },
          ],
        );
        return { job: job(requeued.rows[0]), duplicate: false };
      };
      const duplicate = await client.query<JobRow & { same_request: boolean }>(
        `SELECT j.*, (j.job_type = $3 AND j.payload = $4::jsonb) AS same_request
           FROM agentops_control.jobs j
          WHERE j.registration_id = $1 AND j.idempotency_key = $2`,
        [parsed.registrationId, parsed.idempotencyKey, parsed.jobType, parsed.payload],
      );
      if (duplicate.rows[0]) {
        if (!duplicate.rows[0].same_request) {
          throw new IdempotencyConflictError(
            `idempotency key ${parsed.idempotencyKey} was reused for a different request`,
          );
        }
        const recovered = await requeueAfterRegistrationChange(duplicate.rows[0]);
        if (recovered) return recovered;
        return { job: job(duplicate.rows[0]), duplicate: true };
      }
      const sourceDuplicate = await client.query<JobRow & { same_request: boolean }>(
        `SELECT j.*, (j.job_type = $4 AND j.payload = $5::jsonb) AS same_request
           FROM agentops_control.jobs j
          WHERE j.registration_id = $1 AND j.source_kind = $2 AND j.source_key = $3`,
        [
          parsed.registrationId,
          parsed.source.kind,
          parsed.source.key,
          parsed.jobType,
          parsed.payload,
        ],
      );
      if (sourceDuplicate.rows[0]) {
        if (!sourceDuplicate.rows[0].same_request) {
          throw new IdempotencyConflictError(
            `source key ${parsed.source.kind}:${parsed.source.key} was reused for a different request`,
          );
        }
        const recovered = await requeueAfterRegistrationChange(sourceDuplicate.rows[0]);
        if (recovered) return recovered;
        return { job: job(sourceDuplicate.rows[0]), duplicate: true };
      }

      // Runtime rejection gives a useful error; the partial unique index remains
      // authoritative when parallel transactions race after this read.
      const active = await client.query<{ id: string }>(
        `SELECT id FROM agentops_control.jobs
          WHERE registration_id = $1 AND status IN ('queued', 'leased')
          LIMIT 1`,
        [parsed.registrationId],
      );
      if (active.rows[0]) {
        throw new RepositoryBusyError(
          `repository ${current.repository} already has active job ${active.rows[0].id}`,
        );
      }
      try {
        const result = await client.query<
          JobRow & { duplicate: boolean; same_request: boolean }
        >(
          `INSERT INTO agentops_control.jobs(
             id, registration_id, registration_version, source_kind, source_key,
             idempotency_key, job_type, payload, available_at
           ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, clock_timestamp()))
           ON CONFLICT (registration_id, idempotency_key) DO UPDATE
             SET idempotency_key = EXCLUDED.idempotency_key
           RETURNING *, (xmax <> 0) AS duplicate,
             (job_type = $7 AND payload = $8::jsonb) AS same_request`,
          [
            randomUUID(),
            parsed.registrationId,
            parsed.registrationVersion,
            parsed.source.kind,
            parsed.source.key,
            parsed.idempotencyKey,
            parsed.jobType,
            parsed.payload,
            parsed.availableAt ?? null,
          ],
        );
        if (result.rows[0]!.duplicate && !result.rows[0]!.same_request) {
          throw new IdempotencyConflictError(
            `idempotency key ${parsed.idempotencyKey} raced with a different request`,
          );
        }
        return { job: job(result.rows[0]!), duplicate: result.rows[0]!.duplicate };
      } catch (error) {
        if (
          error
          && typeof error === 'object'
          && 'code' in error
          && error.code === '23505'
        ) {
          if (
            'constraint' in error
            && error.constraint === 'jobs_registration_source_key'
          ) {
            throw new IdempotencyConflictError(
              `source key ${parsed.source.kind}:${parsed.source.key} raced with another request`,
            );
          }
          throw new RepositoryBusyError(
            `repository ${current.repository} already has an active job`,
          );
        }
        throw error;
      }
    });
  }

  async acquireLease(input: {
    workerId: string;
    durationMs: number;
    jobType?: string;
  }): Promise<Lease | null> {
    if (!input.workerId.trim()) throw new Error('workerId is required');
    if (!Number.isInteger(input.durationMs) || input.durationMs <= 0) {
      throw new Error('durationMs must be a positive integer');
    }
    if (input.jobType !== undefined && !input.jobType.trim()) {
      throw new Error('jobType must be non-empty when provided');
    }
    return transaction(this.pool, async (client) => {
      const lifecycleResult = await client.query<{ mode: string }>(
        `SELECT mode
           FROM agentops_control.lifecycle_state
          WHERE singleton
          FOR SHARE`,
      );
      if (lifecycleResult.rows[0]?.mode !== 'ACTIVE') return null;
      const candidate = await client.query<JobRow>(
        `SELECT j.*
           FROM agentops_control.jobs j
           JOIN agentops_control.repository_registrations r ON r.id = j.registration_id
          WHERE j.status = 'queued'
            AND j.available_at <= clock_timestamp()
            AND ($1::text IS NULL OR j.job_type = $1)
            AND r.enabled
            AND r.execution_enabled
            AND r.version = j.registration_version
          ORDER BY j.available_at, j.created_at
          FOR UPDATE OF j, r SKIP LOCKED
          LIMIT 1`,
        [input.jobType ?? null],
      );
      const selected = candidate.rows[0];
      if (!selected) return null;
      const attemptNumber = await client.query<{ next: number }>(
        `SELECT COALESCE(MAX(attempt_number), 0) + 1 AS next
           FROM agentops_control.job_attempts WHERE job_id = $1`,
        [selected.id],
      );
      const attemptId = randomUUID();
      const leaseId = randomUUID();
      const leaseToken = randomUUID();
      await client.query(
        `UPDATE agentops_control.jobs
            SET status = 'leased', updated_at = clock_timestamp()
          WHERE id = $1`,
        [selected.id],
      );
      await client.query(
        `INSERT INTO agentops_control.job_attempts(
           id, job_id, attempt_number, worker_id, status
         ) VALUES ($1, $2, $3, $4, 'running')`,
        [attemptId, selected.id, attemptNumber.rows[0]!.next, input.workerId],
      );
      const leaseResult = await client.query<{ expires_at: Date }>(
        `INSERT INTO agentops_control.job_leases(
           id, job_id, attempt_id, lease_token, worker_id, expires_at
         ) VALUES ($1, $2, $3, $4, $5, clock_timestamp() + ($6 * interval '1 millisecond'))
         RETURNING expires_at`,
        [leaseId, selected.id, attemptId, leaseToken, input.workerId, input.durationMs],
      );
      await client.query(
        `INSERT INTO agentops_control.runtime_audit(
           actor_type, actor_id, event_type, registration_id, job_id, details
         ) VALUES ('runner', $1, 'runner.boundary.claim.allowed', $2, $3, $4)`,
        [
          input.workerId,
          selected.registration_id,
          selected.id,
          {
            registrationVersion: Number(selected.registration_version),
            attemptNumber: attemptNumber.rows[0]!.next,
            leaseId,
            expiresAt: leaseResult.rows[0]!.expires_at.toISOString(),
          },
        ],
      );
      return {
        id: leaseId,
        token: leaseToken,
        workerId: input.workerId,
        attemptId,
        attemptNumber: attemptNumber.rows[0]!.next,
        expiresAt: leaseResult.rows[0]!.expires_at.toISOString(),
        job: { ...job(selected), status: 'leased' },
      };
    });
  }

  async heartbeatLease(token: string, durationMs: number): Promise<string> {
    if (!Number.isInteger(durationMs) || durationMs <= 0) {
      throw new Error('durationMs must be a positive integer');
    }
    const result = await this.pool.query<{ expires_at: Date }>(
      `UPDATE agentops_control.job_leases
          SET heartbeat_at = clock_timestamp(),
              expires_at = clock_timestamp() + ($2 * interval '1 millisecond')
        WHERE lease_token = $1 AND status = 'active' AND expires_at > clock_timestamp()
        RETURNING expires_at`,
      [token, durationMs],
    );
    if (!result.rows[0]) throw new LeaseRejectedError('lease is absent, inactive, or expired');
    return result.rows[0].expires_at.toISOString();
  }

  /**
   * Publish one idempotent operator-facing phase transition. The database
   * derives every ownership coordinate from the still-live runner lease.
   */
  async recordDevelopmentProgress(input: {
    token: string;
    workerId: string;
    event: DevelopmentProgressUpdateType;
  }): Promise<number> {
    if (!input.workerId.trim()) throw new Error('workerId is required');
    const event = DevelopmentProgressUpdate.parse(input.event);
    const result = await this.pool.query<{ progress_id: string }>(
      `SELECT agentops_control.record_development_progress($1, $2, $3)
         AS progress_id`,
      [input.token, input.workerId, event],
    );
    const progressId = Number(result.rows[0]?.progress_id);
    if (!Number.isSafeInteger(progressId) || progressId < 1) {
      throw new Error('development progress insert returned no identity');
    }
    return progressId;
  }

  async recordDevelopmentReviewRound(input: {
    token: string;
    workerId: string;
    review: DevelopmentReviewRoundType;
  }): Promise<number> {
    if (!input.workerId.trim()) throw new Error('workerId is required');
    const review = DevelopmentReviewRound.parse(input.review);
    const result = await this.pool.query<{ review_id: string }>(
      `SELECT agentops_control.record_development_review_round($1, $2, $3)
         AS review_id`,
      [input.token, input.workerId, review],
    );
    const reviewId = Number(result.rows[0]?.review_id);
    if (!Number.isSafeInteger(reviewId) || reviewId < 1) {
      throw new Error('development review round insert returned no identity');
    }
    return reviewId;
  }

  async recordReviewChild(input: {
    token: string;
    workerId: string;
    childIssueNumber: number;
    childIssueUrl: string;
    findingKey: string;
    finding: z.input<typeof DevelopmentReviewFinding>;
    reviewRound: number;
    parentPullRequestNumber: number;
    parentBranch: string;
    parentHeadSha: string;
  }): Promise<string> {
    const child = z.object({
      token: z.string().uuid(),
      workerId: z.string().trim().min(1).max(256),
      childIssueNumber: z.number().int().positive(),
      childIssueUrl: z.string().url().max(2_000),
      findingKey: z.string().regex(/^sha256:[0-9a-f]{64}$/),
      finding: DevelopmentReviewFinding.refine(
        (finding) => finding.disposition === 'separate-issue',
        'review child requires separate-issue disposition',
      ),
      reviewRound: z.number().int().positive().max(1_000),
      parentPullRequestNumber: z.number().int().positive(),
      parentBranch: z.string().trim().min(1).max(500),
      parentHeadSha: z.string().regex(/^[0-9a-f]{40}([0-9a-f]{24})?$/),
    }).strict().parse(input);
    const result = await this.pool.query<{ child_id: string }>(
      `SELECT agentops_control.record_review_child($1, $2, $3) AS child_id`,
      [child.token, child.workerId, {
        childIssueNumber: child.childIssueNumber,
        childIssueUrl: child.childIssueUrl,
        findingKey: child.findingKey,
        finding: child.finding,
        reviewRound: child.reviewRound,
        parentPullRequestNumber: child.parentPullRequestNumber,
        parentBranch: child.parentBranch,
        parentHeadSha: child.parentHeadSha,
      }],
    );
    return z.string().uuid().parse(result.rows[0]?.child_id);
  }

  async getReviewChildTarget(
    repositoryInput: string,
    issueNumberInput: number,
  ): Promise<{
    nodeId: string;
    parentNodeId: string;
    parentIssueNumber: number;
    parentPullRequestNumber: number;
    parentBranch: string;
    parentHeadSha: string;
    reviewRound: number;
  } | null> {
    const repository = CanonicalRepository.parse(repositoryInput);
    const issueNumber = z.number().int().positive().parse(issueNumberInput);
    const result = await this.pool.query<{
      id: string;
      parent_node_id: string;
      parent_issue_number: string;
      parent_pull_request_number: string;
      parent_branch: string;
      parent_head_sha: string;
      review_round: number;
    }>(
      `SELECT child.id, child.parent_node_id,
              parent.issue_number AS parent_issue_number,
              child.parent_pull_request_number, child.parent_branch,
              child.parent_head_sha, child.review_round
         FROM agentops_control.development_lineage_nodes child
         JOIN agentops_control.development_lineage_nodes parent
           ON parent.id = child.parent_node_id
        WHERE child.repository = $1 AND child.issue_number = $2`,
      [repository, issueNumber],
    );
    const row = result.rows[0];
    if (!row) return null;
    return {
      nodeId: row.id,
      parentNodeId: row.parent_node_id,
      parentIssueNumber: Number(row.parent_issue_number),
      parentPullRequestNumber: Number(row.parent_pull_request_number),
      parentBranch: row.parent_branch,
      parentHeadSha: row.parent_head_sha,
      reviewRound: row.review_round,
    };
  }

  async bindReviewChildRelease(input: {
    token: string;
    workerId: string;
    releaseId: string;
  }): Promise<string> {
    const parsed = z.object({
      token: z.string().uuid(),
      workerId: z.string().trim().min(1).max(256),
      releaseId: z.string().uuid(),
    }).strict().parse(input);
    const result = await this.pool.query<{ child_id: string }>(
      `SELECT agentops_control.bind_review_child_release($1, $2, $3)
         AS child_id`,
      [parsed.token, parsed.workerId, parsed.releaseId],
    );
    return z.string().uuid().parse(result.rows[0]?.child_id);
  }

  async reviewLineageGate(releaseIdInput: string): Promise<{
    ready: boolean;
    pending: Array<{ issueNumber: number; status: string }>;
    integratedHeads: string[];
  }> {
    const releaseId = z.string().uuid().parse(releaseIdInput);
    const rows = await this.pool.query<{
      issue_number: string;
      status: string;
      integrated_head_sha: string | null;
    }>(
      `SELECT child.issue_number, child.status, child.integrated_head_sha
         FROM agentops_control.development_lineage_nodes parent
         JOIN agentops_control.development_lineage_nodes child
           ON child.parent_node_id = parent.id
        WHERE parent.release_id = $1
        ORDER BY child.created_at, child.id`,
      [releaseId],
    );
    const pending = rows.rows
      .filter((row) => row.status !== 'integrated')
      .map((row) => ({ issueNumber: Number(row.issue_number), status: row.status }));
    return {
      ready: pending.length === 0,
      pending,
      integratedHeads: rows.rows.flatMap((row) =>
        row.integrated_head_sha ? [row.integrated_head_sha] : []),
    };
  }

  async markReviewChildIntegrated(input: {
    token: string;
    workerId: string;
    releaseId: string;
    pullRequestNumber: number;
    headSha: string;
    integratedHeadSha: string;
  }): Promise<string> {
    const parsed = z.object({
      token: z.string().uuid(),
      workerId: z.string().trim().min(1).max(256),
      releaseId: z.string().uuid(),
      pullRequestNumber: z.number().int().positive(),
      headSha: z.string().regex(/^[0-9a-f]{40}([0-9a-f]{24})?$/),
      integratedHeadSha: z.string().regex(/^[0-9a-f]{40}([0-9a-f]{24})?$/),
    }).strict().parse(input);
    const result = await this.pool.query<{ child_id: string }>(
      `SELECT agentops_control.mark_review_child_integrated(
         $1, $2, $3, $4, $5, $6
       ) AS child_id`,
      [
        parsed.token,
        parsed.workerId,
        parsed.releaseId,
        parsed.pullRequestNumber,
        parsed.headSha,
        parsed.integratedHeadSha,
      ],
    );
    return z.string().uuid().parse(result.rows[0]?.child_id);
  }

  async reclaimExpiredLeases(
    maxAttempts = 3,
    options: { jobType?: string; retryBaseMs?: number } = {},
  ): Promise<number> {
    if (!Number.isInteger(maxAttempts) || maxAttempts < 1) {
      throw new Error('maxAttempts must be a positive integer');
    }
    if (options.jobType !== undefined && !options.jobType.trim()) {
      throw new Error('jobType must be non-empty when provided');
    }
    const retryBaseMs = options.retryBaseMs ?? 0;
    if (!Number.isInteger(retryBaseMs) || retryBaseMs < 0) {
      throw new Error('retryBaseMs must be a non-negative integer');
    }
    return transaction(this.pool, async (client) => {
      const expired = await client.query<{
        id: string;
        job_id: string;
        attempt_id: string;
        attempt_number: number;
      }>(
        `SELECT l.id, l.job_id, l.attempt_id, a.attempt_number
           FROM agentops_control.job_leases l
           JOIN agentops_control.job_attempts a ON a.id = l.attempt_id
           JOIN agentops_control.jobs j ON j.id = l.job_id
           JOIN agentops_control.repository_registrations r ON r.id = j.registration_id
          WHERE l.status = 'active' AND l.expires_at <= clock_timestamp()
            AND ($1::text IS NULL OR j.job_type = $1)
          ORDER BY l.expires_at
          FOR UPDATE OF l, a, j, r SKIP LOCKED`,
        [options.jobType ?? null],
      );
      for (const lease of expired.rows) {
        const retryDelayMs = Math.min(
          60 * 60_000,
          retryBaseMs * (2 ** Math.max(0, lease.attempt_number - 1)),
        );
        await client.query(
          `UPDATE agentops_control.job_leases
              SET status = 'expired', released_at = clock_timestamp()
            WHERE id = $1`,
          [lease.id],
        );
        await client.query(
          `UPDATE agentops_control.job_attempts
              SET status = 'timed_out', finished_at = clock_timestamp(),
                  error = 'lease expired',
                  failure = jsonb_build_object(
                    'schemaVersion', 1,
                    'status', 'failed',
                    'code', 'lease_lost',
                    'message', 'lease expired',
                    'retryable', $2::boolean,
                    'boundary', NULL,
                    'observedAt', to_char(
                      clock_timestamp() AT TIME ZONE 'UTC',
                      'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'
                    )
                  )
            WHERE id = $1 AND status = 'running'`,
          [lease.attempt_id, lease.attempt_number < maxAttempts],
        );
        await client.query(
          `UPDATE agentops_control.jobs j
              SET status = CASE
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version
                     AND $2::integer > $3::integer THEN 'queued'
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version THEN 'failed'
                    ELSE 'rejected'
                  END,
                  available_at = CASE
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version
                     AND $2::integer > $3::integer
                    THEN clock_timestamp() + ($4 * interval '1 millisecond')
                    ELSE j.available_at
                  END,
                  finished_at = CASE
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version
                     AND $2::integer > $3::integer
                    THEN NULL
                    ELSE clock_timestamp()
                  END,
                  updated_at = clock_timestamp(),
                  last_error = CASE
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version
                     AND $2::integer > $3::integer
                    THEN 'lease expired'
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version
                    THEN 'lease expired and max attempts exhausted'
                    ELSE 'lease expired after registration changed'
                  END,
                  failure = CASE
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version
                     AND $2::integer > $3::integer THEN NULL
                    WHEN r.enabled AND r.execution_enabled
                     AND r.version = j.registration_version
                    THEN jsonb_build_object(
                      'schemaVersion', 1,
                      'status', 'failed',
                      'code', 'lease_lost',
                      'message', 'lease expired and max attempts exhausted',
                      'retryable', false,
                      'boundary', NULL,
                      'observedAt', to_char(
                        clock_timestamp() AT TIME ZONE 'UTC',
                        'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'
                      )
                    )
                    ELSE jsonb_build_object(
                      'schemaVersion', 1,
                      'status', 'failed',
                      'code', 'registration_stale',
                      'message', 'lease expired after registration changed',
                      'retryable', false,
                      'boundary', 'claim',
                      'observedAt', to_char(
                        clock_timestamp() AT TIME ZONE 'UTC',
                        'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'
                      )
                    )
                  END
             FROM agentops_control.repository_registrations r
            WHERE j.id = $1 AND j.status = 'leased'
              AND r.id = j.registration_id`,
          [lease.job_id, maxAttempts, lease.attempt_number, retryDelayMs],
        );
        await client.query(
          `INSERT INTO agentops_control.runtime_audit(
             actor_type, actor_id, event_type, job_id, details
           ) VALUES ('runner', 'lease-reclaimer', 'runner.lease.expired', $1, $2)`,
          [
            lease.job_id,
            {
              attemptNumber: lease.attempt_number,
              maxAttempts,
              retryDelayMs,
              jobType: options.jobType ?? null,
            },
          ],
        );
      }
      return expired.rowCount ?? 0;
    });
  }

  async finishLease(
    token: string,
    outcome:
      | { status: 'succeeded'; result?: RunnerJobResultV1 }
      | { status: 'failed'; error?: string; failure?: RunnerJobFailureV1 },
  ): Promise<void> {
    const result = outcome.status === 'succeeded' && outcome.result
      ? RunnerJobResultV1Contract.parse(outcome.result)
      : null;
    const failure = outcome.status === 'failed' && outcome.failure
      ? RunnerJobFailureV1Contract.parse(outcome.failure)
      : null;
    await transaction(this.pool, async (client) => {
      const lease = await client.query<{ id: string; job_id: string; attempt_id: string }>(
        `SELECT id, job_id, attempt_id
           FROM agentops_control.job_leases
          WHERE lease_token = $1 AND status = 'active'
            AND expires_at > clock_timestamp()
          FOR UPDATE`,
        [token],
      );
      const row = lease.rows[0];
      if (!row) throw new LeaseRejectedError('lease is absent, inactive, or expired');
      await client.query(
        `UPDATE agentops_control.job_leases
            SET status = 'completed', released_at = clock_timestamp()
          WHERE id = $1`,
        [row.id],
      );
      await client.query(
        `UPDATE agentops_control.job_attempts
            SET status = $2, finished_at = clock_timestamp(), error = $3,
                failure = $4
          WHERE id = $1`,
        [
          row.attempt_id,
          outcome.status,
          outcome.status === 'failed' ? outcome.error ?? failure?.message ?? null : null,
          failure,
        ],
      );
      await client.query(
        `UPDATE agentops_control.jobs
            SET status = $2, finished_at = clock_timestamp(),
                updated_at = clock_timestamp(), last_error = $3,
                result = $4, failure = $5
          WHERE id = $1`,
        [
          row.job_id,
          outcome.status,
          outcome.status === 'failed' ? outcome.error ?? failure?.message ?? null : null,
          result,
          failure,
        ],
      );
    });
  }

  /**
   * Atomically finish a successful planning WHAT stop and retire its release
   * identity. A later human-ready event must receive fresh authority evidence.
   */
  async finishHumanReviewLease(input: {
    token: string;
    workerId: string;
    result: RunnerJobResultV1;
  }): Promise<string> {
    const result = RunnerJobResultV1Contract.parse(input.result);
    if (result.outcome !== 'needs-human-review') {
      throw new Error('finishHumanReviewLease requires needs-human-review');
    }
    const completed = await this.pool.query<{ release_id: string }>(
      `SELECT agentops_control.complete_runner_human_review($1, $2, $3)
        AS release_id`,
      [input.token, input.workerId, result],
    );
    const releaseId = completed.rows[0]?.release_id;
    if (!releaseId) {
      throw new LeaseRejectedError(
        'runner human-review completion did not return a release identity',
      );
    }
    return z.string().uuid().parse(releaseId);
  }

  async finishTriageLease(input: {
    token: string;
    workerId: string;
    result: TriageJobResultV1;
  }): Promise<void> {
    const result = TriageJobResultV1Contract.parse(input.result);
    await transaction(this.pool, async (client) => {
      const lease = await client.query<{
        id: string;
        job_id: string;
        attempt_id: string;
        attempt_number: number;
      }>(
        `SELECT l.id, l.job_id, l.attempt_id, a.attempt_number
           FROM agentops_control.job_leases l
           JOIN agentops_control.job_attempts a ON a.id = l.attempt_id
           JOIN agentops_control.jobs j ON j.id = l.job_id
          WHERE l.lease_token = $1
            AND l.worker_id = $2
            AND l.status = 'active'
            AND l.expires_at > clock_timestamp()
            AND j.status = 'leased'
            AND j.job_type = 'agentops.triage'
          FOR UPDATE OF l, a, j`,
        [input.token, input.workerId],
      );
      const row = lease.rows[0];
      if (
        !row
        || row.job_id !== result.jobId
        || row.attempt_number !== result.attemptNumber
      ) {
        throw new LeaseRejectedError(
          'triage lease is absent, inactive, expired, or mismatched',
        );
      }
      await client.query(
        `UPDATE agentops_control.job_leases
            SET status = 'completed', released_at = clock_timestamp()
          WHERE id = $1`,
        [row.id],
      );
      await client.query(
        `UPDATE agentops_control.job_attempts
            SET status = 'succeeded', finished_at = clock_timestamp(),
                error = NULL, failure = NULL
          WHERE id = $1`,
        [row.attempt_id],
      );
      await client.query(
        `UPDATE agentops_control.jobs
            SET status = 'succeeded', finished_at = clock_timestamp(),
                updated_at = clock_timestamp(), last_error = NULL,
                result = $2, failure = NULL
          WHERE id = $1`,
        [row.job_id, result],
      );
      await client.query(
        `INSERT INTO agentops_control.runtime_audit(
           actor_type, actor_id, event_type, job_id, details
         ) VALUES ('triage', $1, 'triage.completed', $2, $3)`,
        [
          input.workerId,
          row.job_id,
          {
            outcome: result.outcome,
            repository: result.repository,
            issueNumber: result.issueNumber,
            sourceDigest: result.sourceDigest,
          },
        ],
      );
    });
  }

  async promoteTriageLease(input: {
    token: string;
    workerId: string;
    result: TriageJobResultV1;
    readyLabel: string;
    claimedLabel: string;
    authority: {
      actor: string;
      readyAt: string;
      sourceIssue: ReleaseSourceIssueSnapshot;
      triage?: {
        sourceDigest: string;
        decision: TriageDecisionV1;
        completedAt: string;
        providerProvenance: NonNullable<TriageJobResultV1['providerProvenance']>;
      };
    };
  }): Promise<string> {
    const result = TriageJobResultV1Contract.parse(input.result);
    if (
      result.outcome !== 'promoted'
      || result.decision !== null
      || result.commentUrl !== null
      || result.appliedLabels.length !== 0
      || result.promotedJobId !== null
    ) {
      throw new Error('triage promotion result must describe an uncommitted promotion');
    }
    const readyLabel = GitHubLabelNameContract.parse(input.readyLabel);
    const claimedLabel = GitHubLabelNameContract.parse(input.claimedLabel);
    if (readyLabel === claimedLabel) {
      throw new Error('ready and claimed labels must be distinct');
    }
    const authority = z.object({
      actor: z.string().trim().min(1).max(128),
      readyAt: z.string().datetime({ offset: true }),
      sourceIssue: ReleaseSourceIssueSnapshotContract,
      triage: z.object({
        sourceDigest: z.string().regex(/^[0-9a-f]{64}$/),
        decision: TriageDecisionV1Contract,
        completedAt: z.string().datetime({ offset: true }),
        providerProvenance: TriageProviderProvenanceContract,
      }).strict().optional(),
    }).strict().parse(input.authority);
    const promoted = await this.pool.query<{
      job_id: string | null;
      release_id: string | null;
    }>(
      `SELECT job_id, release_id
         FROM agentops_control.promote_triage_release($1, $2, $3, $4, $5, $6)`,
      [input.token, input.workerId, result, readyLabel, claimedLabel, authority],
    );
    const jobId = promoted.rows[0]?.job_id;
    if (!jobId) {
      throw new LeaseRejectedError('triage promotion lease is stale or lost');
    }
    return jobId;
  }

  /**
   * Revalidates lease ownership and Registration desired state in one locked
   * transaction immediately before a credential-bearing runner boundary. Both
   * allow and deny decisions are committed to runtime_audit before returning.
   */
  async assertExecutionGuard(input: {
    token: string;
    workerId: string;
    boundary: RunnerCriticalBoundary;
  }): Promise<ExecutionGuardVerdict> {
    const boundary = RunnerCriticalBoundary.parse(input.boundary);
    return transaction(this.pool, async (client) => {
      const result = await client.query<{
        lease_status: string | null;
        lease_worker_id: string | null;
        lease_expires_at: Date | null;
        job_id: string | null;
        job_status: string | null;
        registration_id: string | null;
        registration_version: string | null;
        registration_present: boolean;
        current_version: string | null;
        enabled: boolean | null;
        issue_monitor_enabled: boolean | null;
        pr_monitor_enabled: boolean | null;
        execution_enabled: boolean | null;
        repository: string | null;
        configuration: Record<string, unknown> | null;
        created_at: Date | null;
        updated_at: Date | null;
        lease_unexpired: boolean | null;
      }>(
        `SELECT l.status AS lease_status, l.worker_id AS lease_worker_id,
                l.expires_at AS lease_expires_at,
                j.id AS job_id, j.status AS job_status,
                j.registration_id, j.registration_version,
                (r.id IS NOT NULL) AS registration_present,
                r.version AS current_version, r.enabled,
                r.issue_monitor_enabled, r.pr_monitor_enabled,
                r.execution_enabled, r.repository, r.configuration,
                r.created_at, r.updated_at,
                l.expires_at > clock_timestamp() AS lease_unexpired
           FROM agentops_control.job_leases l
           JOIN agentops_control.jobs j ON j.id = l.job_id
           JOIN agentops_control.repository_registrations r
             ON r.id = j.registration_id
          WHERE l.lease_token = $1
          FOR UPDATE OF l, j, r`,
        [input.token],
      );
      const row = result.rows[0];
      let reason: string | null = null;
      if (!row) reason = 'lease_absent';
      else if (row.lease_status !== 'active') reason = `lease_${row.lease_status}`;
      else if (row.lease_worker_id !== input.workerId) reason = 'lease_worker_mismatch';
      else if (!row.lease_unexpired) {
        reason = 'lease_expired';
      } else if (row.job_status !== 'leased') reason = `job_${row.job_status ?? 'absent'}`;
      else if (!row.registration_present) reason = 'registration_absent';
      else if (!row.enabled) reason = 'registration_disabled';
      else if (!row.execution_enabled) reason = 'registration_execution_disabled';
      else if (row.current_version !== row.registration_version) {
        reason = 'registration_version_stale';
      }
      const ok = reason === null;
      await client.query(
        `INSERT INTO agentops_control.runtime_audit(
           actor_type, actor_id, event_type, registration_id, job_id, details
         ) VALUES ('runner', $1, $2, $3, $4, $5)`,
        [
          input.workerId,
          `runner.boundary.${boundary}.${ok ? 'allowed' : 'denied'}`,
          row?.registration_id ?? null,
          row?.job_id ?? null,
          {
            reason,
            leaseStatus: row?.lease_status ?? null,
            leaseWorkerId: row?.lease_worker_id ?? null,
            leaseExpiresAt: row?.lease_expires_at?.toISOString() ?? null,
            jobStatus: row?.job_status ?? null,
            registrationVersion: row?.registration_version
              ? Number(row.registration_version)
              : null,
            currentRegistrationVersion: row?.current_version
              ? Number(row.current_version)
              : null,
            enabled: row?.enabled ?? null,
            executionEnabled: row?.execution_enabled ?? null,
          },
        ],
      );
      return {
        ok,
        reason,
        registration: row?.registration_present && row.repository
          ? {
              id: row.registration_id!,
              repository: row.repository,
              enabled: row.enabled!,
              issueMonitorEnabled: row.issue_monitor_enabled!,
              prMonitorEnabled: row.pr_monitor_enabled!,
              executionEnabled: row.execution_enabled!,
              configuration: persistedRegistrationConfiguration(row.configuration ?? {}),
              version: Number(row.current_version),
              createdAt: row.created_at!.toISOString(),
              updatedAt: row.updated_at!.toISOString(),
            }
          : null,
        jobId: row?.job_id ?? null,
        leaseExpiresAt: row?.lease_expires_at?.toISOString() ?? null,
      };
    });
  }

  /**
   * Ends the current attempt and either requeues it with DB-clock delay or
   * records a terminal typed failure. Stale Registration and lease loss never
   * requeue under an obsolete authorization.
   */
  async failOrRetryLease(input: {
    token: string;
    workerId: string;
    failure: RunnerJobFailureV1;
    retryDelayMs: number;
    maxAttempts: number;
  }): Promise<'queued' | 'failed' | 'rejected' | 'succeeded'> {
    const failure = RunnerJobFailureV1Contract.parse(input.failure);
    if (!Number.isInteger(input.retryDelayMs) || input.retryDelayMs < 0) {
      throw new Error('retryDelayMs must be a non-negative integer');
    }
    if (!Number.isInteger(input.maxAttempts) || input.maxAttempts < 1) {
      throw new Error('maxAttempts must be a positive integer');
    }
    return transaction(this.pool, async (client) => {
      const result = await client.query<{
        lease_id: string;
        attempt_id: string;
        attempt_number: number;
        job_id: string;
        registration_id: string;
        registration_version: string;
        release_id: string | null;
        repository: string;
        current_version: string | null;
        enabled: boolean | null;
        execution_enabled: boolean | null;
      }>(
        `SELECT l.id AS lease_id, l.attempt_id, a.attempt_number,
                j.id AS job_id, j.registration_id, j.registration_version,
                j.release_id, r.repository,
                r.version AS current_version, r.enabled, r.execution_enabled
           FROM agentops_control.job_leases l
           JOIN agentops_control.job_attempts a ON a.id = l.attempt_id
           JOIN agentops_control.jobs j ON j.id = l.job_id
           JOIN agentops_control.repository_registrations r
             ON r.id = j.registration_id
          WHERE l.lease_token = $1
            AND l.worker_id = $2
            AND l.status = 'active'
            AND l.expires_at > clock_timestamp()
            AND j.status = 'leased'
          FOR UPDATE OF l, a, j, r`,
        [input.token, input.workerId],
      );
      const row = result.rows[0];
      if (!row) {
        throw new LeaseRejectedError('lease is absent, inactive, or not owned by worker');
      }
      const durableRelease = row.release_id
        ? await client.query<{
            status: ReleaseRecord['status'];
            final_head: string | null;
            pull_request_number: string | null;
          }>(
            `SELECT status, final_head, pull_request_number
               FROM agentops_control.lock_release_completion_state($1, $2)`,
            [row.job_id, row.release_id],
          )
        : null;
      const mergedRelease = durableRelease?.rows[0]?.status === 'merged'
        ? durableRelease.rows[0]
        : null;
      const registrationCurrent =
        row.enabled === true
        && row.execution_enabled === true
        && row.current_version === row.registration_version;
      const requeue =
        mergedRelease === null
        &&
        failure.retryable
        && row.attempt_number < input.maxAttempts
        && registrationCurrent;
      const finalStatus = mergedRelease
        ? 'succeeded'
        : requeue
        ? 'queued'
        : registrationCurrent
          ? 'failed'
          : 'rejected';
      await client.query(
        `UPDATE agentops_control.job_leases
            SET status = 'released', released_at = clock_timestamp()
          WHERE id = $1`,
        [row.lease_id],
      );
      await client.query(
        `UPDATE agentops_control.job_attempts
            SET status = $2, finished_at = clock_timestamp(),
                error = $3, failure = $4
          WHERE id = $1`,
        [
          row.attempt_id,
          mergedRelease ? 'succeeded' : 'failed',
          mergedRelease ? null : failure.message,
          mergedRelease ? null : failure,
        ],
      );
      const convergedResult = mergedRelease
        ? RunnerJobResultV1Contract.parse({
            schemaVersion: 1,
            status: 'succeeded',
            jobId: row.job_id,
            attemptNumber: row.attempt_number,
            repository: row.repository,
            outcome: 'completed',
            humanReview: null,
            headSha: mergedRelease.final_head,
            pullRequestNumber: mergedRelease.pull_request_number === null
              ? null
              : Number(mergedRelease.pull_request_number),
            artifacts: [],
            completedAt: new Date().toISOString(),
          })
        : null;
      await client.query(
        `UPDATE agentops_control.jobs
            SET status = $2,
                available_at = CASE WHEN $2 = 'queued'
                  THEN clock_timestamp() + ($3 * interval '1 millisecond')
                  ELSE available_at END,
                finished_at = CASE WHEN $2 = 'queued' THEN NULL ELSE clock_timestamp() END,
                updated_at = clock_timestamp(),
                last_error = $4,
                result = $5,
                failure = CASE WHEN $2 IN ('queued', 'succeeded')
                  THEN NULL ELSE $6::jsonb END
          WHERE id = $1`,
        [
          row.job_id,
          finalStatus,
          input.retryDelayMs,
          mergedRelease ? null : failure.message,
          convergedResult,
          failure,
        ],
      );
      await client.query(
        `INSERT INTO agentops_control.runtime_audit(
           actor_type, actor_id, event_type, registration_id, job_id, details
         ) VALUES ('runner', $1, $2, $3, $4, $5)`,
        [
          input.workerId,
          mergedRelease
            ? 'runner.attempt.cleanup_ignored_after_merge'
            : requeue
              ? 'runner.attempt.retry_scheduled'
              : 'runner.attempt.failed',
          row.registration_id,
          row.job_id,
          {
            attemptNumber: row.attempt_number,
            failure,
            finalStatus,
            registrationCurrent,
            retryDelayMs: requeue ? input.retryDelayMs : null,
            releaseId: row.release_id,
            releaseMerged: mergedRelease !== null,
          },
        ],
      );
      return finalStatus;
    });
  }

  async linkLeaseArtifact(input: {
    token: string;
    workerId: string;
    kind: string;
    uri: string;
    sha256: string;
    sizeBytes: number;
  }): Promise<string> {
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(input.kind)) {
      throw new Error('artifact kind is invalid');
    }
    if (!/^[0-9a-f]{64}$/.test(input.sha256)) {
      throw new Error('artifact sha256 is invalid');
    }
    if (!Number.isSafeInteger(input.sizeBytes) || input.sizeBytes < 0) {
      throw new Error('artifact sizeBytes is invalid');
    }
    return transaction(this.pool, async (client) => {
      const lease = await client.query<{
        job_id: string;
        attempt_id: string;
        registration_id: string;
      }>(
        `SELECT l.job_id, l.attempt_id, j.registration_id
           FROM agentops_control.job_leases l
           JOIN agentops_control.jobs j ON j.id = l.job_id
          WHERE l.lease_token = $1 AND l.worker_id = $2
            AND l.status = 'active' AND l.expires_at > clock_timestamp()
            AND j.status = 'leased'
          FOR UPDATE OF l, j`,
        [input.token, input.workerId],
      );
      const row = lease.rows[0];
      if (!row) throw new LeaseRejectedError('artifact write requires active lease ownership');
      const expectedPrefix = `volume://registrations/${row.registration_id}/`;
      if (!input.uri.startsWith(expectedPrefix)) {
        throw new Error(`artifact URI must begin with ${expectedPrefix}`);
      }
      const id = randomUUID();
      const inserted = await client.query<{ id: string }>(
        `INSERT INTO agentops_control.artifact_links(
           id, job_id, attempt_id, kind, uri, sha256, size_bytes
         ) VALUES ($1, $2, $3, $4, $5, $6, $7)
         ON CONFLICT (job_id, attempt_id, kind, uri) DO UPDATE
           SET sha256 = EXCLUDED.sha256, size_bytes = EXCLUDED.size_bytes
         WHERE agentops_control.artifact_links.sha256 = EXCLUDED.sha256
           AND agentops_control.artifact_links.size_bytes = EXCLUDED.size_bytes
         RETURNING id`,
        [
          id,
          row.job_id,
          row.attempt_id,
          input.kind,
          input.uri,
          input.sha256,
          input.sizeBytes,
        ],
      );
      if (!inserted.rows[0]) {
        throw new Error('artifact URI was reused with different digest or size');
      }
      return inserted.rows[0].id;
    });
  }

  async listReconciliationWork(): Promise<ReconciliationWork[]> {
    const result = await this.pool.query<{
      job_id: string;
      registration_id: string;
      repository: string;
      status: 'queued' | 'leased';
      available_at: Date;
      lease_expires_at: Date | null;
    }>(
      `SELECT j.id AS job_id, j.registration_id, r.repository, j.status,
              j.available_at, l.expires_at AS lease_expires_at
         FROM agentops_control.jobs j
         JOIN agentops_control.repository_registrations r ON r.id = j.registration_id
         LEFT JOIN agentops_control.job_leases l
           ON l.job_id = j.id AND l.status = 'active'
        WHERE j.status = 'queued'
           OR (j.status = 'leased' AND l.expires_at <= clock_timestamp())
        ORDER BY j.available_at, j.created_at`,
    );
    return result.rows.map((row) => ({
      jobId: row.job_id,
      registrationId: row.registration_id,
      repository: row.repository,
      status: row.status,
      availableAt: row.available_at.toISOString(),
      leaseExpiresAt: row.lease_expires_at?.toISOString() ?? null,
    }));
  }

  async appendAudit(input: {
    actorType: string;
    actorId: string;
    eventType: string;
    registrationId?: string;
    jobId?: string;
    details?: Record<string, unknown>;
  }): Promise<void> {
    await this.pool.query(
      `INSERT INTO agentops_control.runtime_audit(
         actor_type, actor_id, event_type, registration_id, job_id, details
       ) VALUES ($1, $2, $3, $4, $5, $6)`,
      [
        input.actorType,
        input.actorId,
        input.eventType,
        input.registrationId ?? null,
        input.jobId ?? null,
        input.details ?? {},
      ],
    );
  }

  async linkArtifact(input: {
    jobId?: string;
    attemptId?: string;
    kind: string;
    uri: string;
    sha256: string;
    sizeBytes: number;
  }): Promise<string> {
    const id = randomUUID();
    await this.pool.query(
      `INSERT INTO agentops_control.artifact_links(
         id, job_id, attempt_id, kind, uri, sha256, size_bytes
       ) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
      [
        id,
        input.jobId ?? null,
        input.attemptId ?? null,
        input.kind,
        input.uri,
        input.sha256,
        input.sizeBytes,
      ],
    );
    return id;
  }

  /** LISTEN is only a low-latency hint; callers must also reconcile periodically. */
  async listen(
    channel:
      | 'agentops_job_wake'
      | 'agentops_registration_wake'
      | 'agentops_webhook_wake',
    onNotification: (notification: Notification) => void,
  ): Promise<() => Promise<void>> {
    const client = await this.pool.connect();
    const listener = (notification: Notification): void => {
      if (notification.channel === channel) onNotification(notification);
    };
    client.on('notification', listener);
    try {
      await client.query(`LISTEN ${channel}`);
    } catch (error) {
      client.off('notification', listener);
      client.release();
      throw error;
    }
    return async () => {
      client.off('notification', listener);
      try {
        await client.query(`UNLISTEN ${channel}`);
      } finally {
        client.release();
      }
    };
  }
}
