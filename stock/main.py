"""stock: reserves stock for the orders worker with a Postgres row lock."""

import json
import os
import re

import psycopg
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from starlette.concurrency import run_in_threadpool

SKU_RE = re.compile(r"^[A-Z0-9-]{1,32}$")
MAX_BODY = 4096

app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)


def parse_reserve(raw: bytes) -> tuple[str, int, int]:
    if len(raw) > MAX_BODY:
        raise ValueError("body too large")
    try:
        body = json.loads(raw)
    except json.JSONDecodeError as e:
        raise ValueError("body is not JSON") from e
    if not isinstance(body, dict):
        raise ValueError("body must be an object")
    sku, qty, order_id = body.get("sku"), body.get("qty", 1), body.get("order_id")
    if not isinstance(sku, str) or not SKU_RE.match(sku):
        raise ValueError("bad sku")
    if type(qty) is not int or not 1 <= qty <= 10:
        raise ValueError("qty must be 1 to 10")
    if type(order_id) is not int or order_id < 1:
        raise ValueError("bad order_id")
    return sku, qty, order_id


def connect() -> psycopg.Connection:
    return psycopg.connect(os.environ["DATABASE_URL"], connect_timeout=5, options="-c statement_timeout=5000")


def reserve_rows(conn: psycopg.Connection, sku: str, qty: int, order_id: int) -> tuple[int, dict]:
    with conn.transaction():
        row = conn.execute("select qty from stock where sku = %s for update", (sku,)).fetchone()
        if row is None:
            return 404, {"detail": f"unknown sku {sku}"}
        if row[0] < qty:
            return 409, {"detail": f"out of stock for {sku}"}
        conn.execute("update stock set qty = qty - %s where sku = %s", (qty, sku))
        conn.execute("insert into reservations (order_id, sku, qty) values (%s, %s, %s)", (order_id, sku, qty))
    return 200, {"reserved": True, "sku": sku, "left": row[0] - qty}


@app.get("/health")
def health():
    with connect() as conn:
        n = conn.execute("select count(*) from stock").fetchone()[0]
    return {"ok": True, "detail": f"{n} skus in the stock table"}


@app.get("/items")
def items():
    with connect() as conn:
        rows = conn.execute("select sku, qty from stock order by sku").fetchall()
    return [{"sku": sku, "qty": qty} for sku, qty in rows]


@app.post("/reserve")
async def reserve(request: Request):
    raw = b""
    async for chunk in request.stream():
        raw += chunk
        if len(raw) > MAX_BODY:
            return JSONResponse({"detail": "body too large"}, status_code=413)
    try:
        sku, qty, order_id = parse_reserve(raw)
    except ValueError as e:
        return JSONResponse({"detail": str(e)}, status_code=400)
    with await run_in_threadpool(connect) as conn:
        code, body = await run_in_threadpool(reserve_rows, conn, sku, qty, order_id)
    return JSONResponse(body, status_code=code)
