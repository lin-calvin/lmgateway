CREATE TABLE store_json (
  ns_key text PRIMARY KEY,
  data text NOT NULL,
  version integer NOT NULL DEFAULT 1,
  updated_at text NOT NULL
);

CREATE TABLE store_json_meta (
  id text PRIMARY KEY,
  version integer NOT NULL
);
INSERT INTO store_json_meta(id, version) VALUES ('global', 0);

CREATE TABLE store_ts (
  ts text NOT NULL,
  stream text NOT NULL,
  tags text NOT NULL,
  fields text NOT NULL
);
CREATE INDEX idx_store_ts_time ON store_ts (stream, ts DESC);
