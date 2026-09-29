package lsp

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestMergeRegisteredCapability covers the issue #37 contract: a dynamic
// registration must MERGE into the stored capability value, never overwrite an
// existing options object with bool true.
func TestMergeRegisteredCapability(t *testing.T) {
	tests := []struct {
		name     string
		existing any
		opts     json.RawMessage
		want     any
	}{
		{
			name:     "nil existing with no opts sets true",
			existing: nil,
			opts:     nil,
			want:     true,
		},
		{
			name:     "existing options map kept unchanged when no opts",
			existing: map[string]any{"prepareProvider": true},
			opts:     nil,
			want:     map[string]any{"prepareProvider": true},
		},
		{
			name:     "existing options map merged with incoming opts",
			existing: map[string]any{"prepareProvider": true},
			opts:     json.RawMessage(`{"workDoneProgress":true}`),
			want:     map[string]any{"prepareProvider": true, "workDoneProgress": true},
		},
		{
			name:     "nil existing with opts stores incoming map",
			existing: nil,
			opts:     json.RawMessage(`{"prepareProvider":true}`),
			want:     map[string]any{"prepareProvider": true},
		},
		{
			name:     "existing bool true is replaced by incoming opts map",
			existing: true,
			opts:     json.RawMessage(`{"documentSelector":1}`),
			want:     map[string]any{"documentSelector": float64(1)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeRegisteredCapability(tt.existing, "renameProvider", tt.opts)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mergeRegisteredCapability() = %#v, want %#v", got, tt.want)
			}
			// Never collapse an options map into bool true.
			if _, wantMap := tt.want.(map[string]any); wantMap {
				if _, gotMap := got.(map[string]any); !gotMap {
					t.Fatalf("expected map[string]any result, got %T (%v)", got, got)
				}
			}
		})
	}
}

// TestClientRegisterCapability_MergesOptions drives the full wire path: a
// server-initiated client/registerCapability request (the exact shape emitted
// by mql-lsp-server v2.4.2) must be answered with a null result and must merge
// its registerOptions into the capability map rather than overwriting it.
func TestClientRegisterCapability_MergesOptions(t *testing.T) {
	c, serverW, clientR := newTestClient(t)

	// Seed the capability as mql-lsp-server v2.4.2 declares it in initialize:
	// renameProvider with prepareProvider. The registration must merge its
	// options over this map without dropping prepareProvider.
	c.capsMu.Lock()
	c.capabilities["renameProvider"] = map[string]any{"prepareProvider": true}
	c.capsMu.Unlock()

	id := 7
	if err := writeMsg(serverW, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "client/registerCapability",
		"params": map[string]any{
			"registrations": []map[string]any{
				{
					"id":     "rename-reg",
					"method": "textDocument/rename",
					"registerOptions": map[string]any{
						"documentSelector": []map[string]any{
							{"language": "mql"},
						},
						"workDoneProgress": true,
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp := readNextMsg(t, clientR)
	if resp == nil {
		t.Fatal("expected response from client")
	}
	if resp["id"] != float64(id) {
		t.Errorf("response id = %v, want %d", resp["id"], id)
	}
	if result, ok := resp["result"]; !ok || result != nil {
		t.Errorf("response result = %v, want null", result)
	}

	// The registration must have merged into a map, not replaced it with true.
	c.capsMu.RLock()
	capVal := c.capabilities["renameProvider"]
	c.capsMu.RUnlock()
	capMap, ok := capVal.(map[string]any)
	if !ok {
		t.Fatalf("renameProvider = %T (%v), want map[string]any", capVal, capVal)
	}
	if capMap["workDoneProgress"] != true {
		t.Errorf("workDoneProgress = %v, want true", capMap["workDoneProgress"])
	}
	if _, ok := capMap["documentSelector"]; !ok {
		t.Error("documentSelector missing from merged capability")
	}
	if capMap["prepareProvider"] != true {
		t.Errorf("prepareProvider = %v, want true (existing option must survive the merge)", capMap["prepareProvider"])
	}
}
