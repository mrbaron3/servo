import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { isDeepStrictEqual } from 'node:util';
import type { HarnessConfig } from '../config.js';
import type { TargetGraderConfig } from '../config.js';
import { DEFAULT_CONFIG } from '../config.js';
import { Store } from '../store/store.js';
import {
  PlanningProviderInvocationError,
  runGithubDevelopmentTurn,
  type GithubDevelopmentTurnResult,
} from '../intake/development-turn.js';
import {
  realGithubIssueRunner,
  type GithubIssueRunner,
} from '../intake/github-issues.js';
import { runPlanningSession } from '../intake/planning-session.js';
import { runUiDesignSession } from '../intake/ui-design-session.js';
import { runGeneratorSession } from '../pipeline/execution/session.js';
import { groundArtifact } from '../pipeline/execution/grade.js';
import {
  runPerspectiveSessions,
} from '../pipeline/execution/perspective-session.js';
import { sourceIssueReviewMaterial } from '../pipeline/execution/repository-pr.js';
import type { LiveOptions } from '../pipeline/execution/live.js';
import {
  realGhGateRunner,
  type GhGateRunner,
} from '../pipeline/execution/gate.js';
import {
  realPrNativeGithubRunner,
  reconcileExternalEpicClosure,
  type AutoMergeOptions,
  type PrNativeGithubRunner,
} from '../pipeline/execution/pr-native.js';
import type {
  Lease,
  RunnerJobPayloadV1,
} from '../control-store/types.js';
import type { PostgresControlStore } from '../control-store/store.js';
import type { ReleaseRuntimeConfiguration } from '../evidence/release-projection.js';
import {
  projectReleaseMerge,
  projectReleasePreMerge,
  projectReleaseProgress,
} from '../evidence/release-projection.js';
import { GithubIssueSnapshot, type PR } from '../domain/schema.js';
import {
  realPlanningHumanReviewGitHub,
  renderPlanningHumanReviewComment,
  type PlanningHumanReviewGitHub,
} from '../triage/planning-human-review.js';
import { RunnerExecutionError } from './errors.js';
import type { RunnerLeaseFence } from './guard.js';
import { isolatedGraderEnvironment } from './security.js';
import type { PreparedRunnerWorkspace } from './workspace.js';
import {
  type DevelopmentProgressUpdate,
} from '../domain/development-progress.js';
import { linkedParentIssueNumber } from '../intake/parent-link.js';
import {
  RUNNER_DEPENDENCY_PATH,
  RUNNER_DEPENDENCY_ROOT,
} from '../pipeline/execution/runner-sandbox.js';
import {
  realReviewChildGithub,
  type ReviewChildGithub,
} from '../pipeline/execution/review-children.js';

interface AgentOpsAdapterResultBase {
  headSha: string | null;
  pullRequestNumber: number | null;
  developmentTurn: GithubDevelopmentTurnResult;
  /** Keep the isolated attempt checkout available to the operator. */
  retainWorkspace?: boolean;
}

export type AgentOpsAdapterResult = AgentOpsAdapterResultBase & (
  | {
    outcome: 'completed';
    humanReview: null;
  }
  | {
    outcome: 'needs-human-review';
    humanReview: {
      issueNumber: number;
      reasons: string[];
      commentUrl: string;
    };
  }
);

export interface AgentOpsAdapterInput {
  lease: Lease;
  payload: RunnerJobPayloadV1;
  workspace: PreparedRunnerWorkspace;
  fence: RunnerLeaseFence;
  provider: 'codex' | 'claude';
  controlStore?: PostgresControlStore;
  releaseRuntime?: ReleaseRuntimeConfiguration | null;
  /** Projects the retained outer attempt checkout to an immutable fetched head. */
  projectWorkspaceHead?: (headSha: string) => void;
  log: (message: string) => void;
}

export interface AgentOpsRunnerAdapter {
  execute(input: AgentOpsAdapterInput): Promise<AgentOpsAdapterResult>;
}

export interface ExistingAgentOpsAdapterDependencies {
  issueRunner?: (cwd: string) => GithubIssueRunner;
  gateRunner?: (repository: string) => GhGateRunner;
  prNativeRunner?: (
    mergeMethod: 'squash' | 'merge' | 'rebase',
    repository: string,
  ) => PrNativeGithubRunner;
  planningRunner?: typeof runPlanningSession;
  uiDesignRunner?: typeof runUiDesignSession;
  generatorSession?: typeof runGeneratorSession;
  perspectiveSessions?: typeof runPerspectiveSessions;
  groundBuild?: LiveOptions['groundBuild'];
  regressReport?: LiveOptions['regressReport'];
  planningHumanReviewGithub?: (cwd: string) => PlanningHumanReviewGitHub;
  reviewChildGithub?: (cwd: string) => ReviewChildGithub;
}

/**
 * A current immutable head that already has durable request-changes evidence
 * is a completed PR-event review, not a transient reconciliation failure.
 * Reconciliation can project the PR/revision back to open/reviewing when other
 * perspective evidence is intentionally absent after a deterministic veto, so
 * the revision-bound EvalRun is the fallback source of truth.
 */
export function hasDurableCurrentHeadRequestChanges(
  store: Store,
  pr: PR,
): boolean {
  if (pr.headSha === null || pr.currentRevisionId === null) return false;
  const revision = store.revisionForHead(pr.id, pr.headSha);
  if (!revision || revision.id !== pr.currentRevisionId) return false;
  if (
    pr.status === 'changes-requested'
    || revision.status === 'changes-requested'
  ) {
    return true;
  }
  return store.db.evalRuns.some((run) =>
    run.prId === pr.id
    && run.revisionId === revision.id
    && run.headSha === revision.headSha
    && run.verdict === 'request_changes');
}

/** A rejected or explicitly escalated current-head review waits for external action. */
export function hasDurableCurrentHeadReviewStop(
  store: Store,
  pr: PR,
): boolean {
  if (hasDurableCurrentHeadRequestChanges(store, pr)) return true;
  if (pr.headSha === null || pr.currentRevisionId === null) return false;
  const revision = store.revisionForHead(pr.id, pr.headSha);
  if (!revision || revision.id !== pr.currentRevisionId) return false;
  // `failed` is also used for externally closed PRs and unverified external
  // merges. Only a revision-bound panel escalation is a successful review
  // stop; transport/reconciliation failures must continue into the release
  // path and fail visibly instead of being acknowledged as completed.
  return store.db.evalRuns.some((run) =>
    run.prId === pr.id
    && run.revisionId === revision.id
    && run.headSha === revision.headSha
    && run.verdict === 'needs_human');
}

function baseBranch(ref: string): string {
  return ref.replace(/^refs\/heads\//, '');
}

function missingIntegratedHeads(
  worktree: string,
  currentHead: string,
  integratedHeads: readonly string[],
): string[] {
  return integratedHeads.filter((integratedHead) => {
    const result = spawnSync(
      'git',
      ['-C', worktree, 'merge-base', '--is-ancestor', integratedHead, currentHead],
      {
        encoding: 'utf8',
        timeout: 10_000,
        maxBuffer: 1024 * 1024,
        env: {
          PATH: process.env.PATH,
          HOME: '/home/agentops',
          GIT_CONFIG_GLOBAL: '/dev/null',
          GIT_CONFIG_NOSYSTEM: '1',
        },
      },
    );
    if (result.error) {
      throw new RunnerExecutionError(
        'workspace_failure',
        `cannot verify cumulative child ancestry: ${result.error.message}`,
        true,
      );
    }
    return result.status !== 0;
  });
}

interface NodePackageManifest {
  scripts?: Record<string, unknown>;
  dependencies?: Record<string, unknown>;
  devDependencies?: Record<string, unknown>;
}

/**
 * Select a bounded grader profile from repository-owned, immutable-at-claim
 * metadata. Repository identity is intentionally irrelevant. We support the
 * existing TypeScript/Vitest profile and a dependency-backed Node contract
 * checker expressed as one direct `node relative/script` command. Shell
 * operators, package-manager installs, and arbitrary job-supplied commands are
 * never accepted.
 */
export function inferRepositoryGraders(
  worktreePath: string,
): TargetGraderConfig {
  const manifestPath = path.join(worktreePath, 'package.json');
  let manifest: NodePackageManifest;
  try {
    const stat = fs.statSync(manifestPath);
    if (!stat.isFile() || stat.size > 1024 * 1024) {
      throw new Error('package manifest is not a bounded regular file');
    }
    manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8')) as NodePackageManifest;
  } catch {
    throw new RunnerExecutionError(
      'workspace_failure',
      'registered repository has no supported bounded grader profile',
      false,
    );
  }
  const dependencies = {
    ...(manifest.dependencies ?? {}),
    ...(manifest.devDependencies ?? {}),
  };
  if ('typescript' in dependencies && 'vitest' in dependencies) {
    const tsc = `node ${RUNNER_DEPENDENCY_PATH}/typescript/bin/tsc`;
    const vitest = `node ${RUNNER_DEPENDENCY_PATH}/vitest/vitest.mjs run --configLoader runner`;
    return {
      typecheck: `${tsc} --noEmit`,
      unit_tests: vitest,
      commands: {
        build: tsc,
        typecheck: `${tsc} --noEmit`,
        unit_test: vitest,
      },
    };
  }
  const testScript = manifest.scripts?.test;
  const match = typeof testScript === 'string'
    ? /^node ([A-Za-z0-9._/-]+\.(?:mjs|cjs|js))$/.exec(testScript.trim())
    : null;
  if (match) {
    const relativeScript = match[1]!;
    if (
      relativeScript.split('/').some((segment) =>
        segment === '' || segment === '.' || segment === '..')
    ) {
      throw new RunnerExecutionError(
        'workspace_failure',
        'repository contract checker path is unsafe',
        false,
      );
    }
    const scriptPath = path.resolve(worktreePath, relativeScript);
    if (
      !scriptPath.startsWith(`${path.resolve(worktreePath)}${path.sep}`)
      || !fs.existsSync(scriptPath)
      || !fs.statSync(scriptPath).isFile()
    ) {
      throw new RunnerExecutionError(
        'workspace_failure',
        'repository contract checker is absent',
        false,
      );
    }
    const command = `node ${relativeScript}`;
    return {
      typecheck: command,
      commands: {
        build: command,
        typecheck: command,
        api_test: command,
        db_state_check: command,
      },
    };
  }
  throw new RunnerExecutionError(
    'workspace_failure',
    'registered repository has no supported bounded grader profile',
    false,
  );
}

export function repositoryGraderProfileEvidence(
  worktreePath: string,
  claimedProfile: TargetGraderConfig,
): {
  graderProfileValid: boolean;
  graderProfileError?: string;
} {
  let observedProfile: TargetGraderConfig;
  try {
    observedProfile = inferRepositoryGraders(worktreePath);
  } catch (error) {
    return {
      graderProfileValid: false,
      graderProfileError:
        `built checkout has no supported bounded profile: `
        + `${error instanceof Error ? error.message : String(error)}`,
    };
  }
  if (!isDeepStrictEqual(observedProfile, claimedProfile)) {
    return {
      graderProfileValid: false,
      graderProfileError:
        `built checkout profile differs from the claimed profile `
        + `(claimed=${JSON.stringify(claimedProfile)}, `
        + `observed=${JSON.stringify(observedProfile)})`,
    };
  }
  return { graderProfileValid: true };
}

function scopedIssueRunner(
  fence: RunnerLeaseFence,
  delegate: GithubIssueRunner,
  issueNumber: number,
): GithubIssueRunner {
  return {
    listReadyIssues(repository, readyLabel) {
      return delegate
        .listReadyIssues(repository, readyLabel)
        .filter((issue) => issue.number === issueNumber);
    },
    claimIssue(repository, number, readyLabel, claimedLabel) {
      if (number !== issueNumber) {
        throw new RunnerExecutionError(
          'provider_failure',
          `runner job cannot claim unexpected issue #${number}`,
          false,
          'provider',
        );
      }
      fence.consume('claim');
      delegate.claimIssue(repository, number, readyLabel, claimedLabel);
    },
    ...(delegate.viewIssue
      ? {
          viewIssue(repository: string, number: number) {
            if (number !== issueNumber) {
              throw new RunnerExecutionError(
                'provider_failure',
                `runner job cannot read unexpected issue #${number}`,
                false,
                'provider',
              );
            }
            return delegate.viewIssue!(repository, number);
          },
        }
      : {}),
  };
}

function guardedGateRunner(
  fence: RunnerLeaseFence,
  delegate: GhGateRunner,
): GhGateRunner {
  return {
    preflightPr(cwd, args) {
      return delegate.preflightPr(cwd, args);
    },
    pushBranch(worktree, branch) {
      fence.consume('push');
      delegate.pushBranch(worktree, branch);
    },
    createPr(cwd, args) {
      fence.consume('push');
      return delegate.createPr(cwd, args);
    },
    viewPr: delegate.viewPr.bind(delegate),
  };
}

function guardedPrNativeRunner(
  fence: RunnerLeaseFence,
  delegate: PrNativeGithubRunner,
  allowedPullRequestNumber?: number,
  allowedRepository?: string,
): PrNativeGithubRunner {
  const assertPullRequest = (number: number): void => {
    if (
      allowedPullRequestNumber !== undefined
      && number !== allowedPullRequestNumber
    ) {
      throw new RunnerExecutionError(
        'provider_failure',
        `runner job cannot operate on unexpected PR #${number}`,
        false,
        'provider',
      );
    }
  };
  return {
    viewRevision(cwd, prNumber) {
      assertPullRequest(prNumber);
      return delegate.viewRevision(cwd, prNumber);
    },
    ...(delegate.observeRelease
      ? {
          observeRelease(
            cwd,
            repository,
            issueNumber,
            prNumber,
            expectedHead,
            integrationBranch,
          ) {
            assertPullRequest(prNumber);
            return delegate.observeRelease!(
              cwd,
              repository,
              issueNumber,
              prNumber,
              expectedHead,
              integrationBranch,
            );
          },
        }
      : {}),
    merge(cwd, prNumber, expectedHeadSha) {
      assertPullRequest(prNumber);
      fence.consume('merge');
      delegate.merge(cwd, prNumber, expectedHeadSha);
    },
    closeIssue(cwd, repository, issueNumber) {
      fence.consume('release');
      delegate.closeIssue(cwd, repository, issueNumber);
    },
    ...(delegate.listOpenPullRequests
      ? {
          listOpenPullRequests(cwd, baseBranch) {
            return delegate.listOpenPullRequests!(cwd, baseBranch)
              .filter((pullRequest) =>
                allowedPullRequestNumber === undefined
                || pullRequest.number === allowedPullRequestNumber);
          },
        }
      : {}),
    ...(delegate.listRepositoryIssues
      ? {
          listRepositoryIssues(cwd, requestedRepository) {
            if (
              allowedRepository !== undefined
              && requestedRepository !== allowedRepository
            ) {
              throw new RunnerExecutionError(
                'provider_failure',
                `runner job cannot read the Issue inventory of ${requestedRepository}`,
                false,
                'provider',
              );
            }
            return delegate.listRepositoryIssues!(cwd, requestedRepository);
          },
        }
      : {}),
    ...(delegate.fetchPullRequestHead
      ? {
          fetchPullRequestHead(cwd, prNumber, expectedHeadSha, headRefName, baseRefName) {
            assertPullRequest(prNumber);
            return delegate.fetchPullRequestHead!(
              cwd,
              prNumber,
              expectedHeadSha,
              headRefName,
              baseRefName,
            );
          },
        }
      : {}),
    ...(delegate.pullRequestChangedFiles
      ? {
          pullRequestChangedFiles(cwd, prNumber) {
            assertPullRequest(prNumber);
            return delegate.pullRequestChangedFiles!(cwd, prNumber);
          },
        }
      : {}),
  };
}

function guardedPlanningHumanReviewGithub(
  fence: RunnerLeaseFence,
  delegate: PlanningHumanReviewGitHub,
  repository: string,
  issueNumber: number,
): PlanningHumanReviewGitHub {
  const assertScope = (
    requestedRepository: string,
    requestedIssueNumber: number,
  ): void => {
    if (
      requestedRepository !== repository
      || requestedIssueNumber !== issueNumber
    ) {
      throw new RunnerExecutionError(
        'provider_failure',
        `runner job cannot surface human review for unexpected Issue `
        + `${requestedRepository}#${requestedIssueNumber}`,
        false,
        'release',
      );
    }
  };
  return {
    ensureManagedComment(
      requestedRepository,
      requestedIssueNumber,
      comment,
    ) {
      assertScope(requestedRepository, requestedIssueNumber);
      fence.consume('release');
      return delegate.ensureManagedComment(
        requestedRepository,
        requestedIssueNumber,
        comment,
      );
    },
    removeClaimedLabel(
      requestedRepository,
      requestedIssueNumber,
      claimedLabel,
    ) {
      assertScope(requestedRepository, requestedIssueNumber);
      fence.consume('release');
      delegate.removeClaimedLabel(
        requestedRepository,
        requestedIssueNumber,
        claimedLabel,
      );
    },
  };
}

function runnerConfig(input: AgentOpsAdapterInput): HarnessConfig {
  const repository = `${input.payload.repository.owner}/${input.payload.repository.name}`;
  const branch = baseBranch(input.payload.target.baseRef);
  const route = { provider: input.provider };
  const systemDir = path.join(input.workspace.worktreePath, 'docs', '_system');
  return {
    ...DEFAULT_CONFIG,
    generator: input.provider,
    samples: 1,
    maxConcurrentIssues: 1,
    baseBranch: branch,
    target: {
      repo: input.workspace.worktreePath,
      baseRef: input.payload.target.baseRef,
      ...(fs.existsSync(systemDir) ? { systemDir } : {}),
      protectedPaths: ['.git', '.github/workflows', 'node_modules'],
      graders: inferRepositoryGraders(input.workspace.worktreePath),
    },
    gate: {
      backend: 'github',
      baseBranch: branch,
      requiredChecks: input.payload.execution.requiredChecks,
      mergeMethod: input.payload.execution.mergeMethod,
    },
    intake: {
      backend: 'github',
      repository,
      readyLabel: input.payload.execution.readyLabel,
      claimedLabel: input.payload.execution.claimedLabel,
    },
    routes: {
      generator: route,
      planning: route,
      uiDesign: route,
      reviewer: route,
    },
  };
}

/**
 * Adapter onto the existing planning → generation → PR-native current-head
 * review/repair/test → expected-SHA merge → release path. The runner adds
 * fences at side-effect seams; it does not implement an alternate fast path.
 */
export class ExistingAgentOpsRunnerAdapter implements AgentOpsRunnerAdapter {
  constructor(
    private readonly dependencies: ExistingAgentOpsAdapterDependencies = {},
  ) {}

  async execute(input: AgentOpsAdapterInput): Promise<AgentOpsAdapterResult> {
    const event = input.payload.event;
    const repository =
      `${input.payload.repository.owner}/${input.payload.repository.name}`;
    process.env.AGENTOPS_GITHUB_REPOSITORY = repository;
    process.env.AGENTOPS_RUNNER_REGISTRATION_ROOT =
      input.workspace.registrationRoot;
    process.env[RUNNER_DEPENDENCY_ROOT] = RUNNER_DEPENDENCY_PATH;
    const store = new Store(input.workspace.statePath);
    if (!Store.isInitialized(input.workspace.statePath)) store.save();
    let progressParentIssueNumber: number | null = null;
    const reportProgress = async (
      event: DevelopmentProgressUpdate,
    ): Promise<void> => {
      if (
        !input.controlStore
        || typeof input.controlStore.recordDevelopmentProgress !== 'function'
      ) return;
      try {
        await input.controlStore.recordDevelopmentProgress({
          token: input.lease.token,
          workerId: input.lease.workerId,
          event: {
            ...event,
            eventKey: `lease-a${input.lease.attemptNumber}:${event.eventKey}`,
            parentIssueNumber: progressParentIssueNumber,
          },
        });
      } catch (error) {
        // Delivery work remains authoritative, but a missing progress event is
        // loud and queryable as an operational defect instead of being inferred.
        input.log(
          `⚠ durable development progress failed for ${event.eventKey}: `
          + `${error instanceof Error ? error.message : String(error)}`,
        );
      }
    };
    const config = runnerConfig(input);
    const releaseId = input.lease.job.releaseId ?? null;
    let release = releaseId === null
      ? null
      : await input.controlStore?.getRelease(releaseId) ?? null;
    if (releaseId !== null && !release) {
      throw new RunnerExecutionError(
        'internal_failure',
        `runner release ${releaseId} is unavailable`,
        false,
        'release',
      );
    }
    if (release && !input.releaseRuntime) {
      throw new RunnerExecutionError(
        'startup_isolation_failure',
        'release receipt runtime provenance is not configured',
        false,
        'release',
      );
    }
    let recoveryPullRequest = event.kind === 'issue'
      ? release?.pullRequest ?? null
      : null;
    if (
      event.kind === 'issue'
      && release?.status === 'collecting'
      && release.pullRequest === null
      && input.controlStore
      && typeof input.controlStore.recoverRequirementsUpgradePullRequest === 'function'
    ) {
      await input.fence.arm('release');
      input.fence.consume('release');
      const preservedPullRequest = await input.controlStore
        .recoverRequirementsUpgradePullRequest({
          jobId: input.lease.job.id,
          releaseId: release.id,
        });
      if (preservedPullRequest !== null) {
        release = await input.controlStore.getRelease(release.id);
        if (!release || release.pullRequest !== preservedPullRequest) {
          throw new RunnerExecutionError(
            'internal_failure',
            'requirements-upgrade pull request recovery did not converge',
            false,
            'release',
          );
        }
        recoveryPullRequest = preservedPullRequest;
      }
    }
    const activePullRequest = event.kind === 'pull_request'
      ? event.number
      : recoveryPullRequest;
    await reportProgress({
      eventKey: 'intake:runner-start',
      phase: 'intake',
      step: 'runner workspace prepared',
      state: 'running',
      summary: activePullRequest === null
        ? `Isolated attempt ${input.lease.attemptNumber} started`
        : `Reviewing existing PR #${activePullRequest} in an isolated attempt`,
      nextGate: activePullRequest === null
        ? 'planning session'
        : 'current pull request head review',
      worktreePath: input.workspace.worktreePath,
      pullRequestNumber: activePullRequest,
    });
    const realIssueRunner = (
      this.dependencies.issueRunner ?? realGithubIssueRunner
    )(input.workspace.worktreePath);
    const sourceIssueAuthority = release
      ? await (async () => {
          if (
            release.repository !== repository
            || (
              activePullRequest !== null
              && release.pullRequest !== null
              && release.pullRequest !== activePullRequest
            )
          ) {
            throw new RunnerExecutionError(
              'unknown_job_contract',
              'release Source Issue authority does not match the active pull request',
              false,
              'claim',
            );
          }
          const canonical = await input.controlStore?.getReleaseSourceIssue(release.id)
            ?? null;
          if (
            canonical
            && input.payload.sourceIssue
            && canonical.digest !== input.payload.sourceIssue.digest
          ) {
            throw new RunnerExecutionError(
              'artifact_integrity',
              'runner job Source Issue snapshot conflicts with its release authority',
              false,
              'claim',
            );
          }
          const frozen = canonical ?? input.payload.sourceIssue ?? null;
          if (!frozen) {
            // A pre-snapshot release may already be irreversibly merged. It is
            // safe to acknowledge that terminal fact, but never infer a parent
            // Issue from mutable current text. The terminal branch below makes
            // the skipped epic reconciliation explicit to the operator.
            if (release.status === 'merged') return null;
            await reportProgress({
              eventKey: 'human-review:missing-source-snapshot',
              phase: 'human-review',
              step: 'ready-time Source Issue snapshot required',
              state: 'blocked',
              blocker: [
                'This release predates immutable Source Issue snapshots.',
                'Mutable current Issue text will not be injected into review authority.',
              ].join(' '),
              nextGate: `human removes ${input.payload.execution.claimedLabel}, then reapplies ${input.payload.execution.readyLabel} to attest and freeze current Issue requirements`,
              humanAction: `remove ${input.payload.execution.claimedLabel}, then reapply ${input.payload.execution.readyLabel} to attest current requirements`,
              worktreePath: input.workspace.worktreePath,
              pullRequestNumber: activePullRequest,
            });
            throw new RunnerExecutionError(
              'artifact_integrity',
              `release has no immutable ready-time Source Issue snapshot; human must remove ${input.payload.execution.claimedLabel} and reapply ${input.payload.execution.readyLabel}`,
              false,
              'claim',
            );
          }
          if (
            frozen.repository !== release.repository
            || frozen.number !== release.issueNumber
          ) {
            throw new RunnerExecutionError(
              'artifact_integrity',
              'frozen Source Issue snapshot does not match the release authority',
              false,
              'claim',
            );
          }
          const authoritativeBody = frozen.comments.length === 0
            ? frozen.body
            : [
                frozen.body,
                '',
                '--- Authoritative Issue comments frozen at ready time ---',
                ...frozen.comments.map((comment) => (
                  `[comment ${comment.id} by ${comment.author} at ${comment.updatedAt}]\n${comment.body}`
                )),
              ].join('\n');
          return {
            issue: GithubIssueSnapshot.parse({
              repository: frozen.repository,
              number: frozen.number,
              externalId: `release:${release.id}:issue:${frozen.number}`,
              title: frozen.title,
              body: authoritativeBody,
              url: frozen.url,
              labels: frozen.labels,
              state: frozen.state,
              sourceUpdatedAt: frozen.sourceUpdatedAt,
              snapshotAt: frozen.capturedAt,
            }),
            epicIssue: { number: frozen.number, body: frozen.body },
            sourceDigest: frozen.digest,
          };
        })()
      : null;
    progressParentIssueNumber = sourceIssueAuthority === null
      ? null
      : linkedParentIssueNumber(
          sourceIssueAuthority.epicIssue.body,
          sourceIssueAuthority.epicIssue.number,
        );
    const scopedRunner = input.payload.event.kind === 'issue'
      ? scopedIssueRunner(input.fence, realIssueRunner, input.payload.event.number)
      : {
          listReadyIssues: () => [],
          claimIssue: () => {
            throw new RunnerExecutionError(
              'provider_failure',
              'pull-request reconciliation cannot claim an Issue',
              false,
              'provider',
            );
          },
        };
    const frozenIssueRunner = event.kind === 'issue' && sourceIssueAuthority
      ? {
          ...scopedRunner,
          listReadyIssues(requestedRepository: string, readyLabel: string) {
            if (requestedRepository !== repository) {
              throw new RunnerExecutionError(
                'provider_failure',
                `frozen Source Issue cannot be used for ${requestedRepository}`,
                false,
                'provider',
              );
            }
            return [GithubIssueSnapshot.parse({
              ...sourceIssueAuthority.issue,
              labels: [...new Set([
                ...sourceIssueAuthority.issue.labels,
                readyLabel,
              ])],
              state: 'open',
            })];
          },
          viewIssue(requestedRepository: string, issueNumber: number) {
            if (requestedRepository !== repository || issueNumber !== event.number) {
              throw new RunnerExecutionError(
                'provider_failure',
                'frozen Source Issue lookup escaped the runner job scope',
                false,
                'provider',
              );
            }
            return sourceIssueAuthority.issue;
          },
        }
      : scopedRunner;
    if (
      event.kind === 'issue'
      && recoveryPullRequest !== null
      && release?.status !== 'merged'
    ) {
      const ready = scopedRunner.listReadyIssues(
        repository,
        input.payload.execution.readyLabel,
      ).some((issue) => issue.number === event.number);
      if (ready) {
        await input.fence.arm('claim');
        scopedRunner.claimIssue(
          repository,
          event.number,
          input.payload.execution.readyLabel,
          input.payload.execution.claimedLabel,
        );
      }
    }
    const issueRunner = recoveryPullRequest === null
      ? frozenIssueRunner
      : {
          listReadyIssues: () => [],
          claimIssue: () => {
            throw new RunnerExecutionError(
              'provider_failure',
              'pull-request recovery cannot enter Issue planning',
              false,
              'provider',
            );
          },
        };
    const planningHumanReviewGithub = input.payload.event.kind === 'issue'
      && recoveryPullRequest === null
      ? guardedPlanningHumanReviewGithub(
          input.fence,
          (
            this.dependencies.planningHumanReviewGithub
            ?? realPlanningHumanReviewGitHub
          )(input.workspace.worktreePath),
          repository,
          input.payload.event.number,
        )
      : null;
    const gateRunner = guardedGateRunner(
      input.fence,
      (this.dependencies.gateRunner ?? realGhGateRunner)(repository),
    );
    const prNativeRunner = guardedPrNativeRunner(
      input.fence,
      (
        this.dependencies.prNativeRunner ?? realPrNativeGithubRunner
      )(input.payload.execution.mergeMethod, repository),
      input.payload.event.kind === 'pull_request'
        ? input.payload.event.number
        : recoveryPullRequest ?? undefined,
      repository,
    );
    const beforeProvider = (): Promise<void> => input.fence.arm('provider');
    const beforeMergeAndRelease = async (): Promise<void> => {
      await input.fence.arm('merge');
      await input.fence.arm('release');
      await input.fence.arm('release');
    };
    const producer = {
      jobId: input.lease.job.id,
      attemptId: input.lease.attemptId,
    };
    const releaseMergeOptions: AutoMergeOptions | null =
      release && input.controlStore && input.releaseRuntime
      ? {
          authorizeMerge: async ({ pr, revision, snapshot, github }) => {
            if (typeof input.controlStore!.reviewLineageGate === 'function') {
              const lineageGate = await input.controlStore!.reviewLineageGate(
                release.id,
              );
              if (!lineageGate.ready) {
                const reasons = lineageGate.pending.map((child) =>
                  `child issue #${child.issueNumber} is ${child.status}`);
                await reportProgress({
                  eventKey: `merge:child-gate:${revision.headSha}`,
                  phase: 'merge',
                  step: 'child integration gate',
                  state: 'waiting',
                  summary: reasons.join('; '),
                  nextGate: 'all child PRs integrated into the parent branch',
                  headSha: revision.headSha,
                  gateKey: 'merge',
                  branch: pr.branch,
                  pullRequestNumber: pr.externalRef?.number ?? null,
                });
                return { authorized: false, reasons };
              }
              const missingHeads = missingIntegratedHeads(
                input.workspace.worktreePath,
                revision.headSha,
                lineageGate.integratedHeads,
              );
              if (missingHeads.length > 0) {
                const reasons = missingHeads.map((head) =>
                  `cumulative parent head does not contain integrated child head ${head}`);
                await reportProgress({
                  eventKey: `merge:child-ancestry:${revision.headSha}`,
                  phase: 'merge',
                  step: 'cumulative child ancestry gate',
                  state: 'blocked',
                  blocker: reasons.join('; '),
                  nextGate: 'reconcile the parent integration branch at the expected SHA',
                  humanAction: 'repair the parent branch so every integrated child head is an ancestor',
                  headSha: revision.headSha,
                  gateKey: 'merge',
                  branch: pr.branch,
                  pullRequestNumber: pr.externalRef?.number ?? null,
                });
                return { authorized: false, reasons };
              }
            }
            await input.fence.arm('release');
            input.fence.consume('release');
            await projectReleasePreMerge({
              control: input.controlStore!,
              release,
              local: store,
              pr,
              pullRequestNumber: pr.externalRef!.number,
              observedPrHead: revision.headSha,
              githubChecks: github.checks,
              githubObservedAt: snapshot.createdAt,
              producer,
              runtime: input.releaseRuntime!,
            });
            await input.fence.arm('merge');
            return { authorized: true, reasons: [] };
          },
          completeMerge: async ({ pr, revision }) => {
            if (!pr.pr.externalRef || !prNativeRunner.observeRelease) {
              throw new RunnerExecutionError(
                'internal_failure',
                'release receipt mode requires GitHub release observation',
                false,
                'release',
              );
            }
            const observation = prNativeRunner.observeRelease(
              input.workspace.worktreePath,
              release.repository,
              release.issueNumber,
              pr.pr.externalRef.number,
              revision.headSha,
              baseBranch(input.payload.target.baseRef),
            );
            await input.fence.arm('release');
            input.fence.consume('release');
            await projectReleaseMerge(
              input.controlStore!,
              release,
              producer,
              observation,
            );
            if (
              input.payload.lineage
              && typeof input.controlStore!.markReviewChildIntegrated === 'function'
            ) {
              await input.controlStore!.markReviewChildIntegrated({
                token: input.lease.token,
                workerId: input.lease.workerId,
                releaseId: release.id,
                pullRequestNumber: pr.pr.externalRef.number,
                headSha: revision.headSha,
                integratedHeadSha: observation.mergeSha,
              });
            }
          },
          beforeRelease: async () => {
            await input.fence.arm('release');
            await input.fence.arm('release');
            input.fence.consume('release');
          },
        }
      : null;

    // A merged release may still owe its idempotent parent-epic transition.
    // Keep terminal recovery inside the adapter so a transient Issue inventory
    // or close failure is retried instead of being acknowledged before the
    // external state machine converges.
    if (
      release?.status === 'merged'
      && release.finalHead
      && release.pullRequest
      && (event.kind === 'pull_request' || recoveryPullRequest !== null)
    ) {
      if (sourceIssueAuthority) {
        await input.fence.arm('release');
        const epic = reconcileExternalEpicClosure(
          prNativeRunner,
          input.workspace.worktreePath,
          repository,
          sourceIssueAuthority.epicIssue,
        );
        if (epic.parentIssueNumber !== null) {
          await reportProgress({
            eventKey: `epic:${epic.parentIssueNumber}:${epic.closed ? 'closed' : 'waiting'}`,
            phase: epic.closed ? 'completed' : 'merge',
            step: epic.closed
              ? `parent Issue #${epic.parentIssueNumber} closed`
              : `parent Issue #${epic.parentIssueNumber} completion`,
            state: epic.closed ? 'succeeded' : 'waiting',
            summary: epic.closed
              ? `All required phases complete; parent #${epic.parentIssueNumber} closed`
              : `Parent #${epic.parentIssueNumber} remains open`,
            nextGate: epic.closed
              ? undefined
              : epic.pendingKeys.length > 0
                ? `close required phases: ${epic.pendingKeys.join(', ')}`
                : epic.reason ?? 'reconcile parent Issue structure',
            worktreePath: input.workspace.worktreePath,
            pullRequestNumber: release.pullRequest,
          });
        }
      } else {
        await reportProgress({
          eventKey: 'epic:legacy-snapshot-unavailable',
          phase: 'human-review',
          step: 'parent Issue auto-close skipped',
          state: 'blocked',
          summary: 'Release is merged; legacy release has no immutable Source Issue snapshot',
          blocker: 'Mutable current Issue text cannot authorize a parent Issue transition',
          nextGate: 'human manually reconciles the parent Issue if this phase belongs to an epic',
          humanAction: 'manually reconcile the parent Issue if this phase belongs to an epic',
          worktreePath: null,
          pullRequestNumber: release.pullRequest,
        });
      }
      await reportProgress({
        eventKey: 'completed:release',
        phase: 'completed',
        step: 'implementation released',
        state: 'succeeded',
        summary: sourceIssueAuthority
          ? `Release already merged at ${release.finalHead.slice(0, 12)}`
          : `Release already merged at ${release.finalHead.slice(0, 12)}; legacy epic reconciliation requires a human`,
        nextGate: sourceIssueAuthority
          ? null
          : 'human manually reconciles the parent Issue if this phase belongs to an epic',
        worktreePath: null,
        pullRequestNumber: release.pullRequest,
      });
      return {
        outcome: 'completed',
        humanReview: null,
        headSha: release.finalHead,
        pullRequestNumber: release.pullRequest,
        developmentTurn: { intake: [], enrichmentIds: [], driveResults: [] },
      };
    }

    let developmentTurn: GithubDevelopmentTurnResult;
    try {
      developmentTurn = await runGithubDevelopmentTurn(
        store,
        config,
        {
        issueRunner,
        ...(input.payload.event.kind === 'issue' && recoveryPullRequest === null
          ? { beforeIssueClaim: () => input.fence.arm('claim') }
          : {}),
        planningRunner: async ({ intake, route }) => {
          await beforeProvider();
          return (this.dependencies.planningRunner ?? runPlanningSession)(
            config,
            intake,
            route,
            input.workspace.harnessPath,
            input.log,
          );
        },
        uiDesignRunner: async ({ intake, candidate, route }) => {
          await beforeProvider();
          return (this.dependencies.uiDesignRunner ?? runUiDesignSession)(
            config,
            intake,
            candidate,
            route,
            input.workspace.harnessPath,
            input.log,
          );
        },
        prNativeRunner,
        repositoryGraderProfileEvidence: (worktree) =>
          repositoryGraderProfileEvidence(
            worktree,
            config.target?.graders ?? {},
          ),
        ...(sourceIssueAuthority && activePullRequest !== null
          ? {
              repositoryPullRequestIssueAuthority: {
                pullRequestNumber: activePullRequest,
                issue: sourceIssueAuthority.issue,
                sourceDigest: sourceIssueAuthority.sourceDigest,
              },
            }
          : {}),
        discoverPullRequests:
          input.payload.event.kind !== 'issue' || recoveryPullRequest !== null,
        // Issue jobs start from a fresh job-scoped store, so there is nothing
        // to reconcile before intake. PR/repository jobs arm permits only
        // immediately before their bounded reconciliation pass.
        beforeReconcile: releaseMergeOptions
          ? undefined
          : input.payload.event.kind === 'issue'
          ? async () => {
              if (store.db.prs.some((pr) =>
                pr.externalRef !== null
                && pr.status !== 'merged'
                && pr.status !== 'closed')) {
                await beforeMergeAndRelease();
              }
            }
          : beforeMergeAndRelease,
        reconcileOptions: releaseMergeOptions ?? {
          beforeRelease: () => input.fence.consume('release'),
        },
        progress: reportProgress,
        liveOptions: {
          gateRunner,
          prNativeRunner,
          releaseIdentity: release?.id ?? null,
          ...(input.controlStore
            && typeof input.controlStore.recordDevelopmentReviewRound === 'function'
            ? {
                reviewRoundRecorder: async (
                  review: Parameters<
                    PostgresControlStore['recordDevelopmentReviewRound']
                  >[0]['review'],
                ) => {
                  await input.controlStore!.recordDevelopmentReviewRound({
                    token: input.lease.token,
                    workerId: input.lease.workerId,
                    review,
                  });
                },
              }
            : {}),
          ...(release
            && input.controlStore
            && typeof input.controlStore.recordReviewChild === 'function'
            ? {
                separateFindingHandler: async ({
                  round,
                  headSha,
                  branch,
                  pullRequestNumber,
                  findings,
                }: Parameters<
                  NonNullable<LiveOptions['separateFindingHandler']>
                >[0]) => {
                  const github = (
                    this.dependencies.reviewChildGithub
                    ?? realReviewChildGithub
                  )(input.workspace.worktreePath);
                  for (const finding of findings) {
                    await input.fence.arm('release');
                    input.fence.consume('release');
                    const child = github.ensureChildIssue({
                      repository,
                      readyLabel: input.payload.execution.readyLabel,
                      parentReleaseId: release.id,
                      parentIssueNumber: release.issueNumber,
                      parentPullRequestNumber: pullRequestNumber,
                      parentBranch: branch,
                      parentHeadSha: headSha,
                      reviewRound: round,
                      perspective: finding.perspective,
                      findingIdentity: finding.identity,
                      finding: finding.finding,
                    });
                    await input.controlStore!.recordReviewChild({
                      token: input.lease.token,
                      workerId: input.lease.workerId,
                      childIssueNumber: child.number,
                      childIssueUrl: child.url,
                      findingKey: child.findingKey,
                      finding: finding.finding,
                      reviewRound: round,
                      parentPullRequestNumber: pullRequestNumber,
                      parentBranch: branch,
                      parentHeadSha: headSha,
                    });
                  }
                },
              }
            : {}),
          beforeProviderExecution: beforeProvider,
          beforePush: () => input.fence.arm('push'),
          beforeCreatePr: () => input.fence.arm('push'),
          ...(release && input.controlStore
            ? {
                afterProjectRevision: async ({ pr }: { pr: PR; headSha: string }) => {
                  if (!pr.externalRef) {
                    throw new RunnerExecutionError(
                      'internal_failure',
                      'release build projection has no stable GitHub PR identity',
                      false,
                      'release',
                    );
                  }
                  await input.fence.arm('release');
                  input.fence.consume('release');
                  await input.controlStore!.bindReleasePullRequest({
                    jobId: input.lease.job.id,
                    releaseId: release.id,
                    pullRequestNumber: pr.externalRef.number,
                  });
                },
              }
            : {}),
          ...(releaseMergeOptions
            ? {
                authorizeMerge: releaseMergeOptions.authorizeMerge,
                completeMerge: releaseMergeOptions.completeMerge,
                assertReleasePermit: releaseMergeOptions.beforeRelease,
              }
            : {
                beforeMerge: () => input.fence.arm('merge'),
                beforeRelease: async () => {
                  await input.fence.arm('release');
                  await input.fence.arm('release');
                },
                assertReleasePermit: () => input.fence.consume('release'),
              }),
          graderEnvironment: isolatedGraderEnvironment(
            process.env,
            input.workspace.registrationRoot,
          ),
          regressReport: this.dependencies.regressReport,
          generatorSession:
            this.dependencies.generatorSession ?? runGeneratorSession,
          perspectiveSessions:
            this.dependencies.perspectiveSessions ?? runPerspectiveSessions,
          trustedReviewStateRoot: input.workspace.harnessPath,
          operatorWorktreePath: input.workspace.worktreePath,
          projectOperatorWorktreeHead: (headSha: string) => {
            if (!input.projectWorkspaceHead) {
              throw new RunnerExecutionError(
                'workspace_failure',
                'runner cannot project the operator worktree to the pull request head',
                false,
                'provider',
              );
            }
            input.projectWorkspaceHead(headSha);
          },
          ...(sourceIssueAuthority
            ? {
                sourceIssueMaterial: sourceIssueReviewMaterial({
                  issue: sourceIssueAuthority.issue,
                  sourceDigest: sourceIssueAuthority.sourceDigest,
                }),
                sourceIssueReviewCriterion: {
                  url: sourceIssueAuthority.issue.url,
                  digest: sourceIssueAuthority.sourceDigest,
                  sourceUpdatedAt: sourceIssueAuthority.issue.sourceUpdatedAt,
                },
              }
            : {}),
          groundBuild: (options) => ({
            ...(this.dependencies.groundBuild ?? groundArtifact)(options),
            ...repositoryGraderProfileEvidence(
              options.worktree,
              options.target.graders ?? {},
            ),
          }),
          progress: reportProgress,
        },
      },
        input.workspace.harnessPath,
        input.log,
      );
    } catch (error) {
      if (error instanceof PlanningProviderInvocationError) {
        throw new RunnerExecutionError(
          'provider_failure',
          error.message,
          true,
          'provider',
          { cause: error },
        );
      }
      throw error;
    }

    const issueIntake = event.kind === 'issue'
      ? store.db.intakeRecords.find(
          (record) =>
            record.snapshot.repository === repository
            && record.snapshot.number === event.number,
        )
      : undefined;
    const matchingPr = event.kind === 'pull_request'
      ? [...store.db.prs].reverse().find(
          (pr) => pr.externalRef?.number === event.number,
        )
      : recoveryPullRequest !== null
        ? [...store.db.prs].reverse().find(
          (pr) => pr.externalRef?.number === recoveryPullRequest,
        )
      : event.kind === 'issue'
        ? (() => {
          if (!issueIntake) return undefined;
          return [...store.db.prs].reverse().find((pr) =>
            issueIntake.storeIssueIds.includes(pr.issueId));
        })()
        : [...store.db.prs].reverse().find((pr) => pr.status === 'merged');
    if (input.payload.event.kind === 'repository' && !matchingPr) {
      await reportProgress({
        eventKey: 'completed:repository-reconciliation',
        phase: 'completed',
        step: 'repository reconciliation completed',
        state: 'succeeded',
        summary: 'No pull request required additional work',
      });
      return {
        outcome: 'completed',
        humanReview: null,
        headSha: null,
        pullRequestNumber: null,
        developmentTurn,
      };
    }
    if (
      !matchingPr
      && event.kind === 'issue'
      && issueIntake?.status === 'needs-human-review'
    ) {
      const enrichment = store.planningEnrichmentFor(issueIntake.intakeKey);
      if (
        !enrichment
        || enrichment.status !== 'needs-human-review'
        || enrichment.reasons.length === 0
        || planningHumanReviewGithub === null
      ) {
        throw new RunnerExecutionError(
          'internal_failure',
          'needs-human-review intake has no recorded planning stop reasons',
          false,
          'release',
        );
      }
      const comment = renderPlanningHumanReviewComment({
        repository,
        issueNumber: event.number,
        reasons: enrichment.reasons,
        readyLabel: input.payload.execution.readyLabel,
      });
      await input.fence.arm('release');
      const commentUrl = planningHumanReviewGithub.ensureManagedComment(
        repository,
        event.number,
        comment,
      );
      await input.fence.arm('release');
      planningHumanReviewGithub.removeClaimedLabel(
        repository,
        event.number,
        input.payload.execution.claimedLabel,
      );
      await reportProgress({
        eventKey: 'human-review:planning',
        phase: 'human-review',
        step: 'planning clarification required',
        state: 'blocked',
        blocker: enrichment.reasons.join('; '),
        nextGate: 'human updates the Issue and reapplies the ready label',
        humanAction: 'update the Issue requirements and reapply the ready label',
        worktreePath: input.workspace.worktreePath,
      });
      return {
        outcome: 'needs-human-review',
        humanReview: {
          issueNumber: event.number,
          reasons: [...enrichment.reasons],
          commentUrl,
        },
        headSha: null,
        pullRequestNumber: null,
        developmentTurn,
      };
    }
    if (!matchingPr) {
      throw new RunnerExecutionError(
        'provider_failure',
        'existing AgentOps turn produced no PR for the runner job',
        true,
        'provider',
      );
    }
    // A current-head request-changes verdict is a successful reconciliation
    // that must wait for a new GitHub head. It is deliberately checked before
    // release projection: a rejected revision is not a provider build and must
    // never be forced through final-head release evidence validation.
    if (hasDurableCurrentHeadReviewStop(store, matchingPr)) {
      return {
        outcome: 'completed',
        humanReview: null,
        retainWorkspace: true,
        headSha: matchingPr.headSha,
        pullRequestNumber: matchingPr.externalRef?.number ?? null,
        developmentTurn,
      };
    }
    if (
      release
      && input.controlStore
      && input.releaseRuntime
      && matchingPr.status !== 'merged'
      && matchingPr.externalRef
      && matchingPr.headSha
    ) {
      await input.fence.arm('release');
      input.fence.consume('release');
      await projectReleaseProgress({
        control: input.controlStore,
        release,
        local: store,
        pr: matchingPr,
        pullRequestNumber: matchingPr.externalRef.number,
        observedPrHead: matchingPr.headSha,
        githubChecks: [],
        githubObservedAt: new Date().toISOString(),
        producer,
        runtime: input.releaseRuntime,
      });
    }
    if (matchingPr.status !== 'merged') {
      await reportProgress({
        eventKey: `merge:waiting:${matchingPr.externalRef?.number ?? 'unprojected'}`,
        phase: 'merge',
        step: 'GitHub merge gates',
        state: 'waiting',
        summary: `PR #${matchingPr.externalRef?.number ?? 'unprojected'} is ${matchingPr.status}`,
        nextGate: 'next pull request reconciliation',
        headSha: matchingPr.headSha,
        gateKey: 'merge',
        worktreePath: input.workspace.worktreePath,
        branch: matchingPr.branch,
        pullRequestNumber: matchingPr.externalRef?.number ?? null,
      });
      throw new RunnerExecutionError(
        'required_checks_failure',
        `PR #${matchingPr.externalRef?.number ?? 'unprojected'} is ${matchingPr.status}; retry reconciliation`,
        true,
        'merge',
      );
    }
    if (sourceIssueAuthority) {
      // Arm immediately before the only possible parent-Issue mutation. The
      // deterministic reconciler remains a no-op while any required phase is
      // open; parent closure is authorized only by the frozen Source Issue.
      await input.fence.arm('release');
      const epic = reconcileExternalEpicClosure(
        prNativeRunner,
        input.workspace.worktreePath,
        repository,
        sourceIssueAuthority.epicIssue,
      );
      if (epic.parentIssueNumber !== null) {
        await reportProgress({
          eventKey: `epic:${epic.parentIssueNumber}:${epic.closed ? 'closed' : 'waiting'}`,
          phase: epic.closed ? 'completed' : 'merge',
          step: epic.closed
            ? `parent Issue #${epic.parentIssueNumber} closed`
            : `parent Issue #${epic.parentIssueNumber} completion`,
          state: epic.closed ? 'succeeded' : 'waiting',
          summary: epic.closed
            ? `All required phases complete; parent #${epic.parentIssueNumber} closed`
            : `Parent #${epic.parentIssueNumber} remains open`,
          nextGate: epic.closed
            ? undefined
            : epic.pendingKeys.length > 0
              ? `close required phases: ${epic.pendingKeys.join(', ')}`
              : epic.reason ?? 'reconcile parent Issue structure',
          worktreePath: input.workspace.worktreePath,
          branch: matchingPr.branch,
          pullRequestNumber: matchingPr.externalRef?.number ?? null,
        });
      }
    }
    await reportProgress({
      eventKey: 'completed:release',
      phase: 'completed',
      step: 'implementation released',
      state: 'succeeded',
      summary: `PR #${matchingPr.externalRef?.number ?? 'unprojected'} merged`,
      worktreePath: null,
      branch: matchingPr.branch,
      pullRequestNumber: matchingPr.externalRef?.number ?? null,
    });
    return {
      outcome: 'completed',
      humanReview: null,
      headSha: matchingPr.mergedHeadSha,
      pullRequestNumber: matchingPr.externalRef?.number ?? null,
      developmentTurn,
    };
  }
}
