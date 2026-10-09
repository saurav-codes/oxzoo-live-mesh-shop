"""Schema for the whole project (orders, stock, hops, probe) and the seed stock."""

import os

import psycopg

SCHEMA = """
create table if not exists stock (sku text primary key, qty integer not null check (qty >= 0));
create table if not exists reservations (
  id bigserial primary key, order_id bigint not null, sku text not null, qty integer not null,
  at timestamptz not null default now());
create table if not exists orders (
  id bigserial primary key, sku text not null, trace uuid not null,
  status text not null default 'created', at timestamptz not null default now());
create index if not exists orders_trace on orders (trace);
create table if not exists hops (
  id bigserial primary key, trace uuid not null, step text not null,
  detail text not null default '', at timestamptz not null default now());
create index if not exists hops_trace on hops (trace);
create table if not exists zoo_probe (id bigserial primary key, note text not null, at timestamptz not null default now());
insert into stock (sku, qty) values ('ZOO-1', 100000), ('ZOO-2', 100000), ('ZOO-3', 100000)
  on conflict (sku) do nothing;
delete from hops where at < now() - interval '7 days';
"""

if __name__ == "__main__":
    with psycopg.connect(os.environ["DATABASE_URL"], connect_timeout=10) as conn:
        conn.execute(SCHEMA)
    print("mesh-shop schema ready")
