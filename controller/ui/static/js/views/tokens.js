// Tokens: the token analyzer (ADR-032). The tokenizer counts a text's tokens
// with every model the phones serve, shows how much of each phone's context
// it takes and how long the phone needs to read it, and splits it into
// tokens for one model. Below it, token usage analytics from Grafana.
import { api } from '../api.js';
import { h, fill, int } from '../dom.js';
import { errorToast } from '../ui.js';
import { grafana, dashboardURL, dashboardFrame } from '../grafana.js';

const SHOW_PIECES = 5000; // pieces drawn; the rest are only counted
let lastText = ''; // kept across view re-creation
let pieceModel = '';

function readTime(tokens, tps) {
  if (!tps) return '–';
  const s = tokens / tps;
  return s < 60 ? `≈ ${Math.max(1, Math.round(s))} s` : `≈ ${(s / 60).toFixed(1)} min`;
}

function ctxCell(tokens, ctx) {
  if (!ctx) return '–';
  const pct = (100 * tokens) / ctx;
  return [h('meter', { min: 0, max: 100, low: 50, high: 80, optimum: 0, value: Math.min(pct, 100), title: `${pct.toFixed(1)} % of ${int(ctx)}` }),
    ` ${pct < 0.1 ? '<0.1' : pct.toFixed(1)} %`];
}

function piecesView(res) {
  const shown = res.pieces.slice(0, SHOW_PIECES);
  const spans = shown.map((p, i) => {
    const text = p.piece.replace(/\n/g, '↵\n').replace(/\t/g, '⇥');
    return h('span', { class: 'tok tok' + (i % 4), title: `#${i + 1} · id ${p.id}` }, text === '' ? '∅' : text);
  });
  const more = res.pieces.length - shown.length + (res.truncated ? 1 : 0);
  return [h('div.tok-text', null, spans),
    more > 0 ? h('p.muted.small', null, `Showing the first ${int(shown.length)} tokens.`) : null];
}

export default function tokensView() {
  const text = h('textarea.tok-input', { rows: 8, placeholder: 'Paste a prompt, a document or code…', spellcheck: 'false', 'aria-label': 'Text to tokenize' });
  text.value = lastText;
  const result = h('div');
  const usage = h('div');
  const btn = h('button.primary', { type: 'submit' }, 'Count tokens');
  const form = h('form.card.section', null,
    h('h2', null, 'Tokenizer'),
    h('p.muted.small', null, 'Counted by the phones themselves (llama-server), one phone per served model. Up to 256 KiB.'),
    text, h('div.row', null, btn, h('span.muted.small', null, 'Click a model below to see its tokens.')), result);
  const el = h('section', null, h('div.page-head', null, h('h1', null, 'Tokens')), form,
    h('h2.section-title', null, 'Token usage'), usage);

  async function run() {
    lastText = text.value;
    if (!lastText) return;
    btn.disabled = true;
    try {
      const res = await api('POST', '/admin/tokenize', { text: lastText, model: pieceModel || undefined });
      pieceModel = res.pieces_model || pieceModel;
      const rows = res.models.map((m) => h('tr', { class: m.model === res.pieces_model ? 'selected' : null },
        h('td', null, h('button.link', { type: 'button', onclick: () => { pieceModel = m.model; run(); } }, m.model)),
        h('td.mono', null, m.node_id),
        h('td.num', null, m.error ? h('span.bad-text', { title: m.error }, 'error') : int(m.tokens)),
        h('td.num', null, m.tokens ? (res.chars / m.tokens).toFixed(2) : '–'),
        h('td', null, m.error ? '–' : ctxCell(m.tokens, m.ctx_size)),
        h('td.num', { title: m.prompt_tps ? `self-test prompt speed ${m.prompt_tps.toFixed(1)} tok/s; long prompts are slower` : '' },
          m.error ? '–' : readTime(m.tokens, m.prompt_tps))));
      fill(result,
        h('p.muted.small', null, `${int(res.chars)} characters, ${int(res.bytes)} bytes.`),
        h('div.table-wrap', null, h('table', null,
          h('thead', null, h('tr', null, ['Model', 'Phone', 'Tokens', 'Chars / token', 'Share of context', 'Time to read'].map((c, i) =>
            h('th', { class: i >= 2 && i !== 4 ? 'num' : null }, c)))),
          h('tbody', null, rows))),
        res.pieces ? [h('h3', null, `Tokens of ${res.pieces_model}`), piecesView(res)] : null);
    } catch (e) {
      errorToast(e);
    } finally {
      btn.disabled = false;
    }
  }
  form.addEventListener('submit', (e) => { e.preventDefault(); pieceModel = ''; run(); });

  async function refresh() {
    if (usage.childElementCount) return;
    const g = await grafana();
    fill(usage, g
      ? dashboardFrame(dashboardURL(g, 'phoneborg-tokens', { from: 'now-24h' }), 'Token usage dashboard')
      : h('p.muted', null, 'Token usage charts need Grafana in the panel (-grafana-url).'));
  }

  return { title: 'Tokens', el, live: false, refresh };
}
