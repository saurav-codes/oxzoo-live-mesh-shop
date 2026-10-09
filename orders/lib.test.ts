import { expect, test } from "bun:test";
import { catalogRequest, heartbeatAge, parseStart, withTimeout } from "./lib";
import { fp, sign } from "../shared/zoosig.mjs";

const KEY = "zoo-test-key-0123456789abcdef";
const TRACE = "3f9c2a10-1b2c-4d5e-8f90-a1b2c3d4e5f6";

test("signing vectors", () => {
  expect(sign(KEY, 1760000000, "POST", "/api/items?x=1", '{"sku":"ZOO-1"}')).toBe(
    "50c22839fe6a06cb51a9fd25167d9e457eb0b5ee63ce696f4c5428a6b9271da1",
  );
  expect(sign(KEY, 1760000000, "GET", "/_zoo/verify")).toBe(
    "9a404bebaa32497c5ed39ef8990e8466428f8023d6aa9f5acc94f16fb7670ecb",
  );
  expect(fp(KEY)).toBe("915a");
});

test("catalog request is signed over the exact path and carries the trace", () => {
  const r = catalogRequest("https://catalog-api.s2.zoo.sorv.dev/", KEY, "ZOO-1", TRACE, 1760000000_000);
  expect(r.url).toBe("https://catalog-api.s2.zoo.sorv.dev/api/items/ZOO-1");
  expect(r.headers["x-zoo-trace"]).toBe(TRACE);
  expect(r.headers["x-zoo-signature"]).toBe(
    `t=1760000000,caller=mesh-shop,sig=${sign(KEY, 1760000000, "GET", "/api/items/ZOO-1")}`,
  );
});

test("parseStart validates trace ids and skus", () => {
  expect(parseStart(JSON.stringify({ trace: TRACE, sku: "ZOO-1" }))).toEqual({ trace: TRACE, sku: "ZOO-1" });
  for (const bad of [
    "not json",
    JSON.stringify({ trace: TRACE.toUpperCase(), sku: "ZOO-1" }),
    JSON.stringify({ trace: "abc", sku: "ZOO-1" }),
    JSON.stringify({ trace: TRACE, sku: "../x" }),
    JSON.stringify({ trace: TRACE, sku: "ZOO-1", pad: "x".repeat(5000) }),
  ]) expect(parseStart(bad)).toBeNull();
});

test("heartbeat age and timeouts", async () => {
  expect(heartbeatAge(String(1000), 3000)).toBe(2);
  expect(() => heartbeatAge(null)).toThrow("no heartbeat");
  expect(() => heartbeatAge("0", 60_000)).toThrow("60 s old");
  await expect(withTimeout(new Promise(() => {}), 10)).rejects.toThrow("timeout after 10 ms");
});
