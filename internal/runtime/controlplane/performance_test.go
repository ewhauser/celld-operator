package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Include the otherwise unrelated deployment census in capacity reads: it is
// part of the same /state body and grows with the node's resident population.
func benchmarkState(b *testing.B, cells int) []byte {
	b.Helper()
	data, err := os.ReadFile("testdata/state.json")
	if err != nil {
		b.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		b.Fatal(err)
	}
	state["node_load"].(map[string]any)["sampled_ms"] = time.Now().UnixMilli()
	census := make(map[string]uint64, cells)
	for i := range cells {
		census[fmt.Sprintf("cell-%06d", i)] = uint64(i%2 + 1)
	}
	state["deployment"] = map[string]any{"version": "v2", "prefix": "deploy/v2/", "generation": 2, "swapping": 0, "cells": census}
	data, err = json.Marshal(state)
	if err != nil {
		b.Fatal(err)
	}
	return data
}

func BenchmarkDecodeApplication(b *testing.B) {
	for _, cells := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprintf("cells=%d", cells), func(b *testing.B) {
			data, now := benchmarkState(b, cells), time.Now()
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := decodeApplication(data, now); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkParseLoad(b *testing.B) {
	for _, cells := range []int{0, 1000} {
		b.Run(fmt.Sprintf("cells=%d", cells), func(b *testing.B) {
			data, now := benchmarkState(b, cells), time.Now()
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := parseLoad(data, now, now, time.Hour); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type benchmarkTransport struct{ data []byte }

func (t benchmarkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(t.data)), Header: http.Header{}}, nil
}

// The in-memory transport includes HTTP request construction/body reads while
// excluding network latency, so CPU and allocation profiles expose local work.
func BenchmarkClientObservation(b *testing.B) {
	for _, cells := range []int{0, 1000} {
		data := benchmarkState(b, cells)
		client := New(benchmarkTransport{data: data})
		b.Run(fmt.Sprintf("application/cells=%d", cells), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := client.Application(b.Context(), target); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("capacity/cells=%d", cells), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				state, err := client.State(b.Context(), target)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := state.Capacity(time.Hour); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
