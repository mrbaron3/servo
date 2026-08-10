-- Registration is the durable authority for the integration strategy used by
-- control-plane jobs. Existing Registrations omit mergeMethod and therefore
-- retain the historical squash behavior.

CREATE OR REPLACE FUNCTION agentops_control.valid_release_evidence_configuration(
  value jsonb
) RETURNS boolean LANGUAGE plpgsql IMMUTABLE
SET search_path = pg_catalog
AS $$
DECLARE
  policy jsonb;
  gate_timeouts jsonb;
  gate_key text;
  item jsonb;
  numeric_value numeric;
BEGIN
  IF jsonb_typeof(value) <> 'object'
     OR value - ARRAY[
       'releaseEvidence', 'gateTimeoutSeconds', 'mergeMethod'
     ] <> '{}'::jsonb THEN
    RETURN false;
  END IF;
  IF value ? 'mergeMethod'
     AND (
       jsonb_typeof(value->'mergeMethod') <> 'string'
       OR value->>'mergeMethod' NOT IN ('squash', 'merge', 'rebase')
     ) THEN
    RETURN false;
  END IF;
  IF value ? 'gateTimeoutSeconds' THEN
    gate_timeouts := value->'gateTimeoutSeconds';
    IF jsonb_typeof(gate_timeouts) <> 'object'
       OR gate_timeouts - ARRAY[
         'default', 'planning', 'design', 'repository-graders', 'review',
         'merge', 'lease-recovery'
       ] <> '{}'::jsonb THEN
      RETURN false;
    END IF;
    FOR gate_key IN SELECT * FROM jsonb_object_keys(gate_timeouts)
    LOOP
      IF jsonb_typeof(gate_timeouts->gate_key) <> 'number' THEN
        RETURN false;
      END IF;
      numeric_value := (gate_timeouts->>gate_key)::numeric;
      IF numeric_value NOT BETWEEN 60 AND 2592000
         OR mod(numeric_value, 1) <> 0 THEN
        RETURN false;
      END IF;
    END LOOP;
  END IF;
  IF NOT (value ? 'releaseEvidence') THEN
    RETURN true;
  END IF;
  policy := value->'releaseEvidence';
  IF jsonb_typeof(policy) <> 'object'
     OR policy - ARRAY[
       'authority', 'requiredGateSignals',
       'requiredReviewPerspectives', 'minimumHeadEpochs'
     ] <> '{}'::jsonb
     OR (SELECT count(*) FROM jsonb_object_keys(policy)) <> 4
     OR policy->>'authority' NOT IN (
       'human-ready-allowed', 'ai-triage-required'
     )
     OR jsonb_typeof(policy->'requiredGateSignals') <> 'array'
     OR jsonb_array_length(policy->'requiredGateSignals') NOT BETWEEN 1 AND 64
     OR jsonb_typeof(policy->'requiredReviewPerspectives') <> 'array'
     OR jsonb_array_length(
       policy->'requiredReviewPerspectives'
     ) NOT BETWEEN 2 AND 7
     OR jsonb_typeof(policy->'minimumHeadEpochs') <> 'number' THEN
    RETURN false;
  END IF;
  numeric_value := (policy->>'minimumHeadEpochs')::numeric;
  IF numeric_value NOT BETWEEN 1 AND 32 OR mod(numeric_value, 1) <> 0 THEN
    RETURN false;
  END IF;
  FOR item IN SELECT * FROM jsonb_array_elements(policy->'requiredGateSignals')
  LOOP
    IF jsonb_typeof(item) <> 'object'
       OR item - ARRAY['source', 'name'] <> '{}'::jsonb
       OR (SELECT count(*) FROM jsonb_object_keys(item)) <> 2
       OR item->>'source' NOT IN ('repository-grader', 'github-check')
       OR jsonb_typeof(item->'name') <> 'string'
       OR length(item->>'name') NOT BETWEEN 1 AND 128 THEN
      RETURN false;
    END IF;
  END LOOP;
  FOR item IN SELECT * FROM jsonb_array_elements(
    policy->'requiredReviewPerspectives'
  )
  LOOP
    IF jsonb_typeof(item) <> 'string'
       OR (item #>> '{}') NOT IN (
         'functionality', 'codeQuality', 'testQuality', 'ux',
         'accessibility', 'security', 'type-design'
       ) THEN
      RETURN false;
    END IF;
  END LOOP;
  RETURN true;
EXCEPTION WHEN OTHERS THEN
  RETURN false;
END
$$;

CREATE OR REPLACE FUNCTION agentops_control.promote_triage_job(
  p_lease_token uuid,
  p_worker_id text,
  p_result jsonb,
  p_ready_label text,
  p_claimed_label text
) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
  current_job_id uuid;
  current_attempt_id uuid;
  current_attempt_number integer;
  current_registration_id uuid;
  current_registration_version bigint;
  current_repository text;
  current_configuration jsonb;
  current_payload jsonb;
  current_merge_method text;
  issue_number bigint;
  promoted_job_id uuid := gen_random_uuid();
  promoted_payload jsonb;
  final_result jsonb;
BEGIN
  IF p_lease_token IS NULL
     OR p_worker_id IS NULL OR btrim(p_worker_id) = ''
     OR length(p_worker_id) > 256
     OR p_result IS NULL OR jsonb_typeof(p_result) <> 'object'
     OR p_ready_label IS NULL
     OR p_ready_label !~ '^[A-Za-z0-9][A-Za-z0-9._:/ -]{0,49}$'
     OR p_claimed_label IS NULL
     OR p_claimed_label !~ '^[A-Za-z0-9][A-Za-z0-9._:/ -]{0,49}$'
     OR p_ready_label = p_claimed_label THEN
    RAISE EXCEPTION 'invalid triage promotion request';
  END IF;

  PERFORM 1
    FROM agentops_control.lifecycle_state lifecycle
   WHERE lifecycle.singleton AND lifecycle.mode = 'ACTIVE'
   FOR SHARE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'triage promotion requires ACTIVE lifecycle';
  END IF;

  SELECT job.id, attempt.id, attempt.attempt_number,
         job.registration_id, job.registration_version,
         registration.repository, registration.configuration, job.payload
    INTO current_job_id, current_attempt_id, current_attempt_number,
         current_registration_id, current_registration_version,
         current_repository, current_configuration, current_payload
    FROM agentops_control.job_leases lease
    JOIN agentops_control.job_attempts attempt
      ON attempt.id = lease.attempt_id
    JOIN agentops_control.jobs job
      ON job.id = lease.job_id
    JOIN agentops_control.repository_registrations registration
      ON registration.id = job.registration_id
   WHERE lease.lease_token = p_lease_token
     AND lease.worker_id = p_worker_id
     AND lease.status = 'active'
     AND lease.expires_at > clock_timestamp()
     AND attempt.status = 'running'
     AND job.status = 'leased'
     AND job.job_type = 'agentops.triage'
     AND registration.enabled
     AND registration.execution_enabled
     AND registration.version = job.registration_version
   FOR UPDATE OF lease, attempt, job, registration;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'triage promotion lease is stale or lost';
  END IF;
  current_merge_method := COALESCE(
    current_configuration->>'mergeMethod',
    'squash'
  );

  IF jsonb_typeof(current_payload) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(current_payload)) <> 3
     OR current_payload - ARRAY[
       'schemaVersion', 'repository', 'issue'
     ] <> '{}'::jsonb
     OR current_payload->>'schemaVersion' <> '1'
     OR jsonb_typeof(current_payload->'repository') <> 'object'
     OR (
       SELECT count(*) FROM jsonb_object_keys(current_payload->'repository')
     ) <> 2
     OR (current_payload->'repository') - ARRAY[
       'owner', 'name'
     ] <> '{}'::jsonb
     OR concat(
          current_payload->'repository'->>'owner',
          '/',
          current_payload->'repository'->>'name'
        ) <> current_repository
     OR jsonb_typeof(current_payload->'issue') <> 'object'
     OR (
       SELECT count(*) FROM jsonb_object_keys(current_payload->'issue')
     ) <> 2
     OR (current_payload->'issue') - ARRAY[
       'number', 'observedUpdatedAt'
     ] <> '{}'::jsonb
     OR jsonb_typeof(current_payload->'issue'->'number') <> 'number'
     OR (current_payload->'issue'->>'number')::numeric <= 0
     OR (current_payload->'issue'->>'number')::numeric > 2147483647
     OR mod((current_payload->'issue'->>'number')::numeric, 1) <> 0 THEN
    RAISE EXCEPTION 'triage promotion payload is invalid';
  END IF;
  issue_number := (current_payload->'issue'->>'number')::bigint;

  IF (SELECT count(*) FROM jsonb_object_keys(p_result)) <> 13
     OR p_result - ARRAY[
       'schemaVersion', 'status', 'jobId', 'attemptNumber', 'repository',
       'issueNumber', 'outcome', 'sourceDigest', 'decision', 'commentUrl',
       'appliedLabels', 'promotedJobId', 'completedAt'
     ] <> '{}'::jsonb
     OR p_result->>'schemaVersion' <> '1'
     OR p_result->>'status' <> 'succeeded'
     OR p_result->>'jobId' <> current_job_id::text
     OR (p_result->>'attemptNumber')::integer <> current_attempt_number
     OR p_result->>'repository' <> current_repository
     OR (p_result->>'issueNumber')::bigint <> issue_number
     OR p_result->>'outcome' <> 'promoted'
     OR p_result->'sourceDigest' <> 'null'::jsonb
     OR p_result->'decision' <> 'null'::jsonb
     OR p_result->'commentUrl' <> 'null'::jsonb
     OR jsonb_typeof(p_result->'appliedLabels') <> 'array'
     OR p_result->'appliedLabels' <> '[]'::jsonb
     OR p_result->'promotedJobId' <> 'null'::jsonb
     OR jsonb_typeof(p_result->'completedAt') <> 'string'
     OR NOT pg_input_is_valid(
          p_result->>'completedAt',
          'timestamp with time zone'
        ) THEN
    RAISE EXCEPTION 'triage promotion result is invalid';
  END IF;

  promoted_payload := jsonb_build_object(
    'schemaVersion', 1,
    'repository', current_payload->'repository',
    'event', jsonb_build_object(
      'kind', 'issue',
      'number', issue_number,
      'action', 'recovery'
    ),
    'target', jsonb_build_object('baseRef', 'refs/heads/main'),
    'execution', jsonb_build_object(
      'mode', 'development_turn',
      'requiredChecks', '[]'::jsonb,
      'mergeMethod', current_merge_method,
      'readyLabel', p_ready_label,
      'claimedLabel', p_claimed_label
    ),
    'artifacts', '[]'::jsonb
  );
  final_result := jsonb_set(
    p_result,
    '{promotedJobId}',
    to_jsonb(promoted_job_id::text)
  );

  UPDATE agentops_control.job_leases
     SET status = 'completed', released_at = clock_timestamp()
   WHERE lease_token = p_lease_token;
  UPDATE agentops_control.job_attempts
     SET status = 'succeeded', finished_at = clock_timestamp(), error = NULL
   WHERE id = current_attempt_id;
  UPDATE agentops_control.jobs
     SET status = 'succeeded', result = final_result, failure = NULL,
         finished_at = clock_timestamp(), last_error = NULL,
         updated_at = clock_timestamp()
   WHERE id = current_job_id;

  INSERT INTO agentops_control.jobs(
    id, registration_id, registration_version, source_kind, source_key,
    idempotency_key, job_type, payload
  ) VALUES (
    promoted_job_id,
    current_registration_id,
    current_registration_version,
    'recovery',
    'triage-promotion:' || current_job_id::text,
    'triage-promotion:' || current_job_id::text,
    'agentops.runner',
    promoted_payload
  );

  UPDATE agentops_control.repository_registrations
     SET updated_at = clock_timestamp()
   WHERE id = current_registration_id;
  UPDATE agentops_control.lifecycle_state
     SET updated_at = clock_timestamp()
   WHERE singleton;
  INSERT INTO agentops_control.runtime_audit(
    actor_type, actor_id, event_type, registration_id, job_id, details
  ) VALUES (
    'triage', p_worker_id, 'triage.ready.promoted',
    current_registration_id, current_job_id,
    jsonb_build_object(
      'registrationVersion', current_registration_version,
      'repository', current_repository,
      'issueNumber', issue_number,
      'promotedJobId', promoted_job_id,
      'mergeMethod', current_merge_method
    )
  );
  RETURN promoted_job_id;
END
$$;

REVOKE ALL ON FUNCTION
  agentops_control.promote_triage_job(uuid, text, jsonb, text, text)
  FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentops_triage') THEN
    GRANT EXECUTE ON FUNCTION
      agentops_control.promote_triage_job(uuid, text, jsonb, text, text)
      TO agentops_triage;
  END IF;
END
$$;
