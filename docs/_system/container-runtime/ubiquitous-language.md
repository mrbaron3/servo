# ユビキタス言語 — container-runtime コンテキスト

> container-runtime は、標準 OCI アプリケーションイメージと、Apple Container 等の**コンテナ**ランタイム操作を、
> OS 非依存の core から adapter 境界で隔離する。非決定な**AI**呼出しを扱う `agent-runtime` とは別境界であり、
> 翻訳点で共有する語は`runtime`と`Provider`の2つだけである。ここでProviderはagent-runtimeのAI tool familyを指し、
> Container Runtime engineの別名ではない。採点意味論・queue・liveness は所有しない。追加のみ
> （`LANG-container-runtime-NNN` は安定）。

| ID | 用語 | 意味 |
| --- | --- | --- |
| LANG-container-runtime-001 | Container Runtime | OCI コンテナを build/run し network/volume を管理する engine（Apple Container・docker・podman 等）。agent-runtime の Provider とは別軸。 |
| LANG-container-runtime-002 | Container Runtime Adapter | runtime-neutral な `ContainerRuntime` port を特定 CLI の argv／出力へ翻訳する実装。Apple Container 固有挙動はこの adapter だけに閉じる。 |
| LANG-container-runtime-003 | Standard OCI Image | Apple Container 専用形式でない標準 OCI のアプリイメージ。docker/podman/Apple Container で同一に build/run できる。 |
| LANG-container-runtime-004 | Runtime Preflight | 起動前に runtime capability（CLI・version・arch・service・network・volume・image）を fail-closed で検査した構造化 verdict。捏造 pass を出さない。 |
| LANG-container-runtime-005 | Runtime Role | control／triage／runner／postgres の隔離役割。publish 不変条件の key。 |
| LANG-container-runtime-006 | Publish Surface | Mac へ publish される host port の集合。control の loopback port だけが許容される。 |
| LANG-container-runtime-007 | Publish Invariant | 「control の 127.0.0.1 port だけが Mac へ publish され、triage／runner／postgres は内部 network からのみ到達可能」という不変条件。静的（desired）と grounded（running）で二重に接地する。 |
| LANG-container-runtime-008 | Container-Neutral Path | コンテナ絶対かつ設定可能で、Mac home（`/Users`）に依存しない path。grader／workspace／systemDir をこれで解決する。 |
| LANG-container-runtime-009 | Host-Path Dependency | build/runtime surface に hardcode された Mac 絶対 path。scanner が fail-closed で検出する回帰対象。 |
| LANG-container-runtime-010 | Isolated Runner | PostgreSQL leaseを消費し、private workspace内から既存AgentOps gateを実行するnonroot runtime role。 |
| LANG-container-runtime-011 | Registration Workspace | Registration IDをrootとするrunner-only volume上のmirror/worktree/state/artifact集合。 |
| LANG-container-runtime-012 | Startup Isolation Proof | mount/publish/outbound/HOME/credential/socket境界が副作用前に成立した構造化監査。 |
| LANG-container-runtime-013 | Lifecycle Owner | Apple Container topologyを操作する短命な`agentopsctl` process。 |
| LANG-container-runtime-014 | Actual Topology | runtime inspectで観測したMONITOR_ONLY 3 containerまたはACTIVE 4 container、network、publish、mount、security属性の現在値。 |
| LANG-container-runtime-015 | Scoped Compensation | partial startで当該試行が変更したcontainerだけをrollbackし、volumeと既存workを保存する処理。 |
| LANG-container-runtime-016 | Provider Credential Volume | provider login fileだけをprivate stdinでseedし、ACTIVE workerへread-only mountするnamed volume。 |
| LANG-container-runtime-017 | Typed Monitor Broker | controlの固定Issue/PR read要求をtriage credential境界内で実行するdurable broker。任意HTTP proxyではない。 |
| LANG-container-runtime-018 | Isolated Triage | workspace／git／SSHなしでtyped monitor、strict Issue classification、human-ready promotionだけを行うnonroot runtime role。role、image、build targetの正典名は`triage`で揃え、security境界が異なる`runner`を名称aliasにしない。`deploy/Containerfile`と`agentopsctl` image consumerもこの名称へ統一済みである。 |
| LANG-container-runtime-019 | Ownership Label | `agentopsctl`がcontainer／network／volumeの所有を判定する唯一のlabel。表示名ではなく互換性identifierであり、writer・reader・selectorが同時に移行しないと稼働resourceが孤児化する。key正典は`apps/control-plane/internal/lifecycle/ownership.go`に単一化する。 |
| LANG-container-runtime-020 | Ownership Class | Ownership Labelから導く6つの明示分類（`legacy-only`／`current-only`／`dual`／`conflicting`／`unmanaged`／`missing-label`）。`conflicting`は部分移行した自分のresourceであり、他人のものを意味する`unmanaged`と同一視せずfail-closeする。 |
| LANG-container-runtime-021 | Migration Disposition | Ownership Classから導く掃討の判定（`pending`／`migrated`／`skipped`／`conflicting`／`blocked`）。Ownership Classが「所有の事実」であるのに対しこちらは「掃討が何をするか」であり、Phase 3のentry gateはこの件数で書かれる。`blocked`は**所有しているが同一のreplacementを再構築できない**もので、`skipped`（移行不要または非所有）と混同しない。 |
| LANG-container-runtime-022 | Label Sweep | 既存containerを削除して観測どおりに作り直すことでOwnership Labelを両namespaceへ揃える操作。Apple Containerが既存containerのlabelを変更できないため作り直すしかない。exact idを指定した対象だけを、`re-inspect`→`stop`→`delete`→`volume-release`→`recreate`→`verify`の順で1件ずつ進め、named volumeは決して削除しない。 |
| LANG-container-runtime-023 | Metadata Label Migration | Apple Containerのserviceを停止し、runtime自身のmetadata document（volume/networkの`entity.json`、containerの`runtime-configuration.json`と`config.json`）のlabels fieldだけを書き換えてOwnership Labelを移す操作。1.1.0に既存resourceのlabelを変更するroute（CLI・XPC route・ClientVolume）が存在せず、volumeの作り直しはdata破棄そのものであるため、resourceを1つも削除せずにvolume/networkを移行できる唯一の手段である。Label Sweepが「作り直す」のに対しこちらは「消さない」。 |
| LANG-container-runtime-024 | Byte Stable Round Trip | metadata documentをparseして書き戻したbyte列が元と完全一致することを、書き換えの前に証明するguard。再現できないdocumentはtoolが理解していない証拠として拒否する。これによりlabels field以外のbyteが変わらないことが主張でなく事実になる。 |
| LANG-container-runtime-025 | Migration Stage | Metadata Label Migrationの2段階。`prepare`は`legacy-only`のpairにcurrent側を足して`dual`にし、`retire`はcurrent側が全managed resourceで揃った後にlegacy側を落として`current-only`にする。`retire`はhost全体に`legacy-only`が1件も無いことを条件とし、対象を絞っても緩まない。 |
| LANG-container-runtime-026 | Restore Outcome | rollbackがdocumentを戻した方法。`bytes`はsweepが書いたままのdocumentへbackupのbyteを戻したもの、`relabelled`はruntimeが再直列化していたためlabels以外の全fieldが値として一致することを証明したうえで移行前labelsを書き戻したもの、`already-before`は未適用。Apple Containerは`system start`のたびに`volumes/*/entity.json`をkey順違いで書き直すため、byte一致だけを許すとrollbackが実務上使えない。 |
