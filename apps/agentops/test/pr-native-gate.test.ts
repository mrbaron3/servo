import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, type HarnessConfig } from '../src/config.js';
import {
  EvalRun,
  IntakeRecord,
  Issue,
  PR,
  PrRevision,
  RevisionGateSnapshot,
  transitionPrRevision,
  type Finding,
} from '../src/domain/schema.js';
import { WebhookDelivery } from '../src/webhook/schema.js';
import {
  autoMergeCurrentRevision,
  evaluateRevisionGate,
  GithubPrRevisionState,
  GhPrListResponse,
  GhPrViewResponse,
  githubCheckStatus,
  listOpenGithubPullRequests,
  MAX_REVIEW_THREAD_BODY_CHARS,
  MAX_REVIEW_THREAD_REASON_BODY_CHARS,
  observeGithubRelease,
  observePrRevision,
  parseBlockingReviewThreads,
  reconcileExternalEpicClosure,
  reconcileSplitSourceClosures,
  type GithubRepositoryIssue,
  type PrNativeGithubRunner,
} from '../src/pipeline/execution/pr-native.js';
import { Store, nowISO } from '../src/store/store.js';
import { pollable } from '../src/pipeline/execution/guard.js';

const roots: string[] = [];
const SHA_A = 'a'.repeat(40);
const SHA_B = 'b'.repeat(40);

function githubApiPull(number: number) {
  return {
    number,
    html_url: `https://github.com/acme/theme/pull/${number}`,
    title: `Change ${number}`,
    body: '',
    draft: false,
    head: {
      ref: `feature/${number}`,
      sha: number.toString(16).padStart(40, '0'),
      repo: { full_name: 'acme/theme' },
    },
    base: {
      ref: 'main',
      repo: { full_name: 'acme/theme' },
    },
  };
}
const PERSPECTIVES = ['functionality', 'codeQuality', 'security'];
const CONFIG: HarnessConfig = {
  ...DEFAULT_CONFIG,
  gate: {
    backend: 'github',
    requiredChecks: ['test'],
    mergeMethod: 'squash',
  },
};

function setup(): { store: Store; pr: PR } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'agentops-pr-native-'));
  roots.push(root);
  const store = new Store(root);
  const issue = store.addIssue(Issue.parse({
    id: 'ISSUE-0001',
    type: 'feature',
    title: 'PR native delivery',
    area: 'harness',
    status: 'contract-drafted',
    assignedAgent: 'codex',
    contract: {
      productGoal: 'ship safely',
      userStory: 'As an operator I get reviewed changes',
      scope: { include: ['src/**'], exclude: [] },
      acceptanceCriteria: [{
        id: 'AC-1',
        severity: 'blocker',
        behavior: 'safe delivery',
        verification: { method: 'unit_test', expected: ['passes'] },
      }],
      redLines: [],
    },
    createdAt: nowISO(),
    updatedAt: nowISO(),
  }));
  for (const status of [
    'ready-for-generation',
    'generation-in-progress',
    'ready-for-evaluation',
    'evaluation-in-progress',
    'build-approved',
  ] as const) {
    store.setStatus(issue.id, status);
  }
  const pr = store.addPR(PR.parse({
    id: 'PR-0001',
    issueId: issue.id,
    branch: 'agent/issue-0001-s0',
    generator: 'codex',
    status: 'open',
    currentRevisionId: 'PRREV-INITIAL',
    headSha: SHA_A,
    externalRef: {
      provider: 'github',
      number: 8,
      url: 'https://github.com/acme/theme/pull/8',
    },
    createdAt: nowISO(),
    updatedAt: nowISO(),
  }));
  return { store, pr };
}

function addReview(
  store: Store,
  pr: PR,
  revisionId: string,
  headSha: string,
  perspective: string,
  findings: Finding[] = [],
): void {
  store.addEvalRun(EvalRun.parse({
    id: store.nextId('EVAL'),
    issueId: pr.issueId,
    prId: pr.id,
    attempt: 1,
    sampleIndex: 0,
    agent: 'codex',
    verdict: 'approve',
    findings,
    scores: {
      functionality: 1,
      codeQuality: 1,
      testQuality: 1,
      ux: 1,
      accessibility: 1,
    },
    overall: 1,
    cost: {},
    perspective,
    revisionId,
    headSha,
    createdAt: nowISO(),
  }));
}

function greenGithub(headSha = SHA_A): GithubPrRevisionState {
  return {
    state: 'open',
    headSha,
    isDraft: false,
    mergeability: 'mergeable',
    checks: [{ name: 'test', status: 'success' }],
    unresolvedBlockingThreadIds: [],
  };
}

afterEach(() => {
  while (roots.length > 0) fs.rmSync(roots.pop()!, { recursive: true, force: true });
});

describe('external epic completion', () => {
  const parentBody = [
    'DF-002 -> DF-003',
    'DF-004 -> DF-005',
    'DF-006 -> DF-007',
    'DF-003 + DF-005 -> DF-008',
    'DF-003 + DF-004 -> DF-009',
    'DF-009 is future scope and v0 に含めない',
  ].join('\n');
  const issues: GithubRepositoryIssue[] = [
    {
      number: 1,
      title: 'Servo pipeline',
      body: parentBody,
      authorLogin: 'owner',
      subIssueNumbers: [],
      state: 'open' as const,
      stateReason: null,
    },
    ...['002', '003', '004', '005', '006', '007', '008', '009'].map((key, index) => ({
      number: index + 2,
      title: `[DF-${key}] phase`,
      body: 'Parent: #1',
      authorLogin: 'owner',
      subIssueNumbers: [],
      state: 'closed' as const,
      stateReason: 'completed' as const,
    })),
  ];

  it('keeps the parent open while a required phase is open and excludes future scope', () => {
    const closes: number[] = [];
    const inventory = issues.map((issue) => ({ ...issue }));
    inventory.find((issue) => issue.title.startsWith('[DF-008]'))!.state = 'open';
    inventory.find((issue) => issue.title.startsWith('[DF-008]'))!.stateReason = null;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => {},
      closeIssue: (_cwd, _repository, number) => closes.push(number),
      listRepositoryIssues: () => inventory,
    };

    const result = reconcileExternalEpicClosure(
      runner,
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #1' },
    );

    expect(result.requiredKeys).not.toContain('DF-009');
    expect(result.pendingKeys).toEqual(['DF-008']);
    expect(result.closed).toBe(false);
    expect(closes).toEqual([]);
  });

  it('closes the parent exactly when every required linked phase is closed', () => {
    const closes: string[] = [];
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => {},
      closeIssue: (_cwd, repository, number) => closes.push(`${repository}#${number}`),
      listRepositoryIssues: () => issues,
    };

    const result = reconcileExternalEpicClosure(
      runner,
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #1' },
    );

    expect(result.closed).toBe(true);
    expect(closes).toEqual(['acme/theme#1']);
  });

  it('does not count NOT_PLANNED as a completed required phase', () => {
    const inventory = issues.map((issue) => ({ ...issue }));
    const required = inventory.find((issue) => issue.title.startsWith('[DF-008]'))!;
    required.state = 'closed';
    required.stateReason = 'not_planned';
    const closes: number[] = [];
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => {},
      closeIssue: (_cwd, _repository, number) => closes.push(number),
      listRepositoryIssues: () => inventory,
    };

    const result = reconcileExternalEpicClosure(
      runner, '/repo', 'acme/theme', { number: 8, body: 'Parent: #1' },
    );

    expect(result.pendingKeys).toContain('DF-008');
    expect(result.closed).toBe(false);
    expect(closes).toEqual([]);
  });

  it('does not infer an exclusion from mixed dependency prose', () => {
    const inventory = issues.map((issue) => ({ ...issue }));
    inventory[0] = {
      ...inventory[0]!,
      body: 'DF-003 depends on DF-009 (future scope)',
    };
    const result = reconcileExternalEpicClosure(
      {
        viewRevision: () => greenGithub(),
        merge: () => {},
        closeIssue: () => {},
        listRepositoryIssues: () => inventory,
      },
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #1' },
    );
    expect(result.requiredKeys).toEqual(['DF-003', 'DF-009']);
  });

  it('keeps an explicitly negated future-scope phase required', () => {
    const inventory = issues.map((issue) => ({ ...issue }));
    inventory[0] = { ...inventory[0]!, body: 'DF-003 is not future scope' };
    const required = inventory.find((issue) => issue.title.startsWith('[DF-003]'))!;
    required.state = 'open';
    required.stateReason = null;
    const result = reconcileExternalEpicClosure(
      {
        viewRevision: () => greenGithub(),
        merge: () => {},
        closeIssue: () => { throw new Error('must not close'); },
        listRepositoryIssues: () => inventory,
      },
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #1' },
    );
    expect(result.requiredKeys).toEqual(['DF-003']);
    expect(result.pendingKeys).toEqual(['DF-003']);
  });

  it.each([
    'DF-003 は将来scopeではない',
    'DF-003 does not depend on future scope',
    'DF-003 is not a future scope item',
  ])('does not treat negated or dependency prose as exclusion: %s', (body) => {
    const inventory = issues.map((issue) => ({ ...issue }));
    inventory[0] = { ...inventory[0]!, body };
    const required = inventory.find((issue) => issue.title.startsWith('[DF-003]'))!;
    required.state = 'open';
    required.stateReason = null;
    const result = reconcileExternalEpicClosure(
      {
        viewRevision: () => greenGithub(),
        merge: () => {},
        closeIssue: () => { throw new Error('must not close'); },
        listRepositoryIssues: () => inventory,
      },
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #1' },
    );
    expect(result.requiredKeys).toEqual(['DF-003']);
    expect(result.pendingKeys).toEqual(['DF-003']);
  });

  it('rechecks required children immediately before closing the parent', () => {
    const first = issues.map((issue) => ({ ...issue }));
    const second = issues.map((issue) => ({ ...issue }));
    const reopened = second.find((issue) => issue.title.startsWith('[DF-008]'))!;
    reopened.state = 'open';
    reopened.stateReason = null;
    let inventories = 0;
    let closes = 0;
    const result = reconcileExternalEpicClosure(
      {
        viewRevision: () => greenGithub(),
        merge: () => {},
        closeIssue: () => { closes += 1; },
        listRepositoryIssues: () => inventories++ === 0 ? first : second,
      },
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #1' },
    );
    expect(result.pendingKeys).toEqual(['DF-008']);
    expect(closes).toBe(0);
  });

  it('never treats an Issue as its own parent', () => {
    let inventories = 0;
    let closes = 0;
    const result = reconcileExternalEpicClosure(
      {
        viewRevision: () => greenGithub(),
        merge: () => {},
        closeIssue: () => { closes += 1; },
        listRepositoryIssues: () => {
          inventories += 1;
          return issues;
        },
      },
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #8' },
    );
    expect(result).toMatchObject({ parentIssueNumber: null, closed: false });
    expect(inventories).toBe(0);
    expect(closes).toBe(0);
  });

  it.each([
    'Context before the marker.\n\nParent: #1',
    '> Parent: #1',
    '```text\nParent: #1\n```',
    'parent: #1',
  ])('never closes an Epic for a non-canonical Parent Link: %s', (body) => {
    let inventories = 0;
    let closes = 0;
    const result = reconcileExternalEpicClosure(
      {
        viewRevision: () => greenGithub(),
        merge: () => {},
        closeIssue: () => { closes += 1; },
        listRepositoryIssues: () => {
          inventories += 1;
          return issues;
        },
      },
      '/repo',
      'acme/theme',
      { number: 8, body },
    );
    expect(result).toMatchObject({ parentIssueNumber: null, closed: false });
    expect(inventories).toBe(0);
    expect(closes).toBe(0);
  });

  it('rejects a spoof child that is neither a GitHub sub-issue nor parent-authored', () => {
    const inventory: GithubRepositoryIssue[] = [
      {
        number: 1,
        title: 'Servo pipeline',
        body: 'DF-010',
        authorLogin: 'owner',
        subIssueNumbers: [],
        state: 'open',
        stateReason: null,
      },
      {
        number: 99,
        title: '[DF-010] spoof',
        body: 'Parent: #1',
        authorLogin: 'external-contributor',
        subIssueNumbers: [],
        state: 'closed',
        stateReason: 'completed',
      },
    ];
    let closes = 0;
    const result = reconcileExternalEpicClosure(
      {
        viewRevision: () => greenGithub(),
        merge: () => {},
        closeIssue: () => { closes += 1; },
        listRepositoryIssues: () => inventory,
      },
      '/repo',
      'acme/theme',
      { number: 8, body: 'Parent: #1' },
    );
    expect(result).toMatchObject({
      requiredKeys: ['DF-010'],
      closed: false,
      reason: 'DF-010 must map to exactly one Parent-linked child Issue',
    });
    expect(closes).toBe(0);
  });
});

describe('ISSUE-0024/PR-INTENT durable lifecycle invariants', () => {
  it('rejects approved gate evidence that still contains blockers or pending reasons', () => {
    const base = {
      id: 'PRGATE-0001',
      prId: 'PR-0001',
      revisionId: 'PRREV-0001',
      headSha: SHA_A,
      mergeability: 'mergeable',
      decision: 'approved',
      createdAt: nowISO(),
    };
    expect(() => RevisionGateSnapshot.parse({
      ...base,
      blockingReasons: ['blocker survived'],
    })).toThrow();
    expect(() => RevisionGateSnapshot.parse({
      ...base,
      pendingReasons: ['check pending'],
    })).toThrow();
    expect(() => RevisionGateSnapshot.parse({
      ...base,
      unresolvedBlockingThreadIds: ['PRRT-P1'],
    })).toThrow();
  });

  it('rejects malformed gh JSON instead of coercing it to a merge-eligible PR', () => {
    expect(() => GhPrViewResponse.parse({
      id: 'PR_node',
      state: 'SOMETHING_NEW',
      headRefOid: SHA_A,
      mergeable: 'MERGEABLE',
      statusCheckRollup: [],
    })).toThrow();
    expect(() => GhPrListResponse.parse([{
      number: 9,
      url: 'https://github.com/acme/theme/pull/9',
      title: 'change',
      body: '',
      headRefName: 'feature',
      headRefOid: 'not-a-sha',
      baseRefName: 'main',
      isDraft: false,
    }])).toThrow();
    expect(() => GhPrViewResponse.parse({
      id: 'PR_node',
      state: 'OPEN',
      isDraft: false,
      headRefOid: SHA_A,
      mergeable: 'MERGEABLE',
      statusCheckRollup: [{ name: '', status: 'COMPLETED' }],
    })).toThrow();
  });

  it('captures merged head, completed issue, actor, and default-branch reachability', () => {
    const commands: string[][] = [];
    const observation = observeGithubRelease(
      (_command, args) => {
        commands.push(args);
        if (args[0] === 'pr') return JSON.stringify({
          state: 'MERGED',
          headRefOid: SHA_A,
          mergeCommit: { oid: SHA_B },
          mergedBy: { login: 'merge-bot' },
          mergedAt: '2026-08-01T00:00:00.000Z',
        });
        if (args[0] === 'issue') {
          return JSON.stringify({ state: 'CLOSED', stateReason: 'COMPLETED' });
        }
        if (args[0] === 'repo') {
          return JSON.stringify({ defaultBranchRef: { name: 'main' } });
        }
        return JSON.stringify({ status: 'ahead' });
      },
      '/repo',
      'acme/theme',
      7,
      8,
      SHA_A,
    );

    expect(observation).toMatchObject({
      pullRequestNumber: 8,
      expectedHead: SHA_A,
      observedPrHead: SHA_A,
      mergeSha: SHA_B,
      actor: 'merge-bot',
      sourceIssueClosure: 'completed',
      mergeReachableFromDefaultBranch: true,
    });
    expect(commands.at(-1)).toContain(`repos/acme/theme/compare/${SHA_B}...main`);
  });

  it('AC-PRLOOP-005 paginates the complete open-PR inventory beyond 100 rows', () => {
    const commandArgs: string[][] = [];
    const pages = [
      Array.from({ length: 100 }, (_, index) => githubApiPull(index + 1)),
      [githubApiPull(101)],
    ];
    const pulls = listOpenGithubPullRequests(
      (_command, args) => {
        commandArgs.push(args);
        return JSON.stringify(pages);
      },
      '/repo',
      'main',
    );

    expect(commandArgs).toEqual([expect.arrayContaining([
      'api', '--method', 'GET', '--paginate', '--slurp',
    ])]);
    expect(commandArgs[0]).toContain('base=main');
    expect(pulls).toHaveLength(101);
    expect(new Set(pulls.map((pull) => pull.number)).size).toBe(101);
    expect(pulls.at(-1)).toMatchObject({
      number: 101,
      headRefName: 'feature/101',
      baseRefName: 'main',
      isCrossRepository: false,
    });
  });

  it('treats GitHub CLI empty reviewDecision as no submitted review', () => {
    expect(GhPrViewResponse.parse({
      id: 'PR_node',
      state: 'OPEN',
      isDraft: true,
      headRefOid: SHA_A,
      mergeable: 'MERGEABLE',
      reviewDecision: '',
      statusCheckRollup: [],
    }).reviewDecision).toBeNull();
  });

  it('treats GitHub CLI empty check conclusion as not concluded', () => {
    expect(GhPrViewResponse.parse({
      id: 'PR_node',
      state: 'OPEN',
      isDraft: false,
      headRefOid: SHA_A,
      mergeable: 'MERGEABLE',
      reviewDecision: '',
      statusCheckRollup: [{
        name: 'required-check',
        status: 'IN_PROGRESS',
        conclusion: '',
      }],
    }).statusCheckRollup?.[0]?.conclusion).toBeNull();
  });

  it('PR-INTENT requires an explicit draft fact in every current-revision snapshot', () => {
    expect(() => GithubPrRevisionState.parse({
      state: 'open',
      headSha: SHA_A,
      mergeability: 'mergeable',
      checks: [],
      unresolvedBlockingThreadIds: [],
    })).toThrow();
    expect(GithubPrRevisionState.parse(greenGithub()).isDraft).toBe(false);
  });

  it('PR-INTENT does not treat skipped or neutral required checks as successful', () => {
    expect(githubCheckStatus({ name: 'skipped-check', conclusion: 'SKIPPED' })).toBe('failure');
    expect(githubCheckStatus({ name: 'neutral-check', conclusion: 'NEUTRAL' })).toBe('failure');
    expect(githubCheckStatus({ name: 'successful-check', conclusion: 'SUCCESS' })).toBe('success');
  });

  it('rejects lifecycle states missing completion evidence or carrying forbidden metadata', () => {
    expect(() => PrRevision.parse({
      id: 'PRREV-0001', prId: 'PR-0001', headSha: SHA_A, ordinal: 1,
      status: 'merged', createdAt: nowISO(),
    })).toThrow();
    expect(() => WebhookDelivery.parse({
      id: 'WHDEL-0001', deliveryKey: 'd', repository: 'acme/theme',
      event: 'push', headers: {}, payload: {}, status: 'processed', attempts: 1,
      ignoredReason: 'not applicable', receivedAt: nowISO(), updatedAt: nowISO(),
    })).toThrow();
  });
});

describe('PR revision identity and automatic current-head gate', () => {
  it('PR-INTENT classifies unresolved blocking GraphQL threads and pagination safely', () => {
    const longBody = `[P0] ${'x'.repeat(MAX_REVIEW_THREAD_BODY_CHARS + 10)}`;
    const threads = parseBlockingReviewThreads({
      data: { node: { reviewThreads: {
        pageInfo: { hasNextPage: true },
        nodes: [
          { id: 'resolved', isResolved: true, path: null, line: null, comments: { pageInfo: { hasNextPage: false }, nodes: [{ body: '[P1] old' }] } },
          { id: 'p0', isResolved: false, path: null, line: null, comments: { pageInfo: { hasNextPage: false }, nodes: [{ body: longBody }] } },
          { id: 'blocker', isResolved: false, path: null, line: null, comments: { pageInfo: { hasNextPage: false }, nodes: [{ body: 'blocker: unsafe' }] } },
          { id: 'request', isResolved: false, path: null, line: null, comments: { pageInfo: { hasNextPage: false }, nodes: [{ body: 'request_changes' }] } },
          {
            id: 'overflow',
            isResolved: false,
            path: null,
            line: null,
            comments: { pageInfo: { hasNextPage: true }, nodes: [] },
          },
        ],
      } } },
    });

    expect(MAX_REVIEW_THREAD_REASON_BODY_CHARS).toBe(500);
    expect(MAX_REVIEW_THREAD_BODY_CHARS).toBe(8_000);
    expect(threads.map((thread) => thread.id)).toEqual([
      'p0', 'blocker', 'request', 'overflow', 'review-threads:pagination-incomplete',
    ]);
    expect(threads[0]!.body).toHaveLength(MAX_REVIEW_THREAD_BODY_CHARS);
    expect(threads[3]!.body).toContain('exceeded the inspected page');
  });
  it('AC-PRREV-001 reuses the durable revision for the same PR and head SHA', () => {
    const { store, pr } = setup();

    const first = observePrRevision(store, pr, SHA_A);
    const second = observePrRevision(store, pr, SHA_A);

    expect(second.id).toBe(first.id);
    expect(store.db.prRevisions.filter((row) => row.prId === pr.id && row.headSha === SHA_A)).toHaveLength(1);
  });

  it('AC-PRREV-002 invalidates an approved revision as soon as a new head is observed', () => {
    const { store, pr } = setup();
    const oldRevision = observePrRevision(store, pr, SHA_A);
    const reviewingOld = store.replacePrRevision(transitionPrRevision(oldRevision, {
      status: 'reviewing',
    }));
    store.replacePrRevision(transitionPrRevision(reviewingOld, { status: 'approved' }));

    const current = observePrRevision(store, pr, SHA_B);

    expect(store.revisionForHead(pr.id, SHA_A)?.status).toBe('stale');
    expect(store.revisionForHead(pr.id, SHA_A)?.completedAt).not.toBeNull();
    expect(current).toMatchObject({ headSha: SHA_B, ordinal: 2, status: 'pending' });
    expect(store.getPR(pr.id)).toMatchObject({ currentRevisionId: current.id, headSha: SHA_B });
  });

  it('AC-PRREV-003 AC-PRAUTO-001 refuses merge when a required perspective is missing on the current SHA', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    addReview(store, pr, revision.id, SHA_A, 'functionality');
    addReview(store, pr, revision.id, SHA_A, 'codeQuality');
    // A security approval for another SHA must not count.
    addReview(store, pr, revision.id, SHA_B, 'security');

    let merges = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => { merges += 1; },
      closeIssue: () => {},
    };
    const snapshot = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(snapshot.decision).toBe('pending');
    expect(snapshot.reasons).toContain('missing review: security');
    expect(merges).toBe(0);
  });

  it('AC-PRLOOP-002 blocks an approved review that still contains a P1-equivalent major finding', () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(
        store,
        pr,
        revision.id,
        SHA_A,
        perspective,
        perspective === 'security'
          ? [{
              criterionId: 'P1-auth-bypass',
              severity: 'major',
              expected: 'authorization is enforced',
              observed: 'approved verdict still carries P1',
              reproductionSteps: [],
              evidence: {},
              requiredFix: ['enforce authorization'],
            }]
          : [],
      );
    }

    const snapshot = evaluateRevisionGate(store, {
      pr,
      revision,
      requiredPerspectives: PERSPECTIVES,
      github: greenGithub(),
    });

    expect(snapshot.decision).toBe('changes-requested');
    expect(snapshot.reasons).toContain(
      'security has unresolved major finding P1-auth-bypass',
    );
  });

  it('AC-PRLOOP-004 AC-PRAUTO-001 blocks unresolved GitHub P1 threads even when all internal reviews approve', () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }

    const snapshot = evaluateRevisionGate(store, {
      pr,
      revision,
      requiredPerspectives: PERSPECTIVES,
      github: {
        ...greenGithub(),
        unresolvedBlockingThreadIds: ['PRRT-P1'],
      },
    });

    expect(snapshot.decision).toBe('changes-requested');
    expect(snapshot.reasons).toContain('unresolved blocking review thread: PRRT-P1');
  });

  it('AC-PRAUTO-002 AC-PRAUTO-003 merges with the expected current SHA only after all revision gates pass', async () => {
    const { store, pr } = setup();
    const dependent = store.addIssue(Issue.parse({
      id: 'ISSUE-0002',
      type: 'feature',
      title: 'Run after PR-native delivery',
      area: 'harness',
      status: 'contract-drafted',
      assignedAgent: 'mock',
      dependsOnIssues: [pr.issueId],
      contract: {
        productGoal: 'continue the queue after merge',
        userStory: 'As an operator I get the next unblocked task',
        scope: { include: ['src/**'], exclude: [] },
        acceptanceCriteria: [{
          id: 'AC-NEXT',
          severity: 'blocker',
          behavior: 'run after the dependency releases',
          verification: { method: 'unit_test', expected: ['selected on next poll'] },
        }],
        redLines: [],
      },
      createdAt: nowISO(),
      updatedAt: nowISO(),
    }));
    expect(pollable(store, CONFIG).map((issue) => issue.id)).not.toContain(dependent.id);
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    const merges: Array<{ number: number; sha: string }> = [];
    let gateWasDurableBeforeMerge = false;
    let views = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => {
        views += 1;
        return {
          ...greenGithub(),
          ...(views > 1 ? { state: 'merged' as const } : {}),
        };
      },
      merge: (_cwd, number, sha) => {
        gateWasDurableBeforeMerge = new Store(store.root).db.revisionGateSnapshots.some(
          (snapshot) => snapshot.headSha === sha && snapshot.decision === 'approved',
        );
        merges.push({ number, sha });
      },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result).toMatchObject({ decision: 'merged', merged: true, headSha: SHA_A });
    expect(merges).toEqual([{ number: 8, sha: SHA_A }]);
    expect(gateWasDurableBeforeMerge).toBe(true);
    expect(store.getPR(pr.id)).toMatchObject({
      status: 'merged',
      headSha: SHA_A,
      mergedHeadSha: SHA_A,
    });
    expect(store.getIssue(pr.issueId)?.status).toBe('released');
    expect(pollable(store, CONFIG).map((issue) => issue.id)).toContain(dependent.id);
  });

  it('AC-PRAUTO-003 keeps a failed merge retryable without releasing lifecycle state', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) addReview(store, pr, revision.id, SHA_A, perspective);
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => { throw new Error('merge temporarily unavailable'); },
      closeIssue: () => {},
    };

    await expect(autoMergeCurrentRevision(
      store, CONFIG, pr, runner, '/repo', PERSPECTIVES,
    )).rejects.toThrow('merge temporarily unavailable');

    expect(store.getPR(pr.id)).toMatchObject({ status: 'approved', mergedHeadSha: null });
    expect(store.db.prRevisions.find((row) => row.id === revision.id)).toMatchObject({
      status: 'approved',
      mergeRequestedAt: null,
    });
    expect(store.getIssue(pr.issueId)?.status).not.toBe('released');
    await expect(autoMergeCurrentRevision(
      store, CONFIG, store.getPR(pr.id)!, runner, '/repo', PERSPECTIVES,
    )).rejects.toThrow('merge temporarily unavailable');
  });

  it('awaits durable authorization before merge and durable completion before release', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    const events: string[] = [];
    let views = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => ({
        ...greenGithub(),
        state: ++views > 1 ? 'merged' : 'open',
      }),
      merge: () => { events.push('github-merge'); },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
      {
        authorizeMerge: async ({ revision: authorized }) => {
          expect(authorized.headSha).toBe(SHA_A);
          expect(events).toEqual([]);
          events.push('durable-intent');
        },
        completeMerge: async () => {
          expect(store.getIssue(pr.issueId)?.status).not.toBe('released');
          events.push('durable-merge');
        },
        beforeRelease: () => { events.push('local-release'); },
      },
    );

    expect(result.merged).toBe(true);
    expect(events).toEqual([
      'durable-intent',
      'github-merge',
      'durable-merge',
      'local-release',
    ]);
  });

  it('fails closed without invoking GitHub merge when durable authorization rejects', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    let merges = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => { merges += 1; },
      closeIssue: () => {},
    };
    await expect(autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
      { authorizeMerge: async () => { throw new Error('receipt certification failed'); } },
    )).rejects.toThrow('receipt certification failed');
    expect(merges).toBe(0);
    expect(store.getIssue(pr.issueId)?.status).not.toBe('released');
  });

  it('keeps an approved current head pending while durable child integration is incomplete', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    let merges = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => { merges += 1; },
      closeIssue: () => {},
    };
    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
      {
        authorizeMerge: async () => ({
          authorized: false,
          reasons: ['child issue #72 is running'],
        }),
      },
    );
    expect(result).toMatchObject({
      decision: 'pending',
      merged: false,
      reasons: ['child issue #72 is running'],
    });
    expect(merges).toBe(0);
    expect(store.getIssue(pr.issueId)?.status).not.toBe('released');
  });

  it('PR-INTENT reconciles a confirmed merge after restart only when the durable request marker exists', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    let githubState: GithubPrRevisionState['state'] = 'open';
    const runner: PrNativeGithubRunner = {
      viewRevision: () => ({ ...greenGithub(), state: githubState }),
      merge: () => {},
      closeIssue: () => {},
    };

    const requested = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );
    expect(requested).toMatchObject({ decision: 'pending', merged: false });
    expect(store.revisionForHead(pr.id, SHA_A)?.mergeRequestedAt).not.toBeNull();
    githubState = 'merged';

    const restarted = new Store(store.root);
    const reconciled = await autoMergeCurrentRevision(
      restarted,
      CONFIG,
      restarted.getPR(pr.id)!,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(reconciled).toMatchObject({ decision: 'merged', merged: true });
    expect(restarted.getPR(pr.id)?.status).toBe('merged');
    expect(restarted.getIssue(pr.issueId)?.status).toBe('released');
  });

  it('AC-PRAUTO-001 does not merge while a required check is pending', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    let merges = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => ({
        ...greenGithub(),
        checks: [],
      }),
      merge: () => { merges += 1; },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result.decision).toBe('pending');
    expect(result.reasons).toContain('required check pending: test');
    expect(merges).toBe(0);
    expect(store.getIssue(pr.issueId)?.status).toBe('build-approved');
  });

  it('AC-PRAUTO-001 rejects a conflicting current head with a merge-conflicts reason', () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) addReview(store, pr, revision.id, SHA_A, perspective);

    const snapshot = evaluateRevisionGate(store, {
      pr,
      revision,
      requiredPerspectives: PERSPECTIVES,
      github: { ...greenGithub(), mergeability: 'conflicting' },
    });

    expect(snapshot.decision).toBe('changes-requested');
    expect(snapshot.reasons).toContain('pull request has merge conflicts');
  });

  it('AC-PRAUTO-001 keeps unknown mergeability pending', () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) addReview(store, pr, revision.id, SHA_A, perspective);

    const snapshot = evaluateRevisionGate(store, {
      pr,
      revision,
      requiredPerspectives: PERSPECTIVES,
      github: { ...greenGithub(), mergeability: 'unknown' },
    });

    expect(snapshot.decision).toBe('pending');
    expect(snapshot.reasons).toContain('mergeability is unknown');
  });

  it('PR-INTENT does not regrant repair budget when polling the same rejected head', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(
        store,
        pr,
        revision.id,
        SHA_A,
        perspective,
        perspective === 'security'
          ? [{
              criterionId: 'P1-auth',
              severity: 'major',
              expected: 'authenticated ingress',
              observed: 'authentication is bypassable',
              reproductionSteps: [],
              evidence: {},
              requiredFix: ['close the bypass'],
            }]
          : [],
      );
    }
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => {},
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      store.getPR(pr.id)!,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result.decision).toBe('changes-requested');
    expect(store.getIssue(pr.issueId)?.status).toBe('needs-human-review');
  });

  it('PR-INTENT re-evaluates an unchanged changes-requested head when external gate facts recover', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) addReview(store, pr, revision.id, SHA_A, perspective);
    store.replacePrRevision(transitionPrRevision(revision, {
      status: 'reviewing',
    }));
    const rejected = evaluateRevisionGate(store, {
      pr,
      revision,
      requiredPerspectives: PERSPECTIVES,
      github: { ...greenGithub(), mergeability: 'conflicting' },
    });
    store.addRevisionGateSnapshot(rejected);
    store.replacePrRevision(transitionPrRevision(
      store.revisionForHead(pr.id, SHA_A)!,
      { status: 'changes-requested' },
    ));
    let views = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => {
        views += 1;
        return views === 1 ? greenGithub() : { ...greenGithub(), state: 'merged' };
      },
      merge: () => {},
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      store.getPR(pr.id)!,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result).toMatchObject({ decision: 'merged', merged: true });
    expect(store.revisionForHead(pr.id, SHA_A)).toMatchObject({
      status: 'merged',
      completedAt: expect.any(String),
    });
  });

  it('AC-PRLOOP-006 reviews a draft current head but keeps automatic merge pending until it is ready', () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }

    const snapshot = evaluateRevisionGate(store, {
      pr,
      revision,
      requiredPerspectives: PERSPECTIVES,
      github: { ...greenGithub(), isDraft: true },
    });

    expect(snapshot.decision).toBe('pending');
    expect(snapshot.reasons).toContain('pull request is draft');
  });

  it.each([
    ['draft', { isDraft: true }, 'pull request is draft'],
    ['unknown mergeability', { mergeability: 'unknown' as const }, 'mergeability is unknown'],
  ])('PR-INTENT keeps %s in automatic build-approved waiting', async (_name, githubPatch, reason) => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    const runner: PrNativeGithubRunner = {
      viewRevision: () => ({ ...greenGithub(), ...githubPatch }),
      merge: () => { throw new Error('pending gate must not merge'); },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result).toMatchObject({ decision: 'pending', merged: false });
    expect(result.reasons).toContain(reason);
    expect(store.getIssue(pr.issueId)?.status).toBe('build-approved');
  });

  it('waits for confirmed merged state when GitHub accepts or queues a merge request', async () => {
    const { store, pr } = setup();
    const revision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, revision.id, SHA_A, perspective);
    }
    let merges = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => { merges += 1; },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(merges).toBe(1);
    expect(result).toMatchObject({ decision: 'pending', merged: false });
    expect(store.getPR(pr.id)?.status).toBe('approved');
    expect(store.revisionForHead(pr.id, SHA_A)?.status).toBe('approved');
    expect(store.revisionForHead(pr.id, SHA_A)?.mergeRequestedAt).not.toBeNull();
    expect(store.getIssue(pr.issueId)?.status).toBe('build-approved');
    expect(store.db.revisionGateSnapshots).toHaveLength(1);

    const second = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );
    expect(second.decision).toBe('pending');
    expect(merges).toBe(1);
    expect(store.db.revisionGateSnapshots).toHaveLength(1);
  });

  it('keeps polling a terminal failed revision without resurrecting it as reviewing', async () => {
    const { store, pr } = setup();
    const pending = observePrRevision(store, pr, SHA_A);
    const reviewing = store.replacePrRevision(transitionPrRevision(pending, {
      status: 'reviewing',
    }));
    store.replacePrRevision(transitionPrRevision(reviewing, {
      status: 'failed',
      completedAt: '2026-07-23T00:00:00.000Z',
    }));
    const runner: PrNativeGithubRunner = {
      viewRevision: () => ({ ...greenGithub(), isDraft: true }),
      merge: () => { throw new Error('failed revision must not merge'); },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result).toMatchObject({ decision: 'pending', merged: false });
    expect(store.revisionForHead(pr.id, SHA_A)?.status).toBe('failed');
  });

  it('does not convert an externally merged head without an approved snapshot into released', async () => {
    const { store, pr } = setup();
    const runner: PrNativeGithubRunner = {
      viewRevision: () => ({
        ...greenGithub(),
        state: 'merged',
      }),
      merge: () => { throw new Error('must not merge twice'); },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result).toMatchObject({
      decision: 'unverified-merge',
      merged: false,
    });
    expect(store.getPR(pr.id)?.status).not.toBe('merged');
    expect(store.getIssue(pr.issueId)?.status).toBe('needs-human-review');
    expect(store.db.prRevisions[0]?.status).toBe('failed');
  });

  it('PR-INTENT keeps prior P1 threads blocking until an external reviewer resolves them', async () => {
    const { store, pr } = setup();
    const oldRevision = observePrRevision(store, pr, SHA_A);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, oldRevision.id, SHA_A, perspective);
    }
    const rejected = evaluateRevisionGate(store, {
      pr,
      revision: oldRevision,
      requiredPerspectives: PERSPECTIVES,
      github: {
        ...greenGithub(),
        unresolvedBlockingThreadIds: ['PRRT-P1'],
        blockingReviewThreads: [{
          id: 'PRRT-P1',
          body: '[P1] enforce authorization',
          path: 'src/auth.ts',
          line: 42,
        }],
      },
    });
    store.addRevisionGateSnapshot(rejected);
    const reviewingOld = store.replacePrRevision(transitionPrRevision(oldRevision, {
      status: 'reviewing',
    }));
    store.replacePrRevision(transitionPrRevision(reviewingOld, {
      status: 'changes-requested',
    }));

    const repaired = observePrRevision(store, pr, SHA_B);
    for (const perspective of PERSPECTIVES) {
      addReview(store, pr, repaired.id, SHA_B, perspective);
    }
    const runner: PrNativeGithubRunner = {
      viewRevision: () => ({
        ...greenGithub(SHA_B),
        unresolvedBlockingThreadIds: ['PRRT-P1'],
        blockingReviewThreads: [{
          id: 'PRRT-P1',
          body: '[P1] enforce authorization',
          path: 'src/auth.ts',
          line: 42,
        }],
      }),
      merge: () => { throw new Error('an unresolved P1 must block merge'); },
      closeIssue: () => {},
    };

    const result = await autoMergeCurrentRevision(
      store,
      CONFIG,
      pr,
      runner,
      '/repo',
      PERSPECTIVES,
    );

    expect(result).toMatchObject({ decision: 'changes-requested', merged: false });
    expect(result.reasons.join('\n')).toContain('unresolved blocking review thread: PRRT-P1');
  });

  it('AC-PRAUTO-004 closes a split Source Issue only after every child is released and records the result', () => {
    const { store } = setup();
    store.addIssue(Issue.parse({
      id: 'ISSUE-0002',
      type: 'feature',
      title: 'Sibling work',
      area: 'harness',
      status: 'contract-drafted',
      assignedAgent: 'codex',
      contract: {
        productGoal: 'ship safely',
        userStory: 'As an operator I get the sibling',
        scope: { include: ['src/**'], exclude: [] },
        acceptanceCriteria: [{
          id: 'AC-2',
          severity: 'blocker',
          behavior: 'sibling ships',
          verification: { method: 'unit_test', expected: ['passes'] },
        }],
        redLines: [],
      },
      createdAt: nowISO(),
      updatedAt: nowISO(),
    }));
    const intake = store.addIntakeRecord(IntakeRecord.parse({
      id: 'INTAKE-0001',
      intakeKey: 'acme/theme#7',
      provider: 'github',
      snapshot: {
        repository: 'acme/theme',
        number: 7,
        externalId: 'I_7',
        title: 'Split source',
        body: 'two work units',
        url: 'https://github.com/acme/theme/issues/7',
        state: 'open',
        sourceUpdatedAt: nowISO(),
        snapshotAt: nowISO(),
      },
      status: 'claimed',
      storeIssueIds: ['ISSUE-0001', 'ISSUE-0002'],
      createdAt: nowISO(),
      updatedAt: nowISO(),
    }));
    store.setStatus('ISSUE-0001', 'released');
    const closes: string[] = [];
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => {},
      closeIssue: (_cwd, repository, number) => { closes.push(`${repository}#${number}`); },
    };

    reconcileSplitSourceClosures(store, runner, '/repo');
    expect(closes).toEqual([]);

    store.setStatus('ISSUE-0002', 'released');
    reconcileSplitSourceClosures(store, runner, '/repo');
    reconcileSplitSourceClosures(store, runner, '/repo');

    expect(closes).toEqual(['acme/theme#7']);
    expect(intake.sourceClosedAt).not.toBeNull();
    expect(intake.sourceCloseError).toBeNull();
  });

  it('AC-PRAUTO-004 records a close failure and retries once on the next reconciliation', () => {
    const { store } = setup();
    store.addIssue(Issue.parse({
      ...store.getIssue('ISSUE-0001')!,
      id: 'ISSUE-0002',
      status: 'released',
    }));
    store.setStatus('ISSUE-0001', 'released');
    const intake = store.addIntakeRecord(IntakeRecord.parse({
      id: 'INTAKE-0001',
      intakeKey: 'acme/theme#7',
      provider: 'github',
      snapshot: {
        repository: 'acme/theme', number: 7, externalId: 'I_7', title: 'Split source',
        body: 'two work units', url: 'https://github.com/acme/theme/issues/7',
        state: 'open', sourceUpdatedAt: nowISO(), snapshotAt: nowISO(),
      },
      status: 'claimed',
      storeIssueIds: ['ISSUE-0001', 'ISSUE-0002'],
      createdAt: nowISO(),
      updatedAt: nowISO(),
    }));
    let calls = 0;
    const runner: PrNativeGithubRunner = {
      viewRevision: () => greenGithub(),
      merge: () => {},
      closeIssue: () => {
        calls += 1;
        if (calls === 1) throw new Error('GitHub unavailable');
      },
    };

    reconcileSplitSourceClosures(store, runner, '/repo');
    expect(intake.sourceClosedAt).toBeNull();
    expect(intake.sourceCloseError).toBe('GitHub unavailable');

    reconcileSplitSourceClosures(store, runner, '/repo');
    reconcileSplitSourceClosures(store, runner, '/repo');
    expect(calls).toBe(2);
    expect(intake.sourceClosedAt).not.toBeNull();
    expect(intake.sourceCloseError).toBeNull();
  });
});
