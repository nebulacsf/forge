'use strict';

/* ── State ───────────────────────────────────────────────────────── */

const $ = (id) => document.getElementById(id);

const state = {
  config: null,
  configPath: '',
  logPath: '',
  debugActive: false,
  apiKeySet: false,
  models: new Map(),
  modelsSource: '',
  projects: [],
  activeProjectId: null,
  activeProject: null,
  sessions: [],
  activeSessionId: null,
  session: null,
  checklist: [],
  files: new Map(),
  usage: {
    sessionIn: 0,
    sessionOut: 0,
    sessionCost: 0,
    projectCost: 0,
    contextTokens: 0,
    contextExact: false,
    cached: 0,
  },
  streaming: false,
  abort: null,
  currentRound: null,
  toolCards: new Map(),
  sawCancelled: false,
  turnHadContent: false,
  turnHadError: false,
  turnReasoningOnly: false,
  turnRound: 0,
  turnLimit: 0,
  turnSegment: 1,
  turnSegmentSize: 0,
  messageIndex: -1,
  sessionTimeBase: 0,
  turnBaseTime: 0,
  turnStartedAt: 0,
  turnElapsedMs: 0,
  turnClock: null,
  turnTimerEl: null,
};

let projectModal = null;
let settingsModal = null;
let contentTimer = null;
let checklistDoneOpen = false;

const PALETTE = ['#6c8cff', '#22c55e', '#f59e0b', '#ec4899', '#06b6d4', '#a855f7', '#ef4444', '#14b8a6'];

/* ── Bootstrap ───────────────────────────────────────────────────── */

document.addEventListener('DOMContentLoaded', init);

async function init() {
  if (window.marked) {
    marked.setOptions({ gfm: true, breaks: false });
  }
  applyTheme(localStorage.getItem('forge.theme') || 'dark', { persist: false });
  initFilesCard();
  setPanelHidden(localStorage.getItem('forge.panelHidden') === '1', { persist: false });
  bindEvents();
  if (window.bootstrap) {
    projectModal = new bootstrap.Modal($('projectModal'));
    settingsModal = new bootstrap.Modal($('settingsModal'));
  }
  try {
    await loadConfig();
    $('modelInput').value = state.config?.defaults?.model || '';
    loadModels();
    await loadProjects();
    if (!state.projects.length) {
      showEmpty('project');
      updateControls();
      return;
    }
    const saved = Number(localStorage.getItem('forge.project') || 0);
    const project = state.projects.find((p) => p.id === saved) || state.projects[0];
    await selectProject(project.id, { autoCreate: true });
  } catch (err) {
    toast(err.message, 'error');
    showEmpty('error');
    updateControls();
  }
}

async function api(path, opts = {}) {
  const headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  const res = await fetch(path, { ...opts, headers });
  if (res.status === 204) return null;
  let data = null;
  try {
    data = await res.json();
  } catch {
    data = null;
  }
  if (!res.ok) {
    const msg = (data && data.error) || `Request failed (${res.status})`;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return data;
}

async function loadConfig() {
  const data = await api('/api/config');
  state.config = data.config;
  state.configPath = data.path;
  state.logPath = data.log_path || '';
  state.debugActive = !!data.debug;
  state.apiKeySet = !!data.api_key_set;
}

async function loadModels() {
  try {
    const data = await api('/api/models');
    const list = Array.isArray(data.models) ? data.models : [];
    state.models = new Map(list.map((m) => [String(m.id), m]));
    state.modelsSource = data.source || '';
    renderModelOptions(list);
    updateUsageUI();
  } catch {
    /* Keep the built-in heuristics when the catalog is unavailable. */
  }
}

function renderModelOptions(list) {
  const datalist = $('modelOptions');
  if (!datalist) return;
  datalist.innerHTML = '';
  const seen = new Set();
  for (const model of list) {
    const id = String(model.id || '');
    if (!id || seen.has(id)) continue;
    seen.add(id);
    const option = document.createElement('option');
    option.value = id;
    const name = model.name && model.name !== id ? `${model.name} · ` : '';
    const ctx = model.context_length > 0 ? ` (${formatLimit(model.context_length)} ctx)` : '';
    option.label = `${name}${id}${ctx}`;
    datalist.appendChild(option);
  }
}

async function loadProjects() {
  const data = await api('/api/projects');
  state.projects = data.projects || [];
  renderProjects();
}

/* ── Projects ────────────────────────────────────────────────────── */

function renderProjects() {
  const list = $('projectList');
  list.innerHTML = '';
  for (const project of state.projects) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'project-btn' + (project.id === state.activeProjectId ? ' active' : '');
    btn.style.setProperty('--pc', project.icon_color || '#6c8cff');
    btn.textContent = initials(project.name);
    btn.title = `${project.name}\n${project.path}`;
    btn.setAttribute('aria-label', project.name);
    btn.addEventListener('click', () => {
      if (project.id !== state.activeProjectId) selectProject(project.id, { autoCreate: true });
    });
    list.appendChild(btn);
  }
}

function initials(name) {
  const words = String(name || '?').trim().split(/[\s._/\\-]+/).filter(Boolean);
  if (!words.length) return '?';
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase();
  return (words[0][0] + words[1][0]).toUpperCase();
}

function randomPaletteColor() {
  return PALETTE[Math.floor(Math.random() * PALETTE.length)];
}

/* ── Theme, panel, collapsible cards ─────────────────────────────── */

function applyTheme(theme, { persist = true } = {}) {
  const next = theme === 'light' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  document.documentElement.setAttribute('data-bs-theme', next);
  const dark = $('hljsDark');
  const light = $('hljsLight');
  if (dark) dark.disabled = next !== 'dark';
  if (light) light.disabled = next !== 'light';
  const btn = $('themeBtn');
  if (btn) {
    const icon = btn.querySelector('i');
    if (icon) icon.className = next === 'dark' ? 'bi bi-sun' : 'bi bi-moon-stars';
    btn.title = next === 'dark' ? 'Switch to light theme' : 'Switch to dark theme';
    btn.setAttribute('aria-label', btn.title);
  }
  if (persist) {
    try { localStorage.setItem('forge.theme', next); } catch { /* ignore */ }
  }
}

function toggleTheme() {
  const current = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
  applyTheme(current === 'dark' ? 'light' : 'dark');
}

function setPanelHidden(hidden, { persist = true } = {}) {
  const panel = $('contextPanel');
  const show = $('showPanelBtn');
  if (panel) panel.classList.toggle('panel-hidden', hidden);
  if (show) show.classList.toggle('d-none', !hidden);
  if (persist) {
    try { localStorage.setItem('forge.panelHidden', hidden ? '1' : '0'); } catch { /* ignore */ }
  }
}

function initFilesCard() {
  const card = $('filesCard');
  if (!card) return;
  let open = false;
  try { open = localStorage.getItem('forge.filesOpen') === '1'; } catch { /* ignore */ }
  card.open = open;
  card.addEventListener('toggle', () => {
    try { localStorage.setItem('forge.filesOpen', card.open ? '1' : '0'); } catch { /* ignore */ }
  });
}

/* ── Sessions ────────────────────────────────────────────────────── */

async function selectProject(id, { autoCreate = false } = {}) {
  if (!id) return;
  if (state.streaming) stopStream();
  const project = state.projects.find((p) => p.id === id);
  if (!project) return;
  state.activeProjectId = id;
  state.activeProject = project;
  localStorage.setItem('forge.project', String(id));
  renderProjects();
  $('projectPathLabel').textContent = project.path;
  $('projectPathLabel').title = project.path;

  try {
    const data = await api(`/api/projects/${id}/sessions`);
    state.sessions = data.sessions || [];
  } catch (err) {
    toast(err.message, 'error');
    state.sessions = [];
  }
  renderTabs();

  const savedSession = Number(localStorage.getItem(`forge.session.${id}`) || 0);
  let target = state.sessions.find((s) => s.id === savedSession) || state.sessions[0];
  if (target) {
    await loadSession(target.id);
  } else if (autoCreate) {
    await createSession({ silent: true });
  } else {
    clearSession();
  }
}

async function createSession({ silent = false } = {}) {
  if (!state.activeProjectId) {
    toast('Register or select a project first', 'error');
    return null;
  }
  if (state.streaming) stopStream();
  try {
    const data = await api(`/api/projects/${state.activeProjectId}/sessions`, {
      method: 'POST',
      body: JSON.stringify({}),
    });
    state.sessions.unshift(data.session);
    renderTabs();
    await loadSession(data.session.id);
    if (!silent) toast('New chat started', 'success');
    return data.session;
  } catch (err) {
    toast(err.message, 'error');
    return null;
  }
}

async function loadSession(id) {
  try {
    const data = await api(`/api/sessions/${id}`);
    state.activeSessionId = id;
    state.session = data.session;
    state.activeProject = data.project;
    state.activeProjectId = data.project.id;
    state.sessions = state.sessions.filter((s) => s.id !== id);
    state.sessions.unshift(data.session);
    state.checklist = data.checklist || [];
    state.files = new Map();
    state.toolCards = new Map();
    state.currentRound = null;
    state.sawCancelled = false;
    state.sessionTimeBase = Number(data.session.duration_ms || 0);
    state.turnBaseTime = state.sessionTimeBase;
    state.turnStartedAt = 0;
    state.turnElapsedMs = 0;
    state.turnTimerEl = null;
    stopTurnClock(false);
    state.usage = {
      sessionIn: data.session.tokens_in || 0,
      sessionOut: data.session.tokens_out || 0,
      sessionCost: data.session.cost_usd || 0,
      projectCost: data.project.total_cost_usd || 0,
      contextTokens: estimateContext(data.messages || []),
      contextExact: false,
      cached: data.session.cached_tokens || 0,
    };
    localStorage.setItem(`forge.session.${data.project.id}`, String(id));
    updateProjectTotals(data.project);
    renderTabs();
    renderSession(data.messages || []);
    maybeScroll(true);
  } catch (err) {
    toast(err.message, 'error');
    if (err.status === 404) {
      state.sessions = state.sessions.filter((s) => s.id !== id);
      renderTabs();
      clearSession();
    }
  }
}

function clearSession() {
  finalizeRound();
  state.activeSessionId = null;
  state.session = null;
  state.checklist = [];
  state.files = new Map();
  state.toolCards = new Map();
  state.usage = {
    sessionIn: 0, sessionOut: 0, sessionCost: 0, projectCost: 0,
    contextTokens: 0, contextExact: false, cached: 0,
  };
  stopTurnClock(false);
  state.sessionTimeBase = 0;
  state.turnBaseTime = 0;
  state.turnStartedAt = 0;
  state.turnElapsedMs = 0;
  state.turnTimerEl = null;
  if (state.activeProjectId) localStorage.removeItem(`forge.session.${state.activeProjectId}`);
  $('messages').innerHTML = '';
  $('modelInput').value = state.config?.defaults?.model || '';
  setModeUI(state.config?.defaults?.mode || 'PLAN');
  renderTabs();
  renderFiles();
  renderChecklist();
  updateUsageUI();
  updateControls();
  showEmpty('session');
  document.title = 'Forge';
}

function renderTabs() {
  const bar = $('sessionTabs');
  bar.innerHTML = '';
  if (!state.sessions.length) {
    const hint = document.createElement('span');
    hint.className = 'text-secondary';
    hint.style.fontSize = '11px';
    hint.textContent = state.activeProjectId ? 'No open chats' : 'No project selected';
    bar.appendChild(hint);
    return;
  }
  for (const session of state.sessions) {
    const tab = document.createElement('div');
    tab.className = 'session-tab' + (session.id === state.activeSessionId ? ' active' : '');
    tab.title = session.title;
    tab.addEventListener('click', () => {
      if (session.id !== state.activeSessionId) {
        if (state.streaming) stopStream();
        loadSession(session.id);
      }
    });

    const title = document.createElement('span');
    title.className = 'tab-title';
    title.textContent = session.title || 'New Chat';
    tab.appendChild(title);

    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'tab-close';
    close.title = 'Close chat';
    close.innerHTML = '<i class="bi bi-x-lg"></i>';
    close.addEventListener('click', (e) => {
      e.stopPropagation();
      closeSession(session.id);
    });
    tab.appendChild(close);
    bar.appendChild(tab);
  }
}

async function closeSession(id) {
  if (state.streaming && id === state.activeSessionId) stopStream();
  try {
    await api(`/api/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ is_open: false }) });
    state.sessions = state.sessions.filter((s) => s.id !== id);
    if (id === state.activeSessionId) localStorage.removeItem(`forge.session.${state.activeProjectId}`);
    renderTabs();
    if (id === state.activeSessionId) {
      if (state.sessions.length) await loadSession(state.sessions[0].id);
      else clearSession();
    }
  } catch (err) {
    toast(err.message, 'error');
  }
}

function syncSession(session) {
  if (!session) return;
  state.session = { ...(state.session || {}), ...session };
  const idx = state.sessions.findIndex((s) => s.id === session.id);
  if (idx >= 0) state.sessions[idx] = { ...state.sessions[idx], ...session };
  else if (session.is_open) state.sessions.unshift(session);
  renderTabs();
}

async function patchSession(payload) {
  if (!state.session) return null;
  try {
    const data = await api(`/api/sessions/${state.session.id}`, {
      method: 'PATCH',
      body: JSON.stringify(payload),
    });
    syncSession(data.session);
    if (data.session.title) document.title = `${data.session.title} · Forge`;
    return data.session;
  } catch (err) {
    toast(err.message, 'error');
    return null;
  }
}

/* ── Session rendering ───────────────────────────────────────────── */

function renderSession(messages) {
  const container = $('messages');
  state.currentRound = null;
  state.toolCards = new Map();
  state.messageIndex = -1;
  container.innerHTML = '';
  let hasContent = false;
  for (const msg of messages) {
    if (msg.role === 'user') {
      appendUserMessage(msg.content);
      hasContent = true;
    } else if (msg.role === 'assistant') {
      if (renderAssistantHistory(msg)) hasContent = true;
    }
  }
  $('modelInput').value = state.session?.model || '';
  setModeUI(state.session?.mode || 'PLAN');
  renderFiles();
  renderChecklist();
  updateUsageUI();
  updateControls();
  if (hasContent) hideEmpty();
  else showEmpty('chat');
  document.title = `${state.session?.title || 'Forge'} · Forge`;
  const last = messages[messages.length - 1];
  if (state.session?.mode === 'PLAN' && last?.role === 'assistant' && (last.content || '').trim()) {
    addPlanReadyCard();
  }
  maybeScroll(true);
}

function removePlanReadyCard() {
  const existing = $('messages').querySelector('.plan-ready');
  if (existing) existing.remove();
}

function addPlanReadyCard() {
  removePlanReadyCard();
  const card = document.createElement('div');
  card.className = 'plan-ready';
  card.innerHTML = `
    <div class="plan-ready-icon"><i class="bi bi-map"></i></div>
    <div class="plan-ready-body">
      <strong>Plan ready</strong>
      <span>PLAN mode is read-only. Switch to BUILD to apply these changes.</span>
    </div>
    <div class="plan-ready-actions">
      <button type="button" class="btn btn-accent" data-action="build">Switch to BUILD</button>
      <button type="button" class="plan-ready-close" title="Dismiss" aria-label="Dismiss">&times;</button>
    </div>`;
  const build = card.querySelector('[data-action="build"]');
  build.addEventListener('click', async () => {
    build.disabled = true;
    const updated = await patchSession({ mode: 'BUILD' });
    if (!updated) {
      build.disabled = false;
      return;
    }
    setModeUI(updated.mode);
    card.remove();
    toast('BUILD mode enabled — describe the change you want to apply', 'success');
    $('promptInput').focus();
  });
  card.querySelector('.plan-ready-close').addEventListener('click', () => card.remove());
  $('messages').appendChild(card);
}

function appendUserMessage(text) {
  const wrap = document.createElement('div');
  wrap.className = 'msg msg-user';
  const bubble = document.createElement('div');
  bubble.className = 'user-bubble';
  bubble.textContent = text;
  wrap.appendChild(bubble);
  $('messages').appendChild(wrap);
}

function renderAssistantHistory(msg) {
  let calls = [];
  try {
    calls = Array.isArray(msg.tool_calls) ? msg.tool_calls : JSON.parse(msg.tool_calls || '[]');
  } catch {
    calls = [];
  }
  const hasReasoning = !!(msg.reasoning && msg.reasoning.trim());
  const hasContent = !!(msg.content && msg.content.trim());
  if (!hasReasoning && !hasContent && !calls.length) return false;

  const round = makeRoundElement();
  $('messages').appendChild(round.el);
  if (hasReasoning) {
    const card = addReasoningCard(round, msg.reasoning);
    card.title.textContent = 'Thought process';
    // A reasoning-only turn has no visible answer, so surface it.
    if (!hasContent) card.el.open = true;
  }
  for (const call of calls) {
    addHistoricalToolCall(round, call);
  }
  if (hasContent) {
    round.rawContent = msg.content;
    const contentEl = ensureContentEl(round);
    renderMarkdownInto(contentEl, msg.content, true);
  }
  if (Number(msg.duration_ms) > 0) {
    addHistoricalTurnTime(round, Number(msg.duration_ms));
  }
  return true;
}

function makeRoundElement() {
  const el = document.createElement('div');
  el.className = 'assistant-round msg';
  el.innerHTML = '<div class="round-avatar">F</div><div class="round-main"><div class="round-events"></div><div class="round-footer"></div></div>';
  return {
    el,
    main: el.querySelector('.round-main'),
    events: el.querySelector('.round-events'),
    footer: el.querySelector('.round-footer'),
    reasoning: null,
    contentEl: null,
    rawContent: '',
    thinking: null,
    timer: null,
    timerEl: null,
    hasEvents: false,
    num: 0,
  };
}

/* ── Streaming ───────────────────────────────────────────────────── */

async function sendMessage() {
  if (state.streaming) return;
  const prompt = $('promptInput');
  const text = prompt.value.trim();
  if (!text) return;
  if (!state.session) {
    if (!state.activeProjectId) {
      toast('Select or register a project first', 'error');
      return;
    }
    const created = await createSession({ silent: true });
    if (!created) return;
  }

  if (text.toLowerCase() === '/compact') {
    await compactConversation();
    return;
  }

  prompt.value = '';
  resizePrompt();
  updateSendState();
  await runTurn({ content: text, userText: text });
}

// continueTurn resumes a paused turn from its checkpoint. The server persists
// the synthetic user message, so the transcript stays consistent.
async function continueTurn() {
  if (state.streaming || !state.session) return;
  await runTurn({ isContinue: true, userText: 'Continue from where you left off.' });
}

async function runTurn({ content = '', isContinue = false, userText = content }) {
  removePlanReadyCard();
  updateSendState();
  appendUserMessage(userText);
  hideEmpty();
  state.messageIndex = -1;
  maybeScroll(true);

  state.streaming = true;
  state.sawCancelled = false;
  state.turnHadContent = false;
  state.turnHadError = false;
  state.turnReasoningOnly = false;
  state.turnRound = 0;
  state.turnLimit = 0;
  state.turnSegment = 1;
  state.turnSegmentSize = 0;
  const modeAtSend = state.session?.mode === 'BUILD' ? 'BUILD' : 'PLAN';
  setStreamingUI(true);
  startTurnClock();
  beginRound(0);
  const controller = new AbortController();
  state.abort = controller;

  try {
    const payload = isContinue
      ? { session_id: state.session.id, continue: true }
      : { session_id: state.session.id, content };
    const res = await fetch('/api/chat/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      signal: controller.signal,
    });
    if (!res.ok) {
      let msg = `Request failed (${res.status})`;
      try {
        const data = await res.json();
        if (data && data.error) msg = data.error;
      } catch { /* ignore */ }
      const err = new Error(msg);
      err.preflight = true;
      throw err;
    }
    if (!res.body) throw new Error('Streaming is not supported by this browser.');
    await readSSE(res.body);
  } catch (err) {
    if (err.preflight) {
      const users = $('messages').querySelectorAll('.msg-user');
      if (users.length) users[users.length - 1].remove();
      if (!$('messages').querySelector('.msg-user, .assistant-round')) showEmpty('chat');
      if (!isContinue && !$('promptInput').value.trim()) {
        $('promptInput').value = content;
        resizePrompt();
      }
      updateSendState();
      toast(err.message || 'Message was not sent', 'error');
    } else if (err.name === 'AbortError' || controller.signal.aborted) {
      if (!state.sawCancelled) addNotice('Stopped', 'stopped');
    } else {
      state.turnHadError = true;
      addNotice(err.message || 'Request failed', 'error');
      toast(err.message || 'Request failed', 'error');
    }
  } finally {
    stopTurnClock(true);
    finalizeRound();
    state.streaming = false;
    state.abort = null;
    state.turnRound = 0;
    state.turnLimit = 0;
    state.turnSegment = 1;
    state.turnSegmentSize = 0;
    updateStreamLabel();
    setStreamingUI(false);
    updateControls();
    if (!state.sawCancelled && !state.turnHadError && !state.turnReasoningOnly &&
        modeAtSend === 'PLAN' && state.turnHadContent) {
      addPlanReadyCard();
    }
    maybeScroll(true);
    updateSessionTabCosts();
  }
}

async function compactConversation() {
  if (state.streaming || !state.session) return;
  const prompt = $('promptInput');
  prompt.value = '';
  resizePrompt();
  updateSendState();

  state.streaming = true;
  state.abort = new AbortController();
  setStreamingUI(true);
  try {
    const res = await fetch(`/api/sessions/${state.session.id}/compact`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      signal: state.abort.signal,
    });
    let data = null;
    try {
      data = await res.json();
    } catch { /* ignore */ }
    if (!res.ok) throw new Error((data && data.error) || `Request failed (${res.status})`);

    syncSession(data.session);
    state.currentRound = null;
    state.toolCards = new Map();
    state.messageIndex = -1;
    renderSession(data.messages || []);
    state.usage.contextTokens = estimateContext(data.messages || []);
    state.usage.contextExact = false;
    updateUsageUI();
    toast(`Context compacted — ${data.removed} earlier messages summarized`, 'success');
    addNotice('Conversation compacted. Older messages were replaced by a summary; the recent exchange was kept.', 'stopped');
  } catch (err) {
    if (err.name === 'AbortError') {
      addNotice('Stopped', 'stopped');
    } else {
      toast(err.message || 'Could not compact the conversation', 'error');
    }
  } finally {
    state.streaming = false;
    state.abort = null;
    setStreamingUI(false);
    updateControls();
  }
}

async function readSSE(body) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    buffer = buffer.replace(/\r\n/g, '\n');
    let idx;
    while ((idx = buffer.indexOf('\n\n')) >= 0) {
      const block = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 2);
      handleSSEBlock(block);
    }
  }
  buffer += decoder.decode();
  if (buffer.trim()) handleSSEBlock(buffer);
}

function handleSSEBlock(block) {
  const dataLines = [];
  for (const line of block.split('\n')) {
    if (line.startsWith('data:')) dataLines.push(line.slice(5).replace(/^ /, ''));
  }
  if (!dataLines.length) return;
  let ev;
  try {
    ev = JSON.parse(dataLines.join('\n'));
  } catch {
    return;
  }
  handleEvent(ev);
}

function handleEvent(ev) {
  switch (ev.type) {
    case 'round_start':
      state.turnRound = ev.round || 0;
      state.turnLimit = ev.limit || 0;
      state.turnSegment = ev.segment || 1;
      state.turnSegmentSize = ev.segment_size || 0;
      updateStreamLabel();
      beginRound(ev.round || 0);
      break;
    case 'reasoning': {
      const round = ensureRound();
      removeThinking(round);
      const card = round.reasoning || addReasoningCard(round, '');
      card.pre.textContent += ev.content || '';
      card.meta.textContent = formatChars(card.pre.textContent.length);
      round.hasEvents = true;
      maybeScroll();
      break;
    }
    case 'content': {
      const round = ensureRound();
      removeThinking(round);
      round.rawContent += ev.content || '';
      round.hasEvents = true;
      if ((ev.content || '').trim()) state.turnHadContent = true;
      ensureContentEl(round);
      scheduleContentRender(round);
      break;
    }
    case 'tool_start': {
      const round = ensureRound();
      removeThinking(round);
      const card = addToolCard(round, ev);
      state.toolCards.set(ev.id, card);
      round.hasEvents = true;
      maybeScroll();
      break;
    }
    case 'tool_blocked': {
      const round = ensureRound();
      removeThinking(round);
      addBlockedToolCard(round, ev);
      round.hasEvents = true;
      maybeScroll();
      break;
    }
    case 'tool_end': {
      const round = state.currentRound || ensureRound();
      let card = state.toolCards.get(ev.id);
      if (!card) {
        card = addToolCard(round, ev);
        state.toolCards.set(ev.id, card);
      }
      finishToolCard(card, ev);
      if (Array.isArray(ev.files)) {
        for (const file of ev.files) rememberFile(file.path, file.action);
        renderFiles();
      }
      maybeScroll();
      break;
    }
    case 'usage':
      applyUsage(ev);
      break;
    case 'checklist':
      state.checklist = Array.isArray(ev.checklist) ? ev.checklist : [];
      renderChecklist();
      break;
    case 'session':
      if (ev.session) {
        syncSession(ev.session);
        if (ev.session.title) document.title = `${ev.session.title} · Forge`;
      }
      break;
    case 'done':
      if (ev.session) syncSession(ev.session);
      state.turnReasoningOnly = !!ev.reasoning_only;
      stopTurnClock(true);
      if (state.currentRound) removeThinking(state.currentRound);
      if (ev.reasoning_only) {
        const round = state.currentRound;
        if (round && round.reasoning) round.reasoning.el.open = true;
        addNotice(
          'The model returned reasoning but no final answer. Ask it to summarize the plan, or switch to another model.',
          'error'
        );
      }
      break;
    case 'checkpoint':
      if (ev.message) {
        if (ev.reason === 'auto') addNotice(ev.message, 'checkpoint');
        else addContinueNotice(ev.message);
      }
      break;
    case 'limit':
      stopTurnClock(true);
      if (state.currentRound) removeThinking(state.currentRound);
      addLimitNotice(ev);
      break;
    case 'cancelled':
      state.sawCancelled = true;
      stopTurnClock(true);
      addNotice('Stopped', 'stopped');
      break;
    case 'error':
      state.turnHadError = true;
      addNotice(ev.message || 'The provider returned an error.', 'error');
      break;
    default:
      break;
  }
}

function beginRound(num) {
  if (state.currentRound && state.currentRound.num === 0 && !state.currentRound.hasEvents) {
    state.currentRound.num = num || 1;
    return state.currentRound;
  }
  if (state.currentRound) detachTurnTimer(state.currentRound);
  finalizeRound();
  const round = makeRoundElement();
  round.num = num || 0;
  $('messages').appendChild(round.el);
  round.thinking = addThinkingRow(round);
  state.currentRound = round;
  if (state.streaming) attachTurnTimer(round);
  maybeScroll();
  return round;
}

function ensureRound() {
  if (!state.currentRound) return beginRound(0);
  return state.currentRound;
}

function finalizeRound() {
  const round = state.currentRound;
  if (!round) return;
  if (round.timer) {
    clearTimeout(round.timer);
    round.timer = null;
    renderContent(round, true);
  }
  if (round.reasoning) {
    round.reasoning.title.textContent = 'Thought process';
    round.reasoning.meta.textContent = formatChars((round.reasoning.pre.textContent || '').length);
  }
  if (!round.hasEvents && !round.contentEl) {
    round.el.remove();
  } else {
    renderContent(round, true);
  }
  round.el.classList.remove('is-working');
  state.currentRound = null;
}

/* ── Turn timers ─────────────────────────────────────────────────── */

function attachTurnTimer(round) {
  if (!round || !round.footer) return;
  detachTurnTimer(round);
  const el = document.createElement('div');
  el.className = 'round-time';
  el.title = 'Elapsed time for this response, including planning and tool calls';
  const icon = document.createElement('i');
  icon.className = 'bi bi-stopwatch';
  const label = document.createElement('span');
  label.textContent = formatElapsed(state.turnElapsedMs || 0);
  el.appendChild(icon);
  el.appendChild(label);
  round.footer.appendChild(el);
  round.timerEl = el;
  round.el.classList.add('is-working');
  state.turnTimerEl = label;
}

function detachTurnTimer(round) {
  if (!round) return;
  if (state.turnTimerEl && round.timerEl && round.timerEl.contains(state.turnTimerEl)) {
    state.turnTimerEl = null;
  }
  if (round.timerEl) {
    round.timerEl.remove();
    round.timerEl = null;
  }
  round.el.classList.remove('is-working');
}

function addHistoricalTurnTime(round, ms) {
  if (!round || !round.footer) return;
  const el = document.createElement('div');
  el.className = 'round-time';
  el.title = 'Total elapsed time for this response';
  const icon = document.createElement('i');
  icon.className = 'bi bi-stopwatch';
  const label = document.createElement('span');
  label.textContent = formatElapsed(ms);
  el.appendChild(icon);
  el.appendChild(label);
  round.footer.appendChild(el);
}

function startTurnClock() {
  stopTurnClock(false);
  const persisted = Math.max(Number(state.session?.duration_ms || 0), Number(state.sessionTimeBase || 0));
  state.turnBaseTime = persisted;
  state.sessionTimeBase = persisted;
  state.turnStartedAt = Date.now();
  state.turnElapsedMs = 0;
  state.turnClock = setInterval(tickTurnClock, 500);
  updateSessionTimeUI();
}

function tickTurnClock() {
  if (!state.turnStartedAt) return;
  state.turnElapsedMs = Date.now() - state.turnStartedAt;
  if (state.turnTimerEl) state.turnTimerEl.textContent = formatElapsed(state.turnElapsedMs);
  updateSessionTimeUI();
}

function stopTurnClock(finalize = true) {
  if (state.turnClock) {
    clearInterval(state.turnClock);
    state.turnClock = null;
  }
  if (state.turnStartedAt) {
    state.turnElapsedMs = Date.now() - state.turnStartedAt;
    state.turnStartedAt = 0;
  }
  if (finalize && state.turnTimerEl) {
    state.turnTimerEl.textContent = formatElapsed(state.turnElapsedMs);
    state.turnTimerEl = null;
  }
  if (finalize && state.currentRound) {
    state.currentRound.el.classList.remove('is-working');
  }
  if (finalize) {
    const persisted = Number(state.session?.duration_ms || 0);
    state.sessionTimeBase = Math.max(state.turnBaseTime + state.turnElapsedMs, persisted);
  }
  updateSessionTimeUI();
}

function updateSessionTimeUI() {
  const el = $('sessionTime');
  if (!el) return;
  let total = Math.max(Number(state.sessionTimeBase || 0), Number(state.session?.duration_ms || 0));
  if (state.streaming && state.turnStartedAt) {
    total = state.turnBaseTime + (Date.now() - state.turnStartedAt);
  }
  el.textContent = total > 0 ? formatElapsed(total) : '0s';
}

function addThinkingRow(round) {
  const row = document.createElement('div');
  row.className = 'thinking-row';
  row.innerHTML = '<span></span><span></span><span></span>';
  round.events.appendChild(row);
  return row;
}

function removeThinking(round) {
  if (round?.thinking) {
    round.thinking.remove();
    round.thinking = null;
  }
}

function scheduleContentRender(round) {
  if (round.timer) return;
  round.timer = setTimeout(() => {
    round.timer = null;
    renderContent(round, false);
    maybeScroll();
  }, 70);
}

function ensureContentEl(round) {
  if (!round.contentEl) {
    round.contentEl = document.createElement('div');
    round.contentEl.className = 'md';
    if (round.footer) round.main.insertBefore(round.contentEl, round.footer);
    else round.main.appendChild(round.contentEl);
  }
  return round.contentEl;
}

function renderContent(round, final) {
  if (!round.contentEl || !round.rawContent.trim()) return;
  renderMarkdownInto(round.contentEl, round.rawContent, final);
}

/* ── Event cards ─────────────────────────────────────────────────── */

function addReasoningCard(round, text) {
  const details = document.createElement('details');
  details.className = 'event-card reasoning-card';
  const summary = document.createElement('summary');
  summary.innerHTML =
    '<span class="chev">▶</span><span class="event-icon">🧠</span>' +
    '<span class="event-title">Thinking…</span><span class="event-meta"></span>';
  const body = document.createElement('div');
  body.className = 'event-body';
  const pre = document.createElement('pre');
  pre.textContent = text || '';
  body.appendChild(pre);
  details.appendChild(summary);
  details.appendChild(body);
  round.events.appendChild(details);
  const card = {
    el: details,
    pre,
    title: summary.querySelector('.event-title'),
    meta: summary.querySelector('.event-meta'),
  };
  round.reasoning = card;
  return card;
}

function addToolCard(round, ev) {
  const args = parseArgs(ev.arguments);
  const details = document.createElement('details');
  details.className = 'event-card tool-card is-running';

  const summary = document.createElement('summary');
  summary.innerHTML =
    '<span class="chev">▶</span>' +
    `<span class="event-icon">${toolIcon(ev.name)}</span>` +
    '<span class="event-title"></span>' +
    '<span class="event-meta">running</span>' +
    '<span class="tool-state running"><i class="bi bi-arrow-repeat"></i></span>';
  summary.querySelector('.event-title').textContent = toolTitle(ev.name, args);

  const body = document.createElement('div');
  body.className = 'event-body';
  const argLabel = document.createElement('span');
  argLabel.className = 'tool-label';
  argLabel.textContent = 'input';
  const argPre = document.createElement('pre');
  argPre.textContent = formatToolInput(ev.name, args, ev.arguments);
  const outLabel = document.createElement('span');
  outLabel.className = 'tool-label d-none';
  outLabel.textContent = 'output';
  outLabel.style.marginTop = '8px';
  const outPre = document.createElement('pre');
  outPre.className = 'tool-output';
  body.appendChild(argLabel);
  body.appendChild(argPre);
  body.appendChild(outLabel);
  body.appendChild(outPre);

  details.appendChild(summary);
  details.appendChild(body);
  round.events.appendChild(details);

  return {
    el: details,
    summary,
    meta: summary.querySelector('.event-meta'),
    state: summary.querySelector('.tool-state'),
    outLabel,
    outPre,
  };
}

function addBlockedToolCard(round, ev) {
  const details = document.createElement('details');
  details.className = 'event-card tool-card is-blocked';

  const summary = document.createElement('summary');
  summary.innerHTML =
    '<span class="chev">▶</span>' +
    '<span class="event-icon">🚫</span>' +
    '<span class="event-title"></span>' +
    '<span class="event-meta"></span>' +
    '<span class="tool-state blocked"><i class="bi bi-slash-circle"></i></span>';
  summary.querySelector('.event-title').textContent = `${ev.name} blocked`;
  summary.querySelector('.event-meta').textContent = 'mode check';

  const body = document.createElement('div');
  body.className = 'event-body';
  const label = document.createElement('span');
  label.className = 'tool-label';
  label.textContent = 'blocked before execution';
  const pre = document.createElement('pre');
  pre.className = 'tool-output blocked';
  pre.textContent = ev.message || 'This tool is unavailable in the current mode.';
  body.appendChild(label);
  body.appendChild(pre);

  details.appendChild(summary);
  details.appendChild(body);
  round.events.appendChild(details);
}

function finishToolCard(card, ev) {
  card.el.classList.remove('is-running');
  card.el.classList.toggle('is-error', !!ev.is_error);
  card.meta.textContent = ev.duration_ms ? formatDuration(ev.duration_ms) : 'done';
  card.state.classList.remove('running');
  if (ev.is_error) {
    card.state.classList.add('error');
    card.state.innerHTML = '<i class="bi bi-x-lg"></i>';
  } else {
    card.state.innerHTML = '<i class="bi bi-check2"></i>';
  }
  card.outLabel.classList.remove('d-none');
  card.outLabel.textContent = ev.is_error ? 'error' : 'output';
  card.outPre.textContent = ev.result || (ev.is_error ? 'Tool failed.' : '(no output)');
  card.outPre.classList.toggle('error', !!ev.is_error);
}

function addHistoricalToolCall(round, call) {
  const name = call.function?.name || call.name || 'tool';
  const argsRaw = call.function?.arguments || '{}';
  const card = addToolCard(round, {
    id: call.id,
    name,
    arguments: argsRaw,
    status: call.status,
  });
  const ev = {
    is_error: call.status === 'error',
    duration_ms: call.duration_ms || 0,
    result: call.result || (call.status === 'error' ? 'Tool failed.' : '(no result recorded)'),
  };
  finishToolCard(card, ev);
  applyHistoricalFile(call, name, parseArgs(argsRaw));
}

function applyHistoricalFile(call, name, args) {
  if (call.status === 'error' || !args.path) return;
  if (name === 'read_file') rememberFile(args.path, 'read');
  else if (name === 'write_file' || name === 'patch_file') rememberFile(args.path, 'modified');
}

function addNotice(text, kind) {
  const round = state.currentRound;
  if (round) {
    const existing = round.events.querySelector('.notice:last-child');
    if (existing && existing.textContent.trim() === text) return;
    const div = document.createElement('div');
    div.className = 'notice ' + (kind || '');
    div.textContent = text;
    round.events.appendChild(div);
    round.hasEvents = true;
    maybeScroll();
    return;
  }
  const div = document.createElement('div');
  div.className = 'notice ' + (kind || '');
  div.textContent = text;
  $('messages').appendChild(div);
  maybeScroll(true);
}

const LIMIT_LABELS = {
  loop_detected: 'Loop detected',
  tool_error_limit: 'Too many failed tool calls',
  turn_budget: 'Round budget reached',
  cost_budget: 'Cost budget reached',
};

// addLimitNotice renders a paused turn. Budget and loop stops are not errors:
// the transcript is safe and the user can either continue or change strategy.
function addLimitNotice(ev) {
  const reason = ev.reason || 'limit';
  const text = ev.message || `${LIMIT_LABELS[reason] || 'Turn paused'}.`;
  addContinueNotice(text);
}

// addContinueNotice renders a checkpoint with an explicit Continue action.
function addContinueNotice(text) {
  const round = state.currentRound;
  const host = round ? round.events : $('messages');
  const div = document.createElement('div');
  div.className = 'notice checkpoint';
  const label = document.createElement('span');
  label.textContent = text;
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'btn btn-ghost btn-sm notice-action';
  btn.textContent = 'Continue';
  btn.addEventListener('click', () => {
    btn.disabled = true;
    continueTurn();
  });
  div.append(label, btn);
  host.appendChild(div);
  if (round) {
    round.hasEvents = true;
    maybeScroll();
  } else {
    maybeScroll(true);
  }
}

function updateStreamLabel() {
  const label = $('streamLabel');
  if (!label) return;
  if (state.turnRound > 0 && state.turnLimit > 0) {
    let text = `round ${state.turnRound}/${state.turnLimit}`;
    const segments = state.turnSegmentSize > 0
      ? Math.ceil(state.turnLimit / state.turnSegmentSize)
      : 1;
    if (segments > 1 && state.turnSegment > 1) {
      text += ` · segment ${state.turnSegment}/${segments}`;
    }
    label.textContent = text;
  } else {
    label.textContent = 'working';
  }
}

function parseArgs(raw) {
  if (!raw) return {};
  if (typeof raw === 'object') return raw;
  try {
    return JSON.parse(raw);
  } catch {
    return {};
  }
}

function toolIcon(name) {
  return {
    read_file: '📄',
    write_file: '✍️',
    patch_file: '🩹',
    bash: '⚡',
    grep_search: '🔎',
    list_directory: '📁',
    git_diff: '🔀',
    update_checklist: '✅',
  }[name] || '🛠️';
}

function toolTitle(name, args) {
  switch (name) {
    case 'read_file':
      return `Reading ${args.path || 'file'}`;
    case 'write_file':
      return `Writing ${args.path || 'file'}`;
    case 'patch_file':
      return `Patching ${args.path || 'file'}`;
    case 'bash':
      return `Running: ${truncate(String(args.command || '').split('\n')[0], 68)}`;
    case 'grep_search':
      return `Searching /${truncate(String(args.pattern || ''), 42)}/`;
    case 'list_directory':
      return `Listing ${args.path || '.'}`;
    case 'git_diff':
      return args.staged ? 'Checking staged git diff' : 'Checking git diff';
    case 'update_checklist':
      return `Updating checklist${args.action ? ' · ' + args.action : ''}`;
    default:
      return name || 'Tool';
  }
}

function formatToolInput(name, args, raw) {
  if (name === 'bash' && args.command) return String(args.command);
  if (name === 'read_file' && args.path) {
    const range = args.start_line ? ` (lines ${args.start_line}${args.end_line ? '-' + args.end_line : '+'})` : '';
    return args.path + range;
  }
  if (name === 'patch_file' && args.search !== undefined) {
    return `path: ${args.path || ''}\n\nsearch:\n${args.search}\n\nreplace:\n${args.replace || ''}`;
  }
  if (raw && typeof raw === 'string') {
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }
  try {
    return JSON.stringify(args || {}, null, 2);
  } catch {
    return String(raw || '');
  }
}

/* ── Usage / files / checklist ───────────────────────────────────── */

function applyUsage(ev) {
  if (ev.session_tokens_in !== undefined) state.usage.sessionIn = ev.session_tokens_in;
  if (ev.session_tokens_out !== undefined) state.usage.sessionOut = ev.session_tokens_out;
  if (ev.session_cost_usd !== undefined) state.usage.sessionCost = ev.session_cost_usd;
  if (ev.project_cost_usd !== undefined) state.usage.projectCost = ev.project_cost_usd;
  if (ev.session_cached_tokens !== undefined) {
    state.usage.cached = ev.session_cached_tokens;
  } else if (ev.cached_tokens !== undefined) {
    state.usage.cached += ev.cached_tokens;
  }
  if (ev.context_tokens) {
    state.usage.contextTokens = ev.context_tokens;
    state.usage.contextExact = true;
  }
  if (state.session) {
    if (ev.session_tokens_in !== undefined) state.session.tokens_in = ev.session_tokens_in;
    if (ev.session_tokens_out !== undefined) state.session.tokens_out = ev.session_tokens_out;
    if (ev.session_cost_usd !== undefined) state.session.cost_usd = ev.session_cost_usd;
  }
  if (state.activeProject && ev.project_cost_usd !== undefined) {
    state.activeProject.total_cost_usd = ev.project_cost_usd;
    updateProjectTotals(state.activeProject);
  }
  updateUsageUI();
}

function contextLimit(model) {
  const id = String(model || '');
  const exact = state.models.get(id);
  if (exact && exact.context_length > 0) return exact.context_length;
  if (id) {
    for (const [key, value] of state.models) {
      if (key.toLowerCase() === id.toLowerCase() && value.context_length > 0) {
        return value.context_length;
      }
    }
  }
  const m = id.toLowerCase();
  if (m.includes('claude')) return 200000;
  if (m.includes('gemini')) return 1000000;
  if (m.includes('llama-4') || m.includes('maverick')) return 1000000;
  if (m.includes('gpt-5') || m.includes('gpt-4.1') || m.includes('o3')) return 400000;
  if (m.includes('qwen')) return 32768;
  if (m.includes('grok')) return 256000;
  if (m.includes('deepseek')) return 128000;
  return 128000;
}

function updateUsageUI() {
  const limit = contextLimit(state.session?.model || state.config?.defaults?.model);
  const used = Math.max(0, state.usage.contextTokens || 0);
  const pct = Math.min(100, (used / limit) * 100);
  $('ctxBar').style.width = pct.toFixed(1) + '%';
  $('contextPct').textContent = (pct < 1 && pct > 0 ? '<1' : Math.round(pct)) + '%';
  $('contextMeta').textContent =
    `${state.usage.contextExact ? '' : 'est. '}${formatNum(used)} / ${formatLimit(limit)} tokens`;
  $('contextMeta').title =
    `${used} / ${limit} tokens in the current context (prompt + response). ` +
    'The session input/output totals below add every model round.';
  $('tokensIn').textContent = formatNum(state.usage.sessionIn);
  $('tokensOut').textContent = formatNum(state.usage.sessionOut);
  $('cachedTokens').textContent = formatNum(state.usage.cached);
  const cachePct = state.usage.sessionIn > 0
    ? Math.min(100, (state.usage.cached / state.usage.sessionIn) * 100)
    : 0;
  $('cachedTokens').parentElement.title =
    `Cumulative prompt tokens served from the provider cache (${cachePct.toFixed(0)}% of session input). ` +
    'Cache hits require an identical prompt prefix and the same upstream provider.';
  $('sessionCost').textContent = formatCost(state.usage.sessionCost);
  $('projectCost').textContent = formatCost(state.usage.projectCost);
  updateSessionTimeUI();
  const status = $('sessionStatus');
  if (status) status.textContent = state.session ? formatLimit(limit) + ' ctx' : '';
}

function rememberFile(path, action) {
  if (!path) return;
  const previous = state.files.get(path);
  if (previous === 'modified' || action === 'modified') state.files.set(path, 'modified');
  else state.files.set(path, 'read');
}

function renderFiles() {
  const list = $('fileList');
  list.innerHTML = '';
  $('fileCount').textContent = String(state.files.size);
  if (!state.files.size) {
    const empty = document.createElement('div');
    empty.className = 'file-empty';
    empty.textContent = 'Files read or modified in this session appear here.';
    list.appendChild(empty);
    return;
  }
  for (const [path, action] of state.files) {
    const row = document.createElement('div');
    row.className = 'file-item';
    row.title = path;
    const icon = document.createElement('i');
    icon.className = 'bi ' + (isCodeFile(path) ? 'bi-file-earmark-code' : 'bi-file-earmark-text');
    const name = document.createElement('span');
    name.className = 'file-name';
    name.textContent = path;
    const badge = document.createElement('span');
    badge.className = 'file-badge ' + action;
    badge.textContent = action === 'modified' ? 'W' : 'R';
    row.appendChild(icon);
    row.appendChild(name);
    row.appendChild(badge);
    list.appendChild(row);
  }
}

function isCodeFile(path) {
  return /\.(go|js|ts|tsx|jsx|py|rs|rb|java|kt|kts|c|h|cc|cpp|hpp|cs|php|swift|sh|bash|zsh|sql|html|css|scss|less|json|yaml|yml|toml|xml|md|lua|ex|exs|erl|hs|clj|vue|svelte)$/i.test(path);
}

function estimateContext(messages) {
  if (!messages || !messages.length) return 0;
  let chars = 1800;
  for (const m of messages) {
    if (m.role === 'user') chars += (m.content || '').length;
    if (m.role === 'assistant') {
      chars += (m.content || '').length;
      let calls = [];
      try {
        calls = Array.isArray(m.tool_calls) ? m.tool_calls : JSON.parse(m.tool_calls || '[]');
      } catch {
        calls = [];
      }
      for (const call of calls) {
        chars += (call.function?.arguments || '').length;
        chars += (call.result || '').length;
      }
    }
  }
  return Math.ceil(chars / 4);
}

function renderChecklist() {
  const list = $('checklistItems');
  list.innerHTML = '';
  const total = state.checklist.length;
  const done = state.checklist.filter((i) => i.is_done).length;
  $('checklistCount').textContent = `${done}/${total}`;
  $('checklistBar').style.width = total ? `${(done / total) * 100}%` : '0%';

  if (!total) {
    const empty = document.createElement('div');
    empty.className = 'checklist-empty';
    empty.textContent = 'No tasks yet. Add one, or ask Forge to plan multi-step work.';
    list.appendChild(empty);
    return;
  }

  const pending = state.checklist.filter((i) => !i.is_done);
  const completed = state.checklist.filter((i) => i.is_done);

  const makeRow = (item) => {
    const row = document.createElement('div');
    row.className = 'check-item' + (item.is_done ? ' done' : '');

    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = !!item.is_done;
    box.setAttribute('aria-label', item.task_text);
    box.addEventListener('change', () => toggleChecklist(item, box.checked));

    const text = document.createElement('span');
    text.className = 'check-text';
    text.textContent = item.task_text;

    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'check-del';
    del.title = 'Remove task';
    del.setAttribute('aria-label', 'Remove task');
    del.innerHTML = '<i class="bi bi-x-lg"></i>';
    del.addEventListener('click', (e) => {
      e.preventDefault();
      e.stopPropagation();
      deleteChecklist(item);
    });

    row.appendChild(box);
    row.appendChild(text);
    row.appendChild(del);
    return row;
  };

  if (pending.length) {
    for (const item of pending) list.appendChild(makeRow(item));
  } else {
    const empty = document.createElement('div');
    empty.className = 'checklist-empty';
    empty.textContent = 'All tasks are done.';
    list.appendChild(empty);
  }

  if (completed.length) {
    const details = document.createElement('details');
    details.className = 'checklist-done';
    details.open = checklistDoneOpen;
    details.addEventListener('toggle', () => {
      checklistDoneOpen = details.open;
    });
    const summary = document.createElement('summary');
    const label = document.createElement('span');
    label.textContent = 'Completed';
    const count = document.createElement('span');
    count.className = 'done-count';
    count.textContent = String(completed.length);
    const chevron = document.createElement('i');
    chevron.className = 'bi bi-chevron-right done-chev';
    summary.appendChild(label);
    summary.appendChild(count);
    summary.appendChild(chevron);
    details.appendChild(summary);
    for (const item of completed) details.appendChild(makeRow(item));
    list.appendChild(details);
  }
}

async function toggleChecklist(item, done) {
  if (!state.session) return;
  item.is_done = done;
  renderChecklist();
  try {
    const data = await api(`/api/checklist/${item.id}`, {
      method: 'PATCH',
      body: JSON.stringify({ session_id: state.session.id, is_done: done }),
    });
    state.checklist = data.checklist || state.checklist;
    renderChecklist();
  } catch (err) {
    item.is_done = !done;
    renderChecklist();
    toast(err.message, 'error');
  }
}

async function deleteChecklist(item) {
  if (!state.session) return;
  try {
    const data = await api(`/api/checklist/${item.id}?session_id=${state.session.id}`, { method: 'DELETE' });
    state.checklist = data.checklist || [];
    renderChecklist();
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function addChecklist(text) {
  if (!state.session) return;
  try {
    const data = await api('/api/checklist', {
      method: 'POST',
      body: JSON.stringify({ session_id: state.session.id, task_text: text }),
    });
    state.checklist = data.checklist || [];
    renderChecklist();
  } catch (err) {
    toast(err.message, 'error');
  }
}

/* ── UI helpers ──────────────────────────────────────────────────── */

function setModeUI(mode) {
  const current = mode === 'BUILD' ? 'BUILD' : 'PLAN';
  for (const btn of $('modeToggle').querySelectorAll('button')) {
    btn.classList.toggle('active', btn.dataset.mode === current);
  }
  $('modeHint').textContent =
    current === 'PLAN'
      ? 'PLAN mode is read-only — inspect files, search, and diff'
      : 'BUILD mode can edit files and run commands';
}

function setStreamingUI(streaming) {
  $('streamIndicator').classList.toggle('d-none', !streaming);
  $('stopBtn').classList.toggle('d-none', !streaming);
  $('stopBtn').disabled = false;
  $('modelInput').disabled = streaming || !state.session;
  for (const btn of $('modeToggle').querySelectorAll('button')) btn.disabled = streaming || !state.session;
  updateSendState();
}

function updateControls() {
  const hasSession = !!state.session;
  $('modelInput').disabled = state.streaming || !hasSession;
  for (const btn of $('modeToggle').querySelectorAll('button')) btn.disabled = state.streaming || !hasSession;
  $('promptInput').disabled = !hasSession;
  $('newSessionBtn').disabled = !state.activeProjectId;
  updateSendState();
  updateUsageUI();
}

function updateSendState() {
  const prompt = $('promptInput');
  $('sendBtn').disabled = state.streaming || !state.session || !prompt.value.trim();
}

function maybeScroll(force = false) {
  const el = $('chatScroll');
  const near = el.scrollHeight - el.scrollTop - el.clientHeight < 140;
  if (force || near) el.scrollTop = el.scrollHeight;
}

function showEmpty(kind) {
  const el = $('emptyState');
  el.classList.remove('hidden');
  const h1 = el.querySelector('h1');
  const p = el.querySelector('p');
  const hints = el.querySelector('.empty-hints');
  hints.classList.add('d-none');
  if (kind === 'project') {
    h1.textContent = 'Register a project';
    p.textContent = 'Use the + button in the left rail to add a local working directory, then start a chat.';
  } else if (kind === 'session') {
    h1.textContent = 'New chat';
    p.textContent = 'Start a session with the + button above, or reopen a project.';
  } else if (kind === 'error') {
    h1.textContent = 'Something went wrong';
    p.textContent = 'Check the server logs and refresh the page.';
  } else {
    h1.textContent = 'Forge';
    p.textContent = 'A local coding agent. Pick a project, choose a mode, and start.';
    hints.classList.remove('d-none');
  }
}

function hideEmpty() {
  $('emptyState').classList.add('hidden');
}

function resizePrompt() {
  const el = $('promptInput');
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 230) + 'px';
}

function formatNum(n) {
  const num = Number(n || 0);
  if (num >= 1e6) return (num / 1e6).toFixed(num >= 1e7 ? 0 : 1) + 'M';
  if (num >= 1e3) return (num / 1e3).toFixed(num >= 1e4 ? 0 : 1) + 'k';
  return String(Math.round(num));
}

function formatLimit(n) {
  if (n >= 1e6) return n / 1e6 + 'M';
  if (n >= 1e3) return Math.round(n / 1e3) + 'k';
  return String(n);
}

function formatCost(n) {
  return '$' + Number(n || 0).toFixed(4);
}

function formatDuration(ms) {
  if (ms < 1000) return ms + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 1 : 0) + 's';
  return Math.floor(ms / 60000) + 'm ' + Math.round((ms % 60000) / 1000) + 's';
}

// Timers are shown in whole seconds: 0s, 12s, 1m 05s, 1h 02m.
function formatElapsed(ms) {
  const total = Math.max(0, Math.round(Number(ms || 0) / 1000));
  if (total < 60) return total + 's';
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  if (minutes < 60) return `${minutes}m ${String(seconds).padStart(2, '0')}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${String(minutes % 60).padStart(2, '0')}m`;
}

function formatChars(n) {
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k chars';
  return n + ' chars';
}

function truncate(text, max) {
  const s = String(text || '');
  return s.length > max ? s.slice(0, max - 1) + '…' : s;
}

function updateProjectTotals(project) {
  const idx = state.projects.findIndex((p) => p.id === project.id);
  if (idx >= 0) state.projects[idx] = { ...state.projects[idx], ...project };
  else state.projects.push(project);
  state.activeProject = { ...(state.activeProject || {}), ...project };
  renderProjects();
}

function updateSessionTabCosts() {
  if (!state.session) return;
  const idx = state.sessions.findIndex((s) => s.id === state.session.id);
  if (idx >= 0) {
    state.sessions[idx] = { ...state.sessions[idx], ...state.session };
    renderTabs();
  }
}

/* ── Markdown + sanitizer ────────────────────────────────────────── */

const ALLOWED_TAGS = new Set([
  'P', 'BR', 'STRONG', 'EM', 'DEL', 'CODE', 'PRE', 'BLOCKQUOTE', 'UL', 'OL', 'LI',
  'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'A', 'HR', 'TABLE', 'THEAD', 'TBODY', 'TR',
  'TH', 'TD', 'SPAN', 'INPUT',
]);
const ALLOWED_ATTRS = new Set([
  'href', 'title', 'class', 'start', 'align', 'type', 'checked', 'disabled',
  'target', 'rel', 'colspan', 'rowspan',
]);

function renderMarkdownInto(el, raw, highlight) {
  let html;
  try {
    html = window.marked ? marked.parse(raw) : escapeHtml(raw);
  } catch {
    html = escapeHtml(raw);
  }
  el.innerHTML = sanitizeHtml(html);
  if (highlight && window.hljs) {
    for (const block of el.querySelectorAll('pre code')) {
      try {
        hljs.highlightElement(block);
      } catch { /* ignore */ }
    }
  }
}

function sanitizeHtml(html) {
  const template = document.createElement('template');
  template.innerHTML = html;
  const walk = (parent) => {
    for (const node of Array.from(parent.children)) {
      const tag = node.tagName;
      if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'IFRAME' || tag === 'OBJECT' || tag === 'EMBED') {
        node.remove();
        continue;
      }
      if (!ALLOWED_TAGS.has(tag)) {
        node.replaceWith(document.createTextNode(node.textContent || ''));
        continue;
      }
      for (const attr of Array.from(node.attributes)) {
        const name = attr.name.toLowerCase();
        if (name.startsWith('on')) {
          node.removeAttribute(attr.name);
          continue;
        }
        if (name === 'href') {
          const value = attr.value.trim();
          if (!/^(https?:|mailto:|#|\/|\.{1,2}\/)/i.test(value)) node.removeAttribute('href');
          continue;
        }
        if (!ALLOWED_ATTRS.has(name)) node.removeAttribute(attr.name);
      }
      if (tag === 'A' && node.hasAttribute('href')) {
        node.setAttribute('target', '_blank');
        node.setAttribute('rel', 'noopener noreferrer');
      }
      if (tag === 'INPUT') {
        node.setAttribute('disabled', '');
        if (node.getAttribute('type') !== 'checkbox') {
          node.remove();
          continue;
        }
      }
      walk(node);
    }
  };
  walk(template.content);
  return template.innerHTML;
}

function escapeHtml(text) {
  return String(text)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/* ── Settings / project modals ───────────────────────────────────── */

function openSettings() {
  if (!state.config) return;
  const cfg = state.config;
  $('apiKey').value = '';
  $('apiKey').type = 'password';
  $('apiKey').placeholder = state.apiKeySet
    ? '•••••••• (configured) — leave blank to keep'
    : 'sk-or-v1-…';
  $('apiKeyHelp').innerHTML = state.apiKeySet
    ? 'A key is configured. Leave blank to keep it, or paste a new key.'
    : 'Stored locally in <code>~/.forge/config.json</code>.';
  $('baseURL').value = cfg.providers.openrouter.base_url || '';
  $('defaultModel').value = cfg.defaults.model || '';
  $('defaultMode').value = cfg.defaults.mode || 'PLAN';
  $('maxRead').value = cfg.defaults.max_file_read_chars || 25000;
  $('bashTimeout').value = cfg.defaults.bash_timeout_sec || 30;
  $('maxToolRounds').value = cfg.defaults.max_tool_rounds || 24;
  $('maxTurnRounds').value = cfg.defaults.max_turn_rounds || 96;
  $('maxTurnCost').value = cfg.defaults.max_turn_cost_usd ?? 0;
  $('planRounds').value = cfg.defaults.plan_rounds ?? 16;
  $('planToolCalls').value = cfg.defaults.plan_tool_calls ?? 64;
  $('historyKeepRecent').value = cfg.defaults.history_keep_recent_rounds ?? 4;
  $('historyMaxResult').value = cfg.defaults.history_max_result_chars ?? 4000;
  $('historyMaxArg').value = cfg.defaults.history_max_arg_chars ?? 4000;
  $('historyPruneChunk').value = cfg.defaults.history_prune_chunk_rounds ?? 4;
  $('autoContinue').checked = cfg.defaults.auto_continue !== false;
  $('serverHost').value = cfg.server.host || '127.0.0.1';
  $('serverPort').value = cfg.server.port || 7890;
  $('autoBrowser').checked = !!cfg.server.auto_open_browser;
  $('debugToggle').checked = !!cfg.server.debug || !!state.debugActive;
  $('logPath').textContent = state.logPath || '~/.forge/forge.log';
  $('configPath').textContent = state.configPath || '~/.forge/config.json';
  if (settingsModal) settingsModal.show();
}

async function saveSettings(e) {
  e.preventDefault();
  if (!state.config) return;
  const cfg = JSON.parse(JSON.stringify(state.config));
  cfg.providers.openrouter.base_url = $('baseURL').value.trim();
  const key = $('apiKey').value.trim();
  cfg.providers.openrouter.api_key = key || '__KEEP__';
  cfg.defaults.model = $('defaultModel').value.trim();
  cfg.defaults.mode = $('defaultMode').value;
  cfg.defaults.max_file_read_chars = Number($('maxRead').value) || 25000;
  cfg.defaults.bash_timeout_sec = Number($('bashTimeout').value) || 30;
  cfg.defaults.max_tool_rounds = Number($('maxToolRounds').value) || 24;
  cfg.defaults.max_turn_rounds = Number($('maxTurnRounds').value) || 96;
  cfg.defaults.max_turn_cost_usd = Number($('maxTurnCost').value) || 0;
  cfg.defaults.plan_rounds = Number($('planRounds').value) || 16;
  cfg.defaults.plan_tool_calls = Number($('planToolCalls').value) || 64;
  const numOr = (id, fallback) => {
    const raw = $(id).value.trim();
    if (raw === '') return fallback;
    const n = Number(raw);
    return Number.isFinite(n) ? n : fallback;
  };
  cfg.defaults.history_keep_recent_rounds = numOr('historyKeepRecent', 4);
  cfg.defaults.history_max_result_chars = numOr('historyMaxResult', 4000);
  cfg.defaults.history_max_arg_chars = numOr('historyMaxArg', 4000);
  cfg.defaults.history_prune_chunk_rounds = numOr('historyPruneChunk', 4);
  cfg.defaults.auto_continue = $('autoContinue').checked;
  cfg.server.host = $('serverHost').value.trim();
  cfg.server.port = Number($('serverPort').value) || 7890;
  cfg.server.auto_open_browser = $('autoBrowser').checked;
  cfg.server.debug = $('debugToggle').checked;

  const before = state.config;
  try {
    const data = await api('/api/config', { method: 'PUT', body: JSON.stringify(cfg) });
    state.config = data.config;
    state.configPath = data.path;
    state.logPath = data.log_path || state.logPath;
    state.debugActive = !!data.debug;
    state.apiKeySet = !!data.api_key_set;
    if (settingsModal) settingsModal.hide();
    toast('Settings saved', 'success');
    if (before.server.debug !== state.config.server.debug) {
      toast(state.config.server.debug ? 'Debug logging enabled' : 'Debug logging disabled', 'success');
    }
    if (before.server.port !== state.config.server.port || before.server.host !== state.config.server.host) {
      toast('Host/port will apply after restarting Forge', 'success');
    }
  } catch (err) {
    toast(err.message, 'error');
  }
}

function openProjectModal() {
  $('projectForm').reset();
  $('projectColor').value = randomPaletteColor();
  if (projectModal) projectModal.show();
  setTimeout(() => $('projectPath').focus(), 180);
}

async function saveProject(e) {
  e.preventDefault();
  const path = $('projectPath').value.trim();
  if (!path) return;
  try {
    const data = await api('/api/projects', {
      method: 'POST',
      body: JSON.stringify({
        path,
        name: $('projectName').value.trim(),
        icon_color: $('projectColor').value,
      }),
    });
    state.projects = data.projects || [data.project];
    renderProjects();
    if (projectModal) projectModal.hide();
    toast(`Added ${data.project.name}`, 'success');
    await selectProject(data.project.id, { autoCreate: true });
  } catch (err) {
    toast(err.message, 'error');
  }
}

/* ── Toasts ──────────────────────────────────────────────────────── */

function toast(message, type = 'error') {
  const container = $('toasts');
  // Keep toasts clear of the composer so they never cover Send/Stop.
  const composer = document.querySelector('.composer-wrap');
  if (composer) container.style.marginBottom = `${composer.offsetHeight + 4}px`;
  const el = document.createElement('div');
  el.className = `toast app-toast ${type}`;
  el.setAttribute('role', 'status');
  const icon = type === 'success' ? 'bi-check-circle' : 'bi-exclamation-triangle';
  el.innerHTML = `<div class="toast-body"><i class="bi ${icon}"></i><span class="toast-text"></span></div>`;
  el.querySelector('.toast-text').textContent = message;
  container.appendChild(el);
  if (window.bootstrap) {
    const t = new bootstrap.Toast(el, { delay: 4200 });
    el.addEventListener('hidden.bs.toast', () => el.remove());
    t.show();
  } else {
    setTimeout(() => el.remove(), 4200);
  }
}

/* ── Events ──────────────────────────────────────────────────────── */

function bindEvents() {
  $('addProjectBtn').addEventListener('click', openProjectModal);
  $('themeBtn').addEventListener('click', toggleTheme);
  $('collapsePanelBtn').addEventListener('click', () => setPanelHidden(true));
  $('showPanelBtn').addEventListener('click', () => setPanelHidden(false));
  $('settingsBtn').addEventListener('click', openSettings);
  $('newSessionBtn').addEventListener('click', () => createSession());
  $('sendBtn').addEventListener('click', sendMessage);
  $('stopBtn').addEventListener('click', () => {
    $('stopBtn').disabled = true;
    stopStream();
  });

  $('promptInput').addEventListener('input', () => {
    resizePrompt();
    updateSendState();
  });
  $('promptInput').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendMessage();
    }
  });

  $('modeToggle').addEventListener('click', async (e) => {
    const btn = e.target.closest('button[data-mode]');
    if (!btn || !state.session) return;
    if (state.streaming) {
      toast('Stop the current turn before changing mode', 'error');
      return;
    }
    const mode = btn.dataset.mode;
    if (mode === state.session.mode) return;
    const updated = await patchSession({ mode });
    if (updated) setModeUI(updated.mode);
  });

  $('modelInput').addEventListener('change', async () => {
    if (!state.session) return;
    const model = $('modelInput').value.trim();
    if (!model) {
      $('modelInput').value = state.session.model;
      return;
    }
    if (model === state.session.model) return;
    const updated = await patchSession({ model });
    if (updated) {
      $('modelInput').value = updated.model;
      updateUsageUI();
    }
  });

  $('checklistForm').addEventListener('submit', (e) => {
    e.preventDefault();
    const input = $('checklistInput');
    const text = input.value.trim();
    if (!text) return;
    input.value = '';
    addChecklist(text);
  });

  $('projectForm').addEventListener('submit', saveProject);
  $('settingsForm').addEventListener('submit', saveSettings);
  $('toggleKey').addEventListener('click', () => {
    const input = $('apiKey');
    input.type = input.type === 'password' ? 'text' : 'password';
  });

  $('promptInput').addEventListener('focus', () => {
    setTimeout(resizePrompt, 0);
  });

  document.addEventListener('keydown', handleShortcut);
}

function handleShortcut(e) {
  if (e.isComposing) return;
  // Ctrl+Shift avoids the shortcuts every browser and desktop reserves for
  // Ctrl+L / Ctrl+N / Ctrl+P / Ctrl+W / Ctrl+, / Cmd+? etc.
  if (!e.ctrlKey || !e.shiftKey || e.altKey || e.metaKey) return;

  const key = e.key.toLowerCase();
  const code = e.code || '';
  const modalOpen = !!document.querySelector('.modal.show');

  if (code === 'Comma') {
    e.preventDefault();
    openSettings();
    return;
  }
  if (code === 'Period') {
    if (modalOpen) return;
    e.preventDefault();
    toggleMode();
    return;
  }
  if (code === 'Slash' || code === 'NumpadDivide' || key === '/' || key === '?') {
    if (modalOpen) return;
    e.preventDefault();
    $('modelInput').focus();
    $('modelInput').select();
    return;
  }
  if (code === 'KeyL' || key === 'l') {
    if (modalOpen) return;
    e.preventDefault();
    focusChat();
    return;
  }
  if (key === 'arrowup') {
    if (modalOpen) return;
    e.preventDefault();
    navigateUserMessage(-1);
    return;
  }
  if (key === 'arrowdown') {
    if (modalOpen) return;
    e.preventDefault();
    navigateUserMessage(1);
  }
}

function focusChat() {
  const prompt = $('promptInput');
  if (!prompt || prompt.disabled) return;
  prompt.focus();
  const end = prompt.value.length;
  try {
    prompt.setSelectionRange(end, end);
  } catch { /* ignore */ }
}

async function toggleMode() {
  if (!state.session) {
    toast('Select a project first', 'error');
    return;
  }
  if (state.streaming) {
    toast('Stop the current turn before changing mode', 'error');
    return;
  }
  const next = state.session.mode === 'BUILD' ? 'PLAN' : 'BUILD';
  const updated = await patchSession({ mode: next });
  if (updated) setModeUI(updated.mode);
}

function navigateUserMessage(dir) {
  // Cycle only the user's own messages; assistant/tool rounds are skipped.
  const all = [...$('messages').querySelectorAll('.msg')];
  const items = all.filter((el) => el.classList.contains('msg-user'));
  if (!items.length) return;
  all.forEach((el) => el.classList.remove('msg-focus'));
  let idx = state.messageIndex;
  if (idx < 0 || idx >= items.length) {
    idx = dir > 0 ? -1 : items.length;
  }
  idx += dir;
  if (idx < 0) idx = 0;
  if (idx >= items.length) idx = items.length - 1;
  state.messageIndex = idx;
  for (let i = 0; i < items.length; i++) {
    items[i].classList.toggle('msg-focus', i === idx);
  }
  items[idx].scrollIntoView({ block: 'center', behavior: 'smooth' });
}

function stopStream() {
  if (state.abort) {
    try {
      state.abort.abort();
    } catch { /* ignore */ }
  }
}