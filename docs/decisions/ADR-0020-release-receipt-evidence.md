# ADR-0020: live release evidenceをjob topologyからrelease receiptへ移す

- 状態: 採択・production配線済み（external-target live run待ち）
- 所有 Issue: [#110](https://github.com/mrbaron3/servo/issues/110)
- 関連: [ADR-0009](ADR-0009-pr-native-autonomous-review-and-delivery.md)、
  [ADR-0013](ADR-0013-postgresql-control-plane-source-of-truth.md)、
  [ADR-0015](ADR-0015-postgresql-fenced-isolated-runner.md)、
  [ADR-0017](ADR-0017-private-repository-monitor-broker.md)

## 文脈

`contracts/live-release-evidence.schema.json`のversion 1は、triage、development、review、mergeが
一つの既知のjob列として進むことを証明対象にしていた。そのため、成功したreleaseであっても、human readyが
AI triageより先に付いた場合、generatorとmergeが別jobの場合、repository graderとGitHub CheckRunが別の
gate sourceである場合、providerのdefault modelを使った場合に、存在しないinvocation、同一job ID、架空の
GitHub check、架空のmodel名を要求し得た。

job-local JSON store、PostgreSQL、merge後のGitHub current stateを事後に結合する方式では、一つのjobのretry、
cleanup failure、artifact欠落がrelease全体の証明を壊す。逆に契約を単にoptional化すると、別repository、別Issue、
別headの断片を貼り合わせられる。公開契約が必要とするのはworkflowの形ではなく、誰の権威で、どのheadが作られ、
何を検証し、どのfindingを後続headが解消し、どのexpected headがmergeされたかという因果関係である。

## 決定

1. 一回のreleaseを、Registration内で一意な`release_id`と`release_key`を持つdurable aggregateとして扱う。
   triage/promotion、development、PR reconciliation、review、merge、retry/recoveryの各jobは同じ`release_id`へ
   linkできる。job/attempt IDはproducer provenanceであり、receiptの同一性条件ではない。
2. releaseの事実を、`authority`、`build`、`grade`、`review`、`finding-resolution`、
   `runtime-provenance`、`merge-intent`、`merge`、`intervention`のimmutable receiptとして保存する。
   全receiptは`receipt_id`、release内idempotency key、repository、Issue、release、recorded timeを持ち、
   `causes`で同じrelease内の先行receiptだけを参照する。大きなlogやsnapshotはartifact URI/digestとして外置きする。
3. `authority`は`human-ready`と`ai-triage-then-human-ready`を別variantにする。Registration policyが
   `ai-triage-required`ならtriage decision/source digest receiptのないpromotionとmerge authorizationを拒否する。
   human readyを許すpolicyでは、実行されなかったtriage invocationを生成しない。
4. buildは生成したhead、reviewは読んだheadとhead epoch、gradeはheadと`repository-grader|github-check`の
   source/name、finding resolutionはfindingを挙げたreviewと解消したbuild/headへ束縛する。review roundはjob数でなく、
   head epochとfinding lineageから評価する。required signal sourceを入れ替えた証拠は同名でも別物として拒否する。
5. runtime provenanceはconsumer revision、実行環境reference/digest、各invocationのprovider/model selectionを
   保存する。modelは具体名を観測できる場合の`explicit`、またはresolver identity/digestで再検証できる
   `provider-default`のどちらかとし、不明値を具体値へ捏造しない。
6. receiptはPostgreSQL transactional outboxへ不可逆なmergeより前に記録する。merge直前にcertifierが
   release/repository/Issue/expected head、authority policy、required gates、required perspectives、head epochs、
   finding lineage、runtime provenanceを検証し、`merge-intent`のinsertと`merge-authorized`遷移を一transactionで
   commitする。GitHub mergeはその後だけ許可する。
7. GitHubが同じPR/expected head/merge SHAの成立を返すretry/recoveryは、durable authorizationと照合して
   同一`merge` receiptへ冪等に収束する。異なるhead、未認可merge、別releaseのreceiptはfail closedにする。
   merge成立後のartifact publish、source Issue close、workspace cleanupは再試行可能な後処理であり、成立済みの
   release結果をfailedへ反転させない。
8. published evidence version 2は`contracts/live-release-receipt.schema.json`と独立semantic certifierを正本にする。
   JSON Schemaは形と局所制約、certifierはidentity、causality、chronology、head epoch、finding lineage、policy充足を
   検査する。`passed`と`passed-with-interventions`はHOW介入receiptの件数だけから決め、人間のWHAT判断を
   interventionへ数えない。
   runtime provenanceはproducer jobごとの複数receiptを許し、`release_heads`がhead epoch、`release_artifacts`が
   final headとreceipt IDsへのartifact bindingを永続化する。completed releaseは
   `npm run evidence:live-release:export -- <release-id>`でoutboxから再構成し、再度certifyする。
9. version 1 evidenceはimmutableなhistorical artifactとして残し、既存schemaとsemantic validatorでのみ検証する。
   v1をv2へbackfill、推測変換、dual-writeしない。Registrationに`releaseEvidence` policyがない既存運転はv1-compatible、
   policyを明示した新規releaseだけがv2 receipt pathへ入る。validatorはtop-level `schemaVersion`で1.0/2.0をdispatchし、
   v2 cutover後の新規live claimにはv2を要求する。v1 artifactの存在はv2 receipt欠落を補完しない。
10. 検証はschema/semantic unit test、PostgreSQL integration test、job分割・順序差・retry/recovery testを
    headlessで先に行う。完了条件には別repositoryの新規Issueを使うexternal-target live runを一件含め、
    手作業のID付け替えなしにoutboxからevidenceを生成して独立validatorへ通す。

## 2026-08-06 canonical wire amendment

version 2/3 receiptとversion 1 external evidenceはimmutable historical wireとして残す一方、新規出力の曖昧語を
解消するため次版を追加した。

1. canonical receiptは`contracts/live-release-receipt-v4.schema.json`のversion 4とする。
   provider callの元identityを`invocationKey`、一方向派生参照を`invocationRef`として併記し、
   `pullRequestNumber`、3値Verdict＋`hasFindings`、`sourceIssueClosure:'completed'`を使う。
2. external-target evidenceは`contracts/live-release-evidence-v2.schema.json`のversion 2で同じ
   `pullRequestNumber`、3値Verdict＋`hasFindings`、Source Issue Closure語彙へ揃える。
3. legacy schemaは削除・上書きしない。validatorは`receipts`と`formalReviews`のどちらを持つかでartifact familyを
   先に判別し、family内のschemaVersionを検証する。同じ`2.0`をversionだけで誤dispatchしない。
4. migration 0026はdurable rowをcanonical fieldへ移すが、未記録の元`invocationKey`や旧`findings`に潰れた
   `needs_human`を復元したとは主張しない。前者は旧opaque refをfallbackとして保持し、後者は
   `request_changes`へ保守的に写像する。
5. requirements authority導入前のcompleted releaseをexportするときだけ、canonical DB rowから旧v2 schemaへ
   明示的に逆変換する。新旧fieldを同じwireへ混在させない。新規releaseはversion 4だけを生成する。

このamendmentはdecision 8/9のhistorical artifact保持を維持し、新規Published Languageの正本だけをversion 4/2へ
進める。旧保証を新保証へ自動昇格しない。

## 帰結

- workflowはjobの増減、順序変更、promotion省略、recovery job追加を行っても、同じrelease/head/causalityへ
  収束する限り公開証拠を壊さない。
- PostgreSQLが使えない、policy receiptが欠ける、expected headが変わる、causeが別releaseを指す場合はmergeできない。
  availabilityより未証明mergeの防止を優先する。
- outboxは証拠の小さいsemantic coreを保持する。log、checkout、screenshot等のartifact lifecycleはDB transactionへ
  入れないが、digestとreceipt linkがないartifactはcertificateへ採用しない。
- version 1の過去の主張は保持されるが、version 2の保証へ自動昇格しない。cutover前後の保証を混同しない。
- external-target live runだけはGitHub App、実provider、実mergeの境界を通るためheadless testで代替できない。

## 実装状況

version 2/3 legacy schema、canonical receipt v4、external evidence v2、semantic certifier、migration v8/26、
atomic triage promotion、release/head/receipt/artifact persistence、
job-local build/review/check/runtime projection、pre-merge certifier、expected-head merge境界、GitHub post-merge観測、
idempotent recovery、PostgreSQL-only exporterをproduction runnerへ配線済みである。runner DB roleはrelease table DMLを
持たず、再検証付きSECURITY DEFINER capabilityだけでreceipt／authorization／merge／artifactを更新する。
別jobのPR recoveryはRegistration／Issue／PR座標から同じreleaseへ再linkする。外部targetでの一件のlive runが
完了するまで、Issue #110とADRの実装状態を完了扱いにしない。

## 実装先 id

- architecture: `ARCH-control-store-017`〜`019`
- domain-model: `DOM-control-store-016`〜`019`
- data-model: `DATA-control-store-019`〜`024`
- ubiquitous-language: `LANG-control-store-026`〜`032`
