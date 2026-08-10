/**
 * Machine-readable contracts.
 *
 * These zod schemas ARE the harness's contract language. Everything that crosses
 * an agent boundary — an Issue Contract handed to a Generator, a Scorecard handed
 * back by an Evaluator, a Repair brief — is validated against a schema here. That
 * is the central design bet from the spec: turn "planning / review / QA" into
 * *validatable artifacts* rather than prose, so the loop can be automated, resumed
 * and measured.
 */

import { createHash } from 'node:crypto';
import { z } from 'zod';
import { ISSUE_STATUSES } from './states.js';
import { VERIFICATION_METHODS } from '../authoring/lint.js';
import {
  AgentProvider,
  InvocationOutcome,
  InvocationRole,
  Verdict,
} from './agent-runtime.js';
import {
  NullableRevisionCoordinates,
  PersistedPRDecoder,
  PrHeadSha,
  PrRevision,
  RevisionGateSnapshot,
} from './pr-schema.js';
import { FindingLineageRef } from './finding-lineage.js';
import { HARD_GATE_SIGNAL_NAMES } from '../graders/gate-names.js';
export * from './agent-runtime.js';
export * from './pr-schema.js';
export * from './revision-gate.js';
export * from './pr-lifecycle.js';
export * from './finding-lineage.js';

// --- small vocabularies ----------------------------------------------------

export const Severity = z.enum(['blocker', 'major', 'minor']);
export type Severity = z.infer<typeof Severity>;

/** How an acceptance criterion is checked. Maps onto a grader (see graders/). */
export const VerificationMethod = z.enum(VERIFICATION_METHODS);
export type VerificationMethod = z.infer<typeof VerificationMethod>;

export const IssueType = z.enum([
  'epic',
  'feature',
  'story',
  'bug',
  'tech-debt',
  'harness', // improvements to this harness itself
  'eval', // improvements to the eval dataset / graders
]);
export type IssueType = z.infer<typeof IssueType>;

export const Area = z.enum(['frontend', 'backend', 'fullstack', 'infra', 'docs', 'eval', 'harness']);
export type Area = z.infer<typeof Area>;

export const IssueStatus = z.enum(ISSUE_STATUSES);

export const GateResult = z.enum(['pass', 'fail', 'skip']);
export type GateResult = z.infer<typeof GateResult>;

export { HARD_GATE_SIGNAL_NAMES } from '../graders/gate-names.js';
export const HardGateSignalName = z.enum(HARD_GATE_SIGNAL_NAMES);
export type HardGateSignalName = z.infer<typeof HardGateSignalName>;
export const HardGateResults = z.partialRecord(HardGateSignalName, GateResult);
export type HardGateResults = z.infer<typeof HardGateResults>;

// --- the Issue Contract ----------------------------------------------------

export const AcceptanceCriterion = z.object({
  id: z.string(), // e.g. AC-001
  severity: Severity,
  behavior: z.string(),
  verification: z.object({
    method: VerificationMethod,
    expected: z.array(z.string()).min(1),
  }),
});
export type AcceptanceCriterion = z.infer<typeof AcceptanceCriterion>;

/** Workflow-owned API contract delta; Designflow capabilities never choose this shape. */
export const WorkflowApiOperation = z.object({
  operationId: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  method: z.enum(['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']),
  path: z.string().regex(/^\/[^\s]*$/),
  purpose: z.string().min(1),
}).strict();
export type WorkflowApiOperation = z.infer<typeof WorkflowApiOperation>;

export const IssueContract = z.object({
  productGoal: z.string(),
  userStory: z.string(),
  scope: z.object({
    include: z.array(z.string()).default([]),
    exclude: z.array(z.string()).default([]),
  }),
  acceptanceCriteria: z.array(AcceptanceCriterion).min(1),
  redLines: z.array(z.string()).default([]),
  /** Present only when the workflow planner reconciles approved backend capabilities. */
  apiOperations: z.array(WorkflowApiOperation).optional(),
});
export type IssueContract = z.infer<typeof IssueContract>;

/** Dedicated UI-authoring artifact; HOW design is explicit and traceable to the accepted WHAT. */
export const UiDesignToken = z.object({
  id: z.string().min(1),
  category: z.enum(['color', 'typography', 'spacing', 'radius', 'shadow', 'motion', 'other']),
  value: z.string().min(1),
  rationale: z.string().min(1),
  sourceCriterionIds: z.array(z.string().min(1)).min(1),
});
export type UiDesignToken = z.infer<typeof UiDesignToken>;

export const UiDesignComponent = z.object({
  id: z.string().min(1),
  name: z.string().min(1),
  purpose: z.string().min(1),
  states: z.array(z.string().min(1)).min(1),
  interactions: z.array(z.string().min(1)).default([]),
  accessibility: z.array(z.string().min(1)).min(1),
  sourceCriterionIds: z.array(z.string().min(1)).min(1),
});
export type UiDesignComponent = z.infer<typeof UiDesignComponent>;

export const UiDesignArtifact = z.object({
  candidateKey: z.string().min(1),
  principles: z.array(z.string().min(1)).min(1),
  tokens: z.array(UiDesignToken).min(1),
  components: z.array(UiDesignComponent).min(1),
  criterionTraces: z.array(z.object({
    criterionId: z.string().min(1),
    designElementIds: z.array(z.string().min(1)).min(1),
  })).min(1),
});
export type UiDesignArtifact = z.infer<typeof UiDesignArtifact>;

export const UiDesignOutput = z.object({
  artifact: UiDesignArtifact.nullable(),
  ambiguities: z.array(z.string().min(1)).default([]),
});
export type UiDesignOutput = z.infer<typeof UiDesignOutput>;

/** Exactly one Experience Contract authority is selected for a UI candidate/revision. */
export const DesignContractProvider = z.enum(['legacy-ui-design', 'designflow']);
export type DesignContractProvider = z.infer<typeof DesignContractProvider>;

export const DesignProviderSelection = z.object({
  candidateKey: z.string().min(1),
  provider: DesignContractProvider,
}).strict();
export type DesignProviderSelection = z.infer<typeof DesignProviderSelection>;

const ContentDigest = z.string().regex(/^sha256:[a-f0-9]{64}$/);

/**
 * Durable, prompt-safe identity of the one authoritative design revision. The external
 * Design Bundle itself remains provider-owned and is never copied into the workflow store.
 */
export const DesignAuthority = z.discriminatedUnion('provider', [
  z.object({
    provider: z.literal('legacy-ui-design'),
    candidateKey: z.string().min(1),
    revisionId: z.string().min(1),
    artifactDigest: ContentDigest,
    invocationKey: z.string().min(1),
  }).strict(),
  z.object({
    provider: z.literal('designflow'),
    providerRef: z.string().min(1),
    candidateKey: z.string().min(1),
    requestId: z.string().min(1),
    revisionId: z.string().min(1),
    bundleDigest: ContentDigest,
    decisionId: z.string().min(1),
  }).strict(),
]);
export type DesignAuthority = z.infer<typeof DesignAuthority>;

const ReviewProjectionRecord = z.record(z.string(), z.unknown());

/**
 * Workflow-owned, deterministic subset of WF-DF-005's review projection. Nested records retain
 * the complete validated projection rather than a provider-authored prompt string.
 */
export const ApprovedDesignReviewProjection = z.object({
  identity: z.object({
    schemaVersion: z.string().min(1),
    bundleId: z.string().min(1),
    requestId: z.string().min(1),
    revisionId: z.string().min(1),
    previousRevisionId: z.string().min(1).nullable(),
    createdAt: z.string().min(1),
  }).strict(),
  purposes: z.array(ReviewProjectionRecord).min(1),
  tasks: z.array(ReviewProjectionRecord).min(1),
  effortBudgets: z.array(ReviewProjectionRecord).min(1),
  attentionHierarchy: z.array(ReviewProjectionRecord).min(1),
  elements: z.array(ReviewProjectionRecord).min(1),
  designSystemDelta: ReviewProjectionRecord,
  capabilityDelta: z.array(ReviewProjectionRecord),
  revisionDiff: z.string().min(1),
  ambiguities: z.array(z.object({
    source: z.enum(['experience', 'capability']),
    text: z.string().min(1),
  }).strict()),
  digest: z.object({
    sourceDigest: ContentDigest,
    bundleDigest: ContentDigest,
    artifacts: z.array(ReviewProjectionRecord).min(1),
  }).strict(),
}).strict();
export type ApprovedDesignReviewProjection = z.infer<typeof ApprovedDesignReviewProjection>;

function canonicalJson(value: unknown): string {
  if (value === null || typeof value !== 'object') return JSON.stringify(value) ?? 'null';
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(',')}]`;
  return `{${Object.entries(value as Record<string, unknown>)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, nested]) => `${JSON.stringify(key)}:${canonicalJson(nested)}`)
    .join(',')}}`;
}

/** Stable content identity used to read-migrate an unambiguous legacy UI artifact. */
export function digestLegacyUiDesignArtifact(artifact: UiDesignArtifact): string {
  const parsed = UiDesignArtifact.parse(artifact);
  return `sha256:${createHash('sha256').update(canonicalJson(parsed)).digest('hex')}`;
}

const IssueFields = z.object({
  id: z.string(), // ISSUE-0001
  type: IssueType,
  title: z.string(),
  area: Area,
  epicId: z.string().nullable().default(null),
  // Planning-tree links (DOC_TAXONOMY §2本の木): which feature/spec this issue descends from,
  // so north-star → feature → AC → issue → PR is mechanically traceable.
  featureId: z.string().nullable().default(null), // FEAT-NNN this issue serves (null for non-spec work)
  specPath: z.string().nullable().default(null), // signed spec dir this issue decomposes — the coverage-set key
  sprint: z.string().nullable().default(null), // e.g. 2026-W24
  status: IssueStatus.default('planned'),
  assignedAgent: AgentProvider.nullable().default(null),
  contract: IssueContract.nullable().default(null), // null until contract-drafted
  // Nano decomposition (to-detail-design; replaces the old slice .md — DOC_TAXONOMY §NANO).
  // coversAcIds is the 被覆×排他 unit: every spec AC must be covered by exactly one issue in the set.
  coversAcIds: z.array(z.string()).default([]), // spec AC-IDs this issue satisfies
  // File-scope boundary from to-detail-design (issues.yaml `scope:`): the glob sets the drafted
  // contract's scope_check enforces against CHANGED FILES. null = undeclared → the contract
  // drafts unrestricted (include=[]). Never AC ids — an AC id is not a glob and matches no file.
  scope: z
    .object({
      include: z.array(z.string()).default([]),
      exclude: z.array(z.string()).default([]),
    })
    .nullable()
    .default(null),
  dependsOnSystem: z.array(z.string()).default([]), // system element ids referenced (DOM/DATA/ARCH/…-<CTX>-NNN) — referenced, never copied
  dependsOnIssues: z.array(z.string()).default([]), // predecessor issues, forming the spec's issue DAG
  implementationNotes: z.array(z.string()).default([]), // seam-level HOW hints (optional; internal, not a contract)
  /** GitHub intake origin (FEAT-017). null for spec/adopt/legacy issues. */
  intakeKey: z.string().nullable().default(null),
  planningCandidateKey: z.string().nullable().default(null),
  uiDesign: UiDesignArtifact.nullable().default(null),
  uiDesignInvocationKey: z.string().nullable().default(null),
  /** Approved external Designflow lineage for final UI/fullstack contracts. */
  designRequestId: z.string().nullable().default(null),
  designRevisionId: z.string().nullable().default(null),
  designBundleDigest: z.string().nullable().default(null),
  designCapabilityIds: z.array(z.string()).default([]),
  /** Single-provider revision identity shared verbatim with generator and reviewers. */
  designAuthority: DesignAuthority.nullable().default(null),
  /** WF-DF-005 projection for the exact approved external revision; never provider prompt text. */
  designReview: ApprovedDesignReviewProjection.nullable().default(null),
  /**
   * Decline audit (FEAT-005): why/when a human closed this issue. Set ONLY by the decline
   * organ (pipeline/lifecycle.ts closeIssue — a judgment point, never automated); null on
   * every non-closed issue. Additive — absent on older records.
   */
  closedReason: z.string().nullable().default(null),
  closedAt: z.string().nullable().default(null),
  /**
   * Which Analyst diagnostic rule filed this proposal (FEAT-005). The rule — not the title
   * text, which bakes in moving metric values — is the dedup identity: at most one OPEN
   * proposal per rule, while a terminal (closed/released) one never suppresses re-filing.
   * null = not rule-filed (spec-spawned or hand-created). Additive.
   */
  sourceRuleId: z.string().nullable().default(null),
  createdAt: z.string(),
  updatedAt: z.string(),
});
const IssueDecoder = z.preprocess((candidate) => {
  if (candidate === null || typeof candidate !== 'object' || Array.isArray(candidate)) {
    return candidate;
  }
  const record = candidate as Record<string, unknown>;
  if (record.designAuthority !== null && record.designAuthority !== undefined) {
    return candidate;
  }
  const parsedArtifact = UiDesignArtifact.safeParse(record.uiDesign);
  const invocationKey = record.uiDesignInvocationKey;
  const hasExternalDesign = record.designRequestId != null
    || record.designRevisionId != null
    || record.designBundleDigest != null
    || (Array.isArray(record.designCapabilityIds) && record.designCapabilityIds.length > 0)
    || record.designReview != null;
  if (
    parsedArtifact.success
    && typeof invocationKey === 'string'
    && invocationKey.length > 0
    && !hasExternalDesign
  ) {
    const artifactDigest = digestLegacyUiDesignArtifact(parsedArtifact.data);
    return {
      ...record,
      designAuthority: {
        provider: 'legacy-ui-design',
        candidateKey: parsedArtifact.data.candidateKey,
        revisionId: `legacy-${artifactDigest.slice('sha256:'.length)}`,
        artifactDigest,
        invocationKey,
      },
    };
  }
  return candidate;
}, IssueFields);

export const Issue = IssueDecoder.superRefine((issue, context) => {
  const hasExternalDesign = issue.designRequestId !== null
    || issue.designRevisionId !== null
    || issue.designBundleDigest !== null
    || issue.designCapabilityIds.length > 0;
  if (
    (issue.uiDesign !== null || issue.uiDesignInvocationKey !== null)
    && hasExternalDesign
  ) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'legacy UI design and external Designflow lineage cannot be dual-written',
      path: ['designAuthority'],
    });
  }

  const authority = issue.designAuthority;
  if (authority === null) {
    if (
      issue.uiDesign !== null
      || issue.uiDesignInvocationKey !== null
      || hasExternalDesign
      || issue.designReview !== null
    ) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'partial design state has no authoritative provider revision',
        path: ['designAuthority'],
      });
    }
    return;
  }
  if (
    issue.planningCandidateKey !== null
    && issue.planningCandidateKey !== authority.candidateKey
  ) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'design authority candidateKey must match planningCandidateKey',
      path: ['designAuthority', 'candidateKey'],
    });
  }
  if (authority.provider === 'legacy-ui-design') {
    if (
      issue.uiDesign === null
      || issue.uiDesign.candidateKey !== authority.candidateKey
      || issue.uiDesignInvocationKey !== authority.invocationKey
      || digestLegacyUiDesignArtifact(issue.uiDesign) !== authority.artifactDigest
      || authority.revisionId !== `legacy-${authority.artifactDigest.slice('sha256:'.length)}`
      || hasExternalDesign
      || issue.designReview !== null
    ) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'legacy design authority must exclusively reference its UI artifact and invocation',
        path: ['designAuthority'],
      });
    }
    return;
  }
  if (
    issue.uiDesign !== null
    || issue.uiDesignInvocationKey !== null
    || issue.designRequestId !== authority.requestId
    || issue.designRevisionId !== authority.revisionId
    || issue.designBundleDigest !== authority.bundleDigest
    || issue.designReview === null
  ) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'Designflow authority must exclusively match the approved external revision',
      path: ['designAuthority'],
    });
    return;
  }
  if (
    issue.designReview.identity.requestId !== authority.requestId
    || issue.designReview.identity.revisionId !== authority.revisionId
    || issue.designReview.digest.bundleDigest !== authority.bundleDigest
    || issue.designReview.ambiguities.length > 0
  ) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'Designflow review projection must match the ambiguity-free approved revision',
      path: ['designReview'],
    });
  }
  const projectedCapabilityIds = new Set(
    issue.designReview.capabilityDelta
      .map((capability) => capability.id)
      .filter((capabilityId): capabilityId is string =>
        typeof capabilityId === 'string'),
  );
  if (issue.designCapabilityIds.some((capabilityId) =>
    !projectedCapabilityIds.has(capabilityId))) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'Issue capability lineage must be present in the approved design projection',
      path: ['designCapabilityIds'],
    });
  }
});
export type Issue = z.infer<typeof Issue>;

export const Epic = z.object({
  id: z.string(), // EPIC-01
  title: z.string(),
  theme: z.string(),
  status: z.enum(['planned', 'in-progress', 'done']).default('planned'),
  featureIds: z.array(z.string()).default([]), // ordered features under this epic (bidirectional: Feature.epicId)
  issueIds: z.array(z.string()).default([]),
});
export type Epic = z.infer<typeof Epic>;

export const Roadmap = z.object({
  vision: z.string(),
  principles: z.array(z.string()).default([]),
  epicIds: z.array(z.string()).default([]),
});
export type Roadmap = z.infer<typeof Roadmap>;

export const FeatureStatus = z.enum(['planned', 'specced', 'signed', 'implemented']);
export type FeatureStatus = z.infer<typeof FeatureStatus>;

/**
 * A leaf of the planning tree (DOC_TAXONOMY §2本の木): one signable capability.
 * roadmap-planner emits only the outcome + order — never acceptance criteria (those are
 * authored into the signed spec by to-spec). `specPath` is the 交点 where the planning
 * tree meets the system tree: one Feature becomes exactly one signed spec (AC-PLAN-003/004).
 * Descoping a feature flips `inPlan`, it never deletes a signed spec (AC-PLAN-009).
 */
export const Feature = z.object({
  id: z.string(), // FEAT-NNN
  epicId: z.string().nullable().default(null), // parent epic (bidirectional: Epic.featureIds)
  title: z.string(),
  outcome: z.string(), // the capability/value ("why now") — no acceptance criteria here
  specPath: z.string().nullable().default(null), // signed spec dir once spawned (bidirectional: SpecState.featureId)
  status: FeatureStatus.default('planned'),
  inPlan: z.boolean().default(true), // false = descoped: a flag, never a deletion (AC-PLAN-009)
  createdAt: z.string(),
  updatedAt: z.string(),
});
export type Feature = z.infer<typeof Feature>;

// --- Intake & evaluation ---------------------------------------------------

/** Immutable first-seen projection of one external GitHub Issue (FEAT-016). */
export const GithubIssueSnapshot = z.object({
  repository: z.string().min(1), // owner/name remote identity
  number: z.number().int().positive(),
  externalId: z.string().min(1),
  title: z.string(),
  body: z.string(),
  url: z.string(),
  labels: z.array(z.string()).default([]),
  state: z.enum(['open', 'closed']),
  sourceUpdatedAt: z.string(),
  snapshotAt: z.string(),
});
export type GithubIssueSnapshot = z.infer<typeof GithubIssueSnapshot>;

export const IntakeStatus = z.enum([
  'claim-pending',
  'claimed',
  'planning',
  'design-pending',
  'ready',
  'needs-human-review',
]);
export type IntakeStatus = z.infer<typeof IntakeStatus>;

/** Store-first claim record; the Source Snapshot is immutable after insertion. */
export const IntakeRecord = z.object({
  id: z.string(), // INTAKE-0001
  intakeKey: z.string().min(1),
  provider: z.literal('github'),
  snapshot: GithubIssueSnapshot,
  status: IntakeStatus.default('claim-pending'),
  claimedAt: z.string().nullable().default(null),
  storeIssueIds: z.array(z.string()).default([]),
  /** Split-source aggregation close result (single-child sources use the PR's Closes relation). */
  sourceClosedAt: z.string().nullable().default(null),
  sourceCloseError: z.string().nullable().default(null),
  createdAt: z.string(),
  updatedAt: z.string(),
});
export type IntakeRecord = z.infer<typeof IntakeRecord>;

export const AcceptanceTraceSource = z.discriminatedUnion('kind', [
  z.object({ kind: z.literal('source'), text: z.string().min(1) }),
  z.object({
    kind: z.literal('system'),
    elementId: z.string().regex(/^(?:LANG|DOM|ARCH|DATA)-[a-z0-9]+(?:-[a-z0-9]+)*-\d{3}$/),
  }),
]);
export type AcceptanceTraceSource = z.infer<typeof AcceptanceTraceSource>;

export const AcceptanceTrace = z.object({
  criterionId: z.string().min(1),
  sources: z.array(AcceptanceTraceSource).min(1),
});
export type AcceptanceTrace = z.infer<typeof AcceptanceTrace>;

export const EnrichmentCandidate = z.object({
  candidateKey: z.string().min(1),
  title: z.string().min(1),
  type: IssueType,
  area: Area,
  contract: IssueContract,
  traces: z.array(AcceptanceTrace),
});
export type EnrichmentCandidate = z.infer<typeof EnrichmentCandidate>;

export const CapabilityIssueEdge = z.object({
  candidateKey: z.string().min(1),
  criterionId: z.string().min(1),
}).strict();
export type CapabilityIssueEdge = z.infer<typeof CapabilityIssueEdge>;

/**
 * Planner-owned fulfillment of one provider-authored Capability Requirement.
 * Identity is repeated per binding so a mixed-revision edge fails closed.
 */
export const CapabilityCoverageBinding = z.object({
  capabilityId: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  requestId: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  revisionId: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  bundleDigest: z.string().regex(/^sha256:[a-f0-9]{64}$/),
  issueEdges: z.array(CapabilityIssueEdge).default([]),
  systemElementIds: z.array(
    z.string().regex(/^(?:LANG|DOM|ARCH|DATA)-[a-z0-9]+(?:-[a-z0-9]+)*-\d{3}$/),
  ).default([]),
  apiOperationIds: z.array(
    z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  ).default([]),
}).strict();
export type CapabilityCoverageBinding = z.infer<typeof CapabilityCoverageBinding>;

export const ReconciledPlanningCandidate = z.object({
  candidate: EnrichmentCandidate,
  dependsOnCandidateKeys: z.array(z.string().min(1)).default([]),
}).strict();
export type ReconciledPlanningCandidate = z.infer<typeof ReconciledPlanningCandidate>;

/** Final workflow planning output produced only after an approved Design Bundle. */
export const CapabilityReconciliation = z.object({
  schemaVersion: z.literal('1.0'),
  requestId: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  revisionId: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  bundleDigest: z.string().regex(/^sha256:[a-f0-9]{64}$/),
  candidates: z.array(ReconciledPlanningCandidate).min(1),
  bindings: z.array(CapabilityCoverageBinding).default([]),
  ambiguities: z.array(z.string().min(1)).default([]),
}).strict();
export type CapabilityReconciliation = z.infer<typeof CapabilityReconciliation>;
export type CapabilityReconciliationInput = z.input<typeof CapabilityReconciliation>;

/** Durable edge after candidate keys have been projected to store Issue ids. */
export const CapabilityCoverageProjection = z.object({
  capabilityId: z.string().min(1),
  requestId: z.string().min(1),
  revisionId: z.string().min(1),
  bundleDigest: z.string().regex(/^sha256:[a-f0-9]{64}$/),
  issueId: z.string().min(1),
  criterionId: z.string().min(1),
  systemElementIds: z.array(z.string().min(1)).min(1),
  apiOperationIds: z.array(z.string().min(1)).min(1),
}).strict();
export type CapabilityCoverageProjection = z.infer<typeof CapabilityCoverageProjection>;

export const DesignflowExternalRef = z.object({
  provider: z.string().min(1),
  externalId: z.string().min(1),
  uri: z.string().min(1).optional(),
  revision: z.string().min(1).optional(),
  digest: z.string().regex(/^sha256:[a-f0-9]{64}$/).optional(),
}).strict();
export type DesignflowExternalRef = z.infer<typeof DesignflowExternalRef>;

export const DesignRequest = z.object({
  schemaVersion: z.literal('1.0'),
  requestId: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
  sourceRef: DesignflowExternalRef,
  productIntent: z.object({
    primaryOutcome: z.string().min(1),
    users: z.array(z.string().min(1)).min(1),
    usageContext: z.string().min(1),
  }).strict(),
  requirements: z.array(z.object({
    id: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
    statement: z.string().min(1),
    priority: Severity,
    sourceRefs: z.array(DesignflowExternalRef).min(1),
  }).strict()).min(1),
  constraints: z.array(z.object({
    id: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
    category: z.enum([
      'product',
      'brand',
      'accessibility',
      'security',
      'legal',
      'technical',
      'operational',
      'other',
    ]),
    statement: z.string().min(1),
  }).strict()).default([]),
  targetSurfaces: z.array(z.enum(['web', 'mobile', 'desktop', 'terminal', 'other'])).min(1),
  contextRefs: z.array(DesignflowExternalRef).default([]),
  existingDesignSystemRef: DesignflowExternalRef.nullable().default(null),
  requestedAt: z.string().min(1),
}).strict();
export type DesignRequest = z.infer<typeof DesignRequest>;

const DesignDraftTraceSource = z.discriminatedUnion('kind', [
  z.object({
    kind: z.literal('source'),
    text: z.string().min(1),
  }).strict(),
  z.object({
    kind: z.literal('system'),
    elementId: z.string().regex(
      /^(?:LANG|DOM|ARCH|DATA)-[a-z0-9]+(?:-[a-z0-9]+)*-\d{3}$/,
    ),
  }).strict(),
]);

/**
 * UI/fullstack planning stops at WHAT-level requirements. It intentionally has no IssueContract;
 * the final contract is supplied only after an approved Design Bundle has been consumed.
 */
export const DesignDraftCandidate = z.object({
  candidateKey: z.string().min(1),
  title: z.string().min(1),
  type: IssueType,
  area: z.enum(['frontend', 'fullstack']),
  productIntent: z.object({
    primaryOutcome: z.string().min(1),
    users: z.array(z.string().min(1)).min(1),
    usageContext: z.string().min(1),
  }).strict(),
  requirements: z.array(z.object({
    id: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
    statement: z.string().min(1),
    priority: Severity,
  }).strict()).min(1),
  constraints: z.array(z.object({
    id: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._:-]*$/),
    category: z.enum([
      'product',
      'brand',
      'accessibility',
      'security',
      'legal',
      'technical',
      'operational',
      'other',
    ]),
    statement: z.string().min(1),
  }).strict()).default([]),
  targetSurfaces: z.array(z.enum(['web', 'mobile', 'desktop', 'terminal', 'other'])).min(1),
  existingDesignSystemRef: DesignflowExternalRef.nullable().default(null),
  traces: z.array(z.object({
    requirementId: z.string().min(1),
    sources: z.array(DesignDraftTraceSource).min(1),
  }).strict()),
}).strict();
export type DesignDraftCandidate = z.infer<typeof DesignDraftCandidate>;

export const PlanningEnrichmentOutput = z.object({
  candidates: z.array(EnrichmentCandidate).default([]),
  designDrafts: z.array(DesignDraftCandidate).default([]),
  ambiguities: z.array(z.string().min(1)).default([]),
}).strict().superRefine((output, context) => {
  if (output.candidates.length + output.designDrafts.length === 0) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'at least one backend candidate or Designflow draft is required',
      path: ['candidates'],
    });
  }
});
export type PlanningEnrichmentOutput = z.infer<typeof PlanningEnrichmentOutput>;

export const PendingPlanningCandidate = z.object({
  candidate: EnrichmentCandidate,
  systemIds: z.array(z.string()).default([]),
}).strict();
export type PendingPlanningCandidate = z.infer<typeof PendingPlanningCandidate>;

export const DesignPlanningDraft = z.object({
  candidate: DesignDraftCandidate,
  designRequest: DesignRequest,
  systemIds: z.array(z.string()).default([]),
}).strict();
export type DesignPlanningDraft = z.infer<typeof DesignPlanningDraft>;

export const LegacyDesignRevision = z.object({
  candidateKey: z.string().min(1),
  revisionId: z.string().min(1),
  artifactDigest: ContentDigest,
  invocationKey: z.string().min(1),
  artifact: UiDesignArtifact,
}).strict();
export type LegacyDesignRevision = z.infer<typeof LegacyDesignRevision>;

/**
 * Append-only observation of a Designflow human gate. A request-changes result is durable
 * review history, not implementation authority; only a later exact approval may finalize Issues.
 */
export const DesignPlanningDecision = z.object({
  candidateKey: z.string().min(1),
  requestId: z.string().min(1).nullable(),
  revisionId: z.string().min(1).nullable(),
  previousRevisionId: z.string().min(1).nullable().default(null),
  bundleDigest: ContentDigest.nullable(),
  decisionId: z.string().min(1).nullable(),
  supersedesDecisionId: z.string().min(1).nullable().default(null),
  outcome: z.enum(['approve', 'request-changes', 'reject', 'invalid']),
  reasonCodes: z.array(z.string().min(1)).default([]),
  observedAt: z.string().min(1),
}).strict();
export type DesignPlanningDecision = z.infer<typeof DesignPlanningDecision>;

export const PlanningEnrichmentRecord = z.object({
  id: z.string(), // ENRICH-0001
  intakeKey: z.string().min(1),
  invocationKey: z.string().nullable().default(null),
  status: z.enum(['accepted', 'awaiting-design', 'needs-human-review']),
  reasons: z.array(z.string()).default([]),
  traces: z.array(
    z.object({
      candidateKey: z.string(),
      criterionId: z.string(),
      sources: z.array(AcceptanceTraceSource),
    }),
  ).default([]),
  issueIds: z.array(z.string()).default([]),
  pendingCandidates: z.array(PendingPlanningCandidate).default([]),
  designDrafts: z.array(DesignPlanningDraft).default([]),
  approvedDesigns: z.array(z.object({
    candidateKey: z.string().min(1),
    provider: z.literal('designflow').default('designflow'),
    providerRef: z.string().min(1).nullable().default(null),
    requestId: z.string().min(1),
    revisionId: z.string().min(1),
    bundleDigest: z.string().regex(/^sha256:[a-f0-9]{64}$/),
    decisionId: z.string().min(1).nullable().default(null),
    reviewProjection: ApprovedDesignReviewProjection.nullable().default(null),
  })).default([]),
  capabilityCoverage: z.array(CapabilityCoverageProjection).default([]),
  designProviderSelections: z.array(DesignProviderSelection).default([]),
  legacyDesigns: z.array(LegacyDesignRevision).default([]),
  designDecisionHistory: z.array(DesignPlanningDecision).default([]),
  uiDesignCandidateKeys: z.array(z.string()).default([]),
  uiDesignInvocationKeys: z.record(z.string(), z.string()).default({}),
  createdAt: z.string(),
});
export type PlanningEnrichmentRecord = z.infer<typeof PlanningEnrichmentRecord>;

/**
 * ISSUE-0009: on a re-review (attempt > 1) the reviewer ATTESTS each finding's lineage —
 * 'persisted' = the same problem survived the repair, 'new' = first seen this review.
 * Optional/additive: absent = legacy record, deliberately indeterminate (never defaulted
 * to either value — the Analyst must not guess).
 */
export const FindingLineage = z.enum(['persisted', 'new']);
export type FindingLineage = z.infer<typeof FindingLineage>;

/**
 * Review scope decision. `in-change` is work required to make the current
 * revision satisfy its accepted contract. `separate-issue` is a coherent,
 * independently testable problem whose implementation would expand that
 * contract; it must be integrated through a child branch before the parent
 * can merge.
 */
export const FindingDisposition = z.enum(['in-change', 'separate-issue']);
export type FindingDisposition = z.infer<typeof FindingDisposition>;

export const Finding = z.object({
  criterionId: z.string(),
  severity: Severity,
  expected: z.string(),
  observed: z.string(),
  reproductionSteps: z.array(z.string()).default([]),
  evidence: z.record(z.string(), z.string()).default({}), // label -> relative path under evidence dir
  requiredFix: z.array(z.string()).default([]),
  lineage: FindingLineage.optional(),
  /** Stable prior-finding reference, required by new persisted review outputs. */
  lineageRef: FindingLineageRef.optional(),
  /** Optional only for historical stored runs; every new reviewer output requires it. */
  disposition: FindingDisposition.optional(),
  /** Required evidence that a separate issue is independently scoped. */
  separationReason: z.string().trim().min(1).max(2_000).optional(),
}).superRefine((finding, context) => {
  if (finding.disposition === 'separate-issue' && !finding.separationReason) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['separationReason'],
      message: 'separate-issue finding requires a separation reason',
    });
  }
  if (finding.disposition !== 'separate-issue' && finding.separationReason) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['separationReason'],
      message: 'only separate-issue findings may carry a separation reason',
    });
  }
});
export type Finding = z.infer<typeof Finding>;

export const Scores = z.object({
  functionality: z.number().min(0).max(1),
  codeQuality: z.number().min(0).max(1),
  testQuality: z.number().min(0).max(1),
  ux: z.number().min(0).max(1),
  accessibility: z.number().min(0).max(1),
});
export type Scores = z.infer<typeof Scores>;

export const Cost = z.object({
  usd: z.number().nonnegative().default(0),
  tokens: z.number().nonnegative().default(0),
  seconds: z.number().nonnegative().default(0),
});
export type Cost = z.infer<typeof Cost>;

/**
 * An EvalRun IS the Scorecard, persisted. One row per (PR, attempt). This is the
 * "Eval Result DB" the spec keeps returning to: re-runnable, comparable, the basis
 * for pass@k / pass^k and every dashboard number.
 */
const EvalRunRecord = z.object({
  id: z.string(), // EVAL-...
  issueId: z.string(),
  prId: z.string(),
  attempt: z.number().int().positive(), // 1-based attempt within this sample
  sampleIndex: z.number().int().nonnegative(), // which independent best-of-N sample
  agent: AgentProvider,
  promptVersion: z.string().default('v0'),
  graderVersion: z.string().default('v0'),
  verdict: Verdict,
  hardGates: HardGateResults.default({}),
  findings: z.array(Finding).default([]),
  scores: Scores,
  overall: z.number().min(0).max(1),
  evidenceDir: z.string().nullable().default(null),
  cost: Cost,
  featureArea: z.string().default('unknown'),
  // Optional human label, used to compute false-pass / false-fail when present.
  humanVerdict: Verdict.nullable().default(null),
  // DATA-execution-001: which review perspective (lens) produced this run when graded by
  // the evaluator panel; null = legacy single composite grade. Additive/optional.
  perspective: z.string().nullable().default(null),
  // DATA-agent-runtime-004: reviewer invocation that produced this perspective verdict.
  // null = legacy, deterministic grader, or pre-provenance run.
  invocationKey: z.string().nullable().default(null),
  createdAt: z.string(),
});
// ADR-0009: null only for legacy pre-PR-revision scorecards. The union keeps
// revisionId and headSha correlated in both the runtime schema and inferred type.
export const EvalRun = EvalRunRecord.and(NullableRevisionCoordinates);
export type EvalRun = z.infer<typeof EvalRun>;

/** A row in the Eval Task Registry (lightweight v0). */
export const EvalTask = z.object({
  id: z.string(), // EVAL-TASK-...
  sourceIssueId: z.string().nullable().default(null),
  featureArea: z.string(),
  userGoal: z.string(),
  steps: z.array(z.string()).default([]),
  expected: z.array(z.string()).default([]),
  graders: z.array(VerificationMethod).default([]),
  severity: Severity.default('blocker'),
  /**
   * Which target repo this task's graders bind to (config.target.repo at curation time).
   * The registry can mix tasks from different targets (sandbox vs self-hosted) and AC ids
   * collide across issues, so the regression executor only runs tasks bound to the CURRENT
   * target. null = legacy/unbound: skipped and reported, never guessed. Additive.
   */
  target: z.string().nullable().default(null),
  /**
   * Grader commands captured at curation time, keyed by VERIFICATION METHOD (e.g.
   * 'unit_test' → the runnable command from config.target.graders). The task carries its
   * own means of execution, so repointing config.target at another repo later cannot
   * orphan it. Only methods with a configured command are captured — never fabricated.
   * null = legacy/uncaptured: runnable only via the config fallback when bound to the
   * current target. Additive.
   */
  graderCommands: z.record(z.string(), z.string()).nullable().default(null),
  /**
   * Retirement audit (FEAT-005): why/when a human retired this task from execution. Set
   * ONLY by pipeline/lifecycle.ts retireEvalTask (a judgment point, never automated).
   * Retirement is a STATE, not an erasure: the record stays (capture history — and
   * regressionCaptureRate — are untouched); only execution and the executed/unverified
   * accounting exclude a retired task. null = active. Additive — absent on older records.
   */
  retiredReason: z.string().nullable().default(null),
  retiredAt: z.string().nullable().default(null),
  createdAt: z.string(),
});
export type EvalTask = z.infer<typeof EvalTask>;

/**
 * One execution of a regression EvalTask against its target's real graders — the second
 * half of the steering star ("never repeat the same failure twice"): captured failures are
 * re-verified, durably (ADR-0001). Kept SEPARATE from EvalRun on purpose: EvalRuns are the
 * per-(PR, attempt) scorecards that pass@k / pass^k count over; regression executions must
 * not inflate those denominators. `unverified` = the task's AC id matched no assertion in
 * the report — surfaced, never treated as a pass (never-silent).
 */
export const RegressionRun = z.object({
  id: z.string(), // REGRUN-...
  taskId: z.string(), // EVAL-TASK-... this executed
  target: z.string(), // repo the graders ran against
  result: z.enum(['pass', 'fail', 'unverified']),
  matchedAssertions: z.number().int().nonnegative().default(0),
  failedNames: z.array(z.string()).default([]),
  createdAt: z.string(),
});
export type RegressionRun = z.infer<typeof RegressionRun>;

/**
 * The exact prompt text issued to a role session, preserved for audit (DATA-execution-006).
 * A Session is volatile by design (DOM-execution-002): its `.agentops/PROMPT.md` is OVERWRITTEN in
 * place on each repair attempt and lives only under the gitignored, wiped `.harness/` worktree — so
 * without this the text of attempt 1 (and how the repair brief reshaped attempt 2) is lost. This is
 * an additive AUDIT projection: it does not change the Session's runtime volatility, it copies the
 * issued prompt into the store (the single inspectable SoT). One row per (issue, sample, attempt,
 * role). `perspective` names the lens for a reviewer prompt (null for the generator); `model` is the
 * resolved `--model` for that session (null = the user's default model); `outcome` is the session's
 * liveness result so a stuck attempt — which produces no EvalRun — still leaves a durable trace.
 */
export const PromptRecord = z.object({
  id: z.string(), // PROMPT-0001
  issueId: z.string(),
  prId: z.string(),
  sampleIndex: z.number().int().nonnegative(),
  attempt: z.number().int().positive(), // 1-based; > 1 carries the repair brief
  role: z.enum(['generator', 'reviewer']).default('generator'),
  perspective: z.string().nullable().default(null), // reviewer lens; null for the generator
  agent: AgentProvider,
  model: z.string().nullable().default(null), // resolved --model; null = user default
  outcome: z.string().nullable().default(null), // completed | stuck | timeout; null = not captured
  prompt: z.string(),
  createdAt: z.string(),
});
export type PromptRecord = z.infer<typeof PromptRecord>;

/**
 * Provider-neutral audit record for one logical role session (FEAT-013). Unlike the legacy
 * generator-only PromptRecord, this records generator, planning and each reviewer perspective
 * through one identity and keeps provider separate from model.
 */
const AgentInvocationRecord = z.object({
  id: z.string(), // INVOKE-0001
  invocationKey: z.string().min(1),
  subjectId: z.string().min(1),
  issueId: z.string().nullable().default(null),
  sampleIndex: z.number().int().nonnegative().nullable().default(null),
  attempt: z.number().int().positive(),
  role: InvocationRole,
  perspective: z.string().nullable().default(null),
  provider: AgentProvider,
  model: z.string().nullable().default(null),
  prompt: z.string(),
  outcome: InvocationOutcome,
  createdAt: z.string(),
});
const UnboundAgentInvocation = AgentInvocationRecord.extend({
  prId: z.string().nullable().default(null),
  revisionId: z.null().default(null),
  headSha: z.null().default(null),
});
const BoundAgentInvocation = AgentInvocationRecord.extend({
  prId: z.string().min(1),
  revisionId: z.string().min(1),
  headSha: PrHeadSha,
});
export const AgentInvocation = z.union([
  UnboundAgentInvocation,
  BoundAgentInvocation,
]);
export type AgentInvocation = z.infer<typeof AgentInvocation>;

/**
 * The concurrency FACTS of one live turn (ISSUE-0020, AC-PAR-003): how many issues the
 * turn drove, the effective cap it ran under, and the peak simultaneous in-flight count
 * OBSERVED at the dispatch seam (not reconstructed from logs — never log-only, ADR-0001).
 * Appended once per completed live turn; metrics surface the LATEST record as the turn
 * instruments, and null-when-absent keeps "unobserved" distinct from "observed 0".
 */
export const TurnRecord = z.object({
  id: z.string(), // TURN-0001
  cap: z.number().int().positive(), // effective cap (resolveConcurrentIssueCap) for the turn
  issuesDriven: z.number().int().nonnegative(), // queued issues the turn dispatched
  peakConcurrency: z.number().int().nonnegative(), // max simultaneous in-flight observed
  createdAt: z.string(),
});
export type TurnRecord = z.infer<typeof TurnRecord>;

// --- human HOW-interventions (autonomy axis, ISSUE-0011) --------------------

/**
 * The attested HOW-involvement vocabulary. The WHAT/HOW boundary from the spec
 * (docs/specs/autonomy-axis-instruments-human-how-intervention-accounting) is baked into
 * the vocabulary itself: human JUDGMENT POINTS (adopt / assign / sign / decide / label)
 * are part of the autonomy definition, not interventions — no kind exists for them, so
 * miscounting a judgment as an intervention is structurally impossible.
 */
export const INTERVENTION_KINDS = [
  'conditional-approval-implementation', // ⑥⑦: human implements a gate/release condition inside a conditional approval
  'workspace-hand-edit', // human edits an agent's worktree or artifact by hand
  'repair-brief-hand-edit', // human authors or augments a repair brief
  'manual-evidence-collection', // ⑤: human collects evidence the harness should have produced
] as const;
export const InterventionKind = z.enum(INTERVENTION_KINDS);
export type InterventionKind = z.infer<typeof InterventionKind>;

/**
 * One attested human HOW-intervention, bound to an issue. Only explicit records are
 * intervention facts — nothing may infer one from other store state (⑦'s lesson: a
 * guessing diagnostician produces false positives/negatives). `createdAt` is the RECORD
 * time: retroactive records on released issues are ordinary facts (AC-INTV-004).
 */
export const Intervention = z.object({
  id: z.string(), // INTV-0001
  issueId: z.string(),
  kind: InterventionKind,
  reason: z.string().min(1),
  createdAt: z.string(),
});
export type Intervention = z.infer<typeof Intervention>;

// --- spec authoring (M20 signing) ------------------------------------------

/**
 * The pinned, tamper-evident record a human signature produces (AC-AUTH-007).
 * This is the *version pin*: the signed commit + blob SHAs make the approval
 * auditable, the per-AC fingerprints let drift be detected at AC granularity.
 */
export const ApprovedSpecRef = z.object({
  /** Commit the signature was taken against (`git rev-parse HEAD`). */
  signedCommitSha: z.string(),
  /** Blob gitSha of the requirement doc (requirements.md; legacy spec.md) at the signed commit. */
  specBlobGitSha: z.string(),
  /** Blob gitSha of acceptance.yaml at the signed commit. */
  acceptanceBlobGitSha: z.string(),
  /** AC-ID -> content fingerprint pinned at signing (see authoring/fingerprint.ts). */
  acFingerprints: z.record(z.string(), z.string()),
  /** Version-pinned system-layer elements referenced (empty on greenfield — not yet seeded). */
  systemRefs: z.array(z.string()).default([]),
  /** AC-IDs this signature covers; status derives from their coverage of the current set. */
  approvedAcIds: z.array(z.string()),
});
export type ApprovedSpecRef = z.infer<typeof ApprovedSpecRef>;

/**
 * The spec state object: the first persistence target for a signature (issues
 * only exist post-decomposition). Identity is the spec dir path. `status` is
 * never stored — it is *derived* from approved.approvedAcIds vs the current AC
 * set (AC-AUTH-008, see authoring/drift.ts deriveStatus).
 */
export const SpecState = z.object({
  path: z.string(), // spec dir, e.g. docs/specs/authoring-layer — the identity
  featureId: z.string().nullable().default(null), // planning-tree feature this spec realizes (bidirectional: Feature.specPath)
  approved: ApprovedSpecRef.nullable().default(null), // null until first signed
  signedAt: z.string().nullable().default(null),
  createdAt: z.string(),
  updatedAt: z.string(),
});
export type SpecState = z.infer<typeof SpecState>;

// --- the whole database ----------------------------------------------------

/**
 * Durable one-store/one-target binding (DATA-workspace-001). The configured target is only
 * a request; this record is the source of truth that prevents one organisation store from
 * ingesting two unrelated planning trees after config.target changes.
 */
export const TargetBinding = z.object({
  targetIdentity: z.string().min(1),
  boundAt: z.string(),
});
export type TargetBinding = z.infer<typeof TargetBinding>;

export const DB = z.object({
  version: z.literal(1).default(1),
  targetBinding: TargetBinding.nullable().default(null),
  counters: z.record(z.string(), z.number()).default({}),
  roadmap: Roadmap.nullable().default(null),
  epics: z.array(Epic).default([]),
  features: z.array(Feature).default([]),
  issues: z.array(Issue).default([]),
  prs: z.array(PersistedPRDecoder).default([]),
  prRevisions: z.array(PrRevision).default([]),
  revisionGateSnapshots: z.array(RevisionGateSnapshot).default([]),
  evalRuns: z.array(EvalRun).default([]),
  evalTasks: z.array(EvalTask).default([]),
  regressionRuns: z.array(RegressionRun).default([]), // ③ regression executions (additive)
  promptRecords: z.array(PromptRecord).default([]), // audit trail of issued prompts (additive)
  agentInvocations: z.array(AgentInvocation).default([]), // provider-neutral invocation provenance (additive)
  intakeRecords: z.array(IntakeRecord).default([]), // external Source Issue claims (additive)
  planningEnrichments: z.array(PlanningEnrichmentRecord).default([]), // trace-gated planning decisions
  interventions: z.array(Intervention).default([]), // attested human HOW-interventions (additive)
  turnRecords: z.array(TurnRecord).default([]), // per-live-turn concurrency facts (additive)
  specStates: z.array(SpecState).default([]),
});
export type DB = z.infer<typeof DB>;

export function emptyDB(): DB {
  return DB.parse({});
}
