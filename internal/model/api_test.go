package model

import "testing"

func TestAPIKey(t *testing.T) {
	for _, c := range []struct{ pkg, module, label, want string }{
		{"example.com/x/fts", "example.com/x/fts", "fts.New", "fts.New"},
		{"example.com/x/fts", "example.com/x/fts", "(*fts.Index[K, V]).Search", "fts.Index.Search"},
		{"example.com/x/fts", "example.com/x/fts", "fts.Indexer[string, map[string]int].Search", "fts.Indexer.Search"},
		{"example.com/x/fts/log", "example.com/x/fts", "log.Handler.Handle", "fts/log.Handler.Handle"},
		{"example.com/x/cli/v2", "example.com/x/cli/v2", "(*cli.Runnable).Run", "cli.Runnable.Run"},
		{"example.com/x/cli/v2/flags", "example.com/x/cli/v2", "flags.Parse", "cli/flags.Parse"},
	} {
		if got := APIKey(c.pkg, c.module, c.label); got != c.want {
			t.Errorf("APIKey(%q, %q, %q) = %q, want %q", c.pkg, c.module, c.label, got, c.want)
		}
	}
}

// A journey calling a library's symbols is paired with their journeys; a
// symbol the library's API has no journey for, and a module the run did not
// analyse, are not.
func TestAPICalls(t *testing.T) {
	lib := &Manifest{Service: "fts", Module: "example.com/x/fts", Library: true,
		Endpoints: []Endpoint{
			{ID: "a", Transport: APITransport, Component: "example.com/x/fts"},
			{ID: "b", Transport: APITransport, Component: "example.com/x/fts"},
		},
		Journeys: []Journey{{Entrypoint: "a", Transport: APITransport, Key: "fts.New"}, {Entrypoint: "b", Transport: APITransport, Key: "fts.Indexer.Search"}},
	}
	svc := &Manifest{Service: "search", Module: "example.com/search", Journeys: []Journey{
		{Key: "GET /search", Uses: []SymbolUse{
			{Module: "example.com/x/fts", Package: "example.com/x/fts", Symbol: "fts.Indexer[string, string].Search"},
			{Module: "example.com/x/fts", Package: "example.com/x/fts", Symbol: "fts.WithURI"},
			{Module: "example.com/other", Package: "example.com/other", Symbol: "other.Do"},
		}},
		{Key: "POST /index", Uses: []SymbolUse{{Module: "example.com/x/fts", Package: "example.com/x/fts", Symbol: "fts.New"}}},
	}}
	calls := APICalls([]*Manifest{svc, lib})
	if len(calls) != 2 || calls[0].API != (JourneyAt{1, 1}) || calls[0].Caller != (JourneyAt{0, 0}) ||
		calls[1].API != (JourneyAt{1, 0}) || calls[1].Caller != (JourneyAt{0, 1}) {
		t.Errorf("APICalls = %+v", calls)
	}
	if APICalls([]*Manifest{svc}) != nil {
		t.Error("no library, no calls")
	}
}
