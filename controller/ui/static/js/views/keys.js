// API keys: list with usage, create (the key is shown once), revoke.
import { api, get } from '../api.js';
import { h, fill, table, badge, int, tps, ago } from '../dom.js';
import { toast, errorToast, confirmDialog, modal, field } from '../ui.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));

async function copy(input) {
  try {
    await navigator.clipboard.writeText(input.value);
    toast('Copied to the clipboard.');
  } catch {
    // No clipboard API (plain http on another host): select it for Ctrl+C.
    input.focus();
    input.select();
    toast('Press Ctrl+C (or Cmd+C) to copy the selected key.');
  }
}

function showCreated(k) {
  return modal((close) => {
    const input = h('input', { readonly: true, value: k.key, 'aria-label': 'New API key', spellcheck: 'false' });
    return h('div.dlg', null,
      h('h2', null, `API key for ${k.name}`),
      h('div.banner.warn', null, h('strong', null, 'Copy this key now. '), 'It is shown only once; the controller stores only its hash.'),
      h('div.secret', null, input, h('button.primary', { type: 'button', onclick: () => copy(input) }, 'Copy')),
      (k.notes || []).map((n) => h('p.muted.small', null, n)),
      h('div.row.end', null, h('button', { type: 'button', onclick: () => close() }, 'Done')));
  });
}

async function revoke(name) {
  const ok = await confirmDialog({
    title: `Revoke keys of ${name}?`,
    body: ['Every key owned by this name stops working immediately. Its usage history is kept.'],
    confirm: 'Revoke', danger: true,
  });
  if (!ok) return;
  try {
    await api('DELETE', '/admin/keys/' + encodeURIComponent(name));
    toast(`${name}: revoked.`);
  } catch (e) {
    if (e.status !== 401 && e.status !== 503) errorToast(e);
  }
  refreshNow();
}

const COLS = ['Name', { label: 'Keys', num: true }, 'Created', { label: 'Requests', num: true }, { label: 'Errors', num: true },
  { label: 'Prompt tok', num: true }, { label: 'Completion tok', num: true }, { label: 'Avg tok/s', num: true }, 'Last used',
  { label: 'Actions', num: true }];

export default function keysView() {
  const name = h('input', { name: 'name', required: true, placeholder: 'alice', autocomplete: 'off', spellcheck: 'false', maxlength: 64 });
  const create = h('button.primary', { type: 'submit' }, 'Create key');
  const form = h('form.card.section', null, h('h2', null, 'New key'),
    field('Owner name', name, 'Usage is counted per name. The key is shown once, right after creation.'), create);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    create.disabled = true;
    try {
      const k = await api('POST', '/admin/keys', { name: name.value.trim() });
      name.value = '';
      refreshNow();
      await showCreated(k);
    } catch (ex) {
      if (ex.status !== 401 && ex.status !== 503) errorToast(ex);
    } finally {
      create.disabled = false;
    }
  });

  const info = h('div');
  const list = h('div.card', null, h('p.empty', null, 'Loading…'));
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'API keys')), info, list, form);

  async function refresh() {
    const res = await get('/admin/keys');
    fill(info, 
      h('div.banner', { class: res.auth_mode === 'keys' ? 'info' : 'warn' },
        'Gateway: ', badge(res.auth_mode === 'keys' ? 'keys enforced' : 'open', res.auth_mode === 'keys' ? 'ok' : 'warn'), ' ',
        res.auth_mode === 'keys' ? 'requests need a valid key. ' : 'requests without a valid key are served as "anonymous" (enforce keys under Proxy). ',
        res.persisted ? 'Keys are saved to the API keys file.' : 'No -api-keys-file: keys live in memory and are lost on restart.'));
    const rows = res.keys.map((k) => [
      h('strong', null, k.name), { v: int(k.keys), num: true }, ago(k.created),
      { v: int(k.usage.requests), num: true }, { v: int(k.usage.errors), num: true },
      { v: int(k.usage.prompt_tokens), num: true }, { v: int(k.usage.completion_tokens), num: true },
      { v: tps(k.usage.avg_gen_tokens_per_second), num: true }, ago(k.usage.last_used),
      h('td.actions', null, h('button.small.danger', { type: 'button', onclick: () => revoke(k.name), 'data-focus-key': k.name + ':revoke' }, 'Revoke')),
    ]);
    fill(list, table(COLS, rows, 'No API keys yet.'),
      h('p.muted.small.section', null, 'Usage is the persisted total when the controller runs with -state-dir, otherwise since start.'));
  }

  return { title: 'API keys', el, live: true, refresh };
}
