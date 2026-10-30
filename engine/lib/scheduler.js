"use strict";

/**
 * The scheduler: for one atomized subtask, decides which model handles it
 * (lib/models.js's deterministic routing - no model call decides this) and
 * which skill executes it (skills/index.js's registry), runs it, and
 * returns the result. This is the whole "decide model, run, return result"
 * loop the core program calls once per subtask.
 */

const { chooseModel } = require("./models");
const { pickSkill } = require("../skills");

async function runSubtask(subtask, { context = {}, originalPrompt = null, priorResults = [], onLog } = {}) {
  const model = chooseModel(subtask);
  const skill = pickSkill(subtask);
  onLog?.(`scheduler: "${String(subtask.step).slice(0, 70)}" [${subtask.type}] -> skill=${skill.name} model=${model}`);

  const start = Date.now();
  try {
    const { output, verified, meta } = await skill.run(subtask, { model, context, originalPrompt, priorResults, onLog });
    return { subtask, model, skill: skill.name, status: "done", output, verified, meta: meta || null, durationMs: Date.now() - start };
  } catch (err) {
    onLog?.(`scheduler: subtask failed -> ${err.message}`);
    return { subtask, model, skill: skill.name, status: "error", output: null, verified: null, error: err.message, durationMs: Date.now() - start };
  }
}

module.exports = { runSubtask };
