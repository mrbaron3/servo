(() => {
  'use strict';
  const state = {
    csrf: '',
    items: [],
    lastGood: null,
    selected: null,
    delivery: null,
    connected: false,
    requestGeneration: 0,
    pendingKeys: new Map(),
    dialogTrigger: null,
    restoreSelector: '',
    selectedCardId: '',
    nextPageToken: '',
    anomalyFilter: 'all',
    provenance: null,
  };
  const byId = (id) => document.getElementById(id);
  const live = (message) => {
    byId('command-outcome').textContent = message;
    byId('live').textContent = '';
    window.setTimeout(() => { byId('live').textContent = message; }, 20);
  };
  const escapeHTML = (value) => String(value ?? '').replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
  const formatTime = (value) => value ? new Intl.DateTimeFormat('ja-JP', {
    dateStyle: 'short', timeStyle: 'medium',
  }).format(new Date(value)) : '—';
  const commandKey = (scope) => {
    if (!state.pendingKeys.has(scope)) state.pendingKeys.set(scope, crypto.randomUUID());
    return state.pendingKeys.get(scope);
  };
  const clearCommandKey = (scope) => state.pendingKeys.delete(scope);
  const stableValue = (value) => {
    if (Array.isArray(value)) return value.map(stableValue);
    if (value && typeof value === 'object') return Object.fromEntries(
      Object.keys(value).sort().map((key) => [key, stableValue(value[key])]),
    );
    return value;
  };
  const sameConfiguration = (left, right) =>
    JSON.stringify(stableValue(left || {})) === JSON.stringify(stableValue(right || {}));
  const outcomeText = (body) => {
    const outcome = body?.outcome;
    if (!outcome) return '';
    const recoverability = outcome.recoverability ? ` / 復旧: ${outcome.recoverability}` : '';
    return `${outcome.outcome}${outcome.reason ? ` (${outcome.reason})` : ''}${recoverability}`;
  };
  const deliveryRecoveryReason = (reason) => ({
    monitor_only: 'MONITOR_ONLY: 監視は継続していますが実行キューは停止中です。実行を再開するには agentopsctl start --mode ACTIVE --build を実行してください。',
    lifecycle_monitor_only: 'MONITOR_ONLY: 監視は継続していますが実行キューは停止中です。実行を再開するには agentopsctl start --mode ACTIVE --build を実行してください。',
    lifecycle_draining: 'DRAINING: 既存処理の排出中のため新規実行を開始しません。agentopsctl status で active-leases=0 / in-flight-attempts=0 を確認してください。排出完了後も DRAINING は維持されます。ACTIVE へ復旧するには agentopsctl start --mode ACTIVE --build を実行してください。',
    lifecycle_off: 'OFF: 監視と実行を停止しています。agentopsctl start --mode MONITOR_ONLY --build または agentopsctl start --mode ACTIVE --build を実行してください。',
  }[reason] || reason || '—');
  const showDialogError = (id, error) => {
    const summary = byId(id);
    const structured = outcomeText(error.body);
    summary.textContent = `${error.message}${structured ? ` / outcome: ${structured}` : ''}`;
    summary.hidden = false;
    summary.focus();
  };
  const rememberDialogTrigger = () => {
    state.dialogTrigger = document.activeElement;
    state.restoreSelector = '';
  };
  const restoreDialogFocus = () => {
    const target = (state.restoreSelector && document.querySelector(state.restoreSelector))
      || (state.dialogTrigger && document.contains(state.dialogTrigger) ? state.dialogTrigger : null);
    state.restoreSelector = '';
    state.dialogTrigger = null;
    target?.focus();
  };
  const disableOperationalControls = () => {
    byId('create').disabled = true;
    byId('load-more').disabled = true;
    byId('save-registration').disabled = true;
    byId('confirm-dialog').querySelector('.danger').disabled = true;
    byId('retry-delivery').disabled = true;
  };
  const markDisconnected = (error) => {
    state.connected = false;
    state.lastGood = error.body?.error?.lastSuccessfulAt || state.lastGood;
    disableOperationalControls();
    byId('alert').hidden = false;
    byId('alert').textContent = `Control API に接続できません。表示中の値は最終正常取得 ${formatTime(state.lastGood)} のものです。`;
    byId('freshness').textContent = `切断 · 最終正常 ${formatTime(state.lastGood)}`;
    render();
  };
  const markSessionUnavailable = () => {
    state.connected = false;
    state.csrf = '';
    disableOperationalControls();
    byId('alert').hidden = false;
    byId('alert').textContent = 'Operator session が失効または拒否されました。新しい一回限りの bootstrap URL が必要です。';
    byId('freshness').textContent = 'Operator session 失効';
    render();
  };
  const handleOperationalFailure = (error) => {
    if (error.status === 401) {
      markSessionUnavailable();
      return true;
    }
    if (['control_store_unavailable', 'request_failed'].includes(error.code)) {
      markDisconnected(error);
      return true;
    }
    return false;
  };

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (options.body) headers.set('Content-Type', 'application/json');
    if (options.method && !['GET', 'HEAD'].includes(options.method)) {
      headers.set('X-CSRF-Token', state.csrf);
    }
    let response;
    try {
      response = await fetch(path, {
        ...options, headers, credentials: 'same-origin', redirect: 'error',
      });
    } catch (cause) {
      const error = new Error('Control API network request failed', { cause });
      error.code = 'request_failed';
      error.status = 0;
      error.body = {};
      throw error;
    }
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(body.error?.message || `Control API ${response.status}`);
      error.code = body.error?.code || 'request_failed';
      error.status = response.status;
      error.body = body;
      throw error;
    }
    return body;
  }
  async function statusPage(path) {
    let lastError;
    for (let attempt = 0; attempt < 3; attempt += 1) {
      try {
        return await api(path);
      } catch (error) {
        lastError = error;
        if (!['control_store_unavailable', 'request_failed'].includes(error.code)
          || attempt === 2) throw error;
        await new Promise((resolve) => window.setTimeout(resolve, 250 * (attempt + 1)));
      }
    }
    throw lastError;
  }

  function componentClass(component) {
    if (component.freshness !== 'fresh' || ['failed', 'disconnected', 'unknown'].includes(component.actual)) return 'bad';
    if (component.recoveryState && !['none', 'recovered'].includes(component.recoveryState)) return 'warn';
    return '';
  }
  function componentDiverges(name, component) {
    const desiredStates = {
      issue_monitor: ['starting', 'running'],
      pr_monitor: ['starting', 'running'],
      forwarder: ['starting', 'running'],
      execution: ['running', 'waiting', 'idle', 'paused_by_mode'],
      queue: ['idle', 'queued', 'leased', 'blocked_by_mode'],
    };
    const stoppedStates = {
      issue_monitor: ['stopped'],
      pr_monitor: ['stopped'],
      forwarder: ['stopped'],
      execution: ['stopped', 'idle'],
      queue: ['idle'],
    };
    const allowed = component.desired ? desiredStates[name] : stoppedStates[name];
    return !allowed?.includes(component.actual);
  }
  function isAnomaly(item) {
    return Object.entries(item.components).some(([name, component]) =>
      component.freshness !== 'fresh'
      || ['failed', 'disconnected', 'unknown'].includes(component.actual)
      || componentDiverges(name, component));
  }
  function anomalyKinds(item) {
    const components = Object.entries(item.components);
    const kinds = new Set();
    if (components.some(([, component]) => component.actual === 'failed')) kinds.add('failed');
    if (components.some(([, component]) => component.actual === 'disconnected')) kinds.add('disconnected');
    if (components.some(([, component]) => component.freshness === 'stale')) kinds.add('stale');
    if (components.some(([name, component]) => {
      if (component.actual === 'unknown') return true;
      if (['failed', 'disconnected'].includes(component.actual)) return false;
      return componentDiverges(name, component);
    })) kinds.add('divergent');
    return kinds;
  }
  function component(name, value) {
    const label = {
      issue_monitor: 'Issue Monitor', pr_monitor: 'PR Monitor', forwarder: 'Forwarder',
      execution: 'Execution', queue: 'Queue',
    }[name] || name;
    return `<section class="component" aria-label="${escapeHTML(label)}">
      <p class="component-name">${escapeHTML(label)}</p>
      <div class="state-line">
        <span class="desired">desired: ${value.desired ? 'ON' : 'OFF'}</span>
        <span class="badge ${componentClass(value)}">${escapeHTML(value.actual)}</span>
      </div>
      <dl>
        <dt>鮮度</dt><dd>${escapeHTML(value.freshness)}</dd>
        <dt>観測</dt><dd>${formatTime(value.observedAt)}</dd>
        <dt>最終正常</dt><dd>${formatTime(value.lastGoodAt)}</dd>
        <dt>復旧</dt><dd>${escapeHTML(value.recoveryState || 'none')}</dd>
        <dt>理由</dt><dd>${escapeHTML(value.staleReason || value.lastError || '—')}</dd>
      </dl>
    </section>`;
  }
  const KANBAN_LANES = [
    ['ready', 'ready'],
    ['intake-planning', 'intake / planning'],
    ['design', 'design'],
    ['implementation', 'implementation'],
    ['review', 'review'],
    ['repair', 'repair'],
    ['gate-wait', 'gate wait'],
    ['human-escalated', 'human escalated'],
    ['merge-ready', 'merge ready'],
    ['released', 'released'],
    ['failed', 'failed'],
  ];
  function kanbanLane(current) {
    if (current.kanbanLane) return current.kanbanLane;
    if (['failed', 'cancelled', 'rejected'].includes(current.jobStatus)) return 'failed';
    if (current.state === 'blocked' || current.phase === 'human-review') return 'human-escalated';
    if (current.state === 'waiting') return 'gate-wait';
    if (current.phase === 'completed') return current.state === 'succeeded' ? 'released' : 'failed';
    if (current.phase === 'merge') return 'merge-ready';
    if (current.phase === 'review' || current.phase === 'validation') return 'review';
    if (current.phase === 'repair') return 'repair';
    if (current.phase === 'generation' || current.phase === 'implementation') return 'implementation';
    if (current.phase === 'design') return 'design';
    if (current.phase === 'planning' || current.phase === 'intake') return 'intake-planning';
    return 'ready';
  }
  function formatDuration(seconds) {
    if (!Number.isFinite(Number(seconds))) return '—';
    const total = Math.max(0, Math.floor(Number(seconds)));
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const remainder = total % 60;
    if (hours > 0) return `${hours}h ${minutes}m`;
    if (minutes > 0) return `${minutes}m ${remainder}s`;
    return `${remainder}s`;
  }
  function developmentProgressCard(issue) {
    const current = issue.current;
    const events = Array.isArray(issue.history) ? issue.history : [];
    const visibleState = current.state;
    const lane = kanbanLane(current);
    const subject = `${issue.repository}#${Number(issue.issueNumber)}`;
    const reviewRound = Number(current.reviewRound || 0);
    const laneTitle = lane === 'review' && reviewRound > 0 ? `review round ${reviewRound}` : lane;
    const escalationEvidence = current.escalationId
      ? JSON.stringify(current.escalationEvidence || {})
      : '—';
    const reviewPerspectives = Array.isArray(current.reviewPerspectives)
      ? current.reviewPerspectives
      : [];
    const perspectiveEvidence = reviewPerspectives.map((perspective) => {
      const findings = Array.isArray(perspective.findings) ? perspective.findings : [];
      const findingLines = findings.map((finding) =>
        `<li><strong>${escapeHTML(finding.severity || 'unknown')} · ${escapeHTML(finding.disposition || 'legacy')}</strong> ${escapeHTML(finding.criterionId || 'finding')}: ${escapeHTML(finding.observed || '—')}${finding.separationReason ? `<br><span>分離理由: ${escapeHTML(finding.separationReason)}</span>` : ''}</li>`
      ).join('');
      return `<li><strong>${escapeHTML(perspective.perspective || 'unknown')}</strong> · ${escapeHTML(perspective.verdict || 'unknown')} · ${Number(perspective.findingCount || findings.length)} finding(s)${findingLines ? `<ul>${findingLines}</ul>` : ''}</li>`;
    }).join('');
    const lineage = current.branchLineage && typeof current.branchLineage === 'object'
      ? current.branchLineage
      : {};
    const lineageChildren = Array.isArray(lineage.children) ? lineage.children : [];
    const lineageEvidence = lineage.nodeId
      ? `<p>node <code>${escapeHTML(lineage.nodeId)}</code> · ${escapeHTML(lineage.status || 'unknown')}${lineage.pullRequestNumber ? ` · PR #${Number(lineage.pullRequestNumber)}` : ''}${lineage.headSha ? ` @ <code>${escapeHTML(lineage.headSha)}</code>` : ''}${lineage.parentIssueNumber ? ` · parent #${Number(lineage.parentIssueNumber)} @ <code>${escapeHTML(lineage.parentHeadSha || '—')}</code>` : ''}</p>${lineageChildren.length > 0 ? `<ul>${lineageChildren.map((child) => `<li>#${Number(child.issueNumber)} · ${escapeHTML(child.status || 'unknown')} · parent <code>${escapeHTML(child.parentHeadSha || '—')}</code>${child.pullRequestNumber ? ` · PR #${Number(child.pullRequestNumber)}` : ''}${child.headSha ? ` @ <code>${escapeHTML(child.headSha)}</code>` : ''}</li>`).join('')}</ul>` : ''}`
      : '<p class="quiet">lineage なし</p>';
    const history = [...events].reverse().map((event) =>
      `<li><time>${formatTime(event.occurredAt)}</time><span>${escapeHTML(event.phase)}</span><strong>${escapeHTML(event.state)}</strong><span>${escapeHTML(event.step)}</span></li>`
    ).join('');
    return `<section class="development-progress kanban-issue-card" aria-label="${escapeHTML(subject)} の実装進捗">
      <header class="progress-head">
        <div><p class="component-name">${escapeHTML(laneTitle)} · ${escapeHTML(subject)}</p>
          <strong>${escapeHTML(current.phase)} / ${escapeHTML(current.step)}</strong></div>
        <span class="badge ${['failed', 'blocked'].includes(visibleState) ? 'bad' : visibleState === 'waiting' ? 'warn' : ''}">${escapeHTML(visibleState)}</span>
      </header>
      <dl class="progress-grid">
        <dt>開始</dt><dd>${formatTime(issue.startedAt || current.occurredAt)}</dd>
        <dt>最終活動</dt><dd>${formatTime(issue.lastActivity)}</dd>
        <dt>次の gate</dt><dd>${escapeHTML(current.nextGate || '—')}</dd>
        <dt>gate 待機</dt><dd>${formatDuration(current.gateWaitSeconds)}</dd>
        <dt>blocker</dt><dd>${escapeHTML(current.blocker || current.jobLastError || '—')}</dd>
        <dt>人間の操作</dt><dd>${escapeHTML(current.humanAction || '—')}</dd>
        <dt>escalation</dt><dd>${current.escalationId ? `#${Number(current.escalationId)} · ${formatTime(current.escalatedAt)}` : '—'}</dd>
        <dt>evidence</dt><dd><code>${escapeHTML(escalationEvidence)}</code></dd>
        <dt>head SHA</dt><dd><code>${escapeHTML(current.headSha || '—')}</code></dd>
        <dt>review</dt><dd>${reviewRound > 0 ? `round ${reviewRound} · ${escapeHTML(current.reviewOutcome || '進行中')}` : '—'}</dd>
        <dt>branch</dt><dd>${escapeHTML(current.branch || '—')}</dd>
        <dt>親 Epic</dt><dd>${current.parentIssueNumber ? `#${Number(current.parentIssueNumber)}` : '—'}</dd>
        <dt>PR</dt><dd>${current.pullRequestNumber ? `#${Number(current.pullRequestNumber)}` : '—'}</dd>
        <dt>owner</dt><dd>${escapeHTML(current.workerId)} · attempt ${Number(current.attemptNumber)}</dd>
        <dt>session</dt><dd>${escapeHTML(current.sessionName || '—')}</dd>
        <dt>worktree</dt><dd><code>${escapeHTML(current.worktreePath || '—')}</code></dd>
      </dl>
      <details class="progress-history"><summary>review evidence ${reviewPerspectives.length} perspective(s)</summary><ul>${perspectiveEvidence || '<li>まだ記録されていません</li>'}</ul></details>
      <details class="progress-history"><summary>branch DAG ${lineageChildren.length} child(ren)</summary>${lineageEvidence}</details>
      <details class="progress-history"><summary>工程履歴 ${events.length}件</summary><ol>${history}</ol></details>
    </section>`;
  }
  function developmentProgress(item) {
    const issues = Array.isArray(item.developmentProgress) ? item.developmentProgress : [];
    if (issues.length === 0) {
      return `<section class="development-progress progress-empty" aria-label="実装進捗">
        <div><p class="component-name">実装進捗</p><strong>永続イベントはまだありません</strong></div>
        <p class="quiet">runner が次の工程へ遷移すると、Issue単位の状態がここに表示されます。</p>
      </section>`;
    }
    const issuesByLane = new Map(KANBAN_LANES.map(([lane]) => [lane, []]));
    issues.forEach((issue) => {
      const lane = kanbanLane(issue.current);
      if (!issuesByLane.has(lane)) issuesByLane.set(lane, []);
      issuesByLane.get(lane).push(issue);
    });
    const columns = KANBAN_LANES.map(([lane, label]) => {
      const laneIssues = issuesByLane.get(lane) || [];
      return `<section class="kanban-column" aria-label="${escapeHTML(label)} lane" data-lane="${escapeHTML(lane)}">
        <header class="kanban-column-head"><strong>${escapeHTML(label)}</strong><span>${laneIssues.length}</span></header>
        <div class="kanban-column-cards">${laneIssues.map(developmentProgressCard).join('') || '<p class="quiet kanban-empty">対象なし</p>'}</div>
      </section>`;
    }).join('');
    return `<div class="development-progress-list"><div class="kanban-board" aria-label="${escapeHTML(item.registration.repository)} の開発 Kanban">${columns}</div>${item.developmentProgressTruncated
      ? `<section class="development-progress progress-truncated" aria-label="実装進捗の省略">
        <p class="quiet">直近 ${issues.length} Issue のみ表示しています。全件は <code>agentopsctl progress</code> で読めます。</p>
      </section>`
      : ''}</div>`;
  }
  function card(item) {
    const r = item.registration;
    const componentNames = ['issue_monitor', 'pr_monitor'];
    if (Object.prototype.hasOwnProperty.call(item.components, 'forwarder')) {
      componentNames.push('forwarder');
    }
    componentNames.push('execution', 'queue');
    const components = componentNames
      .map((name) => component(name, item.components[name] || {
        desired: false, actual: 'unknown', freshness: 'unknown', recoveryState: 'unknown',
      })).join('');
    const deliveries = (item.recentDeliveryFailures || []).map((failure) =>
      `<li><span>${escapeHTML(failure.event)} · ${escapeHTML(failure.status)} · attempts ${Number(failure.routeAttempts || 0)}</span>
        <button class="ghost" type="button" data-delivery="${escapeHTML(failure.id)}" ${state.connected ? '' : 'disabled'}>確認${failure.status === 'failed' ? '・再試行' : ''}</button></li>`).join('');
    return `<article class="card ${isAnomaly(item) ? 'anomaly' : ''} ${state.selectedCardId === r.id ? 'selected' : ''}" data-id="${escapeHTML(r.id)}">
      <header class="card-head">
        <div>
          <h3><button class="card-title" type="button" data-select="${escapeHTML(r.id)}" aria-label="${escapeHTML(r.repository)} の状態詳細を選択">${escapeHTML(r.repository)}</button></h3>
          <span class="quiet">version ${r.version} · ${r.enabled ? 'enabled' : 'disabled'} · ${escapeHTML(item.mode)}</span>
        </div>
        <div class="card-actions">
          <button class="ghost" type="button" data-edit="${escapeHTML(r.id)}" ${state.connected ? '' : 'disabled'}>編集</button>
          <button class="primary" type="button" data-enable="${escapeHTML(r.id)}" ${!r.enabled && state.connected ? '' : 'hidden disabled'}>再有効化</button>
          <button class="danger" type="button" data-disable="${escapeHTML(r.id)}" ${r.enabled && state.connected ? '' : 'disabled'}>無効化</button>
        </div>
      </header>
      ${developmentProgress(item)}
      <div class="components">${components}</div>
      <details class="details">
        <summary>配送・ジョブ詳細</summary>
        <div class="details-grid">
          <span>Issue cursor advanced<br><strong>${formatTime((item.cursorObservedAt || item.lastPoll)?.issue)}</strong></span>
          <span>PR cursor advanced<br><strong>${formatTime((item.cursorObservedAt || item.lastPoll)?.pull_request)}</strong></span>
          <span>Last delivery<br><strong>${formatTime(item.lastDelivery)}</strong></span>
          <span>Queue depth<br><strong>${Number(item.queueDepth || 0)}</strong></span>
          <span>Active job<br><strong>${escapeHTML(item.activeJobId || '—')} / ${escapeHTML(item.activeJobState || '—')} / version ${escapeHTML(item.activeJobRegistrationVersion || '—')}</strong></span>
          <span>Last error<br><strong>${escapeHTML(item.lastJobFailure?.lastError || '—')}</strong></span>
          <span>Gate SLA<br><strong>${Number(r.configuration?.gateTimeoutSeconds?.default || 3600)} seconds default</strong></span>
          <ul class="delivery-list" aria-label="最近の失敗・無視配送">${deliveries || '<li>失敗・無視配送はありません</li>'}</ul>
        </div>
      </details>
    </article>`;
  }
  function render() {
    const query = byId('search').value.trim().toLowerCase();
    const matchingRepository = state.items.filter((item) =>
      item.registration.repository.includes(query));
    const counts = {
      all: matchingRepository.length,
      failed: 0,
      disconnected: 0,
      stale: 0,
      divergent: 0,
    };
    matchingRepository.forEach((item) => {
      anomalyKinds(item).forEach((kind) => { counts[kind] += 1; });
    });
    Object.entries(counts).forEach(([kind, count]) => {
      byId('anomaly-filters').querySelector(`[data-filter-count="${kind}"]`).textContent = count;
    });
    const items = matchingRepository.filter((item) =>
      state.anomalyFilter === 'all' || anomalyKinds(item).has(state.anomalyFilter));
    byId('cards').innerHTML = items.map(card).join('');
    byId('empty').hidden = items.length !== 0;
    byId('count').textContent = `${items.length} 件`;
  }

  async function load({ announce = true } = {}) {
    const generation = ++state.requestGeneration;
    byId('refresh').disabled = true;
    byId('cards').setAttribute('aria-busy', 'true');
    byId('freshness').textContent = '取得中';
    try {
      const page = await statusPage('/v1/registrations?limit=200');
      if (generation !== state.requestGeneration) return page;
      state.items = page.items || [];
      state.nextPageToken = page.nextPageToken || '';
      state.connected = true;
      state.lastGood = page.lastSuccessfulAt || page.observedAt;
      byId('mode').textContent = page.mode;
      state.provenance = page.provenance || null;
      const revision = state.provenance?.revision;
      byId('provenance').textContent = revision
        ? `${state.provenance.repository}@${revision.slice(0, 12)}`
        : 'build provenance 未設定';
      byId('freshness').textContent = `最終取得 ${formatTime(state.lastGood)}`;
      byId('alert').hidden = true;
      byId('create').disabled = false;
      byId('load-more').hidden = !state.nextPageToken;
      byId('load-more').disabled = false;
      render();
      if (announce) {
        live(`Operating Mode ${page.mode}、${state.items.length} 件の Registration を取得しました`);
      }
      return page;
    } catch (error) {
      if (generation !== state.requestGeneration) throw error;
      if (error.status === 401) {
        markSessionUnavailable();
        live(`Operator session 拒否: ${error.message}`);
      } else if (error.code === 'control_store_unavailable' || error.code === 'request_failed') {
        markDisconnected(error);
        live(`Control API 接続失敗: ${error.message}`);
      } else {
        byId('alert').hidden = false;
        byId('alert').textContent = `状態取得要求が拒否されました (${error.code})。入力またはsnapshotを再取得してください。`;
        live(`状態取得要求拒否: ${error.message}`);
      }
      throw error;
    } finally {
      if (generation === state.requestGeneration) {
        byId('refresh').disabled = false;
        byId('cards').setAttribute('aria-busy', 'false');
      }
    }
  }

  async function loadMore() {
    const token = state.nextPageToken;
    const generation = state.requestGeneration;
    if (!token) return;
    byId('load-more').disabled = true;
    byId('cards').setAttribute('aria-busy', 'true');
    try {
      const page = await statusPage(`/v1/registrations?limit=200&pageToken=${encodeURIComponent(token)}`);
      if (generation !== state.requestGeneration) return;
      state.items = [...state.items, ...(page.items || [])];
      state.nextPageToken = page.nextPageToken || '';
      byId('load-more').hidden = !state.nextPageToken;
      render();
      byId('load-more').focus();
      live(`${page.items?.length || 0} 件を追加し、合計 ${state.items.length} 件になりました`);
    } catch (error) {
      if (error.status === 401) {
        markSessionUnavailable();
      } else if (error.code === 'control_store_unavailable' || error.code === 'request_failed') {
        markDisconnected(error);
        byId('alert').textContent = `追加ページを確認できませんでした。表示中の値は最終正常取得 ${formatTime(state.lastGood)} のものです。`;
      } else {
        state.nextPageToken = '';
        byId('load-more').hidden = true;
        byId('alert').hidden = false;
        byId('alert').textContent = 'ページsnapshotが失効または拒否されました。「再取得」で先頭から読み直してください。';
      }
      live(`追加ページ取得失敗 (${error.code}): ${error.message}`);
    } finally {
      byId('cards').setAttribute('aria-busy', 'false');
      if (state.connected && state.nextPageToken) byId('load-more').disabled = false;
    }
  }

  function openRegistration(registration = null) {
    rememberDialogTrigger();
    state.selected = registration;
    byId('registration-error').hidden = true;
    byId('registration-reload').hidden = true;
    byId('save-registration').disabled = false;
    byId('registration-title').textContent = registration ? 'Registration を更新' : 'Registration を追加';
    byId('registration-id').value = registration?.id || '';
    byId('registration-version').value = registration?.version || '';
    byId('repository').value = registration?.repository || '';
    byId('repository').readOnly = Boolean(registration);
    byId('enabled').checked = registration?.enabled ?? true;
    byId('issue-enabled').checked = registration?.issueMonitorEnabled ?? true;
    byId('pr-enabled').checked = registration?.prMonitorEnabled ?? true;
    byId('execution-enabled').checked = registration?.executionEnabled ?? false;
    const timeouts = registration?.configuration?.gateTimeoutSeconds || {};
    byId('gate-sla-default').value = String(timeouts.default || 3600);
    byId('gate-sla-planning').value = timeouts.planning || '';
    byId('gate-sla-design').value = timeouts.design || '';
    byId('gate-sla-graders').value = timeouts['repository-graders'] || '';
    byId('gate-sla-review').value = timeouts.review || '';
    byId('gate-sla-merge').value = timeouts.merge || '';
    byId('registration-dialog').showModal();
    byId('repository').focus();
  }

  async function saveRegistration(event) {
    event.preventDefault();
    const id = byId('registration-id').value;
    const version = byId('registration-version').value;
    const repository = byId('repository').value.trim().toLowerCase();
    const body = {
      enabled: byId('enabled').checked,
      issueMonitorEnabled: byId('issue-enabled').checked,
      prMonitorEnabled: byId('pr-enabled').checked,
      executionEnabled: byId('execution-enabled').checked,
    };
    const gateTimeoutSeconds = { default: Number(byId('gate-sla-default').value) };
    [
      ['planning', 'gate-sla-planning'],
      ['design', 'gate-sla-design'],
      ['repository-graders', 'gate-sla-graders'],
      ['review', 'gate-sla-review'],
      ['merge', 'gate-sla-merge'],
    ].forEach(([key, input]) => {
      if (byId(input).value !== '') gateTimeoutSeconds[key] = Number(byId(input).value);
    });
    body.configuration = {
      ...(state.selected?.configuration || {}),
      gateTimeoutSeconds,
    };
    if (!id) body.repository = repository;
    const scope = id ? `update:${id}:${version}` : `create:${repository}`;
    const headers = { 'Idempotency-Key': commandKey(scope) };
    if (id) headers['If-Match'] = `"${version}"`;
    byId('save-registration').disabled = true;
    byId('registration-error').hidden = true;
    let versionConflict = false;
    try {
      const response = await api(id ? `/v1/registrations/${id}` : '/v1/registrations', {
        method: id ? 'PATCH' : 'POST', headers, body: JSON.stringify(body),
      });
      const page = await statusPage(`/v1/registrations?repository=${encodeURIComponent(repository)}`);
      const verified = page.items?.[0]?.registration;
      const expected = response.registration;
      if (!verified || verified.version !== expected.version
        || ['enabled', 'issueMonitorEnabled', 'prMonitorEnabled', 'executionEnabled']
          .some((key) => verified[key] !== expected[key])
        || !sameConfiguration(verified.configuration, expected.configuration)) {
        throw new Error('authoritative re-query did not verify the command outcome');
      }
      clearCommandKey(scope);
      await load({ announce: false });
      state.restoreSelector = id ? `[data-edit="${CSS.escape(id)}"]` : '#create';
      byId('registration-dialog').close();
      live(`${repository} version ${verified.version} の反映を確認しました (${response.outcome.outcome})`);
    } catch (error) {
      live(`保存失敗: ${error.message}`);
      handleOperationalFailure(error);
      showDialogError('registration-error', error);
      versionConflict = error.body?.outcome?.outcome === 'version_conflict';
      byId('registration-reload').hidden = !versionConflict;
    } finally {
      byId('save-registration').disabled = versionConflict || !state.connected;
    }
  }

  async function disableRegistration(event) {
    event.preventDefault();
    const r = state.selected;
    if (!r) return;
    const scope = `disable:${r.id}:${r.version}`;
    byId('confirm-error').hidden = true;
    try {
      const response = await api(`/v1/registrations/${r.id}/disable`, {
        method: 'POST',
        headers: { 'Idempotency-Key': commandKey(scope), 'If-Match': `"${r.version}"` },
        body: '{}',
      });
      const page = await statusPage(`/v1/registrations?repository=${encodeURIComponent(r.repository)}`);
      const verified = page.items?.[0]?.registration;
      if (!verified || verified.enabled || verified.version !== response.registration.version) {
        throw new Error('authoritative re-query did not verify disable');
      }
      clearCommandKey(scope);
      await load({ announce: false });
      state.restoreSelector = `[data-select="${CSS.escape(r.id)}"]`;
      byId('confirm-dialog').close();
      live(`${r.repository} version ${verified.version} の無効化を確認しました (${response.outcome.outcome})`);
    } catch (error) {
      live(`無効化失敗: ${error.message}`);
      handleOperationalFailure(error);
      showDialogError('confirm-error', error);
    }
  }

  async function enableRegistration(registration) {
    const scope = `enable:${registration.id}:${registration.version}`;
    try {
      const response = await api(`/v1/registrations/${registration.id}`, {
        method: 'PATCH',
        headers: {
          'Idempotency-Key': commandKey(scope),
          'If-Match': `"${registration.version}"`,
        },
        body: JSON.stringify({ enabled: true }),
      });
      const page = await statusPage(`/v1/registrations?repository=${encodeURIComponent(registration.repository)}`);
      const verified = page.items?.[0]?.registration;
      if (!verified || !verified.enabled || verified.version !== response.registration.version) {
        throw new Error('authoritative re-query did not verify re-enable');
      }
      clearCommandKey(scope);
      await load({ announce: false });
      document.querySelector(`[data-enable="${CSS.escape(registration.id)}"]`)?.focus();
      live(`${registration.repository} version ${verified.version} の再有効化を確認しました (${response.outcome.outcome})`);
    } catch (error) {
      live(`再有効化失敗: ${error.message}`);
      handleOperationalFailure(error);
    }
  }

  async function openDelivery(item, deliveryId) {
    rememberDialogTrigger();
    try {
      byId('delivery-error').hidden = true;
      const delivery = await api(`/v1/deliveries/${encodeURIComponent(deliveryId)}`);
      state.delivery = { item, delivery };
      byId('delivery-detail').innerHTML = `
        <dt>Delivery</dt><dd>${escapeHTML(delivery.id)}</dd>
        <dt>State</dt><dd>${escapeHTML(delivery.status)}</dd>
        <dt>Attempts</dt><dd>${Number(delivery.routeAttempts || 0)}</dd>
        <dt>Registration</dt><dd>${escapeHTML(delivery.registrationId || '—')} / version ${escapeHTML(delivery.registrationVersion || '—')}</dd>
        <dt>Last error / recovery</dt><dd>${escapeHTML(delivery.lastError || deliveryRecoveryReason(delivery.ignoredReason))}</dd>
        <dt>Updated</dt><dd>${formatTime(delivery.updatedAt)}</dd>`;
      const retryable = delivery.status === 'failed'
        && delivery.registrationId === item.registration.id
        && delivery.registrationVersion === item.registration.version
        && item.registration.enabled
        && item.registration.executionEnabled;
      byId('retry-delivery').disabled = !retryable;
      byId('delivery-dialog').showModal();
      (retryable ? byId('retry-delivery') : byId('delivery-title')).focus();
    } catch (error) {
      handleOperationalFailure(error);
      live(`Delivery 取得失敗: ${error.message}`);
    }
  }

  async function retryDelivery(event) {
    event.preventDefault();
    const current = state.delivery;
    if (!current) return;
    const delivery = current.delivery;
    const scope = `retry:${delivery.id}:${delivery.routeAttempts}:${delivery.registrationVersion}`;
    byId('retry-delivery').disabled = true;
    try {
      const response = await api(`/v1/deliveries/${encodeURIComponent(delivery.id)}/retry`, {
        method: 'POST',
        headers: { 'Idempotency-Key': commandKey(scope) },
        body: JSON.stringify({
          observedAttempts: delivery.routeAttempts,
          expectedRegistrationId: delivery.registrationId,
          expectedRegistrationVersion: delivery.registrationVersion,
        }),
      });
      const verified = await api(`/v1/deliveries/${encodeURIComponent(delivery.id)}`);
      const acceptedAttempt = verified.retryAttempts?.find((attempt) =>
        attempt.attemptId === response.retry.attemptId);
      if (response.retry.deliveryId !== delivery.id
        || response.retry.state !== 'pending'
        || verified.id !== delivery.id
        || verified.registrationId !== delivery.registrationId
        || verified.registrationVersion !== delivery.registrationVersion
        || verified.routeAttempts < delivery.routeAttempts
        || !['pending', 'processing', 'processed', 'ignored', 'failed'].includes(verified.status)
        || acceptedAttempt?.status !== 'accepted'
        || acceptedAttempt.observedRouteAttempts !== delivery.routeAttempts) {
        throw new Error('authoritative re-query did not verify retry');
      }
      clearCommandKey(scope);
      await load({ announce: false });
      state.restoreSelector = `[data-select="${CSS.escape(current.item.registration.id)}"]`;
      byId('delivery-dialog').close();
      live(`Delivery ${delivery.id} の再試行受付と durable ${verified.status} state を確認しました (${response.outcome.outcome})`);
    } catch (error) {
      live(`再試行失敗: ${error.message}`);
      handleOperationalFailure(error);
      showDialogError('delivery-error', error);
      byId('retry-delivery').disabled = !state.connected;
    }
  }

  async function start() {
    let session;
    try {
      session = await api('/v1/browser-session');
    } catch (error) {
      const handled = handleOperationalFailure(error);
      if (error.status === 401 || !handled) {
        byId('alert').hidden = false;
        byId('alert').textContent = 'Operator session がありません。起動時に表示された一回限りの loopback bootstrap URL を開いてください。';
      }
      live(`セッション確認失敗: ${error.message}`);
      return;
    }
    state.csrf = session.csrfToken;
    await load().catch(() => {});
  }

  byId('refresh').addEventListener('click', () => load().catch(() => {}));
  byId('load-more').addEventListener('click', loadMore);
  byId('create').addEventListener('click', () => openRegistration());
  byId('search').addEventListener('input', render);
  byId('anomaly-filters').addEventListener('click', (event) => {
    const button = event.target.closest('[data-filter]');
    if (!button) return;
    state.anomalyFilter = button.dataset.filter;
    byId('anomaly-filters').querySelectorAll('[data-filter]').forEach((candidate) =>
      candidate.setAttribute('aria-pressed', String(candidate === button)));
    render();
    live(`${button.textContent.trim()} で絞り込みました`);
  });
  byId('registration-form').addEventListener('submit', saveRegistration);
  byId('confirm-form').addEventListener('submit', disableRegistration);
  byId('delivery-form').addEventListener('submit', retryDelivery);
  document.querySelectorAll('[data-close]').forEach((button) =>
    button.addEventListener('click', () => byId('registration-dialog').close()));
  document.querySelectorAll('[data-confirm-close]').forEach((button) =>
    button.addEventListener('click', () => byId('confirm-dialog').close()));
  document.querySelectorAll('[data-delivery-close]').forEach((button) =>
    button.addEventListener('click', () => byId('delivery-dialog').close()));
  ['registration-dialog', 'confirm-dialog', 'delivery-dialog'].forEach((id) =>
    byId(id).addEventListener('close', restoreDialogFocus));
  byId('registration-reload').addEventListener('click', async () => {
    const id = byId('registration-id').value;
    state.restoreSelector = id ? `[data-select="${CSS.escape(id)}"]` : '#create';
    await load({ announce: false }).catch(() => {});
    byId('registration-dialog').close();
  });
  byId('cards').addEventListener('click', (event) => {
    const select = event.target.closest('[data-select]');
    const edit = event.target.closest('[data-edit]');
    const disable = event.target.closest('[data-disable]');
    const enable = event.target.closest('[data-enable]');
    const delivery = event.target.closest('[data-delivery]');
    const id = select?.dataset.select || edit?.dataset.edit || disable?.dataset.disable || enable?.dataset.enable
      || delivery?.closest('.card')?.dataset.id;
    const item = state.items.find((candidate) => candidate.registration.id === id);
    if (!item) return;
    if (select) {
      state.selectedCardId = item.registration.id;
      render();
      document.querySelector(`[data-select="${CSS.escape(item.registration.id)}"]`)?.focus();
      live(`${item.registration.repository} の状態詳細を選択しました。異常: ${isAnomaly(item) ? 'あり' : 'なし'}`);
    }
    if (edit) openRegistration(item.registration);
    if (enable) enableRegistration(item.registration);
    if (disable) {
      rememberDialogTrigger();
      state.selected = item.registration;
      byId('confirm-error').hidden = true;
      byId('confirm-dialog').querySelector('.danger').disabled = false;
      byId('confirm-copy').textContent = `${item.registration.repository} version ${item.registration.version} を無効化します。`;
      byId('confirm-dialog').showModal();
      byId('confirm-title').focus();
    }
    if (delivery) openDelivery(item, delivery.dataset.delivery);
  });
  byId('signout').addEventListener('click', async () => {
    try { await api('/v1/browser-session', { method: 'DELETE' }); } finally {
      state.csrf = '';
      state.items = [];
      state.connected = false;
      state.nextPageToken = '';
      byId('create').disabled = true;
      byId('load-more').hidden = true;
      byId('alert').hidden = false;
      byId('alert').textContent = 'Operator session を終了しました。再開するには新しい一回限りの bootstrap が必要です。';
      render();
      live('Operator session を終了しました');
    }
  });
  start();
})();
