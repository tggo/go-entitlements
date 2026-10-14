-- Last known renewal setting; NULL when never reported.
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS auto_renew boolean;
