package core

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// DBFileName is the SQLite file inside the data directory.
	DBFileName = "todo.db"
	// BackupDirName holds pre-migration and manual backups.
	BackupDirName = "backups"
	// EnvHome overrides the default data directory.
	EnvHome = "TODO_CLI_HOME"
)

// DefaultDataDir returns $TODO_CLI_HOME or ~/.todo-cli.
func DefaultDataDir() (string, error) {
	if d := os.Getenv(EnvHome); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".todo-cli"), nil
}

// Options configures Open.
type Options struct {
	// DataDir is where todo.db and backups live; empty means DefaultDataDir.
	DataDir string
	// Actor is recorded in history for every change (cli, web, llm, agent...).
	Actor string
	// Now overrides the clock, mainly for tests.
	Now func() time.Time
}

// Store is the single source of truth for tasks.
type Store struct {
	db      *sql.DB
	dataDir string
	actor   string
	now     func() time.Time

	// LastMigration describes what Open did to the schema, if anything.
	LastMigration *MigrationReport
}

// MigrationReport describes a schema upgrade performed by Open.
type MigrationReport struct {
	From       int    `json:"from"`
	To         int    `json:"to"`
	BackupPath string `json:"backup_path,omitempty"`
}

// Open opens (creating if needed) the store in opts.DataDir, backing up and
// migrating the schema when it is older than this binary expects.
func Open(opts Options) (*Store, error) { return openAt(opts, len(migrations)) }

func openAt(opts Options, target int) (*Store, error) {
	dir := opts.DataDir
	if dir == "" {
		d, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, DBFileName)
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// Task data is private to the current user.
	_ = os.Chmod(path, 0o600)

	s := &Store{db: db, dataDir: dir, actor: opts.Actor, now: opts.Now}
	if s.actor == "" {
		s.actor = "cli"
	}
	if s.now == nil {
		s.now = time.Now
	}
	if err := s.migrate(target); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is the directory holding the database and backups.
func (s *Store) DataDir() string { return s.dataDir }

// DBPath is the SQLite file path.
func (s *Store) DBPath() string { return filepath.Join(s.dataDir, DBFileName) }

// Actor is the history actor used for changes made through this store.
func (s *Store) Actor() string { return s.actor }

func (s *Store) clock() time.Time { return s.now().UTC() }

// DataVersion returns a counter that changes whenever another connection
// or process commits to the database. Long-running UIs poll it to notice
// changes made elsewhere; the store's own writes do not change it.
func (s *Store) DataVersion() (int64, error) {
	var v int64
	err := s.db.QueryRow(`PRAGMA data_version`).Scan(&v)
	return v, err
}

// SchemaVersion returns the currently applied schema version.
func (s *Store) SchemaVersion() (int, error) { return schemaVersion(s.db) }

// LatestSchemaVersion is the schema version this build migrates to.
func LatestSchemaVersion() int { return len(migrations) }

func schemaVersion(q interface {
	QueryRow(string, ...any) *sql.Row
}) (int, error) {
	var exists int
	if err := q.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	var v sql.NullInt64
	if err := q.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// migrate applies migrations up to target in a single transaction, taking a
// backup first when an existing database is being upgraded.
func (s *Store) migrate(target int) error {
	cur, err := schemaVersion(s.db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if cur > len(migrations) {
		return fmt.Errorf("%w: database schema v%d is newer than this build supports (v%d); upgrade todo-cli", ErrSchemaTooNew, cur, len(migrations))
	}
	if cur >= target {
		return nil
	}
	report := &MigrationReport{From: cur, To: target}
	if cur > 0 {
		p, err := s.Backup(fmt.Sprintf("pre-migrate-v%d-to-v%d", cur, target))
		if err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
		report.BackupPath = p
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	for v := cur + 1; v <= target; v++ {
		if _, err := tx.Exec(migrations[v-1]); err != nil {
			return fmt.Errorf("migration v%d failed (rolled back, database unchanged): %w", v, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, v, fmtTime(s.clock())); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.LastMigration = report
	return nil
}

// Backup writes a consistent copy of the database into the backups directory
// and returns its path.
func (s *Store) Backup(label string) (string, error) {
	dir := filepath.Join(s.dataDir, BackupDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("todo-%s", s.clock().Format("20060102T150405.000000000Z"))
	if label != "" {
		name += "-" + label
	}
	path := filepath.Join(dir, name+".db")
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	return path, nil
}

// Backups lists backup files, newest last.
func (s *Store) Backups() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, BackupDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
			out = append(out, filepath.Join(s.dataDir, BackupDirName, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Stats summarizes stored data for `todo status`.
type Stats struct {
	Total    int            `json:"total"`
	ByStatus map[Status]int `json:"by_status"`
	Deleted  int            `json:"deleted"`
	Overdue  int            `json:"overdue"`
}

// Stats counts live tasks per status plus soft-deleted tasks.
func (s *Store) Stats() (Stats, error) {
	st := Stats{ByStatus: map[Status]int{}}
	for _, v := range Statuses {
		st.ByStatus[v] = 0
	}
	rows, err := s.db.Query(`SELECT status, deleted_at IS NOT NULL, count(*) FROM tasks GROUP BY 1, 2`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status Status
		var deleted bool
		var n int
		if err := rows.Scan(&status, &deleted, &n); err != nil {
			return st, err
		}
		if deleted {
			st.Deleted += n
			continue
		}
		st.ByStatus[status] += n
		st.Total += n
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	err = s.db.QueryRow(`SELECT count(*) FROM tasks WHERE deleted_at IS NULL AND status IN ('todo','in_progress') AND due_at IS NOT NULL AND due_at < ?`,
		fmtTime(s.clock())).Scan(&st.Overdue)
	return st, err
}
