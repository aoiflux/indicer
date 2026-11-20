package api

import (
	"context"
	"encoding/json"
	"testing"

	"indicer/internal/core/jsonbridge"
)

// inMemoryOps are the operations a fuzzed request may run: they touch no
// file. The others work on paths the request names, which a fuzzer must not
// choose.
var inMemoryOps = map[string]bool{"capabilities": true, "compress_bytes": true, "decompress_bytes": true, "get_progress": true, "cancel_task": true}

// FuzzDispatchJSON sends the dispatcher requests it did not expect: every
// answer is a JSON envelope with a status, never a panic or an empty string.
func FuzzDispatchJSON(f *testing.F) {
	f.Add(`{"version":"v1","product":"compression","operation":"capabilities"}`)
	f.Add(`{"version":"v1","product":"compression","operation":"compress_bytes","params":{"data_base64":"aGVsbG8=","level":"best"}}`)
	f.Add(`{"version":"v1","product":"compression","operation":"decompress_bytes","params":{"data_base64":"KLUv/QBYIQAAaGVsbG8=","max_output_bytes":10}}`)
	f.Add(`{"version":"v1","product":"compression","operation":"get_progress","params":{"task_id":"x"}}`)
	f.Add(`{"product":"nope"}`)
	f.Add(`{`)
	d := NewDefaultDispatcher(Options{})
	f.Fuzz(func(t *testing.T, input string) {
		if req, err := jsonbridge.DecodeRequest(input); err == nil && !inMemoryOps[req.Operation] {
			return
		}
		out := d.DispatchJSON(context.Background(), input)
		var env struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil || (env.Status != "ok" && env.Status != "error") {
			t.Fatalf("answer %q to %q: %v", out, input, err)
		}
	})
}
