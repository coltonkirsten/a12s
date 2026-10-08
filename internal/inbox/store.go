// Package inbox is the Controller's durable per-Computer inbox (enhancements
// §4): Messages in fleet.db, their events, limits, holds and scoped API
// tokens. Every mutation is one SQLite transaction; the database runs in WAL
// mode with synchronous=FULL, so an enqueue is on disk before it is
// acknowledged.
package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"github.com/coltonkirsten/a12s/internal/api"
)

// Limits (§4.3, §4.11, §4.12, §4.13).
const (
	MaxBody           = 64 << 10
	MaxMetaKeys       = 16
	MaxMetaKey        = 64
	MaxMetaValue      = 1 << 10
	MaxMetaTotal      = 4 << 10
	MaxExpires        = 30 * 24 * 3600
	MaxIdempotencyKey = 128

	DefaultMaxMessages = 200
	MinMaxMessages     = 1
	CeilMaxMessages    = 2000
	DefaultMaxBytes    = 320 << 10
	MinMaxBytes        = 8 << 10
	CeilMaxBytes       = 1 << 20
	// PreambleReserve is set aside inside maxBytes for a Batch's header,
	// intro, safety block and end line (§4.8).
	PreambleReserve = 4 << 10

	// Retention of terminal rows (pruned only once journaled).
	Retention = 7 * 24 * time.Hour
)

// labelRE is the character set of sender and replace keys (§4.2, §4.12).
var labelRE = regexp.MustCompile(`^[A-Za-z0-9._:@/#-]{1,64}$`)

// ValidLabel reports whether s may be a sender or replace key.
func ValidLabel(s string) bool { return labelRE.MatchString(s) }

// Error is an inbox failure with its HTTP status, code and fix.
type Error struct {
	Status  int
	Code    api.ErrorCode
	Message string
	Fix     string
	Details map[string]string
}

func (e *Error) Error() string { return e.Message }

func errf(status int, code api.ErrorCode, fix, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...), Fix: fix}
}

// Store is fleet.db.
type Store struct {
	db  *sql.DB
	now func() time.Time
	// notify, when set, is called after a commit that changed an inbox,
	// with the Computer's name (the drainer's and expiry timer's trigger).
	notify func(computer string)
}

const schema = `
CREATE TABLE IF NOT EXISTS inboxes (
  computer TEXT PRIMARY KEY,
  next_seq INTEGER NOT NULL DEFAULT 1,
  declared INTEGER NOT NULL DEFAULT 1,
  held INTEGER NOT NULL DEFAULT 0,
  hold_reason TEXT NOT NULL DEFAULT '',
  diagnostic TEXT NOT NULL DEFAULT '',
  session_override TEXT NOT NULL DEFAULT '',
  max_messages INTEGER NOT NULL DEFAULT 200,
  max_messages_src TEXT NOT NULL DEFAULT 'builtin',
  max_bytes INTEGER NOT NULL DEFAULT 327680,
  max_bytes_src TEXT NOT NULL DEFAULT 'builtin',
  intro TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  computer TEXT NOT NULL,
  seq INTEGER NOT NULL,
  sender TEXT NOT NULL,
  via TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  body TEXT NOT NULL,
  metadata TEXT NOT NULL DEFAULT '',
  fresh INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER,
  expires_in INTEGER,
  replace_key TEXT NOT NULL DEFAULT '',
  superseded INTEGER NOT NULL DEFAULT 0,
  replaced_by TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  batch TEXT NOT NULL DEFAULT '',
  turn TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  bytes INTEGER NOT NULL,
  idem_key TEXT,
  idem_hash TEXT NOT NULL DEFAULT '',
  ended_at INTEGER,
  end_reason TEXT NOT NULL DEFAULT '',
  ended_by TEXT NOT NULL DEFAULT '',
  journaled INTEGER NOT NULL DEFAULT 1,
  UNIQUE (computer, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS messages_idem ON messages (computer, via, idem_key) WHERE idem_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS messages_state ON messages (computer, state, seq);
CREATE INDEX IF NOT EXISTS messages_expiry ON messages (state, expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS messages_replace ON messages (computer, via, replace_key, state) WHERE replace_key != '';
CREATE INDEX IF NOT EXISTS messages_ended ON messages (computer, ended_at) WHERE ended_at IS NOT NULL;
CREATE TABLE IF NOT EXISTS inbox_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  computer TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL,
  at INTEGER NOT NULL,
  principal TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS inbox_events_message ON inbox_events (message, id);
CREATE INDEX IF NOT EXISTS inbox_events_computer ON inbox_events (computer, id);
CREATE TABLE IF NOT EXISTS tokens (
  name TEXT PRIMARY KEY,
  hash TEXT NOT NULL UNIQUE,
  actions TEXT NOT NULL,
  computers TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
`

// Open opens (creating if needed) fleet.db at path.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open fleet.db: %w", err)
	}
	// One connection: transactions are serialized in-process, which is the
	// ordering §4.5 relies on, and SQLite never returns SQLITE_BUSY to us.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + batchSchema + powerSchema + serviceSchema + registrySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("fleet.db schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// OnChange sets the function called after a commit that changed an inbox.
func (s *Store) OnChange(f func(computer string)) { s.notify = f }

func (s *Store) changed(computer string) {
	if s.notify != nil && computer != "" {
		s.notify(computer)
	}
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// tx runs fn in an immediate transaction.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func event(tx *sql.Tx, computer, message, typ string, at time.Time, principal string, data any) error {
	d := "{}"
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		d = string(b)
	}
	_, err := tx.Exec(`INSERT INTO inbox_events (computer, message, type, at, principal, data) VALUES (?, ?, ?, ?, ?, ?)`,
		computer, message, typ, ms(at), principal, d)
	return err
}

// Limits are an inbox's resolved limits.
type Limits = api.InboxLimits

// BuiltinLimits are the limits of an inbox the Fleet says nothing about.
func BuiltinLimits() Limits {
	return Limits{
		MaxMessages: api.LimitValue{Value: DefaultMaxMessages, Source: "builtin"},
		MaxBytes:    api.LimitValue{Value: DefaultMaxBytes, Source: "builtin"},
	}
}

// Config is an inbox's Fleet configuration: limits and the Batch intro.
type Config struct {
	Limits Limits
	Intro  string
}

// SyncFleet records the declared Computers and their configuration: inboxes
// of new Computers are created, a returning name reattaches its orphaned
// inbox, undeclared ones become orphaned, and a limit or intro change is an
// event.
func (s *Store) SyncFleet(ctx context.Context, declared map[string]Config, by string) error {
	var touched []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		rows, err := tx.Query(`SELECT computer, declared, max_messages, max_messages_src, max_bytes, max_bytes_src, intro FROM inboxes`)
		if err != nil {
			return err
		}
		type row struct {
			declared bool
			lim      Limits
			intro    string
		}
		have := map[string]row{}
		for rows.Next() {
			var name string
			var r row
			if err := rows.Scan(&name, &r.declared, &r.lim.MaxMessages.Value, &r.lim.MaxMessages.Source, &r.lim.MaxBytes.Value, &r.lim.MaxBytes.Source, &r.intro); err != nil {
				rows.Close()
				return err
			}
			have[name] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for name, cfg := range declared {
			lim := cfg.Limits
			r, ok := have[name]
			if !ok {
				if _, err := tx.Exec(`INSERT INTO inboxes (computer, max_messages, max_messages_src, max_bytes, max_bytes_src, intro) VALUES (?, ?, ?, ?, ?, ?)`,
					name, lim.MaxMessages.Value, lim.MaxMessages.Source, lim.MaxBytes.Value, lim.MaxBytes.Source, cfg.Intro); err != nil {
					return err
				}
				continue
			}
			if r.intro != cfg.Intro {
				if _, err := tx.Exec(`UPDATE inboxes SET intro = ? WHERE computer = ?`, cfg.Intro, name); err != nil {
					return err
				}
				if err := event(tx, name, "", "intro_changed", now, by, nil); err != nil {
					return err
				}
			}
			if !r.declared {
				if _, err := tx.Exec(`UPDATE inboxes SET declared = 1 WHERE computer = ?`, name); err != nil {
					return err
				}
				touched = append(touched, name)
			}
			if r.lim != lim {
				if _, err := tx.Exec(`UPDATE inboxes SET max_messages = ?, max_messages_src = ?, max_bytes = ?, max_bytes_src = ? WHERE computer = ?`,
					lim.MaxMessages.Value, lim.MaxMessages.Source, lim.MaxBytes.Value, lim.MaxBytes.Source, name); err != nil {
					return err
				}
				if err := event(tx, name, "", "limits_changed", now, by, map[string]any{"from": r.lim, "to": lim}); err != nil {
					return err
				}
				touched = append(touched, name)
			}
		}
		for name, r := range have {
			if _, ok := declared[name]; !ok && r.declared {
				if _, err := tx.Exec(`UPDATE inboxes SET declared = 0 WHERE computer = ?`, name); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		for _, n := range touched {
			s.changed(n)
		}
	}
	return err
}

// inboxRow is an inbox's settings.
type inboxRow struct {
	nextSeq  int64
	declared bool
	held     bool
	reason   string
	diag     string
	override string
	limits   Limits
}

func getInbox(tx *sql.Tx, computer string) (*inboxRow, error) {
	var r inboxRow
	err := tx.QueryRow(`SELECT next_seq, declared, held, hold_reason, diagnostic, session_override, max_messages, max_messages_src, max_bytes, max_bytes_src FROM inboxes WHERE computer = ?`, computer).
		Scan(&r.nextSeq, &r.declared, &r.held, &r.reason, &r.diag, &r.override, &r.limits.MaxMessages.Value, &r.limits.MaxMessages.Source, &r.limits.MaxBytes.Value, &r.limits.MaxBytes.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func usage(tx *sql.Tx, computer string) (api.InboxUsage, error) {
	var u api.InboxUsage
	err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM messages WHERE computer = ? AND state IN ('pending', 'claimed')`, computer).Scan(&u.Messages, &u.Bytes)
	return u, err
}

// Enqueue is the admission of one Message (§4.4, §4.11, §4.12, §4.13).
type Enqueue struct {
	Computer  string
	Principal string // via
	Req       api.EnqueueRequest
}

// EnqueueResult is what Enqueue did.
type EnqueueResult struct {
	Message  api.Message
	Created  bool   // false: an idempotent repeat
	Replaced string // the superseded Message
	Position int
}

// requestHash identifies the fields an idempotent repeat must match.
func requestHash(r api.EnqueueRequest, sender string) string {
	h := sha256.New()
	exp := "none"
	if r.Expires != nil {
		exp = fmt.Sprint(*r.Expires)
	}
	b, _ := json.Marshal([]any{r.Body, sender, r.Metadata, r.Fresh, exp, r.ReplaceKey})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// SenderFor is the sender label a request resolves to: its own, or the
// principal's name.
func SenderFor(req api.EnqueueRequest, principal string) string {
	if req.Sender != "" {
		return req.Sender
	}
	return principal
}

// ValidateRequest checks an enqueue body against the static limits.
func ValidateRequest(req api.EnqueueRequest, principal string) *Error {
	switch {
	case req.Body == "":
		return errf(400, api.ErrInvalid, "send a non-empty body", "body is empty")
	case len(req.Body) > MaxBody:
		return errf(413, api.ErrBodyTooLarge, "shorten the body to 64 KiB, or put the content on a drive and send its path", "body is %d bytes, over the 64 KiB limit", len(req.Body))
	case !utf8.ValidString(req.Body):
		return errf(400, api.ErrInvalid, "send UTF-8 text", "body is not valid UTF-8")
	}
	if req.Sender != "" && !ValidLabel(req.Sender) {
		return errf(400, api.ErrInvalid, "use up to 64 characters of A-Z a-z 0-9 . _ : @ / # -", "sender %q is not a valid label", req.Sender)
	}
	if req.Sender == "" && !ValidLabel(principal) {
		return errf(400, api.ErrInvalid, "set sender", "principal %q is not a valid sender label", principal)
	}
	if req.ReplaceKey != "" && !ValidLabel(req.ReplaceKey) {
		return errf(400, api.ErrInvalid, "use up to 64 characters of A-Z a-z 0-9 . _ : @ / # -", "replaceKey %q is not a valid label", req.ReplaceKey)
	}
	if len(req.IdempotencyKey) > MaxIdempotencyKey {
		return errf(400, api.ErrInvalid, "use an idempotencyKey of at most 128 bytes", "idempotencyKey is too long")
	}
	if req.Expires != nil && (*req.Expires < 0 || *req.Expires > MaxExpires) {
		return errf(400, api.ErrBadExpires, "use 0 (now or never), 1 to 2592000 seconds, or omit expires to wait indefinitely", "expires %d is out of range", *req.Expires)
	}
	if len(req.Metadata) > MaxMetaKeys {
		return errf(400, api.ErrBadMetadata, "use at most 16 metadata keys", "metadata has %d keys", len(req.Metadata))
	}
	total := 0
	for k, v := range req.Metadata {
		switch {
		case k == "" || len(k) > MaxMetaKey:
			return errf(400, api.ErrBadMetadata, "use metadata keys of 1 to 64 bytes", "metadata key %q is empty or too long", k)
		case len(v) > MaxMetaValue:
			return errf(400, api.ErrBadMetadata, "use metadata values of at most 1 KiB", "metadata value of %q is too long", k)
		case !utf8.ValidString(k) || !utf8.ValidString(v):
			return errf(400, api.ErrBadMetadata, "use UTF-8 metadata", "metadata %q is not valid UTF-8", k)
		}
		total += len(k) + len(v)
	}
	if total > MaxMetaTotal {
		return errf(400, api.ErrBadMetadata, "keep metadata under 4 KiB in total", "metadata is %d bytes", total)
	}
	return nil
}

// ErrUnknownInbox is returned for a Computer with no declared inbox.
var ErrUnknownInbox = errors.New("no such inbox")

// NowOrNever, when set on an Enqueue of expires 0, runs inside the enqueue
// transaction after the row is inserted. It decides whether the Computer can
// take the Message now; returning an *Error rolls the enqueue back.
type NowOrNever func(tx *Tx, m api.Message) *Error

// Enqueue admits a Message. A nil Message and an *Error are returned for a
// refusal.
func (s *Store) Enqueue(ctx context.Context, e Enqueue, non NowOrNever) (*EnqueueResult, error) {
	req := e.Req
	if verr := ValidateRequest(req, e.Principal); verr != nil {
		return nil, verr
	}
	sender := SenderFor(req, e.Principal)
	hash := requestHash(req, sender)
	var res *EnqueueResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ib, err := getInbox(tx, e.Computer)
		if err != nil {
			return err
		}
		if ib == nil || !ib.declared {
			return ErrUnknownInbox
		}
		if req.IdempotencyKey != "" {
			var id, h string
			err := tx.QueryRow(`SELECT id, idem_hash FROM messages WHERE computer = ? AND via = ? AND idem_key = ?`, e.Computer, e.Principal, req.IdempotencyKey).Scan(&id, &h)
			switch {
			case err == nil && h == hash:
				m, err := getMessage(tx, e.Computer, id, true)
				if err != nil {
					return err
				}
				pos, err := position(tx, m)
				if err != nil {
					return err
				}
				res = &EnqueueResult{Message: *m, Position: pos}
				return nil
			case err == nil:
				return errf(409, api.ErrIdempotencyConflict, "use a new idempotencyKey for a different message", "idempotencyKey %q was already used with a different message (%s)", req.IdempotencyKey, id)
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		now := s.now()
		nowOrNever := req.Expires != nil && *req.Expires == 0

		// The replace candidate: the pending Message in (computer, via, key).
		// A now-or-never Message replaces nothing (§4.11).
		var old *api.Message
		if req.ReplaceKey != "" && !nowOrNever {
			var oldID string
			err := tx.QueryRow(`SELECT id FROM messages WHERE computer = ? AND via = ? AND replace_key = ? AND state = 'pending'`, e.Computer, e.Principal, req.ReplaceKey).Scan(&oldID)
			switch {
			case err == nil:
				if old, err = getMessage(tx, e.Computer, oldID, false); err != nil {
					return err
				}
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		superseded := int64(0)
		if old != nil {
			superseded = old.Superseded + 1
		}
		id := "m_" + NewULID(now)
		frame := Frame{ID: id, Sender: sender, Via: e.Principal, CreatedAt: now, Body: req.Body, Metadata: req.Metadata, ReplaceKey: req.ReplaceKey, Superseded: superseded}
		bytes := Reserve(frame)
		use, err := usage(tx, e.Computer)
		if err != nil {
			return err
		}
		maxM, maxB := ib.limits.MaxMessages.Value, ib.limits.MaxBytes.Value
		newM, newB := use.Messages+1, use.Bytes+bytes
		if old != nil {
			newM, newB = newM-1, newB-old.Bytes
		}
		// A replace that grows neither count nor bytes is always admitted (§4.12).
		growsNothing := old != nil && newM <= use.Messages && newB <= use.Bytes
		if !growsNothing {
			if bytes > maxB-PreambleReserve {
				return errf(413, api.ErrExceedsInbox,
					fmt.Sprintf("shorten the body, or raise computers.%s.inbox.maxBytes (ceiling 1Mi)", e.Computer),
					"this message renders to %d bytes, more than %s's whole inbox holds (%d bytes less the 4 KiB preamble)", bytes, e.Computer, maxB)
			}
			if newM > maxM || newB > maxB-PreambleReserve {
				fix := "wait for delivery, withdraw a message, or a12s inbox clear " + e.Computer
				if use.Messages > maxM || use.Bytes > maxB-PreambleReserve {
					fix = fmt.Sprintf("%s has %d outstanding (%d bytes), over its limit of %d (%d bytes); new messages are refused until it drains. Wait for delivery, or a12s inbox clear %s",
						e.Computer, use.Messages, use.Bytes, maxM, maxB, e.Computer)
				}
				return errf(429, api.ErrInboxFull, fix, "%s's inbox is full: %d messages, %d of %d bytes outstanding", e.Computer, use.Messages, use.Bytes, maxB)
			}
		}
		seq := ib.nextSeq
		if _, err := tx.Exec(`UPDATE inboxes SET next_seq = ? WHERE computer = ?`, seq+1, e.Computer); err != nil {
			return err
		}
		meta := ""
		if len(req.Metadata) > 0 {
			b, _ := json.Marshal(req.Metadata)
			meta = string(b)
		}
		var expiresAt, expiresIn any
		if req.Expires != nil {
			expiresAt, expiresIn = ms(now.Add(time.Duration(*req.Expires)*time.Second)), *req.Expires
		}
		var idem any
		if req.IdempotencyKey != "" {
			idem = req.IdempotencyKey
		}
		if _, err := tx.Exec(`INSERT INTO messages (id, computer, seq, sender, via, created_at, body, metadata, fresh, expires_at, expires_in, replace_key, superseded, state, bytes, idem_key, idem_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			id, e.Computer, seq, sender, e.Principal, ms(now), req.Body, meta, req.Fresh, expiresAt, expiresIn, req.ReplaceKey, superseded, bytes, idem, hash); err != nil {
			return err
		}
		data := map[string]any{"seq": seq, "sender": sender, "bytes": bytes}
		if req.Expires != nil {
			data["expires"] = *req.Expires
		}
		if req.ReplaceKey != "" {
			data["key"] = req.ReplaceKey
		}
		if err := event(tx, e.Computer, id, "enqueued", now, e.Principal, data); err != nil {
			return err
		}
		r := &EnqueueResult{Created: true}
		if old != nil {
			if err := endTx(tx, old.ID, api.MsgReplaced, "replaced", e.Principal, now, map[string]any{"by": id}); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE messages SET replaced_by = ? WHERE id = ?`, id, old.ID); err != nil {
				return err
			}
			r.Replaced = old.ID
		}
		m, err := getMessage(tx, e.Computer, id, true)
		if err != nil {
			return err
		}
		if nowOrNever {
			if non == nil {
				return NotIdle(e.Computer, "not_ready")
			}
			if verr := non(&Tx{tx: tx, s: s, at: now}, *m); verr != nil {
				return verr
			}
			if m, err = getMessage(tx, e.Computer, id, true); err != nil {
				return err
			}
		}
		r.Message = *m
		if r.Position, err = position(tx, m); err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res.Created {
		s.changed(e.Computer)
	}
	return res, nil
}

func (e *Error) withReason(reason string) *Error {
	if e.Details == nil {
		e.Details = map[string]string{}
	}
	e.Details["reason"] = reason
	return e
}

// WithReason sets details.reason on a not_idle refusal.
func (e *Error) WithReason(reason string) *Error { return e.withReason(reason) }

// NotIdle is the refusal of a now-or-never Message (§4.11).
func NotIdle(computer, reason string) *Error {
	return errf(409, api.ErrNotIdle, "retry later, or omit expires to queue it", "%s can't take the message now (%s)", computer, reason).withReason(reason)
}

// Tx is an open fleet.db transaction handed to a NowOrNever hook.
type Tx struct {
	tx *sql.Tx
	s  *Store
	at time.Time
}

func position(tx *sql.Tx, m *api.Message) (int, error) {
	if m.State != api.MsgPending && m.State != api.MsgClaimed {
		return 0, nil
	}
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE computer = ? AND state IN ('pending', 'claimed') AND seq < ?`, m.Computer, m.Seq).Scan(&n)
	return n, err
}

// endTx moves a pending row to a terminal state and records the event. The
// row's sealed-journal record is owed (journaled = 0).
func endTx(tx *sql.Tx, id, state, reason, by string, at time.Time, data map[string]any) error {
	res, err := tx.Exec(`UPDATE messages SET state = ?, ended_at = ?, end_reason = ?, ended_by = ?, journaled = 0 WHERE id = ? AND state = 'pending'`,
		state, ms(at), reason, by, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("message %s is no longer pending", id)
	}
	var computer string
	if err := tx.QueryRow(`SELECT computer FROM messages WHERE id = ?`, id).Scan(&computer); err != nil {
		return err
	}
	return event(tx, computer, id, state, at, by, data)
}

const messageCols = `id, computer, seq, sender, via, created_at, body, metadata, fresh, expires_at, replace_key, superseded, replaced_by, state, batch, turn, attempts, bytes, ended_at`

type scanner interface{ Scan(dest ...any) error }

func scanMessage(r scanner, bodies bool) (*api.Message, error) {
	var m api.Message
	var created int64
	var expires, ended sql.NullInt64
	var body, meta string
	if err := r.Scan(&m.ID, &m.Computer, &m.Seq, &m.Sender, &m.Via, &created, &body, &meta, &m.Fresh, &expires, &m.ReplaceKey, &m.Superseded, &m.ReplacedBy, &m.State, &m.Batch, &m.Turn, &m.Attempts, &m.Bytes, &ended); err != nil {
		return nil, err
	}
	m.CreatedAt = fromMS(created)
	if expires.Valid {
		t := fromMS(expires.Int64)
		m.ExpiresAt = &t
	}
	if ended.Valid {
		t := fromMS(ended.Int64)
		m.EndedAt = &t
	}
	if meta != "" {
		_ = json.Unmarshal([]byte(meta), &m.Metadata)
	}
	if bodies {
		m.Body = &body
	}
	return &m, nil
}

func getMessage(tx *sql.Tx, computer, id string, bodies bool) (*api.Message, error) {
	m, err := scanMessage(tx.QueryRow(`SELECT `+messageCols+` FROM messages WHERE computer = ? AND id = ?`, computer, id), bodies)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errf(404, api.ErrNotFound, "", "no message %s in %s's inbox", id, computer)
	}
	return m, err
}

// Get returns one Message. With via set, only a Message whose via matches
// is visible (the send action reads only its own).
func (s *Store) Get(ctx context.Context, computer, id, via string, bodies bool) (*api.Message, error) {
	var m *api.Message
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		m, err = getMessage(tx, computer, id, bodies)
		if err == nil && via != "" && m.Via != via {
			m, err = nil, errf(404, api.ErrNotFound, "", "no message %s in %s's inbox", id, computer)
		}
		return err
	})
	return m, err
}

// stateError is the 409 for acting on a Message that is not pending.
func stateError(m *api.Message) *Error {
	code := map[string]api.ErrorCode{
		api.MsgClaimed: api.ErrAlreadyClaimed, api.MsgDelivered: api.ErrAlreadyDelivered, api.MsgExpired: api.ErrExpired,
		api.MsgReplaced: api.ErrReplaced, api.MsgWithdrawn: api.ErrWithdrawn, api.MsgDiscarded: api.ErrDiscarded,
	}[m.State]
	e := errf(409, code, "", "message %s is %s", m.ID, m.State)
	if m.State == api.MsgReplaced {
		e.Details = map[string]string{"replacedBy": m.ReplacedBy}
		e.Message += " by " + m.ReplacedBy
	}
	return e
}

// Withdraw withdraws a pending Message. With via set, only that
// principal's Messages can be withdrawn.
func (s *Store) Withdraw(ctx context.Context, computer, id, via, by string) (*api.Message, error) {
	var m *api.Message
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if m, err = getMessage(tx, computer, id, false); err != nil {
			return err
		}
		if via != "" && m.Via != via {
			return errf(404, api.ErrNotFound, "", "no message %s in %s's inbox", id, computer)
		}
		if m.State != api.MsgPending {
			return stateError(m)
		}
		if err := endTx(tx, id, api.MsgWithdrawn, "withdrawn", by, s.now(), nil); err != nil {
			return err
		}
		m, err = getMessage(tx, computer, id, false)
		return err
	})
	if err == nil {
		s.changed(computer)
	}
	return m, err
}

// Clear discards pending Messages with seq <= throughSeq (nil: all). An
// orphaned inbox is cleared only with orphan set.
func (s *Store) Clear(ctx context.Context, computer string, throughSeq *int64, orphan bool, reason, by string) (int, error) {
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ib, err := getInbox(tx, computer)
		if err != nil {
			return err
		}
		if ib == nil {
			return ErrUnknownInbox
		}
		if !ib.declared && !orphan {
			return errf(409, api.ErrConflict, fmt.Sprintf("a12s inbox clear %s --orphan", computer), "%s is no longer in the Fleet; its inbox is orphaned", computer)
		}
		through := int64(1<<62 - 1)
		if throughSeq != nil {
			through = *throughSeq
		}
		rows, err := tx.Query(`SELECT id FROM messages WHERE computer = ? AND state = 'pending' AND seq <= ? ORDER BY seq`, computer, through)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		now := s.now()
		for _, id := range ids {
			if err := endTx(tx, id, api.MsgDiscarded, reason, by, now, map[string]any{"reason": reason}); err != nil {
				return err
			}
		}
		n = len(ids)
		return nil
	})
	if err == nil && n > 0 {
		s.changed(computer)
	}
	return n, err
}

// Hold stops delivery for an inbox; it still accepts.
func (s *Store) Hold(ctx context.Context, computer, reason, diagnostic, by string) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ib, err := getInbox(tx, computer)
		if err != nil {
			return err
		}
		if ib == nil {
			return ErrUnknownInbox
		}
		if _, err := tx.Exec(`UPDATE inboxes SET held = 1, hold_reason = ?, diagnostic = ? WHERE computer = ?`, reason, diagnostic, computer); err != nil {
			return err
		}
		return event(tx, computer, "", "held", s.now(), by, map[string]any{"reason": reason, "diagnostic": diagnostic})
	})
	if err == nil {
		s.changed(computer)
	}
	return err
}

// Resume lifts a hold. With newSession, a one-shot session override is
// attached to the next Batch (§4.6).
func (s *Store) Resume(ctx context.Context, computer string, newSession bool, by string) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ib, err := getInbox(tx, computer)
		if err != nil {
			return err
		}
		if ib == nil {
			return ErrUnknownInbox
		}
		override := ib.override
		if newSession {
			override = api.SessionNew
		}
		if _, err := tx.Exec(`UPDATE inboxes SET held = 0, hold_reason = '', diagnostic = '', session_override = ? WHERE computer = ?`, override, computer); err != nil {
			return err
		}
		return event(tx, computer, "", "resumed", s.now(), by, map[string]any{"session": map[bool]string{true: api.SessionNew, false: api.SessionCurrent}[newSession]})
	})
	if err == nil {
		s.changed(computer)
	}
	return err
}

// ExpireDue expires every pending Message whose deadline has passed and
// returns the Computers whose inboxes changed.
func (s *Store) ExpireDue(ctx context.Context) ([]string, error) {
	var touched []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		var err error
		touched, err = expireTx(tx, "", now)
		return err
	})
	if err == nil {
		for _, c := range touched {
			s.changed(c)
		}
	}
	return touched, err
}

// expireTx expires due pending rows (of one Computer, or all with "").
func expireTx(tx *sql.Tx, computer string, now time.Time) ([]string, error) {
	// Strictly past: a now-or-never Message (expiresAt = createdAt) is claimable in its own transaction.
	q := `SELECT id, computer, expires_at FROM messages WHERE state = 'pending' AND expires_at IS NOT NULL AND expires_at < ?`
	args := []any{ms(now)}
	if computer != "" {
		q += ` AND computer = ?`
		args = append(args, computer)
	}
	rows, err := tx.Query(q+` ORDER BY expires_at`, args...)
	if err != nil {
		return nil, err
	}
	type due struct {
		id, computer string
		at           int64
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.computer, &d.at); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, d)
	}
	rows.Close()
	seen := map[string]bool{}
	var touched []string
	for _, d := range list {
		if err := endTx(tx, d.id, api.MsgExpired, "expired", "controller", now, map[string]any{"expiresAt": fromMS(d.at)}); err != nil {
			return nil, err
		}
		if !seen[d.computer] {
			seen[d.computer] = true
			touched = append(touched, d.computer)
		}
	}
	return touched, nil
}

// NextExpiry is the earliest deadline among pending Messages, if any.
func (s *Store) NextExpiry(ctx context.Context) (time.Time, bool, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(expires_at) FROM messages WHERE state = 'pending' AND expires_at IS NOT NULL`).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, false, err
	}
	return fromMS(v.Int64), true, nil
}

// ListOptions filter List.
type ListOptions struct {
	State  string // "" or "pending": outstanding; "all"; or one state
	After  int64
	Bodies bool
	Via    string // only this principal's Messages
	Limit  int
}

// Summary returns an inbox's state.
func (s *Store) Summary(ctx context.Context, computer string) (*api.InboxSummary, error) {
	var sum *api.InboxSummary
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		sum, err = summaryTx(tx, computer, s.now())
		return err
	})
	return sum, err
}

func summaryTx(tx *sql.Tx, computer string, now time.Time) (*api.InboxSummary, error) {
	ib, err := getInbox(tx, computer)
	if err != nil {
		return nil, err
	}
	if ib == nil {
		return nil, ErrUnknownInbox
	}
	sum := &api.InboxSummary{Computer: computer, Held: ib.held, HoldReason: ib.reason, Diagnostic: ib.diag, Limits: ib.limits, Orphaned: !ib.declared, SessionOverride: ib.override}
	if err := tx.QueryRow(`SELECT COALESCE(SUM(state = 'pending'), 0), COALESCE(SUM(state = 'claimed'), 0) FROM messages WHERE computer = ? AND state IN ('pending', 'claimed')`, computer).Scan(&sum.Pending, &sum.Claimed); err != nil {
		return nil, err
	}
	if sum.Usage, err = usage(tx, computer); err != nil {
		return nil, err
	}
	sum.OverLimit = sum.Usage.Messages > ib.limits.MaxMessages.Value || sum.Usage.Bytes > ib.limits.MaxBytes.Value-PreambleReserve
	since := ms(now.Add(-24 * time.Hour))
	if err := tx.QueryRow(`SELECT COALESCE(SUM(state = 'expired'), 0), COALESCE(SUM(state = 'replaced'), 0), COALESCE(SUM(state = 'withdrawn'), 0), COALESCE(SUM(state = 'discarded'), 0)
		FROM messages WHERE computer = ? AND ended_at IS NOT NULL AND ended_at >= ?`, computer, since).
		Scan(&sum.Ended24h.Expired, &sum.Ended24h.Replaced, &sum.Ended24h.Withdrawn, &sum.Ended24h.Discarded); err != nil {
		return nil, err
	}
	return sum, nil
}

// List returns an inbox's summary and Messages in seq order.
func (s *Store) List(ctx context.Context, computer string, o ListOptions) (*api.InboxListResponse, error) {
	var out *api.InboxListResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sum, err := summaryTx(tx, computer, s.now())
		if err != nil {
			return err
		}
		out = &api.InboxListResponse{InboxSummary: *sum, Messages: []api.Message{}}
		q := `SELECT ` + messageCols + ` FROM messages WHERE computer = ? AND seq > ?`
		args := []any{computer, o.After}
		switch o.State {
		case "", api.MsgPending:
			q += ` AND state IN ('pending', 'claimed')`
		case "all":
		default:
			q += ` AND state = ?`
			args = append(args, o.State)
		}
		if o.Via != "" {
			q += ` AND via = ?`
			args = append(args, o.Via)
		}
		limit := o.Limit
		if limit <= 0 || limit > 5000 {
			limit = 5000
		}
		rows, err := tx.Query(q+` ORDER BY seq LIMIT ?`, append(args, limit)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMessage(rows, o.Bodies)
			if err != nil {
				return err
			}
			out.Messages = append(out.Messages, *m)
		}
		return rows.Err()
	})
	return out, err
}

// Inboxes lists every inbox's summary, declared and orphaned.
func (s *Store) Inboxes(ctx context.Context) ([]api.InboxSummary, error) {
	var out []api.InboxSummary
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT computer FROM inboxes ORDER BY computer`)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			names = append(names, n)
		}
		rows.Close()
		now := s.now()
		for _, n := range names {
			sum, err := summaryTx(tx, n, now)
			if err != nil {
				return err
			}
			out = append(out, *sum)
		}
		return nil
	})
	return out, err
}

// InboxEvent is one row of inbox_events.
type InboxEvent struct {
	ID        int64           `json:"id"`
	Computer  string          `json:"computer"`
	Message   string          `json:"message,omitempty"`
	Type      string          `json:"type"`
	At        time.Time       `json:"at"`
	Principal string          `json:"principal"`
	Data      json.RawMessage `json:"data"`
}

// Events returns inbox events of a Computer after id, optionally of one
// Message.
func (s *Store) Events(ctx context.Context, computer, message string, after int64, limit int) ([]InboxEvent, error) {
	q := `SELECT id, computer, message, type, at, principal, data FROM inbox_events WHERE computer = ? AND id > ?`
	args := []any{computer, after}
	if message != "" {
		q += ` AND message = ?`
		args = append(args, message)
	}
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxEvent
	for rows.Next() {
		var e InboxEvent
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &e.Computer, &e.Message, &e.Type, &at, &e.Principal, &data); err != nil {
			return nil, err
		}
		e.At, e.Data = fromMS(at), json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SessionOverride returns the inbox's pending one-shot session override
// ("" or "new").
func (s *Store) SessionOverride(ctx context.Context, computer string) (string, error) {
	var o string
	err := s.db.QueryRowContext(ctx, `SELECT session_override FROM inboxes WHERE computer = ?`, computer).Scan(&o)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return o, err
}
