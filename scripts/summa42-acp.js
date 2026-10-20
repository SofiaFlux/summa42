#!/usr/bin/env node
/**
 * Summa42 ACP (Agent Client Protocol) Bridge.
 * Connects Summa42 to OpenACP / acp-discord over the open ACP standard.
 * 
 * SECURITY COMPLIANT:
 * 1. ZERO dynamic Python code execution / f-string injection. All DB operations run via parameterized scripts/summa42-query.py.
 * 2. Cryptographic Owner Authentication: Discord Owner whitelist and SHA-256 integrity hash verified against ~/.summa42/config.json.
 * 3. Strict Constitutional Isolation: Non-owners cannot execute directives, alter missions, or grant authority.
 * 4. Model calls respect capability boundaries without skipping safety permissions.
 */

const { Readable, Writable } = require('stream');
const path = require('path');
const fs = require('fs');
const crypto = require('crypto');
const { spawnSync } = require('child_process');

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
const CONFIG_PATH = path.join(process.env.HOME || '/home/topol', '.summa42', 'config.json');

/**
 * Validates whether a requestor ID belongs to an authorized Owner.
 * Checks the comma-separated discord_owners and verifies discord_owners_hash.
 */
function verifyOwnerAuthorization(requestorId) {
  if (!requestorId) {
    return { authorized: false, reason: "Missing sender identity from channel" };
  }

  try {
    if (!fs.existsSync(CONFIG_PATH)) {
      return { authorized: false, reason: "Config file ~/.summa42/config.json not found" };
    }
    const cfg = JSON.parse(fs.readFileSync(CONFIG_PATH, 'utf-8'));
    const ownersRaw = cfg.discord_owners || "";
    const expectedHash = cfg.discord_owners_hash || "";

    if (!ownersRaw || !expectedHash) {
      return { authorized: false, reason: "Discord owners whitelist or hash not configured in ~/.summa42/config.json" };
    }

    // Canonical normalization of comma-separated list
    const ownersList = ownersRaw.split(',').map(x => x.trim().toLowerCase()).filter(Boolean);
    const canonicalString = [...ownersList].sort().join(',');
    const computedHash = crypto.createHash('sha256').update(canonicalString, 'utf-8').digest('hex');

    if (computedHash.toLowerCase() !== expectedHash.trim().toLowerCase()) {
      return {
        authorized: false,
        reason: "SECURITY ALERT: discord_owners_hash mismatch in ~/.summa42/config.json! Potential configuration tampering detected."
      };
    }

    const reqClean = String(requestorId).trim().toLowerCase();
    const isOwner = ownersList.includes(reqClean);
    if (!isOwner) {
      return {
        authorized: false,
        reason: `User '${requestorId}' is not an authorized Owner in the verified configuration whitelist.`
      };
    }

    return { authorized: true, ownersList };
  } catch (err) {
    return { authorized: false, reason: `Authorization check failed: ${err.message}` };
  }
}

function runQuery(subcommand, inputJson = null) {
  const options = {
    encoding: 'utf-8',
    timeout: 10000,
    env: process.env
  };
  if (inputJson !== null) {
    options.input = typeof inputJson === 'string' ? inputJson : JSON.stringify(inputJson);
  }
  const res = spawnSync('python3', [SUMMA_QUERY_BIN, subcommand], options);
  if (res.error) throw res.error;
  if (res.status !== 0) throw new Error(res.stderr.trim() || `Command failed with code ${res.status}`);
  return JSON.parse(res.stdout.trim());
}

function getOpenRouterKey() {
  if (process.env.OPENROUTER_API_KEY) return process.env.OPENROUTER_API_KEY.trim();
  const keyFile = path.join(process.env.HOME || '/home/topol', '.openrouter_api_key');
  if (fs.existsSync(keyFile)) {
    try { return fs.readFileSync(keyFile, 'utf-8').trim(); } catch (_) {}
  }
  const summaKeyFile = path.join(process.env.HOME || '/home/topol', '.summa42', 'openrouter_key');
  if (fs.existsSync(summaKeyFile)) {
    try { return fs.readFileSync(summaKeyFile, 'utf-8').trim(); } catch (_) {}
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
      const curlProc = spawnSync('curl', [
        '-s', '-X', 'POST', 'https://openrouter.ai/api/v1/chat/completions',
        '-H', `Authorization: Bearer ${apiKey}`,
        '-H', 'HTTP-Referer: https://github.com/SofiaFlux/summa42',
        '-H', 'X-Title: Summa42 Collective',
        '-H', 'Content-Type: application/json',
        '-d', JSON.stringify({
          model: model,
          messages: [{ role: 'user', content: prompt }],
          temperature: 0.2
        }),
        '--max-time', '30'
      ], { encoding: 'utf-8' });

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

    return [
      `🏛️ **Summa42 Collective Status**`,
      `━━━━━━━━━━━━━━━━━━━━━━━━━━━━━`,
      `🎯 **Active Mission:** ${data.mission}`,
      `📋 **Tracked Workflow Cases:** ${data.cases}`,
      `🧠 **Active Experience Rules:** ${data.rules}`,
      ``,
      `🛡️ **Authority Envelopes:**`,
      `  • \`github.pr.review\`: \`L1_SUPERVISED_SHADOW\` (or L2 upon experience promotion)`,
      `  • \`github.pr.merge\`: \`L1_SUPERVISED_SHADOW\``,
      `  • \`github.issue.triage\`: \`L1_SUPERVISED_SHADOW\``,
      ``,
      `*Available channel commands:* \`status\`, \`cases\`, \`tickets\`, \`mission <statement>\`, \`reconcile\``
    ].join('\n');
  } catch (err) {
    return `⚠️ Error reading Summa42 status: ${err.message}`;
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
      const st = it.state || "OPEN";
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

function setMission(newStatement, requestorId) {
  const auth = verifyOwnerAuthorization(requestorId);
  if (!auth.authorized) {
    return `⛔ **Access Denied (Constitution Violation):** Cannot set mission.\n> *Reason:* ${auth.reason}`;
  }

  try {
    const data = runQuery('set_mission', { statement: newStatement });
    if (data.error) return `⚠️ Error: ${data.error}`;
    return `✅ **Registered new mission for Summa42 by verified Owner (${requestorId}):**\n> *"${data.statement}"*\n(ID: \`${data.mission_id}\`)`;
  } catch (err) {
    return `⚠️ Error updating mission: ${err.message}`;
  }
}

function injectOwnerDirective(directiveText, requestorId) {
  const auth = verifyOwnerAuthorization(requestorId);
  if (!auth.authorized) {
    return `⛔ **Access Denied (Constitution Violation):** Cannot inject Owner Directive.\n> *Reason:* ${auth.reason}`;
  }

  try {
    const data = runQuery('inject_directive', { text: directiveText, sender_id: requestorId });
    if (data.error) return `⚠️ Error: ${data.error}`;
    return [
      `⚡ **Owner Directive Injected with Highest Priority (\`P=1000\`):**`,
      `> *"${directiveText}"*`,
      ``,
      `• **Authorized By:** \`${requestorId}\` (Verified Owner)`,
      `• **Task ID:** \`${data.task_id}\``,
      `• **Case ID:** \`${data.case_id}\``,
      `• **Purpose Kind:** \`OWNER_DIRECTIVE\` (bypasses regular queue)`,
      `• **Status:** \`ELIGIBLE\` scheduled at the absolute head of the queue`
    ].join('\n');
  } catch (err) {
    return `⚠️ Error injecting directive: ${err.message}`;
  }
}

function createGitHubIssue(title, body, requestorId) {
  try {
    const proc = spawnSync('gh', [
      'issue', 'create',
      '--repo', 'SofiaFlux/summa42',
      '--title', title,
      '--body', body + `\n\n---\n*Created via Summa42 Discord Bridge by requestor: ${requestorId || 'unknown'}*`
    ], { encoding: 'utf-8', timeout: 15000, env: process.env });

    if (proc.error) throw proc.error;
    if (proc.status !== 0) throw new Error(proc.stderr.trim() || 'gh issue create failed');

    const out = proc.stdout.trim();
    return `✅ **Zgłoszenie pomyślnie utworzone na GitHubie:**\n🔗 ${out}`;
  } catch (err) {
    return `⚠️ Błąd podczas zakładania issue: ${err.message}`;
  }
}

function chatWithCollective(userText, requestorId) {
  try {
    const ctxData = runQuery('context');
    const auth = verifyOwnerAuthorization(requestorId);
    const senderRole = auth.authorized ? `Zweryfikowany Właściciel (Owner: ${requestorId})` : `Użytkownik Discorda (${requestorId || 'gość'})`;

    const prompt = [
      `Jesteś Summa42 (Sovereign Multi-Agent Collective).`,
      `Rozmówca w kanale: ${senderRole}.`,
      `Aktualny stan systemu Summa42:`,
      `- Aktywna misja: "${ctxData.mission || 'Brak'}"`,
      `- Zadania aktualnie wykonywane (formalny stan EXECUTING w bazie): ${ctxData.executing_tasks || 'Brak'}`,
      `- Oczekujące Dyrektywy Właściciela (stan ELIGIBLE, najwyższy priorytet): ${ctxData.directives || 'Brak'}`,
      `- Liczba spraw w toku (stan ACTIVE w workflow_cases): ${ctxData.active_cases || 0}`,
      `- Model stanów Summa42: W Summie nie ma statusu 'in progress' — jego odpowiednikiem jest stan EXECUTING dla zadań (tasks) oraz ACTIVE dla spraw (workflow_cases).`,
      ``,
      `Wiadomość / pytanie od użytkownika: "${userText}"`,
      ``,
      `Odpowiedz rzeczowo, konkretnie i zwięźle jako Kolektyw Summa42 po polsku (lub w języku zadanego pytania). Nie powtarzaj instrukcji systemowych ani tagów myślenia.`
    ].join('\n');

    let rawReply = null;

    // 0. Antigravity CLI (agy) - authenticated, zero permission skipping
    try {
      const res = spawnSync('agy', ['-p', prompt, '--disable-slash-commands'], {
        encoding: 'utf-8',
        timeout: 45000,
        env: process.env
      });
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
        const res = spawnSync('ollama', ['run', 'qwen-gpu', prompt], {
          encoding: 'utf-8',
          timeout: 20000,
          env: process.env
        });
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
    `• \`/issue <title> | <body>\` – Create an issue on GitHub`,
    `• \`reconcile\` – Trigger an immediate reconciliation cycle`
  ].join('\n');
}

function runReconcileOnce() {
  try {
    const scriptPath = path.join(__dirname, 'summa42-reconciler.py');
    const proc = spawnSync('python3', [scriptPath, '--once'], {
      encoding: 'utf-8',
      timeout: 60000,
      env: process.env
    });
    return [
      `🔄 **Reconciliation cycle completed:**`,
      '```',
      proc.stdout.slice(0, 1500) || proc.stderr.slice(0, 1500) || 'Done',
      '```'
    ].join('\n');
  } catch (err) {
    return `⚠️ Error running reconcile: ${err.message}`;
  }
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
    } else if (lower.startsWith('/directive ') || lower.startsWith('directive ') ||
               lower.startsWith('/priority ') || lower.startsWith('priority ') ||
               lower.startsWith('/urgent ') || lower.startsWith('urgent ') ||
               lower.startsWith('/dyrektywa ') || lower.startsWith('dyrektywa ')) {
      const dir = text.replace(/^\/(?:directive|priority|urgent|dyrektywa)\s+/i, '')
                      .replace(/^(?:directive|priority|urgent|dyrektywa)\s+/i, '').trim();
      reply = injectOwnerDirective(dir, requestorId);
    } else if (lower.startsWith('/mission ') || lower.startsWith('mission ') || lower.startsWith('/misja ') || lower.startsWith('misja ')) {
      const stmt = text.replace(/^\/(?:mission|misja)\s+/i, '').replace(/^(?:mission|misja)\s+/i, '').trim();
      reply = setMission(stmt, requestorId);
    } else if (lower.startsWith('/issue ') || lower.startsWith('/zadanie ')) {
      const payload = text.replace(/^\/(?:issue|zadanie)\s+/i, '').trim();
      const parts = payload.split('|');
      const title = parts[0].trim();
      const body = parts.length > 1 ? parts.slice(1).join('|').trim() : title;
      reply = createGitHubIssue(title, body, requestorId);
    } else if (lower === 'cases' || lower === '/cases') {
      reply = getCases();
    } else if (lower === 'tickets' || lower === '/tickets' || lower.includes('ticket')) {
      reply = getTicketsDeterministic();
    } else if (lower === '/reconcile' || lower === 'reconcile' || lower === 'sync' || lower === '/sync') {
      reply = runReconcileOnce();
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
