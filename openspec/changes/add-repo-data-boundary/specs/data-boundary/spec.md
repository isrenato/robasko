# Spec Delta

## Purpose

Defines which artifacts this project may publish and which it must keep
private, where each kind is stored, and the mechanisms that keep personal
data — the maintainer's own and that of third parties recorded from job ads —
out of
version control in a repository that is published publicly.

## ADDED Requirements

### Requirement: Artifact classification
Every artifact the system produces or consumes SHALL belong to exactly one of
three tiers: public (committed), private (never committed), or generated
(reproducible, not committed). An artifact whose tier is undetermined SHALL be
treated as private.

#### Scenario: Personal artifact is classified private
- **WHEN** the real CV, profile, skills inventory, a generated cover letter, a
  cached raw job advertisement, a log file, or a database dump is produced
- **THEN** it is classified private
- **AND** it is not present in the repository working tree

#### Scenario: Source code is classified public
- **WHEN** source files, migrations, the Makefile, compose files, the README or
  committed example artifacts are produced
- **THEN** they are classified public and are tracked by version control

#### Scenario: Unclassified artifact defaults to private
- **WHEN** a component writes an artifact whose tier has not been declared
- **THEN** the artifact is treated as private and written beneath the private
  data root

### Requirement: Private data root outside the working tree
The system SHALL write every private artifact beneath a data root located
outside the repository working tree. The root SHALL resolve to
`$XDG_DATA_HOME/robasko` when `XDG_DATA_HOME` is set, and to
`~/.local/share/robasko` otherwise. An explicit override SHALL be honoured via
`ROBASKO_DATA_DIR`.

#### Scenario: Default resolution without XDG_DATA_HOME
- **WHEN** a private artifact is written and `XDG_DATA_HOME` is unset
- **THEN** the artifact's path is beneath `~/.local/share/robasko`

#### Scenario: XDG_DATA_HOME is honoured
- **WHEN** `XDG_DATA_HOME` is set to a directory
- **THEN** the artifact's path is beneath `<XDG_DATA_HOME>/robasko`

#### Scenario: Explicit override takes precedence
- **WHEN** `ROBASKO_DATA_DIR` is set
- **THEN** the artifact's path is beneath that directory regardless of
  `XDG_DATA_HOME`

#### Scenario: Resolved root is rejected when inside the working tree
- **WHEN** the resolved data root lies inside the repository working tree
- **THEN** the system refuses to start and reports the offending path

### Requirement: Default-deny version control rules
The repository SHALL ignore all paths by default and track only paths named by
an explicit allowlist, so that a newly created file is untracked until it is
deliberately published.

#### Scenario: New unlisted file is not staged
- **WHEN** a file is created at a path not covered by the allowlist
- **AND** all changes are staged
- **THEN** the file is not among the staged paths

#### Scenario: Allowlisted source file is staged
- **WHEN** a Go source file is created beneath an allowlisted source directory
- **AND** all changes are staged
- **THEN** the file is among the staged paths

#### Scenario: Example artifacts are published
- **WHEN** the committed example CV, inventory and profile artifacts are staged
- **THEN** they are tracked, while their real counterparts remain untracked

### Requirement: Fixtures are scrubbed at record time
Recorded language-model fixtures are committed so that tests run without
network access. The recorder SHALL replace person names, email addresses,
telephone numbers, postal addresses and company names with stable pseudonyms
before writing a fixture, so that an unscrubbed fixture cannot be produced.

#### Scenario: Personal data is replaced during recording
- **WHEN** a fixture is recorded from an exchange containing a contact name,
  email address and telephone number
- **THEN** the written fixture contains none of those values
- **AND** it contains pseudonyms in their place

#### Scenario: Pseudonyms are stable across a fixture set
- **WHEN** the same real value appears in two recorded fixtures
- **THEN** the same pseudonym is written in both

#### Scenario: Replay is unaffected by scrubbing
- **WHEN** a scrubbed fixture is replayed by the fake client
- **THEN** the test consuming it passes without network access

### Requirement: Pre-commit protected-literal scan
A pre-commit check SHALL reject a commit whose staged content contains any
protected literal — the maintainer's name, email addresses, telephone numbers,
home locality, or the configured pseudonymisation targets.

#### Scenario: Commit carrying a protected literal is rejected
- **WHEN** a staged file contains a protected literal
- **AND** a commit is attempted
- **THEN** the commit is rejected
- **AND** the offending path and matched rule are reported

#### Scenario: Clean commit proceeds
- **WHEN** no staged file contains a protected literal
- **THEN** the commit proceeds

#### Scenario: Check is installable from a clean clone
- **WHEN** the repository is cloned and the documented setup step is run
- **THEN** the check is active for subsequent commits

### Requirement: Published example artifacts
The repository SHALL publish an example CV, skills inventory and profile
carrying invented contents, so that the data formats and the inventory's level
scheme are legible to a reader without disclosing the maintainer's real CV,
biography or self-assessed skill gaps.

#### Scenario: Examples demonstrate the format
- **WHEN** a reader opens the committed example inventory
- **THEN** it shows the level scheme including the `managed` and `none` levels
- **AND** every entry's contents are invented

#### Scenario: Examples are not consumed as real input
- **WHEN** the system builds or reads the skills inventory
- **THEN** it reads the private inventory beneath the data root, not the
  committed example
