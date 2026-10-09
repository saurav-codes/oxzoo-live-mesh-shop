// Pure helpers for the orders worker (tested in lib.test.ts).
import { SKU_RE, TRACE_RE, signHeader } from "../shared/zoosig.mjs";

export const QUEUE = "mesh-shop:orders";
export const HEARTBEAT = "mesh-shop:fulfiller:heartbeat";
export const MAX_BODY = 4096;

export function parseStart(raw: string): { trace: string; sku: string } | null {
  if (raw.length > MAX_BODY) return null;
  let body: any;
  try {
    body = JSON.parse(raw);
  } catch {
    return null;
  }
  const { trace, sku } = body ?? {};
  if (typeof trace !== "string" || !TRACE_RE.test(trace)) return null;
  if (typeof sku !== "string" || !SKU_RE.test(sku)) return null;
  return { trace, sku };
}

// The signed catalog lookup: GET ${CATALOG_URL}/api/items/<sku> with the trace header.
export function catalogRequest(base: string, key: string, sku: string, trace: string, nowMs = Date.now()) {
  const path = `/api/items/${sku}`;
  return {
    url: base.replace(/\/+$/, "") + path,
    headers: { "x-zoo-signature": signHeader(key, "GET", path, "", nowMs), "x-zoo-trace": trace },
  };
}

export function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout>;
  const fail = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`timeout after ${ms} ms`)), ms);
  });
  return Promise.race([p, fail]).finally(() => clearTimeout(timer));
}

export function heartbeatAge(value: string | null, nowMs = Date.now()): number {
  if (!value) throw new Error("no heartbeat from the fulfiller");
  const age = (nowMs - Number(value)) / 1000;
  if (!Number.isFinite(age) || age > 30) throw new Error(`heartbeat ${Math.round(age)} s old`);
  return age;
}
