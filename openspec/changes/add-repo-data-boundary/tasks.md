# Tasks

## 1. Ignore rules and example artifacts

- [x] 1.1 Write `.gitignore` using the default-deny idiom (`*`, then `!*/`, then
      an allowlist block for the public tree from SPEC.md §4) and verify by
      inspection that the allowlist covers `cmd/`, `internal/`, `migrations/`,
      `Makefile`, `docker-compose.yml`, `README.md`, `go.mod`, `go.sum`,
      `.gitignore` itself, and the example artifacts
- [x] 1.2 Write `cv/example.md` with invented contents and verify it shows the
      section structure the inventory build will read
- [x] 1.3 Write `inventory.example.yaml` with invented entries covering every
      level in SPEC.md §10 and verify at least one `managed` and one `none`
      entry are present with their evidence fields
- [x] 1.4 Write `profile.example.yaml` with invented citable fact atoms and
      verify each entry carries a stable id usable by the fact lock

## 2. Repository initialisation and boundary verification

- [ ] 2.1 Run `git init` and commit `.gitignore` alone as the first commit;
      verify with `git log --name-only` that the first commit contains exactly
      that one path
- [x] 2.2 Plant a dummy file at each private location named in the spec (a CV,
      an inventory, a letter, a log, a `.env`) and verify `git status
      --porcelain` reports none of them and `git check-ignore -v` names the
      matching rule for each
- [x] 2.3 Create a Go source file under `internal/` and verify `git add -A
      --dry-run` stages it, confirming the allowlist admits the public tree
- [ ] 2.4 Delete the planted dummies and commit the remaining public tree;
      verify `git status --ignored --porcelain` shows no untracked path that
      should have been published

## 3. Data root resolution

- [x] 3.1 Run `go mod init github.com/isrenato/robasko` and verify `go build
      ./...` succeeds on an empty module
- [x] 3.2 Implement `internal/paths` resolving the private data root from
      `ROBASKO_DATA_DIR`, then `$XDG_DATA_HOME/robasko`, then
      `~/.local/share/robasko`, and verify unit tests cover all three branches
- [x] 3.3 Reject a resolved root that lies inside the repository working tree
      with a named error and verify a unit test asserts the failure and that
      the message contains the offending path
- [x] 3.4 Create the data root with owner-only permissions on first use and
      verify a test asserts mode `0700` on a freshly created root
- [x] 3.5 Document the data root, its three resolution branches and the backup
      caveat from design.md in README and verify the documented
      `ROBASKO_DATA_DIR` override behaves as written

## 4. Fixture scrubbing

- [x] 4.1 Implement `internal/scrub` replacing person names, email addresses,
      telephone numbers, postal addresses and company names with pseudonyms,
      and verify unit tests cover each category
- [x] 4.2 Derive pseudonyms deterministically via a keyed hash into a fixed
      pseudonym table, with the key read from the private data root, and verify
      a test asserts the same input yields the same pseudonym across two
      separate scrubber instances
- [x] 4.3 Confine substitution to message content fields, leaving tool names,
      schema keys and token counts untouched, and verify a test scrubs a
      representative fixture and asserts the structural fields are byte-identical
- [x] 4.4 Match telephone numbers insensitively to separators and grouping, and
      names insensitively to case and diacritics, and verify tests cover the
      variants named in design.md's risk section
- [x] 4.5 Document in README that fixtures are scrubbed at record time and must
      never be hand-edited to reintroduce real values

## 5. Pre-commit protected-literal scan

- [x] 5.1 Write the pre-commit hook reading its protected-literal list from the
      private data root and verify a staged file containing a literal is
      rejected with the offending path and matched rule on stderr
- [x] 5.2 Allow the commit with a warning when the literal list is absent, and
      verify a test in a clone without the list commits successfully and prints
      the warning
- [x] 5.3 Apply the same case-insensitive and separator-insensitive matching as
      the scrubber and verify a test rejects a literal written with different
      diacritics and a differently grouped phone number
- [x] 5.4 Add an installation step (`make hooks` or equivalent) and verify it
      activates the hook from a clean clone as documented in README

## 6. Integration verification

- [ ] 6.1 From a fresh clone of the repository, verify no file in the working
      tree contains any protected literal, by running the scan across all
      tracked paths rather than only staged ones
- [x] 6.2 Verify `go test ./...` passes with no network access, exercising the
      scrubbed fixtures through the replay path
- [ ] 6.3 Verify the committed example artifacts are the only CV, inventory and
      profile files tracked by git, and that the system reads its real
      counterparts from the data root
