# Job agent — Spezifikation / spec

A Go service with a tool registry, a tool-calling runtime where tools can call other tools, and three agents that hand work over through a queue. The domain is the maintainer's job search: find real vacancies through public job APIs, score them against a structured inventory of their experience, derive learning priorities from actual demand, and draft a cover letter when a vacancy scores 80 or higher.

Two purposes at once: a tool the maintainer uses daily, and public code that answers the agentic-AI requirements a CV cannot yet answer. Nothing from this spec goes into that CV until it runs and is pushed.

## 0. Intent and agreed decisions

- **Source: public job APIs.** Which boards this runs against is configuration, not part of the design: each is a numbered slot in the environment carrying a name, a base URL and its credentials. One is primary, the rest optional. No scraping of boards whose terms forbid it — that rules out most of the large networks, whose official APIs do not offer job search to individuals anyway, and an adapter that would need scraping is simply not written.
- **One project**, one repository, one spec: runtime mechanics and vacancy domain together.
- **Hybrid scoring.** A skills inventory is built once from the CVs and `profile.md`. The model extracts requirements from the ad and maps them onto inventory ids. The score is computed in code, so the same input gives the same number and every point is explainable.

Assumptions, change them if wrong: threshold 80, demand window 30 days, nothing is ever sent anywhere, letters are produced with the existing cover-letter assets rather than a second set of rules.

## 1. Requirement mapping

| Agentic-AI job requirement | Component |
|---|---|
| Building back-end systems for AI agents | `cmd/agentd` + Postgres, §5-§7 |
| Orchestrating agentic systems, handovers and collaboration between agents | scout → analyst → writer over the step queue, §8 |
| Building agentic systems using existing foundation modules and components | tool registry §6, skills §9 |
| Developing tools that allow agents to call and execute other tools | built-in `call_tool` with depth, budget and cycle limits, §7 |
| Writing custom tools to support the creation and execution of skills | `register_skill` / `run_skill` and the four skills in §9 |
| Managing contact information and contact-management processes through agents | `companies` and `contacts` with dedup and merge tools, §5-§6 |
| Designing shared back-end services and managing how multiple agents access and use them | one vacancy service, one inventory service, three agents with different scopes, §8 |

## 2. Non-goals

No UI beyond JSON endpoints. No auto-apply and no outgoing mail. No multi-tenancy, no vector search, no Kubernetes. No agent framework as a dependency: the loop is written here, that is the point.

## 3. Stack

- Go (current stable), standard library HTTP, `log/slog`.
- PostgreSQL 16 for registry, tasks, call log, vacancies, companies, contacts, inventory, demand, letters.
- Queue: Postgres table with `FOR UPDATE SKIP LOCKED`. `internal/queue` is an interface with a second implementation on AWS SQS behind a flag, so the event-driven experience is visible in the repo.
- LLM: Anthropic Go SDK, `go get github.com/anthropics/anthropic-sdk-go`.
- Python is called once, for the PDF: the existing `scripts/build_letter.py`.
- `docker-compose.yml` with Postgres and the service. `make up`, `make test`, `make run`.

## 4. Repository layout

```
cmd/agentd/main.go          HTTP service, role flag, wiring
cmd/cycle/main.go           one full cycle, the cron entry point
internal/registry/          tool definitions, JSON Schema validation, scopes
internal/runtime/           tool execution, nested calls, limits, idempotency
internal/orchestrator/      scout, analyst, writer; step state machine
internal/queue/             Queue interface, postgres impl, sqs impl
internal/skills/            skill format, loader, the four skills
internal/sources/           Source interface, one adapter per response shape
internal/vacancies/         normalisation, fingerprint dedup, storage
internal/crm/               companies and contacts, dedup and merge
internal/inventory/         inventory build, levels, coverage lookup
internal/scoring/           requirement weights, coverage, caps
internal/demand/            demand aggregation, learning report
internal/letters/           draft generation, fact lock, PDF renderer call
internal/llm/               Anthropic client wrapper, fake client for tests
internal/obs/               trace ids, structured logging
migrations/                 SQL migrations
SPEC.md  README.md  docker-compose.yml  Makefile
```

## 5. Data model

```sql
-- runtime
agents(id uuid pk, name text unique, model text, scopes text[], token_hash text, created_at timestamptz)

tools(id uuid pk, name text, version int, description text,
      input_schema jsonb, output_schema jsonb, required_scopes text[],
      timeout_ms int default 5000, created_at timestamptz, unique(name, version))

tasks(id uuid pk, trace_id text, goal text, subject_id uuid, -- the vacancy, when there is one
      created_by uuid references agents, status text,        -- planning | running | done | failed
      created_at timestamptz, finished_at timestamptz)

task_steps(id uuid pk, task_id uuid references tasks, seq int,
           tool_name text, tool_version int, input jsonb,
           status text,                                      -- pending | claimed | done | failed | dead
           claimed_by uuid references agents, claimed_at timestamptz,
           attempts int default 0, result jsonb, error_code text,
           available_at timestamptz default now(), unique(task_id, seq))

tool_calls(id uuid pk, trace_id text, task_id uuid, step_id uuid,
           parent_call_id uuid references tool_calls, depth int,
           agent_id uuid, tool_name text, tool_version int,
           input jsonb, output jsonb, status text, error_code text,
           input_tokens int, output_tokens int,
           started_at timestamptz, duration_ms int)

-- domain
searches(id uuid pk, name text, source text, query jsonb, active bool, created_at timestamptz)

companies(id uuid pk, name text, normalised_name text unique, website text,
          city text, country text, created_at timestamptz)

contacts(id uuid pk, company_id uuid references companies, full_name text, role text,
         email text, phone text, salutation text, source text,
         merged_into uuid references contacts, created_at timestamptz, updated_at timestamptz)

vacancies(id uuid pk, source text, source_id text, url text, title text,
          company_id uuid references companies, contact_id uuid references contacts,
          city text, remote text,            -- onsite | hybrid | remote | unknown
          language text,                     -- de | en
          published_at date, raw_text text, fingerprint text,
          status text,                       -- new | scored | letter_drafted | rejected | archived
          created_at timestamptz, unique(source, source_id))

requirements(id uuid pk, vacancy_id uuid references vacancies, text text,
             kind text,                      -- must | nice
             category text,                  -- language | framework | db | cloud | practice | domain | language_skill | education | location | other
             canonical_ids text[], coverage numeric, weight int)

scores(vacancy_id uuid pk references vacancies, score numeric, capped_reason text,
       must_gaps text[], nice_gaps text[], model text,
       inventory_version int, computed_at timestamptz)

inventory(id text, version int, kind text, aliases text[],
          level text,                        -- production | used | managed | learning | none
          evidence text, last_used_year int, updated_at timestamptz,
          primary key(id, version))

demand(snapshot_date date, canonical_id text, window_days int,
       vacancy_count int, share numeric, must_share numeric,
       primary key(snapshot_date, canonical_id, window_days))

letters(id uuid pk, vacancy_id uuid references vacancies, txt_path text, pdf_path text,
        facts_used text[], status text,      -- draft | needs_human | approved | discarded
        created_at timestamptz)
```

Indexes: `task_steps(status, available_at)` for the claim query, `tool_calls(trace_id)`, `vacancies(fingerprint)`, `vacancies(status, created_at)`, `companies(normalised_name)`, `contacts(lower(email))`.

`fingerprint` hashes normalised title + company + city, so the same job from two sources is stored once.

## 6. Tool registry

A tool is a name, a version, JSON Schemas for input and output, the scopes required to call it, and a timeout.

- `POST /v1/tools` registers or bumps a version. Schemas must set `"additionalProperties": false` and list `required`.
- `GET /v1/tools?agent=<id>` returns the tools that agent may call, sorted by name. Sorted output matters: the list goes into the model request and a stable order keeps the prompt cache prefix stable.
- Input is validated before execution, output after. A violation becomes a `tool_result` with `is_error: true`, never a panic.

Registered at startup:

| Tool | Scopes | Notes |
|---|---|---|
| `sources.fetch` | `vacancies:write` | one saved search against one adapter, returns raw vacancies |
| `vacancies.upsert` | `vacancies:write` | normalise, fingerprint, dedup, store |
| `vacancies.get` | `vacancies:read` | ad text and stored fields |
| `vacancies.list` | `vacancies:read` | filters: status, min score, since |
| `companies.upsert` | `companies:write` | normalised name, website, city |
| `contacts.upsert` | `companies:write` | contact person from the ad, with salutation |
| `contacts.find_duplicates` | `companies:write` | email and phone normalisation, name similarity |
| `contacts.merge` | `companies:write` | one survivor, writes the merge into the audit log |
| `vacancy.extract_requirements` | `vacancies:read` | model call, strict schema |
| `vacancy.map_requirements` | `inventory:read` | model call, strict schema, ids must exist |
| `score.compute` | `scores:write` | code only, no model |
| `inventory.get` | `inventory:read` | current version, coverage lookup |
| `demand.refresh` | `scores:read` | code only, aggregates the window |
| `learning.suggest` | `scores:read` | top N priorities with evidence |
| `letter.draft` | `letters:write` | writes .txt, renders the PDF, enforces the fact lock |
| `call_tool` | caller's own | the nesting tool, §7 |
| `register_skill` / `run_skill` | `skills:write` / `skills:run` | §9 |

## 7. Runtime and nested tool calls

`runtime.Execute(ctx, caller, name, input, idempotencyKey)`:

1. Resolve tool, latest version unless pinned.
2. Check `caller.scopes ⊇ tool.required_scopes`, else `error_code: "forbidden"`.
3. Validate input against the schema.
4. Write a `tool_calls` row with `depth`, `parent_call_id`, `trace_id`.
5. Run the handler under `context.WithTimeout(tool.timeout_ms)`.
6. Validate output, finish the row with status, `duration_ms` and token counts for model calls.

Nesting rules, enforced in the runtime rather than in a prompt:

- `call_tool` resolves the target by name and recurses through the same `Execute`, so scopes, validation and logging apply identically.
- `max_depth = 3`, else `depth_exceeded`.
- Per-task budget `max_calls = 50`, counted on `tool_calls` by `task_id`, else `budget_exceeded`.
- Cycle detection: the call stack lives in the context; the same tool twice in one stack returns `cycle_detected`.
- Idempotency: a repeated key with a finished call returns the stored output instead of running the handler again. Queue work uses `step_id` as the key.

## 8. Agents, handover, orchestration

Three agents, one process each (`agentd -role=scout|analyst|writer`), each with its own token and scopes.

| Agent | Scopes | Job |
|---|---|---|
| `scout` | `vacancies:write`, `companies:write`, `tools:call` | polls each active search, normalises, deduplicates, stores vacancies, extracts company and contact person |
| `analyst` | `vacancies:read`, `inventory:read`, `scores:write`, `tools:call` | extracts requirements, maps them to inventory ids, computes the score and gaps, refreshes demand |
| `writer` | `vacancies:read`, `scores:read`, `letters:write`, `tools:call` | drafts the letter for vacancies at or above the threshold, renders the PDF, marks it for review |

Handover: `scout` creates one task per new vacancy and queues a `score_vacancy` step. `analyst` claims it; at or above the threshold with no must-gap it queues a `draft_letter` step for `writer`, otherwise it closes the task with the reason. `writer` claims the step, produces the draft, closes the task. Every transition carries `trace_id`, so one request shows the whole path from API poll to PDF.

No agent may do another's writes: `scout` cannot score, `analyst` cannot write letters. The scope check enforces it.

Claim query:

```sql
UPDATE task_steps SET status='claimed', claimed_by=$1, claimed_at=now(), attempts=attempts+1
WHERE id = (SELECT id FROM task_steps
            WHERE status='pending' AND available_at <= now()
            ORDER BY task_id, seq LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING *;
```

Handover envelope, which is also what the logs show:

```json
{"trace_id":"...","task_id":"...","step_id":"...","seq":2,
 "tool":"letter.draft","input":{"vacancy_id":"..."},
 "from":"analyst","to":"writer","attempt":1,"deadline":"2026-10-19T18:00:00Z"}
```

Step state machine: `pending → claimed → done | failed`. A failure retries with backoff (`available_at = now() + 2^attempts × 5s`), `max_attempts = 3`, then `dead`. A dead step closes its task as `failed` with the reason; the daily report lists them.

## 9. Skills

A skill is a declarative composition of registered tools, stored through `register_skill` and executed by `run_skill` through the same runtime, so limits and scopes apply unchanged.

- `vacancy.triage` — for one vacancy: extract requirements, map them, compute the score. Uses `call_tool` so the three steps are one nested chain under one parent call.
- `vacancy.harvest` — for one saved search: fetch, upsert companies and contacts, upsert vacancies, queue a triage task per new vacancy.
- `inventory.audit` — report unmapped requirements from the window, grouped and counted: the list from which the inventory grows.
- `job.cycle` — harvest every active search, triage everything new, refresh demand, draft letters for everything at or above the threshold, return the daily report.

```yaml
name: vacancy.triage
input_schema:
  type: object
  additionalProperties: false
  required: [vacancy_id]
  properties:
    vacancy_id: {type: string}
steps:
  - tool: vacancy.extract_requirements
    input: {vacancy_id: "{{ .input.vacancy_id }}"}
    as: reqs
  - tool: vacancy.map_requirements
    input: {requirements: "{{ .reqs.requirements }}"}
    as: mapped
  - tool: score.compute
    input: {vacancy_id: "{{ .input.vacancy_id }}", mapped: "{{ .mapped }}"}
```

## 10. Skills inventory

Built once by `inventory.build`, versioned, then maintained by hand. Each entry names what the maintainer can do, at what level, with the evidence behind it.

```yaml
- id: php
  kind: language
  aliases: [php, php8, "php 8", php-mvc]
  level: production
  evidence: "Eleven years; Symfony monolith at Kastanienhof Verlag, 2017-2021"
  last_used_year: 2026
- id: python
  kind: language
  aliases: [python, py]
  level: managed
  evidence: "Led the data tooling team at Nordwind Logistik; reviewed the code, did not write it"
  last_used_year: 2024
- id: bootstrap
  kind: framework
  aliases: [bootstrap]
  level: none
  evidence: ""
```

The build reads the English and German CVs plus `profile.md`. A human edits the result and the file is the source of truth afterwards; the model never writes to it. `managed` exists because leading a department is real but is not hands-on code. `none` entries stay on purpose: they make a gap explicit instead of silent.

## 11. Scoring

Model work is limited to reading the ad; the number is code.

1. `vacancy.extract_requirements` returns requirements with the verbatim `text`, `kind` (`must` when the ad lists it as a requirement, `nice` when softened by "idealerweise", "von Vorteil", "wünschenswert", "a plus") and `category`.
2. `vacancy.map_requirements` maps each requirement to zero or more inventory ids. An id outside the inventory fails schema validation; the requirement is recorded as unmapped.
3. `score.compute`:

```
coverage:  production 1.0 | used 0.8 | managed 0.4 | learning 0.2 | none 0.0
           unmapped = 0.0; several ids take the highest coverage

must_coverage = mean coverage over the `must` requirements
nice_coverage = mean coverage over the `nice` requirements

score = 100 × (0.8 × must_coverage + 0.2 × nice_coverage)

an ad with no `nice` requirements → score = 100 × must_coverage
an ad with no `must` requirements → score = 100 × nice_coverage
```

The two kinds are averaged separately and then mixed at a fixed ratio, rather than summed into one weighted mean. A single mean over all requirements makes the denominator grow with however many things the ad happened to list, so the same candidate scores lower against a corporate wishlist than against a terse startup ad:

```
                          Ad A, terse          Ad B, verbose
                          3 must, 1 nice       the same 3 must, 12 nice
                          all must covered     all must covered
                          0 nice covered       2 nice covered

one weighted mean         100 × 9/10  =  90    100 × 11/21  =  52
must and nice separate    80.0                 83.3
```

Ninety against fifty-two is a verbosity measurement, not a fit measurement, and it cuts the wrong way: large employers write long ads, so the threshold would rule out exactly the vacancies worth applying to. Averaging each kind over its own count removes the dependency; the 0.8/0.2 split then states how much a nice-to-have is allowed to matter, in one place, instead of it emerging from a ratio of counts.

Caps applied afterwards, with the reason stored:

- any `must` with coverage 0 → capped at 70, so a missing must-have can never trigger a letter. The cap stays necessary under the split formula: one uncovered must out of ten still leaves `must_coverage` at 0.9, which reaches 92 with good nice coverage;
- a language requirement above the inventory level (ad wants C1, inventory says B1) → capped at 75;
- `remote = onsite` and the city further than `max_commute_km` from the home location in the profile → capped at 60.

Output: the number, the per-requirement table with evidence, `must_gaps`, `nice_gaps`. Every point traces to one ad sentence and one inventory entry.

## 12. Learning priorities from actual demand

`demand.refresh` aggregates over the vacancies actually collected in the window:

```
share(skill)      = vacancies requiring it / vacancies in window
must_share(skill) = vacancies requiring it as must / vacancies in window
gap(skill)        = 1 - coverage(skill)
priority(skill)   = (0.7 × must_share + 0.3 × share) × gap
```

`learning.suggest` returns the top N by priority with the skill, its share of the window, the current level, and three vacancy links where it was required. A skill at `production` drops out because its gap is zero. Unmapped requirements are reported separately as demand the inventory has no word for yet.

## 13. Letter drafting

Runs only when `score >= threshold` and `must_gaps` is empty.

`letter.draft`:

1. Reads `profile.md` and `style.md` from the cover-letter skill directory. Those files stay the single source of style and facts.
2. Writes in the language of the ad, `Sie` in German; salutation from the stored contact (`Sehr geehrte Frau X` when the ad gives the gender, `Guten Tag Vorname Nachname` when it does not, `Liebes <Firma>-Team` with no contact).
3. **Fact lock:** every claim must carry an inventory id or a `profile.md` line, recorded in `letters.facts_used`. A sentence without a source fails validation and the draft is rejected. This is the rule that matters most here: an invented fact surfaces in the interview.
4. Nice-to-have gaps are not mentioned; must-have gaps cannot occur by construction.
5. Saves `cover-letter_<CompanyNoSpaces>_<LastName>_<FirstName>.txt`, then runs `scripts/build_letter.py` with subject, recipient and company. Exit code 2 means it did not fit one page: shorten the lowest-weighted paragraph and retry at most twice, then mark `needs_human`.
6. Status is `draft`. Nothing is sent; approval is a manual `PATCH /v1/letters/{id}`.

## 14. Interfaces

- `POST /v1/searches`, `GET /v1/searches` — saved searches per source.
- `POST /v1/runs` — run `job.cycle`; the cron entry point. `GET /v1/runs/{id}` reports counts and token spend.
- `GET /v1/vacancies?min_score=&status=&since=` — review queue, score descending.
- `GET /v1/vacancies/{id}` — ad, requirement table, score, caps, gaps, letter paths.
- `GET /v1/report/learning?window=30` — §12.
- `PATCH /v1/letters/{id}` — approve or discard.
- `GET /v1/traces/{trace_id}` — the call tree.
- `GET /healthz`, `GET /readyz`.

A daily cron calls `POST /v1/runs` and nothing else. The morning output is one report: new vacancies with scores, drafts waiting for review, dead steps, and the current learning top ten.

## 15. External sources

One adapter per source behind `Source.Fetch(ctx, query) ([]RawVacancy, error)`.

Boards are configured, never named in the source. Each occupies a numbered slot in the environment:

```
SOURCE_<n>_NAME        identifier stored in vacancies.source; stable once used
SOURCE_<n>_BASE_URL    no default in code; a missing value fails at startup
SOURCE_<n>_API_KEY     empty for boards that need no credential
SOURCE_<n>_API_ID      only for two-part credentials
```

An adapter is written against a response *shape* rather than a provider, and is named for that shape. A host compiled into a binary is a third party that cannot be redirected without a rebuild: not to a staging endpoint, not to a recorded fixture, not when a board moves or starts refusing the client.

What a usable board must provide: a full advertisement text rather than a teaser, a stable per-posting identifier, and terms that permit automated reading. Structured location and publication date are welcome and can be derived when missing.

Rules for every adapter: respect the documented rate limit, back off on 429, cache raw responses for a day so re-runs cost nothing, store `raw_text` verbatim for audit. No HTML scraping of boards that forbid it; an adapter that would need it is not written.

## 16. LLM integration

`internal/llm` wraps the Anthropic Go SDK and exposes the one call the agents need, plus a fake implementation for tests.

- Default model `claude-opus-5-5`; the executor-side calls may run on `claude-sonnet-5-5` behind a flag.
- Build tools with `toolrunner.NewBetaToolFromJSONSchema(name, description, fn)` and drive the loop with `client.Beta.Messages.NewToolRunner([]anthropic.BetaTool{...}, anthropic.BetaToolRunnerParams{BetaMessageNewParams: anthropic.BetaMessageNewParams{Model: "claude-opus-5-5", MaxTokens: 16000, Messages: ...}, MaxIterations: 8})`, then `RunToCompletion(ctx)`. Each tool function calls `runtime.Execute`, so the registry stays the single source of truth.
- Thinking: adaptive, `anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized}}`. No `budget_tokens`: it returns 400 on current models.
- Effort: Opus 5.5 defaults to `medium`, so set it explicitly; take the exact field name from the installed SDK rather than guessing.
- `strict: true` on every tool (`Strict: anthropic.Bool(true)`, `additionalProperties: false` via `InputSchema.ExtraFields`). Do not use `tool_choice` `any`/`tool`: both return 400 on Opus 5.5. Steer with `auto` plus an explicit instruction.
- Parallel calls: execute concurrently, return **all** `tool_result` blocks in a single user message; a failed tool still returns a `tool_result` with `is_error: true`.
- Parse tool input with `json.Unmarshal`, never string matching.
- `MaxTokens: 16000` for non-streaming requests; stream if a request ever needs more.
- Prompt caching: request order is tools → system → messages. Keep the tool list sorted and the system prompt frozen, put the ad text last, and watch `usage.cache_read_input_tokens` — zero across runs means something volatile crept into the prefix.
- Errors: `errors.As` into `*anthropic.Error`, then switch on `StatusCode`; 429 and 5xx retry with backoff, 400 and 404 fail the step.
- Model calls per vacancy: at most three (extract, map, letter). Scoring and demand never call a model. CI never calls the API: the fake client replays fixtures.

## 17. Observability

One structured log line per tool call with `trace_id`, `task_id`, `step_id`, agent, tool, depth, `parent_call_id`, status, `duration_ms` and token counts. `GET /v1/traces/{trace_id}` returns the call tree: scout's fetch, the handover, the nested triage chain, the letter render.

## 18. Tests

- Registry: schema accepted, extra property rejected, missing `required` rejected, a version bump keeps the old version callable.
- Runtime: scope denial, input and output validation, timeout, `depth_exceeded` at depth 4, `cycle_detected`, `budget_exceeded`, idempotent replay.
- Orchestrator: two analysts race for one step and exactly one wins, retry backoff, `max_attempts` then `dead`, task closed with a reason.
- Scoring: the coverage table, the 0.8/0.2 mix, both single-kind fallbacks (an ad with no `nice`, an ad with no `must`), every cap, a vacancy with one uncovered must-have never reaches 80, identical input gives an identical score twice.
- Verbosity invariance: two ads with the same `must` requirements and the same coverage of them, one listing a single nice-to-have and one listing twelve, score within a few points of each other. This is the property the split formula exists for, so it is tested directly rather than inferred from the arithmetic.
- Mapping: an id outside the inventory is rejected; an unmapped requirement counts as 0 and lands in the audit.
- Demand: a `production` skill is absent from the suggestions; a skill required everywhere and absent from the inventory comes first.
- Letters: a sentence without a fact source fails validation; exit code 2 triggers exactly one shortening attempt before `needs_human`; the salutation rules hold for all three cases.
- Sources: fingerprint dedup across two adapters returning the same job, 429 backoff, raw cache hit costs no request.
- `go test ./...` passes without network.

## 19. Milestones

1. **Evening 1** — migrations, registry with schema validation, scopes and agent tokens, runtime with the call log, structured logging; tests for registry and runtime basics.
2. **Evening 2** — runtime nesting (`call_tool`, depth, budget, cycles, idempotency), queue interface with the Postgres implementation, the three roles, step state machine with retries; orchestrator tests.
3. **Evening 3** — the first source adapter, normalisation, fingerprint dedup, companies and contacts with merge, `scout`, saved searches, raw-response cache.
4. **Evening 4** — inventory build and hand edit, extraction and mapping with strict schemas, `score.compute` with caps, `analyst`, review queue endpoint.
5. **Evening 5** — demand refresh and learning report, `letter.draft` with the fact lock and the PDF renderer, `writer`, `job.cycle`, the daily report, README with one real vacancy traced end to end.

## 20. Definition of done

- `make up && make test` pass from a clean clone, without network.
- One real cycle against a live source produces stored vacancies, scores with requirement tables, a learning report, and at least one letter draft with its PDF.
- `GET /v1/traces/{trace_id}` shows scout → analyst → writer for that vacancy, including a nested `call_tool` chain.
- README explains the registry, the nesting limits, the handover protocol, the scoring formula with its caps, and the fact lock.
- Public under `github.com/isrenato`.

Only then do these lines go into the CV:

- Built a job-search agent system in Go: three agents with separate scopes handing work over through a queue, a tool registry with JSON Schema validation, nested tool execution with depth, budget and cycle limits, and a traced call log.
- Deterministic matching engine over a structured skills inventory, with model calls limited to requirement extraction and mapping; demand statistics over the collected ads drive the learning priorities.
