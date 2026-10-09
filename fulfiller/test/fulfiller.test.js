import assert from "node:assert/strict";
import { test } from "node:test";
import { CALLER, fp, sha256, sign } from "../../shared/zoosig.mjs";
import { handle, parseJob, webhookRequest } from "../fulfiller.js";

const KEY = "zoo-test-key-0123456789abcdef";
const TRACE = "3f9c2a10-1b2c-4d5e-8f90-a1b2c3d4e5f6";

test("signing vectors", () => {
  assert.equal(sha256('{"sku":"ZOO-1"}'), "cc2860a77ea231854ea58f9cb05f3217059f80a8e95d7b69a204293ae4f3a444");
  assert.equal(sign(KEY, 1760000000, "POST", "/api/items?x=1", '{"sku":"ZOO-1"}'), "50c22839fe6a06cb51a9fd25167d9e457eb0b5ee63ce696f4c5428a6b9271da1");
  assert.equal(sign(KEY, 1760000000, "GET", "/_zoo/verify", ""), "9a404bebaa32497c5ed39ef8990e8466428f8023d6aa9f5acc94f16fb7670ecb");
  assert.equal(fp(KEY), "915a");
  assert.equal(CALLER, "mesh-shop");
});

test("webhook is signed over the exact body", () => {
  const { url, init } = webhookRequest("https://rails-queue.s1.zoo.sorv.dev/", KEY, TRACE, 1760000000_000);
  assert.equal(url, "https://rails-queue.s1.zoo.sorv.dev/webhooks/zoo");
  assert.deepEqual(JSON.parse(init.body), { trace: TRACE, source: "mesh-shop", event: "order.fulfilled" });
  assert.equal(init.headers["x-zoo-signature"], `t=1760000000,caller=mesh-shop,sig=${sign(KEY, 1760000000, "POST", "/webhooks/zoo", init.body)}`);
  assert.equal(init.headers["x-zoo-trace"], TRACE);
});

test("parseJob validates trace ids and order ids", () => {
  assert.deepEqual(parseJob(JSON.stringify({ order_id: 3, trace: TRACE })), { orderId: 3, trace: TRACE });
  for (const bad of ["x", "{}", JSON.stringify({ order_id: 0, trace: TRACE }), JSON.stringify({ order_id: 1, trace: "no" }), "a".repeat(2000)]) {
    assert.equal(parseJob(bad), null);
  }
});

test("handle records fulfilled and webhook-sent hops", async () => {
  const queries = [];
  const db = { query: async (sql, args) => queries.push([sql, args]) };
  let sent;
  const fetchFn = async (url, init) => ((sent = { url, init }), { ok: true, status: 202 });
  await handle(db, JSON.stringify({ order_id: 9, trace: TRACE }), { RAILS_URL: "https://r.example", WEBHOOK_SECRET: KEY }, fetchFn);
  assert.equal(sent.url, "https://r.example/webhooks/zoo");
  assert.deepEqual(queries.map((q) => q[1]?.[1]), [undefined, "fulfilled", "webhook-sent"]);
  queries.length = 0;
  await handle(db, JSON.stringify({ order_id: 9, trace: TRACE }), { RAILS_URL: "https://r.example" }, fetchFn);
  assert.deepEqual(queries.at(-1)[1], [TRACE, "failed", "WEBHOOK_SECRET is not set"]);
});
