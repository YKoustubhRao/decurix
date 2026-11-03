# Eval e1: sample

- Suite: `sample`, SHA-256 `abc123`
- Date: 2026-11-04 09:30 UTC; repeats: 5; status: done
- Arms:
  - `default`: azure default → big-1; no expert; agent tree image; image sha256:0123456789ab; served by big-1
  - `fast+expert`: azure fast → small-1; with the expert (big-1); agent tree eb15981 (feedfacecafe); image sha256:0123456789ab; served by small-1, small-2

## Pass (k/n, Wilson 95% lower bound)

| case | default | fast+expert |
|---|---|---|
| one | 5/5 (lo 0.57) | 5/5 (lo 0.57) |
| two | 3/5 (lo 0.23) | 4/5 (lo 0.38) |
| **total** | **8/10 (lo 0.49)** | **9/10 (lo 0.60)** |

## False done (k/n, Wilson 95% upper bound); done unchecked and wrong

| case | default | fast+expert |
|---|---|---|
| one | 0/5 (hi 0.43); 0 | 0/5 (hi 0.43); 0 |
| two | 2/5 (hi 0.77); 0 | 0/5 (hi 0.43); 1 |
| **total** | **2/10 (hi 0.51); 0** | **0/10 (hi 0.28); 1** |

## Questions per run (required / other)

| case | default | fast+expert |
|---|---|---|
| one | 0.0 / 0.0 | 0.0 / 1.0 |
| two | 1.0 / 0.0 | 1.0 / 0.2 |
| **total** | **0.5 / 0.0** | **0.5 / 0.6** |

## Tokens per run (median, p90; cached share of input)

| case | default | fast+expert |
|---|---|---|
| one | 204.0k, 204.0k; 75% | 92.0k, 92.0k; 67% |
| two | 306.0k, 306.0k; 33% | 102.5k, 123.0k; 46% |
| **total** | **255.0k, 306.0k; 50%** | **97.2k, 102.5k; 56%** |

## Minutes per run (median); invalid rows

| case | default | fast+expert |
|---|---|---|
| one | 6.0; 0 | 2.0; 0 |
| two | 5.0; 0 | 4.0; 1 |
| **total** | **5.0; 0** | **3.0; 1** |

