-- Split the single all-powerful user into admin / viewer.
--
-- Until now `Users` held only email + hash, so any logged-in user could edit
-- relay credentials and (once Central Management lands) approve gateways and
-- deploy configuration. New users default to 'viewer'.
ALTER TABLE `Users`
    ADD COLUMN `role` VARCHAR(32) NOT NULL DEFAULT 'viewer';

-- Existing users were effectively admins. Defaulting them to 'viewer' would
-- lock the operator out of their own console on upgrade, so promote every row
-- that exists at migration time.
UPDATE `Users` SET `role` = 'admin';
