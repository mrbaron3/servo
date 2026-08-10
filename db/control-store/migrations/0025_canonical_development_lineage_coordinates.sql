-- A lineage node exposes its own head and pull-request coordinates with the
-- same names at every depth. Parent coordinates remain explicitly prefixed.

ALTER TABLE agentops_control.development_lineage_nodes
  RENAME COLUMN child_pull_request_number TO pull_request_number;
ALTER TABLE agentops_control.development_lineage_nodes
  RENAME COLUMN child_head_sha TO head_sha;

DO $migration$
DECLARE
  definition text;
  updated_definition text;
BEGIN
  SELECT pg_get_functiondef(
    'agentops_control.record_review_child(uuid,text,jsonb)'::regprocedure
  ) INTO definition;
  updated_definition := replace(
    replace(definition, 'child_pull_request_number', 'pull_request_number'),
    'child_head_sha',
    'head_sha'
  );
  IF updated_definition = definition
     OR position('child_pull_request_number' IN updated_definition) <> 0
     OR position('child_head_sha' IN updated_definition) <> 0 THEN
    RAISE EXCEPTION 'review-child lineage rewrite did not match migration 0024';
  END IF;
  EXECUTE updated_definition;

  SELECT pg_get_functiondef(
    'agentops_control.mark_review_child_integrated(uuid,text,uuid,bigint,text,text)'
      ::regprocedure
  ) INTO definition;
  updated_definition := replace(
    replace(
      replace(definition, 'child_pull_request_number', 'pull_request_number'),
      'child_head_sha',
      'head_sha'
    ),
    'p_head_sha',
    'p_child_head_sha'
  );
  IF updated_definition = definition
     OR position('child_pull_request_number' IN updated_definition) <> 0
     OR position('node.child_head_sha' IN updated_definition) <> 0
     OR position('SET status = ''integrated'',
         child_pull_request_number' IN updated_definition) <> 0 THEN
    RAISE EXCEPTION 'review-child integration rewrite did not match migration 0021';
  END IF;
  EXECUTE updated_definition;
END
$migration$;
