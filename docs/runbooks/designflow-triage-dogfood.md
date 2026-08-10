# Designflowを最初の外部targetとしてtriage／開発する

Tracking: [Workflow #44](https://github.com/mrbaron3/workflow/issues/44)

## 決定

DesignflowをWorkflowの最初のmulti-repository dogfood対象にする。ただし
`mrbaron3/designflow`というrepository名、Issue番号、label名、grader commandはproduction codeへ埋め込まない。
対象はoperator allowlist＋PostgreSQL Registration、labelはpolicy、graderはcheckout済みrepositoryのbounded
metadataから決める。

monitor／triageはdevelopment runnerから分離した専用containerで動かす。一方、このcontainerのcodeは現時点では
`apps/agentops/`に、Go control/broker/lifecycleは`apps/control-plane/`に置き、application sourceを混在させない。
root `db/` / `contracts/`のPostgreSQL contractがdurable business coordinationを担う一方、credential broker
HTTP、CONNECT proxy、shared volume、actual container lifecycle操作は別境界である
（lifecycle mode/drain fenceはDB-backed）。別repositoryへは分けず、control-store schema、
DB capability、lifecycle、OCI topologyと同じreleaseで互換性を保つ。当面はrepository全体が一つのrelease unitであり、
独立した運用主体、release cadence、compatibility protocolが必要になった時だけ抽出する
（[ADR-0021](../decisions/ADR-0021-go-typescript-application-boundaries.md)）。

```text
GitHub Issue
    │
    ▼
control（GitHub credentialなし）
    │ typed monitor request / identity-only triage job
    ▼
GitHub App broker（秘密鍵を持つ唯一のcontainer、host publishなし）
    │ repository/permission限定・短期token
    ▼
triage container（triage capability、workspace/git/SSHなし）
    │ managed label + marker comment
    │
    └─ exact human ready label
             │ atomic DB promotion
             ▼
development runner（別capability、private workspace、ACTIVEだけ）
```

## Bootstrap

1. operatorのprivate env fileで少なくとも次を設定する。値はshell history、Issue、logへ書かない。

   ```sh
   export AGENTOPS_GITHUB_APP_ID='<numeric App id>'
   export AGENTOPS_GITHUB_APP_INSTALLATION_ID='<numeric installation id>'
   export AGENTOPS_GITHUB_APP_SLUG='<canonical app slug>'
   export AGENTOPS_GITHUB_APP_OWNER='mrbaron3'
   export AGENTOPS_GITHUB_APP_PRIVATE_KEY_FILE='<absolute mode-0600 .pem path>'
   # broker capabilityは任意。未設定なら`agentopsctl`がrole別に生成し、
   # privateな`~/.agentops/<prefix>/broker-capabilities.json`（mode 0600）へ保存して読み戻す。
   # export AGENTOPS_GITHUB_BROKER_TRIAGE_CAPABILITY='<43..128 URL-safe文字>'
   # export AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY='<別の43..128 URL-safe文字>'
   export AGENTOPS_TRIAGE_READY_LABEL='ready'
   export AGENTOPS_TRIAGE_CLAIMED_LABEL='agent-claimed'
   export AGENTOPS_TRIAGE_CANDIDATE_LABEL='ready-candidate'
   export AGENTOPS_TRIAGE_BLOCKED_LABEL='blocked'
   export AGENTOPS_TRIAGE_NEEDS_INFO_LABEL='needs-info'
   export AGENTOPS_TRIAGE_CONTEXT_PATHS_JSON='["README.md","AGENTS.md","docs/NORTH_STAR.md","docs/roadmap.yaml","docs/handoffs/HANDOFF.md"]'
   ```

   `GH_TOKEN`、`GITHUB_TOKEN`、`AGENTOPS_{CONTROL,TRIAGE,RUNNER}_GITHUB_TOKEN`は設定しない。
   role capabilityは他のcredentialから導出せず、他のcredentialと同値にもしない（`agentopsctl`が拒否する）。
   生成されたstoreはoperatorが読み書きしない。rotationは`OFF`中にstoreを削除して次のstartで再生成させる。
   AppはWorkflowとDesignflowだけへinstallし、permission union／role別token subsetは
   [ADR-0019](../decisions/ADR-0019-github-app-credential-broker.md)に固定する。
   4つのDB password、control token、Dashboard bootstrap token、webhook secretも
   [credential runbook](ciso-07-credential-bootstrap-and-rotation.md)どおり別値にする。

2. observation-onlyで起動する。

   ```sh
   mise run monitor
   ```

   初回は`mise.local.toml.example`を`mise.local.toml`へコピーし、1.のenv fileの絶対パスを
   指しておく。task側が`[env] _.file`でenvを読み、`[tools] go`でtoolchainを解決するので、
   `set -a; . <env file>; set +a`も`mise exec go@<version> --`も要らない。依存の`env:check`が
   必須13変数の欠落を名前付きで落とし、通ったときはallowlistを表示する（`monitor`が1
   repositoryのまま静かに動く事故を入口で見せるため）。素で叩くなら次と等価:

   ```sh
   go run ./apps/control-plane/cmd/agentopsctl start --mode MONITOR_ONLY --build
   ```

   `--request-id`は渡していない。省略時は`commandID()`が
   `<operation>-<unixnano>-<pid>`を生成するので、固定値を焼き込んで再実行が前回の要求と
   衝突する事故を避けられる。冪等性を明示的に握りたいときだけ
   `mise run monitor -- --request-id <id>`のように`--`の後ろへ渡す。

   このmodeではIssue/PR identityの観測だけを行う。AI classification、label/comment mutation、
   development runner、provider credentialは存在しない。freshnessは更新するがprocessing cursorは
   進めないため、ここで発見した既存IssueもACTIVE最初のpollで再読される。

3. Dashboard、または同じ意味のControl APIでDesignflow Registrationを作る。APIを使う場合はcredentialを
   argvへ置かずstdin configで渡す。

   ```sh
   curl --silent --show-error --fail-with-body --config - <<EOF
   url = "http://127.0.0.1:8080/v1/registrations"
   request = "POST"
   header = "Authorization: Bearer ${AGENTOPS_CONTROL_TOKEN}"
   header = "Idempotency-Key: designflow-registration-001"
   header = "Content-Type: application/json"
   data = "{\"repository\":\"mrbaron3/designflow\",\"enabled\":true,\"issueMonitorEnabled\":true,\"prMonitorEnabled\":true,\"executionEnabled\":true}"
   EOF
   ```

   repository scope は current enabled Registration だけから解決される。process env の
   repository allowlist は設定しない。

4. Dashboardでrepository、monitor freshness、queue、last failureを確認してから`ACTIVE`へ進める。
   ACTIVEではprovider credentialも必要になる。

   ```sh
   mise run active
   ```

   `mise run active` は clean HEAD を検証し、必要なら current job を drain してから全 image を
   rebuild/migrate/promoteする staged deployment である。

   その他のlifecycle操作も同じ入口に揃えてある。`mise tasks`で一覧できる。

   | task | 実体 |
   | --- | --- |
   | `mise run status` | `agentopsctl status`（`-- --json`でJSON） |
   | `mise run progress -- owner/repository#1` | 親Epicから子Issueのphase/blocker/next gateを一覧 |
   | `mise run progress -- owner/repository#8` | 子Issueの工程履歴、session、branch、PR、worktreeを表示 |
   | `mise run worktree -- --diff owner/repository#8` | 明示した子Issueの隔離worktree状態と差分を表示 |
   | `mise run worktree -- --shell owner/repository#8` | 明示した子Issueの隔離worktreeへ再開用shellで入る |
   | `mise run monitor` | `agentopsctl start --mode MONITOR_ONLY --build` |
   | `mise run active` | `agentopsctl deploy` |
   | `mise run drain` | `agentopsctl drain` |
   | `mise run stop` | `agentopsctl stop` |
   | `mise run logs` | `agentopsctl logs`（`-- --component triage --follow`） |
   | `mise run open` | `agentopsctl open` |

   親Epicを`worktree`へ渡すと、任意の子を暗黙選択せず対象子Issueの明示を要求する。
   保持されたattemptは別pathなので、再試行や別Issueの修正とは干渉しない。
   保持されたattemptは既定で14日後にrunnerが自動回収する（`AGENTOPS_RUNNER_RETAINED_WORKSPACE_TTL_MS`、
   `0`で無効）。長く残す必要があるなら、その前にworktreeの内容を退避しておく。

## control schema 17へのupgrade（破壊的・一度きり）

control schema 17は**ready時点のSource Issueを凍結した要件authority**を全releaseに必須化する。
schema 17より前に作られたreleaseはその受領証を持たず、後から遡って作ることもできない
（凍結すべき「ready時点のIssue本文」がもう残っていない）。したがってmigration 0017は
**進行中のrelease（`collecting` / `merge-authorized`）を全件`abandoned`にする**。

**upgrade前に必ずdrainする。**

```sh
mise run drain    # 実行中のjobを完走させてからstop
mise run stop
# ここでupgrade（mise run active）
```

drainせずにupgradeした場合、またはdrain中にreadyが付いていたIssueがある場合は、
**GitHub側のlabelはmigrationからは触れない**ので次の状態が残る:

- Issueに`claimed` labelが付いたまま
- 対応するreleaseは`abandoned`（terminal）
- 既に開いていたPRはそのまま残る（release識別子とは切り離される）

回収手順（Issueごとに人間が行う）:

1. `mise run progress -- <owner>/<repo>#<番号>` で状態を確認する。migration 0017が
   この回収手順を説明するdurable progress eventを書き込んでいる。
2. 残っているPRを見て、continueするかcloseするかを決める。
3. `claimed` labelを外す。
4. 要件を確認し、改めて`ready` labelを付け直す。これが新しいready authorityとなり、
   その時点のIssue本文が新しいreleaseへ凍結される。

`ready`を貼り直さない限りIssueは再開しない。これは意図された fail-closed であり、
人間の再承認なしに古い（もう検証できない）要件で実装を続けないための境界である。

## 最初に流すIssue

最初のheadless縦断候補はDesignflowのDF-006（AuthoringBackend port、現在のroadmap上はIssue #4）。
UI lifecycleを前提にせず、公開contractと`node scripts/check-contracts.mjs`で判定でき、次のDF-007
provider adapterを解放するためである。Issue番号は運用時点の情報であり、code/testには固定しない。

AI triageが`ready-candidate`を付けても開発は始まらない。依存、WHAT、受入境界を人間が確認し、
実装開始を意図した場合だけexact `ready` labelを付ける。development runnerはpromotion payload内の
configured ready／claimed labelを使い、Designflow専用分岐なしにIssueをclaimする。

Designflowは`package.json`のdirect Node contract checkerからgrader profileを選ぶ。repository名で
graderを分岐せず、shell operator、package install、job-supplied commandは拒否する。UIを持つDF-005では、
headless contract testの後に最小のheaded browser／assistive-technology確認を別証拠として追加する。

## Codex asset／Claude Designの役割指定

意図するpolicyは次のとおり。

- Codex asset route: raster asset、illustration、texture等を生成し、artifact URI／digest／size／provenanceを返す。
- Claude Design route: design token、component、pattern、state、interaction、accessibility ruleと
  Design System Deltaを所有する。
- Experience／Design Bundleは両出力をversioned contractで合流し、人間承認をimmutable revision digestへ束縛する。
- providerはready approval、consumer Issue lifecycle、endpoint／table設計を所有しない。

この分担はDesignflowの公開`AuthoringBackend` contract/policyとして指定し、repository名条件やpromptだけの慣習に
しない。現在のlegacy harnessは`.agentops.json`のrole routeでCodex／Claudeを分けられるが、isolated production
runnerは1回のjobで単一providerを全roleへ配る。したがって上記の二provider分担はまだproduction縦断の完了条件を
満たしていない。Workflow側Issueで、job contractに任意commandやcredentialを追加せずrole→provider policyを
Registration-owned configurationとして渡す変更を追跡する。

## 合格条件

- 任意の2 repositoryを同時allowlist／Registrationへ置き、brokerが混同せず観測する。
- Issue本文中の命令を実行せず、strict triage decisionとmanaged label/commentだけを作る。
- AIは`ready`を付けられず、人間のexact label後だけdevelopment jobがatomicに作られる。
- triage containerにworkspace、git／SSH、runner capability、control token、host port/socketがない。
- App秘密鍵はbrokerだけにread-only mountされ、triage／runner／controlから読めない。
- Designflowのdirect Node contract checkerをrepository名分岐なしで実行する。
- 最後にlive GitHub上でclaim→PR→current-head review→required check→expected-head merge→Issue closeを1件通す。

## 証拠の形式と検証

新しい実走の結果は`contracts/live-release-evidence-v2.schema.json`へ適合する1つのJSONにまとめ、
`evidence/live-release/`へ置く。検証は次で行う。

```sh
npm run evidence:live-release -- <evidence.json>
```

CISO-07の`ciso-07-release-evidence.schema.json`はPR 41・`mrbaron3/workflow`・check名`macos`/`postgres`を
`const`で固定した**その1回のための**契約であり、Designflow実走は入らない。live release契約は識別子
（repository、Issue番号、PR番号、SHA、check名）を検証だけして固定せず、**主張したい不変条件のほうを`const`で
固定する**（`aiAppliedReadyLabel: false`、`promotionAtomic: true`、`round3Created: false`、
`blockingReviewThreads: 0`、`sourceIssueClosure: completed`など）。したがって2件目以降の外部targetでも同じ契約を使う。
version 1 artifactは元の`contracts/live-release-evidence.schema.json`で引き続き検証するが、新しい実走をv1へ
down-convertしない。

schemaは形状と個別の不変条件だけを見る。**独立に観測した各セクションを1つのrelease revisionへ束縛する**のは
`apps/agentops/src/evidence/live-release.ts`の意味検証で、別々の実走から集めた断片を合格の証拠へ
組み立てられないようにする。
主な束縛は次のとおり。

- `target.repository`は`consumer.repository`と異なること（自repo cutoverは外部target実証にならない）。
- 観測セクション（`triage`／`execution`／`github`）が記録するrepository／Issue番号が`target`と一致すること。
  これが無いと`target`だけ書き換えて、あるIssueで集めた証拠を別のIssueやrepositoryの証明として
  貼り替えられる。座標は固定しない（別targetの実走は観測セクションごと動けば妥当なまま）。
- `triage.observedAt`→`triage.humanReadyAppliedAt`→`execution.observedAt`→`github.observedAt`が
  厳密に前進すること。human readyゲートより前に始まった実走を証明できないようにする境界であり、
  同時刻は順序を証明しないので同値も拒否する。
- `execution.expectedHead`／`github.finalPrHead`／`formalReviews.round2.head`が`execution.finalHead`と同一。
  merge fenceは実際にreleaseしたrevisionに対して計算されていなければならない。
- round 1でfindingが出たなら round 2のheadは前進していること（同一revisionの2度読みを「2ラウンド」と
  呼べないようにする）。
- `triage.managedLabelsApplied`にready／claimed labelが混ざらないこと。
- `providerInvocations`の`invocationKey`が一意で、`triage`と`generator`のinvocationが最低1件ずつあること。
- 各invocationの`jobId`が、triageなら`triage.promotionJobId`、それ以外なら`execution.jobId`と一致すること。
  これが無いとinvocation配列は出所不明の自由記述で、別のIssueや別実走の記録を貼っても role と一意性の
  条件を満たしてしまう。なおtriage containerはcheckoutを持たないので、triageのinvocationは`head`を持たない
  （持たせると虚偽になる）。
- `designBundle.applicable`と`releaseLineage.applicable`が一致すること。この2つは同じ事実を両側から
  述べているので、揃って有効か揃って不適用かのどちらかしかない。揃って有効なら`revisionId`と
  `bundleDigest`も一致すること（revision IDとbundle digestは本番のreconciliationで1つの同一性として
  扱われる）。加えて`releaseLineage.headSha`は`execution.finalHead`と一致すること。

`execution.graderCommands`は、runnerが実際に出す2つのprofileだけを表現できる。vendored toolchain
（typescriptとvitestを持つrepository向けの固定コマンド）は文字列を固定し、direct Node contract checkerは
**runtimeと同じ相対パス規則**（絶対パス不可、空・`.`・`..`のsegment不可）を課す。evidenceの側がruntimeより
緩いと、runnerなら拒否するcheckerを「通った」と証明できてしまうため、両者が受理・拒否とも揃うことを
`inferRepositoryGraders`を実際に動かすテストで固定している。

### HOW介入とresult

`result`は介入台帳の**読み取り**であり独立した主張ではない。`howInterventions.count`が0なら`passed`、
1以上なら`passed-with-interventions`でなければならず、`count`は`records`の要素数と一致する必要がある。

介入が1回でもあるとevidence自体が作れない設計にはしていない。作れないと「まだ0になっていない」という事実が
記録に残らず、実走を重ねて介入が減っていく過程を機械で追えなくなるためである。介入ありの実走も妥当な
evidenceとして残し、北極星（人間はWHATのみ）へ到達したか否かは`result`が判定する。

なお介入の語彙には人間の**判断点**（adopt／assign／sign／decide／label）が存在しない。`ready` labelを付ける
行為は自律性の定義に含まれる判断であって介入ではないので、ここには数えない。
