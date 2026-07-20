(() => {
  const metrics = document.querySelector('#memoryMetrics');
  const list = document.querySelector('#memoryList');
  const detail = document.querySelector('#memoryDetail');
  const search = document.querySelector('#memorySearch');
  let observations = [];
  let selectedID = '';

  function stageText(stage) {
    return ({ stored: 'Stored', retrieved: 'Retrieved', injected: 'Injected', applied: 'Applied', sent_to_external_model: 'Sent to external model' })[stage] || stage;
  }

  function formatDate(value) {
    if (!value) return '—';
    return new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(value));
  }

  async function loadMemory() {
    try {
      const response = await fetch('/api/v1/memory?limit=200', { credentials: 'same-origin' });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const payload = await response.json();
      observations = payload.observations || [];
      renderMetrics(payload);
      renderList();
      if (!selectedID && observations.length) selectMemory(observations[0].memory_id);
    } catch (_) {
      list.replaceChildren();
      const unavailable = document.createElement('p');
      unavailable.className = 'memory-list-empty';
      unavailable.textContent = 'Memory projection is unavailable.';
      list.append(unavailable);
    }
  }

  function renderMetrics(payload) {
    metrics.replaceChildren();
    [
      ['Stored', payload.total || 0],
      ['Active', payload.active || 0],
      ['Contested', payload.contested || 0],
      ['Expired', payload.expired || 0],
      ['Do not use', payload.do_not_use || 0]
    ].forEach(([label, value]) => {
      const item = document.createElement('div');
      item.className = 'memory-metric';
      const name = document.createElement('span');
      name.textContent = label;
      const number = document.createElement('b');
      number.textContent = String(value);
      item.append(name, number);
      metrics.append(item);
    });
  }

  function filtered() {
    const query = search.value.trim().toLocaleLowerCase();
    if (!query) return observations;
    return observations.filter((memory) => [memory.statement, memory.kind, memory.scope, memory.belief_status, memory.lifecycle_status, memory.usage_policy].some((value) => String(value || '').toLocaleLowerCase().includes(query)));
  }

  function renderList() {
    list.replaceChildren();
    const items = filtered();
    if (!items.length) {
      const empty = document.createElement('p');
      empty.className = 'memory-list-empty';
      empty.textContent = observations.length ? 'No matching memory.' : 'No stored memory yet.';
      list.append(empty);
      return;
    }
    items.forEach((memory) => {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'memory-row';
      button.setAttribute('aria-current', String(memory.memory_id === selectedID));
      const dot = document.createElement('span');
      dot.className = 'memory-belief-dot';
      dot.dataset.status = memory.belief_status;
      dot.dataset.expired = String(Boolean(memory.expired));
      const copy = document.createElement('span');
      copy.className = 'memory-row-copy';
      const statement = document.createElement('b');
      statement.textContent = memory.statement || `Governed memory · ${memory.memory_id}`;
      const meta = document.createElement('small');
      meta.textContent = `${memory.kind || 'Unclassified'}${memory.scope ? ` · ${memory.scope}` : ''} · ${memory.belief_status || 'unknown'}`;
      copy.append(statement, meta);
      const updated = document.createElement('small');
      updated.textContent = formatDate(memory.updated_at);
      button.append(dot, copy, updated);
      button.addEventListener('click', () => selectMemory(memory.memory_id));
      list.append(button);
    });
  }

  function selectMemory(id) {
    selectedID = id;
    renderList();
    const memory = observations.find((item) => item.memory_id === id);
    if (!memory) return;
    detail.replaceChildren();
    const heading = document.createElement('h3');
    heading.textContent = memory.statement || `Governed memory · ${memory.memory_id}`;
    const stages = document.createElement('div');
    stages.className = 'memory-stage-strip';
    (memory.stages || ['stored']).forEach((stage) => {
      const badge = document.createElement('span');
      badge.className = 'memory-stage-badge';
      badge.dataset.stage = stage;
      badge.textContent = stageText(stage);
      stages.append(badge);
    });
    const facts = document.createElement('dl');
    [
      ['Memory ID', memory.memory_id], ['Claim ID', memory.claim_id], ['Type / scope', `${memory.kind || '—'} / ${memory.scope || '—'}`],
      ['Belief', memory.belief_status || '—'], ['Confidence', `${Math.round((memory.confidence || 0) * 100)}%`],
      ['Support / contradiction', `${(memory.support_weight || 0).toFixed(2)} / ${(memory.contradict_weight || 0).toFixed(2)}`],
      ['Independent source groups', memory.independent_groups ?? 0], ['Usage policy', memory.usage_policy || '—'], ['Lifecycle', memory.lifecycle_status || '—'],
      ['Access count', memory.access_count ?? 0], ['Last retrieved', formatDate(memory.last_accessed_at)], ['Last updated', formatDate(memory.updated_at)]
    ].forEach(([label, value]) => {
      const term = document.createElement('dt');
      term.textContent = label;
      const description = document.createElement('dd');
      description.textContent = String(value);
      facts.append(term, description);
    });
    const sources = document.createElement('section');
    sources.className = 'memory-sources';
    const sourceHeading = document.createElement('h4');
    sourceHeading.textContent = `Evidence sources · ${memory.sources?.length || 0}`;
    sources.append(sourceHeading);
    (memory.sources || []).forEach((source) => {
      const row = document.createElement('div');
      row.className = 'memory-source';
      const relation = document.createElement('span');
      relation.className = 'memory-relation';
      relation.textContent = source.relation;
      const copy = document.createElement('span');
      copy.className = 'memory-source-copy';
      const kind = document.createElement('b');
      kind.textContent = `${source.source_type || 'source'} · ${source.independence_group || 'ungrouped'}`;
      const ref = document.createElement('small');
      ref.textContent = source.source_ref || source.evidence_id;
      copy.append(kind, ref);
      const reliability = document.createElement('small');
      reliability.textContent = `${Math.round((source.reliability || 0) * 100)}%`;
      row.append(relation, copy, reliability);
      sources.append(row);
    });
    detail.append(heading, stages, facts, sources);
  }

  function renderRunMemory(record, target) {
    if (!record || !target) return;
    const section = document.createElement('section');
    section.className = 'run-memory-record';
    const heading = document.createElement('h5');
    heading.textContent = 'Memory used in this run';
    const summary = document.createElement('p');
    summary.textContent = `Retrieved ${record.retrieved_count} · Injected ${record.injected_count} · Applied ${record.applied_count}${record.external_sent ? ' · Sent to external model' : ''}`;
    section.append(heading, summary);
    (record.items || []).forEach((memory) => {
      const item = document.createElement('div');
      item.className = 'run-memory-item';
      const statement = document.createElement('b');
      statement.textContent = memory.statement || memory.memory_id;
      item.append(statement);
      (memory.stages || []).forEach((stage) => {
        const badge = document.createElement('span');
        badge.textContent = stageText(stage);
        item.append(badge);
      });
      section.append(item);
    });
    target.append(section);
  }

  search.addEventListener('input', renderList);
  document.addEventListener('eri:observatory-step-selected', (event) => renderRunMemory(event.detail?.step?.memory, event.detail?.target));
  loadMemory();
  setInterval(loadMemory, 10000);
})();
