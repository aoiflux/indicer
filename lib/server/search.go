package server

import (
	"context"
	"indicer/lib/cnst"
	"indicer/lib/search"
	"indicer/pb"
	"strings"
)

func (g *GrpcService) Search(ctx context.Context, req *pb.SearchReq) (*pb.SearchRes, error) {
	keyword := strings.TrimSpace(strings.ToLower(req.GetKeyword()))
	keywordCountMap, totalCount, err := search.SearchSummaryWithContext(ctx, keyword, cnst.DB)
	if err != nil {
		return nil, err
	}

	return &pb.SearchRes{
		Err:             "",
		TotalCount:      totalCount,
		KeywordCountMap: keywordCountMap,
	}, nil
}
