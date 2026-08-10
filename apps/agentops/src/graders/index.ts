/**
 * Graders turn a BuildArtifact + Issue Contract into a verdict with evidence.
 *
 * Two tiers, per the spec:
 *   1. Deterministic hard gates — build/typecheck/tests/secrets/scope + per-criterion
 *      checks. ANY blocker failure => request_changes, regardless of score.
 *   2. A composite score (functionality + quality dimensions) — only consulted when
 *      no blocker failed, compared against a threshold.
 *
 * The LLM/rubric grader is represented here by the artifact's `quality.*` signals;
 * swapping in a real model-graded rubric means replacing how those four numbers are
 * produced, nothing else.
 */

import type { BuildArtifact } from '../domain/artifact.js';
import {
  type AcceptanceCriterion,
  type Finding,
  type GateResult,
  type IssueContract,
  type Scores,
  type Severity,
  type Verdict,
} from '../domain/schema.js';
import type { HarnessConfig } from '../config.js';
import {
  HARD_GATE_SIGNAL_NAMES,
  type HardGateSignalName,
} from './gate-names.js';

export interface GradeResult {
  hardGates: Partial<Record<HardGateSignalName, GateResult>>;
  findings: Finding[];
  scores: Scores;
  overall: number;
  verdict: Verdict;
  blockerCount: number;
}

const SEVERITY_WEIGHT: Record<Severity, number> = { blocker: 3, major: 2, minor: 1 };

/** Blocking global gates (playwright is handled via per-criterion findings). */
const BLOCKING_GATES = HARD_GATE_SIGNAL_NAMES.filter((gate) => gate !== 'playwright');

function acFinding(ac: AcceptanceCriterion): Finding {
  return {
    criterionId: ac.id,
    severity: ac.severity,
    expected: ac.verification.expected.join('; '),
    observed: `Criterion not satisfied by this build (${ac.verification.method} check failed).`,
    reproductionSteps: [
      `Exercise behaviour: ${ac.behavior}`,
      ...ac.verification.expected.map((e) => `Then check: ${e}`),
    ],
    evidence: {},
    requiredFix: [
      `Make true: ${ac.behavior}`,
      ...ac.verification.expected.map((e) => `Ensure: ${e}`),
    ],
  };
}

function gateFinding(gate: string, fix: string): Finding {
  return {
    criterionId: `GATE-${gate}`,
    severity: 'blocker',
    expected: `${gate} passes`,
    observed: `${gate} failed`,
    reproductionSteps: [`Run the ${gate} gate`],
    evidence: {},
    requiredFix: [fix],
  };
}

function normalizedWeights(w: HarnessConfig['scoreWeights']): HarnessConfig['scoreWeights'] {
  const sum = w.functionality + w.codeQuality + w.testQuality + w.ux + w.accessibility || 1;
  return {
    functionality: w.functionality / sum,
    codeQuality: w.codeQuality / sum,
    testQuality: w.testQuality / sum,
    ux: w.ux / sum,
    accessibility: w.accessibility / sum,
  };
}

export function gradeBuild(
  contract: IssueContract,
  artifact: BuildArtifact,
  config: HarnessConfig,
): GradeResult {
  const findings: Finding[] = [];

  // --- per-criterion checks ------------------------------------------------
  let satisfiedWeight = 0;
  let totalWeight = 0;
  let unsatisfiedBlockers = 0;
  for (const ac of contract.acceptanceCriteria) {
    const w = SEVERITY_WEIGHT[ac.severity];
    totalWeight += w;
    if (artifact.satisfied[ac.id]) {
      satisfiedWeight += w;
    } else {
      findings.push(acFinding(ac));
      if (ac.severity === 'blocker') unsatisfiedBlockers++;
    }
  }
  const functionality = totalWeight === 0 ? 1 : satisfiedWeight / totalWeight;

  // --- hard gates ----------------------------------------------------------
  const pwAcs = contract.acceptanceCriteria.filter((a) => a.verification.method === 'playwright');
  const playwright: GateResult =
    pwAcs.length === 0 ? 'skip' : pwAcs.every((a) => artifact.satisfied[a.id]) ? 'pass' : 'fail';

  const hardGates: Record<HardGateSignalName, GateResult> = {
    build: artifact.buildPasses ? 'pass' : 'fail',
    typecheck: artifact.typecheckPasses ? 'pass' : 'fail',
    unit_tests: artifact.unitTestsPass ? 'pass' : 'fail',
    api_tests: artifact.apiTestsPass ? 'pass' : 'fail',
    grader_profile: artifact.graderProfileValid === undefined
      ? 'skip'
      : artifact.graderProfileValid
        ? 'pass'
        : 'fail',
    secrets_scan: artifact.secretsLeaked ? 'fail' : 'pass',
    scope_check: artifact.scopeViolations.length ? 'fail' : 'pass',
    playwright,
  };

  let gateBlockers = 0;
  for (const gate of BLOCKING_GATES) {
    if (hardGates[gate] === 'fail') {
      gateBlockers++;
      findings.push(gateFinding(gate, fixForGate(gate, artifact)));
    }
  }

  // --- composite score -----------------------------------------------------
  const scores: Scores = {
    functionality,
    codeQuality: artifact.quality.codeQuality,
    testQuality: artifact.quality.testQuality,
    ux: artifact.quality.ux,
    accessibility: artifact.quality.accessibility,
  };
  const w = normalizedWeights(config.scoreWeights);
  const overall =
    scores.functionality * w.functionality +
    scores.codeQuality * w.codeQuality +
    scores.testQuality * w.testQuality +
    scores.ux * w.ux +
    scores.accessibility * w.accessibility;

  // --- verdict -------------------------------------------------------------
  const blockerCount = unsatisfiedBlockers + gateBlockers;
  let verdict: Verdict;
  if (blockerCount > 0) {
    verdict = 'request_changes';
  } else {
    verdict = overall >= config.passThreshold ? 'approve' : 'request_changes';
  }

  return { hardGates, findings, scores, overall, verdict, blockerCount };
}

/**
 * Does any *blocking* hard gate fail? The evaluator panel is only convened once the
 * hard gates pass (ADR-0006 E4 / AC-PANEL-002 — the perspective version of
 * hard-gate-before-score). Playwright is excluded here: it is handled per-criterion,
 * not as a pre-panel gate. Reuses the single BLOCKING_GATES list — no duplication.
 */
export function hasBlockingGateFailure(
  hardGates: Partial<Record<HardGateSignalName, GateResult>>,
): boolean {
  return BLOCKING_GATES.some((g) => hardGates[g] === 'fail');
}

function fixForGate(gate: string, artifact: BuildArtifact): string {
  switch (gate) {
    case 'scope_check':
      return `Revert out-of-scope edits: ${artifact.scopeViolations.join(', ')}`;
    case 'secrets_scan':
      return 'Remove leaked secrets and rotate them.';
    case 'typecheck':
      return 'Fix type errors until typecheck passes.';
    case 'unit_tests':
      return 'Fix failing unit tests.';
    case 'api_tests':
      return 'Fix failing API contract tests.';
    case 'grader_profile':
      return artifact.graderProfileError
        ? `Restore the immutable-at-claim grader profile: ${artifact.graderProfileError}`
        : 'Restore the immutable-at-claim repository grader profile.';
    case 'build':
      return 'Fix the build.';
    default:
      return `Make ${gate} pass.`;
  }
}
