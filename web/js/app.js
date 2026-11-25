'use strict';

/* ── State ───────────────────────────────────────────────────────── */

const $ = (id) => document.getElementById(id);

const state = {
  config: null,
  configPath: '',
  logPath: '',
  debugActive: false,
  llmDebugActive: false,
  apiKeySet: false,
  providers: [],
  activeProviderId: '',
  models: new Map(),
  modelsSource: '',
  projects: [],
  activeProjectId: null,
  activeProject: null,
  // Git state of the active project, shown in the workspace bar. `checked`
  // separates "not probed yet" from "probed, plain directory"; `seq` discards
  // probes that land after the user switched project.
  git: { projectId: null, checked: false, isRepo: false, branch: '', detached: false, commit: '', seq: 0 },
  sessions: [],
  allSessions: [],
  activeSessionId: null,
  session: null,
  // Files pinned with the composer's "@" picker for the next message.
  attachments: [],
  fileCache: null,
  filePicker: { open: false, mention: null, files: [], matches: [], index: 0 },
  // Chat-scoped state (transcript, streaming flags, rounds, timers, usage) is
  // kept in one view per session — see chatViews below — so several agents can
  // run at the same time. The accessors installed after this literal project
  // that state onto the active view, which keeps the rest of the UI unchanged.
};

const EMPTY_USAGE = {
  sessionIn: 0, sessionOut: 0, sessionCost: 0, projectCost: 0,
  contextTokens: 0, contextExact: false, cached: 0,
  rounds: 0, prunedRounds: 0, prunedChars: 0,
};

/* ── Per-session chat views ──────────────────────────────────────── */

// Every session owns a view: its own transcript element, streaming flags,
// current round, tool cards, usage and timer. Switching project or tab only
// changes which view is mounted, so a background agent keeps streaming and
// rendering into its own element instead of being cancelled.
const chatViews = new Map();
// renderView is set while a view is being rendered or fed stream events, so the
// state accessors below address that view even when it is not the active one.
let renderView = null;
// Run status polled from the server: lets the tab bar show agents that keep
// working after the user navigated away or closed the browser tab.
const runIndex = new Map();
let runWatchTimer = null;

function makeView(sessionId, projectId = 0) {
  const el = document.createElement('div');
  el.className = 'messages-view';
  el.dataset.session = String(sessionId || '');
  return {
    sessionId,
    projectId,
    el,
    streaming: false,
    stopRequested: false,
    attached: false,
    finishedUnseen: false,
    seq: 0,
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
    answerIndex: -1,
    sessionTimeBase: 0,
    turnBaseTime: 0,
    turnStartedAt: 0,
    turnElapsedMs: 0,
    turnClock: null,
    turnTimerEl: null,
    checklist: [],
    files: new Map(),
    questionBatches: [],
    renderedQuestions: new Set(),
    // Compaction markers: each one records where the context window starts and
    // the summary that replaced the older messages. The transcript stays whole,
    // so these are drawn as cards inside the history instead of hiding it.
    compactions: [],
    usage: { ...EMPTY_USAGE },
  };
}

function viewFor(sessionId, { create = false } = {}) {
  if (!sessionId) return null;
  let view = chatViews.get(sessionId);
  if (!view && create) {
    const session = sessionById(sessionId);
    view = makeView(sessionId, session?.project_id || state.activeProjectId || 0);
    chatViews.set(sessionId, view);
  }
  return view || null;
}

// sessionById reads a chat from the tab list or the active session, so code
// working on a background agent never has to fall back to state.session.
function sessionById(id) {
  if (!id) return null;
  const tab = state.sessions.find((s) => s.id === id);
  if (tab) return tab;
  const all = state.allSessions.find((s) => s.id === id);
  if (all) return all;
  return state.session && state.session.id === id ? state.session : null;
}

function dropView(sessionId) {
  const view = chatViews.get(sessionId);
  if (!view) return;
  if (view.turnClock) clearInterval(view.turnClock);
  chatViews.delete(sessionId);
}

// currentView resolves the view that UI code should read or write right now.
function currentView() {
  return renderView || viewFor(state.activeSessionId);
}

// withView runs fn with all chat-scoped state pointing at `view`. Handlers are
// synchronous, so no other stream can interleave and steal the context.
function withView(view, fn) {
  if (!view) return fn();
  const prev = renderView;
  renderView = view;
  try {
    return fn();
  } finally {
    renderView = prev;
  }
}

// messagesEl is the transcript container of the view being rendered.
function messagesEl() {
  const view = currentView();
  return view ? view.el : $('messages');
}

// viewIsActive reports whether a view is the one currently on screen.
function viewIsActive(view) {
  return !!view && view === viewFor(state.activeSessionId);
}

// activeView is the view the user is looking at. Unlike currentView it ignores
// the view currently being rendered, which is what the shared chrome and the
// Inspector must read to stay immune to background agents.
function activeView() {
  return viewFor(state.activeSessionId);
}

// mountView puts a session's transcript on screen by moving its element. A
// background agent keeps streaming into its own element, untouched.
function mountView(view) {
  const host = $('messages');
  for (const child of [...host.children]) {
    if (child !== (view && view.el)) child.remove();
  }
  if (view && view.el.parentElement !== host) host.appendChild(view.el);
  if (view && view.el.children.length) hideEmpty();
  else showEmpty('chat');
  maybeScroll(true);
  // The Inspector and the composer chrome belong to the chat on screen: repaint
  // both from that view, so what a background agent wrote into its own state
  // becomes visible only now. This is what hides Stop when opening a quiet chat.
  renderInspector();
  renderChrome();
}

// runIndex mirrors the server's live runs so the tab bar and the "running
// agents" list can show agents that keep working while the user is elsewhere.
function setRunStatus(status) {
  if (!status || !status.session_id) return;
  if (status.state === 'running') runIndex.set(status.session_id, status);
  else runIndex.delete(status.session_id);
  afterRunIndexChange();
}

// afterRunIndexChange refreshes everything that reads runIndex. Keeping it in
// one place means a new consumer of the index only has to be added here.
function afterRunIndexChange() {
  renderTabs();
  updateControls();
  updateAgentsBadge();
  if (agentsModal && $('agentsModal')?.classList.contains('show')) renderAgentsList();
}

async function refreshRunIndex() {
  try {
    const data = await api('/api/runs');
    runIndex.clear();
    for (const run of data.runs || []) {
      if (run.state === 'running') runIndex.set(run.session_id, run);
    }
    afterRunIndexChange();
  } catch { /* offline: the tab bar simply shows no spinners */ }
}

function startRunWatch() {
  if (runWatchTimer) clearInterval(runWatchTimer);
  runWatchTimer = setInterval(() => {
    if (document.hidden) return;
    refreshRunIndex();
  }, 5000);
}

const VIEW_DEFAULTS = {
  toolCards: () => new Map(),
  files: () => new Map(),
  checklist: () => [],
  questionBatches: () => [],
  messageIndex: -1,
  answerIndex: -1,
  turnSegment: 1,
  turnTimerEl: null,
  abort: null,
  currentRound: null,
};

for (const field of [
  'streaming', 'stopRequested', 'attached', 'finishedUnseen', 'abort', 'currentRound',
  'toolCards', 'sawCancelled', 'turnHadContent', 'turnHadError', 'turnReasoningOnly',
  'turnRound', 'turnLimit', 'turnSegment', 'turnSegmentSize', 'messageIndex',
  'answerIndex',
  'sessionTimeBase', 'turnBaseTime', 'turnStartedAt', 'turnElapsedMs', 'turnClock',
  'turnTimerEl', 'checklist', 'files', 'questionBatches',
]) {
  Object.defineProperty(state, field, {
    configurable: true,
    get() {
      const view = currentView();
      if (view) return view[field];
      const fallback = VIEW_DEFAULTS[field];
      return typeof fallback === 'function' ? fallback() : (fallback !== undefined ? fallback : false);
    },
    set(value) {
      const view = currentView();
      if (view) view[field] = value;
      // Both shared surfaces are projections of the view that changed: the
      // Inspector and the composer chrome. Scheduling is a no-op for agents
      // that are not on screen, so a new chat-scoped field is armored by default.
      scheduleInspector(view);
      scheduleChrome();
    },
  });
}

Object.defineProperty(state, 'usage', {
  configurable: true,
  get() {
    const view = currentView();
    return view ? view.usage : EMPTY_USAGE;
  },
  set(value) {
    const view = currentView();
    if (view) view.usage = value;
    scheduleInspector(view);
    scheduleChrome();
  },
});

let projectModal = null;
let settingsModal = null;
let sessionsModal = null;
let renameTabModal = null;
let agentsModal = null;
let agentsTimer = null;
let dragTabId = null;
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
  initUsageCard();
  setPanelHidden(localStorage.getItem('forge.panelHidden') === '1', { persist: false });
  bindEvents();
  if (window.bootstrap) {
    projectModal = new bootstrap.Modal($('projectModal'));
    settingsModal = new bootstrap.Modal($('settingsModal'));
    sessionsModal = new bootstrap.Modal($('sessionsModal'));
    renameTabModal = new bootstrap.Modal($('renameTabModal'));
    agentsModal = new bootstrap.Modal($('agentsModal'));
    // The agents list is only polled while it is on screen.
    $('agentsModal').addEventListener('show.bs.modal', startAgentsWatch);
    $('agentsModal').addEventListener('hidden.bs.modal', stopAgentsWatch);
    initTooltips($('settingsModal'));
  }
  try {
    await loadConfig();
    await loadProviders();
    const provider = activeProvider();
    $('modelInput').value = (provider && provider.default_model) || state.config?.defaults?.model || '';
    loadModels(state.activeProviderId);
    await loadProjects();
    if (!state.projects.length) {
      showEmpty('project');
      updateControls();
      return;
    }
    const saved = Number(localStorage.getItem('forge.project') || 0);
    const project = state.projects.find((p) => p.id === saved) || state.projects[0];
    await selectProject(project.id, { autoCreate: true });
    // Rebuild the tab indicators for agents that kept running while the page
    // was closed, then keep the index warm.
    await refreshRunIndex();
    startRunWatch();
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
  state.llmDebugActive = !!data.llm_debug;
  state.apiKeySet = !!data.api_key_set;
}

async function loadModels(providerId = '', { refresh = false } = {}) {
  try {
    const params = new URLSearchParams();
    if (providerId) params.set('provider_id', providerId);
    if (refresh) params.set('refresh', '1');
    const query = params.toString();
    const data = await api('/api/models' + (query ? `?${query}` : ''));
    const list = Array.isArray(data.models) ? data.models : [];
    state.models = new Map(list.map((m) => [String(m.id), m]));
    state.modelsSource = data.source || '';
    renderModelOptions(list);
    updateUsageUI();
  } catch {
    /* Keep the built-in heuristics when the catalog is unavailable. */
  }
}

async function loadProviders() {
  try {
    const data = await api('/api/providers');
    state.providers = Array.isArray(data.providers) ? data.providers : [];
    state.activeProviderId = data.active_id ||
      (state.providers.find((p) => p.is_active) || {}).id || '';
    renderProviderSettings();
    renderSessionProviderOptions();
  } catch (err) {
    state.providers = [];
    state.activeProviderId = '';
    renderProviderSettings();
    renderSessionProviderOptions();
    toast(err.message, 'error');
  }
}

function providerById(id) {
  return state.providers.find((p) => p.id === id) || null;
}

function activeProvider() {
  return providerById(state.activeProviderId) ||
    state.providers.find((p) => p.is_active) || null;
}

function effectiveProviderID() {
  return (state.session && state.session.provider_id) || state.activeProviderId || '';
}

function providerName(id) {
  const p = providerById(id);
  return p ? p.name : (id || 'Default');
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
    btn.addEventListener('contextmenu', (e) => {
      e.preventDefault();
      openProjectModal(project);
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

// Human-friendly label for the active project: its title/name when set,
// falling back to the last path segment, and finally to the raw path.
function projectLabel(project) {
  if (!project) return '';
  const name = String(project.name || '').trim();
  if (name) return name;
  const path = String(project.path || '').trim();
  if (!path) return '';
  const parts = path.split(/[\\/]+/).filter(Boolean);
  return parts.length ? parts[parts.length - 1] : path;
}

// renderWorkspaceBar paints the workspace row above the tabs: which project is
// active, its full path (click to copy) and its git branch. The branch chip is
// filled by refreshGitInfo, which is the only part that needs the server.
function renderWorkspaceBar(project) {
  const bar = $('workspaceBar');
  if (!bar) return;
  if (!project) {
    bar.classList.add('d-none');
    return;
  }
  bar.classList.remove('d-none');
  bar.style.setProperty('--pc', project.icon_color || '#6c8cff');

  const name = projectLabel(project);
  const path = project.path ? String(project.path) : '';
  $('workspaceName').textContent = name || 'project';
  $('workspaceName').title = name || '';

  const pathBtn = $('workspacePath');
  $('workspacePathText').textContent = path;
  pathBtn.title = path ? `${path}\nClick to copy` : '';
  pathBtn.disabled = !path;
  if (path) pathBtn.setAttribute('aria-label', `Project path ${path} (click to copy)`);
  else pathBtn.removeAttribute('aria-label');

  // Drop the branch of the previous project instead of showing it for a frame.
  if (state.git.projectId !== project.id) {
    state.git = { projectId: project.id, checked: false, isRepo: false, branch: '', detached: false, commit: '', seq: state.git.seq };
  }
  renderGitChip();
}

// renderGitChip draws the branch pill from state.git. The three states are a
// named branch, a detached HEAD (the short sha is the only label left) and a
// plain directory, which is muted rather than reported as an error.
function renderGitChip() {
  const chip = $('workspaceBranch');
  if (!chip) return;
  const git = state.git;
  const text = $('workspaceBranchText');
  chip.classList.remove('detached', 'no-git');
  if (!git.projectId || !git.checked) {
    chip.classList.add('d-none');
    return;
  }
  chip.classList.remove('d-none');
  if (!git.isRepo) {
    chip.classList.add('no-git');
    chip.title = 'This project is not a git repository';
    text.textContent = 'no git';
    return;
  }
  if (git.detached) {
    chip.classList.add('detached');
    text.textContent = git.commit || 'detached';
    chip.title = `Detached HEAD${git.commit ? ` at ${git.commit}` : ''}`;
    return;
  }
  text.textContent = git.branch || 'detached';
  chip.title = git.branch ? `On branch ${git.branch}` : '';
}

// refreshGitInfo re-reads the branch of a project. The server runs one bounded
// git probe, so this is cheap enough to call on every project switch and after
// each turn; a late response for another project is dropped.
async function refreshGitInfo(projectId) {
  if (!projectId || projectId !== state.activeProjectId) return;
  const seq = ++state.git.seq;
  const btn = $('workspaceRefreshBtn');
  if (btn) {
    btn.disabled = true;
    btn.classList.add('spinning');
  }
  try {
    const data = await api(`/api/projects/${projectId}/git`);
    if (seq !== state.git.seq || projectId !== state.activeProjectId) return;
    state.git = {
      projectId,
      checked: true,
      isRepo: !!data.is_repo,
      branch: data.branch || '',
      detached: !!data.detached,
      commit: data.commit || '',
      seq,
    };
    renderGitChip();
  } catch {
    // A project on an unmounted volume or without git must not break the bar:
    // keep the last known chip and let the user retry.
  } finally {
    if (seq === state.git.seq && btn) {
      btn.disabled = false;
      btn.classList.remove('spinning');
    }
  }
}

// refreshActiveGit is the turn hook: only a turn that ran in the visible
// project can have moved its branch.
function refreshActiveGit(view) {
  if (view && view.projectId && view.projectId !== state.activeProjectId) return;
  refreshGitInfo(state.activeProjectId);
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

function initUsageCard() {
  const card = $('usageDetails');
  if (!card) return;
  try { card.open = localStorage.getItem('forge.usageOpen') === '1'; } catch { /* ignore */ }
  card.addEventListener('toggle', () => {
    try { localStorage.setItem('forge.usageOpen', card.open ? '1' : '0'); } catch { /* ignore */ }
  });
  const pill = $('prunePill');
  if (pill) {
    pill.addEventListener('click', () => { card.open = true; });
    pill.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter' && e.key !== ' ') return;
      e.preventDefault();
      card.open = true;
      card.querySelector('summary')?.focus();
    });
  }
}

/* ── Sessions ────────────────────────────────────────────────────── */

async function selectProject(id, { autoCreate = false } = {}) {
  if (!id) return;
  const project = await openProject(id);
  if (!project) return;

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

// openProject makes a project active and loads its open chats without choosing
// a session. Switching project never stops a running agent: background views
// keep streaming and the server keeps working even if the browser goes away.
async function openProject(id) {
  if (!id) return null;
  const project = state.projects.find((p) => p.id === id);
  if (!project) return null;
  state.activeProjectId = id;
  state.activeProject = project;
  try { localStorage.setItem('forge.project', String(id)); } catch { /* ignore */ }
  renderProjects();
  renderWorkspaceBar(project);
  refreshGitInfo(id);

  try {
    const data = await api(`/api/projects/${id}/sessions`);
    state.sessions = data.sessions || [];
  } catch (err) {
    toast(err.message, 'error');
    state.sessions = [];
  }
  sortSessionsByOrder();
  renderTabs();
  updateControls();
  return project;
}

async function createSession({ silent = false } = {}) {
  if (!state.activeProjectId) {
    toast('Register or select a project first', 'error');
    return null;
  }
  try {
    const data = await api(`/api/projects/${state.activeProjectId}/sessions`, {
      method: 'POST',
      body: JSON.stringify({}),
    });
    state.sessions.push(data.session);
    persistTabOrder();
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
    renderSessionProviderOptions();
    loadModels(effectiveProviderID());
    // Opening a tab must never reorder the bar: update the entry where it is.
    const tabIndex = state.sessions.findIndex((s) => s.id === id);
    if (tabIndex >= 0) state.sessions[tabIndex] = { ...state.sessions[tabIndex], ...data.session };
    else state.sessions.push(data.session);

    // The session owns its view. Opening a tab only mounts that view; a run
    // that is still working in the background keeps streaming into it.
    const view = viewFor(id, { create: true });
    view.finishedUnseen = false;
    view.checklist = data.checklist || [];
    view.questionBatches = Array.isArray(data.question_batches) ? data.question_batches : [];
    view.compactions = Array.isArray(data.compactions) ? data.compactions : [];
    view.sessionTimeBase = Number(data.session.duration_ms || 0);
    if (!view.streaming) {
      view.turnBaseTime = view.sessionTimeBase;
      view.turnStartedAt = 0;
      view.turnElapsedMs = 0;
    }
    const seededRounds = countToolRounds(data.messages || []);
    const prune = loadPruneState(id);
    // The cached snapshot only applies while the history it described still
    // exists. Compaction can drop the session below it, so ignore it then.
    const usePrune = !!prune && prune.rounds <= seededRounds + 1;
    if (!view.streaming) {
      view.usage = {
        sessionIn: data.session.tokens_in || 0,
        sessionOut: data.session.tokens_out || 0,
        sessionCost: data.session.cost_usd || 0,
        projectCost: data.project.total_cost_usd || 0,
        contextTokens: estimateContext(data.messages || [], view.compactions),
        contextExact: false,
        cached: data.session.cached_tokens || 0,
        rounds: Math.max(seededRounds, usePrune ? prune.rounds : 0),
        prunedRounds: usePrune ? prune.prunedRounds : 0,
        prunedChars: usePrune ? prune.prunedChars : 0,
      };
    }
    localStorage.setItem(`forge.session.${data.project.id}`, String(id));
    updateProjectTotals(data.project);
    renderTabs();
    if (view.streaming) {
      // Re-attach: replay whatever the run produced while we were elsewhere.
      resumeSessionRun(id, view);
    } else {
      withView(view, () => renderSession(data.messages || []));
      if (view.el.children.length) hideEmpty();
      else showEmpty('chat');
    }
    mountView(view);
    const run = await activeRunFor(id);
    if (run) {
      setRunStatus(run);
      if (!view.streaming) attachSessionRun(id, view, 0);
    }
  } catch (err) {
    toast(err.message, 'error');
    if (err.status === 404) {
      dropView(id);
      state.sessions = state.sessions.filter((s) => s.id !== id);
      renderTabs();
      clearSession();
    }
  }
}

// activeRunFor asks the server whether a session still has a background agent.
async function activeRunFor(sessionId) {
  try {
    const data = await api(`/api/sessions/${sessionId}/run`);
    return data && data.state === 'running' ? data : null;
  } catch {
    return null;
  }
}

function clearSession() {
  const view = currentView();
  withView(view, () => {
    finalizeRound();
    stopTurnClock(false);
  });
  // A cleared screen has no active chat, so every panel falls back to its empty
  // state instead of keeping the numbers of the chat that was open.
  state.activeSessionId = null;
  state.session = null;
  if (state.activeProjectId) localStorage.removeItem(`forge.session.${state.activeProjectId}`);
  messagesEl().innerHTML = '';
  renderSessionProviderOptions();
  const defaultProvider = activeProvider();
  $('modelInput').value = (defaultProvider && defaultProvider.default_model) || state.config?.defaults?.model || '';
  setModeUI(state.config?.defaults?.mode || 'PLAN');
  renderTabs();
  renderInspector();
  renderChrome();
  showEmpty('session');
  document.title = 'Forge';
}

function tabOrderKey() {
  return `forge.tabOrder.${state.activeProjectId || 'none'}`;
}

// Tab order is a per-project preference. Without it the bar keeps whatever
// order the server returned (most recently updated first).
function sortSessionsByOrder() {
  let order = [];
  try {
    order = JSON.parse(localStorage.getItem(tabOrderKey()) || '[]');
  } catch {
    order = [];
  }
  if (!Array.isArray(order) || !order.length) return;
  const rank = new Map(order.map((id, i) => [Number(id), i]));
  state.sessions.sort((a, b) => {
    const ra = rank.has(a.id) ? rank.get(a.id) : Number.MAX_SAFE_INTEGER;
    const rb = rank.has(b.id) ? rank.get(b.id) : Number.MAX_SAFE_INTEGER;
    if (ra !== rb) return ra - rb;
    return b.id - a.id;
  });
}

function persistTabOrder() {
  if (!state.activeProjectId) return;
  try {
    localStorage.setItem(tabOrderKey(), JSON.stringify(state.sessions.map((s) => s.id)));
  } catch { /* ignore */ }
}

function moveTab(fromId, toId) {
  if (!fromId || !toId || fromId === toId) return;
  const from = state.sessions.findIndex((s) => s.id === fromId);
  const to = state.sessions.findIndex((s) => s.id === toId);
  if (from < 0 || to < 0) return;
  const [moved] = state.sessions.splice(from, 1);
  state.sessions.splice(to, 0, moved);
  persistTabOrder();
  renderTabs();
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
    tab.title = `${session.title || 'New Chat'} — drag to reorder, double-click to rename`;
    tab.draggable = true;
    tab.dataset.id = String(session.id);
    // Background agents stay visible: a spinner while the run is live and a dot
    // when it finished (or paused for questions) while the user was elsewhere.
    const sessionView = viewFor(session.id);
    const running = runIndex.has(session.id) || !!sessionView?.streaming;
    const unseen = !!sessionView?.finishedUnseen;
    if (running) {
      tab.classList.add('running');
      const dot = document.createElement('span');
      dot.className = 'tab-pulse';
      dot.title = 'Agent working in this chat';
      tab.appendChild(dot);
    } else if (unseen) {
      tab.classList.add('unseen');
      const dot = document.createElement('span');
      dot.className = 'tab-unseen-dot';
      dot.title = hasPendingQuestions(sessionView)
        ? 'Waiting for your answers — open to respond'
        : 'Finished while you were away — open to see the result';
      tab.appendChild(dot);
    }
    // Click only opens the chat; it never moves the tab.
    tab.addEventListener('click', () => {
      if (session.id !== state.activeSessionId) loadSession(session.id);
    });

    tab.addEventListener('dragstart', (e) => {
      dragTabId = session.id;
      tab.classList.add('dragging');
      e.dataTransfer.effectAllowed = 'move';
      try {
        e.dataTransfer.setData('text/plain', String(session.id));
      } catch { /* ignore */ }
    });
    tab.addEventListener('dragend', () => {
      dragTabId = null;
      tab.classList.remove('dragging');
      bar.querySelectorAll('.session-tab').forEach((el) => el.classList.remove('drag-over'));
    });
    tab.addEventListener('dragover', (e) => {
      if (!dragTabId || dragTabId === session.id) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      tab.classList.add('drag-over');
    });
    tab.addEventListener('dragleave', () => tab.classList.remove('drag-over'));
    tab.addEventListener('drop', (e) => {
      e.preventDefault();
      tab.classList.remove('drag-over');
      const from = dragTabId || Number(e.dataTransfer.getData('text/plain') || 0);
      dragTabId = null;
      moveTab(from, session.id);
    });

    const title = document.createElement('span');
    title.className = 'tab-title';
    title.textContent = session.title || 'New Chat';
    title.addEventListener('dblclick', (e) => {
      e.stopPropagation();
      openRenameTabModal(session);
    });
    tab.appendChild(title);

    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'tab-close';
    close.title = 'Close chat (keeps it in Manage chats)';
    close.innerHTML = '<i class="bi bi-x-lg"></i>';
    close.addEventListener('click', (e) => {
      e.stopPropagation();
      closeSession(session.id);
    });
    tab.appendChild(close);
    bar.appendChild(tab);
  }
}

// Open the rename tab modal for the given session.
function openRenameTabModal(session) {
  const input = $('renameTabInput');
  const sessionIdInput = $('renameTabSessionId');
  if (!input || !sessionIdInput) return;
  sessionIdInput.value = String(session.id);
  input.value = session.title || '';
  if (renameTabModal) {
    renameTabModal.show();
    // Focus input after modal is shown
    const modalEl = $('renameTabModal');
    modalEl.addEventListener('shown.bs.modal', () => input.focus(), { once: true });
  }
}

async function closeSession(id) {
  // A background agent keeps the chat open server-side: closing the tab is a
  // UI preference, so the run is left alone and the tab simply disappears.
  try {
    await api(`/api/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ is_open: false }) });
    state.sessions = state.sessions.filter((s) => s.id !== id);
    const all = state.allSessions.find((s) => s.id === id);
    if (all) all.is_open = false;
    if (id === state.activeSessionId) localStorage.removeItem(`forge.session.${state.activeProjectId}`);
    persistTabOrder();
    renderTabs();
    if (id === state.activeSessionId) {
      if (state.sessions.length) await loadSession(state.sessions[0].id);
      else clearSession();
    }
  } catch (err) {
    toast(err.message, 'error');
  }
}

// syncSession merges a session update into the right place. Only the chat on
// screen owns state.session: a background agent updating its own row (tokens,
// title, cost) must not overwrite the chat the user is reading, nor push a chat
// from another project into this project's tab bar.
function syncSession(session) {
  if (!session) return;
  const sameChat = !state.session || state.session.id === session.id;
  if (sameChat && (session.is_open || session.id === state.activeSessionId || !session.id)) {
    state.session = { ...(state.session || {}), ...session };
  }
  const idx = state.sessions.findIndex((s) => s.id === session.id);
  if (idx >= 0) {
    state.sessions[idx] = { ...state.sessions[idx], ...session };
  } else if (session.is_open && session.project_id === state.activeProjectId) {
    state.sessions.push(session);
  }
  const allIdx = state.allSessions.findIndex((s) => s.id === session.id);
  if (allIdx >= 0) state.allSessions[allIdx] = { ...state.allSessions[allIdx], ...session };
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

// messageHasToolCalls reports whether a persisted assistant message is an
// intermediate tool round. Only a final, content-only message is a real plan.
function messageHasToolCalls(msg) {
  try {
    const calls = Array.isArray(msg.tool_calls) ? msg.tool_calls : JSON.parse(msg.tool_calls || '[]');
    return Array.isArray(calls) && calls.length > 0;
  } catch {
    return false;
  }
}

function renderSession(messages) {
  const container = messagesEl();
  const view = currentView();
  state.currentRound = null;
  state.toolCards = new Map();
  state.messageIndex = -1;
  state.answerIndex = -1;
  if (view) view.renderedQuestions = new Set();
  container.innerHTML = '';
  let hasContent = false;
  // Compaction cards are drawn where the context boundary sits: right before
  // the first message that still goes to the model. Older summaries stay in the
  // transcript, so the history never disappears from view.
  const pending = (view?.compactions || []).slice().sort((a, b) => a.start_message_id - b.start_message_id);
  const drawCompactionsUpTo = (messageId) => {
    while (pending.length && pending[0].start_message_id <= messageId) {
      appendCompactionCard(pending.shift());
      hasContent = true;
    }
  };
  for (const msg of messages) {
    drawCompactionsUpTo(Number(msg.id) || 0);
    if (msg.role === 'user') {
      appendUserMessage(msg.content);
      hasContent = true;
    } else if (msg.role === 'assistant') {
      if (renderAssistantHistory(msg)) hasContent = true;
    }
  }
  while (pending.length) {
    appendCompactionCard(pending.shift());
    hasContent = true;
  }
  renderSessionProviderOptions();
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
  if (state.session?.mode === 'PLAN' && last?.role === 'assistant' &&
      (last.content || '').trim() && !messageHasToolCalls(last) && !hasPendingQuestions(view)) {
    addPlanReadyCard();
  }
  // Batches whose tool call never made it to the transcript (a run interrupted
  // between persisting the questions and the tool message, or history compacted
  // afterwards) still need a card, otherwise the user would never see them.
  for (const batch of (view?.questionBatches || [])) {
    if (view.renderedQuestions && view.renderedQuestions.has(batch.id)) continue;
    renderQuestionCard(container, batch);
  }
  maybeScroll(true);
}

// appendCompactionCard renders a compaction marker inside the transcript. The
// summary is what the model saw instead of the messages above it; the messages
// themselves stay visible, so the card is a marker, not a replacement.
function appendCompactionCard(compaction) {
  const wrap = document.createElement('div');
  wrap.className = 'compaction';
  wrap.dataset.compaction = String(compaction.id);
  const removed = Number(compaction.removed) || 0;
  wrap.innerHTML = `
    <div class="compaction-head">
      <div class="compaction-icon"><i class="bi bi-archive"></i></div>
      <div class="compaction-body">
        <strong>Context compacted</strong>
        <span>${removed > 0
          ? `${formatNum(removed)} earlier message${removed === 1 ? '' : 's'} summarized for the model`
          : 'Earlier messages summarized for the model'}. The full history is still here.</span>
      </div>
      <button type="button" class="compaction-toggle" aria-expanded="false">
        <span>Summary</span><i class="bi bi-chevron-down"></i>
      </button>
    </div>
    <div class="compaction-summary md" hidden></div>`;
  const summary = wrap.querySelector('.compaction-summary');
  const toggle = wrap.querySelector('.compaction-toggle');
  const text = String(compaction.summary || '').replace(/^Conversation summary \(context compacted\):\s*/i, '');
  toggle.addEventListener('click', () => {
    const open = summary.hasAttribute('hidden');
    if (open && !summary.dataset.rendered) {
      renderMarkdownInto(summary, text, true);
      summary.dataset.rendered = '1';
    }
    summary.toggleAttribute('hidden', !open);
    toggle.setAttribute('aria-expanded', String(open));
    wrap.classList.toggle('open', open);
  });
  messagesEl().appendChild(wrap);
}

function removePlanReadyCard() {
  const existing = messagesEl().querySelector('.plan-ready');
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
  messagesEl().appendChild(card);
}

/* ── Clarifying questions (ask_user) ─────────────────────────────── */

function hasPendingQuestions(view) {
  return (view?.questionBatches || []).some((batch) => !batch.answered_at);
}

function batchForCall(view, callId) {
  if (!view || !callId) return null;
  return (view.questionBatches || []).find((batch) => batch && batch.tool_call_id === callId) || null;
}

function questionOptionLabel(question, id) {
  const option = (question.options || []).find((opt) => opt.id === id);
  return option ? option.label : '';
}

function questionAnswerText(question) {
  const parts = [];
  for (const id of question.answer_option_ids || []) {
    const label = questionOptionLabel(question, id);
    if (label) parts.push(label);
  }
  const custom = (question.answer_text || '').trim();
  if (custom) parts.push(custom);
  return parts.join('; ');
}

// renderQuestionCard draws one ask_user batch. A pending batch is interactive;
// an answered batch (or readOnly) is a compact record of the chosen answers.
function renderQuestionCard(host, batch, { readOnly = false, replace = null } = {}) {
  const view = currentView();
  if (view && batch.id) {
    if (!(view.renderedQuestions instanceof Set)) view.renderedQuestions = new Set();
    view.renderedQuestions.add(batch.id);
  }
  const sessionId = view?.sessionId || state.activeSessionId;
  const answered = !!batch.answered_at;
  const locked = readOnly || answered;

  const card = document.createElement('div');
  card.className = 'question-card' + (locked ? ' is-answered' : '');
  card.dataset.batch = String(batch.id);

  const head = document.createElement('div');
  head.className = 'question-head';
  head.innerHTML =
    '<span class="question-head-icon"><i class="bi bi-question-circle"></i></span>' +
    '<div class="question-head-text"><strong>Forge needs your input</strong><span class="question-head-sub"></span></div>';
  head.querySelector('.question-head-text strong').textContent = locked
    ? 'Question answered'
    : 'Forge needs your input';
  head.querySelector('.question-head-sub').textContent = locked
    ? 'Your answers are part of the conversation'
    : `${batch.questions.length} question${batch.questions.length === 1 ? '' : 's'} waiting for you`;
  if (!locked) {
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'question-close';
    close.title = 'Dismiss';
    close.setAttribute('aria-label', 'Dismiss questions');
    close.innerHTML = '&times;';
    close.addEventListener('click', () => card.remove());
    head.appendChild(close);
  }

  const body = document.createElement('div');
  body.className = 'question-body';
  const selections = new Map();
  const customs = new Map();

  for (const question of batch.questions || []) {
    const item = document.createElement('div');
    item.className = 'question-item';
    item.dataset.question = String(question.id);

    const title = document.createElement('div');
    title.className = 'question-title';
    title.textContent = question.text;
    item.appendChild(title);

    const context = (question.context || '').trim();
    if (context) {
      const note = document.createElement('div');
      note.className = 'question-context';
      note.textContent = context;
      item.appendChild(note);
    }

    if (locked) {
      const answer = document.createElement('div');
      answer.className = 'question-answer';
      answer.textContent = questionAnswerText(question) || '(no answer)';
      item.appendChild(answer);
      body.appendChild(item);
      continue;
    }

    selections.set(question.id, new Set(question.answer_option_ids || []));
    customs.set(question.id, question.answer_text || '');

    const options = document.createElement('div');
    options.className = 'question-options';
    let custom = null;

    for (const option of question.options || []) {
      const label = document.createElement('label');
      label.className = 'question-option';
      const input = document.createElement('input');
      input.type = question.kind === 'multi' ? 'checkbox' : 'radio';
      input.name = `question-${batch.id}-${question.id}`;
      input.value = String(option.id);
      input.checked = selections.get(question.id).has(option.id);
      const text = document.createElement('span');
      text.className = 'question-option-text';
      const labelText = document.createElement('span');
      labelText.className = 'question-option-label';
      labelText.textContent = option.label;
      text.appendChild(labelText);
      if ((option.description || '').trim()) {
        const desc = document.createElement('span');
        desc.className = 'question-option-desc';
        desc.textContent = option.description;
        text.appendChild(desc);
      }
      label.appendChild(input);
      label.appendChild(text);
      input.addEventListener('change', () => {
        const chosen = selections.get(question.id);
        if (question.kind === 'multi') {
          if (input.checked) chosen.add(option.id);
          else chosen.delete(option.id);
        } else if (input.checked) {
          chosen.clear();
          chosen.add(option.id);
          if (custom) {
            custom.value = '';
            customs.set(question.id, '');
          }
        }
        item.classList.remove('needs-answer');
      });
      options.appendChild(label);
    }
    item.appendChild(options);

    const customWrap = document.createElement('div');
    customWrap.className = 'question-custom';
    custom = document.createElement('input');
    custom.type = 'text';
    custom.className = 'question-custom-input';
    custom.placeholder = 'Or type your own answer…';
    custom.setAttribute('aria-label', `Your own answer for: ${question.text}`);
    custom.value = customs.get(question.id);
    custom.addEventListener('input', () => {
      customs.set(question.id, custom.value);
      if (question.kind !== 'multi' && custom.value.trim()) {
        selections.get(question.id).clear();
        for (const input of options.querySelectorAll('input')) input.checked = false;
      }
      item.classList.remove('needs-answer');
    });
    customWrap.appendChild(custom);
    item.appendChild(customWrap);
    body.appendChild(item);
  }

  const foot = document.createElement('div');
  foot.className = 'question-actions';
  if (!locked) {
    const hint = document.createElement('span');
    hint.className = 'question-hint';
    hint.textContent = 'Choose an option or write your own answer for every question.';
    const send = document.createElement('button');
    send.type = 'button';
    send.className = 'btn btn-accent';
    send.textContent = 'Send answers';
    send.addEventListener('click', () => {
      submitQuestionAnswers(batch, selections, customs, card, send, sessionId);
    });
    foot.appendChild(hint);
    foot.appendChild(send);
  }

  card.appendChild(head);
  card.appendChild(body);
  if (!locked) card.appendChild(foot);
  if (replace && replace.parentElement) replace.replaceWith(card);
  else host.appendChild(card);
  return card;
}

async function submitQuestionAnswers(batch, selections, customs, card, button, sessionId) {
  if (state.streaming) {
    toast('Wait for the current turn to finish before answering.', 'error');
    return;
  }
  const answers = [];
  let missing = false;
  for (const question of batch.questions || []) {
    const optionIds = Array.from(selections.get(question.id) || []);
    const text = (customs.get(question.id) || '').trim();
    if (!optionIds.length && !text) {
      missing = true;
      const item = card.querySelector(`.question-item[data-question="${question.id}"]`);
      if (item) item.classList.add('needs-answer');
    }
    answers.push({ question_id: question.id, option_ids: optionIds, text });
  }
  if (missing) {
    toast('Answer every question before sending.', 'error');
    return;
  }
  button.disabled = true;
  try {
    const data = await api(`/api/sessions/${sessionId}/questions/${batch.id}/answers`, {
      method: 'POST',
      body: JSON.stringify({ answers }),
    });
    const view = viewFor(sessionId, { create: true });
    if (view) {
      if (!Array.isArray(view.questionBatches)) view.questionBatches = [];
      const index = view.questionBatches.findIndex((b) => b.id === batch.id);
      if (index >= 0) view.questionBatches[index] = data.batch;
      else view.questionBatches.push(data.batch);
      withView(view, () => {
        renderQuestionCard(card.parentElement || view.el, data.batch, { readOnly: true, replace: card });
      });
    } else {
      card.remove();
    }
    await runTurn({ content: data.summary, userText: data.summary, sessionId });
  } catch (err) {
    button.disabled = false;
    toast(err.message, 'error');
  }
}

function appendUserMessage(text, attachments = []) {
  const parsed = splitAttachmentBlock(text);
  const body = parsed.text;
  const files = attachments.length ? attachments.slice() : parsed.files;
  const wrap = document.createElement('div');
  wrap.className = 'msg msg-user';
  const bubble = document.createElement('div');
  bubble.className = 'user-bubble';
  if (body) bubble.textContent = body;
  if (files.length) {
    const chips = document.createElement('div');
    chips.className = 'bubble-files';
    for (const path of files) {
      const chip = document.createElement('span');
      chip.className = 'bubble-file';
      const icon = document.createElement('i');
      icon.className = 'bi bi-file-earmark-code';
      const label = document.createElement('span');
      label.textContent = path;
      chip.appendChild(icon);
      chip.appendChild(label);
      chips.appendChild(chip);
    }
    bubble.appendChild(chips);
  }
  const copyText = [body, ...files.map((f) => '@' + f)].filter(Boolean).join('\n');
  const btn = makeCopyButton('msg-copy', 'Copy message', (b) => copyToClipboard(copyText, b));
  wrap.appendChild(btn);
  wrap.appendChild(bubble);
  messagesEl().appendChild(wrap);
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
  messagesEl().appendChild(round.el);
  if (hasReasoning) {
    const card = addReasoningCard(round, msg.reasoning);
    card.title.textContent = 'Thought process';
    // A reasoning-only turn has no visible answer, so surface it.
    if (!hasContent) card.el.open = true;
  }
  const askedBatches = [];
  for (const call of calls) {
    const name = call.function?.name || call.name || '';
    if (name === 'ask_user') {
      const batch = batchForCall(currentView(), call.id);
      if (batch) {
        askedBatches.push(batch);
        continue;
      }
    }
    addHistoricalToolCall(round, call);
  }
  if (hasContent) {
    round.rawContent = msg.content;
    const contentEl = ensureContentEl(round);
    renderMarkdownInto(contentEl, msg.content, true);
  }
  // The ask_user card belongs after the assistant's explanatory text, not
  // above it: the questions are the last thing the user should read.
  for (const batch of askedBatches) {
    renderQuestionCard(round.questions, batch);
  }
  if (Number(msg.duration_ms) > 0) {
    addHistoricalTurnTime(round, Number(msg.duration_ms));
  }
  return true;
}

function makeRoundElement() {
  const el = document.createElement('div');
  el.className = 'assistant-round msg';
  el.innerHTML = '<div class="round-avatar">F</div><div class="round-main"><div class="round-events"></div><div class="round-questions"></div><div class="round-footer"></div></div>';
  return {
    el,
    main: el.querySelector('.round-main'),
    events: el.querySelector('.round-events'),
    questions: el.querySelector('.round-questions'),
    footer: el.querySelector('.round-footer'),
    reasoning: null,
    contentEl: null,
    copyBtn: null,
    jumpBtn: null,
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
  const attachments = state.attachments.slice();
  if (!text && !attachments.length) return;
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
  clearAttachments();
  resizePrompt();
  updateSendState();
  await runTurn({ content: text, userText: text, attachments });
}

// continueTurn resumes a paused turn from its checkpoint. The server persists
// the synthetic user message, so the transcript stays consistent.
async function continueTurn() {
  if (state.streaming || !state.session) return;
  await runTurn({ isContinue: true, userText: 'Continue from where you left off.' });
}

async function runTurn({ content = '', isContinue = false, userText = content, attachments = [], sessionId = state.session?.id || state.activeSessionId }) {
  const view = viewFor(sessionId, { create: true });
  if (!view) return;
  withView(view, () => {
    removePlanReadyCard();
    updateSendState();
    appendUserMessage(userText, attachments);
    if (viewIsActive(view)) hideEmpty();
    view.messageIndex = -1;
    view.answerIndex = -1;
    maybeScroll(true);
  });

  withView(view, () => {
    state.streaming = true;
    state.stopRequested = false;
    state.sawCancelled = false;
    state.turnHadContent = false;
    state.turnHadError = false;
    state.turnReasoningOnly = false;
    state.turnRound = 0;
    state.turnLimit = 0;
    state.turnSegment = 1;
    state.turnSegmentSize = 0;
    setStreamingUI();
    startTurnClock();
    beginRound(0);
  });
  const target = state.session?.id === sessionId ? state.session : sessionById(sessionId);
  const modeAtSend = target?.mode === 'BUILD' ? 'BUILD' : 'PLAN';
  setRunStatus({ session_id: sessionId, state: 'running', mode: modeAtSend });
  renderTabs();

  try {
    const payload = isContinue
      ? { session_id: sessionId, continue: true }
      : { session_id: sessionId, content, attachments };
    const res = await fetch('/api/chat/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
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
    view.attached = true;
    await readSSE(res.body, view);
  } catch (err) {
    if (err.preflight) {
      withView(view, () => {
        const users = messagesEl().querySelectorAll('.msg-user');
        if (users.length) users[users.length - 1].remove();
        if (!messagesEl().querySelector('.msg-user, .assistant-round') && viewIsActive(view)) {
          showEmpty('chat');
        }
      });
      if (viewIsActive(view) && !isContinue && !$('promptInput').value.trim()) {
        $('promptInput').value = content;
        resizePrompt();
      }
      updateSendState();
      toast(err.message || 'Message was not sent', 'error');
      runIndex.delete(sessionId);
    } else if (!view.sawCancelled) {
      withView(view, () => {
        view.turnHadError = true;
        addNotice(err.message || 'Request failed', 'error');
      });
      toast(err.message || 'Request failed', 'error');
    }
  } finally {
    view.attached = false;
    finishTurnView(view, modeAtSend);
  }
}

// finishTurnView settles the UI of a finished turn. It runs identically for a
// foreground turn and for a background agent that kept working after the user
// navigated away, so both end in the same state.
function finishTurnView(view, modeAtSend = '') {
  withView(view, () => {
    stopTurnClock(true);
    finalizeRound();
    state.streaming = false;
    state.stopRequested = false;
    state.turnRound = 0;
    state.turnLimit = 0;
    state.turnSegment = 1;
    state.turnSegmentSize = 0;
    if (!view.sawCancelled && !view.turnHadError && !view.turnReasoningOnly &&
        modeAtSend === 'PLAN' && view.turnHadContent && !hasPendingQuestions(view)) {
      addPlanReadyCard();
    }
  });
  // The chrome is repainted for every finished run, foreground or background:
  // when a background agent ends, the Stop button of the chat on screen is
  // re-derived from the run index instead of keeping its previous state.
  const onScreen = viewIsActive(view);
  const session = state.sessions.find((s) => s.id === view.sessionId);
  const title = session?.title || 'Background agent';
  // The system notification is driven by what the user can actually see, not by
  // which chat is mounted: a turn that ends in the chat on screen still deserves
  // a notification when the whole tab is in the background. notifyBrowser owns
  // that check, so it is called for every outcome below.
  if (onScreen) {
    updateControls();
    maybeScroll(true);
    if (view.turnHadError) {
      // The transcript already shows the failure inline; no toast on top of it.
    }
  } else if (view.turnHadError) {
    toast(`${title} failed — open the chat for details`, 'error');
  } else if (!view.sawCancelled) {
    view.finishedUnseen = true;
    if (hasPendingQuestions(view)) {
      toast(`${title} needs your answers`, 'success');
    } else {
      toast(`${title} finished`, 'success');
    }
  }
  if (view.turnHadError) {
    notifyBrowser(title, 'failed — open the chat for details');
  } else if (view.sawCancelled) {
    // A stop requested by the user needs no alert.
  } else if (hasPendingQuestions(view)) {
    notifyBrowser(title, 'needs your answers');
  } else {
    notifyBrowser(title, 'finished');
  }
  runIndex.delete(view.sessionId);
  updateSessionTabCosts();
  renderTabs();
  // Always repaint the chrome, even for a background run: the chat on screen
  // derives Stop and the spinner from the run index, not from this view.
  scheduleChrome();
}

// attachSessionRun re-attaches a session view to whatever the server is still
// running. The turn never depended on the browser, so this only replays the
// events produced while the view was detached.
async function attachSessionRun(sessionId, view, after = 0) {
  const v = view || viewFor(sessionId, { create: true });
  if (!v || v.attached) return;
  v.attached = true;
  v.streaming = true;
  v.stopRequested = false;
  v.finishedUnseen = false;
  const modeAtSend = state.sessions.find((s) => s.id === sessionId)?.mode || 'PLAN';
  withView(v, () => {
    if (viewIsActive(v)) setStreamingUI();
    startTurnClock();
  });
  try {
    const res = await fetch(`/api/sessions/${sessionId}/stream?after=${Number(after || 0)}`, {
      headers: { Accept: 'text/event-stream' },
    });
    if (res.status === 204) return;
    if (!res.ok || !res.body) throw new Error(`Could not re-attach to the agent (${res.status})`);
    await readSSE(res.body, v);
  } catch (err) {
    withView(v, () => addNotice(err.message || 'Lost the connection to the agent', 'error'));
  } finally {
    v.attached = false;
    finishTurnView(v, modeAtSend);
  }
}

// resumeSessionRun continues a view that was streaming before a navigation or a
// reload, replaying only the events it has not seen yet.
function resumeSessionRun(sessionId, view) {
  return attachSessionRun(sessionId, view, view ? view.seq : 0);
}

async function compactConversation() {
  if (state.streaming || !state.session) return;
  const prompt = $('promptInput');
  prompt.value = '';
  resizePrompt();
  updateSendState();

  state.streaming = true;
  state.abort = new AbortController();
  setStreamingUI();
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
    state.answerIndex = -1;
    const view = currentView();
    if (view) view.compactions = Array.isArray(data.compactions) ? data.compactions : [];
    // The transcript is re-rendered in full: compaction only shrinks the
    // context sent to the model, the history stays readable.
    renderSession(data.messages || []);
    state.usage.contextTokens = estimateContext(data.messages || [], view?.compactions || []);
    state.usage.contextExact = false;
    updateUsageUI();
    toast(`Context compacted — ${data.removed} earlier messages summarized`, 'success');
  } catch (err) {
    if (err.name === 'AbortError') {
      addNotice('Stopped', 'stopped');
    } else {
      toast(err.message || 'Could not compact the conversation', 'error');
    }
  } finally {
    state.streaming = false;
    state.abort = null;
    setStreamingUI();
    updateControls();
  }
}

async function readSSE(body, view = null) {
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
      handleSSEBlock(block, view);
    }
  }
  buffer += decoder.decode();
  if (buffer.trim()) handleSSEBlock(buffer, view);
}

function handleSSEBlock(block, view = null) {
  const dataLines = [];
  let seq = 0;
  for (const line of block.split('\n')) {
    if (line.startsWith('data:')) dataLines.push(line.slice(5).replace(/^ /, ''));
    else if (line.startsWith('id:')) seq = Number(line.slice(3).trim()) || 0;
  }
  if (!dataLines.length) return;
  let ev;
  try {
    ev = JSON.parse(dataLines.join('\n'));
  } catch {
    return;
  }
  // Remember the last sequence seen so a re-attach replays only what is new.
  if (view && seq > view.seq) view.seq = seq;
  withView(view, () => handleEvent(ev));
}

// handleEvent mutates the view selected by withView, so events from a
// background agent never touch the transcript on screen.
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
      // ask_user renders as a question card on the `questions` event instead of
      // a generic tool row.
      if (ev.name === 'ask_user') break;
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
      if (ev.name === 'ask_user') break;
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
    case 'questions': {
      const batch = ev.question_batch;
      if (!batch) break;
      const round = ensureRound();
      removeThinking(round);
      const view = currentView();
      // A reattach after a reload can replay this event while the card is
      // already on screen from the session payload; do not render it twice.
      const alreadyRendered = !!(view?.renderedQuestions && view.renderedQuestions.has(batch.id));
      if (!Array.isArray(state.questionBatches)) state.questionBatches = [];
      const index = state.questionBatches.findIndex((b) => b.id === batch.id);
      if (index >= 0) state.questionBatches[index] = batch;
      else state.questionBatches.push(batch);
      if (!alreadyRendered) {
        renderQuestionCard(round.questions, batch);
        round.hasEvents = true;
        maybeScroll();
      }
      break;
    }
    case 'session':
      if (ev.session) {
        syncSession(ev.session);
        // Only the chat on screen owns the window title: a background agent
        // must not rename the tab of the chat the user is reading.
        if (ev.session.title && viewIsActive(currentView())) {
          document.title = `${ev.session.title} · Forge`;
        }
      }
      break;
    case 'done':
      if (ev.session) syncSession(ev.session);
      state.turnReasoningOnly = !!ev.reasoning_only;
      stopTurnClock(true);
      // A turn may have created commits, branches or a new repository in the
      // visible project, so the workspace bar re-reads it once it settles.
      refreshActiveGit(currentView());
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
      refreshActiveGit(currentView());
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
  messagesEl().appendChild(round.el);
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

// The turn clock is owned by the view that started it. The interval closes over
// that view instead of reading the proxied state, so a background agent's timer
// can never tick (or paint) the elapsed time of the chat on screen.
function startTurnClock() {
  stopTurnClock(false);
  const view = currentView();
  if (!view) return;
  const persisted = Math.max(
    Number(sessionById(view.sessionId)?.duration_ms || 0),
    Number(view.sessionTimeBase || 0)
  );
  view.turnBaseTime = persisted;
  view.sessionTimeBase = persisted;
  view.turnStartedAt = Date.now();
  view.turnElapsedMs = 0;
  if (view.turnClock) clearInterval(view.turnClock);
  view.turnClock = setInterval(() => tickTurnClock(view), 500);
  scheduleInspector(view);
}

function tickTurnClock(view) {
  if (!view || !view.turnStartedAt) return;
  view.turnElapsedMs = Date.now() - view.turnStartedAt;
  if (view.turnTimerEl) view.turnTimerEl.textContent = formatElapsed(view.turnElapsedMs);
  scheduleInspector(view);
}

function stopTurnClock(finalize = true) {
  const view = currentView();
  if (!view) return;
  if (view.turnClock) {
    clearInterval(view.turnClock);
    view.turnClock = null;
  }
  if (view.turnStartedAt) {
    view.turnElapsedMs = Date.now() - view.turnStartedAt;
    view.turnStartedAt = 0;
  }
  if (finalize && view.turnTimerEl) {
    view.turnTimerEl.textContent = formatElapsed(view.turnElapsedMs);
    view.turnTimerEl = null;
  }
  if (finalize && view.currentRound) {
    view.currentRound.el.classList.remove('is-working');
  }
  if (finalize) {
    const persisted = Number(sessionById(view.sessionId)?.duration_ms || 0);
    view.sessionTimeBase = Math.max(view.turnBaseTime + view.turnElapsedMs, persisted);
  }
  scheduleInspector(view);
}

// sessionElapsedMS is the working time of one session, live while it streams.
function sessionElapsedMS(view) {
  if (!view) return 0;
  const stored = Number(sessionById(view.sessionId)?.duration_ms || 0);
  let total = Math.max(Number(view.sessionTimeBase || 0), stored);
  if (view.streaming && view.turnStartedAt) {
    total = view.turnBaseTime + (Date.now() - view.turnStartedAt);
  }
  return Math.max(0, total);
}

// updateSessionTimeUI only schedules a repaint: the shared panel is painted from
// the active view alone, so a background agent's clock cannot overwrite it.
function updateSessionTimeUI() {
  scheduleInspector(currentView());
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
    round.contentEl.className = 'md mt-4';
    // Questions are rendered after the assistant text, even when the content
    // arrives before or after the `questions` event.
    const anchor = round.questions || round.footer;
    if (anchor) round.main.insertBefore(round.contentEl, anchor);
    else round.main.appendChild(round.contentEl);
    addRoundFooterActions(round);
  }
  return round.contentEl;
}

// Assistant responses get their footer actions once the markdown block exists:
// copy plus a jump back to the top of the answer. A long turn can leave the
// reader hundreds of lines below the final text, and that text is the point of
// the answer, so both live next to the elapsed time. The copy handler reads
// round.rawContent at click time so it always copies the latest text.
function addRoundFooterActions(round) {
  if (!round || round.copyBtn || !round.contentEl) return;
  const copyBtn = makeCopyButton('round-copy', 'Copy response', (b) => copyToClipboard(round.rawContent, b));
  const jumpBtn = makeFooterAction('round-jump', 'Go to the start of this answer', 'bi-arrow-up-circle', () => {
    scrollToAnswerStart(round.contentEl);
  });
  if (round.footer) {
    round.footer.insertBefore(jumpBtn, round.footer.firstChild);
    round.footer.insertBefore(copyBtn, round.footer.firstChild);
  } else {
    round.main.appendChild(copyBtn);
    round.main.appendChild(jumpBtn);
  }
  round.copyBtn = copyBtn;
  round.jumpBtn = jumpBtn;
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
  messagesEl().appendChild(div);
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
  const host = round ? round.events : messagesEl();
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
  // The round counter is shared chrome, so it always describes the chat on
  // screen: it reads the active view directly instead of the render context.
  const label = $('streamLabel');
  if (!label) return;
  const view = activeView();
  const round = view ? view.turnRound : 0;
  const limit = view ? view.turnLimit : 0;
  const segment = view ? view.turnSegment : 1;
  const segmentSize = view ? view.turnSegmentSize : 0;
  if (round > 0 && limit > 0) {
    let text = `round ${round}/${limit}`;
    const segments = segmentSize > 0 ? Math.ceil(limit / segmentSize) : 1;
    if (segments > 1 && segment > 1) {
      text += ` · segment ${segment}/${segments}`;
    }
    label.textContent = text;
  } else if (round > 0) {
    // No configured ceiling: report progress without an invented denominator.
    label.textContent = `round ${round} · unlimited`;
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
    symbol_lookup: '🧩',
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
    case 'symbol_lookup':
      return `Looking up symbol ${truncate(String(args.query || ''), 42)}${args.kind ? ' · ' + args.kind : ''}`;
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
  if (name === 'symbol_lookup' && args.query) {
    return `query: ${args.query}${args.kind ? `\nkind: ${args.kind}` : ''}`;
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

// The Inspector (usage, cost, files, checklist, session time) is shared chrome,
// so it belongs to the chat on screen. Handlers never paint it: they mutate the
// state of the view that produced the event and ask for a repaint. renderInspector
// is the only writer and it always reads the active view, so a background agent
// cannot leak its numbers into what the user is reading — and any element added
// here later inherits that isolation for free instead of needing a guard.
let inspectorFrame = 0;

// scheduleInspector coalesces repaints and silently drops those of agents that
// are not on screen.
function scheduleInspector(view) {
  if (view && !viewIsActive(view)) return;
  if (!view && renderView) return;
  if (inspectorFrame) return;
  inspectorFrame = requestAnimationFrame(() => {
    inspectorFrame = 0;
    renderInspector();
  });
}

function renderInspector() {
  const view = activeView();
  paintUsage(view);
  paintSessionTime(view);
  paintFiles(view);
  paintChecklist(view);
  // The card state depends on the run lifecycle as well as on the counters, so
  // it is repainted from here and from the chrome.
  applyChecklistState(view);
}

function paintUsage(view) {
  const usage = view ? view.usage : EMPTY_USAGE;
  const session = view ? sessionById(view.sessionId) : null;
  const limit = contextLimit(session?.model || state.config?.defaults?.model);
  const used = Math.max(0, usage.contextTokens || 0);
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  $('ctxBar').style.width = pct.toFixed(1) + '%';
  $('contextPct').textContent = (pct < 1 && pct > 0 ? '<1' : Math.round(pct)) + '%';
  $('contextMeta').textContent =
    `${usage.contextExact ? '' : 'est. '}${formatNum(used)} / ${formatLimit(limit)} tokens`;
  $('contextMeta').title =
    `${used} / ${limit} tokens in the current context (prompt + response). ` +
    'Session totals in the accordion below add every model round.';
  $('tokensIn').textContent = formatNum(usage.sessionIn);
  $('tokensOut').textContent = formatNum(usage.sessionOut);
  $('cachedTokens').textContent = formatNum(usage.cached);
  const cachePct = usage.sessionIn > 0
    ? Math.min(100, (usage.cached / usage.sessionIn) * 100)
    : 0;
  $('cachedTokens').parentElement.title =
    `Cumulative prompt tokens served from the provider cache (${cachePct.toFixed(0)}% of session input). ` +
    'Cache hits require an identical prompt prefix and the same upstream provider.';
  $('sessionCost').textContent = formatCost(usage.sessionCost);
  $('projectCost').textContent = formatCost(usage.projectCost);
  paintPrune(usage);
  const status = $('sessionStatus');
  if (status) status.textContent = session ? formatLimit(limit) + ' ctx' : '';
}

// paintSessionTime shows the working time of the chat on screen. Its live clock
// keeps running on the view, so switching back picks the total up again.
function paintSessionTime(view) {
  const el = $('sessionTime');
  if (!el) return;
  const total = sessionElapsedMS(view);
  el.textContent = total > 0 ? formatElapsed(total) : '0s';
}

function paintFiles(view) {
  const list = $('fileList');
  if (!list) return;
  const files = view ? view.files : new Map();
  list.innerHTML = '';
  $('fileCount').textContent = String(files.size);
  if (!files.size) {
    const empty = document.createElement('div');
    empty.className = 'file-empty';
    empty.textContent = 'Files read or modified in this session appear here.';
    list.appendChild(empty);
    return;
  }
  for (const [path, action] of files) {
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
  if (ev.history_rounds !== undefined) state.usage.rounds = ev.history_rounds;
  if (ev.pruned_rounds !== undefined) state.usage.prunedRounds = ev.pruned_rounds;
  if (ev.pruned_chars !== undefined) state.usage.prunedChars = ev.pruned_chars;
  // Usage belongs to the session that produced it, on screen or in background,
  // so both the totals and the project cost are attributed to the emitting view
  // and never to whatever chat happens to be open.
  const view = currentView();
  const session = view ? sessionById(view.sessionId) : state.session;
  if (session) {
    if (ev.session_tokens_in !== undefined) session.tokens_in = ev.session_tokens_in;
    if (ev.session_tokens_out !== undefined) session.tokens_out = ev.session_tokens_out;
    if (ev.session_cost_usd !== undefined) session.cost_usd = ev.session_cost_usd;
  }
  const projectId = view?.projectId || state.activeProjectId;
  if (projectId && ev.project_cost_usd !== undefined) {
    const project = state.projects.find((p) => p.id === projectId)
      || (state.activeProject && state.activeProject.id === projectId ? state.activeProject : null);
    if (project) {
      project.total_cost_usd = ev.project_cost_usd;
      if (state.activeProject && state.activeProject.id === projectId) {
        state.activeProject.total_cost_usd = ev.project_cost_usd;
      }
      renderProjects();
    }
  }
  savePruneState(view?.sessionId);
  updateUsageUI();
}

// Pruning stats describe the last request, so they are cached per session to
// survive a reload instead of resetting to "nothing pruned".
function pruneStorageKey(sessionID = state.activeSessionId) {
  return sessionID ? `forge.prune.${sessionID}` : '';
}

function loadPruneState(sessionID) {
  const key = pruneStorageKey(sessionID);
  if (!key) return null;
  try {
    const raw = JSON.parse(localStorage.getItem(key) || 'null');
    if (!raw || typeof raw.rounds !== 'number') return null;
    return raw;
  } catch {
    return null;
  }
}

// savePruneState caches the stats of the emitting view, so a background agent
// never overwrites the entry of the chat on screen.
function savePruneState(sessionID = currentView()?.sessionId) {
  const key = pruneStorageKey(sessionID);
  if (!key) return;
  const usage = currentView()?.usage || EMPTY_USAGE;
  try {
    localStorage.setItem(key, JSON.stringify({
      rounds: usage.rounds || 0,
      prunedRounds: usage.prunedRounds || 0,
      prunedChars: usage.prunedChars || 0,
    }));
  } catch { /* ignore */ }
}

function countToolRounds(messages) {
  let rounds = 0;
  for (const m of messages || []) {
    if (m.role !== 'assistant') continue;
    let calls = [];
    try {
      calls = Array.isArray(m.tool_calls) ? m.tool_calls : JSON.parse(m.tool_calls || '[]');
    } catch {
      calls = [];
    }
    if (Array.isArray(calls) && calls.length) rounds++;
  }
  return rounds;
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

// updateUsageUI asks for a repaint instead of painting. Callers keep working on
// state; the panel is redrawn from the active view only.
function updateUsageUI() {
  scheduleInspector(currentView());
}

// Each dot is one tool round in the persisted history, oldest first. Blue
// means the round was sent verbatim; amber means it was stubbed before the
// request. Pruning always targets the oldest rounds, so amber forms a prefix.
function paintPrune(usage) {
  const rounds = Math.max(0, Math.floor(usage.rounds || 0));
  const pruned = Math.min(rounds, Math.max(0, Math.floor(usage.prunedRounds || 0)));
  const savedChars = Math.max(0, Math.floor(usage.prunedChars || 0));
  const savedTokens = Math.ceil(savedChars / 4);

  const pill = $('prunePill');
  pill.classList.toggle('d-none', pruned <= 0);
  if (pruned > 0) {
    pill.textContent = `${formatNum(pruned)} pruned`;
    pill.title =
      `${pruned} of ${rounds} tool rounds stubbed before the last request` +
      (savedChars > 0 ? ` (~${formatNum(savedTokens)} tokens saved).` : '.') +
      ' Click to inspect.';
  }

  $('roundsCount').textContent = formatNum(rounds);

  const meta = $('pruneMeta');
  const dots = $('pruneDots');
  const legend = document.querySelector('#usageDetails .prune-legend');
  dots.innerHTML = '';

  if (!rounds) {
    meta.textContent = 'no rounds yet';
    meta.title = 'Pruning stats appear after the first model response.';
    dots.setAttribute('aria-label', 'No tool rounds in context');
    if (legend) legend.classList.add('d-none');
    const empty = document.createElement('span');
    empty.className = 'prune-empty';
    empty.textContent = 'Tool rounds appear here as the agent works.';
    dots.appendChild(empty);
    return;
  }
  if (legend) legend.classList.remove('d-none');

  meta.textContent = `${pruned}/${rounds} pruned` +
    (savedChars > 0 ? ` · ${formatNum(savedChars)} chars saved` : '');
  meta.title = savedChars > 0
    ? `About ${formatNum(savedTokens)} tokens saved in the last request, including per-result clipping.`
    : 'The last request sent every tool round verbatim.';

  const frag = document.createDocumentFragment();
  for (let i = 0; i < rounds; i++) {
    const dot = document.createElement('span');
    const isPruned = i < pruned;
    dot.className = 'prune-dot' + (isPruned ? ' pruned' : '');
    dot.title = `Round ${i + 1} · ${isPruned ? 'pruned from the context' : 'sent in full'}`;
    frag.appendChild(dot);
  }
  dots.appendChild(frag);
  dots.setAttribute('aria-label', `${rounds} tool rounds, ${pruned} pruned, oldest first`);
}

function rememberFile(path, action) {
  if (!path) return;
  const previous = state.files.get(path);
  if (previous === 'modified' || action === 'modified') state.files.set(path, 'modified');
  else state.files.set(path, 'read');
}

// renderFiles asks for a repaint of the panel; the file list itself is painted
// from the active view by paintFiles.
function renderFiles() {
  scheduleInspector(currentView());
}

function isCodeFile(path) {
  return /\.(go|js|ts|tsx|jsx|py|rs|rb|java|kt|kts|c|h|cc|cpp|hpp|cs|php|swift|sh|bash|zsh|sql|html|css|scss|less|json|yaml|yml|toml|xml|md|lua|ex|exs|erl|hs|clj|vue|svelte)$/i.test(path);
}

// estimateContext mirrors what the agent actually sends: after a compaction the
// window starts at the newest start_message_id and includes its summary, while
// the transcript keeps showing every message.
function estimateContext(messages, compactions = []) {
  if (!messages || !messages.length) return 0;
  const last = (compactions || [])[compactions.length - 1];
  const startID = last ? Number(last.start_message_id) || 0 : 0;
  let chars = 1800;
  if (last && startID > 0) chars += String(last.summary || '').length;
  for (const m of messages) {
    if (startID > 0 && (Number(m.id) || 0) < startID) continue;
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

// renderChecklist asks for a repaint; paintChecklist reads the active view.
function renderChecklist() {
  scheduleInspector(currentView());
}

// checklistBusy is true while this view has a live agent. The run index counts
// too: a run lives on the server, so a chat can be busy before its stream
// re-attaches (reload, tab switch).
function checklistBusy(view) {
  if (!view) return false;
  return !!view.streaming || runIndex.has(view.sessionId);
}

// checklistState collapses the task counters and the run lifecycle into the
// single state the card advertises, so the sidebar always answers the one
// question a harness usually leaves open: when does this finish?
function checklistState(total, done, busy) {
  if (!total) return busy ? 'running-empty' : 'empty';
  if (done >= total) return busy ? 'running' : 'done';
  return busy ? 'running' : 'idle';
}

function checklistStatusLabel(status, total, done) {
  switch (status) {
    // The count lives in the progress chip now, so the card's line stays
    // qualitative and the two surfaces never repeat the same numbers.
    case 'running-empty': return 'Working — no task list';
    // Every task is ticked but the turn is still open: say so instead of
    // claiming the run is done.
    case 'running': return done >= total ? 'Wrapping up' : 'Working';
    case 'idle': return 'Waiting';
    case 'done': return 'Complete';
    case 'empty': return 'No tasks yet';
    default: return '';
  }
}

// checklistChipVisible mirrors the interactive rule: measuring tasks is the
// chip's only job, so a turn with no task list leaves the run indicator alone
// instead of pulsing a second time next to it.
function checklistChipVisible(total) {
  return total > 0;
}

// checklistChipLive reports whether the checklist card can actually be opened:
// the Inspector is display:none below 1200px, so the chip must not advertise a
// click that would land on nothing.
function checklistChipLive() {
  return !!(window.matchMedia && window.matchMedia('(min-width: 1200px)').matches);
}

// applyChecklistState is the only writer of the card state, the progress chip
// and the status line. renderChrome calls it too, so starting, resuming and
// finishing a run repaint both surfaces even when no checklist event arrives.
function applyChecklistState(view) {
  const card = $('checklistCard');
  if (!card) return;
  const checklist = view && Array.isArray(view.checklist) ? view.checklist : [];
  const total = checklist.length;
  const done = checklist.filter((i) => i.is_done).length;
  const status = checklistState(total, done, checklistBusy(view));
  card.dataset.state = status;
  $('checklistCount').textContent = `${done}/${total}`;

  const chip = $('progressChip');
  const visible = checklistChipVisible(total);
  if (chip) {
    chip.classList.toggle('d-none', !visible);
    chip.dataset.state = status;
    $('progressChipCount').textContent = `${done}/${total}`;
    // total is never 0 while the chip is visible, but a NaN width would leak an
    // invalid declaration into the stylesheet if it ever were.
    $('progressChipFill').style.width = total ? `${(done / total) * 100}%` : '0%';
    // A read-only chip under 1200px: no focus stop and no pointer cursor on a
    // control that cannot do anything.
    const live = checklistChipLive();
    if (live) {
      chip.setAttribute('role', 'button');
      chip.setAttribute('tabindex', '0');
      chip.title = 'Checklist progress — click to open it';
    } else {
      chip.removeAttribute('role');
      chip.removeAttribute('tabindex');
      chip.title = 'Checklist progress — widen the window to open it';
    }
  }

  const text = $('checklistStatusText');
  const label = checklistStatusLabel(status, total, done);
  // Only touch the node when the text really changed: the element is a polite
  // live region and a no-op write would re-announce on every repaint.
  if (text && text.textContent !== label) text.textContent = label;
}

// focusChecklist turns a click on the progress chip into a jump to the task
// list: open the Inspector if the user had closed it, scroll the card into view
// and put the caret in the field, so the click leaves the user able to act.
function focusChecklist() {
  // Below 1200px the Inspector cannot be shown at all; the chip is inert there
  // and applyChecklistState already stripped its button role.
  if (!checklistChipLive()) return;
  const card = $('checklistCard');
  const panel = $('contextPanel');
  if (!card || !panel) return;
  setPanelHidden(false);
  card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  // Restart the highlight even when it is already running: remove, force a
  // reflow, re-add. Without it a second click would not flash again.
  card.classList.remove('flash');
  void card.offsetWidth;
  card.classList.add('flash');
  setTimeout(() => card.classList.remove('flash'), 900);
  const input = $('checklistInput');
  if (input && !input.disabled) input.focus();
}

function paintChecklist(view) {
  const list = $('checklistItems');
  if (!list) return;
  const checklist = view && Array.isArray(view.checklist) ? view.checklist : [];
  list.innerHTML = '';
  const total = checklist.length;

  if (!total) {
    const empty = document.createElement('div');
    empty.className = 'checklist-empty';
    empty.textContent = checklistBusy(view)
      ? 'Forge is working on this turn. Tasks show up here when it plans multi-step work.'
      : 'No tasks yet. Add one, or ask Forge to plan multi-step work.';
    list.appendChild(empty);
    return;
  }

  const pending = checklist.filter((i) => !i.is_done);
  const completed = checklist.filter((i) => i.is_done);

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
  if (current === 'BUILD') removePlanReadyCard();
}

// busySessions counts the chats with a live agent, on screen or in background.
function busySessions() {
  let n = 0;
  for (const view of chatViews.values()) if (view.streaming) n += 1;
  return Math.max(n, runIndex.size);
}

function updateRunSummary() {
  const el = $('runSummary');
  if (!el) return;
  const total = busySessions();
  const others = Math.max(0, total - (activeStreaming() ? 1 : 0));
  if (others > 0) {
    el.classList.remove('d-none');
    el.textContent = `${others} other agent${others === 1 ? '' : 's'} running`;
    el.title = 'Agents keep working when you switch chat or project';
  } else {
    el.classList.add('d-none');
    el.textContent = '';
  }
}

// activeStreaming reports whether the chat on screen has a live agent. The run
// index counts too: a run lives on the server, so a chat can be busy before its
// event stream re-attaches (reload, tab switch) and the Stop button must reflect
// that. A background agent's run never makes another chat look busy.
function activeStreaming() {
  const view = activeView();
  if (!view) return false;
  return !!view.streaming || runIndex.has(view.sessionId);
}

let chromeFrame = 0;

// scheduleChrome coalesces repaints of the shared chrome. It is safe to call
// from any view: the paint itself always reads the active one.
function scheduleChrome() {
  if (chromeFrame) return;
  chromeFrame = requestAnimationFrame(() => {
    chromeFrame = 0;
    renderChrome();
  });
}

// renderChrome is the only writer of the controls that live outside a
// transcript: spinner, round label, Stop, composer, model/mode lock and the
// agents summary. It derives all of them from the chat on screen plus the server
// run index, so switching chats (not only receiving events) corrects them, and a
// control added here later inherits that isolation instead of needing a guard.
function renderChrome() {
  const view = activeView();
  const running = activeStreaming();
  const stopping = !!view && !!view.stopRequested;
  const hasSession = !!state.session;

  $('streamIndicator').classList.toggle('d-none', !running);
  $('stopBtn').classList.toggle('d-none', !running);
  $('stopBtn').disabled = stopping;
  $('stopBtn').title = stopping ? 'Stopping the agent…' : 'Stop generation';

  $('modelInput').disabled = running || !hasSession;
  $('sessionProviderSelect').disabled = running || !hasSession || !state.providers.length;
  for (const btn of $('modeToggle').querySelectorAll('button')) {
    btn.disabled = running || !hasSession;
  }
  $('promptInput').disabled = !hasSession;
  $('newSessionBtn').disabled = !state.activeProjectId;
  $('manageProjectBtn').disabled = !state.activeProjectId;
  $('manageSessionsBtn').disabled = !state.activeProjectId;
  $('agentsBtn').disabled = false;

  if (stopping) {
    const label = $('streamLabel');
    if (label) label.textContent = 'stopping…';
  } else {
    updateStreamLabel();
  }
  updateSendState();
  updateRunSummary();
  // The checklist card advertises whether the agent is still working, so the
  // card follows the run lifecycle even when no checklist event arrives.
  applyChecklistState(view);
}

// setStreamingUI is the run-lifecycle entry point. The chrome is a projection,
// so starting or finishing a run only has to ask for a repaint.
function setStreamingUI() {
  scheduleChrome();
}

function updateControls() {
  renderChrome();
  updateUsageUI();
}

function updateSendState() {
  const prompt = $('promptInput');
  const hasBody = !!prompt.value.trim() || state.attachments.length > 0;
  $('sendBtn').disabled = activeStreaming() || !state.session || !hasBody;
}

// scrollToAnswerStart parks the top of an assistant markdown block at the top of
// the transcript viewport. It is the single target of the round footer button and
// of Ctrl+Shift+↑/↓: a long turn can end hundreds of lines below the paragraph
// that answers the question, and scrolling by hand is the part users lose.
function scrollToAnswerStart(el) {
  if (!el || !viewIsActive(currentView())) return;
  const scroller = $('chatScroll');
  if (!scroller) return;
  const offset = el.getBoundingClientRect().top - scroller.getBoundingClientRect().top + scroller.scrollTop;
  scroller.scrollTo({ top: Math.max(0, offset - 16), behavior: 'smooth' });
}

// navigateAnswerStart walks the assistant answers of the mounted transcript.
// Like navigateUserMessage it clamps instead of wrapping, and the cold start
// lands on the newest answer (the one that just finished) when going up.
function navigateAnswerStart(dir) {
  const rounds = [...messagesEl().querySelectorAll('.assistant-round')];
  const answers = rounds.filter((el) => el.querySelector('.md'));
  if (!answers.length) return;
  rounds.forEach((el) => el.classList.remove('msg-focus'));
  let idx = state.answerIndex;
  if (idx < 0 || idx >= answers.length) idx = dir > 0 ? -1 : answers.length;
  idx += dir;
  if (idx < 0) idx = 0;
  if (idx >= answers.length) idx = answers.length - 1;
  state.answerIndex = idx;
  const round = answers[idx];
  round.classList.add('msg-focus');
  scrollToAnswerStart(round.querySelector('.md'));
}

function maybeScroll(force = false) {
  // Only the mounted view owns the scroll position: a background agent must not
  // yank the chat the user is reading.
  if (!viewIsActive(currentView())) return;
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

/* ── Attachments ("@" file picker) ───────────────────────────────── */

// Persisted user messages carry inline attachments wrapped in these tags, so
// transcript bubbles can show the files without dumping their content.
const ATTACHMENT_OPEN = '<attachments>';
const ATTACHMENT_CLOSE = '</attachments>';

function splitAttachmentBlock(text) {
  const source = String(text || '');
  const start = source.indexOf(ATTACHMENT_OPEN);
  if (start < 0) return { text: source.trim(), files: [] };
  const end = source.indexOf(ATTACHMENT_CLOSE, start);
  const head = (end < 0 ? source.slice(0, start) : source.slice(0, start)).trim();
  const block = source.slice(start, end < 0 ? undefined : end);
  const tail = end < 0 ? '' : source.slice(end + ATTACHMENT_CLOSE.length).trim();
  const files = [...block.matchAll(/<file path="([^"]+)"/g)].map((m) => m[1]);
  return { text: [head, tail].filter(Boolean).join('\n'), files };
}

function formatFileSize(bytes) {
  const n = Number(bytes || 0);
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + ' KB';
  return n + ' B';
}

function detectMention() {
  const el = $('promptInput');
  if (!el || el.disabled) return null;
  const caret = typeof el.selectionStart === 'number' ? el.selectionStart : el.value.length;
  const before = el.value.slice(0, caret);
  // "@" must start a word so emails and handles never open the picker.
  const match = /(^|[\s([])@([^\s@]*)$/.exec(before);
  if (!match) return null;
  return { start: caret - match[2].length - 1, end: caret, query: match[2] };
}

async function loadProjectFiles() {
  if (!state.activeProjectId) return [];
  if (state.fileCache && state.fileCache.projectId === state.activeProjectId) {
    return state.fileCache.files;
  }
  try {
    const data = await api(`/api/projects/${state.activeProjectId}/files`);
    const files = Array.isArray(data.files) ? data.files : [];
    state.fileCache = { projectId: state.activeProjectId, files };
    return files;
  } catch (err) {
    toast(err.message, 'error');
    return [];
  }
}

function rankFileMatches(files, query) {
  const q = query.toLowerCase().trim();
  if (!q) {
    return files.slice().sort((a, b) => a.path.localeCompare(b.path)).slice(0, 80);
  }
  const scored = [];
  for (const file of files) {
    const path = file.path.toLowerCase();
    const base = path.slice(path.lastIndexOf('/') + 1);
    let score = -1;
    if (base.startsWith(q)) score = 0;
    else if (path.endsWith('/' + q) || path === q) score = 1;
    else if (base.includes(q)) score = 2;
    else if (path.includes(q)) score = 3;
    else if (subsequence(q, path)) score = 4;
    if (score >= 0) scored.push({ file, score, len: file.path.length });
  }
  scored.sort((a, b) => a.score - b.score || a.len - b.len || a.file.path.localeCompare(b.file.path));
  return scored.slice(0, 80).map((s) => s.file);
}

function subsequence(needle, haystack) {
  let i = 0;
  for (let j = 0; j < haystack.length && i < needle.length; j++) {
    if (haystack[j] === needle[i]) i++;
  }
  return i === needle.length;
}

function fileIcon(language) {
  const lang = String(language || '').toLowerCase();
  if (lang === 'go') return 'bi-filetype-go';
  if (lang === 'js' || lang === 'javascript') return 'bi-filetype-js';
  if (lang === 'ts' || lang === 'typescript') return 'bi-filetype-ts';
  if (lang === 'html') return 'bi-filetype-html';
  if (lang === 'css') return 'bi-filetype-css';
  if (lang === 'json') return 'bi-filetype-json';
  if (lang === 'md') return 'bi-filetype-md';
  if (lang === 'py' || lang === 'python') return 'bi-filetype-py';
  if (lang === 'sh' || lang === 'bash') return 'bi-filetype-sh';
  return 'bi-file-earmark-code';
}

async function openFilePicker(mention) {
  const picker = $('filePicker');
  if (!picker) return;
  state.filePicker.open = true;
  state.filePicker.mention = mention;
  state.filePicker.index = 0;
  picker.classList.remove('d-none');

  const files = await loadProjectFiles();
  // The user may have kept typing or closed the picker while the list loaded.
  if (!state.filePicker.open || !state.filePicker.mention ||
      state.filePicker.mention.query !== mention.query) return;
  state.filePicker.files = files;
  state.filePicker.matches = rankFileMatches(files, mention.query);
  state.filePicker.index = 0;
  renderFilePicker();
}

function closeFilePicker() {
  const picker = $('filePicker');
  state.filePicker.open = false;
  state.filePicker.mention = null;
  state.filePicker.matches = [];
  state.filePicker.index = 0;
  if (picker) picker.classList.add('d-none');
}

// syncMentionPicker is the single entry point that keeps the dropdown in sync
// with the caret while the user types, pastes, clicks or moves inside the box.
function syncMentionPicker() {
  const mention = detectMention();
  if (!mention) {
    if (state.filePicker.open) closeFilePicker();
    return;
  }
  const same = state.filePicker.open &&
    state.filePicker.mention &&
    state.filePicker.mention.query === mention.query;
  state.filePicker.mention = mention;
  if (same) return;
  openFilePicker(mention);
}

// handlePickerKey consumes the keys the dropdown needs. It returns true when the
// event was handled so the composer never sends a message by accident.
function handlePickerKey(e) {
  if (e.isComposing) return false;
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault();
      movePickerSelection(1);
      return true;
    case 'ArrowUp':
      e.preventDefault();
      movePickerSelection(-1);
      return true;
    case 'Enter':
    case 'Tab': {
      const file = state.filePicker.matches[state.filePicker.index];
      if (!file) {
        e.preventDefault();
        closeFilePicker();
        return true;
      }
      e.preventDefault();
      applyAttachment(file.path);
      return true;
    }
    case 'Escape':
      e.preventDefault();
      closeFilePicker();
      return true;
    default:
      return false;
  }
}

function renderFilePicker() {
  const list = $('filePickerList');
  if (!list) return;
  const { matches, index } = state.filePicker;
  list.innerHTML = '';
  $('filePickerCount').textContent = matches.length
    ? `${Math.min(index + 1, matches.length)}/${matches.length}`
    : '';

  if (!matches.length) {
    const empty = document.createElement('div');
    empty.className = 'file-picker-empty';
    empty.textContent = state.activeProjectId
      ? 'No matching files in this project.'
      : 'Select a project first.';
    list.appendChild(empty);
    return;
  }

  matches.forEach((file, i) => {
    const row = document.createElement('div');
    row.className = 'file-picker-item';
    if (i === index) row.classList.add('active');
    if (state.attachments.includes(file.path)) row.classList.add('already');
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', i === index ? 'true' : 'false');
    row.dataset.path = file.path;

    const icon = document.createElement('i');
    icon.className = 'bi ' + fileIcon(file.language || file.path.split('.').pop());
    const path = document.createElement('span');
    path.className = 'picker-path';
    path.textContent = file.path;
    path.title = file.path;
    const size = document.createElement('span');
    size.className = 'picker-size';
    size.textContent = formatFileSize(file.size_bytes);

    row.appendChild(icon);
    row.appendChild(path);
    row.appendChild(size);
    row.addEventListener('mousedown', (e) => {
      e.preventDefault();
      applyAttachment(file.path);
    });
    list.appendChild(row);
  });

  const active = list.querySelector('.file-picker-item.active');
  if (active && active.scrollIntoView) active.scrollIntoView({ block: 'nearest' });
}

function movePickerSelection(delta) {
  const total = state.filePicker.matches.length;
  if (!total) return;
  state.filePicker.index = (state.filePicker.index + delta + total) % total;
  renderFilePicker();
}

function applyAttachment(path) {
  const el = $('promptInput');
  const mention = state.filePicker.mention;
  if (el && mention) {
    const before = el.value.slice(0, mention.start);
    const after = el.value.slice(mention.end);
    // Drop a single leftover space so the sentence stays tidy.
    const rest = after.replace(/^\s/, '');
    el.value = before + rest;
    const caret = before.length;
    try {
      el.setSelectionRange(caret, caret);
    } catch { /* ignore */ }
  }
  addAttachment(path);
  closeFilePicker();
  resizePrompt();
  updateSendState();
  if (el) el.focus();
}

function addAttachment(path) {
  const clean = String(path || '').trim();
  if (!clean) return;
  if (state.attachments.includes(clean)) {
    toast('Already attached: ' + clean, 'error');
    return;
  }
  if (state.attachments.length >= 12) {
    toast('Up to 12 files per message', 'error');
    return;
  }
  state.attachments.push(clean);
  renderAttachmentChips();
  updateSendState();
}

function removeAttachment(path) {
  state.attachments = state.attachments.filter((p) => p !== path);
  renderAttachmentChips();
  updateSendState();
}

function renderAttachmentChips() {
  const box = $('attachmentChips');
  if (!box) return;
  box.innerHTML = '';
  if (!state.attachments.length) {
    box.classList.add('d-none');
    return;
  }
  box.classList.remove('d-none');
  for (const path of state.attachments) {
    const chip = document.createElement('span');
    chip.className = 'composer-chip';
    chip.title = path;
    const icon = document.createElement('i');
    icon.className = 'bi ' + fileIcon(path.split('.').pop());
    const label = document.createElement('span');
    label.className = 'chip-path';
    label.textContent = path;
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'chip-remove';
    remove.title = 'Remove attachment';
    remove.setAttribute('aria-label', 'Remove ' + path);
    remove.innerHTML = '&times;';
    remove.addEventListener('click', () => removeAttachment(path));
    chip.appendChild(icon);
    chip.appendChild(label);
    chip.appendChild(remove);
    box.appendChild(chip);
  }
}

function clearAttachments() {
  state.attachments = [];
  renderAttachmentChips();
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
  decorateCodeBlocks(el);
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

/* ── Copy to clipboard ───────────────────────────────────────────── */

async function copyToClipboard(text, btn) {
  const value = String(text == null ? '' : text);
  if (!value.trim()) return false;
  let ok = false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(value);
      ok = true;
    }
  } catch {
    ok = false;
  }
  if (!ok) ok = legacyCopy(value);
  if (ok) {
    flashCopied(btn);
    toast('Copied to clipboard', 'success');
  } else {
    toast('Could not copy to clipboard', 'error');
  }
  return ok;
}

function legacyCopy(value) {
  try {
    const ta = document.createElement('textarea');
    ta.value = value;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    return ok;
  } catch {
    return false;
  }
}

function flashCopied(btn) {
  if (!btn) return;
  if (btn.copyTimer) clearTimeout(btn.copyTimer);
  const icon = btn.querySelector('i');
  const prevClass = icon ? icon.className : '';
  btn.classList.add('is-copied');
  if (icon) icon.className = 'bi bi-check2';
  btn.copyTimer = setTimeout(() => {
    btn.classList.remove('is-copied');
    if (icon) icon.className = prevClass;
  }, 1200);
}

function makeCopyButton(className, title, onCopy) {
  return makeFooterAction(className, title, 'bi-clipboard', onCopy);
}

// makeFooterAction builds one of the small hover controls that live in a round
// footer (copy, jump to the answer). They share geometry, so only the icon and
// the handler differ.
function makeFooterAction(className, title, iconClass, action) {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = className;
  btn.title = title;
  btn.setAttribute('aria-label', title);
  btn.innerHTML = `<i class="bi ${iconClass}"></i>`;
  btn.addEventListener('click', () => action(btn));
  return btn;
}

// Wrap every fenced code block so a copy button can float over its top-right
// corner. Called after markdown rendering (which replaces innerHTML), so it is
// safe to re-run on each streaming re-render.
function decorateCodeBlocks(el) {
  for (const pre of Array.from(el.querySelectorAll('pre'))) {
    if (pre.parentElement && pre.parentElement.classList.contains('code-block')) continue;
    const wrap = document.createElement('div');
    wrap.className = 'code-block';
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);
    const btn = makeCopyButton('code-copy', 'Copy code', (b) => {
      const code = pre.querySelector('code');
      copyToClipboard(code ? code.textContent : pre.textContent, b);
    });
    wrap.appendChild(btn);
  }
}

/* ── Settings / project modals ───────────────────────────────────── */

// Bootstrap 5 does not auto-initialise tooltips from data attributes, so every
// explainer icon is created once. `container: body` is required inside the
// scrollable modal, whose overflow would otherwise clip the tooltip.
function initTooltips(root) {
  if (!window.bootstrap || !root) return;
  root.querySelectorAll('[data-bs-toggle="tooltip"]').forEach((el) => {
    bootstrap.Tooltip.getOrCreateInstance(el, { trigger: 'hover focus', delay: { show: 260, hide: 90 } });
  });
}

/* ── Provider settings ───────────────────────────────────────────── */

const PROVIDER_PRESETS = {
  openrouter: {
    name: 'OpenRouter',
    base_url: 'https://openrouter.ai/api/v1',
    default_model: '',
    extra_headers: {
      'HTTP-Referer': 'https://github.com/forge-harness/forge',
      'X-Title': 'Forge',
    },
    keyHelp: 'Create a key at openrouter.ai/keys. Leave blank to keep the current key.',
  },
  ollama: {
    name: 'Ollama Local',
    base_url: 'http://localhost:11434/v1',
    default_model: 'qwen2.5-coder:7b',
    extra_headers: {},
    keyHelp: 'Local Ollama usually needs no key. Start it with `ollama serve`.',
  },
  lmstudio: {
    name: 'LM Studio',
    base_url: 'http://localhost:1234/v1',
    default_model: '',
    extra_headers: {},
    keyHelp: 'Start the LM Studio local server and load a model first.',
  },
  custom: {
    name: '',
    base_url: '',
    default_model: '',
    extra_headers: {},
    keyHelp: 'Leave blank for local endpoints that do not require auth.',
  },
};

function renderProviderSettings() {
  const select = $('activeProviderSelect');
  const list = $('providerList');
  if (!select || !list) return;

  select.innerHTML = '';
  if (!state.providers.length) {
    const opt = document.createElement('option');
    opt.value = '';
    opt.textContent = 'No provider configured';
    select.appendChild(opt);
    select.disabled = true;
  } else {
    select.disabled = false;
    for (const p of state.providers) {
      const opt = document.createElement('option');
      opt.value = p.id;
      opt.textContent = `${p.name}${p.is_active ? ' · active' : ''}`;
      select.appendChild(opt);
    }
    select.value = state.activeProviderId || state.providers[0].id;
  }

  list.innerHTML = '';
  if (!state.providers.length) {
    const empty = document.createElement('div');
    empty.className = 'provider-empty';
    empty.textContent = 'No providers yet. Add OpenRouter, a local endpoint, or a custom OpenAI-compatible URL.';
    list.appendChild(empty);
    return;
  }
  for (const p of state.providers) {
    const row = document.createElement('div');
    row.className = 'provider-row' + (p.is_active ? ' is-active' : '');

    const main = document.createElement('div');
    main.className = 'provider-row-main';
    const name = document.createElement('div');
    name.className = 'provider-row-name';
    name.textContent = p.name;
    if (p.is_active) {
      const badge = document.createElement('span');
      badge.className = 'provider-badge';
      badge.textContent = 'active';
      name.appendChild(badge);
    }
    const meta = document.createElement('div');
    meta.className = 'provider-row-meta';
    const bits = [p.base_url];
    if (p.default_model) bits.push(p.default_model);
    if (p.api_key_set) bits.push('key set');
    meta.textContent = bits.join(' · ');
    main.append(name, meta);

    const actions = document.createElement('div');
    actions.className = 'provider-row-actions';
    if (!p.is_active) {
      const activate = document.createElement('button');
      activate.type = 'button';
      activate.className = 'btn btn-outline-secondary';
      activate.textContent = 'Activate';
      activate.addEventListener('click', () => activateProvider(p.id));
      actions.appendChild(activate);
    }
    const test = document.createElement('button');
    test.type = 'button';
    test.className = 'btn btn-outline-secondary';
    test.title = 'Test connection';
    test.innerHTML = '<i class="bi bi-activity"></i>';
    test.addEventListener('click', () => testProviderFromList(p));
    actions.appendChild(test);

    const edit = document.createElement('button');
    edit.type = 'button';
    edit.className = 'btn btn-outline-secondary';
    edit.innerHTML = '<i class="bi bi-pencil"></i>';
    edit.title = 'Edit provider';
    edit.addEventListener('click', () => openProviderEditor(p));
    actions.appendChild(edit);

    row.append(main, actions);
    list.appendChild(row);
  }
}

function renderSessionProviderOptions() {
  const select = $('sessionProviderSelect');
  if (!select) return;
  const current = (state.session && state.session.provider_id) || '';
  select.innerHTML = '';
  const dflt = document.createElement('option');
  dflt.value = '';
  dflt.textContent = `Default · ${providerName(state.activeProviderId)}`;
  select.appendChild(dflt);
  for (const p of state.providers) {
    const opt = document.createElement('option');
    opt.value = p.id;
    opt.textContent = p.name;
    select.appendChild(opt);
  }
  select.value = current;
  select.disabled = !state.session || !state.providers.length;
}

function openProviderEditor(provider = null) {
  const editing = !!provider;
  $('providerEditID').value = editing ? provider.id : '';
  $('providerEditorTitle').textContent = editing ? 'Edit provider' : 'New provider';
  $('providerName').value = editing ? provider.name : '';
  $('providerBaseURL').value = editing ? provider.base_url : '';
  $('providerDefaultModel').value = editing ? provider.default_model || '' : '';
  $('providerHeaders').value = editing && provider.extra_headers &&
    Object.keys(provider.extra_headers).length
    ? JSON.stringify(provider.extra_headers, null, 2)
    : '';
  $('providerActive').checked = editing ? !!provider.is_active : !state.providers.length;
  $('deleteProviderBtn').classList.toggle('d-none', !editing);
  $('providerAPIKey').value = '';
  $('providerAPIKey').type = 'password';
  $('providerAPIKey').placeholder = editing && provider.api_key_set
    ? `•••••••• (${provider.api_key_hint || 'configured'}) — leave blank to keep`
    : 'sk-…';
  $('providerKeyHelp').textContent = editing && provider.api_key_set
    ? 'A key is stored. Leave blank to keep it, or paste a new key.'
    : 'Stored locally in the Forge database.';
  const status = $('providerTestStatus');
  status.textContent = '';
  status.className = 'provider-test-status';
  document.querySelectorAll('#providerPresets .provider-preset').forEach((b) => b.classList.remove('is-active'));
  $('providerEditor').classList.remove('d-none');
  $('providerName').focus();
}

function closeProviderEditor() {
  $('providerEditor').classList.add('d-none');
  $('providerEditID').value = '';
  $('providerTestStatus').textContent = '';
  $('providerTestStatus').className = 'provider-test-status';
}

function applyProviderPreset(key) {
  const preset = PROVIDER_PRESETS[key];
  if (!preset) return;
  $('providerName').value = preset.name;
  $('providerBaseURL').value = preset.base_url;
  $('providerDefaultModel').value = preset.default_model ||
    (key === 'openrouter' ? (state.config?.defaults?.model || '') : '');
  $('providerHeaders').value = Object.keys(preset.extra_headers || {}).length
    ? JSON.stringify(preset.extra_headers, null, 2)
    : '';
  $('providerKeyHelp').textContent = preset.keyHelp;
  $('providerActive').checked = !state.providers.length;
  $('deleteProviderBtn').classList.add('d-none');
  $('providerEditID').value = '';
  $('providerEditorTitle').textContent = 'New provider';
  $('providerAPIKey').value = '';
  $('providerTestStatus').textContent = '';
  $('providerTestStatus').className = 'provider-test-status';
  document.querySelectorAll('#providerPresets .provider-preset').forEach((b) => {
    b.classList.toggle('is-active', b.dataset.preset === key);
  });
}

function readProviderForm() {
  const rawHeaders = $('providerHeaders').value.trim();
  let headers = {};
  if (rawHeaders) {
    try {
      const parsed = JSON.parse(rawHeaders);
      if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') {
        throw new Error('Extra headers must be a JSON object');
      }
      for (const [key, value] of Object.entries(parsed)) {
        if (typeof value !== 'string') throw new Error(`Header "${key}" must be a string`);
        headers[key] = value;
      }
    } catch (err) {
      toast(`Extra headers: ${err.message}`, 'error');
      return null;
    }
  }
  const id = $('providerEditID').value.trim();
  const key = $('providerAPIKey').value.trim();
  return {
    id,
    name: $('providerName').value.trim(),
    driver: 'openai_compatible',
    base_url: $('providerBaseURL').value.trim(),
    api_key: key || (id ? '__KEEP__' : ''),
    default_model: $('providerDefaultModel').value.trim(),
    extra_headers: headers,
    is_active: $('providerActive').checked,
  };
}

async function saveProviderEditor() {
  const body = readProviderForm();
  if (!body) return;
  if (!body.name || !body.base_url) {
    toast('Provider name and base URL are required', 'error');
    return;
  }
  const editing = !!body.id;
  try {
    const data = await api(editing ? `/api/providers/${encodeURIComponent(body.id)}` : '/api/providers', {
      method: editing ? 'PUT' : 'POST',
      body: JSON.stringify(body),
    });
    const saved = data.provider;
    closeProviderEditor();
    await loadProviders();
    if (saved && saved.is_active) {
      state.activeProviderId = saved.id;
      await loadModels(saved.id, { refresh: true });
      await adoptProviderModel(saved.id);
    } else {
      await loadModels(effectiveProviderID());
    }
    renderSessionProviderOptions();
    toast(editing ? 'Provider updated' : 'Provider added', 'success');
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function deleteProviderEditor() {
  const id = $('providerEditID').value.trim();
  if (!id) return;
  const p = providerById(id);
  if (!window.confirm(`Delete provider "${p ? p.name : id}"? Chats pinned to it fall back to the active provider.`)) {
    return;
  }
  try {
    await api(`/api/providers/${encodeURIComponent(id)}`, { method: 'DELETE' });
    closeProviderEditor();
    await loadProviders();
    await loadModels(effectiveProviderID(), { refresh: true });
    await adoptProviderModel(effectiveProviderID());
    renderSessionProviderOptions();
    toast('Provider deleted', 'success');
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function activateProvider(id) {
  const p = providerById(id);
  if (!p) return;
  try {
    await api(`/api/providers/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify({
        id: p.id,
        name: p.name,
        driver: p.driver,
        base_url: p.base_url,
        api_key: '__KEEP__',
        default_model: p.default_model,
        extra_headers: p.extra_headers || {},
        is_active: true,
      }),
    });
    state.activeProviderId = id;
    await loadProviders();
    if (!state.session || !state.session.provider_id) {
      await loadModels(id, { refresh: true });
      await adoptProviderModel(id);
    }
    renderSessionProviderOptions();
    toast(`Active provider: ${p.name}`, 'success');
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function switchSessionProvider(id) {
  if (!state.session) return;
  const updated = await patchSession({ provider_id: id });
  if (!updated) {
    renderSessionProviderOptions();
    return;
  }
  renderSessionProviderOptions();
  const effective = id || state.activeProviderId;
  await loadModels(effective, { refresh: true });
  await adoptProviderModel(effective);
  toast(`Provider: ${providerName(effective)}`, 'success');
}

async function adoptProviderModel(providerID) {
  if (!state.session || !state.models.size) return;
  if (state.models.has(state.session.model)) return;
  const p = providerById(providerID);
  const fallback = (p && p.default_model) || state.models.keys().next().value;
  if (!fallback || fallback === state.session.model) return;
  const updated = await patchSession({ model: fallback });
  if (updated) {
    $('modelInput').value = updated.model;
    updateUsageUI();
  }
}

function setProviderTestStatus(text, kind = '') {
  const el = $('providerTestStatus');
  if (!el) return;
  el.textContent = text;
  el.className = 'provider-test-status' + (kind ? ` is-${kind}` : '');
}

async function runProviderTest(body) {
  const data = await api('/api/providers/test', {
    method: 'POST',
    body: JSON.stringify(body),
  });
  return data;
}

async function testProviderEditor() {
  const body = readProviderForm();
  if (!body) return;
  if (!body.base_url) {
    setProviderTestStatus('Enter a base URL first.', 'error');
    return;
  }
  setProviderTestStatus('Testing connection…');
  try {
    const data = await runProviderTest(body);
    if (data.ok) {
      const count = data.models_supported ? ` · ${data.count} models` : '';
      setProviderTestStatus(`${data.message}${count}`, 'ok');
    } else {
      setProviderTestStatus(data.message || 'Connection failed', 'error');
    }
  } catch (err) {
    setProviderTestStatus(err.message, 'error');
  }
}

async function testProviderFromList(p) {
  try {
    const data = await runProviderTest({
      id: p.id,
      name: p.name,
      driver: p.driver,
      base_url: p.base_url,
      api_key: '',
      default_model: p.default_model,
      extra_headers: p.extra_headers || {},
    });
    if (data.ok) {
      const count = data.models_supported ? ` · ${data.count} models` : '';
      toast(`${p.name}: ${data.message}${count}`, 'success');
    } else {
      toast(`${p.name}: ${data.message || 'connection failed'}`, 'error');
    }
  } catch (err) {
    toast(err.message, 'error');
  }
}

function openSettings() {
  if (!state.config) return;
  const cfg = state.config;
  renderProviderSettings();
  renderSessionProviderOptions();
  $('defaultModel').value = cfg.defaults.model || '';
  $('defaultMode').value = cfg.defaults.mode || 'PLAN';
  // A stored 0 means "no limit" and must be shown as 0, so every field is read
  // with ?? instead of ||: the latter would silently turn 0 into a default.
  $('maxRead').value = cfg.defaults.max_file_read_chars ?? 25000;
  $('bashTimeout').value = cfg.defaults.bash_timeout_sec ?? 30;
  $('maxToolRounds').value = cfg.defaults.max_tool_rounds ?? 16;
  $('maxTurnRounds').value = cfg.defaults.max_turn_rounds ?? 64;
  $('maxTurnCost').value = cfg.defaults.max_turn_cost_usd ?? 0;
  $('planRounds').value = cfg.defaults.plan_rounds ?? 16;
  $('planToolCalls').value = cfg.defaults.plan_tool_calls ?? 32;
  $('historyPrunePercent').value = cfg.defaults.history_prune_percent ?? 70;
  $('historyPruneMaxTokens').value = cfg.defaults.history_prune_max_tokens ?? 120000;
  $('historyKeepRecent').value = cfg.defaults.history_keep_recent_rounds ?? 0;
  $('historyMaxResult').value = cfg.defaults.history_max_result_chars ?? 16000;
  $('historyMaxArg').value = cfg.defaults.history_max_arg_chars ?? 4000;
  $('historyPruneChunk').value = cfg.defaults.history_prune_chunk_rounds ?? 0;
  $('autoContinue').checked = cfg.defaults.auto_continue !== false;
  $('serverHost').value = cfg.server.host || '127.0.0.1';
  $('serverPort').value = cfg.server.port || 7890;
  $('autoBrowser').checked = !!cfg.server.auto_open_browser;
  $('browserNotifications').checked = !!cfg.defaults.browser_notifications;
  renderNotificationStatus();
  $('debugToggle').checked = !!cfg.server.debug || !!state.debugActive;
  $('debugLLMToggle').checked = !!cfg.server.debug_llm || !!state.llmDebugActive;
  $('logPath').textContent = state.logPath || '~/.forge/forge.log';
  $('configPath').textContent = state.configPath || '~/.forge/config.json';
  renderEffectiveBudget();
  if (settingsModal) settingsModal.show();
}

// renderEffectiveBudget explains the one derived value in the settings modal:
// the token ceiling a request must fit in, min(context × prune %, token cap).
function renderEffectiveBudget() {
  const el = $('effectiveBudget');
  if (!el) return;
  const pct = Math.max(0, Number($('historyPrunePercent').value) || 0);
  const cap = Math.max(0, Number($('historyPruneMaxTokens').value) || 0);
  const ctx = contextLimit($('defaultModel').value.trim() || state.config?.defaults?.model);
  if (pct === 0 && cap === 0) {
    el.innerHTML = 'Token budget disabled — pruning falls back to the round-based window below.';
    return;
  }
  let limit = cap;
  const fromContext = ctx > 0 && pct > 0 ? Math.floor((ctx * pct) / 100) : 0;
  if (fromContext > 0 && (limit === 0 || fromContext < limit)) limit = fromContext;
  const parts = [];
  parts.push(`<strong>${formatLimit(limit)} tokens</strong> per request`);
  const pctLabel = fromContext > 0
    ? `${pct}% of ${formatLimit(ctx)}`
    : `${pct}% of the context window`;
  parts.push(cap > 0 && fromContext > 0
    ? `min(${pctLabel}, ${formatLimit(cap)} cap)`
    : (cap > 0 ? `${formatLimit(cap)} cap` : pctLabel));
  el.innerHTML = parts.join(' — ');
}

async function saveSettings(e) {
  e.preventDefault();
  if (!state.config) return;
  const cfg = JSON.parse(JSON.stringify(state.config));
  // Provider credentials now live in the llm_providers table. The legacy
  // OpenRouter section stays untouched so older config files keep working.
  cfg.defaults.model = $('defaultModel').value.trim();
  cfg.defaults.mode = $('defaultMode').value;
  // Only an empty or non-numeric field falls back to a default: a typed 0 is a
  // real choice ("no limit") and must reach the server unchanged.
  const numOr = (id, fallback) => {
    const raw = $(id).value.trim();
    if (raw === '') return fallback;
    const n = Number(raw);
    return Number.isFinite(n) ? n : fallback;
  };
  cfg.defaults.max_file_read_chars = numOr('maxRead', 25000);
  cfg.defaults.bash_timeout_sec = numOr('bashTimeout', 30);
  cfg.defaults.max_tool_rounds = numOr('maxToolRounds', 16);
  cfg.defaults.max_turn_rounds = numOr('maxTurnRounds', 64);
  cfg.defaults.max_turn_cost_usd = numOr('maxTurnCost', 0);
  cfg.defaults.plan_rounds = numOr('planRounds', 16);
  cfg.defaults.plan_tool_calls = numOr('planToolCalls', 32);
  cfg.defaults.history_prune_percent = numOr('historyPrunePercent', 70);
  cfg.defaults.history_prune_max_tokens = numOr('historyPruneMaxTokens', 120000);
  cfg.defaults.history_keep_recent_rounds = numOr('historyKeepRecent', 0);
  cfg.defaults.history_max_result_chars = numOr('historyMaxResult', 16000);
  cfg.defaults.history_max_arg_chars = numOr('historyMaxArg', 4000);
  cfg.defaults.history_prune_chunk_rounds = numOr('historyPruneChunk', 0);
  cfg.defaults.auto_continue = $('autoContinue').checked;
  cfg.server.host = $('serverHost').value.trim();
  cfg.server.port = Number($('serverPort').value) || 7890;
  cfg.server.auto_open_browser = $('autoBrowser').checked;
  cfg.defaults.browser_notifications = $('browserNotifications').checked;
  cfg.server.debug = $('debugToggle').checked;
  cfg.server.debug_llm = $('debugLLMToggle').checked;
  if (cfg.server.debug_llm) cfg.server.debug = true;
  // Saving is a user gesture, so the browser shows the permission prompt here.
  // Asking from the stream callback would be silently denied.
  if (cfg.defaults.browser_notifications) {
    const perm = await ensureNotificationPermission();
    if (perm === 'denied') {
      toast('Browser notifications are blocked — allow them for this site (padlock icon) and try again', 'error');
    } else if (perm === 'unsupported') {
      toast('This browser cannot show notifications', 'error');
    }
  }

  const before = state.config;
  try {
    const data = await api('/api/config', { method: 'PUT', body: JSON.stringify(cfg) });
    state.config = data.config;
    state.configPath = data.path;
    state.logPath = data.log_path || state.logPath;
    state.debugActive = !!data.debug;
    state.llmDebugActive = !!data.llm_debug;
    state.apiKeySet = !!data.api_key_set;
    if (settingsModal) settingsModal.hide();
    toast('Settings saved', 'success');
    if (before.server.debug !== state.config.server.debug) {
      toast(state.config.server.debug ? 'Debug logging enabled' : 'Debug logging disabled', 'success');
    }
    if (before.server.debug_llm !== state.config.server.debug_llm) {
      toast(state.config.server.debug_llm ? 'LLM payload tracing enabled' : 'LLM payload tracing disabled', 'success');
    }
    if (before.server.port !== state.config.server.port || before.server.host !== state.config.server.host) {
      toast('Host/port will apply after restarting Forge', 'success');
    }
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function saveRenameTab(e) {
  e.preventDefault();
  const sessionId = Number($('renameTabSessionId').value || 0);
  const newTitle = $('renameTabInput').value.trim();
  if (!sessionId || !newTitle) return;
  try {
    const data = await api(`/api/sessions/${sessionId}`, {
      method: 'PATCH',
      body: JSON.stringify({ title: newTitle }),
    });
    syncSession(data.session);
    if (sessionId === state.activeSessionId && data.session.title) {
      document.title = `${data.session.title} · Forge`;
    }
    if ($('sessionsModal')?.classList.contains('show')) renderSessionsList();
    if (renameTabModal) renameTabModal.hide();
    toast('Chat renamed', 'success');
  } catch (err) {
    toast(err.message, 'error');
  }
}

function resetDefaults() {
  $('defaultModel').value = 'qwen/qwen-2.5-coder-32b-instruct';
  $('defaultMode').value = 'PLAN';
  $('maxRead').value = 25000;
  $('bashTimeout').value = 30;
  $('maxToolRounds').value = 16;
  $('maxTurnRounds').value = 64;
  $('maxTurnCost').value = 0;
  $('planRounds').value = 16;
  $('planToolCalls').value = 32;
  $('historyPrunePercent').value = 70;
  $('historyPruneMaxTokens').value = 120000;
  $('historyKeepRecent').value = 0;
  $('historyMaxResult').value = 16000;
  $('historyMaxArg').value = 4000;
  $('historyPruneChunk').value = 0;
  $('autoContinue').checked = true;
  $('serverHost').value = '127.0.0.1';
  $('serverPort').value = 7890;
  $('autoBrowser').checked = true;
  $('browserNotifications').checked = false;
  $('debugToggle').checked = false;
  $('debugLLMToggle').checked = false;
  renderEffectiveBudget();
  toast('Defaults restored — click Save settings to persist', 'success');
}

// `create` opens a blank form; otherwise the given project (or the active one)
// is loaded for editing.
function openProjectModal(project = null, { create = false } = {}) {
  const form = $('projectForm');
  form.reset();
  const editing = create ? null : (project || (
    state.activeProjectId
      ? state.projects.find((p) => p.id === state.activeProjectId) || state.activeProject
      : null
  ));
  const meta = $('projectMeta');
  if (editing) {
    $('projectId').value = String(editing.id);
    $('projectModalTitle').textContent = 'Project settings';
    $('projectModalIcon').className = 'bi bi-sliders me-2';
    $('projectSubmitBtn').textContent = 'Save changes';
    $('deleteProjectBtn').classList.remove('d-none');
    $('projectPath').value = editing.path || '';
    $('projectName').value = editing.name || '';
    $('projectColor').value = editing.icon_color || randomPaletteColor();
    meta.classList.remove('d-none');
    meta.innerHTML = '';
    const info = document.createElement('div');
    info.textContent = `${Number(editing.session_count || 0)} chats · ${formatCost(editing.total_cost_usd)} spent`;
    meta.appendChild(info);
    const hint = document.createElement('div');
    hint.textContent = 'Changing the directory keeps this project and its chats.';
    meta.appendChild(hint);
  } else {
    $('projectId').value = '';
    $('projectModalTitle').textContent = 'Register a project';
    $('projectModalIcon').className = 'bi bi-folder-plus me-2';
    $('projectSubmitBtn').textContent = 'Register project';
    $('deleteProjectBtn').classList.add('d-none');
    $('projectColor').value = randomPaletteColor();
    meta.classList.add('d-none');
    meta.innerHTML = '';
  }
  if (projectModal) projectModal.show();
  setTimeout(() => $('projectPath').focus(), 180);
}

async function saveProject(e) {
  e.preventDefault();
  const path = $('projectPath').value.trim();
  if (!path) return;
  const id = Number($('projectId').value || 0);
  const payload = {
    path,
    name: $('projectName').value.trim(),
    icon_color: $('projectColor').value,
  };
  const btn = $('projectSubmitBtn');
  btn.disabled = true;
  try {
    if (id) {
      const data = await api(`/api/projects/${id}`, {
        method: 'PATCH',
        body: JSON.stringify(payload),
      });
      state.projects = data.projects || state.projects;
      renderProjects();
      if (data.project.id === state.activeProjectId) {
        state.activeProject = data.project;
        // The path or the color may have changed, and a moved directory can be
        // a different repository, so repaint and re-probe.
        renderWorkspaceBar(data.project);
        refreshGitInfo(data.project.id);
      }
      if (projectModal) projectModal.hide();
      toast(`Updated ${data.project.name}`, 'success');
    } else {
      const data = await api('/api/projects', {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      state.projects = data.projects || [data.project];
      renderProjects();
      if (projectModal) projectModal.hide();
      toast(`Added ${data.project.name}`, 'success');
      await selectProject(data.project.id, { autoCreate: true });
    }
  } catch (err) {
    toast(err.message, 'error');
  } finally {
    btn.disabled = false;
  }
}

async function deleteProject() {
  const id = Number($('projectId').value || 0);
  const project = state.projects.find((p) => p.id === id);
  if (!id || !project) return;
  const ok = window.confirm(
    `Delete "${project.name}" and all of its chats? This cannot be undone.`
  );
  if (!ok) return;
  try {
    const data = await api(`/api/projects/${id}`, { method: 'DELETE' });
    state.projects = data?.projects || state.projects.filter((p) => p.id !== id);
    try { localStorage.removeItem(`forge.tabOrder.${id}`); } catch { /* ignore */ }
    try { localStorage.removeItem(`forge.session.${id}`); } catch { /* ignore */ }
    if (projectModal) projectModal.hide();
    toast(`Deleted ${project.name}`, 'success');
    if (id === state.activeProjectId) {
      state.activeProjectId = null;
      state.activeProject = null;
      state.sessions = [];
      clearSession();
      state.git = { projectId: null, checked: false, isRepo: false, branch: '', detached: false, commit: '', seq: state.git.seq };
      renderWorkspaceBar(null);
      const next = state.projects[0];
      if (next) await selectProject(next.id, { autoCreate: true });
      else {
        renderProjects();
        clearSession();
        showEmpty('project');
      }
    }
    renderProjects();
  } catch (err) {
    toast(err.message, 'error');
  }
}

/* ── Manage chats ────────────────────────────────────────────────── */

async function openSessionsModal() {
  if (!state.activeProjectId) {
    toast('Select a project first', 'error');
    return;
  }
  $('sessionSearch').value = '';
  try {
    // Default to every chat so closed ones are discoverable.
    $('sessionsShowAll').checked = localStorage.getItem('forge.showClosed') !== '0';
  } catch { /* ignore */ }
  if (sessionsModal) sessionsModal.show();
  await loadAllSessions();
}

async function loadAllSessions() {
  if (!state.activeProjectId) return;
  const list = $('sessionsList');
  list.innerHTML = '<div class="manage-empty">Loading…</div>';
  try {
    const data = await api(`/api/projects/${state.activeProjectId}/sessions?all=1`);
    state.allSessions = data.sessions || [];
  } catch (err) {
    state.allSessions = [];
    toast(err.message, 'error');
  }
  renderSessionsList();
}

function sessionMatchesFilter(session) {
  if (!$('sessionsShowAll').checked && !session.is_open) return false;
  const q = $('sessionSearch').value.trim().toLowerCase();
  if (!q) return true;
  return `${session.title || ''} ${session.model || ''}`.toLowerCase().includes(q);
}

function renderSessionsList() {
  const list = $('sessionsList');
  if (!list) return;
  list.innerHTML = '';
  const all = Array.isArray(state.allSessions) ? state.allSessions : [];
  const rows = all.filter(sessionMatchesFilter);
  $('sessionsCount').textContent = `${rows.length} of ${all.length} chats`;

  if (!rows.length) {
    const empty = document.createElement('div');
    empty.className = 'manage-empty';
    empty.textContent = all.length ? 'No chat matches this filter.' : 'This project has no chats yet.';
    list.appendChild(empty);
    return;
  }

  for (const session of rows) {
    const row = document.createElement('div');
    row.className = 'manage-row' + (session.id === state.activeSessionId ? ' active' : '')
      + (session.is_open ? '' : ' closed');

    const main = document.createElement('div');
    main.className = 'manage-row-main';

    const titleRow = document.createElement('div');
    titleRow.className = 'manage-row-title';

    const title = document.createElement('span');
    title.className = 'manage-row-name';
    title.textContent = session.title || 'New Chat';
    title.title = 'Double-click to rename';
    title.addEventListener('dblclick', () => startRowRename(title, session));
    titleRow.appendChild(title);

    if (session.is_open) {
      const badge = document.createElement('span');
      badge.className = 'manage-badge';
      badge.textContent = 'open';
      titleRow.appendChild(badge);
    }
    if (session.id === state.activeSessionId) {
      const badge = document.createElement('span');
      badge.className = 'manage-badge accent';
      badge.textContent = 'active';
      titleRow.appendChild(badge);
    }
    main.appendChild(titleRow);

    const meta = document.createElement('div');
    meta.className = 'manage-row-meta';
    meta.textContent = [
      session.mode || 'PLAN',
      session.model || 'no model',
      `${formatNum(session.tokens_in)} in / ${formatNum(session.tokens_out)} out`,
      formatCost(session.cost_usd),
      session.updated_at ? formatWhen(session.updated_at) : '',
    ].filter(Boolean).join(' · ');
    main.appendChild(meta);
    row.appendChild(main);

    const actions = document.createElement('div');
    actions.className = 'manage-row-actions';

    const renameBtn = document.createElement('button');
    renameBtn.type = 'button';
    renameBtn.className = 'manage-icon-btn';
    renameBtn.title = 'Rename chat';
    renameBtn.innerHTML = '<i class="bi bi-pencil"></i>';
    renameBtn.addEventListener('click', () => startRowRename(title, session));
    actions.appendChild(renameBtn);

    const openBtn = document.createElement('button');
    openBtn.type = 'button';
    openBtn.className = 'manage-icon-btn';
    openBtn.title = session.is_open ? 'Switch to this chat' : 'Reopen chat';
    openBtn.innerHTML = '<i class="bi bi-box-arrow-in-right"></i>';
    openBtn.addEventListener('click', () => openSessionFromModal(session));
    actions.appendChild(openBtn);

    const closeBtn = document.createElement('button');
    closeBtn.type = 'button';
    closeBtn.className = 'manage-icon-btn';
    closeBtn.title = 'Close tab (keeps the chat)';
    closeBtn.innerHTML = '<i class="bi bi-x-lg"></i>';
    closeBtn.disabled = !session.is_open;
    closeBtn.addEventListener('click', async () => {
      await closeSession(session.id);
      await loadAllSessions();
    });
    actions.appendChild(closeBtn);

    const deleteBtn = document.createElement('button');
    deleteBtn.type = 'button';
    deleteBtn.className = 'manage-icon-btn danger';
    deleteBtn.title = 'Delete chat permanently';
    deleteBtn.innerHTML = '<i class="bi bi-trash3"></i>';
    deleteBtn.addEventListener('click', () => deleteSession(session));
    actions.appendChild(deleteBtn);

    row.appendChild(actions);
    list.appendChild(row);
  }
}

function startRowRename(label, session) {
  const input = document.createElement('input');
  input.className = 'manage-rename-input';
  input.type = 'text';
  input.value = session.title || '';
  input.spellcheck = false;
  label.replaceWith(input);
  input.focus();
  input.select();
  let done = false;
  const finish = async (commit) => {
    if (done) return;
    done = true;
    const next = input.value.trim();
    label.textContent = session.title || 'New Chat';
    input.replaceWith(label);
    if (!commit || !next || next === session.title) return;
    try {
      const data = await api(`/api/sessions/${session.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ title: next }),
      });
      syncSession(data.session);
      if (session.id === state.activeSessionId && data.session.title) {
        document.title = `${data.session.title} · Forge`;
      }
      await loadAllSessions();
    } catch (err) {
      toast(err.message, 'error');
    }
  };
  input.addEventListener('keydown', (e) => {
    e.stopPropagation();
    if (e.key === 'Enter') {
      e.preventDefault();
      finish(true);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      finish(false);
    }
  });
  input.addEventListener('blur', () => finish(true));
}

async function openSessionFromModal(session) {
  try {
    if (!session.is_open) {
      const data = await api(`/api/sessions/${session.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ is_open: true }),
      });
      const exists = state.sessions.some((s) => s.id === session.id);
      if (!exists) {
        state.sessions.push(data.session);
        persistTabOrder();
      }
    }
    if (sessionsModal) sessionsModal.hide();
    await loadSession(session.id);
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function deleteSession(session) {
  const ok = window.confirm(`Delete "${session.title || 'New Chat'}" permanently? This cannot be undone.`);
  if (!ok) return;
  try {
    // The server stops the agent and lets it persist before deleting.
    await api(`/api/sessions/${session.id}`, { method: 'DELETE' });
    dropView(session.id);
    runIndex.delete(session.id);
    state.sessions = state.sessions.filter((s) => s.id !== session.id);
    state.allSessions = state.allSessions.filter((s) => s.id !== session.id);
    if (session.id === state.activeSessionId) {
      localStorage.removeItem(`forge.session.${state.activeProjectId}`);
      renderTabs();
      if (state.sessions.length) await loadSession(state.sessions[0].id);
      else clearSession();
    } else {
      renderTabs();
    }
    persistTabOrder();
    renderSessionsList();
    toast('Chat deleted', 'success');
  } catch (err) {
    toast(err.message, 'error');
  }
}

async function deleteSessionPermanent(session) {
  const ok = window.confirm(`Delete "${session.title || 'New Chat'}" permanently? This cannot be undone.`);
  if (!ok) return false;
  try {
    await api(`/api/sessions/${session.id}`, { method: 'DELETE' });
    dropView(session.id);
    state.sessions = state.sessions.filter((s) => s.id !== session.id);
    state.allSessions = state.allSessions.filter((s) => s.id !== session.id);
    return true;
  } catch (err) {
    toast(err.message, 'error');
    return false;
  }
}

/* ── Running agents ──────────────────────────────────────────────── */

// The agents modal lists every live run of every project. It reads the server
// index (runIndex) instead of the local views, so it also shows agents whose
// project was never opened in this browser session.
async function openAgentsModal() {
  if (agentsModal) agentsModal.show();
  await refreshRunIndex();
  renderAgentsList();
}

// startAgentsWatch keeps the list fresh while the modal is open; the global
// watcher stays at a slower cadence for the tab bar.
function startAgentsWatch() {
  stopAgentsWatch();
  refreshRunIndex();
  agentsTimer = setInterval(() => {
    if (document.hidden) return;
    refreshRunIndex();
  }, 2000);
  renderAgentsList();
}

function stopAgentsWatch() {
  if (agentsTimer) clearInterval(agentsTimer);
  agentsTimer = null;
}

// updateAgentsBadge shows how many agents are working right now.
function updateAgentsBadge() {
  const badge = $('agentsBadge');
  if (!badge) return;
  const n = runIndex.size;
  badge.textContent = n > 9 ? '9+' : String(n);
  badge.classList.toggle('d-none', n === 0);
  $('agentsBtn').classList.toggle('has-agents', n > 0);
  $('agentsBtn').title = n
    ? `${n} agent${n === 1 ? '' : 's'} running — click to see them (Ctrl+Shift+A)`
    : 'Running agents (Ctrl+Shift+A)';
}

// liveRuns returns the runs of the index, most recently started last.
function liveRuns() {
  return [...runIndex.values()].sort((a, b) => String(a.started_at).localeCompare(String(b.started_at)));
}

function renderAgentsList() {
  const list = $('agentsList');
  if (!list) return;
  const runs = liveRuns();
  $('agentsCount').textContent = runs.length
    ? `${runs.length} agent${runs.length === 1 ? '' : 's'} running`
    : '';
  list.innerHTML = '';
  if (!runs.length) {
    const empty = document.createElement('div');
    empty.className = 'manage-empty';
    empty.textContent = 'No agent is running right now.';
    list.appendChild(empty);
    return;
  }

  let lastProject = null;
  for (const run of runs) {
    if (run.project_id !== lastProject) {
      lastProject = run.project_id;
      const head = document.createElement('div');
      head.className = 'agents-project';
      head.textContent = run.project_name || `Project #${run.project_id}`;
      list.appendChild(head);
    }
    list.appendChild(agentsRow(run));
  }
}

function agentsRow(run) {
  const mine = run.session_id === state.activeSessionId;
  const row = document.createElement('div');
  row.className = 'manage-row agents-row' + (mine ? ' active' : '');

  const dot = document.createElement('span');
  dot.className = 'tab-pulse agents-pulse';
  row.appendChild(dot);

  const main = document.createElement('div');
  main.className = 'manage-row-main';

  const titleRow = document.createElement('div');
  titleRow.className = 'manage-row-title';
  const title = document.createElement('span');
  title.className = 'manage-row-name';
  title.textContent = run.session_title || `Chat #${run.session_id}`;
  title.addEventListener('click', () => openChatFromRun(run));
  titleRow.appendChild(title);
  const mode = document.createElement('span');
  mode.className = 'manage-badge' + (run.mode === 'BUILD' ? ' accent' : '');
  mode.textContent = run.mode || 'PLAN';
  titleRow.appendChild(mode);
  if (mine) {
    const badge = document.createElement('span');
    badge.className = 'manage-badge';
    badge.textContent = 'on screen';
    titleRow.appendChild(badge);
  }
  main.appendChild(titleRow);

  const meta = document.createElement('div');
  meta.className = 'manage-row-meta agents-meta';
  const parts = [
    run.activity ? String(run.activity) : 'working',
    run.round > 0 ? `round ${run.round}` : '',
    elapsedSince(run.started_at),
  ].filter(Boolean);
  meta.textContent = parts.join(' · ');
  main.appendChild(meta);
  row.appendChild(main);

  const actions = document.createElement('div');
  actions.className = 'manage-row-actions';

  const openBtn = document.createElement('button');
  openBtn.type = 'button';
  openBtn.className = 'manage-icon-btn';
  openBtn.title = 'Open this chat';
  openBtn.innerHTML = '<i class="bi bi-box-arrow-in-right"></i>';
  openBtn.addEventListener('click', () => openChatFromRun(run));
  actions.appendChild(openBtn);

  const stopBtn = document.createElement('button');
  stopBtn.type = 'button';
  stopBtn.className = 'manage-icon-btn danger';
  stopBtn.title = 'Stop this agent (keeps what it already produced)';
  stopBtn.innerHTML = '<i class="bi bi-stop-circle"></i>';
  stopBtn.addEventListener('click', () => stopRunFromModal(run, stopBtn));
  actions.appendChild(stopBtn);

  row.appendChild(actions);
  return row;
}

function elapsedSince(iso) {
  const ts = Date.parse(iso);
  if (!Number.isFinite(ts)) return '';
  return formatElapsed(Math.max(0, Date.now() - ts));
}

// openChatFromRun jumps to the chat of a running agent, switching project and
// reopening the tab when needed. The run itself is untouched: the view either
// replays what it missed or renders the persisted transcript.
async function openChatFromRun(run) {
  try {
    if (run.project_id !== state.activeProjectId) {
      const project = await openProject(run.project_id);
      if (!project) {
        toast('That project is not registered any more', 'error');
        return;
      }
    }
    if (!state.sessions.some((s) => s.id === run.session_id)) {
      // A background chat may be closed as a tab: reopen it so it stays visible.
      const data = await api(`/api/sessions/${run.session_id}`, {
        method: 'PATCH',
        body: JSON.stringify({ is_open: true }),
      });
      state.sessions.push(data.session);
      persistTabOrder();
      renderTabs();
    }
    if (agentsModal) agentsModal.hide();
    await loadSession(run.session_id);
  } catch (err) {
    toast(err.message || 'Could not open that chat', 'error');
  }
}

async function stopRunFromModal(run, btn) {
  const label = run.session_title || `Chat #${run.session_id}`;
  if (!window.confirm(`Stop the agent in "${label}"? It keeps what it already produced.`)) return;
  if (btn) btn.disabled = true;
  const view = viewFor(run.session_id);
  if (view) view.stopRequested = true;
  try {
    await api(`/api/sessions/${run.session_id}/stop`, { method: 'POST', body: JSON.stringify({}) });
    runIndex.delete(run.session_id);
    afterRunIndexChange();
    toast(`Stopping "${label}"…`, 'success');
  } catch (err) {
    toast(err.message || 'Could not stop the agent', 'error');
    if (btn) btn.disabled = false;
    if (view) view.stopRequested = false;
  }
  scheduleChrome();
}

function formatWhen(iso) {
  const ts = Date.parse(iso);
  if (!Number.isFinite(ts)) return '';
  const diff = Date.now() - ts;
  const min = Math.round(diff / 60000);
  if (min < 1) return 'just now';
  if (min < 60) return `${min}m ago`;
  const hours = Math.round(min / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(ts).toLocaleDateString();
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

/* ── Browser notifications ───────────────────────────────────────── */

// notificationsSupported reports whether this browser can show system
// notifications at all. Safari on iOS and some embedded webviews cannot.
function notificationsSupported() {
  return typeof window !== 'undefined' && 'Notification' in window;
}

// notificationsWanted is true when the user enabled the setting. It is the
// single guard shared by the permission prompt and the actual notification.
function notificationsWanted() {
  return !!state.config?.defaults?.browser_notifications;
}

// notificationStatus explains the current permission in the settings modal so
// a silently blocked browser is visible instead of looking like a no-op.
function renderNotificationStatus() {
  const el = $('notificationStatus');
  const help = $('notificationHelp');
  if (!el) return;
  if (!notificationsSupported()) {
    el.className = 'form-text text-warning';
    el.textContent = 'This browser does not support notifications.';
    return;
  }
  const perm = Notification.permission;
  if (perm === 'granted') {
    el.className = 'form-text text-success';
    el.textContent = 'Permission granted — notifications will appear while Forge is in the background.';
  } else if (perm === 'denied') {
    el.className = 'form-text text-warning';
    el.textContent = 'Blocked by the browser. Reset the permission from the site settings (padlock icon) and enable it again.';
  } else {
    el.className = 'form-text';
    el.textContent = 'Permission not requested yet — enable this switch and allow the browser prompt.';
  }
  if (help) {
    help.textContent = perm === 'denied'
      ? 'The browser refused notifications for this site. Open the padlock icon in the address bar, allow notifications, then toggle this switch again.'
      : 'Notifications appear only while Forge is in a background tab or window; a turn that ends in the tab you are looking at just shows the in-app toast.';
  }
}

// ensureNotificationPermission asks for the permission from a user gesture.
// Browsers ignore (and silently deny) requestPermission calls made without
// transient user activation, which is why this is only invoked from clicks.
async function ensureNotificationPermission() {
  if (!notificationsSupported()) return 'unsupported';
  if (Notification.permission !== 'default') return Notification.permission;
  try {
    return await Notification.requestPermission();
  } catch {
    return Notification.permission;
  }
}

// notifyBrowser shows a system notification for a finished turn. It never asks
// for permission: that only happens from the settings UI, where a user gesture
// exists. The visibility check is about the whole tab, so a turn that ends in
// the chat on screen still notifies while the user is in another window.
function notifyBrowser(title, body) {
  if (!notificationsWanted()) return;
  if (!notificationsSupported()) return;
  if (Notification.permission !== 'granted') return;
  // The user is looking at Forge: the in-app toast is enough.
  if (!document.hidden && document.hasFocus()) return;
  try {
    const n = new Notification(title, {
      body,
      icon: 'images/logo.png',
      tag: 'forge-turn-end',
      renotify: true,
    });
    n.onclick = () => {
      window.focus();
      n.close();
    };
  } catch {
    // A revoked permission or a platform refusal must never break the turn.
  }
}

/* ── Events ──────────────────────────────────────────────────────── */

function bindEvents() {
  // Wrapped so the click event is not mistaken for a project argument.
  $('addProjectBtn').addEventListener('click', () => openProjectModal(null, { create: true }));
  $('themeBtn').addEventListener('click', toggleTheme);
  $('collapsePanelBtn').addEventListener('click', () => setPanelHidden(true));
  $('showPanelBtn').addEventListener('click', () => setPanelHidden(false));
  $('settingsBtn').addEventListener('click', openSettings);
  $('manageProjectBtn').addEventListener('click', () => {
    if (!state.activeProjectId) {
      toast('Register or select a project first', 'error');
      return;
    }
    openProjectModal();
  });
  $('newSessionBtn').addEventListener('click', () => createSession());
  $('workspacePath').addEventListener('click', () => {
    const path = state.activeProject && state.activeProject.path;
    if (path) copyToClipboard(path, $('workspacePath'));
  });
  $('workspaceRefreshBtn').addEventListener('click', () => refreshGitInfo(state.activeProjectId));
  $('manageSessionsBtn').addEventListener('click', openSessionsModal);
  $('agentsBtn').addEventListener('click', openAgentsModal);
  // The run summary is a shortcut to the same list of live agents.
  const runSummary = $('runSummary');
  runSummary.addEventListener('click', openAgentsModal);
  runSummary.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
    openAgentsModal();
  });
  $('deleteProjectBtn').addEventListener('click', deleteProject);
  $('sessionSearch').addEventListener('input', renderSessionsList);
  $('sessionsShowAll').addEventListener('change', () => {
    localStorage.setItem('forge.showClosed', $('sessionsShowAll').checked ? '1' : '0');
    renderSessionsList();
  });
  $('sendBtn').addEventListener('click', sendMessage);
  // Stop disables itself through the chrome projection (stopRequested), so a
  // second click is impossible without leaving the button stuck for the next run.
  $('stopBtn').addEventListener('click', () => stopStream());

  $('promptInput').addEventListener('input', () => {
    resizePrompt();
    updateSendState();
    syncMentionPicker();
  });
  $('promptInput').addEventListener('click', syncMentionPicker);
  $('promptInput').addEventListener('keydown', (e) => {
    if (state.filePicker.open && handlePickerKey(e)) return;
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendMessage();
    }
  });
  document.addEventListener('mousedown', (e) => {
    if (!state.filePicker.open) return;
    const picker = $('filePicker');
    if (picker && picker.contains(e.target)) return;
    if (e.target === $('promptInput')) return;
    closeFilePicker();
  });
  $('promptInput').addEventListener('blur', () => {
    setTimeout(() => {
      const el = document.activeElement;
      const picker = $('filePicker');
      if (picker && picker.contains(el)) return;
      if (el === $('promptInput')) return;
      closeFilePicker();
    }, 120);
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

  // The progress chip is the checklist's shortcut. It is inert below 1200px,
  // where applyChecklistState removes both attributes, so the handlers can stay
  // unconditional.
  const chip = $('progressChip');
  chip.addEventListener('click', focusChecklist);
  chip.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
    focusChecklist();
  });
  // Crossing the 1200px breakpoint makes the chip interactive or inert again;
  // repainting is all it takes because applyChecklistState owns the attributes.
  const panelQuery = window.matchMedia ? window.matchMedia('(min-width: 1200px)') : null;
  if (panelQuery) {
    const onBreakpoint = () => scheduleChrome();
    if (panelQuery.addEventListener) panelQuery.addEventListener('change', onBreakpoint);
    else if (panelQuery.addListener) panelQuery.addListener(onBreakpoint);
  }

  $('projectForm').addEventListener('submit', saveProject);
  $('settingsForm').addEventListener('submit', saveSettings);
  $('renameTabForm').addEventListener('submit', saveRenameTab);
  $('resetDefaultsBtn').addEventListener('click', resetDefaults);
  // Toggling the switch is the user gesture that lets the browser show its
  // permission prompt. Without a gesture Chrome denies it silently, which is
  // why the prompt never happens from the stream callback.
  $('browserNotifications').addEventListener('change', async (e) => {
    const on = e.target.checked;
    if (on) {
      const perm = await ensureNotificationPermission();
      if (perm === 'denied') {
        e.target.checked = false;
        toast('Notifications are blocked for this site — allow them from the padlock icon in the address bar', 'error');
      } else if (perm === 'unsupported') {
        e.target.checked = false;
        toast('This browser cannot show notifications', 'error');
      }
    }
    renderNotificationStatus();
  });
  // Keep the derived token budget in sync while the two knobs are edited.
  ['defaultModel', 'historyPrunePercent', 'historyPruneMaxTokens'].forEach((id) => {
    const el = $(id);
    if (el) el.addEventListener('input', renderEffectiveBudget);
  });
  $('activeProviderSelect').addEventListener('change', () => {
    const id = $('activeProviderSelect').value;
    if (id && id !== state.activeProviderId) activateProvider(id);
  });
  $('sessionProviderSelect').addEventListener('change', () => {
    if (!state.session) return;
    switchSessionProvider($('sessionProviderSelect').value);
  });
  $('addProviderBtn').addEventListener('click', () => openProviderEditor(null));
  $('closeProviderEditor').addEventListener('click', closeProviderEditor);
  $('cancelProviderBtn').addEventListener('click', closeProviderEditor);
  $('saveProviderBtn').addEventListener('click', saveProviderEditor);
  $('deleteProviderBtn').addEventListener('click', deleteProviderEditor);
  $('testProviderBtn').addEventListener('click', testProviderEditor);
  $('providerPresets').addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-preset]');
    if (btn) applyProviderPreset(btn.dataset.preset);
  });
  $('toggleProviderKey').addEventListener('click', () => {
    const input = $('providerAPIKey');
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
  if (code === 'KeyA' || key === 'a') {
    e.preventDefault();
    openAgentsModal();
    return;
  }
  if (key === 'arrowup') {
    if (modalOpen) return;
    e.preventDefault();
    navigateAnswerStart(-1);
    return;
  }
  if (key === 'arrowdown') {
    if (modalOpen) return;
    e.preventDefault();
    navigateAnswerStart(1);
    return;
  }
  // The arrows belong to the answers the agent produced; the user's own messages
  // keep a Vim-style pair so both axes stay reachable without a modifier storm.
  if (code === 'KeyK' || key === 'k') {
    if (modalOpen) return;
    e.preventDefault();
    navigateUserMessage(-1);
    return;
  }
  if (code === 'KeyJ' || key === 'j') {
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
  const all = [...messagesEl().querySelectorAll('.msg')];
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

// stopStream asks the server to cancel the active chat's run. Detaching the
// event stream would not stop the agent any more, because runs live on the
// server; the explicit endpoint is what ends the turn. The chat is resolved from
// the screen, so the button can never stop a different chat than the one shown.
async function stopStream() {
  const sessionId = state.activeSessionId || state.session?.id;
  if (!sessionId) return;
  const view = viewFor(sessionId);
  if (view) view.stopRequested = true;
  scheduleChrome();
  try {
    await api(`/api/sessions/${sessionId}/stop`, { method: 'POST', body: JSON.stringify({}) });
  } catch (err) {
    toast(err.message || 'Could not stop the agent', 'error');
    if (view) view.stopRequested = false;
  } finally {
    // Ask the server what is still running: the chrome and the tab bar follow
    // the answer, not the click.
    await refreshRunIndex();
    scheduleChrome();
  }
}
