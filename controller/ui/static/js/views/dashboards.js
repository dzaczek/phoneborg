// Dashboards: the Grafana dashboard inside the panel (ADR-031). The
// controller serves Grafana under /grafana/ on its own origin; opening this
// view trades the admin token for a session cookie scoped to /grafana/.
import { get, grafanaSession } from '../api.js';
import { h, fill } from '../dom.js';

const RANGES = [['1h', 'now-1h'], ['6h', 'now-6h'], ['24h', 'now-24h'], ['7d', 'now-7d']];
let range = 'now-6h'; // kept across view re-creation

const theme = () => (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');

function dashboardURL(info, kiosk) {
  const q = new URLSearchParams({ orgId: '1', from: range, to: 'now', refresh: '30s', theme: theme() });
  return `${info.prefix}d/${info.dashboard_uid}/phoneborg?${q}${kiosk ? '&kiosk' : ''}`;
}

export default function dashboardsView() {
  const tools = h('div.row');
  const body = h('div', null, h('p.empty', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Dashboards'), tools), body);
  let info = null;

  function render() {
    const frame = h('iframe.dashboard', { src: dashboardURL(info, true), title: 'PhoneBorg Grafana dashboard', loading: 'lazy' });
    fill(tools,
      RANGES.map(([label, from]) => h(from === range ? 'button.small.primary' : 'button.small', {
        type: 'button', 'aria-pressed': String(from === range),
        onclick: () => { range = from; render(); },
      }, label)),
      h('a.small.dashboard-open', { href: dashboardURL(info, false), target: '_blank', rel: 'noopener noreferrer' }, 'Open in Grafana'));
    fill(body, frame);
  }

  async function refresh() {
    if (info) return; // the dashboard refreshes itself
    const i = await get('/admin/grafana');
    if (!i.enabled) {
      fill(body, h('div.card.section', null,
        h('p', null, 'Grafana is not set up in the panel.'),
        h('p.muted.small', null, 'Start the controller with -grafana-url http://127.0.0.1:3000 and serve Grafana from /grafana/ ' +
          '(serve_from_sub_path = true, root_url ending in /grafana/, allow_embedding = true). See OPERATIONS.md, "Grafana in the panel".')));
      return;
    }
    info = await grafanaSession();
    render();
  }

  return { title: 'Dashboards', el, live: false, refresh };
}
