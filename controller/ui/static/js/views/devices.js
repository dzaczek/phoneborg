// Devices: USB devices pcprov sees over adb, including ones that are not (or
// not yet) nodes: unauthorized, provisioning, failed. Auto-provision toggle
// and per-device Provision/Retry (ADR-019). pcprov only reports when started
// with -admin-token-file; without a report ever received, this view just
// says so.
import { api, get } from '../api.js';
import { h, fill, table, badge, ago } from '../dom.js';
import { toast, errorToast } from '../ui.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));

const STATUS_CLASS = {
  new: 'info',
  'waiting-authorization': 'warn',
  provisioning: 'loading',
  provisioned: 'ok',
  failed: 'error',
  gone: 'idle',
};

function statusCell(d) {
  const label = d.status + (d.status === 'provisioning' && d.step ? ': ' + d.step : '');
  return [badge(label, STATUS_CLASS[d.status] || 'info'), d.pending ? badge('queued', 'info') : null];
}

async function provisionNow(d) {
  try {
    await api('POST', '/admin/devices/' + encodeURIComponent(d.serial) + '/provision');
    toast(`${d.serial}: provisioning requested.`);
  } catch (e) {
    if (e.status !== 401 && e.status !== 503) errorToast(e);
  }
  refreshNow();
}

async function toggleAuto(on) {
  try {
    await api('PUT', '/admin/devices', { auto_provision: on });
    toast('Auto-provision ' + (on ? 'enabled' : 'disabled') + '.');
  } catch (e) {
    if (e.status !== 401 && e.status !== 503) errorToast(e);
  }
  refreshNow();
}

const COLS = ['Serial', 'ADB state', 'Model', 'Status', 'Hint', 'Node', 'Host', 'Last seen', { label: 'Actions', num: true }];

export default function devicesView() {
  const summary = h('span.muted');
  const toggle = h('button.small', { type: 'button' });
  const body = h('div.card', null, h('p.empty', null, 'Loading…'));
  const el = h('section', null,
    h('div.page-head', null, h('h1', null, 'Devices'), summary, toggle),
    h('p.muted.small', null, 'USB devices pcprov sees over adb: not-yet-authorized phones, provisioning in progress and failures, alongside phones that already became nodes. Run with pcprov watch -controller-url ... -admin-token-file ... to show them here.'),
    body);

  async function refresh() {
    const ds = await get('/admin/devices');
    const devices = ds.devices || [];
    toggle.textContent = ds.auto_provision ? 'Turn auto-provision off' : 'Turn auto-provision on';
    toggle.onclick = () => toggleAuto(!ds.auto_provision);
    summary.textContent = `${devices.length} device(s), auto-provision ${ds.auto_provision ? 'on' : 'off'}`;
    if (devices.length === 0) {
      fill(body, h('p.empty', null, 'No devices reported yet.'));
      return;
    }
    const rows = devices.map((d) => {
      const canProvision = d.status === 'new' || d.status === 'waiting-authorization' || d.status === 'failed';
      const node = d.node_id ? (d.alias ? `${d.alias} (${d.node_id})` : d.node_id) : h('span.muted', null, '–');
      const hint = d.hint || d.error;
      return h('tr', null,
        h('td.mono', null, d.serial),
        h('td', null, d.adb_state),
        h('td', null, d.model || h('span.muted', null, '–')),
        h('td', null, statusCell(d)),
        h('td', null, hint ? h('span', { title: d.error || '' }, hint) : h('span.muted', null, '–')),
        h('td', null, node),
        h('td', null, d.host, d.stale ? [' ', badge('not reporting', 'warn')] : null),
        h('td', null, ago(d.last_seen)),
        h('td.actions', null, canProvision
          ? h('button.small', { type: 'button', disabled: d.pending, onclick: () => provisionNow(d), 'data-focus-key': d.serial + ':provision' },
            d.pending ? 'Queued…' : (d.status === 'failed' ? 'Retry' : 'Provision'))
          : null));
    });
    fill(body, table(COLS, rows, 'No devices detected.'));
  }

  return { title: 'Devices', el, live: true, refresh };
}
