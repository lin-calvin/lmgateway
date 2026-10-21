// app.js — console shell: nav, list views, and a slide-over drawer for
// detail/actions. All operations go through api.js, which mirrors the CLI.

import { Client, HTTPError, listAliases, aliasAssignments, aliasRuleBody, aliasRuleID, aliasRulePrefix } from './api.js';
import { RESOURCES, SETTINGS, SETTING_NAMES, META_KEYS } from './schema.js';
import { renderRuleGraph } from './graph.js';

// ---- tiny DOM helper ----
function h(tag, props = {}, ...kids) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k === 'html') node.innerHTML = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2).toLowerCase(), v);
    else if (v === true) node.setAttribute(k, '');
    else if (v !== false && v != null) node.setAttribute(k, v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    node.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return node;
}
const $ = (sel, root = document) => root.querySelector(sel);

const state = {
  client: null,
  view: 'dashboard',
  arg: null,
  filters: {},
  ruleMode: 'table',
};

// ---- auth bar ----
function loadAuth() {
  $('#server').value = localStorage.getItem('lmgateway.server') || '';
  $('#apiKey').value = localStorage.getItem('lmgateway.apiKey') || '';
  $('#authHeader').value = localStorage.getItem('lmgateway.authHeader') || 'Authorization';
}
function buildClient() {
  state.client = new Client({
    server: $('#server').value.trim(),
    apiKey: $('#apiKey').value.trim(),
    authHeader: $('#authHeader').value,
  });
}
function saveAuth() {
  localStorage.setItem('lmgateway.server', $('#server').value.trim());
  localStorage.setItem('lmgateway.apiKey', $('#apiKey').value.trim());
  localStorage.setItem('lmgateway.authHeader', $('#authHeader').value);
  buildClient();
  toast('Saved', 'ok');
  checkAuth();
}
async function checkAuth() {
  const dot = $('#authStatus');
  dot.className = 'dot';
  try {
    await state.client.health();
    dot.className = 'dot ok';
    dot.title = 'reachable';
  } catch (error) {
    dot.className = 'dot bad';
    dot.title = String(error.message || error);
  }
}

// ---- toasts ----
function toast(message, kind) {
  const node = h('div', { class: 'toast' + (kind ? ' ' + kind : '') }, message);
  $('#toasts').append(node);
  setTimeout(() => node.remove(), kind === 'error' ? 9000 : 3500);
}
function toastError(error) {
  const node = h('div', { class: 'toast error' },
    h('div', { text: String(error.message || error) }),
    error instanceof HTTPError && error.body != null
      ? h('pre', { text: JSON.stringify(error.body, null, 2) }) : null);
  $('#toasts').append(node);
  setTimeout(() => node.remove(), 9000);
}

// ---- JSON helpers ----
function stable(v) {
  if (Array.isArray(v)) return '[' + v.map(stable).join(',') + ']';
  if (v && typeof v === 'object') {
    return '{' + Object.keys(v).sort().map(k => JSON.stringify(k) + ':' + stable(v[k])).join(',') + '}';
  }
  return JSON.stringify(v === undefined ? null : v);
}
const deepEqual = (a, b) => stable(a) === stable(b);
function stripMeta(obj) {
  const out = { ...(obj || {}) };
  for (const k of META_KEYS) delete out[k];
  return out;
}
function diffDoc(next, prev) {
  const patch = {};
  for (const k of new Set([...Object.keys(next), ...Object.keys(prev)])) {
    if (!deepEqual(next[k], prev[k])) patch[k] = k in next ? next[k] : null;
  }
  return patch;
}

// ---- enumerations for select / checklist / datalist fields ----
const enumCache = new Map(); // source -> {at, val}
async function loadEnum(client, source) {
  const hit = enumCache.get(source);
  if (hit && Date.now() - hit.at < 15000) return hit.val;
  const val = await fetchEnum(client, source);
  enumCache.set(source, { at: Date.now(), val });
  return val;
}
async function fetchEnum(client, source) {
  switch (source) {
    case 'providers':
      return (await client.resource('provider', 'list')).items?.map((i) => i.name) || [];
    case 'models': {
      const [configured, discovered] = await Promise.all([
        client.resource('model', 'list').then((r) => (r.items || []).map((i) => i.name)).catch(() => []),
        client.get('v1/models').then((r) => (r.data || []).map((m) => m.id)).catch(() => []),
      ]);
      return [...new Set([...configured, ...discovered])].sort();
    }
    case 'pools':
      return (await client.resource('pool', 'list')).items?.map((i) => i.model) || [];
    case 'targets': {
      const [pools, models] = await Promise.all([fetchEnum(client, 'pools'), fetchEnum(client, 'models')]);
      return [...new Set([...pools, ...models])].sort();
    }
    case 'groups': {
      const items = (await client.resource('key', 'list')).items || [];
      const set = new Set();
      for (const k of items) { if (k.id) set.add(k.id); for (const g of k.groups || []) set.add(g); }
      return [...set].sort();
    }
    case 'positions': {
      const rules = (await client.rule('list')).rules || [];
      const set = new Set();
      for (const r of rules) { if (r.from) set.add(r.from); if (r.to) set.add(r.to); }
      return [...set].sort();
    }
    case 'actions': {
      const rules = (await client.rule('list')).rules || [];
      const meta = await client.rule('meta').catch(() => ({ terminals: [] }));
      const set = new Set();
      for (const r of rules) if (r.action) set.add(r.action);
      for (const t of meta.terminals || []) set.add(t);
      return [...set].sort();
    }
    default:
      return [];
  }
}
function invalidateEnums() { enumCache.clear(); }

// ---- nav ----
const ACL_FIELD_NAMES = ['public', 'allowed_groups', 'denied_groups', 'groups'];
const NAV = [
  { items: [{ id: 'dashboard', label: 'Dashboard', icon: '▦' }] },
  { group: 'Configuration', items: [
    { id: 'provider', label: 'Providers', icon: '◧' },
    { id: 'model', label: 'Models', icon: '◇' },
    { id: 'pool', label: 'Pools', icon: '≣' },
    { id: 'key', label: 'Keys', icon: '⚿' },
    { id: 'rule', label: 'Rules', icon: '⑃' },
    { id: 'alias', label: 'Aliases', icon: '→' },
    { id: 'setting', label: 'Settings', icon: '⚙' },
  ] },
  { group: 'Coding plans', items: [{ id: 'codex', label: 'Codex login', icon: '⌘' }] },
  { group: 'Operations', items: [
    { id: 'actions', label: 'Actions', icon: '⚡' },
    { id: 'spend', label: 'Spend / TS', icon: 'Σ' },
  ] },
];
function renderNav() {
  const nav = $('#nav');
  nav.replaceChildren();
  for (const section of NAV) {
    if (section.group) nav.append(h('div', { class: 'group', text: section.group }));
    for (const item of section.items) {
      nav.append(h('button', {
        class: item.id === state.view ? 'active' : '',
        'data-view': item.id,
        onclick: () => { location.hash = '#/' + item.id; },
      }, h('span', { class: 'icon', text: item.icon }), h('span', { text: item.label })));
    }
  }
}

// ---- router ----
function route() {
  closeDrawer();
  const raw = location.hash.replace(/^#\/?/, '');
  const [view, ...rest] = raw.split('/');
  state.view = view || 'dashboard';
  state.arg = rest.length ? decodeURIComponent(rest.join('/')) : null;
  renderNav();
  const render = RENDERERS[state.view] || RENDERERS.dashboard;
  render(state.arg);
}

// ---- drawer ----
function openDrawer() {
  $('#drawer').removeAttribute('hidden');
  $('#drawer').classList.add('open');
  $('#scrim').removeAttribute('hidden');
}
function closeDrawer() {
  $('#drawer').classList.remove('open');
  $('#scrim').setAttribute('hidden', '');
}
function showDrawer({ title, meta, body, actions }) {
  const drawer = $('#drawer');
  drawer.replaceChildren(
    h('div', { class: 'dhead' },
      h('div', {},
        h('h2', { text: title }),
        meta ? h('div', { class: 'meta' }, meta) : null),
      h('button', { class: 'btn ghost small close', onclick: closeDrawer, text: 'Close' })),
    h('div', { class: 'dbody' }, body),
    actions && actions.length ? h('div', { class: 'dactions' }, actions) : null,
  );
  openDrawer();
}

// ---- pills ----
function pill(text, cls) { return h('span', { class: 'pill' + (cls ? ' ' + cls : ''), text: String(text) }); }
function cellText(v) {
  if (v == null) return '—';
  if (Array.isArray(v)) return v.join(', ') || '—';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

// ---- generic list view ----
const LIST_SOURCES = {
  provider: { load: (c) => c.resource('provider', 'list').then(r => r.items || []) },
  model: { load: (c) => c.resource('model', 'list').then(r => r.items || []) },
  pool: { load: (c) => c.resource('pool', 'list').then(r => r.items || []) },
  key: { load: (c) => c.resource('key', 'list').then(r => r.items || []) },
  rule: { load: (c) => c.rule('list').then(r => r.rules || []) },
};

function renderResourceView(kind, arg) {
  const res = RESOURCES[kind];
  $('#viewTitle').textContent = res.title;
  const toolbar = $('#toolbar');
  const toolbarButtons = [h('button', {
    class: 'btn', text: res.createLabel || 'New',
    onclick: () => openResource(kind, null, { isNew: true }),
  })];
  if (kind === 'rule') {
    toolbarButtons.push(h('button', {
      class: 'btn ghost', text: state.ruleMode === 'graph' ? 'Table view' : 'Graph view',
      onclick: () => { state.ruleMode = state.ruleMode === 'graph' ? 'table' : 'graph'; renderResourceView(kind, state.arg); },
    }));
  }
  toolbarButtons.push(h('button', { class: 'btn secondary', text: 'Refresh', onclick: () => renderResourceView(kind, state.arg) }));
  toolbar.replaceChildren(...toolbarButtons);

  const body = $('#viewBody');
  body.replaceChildren(h('div', { class: 'empty', text: 'Loading…' }));

  LIST_SOURCES[kind].load(state.client)
    .then((items) => {
      if (kind === 'rule' && state.ruleMode === 'graph') {
        body.replaceChildren(renderRuleGraph(items, {
          onPick: (rules) => {
            const list = Array.isArray(rules) ? rules : [rules];
            if (list.length === 1) {
              const rule = list[0];
              if (rule.generated) return openGeneratedRule(rule);
              location.hash = `#/rule/${encodeURIComponent(rule.id)}`;
            } else {
              openRuleList(list);
            }
          },
        }));
        if (arg) openResource('rule', arg, {});
        return;
      }
      const filter = state.filters[kind] || '';
      const shown = items.filter((it) => {
        if (!filter) return true;
        return JSON.stringify(it).toLowerCase().includes(filter.toLowerCase());
      });
      const cols = [...res.columns];
      const withActions = kind === 'pool';
      const table = h('table', { class: 'table' },
        h('thead', {}, h('tr', {}, cols.map(c => h('th', { text: c })),
          withActions ? h('th', { text: 'actions' }) : null)),
        h('tbody', {}, shown.map((row) => {
          const key = rowKey(res, row);
          const generated = kind === 'rule' && row.generated;
          const tds = cols.map((c) => {
            const v = row[c];
            if (c === 'source') return h('td', {}, v ? pill(v, 'src-' + v) : '—');
            if (c === 'version') return h('td', { text: v == null ? '—' : String(v) });
            if (c === 'id' && generated) return h('td', {}, h('span', { class: 'mono', text: cellText(v) }), ' ', pill('generated'));
            return h('td', { class: ['name', 'id', 'model', 'provider', 'base_url'].includes(c) ? 'mono' : '', text: cellText(v) });
          });
          if (withActions) {
            tds.push(h('td', { class: 'rowactions' },
              h('button', { class: 'btn small secondary', text: 'Rotate', onclick: (e) => { e.stopPropagation(); runPoolAction('rotate', key); } }),
              h('button', { class: 'btn small secondary', text: 'Clear', onclick: (e) => { e.stopPropagation(); runPoolAction('clear', key); } }),
              h('button', { class: 'btn small secondary', text: 'Recover', onclick: (e) => { e.stopPropagation(); runPoolAction('recover', key, { backends: row.backend || [] }); } })));
          }
          return h('tr', {
            class: state.arg === key ? 'selected' : '',
            onclick: () => {
              if (generated) return openGeneratedRule(row);
              location.hash = `#/${kind}/${encodeURIComponent(key)}`;
            },
          }, tds);
        })));
      const wrap = h('div', { class: 'tablewrap' }, table);
      body.replaceChildren(
        h('div', { class: 'filterbar' },
          h('input', {
            type: 'text', placeholder: 'Filter…', value: filter, spellcheck: 'false',
            oninput: (e) => { state.filters[kind] = e.target.value; const pos = e.target.selectionStart; renderResourceView(kind); const inp = $('.filterbar input'); if (inp) { inp.focus(); inp.setSelectionRange(pos, pos); } },
          }),
          h('span', { class: 'hint', text: `${shown.length} / ${items.length}` })),
        shown.length ? wrap : h('div', { class: 'empty', text: 'No items.' }),
      );
      if (arg) openResource(kind, arg, {});
    })
    .catch(toastError);
}

function rowKey(res, row) { return row[res.idField] ?? row.name ?? row.id; }

// ---- pool quick actions ----
// Shared by the list-row buttons and the drawer footer. `backends` is the
// ordered backend list (for the recover prompt); `backend` targets one directly.
async function runPoolAction(op, name, { backends, backend } = {}) {
  try {
    if (op === 'rotate') await state.client.poolRotate(name);
    else if (op === 'clear') await state.client.poolClear(name);
    else if (op === 'recover') {
      let target = backend;
      if (!target) {
        const hint = backends && backends.length ? ` (${backends.join(', ')})` : '';
        target = prompt(`Backend model id to recover${hint}:`, (backends && backends[0]) || '');
      }
      if (!target) return false;
      await state.client.poolRecover(name, target);
    }
    toast(op + ' ok', 'ok');
    return true;
  } catch (error) {
    toastError(error);
    return false;
  }
}

// ---- resource drawer ----
async function openResource(kind, name, opts = {}) {
  const res = RESOURCES[kind];
  if (opts.isNew && kind === 'key') return openKeyCreate();
  let original = {};
  let version, source;
  if (!opts.isNew) {
    try {
      const got = kind === 'rule' ? await state.client.rule('get', name) : await state.client.resource(kind, 'get', name);
      original = stripMeta(got);
      version = got.version;
      source = got.source;
    } catch (error) {
      if (error instanceof HTTPError && error.status === 404 && opts.row) {
        return openGeneratedRule(opts.row);
      }
      return toastError(error);
    }
  }

  const idName = res.idField;
  const fields = res.fields.map(f => ({ ...f }));
  if (opts.isNew && !fields.some(f => f.name === idName)) {
    const label = idName === 'model' ? 'Model (alias)' : idName[0].toUpperCase() + idName.slice(1);
    fields.unshift({ name: idName, label: label + ' *', type: 'text', required: true, idField: true });
  }
  const metaParts = [];
  if (source) metaParts.push(pill(source, 'src-' + source));
  if (version != null) metaParts.push(h('span', { text: `version ${version}` }));
  const meta = h('span', {}, ...interleave(metaParts));

  // create form needs the id field; edit form hides it (immutable key).
  const formHolder = h('div');
  const aclHolder = h('div');
  const jsonHolder = h('div');
  const isPool = kind === 'pool' && !opts.isNew;
  // Per-resource ACL tab: the fields every addressable resource shares. When
  // editing, those fields live only in the ACL tab; create keeps them in the
  // single form.
  const aclFields = fields.filter((f) => ACL_FIELD_NAMES.includes(f.name));
  const showAcl = !opts.isNew && aclFields.length > 0;
  const formFields = showAcl ? fields.filter((f) => !ACL_FIELD_NAMES.includes(f.name)) : fields;
  // Pool status is its own tab, and the default one.
  const poolStatusBox = isPool
    ? h('section', { class: 'block poolstatus' }, h('div', { class: 'hint', text: 'Loading…' }))
    : null;
  const statusHolder = h('div');
  if (poolStatusBox) statusHolder.append(poolStatusBox);
  const tabDefs = [];
  if (isPool) tabDefs.push(['status', 'Status']);
  tabDefs.push(['fields', 'Fields']);
  if (showAcl) tabDefs.push(['acl', 'ACL']);
  tabDefs.push(['json', 'JSON']);
  let active = tabDefs[0][0];
  let form, aclForm;

  const neededSources = [...new Set(fields.filter((f) => f.source).map((f) => f.source))];
  const enums = {};
  await Promise.all(neededSources.map(async (s) => {
    try { enums[s] = await loadEnum(state.client, s); } catch { enums[s] = []; }
  }));

  const buildFields = () => {
    const doc = { ...original };
    if (opts.isNew) doc[idName] = name || '';
    form = buildForm(formFields, doc, { isNew: opts.isNew, idField: idName }, enums);
    formHolder.replaceChildren(form.grid);
    if (showAcl) {
      aclForm = buildForm(aclFields, { ...original }, { isNew: false }, enums);
      aclHolder.replaceChildren(aclForm.grid);
    }
  };
  const buildJSON = () => {
    jsonHolder.replaceChildren(h('textarea', {
      class: 'json', spellcheck: 'false',
      oninput: (e) => { currentJSON = e.target; },
    }));
    const ta = jsonHolder.querySelector('textarea');
    ta.style.width = '100%'; ta.style.minHeight = '320px'; ta.style.fontFamily = 'var(--mono)';
    ta.value = JSON.stringify(original, null, 2);
    currentJSON = ta;
  };
  let currentJSON = null;
  buildFields();
  buildJSON();

  const tabs = tabDefs.length > 1
    ? h('div', { class: 'tabs' }, ...tabDefs.map(([id, label]) =>
      h('button', { class: id === active ? 'active' : '', text: label, onclick: (e) => { active = id; switchTabs(e.target); } })))
    : null;
  function switchTabs(btn) {
    if (tabs) for (const b of tabs.children) b.classList.toggle('active', b === btn);
    statusHolder.hidden = active !== 'status';
    formHolder.hidden = active !== 'fields';
    aclHolder.hidden = active !== 'acl';
    jsonHolder.hidden = active !== 'json';
  }
  statusHolder.hidden = active !== 'status';
  formHolder.hidden = active !== 'fields';
  aclHolder.hidden = active !== 'acl';
  jsonHolder.hidden = active !== 'json';

  // collect the resulting document from whichever tab is active
  const resultDoc = () => {
    if (active === 'json') {
      const text = (currentJSON.value || '').trim();
      return text ? JSON.parse(text) : {};
    }
    const inputs = active === 'acl' && aclForm ? aclForm.inputs : form.inputs;
    return composeDoc(inputs, original, { isNew: opts.isNew });
  };

  const actions = [];
  if (opts.isNew) {
    actions.push(h('button', { class: 'btn', text: 'Create', onclick: () => create(kind, name) }));
  } else {
    actions.push(h('button', { class: 'btn', text: 'Save changes', onclick: () => patch(kind, name, version) }));
    if (kind === 'rule') actions.push(h('button', { class: 'btn ghost', text: 'Reset', onclick: () => reset('rule', name) }));
    actions.push(h('button', { class: 'btn danger', text: 'Delete', onclick: () => remove(kind, name) }));
  }
  if (isPool) {
    actions.push(h('button', { class: 'btn secondary', text: 'Rotate', onclick: () => poolOp('rotate', name) }));
    actions.push(h('button', { class: 'btn secondary', text: 'Clear cooldowns', onclick: () => poolOp('clear', name) }));
    actions.push(h('button', { class: 'btn secondary', text: 'Recover backend…', onclick: () => poolOp('recover', name) }));
    actions.push(h('button', { class: 'btn ghost', text: 'Refresh status', onclick: () => refreshPoolStatus() }));
  }

  const body = h('div', {},
    kind === 'rule' && opts.isNew ? h('p', { class: 'hint' }, 'Rule verbs: from + (action|to); a pure transform rule needs set or default.') : null,
    tabs, statusHolder, formHolder, aclHolder, jsonHolder);

  showDrawer({
    title: opts.isNew ? (res.createLabel || 'New ' + kind) : (name || idName),
    meta, body, actions,
  });
  if (poolStatusBox) refreshPoolStatus();

  async function invoke(fn) { try { await fn(); } catch (error) { toastError(error); } }

  async function create(kind, name) {
    await invoke(async () => {
      const doc = resultDoc();
      const id = doc[idName] || name || '';
      if (!id) throw new Error(`${idName} is required`);
      if (kind === 'rule') await state.client.rule('set', id, { ...doc, id });
      else await state.client.resource(kind, 'set', id, doc);
      invalidateEnums();
      toast('Created', 'ok');
      closeDrawer();
      location.hash = `#/${kind}/${encodeURIComponent(id)}`;
      route();
    });
  }
  async function patch(kind, name, version) {
    await invoke(async () => {
      const doc = resultDoc();
      const p = diffDoc(doc, original);
      if (Object.keys(p).length === 0) { toast('No changes'); return; }
      if (kind === 'rule') await state.client.rule('patch', name, p, version);
      else await state.client.resource(kind, 'patch', name, p, version);
      invalidateEnums();
      toast('Saved', 'ok');
      route();
    });
  }
  async function reset(kind, name) {
    if (!confirm(`Reset ${name} to its YAML baseline?`)) return;
    await invoke(async () => {
      await state.client.rule('reset', name);
      invalidateEnums();
      toast('Reset', 'ok'); closeDrawer(); route();
    });
  }
  async function remove(kind, name) {
    if (!confirm(`Delete ${name}?`)) return;
    await invoke(async () => {
      if (kind === 'rule') await state.client.rule('delete', name);
      else await state.client.resource(kind, 'delete', name);
      invalidateEnums();
      toast('Deleted', 'ok');
      closeDrawer();
      location.hash = `#/${kind}`;
      route();
    });
  }
  async function poolOp(op, name) {
    if (await runPoolAction(op, name, { backends: original.backend || [] })) refreshPoolStatus();
  }
  async function refreshPoolStatus() {
    if (!poolStatusBox) return;
    try {
      const st = await state.client.poolStatus(name);
      poolStatusBox.replaceChildren(renderPoolStatus(st, {
        recover: (backend) => runPoolAction('recover', name, { backend }).then((ok) => { if (ok) refreshPoolStatus(); }),
      }));
    } catch (error) {
      poolStatusBox.replaceChildren(h('div', { class: 'hint', text: String(error.message || error) }));
    }
  }
}

function interleave(nodes) {
  const out = [];
  nodes.forEach((n, i) => { if (i) out.push(document.createTextNode(' · ')); out.push(n); });
  return out;
}

// ---- pool runtime status ----
function chip(k, v) { return h('span', { class: 'chip' }, h('b', { text: k }), h('span', { text: ' ' + v })); }
function healthPill(v) {
  if (!v) return '—';
  const cls = v === 'normal' ? 'ok' : v === 'dead' ? 'bad' : 'warn';
  return pill(v, cls);
}
function renderPoolStatus(st, opts = {}) {
  const health = st.health || {};
  const backends = (st.backends && st.backends.length ? st.backends : Object.keys(health));
  const withActions = typeof opts.recover === 'function';
  const rows = backends.map((b) => {
    const hlt = health[b] || {};
    const isActive = b === st.active;
    return h('tr', { class: isActive ? 'selected' : '' },
      h('td', { class: 'mono' }, b, isActive ? document.createTextNode(' ') : null, isActive ? pill('active', 'ok') : null),
      h('td', {}, healthPill(hlt.health)),
      h('td', { text: hlt.ttft_ms ? Math.round(hlt.ttft_ms) + ' ms' : '—' }),
      h('td', { text: hlt.cost != null && hlt.cost !== 0 ? String(hlt.cost) : '—' }),
      h('td', { text: hlt.samples ? String(hlt.samples) : '—' }),
      h('td', { text: hlt.until_sec ? hlt.until_sec + 's' : '—' }),
      h('td', { text: hlt.reason || '' }),
      withActions ? h('td', { class: 'rowactions' }, h('button', {
        class: 'btn small secondary', text: 'Recover', title: 'reset this backend to healthy',
        onclick: () => opts.recover(b),
      })) : null);
  });
  const heads = ['backend', 'health', 'ttft', 'cost', 'samples', 'cooldown', 'reason'];
  if (withActions) heads.push('actions');
  const chips = h('div', { class: 'pills' },
    chip('active', st.active || '—'),
    chip('policy', st.policy || '—'),
    chip('fallback', st.fallback || '—'),
    chip('on_all_limited', st.on_all_limited || '—'),
    chip('cooldown_sec', st.cooldown_sec ?? '—'),
    chip('reselect_ttft_ms', st.reselect_ttft_ms ?? '—'),
    st.downgraded ? pill('downgraded', 'bad') : null);
  const policyChips = st.policies && Object.keys(st.policies).length
    ? h('div', { class: 'pills' },
      h('span', { class: 'hint', text: 'per-policy active:' }),
      ...Object.entries(st.policies).map(([policy, backend]) => chip(policy, backend)))
    : null;
  return h('div', {},
    chips,
    policyChips,
    h('div', { class: 'tablewrap' }, h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, heads.map((c) => h('th', { text: c })))),
      h('tbody', {}, rows.length ? rows : h('tr', {}, h('td', { colspan: heads.length, class: 'hint', text: 'no backends' }))))));
}

function openGeneratedRule(row) {
  showDrawer({
    title: row.id,
    meta: h('span', {}, pill('generated'), h('span', { text: ' (compiled routing edge; read-only)' })),
    body: h('pre', { class: 'json', text: JSON.stringify(row, null, 2) }),
    actions: [],
  });
}

// openRuleList shows the several rules merged into one wire in the graph.
function openRuleList(rules) {
  const rows = rules.map((r) => h('tr', {
    style: r.generated ? '' : 'cursor:pointer',
    onclick: () => {
      if (r.generated) return;
      closeDrawer();
      location.hash = `#/rule/${encodeURIComponent(r.id)}`;
    },
  },
  h('td', { class: 'mono', text: r.id || '(generated)' }),
  h('td', { class: 'mono', text: cellText(r.match && r.match.length ? r.match.map(c => `${c.field}${c.op === 'eq' ? '=' : ' ' + c.op + ' '}${c.value ?? ''}`).join(', ') : '') }),
  h('td', {}, r.generated ? pill('generated') : '')));
  showDrawer({
    title: `${rules.length} merged rules`,
    meta: h('span', { class: 'hint', text: 'Same from → to; click a non-generated rule to edit.' }),
    body: h('div', { class: 'tablewrap' }, h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, ['id', 'match', ''].map(c => h('th', { text: c })))),
      h('tbody', {}, rows))),
    actions: [],
  });
}

// ---- multi-select (dropdown + checkboxes) ----
function buildMultiSelect(id, options, current) {
  const selected = new Set(current);
  const root = h('div', { class: 'multiselect', id });
  const summary = h('span', { class: 'ms-summary' });
  const toggle = h('button', { type: 'button', class: 'ms-toggle' }, summary, h('span', { class: 'ms-caret', text: '▾' }));
  const search = h('input', { type: 'text', class: 'ms-search', placeholder: 'Filter…', spellcheck: 'false' });
  const optsBox = h('div', { class: 'ms-options' });
  const addInput = h('input', { type: 'text', class: 'ms-add', placeholder: 'Add value ↵', spellcheck: 'false' });
  const panel = h('div', { class: 'ms-panel', hidden: true }, search, optsBox, addInput);
  root.append(toggle, panel);

  const renderSummary = () => {
    const vals = [...selected].sort();
    summary.textContent = vals.length ? (vals.length > 2 ? `${vals.length} selected` : vals.join(', ')) : '(none)';
    summary.classList.toggle('empty', vals.length === 0);
  };
  const renderOptions = () => {
    const q = search.value.trim().toLowerCase();
    optsBox.replaceChildren();
    for (const o of [...new Set([...options, ...selected])].sort()) {
      if (q && !o.toLowerCase().includes(q)) continue;
      const custom = !options.includes(o);
      const cb = h('input', { type: 'checkbox' });
      cb.checked = selected.has(o);
      cb.addEventListener('change', () => { if (cb.checked) selected.add(o); else selected.delete(o); renderSummary(); });
      const row = h('label', { class: 'ms-opt' + (custom ? ' custom' : '') }, cb, h('span', { text: o }));
      if (custom) row.append(h('button', {
        type: 'button', class: 'ms-x', text: '×', title: 'remove',
        onclick: (e) => { e.preventDefault(); selected.delete(o); renderOptions(); renderSummary(); },
      }));
      optsBox.append(row);
    }
    if (!optsBox.children.length) optsBox.append(h('div', { class: 'hint', text: 'no match' }));
  };

  toggle.addEventListener('click', () => {
    const open = panel.hidden;
    panel.hidden = !open;
    root.classList.toggle('open', open);
    if (open) { search.value = ''; renderOptions(); search.focus(); }
  });
  search.addEventListener('input', renderOptions);
  addInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      const v = addInput.value.trim();
      if (v) { selected.add(v); addInput.value = ''; renderOptions(); renderSummary(); }
    }
  });
  root._values = () => [...selected];
  renderSummary();
  renderOptions();
  return root;
}
function closeMultiSelects(except) {
  for (const ms of document.querySelectorAll('.multiselect.open')) {
    if (ms === except) continue;
    ms.classList.remove('open');
    const panel = ms.querySelector('.ms-panel');
    if (panel) panel.hidden = true;
  }
}

// ---- form building ----
function buildForm(fields, doc, opts = {}, enums = {}) {
  const grid = h('div', { class: 'formgrid' });
  const inputs = {};
  for (const f of fields) {
    if (f.idField && !opts.isNew) continue; // immutable id
    const id = 'f_' + f.name;
    const val = doc ? doc[f.name] : undefined;
    let control;
    let extra = null;
    switch (f.type) {
      case 'checkbox':
        control = h('input', { type: 'checkbox', id }); control.checked = !!val; break;
      case 'tristate': {
        control = h('select', { id });
        for (const [v, t] of [['', 'default'], ['true', 'true'], ['false', 'false']]) control.append(h('option', { value: v, text: t }));
        control.value = val === true ? 'true' : val === false ? 'false' : ''; break;
      }
      case 'select': {
        const options = (f.source ? (enums[f.source] || []) : (f.options || [])).map(String);
        const cur = val == null ? '' : String(val);
        control = h('select', { id });
        if (!f.required) control.append(h('option', { value: '', text: '(unset)' }));
        if (cur && !options.includes(cur)) control.append(h('option', { value: cur, text: cur + ' (current)' }));
        for (const o of options) control.append(h('option', { value: o, text: o }));
        control.value = cur; break;
      }
      case 'datalist': {
        const listId = id + '_list';
        control = h('input', { type: 'text', id, spellcheck: 'false', list: listId, placeholder: f.placeholder || '' });
        control.value = val == null ? '' : String(val);
        const dl = h('datalist', { id: listId });
        for (const o of (enums[f.source] || [])) dl.append(h('option', { value: String(o) }));
        extra = dl; break;
      }
      case 'checklist': {
        const options = (enums[f.source] || []).map(String);
        const cur = Array.isArray(val) ? val.map(String) : [];
        control = buildMultiSelect(id, options, cur);
        break;
      }
      case 'list':
        control = h('input', { type: 'text', id, spellcheck: 'false', placeholder: 'comma,separated' });
        control.value = Array.isArray(val) ? val.join(', ') : ''; break;
      case 'json':
        control = h('textarea', { id, spellcheck: 'false' });
        control.value = val == null ? '' : JSON.stringify(val, null, 2); break;
      case 'number':
        control = h('input', { type: 'number', id });
        control.value = val == null ? '' : String(val); break;
      case 'password':
        control = h('input', { type: 'password', id, autocomplete: 'off', placeholder: f.placeholder || '' });
        control.value = ''; break;
      default:
        control = h('input', { type: 'text', id, spellcheck: 'false', placeholder: f.placeholder || '' });
        control.value = val == null ? '' : String(val);
    }
    inputs[f.name] = { field: f, control };
    const full = f.type === 'json' || f.type === 'checklist';
    const wrap = h('div', { class: 'field' + (f.type === 'checkbox' ? ' check' : '') + (full ? ' full' : '') });
    if (f.type === 'checkbox') wrap.append(h('label', {}, control, h('span', { text: f.label })));
    else wrap.append(h('label', { for: id, text: f.label + (f.required ? ' *' : '') }), control);
    if (extra) wrap.append(extra);
    if (f.note) wrap.append(h('div', { class: 'note', text: f.note }));
    grid.append(wrap);
  }
  return { grid, inputs };
}

function readField(f, control) {
  switch (f.type) {
    case 'checkbox': return control.checked;
    case 'tristate': return control.value === '' ? null : control.value === 'true';
    case 'select': return control.value === '' ? null : control.value;
    case 'datalist': { const t = control.value.trim(); return t === '' ? null : control.value; }
    case 'checklist': return control._values ? control._values() : [];
    case 'list': return control.value.split(',').map(s => s.trim()).filter(Boolean);
    case 'json': { const t = control.value.trim(); return t ? JSON.parse(t) : null; }
    case 'number': { const t = control.value.trim(); return t ? Number(t) : null; }
    case 'password': return control.value === '' ? undefined : control.value;
    default: { const t = control.value; return t.trim() === '' ? null : t; }
  }
}
function composeDoc(inputs, original, opts = {}) {
  const doc = { ...stripMeta(original) };
  for (const [name, { field, control }] of Object.entries(inputs)) {
    let v;
    try { v = readField(field, control); }
    catch (e) { throw new Error(`field "${name}": ${e.message}`); }
    if (v === undefined) continue;
    if (v === null) delete doc[name];
    else doc[name] = v;
  }
  return doc;
}

// ---- key create ----
function openKeyCreate() {
  const idInput = h('input', { type: 'text', spellcheck: 'false', placeholder: 'key id' });
  const groupsInput = h('input', { type: 'text', spellcheck: 'false', placeholder: 'eng, ops' });
  const result = h('div', { hidden: true });
  const body = h('div', {},
    h('p', { class: 'hint' }, 'The plaintext key is returned exactly once. Store it now.'),
    h('div', { class: 'field' }, h('label', { text: 'ID *' }), idInput),
    h('div', { class: 'field' }, h('label', { text: 'Groups' }), groupsInput),
    result);
  showDrawer({
    title: 'Create key', body,
    actions: [h('button', {
      class: 'btn', text: 'Create',
      onclick: async () => {
        try {
          const id = idInput.value.trim();
          if (!id) throw new Error('id is required');
          const groups = groupsInput.value.split(',').map(s => s.trim()).filter(Boolean);
          const out = await state.client.action('key/create', { id, groups });
          result.hidden = false;
          result.replaceChildren(
            h('p', { class: 'hint' }, 'This key is shown once. Creating the first key turns on authentication, so save it here to keep using the console.'),
            h('div', { class: 'field' }, h('label', { text: 'Plaintext key (shown once)' }),
              h('input', { type: 'text', value: out.key, readonly: true, onclick: (e) => e.target.select() })),
            h('div', { style: 'display:flex;gap:8px' },
              h('button', { class: 'btn secondary small', text: 'Copy', onclick: () => { navigator.clipboard?.writeText(out.key); toast('Copied', 'ok'); } }),
              h('button', { class: 'btn small', text: 'Use as console key', onclick: () => { localStorage.setItem('lmgateway.apiKey', out.key); loadAuth(); buildClient(); checkAuth(); toast('Saved as console key', 'ok'); } })));
          toast('Key created', 'ok');
          invalidateEnums();
        } catch (error) { toastError(error); }
      },
    })],
  });
}

// ---- settings view ----
function renderSettingsView(arg) {
  $('#viewTitle').textContent = 'Settings';
  $('#toolbar').replaceChildren(h('button', { class: 'btn secondary', text: 'Refresh', onclick: () => renderSettingsView() }));
  const body = $('#viewBody');
  body.replaceChildren(h('div', { class: 'empty', text: 'Loading…' }));
  state.client.resource('setting', 'list').then((r) => {
    const items = r.items || [];
    const table = h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, ['name', 'source', 'version'].map(c => h('th', { text: c })))),
      h('tbody', {}, items.map((it) => h('tr', {
        onclick: () => openSetting(it.name),
      }, h('td', { class: 'mono', text: it.name }),
        h('td', {}, it.source ? pill(it.source, 'src-' + it.source) : '—'),
        h('td', { text: it.version == null ? '—' : String(it.version) })))));
    body.replaceChildren(h('div', { class: 'tablewrap' }, table));
    if (arg) openSetting(arg);
  }).catch(toastError);
}

async function openSetting(name) {
  let got;
  try { got = await state.client.resource('setting', 'get', name); }
  catch (error) { return toastError(error); }
  const original = stripMeta(got);
  const def = SETTINGS[name] || { fields: [] };
  const form = buildForm(def.fields, original, {});
  const actions = [
    h('button', { class: 'btn', text: 'Save changes', onclick: () => invoke(async () => {
      const doc = composeDoc(form.inputs, original, {});
      const p = diffDoc(doc, original);
      if (!Object.keys(p).length) { toast('No changes'); return; }
      await state.client.resource('setting', 'patch', name, p, got.version);
      toast('Saved', 'ok'); route();
    }) }),
    h('button', { class: 'btn ghost', text: 'Reset', onclick: () => invoke(async () => {
      if (!confirm(`Reset setting ${name} to YAML baseline?`)) return;
      await state.client.resource('setting', 'reset', name);
      toast('Reset', 'ok'); closeDrawer(); route();
    }) }),
  ];
  showDrawer({
    title: 'setting/' + name,
    meta: h('span', {}, got.source ? pill(got.source, 'src-' + got.source) : null, h('span', { text: ` version ${got.version}` })),
    body: form.grid, actions,
  });
}

// ---- alias view ----
function renderAliasView(arg) {
  $('#viewTitle').textContent = 'Aliases';
  $('#toolbar').replaceChildren(
    h('button', { class: 'btn', text: 'New alias', onclick: () => openAlias(null, { isNew: true }) }),
    h('button', { class: 'btn secondary', text: 'Refresh', onclick: () => renderAliasView() }));
  const body = $('#viewBody');
  body.replaceChildren(h('div', { class: 'empty', text: 'Loading…' }));
  listAliases(state.client).then((items) => {
    const table = h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, ['name', 'model', 'default'].map(c => h('th', { text: c })))),
      h('tbody', {}, items.map((it) => h('tr', { onclick: () => openAlias(it.name, {}) },
        h('td', { class: 'mono', text: it.name }),
        h('td', { class: 'mono', text: it.model == null ? '—' : String(it.model) }),
        h('td', { class: 'mono', text: it.default ? JSON.stringify(it.default) : '—' })))));
    body.replaceChildren(
      h('p', { class: 'hint' }, 'An alias is a rule with id alias-<name>: model is the hard target; effort/summary/temperature/top_p/max_tokens are soft defaults.'),
      items.length ? h('div', { class: 'tablewrap' }, table) : h('div', { class: 'empty', text: 'No aliases.' }));
    if (arg) openAlias(arg, {});
  }).catch(toastError);
}

async function openAlias(name, opts = {}) {
  const ruleID = opts.isNew ? '' : aliasRuleID(name);
  let rule = null;
  if (!opts.isNew) {
    try { rule = await state.client.rule('get', ruleID); }
    catch (error) { if (!(error instanceof HTTPError && error.status === 404)) return toastError(error); }
  }
  const sets = (rule && rule.set) || {};
  const defaults = (rule && rule.default) || {};

  const nameInput = h('input', { type: 'text', id: 'alias_name', spellcheck: 'false', value: name || '' });
  const modelInput = h('input', { type: 'text', id: 'alias_model', spellcheck: 'false', value: sets.model == null ? '' : String(sets.model) });
  const soft = {
    effort: valuesOf(defaults['reasoning.effort'] ?? ''),
    summary: valuesOf(defaults['reasoning.summary'] ?? ''),
    temperature: defaults.temperature == null ? '' : String(defaults.temperature),
    top_p: defaults.top_p == null ? '' : String(defaults.top_p),
    max_tokens: defaults.max_tokens == null ? '' : String(defaults.max_tokens),
  };
  const softInputs = {};
  const softGrid = h('div', { class: 'formgrid' });
  for (const key of ['effort', 'summary', 'temperature', 'top_p', 'max_tokens']) {
    const c = h('input', { type: 'text', id: 'alias_' + key, spellcheck: 'false', value: soft[key] });
    softInputs[key] = c;
    softGrid.append(h('div', { class: 'field' }, h('label', { for: 'alias_' + key, text: key + ' (soft default)' }), c));
  }

  const body = h('div', {},
    h('div', { class: 'field' }, h('label', { for: 'alias_name', text: 'Alias name *' }), nameInput),
    h('div', { class: 'field' }, h('label', { for: 'alias_model', text: 'model (hard target) *' }), modelInput),
    h('h3', { text: 'Soft defaults', style: 'margin:14px 0 8px' }),
    softGrid,
    rule ? h('details', { style: 'margin-top:14px' }, h('summary', { text: 'Underlying rule' }), h('pre', { class: 'json', text: JSON.stringify(rule, null, 2) })) : null);

  const actions = [h('button', {
    class: 'btn', text: opts.isNew ? 'Create' : 'Save',
    onclick: () => invoke(async () => {
      const nm = nameInput.value.trim();
      if (!nm) throw new Error('alias name is required');
      const model = modelInput.value.trim();
      if (!model) throw new Error('alias target model is required (model <name>)');
      const newSets = { ...sets, model };
      const newDefaults = { ...defaults };
      applySoft(newDefaults, softInputs);
      const id = aliasRuleID(nm);
      await state.client.rule('set', id, aliasRuleBody(id, nm, newSets, newDefaults), rule && rule.version);
      toast(opts.isNew ? 'Created' : 'Saved', 'ok');
      closeDrawer(); route();
    }),
  })];
  if (!opts.isNew) {
    actions.push(h('button', { class: 'btn ghost', text: 'Reset', onclick: () => invoke(async () => {
      if (!confirm(`Reset alias ${name}?`)) return;
      await state.client.rule('reset', ruleID); toast('Reset', 'ok'); closeDrawer(); route();
    }) }));
    actions.push(h('button', { class: 'btn danger', text: 'Delete', onclick: () => invoke(async () => {
      if (!confirm(`Delete alias ${name}?`)) return;
      await state.client.rule('delete', ruleID); toast('Deleted', 'ok'); closeDrawer(); route();
    }) }));
  }
  showDrawer({
    title: opts.isNew ? 'New alias' : 'alias/' + name,
    meta: rule ? h('span', {}, rule.source ? pill(rule.source, 'src-' + rule.source) : null) : null,
    body, actions,
  });
}
function valuesOf(v) { return v == null ? '' : String(v); }
function applySoft(defaults, inputs) {
  const map = { effort: 'reasoning.effort', summary: 'reasoning.summary', temperature: 'temperature', top_p: 'top_p', max_tokens: 'max_tokens' };
  for (const [key, path] of Object.entries(map)) {
    const raw = inputs[key].value.trim();
    if (raw === '') { delete defaults[path]; continue; }
    let val = raw;
    try { val = JSON.parse(raw); } catch { /* keep string */ }
    defaults[path] = val;
  }
}

// ---- codex view ----
function renderCodexView() {
  $('#viewTitle').textContent = 'Codex login';
  $('#toolbar').replaceChildren(h('button', { class: 'btn secondary', text: 'Check status', onclick: () => codexStatus(out) }));
  const out = h('pre', { class: 'json', text: 'Ready.' });
  const pollBtn = h('button', { class: 'btn secondary', text: 'Poll device login', disabled: true, onclick: () => codexCall(async () => {
    const data = await state.client.codexDevicePoll(requestId);
    out.textContent = JSON.stringify(data, null, 2);
    if (data.status === 'authenticated') { pollBtn.disabled = true; codexStatus(out); }
  }) });
  let requestId = '';
  const body = h('div', {},
    h('p', { class: 'hint' }, 'Authenticate the gateway with a ChatGPT subscription. Tokens are never rendered here.'),
    h('div', { style: 'display:flex;gap:8px;flex-wrap:wrap;margin:10px 0' },
      h('button', { class: 'btn', text: 'Check status', onclick: () => codexStatus(out) }),
      h('button', { class: 'btn', text: 'Device login', onclick: () => codexCall(async () => {
        const data = await state.client.codexDeviceStart();
        requestId = data.request_id || '';
        pollBtn.disabled = !requestId;
        out.textContent = JSON.stringify(data, null, 2);
        if (data.verification_url) window.open(data.verification_url, '_blank', 'noopener,noreferrer');
      }) }),
      pollBtn,
      h('button', { class: 'btn danger', text: 'Logout', onclick: () => codexCall(async () => { out.textContent = JSON.stringify(await state.client.codexLogout(), null, 2); }) })),
    out);
  $('#viewBody').replaceChildren(body);
  async function codexCall(fn) { try { await fn(); } catch (error) { out.textContent = String(error.message || error); toastError(error); } }
  codexStatus(out);
}
async function codexStatus(out) {
  try {
    const data = await state.client.codexStatus();
    out.textContent = JSON.stringify(data, null, 2);
    $('#authStatus').className = 'dot ok';
  } catch (error) { out.textContent = String(error.message || error); }
}

// ---- actions view ----
const KNOWN_ACTIONS = [
  ['config/reload', 'Reload config from the database'],
  ['config/seed', 'Re-seed from the YAML baseline'],
  ['rollup/run', 'Run the spend rollup now'],
  ['spend/flush', 'Flush buffered time-series'],
];
function renderActionsView() {
  $('#viewTitle').textContent = 'Actions';
  $('#toolbar').replaceChildren();
  const out = h('pre', { class: 'json', text: 'Ready.' });
  const nameInput = h('input', { type: 'text', spellcheck: 'false', placeholder: 'action/name' });
  const bodyInput = h('textarea', { spellcheck: 'false', placeholder: '{} (optional JSON body)' });
  bodyInput.style.width = '100%'; bodyInput.style.minHeight = '80px'; bodyInput.style.fontFamily = 'var(--mono)';
  const body = h('div', {},
    h('section', { class: 'block' }, h('h2', { text: 'Common actions' }),
      h('div', { style: 'display:flex;gap:8px;flex-wrap:wrap' }, KNOWN_ACTIONS.map(([name, label]) =>
        h('button', { class: 'btn secondary', text: name, title: label, onclick: () => runAction(name) })))),
    h('section', { class: 'block' }, h('h2', { text: 'Custom action' }),
      h('div', { class: 'field' }, h('label', { text: 'Name' }), nameInput),
      h('div', { class: 'field' }, h('label', { text: 'Body' }), bodyInput),
      h('button', { class: 'btn', text: 'Run', onclick: () => runAction(nameInput.value.trim(), bodyInput.value.trim()) })),
    out);
  $('#viewBody').replaceChildren(body);
  async function runAction(name, bodyText) {
    if (!name) return;
    try {
      let parsed = null;
      if (bodyText) parsed = JSON.parse(bodyText);
      out.textContent = JSON.stringify(await state.client.action(name, parsed), null, 2);
      toast(name + ' ok', 'ok');
    } catch (error) { out.textContent = String(error.message || error); toastError(error); }
  }
}

// ---- spend / ts view ----
function renderSpendView() {
  $('#viewTitle').textContent = 'Spend / Time series';
  $('#toolbar').replaceChildren();
  const summary = h('div', { class: 'cards' });
  const tagSummary = h('div', { class: 'tablewrap' }, h('div', { class: 'empty', text: 'Loading…' }));
  const tagFilter = h('input', { type: 'text', placeholder: 'key_tag (optional)', spellcheck: 'false' });
  const streamInput = h('input', { type: 'text', value: 'spend', spellcheck: 'false' });
  const fromInput = h('input', { type: 'text', placeholder: 'RFC3339 (optional)', spellcheck: 'false' });
  const toInput = h('input', { type: 'text', placeholder: 'RFC3339 (optional)', spellcheck: 'false' });
  const limitInput = h('input', { type: 'number', value: '20' });
  const records = h('pre', { class: 'json', text: 'Query to view records.' });
  const qBody = h('div', {},
    h('div', { class: 'formgrid' },
      h('div', { class: 'field' }, h('label', { text: 'Key tag' }), tagFilter),
      h('div', { class: 'field' }, h('label', { text: 'Stream' }), streamInput),
      h('div', { class: 'field' }, h('label', { text: 'Limit' }), limitInput),
      h('div', { class: 'field' }, h('label', { text: 'From' }), fromInput),
      h('div', { class: 'field' }, h('label', { text: 'To' }), toInput)),
    h('button', { class: 'btn secondary', text: 'Query', onclick: () => loadRecords() }));
  const body = h('div', {},
    h('section', { class: 'block' }, h('h2', { text: 'Token summary' }),
      h('div', { class: 'filterbar' }, ...['today', 'yesterday', '7d', '30d'].map(t =>
        h('button', { class: 'btn small secondary', text: t, onclick: () => loadSummary(t) }))),
      summary),
    h('section', { class: 'block' }, h('h2', { text: 'By key tag' }),
      h('p', { class: 'hint', text: 'Client-declared tags from the key suffix (sk-…#tag) or the request body key_tag. Click a row to filter the raw records.' }),
      tagSummary),
    h('section', { class: 'block' }, h('h2', { text: 'Time-series query' }), qBody, records));
  $('#viewBody').replaceChildren(body);
  loadSummary('today');

  async function loadSummary(time) {
    try {
      const [s, tags] = await Promise.all([
        state.client.spend(time),
        state.client.spendTags(time).catch(() => ({ rows: [] })),
      ]);
      const t = s.tokens || {};
      summary.replaceChildren(
        card('Requests', s.requests),
        card('Total tokens', fmtTokens(t.total_tokens), null, fmtInt(t.total_tokens)),
        card('Prompt', fmtTokens(t.prompt_tokens), null, fmtInt(t.prompt_tokens)),
        card('Completion', fmtTokens(t.completion_tokens), null, fmtInt(t.completion_tokens)),
        card('Reasoning', fmtTokens(t.reasoning_tokens), null, fmtInt(t.reasoning_tokens)),
        card('Cached', fmtTokens(t.cached_tokens), null, fmtInt(t.cached_tokens)),
        card('Cost', typeof s.cost === 'number' ? s.cost.toFixed(4) : s.cost, s.timezone),
        card('TTFT avg (ms)', Math.round(s.timing_ms?.ttft_ms?.avg || 0)),
        card('Source', s.source),
      );
      renderTags(tags.rows || []);
    } catch (error) { toastError(error); }
  }
  function renderTags(rows) {
    if (!rows.length) {
      tagSummary.replaceChildren(h('div', { class: 'empty', text: 'No spend records.' }));
      return;
    }
    const total = rows.reduce((n, r) => n + (r.tokens?.total_tokens || 0), 0);
    tagSummary.replaceChildren(h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, ['key_tag', 'requests', 'tokens', 'share', 'cost'].map(c => h('th', { text: c })))),
      h('tbody', {}, rows.map((r) => {
        const tokens = r.tokens?.total_tokens || 0;
        const share = total > 0 ? (tokens / total * 100).toFixed(1) + '%' : '—';
        const clickable = r.tag !== '(untagged)';
        return h('tr', {
          class: clickable ? 'clickable' : 'static',
          onclick: clickable ? () => { tagFilter.value = r.tag; loadRecords(); } : null,
        },
          h('td', { class: 'mono', text: r.tag }),
          h('td', { text: fmtInt(r.requests) }),
          h('td', { text: fmtTokens(tokens), title: fmtInt(tokens) }),
          h('td', { text: share }),
          h('td', { text: typeof r.cost === 'number' ? r.cost.toFixed(4) : r.cost }));
      }))));
  }
  async function loadRecords() {
    try {
      const r = await state.client.tsQuery(streamInput.value.trim() || 'spend', {
        from: fromInput.value.trim(), to: toInput.value.trim(), limit: limitInput.value.trim(),
        key_tag: tagFilter.value.trim(),
      });
      records.textContent = JSON.stringify(r.records || [], null, 2);
    } catch (error) { toastError(error); }
  }
}
function fmtInt(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n.toLocaleString('en-US') : (v == null ? '—' : String(v));
}
// Tokens are shown compactly (12.3K / 4.5M / 1.2B); the exact count stays in
// the hover title.
function fmtTokens(v) {
  if (v == null) return '—';
  const n = Number(v);
  if (!Number.isFinite(n)) return String(v);
  const abs = Math.abs(n);
  if (abs < 1000) return n.toLocaleString('en-US');
  for (const [div, suffix] of [[1e9, 'B'], [1e6, 'M'], [1e3, 'K']]) {
    if (abs >= div) {
      const x = n / div;
      return (x >= 100 ? Math.round(x).toString() : x.toFixed(1).replace(/\.0$/, '')) + suffix;
    }
  }
  return fmtInt(v);
}
function card(k, v, sub, title) {
  return h('div', { class: 'card' }, h('div', { class: 'k', text: k }),
    h('div', { class: 'v', text: v == null ? '—' : String(v), title: title || null }),
    sub ? h('div', { class: 'sub', text: sub }) : null);
}

// ---- dashboard ----
function renderDashboard() {
  $('#viewTitle').textContent = 'Dashboard';
  $('#toolbar').replaceChildren(h('button', { class: 'btn secondary', text: 'Refresh', onclick: renderDashboard }));
  const cards = h('div', { class: 'cards' });
  const pools = h('div', { class: 'tablewrap' });
  const body = h('div', {},
    h('section', { class: 'block' }, h('h2', { text: 'Overview' }), cards),
    h('section', { class: 'block' }, h('h2', { text: 'Pools' }), pools));
  $('#viewBody').replaceChildren(body);

  (async () => {
    try {
      const [health, providers, models, keys, spend, poolList] = await Promise.all([
        state.client.health().then(() => true).catch(() => false),
        state.client.resource('provider', 'list').then(r => (r.items || []).length).catch(() => '—'),
        state.client.resource('model', 'list').then(r => (r.items || []).length).catch(() => '—'),
        state.client.resource('key', 'list').then(r => (r.items || []).length).catch(() => '—'),
        state.client.spend('today').catch(() => null),
        state.client.resource('pool', 'list').then(r => r.items || []).catch(() => []),
      ]);
      cards.replaceChildren(
        card('Health', health ? 'ok' : 'unreachable'),
        card('Providers', providers),
        card('Models', models),
        card('Keys', keys),
        card('Requests today', spend ? spend.requests : '—'),
        card('Tokens today', spend ? fmtTokens(spend.tokens?.total_tokens) : '—', spend ? spend.source : null, spend ? fmtInt(spend.tokens?.total_tokens) : null),
      );
      await Promise.all(poolList.map(async (p) => {
        try { return await state.client.poolStatus(p.model); } catch { return null; }
      })).then((stats) => {
        const rows = stats.filter(Boolean);
        if (!rows.length) { pools.replaceChildren(h('div', { class: 'empty', text: 'No pools.' })); return; }
        pools.replaceChildren(h('table', { class: 'table' },
          h('thead', {}, h('tr', {}, ['model', 'policy', 'active', 'fallback', 'downgraded', 'backends'].map(c => h('th', { text: c })))),
          h('tbody', {}, rows.map(s => h('tr', { onclick: () => openResource('pool', s.model, {}) },
            h('td', { class: 'mono', text: s.model }),
            h('td', { text: s.policy || '—' }),
            h('td', { class: 'mono', text: s.active || '—' }),
            h('td', { text: s.fallback ? 'yes' : '—' }),
            h('td', {}, s.downgraded ? pill('yes', 'bad') : '—'),
            h('td', { class: 'mono', text: (s.backends || []).join(', ') }))))));
      });
    } catch (error) { toastError(error); }
  })();
}

// ---- invoke wrapper (shared by drawers) ----
async function invoke(fn) { try { await fn(); } catch (error) { toastError(error); } }

// ---- renderers map ----
const RENDERERS = {
  dashboard: () => renderDashboard(),
  provider: (arg) => renderResourceView('provider', arg),
  model: (arg) => renderResourceView('model', arg),
  pool: (arg) => renderResourceView('pool', arg),
  key: (arg) => renderResourceView('key', arg),
  rule: (arg) => renderResourceView('rule', arg),
  alias: (arg) => renderAliasView(arg),
  setting: (arg) => renderSettingsView(arg),
  codex: () => renderCodexView(),
  actions: () => renderActionsView(),
  spend: () => renderSpendView(),
};

// ---- boot ----
export function boot() {
  loadAuth();
  buildClient();
  $('#save').addEventListener('click', saveAuth);
  $('#check').addEventListener('click', checkAuth);
  $('#server').addEventListener('change', saveAuth);
  $('#authHeader').addEventListener('change', saveAuth);
  $('#scrim').addEventListener('click', closeDrawer);
  document.addEventListener('click', (e) => closeMultiSelects(e.target && e.target.closest ? e.target.closest('.multiselect') : null));
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') { closeMultiSelects(null); closeDrawer(); } });
  window.addEventListener('hashchange', route);
  route();
  checkAuth();
}
boot();
