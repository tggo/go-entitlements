-- Event IDs are deduplicated per source, not globally.
ALTER TABLE processed_events DROP CONSTRAINT IF EXISTS processed_events_pkey;
ALTER TABLE processed_events ADD PRIMARY KEY (source, event_id);
