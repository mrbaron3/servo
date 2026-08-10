import { z } from 'zod';
import {
  FindingDisposition,
  FindingLineage,
  FindingLineageRef,
  Severity,
  Verdict,
  type EvalRun,
  type Finding,
} from './schema.js';

export const DevelopmentReviewFinding = z.object({
  criterionId: z.string().trim().min(1).max(200),
  severity: Severity,
  expected: z.string().max(8_000),
  observed: z.string().max(8_000),
  requiredFix: z.array(z.string().max(8_000)).max(32),
  disposition: FindingDisposition,
  separationReason: z.string().trim().min(1).max(8_000).optional(),
  lineage: FindingLineage.optional(),
  lineageRef: FindingLineageRef.optional(),
}).strict().superRefine((finding, context) => {
  if (finding.disposition === 'separate-issue' && !finding.separationReason) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['separationReason'],
      message: 'separate-issue finding requires a separation reason',
    });
  }
  if (finding.disposition === 'in-change' && finding.separationReason) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['separationReason'],
      message: 'in-change finding cannot carry a separation reason',
    });
  }
  if (finding.lineage === 'persisted' && !finding.lineageRef) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['lineageRef'],
      message: 'persisted finding requires a lineage reference',
    });
  }
  if (finding.lineage !== 'persisted' && finding.lineageRef) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['lineageRef'],
      message: 'only persisted finding can carry a lineage reference',
    });
  }
});
export type DevelopmentReviewFinding = z.infer<typeof DevelopmentReviewFinding>;

export const DevelopmentReviewPerspective = z.enum([
  'functionality', 'codeQuality', 'testQuality', 'ux', 'accessibility',
  'security', 'type-design', 'panel-escalation',
]);

export const DevelopmentReviewRound = z.object({
  round: z.number().int().positive().max(1_000),
  headSha: z.string().regex(/^[0-9a-f]{40}([0-9a-f]{24})?$/),
  branch: z.string().trim().min(1).max(500),
  pullRequestNumber: z.number().int().positive().nullable(),
  outcome: z.enum(['running', 'approve', 'request_changes', 'escalated']),
  startedAt: z.string().datetime({ offset: true }),
  completedAt: z.string().datetime({ offset: true }).nullable(),
  perspectives: z.array(z.object({
    perspective: DevelopmentReviewPerspective,
    verdict: Verdict,
    findings: z.array(DevelopmentReviewFinding).max(100),
  }).strict()).max(8),
}).strict().superRefine((review, context) => {
  if (review.outcome === 'running') {
    if (review.completedAt !== null) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['completedAt'],
        message: 'running review cannot be completed',
      });
    }
    if (review.perspectives.length !== 0) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['perspectives'],
        message: 'running review cannot carry completed perspectives',
      });
    }
    return;
  }
  if (review.completedAt === null) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['completedAt'],
      message: 'terminal review requires completedAt',
    });
  }
  if (review.perspectives.length === 0) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['perspectives'],
      message: 'terminal review requires at least one perspective',
    });
  }
  if (review.completedAt !== null
    && Date.parse(review.completedAt) < Date.parse(review.startedAt)) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ['completedAt'],
      message: 'review completedAt cannot precede startedAt',
    });
  }
  const seen = new Set<string>();
  review.perspectives.forEach((perspective, index) => {
    if (seen.has(perspective.perspective)) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['perspectives', index, 'perspective'],
        message: 'review perspectives must be unique',
      });
    }
    seen.add(perspective.perspective);
  });
});
export type DevelopmentReviewRound = z.infer<typeof DevelopmentReviewRound>;

export function reviewFindingProjection(finding: Finding): DevelopmentReviewFinding {
  return DevelopmentReviewFinding.parse({
    criterionId: finding.criterionId,
    severity: finding.severity,
    expected: finding.expected,
    observed: finding.observed,
    requiredFix: finding.requiredFix,
    disposition: finding.disposition ?? 'in-change',
    ...(finding.separationReason
      ? { separationReason: finding.separationReason }
      : {}),
    ...(finding.lineage ? { lineage: finding.lineage } : {}),
    ...(finding.lineageRef ? { lineageRef: finding.lineageRef } : {}),
  });
}

export function reviewPerspectiveProjection(run: EvalRun) {
  return {
    perspective: run.perspective ?? 'functionality',
    verdict: run.verdict,
    findings: run.findings.map(reviewFindingProjection),
  } as const;
}
