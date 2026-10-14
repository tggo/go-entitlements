-- Transactions of a chain that were refunded or revoked (Apple transactionId), oldest first.
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS refunded_transactions text[] NOT NULL DEFAULT '{}';
