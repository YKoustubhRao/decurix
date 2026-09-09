"use strict";

/**
 * Model routing policy. Deterministic lookup, not a model call - the old
 * pipeline's core rule (agents/soul-guard/model-selection.md: "Atomicity
 * check: deterministic heuristics, NOT another model call - we already
 * know models are unreliable judges of their own task granularity")
 * applies just as much to model *choice* as it does to atomicity: asking a
 * model which model should handle a task is the same category of mistake.
 *
 * Picks are empirically grounded in agents/soul-guard/model-selection.md's
 * test history, not vendor benchmarks. Update this table (and cite the new
 * test) whenever evidence changes the picture - it's a living policy, not
 * a one-time snapshot.
 */

const MODELS = {
  // Devstral: best on record for planning and for domain-aware content
  // generation (real technical knowledge, not just syntax). Used for
  // decomposition and as the default for anything requiring real judgment.
  planner: "devstral-small-2:24b",
  judgment: "devstral-small-2:24b",
  // qwen3: best adherence to a requested output format for structured
  // summarization/formatting - overkill to spend the bigger model on this.
  summarizer: "qwen3:14b",
  // qwen2.5-coder: coding-tuned, but NOT YET EMPIRICALLY TESTED anywhere in
  // this project's history (model-selection.md's "Local models tested"
  // table only covers qwen3, devstral, gemma4) - it sits in openclaw.json
  // from onboarding, untested. Kept here as a named, available option for
  // a future dedicated code-generation skill to opt into deliberately, but
  // deliberately NOT wired into the default keyword routing below until it
  // has a real result behind it, the same standard every other model here
  // had to clear first.
  coder: "qwen2.5-coder:14b",
};

// Crashed the underlying llama-server process under this workload on this
// GPU (CUDA stack-overrun) - never route to it until that's independently
// re-verified fixed (driver update / smaller quant / more VRAM).
const AVOID = new Set(["gemma4:26b"]);

/**
 * Chooses a model for one subtask. `subtask` is `{ step, type }` from the
 * atomizer (type is "mechanical" or "judgment") plus whatever the caller
 * adds. Deliberately conservative: a first version of this routed on any
 * mention of "function" or "code" straight to qwen2.5-coder, which sent a
 * plain-English question about binary search's recurrence relation to a
 * model with zero track record here, off-topic, purely because the word
 * "function" appeared in the step text ("Express the number of comparisons
 * as a function of the input size") - caught live on the first real test.
 * Only route away from the validated default (devstral) for a narrow,
 * unambiguous case; when in doubt, keep the model with an actual track
 * record for open-ended judgment.
 */
function chooseModel(subtask) {
  const text = String(subtask.step || subtask.description || "").toLowerCase();

  if (subtask.type === "planning") return MODELS.planner;
  if (/\b(summarize|summarise|list all|format as|extract the|outline)\b/.test(text)) return MODELS.summarizer;
  return MODELS.judgment;
}

module.exports = { MODELS, AVOID, chooseModel };
