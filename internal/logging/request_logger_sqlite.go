package logging

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/reqlogdb"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// sqliteQueueSize bounds how many finished requests wait for the writer.
// Bodies can be a few MB each, so keep it small; when full, records are dropped.
const sqliteQueueSize = 32

// SQLiteRequestLogger writes every proxied request to a SQLite database
// (see internal/reqlogdb) instead of one text file per request. It is always
// enabled; writes happen on one background goroutine and never block a request.
type SQLiteRequestLogger struct {
	store *reqlogdb.Store
	queue chan *reqlogdb.Record
}

// NewSQLiteRequestLogger opens the database at path and starts the writer.
func NewSQLiteRequestLogger(path string) (*SQLiteRequestLogger, error) {
	store, err := reqlogdb.Open(path)
	if err != nil {
		return nil, err
	}
	l := &SQLiteRequestLogger{store: store, queue: make(chan *reqlogdb.Record, sqliteQueueSize)}
	go l.writer()
	log.Infof("request log: writing to SQLite database %s", path)
	return l, nil
}

func (l *SQLiteRequestLogger) writer() {
	for r := range l.queue {
		if err := l.store.Insert(r); err != nil {
			log.Errorf("request log: %v", err)
		}
	}
}

func (l *SQLiteRequestLogger) enqueue(r *reqlogdb.Record) {
	select {
	case l.queue <- r:
	default:
		log.Warnf("request log: writer queue full, dropped %s %s", r.Method, r.URL)
	}
}

// IsEnabled always returns true; the database logger has no off switch.
func (l *SQLiteRequestLogger) IsEnabled() bool { return true }

// LogRequest records a finished non-streaming request.
func (l *SQLiteRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, websocketTimeline, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	if decoded, err := (&FileRequestLogger{}).decompressResponse(responseHeaders, response); err == nil {
		response = decoded
	}
	if len(response) == 0 {
		response = websocketTimeline
	}
	l.enqueue(newSQLiteRecord(url, method, requestHeaders, body, statusCode, response, apiRequest, apiResponse, apiWebsocketTimeline, apiResponseErrors, requestID, requestTimestamp))
	return nil
}

// LogStreamingRequest returns a writer that collects the stream and records it on Close.
func (l *SQLiteRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (StreamingLogWriter, error) {
	return &sqliteStreamWriter{logger: l, url: url, method: method, headers: headers, body: bytes.Clone(body), requestID: requestID, start: time.Now()}, nil
}

type sqliteStreamWriter struct {
	logger                  *SQLiteRequestLogger
	url, method, requestID  string
	headers                 map[string][]string
	body                    []byte
	start                   time.Time
	mu                      sync.Mutex
	response                bytes.Buffer
	status                  int
	apiRequest, apiResponse []byte
	apiWebsocketTimeline    []byte
	apiErrors               []*interfaces.ErrorMessage
	closed                  bool
}

func (w *sqliteStreamWriter) WriteChunkAsync(chunk []byte) {
	w.mu.Lock()
	w.response.Write(chunk)
	w.mu.Unlock()
}

func (w *sqliteStreamWriter) WriteStatus(status int, _ map[string][]string) error {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()
	return nil
}

func (w *sqliteStreamWriter) WriteAPIRequest(b []byte) error {
	w.mu.Lock()
	w.apiRequest = append(w.apiRequest, b...)
	w.mu.Unlock()
	return nil
}

func (w *sqliteStreamWriter) WriteAPIResponse(b []byte) error {
	w.mu.Lock()
	w.apiResponse = append(w.apiResponse, b...)
	w.mu.Unlock()
	return nil
}

func (w *sqliteStreamWriter) WriteAPIWebsocketTimeline(b []byte) error {
	w.mu.Lock()
	w.apiWebsocketTimeline = append(w.apiWebsocketTimeline, b...)
	w.mu.Unlock()
	return nil
}

func (w *sqliteStreamWriter) WriteAPIResponseErrors(errs []*interfaces.ErrorMessage) error {
	w.mu.Lock()
	w.apiErrors = append(w.apiErrors, errs...)
	w.mu.Unlock()
	return nil
}

func (w *sqliteStreamWriter) SetFirstChunkTimestamp(time.Time) {}

func (w *sqliteStreamWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.logger.enqueue(newSQLiteRecord(w.url, w.method, w.headers, w.body, w.status, w.response.Bytes(), w.apiRequest, w.apiResponse, w.apiWebsocketTimeline, w.apiErrors, w.requestID, w.start))
	return nil
}

func newSQLiteRecord(rawURL, method string, headers map[string][]string, body []byte, status int, response, apiRequest, apiResponse, apiWebsocketTimeline []byte, apiErrors []*interfaces.ErrorMessage, requestID string, start time.Time) *reqlogdb.Record {
	if start.IsZero() {
		start = time.Now()
	}
	r := &reqlogdb.Record{
		Time:      start,
		Duration:  time.Since(start),
		RequestID: requestID,
		Method:    method,
		URL:       maskURL(rawURL),
		Status:    status,
		Model:     ojson.GetBytes(body, "model").String(),
		KeyHash:   clientKeyHash(rawURL, headers),
		Headers:   headersJSON(headers),
		Body:      body,
		Response:  bytes.Clone(response),
	}
	// Upstream traffic repeats the client request on success; keep it only for failures.
	if status >= 400 || len(apiErrors) > 0 {
		var b bytes.Buffer
		section := func(name string, data []byte) {
			if len(data) > 0 {
				fmt.Fprintf(&b, "=== %s ===\n%s\n\n", name, data)
			}
		}
		section("API REQUEST", apiRequest)
		section("API RESPONSE", apiResponse)
		section("API WEBSOCKET TIMELINE", apiWebsocketTimeline)
		for _, e := range apiErrors {
			if e != nil && e.Error != nil {
				fmt.Fprintf(&b, "=== API ERROR %d ===\n%v\n\n", e.StatusCode, e.Error)
			}
		}
		r.Upstream = b.Bytes()
	}
	return r
}

// clientKeyHash returns the sha256 hex of the client key, the same hash the
// management dashboard uses for key aliases.
func clientKeyHash(rawURL string, headers map[string][]string) string {
	key := ""
	for name, values := range headers {
		if len(values) == 0 {
			continue
		}
		switch strings.ToLower(name) {
		case "authorization":
			key = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(values[0], "Bearer "), "bearer "))
		case "x-api-key", "x-goog-api-key":
			key = strings.TrimSpace(values[0])
		}
		if key != "" {
			break
		}
	}
	if key == "" {
		if u, err := url.Parse(rawURL); err == nil {
			key = u.Query().Get("key")
		}
	}
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func maskURL(rawURL string) string {
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		return rawURL[:i+1] + util.MaskSensitiveQuery(rawURL[i+1:])
	}
	return rawURL
}

// headersJSON renders headers as a JSON object with sorted keys and secrets masked.
func headersJSON(headers map[string][]string) []byte {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []byte{'{'}
	for i, name := range names {
		if i > 0 {
			out = append(out, ',')
		}
		out = ojson.AppendJSONString(out, name)
		out = append(out, ':')
		out = ojson.AppendJSONString(out, util.MaskSensitiveHeaderValue(name, strings.Join(headers[name], ", ")))
	}
	return append(out, '}')
}
