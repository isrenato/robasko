# robasko

A job-search agent system in Go: a tool registry with JSON Schema validation,
a runtime where tools can call other tools under depth, budget and cycle
limits, and agents that hand work over through a queue. It finds vacancies
through public job APIs, scores them against a structured skills inventory,
and drafts a cover letter when the score clears a threshold.

The project is early. The data boundary described below is in place; the
runtime is not.

## How it works

Specified in full. Some of it runs: the registry, the scoring engine and
requirement extraction are written, the runtime and the queue are not. See
**Status** at the end for which is which.

### Tool registry

A tool is a name, a version, JSON Schemas for its input and output, the
scopes required to call it, and a timeout. Schemas must set
`"additionalProperties": false` and list `required`.

Input is validated before the handler runs and output after it returns. A
violation becomes a `tool_result` with `is_error: true` — never a panic, so a
malformed model argument is a result the loop can react to rather than a
crash.

`GET /v1/tools?agent=<id>` returns the tools that agent may call, **sorted by
name**. The sort is not cosmetic: the list goes into the model request, and a
stable order keeps the prompt-cache prefix stable.

### Nested calls, and the limits on them

A built-in `call_tool` lets a tool call another one. It resolves the target by
name and recurses through the same `Execute`, so scopes, schema validation and
the call log apply identically at every level — there is no second, weaker
path.

Four limits, all enforced in the runtime rather than asked for in a prompt:

| Limit | Value | Error |
|---|---|---|
| Depth | 3 | `depth_exceeded` |
| Calls per task | 50 | `budget_exceeded` |
| Same tool twice in one call stack | — | `cycle_detected` |
| Repeated idempotency key | — | returns the stored output |

A prompt can be ignored; a runtime check cannot. Queue work uses `step_id` as
the idempotency key, so a retried step replays its stored result instead of
running the handler twice.

### Handover

Three agents, one process each (`agentd -role=scout|analyst|writer`), each
with its own token and scope set. No agent can perform another's writes — the
scope check refuses it.

```
  [scout]              [analyst]                 [writer]
  vacancies:write      vacancies:read            vacancies:read
  companies:write      inventory:read            scores:read
  tools:call           scores:write              letters:write
                       tools:call                tools:call
     |                     |                        |
     | poll searches       | extract requirements   | draft letter
     | dedup + store       | map to inventory       | fact lock
     | company + contact   | score + gaps           | render PDF
     |                     |                        |
     +---- score_vacancy --+---- draft_letter ------+
              (queue)              (queue)

  one trace_id follows a vacancy from API poll to PDF
```

`scout` creates one task per new vacancy and queues a `score_vacancy` step.
`analyst` claims it; at or above the threshold with no must-gap it queues
`draft_letter`, otherwise it closes the task with the reason. `writer`
produces the draft and closes the task.

Steps are claimed with `FOR UPDATE SKIP LOCKED`, so two analysts racing for
the same step cannot both win. A step moves `pending -> claimed -> done |
failed`; a failure retries with backoff (`now() + 2^attempts x 5s`) up to
three attempts, then becomes `dead` and closes its task with a reason.

### Scoring

The model only reads the advertisement. The number is computed in code, so
every point traces to one sentence in the ad and one entry in the inventory.

```
coverage:  production 1.0 | used 0.8 | managed 0.4 | learning 0.2 | none 0.0
           unmapped = 0.0; a requirement mapped to several ids takes
           the highest coverage among them

must_coverage = mean coverage over the must requirements
nice_coverage = mean coverage over the nice requirements

score = 100 x (0.8 x must_coverage + 0.2 x nice_coverage)

no nice requirements -> score = 100 x must_coverage
no must requirements -> score = 100 x nice_coverage
```

The two kinds are averaged separately rather than summed into one weighted
mean. A single mean puts every listed requirement in the denominator, so the
score starts to measure how long the advertisement is: the same candidate,
fully covering the same three must-haves, scores 90 against a terse ad with
one nice-to-have and 52 against a corporate one with twelve. Averaged
separately they score 80 and 83. Large employers write long ads, so the
single-mean version rules out precisely the vacancies worth applying to.

`must` is a requirement the ad states outright; `nice` is one it softens
("idealerweise", "von Vorteil", "wünschenswert", "a plus").

Three caps apply afterwards, and the reason is stored alongside the score:

| Condition | Cap |
|---|---|
| Any `must` with coverage 0 | 70 |
| A language requirement above the inventory level (ad wants C1, inventory says B1) | 75 |
| `remote = onsite` and the city beyond `max_commute_km` | 60 |

The first cap is the load-bearing one: a missing must-have can never reach the
threshold, so it can never trigger a letter.

### Fact lock

A letter is drafted only when the score clears the threshold and there are no
must-gaps. Every claim it makes must carry a source — an inventory id or a
line from the profile — and those ids are recorded in `letters.facts_used`.

A sentence with no source fails validation and the draft is rejected.

This is the rule that matters most: an invented fact is not caught by the
reader, it is caught in the interview.

## Data boundary

This repository is public, and the system it runs collects personal data: a
CV, a profile, a skills inventory, generated letters, and the contact details
of recruiters named in job advertisements. None of that belongs in git.

Artifacts fall into three tiers:

| Tier | Where it lives | In git |
|---|---|---|
| Public | the repository | yes, this is the work sample |
| Private | the data root, outside the tree | never |
| Generated | wherever it is built | no |

### The data root

The system writes every private artifact beneath a single root outside the
working tree, resolved in this order:

1. `$ROBASKO_DATA_DIR`, used exactly as given
2. `$XDG_DATA_HOME/robasko`
3. `~/.local/share/robasko`

A root that resolves to a location inside the repository is rejected at
startup, and the error names the offending path. A mistyped override fails
loudly, before any personal data lands where git can reach it.

What lives there:

| File | What it is | Example in the repo |
|---|---|---|
| `inventory.yaml` | skills with levels and evidence; every score is computed against it | `inventory.example.yaml` |
| `profile.yaml` | citable facts a letter may claim | `profile.example.yaml` |
| `style.yaml` | how a letter is written | `style.example.yaml` |
| `protected-literals.txt` | what the pre-commit scan refuses to let through | — |
| `letters/` | generated drafts and their PDFs | — |

The committed examples carry invented contents and are there to show the
format. Nothing reads them at runtime.

`env.lock` is the exception: it holds source hosts, credentials and
`DATABASE_URL`, and it sits in the working tree where the default-deny
ignore rules keep it untracked. `.env.example` shows the variables.

Symlinking the private files into the working tree is convenient and safe —
the ignore rules cover the link as well as the target:

```sh
ln -s "$HOME/.local/share/robasko/inventory.yaml" inventory.yaml
```

```sh
# inspect the resolved root
ROBASKO_DATA_DIR=/tmp/robasko-demo go test ./internal/paths/ -run TestDataRootUsesOverrideAsGiven -v
```

Back up the data root. `~/.local/share` is excluded by many default backup
configurations, and the hand-written inventory and generated letters are
expensive to recreate. Either include it, or set `ROBASKO_DATA_DIR` to
somewhere already covered.

### Ignore rules run default-deny

`.gitignore` ignores `*` and allowlists the public tree, so a newly created
file stays untracked until someone adds it to the allowlist on purpose. If a
file you expected to commit does not appear in `git status`, this rule is why;
add it to the allowlist block at the top of `.gitignore`.

Example artifacts (`cv/example.md`, `inventory.example.yaml`,
`profile.example.yaml`) hold invented contents and are published so the
formats are legible. Their real counterparts live in the data root.

### Fixtures are scrubbed when recorded

Tests replay recorded language-model exchanges so that CI never calls the API.
Those recordings come from real advertisements, which makes them the one place
personal data would otherwise be committed deliberately.

`internal/scrub` rewrites a recording as it is written, well before any
commit, so the unscrubbed form never exists on disk. Names, companies,
addresses, email addresses and telephone numbers become pseudonyms derived
from an HMAC of the real value, keyed by a secret in the data root. The
mapping is stable, so the same person named in an advertisement and in the
letter answering it becomes the same pseudonym in both, and it stays
unreadable to anyone holding only the public repository.

The scrubber touches only the string values of content-bearing keys. Tool
names, schema keys, model identifiers and token counts come through byte for
byte, so a fixture stays faithful to the shape of the real exchange.

Never hand-edit a fixture to put a real value back. If a recording is wrong,
record it again.

### Configuration

Everything environment-dependent is read from the environment. Copy the
template and fill it in:

```sh
cp .env.example .env
```

`.env.example` is committed and holds no values; `.env` is ignored and never
enters the image — `.dockerignore` excludes it from the build context, so a
credential cannot end up in a layer.

**Source hosts are configuration, not constants.** No base URL has a default
in the code. A host compiled into a binary is a third party you cannot
redirect without a rebuild: not to a staging endpoint, not to a recorded
fixture, not when a provider moves or starts refusing you. A missing value
fails at startup naming the variable, rather than silently reaching for
someone else's server.

### Protected literals

A pre-commit check refuses any commit whose staged content carries a
protected literal. Install it once:

```sh
make hooks          # points git at .githooks/
make check          # audit every tracked file, not just the staged ones
```

The list of literals lives outside this repository, because the list is itself
the data the check exists to keep out. It sits in the data root as
`protected-literals.txt`, one `category: value` per line:

```
# categories: person, company, address, locality, email, phone
person:   Your Name
locality: Your Town
email:    you@example.com
phone:    +49 000 0000000
```

Matching is deliberately loose, and identical to the scrubber's: case and
diacritics are ignored (`Müller`, `MUELLER` and `muller` are one name), and a
telephone number matches however it is grouped and whichever of `+49`, `0049`
or a national `0` precedes it.

Without a list the check prints a warning and allows the commit, so cloning
this repository does not block anyone else's work.

## Development

```sh
make help      # every target, split by what it needs
make test      # Go only, offline
make up        # Postgres 16 in Docker, waits until it is ready
```

The Go targets (`test`, `vet`, `fmt`, `check`, `hooks`) need a Go toolchain
and the module cache. The tests themselves make no network calls — recorded
fixtures stand in for the model — but a clean clone resolves dependencies
once through `go mod download`.

The Docker targets need a daemon. `up`, `down`, `logs`, `psql` and `run`
drive the stack.

The toolchain is also available as a service, so the repository can be built
and tested with no local Go at all:

```sh
make test-docker                      # or, directly:
docker compose run --rm go test ./...
docker compose run --rm go vet ./...
```

Two named volumes cache the Go build and module directories, which is what
keeps this usable — without them every run recompiles the standard library.
A warm containerised run costs about 0.2s more than a native one, almost all
of it container start.

```sh
make down                 # stop, keep the data volume
docker compose down -v    # stop and destroy the database
```

## Status

| Part | State |
|---|---|
| Data boundary, data root, scrubber, pre-commit scan | built, tested |
| Docker: Postgres 16, service image, Go toolchain service | built |
| `agentd` with `-role`, `/healthz`, `/readyz`, graceful shutdown | built, tested |
| Source configuration from numbered environment slots | built, tested |
| One source adapter, `cmd/fetch` | built, tested |
| Storage: migrations, fingerprint dedup, company dedup | built, tested |
| Matching vocabulary in `reference_terms` | built, tested |
| Inventory loading and the coverage table | built, tested |
| Scoring: must/nice split, caps, gaps | built, tested, **not yet wired** |
| Requirement extraction, model client, fixture replay | built, tested offline; **never run against the live API** |
| Tool registry: schemas, versions, scopes | built, tested |
| Runtime: nested calls, depth, budget, cycle and idempotency limits | specified, not built |
| Toolchain drift guard | built, tested |
| Queue, step state machine, handover between roles | specified, not built |
| Demand aggregation, letters with the fact lock | specified, not built |

Scoring is complete and tested but nothing calls it yet: the engine takes a
mapped requirement list, extraction produces an unmapped one, and the step
that maps a requirement onto inventory ids is not written.

Extraction has never answered. Its tests replay recorded exchanges, and the
live path got as far as a billing error — so the key and the workspace
header work, and the request body is not yet proven.

An alias-matching stand-in existed for a while and was removed. It read
"German language is NOT necessarily required" as a satisfied demand and a
board's own footer as a requirement, and it could not see a demand the
inventory had no word for, so it only ever scored high. A number that is
wrong in one direction is worse than no number: it would have been quoted.

`make up` applies the schema as well: `agentd` migrates on startup, so a
fresh database is never served against and bringing the stack up is one
step. Concurrent roles are safe — the migration run holds an advisory
lock, and the processes that lose the race find nothing to do. `make
migrate` does the same from the host when the service is not running.

```sh
make up
curl localhost:8080/healthz   # ok
curl localhost:8080/readyz    # ready, or 503 when the database is down

go run ./cmd/fetch -what=backend -save   # fetch and store, skipping duplicates
```

Generated letters, caches and logs are written beneath the data root
through `internal/paths`, never into the working tree — the repository is
published, and a letter is a biography.

Liveness does not touch the database on purpose: a database outage should
not make the orchestrator restart processes that are working.

`go test ./...` makes no network calls: the model client is replayed from
recorded fixtures. Dependencies resolve once through the module cache.
