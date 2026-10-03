// Jobs: Super Borg jobs (ADR-021), long multi-step work the orchestrator runs
// in the background with a task list and documents. The list and the selected
// job refresh live; the message box is built once so typing survives refreshes.
import { api, get, getOptional, token } from '../api.js';
import { h, fill, table, badge, ago } from '../dom.js';
import { toast, errorToast, confirmDialog, formDialog, field, drawer, unavailable } from '../ui.js';
import { markdown } from '../md.js';

const refreshNow = () => window.dispatchEvent(new Event('pb:refresh'));
const STATUS_CLASS = { queued: 'info', running: 'serving', waiting: 'warn', done: 'ok', failed: 'bad', cancelled: 'idle' };
const statusBadge = (s) => badge(s, STATUS_CLASS[s] || 'info');
const base = (id) => '/admin/jobs/' + encodeURIComponent(id);
const quiet = (e) => { if (e.status !== 401 && e.status !== 503) errorToast(e); };

let selected = ''; // kept across view re-creation

function newJobDialog() {
  const title = h('input', { name: 'title', maxlength: 80, placeholder: '(optional) e.g. Bajka o zamku' });
  const goal = h('textarea', { name: 'goal', rows: 6, required: true,
    placeholder: 'What should the cluster produce? e.g. Napisz bajkę dla 5-latka o magicznym zamku: 20 rozdziałów po ok. 3 minuty czytania.' });
  return formDialog({
    title: 'New job',
    submit: 'Start',
    fields: [field('Title', title), field('Goal', goal,
      'The orchestrator plans tasks, writes key documents itself and delegates the rest to the other phones. Follow it here.')],
    onSubmit: async () => {
      const job = await api('POST', '/admin/jobs', { title: title.value.trim(), goal: goal.value.trim() });
      selected = job.id;
      toast(`Job ${job.title} queued.`);
      refreshNow();
      return true;
    },
  });
}

async function showDoc(job, name) {
  try {
    const d = await get(base(job.id) + '/docs/' + encodeURIComponent(name));
    drawer(`${d.name} · ${d.author}`, markdown(d.text));
  } catch (e) { quiet(e); }
}

// fetchResult gets the assembled Markdown with the admin token (a plain
// link cannot send the bearer header).
async function fetchResult(job) {
  const res = await fetch(base(job.id) + '/result', { headers: { Authorization: 'Bearer ' + token.get() }, cache: 'no-store' });
  if (!res.ok) throw new Error('HTTP ' + res.status);
  return res.text();
}

async function previewResult(job) {
  try {
    const md = await fetchResult(job);
    const words = md.split(/\s+/).filter(Boolean).length;
    drawer(`${job.title} · ${words} words`, markdown(md));
  } catch (e) { errorToast(e); }
}

async function downloadResult(job) {
  try {
    const url = URL.createObjectURL(new Blob([await fetchResult(job)], { type: 'text/markdown' }));
    const a = h('a', { href: url, download: `job-${job.id}.md` });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  } catch (e) { errorToast(e); }
}

async function action(job, what) {
  if (what === 'delete' && !(await confirmDialog({ title: `Delete job ${job.title}?`, body: 'Its documents are deleted too.', confirm: 'Delete', danger: true }))) return;
  try {
    if (what === 'delete') {
      await api('DELETE', base(job.id));
      selected = '';
    } else {
      await api('POST', base(job.id) + '/cancel');
    }
    toast(`${job.title}: ${what === 'delete' ? 'deleted' : 'cancelled'}.`);
  } catch (e) { quiet(e); }
  refreshNow();
}

function jobDetail(job, messageBox) {
  const tasks = (job.tasks || []).map((t) => [String(t.id), t.status === 'done' ? badge('done', 'ok') : badge('todo', 'idle'), t.title,
    t.doc ? h('span.mono', null, t.doc) : '–']);
  const docs = (job.docs || []).map((d) => [
    h('button.small', { type: 'button', onclick: () => showDoc(job, d.name), 'data-focus-key': 'doc:' + d.name }, d.name),
    d.author, ago(d.updated), h('span.muted', null, d.text)]);
  const events = (job.events || []).slice(-60).reverse().map((e) => [ago(e.time), badge(e.kind, e.kind === 'error' ? 'bad' : 'info'),
    h('span', { style: 'white-space:pre-wrap' }, e.text)]);
  const busy = job.status === 'running' || job.status === 'queued';
  return h('div', null,
    h('div.card.section', null,
      h('div.row', null, h('h2', null, job.title), statusBadge(job.status), h('span.spacer'),
        h('button.small.primary', { type: 'button', onclick: () => previewResult(job), 'data-focus-key': 'job:preview' }, 'Preview result'),
        h('button.small', { type: 'button', onclick: () => downloadResult(job), 'data-focus-key': 'job:result' }, 'Download .md'),
        busy ? h('button.small', { type: 'button', onclick: () => action(job, 'cancel'), 'data-focus-key': 'job:cancel' }, 'Cancel') : null,
        h('button.small.danger', { type: 'button', onclick: () => action(job, 'delete'), 'data-focus-key': 'job:delete' }, 'Delete')),
      job.question ? h('p', null, badge('waiting', 'warn'), ' ', job.question) : null,
      job.error ? h('p', null, badge('error', 'bad'), ' ', job.error) : null,
      job.summary ? h('p', null, job.summary) : null,
      h('dl.kv', null,
        h('dt', null, 'Goal'), h('dd', { style: 'white-space:pre-wrap' }, job.goal),
        (job.messages || []).length ? [h('dt', null, 'Your messages'), h('dd', null, job.messages.map((m) => h('div', null, '• ' + m)))] : null,
        h('dt', null, 'Steps'), h('dd', null, String(job.steps)),
        h('dt', null, 'Updated'), h('dd', null, ago(job.updated))),
      messageBox),
    h('div.card.section', null, h('h3', null, 'Tasks'), table(['#', 'Status', 'Task', 'Document'], tasks, 'No plan yet.')),
    h('div.card.section', null, h('h3', null, 'Documents'), table(['Name', 'Author', 'Updated', 'Start'], docs, 'No documents yet.')),
    h('div.card.section', null, h('h3', null, 'Progress'), table(['When', 'Kind', 'What'], events, 'Nothing yet.')));
}

export default function jobsView() {
  const newBtn = h('button.primary', { type: 'button', onclick: () => newJobDialog() }, 'New job');
  const listEl = h('div', null, h('p.muted', null, 'Loading…'));
  const detailEl = h('div');
  const text = h('textarea', { rows: 2, placeholder: 'Instruction for this job, e.g. "rozdziały krótsze" or "continue"', 'aria-label': 'Message to the job' });
  const send = h('button.primary', { type: 'submit' }, 'Send');
  const messageBox = h('form.row', { onsubmit: async (e) => {
    e.preventDefault();
    if (!text.value.trim() || !selected) return;
    send.disabled = true;
    try {
      await api('POST', base(selected) + '/messages', { text: text.value.trim() });
      text.value = '';
      toast('Sent; the orchestrator sees it in its next step.');
    } catch (ex) { quiet(ex); } finally { send.disabled = false; }
    refreshNow();
  } }, h('div', { class: 'grow' }, text), send);
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Jobs'), newBtn),
    h('p.muted', null, 'Long, multi-step work (e.g. a story in many chapters) that the Super Borg orchestrator runs in the background: ' +
      'it plans tasks, keeps every result as a document and delegates parts to the other phones. A Super Borg chat request ' +
      'that needs this starts a job by itself.'),
    listEl, detailEl);

  async function refresh() {
    const res = await getOptional('/admin/jobs');
    self.live = res !== null;
    if (res === null) {
      newBtn.hidden = true;
      fill(listEl, unavailable('Jobs', '/admin/jobs', refreshNow));
      fill(detailEl);
      return;
    }
    const jobs = res.jobs || [];
    if (!jobs.some((j) => j.id === selected)) selected = jobs.length ? jobs[0].id : '';
    fill(listEl, h('div.card.section', null, table(['Job', 'Status', 'Tasks', 'Steps', 'Updated'], jobs.map((j) => [
      h('button.small', { type: 'button', 'aria-pressed': String(j.id === selected), 'data-focus-key': 'sel:' + j.id,
        onclick: () => { selected = j.id; refreshNow(); } }, j.title),
      statusBadge(j.status), `${j.tasks_done}/${j.tasks}`, String(j.steps), ago(j.updated)]), 'No jobs yet. Start one with New job.')));
    if (!selected) {
      fill(detailEl);
      return;
    }
    fill(detailEl, jobDetail(await get(base(selected)), messageBox));
  }

  const self = { title: 'Jobs', el, live: true, refresh };
  return self;
}
