// Pools: named groups of nodes routed together as the "pool/<name>" virtual
// model. Membership and eligibility are computed by the controller and come
// back read-only on every GET/PUT.
import { api, get, getOptional, gatewayModels } from '../api.js';
import { h, fill, table, badge, int, tps, bytes, duration, classOf, CLASSES } from '../dom.js';
import { toast, errorToast, confirmDialog, formDialog, modal, field, unavailable } from '../ui.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));
const NAME_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;
const chips = (list) => (list && list.length ? list.map((x) => h('span.chip', null, x)) : h('span.muted', null, 'any'));
const checkedValues = (form, name) => [...form.querySelectorAll(`input[name="${CSS.escape(name)}"]:checked`)].map((i) => i.value);

function validateName(name) {
  if (!name) return 'Enter a name.';
  if (!NAME_RE.test(name)) return 'Use lowercase letters, digits and hyphens, starting with a letter or digit, up to 32 characters.';
  return '';
}

// fetchModelIds prefers the model catalog; on a controller without model
// management it falls back to the served model ids /v1/models already lists.
async function fetchModelIds() {
  const cat = await getOptional('/admin/models');
  if (cat) return (cat.models || []).map((m) => m.id);
  const gw = await gatewayModels();
  if (gw) return (gw.data || []).filter((m) => !m.kind || m.kind === 'model').map((m) => m.id);
  return [];
}

function filtersList(p) {
  return h('dl.kv', null,
    h('dt', null, 'Models'), h('dd', null, chips(p.models)),
    h('dt', null, 'Nodes'), h('dd', null, chips(p.nodes)),
    h('dt', null, 'Classes'), h('dd', null, chips(p.classes)),
    h('dt', null, 'Min tok/s'), h('dd', null, p.min_gen_tps > 0 ? tps(p.min_gen_tps) : 'no limit'));
}

function membersTable(p) {
  const rows = (p.members || []).map((m) => [
    h('span', null, m.alias || h('span.mono', null, m.node_id), m.alias ? h('span.cell-sub.mono', null, m.node_id) : null),
    m.model || h('span.muted', null, '–'),
    m.eligible ? badge('yes', 'ok') : badge('no', 'bad'),
    m.reason || '–']);
  return table(['Node', 'Model', { label: 'Eligible', title: 'Hot, drained, not-ready or downloading/loading nodes are never eligible.' }, 'Reason'],
    rows, 'No matching nodes.');
}

// ---- Big Brain (ADR-034): one model split across the RAM of several phones ----

const BB_RESERVE = 900 * 1024 ** 2; // left free on each phone, as the controller reserves
const BB_MARGIN = 1.05; // KV cache and buffers on top of the weights
const GB = (b) => (b / 1e9).toFixed(1) + ' GB';

// rpcFree estimates a phone's free RAM once it lends it to a Big Brain: what
// is free now plus the model it serves, which stops.
function rpcFree(n) {
  const hb = n.last_heartbeat || {};
  const rt = hb.runtime || {};
  return (hb.ram_avail_bytes || 0) + (rt.state === 'rpc' ? 0 : rt.model_bytes || 0);
}

function bigBrainDialog(existing, nodes, catalog) {
  const isEdit = !!existing;
  const name = h('input', { name: 'name', required: true, value: existing ? existing.name : '', disabled: isEdit, spellcheck: 'false', maxlength: 32, placeholder: 'brain' });
  const description = h('textarea', { name: 'description', rows: 2, value: existing ? existing.description || '' : '' });
  const ready = catalog.filter((m) => m.status === 'ready').sort((a, b) => b.size_bytes - a.size_bytes);
  const model = h('select', { name: 'model', required: true },
    ready.map((m) => h('option', { value: m.id, selected: !!existing && existing.model === m.id }, `${m.id} (${GB(m.size_bytes)})`)));
  const ctx = h('input', { type: 'number', name: 'ctx_size', min: 512, max: 131072, step: 512, value: String(existing && existing.ctx_size ? existing.ctx_size : 4096) });

  // Phone picker with a class and free-RAM filter: with 100 phones, pick the
  // ten largest without scrolling through the rest.
  const sel = new Set(existing ? existing.nodes || [] : []);
  const cls = h('select', { name: 'bb-class' }, h('option', { value: '' }, 'any class'), CLASSES.map((c) => h('option', { value: c.id }, `${c.id} (${c.label})`)));
  const minFree = h('input', { type: 'number', name: 'bb-min-free', min: 0, step: 0.5, value: '0', style: 'width:6em' });
  const list = h('div.checks');
  const meter = h('meter', { min: 0, max: 1, value: 0, style: 'width:100%' });
  const fit = h('p');
  const byName = new Map(nodes.map((n) => [n.alias || n.id, n]));
  const sorted = [...nodes].sort((a, b) => rpcFree(b) - rpcFree(a));

  function update() {
    const m = ready.find((x) => x.id === model.value);
    const need = m ? m.size_bytes * BB_MARGIN : 0;
    let have = 0;
    for (const v of sel) {
      const n = byName.get(v);
      if (n) have += Math.max(rpcFree(n) - BB_RESERVE, 0);
    }
    meter.max = Math.max(need, 1);
    meter.value = Math.min(have, need);
    meter.optimum = need;
    meter.low = need * 0.999;
    fill(fit, need ? [badge(have >= need ? 'fits' : 'too small', have >= need ? 'ok' : 'bad'),
      ` needs ${GB(need)}, ${sel.size} phone(s) give ≈ ${GB(have)} (free RAM minus ${GB(BB_RESERVE)} each).`,
      sel.size === 1 ? ' Pick at least 2.' : ''] : 'Pick a model.');
  }

  function render() {
    const c = cls.value;
    const min = (Number(minFree.value) || 0) * 1e9;
    const shown = sorted.filter((n) => sel.has(n.alias || n.id) ||
      ((!c || classOf((n.inventory || {}).ram_total_bytes) === c) && rpcFree(n) >= min));
    fill(list, shown.length ? shown.map((n) => {
      const v = n.alias || n.id;
      const rt = (n.last_heartbeat || {}).runtime || {};
      return h('label', null,
        h('input', { type: 'checkbox', name: 'bb-nodes', value: v, checked: sel.has(v),
          onchange: (e) => { if (e.target.checked) sel.add(v); else sel.delete(v); update(); } }),
        n.alias || h('span.mono', null, n.id),
        h('span.muted', null, ` ${classOf((n.inventory || {}).ram_total_bytes)} · ≈${GB(rpcFree(n))} free${rt.state === 'rpc' ? ' · in rpc' : ''}`));
    }) : h('p.muted', null, 'No phone matches the filter.'));
    update();
  }
  cls.onchange = render;
  minFree.oninput = render;
  model.onchange = update;
  render();

  formDialog({
    title: isEdit ? `Edit Big Brain ${existing.name}` : 'Add Big Brain',
    submit: isEdit ? 'Save' : 'Create',
    fields: [
      h('p.muted', null, 'One model too large for any single phone, split across the RAM of several. The phones stop serving their own model ' +
        'and run ggml-rpc-server; the controller runs the head. More phones fit a bigger model but do not make it faster.'),
      field('Name', name, isEdit ? 'Names cannot be changed here.' : 'Clients use pool/<name> as the model.'),
      field('Description', description),
      ready.length ? field('Model', model, 'From the model catalog; add a GGUF there first.') : h('p.form-error', null, 'No ready model in the catalog.'),
      field('Context (tokens)', ctx, 'The head’s context window; the KV cache lives on the phones.'),
      h('fieldset.pin-list', null, h('legend', null, 'Phones'),
        h('div.row', null, cls, h('label', null, 'min free GB ', minFree)), list),
      meter, fit,
    ],
    onSubmit: async () => {
      const nm = name.value.trim();
      const err = validateName(nm);
      if (err) throw new Error(err);
      if (sel.size < 2) throw new Error('Pick at least 2 phones.');
      await api('PUT', '/admin/pools/' + encodeURIComponent(nm), {
        name: nm, description: description.value.trim(), routing: 'bigbrain', model: model.value,
        ctx_size: Number(ctx.value) || 0, nodes: [...sel], disabled: !!(existing && existing.disabled),
      });
      toast(`${nm}: saved. The phones switch to RPC mode and the head loads the model; this takes minutes.`);
      refreshNow();
      return true;
    },
  });
}

const BB_STATE = { ready: 'ok', loading: 'warn', preparing: 'info', error: 'bad' };

function bigBrainPanel(p) {
  const bb = p.big_brain || {};
  const loading = bb.state === 'loading' && bb.load_started_at
    ? duration((Date.now() - Date.parse(bb.load_started_at)) / 1000) : null;
  return h('div', null,
    h('dl.kv', null,
      h('dt', null, 'Model'), h('dd', null, h('span.mono', null, p.model), ` · context ${int(p.ctx_size || 4096)}`),
      h('dt', null, 'State'), h('dd', null, badge(bb.state || '?', BB_STATE[bb.state] || 'info'),
        loading ? ` for ${loading}` : null, bb.reason ? h('span.muted', null, ' ' + bb.reason) : null),
      h('dt', null, 'RAM'), h('dd', null, `needs ${GB(bb.need_bytes || 0)}, phones give ≈ ${GB(bb.have_bytes || 0)}`),
      bb.load_seconds ? [h('dt', null, 'Loaded in'), h('dd', null, duration(bb.load_seconds))] : null,
      bb.gen_tps ? [h('dt', null, 'Speed'), h('dd', null, `${tps(bb.gen_tps)} tok/s generation, ${tps(bb.prompt_tps)} tok/s prompt (self-test)`)] : null,
      bb.port ? [h('dt', null, 'Head'), h('dd', null, h('span.mono', null, `127.0.0.1:${bb.port}`), bb.restarts ? ` · ${bb.restarts} restart(s)` : null)] : null),
    h('h3', null, 'Phones'),
    table(['Phone', { label: 'Share', num: true }, 'In RPC mode', 'Reason'], (p.members || []).map((m, i) => [
      h('span', null, m.alias || h('span.mono', null, m.node_id), m.alias ? h('span.cell-sub.mono', null, m.node_id) : null),
      { v: bb.tensor_split ? bytes(bb.tensor_split[i] * 1024 ** 2) : '–', num: true },
      m.eligible ? badge('yes', 'ok') : badge('no', 'bad'),
      m.reason || '–']), 'No phones.'));
}

function poolDialog(existing, nodes, modelIds) {
  const isEdit = !!existing;
  const name = h('input', { name: 'name', required: true, value: existing ? existing.name : '', disabled: isEdit, spellcheck: 'false', maxlength: 32, placeholder: 'fast' });
  const description = h('textarea', { name: 'description', rows: 2, value: existing ? existing.description || '' : '' });

  const selModels = new Set(existing ? existing.models || [] : []);
  const modelsField = modelIds.length
    ? h('fieldset.pin-list', null, h('legend', null, 'Models (none = any)'),
        h('div.checks', null, modelIds.map((id) => h('label', null,
          h('input', { type: 'checkbox', name: 'pool-models', value: id, checked: selModels.has(id) }), h('span.mono', null, id)))))
    : h('p.muted', null, 'No models in the catalog yet; this pool will allow any model.');

  const known = nodes.map((n) => n.alias || n.id);
  const selNodes = new Set(existing ? existing.nodes || [] : []);
  const nodeValues = [...known, ...[...selNodes].filter((v) => !known.includes(v))];
  const nodesField = h('fieldset.pin-list', null, h('legend', null, 'Nodes (none = any)'),
    nodeValues.length ? h('div.checks', null, nodeValues.map((v) => {
      const n = nodes.find((x) => (x.alias || x.id) === v);
      return h('label', null, h('input', { type: 'checkbox', name: 'pool-nodes', value: v, checked: selNodes.has(v) }),
        n ? (n.alias || h('span.mono', null, n.id)) : v,
        n && n.alias ? h('span.muted', null, ` ${n.id}`) : (!n ? h('span.muted', null, ' (unknown)') : null));
    })) : h('p.muted', null, 'No nodes yet.'));

  const selClasses = new Set(existing ? existing.classes || [] : []);
  const classesField = h('fieldset', null, h('legend', null, 'Device classes (none = any)'),
    h('div.checks', null, CLASSES.map((c) => h('label', null,
      h('input', { type: 'checkbox', name: 'pool-classes', value: c.id, checked: selClasses.has(c.id) }), `${c.id} (${c.label})`))));

  const minTps = h('input', { type: 'number', name: 'min_gen_tps', min: 0, step: 'any', value: String(existing ? existing.min_gen_tps || 0 : 0) });
  const routing = h('select', { name: 'routing', value: existing ? existing.routing || 'spread' : 'spread' },
    h('option', { value: 'spread' }, 'spread'), h('option', { value: 'affinity' }, 'affinity'),
    h('option', { value: 'superborg' }, 'superborg'));
  // Super Borg pools (ADR-022): an orchestrator member delegates to the others.
  const orchNames = [...known, ...(existing && existing.orchestrator && !known.includes(existing.orchestrator) ? [existing.orchestrator] : [])];
  const orchestrator = h('select', { name: 'orchestrator' },
    h('option', { value: '' }, 'auto (member serving the largest model)'),
    orchNames.map((v) => h('option', { value: v, selected: !!existing && existing.orchestrator === v }, v)));
  const thinking = h('input', { type: 'checkbox', name: 'thinking', checked: !!(existing && existing.thinking) });

  formDialog({
    title: isEdit ? `Edit pool ${existing.name}` : 'Add pool',
    fields: [
      field('Name', name, isEdit ? 'Names cannot be changed here; delete and recreate to rename.'
        : 'Lowercase letters, digits and hyphens, starting with a letter or digit, up to 32 characters.'),
      field('Description', description),
      modelsField,
      nodesField,
      classesField,
      field('Min gen tok/s', minTps, "Excludes nodes whose measured generation speed (self-test) is below this. 0 = no limit."),
      field('Routing', routing, 'spread: sends each request to the least busy eligible node, ties broken by fastest measured, no session pinning. ' +
        'affinity: same as normal model routing — a session is pinned to the node that served its first request. ' +
        'superborg: the pool answers as one model; an orchestrator member plans and delegates subtasks to the other members, and jobs run on it.'),
      field('Super Borg orchestrator', orchestrator, 'Only for superborg routing. Pick a member serving a model that can call tools (e.g. Qwen3).'),
      h('label', null, thinking, ' Super Borg thinking: the orchestrator reasons before it plans (slower; superborg routing only)'),
    ],
    onSubmit: async (form) => {
      const nm = name.value.trim();
      const err = validateName(nm);
      if (err) throw new Error(err);
      const body = {
        name: nm,
        description: description.value.trim(),
        models: checkedValues(form, 'pool-models'),
        nodes: checkedValues(form, 'pool-nodes'),
        classes: checkedValues(form, 'pool-classes'),
        min_gen_tps: Number(minTps.value) || 0,
        routing: routing.value,
        orchestrator: orchestrator.value,
        thinking: thinking.checked,
        disabled: !!(existing && existing.disabled), // editing keeps the on/off state; use Enable/Disable to change it
      };
      await api('PUT', '/admin/pools/' + encodeURIComponent(nm), body);
      toast(`${nm}: saved.`);
      refreshNow();
      return true;
    },
  });
}

async function deletePool(p) {
  const ok = await confirmDialog({
    title: `Delete pool ${p.name}?`,
    body: [`Requests using "pool/${p.name}" as the model stop routing anywhere.`],
    confirm: 'Delete', danger: true,
  });
  if (!ok) return;
  try {
    await api('DELETE', '/admin/pools/' + encodeURIComponent(p.name));
    toast(`${p.name}: deleted.`);
  } catch (e) {
    if (e.status !== 401 && e.status !== 503) errorToast(e);
  }
  refreshNow();
}

function showPrewarmResults(p, res) {
  const rows = (res.results || []).map((r) => [
    h('span', null, r.alias || h('span.mono', null, r.node_id), r.alias ? h('span.cell-sub.mono', null, r.node_id) : null),
    r.ok ? badge('ok', 'ok') : badge('failed', 'bad'),
    { v: int(r.ms), num: true },
    r.error || '–']);
  modal((close) => h('div.dlg', null,
    h('h2', null, `Prewarm results: pool/${p.name}`),
    table(['Node', 'Result', { label: 'ms', num: true }, 'Error'], rows, 'No eligible nodes.'),
    h('div.row.end', null, h('button', { type: 'button', onclick: () => close() }, 'Done'))));
}

async function prewarm(p) {
  const prompt = h('textarea', { name: 'prompt', rows: 4, placeholder: '(optional) system prompt to cache' });
  let result = null;
  await formDialog({
    title: `Prewarm pool/${p.name}`,
    submit: 'Prewarm',
    fields: [field('System prompt (optional)', prompt,
      'Sent to every eligible node, followed by a 1-token completion, so its prompt cache is ready before the first real request.')],
    onSubmit: async () => {
      const messages = [];
      if (prompt.value.trim()) messages.push({ role: 'system', content: prompt.value.trim() });
      result = await api('POST', '/admin/prewarm', { target: `pool/${p.name}`, messages });
      return true;
    },
  });
  if (result) showPrewarmResults(p, result);
}

async function setEnabled(p, on) {
  const body = { ...p, disabled: !on };
  for (const k of ['members', 'status', 'status_reason', 'active_orchestrator', 'big_brain']) delete body[k];
  try {
    await api('PUT', '/admin/pools/' + encodeURIComponent(p.name), body);
    toast(`${p.name}: ${on ? 'enabled' : 'disabled'}.`);
  } catch (e) {
    if (e.status !== 401 && e.status !== 503) errorToast(e); // 409 explains a conflict with another pool
  }
  refreshNow();
}

const STATUS_CLASS = { ready: 'ok', 'no ready node': 'bad', disabled: 'idle' };

function poolCard(p, nodes, modelIds, catalog) {
  const big = p.routing === 'bigbrain';
  const edit = h('button.small', { type: 'button', onclick: () => (big ? bigBrainDialog(p, nodes, catalog) : poolDialog(p, nodes, modelIds)), 'data-focus-key': p.name + ':edit' }, 'Edit');
  const warm = h('button.small', { type: 'button', onclick: () => prewarm(p), 'data-focus-key': p.name + ':warm' }, 'Prewarm');
  const del = h('button.small.danger', { type: 'button', onclick: () => deletePool(p), 'data-focus-key': p.name + ':delete' }, 'Delete');
  const toggle = h('button.small', { type: 'button', onclick: () => setEnabled(p, !!p.disabled), 'data-focus-key': p.name + ':toggle' },
    big ? (p.disabled ? 'Start' : 'Stop') : (p.disabled ? 'Enable' : 'Disable'));
  const status = badge(p.status === 'ready' ? `ready · ${p.status_reason}` : p.status || '?', STATUS_CLASS[p.status] || 'info');
  const borg = p.routing === 'superborg'
    ? h('p', null, 'Orchestrator: ', p.active_orchestrator
      ? h('strong', null, (nodes.find((n) => n.id === p.active_orchestrator) || {}).alias || p.active_orchestrator)
      : h('span.muted', null, 'no ready member'),
    p.orchestrator ? h('span.muted', null, ` (configured: ${p.orchestrator})`) : h('span.muted', null, ' (automatic)'),
    p.thinking ? h('span.muted', null, ' · thinking on') : null)
    : null;
  return h('div.card.section', null,
    h('div.row', null, h('h2', null, p.name), status, badge(big ? 'big brain' : p.routing || 'spread', p.routing === 'superborg' || big ? 'ok' : 'info'),
      h('span.spacer'), toggle, edit, big ? null : warm, del),
    p.status === 'no ready node' && p.status_reason && !big ? h('p', null, badge('why', 'bad'), ' ', p.status_reason) : null,
    p.description ? h('p.muted', null, p.description) : null,
    big ? bigBrainPanel(p) : [borg, filtersList(p), h('h3', null, 'Members'), membersTable(p)]);
}

export default function poolsView() {
  const addBtn = h('button.primary', { type: 'button', hidden: true }, 'Add pool');
  const addBig = h('button', { type: 'button', hidden: true }, 'Add Big Brain');
  const body = h('div', null, h('p.muted', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Pools'), addBig, addBtn), body);

  async function refresh() {
    const res = await getOptional('/admin/pools');
    self.live = res !== null; // stop polling a controller without the API; "Check again" retries
    if (res === null) {
      addBtn.hidden = addBig.hidden = true;
      fill(body, unavailable('Pools', '/admin/pools', refreshNow));
      return;
    }
    addBtn.hidden = false;
    const [nodes, modelIds, cat] = await Promise.all([get('/admin/nodes'), fetchModelIds(), getOptional('/admin/models')]);
    const catalog = cat ? cat.models || [] : [];
    addBtn.onclick = () => poolDialog(null, nodes, modelIds);
    addBig.hidden = !cat; // Big Brain needs the model catalog
    addBig.onclick = () => bigBrainDialog(null, nodes, catalog);

    const pools = res.pools || [];
    fill(body, pools.length
      ? pools.map((p) => poolCard(p, nodes, modelIds, catalog))
      : h('div.card', null, h('p.empty', null, 'No pools yet. Add one to group nodes under pool/<name>.')));
  }

  const self = { title: 'Pools', el, live: true, refresh };
  return self;
}
