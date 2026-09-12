package modelrows

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const modelsBody = `{"object":"list","data":[
  {"id":"waired/default","object":"model","owned_by":"waired","max_input_tokens":131072,
   "waired_route":true,"display_name":"Waired","description":"Any of your computers"},
  {"id":"waired/local","object":"model","owned_by":"waired","max_input_tokens":131072,
   "waired_route":true,"display_name":"Waired local","description":"This computer"},
  {"id":"waired/peer-big-box","object":"model","owned_by":"waired","max_input_tokens":1048576,
   "waired_route":true,"display_name":"Waired peer: big-box","description":"qwen3.5-35b-a3b"},
  {"id":"qwen3.5-9b","object":"model","owned_by":"waired","max_input_tokens":200704},
  {"id":"waired/tiny","object":"model","owned_by":"waired","max_input_tokens":32768}
]}`

// Only the rows the listing MARKS are rows. The rest of that body is this
// host's model catalog, which answers a different question — and one of those
// entries is itself spelled waired/*, so a name-shaped test would pass while
// the product offered a CI fixture model as a computer.
func TestFromModelsBody(t *testing.T) {
	rows := FromModelsBody([]byte(modelsBody))
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want the three marked ones", rows)
	}
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ID)
	}
	for i, want := range []string{"waired/default", "waired/local", "waired/peer-big-box"} {
		if got[i] != want {
			t.Errorf("row %d = %q, want %q (listing order is the picker's order)", i, got[i], want)
		}
	}
	if rows[1].DisplayName != "Waired local" || rows[1].Description != "This computer" {
		t.Errorf("the two lines a row shows were dropped: %+v", rows[1])
	}
	if rows[1].ContextWindow != 131072 || rows[1].Window1M {
		t.Errorf("local row = %+v, want its own window and no 1M claim", rows[1])
	}
	if !rows[2].Window1M {
		t.Errorf("a peer declaring 1048576 was not read as declaring 1M: %+v", rows[2])
	}
}

func TestFromModelsBody_EmptyAnswers(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not JSON", "<html>not the gateway</html>"},
		{"no data", `{"object":"list"}`},
		{"catalog only", `{"data":[{"id":"qwen3.5-9b"}]}`},
		{"marked but unnamed", `{"data":[{"id":"","waired_route":true}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rows := FromModelsBody([]byte(tc.body)); rows != nil {
				t.Errorf("rows = %+v, want none", rows)
			}
		})
	}
}

// A failure to learn the rows must not fail a link: every way this can come up
// empty answers "not known", and the caller writes the row that needs no facts.
func TestFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsBody))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if rows := Fetch(context.Background(), srv.URL); len(rows) != 3 {
		t.Errorf("rows = %+v, want three", rows)
	}
	// A trailing slash is the same gateway.
	if rows := Fetch(context.Background(), srv.URL+"/"); len(rows) != 3 {
		t.Errorf("trailing slash: rows = %+v, want three", rows)
	}
	if rows := Fetch(context.Background(), ""); rows != nil {
		t.Errorf("no base URL: rows = %+v, want none", rows)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer refusing.Close()
	if rows := Fetch(context.Background(), refusing.URL); rows != nil {
		t.Errorf("a refusing gateway: rows = %+v, want none", rows)
	}

	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := down.URL
	down.Close()
	if rows := Fetch(context.Background(), addr); rows != nil {
		t.Errorf("no listener: rows = %+v, want none", rows)
	}
}
