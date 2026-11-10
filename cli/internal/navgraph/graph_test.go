package navgraph

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/history"
)

var testScope = Scope{App: "example.app", Build: "1.0", Profile: "phone-portrait"}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}
func node(t *testing.T, s *Store, key string, conditions ...string) Node {
	t.Helper()
	n, err := s.SaveNode(Node{Scope: testScope, Key: key, Name: key, Conditions: conditions})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func edge(t *testing.T, s *Store, from, to Node) Edge {
	t.Helper()
	e, err := s.SaveEdge(EdgeInput{From: from.ID, To: to.ID, Label: "open " + to.Key, Action: Action{"action": "tap", "x": 500, "y": 800}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func verify(t *testing.T, s *Store, e Edge) {
	t.Helper()
	if _, err := s.Observe(Observation{Edge: e.ID, ActualNode: e.To, Success: true, Evidence: "Checked destination title and expected controls"}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectedPathsRequireVerificationAndFacts(t *testing.T) {
	s, _ := openTest(t)
	a := node(t, s, "home", "logged_in")
	b := node(t, s, "services", "logged_in")
	c := node(t, s, "register", "logged_in")
	ab, bc := edge(t, s, a, b), edge(t, s, b, c)
	o := PathOptions{From: a.ID, To: c.ID, Facts: []string{"logged_in"}}
	if _, err := s.Route(o); err == nil {
		t.Fatal("unverified edges were planned by default")
	}
	o.IncludeUnverified = true
	p, err := s.Route(o)
	if err != nil || len(p.Edges) != 2 {
		t.Fatalf("exploration: %+v %v", p, err)
	}
	// Sending an action successfully in history never incremented success_count.
	if e, _ := s.Edge(ab.ID); e.SuccessCount != 0 {
		t.Fatal("save claimed verification")
	}
	verify(t, s, ab)
	verify(t, s, bc)
	o.IncludeUnverified = false
	p, err = s.Route(o)
	if err != nil || p.Edges[0].ID != ab.ID || p.Edges[1].ID != bc.ID {
		t.Fatalf("path: %+v %v", p, err)
	}
	o.Facts = nil
	if _, err = s.Route(o); err == nil {
		t.Fatal("missing login facts accepted")
	}
	if _, err = s.Route(PathOptions{From: c.ID, To: a.ID, Facts: []string{"logged_in"}}); err == nil {
		t.Fatal("invented reverse edges")
	}
	ca := edge(t, s, c, a)
	verify(t, s, ca)
	if _, err = s.Route(PathOptions{From: c.ID, To: b.ID, Facts: []string{"logged_in"}}); err != nil {
		t.Fatal("cycle broke route:", err)
	}
	if _, err = s.Route(PathOptions{From: a.ID, To: c.ID, Facts: []string{"logged_in"}, MaxVisited: 1}); err == nil {
		t.Fatal("search budget ignored")
	}
	if p, err = s.Route(PathOptions{From: a.ID, To: a.ID, Facts: []string{"logged_in"}}); err != nil || len(p.Edges) != 0 {
		t.Fatalf("already there: %+v %v", p, err)
	}
}

func TestRouteReliabilityFailuresAndRepair(t *testing.T) {
	s, _ := openTest(t)
	a, b, c := node(t, s, "a"), node(t, s, "b"), node(t, s, "c")
	ab, bc, ac := edge(t, s, a, b), edge(t, s, b, c), edge(t, s, a, c)
	verify(t, s, ab)
	verify(t, s, bc)
	opts := PathOptions{From: a.ID, To: c.ID, IncludeUnverified: true}
	p, err := s.Route(opts)
	if err != nil || len(p.Edges) != 2 {
		t.Fatalf("unverified shortcut preferred: %+v %v", p, err)
	}
	verify(t, s, ac)
	p, err = s.Route(opts)
	if err != nil || len(p.Edges) != 1 {
		t.Fatalf("verified shortcut: %+v %v", p, err)
	}
	if _, err = s.Observe(Observation{Edge: ac.ID, ActualNode: b.ID, Success: false, Evidence: "Unexpected popup; re-localized to b"}); err != nil {
		t.Fatal(err)
	}
	p, err = s.Route(opts)
	if err != nil || len(p.Edges) != 2 {
		t.Fatalf("failed edge not excluded: %+v %v", p, err)
	}
	if _, err = s.Prepare(ac.ID, a.ID, nil, 720, 1558, true); err == nil {
		t.Fatal("failed edge prepared")
	}
	verify(t, s, ac)
	if _, err = s.Prepare(ac.ID, b.ID, nil, 720, 1558, false); err == nil {
		t.Fatal("wrong current node accepted")
	}
	move, err := s.Prepare(ac.ID, a.ID, nil, 720, 1558, false)
	if err != nil || move.Tool != "tap" || move.Arguments["x"] != 360.0 {
		t.Fatalf("prepare: %+v %v", move, err)
	}
	if _, err = s.SetEnabled(ac.ID, false); err != nil {
		t.Fatal(err)
	}
	p, err = s.Route(opts)
	if err != nil || len(p.Edges) != 2 {
		t.Fatalf("disabled edge used: %+v %v", p, err)
	}
	// Age and failure rate affect costs even if both edges are currently successful.
	old := ac
	old.LastSuccess = time.Now().Add(-60 * 24 * time.Hour).UnixMilli()
	old.SuccessCount = 1
	old.FailureCount = 20
	fresh := ac
	fresh.LastSuccess = time.Now().UnixMilli()
	fresh.SuccessCount = 20
	if edgeCost(old, time.Now()) <= edgeCost(fresh, time.Now()) {
		t.Fatal("age/reliability not reflected")
	}
}

func TestScopeDedupPaginationAndPersistence(t *testing.T) {
	s, dir := openTest(t)
	a := node(t, s, "home")
	b := node(t, s, "services")
	again, err := s.SaveNode(Node{Scope: testScope, Key: "home", Name: "Renamed home"})
	if err != nil || again.ID != a.ID {
		t.Fatalf("node not reused: %+v %v", again, err)
	}
	other := testScope
	other.Build = "2.0"
	v2, err := s.SaveNode(Node{Scope: other, Key: "home", Name: "home"})
	if err != nil {
		t.Fatal(err)
	}
	if v2.ID == a.ID {
		t.Fatal("versions share node")
	}
	if _, err = s.SaveEdge(EdgeInput{From: a.ID, To: v2.ID, Label: "invalid", Action: Action{"action": "home"}}); err == nil {
		t.Fatal("cross-version edge accepted")
	}
	if _, err = s.Route(PathOptions{From: a.ID, To: v2.ID}); err == nil {
		t.Fatal("cross-version route accepted")
	}
	e := edge(t, s, a, b)
	verify(t, s, e)
	e2 := edge(t, s, a, b)
	if e2.ID != e.ID || e2.SuccessCount != 1 {
		t.Fatalf("edge duplicate reset evidence: %+v", e2)
	}
	list, err := s.Nodes(testScope, 1, 1)
	if err != nil || len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("page: %+v %v", list, err)
	}
	if list, err := s.Edges(b.ID, true, 100, 0); err != nil || len(list) != 1 {
		t.Fatalf("incoming: %+v %v", list, err)
	}
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Edge(e.ID)
	if err != nil || got.SuccessCount != 1 {
		t.Fatalf("persistence: %+v %v", got, err)
	}
}

func TestObservationsAreValidatedAndAtomic(t *testing.T) {
	s, _ := openTest(t)
	a, b, c := node(t, s, "a"), node(t, s, "b"), node(t, s, "c")
	e := edge(t, s, a, b)
	for _, o := range []Observation{
		{Edge: e.ID, Success: true, ActualNode: c.ID, Evidence: "Wrong destination"},
		{Edge: e.ID, Success: true, ActualNode: b.ID},
		{Edge: e.ID, Success: true, ActualNode: b.ID, Evidence: "Checked title", Screenshot: "missing"},
	} {
		if _, err := s.Observe(o); err == nil {
			t.Fatal("invalid evidence accepted:", o)
		}
	}
	if got, _ := s.Edge(e.ID); got.SuccessCount != 0 || got.FailureCount != 0 {
		t.Fatal("invalid observation changed counters")
	}
	obs, err := s.Observations(e.ID, 100, 0)
	if err != nil || len(obs) != 0 {
		t.Fatalf("invalid observation persisted: %+v %v", obs, err)
	}
	verify(t, s, e)
	if _, err = s.Observe(Observation{Edge: e.ID, Success: false, Evidence: "Network error popup; page unknown"}); err != nil {
		t.Fatal(err)
	}
	obs, err = s.Observations(e.ID, 1, 0)
	if err != nil || len(obs) != 1 || obs[0].Success {
		t.Fatalf("latest observation: %+v %v", obs, err)
	}
}

func TestConcurrentObserversDoNotLoseCounts(t *testing.T) {
	s, dir := openTest(t)
	a, b := node(t, s, "a"), node(t, s, "b")
	e := edge(t, s, a, b)
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			writer := other
			if i%2 == 0 {
				writer = s
			}
			_, err := writer.Observe(Observation{Edge: e.ID, Success: true, ActualNode: b.ID, Evidence: "Checked title"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Edge(e.ID)
	if err != nil || got.SuccessCount != 20 {
		t.Fatalf("lost update: %+v %v", got, err)
	}
	obs, err := s.Observations(e.ID, 100, 0)
	if err != nil || len(obs) != 20 {
		t.Fatalf("evidence/counter mismatch: %d %v", len(obs), err)
	}
}

func TestConcurrentFirstOpen(t *testing.T) {
	dir := t.TempDir()
	start := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			s, err := Open(dir)
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestHistoryReferenceSurvivesRetention(t *testing.T) {
	s, dir := openTest(t)
	a, b := node(t, s, "a"), node(t, s, "b")
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 72, 156))); err != nil {
		t.Fatal(err)
	}
	r := &history.Recorder{Dir: history.Root(dir)}
	id, err := r.New("open services")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write("llm", map[string]any{"role": "screenshot"}, img.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err = r.Write("step", map[string]any{"action": "tap", "x": 500, "y": 800, "count": 2}, nil); err != nil {
		t.Fatal(err)
	}
	out, err := s.SaveHistoryEdge(dir, EdgeInput{From: a.ID, To: b.ID, Label: "Open services", FromRun: id, Step: 1})
	if err != nil || out.Reference == nil || out.Edge.SuccessCount != 0 {
		t.Fatalf("import: %+v %v", out, err)
	}
	if err = os.RemoveAll(history.Root(dir)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out.Reference.Path)
	if err != nil || !bytes.Equal(data, img.Bytes()) {
		t.Fatal("reference lost after history retention", err)
	}
	if out.Reference.Node != a.ID || out.Reference.Width != 72 {
		t.Fatalf("reference: %+v", out.Reference)
	}
	if _, err = s.Observe(Observation{Edge: out.Edge.ID, Success: true, ActualNode: b.ID, Evidence: "Checked target", Screenshot: out.Reference.ID}); err == nil {
		t.Fatal("source screenshot accepted as destination evidence")
	}
	path := filepath.Join(dir, "not-image")
	os.WriteFile(path, []byte("secret text"), 0o600)
	if _, err = s.SaveScreenshot(a.ID, path); err == nil {
		t.Fatal("non-image accepted")
	}
	move, err := out.Edge.Action.Pixels(720, 1558)
	if err != nil || move.Arguments["count"] != float64(2) {
		t.Fatalf("double tap lost: %+v %v", move, err)
	}
}

func TestActionValidationAndConversion(t *testing.T) {
	invalid := []Action{
		{"action": "tap", "x": 1001, "y": 20}, {"action": "tap", "x": 10}, {"action": "tap", "x": 1, "y": 1, "count": 1.5},
		{"action": "longpress", "x": 1, "y": 1, "ms": -1}, {"action": "swipe", "x1": 0, "y1": 0, "x2": 10, "y2": -1},
		{"action": "type", "text": ""}, {"action": "key", "combo": ""}, {"action": "tap", "x": 1, "y": 1, "typo": 2}, {"action": "arbitrary_command"},
	}
	for _, a := range invalid {
		if err := a.Validate(); err == nil {
			t.Fatalf("accepted %+v", a)
		}
	}
	a := Action{"action": "tap", "x": 1000, "y": 1000}
	m, err := a.Pixels(720, 1558)
	if err != nil || m.Arguments["x"] != 719.0 || m.Arguments["y"] != 1557.0 {
		t.Fatalf("screen bounds: %+v %v", m, err)
	}
	if _, err = a.Pixels(0, 1558); err == nil {
		t.Fatal("zero screen accepted")
	}
}

// A long connected route among unrelated nodes exercises indexed frontier access.
func BenchmarkRoute(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	tx, err := s.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		_, err = tx.Exec("INSERT INTO nodes VALUES(?,?,?,?,?,?,?,?)", fmt.Sprint(i), "app", "1", "phone", fmt.Sprint(i), "page", "[]", 0)
		if err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 999; i++ {
		_, err = tx.Exec("INSERT INTO edges(id,from_node,to_node,label,action,conditions,success_count,last_success,last_outcome) VALUES(?,?,?,?,?,?,1,?,'success')", fmt.Sprint(i), fmt.Sprint(i), fmt.Sprint(i+1), "next", `{"action":"tap","x":500,"y":500}`, "[]", time.Now().UnixMilli())
		if err != nil {
			b.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := s.Route(PathOptions{From: "0", To: "999"})
		if err != nil || len(p.Edges) != 999 {
			b.Fatal(err)
		}
	}
}
