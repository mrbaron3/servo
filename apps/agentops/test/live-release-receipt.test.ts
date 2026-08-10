import fs from 'node:fs';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { describe, expect, it } from 'vitest';
import { INTERVENTION_KINDS } from '../src/domain/schema.js';
import {
  HistoricalReleaseGradeReceiptContract,
  HistoricalReleasePolicyContract,
  LiveReleaseReceiptEvidenceContract,
  PersistedLiveReleaseReceiptEvidenceContract,
  ReleaseGradeReceiptContract,
  ReleasePolicyContract,
  legacyLiveReleaseReceiptEvidenceWire,
  liveReleaseReceiptSemanticErrors,
  releasePreMergeSemanticErrors,
} from '../src/evidence/release-receipt.js';

const releaseId = '10000000-0000-4000-8000-000000000000';
const authorityId = '20000000-0000-4000-8000-000000000000';
const requirementsAuthorityId = '20000000-0000-4000-8000-000000000001';
const runtimeId = '30000000-0000-4000-8000-000000000000';
const build1Id = '40000000-0000-4000-8000-000000000001';
const build2Id = '40000000-0000-4000-8000-000000000002';
const firstSecurityId = '50000000-0000-4000-8000-000000000001';
const firstCorrectnessId = '50000000-0000-4000-8000-000000000002';
const finalSecurityId = '50000000-0000-4000-8000-000000000003';
const finalCorrectnessId = '50000000-0000-4000-8000-000000000004';
const resolutionId = '60000000-0000-4000-8000-000000000000';
const repositoryGradeId = '70000000-0000-4000-8000-000000000001';
const githubCheckId = '70000000-0000-4000-8000-000000000002';
const historicalGradeId = '70000000-0000-4000-8000-000000000003';
const mergeIntentId = '75000000-0000-4000-8000-000000000000';
const mergeId = '80000000-0000-4000-8000-000000000000';
const firstHead = 'b'.repeat(40);
const finalHead = 'a'.repeat(40);
const mergeSha = 'c'.repeat(40);
const consumerHead = 'd'.repeat(40);
const digest = `sha256:${'e'.repeat(64)}`;

function base(
  receiptId: string,
  receiptKey: string,
  recordedAt: string,
  causes: string[],
) {
  return {
    receiptId,
    receiptKey,
    releaseId,
    repository: 'mrbaron3/designflow',
    issueNumber: 4,
    producer: {},
    causes,
    recordedAt,
  };
}

function evidence(): any {
  return {
    schemaVersion: '4.0',
    release: {
      id: releaseId,
      repository: 'mrbaron3/designflow',
      issueNumber: 4,
      pullRequestNumber: 12,
      finalHead,
      mergeSha,
      createdAt: '2026-08-01T00:00:00Z',
      completedAt: '2026-08-01T00:09:00Z',
    },
    policy: {
      authority: 'human-ready-allowed',
      requiredGateSignals: [
        { source: 'repository-grader', name: 'unit_tests' },
        { source: 'github-check', name: 'contracts' },
      ],
      requiredReviewPerspectives: ['security', 'codeQuality'],
      minimumHeadEpochs: 2,
    },
    receipts: {
      authority: {
        ...base(authorityId, 'authority:human-ready', '2026-08-01T00:00:10Z', []),
        kind: 'authority',
        route: 'human-ready',
        actor: { type: 'human', login: 'maintainer' },
        readyLabel: 'ready',
        readyAt: '2026-08-01T00:00:05Z',
      },
      requirementsAuthority: {
        ...base(
          requirementsAuthorityId,
          'requirements-authority',
          '2026-08-01T00:00:11Z',
          [authorityId],
        ),
        kind: 'requirements-authority',
        sourceIssueDigest: 'f'.repeat(64),
        sourceUpdatedAt: '2026-08-01T00:00:05Z',
        capturedAt: '2026-08-01T00:00:06Z',
      },
      runtime: [{
        ...base(runtimeId, 'runtime:release', '2026-08-01T00:07:00Z', [authorityId]),
        kind: 'runtime-provenance',
        consumer: { repository: 'mrbaron3/workflow', revision: consumerHead },
        environment: {
          kind: 'container',
          reference: 'ghcr.io/mrbaron3/workflow@sha256:fixture',
          digest,
        },
        invocations: [
          {
            invocationKey: 'generator-key-1',
            invocationRef: 'generator-1',
            role: 'generator',
            provider: 'codex',
            model: {
              kind: 'provider-default',
              reference: 'codex-cli@1.2.3:default-model',
              resolverDigest: digest,
            },
            head: firstHead,
          },
          {
            invocationKey: 'repair-key-1',
            invocationRef: 'repair-1',
            role: 'repair',
            provider: 'codex',
            model: { kind: 'explicit', name: 'gpt-5.6-codex' },
            head: finalHead,
          },
          ...['security-1', 'codeQuality-1'].map((invocationRef) => ({
            invocationKey: `${invocationRef}-key`,
            invocationRef,
            role: 'reviewer',
            provider: 'claude',
            model: { kind: 'explicit', name: 'claude-opus' },
            head: firstHead,
          })),
          ...['security-2', 'codeQuality-2'].map((invocationRef) => ({
            invocationKey: `${invocationRef}-key`,
            invocationRef,
            role: 'reviewer',
            provider: 'claude',
            model: { kind: 'explicit', name: 'claude-opus' },
            head: finalHead,
          })),
        ],
      }],
      builds: [
        {
          ...base(build1Id, 'build:first', '2026-08-01T00:01:00Z', [authorityId]),
          kind: 'build',
          head: firstHead,
          parentHead: null,
          invocationRef: 'generator-1',
          role: 'generator',
        },
        {
          ...base(
            build2Id,
            'build:final',
            '2026-08-01T00:03:00Z',
            [authorityId, build1Id, firstSecurityId],
          ),
          kind: 'build',
          head: finalHead,
          parentHead: firstHead,
          invocationRef: 'repair-1',
          role: 'repair',
        },
      ],
      grades: [
        {
          ...base(repositoryGradeId, 'grade:repository:unit_tests', '2026-08-01T00:05:00Z', [build2Id]),
          kind: 'grade',
          head: finalHead,
          signal: { source: 'repository-grader', name: 'unit_tests' },
          status: 'passed',
          detailsDigest: digest,
        },
        {
          ...base(githubCheckId, 'grade:github:contracts', '2026-08-01T00:05:10Z', [build2Id]),
          kind: 'grade',
          head: finalHead,
          signal: { source: 'github-check', name: 'contracts' },
          status: 'passed',
          detailsDigest: digest,
        },
      ],
      reviews: [
        {
          ...base(firstSecurityId, 'review:1:security', '2026-08-01T00:02:00Z', [build1Id]),
          kind: 'review',
          head: firstHead,
          headEpoch: 1,
          perspective: 'security',
          invocationRef: 'security-1',
          verdict: 'request_changes',
          hasFindings: true,
          findings: [{ findingId: 'finding-1', lineage: 'new' }],
        },
        {
          ...base(firstCorrectnessId, 'review:1:codeQuality', '2026-08-01T00:02:05Z', [build1Id]),
          kind: 'review',
          head: firstHead,
          headEpoch: 1,
          perspective: 'codeQuality',
          invocationRef: 'codeQuality-1',
          verdict: 'approve',
          hasFindings: false,
          findings: [],
        },
        {
          ...base(finalSecurityId, 'review:2:security', '2026-08-01T00:06:00Z', [build2Id]),
          kind: 'review',
          head: finalHead,
          headEpoch: 2,
          perspective: 'security',
          invocationRef: 'security-2',
          verdict: 'approve',
          hasFindings: false,
          findings: [],
        },
        {
          ...base(finalCorrectnessId, 'review:2:codeQuality', '2026-08-01T00:06:05Z', [build2Id]),
          kind: 'review',
          head: finalHead,
          headEpoch: 2,
          perspective: 'codeQuality',
          invocationRef: 'codeQuality-2',
          verdict: 'approve',
          hasFindings: false,
          findings: [],
        },
      ],
      findingResolutions: [{
        ...base(resolutionId, 'finding-resolution:finding-1', '2026-08-01T00:04:00Z', [firstSecurityId, build2Id]),
        kind: 'finding-resolution',
        findingId: 'finding-1',
        raisedByReviewReceiptId: firstSecurityId,
        raisedOnHead: firstHead,
        resolvedByBuildReceiptId: build2Id,
        resolvedOnHead: finalHead,
      }],
      mergeIntent: {
        ...base(mergeIntentId, 'merge-intent:12', '2026-08-01T00:07:30Z', [
          authorityId,
          requirementsAuthorityId,
          runtimeId,
          build2Id,
          repositoryGradeId,
          githubCheckId,
          finalSecurityId,
          finalCorrectnessId,
          resolutionId,
        ]),
        kind: 'merge-intent',
        pullRequestNumber: 12,
        expectedHead: finalHead,
        observedPrHead: finalHead,
      },
      merge: {
        ...base(mergeId, 'merge:12', '2026-08-01T00:08:00Z', [mergeIntentId]),
        kind: 'merge',
        pullRequestNumber: 12,
        expectedHead: finalHead,
        observedPrHead: finalHead,
        mergeSha,
        actor: 'workflow-app[bot]',
        sourceIssueClosure: 'completed',
        mergeReachableFromDefaultBranch: true,
        mergedAt: '2026-08-01T00:08:00Z',
      },
      interventions: [],
    },
    artifacts: [{
      kind: 'receipt-export',
      uri: 'volume://registrations/fixture/release.json',
      sha256: 'f'.repeat(64),
      sizeBytes: 1024,
      releaseId,
      sourceHead: finalHead,
      receiptIds: [authorityId, runtimeId, mergeId],
    }],
    result: 'passed',
  };
}

function historicalEvidence(schemaVersion: '2.0' | '3.0'): any {
  const legacy = structuredClone(evidence());
  legacy.schemaVersion = schemaVersion;
  legacy.release.pullRequest = legacy.release.pullRequestNumber;
  delete legacy.release.pullRequestNumber;
  legacy.receipts.runtime.forEach((runtime: any) => {
    runtime.invocations.forEach((invocation: any) => {
      invocation.invocationId = invocation.invocationRef;
      delete invocation.invocationRef;
    });
  });
  legacy.receipts.builds.forEach((build: any) => {
    build.invocationId = build.invocationRef;
    delete build.invocationRef;
  });
  legacy.receipts.reviews.forEach((review: any) => {
    review.invocationId = review.invocationRef;
    review.verdict = review.verdict === 'approve' ? 'approved' : 'findings';
    delete review.invocationRef;
    delete review.hasFindings;
  });
  legacy.receipts.mergeIntent.pullRequest =
    legacy.receipts.mergeIntent.pullRequestNumber;
  delete legacy.receipts.mergeIntent.pullRequestNumber;
  legacy.receipts.merge.pullRequest = legacy.receipts.merge.pullRequestNumber;
  legacy.receipts.merge.issueState = 'CLOSED';
  legacy.receipts.merge.issueStateReason = 'COMPLETED';
  delete legacy.receipts.merge.pullRequestNumber;
  delete legacy.receipts.merge.sourceIssueClosure;
  if (schemaVersion === '2.0') {
    delete legacy.receipts.requirementsAuthority;
    for (const group of Object.values(legacy.receipts)) {
      for (const receipt of Array.isArray(group) ? group : [group]) {
        if (receipt && Array.isArray((receipt as any).causes)) {
          (receipt as any).causes = (receipt as any).causes.filter(
            (cause: string) => cause !== requirementsAuthorityId,
          );
        }
      }
    }
  }
  return legacy;
}

function compiled() {
  const schema = JSON.parse(fs.readFileSync(
    new URL('../../../contracts/live-release-receipt-v4.schema.json', import.meta.url),
    'utf8',
  ));
  return new Ajv2020({ strict: true, allErrors: true }).compile(schema);
}

function compiledHistorical() {
  const schema = JSON.parse(fs.readFileSync(
    new URL('../../../contracts/live-release-receipt.schema.json', import.meta.url),
    'utf8',
  ));
  return new Ajv2020({ strict: true, allErrors: true }).compile(schema);
}

describe('release receipt evidence v4', () => {
  it('validates gate names according to their source namespace', () => {
    const basePolicy = {
      authority: 'human-ready-allowed' as const,
      requiredReviewPerspectives: ['security', 'codeQuality'] as const,
      minimumHeadEpochs: 1,
    };
    expect(ReleasePolicyContract.safeParse({
      ...basePolicy,
      requiredGateSignals: [{ source: 'repository-grader', name: 'contracts' }],
    }).success).toBe(false);
    expect(ReleasePolicyContract.safeParse({
      ...basePolicy,
      requiredGateSignals: [{ source: 'repository-grader', name: 'unit_tests' }],
    }).success).toBe(true);
    expect(ReleasePolicyContract.safeParse({
      ...basePolicy,
      requiredGateSignals: [{ source: 'github-check', name: 'ci/custom' }],
    }).success).toBe(true);

    const invalidCanonicalGrade = {
      ...evidence().receipts.grades[0],
      signal: { source: 'repository-grader', name: 'contracts' },
    };
    expect(ReleaseGradeReceiptContract.safeParse(
      invalidCanonicalGrade,
    ).success).toBe(false);
    expect(HistoricalReleaseGradeReceiptContract.safeParse(
      invalidCanonicalGrade,
    ).success).toBe(true);
  });

  it('keeps historical v3 grade aliases decode-only while canonical writers stay strict', () => {
    const persistedV3 = historicalEvidence('3.0');
    const historicalGrade = {
      ...persistedV3.receipts.grades[0],
      receiptId: historicalGradeId,
      receiptKey: 'grade:repository:lint',
      recordedAt: '2026-08-01T00:05:05Z',
      signal: { source: 'repository-grader', name: 'lint' },
    };
    persistedV3.receipts.grades.push(historicalGrade);

    expect(ReleaseGradeReceiptContract.safeParse(historicalGrade).success).toBe(false);
    expect(HistoricalReleaseGradeReceiptContract.safeParse(historicalGrade).success)
      .toBe(true);
    expect(PersistedLiveReleaseReceiptEvidenceContract.safeParse(persistedV3).success)
      .toBe(true);
    expect(compiledHistorical()(persistedV3)).toBe(true);
    expect(liveReleaseReceiptSemanticErrors(persistedV3)).toEqual([]);
  });

  it('rejects duplicate policy requirements on writes while preserving historical reads', () => {
    const canonical = {
      authority: 'human-ready-allowed' as const,
      requiredGateSignals: [
        { source: 'repository-grader' as const, name: 'unit_tests' as const },
        { source: 'repository-grader' as const, name: 'unit_tests' as const },
      ],
      requiredReviewPerspectives: ['security' as const, 'codeQuality' as const],
      minimumHeadEpochs: 1,
    };
    expect(ReleasePolicyContract.safeParse(canonical).success).toBe(false);

    const historical = {
      ...canonical,
      requiredGateSignals: [
        { source: 'repository-grader' as const, name: 'contracts' },
        { source: 'repository-grader' as const, name: 'contracts' },
      ],
    };
    expect(HistoricalReleasePolicyContract.safeParse(historical).success).toBe(true);

    expect(ReleasePolicyContract.safeParse({
      ...canonical,
      requiredGateSignals: canonical.requiredGateSignals.slice(0, 1),
      requiredReviewPerspectives: ['security', 'security'],
    }).success).toBe(false);
    expect(HistoricalReleasePolicyContract.safeParse({
      ...historical,
      requiredGateSignals: historical.requiredGateSignals.slice(0, 1),
      requiredReviewPerspectives: ['security', 'security'],
    }).success).toBe(true);

    expect(ReleasePolicyContract.safeParse({
      ...canonical,
      requiredGateSignals: [
        { source: 'repository-grader', name: 'unit_tests' },
        { source: 'github-check', name: 'unit_tests' },
      ],
    }).success).toBe(true);
    expect(HistoricalReleasePolicyContract.safeParse({
      ...historical,
      requiredGateSignals: [
        { source: 'repository-grader', name: 'contracts' },
        { source: 'github-check', name: 'contracts' },
      ],
    }).success).toBe(true);

    const persisted = historicalEvidence('3.0');
    persisted.policy.requiredGateSignals = [
      { source: 'repository-grader', name: 'unit_tests' },
      { source: 'repository-grader', name: 'unit_tests' },
    ];
    persisted.policy.requiredReviewPerspectives = ['security', 'security'];
    expect(PersistedLiveReleaseReceiptEvidenceContract.safeParse(persisted).success)
      .toBe(true);
    expect(compiledHistorical()(persisted)).toBe(true);
    expect(liveReleaseReceiptSemanticErrors(persisted)).toEqual([]);
  });

  it('accepts only review perspectives the production panel can emit', () => {
    expect(ReleasePolicyContract.safeParse(evidence().policy).success).toBe(true);
    const unsupported = evidence();
    unsupported.policy.requiredReviewPerspectives = ['security', 'performance'];
    expect(ReleasePolicyContract.safeParse(unsupported.policy).success).toBe(false);
    expect(compiled()(unsupported)).toBe(false);
  });

  it('accepts a release assembled across unrelated job IDs and provider defaults', () => {
    const value = evidence();
    value.receipts.builds[0].producer = {
      jobId: '90000000-0000-4000-8000-000000000001',
    };
    value.receipts.merge.producer = {
      jobId: '90000000-0000-4000-8000-000000000002',
    };
    expect(PersistedLiveReleaseReceiptEvidenceContract.parse(value)).toEqual(value);
    const validate = compiled();
    expect(validate(value), JSON.stringify(validate.errors)).toBe(true);
    expect(liveReleaseReceiptSemanticErrors(value)).toEqual([]);
  });

  it('allows a changes-requested review without findings but keeps approvals empty', () => {
    const emptyRequest = evidence();
    emptyRequest.receipts.reviews[1].verdict = 'request_changes';
    expect(liveReleaseReceiptSemanticErrors(emptyRequest)).toEqual([]);

    const invalidApproval = evidence();
    invalidApproval.receipts.reviews[1].hasFindings = true;
    invalidApproval.receipts.reviews[1].findings = [{
      findingId: 'finding-on-approved-review',
      lineage: 'new',
    }];
    expect(liveReleaseReceiptSemanticErrors(invalidApproval).join('; '))
      .toContain('approve verdict cannot contain findings');
  });

  it('normalizes immutable legacy v3 evidence without inventing invocation keys', () => {
    const legacy = historicalEvidence('3.0');
    legacy.receipts.runtime.forEach((runtime: any) => {
      runtime.invocations.forEach((invocation: any) => {
        delete invocation.invocationKey;
      });
    });
    legacy.policy.requiredGateSignals[0].name = 'contracts';
    legacy.receipts.grades[0].signal.name = 'contracts';

    expect(PersistedLiveReleaseReceiptEvidenceContract.parse(legacy)).toEqual(legacy);
    const normalized = LiveReleaseReceiptEvidenceContract.parse(legacy);
    expect(normalized).toMatchObject({
      schemaVersion: '3.0',
      release: expect.objectContaining({ pullRequestNumber: 12 }),
      receipts: expect.objectContaining({
        merge: expect.objectContaining({ sourceIssueClosure: 'completed' }),
      }),
    });
    expect(normalized.receipts.runtime[0]?.invocations[0]).toMatchObject({
      invocationKey: 'generator-1',
      invocationRef: 'generator-1',
    });
    const legacySchema = JSON.parse(fs.readFileSync(
      new URL('../../../contracts/live-release-receipt.schema.json', import.meta.url),
      'utf8',
    ));
    const validate = new Ajv2020({ strict: true, allErrors: true }).compile(legacySchema);
    expect(validate(legacy), JSON.stringify(validate.errors)).toBe(true);
    expect(liveReleaseReceiptSemanticErrors(legacy)).toEqual([]);
  });

  it('exports pre-requirements releases on the immutable v2 wire', () => {
    const historical = evidence();
    historical.schemaVersion = '2.0';
    delete historical.receipts.requirementsAuthority;
    for (const group of Object.values(historical.receipts)) {
      for (const receipt of Array.isArray(group) ? group : [group]) {
        if (receipt && Array.isArray((receipt as any).causes)) {
          (receipt as any).causes = (receipt as any).causes.filter(
            (cause: string) => cause !== requirementsAuthorityId,
          );
        }
      }
    }
    const canonical = LiveReleaseReceiptEvidenceContract.parse(historical);
    const legacy = legacyLiveReleaseReceiptEvidenceWire(canonical) as any;
    const legacySchema = JSON.parse(fs.readFileSync(
      new URL('../../../contracts/live-release-receipt.schema.json', import.meta.url),
      'utf8',
    ));
    const validate = new Ajv2020({ strict: true, allErrors: true }).compile(legacySchema);

    expect(validate(legacy), JSON.stringify(validate.errors)).toBe(true);
    expect(legacy.release).toHaveProperty('pullRequest', 12);
    expect(legacy.release).not.toHaveProperty('pullRequestNumber');
    expect(legacy.receipts.runtime[0].invocations[0]).toMatchObject({
      invocationId: 'generator-1',
      invocationKey: 'generator-key-1',
    });
    expect(legacy.receipts).not.toHaveProperty('requirementsAuthority');
    expect(liveReleaseReceiptSemanticErrors(legacy)).toEqual([]);
  });

  it('accepts runtime provenance emitted by separate release jobs', () => {
    const value = evidence();
    const secondRuntimeId = '30000000-0000-4000-8000-000000000001';
    value.receipts.runtime.push({
      ...base(
        secondRuntimeId,
        'runtime:reconciliation-job',
        '2026-08-01T00:07:10Z',
        [authorityId],
      ),
      producer: { jobId: '90000000-0000-4000-8000-000000000003' },
      kind: 'runtime-provenance',
      consumer: { repository: 'mrbaron3/workflow', revision: consumerHead },
      environment: {
        kind: 'container',
        reference: 'ghcr.io/mrbaron3/workflow@sha256:fixture',
        digest,
      },
      invocations: [{
        invocationKey: 'planning-reconciliation-key',
        invocationRef: 'planning-reconciliation',
        role: 'planning',
        provider: 'codex',
        model: { kind: 'explicit', name: 'gpt-5.6-codex' },
      }],
    });
    value.receipts.mergeIntent.causes.push(secondRuntimeId);
    expect(liveReleaseReceiptSemanticErrors(value)).toEqual([]);
  });

  it('requires explicit and consistent finding lineage attestations', () => {
    const withoutOrigin = evidence();
    withoutOrigin.receipts.reviews[0].findings[0].lineage = 'persisted';
    expect(liveReleaseReceiptSemanticErrors(withoutOrigin)).toContain(
      'persisted finding finding-1 must reference an earlier head epoch',
    );

    const duplicateOrigin = evidence();
    duplicateOrigin.receipts.reviews[1].verdict = 'request_changes';
    duplicateOrigin.receipts.reviews[1].hasFindings = true;
    duplicateOrigin.receipts.reviews[1].findings = [{
      findingId: 'finding-1',
      lineage: 'new',
    }];
    expect(liveReleaseReceiptSemanticErrors(duplicateOrigin)).toContain(
      'finding finding-1 must be raised exactly once',
    );
  });

  it('keeps repository graders and GitHub checks as distinct gate sources', () => {
    const value = evidence();
    value.receipts.grades[1].signal = {
      source: 'repository-grader',
      name: 'unit_tests',
    };
    expect(liveReleaseReceiptSemanticErrors(value)).toContain(
      'final head is missing required gate signal github-check:contracts',
    );
  });

  it('supports direct human ready but fails closed when policy requires AI triage', () => {
    const direct = evidence();
    expect(liveReleaseReceiptSemanticErrors(direct)).toEqual([]);
    direct.policy.authority = 'ai-triage-required';
    expect(liveReleaseReceiptSemanticErrors(direct)).toContain(
      'AI-triage-required policy needs an AI triage authority receipt',
    );
  });

  it('accepts AI authority only when triage completed before human ready', () => {
    const value = evidence();
    value.policy.authority = 'ai-triage-required';
    value.receipts.authority = {
      ...value.receipts.authority,
      route: 'ai-triage-then-human-ready',
      triageInvocationRef: 'triage-1',
      triageCompletedAt: '2026-08-01T00:00:04Z',
      sourceDigest: '1'.repeat(64),
      decision: { schemaVersion: 1, readiness: 'ready_candidate' },
    };
    value.receipts.runtime[0].invocations.push({
      invocationKey: 'triage-key-1',
      invocationRef: 'triage-1',
      role: 'triage',
      provider: 'codex',
      model: { kind: 'explicit', name: 'gpt-5.6-codex' },
    });
    expect(liveReleaseReceiptSemanticErrors(value)).toEqual([]);
    value.receipts.authority.triageCompletedAt = '2026-08-01T00:00:06Z';
    expect(liveReleaseReceiptSemanticErrors(value)).toContain(
      'AI triage authority must complete before the human ready event',
    );
  });

  it.each([
    ['another release', (value: any) => { value.receipts.grades[0].releaseId = '99999999-9999-4999-8999-999999999999'; }],
    ['another repository', (value: any) => { value.receipts.reviews[0].repository = 'mrbaron3/workflow'; }],
    ['another issue', (value: any) => { value.receipts.builds[0].issueNumber = 44; }],
    ['a stale final review', (value: any) => { value.receipts.reviews[2].head = firstHead; }],
    ['a mixed build invocation', (value: any) => { value.receipts.builds[1].invocationRef = 'generator-1'; }],
    ['an unresolved finding', (value: any) => { value.receipts.findingResolutions = []; }],
    ['a job-counted fake epoch', (value: any) => { value.receipts.reviews[2].headEpoch = 3; value.receipts.reviews[3].headEpoch = 3; }],
    ['a broken causal edge', (value: any) => { value.receipts.merge.causes = [authorityId]; }],
    ['duplicate causes hidden from the pre-merge Zod path', (value: any) => { value.receipts.builds[0].causes.push(authorityId); }],
    ['runtime provenance detached from authority', (value: any) => { value.receipts.runtime[0].causes = []; }],
    ['a build role different from its invocation', (value: any) => { value.receipts.runtime[0].invocations[1].role = 'generator'; }],
    ['a repair detached from its parent build', (value: any) => { value.receipts.builds[1].causes = [authorityId, firstSecurityId]; }],
  ])('rejects receipt mixing through %s', (_name, mutate) => {
    const value = evidence();
    mutate(value);
    expect(liveReleaseReceiptSemanticErrors(value).length).toBeGreaterThan(0);
  });

  it('keeps writer uniqueness while requiring the final head to be the latest epoch', () => {
    const duplicate = evidence();
    duplicate.policy.requiredReviewPerspectives = ['security', 'security'];
    expect(ReleasePolicyContract.safeParse(duplicate.policy).success).toBe(false);
    expect(PersistedLiveReleaseReceiptEvidenceContract.safeParse(duplicate).success)
      .toBe(false);
    expect(liveReleaseReceiptSemanticErrors(duplicate).length).toBeGreaterThan(0);
    const historicalDuplicate = historicalEvidence('3.0');
    historicalDuplicate.policy.requiredReviewPerspectives = ['security', 'security'];
    expect(PersistedLiveReleaseReceiptEvidenceContract.safeParse(historicalDuplicate).success)
      .toBe(true);
    expect(liveReleaseReceiptSemanticErrors(historicalDuplicate)).toEqual([]);

    const staleFinal = evidence();
    staleFinal.receipts.reviews[0].verdict = 'approve';
    staleFinal.receipts.reviews[0].hasFindings = false;
    staleFinal.receipts.reviews[0].findings = [];
    staleFinal.receipts.findingResolutions = [];
    staleFinal.receipts.reviews[0].headEpoch = 2;
    staleFinal.receipts.reviews[1].headEpoch = 2;
    staleFinal.receipts.reviews[2].headEpoch = 1;
    staleFinal.receipts.reviews[3].headEpoch = 1;
    expect(liveReleaseReceiptSemanticErrors(staleFinal)).toContain(
      'release.finalHead must be the latest reviewed head epoch',
    );
  });

  it('rejects an unknown model instead of inventing a concrete name', () => {
    const value = evidence();
    value.receipts.runtime[0].invocations[0].model = null;
    expect(PersistedLiveReleaseReceiptEvidenceContract.safeParse(value).success).toBe(false);
    expect(compiled()(value)).toBe(false);
  });

  it('reads result from the HOW intervention ledger', () => {
    const value = evidence();
    const intervention = {
      ...base(
        '99999999-0000-4000-8000-000000000001',
        'intervention:1',
        '2026-08-01T00:07:30Z',
        [authorityId],
      ),
      kind: 'intervention',
      interventionKind: 'manual-evidence-collection',
      reason: 'operator recovered a missing artifact',
    };
    value.receipts.interventions.push(intervention);
    value.receipts.mergeIntent.causes.push(intervention.receiptId);
    value.result = 'passed-with-interventions';
    expect(liveReleaseReceiptSemanticErrors(value)).toEqual([]);
    value.result = 'passed';
    expect(liveReleaseReceiptSemanticErrors(value)).toContain(
      'result must be passed-with-interventions',
    );
  });

  it('keeps the published intervention vocabulary aligned with the domain', () => {
    const schema = JSON.parse(fs.readFileSync(
      new URL('../../../contracts/live-release-receipt-v4.schema.json', import.meta.url),
      'utf8',
    ));
    expect(schema.$defs.intervention.allOf[1].properties.interventionKind.enum)
      .toEqual([...INTERVENTION_KINDS]);
  });

  it('authorizes pre-merge state without requiring a completed merge receipt', () => {
    const value = LiveReleaseReceiptEvidenceContract.parse(evidence());
    if (value.schemaVersion !== '4.0') {
      throw new Error('fixture must decode as canonical release evidence v4');
    }
    const receipts = [
      value.receipts.authority,
      value.receipts.requirementsAuthority!,
      ...value.receipts.runtime,
      ...value.receipts.builds,
      ...value.receipts.grades,
      ...value.receipts.reviews,
      ...value.receipts.findingResolutions,
      ...value.receipts.interventions,
    ];
    expect(releasePreMergeSemanticErrors({
      releaseId,
      repository: value.release.repository,
      issueNumber: value.release.issueNumber,
      pullRequestNumber: value.release.pullRequestNumber,
      expectedHead: finalHead,
      policy: value.policy,
      receipts,
    })).toEqual([]);
  });
});
