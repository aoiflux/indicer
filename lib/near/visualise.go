package near

import (
	"bytes"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"strings"

	"github.com/dgraph-io/badger/v4"
)

// type viz struct {
// 	graph *cgraph.Graph
// 	db    *badger.DB
// }

// func visualise(fid []byte, idmap *structs.ConcMap, db *badger.DB) error {
// 	var vg viz
// 	vg.db = db

// 	g := graphviz.New()
// 	defer g.Close()

// 	var err error
// 	vg.graph, err = g.Graph()
// 	if err != nil {
// 		return err
// 	}

// 	title, err := vg.graph.CreateNode("title")
// 	if err != nil {
// 		return err
// 	}
// 	title.SetLabel("Artefact Relation Graph - Arbitrary & Unique Artefact Name Aliases are used")
// 	title.SetShape(cgraph.BoxShape)
// 	title.SetColor("blue")

// 	err = vg.populateGraph(fid, idmap)
// 	if err != nil {
// 		return err
// 	}

// 	vg.graph.SetRankDir(cgraph.LRRank)
// 	return g.RenderFilename(vg.graph, "svg", "./graph.svg")
// }

// func (vg *viz) populateGraph(fid []byte, idmap *structs.ConcMap) error {
// 	fidNode, err := vg.groupNodes(fid, true)
// 	if err != nil {
// 		return err
// 	}

// 	var count int
// 	for id, confidence := range idmap.GetData() {
// 		count++
// 		toNode, err := vg.groupNodes([]byte(id), true)
// 		if err != nil {
// 			return err
// 		}

// 		ename := fmt.Sprintf("e_%d", count)
// 		edge, err := vg.graph.CreateEdge(ename, fidNode, toNode)
// 		if err != nil {
// 			return err
// 		}
// 		edge.SetLabel(fmt.Sprintf("related | confidence: %f%%", confidence))
// 		edge.SetLabelFontColor("blue")
// 		edge.SetColor("blue")
// 		width := confidence / 10
// 		if width < 0.5 {
// 			width = 0.5
// 		}
// 		edge.SetPenWidth(width)
// 	}

// 	return nil
// }

// func (vg *viz) groupNodes(id []byte, isTarget bool, tonodes ...*cgraph.Node) (*cgraph.Node, error) {
// 	names, err := GetNames(id, vg.db, true)
// 	if err != nil {
// 		return nil, err
// 	}

// 	var idx int
// 	var idnode *cgraph.Node
// 	for name := range names {
// 		idx++

// 		split := strings.Split(name, cnst.DataSeperator)
// 		name = split[len(split)-1]
// 		node, err := vg.graph.CreateNode(name)
// 		if err != nil {
// 			return nil, err
// 		}

// 		if len(tonodes) == 0 {
// 			split := bytes.Split(id, []byte(cnst.NamespaceSeperator))
// 			encoded := base64.StdEncoding.EncodeToString(split[1])
// 			idnode, err = vg.graph.CreateNode(encoded)
// 			if err != nil {
// 				return nil, err
// 			}

// 			edge, err := vg.graph.CreateEdge(name, idnode, node)
// 			if err != nil {
// 				return nil, err
// 			}
// 			edge.SetLabel("alias")
// 		}

// 		for _, tonode := range tonodes {
// 			edge, err := vg.graph.CreateEdge(name, node, tonode)
// 			if err != nil {
// 				return nil, err
// 			}
// 			edge.SetLabel("child")
// 		}

// 		if len(split) > 1 {
// 			encoded := split[len(split)-2]
// 			decoded, err := base64.StdEncoding.DecodeString(encoded)
// 			if err != nil {
// 				return nil, err
// 			}

// 			id := util.AppendToBytesSlice(cnst.PartiFileNamespace, decoded)
// 			if len(split) == 2 {
// 				id = util.AppendToBytesSlice(cnst.EviFileNamespace, decoded)
// 			}

// 			if isTarget {
// 				node = idnode
// 			}
// 			_, err = vg.groupNodes(id, false, node)
// 			if err != nil {
// 				return nil, err
// 			}
// 		}
// 	}

// 	return idnode, nil
// }

func GetNames(id []byte, db *badger.DB, unique ...bool) (map[string]struct{}, error) {
	var uniqueFlag bool
	if len(unique) > 0 {
		uniqueFlag = unique[0]
	}

	if bytes.HasPrefix(id, []byte(cnst.IdxFileNamespace)) {
		ifile, err := dbio.GetIndexedFile(id, db)
		if uniqueFlag {
			return getUniqueNames(ifile.Names), err
		}
		return ifile.Names, err
	}

	if bytes.HasPrefix(id, []byte(cnst.PartiFileNamespace)) {
		pfile, err := dbio.GetPartitionFile(id, db)
		if uniqueFlag {
			return getUniqueNames(pfile.Names), err
		}
		return pfile.Names, err
	}

	efile, err := dbio.GetEvidenceFile(id, db)
	if err != nil {
		return nil, err
	}
	return efile.Names, err
}

func getUniqueNames(names map[string]struct{}) map[string]struct{} {
	uniqueMap := make(map[string]struct{})
	interimMap := make(map[string]string)

	for name := range names {
		split := strings.Split(name, cnst.DataSeperator)
		val := split[len(split)-1]

		split = split[:len(split)-1]
		key := strings.Join(split, cnst.DataSeperator)
		interimMap[key] = val
		delete(names, name)
	}

	for inKey, inVal := range interimMap {
		fullName := inKey + cnst.DataSeperator + inVal
		uniqueMap[fullName] = struct{}{}
		delete(interimMap, inKey)
	}

	return uniqueMap
}
