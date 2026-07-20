(() => {
  const liveState = document.querySelector('#liveState');
  const generatedAt = document.querySelector('#generatedAt');
  const healthMark = document.querySelector('#healthMark');
  const healthTitle = document.querySelector('#healthTitle');
  const healthDescription = document.querySelector('#healthDescription');
  const metrics = document.querySelector('#metrics');
  const runPulse = document.querySelector('#runPulse');
  const attentionCount = document.querySelector('#attentionCount');
  const attentionList = document.querySelector('#attentionList');
  const systemTopologyCanvas = document.querySelector('#systemTopologyCanvas');
  const systemTopologyEdges = document.querySelector('#systemTopologyEdges');
  const systemTopologyNodes = document.querySelector('#systemTopologyNodes');
  const systemTopologyDescription = document.querySelector('#systemTopologyDescription');
  const componentDetail = document.querySelector('#componentDetail');
  const runRows = document.querySelector('#runRows');
  const runDrawer = document.querySelector('#runDrawer');
  const drawerScrim = document.querySelector('#drawerScrim');
  const drawerClose = document.querySelector('#drawerClose');
  const drawerTitle = document.querySelector('#drawerTitle');
  const runSummary = document.querySelector('#runSummary');
  const runFlowViewport = document.querySelector('.run-flow-scroll');
  const runFlowCanvas = document.querySelector('#runFlowCanvas');
  const runFlowBands = document.querySelector('#runFlowBands');
  const runFlowEdges = document.querySelector('#runFlowEdges');
  const runFlowNodes = document.querySelector('#runFlowNodes');
  const runFlowDescription = document.querySelector('#runFlowDescription');
  const runStepDetail = document.querySelector('#runStepDetail');
  const runCanvasBack = document.querySelector('#runCanvasBack');
  const runCanvasMode = document.querySelector('#runCanvasMode');
  const runCanvasLegend = document.querySelector('#runCanvasLegend');
  const runCanvasReset = document.querySelector('#runCanvasReset');
  const runActions = document.querySelector('#runActions');
  const cancelRun = document.querySelector('#cancelRun');
  const retryRun = document.querySelector('#retryRun');
  const exportEpisode = document.querySelector('#exportEpisode');
  const runActionStatus = document.querySelector('#runActionStatus');
  const eventCount = document.querySelector('#eventCount');
  const eventList = document.querySelector('#events');
  const notice = document.querySelector('#connectionNotice');
  const evolutionSummary = document.querySelector('#evolutionSummary');
  const evolutionList = document.querySelector('#evolutionList');
  let runs = [];
  let evolutionReleases = [];
  let systemOverview;
  let selectedRun;
  let runFlowSteps = [];
  let focusedRunLoopID = '';
  let selectedRunStepID = '';
  let runCanvasViewKey = '';
  const runCanvasPan = { x: 24, y: 24 };
  let runPanGesture;
  let stream;
  let refreshTimer;

  async function request(path, options = {}) {
    const method = options.method || 'GET';
    const headers = new Headers(options.headers || {});
    if (method !== 'GET') headers.set('X-Eri-CSRF', '1');
    const response = await fetch(path, { ...options, method, headers, credentials: 'same-origin' });
    if (!response.ok) {
      let message = `HTTP ${response.status}`;
      try {
        const payload = await response.json();
        message = payload.error?.message || message;
      } catch (_) { /* safe fallback */ }
      throw new Error(message);
    }
    return response.json();
  }

  function statusText(status) {
    return ({
      active: 'Running', running: 'Running', dispatched: 'Executing', planned: 'Planned', queued: 'Queued', waiting: 'Waiting',
      succeeded: 'Completed', completed: 'Completed', confirmed: 'Confirmed', sent: 'Delivered', failed: 'Failed', unknown: 'Unknown outcome', superseded: 'Superseded by newer input', superseded_before_execution: 'Skipped for newer input',
      canceled: 'Canceled', online: 'Online', idle: 'Idle', pass: 'Passed', repair: 'Repair required', hold: 'On hold', escalate: 'Escalated'
    })[status] || status || 'Unknown';
  }

  function formatDateTime(value) {
    if (!value) return '—';
    return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(value));
  }

  function durationMs(run) {
    if (!run?.started_at) return 0;
    const end = run.ended_at && new Date(run.ended_at).getFullYear() > 1 ? new Date(run.ended_at) : new Date();
    return Math.max(0, end - new Date(run.started_at));
  }

  function formatDuration(milliseconds) {
    if (!milliseconds) return '—';
    if (milliseconds < 1000) return `${milliseconds} ms`;
    if (milliseconds < 60000) return `${(milliseconds / 1000).toFixed(milliseconds < 10000 ? 1 : 0)} s`;
    if (milliseconds < 3600000) return `${Math.floor(milliseconds / 60000)}m ${Math.round((milliseconds % 60000) / 1000)}s`;
    return `${(milliseconds / 3600000).toFixed(1)} h`;
  }

  function percentile(values, percent) {
    if (!values.length) return 0;
    const sorted = [...values].sort((a, b) => a - b);
    return sorted[Math.min(sorted.length - 1, Math.ceil(sorted.length * percent) - 1)];
  }

  async function loadDashboard() {
    const [overview, payload, evolution] = await Promise.all([
      request('/api/v1/system/overview'),
      request('/api/v1/runs?limit=100'),
      request('/api/v1/evolution/releases?limit=20')
    ]);
    systemOverview = overview;
    runs = payload.runs || [];
    evolutionReleases = evolution.releases || [];
    renderDashboard();
    liveState.dataset.live = 'true';
    liveState.querySelector('b').textContent = 'Live';
    generatedAt.textContent = formatDateTime(overview.generated_at);
    notice.hidden = true;
  }

  function renderDashboard() {
    renderHealth();
    renderMetrics();
    renderPulse();
    renderAttention();
    renderSystemTopology();
    renderRunTable();
    renderEvolution();
  }

  function renderEvolution() {
    evolutionSummary.replaceChildren();
    evolutionList.replaceChildren();
    const active = evolutionReleases.find((release) => release.status === 'active');
    const canary = evolutionReleases.find((release) => release.status === 'canary');
    const summary = document.createElement('p');
    summary.textContent = canary
      ? `v${canary.version} is in canary; ${canary.pass_count}/8 online passes. The first non-pass triggers rollback.`
      : active ? `v${active.version} is active; no canary is running.` : 'The baseline is active; no candidate has passed the offline gate.';
    evolutionSummary.append(summary);
    if (!evolutionReleases.length) {
      const empty = document.createElement('p');
      empty.className = 'evolution-empty';
      empty.textContent = 'No evolution release yet. A budgeted offline experiment requires at least six failure signals.';
      evolutionList.append(empty);
      return;
    }
    evolutionReleases.forEach((release) => {
      const row = document.createElement('article');
      row.className = 'evolution-release';
      row.dataset.status = release.status;
      const heading = document.createElement('div');
      const title = document.createElement('b');
      title.textContent = `v${release.version}`;
      const status = document.createElement('span');
      status.textContent = ({ active: 'Active', canary: 'Canary', retired: 'Retired' })[release.status] || release.status;
      heading.append(title, status);
      const evidence = document.createElement('p');
      const gain = Number(release.offline_score || 0) - Number(release.baseline_score || 0);
      evidence.textContent = `Training ${release.training_signal_count} · Holdout ${release.holdout_signal_count} · Offline ${Math.round((release.offline_score || 0) * 100)}% · Δ ${gain >= 0 ? '+' : ''}${Math.round(gain * 100)}%`;
      const online = document.createElement('small');
      online.textContent = `Online passes ${release.pass_count} · Failures ${release.fail_count} · ${formatDateTime(release.created_at)}`;
      row.append(heading, evidence, online);
      evolutionList.append(row);
    });
  }

  function renderHealth() {
    const active = runs.filter((run) => run.status === 'active');
    const recent = runs.slice(0, 10);
    const failures = recent.filter((run) => run.status === 'failed' || run.errors > 0);
    const degraded = failures.length > 0;
    healthMark.dataset.state = degraded ? 'degraded' : 'healthy';
    healthTitle.textContent = degraded ? 'Recent runs need attention' : active.length ? 'Healthy · Eri is working' : 'System healthy';
    if (degraded) {
      healthDescription.textContent = `${failures.length} of the latest 10 runs failed or recorded errors. Inspect them below.`;
    } else if (active.length) {
      healthDescription.textContent = `The daemon is online with ${active.length} active tasks. No failures were recorded in the latest 10 runs.`;
    } else {
      healthDescription.textContent = 'The daemon is online with no active backlog. No failures were recorded in the latest 10 runs.';
    }
  }

  function renderMetrics() {
    const active = runs.filter((run) => run.status === 'active').length;
    const failed = runs.filter((run) => run.status === 'failed' || run.errors > 0).length;
    const decisive = runs.filter((run) => ['succeeded', 'failed'].includes(run.status));
    const succeeded = decisive.filter((run) => run.status === 'succeeded').length;
    const successRate = decisive.length ? `${Math.round(succeeded * 100 / decisive.length)}%` : '—';
    const completedDurations = runs.filter((run) => run.ended_at && new Date(run.ended_at).getFullYear() > 1).map(durationMs);
    const p95 = formatDuration(percentile(completedDurations, .95));
    const tokens = runs.reduce((total, run) => total + run.input_tokens + run.output_tokens, 0);
    const values = [
      ['Active tasks', active, 'Now'],
      ['Success rate', successRate, 'Completed and failed runs'],
      ['Runs with errors', failed, 'Latest 100'],
      ['P95 duration', p95, 'Terminal runs'],
      ['Tokens', new Intl.NumberFormat().format(tokens), 'Input + output']
    ];
    metrics.replaceChildren();
    values.forEach(([label, value, note]) => {
      const item = document.createElement('div');
      item.className = 'metric';
      const name = document.createElement('span');
      name.textContent = label;
      const amount = document.createElement('b');
      amount.textContent = value;
      const context = document.createElement('small');
      context.textContent = note;
      item.append(name, amount, context);
      metrics.append(item);
    });
  }

  function renderPulse() {
    const sample = runs.slice(0, 60).reverse();
    runPulse.replaceChildren();
    if (!sample.length) {
      const empty = document.createElement('p');
      empty.className = 'attention-empty';
      empty.textContent = 'No run data yet.';
      runPulse.append(empty);
      return;
    }
    const durations = sample.map(durationMs);
    const ceiling = Math.max(...durations, 1);
    sample.forEach((run) => {
      const mark = document.createElement('button');
      mark.type = 'button';
      mark.className = 'run-mark';
      mark.dataset.status = run.status;
      mark.dataset.errors = String(run.errors > 0);
      const height = 18 + Math.round(Math.sqrt(durationMs(run) / ceiling) * 132);
      mark.style.setProperty('--mark-height', `${height}px`);
      mark.title = `${formatDateTime(run.started_at)} · ${statusText(run.status)} · ${formatDuration(durationMs(run))}`;
      mark.setAttribute('aria-label', mark.title);
      mark.addEventListener('click', () => openRun(run.id));
      runPulse.append(mark);
    });
  }

  function renderAttention() {
    const attention = runs.filter((run) => run.status === 'failed' || run.status === 'unknown' || run.errors > 0).slice(0, 5);
    attentionCount.textContent = String(attention.length);
    attentionList.replaceChildren();
    if (!attention.length) {
      const empty = document.createElement('div');
      empty.className = 'attention-empty';
      const mark = document.createElement('span');
      mark.textContent = '✓';
      const copy = document.createElement('p');
      copy.textContent = 'No anomalous runs in the recent sample';
      empty.append(mark, copy);
      attentionList.append(empty);
      return;
    }
    attention.forEach((run) => {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'attention-item';
      const dot = document.createElement('span');
      dot.className = 'attention-dot';
      const copy = document.createElement('span');
      copy.className = 'attention-copy';
      const title = document.createElement('b');
      title.textContent = run.status === 'failed' ? 'Run failed' : 'Run recorded errors';
      const meta = document.createElement('small');
      meta.textContent = `${formatDateTime(run.started_at)} · ${run.errors} errors`;
      copy.append(title, meta);
      const status = document.createElement('small');
      status.textContent = 'Inspect';
      button.append(dot, copy, status);
      button.addEventListener('click', () => openRun(run.id));
      attentionList.append(button);
    });
  }

  function renderSystemTopology() {
    const components = systemOverview?.components || [];
    const edges = systemOverview?.topology?.edges || [];
    systemTopologyEdges.replaceChildren();
    systemTopologyNodes.replaceChildren();
    componentDetail.replaceChildren();
    if (!components.length) {
      componentDetail.textContent = 'No component snapshot is available.';
      return;
    }

    const nodeWidth = 140;
    const nodeHeight = 66;
    const columnGap = 34;
    const rowGap = 20;
    const marginX = 28;
    const marginY = 34;
    const groups = new Map();
    components.forEach((component) => {
      const stage = Number(component.stage || 0);
      if (!groups.has(stage)) groups.set(stage, []);
      groups.get(stage).push(component);
    });
    const laneOrder = { primary: 0, action: 1, support: 2 };
    groups.forEach((group) => group.sort((a, b) => (laneOrder[a.lane] ?? 9) - (laneOrder[b.lane] ?? 9)));
    const stages = [...groups.keys()].sort((a, b) => a - b);
    const maxRows = Math.max(...[...groups.values()].map((group) => group.length));
    const width = Math.max(980, marginX * 2 + stages.length * nodeWidth + Math.max(0, stages.length - 1) * columnGap);
    const height = Math.max(300, marginY * 2 + maxRows * nodeHeight + Math.max(0, maxRows - 1) * rowGap);
    systemTopologyCanvas.style.width = `${width}px`;
    systemTopologyCanvas.style.height = `${height}px`;
    systemTopologyEdges.setAttribute('viewBox', `0 0 ${width} ${height}`);
    systemTopologyEdges.setAttribute('width', String(width));
    systemTopologyEdges.setAttribute('height', String(height));

    const positions = new Map();
    stages.forEach((stage, column) => {
      const group = groups.get(stage);
      const groupHeight = group.length * nodeHeight + Math.max(0, group.length - 1) * rowGap;
      const startY = (height - groupHeight) / 2;
      group.forEach((component, row) => {
        positions.set(component.id, { x: marginX + column * (nodeWidth + columnGap), y: startY + row * (nodeHeight + rowGap) });
      });
    });

    const title = svgElement('title');
    title.textContent = 'Eri system execution topology';
    const description = svgElement('desc');
    description.textContent = 'Components are arranged from input through runtime, Agent, model and tools, evaluation, delivery, and outcome evidence.';
    systemTopologyDescription.textContent = description.textContent;
    const defs = svgElement('defs');
    const marker = svgElement('marker', { id: 'system-arrow', markerWidth: 8, markerHeight: 8, refX: 7, refY: 4, orient: 'auto', markerUnits: 'strokeWidth' });
    marker.append(svgElement('path', { d: 'M0 0 L8 4 L0 8 Z', fill: 'var(--line-strong)' }));
    defs.append(marker);
    systemTopologyEdges.append(title, description, defs);
    edges.forEach((edge) => {
      const source = positions.get(edge.from);
      const target = positions.get(edge.to);
      if (!source || !target) return;
      const startX = source.x + nodeWidth;
      const startY = source.y + nodeHeight / 2;
      const endX = target.x;
      const endY = target.y + nodeHeight / 2;
      const bend = Math.max(24, (endX - startX) * .48);
      const path = svgElement('path', {
        class: 'system-edge', d: `M${startX} ${startY} C${startX + bend} ${startY}, ${endX - bend} ${endY}, ${endX - 8} ${endY}`,
        'marker-end': 'url(#system-arrow)'
      });
      path.dataset.kind = edge.kind || 'flow';
      systemTopologyEdges.append(path);
    });

    components.forEach((component) => {
      const position = positions.get(component.id);
      const node = document.createElement('button');
      node.type = 'button';
      node.className = 'system-node';
      node.dataset.componentId = component.id;
      node.dataset.status = component.status;
      node.style.left = `${position.x}px`;
      node.style.top = `${position.y}px`;
      node.setAttribute('aria-pressed', 'false');
      const status = document.createElement('span');
      status.className = 'system-node-status';
      status.setAttribute('aria-hidden', 'true');
      const copy = document.createElement('span');
      copy.className = 'system-node-copy';
      const kind = document.createElement('small');
      kind.textContent = component.kind;
      const label = document.createElement('b');
      label.textContent = component.label;
      const summary = document.createElement('span');
      summary.textContent = metricSummary(component.metrics) || statusText(component.status);
      copy.append(kind, label, summary);
      node.append(status, copy);
      node.addEventListener('click', () => selectComponent(component, node));
      systemTopologyNodes.append(node);
    });
    const initial = components.find((component) => component.id === 'runtime') || components[0];
    selectComponent(initial, systemTopologyNodes.querySelector(`[data-component-id="${CSS.escape(initial.id)}"]`));
  }

  function selectComponent(component, node) {
    systemTopologyNodes.querySelectorAll('.system-node').forEach((item) => item.setAttribute('aria-pressed', 'false'));
    node?.setAttribute('aria-pressed', 'true');
    const heading = document.createElement('h3');
    heading.textContent = component.label;
    const status = document.createElement('p');
    status.className = 'component-detail-status';
    status.dataset.status = component.status;
    status.textContent = statusText(component.status);
    const privacy = document.createElement('p');
    privacy.textContent = component.privacy_summary || 'No additional privacy summary.';
    componentDetail.replaceChildren(heading, status, privacy);
    const entries = Object.entries(component.metrics || {});
    if (entries.length) {
      const list = document.createElement('dl');
      entries.forEach(([key, value]) => {
        const term = document.createElement('dt');
        term.textContent = key.replaceAll('_', ' ');
        const detail = document.createElement('dd');
        detail.textContent = typeof value === 'object' ? JSON.stringify(value) : String(value);
        list.append(term, detail);
      });
      componentDetail.append(list);
    }
  }

  function metricSummary(value) {
    const entries = Object.entries(value || {}).filter(([, item]) => ['string', 'number'].includes(typeof item) && item !== '' && item !== 0);
    return entries.slice(0, 2).map(([key, item]) => `${key.replaceAll('_', ' ')} ${item}`).join(' · ');
  }

  function renderRunTable() {
    runRows.replaceChildren();
    runs.slice(0, 30).forEach((run) => {
      const row = document.createElement('tr');
      const started = document.createElement('td');
      const link = document.createElement('button');
      link.type = 'button';
      link.className = 'run-link';
      link.textContent = formatDateTime(run.started_at);
      link.addEventListener('click', () => openRun(run.id));
      started.append(link);
      const statusCell = document.createElement('td');
      const status = document.createElement('span');
      status.className = 'status-label';
      status.dataset.status = run.status;
      status.dataset.errors = String(run.errors > 0);
      status.textContent = statusText(run.status);
      statusCell.append(status);
      const duration = document.createElement('td');
      duration.textContent = formatDuration(durationMs(run));
      const model = document.createElement('td');
      model.textContent = String(run.model_calls);
      const tool = document.createElement('td');
      tool.textContent = String(run.tool_calls);
      const errors = document.createElement('td');
      errors.textContent = String(run.errors);
      row.append(started, statusCell, duration, model, tool, errors);
      runRows.append(row);
    });
  }

  async function openRun(id) {
    drawerScrim.hidden = false;
    runDrawer.dataset.open = 'true';
    runDrawer.setAttribute('aria-hidden', 'false');
    drawerTitle.textContent = `Run ${id.slice(0, 8)}`;
    focusedRunLoopID = '';
    selectedRunStepID = '';
    runCanvasViewKey = '';
    runSummary.textContent = 'Loading run record…';
    runFlowEdges.replaceChildren();
    runFlowNodes.replaceChildren();
    runStepDetail.replaceChildren();
    eventList.replaceChildren();
    try {
      const detail = await request(`/api/v1/runs/${encodeURIComponent(id)}`);
      selectedRun = detail;
      renderRunDetail(detail);
    } catch (error) {
      runSummary.textContent = error.message;
    }
  }

  function closeRun() {
    selectedRun = undefined;
    runFlowSteps = [];
    focusedRunLoopID = '';
    selectedRunStepID = '';
    runCanvasViewKey = '';
    delete runDrawer.dataset.open;
    runDrawer.setAttribute('aria-hidden', 'true');
    drawerScrim.hidden = true;
  }

  function renderRunDetail(detail) {
    runSummary.replaceChildren();
    const title = document.createElement('h3');
    title.textContent = statusText(detail.run.status);
    const copy = document.createElement('p');
    copy.textContent = `Started ${formatDateTime(detail.run.started_at)} · ${formatDuration(durationMs(detail.run))}`;
    const facts = document.createElement('div');
    facts.className = 'run-facts';
    [`Models ${detail.run.model_calls}`, `Tools ${detail.run.tool_calls}`, `Input ${detail.run.input_tokens}`, `Output ${detail.run.output_tokens}`, `Errors ${detail.run.errors}`].forEach((value) => {
      const item = document.createElement('span');
      item.textContent = value;
      facts.append(item);
    });
    runSummary.append(title, copy, facts);
    renderFlow(buildFlowSteps(detail));
    renderRawEvents(detail.events || []);
    updateRunActions();
  }

  function buildFlowSteps(detail) {
    if (detail.spans?.length) {
      return detail.spans.map((span) => ({
        ...span,
        dependsOn: span.depends_on || [],
        at: span.started_at,
        event: null
      }));
    }
    const invocationByID = new Map((detail.invocations || []).map((item) => [item.id, item]));
    const effectByID = new Map((detail.effects || []).map((item) => [item.id, item]));
    const steps = [];
    const seen = new Set();
    (detail.events || []).forEach((event) => {
      let step;
      if (['task.started', 'task.recovered', 'task.resumed'].includes(event.type) && !seen.has('start')) {
        seen.add('start');
        step = { kind: 'runtime', title: event.type === 'task.recovered' ? 'Recover run' : 'Start processing', status: 'succeeded' };
      } else if (['invocation.dispatched', 'invocation.planned'].includes(event.type) && !seen.has('model')) {
        seen.add('model');
        const invocation = invocationByID.get(event.eriaggregateid);
        step = { kind: 'model', title: 'Model processing', status: invocation?.status || 'running' };
      } else if (event.type === 'effect.planned' && !seen.has(`effect:${event.eriaggregateid}`)) {
        seen.add(`effect:${event.eriaggregateid}`);
        const effect = effectByID.get(event.eriaggregateid);
        step = { kind: 'tool', title: effect?.tool_id ? `Tool · ${effect.tool_id}` : 'Tool operation', status: effect?.status || 'planned' };
      } else if (event.type === 'task.waiting' && !seen.has('waiting')) {
        seen.add('waiting');
        step = { kind: 'approval', title: 'Waiting for approval', status: 'waiting' };
      } else if (event.type === 'artifact.evaluated' && !seen.has(`eval:${event.eriaggregateid}`)) {
        seen.add(`eval:${event.eriaggregateid}`);
        const result = String(event.data?.result || '');
        step = { kind: 'eval', title: 'Pre-delivery evaluation', status: result === 'pass' ? 'succeeded' : result || 'waiting' };
      } else if (event.type === 'delivery.sent' && !seen.has('delivery')) {
        seen.add('delivery');
        step = { kind: 'delivery', title: 'Deliver response', status: 'sent' };
      } else if (['task.completed', 'task.failed', 'task.canceled'].includes(event.type) && !seen.has('finish')) {
        seen.add('finish');
        step = { kind: 'finish', title: event.type === 'task.completed' ? 'Run completed' : event.type === 'task.canceled' ? 'Run canceled' : 'Run failed', status: event.type.replace('task.', '').replace('completed', 'succeeded') };
      }
      if (step) steps.push({ ...step, id: event.id, event, at: event.time });
    });
    if (!steps.length) {
      steps.push({ kind: 'runtime', title: 'Run record', status: detail.run.status, id: detail.run.id });
    }
    return steps.map((step) => ({ ...step, dependsOn: [] }));
  }

  function svgElement(name, attributes = {}) {
    const element = document.createElementNS('http://www.w3.org/2000/svg', name);
    Object.entries(attributes).forEach(([key, value]) => element.setAttribute(key, String(value)));
    return element;
  }

  function glyph(kind) {
    return ({ runtime: 'RT', context: 'CX', memory: 'ME', agent_loop: 'AL', model: 'MD', checkpoint: 'CP', candidate: 'CA', tool: 'TL', observation: 'OB', approval: 'AP', eval: 'EV', repair: 'RP', delivery: 'DL', finish: 'FN' })[kind] || 'EV';
  }

  function calculateFlowDepths(steps) {
    const byID = new Map(steps.map((step) => [step.id, step]));
    const depths = new Map();
    const visiting = new Set();
    const depthOf = (step) => {
      if (depths.has(step.id)) return depths.get(step.id);
      if (visiting.has(step.id)) return 0;
      visiting.add(step.id);
      const parents = (step.dependsOn || []).map((id) => byID.get(id)).filter(Boolean);
      const depth = parents.length ? Math.max(...parents.map(depthOf)) + 1 : 0;
      visiting.delete(step.id);
      depths.set(step.id, depth);
      return depth;
    };
    steps.forEach(depthOf);
    return depths;
  }

  function renderFlow(steps) {
    runFlowSteps = steps;
    if (focusedRunLoopID && !steps.some((step) => step.id === focusedRunLoopID)) focusedRunLoopID = '';
    renderRunCanvas();
  }

  function renderRunCanvas() {
    const loop = focusedRunLoopID ? runFlowSteps.find((step) => step.id === focusedRunLoopID) : null;
    if (focusedRunLoopID && !loop) focusedRunLoopID = '';
    runCanvasBack.hidden = !focusedRunLoopID;
    runCanvasMode.textContent = focusedRunLoopID ? 'Agent Loop' : 'Run overview';
    runCanvasLegend.textContent = focusedRunLoopID ? 'Turns grow downward · recorded tool and eval branches' : 'Architecture-aligned · recorded causality';
    let steps = runFlowSteps.filter((step) => !step.metadata?.loop_id && step.kind !== 'agent_iteration');
    let groups = [];
    if (focusedRunLoopID) {
      const descendants = runFlowSteps.filter((step) => step.metadata?.loop_id === focusedRunLoopID);
      const supportIDs = new Set(loop.dependsOn || []);
      steps = [...runFlowSteps.filter((step) => supportIDs.has(step.id)), ...descendants.filter((step) => step.kind !== 'agent_iteration')];
      groups = descendants.filter((step) => step.kind === 'agent_iteration');
    }
    const nextViewKey = `${selectedRun?.run?.id || 'run'}:${focusedRunLoopID || 'overview'}`;
    const shouldCenter = nextViewKey !== runCanvasViewKey;
    runCanvasViewKey = nextViewKey;
    drawRunFlow(steps, groups, shouldCenter);
  }

  function layoutRunFlow(steps, groupSteps) {
    const nodeWidth = 160, nodeHeight = 68, columnGap = 52, rowGap = 22, marginX = 28, marginY = 20;
    if (!groupSteps.length) {
      const bandWidth = 194, bandGap = 16;
      const stages = [
        { title: 'Runtime intake', description: 'Durable task ownership', kinds: new Set(['runtime']) },
        { title: 'Personal context', description: 'Context and governed memory', kinds: new Set(['context', 'memory']) },
        { title: 'Agent core', description: 'LLM-driven Agent Loop', kinds: new Set(['agent_loop', 'model', 'checkpoint', 'candidate', 'observation', 'repair']) },
        { title: 'Capabilities & safety', description: 'Policy, approval, and tools', kinds: new Set(['approval', 'tool']) },
        { title: 'Evaluation & delivery', description: 'Judge, outbox, and receipt', kinds: new Set(['eval', 'delivery']) },
        { title: 'Runtime outcome', description: 'Committed terminal state', kinds: new Set(['finish']) }
      ];
      const stageFor = (step) => {
        const index = stages.findIndex((stage) => stage.kinds.has(step.kind));
        return index < 0 ? 2 : index;
      };
      const columns = stages.map(() => []);
      steps.forEach((step) => columns[stageFor(step)].push(step));
      const maxRows = Math.max(1, ...columns.map((group) => group.length));
      const width = marginX * 2 + stages.length * bandWidth + (stages.length - 1) * bandGap;
      const height = Math.max(380, marginY * 2 + 62 + maxRows * nodeHeight + Math.max(0, maxRows - 1) * rowGap + 20);
      const positions = new Map();
      const bands = [];
      columns.forEach((group, stageIndex) => {
        const x = marginX + stageIndex * (bandWidth + bandGap);
        bands.push({ kind: 'architecture', x, y: marginY, width: bandWidth, height: height - marginY * 2, title: stages[stageIndex].title, description: stages[stageIndex].description });
        const groupHeight = group.length * nodeHeight + Math.max(0, group.length - 1) * rowGap;
        const startY = Math.max(marginY + 58, (height - groupHeight) / 2 + 20);
        group.forEach((step, index) => positions.set(step.id, { x: x + (bandWidth - nodeWidth) / 2, y: startY + index * (nodeHeight + rowGap) }));
      });
      return { width, height, positions, bands, nodeWidth, nodeHeight };
    }
    const support = steps.filter((step) => !step.metadata?.iteration_id);
    const layouts = groupSteps.slice().sort((left, right) => Number(left.metadata?.iteration_ordinal) - Number(right.metadata?.iteration_ordinal)).map((group) => {
      const children = steps.filter((step) => step.metadata?.iteration_id === group.metadata?.iteration_id);
      const depths = calculateFlowDepths(children);
      const columns = new Map();
      children.forEach((step) => {
        const depth = depths.get(step.id) || 0;
        if (!columns.has(depth)) columns.set(depth, []);
        columns.get(depth).push(step);
      });
      const levels = Math.max(0, ...depths.values()) + 1;
      const maxRows = Math.max(1, ...[...columns.values()].map((column) => column.length));
      const contentWidth = 40 + levels * nodeWidth + Math.max(0, levels - 1) * columnGap;
      const height = 78 + maxRows * nodeHeight + Math.max(0, maxRows - 1) * rowGap + 24;
      return { group, columns, maxRows, contentWidth, height };
    });
    const supportWidth = support.length ? support.length * nodeWidth + Math.max(0, support.length - 1) * columnGap : 0;
    const width = Math.max(760, marginX * 2 + supportWidth, marginX * 2 + Math.max(0, ...layouts.map((layout) => layout.contentWidth)));
    const positions = new Map();
    support.forEach((step, index) => positions.set(step.id, { x: marginX + index * (nodeWidth + columnGap), y: marginY }));
    const bands = [];
    let cursorY = marginY + (support.length ? nodeHeight + 42 : 0);
    layouts.forEach((layout) => {
      const bandWidth = width - marginX * 2;
      bands.push({ kind: 'iteration', x: marginX, y: cursorY, width: bandWidth, height: layout.height, title: layout.group.title, description: layout.group.description });
      layout.columns.forEach((column, depth) => {
        const columnHeight = column.length * nodeHeight + Math.max(0, column.length - 1) * rowGap;
        const startY = cursorY + 58 + Math.max(0, (layout.height - 82 - columnHeight) / 2);
        column.forEach((step, index) => positions.set(step.id, { x: marginX + 20 + depth * (nodeWidth + columnGap), y: startY + index * (nodeHeight + rowGap) }));
      });
      cursorY += layout.height + 20;
    });
    const height = Math.max(380, cursorY + marginY - (layouts.length ? 20 : 0));
    return { width, height, positions, bands, nodeWidth, nodeHeight };
  }

  function applyRunCanvasPan() {
    runFlowCanvas.style.setProperty('--pan-x', `${runCanvasPan.x}px`);
    runFlowCanvas.style.setProperty('--pan-y', `${runCanvasPan.y}px`);
  }

  function centerRunCanvas() {
    const width = Number.parseFloat(runFlowCanvas.style.width) || runFlowCanvas.offsetWidth;
    const height = Number.parseFloat(runFlowCanvas.style.height) || runFlowCanvas.offsetHeight;
    runCanvasPan.x = Math.max(24, (runFlowViewport.clientWidth - width) / 2);
    runCanvasPan.y = Math.max(24, (runFlowViewport.clientHeight - height) / 2);
    applyRunCanvasPan();
  }

  function drawRunFlow(steps, groupSteps, shouldCenter = false) {
    runFlowEdges.replaceChildren();
    runFlowNodes.replaceChildren();
    runFlowBands.replaceChildren();
    runStepDetail.replaceChildren();
    if (!steps.length) return;
    const layout = layoutRunFlow(steps, groupSteps);
    runFlowCanvas.style.width = `${layout.width}px`;
    runFlowCanvas.style.height = `${layout.height}px`;
    runFlowEdges.setAttribute('viewBox', `0 0 ${layout.width} ${layout.height}`);
    runFlowEdges.setAttribute('width', String(layout.width));
    runFlowEdges.setAttribute('height', String(layout.height));
    layout.bands.forEach((band) => {
      const element = document.createElement('section');
      element.className = 'run-iteration-band';
      if (band.kind === 'architecture') element.classList.add('run-architecture-band');
      element.style.left = `${band.x}px`;
      element.style.top = `${band.y}px`;
      element.style.width = `${band.width}px`;
      element.style.height = `${band.height}px`;
      const label = document.createElement('b');
      label.textContent = band.title;
      const description = document.createElement('span');
      description.textContent = band.description || '';
      element.append(label, description);
      runFlowBands.append(element);
    });
    const title = svgElement('title');
    title.textContent = focusedRunLoopID ? 'Execution canvas for the selected Agent Loop turns' : 'Execution canvas for the selected run';
    const description = svgElement('desc');
    description.textContent = steps.map((step) => `${step.title}, ${statusText(step.status)}`).join('; ');
    runFlowDescription.textContent = `${description.textContent}. Edges only represent recorded causal dependencies; isolated nodes mean the projection lacks a dependency fact.`;
    const definitions = svgElement('defs');
    const marker = svgElement('marker', { id: 'runArrow', markerWidth: 8, markerHeight: 8, refX: 7, refY: 4, orient: 'auto', markerUnits: 'strokeWidth' });
    marker.append(svgElement('path', { d: 'M 0 0 L 8 4 L 0 8 z', fill: 'currentColor' }));
    definitions.append(marker);
    runFlowEdges.append(title, description, definitions);
    const stepByID = new Map(steps.map((step) => [step.id, step]));
    steps.forEach((step, index) => {
      const target = layout.positions.get(step.id);
      if (!target) return;
      (step.dependsOn || []).forEach((parentID) => {
        const source = layout.positions.get(parentID);
        if (!source || !target) return;
        const parent = stepByID.get(parentID);
        const crossesTurns = parent?.metadata?.iteration_id && step.metadata?.iteration_id && parent.metadata.iteration_id !== step.metadata.iteration_id;
        const entersFirstTurn = !parent?.metadata?.iteration_id && step.metadata?.iteration_id;
        let path;
        if (focusedRunLoopID && (crossesTurns || entersFirstTurn)) {
          const startX = source.x + layout.nodeWidth / 2;
          const startY = source.y + layout.nodeHeight;
          const endX = target.x + layout.nodeWidth / 2;
          const endY = target.y;
          const bend = Math.max(30, Math.abs(endY - startY) * .45);
          path = `M${startX} ${startY} C${startX} ${startY + bend}, ${endX} ${endY - bend}, ${endX} ${endY - 8}`;
        } else {
          const startX = source.x + layout.nodeWidth;
          const startY = source.y + layout.nodeHeight / 2;
          const endX = target.x;
          const endY = target.y + layout.nodeHeight / 2;
          const bend = Math.max(24, Math.abs(endX - startX) * .46);
          path = `M${startX} ${startY} C${startX + bend} ${startY}, ${endX - bend} ${endY}, ${endX - 8} ${endY}`;
        }
        runFlowEdges.append(svgElement('path', {
          class: 'flow-edge', d: path,
          'marker-end': 'url(#runArrow)'
        }));
      });
      const node = document.createElement('button');
      node.type = 'button';
      node.className = 'run-flow-node';
      node.dataset.status = step.status;
      node.dataset.kind = step.kind;
      node.dataset.stepId = step.id;
      node.style.left = `${target.x}px`;
      node.style.top = `${target.y}px`;
      node.setAttribute('aria-pressed', 'false');
      const state = document.createElement('span');
      state.className = 'run-node-status';
      state.setAttribute('aria-hidden', 'true');
      const copy = document.createElement('span');
      copy.className = 'run-node-copy';
      const kind = document.createElement('small');
      kind.textContent = `${String(index + 1).padStart(2, '0')} · ${glyph(step.kind)}`;
      const label = document.createElement('b');
      label.textContent = step.title;
      const meta = document.createElement('span');
      meta.textContent = statusText(step.status);
      copy.append(kind, label, meta);
      node.append(state, copy);
      node.addEventListener('click', () => selectRunStep(step, node));
      runFlowNodes.append(node);
    });
    const initial = steps.find((step) => step.id === selectedRunStepID)
      || steps.find((step) => ['active', 'running', 'failed', 'unknown', 'waiting'].includes(step.status))
      || steps.find((step) => step.kind === 'agent_loop')
      || steps[steps.length - 1];
    const initialNode = [...runFlowNodes.children].find((node) => node.dataset.stepId === initial.id);
    selectRunStep(initial, initialNode);
    if (shouldCenter) centerRunCanvas();
    else applyRunCanvasPan();
  }

  function selectRunStep(step, node) {
    selectedRunStepID = step.id;
    runFlowNodes.querySelectorAll('.run-flow-node').forEach((item) => item.setAttribute('aria-pressed', 'false'));
    node?.setAttribute('aria-pressed', 'true');
    const heading = document.createElement('h4');
    heading.textContent = step.title;
    const status = document.createElement('p');
    status.textContent = `${statusText(step.status)}${step.at ? ` · ${formatDateTime(step.at)}` : ''}`;
    const eventType = document.createElement('code');
    eventType.textContent = step.event?.type || step.kind;
    const description = document.createElement('p');
    description.textContent = step.description || 'Committed runtime fact.';
    runStepDetail.replaceChildren(heading, status, description, eventType);
    if (step.kind === 'agent_loop' && step.metadata?.focusable) {
      const action = document.createElement('button');
      action.type = 'button';
      action.className = 'run-canvas-action';
      action.textContent = `Open ${step.metadata.turn_count || ''} turns`;
      action.addEventListener('click', () => { focusedRunLoopID = step.id; selectedRunStepID = ''; renderRunCanvas(); });
      runStepDetail.append(action);
    }
    document.dispatchEvent(new CustomEvent('eri:observatory-step-selected', { detail: { step, target: runStepDetail } }));
  }

  function renderRawEvents(events) {
    eventCount.textContent = String(events.length);
    eventList.replaceChildren();
    events.forEach((event) => {
      const item = document.createElement('li');
      const title = document.createElement('b');
      title.textContent = event.type;
      const meta = document.createElement('small');
      meta.textContent = `${formatDateTime(event.time)} · ${event.eriaggregatetype} · ${event.eriaggregateid}`;
      const data = document.createElement('pre');
      data.textContent = JSON.stringify(event.data || {}, null, 2);
      item.append(title, meta, data);
      eventList.append(item);
    });
  }

  function updateRunActions() {
    const active = selectedRun?.run.status === 'active';
    const retryable = ['failed', 'canceled'].includes(selectedRun?.run.status);
    const terminal = ['succeeded', 'failed', 'canceled'].includes(selectedRun?.run.status);
    runActions.hidden = !(active || retryable || terminal);
    cancelRun.hidden = !active;
    retryRun.hidden = !retryable;
    exportEpisode.hidden = !terminal;
    cancelRun.disabled = !active;
    retryRun.disabled = !retryable;
    exportEpisode.disabled = !terminal;
    runActionStatus.textContent = '';
  }

  cancelRun.addEventListener('click', async () => {
    if (!selectedRun || selectedRun.run.status !== 'active') return;
    cancelRun.disabled = true;
    runActionStatus.textContent = 'Requesting safe cancellation…';
    try {
      const result = await request(`/api/v1/tasks/${encodeURIComponent(selectedRun.run.task_id)}/cancel`, { method: 'POST' });
      runActionStatus.textContent = result.effect === 'cancel_requested' ? 'Cancellation requested; the run will stop at a safe boundary.' : 'Canceled.';
      await refreshSelectedRun();
      await loadDashboard();
    } catch (error) {
      cancelRun.disabled = false;
      runActionStatus.textContent = error.message;
    }
  });

  retryRun.addEventListener('click', async () => {
    if (!selectedRun || !['failed', 'canceled'].includes(selectedRun.run.status)) return;
    retryRun.disabled = true;
    runActionStatus.textContent = 'Checking side-effect safety boundaries…';
    try {
      const result = await request(`/api/v1/tasks/${encodeURIComponent(selectedRun.run.task_id)}/retry`, { method: 'POST' });
      runActionStatus.textContent = `Safe retry created from ${result.checkpoint}; previous approvals will not be reused.`;
      await loadDashboard();
    } catch (error) {
      retryRun.disabled = false;
      runActionStatus.textContent = error.message;
    }
  });

  exportEpisode.addEventListener('click', async () => {
    if (!selectedRun) return;
    exportEpisode.disabled = true;
    runActionStatus.textContent = 'Preparing a governed episode…';
    try {
      const payload = await request('/api/v1/episodes?limit=500');
      const episode = payload.episodes.find((record) => record.task_id === selectedRun.run.task_id);
      if (!episode) throw new Error('The episode has not been built yet.');
      const manifest = await request(`/api/v1/episodes/${encodeURIComponent(episode.id)}/export`, { method: 'POST' });
      const blob = new Blob([`${JSON.stringify(manifest, null, 2)}\n`], { type: 'application/json' });
      const link = document.createElement('a');
      link.href = URL.createObjectURL(blob);
      link.download = `eri-episode-${episode.id}.json`;
      link.click();
      URL.revokeObjectURL(link.href);
      runActionStatus.textContent = 'Episode exported with causal metadata and without message bodies.';
    } catch (error) {
      runActionStatus.textContent = error.message;
    } finally {
      exportEpisode.disabled = false;
    }
  });

  async function refreshSelectedRun() {
    if (!selectedRun) return;
    const id = selectedRun.run.id;
    selectedRun = await request(`/api/v1/runs/${encodeURIComponent(id)}`);
    renderRunDetail(selectedRun);
  }

  function scheduleRefresh() {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => Promise.allSettled([loadDashboard(), refreshSelectedRun()]), 180);
  }

  function connectEvents() {
    if (stream) stream.close();
    stream = new EventSource('/api/v1/events');
    stream.addEventListener('eri', scheduleRefresh);
    stream.onopen = () => {
      liveState.dataset.live = 'true';
      liveState.querySelector('b').textContent = 'Live';
    };
    stream.onerror = () => {
      liveState.dataset.live = 'false';
      liveState.querySelector('b').textContent = 'Reconnecting';
    };
  }

  drawerClose.addEventListener('click', closeRun);
  drawerScrim.addEventListener('click', closeRun);
  runCanvasBack.addEventListener('click', () => {
    focusedRunLoopID = '';
    selectedRunStepID = '';
    renderRunCanvas();
  });
  runCanvasReset.addEventListener('click', centerRunCanvas);
  runFlowViewport.tabIndex = 0;
  runFlowViewport.addEventListener('pointerdown', (event) => {
    if (event.button !== 0 || event.target.closest('button')) return;
    runPanGesture = {
      pointerID: event.pointerId,
      originX: event.clientX,
      originY: event.clientY,
      panX: runCanvasPan.x,
      panY: runCanvasPan.y
    };
    runFlowViewport.dataset.panning = 'true';
    runFlowViewport.setPointerCapture(event.pointerId);
  });
  runFlowViewport.addEventListener('pointermove', (event) => {
    if (!runPanGesture || runPanGesture.pointerID !== event.pointerId) return;
    runCanvasPan.x = runPanGesture.panX + event.clientX - runPanGesture.originX;
    runCanvasPan.y = runPanGesture.panY + event.clientY - runPanGesture.originY;
    applyRunCanvasPan();
  });
  const finishRunPan = (event) => {
    if (!runPanGesture || runPanGesture.pointerID !== event.pointerId) return;
    runPanGesture = null;
    delete runFlowViewport.dataset.panning;
    if (runFlowViewport.hasPointerCapture(event.pointerId)) runFlowViewport.releasePointerCapture(event.pointerId);
  };
  runFlowViewport.addEventListener('pointerup', finishRunPan);
  runFlowViewport.addEventListener('pointercancel', finishRunPan);
  runFlowViewport.addEventListener('wheel', (event) => {
    event.preventDefault();
    runCanvasPan.x -= event.deltaX;
    runCanvasPan.y -= event.deltaY;
    applyRunCanvasPan();
  }, { passive: false });
  runFlowViewport.addEventListener('keydown', (event) => {
    const distance = event.shiftKey ? 80 : 28;
    const movement = {
      ArrowLeft: [distance, 0],
      ArrowRight: [-distance, 0],
      ArrowUp: [0, distance],
      ArrowDown: [0, -distance]
    }[event.key];
    if (!movement) return;
    event.preventDefault();
    runCanvasPan.x += movement[0];
    runCanvasPan.y += movement[1];
    applyRunCanvasPan();
  });
  document.addEventListener('keydown', (event) => { if (event.key === 'Escape' && selectedRun) closeRun(); });

  loadDashboard().then(connectEvents).catch((error) => {
    liveState.dataset.live = 'false';
    liveState.querySelector('b').textContent = 'Unavailable';
    healthMark.dataset.state = 'unavailable';
    healthTitle.textContent = 'System Observatory is unavailable';
    healthDescription.textContent = error.message;
    notice.hidden = false;
  });
  setInterval(() => loadDashboard().catch(() => {}), 5000);
})();
