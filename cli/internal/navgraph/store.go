// Package navgraph stores verified, directed navigation between app page states.
// Coordinates use the same 0-1000 screen units as history and saved intents.
package navgraph

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/intent"
	_ "modernc.org/sqlite"
)

type Scope struct {
	App     string `json:"app"`     // bundle ID, or another stable app identifier
	Build   string `json:"build"`   // explicit app version; never shared silently across versions
	Profile string `json:"profile"` // device/layout/orientation, e.g. iphone13pro-portrait-default
}

func (s Scope) validate() error {
	if strings.TrimSpace(s.App) == "" || strings.TrimSpace(s.Build) == "" || strings.TrimSpace(s.Profile) == "" {
		return errors.New("app, build and profile are required")
	}
	return nil
}

type Node struct {
	ID string `json:"id"`
	Scope
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Conditions []string `json:"conditions"`
	UpdatedAt  int64    `json:"updated_at"` // Unix milliseconds
}

type Edge struct {
	ID           string   `json:"id"`
	From         string   `json:"from"`
	To           string   `json:"to"`
	Label        string   `json:"label"`
	Action       Action   `json:"action"`
	Conditions   []string `json:"conditions"`
	Enabled      bool     `json:"enabled"`
	SuccessCount int      `json:"success_count"`
	FailureCount int      `json:"failure_count"`
	LastVerified int64    `json:"last_verified"`
	LastSuccess  int64    `json:"last_success"`
	LastOutcome  string   `json:"last_outcome"`
	FromRun      string   `json:"from_run,omitempty"`
	Step         int      `json:"step,omitempty"`
}

type Observation struct {
	ID         string `json:"id"`
	Edge       string `json:"edge"`
	ActualNode string `json:"actual_node,omitempty"`
	Success    bool   `json:"success"`
	Evidence   string `json:"evidence"` // what was checked, not merely "tap done"
	Screenshot string `json:"screenshot,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

type Screenshot struct {
	ID        string `json:"id"`
	Node      string `json:"node"`
	Path      string `json:"path"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CreatedAt int64  `json:"created_at"`
}

type Store struct {
	db  *sql.DB
	dir string
	now func() time.Time
}

// Open uses a separate local folder; graph images survive history's run retention.
func Open(configDir string) (*Store, error) {
	dir, err := filepath.Abs(filepath.Join(configDir, "navigation"))
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "graph.sqlite3")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir, now: time.Now}
	if err = s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	// WAL setup and a deferred schema transaction can race another process's first
	// open. Retrying the entire rolled-back initialization also handles BUSY_SNAPSHOT.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := s.initOnce()
		var code interface{ Code() int }
		if err == nil || !errors.As(err, &code) || (code.Code()&0xff != 5 && code.Code()&0xff != 6) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *Store) initOnce() error {
	for _, q := range []string{"PRAGMA busy_timeout=10000", "PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL"} {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("navigation database version %d is newer than supported version 1", version)
	}
	if version == 0 {
		_, err = tx.Exec(`
CREATE TABLE nodes (
 id TEXT PRIMARY KEY, app TEXT NOT NULL, build TEXT NOT NULL, profile TEXT NOT NULL,
 state_key TEXT NOT NULL, name TEXT NOT NULL, conditions TEXT NOT NULL, updated_at INTEGER NOT NULL,
 UNIQUE(app, build, profile, state_key)
);
CREATE TABLE edges (
 id TEXT PRIMARY KEY, from_node TEXT NOT NULL REFERENCES nodes(id), to_node TEXT NOT NULL REFERENCES nodes(id),
 label TEXT NOT NULL, action TEXT NOT NULL, conditions TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
 success_count INTEGER NOT NULL DEFAULT 0, failure_count INTEGER NOT NULL DEFAULT 0,
 last_verified INTEGER NOT NULL DEFAULT 0, last_success INTEGER NOT NULL DEFAULT 0, last_outcome TEXT NOT NULL DEFAULT '',
 from_run TEXT NOT NULL DEFAULT '', step INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX edges_from ON edges(from_node, enabled);
CREATE INDEX edges_to ON edges(to_node);
CREATE TABLE screenshots (
 id TEXT PRIMARY KEY, node TEXT NOT NULL REFERENCES nodes(id), path TEXT NOT NULL,
 width INTEGER NOT NULL, height INTEGER NOT NULL, created_at INTEGER NOT NULL
);
CREATE INDEX screenshots_node ON screenshots(node, created_at DESC);
CREATE TABLE observations (
 id TEXT PRIMARY KEY, edge TEXT NOT NULL REFERENCES edges(id), actual_node TEXT REFERENCES nodes(id),
 success INTEGER NOT NULL, evidence TEXT NOT NULL, screenshot TEXT REFERENCES screenshots(id), created_at INTEGER NOT NULL
);
CREATE INDEX observations_edge ON observations(edge, created_at DESC);
PRAGMA user_version=1;`)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func tags(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	sort.Strings(out)
	return out
}

func encode(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *Store) SaveNode(n Node) (Node, error) {
	if err := n.Scope.validate(); err != nil {
		return Node{}, err
	}
	n.Key, n.Name = strings.TrimSpace(n.Key), strings.TrimSpace(n.Name)
	if n.Key == "" || n.Name == "" {
		return Node{}, errors.New("node key and name are required")
	}
	n.ID, n.Conditions, n.UpdatedAt = intent.NewID(), tags(n.Conditions), s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO nodes(id,app,build,profile,state_key,name,conditions,updated_at) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(app,build,profile,state_key) DO UPDATE SET name=excluded.name,conditions=excluded.conditions,updated_at=excluded.updated_at`,
		n.ID, n.App, n.Build, n.Profile, n.Key, n.Name, encode(n.Conditions), n.UpdatedAt)
	if err != nil {
		return Node{}, err
	}
	return s.FindNode(n.Scope, n.Key)
}

const nodeColumns = "id,app,build,profile,state_key,name,conditions,updated_at"

type scanner interface{ Scan(...any) error }

func scanNode(row scanner) (Node, error) {
	var n Node
	var conditions string
	err := row.Scan(&n.ID, &n.App, &n.Build, &n.Profile, &n.Key, &n.Name, &conditions, &n.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return n, errors.New("node not found")
	}
	if err == nil {
		err = json.Unmarshal([]byte(conditions), &n.Conditions)
	}
	return n, err
}
func (s *Store) Node(id string) (Node, error) {
	return scanNode(s.db.QueryRow("SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
}

// FindNode uses the scope/key unique index rather than scanning page names.
func (s *Store) FindNode(scope Scope, key string) (Node, error) {
	if err := scope.validate(); err != nil {
		return Node{}, err
	}
	if strings.TrimSpace(key) == "" {
		return Node{}, errors.New("node key is required")
	}
	return scanNode(s.db.QueryRow("SELECT "+nodeColumns+" FROM nodes WHERE app=? AND build=? AND profile=? AND state_key=?", scope.App, scope.Build, scope.Profile, key))
}

// Nodes returns an indexed page within one exact app/build/profile, never an entire graph.
func (s *Store) Nodes(scope Scope, limit, offset int) ([]Node, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	if err := page(limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT "+nodeColumns+" FROM nodes WHERE app=? AND build=? AND profile=? ORDER BY state_key LIMIT ? OFFSET ?", scope.App, scope.Build, scope.Profile, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func page(limit, offset int) error {
	if limit < 1 || limit > 500 || offset < 0 {
		return errors.New("limit must be 1..500 and offset nonnegative")
	}
	return nil
}

type EdgeInput struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Label      string   `json:"label"`
	Action     Action   `json:"action"`
	Conditions []string `json:"conditions"`
	Disabled   bool     `json:"disabled"`
	FromRun    string   `json:"from_run,omitempty"`
	Step       int      `json:"step,omitempty"`
}

func (s *Store) SaveEdge(in EdgeInput) (Edge, error) {
	from, err := s.Node(in.From)
	if err != nil {
		return Edge{}, err
	}
	to, err := s.Node(in.To)
	if err != nil {
		return Edge{}, err
	}
	if from.Scope != to.Scope {
		return Edge{}, errors.New("edge endpoints must have the same app, build and profile")
	}
	if err = in.Action.Validate(); err != nil {
		return Edge{}, err
	}
	in.Label = strings.TrimSpace(in.Label)
	if in.Label == "" {
		return Edge{}, errors.New("edge label is required")
	}
	in.Conditions = tags(in.Conditions)
	// One edge per transition/action/preconditions. Re-saving never resets accumulated evidence.
	sum := sha256.Sum256([]byte(encode([]any{in.From, in.To, in.Action, in.Conditions})))
	id := hex.EncodeToString(sum[:])
	_, err = s.db.Exec(`INSERT INTO edges(id,from_node,to_node,label,action,conditions,enabled,from_run,step) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET label=excluded.label, enabled=excluded.enabled`, id, in.From, in.To, in.Label, encode(in.Action), encode(in.Conditions), !in.Disabled, in.FromRun, in.Step)
	if err != nil {
		return Edge{}, err
	}
	return s.Edge(id)
}

const edgeColumns = "id,from_node,to_node,label,action,conditions,enabled,success_count,failure_count,last_verified,last_success,last_outcome,from_run,step"

func scanEdge(row scanner) (Edge, error) {
	var e Edge
	var action, conditions string
	err := row.Scan(&e.ID, &e.From, &e.To, &e.Label, &action, &conditions, &e.Enabled, &e.SuccessCount, &e.FailureCount, &e.LastVerified, &e.LastSuccess, &e.LastOutcome, &e.FromRun, &e.Step)
	if errors.Is(err, sql.ErrNoRows) {
		return e, errors.New("edge not found")
	}
	if err == nil {
		err = json.Unmarshal([]byte(action), &e.Action)
	}
	if err == nil {
		err = json.Unmarshal([]byte(conditions), &e.Conditions)
	}
	return e, err
}
func (s *Store) Edge(id string) (Edge, error) {
	return scanEdge(s.db.QueryRow("SELECT "+edgeColumns+" FROM edges WHERE id=?", id))
}
func (s *Store) SetEnabled(id string, enabled bool) (Edge, error) {
	if _, err := s.Edge(id); err != nil {
		return Edge{}, err
	}
	if _, err := s.db.Exec("UPDATE edges SET enabled=? WHERE id=?", enabled, id); err != nil {
		return Edge{}, err
	}
	return s.Edge(id)
}
func (s *Store) Edges(node string, incoming bool, limit, offset int) ([]Edge, error) {
	if _, err := s.Node(node); err != nil {
		return nil, err
	}
	if err := page(limit, offset); err != nil {
		return nil, err
	}
	column := "from_node"
	if incoming {
		column = "to_node"
	}
	rows, err := s.db.Query("SELECT "+edgeColumns+" FROM edges WHERE "+column+"=? ORDER BY id LIMIT ? OFFSET ?", node, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Observe(o Observation) (Observation, error) {
	e, err := s.Edge(o.Edge)
	if err != nil {
		return o, err
	}
	o.Evidence = strings.TrimSpace(o.Evidence)
	if o.Evidence == "" {
		return o, errors.New("evidence must describe the observed page or reliable state signal")
	}
	if o.Success && o.ActualNode != e.To {
		return o, errors.New("success requires actual_node to equal the edge destination after verification")
	}
	if o.ActualNode != "" {
		n, err := s.Node(o.ActualNode)
		if err != nil {
			return o, err
		}
		from, err := s.Node(e.From)
		if err != nil {
			return o, err
		}
		if n.Scope != from.Scope {
			return o, errors.New("actual_node belongs to another app/build/profile")
		}
	}
	if o.Screenshot != "" {
		shot, err := s.Screenshot(o.Screenshot)
		if err != nil {
			return o, err
		}
		if o.ActualNode == "" || shot.Node != o.ActualNode {
			return o, errors.New("screenshot must belong to actual_node")
		}
	}
	o.ID, o.CreatedAt = intent.NewID(), s.now().UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return o, err
	}
	defer tx.Rollback()
	_, err = tx.Exec("INSERT INTO observations(id,edge,actual_node,success,evidence,screenshot,created_at) VALUES(?,?,?,?,?,?,?)", o.ID, o.Edge, nullable(o.ActualNode), o.Success, o.Evidence, nullable(o.Screenshot), o.CreatedAt)
	if err != nil {
		return o, err
	}
	outcome := "failure"
	if o.Success {
		outcome = "success"
	}
	_, err = tx.Exec(`UPDATE edges SET success_count=success_count+?,failure_count=failure_count+?,last_verified=?,last_outcome=?,last_success=CASE WHEN ? THEN ? ELSE last_success END WHERE id=?`, boolInt(o.Success), boolInt(!o.Success), o.CreatedAt, outcome, o.Success, o.CreatedAt, o.Edge)
	if err != nil {
		return o, err
	}
	return o, tx.Commit()
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) Observations(edge string, limit, offset int) ([]Observation, error) {
	if _, err := s.Edge(edge); err != nil {
		return nil, err
	}
	if err := page(limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT id,edge,COALESCE(actual_node,''),success,evidence,COALESCE(screenshot,''),created_at FROM observations WHERE edge=? ORDER BY created_at DESC,rowid DESC LIMIT ? OFFSET ?", edge, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.Edge, &o.ActualNode, &o.Success, &o.Evidence, &o.Screenshot, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
