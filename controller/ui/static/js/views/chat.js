// Chat: send a test chat completion from the browser, streamed, without a
// terminal. Proxied through POST /admin/chat/completions (ADR-018), which
// routes the request through the normal gateway path (routing, failover,
// affinity, usage, metrics) but authenticates with the admin token, so this
// works even from a remote browser that has only the admin token and no API
// key (-gateway-access local). History is kept in this page only: it is
// never sent anywhere but as part of the next turn, and is lost on refresh
// or navigating away.
import { get, chatCompletionsStream } from '../api.js';
import { h, fill, int, tps } from '../dom.js';

const GROUPS = [
  { kind: 'model', label: 'Models' },
  { kind: 'auto', label: 'Auto' },
  { kind: 'pool', label: 'Pools' },
  { kind: 'node', label: 'Nodes' },
];

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
// accumulated assistant text as it grows. It returns the final text plus the
// last usage/timings block seen (llama.cpp sends it on the final event).
async function readSSE(res, onDelta) {
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let content = '';
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
      const delta = evt.choices && evt.choices[0] && evt.choices[0].delta && evt.choices[0].delta.content;
      if (delta) { content += delta; onDelta(content); }
      if (evt.usage || evt.timings) stats = { usage: evt.usage, timings: evt.timings };
    }
  }
  return { content, stats };
}

export default function chatView() {
  const model = h('select', { name: 'model' });
  const system = h('textarea', { name: 'system', rows: 2, placeholder: '(optional) system prompt', spellcheck: 'false' });
  const transcript = h('div.chat-transcript');
  const input = h('textarea', { name: 'message', rows: 2, placeholder: 'Send a message… (Enter to send, Shift+Enter for a new line)' });
  const send = h('button.primary', { type: 'submit' }, 'Send');
  const clear = h('button', { type: 'button' }, 'Clear');
  const reloadModels = h('button.small', { type: 'button' }, 'Refresh models');
  const hint = h('p.muted.small', null, 'Model, node and basic stats appear under each reply.');

  const form = h('form.chat-row', null, input, send);
  const el = h('section', null,
    h('div.page-head', null, h('h1', null, 'Chat'), clear),
    h('div.card.section.chat-layout', null,
      h('div.row', null, h('label', { for: 'chat-model' }, 'Model'), model, reloadModels),
      system,
      transcript,
      form,
      hint));
  model.id = 'chat-model';

  let history = []; // {role, content}, excludes the system prompt
  let nodesByID = {};
  let sending = false;

  function setBusy(busy) {
    sending = busy;
    send.disabled = busy;
    clear.disabled = busy;
    input.disabled = busy;
  }

  function addTurn(role, text) {
    const bubble = h('div.chat-msg', { class: role }, text);
    transcript.append(h('div.chat-turn', { class: role }, bubble));
    transcript.scrollTop = transcript.scrollHeight;
    return bubble;
  }

  async function submit() {
    const text = input.value.trim();
    if (!text || sending || !model.value) return;
    setBusy(true);
    input.value = '';
    addTurn('user', text);
    history.push({ role: 'user', content: text });
    const bubble = addTurn('assistant', '');
    const messages = system.value.trim() ? [{ role: 'system', content: system.value.trim() }, ...history] : history.slice();
    const chosenModel = model.value;
    const started = performance.now();
    try {
      const res = await chatCompletionsStream({ model: chosenModel, messages, stream: true });
      const node = res.headers.get('X-PhoneBorg-Node') || '';
      const { content, stats } = await readSSE(res, (partial) => {
        bubble.textContent = partial;
        transcript.scrollTop = transcript.scrollHeight;
      });
      history.push({ role: 'assistant', content });
      bubble.parentElement.append(h('div.chat-meta', null, statsLine(chosenModel, node, nodesByID, stats, performance.now() - started)));
    } catch (ex) {
      bubble.parentElement.classList.add('chat-failed');
      bubble.parentElement.append(h('div.chat-meta.chat-error', null, ex.message));
    } finally {
      setBusy(false);
      input.focus();
    }
  }

  form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
  });
  clear.addEventListener('click', () => {
    history = [];
    fill(transcript);
  });
  reloadModels.addEventListener('click', () => window.dispatchEvent(new Event('pb:refresh')));

  function renderModels(list) {
    const prev = model.value;
    const groups = GROUPS.map((g) => {
      const opts = list.filter((m) => m.kind === g.kind).map((m) => h('option', { value: m.id }, modelLabel(m)));
      return opts.length ? h('optgroup', { label: g.label }, opts) : null;
    });
    fill(model, groups);
    if (list.some((m) => m.id === prev)) model.value = prev;
    else if (list.some((m) => m.id === 'auto')) model.value = 'auto';
    model.disabled = list.length === 0;
    send.disabled = list.length === 0;
    hint.textContent = list.length === 0
      ? 'No models are being served yet.'
      : 'Model, node and basic stats appear under each reply.';
  }

  async function refresh() {
    const [models, nodes] = await Promise.all([get('/admin/chat/models'), get('/admin/nodes')]);
    nodesByID = Object.fromEntries(nodes.map((n) => [n.id, n]));
    renderModels(models.data || []);
  }

  return { title: 'Chat', el, live: false, refresh };
}
