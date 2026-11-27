CREATE TABLE cleanup_previews (digest TEXT PRIMARY KEY, session TEXT NOT NULL, workspace TEXT NOT NULL, policy TEXT NOT NULL, report TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE TABLE cleanup_runs (id INTEGER PRIMARY KEY, workspace TEXT NOT NULL, at INTEGER NOT NULL, report TEXT NOT NULL);
CREATE TRIGGER cap_cleanup_runs AFTER INSERT ON cleanup_runs BEGIN DELETE FROM cleanup_runs WHERE id < NEW.id - 999; END;
