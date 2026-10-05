// Package proto defines the wire protocol shared by kumaboard and kuma-agent.
// It has no dependencies on either binary.
package proto

// Version is the protocol version this build speaks.
const Version = 1

// MinSupported is the oldest protocol version the server accepts.
// The rule is N and N-1: MinSupported is never more than one behind Version.
const MinSupported = 1

// Supported reports whether a peer's protocol version is accepted.
func Supported(v int) bool {
	return v >= MinSupported && v <= Version
}
