#!/usr/bin/env node
/**
 * Summa42 ACP (Agent Client Protocol) Bridge.
 * Connects Summa42 to OpenACP / acp-discord over the open ACP standard.
 * 
 * SECURITY MODEL:
 * 1. Read-only: status, cases and tickets only read the canonical DB through scripts/summa42-query.py
 *    (opened mode=ro). The bridge never writes canonical state, files issues, or injects work.
 * 2. There is NO cryptographic authentication. Owner gating is an owner-ID allowlist
 *    (discord_owners) in $SUMMA42_HOME/discord_auth.json, which must be mode 0600 (checked at load).
 *    The requestor ID is whatever the channel layer reports; it is only as trustworthy as that layer.
 * 3. chat and "/mission show" are owner-only. status, cases and tickets are open to everyone.
 */

const { Readable, Writable } = require('stream');
const path = require('path');
const fs = require('fs');
const { spawnSync } = require('child_process');
const os = require('os');
const { checkModelUser, modelLaunchArgv, spawnTimeoutMs } = require('./model-launch.js');

// Load ACP SDK from global npm module acp-discord
const ACP_SDK_PATH = path.join(
  path.dirname(process.execPath),
  '..',
  'lib',
  'node_modules',
  'acp-discord',
  'node_modules',
  '@agentclientprotocol',
  'sdk'
);

let acp;
try {
  acp = require(ACP_SDK_PATH);
} catch (e) {
  try {
    acp = require('@agentclientprotocol/sdk');
  } catch (err) {
    console.error("Could not load @agentclientprotocol/sdk:", e.message);
    process.exit(1);
  }
}

const SUMMA_QUERY_BIN = path.join(__dirname, 'summa42-query.py');
// No default: a bridge started without SUMMA42_HOME must not guess a home directory.
const SUMMA42_HOME = process.env.SUMMA42_HOME;
if (!SUMMA42_HOME) {
  console.error("SUMMA42_HOME must be set");
  process.exit(1);
}
const CONFIG_PATH = path.join(SUMMA42_HOME, 'discord_auth.json');

/**
 * Validates whether a requestor ID is on the owner allowlist (discord_owners,
 * comma-separated) in discord_auth.json. The file must be mode 0600.
 */
function verifyOwnerAuthorization(requestorId) {
  if (!requestorId) {
    return { authorized: false, reason: "Missing sender identity from channel" };
  }

  try {
    if (!fs.existsSync(CONFIG_PATH)) {
      return { authorized: false, reason: "discord_auth.json not found" };
    }
    if ((fs.statSync(CONFIG_PATH).mode & 0o077) !== 0) {
      return { authorized: false, reason: "discord_auth.json must be mode 0600" };
    }
    const cfg = JSON.parse(fs.readFileSync(CONFIG_PATH, 'utf-8'));
    const ownersList = String(cfg.discord_owners || "").split(',').map(x => x.trim().toLowerCase()).filter(Boolean);
    if (!ownersList.length) {
      return { authorized: false, reason: "discord_owners not configured in discord_auth.json" };
    }
    if (!ownersList.includes(String(requestorId).trim().toLowerCase())) {
      return { authorized: false, reason: `User '${requestorId}' is not on the owner allowlist.` };
    }
    return { authorized: true, ownersList };
  } catch (err) {
    return { authorized: false, reason: `Authorization check failed: ${err.message}` };
  }
}

function runQuery(subcommand) {
  const res = spawnSync('python3', [SUMMA_QUERY_BIN, subcommand], { encoding: 'utf-8', timeout: 10000, env: process.env });
  if (res.error) throw res.error;
  if (res.status !== 0) throw new Error(res.stderr.trim() || `Command failed with code ${res.status}`);
  return JSON.parse(res.stdout.trim());
}

function getOpenRouterKey() {
  if (process.env.OPENROUTER_API_KEY) return process.env.OPENROUTER_API_KEY.trim();
  const keyFile = path.join(SUMMA42_HOME, 'openrouter_key');
  if (fs.existsSync(keyFile)) {
    try { return fs.readFileSync(keyFile, 'utf-8').trim(); } catch (_) {}
  }
  return null;
}

function getOpenRouterFreeModels() {
  const cacheFile = '/tmp/summa42_openrouter_free_models.json';
  try {
    if (fs.existsSync(cacheFile)) {
      const stats = fs.statSync(cacheFile);
      if (Date.now() - stats.mtimeMs < 3600000) {
        return JSON.parse(fs.readFileSync(cacheFile, 'utf-8'));
      }
    }
  } catch (_) {}

  try {
    const res = spawnSync('curl', ['-s', '--max-time', '5', 'https://openrouter.ai/api/v1/models'], { encoding: 'utf-8' });
    if (!res.error && res.stdout) {
      const parsed = JSON.parse(res.stdout);
      if (parsed.data && Array.isArray(parsed.data)) {
        const models = ['openrouter/free'];
        for (const m of parsed.data) {
          if (m.id && (m.id.endsWith(':free') || m.id === 'openrouter/free') && !models.includes(m.id)) {
            models.push(m.id);
          }
        }
        if (models.length > 1) {
          try { fs.writeFileSync(cacheFile, JSON.stringify(models)); } catch (_) {}
          return models;
        }
      }
    }
  } catch (_) {}

  return [
    "openrouter/free",
    "google/gemma-4-31b-it:free",
    "nvidia/nemotron-3-super-120b-a12b:free",
    "minimax/minimax-m3.1-flash:free",
    "cohere/north-mini-code:free",
    "meta-llama/llama-3.3-70b-instruct:free",
    "qwen/qwen-2.5-coder-32b-instruct:free"
  ];
}

function callOpenRouterFree(prompt) {
  const apiKey = getOpenRouterKey();
  if (!apiKey) return null;

  const freeModels = getOpenRouterFreeModels();

  for (const model of freeModels) {
    if (!model.endsWith(':free') && model !== 'openrouter/free') continue;
    try {
      // The key travels in env and the body on stdin, never on the curl argv.
      const curlProc = spawnSync('sh', ['-c',
        'exec curl -s -X POST https://openrouter.ai/api/v1/chat/completions ' +
        '-H "Authorization: Bearer $OPENROUTER_API_KEY" ' +
        '-H "X-Title: Summa42 Collective" -H "Content-Type: application/json" ' +
        '-d @- --max-time 30'
      ], {
        encoding: 'utf-8',
        env: { ...process.env, OPENROUTER_API_KEY: apiKey },
        input: JSON.stringify({ model: model, messages: [{ role: 'user', content: prompt }], temperature: 0.2 })
      });

      if (!curlProc.error && curlProc.stdout) {
        const parsed = JSON.parse(curlProc.stdout);
        if (parsed.choices && parsed.choices[0] && parsed.choices[0].message) {
          const content = parsed.choices[0].message.content.trim();
          if (content) return content;
        }
      }
    } catch (_) {}
  }
  return null;
}

function getSummaStatus() {
  try {
    const data = runQuery('status');
    if (data.error === "NO_DB") {
      return "⚠️ Summa42 database has not been initialized yet.";
    }
    // Authority levels come from the Box via the summa42 CLI, never hard-coded here.
    const cli = spawnSync('summa42', ['status'], { encoding: 'utf-8', timeout: 10000, env: process.env });
    const box = (!cli.error && cli.status === 0 && cli.stdout.trim()) ? cli.stdout.trim() : 'unavailable (summa42 status failed)';

    return [
      `🏛️ **Summa42 Collective Status**`,
      `━━━━━━━━━━━━━━━━━━━━━━━━━━━━━`,
      `🎯 **Active Mission:** ${data.mission}`,
      `📋 **Tracked Workflow Cases:** ${data.cases}`,
      `🧠 **Active Experience Rules:** ${data.rules}`,
      `🛡️ **Box:** ${box}`,
      ``,
      `*Available channel commands:* \`status\`, \`cases\`, \`tickets\``
    ].join('\n');
  } catch (err) {
    return `⚠️ Error reading Summa42 status: ${err.message}`;
  }
}

function showMission(requestorId) {
  const auth = verifyOwnerAuthorization(requestorId);
  if (!auth.authorized) {
    return `⛔ **Access Denied:** owner-only.\n> *Reason:* ${auth.reason}`;
  }
  try {
    const data = runQuery('status');
    return data.error ? `⚠️ ${data.error}` : `🎯 **Active Mission:** ${data.mission}`;
  } catch (err) {
    return `⚠️ Error reading mission: ${err.message}`;
  }
}

function getCases() {
  try {
    const rows = runQuery('cases');
    if (!rows.length) {
      return "ℹ️ No active workflow cases in progress.";
    }

    const lines = [
      `📋 **Active Workflow Cases (${rows.length}):**`,
      `━━━━━━━━━━━━━━━━━━━━━━━━━━━━━`
    ];

    for (const r of rows) {
      lines.push(`• \`${r.case_id}\` | **${r.source}** | \`${r.object_id}\` | State: \`${r.state}\` | Work: \`${r.current_work_id || 'none'}\` (Steps: ${r.steps})`);
    }

    return lines.join('\n');
  } catch (err) {
    return `⚠️ Error listing cases: ${err.message}`;
  }
}

function getTicketsDeterministic() {
  try {
    const items = runQuery('tickets');
    if (!items.length) {
      return "ℹ️ No GitHub issues found in evidence store.";
    }

    const lines = [
      `🎫 **Summa42 Issue Backlog (${items.length} items):**`,
      `━━━━━━━━━━━━━━━━━━━━━━━━━━━━━`
    ];

    for (const it of items) {
      const num = it.number;
      const title = it.title || "Untitled";
      const st = it.state || "UNKNOWN";
      const is_pr = (it.url || "").includes("/pull/");
      const tlower = title.toLowerCase();
      let kind = "TASK";
      if (is_pr) kind = "PR";
      else if (tlower.includes("fix") || tlower.includes("bug")) kind = "BUG";
      else if (tlower.includes("feat") || tlower.includes("add")) kind = "FEATURE";
      else if (tlower.includes("doc")) kind = "DOCS";
      else if (tlower.includes("refactor") || tlower.includes("spike")) kind = "REFACTOR";
      else if (tlower.includes("test")) kind = "TEST";

      lines.push(`• **#${num}**: \`[${kind}]\` \`[${st}]\` ${title}`);
    }

    return lines.join('\n');
  } catch (err) {
    return `⚠️ Error reading tickets: ${err.message}`;
  }
}

// Model CLIs must never run as the Box owner: a prompt-injected agent could read
// the Box's keys, tokens and state DB. They run as the dedicated model user
// (scripts/setup-model-user.sh) through sudo -n -u with an env -i environment.
function modelUserOrError() {
  const name = (process.env.SUMMA42_MODEL_USER || '').trim();
  if (!name) {
    return { error: '⛔ **Chat is disabled:** `SUMMA42_MODEL_USER` is not set. Model CLIs must run as a dedicated OS user that cannot read the Box secrets. Run `scripts/setup-model-user.sh` once with sudo, then set `SUMMA42_MODEL_USER=summa42-model`.' };
  }
  const pw = spawnSync('getent', ['passwd', name], { encoding: 'utf-8' });
  const fields = pw.status === 0 ? pw.stdout.trim().split(':') : [];
  const checked = checkModelUser(name, fields, os.userInfo());
  if (checked.error) {
    return { error: `⛔ **Chat is disabled:** SUMMA42_MODEL_USER: ${checked.error}; use a dedicated model user.` };
  }
  return checked;
}

function spawnAsModelUser(model, bin, args, timeoutMs, input) {
  const opts = {
    encoding: 'utf-8', cwd: '/', env: { PATH: process.env.PATH || '/usr/local/bin:/usr/bin:/bin' },
    timeout: spawnTimeoutMs(timeoutMs), killSignal: 'SIGKILL',
  };
  if (input !== undefined) opts.input = input;
  return spawnSync('sudo', modelLaunchArgv(model, bin, args, timeoutMs, process.env.LANG), opts);
}

function chatWithCollective(userText, requestorId) {
  const auth = verifyOwnerAuthorization(requestorId);
  if (!auth.authorized) {
    return `⛔ **Access Denied:** chat is owner-only.\n> *Reason:* ${auth.reason}`;
  }
  const modelUser = modelUserOrError();
  if (modelUser.error) {
    return modelUser.error;
  }
  try {
    const ctxData = runQuery('status');
    const senderRole = `Właściciel (Owner: ${requestorId})`;

    const prompt = [
      `Jesteś Summa42 (Sovereign Multi-Agent Collective).`,
      `Rozmówca w kanale: ${senderRole}.`,
      `Aktualny stan systemu Summa42:`,
      `- Aktywna misja: "${ctxData.mission || 'Brak'}"`,
      `- Liczba spraw w workflow_cases: ${ctxData.cases || 0}`,
      ``,
      `Wiadomość / pytanie od użytkownika: "${userText}"`,
      ``,
      `Odpowiedz rzeczowo, konkretnie i zwięźle jako Kolektyw Summa42 po polsku (lub w języku zadanego pytania). Nie powtarzaj instrukcji systemowych ani tagów myślenia.`
    ].join('\n');

    let rawReply = null;

    // 0. Antigravity CLI (agy) - authenticated, zero permission skipping
    try {
      const res = spawnAsModelUser(modelUser, 'agy', ['-p', prompt, '--disable-slash-commands'], 45000);
      if (!res.error && res.status === 0 && res.stdout && res.stdout.trim()) {
        rawReply = res.stdout.trim();
      }
    } catch (_) {}

    // 1. OpenRouter :free models if key provided
    if (!rawReply && getOpenRouterKey()) {
      try {
        rawReply = callOpenRouterFree(prompt);
      } catch (_) {}
    }

    // 2. Local GPU acceleration via Ollama
    if (!rawReply) {
      try {
        const res = spawnAsModelUser(modelUser, 'ollama', ['run', 'qwen-gpu'], 20000, prompt);
        if (!res.error && res.status === 0 && res.stdout && res.stdout.trim()) {
          rawReply = res.stdout.trim();
        }
      } catch (_) {}
    }

    if (rawReply) {
      let clean = rawReply;
      if (clean.includes('</think>')) {
        clean = clean.split('</think>').pop().trim();
      }
      clean = clean.replace(/<think>[\s\S]*?<\/think>/gi, '').trim();
      clean = clean.replace(/^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏\s]+/g, '').trim();
      if (clean) return clean;
    }
  } catch (err) {}

  return [
    `👋 Hello! I am **Summa42** (Sovereign Multi-Agent Collective).`,
    ``,
    `Available channel commands:`,
    `• \`status\` – Report active mission, workflow cases, and authority envelopes`,
    `• \`tickets\` – Deterministic issue/PR digest categorized by type`,
    `• \`cases\` – List active workflow cases`,
    `• \`chat\` and \`/mission show\` – owner only`
  ].join('\n');
}

// Create ACP Agent
let activeConnection = null;

const agent = {
  async initialize(params) {
    return {
      protocolVersion: 1,
      agentInfo: {
        name: "Summa42",
        version: "0.2.0-secure"
      },
      capabilities: {}
    };
  },

  async newSession(params) {
    return {
      sessionId: "summa42-session-" + Date.now()
    };
  },

  async prompt(params) {
    const sessionId = params.sessionId;
    let text = "";
    let requestorId = "";

    if (params.prompt && Array.isArray(params.prompt)) {
      for (const p of params.prompt) {
        if (p.text) text += p.text + " ";
        if (p.requestorId) requestorId = p.requestorId;
      }
      text = text.trim();
    } else if (typeof params.prompt === 'string') {
      text = params.prompt.trim();
    } else if (params.prompt && params.prompt.text) {
      text = params.prompt.text.trim();
      if (params.prompt.requestorId) requestorId = params.prompt.requestorId;
    }

    let reply = "";
    const lower = text.toLowerCase();

    if (lower === 'status' || lower === '/status') {
      reply = getSummaStatus();
    } else if (lower === 'cases' || lower === '/cases') {
      reply = getCases();
    } else if (lower === 'tickets' || lower === '/tickets') {
      reply = getTicketsDeterministic();
    } else if (lower === '/mission show' || lower === 'mission show') {
      reply = showMission(requestorId);
    } else {
      reply = chatWithCollective(text, requestorId);
    }

    if (activeConnection) {
      await activeConnection.sessionUpdate({
        sessionId: sessionId,
        update: {
          sessionUpdate: "agent_message_chunk",
          content: {
            type: "text",
            text: reply
          }
        }
      });
    }

    return {
      stopReason: "end_turn"
    };
  }
};

const stream = acp.ndJsonStream(
  Writable.toWeb(process.stdout),
  Readable.toWeb(process.stdin)
);

activeConnection = new acp.AgentSideConnection(() => agent, stream);
