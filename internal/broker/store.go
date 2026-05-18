package broker

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Agent is a registered agent record.
type Agent struct {
	ID           string          `json:"agent_id"`
	Name         string          `json:"name"`
	CardJSON     json.RawMessage `json:"card"`
	Mode         string          `json:"mode"`     // realtime | offline | scheduled
	Schedule     string          `json:"schedule"` // optional JSON/cron
	LastSeen     time.Time       `json:"last_seen"`
	OnlineRT     bool            `json:"online"`
	CreatedAt    time.Time       `json:"created_at"`
}

// InboxEntry is a task sitting in an agent's inbox.
type InboxEntry struct {
	TaskID      string
	AgentID     string
	ContextID   string
	Payload     json.RawMessage // MessageSendParams
	State       string          // submitted|delivered|completed|failed
	CreatedAt   time.Time
	DeliveredAt sql.NullTime
}

// Result holds B's final answer for a task.
type Result struct {
	TaskID      string
	AgentID     string
	State       string
	ResultJSON  json.RawMessage
	CompletedAt time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS agents (
  id          TEXT PRIMARY KEY,
  token_hash  TEXT NOT NULL,
  name        TEXT NOT NULL,
  card_json   TEXT NOT NULL,
  mode        TEXT NOT NULL,
  schedule    TEXT,
  last_seen   TIMESTAMP,
  online_rt   INTEGER DEFAULT 0,
  created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS inbox (
  task_id      TEXT PRIMARY KEY,
  agent_id     TEXT NOT NULL,
  context_id   TEXT,
  payload      TEXT NOT NULL,
  state        TEXT NOT NULL,
  created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  delivered_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS results (
  task_id      TEXT PRIMARY KEY,
  agent_id     TEXT NOT NULL,
  state        TEXT NOT NULL,
  result_json  TEXT NOT NULL,
  completed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS stats (
  key   TEXT PRIMARY KEY,
  value INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_inbox_agent_state ON inbox(agent_id, state);
CREATE INDEX IF NOT EXISTS idx_agents_mode ON agents(mode);
`

// Store wraps the SQLite connection.
type Store struct {
	db *sql.DB
}

// NewStore opens (and migrates) the SQLite database at dsn.
func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

// --- Agent CRUD ---

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func genToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateAgent inserts a new agent and returns it plus the plaintext token.
func (s *Store) CreateAgent(name, mode, schedule string, card json.RawMessage) (*Agent, string, error) {
	id := uuid.NewString()
	token, err := genToken()
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	_, err = s.db.Exec(`INSERT INTO agents(id, token_hash, name, card_json, mode, schedule, last_seen, online_rt, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		id, hashToken(token), name, string(card), mode, schedule, now, 0, now)
	if err != nil {
		return nil, "", err
	}
	return &Agent{
		ID: id, Name: name, CardJSON: card, Mode: mode, Schedule: schedule,
		LastSeen: now, OnlineRT: false, CreatedAt: now,
	}, token, nil
}

// GetAgent by id.
func (s *Store) GetAgent(id string) (*Agent, error) {
	row := s.db.QueryRow(`SELECT id, name, card_json, mode, COALESCE(schedule,''), last_seen, online_rt, created_at FROM agents WHERE id=?`, id)
	var a Agent
	var cardStr string
	var online int
	if err := row.Scan(&a.ID, &a.Name, &cardStr, &a.Mode, &a.Schedule, &a.LastSeen, &online, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.CardJSON = json.RawMessage(cardStr)
	a.OnlineRT = online != 0
	return &a, nil
}

// VerifyToken returns true if token matches the agent's stored hash.
func (s *Store) VerifyToken(id, token string) (bool, error) {
	var h string
	err := s.db.QueryRow(`SELECT token_hash FROM agents WHERE id=?`, id).Scan(&h)
	if err != nil {
		return false, err
	}
	return h == hashToken(token), nil
}

// SetOnline updates the online flag and refreshes last_seen.
func (s *Store) SetOnline(id string, online bool) error {
	v := 0
	if online {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE agents SET online_rt=?, last_seen=? WHERE id=?`, v, time.Now().UTC(), id)
	return err
}

// ListAgents returns all agents; if onlineOnly, filters for online_rt=1.
func (s *Store) ListAgents(onlineOnly bool) ([]*Agent, error) {
	q := `SELECT id, name, card_json, mode, COALESCE(schedule,''), last_seen, online_rt, created_at FROM agents`
	if onlineOnly {
		q += ` WHERE online_rt=1`
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		var a Agent
		var cardStr string
		var online int
		if err := rows.Scan(&a.ID, &a.Name, &cardStr, &a.Mode, &a.Schedule, &a.LastSeen, &online, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.CardJSON = json.RawMessage(cardStr)
		a.OnlineRT = online != 0
		out = append(out, &a)
	}
	return out, nil
}

// UpdateAgent replaces mutable fields (card / mode / schedule).
func (s *Store) UpdateAgent(id string, card json.RawMessage, mode, schedule string) error {
	_, err := s.db.Exec(`UPDATE agents SET card_json=?, mode=?, schedule=? WHERE id=?`,
		string(card), mode, schedule, id)
	return err
}

// --- Inbox ---

// EnqueueTask writes a new inbox entry (state=submitted).
func (s *Store) EnqueueTask(taskID, agentID, contextID string, payload json.RawMessage) error {
	_, err := s.db.Exec(`INSERT INTO inbox(task_id, agent_id, context_id, payload, state) VALUES(?,?,?,?,'submitted')`,
		taskID, agentID, contextID, string(payload))
	return err
}

// MarkDelivered flips a task to delivered and stamps delivered_at.
func (s *Store) MarkDelivered(taskID string) error {
	_, err := s.db.Exec(`UPDATE inbox SET state='delivered', delivered_at=? WHERE task_id=?`, time.Now().UTC(), taskID)
	return err
}

// ListPending returns inbox entries for agentID still in 'submitted' or 'delivered' state.
func (s *Store) ListPending(agentID string) ([]*InboxEntry, error) {
	rows, err := s.db.Query(`SELECT task_id, agent_id, COALESCE(context_id,''), payload, state, created_at, delivered_at FROM inbox WHERE agent_id=? AND state IN('submitted','delivered') ORDER BY created_at ASC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*InboxEntry
	for rows.Next() {
		var e InboxEntry
		var payloadStr string
		if err := rows.Scan(&e.TaskID, &e.AgentID, &e.ContextID, &payloadStr, &e.State, &e.CreatedAt, &e.DeliveredAt); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payloadStr)
		out = append(out, &e)
	}
	return out, nil
}

// --- Results ---

// SaveResult persists B's reply and removes the inbox row.
func (s *Store) SaveResult(taskID, agentID, state string, result json.RawMessage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO results(task_id, agent_id, state, result_json) VALUES(?,?,?,?)`,
		taskID, agentID, state, string(result)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE inbox SET state=? WHERE task_id=?`, state, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetResult returns a completed result or nil if not found.
func (s *Store) GetResult(taskID string) (*Result, error) {
	row := s.db.QueryRow(`SELECT task_id, agent_id, state, result_json, completed_at FROM results WHERE task_id=?`, taskID)
	var r Result
	var rs string
	if err := row.Scan(&r.TaskID, &r.AgentID, &r.State, &rs, &r.CompletedAt); err != nil {
		return nil, err
	}
	r.ResultJSON = json.RawMessage(rs)
	return &r, nil
}

// --- Stats ---

// IncrStat atomically adds delta to the counter identified by key.
func (s *Store) IncrStat(key string, delta int64) error {
	_, err := s.db.Exec(
		`INSERT INTO stats(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = value + excluded.value`,
		key, delta)
	return err
}

// GetStat returns the current value (0 if the key has never been touched).
func (s *Store) GetStat(key string) (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT value FROM stats WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// CountAgents returns (total registered, currently online via SSE).
func (s *Store) CountAgents() (total, online int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&total); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE online_rt=1`).Scan(&online)
	return
}

// CountTasks returns (total tasks ever enqueued, tasks that reached a final state).
func (s *Store) CountTasks() (total, completed int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM inbox`).Scan(&total); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM results`).Scan(&completed)
	return
}

// GetInboxEntry fetches a single inbox row by task id (for status lookups before a result exists).
func (s *Store) GetInboxEntry(taskID string) (*InboxEntry, error) {
	row := s.db.QueryRow(`SELECT task_id, agent_id, COALESCE(context_id,''), payload, state, created_at, delivered_at FROM inbox WHERE task_id=?`, taskID)
	var e InboxEntry
	var payloadStr string
	if err := row.Scan(&e.TaskID, &e.AgentID, &e.ContextID, &payloadStr, &e.State, &e.CreatedAt, &e.DeliveredAt); err != nil {
		return nil, err
	}
	e.Payload = json.RawMessage(payloadStr)
	return &e, nil
}
