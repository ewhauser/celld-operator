// Package catalog records immutable released runtimes and directional contracts.
package catalog

import (
	"errors"

	v041 "github.com/ewhauser/celld-operator/internal/runtime/v041"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

type Release struct{ Version, Image, Commit string }

func Lookup(image string) (Release, bool) {
	switch image {
	case v041.Image:
		return Release{"v0.4.1", v041.Image, v041.Commit}, true
	case v050.Image:
		return Release{"v0.5.0", v050.Image, v050.Commit}, true
	default:
		return Release{}, false
	}
}
func New(image string) (*v050.Adapter, error) {
	switch image {
	case v041.Image:
		return v041.New(image)
	case v050.Image:
		return v050.New(image)
	default:
		return nil, errors.New("runtime image lacks a source-qualified adapter")
	}
}

// StoppedUpgrade is directional. The upstream v0.5.0 release explicitly requires
// the entire v0.4.1 fleet stopped first. No reverse storage-format contract exists.
func StoppedUpgrade(from, to string) bool { return from == v041.Image && to == v050.Image }
