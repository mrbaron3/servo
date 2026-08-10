# container ownership label 移行（`com.mrbaron3.workflow.*` → `com.mrbaron3.servo.*`）

対象は [Issue #123](https://github.com/mrbaron3/servo/issues/123) と
[ADR-0022](../decisions/ADR-0022-servo-product-and-agentops-component-naming.md)。
container label は表示名ではなく**互換性 identifier** である。`agentopsctl` はこの label だけで
「この container / network / volume は自分の所有か」を判定する。writer と reader の片側だけを
変更すると稼働中の container が孤児化し、Apple Container の named volume は単一 VM へ**排他 attach**
されるため、置換 container も起動できなくなる。だから 3 段階に分ける。

label key の正典は `apps/control-plane/internal/lifecycle/ownership.go` **1 箇所だけ**である。
新しい判定箇所を足すときも、key 文字列を書かず必ずこの file の関数を通す。

## 分類（Phase 1 以降で共通）

| 分類 | 条件 | 所有か | 扱い |
| --- | --- | --- | --- |
| `legacy-only` | 旧 namespace のみ `=v1` | 所有 | 移行前に作られた container。P2 の移行対象。 |
| `current-only` | 新 namespace のみ `=v1` | 所有 | P3 後の姿。P1/P2 でも読める。 |
| `dual` | 新旧とも `=v1` | 所有 | P1 の writer が作った姿。 |
| `conflicting` | 新旧が**異なる値**（片側が空文字も含む） | **判定不能** | fail-closed。unowned として掃討しない。 |
| `unmanaged` | key はあるが値が `v1` でない | 非所有 | 別 deployment の名前。触らない。 |
| `missing-label` | どちらの key も無い | 非所有 | 触らない。 |

`conflicting` を `unmanaged` と同一視しないことが本移行の安全性そのものである。`unmanaged` は
「他人のもの」を意味し、後続 phase の掃討判断で使われる。部分移行した自分の container を
そこへ落とすと、排他 attach 中の volume を持ったまま削除・再作成の対象になり得る。

## Phase gate

| Phase | 入る条件 | 出る条件（次へ進む gate） |
| --- | --- | --- |
| **P1 dual label**（本 PR で実装済み） | なし | 新規作成 resource が新旧両 label を持ち、reader が上表 6 分類を明示し、旧 binary へ戻しても発見できることを grounded に確認済み |
| **P2 旧 container 掃討**（本 PR で実装済み） | P1 が merge 済みで、稼働 host の inventory が取れている | old-only container が 0 件、移行・skip・conflict・block の bounded audit が残り、Apple Container 上で drain/recreate と volume detach/attach、restart 整合、rollback を grounded に確認済み |
| **P3 新 label のみ**（未実装） | P2 の gate を満たし、dual label 観測窓で ownership／attachment の回帰が無い | 旧 write 停止 → 旧 read 削除の順で別々に review・merge され、全 managed container の新 label ownership が証明済み |

**P1 から P3 へ直接飛ばない。** 旧 write と旧 read は同じ PR で消さない（write を先に止める）。

## Inventory command

P2 以降は専用 subcommand を使う。**引数なしの `migrate-labels` は read-only の inventory であり、
host を一切変更しない**（安全な綴りを短い方に割り当てている）。

```sh
agentopsctl migrate-labels                       # dry-run。分類・named volume・件数を出す（file は書かない）
agentopsctl migrate-labels -evidence-dir <dir>   # 上記に加えて durable な控えを <dir> へ書く
```

引数なしの `migrate-labels` は **stdout に出すだけで file を書かない**。read-only を名乗るものが
worktree に file を落とさないためである。観測窓のサンプルなど控えが要るときだけ `-evidence-dir` を渡す。
`--only` は `--apply` 専用で、inventory には渡せない（gate として読む件数が host 全体である必要があるため）。

出力の `pending` が P2 の移行対象（`legacy-only` かつ忠実な replacement を再構築できるもの）、
`blocked` は所有しているが**同一の replacement を作れない**もの、`conflicting` は部分移行、
`skipped` は移行不要（`dual` / `current-only`）または非所有（`unmanaged` / `missing-label`）である。

subcommand が使えない状況（binary が無い等）では Apple Container の listing を直接分類する。
**listing は read-only であり、label selector による一括削除は行わない。**

```sh
container list --all --format json | python3 -c '
import json, sys, collections
LEG = "com.mrbaron3.workflow.agentopsctl"
CUR = "com.mrbaron3.servo.agentopsctl"

def classify(labels):
    legacy, current = LEG in labels, CUR in labels
    if not legacy and not current:
        return "missing-label"
    if legacy and current:
        if labels[LEG] != labels[CUR]:
            return "conflicting"
        return "dual" if labels[LEG] == "v1" else "unmanaged"
    value = labels[LEG] if legacy else labels[CUR]
    if value != "v1":
        return "unmanaged"
    return "legacy-only" if legacy else "current-only"

counts = collections.Counter()
for item in json.load(sys.stdin):
    configuration = item["configuration"]
    kind = classify(configuration.get("labels") or {})
    counts[kind] += 1
    print(configuration["id"], item["status"]["state"], kind)
print("---", dict(counts))
'
```

`volume list` / `network list` は `item["configuration"]["labels"]` と `item["id"]` で同じ分類ができる。

読み方:

- `conflicting` が 1 件でもあれば、**そこで止める**。`agentopsctl` はその resource に触れる操作を
  fail-closed で拒否する。error は**食い違っている 2 つの key 名だけ**を出し、label の値そのものは出さない
  （label 値は事故や外部由来の任意文字列で、durable な lifecycle failure record にも残るため）。
  どちらの値が古いかは `container inspect <name>` で確認する。解消手順は下記「conflicting の解消」を見る。
- `conflicting` の error は drift の error と区別される。「DRAINING して stop して restart」を促す文言が
  出たらそれは drift であって部分移行ではない。**部分移行に対して drift の手順を実行しない**
  （排他 attach 中の named volume を持つ container を削除・再作成することになる）。
- `legacy-only` が残っている限り P3 の gate は満たさない。
- `unmanaged` / `missing-label` は移行対象ではない。数を減らそうとしない。

## Rollback（P1 → 移行前 binary）

P1 の writer は新旧**両方**の label を書く。旧 binary は旧 namespace しか読まないが、
それは常に存在するので、P1 が作った container / network / volume はそのまま発見できる。

1. 移行前 revision の `agentopsctl` を用意する（build し直すか、以前の binary を使う）。
2. 稼働 topology を `agentopsctl drain` → `stop` で止める必要は**無い**。label は変えないので、
   旧 binary はそのまま `status` / `start` を継続できる。
3. 旧 binary で `agentopsctl status` を実行し、control / triage / runner / postgres が
   期待どおり認識されることを確認する。
4. 以後、旧 binary が新しく作る container は `legacy-only` になる。再び P1 の binary へ進めても
   `legacy-only` は所有として読めるので、行き止まりにならない。

戻せない状況が 1 つだけある: 手動または外部 tool で**新旧の値を食い違わせた**場合
（`conflicting`）。これは P1 の binary でも旧 binary でも安全に扱えないので、
下記「conflicting の解消」を先に行ってから rollback する。

## conflicting の解消

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

## Phase 2: old-only container の掃討

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

## P3 の entry gate

次を**すべて**満たすまで P3 へ進まない。

- `agentopsctl migrate-labels` の事後 inventory で **`pending` 0 / `conflicting` 0 / `blocked` 0**。
- dual label 観測窓（**最低 20 分**）で ownership と attachment の回帰が 0 件。最低 3 サンプル:
  移行直後・restart/reconcile 後・窓の終了時。
- grounded Apple Container で drain/recreate、排他 volume の detach/attach、restart 整合、
  旧 binary reader へ戻せる rollback predicate が確認済み。
- **未解決の残課題**: managed な **volume / network は依然 `legacy-only`** である。P2 は
  container だけを移行する（volume の label を直すには volume を作り直す必要があり、それは
  data 破棄そのものだから）。P3 が旧 namespace の **read** を消すと、これらは `missing-label`＝
  非所有に落ち、`EnsureVolume` / `EnsureNetwork` が自分の resource を拒否する。**P3 は
  「旧 read の削除」の前に volume/network の移行方式を決めなければならない。**

## grounded 検証の実行

実機 Apple Container 上で label の round-trip と 6 分類、rollback predicate を接地する:

```sh
AGENTOPS_TEST_APPLE_CONTAINER=1 \
AGENTOPS_TEST_APPLE_IMAGE=<手元にある image reference> \
go test ./apps/control-plane/internal/lifecycle/ -run AppleContainer -v -count=1
```

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
