# Design

## Context

See proposal.md — Why. The relevant current state: the project has no `.git`
directory, no `.gitignore` and no commits, so no history rewrite is needed if
the boundary lands first. SPEC.md §20 commits the repository to being public;
§1 makes the code itself the portfolio evidence, so the public tier has to stay
substantial rather than being hollowed out to achieve privacy.

The constraint that shapes most of what follows is that three different
mechanisms are needed because the data arrives by three different routes:
artifacts the system writes at runtime (letters, caches, logs), artifacts a
human places in the tree (the CV, the inventory), and artifacts that must be
committed yet are derived from private input (LLM fixtures).

## Goals / Non-Goals

**Goals:**

- Make the boundary a property of the filesystem layout, so that the common
  case requires no discipline.
- Keep the public tier legible as a work sample, including the data formats.
- Fail closed: an unclassified or newly added artifact is private by default.

**Non-Goals:**

- Encryption at rest. The private tier relies on filesystem permissions.
- A retention or erasure policy for the third-party contact data in the
  `contacts` table. Keeping it out of git is in scope here; how long it is kept
  and on what basis is a separate concern and a separate change.
- Secret management beyond keeping `.env` untracked. No vault, no KMS.
- Scrubbing data already published elsewhere. Nothing has been committed yet.

## Decisions

### Private data lives outside the working tree, not in an ignored subdirectory

The alternative — a `data/` directory inside the repository, listed in
`.gitignore` — was the first instinct and is weaker. It keeps personal data one
`git add -f`, one misconfigured tool, or one careless `.gitignore` edit away
from being committed, and it puts the data inside any archive made of the
directory. Resolving the root to `$XDG_DATA_HOME/robasko` means git cannot see
the data at all, which removes the failure mode rather than guarding it.

The cost is that a reader cannot see example data in situ, which the committed
example artifacts address, and that the data root must be resolved through a
single helper rather than a relative path. The spec requires the resolved root
to be rejected if it falls inside the working tree, so a misconfigured
`ROBASKO_DATA_DIR` fails loudly at startup instead of silently reintroducing
the problem.

### Default-deny `.gitignore` rather than a conventional deny-list

A conventional `.gitignore` enumerates what to exclude, so a path nobody
anticipated is tracked by default. In this repository the expensive error is
inclusion, not omission, so the polarity is inverted: ignore `*`, then
allowlist the public tree.

The idiom requires `!*/` so that git still descends into directories;
without it the allowlist entries below the first level never match. The
trade-off is maintenance — every new top-level directory needs an allowlist
entry, and a contributor who adds a file and finds it missing from
`git status` has to learn why. Given that the repository has a known, mostly
fixed layout taken from SPEC.md §4, the cost is small and paid rarely.

### Fixtures are scrubbed when recorded, not when committed

Scrubbing at commit time means real personal data exists on disk in the
repository between recording and committing, and that the scrubber's coverage
is enforced by a check that can be bypassed. Scrubbing in the recorder means
the unscrubbed form is never written, so there is nothing to catch.

Pseudonyms must be stable across a fixture set, otherwise a replayed
conversation becomes internally inconsistent — an ad naming a contact in one
turn and the letter addressing a different pseudonym in the next. The mapping
is therefore derived deterministically from the real value (keyed hash into a
fixed pseudonym table) rather than randomly generated per occurrence. The key
lives in the private data root, so the mapping cannot be reversed by a reader
of the public repository.

Scrubbing applies only to recorded message content, never to structural fields
such as tool names, schema keys or token counts, so fixtures stay faithful to
the shape of the real exchange.

### The protected-literal list is itself private

The pre-commit scan needs to know which strings to reject: the maintainer's
name, email addresses, phone number, home locality. Committing that list to the
public repository would publish exactly the data the scan exists to protect.

Hashing the literals and committing the hashes does not work either. These are
low-entropy values — a given name, a village of two thousand people — so a
dictionary attack recovers them immediately. Hashing would provide the
appearance of protection without the substance.

The list therefore lives in the private data root as plain text, and the hook
reads it from there. A clone without the list emits a warning and allows the
commit, since a third party cloning the public repository has no protected
literals to check against and should not be blocked.

### SPEC.md stays private; README is the public face

As written, SPEC.md names the maintainer, their home village, their employers
and their self-assessed skill levels, including one explicitly unflattering
line. It
is classified private, and its architectural content reaches the public tier
through README, which SPEC.md §20 already requires to explain the registry, the
nesting limits, the handover protocol, the scoring formula with its caps, and
the fact lock.

The alternative considered was publishing a genericised SPEC.md with the
personal references removed. It was rejected as duplicated effort: the
genericised document and the required README would carry nearly the same
content and drift apart. Publishing SPEC.md unchanged was rejected because the
locality and the self-assessments are precisely the data this change exists to
keep out.

**This decision is reversible and is the one call in this change made without
an explicit answer from the maintainer.** Flipping it to "genericise and
publish" changes one task and no requirements.

## Risks / Trade-offs

- **Default-deny ignore rules confuse a future contributor; a file silently
  fails to appear in `git status`** → the allowlist is a single commented block
  at the top of `.gitignore`, and README documents the polarity in one line.

- **The scrubber misses a personal value whose form was not anticipated** —
  a name written with different diacritics, a phone number in an unusual
  grouping → the pre-commit scan is the second layer and matches
  case-insensitively with separator-insensitive number matching; neither layer
  is relied on alone.

- **The scrubber damages fixture semantics by replacing a string that mattered
  structurally** → scrubbing is confined to message content fields, and the
  replay tests in SPEC.md §18 exercise scrubbed fixtures, so a damaging
  substitution fails the suite rather than reaching CI unnoticed.

- **The private data root is not covered by the maintainer's backups** —
  `~/.local/share` is excluded by many default backup configurations, and
  generated letters and the hand-written inventory are expensive to recreate →
  documented in README, with `ROBASKO_DATA_DIR` available to relocate the root
  to a backed-up path.

- **Third-party contact data remains a live concern after this change.** The
  `contacts` table holds names, emails and phone numbers of people who were
  never asked. This change keeps that data out of git; it does not establish a
  lawful basis, a retention period or an erasure path, and is not a substitute
  for deciding those.

- **A reader of the public repository sees a project whose central data files
  are all examples** → the example artifacts are realistic rather than
  skeletal, so the inventory's level scheme and the fact-lock's atom shape are
  fully legible.

## Migration Plan

1. Write `.gitignore` and the example artifacts before running `git init`.
2. `git init`, then stage and commit `.gitignore` alone as the first commit, so
   the default-deny rules are in effect for every subsequent `git add`.
3. Verify with `git status --ignored` and a dry-run `git add -A` that no path
   outside the allowlist is staged, and that a planted dummy file in each
   private location is ignored.
4. Create the private data root, move any existing personal files into it, and
   install the pre-commit hook.
5. Commit the remainder of the public tree.

Rollback is `rm -rf .git` while the repository is unpublished; after the first
push, rollback is no longer meaningful, which is why steps 1 to 3 precede any
remote being added.

## Open Questions

- Whether the generated-PDF output path should default to the private data root
  or to a user-visible location such as `~/Documents`. Both satisfy the spec,
  and the choice can be made when `letter.draft` is implemented.
