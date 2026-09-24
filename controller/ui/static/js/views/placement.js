// Placement: which model each node serves. Policies are edited locally,
// previewed (POST /admin/placement/preview) and only then applied (PUT).
import { api, getOptional } from '../api.js';
import { h, fill, table, badge, bytes, int, CLASSES } from '../dom.js';
import { toast, errorToast, unavailable } from '../ui.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));
const MODES = { pin: 'Pin to nodes', replicas: 'Replicas', percent: 'Percent of nodes' };

// clean returns the request body for PUT/preview from the editor state.
function clean(draft) {
  return {
    default_model: draft.default_model || '',
    policies: draft.policies.map((p) => ({
      model_id: p.model_id, mode: p.mode,
      nodes: p.mode === 'pin' ? [...p.nodes] : [],
      replicas: p.mode === 'replicas' ? Number(p.replicas) || 0 : 0,
      percent: p.mode === 'percent' ? Number(p.percent) || 0 : 0,
      classes: [...p.classes],
    })),
  };
}

function fromServer(pl) {
  return {
    default_model: pl.default_model || '',
    policies: (pl.policies || []).map((p) => ({
      model_id: p.model_id || '', mode: p.mode || 'replicas', nodes: p.nodes || [],
      replicas: p.replicas || 1, percent: p.percent || 0, classes: p.classes || [],
    })),
  };
}

function validate(body) {
  for (const [i, p] of body.policies.entries()) {
    const n = `Policy ${i + 1}`;
    if (!p.model_id) return `${n}: choose a model.`;
    if (p.mode === 'pin' && !p.nodes.length) return `${n}: pick at least one node.`;
    if (p.mode === 'replicas' && !(p.replicas >= 1)) return `${n}: replicas must be at least 1.`;
    if (p.mode === 'percent' && !(p.percent >= 0 && p.percent <= 100)) return `${n}: percent must be 0–100.`;
  }
  return '';
}

function runtimeCell(n) {
  if (!n) return '–';
  return [n.state ? badge(n.state) : h('span.muted', null, '–'), n.drained ? badge('DRAINED') : null,
    n.state === 'downloading' && n.progress > 0
      ? h('span.cell-sub', null, h('progress', { max: 1, value: n.progress, 'aria-label': 'download progress' }), ` ${Math.round(n.progress * 100)}%`)
      : null,
    n.error ? h('span.cell-sub', { title: n.error }, n.error) : null];
}

// planTable shows, per node, the current and the planned model.
function planTable(pl) {
  const nodes = Object.fromEntries((pl.nodes || []).map((n) => [n.node_id, n]));
  const rows = (pl.plan || []).map((p) => {
    const n = nodes[p.node_id];
    const cur = n ? n.current_model || '' : '';
    const changed = (p.model_id || '') !== cur;
    return h('tr', { class: changed ? 'changed' : null },
      h('td', null, h('span.mono', null, p.node_id)),
      h('td', null, n ? n.class || '–' : '–'),
      h('td', null, runtimeCell(n)),
      h('td', null, cur || h('span.muted', null, 'none')),
      h('td', null, changed ? h('strong', null, '→ ', p.model_id || 'none') : h('span.muted', null, p.model_id ? 'unchanged' : 'none')),
      h('td', null, p.reason || '–'),
      h('td.num', null, p.ctx_size ? `${int(p.ctx_size)} · ${p.slots || 1} · ${p.kv_type || '?'}` : '–'),
      h('td.num', null, `${bytes(p.est_ram_bytes)} / ${bytes(n && n.ram_total_bytes)}`),
      h('td', null, p.model_id ? (p.fits ? badge('fits', 'ok') : badge('too big', 'bad')) : '–'));
  });
  const changes = rows.filter((r) => r.classList.contains('changed')).length;
  return {
    changes,
    el: table(['Node', 'Class', 'Runtime', 'Current model', 'Target', 'Reason',
      { label: 'Ctx · slots · KV', num: true }, { label: 'Est. RAM / total', num: true }, 'Fits'], rows, 'No nodes to place.'),
  };
}

const warningsList = (w) => (w && w.length ? h('div.banner.warn', null, h('strong', null, 'Warnings'), h('ul.warnings', null, w.map((x) => h('li', null, x)))) : null);

export default function placementView() {
  let server = null; // last GET /admin/placement
  let serverSig = '';
  let draft = null;
  let models = [];
  let preview = null;
  let previewSig = '';

  const dirtyBadge = h('span', { hidden: true }, badge('unsaved changes', 'warn'));
  const body = h('div', null, h('p.muted', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Placement'), dirtyBadge), body);

  const classesCard = h('div.card.section');
  const editorCard = h('div.card.section');
  const previewCard = h('div.card.section', { hidden: true, 'aria-live': 'polite' });
  const planCard = h('div.card.section');

  const sig = () => JSON.stringify(clean(draft));
  const isDirty = () => draft && sig() !== serverSig;
  const modelById = (id) => models.find((m) => m.id === id);

  function eligible(p) {
    const m = modelById(p.model_id);
    const fits = m && m.fits_classes;
    return (server.nodes || []).filter((n) =>
      (!p.classes.length || p.classes.includes(n.class)) && (!fits || fits.includes(n.class)));
  }

  function updateButtons() {
    const dirty = isDirty();
    dirtyBadge.hidden = !dirty;
    const apply = editorCard.querySelector('[data-act="apply"]');
    if (!apply) return; // editor still being built
    apply.disabled = !dirty || previewSig !== sig();
    editorCard.querySelector('[data-act="discard"]').disabled = !dirty;
    if (previewSig && previewSig !== sig()) previewCard.hidden = true;
  }

  function modelSelect(value, onchange, emptyLabel) {
    const opts = models.map((m) => h('option', { value: m.id }, m.id + (m.status && m.status !== 'ready' ? ` (${m.status})` : '')));
    if (value && !modelById(value)) opts.push(h('option', { value }, value + ' (unknown)'));
    return h('select', { value, onchange: (e) => onchange(e.target.value) },
      emptyLabel != null ? h('option', { value: '' }, emptyLabel) : null, opts);
  }

  function policyRow(p, i) {
    const count = h('output');
    const updateCount = () => {
      const el = eligible(p).length;
      if (p.mode === 'percent') {
        const n = p.percent > 0 && el > 0 ? Math.max(1, Math.floor((el * p.percent) / 100 + 0.5)) : 0;
        count.textContent = `${p.percent}% → ${n} of ${el} eligible node(s)`;
      } else if (p.mode === 'replicas') {
        count.textContent = `${el} eligible node(s)`;
      } else {
        count.textContent = `${p.nodes.length} node(s) pinned`;
      }
      updateButtons();
    };
    const change = (fn, rebuild = false) => (e) => { fn(e); if (rebuild) renderEditor(); else updateCount(); };

    let modeControl;
    if (p.mode === 'pin') {
      const pinned = new Set(p.nodes);
      const known = (server.nodes || []).map((n) => n.node_id);
      const ids = [...known, ...p.nodes.filter((id) => !known.includes(id))];
      modeControl = h('fieldset.pin-list', null, h('legend', null, 'Nodes'),
        ids.length ? h('div.checks', null, ids.map((id) => {
          const n = (server.nodes || []).find((x) => x.node_id === id);
          return h('label', null, h('input', { type: 'checkbox', value: id, checked: pinned.has(id),
            onchange: change((e) => { p.nodes = e.target.checked ? [...p.nodes, id] : p.nodes.filter((x) => x !== id); }) }),
          h('span.mono', null, id), n ? h('span.muted', null, ` ${n.class || ''}`) : ' (unknown)');
        })) : h('p.muted', null, 'No nodes.'));
    } else if (p.mode === 'replicas') {
      const input = h('input', { type: 'number', min: 1, max: 1000, value: String(p.replicas), 'aria-label': 'Replicas',
        oninput: change((e) => { p.replicas = Number(e.target.value); }) });
      modeControl = h('div.row', null, h('label', null, 'Replicas ', input), count);
    } else {
      const input = h('input', { type: 'range', min: 0, max: 100, step: 1, value: String(p.percent), 'aria-label': 'Percent of eligible nodes',
        oninput: change((e) => { p.percent = Number(e.target.value); }) });
      modeControl = h('div.pct', null, input, count);
    }

    const classes = h('fieldset', null, h('legend', null, 'Only device classes (none = all)'),
      h('div.checks', null, CLASSES.map((c) => h('label', null,
        h('input', { type: 'checkbox', value: c.id, checked: p.classes.includes(c.id),
          onchange: change((e) => { p.classes = e.target.checked ? [...p.classes, c.id] : p.classes.filter((x) => x !== c.id); }) }),
        c.id))));

    const row = h('div.policy', { role: 'group', 'aria-label': `Policy ${i + 1}` },
      h('div.row', null,
        h('label', { class: 'grow' }, 'Model ', modelSelect(p.model_id, (v) => { p.model_id = v; updateCount(); }, '— choose —')),
        h('label', null, 'Mode ', h('select', { value: p.mode, onchange: change((e) => { p.mode = e.target.value; }, true) },
          Object.entries(MODES).map(([k, v]) => h('option', { value: k }, v)))),
        h('button.small.danger', { type: 'button', onclick: () => { draft.policies.splice(i, 1); renderEditor(); } }, 'Remove')),
      modeControl, classes, p.mode === 'pin' ? count : null);
    updateCount();
    return row;
  }

  function renderEditor() {
    const err = h('p.form-error', { role: 'alert', hidden: true });
    const showErr = (m) => { err.textContent = m; err.hidden = !m; };
    const previewBtn = h('button', { type: 'button', 'data-act': 'preview' }, 'Preview');
    const applyBtn = h('button.primary', { type: 'button', 'data-act': 'apply', title: 'Preview the current changes first' }, 'Apply');
    const discardBtn = h('button', { type: 'button', 'data-act': 'discard' }, 'Discard changes');

    previewBtn.addEventListener('click', async () => {
      const req = clean(draft);
      const bad = validate(req);
      showErr(bad);
      if (bad) return;
      previewBtn.disabled = true;
      try {
        preview = await api('POST', '/admin/placement/preview', req);
        previewSig = JSON.stringify(req);
        renderPreview();
      } catch (e) {
        if (e.status !== 401 && e.status !== 503) showErr(e.message);
      } finally {
        previewBtn.disabled = false;
        updateButtons();
      }
    });
    applyBtn.addEventListener('click', async () => {
      applyBtn.disabled = true;
      try {
        server = await api('PUT', '/admin/placement', clean(draft));
        serverSig = JSON.stringify(clean(fromServer(server)));
        draft = fromServer(server);
        preview = null;
        previewSig = '';
        previewCard.hidden = true;
        toast('Placement applied. Nodes pick it up on their next heartbeat.');
        renderEditor();
        renderPlan();
      } catch (e) {
        if (e.status !== 401 && e.status !== 503) { showErr(e.message); errorToast(e); }
        updateButtons();
      }
    });
    discardBtn.addEventListener('click', () => {
      draft = fromServer(server);
      preview = null;
      previewSig = '';
      previewCard.hidden = true;
      renderEditor();
    });

    fill(editorCard, 
      h('h2', null, 'Policies'),
      h('p.muted.small', null, 'Pins are placed first, then replicas, then percentages; remaining nodes get the default model, or keep what they serve.'),
      draft.policies.length ? draft.policies.map(policyRow) : h('p.muted', null, 'No policies.'),
      h('div.row', null, h('button', { type: 'button', onclick: () => {
        draft.policies.push({ model_id: '', mode: 'replicas', nodes: [], replicas: 1, percent: 50, classes: [] });
        renderEditor();
      } }, '+ Add policy')),
      h('div.field', null, h('label', { for: 'pl-default' }, 'Default model'),
        Object.assign(modelSelect(draft.default_model, (v) => { draft.default_model = v; updateButtons(); }, '(none: keep current model)'), { id: 'pl-default' }),
        h('span.hint', null, 'Served by nodes that no policy assigns.')),
      err,
      h('div.row', null, previewBtn, applyBtn, discardBtn));
    updateButtons();
  }

  function renderPreview() {
    const t = planTable(preview);
    fill(previewCard, 
      h('h2', null, 'Preview'),
      h('p', null, `${t.changes} of ${(preview.plan || []).length} node(s) change model. Nothing is applied until you press Apply.`),
      warningsList(preview.warnings), t.el);
    previewCard.hidden = false;
  }

  function renderClasses(classes) {
    const rows = (classes || []).map((c) => [
      h('strong', null, c.id), c.label || '–',
      { v: `${bytes(c.min_ram_bytes)} – ${c.max_ram_bytes ? bytes(c.max_ram_bytes) : '∞'}`, num: true },
      { v: int(c.nodes), num: true },
      (c.recommended_models || []).length ? c.recommended_models.map((m) => h('span.chip', null, m)) : h('span.muted', null, '–')]);
    fill(classesCard, h('h2', null, 'Device classes'),
      table(['Class', 'Label', { label: 'RAM range', num: true }, { label: 'Nodes', num: true }, 'Recommended models'], rows, 'No classes reported.'));
  }

  function renderPlan() {
    const t = planTable(server);
    fill(planCard, h('h2', null, 'Current plan', h('span.muted.small', null, `${t.changes} node(s) still moving to their target`)),
      warningsList(server.warnings), t.el);
  }

  async function refresh() {
    const pl = await getOptional('/admin/placement');
    self.live = pl !== null; // stop polling a controller without the API; "Check again" retries
    if (pl === null) {
      server = null;
      fill(body, unavailable('Placement', '/admin/placement', refreshNow));
      return;
    }
    const [cls, cat] = await Promise.all([getOptional('/admin/device-classes'), getOptional('/admin/models')]);
    const first = server === null;
    const wasDirty = !first && isDirty();
    server = pl;
    models = (cat && cat.models) || [];
    const newSig = JSON.stringify(clean(fromServer(pl)));
    renderClasses(cls && cls.classes);
    renderPlan();
    if (first || (!wasDirty && newSig !== serverSig)) {
      // Pick up the server's policies unless the operator is editing.
      serverSig = newSig;
      draft = fromServer(pl);
      renderEditor();
    } else {
      serverSig = newSig;
      updateButtons();
    }
    if (first) fill(body, classesCard, editorCard, previewCard, planCard);
  }

  const self = { title: 'Placement', el, live: true, refresh };
  return self;
}
