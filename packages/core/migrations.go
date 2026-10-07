package core

// migrations[i] upgrades the schema from version i to i+1. Append only:
// never edit a migration that has shipped.
var migrations = []string{
	// v1: core tables.
	`
CREATE TABLE tasks (
	id           TEXT PRIMARY KEY,
	title        TEXT NOT NULL CHECK (length(trim(title)) > 0),
	description  TEXT NOT NULL DEFAULT '',
	due_at       TEXT,
	priority     INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 4),
	category     TEXT NOT NULL DEFAULT '',
	parent_id    TEXT NOT NULL DEFAULT '',
	notes        TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL CHECK (status IN ('todo','in_progress','done','archived')),
	position     REAL NOT NULL DEFAULT 0,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL,
	completed_at TEXT,
	archived_at  TEXT,
	deleted_at   TEXT,
	version      INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE task_tags (
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	tag     TEXT NOT NULL,
	PRIMARY KEY (task_id, tag)
);
CREATE TABLE operations (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL,
	actor      TEXT NOT NULL,
	summary    TEXT NOT NULL,
	snapshots  TEXT NOT NULL,
	created_at TEXT NOT NULL,
	undone_at  TEXT
);
CREATE TABLE task_history (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id      TEXT NOT NULL,
	operation_id INTEGER,
	action       TEXT NOT NULL,
	actor        TEXT NOT NULL,
	changes      TEXT NOT NULL DEFAULT '{}',
	created_at   TEXT NOT NULL
);
`,
	// v2: indexes for filtering and sorting at 10k+ tasks.
	`
CREATE INDEX idx_tasks_status   ON tasks(status) WHERE deleted_at IS NULL;
CREATE INDEX idx_tasks_due      ON tasks(due_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_tasks_parent   ON tasks(parent_id);
CREATE INDEX idx_tasks_position ON tasks(position);
CREATE INDEX idx_tasks_updated  ON tasks(updated_at);
CREATE INDEX idx_task_tags_tag  ON task_tags(tag);
CREATE INDEX idx_history_task   ON task_history(task_id, id);
`,
	// v3: full snapshot of every task version (recoverable versions) and
	// rejected concurrent edits kept for later resolution.
	`
CREATE TABLE task_versions (
	task_id    TEXT NOT NULL,
	version    INTEGER NOT NULL,
	snapshot   TEXT NOT NULL,
	actor      TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (task_id, version)
);
CREATE TABLE conflicts (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id         TEXT NOT NULL,
	base_version    INTEGER NOT NULL,
	current_version INTEGER NOT NULL,
	actor           TEXT NOT NULL,
	patch           TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	resolved_at     TEXT,
	resolution      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_conflicts_task ON conflicts(task_id, id);
`,
}
