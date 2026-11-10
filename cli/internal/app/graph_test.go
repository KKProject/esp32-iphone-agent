package app

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/navgraph"
)

func TestGraphCLIAndMCPWorkflowWithoutBoard(t *testing.T) {
	a := &App{Dir: t.TempDir(), Err: io.Discard}
	call := func(op, input string) out {
		t.Helper()
		a.In = strings.NewReader(input)
		o := run(t, a, "graph", op, "--input", "-")
		if o.Status != "ok" {
			t.Fatalf("%s: %+v", op, o)
		}
		return o
	}
	home := call("node-save", `{"app":"58","build":"1","profile":"phone","key":"home.guest","name":"首页"}`).Data["result"].(map[string]any)["id"].(string)
	services := call("node-save", `{"app":"58","build":"1","profile":"phone","key":"services.guest","name":"本地服务"}`).Data["result"].(map[string]any)["id"].(string)
	result, err := a.graphCall("graph_edge_save", map[string]any{"from": home, "to": services, "label": "本地服务", "action": map[string]any{"action": "tap", "x": 880, "y": 210}})
	if err != nil {
		t.Fatal(err)
	}
	e := result.(navgraph.SavedEdge).Edge
	// MCP routes local graph calls without contacting a daemon.
	r := a.callTool("graph_path", map[string]any{"from": home, "to": services})
	if r["isError"] != true {
		t.Fatal("unverified route accepted")
	}
	r = a.callTool("graph_observe", map[string]any{"edge": e.ID, "success": true, "actual_node": services, "evidence": "截图确认本地服务标题和分类栏"})
	if r["isError"] != false {
		t.Fatalf("observe: %+v", r)
	}
	r = a.callTool("graph_path", map[string]any{"from": home, "to": services})
	if r["isError"] != false {
		t.Fatalf("route: %+v", r)
	}
	p := call("prepare", `{"edge":"`+e.ID+`","current_node":"`+home+`","width":720,"height":1558}`).Data["result"].(map[string]any)
	if p["tool"] != "tap" {
		t.Fatalf("prepared: %+v", p)
	}
	list := call("node-list", `{"app":"58","build":"1","profile":"phone","limit":1,"offset":1}`).Data["result"].([]any)
	if len(list) != 1 {
		t.Fatal("pagination")
	}
	if _, err = a.graphCall("graph_node_list", map[string]any{"app": "58", "build": "1", "profile": "phone", "limti": 1}); err == nil {
		t.Fatal("unknown parameter discarded")
	}
	if _, err = a.graphCall("graph_observe", map[string]any{"edge": e.ID, "evidence": "missing success"}); err == nil {
		t.Fatal("missing result recorded as failure")
	}
	if _, err = a.graphCall("graph_edge_enable", map[string]any{"edge": e.ID}); err == nil {
		t.Fatal("missing enabled silently disabled edge")
	}
	for _, tool := range graphTools {
		b, err := json.Marshal(tool.InputSchema)
		if err != nil || !strings.Contains(string(b), `"required":[`) {
			t.Fatalf("schema %s: %s %v", tool.Name, b, err)
		}
	}
}
