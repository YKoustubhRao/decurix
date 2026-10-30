"use strict";

/**
 * Minimal .env loader - no external dependency (this project avoids them
 * on principle). Loads KEY=VALUE lines into process.env without
 * overwriting anything already set, so a real env var always wins over
 * the file.
 *
 * The .env file itself is gitignored (see the repo's root .gitignore's
 * `*.env` rule, confirmed with `git check-ignore`) - it holds GH_TOKEN,
 * which must never end up in git history. It IS baked into the Docker
 * image (Dockerfile COPYs it in) so the container has it without the user
 * re-entering it every run; that's a deliberate, contained tradeoff for a
 * local-only image that never gets pushed to a registry, not the same
 * risk as committing it to source control.
 */

const fs = require("node:fs");
const path = require("node:path");

function loadEnvFile(file = path.join(__dirname, "..", ".env")) {
  if (!fs.existsSync(file)) return;
  const lines = fs.readFileSync(file, "utf8").split("\n");
  for (const line of lines) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const idx = trimmed.indexOf("=");
    if (idx === -1) continue;
    const key = trimmed.slice(0, idx).trim();
    const value = trimmed.slice(idx + 1).trim();
    if (!(key in process.env)) process.env[key] = value;
  }
}

module.exports = { loadEnvFile };
