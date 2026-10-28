// api.js — a browser port of internal/lmgcli/client.go.
//
// Every operation below maps to the same method + path the CLI uses, so the
// console and `lmgcli` share one operation vocabulary (health, resource, rule,
// pool runtime, action, key create, codex, spend, time-series).

export class HTTPError extends Error {
  constructor(status, body) {
    const message = describe(status, body);
    super(message);
    this.name = 'HTTPError';
    this.status = status;
    this.body = body;
  }
}

function describe(status, body) {
  if (body != null) {
    if (typeof body === 'object') {
      if (typeof body.error === 'string') return `server returned ${status}: ${body.error}`;
      if (typeof body.message === 'string') return `server returned ${status}: ${body.message}`;
    }
    return `server returned ${status}: ${typeof body === 'string' ? body : JSON.stringify(body)}`;
  }
  return `server returned ${status}`;
}

export class Client {
  // server: "" => same origin; otherwise an absolute base URL.
  constructor({ server = '', apiKey = '', authHeader = 'Authorization', timeout = 30000 } = {}) {
    this.server = String(server || '').replace(/\/+$/, '');
    this.apiKey = apiKey;
    this.authHeader = authHeader;
    this.timeout = timeout;
  }

  // do mirrors Client.Do: JSON body in, parsed JSON out, HTTPError on non-2xx.
  async do(method, path, body, ifMatch) {
    const headers = {};
    if (body !== undefined && body !== null) headers['Content-Type'] = 'application/json';
    if (this.apiKey) {
      if (this.authHeader === 'X-API-Key') headers['X-API-Key'] = this.apiKey;
      else headers['Authorization'] = `Bearer ${this.apiKey}`;
    }
    if (ifMatch) headers['If-Match'] = String(ifMatch);

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeout);
    let response;
    try {
      response = await fetch(this.server + '/' + String(path).replace(/^\/+/, ''), {
        method,
        headers,
        body: body === undefined || body === null ? undefined : JSON.stringify(body),
        signal: controller.signal,
      });
    } catch (error) {
      clearTimeout(timer);
      throw new Error(error.name === 'AbortError' ? `request timed out after ${this.timeout}ms` : String(error));
    }
    clearTimeout(timer);

    const text = await response.text();
    let data;
    if (text.length === 0) data = {};
    else {
      try { data = JSON.parse(text); }
      catch { data = text; }
    }
    if (!response.ok) throw new HTTPError(response.status, data);
    return data;
  }

  get(path) { return this.do('GET', path, null, null); }
  post(path, body) { return this.do('POST', path, body, null); }

  health() { return this.get('healthz'); }

  // resource mirrors Client.Resource; the `setting` kind lives under a
  // different prefix, exactly as in the CLI.
  resource(kind, operation, name, body, ifMatch) {
    const prefix = kind === 'setting' ? 'api/config/settings' : `api/${kind}`;
    switch (operation) {
      case 'list': return this.do('GET', `${prefix}/list`, null, null);
      case 'get': return this.do('GET', `${prefix}/get/${encodeURIComponent(name)}`, null, null);
      case 'set': return this.do('PUT', `${prefix}/set/${encodeURIComponent(name)}`, body ?? {}, ifMatch);
      case 'patch': return this.do('PATCH', `${prefix}/patch/${encodeURIComponent(name)}`, body ?? {}, ifMatch);
      case 'delete': return this.do('DELETE', `${prefix}/delete/${encodeURIComponent(name)}`, null, null);
      case 'reset': return this.do('POST', `${prefix}/reset/${encodeURIComponent(name)}`, null, null);
      default: throw new Error(`unknown resource operation ${operation}`);
    }
  }

  rule(operation, id, body, ifMatch) {
    if (operation === 'meta') return this.get('api/config/rules/meta');
    if (operation === 'list') return this.get('api/config/rules/list');
    const method = { get: 'GET', set: 'PUT', patch: 'PATCH', delete: 'DELETE', reset: 'POST' }[operation];
    if (!method) throw new Error(`unknown rule operation ${operation}`);
    return this.do(method, `api/config/rules/${operation}/${encodeURIComponent(id)}`, body ?? (method === 'GET' ? null : {}), ifMatch);
  }

  poolStatus(model) { return this.get(`api/pool/status/${encodeURIComponent(model)}`); }
  poolRotate(model) { return this.post(`api/pool/rotate/${encodeURIComponent(model)}`); }
  poolClear(model) { return this.post(`api/pool/clear/${encodeURIComponent(model)}`); }
  poolRecover(model, backend) { return this.post(`api/pool/recover/${encodeURIComponent(model)}`, { backend }); }

  action(name, body) { return this.post(`api/action/${String(name).replace(/^\/+/, '')}`, body ?? null); }

  codexStatus() { return this.get('api/action/codex/status'); }
  codexDeviceStart() { return this.post('api/action/codex/login/device/start'); }
  codexDevicePoll(requestId) { return this.post('api/action/codex/login/device/poll', { request_id: requestId }); }
  codexLogout() { return this.post('api/action/codex/logout'); }

  spend(time = 'today') { return this.get(`api/spend/token?time=${encodeURIComponent(time)}`); }
  spendTags(time = 'today') { return this.get(`api/spend/tags?time=${encodeURIComponent(time)}`); }
  spendSeries(time = 'today') { return this.get(`api/spend/series?time=${encodeURIComponent(time)}`); }

  tsQuery(stream, params = {}) {
    const qs = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) if (v !== '' && v != null) qs.set(k, v);
    return this.get(`api/ts/${encodeURIComponent(stream)}/query?${qs}`);
  }

  tsAgg(stream, params = {}) {
    const qs = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) if (v !== '' && v != null) qs.set(k, v);
    return this.get(`api/ts/${encodeURIComponent(stream)}/agg?${qs}`);
  }
}

// ---- alias helpers (mirror cmd/lmgcli/alias.go) ----

export const aliasRulePrefix = 'alias-';

export const aliasKeyPaths = {
  effort: 'reasoning.effort',
  summary: 'reasoning.summary',
  temperature: 'temperature',
  top_p: 'top_p',
  max_tokens: 'max_tokens',
};

export const aliasKeys = ['model', 'effort', 'summary', 'temperature', 'top_p', 'max_tokens'];

export function aliasRuleID(name) {
  return String(name).startsWith(aliasRulePrefix) ? name : aliasRulePrefix + name;
}

// listAliases derives aliases from the rule list (id alias-<name>).
export async function listAliases(client) {
  const payload = await client.rule('list');
  const out = [];
  for (const rule of payload.rules || []) {
    const id = rule.id || '';
    if (!id.startsWith(aliasRulePrefix)) continue;
    const item = { name: id.slice(aliasRulePrefix.length), ruleID: id };
    if (rule.set && rule.set.model != null) item.model = rule.set.model;
    if (rule.default && Object.keys(rule.default).length > 0) item.default = rule.default;
    out.push(item);
  }
  out.sort((a, b) => a.name.localeCompare(b.name));
  return out;
}

// aliasAssignments turns KEY VALUE pairs into hard `set` and soft `default`
// maps, with the same vocabulary as the CLI.
export function aliasAssignments(pairs) {
  const sets = {};
  const defaults = {};
  for (let i = 0; i + 1 < pairs.length; i += 2) {
    const key = pairs[i];
    const value = pairs[i + 1];
    if (key === 'model') { sets.model = parseScalar(value); continue; }
    let path = aliasKeyPaths[key];
    if (!path) {
      if (!key.includes('.')) {
        throw new Error(`unknown alias key "${key}" (known: ${aliasKeys.join(', ')}, or a dotted path)`);
      }
      path = key;
    }
    defaults[path] = parseScalar(value);
  }
  return { sets, defaults };
}

export function aliasRuleBody(ruleID, aliasName, sets, defaults) {
  const body = {
    id: ruleID,
    from: 'http',
    match: [{ field: 'model', op: 'eq', value: aliasName }],
    set: sets,
    to: 'http',
  };
  if (defaults && Object.keys(defaults).length > 0) body.default = defaults;
  return body;
}

function parseScalar(value) {
  try { return JSON.parse(value); } catch { return value; }
}
