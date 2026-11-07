# SQLite request log

Set `CPA_REQUEST_LOG_DB` to a file path and the proxy writes every request and response to that SQLite database instead of one text file per request in `logs/`. The `request-log` setting does not affect it; unset the variable to switch it off.

```bash
CPA_REQUEST_LOG_DB=/data/requests.sqlite ./cli-proxy-api --config config.yaml
```

## What is stored

One row in `requests` per proxied request (management endpoints are never logged):

- Time, duration, method, URL (sensitive query values masked), status, model.
- `key_hash`: sha256 hex of the client key, the same hash the dashboard uses for key aliases.
- Request headers as JSON, with authorization and key headers masked.
- The client's request body, deduplicated (below).
- The response exactly as sent to the client: the raw event stream for streamed requests.
- `upstream`: the request sent upstream, its response and errors, only when the request failed (status 400 or higher, or an upstream error). On success it repeats the client request.

Every blob is zstd-compressed.

## Deduplication

Clients resend the whole conversation on every turn. The logger cuts a JSON body into a head plus parts: `system`, `tools`, `instructions` and `systemInstruction` whole, and each element of `messages`, `input` and `contents`. Each part is stored once in `blobs`, keyed by its sha256 (first 16 bytes). A request row keeps the head and the list of part hashes. On 3,490 real Claude Code requests (1.38 GB of bodies) the database was 50 MB.

## Reading it

Bodies are compressed and split, so use the `reqlog` command:

```bash
go build -o reqlog ./cmd/reqlog
./reqlog requests.sqlite list 20
./reqlog requests.sqlite show 42            # request body, rebuilt byte for byte
./reqlog requests.sqlite show 42 response   # or: upstream, headers
```

Metadata columns are plain SQL: `sqlite3 requests.sqlite "SELECT model, count(*) FROM requests GROUP BY 1"`.

## Limits

- Writes run on one background goroutine with a queue of 32 requests. When the queue is full, records are dropped with a warning; requests are never slowed down.
- Records still in the queue when the process stops are lost.
- There is no automatic retention. Delete old rows with SQL; blobs no longer referenced stay until a cleanup removes them.
