import os

import pytest

from main import parse_reserve, reserve_rows
from migrate import SCHEMA


def test_parse_reserve_accepts_valid():
    assert parse_reserve(b'{"sku":"ZOO-1","qty":2,"order_id":7}') == ("ZOO-1", 2, 7)
    assert parse_reserve(b'{"sku":"ZOO-1","order_id":7}') == ("ZOO-1", 1, 7)


@pytest.mark.parametrize(
    "raw",
    [
        b"nope",
        b"[]",
        b'{"sku":"zoo-1","order_id":1}',
        b'{"sku":"ZOO-1","qty":0,"order_id":1}',
        b'{"sku":"ZOO-1","qty":true,"order_id":1}',
        b'{"sku":"ZOO-1","qty":1,"order_id":"1"}',
        b'{"sku":"ZOO-1","order_id":1,"pad":"' + b"x" * 5000 + b'"}',
    ],
)
def test_parse_reserve_rejects(raw):
    with pytest.raises(ValueError):
        parse_reserve(raw)


@pytest.mark.skipif(not os.environ.get("TEST_DATABASE_URL"), reason="TEST_DATABASE_URL not set")
def test_reserve_with_row_lock():
    import psycopg

    with psycopg.connect(os.environ["TEST_DATABASE_URL"], autocommit=True) as conn:
        conn.execute(SCHEMA)
        conn.execute("update stock set qty = 1 where sku = 'ZOO-3'")
        assert reserve_rows(conn, "ZOO-3", 1, 1) == (200, {"reserved": True, "sku": "ZOO-3", "left": 0})
        assert reserve_rows(conn, "ZOO-3", 1, 2)[0] == 409
        assert reserve_rows(conn, "NOPE", 1, 3)[0] == 404
        conn.execute("update stock set qty = 100000 where sku = 'ZOO-3'")
