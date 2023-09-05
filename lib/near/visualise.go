package near

import (
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"io"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/go-echarts/go-echarts/v2/opts"
)

const title = "Graph of Related Artefacts"

func sankeyBase(series string, nodes []opts.SankeyNode, links []opts.SankeyLink) *charts.Sankey {
	sankey := charts.NewSankey()
	sankey.SetGlobalOptions(
		charts.WithTitleOpts(opts.Title{
			Title: title,
		}),
		charts.WithTooltipOpts(opts.Tooltip{Show: true}),
	)

	labels := charts.WithLabelOpts(opts.Label{
		Show:      true,
		Color:     "#000",
		FontSize:  14,
		Formatter: "{b}",
	})
	sankey.AddSeries(series, nodes, links, labels)
	return sankey
}

func visualise(fid []byte, idmap *structs.ConcMap, db *badger.DB) error {
	page := components.NewPage()
	page.Initialization.AssetsHost = "."
	b64fid := getB64HashedID(string(fid))

	nodes := []opts.SankeyNode{
		{
			Name: b64fid,
		},
	}
	var links []opts.SankeyLink

	for k, v := range idmap.GetData() {
		var node opts.SankeyNode
		node.Name = getB64HashedID(k)
		nodes = append(nodes, node)

		var link opts.SankeyLink
		link.Source = b64fid
		link.Target = node.Name
		link.Value = v
		links = append(links, link)
	}

	page.PageTitle = title
	page.AddCharts(sankeyBase("GReAt", nodes, links))

	f, err := os.Create("./graph.html")
	if err != nil {
		return err
	}
	defer f.Close()
	return page.Render(io.MultiWriter(f))
}

func getB64HashedID(id string) string {
	splits := strings.Split(id, cnst.NamespaceSeperator)
	b64hash := base64.StdEncoding.EncodeToString([]byte(splits[1]))
	return fmt.Sprintf("%s%s%s", splits[0], cnst.NamespaceSeperator, b64hash)
}
