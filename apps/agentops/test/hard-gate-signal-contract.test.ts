import fs from 'node:fs';
import { parse as parseYaml } from 'yaml';
import { describe, expect, it } from 'vitest';
import { HARD_GATE_SIGNAL_NAMES } from '../src/graders/gate-names.js';
import { EvalRun } from '../src/domain/schema.js';
import { repositoryPath } from '../src/runtime/roots.js';

describe('Hard Gate Signal Name published language', () => {
  it('keeps generated Go, JSON Schema, and OpenAPI mirrors aligned', () => {
    const generatedGoNames = JSON.parse(fs.readFileSync(repositoryPath(
      'apps', 'control-plane', 'internal', 'control',
      'hard_gate_signal_names.generated.json',
    ), 'utf8')) as string[];
    const legacyReceiptSchema = JSON.parse(fs.readFileSync(repositoryPath(
      'contracts', 'live-release-receipt.schema.json',
    ), 'utf8')) as any;
    const receiptSchema = JSON.parse(fs.readFileSync(repositoryPath(
      'contracts', 'live-release-receipt-v4.schema.json',
    ), 'utf8')) as any;
    const openapi = parseYaml(fs.readFileSync(repositoryPath(
      'contracts', 'control-api', 'v1', 'openapi.yaml',
    ), 'utf8')) as any;

    expect(generatedGoNames).toEqual(HARD_GATE_SIGNAL_NAMES);
    expect(legacyReceiptSchema.$defs.gateSignal.oneOf[0].properties.name.enum)
      .toEqual(HARD_GATE_SIGNAL_NAMES);
    expect(receiptSchema.$defs.gateSignal.oneOf[0].properties.name.enum)
      .toEqual(HARD_GATE_SIGNAL_NAMES);
    expect(openapi.components.schemas.ReleaseEvidencePolicy.properties
      .requiredGateSignals.items.oneOf[0].properties.name.enum)
      .toEqual(HARD_GATE_SIGNAL_NAMES);
    expect(openapi.components.schemas.RegistrationInput.properties.configuration.$ref)
      .toBe('#/components/schemas/RegistrationConfiguration');
    expect(openapi.components.schemas.Registration.properties.configuration.$ref)
      .toBe('#/components/schemas/HistoricalRegistrationConfiguration');
    expect(openapi.components.schemas.HistoricalReleaseEvidencePolicy.properties
      .requiredGateSignals.items.properties.name)
      .toEqual({ type: 'string', minLength: 1, maxLength: 128 });
    expect(openapi.components.schemas.ReleaseEvidencePolicy.properties
      .requiredGateSignals.uniqueItems).toBe(true);
    expect(openapi.components.schemas.ReleaseEvidencePolicy.properties
      .requiredReviewPerspectives.uniqueItems).toBe(true);
    expect(openapi.components.schemas.HistoricalReleaseEvidencePolicy.properties
      .requiredGateSignals.uniqueItems).toBeUndefined();
    expect(openapi.components.schemas.HistoricalReleaseEvidencePolicy.properties
      .requiredReviewPerspectives.uniqueItems).toBeUndefined();
  });

  it('closes EvalRun hard-gate keys over the canonical namespace', () => {
    const base = {
      id: 'EVAL-HARD-GATES',
      issueId: 'ISSUE-1',
      prId: 'PR-1',
      attempt: 1,
      sampleIndex: 0,
      agent: 'mock',
      verdict: 'approve',
      hardGates: Object.fromEntries(HARD_GATE_SIGNAL_NAMES.map((name) => [name, 'pass'])),
      scores: {
        functionality: 1,
        codeQuality: 1,
        testQuality: 1,
        ux: 1,
        accessibility: 1,
      },
      overall: 1,
      cost: {},
      createdAt: '2026-08-06T00:00:00.000Z',
    };
    expect(EvalRun.safeParse(base).success).toBe(true);
    expect(EvalRun.safeParse({
      ...base,
      hardGates: { ...base.hardGates, not_registered: 'pass' },
    }).success).toBe(false);
  });
});
