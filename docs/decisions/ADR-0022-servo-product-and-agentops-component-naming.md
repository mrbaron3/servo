# ADR-0022: 製品名をServo、実行系technical prefixをagentopsに固定する

- 状態: 採択・system viewへ吸収済み
- 関連: [ADR-0010](ADR-0010-webhook-ingress-and-multi-repository-control-plane.md)、
  [ADR-0011](ADR-0011-standard-oci-image-and-container-runtime-adapter.md)、
  [ADR-0016](ADR-0016-agentopsctl-lifecycle-authority.md)、
  [ADR-0019](ADR-0019-github-app-credential-broker.md)、
  [ADR-0021](ADR-0021-go-typescript-application-boundaries.md)

## 文脈

repository、文書、CLI、environment、image、JSON Schema、container labelに
`servo`、`AgentOps`、`agentops`、`workflow`の4系統が残っている。すべてを一括置換すると、
人間向け製品名と、互換性を持つmachine identifierを同じ速度で移行することになる。

とくに`com.mrbaron3.workflow.*` container labelは見た目だけの名称ではない。
`apps/control-plane/internal/lifecycle/runtime.go`と
`apps/control-plane/cmd/agentopsctl/manager.go`が稼働containerの所有権判定に使う。
writerとselectorの片側だけを変更すると既存containerが孤児化し、Apple Containerのvolume排他attachにより
置換containerも起動できなくなる。JSON Schemaの`$id`もconsumerが参照し得る識別子であり、title置換と同列に扱えない。

一方、`agentopsctl`、`AGENTOPS_*`、`agentops-control`等は既にoperator contractとして広く使われる。
これらは製品名ではなく、Servo内の実行・評価component族を示すtechnical prefixとして意味を持つ。

## 決定

1. **人間向けの製品名をServoに固定する。** repositoryは`mrbaron3/servo`、文書見出し、release、
   operator向け説明では`Servo`を使う。`workflow`と`AgentOps`を新しい製品名として使わない。
2. **`agentops`を実行系technical prefixとして維持する。** `apps/agentops/`、`agentopsctl`、
   `agentops-control`、`AGENTOPS_*`、database schema `agentops_control`は、製品とは別のcomponent／contract名である。
   大文字の`AgentOps`をServoと並ぶ製品として説明しない。
3. **物理application名はADR-0021に従う。** Go applicationは`apps/control-plane/`、TypeScript applicationは
   `apps/agentops/`である。歴史文書以外へ旧root `src/` / `internal/`等を再導入しない。
4. **machine identifierは互換性境界ごとに移行する。** 現在と次の正典を次表に固定する。

   | 対象 | 現在のauthority | 次の正典／方針 |
   | --- | --- | --- |
   | 製品・repository | `Servo` / `mrbaron3/servo` | 現在から正典 |
   | component・CLI・env | `agentops*` / `AGENTOPS_*` | technical prefixとして維持 |
   | PostgreSQL schema | `agentops_control` | published DB contractとして維持 |
   | container label | `com.mrbaron3.workflow.*` | [Issue #123](https://github.com/mrbaron3/servo/issues/123)で`com.mrbaron3.servo.*`へ3段階移行 |
   | JSON Schema `$id` | `https://github.com/mrbaron3/servo/contracts/**` | repository内consumer照合後に旧`workflow`IDから移行済み。値域・schema versionは不変 |
   | schema title・人間向け表示 | `Servo` | legacy `AgentOps` titleから統一済み |

5. **container labelはこのADRの実装PRで変更しない。** 専用issueは必ず次の3段階を別々に検証する。
   1. writerが新旧両labelを付け、reader/selectorが両方を認識する。
   2. 稼働・停止containerを調査し、旧labelだけの対象を安全に掃討または再作成する。
   3. 旧labelが0件であるgrounded確認後に新labelのみへcontractする。
6. **略称を見つけただけで機械置換しない。** identifier変更はowner、reader、writer、selector、
   persisted data、external consumer、rollbackを列挙してから行う。Apple Container固有のownership／volume確認は
   headless testの代替で証明したと扱わない。

## 帰結

- README、runbook、context mapはServoを一つの製品として説明できる。
- `agentops` prefixを直ちに破壊せず、人間向けrenameとmachine compatibility migrationを分離できる。
- 旧`workflow`はcontainer labelだけに残る明示的なcompatibility identifierで、JSON Schema IDとは移行速度を分離する。
- label移行完了までは新旧どちらか片側だけの実装を受け入れられない。
- この判断は未監査領域の語彙整合やApple Container上の移行完了を主張しない。

## 実装先 id

- context map: `docs/context-map.md`「製品・実行系の正典名」
- language: `LANG-agent-runtime-003`、`LANG-container-runtime-005`、
  `LANG-container-runtime-013`、`LANG-container-runtime-018`
