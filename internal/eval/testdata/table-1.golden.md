# Eval e1: sample

**n=1: not a result.** Fewer than 5 repeats show what happened, not a rate.

- Suite: `sample`, SHA-256 `abc123`
- Date: 2026-11-04 09:30 UTC; repeats: 1; status: done
- State: reset to the snapshot before every case (independent samples)
- Arms:
  - `default`: azure default → big-1; no expert; agent tree image; image sha256:0123456789ab; served by big-1
  - `fast+expert`: azure fast → small-1; with the expert (big-1); agent tree eb15981 (feedfacecafe); image sha256:0123456789ab; served by small-1

## Pass (k/n, Wilson 95% lower bound)

| case | default | fast+expert |
|---|---|---|
| one | 1/1 (lo 0.21) | 1/1 (lo 0.21) |
| two | 1/1 (lo 0.21) | 1/1 (lo 0.21) |
| **total** | **2/2 (lo 0.34)** | **2/2 (lo 0.34)** |

## False done (k/n, Wilson 95% upper bound); done unchecked and wrong

| case | default | fast+expert |
|---|---|---|
| one | 0/1 (hi 0.79); 0 | 0/1 (hi 0.79); 0 |
| two | 0/1 (hi 0.79); 0 | 0/1 (hi 0.79); 0 |
| **total** | **0/2 (hi 0.66); 0** | **0/2 (hi 0.66); 0** |

## Questions per run (required / other)

| case | default | fast+expert |
|---|---|---|
| one | 0.0 / 0.0 | 0.0 / 1.0 |
| two | 1.0 / 0.0 | 1.0 / 0.0 |
| **total** | **0.5 / 0.0** | **0.5 / 0.5** |

## Tokens per run (median, p90; cached share of input)

| case | default | fast+expert |
|---|---|---|
| one | 204.0k, 204.0k; 75% | 92.0k, 92.0k; 67% |
| two | 306.0k, 306.0k; 33% | 102.5k, 102.5k; 50% |
| **total** | **255.0k, 306.0k; 50%** | **97.2k, 102.5k; 58%** |

## Minutes per run (median); rows left out: invalid, infrastructure

| case | default | fast+expert |
|---|---|---|
| one | 4.0; 0, 0 | 2.0; 0, 0 |
| two | 5.0; 0, 0 | 4.0; 0, 0 |
| **total** | **4.5; 0, 0** | **3.0; 0, 0** |

