package navgraph

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type PathOptions struct {
	From              string   `json:"from"`
	To                string   `json:"to"`
	Facts             []string `json:"facts"`
	IncludeUnverified bool     `json:"include_unverified"` // exploration only; latest failures still excluded
	MaxVisited        int      `json:"max_visited"`
}
type Path struct {
	From                 string  `json:"from"`
	To                   string  `json:"to"`
	Edges                []Edge  `json:"edges"`
	Cost                 float64 `json:"cost"`
	Visited              int     `json:"visited"`
	RequiresVerification bool    `json:"requires_verification"`
}

func satisfied(conditions []string, facts map[string]bool) bool {
	for _, c := range conditions {
		if !facts[c] {
			return false
		}
	}
	return true
}
func factSet(facts []string) map[string]bool {
	m := map[string]bool{}
	for _, f := range tags(facts) {
		m[f] = true
	}
	return m
}

// Route uses indexed outgoing-edge queries in a read snapshot, not a full graph load.
// Latest failures are excluded until a successful observation repairs that edge.
func (s *Store) Route(o PathOptions) (Path, error) {
	out := Path{From: o.From, To: o.To, Edges: []Edge{}, RequiresVerification: true}
	from, err := s.Node(o.From)
	if err != nil {
		return out, err
	}
	to, err := s.Node(o.To)
	if err != nil {
		return out, err
	}
	if from.Scope != to.Scope {
		return out, errors.New("path endpoints must have the same app, build and profile")
	}
	facts := factSet(o.Facts)
	if !satisfied(from.Conditions, facts) || !satisfied(to.Conditions, facts) {
		return out, errors.New("missing facts required by start or destination node")
	}
	if o.MaxVisited == 0 {
		o.MaxVisited = 10000
	}
	if o.MaxVisited < 1 || o.MaxVisited > 100000 {
		return out, errors.New("max_visited must be 1..100000")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	dist := map[string]float64{o.From: 0}
	prev := map[string]Edge{}
	done := map[string]bool{}
	q := &queue{{node: o.From, cost: 0}}
	for q.Len() > 0 {
		cur := heap.Pop(q).(entry)
		if done[cur.node] {
			continue
		}
		if out.Visited >= o.MaxVisited {
			return out, fmt.Errorf("path search exceeded max_visited=%d", o.MaxVisited)
		}
		done[cur.node] = true
		out.Visited++
		if cur.node == o.To {
			out.Cost = cur.cost
			for n := o.To; n != o.From; {
				e := prev[n]
				out.Edges = append(out.Edges, e)
				n = e.From
			}
			for i, j := 0, len(out.Edges)-1; i < j; i, j = i+1, j-1 {
				out.Edges[i], out.Edges[j] = out.Edges[j], out.Edges[i]
			}
			return out, nil
		}
		// Join only the destinations of this frontier node to check their preconditions.
		rows, err := tx.Query("SELECT "+qualifiedEdgeColumns+",n.conditions FROM edges e JOIN nodes n ON n.id=e.to_node WHERE e.from_node=? AND e.enabled=1 ORDER BY e.id", cur.node)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			e, conditions, err := scanCandidate(rows)
			if err != nil {
				rows.Close()
				return out, err
			}
			if done[e.To] || e.LastOutcome == "failure" || (!o.IncludeUnverified && e.SuccessCount == 0) || !satisfied(e.Conditions, facts) || !satisfied(conditions, facts) {
				continue
			}
			cost := cur.cost + edgeCost(e, s.now())
			if old, ok := dist[e.To]; !ok || cost < old {
				dist[e.To] = cost
				prev[e.To] = e
				heap.Push(q, entry{node: e.To, cost: cost})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	return out, errors.New("no eligible path; check facts, disabled/failed edges, or use include_unverified for exploration")
}

const qualifiedEdgeColumns = "e.id,e.from_node,e.to_node,e.label,e.action,e.conditions,e.enabled,e.success_count,e.failure_count,e.last_verified,e.last_success,e.last_outcome,e.from_run,e.step"

// An extra column is scanned alongside the normal edge without a second per-edge query.
type extraScanner struct {
	row   scanner
	extra *string
}

func (x extraScanner) Scan(dest ...any) error { return x.row.Scan(append(dest, x.extra)...) }
func scanCandidate(row scanner) (Edge, []string, error) {
	var raw string
	e, err := scanEdge(extraScanner{row: row, extra: &raw})
	var conditions []string
	if err == nil {
		err = json.Unmarshal([]byte(raw), &conditions)
	}
	return e, conditions, err
}

func edgeCost(e Edge, now time.Time) float64 {
	if e.SuccessCount == 0 {
		return 10 + float64(e.FailureCount)
	}
	age := max(0, float64(now.UnixMilli()-e.LastSuccess)/float64((24*time.Hour).Milliseconds()))
	return 1 + float64(e.FailureCount+1)/float64(e.SuccessCount+1) + min(age, 30)/30
}

type entry struct {
	node string
	cost float64
}
type queue []entry

func (q queue) Len() int { return len(q) }
func (q queue) Less(i, j int) bool {
	if q[i].cost == q[j].cost {
		return q[i].node < q[j].node
	}
	return q[i].cost < q[j].cost
}
func (q queue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *queue) Push(v any)   { *q = append(*q, v.(entry)) }
func (q *queue) Pop() any     { old := *q; n := len(old); v := old[n-1]; *q = old[:n-1]; return v }

// Prepare rechecks applicability immediately before converting a stored action.
func (s *Store) Prepare(edge, current string, facts []string, width, height int, allowUnverified bool) (Move, error) {
	e, err := s.Edge(edge)
	if err != nil {
		return Move{}, err
	}
	if e.From != current {
		return Move{}, errors.New("current_node must equal the edge source; re-localize after any mismatch")
	}
	if !e.Enabled || e.LastOutcome == "failure" || (!allowUnverified && e.SuccessCount == 0) {
		return Move{}, errors.New("edge is disabled, failed or unverified")
	}
	from, err := s.Node(e.From)
	if err != nil {
		return Move{}, err
	}
	to, err := s.Node(e.To)
	if err != nil {
		return Move{}, err
	}
	f := factSet(facts)
	if !satisfied(from.Conditions, f) || !satisfied(to.Conditions, f) || !satisfied(e.Conditions, f) {
		return Move{}, errors.New("missing facts required by nodes or edge")
	}
	return e.Action.Pixels(width, height)
}
