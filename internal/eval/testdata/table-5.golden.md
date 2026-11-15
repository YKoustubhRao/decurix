# Eval e1: sample

**INVALID: a model changed during the run.** 1 row(s) were served by another deployment than the run pinned; they are left out of every rate below and counted apart.

- Suite: `sample`, SHA-256 `abc123`
- Date: 2026-11-04 09:30 UTC; repeats: 5; status: done
- State: reset to the snapshot before every case (independent samples)
- Bookkeeping: a case run whose model made no turn (every call failed at the provider) is invalid and run again, at most 2 more times, by one rule for every arm; a case run left out of one model arm is left out of every model arm; a run that errored (error, timeout, token budget) counts 0 in pass and is left out of tokens and minutes; efficiency is compared in tokens, never in money
- Arms:
  - `default`: azure default → big-1; no expert; agent tree image; image sha256:0123456789ab; served by big-1; provider errors 0; no-turn reruns 0
  - `fast+expert`: azure fast → small-1; with the expert (big-1); agent tree eb15981 (feedfacecafe); image sha256:0123456789ab; served by small-1, small-2; provider errors 0; no-turn reruns 0

## Pass (k/n, Wilson 95% lower bound)

| case | default | fast+expert |
|---|---|---|
| one | 5/5 (lo 0.57) | 5/5 (lo 0.57) |
| two | 3/4 (lo 0.30) | 4/4 (lo 0.51) |
| **total** | **8/9 (lo 0.56)** | **9/9 (lo 0.70)** |

## False done (k/n, Wilson 95% upper bound); done unchecked and wrong

| case | default | fast+expert |
|---|---|---|
| one | 0/5 (hi 0.43); 0 | 0/5 (hi 0.43); 0 |
| two | 1/4 (hi 0.70); 0 | 0/4 (hi 0.49); 0 |
| **total** | **1/9 (hi 0.44); 0** | **0/9 (hi 0.30); 0** |

## Questions per run (required / other / declined approvals)

| case | default | fast+expert |
|---|---|---|
| one | 0.0 / 0.0 / 0.0 | 0.0 / 1.0 / 0.0 |
| two | 1.0 / 0.0 / 0.0 | 1.0 / 0.0 / 0.0 |
| **total** | **0.4 / 0.0 / 0.0** | **0.4 / 0.6 / 0.0** |

## Tokens per run (median, p90; cached share of input), runs that errored left out

| case | default | fast+expert |
|---|---|---|
| one | 204.0k, 204.0k; 75% | 92.0k, 92.0k; 67% |
| two | 306.0k, 306.0k; 33% | 102.5k, 102.5k; 50% |
| **total** | **204.0k, 306.0k; 52%** | **92.0k, 102.5k; 59%** |

## Minutes per run (median, runs that errored left out); rows left out: invalid, infrastructure, no model turn, paired

| case | default | fast+expert |
|---|---|---|
| one | 6.0; 0, 0, 0, 0 | 2.0; 0, 0, 0, 0 |
| two | 5.0; 0, 0, 0, 1 | 4.0; 1, 0, 0, 0 |
| **total** | **5.0; 0, 0, 0, 1** | **2.0; 1, 0, 0, 0** |

