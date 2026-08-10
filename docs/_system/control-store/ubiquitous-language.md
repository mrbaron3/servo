# Control Store Ubiquitous Language

- **LANG-control-store-001 Registration** — repositoryのdesired control state。
- **LANG-control-store-002 Registration Version** — stale jobを拒否する単調増加revision。
- **LANG-control-store-003 Delivery Key** — provider webhook receiptの重複排除identity。
- **LANG-control-store-004 Idempotency Key** — webhook/pollを跨ぐ同一logical jobのidentity。
- **LANG-control-store-005 Active Job** — `queued`または`leased`のsingle-flight対象。
- **LANG-control-store-006 Lease** — expiry付きworker所有権。
- **LANG-control-store-007 Attempt** — reclaim後も残る1回の実行履歴。
- **LANG-control-store-008 Wake** — LISTEN/NOTIFYの非権威hint。
- **LANG-control-store-009 Reconciliation** — DBからdesired workを回収する周期query。
- **LANG-control-store-010 Escape（retired）** — panel approve済みbuildへrelease後defectを紐づける旧実験語。
  production writerが一度も接続されなかったため、migration 0027でactive modelから廃止した。既存行だけを
  `retired_released_builds` / `retired_build_defects`へread-only archiveとして保存し、旧severityは
  `blocker|major|minor`へ正規化した。archiveの0件をescape 0件という品質証拠にしてはならない。
- **LANG-control-store-011 Runner Job** — shell commandでなく、version付きrepository/event/ref/gate identity。
- **LANG-control-store-012 Critical Boundary** — claim/provider/push/merge/releaseの副作用直前の再認可点。
- **LANG-control-store-013 Execution Guard** — lease ownership/expiryとRegistration current stateをDBで裁定する操作。
- **LANG-control-store-014 Side-effect Permit** — guard成功から対応side effectまでを結ぶ短命・単回token。
- **LANG-control-store-015 Runner Failure** — code/retryable/boundary/timeを持つversioned failure。
- **LANG-control-store-016 Lifecycle State** — operatorが明示したOFF／MONITOR_ONLY／ACTIVE／DRAININGの永続運転状態。
  `lifecycle_state.mode`（**Lifecycle Mode**）だけが運転状態の正本である。process起動時の
  `LANG-registration-control-011` Startup ModeとAPI応答の`mode`は読み取り投影であり、この値を上書きしない。
- **LANG-control-store-017 Lifecycle Transition** — validation、idempotency、actor、時刻、結果を一体で保存する状態変更。
- **LANG-control-store-018 Drain Fence** — DRAINING後の新規routing/enqueue/leaseをDBで拒否する競合境界。
- **LANG-control-store-019 Recovery Reconciliation** — persisted mode/lease/attemptとactual topologyをrestart時に照合する操作。
- **LANG-control-store-020 Monitor Broker Request** — 固定repository/kind/cursorだけを持つprivate monitorのdurable read要求。
- **LANG-control-store-021 Monitor Broker Lease** — triage processが1 requestを期限付きで処理する所有権。expiry後は回収される。
- **LANG-control-store-022 Sanitized Monitor Response** — repository/kind/number/updatedAtとnext cursorだけのbounded応答。
- **LANG-control-store-023 Triage Job** — Issue identityと観測時刻だけを運ぶ非実行job。
- **LANG-control-store-024 Triage Decision** — North Star整合、readiness、priority、依存／重複／不足情報だけを持つ
  strict provider出力。ready approvalや任意label／commandを含まない。
- **LANG-control-store-025 Ready Promotion** — human-owned exact ready labelとlive triage leaseを再検証し、
  development jobへ進めるatomic capability。
- **LANG-control-store-026 Release Identity** — jobやretryの個数から独立した、一回のreleaseを指すdurable identity。
- **LANG-control-store-027 Receipt** — release/headへ束縛された一つのsemantic fact。job logや事後推測ではない。
- **LANG-control-store-028 Authority Route** — human ready単独、またはAI triage decision後のhuman readyという着手権威の経路。
- **LANG-control-store-029 Gate Signal Source** — 同名でも混同しない`repository-grader`または`github-check`の正本種別。
  `repository-grader`は`LANG-evaluation-019` Grader Profile由来で、`name`は
  `LANG-evaluation-020` Hard Gate Signal Nameの閉じた集合に限る。
- **LANG-control-store-030 Head Epoch** — buildでheadが変わるたびに進むreview単位。jobやattemptの回数ではない。
- **LANG-control-store-031 Finding Lineage Ref** — findingを挙げたreview/headと、それを解消したbuild/後続headを結ぶ
  `finding-origin-v1:<64hex>`形式の文字列参照。findingの由来分類やbranch DAG objectをこの語で呼ばない。
- **LANG-control-store-032 Release Certificate** — policyを満たすsame-release receiptとartifactだけからcertifierが導出する
  `passed|passed-with-interventions`の公開証拠。
- **LANG-control-store-033 Monitor Cursor Observation** — `monitor_cursors.observed_at`に保存する、monitor cursorを
  durableに前進させた時刻。poll attempt時刻ではない。MONITOR_ONLY、`executionEnabled=false`、enqueue busyによる
  cursor凍結では前進しないため、停滞をmonitor停止と解釈しない。API v1.4の正典keyは
  `cursorObservedAt`、UI表記は`cursor advanced`である。`lastPoll`は同じ値を返す1版限りのdeprecated aliasで、
  poll attemptを表さず、次API版で削除する。
- **LANG-control-store-034 Monitor Poll Attempt** — polling loopが1周した事実。
  `monitor_actual_states.observed_at`から観測し、modeにかかわらず更新される。死活判定はこちらを使う。
- **LANG-control-store-035 Development Phase** — 1つのIssueが現在いるdurableな開発段階。値集合の正本は
  `apps/agentops/src/domain/development-progress.ts`の`DevelopmentPhase`で、派生表示のKanban Laneと混同しない。
- **LANG-control-store-036 Development Progress State** — Phase内の`pending|running|waiting|blocked|succeeded|failed`。
  同ファイルの`DevelopmentProgressState`が正本で、Phaseと組で1つの進捗事実をなす。
- **LANG-control-store-037 Gate Key** — Issueが待っているgateの識別子。値集合の正本は
  `DevelopmentProgressUpdate.gateKey`で、`human_escalations`のSLAとone-shot identityはこのkey単位である。
- **LANG-control-store-038 Review Outcome** — review roundの進行／結果分類。
  durable roundの正典値は`DevelopmentReviewRound.outcome`の
  `running|approve|request_changes|escalated`で、migration 0024が旧`request-changes`行を更新した。
  progress eventの`DevelopmentProgressUpdate.reviewOutcome`はUI/event互換の`request-changes`を受け、
  永続round境界で明示翻訳する。perspective単位の`LANG-evaluation-007` Verdictとは粒度が異なる。
- **LANG-control-store-039 Finding Origin** — findingが初出か既出かを表す`new|persisted`の文字列分類。
  正本は`apps/agentops/src/domain/schema.ts`の`FindingLineage`。永続層はstring typeとenumを検証する。
- **LANG-control-store-040 Branch Lineage Node** — reviewから分離したchild workのDAG node
  (`development_lineage_nodes`)。`lineage`というobject語はこのbranch DAGだけに予約し、finding originやrefへ流用しない。
- **LANG-control-store-041 Pull Request Number** — GitHub Issue/PRの共有正整数値域に役割を与えたPR番号。
  正典名は`pullRequestNumber`、SQL列は`pull_request_number`、公開値域名は`$defs.githubNumber`。
  canonical `live-release-receipt` v4とexternal evidence v2がこの形を使う。旧`pullRequest`と
  `$defs.issueNumber`はimmutableなreceipt v2/v3・external evidence v1だけのlegacy名である。
- **LANG-control-store-042 Integration Strategy** — merge時の`squash|merge|rebase`選択を表す`mergeMethod`概念。
  control-plane経路ではRegistration `configuration.mergeMethod`がdurable authorityで、未指定時は`squash`。
  migration 0023がpromotion時に値をrunner payloadへ固定する。workspace `gate.mergeMethod`はRegistrationを持たない
  legacy TypeScript `github-turn`経路だけのfallbackで、Registration値を上書きしない。
- **LANG-control-store-043 Source Issue Closure** — release完了時にsource Issueが完了状態へ閉じた正規化事実。
  canonical receipt v4は`sourceIssueClosure: 'completed'`だけを保存し、GitHub GraphQLの
  `CLOSED`／`COMPLETED`は`apps/agentops/src/pipeline/execution/pr-native-github.ts`で一度だけ翻訳する。
  `release_source_issue_snapshots.state='open'`はclaim時点の入力snapshotという別事実で、closureの反対値ではない。

根拠: [ADR-0013](../../decisions/ADR-0013-postgresql-control-plane-source-of-truth.md)、
[ADR-0015](../../decisions/ADR-0015-postgresql-fenced-isolated-runner.md)、
[ADR-0017](../../decisions/ADR-0017-private-repository-monitor-broker.md)、
[ADR-0020](../../decisions/ADR-0020-release-receipt-evidence.md)、
[ADR-0023](../../decisions/ADR-0023-retire-legacy-escape-tables.md)
