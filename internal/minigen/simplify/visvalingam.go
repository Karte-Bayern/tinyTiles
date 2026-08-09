// Package simplify implements Visvalingam-Whyatt line and polygon-ring
// generalization, the same "weighted area" method mapshaper.org uses as its
// default simplification algorithm. It operates on plain coordinates so
// callers can apply it in whichever space (geographic or projected tile
// pixels) suits them.
package simplify

import (
	"container/heap"
	"math"
)

// Point is a plain 2D coordinate.
type Point [2]float64

// Line reduces an open polyline, repeatedly discarding the point that forms
// the smallest triangle area with its current neighbors until every
// remaining interior point's area exceeds tolerance. The first and last
// points are never removed. A tolerance <= 0 or too few points returns the
// input unchanged.
func Line(points []Point, tolerance float64) []Point {
	return simplify(points, tolerance, false)
}

// Ring reduces a closed polygon ring given as distinct points (no repeated
// closing point). Unlike Line, every vertex is a removal candidate; at least
// three points are always kept so the result remains a valid polygon.
func Ring(points []Point, tolerance float64) []Point {
	return simplify(points, tolerance, true)
}

// node is one point in the doubly linked list simplify walks. prev/next hold
// -1 for a line's fixed endpoints, which are never removable.
type node struct {
	p          Point
	prev, next int
	alive      bool
	area       float64
	version    int
}

// heapItem is a priority-queue entry naming the area a node had when it was
// pushed. version pairs it with the node's current state so a pop can detect
// a stale entry left behind by an earlier neighbor recomputation instead of
// tracking down and mutating queued entries in place.
type heapItem struct {
	idx     int
	area    float64
	version int
}

type priorityQueue []heapItem

func (pq priorityQueue) Len() int           { return len(pq) }
func (pq priorityQueue) Less(i, j int) bool { return pq[i].area < pq[j].area }
func (pq priorityQueue) Swap(i, j int)      { pq[i], pq[j] = pq[j], pq[i] }
func (pq *priorityQueue) Push(x any)        { *pq = append(*pq, x.(heapItem)) }
func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[:n-1]
	return item
}

func simplify(points []Point, tolerance float64, closed bool) []Point {
	n := len(points)
	minKeep := 2
	if closed {
		minKeep = 3
	}
	if n <= minKeep || tolerance <= 0 {
		return points
	}

	nodes := make([]node, n)
	for i, p := range points {
		nodes[i] = node{p: p, prev: i - 1, next: i + 1, alive: true}
	}
	if closed {
		nodes[0].prev = n - 1
		nodes[n-1].next = 0
	} else {
		nodes[0].prev = -1
		nodes[n-1].next = -1
	}

	removable := func(i int) bool { return nodes[i].prev >= 0 && nodes[i].next >= 0 }

	pq := make(priorityQueue, 0, n)
	for i := range nodes {
		if !removable(i) {
			continue
		}
		nodes[i].area = triangleArea(nodes[nodes[i].prev].p, nodes[i].p, nodes[nodes[i].next].p)
		pq = append(pq, heapItem{idx: i, area: nodes[i].area})
	}
	heap.Init(&pq)

	alive := n
	for pq.Len() > 0 && alive > minKeep {
		item := heap.Pop(&pq).(heapItem)
		if !nodes[item.idx].alive || item.version != nodes[item.idx].version {
			continue // superseded by a later recomputation of this node's area
		}
		if item.area > tolerance {
			break
		}
		removed := nodes[item.idx]
		nodes[item.idx].alive = false
		if removed.prev >= 0 {
			nodes[removed.prev].next = removed.next
		}
		if removed.next >= 0 {
			nodes[removed.next].prev = removed.prev
		}
		alive--
		for _, neighbor := range [2]int{removed.prev, removed.next} {
			if neighbor < 0 || !nodes[neighbor].alive || !removable(neighbor) {
				continue
			}
			nodes[neighbor].area = triangleArea(nodes[nodes[neighbor].prev].p, nodes[neighbor].p, nodes[nodes[neighbor].next].p)
			nodes[neighbor].version++
			heap.Push(&pq, heapItem{idx: neighbor, area: nodes[neighbor].area, version: nodes[neighbor].version})
		}
	}

	out := make([]Point, 0, alive)
	start := 0
	for !nodes[start].alive {
		start++
	}
	if closed {
		for i := start; ; {
			out = append(out, nodes[i].p)
			i = nodes[i].next
			if i == start {
				break
			}
		}
	} else {
		for i := start; i != -1; i = nodes[i].next {
			out = append(out, nodes[i].p)
		}
	}
	return out
}

func triangleArea(a, b, c Point) float64 {
	return math.Abs((b[0]-a[0])*(c[1]-a[1])-(c[0]-a[0])*(b[1]-a[1])) / 2
}
