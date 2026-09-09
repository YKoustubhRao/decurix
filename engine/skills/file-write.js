"use strict";

/**
 * Writes generated content to a real file - organized into the right
 * folder by content type, not dumped wherever the user happened to name
 * (which is often wrong anyway: "save it to /usr/poem.txt" isn't
 * writable by a non-root container user, and /usr is for installed
 * system binaries, not generated content regardless of permissions).
 *
 * This is the skill that was missing when "write a poem and store it in
 * /usr/poem.txt" went through skills/general.js: that skill has no file
 * access at all, so the model just answered in text and - worse -
 * confidently claimed "This has been saved to /usr/poem.txt" when
 * nothing was ever written. Verification discipline carried over from
 * skills/code-project.js: `verified` reflects a real fs.existsSync check
 * on the path this code itself wrote, never the model's own claim.
 */

const fs = require("node:fs");
const path = require("node:path");
const { chat } = require("../lib/ollama");
const { isCodeProjectTask, isFileWriteTask } = require("../lib/atomizer");

const WORKSPACE_DIR = process.env.WORKSPACE_DIR || path.join(__dirname, "..", "workspace");

// A fixed, small set of top-level categories - keeps organization
// predictable instead of letting a classification call invent an
// unbounded pile of ad hoc folder names.
const CATEGORIES = new Set(["code", "documents", "data", "misc"]);

function log(onLog, msg) {
  onLog?.(`skill:file-write -> ${msg}`);
}

/** A model-suggested relative path is never trusted directly - path
 * traversal (`../../etc/passwd`) or an absolute/drive-rooted path is
 * stripped back to something that can only land inside WORKSPACE_DIR.
 * This is the actual safety boundary; the category/filename the model
 * picks is just a suggestion within it. */
function sanitizeRelPath(relPath) {
  const cleaned = String(relPath)
    .replace(/^[A-Za-z]:[\\/]+/, "") // strip a Windows drive root if the model hallucinates one
    .replace(/^[\\/]+/, "") // strip a leading absolute-path slash
    .split(/[\\/]+/)
    .filter((seg) => seg && seg !== "." && seg !== "..")
    .join("/");
  if (!cleaned) throw new Error("model returned an empty/unsafe file path");
  const root = path.resolve(WORKSPACE_DIR);
  const resolved = path.resolve(root, cleaned);
  if (resolved !== root && !resolved.startsWith(root + path.sep)) {
    throw new Error(`unsafe path "${relPath}" resolved outside the workspace`);
  }
  return resolved;
}

async function planFile(model, task, { originalPrompt, priorResults = [] } = {}, onLog) {
  // If the atomizer fragmented "write X and save it" into separate steps
  // (it doesn't always catch every phrasing - see isFileWriteTask), this
  // step alone is just "save the poem to ocean.txt" with no idea what the
  // poem actually was. Without the prior step's real output, this skill
  // would independently invent its OWN version of the content - caught
  // live: a haiku step produced one version, this skill silently produced
  // a DIFFERENT one, and the file on disk didn't match what the user was
  // told. Reusing real prior output when present is the fix.
  const priorBlock = priorResults.length
    ? "\n\nContent already produced by an earlier step in this same request - REUSE this exact content if it's what should be saved, do not invent a different version:\n" +
      priorResults.map((r, i) => `${i + 1}. ${r.subtask.step}\n${r.output ?? ""}`).join("\n\n")
    : "";
  const originalBlock = originalPrompt ? `Original request: ${originalPrompt}\n\n` : "";

  const prompt = `${originalBlock}This step: ${task}${priorBlock}

Produce the file this request needs. Return a JSON object ONLY, nothing else, no fenced code block, no prose - just the raw JSON:
{"category": "code" | "documents" | "data" | "misc", "relativePath": "<category>/<short-descriptive-filename.ext>", "content": "<the complete, real file content requested>"}

Rules:
- category must be exactly one of: code, documents, data, misc. Source code/scripts -> "code". Letters/poems/notes/essays/reports/plain writing -> "documents". Structured data (csv/json/etc) -> "data". Anything else -> "misc".
- relativePath must start with the category name, then a real filename with a correct extension for the content (.py/.cpp/.js/.go/.java for code by language, .txt or .md for documents, .json/.csv for data).
- content must be the actual requested content, complete and ready to save exactly as-is - not a description of it. If the content already appears above (produced by an earlier step), reuse it exactly rather than rewriting it.`;

  log(onLog, `planning file placement + content via ${model}...`);
  const result = await chat(model, [{ role: "user", content: prompt }], { timeoutMs: 180000 });
  const match = result.text.match(/\{[\s\S]*\}/);
  if (!match) throw new Error(`model did not return a JSON file plan: ${result.text.slice(0, 200)}`);

  let plan;
  try {
    plan = JSON.parse(match[0]);
  } catch (e) {
    throw new Error(`model's file plan was not valid JSON: ${e.message}`);
  }
  if (!CATEGORIES.has(plan.category)) plan.category = "misc";
  if (!plan.relativePath || typeof plan.content !== "string") {
    throw new Error("model's file plan was missing relativePath or content");
  }
  return plan;
}

module.exports = {
  name: "file-write",
  description: "Classifies content type, organizes it into the right workspace folder, and actually writes the file - verified against disk, not the model's claim.",
  matches(subtask) {
    const text = subtask.step || subtask.description || "";
    return !isCodeProjectTask(text) && isFileWriteTask(text);
  },
  async run(subtask, { model, originalPrompt = null, priorResults = [], onLog } = {}) {
    const task = subtask.step;
    const plan = await planFile(model, task, { originalPrompt, priorResults }, onLog);
    const targetPath = sanitizeRelPath(plan.relativePath);

    fs.mkdirSync(path.dirname(targetPath), { recursive: true });
    const content = plan.content.endsWith("\n") ? plan.content : plan.content + "\n";
    fs.writeFileSync(targetPath, content);

    const actuallyExists = fs.existsSync(targetPath);
    log(onLog, `wrote ${targetPath} (category=${plan.category}, ${content.length} bytes) - verified on disk: ${actuallyExists}`);

    return {
      output: `Saved to ${targetPath}\n\n${plan.content}`,
      verified: actuallyExists,
      meta: { path: targetPath, category: plan.category },
    };
  },
};
