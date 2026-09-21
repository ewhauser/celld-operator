// Package launcher supervises one exact celld invocation and captures its strict shutdown result.
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
	// NotAfterMS bounds request replay; DeadlineMS bounds the first accepted
	// operation. Retries cannot extend that operation deadline.
	NotAfterMS, DeadlineMS int64
}
type State struct {
	PodUID, Node, Host, Invocation, Generation, Phase, Operation, Error string
	PID                                                                 int
	BootID, DiskID                                                      string
	DeadlineMS                                                          int64
	Removal                                                             RemovalResult
	ChildExited, InheritedLockReleased, RestartDenied                   bool
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

// RemovalResult is the one current runtime operation, captured through the typed
// client. It is volatile: a disk restart-deny marker never reconstructs it.
// Mode is fixed by the strict request and validated by the control-plane client.
type RemovalResult struct {
	Operation, Generation, Mode, Phase, Blocker string
	ControlOnly, DataSafe                       bool
}

func (s State) RuntimeDataSafe() bool {
	r := s.Removal
	return s.Operation != "" && s.Generation != "" && r.Operation == s.Operation &&
		r.Generation == s.Generation && r.Mode == "remove-disk" && r.Phase == "data_safe" &&
		r.ControlOnly && r.Blocker == "" && r.DataSafe
}

// RemovalReady requires independent runtime, process, lock and restart proofs.
// Callers must also bind the response to the expected Kubernetes invocation.
func (s State) RemovalReady() bool {
	return s.Phase == "Stopped" && s.RuntimeDataSafe() && s.ChildExited &&
		s.InheritedLockReleased && s.RestartDenied
}
