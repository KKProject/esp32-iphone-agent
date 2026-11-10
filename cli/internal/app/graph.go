package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/daemon"
	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/navgraph"
)

var graphTools = []tool{
	{"graph_node_save", "Create/update a page STATE by app+build+profile+key. Use distinct keys for login, modal, keyboard and scroll states. Conditions are required environment facts. Returns a stable node id.", local([]string{"app", "build", "profile", "key", "name"}, map[string]any{"app": strProp, "build": strProp, "profile": strProp, "key": strProp, "name": strProp, "conditions": stringArray})},
	{"graph_node_list", "List a page of nodes in one exact app/build/profile. Does not load the full graph.", local([]string{"app", "build", "profile"}, map[string]any{"app": strProp, "build": strProp, "profile": strProp, "limit": integerProp, "offset": integerProp})},
	{"graph_node_find", "Find one state by indexed app+build+profile+key; use its id for paths and edges.", local([]string{"app", "build", "profile", "key"}, map[string]any{"app": strProp, "build": strProp, "profile": strProp, "key": strProp})},
	{"graph_node_show", "Read a node and its newest 20 reference screenshots (absolute local paths).", local([]string{"node"}, map[string]any{"node": strProp})},
	{"graph_edge_save", "Save a directed transition; both endpoints must share app/build/profile. Provide an action in 0-1000 coordinates OR from_run+step (history's 1-based action number); the latter also copies the source reference. Saving command history does NOT verify arrival. Deduplicates without resetting evidence.", local([]string{"from", "to", "label"}, map[string]any{"from": strProp, "to": strProp, "label": strProp, "action": actionSchema, "conditions": stringArray, "disabled": boolProp, "from_run": strProp, "step": integerProp})},
	{"graph_edge_list", "List a node's outgoing edges; incoming=true lists entrances. Paginated.", local([]string{"node"}, map[string]any{"node": strProp, "incoming": boolProp, "limit": integerProp, "offset": integerProp})},
	{"graph_edge_show", "Read one edge, including verification counts and the latest outcome.", local([]string{"edge"}, map[string]any{"edge": strProp})},
	{"graph_edge_enable", "Enable/disable an edge without deleting evidence. Enabling does not clear its latest failure; verify a successful retry before planning it again.", local([]string{"edge", "enabled"}, map[string]any{"edge": strProp, "enabled": boolProp})},
	{"graph_observe", "Record an actual checked result, NOT command delivery. Success requires actual_node equal to the destination and evidence describing the screenshot or reliable signal checked. A failure excludes this edge until a successful revalidation. Optional screenshot is a graph screenshot ID belonging to actual_node.", local([]string{"edge", "success", "evidence"}, map[string]any{"edge": strProp, "actual_node": strProp, "success": boolProp, "evidence": strProp, "screenshot": strProp})},
	{"graph_observations", "List an edge's verification evidence, newest first. Paginated.", local([]string{"edge"}, map[string]any{"edge": strProp, "limit": integerProp, "offset": integerProp})},
	{"graph_screenshot_save", "Copy a local JPEG/PNG into graph storage as a node reference; DB stores only its path and dimensions. This does not verify an edge.", local([]string{"node", "path"}, map[string]any{"node": strProp, "path": strProp})},
	{"graph_screenshot_list", "List a node's stored reference screenshots with absolute local paths. Paginated.", local([]string{"node"}, map[string]any{"node": strProp, "limit": integerProp, "offset": integerProp})},
	{"graph_path", "Plan a directed path from a KNOWN current node to a goal. Exact app/build/profile only; facts must satisfy all conditions. Uses indexed Dijkstra, preferring reliable/recent edges. Disabled or latest-failed edges are excluded. Unverified edges require include_unverified=true (exploration). Does not execute or detect the current page. Recheck at checkpoints and after any mismatch.", local([]string{"from", "to"}, map[string]any{"from": strProp, "to": strProp, "facts": stringArray, "include_unverified": boolProp, "max_visited": integerProp})},
	{"graph_prepare", "Prepare ONE saved edge as a normal MCP tool+arguments in screenshot pixels; DOES NOT EXECUTE it or update current_node. Supply a freshly confirmed current_node and current screenshot dimensions. Rechecks edge status/facts. Execute the returned tool, check the result, then graph_observe. Stop and re-localize on mismatch.", local([]string{"edge", "current_node", "width", "height"}, map[string]any{"edge": strProp, "current_node": strProp, "facts": stringArray, "width": integerProp, "height": integerProp, "include_unverified": boolProp})},
}

var (
	integerProp  = map[string]any{"type": "integer"}
	boolProp     = map[string]any{"type": "boolean"}
	stringArray  = map[string]any{"type": "array", "items": strProp}
	actionSchema = map[string]any{"type": "object", "required": []string{"action"}, "additionalProperties": false, "properties": map[string]any{
		"action": map[string]any{"type": "string", "enum": []string{"tap", "longpress", "swipe", "type", "key", "home"}},
		"x":      unitProp, "y": unitProp, "x1": unitProp, "y1": unitProp, "x2": unitProp, "y2": unitProp,
		"text": strProp, "combo": strProp, "ms": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}, "count": map[string]any{"type": "integer", "minimum": 1, "maximum": 3},
	}}
	unitProp = map[string]any{"type": "number", "minimum": 0, "maximum": 1000}
)

// decodeGraph rejects typos rather than silently dropping a graph's conditions/coordinates.
func decodeGraph(args map[string]any, out any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

type graphPage struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func (p graphPage) limit() int {
	if p.Limit == 0 {
		return 100
	}
	return p.Limit
}

func (a *App) graphCall(name string, args map[string]any) (any, error) {
	// Only named graph tools may open/create the database.
	found := false
	for _, t := range graphTools {
		if t.Name == name {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown graph operation %s", name)
	}
	s, err := navgraph.Open(a.Dir)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	switch name {
	case "graph_node_find":
		var in struct {
			navgraph.Scope
			Key string `json:"key"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.FindNode(in.Scope, in.Key)
	case "graph_node_save":
		var in struct {
			navgraph.Scope
			Key        string   `json:"key"`
			Name       string   `json:"name"`
			Conditions []string `json:"conditions"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.SaveNode(navgraph.Node{Scope: in.Scope, Key: in.Key, Name: in.Name, Conditions: in.Conditions})
	case "graph_node_list":
		var in struct {
			navgraph.Scope
			graphPage
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.Nodes(in.Scope, in.limit(), in.Offset)
	case "graph_node_show":
		var in struct {
			Node string `json:"node"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		n, err := s.Node(in.Node)
		if err != nil {
			return nil, err
		}
		shots, err := s.Screenshots(in.Node, 20, 0)
		return map[string]any{"node": n, "screenshots": shots}, err
	case "graph_edge_save":
		var in navgraph.EdgeInput
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.SaveHistoryEdge(a.Dir, in)
	case "graph_edge_show":
		var in struct {
			Edge string `json:"edge"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.Edge(in.Edge)
	case "graph_edge_enable":
		var in struct {
			Edge    string `json:"edge"`
			Enabled *bool  `json:"enabled"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		if in.Enabled == nil {
			return nil, errors.New("enabled is required")
		}
		return s.SetEnabled(in.Edge, *in.Enabled)
	case "graph_edge_list":
		var in struct {
			Node     string `json:"node"`
			Incoming bool   `json:"incoming"`
			graphPage
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.Edges(in.Node, in.Incoming, in.limit(), in.Offset)
	case "graph_observe":
		var in struct {
			Edge       string `json:"edge"`
			ActualNode string `json:"actual_node"`
			Success    *bool  `json:"success"`
			Evidence   string `json:"evidence"`
			Screenshot string `json:"screenshot"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		if in.Success == nil {
			return nil, errors.New("success is required")
		}
		return s.Observe(navgraph.Observation{Edge: in.Edge, ActualNode: in.ActualNode, Success: *in.Success, Evidence: in.Evidence, Screenshot: in.Screenshot})
	case "graph_observations":
		var in struct {
			Edge string `json:"edge"`
			graphPage
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.Observations(in.Edge, in.limit(), in.Offset)
	case "graph_screenshot_save":
		var in struct {
			Node string `json:"node"`
			Path string `json:"path"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.SaveScreenshot(in.Node, in.Path)
	case "graph_screenshot_list":
		var in struct {
			Node string `json:"node"`
			graphPage
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.Screenshots(in.Node, in.limit(), in.Offset)
	case "graph_path":
		var in navgraph.PathOptions
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.Route(in)
	case "graph_prepare":
		var in struct {
			Edge              string   `json:"edge"`
			Current           string   `json:"current_node"`
			Facts             []string `json:"facts"`
			Width             int      `json:"width"`
			Height            int      `json:"height"`
			IncludeUnverified bool     `json:"include_unverified"`
		}
		if err = decodeGraph(args, &in); err != nil {
			return nil, err
		}
		return s.Prepare(in.Edge, in.Current, in.Facts, in.Width, in.Height, in.IncludeUnverified)
	}
	return nil, errors.New("unhandled graph operation")
}

// The CLI takes the same JSON as MCP. Files/stdin avoid quoting long Chinese names and action objects.
func (a *App) graph(args []string) int {
	fs := newFS(new(string))
	input := fs.String("input", "", "JSON argument file, or - for stdin")
	pos, err := parse(fs, args)
	if err != nil {
		return a.fail(err)
	}
	if len(pos) != 1 {
		return a.fail(errors.New("usage: esp32agent graph node-save|node-find|node-list|node-show|edge-save|edge-list|edge-show|edge-enable|observe|observations|screenshot-save|screenshot-list|path|prepare --input FILE|-"))
	}
	if *input == "" {
		return a.fail(errors.New("graph requires --input FILE or --input - (same JSON arguments as the MCP graph tool)"))
	}
	var r io.Reader
	if *input == "-" {
		r = a.In
		if r == nil {
			r = os.Stdin
		}
	} else {
		f, err := os.Open(*input)
		if err != nil {
			return a.fail(err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return a.fail(err)
	}
	if len(b) > 1<<20 {
		return a.fail(errors.New("graph input exceeds 1 MiB"))
	}
	var in map[string]any
	if err = json.Unmarshal(b, &in); err != nil {
		return a.fail(err)
	}
	if in == nil {
		return a.fail(errors.New("graph input must be a JSON object"))
	}
	out, err := a.graphCall("graph_"+strings.ReplaceAll(pos[0], "-", "_"), in)
	if err != nil {
		return a.fail(err)
	}
	return a.print(daemon.Response{Status: "ok", Data: map[string]any{"result": out}})
}
