-- Companies and vacancies: the minimum needed to remember what has already
-- been seen. The runtime tables (agents, tools, tasks, task_steps,
-- tool_calls) arrive with the registry and the queue.

CREATE TABLE companies (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL,
    -- The spelling the advertisement used is kept in name; this is what
    -- dedup compares, with the legal form and punctuation removed.
    normalised_name text NOT NULL UNIQUE,
    city            text,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE vacancies (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source       text NOT NULL,
    source_id    text NOT NULL,
    url          text,
    title        text NOT NULL,
    company_id   uuid REFERENCES companies (id),
    city         text,
    remote       text NOT NULL DEFAULT 'unknown'
                 CHECK (remote IN ('onsite', 'hybrid', 'remote', 'unknown')),
    language     text CHECK (language IN ('de', 'en')),
    published_at date,
    raw_text     text,
    -- Hash of the normalised title, company and city. Unique, because the
    -- same post reaching us from two boards is one vacancy, not two.
    fingerprint  text NOT NULL UNIQUE,
    status       text NOT NULL DEFAULT 'new'
                 CHECK (status IN ('new', 'scored', 'letter_drafted', 'rejected', 'archived')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, source_id)
);

CREATE INDEX vacancies_status_created_at_idx ON vacancies (status, created_at DESC);
CREATE INDEX vacancies_company_idx ON vacancies (company_id);
