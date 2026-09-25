// Nodes: live table with drain / undrain / forget and a details drawer.
// External engine nodes (ADR-016) are listed after the phones; they can be
// drained, but have no alias (their name is the alias) and are removed with
// pbctl external rm.
import { api, get } from '../api.js';
import { h, fill, table, badge, bytes, int, tps, ago, classOf } from '../dom.js';
import { toast, errorToast, confirmDialog, formDialog, field, drawer } from '../ui.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));
const ALIAS_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;

// validateAlias mirrors the controller's rules so mistakes are caught before
// the request is sent; the controller is still the authority (its errors are
// shown as-is if this check misses something).
function validateAlias(alias, id, allNodes) {
  if (!alias) return ''; // clears it
  if (!ALIAS_RE.test(alias)) return 'Use lowercase letters, digits and hyphens, starting with a letter or digit, up to 32 characters.';
  if (alias === 'auto') return '"auto" is reserved for automatic routing.';
  if (alias.startsWith('pool')) return 'Aliases cannot start with "pool" (reserved for pool/<name> routing).';
  if (allNodes.some((n) => n.id !== id && (n.id === alias || n.alias === alias))) return 'Already used by another node.';
  return '';
}

function setAlias(n, allNodes) {
  const input = h('input', { name: 'alias', value: n.alias || '', placeholder: 'phone-01', spellcheck: 'false', maxlength: 32 });
  formDialog({
    title: `Set alias for ${n.id}`,
    fields: [field('Alias', input, 'Lowercase letters, digits and hyphens, starting with a letter or digit, up to 32 characters. Clear to remove it. Cannot start with "pool" or be "auto".')],
    onSubmit: async () => {
      const alias = input.value.trim();
      const err = validateAlias(alias, n.id, allNodes);
      if (err) throw new Error(err);
      await api('PATCH', '/admin/nodes/' + encodeURIComponent(n.id), { alias });
      toast(`${n.id}: alias ${alias ? 'set to ' + alias : 'cleared'}.`);
      refreshNow();
      return true;
    },
  });
}

function stateCell(n) {
  return [badge(n.state), n.drained ? badge('DRAINED') : null, n.hot ? badge('HOT') : null];
}

// modelCell shows the served model and, when the agent reports it, the
// runtime state (downloading with progress, loading, error).
function modelCell(rt) {
  if (!rt) return h('span.muted', null, 'none');
  const name = rt.model_id || rt.model || '(unnamed)';
  const st = rt.state || (rt.ready ? 'ready' : 'loading');
  return [h('span', null, name, ' '), badge(st), rt.engine ? h('span.cell-sub', null, rt.engine) : null,
    st === 'downloading' && rt.progress > 0
      ? h('span.cell-sub', null, h('progress', { max: 1, value: rt.progress, 'aria-label': 'download progress' }), ` ${Math.round(rt.progress * 100)}%`)
      : null,
    rt.error ? h('span.cell-sub', { title: rt.error }, rt.error.length > 60 ? rt.error.slice(0, 60) + '…' : rt.error) : null];
}

async function act(n, action) {
  const id = n.id;
  if (action === 'forget') {
    const ok = await confirmDialog({
      title: `Forget node ${id}?`,
      body: ['The node is removed from the cluster. If it is still running, it registers again on its next heartbeat.',
        n.inflight ? `${n.inflight} request(s) in flight will be retried on another phone. Drain it first to let them finish.` : ''],
      confirm: 'Forget', danger: true,
    });
    if (!ok) return;
  } else if (action === 'drain') {
    const ok = await confirmDialog({
      title: `Drain node ${id}?`,
      body: ['The node gets no new requests; running ones finish. Its pinned sessions move to other phones on their next request.'],
      confirm: 'Drain',
    });
    if (!ok) return;
  }
  try {
    const path = '/admin/nodes/' + encodeURIComponent(id) + (action === 'forget' ? '' : '/' + action);
    const res = await api(action === 'forget' ? 'DELETE' : 'POST', path);
    const moved = res && res.moved_sessions ? `, ${res.moved_sessions} session(s) moved` : '';
    toast(`${id}: ${action === 'forget' ? 'forgotten' : action + 'ed'}${moved}.`);
  } catch (e) {
    if (e.status !== 401 && e.status !== 503) errorToast(e);
  }
  refreshNow();
}

function details(n) {
  const inv = n.inventory || {};
  const hb = n.last_heartbeat || {};
  const kv = (pairs) => h('dl.kv', null, pairs.filter(([, v]) => v !== '' && v != null).map(([k, v]) => [h('dt', null, k), h('dd', null, v)]));
  const json = (v) => h('pre', null, JSON.stringify(v ?? null, null, 2));
  drawer(`Node ${n.id}`, [
    kv([
      ['State', stateCell(n)],
      ['Device', `${inv.manufacturer || ''} ${inv.model || ''}`.trim()],
      ['SoC / ABI', `${inv.soc || '?'} / ${inv.abi || '?'}`],
      ['Android', inv.android_release ? `${inv.android_release} (SDK ${inv.sdk})` : ''],
      ['Class', `${classOf(inv.ram_total_bytes) || '–'} / ${n.perf_tier || '?'}`],
      ['Gen / prompt bandwidth', n.gen_gbps ? `${n.gen_gbps.toFixed(1)} / ${(n.prompt_gbps || 0).toFixed(1)} GB/s` : ''],
      ['Remote address', n.remote_addr],
      ['Registered', ago(n.registered_at)],
      ['Last seen', ago(n.last_seen)],
      ['Uptime', hb.uptime_sec ? `${Math.round(hb.uptime_sec / 60)} min` : ''],
      ['Load (1 min)', hb.load1 != null ? hb.load1.toFixed(2) : ''],
      ['Benchmark', n.benchmark ? `${n.benchmark.cpu_gflops.toFixed(1)} GFLOPS, ${n.benchmark.mem_bandwidth_gbps.toFixed(1)} GB/s (${n.benchmark.kind})` : ''],
    ]),
    h('h3', null, 'Inventory'), json(n.inventory),
    h('h3', null, 'Runtime'), json(hb.runtime),
    h('p.muted.small', null, 'Snapshot taken when this panel was opened.'),
  ]);
}

// externalSpeed is the operator's hint, else the fastest self-test.
function externalSpeed(x) {
  if (x.speed_tps > 0) return x.speed_tps;
  return Math.max(0, ...Object.values(x.measured || {}).map((m) => m.gen_tps || 0));
}

function externalDetails(x) {
  const kv = (pairs) => h('dl.kv', null, pairs.filter(([, v]) => v !== '' && v != null).map(([k, v]) => [h('dt', null, k), h('dd', null, v)]));
  drawer(`External node ${x.name}`, [
    kv([
      ['State', [badge(x.state), x.drained ? badge('DRAINED') : null]],
      ['Node id', x.node_id],
      ['URL', x.url],
      ['API key', x.has_api_key ? 'set (never shown)' : 'none'],
      ['Allowed models', (x.models || []).length ? x.models.join(', ') : 'any'],
      ['Models', (x.discovered_models || []).join(', ') || '–'],
      ['Max concurrency', x.max_concurrency],
      ['Context', x.ctx_size ? int(x.ctx_size) : 'unknown'],
      ['Speed hint', x.speed_tps ? `${x.speed_tps} tok/s` : ''],
      ['Last check', ago(x.last_check)],
      ['Last error', x.last_error || ''],
    ]),
    h('h3', null, 'Self-tests'), h('pre', null, JSON.stringify(x.measured || {}, null, 2)),
    h('p.muted.small', null, 'Snapshot taken when this panel was opened. Managed with pbctl external.'),
  ]);
}

function externalRow(x) {
  const n = { id: x.node_id, drained: x.drained, inflight: x.inflight };
  const models = x.discovered_models || [];
  return h('tr', null,
    h('td.nowrap', null,
      h('button.link', { type: 'button', onclick: () => externalDetails(x), title: 'Show details', 'data-focus-key': x.node_id + ':details' }, x.name),
      ' ', badge('external', 'info'),
      h('span.cell-sub.mono', null, x.node_id)),
    h('td', null, h('span.mono', null, x.url)),
    h('td', null, '–'),
    h('td', null, [badge(x.state), x.drained ? badge('DRAINED') : null]),
    h('td', null, models.length ? h('span', null, models[0]) : h('span.muted', null, 'none'),
      models.length > 1 ? h('span.cell-sub', { title: models.join(', ') }, `+${models.length - 1} more`) : null,
      x.last_error ? h('span.cell-sub', { title: x.last_error }, x.last_error.length > 60 ? x.last_error.slice(0, 60) + '…' : x.last_error) : null),
    h('td.num', null, '–'),
    h('td.num', null, x.ctx_size ? int(x.ctx_size) : '–'),
    h('td.num', null, tps(externalSpeed(x))),
    h('td.num', null, '–'),
    h('td.num', null, '–'),
    h('td.num', null, '–'),
    h('td.num', { title: `max ${x.max_concurrency} concurrent` }, `${int(x.inflight)} / ${int(x.pinned_sessions)}`),
    h('td', null, ago(x.last_check)),
    h('td.actions', null, x.drained
      ? h('button.small', { type: 'button', onclick: () => act(n, 'undrain'), 'data-focus-key': x.node_id + ':drain' }, 'Undrain')
      : h('button.small', { type: 'button', onclick: () => act(n, 'drain'), 'data-focus-key': x.node_id + ':drain' }, 'Drain')));
}

const COLS = [{ label: 'Node', title: 'Select a node for details' }, 'Device', { label: 'Class', title: 'Device class by total RAM / performance tier by measured generation bandwidth (ADR-015)' }, 'State', { label: 'Model', title: 'Served model, runtime state and llama.cpp build' },
  { label: 'Threads', num: true }, { label: 'Ctx', num: true }, { label: 'Tok/s', num: true, title: 'Measured generation speed (self-test)' },
  { label: 'RAM avail / total', num: true }, { label: 'Temp', num: true }, { label: 'Battery', num: true },
  { label: 'In flight / pinned', num: true, title: 'Requests in flight / sessions pinned by affinity' }, 'Last seen', { label: 'Actions', num: true }];

export default function nodesView() {
  const summary = h('span.muted');
  const body = h('div.card', null, h('p.empty', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Nodes'), summary), body);

  async function refresh() {
    const [nodes, ext] = await Promise.all([get('/admin/nodes'), get('/admin/external')]);
    const externals = (ext && ext.external) || [];
    summary.textContent = `${nodes.length} node(s), ${nodes.filter((n) => n.state === 'ACTIVE').length} active` +
      (externals.length ? `, ${externals.length} external` : '');
    const rows = nodes.map((n) => {
      const inv = n.inventory || {};
      const hb = n.last_heartbeat || {};
      const rt = hb.runtime;
      const actions = h('td.actions', null,
        h('button.small', { type: 'button', onclick: () => setAlias(n, nodes), 'data-focus-key': n.id + ':alias' }, n.alias ? 'Edit alias' : 'Set alias'),
        n.drained
          ? h('button.small', { type: 'button', onclick: () => act(n, 'undrain'), 'data-focus-key': n.id + ':drain' }, 'Undrain')
          : h('button.small', { type: 'button', onclick: () => act(n, 'drain'), 'data-focus-key': n.id + ':drain' }, 'Drain'),
        h('button.small.danger', { type: 'button', onclick: () => act(n, 'forget'), 'data-focus-key': n.id + ':forget' }, 'Forget'));
      return h('tr', null,
        h('td.nowrap', null,
          h('button.link' + (n.alias ? '' : '.mono'), { type: 'button', onclick: () => details(n), title: 'Show details', 'data-focus-key': n.id + ':details' }, n.alias || n.id),
          n.alias ? h('span.cell-sub.mono', null, n.id) : null),
        h('td', null, `${inv.manufacturer || ''} ${inv.model || ''}`.trim() || '–', h('span.cell-sub', null, inv.soc || '')),
        h('td', null, `${classOf(inv.ram_total_bytes) || '–'}/${n.perf_tier || '?'}`),
        h('td', null, stateCell(n)),
        h('td', null, modelCell(rt)),
        h('td.num', null, rt && rt.threads ? rt.threads : '–'),
        h('td.num', null, rt && rt.ctx_size ? int(rt.ctx_size) : '–'),
        h('td.num', null, rt ? tps(rt.gen_tps) : '–'),
        h('td.num', null, `${bytes(hb.ram_avail_bytes)} / ${bytes(inv.ram_total_bytes)}`),
        h('td.num', { class: n.hot ? 'hot-text' : null }, hb.temperature_c != null ? hb.temperature_c.toFixed(1) + ' °C' : '–'),
        h('td.num', null, hb.battery_level != null ? hb.battery_level + '%' : '–'),
        h('td.num', null, `${int(n.inflight)} / ${int(n.pinned_sessions)}`),
        h('td', null, ago(n.last_seen)),
        actions);
    });
    fill(body, table(COLS, rows.concat(externals.map(externalRow)), 'No nodes yet. Connect a phone and run pcprov watch.'));
  }

  return { title: 'Nodes', el, live: true, refresh };
}
