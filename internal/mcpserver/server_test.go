package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zalgonoise/codegraph/internal/manifest"
	"github.com/zalgonoise/codegraph/internal/model"
)

// writeRun writes a run like make graph does: one manifest per service.
func writeRun(t *testing.T, dir string, ms ...*model.Manifest) {
	t.Helper()
	for _, m := range ms {
		if err := os.MkdirAll(filepath.Join(dir, m.Service), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := manifest.WriteJSON(filepath.Join(dir, m.Service, "manifest.json"), m); err != nil {
			t.Fatal(err)
		}
	}
}

func shopAndStock() []*model.Manifest {
	return []*model.Manifest{
		{Service: "shop", Module: "example.com/shop", Endpoints: []model.Endpoint{
			{ID: "in", Transport: "http", Role: "server", Direction: model.Inbound, Keys: []string{"POST /orders"}},
			{ID: "pub", Transport: "kafka", Role: "producer", Direction: model.Outbound, Keys: []string{"orders.created"}, Position: "events.go:20"},
		}, Journeys: []model.Journey{{Entrypoint: "in", Transport: "http", Key: "POST /orders", Handler: "create",
			Outbound: []model.JourneyOutbound{{Endpoint: "pub", Kind: model.Async}}}}},
		{Service: "stock", Module: "example.com/stock", Endpoints: []model.Endpoint{
			{ID: "sub", Transport: "kafka", Role: "consumer", Direction: model.Inbound, Keys: []string{"orders.created"}},
		}, Journeys: []model.Journey{{Entrypoint: "sub", Transport: "kafka", Key: "orders.created", Handler: "reserve"}}},
	}
}

func connect(t *testing.T, dir string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(dir, "test", "http://codegraph.example/system.html", "").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return text.String(), res.IsError
}

func TestServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "graph")
	cs := connect(t, dir)
	ctx := context.Background()

	// Before a run exists, a question says how to make one.
	if text, isErr := call(t, cs, "services", nil); !isErr || !strings.Contains(text, "make graph") {
		t.Errorf("without a run: %q, error %v", text, isErr)
	}

	writeRun(t, dir, shopAndStock()...)
	var tools, drawn []string
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool.Name)
		if props, _ := tool.InputSchema.(map[string]any)["properties"].(map[string]any); props["diagram"] != nil {
			drawn = append(drawn, tool.Name)
		}
	}
	if strings.Join(tools, ",") != "image,impact,journey,path,search,service,services,uses,who" {
		t.Errorf("tools = %v", tools)
	}
	if strings.Join(drawn, ",") != "impact,journey,path,service,who" {
		t.Errorf("tools taking diagram = %v", drawn)
	}

	if text, isErr := call(t, cs, "journey", map[string]any{"service": "shop", "key": "orders"}); isErr ||
		!strings.Contains(text, "→ **stock** `orders.created` (`reserve`)") || !strings.Contains(text, "@ `events.go:20`") {
		t.Errorf("journey: %s", text)
	}
	if text, _ := call(t, cs, "journey", map[string]any{"service": "shop", "key": "orders", "depth": 0}); !strings.Contains(text, "deeper than asked") {
		t.Errorf("depth 0 should not follow into stock: %s", text)
	}
	// A diagram on request, and the explorer's page in every answer.
	if text, _ := call(t, cs, "journey", map[string]any{"service": "shop", "key": "orders", "diagram": true}); !strings.Contains(text, "```mermaid\nsequenceDiagram\n") ||
		!strings.Contains(text, "In the explorer: [shop · http POST /orders](http://codegraph.example/system.html#/j/shop/http/POST%20%2Forders).") {
		t.Errorf("journey with a diagram: %s", text)
	}
	if text, _ := call(t, cs, "service", map[string]any{"name": "shop"}); strings.Contains(text, "mermaid") ||
		!strings.Contains(text, "(http://codegraph.example/system.html#/s/shop)") {
		t.Errorf("service without a diagram: %s", text)
	}
	// A mistake is a tool error the model reads, with a suggestion.
	if text, isErr := call(t, cs, "service", map[string]any{"name": "stok"}); !isErr || !strings.Contains(text, "stock") {
		t.Errorf("misspelt service: %q, error %v", text, isErr)
	}
	if text, _ := call(t, cs, "who", map[string]any{"key": "orders.created", "json": true}); !strings.HasPrefix(strings.TrimSpace(text), "[") {
		t.Errorf("json answer: %s", text)
	}

	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: ContextURI})
	if err != nil || len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, "## shop") {
		t.Errorf("context resource: %+v, %v", res, err)
	}

	// make graph replaces the run: the next question sees the new one.
	if err := os.Rename(dir, dir+".prev"); err != nil {
		t.Fatal(err)
	}
	writeRun(t, dir, append(shopAndStock(), &model.Manifest{Service: "audit", Module: "example.com/audit"})...)
	if text, _ := call(t, cs, "services", nil); !strings.Contains(text, "3 services") {
		t.Errorf("after a new run: %s", text)
	}
}
