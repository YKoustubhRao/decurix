"use strict";

/**
 * Git + GitHub operations - deterministic, no model call for any of this
 * (same founding rule as everything else in this engine: a model never
 * decides mechanics it doesn't need to). Auth is a single Personal Access
 * Token read from GH_TOKEN (see lib/env.js) - a Linux container can't use
 * whatever OS credential manager `gh auth login` would normally rely on,
 * so a token passed in directly is the reliable path.
 */

const { execFileSync } = require("node:child_process");

const GITHUB_API = "https://api.github.com";

function token() {
  const t = process.env.GH_TOKEN;
  if (!t) throw new Error("GH_TOKEN is not set - see soul-guard/engine/.env");
  return t;
}

async function getAuthenticatedUser() {
  const res = await fetch(`${GITHUB_API}/user`, {
    headers: { Authorization: `Bearer ${token()}`, Accept: "application/vnd.github+json" },
    signal: AbortSignal.timeout(10000),
  });
  if (!res.ok) throw new Error(`GitHub auth check failed: HTTP ${res.status}`);
  return res.json();
}

async function repoExists(owner, name) {
  const res = await fetch(`${GITHUB_API}/repos/${owner}/${name}`, {
    headers: { Authorization: `Bearer ${token()}`, Accept: "application/vnd.github+json" },
    signal: AbortSignal.timeout(10000),
  });
  return res.status === 200;
}

/** Creates the repo if it doesn't already exist; returns its info either way. */
async function ensureRepo(owner, name, { description = "", isPrivate = false } = {}) {
  if (await repoExists(owner, name)) {
    const res = await fetch(`${GITHUB_API}/repos/${owner}/${name}`, {
      headers: { Authorization: `Bearer ${token()}`, Accept: "application/vnd.github+json" },
    });
    return { created: false, repo: await res.json() };
  }
  const res = await fetch(`${GITHUB_API}/user/repos`, {
    method: "POST",
    headers: { Authorization: `Bearer ${token()}`, Accept: "application/vnd.github+json", "Content-Type": "application/json" },
    body: JSON.stringify({ name, description, private: isPrivate, auto_init: false }),
    signal: AbortSignal.timeout(15000),
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`Failed to create repo "${name}": HTTP ${res.status} ${body.slice(0, 300)}`);
  }
  return { created: true, repo: await res.json() };
}

function git(dir, args) {
  return execFileSync("git", args, { cwd: dir, encoding: "utf8" }).trim();
}

function ensureGitRepo(dir) {
  try {
    git(dir, ["rev-parse", "--is-inside-work-tree"]);
  } catch {
    git(dir, ["init", "-b", "main"]);
  }
  // Always set identity explicitly - a fresh container has no global git
  // config, and a commit with no configured identity fails outright.
  git(dir, ["config", "user.email", "agent-engine@local"]);
  git(dir, ["config", "user.name", "Agent Engine"]);
}

/** Stages and commits everything; returns false (no error) if there was
 * nothing to commit, matching the legacy pipeline's "skip cleanly rather
 * than fail on a no-op commit" behavior. */
function commitAll(dir, message) {
  git(dir, ["add", "-A"]);
  const status = git(dir, ["status", "--porcelain"]);
  if (!status) return false;
  git(dir, ["commit", "-m", message]);
  return true;
}

function checkoutBranch(dir, branch) {
  try {
    git(dir, ["checkout", "-b", branch]);
  } catch {
    git(dir, ["checkout", branch]);
  }
}

/** Starts `branch` with no history and no inherited files if it's new
 * (git's --orphan), or just switches to it if it already exists (a rerun
 * of the same task updates the same branch rather than duplicating it).
 * Matters because the workspace repo is shared across different one-shot
 * code-project tasks over time - without this, a brand-new branch would
 * start from whatever files happened to be on the current branch,
 * silently mixing an unrelated earlier project's files into a new one. */
function startCleanBranch(dir, branch) {
  const existing = git(dir, ["branch", "--list", branch]);
  if (existing) {
    git(dir, ["checkout", branch]);
    return;
  }
  git(dir, ["checkout", "--orphan", branch]);
  try {
    const tracked = git(dir, ["ls-files"]);
    if (tracked) git(dir, ["rm", "-rf", "."]);
  } catch {
    /* nothing tracked yet on a fresh orphan branch - fine */
  }
}

/** Pushes via a token-embedded HTTPS remote (the standard CI pattern) -
 * no credential helper needed, works the same on a fresh container every
 * time. The remote is reset each call so a stale/wrong URL never lingers. */
function pushBranch(dir, owner, repo, branch) {
  const authUrl = `https://x-access-token:${token()}@github.com/${owner}/${repo}.git`;
  try {
    git(dir, ["remote", "remove", "origin"]);
  } catch {
    /* no remote yet - fine */
  }
  git(dir, ["remote", "add", "origin", authUrl]);
  return git(dir, ["push", "-u", "origin", branch]);
}

module.exports = { getAuthenticatedUser, repoExists, ensureRepo, ensureGitRepo, commitAll, checkoutBranch, startCleanBranch, pushBranch, git };
