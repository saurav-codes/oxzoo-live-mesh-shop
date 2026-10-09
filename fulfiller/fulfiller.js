// fulfiller: pops orders from the Redis list, marks them fulfilled in Postgres
// and sends the signed order.fulfilled webhook to rails-queue.
import { fileURLToPath } from "node:url";
import pg from "pg";
import { createClient } from "redis";
import { TRACE_RE, signHeader } from "../shared/zoosig.mjs";

export const QUEUE = "mesh-shop:orders";
export const HEARTBEAT = "mesh-shop:fulfiller:heartbeat";
const errMsg = (e) => String(e?.message ?? e).slice(0, 200);

export function parseJob(raw) {
  if (typeof raw !== "string" || raw.length > 1024) return null;
  let job;
  try {
    job = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!Number.isInteger(job?.order_id) || job.order_id < 1) return null;
  if (typeof job.trace !== "string" || !TRACE_RE.test(job.trace)) return null;
  return { orderId: job.order_id, trace: job.trace };
}

export function webhookRequest(base, key, trace, nowMs = Date.now()) {
  const path = "/webhooks/zoo";
  const body = JSON.stringify({ trace, source: "mesh-shop", event: "order.fulfilled" });
  return {
    url: base.replace(/\/+$/, "") + path,
    init: {
      method: "POST",
      body,
      headers: {
        "content-type": "application/json",
        "x-zoo-signature": signHeader(key, "POST", path, body, nowMs),
        "x-zoo-trace": trace,
      },
    },
  };
}

const hop = (db, trace, step, detail) =>
  db.query("insert into hops (trace, step, detail) values ($1, $2, $3)", [trace, step, detail]);

export async function handle(db, raw, env = process.env, fetchFn = fetch) {
  const job = parseJob(raw);
  if (!job) {
    console.error("dropping a malformed job");
    return;
  }
  await db.query("update orders set status = 'fulfilled' where id = $1", [job.orderId]);
  await hop(db, job.trace, "fulfilled", `order ${job.orderId}`);
  try {
    if (!env.RAILS_URL) throw new Error("RAILS_URL is not set");
    if (!env.WEBHOOK_SECRET) throw new Error("WEBHOOK_SECRET is not set");
    const { url, init } = webhookRequest(env.RAILS_URL, env.WEBHOOK_SECRET, job.trace);
    const res = await fetchFn(url, { ...init, signal: AbortSignal.timeout(8000) });
    if (!res.ok) throw new Error(`rails-queue answered ${res.status}`);
    await hop(db, job.trace, "webhook-sent", `rails-queue answered ${res.status}`);
  } catch (e) {
    await hop(db, job.trace, "failed", errMsg(e));
  }
}

async function main() {
  const redis = createClient({ url: process.env.REDIS_URL, socket: { connectTimeout: 5000 } });
  redis.on("error", (e) => console.error("redis", errMsg(e)));
  const db = new pg.Client({
    connectionString: process.env.DATABASE_URL,
    connectionTimeoutMillis: 5000,
    statement_timeout: 5000,
  });
  await redis.connect();
  await db.connect();
  console.log("fulfiller waiting for orders");
  for (;;) {
    await redis.set(HEARTBEAT, String(Date.now()), { EX: 60 });
    const item = await redis.brPop(QUEUE, 5);
    if (item) await handle(db, item.element);
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((e) => {
    console.error(errMsg(e));
    process.exit(1);
  });
}
