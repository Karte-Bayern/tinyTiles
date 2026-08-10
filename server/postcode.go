package server

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Karte-Bayern/tinyTiles/v2/internal/geo"
)

// postcodeSearchLimit bounds a single /postcode/search response — this is a
// lightweight lookup index, not a paginated search API.
const postcodeSearchLimit = 50

// postcodeRecord is one postal-code boundary loaded from the GeoJSON sidecar
// a PostalCodes-enabled build writes (see BuildPBF/`tinytiles build
// --postal-codes` and internal/minigen's postal_code layer).
type postcodeRecord struct {
	Code     string
	Name     string
	Geometry geo.MultiPolygon
	Center   [2]float64
	BBox     [4]float64
	HasBBox  bool
}

// postcodeIndex is immutable once loaded and read concurrently without
// locking, the same way other Server fields set once in New are.
type postcodeIndex struct {
	byCode map[string]postcodeRecord
	all    []postcodeRecord // sorted by Code
}

func loadPostcodeIndex(path string) (*postcodeIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	features, err := geo.ReadFeatures(data)
	if err != nil {
		return nil, err
	}
	idx := &postcodeIndex{byCode: make(map[string]postcodeRecord, len(features))}
	for _, f := range features {
		code := propertyString(f.Properties, "postcode")
		if code == "" {
			continue
		}
		rec := postcodeRecord{Code: code, Name: propertyString(f.Properties, "name"), Geometry: f.Geometry}
		if bbox, ok := geo.BBox(f.Geometry); ok {
			rec.BBox, rec.HasBBox = bbox, true
			rec.Center = [2]float64{(bbox[0] + bbox[2]) / 2, (bbox[1] + bbox[3]) / 2}
		}
		idx.byCode[normalizePostcode(code)] = rec
		idx.all = append(idx.all, rec)
	}
	sort.Slice(idx.all, func(i, j int) bool { return idx.all[i].Code < idx.all[j].Code })
	return idx, nil
}

func propertyString(properties map[string]any, key string) string {
	s, _ := properties[key].(string)
	return strings.TrimSpace(s)
}

func normalizePostcode(code string) string { return strings.ToLower(strings.TrimSpace(code)) }

// servePostcode dispatches GET /postcode/{code}, /postcode/search and
// /postcode/at from one mux entry, mirroring XYZHandler's own
// prefix-then-parse style rather than the stdlib's newer method/wildcard mux
// patterns the rest of this package does not otherwise use.
func (s *Server) servePostcode(w http.ResponseWriter, r *http.Request) {
	if s.postcodeIndex == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	switch rest := strings.TrimPrefix(r.URL.Path, "/postcode/"); rest {
	case "":
		http.NotFound(w, r)
	case "search":
		s.servePostcodeSearch(w, r)
	case "at":
		s.servePostcodeAt(w, r)
	default:
		s.servePostcodeLookup(w, r, rest)
	}
}

// servePostcodeLookup answers "what does postcode X look like" — the
// suche-postleitzahl.org lookup use case — with the full boundary geometry.
func (s *Server) servePostcodeLookup(w http.ResponseWriter, r *http.Request, code string) {
	rec, ok := s.postcodeIndex.byCode[normalizePostcode(code)]
	if !ok {
		http.Error(w, "postcode not found", http.StatusNotFound)
		return
	}
	payload, err := json.Marshal(postcodeSummary(rec, true))
	if err != nil {
		http.Error(w, "encode postcode", http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, payload, nil, quoteETag(digest(payload)), "public, max-age=3600, stale-while-revalidate=300")
}

// servePostcodeSearch answers "which postcodes match this text" — a
// search-as-you-type box, the way suche-postleitzahl.org's own search
// works. Geometry is omitted; a search result only needs enough to let a
// client pick one, then look it up.
func (s *Server) servePostcodeSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	results := make([]map[string]any, 0, postcodeSearchLimit)
	for _, rec := range s.postcodeIndex.all {
		if query != "" && !strings.Contains(strings.ToLower(rec.Code), query) && !strings.Contains(strings.ToLower(rec.Name), query) {
			continue
		}
		results = append(results, postcodeSummary(rec, false))
		if len(results) >= postcodeSearchLimit {
			break
		}
	}
	writePostcodeResults(w, r, results)
}

// servePostcodeAt answers "which postcode contains this point" — reverse
// lookup for a map click or an address's geocoded coordinate, the natural
// complement to lookup-by-code and the concrete case behind the territory
// builder's stated goal of datasets a consumer can query "which territory
// contains this coordinate" against.
func (s *Server) servePostcodeAt(w http.ResponseWriter, r *http.Request) {
	lon, lonErr := strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
	lat, latErr := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	if lonErr != nil || latErr != nil {
		http.Error(w, "lon and lat query parameters are required", http.StatusBadRequest)
		return
	}
	point := geo.Point{lon, lat}
	results := make([]map[string]any, 0, 1)
	for _, rec := range s.postcodeIndex.all {
		if rec.HasBBox && !bboxContainsPoint(rec.BBox, point) {
			continue
		}
		if geo.Contains(rec.Geometry, point) {
			results = append(results, postcodeSummary(rec, false))
		}
	}
	writePostcodeResults(w, r, results)
}

func bboxContainsPoint(bbox [4]float64, p geo.Point) bool {
	return p[0] >= bbox[0] && p[0] <= bbox[2] && p[1] >= bbox[1] && p[1] <= bbox[3]
}

func writePostcodeResults(w http.ResponseWriter, r *http.Request, results []map[string]any) {
	payload, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		http.Error(w, "encode postcode results", http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, payload, nil, quoteETag(digest(payload)), "public, max-age=60")
}

func postcodeSummary(rec postcodeRecord, includeGeometry bool) map[string]any {
	out := map[string]any{"postcode": rec.Code}
	if rec.Name != "" {
		out["name"] = rec.Name
	}
	if rec.HasBBox {
		out["center"] = []float64{rec.Center[0], rec.Center[1]}
		out["bbox"] = []float64{rec.BBox[0], rec.BBox[1], rec.BBox[2], rec.BBox[3]}
	}
	if includeGeometry {
		out["geometry"] = geo.ToGeoJSONGeometry(rec.Geometry)
	}
	return out
}
