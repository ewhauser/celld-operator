// Package v041 binds the shared protocol-5 evidence codec to celld v0.4.1.
// Source comparison confirms NodeLeaseWire/NodeLogWire and the required /state
// fields have the same shape and sealing meaning as v0.5.0. Codec compatibility
// does not imply permission to mix runtime versions or to roll back stored data.
package v041

import (
	"errors"

	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

const (
	Commit = "10cb1303dac710dcb3b557e318e08c855261f68b"
	Image  = "ghcr.io/denoland/celld@sha256:ce8bbc3c26a16c9ee00e3ce0501f36bfea2663b5af8285a08fc16a54568060a5"
)

func New(image string) (*v050.Adapter, error) {
	if image != Image {
		return nil, errors.New("unsupported v0.4.1 runtime image")
	}
	return &v050.Adapter{}, nil
}
