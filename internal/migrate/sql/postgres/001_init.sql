CREATE TABLE store_json (
  ns_key text PRIMARY KEY,
  data jsonb NOT NULL,
  version bigint NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_store_json_data ON store_json USING gin (data jsonb_path_ops);

CREATE TABLE store_json_meta (
  id text PRIMARY KEY,
  version bigint NOT NULL
);
INSERT INTO store_json_meta(id, version) VALUES ('global', 0);

CREATE TABLE store_ts (
  ts timestamptz NOT NULL,
  stream text NOT NULL,
  tags jsonb NOT NULL,
  fields jsonb NOT NULL
);
CREATE INDEX idx_store_ts_time ON store_ts (stream, ts DESC);
CREATE INDEX idx_store_ts_tags ON store_ts USING gin (tags jsonb_path_ops);
