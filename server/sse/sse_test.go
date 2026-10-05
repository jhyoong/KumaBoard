package sse

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubscribePublish(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	defer cancel()
	b.Publish("device", map[string]string{"name": "a"})
	select {
	case ev := <-ch:
		if ev.Name != "device" {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestServeHTTPStreams(t *testing.T) {
	b := New()
	srv := httptest.NewServer(b)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	time.Sleep(50 * time.Millisecond) // let the subscription register
	b.Publish("device", map[string]string{"name": "a"})
	r := bufio.NewReader(resp.Body)
	var lines []string
	for len(lines) < 2 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, ":") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if lines[0] != "event: device" || lines[1] != `data: {"name":"a"}` {
		t.Fatalf("got %q", lines)
	}
}
