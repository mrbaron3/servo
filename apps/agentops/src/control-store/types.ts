import { z } from 'zod';
import { createHash } from 'node:crypto';

import {
  HistoricalReleasePolicyContract,
  ProviderModelSelectionContract,
  ReleaseRuntimeConsumerContract,
  ReleaseRuntimeEnvironmentContract,
  ReleasePolicyContract,
  type HistoricalDurableReleaseReceipt,
  type HistoricalReleasePolicy,
} from '../evidence/release-receipt.js';

export const CONTROL_SCHEMA_VERSION = 27;

const RepositoryOwner = z.string()
  .min(1)
  .max(39)
  .regex(/^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$/);
const RepositoryName = z.string()
  .min(1)
  .max(100)
  .regex(/^[a-z0-9_.-]+$/)
  .refine((value) => value !== '.' && value !== '..');
export const CanonicalRepository = z.string().superRefine((value, context) => {
  const [owner, name, ...rest] = value.split('/');
  if (
    rest.length !== 0
    || !RepositoryOwner.safeParse(owner).success
    || !RepositoryName.safeParse(name).success
  ) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'repository must be canonical owner/name',
    });
  }
});

export const MonitorBrokerKind = z.enum(['issue', 'pull_request']);
export type MonitorBrokerKind = z.infer<typeof MonitorBrokerKind>;

export const MonitorBrokerCursor = z.object({
  updatedAfter: z.string().datetime({ offset: true }).or(z.literal('')),
}).strict();
export type MonitorBrokerCursor = z.infer<typeof MonitorBrokerCursor>;

export interface MonitorBrokerRequest {
  id: string;
  registrationId: string;
  registrationVersion: number;
  repository: string;
  monitorKind: MonitorBrokerKind;
  cursor: MonitorBrokerCursor;
  leaseToken: string;
}

export const MonitorBrokerResponse = z.object({
  items: z.array(z.object({
    repository: CanonicalRepository,
    kind: MonitorBrokerKind,
    number: z.number().int().positive().max(2_147_483_647),
    updatedAt: z.string().datetime({ offset: true }),
  }).strict()).max(1_000),
  nextCursor: MonitorBrokerCursor,
  observedAt: z.string().datetime({ offset: true }),
}).strict();
export type MonitorBrokerResponse = z.infer<typeof MonitorBrokerResponse>;

const RegistrationGateTimeoutSecondsContract = z.object({
  default: z.number().int().min(60).max(2_592_000).optional(),
  planning: z.number().int().min(60).max(2_592_000).optional(),
  design: z.number().int().min(60).max(2_592_000).optional(),
  'repository-graders': z.number().int().min(60).max(2_592_000).optional(),
  review: z.number().int().min(60).max(2_592_000).optional(),
  merge: z.number().int().min(60).max(2_592_000).optional(),
  'lease-recovery': z.number().int().min(60).max(2_592_000).optional(),
}).strict();

/** Canonical create/patch configuration for every new Registration version. */
export const RepositoryRegistrationConfigurationContract = z.object({
  mergeMethod: z.enum(['squash', 'merge', 'rebase']).optional(),
  releaseEvidence: ReleasePolicyContract.optional(),
  gateTimeoutSeconds: RegistrationGateTimeoutSecondsContract.optional(),
}).strict();
export type RepositoryRegistrationConfiguration = z.infer<
  typeof RepositoryRegistrationConfigurationContract
>;

/** Decode-only configuration for immutable versions with historical policy rules. */
export const HistoricalRepositoryRegistrationConfigurationContract = z.object({
  mergeMethod: z.enum(['squash', 'merge', 'rebase']).optional(),
  releaseEvidence: HistoricalReleasePolicyContract.optional(),
  gateTimeoutSeconds: RegistrationGateTimeoutSecondsContract.optional(),
}).strict();
export type HistoricalRepositoryRegistrationConfiguration = z.infer<
  typeof HistoricalRepositoryRegistrationConfigurationContract
>;

export const RepositoryRegistrationInput = z.object({
  repository: z.string().trim().toLowerCase()
    .pipe(CanonicalRepository),
  enabled: z.boolean().default(true),
  issueMonitorEnabled: z.boolean().default(true),
  prMonitorEnabled: z.boolean().default(true),
  executionEnabled: z.boolean().default(true),
  configuration: RepositoryRegistrationConfigurationContract.default({}),
});
export type RepositoryRegistrationInput = z.infer<typeof RepositoryRegistrationInput>;
// Build the patch schema from the same leaf validators without carrying the
// create-time defaults into partial updates. Zod 4 applies defaults inside
// `.partial()`, which would otherwise turn `{ enabled: undefined }` into a
// full default-valued update and silently re-enable a registration.
export const RepositoryRegistrationPatch = z.object({
  enabled: RepositoryRegistrationInput.shape.enabled.removeDefault().optional(),
  issueMonitorEnabled: RepositoryRegistrationInput.shape.issueMonitorEnabled
    .removeDefault().optional(),
  prMonitorEnabled: RepositoryRegistrationInput.shape.prMonitorEnabled
    .removeDefault().optional(),
  executionEnabled: RepositoryRegistrationInput.shape.executionEnabled
    .removeDefault().optional(),
  configuration: RepositoryRegistrationInput.shape.configuration
    .removeDefault().optional(),
}).refine(
  (patch) => Object.values(patch).some((value) => value !== undefined),
  'registration patch is empty',
);
export type RepositoryRegistrationPatch = z.infer<typeof RepositoryRegistrationPatch>;

export type RepositoryRegistration = Omit<RepositoryRegistrationInput, 'configuration'> & {
  /** Persisted configuration; new create/patch writers remain canonical. */
  configuration: HistoricalRepositoryRegistrationConfiguration;
  id: string;
  version: number;
  createdAt: string;
  updatedAt: string;
};

export const JobSource = z.object({
  kind: z.enum(['webhook', 'poll', 'manual', 'recovery']),
  key: z.string().min(1),
}).strict();
export type JobSource = z.infer<typeof JobSource>;

export const ArtifactReferenceContract = z.object({
  uri: z.string().regex(
    /^volume:\/\/registrations\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\/[A-Za-z0-9._/-]+$/,
    'artifact URI must be registration-scoped runner-volume URI',
  ).refine(
    (uri) => !uri.split('/').some((segment) => segment === '.' || segment === '..'),
    'artifact URI must not contain dot path segments',
  ),
  sha256: z.string().regex(/^[0-9a-f]{64}$/),
  sizeBytes: z.number().int().nonnegative(),
  createdAt: z.string().datetime(),
}).strict();
export type ArtifactReference = z.infer<typeof ArtifactReferenceContract>;

export const RunnerRepositoryIdentity = z.object({
  owner: RepositoryOwner,
  name: RepositoryName,
}).strict();
export type RunnerRepositoryIdentity = z.infer<typeof RunnerRepositoryIdentity>;

const ReleaseSourceIssueCommentsContract = z.array(z.object({
  id: z.number().int().positive().max(Number.MAX_SAFE_INTEGER),
  body: z.string().max(100_000),
  updatedAt: z.string().datetime({ offset: true }),
  url: z.string().url().max(2_000),
  author: z.string().min(1).max(128),
}).strict()).max(1_000).superRefine((comments, context) => {
  if (comments.reduce((bytes, comment) => bytes + Buffer.byteLength(comment.body), 0) > 500_000) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'Source Issue authoritative comments exceed 500000 bytes',
    });
  }
  for (let index = 1; index < comments.length; index += 1) {
    if (comments[index - 1]!.id >= comments[index]!.id) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: [index, 'id'],
        message: 'Source Issue authoritative comments must have unique ascending IDs',
      });
    }
  }
});

const ReleaseSourceIssueSnapshotCoreContract = z.object({
  repository: CanonicalRepository,
  number: z.number().int().positive().max(2_147_483_647),
  title: z.string().max(4_096),
  body: z.string().max(1_000_000),
  url: z.string().url(),
  labels: z.array(z.string().max(100)).max(100),
  comments: ReleaseSourceIssueCommentsContract,
  state: z.enum(['open', 'closed']),
  sourceUpdatedAt: z.string().datetime({ offset: true }),
  capturedAt: z.string().datetime({ offset: true }),
}).strict();
export type ReleaseSourceIssueSnapshotCore = z.infer<
  typeof ReleaseSourceIssueSnapshotCoreContract
>;

/** Digest the exact requirements-bearing Source Issue bytes frozen at ready promotion. */
export function releaseSourceIssueSnapshotDigest(
  input: ReleaseSourceIssueSnapshotCore,
): string {
  const source = ReleaseSourceIssueSnapshotCoreContract.parse(input);
  // Capture metadata is intentionally outside the requirements identity. A
  // retry may observe a new capture time (or runner-owned label changes) while
  // the human-attested requirements are byte-for-byte identical. Conversely,
  // any title/body/URL change produces a new digest and cannot silently reuse
  // the authority bound to an open release.
  const fields = [
    source.repository,
    String(source.number),
    source.title,
    source.body,
    source.url,
    ...source.comments.flatMap((comment) => [
      String(comment.id),
      comment.body,
      comment.updatedAt,
      comment.url,
      comment.author,
    ]),
  ];
  const canonical = fields.map((value) => (
    `${Buffer.byteLength(value, 'utf8')}:${value}`
  )).join('|');
  return createHash('sha256').update(canonical).digest('hex');
}

export const ReleaseSourceIssueSnapshotContract =
  ReleaseSourceIssueSnapshotCoreContract.extend({
    digest: z.string().regex(/^[0-9a-f]{64}$/),
  }).strict().superRefine((snapshot, context) => {
    const { digest, ...source } = snapshot;
    if (releaseSourceIssueSnapshotDigest(source) !== digest) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['digest'],
        message: 'Source Issue digest does not match the frozen snapshot',
      });
    }
  });
export type ReleaseSourceIssueSnapshot = z.infer<
  typeof ReleaseSourceIssueSnapshotContract
>;

export const TriageJobPayloadV1Contract = z.object({
  schemaVersion: z.literal(1),
  repository: RunnerRepositoryIdentity,
  issue: z.object({
    number: z.number().int().positive().max(2_147_483_647),
    observedUpdatedAt: z.string().datetime({ offset: true }),
  }).strict(),
}).strict();
export type TriageJobPayloadV1 = z.infer<typeof TriageJobPayloadV1Contract>;

export const TriageDecisionV1Contract = z.object({
  schemaVersion: z.literal(1),
  type: z.enum(['feature', 'bug', 'tech_debt', 'question', 'documentation']),
  northStarAlignment: z.enum(['aligned', 'unclear', 'misaligned']),
  readiness: z.enum(['ready_candidate', 'blocked', 'needs_info']),
  priority: z.enum(['p0', 'p1', 'p2', 'p3']),
  summary: z.string().trim().min(1).max(500),
  rationale: z.array(z.string().trim().min(1).max(500)).min(1).max(8),
  dependencies: z.array(z.object({
    repository: z.string().trim().toLowerCase()
      .pipe(CanonicalRepository),
    issueNumber: z.number().int().positive().max(2_147_483_647),
    relationship: z.enum(['blocks', 'blocked_by', 'relates_to']),
  }).strict()).max(16),
  duplicateCandidates: z.array(z.object({
    repository: z.string().trim().toLowerCase()
      .pipe(CanonicalRepository),
    issueNumber: z.number().int().positive().max(2_147_483_647),
    reason: z.string().trim().min(1).max(500),
  }).strict()).max(16),
  missingInformation: z.array(
    z.string().trim().min(1).max(500),
  ).max(16),
}).strict();
export type TriageDecisionV1 = z.infer<typeof TriageDecisionV1Contract>;

export const TriageProviderProvenanceContract = z.object({
  attemptId: z.string().uuid(),
  provider: z.string().min(1).max(128),
  model: ProviderModelSelectionContract,
  consumer: ReleaseRuntimeConsumerContract,
  environment: ReleaseRuntimeEnvironmentContract,
}).strict();
export type TriageProviderProvenance = z.infer<
  typeof TriageProviderProvenanceContract
>;

export const TriageJobResultV1Contract = z.object({
  schemaVersion: z.literal(1),
  status: z.literal('succeeded'),
  jobId: z.string().uuid(),
  attemptNumber: z.number().int().positive(),
  repository: CanonicalRepository,
  issueNumber: z.number().int().positive().max(2_147_483_647),
  outcome: z.enum(['triaged', 'unchanged', 'promoted', 'skipped']),
  sourceDigest: z.string().regex(/^[0-9a-f]{64}$/).nullable(),
  decision: TriageDecisionV1Contract.nullable(),
  commentUrl: z.string().url().nullable(),
  appliedLabels: z.array(
    z.string().trim().min(1).max(50).regex(
      /^[a-zA-Z0-9][a-zA-Z0-9._:/ -]*$/,
    ),
  ).max(3),
  promotedJobId: z.string().uuid().nullable(),
  providerProvenance: TriageProviderProvenanceContract.nullable().default(null),
  completedAt: z.string().datetime(),
}).strict();
export type TriageJobResultV1 = z.infer<typeof TriageJobResultV1Contract>;

export const RunnerEventContract = z.discriminatedUnion('kind', [
  z.object({
    kind: z.literal('issue'),
    number: z.number().int().positive(),
    action: z.enum(['opened', 'labeled', 'reopened', 'synchronize', 'recovery']),
  }).strict(),
  z.object({
    kind: z.literal('pull_request'),
    number: z.number().int().positive(),
    action: z.enum([
      'opened',
      'synchronize',
      'review_requested',
      'check_completed',
      'recovery',
    ]),
  }).strict(),
  z.object({
    kind: z.literal('repository'),
    trigger: z.enum(['push', 'check_run', 'check_suite']),
    identity: z.string().min(1).max(512).regex(/^\S(?:[\s\S]*\S)?$/),
    ref: z.string().max(255).optional(),
    after: z.string().regex(/^[0-9a-f]{40,64}$/).optional(),
  }).strict(),
]);
export type RunnerEvent = z.infer<typeof RunnerEventContract>;

const GitRef = z.string()
  .min(1)
  .max(255)
  .refine(
    (value) =>
      !value.startsWith('-')
      && !value.includes('..')
      && !value.includes('@{')
      && !/[~^:?*\\\s]/.test(value)
      && !value.endsWith('/')
      && !value.endsWith('.lock'),
    'unsafe git ref',
  );
export const GitHubLabelNameContract = z.string()
  .trim()
  .min(1)
  .max(50)
  .regex(/^[a-zA-Z0-9][a-zA-Z0-9._:/ -]*$/);

/**
 * The only job payload executable by agentops-runner. It deliberately contains
 * repository/event/ref identities, never a shell command, host path, clone URL,
 * credential, or arbitrary process environment.
 */
export const RunnerJobPayloadV1Contract = z.object({
  schemaVersion: z.literal(1),
  repository: RunnerRepositoryIdentity,
  event: RunnerEventContract,
  target: z.object({
    baseRef: GitRef,
    headRef: GitRef.optional(),
  }).strict(),
  execution: z.object({
    mode: z.enum(['development_turn', 'pr_reconciliation']),
    requiredChecks: z.array(
      z.string().min(1).regex(/^\S(?:[\s\S]*\S)?$/),
    ).max(64),
    mergeMethod: z.enum(['squash', 'merge', 'rebase']),
    readyLabel: GitHubLabelNameContract,
    claimedLabel: GitHubLabelNameContract,
  }).strict(),
  artifacts: z.array(ArtifactReferenceContract).max(64),
  /** Immutable ready-time requirements snapshot; present on promoted Issue jobs. */
  sourceIssue: ReleaseSourceIssueSnapshotContract.optional(),
  /**
   * Runner-resolved durable child lineage. It is injected from the control DB
   * after claim; no Issue-authored bytes can select a parent commit or base.
   */
  lineage: z.object({
    nodeId: z.string().uuid(),
    parentNodeId: z.string().uuid(),
    parentIssueNumber: z.number().int().positive(),
    parentPullRequestNumber: z.number().int().positive(),
    parentBranch: GitRef,
    parentHeadSha: z.string().regex(/^[0-9a-f]{40}([0-9a-f]{24})?$/),
    reviewRound: z.number().int().positive().max(1_000),
  }).strict().optional(),
}).strict().superRefine((payload, context) => {
  const expectedMode = payload.event.kind === 'issue'
    ? 'development_turn'
    : 'pr_reconciliation';
  if (payload.execution.mode !== expectedMode) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['execution', 'mode'],
      message: `${payload.event.kind} event requires ${expectedMode}`,
    });
  }
  if (
    payload.sourceIssue
    && (
      payload.event.kind !== 'issue'
      || payload.sourceIssue.repository
        !== `${payload.repository.owner}/${payload.repository.name}`
      || payload.sourceIssue.number !== payload.event.number
    )
  ) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['sourceIssue'],
      message: 'Source Issue snapshot must match the promoted Issue event',
    });
  }
  if (payload.lineage && payload.event.kind !== 'issue') {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['lineage'],
      message: 'only a child Issue development turn may carry review lineage',
    });
  }
});
export type RunnerJobPayloadV1 = z.infer<typeof RunnerJobPayloadV1Contract>;

export const RunnerHumanReviewV1Contract = z.object({
  issueNumber: z.number().int().positive().max(2_147_483_647),
  reasonCount: z.number().int().positive(),
  commentUrl: z.string().url(),
  classification: z.literal('what-judgment'),
  howIntervention: z.literal(false),
  aiAppliedReadyLabel: z.literal(false),
  claimedLabelRemoved: z.literal(true),
}).strict();
export type RunnerHumanReviewV1 = z.infer<
  typeof RunnerHumanReviewV1Contract
>;

export const RunnerJobResultV1Contract = z.object({
  schemaVersion: z.literal(1),
  status: z.literal('succeeded'),
  jobId: z.string().uuid(),
  attemptNumber: z.number().int().positive(),
  repository: CanonicalRepository,
  outcome: z.enum(['completed', 'needs-human-review']).default('completed'),
  humanReview: RunnerHumanReviewV1Contract.nullable().default(null),
  headSha: z.string().regex(/^[0-9a-f]{40,64}$/).nullable(),
  pullRequestNumber: z.number().int().positive().nullable(),
  artifacts: z.array(ArtifactReferenceContract),
  completedAt: z.string().datetime(),
}).strict().superRefine((result, context) => {
  if (result.outcome === 'needs-human-review') {
    if (result.humanReview === null) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['humanReview'],
        message: 'needs-human-review outcome requires human-review evidence',
      });
    }
    if (result.headSha !== null || result.pullRequestNumber !== null) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['headSha'],
        message: 'needs-human-review outcome cannot claim a PR revision',
      });
    }
  } else if (result.humanReview !== null) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['humanReview'],
      message: 'completed outcome cannot carry human-review evidence',
    });
  }
});
export type RunnerJobResultV1 = z.infer<typeof RunnerJobResultV1Contract>;

export const RunnerFailureCode = z.enum([
  'unknown_job_contract',
  'registration_stale',
  'lease_lost',
  'artifact_integrity',
  'workspace_failure',
  'provider_failure',
  'push_failure',
  'required_checks_failure',
  'merge_failure',
  'release_failure',
  'startup_isolation_failure',
  'internal_failure',
]);
export type RunnerFailureCode = z.infer<typeof RunnerFailureCode>;

export const RunnerCriticalBoundary = z.enum([
  'claim',
  'provider',
  'push',
  'merge',
  'release',
]);
export type RunnerCriticalBoundary = z.infer<typeof RunnerCriticalBoundary>;

export const RunnerJobFailureV1Contract = z.object({
  schemaVersion: z.literal(1),
  status: z.literal('failed'),
  code: RunnerFailureCode,
  message: z.string().min(1).max(2_000).regex(/^\S(?:[\s\S]*\S)?$/),
  retryable: z.boolean(),
  boundary: RunnerCriticalBoundary.nullable(),
  observedAt: z.string().datetime(),
}).strict();
export type RunnerJobFailureV1 = z.infer<typeof RunnerJobFailureV1Contract>;

export const EnqueueJobInput = z.object({
  registrationId: z.string().uuid(),
  registrationVersion: z.number().int().positive(),
  source: JobSource,
  idempotencyKey: z.string().trim().min(1),
  jobType: z.string().trim().min(1),
  payload: z.record(z.string(), z.unknown()),
  availableAt: z.date().optional(),
}).superRefine((input, context) => {
  const contract = input.jobType === 'agentops.runner'
    ? RunnerJobPayloadV1Contract
    : input.jobType === 'agentops.triage'
      ? TriageJobPayloadV1Contract
      : null;
  if (!contract) return;
  const parsed = contract.safeParse(input.payload);
  if (!parsed.success) {
    for (const issue of parsed.error.issues) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['payload', ...issue.path],
        message: issue.message,
      });
    }
  }
});
export type EnqueueJobInput = z.infer<typeof EnqueueJobInput>;

export interface JobEnvelope {
  contractVersion: 1;
  id: string;
  registrationId: string;
  registrationVersion: number;
  source: JobSource;
  idempotencyKey: string;
  jobType: string;
  payload: Record<string, unknown>;
  status: 'queued' | 'leased' | 'succeeded' | 'failed' | 'cancelled' | 'rejected';
  createdAt: string;
  /** Durable release identity; optional only for legacy/non-release jobs. */
  releaseId?: string;
}

export const JobEnvelopeContract = z.object({
  contractVersion: z.literal(1),
  id: z.string().uuid(),
  registrationId: z.string().uuid(),
  registrationVersion: z.number().int().positive(),
  source: JobSource,
  idempotencyKey: z.string().min(1),
  jobType: z.string().min(1),
  payload: z.record(z.string(), z.unknown()),
  status: z.enum(['queued', 'leased', 'succeeded', 'failed', 'cancelled', 'rejected']),
  createdAt: z.string().datetime(),
  releaseId: z.string().uuid().optional(),
}).strict();

export interface EnqueueResult {
  job: JobEnvelope;
  duplicate: boolean;
}

export interface Lease {
  id: string;
  token: string;
  workerId: string;
  attemptId: string;
  attemptNumber: number;
  expiresAt: string;
  job: JobEnvelope;
}

export interface ExecutionGuardVerdict {
  ok: boolean;
  reason: string | null;
  registration: RepositoryRegistration | null;
  jobId: string | null;
  leaseExpiresAt: string | null;
}

export interface ReconciliationWork {
  jobId: string;
  registrationId: string;
  repository: string;
  status: 'queued' | 'leased';
  availableAt: string;
  leaseExpiresAt: string | null;
}

export interface WebhookClaim {
  deliveryId: string;
  deliveryKey: string;
  repository: string;
  event: string;
  action: string | null;
  headers: Record<string, string>;
  payload: Record<string, unknown>;
  token: string;
  expiresAt: string;
  receivedAt: string;
}

export interface ReleaseRecord {
  id: string;
  registrationId: string;
  releaseKey: string;
  repository: string;
  issueNumber: number;
  /** Persisted policy; historical v2/v3 grader aliases remain decode-only readable. */
  policy: HistoricalReleasePolicy;
  status: 'collecting' | 'merge-authorized' | 'merged' | 'abandoned';
  pullRequest: number | null;
  finalHead: string | null;
  mergeSha: string | null;
  mergeActor: string | null;
  createdAt: string;
  updatedAt: string;
  completedAt: string | null;
}

export interface ReleaseReceiptOutboxEntry {
  /** Persisted v2/v3 receipt; new writes remain restricted to the canonical contract. */
  receipt: HistoricalDurableReleaseReceipt;
  publishedAt: string | null;
}

export class ControlStoreUnavailableError extends Error {
  override readonly name = 'ControlStoreUnavailableError';
}

export class ControlSchemaError extends Error {
  override readonly name = 'ControlSchemaError';
}

export class StaleRegistrationError extends Error {
  override readonly name = 'StaleRegistrationError';
}

export class RepositoryBusyError extends Error {
  override readonly name = 'RepositoryBusyError';
}

export class OperatingModeError extends Error {
  override readonly name = 'OperatingModeError';
}

export class IdempotencyConflictError extends Error {
  override readonly name = 'IdempotencyConflictError';
}

export class ReleaseReceiptConflictError extends Error {
  override readonly name = 'ReleaseReceiptConflictError';
}

export class ReleaseCertificationError extends Error {
  override readonly name = 'ReleaseCertificationError';
}

export class LeaseRejectedError extends Error {
  override readonly name = 'LeaseRejectedError';
}

export class RunnerContractError extends Error {
  override readonly name = 'RunnerContractError';
}
