# Control Store Domain Model

- **DOM-control-store-001 Registration** — repository desired stateと単調増加version。
- **DOM-control-store-002 Delivery** — transport receiptとconsumer別完了状態。
- **DOM-control-store-003 Job** — version付きenvelope、source、idempotency identity、queue status。
- **DOM-control-store-004 Lease / Attempt** — 時限worker所有権と、reclaim後も失われない実行履歴。
- **DOM-control-store-005 Released Build / Defect（retired）** — production writerを得なかった旧escape実験。
  migration 0027以降は`retired_*` historical archiveだけを残し、active aggregate／application APIとして扱わない。
- **DOM-control-store-006 Artifact Link** — artifact本体でなくURI/digest/size/timeだけを指すmetadata。
- **DOM-control-store-007 Runner Job Contract** — repository/event/ref/gate identityだけを持つversioned executable envelope。
- **DOM-control-store-008 Execution Guard** — lease/Registrationの現在値をcritical boundaryごとに裁定するtransactional verdict。
- **DOM-control-store-009 Side-effect Permit** — DB guard成功後、対応するpush/merge/releaseの1回だけ消費できる短命capability。
- **DOM-control-store-010 Lifecycle State** — OFF／MONITOR_ONLY／ACTIVE／DRAININGのsingleton aggregate。
- **DOM-control-store-011 Lifecycle Transition** — actorとidempotency identityを持ち、明示graphだけを進む監査entity。
- **DOM-control-store-012 Drain Fence** — DRAINING commitと新規routing/enqueue/leaseを直列化するDB権威の境界。
- **DOM-control-store-013 Monitor Broker Request** — 1 Registration revisionの1 typed cursor readを表すdurable entity。
  triage lease、stale Registration fence、bounded response、digest auditを一体で所有する。
- **DOM-control-store-014 Triage Job Contract** — repository／Issue number／observedUpdatedAtだけを持つ
  identity-only envelope。command、ref、clone URL、label、provider credentialを持たない。
- **DOM-control-store-015 Triage Promotion Capability** — exact human ready labelをGitHubで再確認したtriage leaseを
  完了し、設定済みready／claimed labelを持つdevelopment jobを同一transactionで作る限定DB capability。
- **DOM-control-store-016 Release** — Registration、repository、Issue、policy、final head、PR、merge resultを所有する
  durable aggregate。複数jobとattemptは同じReleaseへlinkする。
- **DOM-control-store-017 Release Receipt** — release内idempotency keyとcausal predecessorsを持つimmutable semantic fact。
  authority/build/grade/review/finding-resolution/runtime/merge/interventionをkindで区別する。
- **DOM-control-store-018 Merge Authorization** — required receipt集合をexpected headへ束縛したpre-merge verdict。
  authorizationなしのGitHub mergeはrelease成功として取り込まない。
- **DOM-control-store-019 Release Certificate** — outbox receiptとdigest-bound artifactから独立certifierが導出する
  published evidence。複数runtime producer、durable head epoch、artifact receipt bindingをPostgreSQLから再構成し、
  job topologyやjob-local DBを入力の必須条件にしない。新規wireはcanonical v4で、immutable v2/v3 artifactは
  legacy anti-corruption layerだけが読む。

不変条件: active jobはrepositoryごとに高々1件、active leaseはjobごとに高々1件、stale/disabled Registrationのjobは
claimしない。artifactはRegistration rootからreal pathで逸脱できず、lease loss後にside effect permitを消費できない。
monitor capabilityはtriage roleだけ、development lease／guard capabilityはrunner roleだけが持つ。共有する
jobs／attempts／leases tableでもRLSがjob typeを強制し、両roleは互いのjob rowを読書きできない。
根拠: [ADR-0013](../../decisions/ADR-0013-postgresql-control-plane-source-of-truth.md)、
[ADR-0015](../../decisions/ADR-0015-postgresql-fenced-isolated-runner.md)、
[ADR-0017](../../decisions/ADR-0017-private-repository-monitor-broker.md)、
[ADR-0020](../../decisions/ADR-0020-release-receipt-evidence.md)、
[ADR-0023](../../decisions/ADR-0023-retire-legacy-escape-tables.md)
