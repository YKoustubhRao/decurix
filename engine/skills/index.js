"use strict";

/**
 * Skill registry - the extension point for "keep adding skills". A skill
 * is `{ name, description, matches(subtask), run(subtask, ctx) }`.
 * `matches()` decides whether a skill claims a subtask; the first match in
 * this array wins, so register more specific skills ABOVE `general`, which
 * matches everything and must stay last.
 *
 * Adding a skill later that needs real verification (compile+run, not just
 * "the model said so") should follow the pattern already proven in the
 * legacy OpenClaw-based pipeline's solve-cp-problem.js: never accept a
 * model's own claim of correctness - run the real check yourself and
 * return `verified: true/false` accordingly, feeding a failed check's
 * actual detail back on retry rather than a vague "try again".
 */

const codeProject = require("./code-project");
const general = require("./general");

const SKILLS = [codeProject, general];

function pickSkill(subtask) {
  return SKILLS.find((s) => s.matches(subtask)) || general;
}

module.exports = { SKILLS, pickSkill };
