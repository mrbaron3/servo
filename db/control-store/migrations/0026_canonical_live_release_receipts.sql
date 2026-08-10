-- Canonical live-release receipt wire (v4): retain the provider invocation
-- key beside its opaque deterministic ref, use Pull Request Number everywhere,
-- preserve the three-value Verdict, and record the domain closure fact instead
-- of duplicating GitHub GraphQL enum spellings.
--
-- Historical receipts cannot recover an invocationKey that was never stored.
-- For those rows only, the previous opaque invocationId is retained as both
-- invocationKey and invocationRef. Likewise, legacy "findings" Verdict rows
-- cannot be split back into request_changes versus needs_human; migration maps
-- them to request_changes without claiming that the lost distinction existed.

ALTER TABLE agentops_control.release_receipt_outbox
  DISABLE TRIGGER release_receipt_immutable;

UPDATE agentops_control.release_receipt_outbox
   SET payload = (payload - 'triageInvocationId') || jsonb_build_object(
     'triageInvocationRef', payload->'triageInvocationId'
   )
 WHERE kind = 'authority'
   AND payload ? 'triageInvocationId';

UPDATE agentops_control.release_receipt_outbox receipt
   SET payload = jsonb_set(
     receipt.payload,
     '{invocations}',
     (
       SELECT jsonb_agg(
         (invocation - 'invocationId') || jsonb_build_object(
           'invocationKey', COALESCE(
             invocation->'invocationKey', invocation->'invocationId'
           ),
           'invocationRef', invocation->'invocationId'
         )
         ORDER BY ordinal
       )
         FROM jsonb_array_elements(receipt.payload->'invocations')
           WITH ORDINALITY AS source(invocation, ordinal)
     )
   )
 WHERE receipt.kind = 'runtime-provenance'
   AND EXISTS (
     SELECT 1
       FROM jsonb_array_elements(receipt.payload->'invocations') invocation
      WHERE invocation ? 'invocationId'
   );

UPDATE agentops_control.release_receipt_outbox
   SET payload = (payload - 'invocationId') || jsonb_build_object(
     'invocationRef', payload->'invocationId'
   )
 WHERE kind = 'build'
   AND payload ? 'invocationId';

UPDATE agentops_control.release_receipt_outbox
   SET payload = (
     payload - ARRAY['invocationId', 'verdict']
   ) || jsonb_build_object(
     'invocationRef', payload->'invocationId',
     'verdict', CASE payload->>'verdict'
       WHEN 'approved' THEN 'approve'
       ELSE 'request_changes'
     END,
     'hasFindings', jsonb_array_length(payload->'findings') > 0
   )
 WHERE kind = 'review'
   AND payload ? 'invocationId';

UPDATE agentops_control.release_receipt_outbox
   SET payload = (payload - 'pullRequest') || jsonb_build_object(
     'pullRequestNumber', payload->'pullRequest'
   )
 WHERE kind = 'merge-intent'
   AND payload ? 'pullRequest';

UPDATE agentops_control.release_receipt_outbox
   SET payload = (
     payload - ARRAY['pullRequest', 'issueState', 'issueStateReason']
   ) || jsonb_build_object(
     'pullRequestNumber', payload->'pullRequest',
     'sourceIssueClosure', 'completed'
   )
 WHERE kind = 'merge'
   AND payload ? 'pullRequest';

ALTER TABLE agentops_control.release_receipt_outbox
  ENABLE TRIGGER release_receipt_immutable;

-- Fresh AI-triage promotions still call the private producer retained by
-- migration 0016. Rewrite that producer as part of the wire migration so rows
-- created after this migration cannot bypass the canonical receipt contract.
DO $migration$
DECLARE
  definition text;
  updated_definition text;
BEGIN
  SELECT pg_get_functiondef(
    'agentops_control.promote_triage_release_new_identity(uuid,text,jsonb,text,text,jsonb)'
      ::regprocedure
  ) INTO definition;
  updated_definition := replace(
    definition,
    'triage_invocation_id',
    'triage_invocation_key'
  );
  updated_definition := replace(
    updated_definition,
    '  triage_invocation_key text;',
    $declarations$  triage_invocation_key text;
  triage_invocation_ref text;
  triage_invocation_hash bytea;
  triage_invocation_hex text;$declarations$
  );
  updated_definition := replace(
    updated_definition,
    '  authority_route := CASE',
    $canonical$  IF triage_invocation_key IS NOT NULL THEN
    triage_invocation_hash := sha256(
      convert_to(durable_release_id::text, 'UTF8')
      || decode('00', 'hex')
      || convert_to(triage_invocation_key, 'UTF8')
    );
    triage_invocation_hex := encode(triage_invocation_hash, 'hex');
    triage_invocation_ref := 'invocation:'
      || substr(triage_invocation_hex, 1, 8) || '-'
      || substr(triage_invocation_hex, 9, 4) || '-'
      || '4' || substr(triage_invocation_hex, 14, 3) || '-'
      || lpad(to_hex(
        (get_byte(triage_invocation_hash, 8) & 63) | 128
      ), 2, '0')
      || substr(triage_invocation_hex, 19, 2) || '-'
      || substr(triage_invocation_hex, 21, 12);
  END IF;

  authority_route := CASE$canonical$
  );
  updated_definition := replace(
    updated_definition,
    '''triageInvocationId'', triage_invocation_key',
    '''triageInvocationRef'', triage_invocation_ref'
  );
  updated_definition := replace(
    updated_definition,
    '''invocationId'', triage_invocation_key',
    $canonical$'invocationKey', triage_invocation_key,
          'invocationRef', triage_invocation_ref$canonical$
  );
  IF updated_definition = definition
     OR position('triage_invocation_id' IN updated_definition) <> 0
     OR position('''triageInvocationId''' IN updated_definition) <> 0
     OR position('''invocationId''' IN updated_definition) <> 0
     OR position('''triageInvocationRef''' IN updated_definition) = 0
     OR position('''invocationKey''' IN updated_definition) = 0
     OR position('''invocationRef''' IN updated_definition) = 0
     OR position('triage_invocation_hash := sha256(' IN updated_definition) = 0 THEN
    RAISE EXCEPTION 'triage promotion receipt rewrite did not match migration 0017';
  END IF;
  EXECUTE updated_definition;
END
$migration$;

ALTER FUNCTION agentops_control.record_release_receipt(jsonb)
  RENAME TO record_release_receipt_without_canonical_wire;
REVOKE ALL ON FUNCTION
  agentops_control.record_release_receipt_without_canonical_wire(jsonb)
  FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentops_runner') THEN
    REVOKE ALL ON FUNCTION
      agentops_control.record_release_receipt_without_canonical_wire(jsonb)
      FROM agentops_runner;
  END IF;
END
$$;

CREATE FUNCTION agentops_control.record_release_receipt(
  p_payload jsonb
) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
  invocation jsonb;
  seen_invocation_keys text[] := ARRAY[]::text[];
  seen_invocation_refs text[] := ARRAY[]::text[];
BEGIN
  IF jsonb_typeof(p_payload) <> 'object' THEN
    RAISE EXCEPTION 'release receipt must be an object';
  END IF;
  IF p_payload->>'kind' = 'authority'
     AND p_payload->>'route' = 'ai-triage-then-human-ready'
     AND (
       jsonb_typeof(p_payload->'triageInvocationRef') <> 'string'
       OR p_payload ? 'triageInvocationId'
     ) THEN
    RAISE EXCEPTION 'AI triage authority invocation reference is invalid';
  END IF;
  IF p_payload->>'kind' = 'build'
     AND (
       jsonb_typeof(p_payload->'invocationRef') <> 'string'
       OR p_payload ? 'invocationId'
     ) THEN
    RAISE EXCEPTION 'build invocation reference is invalid';
  END IF;
  IF p_payload->>'kind' = 'review' THEN
    IF jsonb_typeof(p_payload->'invocationRef') <> 'string'
       OR p_payload ? 'invocationId'
       OR p_payload->>'verdict' NOT IN (
         'approve', 'request_changes', 'needs_human'
       )
       OR jsonb_typeof(p_payload->'hasFindings') <> 'boolean'
       OR jsonb_typeof(p_payload->'findings') <> 'array'
       OR (p_payload->>'hasFindings')::boolean
          <> (jsonb_array_length(p_payload->'findings') > 0)
       OR (
         p_payload->>'verdict' = 'approve'
         AND (p_payload->>'hasFindings')::boolean
       ) THEN
      RAISE EXCEPTION 'review Verdict or finding projection is invalid';
    END IF;
  END IF;
  IF p_payload->>'kind' = 'runtime-provenance' THEN
    IF jsonb_typeof(p_payload->'invocations') <> 'array' THEN
      RAISE EXCEPTION 'runtime invocation projection is invalid';
    END IF;
    FOR invocation IN
      SELECT * FROM jsonb_array_elements(p_payload->'invocations')
    LOOP
      IF jsonb_typeof(invocation->'invocationKey') <> 'string'
         OR jsonb_typeof(invocation->'invocationRef') <> 'string'
         OR invocation ? 'invocationId'
         OR invocation->>'invocationKey' = ANY(seen_invocation_keys)
         OR invocation->>'invocationRef' = ANY(seen_invocation_refs) THEN
        RAISE EXCEPTION 'runtime invocation identity is invalid';
      END IF;
      seen_invocation_keys := array_append(
        seen_invocation_keys, invocation->>'invocationKey'
      );
      seen_invocation_refs := array_append(
        seen_invocation_refs, invocation->>'invocationRef'
      );
    END LOOP;
  END IF;
  RETURN agentops_control.record_release_receipt_without_canonical_wire(
    p_payload
  );
END
$$;

DO $migration$
DECLARE
  definition text;
  updated_definition text;
BEGIN
  SELECT pg_get_functiondef(
    'agentops_control.authorize_release_merge_without_requirements(jsonb)'
      ::regprocedure
  ) INTO definition;
  updated_definition := replace(
    replace(
      definition,
      'p_intent->>''pullRequest''',
      'p_intent->>''pullRequestNumber'''
    ),
    'receipt.payload->>''verdict'' = ''approved''',
    'receipt.payload->>''verdict'' = ''approve'''
  );
  IF updated_definition = definition
     OR position('pullRequest''' IN updated_definition) <> 0
     OR position('''approved''' IN updated_definition) <> 0 THEN
    RAISE EXCEPTION 'merge authorization rewrite did not match migration 0017';
  END IF;
  EXECUTE updated_definition;

  SELECT pg_get_functiondef(
    'agentops_control.complete_release_merge(jsonb)'::regprocedure
  ) INTO definition;
  updated_definition := replace(
    definition,
    'p_receipt->>''pullRequest''',
    'p_receipt->>''pullRequestNumber'''
  );
  updated_definition := replace(
    updated_definition,
    $legacy$
     OR p_receipt->>'issueState' <> 'CLOSED'
     OR p_receipt->>'issueStateReason' <> 'COMPLETED'
$legacy$,
    $canonical$
     OR p_receipt->>'sourceIssueClosure' <> 'completed'
     OR p_receipt ? 'issueState'
     OR p_receipt ? 'issueStateReason'
$canonical$
  );
  IF updated_definition = definition
     OR position('pullRequest''' IN updated_definition) <> 0
     OR position('p_receipt->>''issueState''' IN updated_definition) <> 0
     OR position('p_receipt->>''issueStateReason''' IN updated_definition) <> 0
     OR position('sourceIssueClosure' IN updated_definition) = 0 THEN
    RAISE EXCEPTION 'merge completion rewrite did not match migration 0008';
  END IF;
  EXECUTE updated_definition;
END
$migration$;

REVOKE ALL ON FUNCTION agentops_control.record_release_receipt(jsonb)
  FROM PUBLIC;
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentops_runner') THEN
    GRANT EXECUTE ON FUNCTION agentops_control.record_release_receipt(jsonb)
      TO agentops_runner;
  END IF;
END
$$;
