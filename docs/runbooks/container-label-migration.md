# container ownership label 移行（`com.mrbaron3.workflow.*` → `com.mrbaron3.servo.*`）

対象は [Issue #123](https://github.com/mrbaron3/servo/issues/123) と
[ADR-0022](../decisions/ADR-0022-servo-product-and-agentops-component-naming.md)。
container label は表示名ではなく**互換性 identifier** である。`agentopsctl` はこの label だけで
「この container / network / volume は自分の所有か」を判定する。writer と reader の片側だけを
変更すると稼働中の container が孤児化し、Apple Container の named volume は単一 VM へ**排他 attach**
されるため、置換 container も起動できなくなる。だから 3 段階に分ける。

label key の正典は `apps/control-plane/internal/lifecycle/ownership.go` **1 箇所だけ**である。
新しい判定箇所を足すときも、key 文字列を書かず必ずこの file の関数を通す。

## 分類（P3B 以降）

**P3B の binary は `com.mrbaron3.servo.*` だけを読む。** 判定はすべて新 namespace の
`com.mrbaron3.servo.agentopsctl` から導かれ、旧 namespace は read も分類もされない。

| 分類 | 条件 | 所有か | 扱い |
| --- | --- | --- | --- |
| `owned` | `com.mrbaron3.servo.agentopsctl=v1` | 所有 | 通常の managed resource。 |
| `unmanaged` | 新 key はあるが値が `v1` でない | 非所有 | 別 deployment の名前。触らない。 |
| `missing-label` | 新 key が無い | 非所有 | 触らない。**旧 namespace だけを持つ resource もここに落ちる。** |
| `malformed` | 新 namespace が中途半端（marker が空文字／marker 無しで `com.mrbaron3.servo.role`・`spec-sha256` がある） | **判定不能** | fail-closed。unowned として扱わない。 |

`malformed` を `unmanaged` と同一視しないことが本契約の安全性そのものである。`unmanaged` は
「他人のもの」を意味し、名前が空いているという判断に使われる。作成途中で中断した自分の
container をそこへ落とすと、排他 attach 中の volume を持ったまま削除・再作成の対象になり得る。

**「空文字」と「不在」は別扱いである。** `role` / `spec-sha256` が**空文字**なら書きかけと見なして
fail-closed にするが、**不在**は正常な形として受け入れる。volume と network は role を持たないし、
`spec.SpecDigest` が空の container は spec label を書かずに作られる（`buildContainerArgs`）。
不在を fail-closed にすると、そうした container が **delete できなくなる**——所有しているのに
触れない resource を作るのは、この phase が防ごうとしている事故そのものである。
したがって `owned` は「所有しており、かつ書きかけの label が無い」を意味し、
「3 つの label が揃っている」は意味しない。inventory の reason 文言もそう書いてある。

### P3B で何が変わったか

- **旧 namespace だけを持つ resource は `missing-label`＝非所有になった。** `EnsureVolume` /
  `EnsureNetwork` / `Delete` / drain / reconcile はいずれも**拒否する**。**採用も変更も削除もしない。**
  Apple Container の named volume は排他 attach なので、「見えない」ものは「空いている名前」ではない。
- **`dual` の resource は新 label だけで評価される。** 旧 label が食い違っていても参照しないので、
  かつて `conflicting` だった組み合わせは今は単に新 label の値どおりに読まれる
  （新が `v1` なら所有、`v2` なら `unmanaged`）。
- **中途半端な新 label は依然 fail-closed** である（上表 `malformed`）。
- 旧分類 `legacy-only` / `current-only` / `dual` は `owned` に統合され、`conflicting` は
  `malformed` に置き換わった。

### 旧 binary との対照（P1〜P3A の 6 分類）

移行期の evidence を読むときのために残す。P1〜P3A の binary は新旧両方を読み、
`legacy-only` / `current-only` / `dual` / `conflicting` / `unmanaged` / `missing-label` の
6 分類を出した。`conflicting` は新旧が異なる値（片側が空文字も含む）を指し、P3B の
`malformed` とは**別の条件**である。

## Phase gate

| Phase | 入る条件 | 出る条件（次へ進む gate） |
| --- | --- | --- |
| **P1 dual label**（本 PR で実装済み） | なし | 新規作成 resource が新旧両 label を持ち、reader が上表 6 分類を明示し、旧 binary へ戻しても発見できることを grounded に確認済み |
| **P2 旧 container 掃討**（本 PR で実装済み） | P1 が merge 済みで、稼働 host の inventory が取れている | old-only container が 0 件、移行・skip・conflict・block の bounded audit が残り、Apple Container 上で drain/recreate と volume detach/attach、restart 整合、rollback を grounded に確認済み |
| **P3A 旧 write 停止＋obsolete label 掃討**（本 PR で実装済み） | P2 の gate を満たし、dual label 観測窓で ownership／attachment の回帰が無い | 新規作成 resource が `current-only` になり、reader は 6 分類を保ったまま、全 managed container / volume / network が `current-only` へ移行済み |
| **P3B 旧 read 削除**（本 PR で実装済み） | P3A が merge 済みで、host に `legacy-only` が 0 件 | 旧 namespace の read が消え、reader が `com.mrbaron3.servo.*` だけを見る。production source に旧 key の参照が 1 つも残らない（`TestNoProductionCodeReferencesTheLegacyNamespace` が repository 全体で回帰を止める） |

**P1 から P3 へ直接飛ばない。** 旧 write と旧 read は同じ PR で消さない（write を先に止める）。
**P3A と P3B も同じ PR にしない。** write 停止と掃討が終わって初めて read を消せる。

## Inventory command

P2 以降は専用 subcommand を使う。**引数なしの `migrate-labels` は read-only の inventory であり、
host を一切変更しない**（安全な綴りを短い方に割り当てている）。

```sh
agentopsctl migrate-labels                       # dry-run。分類・named volume・件数を出す（file は書かない）
agentopsctl migrate-labels -evidence-dir <dir>   # 上記に加えて durable な控えを <dir> へ書く
```

引数なしの `migrate-labels` は **stdout に出すだけで file を書かない**。read-only を名乗るものが
worktree に file を落とさないためである。観測窓のサンプルなど控えが要るときだけ `-evidence-dir` を渡す。
`--only` はどの path からも受け付けられない。`--apply` は撤去済みで、inventory は gate として読む件数が host 全体である必要があるため受け取らない。

出力の `skipped` は所有かつ完全に label 済み、または非所有（`unmanaged` / `missing-label`）、
`malformed` は中途半端な新 label（fail-closed）、`blocked` は未知 class に対する fail-closed 既定である。
`pending` / `migrated` は**撤去済みの P2 sweep だけが出した歴史的な値**で、
`evidence/label-p2/*.json` を読むために型としてのみ残っている。

subcommand が使えない状況（binary が無い等）では Apple Container の listing を直接分類する。
**listing は read-only であり、label selector による一括削除は行わない。**

```sh
container list --all --format json | python3 -c '
import json, sys, collections
CUR = "com.mrbaron3.servo.agentopsctl"
ROLE = "com.mrbaron3.servo.role"
SPEC = "com.mrbaron3.servo.spec-sha256"

def classify(labels):
    if CUR not in labels:
        # 旧 namespace だけの resource もここに落ちる（P3B は読まない）。
        if ROLE in labels or SPEC in labels:
            return "malformed"
        return "missing-label"
    value = labels[CUR]
    if value.strip() == "":
        return "malformed"
    return "owned" if value == "v1" else "unmanaged"

counts = collections.Counter()
for item in json.load(sys.stdin):
    configuration = item["configuration"]
    kind = classify(configuration.get("labels") or {})
    counts[kind] += 1
    print(configuration["id"], item["status"]["state"], kind)
print("---", dict(counts))
'
```

移行期の host を旧 6 分類で読み直したいときは、`git show` で P3A 時点のこの節を参照する。

`volume list` / `network list` は `item["configuration"]["labels"]` と `item["id"]` で同じ分類ができる。

読み方:

- `malformed` が 1 件でもあれば、**そこで止める**。`agentopsctl` はその resource に触れる操作を
  fail-closed で拒否する。error は**問題のある key 名だけ**を出し、label の値そのものは出さない
  （label 値は事故や外部由来の任意文字列で、durable な lifecycle failure record にも残るため）。
  実際の値は `container inspect <name>` で確認する。
- `malformed` の error は drift の error と区別される。「DRAINING して stop して restart」を促す文言が
  出たらそれは drift であって中途半端な label ではない。**中途半端な label に対して drift の手順を
  実行しない**（排他 attach 中の named volume を持つ container を削除・再作成することになる）。
- `missing-label` の中に**旧 namespace だけを持つ resource が混ざり得る**。P3B の binary からは
  区別が付かないので、素性を知りたいときは `container inspect <name>` で label を直接見る。
  いずれにせよ `agentopsctl` は触らない。
- `unmanaged` / `missing-label` は移行対象ではない。数を減らそうとしない。

## Rollback（P3B → P1/P2/P3A）

**P3B へ上げたあとの rollback は、意図的な運用判断である。** 自動 fallback は存在しない。
P3B の binary は `com.mrbaron3.servo.*` しか読まないので、旧 label へ戻した resource は
**その binary からは見えなくなる**。戻すなら binary も一緒に戻す。

### 一方向であること

`agentopsctl migrate-label-metadata --rollback` は残してある。これは P3A の書き換えを
**undo できるが redo できない**。P3A の 2 stage（`prepare` / `retire`）は撤去済みで、
`--stage` を渡すと理由を出して拒否される（runtime には一切触れない）。

- **undo できる**: rollback は document を**移行前の label** へ戻す。label の中身を解釈しない
  ので、このbinaryが読めない namespace の label でも正しく復元できる。戻し方は document ごとに
  異なり、その結果は per-document の outcome として出力される。
  - `bytes`: migration が書いた bytes のままだった document。private backup の**逐語 bytes**
    で置き換える。
  - `relabelled`: migration 以降に runtime が再直列化した document（`container system start`
    は `volumes/<name>/entity.json` を書き直す。値は保つが key 順は保たない）。**label 以外の
    全 field が値として一致することを証明したうえで**、現状の document へ移行前 label を書き戻す。
    ここで古い bytes を上書きすると、runtime がその後に記録した内容を黙って巻き戻すことになる。
  - `already-before`: 既に移行前 label だった document。何も書かない。
- **redo できない**: 前へ進める stage が無い。もう一度 `current-only` にしたければ
  P3A の binary を使う。

### 必要なもの

次の 3 つが揃わない限り rollback しない。

1. **保全済みの private backup root**（P3A の `--apply` が書いた `rollback-plan.json` を含む）。
   repository には入っていない。既定は `$XDG_STATE_HOME/agentops/label-metadata-backups`。
   backup は container の `config.json` の逐語 copy であり `POSTGRES_PASSWORD` を含む
   **credential store**なので、git work tree の中には無い。
2. **全 managed resource が `current-only` である証跡**（P3A の観測窓と事後 inventory）。
   これが無いと、戻した先がどの状態なのかを後から言えない。
3. **P3B 以前の binary**。rollback 後の host を操作するのはこちらである。

### 手順

```sh
# 1. lease を quiesce したうえで、plan が名指しする container を「削除せずに」停止する。
#
#    **`agentopsctl stop` を使ってはならない。** stop は runner / triage /
#    github-broker / control を gracefulStop の後に delete し、続けて postgres も
#    delete する。**計 5 本**であり、これは retire plan が復元対象として記録している
#    container の全てである。とくに `agentops-postgres` は、排他 attach された named
#    volume を持つという理由でこの移行が存在する当の container であり、これも道連れに
#    なる（「postgres は残るから被害は限定的」は誤り）。削除すると復元すべき metadata
#    document ごと消える。document が消えると preflightRestore は ENOENT で撥ね、
#    BindToHost は最初の失敗で plan 全体を拒否するため、まだ健全な volume 20 本と
#    network 7 本まで巻き添えで復元不能になる。意図して受け入れた one-way boundary が
#    no-way boundary に変わる。
#
#    stop 後に `agentopsctl start` で作り直しても復旧しない。同じ path に別 bytes の
#    container が座るため、今度は digest 不一致で撥ねられる。
agentopsctl drain
container stop agentops-runner agentops-triage agentops-github-broker \
  agentops-control agentops-postgres

# 2. 保全済み plan を渡す。runtime の停止・再起動は command 側が面倒を見る。
agentopsctl migrate-label-metadata --rollback <backup-root>/retire-<stamp>/rollback-plan.json

# 3. ここから先は P3B の binary では host が見えない。P3B 以前の binary へ差し替える。
#    差し替えずに `agentopsctl status` を叩くと、resource は missing-label として
#    「所有していない」と報告される。これは異常ではなく、この phase の設計どおりである。
```

rollback は**冪等**である。中断したら同じ plan で再実行してよい。P3A 実行後に生まれた
document（container を start すると runtime が作る `config.json`）も、sweep が書いた label と
完全一致することを確認したうえで面倒を見る（`reconciled` に記録される）。

ただし**中断後の再実行では手順 1 の `agentopsctl drain` も使えなくなる**。既に戻された
resource は pre-P3A label を持つので、この binary からは `missing-label` に見える。
gracefulStop は `RequireManaged` を通すため、それらの container を触ろうとする
`agentopsctl` の経路は軒並み拒否する。再実行時は上記の生 `container stop` だけで
not-running gate を満たすこと。

rollback が plan の途中で失敗した場合、host は**部分的に戻った状態**になる（reverse 順に
処理するため、失敗した application より後ろは既に復元済み）。どこまで戻ったかは
コマンドが出力する per-document の outcome 表に出る。同じ plan の再実行が正規の復旧手段
である（`RestoreAlreadyBefore` で冪等）。

### 移行期の rollback（歴史的記録）

P1 の writer は新旧**両方**の label を書いていたため、P1/P2 の間は binary を戻すだけで
rollback できた（旧 binary は旧 namespace を読み、それは常に存在した）。P3A が旧 write を止め、
P3B が旧 read を止めたことで、その無料の互換性は失われている。上記が現在の手順である。

## conflicting の解消（**移行期の記録**）

> `conflicting`（新旧 namespace が異なる値）は P1〜P3A の分類である。P3B の binary は旧
> namespace を読まないので、この状態を検出しない——新 label の値どおりに読むだけである。
> 移行期の host を扱うときのために手順を残す。P3B 時点の fail-closed は `malformed` であり、
> こちらは resource を作り直さずに label を書き直せば解ける。

**Apple Container は既存 resource の label を変更できない。** `container` CLI に update/relabel 相当の
subcommand は無く（1.1.0 で確認）、label は create 時にしか設定できない。したがって解消は
「作り直す」しかなく、resource 種別ごとに危険度が違う。

**container** — data は named volume 側にあるので、container 自体は捨ててよい。

```sh
container inspect <name>            # どちらの label が古いかを確認する
container stop <name>               # running なら
container delete <name>             # agentopsctl ではなく raw CLI で消す
```

そのあと `agentopsctl start` に作り直させる。**container を消しても named volume の data は消えない。**

**network** — 状態を持たないので同じく作り直す。

```sh
container network delete <name>
```

**volume — ここだけは delete が data 破棄そのものである。** label を直せないので、
`container volume delete` は PostgreSQL の data を消すことを意味する。次のどちらかを選ぶ。

1. **触らない**（推奨）。conflicting な volume を持つ topology は起動できないが、data は保持される。
   Phase 2 の migration までそのまま待つ。
2. どうしても今直すなら、まず `container volume inspect <name>` で
   `configuration.source`（host 上の volume image path）を確認し、**data を退避してから**
   `container volume delete` → `agentopsctl start` で作り直し、退避した data を戻す。

いずれの場合も、解消の前後で inventory を取り直して `conflicting` が 0 件になったことを確認する。

## Phase 2: old-only container の掃討（**P3A で撤去済み**）

> **`agentopsctl migrate-labels --apply` は Phase 3A で撤去された。** 実装ごと削除されており、
> 呼んでも runtime に一切触れずに拒否される（CLI・library の両方で拒否し、拒否は listing より前に起きる）。
> 引数なしの `migrate-labels` は read-only inventory として**そのまま残る**。
>
> 理由: この sweep は container を削除して観測どおり作り直すことで label を移した。writer が
> 新旧両 namespace を書いていた間は「legacy-only → dual」になり正しかったが、P3A で writer が
> 新 namespace だけを書くようになると、同じ経路が **legacy-only → current-only を 1 手で**やることになる。
> これは Issue #123 が禁じている飛び越しであり、しかも sweep 自身の verify は dual を要求するので、
> **container を削除した後に**失敗して replacement を隔離する。
>
> 代替は `migrate-label-metadata`（下記「Phase 3A」節）。同じ label を 2 段階で動かし、**何も削除しない**。
>
> 以下は撤去された sweep の設計記録である。**手順として実行しないこと。**

### なぜ作り直すのか

**Apple Container 1.1.0 は既存 container の label を変更できない**（`container` に update/relabel 相当の
subcommand が無い）。したがって新 namespace を足す唯一の方法は **container を作り直すこと**である。
data は named volume 側にあるので container 自体は捨ててよい。**named volume は絶対に削除しない。**

replacement は **その container 自身の観測結果から再構築**する。topology の spec builder
（`agentopsctl start`）から作り直すのは image を rebuild して spec digest を変えてしまい、
label 移行ではなく再 deploy になる。再構築できない container は近似せず `blocked` にする。

### 手順

```sh
# 1. dry-run。ここで対象の exact id を確定する。host は変わらない。
agentopsctl migrate-labels

# 2. conflicting が 1 件でもあれば、ここで止める（上記「conflicting の解消」へ）。

# 3. exact id を明示して移行する。--only は --apply に必須である。
agentopsctl migrate-labels --apply --only <id1>,<id2>,...

# 4. 事後 inventory。pending 0 / conflicting 0 を確認する。
agentopsctl migrate-labels
```

**`--apply` は `--only` 無しでは必ず失敗する。** 「old-only な container 全部」は
その時 host に何が居るかで意味が変わる broad selector であり、Issue #123 が mutation を
禁じているのはまさにそれである。id を書かせることで、影響範囲が inventory の副作用ではなく
**operator が書いた決定**になり、shell history にも残る。id の重複も拒否する（対象件数が曖昧になるため）。

不安なら 1 件ずつ流してよい。sweep は container 単位で逐次実行し、最初の失敗で全体を止める。

### 各 container が通る段階と中断点

sweep は container ごとに次の順で進む。**どの段階で中断しても named volume の data は失われない。**

| 段階 | 何をするか | 中断したときの位置 | rollback |
| --- | --- | --- | --- |
| `re-inspect` | exact id を取り直し、**snapshot 時の観測と全項目一致すること**、image tag が作成時の digest を今も指すこと、ownership/role/spec が conflict でないことを**削除直前に**再検証する | 何も変わっていない | 不要。再実行するだけ |
| `stop` | running のときだけ graceful stop。**stopped のものは起動しない** | 元の container が stopped で残る（まだ legacy-only） | `container start <id>` |
| `delete` | exact id の container を削除。volume は触らない | **container が存在しない唯一の窓**。volume と data は無傷 | `pre-mutation-*.json` の `plannedSpecs[]` から再作成する（下記） |
| `volume-release` | 削除した container が listing から消え、どの container も当該 volume を掴んでいないことを polling で証明し、volume が今も存在することを確認する | read-only。位置は `delete` と同じ | 同上 |
| `recreate` | **snapshot 時に確定した spec** から replacement を作る（**元が stopped なら stopped のまま作る**）。volume がまだ排他 attach されている旨の既知 error のときだけ、下記の fail-closed reconciliation を経て有限回 retry する | replacement が存在する（retry 中なら存在しない） | replacement を削除し `plannedSpecs[]` から作り直す |
| `verify` | 元と replacement が label 以外すべて一致し、新旧両 namespace を持つことを証明する | drift 検出時は sweep 全体を停止し、以降の container に触れない | **自動修復しない。** running だった場合は replacement を **stop して隔離**し（削除はしない＝その構成の唯一の複製のため）、operator の判断事項として escalate する |

`verify` が失敗した replacement を自動で作り直さないのは意図的である。「同一だと証明できない」状態は
機械が繕ってよい状態ではない。

### named volume の保全

- sweep は **volume を作らず・消さない**。`sweep-*.json` の `volumes[]` に
  `presentBefore` / `presentAfter` が残る。
- Apple Container の named volume は **単一 VM へ排他 attach** される。だから
  `delete` → `volume-release` の証明 → `recreate` の順序を崩さない。証明できないまま timeout した
  場合は re-attach せずに失敗する（先に attach して replacement 側が VZ Code=2 で落ちる方が診断が難しい）。
- **volume の label は P2 では変わらない**（作り直す＝data 破棄のため）。これは既知の残課題であり、
  P3 の設計時に扱う。下記「P3 の entry gate」を見る。

### 停止と再開

sweep は container 単位で冪等である。中断したら **dry-run を取り直し**、まだ `pending` の
exact id だけを `--only` に渡して再開する。既に `dual` になったものは `skipped` になるので、
同じ id を二度渡しても作り直しは起きない。

### 中断後の再作成（`plannedSpecs`）

`pre-mutation-<stamp>.json` の `plannedSpecs[]` には、**対象ごとの replacement を作り直すのに必要な
構成**が入っている: image と digest、role、spec digest、network、named volume の mount（read-only
含む）、tmpfs、publish、user、entrypoint と command、readOnly / capDropAll / init、**cpus と
memoryMiB**、観測時の state と再作成 verb（`create` か `run`）。

working directory は 2 つ記録される。`workingDirOverride` は **argv で渡す override**（image 既定を
継承する場合は空）、`observedWorkingDirectory` は**観測された実効値**である。実効値だけを記録して
作り直すと、image から継承していたものを明示 override に変えてしまい、今日の挙動が同じでも
container の構成としては別物になる。sweep は**どれか 1 つでも再構築できなければ、1 件も mutate せずに停止する**
ので、この配列は常に全対象ぶん揃っている。

**環境変数は key だけが記録され、値は記録されない**（durable な evidence に credential を残さないため）。
手で作り直すときは値を運用側の設定から補う。値を補えない状態で `container run` を組み立てないこと。

### P2 の evidence（撤去後も読める）

`SweepReport` / `SweepStep` / `VolumePreservation` / `PlannedReplacement` の schema は
**merge 済みの `evidence/label-p2/*.json` を読むために残してある**（`TestMergedPhase2EvidenceStillDecodes`
が実際の記録を decode して回帰を止める）。schema を消して diff を小さくすることはしない。

### P2 の evidence 保全

`--apply` は**最初の mutation より前に** `pre-mutation-<stamp>.json` を書く。書けなければ sweep は
実行されない（監査も rollback もできない移行を始めないため）。file 名には run ごとの乱数 id が入り、
書き込みは `O_EXCL` である——evidence は不可逆な操作の記録なので、**既存 file を上書きしない**
（衝突したら失敗する）。sweep 後に `sweep-<stamp>.json` を
書く。**失敗した sweep でも書く**——どの段階で止まったかが必要になるのはその場合だからである。
halt した場合でも事後 inventory を書くので、「3 件移行して 4 件目で止まった」状態が evidence から読める。

引数なしの `migrate-labels` は既定では**何も書かない**。durable な控えが要るときだけ
`-evidence-dir <dir>` を渡す（read-only を名乗るものが worktree に file を落とさないため）。

evidence には secret 値・host path・環境変数値を入れない。記録するのは container の exact id、
state、image と digest、ownership/role/spec label、named volume の**名前**と mount 先、tmpfs、
network、分類と理由だけである（volume の host path は記録しない）。

### 同一性と volume release の扱い

Apple Container の container は**再利用可能な名前**で識別され、世代 id が無い。そこで sweep は:

- snapshot 時の**完全な観測**を記憶し、削除の直前に取り直した観測と**全項目比較**する。名前が同じでも
  中身が違えば「別の container が同じ名前を取った」として停止する（disposition だけを見ていると、
  同じく `pending` な別 container を掴んでしまう）。
- replacement は**snapshot 時に確定した spec** から作る。mutation 時に作り直すと、その時点で
  その名前に居るものを migrate してしまう。
- stop と delete の**直前に毎回** exact id を引き直して所有を再証明する。delete は
  「既に居なかった」を**成功ではなく失敗**として返す（別 actor が触っている証拠だから）。

それでも 1 呼び出しぶんの窓は消せない。volume の release 証明も「listing 上どの container も
掴んでいない」ことの証明であって attach の予約ではなく、**Apple Container は VM が block device を
手放し切る前に container record を消す**。そこで recreate は、排他 attach を示す既知の error
（`VZ error code=2` 等）に限り**有限回**だけ retry する。それ以外の error は即座に失敗させる。

**その reconciliation は fail-closed であり、決して delete しない。** Apple Container の名前には
世代 id が無いので、busy の後にその名前で見つかった record が「自分の失敗した create の残骸」だとは
**証明できない**——その隙に別の actor が同じ名前を取った可能性と区別が付かないからである。よって:

1. **各 attempt の前に名前が空いていることを証明する。** 既に何か居るなら、自分が作ったと言えないので
   その上に create しない。
2. busy の後に record が居た場合、それが**捕捉済みの移行前観測と `VerifyMigrationEquivalence` 互換**
   なら「create は結局成功していた」として**受理**する（作り直さない）。
3. 互換でなければ**そこで停止し、その record には触れずに残す**。operator の判断事項である。
4. retry するのは**名前が空いていて、かつ named volume がある**ときだけ。volume が無ければ排他 attach は
   retry で解ける類の話ではない。retry の前に listing 由来の release を取り直す。

**sweep 実行中に別の actor が同じ host の managed resource を触らないこと**は依然として前提である。

## Phase 3A: 旧 write の停止と obsolete label の掃討

### 何が変わったか

- **writer は新 namespace だけを書く**（`ownership.go` の `ownershipLabelArgs`）。以後 `agentopsctl` が
  作る container / network / volume は `current-only` になる。
- **reader は 6 分類のまま**である。host にはまだ移行前の resource が居るし、P1/P2 の binary は
  どちらの namespace も読むので **P2 binary への rollback は引き続き安全**である。
  失われるのは **P1 より前の binary へ戻す道**だけで、これは意図した片道である。
- 旧 namespace の **read 削除は P3B**（別 PR）。

### なぜ metadata を直接書くのか

**Apple Container 1.1.0 には既存 resource の label を変更する route が無い。** CLI に update/relabel が
無いだけでなく、apiserver の XPC route は `volumeCreate` / `volumeDelete` / `volumeInspect` /
`volumeList` / `networkCreate` / `networkDelete` / `networkList` が全てであり、公開されている
`ClientVolume` も `create` / `delete` / `list` / `inspect` / `volumeDiskUsage` しか持たない。

container は P2 のように作り直せる。しかし **volume を作り直すことは data 破棄そのもの**であり、
network も P3A では削除しない。したがって残る手段は **service を止めて Apple Container 自身の
metadata document を書き換えること**だけである。これは特権的な操作なので、専用 subcommand
`migrate-label-metadata` に閉じ込め、下記の gate を全部通らないと 1 byte も書かない。

### 書き換える document

| 種別 | document | label の位置 | 備考 |
| --- | --- | --- | --- |
| volume | `volumes/<name>/entity.json` | `labels` | `volume.img` には触れない |
| network | `networks/<name>/entity.json` | `labels` | |
| container | `containers/<id>/runtime-configuration.json` | `containerConfiguration.labels` | 常に存在する |
| container | `containers/<id>/config.json` | `labels` | **一度でも start した container だけ**に存在し、**listing はこちらを優先する** |

**container の document は 1 つとは限らない。** 未 start の container は
`runtime-configuration.json` しか持たず、start 済みは両方持つ。片方だけ書くと
「listing は変わったのに片割れが旧 label のまま」または「書いたのに listing が変わらない」に
なるので、**存在する document を全部書き、全部が一致していることを事前に要求する**。

### 安全装置（**P3A の設計記録**。現行は下記「P3B の rollback safety」）

`migrate-label-metadata --stage` は次を全部通らなければ実行されなかった。**stage は撤去済み**
なので、以下は当時の設計記録である。

- **appRoot は `container system status` から取る**（hardcode しない）。version は
  CLI・apiserver とも **`1.1.0` の exact allowlist**。layout は公開契約ではないので、
  未知 version は「止まって layout を人が見直す」が正しい。
- **対象は operator が書いた exact id だけ**（`kind/identity`）。重複・不在・path 区切りを含む
  identity は拒否する。broad selector は無い。
- **symlink 拒否**: document 本体と親 directory を lstat し、appRoot からの各 component も検査する。
- **regular file / owner / mode 検査**: 現在の user 所有で、group・other から書けない regular file だけ。
- **round-trip guard**: parse した document を書き戻して **元の byte と完全一致すること**を先に証明する。
  再現できない document は「この tool が理解できていない」ので**書かずに拒否する**。
  これにより、完了後の diff は **labels field 以外に出ない**。
- **ownership pair 検査**: `conflicting`・片側だけ書かれた pair・`unmanaged`・`missing-label` は拒否。
- **service 停止の二重証明**: `container system status` が落ちること **かつ** data plane 呼び出しが
  失敗すること。片方だけでは半分生きた runtime を通してしまう。
- **running managed container が 1 つでもあれば実行しない。**
- **before/after hash・O_EXCL backup・同一 directory の temp file・mode/owner 保持・
  fsync → atomic rename → directory fsync。**
- **事後は runtime の API で検証する。** 自分が書いた file を読み返しても「writer が自分と一致した」
  ことしか言えない。意味があるのは Apple Container が何を報告するかである。

### backup は credential store である（repository に置かない）

> **P3B での変更**: `--backup-dir` / `AGENTOPS_LABEL_BACKUP_ROOT` / `$XDG_STATE_HOME` 既定は
> `ResolveBackupRoot` ごと撤去された（forward run が無いので backup root を*選ぶ*処理も無い）。
> 現行の rollback は plan から backup root を導き、`RollbackPlan.BindToHost` が
> **0700・現 user 所有・git work tree の外・symlink でない**ことを確認する。下記の規則は
> 「どこに置くべきか」としてそのまま有効であり、強制する場所が変わっただけである。

**`containers/<id>/config.json` は `initProcess.environment` を値ごと持つ。** 本 project の topology では
`POSTGRES_PASSWORD` がここに入る。backup はその document の**逐語コピー**なので、backup directory は
artifact ではなく **credential store** である。したがって:

- backup root は **`--backup-dir` 省略時 `$XDG_STATE_HOME/agentops/label-metadata-backups`**
  （無ければ `~/.local/state/...`）。`AGENTOPS_LABEL_BACKUP_ROOT` でも指定できる。
- **git work tree の中は拒否する。** 祖先を辿って `.git` があれば実行しない（worktree の `.git` file も検出する）。
  checkout の中から sweep を回した operator が、生きた credential の複製を stage・commit・push できないようにする。
- **root は 0700 で作り、既存が広ければ絞り直す。** backup file 自体は 0600。

### evidence と rollback plan は別物である

| | 置き場所 | 中身 | commit するか |
| --- | --- | --- | --- |
| **evidence** | `--evidence-dir`（既定 `evidence/label-p3a/`） | identity・ownership class・**3 つ**の label key と**固定 token 化した値**（`managed` / `present` / `digest-shaped` / `unrecognized-value` / `blank`）・digest・**appRoot / backup root からの相対 path** | **する** |
| **rollback plan** | backup root の中（0600） | rollback に必要な**絶対 path** | **しない** |

**sanitize した evidence では rollback できない**（絶対 path を持たないため）。だから 2 つに分ける。
evidence には host path も document の中身も入らない――`TestCommittedEvidenceCarriesNoHostPath` が回帰を止める。

### 手順（**P3B で撤去済み。実行しないこと**）

> **`migrate-label-metadata --stage` は Phase 3B で撤去された。** `--stage` / `--apply` /
> `--only` / `--evidence-dir` / `--backup-dir` を渡すと、runtime に一切触れずに理由を出して
> 拒否される。両 stage とも 2 つの namespace を比較して書き込む処理であり、読む側が 1 つに
> なった以上成立しない。**残っているのは `--rollback` だけ**である（上記「Rollback」節）。
>
> 以下は撤去された staged migration の設計記録である。

```sh
# 1. read-only の plan。host は変わらない。全 managed resource の分類と対象 document が出る。
agentopsctl migrate-label-metadata --stage prepare

# 2. prepare: legacy-only の volume/network に current pair を足して dual にする。
agentopsctl migrate-label-metadata --stage prepare --apply \
  --only volume/agentops-postgres-data,network/agentops-internal,... \
  --evidence-dir evidence/label-p3a      # backup は既定の private root へ

# 3. 全 managed resource が dual / current-only になったことを確認する。
agentopsctl migrate-label-metadata --stage retire

# 4. retire: legacy pair を落として current-only にする。
agentopsctl migrate-label-metadata --stage retire --apply --only <...> \
  --evidence-dir evidence/label-p3a

# 5. rollback が要るとき。private backup root の rollback-plan.json を渡す
#    （commit される evidence ではない。--apply の最後に path が出る）
agentopsctl migrate-label-metadata --rollback <backup-root>/retire-<stamp>/rollback-plan.json
```

**`--stage retire` は、host のどこかに `legacy-only` が残っている限り拒否される。** 対象を絞っても
拒否される――「そこだけ安全」ではなく「全体として旧 label が冗長になった」ことが retire の条件だからである。

### rollback の 2 つの mode

**Apple Container は `container system start` のたびに `volumes/*/entity.json` を書き直す。**
値は同じだが key 順が変わる（Foundation の dictionary 順は run 間で安定しない）。network の
entity.json は書き直されない。したがって:

- **`bytes`** — document が sweep の書いた通りなら、backup の byte を丸ごと戻す。
- **`relabelled`** — runtime が再直列化していた場合。**labels 以外の全 field が値として一致すること**と
  **labels が sweep の書いた通りであること**を証明したうえで、現在の document に移行前 labels を書き戻す。
  古い byte を被せると、その後 runtime が記録した内容を巻き戻してしまうためこうする。
- それ以外（labels 以外が変わっている等）は **拒否する**。rollback という名前の未 review な mutation を
  しないためである。

さらに、**sweep 後に生まれた document** も rollback は面倒を見る。container を start すると
runtime が in-memory model から `config.json` を作るので、記録済み document だけ戻すと
「片方だけ戻った container」になる。そこで rollback は、既知 document のうち記録に無いものが
現れていたら、**その labels が sweep の書いたものと完全一致することを確認したうえで**移行前 labels を
書き込む（`reconciled` に記録される）。一致しなければ拒否する。

### grounded 検証

```sh
AGENTOPS_TEST_APPLE_CONTAINER=1 \
AGENTOPS_TEST_APPLE_IMAGE=<shell を持つ image。例 agentops-postgres:dev> \
go test ./apps/control-plane/internal/lifecycle/ -run AppleContainerMetadata -v -count=1
```

固有 prefix の使い捨て container / volume / network を作り、volume に sentinel を書いてから
system stop → prepare → start → retire → start → **volume.img の size と mtime が sweep を跨いで
不変であること** → sentinel が読めること → container が start/stop できること →
rollback（dual へ）→ reapply（current-only へ）→ 旧 namespace の残渣 0 を確認し、最後に全て削除する。
image は `/bin/sh` を持つ必要がある（`--entrypoint` で override する）。

### P3A でやらないこと

- 旧 namespace の **read 削除**（P3B）。
- container / network / volume の **削除・再作成**。P3A は 1 つも消さない。
- `unmanaged` / `missing-label` の resource への操作。**件数を減らそうとしない。**

## P3 の entry gate

次を**すべて**満たすまで P3 へ進まない。

- `agentopsctl migrate-labels` の事後 inventory で **`pending` 0 / `conflicting` 0 / `blocked` 0**。
- dual label 観測窓（**最低 20 分**）で ownership と attachment の回帰が 0 件。最低 3 サンプル:
  移行直後・restart/reconcile 後・窓の終了時。
- grounded Apple Container で drain/recreate、排他 volume の detach/attach、restart 整合、
  旧 binary reader へ戻せる rollback predicate が確認済み。
- ~~**未解決の残課題**: managed な volume / network は依然 `legacy-only` である~~
  **P3A で解決済み。** `migrate-label-metadata` が service を止めて Apple Container の
  metadata document を書き換えることで、volume を 1 つも削除せずに
  `legacy-only` → `dual` → `current-only` を通す。上記「Phase 3A」節を見る。

### P3B の entry gate（**充足済み**）

P3B（旧 read の削除）へ進む条件と、その充足状況。

- `agentopsctl migrate-label-metadata --stage retire` の plan で、**host の `legacy-only` が 0 件**。
  container だけでなく **volume と network も 0 件**であること。
  → **充足。** 実 host の read-only inventory は 7 container / 23 volume / 8 network、
  うち 32 件が `current-only`、`legacy-only` / `dual` / `conflicting` は 0 件、
  `com.mrbaron3.workflow.*` の key は 1 つも存在しない。
- P3A の掃討後に **20 分以上・3 サンプル以上**の観測窓で ownership／attachment の回帰が 0 件。
  → **充足。** `evidence/label-p3a/observation-window.json`（21m17s / 3 サンプル）。
- 旧 read を消しても `EnsureVolume` / `EnsureNetwork` が自分の resource を所有と読めること
  （＝全 managed resource が `current-only`）。
  → **充足。** 上記 32 件が `current-only`。

## Phase 3B: 旧 read の削除

### 何が変わったか

- **reader は `com.mrbaron3.servo.*` だけを見る。** `ClassifyOwnership` / `RequireOwned` /
  `RequireManaged` / `RequireRole` / `RequireSpecDigest`、inventory、reconcile、attachment lookup、
  operator diagnostics のすべてが新 namespace 単独で判定する。
- **分類が 6 から 4 になった**（上記「分類（P3B 以降）」）。`legacy-only` / `current-only` / `dual`
  は `owned` に統合、`conflicting` は `malformed` に置き換え。
- **旧 namespace だけの resource は非所有**になった。`missing-label` として扱われ、
  採用も変更も削除もされない。
- **production source に旧 key の参照が 1 つも無い。**
  `TestNoProductionCodeReferencesTheLegacyNamespace` が repository 全体を走査して回帰を止める。
  Go は AST の string literal を、TypeScript・script・manifest は plain text を見る。
  comment は対象外——この境界は説明されるべきものだからである。`evidence/` `docs/` `_test.go` は
  互換性の歴史を意図的に記録する場所なので除外する。
- **前へ進める migration は 1 つも残っていない。** `migrate-labels --apply` は P3A で、
  `migrate-label-metadata --stage` は P3B で撤去された。両方とも runtime に触れる前に拒否する。
- **`migrate-label-metadata --rollback` は残る。** label key を一切解釈せず、記録済みの label map を
  逐語で書き戻すだけなので、このbinaryが読めない namespace でも正しく復元できる。

### P3B の rollback safety

**plan は host に束縛してから実行する。** plan の path・digest・label map はすべて file から
逐語で読んだ値であり、「内部整合している」ことは偽造 plan がまさに満たす条件である。
`RollbackPlan.BindToHost` が **service を止める前に**次を全部証明する。

- kind が既知で、identity が directory を脱出しないこと。
- 各 document の path が `<appRoot>/<kind directory>/<id>/<既知の document 名>` を
  **再構成したもの**と完全一致すること（比較ではなく再構成である）。
- label path を plan からではなく layout から取ること
  （偽造 labelPath で labels 以外の field を書き換えさせない）。
- appRoot からの各 component が symlink でないこと。
- **全 backup が単一の canonical root の下**の同じ `kind/id/document` 位置にあること。
  その root が **0700・現 user 所有・git work tree の外・symlink でない**こと。
- 各 backup の **bytes を hash し**、plan が記録した digest と一致し、かつ plan が復元すると
  主張する `BeforeLabels` を実際に含んでいること。
- **記録された before→after 変換を再導出**し、backup へ `AfterLabels` を書いたものが
  plan の `AfterSHA256` を再現すること（digest の自己言及を破る唯一の検査）。
- resource と document の重複を拒否すること（触る件数が曖昧な run をしない）。

さらに、**stop する前の not-running 検査は plan の対象 container を名前で見る**。中断した
rollback が旧 namespace へ戻した resource は `missing-label` になり分類では見えなくなるが、
再開時にまさにそれらを書き換えるためである。

**live document の検査も stop より前に 1 回行う。** 適用できない plan のために runtime を
止めない。stop 後にもう一度同じ検査を行うのは冗長ではない——Apple Container は
`system start`/`stop` を跨いで `volumes/*/entity.json` を書き直すので、実際に書き換える
document は後者だからである。

**backup path も 1 component ずつ検査する。** root だけを見ると `<root>/<kind>` を symlink に
差し替えられる。rollback は backup を**読む**だけでなく created-since の copy を**書く**ので、
document path と同じ扱いにする。

**runtime の再起動は「1 度だけ」走る closure で、呼び口が 2 つある。**

- **stop より前に fallback を defer 登録する。** 途中で失敗した stop も service を落として
  いる可能性があり、その error path で復旧が登録されていなければ、operator の machine は
  container runtime を失ったまま残る。
- **起動と証明は同じ closure の中にある。** 検証を closure の外に置くと、fallback 経路
  （stop の部分失敗・cancel・復元失敗）は runtime を起動するだけで**確認しない**。
  `system start` が 0 を返しつつ apiserver が落ちたままなら、operator は「rollback が
  失敗した」としか知らされず、runtime が落ちていることは 1 文字も出ない。
- **成功路では明示的に restart してから検証する。** defer だけでは足りない——関数本体の
  末尾に書いた検証は**どの defer よりも先に**走るので、成功した rollback のたびに
  「止まっている runtime」を読んで、起きていない失敗を報告することになる。
- **`system start` は多くとも 1 回**（`restarted` flag）。deferred fallback が、明示呼び出しが
  既に報告した start をやり直さない。
- **defer の restart 失敗は返り値へ join する**（named result）。defer は print では exit status を
  変えられず、「rollback が失敗し、かつ runtime が落ちている」は片方だけとは別の事故である。
- restart context は `context.WithoutCancel` で作る。SIGINT は rollback を中断してよいが、
  復旧を中断してはいけない。

### P3B でやらないこと

- host への破壊的操作。P3B は code / test / docs だけの変更であり、
  container・volume・network を 1 つも作らず・消さず・書き換えない。
- `unmanaged` / `missing-label` の resource への操作。**件数を減らそうとしない。**
  旧 namespace だけを持つ resource が仮に現れても、掃討対象にしない。

## grounded 検証の実行

実機 Apple Container 上で label の round-trip と分類、そして P3B の一方向境界を接地する:

```sh
AGENTOPS_TEST_APPLE_CONTAINER=1 \
AGENTOPS_TEST_APPLE_IMAGE=<手元にある image reference> \
go test ./apps/control-plane/internal/lifecycle/ -run AppleContainer -v -count=1
```

実行結果は `evidence/label-p3b/grounded-<stamp>.json` に残す（identity・分類・state・
volume attachment だけを記録し、host path も環境変数値も raw な label 値も入れない）。
**この suite が接地しないこと**も同じ file に書く: binding 失敗が `StopSystem` より前に
起きることは、suite が既に runtime を止めた後に bind するため接地できない。順序の証明は
command test `TestRollbackNeverStopsTheRuntimeWhenThePlanDoesNotBind` が持つ。
`legacyKeysPresent: 0` は **P3B の binary の分類では出せない**（legacy-only と missing-label は
設計上区別できない）ので、read-only listing から key を直接数えた結果として記録する。

P3B が実機で証明すること:

- 新 namespace だけを書いた resource を runtime が返し、reader が `owned` と読む。
- **旧 namespace だけの volume / container を refuse し、かつ消さない。**
  refuse したあとも resource と label がそのまま残っていることまで確認する
  （排他 attach の named volume を「空いた名前」と誤読しないこと）。
- rollback（`-run AppleContainerMetadata`）が document を逐語 bytes へ戻し、runtime が
  戻った label を報告し、**その resource がこの binary から見えなくなり**、それでも
  volume の中の sentinel は無傷であること。一方向境界そのものの接地である。

P2 の掃討だけを接地する場合（`/bin/sh` と `/bin/sleep` を持つ image が必要）:

```sh
AGENTOPS_TEST_APPLE_CONTAINER=1 \
AGENTOPS_TEST_APPLE_IMAGE=<手元にある image reference> \
go test ./apps/control-plane/internal/lifecycle/ -run AppleContainerSweep -v -count=1
```

P2 の grounded suite は probe container の volume に sentinel を書き、container の
削除・再作成と restart を跨いで**その中身が残ること**まで確認する。sweep は probe の exact id へ
`Only` で限定されるため、同じ host で稼働している managed topology は対象にならない。

環境変数が無ければ suite ごと skip する。**opt-in したのに前提が欠けている場合は skip ではなく fail する**
（skip は exit 0 なので、gate が「証明が無い」を「証明した」と読んでしまう）。
検証用 resource は固有 prefix 付きで作られ、label selector による一括削除は行わず、実行後に自動削除される。
稼働中の managed topology には触れない。

## 保全する evidence

| 何を | どこに | いつ |
| --- | --- | --- |
| grounded Apple Container run（新旧 label の実 round-trip、6 分類、rollback predicate） | `evidence/label-p1/apple-container-dual-label-smoke.json` | P1 merge 前 |
| 移行前 host の read-only inventory（分類ごとの件数と container 一覧） | 同上 `readOnlyHostInventory` | P1 merge 前と、P2 の掃討前後 |
| local validation（Go test / vet / typecheck） | PR 本文の Validation 節 | 各 phase の PR |
| grounded Apple Container run（drain/recreate、排他 volume の detach/attach、volume data 保全、restart 整合、rollback predicate） | `evidence/label-p2/apple-container-sweep-smoke.json` | P2 merge 前 |
| grounded Apple Container run（新 namespace 単独 read、legacy-only の refuse かつ非削除、plan の host 束縛、rollback の逐語復元と一方向境界、volume sentinel 保全、前後の host 不変） | `evidence/label-p3b/grounded-<stamp>.json` | P3B merge 前 |
| P2 の bounded audit（pending / migrated / skipped / conflicting / blocked） | `evidence/label-p2/pre-mutation-<stamp>.json` と `sweep-<stamp>.json`（subcommand が自動生成） | 掃討の直前と直後 |
| dual label 観測窓のサンプル（最低 3 点・20 分以上） | `evidence/label-p2/inventory-<stamp>.json` | 移行直後・restart 後・窓の終了時 |

evidence には secret 値、host path、credential を入れない。`agentopsctl` の error も同じ方針で
redact 済みであることを前提にする。

## P2 でやらないこと

- 旧 namespace の write 停止・read 削除（P3）。**旧 label は 1 つも消さない。**
- named volume / network の label 移行（作り直し＝data 破棄になるため。P3 の設計事項）
- obsolete label の sweep（P3）
- control-store schema 変更、release receipt の wire 変更、無関係な製品名 cleanup（Issue #123 の非目標）
- `unmanaged` / `missing-label` の resource への操作。**件数を減らそうとしない。**
