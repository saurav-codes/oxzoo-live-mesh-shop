// zoo-sig v1 (DESIGN.md), shared by the Bun orders worker and the Node fulfiller.
import { createHash, createHmac } from "node:crypto";

export const TRACE_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
export const SKU_RE = /^[A-Z0-9-]{1,32}$/;
export const CALLER = "mesh-shop";

export const sha256 = (data) => createHash("sha256").update(data).digest("hex");
export const fp = (value) => sha256(value).slice(-4);

export function sign(key, t, method, path, body = "") {
  return createHmac("sha256", key).update(`${t}.${method.toUpperCase()}.${path}.${sha256(body)}`).digest("hex");
}

export function signHeader(key, method, path, body = "", nowMs = Date.now()) {
  const t = Math.floor(nowMs / 1000);
  return `t=${t},caller=${CALLER},sig=${sign(key, t, method, path, body)}`;
}
