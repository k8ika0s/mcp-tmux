#!/usr/bin/env node

import { execa } from 'execa';
import { parseArgs } from 'node:util';
import fs from 'node:fs/promises';
import path from 'node:path';
import { z } from 'zod';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { ErrorCode, McpError } from '@modelcontextprotocol/sdk/types.js';
import { InMemoryTaskStore, InMemoryTaskMessageQueue } from '@modelcontextprotocol/sdk/experimental/tasks/stores/in-memory.js';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const PKG_META: { version: string; name: string } = (() => {
  try {
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const pkg = require('../package.json');
    return { version: pkg.version as string, name: pkg.name as string };
  } catch {
    return { version: process.env.npm_package_version ?? 'unknown', name: 'mcp-tmux' };
  }
})();
const VERSION = PKG_META.version;
const PACKAGE_NAME = PKG_META.name;

type TmuxSession = {
  id: string;
  name: string;
  windows: number;
  attached: number;
  created: number;
};

type TmuxWindow = {
  session: string;
  id: string;
  index: number;
  name: string;
  active: boolean;
  panes: number;
  flags: string;
};

type TmuxPane = {
  session: string;
  window: string;
  id: string;
  index: number;
  active: boolean;
  tty: string;
  command: string;
  title: string;
};

const tmuxBinary = process.env.TMUX_BIN || 'tmux';
const tmuxFallbackPaths = ['/opt/homebrew/bin', '/usr/local/bin', '/usr/bin'];
const extraPath = tmuxFallbackPaths.join(':');
const tmuxCommandTimeoutMs = Number(process.env.MCP_TMUX_TIMEOUT_MS ?? '15000');
const hostProfilePath =
  process.env.MCP_TMUX_HOSTS_FILE ||
  path.join(process.env.HOME || process.cwd(), '.config', 'mcp-tmux', 'hosts.json');
const logBaseDir =
  process.env.MCP_TMUX_LOG_DIR || path.join(process.env.HOME || process.cwd(), '.config', 'mcp-tmux', 'logs');
const layoutProfilePath = path.join(process.env.HOME || process.cwd(), '.config', 'mcp-tmux', 'layouts.json');
const defaultCapturePageSizes = [20, 100, 400]; // incremental paging budget
const defaultMaxPages = 3;
let hostProfiles: Record<
  string,
  {
    pathAdd?: string[];
    tmuxBin?: string;
    defaultSession?: string;
  }
> = {};
let layoutProfiles: Record<
  string,
  {
    host?: string;
    session: string;
    windows: { index: number; name: string; layout: string }[];
  }
> = {};
let defaultHost = process.env.MCP_TMUX_HOST || undefined;
let defaultSession = process.env.MCP_TMUX_SESSION || undefined;
let defaultWindow: string | undefined;
let defaultPane: string | undefined;
const defaultTargetNote = () =>
  `Defaults -> host: ${defaultHost ?? '(unset)'}, session: ${defaultSession ?? '(unset)'}, pane: ${
    defaultPane ?? '(unset)'
  }`;
const isoTimestamp = () => new Date().toISOString();

const instructions = `
You are connected to a tmux MCP server. Use these tools to collaborate with a human inside tmux.

- Playbook: (1) tmux.open_session (host+session), (2) tmux.default_context, (3) tmux.list_windows/panes, (4) tmux.send_keys or tmux.run_batch, then tmux.capture_pane, (5) repeat.
- Targets: session, session:window, session:window.pane, or IDs. Use tmux.set_default to pin host/session/window/pane.
- Remote: provide host (ssh alias) + session or set MCP_TMUX_HOST/MCP_TMUX_SESSION. Server runs tmux via ssh -T <host> tmux ....
- Safety: destructive tools require confirm=true; prefer tmux.command only when helpers don’t cover it.
- After sending keys, always capture-pane to read output; re-list panes/windows to stay in sync.
- Helpers: tmux.tail_pane (poll output), tmux.capture_layout/tmux.restore_layout (save/apply layouts), host profiles via MCP_TMUX_HOSTS_FILE for per-host PATH/tmux bin defaults.
- Fanout: tmux.multi_run to send the same command to multiple hosts/panes and aggregate results.
- Batching: tmux.run_batch runs multiple commands in one call; tmux.batch_capture gathers multiple panes in one call (readonly). run_batch can clean the prompt (C-c/C-u for bash/zsh) before writes and auto-captures output with paging.
- Keys: tmux.send_keys accepts <SPACE>/<ENTER>/<TAB>/<ESC> tokens; empty keys with enter=true will send Enter.
`.trim();

function assertValidHost(host?: string) {
  if (!host) return;
  if (host.startsWith('-')) {
    throw new McpError(ErrorCode.InvalidParams, 'host may not start with "-" (disallowed ssh options)');
  }
  if (/\s/.test(host)) {
    throw new McpError(ErrorCode.InvalidParams, 'host may not contain whitespace');
  }
}

function sanitizePathSegment(segment: string | undefined, fallback = 'unknown') {
  const cleaned = (segment ?? fallback).replace(/[^A-Za-z0-9_.-]/g, '_');
  return cleaned || fallback;
}

async function runTmux(args: string[], host?: string) {
  try {
    assertValidHost(host);
    const hostConfig = getHostProfile(host);
    const bin = hostConfig?.tmuxBin || tmuxBinary;
    const pathAdd = hostConfig?.pathAdd ?? [];
    const basePath = buildPath(process.env.PATH, [...tmuxFallbackPaths, ...pathAdd]);
    const command = host ? 'ssh' : bin;
    const commandArgs = host ? ['-T', host, 'env', `PATH=${basePath}`, bin, ...args] : args;
    const { stdout } = await execa(command, commandArgs, {
      env: host ? undefined : { ...process.env, PATH: basePath },
      timeout: tmuxCommandTimeoutMs,
    });
    return stdout.trim();
  } catch (error) {
    const err = error as { stderr?: string; stdout?: string; message: string };
    const detail = err.stderr || err.stdout || err.message;
    throw new McpError(
      ErrorCode.InternalError,
      `${host ? `ssh ${host} ` : ''}tmux ${args.join(' ')} failed: ${detail}`.trim(),
    );
  }
}

async function capturePaged(target: string, host: string | undefined, pageSizes = defaultCapturePageSizes) {
  // Try progressively larger captures until we either cover history or exhaust sizes.
  const historySizeRaw = await runTmux(['display-message', '-p', '#{history_size}'], host).catch(() => '0');
  const historySize = Number(historySizeRaw) || 0;
  let captured = '';
  let usedLines = 0;
  let usedPage = 0;
  let moreAvailable = false;

  for (let i = 0; i < pageSizes.length; i++) {
    const lines = pageSizes[i];
    const output = await capturePane(target, -lines, undefined, host);
    usedLines = lines;
    usedPage = i + 1;
    captured = output;
    if (output.split('\n').length >= Math.min(lines, historySize) || lines >= historySize) {
      moreAvailable = false;
      break;
    }
    moreAvailable = true;
  }

  return {
    captured,
    requested: usedLines,
    historySize,
    pagesTried: usedPage,
    moreAvailable: moreAvailable || (historySize > usedLines),
  };
}

function resolveHost(host?: string) {
  return host ?? defaultHost;
}

function resolveSession(session?: string) {
  return session ?? defaultSession;
}

function resolvePaneTarget(target?: string) {
  return target ?? defaultPane;
}

function requirePaneTarget(target?: string) {
  const resolved = resolvePaneTarget(target);
  if (!resolved) {
    throw new McpError(
      ErrorCode.InvalidParams,
      'target is required (provide target or set default pane via tmux.set_default or tmux.select_pane)',
    );
  }
  return resolved;
}

function isDestructiveTmuxArgs(args: string[]) {
  if (!args.length) return false;
  const verbs = new Set(['kill-session', 'kill-window', 'kill-pane', 'kill-server', 'unlink-window', 'unlink-pane']);
  const first = args[0];
  if (verbs.has(first) || first.startsWith('kill-')) return true;
  if (first === 'attach' && args.includes('-k')) return true;
  return false;
}

function requireHost(host?: string) {
  const resolved = resolveHost(host);
  if (!resolved) {
    throw new McpError(ErrorCode.InvalidParams, 'host is required (set host param or MCP_TMUX_HOST)');
  }
  return resolved;
}

function requireSession(session?: string) {
  const resolved = resolveSession(session);
  if (!resolved) {
    throw new McpError(
      ErrorCode.InvalidParams,
      'session is required (set session param, MCP_TMUX_SESSION, or use tmux.open_session)',
    );
  }
  return resolved;
}

async function ensureLocalTmuxAvailable() {
  try {
    await runTmux(['-V']);
  } catch (error) {
    // Keep server running even if local tmux missing; remote hosts may still be usable.
    console.warn(`Warning: local tmux unavailable (${tmuxBinary} -V failed). Remote hosts may still work.`, error);
  }
}

async function detectDefaultSession(): Promise<string | undefined> {
  if (defaultSession) {
    return defaultSession;
  }

  if (process.env.TMUX) {
    try {
      const name = await runTmux(['display-message', '-p', '#S']);
      return name || undefined;
    } catch {
      // Ignore; not inside an attached tmux client.
    }
  }

  return undefined;
}

async function listSessions(host?: string): Promise<TmuxSession[]> {
  const fmt = '#{session_id}\t#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_created}';
  const raw = await runTmux(['list-sessions', '-F', fmt], host);
  if (!raw) return [];

  return raw.split('\n').map((line) => {
    const [id, name, windows, attached, created] = line.split('\t');
    return {
      id,
      name,
      windows: Number(windows),
      attached: Number(attached),
      created: Number(created),
    };
  });
}

async function listWindows(target?: string, host?: string): Promise<TmuxWindow[]> {
  const fmt =
    '#{session_name}\t#{window_id}\t#{window_index}\t#{window_name}\t#{window_active}\t#{window_panes}\t#{window_flags}';
  const args = ['list-windows', '-F', fmt];
  if (target) {
    args.push('-t', target);
  }

  const raw = await runTmux(args, host);
  if (!raw) return [];

  return raw.split('\n').map((line) => {
    const [session, id, index, name, active, panes, flags] = line.split('\t');
    return {
      session,
      id,
      index: Number(index),
      name,
      active: active === '1',
      panes: Number(panes),
      flags,
    };
  });
}

async function listPanes(target?: string, host?: string): Promise<TmuxPane[]> {
  const fmt =
    '#{session_name}\t#{window_id}\t#{pane_id}\t#{pane_index}\t#{pane_active}\t#{pane_tty}\t#{pane_current_command}\t#{pane_title}';
  const args = ['list-panes', '-F', fmt];
  if (target) {
    args.push('-t', target);
  }

  const raw = await runTmux(args, host);
  if (!raw) return [];

  return raw.split('\n').map((line) => {
    const [session, window, id, index, active, tty, command, title] = line.split('\t');
    return {
      session,
      window,
      id,
      index: Number(index),
      active: active === '1',
      tty,
      command,
      title,
    };
  });
}

async function capturePane(target: string, start?: number, end?: number, host?: string) {
  const args = ['capture-pane', '-p', '-t', target];
  if (typeof start === 'number') {
    args.push('-S', start.toString());
  } else {
    args.push('-S', '-200'); // default: last ~200 lines
  }
  if (typeof end === 'number') {
    args.push('-E', end.toString());
  }

  return runTmux(args, host);
}

async function sendKeys(target: string, keys: string, enter?: boolean, host?: string) {
  const specialMap: Record<string, string> = {
    '<SPACE>': 'Space',
    '<TAB>': 'Tab',
    '<ESC>': 'Escape',
    '<ENTER>': 'Enter',
  };

  // Allow empty when enter=true (send Enter only)
  const trimmed = keys?.trim() ?? '';
  if (!keys && enter) {
    await runTmux(['send-keys', '-t', target, 'Enter'], host);
    return;
  }
  if (!keys && !enter) {
    throw new McpError(ErrorCode.InvalidParams, 'keys must be non-empty or enter=true to send Enter');
  }

  const mapped = specialMap[keys] || specialMap[trimmed] || null;
  const args = ['send-keys', '-t', target, '--'];

  if (mapped) {
    args.push(mapped);
  } else {
    // Permit whitespace (e.g., single space)
    args.push(keys);
  }

  if (enter && mapped !== 'Enter') {
    args.push('Enter');
  }

  await runTmux(args, host);
}

async function createSession(name: string, command?: string, host?: string) {
  if (!name || !name.trim()) {
    throw new McpError(ErrorCode.InvalidParams, 'session name is required');
  }
  const args = ['new-session', '-d', '-s', name];
  if (command) {
    args.push(command);
  }
  await runTmux(args, host);
}

async function createWindow(target: string, name?: string, command?: string, host?: string) {
  const args = ['new-window', '-t', target];
  const finalName = name || `llm-window-${Date.now().toString(36)}`;
  if (name) {
    args.push('-n', name);
  } else {
    args.push('-n', finalName);
  }
  if (command) {
    args.push(command);
  }
  await runTmux(args, host);
  return finalName;
}

async function splitPane(target: string, orientation: 'horizontal' | 'vertical', command?: string, host?: string) {
  const args = ['split-window', '-t', target];
  if (orientation === 'horizontal') {
    args.push('-h');
  } else {
    args.push('-v');
  }
  if (command) {
    args.push(command);
  }
  await runTmux(args, host);
}

async function killSession(target: string, host?: string) {
  await runTmux(['kill-session', '-t', target], host);
}

async function killWindow(target: string, host?: string) {
  await runTmux(['kill-window', '-t', target], host);
}

async function killPane(target: string, host?: string) {
  await runTmux(['kill-pane', '-t', target], host);
}

async function renameSession(target: string, name: string, host?: string) {
  await runTmux(['rename-session', '-t', target, name], host);
}

async function renameWindow(target: string, name: string, host?: string) {
  await runTmux(['rename-window', '-t', target, name], host);
}

async function ensureSession(host: string | undefined, session: string, command?: string) {
  let existed = true;
  try {
    await runTmux(['has-session', '-t', session], host);
  } catch {
    existed = false;
    const args = ['new-session', '-d', '-s', session];
    if (command) {
      args.push(command);
    }
    await runTmux(args, host);
  }
  return existed;
}

async function setPaneTitle(target: string | undefined, title: string, host?: string) {
  const args = ['select-pane', '-T', title];
  if (target) {
    args.push('-t', target);
  }
  await runTmux(args, host);
}

function summarizeDefaults() {
  return [
    `host: ${defaultHost ?? '(unset)'}`,
    `session: ${defaultSession ?? '(unset)'}`,
    `window: ${defaultWindow ?? '(unset)'}`,
    `pane: ${defaultPane ?? '(unset)'}`,
  ].join('\n');
}

function formatSessions(sessions: TmuxSession[]) {
  if (!sessions.length) return 'No tmux sessions found.';
  return sessions
    .map(
      (s) =>
        `${s.name} (${s.id}) windows=${s.windows} attached=${s.attached} started=${new Date(
          s.created * 1000,
        ).toISOString()}`,
    )
    .join('\n');
}

function formatWindows(windows: TmuxWindow[]) {
  if (!windows.length) return 'No windows found.';
  return windows
    .map(
      (w) =>
        `${w.session}:${w.index} ${w.name} (${w.id}) active=${w.active} panes=${w.panes} flags=${w.flags}`,
    )
    .join('\n');
}

function formatPanes(panes: TmuxPane[]) {
  if (!panes.length) return 'No panes found.';
  return panes
    .map(
      (p) =>
        `${p.session}:${p.window}.${p.index} ${p.id} active=${p.active} tty=${p.tty} cmd=${p.command} title=${p.title}`,
    )
    .join('\n');
}

function getHostProfile(host?: string) {
  if (!host) return undefined;
  return hostProfiles[host];
}

export function buildPath(current: string | undefined, additions: string[]) {
  const parts = current ? current.split(':') : [];
  for (const entry of additions) {
    if (!entry) continue;
    if (!parts.includes(entry)) {
      parts.push(entry);
    }
  }
  return parts.join(':');
}

async function loadHostProfiles() {
  try {
    const data = await fs.readFile(hostProfilePath, 'utf8');
    const parsed = JSON.parse(data) as typeof hostProfiles;
    hostProfiles = parsed;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') {
      console.warn(`Failed to read host profile file at ${hostProfilePath}:`, error);
    }
    hostProfiles = {};
  }
}

async function loadLayoutProfiles() {
  try {
    const data = await fs.readFile(layoutProfilePath, 'utf8');
    const parsed = JSON.parse(data) as typeof layoutProfiles;
    layoutProfiles = parsed;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') {
      console.warn(`Failed to read layout profile file at ${layoutProfilePath}:`, error);
    }
    layoutProfiles = {};
  }
}

async function persistLayouts() {
  try {
    await fs.mkdir(path.dirname(layoutProfilePath), { recursive: true });
    await fs.writeFile(layoutProfilePath, JSON.stringify(layoutProfiles, null, 2), 'utf8');
  } catch (error) {
    console.warn(`Failed to persist layout profiles to ${layoutProfilePath}:`, error);
  }
}

async function buildStateSnapshot({
  host,
  session,
  captureLines = 200,
}: {
  host?: string;
  session?: string;
  captureLines?: number;
}) {
  const resolvedHost = resolveHost(host);
  const resolvedSession = resolveSession(session);
  if (!resolvedSession) {
    throw new McpError(
      ErrorCode.InvalidParams,
      'session is required to build a state snapshot (set default or pass session).',
    );
  }

  const sessions = await listSessions(resolvedHost);
  const windows = await listWindows(resolvedSession, resolvedHost);
  const panes = await listPanes(resolvedSession, resolvedHost);
  const activeWindow = windows.find((w) => w.active);
  const activePane = panes.find((p) => p.active && (!defaultPane || p.id === defaultPane)) || panes.find((p) => p.active);
  const targetPane = defaultPane ?? activePane?.id;
  let capture = '(no capture target)';
  if (targetPane) {
    capture = await capturePane(targetPane, -captureLines, undefined, resolvedHost);
  }

  return {
    host: resolvedHost ?? '(local)',
    session: resolvedSession,
    windows,
    panes,
    captureTarget: targetPane,
    capture,
    sessionsText: formatSessions(sessions),
    windowsText: formatWindows(windows),
    panesText: formatPanes(panes),
  };
}

async function captureLayouts(session: string, host?: string) {
  const fmt = '#{window_id}\t#{window_layout}\t#{window_name}\t#{window_index}';
  const output = await runTmux(['list-windows', '-t', session, '-F', fmt], host);
  return output
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      const [id, layout, name, index] = line.split('\t');
      return { id, layout, name, index: Number(index) };
    });
}

async function applyLayout(target: string, layout: string, host?: string) {
  await runTmux(['select-layout', '-t', target, layout], host);
}

async function tailPane({
  host,
  target,
  lines,
  iterations,
  intervalMs,
}: {
  host?: string;
  target: string;
  lines: number;
  iterations: number;
  intervalMs: number;
}) {
  const resolvedHost = resolveHost(host);
  let lastCapture = '';
  for (let i = 0; i < iterations; i++) {
    lastCapture += `\n--- tail iteration ${i + 1}/${iterations} ---\n`;
    lastCapture += await capturePane(target, -lines, undefined, resolvedHost);
    if (i < iterations - 1) {
      await new Promise((r) => setTimeout(r, intervalMs));
    }
  }
  return lastCapture.trim();
}

function extractRecentCommands(text: string, max = 15) {
  const cmds: string[] = [];
  const lines = text.split('\n');
  const promptPattern = /[$#>] ([^\s].*)$/;
  for (let i = lines.length - 1; i >= 0 && cmds.length < max; i--) {
    const line = lines[i];
    const match = line.match(promptPattern);
    if (match && match[1]) {
      cmds.push(match[1]);
    }
  }
  return cmds.reverse();
}

const auditFlags: Record<string, boolean> = {};

function auditKey(host?: string, session?: string) {
  return `${host ?? defaultHost ?? 'local'}:${session ?? defaultSession ?? 'unknown'}`;
}

function isAuditEnabled(host?: string, session?: string) {
  return Boolean(auditFlags[auditKey(host, session)]);
}

function setAuditEnabled(host: string | undefined, session: string | undefined, enabled: boolean) {
  auditFlags[auditKey(host, session)] = enabled;
}

async function auditLog(host: string | undefined, session: string | undefined, event: string, meta?: unknown) {
  if (!isAuditEnabled(host, session)) return;
  const h = sanitizePathSegment(host ?? defaultHost, 'local');
  const s = sanitizePathSegment(session ?? defaultSession, 'unknown');
  const dir = path.join(logBaseDir, h, s);
  const file = path.join(dir, `audit-${isoTimestamp().slice(0, 10)}.log`);
  await fs.mkdir(dir, { recursive: true });
  const line = `[${isoTimestamp()}] ${event}${meta !== undefined ? ` ${JSON.stringify(meta)}` : ''}\n`;
  await fs.appendFile(file, line);
}

function getSessionFromTarget(target: string | undefined) {
  if (!target) return defaultSession;
  const parts = target.split(':');
  return parts[0] || defaultSession;
}

async function appendSessionLog(host: string | undefined, session: string | undefined, message: string) {
  const h = sanitizePathSegment(host ?? defaultHost, 'local');
  const s = sanitizePathSegment(session ?? defaultSession, 'unknown');
  const dir = path.join(logBaseDir, h, s);
  const file = path.join(dir, `${isoTimestamp().slice(0, 10)}.log`);
  await fs.mkdir(dir, { recursive: true });
  await fs.appendFile(file, `[${isoTimestamp()}] ${message}\n`);
}

async function captureHistory({
  host,
  session,
  target,
  lines = 800,
  allPanes = false,
}: {
  host?: string;
  session?: string;
  target?: string;
  lines?: number;
  allPanes?: boolean;
}) {
  const resolvedHost = resolveHost(host);
  if (target) {
    const capture = await capturePane(target, -lines, undefined, resolvedHost);
    return { captures: [{ target, text: capture }], commands: extractRecentCommands(capture) };
  }
  const resolvedSession = requireSession(session);
  const panes = await listPanes(resolvedSession, resolvedHost);
  const targets = allPanes ? panes : panes.filter((p) => p.active);
  const captures = await Promise.all(
    targets.map(async (p) => ({
      target: `${p.session}:${p.window}.${p.index}`,
      text: await capturePane(p.id, -lines, undefined, resolvedHost),
    })),
  );
  const combinedText = captures.map((c) => c.text).join('\n');
  return { captures, commands: extractRecentCommands(combinedText) };
}

async function listDirSimple(dir: string, host?: string) {
  if (host) {
    assertValidHost(host);
    const { stdout } = await execa('ssh', ['-T', host, 'ls', '-1', dir], { timeout: tmuxCommandTimeoutMs });
    return stdout.split('\n').filter(Boolean);
  }
  const entries = await fs.readdir(dir);
  return entries.sort();
}

export function diffNewFiles(prev: string[], next: string[]) {
  const prevSet = new Set(prev);
  return next.filter((f) => !prevSet.has(f));
}

async function fanoutSendCapture({
  targets,
  keys,
  enter,
  capture = true,
  captureLines = 200,
  delayMs = 0,
  mode = 'send_capture',
  pattern,
  patternFlags,
  tailIterations = 3,
  tailIntervalMs = 1000,
}: {
  targets: { host?: string; target?: string; captureLines?: number; delayMs?: number }[];
  keys: string;
  enter: boolean;
  capture?: boolean;
  captureLines?: number;
  delayMs?: number;
  mode?: 'send_capture' | 'tail' | 'pattern';
  pattern?: string;
  patternFlags?: string;
  tailIterations?: number;
  tailIntervalMs?: number;
}) {
  const results = await Promise.allSettled(
    targets.map(async (t) => {
      const resolvedHost = resolveHost(t.host);
      const paneTarget = t.target ?? defaultPane;
      if (!paneTarget) {
        throw new McpError(ErrorCode.InvalidParams, 'target is required (or set default pane)');
      }
      const sessionForLog = getSessionFromTarget(paneTarget);
      await sendKeys(paneTarget, keys, enter, resolvedHost);
      await auditLog(resolvedHost, sessionForLog, 'multi_run.send_keys', {
        target: paneTarget,
        keys,
        enter,
        mode,
      });
      if (delayMs && delayMs > 0) {
        await new Promise((r) => setTimeout(r, delayMs));
      }
      let output = '';
      if (mode === 'tail') {
        output = await tailPane({
          host: resolvedHost,
          target: paneTarget,
          lines: t.captureLines ?? captureLines,
          iterations: tailIterations,
          intervalMs: tailIntervalMs,
        });
      } else if (mode === 'pattern') {
        const regex = new RegExp(pattern ?? '.*', patternFlags);
        const capture = await capturePane(paneTarget, -(t.captureLines ?? captureLines), undefined, resolvedHost);
        if (regex.test(capture)) {
          output = `Pattern matched.\n${capture}`;
        } else {
          output = `Pattern not found.\n${capture}`;
        }
      } else if (capture) {
        output = await capturePane(paneTarget, -(t.captureLines ?? captureLines), undefined, resolvedHost);
        await auditLog(resolvedHost, sessionForLog, 'multi_run.capture', {
          target: paneTarget,
          length: output.length,
        });
      }
      return { host: resolvedHost ?? 'local', target: paneTarget, output };
    }),
  );

  const lines: string[] = [];
  let ok = 0;
  let fail = 0;
  for (const r of results) {
    if (r.status === 'fulfilled') {
      ok++;
      lines.push(`== ${r.value.host} ${r.value.target} ==`);
      if (capture) {
        lines.push(r.value.output || '(no output)');
      } else {
        lines.push('(capture disabled)');
      }
    } else {
      fail++;
      const msg = r.reason instanceof Error ? r.reason.message : String(r.reason);
      lines.push(`== error ==`);
      lines.push(msg);
    }
  }
  lines.push('');
  lines.push(`Summary: ${ok} succeeded, ${fail} failed`);
  return lines.join('\n');
}

async function saveLayoutProfile(name: string, session: string, host?: string) {
  const windows = await captureLayouts(session, host);
  layoutProfiles[name] = {
    host,
    session,
    windows,
  };
  await persistLayouts();
}

async function applyLayoutProfile(name: string, targetSession?: string, host?: string) {
  const profile = layoutProfiles[name];
  if (!profile) {
    throw new McpError(ErrorCode.InvalidParams, `No layout profile named '${name}' found.`);
  }
  const resolvedHost = resolveHost(host) ?? profile.host;
  const session = targetSession ?? profile.session;
  for (const w of profile.windows) {
    const windowTarget = `${session}:${w.index}`;
    try {
      await applyLayout(windowTarget, w.layout, resolvedHost);
    } catch (error) {
      throw new McpError(
        ErrorCode.InternalError,
        `Failed to apply layout to ${windowTarget}: ${(error as Error).message}`,
      );
    }
  }
}

async function selectWindow(target: string, host?: string) {
  await runTmux(['select-window', '-t', target], host);
}

async function selectPane(target: string, host?: string) {
  await runTmux(['select-pane', '-t', target], host);
}

async function setSyncPanes(target: string, on: boolean, host?: string) {
  await runTmux(['set-window-option', '-t', target, 'synchronize-panes', on ? 'on' : 'off'], host);
}

async function main() {
  const { values } = parseArgs({
    options: {
      'shell-type': { type: 'string', default: 'bash', short: 's' },
      version: { type: 'boolean', default: false, short: 'v' },
    },
  });

  if (values.version) {
    console.log(VERSION);
    return;
  }

  await loadHostProfiles();
  await loadLayoutProfiles();
  await ensureLocalTmuxAvailable();

  const server = new McpServer(
    {
      name: 'mcp-tmux',
      version: VERSION,
      websiteUrl: 'https://github.com/k8ika0s/mcp-tmux',
      title: 'tmux MCP server',
    },
    {
      capabilities: {
        tools: {},
        logging: {},
        resources: {},
        tasks: {
          requests: {
            tools: {
              call: {},
            },
          },
        },
      },
      instructions,
      taskStore: new InMemoryTaskStore(),
      taskMessageQueue: new InMemoryTaskMessageQueue(),
    },
  );

  server.registerResource(
    'tmux.state_resource',
    'tmux://state/default',
    {
      title: 'Default tmux state snapshot',
      description: 'On read, captures current default host/session state and recent output.',
      mimeType: 'text/plain',
    },
    async () => {
      const snapshot = await buildStateSnapshot({ host: defaultHost, session: defaultSession });
      const text = [
        `Host: ${snapshot.host}`,
        `Session: ${snapshot.session}`,
        `Capture target: ${snapshot.captureTarget ?? '(none)'}`,
        '',
        snapshot.sessionsText,
        '',
        snapshot.windowsText,
        '',
        snapshot.panesText,
        '',
        snapshot.capture,
        '',
        defaultTargetNote(),
      ].join('\n');
      return { contents: [{ uri: 'tmux://state/default', text }] };
    },
  );


  const log = async (
    level: 'info' | 'debug' | 'error' | 'notice' | 'warning' | 'critical' | 'alert' | 'emergency',
    data: string,
    sessionId?: string,
  ) => {
    try {
      await server.sendLoggingMessage({ level, data }, sessionId);
    } catch {
      // best effort
    }
  };

  server.registerTool(
    'tmux.set_default',
    {
      title: 'Set default host/session/window/pane',
      description: 'Persist defaults for later tool calls. Omit fields you do not want to change.',
      inputSchema: {
        host: z.string().describe('SSH host alias to remember.').optional(),
        session: z.string().describe('Session name to remember.').optional(),
        window: z.string().describe('Window target to remember.').optional(),
        pane: z.string().describe('Pane target to remember.').optional(),
      },
    },
    async ({ host, session, window, pane }) => {
      if (host !== undefined) defaultHost = host || undefined;
      if (session !== undefined) defaultSession = session || undefined;
      if (window !== undefined) defaultWindow = window || undefined;
      if (pane !== undefined) defaultPane = pane || undefined;
      return { content: [{ type: 'text', text: `Defaults updated:\n${summarizeDefaults()}` }] };
    },
  );

  server.registerTool(
    'tmux.get_default',
    {
      title: 'Show default host/session/window/pane',
      description: 'Display the current remembered defaults.',
    },
    async () => ({ content: [{ type: 'text', text: summarizeDefaults() }] }),
  );

  server.registerTool(
    'tmux.open_session',
    {
      title: 'Ensure/attach remote tmux session',
      description:
        'Given an ssh host alias and session name, ensure the remote tmux session exists (create if missing) and set it as default for subsequent commands.',
      inputSchema: {
        host: z.string().describe('SSH config host alias to connect to.'),
        session: z.string().describe('tmux session name on the remote host.'),
        command: z
          .string()
          .describe('Optional command to start the session with if it needs to be created.')
          .optional(),
      },
    },
    async ({ host, session, command }) => {
      const existed = await ensureSession(host, session, command);
      defaultHost = host;
      defaultSession = session;
      defaultWindow = undefined;
      defaultPane = undefined;
      await log('info', `${existed ? 'reconnected' : 'created'} session ${session} on ${host}`);
      const attachHint = `ssh -t ${host} ${tmuxBinary} attach -t ${session}`;
      const text = existed
        ? `Reconnected to remote session ${session} on ${host}. Attach with: ${attachHint}`
        : `Created remote session ${session} on ${host}. Attach with: ${attachHint}`;
      await appendSessionLog(host, session, `mcp-tmux ${VERSION} ${existed ? 'reconnected' : 'created'} session`);
      return { content: [{ type: 'text', text }] };
    },
  );

  server.registerTool(
    'tmux.default_context',
    {
      title: 'Show default tmux target context',
      description: 'Returns the default session (if any) and a quick layout snapshot.',
    },
    async () => {
      const host = resolveHost();
      const sessions = await listSessions(host);
      const defaultSession = await detectDefaultSession();

      const summary = [
        host ? `Default host: ${host}` : 'Default host: local (no host set)',
        defaultSession ? `Default session: ${defaultSession}` : 'No default session detected.',
        formatSessions(sessions),
        defaultTargetNote(),
      ].join('\n\n');

      return {
        content: [{ type: 'text', text: summary }],
      };
    },
  );

  server.registerTool(
    'tmux.state',
    {
      title: 'Snapshot tmux state',
      description: 'Return sessions, windows, panes, and the last lines of the active/default pane.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        session: z
          .string()
          .describe('Session name (optional). Uses default session if set; required if none set.')
          .optional(),
        captureLines: z
          .number()
          .describe('How many lines of scrollback to include from the capture target (default 200).')
          .optional(),
      },
    },
    async ({ host, session, captureLines }) => {
      const snapshot = await buildStateSnapshot({
        host,
        session,
        captureLines: captureLines ?? 200,
      });
      const text = [
        `Host: ${snapshot.host}`,
        `Session: ${snapshot.session}`,
        `Capture target: ${snapshot.captureTarget ?? '(none)'}`,
        '',
        snapshot.sessionsText,
        '',
        snapshot.windowsText,
        '',
        snapshot.panesText,
        '',
        `Capture (last ${captureLines ?? 200} lines):`,
        snapshot.capture,
        '',
        defaultTargetNote(),
      ].join('\n');
      return { content: [{ type: 'text', text }] };
    },
  );

  server.registerTool(
    'tmux.readonly_state',
    {
      title: 'Snapshot tmux state (readonly)',
      description: 'Readonly variant of tmux.state to retrieve sessions, windows, panes, and recent capture without modifying defaults.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        session: z
          .string()
          .describe('Session name (optional). Uses default session if set; required if none set.')
          .optional(),
        captureLines: z
          .number()
          .describe('How many lines of scrollback to include from the capture target (default 200).')
          .optional(),
      },
    },
    async ({ host, session, captureLines }) => {
      const snapshot = await buildStateSnapshot({
        host,
        session,
        captureLines: captureLines ?? 200,
      });
      const text = [
        `Host: ${snapshot.host}`,
        `Session: ${snapshot.session}`,
        `Capture target: ${snapshot.captureTarget ?? '(none)'}`,
        '',
        snapshot.sessionsText,
        '',
        snapshot.windowsText,
        '',
        snapshot.panesText,
        '',
        `Capture (last ${captureLines ?? 200} lines):`,
        snapshot.capture,
        '',
        defaultTargetNote(),
      ].join('\n');
      return { content: [{ type: 'text', text }] };
    },
  );

  server.registerTool(
    'tmux.context_history',
    {
      title: 'Capture recent tmux history',
      description:
        'Capture recent scrollback from a pane or session to infer context; also extracts recent commands heuristically.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        session: z.string().describe('Session (optional; uses default).').optional(),
        target: z
          .string()
          .describe('Pane target (pane id or session:window.pane). If omitted, uses active panes in session.')
          .optional(),
        lines: z.number().describe('How many lines to capture per pane.').default(800).optional(),
        allPanes: z.boolean().describe('If true, capture all panes in the session.').default(false).optional(),
      },
    },
    async ({ host, session, target, lines = 800, allPanes = false }) => {
      const history = await captureHistory({ host, session, target, lines, allPanes });
      const parts: string[] = [];
      for (const c of history.captures) {
        parts.push(`--- ${c.target} ---`);
        parts.push(c.text);
      }
      parts.push('');
      parts.push('Recent commands (heuristic):');
      parts.push(history.commands.join('\n') || '(none)');
      parts.push('');
      parts.push(defaultTargetNote());
      const sessForLog = session ?? getSessionFromTarget(target);
      await appendSessionLog(resolveHost(host), sessForLog, 'context_history captured');
      await auditLog(resolveHost(host), sessForLog, 'context_history', {
        lines,
        allPanes,
        targets: history.captures.map((c) => c.target),
      });
      return { content: [{ type: 'text', text: parts.join('\n') }] };
    },
  );

  server.registerTool(
    'tmux.quickstart',
    {
      title: 'Quickstart instructions',
      description: 'Returns a concise how-to for using the tmux MCP tools safely.',
    },
    async () => {
      const text = [
        'Playbook:',
        '1) tmux.open_session (host+session)',
        '2) tmux.default_context',
        '3) tmux.list_windows / tmux.list_panes',
        '4) tmux.select_window / tmux.select_pane (optional)',
        '5) tmux.send_keys then tmux.capture_pane (or tmux.tail_pane / tmux.context_history)',
        '6) Use tmux.capture_layout / tmux.save_layout_profile for layouts; tmux.set_sync_panes when needed.',
        '',
        'Safety:',
        '- Destructive commands require confirm=true.',
        '- Prefer helper tools over raw tmux.command.',
        '- Always capture after sending keys; re-list panes/windows to stay in sync.',
        '',
        defaultTargetNote(),
      ].join('\n');
      return { content: [{ type: 'text', text }] };
    },
  );

  server.registerTool(
    'tmux.server_info',
    {
      title: 'Server info and version',
      description: 'Return the running server version and package identifier for verification.',
    },
    async () => {
      const text = [`Package: ${PACKAGE_NAME}`, `Version: ${VERSION}`, `Log dir: ${logBaseDir}`].join('\n');
      return { content: [{ type: 'text', text }] };
    },
  );

  server.registerTool(
    'tmux.set_audit_logging',
    {
      title: 'Set audit logging',
      description: 'Enable or disable verbose audit logging for a host/session (logs commands and outputs).',
      inputSchema: {
        host: z.string().describe('Host alias (optional). Uses default host if set.').optional(),
        session: z.string().describe('Session name (optional). Uses default session if set.').optional(),
        enabled: z.boolean().describe('Set to true to enable audit logging, false to disable.'),
      },
    },
    async ({ host, session, enabled }) => {
      const resolvedSession = session ?? defaultSession;
      if (!resolvedSession) {
        throw new McpError(ErrorCode.InvalidParams, 'session is required when no default session is set');
      }
      setAuditEnabled(host, resolvedSession, enabled);
      await appendSessionLog(resolveHost(host), resolvedSession, `audit_logging ${enabled ? 'enabled' : 'disabled'}`);
      return {
        content: [
          {
            type: 'text',
            text: `Audit logging ${enabled ? 'enabled' : 'disabled'} for session ${resolvedSession}${
              host ? ` on ${host}` : ''
            }.`,
          },
        ],
      };
    },
  );

  server.registerTool(
    'tmux.capture_layout',
    {
      title: 'Capture window layouts',
      description: 'Return window layouts for a session so they can be restored later.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        session: z.string().describe('Session name (optional, defaults to stored session).').optional(),
      },
    },
    async ({ host, session }) => {
      const resolvedSession = requireSession(session);
      const layouts = await captureLayouts(resolvedSession, resolveHost(host));
      const text = layouts
        .map((l) => `${resolvedSession}:${l.index} (${l.id}) ${l.name} layout=${l.layout}`)
        .join('\n');
      return { content: [{ type: 'text', text: text || '(no windows)' }] };
    },
  );

  server.registerTool(
    'tmux.restore_layout',
    {
      title: 'Restore a window layout',
      description: 'Apply a saved layout string to a target window.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Window target (e.g., session:window or window id).'),
        layout: z.string().describe('Layout string obtained from tmux.capture_layout.'),
      },
    },
    async ({ host, target, layout }) => {
      await applyLayout(target, layout, resolveHost(host));
      await log('info', `restored layout on ${target}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Restored layout on ${target}.` }] };
    },
  );

  server.registerTool(
    'tmux.tail_pane',
    {
      title: 'Tail a pane buffer',
      description: 'Poll a pane multiple times to watch output without reissuing commands.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z
          .string()
          .describe('Pane target to tail (pane id or session:window.pane). If omitted, uses default pane if set.')
          .optional(),
        lines: z.number().describe('How many lines per fetch.').default(200).optional(),
        iterations: z.number().describe('How many polling iterations.').default(3).optional(),
        intervalMs: z.number().describe('Delay between polls in milliseconds.').default(1000).optional(),
      },
    },
    async ({ host, target, lines = 200, iterations = 3, intervalMs = 1000 }) => {
      const resolvedTarget = requirePaneTarget(target);
      const tailText = await tailPane({ host, target: resolvedTarget, lines, iterations, intervalMs });
      await appendSessionLog(
        resolveHost(host),
        getSessionFromTarget(resolvedTarget),
        `tail_pane ${resolvedTarget} lines=${lines}`,
      );
      return { content: [{ type: 'text', text: tailText || '(no output)' }] };
    },
  );

  server.experimental.tasks.registerToolTask(
    'tmux.tail_task',
    {
      title: 'Tail a pane (task)',
      description: 'Create a task to poll pane output over time. Client can poll for incremental results.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z
          .string()
          .describe('Pane target to tail (pane id or session:window.pane). If omitted, uses default pane if set.')
          .optional(),
        lines: z.number().describe('How many lines per fetch.').default(200).optional(),
        intervalMs: z.number().describe('Delay between polls in milliseconds.').default(1500).optional(),
        iterations: z.number().describe('How many polling iterations before auto-complete.').default(5).optional(),
      },
      outputSchema: undefined,
    } as any,
    {
      async createTask(
        { host, target, lines = 200, intervalMs = 1500, iterations = 5 }: any,
        { taskStore }: any,
      ) {
        const resolvedTarget = requirePaneTarget(target);
        const task = await taskStore.createTask({});
        (async () => {
          const resolvedHost = resolveHost(host);
          const parts: string[] = [];
          for (let i = 0; i < iterations; i++) {
            const capture = await capturePane(resolvedTarget, -lines, undefined, resolvedHost);
            parts.push(`Iteration ${i + 1}/${iterations}`);
            parts.push(capture || '(empty)');
            if (i < iterations - 1) {
              await new Promise((r) => setTimeout(r, intervalMs));
            }
          }
          const finalCapture = await capturePane(resolvedTarget, -lines, undefined, resolvedHost);
          parts.push('Final:');
          parts.push(finalCapture || '(empty)');
          await taskStore.storeTaskResult(task.taskId, 'completed', {
            content: [{ type: 'text', text: parts.join('\n') }],
          });
        })();
        return { task };
      },
      async getTask(_args: any, { taskId, taskStore }: any) {
        return taskStore.getTask(taskId);
      },
      async getTaskResult(_args: any, { taskId, taskStore }: any) {
        return taskStore.getTaskResult(taskId);
      },
    } as any,
  );

  server.registerTool(
    'tmux.multi_run',
    {
      title: 'Fan-out send and capture',
      description: 'Send the same keys to multiple targets (across hosts) and optionally capture results.',
      inputSchema: {
        targets: z
          .array(
            z.object({
              host: z.string().describe('SSH host alias (optional).').optional(),
              target: z.string().describe('Pane target (pane id or session:window.pane).').optional(),
              captureLines: z.number().describe('Lines to capture for this target.').optional(),
              delayMs: z.number().describe('Delay before capture for this target.').optional(),
            }),
          )
          .nonempty()
          .describe('List of targets to fan-out to.'),
        keys: z.string().describe('Keys/command to send.'),
        enter: z.boolean().describe('Append Enter.').default(true).optional(),
        capture: z.boolean().describe('Capture after sending.').default(true).optional(),
        captureLines: z.number().describe('Default lines to capture (per-target override available).').default(200).optional(),
        delayMs: z.number().describe('Default delay before capture in ms (per-target override available).').default(0).optional(),
        mode: z
          .enum(['send_capture', 'tail', 'pattern'])
          .describe('send_capture (default) captures once, tail polls, pattern checks for regex.')
          .optional(),
        pattern: z.string().describe('Regex pattern when mode=pattern.').optional(),
        patternFlags: z.string().describe('Regex flags (e.g., i)').optional(),
        tailIterations: z.number().describe('Tail iterations (mode=tail).').default(3).optional(),
        tailIntervalMs: z.number().describe('Tail interval ms (mode=tail).').default(1000).optional(),
      },
    },
    async ({
      targets,
      keys,
      enter = true,
      capture = true,
      captureLines = 200,
      delayMs = 0,
      mode = 'send_capture',
      pattern,
      patternFlags,
      tailIterations = 3,
      tailIntervalMs = 1000,
    }) => {
      const text = await fanoutSendCapture({
        targets,
        keys,
        enter,
        capture,
        captureLines,
        delayMs,
        mode,
        pattern,
        patternFlags,
        tailIterations,
        tailIntervalMs,
      });
      return { content: [{ type: 'text', text }] };
    },
  );

  server.experimental.tasks.registerToolTask(
    'tmux.watch_dir_task',
    {
      title: 'Watch a directory for new files',
      description: 'Poll a directory (local or via SSH) and complete when new files appear or after max iterations.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional).').optional(),
        path: z.string().describe('Directory to watch.').default('.').optional(),
        intervalMs: z.number().describe('Polling interval ms.').default(2000).optional(),
        iterations: z.number().describe('Max polling iterations.').default(10).optional(),
      },
      outputSchema: undefined,
    } as any,
    {
      async createTask({ host, path = '.', intervalMs = 2000, iterations = 10 }: any, { taskStore }: any) {
        const task = await taskStore.createTask({});
        (async () => {
          let prev = await listDirSimple(path, host);
          for (let i = 0; i < iterations; i++) {
            await new Promise((r) => setTimeout(r, intervalMs));
            const curr = await listDirSimple(path, host);
            const added = diffNewFiles(prev, curr);
            prev = curr;
            if (added.length) {
              await taskStore.storeTaskResult(task.taskId, 'completed', {
                content: [
                  {
                    type: 'text',
                    text: `New files detected in ${path}${host ? ` on ${host}` : ''}:\n${added.join('\n')}`,
                  },
                ],
              });
              return;
            }
          }
          await taskStore.storeTaskResult(task.taskId, 'completed', {
            content: [
              {
                type: 'text',
                text: `No new files detected in ${path}${host ? ` on ${host}` : ''} after ${iterations} checks.`,
              },
            ],
          });
        })();
        return { task };
      },
      async getTask(_args: any, { taskId, taskStore }: any) {
        return taskStore.getTask(taskId);
      },
      async getTaskResult(_args: any, { taskId, taskStore }: any) {
        return taskStore.getTaskResult(taskId);
      },
    } as any,
  );

  server.experimental.tasks.registerToolTask(
    'tmux.wait_for_pattern_task',
    {
      title: 'Wait for output pattern',
      description: 'Poll a pane for a regex pattern and complete when matched or after iterations.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z
          .string()
          .describe('Pane target to watch (pane id or session:window.pane). If omitted, uses default pane if set.')
          .optional(),
        pattern: z.string().describe('Regex pattern to search for.'),
        flags: z.string().describe('Regex flags (e.g., i)').optional(),
        lines: z.number().describe('Lines per fetch.').default(400).optional(),
        intervalMs: z.number().describe('Delay between polls in milliseconds.').default(1500).optional(),
        iterations: z.number().describe('Max polling iterations.').default(8).optional(),
      },
      outputSchema: undefined,
    } as any,
    {
      async createTask(
        { host, target, pattern, flags, lines = 400, intervalMs = 1500, iterations = 8 }: any,
        { taskStore }: any,
      ) {
        const resolvedTarget = requirePaneTarget(target);
        const task = await taskStore.createTask({});
        (async () => {
          const resolvedHost = resolveHost(host);
          const regex = new RegExp(pattern, flags);
          for (let i = 0; i < iterations; i++) {
            const capture = await capturePane(resolvedTarget, -lines, undefined, resolvedHost);
            if (regex.test(capture)) {
              await taskStore.storeTaskResult(task.taskId, 'completed', {
                content: [{ type: 'text', text: `Pattern matched on iteration ${i + 1}.\n${capture}` }],
              });
              return;
            }
            if (i < iterations - 1) {
              await new Promise((r) => setTimeout(r, intervalMs));
            }
          }
          const finalCapture = await capturePane(resolvedTarget, -lines, undefined, resolvedHost);
          await taskStore.storeTaskResult(task.taskId, 'completed', {
            content: [
              {
                type: 'text',
                text: `Pattern not found after ${iterations} checks.\nLast capture:\n${finalCapture}`,
              },
            ],
          });
        })();
        return { task };
      },
      async getTask(_args: any, { taskId, taskStore }: any) {
        return taskStore.getTask(taskId);
      },
      async getTaskResult(_args: any, { taskId, taskStore }: any) {
        return taskStore.getTaskResult(taskId);
      },
    } as any,
  );


  server.registerTool(
    'tmux.select_window',
    {
      title: 'Focus a window',
      description: 'Select a window so subsequent commands target it.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Window target (session:window or window id).'),
      },
    },
    async ({ host, target }) => {
      await selectWindow(target, resolveHost(host));
      await log('info', `selected window ${target}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Selected window ${target}.` }] };
    },
  );

  server.registerTool(
    'tmux.select_pane',
    {
      title: 'Focus a pane',
      description: 'Select a pane so subsequent commands target it.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Pane target (pane id or session:window.pane).'),
      },
    },
    async ({ host, target }) => {
      await selectPane(target, resolveHost(host));
      await log('info', `selected pane ${target}${host ? ` on ${host}` : ''}`);
      defaultPane = target;
      return { content: [{ type: 'text', text: `Selected pane ${target}.` }] };
    },
  );

  server.registerTool(
    'tmux.set_sync_panes',
    {
      title: 'Toggle synchronize-panes',
      description: 'Enable or disable synchronize-panes for a window.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Window target (session:window or window id).'),
        on: z.boolean().describe('Whether to enable synchronize-panes.'),
      },
    },
    async ({ host, target, on }) => {
      await setSyncPanes(target, on, resolveHost(host));
      await log('info', `sync-panes ${on ? 'on' : 'off'} for ${target}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `sync-panes ${on ? 'enabled' : 'disabled'} on ${target}.` }] };
    },
  );

  server.registerTool(
    'tmux.save_layout_profile',
    {
      title: 'Save a layout profile',
      description: 'Capture layouts for all windows in a session and store them under a profile name.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        session: z.string().describe('Session to capture (optional, uses default session).').optional(),
        name: z.string().describe('Profile name to store.'),
      },
    },
    async ({ host, session, name }) => {
      const resolvedSession = requireSession(session);
      await saveLayoutProfile(name, resolvedSession, resolveHost(host));
      return { content: [{ type: 'text', text: `Saved layout profile '${name}' for session ${resolvedSession}.` }] };
    },
  );

  server.registerTool(
    'tmux.apply_layout_profile',
    {
      title: 'Apply a saved layout profile',
      description:
        'Apply a previously saved layout profile to a session. Windows are matched by index; pane counts must be compatible.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        session: z
          .string()
          .describe('Session to apply to (optional; uses saved session from profile if omitted).')
          .optional(),
        name: z.string().describe('Profile name to load.'),
      },
    },
    async ({ host, session, name }) => {
      await applyLayoutProfile(name, session, resolveHost(host));
      await log('info', `applied layout profile ${name}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Applied layout profile '${name}'.` }] };
    },
  );

  server.registerTool(
    'tmux.health',
    {
      title: 'Health check',
      description: 'Check tmux availability, PATH/bin resolution, and session listing for a host.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
      },
    },
    async ({ host }) => {
      const resolvedHost = resolveHost(host);
      const results: string[] = [];
      try {
        await runTmux(['-V'], resolvedHost);
        results.push('tmux -V: ok');
      } catch (error) {
        throw new McpError(
          ErrorCode.InternalError,
          `tmux not reachable on ${resolvedHost ?? 'local'}: ${(error as Error).message}`,
        );
      }
      try {
        const sessions = await listSessions(resolvedHost);
        results.push(`sessions: ${sessions.length}`);
      } catch (error) {
        results.push(`sessions: failed (${(error as Error).message})`);
      }
      const hostCfg = getHostProfile(resolvedHost);
      results.push(`host profile: ${hostCfg ? JSON.stringify(hostCfg) : 'none'}`);
      return { content: [{ type: 'text', text: results.join('\n') }] };
    },
  );

  server.registerTool(
    'tmux.list_sessions',
    {
      title: 'List tmux sessions',
      description: 'Enumerate sessions with attachment counts and window totals.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). If omitted, uses default host or local.').optional(),
      },
    },
    async ({ host }) => {
      const sessions = await listSessions(resolveHost(host));
      return {
        content: [{ type: 'text', text: formatSessions(sessions) }],
      };
    },
  );

  server.registerTool(
    'tmux.list_windows',
    {
      title: 'List windows',
      description: 'List windows within a session (or all sessions if no target provided).',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Session name or id to list (optional). If omitted, lists all.').optional(),
      },
    },
    async ({ target, host }) => {
      const windows = await listWindows(target, resolveHost(host));
      return {
        content: [{ type: 'text', text: formatWindows(windows) }],
      };
    },
  );

  server.registerTool(
    'tmux.list_panes',
    {
      title: 'List panes',
      description: 'List panes across windows or inside a specific target.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z
          .string()
          .describe('Target (session, window, or pane) to narrow the list. Optional.')
          .optional(),
      },
    },
    async ({ target, host }) => {
      const panes = await listPanes(target, resolveHost(host));
      return {
        content: [{ type: 'text', text: formatPanes(panes) }],
      };
    },
  );

  server.registerTool(
    'tmux.capture_pane',
    {
      title: 'Capture pane output',
      description: 'Read the scrollback of a pane to observe command results.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z
          .string()
          .describe('Pane target (pane id or session:window.pane). If omitted, uses default pane if set.')
          .optional(),
        start: z
          .number()
          .describe('Optional start line offset (e.g. -200 for last 200 lines). Defaults to -200.')
          .optional(),
        end: z.number().describe('Optional end line offset.').optional(),
      },
    },
    async ({ target, start, end, host }) => {
      const resolvedTarget = requirePaneTarget(target);
      const output = await capturePane(resolvedTarget, start, end, resolveHost(host));
      await auditLog(resolveHost(host), getSessionFromTarget(resolvedTarget), 'capture_pane', {
        target: resolvedTarget,
        start,
        end,
        length: output.length,
      });
      return {
        content: [{ type: 'text', text: output || '(empty pane)' }],
      };
    },
  );

  server.registerTool(
    'tmux.batch_capture',
    {
      title: 'Capture multiple panes (batch)',
      description: 'Capture scrollback from multiple panes in parallel (readonly).',
      inputSchema: {
        targets: z
          .array(
            z.object({
              target: z.string().describe('Pane target (pane id or session:window.pane).'),
              host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
              lines: z.number().describe('Lines to capture for this target (default 200).').optional(),
            }),
          )
          .nonempty(),
        defaultLines: z.number().describe('Default lines to capture for targets without lines.').optional(),
      },
    },
    async ({ targets, defaultLines = 200 }) => {
      const results = await Promise.allSettled(
        targets.map(async (t) => {
          const resolvedHost = resolveHost(t.host);
          const output = await capturePane(t.target, -(t.lines ?? defaultLines), undefined, resolvedHost);
          return { target: t.target, host: resolvedHost ?? 'local', output };
        }),
      );

      const lines: string[] = [];
      let ok = 0;
      let fail = 0;
      for (const r of results) {
        if (r.status === 'fulfilled') {
          ok++;
          lines.push(`== ${r.value.host} ${r.value.target} ==`);
          lines.push(r.value.output || '(empty)');
        } else {
          fail++;
          const msg = r.reason instanceof Error ? r.reason.message : String(r.reason);
          lines.push('== error ==');
          lines.push(msg);
        }
      }
      lines.push('');
      lines.push(`Summary: ${ok} succeeded, ${fail} failed`);

      return { content: [{ type: 'text', text: lines.join('\n') }] };
    },
  );

  server.registerTool(
    'tmux.send_keys',
    {
      title: 'Send keys to pane',
      description: 'Send keystrokes to a tmux target, optionally appending Enter.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z
          .string()
          .describe('Pane target (pane id or session:window.pane). If omitted, uses default pane if set.')
          .optional(),
        keys: z
          .string()
          .describe('The text/keys to send. Supports <SPACE>/<ENTER>/<TAB>/<ESC>. Empty + enter=true sends Enter.'),
        enter: z.boolean().describe('Append Enter after the keys.').default(true).optional(),
      },
    },
    async ({ target, keys, enter = true, host }) => {
      const resolvedHost = resolveHost(host);
      const resolvedTarget = requirePaneTarget(target);
      await sendKeys(resolvedTarget, keys, enter, resolvedHost);
      await log('debug', `send-keys to ${resolvedTarget}${resolvedHost ? ` on ${resolvedHost}` : ''}: "${keys}"`);
      await auditLog(resolvedHost, getSessionFromTarget(resolvedTarget), 'send_keys', {
        target: resolvedTarget,
        keys,
        enter,
      });
      await appendSessionLog(
        resolvedHost,
        getSessionFromTarget(resolvedTarget),
        `send-keys "${keys}" enter=${enter}`,
      );
      return {
        content: [{ type: 'text', text: `Sent keys to ${resolvedTarget}${enter ? ' (with Enter)' : ''}.` }],
      };
    },
  );

  server.registerTool(
    'tmux.run_batch',
    {
      title: 'Run a batch of commands in one call',
      description:
        'Execute multiple shell commands sequentially in one tmux pane and capture the output. Uses "&&" by default (fail fast) or ";" when failFast=false. Automatically captures output (paged) and can clean the prompt before writing.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z
          .string()
          .describe('Pane target (pane id or session:window.pane). If omitted, uses default pane if set.')
          .optional(),
        steps: z
          .array(
            z.object({
              command: z.string().describe('Command text to send.'),
              enter: z.boolean().describe('Whether to press Enter after sending this command.').default(true).optional(),
            }),
          )
          .nonempty()
          .describe('List of commands to run sequentially in the same pane, with optional per-step Enter.'),
        failFast: z
          .boolean()
          .describe('Use && between commands (default true). Set false to use ";" so later commands run even if earlier fail.')
          .optional(),
        captureLines: z
          .number()
          .describe('Lines to capture after execution (default 200).')
          .optional(),
        cleanPrompt: z
          .boolean()
          .describe('Send Ctrl+C then Ctrl+U before first write to clear any stray input (bash/zsh friendly). Default true.')
          .default(true)
          .optional(),
      },
    },
    async ({ host, target, steps, failFast = true, captureLines = 200, cleanPrompt = true }) => {
      const resolvedHost = resolveHost(host);
      const resolvedTarget = requirePaneTarget(target);
      const separator = failFast ? '&&' : ';';
      const hasMultiple = steps.length > 1;

      // Clean prompt if requested (bash/zsh friendly: Ctrl+C then Ctrl+U)
      if (cleanPrompt) {
        await sendKeys(resolvedTarget, '', false, resolvedHost);
        await sendKeys(resolvedTarget, '\u0003', false, resolvedHost); // Ctrl+C
        await sendKeys(resolvedTarget, '\u0015', false, resolvedHost); // Ctrl+U (line clear)
      }

      if (failFast && hasMultiple) {
        const joined = steps.map((s) => s.command).join(` ${separator} `);
        await sendKeys(resolvedTarget, joined, true, resolvedHost);
      } else {
        for (const step of steps) {
          await sendKeys(resolvedTarget, step.command, step.enter ?? true, resolvedHost);
        }
      }

      // allow output to flush
      await new Promise((r) => setTimeout(r, 300));
      const capture = await capturePaged(resolvedTarget, resolvedHost, [20, 100, Math.max(captureLines, 400)]);

      const text = [
        `Commands: ${steps.map((s) => s.command).join(' | ')}`,
        `Target: ${resolvedTarget}${resolvedHost ? ` on ${resolvedHost}` : ''}`,
        cleanPrompt ? 'Prompt cleanup: yes (C-c/C-u)' : 'Prompt cleanup: no',
        `Capture: last ${capture.requested} of ${capture.historySize || '?'} lines${capture.moreAvailable ? ' (truncated, request more)' : ''}`,
        '',
        capture.captured || '(no output)',
      ].join('\n');

      return {
        content: [
          {
            type: 'text',
            text,
          },
        ],
      };
    },
  );

  server.registerTool(
    'tmux.new_session',
    {
      title: 'Create a new session',
      description: 'Create a detached tmux session to collaborate in.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        name: z.string().describe('Session name to create.'),
        command: z.string().describe('Optional command to start in the first window.').optional(),
      },
    },
    async ({ name, command, host }) => {
      const resolvedHost = resolveHost(host);
      await createSession(name, command, resolvedHost);
      defaultHost = resolvedHost ?? defaultHost;
      defaultSession = name;
      defaultWindow = undefined;
      defaultPane = undefined;
      await log('info', `created session ${name}${resolvedHost ? ` on ${resolvedHost}` : ''}`);
      return {
        content: [{ type: 'text', text: `Created session ${name}${resolvedHost ? ` on ${resolvedHost}` : ''}.` }],
      };
    },
  );

  server.registerTool(
    'tmux.new_window',
    {
      title: 'Create a new window',
      description: 'Create a window in a target session, useful for side-by-side collaboration.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Target session (or window) to create the window in, e.g. mysession.'),
        name: z.string().describe('Optional window name.').optional(),
        command: z.string().describe('Optional command to run when the window starts.').optional(),
      },
    },
    async ({ target, name, command, host }) => {
      const resolvedHost = resolveHost(host);
      const finalName = await createWindow(target, name, command, resolvedHost);
      defaultWindow = `${target}:${name ?? finalName}`;
      await log(
        'info',
        `created window ${target}:${name ?? finalName}${resolvedHost ? ` on ${resolvedHost}` : ''}`,
      );
      return {
        content: [{ type: 'text', text: `Created window in ${target}${name ? ` named ${name}` : ` named ${finalName}`}.` }],
      };
    },
  );

  server.registerTool(
    'tmux.split_pane',
    {
      title: 'Split a pane',
      description: 'Split a pane horizontally or vertically, optionally running a command.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Pane target (pane id or session:window.pane).'),
        orientation: z
          .enum(['horizontal', 'vertical'])
          .describe('horizontal = side-by-side (-h), vertical = stacked (-v).')
          .default('horizontal'),
        command: z.string().describe('Optional command to run in the new pane.').optional(),
      },
    },
    async ({ target, orientation, command, host }) => {
      const resolvedHost = resolveHost(host);
      const title = `llm-pane-${Date.now().toString(36)}`;
      await splitPane(target, orientation, command, resolvedHost);
      await setPaneTitle(undefined, title, resolvedHost).catch(() => {}); // best effort title on new active pane
      await log('info', `split ${target} (${orientation})${resolvedHost ? ` on ${resolvedHost}` : ''}`);
      return {
        content: [{ type: 'text', text: `Split ${target} (${orientation}).` }],
      };
    },
  );

  server.registerTool(
    'tmux.kill_session',
    {
      title: 'Kill a session',
      description: 'Terminate a tmux session. Use with care.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Session name or id to kill.'),
        confirm: z
          .boolean()
          .describe('Must be true to proceed.')
          .default(false)
          .optional(),
      },
    },
    async ({ target, host, confirm }) => {
      if (!confirm) {
        throw new McpError(ErrorCode.InvalidParams, 'confirm=true is required to kill a session');
      }
      await killSession(target, resolveHost(host));
      await log('warning', `killed session ${target}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Killed session ${target}${host ? ` on ${host}` : ''}.` }] };
    },
  );

  server.registerTool(
    'tmux.kill_window',
    {
      title: 'Kill a window',
      description: 'Close a tmux window. Use with care.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Window target, e.g. session:window or window id.'),
        confirm: z
          .boolean()
          .describe('Must be true to proceed.')
          .default(false)
          .optional(),
      },
    },
    async ({ target, host, confirm }) => {
      if (!confirm) {
        throw new McpError(ErrorCode.InvalidParams, 'confirm=true is required to kill a window');
      }
      await killWindow(target, resolveHost(host));
      await log('warning', `killed window ${target}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Killed window ${target}${host ? ` on ${host}` : ''}.` }] };
    },
  );

  server.registerTool(
    'tmux.kill_pane',
    {
      title: 'Kill a pane',
      description: 'Close a tmux pane. Use with care.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Pane target, e.g. session:window.pane or pane id.'),
        confirm: z
          .boolean()
          .describe('Must be true to proceed.')
          .default(false)
          .optional(),
      },
    },
    async ({ target, host, confirm }) => {
      if (!confirm) {
        throw new McpError(ErrorCode.InvalidParams, 'confirm=true is required to kill a pane');
      }
      await killPane(target, resolveHost(host));
      await log('warning', `killed pane ${target}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Killed pane ${target}${host ? ` on ${host}` : ''}.` }] };
    },
  );

  server.registerTool(
    'tmux.rename_session',
    {
      title: 'Rename a session',
      description: 'Rename a tmux session.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Existing session name or id.'),
        name: z.string().describe('New session name.'),
      },
    },
    async ({ target, name, host }) => {
      await renameSession(target, name, resolveHost(host));
      await log('info', `renamed session ${target} -> ${name}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Renamed session ${target} -> ${name}.` }] };
    },
  );

  server.registerTool(
    'tmux.rename_window',
    {
      title: 'Rename a window',
      description: 'Rename a tmux window.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        target: z.string().describe('Window target, e.g. session:window or window id.'),
        name: z.string().describe('New window name.'),
      },
    },
    async ({ target, name, host }) => {
      await renameWindow(target, name, resolveHost(host));
      await log('info', `renamed window ${target} -> ${name}${host ? ` on ${host}` : ''}`);
      return { content: [{ type: 'text', text: `Renamed window ${target} -> ${name}.` }] };
    },
  );

  server.registerTool(
    'tmux.command',
    {
      title: 'Run arbitrary tmux command',
      description:
        'Execute any tmux subcommand with raw args. Use this when helpers do not cover your use case. You are responsible for correctness and safety.',
      inputSchema: {
        host: z.string().describe('SSH host alias (optional). Uses default host if set.').optional(),
        args: z
          .array(z.string())
          .nonempty()
          .describe('Arguments to pass to tmux (do not include the tmux binary itself).'),
        confirm: z
          .boolean()
          .describe('Set true if the command is destructive (kill*, attach -k, unlink-window, etc).')
          .optional(),
      },
    },
    async ({ args, host, confirm }) => {
      const needsConfirm = isDestructiveTmuxArgs(args);
      if (needsConfirm && !confirm) {
        throw new McpError(
          ErrorCode.InvalidParams,
          'confirm=true is required for destructive tmux.command calls (kill*, unlink, attach -k)',
        );
      }
      const resolvedHost = resolveHost(host);
      const output = await runTmux(args, resolvedHost);
      await log('info', `command: tmux ${args.join(' ')}`);
      await auditLog(resolvedHost, defaultSession, 'tmux.command', {
        args,
        outputLength: output.length,
      });
      return {
        content: [{ type: 'text', text: output || '(no output)' }],
      };
    },
  );

  const transport = new StdioServerTransport();
  await server.connect(transport);
}

main().catch((error) => {
  console.error('mcp-tmux failed to start:', error);
  process.exit(1);
});
