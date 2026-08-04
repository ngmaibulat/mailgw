-- The latest counter snapshot each gateway reported.
--
-- mailgw-go has posted a `metrics` object on every heartbeat since M6, and the
-- console has been validating and discarding it. This is where it lands, so
-- "what is this gateway doing right now?" is answerable from the fleet view
-- instead of by scraping each node's /metrics by hand.
--
-- Deliberately ONE ROW PER GATEWAY, not a time series. A latest snapshot
-- answers roughly ninety percent of what an operator asks and costs nothing to
-- keep; a time series is a retention-and-rollup decision that deserves its own
-- migration and its own purge job alongside purgeOldLogs, rather than being
-- smuggled in behind a heartbeat. Prometheus is already scraping the same
-- counters from the gateways themselves if real history is wanted.
CREATE TABLE IF NOT EXISTS `GatewayMetrics` (
    -- The primary key, so the write is a plain upsert and "the latest
    -- snapshot" needs no ordering to resolve. A gateway's row disappears with
    -- the gateway (see CtrlGateway.deleteHandle).
    `gateway_id` INT      NOT NULL PRIMARY KEY,

    -- The snapshot object verbatim, exactly as the gateway sent it.
    --
    -- JSON rather than a column per counter: the counter set is defined in
    -- mailgw-go/internal/obs, and pinning it into a schema here would mean a
    -- migration every time a counter is added, with a fleet running mixed
    -- versions in the meantime. The keys are a stable contract on that side
    -- (internal/obs has a golden test for them), so the console can read the
    -- ones it knows and ignore the rest.
    `metrics`    TEXT     NOT NULL,

    -- When this snapshot arrived. Gateways.last_seen tracks contact of any
    -- kind; this tracks the numbers specifically, and the two can differ if a
    -- report ever arrives without metrics.
    `updated_at` DATETIME NOT NULL
);
