package ir

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
)

const (
	ReferenceEnv      = "env"
	ReferenceService  = "service"
	ReferenceDerived  = "derived"
	ReferenceConsumer = "consumer"
	ReferenceStartup  = "startup"
)

// ReferenceNode and ReferenceEdge retain names and relationships only. They
// must never acquire source values, snippets, credentials or descriptions.
type ReferenceNode struct {
	ID      string
	Kind    string
	Name    string
	Missing bool
}
type ReferenceEdge struct {
	From  string
	To    string
	Kind  string
	Field string
}
type ReferenceGraph struct {
	Nodes  []ReferenceNode
	Edges  []ReferenceEdge
	Cycles [][]ReferenceNode
}

// ReferenceID gives distinct variable/service namespaces stable HTML anchors.
func ReferenceID(kind, name string) string {
	return fmt.Sprintf("reference-%x", sha256.Sum256([]byte(kind+"\x00"+name)))
}

// References describes the saved plan, not live state or a hand-edited YAML.
// A derived edge points from the input to the value that reads it; a startup
// edge points from the prerequisite service to the service waiting for it.
func (plan *IR) References() ReferenceGraph {
	graph := ReferenceGraph{}
	nodes := map[string]ReferenceNode{}
	defined := map[string]bool{}
	for _, entry := range plan.EnvFile {
		defined[ReferenceID(ReferenceEnv, entry.Key)] = true
	}
	for _, service := range plan.Services {
		defined[ReferenceID(ReferenceService, service.Name)] = true
	}
	add := func(kind, name string) string {
		id := ReferenceID(kind, name)
		nodes[id] = ReferenceNode{ID: id, Kind: kind, Name: name, Missing: !defined[id]}
		return id
	}
	edges := map[ReferenceEdge]bool{}
	edge := func(from, to, kind, field string) {
		edges[ReferenceEdge{From: from, To: to, Kind: kind, Field: field}] = true
	}
	entries := map[string]EnvFileEntry{}
	for _, entry := range plan.EnvFile {
		entries[entry.Key] = entry
		target := add(ReferenceEnv, entry.Key)
		if entry.Generate != "" {
			continue
		}
		for _, name := range referenceNames(entry.Value, true) {
			edge(add(ReferenceEnv, name), target, ReferenceDerived, "")
		}
	}
	for _, service := range plan.Services {
		target := add(ReferenceService, service.Name)
		for _, variable := range service.Env {
			if variable.Ref != "" {
				field := variable.Key
				if entry, found := entries[variable.Ref]; found && entry.AsFile() {
					field = entry.FileEnvKey
				}
				edge(add(ReferenceEnv, variable.Ref), target, ReferenceConsumer, field)
			} else {
				for _, name := range referenceNames(variable.Literal, false) {
					edge(add(ReferenceEnv, name), target, ReferenceConsumer, variable.Key)
				}
			}
		}
		for _, part := range []struct {
			name  string
			texts []string
		}{{"command", service.Command}, {"entrypoint", service.Entrypoint}, {"healthcheck", service.Healthcheck.Test}} {
			for _, text := range part.texts {
				for _, name := range referenceNames(text, false) {
					edge(add(ReferenceEnv, name), target, ReferenceConsumer, part.name)
				}
			}
		}
		for _, dependency := range service.DependsOn {
			edge(add(ReferenceService, dependency.Service), target, ReferenceStartup, "")
		}
	}
	// Optional settings are emitted only once, with the same first-name wins
	// rule as render.OptionalSettings. Active env/refs suppress passthroughs.
	active := map[string]bool{}
	for _, entry := range plan.EnvFile {
		active[entry.Key] = true
	}
	for _, service := range plan.Services {
		for _, variable := range service.Env {
			active[variable.Key] = true
		}
	}
	for _, setting := range plan.OptionalEnv {
		if active[setting.Key] {
			continue
		}
		active[setting.Key] = true
		if defined[ReferenceID(ReferenceService, setting.Service)] {
			id := add(ReferenceEnv, setting.Key)
			node := nodes[id]
			node.Missing = false
			nodes[id] = node
			edge(id, add(ReferenceService, setting.Service), ReferenceConsumer, setting.Key)
		}
	}
	for _, node := range nodes {
		graph.Nodes = append(graph.Nodes, node)
	}
	slices.SortFunc(graph.Nodes, func(a, b ReferenceNode) int { return strings.Compare(a.Kind+"\x00"+a.Name, b.Kind+"\x00"+b.Name) })
	for item := range edges {
		graph.Edges = append(graph.Edges, item)
	}
	slices.SortFunc(graph.Edges, func(a, b ReferenceEdge) int {
		return strings.Compare(a.From+a.To+a.Kind+a.Field, b.From+b.To+b.Kind+b.Field)
	})
	graph.Cycles = referenceCycles(graph)
	return graph
}

// referenceNames follows YAML dollar escaping. The env renderer treats bare
// dollar expressions as literals and interpolates braced expressions (including
// one preceded by another dollar, which the renderer quotes separately).
func referenceNames(raw string, bracedOnly bool) []string {
	names := map[string]bool{}
	word := func(b byte) bool { return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }
	for i := 0; i < len(raw); i++ {
		if raw[i] != '$' || i+1 >= len(raw) {
			continue
		}
		if raw[i+1] == '$' && !bracedOnly {
			i++
			continue
		}
		start := i + 1
		braced := raw[start] == '{'
		if braced {
			start++
		} else if bracedOnly {
			continue
		}
		if start >= len(raw) || !word(raw[start]) {
			continue
		}
		end := start + 1
		for end < len(raw) && (word(raw[end]) || raw[end] >= '0' && raw[end] <= '9') {
			end++
		}
		if braced && strings.IndexByte(raw[end:], '}') < 0 {
			continue
		}
		names[raw[start:end]] = true
	}
	var result []string
	for name := range names {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

// Consumers returns service consumers reachable through derived values only.
// Startup ordering does not mean a service receives a prerequisite's values.
func (graph ReferenceGraph) Consumers(key string) []ReferenceNode {
	seen := map[string]bool{ReferenceID(ReferenceEnv, key): true}
	queue := []string{ReferenceID(ReferenceEnv, key)}
	outgoing := map[string][]ReferenceEdge{}
	for _, edge := range graph.Edges {
		if edge.Kind != ReferenceStartup {
			outgoing[edge.From] = append(outgoing[edge.From], edge)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, edge := range outgoing[id] {
			if !seen[edge.To] {
				seen[edge.To] = true
				queue = append(queue, edge.To)
			}
		}
	}
	var result []ReferenceNode
	for _, node := range graph.Nodes {
		if node.Kind == ReferenceService && seen[node.ID] {
			result = append(result, node)
		}
	}
	return result
}

// Tarjan's components report every cycle member once, including self loops.
func referenceCycles(graph ReferenceGraph) [][]ReferenceNode {
	nodes := map[string]ReferenceNode{}
	next := map[string][]string{}
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range graph.Edges {
		next[edge.From] = append(next[edge.From], edge.To)
	}
	index, low := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var cycles [][]ReferenceNode
	counter := 0
	var visit func(string)
	visit = func(id string) {
		counter++
		index[id] = counter
		low[id] = counter
		stack = append(stack, id)
		onStack[id] = true
		for _, to := range next[id] {
			if index[to] == 0 {
				visit(to)
				low[id] = min(low[id], low[to])
			} else if onStack[to] {
				low[id] = min(low[id], index[to])
			}
		}
		if low[id] != index[id] {
			return
		}
		var component []ReferenceNode
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, nodes[last])
			if last == id {
				break
			}
		}
		if len(component) > 1 || slices.Contains(next[id], id) {
			slices.SortFunc(component, func(a, b ReferenceNode) int { return strings.Compare(a.Kind+a.Name, b.Kind+b.Name) })
			cycles = append(cycles, component)
		}
	}
	for _, node := range graph.Nodes {
		if index[node.ID] == 0 {
			visit(node.ID)
		}
	}
	return cycles
}
