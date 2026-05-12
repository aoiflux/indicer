package server

import (
	"context"
	"indicer/lib/cnst"
	"indicer/pb"
	"testing"

	"github.com/dgraph-io/badger/v4"
)

func TestGrpcServiceSearchRespectsCanceledContext(t *testing.T) {
	prevDB := cnst.DB
	cnst.DB = nil
	t.Cleanup(func() {
		cnst.DB = prevDB
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc := &GrpcService{}
	_, err := svc.Search(ctx, &pb.SearchReq{Keyword: "ab"})
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestGrpcServiceSearchSuccessContractOnEmptyDB(t *testing.T) {
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	defer db.Close()

	prevDB := cnst.DB
	cnst.DB = db
	t.Cleanup(func() {
		cnst.DB = prevDB
	})

	svc := &GrpcService{}
	res, err := svc.Search(context.Background(), &pb.SearchReq{Keyword: "  AB  "})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil response")
	}
	if res.TotalCount != 0 {
		t.Fatalf("expected total_count 0, got %d", res.TotalCount)
	}
	if len(res.KeywordCountMap) != 0 {
		t.Fatalf("expected empty keyword_count_map, got %d entries", len(res.KeywordCountMap))
	}
	if res.Err != "" {
		t.Fatalf("expected empty err field, got %q", res.Err)
	}
}
