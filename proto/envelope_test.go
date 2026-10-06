package proto

import (
	"strings"
	"testing"
)

func TestSupported(t *testing.T) {
	cases := map[int]bool{0: false, 1: true, 2: true, 3: false}
	for v, want := range cases {
		if got := Supported(v); got != want {
			t.Errorf("Supported(%d) = %v, want %v", v, got, want)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	type payload struct {
		A string `json:"a"`
	}
	env, err := New("hello", payload{A: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if env.V != Version || env.Type != "hello" || len(env.ID) != 26 || env.TS.IsZero() {
		t.Fatalf("bad envelope: %+v", env)
	}
	b, err := Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	if err := got.Unmarshal(&p); err != nil {
		t.Fatal(err)
	}
	if got.ID != env.ID || p.A != "x" {
		t.Fatalf("round trip mismatch: %+v %+v", got, p)
	}
}

func TestReplyEchoesID(t *testing.T) {
	req, _ := New("ping", nil)
	rep, err := Reply(req, "pong", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ID != req.ID || rep.Type != "pong" {
		t.Fatalf("reply did not echo id: %+v", rep)
	}
}

func TestDecodeRejectsOversize(t *testing.T) {
	big := []byte(`{"v":1,"id":"x","type":"t","payload":"` + strings.Repeat("a", MaxMessageSize) + `"}`)
	if _, err := Decode(big); err != ErrMessageTooLarge {
		t.Fatalf("want ErrMessageTooLarge, got %v", err)
	}
}

func TestDecodeRejectsMissingType(t *testing.T) {
	if _, err := Decode([]byte(`{"v":1,"id":"x"}`)); err == nil {
		t.Fatal("want error for missing type")
	}
}
