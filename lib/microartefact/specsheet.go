package microartefact

import specpkg "indicer/lib/microartefact/spec"

type SpecSheet = specpkg.Sheet
type DetectionRule = specpkg.DetectionRule
type RelationRule = specpkg.RelationRule

func DefaultSpecSheet() SpecSheet {
	return specpkg.Default()
}
