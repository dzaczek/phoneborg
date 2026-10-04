// MMB, the multi-model benchmark (ADR-028): load every model that fits on
// the chosen phones and time the same requests on each. The start form is
// built once (selections survive refreshes); runs and results refresh live.
import { api, get, getOptional } from '../api.js';
import { h, fill, table, badge, bytes, ago } from '../dom.js';
import { toast, errorToast, confirmDialog, field, unavailable } from '../ui.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));
const quiet = (e) => { if (e.status !== 401 && e.status !== 503) errorToast(e); };
const STATUS_CLASS = { queued: 'idle', loading: 'loading', running: 'serving', done: 'ok', failed: 'bad', skipped: 'warn', cancelled: 'idle' };
const sec = (ms) => (ms > 0 ? (ms / 1000).toFixed(1) + ' s' : '–');
const rate = (v) => (v > 0 ? v.toFixed(1) : '–');
const nodeName = (r) => r.alias || r.node_id;

let selected = '';

// PARAMS defines every measured parameter once: the results table, the
// phone × model table and the legend all use it.
const PARAMS = [
  { id: 'gen', label: 'Gen t/s', better: 'higher', get: (r) => r.cold && r.cold.gen_tps, fmt: rate,
    what: 'Generation speed: tokens per second while the model writes its answer (128 fixed tokens, llama.cpp).',
    read: 'How fast text appears. Matters for long answers and chat. Mostly limited by the phone\'s memory bandwidth and the model size: a model twice as big is about twice as slow.' },
  { id: 'pp', label: 'Prompt t/s', better: 'higher', get: (r) => r.cold && r.cold.prompt_tps, fmt: rate,
    what: 'Prompt processing speed: tokens per second while the model reads the input (the ~500-token prompt).',
    read: 'Decides the wait before the first word for long inputs: agents such as opencode send 10k+ tokens, so 10 t/s means ~17 minutes, 100 t/s under 2. Usually 3–10× the generation speed.' },
  { id: 'cold', label: 'Cold TTFT', better: 'lower', get: (r) => r.cold && r.cold.ttft_ms, fmt: sec,
    what: 'Time to first token of a ~500-token prompt with an empty cache.',
    read: 'Roughly prompt length ÷ prompt t/s. The wait you feel when a new conversation or a new long document starts.' },
  { id: 'warm', label: 'Warm TTFT', better: 'lower', get: (r) => r.warm && r.warm.ttft_ms, fmt: sec,
    what: 'Time to first token of a second request with the same long prefix: the answer after the first prompt.',
    read: 'The prompt cache at work: only the new part is processed. Low values mean follow-up questions in a conversation start quickly. Session affinity keeps conversations on the phone that has their cache.' },
  { id: 'cache', label: 'Cache ×', better: 'higher', get: (r) => (r.cold && r.warm && r.warm.ttft_ms > 0 ? r.cold.ttft_ms / r.warm.ttft_ms : 0), fmt: (v) => (v > 0 ? v.toFixed(1) + '×' : '–'),
    what: 'Cold TTFT ÷ warm TTFT.',
    read: 'How much a reused prompt speeds up the start. High (10× and more) is normal for a long prefix; close to 1× means the cache did not help.' },
  { id: 'short', label: 'Short', better: 'lower', get: (r) => r.short && r.short.total_ms, fmt: sec,
    what: 'Total time of a one-line question with a one-word answer.',
    read: 'Interactive latency for small requests (quick questions, classifications, tool-less subagents).' },
  { id: 'load', label: 'Load', better: 'lower', get: (r) => r.load_seconds * 1000, fmt: sec,
    what: 'From switching the model until the phone serves it: download to the phone (first time only) plus loading.',
    read: 'The cost of changing models on a phone. A first load includes the download; later loads from the phone\'s storage are much shorter.' },
];
const param = (id) => PARAMS.find((p) => p.id === id);

let pivotParam = 'gen';
let pivotAll = false;
let legendOpen = false; // survives the live refresh

// best returns the best value of a parameter among finished results.
function best(results, p) {
  const vals = results.filter((r) => r.status === 'done').map(p.get).filter((v) => v > 0);
  if (!vals.length) return null;
  return p.better === 'lower' ? Math.min(...vals) : Math.max(...vals);
}

function cell(p, r, bestV) {
  const v = p.get(r);
  const c = { v: p.fmt(v), num: true };
  if (bestV != null && v === bestV) c.v = h('strong', { title: 'best in this run' }, p.fmt(v), ' ★');
  return c;
}

const header = (p) => ({ label: p.label, num: true, title: `${p.what} ${p.better === 'lower' ? 'Lower' : 'Higher'} is better.` });

function resultsTable(run) {
  const rs = run.results || [];
  const order = ['load', 'cold', 'pp', 'gen', 'warm', 'cache', 'short'].map(param);
  const bests = order.map((p) => best(rs, p));
  const rows = rs.map((r) => [
    h('span', null, nodeName(r), h('span.cell-sub', null, r.device)),
    h('span', null, h('span.mono', null, r.model_id), h('span.cell-sub', null, [r.params, r.size_bytes ? bytes(r.size_bytes) : ''].filter(Boolean).join(' · '))),
    badge(r.status, STATUS_CLASS[r.status] || 'info'),
    ...order.map((p, i) => cell(p, r, bests[i])),
    r.cold ? { v: `${r.cold.prompt_tokens || '–'} / ${r.cold.completion_tokens || '–'}`, num: true } : '–',
    r.ctx_size ? `${r.ctx_size / 1024}k · ${r.kv_type || '?'}` : '–',
    r.error ? h('span.muted', null, r.error) : (r.short && r.short.text ? h('span.muted', null, r.short.text) : '–'),
  ]);
  return table(['Phone', 'Model', 'Status', ...order.map(header),
    { label: 'Tokens in/out', num: true, title: 'Prompt and generated tokens of the cold request' },
    { label: 'Context', title: 'Context size and KV-cache type the phone chose for the model' },
    { label: 'Note / answer', title: 'Why a model failed or was skipped, or the answer to the short question' }], rows, 'No results.');
}

// pivot shows one parameter for every phone (rows) and model (columns),
// from the selected run or the latest result of each pair over all runs.
function pivot(runs, run, rerender) {
  const p = param(pivotParam);
  let rs = [];
  if (pivotAll) {
    const latest = new Map();
    for (const r of runs) {
      for (const x of r.results || []) {
        const key = x.node_id + '|' + x.model_id;
        const prev = latest.get(key);
        if (x.status === 'done' && (!prev || x.finished > prev.finished)) latest.set(key, x);
      }
    }
    rs = [...latest.values()];
  } else if (run) {
    rs = (run.results || []).filter((x) => x.status === 'done');
  }
  const phones = [...new Map(rs.map((r) => [r.node_id, r])).values()];
  const size = (m) => (rs.find((r) => r.model_id === m) || {}).size_bytes || 0;
  const modelIds = [...new Set(rs.map((r) => r.model_id))].sort((a, b) => size(a) - size(b));
  const colBest = Object.fromEntries(modelIds.map((m) => [m, best(rs.filter((r) => r.model_id === m), p)]));
  const rows = phones.map((ph) => [
    h('span', null, nodeName(ph), h('span.cell-sub', null, ph.device)),
    ...modelIds.map((m) => {
      const r = rs.find((x) => x.node_id === ph.node_id && x.model_id === m);
      if (!r) return { v: h('span.muted', null, '–'), num: true };
      const v = p.get(r);
      const tip = PARAMS.map((q) => `${q.label}: ${q.fmt(q.get(r))}`).join('\n');
      const text = p.fmt(v);
      return { v: v === colBest[m] && phones.length > 1 ? h('strong', { title: tip }, text, ' ★') : h('span', { title: tip }, text), num: true };
    }),
  ]);
  const buttons = h('div.row', null, PARAMS.map((q) => h(q.id === pivotParam ? 'button.small.primary' : 'button.small', {
    type: 'button', 'aria-pressed': String(q.id === pivotParam), title: q.what, 'data-focus-key': 'pivot:' + q.id,
    onclick: () => { pivotParam = q.id; rerender(); } }, q.label)),
  h('label.muted.small', null, h('input', { type: 'checkbox', checked: pivotAll, 'data-focus-key': 'pivot:all',
    onchange: (e) => { pivotAll = e.target.checked; rerender(); } }), ' combine all runs (latest result per phone and model)'));
  return h('div.card.section', null, h('h2', null, 'Phones × models'),
    h('p.muted.small', null, `${p.what} ${p.better === 'lower' ? 'Lower' : 'Higher'} is better; ★ marks the best phone for each model. Hover a cell for all its values.`),
    buttons,
    rows.length ? table(['Phone', ...modelIds.map((m) => ({ label: m, num: true }))], rows, '') : h('p.empty', null, 'No finished results yet.'));
}

// legend explains every parameter and how to read it.
function legend() {
  return h('details.card.section', { open: legendOpen, ontoggle: (e) => { legendOpen = e.target.open; } },
    h('summary', null, h('strong', null, 'What the parameters mean')),
    h('dl.kv', null, PARAMS.map((p) => [
      h('dt', null, p.label, h('span.cell-sub', null, p.better === 'lower' ? 'lower is better' : 'higher is better')),
      h('dd', null, h('div', null, p.what), h('div.muted', null, p.read))])),
    h('p.muted.small', null, 'TTFT = time to first token. t/s = tokens per second; a token is about ¾ of an English word, less for Polish. ' +
      'Speeds come from llama.cpp\'s own timings, times are measured by the controller. All requests use the same prompts with thinking off, ' +
      'so models and phones are comparable; a hot phone throttles and measures slower.'));
}

async function cancelRun(run) {
  try {
    await api('POST', `/admin/mmb/${encodeURIComponent(run.id)}/cancel`);
    toast('Cancelling; the phones get their models back.');
  } catch (e) { quiet(e); }
  refreshNow();
}

async function deleteRun(run) {
  if (!(await confirmDialog({ title: 'Delete this benchmark run?', body: 'Its results are deleted.', confirm: 'Delete', danger: true }))) return;
  try {
    await api('DELETE', `/admin/mmb/${encodeURIComponent(run.id)}`);
    if (selected === run.id) selected = '';
  } catch (e) { quiet(e); }
  refreshNow();
}

export default function mmbView() {
  // Start form, filled with phones, pools and models on the first refresh.
  const phonesBox = h('div.checks');
  const poolSel = h('select', { name: 'pool' }, h('option', { value: '' }, '(choose phones above instead)'));
  const modelsBox = h('div.checks');
  const parallel = h('input', { type: 'checkbox', name: 'parallel' });
  const start = h('button.primary', { type: 'submit' }, 'Start benchmark');
  const form = h('form.card.section', null, h('h2', null, 'New benchmark'),
    h('fieldset', null, h('legend', null, 'Phones'), phonesBox),
    field('Or a pool', poolSel, 'All phones of the pool.'),
    h('fieldset.pin-list', null, h('legend', null, 'Models (none = every model that fits each phone)'), modelsBox),
    h('label', null, parallel, ' Run the phones at the same time (otherwise one by one)'),
    h('p.muted.small', null, 'Each phone under test is taken out of normal traffic, loads every model in turn (download + load are timed) ' +
      'and answers the same three requests; afterwards it gets its placement and traffic back. A model download can take minutes.'),
    start);
  let formFilled = false;
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const checked = (name) => [...form.querySelectorAll(`input[name="${name}"]:checked`)].map((i) => i.value);
    const body = { nodes: checked('mmb-node'), pool: poolSel.value, models: checked('mmb-model'), parallel: parallel.checked };
    start.disabled = true;
    try {
      const run = await api('POST', '/admin/mmb', body);
      selected = run.id;
      toast(`Benchmark started: ${run.results.length} model/phone pairs.`);
    } catch (ex) { quiet(ex); } finally { start.disabled = false; }
    refreshNow();
  });

  const listEl = h('div');
  const detailEl = h('div');
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'MMB · multi-model benchmark')), form, listEl, detailEl);

  async function fillForm() {
    const [nodes, pools, cat] = await Promise.all([get('/admin/nodes'), getOptional('/admin/pools'), getOptional('/admin/models')]);
    fill(phonesBox, nodes.filter((n) => !n.id.startsWith('ext:')).map((n) => h('label', null,
      h('input', { type: 'checkbox', name: 'mmb-node', value: n.id }),
      ` ${n.alias || n.id}`, h('span.muted', null, ` ${(n.inventory && n.inventory.model) || ''} · ${n.class || ''}`))));
    fill(poolSel, h('option', { value: '' }, '(choose phones above instead)'),
      ((pools && pools.pools) || []).map((p) => h('option', { value: p.name }, 'pool/' + p.name)));
    fill(modelsBox, ((cat && cat.models) || []).filter((m) => m.status === 'ready').map((m) => h('label', null,
      h('input', { type: 'checkbox', name: 'mmb-model', value: m.id }), ' ', h('span.mono', null, m.id),
      h('span.muted', null, ` ${m.params || ''} · ${bytes(m.size_bytes)}`))));
    formFilled = true;
  }

  async function refresh() {
    const res = await getOptional('/admin/mmb');
    self.live = res !== null;
    if (res === null) {
      fill(listEl, unavailable('MMB', '/admin/mmb', refreshNow));
      fill(detailEl);
      return;
    }
    if (!formFilled) await fillForm();
    const runs = res.runs || [];
    if (!runs.some((r) => r.id === selected)) selected = runs.length ? runs[0].id : '';
    fill(listEl, h('div.card.section', null, h('h2', null, 'Runs'), table(['Run', 'Status', 'Mode', 'Phones', 'Done', 'Started', { label: 'Actions', num: true }], runs.map((r) => {
      const done = (r.results || []).filter((x) => x.status === 'done').length;
      return [
        h('button.small', { type: 'button', 'aria-pressed': String(r.id === selected), 'data-focus-key': 'run:' + r.id, onclick: () => { selected = r.id; refreshNow(); } }, r.id.slice(0, 8)),
        badge(r.status, STATUS_CLASS[r.status] || 'info'), r.parallel ? 'at the same time' : 'one by one', r.nodes.join(', '),
        `${done}/${(r.results || []).length}`, ago(r.created),
        h('td.actions', null,
          r.status === 'running' ? h('button.small', { type: 'button', onclick: () => cancelRun(r) }, 'Cancel') : null,
          r.status !== 'running' ? h('button.small.danger', { type: 'button', onclick: () => deleteRun(r) }, 'Delete') : null),
      ];
    }), 'No benchmark yet. Choose phones and start one.')));
    const run = runs.find((r) => r.id === selected);
    const render = () => fill(detailEl, pivot(runs, run, render),
      run ? h('div.card.section', null, h('h2', null, `Results · ${run.id.slice(0, 8)}`), run.error ? h('p', null, badge('error', 'bad'), ' ', run.error) : null,
        resultsTable(run)) : null,
      legend());
    render();
  }

  const self = { title: 'MMB', el, live: true, refresh };
  return self;
}
