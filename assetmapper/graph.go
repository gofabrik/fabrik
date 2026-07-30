package assetmapper

import "sort"

type dependencyComponent struct {
	nodes  []string
	cyclic bool
}

// dependencyComponents condenses the dependency graph into strongly connected
// components and returns those components with dependencies before importers.
func dependencyComponents(deps map[string][]string) []dependencyComponent {
	nodes := make([]string, 0, len(deps))
	for node := range deps {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	index := 0
	indices := make(map[string]int, len(deps))
	lowlink := make(map[string]int, len(deps))
	onStack := make(map[string]bool, len(deps))
	var stack []string
	var components [][]string

	var connect func(string)
	connect = func(node string) {
		indices[node] = index
		lowlink[node] = index
		index++
		stack = append(stack, node)
		onStack[node] = true

		neighbors := append([]string(nil), deps[node]...)
		sort.Strings(neighbors)
		for _, dependency := range neighbors {
			if _, visited := indices[dependency]; !visited {
				connect(dependency)
				lowlink[node] = min(lowlink[node], lowlink[dependency])
			} else if onStack[dependency] {
				lowlink[node] = min(lowlink[node], indices[dependency])
			}
		}

		if lowlink[node] != indices[node] {
			return
		}
		var component []string
		for {
			last := len(stack) - 1
			member := stack[last]
			stack = stack[:last]
			onStack[member] = false
			component = append(component, member)
			if member == node {
				break
			}
		}
		sort.Strings(component)
		components = append(components, component)
	}

	for _, node := range nodes {
		if _, visited := indices[node]; !visited {
			connect(node)
		}
	}

	componentByNode := make(map[string]int, len(deps))
	for component, members := range components {
		for _, member := range members {
			componentByNode[member] = component
		}
	}

	componentDeps := make([]map[int]struct{}, len(components))
	dependents := make([]map[int]struct{}, len(components))
	for component := range components {
		componentDeps[component] = make(map[int]struct{})
		dependents[component] = make(map[int]struct{})
	}
	for node, dependencies := range deps {
		importerComponent := componentByNode[node]
		for _, dependency := range dependencies {
			dependencyComponent := componentByNode[dependency]
			if dependencyComponent == importerComponent {
				continue
			}
			componentDeps[importerComponent][dependencyComponent] = struct{}{}
			dependents[dependencyComponent][importerComponent] = struct{}{}
		}
	}

	var ready []int
	for component, dependencies := range componentDeps {
		if len(dependencies) == 0 {
			ready = append(ready, component)
		}
	}
	sortComponents(ready, components)

	ordered := make([]dependencyComponent, 0, len(components))
	for len(ready) > 0 {
		component := ready[0]
		ready = ready[1:]
		members := components[component]
		cyclic := len(members) > 1
		if !cyclic {
			for _, dependency := range deps[members[0]] {
				if dependency == members[0] {
					cyclic = true
					break
				}
			}
		}
		ordered = append(ordered, dependencyComponent{
			nodes:  members,
			cyclic: cyclic,
		})

		for dependent := range dependents[component] {
			delete(componentDeps[dependent], component)
			if len(componentDeps[dependent]) == 0 {
				ready = append(ready, dependent)
			}
		}
		sortComponents(ready, components)
	}
	return ordered
}

func sortComponents(componentsToSort []int, components [][]string) {
	sort.Slice(componentsToSort, func(i, j int) bool {
		return components[componentsToSort[i]][0] < components[componentsToSort[j]][0]
	})
}
