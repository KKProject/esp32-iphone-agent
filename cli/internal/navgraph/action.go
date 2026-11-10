package navgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Action is history's action object, with coordinates in 0-1000 units.
type Action map[string]any

func number(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case json.Number:
		var err error
		f, err = n.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

func (a Action) Validate() error {
	kind, _ := a["action"].(string)
	allowed := map[string]bool{"action": true}
	coords := []string{}
	textKey := ""
	switch kind {
	case "tap":
		coords = []string{"x", "y"}
		allowed["count"] = true
	case "longpress":
		coords = []string{"x", "y"}
		allowed["ms"] = true
	case "swipe":
		coords = []string{"x1", "y1", "x2", "y2"}
	case "type":
		textKey = "text"
	case "key":
		textKey = "combo"
	case "home":
	default:
		return fmt.Errorf("unsupported navigation action %q (tap, longpress, swipe, type, key, home)", kind)
	}
	for _, k := range coords {
		allowed[k] = true
		f, ok := number(a[k])
		if !ok || f < 0 || f > 1000 {
			return fmt.Errorf("%s must be a number in 0..1000 screen units", k)
		}
	}
	if textKey != "" {
		allowed[textKey] = true
		t, ok := a[textKey].(string)
		if !ok || strings.TrimSpace(t) == "" {
			return fmt.Errorf("%s is required", textKey)
		}
	}
	for k, v := range a {
		if !allowed[k] {
			return fmt.Errorf("unknown %s action field %q", kind, k)
		}
		if k == "ms" || k == "count" {
			n, ok := number(v)
			hi := 10000.0
			if k == "count" {
				hi = 3
			}
			if !ok || n < 1 || n > hi || n != math.Trunc(n) {
				return fmt.Errorf("%s must be an integer in 1..%g", k, hi)
			}
		}
	}
	return nil
}

type Move struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// Pixels prepares one normal MCP action. It never sends input or claims arrival.
func (a Action) Pixels(width, height int) (Move, error) {
	if err := a.Validate(); err != nil {
		return Move{}, err
	}
	if width < 1 || height < 1 || width > 16384 || height > 16384 {
		return Move{}, errors.New("current screenshot width and height must be 1..16384")
	}
	kind := a["action"].(string)
	m := Move{Arguments: map[string]any{}}
	switch kind {
	case "tap", "longpress", "swipe":
		m.Tool = kind
		for k, v := range a {
			size := width
			if strings.HasPrefix(k, "y") {
				size = height
			}
			if strings.HasPrefix(k, "x") || strings.HasPrefix(k, "y") {
				f, _ := number(v)
				m.Arguments[k] = min(float64(size-1), f*float64(size)/1000)
			}
		}
		if v, ok := a["count"]; ok {
			m.Arguments["count"] = v
		}
		if v, ok := a["ms"]; ok {
			m.Arguments["duration_ms"] = v
		}
	case "type":
		m.Tool = "type_text"
		m.Arguments["text"] = a["text"]
	case "key":
		m.Tool = "press_key"
		m.Arguments["key"] = a["combo"]
	case "home":
		m.Tool = "press_button"
		m.Arguments["button"] = "HOME"
	}
	return m, nil
}
