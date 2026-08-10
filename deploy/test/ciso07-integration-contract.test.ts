import fs from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { describe, expect, it } from 'vitest';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const read = (relative: string) => fs.readFileSync(path.join(root, relative), 'utf8');

describe('CISO-07 integrated release source contracts', () => {
  // Every runtime stage must fix its own working directory. Left unset, the
  // runtime starts the process in $HOME, and each of these images points HOME at
  // a 0700 directory owned by its unprivileged user. That is fine for the image's
  // own user and fatal for the volume-init containers `agentopsctl` runs from
  // these same images as root with `--cap-drop ALL`: without CAP_DAC_OVERRIDE,
  // root cannot enter a home it does not own, so the process dies before it can
  // seed a credential. github-broker shipped without WORKDIR and failed exactly
  // that way on the first live MONITOR_ONLY start.
  it('fixes a working directory in every runtime stage', () => {
    const containerfile = read('deploy/Containerfile');
    const stages = new Map<string, string[]>();
    let current = '';
    for (const line of containerfile.split('\n')) {
      const from = /^FROM\s+.*\sAS\s+([a-z0-9-]+)\s*$/.exec(line);
      if (from) {
        current = from[1]!;
        stages.set(current, []);
        continue;
      }
      if (current) stages.get(current)!.push(line);
    }
    for (const stage of ['control', 'runner', 'triage', 'github-broker']) {
      const body = stages.get(stage);
      expect(body, `stage ${stage} is missing`).toBeDefined();
      expect(
        body!.some((line) => line.startsWith('WORKDIR ')),
        `stage ${stage} must declare WORKDIR`,
      ).toBe(true);
    }
  });

  // The runner's whole job is to run provider sessions inside tmux. With SHELL unset,
  // tmux resolves the passwd login shell, and a nologin shell kills every window at
  // spawn — the holder's home tab dies, the last session closes, the server exits, and
  // the next new-window fails with "no server running". DF-006's first live claim burned
  // all three attempts exactly this way, so the runner user must keep a real shell.
  // triage deliberately ships no tmux, so nologin stays correct there.
  it('gives the runner user a real login shell for tmux sessions', () => {
    const containerfile = read('deploy/Containerfile');
    const stages = new Map<string, string[]>();
    let current = '';
    for (const line of containerfile.split('\n')) {
      const from = /^FROM\s+.*\sAS\s+([a-z0-9-]+)\s*$/.exec(line);
      if (from) {
        current = from[1]!;
        stages.set(current, []);
        continue;
      }
      if (current) stages.get(current)!.push(line);
    }
    const runner = (stages.get('runner') ?? []).join('\n');
    expect(runner, 'runner stage must create the agentops user').toContain('useradd');
    expect(runner).toContain('--shell /bin/sh');
    expect(runner).not.toContain('--shell /usr/sbin/nologin');
  });

  it('builds all five release roles from immutable standard-OCI bases', () => {
    const containerfile = read('deploy/Containerfile');
    const externalFromLines = containerfile
      .split('\n')
      .filter((line) => line.startsWith('FROM ') && line.includes(':'));
    expect(externalFromLines).toHaveLength(7);
    for (const line of externalFromLines) {
      expect(line).toMatch(/@sha256:[0-9a-f]{64}\s+AS\s+[a-z0-9-]+$/);
    }
    expect(containerfile).toContain(' AS control');
    expect(containerfile).toContain(' AS runner');
    expect(containerfile).toContain(' AS triage');
    expect(containerfile).toContain(' AS github-broker');
    expect(containerfile).toContain(' AS postgres');
    expect(containerfile).toContain('git=1:2.47.3-0+deb13u1');
    expect(containerfile).toContain(
      'snapshot.debian.org/archive/debian/20260804T000000Z',
    );
    expect(containerfile).toContain(
      'snapshot.debian.org/archive/debian-security/20260804T000000Z',
    );
    expect(containerfile).toContain(
      'deploy/tools/provider-cli/package-lock.json',
    );
    expect(containerfile).toContain(
      'npm ci --omit=dev --ignore-scripts --prefix /opt/provider-cli',
    );
    expect(containerfile).toContain('golang:1.26.5-trixie@sha256:');
    expect(containerfile).toContain(
      'gcr.io/distroless/static-debian13:nonroot@sha256:',
    );
    expect(containerfile).toContain('USER 65532:65532');
    expect(containerfile).toContain('github.com/cli/cli/v2/cmd/gh');
    expect(read('deploy/tools/gh/go.mod')).toContain(
      'google.golang.org/grpc v1.82.1',
    );
    const providerLock = JSON.parse(
      read('deploy/tools/provider-cli/package-lock.json'),
    ) as {
      packages: Record<string, {
        version?: string;
        integrity?: string;
      }>;
    };
    expect(providerLock.packages['node_modules/@openai/codex']).toEqual(
      expect.objectContaining({
        version: '0.146.0',
        integrity: expect.stringMatching(/^sha512-/),
      }),
    );
    expect(providerLock.packages['node_modules/@anthropic-ai/claude-code'])
      .toEqual(expect.objectContaining({
        version: '2.1.221',
        integrity: expect.stringMatching(/^sha512-/),
      }));

    const manager = read('apps/control-plane/cmd/agentopsctl/manager.go');
    for (const [image, target] of [
      ['PostgresImage', 'postgres'],
      ['ControlImage', 'control'],
      ['GitHubBrokerImage', 'github-broker'],
      ['TriageImage', 'triage'],
      ['RunnerImage', 'runner'],
    ]) {
      expect(manager).toMatch(new RegExp(
        `BuildImage\\(\\s*ctx,\\s*manager\\.config\\.${image},\\s*"${target}",`,
      ));
    }
    expect(manager).not.toContain('"triage-runner"');
  });

  it('vendors the direct Node contract checker dependencies into the runner root', () => {
    const manifest = JSON.parse(read('apps/agentops/package.json')) as {
      devDependencies: Record<string, string>;
    };
    const lock = JSON.parse(read('package-lock.json')) as {
      packages: Record<string, { version?: string; integrity?: string }>;
    };
    const require = createRequire(import.meta.url);
    for (const dependency of ['ajv', 'ajv-formats']) {
      expect(manifest.devDependencies[dependency]).toBeDefined();
      expect(lock.packages[`node_modules/${dependency}`]).toEqual(
        expect.objectContaining({
          version: expect.any(String),
          integrity: expect.stringMatching(/^sha512-/),
        }),
      );
      expect(require.resolve(`${dependency}/package.json`)).toContain(
        `${path.sep}node_modules${path.sep}${dependency}${path.sep}`,
      );
    }
    const containerfile = read('deploy/Containerfile');
    expect(containerfile).toContain(
      'COPY --from=build --chown=node:node /app /app',
    );
    expect(containerfile).toContain('FROM runtime AS runner');
  });

  it('keeps Codex login and runtime mounts inside named credential boundaries', () => {
    const config = read('apps/control-plane/cmd/agentopsctl/config.go');
    const manager = read('apps/control-plane/cmd/agentopsctl/manager.go');
    const runtime = read('apps/control-plane/internal/lifecycle/runtime.go');
    expect(config).toContain('validateCodexAuthSource');
    expect(manager).toContain('CredentialVolume');
    expect(manager).toContain('Target:   "/run/agentops-credentials"');
    expect(manager).toContain('ReadOnly: true');
    expect(runtime).toContain('"exec", "--interactive", "--user", "65532:65532", name');
    expect(runtime).toContain('command.Stdin = stdin');
    expect(runtime).not.toContain('"copy", source');
    for (const forbidden of [
      'SSH_AUTH_SOCK',
      'docker.sock',
      'container.sock',
      ':/Users/',
      ':/home/',
    ]) {
      expect(manager).not.toContain(forbidden);
    }
  });

  it('preserves failed planning provenance and bounds the pre-main cutover', () => {
    const planning = JSON.parse(
      read('evidence/ciso-07/planning-resolution.json'),
    ) as {
      planningEnrichment: {
        status: string;
        preservedWithoutSuccessRewrite: boolean;
      };
      resolvedDecisions: unknown[];
      bootstrapCutover: { normalExecutionClaimsExcluded: string[] };
    };
    expect(planning.planningEnrichment).toEqual(expect.objectContaining({
      status: 'needs-human-review',
      preservedWithoutSuccessRewrite: true,
    }));
    expect(planning.resolvedDecisions).toHaveLength(7);
    expect(planning.bootstrapCutover.normalExecutionClaimsExcluded)
      .toHaveLength(3);
  });

  it('publishes a fail-closed complete-release evidence schema', () => {
    const schema = JSON.parse(
      read('contracts/ciso-07-release-evidence.schema.json'),
    ) as {
      required: string[];
      properties: Record<string, unknown>;
    };
    expect(schema.required).toEqual(expect.arrayContaining([
      'source',
      'images',
      'topology',
      'registration',
      'monitoring',
      'execution',
      'recovery',
      'formalReviews',
      'github',
      'artifacts',
    ]));
    expect(schema.properties.result).toEqual({ const: 'passed' });
    expect(read('apps/agentops/src/evidence/ciso07.ts')).toContain(
      'ciso07SemanticErrors',
    );

    const ajv = new Ajv2020({ strict: true, allErrors: true });
    const validate = ajv.compile(schema);
    expect(validate({
      schemaVersion: '1.0',
      issue: 'mrbaron3/workflow#17',
      source: {
        immutableBase: '257dc557753b099a646c94d3e3cc700468ffb32a',
        initialHead: '0'.repeat(40),
        finalHead: '1'.repeat(40),
        treeSha256: '2'.repeat(64),
        pullRequest: 41,
        capturedAt: '2026-07-26T00:00:00Z',
      },
      result: 'passed',
    })).toBe(false);
    expect(validate.errors).toEqual(expect.arrayContaining([
      expect.objectContaining({ keyword: 'required' }),
    ]));
  });
});
