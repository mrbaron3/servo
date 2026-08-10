import fs from 'node:fs';
import path from 'node:path';
import { parse as parseYaml } from 'yaml';
import { describe, expect, it } from 'vitest';
import { repositoryPath } from '../src/runtime/roots.js';

function schemaFiles(directory: string): string[] {
  return fs.readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const candidate = path.join(directory, entry.name);
    if (entry.isDirectory()) return schemaFiles(candidate);
    return entry.name.endsWith('.schema.json') ? [candidate] : [];
  });
}

describe('Servo contract naming', () => {
  it('uses the product repository for HTTP schema identifiers and titles', () => {
    for (const file of schemaFiles(repositoryPath('contracts'))) {
      const schema = JSON.parse(fs.readFileSync(file, 'utf8')) as {
        $id?: string;
        title?: string;
      };
      if (schema.$id?.startsWith('http')) {
        expect(schema.$id, path.relative(repositoryPath(), file))
          .toMatch(/^https:\/\/github\.com\/mrbaron3\/servo\/contracts\//);
      }
      expect(schema.title ?? '', path.relative(repositoryPath(), file))
        .not.toContain('AgentOps');
    }

    const openapi = parseYaml(fs.readFileSync(repositoryPath(
      'contracts', 'control-api', 'v1', 'openapi.yaml',
    ), 'utf8')) as { info: { title: string } };
    expect(openapi.info.title).toBe('Servo Registration Control API');

    const dashboard = fs.readFileSync(repositoryPath(
      'apps', 'control-plane', 'internal', 'control', 'dashboard', 'index.html',
    ), 'utf8');
    expect(dashboard).toContain('<title>Servo Control</title>');
    expect(dashboard).toContain('<h1>Servo Control</h1>');
    expect(dashboard).not.toContain('AgentOps Control');

    const harnessDashboard = fs.readFileSync(repositoryPath(
      'apps', 'agentops', 'src', 'dashboard', 'dashboard.ts',
    ), 'utf8');
    expect(harnessDashboard).toContain('<title>Servo Dashboard</title>');
    expect(harnessDashboard).toContain('<h1>Servo — Development & Eval Harness</h1>');
    expect(harnessDashboard).not.toContain('<title>AgentOps Dashboard</title>');
    expect(harnessDashboard).not.toContain('<h1>AgentOps — Development & Eval Harness</h1>');
  });
});
