import { createHash, randomUUID } from 'node:crypto';
import { Pool } from 'pg';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import {
  CONTROL_SCHEMA_VERSION,
  PostgresControlStore,
  ReleaseCertificationError,
  ReleaseReceiptConflictError,
  migrateControlSchema,
  releaseSourceIssueSnapshotDigest,
} from '../src/control-store/index.js';
import type { DurableReleaseReceipt } from '../src/evidence/release-receipt.js';

vi.setConfig({ testTimeout: 20_000 });

function frozenSourceIssue(
  repository: string,
  number: number,
  at: string,
  comments: Array<{
    id: number;
    body: string;
    updatedAt: string;
    url: string;
    author: string;
  }> = [],
) {
  const source = {
    repository,
    number,
    title: `Issue ${number}`,
    body: 'Frozen acceptance requirements.',
    url: `https://github.com/${repository}/issues/${number}`,
    labels: ['ready'],
    comments,
    state: 'open' as const,
    sourceUpdatedAt: at,
    capturedAt: at,
  };
  return { ...source, digest: releaseSourceIssueSnapshotDigest(source) };
}

function deterministicInvocationRef(releaseId: string, invocationKey: string): string {
  const bytes = createHash('sha256')
    .update(`${releaseId}\0${invocationKey}`)
    .digest()
    .subarray(0, 16);
  bytes[6] = (bytes[6]! & 0x0f) | 0x40;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = bytes.toString('hex');
  return `invocation:${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-`
    + `${hex.slice(16, 20)}-${hex.slice(20)}`;
}

const databaseUrl = process.env.AGENTOPS_TEST_DATABASE_URL;
const integration = databaseUrl ? describe.sequential : describe.skip;

integration('PostgreSQL release receipt outbox', () => {
  let pool: Pool;
  let store: PostgresControlStore;

  beforeAll(async () => {
    pool = new Pool({ connectionString: databaseUrl, max: 8 });
    await pool.query('DROP SCHEMA IF EXISTS agentops_control CASCADE');
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
    expect(await migrateControlSchema(pool)).toBe(CONTROL_SCHEMA_VERSION);
    await pool.query(
      'GRANT USAGE ON SCHEMA agentops_control TO agentops_triage, agentops_runner',
    );
    await pool.query(
      `UPDATE agentops_control.lifecycle_state
          SET mode = 'ACTIVE', generation = generation + 1,
              updated_at = clock_timestamp()
        WHERE singleton`,
    );
    store = new PostgresControlStore(pool);
  });

  afterAll(async () => {
    await pool?.end();
  });

  it('crosses split jobs and converges an already completed merge idempotently', async () => {
    const registration = await store.createRegistration({
      repository: 'sample/release-target',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const policy = {
      authority: 'human-ready-allowed' as const,
      requiredGateSignals: [
        { source: 'repository-grader' as const, name: 'unit_tests' as const },
        { source: 'github-check' as const, name: 'contracts' },
      ],
      requiredReviewPerspectives: ['security' as const, 'codeQuality' as const],
      minimumHeadEpochs: 1,
    };
    const created = await store.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:7:release:1',
      repository: registration.repository,
      issueNumber: 7,
      policy,
    });
    const duplicate = await store.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:7:release:1',
      repository: registration.repository,
      issueNumber: 7,
      policy,
    });
    expect(duplicate).toMatchObject({
      duplicate: true,
      release: { id: created.release.id },
    });

    const jobIds: string[] = [];
    for (const key of ['generator-job', 'reconciliation-job']) {
      const job = await store.enqueueJob({
        registrationId: registration.id,
        registrationVersion: registration.version,
        source: { kind: 'manual', key },
        idempotencyKey: key,
        jobType: 'release-receipt-test',
        payload: { key },
      });
      await store.linkJobToRelease({
        jobId: job.job.id,
        releaseId: created.release.id,
      });
      const lease = await store.acquireLease({
        workerId: key,
        durationMs: 10_000,
      });
      expect(lease?.job).toMatchObject({
        id: job.job.id,
        releaseId: created.release.id,
      });
      await store.finishLease(lease!.token, { status: 'succeeded' });
      jobIds.push(job.job.id);
    }

    const releaseId = created.release.id;
    const repository = registration.repository;
    const at = (seconds: number) => new Date(
      Date.parse(created.release.createdAt) + seconds * 1_000,
    ).toISOString();
    const head = 'a'.repeat(40);
    const digest = `sha256:${'b'.repeat(64)}`;
    const authorityId = randomUUID();
    const requirementsAuthorityId = randomUUID();
    const buildId = randomUUID();
    const repositoryGradeId = randomUUID();
    const githubGradeId = randomUUID();
    const securityReviewId = randomUUID();
    const codeQualityReviewId = randomUUID();
    const runtimeId = randomUUID();
    const common = (
      receiptId: string,
      receiptKey: string,
      recordedAt: string,
      causes: string[],
      jobId: string,
    ) => ({
      receiptId,
      receiptKey,
      releaseId,
      repository,
      issueNumber: 7,
      producer: { jobId },
      causes,
      recordedAt,
    });
    const receipts: DurableReleaseReceipt[] = [
      {
        ...common(authorityId, 'authority:human-ready', at(1), [], jobIds[0]!),
        kind: 'authority',
        route: 'human-ready',
        actor: { type: 'human', login: 'maintainer' },
        readyLabel: 'ready',
        readyAt: at(1),
      },
      {
        ...common(
          requirementsAuthorityId,
          'requirements-authority',
          at(1.5),
          [authorityId],
          jobIds[0]!,
        ),
        kind: 'requirements-authority',
        sourceIssueDigest: 'f'.repeat(64),
        sourceUpdatedAt: at(1),
        capturedAt: at(1.5),
      },
      {
        ...common(buildId, 'build:final', at(2), [authorityId], jobIds[0]!),
        kind: 'build',
        head,
        parentHead: null,
        invocationRef: 'generator-1',
        role: 'generator',
      },
      {
        ...common(repositoryGradeId, 'grade:repository:unit_tests', at(3), [buildId], jobIds[1]!),
        kind: 'grade',
        head,
        signal: { source: 'repository-grader', name: 'unit_tests' },
        status: 'passed',
        detailsDigest: digest,
      },
      {
        ...common(githubGradeId, 'grade:github:contracts', at(4), [buildId], jobIds[1]!),
        kind: 'grade',
        head,
        signal: { source: 'github-check', name: 'contracts' },
        status: 'passed',
        detailsDigest: digest,
      },
      {
        ...common(securityReviewId, 'review:1:security', at(5), [buildId], jobIds[1]!),
        kind: 'review',
        head,
        headEpoch: 1,
        perspective: 'security',
        invocationRef: 'review-security',
        verdict: 'approve',
        hasFindings: false,
        findings: [],
      },
      {
        ...common(codeQualityReviewId, 'review:1:codeQuality', at(6), [buildId], jobIds[1]!),
        kind: 'review',
        head,
        headEpoch: 1,
        perspective: 'codeQuality',
        invocationRef: 'review-codeQuality',
        verdict: 'approve',
        hasFindings: false,
        findings: [],
      },
      {
        ...common(runtimeId, 'runtime:release', at(7), [authorityId], jobIds[1]!),
        kind: 'runtime-provenance',
        consumer: {
          repository: 'sample/workflow',
          revision: 'c'.repeat(40),
        },
        environment: {
          kind: 'container',
          reference: 'runner@fixture',
          digest,
        },
        invocations: [
          {
            invocationKey: 'generator-key-1',
            invocationRef: 'generator-1',
            role: 'generator',
            provider: 'codex',
            model: {
              kind: 'provider-default',
              reference: 'codex-cli@fixture:default',
              resolverDigest: digest,
            },
            head,
          },
          ...['security', 'codeQuality'].map((perspective) => ({
            invocationKey: `review-${perspective}-key`,
            invocationRef: `review-${perspective}`,
            role: 'reviewer' as const,
            provider: 'claude',
            model: { kind: 'explicit' as const, name: 'claude-fixture' },
            head,
          })),
        ],
      },
    ];
    for (const receipt of receipts) {
      if (receipt.kind !== 'requirements-authority') {
        await store.recordReleaseReceipt(receipt);
        continue;
      }
      await pool.query(
        `INSERT INTO agentops_control.release_receipt_outbox(
           receipt_id, release_id, receipt_key, kind, repository, issue_number,
           head_sha, causes, payload, recorded_at
         ) VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $8, $9)`,
        [
          receipt.receiptId,
          releaseId,
          receipt.receiptKey,
          receipt.kind,
          repository,
          7,
          receipt.causes,
          receipt,
          receipt.recordedAt,
        ],
      );
    }
    await expect(store.recordReleaseReceipt(receipts[0])).resolves.toMatchObject({
      receipt: { receiptId: authorityId },
    });

    const intent = {
      ...common(randomUUID(), 'merge-intent:21', at(8), receipts.map((receipt) => receipt.receiptId), jobIds[1]!),
      kind: 'merge-intent' as const,
      pullRequestNumber: 21,
      expectedHead: head,
      observedPrHead: head,
    };
    await expect(store.authorizeReleaseMerge({ releaseId, intent }))
      .resolves.toMatchObject({
        id: releaseId,
        status: 'merge-authorized',
        pullRequest: 21,
        finalHead: head,
      });

    const merge = {
      ...common(randomUUID(), 'merge:21', at(9), [intent.receiptId], jobIds[1]!),
      kind: 'merge' as const,
      pullRequestNumber: 21,
      expectedHead: head,
      observedPrHead: head,
      mergeSha: 'd'.repeat(40),
      actor: 'workflow-app[bot]',
      sourceIssueClosure: 'completed' as const,
      mergeReachableFromDefaultBranch: true as const,
      mergedAt: at(9),
    };
    const completed = await store.completeReleaseMerge({ releaseId, receipt: merge });
    expect(completed).toMatchObject({
      status: 'merged',
      finalHead: head,
      mergeSha: 'd'.repeat(40),
    });
    await expect(store.completeReleaseMerge({
      releaseId,
      receipt: {
        ...merge,
        receiptId: randomUUID(),
        receiptKey: 'merge:21:recovery-observation',
        recordedAt: at(10),
      },
    }))
      .resolves.toEqual(completed);

    const receiptIds = (await store.listReleaseReceipts(releaseId, {
      includeMergeIntent: true,
    })).map((entry) => entry.receipt.receiptId);
    const artifact = {
      kind: 'runner-result',
      uri: `volume://registrations/${registration.id}/release/evidence.json`,
      sha256: 'e'.repeat(64),
      sizeBytes: 512,
      releaseId,
      sourceHead: head,
      receiptIds,
    };
    await expect(store.recordReleaseArtifact({
      artifactKey: 'runner-result:final',
      artifact,
    })).resolves.toEqual(artifact);
    await expect(store.recordReleaseArtifact({
      artifactKey: 'runner-result:final',
      artifact,
    })).resolves.toEqual(artifact);
    await expect(pool.query(
      `UPDATE agentops_control.release_artifacts
          SET uri = $2
        WHERE release_id = $1 AND artifact_key = 'runner-result:final'`,
      [releaseId, `${artifact.uri}.mutated`],
    )).rejects.toThrow(/immutable/);
    await expect(pool.query(
      `DELETE FROM agentops_control.release_receipt_outbox
        WHERE receipt_id = $1`,
      [authorityId],
    )).rejects.toThrow(/immutable/);
    await expect(store.exportReleaseEvidence(releaseId)).resolves.toMatchObject({
      schemaVersion: '4.0',
      release: {
        id: releaseId,
        pullRequestNumber: 21,
        finalHead: head,
        mergeSha: 'd'.repeat(40),
      },
      receipts: {
        mergeIntent: { receiptId: intent.receiptId },
        merge: { receiptId: merge.receiptId },
      },
      artifacts: [artifact],
      result: 'passed',
    });

    const cleanupJob = await store.enqueueJob({
      registrationId: registration.id,
      registrationVersion: registration.version,
      source: { kind: 'recovery', key: 'cleanup-after-merge' },
      idempotencyKey: 'cleanup-after-merge',
      jobType: 'release-receipt-test',
      payload: { releaseId },
    });
    await store.linkJobToRelease({
      jobId: cleanupJob.job.id,
      releaseId,
    });
    const cleanupLease = await store.acquireLease({
      workerId: 'cleanup-after-merge',
      durationMs: 10_000,
    });
    await expect(store.failOrRetryLease({
      token: cleanupLease!.token,
      workerId: 'cleanup-after-merge',
      failure: {
        schemaVersion: 1,
        status: 'failed',
        code: 'release_failure',
        message: 'post-merge cleanup was interrupted',
        retryable: true,
        boundary: 'release',
        observedAt: '2026-08-01T00:08:00Z',
      },
      retryDelayMs: 0,
      maxAttempts: 3,
    })).resolves.toBe('succeeded');
    const converged = await pool.query<{
      job_status: string;
      attempt_status: string;
      result: Record<string, unknown>;
      event_type: string;
    }>(
      `SELECT j.status AS job_status, a.status AS attempt_status, j.result,
              audit.event_type
         FROM agentops_control.jobs j
         JOIN agentops_control.job_attempts a ON a.job_id = j.id
         JOIN agentops_control.runtime_audit audit ON audit.job_id = j.id
        WHERE j.id = $1
          AND audit.event_type = 'runner.attempt.cleanup_ignored_after_merge'`,
      [cleanupJob.job.id],
    );
    expect(converged.rows[0]).toMatchObject({
      job_status: 'succeeded',
      attempt_status: 'succeeded',
      result: {
        status: 'succeeded',
        headSha: head,
        pullRequestNumber: 21,
      },
      event_type: 'runner.attempt.cleanup_ignored_after_merge',
    });

    const outbox = await store.listReleaseReceipts(releaseId, {
      includeMergeIntent: true,
    });
    expect(outbox.map((entry) => entry.receipt.kind)).toEqual([
      'authority',
      'requirements-authority',
      'build',
      'grade',
      'grade',
      'review',
      'review',
      'runtime-provenance',
      'merge-intent',
      'merge',
    ]);
    expect(outbox.every((entry) => entry.publishedAt === null)).toBe(true);
    await store.markReleaseReceiptPublished(authorityId);
    expect((await store.listReleaseReceipts(releaseId, {
      includeMergeIntent: true,
    }))[0]?.publishedAt).not.toBeNull();
  });

  it('reads, recovers, and exports a persisted v2 grader policy without rewriting it', async () => {
    const registration = await store.createRegistration({
      repository: 'sample/historical-release',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const created = await store.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:70:release:v2',
      repository: registration.repository,
      issueNumber: 70,
      policy: {
        authority: 'human-ready-allowed',
        requiredGateSignals: [{ source: 'repository-grader', name: 'unit_tests' }],
        requiredReviewPerspectives: ['security', 'codeQuality'],
        minimumHeadEpochs: 1,
      },
    });
    const legacyPolicy = {
      authority: 'human-ready-allowed',
      requiredGateSignals: [{ source: 'repository-grader', name: 'contracts' }],
      requiredReviewPerspectives: ['security', 'codeQuality'],
      minimumHeadEpochs: 1,
    };
    await pool.query(
      'UPDATE agentops_control.releases SET policy = $2 WHERE id = $1',
      [created.release.id, legacyPolicy],
    );

    await expect(store.getRelease(created.release.id)).resolves.toMatchObject({
      id: created.release.id,
      policy: legacyPolicy,
    });
    await expect(store.findReleaseForRunnerEvent({
      registrationId: registration.id,
      issueNumber: 70,
    })).resolves.toMatchObject({
      id: created.release.id,
      policy: legacyPolicy,
    });

    const releaseId = created.release.id;
    const repository = registration.repository;
    const head = '1'.repeat(40);
    const mergeSha = '2'.repeat(40);
    const digest = `sha256:${'3'.repeat(64)}`;
    const at = (seconds: number) => new Date(
      Date.parse(created.release.createdAt) + seconds * 1_000,
    ).toISOString();
    const authorityId = randomUUID();
    const runtimeId = randomUUID();
    const buildId = randomUUID();
    const gradeId = randomUUID();
    const securityReviewId = randomUUID();
    const codeQualityReviewId = randomUUID();
    const intentId = randomUUID();
    const common = (
      receiptId: string,
      receiptKey: string,
      recordedAt: string,
      causes: string[],
    ) => ({
      receiptId,
      receiptKey,
      releaseId,
      repository,
      issueNumber: 70,
      producer: {},
      causes,
      recordedAt,
    });
    const authority = {
      ...common(authorityId, 'authority:historical-human-ready', at(1), []),
      kind: 'authority' as const,
      route: 'human-ready' as const,
      actor: { type: 'human' as const, login: 'maintainer' },
      readyLabel: 'ready',
      readyAt: at(1),
    };
    const runtime = {
      ...common(runtimeId, 'runtime:historical-v2', at(2), [authorityId]),
      kind: 'runtime-provenance' as const,
      consumer: { repository: 'mrbaron3/servo', revision: '4'.repeat(40) },
      environment: {
        kind: 'container' as const,
        reference: 'runner@historical-v2',
        digest,
      },
      invocations: [{
        invocationId: 'historical-generator',
        role: 'generator' as const,
        provider: 'codex',
        model: { kind: 'explicit' as const, name: 'historical-generator' },
        head,
      }, {
        invocationId: 'historical-security-review',
        role: 'reviewer' as const,
        provider: 'claude',
        model: { kind: 'explicit' as const, name: 'historical-reviewer' },
        head,
      }, {
        invocationId: 'historical-code-quality-review',
        role: 'reviewer' as const,
        provider: 'claude',
        model: { kind: 'explicit' as const, name: 'historical-reviewer' },
        head,
      }],
    };
    const build = {
      ...common(buildId, 'build:historical-final', at(3), [authorityId]),
      kind: 'build' as const,
      head,
      parentHead: null,
      invocationId: 'historical-generator',
      role: 'generator' as const,
    };
    const grade = {
      ...common(gradeId, 'grade:repository:contracts', at(4), [buildId]),
      kind: 'grade' as const,
      head,
      signal: { source: 'repository-grader' as const, name: 'contracts' },
      status: 'passed' as const,
      detailsDigest: digest,
    };
    const securityReview = {
      ...common(securityReviewId, 'review:1:security', at(5), [buildId]),
      kind: 'review' as const,
      head,
      headEpoch: 1,
      perspective: 'security',
      invocationId: 'historical-security-review',
      verdict: 'approved' as const,
      findings: [],
    };
    const codeQualityReview = {
      ...common(codeQualityReviewId, 'review:1:codeQuality', at(6), [buildId]),
      kind: 'review' as const,
      head,
      headEpoch: 1,
      perspective: 'codeQuality',
      invocationId: 'historical-code-quality-review',
      verdict: 'approved' as const,
      findings: [],
    };
    for (const receipt of [authority, runtime, build, securityReview, codeQualityReview]) {
      await store.recordReleaseReceipt(receipt);
    }

    const insertHistoricalReceipt = async (
      receipt: Record<string, unknown> & {
        receiptId: string;
        receiptKey: string;
        kind: string;
        causes: string[];
        recordedAt: string;
      },
      receiptHead: string | null,
    ) => {
      await pool.query(
        `INSERT INTO agentops_control.release_receipt_outbox(
           receipt_id, release_id, receipt_key, kind, repository, issue_number,
           head_sha, causes, payload, recorded_at
         ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
        [
          receipt.receiptId,
          releaseId,
          receipt.receiptKey,
          receipt.kind,
          repository,
          70,
          receiptHead,
          receipt.causes,
          receipt,
          receipt.recordedAt,
        ],
      );
    };
    await insertHistoricalReceipt(grade, head);
    expect((await store.listReleaseReceipts(releaseId)).find(
      (entry) => entry.receipt.kind === 'grade',
    )?.receipt).toMatchObject({
      signal: { source: 'repository-grader', name: 'contracts' },
    });

    const intent = {
      ...common(intentId, 'merge-intent:70:historical', at(7), [
        authorityId,
        runtimeId,
        buildId,
        gradeId,
        securityReviewId,
        codeQualityReviewId,
      ]),
      kind: 'merge-intent' as const,
      pullRequestNumber: 70,
      expectedHead: head,
      observedPrHead: head,
    };
    await insertHistoricalReceipt(intent, head);
    await pool.query(
      `UPDATE agentops_control.releases
          SET status = 'merge-authorized', pull_request_number = 70,
              final_head = $2, updated_at = clock_timestamp()
        WHERE id = $1`,
      [releaseId, head],
    );
    const merge = {
      ...common(randomUUID(), 'merge:70:historical', at(8), [intentId]),
      kind: 'merge' as const,
      pullRequestNumber: 70,
      expectedHead: head,
      observedPrHead: head,
      mergeSha,
      actor: 'workflow-app[bot]',
      sourceIssueClosure: 'completed' as const,
      mergeReachableFromDefaultBranch: true as const,
      mergedAt: at(8),
    };
    await expect(store.completeReleaseMerge({ releaseId, receipt: merge }))
      .resolves.toMatchObject({ status: 'merged', policy: legacyPolicy });
    await expect(store.findReleaseForRunnerEvent({
      registrationId: registration.id,
      issueNumber: 70,
      includeMerged: true,
    })).resolves.toMatchObject({ id: releaseId, policy: legacyPolicy });

    const receiptIds = (await store.listReleaseReceipts(releaseId, {
      includeMergeIntent: true,
    })).map((entry) => entry.receipt.receiptId);
    const artifact = {
      kind: 'runner-result',
      uri: `volume://historical/${releaseId}/runner-result.json`,
      sha256: '5'.repeat(64),
      sizeBytes: 512,
      releaseId,
      sourceHead: head,
      receiptIds,
    };
    await store.recordReleaseArtifact({
      artifactKey: 'runner-result:historical-v2',
      artifact,
    });
    await expect(store.exportReleaseEvidence(releaseId)).resolves.toMatchObject({
      schemaVersion: '2.0',
      policy: legacyPolicy,
      receipts: {
        grades: [{ signal: { source: 'repository-grader', name: 'contracts' } }],
      },
      artifacts: [artifact],
    });
  });

  it('authorizes and exports persisted v3 grader aliases without rewriting them', async () => {
    const registration = await store.createRegistration({
      repository: 'sample/historical-release-v3',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const created = await store.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:71:release:v3',
      repository: registration.repository,
      issueNumber: 71,
      policy: {
        authority: 'human-ready-allowed',
        requiredGateSignals: [{ source: 'repository-grader', name: 'unit_tests' }],
        requiredReviewPerspectives: ['security', 'codeQuality'],
        minimumHeadEpochs: 1,
      },
    });
    const historicalPolicy = {
      authority: 'human-ready-allowed',
      requiredGateSignals: [{ source: 'repository-grader', name: 'contracts' }],
      requiredReviewPerspectives: ['security', 'codeQuality'],
      minimumHeadEpochs: 1,
    };
    await pool.query(
      'UPDATE agentops_control.releases SET policy = $2 WHERE id = $1',
      [created.release.id, historicalPolicy],
    );

    const releaseId = created.release.id;
    const repository = registration.repository;
    const head = '6'.repeat(40);
    const mergeSha = '7'.repeat(40);
    const digest = `sha256:${'8'.repeat(64)}`;
    const at = (seconds: number) => new Date(
      Date.parse(created.release.createdAt) + seconds * 1_000,
    ).toISOString();
    const authorityId = randomUUID();
    const requirementsAuthorityId = randomUUID();
    const runtimeId = randomUUID();
    const buildId = randomUUID();
    const gradeId = randomUUID();
    const securityReviewId = randomUUID();
    const codeQualityReviewId = randomUUID();
    const common = (
      receiptId: string,
      receiptKey: string,
      recordedAt: string,
      causes: string[],
    ) => ({
      receiptId,
      receiptKey,
      releaseId,
      repository,
      issueNumber: 71,
      producer: {},
      causes,
      recordedAt,
    });
    const authority = {
      ...common(authorityId, 'authority:historical-v3', at(1), []),
      kind: 'authority' as const,
      route: 'human-ready' as const,
      actor: { type: 'human' as const, login: 'maintainer' },
      readyLabel: 'ready',
      readyAt: at(1),
    };
    const requirementsAuthority = {
      ...common(
        requirementsAuthorityId,
        'requirements-authority:historical-v3',
        at(2),
        [authorityId],
      ),
      kind: 'requirements-authority' as const,
      sourceIssueDigest: '9'.repeat(64),
      sourceUpdatedAt: at(1),
      capturedAt: at(2),
    };
    const runtime = {
      ...common(runtimeId, 'runtime:historical-v3', at(3), [authorityId]),
      kind: 'runtime-provenance' as const,
      consumer: { repository: 'mrbaron3/servo', revision: 'a'.repeat(40) },
      environment: {
        kind: 'container' as const,
        reference: 'runner@historical-v3',
        digest,
      },
      invocations: [{
        invocationId: 'historical-v3-generator',
        role: 'generator' as const,
        provider: 'codex',
        model: { kind: 'explicit' as const, name: 'historical-v3-generator' },
        head,
      }, {
        invocationId: 'historical-v3-security-review',
        role: 'reviewer' as const,
        provider: 'claude',
        model: { kind: 'explicit' as const, name: 'historical-v3-reviewer' },
        head,
      }, {
        invocationId: 'historical-v3-code-quality-review',
        role: 'reviewer' as const,
        provider: 'claude',
        model: { kind: 'explicit' as const, name: 'historical-v3-reviewer' },
        head,
      }],
    };
    const build = {
      ...common(buildId, 'build:historical-v3', at(4), [authorityId]),
      kind: 'build' as const,
      head,
      parentHead: null,
      invocationId: 'historical-v3-generator',
      role: 'generator' as const,
    };
    const historicalGrade = {
      ...common(gradeId, 'grade:repository:contracts:v3', at(5), [buildId]),
      kind: 'grade' as const,
      head,
      signal: { source: 'repository-grader' as const, name: 'contracts' },
      status: 'passed' as const,
      detailsDigest: digest,
    };
    const securityReview = {
      ...common(securityReviewId, 'review:v3:security', at(6), [buildId]),
      kind: 'review' as const,
      head,
      headEpoch: 1,
      perspective: 'security',
      invocationId: 'historical-v3-security-review',
      verdict: 'approved' as const,
      findings: [],
    };
    const codeQualityReview = {
      ...common(codeQualityReviewId, 'review:v3:codeQuality', at(7), [buildId]),
      kind: 'review' as const,
      head,
      headEpoch: 1,
      perspective: 'codeQuality',
      invocationId: 'historical-v3-code-quality-review',
      verdict: 'approved' as const,
      findings: [],
    };
    for (const receipt of [authority, runtime, build, securityReview, codeQualityReview]) {
      await store.recordReleaseReceipt(receipt);
    }
    await expect(store.recordReleaseReceipt(historicalGrade)).rejects.toThrow();

    const insertHistoricalReceipt = async (
      receipt: Record<string, unknown> & {
        receiptId: string;
        receiptKey: string;
        kind: string;
        causes: string[];
        recordedAt: string;
      },
      receiptHead: string | null,
    ) => {
      await pool.query(
        `INSERT INTO agentops_control.release_receipt_outbox(
           receipt_id, release_id, receipt_key, kind, repository, issue_number,
           head_sha, causes, payload, recorded_at
         ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
        [
          receipt.receiptId,
          releaseId,
          receipt.receiptKey,
          receipt.kind,
          repository,
          71,
          receiptHead,
          receipt.causes,
          receipt,
          receipt.recordedAt,
        ],
      );
    };
    await insertHistoricalReceipt(requirementsAuthority, null);
    await insertHistoricalReceipt(historicalGrade, head);

    await expect(store.getRelease(releaseId)).resolves.toMatchObject({
      id: releaseId,
      policy: historicalPolicy,
    });
    const intent = {
      ...common(randomUUID(), 'merge-intent:71:historical-v3', at(8), [
        authorityId,
        requirementsAuthorityId,
        runtimeId,
        buildId,
        gradeId,
        securityReviewId,
        codeQualityReviewId,
      ]),
      kind: 'merge-intent' as const,
      pullRequestNumber: 71,
      expectedHead: head,
      observedPrHead: head,
    };
    await expect(store.authorizeReleaseMerge({ releaseId, intent }))
      .resolves.toMatchObject({
        id: releaseId,
        status: 'merge-authorized',
        policy: historicalPolicy,
      });
    const merge = {
      ...common(randomUUID(), 'merge:71:historical-v3', at(9), [intent.receiptId]),
      kind: 'merge' as const,
      pullRequestNumber: 71,
      expectedHead: head,
      observedPrHead: head,
      mergeSha,
      actor: 'workflow-app[bot]',
      sourceIssueClosure: 'completed' as const,
      mergeReachableFromDefaultBranch: true as const,
      mergedAt: at(9),
    };
    await expect(store.completeReleaseMerge({ releaseId, receipt: merge }))
      .resolves.toMatchObject({ status: 'merged', policy: historicalPolicy });

    const receiptIds = (await store.listReleaseReceipts(releaseId, {
      includeMergeIntent: true,
    })).map((entry) => entry.receipt.receiptId);
    const artifact = {
      kind: 'runner-result',
      uri: `volume://historical/${releaseId}/runner-result-v3.json`,
      sha256: 'b'.repeat(64),
      sizeBytes: 768,
      releaseId,
      sourceHead: head,
      receiptIds,
    };
    await store.recordReleaseArtifact({
      artifactKey: 'runner-result:historical-v3',
      artifact,
    });
    await expect(store.exportReleaseEvidence(releaseId)).resolves.toMatchObject({
      schemaVersion: '3.0',
      policy: historicalPolicy,
      receipts: {
        requirementsAuthority: { receiptId: requirementsAuthorityId },
        grades: [{ signal: { source: 'repository-grader', name: 'contracts' } }],
      },
      artifacts: [artifact],
    });
  });

  it('fails closed for a required AI authority and for mixed release identity', async () => {
    const registration = await store.createRegistration({
      repository: 'sample/strict-target',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const created = await store.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:8:release:1',
      repository: registration.repository,
      issueNumber: 8,
      policy: {
        authority: 'ai-triage-required',
        requiredGateSignals: [{ source: 'github-check', name: 'test' }],
        requiredReviewPerspectives: ['security', 'codeQuality'],
        minimumHeadEpochs: 1,
      },
    });
    const strictAt = (seconds: number) => new Date(
      Date.parse(created.release.createdAt) + seconds * 1_000,
    ).toISOString();
    const authority = {
      receiptId: randomUUID(),
      receiptKey: 'authority:human-ready',
      releaseId: created.release.id,
      repository: registration.repository,
      issueNumber: 8,
      producer: {},
      causes: [],
      recordedAt: strictAt(1),
      kind: 'authority' as const,
      route: 'human-ready' as const,
      actor: { type: 'human' as const, login: 'maintainer' },
      readyLabel: 'ready',
      readyAt: strictAt(1),
    };
    await store.recordReleaseReceipt(authority);
    const runner = await pool.connect();
    try {
      await runner.query('SET ROLE agentops_runner');
      await expect(runner.query(
        `SELECT agentops_control.authorize_release_merge($1)`,
        [{
          receiptId: randomUUID(),
          receiptKey: 'merge-intent:direct-without-requirements',
          releaseId: created.release.id,
          repository: registration.repository,
          issueNumber: 8,
          producer: {},
          causes: [authority.receiptId],
          recordedAt: strictAt(2),
          kind: 'merge-intent',
          pullRequestNumber: 22,
          expectedHead: 'e'.repeat(40),
          observedPrHead: 'e'.repeat(40),
        }],
      )).rejects.toThrow(/requirements receipt/);
    } finally {
      await runner.query('RESET ROLE');
      runner.release();
    }
    await expect(store.recordReleaseReceipt({
      ...authority,
      receiptId: randomUUID(),
      receiptKey: 'authority:mixed',
      releaseId: randomUUID(),
    })).rejects.toThrow();
    await expect(store.authorizeReleaseMerge({
      releaseId: created.release.id,
      intent: {
        receiptId: randomUUID(),
        receiptKey: 'merge-intent:22',
        releaseId: created.release.id,
        repository: registration.repository,
        issueNumber: 8,
        producer: {},
        causes: [authority.receiptId],
        recordedAt: strictAt(2),
        kind: 'merge-intent',
        pullRequestNumber: 22,
        expectedHead: 'e'.repeat(40),
        observedPrHead: 'e'.repeat(40),
      },
    })).rejects.toBeInstanceOf(ReleaseCertificationError);
    await expect(store.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:8:release:changed',
      repository: registration.repository,
      issueNumber: 8,
      policy: {
        authority: 'human-ready-allowed',
        requiredGateSignals: [{ source: 'github-check', name: 'test' }],
        requiredReviewPerspectives: ['security', 'codeQuality'],
        minimumHeadEpochs: 1,
      },
    })).rejects.toBeInstanceOf(ReleaseReceiptConflictError);
  });

  it('creates and links a direct-human release during atomic promotion', async () => {
    const policy = {
      authority: 'human-ready-allowed' as const,
      requiredGateSignals: [{ source: 'github-check' as const, name: 'test' }],
      requiredReviewPerspectives: ['security', 'codeQuality'],
      minimumHeadEpochs: 1,
    };
    const registration = await store.createRegistration({
      repository: 'sample/promotion-v2',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: { releaseEvidence: policy },
    });
    const queued = await store.enqueueJob({
      registrationId: registration.id,
      registrationVersion: registration.version,
      source: { kind: 'poll', key: 'issue:31' },
      idempotencyKey: 'issue:31',
      jobType: 'agentops.triage',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'promotion-v2' },
        issue: {
          number: 31,
          observedUpdatedAt: '2026-08-01T00:00:00Z',
        },
      },
    });
    const lease = await store.acquireLease({
      workerId: 'triage-direct-human',
      durationMs: 10_000,
      jobType: 'agentops.triage',
    });
    const result = {
      schemaVersion: 1,
      status: 'succeeded',
      jobId: queued.job.id,
      attemptNumber: lease!.attemptNumber,
      repository: registration.repository,
      issueNumber: 31,
      outcome: 'promoted',
      sourceDigest: null,
      decision: null,
      commentUrl: null,
      appliedLabels: [],
      promotedJobId: null,
      providerProvenance: null,
      completedAt: '2026-08-01T00:00:31Z',
    };
    const triage = await pool.connect();
    let promotedJobId: string;
    try {
      await triage.query('SET ROLE agentops_triage');
      const promoted = await triage.query<{ job_id: string }>(
        `SELECT job_id
           FROM agentops_control.promote_triage_release($1, $2, $3, $4, $5, $6)`,
        [
          lease!.token,
          'triage-direct-human',
          result,
          'ready',
          'claimed',
          {
            actor: 'product-owner',
            readyAt: '2026-08-01T00:00:30Z',
            sourceIssue: frozenSourceIssue(
              registration.repository,
              31,
              '2026-08-01T00:00:29Z',
              [{
                id: 101,
                body: 'Frozen acceptance clarification.',
                updatedAt: '2026-08-01T00:00:28Z',
                url: 'https://github.com/sample/promotion-v2/issues/31#issuecomment-101',
                author: 'product-owner',
              }],
            ),
          },
        ],
      );
      promotedJobId = promoted.rows[0]!.job_id;
    } finally {
      await triage.query('RESET ROLE');
      triage.release();
    }
    const linked = await pool.query<{
      id: string;
      release_id: string;
      status: string;
    }>(
      `SELECT id, release_id, status
         FROM agentops_control.jobs
        WHERE id = ANY($1::uuid[])
        ORDER BY id`,
      [[queued.job.id, promotedJobId]],
    );
    expect(new Set(linked.rows.map((row) => row.release_id)).size).toBe(1);
    expect(linked.rows).toEqual(expect.arrayContaining([
      expect.objectContaining({ id: queued.job.id, status: 'succeeded' }),
      expect.objectContaining({ id: promotedJobId, status: 'queued' }),
    ]));
    await expect(store.getReleaseSourceIssue(linked.rows[0]!.release_id))
      .resolves.toMatchObject({
        repository: registration.repository,
        number: 31,
        body: 'Frozen acceptance requirements.',
        comments: [expect.objectContaining({
          id: 101,
          body: 'Frozen acceptance clarification.',
        })],
        digest: expect.stringMatching(/^[0-9a-f]{64}$/),
      });
    const runner = await pool.connect();
    try {
      await runner.query('SET ROLE agentops_runner');
      const locked = await runner.query<{
        status: string;
        final_head: string | null;
        pull_request_number: string | null;
      }>(
        `SELECT status, final_head, pull_request_number
           FROM agentops_control.lock_release_completion_state($1, $2)`,
        [promotedJobId, linked.rows[0]!.release_id],
      );
      expect(locked.rows).toEqual([{
        status: 'collecting',
        final_head: null,
        pull_request_number: null,
      }]);
      const bound = await runner.query<{ pull_request_number: string }>(
        `SELECT agentops_control.bind_release_pull_request($1, $2, $3)
           AS pull_request_number`,
        [promotedJobId, linked.rows[0]!.release_id, 91],
      );
      expect(bound.rows).toEqual([{ pull_request_number: '91' }]);
      const observed = await runner.query<{ head_epoch: number }>(
        `SELECT agentops_control.observe_release_head($1, $2, $3)
           AS head_epoch`,
        [linked.rows[0]!.release_id, 'a'.repeat(40), null],
      );
      expect(observed.rows).toEqual([{ head_epoch: 1 }]);
    } finally {
      await runner.query('RESET ROLE');
      runner.release();
    }
    const receipt = (await store.listReleaseReceipts(
      linked.rows[0]!.release_id,
    ))[0]!.receipt;
    expect(receipt).toMatchObject({
      kind: 'authority',
      route: 'human-ready',
      actor: { type: 'human', login: 'product-owner' },
    });
    expect(receipt).not.toHaveProperty('triageInvocationId');
    await expect(store.findReleaseForRunnerEvent({
      registrationId: registration.id,
      pullRequest: 91,
    })).resolves.toMatchObject({ id: linked.rows[0]!.release_id });
    const cleanupLease = await store.acquireLease({
      workerId: 'runner-direct-human-cleanup',
      durationMs: 10_000,
      jobType: 'agentops.runner',
    });
    expect(cleanupLease?.job.id).toBe(promotedJobId);
    await store.finishLease(cleanupLease!.token, { status: 'succeeded' });
  });

  it('reuses unchanged requirements and rotates release identity when requirements change', async () => {
    const policy = {
      authority: 'human-ready-allowed' as const,
      requiredGateSignals: [{ source: 'github-check' as const, name: 'test' }],
      requiredReviewPerspectives: ['security' as const, 'codeQuality' as const],
      minimumHeadEpochs: 1,
    };
    let registration = await store.createRegistration({
      repository: 'sample/open-release-retry',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: { releaseEvidence: policy },
    });
    const promote = async (
      key: string,
      workerId: string,
      observedUpdatedAt: string,
      readyAt: string,
      completedAt: string,
      sourceBody?: string,
    ) => {
      const queued = await store.enqueueJob({
        registrationId: registration.id,
        registrationVersion: registration.version,
        source: { kind: 'poll', key },
        idempotencyKey: key,
        jobType: 'agentops.triage',
        payload: {
          schemaVersion: 1,
          repository: { owner: 'sample', name: 'open-release-retry' },
          issue: { number: 34, observedUpdatedAt },
        },
      });
      const lease = await store.acquireLease({
        workerId,
        durationMs: 10_000,
        jobType: 'agentops.triage',
      });
      const baseSourceIssue = frozenSourceIssue(
        registration.repository,
        34,
        '2026-08-01T00:00:00Z',
      );
      const sourceIssue = sourceBody === undefined
        ? baseSourceIssue
        : (() => {
            const { digest: _digest, ...source } = baseSourceIssue;
            const changed = { ...source, body: sourceBody };
            return {
              ...changed,
              digest: releaseSourceIssueSnapshotDigest(changed),
            };
          })();
      const promotedJobId = await store.promoteTriageLease({
        token: lease!.token,
        workerId,
        readyLabel: 'ready',
        claimedLabel: 'claimed',
        authority: {
          actor: 'product-owner',
          readyAt,
          sourceIssue,
        },
        result: {
          schemaVersion: 1,
          status: 'succeeded',
          jobId: queued.job.id,
          attemptNumber: lease!.attemptNumber,
          repository: registration.repository,
          issueNumber: 34,
          outcome: 'promoted',
          sourceDigest: null,
          decision: null,
          commentUrl: null,
          appliedLabels: [],
          promotedJobId: null,
          providerProvenance: null,
          completedAt,
        },
      });
      return { promotedJobId, triageJobId: queued.job.id };
    };

    const first = await promote(
      'issue:34:first',
      'triage-open-release-first',
      '2026-08-01T00:00:00Z',
      '2026-08-01T00:00:30Z',
      '2026-08-01T00:00:31Z',
    );
    const firstRunner = await store.acquireLease({
      workerId: 'runner-open-release-first',
      durationMs: 10_000,
      jobType: 'agentops.runner',
    });
    const releaseId = firstRunner!.job.releaseId!;
    await expect(store.failOrRetryLease({
      token: firstRunner!.token,
      workerId: 'runner-open-release-first',
      failure: {
        schemaVersion: 1,
        status: 'failed',
        code: 'provider_failure',
        message: 'runner infrastructure interrupted the first attempt',
        retryable: false,
        boundary: 'provider',
        observedAt: '2026-08-01T00:01:00Z',
      },
      retryDelayMs: 0,
      maxAttempts: 1,
    })).resolves.toBe('failed');

    const configuredPolicy = {
      authority: 'human-ready-allowed' as const,
      requiredGateSignals: [
        { source: 'github-check' as const, name: 'test-v2' },
      ],
      requiredReviewPerspectives: [
        'security' as const,
        'testQuality' as const,
      ],
      minimumHeadEpochs: 2,
    };
    registration = await store.updateRegistration(registration.id, {
      configuration: { releaseEvidence: configuredPolicy },
    });

    const second = await promote(
      'issue:34:retry',
      'triage-open-release-retry',
      '2026-08-01T00:02:00Z',
      '2026-08-01T00:02:30Z',
      '2026-08-01T00:02:31Z',
    );
    const linked = await pool.query<{ id: string; release_id: string }>(
      `SELECT id, release_id
         FROM agentops_control.jobs
        WHERE id = ANY($1::uuid[])
        ORDER BY id`,
      [[first.triageJobId, second.triageJobId, second.promotedJobId]],
    );
    expect(new Set(linked.rows.map((row) => row.release_id)))
      .toEqual(new Set([releaseId]));
    const receipts = await store.listReleaseReceipts(releaseId);
    expect(receipts).toHaveLength(2);
    expect(receipts.find((entry) => entry.receipt.kind === 'authority')?.receipt).toMatchObject({
      kind: 'authority',
      route: 'human-ready',
    });
    expect(receipts.find(
      (entry) => entry.receipt.kind === 'requirements-authority',
    )?.receipt).toMatchObject({
      kind: 'requirements-authority',
    });
    await expect(store.getRelease(releaseId)).resolves.toMatchObject({
      policy,
    });
    const audit = await pool.query<{ event_type: string; details: Record<string, unknown> }>(
      `SELECT event_type, details
         FROM agentops_control.runtime_audit
        WHERE job_id = $1 AND event_type = 'release.identity.reused'`,
      [second.triageJobId],
    );
    expect(audit.rows).toEqual([expect.objectContaining({
      event_type: 'release.identity.reused',
      details: expect.objectContaining({
        releaseId,
        configuredPolicyChanged: true,
        reason: 'retry-open-release',
      }),
    })]);

    const cleanup = await store.acquireLease({
      workerId: 'runner-open-release-retry',
      durationMs: 10_000,
      jobType: 'agentops.runner',
    });
    expect(cleanup?.job.id).toBe(second.promotedJobId);
    await store.finishLease(cleanup!.token, { status: 'succeeded' });

    const changedBody = 'Frozen acceptance requirements with a new mandatory constraint.';
    const third = await promote(
      'issue:34:requirements-changed',
      'triage-open-release-changed',
      '2026-08-01T00:03:00Z',
      '2026-08-01T00:03:30Z',
      '2026-08-01T00:03:31Z',
      changedBody,
    );
    const rotated = await pool.query<{ release_id: string }>(
      `SELECT release_id
         FROM agentops_control.jobs
        WHERE id = $1`,
      [third.promotedJobId],
    );
    const rotatedReleaseId = rotated.rows[0]!.release_id;
    expect(rotatedReleaseId).not.toBe(releaseId);
    await expect(store.getRelease(releaseId)).resolves.toMatchObject({
      status: 'abandoned',
      policy,
    });
    await expect(store.getRelease(rotatedReleaseId)).resolves.toMatchObject({
      status: 'collecting',
      policy: configuredPolicy,
    });
    await expect(store.getReleaseSourceIssue(rotatedReleaseId)).resolves.toMatchObject({
      body: changedBody,
    });
    const rotatedReceipts = await store.listReleaseReceipts(rotatedReleaseId);
    expect(rotatedReceipts.map((entry) => entry.receipt.kind).sort()).toEqual([
      'authority',
      'requirements-authority',
    ]);
    const changedAudit = await pool.query<{ details: Record<string, unknown> }>(
      `SELECT details
         FROM agentops_control.runtime_audit
        WHERE event_type = 'release.abandoned.requirements-changed'
          AND details->>'releaseId' = $1`,
      [releaseId],
    );
    expect(changedAudit.rows).toEqual([expect.objectContaining({
      details: expect.objectContaining({
        reason: 'new-human-ready-requirements-authority',
      }),
    })]);
    const rotatedCleanup = await store.acquireLease({
      workerId: 'runner-open-release-changed',
      durationMs: 10_000,
      jobType: 'agentops.runner',
    });
    expect(rotatedCleanup?.job.id).toBe(third.promotedJobId);
    await store.finishLease(rotatedCleanup!.token, { status: 'succeeded' });
  });

  it('abandons a planning human-review release before a fresh authorization', async () => {
    const policy = {
      authority: 'human-ready-allowed' as const,
      requiredGateSignals: [{ source: 'github-check' as const, name: 'test' }],
      requiredReviewPerspectives: [
        'security',
        'codeQuality',
      ] as Array<'security' | 'codeQuality'>,
      minimumHeadEpochs: 1,
    };
    const registration = await store.createRegistration({
      repository: 'sample/human-review-resume',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: { releaseEvidence: policy },
    });
    const queued = await store.enqueueJob({
      registrationId: registration.id,
      registrationVersion: registration.version,
      source: { kind: 'poll', key: 'issue:33' },
      idempotencyKey: 'issue:33',
      jobType: 'agentops.triage',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'human-review-resume' },
        issue: {
          number: 33,
          observedUpdatedAt: '2026-08-01T00:00:00Z',
        },
      },
    });
    const triageLease = await store.acquireLease({
      workerId: 'triage-human-review-resume',
      durationMs: 10_000,
      jobType: 'agentops.triage',
    });
    const promotedJobId = await store.promoteTriageLease({
      token: triageLease!.token,
      workerId: 'triage-human-review-resume',
      readyLabel: 'ready',
      claimedLabel: 'claimed',
      authority: {
        actor: 'product-owner',
        readyAt: '2026-08-01T00:00:30Z',
        sourceIssue: frozenSourceIssue(
          registration.repository,
          33,
          '2026-08-01T00:00:29Z',
        ),
      },
      result: {
        schemaVersion: 1,
        status: 'succeeded',
        jobId: queued.job.id,
        attemptNumber: triageLease!.attemptNumber,
        repository: registration.repository,
        issueNumber: 33,
        outcome: 'promoted',
        sourceDigest: null,
        decision: null,
        commentUrl: null,
        appliedLabels: [],
        promotedJobId: null,
        providerProvenance: null,
        completedAt: '2026-08-01T00:00:31Z',
      },
    });
    const runnerLease = await store.acquireLease({
      workerId: 'runner-human-review-resume',
      durationMs: 10_000,
      jobType: 'agentops.runner',
    });
    expect(runnerLease?.job.id).toBe(promotedJobId);
    const releaseId = runnerLease!.job.releaseId!;
    const validHumanReviewResult = {
      schemaVersion: 1 as const,
      status: 'succeeded' as const,
      jobId: promotedJobId,
      attemptNumber: runnerLease!.attemptNumber,
      repository: registration.repository,
      outcome: 'needs-human-review' as const,
      humanReview: {
        issueNumber: 33,
        reasonCount: 1,
        commentUrl:
          'https://github.com/sample/human-review-resume/issues/33#issuecomment-1',
        classification: 'what-judgment' as const,
        howIntervention: false as const,
        aiAppliedReadyLabel: false as const,
        claimedLabelRemoved: true as const,
      },
      headSha: null,
      pullRequestNumber: null,
      artifacts: [],
      completedAt: '2026-08-01T00:01:00Z',
    };
    const invalidResults: Array<{
      name: string;
      result: unknown;
      error: RegExp;
    }> = [
      { name: 'SQL NULL', result: null, error: /result is invalid/ },
      {
        name: 'missing required headSha',
        result: { ...validHumanReviewResult, headSha: undefined },
        error: /result is invalid/,
      },
      {
        name: 'unexpected root key',
        result: { ...validHumanReviewResult, unexpected: true },
        error: /result is invalid/,
      },
      {
        name: 'malformed job UUID',
        result: { ...validHumanReviewResult, jobId: 'not-a-uuid' },
        error: /result is invalid/,
      },
      {
        name: 'different valid job UUID',
        result: { ...validHumanReviewResult, jobId: randomUUID() },
        error: /lease or release is invalid/,
      },
      {
        name: 'null attempt number',
        result: { ...validHumanReviewResult, attemptNumber: null },
        error: /result is invalid/,
      },
      {
        name: 'different attempt number',
        result: {
          ...validHumanReviewResult,
          attemptNumber: runnerLease!.attemptNumber + 1,
        },
        error: /lease or release is invalid/,
      },
      {
        name: 'non-null head SHA',
        result: { ...validHumanReviewResult, headSha: 'a'.repeat(40) },
        error: /result is invalid/,
      },
      {
        name: 'non-null pull request number',
        result: { ...validHumanReviewResult, pullRequestNumber: 42 },
        error: /result is invalid/,
      },
      {
        name: 'string reason count',
        result: {
          ...validHumanReviewResult,
          humanReview: {
            ...validHumanReviewResult.humanReview,
            reasonCount: '1',
          },
        },
        error: /result is invalid/,
      },
      {
        name: 'missing nested comment URL',
        result: {
          ...validHumanReviewResult,
          humanReview: {
            ...validHumanReviewResult.humanReview,
            commentUrl: undefined,
          },
        },
        error: /result is invalid/,
      },
      {
        name: 'unexpected nested key',
        result: {
          ...validHumanReviewResult,
          humanReview: {
            ...validHumanReviewResult.humanReview,
            unexpected: true,
          },
        },
        error: /result is invalid/,
      },
    ];
    for (const invalid of invalidResults) {
      await expect(pool.query(
        `SELECT agentops_control.complete_runner_human_review($1, $2, $3)`,
        [runnerLease!.token, 'runner-human-review-resume', invalid.result],
      ), invalid.name).rejects.toThrow(invalid.error);
    }
    await expect(store.finishHumanReviewLease({
      token: runnerLease!.token,
      workerId: 'runner-human-review-resume',
      result: validHumanReviewResult,
    })).resolves.toBe(releaseId);
    await expect(store.getRelease(releaseId)).resolves.toMatchObject({
      status: 'abandoned',
      pullRequest: null,
      finalHead: null,
    });
    await expect(store.findReleaseForRunnerEvent({
      registrationId: registration.id,
      issueNumber: 33,
    })).resolves.toBeNull();
    await expect(store.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:33:release:resumed',
      repository: registration.repository,
      issueNumber: 33,
      policy,
    })).resolves.toMatchObject({
      duplicate: false,
      release: { status: 'collecting' },
    });
  });

  it('fails promotion closed until a required pre-ready AI decision is proven', async () => {
    const policy = {
      authority: 'ai-triage-required' as const,
      requiredGateSignals: [{ source: 'github-check' as const, name: 'test' }],
      requiredReviewPerspectives: ['security', 'codeQuality'],
      minimumHeadEpochs: 1,
    };
    const registration = await store.createRegistration({
      repository: 'sample/strict-promotion',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: { releaseEvidence: policy },
    });
    const queued = await store.enqueueJob({
      registrationId: registration.id,
      registrationVersion: registration.version,
      source: { kind: 'poll', key: 'issue:32' },
      idempotencyKey: 'issue:32',
      jobType: 'agentops.triage',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'strict-promotion' },
        issue: {
          number: 32,
          observedUpdatedAt: '2026-08-01T00:00:00Z',
        },
      },
    });
    const lease = await store.acquireLease({
      workerId: 'triage-ai-required',
      durationMs: 10_000,
      jobType: 'agentops.triage',
    });
    const result = {
      schemaVersion: 1 as const,
      status: 'succeeded' as const,
      jobId: queued.job.id,
      attemptNumber: lease!.attemptNumber,
      repository: registration.repository,
      issueNumber: 32,
      outcome: 'promoted' as const,
      sourceDigest: null,
      decision: null,
      commentUrl: null,
      appliedLabels: [],
      promotedJobId: null,
      providerProvenance: null,
      completedAt: '2026-08-01T00:00:31Z',
    };
    await expect(store.promoteTriageLease({
      token: lease!.token,
      workerId: 'triage-ai-required',
      readyLabel: 'ready',
      claimedLabel: 'claimed',
      authority: {
        actor: 'product-owner',
        readyAt: '2026-08-01T00:00:30Z',
        sourceIssue: frozenSourceIssue(
          registration.repository,
          32,
          '2026-08-01T00:00:29Z',
        ),
      },
      result,
    })).rejects.toThrow(/AI triage receipt is required/);
    const rolledBack = await pool.query<{
      status: string;
      release_id: string | null;
      promoted_count: string;
    }>(
      `SELECT triage.status, triage.release_id,
              (SELECT count(*)::text
                 FROM agentops_control.jobs promoted
                WHERE promoted.registration_id = triage.registration_id
                  AND promoted.job_type = 'agentops.runner') AS promoted_count
         FROM agentops_control.jobs triage
        WHERE triage.id = $1`,
      [queued.job.id],
    );
    expect(rolledBack.rows[0]).toEqual({
      status: 'leased',
      release_id: null,
      promoted_count: '0',
    });

    const decision = {
      schemaVersion: 1 as const,
      type: 'feature' as const,
      northStarAlignment: 'aligned' as const,
      readiness: 'ready_candidate' as const,
      priority: 'p1' as const,
      summary: 'Ready after AI triage.',
      rationale: ['The request and acceptance contract are complete.'],
      dependencies: [],
      duplicateCandidates: [],
      missingInformation: [],
    };
    const promotedJobId = await store.promoteTriageLease({
      token: lease!.token,
      workerId: 'triage-ai-required',
      readyLabel: 'ready',
      claimedLabel: 'claimed',
      authority: {
        actor: 'product-owner',
        readyAt: '2026-08-01T00:00:30Z',
        sourceIssue: frozenSourceIssue(
          registration.repository,
          32,
          '2026-08-01T00:00:29Z',
        ),
        triage: {
          sourceDigest: 'f'.repeat(64),
          decision,
          completedAt: '2026-08-01T00:00:20Z',
          providerProvenance: {
            attemptId: lease!.attemptId,
            provider: 'codex',
            model: { kind: 'explicit', name: 'gpt-5' },
            consumer: {
              repository: 'mrbaron3/servo',
              revision: 'c'.repeat(40),
            },
            environment: {
              kind: 'container',
              reference: 'ghcr.io/mrbaron3/agentops@sha256:triage',
              digest: `sha256:${'d'.repeat(64)}`,
            },
          },
        },
      },
      result,
    });
    const promoted = await pool.query<{ release_id: string }>(
      `SELECT release_id FROM agentops_control.jobs WHERE id = $1`,
      [promotedJobId],
    );
    const releaseId = promoted.rows[0]!.release_id;
    const invocationKey = `triage-job:${queued.job.id}`;
    const invocationRef = deterministicInvocationRef(releaseId, invocationKey);
    const rawReceipts = await pool.query<{ kind: string; payload: any }>(
      `SELECT kind, payload
         FROM agentops_control.release_receipt_outbox
        WHERE release_id = $1
        ORDER BY recorded_at, receipt_id`,
      [releaseId],
    );
    const rawAuthority = rawReceipts.rows.find(({ kind }) => kind === 'authority')!.payload;
    const rawRuntime = rawReceipts.rows.find(
      ({ kind }) => kind === 'runtime-provenance',
    )!.payload;
    expect(rawAuthority).toMatchObject({ triageInvocationRef: invocationRef });
    expect(rawAuthority).not.toHaveProperty('triageInvocationId');
    expect(rawRuntime.invocations).toEqual([
      expect.objectContaining({ invocationKey, invocationRef }),
    ]);
    expect(rawRuntime.invocations[0]).not.toHaveProperty('invocationId');

    const receipt = (await store.listReleaseReceipts(releaseId))
      .find((entry) => entry.receipt.kind === 'authority')!.receipt;
    expect(receipt).toMatchObject({
      kind: 'authority',
      route: 'ai-triage-then-human-ready',
      triageInvocationRef: invocationRef,
      triageCompletedAt: '2026-08-01T00:00:20.000000Z',
      sourceDigest: 'f'.repeat(64),
    });
  });
});
