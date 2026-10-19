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
mission = m[1] if m else "Brak aktywnej misji"
cur.execute("SELECT count(*) FROM workflow_cases")
cases = cur.fetchone()[0]
cur.execute("SELECT count(*) FROM experience_rules WHERE state = 'ACTIVE'")
rules = cur.fetchone()[0]
print(f"{mission}:::{cases}:::{rules}")
`;
    const res = runPythonSqlite(pyCode);
    if (res === "NO_DB") {
      return "⚠️ Baza danych Summa42 nie została jeszcze zainicjalizowana.";
    }

    const [missionText, casesCount, rulesCount] = res.split(':::');

    return [
      `🏛️ **Summa42 Collective Status**`,
      `━━━━━━━━━━━━━━━━━━━━━━━━━━━━━`,
      `🎯 **Aktywna Misja:** ${missionText}`,
      `📋 **Śledzone Sprawy (Cases):** ${casesCount}`,
      `🧠 **Zasady Doświadczenia (Active Rules):** ${rulesCount}`,
      ``,
      `🛡️ **Koperty Uprawnień:**`,
      `  • \`github.pr.review\`: \`L1_SUPERVISED_SHADOW\` (lub L2 przy awansie z doświadczenia)`,
      `  • \`github.pr.merge\`: \`L1_SUPERVISED_SHADOW\``,
      `  • \`github.issue.triage\`: \`L1_SUPERVISED_SHADOW\``,
      ``,
      `*Dostępne komendy na kanale:* \`status\`, \`cases\`, \`misja <cel>\`, \`reconcile\``
    ].join('\n');
  } catch (err) {
    return `⚠️ Błąd odczytu stanu Summa42: ${err.message}`;
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
      return "Brak zarejestrowanych spraw w kolejce.";
    }

    const lines = ["📋 **Ostatnie sprawy w workflow Summa42:**", "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"];
    for (const row of res.split('\n')) {
      const [cid, st, obj] = row.split('|');
      lines.push(`• **${obj || cid}**: stan = \`${st}\` (id: \`${cid}\`)`);
    }
    return lines.join('\n');
  } catch (err) {
    return `⚠️ Błąd odczytu spraw: ${err.message}`;
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
lines = ["📥 **Deterministyczny Wykaz Ticketów (0 tokenów LLM, 0 Human Friction):**", "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"]
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
    return `⚠️ Błąd odczytu ticketów: ${err.message}`;
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
    return `✅ **Zarejestrowano nową misję dla Summy:**\n> *"${newStatement}"*\n(ID: \`${mid}\`)`;
  } catch (err) {
    return `⚠️ Błąd aktualizacji misji: ${err.message}`;
  }
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
      `🔄 **Cykl pojednania (Reconcile Tick) zakończony:**`,
      '```',
      proc.stdout.slice(0, 1500) || proc.stderr.slice(0, 1500) || 'Done',
      '```'
    ].join('\n');
  } catch (err) {
    return `⚠️ Błąd uruchomienia reconcile: ${err.message}`;
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

    if (lower === 'status' || lower === '/status' || lower.includes('stan') || lower.includes('status')) {
      reply = getSummaStatus();
    } else if (lower.startsWith('/mission ') || lower.startsWith('misja ')) {
      const stmt = text.replace(/^\/(?:mission|misja)\s+/i, '').trim();
      reply = setMission(stmt);
    } else if (lower === 'cases' || lower === '/cases' || lower.includes('sprawy') || lower.includes('pry')) {
      reply = getCases();
    } else if (lower.includes('ticket') || lower.includes('nowe') || lower === '/tickets') {
      reply = getTicketsDeterministic();
    } else if (lower === '/reconcile' || lower === 'reconcile' || lower === 'sync') {
      reply = runReconcileOnce();
    } else {
      reply = [
        `👋 Cześć! Jestem **Summa42** (Sovereign Multi-Agent Collective).`,
        ``,
        `Oto co możesz mi zlecić przez ten kanał Discorda:`,
        `• \`status\` – raport z aktualnej misji, spraw i uprawnień`,
        `• \`tickets\` – deterministyczny wykaz ticketów z podziałem na typy (0 tokenów)`,
        `• \`cases\` – lista otwartych spraw (PRy, zadania w toku)`,
        `• \`misja <cel>\` – zdefiniowanie nowej misji (np. *wszystkie PRy z @uzytkownik jako reviewer obsłużone ASAP*)`,
        `• \`reconcile\` – natychmiastowe uruchomienie cyklu weryfikacji i review`
      ].join('\n');
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
