// graph.js — render the rule table as a directed graph (DAG) in SVG.
//
// Rules are `from → (action) → to` edges evaluated first-match-per-position, so
// the table is a decision graph whose nodes are positions/handlers. Multiple
// rules between the same pair are merged into one wire (labelled ×N, hover shows
// them all). Self-loops (pure transform rules such as an alias returning to
// `http`) and back edges are drawn as curves; everything else is layered
// left-to-right. The SVG scales to fill and centre in its pane; hovering a wire
// or node shows the full detail in a tooltip.

const TERMINALS = new Set(['respond', 'error']);

const SVG = 'http://www.w3.org/2000/svg';
function s(tag, attrs = {}, ...kids) {
  const node = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'text') node.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2).toLowerCase(), v);
    else if (v != null) node.setAttribute(k, String(v));
  }
  for (const kid of kids.flat()) if (kid != null) node.append(kid);
  return node;
}

// merge rules that share the same from→to into one wire
function toEdges(rules) {
  const map = new Map();
  for (const r of rules) {
    const from = r.from || '(any)';
    const to = r.to || (r.action || '(end)');
    const key = from + '\u0000' + to;
    if (!map.has(key)) map.set(key, { from, to, rules: [] });
    map.get(key).rules.push(r);
  }
  const edges = [];
  for (const e of map.values()) {
    const actions = [...new Set(e.rules.map((r) => (r.to ? (r.action || 'set') : '')).filter(Boolean))];
    e.label = actions.join(' / ');
    e.count = e.rules.length;
    edges.push(e);
  }
  return edges;
}

function condText(match) {
  if (!Array.isArray(match) || !match.length) return '';
  return match.map((c) => {
    const sym = opSym(c.op);
    const v = c.values ? c.values.join('|') : c.value;
    if (c.op === 'empty' || c.op === 'exists' || v == null) return `${c.field}${sym}`;
    return `${c.field}${sym}${v}`;
  }).join(' & ');
}
function opSym(op) {
  return { eq: '=', neq: '!=', in: '∈', prefix: '^', regex: '~', exists: '?', empty: '∅' }[op] || (' ' + op + ' ');
}

function nodeKind(id, hasOut) {
  if (id === '@ingress') return 'entry';
  if (TERMINALS.has(id)) return 'terminal';
  if (!hasOut) return 'sink';
  return 'position';
}

// layered layout: longest-path over the DAG with back edges removed
function layout(edges) {
  const nodes = new Set();
  for (const e of edges) { nodes.add(e.from); nodes.add(e.to); }
  const adj = new Map([...nodes].map((n) => [n, []]));
  for (const e of edges) if (e.from !== e.to) adj.get(e.from).push(e.to);

  const color = new Map([...nodes].map((n) => [n, 0]));
  const back = new Set();
  const visit = (u) => {
    color.set(u, 1);
    for (const v of adj.get(u)) {
      if (color.get(v) === 1) back.add(u + '\u0000' + v);
      else if (color.get(v) === 0) visit(v);
    }
    color.set(u, 2);
  };
  for (const n of nodes) if (color.get(n) === 0) visit(n);

  const dag = new Map([...nodes].map((n) => [n, []]));
  const indeg = new Map([...nodes].map((n) => [n, 0]));
  for (const e of edges) {
    if (e.from === e.to || back.has(e.from + '\u0000' + e.to)) continue;
    dag.get(e.from).push(e.to);
    indeg.set(e.to, indeg.get(e.to) + 1);
  }
  const q = [...nodes].filter((n) => indeg.get(n) === 0);
  const order = [];
  const deg = new Map(indeg);
  while (q.length) {
    const u = q.shift(); order.push(u);
    for (const v of dag.get(u)) { deg.set(v, deg.get(v) - 1); if (deg.get(v) === 0) q.push(v); }
  }
  for (const n of nodes) if (!order.includes(n)) order.push(n);

  const layer = new Map([...nodes].map((n) => [n, 0]));
  for (const u of order) for (const v of dag.get(u)) layer.set(v, Math.max(layer.get(v), layer.get(u) + 1));
  return { nodes: [...nodes], layer, back };
}

const COL = 210, ROW = 78, W = 136, H = 44, PAD = 30;

export function renderRuleGraph(rules, { onPick } = {}) {
  const edges = toEdges(rules);
  const { nodes, layer, back } = layout(edges);

  const byLayer = new Map();
  for (const n of nodes) {
    const l = layer.get(n) || 0;
    if (!byLayer.has(l)) byLayer.set(l, []);
    byLayer.get(l).push(n);
  }
  const pos = new Map();
  const rows = Math.max(1, ...[...byLayer.values()].map((a) => a.length));
  for (const [l, arr] of byLayer) {
    const offset = (rows - arr.length) / 2;
    arr.forEach((n, i) => pos.set(n, { x: PAD + l * COL, y: PAD + (i + offset) * ROW }));
  }

  const maxLayer = Math.max(0, ...[...byLayer.keys()]);
  const width = PAD * 2 + (maxLayer + 1) * COL;
  const height = PAD * 2 + rows * ROW + 30;

  const svg = s('svg', { class: 'dag', viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'xMidYMid meet' });
  const defs = s('defs');
  for (const [id, color] of [['arrow', '#8a93a0'], ['arrow-back', '#c08a3e']]) {
    defs.append(s('marker', {
      id, markerWidth: 10, markerHeight: 10, refX: 9, refY: 3, orient: 'auto', markerUnits: 'userSpaceOnUse',
    }, s('path', { d: 'M0,0 L9,3 L0,6 Z', fill: color })));
  }
  svg.append(defs);

  const edgeG = s('g', { class: 'edges' });
  const nodeG = s('g', { class: 'nodes' });
  svg.append(edgeG, nodeG);

  // floating tooltip (shows the full, untruncated detail on hover)
  const tip = document.createElement('div');
  tip.className = 'dagtip';
  tip.hidden = true;

  for (const e of edges) drawEdge(e);
  for (const n of nodes) {
    const p = pos.get(n);
    const hasOut = edges.some((edge) => edge.from === n);
    const kind = nodeKind(n, hasOut);
    const label = n === '(any)' ? '(any position)' : n;
    nodeG.append(s('g', {
      class: 'node ' + kind, transform: `translate(${p.x},${p.y})`,
      onmouseenter: (ev) => showTip(ev, nodeHint(n)),
      onmousemove: moveTip,
      onmouseleave: hideTip,
    },
    s('rect', { width: W, height: H, rx: 9 }),
    s('text', { x: W / 2, y: H / 2 + 4, 'text-anchor': 'middle', text: clip(label, 18) })));
  }

  function drawEdge(e) {
    const a = pos.get(e.from), b = pos.get(e.to);
    if (!a || !b) return;
    const self = e.from === e.to;
    const isBack = back.has(e.from + '\u0000' + e.to) || (b.x < a.x && !self);
    let d, lx, ly;
    if (self) {
      const x = a.x + W / 2, y = a.y;
      d = `M ${x - 20} ${y} C ${x - 20} ${y - 34}, ${x + 20} ${y - 34}, ${x + 20} ${y}`;
      lx = x; ly = y - 26;
    } else if (isBack) {
      const sx = a.x + W / 2, sy = a.y + H, tx = b.x + W / 2, ty = b.y + H;
      const dip = 40;
      d = `M ${sx} ${sy} C ${sx} ${sy + dip}, ${tx} ${ty + dip}, ${tx} ${ty}`;
      lx = (sx + tx) / 2; ly = ((sy + ty) / 2) + dip * 0.75;
    } else {
      const sx = a.x + W, sy = a.y + H / 2, tx = b.x, ty = b.y + H / 2;
      const mx = (sx + tx) / 2;
      d = `M ${sx} ${sy} C ${mx} ${sy}, ${mx} ${ty}, ${tx} ${ty}`;
      lx = mx; ly = (sy + ty) / 2 - 6;
    }
    const g = s('g', {
      class: 'edge' + (e.rules.every((r) => r.generated) ? ' generated' : '')
        + (isBack ? ' back' : '') + (self ? ' self' : ''),
      'data-from': e.from,
      'data-to': e.to,
      onclick: () => onPick && onPick(e.rules),
      onmouseenter: (ev) => showTip(ev, edgeHint(e)),
      onmousemove: moveTip,
      onmouseleave: hideTip,
    });
    g.append(s('path', {
      d, fill: 'none', stroke: isBack ? '#c08a3e' : '#8a93a0', 'stroke-width': 1.7,
      'marker-end': `url(#${isBack ? 'arrow-back' : 'arrow'})`,
    }));
    const label = edgeLabel(e);
    if (label) {
      g.append(s('text', {
        x: lx, y: ly, 'text-anchor': 'middle', class: 'elabel', text: clip(label, 30),
        onmouseenter: (ev) => showTip(ev, edgeHint(e)),
        onmousemove: moveTip,
        onmouseleave: hideTip,
        onclick: () => onPick && onPick(e.rules),
      }));
    }
    edgeG.append(g);
  }

  const legend = document.createElement('div');
  legend.className = 'dag-legend';
  legend.innerHTML = '<span class="dot entry"></span>entry <span class="dot position"></span>position '
    + '<span class="dot sink"></span>sink/provider <span class="dot terminal"></span>terminal '
    + '<span class="rel"></span>back/self edge <span class="hint">· ×N = merged wires · hover for detail · click a wire for its rules</span>';
  const wrap = document.createElement('div');
  wrap.className = 'dagwrap';
  wrap.append(legend, tip, svg);

  function showTip(ev, text) {
    tip.textContent = text;
    tip.hidden = false;
    moveTip(ev);
  }
  function moveTip(ev) {
    if (tip.hidden) return;
    const rect = wrap.getBoundingClientRect();
    let x = ev.clientX - rect.left + 14;
    let y = ev.clientY - rect.top + 14;
    const maxX = rect.width - tip.offsetWidth - 8;
    const maxY = rect.height - tip.offsetHeight - 8;
    if (x > maxX) x = Math.max(4, maxX);
    if (y > maxY) y = Math.max(4, maxY);
    tip.style.left = x + 'px';
    tip.style.top = y + 'px';
  }
  function hideTip() { tip.hidden = true; }

  return wrap;
}

function edgeLabel(e) {
  if (e.count === 1) {
    const r = e.rules[0];
    const base = r.to ? (r.action || 'set') : '';
    const cond = condText(r.match);
    return (base + (cond ? `  [${cond}]` : '')) || '';
  }
  return (e.label ? e.label + ' ' : '') + '×' + e.count;
}

function edgeHint(e) {
  const head = `${e.from} → ${e.to}  (${e.count} rule${e.count > 1 ? 's' : ''})`;
  const lines = e.rules.map((r) => {
    const cond = condText(r.match);
    const act = r.to ? (r.action || 'set') : '(terminal)';
    return `  ${(r.id || '(generated)').padEnd(18)} ${act}${cond ? '  [' + cond + ']' : ''}`;
  });
  return head + '\n' + lines.join('\n');
}

function clip(text, n) { return text.length > n ? text.slice(0, n - 1) + '…' : text; }

function nodeHint(id) {
  switch (id) {
    case '@ingress': return 'ingress: entry position for every request';
    case '(any)': return 'fallback: matches when the current position has no other matching rule (from: "")';
    case 'respond': return 'terminal: return the current packet as the response';
    case 'error': return 'terminal: fail the request';
    case 'stream': return 'streaming handler (selected by providers via serve(pkt, "stream"))';
    default: return id;
  }
}
