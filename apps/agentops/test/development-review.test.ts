import { describe, expect, it } from 'vitest';
import { DevelopmentReviewFinding } from '../src/domain/development-review.js';

describe('development review finding lineage', () => {
  const finding = {
    criterionId: 'QUALITY-lineage',
    severity: 'major' as const,
    expected: 'lineage remains traceable',
    observed: 'finding under review',
    requiredFix: ['preserve the origin'],
    disposition: 'in-change' as const,
  };
  const lineageRef = `finding-origin-v1:${'a'.repeat(64)}`;

  it('requires lineageRef exactly for persisted findings', () => {
    expect(DevelopmentReviewFinding.safeParse({
      ...finding,
      lineage: 'new',
    }).success).toBe(true);
    expect(DevelopmentReviewFinding.safeParse({
      ...finding,
      lineage: 'persisted',
      lineageRef,
    }).success).toBe(true);
    expect(DevelopmentReviewFinding.safeParse({
      ...finding,
      lineage: 'persisted',
    }).success).toBe(false);
    expect(DevelopmentReviewFinding.safeParse({
      ...finding,
      lineage: 'new',
      lineageRef,
    }).success).toBe(false);
    expect(DevelopmentReviewFinding.safeParse({
      ...finding,
      lineageRef,
    }).success).toBe(false);
  });
});
