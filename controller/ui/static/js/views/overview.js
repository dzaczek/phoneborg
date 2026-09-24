// Overview: cluster health, request and token rates, models, placement warnings.
import { get, getOptional } from '../api.js';
import { h, fill, table, int, duration, sum, sparkline } from '../dom.js';

const STATES = ['ACTIVE', 'BENCHMARKING', 'SUSPECT', 'OFFLINE'];

// Rates are derived from successive /admin/stats totals. Samples survive
// navigating away, so the sparklines keep their history.
const samples = [];
const MAX_SAMPLES = 60;
let placementCheckedAt = 0;
let placementMissing = false;

function addSample(stats) {
  const s = { t: Date.now(), started: stats.started_at,
    req: sum(stats.since_start.keys, 'requests'), tok: sum(stats.since_start.keys, 'completion_tokens') };
  const last = samples[samples.length - 1];
  if (last && (last.started !== s.started || s.req < last.req)) samples.length = 0; // controller restarted
  samples.push(s);
  if (samples.length > MAX_SAMPLES) samples.shift();
}

// rate over the last ~30 s, and per-interval history for the sparkline.
function rate(field) {
  const n = samples.length;
  if (n < 2) return { now: null, hist: [] };
  const a = samples[Math.max(0, n - 7)];
  const b = samples[n - 1];
  const hist = [];
  for (let i = 1; i < n; i++) {
    const dt = (samples[i].t - samples[i - 1].t) / 1000;
    hist.push(dt > 0 ? (samples[i][field] - samples[i - 1][field]) / dt : 0);
  }
  return { now: (b[field] - a[field]) / ((b.t - a.t) / 1000), hist };
}

function tile(label, value, sub, extra) {
  return h('div.card.tile', null, h('div.label', null, label), h('div.value', null, value), h('div.sub', null, sub || ' '), extra);
}

export default function overview() {
  const body = h('div', null, h('p.muted', null, 'Loading…'));
  const head = h('span.muted');
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Overview'), head), body);

  async function refresh() {
    const recheck = Date.now() - placementCheckedAt > 60000;
    const [stats, nodes, placement] = await Promise.all([
      get('/admin/stats'),
      get('/admin/nodes'),
      placementMissing && !recheck ? null : getOptional('/admin/placement'),
    ]);
    if (!placementMissing || recheck) {
      placementMissing = placement === null;
      placementCheckedAt = Date.now();
    }
    addSample(stats);
    const c = stats.cluster;
    const hot = nodes.filter((n) => n.hot).length;
    const keysTotals = stats.since_start.keys;
    const reqs = sum(keysTotals, 'requests');
    const errs = sum(keysTotals, 'errors');
    const rq = rate('req');
    const tk = rate('tok');
    const fmtRate = (r) => (r.now == null ? '–' : r.now.toFixed(r.now < 10 ? 2 : 1));

    head.textContent = `controller up ${duration(stats.uptime_seconds)}`;
    const tiles = h('div.grid', null,
      tile('Nodes', int(c.nodes), `${int(c.by_state.ACTIVE || 0)} active`),
      tile('Ready', int(c.ready_backends), 'serving, not drained'),
      tile('Drained', int(c.drained), c.drained ? 'no new requests' : ''),
      tile('Hot', int(hot), hot ? 'routed around (thermal limit)' : ''),
      tile('In flight', int(c.inflight), 'requests now'),
      tile('Requests/s', fmtRate(rq), rq.now == null ? 'measuring…' : 'last 30 s', sparkline(rq.hist)),
      tile('Tokens/s', fmtRate(tk), 'generated, last 30 s', sparkline(tk.hist)),
      tile('Errors', int(errs), reqs ? `${((100 * errs) / reqs).toFixed(1)}% of ${int(reqs)} since start` : 'since start'),
    );

    const total = c.nodes || 0;
    const bar = h('div.statebar', { role: 'img', 'aria-label': STATES.map((s) => `${s} ${c.by_state[s] || 0}`).join(', ') },
      STATES.filter((s) => c.by_state[s]).map((s) => {
        const seg = h('span', { class: 'bg-' + s, title: `${s}: ${c.by_state[s]}` });
        seg.style.flex = String(c.by_state[s]);
        return seg;
      }));
    const health = h('div.card.section', null, h('h2', null, 'Cluster health'),
      total ? bar : h('p.muted', null, 'No nodes yet. Connect a phone and run pcprov watch.'),
      h('div.legend', null, STATES.map((s) => h('span', null, h('i', { class: 'bg-' + s }), `${s} ${c.by_state[s] || 0}`)),
        h('span', null, `DRAINED ${c.drained}`), h('span', null, `HOT ${hot}`)));

    const modelRows = Object.entries(c.models || {}).sort((a, b) => b[1] - a[1]).map(([m, n]) => [m || '(unnamed)', int(n)]);
    const modelsCard = h('div.card.section', null, h('h2', null, 'Models served'),
      table(['Model', { label: 'Ready nodes', num: true }], modelRows, 'No model is being served.'));

    let warnCard;
    if (placement === null && placementMissing) {
      warnCard = h('div.card.section', null, h('h2', null, 'Placement'),
        h('p.muted', null, 'Model management is not available on this controller.'));
    } else if (placement) {
      const w = placement.warnings || [];
      warnCard = h('div.card.section', null, h('h2', null, 'Placement warnings'),
        w.length ? h('ul.warnings', null, w.map((x) => h('li', null, x))) : h('p.muted', null, 'No warnings.'),
        h('p', null, h('a', { href: '#/placement' }, 'Open placement')));
    } else {
      warnCard = h('div.card.section', null, h('h2', null, 'Placement'), h('p.muted', null, 'Checking…'));
    }

    fill(body, tiles, health, h('div.two', null, modelsCard, warnCard));
  }

  return { title: 'Overview', el, live: true, refresh };
}
