// orders: the shop-order chain. Creates the order, does the signed catalog
// lookup, asks the stock worker to reserve, and queues the order on Redis for
// the fulfiller. Also runs the Postgres and Redis probe checks for the gateway.
import { SQL, RedisClient } from "bun";
import { HEARTBEAT, MAX_BODY, QUEUE, catalogRequest, heartbeatAge, parseStart, withTimeout } from "./lib";
import { TRACE_RE } from "../shared/zoosig.mjs";

const db = new SQL({ url: process.env.DATABASE_URL!, max: 5, connectionTimeout: 5 });
const redis = new RedisClient(process.env.REDIS_URL!, { connectionTimeout: 5000 });
const stockURL = (process.env.STOCK_URL ?? "").replace(/\/+$/, "");
const errMsg = (e: unknown) => String((e as Error)?.message ?? e).slice(0, 200);

async function hop(trace: string, step: string, detail: string) {
  await db`insert into hops (trace, step, detail) values (${trace}, ${step}, ${detail})`;
}

async function timed(id: string, label: string, env: string[], fn: () => Promise<string>) {
  const t0 = performance.now();
  const ms = () => Math.round(performance.now() - t0);
  try {
    const detail = await withTimeout(fn(), 5000);
    return { id, label, ok: true, ms: ms(), detail, env };
  } catch (e) {
    return { id, label, ok: false, ms: ms(), error: errMsg(e), env };
  }
}

function checks() {
  return Promise.all([
    timed("postgres", "Postgres write, read, delete", ["DATABASE_URL"], async () => {
      const note = crypto.randomUUID();
      const [row] = await db`insert into zoo_probe (note) values (${note}) returning id`;
      const [back] = await db`select note from zoo_probe where id = ${row.id}`;
      await db`delete from zoo_probe where id = ${row.id}`;
      if (back?.note !== note) throw new Error("read back a different row");
      const [v] = await db`show server_version`;
      return `zoo_probe row round trip, server ${v.server_version}`;
    }),
    timed("redis", "Redis SET/GET/DEL with TTL", ["REDIS_URL"], async () => {
      const key = `mesh-shop:probe:${crypto.randomUUID()}`;
      const value = crypto.randomUUID();
      await redis.send("SET", [key, value, "EX", "30"]);
      const back = await redis.get(key);
      await redis.del(key);
      if (back !== value) throw new Error("read back a different value");
      return "probe key round trip, TTL 30 s";
    }),
    timed("fulfiller", "Fulfiller worker heartbeat in Redis", ["REDIS_URL"], async () => {
      return `last beat ${heartbeatAge(await redis.get(HEARTBEAT)).toFixed(1)} s ago`;
    }),
  ]);
}

async function runChain(trace: string, sku: string) {
  const [order] = await db`insert into orders (sku, trace) values (${sku}, ${trace}) returning id`;
  const id = Number(order.id);
  await hop(trace, "order-created", `order ${id} for ${sku}`);
  try {
    const base = process.env.CATALOG_URL, key = process.env.INTERNAL_TOKEN;
    if (!base) throw new Error("CATALOG_URL is not set");
    if (!key) throw new Error("INTERNAL_TOKEN is not set");
    const req = catalogRequest(base, key, sku, trace);
    const res = await fetch(req.url, { headers: req.headers, signal: AbortSignal.timeout(8000) });
    if (!res.ok) throw new Error(`catalog-api answered ${res.status} for ${sku}`);
    await hop(trace, "catalog-ok", `catalog-api has ${sku}`);
    const reserve = { sku, qty: 1, order_id: id };
    const st = await fetch(`${stockURL}/reserve`, { method: "POST", body: JSON.stringify(reserve), signal: AbortSignal.timeout(5000) });
    const body: any = await st.json();
    if (!st.ok) throw new Error(`stock: ${body?.detail ?? st.status}`);
    await hop(trace, "stock-reserved", `${body.left} ${sku} left`);
    await db`update orders set status = 'reserved' where id = ${id}`;
    await redis.send("LPUSH", [QUEUE, JSON.stringify({ order_id: id, trace })]);
  } catch (e) {
    await db`update orders set status = 'failed' where id = ${id}`;
    await hop(trace, "failed", errMsg(e));
  }
}

async function route(req: Request, url: URL): Promise<Response> {
  if (req.method === "GET" && url.pathname === "/health") return Response.json({ ok: true, detail: `bun ${Bun.version}` });
  if (req.method === "GET" && url.pathname === "/checks") return Response.json(await checks());
  if (req.method === "POST" && url.pathname === "/chain") {
    const raw = await req.text();
    const start = parseStart(raw.length > MAX_BODY ? "" : raw);
    if (!start) return Response.json({ error: "bad trace or sku" }, { status: 400 });
    runChain(start.trace, start.sku).catch((e) => console.error("chain", start.trace, errMsg(e)));
    return Response.json({ trace: start.trace, started: true }, { status: 202 });
  }
  const trace = url.pathname.match(/^\/trace\/(.+)$/)?.[1];
  if (req.method === "GET" && trace) {
    if (!TRACE_RE.test(trace)) return Response.json({ error: "bad trace" }, { status: 400 });
    const rows = await db`select at, step, detail from hops where trace = ${trace} order by id limit 50`;
    return Response.json({ trace, found: rows.length > 0, hops: rows });
  }
  if (req.method === "GET" && url.pathname === "/orders") return Response.json(await db`select id, sku, status, at from orders order by id desc limit 20`);
  return Response.json({ error: "not found" }, { status: 404 });
}

Bun.serve({
  hostname: "127.0.0.1",
  port: Number(process.env.PORT),
  maxRequestBodySize: MAX_BODY,
  async fetch(req) {
    try {
      return await route(req, new URL(req.url));
    } catch (e) {
      console.error(req.method, new URL(req.url).pathname, errMsg(e));
      return Response.json({ error: "internal error" }, { status: 500 });
    }
  },
});
console.log(`orders on 127.0.0.1:${process.env.PORT}`);
