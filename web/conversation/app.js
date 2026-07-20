(() => {
  const timeline = document.querySelector('#timeline');
  const emptyState = document.querySelector('#emptyState');
  const composer = document.querySelector('#composer');
  const input = document.querySelector('#messageInput');
  const sendButton = document.querySelector('#sendButton');
  const fileInput = document.querySelector('#fileInput');
  const attachmentButton = document.querySelector('#attachmentButton');
  const attachmentTray = document.querySelector('#attachmentTray');
  const presence = document.querySelector('#presence');
  const notice = document.querySelector('#connectionNotice');
  const searchTrigger = document.querySelector('#searchTrigger');
  const searchPanel = document.querySelector('#searchPanel');
  const searchClose = document.querySelector('#searchClose');
  const searchInput = document.querySelector('#searchInput');
  const searchResults = document.querySelector('#searchResults');
  const rendered = new Map();
  let lastSequence = 0;
  let oldestSequence = 0;
  let loadingOlder = false;
  let hasOlder = true;
  let stream;
  let selectedFiles = [];
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

  function formatTime(value) {
    if (!value) return '—';
    return new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' }).format(new Date(value));
  }

  function formatDateTime(value) {
    if (!value) return '—';
    return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(value));
  }

  function formatBytes(value) {
    if (value < 1024) return `${value} B`;
    if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MiB`;
  }

  function appendInlineMarkdown(target, source) {
    const tokenPattern = /(\*\*([^*]+)\*\*|`([^`]+)`|\[([^\]]+)\]\(([^)\s]+)\)|\*([^*]+)\*)/g;
    let cursor = 0;
    for (const match of source.matchAll(tokenPattern)) {
      if (match.index > cursor) target.append(document.createTextNode(source.slice(cursor, match.index)));
      if (match[2]) {
        const strong = document.createElement('strong');
        strong.textContent = match[2];
        target.append(strong);
      } else if (match[3]) {
        const code = document.createElement('code');
        code.textContent = match[3];
        target.append(code);
      } else if (match[4]) {
        let safeURL;
        try {
          const parsed = new URL(match[5], window.location.origin);
          if (['http:', 'https:', 'mailto:'].includes(parsed.protocol)) safeURL = parsed.href;
        } catch (_) { /* leave unsafe or invalid links as text */ }
        if (safeURL) {
          const link = document.createElement('a');
          link.href = safeURL;
          link.textContent = match[4];
          if (safeURL.startsWith('http')) {
            link.target = '_blank';
            link.rel = 'noreferrer noopener';
          }
          target.append(link);
        } else {
          target.append(document.createTextNode(match[0]));
        }
      } else {
        const emphasis = document.createElement('em');
        emphasis.textContent = match[6];
        target.append(emphasis);
      }
      cursor = match.index + match[0].length;
    }
    if (cursor < source.length) target.append(document.createTextNode(source.slice(cursor)));
  }

  function renderMarkdown(source) {
    const container = document.createElement('div');
    container.className = 'message-body markdown-body';
    const lines = source.replace(/\r\n?/g, '\n').split('\n');
    const startsBlock = (line) => /^\s*(```|#{1,4}\s|>\s?|[-*+]\s+|\d+[.)]\s+|---+\s*$)/.test(line);
    for (let index = 0; index < lines.length;) {
      const line = lines[index];
      if (!line.trim()) { index += 1; continue; }
      const fence = line.match(/^\s*```([^`]*)$/);
      if (fence) {
        const codeLines = [];
        index += 1;
        while (index < lines.length && !/^\s*```\s*$/.test(lines[index])) codeLines.push(lines[index++]);
        if (index < lines.length) index += 1;
        const pre = document.createElement('pre');
        const code = document.createElement('code');
        if (fence[1].trim()) code.dataset.language = fence[1].trim();
        code.textContent = codeLines.join('\n');
        pre.append(code);
        container.append(pre);
        continue;
      }
      const heading = line.match(/^\s*(#{1,4})\s+(.+)$/);
      if (heading) {
        const element = document.createElement(`h${Math.min(4, heading[1].length + 1)}`);
        appendInlineMarkdown(element, heading[2]);
        container.append(element);
        index += 1;
        continue;
      }
      if (/^\s*>/.test(line)) {
        const quote = document.createElement('blockquote');
        while (index < lines.length && /^\s*>/.test(lines[index])) {
          if (quote.childNodes.length) quote.append(document.createElement('br'));
          appendInlineMarkdown(quote, lines[index].replace(/^\s*>\s?/, ''));
          index += 1;
        }
        container.append(quote);
        continue;
      }
      const listMatch = line.match(/^\s*([-*+]|\d+[.)])\s+(.+)$/);
      if (listMatch) {
        const ordered = /^\d/.test(listMatch[1]);
        const list = document.createElement(ordered ? 'ol' : 'ul');
        while (index < lines.length) {
          const itemMatch = lines[index].match(/^\s*([-*+]|\d+[.)])\s+(.+)$/);
          if (!itemMatch || /^\d/.test(itemMatch[1]) !== ordered) break;
          const item = document.createElement('li');
          appendInlineMarkdown(item, itemMatch[2]);
          list.append(item);
          index += 1;
        }
        container.append(list);
        continue;
      }
      if (/^\s*---+\s*$/.test(line)) {
        container.append(document.createElement('hr'));
        index += 1;
        continue;
      }
      const paragraphLines = [line.trim()];
      index += 1;
      while (index < lines.length && lines[index].trim() && !startsBlock(lines[index])) paragraphLines.push(lines[index++].trim());
      const paragraph = document.createElement('p');
      appendInlineMarkdown(paragraph, paragraphLines.join(' '));
      container.append(paragraph);
    }
    return container;
  }


  function appendMessage(message, placement = 'append') {
    if (rendered.has(message.id)) return;
    const article = document.createElement('article');
    article.className = 'message';
    article.dataset.direction = message.direction;
    article.dataset.role = message.role;
    article.dataset.kind = message.kind;
    article.dataset.sequence = String(message.sequence);
    if (message.task_id) article.dataset.taskId = message.task_id;
    article.id = `message-${message.sequence}`;

    const bubble = document.createElement('div');
    bubble.className = 'bubble';
    if (message.content) {
      if (message.role === 'assistant') {
        bubble.append(renderMarkdown(message.content));
      } else {
        const body = document.createElement('p');
        body.className = 'message-body';
        body.textContent = message.content;
        bubble.append(body);
      }
    }

    if (message.kind === 'runtime_error') {
      const errorCard = document.createElement('div');
      errorCard.className = 'runtime-error';
      const title = document.createElement('b');
      title.textContent = 'Task not completed';
      const detail = document.createElement('code');
      detail.textContent = message.data?.code || 'runtime_error';
      errorCard.append(title, detail);
      bubble.append(errorCard);
    }

    if (message.attachments?.length) {
      const attachments = document.createElement('div');
      attachments.className = 'message-attachments';
      message.attachments.forEach((attachment) => {
        const link = document.createElement('a');
        link.href = `/api/v1/attachments/${encodeURIComponent(attachment.id)}`;
        link.download = attachment.name;
        const name = document.createElement('b');
        name.textContent = attachment.name;
        const meta = document.createElement('small');
        meta.textContent = `${attachment.media_type} · ${formatBytes(attachment.size_bytes)}`;
        link.append(name, meta);
        attachments.append(link);
      });
      bubble.append(attachments);
    }

    if (message.kind === 'approval_request' && message.data?.approval_id) {
      bubble.append(buildDecision(message));
    }

    const footer = document.createElement('div');
    footer.className = 'message-footer';
    const meta = document.createElement('time');
    meta.className = 'message-meta';
    meta.dateTime = message.created_at;
    meta.textContent = formatTime(message.created_at);
    footer.append(meta);
    article.append(bubble, footer);
    const firstMessage = timeline.querySelector('.message');
    if (placement === 'prepend' && firstMessage) timeline.insertBefore(article, firstMessage);
    else timeline.append(article);
    rendered.set(message.id, article);
    lastSequence = Math.max(lastSequence, message.sequence);
    oldestSequence = oldestSequence === 0 ? message.sequence : Math.min(oldestSequence, message.sequence);
    emptyState.hidden = true;
  }

  function buildDecision(message) {
    const wrapper = document.createDocumentFragment();
    const details = document.createElement('details');
    details.className = 'decision-details';
    const detailsLabel = document.createElement('summary');
    detailsLabel.textContent = 'View exact operation';
    const operation = document.createElement('pre');
    operation.textContent = JSON.stringify({
      tool: message.data.tool_id,
      effect: message.data.effect,
      target: message.data.target,
      parameters: message.data.parameters
    }, null, 2);
    details.append(detailsLabel, operation);

    const decision = document.createElement('div');
    decision.className = 'decision-actions';
    const status = document.createElement('small');
    const initialStatus = message.data.status || 'pending';
    status.textContent = ({ approved: 'Approved', denied: 'Denied', expired: 'Expired' })[initialStatus]
      || (message.data.control === 'strong_approval' ? 'Explicit approval required' : 'Waiting for confirmation');
    const approve = document.createElement('button');
    approve.type = 'button';
    approve.textContent = message.data.control === 'strong_approval' ? 'Approve explicitly' : 'Approve';
    const deny = document.createElement('button');
    deny.type = 'button';
    deny.className = 'decision-deny';
    deny.textContent = 'Deny';
    if (initialStatus !== 'pending') {
      approve.disabled = true;
      deny.disabled = true;
    }
    const decide = async (value) => {
      approve.disabled = true;
      deny.disabled = true;
      status.textContent = 'Recording…';
      try {
        const result = await request(`/api/v1/approvals/${encodeURIComponent(message.data.approval_id)}`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ decision: value })
        });
        status.textContent = result.status === 'approved' ? 'Approved. Eri will continue.' : 'Denied';
        await refreshPresence();
      } catch (error) {
        approve.disabled = false;
        deny.disabled = false;
        status.textContent = error.message;
      }
    };
    approve.addEventListener('click', () => decide('approve'));
    deny.addEventListener('click', () => decide('deny'));
    decision.append(status, approve, deny);
    wrapper.append(details, decision);
    return wrapper;
  }

  function addFiles(files) {
    for (const file of files) {
      if (!file.size || file.size > 10 * 1024 * 1024 || selectedFiles.length >= 10) continue;
      const duplicate = selectedFiles.some((item) => item.name === file.name && item.size === file.size && item.lastModified === file.lastModified);
      if (!duplicate) selectedFiles.push(file);
    }
    renderSelectedFiles();
  }

  function renderSelectedFiles() {
    attachmentTray.replaceChildren();
    attachmentTray.hidden = selectedFiles.length === 0;
    selectedFiles.forEach((file, index) => {
      const item = document.createElement('span');
      const label = document.createElement('span');
      label.textContent = `${file.name} · ${formatBytes(file.size)}`;
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.setAttribute('aria-label', `Remove ${file.name}`);
      remove.textContent = '×';
      remove.addEventListener('click', () => {
        selectedFiles.splice(index, 1);
        renderSelectedFiles();
      });
      item.append(label, remove);
      attachmentTray.append(item);
    });
  }

  async function loadMessages(initial = false) {
    const shouldStick = timeline.scrollHeight - timeline.scrollTop - timeline.clientHeight < 140 || rendered.size === 0;
    const cursor = initial ? 'before=0' : `after=${lastSequence}`;
    const payload = await request(`/api/v1/messages?${cursor}&limit=200`);
    payload.messages.forEach(appendMessage);
    if (payload.messages.length && shouldStick) timeline.scrollTop = timeline.scrollHeight;
  }

  async function connectConversation() {
    const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
    return request('/api/v1/conversation/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ locale: navigator.language || '', timezone })
    });
  }

  async function waitForIntroduction(taskID) {
    while (taskID) {
      const task = await request(`/api/v1/tasks/${encodeURIComponent(taskID)}`);
      if (['completed', 'failed', 'canceled'].includes(task.status)) return task.status;
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    return 'completed';
  }

  async function loadOlderMessages() {
    if (loadingOlder || !hasOlder || oldestSequence <= 1) return;
    loadingOlder = true;
    const previousHeight = timeline.scrollHeight;
    try {
      const payload = await request(`/api/v1/messages?before=${oldestSequence}&limit=200`);
      hasOlder = payload.messages.length === 200;
      [...payload.messages].reverse().forEach((message) => appendMessage(message, 'prepend'));
      timeline.scrollTop += timeline.scrollHeight - previousHeight;
    } finally {
      loadingOlder = false;
    }
  }

  async function refreshPresence() {
    try {
      const state = await request('/api/v1/presence');
      presence.dataset.state = state.state;
      presence.querySelector('b').textContent = state.state === 'working' ? 'Working' : 'Online';
      notice.hidden = true;
    } catch (_) {
      presence.dataset.state = 'offline';
      presence.querySelector('b').textContent = 'Offline';
    }
  }

  function scheduleRefresh() {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => {
      Promise.allSettled([loadMessages(), refreshPresence()]);
    }, 160);
  }

  function connectEvents() {
    if (stream) stream.close();
    stream = new EventSource('/api/v1/events');
    stream.addEventListener('eri', scheduleRefresh);
    stream.onopen = () => { notice.hidden = true; };
    stream.onerror = () => {
      presence.dataset.state = 'offline';
      presence.querySelector('b').textContent = 'Offline';
    };
  }

  composer.addEventListener('submit', async (event) => {
    event.preventDefault();
    const text = input.value.trim();
    if ((!text && selectedFiles.length === 0) || sendButton.disabled) return;
    sendButton.disabled = true;
    try {
      const body = new FormData();
      body.append('text', text);
      selectedFiles.forEach((file) => body.append('files', file, file.name));
      await request('/api/v1/messages', { method: 'POST', body });
      input.value = '';
      input.style.height = '';
      selectedFiles = [];
      fileInput.value = '';
      renderSelectedFiles();
      await Promise.all([loadMessages(), refreshPresence()]);
    } catch (error) {
      notice.textContent = error.message || 'Your message was not sent.';
      notice.hidden = false;
    } finally {
      sendButton.disabled = false;
      input.focus();
    }
  });

  timeline.addEventListener('scroll', () => {
    if (timeline.scrollTop < 80) loadOlderMessages().catch(() => {});
  });

  attachmentButton.addEventListener('click', () => fileInput.click());
  fileInput.addEventListener('change', () => addFiles(fileInput.files));
  input.addEventListener('paste', (event) => { if (event.clipboardData?.files?.length) addFiles(event.clipboardData.files); });
  timeline.addEventListener('dragover', (event) => {
    if (event.dataTransfer?.types?.includes('Files')) {
      event.preventDefault();
      timeline.dataset.drop = 'true';
    }
  });
  timeline.addEventListener('dragleave', () => { delete timeline.dataset.drop; });
  timeline.addEventListener('drop', (event) => {
    event.preventDefault();
    delete timeline.dataset.drop;
    if (event.dataTransfer?.files?.length) addFiles(event.dataTransfer.files);
  });
  input.addEventListener('input', () => {
    input.style.height = 'auto';
    input.style.height = `${Math.min(input.scrollHeight, 196)}px`;
  });
  input.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
      event.preventDefault();
      composer.requestSubmit();
    }
  });

  searchTrigger.addEventListener('click', () => {
    const opening = searchPanel.hidden;
    searchPanel.hidden = !opening;
    searchTrigger.setAttribute('aria-expanded', String(opening));
    if (opening) searchInput.focus();
  });
  searchClose.addEventListener('click', () => {
    searchPanel.hidden = true;
    searchTrigger.setAttribute('aria-expanded', 'false');
    searchTrigger.focus();
  });
  searchPanel.addEventListener('submit', async (event) => {
    event.preventDefault();
    searchResults.textContent = 'Searching…';
    try {
      const payload = await request(`/api/v1/search?q=${encodeURIComponent(searchInput.value)}&limit=30`);
      searchResults.replaceChildren();
      if (!payload.messages.length) {
        searchResults.textContent = 'No results found.';
        return;
      }
      payload.messages.forEach((message) => {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'search-result';
        const excerpt = document.createElement('span');
        excerpt.textContent = message.content.length > 90 ? `${message.content.slice(0, 90)}…` : message.content;
        const meta = document.createElement('small');
        meta.textContent = `${message.role === 'user' ? 'You' : 'Eri'} · ${formatDateTime(message.created_at)}`;
        button.append(excerpt, meta);
        button.addEventListener('click', () => {
          searchPanel.hidden = true;
          searchTrigger.setAttribute('aria-expanded', 'false');
          const target = document.querySelector(`#message-${message.sequence}`);
          if (target) target.scrollIntoView({ block: 'center', behavior: 'smooth' });
        });
        searchResults.append(button);
      });
    } catch (error) {
      searchResults.textContent = error.message;
    }
  });

  async function initializeConversation() {
    sendButton.disabled = true;
    let connected = false;
    try {
      const connection = await connectConversation();
      connectEvents();
      await Promise.all([loadMessages(true), refreshPresence()]);
      if (connection.introduction_started) {
        await waitForIntroduction(connection.task_id);
        await loadMessages();
      }
      connected = true;
    } catch (error) {
      await refreshPresence();
      notice.textContent = error.message || 'Eri could not establish the conversation.';
      notice.hidden = false;
    } finally {
      sendButton.disabled = !connected;
    }
  }

  initializeConversation();
  setInterval(refreshPresence, 3000);
})();
