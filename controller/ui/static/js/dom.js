// DOM building and number formatting. Server-provided strings only ever
// become text nodes or attribute values (never HTML), so they need no escaping.

const PROPS = new Set(['value', 'checked', 'disabled', 'selected', 'required', 'multiple', 'indeterminate', 'min', 'max', 'step']);

// h('tag.class', {attr: v, onclick: fn}, ...children) creates an element.
export function h(tag, props, ...kids) {
  const [name, ...classes] = tag.split('.');
  const el = document.createElement(name);
  if (classes.length) el.className = classes.join(' ');
  append(el, kids);
  // Properties after children, so a <select>'s value matches an option.
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = [el.className, v].filter(Boolean).join(' ');
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (PROPS.has(k)) el[k] = v;
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  return el;
}

// fill replaces el's children; like append, it flattens arrays and skips null.
export function fill(el, ...kids) {
  el.replaceChildren();
  return append(el, kids);
}

export function append(el, kids) {
  for (const k of kids.flat(Infinity)) {
    if (k == null || k === false) continue;
    el.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
  return el;
}

// table(['Name', {label: 'Req', num: true}], rows, emptyText) where each row
// is an array of cells (string, Node, <td>, or {v, num, class}) or a <tr>.
export function table(cols, rows, emptyText = 'Nothing here yet.') {
  const head = h('tr', null, cols.map((c) => {
    const col = typeof c === 'string' ? { label: c } : c;
    return h('th', { scope: 'col', class: col.num ? 'num' : null, title: col.title }, col.label);
  }));
  const body = rows.length
    ? rows.map((r) => (r instanceof Node ? r : h('tr', null, r.map((cell, i) => td(cell, cols[i])))))
    : h('tr', null, h('td.empty', { colspan: cols.length }, emptyText));
  return h('div.table-wrap', null, h('table', null, h('thead', null, head), h('tbody', null, body)));
}

export function td(cell, col) {
  if (cell instanceof HTMLTableCellElement) return cell;
  const num = typeof col === 'object' && col.num;
  if (cell && typeof cell === 'object' && !(cell instanceof Node) && !Array.isArray(cell)) {
    return h('td', { class: [num || cell.num ? 'num' : '', cell.class || ''].join(' ').trim() || null }, cell.v);
  }
  return h('td', { class: num ? 'num' : null }, cell);
}

export const badge = (text, cls = text) => h('span.badge', { class: 'st-' + cls }, text);

// ---- formatting ----
const GiB = 1024 ** 3;
const MiB = 1024 ** 2;
const intFmt = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });

export function bytes(b) {
  if (b == null || !Number.isFinite(b) || b <= 0) return '–';
  if (b >= GiB) return (b / GiB).toFixed(1) + ' GiB';
  return Math.round(b / MiB) + ' MiB';
}
export const int = (n) => (n == null || !Number.isFinite(n) ? '–' : intFmt.format(n));
export const tps = (n) => (n > 0 ? n.toFixed(1) : '–');
export const pct = (part, whole) => (whole > 0 ? Math.round((100 * part) / whole) + '%' : '–');

export function duration(sec) {
  if (!(sec >= 0)) return '–';
  if (sec < 60) return Math.round(sec) + ' s';
  if (sec < 3600) return Math.round(sec / 60) + ' min';
  if (sec < 86400) return (sec / 3600).toFixed(1) + ' h';
  return (sec / 86400).toFixed(1) + ' d';
}

// A timestamp as "12 s ago"; the absolute time goes in a tooltip.
export function ago(ts) {
  const t = ts ? Date.parse(ts) : NaN;
  if (!Number.isFinite(t) || t <= 0 || new Date(t).getUTCFullYear() < 2000) return h('span.muted', null, 'never');
  const s = Math.max(0, (Date.now() - t) / 1000);
  return h('span.nowrap', { title: new Date(t).toLocaleString() }, s < 2 ? 'just now' : duration(s) + ' ago');
}

export const sum = (obj, field) => Object.values(obj || {}).reduce((a, c) => a + (c[field] || 0), 0);

// Device classes by total RAM, as the controller computes them (model management API).
export const CLASSES = [
  { id: 'xs', label: '< 3 GiB', max: 3 * GiB },
  { id: 's', label: '3–5 GiB', max: 5 * GiB },
  { id: 'm', label: '5–7 GiB', max: 7 * GiB },
  { id: 'l', label: '7–10 GiB', max: 10 * GiB },
  { id: 'xl', label: '10 GiB+', max: Infinity },
];
export const classOf = (ramBytes) => (ramBytes > 0 ? CLASSES.find((c) => ramBytes < c.max).id : '');

// Performance tiers by measured generation bandwidth (ADR-015), as the
// controller computes them (model management API).
export const TIERS = [
  { id: 't1', label: '< 4 GB/s' },
  { id: 't2', label: '4–10 GB/s' },
  { id: 't3', label: '10–25 GB/s' },
  { id: 't4', label: '25 GB/s+' },
];

export function sparkline(values) {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 100 28');
  svg.setAttribute('preserveAspectRatio', 'none');
  svg.setAttribute('aria-hidden', 'true');
  if (values.length > 1) {
    const max = Math.max(...values, 1e-9);
    const pts = values.map((v, i) => `${((100 * i) / (values.length - 1)).toFixed(1)},${(26 - (24 * v) / max).toFixed(1)}`);
    const line = document.createElementNS(NS, 'polyline');
    line.setAttribute('points', pts.join(' '));
    line.setAttribute('class', 'spark');
    line.setAttribute('vector-effect', 'non-scaling-stroke');
    svg.append(line);
  }
  return svg;
}
