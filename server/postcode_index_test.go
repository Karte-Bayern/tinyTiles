package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func postcodeTestIndex(t *testing.T) *postcodeIndex {
	t.Helper()
	path := filepath.Join(t.TempDir(), "postcodes.geojson")
	// Two disjoint areas share a code; a hole must remain excluded.
	data := `{"type":"FeatureCollection","features":[
 {"type":"Feature","properties":{"postcode":"AB1","name":"North"},"geometry":{"type":"Polygon","coordinates":[[[0,0],[4,0],[4,4],[0,4],[0,0]],[[1,1],[1,2],[2,2],[2,1],[1,1]]]}},
 {"type":"Feature","properties":{"postcode":" ab1 ","name":"South"},"geometry":{"type":"Polygon","coordinates":[[[10,10],[11,10],[11,11],[10,11],[10,10]]]}},
 {"type":"Feature","properties":{"postcode":"CD2","name":"East"},"geometry":{"type":"Polygon","coordinates":[[[20,20],[21,20],[21,21],[20,21],[20,20]]]}}
 ]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	idx, err := loadPostcodeIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func postcodeRequest(s *Server, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.servePostcode(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestPostcodeMergedAreas(t *testing.T) {
	idx := postcodeTestIndex(t)
	if len(idx.all) != 2 {
		t.Fatalf("got %d records", len(idx.all))
	}
	rec := idx.byCode["ab1"]
	if len(rec.Geometry) != 2 || rec.BBox != [4]float64{0, 0, 11, 11} || rec.Name != "North" {
		t.Fatalf("merged record: %+v", rec)
	}
	s := &Server{postcodeIndex: idx}
	for _, tc := range []struct {
		path  string
		count int
	}{
		{"/postcode/at?lon=0.5&lat=0.5", 1},
		{"/postcode/at?lon=10.5&lat=10.5", 1},
		{"/postcode/at?lon=1.5&lat=1.5", 0},
		{"/postcode/at?lon=5&lat=5", 0},
		{"/postcode/search?q=SOUTH", 1},
		{"/postcode/search?q=ab1", 1},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := postcodeRequest(s, tc.path)
			var body struct {
				Results []json.RawMessage `json:"results"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || len(body.Results) != tc.count {
				t.Fatalf("response: %d %s", w.Code, w.Body.String())
			}
		})
	}
	w := postcodeRequest(s, "/postcode/AB1")
	var body struct {
		Geometry struct {
			Coordinates []json.RawMessage `json:"coordinates"`
		} `json:"geometry"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Geometry.Coordinates) != 2 {
		t.Fatalf("lookup lost an area: %s", w.Body.String())
	}
}

func TestPostcodePagination(t *testing.T) {
	s := &Server{postcodeIndex: postcodeTestIndex(t)}
	for _, tc := range []struct{ query, want string }{
		{"limit=1", "AB1"}, {"limit=1&offset=1", "CD2"},
		{"limit=1&offset=2", ""}, {"q=east&offset=1", ""},
		{"q=east&limit=1", "CD2"}, {"offset=999999", ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := postcodeRequest(s, "/postcode/search?"+tc.query)
			var body struct {
				Results []struct {
					Code string `json:"postcode"`
				} `json:"results"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
			if tc.want == "" {
				if len(body.Results) != 0 {
					t.Fatal(w.Body.String())
				}
			} else if len(body.Results) != 1 || body.Results[0].Code != tc.want {
				t.Fatal(w.Body.String())
			}
		})
	}
	for _, q := range []string{"limit=0", "limit=51", "limit=-1", "limit=abc", "limit=", "offset=-1", "offset=1.5", "offset=", "offset=9999999999999999999999999", "limit=1&limit=2", "offset=0&offset=1"} {
		if w := postcodeRequest(s, "/postcode/search?"+q); w.Code != 400 {
			t.Errorf("%s: status %d", q, w.Code)
		}
	}
}

func TestPostcodeInvalidCoordinates(t *testing.T) {
	s := &Server{postcodeIndex: postcodeTestIndex(t)}
	for _, q := range []string{"lon=NaN&lat=0", "lon=0&lat=NaN", "lon=Inf&lat=0", "lon=0&lat=-Inf", "lon=181&lat=0", "lon=-181&lat=0", "lon=0&lat=91", "lon=0&lat=-91", "lon=0", "lat=0"} {
		if w := postcodeRequest(s, "/postcode/at?"+q); w.Code != 400 {
			t.Errorf("%s: status %d", q, w.Code)
		}
	}
	for _, q := range []string{"lon=-180&lat=-90", "lon=180&lat=90"} {
		if w := postcodeRequest(s, "/postcode/at?"+q); w.Code != 200 {
			t.Errorf("%s: status %d", q, w.Code)
		}
	}
}

func BenchmarkPostcodeSearchNormalization(b *testing.B) {
	records := make([]postcodeRecord, 10000)
	for i := range records {
		code, name := fmt.Sprintf("AB%05d", i), "München Nord"
		records[i] = postcodeRecord{Code: code, Name: name, searchCode: strings.ToLower(code), searchNames: []string{strings.ToLower(name)}}
	}
	b.Run("per-request", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, rec := range records {
				if strings.Contains(strings.ToLower(rec.Code), "missing") || strings.Contains(strings.ToLower(rec.Name), "missing") {
					b.Fatal("unexpected match")
				}
			}
		}
	})
	b.Run("precomputed", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, rec := range records {
				if rec.matches("missing") {
					b.Fatal("unexpected match")
				}
			}
		}
	})
}

func TestPostcodeSearchDefaultLimitAndHTTPValidation(t *testing.T) {
	idx := &postcodeIndex{}
	for i := 0; i < 55; i++ {
		idx.all = append(idx.all, postcodeRecord{Code: fmt.Sprintf("%05d", i)})
	}
	s := &Server{postcodeIndex: idx}
	for _, tc := range []struct {
		path  string
		count int
	}{
		{"/postcode/search", 50}, {"/postcode/search?offset=50", 5},
	} {
		w := postcodeRequest(s, tc.path)
		var body struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Results) != tc.count {
			t.Fatalf("%s: %s", tc.path, w.Body.String())
		}
	}
	path := "/postcode/search?limit=1&offset=1"
	get := postcodeRequest(s, path)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("If-None-Match", get.Header().Get("ETag"))
		w := httptest.NewRecorder()
		s.servePostcode(w, r)
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Fatalf("conditional %s: %d %s", method, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.servePostcode(w, httptest.NewRequest(http.MethodHead, path, nil))
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("ETag") != get.Header().Get("ETag") {
		t.Fatalf("HEAD: %d %s", w.Code, w.Body.String())
	}
}
