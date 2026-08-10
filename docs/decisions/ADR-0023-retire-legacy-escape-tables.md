# ADR-0023: writerのないlegacy escape tableをread-only archiveへretireする

- 状態: 採択・吸収・実装済み
- 関連: [ADR-0013](ADR-0013-postgresql-control-plane-source-of-truth.md)、
  [ADR-0020](ADR-0020-release-receipt-evidence.md)

## 文脈

ADR-0013で導入した`released_builds` / `build_defects`は、panel承認後に見つかったdefectを
buildへ結び、false-passやescapeを観測する実験モデルだった。table、TypeScript store API、integration testは
存在したが、production writerは一度も接続されなかった。この状態で0行を表示すると、単なる未計測を
「escape 0件」という品質証拠に読み違えられる。

同じ期間にproduction release evidenceは`releases`、`release_receipt_outbox`、head-bound review/finding receiptへ
移行した。旧`build_defects.severity`の`low|medium|high|critical`も、evaluation domainの正典
`blocker|major|minor`と二重化していた。writerを新設して二つのrelease modelを再び並走させるより、誤解を生む
active surfaceを閉じ、存在し得るhistorical rowだけを保全する方が現在の境界に合う。

## 決定

1. legacy escape modelをproduction aggregateとして廃止する。新しいwriter、dual-write、0件metricは作らない。
2. migration 0027は両tableをexclusive lockし、既存severityを次の決定論規則で正規化する。
   `low → minor`、`medium → major`、`high|critical → blocker`。
3. rowは削除せず、tableを`retired_released_builds` / `retired_build_defects`へ改名してhistorical archiveとして残す。
   archiveにはcanonical severity constraintと「absenceをzero escapeと解釈しない」commentを付け、
   application roleの権限を剥がす。
4. `apps/agentops`から旧`BuildDefect`型とrecord/list/false-pass APIを削除する。production releaseの品質事実は
   `releases`と`release_receipt_outbox`上のreview、finding lineage、grade、merge receiptだけから読む。
5. 旧rowをrelease receiptへbackfillしない。因果関係、head epoch、authority receiptを持たないrowから、
   canonical release evidenceを発明しない。

## 帰結

- historical rowは失われないが、通常applicationからactive metricとして照会できない。
- `blocker|major|minor`以外のseverityを新しい契約へ持ち込まない。
- escape観測を将来再導入する場合は、receipt model上のproduction writer、分母、観測期間、未計測状態を含む
  新しいADRとPublished Languageが必要である。retired table名を復活させない。
- migrationはmetadata/table renameを伴うため、実行中writerがないことを前提に短いexclusive lockを取る。
  実データ量とlock時間は各環境のmigration前に別途観測する。

## 実装先 id

- architecture: `ARCH-control-store-003`
- domain-model: `DOM-control-store-005`
- data-model: `DATA-control-store-008`
- ubiquitous-language: `LANG-control-store-010`
