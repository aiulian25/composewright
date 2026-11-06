package web

import (
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/pipeline"
)

type referenceUse struct {
	Target ir.ReferenceNode
	Field  string
}
type serviceReference struct {
	Node       ir.ReferenceNode
	DependsOn  []ir.ReferenceNode
	Dependents []ir.ReferenceNode
}
type referenceView struct {
	Base     string
	Number   int
	Edited   bool
	Services []serviceReference
	Missing  []ir.ReferenceNode
	Cycles   [][]ir.ReferenceNode
}

func referenceUses(graph ir.ReferenceGraph, from string) []referenceUse {
	nodes := map[string]ir.ReferenceNode{}
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	var uses []referenceUse
	for _, edge := range graph.Edges {
		if edge.From == from && edge.Kind != ir.ReferenceStartup {
			uses = append(uses, referenceUse{Target: nodes[edge.To], Field: edge.Field})
		}
	}
	return uses
}

func referenceViewFor(stored *pipeline.Stored) referenceView {
	graph := stored.Plan.References()
	view := referenceView{Base: stored.Generation.ID, Number: stored.Generation.Number, Edited: stored.Options.Edited, Cycles: graph.Cycles}
	nodes := map[string]ir.ReferenceNode{}
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	for _, node := range graph.Nodes {
		if node.Kind == ir.ReferenceEnv && node.Missing {
			view.Missing = append(view.Missing, node)
		}
		if node.Kind != ir.ReferenceService {
			continue
		}
		service := serviceReference{Node: node}
		for _, edge := range graph.Edges {
			if edge.Kind == ir.ReferenceStartup {
				if edge.To == node.ID {
					service.DependsOn = append(service.DependsOn, nodes[edge.From])
				}
				if edge.From == node.ID {
					service.Dependents = append(service.Dependents, nodes[edge.To])
				}
			}
		}
		view.Services = append(view.Services, service)
	}
	return view
}
