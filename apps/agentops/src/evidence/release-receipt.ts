import { z } from 'zod';
import { REVIEW_PERSPECTIVE_KEYS } from '../domain/review-perspectives.js';
import { INTERVENTION_KINDS } from '../domain/schema.js';
import { HARD_GATE_SIGNAL_NAMES } from '../graders/gate-names.js';

const Repository = z.string().regex(
  /^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?\/(?!\.{1,2}$)[a-z0-9_.-]{1,100}$/,
);
const Uuid = z.string().uuid();
const Head = z.string().regex(/^[0-9a-f]{40}$/);
const Sha256 = z.string().regex(/^[0-9a-f]{64}$/);
const Digest = z.string().regex(/^sha256:[0-9a-f]{64}$/);
const Timestamp = z.string().datetime({ offset: true });
const ReceiptKey = z.string().min(1).max(256).regex(/^[A-Za-z0-9][A-Za-z0-9._:/-]*$/);
const BoundedName = z.string().min(1).max(128);

function record(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

/**
 * Read legacy v2/v3 receipts through one explicit anti-corruption layer. New
 * writes are canonical v4. A legacy invocation without the additive key can
 * only fall back to its opaque ref; the original provider key is not claimed
 * to be recoverable.
 */
function normalizeDurableReceipt(input: unknown): unknown {
  const value = record(input);
  if (!value) return input;
  if (value.kind === 'authority' && value.route === 'ai-triage-then-human-ready'
    && value.triageInvocationRef === undefined && value.triageInvocationId !== undefined) {
    const { triageInvocationId, ...rest } = value;
    return { ...rest, triageInvocationRef: triageInvocationId };
  }
  if ((value.kind === 'build' || value.kind === 'review')
    && value.invocationRef === undefined && value.invocationId !== undefined) {
    const { invocationId, ...rest } = value;
    return normalizeDurableReceipt({ ...rest, invocationRef: invocationId });
  }
  if (value.kind === 'runtime-provenance' && Array.isArray(value.invocations)) {
    return {
      ...value,
      invocations: value.invocations.map((candidate) => {
        const invocation = record(candidate);
        if (!invocation || invocation.invocationRef !== undefined) return candidate;
        const { invocationId, ...rest } = invocation;
        return {
          ...rest,
          invocationKey: invocation.invocationKey ?? invocationId,
          invocationRef: invocationId,
        };
      }),
    };
  }
  if (value.kind === 'review') {
    const findings = Array.isArray(value.findings) ? value.findings : [];
    const legacyVerdict = value.verdict === 'approved'
      ? 'approve'
      : value.verdict === 'findings'
        ? 'request_changes'
        : value.verdict;
    return {
      ...value,
      verdict: legacyVerdict,
      hasFindings: value.hasFindings ?? findings.length > 0,
    };
  }
  if ((value.kind === 'merge-intent' || value.kind === 'merge')
    && value.pullRequestNumber === undefined && value.pullRequest !== undefined) {
    const { pullRequest, ...rest } = value;
    return normalizeDurableReceipt({ ...rest, pullRequestNumber: pullRequest });
  }
  if (value.kind === 'merge' && value.sourceIssueClosure === undefined
    && value.issueState === 'CLOSED' && value.issueStateReason === 'COMPLETED') {
    const { issueState: _issueState, issueStateReason: _issueStateReason, ...rest } = value;
    return { ...rest, sourceIssueClosure: 'completed' };
  }
  return value;
}

function normalizeLiveReleaseEvidence(input: unknown): unknown {
  const value = record(input);
  const release = record(value?.release);
  const receipts = record(value?.receipts);
  if (!value || !release || !receipts) return input;
  const canonicalRelease = release.pullRequestNumber === undefined
    && release.pullRequest !== undefined
    ? (() => {
      const { pullRequest, ...rest } = release;
      return { ...rest, pullRequestNumber: pullRequest };
    })()
    : release;
  return {
    ...value,
    release: canonicalRelease,
    receipts: {
      ...receipts,
      authority: normalizeDurableReceipt(receipts.authority),
      requirementsAuthority: receipts.requirementsAuthority === undefined
        ? undefined
        : normalizeDurableReceipt(receipts.requirementsAuthority),
      runtime: Array.isArray(receipts.runtime)
        ? receipts.runtime.map(normalizeDurableReceipt)
        : receipts.runtime,
      builds: Array.isArray(receipts.builds)
        ? receipts.builds.map(normalizeDurableReceipt)
        : receipts.builds,
      grades: Array.isArray(receipts.grades)
        ? receipts.grades.map(normalizeDurableReceipt)
        : receipts.grades,
      reviews: Array.isArray(receipts.reviews)
        ? receipts.reviews.map(normalizeDurableReceipt)
        : receipts.reviews,
      findingResolutions: Array.isArray(receipts.findingResolutions)
        ? receipts.findingResolutions.map(normalizeDurableReceipt)
        : receipts.findingResolutions,
      mergeIntent: normalizeDurableReceipt(receipts.mergeIntent),
      merge: normalizeDurableReceipt(receipts.merge),
      interventions: Array.isArray(receipts.interventions)
        ? receipts.interventions.map(normalizeDurableReceipt)
        : receipts.interventions,
    },
  };
}

/**
 * Serialize a canonical in-memory certificate back to an immutable v2/v3 wire.
 * The old two-valued review field cannot preserve `needs_human`; that loss is
 * a property of the historical wire and is never used for new v4 evidence.
 */
export function legacyLiveReleaseReceiptEvidenceWire(
  evidence: LiveReleaseReceiptEvidence,
  schemaVersion: '2.0' | '3.0' = '2.0',
): unknown {
  const legacyReceipt = (receipt: HistoricalDurableReleaseReceipt): unknown => {
    if (receipt.kind === 'authority'
      && receipt.route === 'ai-triage-then-human-ready') {
      const { triageInvocationRef, ...rest } = receipt;
      return { ...rest, triageInvocationId: triageInvocationRef };
    }
    if (receipt.kind === 'build') {
      const { invocationRef, ...rest } = receipt;
      return { ...rest, invocationId: invocationRef };
    }
    if (receipt.kind === 'review') {
      const { invocationRef, hasFindings: _hasFindings, ...rest } = receipt;
      return {
        ...rest,
        invocationId: invocationRef,
        verdict: receipt.verdict === 'approve' ? 'approved' : 'findings',
      };
    }
    if (receipt.kind === 'runtime-provenance') {
      return {
        ...receipt,
        invocations: receipt.invocations.map((invocation) => {
          const { invocationRef, ...rest } = invocation;
          return { ...rest, invocationId: invocationRef };
        }),
      };
    }
    if (receipt.kind === 'merge-intent') {
      const { pullRequestNumber, ...rest } = receipt;
      return { ...rest, pullRequest: pullRequestNumber };
    }
    if (receipt.kind === 'merge') {
      const {
        pullRequestNumber,
        sourceIssueClosure: _sourceIssueClosure,
        ...rest
      } = receipt;
      return {
        ...rest,
        pullRequest: pullRequestNumber,
        issueState: 'CLOSED',
        issueStateReason: 'COMPLETED',
      };
    }
    return receipt;
  };
  const { pullRequestNumber, ...release } = evidence.release;
  return {
    ...evidence,
    schemaVersion,
    release: { ...release, pullRequest: pullRequestNumber },
    receipts: {
      authority: legacyReceipt(evidence.receipts.authority),
      ...(schemaVersion === '3.0' && evidence.receipts.requirementsAuthority
        ? { requirementsAuthority: legacyReceipt(evidence.receipts.requirementsAuthority) }
        : {}),
      runtime: evidence.receipts.runtime.map(legacyReceipt),
      builds: evidence.receipts.builds.map(legacyReceipt),
      grades: evidence.receipts.grades.map(legacyReceipt),
      reviews: evidence.receipts.reviews.map(legacyReceipt),
      findingResolutions: evidence.receipts.findingResolutions.map(legacyReceipt),
      mergeIntent: legacyReceipt(evidence.receipts.mergeIntent),
      merge: legacyReceipt(evidence.receipts.merge),
      interventions: evidence.receipts.interventions.map(legacyReceipt),
    },
  };
}
const GitHubCheckSignalContract = z.object({
  source: z.literal('github-check'),
  name: BoundedName,
}).strict();
const RepositoryGraderSignalContract = z.object({
  source: z.literal('repository-grader'),
  name: z.enum(HARD_GATE_SIGNAL_NAMES),
}).strict();
const HistoricalRepositoryGraderSignalContract = z.object({
  source: z.literal('repository-grader'),
  name: BoundedName,
}).strict();

/** Canonical source-discriminated signal contract used by every new writer. */
export const ReleaseGateSignalContract = z.discriminatedUnion('source', [
  RepositoryGraderSignalContract,
  GitHubCheckSignalContract,
]);
const HistoricalReleaseGateSignalContract = z.discriminatedUnion('source', [
  HistoricalRepositoryGraderSignalContract,
  GitHubCheckSignalContract,
]);

const ReceiptBase = z.object({
  receiptId: Uuid,
  receiptKey: ReceiptKey,
  releaseId: Uuid,
  repository: Repository,
  issueNumber: z.number().int().positive().max(2_147_483_647),
  producer: z.object({
    jobId: Uuid.optional(),
    attemptId: Uuid.optional(),
  }).strict(),
  causes: z.array(Uuid).max(512),
  recordedAt: Timestamp,
}).strict();

const HumanActor = z.object({
  type: z.literal('human'),
  login: BoundedName,
}).strict();

const HumanReadyAuthority = ReceiptBase.extend({
  kind: z.literal('authority'),
  route: z.literal('human-ready'),
  actor: HumanActor,
  readyLabel: BoundedName,
  readyAt: Timestamp,
}).strict();

const AiTriageAuthority = ReceiptBase.extend({
  kind: z.literal('authority'),
  route: z.literal('ai-triage-then-human-ready'),
  actor: HumanActor,
  readyLabel: BoundedName,
  readyAt: Timestamp,
  triageInvocationRef: BoundedName,
  triageCompletedAt: Timestamp,
  sourceDigest: Sha256,
  decision: z.object({
    schemaVersion: z.literal(1),
    readiness: z.literal('ready_candidate'),
  }).strict(),
}).strict();

export const ReleaseRequirementsAuthorityReceiptContract = ReceiptBase.extend({
  kind: z.literal('requirements-authority'),
  sourceIssueDigest: Sha256,
  sourceUpdatedAt: Timestamp,
  capturedAt: Timestamp,
}).strict();
export type ReleaseRequirementsAuthorityReceipt = z.infer<
  typeof ReleaseRequirementsAuthorityReceiptContract
>;

export const ReleaseAuthorityReceiptContract = z.preprocess(
  normalizeDurableReceipt,
  z.discriminatedUnion('route', [HumanReadyAuthority, AiTriageAuthority]),
);
export type ReleaseAuthorityReceipt = z.infer<
  typeof ReleaseAuthorityReceiptContract
>;

const CanonicalReleaseBuildReceiptContract = ReceiptBase.extend({
  kind: z.literal('build'),
  head: Head,
  parentHead: Head.nullable(),
  invocationRef: BoundedName,
  role: z.enum(['generator', 'repair']),
}).strict();
export const ReleaseBuildReceiptContract = z.preprocess(
  normalizeDurableReceipt,
  CanonicalReleaseBuildReceiptContract,
);
export type ReleaseBuildReceipt = z.infer<typeof ReleaseBuildReceiptContract>;

export const ReleaseGradeReceiptContract = ReceiptBase.extend({
  kind: z.literal('grade'),
  head: Head,
  signal: ReleaseGateSignalContract,
  status: z.literal('passed'),
  detailsDigest: Digest,
}).strict();
export type ReleaseGradeReceipt = z.infer<typeof ReleaseGradeReceiptContract>;

/** Decode-only grade receipt for immutable v2/v3 evidence written before Stage 2. */
export const HistoricalReleaseGradeReceiptContract = ReceiptBase.extend({
  kind: z.literal('grade'),
  head: Head,
  signal: HistoricalReleaseGateSignalContract,
  status: z.literal('passed'),
  detailsDigest: Digest,
}).strict();
export type HistoricalReleaseGradeReceipt = z.infer<
  typeof HistoricalReleaseGradeReceiptContract
>;

const CanonicalReleaseReviewReceiptContract = ReceiptBase.extend({
  kind: z.literal('review'),
  head: Head,
  headEpoch: z.number().int().positive().max(1_024),
  perspective: BoundedName,
  invocationRef: BoundedName,
  verdict: z.enum(['approve', 'request_changes', 'needs_human']),
  hasFindings: z.boolean(),
  findings: z.array(z.object({
    findingId: BoundedName,
    lineage: z.enum(['new', 'persisted']),
  }).strict()).max(1_024),
}).strict().superRefine((review, context) => {
  if (review.hasFindings !== (review.findings.length > 0)) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['hasFindings'],
      message: 'hasFindings must agree with findings',
    });
  }
  if (review.verdict === 'approve' && review.hasFindings) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['findings'],
      message: 'approve verdict cannot contain findings',
    });
  }
});
export const ReleaseReviewReceiptContract = z.preprocess(
  normalizeDurableReceipt,
  CanonicalReleaseReviewReceiptContract,
);
export type ReleaseReviewReceipt = z.infer<typeof ReleaseReviewReceiptContract>;

export const ReleaseFindingResolutionReceiptContract = ReceiptBase.extend({
  kind: z.literal('finding-resolution'),
  findingId: BoundedName,
  raisedByReviewReceiptId: Uuid,
  raisedOnHead: Head,
  resolvedByBuildReceiptId: Uuid,
  resolvedOnHead: Head,
}).strict();
export type ReleaseFindingResolutionReceipt = z.infer<
  typeof ReleaseFindingResolutionReceiptContract
>;

export const ProviderModelSelectionContract = z.discriminatedUnion('kind', [
  z.object({
    kind: z.literal('explicit'),
    name: BoundedName,
  }).strict(),
  z.object({
    kind: z.literal('provider-default'),
    reference: z.string().min(1).max(256),
    resolverDigest: Digest,
  }).strict(),
]);

export const ReleaseRuntimeConsumerContract = z.object({
  repository: Repository,
  revision: Head,
}).strict();
export const ReleaseRuntimeEnvironmentContract = z.object({
  kind: z.enum(['container', 'host', 'managed-runner']),
  reference: z.string().min(1).max(512),
  digest: Digest,
}).strict();

const CanonicalReleaseRuntimeReceiptContract = ReceiptBase.extend({
  kind: z.literal('runtime-provenance'),
  consumer: ReleaseRuntimeConsumerContract,
  environment: ReleaseRuntimeEnvironmentContract,
  invocations: z.array(z.object({
    invocationKey: BoundedName,
    invocationRef: BoundedName,
    role: z.enum(['triage', 'planning', 'ui-design', 'generator', 'repair', 'reviewer']),
    provider: BoundedName,
    model: ProviderModelSelectionContract,
    head: Head.optional(),
  }).strict()).min(1).max(512),
}).strict();
export const ReleaseRuntimeReceiptContract = z.preprocess(
  normalizeDurableReceipt,
  CanonicalReleaseRuntimeReceiptContract,
);
export type ReleaseRuntimeReceipt = z.infer<typeof ReleaseRuntimeReceiptContract>;

const CanonicalReleaseMergeIntentReceiptContract = ReceiptBase.extend({
  kind: z.literal('merge-intent'),
  pullRequestNumber: z.number().int().positive().max(2_147_483_647),
  expectedHead: Head,
  observedPrHead: Head,
}).strict();
export const ReleaseMergeIntentReceiptContract = z.preprocess(
  normalizeDurableReceipt,
  CanonicalReleaseMergeIntentReceiptContract,
);
export type ReleaseMergeIntentReceipt = z.infer<
  typeof ReleaseMergeIntentReceiptContract
>;

const CanonicalReleaseMergeReceiptContract = ReceiptBase.extend({
  kind: z.literal('merge'),
  pullRequestNumber: z.number().int().positive().max(2_147_483_647),
  expectedHead: Head,
  observedPrHead: Head,
  mergeSha: Head,
  actor: BoundedName,
  sourceIssueClosure: z.literal('completed'),
  mergeReachableFromDefaultBranch: z.literal(true),
  mergedAt: Timestamp,
}).strict();
export const ReleaseMergeReceiptContract = z.preprocess(
  normalizeDurableReceipt,
  CanonicalReleaseMergeReceiptContract,
);
export type ReleaseMergeReceipt = z.infer<typeof ReleaseMergeReceiptContract>;

export const ReleaseInterventionReceiptContract = ReceiptBase.extend({
  kind: z.literal('intervention'),
  interventionKind: z.enum(INTERVENTION_KINDS),
  reason: z.string().trim().min(1).max(2_000),
}).strict();
export type ReleaseInterventionReceipt = z.infer<
  typeof ReleaseInterventionReceiptContract
>;

export const DurableReleaseReceiptContract = z.union([
  ReleaseAuthorityReceiptContract,
  ReleaseRequirementsAuthorityReceiptContract,
  ReleaseBuildReceiptContract,
  ReleaseGradeReceiptContract,
  ReleaseReviewReceiptContract,
  ReleaseFindingResolutionReceiptContract,
  ReleaseRuntimeReceiptContract,
  ReleaseMergeIntentReceiptContract,
  ReleaseMergeReceiptContract,
  ReleaseInterventionReceiptContract,
]);
export type DurableReleaseReceipt = z.infer<typeof DurableReleaseReceiptContract>;

const HistoricalAiTriageAuthority = ReceiptBase.extend({
  kind: z.literal('authority'),
  route: z.literal('ai-triage-then-human-ready'),
  actor: HumanActor,
  readyLabel: BoundedName,
  readyAt: Timestamp,
  triageInvocationId: BoundedName,
  triageCompletedAt: Timestamp,
  sourceDigest: Sha256,
  decision: z.object({
    schemaVersion: z.literal(1),
    readiness: z.literal('ready_candidate'),
  }).strict(),
}).strict();
const HistoricalReleaseAuthorityReceiptContract = z.discriminatedUnion('route', [
  HumanReadyAuthority,
  HistoricalAiTriageAuthority,
]);
const HistoricalReleaseBuildReceiptContract = ReceiptBase.extend({
  kind: z.literal('build'),
  head: Head,
  parentHead: Head.nullable(),
  invocationId: BoundedName,
  role: z.enum(['generator', 'repair']),
}).strict();
const HistoricalReleaseReviewReceiptContract = ReceiptBase.extend({
  kind: z.literal('review'),
  head: Head,
  headEpoch: z.number().int().positive().max(1_024),
  perspective: BoundedName,
  invocationId: BoundedName,
  verdict: z.enum(['approved', 'findings']),
  findings: z.array(z.object({
    findingId: BoundedName,
    lineage: z.enum(['new', 'persisted']),
  }).strict()).max(1_024),
}).strict();
const HistoricalReleaseRuntimeReceiptContract = ReceiptBase.extend({
  kind: z.literal('runtime-provenance'),
  consumer: ReleaseRuntimeConsumerContract,
  environment: ReleaseRuntimeEnvironmentContract,
  invocations: z.array(z.object({
    invocationId: BoundedName,
    invocationKey: BoundedName.optional(),
    role: z.enum(['triage', 'planning', 'ui-design', 'generator', 'repair', 'reviewer']),
    provider: BoundedName,
    model: ProviderModelSelectionContract,
    head: Head.optional(),
  }).strict()).min(1).max(512),
}).strict();
const HistoricalReleaseMergeIntentReceiptContract = ReceiptBase.extend({
  kind: z.literal('merge-intent'),
  pullRequest: z.number().int().positive().max(2_147_483_647),
  expectedHead: Head,
  observedPrHead: Head,
}).strict();
const HistoricalReleaseMergeReceiptContract = ReceiptBase.extend({
  kind: z.literal('merge'),
  pullRequest: z.number().int().positive().max(2_147_483_647),
  expectedHead: Head,
  observedPrHead: Head,
  mergeSha: Head,
  actor: BoundedName,
  issueState: z.literal('CLOSED'),
  issueStateReason: z.literal('COMPLETED'),
  mergeReachableFromDefaultBranch: z.literal(true),
  mergedAt: Timestamp,
}).strict();

/** Decode durable rows written before canonical grader names were enforced. */
export const HistoricalDurableReleaseReceiptContract = z.union([
  ReleaseAuthorityReceiptContract,
  ReleaseRequirementsAuthorityReceiptContract,
  ReleaseBuildReceiptContract,
  HistoricalReleaseGradeReceiptContract,
  ReleaseReviewReceiptContract,
  ReleaseFindingResolutionReceiptContract,
  ReleaseRuntimeReceiptContract,
  ReleaseMergeIntentReceiptContract,
  ReleaseMergeReceiptContract,
  ReleaseInterventionReceiptContract,
]);
export type HistoricalDurableReleaseReceipt = z.infer<
  typeof HistoricalDurableReleaseReceiptContract
>;

const RequiredReviewPerspectivesContract = z.array(z.enum(REVIEW_PERSPECTIVE_KEYS))
  .min(2)
  .max(REVIEW_PERSPECTIVE_KEYS.length)
  .superRefine((perspectives, context) => {
    const seen = new Set<string>();
    perspectives.forEach((perspective, index) => {
      if (seen.has(perspective)) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          path: [index],
          message: 'required review perspectives must be unique',
        });
      }
      seen.add(perspective);
    });
  });

const HistoricalRequiredReviewPerspectivesContract = z
  .array(z.enum(REVIEW_PERSPECTIVE_KEYS))
  .min(2)
  .max(REVIEW_PERSPECTIVE_KEYS.length);

export const ReleasePolicyContract = z.object({
  authority: z.enum(['human-ready-allowed', 'ai-triage-required']),
  requiredGateSignals: z.array(ReleaseGateSignalContract).min(1).max(64)
    .superRefine((signals, context) => {
      const seen = new Set<string>();
      signals.forEach((signal, index) => {
        const key = `${signal.source}:${signal.name}`;
        if (seen.has(key)) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            path: [index],
            message: 'required gate signals must be unique by source and name',
          });
        }
        seen.add(key);
      });
    }),
  requiredReviewPerspectives: RequiredReviewPerspectivesContract,
  minimumHeadEpochs: z.number().int().positive().max(32),
}).strict();
export type ReleasePolicy = z.infer<typeof ReleasePolicyContract>;

/**
 * Decode-only persisted policy; grader names and requirement-array uniqueness
 * were historically looser. New writers use ReleasePolicyContract above.
 */
export const HistoricalReleasePolicyContract = z.object({
  authority: z.enum(['human-ready-allowed', 'ai-triage-required']),
  requiredGateSignals: z.array(HistoricalReleaseGateSignalContract).min(1).max(64),
  requiredReviewPerspectives: HistoricalRequiredReviewPerspectivesContract,
  minimumHeadEpochs: z.number().int().positive().max(32),
}).strict();
export type HistoricalReleasePolicy = z.infer<
  typeof HistoricalReleasePolicyContract
>;

export const ReleaseArtifactContract = z.object({
  kind: BoundedName,
  uri: z.string().min(1).max(1_024),
  sha256: Sha256,
  sizeBytes: z.number().int().nonnegative(),
  releaseId: Uuid,
  sourceHead: Head,
  receiptIds: z.array(Uuid).min(1).max(512),
}).strict();
export type ReleaseArtifact = z.infer<typeof ReleaseArtifactContract>;

const ReleaseEvidenceRecord = z.object({
  id: Uuid,
  repository: Repository,
  issueNumber: z.number().int().positive().max(2_147_483_647),
  pullRequestNumber: z.number().int().positive().max(2_147_483_647),
  finalHead: Head,
  mergeSha: Head,
  createdAt: Timestamp,
  completedAt: Timestamp,
}).strict();
const ReleaseReceiptCollection = {
  authority: ReleaseAuthorityReceiptContract,
  runtime: z.array(ReleaseRuntimeReceiptContract).min(1).max(256),
  builds: z.array(ReleaseBuildReceiptContract).min(1).max(256),
  reviews: z.array(ReleaseReviewReceiptContract).min(2).max(512),
  findingResolutions: z.array(ReleaseFindingResolutionReceiptContract).max(1_024),
  mergeIntent: ReleaseMergeIntentReceiptContract,
  merge: ReleaseMergeReceiptContract,
  interventions: z.array(ReleaseInterventionReceiptContract).max(256),
};
const ReleaseEvidenceEnvelope = {
  release: ReleaseEvidenceRecord,
  artifacts: z.array(ReleaseArtifactContract).min(1).max(256),
  result: z.enum(['passed', 'passed-with-interventions']),
};

const NormalizedHistoricalLiveReleaseReceiptEvidenceV2Contract = z.object({
  ...ReleaseEvidenceEnvelope,
  schemaVersion: z.literal('2.0'),
  policy: HistoricalReleasePolicyContract,
  receipts: z.object({
    ...ReleaseReceiptCollection,
    requirementsAuthority: z.never().optional(),
    grades: z.array(HistoricalReleaseGradeReceiptContract).min(1).max(256),
  }).strict(),
}).strict();

const NormalizedHistoricalLiveReleaseReceiptEvidenceV3Contract = z.object({
  ...ReleaseEvidenceEnvelope,
  schemaVersion: z.literal('3.0'),
  policy: HistoricalReleasePolicyContract,
  receipts: z.object({
    ...ReleaseReceiptCollection,
    requirementsAuthority: ReleaseRequirementsAuthorityReceiptContract,
    grades: z.array(HistoricalReleaseGradeReceiptContract).min(1).max(256),
  }).strict(),
}).strict();

const LiveReleaseReceiptEvidenceV4Contract = z.object({
  ...ReleaseEvidenceEnvelope,
  schemaVersion: z.literal('4.0'),
  policy: ReleasePolicyContract,
  receipts: z.object({
    ...ReleaseReceiptCollection,
    requirementsAuthority: ReleaseRequirementsAuthorityReceiptContract,
    grades: z.array(ReleaseGradeReceiptContract).min(1).max(256),
  }).strict(),
}).strict();

const CanonicalLiveReleaseReceiptEvidenceContract = z.discriminatedUnion(
  'schemaVersion',
  [
    NormalizedHistoricalLiveReleaseReceiptEvidenceV2Contract,
    NormalizedHistoricalLiveReleaseReceiptEvidenceV3Contract,
    LiveReleaseReceiptEvidenceV4Contract,
  ],
);

export const LiveReleaseReceiptEvidenceContract = z.preprocess(
  normalizeLiveReleaseEvidence,
  CanonicalLiveReleaseReceiptEvidenceContract,
);
export type LiveReleaseReceiptEvidence = z.infer<
  typeof LiveReleaseReceiptEvidenceContract
>;

const HistoricalReleaseEvidenceRecord = z.object({
  id: Uuid,
  repository: Repository,
  issueNumber: z.number().int().positive().max(2_147_483_647),
  pullRequest: z.number().int().positive().max(2_147_483_647),
  finalHead: Head,
  mergeSha: Head,
  createdAt: Timestamp,
  completedAt: Timestamp,
}).strict();
const HistoricalReleaseReceiptCollection = {
  authority: HistoricalReleaseAuthorityReceiptContract,
  runtime: z.array(HistoricalReleaseRuntimeReceiptContract).min(1).max(256),
  builds: z.array(HistoricalReleaseBuildReceiptContract).min(1).max(256),
  reviews: z.array(HistoricalReleaseReviewReceiptContract).min(2).max(512),
  findingResolutions: z.array(ReleaseFindingResolutionReceiptContract).max(1_024),
  mergeIntent: HistoricalReleaseMergeIntentReceiptContract,
  merge: HistoricalReleaseMergeReceiptContract,
  interventions: z.array(ReleaseInterventionReceiptContract).max(256),
};
const HistoricalReleaseEvidenceEnvelope = {
  release: HistoricalReleaseEvidenceRecord,
  artifacts: z.array(ReleaseArtifactContract).min(1).max(256),
  result: z.enum(['passed', 'passed-with-interventions']),
};
const HistoricalLiveReleaseReceiptEvidenceV2Contract = z.object({
  ...HistoricalReleaseEvidenceEnvelope,
  schemaVersion: z.literal('2.0'),
  policy: HistoricalReleasePolicyContract,
  receipts: z.object({
    ...HistoricalReleaseReceiptCollection,
    requirementsAuthority: z.never().optional(),
    grades: z.array(HistoricalReleaseGradeReceiptContract).min(1).max(256),
  }).strict(),
}).strict();
const HistoricalLiveReleaseReceiptEvidenceV3Contract = z.object({
  ...HistoricalReleaseEvidenceEnvelope,
  schemaVersion: z.literal('3.0'),
  policy: HistoricalReleasePolicyContract,
  receipts: z.object({
    ...HistoricalReleaseReceiptCollection,
    requirementsAuthority: ReleaseRequirementsAuthorityReceiptContract,
    grades: z.array(HistoricalReleaseGradeReceiptContract).min(1).max(256),
  }).strict(),
}).strict();

export const PersistedLiveReleaseReceiptEvidenceContract = z.discriminatedUnion(
  'schemaVersion',
  [
    HistoricalLiveReleaseReceiptEvidenceV2Contract,
    HistoricalLiveReleaseReceiptEvidenceV3Contract,
    LiveReleaseReceiptEvidenceV4Contract,
  ],
);
export type PersistedLiveReleaseReceiptEvidence = z.infer<
  typeof PersistedLiveReleaseReceiptEvidenceContract
>;

/** @deprecated Use PersistedLiveReleaseReceiptEvidenceContract. */
export const LiveReleaseReceiptEvidenceV2Contract =
  PersistedLiveReleaseReceiptEvidenceContract;
/** @deprecated Use PersistedLiveReleaseReceiptEvidence. */
export type LiveReleaseReceiptEvidenceV2 = PersistedLiveReleaseReceiptEvidence;

type SemanticDurableReleaseReceipt = HistoricalDurableReleaseReceipt;

function receiptList(
  evidence: LiveReleaseReceiptEvidence,
): SemanticDurableReleaseReceipt[] {
  return [
    evidence.receipts.authority,
    ...(evidence.receipts.requirementsAuthority
      ? [evidence.receipts.requirementsAuthority]
      : []),
    ...evidence.receipts.runtime,
    ...evidence.receipts.builds,
    ...evidence.receipts.grades,
    ...evidence.receipts.reviews,
    ...evidence.receipts.findingResolutions,
    evidence.receipts.mergeIntent,
    evidence.receipts.merge,
    ...evidence.receipts.interventions,
  ];
}

function signalKey(signal: { source: string; name: string }): string {
  return `${signal.source}:${signal.name}`;
}

function invocationByRef(evidence: LiveReleaseReceiptEvidence) {
  return new Map(
    evidence.receipts.runtime.flatMap((receipt) => receipt.invocations).map((invocation) => [
      invocation.invocationRef,
      invocation,
    ]),
  );
}

/**
 * Binds independently produced receipts to one release and verifies their
 * causal graph. Job coordinates remain optional provenance and are never used
 * as release identity.
 */
export function liveReleaseReceiptSemanticErrors(input: unknown): string[] {
  const parsed = LiveReleaseReceiptEvidenceContract.safeParse(input);
  if (!parsed.success) {
    return parsed.error.issues.map((issue) => (
      `${issue.path.join('.') || '$'}: ${issue.message}`
    ));
  }
  const evidence = parsed.data;
  const errors: string[] = [];
  const release = evidence.release;
  const all = receiptList(evidence);
  const byId = new Map<string, SemanticDurableReleaseReceipt>();

  for (const receipt of all) {
    if (byId.has(receipt.receiptId)) {
      errors.push(`receiptId ${receipt.receiptId} must be unique`);
    }
    byId.set(receipt.receiptId, receipt);
    if (receipt.releaseId !== release.id) {
      errors.push(`${receipt.receiptKey}.releaseId must equal release.id`);
    }
    if (receipt.repository !== release.repository) {
      errors.push(`${receipt.receiptKey}.repository must equal release.repository`);
    }
    if (receipt.issueNumber !== release.issueNumber) {
      errors.push(`${receipt.receiptKey}.issueNumber must equal release.issueNumber`);
    }
  }

  const receiptKeys = all.map((receipt) => receipt.receiptKey);
  if (new Set(receiptKeys).size !== receiptKeys.length) {
    errors.push('receiptKey must be unique within a release');
  }

  for (const receipt of all) {
    if (new Set(receipt.causes).size !== receipt.causes.length) {
      errors.push(`${receipt.receiptKey}.causes must be unique`);
    }
    for (const cause of receipt.causes) {
      const source = byId.get(cause);
      if (!source) {
        errors.push(`${receipt.receiptKey}.causes references an unknown receipt`);
      } else if (source.receiptId === receipt.receiptId) {
        errors.push(`${receipt.receiptKey}.causes must not reference itself`);
      } else if (Date.parse(source.recordedAt) > Date.parse(receipt.recordedAt)) {
        errors.push(`${receipt.receiptKey} must not precede its cause ${source.receiptKey}`);
      }
    }
  }
  const visiting = new Set<string>();
  const visited = new Set<string>();
  const visit = (id: string): void => {
    if (visiting.has(id)) {
      errors.push(`receipt causality must be acyclic at ${id}`);
      return;
    }
    if (visited.has(id)) return;
    visiting.add(id);
    for (const cause of byId.get(id)?.causes ?? []) visit(cause);
    visiting.delete(id);
    visited.add(id);
  };
  for (const id of byId.keys()) visit(id);

  const authority = evidence.receipts.authority;
  if (authority.causes.length !== 0) {
    errors.push('authority receipt must be a causal root');
  }
  const requirementsAuthority = evidence.receipts.requirementsAuthority;
  if (requirementsAuthority && (
    requirementsAuthority.causes.length !== 1
    || requirementsAuthority.causes[0] !== authority.receiptId
  )) {
    errors.push('requirements authority must be caused only by the human-ready authority');
  }
  if (
    evidence.policy.authority === 'ai-triage-required'
    && authority.route !== 'ai-triage-then-human-ready'
  ) {
    errors.push('AI-triage-required policy needs an AI triage authority receipt');
  }

  const invocations = invocationByRef(evidence);
  const invocationCount = evidence.receipts.runtime.reduce(
    (count, receipt) => count + receipt.invocations.length,
    0,
  );
  if (invocations.size !== invocationCount) {
    errors.push('runtime invocationRef must be unique');
  }
  const invocationKeys = evidence.receipts.runtime.flatMap(
    (receipt) => receipt.invocations.map((invocation) => invocation.invocationKey),
  );
  if (new Set(invocationKeys).size !== invocationKeys.length) {
    errors.push('runtime invocationKey must be unique');
  }
  if (authority.route === 'ai-triage-then-human-ready') {
    if (Date.parse(authority.triageCompletedAt) > Date.parse(authority.readyAt)) {
      errors.push('AI triage authority must complete before the human ready event');
    }
    const invocation = invocations.get(authority.triageInvocationRef);
    if (!invocation || invocation.role !== 'triage' || invocation.head !== undefined) {
      errors.push('AI triage authority must reference one headless triage invocation');
    }
  }
  for (const runtime of evidence.receipts.runtime) {
    if (runtime.consumer.repository === release.repository) {
      errors.push('runtime consumer repository must differ from the release target');
    }
    if (!runtime.causes.includes(authority.receiptId)) {
      errors.push(`${runtime.receiptKey} must be caused by the authority receipt`);
    }
  }

  const buildsByHead = new Map<string, ReleaseBuildReceipt>();
  for (const build of evidence.receipts.builds) {
    if (buildsByHead.has(build.head)) {
      errors.push(`build head ${build.head} must have exactly one receipt`);
    }
    buildsByHead.set(build.head, build);
    if (!build.causes.includes(authority.receiptId)) {
      errors.push(`${build.receiptKey} must be caused by the authority receipt`);
    }
    const invocation = invocations.get(build.invocationRef);
    if (
      !invocation
      || invocation.role !== build.role
      || invocation.head !== build.head
    ) {
      errors.push(`${build.receiptKey} must reference its generator/repair role and head`);
    }
  }
  for (const build of evidence.receipts.builds) {
    if (build.parentHead === null) continue;
    const parent = buildsByHead.get(build.parentHead);
    if (!parent || !build.causes.includes(parent.receiptId)) {
      errors.push(`${build.receiptKey} must be caused by its parent build receipt`);
    }
  }
  const finalBuild = buildsByHead.get(release.finalHead);
  if (!finalBuild) errors.push('release.finalHead must have a build receipt');

  const requiredSignals = evidence.policy.requiredGateSignals.map(signalKey);
  const finalGrades = new Map<string, HistoricalReleaseGradeReceipt>();
  for (const grade of evidence.receipts.grades) {
    const build = buildsByHead.get(grade.head);
    if (!build || !grade.causes.includes(build.receiptId)) {
      errors.push(`${grade.receiptKey} must be caused by the build receipt for its head`);
    }
    if (grade.head === release.finalHead) {
      const key = signalKey(grade.signal);
      if (finalGrades.has(key)) errors.push(`final gate signal ${key} must be unique`);
      finalGrades.set(key, grade);
    }
  }
  for (const key of requiredSignals) {
    if (!finalGrades.has(key)) errors.push(`final head is missing required gate signal ${key}`);
  }

  const headByEpoch = new Map<number, string>();
  const epochByHead = new Map<string, number>();
  const reviewById = new Map<string, ReleaseReviewReceipt>();
  const raisedFindings = new Map<string, ReleaseReviewReceipt>();
  const latestFindingEpoch = new Map<string, number>();
  const finalApprovedPerspectives = new Map<string, ReleaseReviewReceipt>();
  for (const review of evidence.receipts.reviews) {
    reviewById.set(review.receiptId, review);
    const knownHead = headByEpoch.get(review.headEpoch);
    if (knownHead && knownHead !== review.head) {
      errors.push(`head epoch ${review.headEpoch} must identify one head`);
    }
    const knownEpoch = epochByHead.get(review.head);
    if (knownEpoch && knownEpoch !== review.headEpoch) {
      errors.push(`review head ${review.head} must identify one epoch`);
    }
    headByEpoch.set(review.headEpoch, review.head);
    epochByHead.set(review.head, review.headEpoch);
    const build = buildsByHead.get(review.head);
    if (!build || !review.causes.includes(build.receiptId)) {
      errors.push(`${review.receiptKey} must be caused by the build receipt for its head`);
    }
    const invocation = invocations.get(review.invocationRef);
    if (!invocation || invocation.role !== 'reviewer' || invocation.head !== review.head) {
      errors.push(`${review.receiptKey} must reference a reviewer invocation for its head`);
    }
    if (review.head === release.finalHead && review.verdict === 'approve') {
      if (finalApprovedPerspectives.has(review.perspective)) {
        errors.push(`final review perspective ${review.perspective} must be unique`);
      }
      finalApprovedPerspectives.set(review.perspective, review);
    }
  }
  const orderedReviews = [...evidence.receipts.reviews].sort((left, right) => (
    left.headEpoch - right.headEpoch
    || left.recordedAt.localeCompare(right.recordedAt)
    || left.receiptId.localeCompare(right.receiptId)
  ));
  for (const review of orderedReviews) {
    if (review.verdict === 'approve' && review.hasFindings) {
      errors.push(`${review.receiptKey}.approve verdict cannot contain findings`);
    }
    const findingIds = review.findings.map((finding) => finding.findingId);
    if (new Set(findingIds).size !== findingIds.length) {
      errors.push(`${review.receiptKey}.findings must be unique`);
    }
    for (const finding of review.findings) {
      const raised = raisedFindings.get(finding.findingId);
      const latestEpoch = latestFindingEpoch.get(finding.findingId);
      if (finding.lineage === 'new') {
        if (raised) {
          errors.push(`finding ${finding.findingId} must be raised exactly once`);
        } else {
          raisedFindings.set(finding.findingId, review);
        }
      } else if (
        !raised
        || raised.headEpoch >= review.headEpoch
        || latestEpoch === undefined
        || latestEpoch >= review.headEpoch
      ) {
        errors.push(
          `persisted finding ${finding.findingId} must reference an earlier head epoch`,
        );
      }
      latestFindingEpoch.set(finding.findingId, review.headEpoch);
    }
  }
  const epochs = [...headByEpoch.keys()].sort((left, right) => left - right);
  if (epochs.some((epoch, index) => epoch !== index + 1)) {
    errors.push('review head epochs must be contiguous and start at 1');
  }
  if (epochs.length < evidence.policy.minimumHeadEpochs) {
    errors.push('review evidence has fewer head epochs than policy requires');
  }
  const requiredPerspectives = evidence.policy.requiredReviewPerspectives;
  const finalEpoch = epochByHead.get(release.finalHead);
  if (
    finalEpoch === undefined
    || finalEpoch !== epochs.at(-1)
  ) {
    errors.push('release.finalHead must be the latest reviewed head epoch');
  }
  for (const perspective of requiredPerspectives) {
    if (!finalApprovedPerspectives.has(perspective)) {
      errors.push(`final head is missing approved review perspective ${perspective}`);
    }
  }

  const resolutions = new Map<string, ReleaseFindingResolutionReceipt>();
  for (const resolution of evidence.receipts.findingResolutions) {
    if (resolutions.has(resolution.findingId)) {
      errors.push(`finding ${resolution.findingId} must have one resolution receipt`);
    }
    resolutions.set(resolution.findingId, resolution);
    const raised = raisedFindings.get(resolution.findingId);
    if (
      !raised
      || raised.receiptId !== resolution.raisedByReviewReceiptId
      || raised.head !== resolution.raisedOnHead
    ) {
      errors.push(`${resolution.receiptKey} must bind to the review that raised its finding`);
    }
    const build = buildsByHead.get(resolution.resolvedOnHead);
    if (!build || build.receiptId !== resolution.resolvedByBuildReceiptId) {
      errors.push(`${resolution.receiptKey} must bind to the build that resolved its finding`);
    }
    const raisedEpoch = epochByHead.get(resolution.raisedOnHead);
    const resolvedEpoch = epochByHead.get(resolution.resolvedOnHead);
    const latestUnresolvedEpoch = latestFindingEpoch.get(resolution.findingId);
    if (
      !raisedEpoch
      || !resolvedEpoch
      || !latestUnresolvedEpoch
      || resolvedEpoch <= latestUnresolvedEpoch
    ) {
      errors.push(`${resolution.receiptKey} must resolve on a later reviewed head epoch`);
    }
    for (const cause of [
      resolution.raisedByReviewReceiptId,
      resolution.resolvedByBuildReceiptId,
    ]) {
      if (!resolution.causes.includes(cause)) {
        errors.push(`${resolution.receiptKey}.causes must include its finding lineage`);
      }
    }
  }
  for (const findingId of raisedFindings.keys()) {
    if (!resolutions.has(findingId)) {
      errors.push(`finding ${findingId} has no resolution receipt`);
    }
  }

  const mergeIntent = evidence.receipts.mergeIntent;
  if (
    mergeIntent.pullRequestNumber !== release.pullRequestNumber
    || mergeIntent.expectedHead !== release.finalHead
    || mergeIntent.observedPrHead !== release.finalHead
  ) {
    errors.push('merge intent must bind the release PR and final head');
  }
  const requiredIntentCauses = [
    authority.receiptId,
    ...(requirementsAuthority ? [requirementsAuthority.receiptId] : []),
    ...evidence.receipts.runtime.map((receipt) => receipt.receiptId),
    ...(finalBuild ? [finalBuild.receiptId] : []),
    ...requiredSignals.flatMap((key) => {
      const receipt = finalGrades.get(key);
      return receipt ? [receipt.receiptId] : [];
    }),
    ...evidence.policy.requiredReviewPerspectives.flatMap((perspective) => {
      const receipt = finalApprovedPerspectives.get(perspective);
      return receipt ? [receipt.receiptId] : [];
    }),
    ...evidence.receipts.findingResolutions.map((receipt) => receipt.receiptId),
    ...evidence.receipts.interventions.map((receipt) => receipt.receiptId),
  ];
  for (const cause of requiredIntentCauses) {
    if (!mergeIntent.causes.includes(cause)) {
      errors.push('merge intent must be caused by every required pre-merge receipt');
      break;
    }
  }

  const merge = evidence.receipts.merge;
  if (
    merge.pullRequestNumber !== release.pullRequestNumber
    || merge.expectedHead !== release.finalHead
    || merge.observedPrHead !== release.finalHead
    || merge.mergeSha !== release.mergeSha
  ) {
    errors.push('merge receipt must bind the release PR, final head, and merge SHA');
  }
  if (!merge.causes.includes(mergeIntent.receiptId)) {
    errors.push('merge receipt must be caused by its durable merge intent');
  }

  for (const [index, artifact] of evidence.artifacts.entries()) {
    if (artifact.releaseId !== release.id) {
      errors.push(`artifacts.${index}.releaseId must equal release.id`);
    }
    if (artifact.sourceHead !== release.finalHead) {
      errors.push(`artifacts.${index}.sourceHead must equal release.finalHead`);
    }
    if (new Set(artifact.receiptIds).size !== artifact.receiptIds.length) {
      errors.push(`artifacts.${index}.receiptIds must be unique`);
    }
    for (const receiptId of artifact.receiptIds) {
      if (!byId.has(receiptId)) {
        errors.push(`artifacts.${index}.receiptIds references an unknown receipt`);
      }
    }
  }
  const expectedResult = evidence.receipts.interventions.length === 0
    ? 'passed'
    : 'passed-with-interventions';
  if (evidence.result !== expectedResult) {
    errors.push(`result must be ${expectedResult}`);
  }
  if (Date.parse(release.createdAt) > Date.parse(authority.recordedAt)) {
    errors.push('release.createdAt must not be later than its authority receipt');
  }
  if (Date.parse(release.completedAt) < Date.parse(merge.mergedAt)) {
    errors.push('release.completedAt must not precede the merge');
  }
  return [...new Set(errors)];
}

export function assertLiveReleaseReceiptEvidence(input: unknown): asserts input is LiveReleaseReceiptEvidence {
  const errors = liveReleaseReceiptSemanticErrors(input);
  if (errors.length > 0) {
    throw new Error(`live release receipt semantics failed: ${errors.join('; ')}`);
  }
}

export function releasePreMergeSemanticErrors(input: {
  releaseId: string;
  repository: string;
  issueNumber: number;
  pullRequestNumber: number;
  expectedHead: string;
  policy: HistoricalReleasePolicy;
  receipts: readonly HistoricalDurableReleaseReceipt[];
}): string[] {
  const errors: string[] = [];
  const authority = input.receipts.filter((receipt) => receipt.kind === 'authority');
  const requirementsAuthority = input.receipts.filter(
    (receipt) => receipt.kind === 'requirements-authority',
  );
  const runtime = input.receipts.filter((receipt) => receipt.kind === 'runtime-provenance');
  const builds = input.receipts.filter((receipt) => receipt.kind === 'build');
  const grades = input.receipts.filter((receipt) => receipt.kind === 'grade');
  const reviews = input.receipts.filter((receipt) => receipt.kind === 'review');
  if (authority.length !== 1) errors.push('release needs exactly one authority receipt');
  if (requirementsAuthority.length !== 1) {
    errors.push('release needs exactly one frozen requirements authority receipt');
  }
  if (runtime.length < 1) errors.push('release needs at least one runtime provenance receipt');
  if (
    input.policy.authority === 'ai-triage-required'
    && authority[0]?.route !== 'ai-triage-then-human-ready'
  ) {
    errors.push('AI triage authority receipt is required before merge');
  }
  if (!builds.some((receipt) => receipt.head === input.expectedHead)) {
    errors.push('expected head has no build receipt');
  }
  for (const signal of input.policy.requiredGateSignals) {
    if (!grades.some((receipt) => (
      receipt.head === input.expectedHead
      && receipt.signal.source === signal.source
      && receipt.signal.name === signal.name
    ))) {
      errors.push(`expected head is missing ${signalKey(signal)}`);
    }
  }
  for (const perspective of input.policy.requiredReviewPerspectives) {
    if (!reviews.some((receipt) => (
      receipt.head === input.expectedHead
      && receipt.perspective === perspective
      && receipt.verdict === 'approve'
    ))) {
      errors.push(`expected head is missing approved review perspective ${perspective}`);
    }
  }
  const identities = input.receipts.every((receipt) => (
    receipt.releaseId === input.releaseId
    && receipt.repository === input.repository
    && receipt.issueNumber === input.issueNumber
  ));
  if (!identities) errors.push('pre-merge receipts contain mixed release identities');
  if (input.receipts.some((receipt) => receipt.kind === 'merge')) {
    errors.push('a collecting release must not already have a merge receipt');
  }
  if (
    errors.length > 0
    || authority.length !== 1
    || requirementsAuthority.length !== 1
    || runtime.length < 1
  ) {
    return errors;
  }

  const findingResolutions = input.receipts.filter(
    (receipt) => receipt.kind === 'finding-resolution',
  );
  const interventions = input.receipts.filter(
    (receipt) => receipt.kind === 'intervention',
  );
  const latestRecordedAt = input.receipts.reduce(
    (latest, receipt) => Math.max(latest, Date.parse(receipt.recordedAt)),
    0,
  );
  const completedAt = new Date(latestRecordedAt + 1).toISOString();
  const reservedIds = new Set(input.receipts.map((receipt) => receipt.receiptId));
  const syntheticId = (suffix: string): string => {
    const id = `00000000-0000-4000-8000-${suffix.padStart(12, '0')}`;
    if (reservedIds.has(id)) {
      throw new Error('pre-merge receipt set collides with certifier-reserved UUIDs');
    }
    reservedIds.add(id);
    return id;
  };
  const syntheticIntentId = syntheticId('1');
  const syntheticMergeId = syntheticId('2');
  const syntheticMergeSha = '0'.repeat(40);
  const mergeIntent: ReleaseMergeIntentReceipt = {
    receiptId: syntheticIntentId,
    receiptKey: 'merge-intent:preflight',
    releaseId: input.releaseId,
    repository: input.repository,
    issueNumber: input.issueNumber,
    producer: {},
    causes: input.receipts.map((receipt) => receipt.receiptId),
    recordedAt: completedAt,
    kind: 'merge-intent',
    pullRequestNumber: input.pullRequestNumber,
    expectedHead: input.expectedHead,
    observedPrHead: input.expectedHead,
  };
  const mergedAt = new Date(Date.parse(completedAt) + 1).toISOString();
  const fullErrors = liveReleaseReceiptSemanticErrors({
    // Preflight also certifies immutable v3 policies/grade aliases. Receipt
    // fields have already been normalized by the durable-row decoder.
    schemaVersion: '3.0',
    release: {
      id: input.releaseId,
      repository: input.repository,
      issueNumber: input.issueNumber,
      pullRequestNumber: input.pullRequestNumber,
      finalHead: input.expectedHead,
      mergeSha: syntheticMergeSha,
      createdAt: authority[0]!.recordedAt,
      completedAt: mergedAt,
    },
    policy: input.policy,
    receipts: {
      authority: authority[0],
      requirementsAuthority: requirementsAuthority[0],
      runtime,
      builds,
      grades,
      reviews,
      findingResolutions,
      mergeIntent,
      merge: {
        receiptId: syntheticMergeId,
        receiptKey: 'merge:preflight',
        releaseId: input.releaseId,
        repository: input.repository,
        issueNumber: input.issueNumber,
        producer: {},
        causes: [syntheticIntentId],
        recordedAt: mergedAt,
        kind: 'merge',
        pullRequestNumber: input.pullRequestNumber,
        expectedHead: input.expectedHead,
        observedPrHead: input.expectedHead,
        mergeSha: syntheticMergeSha,
        actor: 'pre-merge-certifier',
        sourceIssueClosure: 'completed',
        mergeReachableFromDefaultBranch: true,
        mergedAt,
      },
      interventions,
    },
    artifacts: [{
      kind: 'pre-merge-certification',
      uri: 'pre-merge://durable-receipts',
      sha256: '0'.repeat(64),
      sizeBytes: 0,
      releaseId: input.releaseId,
      sourceHead: input.expectedHead,
      receiptIds: input.receipts.map((receipt) => receipt.receiptId),
    }],
    result: interventions.length === 0 ? 'passed' : 'passed-with-interventions',
  });
  return [...new Set([...errors, ...fullErrors])];
}
