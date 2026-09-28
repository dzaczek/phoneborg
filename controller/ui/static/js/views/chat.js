// Chat: send a test chat completion from the browser, streamed, without a
// terminal. Proxied through POST /admin/chat/completions (ADR-018), which
// routes the request through the normal gateway path (routing, failover,
// affinity, usage, metrics) but authenticates with the admin token, so this
// works even from a remote browser that has only the admin token and no API
// key (-gateway-access local). Conversations (messages, model, system
// prompt) are saved to this browser's localStorage under "phoneborg.chat.v1"
// so they survive a view switch or reload; nothing is sent anywhere but as
// part of the next turn. A reply keeps streaming when the user switches to
// another view or conversation and is shown live again on return; only a
// reload/close (or Stop) ends it early, keeping the partial reply marked as
// interrupted.
import { get, chatCompletionsStream } from '../api.js';
import { h, fill, int, tps, ago } from '../dom.js';
import { confirmDialog } from '../ui.js';

const GROUPS = [
  { kind: 'model', label: 'Models' },
  { kind: 'auto', label: 'Auto' },
  { kind: 'pool', label: 'Pools' },
  { kind: 'node', label: 'Nodes' },
];

const STORAGE_KEY = 'phoneborg.chat.v1';
const MAX_CONVERSATIONS = 50;
const TITLE_MAX = 48;

function modelLabel(m) {
  if (m.kind === 'node') return `${m.id} — ${m.model || 'no model'}${m.ready === false ? ' (not ready)' : ''}`;
  return `${m.id} (${m.nodes} node${m.nodes === 1 ? '' : 's'})`;
}

function formatMs(ms) {
  return ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(1)} s`;
}

// statsLine summarises a finished turn: requested model (and, when known,
// the concrete model/node that actually served it), token counts and speed
// from the response's usage/timings (OpenAI usage or llama.cpp timings,
// whichever the backend sent), and latency measured in the browser.
function statsLine(model, node, nodesByID, stats, latencyMs) {
  const n = nodesByID[node];
  const nodeLabel = node ? (n && n.alias) || node : 'unknown';
  const served = n && n.last_heartbeat && n.last_heartbeat.runtime && n.last_heartbeat.runtime.model;
  const u = (stats && stats.usage) || {};
  const t = (stats && stats.timings) || {};
  const promptTok = u.prompt_tokens ?? (t.prompt_n != null ? (t.cache_n || 0) + t.prompt_n : null);
  const complTok = u.completion_tokens ?? t.predicted_n ?? null;
  const genTps = t.predicted_per_second || null;
  return [
    `model ${model}` + (served && served !== model ? ` → ${served}` : ''),
    `node ${nodeLabel}`,
    promptTok != null ? `${int(promptTok)} prompt tok` : null,
    complTok != null ? `${int(complTok)} completion tok` : null,
    genTps ? `${tps(genTps)} tok/s` : null,
    formatMs(latencyMs) + ' latency',
  ].filter(Boolean).join(' · ');
}

// readSSE consumes the streamed response body, calling onDelta with the
// accumulated assistant text and reasoning as they grow. Qwen3-style
// "thinking" models send reasoning as `delta.reasoning_content`, often with
// an empty `content` until the answer itself starts. Returns the final text,
// reasoning and the last usage/timings block seen (llama.cpp sends it on the
// final event).
async function readSSE(res, onDelta) {
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let content = '';
  let reasoning = '';
  let stats = null;
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let nl;
    while ((nl = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (!line.startsWith('data:')) continue;
      const data = line.slice(5).trim();
      if (!data || data === '[DONE]') continue;
      let evt;
      try { evt = JSON.parse(data); } catch { continue; }
      const delta = evt.choices && evt.choices[0] && evt.choices[0].delta;
      if (delta && delta.content) { content += delta.content; onDelta(content, reasoning); }
      if (delta && delta.reasoning_content) { reasoning += delta.reasoning_content; onDelta(content, reasoning); }
      if (evt.usage || evt.timings) stats = { usage: evt.usage, timings: evt.timings };
    }
  }
  return { content, reasoning, stats };
}

// ---- conversation storage (localStorage, namespaced; in-memory only if it
// throws, e.g. private browsing with storage disabled) ----

const nowISO = () => new Date().toISOString();
const genId = () => 'c' + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
const truncate = (s, n) => (s.length > n ? s.slice(0, n - 1).trimEnd() + '…' : s);
const byUpdatedDesc = (a, b) => (a.updatedAt < b.updatedAt ? 1 : -1);

function loadStore() {
  try {
    const parsed = JSON.parse(localStorage.getItem(STORAGE_KEY));
    if (parsed && Array.isArray(parsed.conversations)) {
      // A reply still marked streaming was cut off by a reload or close.
      for (const c of parsed.conversations) {
        for (const m of c.messages || []) {
          if (m.streaming) { delete m.streaming; m.interrupted = true; }
        }
      }
      return parsed;
    }
  } catch { /* unavailable or corrupt: start fresh, in-memory only */ }
  return { conversations: [], activeId: null };
}

// Kept at module scope so conversations survive a view switch even when
// localStorage itself is unavailable (persist() below then just no-ops).
let store = loadStore();

function persist() {
  if (store.conversations.length > MAX_CONVERSATIONS) {
    store.conversations = store.conversations.slice().sort(byUpdatedDesc).slice(0, MAX_CONVERSATIONS);
  }
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(store));
  } catch {
    // Likely quota exceeded: drop the oldest conversation and retry once;
    // if it still fails, keep working with the in-memory store only.
    if (store.conversations.length > 1) {
      store.conversations = store.conversations.slice().sort(byUpdatedDesc);
      store.conversations.pop();
      try { localStorage.setItem(STORAGE_KEY, JSON.stringify(store)); } catch { /* in-memory only */ }
    }
  }
}

// inflight is the one reply being streamed, if any: {conv, msg, abortCtl,
// view}. It lives at module scope, not in a view, so leaving the Chat view
// or switching conversations does not stop it; the reply keeps streaming
// into its conversation and the chat view showing it attaches `view`
// ({onDelta, onDone}) to follow it live.
let inflight = null;

function startReply(conv, msg, body) {
  const f = { conv, msg, abortCtl: new AbortController(), view: null };
  inflight = f;
  const started = performance.now();
  let lastSave = 0;
  (async () => {
    try {
      const res = await chatCompletionsStream(body, f.abortCtl.signal);
      msg.node = res.headers.get('X-PhoneBorg-Node') || '';
      const { content, reasoning, stats } = await readSSE(res, (partial, partialReasoning) => {
        msg.content = partial;
        msg.reasoning = partialReasoning;
        if (f.view) f.view.onDelta();
        if (performance.now() - lastSave > 2000) { lastSave = performance.now(); persist(); }
      });
      Object.assign(msg, { content, reasoning, stats, latencyMs: performance.now() - started });
    } catch (ex) {
      if (!msg.interrupted) { msg.failed = true; msg.error = ex.message; }
    } finally {
      delete msg.streaming;
      conv.updatedAt = nowISO();
      inflight = null;
      persist();
      if (f.view) f.view.onDone();
    }
  })();
}

// stopReply ends the reply in progress, keeping what came in so far.
function stopReply(byUser) {
  if (!inflight) return;
  inflight.msg.interrupted = true;
  if (byUser) inflight.msg.stopped = true;
  inflight.abortCtl.abort();
}

// A reload or close cannot keep the stream: save the partial reply now,
// synchronously, since the async cleanup will not get to run.
window.addEventListener('beforeunload', () => {
  if (!inflight) return;
  delete inflight.msg.streaming;
  stopReply(false);
  persist();
});

export default function chatView() {
  const model = h('select', { name: 'model' });
  const system = h('textarea', { name: 'system', rows: 2, placeholder: '(optional) system prompt', spellcheck: 'false' });
  const transcript = h('div.chat-transcript');
  const input = h('textarea', { name: 'message', rows: 2, placeholder: 'Send a message… (Enter to send, Shift+Enter for a new line)' });
  const send = h('button.primary', { type: 'submit' }, 'Send');
  // Stop ends a reply the model will not finish on its own; what came in so
  // far is kept, marked as stopped.
  const stop = h('button.danger', { type: 'button', hidden: true }, 'Stop');
  const newChat = h('button.small', { type: 'button' }, 'New chat');
  const reloadModels = h('button.small', { type: 'button' }, 'Refresh models');
  const hint = h('p.muted.small', null, 'Model, node and basic stats appear under each reply.');

  const historyList = h('div.chat-history-list');
  const history = h('details.chat-history', { open: true },
    h('summary', null, 'Conversations'),
    h('div.chat-history-body', null, h('div.row', null, newChat), historyList));

  const form = h('form.chat-row', null, input, send, stop);
  const el = h('section', null,
    h('div.page-head', null, h('h1', null, 'Chat')),
    h('div.card.section.chat-layout', null,
      history,
      h('div.row', null, h('label', { for: 'chat-model' }, 'Model'), model, reloadModels),
      system,
      transcript,
      form,
      hint));
  model.id = 'chat-model';

  let nodesByID = {};
  let modelsAvailable = false;

  // active is the open conversation: {id, title, model, system, createdAt,
  // updatedAt, messages: [{role, content, reasoning?, model?, node?, stats?,
  // latencyMs?, failed?, error?, interrupted?}]}. Restored from the last
  // session, or a fresh one if there is none yet.
  let active = store.conversations.find((c) => c.id === store.activeId) || store.conversations.slice().sort(byUpdatedDesc)[0];
  if (!active) {
    active = { id: genId(), title: '', model: '', system: '', createdAt: nowISO(), updatedAt: nowISO(), messages: [] };
    store.conversations.push(active);
    store.activeId = active.id;
    persist();
  }

  // syncBusy reflects whether a reply is streaming (in any conversation):
  // one at a time, and Stop ends it wherever it is.
  function syncBusy() {
    const busy = inflight !== null;
    send.disabled = busy || !modelsAvailable;
    input.disabled = busy;
    send.hidden = busy;
    stop.hidden = !busy;
  }

  function addTurn(role, text) {
    const bubble = h('div.chat-msg', { class: role }, text);
    transcript.append(h('div.chat-turn', { class: role }, bubble));
    transcript.scrollTop = transcript.scrollHeight;
    return bubble;
  }

  // renderStoredMessage rebuilds the static DOM for a completed message when
  // a conversation is (re)opened.
  function renderStoredMessage(msg) {
    const bubble = h('div.chat-msg', { class: msg.role }, msg.content);
    if (msg.role === 'user') return h('div.chat-turn.user', null, bubble);
    const think = msg.reasoning
      ? h('details.chat-think', null, h('summary', null, 'Thinking'), h('div.chat-think-body', null, msg.reasoning))
      : null;
    const metaText = msg.stopped ? 'Stopped.'
      : msg.interrupted ? 'Interrupted before it finished.'
      : msg.stats ? statsLine(msg.model, msg.node, nodesByID, msg.stats, msg.latencyMs) : null;
    const meta = metaText ? h('div.chat-meta', null, metaText) : null;
    const err = msg.failed ? h('div.chat-meta.chat-error', null, msg.error) : null;
    return h('div.chat-turn', { class: 'assistant' + (msg.failed ? ' chat-failed' : '') }, think, bubble, meta, err);
  }

  function historyItem(conv) {
    const open = h('button.chat-history-open', { type: 'button', title: conv.title || 'New chat' },
      h('span.chat-history-title', null, conv.title || 'New chat'),
      h('span.chat-history-meta', null, conv.model || '—'), ago(conv.updatedAt));
    open.addEventListener('click', () => { if (conv.id !== active.id) openConversation(conv.id); });
    const del = h('button.chat-history-del', { type: 'button', 'aria-label': 'Delete conversation', title: 'Delete' }, '×');
    del.addEventListener('click', () => deleteConversation(conv.id));
    return h('div.chat-history-item', { class: conv.id === active.id ? 'active' : null }, open, del);
  }

  function renderHistoryList() {
    const items = store.conversations.slice().sort(byUpdatedDesc).map(historyItem);
    fill(historyList, items.length ? items : h('p.muted.small', null, 'No conversations yet.'));
  }

  // liveTurn renders the streaming reply and hooks this view to it, so its
  // text grows as tokens arrive and it becomes a normal message when done.
  function liveTurn() {
    const msg = inflight.msg;
    const thinkBody = h('div.chat-think-body', null, msg.reasoning || '');
    const thinkWrap = h('details.chat-think', { hidden: !msg.reasoning }, h('summary', null, 'Thinking'), thinkBody);
    const bubble = h('div.chat-msg.assistant', null, msg.content || '');
    const turn = h('div.chat-turn.assistant', null, thinkWrap, bubble);
    inflight.view = {
      onDelta() {
        if (msg.reasoning) { thinkWrap.hidden = false; thinkBody.textContent = msg.reasoning; }
        bubble.textContent = msg.content;
        transcript.scrollTop = transcript.scrollHeight;
      },
      onDone() {
        turn.replaceWith(renderStoredMessage(msg));
        syncBusy();
        renderHistoryList();
      },
    };
    return turn;
  }

  // renderActive paints the whole view (system prompt, transcript, history
  // list) from the current `active` conversation, following its reply live
  // if one is streaming.
  function renderActive() {
    if (inflight) inflight.view = { onDelta() {}, onDone() { syncBusy(); renderHistoryList(); } };
    system.value = active.system || '';
    fill(transcript, active.messages.map((m) => (inflight && m === inflight.msg ? liveTurn() : renderStoredMessage(m))));
    transcript.scrollTop = transcript.scrollHeight;
    renderHistoryList();
    syncBusy();
  }

  function openConversation(id) {
    const conv = store.conversations.find((c) => c.id === id);
    if (!conv) return;
    active = conv;
    store.activeId = id;
    persist();
    renderActive();
  }

  function newConversation() {
    active = { id: genId(), title: '', model: model.value || '', system: '', createdAt: nowISO(), updatedAt: nowISO(), messages: [] };
    store.conversations.push(active);
    store.activeId = active.id;
    persist();
    renderActive();
    input.focus();
  }

  async function deleteConversation(id) {
    const conv = store.conversations.find((c) => c.id === id);
    if (!conv) return;
    const ok = await confirmDialog({
      title: `Delete "${conv.title || 'New chat'}"?`,
      body: 'It is removed from this browser. This cannot be undone.',
      confirm: 'Delete', danger: true,
    });
    if (!ok) return;
    if (inflight && inflight.conv.id === id) stopReply(false);
    store.conversations = store.conversations.filter((c) => c.id !== id);
    if (id === active.id) {
      const next = store.conversations.slice().sort(byUpdatedDesc)[0];
      if (next) openConversation(next.id); else newConversation();
      return;
    }
    persist();
    renderHistoryList();
  }

  async function submit() {
    const text = input.value.trim();
    if (!text || inflight || !model.value) return;
    input.value = '';
    if (!active.title) active.title = truncate(text, TITLE_MAX);
    active.messages.push({ role: 'user', content: text });
    active.updatedAt = nowISO();
    addTurn('user', text);
    persist();
    renderHistoryList();

    // Only completed assistant turns become context for the next request,
    // same as a failed or interrupted one was never sent before.
    const messages = active.messages
      .filter((m) => m.role === 'user' || (m.role === 'assistant' && !m.failed && !m.interrupted))
      .map((m) => ({ role: m.role, content: m.content }));
    const chosenModel = model.value;
    const systemText = system.value.trim();
    active.model = chosenModel;
    active.system = systemText;
    if (systemText) messages.unshift({ role: 'system', content: systemText });

    const assistantMsg = { role: 'assistant', content: '', reasoning: '', model: chosenModel, streaming: true };
    active.messages.push(assistantMsg);
    persist();
    startReply(active, assistantMsg, { model: chosenModel, messages, stream: true });
    transcript.append(liveTurn());
    transcript.scrollTop = transcript.scrollHeight;
    syncBusy();
  }

  form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });
  stop.addEventListener('click', () => stopReply(true));
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
  });
  newChat.addEventListener('click', () => newConversation());
  reloadModels.addEventListener('click', () => window.dispatchEvent(new Event('pb:refresh')));
  model.addEventListener('change', () => {
    active.model = model.value;
    active.updatedAt = nowISO();
    persist();
    renderHistoryList();
  });
  let systemSaveTimer = null;
  system.addEventListener('input', () => {
    active.system = system.value;
    clearTimeout(systemSaveTimer);
    systemSaveTimer = setTimeout(() => { active.updatedAt = nowISO(); persist(); }, 500);
  });

  // Leaving the view lets a reply in progress keep streaming; this view just
  // stops following it (the next chat view mount re-attaches).
  function onLeave() {
    window.removeEventListener('hashchange', onLeave);
    if (inflight) inflight.view = null;
  }
  window.addEventListener('hashchange', onLeave);

  function renderModels(list) {
    const prev = model.value || active.model || '';
    const groups = GROUPS.map((g) => {
      const opts = list.filter((m) => m.kind === g.kind).map((m) => h('option', { value: m.id }, modelLabel(m)));
      return opts.length ? h('optgroup', { label: g.label }, opts) : null;
    });
    fill(model, groups);
    if (list.some((m) => m.id === prev)) model.value = prev;
    else if (list.some((m) => m.id === 'auto')) model.value = 'auto';
    model.disabled = list.length === 0;
    modelsAvailable = list.length > 0;
    syncBusy();
    hint.textContent = list.length === 0
      ? 'No models are being served yet.'
      : 'Model, node and basic stats appear under each reply.';
  }

  async function refresh() {
    const [models, nodes] = await Promise.all([get('/admin/chat/models'), get('/admin/nodes')]);
    nodesByID = Object.fromEntries(nodes.map((n) => [n.id, n]));
    renderModels(models.data || []);
  }

  renderActive();
  return { title: 'Chat', el, live: false, refresh };
}
