#!/usr/bin/env node
"use strict";

/**
 * The core program: a chatbot-style HTTP server. Takes a prompt, runs the
 * atomizer to decompose it, submits each subtask to the scheduler, streams
 * progress live, and composes a final answer. Ships its own UI (public/).
 *
 * No OpenClaw anywhere in this file or anything it calls - see lib/ollama.js.
 */

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");

require("./lib/env").loadEnvFile(); // must run before anything reads GH_TOKEN

const ollama = require("./lib/ollama");
const atomizer = require("./lib/atomizer");
const scheduler = require("./lib/scheduler");
const { MODELS } = require("./lib/models");

const PORT = Number(process.env.ENGINE_PORT || 4478);

/** jobs: id -> { id, prompt, status, createdAt, events: [], finalAnswer, plan, results, subscribers: Set<res> } */
const jobs = new Map();

function newJob(prompt) {
  const id = crypto.randomUUID().slice(0, 8);
  const job = { id, prompt, status: "running", createdAt: new Date().toISOString(), events: [], finalAnswer: null, plan: null, results: [], subscribers: new Set() };
  jobs.set(id, job);
  return job;
}

function emit(job, event) {
  job.events.push(event);
  const payload = `data: ${JSON.stringify(event)}\n\n`;
  for (const res of job.subscribers) res.write(payload);
}

function finish(job, status) {
  job.status = status;
  emit(job, { type: "status", status });
  for (const res of job.subscribers) res.end();
  job.subscribers.clear();
}

/** Merges completed subtask outputs into one direct answer to the
 * original prompt - the user shouldn't have to read "subtask 1 says X,
 * subtask 2 says Y" and synthesize it themselves when there was more than
 * one step. */
async function composeFinalAnswer(job) {
  const parts = job.results.map((r, i) => `Subtask ${i + 1} (${r.subtask.step}):\n${r.output ?? `[failed: ${r.error}]`}`).join("\n\n");
  const messages = [
    {
      role: "system",
      content:
        "Combine the following completed subtask results into one single, direct, well-written final answer to the user's original request. Do not mention subtasks, steps, or the process - answer as if you handled the whole request yourself in one pass.",
    },
    { role: "user", content: `Original request: ${job.prompt}\n\n${parts}` },
  ];
  const result = await ollama.chat(MODELS.judgment, messages, { timeoutMs: 180000 });
  return result.text;
}

async function runJob(job) {
  const onLog = (line) => emit(job, { type: "log", line });
  try {
    onLog("Atomizing prompt...");
    const rawSteps = await atomizer.atomize(job.prompt, { onLog });
    job.plan = rawSteps.map((step) => ({ step, type: atomizer.classifyStep(step) }));
    emit(job, { type: "plan", plan: job.plan });
    onLog(`Plan ready: ${job.plan.length} step(s).`);

    for (const subtask of job.plan) {
      // Each subtask sees the original request and every prior subtask's
      // real result, not just its own one-line text - see the comment in
      // skills/general.js for the live failure this fixes.
      const result = await scheduler.runSubtask(subtask, { onLog, originalPrompt: job.prompt, priorResults: job.results });
      job.results.push(result);
      emit(job, { type: "result", result });
    }

    if (job.plan.length === 1) {
      job.finalAnswer = job.results[0]?.output ?? `[failed: ${job.results[0]?.error}]`;
    } else {
      onLog(`Composing final answer from ${job.results.length} subtask result(s)...`);
      job.finalAnswer = await composeFinalAnswer(job);
    }
    emit(job, { type: "final", finalAnswer: job.finalAnswer });
    finish(job, "done");
  } catch (err) {
    onLog(`Job failed: ${err.message}`);
    finish(job, "error");
  }
}

function sendJSON(res, status, body) {
  const data = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(data) });
  res.end(data);
}

function readBody(req, cb) {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    try {
      cb(null, body ? JSON.parse(body) : {});
    } catch (e) {
      cb(e);
    }
  });
}

function jobSummary(j) {
  return { id: j.id, prompt: j.prompt, status: j.status, createdAt: j.createdAt, finalAnswer: j.finalAnswer };
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://localhost:${PORT}`);

  if (req.method === "GET" && url.pathname === "/") {
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    return res.end(fs.readFileSync(path.join(__dirname, "public", "index.html")));
  }

  if (req.method === "GET" && url.pathname === "/api/health") {
    return ollama
      .isReachable()
      .then(async (up) => {
        const models = up ? await ollama.listModels().catch(() => []) : [];
        sendJSON(res, 200, { ollama: up, ollamaHost: ollama.OLLAMA_HOST, models });
      });
  }

  if (req.method === "GET" && url.pathname === "/api/jobs") {
    return sendJSON(res, 200, [...jobs.values()].map(jobSummary).reverse());
  }

  if (req.method === "GET" && url.pathname.startsWith("/api/jobs/") && url.pathname.endsWith("/stream")) {
    const id = url.pathname.split("/")[3];
    const job = jobs.get(id);
    if (!job) return sendJSON(res, 404, { error: "no such job" });
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
    res.write(`data: ${JSON.stringify({ type: "backlog", events: job.events, status: job.status })}\n\n`);
    if (job.status !== "running") return res.end();
    job.subscribers.add(res);
    req.on("close", () => job.subscribers.delete(res));
    return;
  }

  if (req.method === "GET" && url.pathname.startsWith("/api/jobs/")) {
    const id = url.pathname.split("/")[3];
    const job = jobs.get(id);
    if (!job) return sendJSON(res, 404, { error: "no such job" });
    return sendJSON(res, 200, { ...jobSummary(job), plan: job.plan, results: job.results, events: job.events });
  }

  if (req.method === "POST" && url.pathname === "/api/chat") {
    return readBody(req, (err, body) => {
      if (err || !body.prompt || !String(body.prompt).trim()) return sendJSON(res, 400, { error: "prompt is required" });
      const job = newJob(String(body.prompt).trim());
      runJob(job); // fire and forget - progress is via SSE
      sendJSON(res, 200, { id: job.id });
    });
  }

  res.writeHead(404);
  res.end("not found");
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`[engine] listening on http://0.0.0.0:${PORT}`);
  console.log(`[engine] ollama host: ${ollama.OLLAMA_HOST}`);
});
