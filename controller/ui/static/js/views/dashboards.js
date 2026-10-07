// Dashboards: the Grafana dashboards inside the panel (ADR-031): the cluster
// dashboard, or the analytics of one phone chosen here.
import { get } from '../api.js';
import { h, fill } from '../dom.js';
import { grafana, dashboardURL, dashboardFrame, notSetUp } from '../grafana.js';

const RANGES = [['1h', 'now-1h'], ['6h', 'now-6h'], ['24h', 'now-24h'], ['7d', 'now-7d']];
const BOARDS = { cluster: { uid: 'phoneborg', label: 'Cluster' }, node: { uid: 'phoneborg-node', label: 'Node' } };
// Kept across view re-creation.
let range = 'now-6h';
let board = 'cluster';
let node = '';

// showNodeAnalytics opens this view on one phone's analytics.
export function showNodeAnalytics(id) {
  board = 'node';
  node = id;
  location.hash = '#/dashboards';
}

const pressed = (on) => (on ? 'button.small.primary' : 'button.small');

export default function dashboardsView() {
  const tools = h('div.row');
  const body = h('div', null, h('p.empty', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Dashboards'), tools), body);
  let g = null;
  let nodes = [];

  function render() {
    if (board === 'node' && !nodes.some((n) => n.id === node)) node = nodes.length ? nodes[0].id : '';
    const vars = board === 'node' ? { node } : {};
    const url = (kiosk) => dashboardURL(g, BOARDS[board].uid, { from: range, vars, kiosk });
    const nodeSel = h('select', { 'aria-label': 'Phone', onchange: (e) => { node = e.target.value; render(); } },
      nodes.map((n) => h('option', { value: n.id, selected: n.id === node }, n.alias ? `${n.alias} (${n.id})` : n.id)));
    fill(tools,
      Object.entries(BOARDS).map(([k, b]) => h(pressed(k === board), {
        type: 'button', 'aria-pressed': String(k === board), onclick: () => { board = k; render(); } }, b.label)),
      board === 'node' ? nodeSel : null,
      h('span.muted', null, '·'),
      RANGES.map(([label, from]) => h(pressed(from === range), {
        type: 'button', 'aria-pressed': String(from === range), onclick: () => { range = from; render(); } }, label)),
      h('a.small.dashboard-open', { href: url(false), target: '_blank', rel: 'noopener noreferrer' }, 'Open in Grafana'));
    fill(body, board === 'node' && !node ? h('p.empty', null, 'No phones yet.') : dashboardFrame(url(true), `${BOARDS[board].label} dashboard`));
  }

  async function refresh() {
    if (g) return; // the dashboards refresh themselves
    g = await grafana();
    if (!g) {
      fill(body, notSetUp());
      return;
    }
    nodes = (await get('/admin/nodes')).map((n) => ({ id: n.id, alias: n.alias }));
    render();
  }

  return { title: 'Dashboards', el, live: false, refresh };
}
