package render

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zalgonoise/codegraph/internal/model"
)

// A service with more calls than a Mermaid diagram can hold keeps its
// endpoint edges and heaviest calls, drops the entry's wiring first, and says
// how many calls it left out.
func TestServiceEdgeBudget(t *testing.T) {
	m := &model.Manifest{Service: "big", Entry: "x/cmd"}
	m.Components = append(m.Components, model.Component{Package: "x/cmd", Name: "cmd", Layer: "entry"})
	for i := 0; i < 300; i++ {
		pkg := fmt.Sprintf("x/p%03d", i)
		m.Components = append(m.Components, model.Component{Package: pkg, Name: pkg, Layer: "other"})
		m.Calls = append(m.Calls, model.Call{From: "x/cmd", To: pkg, Count: 1})
		if i > 0 {
			m.Calls = append(m.Calls, model.Call{From: pkg, To: fmt.Sprintf("x/p%03d", i-1), Count: 2})
		}
	}
	for i := 0; i < 100; i++ {
		m.Endpoints = append(m.Endpoints, model.Endpoint{ID: fmt.Sprint("ep", i), Transport: "http", Role: "server",
			Direction: model.Inbound, Keys: []string{fmt.Sprint("/r", i)}, Component: "x/p000"})
	}
	g := Service(m, ServiceOptions{})
	if len(g.Edges) > maxServiceEdges {
		t.Fatalf("%d edges, want at most %d", len(g.Edges), maxServiceEdges)
	}
	fromEntry := 0
	for _, e := range g.Edges {
		if e.From == g.nodes["c:x/cmd"].ID {
			fromEntry++
		}
	}
	if fromEntry != 51 { // 450 - 100 endpoints = 350 calls: the 299 non-entry ones, then 51 of main's
		t.Errorf("%d edges from the entry, want 51", fromEntry)
	}
	if mmd := Mermaid(g); !strings.Contains(mmd, "249 of 599 component calls not drawn") {
		t.Errorf("no omission note in:\n%s", mmd[:200])
	}
}
