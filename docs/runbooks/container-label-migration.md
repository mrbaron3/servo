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
| **P2 旧 container 掃討**（未実装） | P1 が merge 済みで、稼働 host の inventory が取れている | old-only container が 0 件、移行・skip・conflict・block の bounded audit が残り、Apple Container 上で drain/recreate と volume detach/attach、restart 整合、rollback を grounded に確認済み |
| **P3 新 label のみ**（未実装） | P2 の gate を満たし、dual label 観測窓で ownership／attachment の回帰が無い | 旧 write 停止 → 旧 read 削除の順で別々に review・merge され、全 managed container の新 label ownership が証明済み |

**P1 から P3 へ直接飛ばない。** 旧 write と旧 read は同じ PR で消さない（write を先に止める）。

## Inventory command

P1 時点では専用 subcommand は無い（inventory subcommand は P2 の作業）。Apple Container の
listing を直接分類する。**listing は read-only であり、label selector による一括削除は行わない。**

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
  fail-closed で拒否する。どちらの値が古いかを `container inspect` で確認し、手で解消してから続行する。
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
label を手で揃えてから rollback する。

## 保全する evidence

| 何を | どこに | いつ |
| --- | --- | --- |
| grounded Apple Container run（新旧 label の実 round-trip、6 分類、rollback predicate） | `evidence/label-p1/apple-container-dual-label-smoke.json` | P1 merge 前 |
| 移行前 host の read-only inventory（分類ごとの件数と container 一覧） | 同上 `readOnlyHostInventory` | P1 merge 前と、P2 の掃討前後 |
| local validation（Go test / vet / typecheck） | PR 本文の Validation 節 | 各 phase の PR |
| P2 の bounded audit（migrated / skipped / conflicting / blocked） | P2 で定義する | P2 |

evidence には secret 値、host path、credential を入れない。`agentopsctl` の error も同じ方針で
redact 済みであることを前提にする。

## 本 phase でやらないこと

- 稼働・停止 container の掃討／再作成（P2）
- 旧 namespace の write 停止・read 削除（P3）
- control-store schema 変更、release receipt の wire 変更、無関係な製品名 cleanup（Issue #123 の非目標）
