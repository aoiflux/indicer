package near

import (
	"bytes"
	"encoding/base64"
	"indicer/lib/cnst"
	"io"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v3"
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

func visualise(fid []byte, idmap map[string]int64, db *badger.DB) error {
	page := components.NewPage()

	fid = bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	b64fid := base64.StdEncoding.EncodeToString(fid)

	nodes := []opts.SankeyNode{
		{
			Name: b64fid,
		},
	}
	var links []opts.SankeyLink

	for k, v := range idmap {
		var node opts.SankeyNode
		k = strings.Split(k, cnst.NamespaceSeperator)[1]
		b64k := base64.StdEncoding.EncodeToString([]byte(k))
		node.Name = b64k
		nodes = append(nodes, node)

		var link opts.SankeyLink
		link.Source = b64fid
		link.Target = b64k
		link.Value = float32(v)
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
