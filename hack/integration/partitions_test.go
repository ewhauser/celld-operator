package main

import "testing"

func TestPartitionProofRequiresCurlExit(t *testing.T) {
	for out, want := range map[string]int{"CURL_EXIT:0\n": 0, "CURL_EXIT:7\n": 7, "CURL_EXIT:28\n": 28} {
		if got, ok := curlExit(out); !ok || got != want {
			t.Fatalf("curlExit(%q) = %d, %v; want %d", out, got, ok, want)
		}
	}
	for _, out := range []string{"", "error: pod not found", "command terminated with exit code 1", "CURL_EXIT:", "CURL_EXIT:0\nCURL_EXIT:28", "CURL_EXIT:-1", "CURL_EXIT:256"} {
		if _, ok := curlExit(out); ok {
			t.Fatalf("accepted failed or ambiguous network proof: %q", out)
		}
	}
}
