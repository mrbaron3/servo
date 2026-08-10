# Servo — coding-agent development & eval harness

Servo is a **local-first operating harness** that runs existing coding agents (Claude Code /
Codex / Gemini) through a real development organisation's loop: **Roadmap → Issue
Contract → Generator PR → Evaluator Scorecard → Repair → Release → Eval-curation →
Dashboard → harness improvement.**

`agentops` remains the technical prefix for the TypeScript execution subsystem, CLI,
components, and environment variables; it is not a second product name.

The bet (from the design brief): the leverage is **not more agents** — it's turning
"planning / implementation / review / QA / release / retro / eval-improvement" into
**machine-readable contracts and a measurable eval loop**, with state that lives in a
durable store you can resume, analyse and improve from.

> This is an **MVP**: the entire pipeline runs end-to-end offline on a deterministic
> **mock** backend. The grounded path adds GitHub Issue intake, provider-routed planning,
> implementation, PR-first isolated review, current-head repair, and expected-SHA automatic
> merge. Its seams are permanently tested;
> a real remote/provider run is still required before claiming environment-level validation.

---

## 日本語概要

これは「コーディングエージェントを束ねて、ロードマップ→Issue Contract→PR→評価→修正→
リリース→評価ハーネス改善まで回す **ローカルファーストな開発運用ハーネス**」の MVP です。
設計ドキュメントの section 18 が言う「まず通すべき 1 本」

```text
Issue Contract → Generator PR → Evaluator Scorecard → Repair → PASS → Eval Result DB
```

を実際に動く形にし、その周りに Roadmap / 複数サンプル(pass@k / pass^k)/ ダッシュボード /
Eval Curator / Harness Analyst を薄く一通り載せてあります。

エージェント実行は既定で **mock**(オフライン・決定論的)なので、すぐ動かせます。

```bash
npm install
npm run demo        # 全工程を一気に実行（init→plan→run→curate→analyze→dashboard）
npm run dashboard   # .harness/dashboard.html をブラウザで開く
```

実運用向けには、GitHub の `ready` Issue を取り込み、独立した planning session で契約化し、
Claude Code / Codex を役割・レビュー観点ごとに振り分けて、PR作成後のcurrent-head評価・修正・自動mergeへ
接続する経路もあります。設定例は [GitHub Issue watcher](#github-issue-watcher) を参照してください。

## Application boundaries / アプリケーション境界

この repository には、別々のアプリケーションが2つあります。

- `apps/control-plane/` — Go の control-plane。Control API、Registration monitor、
  GitHub credential broker、lifecycle CLI（`agentopsctl`）を所有する。
- `apps/agentops/` — TypeScript の AgentOps。計画・評価ハーネス、triage、isolated runner、
  runtime adapter を所有する。
- `db/` と `contracts/` — 両アプリケーションが順応する language-neutral な共有境界。
  どちらか一方のアプリケーション配下へ置かない。
- `deploy/` — 2つのアプリケーションと PostgreSQL を OCI topology に組み立てる統合層。
  アプリケーションのソース所有境界ではない。

Go と TypeScript の間で durable な業務調整を行う唯一の正本は PostgreSQL
`agentops_control` です。Registration、lifecycle mode/drain fence、job、lease/attempt、結果、進捗、
receipt、artifact metadata は
`db/control-store/migrations/` と `contracts/` の versioned contract を介して共有します。
`LISTEN/NOTIFY` は wake-up hint にすぎず、再取得は PostgreSQL query で行います。

ただし「すべての接続が DB」という意味ではありません。短期 credential は専用 broker の HTTP と
credential helper、外向き通信は control-plane の CONNECT egress proxy、checkout と大きな artifact は
runner の shared volume、container の build/start/stop と actual topology 操作は `agentopsctl` という、
それぞれ別の
security/runtime 境界を通ります。shared volume の durable な URI・digest・因果 metadata だけが
PostgreSQL に残ります。lifecycleのmode/generation/drain fenceはPostgreSQLに残しますが、container runtime操作を
DB jobにはしません。

物理ディレクトリは分離しましたが、当面の release unit は repository 全体で一体です。両 consumer が
schema version/checksum を exact verify し、`agentopsctl` が同一 revision の image、migration、topology を
まとめて収束させるためです。独立 versioning/deploy を意味する分割ではありません。詳細は
[ADR-0021](docs/decisions/ADR-0021-go-typescript-application-boundaries.md)を参照してください。

---

## Quick start

```bash
npm install
npm run demo            # runs the whole loop on the sample roadmap
npm run dashboard       # opens .harness/dashboard.html
```

Or drive it step by step (`agentops` == `npm run harness --`):

```bash
npm run harness -- init
npm run harness -- plan-roadmap          # ingest apps/agentops/seed/sample-plan.yaml into the planning tree
npm run harness -- spawn-specs           # materialize one authorable spec stub per feature
npm run harness -- plan-tree             # print roadmap → epic → feature → spec
npm run harness -- plan                  # LEGACY: ingest apps/agentops/seed/sample-roadmap.yaml (drives the demo)
npm run harness -- run                   # Generate → Evaluate → Repair → Release
npm run harness -- status                # pass@k / pass^k / cost
npm run harness -- curate                # promote blocker criteria into the eval registry
npm run harness -- analyze --create      # file harness/eval improvement issues
npm run harness -- dashboard --open
```

## The loop

```text
 Roadmap ─▶ Epic ─▶ Issue Contract ─▶ Coordinator
                                          │
                          ┌───────────────┴───────────────┐
                          ▼            (per issue: N samples)
                     Generator ──▶ PR ──▶ Evaluator ──▶ Scorecard
                          ▲                                │
                          │   request_changes              │ approve
                     Repair Router ◀──────────────────────┤
                                                           ▼
                                                    Release Manager ─▶ released
                                                           │
   Eval Curator ◀── failures ── Eval Result DB ◀───────────┘
        │                            │
        ▼                            ▼
  Eval Task Registry            Dashboard ─▶ Harness Analyst ─▶ type:harness / type:eval issues
```

Each issue is run as **N independent best-of-N samples**; each sample has its own
bounded **repair loop**. Running every sample to completion is what makes **pass@k**
(any sample passes — exploration) and **pass^k** (all samples pass — consistency)
both measurable for the same issue.

## What you get from a run

- `.harness/db.json` — the TypeScript evaluation domain's local **Eval Result DB** / source of
  truth (issues, PRs, eval runs). It is not the Go↔TypeScript control-plane bridge and is not a
  duplicate of PostgreSQL `agentops_control`.
- `.harness/evidence/<run>/` — per-eval evidence: `scorecard.yaml`, `trace.txt`,
  `artifact.json`, and a placeholder `screenshot.svg` for UI checks. **A verdict never
  ships without evidence.**
- `.harness/dashboard.html` — roadmap/epic progress, pass@k vs pass^k curve, the
  **area × failure-type heatmap**, per-agent stats, and recent scorecards with links.

## Repository layout

```text
apps/
  control-plane/   Go application
    cmd/            agentops-control · agentopsctl · github-broker · credential-helper
    internal/       lifecycle · control · githubapp · designgate
  agentops/        TypeScript application
    agents/         role prompts
    seed/           sample roadmap/plan
    src/            harness · triage · runner · runtime adapter · PostgreSQL client
    scripts/        migration, evidence and self-host tooling
    test/           application-local Vitest suite
contracts/         shared JSON Schema + OpenAPI published language
db/control-store/  shared PostgreSQL migrations
deploy/            OCI build/topology integration layer for both applications
  scripts/          cross-application runtime smoke drivers
  test/             topology contract + Control dashboard integration tests
  tools/            image-composition tools: provider-cli · gh · gosu
evidence/          durable run / release evidence
docs/              context-map.md · _system/<ctx>/ · decisions/ (ADR) · specs/ · runbooks/
package.json       root npm workspace/developer command router (not application source)
go.work            root Go workspace/developer command router (not application source)
```

## Concepts (mapped to the brief)

| Concept | Where | Notes |
| --- | --- | --- |
| Planning tree | `apps/agentops/src/planning/planning-tree.ts`, `Feature` schema | roadmap→epic→feature→spec; planner emits outcomes, AC are never inlined (`docs/specs/planning-tree/`) |
| Issue Contract | `IssueContract` in `apps/agentops/src/domain/schema.ts` | a contract is "ready" only when it parses |
| State machine | `apps/agentops/src/domain/states.ts` | `ISSUE_STATUSES` + `TRANSITIONS`; `status:*` labels mirror it |
| Generator / Evaluator / Repair | `apps/agentops/agents/*.md`, `apps/agentops/src/pipeline/*` | Evaluator is independent; verdict from evidence |
| Hard gates then score | `apps/agentops/src/graders/index.ts` | any blocker fails ⇒ `request_changes`, regardless of score |
| Scorecard + evidence | `apps/agentops/src/pipeline/evaluate.ts`, `.harness/evidence/` | `renderScorecard()` writes `scorecard.yaml` from the `EvalRun` contract |
| pass@k vs pass^k | `apps/agentops/src/metrics/metrics.ts` | unbiased estimators; both reported |
| Eval Task Registry | `apps/agentops/src/pipeline/curator.ts` | grows regressions from real failures |
| Harness self-improvement | `apps/agentops/src/pipeline/analyst.ts` | files `type:harness` / `type:eval` issues |

See [docs/context-map.md](docs/context-map.md) for the bounded contexts and where each
term lives (terminology is per-context under `docs/_system/<ctx>/ubiquitous-language.md`).

## Two backends: mock (deterministic) and real (live tmux)

The **mock** backend runs the whole loop offline and deterministically — it is what the test
suite and `agentops run` demo use (`"generator": "mock"`). It is the reproducible substrate:
drive → panel → repair → gate all run without a real agent.

**Real agents run as interactive tmux sessions on an actual git worktree**, grounded by real
`tsc` / `vitest` against that checkout — not a headless `claude -p` shell-out (the old
`CliAgentRunner` was deprecated with the tmux orchestration; headless is a North-Star non-goal,
ADR-0005 Q2). A generator session edits files and commits the build, the GitHub PR is opened, then
read-only perspective sessions review that exact head SHA. A blocker/major finding repairs and
pushes the same branch; a clean current head is merged with an expected-SHA guard. The `store`
gate remains available for local/manual runs.

```bash
npx tsx apps/agentops/scripts/real-run-sandbox.ts                  # scaffold a throwaway sandbox + ai-managed issue + config
LENSES=codeQuality npx tsx apps/agentops/scripts/real-panel-run.ts # cheap: generator + one review lens
npx tsx apps/agentops/scripts/real-panel-run.ts                    # full 6-perspective panel
```

`config.gate.backend` (`store` | `github`) chooses local/manual vs PR-native delivery.
`gate.requiredChecks` names GitHub checks that must be green (empty means all visible checks), and
`mergeMethod` is `squash`, `merge`, or `rebase`. For a Registration-driven Go control-plane job,
`configuration.mergeMethod` is the durable authority and is copied into the versioned runner payload;
omission defaults to `squash`. Workspace `gate.mergeMethod` is only the fallback for the legacy
TypeScript `github-turn` path when no Registration exists, and never overrides Registration.
`config.panel.maxConcurrent` bounds the review fan-out. The execution layer's design lives in
[ADR-0005](docs/decisions/ADR-0005-execution-layer-tmux-orchestration.md) /
[ADR-0006](docs/decisions/ADR-0006-evaluator-panel-sessions-and-github-pr-gate.md) and the
[`_system/execution`](docs/_system/execution/) views.

## GitHub Issue watcher

The live entry path is:

```text
ready GitHub Issue → durable claim → isolated planning → conditional isolated UI design → trace gate → implementation
→ GitHub PR → current-head isolated perspective reviews → same-branch repair → expected-SHA auto-merge
```

Initialize the harness, then configure `.harness/config.json`. This example routes planning
and general review to Claude, UI design, generation, and the security lens to Codex; every omitted model
inherits the corresponding CLI default.

```json
{
  "generator": "codex",
  "baseBranch": "main",
  "samples": 1,
  "maxConcurrentIssues": 1,
  "maxRepairs": 2,
  "passThreshold": 0.7,
  "scoreWeights": {
    "functionality": 0.4,
    "codeQuality": 0.2,
    "testQuality": 0.15,
    "ux": 0.15,
    "accessibility": 0.1
  },
  "target": {
    "repo": "../target-app",
    "baseRef": "main",
    "systemDir": "../target-app/docs/_system",
    "graders": {
      "typecheck": "npm run typecheck",
      "unit_tests": "npm test",
      "commands": {
        "playwright": "npm run test:e2e",
        "api_test": "npm run test:api"
      }
    }
  },
  "gate": {
    "backend": "github",
    "baseBranch": "main",
    "requiredChecks": ["test"],
    "mergeMethod": "squash"
  },
  "routes": {
    "planning": { "provider": "claude" },
    "uiDesign": { "provider": "codex" },
    "generator": { "provider": "codex" },
    "reviewer": { "provider": "claude" },
    "perspectives": {
      "security": { "provider": "codex" }
    }
  },
  "intake": {
    "backend": "github",
    "repository": "owner/target-app",
    "readyLabel": "ready",
    "claimedLabel": "agent-claimed",
    "pollIntervalMs": 30000
  },
  "panel": { "maxConcurrent": 4 }
}
```

The target must be a git repository, `gh` must already be authenticated, the configured labels
must exist, and each routed provider CLI must be available. Interactive adapters currently exist
for Claude Code and Codex; unsupported providers fail closed. Run one turn or keep watching:

```bash
npm run harness -- poll-intake    # optional: inspect/claim ready input only
npm run harness -- github-turn    # one intake/planning/development turn
npm run harness -- watch-github   # repeat turns; Ctrl-C stops the watcher
```

`intake.pollIntervalMs` is optional and controls the delay between recurring `watch-github`
turns in milliseconds. It does not affect the one-shot `github-turn` command. Valid values are
positive integers up to 2147483647 ms (Node's maximum timer delay); omitted or invalid
values fall back to `DEFAULT_GITHUB_WATCH_INTERVAL_MS` (30000 ms).

### Multi-repository monitor, triage, and execution

Webhook remains an immediate trigger and polling remains the truth-recovery path.
`agentops-control` is the PostgreSQL Registration-driven production process: it exposes the
operator Control API, supervises per-Registration Issue/PR monitors and signed HTTP webhook
ingress, persists webhook deliveries before acknowledgement, and routes webhook/poll observations
through the same idempotent queue. The standard OCI control process never receives a GitHub
credential or executes `gh`.

```bash
export AGENTOPS_POSTGRES_PASSWORD='<32+ bytes>'
export AGENTOPS_CONTROL_DB_PASSWORD='<different 32+ bytes>'
export AGENTOPS_TRIAGE_DB_PASSWORD='<different 32+ bytes>'
export AGENTOPS_RUNNER_DB_PASSWORD='<different 32+ bytes>'
export AGENTOPS_CONTROL_TOKEN='<32+ byte random operator bearer token>'
export AGENTOPS_DASHBOARD_BOOTSTRAP_TOKEN='<random single-use bootstrap token>'
export AGENTOPS_GITHUB_WEBHOOK_SECRET='<webhook secret>'
export AGENTOPS_GITHUB_APP_ID='<numeric App id>'
export AGENTOPS_GITHUB_APP_INSTALLATION_ID='<numeric installation id>'
export AGENTOPS_GITHUB_APP_SLUG='<canonical app slug>'
export AGENTOPS_GITHUB_APP_OWNER='mrbaron3'
export AGENTOPS_GITHUB_APP_PRIVATE_KEY_FILE='<absolute mode-0600 .pem path>'
# Broker capabilities need no operator input. On first use agentopsctl generates one per
# role into a private mode-0600 ~/.agentops/<prefix>/broker-capabilities.json and reads the
# same values back on every later command. Holding a capability is the right to mint that
# role's installation token, so each stays its own secret: never derived from, or equal to,
# any other credential above. Set both below only to keep an external secret manager
# authoritative — 43..128 URL-safe characters, and then the store is never written:
# export AGENTOPS_GITHUB_BROKER_TRIAGE_CAPABILITY="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"
# export AGENTOPS_GITHUB_BROKER_RUNNER_CAPABILITY="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"

# Observation is on; AI classification and development execution remain off.
go run ./apps/control-plane/cmd/agentopsctl \
  start --mode MONITOR_ONLY --build --request-id monitor-bootstrap-001
```

The operator API defaults to `127.0.0.1:8080`; the container target keeps it on
`127.0.0.1:8081` behind an in-process port-8080 publication proxy, and publishes that proxy to host
loopback only. PostgreSQL and runner ports stay internal.
Run `agentopsctl open` (or `mise run open`) to open the latest one-time bootstrap URL from the
control log. An existing valid browser session is reused without consuming that token; a new
session consumes it and immediately rotates the next URL. Browser code receives no bearer
credential and uses only an HttpOnly same-origin session cookie plus a memory-only CSRF proof. Its
contract is
[`contracts/control-api/v1/openapi.yaml`](contracts/control-api/v1/openapi.yaml). Create requires
`Idempotency-Key`; update/disable require both it and `If-Match: "<registration version>"`; retry
also fences the Registration identity/version and observed route attempts. Non-browser automation
may use `Authorization: Bearer …`, while browser mutation requires the exact configured
Origin/fetch metadata/CSRF tuple. Public webhook ingress is disabled unless the HMAC secret is configured.

Polling defaults to one minute and shares the same per-repository single-flight queue as webhook
deliveries. Configure `AGENTOPS_GITHUB_POLL_INTERVAL` and
`AGENTOPS_RECONCILIATION_INTERVAL` with positive Go durations. `LISTEN/NOTIFY` only accelerates
wake-up: periodic PostgreSQL reconciliation remains the recovery path after missed notifications,
control restart, forwarder exit, or DB reconnect. Unknown, disabled, stale, or disconnected
Registrations never create jobs.
PostgreSQL Registration is the single durable repository authority used at every broker claim;
there is no process environment repository allowlist. `MONITOR_ONLY` runs only the credential-limited triage
container's typed Issue/PR broker; it does not invoke an AI provider, add labels/comments, or start
the development runner. It updates monitor freshness but deliberately does not advance the
processing cursor, so existing work observed before cutover is re-read once ACTIVE. `ACTIVE` lets
that container classify Issue work and apply only its
configured triage labels/comment. An exact human-owned `ready` label is then checked again before a
database capability atomically creates a development job.

The triage container and development runner are different images, processes, database roles, and
GitHub broker capabilities. Triage has no repository workspace, git/SSH tools, runtime socket,
host path, or host port. The development runner is absent in `MONITOR_ONLY`; in `ACTIVE` it receives
its own capability, workspace, and provider credential. A dedicated internal-only broker is the
only process that can read the GitHub App private key; it returns repository/permission-scoped
installation tokens in memory to the `gh`/Git helper. Static PAT variables are rejected and the
control process receives neither a GitHub token nor a broker capability.
See the [Servo autonomous-pipeline runbook](docs/runbooks/servo-autonomous-pipeline.md) for registration,
staged deployment, rollback, and operator verification. The credential threat model and exact
role matrix are in [ADR-0019](docs/decisions/ADR-0019-github-app-credential-broker.md).

The old TypeScript GUI/router types remain as a non-durable PR #9 compatibility oracle. The legacy
`webhook-daemon` production command remains fail closed; no production entry point reads or writes
a JSON control store, and evaluation-domain `.harness/db.json` is unchanged.

Claiming removes `ready` and adds `agent-claimed`. The first source snapshot and every planning /
UI-design / generation / perspective invocation retain stable provenance in `.harness/db.json`; restarts reuse
those records instead of re-planning the same source. For a pre-existing unbound legacy store, run
`npm run harness -- bind-target` once after setting the intended target.

`frontend`/`fullstack` planning candidates are each sent to a fresh, read-only `ui-designer`
session before Issue creation. Its principles, tokens, components, states/interactions,
accessibility rules, and AC traces must pass schema and invocation-provenance validation; missing,
ambiguous, or invalid output stops the whole enrichment at `needs-human-review`. Backend-only
candidates do not select or invoke the UI route. When `routes.uiDesign` is omitted, it inherits the
planning route (then the legacy generator route) while keeping a separate session/context.

`target.graders.commands` is keyed by the acceptance criterion's `verification.method`. Non-unit
methods run once per criterion with `AGENTOPS_AC_ID`, `AGENTOPS_ISSUE_ID`,
`AGENTOPS_VERIFICATION_METHOD`, and `AGENTOPS_EXPECTED_JSON`; a missing command fails closed.

## What is real vs mocked (so there are no surprises)

- **Always real:** contracts and schema validation, state transitions, the durable store,
  trace gates, scoring, evidence records, metrics, curation, dashboard, and CLI behavior.
- **Offline demo:** `npm run demo` uses deterministic agent simulation; its described artifacts,
  cost, and token values are synthetic.
- **Grounded live path:** the watcher uses real git worktrees, provider CLIs, grader commands,
  isolated reviewer checkouts, `gh`, and the existing PR gate. Repository tests fake external
  systems at their boundaries; they are not reported as proof of a real remote run.

## Develop

```bash
npm run typecheck
npm run test
npm run test:dashboard # requires AGENTOPS_TEST_DATABASE_URL
npm run go:test
```
