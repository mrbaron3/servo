-- Review-round outcomes use the same underscore spelling as perspective
-- Verdict. Rewrite durable rows before installing the canonical constraint.
-- This migration also completes the finding-origin pairing invariant that the
-- 0022 type fix intentionally left additive-only.

ALTER TABLE agentops_control.development_review_rounds
  DROP CONSTRAINT development_review_rounds_outcome_check;

UPDATE agentops_control.development_review_rounds
   SET outcome = 'request_changes'
 WHERE outcome = 'request-changes';

ALTER TABLE agentops_control.development_review_rounds
  ADD CONSTRAINT development_review_rounds_outcome_check CHECK (
    outcome IN ('running', 'approve', 'request_changes', 'escalated')
  );

DO $migration$
DECLARE
  definition text;
  updated_definition text;
  lineage_ref_check text := $needle$
         OR (
           finding ? 'lineageRef'
           AND (
             jsonb_typeof(finding->'lineageRef') <> 'string'
             OR finding->>'lineageRef' !~ '^finding-origin-v1:[0-9a-f]{64}$'
           )
         ) THEN
$needle$;
  paired_lineage_check text := $replacement$
         OR (
           finding ? 'lineageRef'
           AND (
             jsonb_typeof(finding->'lineageRef') <> 'string'
             OR finding->>'lineageRef' !~ '^finding-origin-v1:[0-9a-f]{64}$'
           )
         )
         OR (
           finding->>'lineage' = 'persisted'
           AND NOT (finding ? 'lineageRef')
         )
         OR (
           finding ? 'lineageRef'
           AND finding->>'lineage' IS DISTINCT FROM 'persisted'
         ) THEN
$replacement$;
BEGIN
  SELECT pg_get_functiondef(
    'agentops_control.record_development_review_round(uuid,text,jsonb)'
      ::regprocedure
  ) INTO definition;
  updated_definition := replace(
    definition,
    '''request-changes''',
    '''request_changes'''
  );
  updated_definition := replace(
    updated_definition,
    lineage_ref_check,
    paired_lineage_check
  );
  IF updated_definition = definition
     OR position('request-changes' IN updated_definition) <> 0
     OR position(
       'finding->>''lineage'' = ''persisted''' IN updated_definition
     ) = 0 THEN
    RAISE EXCEPTION 'review-round validator rewrite did not match migration 0022';
  END IF;
  EXECUTE updated_definition;

  SELECT pg_get_functiondef(
    'agentops_control.record_review_child(uuid,text,jsonb)'::regprocedure
  ) INTO definition;
  updated_definition := replace(
    definition,
    '''request-changes''',
    '''request_changes'''
  );
  IF updated_definition = definition
     OR position('request-changes' IN updated_definition) <> 0 THEN
    RAISE EXCEPTION 'review-child validator rewrite did not match migration 0021';
  END IF;
  EXECUTE updated_definition;
END
$migration$;
