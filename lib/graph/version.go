package graph

import "runtime/debug"

const grapheneModule = "github.com/aoiflux/graphene"

// linkedDuesVersion and linkedGrapheneVersion are the versions this binary
// was built from, as its build information records them. They were constants
// once, and went stale when graphene moved from v0.4.0 to v0.9.0 (GRA-11).
var linkedDuesVersion, linkedGrapheneVersion = linkedVersions()

// writtenBy is recorded in the schema marker so a store says which build
// stamped it.
var writtenBy = "dues " + linkedDuesVersion + " / graphene " + linkedGrapheneVersion

func linkedVersions() (dues, graphene string) {
	dues, graphene = "(devel)", "(unknown)"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return dues, graphene
	}
	if v := info.Main.Version; v != "" {
		dues = v
	}
	for _, dep := range info.Deps {
		if dep.Path != grapheneModule {
			continue
		}
		graphene = dep.Version
		if dep.Replace != nil && dep.Replace.Version != "" {
			graphene = dep.Replace.Version
		}
	}
	return dues, graphene
}
