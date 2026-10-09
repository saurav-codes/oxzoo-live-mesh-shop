// Shop page: lists stock, places an order and follows its chain hops.
const STEPS = ["order-created", "catalog-ok", "stock-reserved", "fulfilled", "webhook-sent"];
const $ = (id: string) => document.getElementById(id)!;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function getJSON(path: string) {
  const res = await fetch(path, { signal: AbortSignal.timeout(8000) });
  if (!res.ok) throw new Error(`${path} answered ${res.status}`);
  return res.json();
}

function row(cells: (string | Node)[]) {
  const tr = document.createElement("tr");
  for (const c of cells) {
    const td = document.createElement("td");
    td.append(c);
    tr.append(td);
  }
  return tr;
}

async function refresh() {
  try {
    const stock: { sku: string; qty: number }[] = await getJSON("/api/stock");
    $("stock").replaceChildren(
      ...stock.map((s) => {
        const b = document.createElement("button");
        b.textContent = "Order one";
        b.onclick = () => place(s.sku);
        return row([s.sku, String(s.qty), b]);
      }),
    );
    const orders: { id: number; sku: string; status: string }[] = await getJSON("/api/orders");
    $("orders").replaceChildren(...orders.map((o) => row([String(o.id), o.sku, o.status])));
  } catch (e) {
    $("status").textContent = String((e as Error).message);
  }
}

async function place(sku: string) {
  const res = await fetch("/api/orders", { method: "POST", body: JSON.stringify({ sku, trace: crypto.randomUUID() }) });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    $("status").textContent = `Order refused: ${body.error ?? res.status}`;
    return;
  }
  follow(body.trace);
}

async function follow(trace: string) {
  $("status").textContent = `Trace ${trace}`;
  for (let i = 0; i < 60; i++) {
    const t: { hops: { step: string; detail: string }[] } = await getJSON(`/_zoo/trace/${trace}`).catch(() => ({ hops: [] }));
    const seen = new Map(t.hops.map((h) => [h.step, h.detail]));
    const failed = seen.get("failed");
    $("steps").replaceChildren(
      ...STEPS.map((s) => {
        const li = document.createElement("li");
        li.className = seen.has(s) ? "done" : "wait";
        li.textContent = seen.has(s) ? `${s}: ${seen.get(s)}` : s;
        return li;
      }),
    );
    if (failed) Object.assign($("status"), { className: "fail", textContent: `Failed: ${failed}` });
    if (failed || STEPS.every((s) => seen.has(s))) break;
    await sleep(1000);
  }
  refresh();
}

refresh();
