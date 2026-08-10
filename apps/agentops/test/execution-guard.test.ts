import { describe, it, expect } from 'vitest';
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import { Store } from '../src/store/store.js';
import { DEFAULT_CONFIG } from '../src/config.js';
import { Issue, type AgentProvider } from '../src/domain/schema.js';
// @ts-expect-error Stage 2 permanently removed the legacy GeneratorAgent alias.
import type { GeneratorAgent } from '../src/domain/agent-runtime.js';
import { pollable, isAiManaged } from '../src/pipeline/execution/guard.js';

void (null as GeneratorAgent | null);

function tmpStore(name: string): Store {
  const dir = path.join(os.tmpdir(), 'agentops-test', `${name}-${process.pid}`);
  fs.rmSync(dir, { recursive: true, force: true });
  return new Store(dir);
}

function issue(id: string, status: string, assignedAgent: AgentProvider | null): Issue {
  return Issue.parse({
    id,
    type: 'story',
    title: id,
    area: 'harness',
    status,
    assignedAgent,
    createdAt: '2026-07-01T00:00:00.000Z',
    updatedAt: '2026-07-01T00:00:00.000Z',
  });
}

describe('execution scoping guard (ARCH-execution-002 / DOM-execution-006)', () => {
  const config = { ...DEFAULT_CONFIG, generator: 'claude' as AgentProvider };

  it('polls only contract-drafted issues assigned to the running agent (opt-in)', () => {
    const store = tmpStore('guard');
    store.addIssue(issue('ISSUE-0002', 'contract-drafted', 'claude')); // in scope
    store.addIssue(issue('ISSUE-0001', 'contract-drafted', 'claude')); // in scope (earlier id)
    store.addIssue(issue('ISSUE-0003', 'contract-drafted', null)); // human-owned → skip
    store.addIssue(issue('ISSUE-0004', 'contract-drafted', 'codex')); // other agent → skip
    store.addIssue(issue('ISSUE-0005', 'planned', 'claude')); // wrong status → skip
    store.addIssue(issue('ISSUE-0006', 'released', 'claude')); // already done → skip

    const got = pollable(store, config).map((i) => i.id);
    expect(got).toEqual(['ISSUE-0001', 'ISSUE-0002']); // only ai-managed + contract-drafted, id-sorted
  });

  it('never touches issues others created / left unassigned', () => {
    const store = tmpStore('guard-owner');
    const human = issue('ISSUE-0010', 'contract-drafted', null);
    const other = issue('ISSUE-0011', 'contract-drafted', 'codex');
    store.addIssue(human);
    store.addIssue(other);

    expect(isAiManaged(human, config)).toBe(false);
    expect(isAiManaged(other, config)).toBe(false);
    expect(pollable(store, config)).toHaveLength(0);
  });

  it('is agent-specific: the codex daemon and the claude daemon see disjoint queues', () => {
    const store = tmpStore('guard-agents');
    store.addIssue(issue('ISSUE-0020', 'contract-drafted', 'claude'));
    store.addIssue(issue('ISSUE-0021', 'contract-drafted', 'codex'));

    expect(pollable(store, { ...config, generator: 'claude' }).map((i) => i.id)).toEqual(['ISSUE-0020']);
    expect(pollable(store, { ...config, generator: 'codex' }).map((i) => i.id)).toEqual(['ISSUE-0021']);
  });
});
