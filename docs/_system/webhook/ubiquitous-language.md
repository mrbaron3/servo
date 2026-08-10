# Webhook コンテキスト — ユビキタス言語

| ID | 用語 | 定義 |
| --- | --- | --- |
| LANG-webhook-001 | Delivery Envelope | providerのdelivery id、event、repository、headers、payload、受信時刻を保持する受信契約。 |
| LANG-webhook-002 | Durable Event Inbox | ACK前にDelivery Envelopeを耐久保存し、処理状態と再実行履歴を持つ受信箱。 |
| LANG-webhook-003 | Delivery Identity | `X-GitHub-Delivery`を正規keyとする再送同一性。欠落するlocal testではpayload hashを明示fallbackにする。 |
| LANG-webhook-004 | Repository Registration | `owner/name`とevents、consumer、workspace/store binding、enabled状態を対応付ける管理レコード。 |
| LANG-webhook-005 | Normalized GitHub Event | consumerが受け取るtransport非依存イベント。production実体は`apps/control-plane/internal/control/model.go`の`WorkItem`で、registration-controlの`Convergent Work Item`と同じ値を指す翻訳名である。`Kind`は実体種別`issue\|pull-request\|repository-head`だけを表し、GitHub event名は`SourceEvent`へ分離する。旧`pull_request\|push\|check_run\|check_suite` kindはdurable idempotency keyを変えない受理互換に限定する。TypeScript compatibility oracleは`apps/agentops/src/webhook/schema.ts`の`NormalizedGithubEvent`を使う。raw payloadを制御フローへ漏らさない。 |
| LANG-webhook-006 | Reconciliation Poll | Webhook未配送・順序逆転・停止中イベントをGitHub current snapshotから回収する周期照合。 |
| LANG-webhook-007 | Signed Webhook Ingress | productionのHMAC検証付きHTTP ingress。deliveryをACK前にPostgreSQLへ保存するが、受信がない期間に外部からの到達性を推測してhealthyと投影しない。実装は`apps/control-plane/cmd/agentops-control/`と`apps/control-plane/internal/control/`。 |
| LANG-webhook-008 | Legacy CLI Forwarder | registrationごとに`gh webhook forward`を監督するcompatibility-oracle限定runtime adapter。standard OCI productionは`RuntimeTopologySignedWebhookIngress`で起動せず、forwarder heartbeatも投影しない。 |
