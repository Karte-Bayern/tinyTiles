package minigen

// This file adds OSM relation decoding to the wire reader in pbf.go.
// Relations are the only primitive scanPBF does not already visit; postal.go
// uses them to assemble boundary=postal_code multipolygons.

import (
	"context"
	"fmt"
)

// memberType mirrors OSM PBF's Relation.MemberType enum.
type memberType int

const (
	memberNode memberType = iota
	memberWay
	memberRelation
)

type relationMember struct {
	Type memberType
	Ref  int64
	Role string
}

type relation struct {
	ID      int64
	Tags    map[string]string
	Members []relationMember
}

// scanPBFRelations re-reads a PBF file's primitive blocks, visiting only
// relations. It is a separate pass from scanPBF's node/way scan so the much
// more common node/way path stays free of relation bookkeeping; the
// generator already re-scans its inputs once per build phase.
func scanPBFRelations(ctx context.Context, path string, visit func(*relation) error) error {
	return scanBlocks(ctx, path, func(data []byte) error {
		return parsePrimitiveBlockRelations(data, visit)
	})
}

func parsePrimitiveBlockRelations(b []byte, visit func(*relation) error) error {
	var stringsTable []string
	var groups [][]byte
	if err := walkProto(b, func(num, wire int, value []byte, _ uint64) error {
		switch num {
		case 1:
			if wire == 2 {
				stringsTable = parseStringTable(value)
			}
		case 2:
			if wire == 2 {
				groups = append(groups, value)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, group := range groups {
		if err := parsePrimitiveGroupRelations(group, stringsTable, visit); err != nil {
			return err
		}
	}
	return nil
}

func parsePrimitiveGroupRelations(b []byte, table []string, visit func(*relation) error) error {
	return walkProto(b, func(num, wire int, value []byte, _ uint64) error {
		if wire != 2 || num != 4 {
			return nil
		}
		r, err := parseRelation(value, table)
		if err != nil {
			return err
		}
		return visit(r)
	})
}

func parseRelation(b []byte, table []string) (*relation, error) {
	r := &relation{Tags: map[string]string{}}
	var keys, vals, types, rolesSid, memidsDelta []uint64
	err := walkProto(b, func(n, wire int, v []byte, x uint64) error {
		switch n {
		case 1:
			if wire == 0 {
				r.ID = int64(x)
			}
		case 2:
			if wire == 2 {
				keys = packedVarints(v)
			}
		case 3:
			if wire == 2 {
				vals = packedVarints(v)
			}
		case 8:
			if wire == 2 {
				rolesSid = packedVarints(v)
			}
		case 9:
			if wire == 2 {
				memidsDelta = packedVarints(v)
			}
		case 10:
			if wire == 2 {
				types = packedVarints(v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(keys) && i < len(vals); i++ {
		k, v := int(keys[i]), int(vals[i])
		if k < len(table) && v < len(table) {
			r.Tags[table[k]] = table[v]
		}
	}
	if len(memidsDelta) != len(types) {
		return nil, fmt.Errorf("relation %d: member id/type count mismatch", r.ID)
	}
	var memid int64
	for i := range memidsDelta {
		memid += zigzag(memidsDelta[i])
		role := ""
		if i < len(rolesSid) {
			if idx := int(rolesSid[i]); idx >= 0 && idx < len(table) {
				role = table[idx]
			}
		}
		r.Members = append(r.Members, relationMember{Type: memberType(types[i]), Ref: memid, Role: role})
	}
	return r, nil
}
