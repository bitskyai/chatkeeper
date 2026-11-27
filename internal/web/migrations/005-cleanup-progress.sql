ALTER TABLE cleanup_runs ADD COLUMN status TEXT NOT NULL DEFAULT 'complete';
UPDATE cleanup_runs SET status='interrupted' WHERE status='running';
