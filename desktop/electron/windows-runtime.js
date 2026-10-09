'use strict';

const fs = require('node:fs');
const os = require('node:os');

function readSettings(text) {
  const values = {};
  for (const line of text.split(/\r?\n/)) {
    const match = /^\s*([A-Z][A-Z0-9_]*)\s*=\s*(.*?)\s*$/.exec(line);
    if (!match) continue;
    let value = match[2];
    if (/^['"]/.test(value)) value = value.slice(1, value.indexOf(value[0], 1));
    else value = value.replace(/\s+#.*$/, '').trim();
    values[match[1]] = value;
  }
  return values;
}

function positive(value, fallback, name, allowZero = false) {
  if (value === undefined || value === '') return fallback;
  if (!/^\d+$/.test(value) || Number(value) < (allowZero ? 0 : 1) || !Number.isSafeInteger(Number(value))) {
    throw new Error(name + (allowZero ? ' must be a nonnegative integer.' : ' must be a positive integer.'));
  }
  return Number(value);
}

function resourcePlan(settings, totalBytes = os.totalmem(), logicalCPUs = os.cpus().length) {
  const totalMB = Math.floor(totalBytes / 1024 / 1024);
  const reserveMB = positive(settings.DECURIX_WINDOWS_RESERVE_MB, Math.ceil(totalMB / 2), 'DECURIX_WINDOWS_RESERVE_MB');
  const memoryMB = positive(settings.DECURIX_WSL_MEMORY_MB, Math.max(1024, totalMB - reserveMB), 'DECURIX_WSL_MEMORY_MB');
  const cpus = positive(settings.DECURIX_WSL_CPUS, logicalCPUs, 'DECURIX_WSL_CPUS');
  if (memoryMB > totalMB) throw new Error("DECURIX_WSL_MEMORY_MB exceeds physical RAM.");
  if (cpus > logicalCPUs) throw new Error('DECURIX_WSL_CPUS exceeds the number of logical processors.');
  const workerMB = positive(settings.DECURIX_DEFAULT_DESKTOP_MEMORY_MB, 2048, 'DECURIX_DEFAULT_DESKTOP_MEMORY_MB');
  const workerCPUs = positive(settings.DECURIX_DEFAULT_DESKTOP_CPUS, Math.min(2, cpus), 'DECURIX_DEFAULT_DESKTOP_CPUS');
  if (workerCPUs > Math.min(cpus, 64) || workerMB > Math.min(memoryMB, 65536)) throw new Error('Worker defaults exceed the WSL capacity or supported per-worker limits.');
  return { memoryMB, cpus, workerMB, workerCPUs,
    swapMB: positive(settings.DECURIX_WSL_SWAP_MB, Math.min(8192, Math.max(2048, Math.ceil(memoryMB / 4))), 'DECURIX_WSL_SWAP_MB', true),
    diskGB: positive(settings.DECURIX_WSL_DISK_GB, 256, 'DECURIX_WSL_DISK_GB'),
    slots: Math.max(0, Math.floor((memoryMB - 1024) / workerMB)),
  };
}

// Preserve unknown sections, comments and existing values. Explicit overrides
// may change an existing limit; automatic defaults only fill missing keys.
function mergeWSLConfig(text, plan, settings) {
  let lines = text.replace(/^\uFEFF/, '').split(/\r?\n/);
  function set(section, key, value, overwrite) {
    let start = lines.findIndex(line => line.trim().toLowerCase() === '[' + section.toLowerCase() + ']');
    if (start < 0) { if (lines.at(-1) !== '') lines.push(''); lines.push('[' + section + ']'); start = lines.length - 1; }
    let end = start + 1;
    while (end < lines.length && !/^\s*\[/.test(lines[end])) end++;
    const at = lines.findIndex((line, i) => i > start && i < end && line.split('=')[0].trim().toLowerCase() === key.toLowerCase());
    if (at >= 0) { if (overwrite) lines[at] = key + '=' + value; }
    else lines.splice(end, 0, key + '=' + value);
  }
  for (const [key, value, env] of [
    ['memory', plan.memoryMB + 'MB', 'DECURIX_WSL_MEMORY_MB'],
    ['processors', plan.cpus, 'DECURIX_WSL_CPUS'],
    ['swap', plan.swapMB + 'MB', 'DECURIX_WSL_SWAP_MB'],
    ['defaultVhdSize', plan.diskGB + 'GB', 'DECURIX_WSL_DISK_GB'],
  ]) set('wsl2', key, value, Boolean(settings[env]));
  set('experimental', 'autoMemoryReclaim', 'gradual', false);
  return lines.join('\n').replace(/\n*$/, '\n');
}

function configureResources(configFile, settings, owned, report) {
  let original = '';
  try { original = fs.readFileSync(configFile, 'utf8'); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  const effective = { ...settings };
  let section = '';
  for (const line of original.split(/\r?\n/)) {
    if (/^\s*\[/.test(line)) { section = line.trim().toLowerCase(); continue; }
    if (section !== '[wsl2]') continue;
    const match = /^\s*(memory|processors|swap)\s*=\s*([^#;]+)/i.exec(line);
    if (!match) continue;
    const keys = { memory: 'DECURIX_WSL_MEMORY_MB', processors: 'DECURIX_WSL_CPUS', swap: 'DECURIX_WSL_SWAP_MB' };
    const key = keys[match[1].toLowerCase()];
    if (effective[key] || (key === 'DECURIX_WSL_MEMORY_MB' && settings.DECURIX_WINDOWS_RESERVE_MB)) continue;
    const size = /^([0-9]+)\s*(MB|GB)?$/i.exec(match[2].trim());
    if (size && (Number(size[1]) > 0 || key === 'DECURIX_WSL_SWAP_MB')) effective[key] = String(Number(size[1]) * (size[2]?.toUpperCase() === 'GB' ? 1024 : 1));
  }
  const plan = resourcePlan(effective);
  const explicit = ['DECURIX_WSL_MEMORY_MB', 'DECURIX_WSL_CPUS', 'DECURIX_WSL_SWAP_MB', 'DECURIX_WSL_DISK_GB', 'DECURIX_WINDOWS_RESERVE_MB'].some(key => settings[key]);
  if (owned || explicit) {
    const overrides = { ...settings };
    if (settings.DECURIX_WINDOWS_RESERVE_MB && !settings.DECURIX_WSL_MEMORY_MB) overrides.DECURIX_WSL_MEMORY_MB = String(plan.memoryMB);
    const next = mergeWSLConfig(original, plan, overrides);
    if (next !== original) {
      if (original) fs.writeFileSync(configFile + '.decurix.bak', original, 'utf8');
      fs.writeFileSync(configFile, next, 'utf8');
      report('WSL resource settings saved. They apply when WSL next starts; quit running WSL workloads before running wsl --shutdown.');
    }
  }
  report('WSL target: ' + plan.memoryMB + ' MB RAM, ' + plan.cpus + ' logical CPUs; about ' + plan.slots + ' active ' + plan.workerMB + ' MB workers after control-plane overhead. CPU time and disk are shared.');
  return plan;
}

function usesWindowsOllama(settings) {
  if (settings.DECURIX_WINDOWS_NATIVE_OLLAMA === '0') return false;
  return !settings.OLLAMA_URL || ['http://decurix-ollama:11434', 'http://host.docker.internal:11434'].includes(settings.OLLAMA_URL.replace(/\/$/, ''));
}

function windowsHost(route, interfaces = os.networkInterfaces(), override = '') {
  const addresses = Object.values(interfaces).flat().filter(Boolean).filter(a => a.family === 'IPv4' && !a.internal).map(a => a.address);
  if (override) {
    if (!addresses.includes(override)) throw new Error('DECURIX_WINDOWS_OLLAMA_HOST must name a local Windows IPv4 address reachable from WSL.');
    return override;
  }
  const gateway = /default via (\d+\.\d+\.\d+\.\d+)/.exec(route)?.[1];
  if (gateway && addresses.includes(gateway)) return gateway;
  throw new Error('Could not find the Windows address on the WSL network. Set DECURIX_WINDOWS_OLLAMA_HOST to a Windows IPv4 address reachable from WSL (needed for mirrored networking).');
}

function stackSettings(original, settings, plan, host) {
  const values = {
    DECURIX_DEFAULT_DESKTOP_CPUS: settings.DECURIX_DEFAULT_DESKTOP_CPUS || plan.workerCPUs,
    DECURIX_DEFAULT_DESKTOP_MEMORY_MB: settings.DECURIX_DEFAULT_DESKTOP_MEMORY_MB || plan.workerMB,
  };
  if (!host && settings.DECURIX_WINDOWS_NATIVE_OLLAMA === '0' && !settings.OLLAMA_URL) values.OLLAMA_URL = 'http://decurix-ollama:11434';
  if (host) Object.assign(values, {
    OLLAMA_URL: 'http://' + host + ':11435',
    // Workers keep one stable name as the hardware-selected model changes.
    OLLAMA_MODEL: 'decurix-local',
    OLLAMA_KEEP_ALIVE: settings.OLLAMA_KEEP_ALIVE || '-1',
    DECURIX_WINDOWS_OLLAMA_URL: 'http://' + host + ':11435',
    DECURIX_WINDOWS_SELECTOR_URL: 'http://' + host + ':11436',
    // Model preparation is explicit in the desktop shell.
    DECURIX_MODEL_AUTOPULL: '0',
    OLLAMA_NUM_THREAD: settings.OLLAMA_NUM_THREAD || Math.max(1, Math.floor(os.cpus().length / 2)),
  });
  let result = original;
  // No secrets are logged; settings only travel through the local files.
  for (const [key, value] of Object.entries(values)) {
    const re = new RegExp('^\\s*' + key + '\\s*=.*$', 'gm');
    if (re.test(result)) result = result.replace(re, key + '=' + value);
    else result += (result.endsWith('\n') ? '' : '\n') + key + '=' + value + '\n';
  }
  return result;
}

module.exports = { readSettings, resourcePlan, mergeWSLConfig, configureResources, usesWindowsOllama, windowsHost, stackSettings };
