package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// mcpVersion is the Model Context Protocol revision kwhy speaks when the client doesn't ask for one.
const mcpVersion = "2025-06-18"

// tool is one MCP tool: its schema, and how its arguments become a kwhy command line.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	argv func(a map[string]any) []string
}

var tools = []tool{
	{
		Name:        "kwhy_value",
		Description: "Where a rendered Helm value comes from: the values file and line that set a field of a rendered resource, and what it overrides.",
		InputSchema: schema([]string{"chart", "resource"}, map[string]any{
			"chart":     prop("string", "chart directory, .tgz, oci:// reference or repo/chart"),
			"resource":  prop("string", "kind/name of the rendered resource, e.g. deploy/api"),
			"path":      prop("string", "field path, e.g. .spec.template.spec.containers[name=api].image; omit for every field"),
			"values":    list("values files, later wins"),
			"set":       list("key=value overrides"),
			"release":   prop("string", "release name"),
			"namespace": prop("string", "namespace"),
		}),
		argv: func(a map[string]any) []string {
			args := []string{"value", "chart", s(a, "chart"), s(a, "resource")}
			if p := s(a, "path"); p != "" {
				args = append(args, p)
			} else {
				args = append(args, "--all")
			}
			return append(args, valueArgs(a)...)
		},
	},
	{
		Name:        "kwhy_change",
		Description: "What merging a git change does to running workloads: restarts, ordered rollouts, failures on immutable fields, ConfigMap changes pods won't see, data loss. Run in the git repo.",
		InputSchema: schema([]string{"range"}, map[string]any{
			"range":     prop("string", "base..head git refs, e.g. main..feature"),
			"chart":     prop("string", "chart directory in the repo"),
			"manifests": prop("string", "directory of plain YAML in the repo"),
			"config":    prop("string", "file listing several targets (default .kwhy.yaml)"),
			"apps":      prop("string", "directory with Argo CD Applications or Flux HelmReleases"),
			"values":    list("values files, later wins"),
			"set":       list("key=value overrides"),
			"context":   prop("string", "kube context for live replica counts and rollout estimates (read-only)"),
		}),
		argv: func(a map[string]any) []string {
			args := []string{"change", s(a, "range")}
			for _, f := range []string{"chart", "manifests", "config", "apps", "context"} {
				if v := s(a, f); v != "" {
					args = append(args, "--"+f, v)
				}
			}
			return append(args, valueArgs(a)...)
		},
	},
	{
		Name:        "kwhy_pod",
		Description: "Why a pod or workload isn't running: every failing scheduler check on every node for a Pending pod, or the crash, pull error, OOM kill, missing config, probe or controller problem for one that is placed.",
		InputSchema: schema([]string{"name"}, map[string]any{
			"name":      prop("string", "pod name, or deploy/x, sts/x, ds/x, job/x"),
			"namespace": prop("string", "namespace (default the context's)"),
			"context":   prop("string", "kube context (read-only)"),
			"logs":      prop("integer", "last N log lines of a crashed container (logs may contain secrets)"),
		}),
		argv: func(a map[string]any) []string {
			args := []string{"pod", s(a, "name")}
			for _, f := range []string{"namespace", "context"} {
				if v := s(a, f); v != "" {
					args = append(args, "--"+f, v)
				}
			}
			if n, ok := a["logs"].(float64); ok && n > 0 {
				args = append(args, "--logs", fmt.Sprint(int(n)))
			}
			return args
		},
	},
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve kwhy's commands to AI agents over the Model Context Protocol (stdio)",
		Args:  usageArgs(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveMCP(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// serveMCP answers JSON-RPC requests, one per line, until in ends. Notifications get no answer.
func serveMCP(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(out)

	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			if err := enc.Encode(rpcError(nil, -32700, "parse error")); err != nil {
				return err
			}
			continue
		}
		if req.ID == nil {
			continue // a notification, e.g. notifications/initialized
		}

		var resp any
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			resp = result(req.ID, map[string]any{
				"protocolVersion": or(p.ProtocolVersion, mcpVersion),
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "kwhy", "version": version},
			})
		case "ping":
			resp = result(req.ID, map[string]any{})
		case "tools/list":
			resp = result(req.ID, map[string]any{"tools": tools})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				resp = rpcError(req.ID, -32602, "invalid params")
				break
			}
			resp = callTool(req.ID, p.Name, p.Arguments)
		default:
			resp = rpcError(req.ID, -32601, "method not found: "+req.Method)
		}

		if err := enc.Encode(resp); err != nil {
			return err
		}
	}

	return sc.Err()
}

// callTool runs the tool's kwhy command with JSON output; a runtime or usage error is a tool error, not an RPC one.
func callTool(id json.RawMessage, name string, args map[string]any) any {
	for _, t := range tools {
		if t.Name != name {
			continue
		}

		var stdout, stderr bytes.Buffer
		code := run(append(t.argv(args), "-o", "json"), &stdout, &stderr)
		text := stdout.String()
		if code != 0 && code != 3 {
			text = strings.TrimSpace(stderr.String())
		}

		return result(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": code != 0 && code != 3,
		})
	}

	return rpcError(id, -32602, "unknown tool: "+name)
}

func result(id json.RawMessage, r any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": r}
}

func rpcError(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func schema(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "required": required, "properties": props}
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func list(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// valueArgs turns the values, set, release and namespace arguments into flags.
func valueArgs(a map[string]any) []string {
	var args []string
	for _, f := range strs(a["values"]) {
		args = append(args, "-f", f)
	}
	for _, kv := range strs(a["set"]) {
		args = append(args, "--set", kv)
	}
	for _, f := range []string{"release", "namespace"} {
		if v := s(a, f); v != "" {
			args = append(args, "--"+f, v)
		}
	}

	return args
}

func s(a map[string]any, k string) string {
	v, _ := a[k].(string)
	return v
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		if str, ok := x.(string); ok {
			out = append(out, str)
		}
	}

	return out
}

func or(v, def string) string {
	if v == "" {
		return def
	}

	return v
}
