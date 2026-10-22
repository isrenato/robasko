-- Drafted letters: where the file is, what it cited, and whether a human
-- has seen it.
--
-- The body is deliberately not a column. A letter is a biography - a name,
-- an employment history and the employer it is addressed to - and keeping
-- it on disk under `letters/` means there is one copy to delete rather
-- than two. The row records the path, so a lost file is visible as a row
-- pointing at nothing rather than as silence.

CREATE TABLE letters (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    vacancy_id uuid NOT NULL REFERENCES vacancies (id) ON DELETE CASCADE,
    -- Relative to the repository working tree, not absolute. An absolute
    -- path records the home directory of whoever ran the draft, which is
    -- personal data this table has no reason to hold.
    txt_path   text NOT NULL,
    -- Empty until the renderer has run. There is no python in the runtime
    -- image, so a draft without a PDF is an ordinary state, not a failure.
    pdf_path   text,
    -- Every inventory and profile id the letter cited (SPEC.md 13.3). This
    -- is the stored half of the fact lock: the draft is checked before it
    -- is written, and this is what makes the check auditable afterwards.
    facts_used text[] NOT NULL DEFAULT '{}',
    status     text NOT NULL DEFAULT 'draft'
               CHECK (status IN ('draft', 'needs_human', 'approved', 'discarded')),
    -- Why it needs a human, when it does: the one-page retry gave up, or
    -- the renderer was unavailable.
    reason     text,
    model      text,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- One live draft per vacancy. Redrafting replaces it; the file on disk
    -- is overwritten for the same reason.
    UNIQUE (vacancy_id)
);

CREATE INDEX letters_status_idx ON letters (status, created_at DESC);
