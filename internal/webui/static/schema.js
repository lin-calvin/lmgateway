// schema.js — declarative descriptors for each CLI resource group.
//
// These describe only presentation (columns + editable fields); the verbs still
// map 1:1 onto the CLI's list/get/set/patch/delete/reset in api.js.

// Field types: text | password | number | checkbox | tristate | list | json |
//              select | checklist | datalist
// `source` pulls options from an enumerator (see loadEnum in app.js):
//              providers | models | pools | targets | groups | positions | actions
// Meta keys returned by GET that are not part of the editable document.
export const META_KEYS = ['name', 'source', 'version', 'has_api_key', 'has_key_hash'];

export const PROVIDER_TYPES = ['openai', 'openai_response', 'chatgpt_codex', 'commandcode'];

export const RESOURCES = {
  provider: {
    kind: 'provider',
    title: 'Providers',
    idField: 'name',
    createLabel: 'New provider',
    columns: ['name', 'type', 'base_url', 'source', 'version'],
    fields: [
      { name: 'type', label: 'Type', type: 'select', required: true, options: PROVIDER_TYPES },
      { name: 'base_url', label: 'Base URL', type: 'text', required: true, placeholder: 'https://api.example.com/v1' },
      { name: 'api_key', label: 'API key', type: 'password', placeholder: 'leave blank to keep unchanged' },
      { name: 'api_key_env', label: 'API key env', type: 'text' },
      { name: 'timeout_sec', label: 'Timeout (s)', type: 'number' },
      { name: 'originator', label: 'Originator', type: 'text' },
      { name: 'discover', label: 'Discover models', type: 'checkbox' },
      { name: 'forward_client_metadata', label: 'Forward client metadata', type: 'checkbox' },
      { name: 'public', label: 'Public', type: 'tristate' },
      { name: 'allowed_groups', label: 'Allowed groups', type: 'checklist', source: 'groups' },
      { name: 'denied_groups', label: 'Denied groups', type: 'checklist', source: 'groups' },
      { name: 'options', label: 'Options', type: 'json' },
    ],
  },
  model: {
    kind: 'model',
    title: 'Models',
    idField: 'name',
    createLabel: 'New model',
    columns: ['name', 'provider', 'upstream_model', 'source', 'version'],
    fields: [
      { name: 'provider', label: 'Provider', type: 'select', source: 'providers', required: true },
      { name: 'upstream_model', label: 'Upstream model', type: 'text' },
      { name: 'default', label: 'Default (fallback)', type: 'checkbox' },
      { name: 'input_per_mtok', label: 'Input $/Mtok', type: 'number' },
      { name: 'output_per_mtok', label: 'Output $/Mtok', type: 'number' },
      { name: 'probe_max_tokens', label: 'Probe max tokens', type: 'number' },
      { name: 'public', label: 'Public', type: 'tristate' },
      { name: 'allowed_groups', label: 'Allowed groups', type: 'checklist', source: 'groups' },
      { name: 'denied_groups', label: 'Denied groups', type: 'checklist', source: 'groups' },
      { name: 'extra_body', label: 'Extra body', type: 'json' },
      { name: 'default_extra_body', label: 'Default extra body', type: 'json' },
    ],
  },
  pool: {
    kind: 'pool',
    title: 'Pools',
    idField: 'model',
    createLabel: 'New pool',
    columns: ['model', 'backend', 'policy', 'source', 'version'],
    fields: [
      { name: 'backend', label: 'Backends (ordered)', type: 'checklist', source: 'models', required: true },
      { name: 'policy', label: 'Policy', type: 'select', options: ['speed_first', 'price_first'] },
      { name: 'on_all_limited', label: 'On all limited', type: 'select', options: ['fail', 'force-least-recent'] },
      { name: 'cooldown_sec', label: 'Cooldown (s)', type: 'number' },
      { name: 'reselect_ttft_ms', label: 'Reselect TTFT (ms)', type: 'number' },
      { name: 'probe_interval_min', label: 'Probe interval (min)', type: 'number' },
      { name: 'fallback', label: 'Fallback', type: 'select', source: 'targets' },
      { name: 'allowed_groups', label: 'Allowed groups', type: 'checklist', source: 'groups' },
      { name: 'denied_groups', label: 'Denied groups', type: 'checklist', source: 'groups' },
    ],
  },
  key: {
    kind: 'key',
    title: 'Keys',
    idField: 'id',
    createLabel: 'Create key',
    // key creation goes through the key/create action, not the entity set verb.
    creator: 'key',
    columns: ['id', 'groups', 'status', 'source', 'version'],
    fields: [
      { name: 'status', label: 'Status', type: 'select', options: ['active', 'disabled'] },
      { name: 'groups', label: 'Groups', type: 'checklist', source: 'groups' },
      { name: 'expires_at', label: 'Expires at (RFC3339)', type: 'text' },
    ],
  },
  rule: {
    kind: 'rule',
    title: 'Rules',
    idField: 'id',
    createLabel: 'New rule',
    prefix: 'api/config/rules',
    columns: ['id', 'from', 'action', 'to'],
    fields: [
      { name: 'id', label: 'ID', type: 'text', required: true, idField: true },
      { name: 'from', label: 'From', type: 'select', source: 'positions', required: true, placeholder: 'http' },
      { name: 'action', label: 'Action', type: 'datalist', source: 'actions' },
      { name: 'to', label: 'To', type: 'datalist', source: 'positions' },
      { name: 'match', label: 'Match', type: 'json' },
      { name: 'set', label: 'Set (hard)', type: 'json' },
      { name: 'default', label: 'Default (soft)', type: 'json' },
      { name: 'allowed_groups', label: 'Allowed groups', type: 'checklist', source: 'groups' },
      { name: 'denied_groups', label: 'Denied groups', type: 'checklist', source: 'groups' },
    ],
  },
};

export const SETTING_NAMES = ['server', 'spend', 'logprobs'];

export const SETTINGS = {
  server: {
    title: 'Server',
    columns: ['name', 'source', 'version'],
    fields: [
      { name: 'addr', label: 'Listen addr', type: 'text', placeholder: ':8080' },
      { name: 'master_key', label: 'Master key', type: 'password', note: 'write-only; never returned' },
    ],
  },
  spend: {
    title: 'Spend',
    columns: ['name', 'source', 'version'],
    fields: [
      { name: 'raw_retention_days', label: 'Raw retention (days)', type: 'number' },
      { name: 'rollup_dimensions', label: 'Rollup dimensions', type: 'list' },
      { name: 'timezone', label: 'Timezone', type: 'text', placeholder: 'UTC' },
    ],
  },
  logprobs: {
    title: 'Logprobs',
    columns: ['name', 'source', 'version'],
    fields: [
      { name: 'webhook_url', label: 'Webhook URL', type: 'text' },
      { name: 'timeout_sec', label: 'Timeout (s)', type: 'number' },
    ],
  },
};

export const EFFORT_LEVELS = ['none', 'low', 'medium', 'high', 'max'];
export const SUMMARY_VALUES = ['auto', 'concise', 'detailed'];
