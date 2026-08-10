import { createHash } from 'node:crypto';
import { z } from 'zod';
import type { PostgresControlStore } from '../control-store/store.js';
import { CanonicalRepository, type ReleaseRecord } from '../control-store/types.js';
import type {
  AgentInvocation,
  Finding,
  PR,
  RevisionCheck,
} from '../domain/schema.js';
import { HardGateSignalName } from '../domain/schema.js';
import { findingOriginRef } from '../domain/finding-lineage.js';
import type { GithubReleaseObservation } from '../pipeline/execution/pr-native.js';
import type { Store } from '../store/store.js';
import {
  type DurableReleaseReceipt,
  type ReleaseBuildReceipt,
  type ReleaseReviewReceipt,
} from './release-receipt.js';

const Digest = z.string().regex(/^sha256:[0-9a-f]{64}$/);
const Head = z.string().regex(/^[0-9a-f]{40}$/);
const ProviderDefault = z.object({
  provider: z.enum(['codex', 'claude', 'gemini', 'mock']),
  reference: z.string().min(1).max(256),
  resolverDigest: Digest,
}).strict();

export const ReleaseRuntimeConfigurationContract = z.object({
  consumer: z.object({
    repository: CanonicalRepository,
    revision: Head,
  }).strict(),
  environment: z.object({
    kind: z.enum(['container', 'host', 'managed-runner']),
    reference: z.string().min(1).max(512),
    digest: Digest,
  }).strict(),
  providerDefaults: z.array(ProviderDefault).max(4),
}).strict().superRefine((value, context) => {
  const providers = value.providerDefaults.map((entry) => entry.provider);
  if (new Set(providers).size !== providers.length) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['providerDefaults'],
      message: 'provider default provenance must be unique by provider',
    });
  }
});
export type ReleaseRuntimeConfiguration = z.infer<
  typeof ReleaseRuntimeConfigurationContract
>;

export function releaseRuntimeConfigurationFromEnvironment(
  environment: NodeJS.ProcessEnv,
): ReleaseRuntimeConfiguration | null {
  const keys = [
    'AGENTOPS_RELEASE_CONSUMER_REPOSITORY',
    'AGENTOPS_RELEASE_CONSUMER_REVISION',
    'AGENTOPS_RELEASE_ENVIRONMENT_KIND',
    'AGENTOPS_RELEASE_ENVIRONMENT_REFERENCE',
    'AGENTOPS_RELEASE_ENVIRONMENT_DIGEST',
    'AGENTOPS_RELEASE_PROVIDER_DEFAULTS_JSON',
  ] as const;
  if (keys.every((key) => environment[key] === undefined)) return null;
  let providerDefaults: unknown;
  try {
    providerDefaults = JSON.parse(
      environment.AGENTOPS_RELEASE_PROVIDER_DEFAULTS_JSON ?? '',
    );
  } catch (error) {
    throw new Error(
      'AGENTOPS_RELEASE_PROVIDER_DEFAULTS_JSON must be valid JSON',
      { cause: error },
    );
  }
  return ReleaseRuntimeConfigurationContract.parse({
    consumer: {
      repository: environment.AGENTOPS_RELEASE_CONSUMER_REPOSITORY,
      revision: environment.AGENTOPS_RELEASE_CONSUMER_REVISION,
    },
    environment: {
      kind: environment.AGENTOPS_RELEASE_ENVIRONMENT_KIND,
      reference: environment.AGENTOPS_RELEASE_ENVIRONMENT_REFERENCE,
      digest: environment.AGENTOPS_RELEASE_ENVIRONMENT_DIGEST,
    },
    providerDefaults,
  });
}

function digest(value: unknown): `sha256:${string}` {
  return `sha256:${createHash('sha256').update(JSON.stringify(value)).digest('hex')}`;
}

function deterministicUuid(releaseId: string, key: string): string {
  const bytes = Buffer.from(
    createHash('sha256').update(`${releaseId}\0${key}`).digest('hex').slice(0, 32),
    'hex',
  );
  bytes[6] = (bytes[6]! & 0x0f) | 0x40;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = bytes.toString('hex');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-`
    + `${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function after(...timestamps: string[]): string {
  return new Date(Math.max(...timestamps.map((value) => Date.parse(value))) + 1)
    .toISOString();
}

function runtimeRole(
  invocation: AgentInvocation,
): 'planning' | 'ui-design' | 'generator' | 'repair' | 'reviewer' | null {
  if (invocation.role === 'issue-planner') return 'planning';
  if (invocation.role === 'ui-designer') return 'ui-design';
  if (invocation.role === 'generator') {
    return invocation.attempt > 1 ? 'repair' : 'generator';
  }
  if (invocation.role === 'reviewer') return 'reviewer';
  return null;
}

function findingId(perspective: string, lineageRef: string): string {
  return `finding:${createHash('sha256')
    .update(JSON.stringify({ perspective, lineageRef }))
    .digest('hex')}`;
}

export function projectReleaseFinding(input: {
  perspective: string;
  finding: Finding;
  findingIndex: number;
  run: {
    id: string;
    prId: string;
    headSha: string | null;
    attempt: number;
  };
  headEpoch: number;
  findingEpochs: Map<string, number[]>;
}): { findingId: string; lineage: 'persisted' | 'new' } {
  const { finding } = input;
  if (finding.lineage === 'persisted' && !finding.lineageRef) {
    throw new Error('persisted finding has no stable prior finding reference');
  }
  if (finding.lineage !== 'persisted' && finding.lineageRef) {
    throw new Error('new finding must not reuse a prior finding reference');
  }
  const lineageRef = finding.lineageRef ?? findingOriginRef({
    runId: input.run.id,
    prId: input.run.prId,
    headSha: input.run.headSha,
    attempt: input.run.attempt,
    perspective: input.perspective,
  }, input.findingIndex);
  const id = findingId(input.perspective, lineageRef);
  const epochs = input.findingEpochs.get(id) ?? [];
  const persisted = epochs.some((candidate) => candidate < input.headEpoch);
  if (persisted && finding.lineage !== 'persisted') {
    throw new Error(`finding ${id} lacks persisted lineage on a later head`);
  }
  if (!persisted && finding.lineage === 'persisted') {
    throw new Error(`finding ${id} claims persisted lineage without an earlier receipt`);
  }
  input.findingEpochs.set(id, [...epochs, input.headEpoch]);
  return { findingId: id, lineage: persisted ? 'persisted' : 'new' };
}

type ReleaseReceiptControl = Pick<PostgresControlStore,
  | 'listReleaseReceipts'
  | 'recordReleaseReceipt'
  | 'observeReleaseHead'
  | 'bindReleasePullRequest'
  | 'authorizeReleaseMerge'
  | 'completeReleaseMerge'
>;

export interface ProjectReleasePreMergeInput {
  control: ReleaseReceiptControl;
  release: ReleaseRecord;
  local: Store;
  pr: PR;
  pullRequestNumber: number;
  observedPrHead: string;
  githubChecks: readonly RevisionCheck[];
  githubObservedAt: string;
  producer: { jobId: string; attemptId: string };
  runtime: ReleaseRuntimeConfiguration;
}

/** Project independently produced facts without requiring this job to reach merge. */
async function projectReleaseEvidence(
  input: ProjectReleasePreMergeInput,
  authorizeMerge: boolean,
): Promise<void> {
  const runtime = ReleaseRuntimeConfigurationContract.parse(input.runtime);
  const head = Head.parse(input.observedPrHead.toLowerCase());
  if (input.pr.headSha?.toLowerCase() !== head
    || input.pr.externalRef?.number !== input.pullRequestNumber) {
    throw new Error('release projection PR/head does not match the observed GitHub revision');
  }
  await input.control.bindReleasePullRequest({
    jobId: input.producer.jobId,
    releaseId: input.release.id,
    pullRequestNumber: input.pullRequestNumber,
  });
  if (input.release.status === 'merge-authorized') {
    if (!authorizeMerge) return;
    const intent = (await input.control.listReleaseReceipts(input.release.id, {
      includeMergeIntent: true,
    })).map((entry) => entry.receipt).find(
      (receipt) => receipt.kind === 'merge-intent',
    );
    if (!intent
      || intent.pullRequestNumber !== input.pullRequestNumber
      || intent.expectedHead !== head
      || intent.observedPrHead !== head) {
      throw new Error('authorized release has no matching durable merge intent');
    }
    return;
  }
  if (input.release.status !== 'collecting') {
    throw new Error(`release ${input.release.id} is already merged`);
  }
  const existing = (await input.control.listReleaseReceipts(input.release.id))
    .map((entry) => entry.receipt);
  const authority = existing.find((receipt) => receipt.kind === 'authority');
  if (!authority) throw new Error('release projection requires a durable authority receipt');
  const receiptKeys = new Set(existing.map((receipt) => receipt.receiptKey));

  const invocations = input.local.db.agentInvocations
    .filter((invocation) => invocation.outcome === 'completed'
      && (invocation.prId === input.pr.id || invocation.issueId === input.pr.issueId))
    .map((source) => ({ source, role: runtimeRole(source) }))
    .filter((entry): entry is typeof entry & {
      role: Exclude<ReturnType<typeof runtimeRole>, null>;
    } => entry.role !== null)
    .sort((left, right) => left.source.createdAt.localeCompare(right.source.createdAt));
  const invocationRef = (key: string): string => (
    `invocation:${deterministicUuid(input.release.id, key)}`
  );
  const priorInvocationRefs = new Set(existing
    .filter((receipt) => receipt.kind === 'runtime-provenance')
    .flatMap((receipt) => receipt.invocations.map((invocation) => invocation.invocationRef)));
  const newInvocations = invocations.filter(
    ({ source }) => !priorInvocationRefs.has(invocationRef(source.invocationKey)),
  );
  if (newInvocations.length > 0) {
    const key = `runtime:${input.producer.jobId}:${input.producer.attemptId}`;
    const defaults = new Map(runtime.providerDefaults.map((entry) => [entry.provider, entry]));
    await input.control.recordReleaseReceipt({
      receiptId: deterministicUuid(input.release.id, key),
      receiptKey: key,
      releaseId: input.release.id,
      repository: input.release.repository,
      issueNumber: input.release.issueNumber,
      producer: input.producer,
      causes: [authority.receiptId],
      recordedAt: after(authority.recordedAt, ...newInvocations.map(
        ({ source }) => source.createdAt,
      )),
      kind: 'runtime-provenance',
      consumer: runtime.consumer,
      environment: runtime.environment,
      invocations: newInvocations.map(({ source, role }) => {
        const providerDefault = defaults.get(source.provider);
        if (source.model === null && !providerDefault) {
          throw new Error(`no default-model resolver evidence for provider ${source.provider}`);
        }
        return {
          invocationKey: source.invocationKey,
          invocationRef: invocationRef(source.invocationKey),
          role,
          provider: source.provider,
          model: source.model === null
            ? {
                kind: 'provider-default' as const,
                reference: providerDefault!.reference,
                resolverDigest: providerDefault!.resolverDigest,
              }
            : { kind: 'explicit' as const, name: source.model },
          ...(source.headSha ? { head: source.headSha.toLowerCase() } : {}),
        };
      }),
    });
  }
  if (invocations.length === 0
    && !existing.some((receipt) => receipt.kind === 'runtime-provenance')) {
    throw new Error('release projection found no completed provider invocations');
  }

  const revisions = input.local.db.prRevisions
    .filter((revision) => revision.prId === input.pr.id)
    .sort((left, right) => left.ordinal - right.ordinal);
  const buildByHead = new Map(existing
    .filter((receipt): receipt is ReleaseBuildReceipt => receipt.kind === 'build')
    .map((receipt) => [receipt.head, receipt]));
  for (const revision of revisions) {
    const buildHead = revision.headSha.toLowerCase();
    if (buildByHead.has(buildHead)) continue;
    const generated = invocations.find(({ source, role }) => (
      (role === 'generator' || role === 'repair')
      && source.headSha?.toLowerCase() === buildHead
    ));
    if (!generated) continue;
    const parentRevision = revisions.find(
      (candidate) => candidate.ordinal === revision.ordinal - 1,
    );
    const parentHead = parentRevision?.headSha.toLowerCase() ?? null;
    const parentBuild = parentHead === null ? null : buildByHead.get(parentHead);
    if (parentHead !== null && !parentBuild) {
      throw new Error(`release build ${buildHead} has no durable parent build ${parentHead}`);
    }
    const key = `build:${buildHead}`;
    const receipt: ReleaseBuildReceipt = {
      receiptId: deterministicUuid(input.release.id, key),
      receiptKey: key,
      releaseId: input.release.id,
      repository: input.release.repository,
      issueNumber: input.release.issueNumber,
      producer: input.producer,
      causes: [authority.receiptId, ...(parentBuild ? [parentBuild.receiptId] : [])],
      recordedAt: after(
        authority.recordedAt,
        generated.source.createdAt,
        ...(parentBuild ? [parentBuild.recordedAt] : []),
      ),
      kind: 'build',
      head: buildHead,
      parentHead,
      invocationRef: invocationRef(generated.source.invocationKey),
      role: generated.role === 'generator' ? 'generator' : 'repair',
    };
    await input.control.recordReleaseReceipt(receipt);
    receiptKeys.add(key);
    buildByHead.set(buildHead, receipt);
  }
  const finalBuild = buildByHead.get(head);
  if (!finalBuild) throw new Error(`release final head ${head} has no provider build`);

  const priorReviews = existing.filter(
    (receipt): receipt is ReleaseReviewReceipt => receipt.kind === 'review',
  );
  const findingEpochs = new Map<string, number[]>();
  for (const review of priorReviews) {
    for (const finding of review.findings) {
      findingEpochs.set(finding.findingId, [
        ...(findingEpochs.get(finding.findingId) ?? []),
        review.headEpoch,
      ]);
    }
  }
  const revisionOrdinal = new Map(revisions.map((revision) => [
    revision.headSha.toLowerCase(), revision.ordinal,
  ]));
  const runs = input.local.db.evalRuns
    .filter((run) => run.prId === input.pr.id && run.revisionId !== null
      && run.headSha !== null && run.perspective !== null && run.invocationKey !== null)
    .sort((left, right) => (
      (revisionOrdinal.get(left.headSha!.toLowerCase()) ?? 0)
      - (revisionOrdinal.get(right.headSha!.toLowerCase()) ?? 0)
      || left.createdAt.localeCompare(right.createdAt)
    ));
  for (const run of runs) {
    const reviewHead = run.headSha!.toLowerCase();
    const build = buildByHead.get(reviewHead);
    const reviewerInvocation = invocations.find(
      ({ source, role }) => role === 'reviewer' && source.invocationKey === run.invocationKey,
    );
    if (!build || !reviewerInvocation) continue;
    const key = `review:${reviewHead}:${run.perspective}`;
    if (receiptKeys.has(key)) continue;
    const epoch = await input.control.observeReleaseHead({
      releaseId: input.release.id,
      head: reviewHead,
      parentHead: build.parentHead,
    });
    const findings = run.findings.map((finding, findingIndex) =>
      projectReleaseFinding({
        perspective: run.perspective!,
        finding,
        findingIndex,
        run,
        headEpoch: epoch,
        findingEpochs,
      }));
    if (run.verdict === 'approve' && findings.length !== 0) {
      throw new Error(`review ${run.id} approve verdict cannot contain findings`);
    }
    await input.control.recordReleaseReceipt({
      receiptId: deterministicUuid(input.release.id, key),
      receiptKey: key,
      releaseId: input.release.id,
      repository: input.release.repository,
      issueNumber: input.release.issueNumber,
      producer: input.producer,
      causes: [build.receiptId],
      recordedAt: after(build.recordedAt, run.createdAt),
      kind: 'review',
      head: reviewHead,
      headEpoch: epoch,
      perspective: run.perspective!,
      invocationRef: invocationRef(reviewerInvocation.source.invocationKey),
      verdict: run.verdict,
      hasFindings: findings.length > 0,
      findings,
    });
    receiptKeys.add(key);
  }

  const finalRuns = runs.filter((run) => run.headSha?.toLowerCase() === head);
  const approvedSnapshot = [...input.local.db.revisionGateSnapshots].reverse().find(
    (snapshot) => snapshot.prId === input.pr.id
      && snapshot.headSha.toLowerCase() === head && snapshot.decision === 'approved',
  );
  for (const signal of input.release.policy.requiredGateSignals) {
    const repositoryGate = HardGateSignalName.safeParse(signal.name);
    const facts = signal.source === 'github-check'
      ? input.githubChecks.find(
          (check) => check.name === signal.name && check.status === 'success',
        )
      : repositoryGate.success
        ? finalRuns.map((run) => ({
            run,
            status: run.hardGates[repositoryGate.data],
          })).find(({ status }) => status === 'pass')
        : undefined;
    if (!facts) continue;
    const key = `grade:${signal.source}:${signal.name}:${head}`;
    if (receiptKeys.has(key)) continue;
    await input.control.recordReleaseReceipt({
      receiptId: deterministicUuid(input.release.id, key),
      receiptKey: key,
      releaseId: input.release.id,
      repository: input.release.repository,
      issueNumber: input.release.issueNumber,
      producer: input.producer,
      causes: [finalBuild.receiptId],
      recordedAt: after(
        finalBuild.recordedAt,
        signal.source === 'github-check'
          ? input.githubObservedAt
          : approvedSnapshot?.createdAt ?? finalBuild.recordedAt,
      ),
      kind: 'grade',
      head,
      signal,
      status: 'passed',
      detailsDigest: digest(facts),
    });
    receiptKeys.add(key);
  }

  for (const intervention of input.local.db.interventions.filter(
    (item) => item.issueId === input.pr.issueId,
  )) {
    const key = `intervention:${intervention.id}`;
    if (receiptKeys.has(key)) continue;
    await input.control.recordReleaseReceipt({
      receiptId: deterministicUuid(input.release.id, key),
      receiptKey: key,
      releaseId: input.release.id,
      repository: input.release.repository,
      issueNumber: input.release.issueNumber,
      producer: input.producer,
      causes: [authority.receiptId],
      recordedAt: after(authority.recordedAt, intervention.createdAt),
      kind: 'intervention',
      interventionKind: intervention.kind,
      reason: intervention.reason,
    });
    receiptKeys.add(key);
  }

  const projected = (await input.control.listReleaseReceipts(input.release.id))
    .map((entry) => entry.receipt);
  const reviews = projected.filter(
    (receipt): receipt is ReleaseReviewReceipt => receipt.kind === 'review',
  ).sort((left, right) => left.headEpoch - right.headEpoch
    || left.recordedAt.localeCompare(right.recordedAt));
  const builds = new Map(projected
    .filter((receipt): receipt is ReleaseBuildReceipt => receipt.kind === 'build')
    .map((receipt) => [receipt.head, receipt]));
  const resolved = new Set(projected
    .filter((receipt) => receipt.kind === 'finding-resolution')
    .map((receipt) => receipt.findingId));
  const raised = new Map<string, ReleaseReviewReceipt>();
  const latest = new Map<string, ReleaseReviewReceipt>();
  for (const review of reviews) {
    for (const finding of review.findings) {
      if (finding.lineage === 'new') raised.set(finding.findingId, review);
      latest.set(finding.findingId, review);
    }
  }
  for (const [id, origin] of raised) {
    if (resolved.has(id)) continue;
    const last = latest.get(id)!;
    const approval = reviews.find((review) => review.perspective === origin.perspective
      && review.headEpoch > last.headEpoch && review.verdict === 'approve');
    const resolutionBuild = approval ? builds.get(approval.head) : undefined;
    if (!resolutionBuild) continue;
    const key = `finding-resolution:${id}`;
    await input.control.recordReleaseReceipt({
      receiptId: deterministicUuid(input.release.id, key),
      receiptKey: key,
      releaseId: input.release.id,
      repository: input.release.repository,
      issueNumber: input.release.issueNumber,
      producer: input.producer,
      causes: [origin.receiptId, resolutionBuild.receiptId],
      recordedAt: after(origin.recordedAt, resolutionBuild.recordedAt),
      kind: 'finding-resolution',
      findingId: id,
      raisedByReviewReceiptId: origin.receiptId,
      raisedOnHead: origin.head,
      resolvedByBuildReceiptId: resolutionBuild.receiptId,
      resolvedOnHead: resolutionBuild.head,
    });
  }

  if (!authorizeMerge) return;

  const preMerge = (await input.control.listReleaseReceipts(input.release.id))
    .map((entry) => entry.receipt)
    .filter((receipt) => receipt.kind !== 'merge-intent' && receipt.kind !== 'merge');
  const key = `merge-intent:${input.pullRequestNumber}:${head}`;
  await input.control.authorizeReleaseMerge({
    releaseId: input.release.id,
    intent: {
      receiptId: deterministicUuid(input.release.id, key),
      receiptKey: key,
      releaseId: input.release.id,
      repository: input.release.repository,
      issueNumber: input.release.issueNumber,
      producer: input.producer,
      causes: preMerge.map((receipt) => receipt.receiptId),
      recordedAt: after(...preMerge.map((receipt) => receipt.recordedAt)),
      kind: 'merge-intent',
      pullRequestNumber: input.pullRequestNumber,
      expectedHead: head,
      observedPrHead: head,
    },
  });
}

/** Persist progress after a reviewed head even when it requests another job. */
export async function projectReleaseProgress(
  input: ProjectReleasePreMergeInput,
): Promise<void> {
  await projectReleaseEvidence(input, false);
}

/** Project all current facts and durably authorize the exact merge head. */
export async function projectReleasePreMerge(
  input: ProjectReleasePreMergeInput,
): Promise<void> {
  await projectReleaseEvidence(input, true);
}

export async function projectReleaseMerge(
  control: ReleaseReceiptControl,
  release: ReleaseRecord,
  producer: { jobId: string; attemptId: string },
  observation: GithubReleaseObservation,
): Promise<void> {
  const receipts = (await control.listReleaseReceipts(release.id, {
    includeMergeIntent: true,
  })).map((entry) => entry.receipt);
  const intent = receipts.find((receipt) => receipt.kind === 'merge-intent');
  if (!intent) throw new Error('release merge observation has no durable merge intent');
  const key = `merge:${observation.pullRequestNumber}:${observation.mergeSha}`;
  await control.completeReleaseMerge({
    releaseId: release.id,
    receipt: {
      receiptId: deterministicUuid(release.id, key),
      receiptKey: key,
      releaseId: release.id,
      repository: release.repository,
      issueNumber: release.issueNumber,
      producer,
      causes: [intent.receiptId],
      recordedAt: observation.mergedAt,
      kind: 'merge',
      ...observation,
    },
  });
}

export type ReleaseProjectionReceipt = DurableReleaseReceipt;
