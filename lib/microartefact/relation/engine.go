package relation

import "indicer/lib/microartefact/model"

type Method struct {
	ID            string
	BuilderName   string
	RelationType  string
	Deterministic bool
	Enabled       bool
	Confidence    float32
}

type Builder interface {
	Name() string
	Build(file model.FileRecord, artefacts []model.Artefact, method Method) []model.Relation
}

type Engine struct {
	methods        []Method
	buildersByName map[string]Builder
}

func NewEngine(builders []Builder, methods []Method) *Engine {
	if len(builders) == 0 {
		builders = DefaultBuilders()
	}
	if len(methods) == 0 {
		methods = DefaultMethods()
	}

	lookup := make(map[string]Builder, len(builders))
	for _, builder := range builders {
		lookup[builder.Name()] = builder
	}
	return &Engine{methods: methods, buildersByName: lookup}
}

func (e *Engine) Relate(file model.FileRecord, artefacts []model.Artefact) []model.Relation {
	if len(artefacts) == 0 {
		return nil
	}
	relations := make([]model.Relation, 0, 32)
	for _, method := range e.methods {
		if !method.Enabled {
			continue
		}
		builder, ok := e.buildersByName[method.BuilderName]
		if !ok {
			continue
		}
		relations = append(relations, builder.Build(file, artefacts, method)...)
	}
	return dedupe(relations)
}

func dedupe(relations []model.Relation) []model.Relation {
	if len(relations) < 2 {
		return relations
	}
	seen := make(map[string]struct{}, len(relations))
	out := make([]model.Relation, 0, len(relations))
	for _, relation := range relations {
		key := relation.RelationType + "\x00" + relation.Method + "\x00" + relation.FromKind + "\x00" + relation.FromValue + "\x00" + relation.ToKind + "\x00" + relation.ToValue
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, relation)
	}
	return out
}
