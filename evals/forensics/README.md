# Forensics lanes: where a run's tokens, questions, stops and rework went

Read a **copy** of a control plane's `decurix.db` (never the live file: copy it while the control plane is stopped,
or `sqlite3 decurix.db ".backup copy.db"`) and, optionally, its run bundles, and recompute the observed figures of
the work it holds. Standard library only; no model, no network; the database is opened read-only (`mode=ro`).

```
py -3 evals/forensics/lanes.py --db C:/copies/decurix.db
py -3 evals/forensics/lanes.py --db C:/copies/decurix.db --bundles C:/copies/blobs --origin eval --lane halts
```

`--bundles` is the control plane's blob store (the folder holding `sha256/`, read through `run_bundles.files`) or a
folder of `<execution id>/<bundle path>` files. `--origin` keeps one origin's executions (person, eval, practice,
schedule, ...). Each lane writes `<out>/<lane>.json` (default `evals/results/forensics/`, git-ignored) and prints its
figures.

| lane | reads | reports |
|---|---|---|
| `tokens` | executions (checked = 1), `llm_calls` | tokens per checked task by kind of work (the taught skill, else the task type) and by purpose tag, the cached share; the desktop's own totals apart |
| `questions` | interventions, `query_notes`, `eval_rows` | questions and approvals per run, by kind of work; questions asked again on the same worker (by hash, never the text); the eval's graded questions |
| `gates` | executions.verdict, interventions, `query_notes`, `eval_rows` | final-check verdicts by gate; approval-gate stops; done never checked; false-done candidates (MODELED) beside the eval's graded false done |
| `halts` | `run_bundles` + soul's trace in each bundle | replay outcomes of `gui do` / `gui replay`; halts grouped by their own words, numbers, quotes and ids folded |
| `rework` | executions, tasks, `llm_calls`, `eval_rows` | retries and what they spent; the same request sent again within a day; the eval's no-turn reruns; the retry overhead (MODELED) |

## The rules

- **Every figure is OBSERVED or MODELED.** OBSERVED is counted from rows the control plane or the desktop kept;
  MODELED is a rule's lead or an estimate. The two are never added (`common.add` refuses it), and no lane prints a
  single total saving.
- **Two sources of tokens, never added.** The proxy's calls (`llm_calls`: the provider's reported usage) and the
  desktop's own report on its final reply (`executions.*_tokens`: the worker's word) are both OBSERVED and kept apart.
- **A call belongs to an execution by worker and time**, as the run record joins it (docs/trajectory.md): from 2 s
  before its start to a minute after its end, never into the worker's next execution. The task id in a call's header
  is the desktop's word and is not used.
- **Each lane keeps its own caveats** in its JSON. Read them before quoting a number.
- **Nothing private is written out**: no request, question or reply text; a repeated one is shown by a short hash.

## Tests

`evals/tests/test_forensics.py` builds a tiny database from the real schema (every Migrate step of `internal/tasks`,
`internal/llmrec`, `internal/runs`, `internal/querynotes` and `internal/eval`, read from the Go source in order) and
checks each lane's figures, the read-only open, the never-added rule and the bundle reads. A renamed column fails there
first.

```
py -3 -m unittest discover -s evals/tests -p test_forensics.py
```

## Not here

- Tokens by phase inside a skill run (gather, fill, replay): not kept per call; the purpose tag is the nearest.
- A replay run by a taught skill's engine outside soul: its outcome is not in soul's trace.
- Why a retry was asked for: the records keep that it happened, not who wanted it.
