// Pilot evaluator sidecar — uses Anthropic SDK directly for fast evaluations.
// ~2s per call vs 7-18s with the Agent SDK.

import Anthropic from "@anthropic-ai/sdk";
import { createServer } from "http";
import { readFileSync } from "fs";
import { resolve, dirname } from "path";
import { fileURLToPath } from "url";
import { homedir } from "os";

const __dirname = dirname(fileURLToPath(import.meta.url));

// Load .env from multiple locations
for (const envPath of [
  resolve(homedir(), ".pilot", ".env"),
  resolve(__dirname, ".env"),
  resolve(__dirname, "..", ".env"),
]) {
  try {
    const env = readFileSync(envPath, "utf8");
    for (const line of env.split("\n")) {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith("#")) continue;
      const eqIdx = trimmed.indexOf("=");
      if (eqIdx === -1) continue;
      const key = trimmed.slice(0, eqIdx);
      const val = trimmed.slice(eqIdx + 1);
      if (!process.env[key]) process.env[key] = val;
    }
  } catch {}
}

const PORT = parseInt(process.env.PILOT_EVALUATOR_PORT || "9722", 10);
const client = new Anthropic();

const APPROVAL_SCHEMA = {
  type: "json_schema",
  schema: {
    type: "object",
    properties: {
      decision: { type: "string", enum: ["approve", "deny"] },
      reason: { type: "string" },
    },
    required: ["decision", "reason"],
    additionalProperties: false,
  },
};

const IDLE_SCHEMA = {
  type: "json_schema",
  schema: {
    type: "object",
    properties: {
      should_respond: { type: "boolean" },
      message: { type: "string" },
      confidence: { type: "number" },
      reasoning: { type: "string" },
    },
    required: ["should_respond", "message", "confidence", "reasoning"],
    additionalProperties: false,
  },
};

// Separate semaphores so idle evals can't block approvals
function makeSem(max) {
  let active = 0;
  const queue = [];
  return {
    get active() { return active; },
    get queued() { return queue.length; },
    acquire() {
      return new Promise((resolve) => {
        if (active < max) { active++; resolve(); }
        else { queue.push(resolve); }
      });
    },
    release() {
      if (queue.length > 0) { queue.shift()(); }
      else { active--; }
    },
  };
}

const approvalSem = makeSem(4);
const idleSem = makeSem(2);

async function evaluateApproval(systemPrompt, toolName, toolInput) {
  const resp = await client.messages.create({
    model: "claude-haiku-4-5",
    max_tokens: 512,
    system: systemPrompt,
    messages: [{ role: "user", content: `Tool: ${toolName}\nInput: ${toolInput.slice(0, 2000)}` }],
    output_config: { format: APPROVAL_SCHEMA },
  });

  return JSON.parse(resp.content[0].text);
}

async function evaluateIdle(systemPrompt, transcriptContext) {
  const resp = await client.messages.create({
    model: "claude-haiku-4-5",
    max_tokens: 512,
    system: systemPrompt,
    messages: [{
      role: "user",
      content: `Here is the recent Claude Code conversation. Claude has stopped. Should I auto-respond?\n\n---\n${transcriptContext.slice(0, 4000)}`,
    }],
    output_config: { format: IDLE_SCHEMA },
  });

  return JSON.parse(resp.content[0].text);
}

async function handleRequest(req, res) {
  if (req.method === "GET" && req.url === "/health") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({
      ok: true,
      approval: { active: approvalSem.active, queued: approvalSem.queued },
      idle: { active: idleSem.active, queued: idleSem.queued },
    }));
    return;
  }

  if (req.method !== "POST") {
    res.writeHead(405);
    res.end("method not allowed");
    return;
  }

  const body = await new Promise((resolve) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => resolve(JSON.parse(Buffer.concat(chunks).toString())));
  });

  const isApproval = req.url === "/evaluate-approval";
  const isIdle = req.url === "/evaluate-idle";
  if (!isApproval && !isIdle) {
    res.writeHead(404);
    res.end("not found");
    return;
  }

  const sem = isApproval ? approvalSem : idleSem;
  await sem.acquire();
  try {
    const result = isApproval
      ? await evaluateApproval(body.system_prompt, body.tool_name, body.tool_input)
      : await evaluateIdle(body.system_prompt, body.transcript_context);

    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(result));
  } catch (err) {
    console.error("Evaluation error:", err.message);
    res.writeHead(500, { "Content-Type": "application/json" });
    res.end(JSON.stringify(isApproval
      ? { decision: "deny", reason: `error: ${err.message}` }
      : { should_respond: false, message: "", confidence: 0, reasoning: `error: ${err.message}` }
    ));
  } finally {
    sem.release();
  }
}

const server = createServer(handleRequest);
server.listen(PORT, () => {
  console.log(`Pilot evaluator listening on port ${PORT} (Anthropic SDK)`);
});

process.on("SIGTERM", () => { server.close(); process.exit(0); });
process.on("SIGINT", () => { server.close(); process.exit(0); });
process.on("unhandledRejection", (err) => {
  console.error("Unhandled rejection:", err);
});
process.on("uncaughtException", (err) => {
  console.error("Uncaught exception:", err);
  process.exit(1);
});
