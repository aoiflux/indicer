package server

import (
	"context"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/search"
	"indicer/pb"
	"strings"
	"time"
)

// searchBudget bounds the time one search request may take (SRC-2).
const searchBudget = 2 * time.Minute

func (g *GrpcService) Search(ctx context.Context, req *pb.SearchReq) (*pb.SearchRes, error) {
	keyword := strings.TrimSpace(strings.ToLower(req.GetKeyword()))
	ctx, cancel := context.WithTimeout(ctx, searchBudget)
	defer cancel()
	keywordCountMap, totalCount, err := search.SearchSummaryWithContext(ctx, keyword, cnst.DB)
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("search exceeded its %v time budget", searchBudget)
	}
	if err != nil {
		return nil, err
	}

	return &pb.SearchRes{
		Err:             "",
		TotalCount:      totalCount,
		KeywordCountMap: keywordCountMap,
	}, nil
}
