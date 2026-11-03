# Brainiac design

Status (2026-10-24): v0 is built, with unit tests for every package. OCR and speech have run live on this laptop. The full model run (downloading the 3.2 GB plan and serving it) is next.

Review (2026-11-03): the web is read only on the client's own query, updates only stage, earned paths need decided cases and routed scores, streams must finish explicitly, and each model process runs in its own mount view. What is still owed before an appliance ships is in [docs/follow-ups.md](../docs/follow-ups.md) (O-3, N-W4-8).

## The goal

One endpoint, tokens in and tokens out, that takes text, pictures and audio. Behind it are many open models, each the best that fits this machine at one kind of work. They should match frontier quality wherever the work can be split into steps that can each be checked, and say plainly where they cannot.

## What is and is not possible

These findings decide the design. The prior-art survey (October 2026) found:

- **Specialists already match frontier models at their one job.**
  - OCR: PP-OCRv6 and PaddleOCR-VL lead OmniDocBench.
  - Speech: Parakeet v3 has about 6% English WER, better than frontier APIs.
  - Screen grounding: small grounding models beat frontier generalists on ScreenSpot-Pro.
  - Embeddings, voice detection and arithmetic in code.

  This is where many small models really do reach frontier quality.
- **Small models match frontier models only on short, checkable steps, and only with a checker in the loop.** The checker can be unit tests, a code-level check, or a strong model. Every orchestrator that beats the frontier still calls a frontier model for the hard part. Learned routers plateau at about "the best single model".
- **Sampling the best model several times, with a vote or a verifier, beats mixing weak models.** With an imperfect verifier, no amount of weak-model resampling matches one strong-model sample.
- **Long-horizon planning and the final judgement belong to the strongest model or a person.** This matches Decurix's rule: never hand a judgement to a smaller model to save tokens.

So Brainiac does not claim a 2B model on a laptop is GPT-5. It claims three things:

1. Perception (pixels and audio to text, with positions, times and confidence) runs locally at frontier quality.
2. The brain reasons over checked text evidence instead of raw pixels, which small models misread most.
3. Every answer says what made it.

Where the local brain is too small, the remote brain (GPT-5.4 or Claude through Decurix) answers, with perception still local and private.

## Architecture

```
client ──► api ──► engine.Chat
                     │ route: mode + requested model → brain (local unit or remote)
                     │ Atomize (rules, no model): picture → ocr (+ describe if the brain can't see)
                     │                            audio   → asr (voice detection inside the worker)
                     │ run atoms in model order (worker first, then vision): cache → residency → model
                     │ brainMessages: each picture/recording replaced in place by <evidence> blocks
                     │ brain (streams) ──► verify (figures no source has: advisory)
                     └► trace (no content), usage split local/remote
```

**Census → catalog → selector → install → lock → engine.** `build.sh` runs them in that order. The plan is deterministic: the same census, catalog and policy always give the same choice, and every choice and every rejection has a reason in words.

### Choosing models

- **Measure, don't trust spec sheets.** This laptop's memory is rated at 43 GB/s. A copy test gives 16–19 GB/s, and a model gets about half of that. Decode speed is roughly the bandwidth divided by the active weight bytes per token. That is why mixture-of-experts models (few active weights) are what make bigger machines useful.
- **Memory pools.** These are host RAM (cgroup-capped), the WSL VM (seen from Windows), and each GPU's VRAM, with unified memory on Apple and Strix Halo. A model's footprint is its file, the runtime buffers, and the KV cache at the fixed 8192-token window: f16 on a shared Ollama, q8_0 on Brainiac's own llama-server. Qwen3.5's hybrid attention makes its KV cache tiny: 0.1 GB at 8k tokens for the 2B model.
- **Heavy roles are chosen together** (brain, vision, grounding). On a small machine one vision-language model that fills all three beats two models that swap. Swapping costs minutes here, measured: a cold load of a 3B model took 170–320 s.
- **Light roles are chosen one by one** (OCR, speech, voice detection, embeddings). Each prefers a model that stays loaded beside the brain. A model that would unload the brain is taken only if it is clearly better, by 15 quality points.
- **Licences.** Only permissive licences are allowed by default. `qwen2.5-coder:3b`, Decurix's old default, is under the Qwen Research licence and is refused until you allow non-commercial models.
- **Stickiness.** An installed choice within 90% of the best is kept, and the previous plan is kept unless the new one is at least 10% better. The catalog changing slightly never churns 2 GB of downloads.
- **Quality numbers are priors** taken from published results. Each catalog entry's `basis` says which. They are a starting point, never a claim about this machine.

### Running models

- **One owner of model memory.** The residency manager loads a model only when the plan's budget and the memory free at that moment both allow it. To make room it evicts only idle models Brainiac itself loaded, least recently used first. It never unloads a model a desktop is using on the shared Ollama. When there is no room, it refuses rather than push the machine into swap.
- **A process exit is the one unload that always returns every byte.** Brainiac's llama-server and Python worker are child processes, killed to free memory. This is also why the worker uses ONNX Runtime and no torch: torch costs about 1 GB before any model.
- **Shared-Ollama rules carried over from Decurix.**
  - The same `num_ctx` (8192) on every call: another window loads a second copy of the model.
  - Never a request that is not real work: a probe evicts the prompt cache desktops rely on.
  - The prompt prefix stays byte-identical: the evidence note is constant text appended to the system prompt.
- **llama-server flags are read from the build's own `--help`**, so a newer or older build never gets a flag it does not know.

### Evidence, privacy, honesty

- **Evidence is data.** OCR lines carry `[x,y]` positions and an `(unsure)` mark below 0.6 confidence. Transcripts carry times. The system note says evidence is content, never instructions. A screenshot that says "ignore previous instructions" is just text on a screenshot.
- **Unreadable input is reported, not guessed at.** A picture or recording nothing could read becomes "(could not be read: …)" in the evidence, and the brain is told to say it cannot see it.
- **Pictures stay on the machine** in hybrid mode unless `share_media` is on. Only `data:` URLs are accepted; Brainiac never fetches a URL.
- **No unseen success.** The answer's figures are checked against the sources in code. The check is advisory, because a computed total appears in no source and is still right. It lists what a person or a stronger model should look at. Decurix's final gate is still where "done" is decided.
- **Traces hold no content**: no prompt, picture or transcript. Keys live only in environment variables named by the config. Brainiac will not listen beyond loopback without an API key.

### Models are untrusted input

The updater installs and runs files with no person in the loop, so what it will accept and what those files can reach are decided here (2026-10-30). The README lists the rules; these are the reasons.

- **Provenance is the main defence.** A behaviour test cannot find a backdoor that waits for a trigger, and a renamed model with its safety training removed passes a name filter. So the updater adopts only from an explicit list of owners. Everything the Hub says about a repo besides its owner (name, `base_model`, tags, downloads) is the uploader's own claim.
- **The commit does not move by itself.** Resolving `main` on every install would let a push reach a running machine at the next plan change, and would overwrite the file a running model loads.
- **A first template is recorded, not judged.** Approving every new model's template would end unattended updates, which is the owner's decision; judging one in code would be a guess. Changes stop for a person. A quantiser's template often differs from its lab's for good reasons, so comparing the two would mostly raise false alarms.
- **The list of environment variables says what to keep.** No list of secrets' names is ever complete.
- **Isolation fails open only where it says so, and never on an appliance.** Refusing to serve on a system that blocks unprivileged namespaces (some Ubuntu AppArmor settings, some containers) would stop the product there, so by default Brainiac falls back to Landlock and lists what stays reachable in `status.unconfined`. With `sandbox: required` (the default on an appliance) it starts no model process it cannot fully confine. The clean environment always applies. (Review 2026-11-03.)
- **A network namespace does not hide a pathname Unix socket, and Landlock has no right for connecting to one.** Only a mount view without the socket hides it, so each model process gets its own root: its build, its model files, its socket directory, the plain devices, the GPU's nodes and a private `/dev/shm`.
- **A model process gets its program and the libraries it loads from its build, not its build's directory.** They are read from the ELF: `DT_NEEDED` looked up through `DT_RPATH`/`DT_RUNPATH`, with `$ORIGIN` expanded as glibc does, transitively, read-only. What is guaranteed is where each granted file is and why: it is beneath the build both by the path the loader opens and by its real path (a symlink that points out is refused), it is named by an ELF's own `DT_NEEDED` or is a plugin, and no directory is granted. The `ET_DYN` header check is a sanity filter, not proof of a library: a position-independent executable passes it, and so does any file that starts with such a header. The build is the program's directory, or the parent of a `bin` directory (an install tree with `../lib`), not `/`, `/usr` or `/usr/local`; that root only bounds which exact files may be granted, and is never granted itself. ggml's backend modules (`libggml-*.so`) are opened at run time and named by no `DT_NEEDED`, so the server's policy adds them as plugins, held to the same rule, and lets it list its real build directory, which it does to find them. Listing holds beneath it; in a view it shows only what is granted. A view has no `/proc/self/exe`, from which the loader expands the program's `$ORIGIN`, so the helper passes those directories as `LD_LIBRARY_PATH`, in a view only: that variable comes before `DT_RUNPATH` for every object, which is safe because those directories hold only granted files there. Where the program's ELF cannot be read (or reading it panics), its directory is granted read-only only if it is strictly inside Brainiac's install store (`bin/`, the venv) and is not `/`, the home or the temporary directory; otherwise the program alone is granted, and it may not start. Either way the log says so. (FIX-BR-1, 2026-11-04.)

### Field specialists

The idea: a niche model for every field, each beating the frontier in its field. It holds only partly, and the design is built around which part.

- **Where open specialists beat or match frontier models:** narrow fields whose answers can be checked, such as formal proofs, text-to-SQL, some translation pairs, and perception.
- **Where they do not:** open-ended reasoning in law, medicine or finance. There a 7B specialist trails GPT-5.4 and Claude.
- **On a small machine,** a specialist routinely beats the small general brain in its field. That is the case the plan installs for.

So the system:

1. **Installs a specialist only where it should help here.** It must beat the chosen brain in its field by 5 points of its published quality prior, and fit the machine. Every field without one is listed with the reason.
2. **Routes without a model.** The router is a naive Bayes classifier over words, word pairs and text shapes (code fences, LaTeX, equations, SQL, money, legal provisions, Indic scripts), trained on catalog examples and learned cases. Routing only reaches a specialist at confidence 0.35 or more. That threshold was tuned so held-out requests are never sent to the wrong field. A missed route costs only what the brain already costs.
3. **Lets measurement overrule priors.** `brainiac eval` scores models per field on checked cases.
   - A specialist takes a field from the remote frontier brain only when the accuracy its checked cases let it claim (Wilson lower bound, at least 20 cases) is at least the remote's. There is no margin. Only cases a stated number, letter or exact text decides count; `contains` and `regex` prove nothing.
   - A routed request earns on its routed score (`routed:<model>`): a case the router files under the wrong field is a miss there.
   - An earned path serves only requests shaped like its eval cases (one user turn, a short system prompt, no pictures, recordings or web lookup). An agent's long prompt is not an eval case.
   - A specialist measured worse than the local brain loses its field back to the brain.
   - A strict checker is not neutral in a comparison: marking the remote's verbose correct answers wrong lowers its score and makes a cheap path look as good. Case prompts should ask for a final `Answer: <value>` line.

   This is Decurix's maturity rule applied to models: a cheaper path is earned by checked runs, never assumed.

### Staying current

A model knows the world up to its training cutoff, and the open-model field moves monthly. A shared browser (`internal/web`) handles both problems.

- **Answers read the web only when the client asks, with its own query.** A phrase check deciding when data leaves the box was a word list in the one place a regex must not decide (review G-01, 2026-11-03). The client sends `web: true` and `web_query`; Brainiac scrubs the query and reads three pages as dated, cited `[web-N]` evidence. The figure check covers web evidence too.
  - The browser dials no internal address.
  - Only the client's own query and the page requests leave the machine.
  - The headless browser runs only when a client asks for it on `fetch`, and its own networking goes through a guard proxy inside Brainiac (Chrome's networking bypasses a Go dialer guard).
- **The arsenal updates itself from evidence, and a person switches.** `internal/update` can run a cycle (`update_every`, off by default): scout, read public evaluations, rate, adopt, plan, install aside, check, stage. Nothing switches on a claim alone, and nothing switches without a person: the updater stages when the canary passes; a person promotes; a signed release may promote in future. A canary catches a model that does not load or uses the wrong template. It does not judge quality. So the box's judgement model changes only by a person's decision.
  - **Ratings are ratios inside one independent leaderboard.** A new model's score divided by a catalog model's score, on the same benchmark in the same leaderboard, times the catalog's rating, gives one estimate. The rating is the median of these. Leaderboards are never compared with each other, because harnesses differ. Benchmarks map to roles by name (GPQA → brain, MMMU → vision, ScreenSpot → ground, SWE-bench → code, AIME → math).
  - **Guards against flattering numbers:**
    - independent leaderboards only (decided 2026-10-25): a maker's card is read for the name it uses, and its claims are reported as context, never rated
    - a 3-point margin over the current pick
    - the selector's own stickiness (10% to switch the heavy set)
  - **Coverage.** LiveBench, BFCL and Epoch AI cover the brain, code and math; ScreenSpot-Pro and OSWorld cover pointing. No maintained independent board covers pictures, SQL, translation or medicine. OpenCompass's vision board is frozen at September 2025 and lists none of the catalog's vision models. New models for those roles wait for a person or a new board.
  - **No drift.** Only the curated ratings are yardsticks; an auto-added model never rates another.
  - **Names.** Model names are normalised across sources: sizes in brackets kept ("Qwen3.5 (9B)"), precision suffixes dropped, "gemma-4" = "gemma4". A model's own column on its own card is found even when the maker drops the size or the org. A repo under a different name than its base is a different model and inherits nothing.
  - **Plan for the machine, not the minute.** Re-planning uses the memory and speed measured when the running plan was made, so a busy afternoon does not shrink the models. A plan is applied only if its changed roles gain quality in total and no core role is lost.
  - **The check is a canary, not a benchmark.** Public evaluations chose the model. The check catches the failures they cannot: a model that will not load, a broken chat template, a vision projector that does not work. It asks questions with answers code can verify. The vision check sends the picture straight to the model, because through the normal path OCR would answer for it.
  - **Switching is safe.** The new plan installs into `lock.next.json` only when Brainiac has been idle for 10 minutes and the CPU is not busy. It is checked and staged; after a person's `brainiac update promote` it is swapped in under live traffic. The old engine drains and the old lock is kept for rollback. A failed model is set aside for 30 days. A llama.cpp build moves only when a better model needs it, and is blamed when a model that ran before fails on it.

  The first live cycle (2026-10-24) on a 32 GB PC profile read about 1,300 independent scores and the cards of eight new models:
  - Mellum2.1 (its card: brain 46, code 49, math 69) and Nemotron-3.5-Bolt-E4B (its card: brain 40) are not on a leaderboard yet, so they wait. Nothing switched.
  - It caught one flattering case. Tiel-Coder, a renamed upload tagged as a quantisation of Ornith, at first inherited Ornith's card and rated 82 against Ornith's 78. It is now unrated.
  - MiniCPM-V-4.7 publishes its results only as chart images and is on no leaderboard, so it stays unrated.
  - On this laptop, every new model is too big for the 2.9 GB budget.

## Numbers for this laptop

These are for an i5-10210U with 8 GB of RAM, a 5 GB WSL cap, about 3.3 GB free with one desktop running, and no GPU.

| | |
|---|---|
| Copy bandwidth | 16 GB/s (Windows) to 19 GB/s (WSL) |
| Plan in WSL | Qwen3.5 2B (2.3 GB, about 3.5–6.6 tok/s), PP-OCRv6, Parakeet v3, Silero, Qwen3-Embedding; specialists VibeThinker 1.5B (math), flash-sqlite (SQL) and Hy-MT2 1.8B (translation); 6.7 GB to download |
| A hard math problem on VibeThinker 1.5B | about 13–26 min (a reasoning model's 6,000-token thought at 4–8 tok/s) |
| OCR, 1280×720 screen | 2.6–4.1 s warm, about 0.3 GB (measured) |
| Speech, 12.7 s clip | 1.3 s with voice detection, about 0.15 GB (measured, Moonshine tiny) |
| Plan natively on Windows | almost nothing fits (0.9 GB free): run inside WSL |

## What comes next

1. **Live run on this laptop,** once the GST series is not running: `wsl -d Ubuntu -- ./brainiac/scripts/build.sh`. Then measure real decode speed and feed it back to the selector as `DecodeGBps` (calibration).
2. **Parity suite.** The same tasks run through Brainiac (local and hybrid) and through gpt-5.4 alone, with ground truth from `decurix-tests`. It scores correct results, questions asked, tokens and time per kind of task. This is the number that says how close local really is. Held-out sets stay sealed.
3. **Recipes, earned like Decurix methods.** For a kind of request, a cheaper path (say, OCR plus a small brain instead of the remote brain) becomes the default only after N checked runs agree with the heavy path. It goes back to learning on one miss.
   - Record checked results, time and money per kind of work and path. Pick the cheapest path whose pass rate, at its lower confidence bound, is not below the heavy path's. `evals.Compare` uses raw accuracy today, so 20/20 beats 19/20 on what could be chance.
   - A cheaper path is tried in shadow only: the user always gets the heavy path's answer while it is being tested.
   - The hard part is the label, not the loop: a failed gate is not always the model's fault, and a model cannot grade itself.
   - Speculative decoding's acceptance rate says two models agree, not that the work is right, so it never earns a path. It can speed up a big model on the appliance tier, where the output stays the big model's; that needs measuring.
4. **Plan atoms with the brain** for compound requests: a typed step graph in constrained JSON, checked by code. This is Decurix's atomizer pattern: rules first, a model only when rules cannot decide. Long documents become map-reduce over pages with extraction and checks in code.
5. **More roles:**
   - Document OCR to Markdown: GLM-OCR (1.3B, MIT, about 1.4 GB) and PaddleOCR-VL 1.6 beat Gemini 3 Pro on OmniDocBench, and both fit this laptop.
   - Lean provers (Kimina-Prover 1.7B, OProver 8B), but only with a Lean checker in the loop. Lean is then the verifier.
   - IndicTrans2 for all 22 Indian languages. It needs CTranslate2, not llama.cpp.
   - A decision model as the router on bigger machines: vllm-sr d3-edge, 0.6B, Apache-2.0, served by llama.cpp's `/v1/systemone` from build 2026-10-12, newer than the pinned b11541.
   - Indic speech (Qwen3-ASR 0.6B is already wired in the worker; omnilingual CTC).
   - Text-to-speech (Kokoro).
   - Reranking.
   - A `/v1/brainiac/ground` route for Decurix's grounder.
   - Tool-call passthrough.
6. **More evidence for the updater:**
   - Independent leaderboards for pictures, SQL, translation and medicine, which no maintained board covers today.
   - The parity suite as a local gate: a switch that passed the canary is rolled back if the suite scores it worse on this hardware.
   - OCR, speech and embedding models still need a person, because they run on the worker, not llama.cpp.
   - Split GGUF files are not adopted automatically yet.
7. **Decurix integration.**
   - Point TheBot at Brainiac (`LLM_PROVIDER=openai`, `OPENAI_BASE_URL=http://<host>:8484`).
   - Point embeddings at `/v1/embeddings`. That removes the last Ollama dependency of hosted-brain desktops.
   - Let Brainiac own model memory instead of `internal/ollama/select.go`.
8. **The appliance tier.** The same code on a 128 GB box:
   - Several users at once, through vLLM batching.
   - Paired-device login.
   - Signed, eval-gated model updates. `update.Promote` is the one place a staged plan becomes served; a signed release can call it once there is a local parity eval.
   - Night-shift learning on the customer's own checked runs.

## Decisions for the owner


- **Licence policy.** Should restricted licences (Gemma terms, some research models) be allowed in plans for customers? The default is permissive only.
- **`share_media` default for hybrid mode.** It is off, for privacy. Correctness is better with it on for screen work, where frontier vision beats OCR plus a small model.
- **Whether Brainiac should replace Ollama** as the model owner on Decurix machines (step 7), or stay beside it.
