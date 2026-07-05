package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"indicer/internal/core/jsonbridge"
	platformapi "indicer/pkg/api"
)

const (
	cmdDispatch       = "dispatch"
	cmdCapabilities   = "capabilities"
	cmdCreateArchive  = "create-archive"
	cmdExtractArchive = "extract-archive"
	cmdListArchive    = "list-archive"
	cmdVerifyArchive  = "verify-archive"
	cmdCompress       = "compress"
	cmdDecompress     = "decompress"
	cmdCompressFile   = "compress-file"
	cmdDecompressFile = "decompress-file"

	opCapabilities    = "capabilities"
	opCreateArchive   = "create_archive"
	opExtractArchive  = "extract_archive"
	opListArchive     = "list_archive"
	opVerifyArchive   = "verify_archive"
	opCompressBytes   = "compress_bytes"
	opDecompressBytes = "decompress_bytes"
	opCompressFile    = "compress_file"
	opDecompressFile  = "decompress_file"

	productCompression = "compression_product"

	defaultLevel       = "best"
	defaultChunkSizeKB = 256
	defaultSplitSizeMB = 0

	usageText = "usage: compression <dispatch|capabilities|create-archive|extract-archive|list-archive|verify-archive|compress|decompress|compress-file|decompress-file> [flags]"

	errUnknownCommandFmt   = "unknown command: %s\n"
	errRequestRequired     = "--request is required"
	errInputRequired       = "--input is required"
	errCreateInputRequired = "--input or at least one --input-file is required"
	errDataBase64Required  = "--data-base64 is required"

	jsonVersionKey     = "version"
	jsonProductKey     = "product"
	jsonOperationKey   = "operation"
	jsonParamsKey      = "params"
	jsonDataBase64Key  = "data_base64"
	jsonLevelKey       = "level"
	jsonInputPathKey   = "input_path"
	jsonInputPathsKey  = "input_paths"
	jsonOutputPathKey  = "output_path"
	jsonWorkDirKey     = "work_dir"
	jsonChunkSizeKBKey = "chunk_size_kb"
	jsonPasswordKey    = "password"
	jsonSplitSizeMBKey = "split_size_mb"
	jsonKeepWorkDirKey = "keep_work_dir"
	jsonAsyncKey       = "async"
)

type command struct {
	name string
	run  func(args []string) int
}

func main() {
	commands := map[string]command{
		cmdDispatch:       {name: cmdDispatch, run: runDispatch},
		cmdCapabilities:   {name: cmdCapabilities, run: runCapabilities},
		cmdCreateArchive:  {name: cmdCreateArchive, run: runCreateArchive},
		cmdExtractArchive: {name: cmdExtractArchive, run: runExtractArchive},
		cmdListArchive:    {name: cmdListArchive, run: runListArchive},
		cmdVerifyArchive:  {name: cmdVerifyArchive, run: runVerifyArchive},
		cmdCompress:       {name: cmdCompress, run: runCompress},
		cmdDecompress:     {name: cmdDecompress, run: runDecompress},
		cmdCompressFile:   {name: cmdCompressFile, run: runCompressFile},
		cmdDecompressFile: {name: cmdDecompressFile, run: runDecompressFile},
	}

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, errUnknownCommandFmt, os.Args[1])
		os.Exit(2)
	}
	os.Exit(cmd.run(os.Args[2:]))
}

func runDispatch(args []string) int {
	fs := flag.NewFlagSet(cmdDispatch, flag.ContinueOnError)
	request := fs.String("request", "", "full JSON request envelope")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *request == "" {
		fmt.Fprintln(os.Stderr, errRequestRequired)
		return 2
	}
	out := platformapi.DefaultDispatcher().DispatchJSON(context.Background(), *request)
	fmt.Println(out)
	return 0
}

func runCapabilities(_ []string) int {
	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opCapabilities,
		jsonParamsKey:    map[string]any{},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runCreateArchive(args []string) int {
	fs := flag.NewFlagSet(cmdCreateArchive, flag.ContinueOnError)
	input := fs.String("input", "", "input file or folder path")
	var inputFiles stringSliceFlag
	fs.Var(&inputFiles, "input-file", "add input file (repeat flag for multiple files)")
	output := fs.String("output", "", "output .duesarc path (optional)")
	workDir := fs.String("work-dir", "", "working directory for generated pipeline data")
	chunkSize := fs.Int("chunk-size-kb", defaultChunkSizeKB, "chunk size in KB")
	password := fs.String("password", "", "optional archive password for db key derivation")
	splitSizeMB := fs.Int("split-size-mb", defaultSplitSizeMB, "optional split volume size in MB")
	keepWorkDir := fs.Bool("keep-work-dir", false, "keep generated working directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *input == "" && len(inputFiles) == 0 {
		fmt.Fprintln(os.Stderr, errCreateInputRequired)
		return 2
	}

	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opCreateArchive,
		jsonParamsKey: map[string]any{
			jsonInputPathKey:   *input,
			jsonInputPathsKey:  []string(inputFiles),
			jsonOutputPathKey:  *output,
			jsonWorkDirKey:     *workDir,
			jsonChunkSizeKBKey: *chunkSize,
			jsonPasswordKey:    *password,
			jsonSplitSizeMBKey: *splitSizeMB,
			jsonKeepWorkDirKey: *keepWorkDir,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runExtractArchive(args []string) int {
	fs := flag.NewFlagSet(cmdExtractArchive, flag.ContinueOnError)
	input := fs.String("input", "", "input .duesarc or .duesarc.001 path")
	output := fs.String("output", "", "output directory path (optional)")
	password := fs.String("password", "", "optional archive password for db key derivation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, errInputRequired)
		return 2
	}

	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opExtractArchive,
		jsonParamsKey: map[string]any{
			jsonInputPathKey:  *input,
			jsonOutputPathKey: *output,
			jsonPasswordKey:   *password,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runListArchive(args []string) int {
	fs := flag.NewFlagSet(cmdListArchive, flag.ContinueOnError)
	input := fs.String("input", "", "input .duesarc or .duesarc.001 path")
	password := fs.String("password", "", "optional archive password for db key derivation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, errInputRequired)
		return 2
	}

	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opListArchive,
		jsonParamsKey: map[string]any{
			jsonInputPathKey: *input,
			jsonPasswordKey:  *password,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runVerifyArchive(args []string) int {
	fs := flag.NewFlagSet(cmdVerifyArchive, flag.ContinueOnError)
	input := fs.String("input", "", "input .duesarc or .duesarc.001 path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, errInputRequired)
		return 2
	}

	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opVerifyArchive,
		jsonParamsKey: map[string]any{
			jsonInputPathKey: *input,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runCompress(args []string) int {
	fs := flag.NewFlagSet(cmdCompress, flag.ContinueOnError)
	text := fs.String("text", "", "input text")
	level := fs.String("level", defaultLevel, "compression level: fast|default|best")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opCompressBytes,
		jsonParamsKey: map[string]any{
			jsonDataBase64Key: base64.StdEncoding.EncodeToString([]byte(*text)),
			jsonLevelKey:      *level,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runDecompress(args []string) int {
	fs := flag.NewFlagSet(cmdDecompress, flag.ContinueOnError)
	dataBase64 := fs.String("data-base64", "", "compressed data in base64")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dataBase64 == "" {
		fmt.Fprintln(os.Stderr, errDataBase64Required)
		return 2
	}
	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opDecompressBytes,
		jsonParamsKey: map[string]any{
			jsonDataBase64Key: *dataBase64,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runCompressFile(args []string) int {
	fs := flag.NewFlagSet(cmdCompressFile, flag.ContinueOnError)
	input := fs.String("input", "", "input file path")
	output := fs.String("output", "", "output archive path (optional, .duesarc)")
	level := fs.String("level", defaultLevel, "compression level hint (currently ignored for store pipeline)")
	async := fs.Bool("async", false, "run operation asynchronously and return task_id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, errInputRequired)
		return 2
	}
	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opCompressFile,
		jsonParamsKey: map[string]any{
			jsonInputPathKey:  *input,
			jsonOutputPathKey: *output,
			jsonLevelKey:      *level,
			jsonAsyncKey:      *async,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func runDecompressFile(args []string) int {
	fs := flag.NewFlagSet(cmdDecompressFile, flag.ContinueOnError)
	input := fs.String("input", "", "input compressed file path")
	output := fs.String("output", "", "output file path (optional)")
	async := fs.Bool("async", false, "run operation asynchronously and return task_id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *input == "" {
		fmt.Fprintln(os.Stderr, errInputRequired)
		return 2
	}
	request := map[string]any{
		jsonVersionKey:   jsonbridge.CurrentVersion,
		jsonProductKey:   productCompression,
		jsonOperationKey: opDecompressFile,
		jsonParamsKey: map[string]any{
			jsonInputPathKey:  *input,
			jsonOutputPathKey: *output,
			jsonAsyncKey:      *async,
		},
	}
	out, err := dispatchRequest(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	fmt.Println(out)
	return 0
}

func dispatchRequest(req map[string]any) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	return platformapi.DefaultDispatcher().DispatchJSON(context.Background(), string(body)), nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return fmt.Sprintf("%v", []string(*s))
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
