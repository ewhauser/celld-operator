// Package launcher supervises the pinned, unmodified celld invocation.
package launcher

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type Request struct {
	Nonce, Operation, Generation string
	NotAfterMS                   int64
	Handoff                      *Handoff `json:",omitempty"`
}
type State struct {
	PodUID, Node, Host, Invocation, Generation, Phase, Operation, Error string
	PID                                                                 int
	BootID, DiskID, PreviousHost                                        string `json:",omitempty"`
	RestartDenied                                                       bool   `json:",omitempty"`
}
type Response struct {
	Nonce string
	State State
	MAC   string
}

func Nonce() string { var b [32]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func MAC(key []byte, domain string, value any) string {
	b, _ := json.Marshal(value)
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(domain + "\x00"))
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func Verify(key []byte, domain string, value any, signature string) bool {
	want, e := hex.DecodeString(MAC(key, domain, value))
	if e != nil {
		return false
	}
	got, e := hex.DecodeString(signature)
	return e == nil && hmac.Equal(want, got)
}

// Handoff authorizes one waiting invocation, never an arbitrary future opener.
// The controller obtains positive predecessor termination before sending it.
type Handoff struct {
	Invocation, Generation, PodUID, Host, BootID, DiskID, PreviousHost string `json:",omitempty"`
}
