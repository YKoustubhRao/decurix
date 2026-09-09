"use strict";

/**
 * Generates a real, multi-file code project: plans the component files
 * from the request, generates each one, writes them to disk, compiles the
 * whole thing for real (g++), writes a demo/smoke-test program, generates
 * README.md + CODEINDEX.md grounded in the actual generated code, then
 * commits and pushes a branch to GitHub. This is the skill that was
 * missing when a graph-algorithms request went through skills/general.js
 * and came back with a fabricated "I ran the tests" answer with no file,
 * no compile, and no binary anywhere - see the chat history this was
 * built in response to.
 *
 * Verification discipline carried over from the legacy pipeline's
 * solve-cp-problem.js: a model's own claim of correctness is never
 * accepted. The only things this skill trusts are a real `g++` exit code
 * and real command output - `verified` on the returned result reflects
 * that, not what the model said about itself.
 */

const fs = require("node:fs");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const { chat } = require("../lib/ollama");
const { isCodeProjectTask } = require("../lib/atomizer");
const github = require("../lib/github");

const WORKSPACE_DIR = process.env.WORKSPACE_DIR || path.join(__dirname, "..", "workspace");
const DEFAULT_REPO_NAME = "agent-code-projects";
const MAX_COMPILE_ATTEMPTS = 3;

function log(onLog, msg) {
  onLog?.(`skill:code-project -> ${msg}`);
}

function extractFencedCode(text) {
  const fence = text.match(/```[\w-]*\n([\s\S]*?)```/);
  return fence ? fence[1].trim() + "\n" : null;
}

function extractBranchName(text) {
  const m = text.match(/branch\s+(?:called|named)\s+["']?([A-Za-z0-9_\-./]+)["']?/i);
  return m ? m[1] : "agent-generated-" + Date.now();
}

function extractRepoName(text) {
  const m = text.match(/repo(?:sitory)?\s+(?:called|named)\s+["']?([A-Za-z0-9_\-]+)["']?/i);
  return m ? m[1] : DEFAULT_REPO_NAME;
}

/** One deterministic-ish structuring call: turn the user's own request
 * into a concrete file list. Content-fed with only the user's own text,
 * so there's nothing here for the model to hallucinate about beyond
 * re-organizing what was already explicitly asked for. */
async function planComponents(model, originalPrompt, onLog) {
  const prompt = `A user asked for this coding project:
"""
${originalPrompt}
"""

List each distinct implementation file this project needs as a JSON array ONLY - no prose before or after it. Each item: {"name": short lowercase identifier with no spaces (e.g. "avl_tree"), "description": one sentence of exactly what it must implement}. One item per distinct data structure/algorithm/library explicitly requested. Do not include a main/demo/test file - list only real implementation components.`;
  log(onLog, `planning component files via ${model}...`);
  const result = await chat(model, [{ role: "user", content: prompt }], { timeoutMs: 120000 });
  const match = result.text.match(/\[[\s\S]*\]/);
  if (!match) throw new Error(`could not find a JSON component list in the planning response: ${result.text.slice(0, 200)}`);
  const components = JSON.parse(match[0]);
  if (!Array.isArray(components) || components.length === 0) throw new Error("component plan was empty");
  return components.map((c) => ({ ...c, filename: `${c.name}.hpp` }));
}

async function generateComponentFile(model, component, originalPrompt, onLog) {
  const prompt = `Implement "${component.name}" - ${component.description}

This is one file of a larger C++ project the user requested:
"""
${originalPrompt}
"""

Requirements:
- Modern C++17, using the STL wherever it genuinely fits (containers, algorithms) rather than reinventing standard functionality.
- A single self-contained header file with "#pragma once" - header-only (inline/template implementations), no separate .cpp needed.
- Return ONLY the complete file content in a single fenced code block. No explanation before or after.`;

  for (let attempt = 1; attempt <= 2; attempt++) {
    log(onLog, `[${component.name}] generating via ${model} (attempt ${attempt}/2)...`);
    const result = await chat(model, [{ role: "user", content: prompt }], { timeoutMs: 180000 });
    const code = extractFencedCode(result.text);
    if (code) return code;
    log(onLog, `[${component.name}] response had no fenced code block, retrying...`);
  }
  throw new Error(`model never returned a fenced code block for component "${component.name}"`);
}

async function generateDemoMain(model, components, fileContents, originalPrompt, onLog) {
  const includeList = components.map((c) => `#include "${c.filename}"`).join("\n");
  const contentBlock = components.map((c) => `--- ${c.filename} ---\n${fileContents[c.name]}`).join("\n\n");
  const prompt = `Write a single main.cpp that #includes each of these real header files (they already exist exactly as shown below) and demonstrates each one with a small, sensible smoke test (construct it, run one or two basic operations, print a result). Use the EXACT class/function names and signatures shown in the real content below - do not invent different ones.

${includeList}

Real header contents (ground truth - match these exactly):
${contentBlock}

Original request for context: ${originalPrompt}

Return ONLY the complete main.cpp content in a single fenced code block.`;
  log(onLog, `generating demo main.cpp via ${model}...`);
  const result = await chat(model, [{ role: "user", content: prompt }], { timeoutMs: 180000 });
  const code = extractFencedCode(result.text);
  if (!code) throw new Error("model returned no fenced code block for main.cpp");
  return code;
}

function compile(srcDir, onLog) {
  try {
    execFileSync("g++", ["-std=c++17", "-O2", "-Wall", "main.cpp", "-o", "demo"], { cwd: srcDir, encoding: "utf8" });
    return { ok: true, error: null };
  } catch (e) {
    log(onLog, `compile failed: ${String(e.stderr || e.message).split("\n").slice(0, 5).join(" | ")}`);
    return { ok: false, error: String(e.stderr || e.message) };
  }
}

function runDemo(srcDir, onLog) {
  try {
    const out = execFileSync("./demo", [], { cwd: srcDir, encoding: "utf8", timeout: 10000 });
    return { ok: true, output: out.slice(0, 2000) };
  } catch (e) {
    log(onLog, `demo binary run failed/crashed: ${e.message}`);
    return { ok: false, output: String(e.stdout || e.message).slice(0, 2000) };
  }
}

/** Multi-file repair: feeds the real compiler error plus every current
 * file's real content back, asks for corrected versions of just the
 * broken ones, and only overwrites files it actually got a fenced block
 * for - a file the model doesn't resend stays exactly as it was, since a
 * multi-file repair replying about only 2 of 6 files is a normal, correct
 * response, not a truncation to panic over. */
async function repairFiles(model, files, compileError, originalPrompt, onLog) {
  const filesBlock = Object.entries(files)
    .map(([name, content]) => `=== FILE: ${name} ===\n\`\`\`cpp\n${content}\`\`\`\n=== END FILE ===`)
    .join("\n\n");
  const prompt = `This C++ project failed to compile. Real compiler output:
\`\`\`
${compileError.slice(0, 4000)}
\`\`\`

Original request for context: ${originalPrompt}

Current files:
${filesBlock}

Return ONLY the corrected version of each file that needed a fix, using EXACTLY this format per file (repeat for every file you're correcting - omit files that don't need changes):
=== FILE: <exact filename> ===
\`\`\`cpp
<complete corrected file content>
\`\`\`
=== END FILE ===`;
  log(onLog, `requesting a repair from ${model} for the real compile error...`);
  const result = await chat(model, [{ role: "user", content: prompt }], { timeoutMs: 240000 });
  const re = /=== FILE: (.+?) ===\s*```[\w-]*\n([\s\S]*?)```\s*=== END FILE ===/g;
  const fixed = {};
  let m;
  while ((m = re.exec(result.text))) {
    fixed[m[1].trim()] = m[2].trim() + "\n";
  }
  return fixed;
}

async function generateDocs(model, originalPrompt, allFileNames, fileContents, compileResult, onLog) {
  const filesBlock = allFileNames.map((name) => `--- ${name} ---\n${fileContents[name]}`).join("\n\n");
  const prompt = `Write documentation for this project based ONLY on the real code below - do not invent anything not actually present.

Original request: ${originalPrompt}

Real files:
${filesBlock}

Compile status: ${compileResult.ok ? "compiled successfully with g++ -std=c++17" : "DID NOT COMPILE - the documentation must not claim it builds or runs"}

Return your answer as exactly two sections in this format:

=== README.md ===
(full file content - project overview, what's implemented, how to build with g++, how to run the demo)
=== END ===

=== CODEINDEX.md ===
(full file content - one section per file, listing its real classes/functions with a one-line description of each, based on the actual code above)
=== END ===`;
  log(onLog, `generating README.md + CODEINDEX.md via ${model}...`);
  const result = await chat(model, [{ role: "user", content: prompt }], { timeoutMs: 180000 });
  const readmeM = result.text.match(/=== README\.md ===\s*([\s\S]*?)\s*=== END ===/);
  const indexM = result.text.match(/=== CODEINDEX\.md ===\s*([\s\S]*?)\s*=== END ===/);
  return {
    readme: readmeM ? readmeM[1].trim() + "\n" : `# Project\n\nGenerated from: ${originalPrompt}\n`,
    codeIndex: indexM ? indexM[1].trim() + "\n" : `# Code index\n\nFiles: ${allFileNames.join(", ")}\n`,
  };
}

module.exports = {
  name: "code-project",
  description: "Plans, generates, compiles, documents, and pushes a real multi-file code project to GitHub.",
  matches(subtask) {
    return isCodeProjectTask(subtask.step || subtask.description || "");
  },
  async run(subtask, { model, onLog }) {
    const originalPrompt = subtask.step;
    const repoName = extractRepoName(originalPrompt);
    const branchName = extractBranchName(originalPrompt);
    const repoDir = path.join(WORKSPACE_DIR, repoName);
    const srcDir = path.join(repoDir, "src");
    fs.mkdirSync(srcDir, { recursive: true });

    // 1. Plan + generate every component file, content-fed and grounded
    // only in the user's own request text.
    const components = await planComponents(model, originalPrompt, onLog);
    const fileContents = {};
    for (const c of components) {
      fileContents[c.name] = await generateComponentFile(model, c, originalPrompt, onLog);
      fs.writeFileSync(path.join(srcDir, c.filename), fileContents[c.name]);
      log(onLog, `wrote ${c.filename} (${fileContents[c.name].length} bytes)`);
    }

    // 2. Demo/smoke-test program, grounded in the REAL generated headers
    // (not the model's memory of what it wrote) so it calls real signatures.
    const mainCode = await generateDemoMain(model, components, fileContents, originalPrompt, onLog);
    fs.writeFileSync(path.join(srcDir, "main.cpp"), mainCode);

    // 3. The real verification gate - never trust the model's own claim.
    const allFiles = () => {
      const map = { "main.cpp": fs.readFileSync(path.join(srcDir, "main.cpp"), "utf8") };
      for (const c of components) map[c.filename] = fs.readFileSync(path.join(srcDir, c.filename), "utf8");
      return map;
    };
    let compileResult = compile(srcDir, onLog);
    for (let attempt = 1; attempt < MAX_COMPILE_ATTEMPTS && !compileResult.ok; attempt++) {
      const fixed = await repairFiles(model, allFiles(), compileResult.error, originalPrompt, onLog);
      for (const [name, content] of Object.entries(fixed)) {
        fs.writeFileSync(path.join(srcDir, name), content);
        log(onLog, `repair attempt ${attempt}: rewrote ${name}`);
      }
      if (Object.keys(fixed).length === 0) {
        log(onLog, `repair attempt ${attempt}: model returned no fixed files - stopping repair loop.`);
        break;
      }
      compileResult = compile(srcDir, onLog);
    }
    log(onLog, `final compile status: ${compileResult.ok ? "SUCCESS" : "FAILED"}`);

    const demoResult = compileResult.ok ? runDemo(srcDir, onLog) : { ok: false, output: "(not run - compile failed)" };

    // 4. Docs grounded in the real, final code - not what was planned,
    // what actually ended up on disk (may differ after repairs).
    const finalFiles = allFiles();
    const allFileNames = Object.keys(finalFiles);
    const docs = await generateDocs(model, originalPrompt, allFileNames, finalFiles, compileResult, onLog);
    fs.writeFileSync(path.join(repoDir, "README.md"), docs.readme);
    fs.writeFileSync(path.join(repoDir, "CODEINDEX.md"), docs.codeIndex);

    // 5. Git + GitHub - deterministic, no model call.
    github.ensureGitRepo(repoDir);
    github.startCleanBranch(repoDir, branchName);
    const committed = github.commitAll(repoDir, `Add ${components.map((c) => c.name).join(", ")} (agent-generated)`);

    let pushResult = { attempted: false };
    let repoInfo = null;
    try {
      const user = await github.getAuthenticatedUser();
      const ensured = await github.ensureRepo(repoName, { description: `Agent-generated: ${originalPrompt.slice(0, 100)}` });
      repoInfo = ensured.repo;
      log(onLog, `GitHub repo ${ensured.created ? "created" : "already existed"}: ${repoInfo.html_url}`);
      github.pushBranch(repoDir, user.login, repoName, branchName);
      pushResult = { attempted: true, ok: true, url: `${repoInfo.html_url}/tree/${branchName}` };
      log(onLog, `pushed branch "${branchName}" to ${pushResult.url}`);
    } catch (err) {
      pushResult = { attempted: true, ok: false, error: err.message };
      log(onLog, `push failed: ${err.message}`);
    }

    const summaryLines = [
      `Files written to ${repoDir}:`,
      ...allFileNames.map((n) => `  - src/${n}`),
      `  - README.md`,
      `  - CODEINDEX.md`,
      ``,
      `Compile (g++ -std=c++17): ${compileResult.ok ? "SUCCEEDED" : "FAILED"}`,
      ...(compileResult.ok ? [] : [compileResult.error.slice(0, 500)]),
      `Demo run: ${demoResult.ok ? "ran successfully" : "did not run cleanly"}`,
      demoResult.output ? demoResult.output.slice(0, 500) : "",
      ``,
      committed ? `Committed to branch "${branchName}".` : `Nothing new to commit on branch "${branchName}".`,
      pushResult.ok ? `Pushed to ${pushResult.url}` : `Push ${pushResult.attempted ? "failed" : "not attempted"}: ${pushResult.error || ""}`,
    ];

    return {
      output: summaryLines.join("\n"),
      verified: compileResult.ok && demoResult.ok && !!pushResult.ok,
      meta: { repoDir, repoName, branchName, files: allFileNames, compileOk: compileResult.ok, demoOk: demoResult.ok, push: pushResult },
    };
  },
};
