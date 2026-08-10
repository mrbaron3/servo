/**
 * Self-contained HTML dashboard + a terminal status report.
 *
 * The dashboard answers the spec's questions: what are we building (roadmap/epics),
 * is the *harness* trustworthy (pass@k/pass^k, false pass/fail), and where do things
 * fail (the area x failure-type heatmap). It's a single static file written to
 * .harness/dashboard.html — no server, just open it.
 */

import fs from 'node:fs';
import path from 'node:path';
import type { Store } from '../store/store.js';
import {
  computeMetrics,
  MIN_HUMAN_DECISIONS_FOR_CALIBRATION,
  type Metrics,
} from '../metrics/metrics.js';

export function writeDashboard(store: Store): { path: string; metrics: Metrics } {
  const metrics = computeMetrics(store);
  const html = renderHtml(store, metrics);
  const out = path.join(store.dir, 'dashboard.html');
  fs.mkdirSync(store.dir, { recursive: true });
  fs.writeFileSync(out, html, 'utf8');
  return { path: out, metrics };
}

const pct = (x: number) => `${(x * 100).toFixed(0)}%`;
const pct1 = (x: number) => `${(x * 100).toFixed(1)}%`;
const esc = (s: string) =>
  s.replace(/[<>&"]/g, (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;', '"': '&quot;' })[c] ?? c);

function verdictClass(v: string): string {
  return v === 'approve' ? 'ok' : v === 'needs_human' ? 'warn' : 'bad';
}

function renderHtml(store: Store, m: Metrics): string {
  const r = store.db.roadmap;
  const calibrationSufficient =
    m.humanDecisionCount >= MIN_HUMAN_DECISIONS_FOR_CALIBRATION;
  const evHref = (rel: string | null, file: string): string | null => {
    if (!rel) return null;
    const abs = path.join(store.root, rel);
    return path.relative(store.dir, abs).split(path.sep).join('/') + '/' + file;
  };

  const cards = [
    card('pass@1', pct(m.passAt1), 'first attempt, no repair'),
    card(`pass@${m.headlineK}`, pct(m.passAtK), 'any sample eventually passes (exploration)'),
    card(`pass^${m.headlineK}`, pct(m.passHatK), 'all samples pass (consistency)'),
    card('repair success', pct(m.repairSuccessRate), 'failed-first that recovered'),
    card('avg attempts', m.avgRepairAttempts.toFixed(2), 'generations per sample'),
    card('instability', pct(m.instabilityRate), 'issues whose samples disagree'),
    card('released', `${m.totals.released}/${m.totals.issues}`, 'issues shipped'),
    card('cost', `$${m.cost.usd.toFixed(2)}`, `${(m.cost.tokens / 1000).toFixed(0)}k tok · ${m.cost.seconds}s`),
    card(
      'false pass / fail',
      !calibrationSufficient || m.falsePassRate === null
        ? 'n/a'
        : `${pct(m.falsePassRate)} / ${pct(m.falseFailRate ?? 0)}`,
      !calibrationSufficient
        ? `較正不足: n=${m.humanDecisionCount}`
        : `vs human review · n=${m.humanDecisionCount} decisions${
          m.falsePassTrend.length >= 2
            ? ` · trend ${m.falsePassTrend.slice(-4).map((p) => pct(p.rate)).join('→')}`
            : ''
        }`,
    ),
    card(
      'regression capture',
      m.regressionCaptureRate === null ? 'n/a' : pct(m.regressionCaptureRate),
      m.regressionCaptureRate === null ? 'no blocker AC failures observed' : 'failed ACs promoted to eval tasks (③)',
    ),
    card(
      'regression executed',
      m.regressionExecutedRate === null ? 'n/a' : pct(m.regressionExecutedRate),
      m.regressionExecutedRate === null
        ? 'registry is empty'
        : m.regressionFailingTasks > 0
          ? `${m.regressionFailingTasks} captured failure(s) are BACK`
          : m.regressionUnverifiedTasks > 0
            ? `${m.regressionUnverifiedTasks} unverifiable task(s)`
            : 'captured failures re-verified, holding',
    ),
  ].join('\n');

  const epicRows = store.db.epics
    .map((e) => {
      const issues = e.issueIds.map((id) => store.getIssue(id)).filter(Boolean);
      const released = issues.filter((i) => i && i.status === 'released').length;
      const frac = issues.length ? released / issues.length : 0;
      return `<tr><td>${esc(e.id)}</td><td>${esc(e.title)}</td><td>${esc(e.theme)}</td>
        <td><span class="chip ${e.status === 'done' ? 'ok' : e.status === 'in-progress' ? 'warn' : ''}">${e.status}</span></td>
        <td>${bar(frac)} <span class="muted">${released}/${issues.length}</span></td></tr>`;
    })
    .join('\n');

  const issueRows = m.issues
    .map(
      (i) => `<tr>
      <td>${esc(i.issueId)}</td><td>${esc(i.title)}</td>
      <td><span class="tag">${i.type}</span></td><td>${i.area}</td>
      <td><span class="chip ${i.status === 'released' ? 'ok' : i.status === 'needs-human-review' ? 'bad' : 'warn'}">${i.status}</span></td>
      <td>${i.passing}/${i.n}</td><td>${i.maxAttempts}</td></tr>`,
    )
    .join('\n');

  const agentRows = m.byAgent
    .map(
      (a) => `<tr><td>${a.agent}</td><td>${a.samples}</td><td>${pct(a.passAt1)}</td>
      <td>${pct(a.passEventual)}</td><td>${a.avgAttempts.toFixed(2)}</td><td>$${a.costUsd.toFixed(2)}</td></tr>`,
    )
    .join('\n');

  const invocationProviderRows = m.byInvocationProvider
    .map(
      (provider) => `<tr><td>${provider.provider}</td><td>${provider.total}</td><td>${provider.completed}</td>
      <td>${provider.stuck}</td><td>${provider.timeout}</td><td>${provider.failed}</td></tr>`,
    )
    .join('\n');

  const recent = [...store.db.evalRuns]
    .slice(-12)
    .reverse()
    .map((run) => {
      const card = evHref(run.evidenceDir, 'scorecard.yaml');
      const trace = evHref(run.evidenceDir, 'trace.txt');
      const links = [
        card ? `<a href="${card}">scorecard</a>` : '',
        trace ? `<a href="${trace}">trace</a>` : '',
      ]
        .filter(Boolean)
        .join(' · ');
      return `<tr><td>${esc(run.id)}</td><td>${esc(run.issueId)}</td><td>s${run.sampleIndex}/a${run.attempt}</td>
        <td><span class="chip ${verdictClass(run.verdict)}">${run.verdict}</span></td>
        <td>${run.overall.toFixed(2)}</td><td>${run.findings.length}</td><td>${links}</td></tr>`;
    })
    .join('\n');

  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Servo Dashboard</title>
<style>
  :root{
    --bg:#11111b; --panel:#181825; --panel2:#1e1e2e; --text:#cdd6f4; --muted:#9399b2;
    --line:#313244; --ok:#a6e3a1; --warn:#f9e2af; --bad:#f38ba8; --accent:#89b4fa;
  }
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 ui-sans-serif,system-ui,Segoe UI,Roboto,sans-serif;padding:24px}
  h1{font-size:20px;margin:0} h2{font-size:15px;margin:28px 0 10px;color:var(--accent)}
  .sub{color:var(--muted);font-size:12px;margin-top:4px}
  .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:12px;margin-top:16px}
  .card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}
  .card .k{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.04em}
  .card .v{font-size:26px;font-weight:600;margin:4px 0}
  .card .d{color:var(--muted);font-size:11px}
  table{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line);border-radius:10px;overflow:hidden}
  th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--line);font-size:13px}
  th{color:var(--muted);font-weight:600;font-size:11px;text-transform:uppercase;letter-spacing:.04em}
  tr:last-child td{border-bottom:none}
  .chip{display:inline-block;padding:1px 8px;border-radius:999px;font-size:11px;background:var(--panel2);border:1px solid var(--line)}
  .chip.ok{color:var(--ok);border-color:var(--ok)} .chip.warn{color:var(--warn);border-color:var(--warn)} .chip.bad{color:var(--bad);border-color:var(--bad)}
  .tag{font-size:11px;color:var(--muted)}
  .muted{color:var(--muted);font-size:12px}
  .barwrap{display:inline-block;width:120px;height:8px;background:var(--panel2);border-radius:999px;vertical-align:middle;overflow:hidden}
  .barfill{height:100%;background:var(--accent)}
  a{color:var(--accent);text-decoration:none} a:hover{text-decoration:underline}
  .two{display:grid;grid-template-columns:1fr 1fr;gap:24px}
  @media(max-width:860px){.two{grid-template-columns:1fr}}
  .hm td,.hm th{text-align:center}
  .legend{color:var(--muted);font-size:11px;margin-top:6px}
</style></head>
<body>
  <h1>Servo — Development & Eval Harness</h1>
  <div class="sub">${r ? esc(r.vision) : 'No roadmap loaded.'} &nbsp;·&nbsp; ${m.totals.evalRuns} eval runs · ${m.totals.samples} samples · ${m.totals.issuesRun}/${m.totals.issues} issues run</div>

  <div class="grid">${cards}</div>

  <div class="two">
    <div>
      <h2>pass@k vs pass^k</h2>
      ${passCurveSvg(m)}
      <div class="legend">pass@k (blue) rises with k — exploration. pass^k (peach) falls with k — consistency.</div>
    </div>
    <div>
      <h2>Roadmap / Epics</h2>
      <table><thead><tr><th>Epic</th><th>Title</th><th>Theme</th><th>Status</th><th>Progress</th></tr></thead>
      <tbody>${epicRows || emptyRow(5)}</tbody></table>
    </div>
  </div>

  <h2>Failure heatmap — area × failure type</h2>
  ${heatmapTable(m)}

  <h2>Issues</h2>
  <table><thead><tr><th>ID</th><th>Title</th><th>Type</th><th>Area</th><th>Status</th><th>pass/N</th><th>max attempts</th></tr></thead>
  <tbody>${issueRows || emptyRow(7)}</tbody></table>

  <div class="two">
    <div>
      <h2>By agent</h2>
      <table><thead><tr><th>Agent</th><th>Samples</th><th>pass@1</th><th>pass (eventual)</th><th>avg attempts</th><th>cost</th></tr></thead>
      <tbody>${agentRows || emptyRow(6)}</tbody></table>
      <h2>Invocations by actual provider</h2>
      <table><thead><tr><th>Provider</th><th>Total</th><th>Completed</th><th>Stuck</th><th>Timeout</th><th>Failed</th></tr></thead>
      <tbody>${invocationProviderRows || emptyRow(6)}</tbody></table>
    </div>
    <div>
      <h2>Recent eval runs</h2>
      <table><thead><tr><th>Run</th><th>Issue</th><th>s/a</th><th>Verdict</th><th>Overall</th><th>Findings</th><th>Evidence</th></tr></thead>
      <tbody>${recent || emptyRow(7)}</tbody></table>
    </div>
  </div>
  <div class="sub" style="margin-top:28px">db: ${esc(store.dbPath)}</div>
</body></html>`;
}

function card(k: string, v: string, d: string): string {
  return `<div class="card"><div class="k">${k}</div><div class="v">${v}</div><div class="d">${d}</div></div>`;
}
function bar(frac: number): string {
  return `<span class="barwrap"><span class="barfill" style="width:${Math.round(frac * 100)}%"></span></span>`;
}
function emptyRow(cols: number): string {
  return `<tr><td colspan="${cols}" class="muted">— nothing yet —</td></tr>`;
}

function heatmapTable(m: Metrics): string {
  const h = m.heatmap;
  if (h.areas.length === 0) return `<table class="hm"><tbody>${emptyRow(1)}</tbody></table>`;
  const head = `<tr><th></th>${h.types.map((t) => `<th>${esc(t)}</th>`).join('')}</tr>`;
  const rows = h.areas
    .map((area) => {
      const cells = h.types
        .map((t) => {
          const n = m.heatmap.counts[area]?.[t] ?? 0;
          const intensity = h.max ? n / h.max : 0;
          const bg = n ? `background:rgba(243,139,168,${0.15 + intensity * 0.6})` : '';
          return `<td style="${bg}">${n || ''}</td>`;
        })
        .join('');
      return `<tr><th>${esc(area)}</th>${cells}</tr>`;
    })
    .join('');
  return `<table class="hm"><thead>${head}</thead><tbody>${rows}</tbody></table>`;
}

function passCurveSvg(m: Metrics): string {
  const W = 360;
  const H = 180;
  const pad = 28;
  const pts = m.passCurve;
  if (pts.length === 0) return `<div class="muted">no data</div>`;
  const maxK = Math.max(...pts.map((p) => p.k), 1);
  const x = (k: number) => pad + ((k - 1) / Math.max(maxK - 1, 1)) * (W - pad * 2);
  const y = (v: number) => H - pad - v * (H - pad * 2);
  const line = (key: 'passAtK' | 'passHatK', color: string) =>
    `<polyline fill="none" stroke="${color}" stroke-width="2" points="${pts
      .map((p) => `${x(p.k).toFixed(1)},${y(p[key]).toFixed(1)}`)
      .join(' ')}"/>` +
    pts.map((p) => `<circle cx="${x(p.k).toFixed(1)}" cy="${y(p[key]).toFixed(1)}" r="3" fill="${color}"/>`).join('');
  const grid = [0, 0.25, 0.5, 0.75, 1]
    .map(
      (v) =>
        `<line x1="${pad}" y1="${y(v)}" x2="${W - pad}" y2="${y(v)}" stroke="#313244" stroke-width="1"/>` +
        `<text x="4" y="${y(v) + 3}" fill="#9399b2" font-size="9">${v * 100}</text>`,
    )
    .join('');
  const xlabels = pts
    .map((p) => `<text x="${x(p.k)}" y="${H - 8}" fill="#9399b2" font-size="9" text-anchor="middle">k=${p.k}</text>`)
    .join('');
  return `<svg width="100%" viewBox="0 0 ${W} ${H}" preserveAspectRatio="xMidYMid meet" style="background:var(--panel);border:1px solid var(--line);border-radius:10px">
    ${grid}${xlabels}${line('passAtK', '#89b4fa')}${line('passHatK', '#f38ba8')}</svg>`;
}

// --- terminal status report -------------------------------------------------

export function statusReport(store: Store, m: Metrics): string {
  const L: string[] = [];
  const bar20 = (x: number) => {
    const n = Math.round(x * 20);
    return '█'.repeat(n) + '░'.repeat(20 - n);
  };
  L.push(`AgentOps status — ${m.totals.issuesRun}/${m.totals.issues} issues run, ${m.totals.released} released`);
  L.push('');
  L.push(`  pass@1 (first try)   ${bar20(m.passAt1)} ${pct1(m.passAt1)}`);
  L.push(`  pass@${m.headlineK} (exploration) ${bar20(m.passAtK)} ${pct1(m.passAtK)}`);
  L.push(`  pass^${m.headlineK} (consistency) ${bar20(m.passHatK)} ${pct1(m.passHatK)}`);
  L.push(`  repair success       ${bar20(m.repairSuccessRate)} ${pct1(m.repairSuccessRate)}`);
  L.push(`  PR pass rate         ${bar20(m.prPassRate)} ${pct1(m.prPassRate)}`);
  L.push('');
  L.push(`  avg attempts/sample: ${m.avgRepairAttempts.toFixed(2)}   instability: ${pct1(m.instabilityRate)}`);
  L.push(`  cost: $${m.cost.usd.toFixed(2)} · ${(m.cost.tokens / 1000).toFixed(0)}k tokens · ${m.cost.seconds}s`);
  const calibrationSufficient =
    m.humanDecisionCount >= MIN_HUMAN_DECISIONS_FOR_CALIBRATION;
  if (calibrationSufficient && m.falsePassRate !== null) {
    const denominator = `(n=${m.humanDecisionCount} 判断)`;
    L.push(`  false-pass: ${pct1(m.falsePassRate)} ${denominator}`);
    L.push(`  false-fail: ${pct1(m.falseFailRate ?? 0)} ${denominator}`);
    L.push(`  grader agreement: ${pct1(m.graderAgreement ?? 0)} ${denominator}`);
  } else {
    const unavailable = `n/a（較正不足: n=${m.humanDecisionCount}）`;
    L.push(`  false-pass: ${unavailable}`);
    L.push(`  false-fail: ${unavailable}`);
    L.push(`  grader agreement: ${unavailable}`);
  }
  if (calibrationSufficient && m.falsePassTrend.length >= 2) {
    L.push(`  false-pass trend:    ${m.falsePassTrend.slice(-6).map((p) => pct1(p.rate)).join(' → ')}`);
  }
  L.push(
    m.regressionCaptureRate === null
      ? `  regression capture:  n/a (no blocker AC failures observed)`
      : `  regression capture:  ${bar20(m.regressionCaptureRate)} ${pct1(m.regressionCaptureRate)} (③ failed ACs → eval tasks)`,
  );
  if (m.regressionExecutedRate !== null) {
    const health =
      m.regressionFailingTasks > 0
        ? `${m.regressionFailingTasks} FAILING`
        : m.regressionUnverifiedTasks > 0
          ? `${m.regressionUnverifiedTasks} unverified`
          : 'holding';
    L.push(`  regression executed: ${bar20(m.regressionExecutedRate)} ${pct1(m.regressionExecutedRate)} (③ registry re-verified · ${health})`);
  }
  L.push(
    m.howNonInterventionRate === null || m.interventionsPerIssue === null
      ? `  autonomy (HOW):      n/a (no issue driven yet)`
      : `  autonomy (HOW):      ${bar20(m.howNonInterventionRate)} ${pct1(m.howNonInterventionRate)} intervention-free · ${m.interventionsPerIssue.toFixed(2)} interventions/issue`,
  );
  if (m.byAgent.length) {
    L.push('');
    L.push('  by agent:');
    for (const a of m.byAgent) {
      L.push(`    ${a.agent.padEnd(8)} samples=${a.samples} pass@1=${pct1(a.passAt1)} eventual=${pct1(a.passEventual)} cost=$${a.costUsd.toFixed(2)}`);
    }
  }
  if (m.byInvocationProvider.length) {
    L.push('');
    L.push('  invocations by actual provider:');
    for (const provider of m.byInvocationProvider) {
      L.push(
        `    ${provider.provider.padEnd(8)} total=${provider.total} completed=${provider.completed} stuck=${provider.stuck} timeout=${provider.timeout} failed=${provider.failed}`,
      );
    }
  }
  return L.join('\n');
}

export { computeMetrics };
