CREATE TABLE players (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  token_hash  TEXT NOT NULL UNIQUE,
  role        TEXT NOT NULL DEFAULT 'player' CHECK (role IN ('player','admin')),
  created_at  TEXT NOT NULL
);

CREATE TABLE invites (
  code        TEXT PRIMARY KEY,
  role        TEXT NOT NULL DEFAULT 'player' CHECK (role IN ('player','admin')),
  created_at  TEXT NOT NULL,
  used_by     TEXT REFERENCES players(id)
);

CREATE TABLE requests (
  id            TEXT PRIMARY KEY,
  player_id     TEXT NOT NULL REFERENCES players(id),
  text          TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN
                 ('queued','planning','awaiting_clarification','awaiting_approval',
                  'materializing','applying_server','published','failed','rejected','infeasible')),
  plan_json     TEXT,
  clarification_question TEXT,
  clarification_answer   TEXT,
  clarification_rounds   INTEGER NOT NULL DEFAULT 0,
  error         TEXT,
  version       INTEGER,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE INDEX idx_requests_status ON requests(status);
CREATE INDEX idx_requests_created ON requests(created_at DESC);

CREATE TABLE versions (
  number        INTEGER PRIMARY KEY,
  created_at    TEXT NOT NULL,
  request_id    TEXT REFERENCES requests(id),
  git_commit    TEXT NOT NULL,
  summary       TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('published','superseded')),
  manifest_path TEXT NOT NULL
);

CREATE TABLE events (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  type        TEXT NOT NULL,
  payload     TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
