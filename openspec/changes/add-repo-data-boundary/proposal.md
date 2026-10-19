# Proposal

## Why

SPEC.md §20 makes this repository public under `github.com/isrenato`, while the
system it describes collects and stores personal data: the maintainer's CV,
profile and self-assessed skill inventory, generated cover letters, and — in
the `contacts` table — the names, emails and phone numbers of recruiters
harvested from job ads. There is currently no boundary between the two. The
repository has not yet been initialised with git and has no `.gitignore`, so
the boundary can be established before anything enters history rather than
rewritten out of it later.

## What Changes

- **A three-tier classification** for every artifact the project produces:
  public (committed, and serving as the portfolio evidence §1 calls for),
  private (personal data, never committed), and generated (reproducible,
  ignored).
- **A data root outside the repository.** Private artifacts — the real CV,
  inventory, profile, letters, raw ad cache, logs and database dumps — are
  written to `$XDG_DATA_HOME/robasko` (falling back to `~/.local/share/robasko`)
  rather than any path inside the working tree. This makes the boundary a
  property of the layout instead of a rule that must be remembered.
- **A default-deny `.gitignore`.** The repository ignores everything and
  allowlists the public tree, so a new file is invisible to git until it is
  explicitly published. The failure direction is omission, not disclosure.
- **Scrubbing at fixture-record time.** SPEC.md §16 and §18 require committed
  LLM fixtures so CI runs without network. Those fixtures are recorded against
  real ads and real letters, which makes them the one path where personal data
  is *supposed* to be committed. The recorder replaces person names, emails,
  phone numbers and company names with stable pseudonyms as it writes, so an
  unscrubbed fixture cannot be produced in the first place.
- **A pre-commit scan** for the small set of literals that should never appear
  in a commit, as a last line behind the three structural defenses.
- **Committed example artifacts** — `cv/example.md`,
  `inventory.example.yaml`, `profile.example.yaml` — carrying realistic but
  invented contents. The inventory's design (the `managed` and `none` levels of
  §10) is the most interesting thing to show and the most costly to fill in
  truthfully in public; publishing the shape resolves both.
- **A decision on SPEC.md itself.** As written it names the maintainer, their
  home village, their employers and their candid self-assessments. Design
  records the options and the chosen disposition; see design.md.

## Capabilities

### New Capabilities
- `data-boundary`: Which artifacts the project may publish and which it must
  keep private, where each kind is stored, and the mechanisms that keep
  personal data out of version control — the data root, the default-deny
  ignore rules, fixture scrubbing, and the pre-commit scan.

### Modified Capabilities
<!-- None. This is the project's first capability. -->

## Impact

- **New**: `.gitignore` (default-deny), `cv/example.md`,
  `inventory.example.yaml`, `profile.example.yaml`, the pre-commit hook and its
  installation step.
- **Constrains future code**: every component that writes a file — `letter.draft`
  (§13), the raw-response cache (§15), `internal/obs` logging (§17), and the
  planned `agentctl cv add` — must resolve its output path through the data
  root rather than hardcoding a path relative to the repository.
- **Constrains `internal/llm`**: the fake client's fixture recorder (§16) gains
  a scrubbing stage; existing fixture format is unaffected.
- **No runtime dependencies added.** The data root is resolved from environment
  variables; the pre-commit hook is shell.
- **Precedes git init.** This change should land before the first commit, so
  that no history rewrite is required.
