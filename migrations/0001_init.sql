-- Orders table.
--
-- Note the CHECK on status: the set of statuses is small and closed. That
-- matters later — a closed set is safe to use as a Prometheus label value,
-- an open set (like customer_id) is not.
CREATE TABLE IF NOT EXISTS orders (
    id           UUID        PRIMARY KEY,
    customer_id  TEXT        NOT NULL,
    status       TEXT        NOT NULL
                             CHECK (status IN ('pending','processing','paid','failed','cancelled')),
    amount_cents BIGINT      NOT NULL CHECK (amount_cents > 0),
    currency     TEXT        NOT NULL CHECK (char_length(currency) = 3),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports the "oldest pending orders" query the worker will run in Phase 4,
-- and the status filter on the list endpoint.
CREATE INDEX IF NOT EXISTS orders_status_created_at_idx
    ON orders (status, created_at);

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
