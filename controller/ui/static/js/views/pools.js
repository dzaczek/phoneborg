// Pools: named groups of nodes routed together as the "pool/<name>" virtual
// model. Membership and eligibility are computed by the controller and come
// back read-only on every GET/PUT.
import { api, get, getOptional, gatewayModels } from '../api.js';
import { h, fill, table, badge, int, tps, CLASSES } from '../dom.js';
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
    h('option', { value: 'spread' }, 'spread'), h('option', { value: 'affinity' }, 'affinity'));

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
        'affinity: same as normal model routing — a session is pinned to the node that served its first request.'),
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

function poolCard(p, nodes, modelIds) {
  const edit = h('button.small', { type: 'button', onclick: () => poolDialog(p, nodes, modelIds), 'data-focus-key': p.name + ':edit' }, 'Edit');
  const warm = h('button.small', { type: 'button', onclick: () => prewarm(p), 'data-focus-key': p.name + ':warm' }, 'Prewarm');
  const del = h('button.small.danger', { type: 'button', onclick: () => deletePool(p), 'data-focus-key': p.name + ':delete' }, 'Delete');
  return h('div.card.section', null,
    h('div.row', null, h('h2', null, p.name), badge(p.routing || 'spread', 'info'), h('span.spacer'), edit, warm, del),
    p.description ? h('p.muted', null, p.description) : null,
    filtersList(p),
    h('h3', null, 'Members'),
    membersTable(p));
}

export default function poolsView() {
  const addBtn = h('button.primary', { type: 'button', hidden: true }, 'Add pool');
  const body = h('div', null, h('p.muted', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Pools'), addBtn), body);

  async function refresh() {
    const res = await getOptional('/admin/pools');
    self.live = res !== null; // stop polling a controller without the API; "Check again" retries
    if (res === null) {
      addBtn.hidden = true;
      fill(body, unavailable('Pools', '/admin/pools', refreshNow));
      return;
    }
    addBtn.hidden = false;
    const [nodes, modelIds] = await Promise.all([get('/admin/nodes'), fetchModelIds()]);
    addBtn.onclick = () => poolDialog(null, nodes, modelIds);

    const pools = res.pools || [];
    fill(body, pools.length
      ? pools.map((p) => poolCard(p, nodes, modelIds))
      : h('div.card', null, h('p.empty', null, 'No pools yet. Add one to group nodes under pool/<name>.')));
  }

  const self = { title: 'Pools', el, live: true, refresh };
  return self;
}
