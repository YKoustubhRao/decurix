# Brainiac

Brainiac puts many open models behind one OpenAI-compatible endpoint. It measures the machine it runs on, chooses the best model that fits for each kind of work (reasoning, pictures, OCR, speech, finding things on a screen, embeddings), installs them, and serves them. A request that carries pictures or audio is broken into small jobs; each runs on the specialist model for it, and the brain answers from what they found.

It is its own Go module (standard library only) inside the Decurix repo. It imports nothing from Decurix; Decurix can use it like any other OpenAI-compatible model.

## Quick start

```bash
brainiac/scripts/build.sh            # build, measure, show the plan, install after a yes, serve
brainiac/scripts/build.sh plan       # only show what it would choose, and why
brainiac/scripts/build.sh --yes      # unattended
```

On Windows, `brainiac\scripts\build.ps1` is the twin. On this repo's laptop, where Ollama and the desktops live in WSL, run the Linux script inside WSL so Brainiac and its models share one memory pool:

```bash
wsl -d Ubuntu -- ./brainiac/scripts/build.sh
```

The same script runs on a laptop, a GPU box or a server. Only what it measures changes, and so does what it picks:

| Machine | Brain | Pictures | OCR | Speech |
|---|---|---|---|---|
| This laptop in WSL (2.8 GB to give, 3 cores) | Qwen3.5 2B (also does vision and screen pointing) | the same model | PP-OCRv6 | Parakeet 0.6B v3 |
| 128 GB unified memory | Qwen3.8 27B | the same model | PP-OCRv6 | Parakeet 0.6B v3 |
| 24 GB NVIDIA GPU | Qwen3.8 27B in VRAM | the same model | PP-OCRv6 | Parakeet 0.6B v3 |

Models and state live in `BRAINIAC_HOME` (default `~/.brainiac`), never in the repo. Do not put it on a RAM disk (WSL's `/tmp` is one); the census warns if you do.

## Fields: math, code, SQL, law, medicine, finance, science, translation

What the plan picks depends on the machine:
- **This laptop:** VibeThinker 1.5B for math, flash-sqlite for SQL, Hy-MT2 1.8B for translation (Indian languages: hi, bn, ta, te, mr, gu, ur). Code, law, medicine, finance and science stay with the brain.
- **A 128 GB box:** Hulu-Med 27B for medicine and Hy-MT2 30B for translation. Its 27B brain already beats the math, code and SQL specialists.

For each field the plan looks for an open specialist that fits this machine and beats the chosen general brain in that field by at least 5 quality points. A field with no such specialist is answered by the brain, and the plan says why: none fits, or none beats a modern general model of the same size. That is common, because 2023–24 domain models lose to today's general models.

Requests reach a specialist three ways:
- The client names the field: `"domain": "math"`, or the model `brainiac-math`.
- The router decides. It is a naive Bayes classifier in plain Go: no model, microseconds, no memory. It learns from the catalog's examples and from checked cases. It routes only when confident (0.35 or more); otherwise the brain answers. On held-out requests it sends none to the wrong field.
- Measured results decide over published ones:
  - `brainiac eval` runs your checked cases against the brain, each specialist and the remote brain, and keeps scores per field in `evals.json`.
  - A local model takes a field only when the accuracy its checked cases let it claim (Wilson lower bound, at least 20 cases) is at least what the remote brain scored. There is no margin, and nothing can prove itself as good as a perfect score.
  - Only cases that a stated number, letter or exact text decides count. `contains` and `regex` cases are scored but prove nothing.
  - A routed request earns on its routed score. `brainiac eval` asks `POST /v1/brainiac/route` where each case goes (no model runs) and keeps `routed:<model>` scores; a case the router files under the wrong field is a miss there.
  - An earned path serves only requests shaped like eval cases: one user turn, a system prompt of at most 600 characters, no pictures, recordings or web lookup.
  - In local mode, a specialist takes routed requests only once it is proven at least as good as the brain. Naming the field (`"domain"`) always reaches it.
  - A specialist measured worse than the local brain loses its field back to the brain.
  - Among the local models that earned a field, the fastest answers. A request too long for its window, or a path slower on average than `max_local_seconds`, stays with the remote brain.
  - A client can report its final verdict on real answers: `POST /v1/brainiac/outcomes {"ok": false, "trace": id}`, or `"since"` (RFC 3339) for every answer since then. A miss reported after a path was measured takes the field back until `brainiac eval` runs again. A pass earns nothing, because a real request has no answer key.
  - `brainiac paths` (`GET /v1/brainiac/paths`) says, per field, which local models have earned it and why each has or has not.
  - Scores measured before 2026-11-03 have no decided counts and prove nothing until `brainiac eval` runs again.

```bash
brainiac eval --cases ~/decurix-tests/brainiac/sample-cases.jsonl --learn
```

A case is one JSON line: `{"domain":"math","prompt":"...","check":{"kind":"number","value":"42"}}`. The check kinds are `number`, `exact`, `contains`, `regex` and `choice`. `number`, `exact` and `choice` judge only the answer's final part (the last `\boxed{}`, else what follows the last "answer is" or "answer:", else the last line), with thinking removed; two different numbers or letters there are a miss. `contains` and `regex` read the whole answer. `--learn` also teaches the router the cases' fields. Cases live outside the repo.

When the client sets no `max_tokens`, a local answer gets 2,048 tokens and a reasoning specialist gets room to think: its window less 2,048, at most 12,288 (the catalog's reasoning models have a 16,384-token window). On a CPU that is slow.

## The browser: fresh answers and a current arsenal

One web tool (`internal/web`) serves two jobs.

**Fresh answers, only when the client asks.** A request reads live web pages only when it sends its own search query as `"web_query"`. The default setting `web: on-request` also needs `"web": true`; `web: auto` lets a `web_query` alone ask (refused in `mode: local`); `web: off` never reads. Brainiac never builds a query from the conversation: with no `web_query` nothing is looked up, and the trace says so. `"web": false` always forbids it.

How it works:
1. The client's query is scrubbed of PAN, GSTIN, Aadhaar-like and account numbers, emails, phone numbers and secrets, and cut to 200 characters.
2. It is searched: SearXNG or Brave if set, otherwise DuckDuckGo, then Bing.
3. Up to three result pages are read with the guarded fetcher (never a loopback, private, link-local or metadata address). Only the passages about the query are kept.
4. The brain sees them as `<evidence kind="web" url=... retrieved=...>` and cites them as `[web-N]`.

Safety:
- What leaves the machine is that query and the page requests, nothing else of the conversation.
- The fetcher's address check runs on the address actually dialed, so a DNS answer cannot send it inside the network.
- Pages are size- and time-limited, and their text is evidence, never instructions.
- The headless browser never runs on its own. `POST /v1/brainiac/fetch {"url", "render": true}` asks for it, and `web_render: false` refuses it (the default on an appliance). Chrome then reaches the web only through a proxy inside Brainiac that applies the same address guard to every connection, loopback included. The page it reads is size-bounded. It runs with a model process's short environment (`isolate.Env`), never Brainiac's keys such as a remote brain's API key, and on Linux it is killed if Brainiac exits (2026-11-07).

Agents can use the same browser directly: `POST /v1/brainiac/search {"query"}` and `POST /v1/brainiac/fetch {"url", "render", "query"}`.

**A current arsenal.** `brainiac scout` looks for open models newer than the catalog:
- **Where it looks:** Hugging Face searches for trending and newest GGUF models, per field, plus speech, OCR and embeddings.
- **What it drops:** safety-stripped, merged and roleplay remixes, and general-purpose remixes of models the catalog already has. A model must come from a recognised lab (or a quantiser's copy of one), or show real adoption plus published claims.
- **What it checks per model:**
  - licence class, including the base model's licence when the GGUF repo has none
  - gating
  - the best GGUF file, with split parts added up and helper draft files skipped
  - the vision projector file
  - whether it fits this machine
  - whether the pinned llama.cpp build runs its architecture, read from llama.cpp's source at that tag
  - the card's own benchmark claims
  - for the top models, links to benchmark pages found on the web
- **It also checks llama.cpp itself:** whether a build at least two weeks old would add architectures.

## Staying on the best models

`serve` can look for better open models (`update_every: "24h"`; off by default, and off whenever `web` is `off`). A cycle reads public evaluations, installs a better plan beside the running one, checks it with a canary, and **stages** it. It never changes what is served; a person promotes. The steps:

1. **Scout.** Find new open models this machine could run (above).
2. **Read public evaluations.** Only independent leaderboards rate: LiveBench, ScreenSpot-Pro, BFCL and Epoch AI. A new model's own card is read only for the name its maker uses, so the leaderboards' rows can be matched to it.
3. **Rate.** A new model is rated per role against the catalog models the same leaderboard also scores: if it gets 50 where Qwen3.5-2B gets 40, and the catalog rates Qwen3.5-2B 34, it is about 42. The rating is the median over every such comparison inside one leaderboard, never across them.
   - A model that only its maker's card rates waits until a leaderboard lists it. The report says what the card claims, as context.
   - A model under a new name never inherits its parent's scores, whatever the Hub's tags say.
   - The catalog's own ratings are the only yardstick; ratings the updater made are never used to rate others.
4. **Adopt.** A model rated at least 3 points above the current pick for a role goes into `catalog.local.json`, marked `auto`, with the comparisons as its basis.
   Only a repo owned by a lab or quantiser on the trusted list (`internal/scout`) is adopted on its own. A repo's name and tags are its uploader's own words, so a stranger's upload under a real model's name waits for a person (`brainiac scout adopt`).
5. **Plan again** for the machine as it was planned (today's memory pressure is not a reason to downgrade). The plan goes on only if it is better overall and no core role is lost.
6. **Install beside the running models** (`lock.next.json`), only after 10 minutes with no requests and when the CPU is not busy.
7. **Check every changed role** with questions code can verify: the brain must answer 17 + 25 and repeat a word, a vision model must read a number from a picture, OCR must read it too, a math model must get 12 × 13 right, a code model must write `def add`, a SQL model a `count`, a translator must write Hindi, embeddings must come back the same shape, and speech must transcribe a second of silence without failing. This catches a model that does not load, cannot see, or answers in the wrong template.
8. **Stage.** The checked plan waits in `lock.next.json`, recorded in `update-staged.json` with the lock's sha256. Nothing served changes.

`brainiac update promote` is a person switching to the staged plan. It refuses when `lock.next.json` is not the plan that passed its check. `lock.json` becomes the staged lock, the old one is kept as `lock.prev.json`, and a newer llama.cpp the plan needs becomes the pin. Downloads neither lock uses are deleted from Brainiac's own store (never from a shared Ollama). A running `serve` notices that `lock.json` changed (within 30 s) and moves to it; requests already running finish on the old models.

A canary catches a model that does not load or uses the wrong template. It does not judge quality, so the box's judgement model changes only by a person's decision.

A model that fails its check is set aside for 30 days and the next best is tried. A check that cannot load for lack of memory waits for later instead.

The leaderboards cover the brain, code, math and pointing on a screen. No maintained independent leaderboard covers pictures, SQL, translation or medicine (OpenCompass's vision board stopped in September 2025, before the catalog's vision models), so new models for those wait for a person or for a new board.

`brainiac update` shows what a cycle would do, with every rating and its evidence. `brainiac update --apply` installs, checks and stages it by hand (stop `serve` first). `GET /v1/brainiac/update` returns the last report. With `update_every: "off"` models change only by hand (`brainiac scout adopt`, `brainiac scout pin`, `brainiac pull`).

## What a downloaded model can reach

A model file is untrusted input to a large native parser, and it carries a chat template that the server runs on every request.

- **Where a model comes from.** Only trusted uploaders are adopted automatically (above). A sha256 only proves a file is what its uploader published.
- **What is installed stays installed.** A model stays at the commit it was installed from; a push to its repo changes nothing here. `brainiac pull --latest` moves it on purpose.
- **Chat templates.** The first template a model installs is recorded in `templates.json` and its text kept under `templates/`, to read. A different one later stops the install, and the updater sets that model aside, until a person reads it and runs `brainiac update approve-template <model>`. A file whose template no longer matches its lock is not served. This catches a change; code cannot judge whether a model's first template is honest.
- **Secrets.** `llama-server` and the Python worker get a short list of environment variables, none of them Brainiac's keys. `pip` installs for the worker run without any `*_KEY` or `*_TOKEN`.
- **Downloads.** A GitHub asset with no digest from GitHub and no sha256 in the catalog is refused (fail closed). A llama.cpp build is reused only when its install record shows it intact.
- **Network and files.** On Linux each model process (llama-server, the Python worker) runs with no network and in its own mount namespace, reached through a Unix socket only this user can open. Its root holds only the system's read-only directories (`/usr`, `/lib*`, `/bin`, `/sbin`, `/opt`, `/etc`, `/sys`, a few `/proc` files), its program and the libraries it loads from its build, its model files and its socket directory. Only the socket directory is writable, plus, for the worker, its venv and a temp folder in the store. Other programs' Unix sockets, this user's home and `/run` are not in its view. Its `/dev` holds only the plain devices (`null`, `zero`, `full`, `random`, `urandom`), the GPU nodes this machine has (`/dev/dri`, `/dev/kfd`, `/dev/nvidia*`, and `/dev/dxg` on WSL2) and a private `/dev/shm`. `/proc/driver/nvidia` is read-only where it exists.
- **Where that cannot work.** Where the host refuses unprivileged namespaces (an AppArmor rule, a container), Brainiac falls back to Landlock and lists what stays reachable in `/v1/brainiac/status` under `unconfined` (`sockets`, `udp`, `network`, `files`). Windows and macOS have neither, so `unconfined` lists more there. With `sandbox: required` (the default with `BRAINIAC_APPLIANCE`) Brainiac refuses to start a process it cannot fully confine. `sandbox: off` is for a GPU driver that needs a path the sandbox does not allow; it is refused on an appliance.
- **Lifetime.** On Linux, model processes die with Brainiac (a parent-death signal, set from a locked OS thread), and `serve` kills llama-servers a crashed Brainiac left behind.
- **The headless browser** is not a model process: it gets a throwaway profile, a 45-second budget and the guard proxy, but no sandbox (N-W4-8 in follow-ups).

Not covered: a hostile model can still give bad answers; that is what Decurix's final gate and the approval gate are for. Where the fallback applies, a model process can reach what `unconfined` lists. The open items for an appliance (a dedicated model user, an AppArmor profile, a signed catalog) are in [docs/follow-ups.md](../docs/follow-ups.md), O-3.

## Commands

| Command | Does |
|---|---|
| `brainiac census` | Measures CPU and instruction sets, memory pools (cgroup and WSL aware), GPUs, disk, memory bandwidth, the local Ollama and its models, network, Python |
| `brainiac plan [--verbose]` | Chooses a model per role and says why, and why every other candidate was not chosen |
| `brainiac pull [--yes] [--latest]` | Downloads the plan (pinned commits, sha256-checked, resumable) and writes `lock.json`; `--latest` moves installed models to their repo's newest commit |
| `brainiac serve` | Serves what is installed |
| `brainiac up [--yes]` | All of the above; what `build.sh` runs |
| `brainiac ask "..." --image f --audio f [--model m]` | Asks a running Brainiac, streaming the answer |
| `brainiac status` | What a running Brainiac holds in memory |
| `brainiac paths` | Per field: which local models have earned it, and why each has or has not |
| `brainiac eval --cases f.jsonl [--models a,b] [--learn]` | Measures models field by field on checked cases; the server uses the results to route |
| `brainiac update [--apply] [--json]` | Rates new open models from public evaluations and says what would change; `--apply` installs, checks and stages it (never switches) |
| `brainiac update promote` | A person switches to the staged plan; a running `serve` follows within 30 s |
| `brainiac update approve-template <model>` | Accepts a model's changed chat template, after a person has read it |
| `brainiac scout [--days 120]` | Finds and checks open models newer than the catalog |
| `brainiac scout adopt <repo> --roles r=q` / `scout pin <tag>` | Adds a scouted model to the local catalog / moves llama.cpp |
| `brainiac version` | The build's version |

Plan flags: `--offline` (only what is installed), `--licenses permissive,restricted`, `--reserve GB` (memory to keep free), `--planned-load GB` (memory desktops will need later), `--domains math,law` or `--domains none`. Commands that read the store take `--home`; those that ask a running Brainiac take `--url`, and `ask` and `eval` take `--key-env`.

## API

Default address `http://127.0.0.1:8484`.

- `POST /v1/chat/completions`: messages with text, `image_url` (inline `data:` URLs only; Brainiac never fetches a URL), `input_audio`, or `file` (images and audio). Streaming or not.
- `POST /v1/audio/transcriptions`: multipart `file`, optional `language`, `response_format` json, text or verbose_json.
- `POST /v1/embeddings`
- `POST /v1/brainiac/ocr` with `{"image": "data:image/png;base64,..."}`: text lines with boxes and confidence.
- `GET /v1/models`, `/v1/brainiac/status`, `/v1/brainiac/traces[/{id}]`, `/v1/brainiac/scout`, `/v1/brainiac/update`, `/healthz`

- `POST /v1/brainiac/route` with `{"text"}`: where the router would send it (no model runs). `brainiac eval` uses it.
- `GET /v1/brainiac/paths`: per field, what could answer here and why (`brainiac paths`).
- `POST /v1/brainiac/outcomes {"ok", "trace" | "since"}`: a client's final verdict on real answers (above).
- `POST /v1/brainiac/search {"query", "n"}`, `POST /v1/brainiac/fetch {"url", "render", "query", "max_chars"}`: the browser (above).

Model names: `brainiac` (the configured mode, with field routing), `brainiac-local`, `brainiac-hybrid`, `brainiac-remote`, `brainiac-<field>` (that field's specialist), `remote` (the remote brain alone), or an installed model id from `/v1/models` (that model alone). Brainiac's extra request fields: `"domain"` names the field directly; `"web"` and `"web_query"` ask for a web lookup with the client's own query (above); `"think"` turns a local model's thinking on or off (without it, `reasoning_effort` `medium` or `high` turns it on).

Every answer carries a `brainiac` object: the trace id, the mode, the brain, every job that ran (model, time, cache hit, load time, tokens), the field and the router's confidence when one was chosen, what the checks found, and notes (why a web lookup did or did not happen, a fallback). `usage` adds `local_tokens` and `remote_tokens`. A trace holds sizes and timings only, never content.

Tool calling is not supported yet: a request with `tools` gets a 400 that says so.

## Modes and the remote brain

| Mode | Brain | Pictures leave the machine |
|---|---|---|
| `local` | the local model chosen for this machine | never |
| `hybrid` | the remote brain, given text evidence the local models found | only with `share_media: true` |
| `remote` | the remote brain, which sees pictures itself (OCR and speech still run here) | yes, when the remote is marked as seeing (`vision`, `BRAINIAC_REMOTE_VISION`) |

A remote that is not marked as seeing gets text evidence in every mode.

The remote brain is reached the way TheBot reaches its hosted brains:

```bash
BRAINIAC_REMOTE=decurix:azure   # or decurix:claude: through Decurix's control plane, keys stay sealed there
BRAINIAC_REMOTE=azure           # Azure AI directly: AZURE_ENDPOINT, key in AZURE_KEY, model gpt-5.4
BRAINIAC_REMOTE=openai BRAINIAC_REMOTE_URL=... BRAINIAC_REMOTE_KEY_ENV=...
```

`BRAINIAC_REMOTE_MODEL` names the model, and `BRAINIAC_REMOTE_URL` overrides the default address (Decurix's control plane at `http://127.0.0.1:8080`). In `config.json` the same settings live under `remote` (`kind`, `url`, `provider`, `model`, `key_env`, `api_version`, `vision`), and `usd_per_mtok_in` / `usd_per_mtok_out` let a trace report what the remote answers cost.

If the remote brain fails, the request fails with the status. Set `local_fallback: true` to answer with the local brain instead; the trace says so in a fixed note (the HTTP status when there is one, never the provider's error text, which goes to the log). A weaker answer is never passed off as the remote brain's.

## Settings

`~/.brainiac/config.json`; environment variables win.

| Setting | Env | Default |
|---|---|---|
| `listen` | `BRAINIAC_LISTEN` | `127.0.0.1:8484` |
| `api_key_env` | `BRAINIAC_API_KEY_ENV` | none: allowed only on loopback |
| `mode` | `BRAINIAC_MODE` | `local`, or `hybrid` when a remote is set |
| `ollama_url` | `OLLAMA_URL` | `http://127.0.0.1:11434` |
| `share_media` | `BRAINIAC_SHARE_MEDIA` | false |
| `local_fallback` | | false: when the remote brain fails, the request fails |
| `licenses` | | permissive only |
| `reserve_gb`, `planned_load_gb` | | the selector's default; the plan flags `--reserve` and `--planned-load` win |
| `threads` | | half the logical CPUs, at least 1 (llama-server's `-t`, the worker's `OMP_NUM_THREADS`) |
| `image_max_side` | | 1280 for local vision models (-1 never shrinks) |
| `max_local_seconds` | `BRAINIAC_MAX_LOCAL_SECONDS` | 0 (no limit): an earned path slower than this on average leaves the field with the remote brain |
| `web` | `BRAINIAC_WEB` | `on-request` (`auto`, not in local mode; `off` also stops updates) |
| `searx_url` | `BRAINIAC_SEARX_URL` | none: a SearXNG instance to search first |
| `brave_key_env` | `BRAINIAC_BRAVE_KEY_ENV` | none: the variable holding a Brave Search key |
| `web_render` | | true (false on an appliance): whether `fetch` may use the headless browser |
| `update_every` | `BRAINIAC_UPDATE_EVERY` | `off` (`24h`: install, check and stage a better plan daily; never switches) |
| `sandbox` | `BRAINIAC_SANDBOX` | empty: confine as far as the system allows; `required`: start no model process that is not fully confined (the default with `BRAINIAC_APPLIANCE`); `off`: refused on an appliance |
| (none) | `BRAINIAC_APPLIANCE` | set on an office box: sandbox required, `web_render` off, an API key required even on loopback |

A key is never stored in the config, only the name of the variable that holds it. Brainiac refuses to listen beyond loopback without an API key, because it controls this machine's memory and disk.

## Layout

```
cmd/brainiac/        the command
internal/census/     measures the machine
internal/catalog/    the curated model list (catalog.json, embedded) with quality priors and their sources
internal/selector/   chooses a model per role and explains it
internal/hub/        Hugging Face and GitHub downloads: pinned, sha256-checked, resumable, safe unpacking
internal/install/    installs a plan, writes lock.json
internal/engine/     atomizer, residency (memory), llama-server processes, evidence, the chat flow
internal/backend/    Ollama and OpenAI-compatible clients (llama-server, Azure, the Decurix proxy)
internal/worker/     Go client of the Python worker
workers/python/      brainiac_worker.py: OCR (RapidOCR) and speech (sherpa-onnx), ONNX Runtime only
internal/api/        the HTTP surface
internal/verify/     checks in code (figures in an answer that no source has)
internal/router/     the field router (naive Bayes over words and text shapes, no model)
internal/web/        the browser: search, safe fetch, headless render, passages, query scrubbing
internal/scout/      finds and checks new open models
internal/bench/      public evaluations: leaderboards and model-card tables, ratings against the catalog
internal/update/     the update cycle: rate, adopt, plan, install aside, check, stage; Promote is the one switch
internal/evals/      checked cases, scoring, evals.json
internal/gguf/       reads the chat templates in a GGUF header, with every length bounded
internal/isolate/    what a model process can reach: its environment and, on Linux, its network and mount view
internal/{config,store,cache,trace}/
```

Tests: `cd brainiac && go test ./...` and `python -m unittest discover -s workers/python`. Live tests are opt-in with `BRAINIAC_LIVE=1`; the llama-server ones need `BRAINIAC_LIVE_LLAMA` (a llama-server) and `BRAINIAC_LIVE_GGUF` (a small model), and the browser one `BRAINIAC_LIVE_RENDER`.

See [DESIGN.md](DESIGN.md) for why it is built this way and what comes next.
