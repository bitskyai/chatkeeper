CREATE TABLE workspace_flags (workspace TEXT PRIMARY KEY, paused INTEGER NOT NULL CHECK(paused IN (0,1)));
