-- Environment replaces the sandbox flag: it says which non-production environment, if any.
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS environment text NOT NULL DEFAULT '';
UPDATE entitlements SET environment = 'Sandbox' WHERE sandbox AND environment = '';
ALTER TABLE entitlements DROP COLUMN IF EXISTS sandbox;
