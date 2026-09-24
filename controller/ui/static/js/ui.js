// Toasts, dialogs and the details drawer, built on native <dialog>
// (focus trap and Escape come for free).
import { h } from './dom.js';

const toasts = () => document.getElementById('toasts');
let lastError = { msg: '', at: 0 };

export function toast(msg, kind = 'info', ms = kind === 'error' ? 8000 : 4000) {
  const close = h('button', { type: 'button', 'aria-label': 'Dismiss' }, '×');
  const el = h('div.toast', { class: kind, role: kind === 'error' ? 'alert' : null }, h('span', null, msg), close);
  const remove = () => el.remove();
  close.addEventListener('click', remove);
  setTimeout(remove, ms);
  toasts().append(el);
}

// errorToast shows an error once per 30 s, so a failing 5 s refresh does not
// flood the screen.
export function errorToast(err) {
  const msg = err && err.message ? err.message : String(err);
  const now = Date.now();
  if (msg === lastError.msg && now - lastError.at < 30000) return;
  lastError = { msg, at: now };
  toast(msg, 'error');
}

export const anyDialogOpen = () => document.querySelector('dialog[open]') !== null;

// modal(builder) opens a modal dialog. builder(close) returns its content;
// close(value) resolves the returned promise and removes the dialog.
export function modal(builder, cls = '') {
  return new Promise((resolve) => {
    const dlg = h('dialog', { class: cls || null });
    let result;
    const close = (v) => { result = v; dlg.close(); };
    dlg.addEventListener('close', () => { dlg.remove(); resolve(result); });
    dlg.append(builder(close));
    document.body.append(dlg);
    dlg.showModal();
  });
}

// confirmDialog resolves true when the operator confirms.
export function confirmDialog({ title, body, confirm = 'Confirm', danger = false }) {
  return modal((close) => {
    const ok = h('button', { type: 'submit', class: danger ? 'danger solid' : 'primary' }, confirm);
    const form = h('form', { method: 'dialog' },
      h('h2', null, title),
      [].concat(body).filter(Boolean).map((p) => (p instanceof Node ? p : h('p', null, p))),
      h('div.row.end', null,
        h('button', { type: 'button', onclick: () => close(false) }, 'Cancel'),
        ok));
    form.addEventListener('submit', (e) => { e.preventDefault(); close(true); });
    queueMicrotask(() => ok.focus());
    return form;
  }).then((v) => v === true);
}

// formDialog shows fields and calls onSubmit(form) until it resolves without
// throwing; errors are shown inside the dialog.
export function formDialog({ title, fields, submit = 'Save', onSubmit }) {
  return modal((close) => {
    const err = h('p.form-error', { role: 'alert', hidden: true });
    const btn = h('button.primary', { type: 'submit' }, submit);
    const form = h('form', null,
      h('h2', null, title), fields, err,
      h('div.row.end', null, h('button', { type: 'button', onclick: () => close(null) }, 'Cancel'), btn));
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      btn.disabled = true;
      err.hidden = true;
      try {
        close(await onSubmit(form));
      } catch (ex) {
        err.textContent = ex.message;
        err.hidden = false;
      } finally {
        btn.disabled = false;
      }
    });
    return form;
  });
}

export function drawer(title, content) {
  return modal((close) => h('div.dlg.drawer-body', null,
    h('div.row', null, h('h2', { class: 'grow' }, title), h('span.spacer'),
      h('button', { type: 'button', onclick: () => close(), 'aria-label': 'Close details' }, 'Close')),
    content), 'drawer');
}

// field(label, input, hint) wraps an input with its label.
let fieldSeq = 0;
export function field(label, input, hint) {
  input.id = input.id || 'f' + ++fieldSeq;
  return h('div.field', null, h('label', { for: input.id }, label), input, hint ? h('span.hint', null, hint) : null);
}

export function unavailable(what, path, retry) {
  return h('div.card.unavailable', null,
    h('h2', null, `${what} not available on this controller`),
    h('p.muted', null, `This controller does not have the model management API (GET ${path} answered 404). `,
      'Upgrade the controller to manage models and placement here; the other sections work as usual.'),
    retry ? h('button', { type: 'button', onclick: retry }, 'Check again') : null);
}
