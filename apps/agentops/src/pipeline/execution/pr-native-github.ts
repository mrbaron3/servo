import { z } from 'zod';
import type { HarnessConfig } from '../../config.js';
import type { RevisionCheck, RevisionReviewThread } from '../../domain/schema.js';
import { runCommand as run } from './command.js';
import type {
  GithubOpenPullRequest,
  GithubReleaseObservation,
  PrNativeGithubRunner,
} from './pr-native.js';

export const MAX_REVIEW_THREAD_BODY_CHARS = 8_000;
export const BLOCKING_REVIEW_COMMENT = /\[(?:P0|P1)\]|\bblocker\b|\brequest_changes\b/i;
const GithubSha = z.string().regex(/^[0-9a-f]{40}$/i, 'expected a 40-character GitHub SHA');
const GithubCheck = z.object({
  name: z.string().min(1).optional(),
  context: z.string().min(1).optional(),
  conclusion: z.preprocess(
    (value) => value === '' ? null : value,
    z.enum(['SUCCESS', 'NEUTRAL', 'SKIPPED', 'FAILURE', 'ERROR', 'CANCELLED', 'TIMED_OUT', 'ACTION_REQUIRED', 'STALE', 'STARTUP_FAILURE']).nullable().optional(),
  ),
  state: z.enum(['EXPECTED', 'ERROR', 'FAILURE', 'PENDING', 'SUCCESS']).optional(),
  status: z.enum(['QUEUED', 'IN_PROGRESS', 'COMPLETED', 'PENDING', 'SUCCESS', 'FAILURE']).optional(),
}).refine((check) => Boolean(check.name ?? check.context), 'check requires name or context')
  .refine((check) => check.conclusion !== undefined || check.state !== undefined || check.status !== undefined, 'check requires status');

export const GhPrViewResponse = z.object({
  id: z.string().min(1),
  state: z.enum(['OPEN', 'MERGED', 'CLOSED']),
  isDraft: z.boolean(),
  headRefOid: GithubSha,
  mergeable: z.enum(['MERGEABLE', 'CONFLICTING', 'UNKNOWN']),
  reviewDecision: z.preprocess(
    (value) => value === '' ? null : value,
    z.enum(['APPROVED', 'CHANGES_REQUESTED', 'REVIEW_REQUIRED']).nullable().optional(),
  ),
  statusCheckRollup: z.array(GithubCheck).optional(),
});

export const GhPrListResponse = z.array(z.object({
  number: z.number().int().positive(),
  url: z.string().url(),
  title: z.string(),
  body: z.string(),
  headRefName: z.string().min(1),
  headRefOid: GithubSha,
  baseRefName: z.string().min(1),
  isDraft: z.boolean(),
  isCrossRepository: z.boolean(),
}));

const GithubApiRepository = z.object({
  full_name: z.string().min(1),
});
const GithubApiPullRequest = z.object({
  number: z.number().int().positive(),
  html_url: z.string().url(),
  title: z.string(),
  body: z.string().nullable(),
  draft: z.boolean(),
  head: z.object({
    ref: z.string().min(1),
    sha: GithubSha,
    repo: GithubApiRepository,
  }),
  base: z.object({
    ref: z.string().min(1),
    repo: GithubApiRepository,
  }),
});
export const GhPrApiPagesResponse = z.array(z.array(GithubApiPullRequest));
export type GithubCommandRunner = (
  command: string,
  args: string[],
  cwd: string,
) => string;

const GhReleasePrViewResponse = z.object({
  state: z.literal('MERGED'),
  headRefOid: GithubSha,
  mergeCommit: z.object({ oid: GithubSha }).strict(),
  mergedBy: z.object({ login: z.string().min(1).max(128) }).strict(),
  mergedAt: z.string().datetime({ offset: true }),
}).strict();
const GhReleaseIssueViewResponse = z.object({
  state: z.literal('CLOSED'),
  stateReason: z.literal('COMPLETED'),
}).strict();
const GhRepositoryViewResponse = z.object({
  defaultBranchRef: z.object({ name: z.string().min(1).max(255) }).strict(),
}).strict();
const GhCompareResponse = z.object({
  status: z.enum(['ahead', 'identical', 'behind', 'diverged']),
}).passthrough();

/** Capture the external release boundary from independently queried GitHub facts. */
export function observeGithubRelease(
  commandRunner: GithubCommandRunner,
  cwd: string,
  repository: string,
  issueNumber: number,
  prNumber: number,
  expectedHead: string,
  integrationBranch?: string,
): GithubReleaseObservation {
  const parsedHead = GithubSha.parse(expectedHead);
  const pr = GhReleasePrViewResponse.parse(JSON.parse(commandRunner('gh', [
    'pr', 'view', String(prNumber), '--repo', repository, '--json',
    'state,headRefOid,mergeCommit,mergedBy,mergedAt',
  ], cwd)));
  if (pr.headRefOid !== parsedHead) {
    throw new Error(
      `merged PR #${prNumber} head ${pr.headRefOid} does not match ${parsedHead}`,
    );
  }
  GhReleaseIssueViewResponse.parse(JSON.parse(commandRunner('gh', [
    'issue', 'view', String(issueNumber), '--repo', repository, '--json',
    'state,stateReason',
  ], cwd)));
  const releaseBranch = integrationBranch ?? GhRepositoryViewResponse.parse(
    JSON.parse(commandRunner('gh', [
      'repo', 'view', repository, '--json', 'defaultBranchRef',
    ], cwd)),
  ).defaultBranchRef.name;
  const comparison = GhCompareResponse.parse(JSON.parse(commandRunner('gh', [
    'api', `repos/${repository}/compare/${pr.mergeCommit.oid}...${releaseBranch}`,
  ], cwd)));
  if (comparison.status !== 'ahead' && comparison.status !== 'identical') {
    throw new Error(
      `merge commit ${pr.mergeCommit.oid} is not reachable from `
      + `${releaseBranch}`,
    );
  }
  return {
    pullRequestNumber: prNumber,
    expectedHead: parsedHead,
    observedPrHead: pr.headRefOid,
    mergeSha: pr.mergeCommit.oid,
    actor: pr.mergedBy.login,
    sourceIssueClosure: 'completed',
    mergeReachableFromDefaultBranch: true,
    mergedAt: pr.mergedAt,
  };
}

/** Retrieve every open PR page; `gh pr list --limit` silently truncates large repositories. */
export function listOpenGithubPullRequests(
  commandRunner: GithubCommandRunner,
  cwd: string,
  baseBranch: string,
  repository?: string,
): GithubOpenPullRequest[] {
  const pages = GhPrApiPagesResponse.parse(JSON.parse(commandRunner('gh', [
    'api',
    '--method', 'GET',
    '--paginate',
    '--slurp',
    '-f', 'state=open',
    '-f', `base=${baseBranch}`,
    '-f', 'per_page=100',
    repository ? `repos/${repository}/pulls` : 'repos/{owner}/{repo}/pulls',
  ], cwd)));
  return pages.flatMap((page) => page.map((row) => ({
    number: row.number,
    url: row.html_url,
    title: row.title,
    body: row.body ?? '',
    headRefName: row.head.ref,
    headSha: row.head.sha,
    baseRefName: row.base.ref,
    isDraft: row.draft,
    isCrossRepository: row.head.repo.full_name !== row.base.repo.full_name,
  })));
}

export const ReviewThreadsResponse = z.object({
  data: z.object({ node: z.object({ reviewThreads: z.object({
    pageInfo: z.object({ hasNextPage: z.boolean() }),
    nodes: z.array(z.object({
      id: z.string().min(1),
      isResolved: z.boolean(),
      path: z.string().nullable(),
      line: z.number().int().positive().nullable(),
      comments: z.object({
        pageInfo: z.object({ hasNextPage: z.boolean() }),
        nodes: z.array(z.object({ body: z.string() })),
      }),
    })),
  }) }) }),
});
type ReviewThreadsResponse = z.infer<typeof ReviewThreadsResponse>;

export function githubCheckStatus(rawInput: unknown): RevisionCheck['status'] {
  const raw = GithubCheck.parse(rawInput);
  const value = (raw.conclusion ?? raw.state ?? raw.status ?? '').toUpperCase();
  if (value === 'SUCCESS') return 'success';
  if ([
    'NEUTRAL',
    'SKIPPED',
    'FAILURE',
    'ERROR',
    'CANCELLED',
    'TIMED_OUT',
    'ACTION_REQUIRED',
  ].includes(value)) {
    return 'failure';
  }
  return 'pending';
}

export function parseBlockingReviewThreads(
  parsed: ReviewThreadsResponse,
): RevisionReviewThread[] {
  const threads = parsed.data.node.reviewThreads;
  const blocking = threads.nodes
    .filter((thread) => !thread.isResolved)
    .filter((thread) => thread.comments.nodes.some(
      (comment) => BLOCKING_REVIEW_COMMENT.test(comment.body),
    ) || thread.comments.pageInfo.hasNextPage)
    .map((thread): RevisionReviewThread => {
      const body = thread.comments.nodes
        .map((comment) => comment.body)
        .filter((comment) => BLOCKING_REVIEW_COMMENT.test(comment))
        .join('\n\n')
        .trim()
        || 'Blocking thread comments exceeded the inspected page; inspect the thread before merge.';
      return {
        id: thread.id,
        body: body.slice(0, MAX_REVIEW_THREAD_BODY_CHARS),
        path: thread.path,
        line: thread.line,
      };
    });
  if (threads.pageInfo.hasNextPage) {
    blocking.push({
      id: 'review-threads:pagination-incomplete',
      body: 'More than 100 review threads exist; inspect the remaining page before merge.',
      path: null,
      line: null,
    });
  }
  return blocking;
}

function blockingReviewThreads(cwd: string, nodeId: string): RevisionReviewThread[] {
  const query = `query($id: ID!) {
    node(id: $id) {
      ... on PullRequest {
        reviewThreads(first: 100) {
          pageInfo { hasNextPage }
          nodes {
            id isResolved path line
            comments(first: 100) { pageInfo { hasNextPage } nodes { body } }
          }
        }
      }
    }
  }`;
  const output = run('gh', ['api', 'graphql', '-f', `query=${query}`, '-F', `id=${nodeId}`], cwd);
  return parseBlockingReviewThreads(ReviewThreadsResponse.parse(JSON.parse(output)));
}

/** Concrete GitHub CLI transport; domain gate policy remains in pr-native.ts. */
export function realPrNativeGithubRunner(
  mergeMethod: NonNullable<HarnessConfig['gate']>['mergeMethod'] = 'squash',
  repository?: string,
): PrNativeGithubRunner {
  const repoArgs = repository ? ['--repo', repository] : [];
  const remote = repository
    ? `https://github.com/${repository}.git`
    : 'origin';
  return {
    viewRevision(cwd, prNumber) {
      const raw = GhPrViewResponse.parse(JSON.parse(run('gh', [
        'pr', 'view', String(prNumber), ...repoArgs, '--json',
        'id,state,isDraft,headRefOid,mergeable,reviewDecision,statusCheckRollup',
      ], cwd)));
      const checks = (raw.statusCheckRollup ?? []).map((check) => ({
        name: check.name ?? check.context!,
        status: githubCheckStatus(check),
      }));
      const blockingThreads = blockingReviewThreads(cwd, raw.id);
      if (raw.reviewDecision === 'CHANGES_REQUESTED') {
        blockingThreads.push({
          id: 'review-decision:changes-requested',
          body: 'GitHub reviewDecision is CHANGES_REQUESTED.',
          path: null,
          line: null,
        });
      }
      return {
        state: raw.state === 'MERGED' ? 'merged' : raw.state === 'CLOSED' ? 'closed' : 'open',
        headSha: raw.headRefOid,
        isDraft: raw.isDraft,
        mergeability: raw.mergeable === 'MERGEABLE'
          ? 'mergeable'
          : raw.mergeable === 'CONFLICTING' ? 'conflicting' : 'unknown',
        checks,
        unresolvedBlockingThreadIds: [...new Set(blockingThreads.map((thread) => thread.id))],
        blockingReviewThreads: blockingThreads,
      };
    },
    listOpenPullRequests(cwd, baseBranch) {
      return listOpenGithubPullRequests(run, cwd, baseBranch, repository);
    },
    fetchPullRequestHead(cwd, prNumber, expectedHeadSha, headRefName, baseRefName) {
      const localRef = `refs/agentops/pull/${prNumber}`;
      const remoteBaseRef = `refs/remotes/origin/${baseRefName}`;
      run('git', [
        'fetch', '--no-tags', remote,
        `+refs/pull/${prNumber}/head:${localRef}`,
        `+refs/heads/${headRefName}:refs/remotes/origin/${headRefName}`,
        ...(baseRefName !== headRefName
          ? [`+refs/heads/${baseRefName}:refs/remotes/origin/${baseRefName}`]
          : []),
      ], cwd, { credentials: 'github' });
      const fetchedHeadSha = GithubSha.parse(
        run('git', ['rev-parse', '--verify', `${localRef}^{commit}`], cwd).trim(),
      );
      const fetchedBaseSha = GithubSha.parse(
        run('git', ['rev-parse', '--verify', `${remoteBaseRef}^{commit}`], cwd).trim(),
      );
      if (fetchedHeadSha !== expectedHeadSha) {
        throw new Error(
          `PR #${prNumber} head changed while fetching: expected ${expectedHeadSha}, got ${fetchedHeadSha}`,
        );
      }
      return { headSha: fetchedHeadSha, baseSha: fetchedBaseSha };
    },
    pullRequestChangedFiles: (cwd, prNumber) => run(
      'gh', [
        'pr', 'diff', String(prNumber), ...repoArgs, '--name-only',
      ], cwd,
    ).split('\n').map((line) => line.trim()).filter(Boolean),
    observeRelease(
      cwd,
      targetRepository,
      issueNumber,
      prNumber,
      expectedHead,
      integrationBranch,
    ) {
      if (repository && repository !== targetRepository) {
        throw new Error(
          `release repository ${targetRepository} does not match scoped ${repository}`,
        );
      }
      return observeGithubRelease(
        run,
        cwd,
        targetRepository,
        issueNumber,
        prNumber,
        expectedHead,
        integrationBranch,
      );
    },
    merge(cwd, prNumber, expectedHeadSha) {
      run('gh', [
        'pr', 'merge', String(prNumber), ...repoArgs, `--${mergeMethod}`,
        '--match-head-commit', expectedHeadSha, '--delete-branch',
      ], cwd);
    },
    closeIssue(cwd, repository, issueNumber) {
      run('gh', [
        'issue', 'close', String(issueNumber), '--repo', repository,
        '--reason', 'completed', '--comment',
        'すべての必須work unitが自動レビュー・merge済みのためcloseします。',
      ], cwd);
    },
    listRepositoryIssues(cwd, targetRepository) {
      if (repository && repository !== targetRepository) {
        throw new Error(
          `Issue inventory repository ${targetRepository} does not match scoped ${repository}`,
        );
      }
      const raw = JSON.parse(run('gh', [
        'issue', 'list', '--repo', targetRepository, '--state', 'all',
        '--limit', '1000', '--json',
        'number,title,body,author,subIssues,state,stateReason',
      ], cwd)) as Array<{
        number: number;
        title: string;
        body: string | null;
        author: { login: string } | null;
        subIssues: { nodes: Array<{ number: number }> };
        state: string;
        stateReason: string | null;
      }>;
      return raw.map((issue) => ({
        number: issue.number,
        title: issue.title,
        body: issue.body ?? '',
        authorLogin: issue.author?.login ?? '',
        subIssueNumbers: issue.subIssues.nodes.map((child) => child.number),
        state: issue.state.toLowerCase() === 'closed' ? 'closed' : 'open',
        stateReason: issue.stateReason?.toLowerCase() === 'completed'
          ? 'completed'
          : issue.stateReason?.toLowerCase() === 'not_planned'
            ? 'not_planned'
            : null,
      }));
    },
  };
}
