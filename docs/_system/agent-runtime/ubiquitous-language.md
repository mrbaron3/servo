# ユビキタス言語 — agent-runtime コンテキスト

> agent-runtime は、planning / UI-design / generation / review が利用する非決定なAI呼出しを、provider固有CLIから
> 独立した共通言語へ翻訳する。executionのqueue・liveness・fan-out、evaluationの採点意味論は所有しない。
> 追加のみ（`LANG-agent-runtime-NNN` は安定）。

| ID | 用語 | 意味 |
| --- | --- | --- |
| LANG-agent-runtime-001 | Agent Invocation | 1つのroleが1つのprovider/modelへpromptを発行し、1つのoutcomeを得る監査可能な呼出し単位。 |
| LANG-agent-runtime-002 | Invocation Identity | subject・sample・attempt・role・perspectiveから決定論的に作るlogical identity。同じ仕事のresumeを重複recordにしない。 |
| LANG-agent-runtime-003 | Provider | Claude Code / Codex / Gemini / mock等、sessionを実際に遂行するtool family。modelとは別軸。正典型名は`AgentProvider`で、role別aliasを新設しない。旧`GeneratorAgent` aliasは削除済みであり、新しい契約語へ再導入しない。 |
| LANG-agent-runtime-004 | Model | Provider内で選ぶmodel id/alias。未指定はprovider defaultを意味し、unknownを特定modelとして捏造しない。 |
| LANG-agent-runtime-005 | Invocation Role | generator / reviewer / roadmap-planner等、呼出しが負う単一責務。executionのrole-scoped contextと対応する。 |
| LANG-agent-runtime-006 | Perspective Route | reviewer roleをcodeQuality/security等のlensへさらに限定する任意のroute。非reviewerではnull。 |
| LANG-agent-runtime-007 | Provider Adapter | 共通session契約をprovider固有のinteractive CLI起動・tool権限・prompt投入へ翻訳するadapter。 |
| LANG-agent-runtime-008 | Invocation Provenance | role・perspective・provider・model・prompt・outcomeをInvocation Identityへ結び付けた耐久記録。 |
| LANG-agent-runtime-009 | Invocation Key | AI呼出しのlogical identityをevidence間で保持する正典string。`apps/agentops/src/domain/schema.ts`の`AgentInvocation.invocationKey`が正本である。canonical receipt v4はkeyをそのまま`invocationKey`へ載せ、opaqueな派生値を`invocationRef`へ分離する。現行導出は`invocation:`＋`deterministicUuid(releaseId, invocationKey)`で、`deterministicUuid`はUTF-8の`releaseId + NUL + invocationKey`をSHA-256し、先頭128 bitへRFC 4122 version 4 / variant bitを設定する。UUIDv5ではなく、refから元keyへ逆写像できない。旧receipt v2/v3の`invocationId`はhistorical aliasで、key未記録の行をmigration 0026がrefと同値で補う場合も元keyを復元したとは主張しない。 |
