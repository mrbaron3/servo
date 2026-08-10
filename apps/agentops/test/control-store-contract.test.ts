import fs from 'node:fs';
import path from 'node:path';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { describe, expect, it } from 'vitest';
import {
  CONTROL_SCHEMA_VERSION,
  HistoricalRepositoryRegistrationConfigurationContract,
  JobEnvelopeContract,
  RepositoryRegistrationInput,
  loadControlMigrations,
} from '../src/control-store/index.js';
import { agentopsPackagePath, repositoryPath } from '../src/runtime/roots.js';

describe('language-neutral control-store contract', () => {
  it('publishes a contiguous checksummed migration set', () => {
    const migrations = loadControlMigrations();
    expect(migrations.map(({ version }) => version))
      .toEqual(Array.from({ length: CONTROL_SCHEMA_VERSION }, (_, index) => index + 1));
    expect(migrations.every(({ checksum }) => /^[0-9a-f]{64}$/.test(checksum))).toBe(true);
    expect(migrations[0]?.sql).toContain('jobs_one_active_per_repository');
    expect(migrations[0]?.sql).toContain('pg_notify');
    expect(migrations[1]?.sql).toContain('monitor_actual_states');
    expect(migrations[1]?.sql).toContain('control_api_requests');
    expect(migrations[2]?.sql).toContain('jobs_terminal_outcome_shape');
    expect(migrations[2]?.sql).toContain('runner.boundary.claim.denied');
    expect(migrations[3]?.sql).toContain('lifecycle_state');
    expect(migrations[3]?.sql).toContain('jobs_require_active_lifecycle');
    expect(migrations[6]?.sql).toContain('promote_triage_job');
    expect(migrations[6]?.sql).toContain('p_allowed_repositories text[]');
    expect(migrations[11]?.sql).toContain('observe_release_head(uuid, text, text)');
    expect(migrations[11]?.sql).toContain("'type-design'");
    expect(migrations[21]?.sql).toContain("finding->>'lineage' NOT IN ('new', 'persisted')");
    expect(migrations[21]?.sql).toContain("^finding-origin-v1:[0-9a-f]{64}$");
    expect(migrations[22]?.sql).toContain("'mergeMethod', current_merge_method");
    expect(migrations[23]?.sql).toContain("SET outcome = 'request_changes'");
    expect(migrations[23]?.sql).toContain("finding->>'lineage' = 'persisted'");
    expect(migrations[23]?.sql).toContain("finding->>'lineage' IS DISTINCT FROM 'persisted'");
    expect(migrations[24]?.sql).toContain('RENAME COLUMN child_head_sha TO head_sha');
    expect(migrations[25]?.sql).toContain("'sourceIssueClosure', 'completed'");
    expect(migrations[26]?.sql).toContain('RENAME TO retired_build_defects');
    expect(fs.readFileSync(
      agentopsPackagePath('src', 'control-store', 'store.ts'),
      'utf8',
    )).toContain('FOR UPDATE OF j, r SKIP LOCKED');
  });

  it('keeps the v1 JSON fixture compatible with the TypeScript contract', () => {
    const fixture = JSON.parse(fs.readFileSync(
      repositoryPath(
        'contracts',
        'control-store',
        'v1',
        'fixtures',
        'job-envelope.valid.json',
      ),
      'utf8',
    ));
    const schema = JSON.parse(fs.readFileSync(
      repositoryPath(
        'contracts',
        'control-store',
        'v1',
        'job-envelope.schema.json',
      ),
      'utf8',
    ));
    const ajv = new Ajv2020({ strict: true });
    ajv.addFormat(
      'uuid',
      /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i,
    );
    ajv.addFormat('date-time', (value: string) => !Number.isNaN(Date.parse(value)));
    const validate = ajv.compile(schema);
    expect(validate(fixture), validate.errors?.map((error) => error.message).join(', '))
      .toBe(true);
    expect(JobEnvelopeContract.parse(fixture)).toEqual(fixture);
    expect(fixture.status).toBe('queued');
    const unexpected = { ...fixture, unexpected: true };
    expect(validate(unexpected)).toBe(false);
    expect(() => JobEnvelopeContract.parse(unexpected)).toThrow();
    expect(validate({ ...fixture, status: 'unknown' })).toBe(false);
  });

  it('keeps the Registration fixture compatible across JSON Schema, TypeScript, and Go', () => {
    const fixture = JSON.parse(fs.readFileSync(
      repositoryPath(
        'contracts',
        'control-store',
        'v1',
        'fixtures',
        'registration.valid.json',
      ),
      'utf8',
    ));
    const schema = JSON.parse(fs.readFileSync(
      repositoryPath(
        'contracts',
        'control-store',
        'v1',
        'registration.schema.json',
      ),
      'utf8',
    ));
    const ajv = new Ajv2020({ strict: true });
    ajv.addFormat(
      'uuid',
      /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i,
    );
    ajv.addFormat('date-time', (value: string) => !Number.isNaN(Date.parse(value)));
    const validate = ajv.compile(schema);
    expect(validate(fixture), validate.errors?.map((error) => error.message).join(', '))
      .toBe(true);
    expect(RepositoryRegistrationInput.parse(fixture)).toEqual({
      repository: fixture.repository,
      enabled: fixture.enabled,
      issueMonitorEnabled: fixture.issueMonitorEnabled,
      prMonitorEnabled: fixture.prMonitorEnabled,
      executionEnabled: fixture.executionEnabled,
      configuration: fixture.configuration,
    });
    expect(validate({ ...fixture, version: 0 })).toBe(false);
    expect(validate({ ...fixture, repository: 'UNKNOWN/Repo' })).toBe(false);
    const completeConfiguration = {
      ...fixture,
      configuration: {
        mergeMethod: 'merge',
        gateTimeoutSeconds: { default: 3_600, review: 7_200 },
        releaseEvidence: {
          authority: 'human-ready-allowed',
          requiredGateSignals: [{ source: 'repository-grader', name: 'unit_tests' }],
          requiredReviewPerspectives: ['security', 'codeQuality'],
          minimumHeadEpochs: 1,
        },
      },
    };
    expect(validate(completeConfiguration), JSON.stringify(validate.errors)).toBe(true);
    expect(RepositoryRegistrationInput.parse(completeConfiguration).configuration)
      .toEqual(completeConfiguration.configuration);
    expect(validate({ ...fixture, configuration: { mergeMethod: 'octopus' } })).toBe(false);
    const historical = {
      ...fixture,
      configuration: {
        releaseEvidence: {
          authority: 'human-ready-allowed',
          requiredGateSignals: [{ source: 'repository-grader', name: 'contracts' }],
          requiredReviewPerspectives: ['security', 'codeQuality'],
          minimumHeadEpochs: 1,
        },
      },
    };
    expect(validate(historical), validate.errors?.map((error) => error.message).join(', '))
      .toBe(true);
    expect(RepositoryRegistrationInput.safeParse(historical).success).toBe(false);
    expect(HistoricalRepositoryRegistrationConfigurationContract.safeParse(
      historical.configuration,
    ).success).toBe(true);
    const historicalDuplicates = {
      ...fixture,
      configuration: {
        releaseEvidence: {
          authority: 'human-ready-allowed',
          requiredGateSignals: [
            { source: 'repository-grader', name: 'unit_tests' },
            { source: 'repository-grader', name: 'unit_tests' },
          ],
          requiredReviewPerspectives: ['security', 'security'],
          minimumHeadEpochs: 1,
        },
      },
    };
    expect(validate(historicalDuplicates), validate.errors?.map(
      (error) => error.message,
    ).join(', ')).toBe(true);
    expect(RepositoryRegistrationInput.safeParse(historicalDuplicates).success)
      .toBe(false);
    expect(HistoricalRepositoryRegistrationConfigurationContract.safeParse(
      historicalDuplicates.configuration,
    ).success).toBe(true);
    const unsafeConfiguration = {
      ...fixture,
      configuration: { command: 'host-native-daemon' },
    };
    expect(validate(unsafeConfiguration)).toBe(false);
    expect(() => RepositoryRegistrationInput.parse(unsafeConfiguration)).toThrow();
  });

  it('stores only artifact metadata in the schema', () => {
    const sql = loadControlMigrations()[0]!.sql;
    expect(sql).toContain('uri text NOT NULL');
    expect(sql).toContain('sha256 text NOT NULL');
    expect(sql).toContain('size_bytes bigint NOT NULL');
    expect(sql).not.toMatch(/\bartifact_(?:body|content|bytes|data)\b/);
  });

  it('has no JSON-file fallback in the PostgreSQL control-store implementation', () => {
    const directory = agentopsPackagePath('src', 'control-store');
    const source = fs.readdirSync(directory)
      .filter((name) => name.endsWith('.ts') && name !== 'migrations.ts')
      .map((name) => fs.readFileSync(path.join(directory, name), 'utf8'))
      .join('\n');
    expect(source).not.toContain('webhooks.json');
    expect(source).not.toContain("from 'node:fs'");
    const productionSource = fs.readdirSync(agentopsPackagePath('src'), {
      recursive: true,
      encoding: 'utf8',
    })
      .filter((name) => name.endsWith('.ts'))
      .map((name) => fs.readFileSync(agentopsPackagePath('src', name), 'utf8'))
      .join('\n');
    expect(productionSource).not.toContain(['webhooks', 'json'].join('.'));
  });
});
