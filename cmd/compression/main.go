// Command compression is the qwikpak command line: each command sends one
// request to the same dispatcher the FFI library uses, prints the response
// envelope on stdout, and exits with a code taken from it.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"indicer/internal/console"
	"indicer/internal/core/jsonbridge"
	"indicer/internal/lifecycle"
	"indicer/internal/osdirs"
	"indicer/lib/logging"
	platformapi "indicer/pkg/api"

	"go.uber.org/zap"
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

	productCompression = "qwikpak"

	defaultLevel       = "best"
	defaultChunkSizeKB = 256
	defaultFormat      = "duesarc"

	usageText = `usage: compression [--log-dir DIR] [--temp-dir DIR] <command> [flags]

global options (before the command):
  --log-dir DIR     directory for log files (default: the per-user cache directory)
  --temp-dir DIR    directory for temporary work directories (default: /tmp, or
                    AppData\Local\Temp on Windows); it must exist

commands:
  create-archive    archive a file, a folder, or files (--input, --input-file)
  extract-archive   extract an archive into a folder
  list-archive      list an archive's entries and files
  verify-archive    read back every entry and stored chunk of an archive
  compress-file     archive one file as <file>.duesarc
  decompress-file   restore the file of a one-file .duesarc, or a zstd file
  compress          compress --text, printing base64
  decompress        decompress --data-base64
  capabilities      list operations and formats
  dispatch          send a full request envelope (--request or --request-file)

The response envelope is printed on stdout. Exit codes: 0 ok; 2 invalid
request; 1 failed; 70 internal error; 130/143 interrupted. Passwords are read
from --password-file or --password-stdin only.`
)

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) (code int) {
	// Stdout carries the JSON result; library output goes to stderr (FFI-3).
	console.Configure(os.Stderr, os.Stderr)
	// The log exists before run reports a bad global option, so the global
	// options are read twice; run reports what is wrong with them.
	g, _, _ := parseGlobal(args, io.Discard)
	logging.Init(logging.Config{Component: "qwikpak", Dir: g.logDir, TempDir: g.tempDir})
	defer logging.Close()
	logging.ReportPanics()
	defer func() {
		if v := recover(); v != nil {
			logging.GetLogger().Error("unrecovered panic")
			fmt.Fprintf(os.Stderr, "compression: internal error: %v\n%s", v, debug.Stack())
			code = lifecycle.ExitInternal
		}
	}()
	ctx, stop := lifecycle.Context(context.Background())
	defer stop()
	return run(ctx, args, os.Stdout, os.Stderr, os.Stdin)
}

// globalFlags are the options before the command.
type globalFlags struct {
	logDir, tempDir string
	faults          faultFlags
	set             map[string]bool // the options given
}

// parseGlobal reads the global options and returns the arguments from the
// command on. A --temp-dir that is not an existing directory is an error.
func parseGlobal(args []string, stderr io.Writer) (globalFlags, []string, error) {
	var g globalFlags
	fs := flag.NewFlagSet("compression", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&g.logDir, "log-dir", "", "directory for log files")
	fs.StringVar(&g.tempDir, "temp-dir", "", "directory for temporary work directories")
	g.faults.register(fs)
	if err := fs.Parse(args); err != nil {
		return g, nil, err
	}
	g.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { g.set[f.Name] = true })
	if g.tempDir != "" {
		info, err := os.Stat(g.tempDir)
		if err != nil {
			return g, nil, fmt.Errorf("--temp-dir: %w", err)
		}
		if !info.IsDir() {
			return g, nil, fmt.Errorf("--temp-dir %s is not a directory", g.tempDir)
		}
	}
	return g, fs.Args(), nil
}

// logSettings writes the run's "settings" record: the log and temporary
// directories in effect, each with whether an option or the default chose
// it.
func (g globalFlags) logSettings() {
	from := func(name string) string {
		if g.set[name] {
			return logging.FromFlag
		}
		return logging.FromDefault
	}
	temp, err := osdirs.TempOr(g.tempDir)
	tempSetting := logging.Setting("temp-dir", temp, from("temp-dir"))
	if err != nil {
		tempSetting = logging.Setting("temp-dir", "", from("temp-dir"), zap.Error(err))
	}
	logging.GetLogger().Info("settings", logging.DirSetting("log-dir", g.logDir, from("log-dir")), tempSetting)
}

type runner struct {
	ctx      context.Context
	dispatch *platformapi.Dispatcher
	stdout   io.Writer
	stderr   io.Writer
	stdin    io.Reader
}

// run executes one command, after the global options, and returns the
// exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	g, args, err := parseGlobal(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stdout, usageText)
		return lifecycle.ExitOK
	}
	if err == nil {
		err = g.faults.apply()
	}
	if err != nil {
		fmt.Fprintf(stderr, "%v\n%s\n", err, usageText)
		return lifecycle.ExitUsage
	}
	g.logSettings()
	r := &runner{
		ctx:      ctx,
		dispatch: platformapi.NewDefaultDispatcher(platformapi.Options{TempDir: g.tempDir}),
		stdout:   stdout, stderr: stderr, stdin: stdin,
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return lifecycle.ExitUsage
	}
	commands := map[string]func([]string) int{
		cmdDispatch:       r.dispatchRequest,
		cmdCapabilities:   r.capabilities,
		cmdCreateArchive:  r.createArchive,
		cmdExtractArchive: r.extractArchive,
		cmdListArchive:    r.readArchive(cmdListArchive, opListArchive),
		cmdVerifyArchive:  r.readArchive(cmdVerifyArchive, opVerifyArchive),
		cmdCompress:       r.compress,
		cmdDecompress:     r.decompress,
		cmdCompressFile:   r.compressFile,
		cmdDecompressFile: r.decompressFile,
	}
	cmd, ok := commands[args[0]]
	if !ok {
		if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
			fmt.Fprintln(stdout, usageText)
			return lifecycle.ExitOK
		}
		fmt.Fprintf(stderr, "unknown command: %s\n%s\n", args[0], usageText)
		return lifecycle.ExitUsage
	}
	return cmd(args[1:])
}

func (r *runner) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(r.stderr)
	return fs
}

func (r *runner) usage(msg string) int {
	fmt.Fprintln(r.stderr, msg)
	return lifecycle.ExitUsage
}

// passwordFlags are the ways a password can be given. A password on the
// command line or in the environment is visible to other users of the
// machine, so neither is read.
type passwordFlags struct {
	file  *string
	stdin *bool
	// removed is the removed --password, kept so that using it fails with
	// what replaces it.
	removed *string
}

func addPasswordFlags(fs *flag.FlagSet) passwordFlags {
	return passwordFlags{
		file:    fs.String("password-file", "", "read the archive password from the first line of this file"),
		stdin:   fs.Bool("password-stdin", false, "read the archive password from the first line of standard input"),
		removed: fs.String("password", "", "removed: use --password-file or --password-stdin"),
	}
}

// errPasswordRemoved refuses the removed --password.
var errPasswordRemoved = errors.New("--password was removed: a password on the command line is visible to other users of this machine; use --password-file or --password-stdin")

// password resolves the password: --password-file, then --password-stdin.
func (r *runner) password(p passwordFlags) (string, error) {
	switch {
	case *p.removed != "":
		return "", errPasswordRemoved
	case *p.file != "":
		f, err := os.Open(*p.file)
		if err != nil {
			return "", fmt.Errorf("--password-file: %w", err)
		}
		defer f.Close()
		return firstLine(f)
	case *p.stdin:
		return firstLine(r.stdin)
	}
	return "", nil
}

func firstLine(rd io.Reader) (string, error) {
	line, err := bufio.NewReader(rd).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// limitFlags are the extraction limits.
type limitFlags struct {
	total, entries, ratio *int64
}

func addLimitFlags(fs *flag.FlagSet) limitFlags {
	return limitFlags{
		total:   fs.Int64("max-total-bytes", 0, "refuse to write more than this many bytes (default 1 TiB)"),
		entries: fs.Int64("max-entries", 0, "refuse archives with more entries (default 1048576)"),
		ratio:   fs.Int64("max-ratio", 0, "refuse output more than this many times the archive's bytes, past the first GiB (default 10000)"),
	}
}

func (l limitFlags) params() map[string]any {
	return map[string]any{"max_total_bytes": *l.total, "max_entries": *l.entries, "max_ratio": *l.ratio}
}

func (r *runner) dispatchRequest(args []string) int {
	fs := r.flags(cmdDispatch)
	request := fs.String("request", "", "full JSON request envelope")
	requestFile := fs.String("request-file", "", "read the request envelope from this file (- for standard input)")
	if err := fs.Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	body := *request
	switch {
	case *requestFile == "-":
		data, err := io.ReadAll(io.LimitReader(r.stdin, jsonbridge.MaxRequestBytes+1))
		if err != nil {
			return r.usage(err.Error())
		}
		body = string(data)
	case *requestFile != "":
		data, err := os.ReadFile(*requestFile)
		if err != nil {
			return r.usage(err.Error())
		}
		body = string(data)
	}
	if body == "" {
		return r.usage("--request or --request-file is required")
	}
	return r.emit(r.dispatch.DispatchJSON(r.ctx, body))
}

func (r *runner) send(operation string, params map[string]any) int {
	body, err := json.Marshal(map[string]any{
		"version": jsonbridge.CurrentVersion, "product": productCompression, "operation": operation, "params": params,
	})
	if err != nil {
		fmt.Fprintln(r.stderr, err)
		return lifecycle.ExitInternal
	}
	return r.emit(r.dispatch.DispatchJSON(r.ctx, string(body)))
}

// emit prints the response envelope and returns its exit code.
func (r *runner) emit(out string) int {
	fmt.Fprintln(r.stdout, out)
	return exitCode(r.ctx, out)
}

// exitCode maps a response envelope to the process exit code.
func exitCode(ctx context.Context, out string) int {
	var env struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string            `json:"code"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return lifecycle.ExitInternal
	}
	if env.Status == "ok" {
		return lifecycle.ExitOK
	}
	if env.Error == nil {
		return lifecycle.ExitInternal
	}
	switch env.Error.Code {
	case "invalid_request", "unknown_product", "unknown_operation":
		return lifecycle.ExitUsage
	case "canceled":
		var sig *lifecycle.SignalError
		if errors.As(context.Cause(ctx), &sig) {
			return lifecycle.ExitCode(sig)
		}
	case "internal_error":
		if env.Error.Details["panic_id"] != "" {
			return lifecycle.ExitInternal
		}
	}
	return lifecycle.ExitFailure
}

func (r *runner) capabilities(args []string) int {
	if err := r.flags(cmdCapabilities).Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	return r.send(opCapabilities, map[string]any{})
}

func (r *runner) createArchive(args []string) int {
	fs := r.flags(cmdCreateArchive)
	input := fs.String("input", "", "input file or folder")
	var inputFiles stringSliceFlag
	fs.Var(&inputFiles, "input-file", "add an input file (repeat for several)")
	output := fs.String("output", "", "output archive path (default: beside the input)")
	workDir := fs.String("work-dir", "", "empty directory to build the archive's database in")
	chunkSize := fs.Int("chunk-size-kb", defaultChunkSizeKB, "chunk size in KiB (4 to 65536, a multiple of 4)")
	splitSizeMB := fs.Int("split-size-mb", 0, "write parts of this many MiB with a .split.json sidecar")
	keepWorkDir := fs.Bool("keep-work-dir", false, "keep the generated work directory")
	format := fs.String("format", defaultFormat, "archive format: duesarc|zip|tar|tar.gz|tar.xz")
	pw := addPasswordFlags(fs)
	if err := fs.Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	if *input == "" && len(inputFiles) == 0 {
		return r.usage("--input or at least one --input-file is required")
	}
	password, err := r.password(pw)
	if err != nil {
		return r.usage(err.Error())
	}
	return r.send(opCreateArchive, map[string]any{
		"input_path": *input, "input_paths": []string(inputFiles), "output_path": *output,
		"work_dir": *workDir, "chunk_size_kb": *chunkSize, "password": password,
		"split_size_mb": *splitSizeMB, "keep_work_dir": *keepWorkDir, "format": *format,
	})
}

func (r *runner) extractArchive(args []string) int {
	fs := r.flags(cmdExtractArchive)
	input := fs.String("input", "", "input archive (a split archive's .001 part or .split.json)")
	output := fs.String("output", "", "output folder (default: the archive's name without its extension)")
	format := fs.String("format", "", "archive format override: duesarc|zip|tar|tar.gz|tar.xz")
	onConflict := fs.String("on-conflict", "overwrite", "what an entry does to an existing file: overwrite|skip|error")
	pw := addPasswordFlags(fs)
	limits := addLimitFlags(fs)
	if err := fs.Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	if *input == "" {
		return r.usage("--input is required")
	}
	password, err := r.password(pw)
	if err != nil {
		return r.usage(err.Error())
	}
	return r.send(opExtractArchive, map[string]any{
		"input_path": *input, "output_path": *output, "password": password, "format": *format,
		"on_conflict": *onConflict, "limits": limits.params(),
	})
}

func (r *runner) readArchive(name, operation string) func([]string) int {
	return func(args []string) int {
		fs := r.flags(name)
		input := fs.String("input", "", "input archive")
		format := fs.String("format", "", "archive format override: duesarc|zip|tar|tar.gz|tar.xz")
		pw := addPasswordFlags(fs)
		if err := fs.Parse(args); err != nil {
			return lifecycle.ExitUsage
		}
		if *input == "" {
			return r.usage("--input is required")
		}
		password, err := r.password(pw)
		if err != nil {
			return r.usage(err.Error())
		}
		return r.send(operation, map[string]any{"input_path": *input, "password": password, "format": *format})
	}
}

func (r *runner) compress(args []string) int {
	fs := r.flags(cmdCompress)
	text := fs.String("text", "", "input text")
	level := fs.String("level", defaultLevel, "compression level: fast|default|best")
	if err := fs.Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	return r.send(opCompressBytes, map[string]any{"data_base64": base64.StdEncoding.EncodeToString([]byte(*text)), "level": *level})
}

func (r *runner) decompress(args []string) int {
	fs := r.flags(cmdDecompress)
	data := fs.String("data-base64", "", "compressed data in base64")
	maxOutput := fs.Int64("max-output-bytes", 0, "refuse output larger than this (default 64 MiB, at most 256 MiB)")
	if err := fs.Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	if *data == "" {
		return r.usage("--data-base64 is required")
	}
	return r.send(opDecompressBytes, map[string]any{"data_base64": *data, "max_output_bytes": *maxOutput})
}

func (r *runner) compressFile(args []string) int {
	fs := r.flags(cmdCompressFile)
	input := fs.String("input", "", "input file")
	output := fs.String("output", "", "output archive (default: <input>.duesarc)")
	pw := addPasswordFlags(fs)
	if err := fs.Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	if *input == "" {
		return r.usage("--input is required")
	}
	password, err := r.password(pw)
	if err != nil {
		return r.usage(err.Error())
	}
	return r.send(opCompressFile, map[string]any{"input_path": *input, "output_path": *output, "password": password})
}

func (r *runner) decompressFile(args []string) int {
	fs := r.flags(cmdDecompressFile)
	input := fs.String("input", "", "input .duesarc of one file, or zstd file")
	output := fs.String("output", "", "output file (default: the input without .duesarc, .dues.zst or .zst)")
	pw := addPasswordFlags(fs)
	limits := addLimitFlags(fs)
	if err := fs.Parse(args); err != nil {
		return lifecycle.ExitUsage
	}
	if *input == "" {
		return r.usage("--input is required")
	}
	password, err := r.password(pw)
	if err != nil {
		return r.usage(err.Error())
	}
	return r.send(opDecompressFile, map[string]any{"input_path": *input, "output_path": *output, "password": password, "limits": limits.params()})
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return fmt.Sprintf("%v", []string(*s))
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
