import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { randomUUID } from 'node:crypto';
import { Pool } from 'pg';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import {
  AgentInvocation,
  EvalRun,
  Issue,
  PR,
} from '../src/domain/schema.js';
import {
  CONTROL_SCHEMA_VERSION,
  PostgresControlStore,
  migrateControlSchema,
} from '../src/control-store/index.js';
import {
  projectReleaseMerge,
  projectReleasePreMerge,
  projectReleaseProgress,
} from '../src/evidence/release-projection.js';
import { observePrRevision } from '../src/pipeline/execution/pr-native.js';
import { Store } from '../src/store/store.js';

vi.setConfig({ testTimeout: 20_000 });
const databaseUrl = process.env.AGENTOPS_TEST_DATABASE_URL;
const integration = databaseUrl ? describe.sequential : describe.skip;

integration('production release receipt projection', () => {
  let pool: Pool;
  let control: PostgresControlStore;
  const roots: string[] = [];

  beforeAll(async () => {
    pool = new Pool({ connectionString: databaseUrl, max: 4 });
    await pool.query('DROP SCHEMA IF EXISTS agentops_control CASCADE');
    expect(await migrateControlSchema(pool)).toBe(CONTROL_SCHEMA_VERSION);
    await pool.query(
      `UPDATE agentops_control.lifecycle_state
          SET mode = 'ACTIVE', generation = generation + 1,
              updated_at = clock_timestamp()
        WHERE singleton`,
    );
    control = new PostgresControlStore(pool);
  });

  afterAll(async () => {
    for (const root of roots) fs.rmSync(root, { recursive: true, force: true });
    await pool?.end();
  });

  it('certifies local invocations and independent gates before merge, then exports the artifact', async () => {
    const registration = await control.createRegistration({
      repository: 'sample/projection-target',
      enabled: true,
      issueMonitorEnabled: true,
      prMonitorEnabled: true,
      executionEnabled: true,
      configuration: {},
    });
    const created = await control.createRelease({
      registrationId: registration.id,
      registrationVersion: registration.version,
      releaseKey: 'issue:41:release:1',
      repository: registration.repository,
      issueNumber: 41,
      policy: {
        authority: 'human-ready-allowed',
        requiredGateSignals: [
          { source: 'repository-grader', name: 'unit_tests' },
          { source: 'github-check', name: 'ci' },
        ],
        requiredReviewPerspectives: ['security', 'codeQuality'],
        minimumHeadEpochs: 1,
      },
    });
    const queued = await control.enqueueJob({
      registrationId: registration.id,
      registrationVersion: registration.version,
      source: { kind: 'manual', key: 'projection' },
      idempotencyKey: 'projection',
      jobType: 'agentops.runner',
      payload: {
        schemaVersion: 1,
        repository: { owner: 'sample', name: 'projection-target' },
        event: { kind: 'issue', number: 41, action: 'recovery' },
        target: { baseRef: 'refs/heads/main' },
        execution: {
          mode: 'development_turn',
          requiredChecks: ['ci'],
          mergeMethod: 'squash',
          readyLabel: 'ready',
          claimedLabel: 'claimed',
        },
        artifacts: [],
      },
    });
    await control.linkJobToRelease({ jobId: queued.job.id, releaseId: created.release.id });
    const lease = await control.acquireLease({
      workerId: 'projection-runner',
      durationMs: 30_000,
      jobType: 'agentops.runner',
    });
    const at = (seconds: number) => new Date(
      Date.parse(created.release.createdAt) + seconds * 1_000,
    ).toISOString();
    const authorityId = randomUUID();
    await control.recordReleaseReceipt({
      receiptId: authorityId,
      receiptKey: 'authority:projection',
      releaseId: created.release.id,
      repository: registration.repository,
      issueNumber: 41,
      producer: { jobId: queued.job.id, attemptId: lease!.attemptId },
      causes: [],
      recordedAt: at(1),
      kind: 'authority',
      route: 'human-ready',
      actor: { type: 'human', login: 'maintainer' },
      readyLabel: 'ready',
      readyAt: at(1),
    });
    const requirementsReceiptId = randomUUID();
    const requirementsReceipt = {
      receiptId: requirementsReceiptId,
      receiptKey: 'requirements-authority',
      releaseId: created.release.id,
      repository: registration.repository,
      issueNumber: 41,
      producer: { jobId: queued.job.id, attemptId: lease!.attemptId },
      causes: [authorityId],
      recordedAt: at(2),
      kind: 'requirements-authority',
      sourceIssueDigest: 'f'.repeat(64),
      sourceUpdatedAt: at(1),
      capturedAt: at(2),
    };
    await pool.query(
      `INSERT INTO agentops_control.release_receipt_outbox(
         receipt_id, release_id, receipt_key, kind, repository, issue_number,
         head_sha, causes, payload, recorded_at
       ) VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $8, $9)`,
      [
        requirementsReceiptId,
        created.release.id,
        requirementsReceipt.receiptKey,
        requirementsReceipt.kind,
        registration.repository,
        41,
        [authorityId],
        requirementsReceipt,
        at(2),
      ],
    );

    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'release-projection-'));
    roots.push(root);
    const local = new Store(root);
    local.addIssue(Issue.parse({
      id: 'ISSUE-0041',
      type: 'feature',
      title: 'Project durable release evidence',
      area: 'backend',
      status: 'build-approved',
      assignedAgent: 'codex',
      contract: {
        productGoal: 'certify releases',
        userStory: 'As an operator I can audit a release',
        scope: { include: ['src/**'], exclude: [] },
        acceptanceCriteria: [{
          id: 'AC-41',
          severity: 'blocker',
          behavior: 'release receipts certify the exact head',
          verification: { method: 'unit_test', expected: ['passes'] },
        }],
        redLines: [],
      },
      createdAt: at(1),
      updatedAt: at(1),
    }));
    const head = 'a'.repeat(40);
    const pr = local.addPR(PR.parse({
      id: 'PR-0041',
      issueId: 'ISSUE-0041',
      branch: 'agent/release-projection',
      baseBranch: 'main',
      generator: 'codex',
      origin: 'issue-pipeline',
      agentGeneratedHeadSha: head,
      attempts: 1,
      externalRef: {
        provider: 'github',
        repository: registration.repository,
        number: 51,
        url: 'https://github.com/sample/projection-target/pull/51',
      },
      status: 'open',
      currentRevisionId: null,
      headSha: null,
      mergedHeadSha: null,
      createdAt: at(2),
      updatedAt: at(2),
    }));
    const revision = observePrRevision(local, pr, head);
    const currentPr = local.getPR(pr.id)!;
    const invocation = (key: string, role: 'generator' | 'reviewer', perspective: string | null) =>
      local.addAgentInvocation(AgentInvocation.parse({
        id: `INVOKE-${key}`,
        invocationKey: key,
        subjectId: perspective ?? 'build',
        issueId: currentPr.issueId,
        sampleIndex: 0,
        attempt: 1,
        role,
        perspective,
        provider: 'codex',
        model: 'gpt-5',
        prompt: 'bounded fixture',
        outcome: 'completed',
        prId: currentPr.id,
        revisionId: revision.id,
        headSha: head,
        createdAt: at(perspective ? 4 : 3),
      }));
    invocation('generator-1', 'generator', null);
    for (const perspective of ['security', 'codeQuality']) {
      const key = `review-${perspective}`;
      invocation(key, 'reviewer', perspective);
      local.addEvalRun(EvalRun.parse({
        id: `EVAL-${perspective}`,
        issueId: currentPr.issueId,
        prId: currentPr.id,
        attempt: 1,
        sampleIndex: 0,
        agent: 'codex',
        verdict: 'approve',
        hardGates: { unit_tests: 'pass' },
        findings: [],
        scores: {
          functionality: 1,
          codeQuality: 1,
          testQuality: 1,
          ux: 1,
          accessibility: 1,
        },
        overall: 1,
        evidenceDir: null,
        cost: { usd: 0, tokens: 1, seconds: 1 },
        featureArea: 'backend',
        perspective,
        invocationKey: key,
        revisionId: revision.id,
        headSha: head,
        createdAt: at(5),
      }));
    }
    const emptyRequestKey = 'review-ux-empty-request';
    invocation(emptyRequestKey, 'reviewer', 'ux');
    local.addEvalRun(EvalRun.parse({
      id: 'EVAL-ux-empty-request',
      issueId: currentPr.issueId,
      prId: currentPr.id,
      attempt: 1,
      sampleIndex: 0,
      agent: 'codex',
      verdict: 'request_changes',
      hardGates: { unit_tests: 'pass' },
      findings: [],
      scores: {
        functionality: 1,
        codeQuality: 1,
        testQuality: 1,
        ux: 1,
        accessibility: 1,
      },
      overall: 1,
      evidenceDir: null,
      cost: { usd: 0, tokens: 1, seconds: 1 },
      featureArea: 'backend',
      perspective: 'ux',
      invocationKey: emptyRequestKey,
      revisionId: revision.id,
      headSha: head,
      createdAt: at(5),
    }));
    local.save();

    const producer = { jobId: queued.job.id, attemptId: lease!.attemptId };
    const projectionInput = {
      control,
      release: created.release,
      local,
      pr: currentPr,
      pullRequestNumber: 51,
      observedPrHead: head,
      githubChecks: [{ name: 'ci', status: 'success' as const }],
      githubObservedAt: at(6),
      producer,
      runtime: {
        consumer: { repository: 'mrbaron3/servo', revision: 'b'.repeat(40) },
        environment: {
          kind: 'container' as const,
          reference: 'ghcr.io/mrbaron3/agentops@sha256:fixture',
          digest: `sha256:${'c'.repeat(64)}`,
        },
        providerDefaults: [],
      },
    };
    await projectReleaseProgress({
      ...projectionInput,
      githubChecks: [],
    });
    expect((await control.listReleaseReceipts(created.release.id))
      .map((entry) => entry.receipt)
      .find((receipt) => receipt.kind === 'review' && receipt.perspective === 'ux'))
      .toMatchObject({
        verdict: 'request_changes',
        hasFindings: false,
        findings: [],
      });
    expect(await control.getRelease(created.release.id)).toMatchObject({
      status: 'collecting',
      pullRequest: 51,
      finalHead: null,
    });
    await expect(control.findReleaseForRunnerEvent({
      registrationId: registration.id,
      pullRequest: 51,
    })).resolves.toMatchObject({ id: created.release.id });
    await projectReleasePreMerge(projectionInput);
    expect(await control.getRelease(created.release.id)).toMatchObject({
      status: 'merge-authorized',
      pullRequest: 51,
      finalHead: head,
    });
    const mergeSha = 'd'.repeat(40);
    await projectReleaseMerge(control, created.release, producer, {
      pullRequestNumber: 51,
      expectedHead: head,
      observedPrHead: head,
      mergeSha,
      actor: 'merge-bot',
      sourceIssueClosure: 'completed',
      mergeReachableFromDefaultBranch: true,
      mergedAt: at(8),
    });
    await expect(control.findReleaseForRunnerEvent({
      registrationId: registration.id,
      pullRequest: 51,
    })).resolves.toBeNull();
    await expect(control.findReleaseForRunnerEvent({
      registrationId: registration.id,
      pullRequest: 51,
      includeMerged: true,
    })).resolves.toMatchObject({
      id: created.release.id,
      status: 'merged',
      finalHead: head,
    });
    const receipts = await control.listReleaseReceipts(created.release.id, {
      includeMergeIntent: true,
    });
    await control.recordReleaseArtifact({
      artifactKey: 'runner-result:projection',
      artifact: {
        kind: 'runner-result',
        uri: `volume://registrations/${registration.id}/projection/result.json`,
        sha256: 'e'.repeat(64),
        sizeBytes: 128,
        releaseId: created.release.id,
        sourceHead: head,
        receiptIds: receipts.map((entry) => entry.receipt.receiptId),
      },
    });
    const evidence = await control.exportReleaseEvidence(created.release.id);
    expect(evidence).toMatchObject({
      schemaVersion: '4.0',
      release: {
        id: created.release.id,
        pullRequestNumber: 51,
        finalHead: head,
        mergeSha,
      },
      result: 'passed',
    });
    expect(evidence.receipts.runtime[0]?.invocations).toHaveLength(4);
    expect(evidence.receipts.grades.map((receipt) => receipt.signal.source).sort())
      .toEqual(['github-check', 'repository-grader']);
  });

  it('keeps runner table DML denied while exposing only release capabilities', async () => {
    const privileges = await pool.query<{
      insert: boolean;
      update: boolean;
      execute: boolean;
      completion: boolean;
      bindPullRequest: boolean;
      observeHead: boolean;
    }>(
      `SELECT
         has_table_privilege(
           'agentops_runner', 'agentops_control.release_receipt_outbox', 'INSERT'
         ) AS insert,
         has_table_privilege(
           'agentops_runner', 'agentops_control.releases', 'UPDATE'
         ) AS update,
         has_function_privilege(
           'agentops_runner',
           'agentops_control.authorize_release_merge(jsonb)', 'EXECUTE'
         ) AS execute,
         has_function_privilege(
           'agentops_runner',
           'agentops_control.lock_release_completion_state(uuid,uuid)', 'EXECUTE'
         ) AS completion,
         has_function_privilege(
           'agentops_runner',
           'agentops_control.bind_release_pull_request(uuid,uuid,bigint)', 'EXECUTE'
         ) AS "bindPullRequest",
         has_function_privilege(
           'agentops_runner',
           'agentops_control.observe_release_head(uuid,text,text)', 'EXECUTE'
         ) AS "observeHead"`,
    );
    expect(privileges.rows[0]).toEqual({
      insert: false,
      update: false,
      execute: true,
      completion: true,
      bindPullRequest: true,
      observeHead: true,
    });
  });
});
