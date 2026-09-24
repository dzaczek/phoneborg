// Proxy: the gateway's runtime settings (GET/PUT /admin/gateway).
import { api, get } from '../api.js';
import { h, fill, badge } from '../dom.js';
import { toast, errorToast, confirmDialog, field } from '../ui.js';

export default function proxyView() {
  let current = null;
  const body = h('div', null, h('p.muted', null, 'Loading…'));
  const reload = h('button', { type: 'button' }, 'Reload');
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Proxy'), reload), body);

  function render(s) {
    current = s;
    const policy = h('select', { name: 'policy', value: s.policy },
      h('option', { value: 'affinity' }, 'affinity: keep sessions on one phone'),
      h('option', { value: 'least_inflight' }, 'least_inflight: plain load balancing'));
    const spill = h('input', { type: 'number', name: 'spill', min: 1, max: 1000, required: true, value: String(s.affinity_spill) });
    const timeout = h('input', { name: 'timeout', required: true, value: s.upstream_timeout, spellcheck: 'false' });
    const thermal = h('input', { type: 'number', name: 'thermal', min: 0, max: 150, step: 'any', required: true, value: String(s.thermal_limit_c) });
    const err = h('p.form-error', { role: 'alert', hidden: true });
    const save = h('button.primary', { type: 'submit' }, 'Save');

    const form = h('form.card.section', null,
      h('h2', null, 'Routing'),
      field('Policy', policy, 'affinity sends requests that share a prompt prefix to the same phone, so its prompt cache is reused; it moves a session only when that phone is busy, hot or gone.'),
      field('Affinity spill', spill, 'Extra in-flight requests a pinned phone may have over the least busy one before a session spills to another phone (1–1000).'),
      field('Upstream timeout', timeout, 'Longest a single proxied request may run, e.g. 600s or 15m (1s–24h). Applies to requests that start afterwards.'),
      field('Thermal limit (°C)', thermal, 'Phones at or above this temperature get no new sessions until they cool down. 0 disables it.'),
      err,
      h('div.row', null, save, h('span.muted.small', null, 'Changes apply at once and are not saved: after a restart the controller uses its flags again.')));

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const u = {};
      if (policy.value !== current.policy) u.policy = policy.value;
      if (Number(spill.value) !== current.affinity_spill) u.affinity_spill = Number(spill.value);
      if (timeout.value.trim() !== current.upstream_timeout) u.upstream_timeout = timeout.value.trim();
      if (Number(thermal.value) !== current.thermal_limit_c) u.thermal_limit_c = Number(thermal.value);
      if (!Object.keys(u).length) { toast('Nothing changed.'); return; }
      save.disabled = true;
      err.hidden = true;
      try {
        render(await api('PUT', '/admin/gateway', u));
        toast('Gateway settings saved.');
      } catch (ex) {
        err.textContent = ex.message;
        err.hidden = false;
      } finally {
        save.disabled = false;
      }
    });

    const enforce = h('button.danger', { type: 'button' }, 'Enforce API keys');
    enforce.addEventListener('click', async () => {
      const ok = await confirmDialog({
        title: 'Enforce API keys?',
        body: ['From now on, gateway requests without a valid API key get HTTP 401.',
          h('p', null, h('strong', null, 'This is one-way: '), 'key enforcement cannot be turned off at runtime. Opening the gateway again needs a controller restart without -api-keys-file.'),
          'Create keys and give them to every client first. The controller refuses while no key exists.'],
        confirm: 'Enforce keys', danger: true,
      });
      if (!ok) return;
      try {
        render(await api('PUT', '/admin/gateway', { auth_mode: 'keys' }));
        toast('API keys are now enforced.');
      } catch (ex) {
        if (ex.status !== 401 && ex.status !== 503) errorToast(ex);
      }
    });

    const auth = h('div.card.section', null,
      h('h2', null, 'Authentication ', badge(s.auth_mode === 'keys' ? 'keys enforced' : 'open', s.auth_mode === 'keys' ? 'ok' : 'warn')),
      s.auth_mode === 'keys'
        ? h('p', null, 'Every gateway request needs a valid API key. This cannot be switched back to open at runtime.')
        : [h('p', null, 'The gateway is open: requests without a key, or with an unknown one, are served as "anonymous". Requests with a known key are counted under its name.'),
          h('div.row', null, enforce, h('a', { href: '#/keys' }, 'Manage API keys'))]);

    fill(body, form, auth);
  }

  async function refresh() {
    render(await get('/admin/gateway'));
  }
  reload.addEventListener('click', () => window.dispatchEvent(new Event('pb:refresh')));

  // Not live: a refresh would overwrite what the operator is typing.
  return { title: 'Proxy', el, live: false, refresh };
}
