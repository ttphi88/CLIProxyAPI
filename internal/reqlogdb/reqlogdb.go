// Package reqlogdb stores proxied requests and responses in one SQLite file.
//
// Clients resend the whole conversation on every turn, so most of a request
// body repeats the previous one. Split cuts a JSON body into a small head plus
// content-addressed parts (system prompt, tool list, each message); each part
// is stored once in the blobs table, zstd-compressed. A request row keeps the
// compressed head and the list of part hashes. Join reverses Split.
package reqlogdb

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"
)

// HashSize is the number of sha256 bytes kept per part.
const HashSize = 16

const schema = `
CREATE TABLE IF NOT EXISTS requests (
  id          INTEGER PRIMARY KEY,
  ts          TEXT NOT NULL,     -- request time, RFC 3339 UTC
  duration_ms INTEGER,
  request_id  TEXT,
  method      TEXT,
  url         TEXT,
  status      INTEGER,
  model       TEXT,
  key_hash    TEXT,              -- sha256 hex of the client key (matches dashboard aliases)
  headers     BLOB,              -- zstd JSON object, secrets masked
  body_head   BLOB,              -- zstd body without the parts
  body_parts  BLOB,              -- concatenated 16-byte part hashes
  response    BLOB,              -- zstd response as sent to the client
  upstream    BLOB               -- zstd upstream request/response/errors, failed requests only
);
CREATE INDEX IF NOT EXISTS requests_ts ON requests(ts);
CREATE TABLE IF NOT EXISTS blobs (
  hash BLOB PRIMARY KEY,
  data BLOB NOT NULL             -- zstd
) WITHOUT ROWID;
`

// Body keys stored as one part each, and body keys whose array elements are
// stored as one part each. Order matters: Join consumes parts in this order.
var (
	wholeKeys = []string{"system", "tools", "instructions", "systemInstruction"}
	arrayKeys = []string{"messages", "input", "contents"}
)

var (
	enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ = zstd.NewReader(nil)
)

// Record is one proxied request, uncompressed.
type Record struct {
	Time      time.Time
	Duration  time.Duration
	RequestID string
	Method    string
	URL       string
	Status    int
	Model     string
	KeyHash   string
	Headers   []byte
	Body      []byte
	Response  []byte
	Upstream  []byte
}

// Store is an open request log database.
type Store struct{ db *sql.DB }

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open request log db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create request log schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read-only queries.
func (s *Store) DB() *sql.DB { return s.db }

// Insert writes one record in a single transaction.
func (s *Store) Insert(r *Record) error {
	head, parts := Split(r.Body)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	hashes := make([]byte, 0, len(parts)*HashSize)
	for _, p := range parts {
		sum := sha256.Sum256(p)
		h := sum[:HashSize]
		hashes = append(hashes, h...)
		var one int
		if errExists := tx.QueryRow("SELECT 1 FROM blobs WHERE hash = ?", h).Scan(&one); errExists == nil {
			continue
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO blobs (hash, data) VALUES (?, ?)", h, compress(p)); err != nil {
			return fmt.Errorf("insert blob: %w", err)
		}
	}
	_, err = tx.Exec(`INSERT INTO requests (ts, duration_ms, request_id, method, url, status, model, key_hash, headers, body_head, body_parts, response, upstream)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Time.UTC().Format(time.RFC3339Nano), r.Duration.Milliseconds(), r.RequestID, r.Method, r.URL, r.Status, r.Model, r.KeyHash,
		compress(r.Headers), compress(head), hashes, compress(r.Response), compress(r.Upstream))
	if err != nil {
		return fmt.Errorf("insert request: %w", err)
	}
	return tx.Commit()
}

// Get returns the stored record with the given id, with the body rebuilt.
func (s *Store) Get(id int64) (*Record, error) {
	var (
		r                                        Record
		ts                                       string
		dur                                      int64
		headers, head, parts, response, upstream []byte
	)
	err := s.db.QueryRow(`SELECT ts, duration_ms, request_id, method, url, status, model, key_hash, headers, body_head, body_parts, response, upstream
		FROM requests WHERE id = ?`, id).Scan(&ts, &dur, &r.RequestID, &r.Method, &r.URL, &r.Status, &r.Model, &r.KeyHash, &headers, &head, &parts, &response, &upstream)
	if err != nil {
		return nil, err
	}
	r.Time, _ = time.Parse(time.RFC3339Nano, ts)
	r.Duration = time.Duration(dur) * time.Millisecond
	var errs [4]error
	r.Headers, errs[0] = decompress(headers)
	r.Response, errs[1] = decompress(response)
	r.Upstream, errs[2] = decompress(upstream)
	head, errs[3] = decompress(head)
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	var blobs [][]byte
	for i := 0; i+HashSize <= len(parts); i += HashSize {
		var data []byte
		if err = s.db.QueryRow("SELECT data FROM blobs WHERE hash = ?", parts[i:i+HashSize]).Scan(&data); err != nil {
			return nil, fmt.Errorf("blob %x: %w", parts[i:i+HashSize], err)
		}
		if data, err = decompress(data); err != nil {
			return nil, err
		}
		blobs = append(blobs, data)
	}
	if r.Body, err = Join(head, blobs); err != nil {
		return nil, err
	}
	return &r, nil
}

// Split cuts a JSON object body into a head and parts. In the head each split
// key keeps its place, with the part count as its value. Non-object bodies
// come back unchanged as the head.
func Split(body []byte) (head []byte, parts [][]byte) {
	if len(body) == 0 || !ojson.ValidBytes(body) {
		return body, nil
	}
	root := ojson.ParseBytes(body)
	if !root.IsObject() {
		return body, nil
	}
	doc := ojson.NewDoc(body)
	for _, k := range wholeKeys {
		if v := root.Get(k); v.Exists() {
			parts = append(parts, []byte(v.Raw))
			_ = doc.Set(k, 1)
		}
	}
	for _, k := range arrayKeys {
		if v := root.Get(k); v.IsArray() {
			items := v.Array()
			for _, item := range items {
				parts = append(parts, []byte(item.Raw))
			}
			_ = doc.Set(k, len(items))
		}
	}
	if len(parts) == 0 {
		return body, nil
	}
	return doc.Bytes(), parts
}

// Join rebuilds a body from Split's head and parts.
func Join(head []byte, parts [][]byte) ([]byte, error) {
	if len(parts) == 0 {
		return head, nil
	}
	root := ojson.ParseBytes(head)
	doc := ojson.NewDoc(head)
	next := 0
	take := func() ([]byte, error) {
		if next >= len(parts) {
			return nil, fmt.Errorf("request body needs more parts than stored (%d)", len(parts))
		}
		next++
		return parts[next-1], nil
	}
	for _, k := range wholeKeys {
		if !root.Get(k).Exists() {
			continue
		}
		p, err := take()
		if err != nil {
			return nil, err
		}
		if err = doc.SetRawBytes(k, p); err != nil {
			return nil, fmt.Errorf("restore %s: %w", k, err)
		}
	}
	for _, k := range arrayKeys {
		v := root.Get(k)
		if v.Type != ojson.Number {
			continue
		}
		raw := []byte{'['}
		for i := int64(0); i < v.Int(); i++ {
			p, err := take()
			if err != nil {
				return nil, err
			}
			if i > 0 {
				raw = append(raw, ',')
			}
			raw = append(raw, p...)
		}
		raw = append(raw, ']')
		if err := doc.SetRawBytes(k, raw); err != nil {
			return nil, fmt.Errorf("restore %s: %w", k, err)
		}
	}
	if next != len(parts) {
		return nil, fmt.Errorf("request body used %d of %d stored parts", next, len(parts))
	}
	return doc.Bytes(), nil
}

func compress(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return enc.EncodeAll(b, make([]byte, 0, len(b)/4))
}

func decompress(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	out, err := dec.DecodeAll(b, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	return out, nil
}
