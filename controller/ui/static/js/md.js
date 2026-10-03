// A small Markdown renderer for job documents and results: headings, lists,
// quotes, code blocks, rules, paragraphs and **bold**, *italic*, `code`.
// It builds DOM nodes with text content only (never innerHTML), so model
// output cannot inject markup. The panel loads nothing from the internet,
// hence no library.
import { h } from './dom.js';

const INLINE = /(\*\*[^*]+\*\*|__[^_]+__|\*[^*\s][^*]*\*|_[^_\s][^_]*_|`[^`]+`)/;

function inline(text) {
  return text.split(INLINE).filter(Boolean).map((part) => {
    if (/^(\*\*|__).+\1$/.test(part)) return h('strong', null, part.slice(2, -2));
    if (/^`.+`$/.test(part)) return h('code', null, part.slice(1, -1));
    if (/^([*_]).+\1$/.test(part)) return h('em', null, part.slice(1, -1));
    return part;
  });
}

export function markdown(src) {
  const out = h('div.md');
  const lines = String(src || '').replace(/\r\n?/g, '\n').split('\n');
  let para = [];
  let list = null;
  const flushPara = () => {
    if (para.length) out.append(h('p', null, inline(para.join(' '))));
    para = [];
  };
  const flushList = () => {
    if (list) out.append(list);
    list = null;
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const t = line.trim();
    let m;
    if (t.startsWith('```')) {
      flushPara(); flushList();
      const code = [];
      for (i++; i < lines.length && !lines[i].trim().startsWith('```'); i++) code.push(lines[i]);
      out.append(h('pre', null, h('code', null, code.join('\n'))));
    } else if (!t) {
      flushPara(); flushList();
    } else if ((m = /^(#{1,6})\s+(.*)$/.exec(t))) {
      flushPara(); flushList();
      out.append(h('h' + Math.min(m[1].length + 1, 6), null, inline(m[2])));
    } else if (/^(-{3,}|\*{3,}|_{3,})$/.test(t)) {
      flushPara(); flushList();
      out.append(h('hr'));
    } else if ((m = /^[-*+]\s+(.*)$/.exec(t)) || (m = /^\d+[.)]\s+(.*)$/.exec(t))) {
      flushPara();
      const tag = /^\d/.test(t) ? 'ol' : 'ul';
      if (!list || list.tagName.toLowerCase() !== tag) { flushList(); list = h(tag); }
      list.append(h('li', null, inline(m[1])));
    } else if ((m = /^>\s?(.*)$/.exec(t))) {
      flushPara(); flushList();
      out.append(h('blockquote', null, inline(m[1])));
    } else {
      flushList();
      para.push(t);
    }
  }
  flushPara(); flushList();
  return out;
}
