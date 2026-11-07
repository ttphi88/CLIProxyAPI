// Command reqlog reads the SQLite request log written when CPA_REQUEST_LOG_DB is set.
//
//	reqlog <db> list [n]                                     latest n requests (default 20)
//	reqlog <db> show <id> [request|response|upstream|headers] one stored part (default request)
//	reqlog <db> import <file>                                  load {"request":...,"response":"..."} JSON lines (testing)
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/reqlogdb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "reqlog:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: reqlog <db> list [n] | show <id> [request|response|upstream|headers] | import <file>")
	}
	store, err := reqlogdb.Open(args[0])
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	switch args[1] {
	case "list":
		n := 20
		if len(args) > 2 {
			if n, err = strconv.Atoi(args[2]); err != nil {
				return fmt.Errorf("list: count %q is not a number", args[2])
			}
		}
		rows, errQuery := store.DB().Query(`SELECT id, ts, status, duration_ms, model, substr(key_hash, 1, 8), method, url, length(body_parts) / ? FROM requests ORDER BY id DESC LIMIT ?`, reqlogdb.HashSize, n)
		if errQuery != nil {
			return errQuery
		}
		defer func() { _ = rows.Close() }()
		fmt.Println("id\ttime\tstatus\tms\tmodel\tkey\tparts\tmethod url")
		for rows.Next() {
			var (
				id, dur, parts int64
				status         int
				ts, model, key string
				method, url    string
			)
			if err = rows.Scan(&id, &ts, &status, &dur, &model, &key, &method, &url, &parts); err != nil {
				return err
			}
			fmt.Printf("%d\t%s\t%d\t%d\t%s\t%s\t%d\t%s %s\n", id, ts, status, dur, model, key, parts, method, url)
		}
		return rows.Err()
	case "show":
		if len(args) < 3 {
			return fmt.Errorf("show: missing id")
		}
		id, errID := strconv.ParseInt(args[2], 10, 64)
		if errID != nil {
			return fmt.Errorf("show: id %q is not a number", args[2])
		}
		r, errGet := store.Get(id)
		if errGet != nil {
			return errGet
		}
		part := "request"
		if len(args) > 3 {
			part = args[3]
		}
		out := map[string][]byte{"request": r.Body, "response": r.Response, "upstream": r.Upstream, "headers": r.Headers}[part]
		if out == nil && part != "request" && part != "response" && part != "upstream" && part != "headers" {
			return fmt.Errorf("show: unknown part %q", part)
		}
		_, err = os.Stdout.Write(out)
		return err
	case "import":
		if len(args) < 3 {
			return fmt.Errorf("import: missing file")
		}
		f, errOpen := os.Open(args[2])
		if errOpen != nil {
			return errOpen
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		n := 0
		for sc.Scan() {
			line := sc.Bytes()
			body := []byte(ojson.GetBytes(line, "request").Raw)
			r := &reqlogdb.Record{Time: time.Now(), Method: "POST", URL: "/v1/messages", Status: 200,
				Model: ojson.GetBytes(body, "model").String(), Body: body, Response: []byte(ojson.GetBytes(line, "response").String())}
			if err = store.Insert(r); err != nil {
				return err
			}
			n++
		}
		fmt.Println("imported", n)
		return sc.Err()
	}
	return fmt.Errorf("unknown command %q", args[1])
}
