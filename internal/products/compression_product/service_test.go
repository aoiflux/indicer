package compression_product

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testOpCompressBytes   = "compress_bytes"
	testOpDecompressBytes = "decompress_bytes"
	testOpCompressFile    = "compress_file"
	testOpDecompressFile  = "decompress_file"
	testOpCapabilities    = "capabilities"
	testOpCreateArchive   = "create_archive"
	testOpExtractArchive  = "extract_archive"
	testOpListArchive     = "list_archive"
	testOpVerifyArchive   = "verify_archive"
	testOpGetProgress     = "get_progress"
	testOpCancelTask      = "cancel_task"

	testKeyDataBase64  = "data_base64"
	testKeyInputPath   = "input_path"
	testKeyOutputPath  = "output_path"
	testKeyLevel       = "level"
	testKeySplitSizeMB = "split_size_mb"
	testKeyOutputFiles = "output_files"
	testKeyProduct     = "product"
	testKeyEntryCount  = "entry_count"
	testKeyValid       = "valid"

	testLevelBest    = "best"
	testLevelDefault = "default"

	testManifestName = "manifest.json"
	testArchiveExt   = ".duesarc"
	testExtractDir   = "extracted"
	testFilePerm     = 0644
)

func TestHandle_CompressDecompressBytes_RoundTrip(t *testing.T) {
	svc := New()
	input := []byte("compression product round trip")

	compressParams, _ := json.Marshal(map[string]any{
		testKeyDataBase64: base64.StdEncoding.EncodeToString(input),
		testKeyLevel:      testLevelBest,
	})
	cres, cerr := svc.Handle(context.Background(), testOpCompressBytes, compressParams)
	if cerr != nil {
		t.Fatalf("compress_bytes returned error: %v", cerr)
	}
	compressMap, ok := structToMap(cres)
	if !ok {
		t.Fatalf("compress_bytes result type assertion failed")
	}
	compressedB64, _ := compressMap["data_base64"].(string)
	if compressedB64 == "" {
		t.Fatalf("compress_bytes returned empty payload")
	}

	decompressParams, _ := json.Marshal(map[string]any{
		testKeyDataBase64: compressedB64,
	})
	dres, derr := svc.Handle(context.Background(), testOpDecompressBytes, decompressParams)
	if derr != nil {
		t.Fatalf("decompress_bytes returned error: %v", derr)
	}
	decompressMap, ok := structToMap(dres)
	if !ok {
		t.Fatalf("decompress_bytes result type assertion failed")
	}
	rawB64, _ := decompressMap["data_base64"].(string)
	raw, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		t.Fatalf("failed to decode decompressed base64: %v", err)
	}
	if string(raw) != string(input) {
		t.Fatalf("round trip mismatch: got %q want %q", string(raw), string(input))
	}
}

func TestHandle_CompressDecompressFile_RoundTrip(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "input.txt")
	outPath := filepath.Join(dir, "input.txt"+testArchiveExt)
	want := "file round trip for compression product"

	if err := os.WriteFile(inPath, []byte(want), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	cparams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  inPath,
		testKeyOutputPath: outPath,
		testKeyLevel:      testLevelDefault,
	})
	res, cerr := svc.Handle(context.Background(), testOpCompressFile, cparams)
	if cerr != nil {
		t.Fatalf("compress_file returned error: %v", cerr)
	}

	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("compress_file result parse failed")
	}
	gotOutputPath, _ := m[testKeyOutputPath].(string)
	if gotOutputPath == "" {
		t.Fatalf("missing output_path")
	}
	if _, err := os.Stat(gotOutputPath); err != nil {
		t.Fatalf("archive not found: %v", err)
	}

	reader, err := zip.OpenReader(gotOutputPath)
	if err != nil {
		t.Fatalf("archive open failed: %v", err)
	}
	defer reader.Close()

	foundManifest := false
	for _, f := range reader.File {
		if f.Name == testManifestName {
			foundManifest = true
			break
		}
	}
	if !foundManifest {
		t.Fatalf("manifest.json missing in compress_file archive")
	}
}

func TestHandle_DecompressFile_LegacyZstdRoundTrip(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "legacy.txt")
	compressedPath := filepath.Join(dir, "legacy.txt.dues.zst")
	restorePath := filepath.Join(dir, "legacy-restored.txt")
	want := "legacy zstd decompress compatibility"

	if err := os.WriteFile(inPath, []byte(want), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	inputRaw, err := os.ReadFile(inPath)
	if err != nil {
		t.Fatalf("read input failed: %v", err)
	}

	compressBytesParams, _ := json.Marshal(map[string]any{
		testKeyDataBase64: base64.StdEncoding.EncodeToString(inputRaw),
		testKeyLevel:      testLevelDefault,
	})
	compressBytesRes, compressBytesErr := svc.Handle(context.Background(), testOpCompressBytes, compressBytesParams)
	if compressBytesErr != nil {
		t.Fatalf("compress_bytes returned error: %v", compressBytesErr)
	}
	compressMap, ok := structToMap(compressBytesRes)
	if !ok {
		t.Fatalf("compress_bytes result parse failed")
	}
	compressedB64, _ := compressMap[testKeyDataBase64].(string)
	compressedRaw, err := base64.StdEncoding.DecodeString(compressedB64)
	if err != nil {
		t.Fatalf("decode compressed payload failed: %v", err)
	}
	if err := os.WriteFile(compressedPath, compressedRaw, testFilePerm); err != nil {
		t.Fatalf("write compressed payload failed: %v", err)
	}

	dparams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  compressedPath,
		testKeyOutputPath: restorePath,
	})
	_, derr := svc.Handle(context.Background(), testOpDecompressFile, dparams)
	if derr != nil {
		t.Fatalf("decompress_file returned error: %v", derr)
	}

	got, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatalf("read restored failed: %v", err)
	}
	if string(got) != want {
		t.Fatalf("restored mismatch: got %q want %q", string(got), want)
	}
}

func TestHandle_CompressFile_AsyncProgress(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "compress-async.txt")
	outPath := filepath.Join(dir, "compress-async"+testArchiveExt)

	if err := os.WriteFile(inPath, []byte(strings.Repeat("compress-file-async-", 50000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	cparams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  inPath,
		testKeyOutputPath: outPath,
		"async":           true,
	})
	startRes, startErr := svc.Handle(context.Background(), testOpCompressFile, cparams)
	if startErr != nil {
		t.Fatalf("compress_file async start failed: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("compress_file async start parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}

	deadline := time.Now().Add(12 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "failed" {
			t.Fatalf("compress_file async failed: %v", pm["error"])
		}
		if status == "completed" {
			resultAny, ok := pm["result"].(map[string]any)
			if !ok {
				t.Fatalf("missing completed result payload")
			}
			resultOut, _ := resultAny[testKeyOutputPath].(string)
			if resultOut == "" {
				t.Fatalf("missing output_path in result")
			}
			if _, err := os.Stat(resultOut); err != nil {
				t.Fatalf("expected archive output: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for compress_file async completion")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestHandle_CompressFile_AsyncCancel(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "compress-cancel.txt")
	outPath := filepath.Join(dir, "compress-cancel"+testArchiveExt)

	if err := os.WriteFile(inPath, []byte(strings.Repeat("compress-file-cancel-", 1_000_000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	cparams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  inPath,
		testKeyOutputPath: outPath,
		"async":           true,
	})
	startRes, startErr := svc.Handle(context.Background(), testOpCompressFile, cparams)
	if startErr != nil {
		t.Fatalf("compress_file async start failed: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("compress_file async start parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}

	cancelParams, _ := json.Marshal(map[string]any{"task_id": taskID})
	_, cancelErr := svc.Handle(context.Background(), testOpCancelTask, cancelParams)
	if cancelErr != nil {
		t.Fatalf("cancel compress_file task failed: %v", cancelErr)
	}

	deadline := time.Now().Add(12 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "canceled" || status == "completed" || status == "failed" {
			if status == "failed" {
				t.Fatalf("unexpected failed status after compress_file cancel: %v", pm["error"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for compress_file cancel terminal state")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHandle_DecompressFile_AsyncProgress(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "legacy-async.txt")
	compressedPath := filepath.Join(dir, "legacy-async.txt.dues.zst")
	restorePath := filepath.Join(dir, "legacy-async-restored.txt")

	if err := os.WriteFile(inPath, []byte(strings.Repeat("legacy-async-decompress-", 10000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	inputRaw, err := os.ReadFile(inPath)
	if err != nil {
		t.Fatalf("read input failed: %v", err)
	}

	compressBytesParams, _ := json.Marshal(map[string]any{
		testKeyDataBase64: base64.StdEncoding.EncodeToString(inputRaw),
		testKeyLevel:      testLevelDefault,
	})
	compressBytesRes, compressBytesErr := svc.Handle(context.Background(), testOpCompressBytes, compressBytesParams)
	if compressBytesErr != nil {
		t.Fatalf("compress_bytes returned error: %v", compressBytesErr)
	}
	compressMap, ok := structToMap(compressBytesRes)
	if !ok {
		t.Fatalf("compress_bytes result parse failed")
	}
	compressedB64, _ := compressMap[testKeyDataBase64].(string)
	compressedRaw, err := base64.StdEncoding.DecodeString(compressedB64)
	if err != nil {
		t.Fatalf("decode compressed payload failed: %v", err)
	}
	if err := os.WriteFile(compressedPath, compressedRaw, testFilePerm); err != nil {
		t.Fatalf("write compressed payload failed: %v", err)
	}

	dparams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  compressedPath,
		testKeyOutputPath: restorePath,
		"async":           true,
	})
	startRes, startErr := svc.Handle(context.Background(), testOpDecompressFile, dparams)
	if startErr != nil {
		t.Fatalf("decompress_file async start failed: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("decompress_file async start parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}

	deadline := time.Now().Add(12 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "failed" {
			t.Fatalf("decompress_file async failed: %v", pm["error"])
		}
		if status == "completed" {
			if _, err := os.Stat(restorePath); err != nil {
				t.Fatalf("expected decompressed output: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for decompress_file async completion")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestHandle_DecompressFile_AsyncCancel(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "legacy-cancel.txt")
	compressedPath := filepath.Join(dir, "legacy-cancel.txt.dues.zst")
	restorePath := filepath.Join(dir, "legacy-cancel-restored.txt")

	if err := os.WriteFile(inPath, []byte(strings.Repeat("legacy-cancel-decompress-", 500000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	inputRaw, err := os.ReadFile(inPath)
	if err != nil {
		t.Fatalf("read input failed: %v", err)
	}

	compressBytesParams, _ := json.Marshal(map[string]any{
		testKeyDataBase64: base64.StdEncoding.EncodeToString(inputRaw),
		testKeyLevel:      testLevelDefault,
	})
	compressBytesRes, compressBytesErr := svc.Handle(context.Background(), testOpCompressBytes, compressBytesParams)
	if compressBytesErr != nil {
		t.Fatalf("compress_bytes returned error: %v", compressBytesErr)
	}
	compressMap, ok := structToMap(compressBytesRes)
	if !ok {
		t.Fatalf("compress_bytes result parse failed")
	}
	compressedB64, _ := compressMap[testKeyDataBase64].(string)
	compressedRaw, err := base64.StdEncoding.DecodeString(compressedB64)
	if err != nil {
		t.Fatalf("decode compressed payload failed: %v", err)
	}
	if err := os.WriteFile(compressedPath, compressedRaw, testFilePerm); err != nil {
		t.Fatalf("write compressed payload failed: %v", err)
	}

	dparams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  compressedPath,
		testKeyOutputPath: restorePath,
		"async":           true,
	})
	startRes, startErr := svc.Handle(context.Background(), testOpDecompressFile, dparams)
	if startErr != nil {
		t.Fatalf("decompress_file async start failed: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("decompress_file async start parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}

	cancelParams, _ := json.Marshal(map[string]any{"task_id": taskID})
	_, cancelErr := svc.Handle(context.Background(), testOpCancelTask, cancelParams)
	if cancelErr != nil {
		t.Fatalf("cancel decompress_file task failed: %v", cancelErr)
	}

	deadline := time.Now().Add(12 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "canceled" || status == "completed" || status == "failed" {
			if status == "failed" {
				t.Fatalf("unexpected failed status after decompress_file cancel: %v", pm["error"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for decompress_file cancel terminal state")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHandle_Capabilities(t *testing.T) {
	svc := New()
	res, rerr := svc.Handle(context.Background(), testOpCapabilities, json.RawMessage(`{}`))
	if rerr != nil {
		t.Fatalf("capabilities returned error: %v", rerr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("capabilities result parse failed")
	}
	product, _ := m[testKeyProduct].(string)
	if product != Name {
		t.Fatalf("unexpected product: got %q want %q", product, Name)
	}
}

func TestHandle_InvalidBase64(t *testing.T) {
	svc := New()
	params := json.RawMessage(`{"data_base64":"@@@"}`)
	_, rerr := svc.Handle(context.Background(), testOpCompressBytes, params)
	if rerr == nil {
		t.Fatalf("expected error for invalid base64")
	}
	if !strings.Contains(rerr.Message, "base64") {
		t.Fatalf("unexpected error message: %q", rerr.Message)
	}
}

func TestHandle_CreateArchive_File(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "sample.txt")
	out := filepath.Join(dir, "sample"+testArchiveExt)

	if err := os.WriteFile(in, []byte("archive pipeline test payload"), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	params, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: out,
	})
	res, rerr := svc.Handle(context.Background(), testOpCreateArchive, params)
	if rerr != nil {
		t.Fatalf("create_archive returned error: %v", rerr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("result parse failed")
	}
	outputPath, _ := m[testKeyOutputPath].(string)
	if outputPath == "" {
		t.Fatalf("missing output_path")
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("archive not found: %v", err)
	}

	reader, err := zip.OpenReader(outputPath)
	if err != nil {
		t.Fatalf("archive open failed: %v", err)
	}
	defer reader.Close()

	foundManifest := false
	for _, f := range reader.File {
		if f.Name == testManifestName {
			foundManifest = true
			break
		}
	}
	if !foundManifest {
		t.Fatalf("manifest.json missing in archive")
	}
}

func TestHandle_CreateArchive_SplitVolumes(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "big.txt")
	out := filepath.Join(dir, "big"+testArchiveExt)

	// Keep payload slightly above 1MB to force at least two parts when split_size_mb=1.
	payload := strings.Repeat("DUES-ARCHIVE-TEST-", 80000)
	if err := os.WriteFile(in, []byte(payload), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	params, _ := json.Marshal(map[string]any{
		testKeyInputPath:   in,
		testKeyOutputPath:  out,
		testKeySplitSizeMB: 1,
	})
	res, rerr := svc.Handle(context.Background(), testOpCreateArchive, params)
	if rerr != nil {
		t.Fatalf("create_archive split returned error: %v", rerr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("result parse failed")
	}
	filesAny, ok := m[testKeyOutputFiles].([]any)
	if !ok {
		t.Fatalf("output_files type assertion failed")
	}
	if len(filesAny) < 1 {
		t.Fatalf("expected split output files, got %d", len(filesAny))
	}
	for _, f := range filesAny {
		path, _ := f.(string)
		if path == "" {
			t.Fatalf("empty split file path")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("split file missing: %v", err)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatalf("base archive should be removed after split")
	}
}

func TestHandle_ExtractArchive_File(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "extract-src.txt")
	arc := filepath.Join(dir, "extract-src"+testArchiveExt)
	outDir := filepath.Join(dir, testExtractDir)
	content := "extract archive smoke"

	if err := os.WriteFile(in, []byte(content), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: arc,
	})
	_, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create archive failed: %v", createErr)
	}

	extractParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  arc,
		testKeyOutputPath: outDir,
	})
	res, extractErr := svc.Handle(context.Background(), testOpExtractArchive, extractParams)
	if extractErr != nil {
		t.Fatalf("extract archive failed: %v", extractErr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("extract result parse failed")
	}
	extractPath, _ := m[testKeyOutputPath].(string)
	if extractPath == "" {
		t.Fatalf("missing extract output_path")
	}
	restoredPath := filepath.Join(extractPath, filepath.Base(in))
	restoredData, err := os.ReadFile(restoredPath)
	if err != nil {
		t.Fatalf("restored file missing in extracted output: %v", err)
	}
	if string(restoredData) != content {
		t.Fatalf("restored file content mismatch: got %q want %q", string(restoredData), content)
	}
}

func TestHandle_CreateArchive_MultiFile(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inputDir := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatalf("create input dir failed: %v", err)
	}
	fileA := filepath.Join(inputDir, "a.txt")
	fileB := filepath.Join(inputDir, "b.txt")
	if err := os.WriteFile(fileA, []byte("alpha"), testFilePerm); err != nil {
		t.Fatalf("write fileA failed: %v", err)
	}
	if err := os.WriteFile(fileB, []byte("beta"), testFilePerm); err != nil {
		t.Fatalf("write fileB failed: %v", err)
	}

	arc := filepath.Join(dir, "bundle"+testArchiveExt)
	createParams, _ := json.Marshal(map[string]any{
		testKeyOutputPath: arc,
		"input_paths":     []string{fileA, fileB},
	})
	res, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create archive multi-file failed: %v", createErr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("create archive result parse failed")
	}
	inputPathsAny, ok := m["input_paths"].([]any)
	if !ok || len(inputPathsAny) != 2 {
		t.Fatalf("expected 2 input_paths in response, got %#v", m["input_paths"])
	}

	outDir := filepath.Join(dir, "restored")
	extractParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  arc,
		testKeyOutputPath: outDir,
	})
	_, extractErr := svc.Handle(context.Background(), testOpExtractArchive, extractParams)
	if extractErr != nil {
		t.Fatalf("extract archive multi-file failed: %v", extractErr)
	}

	dataA, err := os.ReadFile(filepath.Join(outDir, "a.txt"))
	if err != nil {
		t.Fatalf("restored a.txt missing: %v", err)
	}
	if string(dataA) != "alpha" {
		t.Fatalf("restored a.txt mismatch: got %q", string(dataA))
	}
	dataB, err := os.ReadFile(filepath.Join(outDir, "b.txt"))
	if err != nil {
		t.Fatalf("restored b.txt missing: %v", err)
	}
	if string(dataB) != "beta" {
		t.Fatalf("restored b.txt mismatch: got %q", string(dataB))
	}
}

func TestHandle_CreateArchive_MultiFileRejectsDirectory(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	inputDir := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatalf("create input dir failed: %v", err)
	}
	fileA := filepath.Join(inputDir, "a.txt")
	if err := os.WriteFile(fileA, []byte("alpha"), testFilePerm); err != nil {
		t.Fatalf("write fileA failed: %v", err)
	}

	params, _ := json.Marshal(map[string]any{
		"input_paths": []string{fileA, inputDir},
	})
	_, rerr := svc.Handle(context.Background(), testOpCreateArchive, params)
	if rerr == nil {
		t.Fatalf("expected error when input_paths contains directory")
	}
	if !strings.Contains(rerr.Message, "input_paths only accepts files") {
		t.Fatalf("unexpected error message: %q", rerr.Message)
	}
}

func TestHandle_CreateArchive_AsyncProgress(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "async-src.txt")
	out := filepath.Join(dir, "async-src"+testArchiveExt)

	if err := os.WriteFile(in, []byte(strings.Repeat("async-progress-", 1024)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: out,
		"async":           true,
	})
	startRes, startErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if startErr != nil {
		t.Fatalf("create archive async failed to start: %v", startErr)
	}
	m, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("async start result parse failed")
	}
	taskID, _ := m["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id in async start response")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress result parse failed")
		}

		status, _ := pm["status"].(string)
		if status == "failed" {
			t.Fatalf("async task failed: %v", pm["error"])
		}
		if status == "completed" {
			percent, _ := pm["percent"].(float64)
			if percent != 100 {
				t.Fatalf("expected completed percent=100, got %v", percent)
			}
			resultAny, ok := pm["result"].(map[string]any)
			if !ok {
				t.Fatalf("expected completed result payload")
			}
			resultOut, _ := resultAny[testKeyOutputPath].(string)
			if resultOut == "" {
				t.Fatalf("expected output_path in completed result")
			}
			if _, err := os.Stat(resultOut); err != nil {
				t.Fatalf("expected archive on disk: %v", err)
			}
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for async archive completion")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestHandle_CancelTask_NotFound(t *testing.T) {
	svc := New()
	params, _ := json.Marshal(map[string]any{"task_id": "missing-task"})
	_, rerr := svc.Handle(context.Background(), testOpCancelTask, params)
	if rerr == nil {
		t.Fatalf("expected cancel_task error for unknown task")
	}
	if !strings.Contains(rerr.Message, "progress task not found") {
		t.Fatalf("unexpected error: %q", rerr.Message)
	}
}

func TestHandle_CreateArchive_AsyncCancel(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "cancel-src.txt")
	out := filepath.Join(dir, "cancel-src"+testArchiveExt)

	if err := os.WriteFile(in, []byte(strings.Repeat("cancel-me-", 1_000_000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: out,
		"async":           true,
	})
	startRes, startErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if startErr != nil {
		t.Fatalf("create archive async failed to start: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("start result parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("expected task_id")
	}

	cancelParams, _ := json.Marshal(map[string]any{"task_id": taskID})
	cancelRes, cancelErr := svc.Handle(context.Background(), testOpCancelTask, cancelParams)
	if cancelErr != nil {
		t.Fatalf("cancel_task failed: %v", cancelErr)
	}
	cancelMap, ok := structToMap(cancelRes)
	if !ok {
		t.Fatalf("cancel result parse failed")
	}
	if gotTask, _ := cancelMap["task_id"].(string); gotTask != taskID {
		t.Fatalf("unexpected cancel task id: %q", gotTask)
	}

	deadline := time.Now().Add(12 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "canceled" || status == "completed" || status == "failed" {
			if status == "failed" {
				t.Fatalf("unexpected failed status after cancellation: %v", pm["error"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cancel terminal state")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHandle_ListArchive_File(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "list-src.txt")
	arc := filepath.Join(dir, "list-src"+testArchiveExt)

	if err := os.WriteFile(in, []byte("list archive smoke"), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: arc,
	})
	_, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create archive failed: %v", createErr)
	}

	listParams, _ := json.Marshal(map[string]any{testKeyInputPath: arc})
	res, listErr := svc.Handle(context.Background(), testOpListArchive, listParams)
	if listErr != nil {
		t.Fatalf("list archive failed: %v", listErr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("list result parse failed")
	}
	entryCount, _ := m[testKeyEntryCount].(float64)
	if entryCount < 1 {
		t.Fatalf("expected archive entries, got %v", entryCount)
	}
	logicalFileCount, _ := m["logical_file_count"].(float64)
	if logicalFileCount < 1 {
		t.Fatalf("expected logical archive files, got %v", logicalFileCount)
	}
	logicalFiles, ok := m["logical_files"].([]any)
	if !ok || len(logicalFiles) == 0 {
		t.Fatalf("expected logical_files in list result")
	}
	first, ok := logicalFiles[0].(map[string]any)
	if !ok {
		t.Fatalf("logical file object parse failed")
	}
	totalSize, _ := first["total_size"].(float64)
	compressedSize, _ := first["compressed_size"].(float64)
	if totalSize <= 0 {
		t.Fatalf("expected total_size > 0, got %v", totalSize)
	}
	if compressedSize <= 0 {
		t.Fatalf("expected compressed_size > 0, got %v", compressedSize)
	}
}

func TestHandle_ListArchive_AsyncProgress(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "list-async-src.txt")
	arc := filepath.Join(dir, "list-async-src"+testArchiveExt)

	if err := os.WriteFile(in, []byte(strings.Repeat("list-async-", 10000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: arc,
	})
	_, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create archive failed: %v", createErr)
	}

	listParams, _ := json.Marshal(map[string]any{testKeyInputPath: arc, "async": true})
	startRes, startErr := svc.Handle(context.Background(), testOpListArchive, listParams)
	if startErr != nil {
		t.Fatalf("list archive async start failed: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("list async start parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "failed" {
			t.Fatalf("list async failed: %v", pm["error"])
		}
		if status == "completed" {
			resultAny, ok := pm["result"].(map[string]any)
			if !ok {
				t.Fatalf("missing completed result payload")
			}
			entryCount, _ := resultAny[testKeyEntryCount].(float64)
			if entryCount < 1 {
				t.Fatalf("expected entries in list result")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for async list completion")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestHandle_ListArchive_AsyncCancel(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "list-cancel-src.txt")
	arc := filepath.Join(dir, "list-cancel-src"+testArchiveExt)

	if err := os.WriteFile(in, []byte(strings.Repeat("list-cancel-", 1_000_000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: arc,
	})
	_, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create archive failed: %v", createErr)
	}

	listParams, _ := json.Marshal(map[string]any{testKeyInputPath: arc, "async": true})
	startRes, startErr := svc.Handle(context.Background(), testOpListArchive, listParams)
	if startErr != nil {
		t.Fatalf("list archive async start failed: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("list async start parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}

	cancelParams, _ := json.Marshal(map[string]any{"task_id": taskID})
	_, cancelErr := svc.Handle(context.Background(), testOpCancelTask, cancelParams)
	if cancelErr != nil {
		t.Fatalf("cancel list task failed: %v", cancelErr)
	}

	deadline := time.Now().Add(12 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "canceled" || status == "completed" || status == "failed" {
			if status == "failed" {
				t.Fatalf("unexpected failed status after list cancellation: %v", pm["error"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for list cancel terminal state")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHandle_VerifyArchive_File(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "verify-src.txt")
	arc := filepath.Join(dir, "verify-src"+testArchiveExt)

	if err := os.WriteFile(in, []byte("verify archive smoke"), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: arc,
	})
	_, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create archive failed: %v", createErr)
	}

	verifyParams, _ := json.Marshal(map[string]any{testKeyInputPath: arc})
	res, verifyErr := svc.Handle(context.Background(), testOpVerifyArchive, verifyParams)
	if verifyErr != nil {
		t.Fatalf("verify archive failed: %v", verifyErr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("verify result parse failed")
	}
	valid, _ := m[testKeyValid].(bool)
	if !valid {
		t.Fatalf("expected valid archive")
	}
	format, _ := m["format"].(string)
	if format != "duesarc" {
		t.Fatalf("expected duesarc format, got %q", format)
	}
	manifestFound, _ := m["manifest_found"].(bool)
	if !manifestFound {
		t.Fatalf("expected manifest_found=true")
	}
	fileCount, _ := m["file_count"].(float64)
	if fileCount != 1 {
		t.Fatalf("expected duesarc logical file_count=1, got %v", fileCount)
	}
	inputBytes, _ := m["input_bytes"].(float64)
	if inputBytes <= 0 {
		t.Fatalf("expected input_bytes > 0, got %v", inputBytes)
	}
}

func TestHandle_VerifyArchive_ZipProvidesFormatMetadata(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "verify-zip-src.txt")
	arc := filepath.Join(dir, "verify-zip-src.zip")

	if err := os.WriteFile(in, []byte("verify zip metadata"), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: arc,
		"format":          "zip",
	})
	_, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create zip archive failed: %v", createErr)
	}

	verifyParams, _ := json.Marshal(map[string]any{testKeyInputPath: arc})
	res, verifyErr := svc.Handle(context.Background(), testOpVerifyArchive, verifyParams)
	if verifyErr != nil {
		t.Fatalf("verify zip archive failed: %v", verifyErr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("verify zip result parse failed")
	}
	format, _ := m["format"].(string)
	if format != "zip" {
		t.Fatalf("expected zip format, got %q", format)
	}
	manifestFound, _ := m["manifest_found"].(bool)
	if manifestFound {
		t.Fatalf("expected manifest_found=false for zip compatibility archive")
	}
	entryCount, _ := m[testKeyEntryCount].(float64)
	fileCount, _ := m["file_count"].(float64)
	if entryCount < 1 || fileCount < 1 {
		t.Fatalf("expected entry_count/file_count >= 1, got entry_count=%v file_count=%v", entryCount, fileCount)
	}
}

func TestHandle_VerifyArchive_CorruptFile(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	arc := filepath.Join(dir, "corrupt"+testArchiveExt)

	if err := os.WriteFile(arc, []byte("not a zip archive"), testFilePerm); err != nil {
		t.Fatalf("write corrupt archive failed: %v", err)
	}

	verifyParams, _ := json.Marshal(map[string]any{testKeyInputPath: arc})
	_, verifyErr := svc.Handle(context.Background(), testOpVerifyArchive, verifyParams)
	if verifyErr == nil {
		t.Fatalf("expected verify error for corrupt archive")
	}
}

func TestHandle_VerifyArchive_MissingSplitPart(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "split-src.txt")
	arc := filepath.Join(dir, "split-src"+testArchiveExt)

	payload := strings.Repeat("MISSING-SPLIT-PART-TEST-", 70000)
	if err := os.WriteFile(in, []byte(payload), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:   in,
		testKeyOutputPath:  arc,
		testKeySplitSizeMB: 1,
	})
	res, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create split archive failed: %v", createErr)
	}
	m, ok := structToMap(res)
	if !ok {
		t.Fatalf("create split result parse failed")
	}
	filesAny, ok := m[testKeyOutputFiles].([]any)
	if !ok || len(filesAny) < 1 {
		t.Fatalf("expected split output files")
	}
	partPath, _ := filesAny[0].(string)
	if partPath == "" {
		t.Fatalf("expected split part path")
	}

	if err := os.Remove(partPath); err != nil {
		t.Fatalf("remove split part failed: %v", err)
	}

	verifyParams, _ := json.Marshal(map[string]any{testKeyInputPath: partPath})
	_, verifyErr := svc.Handle(context.Background(), testOpVerifyArchive, verifyParams)
	if verifyErr == nil {
		t.Fatalf("expected verify error for missing split part")
	}
}

func TestHandle_VerifyArchive_AsyncCancel(t *testing.T) {
	svc := New()
	dir := t.TempDir()
	in := filepath.Join(dir, "verify-cancel-src.txt")
	arc := filepath.Join(dir, "verify-cancel-src"+testArchiveExt)

	if err := os.WriteFile(in, []byte(strings.Repeat("verify-cancel-", 1_000_000)), testFilePerm); err != nil {
		t.Fatalf("write input failed: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		testKeyInputPath:  in,
		testKeyOutputPath: arc,
	})
	_, createErr := svc.Handle(context.Background(), testOpCreateArchive, createParams)
	if createErr != nil {
		t.Fatalf("create archive failed: %v", createErr)
	}

	verifyParams, _ := json.Marshal(map[string]any{testKeyInputPath: arc, "async": true})
	startRes, startErr := svc.Handle(context.Background(), testOpVerifyArchive, verifyParams)
	if startErr != nil {
		t.Fatalf("verify archive async start failed: %v", startErr)
	}
	startMap, ok := structToMap(startRes)
	if !ok {
		t.Fatalf("verify async start parse failed")
	}
	taskID, _ := startMap["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id")
	}

	cancelParams, _ := json.Marshal(map[string]any{"task_id": taskID})
	_, cancelErr := svc.Handle(context.Background(), testOpCancelTask, cancelParams)
	if cancelErr != nil {
		t.Fatalf("cancel verify task failed: %v", cancelErr)
	}

	deadline := time.Now().Add(12 * time.Second)
	for {
		progressParams, _ := json.Marshal(map[string]any{"task_id": taskID})
		progressRes, progressErr := svc.Handle(context.Background(), testOpGetProgress, progressParams)
		if progressErr != nil {
			t.Fatalf("get_progress failed: %v", progressErr)
		}
		pm, ok := structToMap(progressRes)
		if !ok {
			t.Fatalf("progress parse failed")
		}
		status, _ := pm["status"].(string)
		if status == "canceled" || status == "completed" || status == "failed" {
			if status == "failed" {
				t.Fatalf("unexpected failed status after verify cancellation: %v", pm["error"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for verify cancel terminal state")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func structToMap(v any) (map[string]any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	return out, true
}
