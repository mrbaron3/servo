# Control Store Data Model

- **DATA-control-store-001** `repository_registrations` — canonical repository、desired switches、version。
- **DATA-control-store-002** `monitor_cursors` — Registration/monitor kindごとの単調な最後の観測点。
- **DATA-control-store-003** `webhook_deliveries` / `webhook_consumers` — receipt dedup、allowlist済みheader、
  consumer state、pending claim、routing ownership token/expiry。
- **DATA-control-store-004** `jobs` — v1 envelope、registration-scoped source/idempotency unique、repository active
  partial unique。
- **DATA-control-store-005** `job_leases` / `job_attempts` — active token、heartbeat/expiry、再試行履歴。
- **DATA-control-store-006** `runtime_audit` — actor/event/entityへのappend-only監査。
- **DATA-control-store-007** `artifact_links` — URI、SHA-256、size、createdAtだけ。
- **DATA-control-store-008** `retired_released_builds` / `retired_build_defects` — production writerを持たなかった
  escape実験のread-only historical archive。migration 0027が旧active名を廃止し、severityを
  `blocker|major|minor`へ写像してapplication roleの権限を剥がす。absenceを品質指標に使わない。
- **DATA-control-store-009** `schema_migrations` — version、filename、SHA-256 checksum。
- **DATA-control-store-010** `monitor_actual_states` — Registration version付きcomponent actual state、
  supervisor、observed/healthy/error。
- **DATA-control-store-011** `control_api_requests` / `delivery_retry_attempts` — command idempotency、
  request hash/response、operator retry attempt。
- **DATA-control-store-012** runner job `payload` / `result` / `failure` — strict version 1 JSON contract。
- **DATA-control-store-013** runner attempt `failure` — retry/restart後も残るtyped failureとboundary。
- **DATA-control-store-014** runner artifact/audit — Registration-scoped volume URI＋digest/size/time、および
  boundaryごとのallow/deny reason。artifact bytes、credential、test output本文はDBへ入れない。
- **DATA-control-store-015** `lifecycle_state` — singleton mode、generation、transition identity/time、
  drain deadline/timeout、redacted last error。
- **DATA-control-store-016** `lifecycle_transitions` — unique idempotency key、actor、from/to、applied/idempotent/rejected/
  compensated status、deadline、error、details、開始/完了時刻を持つ監査履歴。
- **DATA-control-store-017** `monitor_broker_requests` — Registration/version、固定repository、issue/PR kind、
  strict cursor/digest、pending/leased/succeeded/failed state、worker/lease expiry、sanitized responseまたはbounded error。
  active cursorのpartial unique indexとclaim order indexを持ち、credential/provider本文は保存しない。
- **DATA-control-store-018** triage job `payload` / `decision` / `result` — payloadはrepository／Issue identityだけ、
  decisionはbounded classificationだけ、resultはsource digest／managed mutation／promotion identityだけを持つ
  strict version 1 JSON contract。`promote_triage_job`はready／claimed labelをdevelopment payloadへ写し、
  triage完了とrunner enqueueをatomicにする。jobs／attempts／leases／artifact linksのRLSは
  `agentops_triage=agentops.triage`、`agentops_runner=agentops.runner`をDBで再強制する。
- **DATA-control-store-019** `releases` — Registration-scoped release key、repository/Issue、policy、
  `collecting|merge-authorized|merged`、PR/final head/merge SHA/actor/timeを持つ。
- **DATA-control-store-020** `jobs.release_id` — topologyを同一性へ昇格させず、任意個のproducer jobを一つのreleaseへ
  linkするnullable foreign key。
- **DATA-control-store-021** `release_receipt_outbox` — immutable receipt payload、release内key、kind、head、cause IDs、
  recorded/published time。causeは同じreleaseの既存receiptだけを指し、authority/merge intent/mergeは
  releaseごとに高々一件、runtimeはproducer jobごとに複数件を許す。migration 0026以降の新規payloadは
  canonical receipt v4語彙（`invocationKey` / `invocationRef`、3値Verdict＋`hasFindings`、
  `pullRequestNumber`、`sourceIssueClosure`）だけを受ける。既存v2/v3 wireは書き換え前のschemaで検証可能な
  historical artifactとして保持する。
- **DATA-control-store-022** Registration `configuration.releaseEvidence` — authority route、required gate signals、
  required review perspectives、minimum head epochsを固定するopt-in policy。未設定Registrationはv1-compatibleであり、
  v2 releaseのreceipt欠落をv1 artifactから補完しない。同じRegistration configurationの`mergeMethod`は
  migration 0023以降のIntegration Strategy authorityで、未指定を`squash`としてpromotion payloadへ固定する。
- **DATA-control-store-023** `release_heads` — release内のhead SHA、単調なhead epoch、同一releaseの先行parent headを
  保持する。job/retry順序からreview epochを推測しない。
- **DATA-control-store-024** `release_artifacts` — release/final head/receipt IDsへ束縛したartifact key、URI、digest、size。
  bytesはDBへ入れず、未知receipt、重複receipt ID、別headをconstraint triggerとstoreで拒否する。
- **DATA-control-store-025** `development_progress_events` — job/headに束縛したphase、step、state、gate、review、
  worktree/branch/PR等のcurrent-progress fact。入力契約の正本は
  `apps/agentops/src/domain/development-progress.ts`の`DevelopmentProgressUpdate`。
- **DATA-control-store-026** `human_escalations` — gate/headごとに高々1件のSLA超過とhuman actionを保存し、
  gate進行またはterminal factでresolveする。`gate_key`の値域はzod
  `DevelopmentProgressUpdate.gateKey`と一致する。独立した全体zod objectは現行実装に存在せず、残りのshapeは
  `db/control-store/migrations/0019_durable_kanban_gates.sql`と
  `apps/control-plane/internal/control/store.go`が共有する現行contractである。
- **DATA-control-store-027** `development_review_rounds` — immutable headに対するround、branch、PR、outcome、
  start/completionを保持する。入力shapeの正本は
  `apps/agentops/src/domain/development-review.ts`の`DevelopmentReviewRound`。outcomeはmigration 0024以降
  `running|approve|request_changes|escalated`で、旧hyphen行は同migrationが更新する。
- **DATA-control-store-028** `development_review_perspectives` — round内のperspective、Verdict、finding配列を保持する。
  正本は同ファイルの`DevelopmentReviewPerspective`と`DevelopmentReviewFinding`。findingの`lineage`は
  `new|persisted`、`lineageRef`は`finding-origin-v1:<64hex>`の文字列としてmigration 0022が型を検証し、
  migration 0024が`persisted`との必須pairingと`new`での禁止を検証する。
- **DATA-control-store-029** `development_lineage_nodes` — separate-issue child branchのparent/head、PR、status、
  source findingを保持するDAG。runnerへ公開するzod投影は
  `apps/agentops/src/control-store/types.ts`の`RunnerJobPayloadV1Contract.lineage`で、durable全shapeは
  `db/control-store/migrations/0021_review_rounds_and_branch_dag.sql`が正本である。migration 0025以降、node自身の
  列と全JSON深度は`head_sha` / `pull_request_number`、`headSha` / `pullRequestNumber`で統一し、親座標だけを
  `parent*`で修飾する。
- **DATA-control-store-030** `release_source_issue_snapshots` — release claim時に固定したsource Issue入力の
  repository/number/title/body/author/state/updatedAt/digest。ここでの`state='open'`は「claim時点で入力Issueがopen」
  というsnapshot factであり、merge後の`LANG-control-store-043` Source Issue Closureとは別field・別時刻・別責務である。
  completion時にこのrowを`completed`へ更新せず、closureはmerge receiptへ保存する。

根拠: [ADR-0013](../../decisions/ADR-0013-postgresql-control-plane-source-of-truth.md)、
[ADR-0015](../../decisions/ADR-0015-postgresql-fenced-isolated-runner.md)、
[ADR-0017](../../decisions/ADR-0017-private-repository-monitor-broker.md)、
[ADR-0020](../../decisions/ADR-0020-release-receipt-evidence.md)、
[ADR-0023](../../decisions/ADR-0023-retire-legacy-escape-tables.md)
