(() => {
  const $ = (selector) => document.querySelector(selector);
  const workspace = $('#runWorkspace');
  const toggle = $('#runtimeToggle');
  const close = $('#runClose');
  const scrim = $('#runScrim');
  const count = $('#runtimeCount');
  const runtimeDot = $('#runtimeDot');
  const livePulse = $('#runLivePulse');
  const headerStatus = $('#runHeaderStatus');
  const nowCard = $('#nowCard');
  const activityList = $('#activityList');
  const runEmpty = $('#runEmpty');
  const traceView = $('#traceView');
  const traceTitle = $('#traceTitle');
  const traceDescription = $('#traceDescription');
  const traceFacts = $('#traceFacts');
  const canvasViewport = document.querySelector('.topology-scroll');
  const canvas = $('#topologyCanvas');
  const bands = $('#topologyBands');
  const edges = $('#topologyEdges');
  const nodes = $('#topologyNodes');
  const inspector = $('#stepInspector');
  const topologyDescription = $('#topologyDescription');
  const canvasBack = $('#canvasBack');
  const canvasMode = $('#canvasMode');
  const canvasSubtitle = $('#canvasSubtitle');
  const canvasLegend = $('#canvasLegend');
  const canvasReset = $('#canvasReset');
  const turnStrip = $('#turnStrip');
  const timeline = $('#timeline');
  let activity = { active: [], recent: [] };
  let selectedTaskID = '';
  let currentSteps = [];
  let focusedLoopID = '';
  let unfoldAllTurns = false;
  let selectedTurnOrdinal = 0;
  let selectedStepID = '';
  let canvasViewKey = '';
  const canvasPan = { x: 24, y: 24 };
  let panGesture;

  async function request(path) {
    const response = await fetch(path, { credentials: 'same-origin' });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    return response.json();
  }

  function statusText(status) {
    return ({ active: 'Running', running: 'Running', succeeded: 'Completed', completed: 'Completed', confirmed: 'Confirmed', failed: 'Failed', unknown: 'Unknown outcome', canceled: 'Canceled', superseded: 'Superseded by newer input', superseded_before_execution: 'Skipped for newer input', waiting: 'Waiting', planned: 'Planned', approved: 'Approved', sent: 'Delivered', pass: 'Passed', repair: 'Repair required', escalate: 'Escalation required', hold: 'On hold', blocked: 'Blocked', rejected_unknown_tool: 'Tool unavailable', rejected_truncated: 'Output truncated', user_denied: 'Denied by user' })[status] || status || 'Unknown';
  }

  function formatTime(value) {
    if (!value) return '—';
    return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(value));
  }

  function setOpen(open) {
    workspace.dataset.open = String(open);
    toggle.setAttribute('aria-expanded', String(open));
    scrim.hidden = !(open && window.matchMedia('(max-width: 900px)').matches);
  }

  function allRuns() { return [...(activity.active || []), ...(activity.recent || [])]; }

  function renderActivity() {
    const active = activity.active || [];
    const runs = allRuns().slice(0, 18);
    count.textContent = String(active.length);
    runtimeDot.dataset.active = String(active.length > 0);
    livePulse.dataset.active = String(active.length > 0);
    headerStatus.textContent = active.length ? `${active.length} ${active.length === 1 ? 'run is' : 'runs are'} active` : 'No active runs';
    nowCard.dataset.active = String(active.length > 0);
    nowCard.replaceChildren();
    const nowLabel = document.createElement('small');
    nowLabel.textContent = 'Current activity';
    const nowTitle = document.createElement('b');
    nowTitle.textContent = active.length ? `${active.length} ${active.length === 1 ? 'run' : 'runs'} in progress` : 'No run in progress';
    const nowMeta = document.createElement('span');
    nowMeta.textContent = active.length ? `Latest started ${formatTime(active[0].started_at)}` : 'Eri is standing by';
    nowCard.append(nowLabel, nowTitle, nowMeta);

    activityList.replaceChildren();
    if (!runs.length) {
      const empty = document.createElement('p');
      empty.className = 'activity-empty';
      empty.textContent = 'No observable runs yet.';
      activityList.append(empty);
      return;
    }
    runs.forEach((run) => {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'activity-item';
      button.setAttribute('aria-current', String(run.task_id === selectedTaskID));
      const dot = document.createElement('span');
      dot.className = 'activity-dot';
      dot.dataset.status = run.status;
      const copy = document.createElement('span');
      copy.className = 'activity-copy';
      const name = document.createElement('span');
      name.className = 'activity-name';
      name.textContent = run.status === 'active' ? 'Working' : `Run · ${run.run_id.slice(-6)}`;
      const meta = document.createElement('span');
      meta.className = 'activity-meta';
      meta.textContent = `${formatTime(run.started_at)} · Models ${run.model_calls} · Tools ${run.tool_calls}`;
      const state = document.createElement('span');
      state.className = 'activity-status';
      state.textContent = statusText(run.status);
      copy.append(name, meta, state);
      button.append(dot, copy);
      button.addEventListener('click', () => selectTrace(run.task_id));
      activityList.append(button);
    });
  }

  async function refreshActivity() {
    try {
      activity = await request('/api/v1/activity?limit=24');
      renderActivity();
      if (selectedTaskID && allRuns().some((run) => run.task_id === selectedTaskID)) await selectTrace(selectedTaskID, false);
    } catch (_) {
      headerStatus.textContent = 'Run observation is unavailable';
    }
  }

  function augmentMessages() {
    timeline?.querySelectorAll('.message[data-role="assistant"][data-task-id]').forEach((message) => {
      if (message.querySelector('.trace-button')) return;
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'trace-button';
      button.textContent = 'Inspect this run';
      button.addEventListener('click', () => selectTrace(message.dataset.taskId));
      message.querySelector('.message-footer')?.append(button);
    });
  }

  async function selectTrace(taskID, shouldOpen = true) {
    if (!taskID) return;
    if (selectedTaskID !== taskID) {
      focusedLoopID = '';
      selectedTurnOrdinal = 0;
      selectedStepID = '';
      unfoldAllTurns = false;
      canvasViewKey = '';
    }
    selectedTaskID = taskID;
    if (shouldOpen) setOpen(true);
    renderActivity();
    timeline?.querySelectorAll('.message').forEach((message) => {
      const selectedAnswer = message.dataset.role === 'assistant' && message.dataset.taskId === taskID;
      message.classList.toggle('is-selected', selectedAnswer);
    });
    try {
      renderTrace(await request(`/api/v1/traces/${encodeURIComponent(taskID)}`));
    } catch (_) {
      runEmpty.hidden = false;
      traceView.hidden = true;
      runEmpty.querySelector('h3').textContent = 'This run has no projection yet';
      runEmpty.querySelector('p').textContent = 'The runtime may still be committing facts. This view will refresh automatically.';
    }
  }

  function renderTrace(trace) {
    runEmpty.hidden = true;
    traceView.hidden = false;
    traceTitle.textContent = `Run · ${trace.run_id.slice(-8)}`;
    traceDescription.textContent = `${statusText(trace.status)} · Started ${formatTime(trace.started_at)}. Only committed runtime facts are shown.`;
    traceFacts.replaceChildren();
    [`Models ${trace.model_calls}`, `Tools ${trace.tool_calls}`, `Input ${trace.input_tokens}`, `Output ${trace.output_tokens}`].forEach((value) => {
      const item = document.createElement('span');
      item.textContent = value;
      traceFacts.append(item);
    });
    currentSteps = trace.steps || [];
    if (focusedLoopID && !currentSteps.some((step) => step.id === focusedLoopID)) focusedLoopID = '';
    renderCanvas();
  }

  function calculateDepths(steps) {
    const byID = new Map(steps.map((step) => [step.id, step]));
    const memo = new Map();
    const visiting = new Set();
    const depthOf = (step) => {
      if (memo.has(step.id)) return memo.get(step.id);
      if (visiting.has(step.id)) return 0;
      visiting.add(step.id);
      const parents = (step.depends_on || []).map((id) => byID.get(id)).filter(Boolean);
      const depth = parents.length ? Math.max(...parents.map(depthOf)) + 1 : 0;
      visiting.delete(step.id);
      memo.set(step.id, depth);
      return depth;
    };
    steps.forEach(depthOf);
    return memo;
  }

  function svgElement(name, attributes = {}) {
    const element = document.createElementNS('http://www.w3.org/2000/svg', name);
    Object.entries(attributes).forEach(([key, value]) => element.setAttribute(key, String(value)));
    return element;
  }

  function renderCanvas() {
    const loop = focusedLoopID ? currentSteps.find((step) => step.id === focusedLoopID) : null;
    if (focusedLoopID && !loop) focusedLoopID = '';
    canvasBack.hidden = !focusedLoopID;
    canvasMode.textContent = focusedLoopID ? 'Agent Loop' : 'Run overview';
    canvasSubtitle.textContent = focusedLoopID
      ? 'Turns grow downward; each turn keeps its recorded Model, Tool, Observation, checkpoint, and Eval dependencies.'
      : 'The run follows Eri system boundaries; edges only represent recorded dependencies.';
    canvasLegend.textContent = focusedLoopID
      ? 'Turn ↓ · Model → Tool / Approval → Observation / Eval'
      : 'Runtime → Context → Agent Loop → Capabilities → Eval → Delivery';
    const view = focusedLoopID ? buildLoopView(loop) : {
      steps: currentSteps.filter((step) => !step.metadata?.loop_id && step.kind !== 'agent_iteration'),
      focus: false,
      groups: []
    };
    renderTurnStrip(loop);
    const nextViewKey = `${selectedTaskID}:${focusedLoopID || 'run'}:${unfoldAllTurns ? 'all' : 'folded'}`;
    const shouldCenter = nextViewKey !== canvasViewKey;
    canvasViewKey = nextViewKey;
    renderTopology(view.steps, view.focus, view.groups, shouldCenter);
  }

  function buildLoopView(loop) {
    const descendants = currentSteps.filter((step) => step.metadata?.loop_id === loop.id);
    const supportIDs = new Set(loop.depends_on || []);
    const support = currentSteps.filter((step) => supportIDs.has(step.id));
    let groups = descendants.filter((step) => step.kind === 'agent_iteration');
    let visible = descendants.filter((step) => step.kind !== 'agent_iteration');
    const ordinals = groups.map((step) => Number(step.metadata?.iteration_ordinal || 0)).filter(Boolean).sort((a, b) => a - b);
    if (window.matchMedia('(max-width: 600px)').matches && ordinals.length) {
      if (!ordinals.includes(selectedTurnOrdinal)) selectedTurnOrdinal = Number(loop.metadata?.current_turn || ordinals[0]);
      groups = groups.filter((step) => Number(step.metadata?.iteration_ordinal) === selectedTurnOrdinal);
      visible = visible.filter((step) => Number(step.metadata?.iteration_ordinal) === selectedTurnOrdinal);
    } else if (ordinals.length > 5 && !unfoldAllTurns) {
      const keep = new Set([ordinals[0], ordinals[1], ordinals[ordinals.length - 2], ordinals[ordinals.length - 1]]);
      const hiddenOrdinals = ordinals.filter((ordinal) => !keep.has(ordinal));
      const hiddenIDs = new Set(descendants.filter((step) => hiddenOrdinals.includes(Number(step.metadata?.iteration_ordinal))).map((step) => step.id));
      const foldedID = `folded:${loop.id}`;
      const incoming = [...new Set(descendants.filter((step) => hiddenIDs.has(step.id)).flatMap((step) => step.depends_on || []).filter((id) => !hiddenIDs.has(id)))];
      groups = groups.filter((step) => keep.has(Number(step.metadata?.iteration_ordinal)));
      visible = visible.filter((step) => !hiddenIDs.has(step.id)).map((step) => ({
        ...step,
        depends_on: [...new Set((step.depends_on || []).map((id) => hiddenIDs.has(id) ? foldedID : id))]
      }));
      const foldedMeta = { loop_id: loop.id, iteration_id: foldedID, iteration_ordinal: ordinals[1] + .5, folded_count: hiddenOrdinals.length, hidden_turns: hiddenOrdinals };
      groups.push({ id: `iteration:${foldedID}`, kind: 'agent_iteration', title: `${hiddenOrdinals.length} Turns folded`, status: 'succeeded', metadata: foldedMeta, depends_on: [] });
      visible.push({ id: foldedID, parent_id: loop.id, kind: 'folded', lane: 'agent', title: `${hiddenOrdinals.length} intermediate turns`, description: 'Intermediate turns are folded for readability. Expanding them does not change any runtime fact.', status: 'succeeded', depends_on: incoming, metadata: foldedMeta });
    }
    return { steps: [...support, ...groups, ...visible], focus: true, groups };
  }

  function renderTurnStrip(loop) {
    turnStrip.replaceChildren();
    if (!loop || !window.matchMedia('(max-width: 600px)').matches) {
      turnStrip.hidden = true;
      return;
    }
    const groups = currentSteps.filter((step) => step.kind === 'agent_iteration' && step.metadata?.loop_id === loop.id)
      .sort((left, right) => Number(left.metadata?.iteration_ordinal) - Number(right.metadata?.iteration_ordinal));
    turnStrip.hidden = groups.length === 0;
    groups.forEach((group) => {
      const ordinal = Number(group.metadata?.iteration_ordinal);
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = `Turn ${ordinal}`;
      button.setAttribute('aria-current', String(ordinal === selectedTurnOrdinal));
      button.addEventListener('click', () => { selectedTurnOrdinal = ordinal; renderCanvas(); });
      turnStrip.append(button);
    });
  }

  function layoutRun(steps) {
    const nodeWidth = 156, nodeHeight = 68, rowGap = 24, marginX = 24, marginY = 18, bandWidth = 188, bandGap = 16;
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
    const height = Math.max(560, canvasViewport.clientHeight || 0, marginY * 2 + 62 + maxRows * nodeHeight + Math.max(0, maxRows - 1) * rowGap + 20);
    const positions = new Map();
    const runBands = [];
    columns.forEach((group, stageIndex) => {
      const x = marginX + stageIndex * (bandWidth + bandGap);
      runBands.push({ kind: 'architecture', x, y: marginY, width: bandWidth, height: height - marginY * 2, title: stages[stageIndex].title, trigger: stages[stageIndex].description });
      const groupHeight = group.length * nodeHeight + Math.max(0, group.length - 1) * rowGap;
      const startY = Math.max(marginY + 58, (height - groupHeight) / 2 + 20);
      group.forEach((step, index) => positions.set(step.id, { x: x + (bandWidth - nodeWidth) / 2, y: startY + index * (nodeHeight + rowGap) }));
    });
    return { width, height, positions, bands: runBands, nodeWidth, nodeHeight };
  }

  function layoutLoop(steps, groupSteps) {
    const nodeWidth = 156, nodeHeight = 68, columnGap = 54, rowGap = 20, marginX = 28, marginY = 24, bandGap = 20;
    const support = steps.filter((step) => !step.metadata?.iteration_id && step.kind !== 'agent_iteration');
    const renderedGroups = groupSteps.slice().sort((left, right) => Number(left.metadata?.iteration_ordinal) - Number(right.metadata?.iteration_ordinal));
    const layouts = renderedGroups.map((group) => {
      const children = steps.filter((step) => step.kind !== 'agent_iteration' && step.metadata?.iteration_id === group.metadata?.iteration_id);
      const depths = calculateDepths(children);
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
      return { group, children, columns, levels, maxRows, contentWidth, height };
    });
    const supportWidth = support.length ? support.length * nodeWidth + Math.max(0, support.length - 1) * columnGap : 0;
    const width = Math.max(760, marginX * 2 + supportWidth, marginX * 2 + Math.max(0, ...layouts.map((layout) => layout.contentWidth)));
    const positions = new Map();
    support.forEach((step, index) => positions.set(step.id, { x: marginX + index * (nodeWidth + columnGap), y: marginY }));
    const bandLayouts = [];
    let cursorY = marginY + (support.length ? nodeHeight + 42 : 0);
    layouts.forEach((layout) => {
      const bandWidth = width - marginX * 2;
      bandLayouts.push({ kind: 'iteration', x: marginX, y: cursorY, width: bandWidth, height: layout.height, title: layout.group.title, status: layout.group.status, trigger: layout.group.description });
      layout.columns.forEach((column, depth) => {
        const columnHeight = column.length * nodeHeight + Math.max(0, column.length - 1) * rowGap;
        const startY = cursorY + 58 + Math.max(0, (layout.height - 82 - columnHeight) / 2);
        column.forEach((step, index) => positions.set(step.id, { x: marginX + 20 + depth * (nodeWidth + columnGap), y: startY + index * (nodeHeight + rowGap) }));
      });
      cursorY += layout.height + bandGap;
    });
    const height = Math.max(560, canvasViewport.clientHeight || 0, cursorY + marginY - (layouts.length ? bandGap : 0));
    return { width, height, positions, bands: bandLayouts, nodeWidth, nodeHeight };
  }

  function applyCanvasPan() {
    canvas.style.setProperty('--pan-x', `${canvasPan.x}px`);
    canvas.style.setProperty('--pan-y', `${canvasPan.y}px`);
  }

  function centerCanvas() {
    const width = Number.parseFloat(canvas.style.width) || canvas.offsetWidth;
    const height = Number.parseFloat(canvas.style.height) || canvas.offsetHeight;
    canvasPan.x = Math.max(24, (canvasViewport.clientWidth - width) / 2);
    canvasPan.y = Math.max(24, (canvasViewport.clientHeight - height) / 2);
    applyCanvasPan();
  }

  function renderTopology(steps, loopFocus, groupSteps, shouldCenter = false) {
    edges.replaceChildren();
    nodes.replaceChildren();
    bands.replaceChildren();
    inspector.replaceChildren();
    const renderedSteps = steps.filter((step) => step.kind !== 'agent_iteration');
    if (!renderedSteps.length) return;
    const layout = loopFocus ? layoutLoop(steps, groupSteps) : layoutRun(renderedSteps);
    canvas.style.width = `${layout.width}px`;
    canvas.style.height = `${layout.height}px`;
    edges.setAttribute('viewBox', `0 0 ${layout.width} ${layout.height}`);
    edges.setAttribute('width', String(layout.width));
    edges.setAttribute('height', String(layout.height));
    layout.bands.forEach((band) => {
      const element = document.createElement('section');
      element.className = 'iteration-band';
      if (band.kind === 'architecture') element.classList.add('architecture-band');
      element.style.left = `${band.x}px`;
      element.style.top = `${band.y}px`;
      element.style.width = `${band.width}px`;
      element.style.height = `${band.height}px`;
      const title = document.createElement('b');
      title.textContent = band.title;
      const trigger = document.createElement('span');
      trigger.textContent = band.trigger || statusText(band.status);
      element.append(title, trigger);
      bands.append(element);
    });
    const definitions = svgElement('defs');
    const marker = svgElement('marker', { id: 'conversationArrow', markerWidth: 8, markerHeight: 8, refX: 7, refY: 4, orient: 'auto', markerUnits: 'strokeWidth' });
    marker.append(svgElement('path', { d: 'M 0 0 L 8 4 L 0 8 z', fill: 'currentColor' }));
    definitions.append(marker);
    edges.append(definitions);
    const stepByID = new Map(renderedSteps.map((step) => [step.id, step]));
    renderedSteps.forEach((step) => {
      const target = layout.positions.get(step.id);
      if (!target) return;
      (step.depends_on || []).forEach((parentID) => {
        const source = layout.positions.get(parentID);
        if (!source) return;
        const parent = stepByID.get(parentID);
        const crossesTurns = parent?.metadata?.iteration_id && step.metadata?.iteration_id && parent.metadata.iteration_id !== step.metadata.iteration_id;
        const entersFirstTurn = !parent?.metadata?.iteration_id && step.metadata?.iteration_id;
        let path;
        let direction = 'horizontal';
        if (loopFocus && (crossesTurns || entersFirstTurn)) {
          direction = 'vertical';
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
        edges.append(svgElement('path', { class: 'topology-edge', d: path, 'marker-end': 'url(#conversationArrow)', 'data-direction': direction, 'data-kind': step.kind === 'memory' ? 'support' : 'causal' }));
      });
      const node = document.createElement('button');
      node.type = 'button';
      node.className = 'topology-node';
      node.dataset.status = step.status;
      node.dataset.kind = step.kind;
      node.dataset.stepId = step.id;
      node.style.left = `${target.x}px`;
      node.style.top = `${target.y}px`;
      node.setAttribute('aria-pressed', 'false');
      const state = document.createElement('span');
      state.className = 'node-status';
      const copy = document.createElement('span');
      copy.className = 'node-copy';
      const kind = document.createElement('span');
      kind.className = 'node-kind';
      kind.textContent = `${step.lane || 'run'} · ${step.kind}${step.metadata?.focusable ? ' · open' : ''}`;
      const label = document.createElement('b');
      label.textContent = step.title;
      const meta = document.createElement('small');
      meta.textContent = statusText(step.status);
      copy.append(kind, label, meta);
      node.append(state, copy);
      node.addEventListener('click', () => selectStep(step, node));
      nodes.append(node);
    });
    topologyDescription.textContent = renderedSteps.map((step) => `${step.title}, ${statusText(step.status)}`).join('; ');
    const initial = renderedSteps.find((step) => step.id === selectedStepID)
      || renderedSteps.find((step) => ['active', 'running', 'failed', 'unknown', 'waiting'].includes(step.status))
      || renderedSteps.find((step) => step.kind === 'agent_loop')
      || renderedSteps.find((step) => step.kind === 'memory')
      || renderedSteps[renderedSteps.length - 1];
    const initialNode = [...nodes.children].find((node) => node.dataset.stepId === initial.id);
    selectStep(initial, initialNode);
    if (shouldCenter) centerCanvas();
    else applyCanvasPan();
  }

  function selectStep(step, node) {
    selectedStepID = step.id;
    nodes.querySelectorAll('.topology-node').forEach((item) => item.setAttribute('aria-pressed', 'false'));
    node?.setAttribute('aria-pressed', 'true');
    const heading = document.createElement('h4');
    heading.textContent = step.title;
    const description = document.createElement('p');
    description.className = 'step-description';
    description.textContent = step.description || 'Committed runtime fact.';
    const state = document.createElement('span');
    state.className = 'step-state';
    state.textContent = statusText(step.status);
    const metadata = document.createElement('dl');
    metadata.className = 'step-metadata';
    const hiddenMetadata = new Set(['loop_id', 'iteration_id', 'compound', 'focusable']);
    const labels = { turn_count: 'Turns', current_turn: 'Current turn', current_phase: 'Current phase', iteration_ordinal: 'Turn', tool_count: 'Tools', eval_attempts: 'Eval attempts', repair_count: 'Repairs', trigger: 'Trigger', folded_count: 'Folded turns', hidden_turns: 'Included turns', target: 'Target', control: 'Control level', error_code: 'Error code' };
    Object.entries(step.metadata || {}).filter(([key, value]) => !hiddenMetadata.has(key) && value !== '' && value !== null && value !== undefined).forEach(([key, value]) => {
      const term = document.createElement('dt');
      term.textContent = labels[key] || key.replaceAll('_', ' ');
      const detail = document.createElement('dd');
      detail.textContent = Array.isArray(value) ? (value.join(', ') || 'None') : String(value);
      metadata.append(term, detail);
    });
    inspector.replaceChildren(heading, description, state, metadata);
	if (step.exchange) inspector.append(renderExchange(step.exchange));
    if (step.kind === 'agent_loop' && step.metadata?.focusable) {
      const action = document.createElement('button');
      action.type = 'button';
      action.className = 'inspect-action';
      action.textContent = `Open ${step.metadata.turn_count || ''} turns`;
      action.addEventListener('click', () => {
        focusedLoopID = step.id;
        selectedTurnOrdinal = Number(step.metadata.current_turn || 1);
        selectedStepID = '';
        unfoldAllTurns = false;
        renderCanvas();
      });
      inspector.append(action);
    }
    if (step.kind === 'folded') {
      const action = document.createElement('button');
      action.type = 'button';
      action.className = 'inspect-action';
      action.textContent = 'Expand all turns';
      action.addEventListener('click', () => { unfoldAllTurns = true; selectedStepID = ''; renderCanvas(); });
      inspector.append(action);
    }
    if (step.memory) inspector.append(renderMemory(step.memory));
  }

  function renderExchange(exchange) {
    const section = document.createElement('section');
    section.className = 'step-exchange';
    const heading = document.createElement('h5');
    heading.textContent = 'Call exchange';
    const disclosure = document.createElement('p');
    disclosure.textContent = exchange.disclosure || 'Governed request and response projection.';
    section.append(heading, disclosure);
    [['Request', exchange.request], ['Response', exchange.response]].forEach(([label, value]) => {
      if (value === undefined || value === null) return;
      const details = document.createElement('details');
      const summary = document.createElement('summary');
      summary.textContent = label;
      const body = document.createElement('pre');
      body.textContent = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
      details.append(summary, body);
      section.append(details);
    });
    return section;
  }

  function renderMemory(record) {
    const section = document.createElement('section');
    section.className = 'memory-record';
    const heading = document.createElement('h5');
    heading.textContent = 'Memory used in this run';
    const summary = document.createElement('p');
    summary.className = 'memory-summary';
    summary.textContent = `${record.checked ? 'Checked' : 'Check not recorded'} · Retrieved ${record.retrieved_count} · Injected ${record.injected_count} · Applied ${record.applied_count}${record.external_sent ? ' · Sent to external model' : ' · Not sent to external model'}`;
    section.append(heading, summary);
    if (!record.items?.length) {
      const empty = document.createElement('p');
      empty.className = 'memory-summary';
      empty.textContent = 'No memory entered the model context for this run.';
      section.append(empty);
      return section;
    }
    record.items.forEach((memory) => {
      const item = document.createElement('article');
      item.className = 'memory-item';
      const statement = document.createElement('p');
      statement.textContent = memory.statement || `Governed memory · ${memory.memory_id}`;
      const stages = document.createElement('div');
      stages.className = 'memory-stage-row';
      (memory.stages || []).forEach((stage) => {
        const badge = document.createElement('span');
        badge.className = 'memory-stage';
        badge.textContent = ({ stored: 'Stored', retrieved: 'Retrieved', injected: 'Injected', applied: 'Applied', sent_to_external_model: 'Sent to external model' })[stage] || stage;
        stages.append(badge);
      });
      const belief = document.createElement('div');
      belief.className = 'memory-belief';
      belief.textContent = `${memory.belief_status || 'Unknown status'} · Confidence ${Math.round((memory.confidence || 0) * 100)}% · ${memory.sources?.length || 0} sources`;
      item.append(statement, stages, belief);
      section.append(item);
    });
    return section;
  }

  toggle.addEventListener('click', () => setOpen(workspace.dataset.open !== 'true'));
  canvasBack.addEventListener('click', () => { focusedLoopID = ''; selectedTurnOrdinal = 0; selectedStepID = ''; unfoldAllTurns = false; renderCanvas(); });
  canvasReset.addEventListener('click', centerCanvas);
  canvasViewport.tabIndex = 0;
  canvasViewport.addEventListener('pointerdown', (event) => {
    if (event.button !== 0 || event.target.closest('button')) return;
    panGesture = { id: event.pointerId, startX: event.clientX, startY: event.clientY, originX: canvasPan.x, originY: canvasPan.y };
    canvasViewport.dataset.panning = 'true';
    canvasViewport.setPointerCapture(event.pointerId);
  });
  canvasViewport.addEventListener('pointermove', (event) => {
    if (!panGesture || panGesture.id !== event.pointerId) return;
    canvasPan.x = panGesture.originX + event.clientX - panGesture.startX;
    canvasPan.y = panGesture.originY + event.clientY - panGesture.startY;
    applyCanvasPan();
  });
  const endPan = (event) => {
    if (!panGesture || panGesture.id !== event.pointerId) return;
    panGesture = undefined;
    delete canvasViewport.dataset.panning;
    if (canvasViewport.hasPointerCapture(event.pointerId)) canvasViewport.releasePointerCapture(event.pointerId);
  };
  canvasViewport.addEventListener('pointerup', endPan);
  canvasViewport.addEventListener('pointercancel', endPan);
  canvasViewport.addEventListener('wheel', (event) => {
    event.preventDefault();
    canvasPan.x -= event.shiftKey && !event.deltaX ? event.deltaY : event.deltaX;
    canvasPan.y -= event.shiftKey ? 0 : event.deltaY;
    applyCanvasPan();
  }, { passive: false });
  canvasViewport.addEventListener('keydown', (event) => {
    const movement = { ArrowLeft: [28, 0], ArrowRight: [-28, 0], ArrowUp: [0, 28], ArrowDown: [0, -28] }[event.key];
    if (!movement) return;
    event.preventDefault();
    canvasPan.x += movement[0];
    canvasPan.y += movement[1];
    applyCanvasPan();
  });
  close.addEventListener('click', () => setOpen(false));
  scrim.addEventListener('click', () => setOpen(false));
  new MutationObserver(augmentMessages).observe(timeline, { childList: true });
  augmentMessages();
  refreshActivity();
  setInterval(refreshActivity, 3500);
  let compactCanvas = window.matchMedia('(max-width: 600px)').matches;
  window.addEventListener('resize', () => {
    const nextCompactCanvas = window.matchMedia('(max-width: 600px)').matches;
    if (nextCompactCanvas !== compactCanvas) {
      compactCanvas = nextCompactCanvas;
      renderCanvas();
    }
  });
})();
