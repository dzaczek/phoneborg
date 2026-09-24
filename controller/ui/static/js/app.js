// Panel shell: login, navigation, auto-refresh and settings.
import { api, token, setAuthErrorHandler } from './api.js';
import { h } from './dom.js';
import { toast, errorToast, anyDialogOpen, formDialog, field } from './ui.js';
import overview from './views/overview.js';
import nodes from './views/nodes.js';
import models from './views/models.js';
import placement from './views/placement.js';
import proxy from './views/proxy.js';
import keys from './views/keys.js';
import usage from './views/usage.js';

// Each view factory returns {title, el, live, refresh()}; live views are
// refreshed every REFRESH_MS while the tab is visible and no dialog is open.
const VIEWS = { overview, nodes, models, placement, proxy, keys, usage };
const REFRESH_MS = 5000;

const $ = (id) => document.getElementById(id);
let view = null;
let lastOK = 0;

// ---------- login ----------

function showLogin(message = '') {
  view = null;
  $('app').hidden = true;
  $('login').hidden = false;
  const err = $('login-error');
  err.textContent = message;
  err.hidden = !message;
  $('login-token').value = '';
  $('login-token').focus();
}

function signOut(message) {
  token.clear();
  document.querySelectorAll('dialog[open]').forEach((d) => d.close());
  showLogin(message);
}

setAuthErrorHandler((err) => {
  signOut(err.status === 401 ? 'The admin token was rejected. Sign in again.' : err.message);
});

$('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const tok = $('login-token').value.trim();
  if (!tok) return;
  const btn = e.submitter || $('login-form').querySelector('button');
  btn.disabled = true;
  try {
    await api('GET', '/admin/gateway', undefined, tok); // cheap check of the token
    token.set(tok);
    showApp();
  } catch (err) {
    if (err.status !== 401 && err.status !== 503) showLogin(err.message); // those two are shown by the handler
  } finally {
    btn.disabled = false;
  }
});

$('btn-logout').addEventListener('click', () => signOut('Signed out.'));

// ---------- navigation ----------

function route() {
  const name = location.hash.replace(/^#\/?/, '');
  return VIEWS[name] ? name : 'overview';
}

function showApp() {
  $('login').hidden = true;
  $('app').hidden = false;
  navigate(false);
}

function navigate(focus = true) {
  if (!token.get()) return showLogin();
  const name = route();
  view = VIEWS[name]();
  document.title = `${view.title} · PhoneBorg`;
  for (const a of $('nav').querySelectorAll('a')) {
    if (a.getAttribute('href') === '#/' + name) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  $('main').replaceChildren(view.el);
  if (focus) $('main').focus();
  refresh(true);
}

window.addEventListener('hashchange', () => navigate());

async function refresh(force = false) {
  const v = view;
  if (!v || v.busy || (!force && (!v.live || document.hidden || anyDialogOpen()))) return;
  v.busy = true;
  // Re-rendering replaces buttons; put keyboard focus back on the same one.
  const focusKey = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.focusKey : '';
  try {
    await v.refresh();
    if (focusKey && view === v) {
      const again = $('main').querySelector(`[data-focus-key="${CSS.escape(focusKey)}"]`);
      if (again) again.focus();
    }
    lastOK = Date.now();
    setLive(true);
  } catch (err) {
    if (view !== v) return; // navigated away meanwhile
    setLive(false);
    if (err.status !== 401 && err.status !== 503) errorToast(err);
  } finally {
    v.busy = false;
  }
}

function setLive(ok) {
  const el = $('live');
  el.className = 'live ' + (ok ? 'ok' : 'err');
  el.textContent = ok ? (view && view.live ? 'Live' : 'Updated') : 'Update failed';
  el.title = lastOK ? 'Last successful update ' + new Date(lastOK).toLocaleTimeString() : '';
}

setInterval(refresh, REFRESH_MS);
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
// Views ask for an immediate refresh after an action.
window.addEventListener('pb:refresh', () => refresh(true));

// ---------- external links (Grafana, Prometheus) ----------

const LINKS = {
  grafana: { el: 'link-grafana', port: 3000 },
  prometheus: { el: 'link-prometheus', port: 9090 },
};

function safeURL(s) {
  try {
    const u = new URL(s, location.href);
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.href : '';
  } catch { return ''; }
}

function linkURL(name) {
  let saved = '';
  try { saved = localStorage.getItem('phoneborg.link.' + name) || ''; } catch { /* ignore */ }
  return safeURL(saved) || `${location.protocol}//${location.hostname}:${LINKS[name].port}/`;
}

function saveLink(name, value) {
  try {
    if (value) localStorage.setItem('phoneborg.link.' + name, value);
    else localStorage.removeItem('phoneborg.link.' + name);
  } catch { /* ignore */ }
}

function applyLinks() {
  for (const name of Object.keys(LINKS)) $(LINKS[name].el).href = linkURL(name);
}

// ?grafana=URL&prometheus=URL presets the links, then is removed from the address bar.
(function linksFromQuery() {
  const q = new URLSearchParams(location.search);
  let changed = false;
  for (const name of Object.keys(LINKS)) {
    const v = safeURL(q.get(name) || '');
    if (v) { saveLink(name, v); changed = true; }
  }
  if (changed || location.search) history.replaceState(null, '', location.pathname + location.hash);
})();

$('btn-settings').addEventListener('click', () => {
  const inputs = Object.fromEntries(Object.keys(LINKS).map((name) => [name,
    h('input', { type: 'url', value: linkURL(name), placeholder: `http://host:${LINKS[name].port}/`, spellcheck: 'false' })]));
  formDialog({
    title: 'Settings',
    fields: [
      field('Grafana URL', inputs.grafana, 'Opened from the top bar. Stored in this browser only.'),
      field('Prometheus URL', inputs.prometheus),
      h('p.muted.small', null, 'Tip: open /ui/?grafana=URL&prometheus=URL to preset these links. Clear a field to use the default port on this host.'),
    ],
    onSubmit: () => {
      for (const [name, input] of Object.entries(inputs)) {
        const raw = input.value.trim();
        const v = safeURL(raw);
        if (raw && !v) throw new Error(`${name}: enter an http:// or https:// URL`);
        saveLink(name, v);
      }
      applyLinks();
      toast('Settings saved.');
      return true;
    },
  });
});

// ---------- start ----------

applyLinks();
if (token.get()) showApp();
else showLogin();
