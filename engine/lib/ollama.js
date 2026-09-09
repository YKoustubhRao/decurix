"use strict";

/**
 * Direct HTTP client for Ollama. This is the ONLY thing in this engine that
 * talks to a model provider - no OpenClaw, no agent session, no ambient
 * tool access. A plain chat completion has nothing for the model to
 * misuse, which is deliberate: the previous OpenClaw-based pipeline lost an
 * entire run to a model invoking real tools during what should have been a
 * text-only call (see agents/soul-guard/model-selection.md, Round 3 finding
 * #3 and the Round 4 problem-002 postmortem). Removing tool access at the
 * transport level makes that whole failure class structurally impossible
 * here, instead of something every prompt has to defend against.
 */

const OLLAMA_HOST = process.env.OLLAMA_HOST || "http://127.0.0.1:11434";

/**
 * One non-streaming chat call. Returns the response text plus whatever
 * usage/timing Ollama reports - callers (the scheduler) log this so which
 * model actually answered, and how expensive it was, is always visible,
 * not just which model was requested.
 */
async function chat(model, messages, { timeoutMs = 180000, temperature } = {}) {
  const body = { model, messages, stream: false };
  if (temperature !== undefined) body.options = { temperature };

  let res;
  try {
    res = await fetch(`${OLLAMA_HOST}/api/chat`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs),
    });
  } catch (err) {
    if (err.name === "TimeoutError" || err.name === "AbortError") {
      throw new Error(`ollama call to ${model} timed out after ${timeoutMs}ms`);
    }
    throw new Error(`ollama call to ${model} failed: ${err.message} (is Ollama reachable at ${OLLAMA_HOST}?)`);
  }

  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`ollama ${model} returned HTTP ${res.status}: ${text.slice(0, 500)}`);
  }

  const data = await res.json();
  if (data.error) throw new Error(`ollama ${model} error: ${data.error}`);

  return {
    text: data.message?.content ?? "",
    model: data.model ?? model,
    promptTokens: data.prompt_eval_count ?? null,
    responseTokens: data.eval_count ?? null,
    // Ollama reports durations in nanoseconds.
    totalDurationMs: typeof data.total_duration === "number" ? Math.round(data.total_duration / 1e6) : null,
  };
}

/** Real state, not a config guess - what Ollama actually has pulled right now. */
async function listModels() {
  const res = await fetch(`${OLLAMA_HOST}/api/tags`, { signal: AbortSignal.timeout(5000) });
  if (!res.ok) throw new Error(`ollama /api/tags returned HTTP ${res.status}`);
  const data = await res.json();
  return (data.models || []).map((m) => ({ name: m.name, sizeBytes: m.size, modifiedAt: m.modified_at }));
}

async function isReachable() {
  try {
    const res = await fetch(`${OLLAMA_HOST}/api/version`, { signal: AbortSignal.timeout(3000) });
    return res.ok;
  } catch {
    return false;
  }
}

module.exports = { chat, listModels, isReachable, OLLAMA_HOST };
