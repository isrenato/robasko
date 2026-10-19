-- The runtime: who may call what, what was called, and what is queued.
--
-- Agents are rows rather than configuration because the scope check needs
-- something to check against, and the call log needs something to attribute
-- a call to.

CREATE TABLE agents (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    model      text,
    scopes     text[] NOT NULL DEFAULT '{}',
    token_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A tool is a name, a version, its schemas, the scopes required to call it,
-- and a timeout. Registering a changed tool bumps the version; the old
-- version stays callable, because a task in flight pinned it.
CREATE TABLE tools (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL,
    version         int  NOT NULL CHECK (version > 0),
    description     text,
    input_schema    jsonb NOT NULL,
    output_schema   jsonb NOT NULL,
    required_scopes text[] NOT NULL DEFAULT '{}',
    timeout_ms      int NOT NULL DEFAULT 5000 CHECK (timeout_ms > 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

CREATE TABLE tasks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    trace_id    text NOT NULL,
    goal        text NOT NULL,
    subject_id  uuid,                      -- the vacancy, when there is one
    created_by  uuid REFERENCES agents (id),
    status      text NOT NULL DEFAULT 'planning'
                CHECK (status IN ('planning', 'running', 'done', 'failed')),
    reason      text,                      -- why it ended, when it failed
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

CREATE TABLE task_steps (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id      uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    seq          int  NOT NULL,
    tool_name    text NOT NULL,
    tool_version int,                      -- NULL means latest at claim time
    input        jsonb NOT NULL,
    status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'claimed', 'done', 'failed', 'dead')),
    claimed_by   uuid REFERENCES agents (id),
    claimed_at   timestamptz,
    attempts     int NOT NULL DEFAULT 0,
    result       jsonb,
    error_code   text,
    available_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, seq)
);

-- The claim query orders by this, and it is the hot path.
CREATE INDEX task_steps_claimable_idx
    ON task_steps (status, available_at, task_id, seq);

-- One row per tool call, nested calls included, so a trace id reconstructs
-- the whole tree: the fetch, the handover, the nested chain, the render.
CREATE TABLE tool_calls (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    trace_id       text NOT NULL,
    task_id        uuid REFERENCES tasks (id) ON DELETE SET NULL,
    step_id        uuid REFERENCES task_steps (id) ON DELETE SET NULL,
    parent_call_id uuid REFERENCES tool_calls (id),
    depth          int  NOT NULL DEFAULT 0 CHECK (depth >= 0),
    agent_id       uuid REFERENCES agents (id),
    tool_name      text NOT NULL,
    tool_version   int,
    input          jsonb,
    output         jsonb,
    status         text NOT NULL DEFAULT 'running'
                   CHECK (status IN ('running', 'ok', 'error')),
    error_code     text,
    input_tokens   int,
    output_tokens  int,
    idempotency_key text,
    started_at     timestamptz NOT NULL DEFAULT now(),
    duration_ms    int
);

CREATE INDEX tool_calls_trace_idx ON tool_calls (trace_id, started_at);
CREATE INDEX tool_calls_task_idx ON tool_calls (task_id);
CREATE INDEX tool_calls_parent_idx ON tool_calls (parent_call_id);

-- A finished call can be replayed by key instead of re-running the handler.
-- Partial, because only finished calls may be replayed.
CREATE UNIQUE INDEX tool_calls_idempotency_idx
    ON tool_calls (idempotency_key)
    WHERE idempotency_key IS NOT NULL AND status <> 'running';
