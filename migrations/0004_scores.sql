-- What the model read off an advertisement, and the number computed from it.
--
-- Stored because a score has to be explainable after the fact: the
-- per-requirement table is the explanation, and without it a number in a
-- review queue is an opinion.

CREATE TABLE requirements (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    vacancy_id    uuid NOT NULL REFERENCES vacancies (id) ON DELETE CASCADE,
    seq           int  NOT NULL,          -- position in the advertisement
    text          text NOT NULL,          -- quoted, never paraphrased
    kind          text NOT NULL CHECK (kind IN ('must', 'nice')),
    category      text,
    -- Inventory ids this requirement was mapped onto. Empty means the
    -- inventory has no word for it, which scores zero and is reported
    -- separately as demand the inventory cannot name.
    canonical_ids text[] NOT NULL DEFAULT '{}',
    coverage      numeric NOT NULL DEFAULT 0 CHECK (coverage >= 0 AND coverage <= 1),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (vacancy_id, seq)
);

CREATE INDEX requirements_vacancy_idx ON requirements (vacancy_id);
-- Finding every advertisement that asked for a skill, for demand (SPEC.md 12).
CREATE INDEX requirements_canonical_idx ON requirements USING gin (canonical_ids);

-- SPEC.md 5 lists a `weight` column here. The split formula has no
-- per-requirement weight: must and nice are averaged over their own counts
-- and mixed at a fixed ratio, so a weight stored per row would be unused
-- and would invite someone to reintroduce the measurement of advertisement
-- length that the split was written to remove.

CREATE TABLE scores (
    vacancy_id    uuid PRIMARY KEY REFERENCES vacancies (id) ON DELETE CASCADE,
    score         numeric NOT NULL CHECK (score >= 0 AND score <= 100),
    must_coverage numeric NOT NULL DEFAULT 0,
    nice_coverage numeric NOT NULL DEFAULT 0,
    must_count    int NOT NULL DEFAULT 0,
    nice_count    int NOT NULL DEFAULT 0,
    capped_reason text,
    must_gaps     text[] NOT NULL DEFAULT '{}',
    nice_gaps     text[] NOT NULL DEFAULT '{}',
    model         text,
    -- SPEC.md 5 has inventory_version. The inventory is a hand-maintained
    -- file with no version field, and a number nobody increments is worse
    -- than no number. A digest of the file is recorded instead: it changes
    -- exactly when the inventory does, which is what the column is for.
    inventory_digest text,
    computed_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX scores_score_idx ON scores (score DESC);
