package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ApplicationReader observes deployments without reloading or modifying a node.
type ApplicationReader interface {
	Application(context.Context, Target) (Application, error)
}

type ApplicationVersion struct {
	Version string `json:"version"`
	Prefix  string `json:"prefix"`
}

type Application struct {
	RuntimeGeneration                          string
	Loaded                                     *ApplicationVersion
	LocalGeneration                            uint64
	ResidentCells, PendingCells, SwappingCells int64
	ReceivedAt                                 time.Time
}

func (c *Client) Application(ctx context.Context, target Target) (Application, error) {
	data, code, err := c.call(ctx, target, http.MethodGet, "/state", nil)
	if err != nil {
		return Application{}, err
	}
	if code != http.StatusOK {
		return Application{}, errors.New("unexpected application state status")
	}
	return decodeApplication(data, target.Generation, time.Now())
}

// Decode the existing deployment object. Application generations are local to
// each process and must never be compared between nodes. The actor census and
// current generation are sampled separately by celld; this is an observation,
// not an atomic rollout-completion receipt or a view of the S3 deployment pointer.
func decodeApplication(data []byte, generation string, received time.Time) (Application, error) {
	m, err := decodeObject(data)
	if err != nil {
		return Application{}, err
	}
	raw, ok := m["deployment"]
	if !ok {
		return Application{}, ErrUnsupported
	}
	deployment, err := decodeObject(raw)
	if err != nil {
		return Application{}, err
	}
	out := Application{Loaded: &ApplicationVersion{}, ReceivedAt: received}
	var cells map[string]json.RawMessage
	for k, p := range map[string]any{
		"version": &out.Loaded.Version, "prefix": &out.Loaded.Prefix,
		"generation": &out.LocalGeneration, "swapping": &out.SwappingCells, "cells": &cells,
	} {
		if err := required(deployment, k, p); err != nil {
			return Application{}, err
		}
	}
	if out.Loaded.Version == "" || len(out.Loaded.Version) > 256 || out.Loaded.Prefix == "" || len(out.Loaded.Prefix) > 1024 || out.LocalGeneration == 0 || out.SwappingCells < 0 || out.SwappingCells > 1_000_000_000 {
		return Application{}, errors.New("invalid deployment observation")
	}
	for cell := range cells {
		var local uint64
		if err := required(cells, cell, &local); err != nil {
			return Application{}, err
		}
		if local > out.LocalGeneration {
			return Application{}, errors.New("inconsistent deployment generations")
		}
		if local != out.LocalGeneration {
			out.PendingCells++
		}
	}
	out.ResidentCells = int64(len(cells))
	// Older runtimes need not expose the lifecycle extension for this read. When
	// available, retain the identity as diagnostics and honor an explicit target.
	if raw, ok := m["shutdown"]; ok {
		var shutdown struct {
			RuntimeGeneration string `json:"runtime_generation"`
		}
		if err := json.Unmarshal(raw, &shutdown); err != nil {
			return Application{}, err
		}
		out.RuntimeGeneration = shutdown.RuntimeGeneration
	}
	if len(out.RuntimeGeneration) > 128 || (generation != "" && generation != out.RuntimeGeneration) {
		return Application{}, ErrIdentity
	}
	return out, nil
}

// Fresh measures the operator's HTTP observation, not deployment-pointer age.
func (a Application) Fresh(now, started time.Time, maxAge time.Duration) bool {
	return a.Loaded != nil && fresh(a.ReceivedAt, now, maxAge) && !a.ReceivedAt.Before(started)
}
