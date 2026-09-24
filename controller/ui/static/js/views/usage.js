// Usage: per API key and per node totals from GET /admin/stats.
import { get } from '../api.js';
import { h, fill, table, int, tps, pct, ago } from '../dom.js';

const COLS = (first) => [first, { label: 'Requests', num: true }, { label: 'Errors', num: true },
  { label: 'Prompt tok', num: true }, { label: 'Cached', num: true, title: 'Prompt tokens served from the prompt cache' },
  { label: 'Completion tok', num: true }, { label: 'Avg tok/s', num: true, title: 'Token-weighted generation speed' }, 'Last used'];

function rows(map) {
  return Object.entries(map || {}).sort((a, b) => b[1].requests - a[1].requests).map(([name, c]) => [
    h('span.mono', null, name),
    { v: int(c.requests), num: true },
    { v: [int(c.errors), c.errors ? h('span.cell-sub', null, pct(c.errors, c.requests)) : null], num: true },
    { v: int(c.prompt_tokens), num: true },
    { v: [int(c.cached_prompt_tokens), c.cached_prompt_tokens ? h('span.cell-sub', null, pct(c.cached_prompt_tokens, c.prompt_tokens)) : null], num: true },
    { v: int(c.completion_tokens), num: true },
    { v: tps(c.avg_gen_tokens_per_second), num: true },
    ago(c.last_used)]);
}

export default function usageView() {
  let scope = 'since_start';
  const radios = h('fieldset.checks', { 'aria-label': 'Period' });
  const note = h('p.muted.small');
  const byKey = h('div.card.section');
  const byNode = h('div.card.section');
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Usage'), radios), note, byKey, byNode);
  let last = null;

  function render() {
    const s = last;
    const hasLifetime = !!s.lifetime;
    if (!hasLifetime) scope = 'since_start';
    fill(radios, 
      [['since_start', 'Since controller start'], ['lifetime', 'Since first use (persisted)']].map(([v, label]) =>
        h('label', { title: v === 'lifetime' && !hasLifetime ? 'Needs -state-dir' : null },
          h('input', { type: 'radio', name: 'usage-scope', value: v, checked: scope === v, 'data-focus-key': 'scope:' + v, disabled: v === 'lifetime' && !hasLifetime,
            onchange: () => { scope = v; renderTables(); } }), label)));
    renderTables();
  }

  function renderTables() {
    const s = last;
    const hasLifetime = !!s.lifetime;
    const t = s[scope];
    note.textContent = (scope === 'lifetime' ? 'Totals saved in usage.json' : 'Totals in memory') +
      ` since ${new Date(t.since).toLocaleString()}.` +
      (hasLifetime ? '' : ' Start the controller with -state-dir to keep totals across restarts.') +
      ' Key counts include client requests only; a failed attempt retried on another phone is an error for that node, not a key request.';
    fill(byKey, h('h2', null, 'By API key'), table(COLS('Key'), rows(t.keys), 'No requests yet.'));
    fill(byNode, h('h2', null, 'By node'), table(COLS('Node'), rows(t.nodes), 'No requests yet.'));
  }

  async function refresh() {
    last = await get('/admin/stats');
    render();
  }

  return { title: 'Usage', el, live: true, refresh };
}
