package wake

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestMagicPacket(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	p := MagicPacket(mac)
	if len(p) != 102 || !bytes.Equal(p[:6], bytes.Repeat([]byte{0xff}, 6)) {
		t.Fatalf("bad packet len=%d head=%x", len(p), p[:6])
	}
	for i := 0; i < 16; i++ {
		if !bytes.Equal(p[6+i*6:12+i*6], mac) {
			t.Fatalf("repetition %d wrong", i)
		}
	}
}

func TestSendDeliversThreePackets(t *testing.T) {
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.LocalAddr().(*net.UDPAddr).Port
	s := &Sender{SourceIP: net.IPv4(127, 0, 0, 1), Broadcast: "127.0.0.1", Port: port}
	if err := s.Send("aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 200)
	for i := 0; i < 3; i++ {
		ln.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := ln.ReadFromUDP(buf)
		if err != nil || n != 102 {
			t.Fatalf("packet %d: n=%d err=%v", i, n, err)
		}
	}
}

func TestSendRejectsBadMAC(t *testing.T) {
	s := &Sender{SourceIP: net.IPv4(127, 0, 0, 1), Broadcast: "127.0.0.1", Port: 9}
	if err := s.Send("not-a-mac"); err == nil {
		t.Fatal("bad mac accepted")
	}
}

func TestSendMACFormats(t *testing.T) {
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.LocalAddr().(*net.UDPAddr).Port
	s := &Sender{SourceIP: net.IPv4(127, 0, 0, 1), Broadcast: "127.0.0.1", Port: port}
	want := MagicPacket(net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	buf := make([]byte, 200)
	for _, mac := range []string{"AA:BB:CC:DD:EE:FF", "aabb.ccdd.eeff", "aa-bb-cc-dd-ee-ff", "AA-BB-CC-DD-EE-FF"} {
		if err := s.Send(mac); err != nil {
			t.Fatalf("%s: %v", mac, err)
		}
		for i := 0; i < 3; i++ {
			ln.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, _, err := ln.ReadFromUDP(buf)
			if err != nil || !bytes.Equal(buf[:n], want) {
				t.Fatalf("%s packet %d: n=%d err=%v", mac, i, n, err)
			}
		}
	}
	for _, mac := range []string{"aa:bb:cc:dd", "aa:bb:cc:dd:ee:ff:00:11", ""} {
		if err := s.Send(mac); err == nil {
			t.Errorf("%q accepted", mac)
		}
	}
	ln.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := ln.ReadFromUDP(buf); err == nil {
		t.Fatalf("rejected MAC still sent %d bytes", n)
	}
}
