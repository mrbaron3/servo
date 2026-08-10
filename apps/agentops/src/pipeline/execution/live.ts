/**
 * The live execution loop: the real-backend wiring of the deterministic drive. Where
 * driveIssueOnce uses the mock runner + deterministic graders, this drives REAL Claude
 * sessions and grounds every gate in real tsc/vitest and real perspective reviews:
 *
 *   generator session → grounded BuildArtifact (real tsc/vitest)
 *     → six read-only perspective sessions write findings.json
 *       → runPanel grades from those files (sessionBackedGrader) + deterministic functionality
 *         → applyPanelVerdict routes to the human gate or the repair lane
 *
 * The orchestration (poll / dispatch / grade / gate) stays deterministic code
 * (ARCH-execution-011); only the sessions inside are non-deterministic. Not unit-tested —
 * it drives live tmux + Claude; the seams it composes are each tested on their own.
 */

import path from 'node:path';
import { createHash } from 'node:crypto';
import type { Issue, PR as PRType } from '../../domain/schema.js';
import { findingOriginRef } from '../../domain/finding-lineage.js';
import { resolveConcurrentIssueCap, type HarnessConfig } from '../../config.js';
import type { RepairBrief } from '../../domain/artifact.js';
import { Store, nowISO } from '../../store/store.js';
import { IssueContract, PR, TurnRecord } from '../../domain/schema.js';
import { recordAgentInvocation } from '../../agents/invocation.js';
import { resolveAgentRoute, resolvedGeneratorProvider } from '../../agents/routing.js';
import { pollable, blockedByDependencies, formatBlockedLine } from './guard.js';
import { mapPool } from './pool.js';
import {
  generatorSessionName,
  generatorWorktreePath,
  runGeneratorSession,
} from './session.js';
import {
  canonicalGithubRepository,
  sampleKey,
  type ExternalWorkIdentity,
} from './work-identity.js';
import { groundArtifact } from './grade.js';
import { runPerspectiveSessions, sessionBackedGrader, type PriorFinding } from './perspective-session.js';
import { runPanel, PERSPECTIVES, type PerspectiveSpec } from '../panel.js';
import { runBoundedRepairLoop, runBestOfN, applyPanelVerdict, type DriveResult, type SampleOutcome } from './loop.js';
import {
  projectReviewRevision,
  realGhGateRunner,
  renderReviewPrTitle,
  type GhGateRunner,
} from './gate.js';
import {
  autoMergeCurrentRevision,
  realPrNativeGithubRunner,
  type AutoMergeOptions,
  type PrNativeGithubRunner,
} from './pr-native.js';
import { improveTick } from '../improve.js';
import type { RegressReportRunner } from '../regression.js';
import { surrogateOracleMismatchRevisions } from '../verification-signal.js';
import type { DevelopmentProgressReporter } from '../../domain/development-progress.js';
import type { DevelopmentReviewRound } from '../../domain/development-review.js';
import {
  DevelopmentReviewPerspective,
  reviewFindingProjection,
} from '../../domain/development-review.js';
import type { SeparateReviewFinding } from './review-children.js';

export interface LiveOptions {
  /** Which lenses to convene (default: all 7). Reduce it for a cheap smoke. */
  perspectives?: PerspectiveSpec[];
  /** Gate backend runner (github only). Injectable for tests; defaults to the real `gh` runner. */
  gateRunner?: GhGateRunner;
  /** Current-head snapshot + expected-SHA merge adapter (github only). */
  prNativeRunner?: PrNativeGithubRunner;
  /** Test/embedding seam for the real generator role session. */
  generatorSession?: typeof runGeneratorSession;
  /** Test/embedding seam for current-head push/PR projection. */
  projectRevision?: typeof projectReviewRevision;
  /** Test/embedding seam for grounded artifact construction. */
  groundBuild?: typeof groundArtifact;
  /** Credential-free environment inherited by repository-controlled grader commands. */
  graderEnvironment?: NodeJS.ProcessEnv;
  /** Test/embedding seam for the turn-tail unit regression report. */
  regressReport?: RegressReportRunner;
  /** Test/embedding seam for the Perspective fan-out. */
  perspectiveSessions?: typeof runPerspectiveSessions;
  /** Runner-owned review evidence root outside registered repository bytes. */
  trustedReviewStateRoot?: string;
  /** Stable operator checkout retained when repository-head review stops. */
  operatorWorktreePath?: string;
  /** Move that checkout to the exact fetched PR head before review starts. */
  projectOperatorWorktreeHead?: (headSha: string) => void;
  /** Frozen ready-time Source Issue supplied only as inert reviewer data. */
  sourceIssueMaterial?: string;
  /** Trusted binding that makes the full frozen Issue a mandatory review AC. */
  sourceIssueReviewCriterion?: {
    url: string;
    digest: string;
    sourceUpdatedAt: string;
  };
  /** Isolated-runner lease/Registration fence immediately before provider execution. */
  beforeProviderExecution?: () => Promise<void>;
  /** Isolated-runner lease/Registration fence immediately before a generated head is pushed. */
  beforePush?: () => Promise<void>;
  /** Fresh isolated-runner fence immediately before the distinct GitHub PR-create mutation. */
  beforeCreatePr?: () => Promise<void>;
  /** Durable observation after the generated head and stable PR identity are projected. */
  afterProjectRevision?: (input: {
    pr: PRType;
    headSha: string;
  }) => Promise<void>;
  /** Durable release correlation for external branch/PR identity. */
  releaseIdentity?: string | null;
  /** Exact release-bound identities allowed to repair imported AgentOps PRs. */
  repositoryRepairIdentities?: Readonly<Record<string, ExternalWorkIdentity>>;
  /** Isolated-runner lease/Registration fence immediately before expected-SHA merge evaluation. */
  beforeMerge?: () => Promise<void>;
  /** Isolated-runner lease/Registration fence armed for the exact durable release mutation. */
  beforeRelease?: () => Promise<void>;
  /** Synchronous single-use permit consumed at the exact durable release mutation. */
  assertReleasePermit?: () => void | Promise<void>;
  /** Durable receipt certification performed against the exact observed head. */
  authorizeMerge?: AutoMergeOptions['authorizeMerge'];
  /** Durable GitHub merge observation performed before local release state. */
  completeMerge?: AutoMergeOptions['completeMerge'];
  /** Best-of-N: independent samples to drive per issue (default config.samples; real default = 1). */
  samples?: number;
  /** Measurement run: drive ALL samples to completion for pass@k / pass^k, not first-approve-stop (E5). */
  measure?: boolean;
  /**
   * Injectable issue-driver for one queued issue (default: the real `driveIssueLive`).
   * ADDITIVE seam (ISSUE-0019): it makes the turn's concurrency scheduling decidable
   * without tmux — an injected worker records start/finish so overlap, cap adherence and
   * dependency exclusion are observable — while the real path stays byte-for-byte the same.
   */
  driveIssue?: (issue: Issue) => Promise<DriveResult>;
  /** Structured, durable progress reporter shared by CLI and Dashboard. */
  progress?: DevelopmentProgressReporter;
  /** Durable, per-perspective review evidence bound to the current immutable head. */
  reviewRoundRecorder?: (review: DevelopmentReviewRound) => Promise<void>;
  /** Create and durably bind independently scoped findings to child Issues. */
  separateFindingHandler?: (input: {
    round: number;
    headSha: string;
    branch: string;
    pullRequestNumber: number;
    findings: SeparateReviewFinding[];
  }) => Promise<void>;
}

function progressKeyPart(value: string): string {
  const normalized = value.toLowerCase()
    .replace(/[^a-z0-9._/-]+/g, '-')
    .replace(/^-+|-+$/g, '') || 'work';
  if (normalized.length <= 96) return normalized;
  const digest = createHash('sha256').update(normalized).digest('hex').slice(0, 16);
  return `${normalized.slice(0, 72)}-${digest}`;
}

/**
 * Resolve external mutation coordinates without using the Store-local display
 * Issue id. A split Source Issue must carry a stable planning candidate key;
 * falling back to ISSUE-0001 there would recreate the collision this boundary
 * exists to prevent.
 */
export function externalWorkIdentityFor(
  store: Store,
  issue: Issue,
  releaseId: string | null = null,
): ExternalWorkIdentity | null {
  const byKey = issue.intakeKey ? store.intakeByKey(issue.intakeKey) : undefined;
  const byProjection = store.db.intakeRecords.find((record) =>
    record.storeIssueIds.includes(issue.id));
  if (byKey && byProjection && byKey.intakeKey !== byProjection.intakeKey) {
    throw new Error(`${issue.id} has conflicting external intake identities`);
  }
  const intake = byKey ?? byProjection;
  if (!intake) return null;
  if (
    intake.storeIssueIds.length > 0
    && !intake.storeIssueIds.includes(issue.id)
  ) {
    throw new Error(`${issue.id} is not projected by intake ${intake.intakeKey}`);
  }
  const workUnitKey = intake.storeIssueIds.length <= 1
    ? 'source'
    : issue.planningCandidateKey;
  if (!workUnitKey) {
    throw new Error(
      `${issue.id} is one of multiple external work units but has no stable planning candidate key`,
    );
  }
  return {
    repository: intake.snapshot.repository,
    issueNumber: intake.snapshot.number,
    intakeKey: intake.intakeKey,
    workUnitKey,
    releaseId,
  };
}

/**
 * The store→prior-findings selection for a re-review (ISSUE-0009), extracted pure like
 * collectFindings (AC-LIVE-003) so the deterministic sub-logic is pinned by unit tests, not
 * buried in the tmux orchestration: each lens is handed ONLY its own findings from the
 * IMMEDIATELY previous attempt of THIS PR, keyed by lens. perspective=null gate runs carry
 * no lens and are excluded; attempt 1 (no previous attempt) selects nothing, so every lens
 * keeps its first-review prompt.
 */
export function priorFindingsByLens(store: Store, prId: string, attempt: number): Record<string, readonly PriorFinding[]> {
  return Object.fromEntries(
    store.db.evalRuns
      .filter((r) => r.prId === prId && r.attempt === attempt - 1 && r.perspective !== null)
      .map((r) => [r.perspective!, r.findings.map((finding, findingIndex) => ({
        criterionId: finding.criterionId,
        observed: finding.observed,
        lineageRef: finding.lineage === 'persisted' && finding.lineageRef
          ? finding.lineageRef
          : findingOriginRef({
              runId: r.id,
              prId: r.prId,
              headSha: r.headSha,
              attempt: r.attempt,
              perspective: r.perspective!,
            }, findingIndex),
      }))]),
  );
}

/**
 * Drive ONE live sample of an issue: (generate → ground → panel)* bounded repair loop, in its own
 * worktree/branch `agent/<issue>-s<n>`. `manageIssueStatus` is false under best-of-N (>1 sample)
 * so the issue's terminal status is applied once by the caller at the winner level, not per sample.
 */
export async function runLiveSample(
  store: Store, config: HarnessConfig, issue: Issue, sampleIndex: number,
  harnessRoot: string,
  opts: LiveOptions & { manageIssueStatus: boolean; resumePr?: PRType },
  log: (m: string) => void,
): Promise<SampleOutcome> {
  const contract = issue.contract!;
  const reviewContract = opts.sourceIssueReviewCriterion
    && !contract.acceptanceCriteria.some((criterion) => criterion.id === 'SOURCE-ISSUE')
    ? IssueContract.parse({
        ...contract,
        acceptanceCriteria: [
          ...contract.acceptanceCriteria,
          {
            id: 'SOURCE-ISSUE',
            severity: 'blocker',
            behavior: [
              'Evaluate the implementation against every declarative requirement and acceptance item',
              'in the separately supplied inert, frozen ready-time Source Issue snapshot.',
              `Source: ${opts.sourceIssueReviewCriterion.url}`,
              `Source updated at: ${opts.sourceIssueReviewCriterion.sourceUpdatedAt}`,
              `Snapshot digest: ${opts.sourceIssueReviewCriterion.digest}`,
            ].join('\n'),
            verification: {
              method: 'scope_check',
              expected: [
                'the immutable current head satisfies every applicable frozen Source Issue requirement',
              ],
            },
          },
        ],
      })
    : contract;
  const target = config.target!;
  const perspectives = opts.perspectives ?? PERSPECTIVES;
  const workIdentity = opts.repositoryRepairIdentities?.[issue.id]
    ?? externalWorkIdentityFor(store, issue, opts.releaseIdentity ?? null);
  if (
    workIdentity
    && config.intake?.backend === 'github'
    && canonicalGithubRepository(config.intake.repository)
      !== canonicalGithubRepository(workIdentity.repository)
  ) {
    throw new Error(
      `external work repository ${workIdentity.repository} does not match configured intake ${config.intake.repository}`,
    );
  }
  const issueKey = sampleKey(issue.id, sampleIndex, workIdentity);
  const startAttempt = opts.resumePr ? opts.resumePr.attempts + 1 : 1;
  const maxAttempts = startAttempt + config.maxRepairs;
  const manageIssueStatus = opts.manageIssueStatus;
  const generatorRoute = resolveAgentRoute(config, 'generator');

  const returnedPr = opts.resumePr ?? store.addPR(
    PR.parse({
      id: store.nextId('PR'), issueId: issue.id, branch: `agent/${issueKey}`,
      baseBranch: config.baseBranch, generator: generatorRoute.provider, attempts: 0, status: 'open',
      createdAt: nowISO(), updatedAt: nowISO(),
    }),
  );
  const pr = opts.resumePr
    ? returnedPr
    : store.db.prs.find((candidate) => candidate.id === returnedPr.id)!;
  let worktree: string | null = null; // the last completed attempt's checkout = the build at the gate
  const progressWork = progressKeyPart(issueKey);
  // Reported before the session exists, so the coordinates come from the module
  // that creates them rather than from a second copy of the layout rule.
  const expectedSession = generatorSessionName(issueKey);
  const expectedWorktree = generatorWorktreePath(harnessRoot, issueKey);

  const loop = await runBoundedRepairLoop(store, config, issue.id, pr, async (attempt, repairBrief) => {
    // 1. real generator session — carries the repair brief on attempt > 1 and reuses the worktree
    log(`▶ ${issue.id} s${sampleIndex}: generator session (attempt ${attempt}/${maxAttempts})`);
    await opts.progress?.({
      eventKey: `generation:${progressWork}:a${attempt}:start`,
      phase: 'generation',
      step: `generator session attempt ${attempt}/${maxAttempts}`,
      state: 'running',
      summary: repairBrief ? 'Applying review feedback in the isolated worktree' : 'Implementing the accepted contract',
      nextGate: 'generated revision projected to a pull request',
      sessionName: expectedSession,
      worktreePath: expectedWorktree,
      branch: pr.branch,
      pullRequestNumber: pr.externalRef?.number ?? null,
    });
    await opts.beforeProviderExecution?.();
    const sess = await (opts.generatorSession ?? runGeneratorSession)(
      config,
      {
        issue,
        contract,
        sampleIndex,
        workIdentity,
        attempt,
        repairBrief,
        resumeRef: store.getPR(pr.id)?.headSha ?? pr.headSha,
      },
      harnessRoot,
      log,
    );
    let revision: Awaited<ReturnType<typeof projectReviewRevision>> | null = null;
    if (sess.outcome === 'completed' && sess.headSha) {
      revision = await (opts.projectRevision ?? projectReviewRevision)(
        store,
        config,
        {
          pr,
          worktree: sess.worktree,
          title: renderReviewPrTitle(store, issue.id),
          headSha: sess.headSha,
          changedFiles: sess.changed,
          workIdentity,
          sampleIndex,
        },
        opts.gateRunner ?? realGhGateRunner(
          config.intake?.backend === 'github'
            ? config.intake.repository
            : undefined,
        ),
        log,
        opts.beforeCreatePr,
        opts.beforePush,
      );
      await opts.afterProjectRevision?.({
        pr: store.getPR(pr.id)!,
        headSha: revision.headSha,
      });
    }
    // Persist the actual runtime provider separately from its model/routing intent. This replaces
    // new PromptRecord writes; legacy promptRecords remain readable but are never dual-written.
    recordAgentInvocation(store, {
      subjectId: issue.id, issueId: issue.id, prId: pr.id, sampleIndex, attempt,
      role: 'generator', perspective: null, provider: sess.provider,
      model: sess.model, outcome: sess.outcome, prompt: sess.prompt,
      ...(revision
        ? { revisionId: revision.id, headSha: revision.headSha }
        : { revisionId: null, headSha: null }),
    });
    if (sess.outcome !== 'completed') {
      log(`  ⚠ ${issue.id} s${sampleIndex}: generator ${sess.outcome} — escalating, session kept alive`);
      await opts.progress?.({
        eventKey: `generation:${progressWork}:a${attempt}:blocked`,
        phase: 'human-review',
        step: `generator session ${sess.outcome}`,
        state: 'blocked',
      blocker: `generator session ${sess.outcome}; the isolated session and worktree were retained`,
      nextGate: 'human inspects or resumes the retained session',
      humanAction: 'inspect or resume the retained generator session',
        sessionName: sess.session,
        worktreePath: sess.worktree,
        branch: sess.branch,
        pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
      });
      return { stuck: true };
    }
    if (!sess.headSha || !revision) {
      log(`  ⚠ ${issue.id} s${sampleIndex}: no committed build revision — escalating`);
      await opts.progress?.({
        eventKey: `generation:${progressWork}:a${attempt}:no-revision`,
        phase: 'human-review',
        step: 'generated revision missing',
        state: 'blocked',
        blocker: 'generator completed without a committed revision',
        nextGate: 'human inspects the retained worktree',
        humanAction: 'inspect the retained worktree and create a committed revision',
        sessionName: sess.session,
        worktreePath: sess.worktree,
        branch: sess.branch,
      });
      return { stuck: true };
    }
    worktree = sess.worktree;
    await opts.progress?.({
      eventKey: `pull-request:${progressWork}:a${attempt}:projected`,
      phase: 'pull-request',
      step: 'generated revision projected',
      state: 'succeeded',
      summary: `Revision ${sess.headSha.slice(0, 12)} is available for validation`,
      nextGate: 'repository graders',
      headSha: sess.headSha,
      worktreePath: sess.worktree,
      branch: sess.branch,
      pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
    });
    if (manageIssueStatus) {
      store.setStatus(issue.id, 'ready-for-evaluation');
      store.setStatus(issue.id, 'evaluation-in-progress');
    }

    // 2. ground the checkout with real graders (real tsc/vitest)
    await opts.progress?.({
      eventKey: `validation:${progressWork}:a${attempt}:start`,
      phase: 'validation',
      step: 'repository graders',
      state: 'running',
      summary: 'Running configured typecheck and test commands in the isolated worktree',
      nextGate: 'review panel',
      headSha: sess.headSha,
      gateKey: 'repository-graders',
      worktreePath: sess.worktree,
      branch: sess.branch,
      pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
    });
    const artifact = (opts.groundBuild ?? groundArtifact)({
      contract: reviewContract,
      target,
      worktree: sess.worktree,
      branch: sess.branch,
      changed: sess.changed,
      issueId: issue.id,
      graderEnvironment: opts.graderEnvironment,
    });

    // 3. real read-only perspective sessions — each in its own detached worktree of the committed
    //    build (isolated + concurrent), collecting findings.json into the generator worktree's evalRoot.
    //    A re-review (attempt > 1) hands each lens its OWN previous-attempt findings so the reviewer
    //    attests lineage (persisted/new) per finding — never inferred downstream (ISSUE-0009).
    const priorFindings = priorFindingsByLens(store, pr.id, attempt);
    const surrogateOracleMismatchCount = surrogateOracleMismatchRevisions(
      store.db.revisionGateSnapshots,
      pr.id,
    ).length;
    const reviewStartedAt = new Date().toISOString();
    await opts.reviewRoundRecorder?.({
      round: attempt,
      headSha: revision.headSha,
      branch: sess.branch,
      pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
      outcome: 'running',
      startedAt: reviewStartedAt,
      completedAt: null,
      perspectives: [],
    });
    await opts.progress?.({
      eventKey: `review:${progressWork}:a${attempt}:start`,
      phase: 'review',
      step: 'perspective review panel',
      state: 'running',
      summary: `${perspectives.length} independent review perspective(s) evaluating the current revision`,
      nextGate: 'panel verdict',
      headSha: sess.headSha,
      reviewRound: attempt,
      reviewOutcome: 'running',
      gateKey: 'review',
      worktreePath: sess.worktree,
      branch: sess.branch,
      pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
    });
    await opts.beforeProviderExecution?.();
    const panelSessions = await (opts.perspectiveSessions ?? runPerspectiveSessions)(
      config,
      {
        worktree: sess.worktree,
        contract: reviewContract,
        perspectives,
        issueKey,
        repo: path.resolve(harnessRoot, target.repo),
        buildRef: sess.headSha,
        baseRef: target.baseRef,
        priorFindings,
        ...(opts.sourceIssueMaterial ? { untrusted: true } : {}),
        ...(opts.trustedReviewStateRoot
          ? { trustedStateRoot: opts.trustedReviewStateRoot }
          : {}),
        ...(opts.sourceIssueMaterial
          ? { sourceIssueMaterial: opts.sourceIssueMaterial }
          : {}),
        uiDesign: issue.uiDesign,
        designAuthority: issue.designAuthority,
        designReview: issue.designReview,
        surrogateOracleMismatchCount,
      },
      log,
    );
    const invocationKeys = Object.fromEntries(
      panelSessions.invocations.map((invocation) => {
        const record = recordAgentInvocation(store, {
          subjectId: issue.id, issueId: issue.id, prId: pr.id, sampleIndex, attempt,
          ...invocation,
          revisionId: revision.id,
          headSha: revision.headSha,
        });
        return [invocation.perspective, record.invocationKey];
      }),
    );

    // 4. panel grades from the findings.json files (missing/broken -> escalate); functionality is deterministic
    const panel = runPanel(
      store, config,
      {
        issueId: issue.id, prId: pr.id, contract: reviewContract, artifact, sampleIndex, attempt,
        agent: sess.provider,
        invocationKeys,
        revisionId: revision.id,
        headSha: revision.headSha,
        featureArea: issue.area,
      },
      { perspectives, grader: sessionBackedGrader(panelSessions.evalRoot) },
    );
    const completedAt = new Date().toISOString();
    await opts.reviewRoundRecorder?.({
      round: attempt,
      headSha: revision.headSha,
      branch: sess.branch,
      pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
      outcome: panel.verdict === 'approve'
        ? 'approve'
        : panel.verdict === 'needs_human'
          ? 'escalated'
          : 'request_changes',
      startedAt: reviewStartedAt,
      completedAt,
      perspectives: panel.runs.map((run) => ({
        perspective: DevelopmentReviewPerspective.parse(
          run.perspective ?? 'functionality',
        ),
        verdict: run.verdict,
        findings: run.findings.map(reviewFindingProjection),
      })),
    });
    const separateFindings = panel.runs.flatMap((run) =>
      run.findings.flatMap((finding, findingIndex): SeparateReviewFinding[] => {
        if (finding.disposition !== 'separate-issue') return [];
        if (!run.headSha || !run.perspective) {
          throw new Error('separate review finding has no immutable review identity');
        }
        return [{
          identity: finding.lineageRef ?? findingOriginRef({
            runId: run.id,
            prId: run.prId,
            headSha: run.headSha,
            attempt: run.attempt,
            perspective: run.perspective,
          }, findingIndex),
          perspective: run.perspective,
          finding: reviewFindingProjection(finding),
        }];
      }),
    );
    if (separateFindings.length > 0) {
      const pullRequestNumber = store.getPR(pr.id)?.externalRef?.number;
      if (!opts.separateFindingHandler || !pullRequestNumber) {
        throw new Error(
          'separate review findings require a durable child-Issue coordinator and parent PR',
        );
      }
      await opts.separateFindingHandler({
        round: attempt,
        headSha: revision.headSha,
        branch: sess.branch,
        pullRequestNumber,
        findings: separateFindings,
      });
    }
    await opts.progress?.({
      eventKey: `review:${progressWork}:a${attempt}:verdict`,
      phase: panel.verdict === 'approve'
        ? 'review'
        : separateFindings.length > 0
          ? 'merge'
          : 'repair',
      step: `panel verdict: ${panel.verdict}`,
      state: panel.verdict === 'approve' ? 'succeeded' : 'waiting',
      summary: panel.verdict === 'approve'
        ? 'Current revision passed the review panel'
        : separateFindings.length > 0
          ? `${separateFindings.length} independently scoped finding(s) are running as child Issues`
        : 'Current revision requires another bounded repair attempt',
      nextGate: panel.verdict === 'approve'
        ? 'merge gates'
        : separateFindings.length > 0
          ? 'all child PRs integrated into the parent branch'
          : 'generator repair session',
      headSha: sess.headSha,
      reviewRound: attempt,
      reviewOutcome: panel.verdict === 'approve' ? 'approve' : 'request-changes',
      gateKey: panel.verdict === 'approve' ? null : 'review',
      worktreePath: sess.worktree,
      branch: sess.branch,
      pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
    });
    return { panel, waitingForChildren: separateFindings.length > 0 };
  }, {
    log,
    manageIssueStatus,
    startAttempt,
    initialRepairBrief: opts.resumePr ? revisionGateRepairBrief(store, pr) : null,
  });

  if (loop.status === 'needs-human-review' && worktree) {
    await opts.progress?.({
      eventKey: `human-review:${progressWork}:review-stop`,
      phase: 'human-review',
      step: 'current-head review requires human attention',
      state: 'blocked',
      blocker: loop.escalated
        ? 'one or more required reviewer results were missing or invalid'
        : 'bounded repair attempts did not satisfy every required review perspective',
      nextGate: 'inspect or resume the retained isolated worktree; a new PR head triggers re-review',
      humanAction: 'inspect or resume the retained isolated worktree and push a new PR head',
      headSha: store.getPR(pr.id)?.headSha ?? null,
      reviewRound: store.getPR(pr.id)?.attempts || null,
      reviewOutcome: 'escalated',
      worktreePath: worktree,
      branch: pr.branch,
      pullRequestNumber: store.getPR(pr.id)?.externalRef?.number ?? null,
    });
  }

  return { ...loop, sampleIndex, prId: pr.id, approved: loop.verdict === 'approve', worktree };
}

export function revisionGateRepairBrief(store: Store, pr: PRType): RepairBrief | null {
  const currentRuns = store.db.evalRuns.filter((run) =>
    run.prId === pr.id
    && run.revisionId === pr.currentRevisionId
    && run.headSha === pr.headSha);
  const latestByPerspective = new Map<string, (typeof currentRuns)[number]>();
  for (const run of currentRuns) {
    const perspective = run.perspective ?? 'functionality';
    const previous = latestByPerspective.get(perspective);
    if (
      !previous
      || run.attempt > previous.attempt
      || (run.attempt === previous.attempt && run.createdAt > previous.createdAt)
    ) {
      latestByPerspective.set(perspective, run);
    }
  }
  const reviewFindings = [...latestByPerspective].flatMap(([perspective, run]) =>
    run.findings.map((finding) => ({
      ...finding,
      criterionId: `${perspective}:${finding.criterionId}`,
    })));
  if (reviewFindings.length > 0) {
    const sourceRun = [...latestByPerspective.values()]
      .sort((left, right) => right.createdAt.localeCompare(left.createdAt))[0]!;
    const instructions = reviewFindings.flatMap((finding) =>
      finding.requiredFix.length > 0
        ? finding.requiredFix.map((fix) => `[${finding.criterionId}] ${fix}`)
        : [`[${finding.criterionId}] ${finding.observed}`]);
    return {
      fromEvalRunId: sourceRun.id,
      findings: reviewFindings,
      instructions,
    };
  }

  const snapshot = store.db.revisionGateSnapshots
    .filter((row) => row.prId === pr.id && row.decision === 'changes-requested')
    .at(-1);
  if (!snapshot) return null;
  const classifiedBlockingReasons = snapshot.blockingReasons.length > 0
    ? snapshot.blockingReasons
    // Migration shim for snapshots persisted before blockingReasons/pendingReasons
    // were separated. These strings are rendered by evaluateRevisionGate.
    : snapshot.reasons.filter((reason) =>
      !reason.startsWith('required check pending:')
      && !reason.startsWith('missing review:')
      && reason !== 'mergeability is unknown'
      && reason !== 'pull request is draft');
  if (classifiedBlockingReasons.length === 0) return null;
  const threadInstructions = snapshot.blockingReviewThreads.map((thread) =>
    `${thread.path ?? 'PR'}${thread.line ? `:${thread.line}` : ''}: ${thread.body}`);
  const otherInstructions = classifiedBlockingReasons.filter(
    (reason) => !reason.startsWith('unresolved blocking review thread:'),
  );
  const instructions = [...threadInstructions, ...otherInstructions];
  if (instructions.length === 0) return null;
  return {
    fromEvalRunId: `revision-gate:${snapshot.id}`,
    instructions,
    findings: instructions.map((reason, index) => ({
      criterionId: `PR-GATE-${index + 1}`,
      severity: 'major',
      expected: 'current PR revision passes every merge gate',
      observed: reason,
      reproductionSteps: [`Inspect PR revision ${snapshot.headSha}`],
      evidence: {},
      requiredFix: [reason],
    })),
  };
}

/**
 * Drive ONE ai-managed issue through best-of-N live samples → the review gate (ADR-0006 E5). Each
 * sample is a real generator session grounded in real tsc/vitest and reviewed by real read-only
 * perspective sessions, bounded by config.maxRepairs+1 with cross-perspective repair (AC-REPAIR-*).
 * Default is one sample, first-approve-stop; opts.measure runs all opts.samples for pass@k/pass^k.
 * The WINNING sample (first to approve) is projected to the gate; a stuck/exhausted issue with no
 * approver escalates to needs-human-review (session kept alive). The seams are each unit-tested;
 * this orchestration drives live tmux + Claude and is not.
 */
export async function driveIssueLive(
  store: Store,
  config: HarnessConfig,
  issue: Issue,
  harnessRoot: string = process.cwd(),
  opts: LiveOptions = {},
  log: (m: string) => void = () => {},
): Promise<DriveResult> {
  if (!issue.contract) throw new Error(`${issue.id} has no contract`);
  if (!config.target) throw new Error('driveIssueLive requires config.target (a real repo)');
  const resumePr = issue.status === 'changes-requested'
    ? [...store.db.prs].reverse().find((pr) =>
      pr.issueId === issue.id && pr.status !== 'merged' && pr.status !== 'closed')
    : undefined;
  if (issue.status === 'changes-requested' && !resumePr) {
    throw new Error(`${issue.id} is changes-requested but has no resumable PR`);
  }
  // PR-native delivery owns one stable PR branch per work unit. Independent best-of-N
  // candidates remain available for store-local measurement, where they do not leak
  // losing candidate PRs into GitHub.
  const n = resumePr || (config.gate?.backend ?? 'store') === 'github'
    ? 1
    : Math.max(1, opts.samples ?? config.samples);
  const measure = opts.measure ?? false;
  const single = n === 1; // single sample keeps the loop's own status management (unchanged behaviour)
  const resumeSampleIndex = resumePr
    ? store.db.evalRuns.find((run) => run.prId === resumePr.id)?.sampleIndex ?? 0
    : 0;

  if (issue.status === 'contract-drafted') store.setStatus(issue.id, 'ready-for-generation');
  store.setStatus(issue.id, 'generation-in-progress');

  const { samples, winner } = await runBestOfN(n, measure, (s) =>
    runLiveSample(
      store,
      config,
      issue,
      resumePr ? resumeSampleIndex : s,
      harnessRoot,
      { ...opts, manageIssueStatus: single, ...(resumePr ? { resumePr } : {}) },
      log,
    ));

  // Terminal issue status. Single-sample already had it managed inside the loop; best-of-N applies
  // it once here so the resting state reflects the WINNER, not whichever sample happened to run last.
  if (!single) {
    if (winner) {
      store.setStatus(issue.id, 'ready-for-evaluation');
      store.setStatus(issue.id, 'evaluation-in-progress');
      applyPanelVerdict(store, issue.id, 'approve', config.gate?.backend ?? 'store');
    } else if (store.getIssue(issue.id)!.status !== 'needs-human-review') {
      store.setStatus(issue.id, 'needs-human-review'); // no sample converged -> escalate
    }
  }

  // The PR was projected before its first review. Once all internal perspectives approve,
  // consume only current-head evidence and use an expected-SHA merge.
  if (winner?.worktree && (config.gate?.backend ?? 'store') === 'github') {
    const pr = store.getPR(winner.prId)!;
    await opts.progress?.({
      eventKey: `merge:${progressKeyPart(issue.id)}:start`,
      phase: 'merge',
      step: 'current-head merge gates',
      state: 'running',
      summary: 'Checking required reviews, checks, threads, and expected head SHA',
      nextGate: 'GitHub merge confirmation',
      headSha: pr.headSha,
      gateKey: 'merge',
      worktreePath: winner.worktree,
      branch: pr.branch,
      pullRequestNumber: pr.externalRef?.number ?? null,
    });
    await opts.beforeMerge?.();
    await opts.beforeRelease?.();
    const result = await autoMergeCurrentRevision(
      store,
      config,
      pr,
      opts.prNativeRunner ?? realPrNativeGithubRunner(config.gate?.mergeMethod),
      path.resolve(harnessRoot, config.target.repo),
      (opts.perspectives ?? PERSPECTIVES).map((perspective) => perspective.key),
      {
        beforeRelease: opts.assertReleasePermit,
        authorizeMerge: opts.authorizeMerge,
        completeMerge: opts.completeMerge,
      },
    );
    log(
      `  ⇩ ${issue.id}: revision ${result.headSha?.slice(0, 12) ?? 'unobserved'} `
      + `→ ${result.decision}${result.reasons.length ? ` (${result.reasons.join('; ')})` : ''}`,
    );
    await opts.progress?.({
      eventKey: `merge:${progressKeyPart(issue.id)}:${result.decision}`,
      phase: result.merged ? 'completed' : 'merge',
      step: result.merged ? 'pull request merged' : `merge gate: ${result.decision}`,
      state: result.merged ? 'succeeded' : result.decision === 'pending' ? 'waiting' : 'blocked',
      summary: result.merged ? 'Implementation released' : result.reasons.join('; '),
      nextGate: result.merged ? null : 'next GitHub pull request reconciliation',
      blocker: result.merged || result.decision === 'pending' ? null : result.reasons.join('; '),
      headSha: result.headSha ?? pr.headSha,
      gateKey: result.merged ? null : 'merge',
      humanAction: result.merged || result.decision === 'pending'
        ? null
        : 'resolve the listed merge blockers on the current expected head',
      worktreePath: winner.worktree,
      branch: pr.branch,
      pullRequestNumber: pr.externalRef?.number ?? null,
    });
  }

  const status = store.getIssue(issue.id)!.status;
  const chosen = winner ?? samples[samples.length - 1]!; // the winner, else the last sample tried
  log(`  = ${issue.id}: ${samples.length} sample(s)${measure ? ' [measure]' : ''}, ${winner ? `winner s${winner.sampleIndex}` : 'none approved'} → ${status}`);
  return {
    issueId: issue.id, prId: chosen.prId, verdict: chosen.verdict, status,
    gateFailed: chosen.gateFailed, escalated: chosen.escalated, attempts: chosen.attempts,
    exhausted: chosen.exhausted,
    waitingForChildren: chosen.waitingForChildren,
    sampleCount: samples.length,
  };
}

/** One live turn over the ai-managed queue (the watch daemon's live run-once). */
export async function runLoopLive(
  store: Store, config: HarnessConfig, harnessRoot: string = process.cwd(),
  opts: LiveOptions = {}, log: (m: string) => void = () => {},
): Promise<DriveResult[]> {
  const repairQueue = store.db.issues
    .filter(
      (issue) => issue.status === 'changes-requested'
        && issue.assignedAgent === resolvedGeneratorProvider(config)
        && !store.db.prs.some(
          (pr) => pr.issueId === issue.id && (
            (
              pr.origin === 'repository-discovery'
              && !opts.repositoryRepairIdentities?.[issue.id]
            )
            || (
              pr.externalRef !== null
              && (
                pr.headSha === null
                || pr.agentGeneratedHeadSha !== pr.headSha
              )
            )
          ),
        ),
    );
  const queue = [...pollable(store, config), ...repairQueue]
    .sort((a, b) => a.id.localeCompare(b.id));
  const cap = resolveConcurrentIssueCap(config);
  log(`queue: ${queue.length} ai-managed issue(s) [generator=${resolvedGeneratorProvider(config)}, cap=${cap}]`);
  // Dependency blocks are surfaced every turn (AC-DAG-001): an issue held back by the
  // guard names what it waits on in the log — it never just vanishes from the queue.
  // Under parallelism this stays an invariant (AC-PAR-002): the in-flight set is drawn
  // from `pollable` alone, so a dependency-blocked issue can never enter it.
  for (const b of blockedByDependencies(store, config)) {
    log(formatBlockedLine(b.issueId, b.waitingOn));
  }
  // Bounded fan-out (AC-PAR-001): at most `cap` issues in flight; excess waits and takes
  // slots in stable queue (id) order, so every queued issue is driven — no starvation.
  // Results keep queue order, and cap 1 reproduces today's sequential drive exactly.
  const drive = opts.driveIssue ?? ((issue: Issue) => driveIssueLive(store, config, issue, harnessRoot, opts, log));
  // The peak is OBSERVED here, at the dispatch seam — the one place every in-flight
  // interval passes through, whatever worker drives the issue — so the recorded fact
  // holds for the real driver and the injected one alike (AC-PAR-003).
  let inFlight = 0;
  let peak = 0;
  const results = await mapPool(queue, cap, async (issue) => {
    inFlight += 1;
    peak = Math.max(peak, inFlight);
    try {
      return await drive(issue);
    } finally {
      inFlight -= 1;
    }
  });
  // The turn's concurrency facts persist in the store (never log-only): metrics read
  // the latest record as the turn instruments, null when no turn was ever recorded.
  store.addTurnRecord(
    TurnRecord.parse({
      id: store.nextId('TURN'), cap, issuesDriven: queue.length, peakConcurrency: peak, createdAt: nowISO(),
    }),
  );
  // ③ every live turn ends by capturing failures into the regression registry,
  // re-verifying the bound registry against the target's real graders, and reporting
  // (never enacting) improvement suggestions — ADR-0007 I2.
  improveTick(store, log, {
    config,
    regressReport: opts.regressReport,
    graderEnvironment: opts.graderEnvironment,
  });
  store.save();
  return results;
}
