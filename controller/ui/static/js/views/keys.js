// API keys: list with usage, create (the key is shown once), revoke. Below
// them, named admin tokens (ADR-026): one per operator or device, so the
// audit log says who did what.
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

function showCreatedToken(t) {
  return modal((close) => {
    const input = h('input', { readonly: true, value: t.token, 'aria-label': 'New admin token', spellcheck: 'false' });
    return h('div.dlg', null,
      h('h2', null, `Admin token ${t.name}`),
      h('div.banner.warn', null, h('strong', null, 'Copy this token now. '), 'It is shown only once; the controller stores only its hash. ' +
        'Sign in to the panel or set PHONEBORG_ADMIN_TOKEN with it on that device.'),
      h('div.secret', null, input, h('button.primary', { type: 'button', onclick: () => copy(input) }, 'Copy')),
      t.persisted ? null : h('p.muted.small', null, 'No -admin-tokens-file or -state-dir: the token is lost on restart.'),
      h('div.row.end', null, h('button', { type: 'button', onclick: () => close() }, 'Done')));
  });
}

async function revokeToken(name) {
  const ok = await confirmDialog({
    title: `Revoke admin token ${name}?`,
    body: ['Panels and tools using it are signed out at their next request.'],
    confirm: 'Revoke', danger: true,
  });
  if (!ok) return;
  try {
    await api('DELETE', '/admin/tokens/' + encodeURIComponent(name));
    toast(`${name}: revoked.`);
  } catch (e) {
    if (e.status !== 401 && e.status !== 503) errorToast(e);
  }
  refreshNow();
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

  const tokName = h('input', { name: 'token-name', required: true, placeholder: 'laptop', autocomplete: 'off', spellcheck: 'false', maxlength: 32 });
  const tokCreate = h('button.primary', { type: 'submit' }, 'Create admin token');
  const tokForm = h('form', null, field('Name', tokName, 'One token per operator or device, e.g. laptop. Lowercase letters, digits and hyphens.'), tokCreate);
  tokForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    tokCreate.disabled = true;
    try {
      const t = await api('POST', '/admin/tokens', { name: tokName.value.trim() });
      tokName.value = '';
      refreshNow();
      await showCreatedToken(t);
    } catch (ex) {
      if (ex.status !== 401 && ex.status !== 503) errorToast(ex);
    } finally {
      tokCreate.disabled = false;
    }
  });
  const tokList = h('div');
  const tokens = h('div.card.section', null, h('h2', null, 'Admin tokens'),
    h('p.muted', null, 'Tokens for this panel and pbctl. The audit log names the token behind every change.'), tokList, tokForm);

  const el = h('section', null, h('div.page-head', null, h('h1', null, 'API keys')), info, list, form, tokens);

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
    const toks = await get('/admin/tokens').catch(() => null);
    fill(tokList, toks ? table(['Name', 'Source', 'Created', { label: 'Actions', num: true }], toks.tokens.map((t) => [
      h('strong', null, t.name), t.primary ? h('span.mono', null, '-admin-token-file') : 'named', t.primary ? '–' : ago(t.created),
      h('td.actions', null, t.primary ? null : h('button.small.danger', { type: 'button', onclick: () => revokeToken(t.name), 'data-focus-key': t.name + ':tok-revoke' }, 'Revoke')),
    ]), 'No admin tokens.') : h('p.muted', null, 'This controller has no named admin tokens.'));
  }

  return { title: 'API keys', el, live: true, refresh };
}
