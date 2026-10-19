-- Vocabulary the matching depends on, kept as data rather than as literals
-- in Go: it grows every time an unfamiliar legal form or a new way of
-- softening a requirement turns up, and that should not need a rebuild.

CREATE TABLE reference_terms (
    kind       text NOT NULL,
    value      text NOT NULL,
    note       text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, value),
    CHECK (kind IN ('legal_form', 'connector', 'softener',
                    'lang_marker_de', 'lang_marker_en'))
);

COMMENT ON TABLE reference_terms IS
    'legal_form and connector feed the company fingerprint: changing them '
    'changes how already-stored vacancies would be identified. Every '
    'vacancy records the fingerprint_terms it was computed under so the '
    'drift is visible instead of silently producing duplicates.';

-- Values are stored as they look after normalisation: lowercased, folded,
-- without punctuation. That is the form they are compared against.
INSERT INTO reference_terms (kind, value) VALUES
    ('legal_form', 'gmbh co kg'), ('legal_form', 'ag co kg'),
    ('legal_form', 'gmbh co'),    ('legal_form', 'ug haftungsbeschraenkt'),
    ('legal_form', 'gmbh'),       ('legal_form', 'mbh'),
    ('legal_form', 'ug'),         ('legal_form', 'ag'),
    ('legal_form', 'se'),         ('legal_form', 'kg'),
    ('legal_form', 'ohg'),        ('legal_form', 'ev'),
    ('legal_form', 'ltd'),        ('legal_form', 'limited'),
    ('legal_form', 'inc'),        ('legal_form', 'llc'),
    ('legal_form', 'plc'),        ('legal_form', 'bv'),
    ('legal_form', 'nv'),         ('legal_form', 'sa'),
    ('legal_form', 'sarl'),       ('legal_form', 'oy'),
    ('legal_form', 'ab'),         ('legal_form', 'a s'),

    ('connector', 'und'), ('connector', 'and'), ('connector', 'et'),

    ('softener', 'idealerweise'),   ('softener', 'von vorteil'),
    ('softener', 'wuenschenswert'), ('softener', 'wünschenswert'),
    ('softener', 'pluspunkt'),      ('softener', 'gerne auch'),
    ('softener', 'bonus'),          ('softener', 'nice to have'),
    ('softener', 'a plus'),         ('softener', 'optional'),
    ('softener', 'desirable'),      ('softener', 'preferred'),
    ('softener', 'would be great'),

    ('lang_marker_de', ' und '),   ('lang_marker_de', ' oder '),
    ('lang_marker_de', ' nicht '), ('lang_marker_de', ' eine '),
    ('lang_marker_de', ' wir '),   ('lang_marker_de', ' sie '),
    ('lang_marker_de', ' mit '),   ('lang_marker_de', ' fuer '),
    ('lang_marker_de', ' für '),   ('lang_marker_de', ' bei '),
    ('lang_marker_de', ' unser '), ('lang_marker_de', ' werden '),
    ('lang_marker_de', ' haben '), ('lang_marker_de', ' sind '),

    ('lang_marker_en', ' and '),  ('lang_marker_en', ' or '),
    ('lang_marker_en', ' the '),  ('lang_marker_en', ' you '),
    ('lang_marker_en', ' we '),   ('lang_marker_en', ' with '),
    ('lang_marker_en', ' for '),  ('lang_marker_en', ' your '),
    ('lang_marker_en', ' our '),  ('lang_marker_en', ' will '),
    ('lang_marker_en', ' have '), ('lang_marker_en', ' are ');

-- Which vocabulary a row's fingerprint was computed under.
ALTER TABLE vacancies ADD COLUMN fingerprint_terms text;
