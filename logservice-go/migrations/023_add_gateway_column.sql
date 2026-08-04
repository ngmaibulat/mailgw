-- Which gateway wrote this row, and which rule sent the message there.
--
-- Until now a log row said nothing about its origin. Haraka and mailgw-go rows
-- were distinguishable only by the X-NGM-Gateway header on the relayed message,
-- which is invisible from the log viewer — and with a fleet of gateways rather
-- than one box, "which of them handled this?" stops being a curiosity and
-- becomes the first question asked about any incident.
--
-- Existing rows get NULL and are deliberately not backfilled: NULL honestly
-- means "written before this column existed", and guessing would invent an
-- attribution nobody could later distinguish from a real one.

-- The gateway's Central Management uid, so a log row joins the console's
-- Gateways record directly. A file-mode gateway has no uid and sends its
-- server.hostname instead; mailgw-go truncates to 64 characters and warns when
-- it has to, so a value here is never silently cut.
ALTER TABLE `Connection`  ADD COLUMN `gateway` VARCHAR(64);
ALTER TABLE `Transaction` ADD COLUMN `gateway` VARCHAR(64);
ALTER TABLE `Delivery`    ADD COLUMN `gateway` VARCHAR(64);

-- The routing rule that chose this recipient's relay group, so the log tables
-- can answer "why did this go there?" — the question the rule DSL made possible
-- to ask and, until now, impossible to answer after the fact.
--
-- On Delivery alone, deliberately. Routing is evaluated per RECIPIENT, and one
-- message can have two recipients sent to the same relay group by two different
-- rules. Transaction is one row per message, so a column there would be either
-- ambiguous or lossy; Delivery is one row per recipient and is exact.
--
-- Empty when the default action applied, which is the correct record of "no
-- rule chose this".
ALTER TABLE `Delivery` ADD COLUMN `route_rule` VARCHAR(255);

-- Indexed because filtering by gateway is the first thing done with the column:
-- narrow to one box, then read. route_rule is deliberately NOT indexed — it is
-- a drill-down read after a gateway or time filter has already cut the set
-- down, so an index would cost writes to save nothing.
CREATE INDEX `idx_connection_gateway`  ON `Connection`  (`gateway`);
CREATE INDEX `idx_transaction_gateway` ON `Transaction` (`gateway`);
CREATE INDEX `idx_delivery_gateway`    ON `Delivery`    (`gateway`);
