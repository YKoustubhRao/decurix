"use strict";

/**
 * The fallback skill: answer the subtask directly with whatever model the
 * scheduler chose, no verification. Every subtask that no more specific
 * skill claims ends up here - this is intentionally the simplest possible
 * skill so the extension pattern (see skills/index.js) is obvious for the
 * next one added.
 */

const { chat } = require("../lib/ollama");

module.exports = {
  name: "general",
  description: "Answers the subtask directly with the routed model. No verification.",
  matches() {
    return true; // fallback - must stay registered last in skills/index.js
  },
  async run(subtask, { model, context = {}, originalPrompt = null, priorResults = [], onLog } = {}) {
    // A subtask sent with nothing but its own one-line text has no way to
    // know what the original request actually was, or what earlier steps
    // already established - caught live on the first real test: steps
    // like "solve the recurrence relation" got answered with "please
    // provide the recurrence relation" because nothing upstream had ever
    // told this call it was about binary search, or that an earlier step
    // had already derived T(n) = T(n/2) + O(1). Both are threaded through
    // every call now, not just file content.
    const originalBlock = originalPrompt ? `Original request: ${originalPrompt}\n\n` : "";
    const priorBlock = priorResults.length
      ? "\n\nProgress so far on this same request:\n" +
        priorResults.map((r, i) => `${i + 1}. ${r.subtask.step}\n   -> ${r.output ?? `[failed: ${r.error}]`}`).join("\n")
      : "";
    const contextBlock = Object.keys(context).length
      ? "\n\nRelevant context:\n" + Object.entries(context).map(([k, v]) => `--- ${k} ---\n${v}`).join("\n\n")
      : "";
    const messages = [
      {
        role: "system",
        content:
          "You are working through one step of a multi-step plan for the user's original request. Use the original request and the progress so far as context, but answer only THIS step, directly and completely, in plain text. Do not ask clarifying questions - state any necessary assumption briefly instead.",
      },
      { role: "user", content: `${originalBlock}This step: ${subtask.step}${priorBlock}${contextBlock}` },
    ];
    onLog?.(`skill:general -> calling ${model}...`);
    const result = await chat(model, messages, { timeoutMs: 180000 });
    onLog?.(`skill:general -> ${result.model} responded (tokens in/out ${result.promptTokens}/${result.responseTokens}, ${result.totalDurationMs}ms)`);
    return {
      output: result.text,
      verified: null,
      meta: { promptTokens: result.promptTokens, responseTokens: result.responseTokens, durationMs: result.totalDurationMs },
    };
  },
};
