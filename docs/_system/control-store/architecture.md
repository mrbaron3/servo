# Control Store Architecture

- **ARCH-control-store-001 PostgreSQL SoT** — CISO control-planeのdurable stateは`agentops_control`だけへ書く。
  evaluation domainのJSON storeとはentity境界を共有しない（ADR-0013）。
- **ARCH-control-store-002 Version Gate** — consumer/runnerは接続後、既知の連番version/name/checksumが完全一致するまで
  起動しない。migrationとverifyは同じadvisory lockで直列化し、migrationは単一transactionでのみ進める。
- **ARCH-control-store-003 Transactional Repositories** — Registration、cursor、delivery/consumer、job、lease/attempt、
  audit、artifact metadata、release receiptを同じDB transaction境界で操作する。旧build defect modelは
  migration 0027でread-only archiveへretire済みで、production repository APIを持たない。
- **ARCH-control-store-004 Durable Queue** — registration-scoped idempotency/source unique制約とrepository partial unique
  indexをDB権威とし、runtime rejectionを補助に置く。
- **ARCH-control-store-005 Lease Competition** — workerは`FOR UPDATE SKIP LOCKED`で1 jobだけをclaimし、heartbeat/expiry/
  reclaimをattempt historyと原子的に更新する。webhook routingもtoken＋expiry＋heartbeatでlive ownerとcrashを区別する。
- **ARCH-control-store-006 Wake + Reconcile** — job/registration/webhookのLISTEN/NOTIFYはhint、周期queryと
  pending webhook claimは真実回収経路である。
- **ARCH-control-store-007 Published Contract** — SQL migrationとv1 JSON Schema/fixtureがTypeScript/Go間のPublished
  Languageである。
- **ARCH-control-store-008 Registration control projection** — Go controlはactual state、API idempotency、
  delivery retryをschema version 2へtransactionalに保存し、status readをrepeatable-read snapshotから構成する。
- **ARCH-control-store-009 Runner Published Contract** — `agentops.runner`だけがstrictなversion 1 payload/result/failureを
  実行でき、未知version・余分field・command/credential/host pathは実行前に拒否する。
- **ARCH-control-store-010 Critical Boundary Fence** — claim/provider/push/merge/releaseの各直前にactive lease ownership、
  DB clock expiry、job status、Registration version/enabled/execution_enabledをrow lock下で再検証し、allow/deny理由を
  同じtransactionへ監査する。
- **ARCH-control-store-011 Retry and Restart Recovery** — heartbeat loss後はside effect permitを失効し、retryable failureは
  attempt履歴を閉じて指数backoff再queueする。stale Registrationはretryせず、restart workerはDB queueを再発見する。
- **ARCH-control-store-012 Lifecycle SoT** — singleton mode/generation/drain deadline/timeout/last errorと
  idempotent transition履歴をPostgreSQLだけへ保存し、actual container状態をmodeとして推測保存しない。
- **ARCH-control-store-013 Drain Fence** — lifecycle rowのexclusive transition lockとenqueue/leaseのshared lock、
  job INSERT triggerを組み合わせ、DRAINING commit後のrouting/enqueue/leaseを原子的に拒否する。
- **ARCH-control-store-014 Recovery Reconciliation** — CLI restartはpersisted mode、active lease、running attempt、
  actual containerを照合し、欠損ACTIVEをDRAININGへ寄せてから安全な復旧経路だけを通す。
- **ARCH-control-store-015 Explicit Owner Migration** — 通常consumerはschema verify-onlyを保ち、短命なowner-only
  admin containerだけがadvisory lock下でcurrent imageが宣言するexact schema
  （2026-08-06時点はversion 27）までのadditive migrationとleast-privilege role bootstrapを行う。
  commit前failureは旧versionを保持し、commit後はdurable rowを消すdown migrationでなくsafe modeへのcompensationと
  current imageによるforward recoveryを行う。
- **ARCH-control-store-016 Typed Private Monitor Broker** — credential-free controlはRegistration/version、固定repository、
  issue/PR kind、strict cursorだけをdurable requestへ保存し、credential-bearing triage roleがleaseしてsanitized identityだけを
  応答する。stale/disabled/out-of-allowlist requestはprovider call前に拒否し、expiry後の回収とresponse digestを監査する。
  provider operationはtotal deadline/page/item/raw-byte上限を持ち、claim/failure persistence障害はexecution serviceから
  隔離する。Issue identityはstrict triage jobへ入り、AIはready approvalを作れない。human exact ready label後だけ、
  live triage leaseの完了とconfigured-label付きdevelopment job enqueueをSECURITY DEFINER capabilityでatomicに行う。
- **ARCH-control-store-017 Release Identity Boundary** — 一回のreleaseはRegistration内のdurable identityであり、
  triage/promotion、development、PR reconciliation、review、merge、retry/recovery jobを横断する。job/attemptは
  producer provenanceであってrelease identityではない。
- **ARCH-control-store-018 Causal Receipt Outbox** — authority/build/grade/review/finding/runtime/merge/interventionの
  immutable receiptをtransactional outboxへ保存し、same-release causeとrepository/Issue/head座標をDB制約と
  certifierで再強制する。runtimeはjobごとに分割でき、head epochは`release_heads`、大きなartifactは
  `release_artifacts`のURI/digest/receipt linkだけを参照する。
- **ARCH-control-store-019 Pre-merge Certification** — policyが要求するauthority、gate source、review perspective、
  head epoch、finding resolution、runtime provenanceをmerge前に独立certifyし、merge intentとauthorizationを
  atomic commitする。同一expected-head mergeのrecoveryは冪等、未認可または異なるmergeはfail closedである。
  completed releaseは`evidence:live-release:export`でPostgreSQLだけからcanonical v4 certificateへ再構成し、
  同じsemantic certifierを通過しない出力を公開しない。requirements authority導入前のreleaseだけは、
  historical v2 schemaに適合する明示的なlegacy wireへ逆変換し、canonical fieldとの混在を許さない。
- **ARCH-control-store-020 Cross-application durable coordination** —
  `apps/control-plane`のGo consumerと`apps/agentops`のTypeScript consumerは、root
  `db/control-store/migrations/`と`contracts/`へ順応し、Registration/lifecycle fence/job/lease/result/progress/receipt/
  artifact metadataをPostgreSQLだけでdurableに調整する。`LISTEN/NOTIFY`はwakeに限定する。
  credential broker HTTP、CONNECT egress proxy、runner shared volume、`agentopsctl`のactual container操作は別の
  security/runtime contractであり、control-storeへ統合しない。`.harness/db.json`はTypeScript evaluation
  domainの別SoTで、Go consumerは読まない。exact schema gateを保つ間のrelease unitはrepository全体で一体とする
  （ADR-0021）。

根拠: [ADR-0013](../../decisions/ADR-0013-postgresql-control-plane-source-of-truth.md)、
[ADR-0015](../../decisions/ADR-0015-postgresql-fenced-isolated-runner.md)、
[ADR-0017](../../decisions/ADR-0017-private-repository-monitor-broker.md)、
[ADR-0020](../../decisions/ADR-0020-release-receipt-evidence.md)、
[ADR-0021](../../decisions/ADR-0021-go-typescript-application-boundaries.md)、
[ADR-0023](../../decisions/ADR-0023-retire-legacy-escape-tables.md)
