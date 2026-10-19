#!/usr/bin/env node
/**
 * Summa42 ACP (Agent Client Protocol) Bridge.
 * Connects Summa42 to OpenACP / acp-discord over the open ACP standard.
 */

const { Readable, Writable } = require('stream');
const path = require('path');
const fs = require('fs');
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
  // Try local require
  try {
    acp = require('@agentclientprotocol/sdk');
  } catch (err) {
    console.error("Could not load @agentclientprotocol/sdk:", e.message);
    process.exit(1);
  }
}

const DB_PATH = process.env.SUMMA42_DB || path.expanduser ? path.expanduser('~/.summa42/state/summa42.db') : path.join(process.env.HOME || '/home/topol', '.summa42', 'state', 'summa42.db');

function getOpenRouterKey() {
  if (process.env.OPENROUTER_API_KEY) return process.env.OPENROUTER_API_KEY.trim();
  const keyFile = path.join(process.env.HOME || '/home/topol', '.openrouter_api_key');
  if (fs.existsSync(keyFile)) {
    try { return fs.readFileSync(keyFile, 'utf-8').trim(); } catch (_) {}
  }
  const tmpFile = '/tmp/summa42_openrouter_key';
  if (fs.existsSync(tmpFile)) {
    try { return fs.readFileSync(tmpFile, 'utf-8').trim(); } catch (_) {}
  }
  return null;
}

function callOpenRouterFree(prompt) {
  const apiKey = getOpenRouterKey();
  if (!apiKey) return null;

  const freeModels = [
    "meta-llama/llama-3.3-70b-instruct:free",
    "qwen/qwen-2.5-coder-32b-instruct:free",
    "google/gemini-2.0-flash-exp:free",
    "deepseek/deepseek-r1:free",
    "mistralai/mistral-small-24b-instruct-2501:free"
  ];

  for (const model of freeModels) {
    if (!model.endsWith(':free')) continue; // Strict safety check
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

function runPythonSqlite(code) {
  const res = spawnSync('python3', ['-c', code], { encoding: 'utf-8', timeout: 5000 });
  if (res.error) throw res.error;
  if (res.status !== 0) throw new Error(res.stderr.trim());
  return res.stdout.trim();
}

function getSummaStatus() {
  try {
    const pyCode = `
import sqlite3, os
db_path = "${DB_PATH}"
if not os.path.exists(db_path):
    print("NO_DB")
    exit(0)
conn = sqlite3.connect(db_path)
cur = conn.cursor()
cur.execute("SELECT mission_id, statement FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
m = cur.fetchone()
mission = m[1] if m else "No active mission"
cur.execute("SELECT count(*) FROM workflow_cases")
cases = cur.fetchone()[0]
cur.execute("SELECT count(*) FROM experience_rules WHERE state = 'ACTIVE'")
rules = cur.fetchone()[0]
print(f"{mission}:::{cases}:::{rules}")
`;
    const res = runPythonSqlite(pyCode);
    if (res === "NO_DB") {
      return "⚠️ Summa42 database has not been initialized yet.";
    }

    const [missionText, casesCount, rulesCount] = res.split(':::');

    return [
      `🏛️ **Summa42 Collective Status**`,
      `━━━━━━━━━━━━━━━━━━━━━━━━━━━━━`,
      `🎯 **Active Mission:** ${missionText}`,
      `📋 **Tracked Workflow Cases:** ${casesCount}`,
      `🧠 **Active Experience Rules:** ${rulesCount}`,
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
    const pyCode = `
import sqlite3, os
db_path = "${DB_PATH}"
if not os.path.exists(db_path):
    exit(0)
conn = sqlite3.connect(db_path)
cur = conn.cursor()
cur.execute("SELECT case_id, state, object_id FROM workflow_cases ORDER BY updated_at DESC LIMIT 10")
for cid, st, obj in cur.fetchall():
    print(f"{cid}|{st}|{obj}")
`;
    const res = runPythonSqlite(pyCode);
    if (!res) {
      return "No tracked workflow cases found in queue.";
    }

    const lines = ["📋 **Recent workflow cases in Summa42:**", "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"];
    for (const row of res.split('\n')) {
      const [cid, st, obj] = row.split('|');
      lines.push(`• **${obj || cid}**: state = \`${st}\` (id: \`${cid}\`)`);
    }
    return lines.join('\n');
  } catch (err) {
    return `⚠️ Error reading cases: ${err.message}`;
  }
}

function getTicketsDeterministic() {
  try {
    const pyCode = `
import subprocess, json, os
token = None
if os.path.exists("/tmp/summa42_github_token"):
    with open("/tmp/summa42_github_token") as f:
        token = f.read().strip()
env = os.environ.copy()
if token:
    env["GITHUB_TOKEN"] = token
proc = subprocess.run([
    "gh", "issue", "list",
    "--repo", "SofiaFlux/summa42",
    "--state", "all",
    "--limit", "15",
    "--json", "number,title,state,labels,url"
], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env, timeout=10)
if proc.returncode != 0:
    print("ERR:" + proc.stderr.strip())
    exit(0)
items = json.loads(proc.stdout)
lines = ["📥 **Deterministic Ticket Digest (0 LLM tokens, 0 human friction):**", "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"]
for it in items:
    num = it.get("number")
    title = it.get("title", "")
    st = it.get("state", "OPEN")
    url = it.get("url", "")
    is_pr = "/pull/" in url
    tlower = title.lower()
    if is_pr:
        kind = "PR"
    elif "fix" in tlower or "bug" in tlower:
        kind = "BUG"
    elif "feat" in tlower or "add" in tlower:
        kind = "FEATURE"
    elif "doc" in tlower:
        kind = "DOCS"
    elif "refactor" in tlower or "spike" in tlower:
        kind = "REFACTOR"
    elif "test" in tlower:
        kind = "TEST"
    else:
        kind = "TASK"
    lines.append(f"• **#{num}**: \`[{kind}]\` \`[{st}]\` {title}")
print("\\n".join(lines))
`;
    return runPythonSqlite(pyCode);
  } catch (err) {
    return `⚠️ Error reading tickets: ${err.message}`;
  }
}

function setMission(newStatement) {
  try {
    const cleanStmt = newStatement.replace(/"/g, '\\"');
    const pyCode = `
import sqlite3, time, random
db_path = "${DB_PATH}"
conn = sqlite3.connect(db_path)
cur = conn.cursor()
cur.execute("UPDATE missions SET active = 0 WHERE active = 1")
mid = "mission_" + "".join(random.choices("0123456789abcdef", k=8))
now = time.strftime("%Y-%m-%d %H:%M:%S")
cur.execute("INSERT INTO missions(mission_id, statement, active, created_at) VALUES (?, ?, 1, ?)", (mid, """${cleanStmt}""", now))
conn.commit()
print(mid)
`;
    const mid = runPythonSqlite(pyCode);
    return `✅ **Registered new mission for Summa42:**\n> *"${newStatement}"*\n(ID: \`${mid}\`)`;
  } catch (err) {
    return `⚠️ Error updating mission: ${err.message}`;
  }
}

function injectOwnerDirective(directiveText) {
  try {
    const cleanDir = directiveText.replace(/"/g, '\\"');
    const pyCode = `
import sqlite3, time, hashlib, json
db_path = "${DB_PATH}"
conn = sqlite3.connect(db_path)
cur = conn.cursor()
tid = "task_dir_" + hashlib.md5(f"""${cleanDir}{time.time()}""".encode()).hexdigest()[:12]
now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

# Insert highest priority task as OWNER_DIRECTIVE (priority = 1000)
cur.execute("""
INSERT INTO tasks(
    task_id, purpose_kind, purpose_id, state, current_fence,
    acceptance_criteria_json, required_capabilities_json, required_enforcement,
    authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at,
    task_class, objective, payload_json
) VALUES (?, 'OWNER_DIRECTIVE', 'owner_discord', 'ELIGIBLE', 0,
    '["Implemented in codebase","Tests pass"]',
    '["workspace.write","execution.run"]',
    'UNENFORCED',
    '["workspace.write","execution.run","github.pr.create"]',
    'envelope_1', 1000, ?, ?,
    'repo.feature.gateway', ?, '{}'
)
""", (tid, now, now, """${cleanDir}"""))

# Also register matching workflow case in active state
cid = "case_dir_" + hashlib.md5(tid.encode()).hexdigest()[:12]
cur.execute("SELECT mission_id FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
m_row = cur.fetchone()
mid = m_row[0] if m_row else "mission_owner"
cur.execute("""
INSERT INTO workflow_cases(
    case_id, mission_id, source, object_id, revision_id,
    observation_evidence_id, initial_request_json, grant_json,
    state, current_work_id, next_work_json, completed_steps,
    max_steps, remaining_budget, progress_signature, created_at, updated_at
) VALUES (?, ?, 'discord_owner', ?, 'v1',
    '', ?, ?,
    'ACTIVE', ?, '{}', 0, 5, 5, 'ready', ?, ?
)
""", (cid, mid, "owner_directive:" + tid, json.dumps({"directive": tid}), json.dumps({"priority": 1000}), "work_" + tid, now, now))

conn.commit()
print(tid)
`;
    const tid = runPythonSqlite(pyCode);
    return [
      `⚡ **Owner Directive Injected with Highest Priority (\`P=1000\`):**`,
      `> *"${directiveText}"*`,
      ``,
      `• **Task ID:** \`${tid}\``,
      `• **Purpose Kind:** \`OWNER_DIRECTIVE\` (bypasses regular queue)`,
      `• **Status:** \`ELIGIBLE\` scheduled at the absolute head of the queue`
    ].join('\n');
  } catch (err) {
    return `⚠️ Error injecting owner directive: ${err.message}`;
  }
}

function chatWithCollective(userText) {
  try {
    const pyCode = `
import sqlite3, os
db_path = "${DB_PATH}"
mission = "None"
executing = []
eligible_directives = []
cases_count = 0
if os.path.exists(db_path):
    conn = sqlite3.connect(db_path)
    cur = conn.cursor()
    cur.execute("SELECT statement FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1")
    m = cur.fetchone()
    if m: mission = m[0]
    cur.execute("SELECT task_id, objective, priority FROM tasks WHERE state = 'EXECUTING' ORDER BY priority DESC LIMIT 5")
    executing = [f"{r[1]} (id: {r[0]})" for r in cur.fetchall()]
    cur.execute("SELECT task_id, objective, priority FROM tasks WHERE purpose_kind = 'OWNER_DIRECTIVE' AND state = 'ELIGIBLE' ORDER BY priority DESC LIMIT 3")
    eligible_directives = [f"{r[1]} (priorytet: {r[2]})" for r in cur.fetchall()]
    cur.execute("SELECT count(*) FROM workflow_cases WHERE state = 'ACTIVE'")
    cases_count = cur.fetchone()[0]

print(f"{mission}:::{' | '.join(executing) if executing else 'Brak'}:::{' | '.join(eligible_directives) if eligible_directives else 'Brak'}:::{cases_count}")
`;
    let contextStr = "";
    try {
      contextStr = runPythonSqlite(pyCode);
    } catch (_) {}

    const [mission, executingTasks, directives, activeCases] = (contextStr || "").split(":::");

    const prompt = [
      `Jesteś Summa42 (Sovereign Multi-Agent Collective). Rozmawiasz z Właścicielem (Ownerem) na Discordzie.`,
      `Aktualny stan systemu Summa42:`,
      `- Aktywna misja: "${mission || 'Brak'}"`,
      `- Zadania aktualnie wykonywane (formalny stan EXECUTING w bazie): ${executingTasks || 'Brak'}`,
      `- Oczekujące Dyrektywy Właściciela (stan ELIGIBLE, najwyższy priorytet): ${directives || 'Brak'}`,
      `- Liczba spraw w toku (stan ACTIVE w workflow_cases): ${activeCases || 0}`,
      `- Model stanów Summa42: W Summie nie ma statusu 'in progress' — jego odpowiednikiem jest stan EXECUTING dla zadań (tasks) oraz ACTIVE dla spraw (workflow_cases).`,
      ``,
      `Wiadomość / pytanie od użytkownika: "${userText}"`,
      ``,
      `Odpowiedz rzeczowo, konkretnie i zwięźle jako Kolektyw Summa42 po polsku (lub w języku zadanego pytania). Nie powtarzaj instrukcji systemowych ani tagów myślenia.`
    ].join('\n');

    let rawReply = null;

    // 0. Antigravity CLI (agy) - native, high capability, authenticated, zero configuration
    try {
      const res = spawnSync('agy', ['-p', prompt, '--dangerously-skip-permissions'], {
        encoding: 'utf-8',
        timeout: 30000,
        env: process.env
      });
      if (!res.error && res.status === 0 && res.stdout && res.stdout.trim()) {
        rawReply = res.stdout.trim();
      }
    } catch (_) {}

    // 1. OpenRouter :free models (Llama 3.3 70B, Qwen 2.5 Coder 32B, Gemini 2.0 Flash) if key provided
    if (!rawReply && getOpenRouterKey()) {
      try {
        rawReply = callOpenRouterFree(prompt);
      } catch (_) {}
    }

    // 1. Local GPU acceleration via Ollama (fastest, private, 0 external API/rate limits)
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

    // 2. Local CPU/GPU fallback via Ollama qwen2.5:3b
    if (!rawReply) {
      try {
        const res = spawnSync('ollama', ['run', 'qwen2.5:3b', prompt], {
          encoding: 'utf-8',
          timeout: 25000,
          env: process.env
        });
        if (!res.error && res.status === 0 && res.stdout && res.stdout.trim()) {
          rawReply = res.stdout.trim();
        }
      } catch (_) {}
    }

    // 3. Cloud/CLI fallback via OpenCode
    if (!rawReply) {
      try {
        const res = spawnSync('opencode', ['run', '--pure', prompt], {
          encoding: 'utf-8',
          timeout: 35000,
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
      // Remove Ollama terminal progress spinners if captured in output
      clean = clean.replace(/^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏\s]+/g, '').trim();
      if (clean) {
        return clean;
      }
    }
  } catch (err) {
    // fallback gracefully
  }

  return [
    `👋 Hello! I am **Summa42** (Sovereign Multi-Agent Collective).`,
    ``,
    `Available channel commands:`,
    `• \`status\` – Report active mission, workflow cases, and authority envelopes`,
    `• \`tickets\` – Deterministic issue/PR digest categorized by type (0 tokens)`,
    `• \`cases\` – List active workflow cases (PRs, issues in progress)`,
    `• \`priority <instruction>\` (or \`directive <instruction>\`) – Inject an OWNER_DIRECTIVE with highest priority (P=1000)`,
    `• \`mission <statement>\` – Set a new mission goal (e.g. *Resolve all open issues to advance Summa42*)`,
    `• \`reconcile\` – Trigger an immediate reconciliation, verification, and review cycle`
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
      `🔄 **Reconciliation cycle (Reconcile Tick) completed:**`,
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
        version: "0.1.0"
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
    if (params.prompt && Array.isArray(params.prompt)) {
      text = params.prompt.map(p => p.text || (typeof p === 'string' ? p : '')).join(' ').trim();
    } else if (typeof params.prompt === 'string') {
      text = params.prompt.trim();
    } else if (params.prompt && params.prompt.text) {
      text = params.prompt.text.trim();
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
      reply = injectOwnerDirective(dir);
    } else if (lower.startsWith('/mission ') || lower.startsWith('mission ') || lower.startsWith('/misja ') || lower.startsWith('misja ')) {
      const stmt = text.replace(/^\/(?:mission|misja)\s+/i, '').replace(/^(?:mission|misja)\s+/i, '').trim();
      reply = setMission(stmt);
    } else if (lower === 'cases' || lower === '/cases') {
      reply = getCases();
    } else if (lower === 'tickets' || lower === '/tickets' || lower.includes('ticket')) {
      reply = getTicketsDeterministic();
    } else if (lower === '/reconcile' || lower === 'reconcile' || lower === 'sync' || lower === '/sync') {
      reply = runReconcileOnce();
    } else {
      reply = chatWithCollective(text);
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
