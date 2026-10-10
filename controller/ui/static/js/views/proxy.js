// Proxy: the gateway's runtime settings (GET/PUT /admin/gateway).
import { api, get } from '../api.js';
import { h, fill, badge } from '../dom.js';
import { toast, errorToast, confirmDialog, field } from '../ui.js';

export default function proxyView() {
  let current = null;
  let targets = []; // GET /admin/chat/models entries: auto, pools, nodes, models
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
    const firstToken = h('input', { name: 'first_token_timeout', required: true, value: s.first_token_timeout || '', spellcheck: 'false' });
    const thermal = h('input', { type: 'number', name: 'thermal', min: 0, max: 150, step: 'any', required: true, value: String(s.thermal_limit_c) });
    const err = h('p.form-error', { role: 'alert', hidden: true });
    const save = h('button.primary', { type: 'submit' }, 'Save');

    const form = h('form.card.section', null,
      h('h2', null, 'Routing'),
      field('Policy', policy, 'affinity sends requests that share a prompt prefix to the same phone, so its prompt cache is reused; it moves a session only when that phone is busy, hot or gone.'),
      field('Affinity spill', spill, 'Extra in-flight requests a pinned phone may have over the least busy one before a session spills to another phone (1–1000).'),
      field('Upstream timeout', timeout, 'Longest silence from a node once its answer has started (between streamed chunks), e.g. 600s (1s–24h). A node that keeps generating is never cut off.'),
      field('First-token timeout', firstToken, 'Longest wait for a node to start answering, i.e. prompt processing, e.g. 30m (1s–24h). Agent prompts of 10k+ tokens take a phone many minutes.'),
      field('Thermal limit (°C)', thermal, 'Phones at or above this temperature get no new sessions until they cool down. 0 disables it.'),
      err,
      h('div.row', null, save, h('span.muted.small', null, 'Changes apply at once and are not saved: after a restart the controller uses its flags again.')));

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const u = {};
      if (policy.value !== current.policy) u.policy = policy.value;
      if (Number(spill.value) !== current.affinity_spill) u.affinity_spill = Number(spill.value);
      if (timeout.value.trim() !== current.upstream_timeout) u.upstream_timeout = timeout.value.trim();
      if (firstToken.value.trim() !== current.first_token_timeout) u.first_token_timeout = firstToken.value.trim();
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

    fill(body, form, routerForm(s.router), auth);
  }

  // Semantic router (ADR-033): its own form, so switching it on or off
  // touches nothing else.
  function routerForm(r) {
    const enabled = h('input', { type: 'checkbox', name: 'router_enabled', checked: r.enabled });
    const classifier = targetSelect('router_classifier', r.classifier, true);
    const easy = targetSelect('router_easy', r.easy, false);
    const hard = targetSelect('router_hard', r.hard, false);
    const threshold = h('input', { type: 'number', name: 'router_threshold', min: 0.01, max: 0.99, step: 0.01, required: true, value: String(r.threshold) });
    const timeout = h('input', { name: 'router_timeout', required: true, value: r.timeout, spellcheck: 'false' });
    const err = h('p.form-error', { role: 'alert', hidden: true });
    const save = h('button.primary', { type: 'submit' }, 'Save router');
    const f = h('form.card.section', null,
      h('h2', null, 'Semantic router ', badge(r.enabled ? 'on' : 'off', r.enabled ? 'ok' : 'idle'), ' ', badge('experimental', 'warn')),
      h('p.muted', null, 'Requests for "auto" are first classified as easy or hard by one phone (a one-token answer read from its logprobs), then sent to the easy or the hard target. Any classifier failure serves the request as plain auto. Other models, pools and nodes are not affected.'),
      h('label', null, enabled, ' Enabled'),
      field('Classifier', classifier, 'The phone that classifies. Prefer a Qwen3 or Llama model: Gemma 3/3n reprocess the whole prompt every time (see OPERATIONS.md).'),
      field('Easy target', easy, 'Where easy requests go: a pool, a phone or a model.'),
      field('Hard target', hard, 'Where hard requests go.'),
      field('Threshold', threshold, 'P(hard) at or above which a request is hard (0–1).'),
      field('Timeout', timeout, 'Longest wait for the classifier, e.g. 20s; after it the request is plain auto.'),
      err,
      h('div.row', null, save, h('span.muted.small', null, 'Applies at once, not saved across a controller restart.')));
    f.addEventListener('submit', async (e) => {
      e.preventDefault();
      const u = {
        enabled: enabled.checked, classifier: classifier.value, easy: easy.value, hard: hard.value,
        threshold: Number(threshold.value), timeout: timeout.value.trim(),
      };
      save.disabled = true;
      err.hidden = true;
      try {
        render(await api('PUT', '/admin/gateway', { router: u }));
        toast(u.enabled ? 'Semantic router on.' : 'Semantic router off.');
      } catch (ex) {
        err.textContent = ex.message;
        err.hidden = false;
      } finally {
        save.disabled = false;
      }
    });
    return f;
  }

  // targetSelect lists the routing targets grouped as pools, phones and
  // models (phones only for the classifier). A configured target that is
  // gone (phone offline, pool deleted) stays selectable, marked.
  function targetSelect(name, value, phonesOnly) {
    const groups = phonesOnly
      ? [['Phones', 'node']]
      : [['Pools', 'pool'], ['Phones', 'node'], ['Models', 'model']];
    const opt = (id, label) => h('option', { value: id }, label || id);
    const known = new Set(targets.map((t) => t.id));
    const first = phonesOnly ? opt('', '— choose a phone —') : opt('', 'auto (any phone)');
    const missing = value && !known.has(value) ? opt(value, `${value} (not available now)`) : null;
    return h('select', { name, value },
      first, missing,
      groups.map(([label, kind]) => {
        const items = targets.filter((t) => t.kind === kind);
        return items.length ? h('optgroup', { label }, items.map((t) =>
          opt(t.id, kind === 'node' && t.model ? `${t.id} — ${t.model}` : t.id))) : null;
      }));
  }

  async function refresh() {
    const [s, models] = await Promise.all([get('/admin/gateway'), get('/admin/chat/models').catch(() => ({ data: [] }))]);
    targets = models.data || [];
    render(s);
  }
  reload.addEventListener('click', () => window.dispatchEvent(new Event('pb:refresh')));

  // Not live: a refresh would overwrite what the operator is typing.
  return { title: 'Proxy', el, live: false, refresh };
}
