"use strict";

/**
 * Recursive task atomizer - decomposes a prompt into atomic, independently
 * schedulable subtasks. Direct-to-Ollama rewrite of the OpenClaw-based
 * atomizer (agents/soul-guard/atomizer/atomizer.js); the decomposition
 * heuristics below are carried over unchanged because they were earned the
 * hard way against real failures, not guessed - see model-selection.md for
 * the incidents behind each one. What changed is only the transport: no
 * subprocess, no agent session, no tool access - just a chat completion.
 */

const { chat } = require("./ollama");
const { MODELS } = require("./models");

const MAX_ATOMIZE_DEPTH = 3;
const MAX_PLAN_CALLS = 20;

// Shared noun lists - a round-2 eval (100 harder/adversarial tasks, see
// engine/eval/) found this same enumeration problem independently in three
// different places (isCodeProjectTask, the file-write bypass, and a
// missing "just implement this one thing" bypass) because each one had
// its own copy that had each individually gone stale. One shared list per
// category now - still an enumeration (English has no fixed vocabulary
// for this), but at least fixed once instead of three times.
const CODE_NOUNS = "code|library|libraries|class|program|algorithm|tree|struct|structure|function|cache|filter|implementation|files?|api|stack|queue|method|script|module|component|query|heap|graph|matrix";
const CONTENT_NOUNS = "poem|haiku|limerick|sonnet|verse|rap|song|story|letter|message|note|joke|quote|essay|review|caption|tagline|greeting|toast|speech|bio|summary|paragraph|sentence|description";

/** A step is atomic if it has exactly one command/action reference and no
 * sequencing language. A fenced code block is a literal, already-concrete
 * answer - not a further-decomposable instruction (recursing into it burned
 * 10+ minutes on bogus sub-steps in the original project's Issue 4). */
function isAtomic(stepText) {
  const compoundIndicators = [" and then ", "; then", " after that ", " followed by ", " once done", " next,"];
  const lower = stepText.toLowerCase();
  const hasCompoundLanguage = compoundIndicators.some((ind) => lower.includes(ind));
  const hasFencedCode = /```/.test(stepText);
  const withoutFencedBlocks = stepText.replace(/```[\s\S]*?```/g, "");
  const backtickCommands = (withoutFencedBlocks.match(/`[^`]+`/g) || []).length;
  const bulletSubitems = (withoutFencedBlocks.match(/\n\s*[-*]\s/g) || []).length;
  return !hasCompoundLanguage && (hasFencedCode || (backtickCommands <= 1 && bulletSubitems === 0));
}

/** "Move/extract/split existing code"-shaped tasks read as endlessly
 * divisible to a planner (it will fragment down to single field
 * assignments) rather than as one coherent unit - skip recursive planning
 * entirely for this task shape and hand it over as one atomic step. */
function isStructuralTask(taskDescription) {
  const patterns = [
    /\bcreate\s+(a\s+|an\s+)?new\s+function\b/i,
    /\badd\s+(a\s+)?(new\s+)?(helper\s+)?function\b/i,
    /\bsplit\b[\s\S]*\binto\b/i,
    /\bextract\b[\s\S]*\binto\b/i,
    /\brefactor\b/i,
    /\brestructure\b/i,
    /\breorganize\b/i,
  ];
  return patterns.some((p) => p.test(taskDescription));
}

/** Short, single-question/single-instruction requests ("what is X", "explain
 * Y", "write a one-paragraph summary of Z") have a real tendency to get
 * fragmented into artificial sub-steps ("identify the variable", "conclude
 * with a summary") that add several sequential model calls without
 * improving the answer - a prompt instruction telling the planner to avoid
 * this helps some but isn't reliable enough alone (caught live: it cut a
 * ~10-step plan to 5, not to the 1 step this shape actually needs). This is
 * a deterministic backstop, same pattern as isStructuralTask: skip planning
 * entirely rather than trust the model to self-regulate its own granularity
 * - the atomizer's founding rule, just applied to "don't over-decompose"
 * instead of only "don't under-decompose". */
function looksLikeSingleDirectAnswer(taskDescription) {
  const t = taskDescription.trim();
  if (t.length > 400) return false; // let longer/richer requests still go through real planning

  // Round-2 eval finding: a request this short essentially cannot contain
  // a genuine second ask - "fix this", "repo pls", "make it better" kept
  // getting fragmented into 5-7 steps by real planning with nothing to
  // actually decompose. No prompt in either eval round that's genuinely
  // multi-part is anywhere close to this short, so this is a safe,
  // unconditional shortcut, not just a default.
  if (t.split(/\s+/).filter(Boolean).length <= 4) return true;

  // Same eval: "Do the following: 1) ..., 2) ..." matched the yes/no-question
  // "do" starter below (added for "Do you know...") even though this "do"
  // is an imperative introducing a real multi-part list, not a question -
  // checked before the "and X" signals since this one doesn't use "and" at
  // all. Also found: "and give/note/compare/create/format/translate ..."
  // is a real second ask that "and also/then" alone didn't catch (multiple
  // genuinely multi-part "explain X, and Y" requests wrongly collapsed to
  // one step), and a bare "," + "then" (no "and") was never checked either.
  // NOT fixed: "and how/why" (e.g. "explain X, and how does Y work") -
  // deliberately left alone because "what is the time complexity of binary
  // search, and why?" is a validated single-step case in production, and
  // there's no cheap signal that tells the two apart. "include" was tried
  // and removed: "...and include brief comments explaining the eviction
  // logic" is a minor ancillary detail on ONE deliverable (a class), not a
  // second one, and treating it as multi-part sent a single "implement
  // this class" request back into 17-step fragmentation - the opposite of
  // what this whole function exists to prevent. No evidence any tested
  // case actually needed "include" to be caught as multi-part.
  const multiPartSignals =
    /^do(es)?\s+the\s+following\b/i.test(t) ||
    /\band\b[\s\S]*\b(also|then|give|provide|show|compare|list|note|discuss|analyze|evaluate|summarize|identify|calculate|describe|explain|write|create|format|translate|verify)\b/i.test(
      t
    ) ||
    /,\s*then\b/i.test(t) ||
    /^\s*\d+[.)]\s/.test(t) ||
    /\bfirst\b[\s\S]*\bsecond\b/i.test(t) ||
    /;/.test(t);
  if (multiPartSignals) return false;

  // Same eval: "write a haiku/limerick/sonnet/rap verse/..." only matched
  // when the noun happened to be "short"/"one-paragraph"/"brief" - every
  // other single-shot creative request (6 of 10 tested) fell through to
  // real planning and got fragmented up to 9 steps for a two-line poem.
  // "solve"/yes-no question starters ("Are you...", "Solve the equation
  // ...") had the same gap - caught the same way, on real prompts.
  const directShapes =
    /^(what|why|how|when|where|who|which|is|are|do|does|can|could|would|should|will|explain|describe|solve|summarize|summarise|define|list|give)\b/i;
  if (directShapes.test(t)) return true;

  // A first attempt at the "write/compose/create a <anything>" case above
  // matched ANY noun, not just short creative content - "create a study
  // plan for learning DS&A over 8 weeks" matched exactly like "write a
  // poem" does, and got wrongly collapsed to one step even though a
  // multi-week study plan is genuinely multi-part (caught on the very
  // next eval run after that fix went in). An explicit list of short-form
  // content types, not "any noun after a/an", avoids catching planning
  // artifacts (plan/itinerary/routine/roadmap/...) that only happen to
  // share the same "write/create a ___" grammar. Round 2 extended this to
  // plain technical asks too ("implement/design a function/class/cache/
  // ...") with no repo or file-save signal at all (those are already
  // claimed by isCodeProjectTask/isFileWriteTask before this ever runs) -
  // "Implement a thread-safe LRU cache in Java..." had NO bypass at all
  // and fragmented into 27 steps for what should be one class.
  // "one" alongside "a/an": "just write ONE function that adds two
  // numbers" - a real phrasing variant of the same "a function" shape.
  const singleContentNoun = new RegExp(
    `\\b(write|compose|create|implement|build|design)\\s+(a|an|one)\\s+(?:[\\w-]+\\s+){0,4}(${CONTENT_NOUNS}|${CODE_NOUNS})\\b`,
    "i"
  );
  return singleContentNoun.test(t);
}

/** A request to build actual code artifacts (files, a compiled binary, a
 * git repo/branch/push) is the same failure shape the legacy pipeline
 * already learned the hard way with solve-cp-problem.js: fragmenting it
 * into independent, isolated subtasks destroys the shared project context
 * every file needs (which headers exist, what a shared main.cpp should
 * call). Detected here so it can be handed to skills/code-project.js as
 * ONE atomic unit instead of being decomposed - exported so that skill's
 * own `matches()` uses the exact same signal, not a re-derived copy of it. */
function isCodeProjectTask(taskDescription) {
  const t = taskDescription.toLowerCase();
  // The noun list kept missing real data-structure names as they came up
  // in testing (a "trie", a "bloom filter", an "LRU cache" all failed to
  // match "tree|struct|..." even with an explicit repo ask right next to
  // them; round 2 found "files"/"API"/"stack"/"queue" missing too, e.g.
  // "create two Python FILES ... push both to a new repo" never matched
  // at all) - moved to the shared CODE_NOUNS list (see its own comment)
  // rather than maintaining a second copy that drifts out of sync.
  // Verb form matters too: "creat(?:e|ing)" rather than bare "create",
  // because "saving each in its own file" and "writes its own docs" use
  // participle/third-person forms that a bare `\bcreate\b` word-boundary
  // match silently never matches - caught live on both verbs in round 2.
  const wantsCode = new RegExp(`\\b(implement|writ(?:e|es|ing)|creat(?:e|es|ing)|build)\\b[\\s\\S]*\\b(${CODE_NOUNS})\\b`).test(t);
  // "readme" dropped from this list as a precaution, not something the
  // eval actually caught failing: broadening wantsCode's noun list above
  // means a request like "write a class and add a README" would now
  // match wantsCode too, and "readme" alone (no repo/branch/github/commit
  // anywhere) is common in plain file-save requests - too easy a
  // collision to leave in once wantsCode got broader. "codeindex"/"code
  // index" kept since that phrase alone is specific enough not to collide
  // with an ordinary filename.
  const wantsRepo = /\b(repo|repository|branch|github|commit|push|codeindex|code index)\b/.test(t);
  return wantsCode && wantsRepo;
}

/** A request to save/store generated content to a real file (a poem, a
 * letter, a script, anything) has the same "don't fragment, one call
 * needs the full picture" shape as isCodeProjectTask, for a different
 * reason: fragmenting it into "write the content" then "save it" as two
 * isolated judgment steps sends the save step to skills/general.js, which
 * has no file-system access at all - it just answers in text, and the
 * model, having nothing telling it otherwise, confidently CLAIMS the file
 * was saved ("This has been saved to /usr/poem.txt.") when nothing on
 * disk was ever touched. Caught live this way. Routes the whole request
 * to skills/file-write.js as one atomic step instead - exported so that
 * skill's own `matches()` shares this exact signal. Excludes
 * isCodeProjectTask's territory (code+repo requests keep going to
 * code-project.js, which has its own, more specific file-organization
 * logic for a whole project rather than one file). */
function isFileWriteTask(taskDescription) {
  if (isCodeProjectTask(taskDescription)) return false;
  const t = taskDescription.toLowerCase();
  // A real path (/foo/bar.txt), OR a bare filename typed informally
  // ("save it to fib.py" - no leading slash at all) - caught live: the
  // bare-filename case fell through to skills/general.js because this
  // originally only matched an actual slash-containing path.
  const hasPathOrFileWord =
    /\/[\w.\-/]+\.\w+/.test(taskDescription) ||
    /\b[a-zA-Z_][\w-]*\.[a-zA-Z]{1,5}\b/.test(taskDescription) ||
    /\bfiles?\b/.test(t);
  // "it"/"this"/"that" catches "write a haiku... and save it to X" - the
  // content-type noun list alone missed "haiku" (and would keep missing
  // whatever noun isn't thought of next) and let the request fall through
  // to normal fragmented planning, which is exactly the failure this
  // whole function exists to prevent - caught live on that exact phrase.
  //
  // The verb list itself had the same "keeps missing the next one" problem
  // one level up: round 2 tested 10 real-sounding synonyms for "save this
  // somewhere" (dump/export/persist/output/log/place/stash/produce/
  // compile/tuck) and 7 of 10 fell straight through to skills/general.js -
  // the exact original failure this file exists to prevent, just with a
  // different verb tripping it. Broadened the verb list rather than the
  // noun list this time; also fixed the same "-e/-ing/-ed" word-boundary
  // gap as isCodeProjectTask's wantsCode ("saving each in its own file"
  // didn't match a bare `\bsave\b`).
  const saveVerbs =
    "sav(?:e|es|ed|ing)|stor(?:e|es|ed|ing)|writ(?:e|es|ing)|put|creat(?:e|es|ed|ing)|mak(?:e|es|ing)|draft(?:s|ed|ing)?|compos(?:e|es|ed|ing)|jot(?:s|ted|ting)?|produc(?:e|es|ed|ing)|generat(?:e|es|ed|ing)|output(?:s|ted|ting)?|export(?:s|ed|ing)?|dump(?:s|ed|ing)?|log(?:s|ged|ging)?|stash(?:es|ed|ing)?|drop(?:s|ped|ping)?|squirrel(?:s|ed|ing)?|tuck(?:s|ed|ing)?|plac(?:e|es|ed|ing)|persist(?:s|ed|ing)?|compil(?:e|es|ed|ing)";
  // "them" added alongside "it/this/that": "log THEM to notes.md" uses the
  // plural pronoun because its antecedent ("some meeting notes") is
  // plural - missed live, same for "tuck THEM into tests.py". Object
  // nouns made plural-tolerant ("note" alone doesn't match "notes") for
  // the same reason.
  const wantsSave = new RegExp(
    `\\b(${saveVerbs})\\b[\\s\\S]*\\b(files?|documents?|notes?|poems?|letters?|essays?|stor(?:y|ies)|scripts?|reports?|text|it|this|that|them)\\b`
  ).test(t);
  return hasPathOrFileWord && wantsSave;
}

/** Plans one task into a numbered list of steps via the planner model.
 * `context` is filename -> real content, fed in verbatim so planning is
 * grounded in ground truth rather than invented specifics - the single
 * highest-impact fix from the original project (58 hallucinated steps ->
 * 8 correct ones). Optional here (a chatbot prompt often has no file to
 * ground against), but used whenever the caller has something concrete. */
async function planTask(taskDescription, context = {}, onLog) {
  const files = Object.keys(context);
  const contextBlock = files.length
    ? `\n\nGround truth - this is the ACTUAL real content of the file(s) involved. Base every step only on what is really here:\n\n` +
      files.map((f) => `--- ${f} ---\n${context[f]}\n--- END ${f} ---`).join("\n\n")
    : "";
  const prompt = `Break the following task into a numbered list of atomic steps. Each step must be a single, self-contained action - nothing chained with "and then". Do not solve the task, only plan it.

If the task is a single, self-contained request that a competent person would just answer directly in one response - a question, a short explanation, a paragraph, a summary, a simple factual lookup - output exactly ONE step that restates the task verbatim. Only produce multiple steps when the task genuinely has multiple independent parts or a real sequential dependency (e.g. research something, then use that to build something else; do X in several distinct languages/files; a multi-part question with several distinct sub-questions). Do not invent structure (like a separate "identify the variable" or "conclude with a summary" step) for a request that's naturally answered in one pass - that wastes calls without improving the answer.

When a task DOES genuinely need multiple steps (a multi-day plan, a project with several phases, a comparison with several aspects), use as FEW steps as the real structure requires - one step per day, per phase, or per aspect is normally enough. Do not split a single day/phase/aspect into separate sub-steps (e.g. a travel day into separate morning/afternoon/evening steps, or one comparison aspect into separate "describe A" and "describe B" steps) - a 3-day trip plan should usually be about 3 steps, not 10+, and each step should be substantial enough to answer in one real response, not a granular fragment of one.

Task: ${taskDescription}${contextBlock}`;

  onLog?.(`atomizer: planning via ${MODELS.planner}...`);
  const result = await chat(MODELS.planner, [{ role: "user", content: prompt }], { timeoutMs: 180000 });
  onLog?.(`atomizer: plan call resolved -> model=${result.model} tokens(in/out)=${result.promptTokens}/${result.responseTokens} duration=${result.totalDurationMs}ms`);

  // Each numbered marker starts a new step; everything up to the NEXT
  // numbered marker belongs to that step, including wrapped continuation
  // text or a fenced code block - a parser that kept only the first line
  // of each step once silently dropped the one number that mattered
  // (caught live: an "8192" requirement vanished this way).
  const lines = result.text.split(/\n/);
  const steps = [];
  let current = null;
  for (const line of lines) {
    const m = line.match(/^\s*\d+[.)]\s*(.*)$/);
    if (m) {
      if (current !== null) steps.push(current.trim());
      current = m[1];
    } else if (current !== null) {
      current += "\n" + line;
    }
  }
  if (current !== null) steps.push(current.trim());
  return steps.length ? steps.filter(Boolean) : [taskDescription];
}

/** Recursively atomize, budget-capped across the whole call tree (not per
 * branch) - a wide tree of "atomic-looking but actually compound" steps can
 * still cost dozens of calls even with a shallow depth cap. Past budget,
 * stop decomposing and accept steps as-is rather than fragmenting forever. */
async function atomize(taskDescription, { depth = 0, context = {}, budget = { calls: 0 }, onLog } = {}) {
  if (depth === 0 && isCodeProjectTask(taskDescription)) {
    onLog?.(`atomizer: task looks like a code+repo project - skipping planning, one atomic step for skills/code-project.js.`);
    return [taskDescription];
  }
  if (depth === 0 && isFileWriteTask(taskDescription)) {
    onLog?.(`atomizer: task looks like "save this to a file" - skipping planning, one atomic step for skills/file-write.js.`);
    return [taskDescription];
  }
  if (depth === 0 && isStructuralTask(taskDescription)) {
    onLog?.(`atomizer: task looks structural (creates/splits/refactors code) - skipping planning, one atomic step.`);
    return [taskDescription];
  }
  if (depth === 0 && looksLikeSingleDirectAnswer(taskDescription)) {
    onLog?.(`atomizer: task looks like a single direct-answer request - skipping planning, one atomic step.`);
    return [taskDescription];
  }
  if (depth >= MAX_ATOMIZE_DEPTH) {
    onLog?.(`atomizer: max recursion depth reached for "${taskDescription.slice(0, 60)}..." - accepting as-is.`);
    return [taskDescription];
  }
  if (budget.calls >= MAX_PLAN_CALLS) {
    onLog?.(`atomizer: planning call budget (${MAX_PLAN_CALLS}) exhausted - accepting as-is: "${taskDescription.slice(0, 60)}..."`);
    return [taskDescription];
  }
  budget.calls++;
  const steps = await planTask(taskDescription, context, onLog);
  const result = [];
  for (const step of steps) {
    if (isAtomic(step)) {
      result.push(step);
    } else if (budget.calls >= MAX_PLAN_CALLS) {
      onLog?.(`atomizer: budget exhausted mid-list - accepting remaining step as-is: "${step.slice(0, 60)}..."`);
      result.push(step);
    } else {
      onLog?.(`atomizer: step not atomic, decomposing further (depth ${depth + 1}): "${step.slice(0, 60)}..."`);
      result.push(...(await atomize(step, { depth: depth + 1, context, budget, onLog })));
    }
  }
  return result;
}

/** Mechanical (deterministic, no model needed) vs judgment (needs a
 * model). Covers command-shaped ("`git push`") and prose-phrased steps
 * alike - the planner mixes both freely. There is no executor for
 * mechanical steps wired into the scheduler yet (see engine/README.md) -
 * this classification exists now so it's visible in the UI and so a
 * future mechanical executor has something real to plug into, not so it
 * changes behavior today. */
function classifyStep(step) {
  if (isCodeProjectTask(step)) return "project";
  if (isFileWriteTask(step)) return "file-write";
  const mechanicalPatterns = [
    /^git /i,
    /^gh /i,
    /^run /i,
    /`git /,
    /`gh /,
    /\bcreate\s+(a\s+)?(new\s+)?branch\b/i,
    /\bcheckout\b.*\bbranch\b/i,
    /\bpush\b.*\b(branch|remote|repository|origin)\b/i,
    /\bcommit\b.*\bchanges?\b/i,
    /\bstage\b.*\bchanges?\b/i,
    /\bopen\s+a\s+pull\s+request\b/i,
    /\bcreate\s+a\s+pull\s+request\b/i,
    /\bclone\s+(the\s+)?repo/i,
    /\bmerge\b.*\bbranch\b/i,
  ];
  return mechanicalPatterns.some((p) => p.test(step)) ? "mechanical" : "judgment";
}

module.exports = { isAtomic, isStructuralTask, isCodeProjectTask, isFileWriteTask, looksLikeSingleDirectAnswer, planTask, atomize, classifyStep };
