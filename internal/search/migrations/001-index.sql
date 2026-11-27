CREATE TABLE IF NOT EXISTS origin(id INTEGER PRIMARY KEY CHECK(id=1),source TEXT);
CREATE TABLE IF NOT EXISTS state(id INTEGER PRIMARY KEY CHECK(id=1),revision TEXT);
CREATE VIRTUAL TABLE IF NOT EXISTS threads USING fts5(channel UNINDEXED,root_ts UNINDEXED,root_time UNINDEXED,authors UNINDEXED,fingerprint UNINDEXED,body,source_deleted UNINDEXED,prior_versions UNINDEXED,permalink UNINDEXED,tokenize='unicode61');
PRAGMA user_version=1;
