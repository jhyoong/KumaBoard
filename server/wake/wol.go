package wake

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// Sender delivers Wake-on-LAN magic packets from a specific interface.
type Sender struct {
	Interface string
	SourceIP  net.IP
	Broadcast string
	Port      int
}

// NewSender creates a Sender bound to the first IPv4 address of iface.
func NewSender(iface, broadcast string) (*Sender, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("wake: interface %q: %w", iface, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	bcast := net.ParseIP(broadcast)
	if bcast == nil || bcast.To4() == nil {
		return nil, fmt.Errorf("wake: broadcast %q is not a valid IPv4 address", broadcast)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return &Sender{Interface: iface, SourceIP: ipn.IP.To4(), Broadcast: broadcast, Port: 9}, nil
		}
	}
	return nil, fmt.Errorf("wake: interface %q has no IPv4 address", iface)
}

// MagicPacket builds the 102-byte WOL payload: six 0xFF bytes followed by
// the target MAC repeated 16 times.
func MagicPacket(mac net.HardwareAddr) []byte {
	p := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		p = append(p, 0xff)
	}
	for i := 0; i < 16; i++ {
		p = append(p, mac...)
	}
	return p
}

// Send transmits three magic packets for macStr to the configured broadcast
// address with a short pause between each.
func (s *Sender) Send(macStr string) error {
	mac, err := net.ParseMAC(macStr)
	if err != nil || len(mac) != 6 {
		return errors.New("wake: invalid MAC address")
	}
	port := s.Port
	if port == 0 {
		port = 9
	}
	bcast := net.ParseIP(s.Broadcast)
	if bcast == nil {
		return errors.New("wake: invalid broadcast address")
	}
	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: s.SourceIP}, &net.UDPAddr{IP: bcast, Port: port})
	if err != nil {
		return fmt.Errorf("wake: dial: %w", err)
	}
	defer conn.Close()
	packet := MagicPacket(mac)
	for i := 0; i < 3; i++ {
		if _, err := conn.Write(packet); err != nil {
			return fmt.Errorf("wake: send: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
