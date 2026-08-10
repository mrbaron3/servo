# コンテキストマップ（macro 索引・keystone）

> system の木の入口。全**境界コンテキスト**の一覧と、その**関係**を DDD の関係パターンで1枚にする
> （DOC_TAXONOMY §索引）。「どの境界がどの言語で話し、どこで翻訳されるか」を示す。各コンテキストの内実は
> `_system/<ctx>/` に住む。語はコンテキストで意味が変わるので、グローバル1冊の用語集は持たない
> （per-context の `ubiquitous-language.md` ＋ ここでの翻訳点）。

- 種別: system（macro 索引）
- 状態: ドラフト（planning・execution・workspace・agent-runtime・intake・container-runtime・registration-control は4ビュー実体化済み・他は移行待ち）

## 物理アプリケーション境界

境界コンテキストはドメイン言語の地図であり、deployable application の境界とは一致しない。この repository の
物理的な application ownership は次の2つに固定する。

| 物理境界 | 所有するもの | 所有しない共有物 |
| --- | --- | --- |
| `apps/control-plane/` | Go の Control API、Registration supervision、GitHub credential broker、egress proxy、`agentopsctl` lifecycle | PostgreSQL migration、published JSON/OpenAPI contract、OCI composition |
| `apps/agentops/` | TypeScript の evaluation harness、triage、isolated runner、runtime adapter | PostgreSQL migration、published JSON/OpenAPI contract、OCI composition |

`db/` と `contracts/` は両 application が順応する language-neutral な共有境界、`deploy/` は両 application と
PostgreSQL を同じ OCI topology に組み立てる統合層である。root の `package.json` と `go.work` は
developer command/workspace router であり、application source の所有者ではない。image構成専用tool
（`provider-cli`・`gh`・`gosu`）も`deploy/tools/`へ置く。

cross-application の durable business coordination（lifecycle mode/drain fenceを含む）は
PostgreSQL `agentops_control` が唯一の正本である。
一方、credential broker HTTP、CONNECT egress proxy、runner shared volume、`agentopsctl` による
actual container lifecycle操作は
別の security/runtime contract であり、DB bridge には含めない。directory 分離後も exact schema/checksum gate と
topology orchestrationを保つため、当面の release unit は repository 全体で一体とする
（[ADR-0021](decisions/ADR-0021-go-typescript-application-boundaries.md)）。

### 製品・実行系の正典名

| 対象 | 正典 | 互換名の扱い |
| --- | --- | --- |
| 製品、repository、人間向け文書 | **Servo** / `mrbaron3/servo` | `workflow` / `AgentOps`を製品名として新規使用しない |
| 実行系component、CLI、環境変数 | `agentops` / `agentopsctl` / `AGENTOPS_*` | technical prefixとして維持し、製品名と同一視しない |
| application root | `apps/control-plane/` / `apps/agentops/` | ADR-0021以前の旧pathは歴史記録に限る |
| container label | 現行authorityは`com.mrbaron3.workflow.*` | `com.mrbaron3.servo.*`移行は新旧併記→旧掃討→新のみの独立3段階issue。片側変更禁止 |
| schema `$id` | `https://github.com/mrbaron3/servo/contracts/**` | repository内consumerを照合して旧`workflow`識別子をretire済み。schemaの値域・versionとは別のidentity変更 |

判断根拠は[ADR-0022](decisions/ADR-0022-servo-product-and-agentops-component-naming.md)。

## 境界コンテキスト一覧

ハーネスを「1つのユビキタス言語が一貫する範囲」で切ると12。技術レイヤー（Product/…）ではなく
**言語の境界**で分ける。**execution は旧 ARCHITECTURE の技術層「Execution」ではない**——orchestration /
session / panel / sentinel という固有のユビキタス言語を持つ境界として立てる（[ADR-0005](decisions/ADR-0005-execution-layer-tmux-orchestration.md)）。
evaluation の採点語（Scorecard・Verdict）は所有せず参照する。

| コンテキスト | 責務（一文） | 主なコード | system 層 | 代表用語 |
| --- | --- | --- | --- | --- |
| **workspace** | 1つの組織storeを1つのtarget repositoryへ耐久的に束縛し、異targetへの状態変更を拒否 | `apps/agentops/src/workspace/`・`apps/agentops/src/store/`・`apps/agentops/src/config.ts` | `_system/workspace/` ✅ | Workspace・Target Identity・Target Binding・Binding Mismatch |
| **planning** | 製品ゴールを roadmap→epic→feature に分解し永続、各 feature を spec へ materialize | `apps/agentops/src/planning/` | `_system/planning/` ✅ | Roadmap・Theme・Epic・Feature・Outcome・取込・spawn |
| **authoring** | 人間が spec の WHAT（AC）を著述し**署名**、署名後のドリフトを検知 | `apps/agentops/src/authoring/` | `_system/authoring/`（言語のみ） | Spec・acceptance.yaml・ApprovedSpecRef・署名・fingerprint・ドリフト |
| **design** | 署名 spec から system 層（4ビュー）を設計し、PR サイズの Issue 集合へ分解 | `apps/agentops/src/design/`・`apps/agentops/src/planning/planning-tree.ts:spawnIssues` | `_system/design/`（言語のみ・実装被覆未確認） | 境界コンテキスト・ドメインモデル・seam・被覆×排他・dependsOnSystem |
| **evaluation** | Issue Contract を生成→評価→修正→リリースし、証拠から指標・回帰・改善を育てる | `apps/agentops/src/pipeline/`・`apps/agentops/src/graders/`・`apps/agentops/src/metrics/`・`apps/agentops/src/agents/`・`apps/agentops/src/resolve/` | `_system/evaluation/`（言語・アーキのみ） | Issue Contract・Generator・PR・Scorecard・EvalRun・pass@k・Repair・Curator・Analyst |
| **execution** | issue queue を入力に、issue ごとの実装を role-scoped tmux セッションのオーケストレーションで自律に進める独立層 | `apps/agentops/src/pipeline/execution/`・`apps/agentops/src/agents/` | `_system/execution/` ✅ | 実装層・Issue Queue・Orchestrator・Watch・Session・Sentinel・Evaluator Panel・観点・審査ゲート・Scoping Guard |
| **agent-runtime** | planning/UI-design/generation/reviewのAI呼出しを共通identityで監査し、provider/model固有差をadapterとrouteへ閉じる | `apps/agentops/src/agents/`・`apps/agentops/src/pipeline/execution/` | `_system/agent-runtime/` ✅ | Agent Invocation・Provider・Model・Role・Perspective Route・Provider Adapter |
| **intake** | target GitHub Issueをclaimし、planningと条件付きUI著述をtrace/provenance gate経由でIssueへ投影する | `apps/agentops/src/intake/` | `_system/intake/` ✅ | Source Issue・Acceptance Trace・UI Design Artifact・Claim・Enrichment |
| **webhook** | GitHub deliveryをdurable inboxへ受け、複数repoをtransport非依存eventとしてconsumerへrouteし、pollで照合する | `apps/agentops/src/webhook/`（compatibility oracle）・`apps/control-plane/internal/control/`（production） | `_system/webhook/` ✅ | Delivery Envelope・Durable Inbox・Signed Webhook Ingress・Normalized Event・Reconciliation |
| **container-runtime** | 標準OCIアプリイメージをbuildし、Apple Container等の**コンテナ**runtime操作をadapter境界へ隔離、network/volume/port/capabilityを起動前にfail-closed検査する（AC-CISO-011） | `apps/agentops/src/runtime/`・`deploy/Containerfile` | `_system/container-runtime/` ✅ | Container Runtime Adapter・Standard OCI Image・Runtime Preflight・Publish Invariant・Container-Neutral Path |
| **control-store** | Registration、delivery、job、lease、attempt、audit、review、release evidenceをPostgreSQL transactionへ永続化し、queue/single-flight/recoveryを保証 | `apps/agentops/src/control-store/`・`apps/control-plane/internal/control/`・`db/control-store/`・`contracts/control-store/` | `_system/control-store/` ✅ | Registration Version・Lifecycle Mode・Monitor Cursor Observation・Development Phase・Receipt |
| **registration-control** | PostgreSQL RegistrationをControl API・Issue/PR monitor・signed ingress・durable routerのdesired/actualへ動的収束 | `apps/control-plane/cmd/agentops-control/`・`apps/control-plane/internal/control/`・`apps/control-plane/internal/designgate/` | `_system/registration-control/` ✅ | Desired State・Actual State・Startup Mode・Dynamic Supervision・Convergent Work Identity |

**TypeScript 内の共有カーネル（Shared Kernel）**: `apps/agentops/src/domain/`（`schema.ts` の zod 契約 ＋
`states.ts` の状態機械）と `apps/agentops/src/store/`（Eval Result DB）は AgentOps 内の状態コンテキストが共有する。
Go application との共有カーネルではない。cross-application の Published Language は root の `db/` と
`contracts/` である。`apps/agentops/src/cli/`・`dashboard/`・`config.ts`・`util/` は
プレゼンテーション/インフラでドメイン境界ではない。

## 関係（DDD 関係パターン）

```text
                 ┌──────────────── workspace: store ↔ target binding ────────────────┐
                 ▼                                                                    ▼
       ┌──────────────────── 改善フィードバック（評価→計画）──────────────────────────────┐
       ▼                                                                                 │
 ┌──────────┐ spec stub ┌──────────┐ 署名spec ┌────────┐ issues  ┌───────────┐  駆動  ┌────────────┐
 │ planning │ ────────▶ │authoring │ ───────▶ │ design │ ═══════▶│ execution │ ─────▶ │ evaluation │
 └──────────┘   C/S     └──────────┘ C/S順応  └────────┘ queue   └───────────┘  SK    └────────────┘
       │                     │                   │       =ACL         │                     │
       └─────────────────────┴──── Shared Kernel: domain（zod 契約・状態機械）＋ store ───────┴─────────┘
```

> `design ═▶ execution` の二重線は**層境界（ACL）**＝issue queue。execution は queue を poll して消費し、
> evaluation の役割（Generator/Evaluator）を tmux セッションとして**駆動**する（Shared Kernel）。

- **planning → authoring**（Customer-Supplier）: planning が feature ごとに**著述 stub spec** を供給し、authoring が AC を著述・署名する。
- **workspace → 全状態変更コンテキスト**（Shared Kernel guard）: workspace がstoreとtargetの対応を検証してから
  target rootを供給する。各コンテキストはconfig pathを独自解釈せず、Binding Mismatchでは書込み前に停止する。
- **authoring → design**（Customer-Supplier・Conformist）: design は**署名された AC 集合に順応**する——issue の被覆は署名 AC とちょうど一致せねばならない（勝手に AC を足さない）。
- **design → execution**（Customer-Supplier・issue queue が ACL）: design が署名 spec を Issue（被覆・seam 参照）へ分解し供給、execution が **contract-drafted かつ ai-managed** な issue を queue から poll して消費する。橋は `contract-draft`（署名 spec の AC を契約へ・新規著述しない）。実装層は「issue がどう作られたか」に依存しない（`_system/execution/`・ADR-0005）。
- **execution ↔ evaluation**（Shared Kernel・execution が駆動）: execution は evaluation の Generator/Evaluator/grader を **role-scoped tmux セッション**として起動・fan-in し、観点パネルで採点を束ねる。採点の意味論（hard-gate→score・Scorecard・Verdict）は evaluation が所有し、execution は再定義せず参照する。
- **execution → agent-runtime**（Customer-Supplier）: executionはrole/perspective/cwd/evidence契約を供給し、
  agent-runtimeがprovider固有起動へ翻訳してInvocation Provenanceをstoreへ投影する。executionはClaude/Codexの
  command形を知らず、agent-runtimeはqueue・panel verdictを決めない。
- **GitHub → intake → planning**（ACL・Customer-Supplier）: intakeはreadyなGitHub Issueを外部投影としてclaimし、
  immutable原文snapshotをplanningへ供給する。planningがIssue Contract-readyへ昇格するまでexecution queueへ入れない。
- **GitHub → webhook → intake/execution**（ACL・Published Language）: webhookはdeliveryを保存・重複排除し、
  Normalized GitHub Eventだけをintake/PR revision loopへ渡す。productionの翻訳点は
  **Normalized GitHub Event ≡ `WorkItem` ≡ Convergent Work Item**であり、Go実体は
  `apps/control-plane/internal/control/model.go`が所有する。payloadはtriggerであり真実ではないため、
  consumerはcurrent snapshotを再取得する。pollは同じseamへreconciliation eventを供給する。
- **planning → intake UI authoring → execution**（Customer-Supplier・ACL）: frontend/fullstack Candidateだけを専用
  ui-designer sessionへ渡し、AC-traceableなUI Design Artifactを検証する。不在・曖昧・不正はqueueへ投影せず、
  accepted artifactはIssueのPublished Languageとしてgeneratorと各reviewerへ渡す。この経路は
  ADR-0012移行中のlegacy providerであり、標準経路は次項へ移す。
- **Designflow Provider → intake/planning**（Open Host Service・Published Language・ACL）:
  intakeはversioned Design Requestだけを外部providerへ渡し、Design Bundle、preview、Capability Requirements、
  digest-bound Human Design Decisionを受ける。providerの内部model/store/runtimeは共有しない。planningは
  approve済みcapabilityを最終Issue Contract/API設計へreconcileし、intakeの決定論gateがschema/digest/trace/
  decision/coverageを検証してからexecutionへ投影する（ADR-0012）。CISO-03/05の
  `apps/control-plane/internal/designgate`と
  固定Dashboard bundleはこの境界のgrounded bootstrapであり、汎用intake portそのものではない。
- **evaluation → planning**（改善フィードバック・Customer-Supplier）: Harness Analyst が `type:harness`/`type:eval` の改善 issue を計画の木へ戻し、Curator が失敗を回帰として育てる。北極星の「改善」軸の閉路。
- **全稼働コンテキスト → container-runtime**（Customer-Supplier・OS非依存port）: 稼働コンテキストは role／topology／container-neutral path 契約を供給し、container-runtime が標準 OCI イメージと Apple Container 等の runtime 操作へ翻訳する。core は Provider CLI 形や macOS 詳細を知らず、Apple Container 固有は adapter 配下のみ（AC-CISO-011）。CISO epic #10 の #12 以降はこの port 越しに runtime を使う。**agent-runtime（AI呼出し）とは別境界**で、橋に現れる共有語は`runtime`と`Provider`の2つだけ。`Provider`はagent-runtimeのAI tool familyを指し、container runtime engineの別名ではない。
- **registration-control/runner → control-store**（Customer-Supplier・Published Language）:
  `apps/agentops` のTypeScript runnerと `apps/control-plane` のGo controlは、root `db/` / `contracts/` の
  version付きSQL/JSON Schema契約へ順応し、Registration/cursor/delivery/lifecycle fence/job/lease/result/progress/receipt/
  artifact metadataをPostgreSQL transactionで共有する。これがcross-applicationの唯一のdurable business
  coordinationである。LISTEN/NOTIFYはwakeだけ、periodic reconciliationは真実回収経路として残す
  （AC-CISO-003〜005、ADR-0021）。運転状態の正本はcontrol-storeのLifecycle Modeであり、
  registration-controlのStartup Modeは起動時観測に限ってLifecycle Modeを上書きしない。
- **evaluation ↔ control-store**（Published Language・翻訳）: evaluationのVerdict正典は
  `approve|request_changes|needs_human`。`development_review_perspectives.verdict`は3値を保持する一方、
  canonical release receipt v4とexternal evidence v2も`verdict`の3値を保持し、finding有無は`hasFindings`へ
  分離する。旧receipt v2/v3の`approved|findings`とexternal evidence v1の`no_findings|findings`はimmutableな
  historical wireだけのlossy投影であり、新規判断へ戻さない。特に`needs_human`と`request_changes`を同じ判断と
  解釈しない。
- **runtime integration（別 security/runtime contract）**: GitHub credential broker HTTP＋credential helper、
  CONNECT egress proxy、runner shared volume、`agentopsctl`のactual container操作はPostgreSQLを迂回する意図的な
  point-to-point境界である。business resultをこれらへ永続化せず、volume artifactのURI/digest/receipt linkだけを
  control-storeへ記録する。
- **Experience Provider → registration-control**（Customer-Supplier・Published Language）: pinned provider bundleの
  human-approved revision/digestとCapability RequirementだけをControl API contractへ取り込み、API/system/Issue ACの
  lineageが欠ける入力を起動前に拒否する（AC-CISO-001〜002、ADR-0014）。
- **Shared Kernel**: AgentOps内の状態コンテキストは `domain`（契約・状態機械）と `store`（Eval DB）を共有する。
  application境界を跨ぐ語彙はroot `db/` / `contracts/`に限定し、`.harness/db.json`をcontrol-planeと共有しない。

## GLOSSARY.md / ARCHITECTURE.md 移行マップ

旧モノリシック2文書は理想ツリーに居場所が無い（DOC_TAXONOMY §理想ツリー）。中身を以下へ分解し、分解後に retire する。

### `docs/GLOSSARY.md` → per-context 言語 ＋ ここ

| 旧 GLOSSARY 用語 | 移行先 |
| --- | --- |
| Roadmap・Theme/Initiative・Epic・Feature・計画の木・planRoadmap/spawnSpecs | `_system/planning/ubiquitous-language.md`（Themeを正典化。Initiativeは実体のないlegacy alias。残りの実装対応は未監査） |
| Spec・spec.md・acceptance.yaml・ApprovedSpecRef・contract-approved・manual-requirements | `_system/authoring/ubiquitous-language.md`（言語移行済み。manual-requirements/MR-IDは実体がないためManual Verification Exclusionへ是正） |
| 被覆×排他・seam・DesignSlice・ArchitectureSpine（※レガシー語は整理）・IssueSpawnOrder | `_system/design/ubiquitous-language.md`（言語fileあり。実装被覆と陳腐化語の整理は未監査） |
| Issue Contract・Agent Work Unit/sample・PR・Eval Run・Scorecard・Repair Loop・Eval Task Registry・Grader・Evidence・pass@k/pass^k・False pass/fail・Harness improvement・build-approved | `_system/evaluation/ubiquitous-language.md`（言語・architectureあり、domain/data viewは未実体化） |
| Agile Sprint（時間箱）等の横断・翻訳点 | このコンテキストマップ（翻訳点として明記） |

### `docs/ARCHITECTURE.md` → context-map ＋ per-context architecture ＋ ADR

| 旧 ARCHITECTURE セクション | 移行先 |
| --- | --- |
| Layers（Product/Planning/Execution/Evaluation/Learning）= 技術レイヤーの俯瞰 | このコンテキストマップ（C4 文脈＝コンテキスト関係に再表現） |
| Data flow（one issue）= 評価ループの流れ | `_system/evaluation/architecture.md`（未・シーケンスは派生） |
| Extension seams（runner/store/grader/persona/metric の拡張点） | 各コンテキストの `architecture.md` の seam（`ARCH-<ctx>-NNN`）へ分配 |
| Key design choices（zod 契約・hard gates・決定論・pluggable backend） | 設計**判断**＝ `docs/decisions/`（ADR・未新設）／一部は Shared Kernel の不変条件としてここ |
| Why JSON, not SQLite | `docs/decisions/`（ADR・未新設） |

## 未 first-class（理想ツリーで前方参照済み）

- `_system/{authoring,design,evaluation}/` の4ビュー（言語/ドメイン/アーキ/データ）
- `docs/decisions/`（ADR・append-only・supersede 可）— ARCHITECTURE の設計判断の住処
- `docs/cross-cutting/`（NFR 横断）・`_system/<ctx>/contracts/`（CONTRACT・公開言語）
- `docs/_derived/`（feature-catalog・traceability・図一式）
