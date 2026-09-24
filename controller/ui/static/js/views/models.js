// Models: the catalog of GGUF models the controller can hand out to nodes.
import { api, getOptional } from '../api.js';
import { h, fill, table, badge, bytes, int, CLASSES, TIERS } from '../dom.js';
import { toast, errorToast, confirmDialog, formDialog, field, unavailable } from '../ui.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));
const chips = (list) => (list && list.length ? list.map((x) => h('span.chip', null, x)) : h('span.muted', null, '–'));
const splitTags = (s) => s.split(',').map((t) => t.trim()).filter(Boolean);

function classChecks(legend, selected = []) {
  const set = new Set(selected);
  return h('fieldset', null, h('legend', null, legend),
    h('div.checks', null, CLASSES.map((c) => h('label', null,
      h('input', { type: 'checkbox', name: 'classes', value: c.id, checked: set.has(c.id) }), `${c.id} (${c.label})`))));
}
const checkedClasses = (form) => [...form.querySelectorAll('input[name="classes"]:checked')].map((i) => i.value);

function tierChecks(legend, selected = []) {
  const set = new Set(selected);
  return h('fieldset', null, h('legend', null, legend),
    h('div.checks', null, TIERS.map((t) => h('label', null,
      h('input', { type: 'checkbox', name: 'tiers', value: t.id, checked: set.has(t.id) }), `${t.id} (${t.label})`))));
}
const checkedTiers = (form) => [...form.querySelectorAll('input[name="tiers"]:checked')].map((i) => i.value);

function statusCell(m) {
  return [badge(m.status || '?'),
    m.status === 'downloading'
      ? h('span.cell-sub', null, h('progress', { max: 1, value: m.progress || 0, 'aria-label': `download progress of ${m.id}` }),
        ` ${Math.round((m.progress || 0) * 100)}%`)
      : null,
    m.error ? h('span.cell-sub', { title: m.error }, m.error) : null];
}

function addModel() {
  const source = h('input', { name: 'source', required: true, placeholder: 'hf://owner/repo/file.gguf', spellcheck: 'false' });
  const id = h('input', { name: 'id', placeholder: 'derived from the file name', spellcheck: 'false' });
  const name = h('input', { name: 'name', placeholder: 'optional display name' });
  const tags = h('input', { name: 'tags', placeholder: 'chat, small' });
  formDialog({
    title: 'Add model',
    submit: 'Add and download',
    fields: [
      field('Source', source, 'https://huggingface.co/<owner>/<repo>/resolve/main/<file>.gguf, hf://<owner>/<repo>/<file>.gguf, or file:///abs/path.gguf on the controller host.'),
      field('ID (optional)', id, 'Also the model name clients send in requests.'),
      field('Name (optional)', name),
      field('Tags (optional)', tags, 'Comma separated.'),
      classChecks('Recommended device classes (optional)'),
      tierChecks('Recommended performance tiers (optional)'),
    ],
    onSubmit: async (form) => {
      const body = { source: source.value.trim() };
      if (!body.source) throw new Error('Enter a source.');
      if (id.value.trim()) body.id = id.value.trim();
      if (name.value.trim()) body.name = name.value.trim();
      if (tags.value.trim()) body.tags = splitTags(tags.value);
      const rc = checkedClasses(form);
      if (rc.length) body.recommended_classes = rc;
      const rt = checkedTiers(form);
      if (rt.length) body.recommended_tiers = rt;
      const m = await api('POST', '/admin/models', body);
      toast(`${m && m.id ? m.id : 'Model'}: download started.`);
      refreshNow();
      return true;
    },
  });
}

function editModel(m) {
  const name = h('input', { name: 'name', value: m.name || '' });
  const tags = h('input', { name: 'tags', value: (m.tags || []).join(', ') });
  const def = h('input', { type: 'checkbox', name: 'default', checked: !!m.default });
  formDialog({
    title: `Edit ${m.id}`,
    fields: [
      field('Name', name),
      field('Tags', tags, 'Comma separated.'),
      classChecks('Recommended device classes', m.recommended_classes),
      tierChecks('Recommended performance tiers', m.recommended_tiers),
      h('label.row', null, def, 'Default model'),
    ],
    onSubmit: async (form) => {
      await api('PATCH', '/admin/models/' + encodeURIComponent(m.id), {
        name: name.value.trim(), tags: splitTags(tags.value), recommended_classes: checkedClasses(form),
        recommended_tiers: checkedTiers(form), default: def.checked,
      });
      toast(`${m.id}: saved.`);
      refreshNow();
      return true;
    },
  });
}

async function deleteModel(m) {
  const ok = await confirmDialog({
    title: `Delete ${m.id}?`,
    body: ['The model is removed from the catalog. The controller refuses if a placement policy or the default model refers to it.',
      m.nodes_serving ? `${m.nodes_serving} node(s) serve it right now.` : ''],
    confirm: 'Delete', danger: true,
  });
  if (!ok) return;
  try {
    await api('DELETE', '/admin/models/' + encodeURIComponent(m.id));
    toast(`${m.id}: deleted.`);
  } catch (e) {
    if (e.status === 409) toast(`Cannot delete ${m.id}: ${e.message}`, 'error');
    else if (e.status !== 401 && e.status !== 503) errorToast(e);
  }
  refreshNow();
}

const COLS = ['Model', 'Status', 'Params', 'Quant', { label: 'Size', num: true },
  { label: 'Est. RAM', num: true, title: 'Weights + 16k context KV cache (f16) + overhead' },
  { label: 'Fits', title: 'Device classes where it fits with a 16k context' }, 'Tags', 'Recommended',
  { label: 'Tiers', title: 'Recommended performance tiers (ADR-015)' },
  { label: 'Serving', num: true, title: 'Nodes serving it' }, { label: 'Actions', num: true }];

export default function modelsView() {
  const addBtn = h('button.primary', { type: 'button', onclick: addModel, hidden: true }, 'Add model');
  const body = h('div', null, h('p.muted', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Models'), addBtn), body);

  async function refresh() {
    const res = await getOptional('/admin/models');
    self.live = res !== null; // stop polling a controller without the API; "Check again" retries
    if (res === null) {
      addBtn.hidden = true;
      fill(body, unavailable('Model management', '/admin/models', refreshNow));
      return;
    }
    addBtn.hidden = false;
    const rows = (res.models || []).map((m) => h('tr', null,
      h('td', null, h('span', null, m.name || m.id), m.default ? [' ', badge('default', 'info')] : null,
        h('span.cell-sub.mono', null, m.id), m.license ? h('span.cell-sub', null, m.license) : null),
      h('td', null, statusCell(m)),
      h('td', null, m.params || '–', m.arch ? h('span.cell-sub', null, m.arch) : null),
      h('td', null, m.quant || '–'),
      h('td.num', null, bytes(m.size_bytes)),
      h('td.num', null, bytes(m.est_ram_bytes_16k)),
      h('td', null, chips(m.fits_classes)),
      h('td', null, chips(m.tags)),
      h('td', null, chips(m.recommended_classes)),
      h('td', null, chips(m.recommended_tiers)),
      h('td.num', null, int(m.nodes_serving || 0)),
      h('td.actions', null,
        h('button.small', { type: 'button', onclick: () => editModel(m), 'data-focus-key': m.id + ':edit' }, 'Edit'),
        h('button.small.danger', { type: 'button', onclick: () => deleteModel(m), 'data-focus-key': m.id + ':delete' }, 'Delete'))));
    fill(body, h('div.card', null,
      table(COLS, rows, 'The catalog is empty. Add a model to download it to the controller.')));
  }

  const self = { title: 'Models', el, live: true, refresh };
  return self;
}
