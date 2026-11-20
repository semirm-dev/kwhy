package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestServeMCP(t *testing.T) {
	call := func(id int, name string, args map[string]any) string {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return string(b)
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		call(3, "kwhy_value", map[string]any{"chart": mini, "resource": "deploy/api", "path": ".spec.replicas", "values": []string{prod}}),
		call(4, "kwhy_value", map[string]any{"chart": mini, "resource": "deploy/nope", "path": ".spec.replicas"}),
		call(5, "kwhy_nope", nil),
		`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`,
	}, "\n")

	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}

	type response struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Tools           []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	var rs []response
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r response
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}

	// The notification gets no answer: six requests, six responses.
	if len(rs) != 6 {
		t.Fatalf("%d responses, want 6: %+v", len(rs), rs)
	}
	if rs[0].Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("initialize: version %q, want the client's", rs[0].Result.ProtocolVersion)
	}
	if len(rs[1].Result.Tools) != 3 {
		t.Errorf("tools/list: %+v", rs[1].Result.Tools)
	}
	if r := rs[2].Result; r.IsError || !strings.Contains(r.Content[0].Text, `"schema": "kwhy.value/v1"`) || !strings.Contains(r.Content[0].Text, `"kind": "direct"`) {
		t.Errorf("kwhy_value: %+v", r)
	}
	if r := rs[3].Result; !r.IsError || !strings.Contains(r.Content[0].Text, "no deploy/nope in the render") {
		t.Errorf("kwhy_value error: %+v", r)
	}
	if rs[4].Error == nil || rs[5].Error == nil || rs[5].Error.Code != -32601 {
		t.Errorf("unknown tool %+v, unknown method %+v", rs[4].Error, rs[5].Error)
	}
}
