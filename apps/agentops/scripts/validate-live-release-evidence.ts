import fs from 'node:fs';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { assertLiveReleaseSemanticEvidence } from '../src/evidence/live-release.js';
import { assertLiveReleaseReceiptEvidence } from '../src/evidence/release-receipt.js';
import { repositoryPath } from '../src/runtime/roots.js';

const evidencePath = process.argv[2];
if (!evidencePath) {
  throw new Error('usage: validate-live-release-evidence <evidence.json>');
}
const evidence = JSON.parse(fs.readFileSync(evidencePath, 'utf8')) as {
  schemaVersion?: unknown;
  receipts?: unknown;
  formalReviews?: unknown;
};
const receiptEvidence = evidence.receipts !== undefined;
const externalEvidence = evidence.formalReviews !== undefined;
if (receiptEvidence === externalEvidence) {
  throw new Error('evidence must contain exactly one of receipts or formalReviews');
}
let schemaName: string;
if (receiptEvidence) {
  if (evidence.schemaVersion === '4.0') {
    schemaName = 'live-release-receipt-v4.schema.json';
  } else if (evidence.schemaVersion === '2.0' || evidence.schemaVersion === '3.0') {
    schemaName = 'live-release-receipt.schema.json';
  } else {
    throw new Error(`unsupported release receipt schemaVersion: ${String(evidence.schemaVersion)}`);
  }
} else if (evidence.schemaVersion === '2.0') {
  schemaName = 'live-release-evidence-v2.schema.json';
} else if (evidence.schemaVersion === '1.0') {
  schemaName = 'live-release-evidence.schema.json';
} else {
  throw new Error(`unsupported external release evidence schemaVersion: ${String(evidence.schemaVersion)}`);
}
const schema = JSON.parse(
  fs.readFileSync(
    repositoryPath('contracts', schemaName),
    'utf8',
  ),
) as object;
const validate = new Ajv2020({ strict: true, allErrors: true }).compile(schema);
if (!validate(evidence)) {
  throw new Error(`live release evidence schema failed: ${JSON.stringify(validate.errors)}`);
}
if (receiptEvidence) assertLiveReleaseReceiptEvidence(evidence);
else assertLiveReleaseSemanticEvidence(evidence);
