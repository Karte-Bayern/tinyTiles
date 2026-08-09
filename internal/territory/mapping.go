package territory

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MappingRow is one row of an external territory mapping (a CSV or JSON
// table joining a geometry key like "postcode" to business attributes such
// as "territory", "employee", "vehicle" or "depot").
type MappingRow map[string]string

// MappingTable is a mapping file's rows plus a lookup index built by Index.
type MappingTable struct {
	Rows   []MappingRow
	Header []string
}

// LoadMapping reads a CSV or JSON (array-of-objects) mapping file. Format is
// chosen by file extension; every other extension is an error rather than a
// silent guess.
func LoadMapping(path string) (*MappingTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("territory: read mapping %q: %w", path, err)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return parseCSVMapping(data)
	case ".json":
		return parseJSONMapping(data)
	default:
		return nil, fmt.Errorf("territory: mapping %q: unsupported extension (want .csv or .json)", path)
	}
}

func parseCSVMapping(data []byte) (*MappingTable, error) {
	reader := csv.NewReader(strings.NewReader(string(data)))
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("territory: parse CSV mapping: %w", err)
	}
	if len(records) == 0 {
		return &MappingTable{}, nil
	}
	header := records[0]
	rows := make([]MappingRow, 0, len(records)-1)
	for _, record := range records[1:] {
		row := make(MappingRow, len(header))
		for i, column := range header {
			if i < len(record) {
				row[column] = strings.TrimSpace(record[i])
			}
		}
		rows = append(rows, row)
	}
	return &MappingTable{Rows: rows, Header: header}, nil
}

func parseJSONMapping(data []byte) (*MappingTable, error) {
	var records []map[string]any
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("territory: parse JSON mapping (want an array of objects): %w", err)
	}
	headerSeen := map[string]bool{}
	var header []string
	rows := make([]MappingRow, 0, len(records))
	for _, record := range records {
		row := make(MappingRow, len(record))
		for k, v := range record {
			row[k] = stringify(v)
			if !headerSeen[k] {
				headerSeen[k] = true
				header = append(header, k)
			}
		}
		rows = append(rows, row)
	}
	return &MappingTable{Rows: rows, Header: header}, nil
}

// Index builds a lookup from the normalized value of keyColumn to every row
// sharing it (mapping keys are not assumed unique — DuplicateKeys reports
// that separately), plus the count of blank keys.
func (t *MappingTable) Index(keyColumn string) map[string][]MappingRow {
	index := make(map[string][]MappingRow)
	for _, row := range t.Rows {
		key := normalizeKey(row[keyColumn])
		if key == "" {
			continue
		}
		index[key] = append(index[key], row)
	}
	return index
}

// DuplicateKeys returns the normalized key values that occur on more than
// one mapping row, sorted, for a validate report.
func (t *MappingTable) DuplicateKeys(keyColumn string) []string {
	counts := make(map[string]int)
	for _, row := range t.Rows {
		key := normalizeKey(row[keyColumn])
		if key == "" {
			continue
		}
		counts[key]++
	}
	var dups []string
	for key, n := range counts {
		if n > 1 {
			dups = append(dups, key)
		}
	}
	return SortedUnique(dups)
}
