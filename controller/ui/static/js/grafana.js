// Grafana dashboards served by the controller under /grafana/ (ADR-031).
import { get, grafanaSession } from './api.js';
import { h } from './dom.js';

let info = null; // GET /admin/grafana, once a session cookie is set

// grafana returns the Grafana settings after getting a session cookie, or
// null when the controller does not serve Grafana.
export async function grafana() {
  if (info) return info;
  const i = await get('/admin/grafana');
  if (!i.enabled) return null;
  info = await grafanaSession();
  return info;
}

const theme = () => (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');

// dashboardURL builds a /grafana/ URL of dashboard uid with variables vars.
export function dashboardURL(g, uid, { from = 'now-6h', vars = {}, kiosk = true } = {}) {
  const q = new URLSearchParams({ orgId: '1', from, to: 'now', refresh: '30s', theme: theme() });
  for (const [k, v] of Object.entries(vars)) q.set('var-' + k, v);
  return `${g.prefix}d/${uid}/${uid}?${q}${kiosk ? '&kiosk' : ''}`;
}

export const dashboardFrame = (src, title) => h('iframe.dashboard', { src, title, loading: 'lazy' });

export const notSetUp = () => h('div.card.section', null,
  h('p', null, 'Grafana is not set up in the panel.'),
  h('p.muted.small', null, 'Start the controller with -grafana-url http://127.0.0.1:3000 and serve Grafana from /grafana/ ' +
    '(serve_from_sub_path = true, root_url ending in /grafana/, allow_embedding = true). See OPERATIONS.md, "Grafana in the panel".'));
