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
  - With a remote brain set up, a specialist takes a field only after at least 20 cases there show it does at least as well as the remote brain.
  - A specialist measured worse than the local brain loses its field back to the brain.

```bash
brainiac eval --cases ~/decurix-tests/brainiac/sample-cases.jsonl --learn
```

A case is one JSON line: `{"domain":"math","prompt":"...","check":{"kind":"number","value":"42"}}`. The check kinds are `number` (reads `\boxed{}`, "the answer is", or the last figure), `exact`, `contains`, `regex` and `choice`. `--learn` also teaches the router the cases' fields. Cases live outside the repo.

Reasoning specialists are given room to think (up to 6,144 tokens); on a CPU that is slow.

## The browser: fresh answers and a current arsenal

One web tool (`internal/web`) serves two jobs.

**Fresh answers.** When a request asks about the world now, the brain gets live web pages as evidence:
- What counts as "now": latest, today, news, a current price or rate, who holds an office, a new rule or version, or this year or last.
- The client can force it on with `"web": true` or off with `"web": false`.

How it works:
1. The question is searched: SearXNG or Brave if set, otherwise DuckDuckGo, then Bing.
2. The three best pages are read in parallel. A real headless browser (Chrome, Chromium or Edge) is used when a page has no text without JavaScript.
3. Only the passages about the question are kept.
4. The brain sees them as `<evidence kind="web" url=... retrieved=...>` and cites them as `[web-N]`.

Safety:
- Only a search query leaves the machine, never the conversation, and it is scrubbed of PAN, GSTIN, Aadhaar-like and account numbers, emails, phone numbers and secrets first.
- The browser never connects to a private, loopback or link-local address. The check runs on the address actually dialed, so a DNS answer cannot send it inside the network.
- Pages are size- and time-limited, and their text is evidence, never instructions.

The `web` setting is `auto` (default), `on-request` or `off`. Agents can use the same browser directly: `POST /v1/brainiac/search {"query"}` and `POST /v1/brainiac/fetch {"url", "render", "query"}`.

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

`serve` updates itself every 24 hours (`update_every`), with no one in the loop:

1. **Scout.** Find new open models this machine could run (above).
2. **Read public evaluations.** Only independent leaderboards rate: LiveBench, ScreenSpot-Pro, BFCL and Epoch AI. A new model's own card is read only for the name its maker uses, so the leaderboards' rows can be matched to it.
3. **Rate.** A new model is rated per role against the catalog models the same leaderboard also scores: if it gets 50 where Qwen3.5-2B gets 40, and the catalog rates Qwen3.5-2B 34, it is about 42. The rating is the median over every such comparison inside one leaderboard, never across them.
   - A model that only its maker's card rates waits until a leaderboard lists it. The report says what the card claims, as context.
   - A model under a new name never inherits its parent's scores, whatever the Hub's tags say.
   - The catalog's own ratings are the only yardstick; ratings the updater made are never used to rate others.
4. **Adopt.** A model rated at least 3 points above the current pick for a role goes into `catalog.local.json`, marked `auto`, with the comparisons as its basis.
5. **Plan again** for the machine as it was planned (today's memory pressure is not a reason to downgrade). The plan is applied only if it is better overall and no core role is lost.
6. **Install beside the running models** (`lock.next.json`), only after 10 minutes with no requests and when the CPU is not busy.
7. **Check every changed role** with questions code can verify: the brain must answer 17 + 25 and repeat a word, a vision model must read a number from a picture, OCR must read it too, a math model must get 12 × 13 right, a translator must write Hindi. This catches a model that does not load, cannot see, or answers in the wrong template.
8. **Switch.** New requests go to the new models, and requests in progress finish on the old ones. The old lock stays as `lock.prev.json`; downloads neither lock uses are deleted from Brainiac's own store (never from a shared Ollama).

A model that fails its check is set aside for 30 days and the next best is tried. A check that cannot load for lack of memory waits for later instead.

The leaderboards cover the brain, code, math and pointing on a screen. No maintained independent leaderboard covers pictures, SQL, translation or medicine (OpenCompass's vision board stopped in September 2025, before the catalog's vision models), so new models for those wait for a person or for a new board.

`brainiac update` shows what the next cycle would do, with every rating and its evidence. `GET /v1/brainiac/update` returns the last report. Set `update_every: "off"` to change models only by hand (`brainiac scout adopt`, `brainiac scout pin`).

## Commands

| Command | Does |
|---|---|
| `brainiac census` | Measures CPU and instruction sets, memory pools (cgroup and WSL aware), GPUs, disk, memory bandwidth, the local Ollama and its models, network, Python |
| `brainiac plan [--verbose]` | Chooses a model per role and says why, and why every other candidate was not chosen |
| `brainiac pull [--yes]` | Downloads the plan (pinned commits, sha256-checked, resumable) and writes `lock.json` |
| `brainiac serve` | Serves what is installed |
| `brainiac up [--yes]` | All of the above; what `build.sh` runs |
| `brainiac ask "..." --image f --audio f` | Asks a running Brainiac, streaming the answer |
| `brainiac status` | What a running Brainiac holds in memory |
| `brainiac eval --cases f.jsonl [--learn]` | Measures models field by field on checked cases; the server uses the results to route |
| `brainiac update [--apply]` | Rates new open models from public evaluations and says what would change; `--apply` switches now (a running `serve` does it on its own) |
| `brainiac scout [--days 120]` | Finds and checks open models newer than the catalog |
| `brainiac scout adopt <repo> --roles r=q` / `scout pin <tag>` | Adds a scouted model to the local catalog / moves llama.cpp |

Plan flags: `--offline` (only what is installed), `--licenses permissive,restricted`, `--reserve GB` (memory to keep free), `--planned-load GB` (memory desktops will need later), `--domains math,law` or `--domains none`.

## API

Default address `http://127.0.0.1:8484`.

- `POST /v1/chat/completions`: messages with text, `image_url` (inline `data:` URLs only; Brainiac never fetches a URL), `input_audio`, or `file` (images and audio). Streaming or not.
- `POST /v1/audio/transcriptions`: multipart `file`, optional `language`, `response_format` json, text or verbose_json.
- `POST /v1/embeddings`
- `POST /v1/brainiac/ocr` with `{"image": "data:image/png;base64,..."}`: text lines with boxes and confidence.
- `GET /v1/models`, `/v1/brainiac/status`, `/v1/brainiac/traces[/{id}]`, `/v1/brainiac/scout`, `/v1/brainiac/update`, `/healthz`

Model names: `brainiac` (the configured mode, with field routing), `brainiac-local`, `brainiac-hybrid`, `brainiac-remote`, `brainiac-<field>` (that field's specialist), `remote` (the remote brain alone), or an installed model id from `/v1/models` (that model alone). The extra request field `"domain"` names the field directly.

Every answer carries a `brainiac` object: the trace id, the mode, the brain, every job that ran (model, time, cache hit, load time, tokens), and what the checks found. `usage` adds `local_tokens` and `remote_tokens`. A trace holds sizes and timings only, never content.

Tool calling is not supported yet: a request with `tools` gets a 400 that says so.

## Modes and the remote brain

| Mode | Brain | Pictures leave the machine |
|---|---|---|
| `local` | the local model chosen for this machine | never |
| `hybrid` | the remote brain, given text evidence the local models found | only with `share_media: true` |
| `remote` | the remote brain, which sees pictures itself (OCR and speech still run here) | yes |

The remote brain is reached the way TheBot reaches its hosted brains:

```bash
BRAINIAC_REMOTE=decurix:azure   # or decurix:claude: through Decurix's control plane, keys stay sealed there
BRAINIAC_REMOTE=azure           # Azure AI directly: AZURE_ENDPOINT, key in AZURE_KEY, model gpt-5.4
BRAINIAC_REMOTE=openai BRAINIAC_REMOTE_URL=... BRAINIAC_REMOTE_KEY_ENV=...
```

If the remote brain fails, the request fails with the status. Set `local_fallback: true` to answer with the local brain instead; the trace says so. A weaker answer is never passed off as the remote brain's.

## Settings

`~/.brainiac/config.json`; environment variables win.

| Setting | Env | Default |
|---|---|---|
| `listen` | `BRAINIAC_LISTEN` | `127.0.0.1:8484` |
| `api_key_env` | `BRAINIAC_API_KEY_ENV` | none: allowed only on loopback |
| `mode` | `BRAINIAC_MODE` | `local`, or `hybrid` when a remote is set |
| `ollama_url` | `OLLAMA_URL` | `http://127.0.0.1:11434` |
| `share_media` | `BRAINIAC_SHARE_MEDIA` | false |
| `licenses` | | permissive only |
| `image_max_side` | | 1280 for local vision models |
| `web` | `BRAINIAC_WEB` | `auto` (`on-request`, `off`; off also stops updates) |
| `update_every` | `BRAINIAC_UPDATE_EVERY` | `24h` (`off`: models change only by hand) |

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
internal/update/     the self-update cycle: rate, adopt, plan, install aside, check, switch
internal/evals/      checked cases, scoring, evals.json
internal/{config,store,cache,trace}/
```

Tests: `cd brainiac && go test ./...` and `python -m unittest discover -s workers/python`. Live tests are opt-in with `BRAINIAC_LIVE=1`.

See [DESIGN.md](DESIGN.md) for why it is built this way and what comes next.
