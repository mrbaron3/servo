-- The released_builds/build_defects experiment never gained a production
-- writer. Preserve any historical rows as a read-only archive and retire the
-- misleading active-table/API names. Durable releases and release receipts
-- remain the production evidence model.

LOCK TABLE agentops_control.released_builds,
  agentops_control.build_defects IN ACCESS EXCLUSIVE MODE;

ALTER TABLE agentops_control.build_defects
  DROP CONSTRAINT build_defects_severity_check;

UPDATE agentops_control.build_defects
   SET severity = CASE severity
     WHEN 'low' THEN 'minor'
     WHEN 'medium' THEN 'major'
     WHEN 'high' THEN 'blocker'
     WHEN 'critical' THEN 'blocker'
   END;

ALTER TABLE agentops_control.build_defects
  ADD CONSTRAINT retired_build_defects_severity_check CHECK (
    severity IN ('blocker', 'major', 'minor')
  );

ALTER TABLE agentops_control.build_defects
  RENAME TO retired_build_defects;
ALTER TABLE agentops_control.released_builds
  RENAME TO retired_released_builds;

COMMENT ON TABLE agentops_control.retired_released_builds IS
  'Read-only historical archive; no production writer ever existed. Use releases and release_receipt_outbox.';
COMMENT ON TABLE agentops_control.retired_build_defects IS
  'Read-only historical archive; severity was normalized during retirement. Do not interpret absence as zero escapes.';

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentops_control') THEN
    REVOKE ALL ON TABLE agentops_control.retired_released_builds,
      agentops_control.retired_build_defects FROM agentops_control;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentops_runner') THEN
    REVOKE ALL ON TABLE agentops_control.retired_released_builds,
      agentops_control.retired_build_defects FROM agentops_runner;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentops_triage') THEN
    REVOKE ALL ON TABLE agentops_control.retired_released_builds,
      agentops_control.retired_build_defects FROM agentops_triage;
  END IF;
END
$$;
