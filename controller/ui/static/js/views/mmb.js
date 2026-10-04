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

// best marks the best value of a column among finished results.
function best(results, get, lower) {
  const vals = results.filter((r) => r.status === 'done').map(get).filter((v) => v > 0);
  if (!vals.length) return null;
  return lower ? Math.min(...vals) : Math.max(...vals);
}

function cell(text, v, bestV) {
  const c = { v: text, num: true };
  if (bestV != null && v === bestV) c.v = h('strong', { title: 'best in this run' }, text, ' ★');
  return c;
}

function resultsTable(run) {
  const rs = run.results || [];
  const b = {
    load: best(rs, (r) => r.load_seconds, true),
    cold: best(rs, (r) => r.cold && r.cold.ttft_ms, true),
    pp: best(rs, (r) => r.cold && r.cold.prompt_tps, false),
    tg: best(rs, (r) => r.cold && r.cold.gen_tps, false),
    warm: best(rs, (r) => r.warm && r.warm.ttft_ms, true),
    short: best(rs, (r) => r.short && r.short.total_ms, true),
  };
  const rows = rs.map((r) => {
    const c = r.cold || {}, w = r.warm || {}, s = r.short || {};
    const speedup = c.ttft_ms > 0 && w.ttft_ms > 0 ? (c.ttft_ms / w.ttft_ms).toFixed(1) + '×' : '–';
    return [
      h('span', null, nodeName(r), h('span.cell-sub', null, r.device)),
      h('span', null, h('span.mono', null, r.model_id), h('span.cell-sub', null, [r.params, r.size_bytes ? bytes(r.size_bytes) : ''].filter(Boolean).join(' · '))),
      badge(r.status, STATUS_CLASS[r.status] || 'info'),
      cell(r.load_seconds > 0 ? r.load_seconds.toFixed(0) + ' s' : '–', r.load_seconds, b.load),
      cell(sec(c.ttft_ms), c.ttft_ms, b.cold),
      cell(rate(c.prompt_tps), c.prompt_tps, b.pp),
      cell(rate(c.gen_tps), c.gen_tps, b.tg),
      cell(sec(w.ttft_ms), w.ttft_ms, b.warm),
      { v: speedup, num: true },
      cell(sec(s.total_ms), s.total_ms, b.short),
      r.ctx_size ? `${r.ctx_size / 1024}k · ${r.kv_type || '?'}` : '–',
      r.error ? h('span.muted', null, r.error) : (s.text ? h('span.muted', null, s.text) : '–'),
    ];
  });
  return table([
    'Phone', 'Model', 'Status',
    { label: 'Load', num: true, title: 'From switching the model until the phone serves it (download + load)' },
    { label: 'Cold TTFT', num: true, title: 'Time to first token of a ~500-token prompt with an empty cache' },
    { label: 'Prompt t/s', num: true, title: 'Prompt processing speed of the cold request (llama.cpp)' },
    { label: 'Gen t/s', num: true, title: 'Generation speed, 128 tokens (llama.cpp)' },
    { label: 'Warm TTFT', num: true, title: 'Time to first token of a second request with the same long prefix: the answer after the first prompt' },
    { label: 'Cache ×', num: true, title: 'Cold TTFT ÷ warm TTFT' },
    { label: 'Short', num: true, title: 'Total time of a one-line question' },
    'Context', 'Note / answer'], rows, 'No results.');
}

// matrix compares phones: generation tok/s per model and phone.
function matrix(run) {
  const rs = (run.results || []).filter((r) => r.status === 'done');
  const phones = [...new Set(rs.map(nodeName))];
  if (phones.length < 2) return null;
  const modelIds = [...new Set(rs.map((r) => r.model_id))];
  const rows = modelIds.map((m) => [h('span.mono', null, m), ...phones.map((p) => {
    const r = rs.find((x) => x.model_id === m && nodeName(x) === p);
    return { v: r && r.cold ? `${rate(r.cold.gen_tps)} · ${sec(r.warm && r.warm.ttft_ms)}` : '–', num: true };
  })]);
  return h('div.card.section', null, h('h3', null, 'Phones side by side'),
    h('p.muted.small', null, 'Generation tok/s · warm time to first token, per model and phone.'),
    table(['Model', ...phones.map((p) => ({ label: p, num: true }))], rows, ''));
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
    fill(detailEl, run ? [h('div.card.section', null, h('h2', null, `Results · ${run.id.slice(0, 8)}`), run.error ? h('p', null, badge('error', 'bad'), ' ', run.error) : null,
      resultsTable(run)), matrix(run)] : null);
  }

  const self = { title: 'MMB', el, live: true, refresh };
  return self;
}
