import { z } from 'zod';
import type { HarnessConfig } from '../../config.js';
import {
  PrRevision,
  PrHeadSha,
  ApprovedRevisionGateSnapshot,
  RevisionCheck,
  RevisionBinding,
  RevisionReviewThread,
  approvePR,
  bindApprovalRevisionToPR,
  bindMergeRevisionToPR,
  mergeApprovedPR,
  reconcileRequestedMerge,
  stalePrRevision,
  transitionPR,
  transitionPrRevision,
  evaluateRevisionGateEvidence,
  MAX_REVIEW_THREAD_REASON_BODY_CHARS,
  type EvaluatedRevisionGateSnapshot,
  type PR,
  type RevisionGateSnapshot as RevisionGateSnapshotType,
} from '../../domain/schema.js';
import { Store, nowISO } from '../../store/store.js';
import { linkedParentIssueNumber } from '../../intake/parent-link.js';
export {
  BLOCKING_REVIEW_COMMENT,
  GhPrListResponse,
  GhPrApiPagesResponse,
  GhPrViewResponse,
  githubCheckStatus,
  listOpenGithubPullRequests,
  MAX_REVIEW_THREAD_BODY_CHARS,
  observeGithubRelease,
  ReviewThreadsResponse,
  parseBlockingReviewThreads,
  realPrNativeGithubRunner,
} from './pr-native-github.js';

export const GithubPrRevisionState = z.object({
  state: z.enum(['open', 'merged', 'closed']),
  headSha: z.string().regex(/^[0-9a-f]{40}$/i, 'expected a 40-character git SHA'),
  isDraft: z.boolean(),
  mergeability: z.enum(['mergeable', 'conflicting', 'unknown']),
  checks: z.array(RevisionCheck),
  unresolvedBlockingThreadIds: z.array(z.string()),
  blockingReviewThreads: z.array(RevisionReviewThread).optional(),
});
export type GithubPrRevisionState = z.infer<typeof GithubPrRevisionState>;

/** External facts were unavailable or stale; callers may defer capture to reconciliation. */
export class RevisionGateCaptureUnavailableError extends Error {
  constructor(message: string, readonly captureCause?: unknown) {
    super(message);
    this.name = 'RevisionGateCaptureUnavailableError';
  }
}

export interface GithubOpenPullRequest {
  number: number;
  url: string;
  title: string;
  body: string;
  headRefName: string;
  headSha: string;
  baseRefName: string;
  isDraft: boolean;
  isCrossRepository: boolean;
}

export interface FetchedPullRequestRevision {
  headSha: string;
  baseSha: string;
}

export interface GithubReleaseObservation {
  pullRequestNumber: number;
  expectedHead: string;
  observedPrHead: string;
  mergeSha: string;
  actor: string;
  sourceIssueClosure: 'completed';
  mergeReachableFromDefaultBranch: true;
  mergedAt: string;
}

export interface PrNativeGithubRunner {
  viewRevision(cwd: string, prNumber: number): GithubPrRevisionState;
  merge(cwd: string, prNumber: number, expectedHeadSha: string): void;
  closeIssue(cwd: string, repository: string, issueNumber: number): void;
  /** Optional deterministic epic reconciliation inventory. */
  listRepositoryIssues?(
    cwd: string,
    repository: string,
  ): GithubRepositoryIssue[];
  /** Optional on test doubles; the production runner enables repository-wide discovery. */
  listOpenPullRequests?(cwd: string, baseBranch: string): GithubOpenPullRequest[];
  fetchPullRequestHead?(
    cwd: string,
    prNumber: number,
    expectedHeadSha: string,
    headRefName: string,
    baseRefName: string,
  ): FetchedPullRequestRevision;
  pullRequestChangedFiles?(cwd: string, prNumber: number): string[];
  /** Observe post-merge facts from GitHub without deriving them from local state. */
  observeRelease?(
    cwd: string,
    repository: string,
    issueNumber: number,
    prNumber: number,
    expectedHead: string,
    integrationBranch?: string,
  ): GithubReleaseObservation;
}

export interface GithubRepositoryIssue {
  number: number;
  title: string;
  body: string;
  authorLogin: string;
  subIssueNumbers: number[];
  state: 'open' | 'closed';
  stateReason: 'completed' | 'not_planned' | null;
}

export interface ExternalEpicClosureResult {
  parentIssueNumber: number | null;
  requiredKeys: string[];
  pendingKeys: string[];
  closed: boolean;
  reason: string | null;
}

function issuePhaseKey(title: string): string | null {
  return title.match(/\[(DF-[0-9]{3})\]/i)?.[1]?.toUpperCase() ?? null;
}

function explicitlyExcludedPhaseKeys(body: string): Set<string> {
  const excluded = new Set<string>();
  for (const line of body.split(/\r?\n/)) {
    const keys = [...line.matchAll(/\bDF-[0-9]{3}\b/gi)].map((match) => ({
      key: match[0].toUpperCase(),
      start: match.index ?? 0,
      end: (match.index ?? 0) + match[0].length,
    }));
    // Exclusion is authority-bearing. Accept only an affirmative, single-key
    // declaration; prose that compares phases or says "not future scope"
    // remains required and therefore fails closed.
    if (keys.length !== 1) continue;
    const declaration = `${line.slice(0, keys[0]!.start)} ${line.slice(keys[0]!.end)}`
      .trim()
      .replace(/^[-:：=→>\s]+/, '')
      .trim();
    const explicitEnglish = /^(?:is\s+)?future[ -]*scope(?:\s+(?:and|[,;]).*)?\.?$/i
      .test(declaration)
      || /^(?:is\s+)?out[ -]of[ -]scope(?:\s+.*)?\.?$/i.test(declaration);
    const explicitJapanese = /将来\s*scope/i.test(declaration)
      && /(?:含めない|対象外)/.test(declaration)
      && !/(?:ではない|でない|じゃない|ではなく)/.test(declaration);
    const explicitVersionExclusion = /^(?:v0|今回|現行).*(?:含めない|対象外)$/i
      .test(declaration);
    if (explicitEnglish || explicitJapanese || explicitVersionExclusion) {
      excluded.add(keys[0]!.key);
    }
  }
  return excluded;
}

/**
 * Close a body-linked external epic only when every explicitly named v0 phase
 * is closed. Lines that mark a phase as future/out-of-scope remove that phase
 * from the required set. Missing or duplicate phase mappings fail closed.
 */
export function reconcileExternalEpicClosure(
  runner: PrNativeGithubRunner,
  cwd: string,
  repository: string,
  sourceIssue: { number: number; body: string },
): ExternalEpicClosureResult {
  const parentNumber = linkedParentIssueNumber(sourceIssue.body, sourceIssue.number);
  if (parentNumber === null) {
    return {
      parentIssueNumber: null,
      requiredKeys: [],
      pendingKeys: [],
      closed: false,
      reason: 'Source Issue has no Parent marker',
    };
  }
  if (!runner.listRepositoryIssues) {
    return {
      parentIssueNumber: parentNumber,
      requiredKeys: [],
      pendingKeys: [],
      closed: false,
      reason: 'GitHub Issue inventory is unavailable',
    };
  }
  const evaluate = (issues: GithubRepositoryIssue[]): ExternalEpicClosureResult => {
    const parent = issues.find((issue) => issue.number === parentNumber);
    if (!parent) {
      return {
        parentIssueNumber: parentNumber,
        requiredKeys: [],
        pendingKeys: [],
        closed: false,
        reason: 'Parent Issue is missing',
      };
    }
    if (parent.state === 'closed') {
      return {
        parentIssueNumber: parentNumber,
        requiredKeys: [],
        pendingKeys: [],
        closed: parent.stateReason === 'completed',
        reason: parent.stateReason === 'completed'
          ? null
          : 'Parent Issue is closed without completion',
      };
    }
    const allKeys = new Set(
      [...parent.body.matchAll(/\bDF-[0-9]{3}\b/gi)]
        .map((match) => match[0].toUpperCase()),
    );
    const excluded = explicitlyExcludedPhaseKeys(parent.body);
    const requiredKeys = [...allKeys].filter((key) => !excluded.has(key)).sort();
    if (requiredKeys.length === 0) {
      return {
        parentIssueNumber: parentNumber,
        requiredKeys,
        pendingKeys: [],
        closed: false,
        reason: 'Parent Issue has no explicit required phase keys',
      };
    }
    const explicitSubIssues = new Set(parent.subIssueNumbers);
    const children = issues.filter((issue) => (
      linkedParentIssueNumber(issue.body, issue.number) === parentNumber
      && (
        explicitSubIssues.size > 0
          ? explicitSubIssues.has(issue.number)
          : parent.authorLogin !== '' && issue.authorLogin === parent.authorLogin
      )
    ));
    const pendingKeys: string[] = [];
    for (const key of requiredKeys) {
      const matches = children.filter((issue) => issuePhaseKey(issue.title) === key);
      if (matches.length !== 1) {
        return {
          parentIssueNumber: parentNumber,
          requiredKeys,
          pendingKeys,
          closed: false,
          reason: `${key} must map to exactly one Parent-linked child Issue`,
        };
      }
      if (
        matches[0]!.state !== 'closed'
        || matches[0]!.stateReason !== 'completed'
      ) pendingKeys.push(key);
    }
    return {
      parentIssueNumber: parentNumber,
      requiredKeys,
      pendingKeys,
      closed: false,
      reason: pendingKeys.length > 0 ? 'Required epic phases remain open' : null,
    };
  };
  const initial = evaluate(runner.listRepositoryIssues(cwd, repository));
  if (initial.closed || initial.reason !== null) return initial;
  // Re-read immediately before mutation so a child reopened during the first
  // inventory cannot leave only the parent closed.
  const refreshed = evaluate(runner.listRepositoryIssues(cwd, repository));
  if (refreshed.closed || refreshed.reason !== null) return refreshed;
  if (refreshed.requiredKeys.join('\0') !== initial.requiredKeys.join('\0')) {
    return {
      ...refreshed,
      reason: 'Parent required phase inventory changed before close',
    };
  }
  runner.closeIssue(cwd, repository, parentNumber);
  return {
    parentIssueNumber: parentNumber,
    requiredKeys: refreshed.requiredKeys,
    pendingKeys: [],
    closed: true,
    reason: null,
  };
}

export interface RevisionGateInput {
  pr: PR;
  revision: PrRevision;
  requiredPerspectives: string[];
  github: GithubPrRevisionState;
  requiredChecks?: string[];
}

export { MAX_REVIEW_THREAD_REASON_BODY_CHARS };

/**
 * Observe one immutable PR head. A changed head immediately makes every older
 * non-terminal approval stale, before any review of the new revision can count.
 */
export function observePrRevision(
  store: Store,
  observedPr: PR,
  headSha: string,
): PrRevision {
  const pr = store.getPR(observedPr.id) ?? observedPr;
  if (pr.status === 'merged' || pr.status === 'closed') {
    throw new Error(`cannot observe a new revision for terminal PR ${pr.id} (${pr.status})`);
  }
  const parsedSha = PrHeadSha.parse(headSha);
  const existing = store.revisionForHead(pr.id, parsedSha);
  if (existing) {
    const alreadyBoundApproval = pr.status === 'approved'
      && existing.status === 'approved'
      && pr.currentRevisionId === existing.id
      && pr.headSha === existing.headSha;
    if (!alreadyBoundApproval) {
      store.replacePR(transitionPR(pr, {
        status: pr.status === 'approved' ? 'open' : pr.status,
        currentRevisionId: existing.id,
        headSha: existing.headSha,
        mergedHeadSha: null,
      }));
    }
    return existing;
  }

  for (const revision of store.db.prRevisions) {
    if (
      revision.prId === pr.id
      && revision.status !== 'merged'
      && revision.status !== 'failed'
      && revision.status !== 'stale'
    ) {
      store.replacePrRevision(stalePrRevision(revision, nowISO()));
    }
  }
  const revision = store.upsertPrRevision(PrRevision.parse({
    id: store.nextId('PRREV'),
    prId: pr.id,
    headSha: parsedSha,
    ordinal: store.db.prRevisions.filter((row) => row.prId === pr.id).length + 1,
    status: 'pending',
    createdAt: nowISO(),
  }));
  store.replacePR(transitionPR(pr, {
    status: 'open',
    currentRevisionId: revision.id,
    headSha: revision.headSha,
    mergedHeadSha: null,
  }));
  return revision;
}

/**
 * Deterministically decide one current-head gate. Even an `approve` EvalRun
 * cannot mask a P0/P1-equivalent blocker/major finding on the same revision.
 */
export function evaluateRevisionGate(
  store: Store,
  input: RevisionGateInput,
): EvaluatedRevisionGateSnapshot {
  const reviewRuns = store.db.evalRuns.filter(
    (run) => run.prId === input.revision.prId
      && run.revisionId === input.revision.id
      && run.headSha === input.revision.headSha,
  ).map((run) => ({
    prId: run.prId,
    binding: RevisionBinding.parse({
      revisionId: run.revisionId,
      headSha: run.headSha,
    }),
    perspective: run.perspective,
    verdict: run.verdict,
    findings: run.findings,
  }));
  const currentPr = store.getPR(input.pr.id) ?? input.pr;
  return evaluateRevisionGateEvidence({
    id: store.nextId('PRGATE'),
    pr: currentPr,
    revision: input.revision,
    requiredPerspectives: input.requiredPerspectives,
    reviewRuns,
    github: input.github,
    requiredChecks: input.requiredChecks,
    createdAt: nowISO(),
  });
}

function persistRevisionGateSnapshot(
  store: Store,
  candidate: EvaluatedRevisionGateSnapshot,
): EvaluatedRevisionGateSnapshot {
  const latest = store.db.revisionGateSnapshots
    .filter((snapshot) => snapshot.revisionId === candidate.revisionId)
    .at(-1);
  const comparable = (snapshot: RevisionGateSnapshotType) => ({
    headSha: snapshot.headSha,
    requiredPerspectives: snapshot.requiredPerspectives,
    perspectiveVerdicts: snapshot.perspectiveVerdicts,
    checks: snapshot.checks,
    unresolvedBlockingThreadIds: snapshot.unresolvedBlockingThreadIds,
    blockingReviewThreads: snapshot.blockingReviewThreads,
    mergeability: snapshot.mergeability,
    decision: snapshot.decision,
    blockingReasons: snapshot.blockingReasons,
    pendingReasons: snapshot.pendingReasons,
    reasons: snapshot.reasons,
  });
  if (latest && JSON.stringify(comparable(latest)) === JSON.stringify(comparable(candidate))) {
    return candidate;
  }
  return store.addRevisionGateSnapshot(candidate);
}

/**
 * Persist the observable GitHub gate facts for a reviewed immutable revision
 * without requesting a merge. Keeping this capture adjacent to review
 * completion ensures the final reviewed revision retains checks and blocking
 * thread IDs even if the process stops before the broader reconciliation pass.
 */
export function captureCurrentRevisionGateSnapshot(
  store: Store,
  config: HarnessConfig,
  pr: PR,
  revision: PrRevision,
  runner: PrNativeGithubRunner,
  cwd: string,
  requiredPerspectives: string[],
): EvaluatedRevisionGateSnapshot {
  const externalRef = pr.externalRef;
  if (!externalRef) throw new Error(`${pr.id} is not projected to GitHub`);
  let github: GithubPrRevisionState;
  try {
    github = GithubPrRevisionState.parse(
      runner.viewRevision(cwd, externalRef.number),
    );
  } catch (error) {
    throw new RevisionGateCaptureUnavailableError(
      `cannot capture gate snapshot for ${revision.id}: GitHub facts unavailable`,
      error,
    );
  }
  if (github.headSha !== revision.headSha) {
    throw new RevisionGateCaptureUnavailableError(
      `cannot capture gate snapshot for ${revision.id}: `
      + `head changed from ${revision.headSha} to ${github.headSha}`,
    );
  }
  return persistRevisionGateSnapshot(store, evaluateRevisionGate(store, {
    pr,
    revision,
    requiredPerspectives,
    github,
    requiredChecks: config.gate?.requiredChecks,
  }));
}

export interface AutoMergeResult {
  prId: string;
  revisionId: string | null;
  headSha: string | null;
  decision:
    | RevisionGateSnapshotType['decision']
    | 'merged'
    | 'closed'
    | 'unverified-merge'
    | 'error';
  merged: boolean;
  reasons: string[];
}

async function finalizeMergedRevision(
  store: Store,
  pr: Extract<PR, { status: 'approved' }>,
  revision: Extract<PrRevision, { status: 'approved' }>,
  authorization: ReturnType<typeof approvePR>,
  runner: PrNativeGithubRunner,
  cwd: string,
  options: AutoMergeOptions,
): Promise<AutoMergeResult> {
  const mergeBinding = bindMergeRevisionToPR(authorization);
  const mergedPr = mergeApprovedPR(pr, mergeBinding);
  return persistMergedRevision(store, mergedPr, revision, runner, cwd, options);
}

async function persistMergedRevision(
  store: Store,
  mergedPr: ReturnType<typeof mergeApprovedPR>,
  revision: Extract<PrRevision, { status: 'approved' }>,
  runner: PrNativeGithubRunner,
  cwd: string,
  options: AutoMergeOptions,
): Promise<AutoMergeResult> {
  await options.completeMerge?.({ pr: mergedPr, revision });
  await options.beforeRelease?.();
  const mergedRevision = store.replacePrRevision(transitionPrRevision(revision, {
    status: 'merged',
    completedAt: nowISO(),
  }));
  const storedMergedPr = store.mergePR(mergedPr);
  if (store.getIssue(storedMergedPr.issueId)?.status !== 'released') {
    store.setStatus(storedMergedPr.issueId, 'released');
  }
  store.save();
  reconcileSplitSourceClosures(store, runner, cwd);
  return {
    prId: storedMergedPr.id,
    revisionId: mergedRevision.id,
    headSha: mergedRevision.headSha,
    decision: 'merged',
    merged: true,
    reasons: [],
  };
}

/** Reconcile previously reviewed PRs whose checks/threads may have changed since the last turn. */
export async function reconcilePrNativeGates(
  store: Store,
  config: HarnessConfig,
  runner: PrNativeGithubRunner,
  cwd: string,
  requiredPerspectives: string[],
  options: AutoMergeOptions = {},
): Promise<AutoMergeResult[]> {
  if ((config.gate?.backend ?? 'store') !== 'github') return [];
  const candidates = store.db.prs
    .filter((pr) =>
      pr.externalRef !== null && pr.status !== 'merged' && pr.status !== 'closed')
    .filter((pr) => {
      const issue = store.getIssue(pr.issueId);
      return issue !== undefined && issue.status !== 'released' && issue.status !== 'closed';
    });
  const results: AutoMergeResult[] = [];
  for (const pr of candidates) {
    try {
      results.push(await autoMergeCurrentRevision(
        store,
        config,
        pr,
        runner,
        cwd,
        requiredPerspectives,
        options,
      ));
    } catch (error) {
      results.push({
        prId: pr.id,
        revisionId: pr.currentRevisionId,
        headSha: pr.headSha,
        decision: 'error',
        merged: false,
        reasons: [error instanceof Error ? error.message : String(error)],
      });
    }
  }
  reconcileSplitSourceClosures(store, runner, cwd);
  return results;
}

/**
 * A split Source Issue is closed only after every projected child work unit is
 * released. Failures stay durable and are retried by the next reconciliation.
 */
export function reconcileSplitSourceClosures(
  store: Store,
  runner: PrNativeGithubRunner,
  cwd: string,
): void {
  for (const intake of store.db.intakeRecords) {
    if (intake.storeIssueIds.length < 2 || intake.sourceClosedAt) continue;
    const allReleased = intake.storeIssueIds.every(
      (issueId) => store.getIssue(issueId)?.status === 'released',
    );
    if (!allReleased) continue;
    try {
      runner.closeIssue(cwd, intake.snapshot.repository, intake.snapshot.number);
      intake.sourceClosedAt = nowISO();
      intake.sourceCloseError = null;
    } catch (error) {
      intake.sourceCloseError = error instanceof Error ? error.message : String(error);
    }
    intake.updatedAt = nowISO();
    store.save();
  }
}

/**
 * Re-fetch current GitHub state, evaluate evidence tied to that exact SHA, then
 * merge with `--match-head-commit`. A push between evaluation and merge makes
 * the merge command fail rather than consuming stale approval.
 */
export async function autoMergeCurrentRevision(
  store: Store,
  config: HarnessConfig,
  pr: PR,
  runner: PrNativeGithubRunner,
  cwd: string,
  requiredPerspectives: string[],
  options: AutoMergeOptions = {},
): Promise<AutoMergeResult> {
  const externalRef = pr.externalRef;
  if (!externalRef) throw new Error(`${pr.id} is not projected to GitHub`);
  if (pr.status === 'merged' || pr.status === 'closed') {
    throw new Error(`${pr.id} is terminal (${pr.status})`);
  }
  const github = GithubPrRevisionState.parse(
    runner.viewRevision(cwd, externalRef.number),
  );
  let revision = observePrRevision(store, pr, github.headSha);
  // Observation atomically projects the exact revision coordinates onto the PR.
  // Continue from that stored value rather than the caller's potentially unbound snapshot.
  pr = store.getPR(pr.id)!;
  if (revision.status === 'pending') {
    revision = store.replacePrRevision(transitionPrRevision(revision, {
      status: 'reviewing',
    }));
  }

  if (github.state === 'merged') {
    const approvedBeforeMerge = store.db.revisionGateSnapshots.find(
      (snapshot) => snapshot.revisionId === revision.id
        && snapshot.headSha === revision.headSha
        && snapshot.decision === 'approved',
    );
    const validatedApproval = ApprovedRevisionGateSnapshot.safeParse(approvedBeforeMerge);
    if (
      !validatedApproval.success
      || pr.status !== 'approved'
      || revision.status !== 'approved'
      || revision.mergeRequestedAt === null
    ) {
      const revisionCanFail = revision.status !== 'merged'
        && revision.status !== 'stale'
        && revision.status !== 'failed';
      if (revisionCanFail) {
        revision = store.replacePrRevision(transitionPrRevision(revision, {
          status: 'failed', completedAt: nowISO(),
        }));
      }
      const issue = store.getIssue(pr.issueId);
      if (
        issue
        && issue.status !== 'needs-human-review'
        && issue.status !== 'released'
        && issue.status !== 'closed'
      ) {
        store.setStatus(issue.id, 'needs-human-review');
      }
      store.save();
      return {
        prId: pr.id,
        revisionId: revision.id,
        headSha: revision.headSha,
        decision: 'unverified-merge',
        merged: false,
        reasons: [
          !validatedApproval.success
            ? 'GitHub reports merged but no approved gate snapshot exists for this head'
            : 'GitHub reports merged without a matching durable merge request',
        ],
      };
    }
    return persistMergedRevision(
      store,
      reconcileRequestedMerge(pr, revision, validatedApproval.data),
      revision,
      runner,
      cwd,
      options,
    );
  }
  if (github.state === 'closed') {
    revision = store.replacePrRevision(transitionPrRevision(revision, {
      status: 'failed', completedAt: nowISO(),
    }));
    pr = store.replacePR(transitionPR(pr, { status: 'closed' }));
    store.save();
    return {
      prId: pr.id,
      revisionId: revision.id,
      headSha: revision.headSha,
      decision: 'closed',
      merged: false,
      reasons: ['pull request is closed without merge'],
    };
  }

  const snapshot = persistRevisionGateSnapshot(store, evaluateRevisionGate(store, {
    pr,
    revision,
    requiredPerspectives,
    github,
    requiredChecks: config.gate?.requiredChecks,
  }));
  if (snapshot.decision !== 'approved') {
    // A failed/stale revision is immutable evidence about this exact head. Gate
    // polling may still refresh checks, draft state, and external review threads,
    // but it must not resurrect that terminal revision as "reviewing".
    if (
      revision.status !== 'failed'
      && revision.status !== 'stale'
      && revision.status !== 'merged'
    ) {
      revision = store.replacePrRevision(snapshot.decision === 'pending'
        ? transitionPrRevision(revision, { status: 'reviewing' })
        : transitionPrRevision(revision, {
          status: 'changes-requested',
        }));
    }
    pr = store.replacePR(snapshot.decision === 'changes-requested'
      ? transitionPR(pr, { status: 'changes-requested' })
      : transitionPR(pr, { status: 'open' }));
    const issue = store.getIssue(pr.issueId);
    if (snapshot.decision === 'changes-requested' && issue) {
      if (issue.status === 'build-approved') {
        store.setStatus(issue.id, 'needs-human-review');
      }
      // Exhausting the bounded repair loop is a durable escalation. Polling the
      // same rejected revision must not turn needs-human-review back into a fresh
      // repair budget; only observing a new head re-enters evaluation.
      if (issue.status === 'evaluation-in-progress') {
        store.setStatus(issue.id, 'changes-requested');
      }
    }
    store.save();
    return {
      prId: pr.id,
      revisionId: revision.id,
      headSha: revision.headSha,
      decision: snapshot.decision,
      merged: false,
      reasons: [...snapshot.reasons],
    };
  }

  revision = store.replacePrRevision(transitionPrRevision(revision, { status: 'approved' }));
  if (revision.status !== 'approved') {
    throw new Error('approved gate did not produce an approved revision');
  }
  const approval = bindApprovalRevisionToPR(pr, revision, snapshot);
  const authorization = approvePR(pr, approval);
  pr = store.approvePR(authorization);
  if (revision.mergeRequestedAt) {
    store.save();
    return {
      prId: pr.id,
      revisionId: revision.id,
      headSha: revision.headSha,
      decision: 'pending',
      merged: false,
      reasons: [`merge was requested at ${revision.mergeRequestedAt}; awaiting GitHub merged state`],
    };
  }
  // Persist the exact approved snapshot before the external merge. If the
  // process crashes after GitHub accepts the merge, reconciliation can prove
  // that this head had already passed rather than guessing from `state=merged`.
  store.save();
  const mergeAuthorization = await options.authorizeMerge?.({
    pr,
    revision,
    snapshot,
    github,
  });
  if (mergeAuthorization && !mergeAuthorization.authorized) {
    store.save();
    return {
      prId: pr.id,
      revisionId: revision.id,
      headSha: revision.headSha,
      decision: 'pending',
      merged: false,
      reasons: mergeAuthorization.reasons,
    };
  }
  runner.merge(cwd, externalRef.number, revision.headSha);
  revision = store.replacePrRevision(transitionPrRevision(revision, {
    status: 'approved', mergeRequestedAt: nowISO(),
  }));
  if (revision.status !== 'approved') {
    throw new Error('merge request did not preserve the approved revision');
  }
  store.save();
  const afterMerge = GithubPrRevisionState.parse(
    runner.viewRevision(cwd, externalRef.number),
  );
  if (afterMerge.headSha !== revision.headSha) {
    observePrRevision(store, pr, afterMerge.headSha);
    store.save();
    return {
      prId: pr.id,
      revisionId: revision.id,
      headSha: revision.headSha,
      decision: 'changes-requested',
      merged: false,
      reasons: [`head changed after merge request to ${afterMerge.headSha}`],
    };
  }
  if (afterMerge.state !== 'merged') {
    store.save();
    return {
      prId: pr.id,
      revisionId: revision.id,
      headSha: revision.headSha,
      decision: 'pending',
      merged: false,
      reasons: ['merge request accepted or queued; awaiting GitHub merged state'],
    };
  }
  if (pr.status !== 'approved' || revision.status !== 'approved') {
    throw new Error('merge finalization requires approved PR and revision variants');
  }
  return finalizeMergedRevision(
    store,
    pr,
    revision,
    authorization,
    runner,
    cwd,
    options,
  );
}

export interface AutoMergeOptions {
  /** Exact isolated-runner release boundary; absent for non-runner callers. */
  beforeRelease?: () => void | Promise<void>;
  /** Persist a durable, current-head authorization before GitHub can merge. */
  authorizeMerge?: (input: {
    pr: Extract<PR, { status: 'approved' }>;
    revision: Extract<PrRevision, { status: 'approved' }>;
    snapshot: z.infer<typeof ApprovedRevisionGateSnapshot>;
    github: GithubPrRevisionState;
  }) => Promise<void | { authorized: boolean; reasons: string[] }>;
  /** Persist the observed merge before the local issue can become released. */
  completeMerge?: (input: {
    pr: ReturnType<typeof mergeApprovedPR>;
    revision: Extract<PrRevision, { status: 'approved' }>;
  }) => Promise<void>;
}
