package reqlogdb

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func TestSplitJoinRoundTrip(t *testing.T) {
	cases := []string{
		`{"model":"claude-opus-5-5","system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"yo"}]}],"tools":[{"name":"Bash"}],"stream":true}`,
		`{"model":"gpt-5.5","instructions":"be brief","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
		`{"model":"gpt-5.5","input":"plain string input"}`,
		`{"model":"x","messages":[]}`,
		`{"contents":[{"parts":[{"text":"a"}]}],"systemInstruction":{"parts":[{"text":"s"}]}}`,
		`not json`,
		`[1,2,3]`,
		``,
	}
	for _, c := range cases {
		head, parts := Split([]byte(c))
		got, err := Join(head, parts)
		if err != nil {
			t.Fatalf("Join(%s): %v", c, err)
		}
		if !bytes.Equal(got, []byte(c)) {
			t.Fatalf("round trip changed body\nwant %s\ngot  %s", c, got)
		}
	}
}

func TestStoreDeduplicatesHistory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "log.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	first := []byte(`{"model":"m","system":"sys","messages":[{"role":"user","content":"one"}]}`)
	second := []byte(`{"model":"m","system":"sys","messages":[{"role":"user","content":"one"},{"role":"assistant","content":"two"}]}`)
	for _, body := range [][]byte{first, second} {
		if err = s.Insert(&Record{Time: time.Now(), Method: "POST", URL: "/v1/messages", Status: 200, Body: body, Response: []byte("event: x\n\n")}); err != nil {
			t.Fatal(err)
		}
	}
	var blobs int
	if err = s.DB().QueryRow("SELECT count(*) FROM blobs").Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 3 { // "sys", message one, message two
		t.Fatalf("blobs = %d, want 3", blobs)
	}
	r, err := s.Get(2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.Body, second) || string(r.Response) != "event: x\n\n" {
		t.Fatalf("Get(2) = %s / %q", r.Body, r.Response)
	}
}
