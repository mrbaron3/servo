/**
 * The Evaluator Panel (ARCH-execution-006, ADR-0006): instead of one composite verdict,
 * grade a completed sample through several independent *perspectives* (LANG-execution-010),
 * persist one perspective-tagged EvalRun per lens, and derive the sample's verdict
 * deterministically from those runs (DOM-execution-004 — no score-averaging across
 * perspectives; any perspective that withholds approval vetoes the sample).
 *
 * The deterministic boundary (ADR-0006): this module — fan-out, gate-before-panel,
 * resume, aggregation, escalation — is deterministic harness code. Only the per-perspective
 * *judgement* is pluggable via `PerspectiveGrader`: the built-in one is deterministic
 * (so the whole loop runs offline in tests); the real backend swaps in tmux LLM sessions
 * for the six non-functionality lenses (E2), behind the same seam.
 */

import { z } from 'zod';
import {
  EvalRun,
  Finding,
  Scores,
  Verdict,
  type IssueContract,
  type RevisionBinding,
} from '../domain/schema.js';
import type { BuildArtifact } from '../domain/artifact.js';
import type { HarnessConfig } from '../config.js';
import { Store, nowISO } from '../store/store.js';
import { gradeBuild, hasBlockingGateFailure } from '../graders/index.js';
import { hashUnit } from '../util/hash.js';
import { REVIEW_PERSPECTIVE_KEYS } from '../domain/review-perspectives.js';

/**
 * The 7 review perspectives (LANG-execution-010). `deterministic` lenses are graded by
 * code, not an LLM session (functionality; ADR-0006 E2). The aggregate withholds approval
 * unless *every* perspective approves (DOM-execution-004), so a per-perspective request_changes
 * always vetoes — the point of "hard-gate-before-score, per perspective".
 */
export interface PerspectiveSpec {
  key: string;
  deterministic: boolean;
}
export const PERSPECTIVES: PerspectiveSpec[] = REVIEW_PERSPECTIVE_KEYS.map((key) => ({
  key,
  deterministic: key === 'functionality',
}));
export const PANEL_ESCALATION_PERSPECTIVE = 'panel-escalation';

/** What a single perspective grader returns. Validated before it is trusted (AC-PANEL-006). */
export const PerspectiveResult = z.object({
  verdict: Verdict,
  findings: z.array(Finding).default([]),
  scores: Scores,
  overall: z.number().min(0).max(1),
});
export type PerspectiveResult = z.infer<typeof PerspectiveResult>;

/**
 * A per-perspective judge. May throw or return something invalid — the panel validates
 * every result and escalates rather than trusting it (AC-PANEL-006). The real backend's
 * implementation drives a tmux session and parses its findings.json here.
 */
export type PerspectiveGrader = (
  perspective: string,
  contract: IssueContract,
  artifact: BuildArtifact,
  config: HarnessConfig,
) => PerspectiveResult;

interface PanelInputBase {
  issueId: string;
  prId: string;
  contract: IssueContract;
  artifact: BuildArtifact;
  sampleIndex: number;
  attempt: number;
  agent: EvalRun['agent'];
  /** Non-deterministic reviewer invocation identities, keyed by perspective. */
  invocationKeys?: Record<string, string>;
  featureArea?: string;
}
type UnboundPanelInput = { revisionId?: null; headSha?: null };
export type PanelInput = PanelInputBase & (RevisionBinding | UnboundPanelInput);

export interface PanelResult {
  verdict: Verdict; // aggregate (approve | request_changes | needs_human)
  runs: EvalRun[]; // the perspective-tagged runs graded this call (excludes reused ones)
  gateFailed: boolean;
  escalated: boolean;
  perspectives: string[]; // which perspectives now have a run for this (pr, attempt)
}

export interface RunPanelOptions {
  perspectives?: PerspectiveSpec[];
  grader?: PerspectiveGrader;
  /** Max attempts to get a schema-valid result from a perspective grader before escalating. */
  maxGraderRetries?: number;
}

/**
 * Aggregate perspective runs into the sample verdict (DOM-execution-004). Pure: derived,
 * never stored (AC-PANEL-004). Rule: approve iff every run approves; a single request_changes
 * vetoes; any needs_human forces needs_human (an unresolved perspective is not an approval).
 * Given legacy single (perspective=null) runs it returns that run's verdict — backward compatible.
 */
export function aggregatePanelVerdict(runs: Pick<EvalRun, 'verdict'>[]): Verdict {
  if (runs.length === 0) return 'needs_human'; // no evidence is never an approval
  if (runs.some((r) => r.verdict === 'needs_human')) return 'needs_human';
  return runs.every((r) => r.verdict === 'approve') ? 'approve' : 'request_changes';
}

/** Deterministic stand-in judge for every perspective — real, offline, reproducible. */
export function deterministicPerspectiveGrade(
  perspective: string,
  contract: IssueContract,
  artifact: BuildArtifact,
  config: HarnessConfig,
): PerspectiveResult {
  const base = gradeBuild(contract, artifact, config);
  const threshold = config.passThreshold;
  const zero = { functionality: 0, codeQuality: 0, testQuality: 0, ux: 0, accessibility: 0 };

  // functionality: the grounded AC-satisfaction grade (findings for unsatisfied criteria).
  if (perspective === 'functionality') {
    const findings = base.findings.filter((f) => !f.criterionId.startsWith('GATE-'));
    const score = base.scores.functionality;
    return PerspectiveResult.parse({
      verdict: perspectiveVerdict(findings, score, threshold),
      findings,
      scores: { ...zero, functionality: score },
      overall: score,
    });
  }

  // security: a real deterministic signal already in the artifact.
  if (perspective === 'security') {
    const findings: Finding[] = artifact.secretsLeaked
      ? [lensFinding('SEC-secrets', 'blocker', 'no secrets in the diff', 'a secret was leaked', 'Remove leaked secrets and rotate them.')]
      : [];
    const score = artifact.secretsLeaked ? 0 : 1;
    return PerspectiveResult.parse({
      verdict: perspectiveVerdict(findings, score, threshold),
      findings,
      scores: zero,
      overall: score,
    });
  }

  // the quality lenses read their own dimension of artifact.quality (type-design proxies codeQuality).
  const dim =
    perspective === 'codeQuality'
      ? 'codeQuality'
      : perspective === 'testQuality'
        ? 'testQuality'
        : perspective === 'ux'
          ? 'ux'
          : perspective === 'accessibility'
            ? 'accessibility'
            : 'codeQuality'; // type-design proxy
  const score = artifact.quality[dim as keyof BuildArtifact['quality']];
  const findings: Finding[] =
    score < threshold
      ? [lensFinding(`${perspective}-below-bar`, 'major', `${perspective} score >= ${threshold}`, `${perspective} score ${score.toFixed(2)}`, `Improve ${perspective} to at least ${threshold}.`)]
      : [];
  return PerspectiveResult.parse({
    verdict: perspectiveVerdict(findings, score, threshold),
    findings,
    scores: { ...zero, [dim]: score },
    overall: score,
  });
}

/** A perspective withholds approval on any blocker finding, or when its score is below bar. */
function perspectiveVerdict(findings: Finding[], score: number, threshold: number): Verdict {
  if (findings.some((f) => f.severity === 'blocker')) return 'request_changes';
  return score >= threshold ? 'approve' : 'request_changes';
}

function lensFinding(
  criterionId: string,
  severity: Finding['severity'],
  expected: string,
  observed: string,
  fix: string,
): Finding {
  return {
    criterionId,
    severity,
    expected,
    observed,
    reproductionSteps: [`Re-grade this perspective and observe: ${observed}`],
    evidence: { trace: 'trace.txt' },
    requiredFix: [fix],
  };
}

/**
 * Convene the panel for one (sample, attempt) of a PR and persist its perspective runs.
 *
 * - gate-before-panel (AC-PANEL-002): if a blocking hard gate fails, no perspective run is
 *   created; a single perspective=null gate run carries the gate findings for repair.
 * - resume-idempotent (AC-PANEL-007): a perspective already graded for this (pr, attempt) is
 *   not re-graded; only missing perspectives run.
 * - escalation (AC-PANEL-006): a grader whose output fails validation after retries sends the
 *   issue to needs-human-review; the panel never counts an invalid perspective as an approval.
 * - read-only (AC-PANEL-008): grading reads the artifact; it never mutates changedFiles/scope.
 */
export function runPanel(store: Store, config: HarnessConfig, input: PanelInput, opts: RunPanelOptions = {}): PanelResult {
  const perspectives = opts.perspectives ?? PERSPECTIVES;
  const grader = opts.grader ?? deterministicPerspectiveGrade;
  const maxRetries = opts.maxGraderRetries ?? 1;
  const area = input.featureArea ?? 'unknown';

  const existing = store
    .runsForIssue(input.issueId)
    .filter((r) => r.prId === input.prId && r.attempt === input.attempt)
    .filter((r) => input.revisionId
      ? r.revisionId === input.revisionId && r.headSha === input.headSha
      : true);

  // --- gate-before-panel (AC-PANEL-002) -----------------------------------
  const base = gradeBuild(input.contract, input.artifact, config);
  if (hasBlockingGateFailure(base.hardGates)) {
    const gateFindings = base.findings.filter((f) => f.criterionId.startsWith('GATE-'));
    const runs: EvalRun[] = [];
    // one perspective=null gate run per attempt (skip if already present — idempotent)
    if (!existing.some((r) => r.perspective === null)) {
      runs.push(persistRun(store, config, input, null, {
        verdict: 'request_changes',
        findings: gateFindings,
        scores: base.scores,
        overall: base.overall,
      }, base.hardGates, area));
    }
    return { verdict: 'request_changes', runs, gateFailed: true, escalated: false, perspectives: [] };
  }

  // --- per-perspective grading (AC-PANEL-001), resume-idempotent (AC-PANEL-007) ---
  const graded: EvalRun[] = [];
  let escalated = false;
  for (const p of perspectives) {
    const prior = existing.find((r) => r.perspective === p.key);
    if (prior) {
      graded.push(prior);
      continue; // already graded — do not re-run (resume)
    }
    const result = gradeWithRetry(grader, p.key, input, config, maxRetries);
    if (!result) {
      // AC-PANEL-006: invalid output after retries — escalate, never silently pass.
      escalated = true;
      continue;
    }
    // Keep repository-grader facts on successful perspective rows. Release
    // evidence must not reconstruct a passed local gate after the job-local
    // build artifact is gone or relabel it as a GitHub CheckRun.
    graded.push(persistRun(store, config, input, p.key, result, base.hardGates, area));
  }

  if (escalated) {
    let escalationRun = existing.find((run) =>
      run.perspective === PANEL_ESCALATION_PERSPECTIVE
      && run.verdict === 'needs_human');
    if (!escalationRun) {
      escalationRun = persistRun(store, config, input, PANEL_ESCALATION_PERSPECTIVE, {
        verdict: 'needs_human',
        findings: [lensFinding(
          'PANEL-OUTPUT',
          'blocker',
          'every required reviewer returns one schema-valid result',
          'one or more required reviewer results were missing or invalid after retry',
          'Inspect the retained reviewer evidence/session and authorize a new head for re-review.',
        )],
        scores: {
          functionality: 0,
          codeQuality: 0,
          testQuality: 0,
          ux: 0,
          accessibility: 0,
        },
        overall: 0,
      }, base.hardGates, area);
    }
    if (store.getIssue(input.issueId)?.status !== 'needs-human-review') {
      store.setStatus(input.issueId, 'needs-human-review');
    }
    return {
      verdict: 'needs_human',
      runs: [
        ...graded.filter((r) => !existing.includes(r)),
        ...(existing.includes(escalationRun) ? [] : [escalationRun]),
      ],
      gateFailed: false,
      escalated: true,
      perspectives: graded.map((r) => r.perspective!).filter(Boolean),
    };
  }

  const verdict = aggregatePanelVerdict(graded);
  return {
    verdict,
    runs: graded.filter((r) => !existing.includes(r)),
    gateFailed: false,
    escalated: false,
    perspectives: graded.map((r) => r.perspective).filter((x): x is string => x !== null),
  };
}

/** Grade one perspective, validating the result; retry on invalid; null = give up (escalate). */
function gradeWithRetry(
  grader: PerspectiveGrader,
  perspective: string,
  input: PanelInput,
  config: HarnessConfig,
  maxRetries: number,
): PerspectiveResult | null {
  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    try {
      const parsed = PerspectiveResult.parse(grader(perspective, input.contract, input.artifact, config));
      // ADR-0009: an approve token can never mask a P0/P1-equivalent finding.
      if (
        parsed.verdict === 'approve'
        && parsed.findings.some((finding) =>
          finding.severity === 'blocker' || finding.severity === 'major')
      ) {
        return { ...parsed, verdict: 'request_changes' };
      }
      return parsed;
    } catch {
      // invalid output (threw, or failed schema) — retry, then escalate
    }
  }
  return null;
}

function persistRun(
  store: Store,
  config: HarnessConfig,
  input: PanelInput,
  perspective: string | null,
  result: Pick<PerspectiveResult, 'verdict' | 'findings' | 'scores' | 'overall'>,
  hardGates: EvalRun['hardGates'],
  area: string,
): EvalRun {
  const id = store.nextId('EVAL', 5);
  const c = hashUnit(`${id}|cost`);
  const tokens = Math.round(900 + input.attempt * 400 + c * 900);
  const run = EvalRun.parse({
    id,
    issueId: input.issueId,
    prId: input.prId,
    attempt: input.attempt,
    sampleIndex: input.sampleIndex,
    agent: input.agent,
    verdict: result.verdict,
    hardGates,
    findings: result.findings,
    scores: result.scores,
    overall: result.overall,
    evidenceDir: null,
    cost: { tokens, usd: Number(((tokens / 1_000_000) * 3).toFixed(4)), seconds: Math.round(10 + c * 15) },
    featureArea: area,
    humanVerdict: null,
    perspective,
    invocationKey: perspective === null ? null : input.invocationKeys?.[perspective] ?? null,
    revisionId: input.revisionId ?? null,
    headSha: input.headSha ?? null,
    createdAt: nowISO(),
  });
  return store.addEvalRun(run);
}
