package main

import (
	"fmt"
	"indicer/api"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/logging"
	"indicer/lib/util"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/fatih/color"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

const (
	duesVersion  = "0.39"
	duesCodename = "<pineapple> spacebar"
)

func init() {
	var err error

	cnst.DECODER, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	handle(err)

	cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	handle(err)
}

type cliFlags struct {
	dbpath            *string
	pwd               *string
	chonkSize         *int
	preset            *string
	quickMode         *bool
	performanceMode   *bool
	lowResourceMode   *bool
	memopt            *bool
	quickOpt          *bool
	containerMode     *bool
	hierarchicalIndex *bool
	compressLevel     *string

	evipath             *string
	parserDiffPath      *string
	syncIndex           *bool
	noIndex             *bool
	enableFTS           *bool
	enableEnrichment    *bool
	storeHashAlgo       *string
	storeParser         *string
	storeHashStrategy   *string
	storePipeline       *string
	storeAsyncFileWrite *bool
	storeIOEngine       *string
	storeIOUringQueue   *int
	hochoMode           *string
	enableSimhash       *bool
	enableRevRel        *bool
	storeWorkers        *int
	storeQueue          *int

	rpath             *string
	restoreWorkers    *int
	restoreQueue      *int
	restoreProgressMs *int
	restoreBufferMB   *int
	rhash             *string

	listStatus *string
	repairFix  *bool

	deep            *bool
	inVerify        *bool
	inTopK          *int
	inhash          *string
	outDeep         *bool
	outExplainExact *bool
	outVerify       *bool
	outTopK         *int
	outpath         *string

	query     *string
	rankAlpha *float64
	fullText  *bool

	microExtractForce *bool
	microExtractTopK  *int

	serverHashAlgo *string
}

type cliCommands struct {
	version      *kingpin.CmdClause
	tui          *kingpin.CmdClause
	store        *kingpin.CmdClause
	restore      *kingpin.CmdClause
	list         *kingpin.CmdClause
	stats        *kingpin.CmdClause
	repair       *kingpin.CmdClause
	near         *kingpin.CmdClause
	nearIn       *kingpin.CmdClause
	nearOut      *kingpin.CmdClause
	search       *kingpin.CmdClause
	micro        *kingpin.CmdClause
	microExtract *kingpin.CmdClause
	microList    *kingpin.CmdClause
	enrich       *kingpin.CmdClause
	server       *kingpin.CmdClause
	reset        *kingpin.CmdClause
	parserDiff   *kingpin.CmdClause
}

type cliDefinition struct {
	app      *kingpin.Application
	flags    cliFlags
	commands cliCommands
}

type pipelineTuning struct {
	storeWorkers   int
	storeQueue     int
	restoreWorkers int
	restoreQueue   int
	activePreset   string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		handle(err)
	}

	if err := closeCompressionCodecs(); err != nil {
		handle(err)
	}
}

func run(args []string) error {
	cliDef := newCLIDefinition()

	if maybeHandleHelp(args) {
		return nil
	}

	debugMode := os.Getenv("DUES_DEBUG") != ""
	if err := logging.InitLogger(debugMode); err != nil {
		return err
	}
	defer logging.Sync()

	logging.GetLogger().Info("DUES starting",
		zap.String("version", duesVersion),
		zap.String("codename", duesCodename))

	parsed := kingpin.MustParse(cliDef.app.Parse(args))

	tuning, err := applyRuntimeConfig(parsed, args, cliDef)
	if err != nil {
		return err
	}

	if err := reconfigureEncoderForCompressionLevel(); err != nil {
		return err
	}

	enforceHierarchicalContainerDependency()

	key, err := util.HashPassword(*cliDef.flags.pwd)
	if err != nil {
		return err
	}

	printRuntimeBanner(parsed, cliDef, tuning)

	return executeCommand(parsed, cliDef, key, tuning)
}

func closeCompressionCodecs() error {
	if err := cnst.ENCODER.Close(); err != nil {
		return err
	}
	cnst.DECODER.Close()
	return nil
}

func newCLIDefinition() cliDefinition {
	app := kingpin.New("DUES", "Deduplicated Unified Evidence Store")
	app.Version(getVersionText())
	app.VersionFlag.Short('v')
	app.HelpFlag.Short('h')

	flags := cliFlags{}
	flags.dbpath = app.Flag(cnst.FlagDBPath, "Custom path for DUES database").Short(cnst.FlagDBPathShort).String()
	flags.pwd = app.Flag(cnst.FlagPassword, "Password for the DUES database").Short(cnst.FlagPasswordShort).String()
	flags.chonkSize = app.Flag(cnst.FlagChonkSize, "Custom chunk size(KB) to be used for dedup").Short(cnst.FlagChonkSizeShort).Default("256").Int()
	flags.preset = app.Flag(cnst.FlagPreset, "Apply convenience preset [quick|performance|low-resource] (explicit flags override preset)").Short(cnst.FlagPresetShort).Default("").String()
	flags.quickMode = app.Flag(cnst.FlagQuickMode, "Convenience preset: equivalent to --preset quick").Short(cnst.FlagQuickModeShort).Default("false").Bool()
	flags.performanceMode = app.Flag(cnst.FlagPerformanceMode, "Convenience preset: equivalent to --preset performance").Short(cnst.FlagPerformanceModeShort).Default("false").Bool()
	flags.lowResourceMode = app.Flag(cnst.FlagLowResourceMode, "Convenience preset: equivalent to --preset low-resource").Short(cnst.FlagLowResourceModeShort).Default("false").Bool()
	flags.memopt = app.Flag(cnst.FlagLowResource, "Low resource use mode, foregoes performance in favour of utilising less memory, cpu, and energy").Short(cnst.FlagLowResourceShort).Default("false").Bool()
	flags.quickOpt = app.Flag(cnst.FlagFastMode, "Quick mode, forgoes encryption, intra-chunk & overall db compression in favour of higher throughput").Short(cnst.FlagFastModeShort).Default("false").Bool()
	flags.containerMode = app.Flag(cnst.FlagContainerMode, "Use container-based storage (packs multiple chunks into 1GB containers)").Short(cnst.FlagContainerModeShort).Default("false").Bool()
	flags.hierarchicalIndex = app.Flag(cnst.FlagHierarchicalIndex, "Use hierarchical block index (groups 1000 chunks per block, requires container mode)").Short(cnst.FlagHierarchicalShort).Default("false").Bool()
	flags.compressLevel = app.Flag(cnst.FlagCompressLevel, "Per-chunk zstd compression level [fast|default|best] (default: best)").Short(cnst.FlagCompressLevelShort).Default("best").String()

	commands := cliCommands{}
	commands.version = app.Command(cnst.CmdVeresion, "Show version details and quick feature overview")
	commands.tui = app.Command(cnst.CmdTui, "Launch interactive TUI interface")

	commands.store = app.Command(cnst.CmdStore, "Store file in database")
	flags.evipath = commands.store.Arg(cnst.OperandFile, "Path of file that must be saved").Required().String()
	flags.syncIndex = commands.store.Flag(cnst.FlagSyncIndex, "Run indexer synchronously (disables overlap with chunk ingest; slower but deterministic ordering)").Short(cnst.FlagSyncIndexShort).Default("false").Bool()
	flags.noIndex = commands.store.Flag(cnst.FlagNoIndex, "Don't run indexer").Short(cnst.FlagNoIndexShort).Default("false").Bool()
	flags.enableFTS = commands.store.Flag(cnst.FlagEnableFts, "Enable full-text search indexing (sidecar Bleve index)").Short(cnst.FlagEnableFtsShort).Default("false").Bool()
	flags.enableEnrichment = commands.store.Flag(cnst.FlagEnableEnrichment, "Upsert disk/partition/indexed-file metadata nodes into graphdb during store").Short(cnst.FlagEnableEnrichmentShort).Default("false").Bool()
	flags.storeHashAlgo = commands.store.Flag(cnst.FlagHashAlgo, "Hashing algorithm to use [sha3|blake3] (default: BLAKE3)").Short(cnst.FlagHashAlgoShort).Default(cnst.BLAKE3).String()
	flags.storeParser = commands.store.Flag(cnst.FlagParser, "Evidence parser backend [tusk|go] (default: tusk, CGo libtsk). 'go' uses the pure-Go lib/parser stack.").Default(cnst.ParserTusk).String()
	flags.storeHashStrategy = commands.store.Flag(cnst.FlagHashStrategy, "Store hash strategy for evidence [sync|async|hocho] (default: sync)").Short(cnst.FlagHashStrategyShort).Default(cnst.SyncHashStrategy).String()
	flags.storePipeline = commands.store.Flag(cnst.FlagStorePipeline, "Store ingest pipeline mode [batch-owner|staged] (default: batch-owner)").Short(cnst.FlagStorePipelineShort).Default(cnst.StorePipelineBatch).String()
	flags.storeAsyncFileWrite = commands.store.Flag(cnst.FlagStoreAsyncFileWrite, "EXPERIMENTAL: queue file-mode chunk writes to an async writer and drain before completion").Short(cnst.FlagStoreAsyncFileWriteShort).Default("false").Bool()
	flags.storeIOEngine = commands.store.Flag(cnst.FlagStoreIOEngine, "Store write engine [auto|stdlib|io-uring] (default: auto)").Short(cnst.FlagStoreIOEngineShort).Default(cnst.StoreIOEngineAuto).String()
	flags.storeIOUringQueue = commands.store.Flag(cnst.FlagStoreIOUringQueueDepth, "io-uring SQ/CQ depth (0 = auto-tuned)").Short(cnst.FlagStoreIOUringQueueDepthShort).Default("0").Int()
	flags.hochoMode = commands.store.Flag(cnst.FlagHochoMode, "Hocho logical hashing mode [baseline|reuse|postdedup] (default: baseline)").Short(cnst.FlagHochoModeShort).Default(cnst.HochoModeBaseline).String()
	flags.enableSimhash = commands.store.Flag(cnst.FlagSimhash, "Compute and store per-chunk simhash signatures during ingest (enables NeAR chunk-level similarity; off by default)").Short(cnst.FlagSimhashShort).Default("false").Bool()
	flags.enableRevRel = commands.store.Flag(cnst.FlagEnableRevRel, "Persist reverse-relation keys during ingest (off by default for higher ingest throughput)").Short(cnst.FlagEnableRevRelShort).Default("false").Bool()
	flags.storeWorkers = commands.store.Flag(cnst.FlagStoreWorkers, "Override batch-owner worker count (0 = mode-aware default)").Short(cnst.FlagStoreWorkersShort).Default("0").Int()
	flags.storeQueue = commands.store.Flag(cnst.FlagStoreQueue, "Override batch-owner task queue depth (0 = mode-aware default)").Short(cnst.FlagStoreQueueShort).Default("0").Int()

	commands.restore = app.Command(cnst.CmdRestore, "Restore file from database")
	flags.rpath = commands.restore.Flag(cnst.FlagRestoreFilePath, "Path for restoring the file").Short(cnst.FlagRestoreFilePathShort).Default("restored").String()
	flags.restoreWorkers = commands.restore.Flag(cnst.FlagRestoreWorkers, "Override restore worker count (0 = adaptive default)").Short(cnst.FlagRestoreWorkersShort).Default("0").Int()
	flags.restoreQueue = commands.restore.Flag(cnst.FlagRestoreQueue, "Override restore pipeline queue depth (0 = adaptive default)").Short(cnst.FlagRestoreQueueShort).Default("0").Int()
	flags.restoreProgressMs = commands.restore.Flag(cnst.FlagRestoreProgressMs, "Restore progress update interval in milliseconds (default: 120)").Short(cnst.FlagRestoreProgressMsShort).Default("120").Int()
	flags.restoreBufferMB = commands.restore.Flag(cnst.FlagRestoreBufferMB, "Restore output buffer size in MB (0 = mode-aware default: low=32, high=256)").Short(cnst.FlagRestoreBufferMBShort).Default("0").Int()
	flags.rhash = commands.restore.Arg(cnst.OperandHash, "Hash of file that must be restoed").String()

	commands.list = app.Command(cnst.CmdList, "List all the saved files in the database")
	flags.listStatus = commands.list.Flag(cnst.FlagListStatus, "Filter evidence by status [all|completed|pending|failed]").Short(cnst.FlagListStatusShort).Default("all").String()
	commands.stats = app.Command(cnst.CmdStats, "Show database statistics")
	commands.repair = app.Command(cnst.CmdRepair, "Inspect and optionally mark partial evidence ingests as failed")
	flags.repairFix = commands.repair.Flag(cnst.FlagRepairFix, "Mark pending evidence ingests as failed so they are visible and retryable").Short(cnst.FlagRepairFixShort).Default("false").Bool()

	commands.near = app.Command(cnst.CmdNear, "Get NeAR file objects")
	commands.nearIn = commands.near.Command(cnst.SubCmdIn, "Finds NeAR objects & generates GReAt graph for file INside of the database")
	flags.deep = commands.nearIn.Flag(cnst.FlagDeep, "Enable/Disable partial chunk match").Short(cnst.FlagDeepShort).Default("false").Bool()
	flags.inVerify = commands.nearIn.Flag(cnst.FlagAdvancedDeep, "Phase 2: full-file SimHash re-ranking of top K candidates").Short(cnst.FlagAdvancedDeepShort).Default("false").Bool()
	flags.inTopK = commands.nearIn.Flag(cnst.FlagTopK, "Top K candidates for Phase 2 re-ranking (0 = auto-select based on available resources)").Short(cnst.FlagTopKShort).Default("0").Int()
	flags.inhash = commands.nearIn.Arg(cnst.OperandHash, "Hash of the file in DUES DB for which you need to run NeAR").String()

	commands.nearOut = commands.near.Command(cnst.SubCmdOut, "Finds NeAR objects & generates GReAt graph for file OUTside of the database")
	flags.outDeep = commands.nearOut.Flag(cnst.FlagDeep, "Enable/Disable partial chunk match").Short(cnst.FlagDeepShort).Default("false").Bool()
	flags.outExplainExact = commands.nearOut.Flag(cnst.FlagExplainExact, "Force chunk-level drilldown even when an exact file hash match exists").Short(cnst.FlagExplainExactShort).Default("false").Bool()
	flags.outVerify = commands.nearOut.Flag(cnst.FlagAdvancedDeep, "Phase 2: full-file SimHash re-ranking of top K candidates").Short(cnst.FlagAdvancedDeepShort).Default("false").Bool()
	flags.outTopK = commands.nearOut.Flag(cnst.FlagTopK, "Top K candidates for Phase 2 re-ranking (0 = auto-select based on available resources)").Short(cnst.FlagTopKShort).Default("0").Int()
	flags.outpath = commands.nearOut.Arg(cnst.OperandFile, "Path to the file for which you need to run NeAR").String()

	commands.search = app.Command(cnst.CmdSearch, "Search anything in DUES DB")
	flags.query = commands.search.Arg(cnst.OperandQuery, "Search query string").String()
	flags.rankAlpha = commands.search.Flag(cnst.FlagRankAlpha, "Occurrence boost weight for ranking (>= 0, default: 0.35)").Short(cnst.FlagRankAlphaShort).Default("0.35").Float64()
	flags.fullText = commands.search.Flag(cnst.FlagEnableFts, "Enable sidecar full-text search first, then fallback to scan path").Short(cnst.FlagEnableFtsShort).Default("false").Bool()

	commands.micro = app.Command(cnst.CmdMicro, "Manage micro-artefacts")
	commands.microExtract = commands.micro.Command("extract", "Extract micro-artefacts from all indexed files and populate graph database")
	flags.microExtractForce = commands.microExtract.Flag("force", "Re-process indexed files even when micro-artefacts already exist in graph").Short(cnst.FlagMicroExtractForceShort).Default("false").Bool()
	flags.microExtractTopK = commands.microExtract.Flag("top-k", "Top-K indexed files (by artefact count) to include in exported HTML graph (0 = all)").Short(cnst.FlagMicroExtractTopKShort).Default("1").Int()
	commands.microList = commands.micro.Command(cnst.CmdList, "List all micro-artefacts in JSON format")

	commands.enrich = app.Command(cnst.CmdEnrich, "Backfill file hierarchy metadata enrichment into graphdb")

	commands.server = app.Command(cnst.CmdServer, "Run gRPC / Web combined DUES server")
	flags.serverHashAlgo = commands.server.Flag(cnst.FlagHashAlgo, "Hashing algorithm to use [sha3|blake3] (default: BLAKE3)").Short(cnst.FlagHashAlgoShort).Default(cnst.BLAKE3).String()

	commands.reset = app.Command(cnst.CmdReset, "Delete the database").Alias(cnst.CmdPurge).Alias(cnst.CmdDelete).Alias(cnst.CmdDestroy)

	commands.parserDiff = app.Command("parser-diff", "Compare libtusk vs pure-Go parser output on an image (migration validation)")
	flags.parserDiffPath = commands.parserDiff.Arg(cnst.OperandFile, "Path of the evidence image to compare").Required().String()

	return cliDefinition{app: app, flags: flags, commands: commands}
}

func applyRuntimeConfig(parsed string, args []string, cliDef cliDefinition) (pipelineTuning, error) {
	tuning := pipelineTuning{}
	preset, err := resolvePreset(args, cliDef.flags)
	if err != nil {
		return tuning, err
	}
	tuning.activePreset = preset

	memOptEffective := *cliDef.flags.memopt
	quickOptEffective := *cliDef.flags.quickOpt
	compressLevelEffective := strings.ToLower(*cliDef.flags.compressLevel)
	if preset != "" {
		memOptEffective, quickOptEffective, compressLevelEffective, err = applyPresetDefaults(preset, args, memOptEffective, quickOptEffective, compressLevelEffective)
		if err != nil {
			return tuning, err
		}
	}

	cnst.MEMOPT = memOptEffective
	cnst.QUICKOPT = quickOptEffective
	cnst.CONTAINERMODE = *cliDef.flags.containerMode
	cnst.HIERARCHICALINDEX = *cliDef.flags.hierarchicalIndex
	cnst.CompressLevel = compressLevelEffective

	selectedHashAlgo := cnst.BLAKE3
	switch parsed {
	case cliDef.commands.store.FullCommand():
		selectedHashAlgo = *cliDef.flags.storeHashAlgo
	case cliDef.commands.server.FullCommand():
		selectedHashAlgo = *cliDef.flags.serverHashAlgo
	}
	if err := cnst.SetHashAlgo(selectedHashAlgo); err != nil {
		return tuning, err
	}

	if parsed == cliDef.commands.store.FullCommand() {
		if err := cnst.SetStoreHashStrategy(*cliDef.flags.storeHashStrategy); err != nil {
			return tuning, err
		}
		if err := cnst.SetStorePipeline(*cliDef.flags.storePipeline); err != nil {
			return tuning, err
		}
		if err := cnst.SetHochoMode(*cliDef.flags.hochoMode); err != nil {
			return tuning, err
		}
		if err := cnst.SetParser(*cliDef.flags.storeParser); err != nil {
			return tuning, err
		}
		if cnst.StoreHashStrategy != cnst.HochoHashStrategy && hasFlagArg(args, cnst.FlagHochoMode) {
			return tuning, fmt.Errorf("--%s is only valid when --%s=hocho", cnst.FlagHochoMode, cnst.FlagHashStrategy)
		}

		cnst.ENABLESIMHASH = *cliDef.flags.enableSimhash
		cnst.ENABLEREVREL = *cliDef.flags.enableRevRel
		cnst.StoreAsyncFileWrites = *cliDef.flags.storeAsyncFileWrite
		if err := cnst.SetStoreIOEngine(*cliDef.flags.storeIOEngine); err != nil {
			return tuning, err
		}
		if err := cnst.SetStoreIOUringQueueDepth(*cliDef.flags.storeIOUringQueue); err != nil {
			return tuning, err
		}

		cnst.StoreWorkerCount = *cliDef.flags.storeWorkers
		cnst.StoreTaskQueueDepth = *cliDef.flags.storeQueue
		tuning.storeWorkers, tuning.storeQueue = cnst.GetStorePipelineTuning(cnst.CONTAINERMODE, cnst.HIERARCHICALINDEX)
	}

	if parsed == cliDef.commands.restore.FullCommand() {
		cnst.RestoreWorkerCount = *cliDef.flags.restoreWorkers
		cnst.RestoreTaskQueueDepth = *cliDef.flags.restoreQueue
		cnst.RestoreProgressIntervalMs = *cliDef.flags.restoreProgressMs
		cnst.RestoreWriteBufferMB = *cliDef.flags.restoreBufferMB
		tuning.restoreWorkers, tuning.restoreQueue = cnst.GetRestorePipelineTuning()
	}

	return tuning, nil
}

func resolvePreset(args []string, flags cliFlags) (string, error) {
	presets := make([]string, 0, 4)
	if val := normalizePresetName(*flags.preset); val != "" {
		presets = append(presets, val)
	} else if strings.TrimSpace(*flags.preset) != "" {
		return "", fmt.Errorf("invalid --%s value: must be %s, %s, or %s", cnst.FlagPreset, cnst.PresetQuick, cnst.PresetPerformance, cnst.PresetLowResource)
	}
	if *flags.quickMode {
		presets = append(presets, cnst.PresetQuick)
	}
	if *flags.performanceMode {
		presets = append(presets, cnst.PresetPerformance)
	}
	if *flags.lowResourceMode {
		presets = append(presets, cnst.PresetLowResource)
	}

	if len(presets) == 0 {
		return "", nil
	}

	active := presets[0]
	for i := 1; i < len(presets); i++ {
		if presets[i] != active {
			return "", fmt.Errorf("conflicting mode presets: choose only one of %s, %s, %s", cnst.PresetQuick, cnst.PresetPerformance, cnst.PresetLowResource)
		}
	}

	if hasAnyFlagArg(args, cnst.FlagQuickMode, cnst.FlagQuickModeShort) {
		active = cnst.PresetQuick
	}
	if hasAnyFlagArg(args, cnst.FlagPerformanceMode, cnst.FlagPerformanceModeShort) {
		active = cnst.PresetPerformance
	}
	if hasAnyFlagArg(args, cnst.FlagLowResourceMode, cnst.FlagLowResourceModeShort) {
		active = cnst.PresetLowResource
	}

	return active, nil
}

func normalizePresetName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case cnst.PresetQuick:
		return cnst.PresetQuick
	case cnst.PresetPerformance:
		return cnst.PresetPerformance
	case "low", "low-resource", "lowresource":
		return cnst.PresetLowResource
	default:
		return ""
	}
}

func applyPresetDefaults(preset string, args []string, memOpt bool, quickOpt bool, compressLevel string) (bool, bool, string, error) {
	switch preset {
	case cnst.PresetQuick:
		if !hasAnyFlagArg(args, cnst.FlagFastMode, cnst.FlagFastModeShort) {
			quickOpt = true
		}
		if !hasAnyFlagArg(args, cnst.FlagLowResource, cnst.FlagLowResourceShort) {
			memOpt = false
		}
		if !hasAnyFlagArg(args, cnst.FlagCompressLevel, cnst.FlagCompressLevelShort) {
			compressLevel = "fast"
		}
	case cnst.PresetPerformance:
		if !hasAnyFlagArg(args, cnst.FlagFastMode, cnst.FlagFastModeShort) {
			quickOpt = false
		}
		if !hasAnyFlagArg(args, cnst.FlagLowResource, cnst.FlagLowResourceShort) {
			memOpt = false
		}
		if !hasAnyFlagArg(args, cnst.FlagCompressLevel, cnst.FlagCompressLevelShort) {
			compressLevel = "best"
		}
	case cnst.PresetLowResource:
		if !hasAnyFlagArg(args, cnst.FlagFastMode, cnst.FlagFastModeShort) {
			quickOpt = false
		}
		if !hasAnyFlagArg(args, cnst.FlagLowResource, cnst.FlagLowResourceShort) {
			memOpt = true
		}
		if !hasAnyFlagArg(args, cnst.FlagCompressLevel, cnst.FlagCompressLevelShort) {
			compressLevel = "default"
		}
	default:
		return false, false, "", fmt.Errorf("unsupported preset: %s", preset)
	}
	return memOpt, quickOpt, compressLevel, nil
}

func hasFlagArg(args []string, flagName string) bool {
	prefix := "--" + flagName
	for _, arg := range args {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
}

func hasAnyFlagArg(args []string, longFlag string, shortFlag rune) bool {
	if hasFlagArg(args, longFlag) {
		return true
	}
	if shortFlag == 0 {
		return false
	}
	short := "-" + string(shortFlag)
	for _, arg := range args {
		if arg == short || strings.HasPrefix(arg, short+"=") {
			return true
		}
	}
	return false
}

func reconfigureEncoderForCompressionLevel() error {
	if err := cnst.ENCODER.Close(); err != nil {
		return err
	}

	encLevel := zstd.SpeedBestCompression
	switch cnst.CompressLevel {
	case "fast":
		encLevel = zstd.SpeedFastest
	case "default":
		encLevel = zstd.SpeedDefault
	case "best":
		encLevel = zstd.SpeedBestCompression
	}

	var err error
	cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(encLevel))
	return err
}

func enforceHierarchicalContainerDependency() {
	if cnst.HIERARCHICALINDEX && !cnst.CONTAINERMODE {
		color.Red("⚠️  Hierarchical index requires container mode. Enabling container mode automatically.")
		cnst.CONTAINERMODE = true
	}
}

func printRuntimeBanner(parsed string, cliDef cliDefinition, tuning pipelineTuning) {
	if parsed == cliDef.commands.tui.FullCommand() || parsed == cliDef.commands.version.FullCommand() {
		return
	}
	if tuning.activePreset != "" {
		color.Cyan("🎛 active preset: %s", tuning.activePreset)
	}

	if cnst.MEMOPT {
		color.Green("🍃 running in LOW RESOURCE mode 🍃")
	} else {
		color.Cyan("⚡ running in HIGH PERFORMANCE mode (CPU x2 workers) ⚡")
	}
	if cnst.QUICKOPT {
		color.Magenta("🛫 quick mode enabled 🛬")
	}
	if cnst.CONTAINERMODE {
		color.Yellow("📦 container mode enabled 📦")
	}
	if cnst.HIERARCHICALINDEX {
		color.Magenta("🏛 hierarchical index enabled (2-level lookup) 🏛")
	}
	if cnst.CompressLevel != "default" {
		color.Yellow("🗜  chunk compression level: %s", cnst.CompressLevel)
	}
	if cnst.ENABLESIMHASH {
		color.Yellow("🔍 simhash enabled (--simhash)")
	}
	if cnst.ENABLEREVREL {
		color.Yellow("↩ reverse relations enabled (--%s)", cnst.FlagEnableRevRel)
	}

	if parsed == cliDef.commands.store.FullCommand() {
		color.Cyan("⚙ store pipeline: workers=%d queue=%d", tuning.storeWorkers, tuning.storeQueue)
		color.Cyan("⚙ store pipeline mode: %s", cnst.StorePipelineMode)
		color.Cyan("⚙ store io engine: %s", cnst.StoreIOEngine)
		if cnst.StoreAsyncFileWrites {
			color.Yellow("⚗ async file writes: enabled (experimental)")
		}
		if cnst.StoreHashStrategy == cnst.HochoHashStrategy {
			color.Cyan("⚙ hocho mode: %s", cnst.HochoMode)
		}
	}

	if parsed == cliDef.commands.restore.FullCommand() {
		color.Cyan("⚙ restore pipeline: workers=%d queue=%d progress=%s buffer=%dMB", tuning.restoreWorkers, tuning.restoreQueue, cnst.GetRestoreProgressInterval(), cnst.GetRestoreWriteBufferSize()/(1024*1024))
	}
}

func executeCommand(parsed string, cliDef cliDefinition, key []byte, tuning pipelineTuning) error {
	handlers := buildCommandHandlers(cliDef, key, tuning)
	handler, ok := handlers[parsed]
	if !ok {
		return fmt.Errorf("unsupported command: %s", parsed)
	}
	return handler()
}

func buildCommandHandlers(cliDef cliDefinition, key []byte, tuning pipelineTuning) map[string]func() error {
	return map[string]func() error{
		cliDef.commands.version.FullCommand(): func() error {
			logging.GetLogger().Debug("Command: version")
			printVersionInfo()
			return nil
		},
		cliDef.commands.tui.FullCommand(): func() error {
			logging.GetLogger().Info("Command: tui", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.TUICmd(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key)
		},
		cliDef.commands.store.FullCommand(): func() error {
			logging.GetLogger().Info("Command: store",
				zap.String("file", *cliDef.flags.evipath),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.String("store_pipeline_mode", cnst.StorePipelineMode),
				zap.String("store_io_engine", cnst.StoreIOEngine),
				zap.Bool("store_async_file_write", cnst.StoreAsyncFileWrites),
				zap.String("hash_strategy", cnst.StoreHashStrategy),
				zap.Bool("sync_index", *cliDef.flags.syncIndex),
				zap.Bool("no_index", *cliDef.flags.noIndex),
				zap.Bool("fts_enabled", *cliDef.flags.enableFTS),
				zap.Bool("enrichment_enabled", *cliDef.flags.enableEnrichment),
				zap.Bool("reverse_relations_enabled", cnst.ENABLEREVREL),
				zap.Int("store_workers", *cliDef.flags.storeWorkers),
				zap.Int("store_queue", *cliDef.flags.storeQueue),
				zap.Int("store_workers_effective", tuning.storeWorkers),
				zap.Int("store_queue_effective", tuning.storeQueue))
			return cli.StoreData(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, *cliDef.flags.evipath, key, *cliDef.flags.syncIndex, *cliDef.flags.noIndex, *cliDef.flags.enableFTS, *cliDef.flags.enableEnrichment)
		},
		cliDef.commands.parserDiff.FullCommand(): func() error {
			logging.GetLogger().Info("Command: parser-diff", zap.String("file", *cliDef.flags.parserDiffPath))
			return cli.ParserDiff(*cliDef.flags.parserDiffPath)
		},
		cliDef.commands.restore.FullCommand(): func() error {
			logging.GetLogger().Info("Command: restore",
				zap.String("hash", *cliDef.flags.rhash),
				zap.String("output_path", *cliDef.flags.rpath),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.Int("restore_workers", *cliDef.flags.restoreWorkers),
				zap.Int("restore_queue", *cliDef.flags.restoreQueue),
				zap.Int("restore_progress_ms", *cliDef.flags.restoreProgressMs),
				zap.Int("restore_buffer_mb", *cliDef.flags.restoreBufferMB),
				zap.Int("restore_workers_effective", tuning.restoreWorkers),
				zap.Int("restore_queue_effective", tuning.restoreQueue))
			return cli.RestoreData(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, *cliDef.flags.rhash, *cliDef.flags.rpath, key)
		},
		cliDef.commands.list.FullCommand(): func() error {
			logging.GetLogger().Info("Command: list",
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.String("status", strings.ToLower(*cliDef.flags.listStatus)))
			return cli.ListData(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key, *cliDef.flags.listStatus)
		},
		cliDef.commands.stats.FullCommand(): func() error {
			logging.GetLogger().Info("Command: stats", zap.String("dbpath", *cliDef.flags.dbpath), zap.Int("chonksize_kb", *cliDef.flags.chonkSize))
			return cli.StatsData(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key)
		},
		cliDef.commands.repair.FullCommand(): func() error {
			logging.GetLogger().Info("Command: repair",
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.Bool("fix", *cliDef.flags.repairFix))
			return cli.RepairData(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key, *cliDef.flags.repairFix)
		},
		cliDef.commands.nearIn.FullCommand(): func() error {
			logging.GetLogger().Info("Command: near in",
				zap.String("hash", *cliDef.flags.inhash),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Bool("deep", *cliDef.flags.deep),
				zap.Int("topk", *cliDef.flags.inTopK),
				zap.Bool("verify", *cliDef.flags.inVerify))
			return cli.NearInData(*cliDef.flags.deep, *cliDef.flags.inVerify, *cliDef.flags.inTopK, *cliDef.flags.chonkSize, *cliDef.flags.dbpath, *cliDef.flags.inhash, key)
		},
		cliDef.commands.nearOut.FullCommand(): func() error {
			logging.GetLogger().Info("Command: near out",
				zap.String("file_path", *cliDef.flags.outpath),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Bool("deep", *cliDef.flags.outDeep),
				zap.Int("topk", *cliDef.flags.outTopK),
				zap.Bool("verify", *cliDef.flags.outVerify),
				zap.Bool("explain_exact", *cliDef.flags.outExplainExact))
			return cli.NearOutData(*cliDef.flags.outDeep, *cliDef.flags.outExplainExact, *cliDef.flags.outVerify, *cliDef.flags.outTopK, *cliDef.flags.chonkSize, *cliDef.flags.dbpath, *cliDef.flags.outpath, key)
		},
		cliDef.commands.search.FullCommand(): func() error {
			logging.GetLogger().Info("Command: search",
				zap.String("query", *cliDef.flags.query),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Float64("rank_alpha", *cliDef.flags.rankAlpha),
				zap.Bool("fulltext", *cliDef.flags.fullText))
			return cli.SearchCmd(*cliDef.flags.chonkSize, *cliDef.flags.query, *cliDef.flags.dbpath, key, *cliDef.flags.rankAlpha, *cliDef.flags.fullText)
		},
		cliDef.commands.microExtract.FullCommand(): func() error {
			logging.GetLogger().Info("Command: micro extract",
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("topk", *cliDef.flags.microExtractTopK),
				zap.Bool("force", *cliDef.flags.microExtractForce))
			return cli.MicroArtefactCmd(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key, *cliDef.flags.microExtractForce, *cliDef.flags.microExtractTopK)
		},
		cliDef.commands.microList.FullCommand(): func() error {
			logging.GetLogger().Info("Command: micro list", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.ListMicroArtefactsCmd(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key)
		},
		cliDef.commands.enrich.FullCommand(): func() error {
			logging.GetLogger().Info("Command: enrich", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.EnrichData(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key)
		},
		cliDef.commands.reset.FullCommand(): func() error {
			logging.GetLogger().Info("Command: reset", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.ResetData(*cliDef.flags.dbpath)
		},
		cliDef.commands.server.FullCommand(): func() error {
			logging.GetLogger().Info("Command: server", zap.String("dbpath", *cliDef.flags.dbpath), zap.Int("chonksize_kb", *cliDef.flags.chonkSize))
			return api.Server(*cliDef.flags.chonkSize, *cliDef.flags.dbpath, key)
		},
	}
}

func getVersionText() string {
	return fmt.Sprintf(
		"DUES v%s  (%s)\n"+
			"Deduplicated Unified Evidence Store\n\n"+
			"Highlights\n"+
			"  - Chunk-level deduplication with encrypted storage\n"+
			"  - Fast restore, NeAR similarity analysis, and search\n"+
			"  - Short flags + presets for faster daily usage\n\n"+
			"Try: dues --help\n"+
			"For detailed guide: dues version\n",
		duesVersion,
		duesCodename,
	)
}

func printVersionInfo() {
	title := color.New(color.FgCyan, color.Bold).SprintFunc()
	accent := color.New(color.FgBlue, color.Bold).SprintFunc()
	muted := color.New(color.FgHiBlack).SprintFunc()

	fmt.Println(title("\nDUES"), muted("Deduplicated Unified Evidence Store"))
	fmt.Println(accent("Version:"), fmt.Sprintf("%s (%s)", duesVersion, duesCodename))
	fmt.Println(muted("----------------------------------------"))

	printHelpSection("Overview")
	fmt.Println("  Built for evidence-heavy workflows where space, speed, and traceability matter.")

	printHelpSection("Core Capabilities")
	fmt.Println("  - Store files with chunk-level deduplication")
	fmt.Println("  - Restore by hash")
	fmt.Println("  - Search indexed content and metadata")
	fmt.Println("  - Run NeAR similarity checks")

	printHelpSection("Experimental Features")
	fmt.Println("  - Rich TUI")
	fmt.Println("  - Server via gRPC/Web mode")

	printHelpSection("Presets")
	fmt.Println("  --preset quick        Fast ingest defaults (throughput-first)")
	fmt.Println("  --preset performance  Balanced/high-performance defaults")
	fmt.Println("  --preset low-resource Conservative defaults for constrained systems")
	fmt.Println("  Aliases: --quick-mode, --performance-mode, --low-resource-mode")

	printHelpSection("Quick Start")
	fmt.Println("  dues store FILE --dbpath ./caseA")
	fmt.Println("  dues list --dbpath ./caseA")
	fmt.Println("  dues restore HASH --filepath restored.bin")
	fmt.Println("  dues search QUERY --fts")

	fmt.Println(muted("----------------------------------------"))
}

func maybeHandleHelp(args []string) bool {
	if len(args) == 0 {
		printRootHelp()
		return true
	}

	if len(args) == 1 {
		switch args[0] {
		case "--help", "-h", "help":
			printRootHelp()
			return true
		}
	}

	if args[0] == "help" {
		printHelpForPath(args[1:])
		return true
	}

	for i, arg := range args {
		if arg == "--help" || arg == "-h" {
			printHelpForPath(args[:i])
			return true
		}
	}

	return false
}

func printHelpForPath(path []string) {
	if len(path) == 0 {
		printRootHelp()
		return
	}

	if path[0] == cnst.CmdNear {
		if len(path) > 1 {
			switch path[1] {
			case cnst.SubCmdIn:
				printNearInHelp()
				return
			case cnst.SubCmdOut:
				printNearOutHelp()
				return
			}
		}
		printNearHelp()
		return
	}

	if path[0] == cnst.CmdMicro {
		if len(path) > 1 {
			switch path[1] {
			case "extract":
				printMicroExtractHelp()
				return
			case cnst.CmdList:
				printMicroListHelp()
				return
			}
		}
		printMicroArtefactsHelp()
		return
	}

	switch path[0] {
	case cnst.CmdStore:
		printStoreHelp()
	case cnst.CmdRestore:
		printRestoreHelp()
	case cnst.CmdList:
		printListHelp()
	case cnst.CmdStats:
		printStatsHelp()
	case cnst.CmdSearch:
		printSearchHelp()
	case cnst.CmdEnrich:
		printEnrichHelp()
	case cnst.CmdServer:
		printServerHelp()
	case cnst.CmdReset:
		printResetHelp()
	case cnst.CmdVeresion:
		printVersionHelp()
	case cnst.CmdTui:
		printTuiHelp()
	default:
		color.Red("Unknown command: %s", strings.Join(path, " "))
		fmt.Println("")
		printRootHelp()
	}
}

func printRootHelp() {
	title := color.New(color.FgCyan, color.Bold).SprintFunc()
	cmd := color.New(color.FgBlue, color.Bold).SprintFunc()
	muted := color.New(color.FgHiBlack).SprintFunc()

	fmt.Println(title("\nDUES - Deduplicated Unified Evidence Store"))
	fmt.Println(muted("Reliable evidence storage, deduplication, search, and restore."))
	printHelpSection("Usage")
	fmt.Println("  dues [global options] <command> [command options]")

	printHelpSection("Global Options")
	fmt.Printf("  %s, %s   Show this help\n", cmd("--help"), cmd("-h"))
	fmt.Printf("  %s, %s   Show version and highlights\n", cmd("--version"), cmd("-v"))
	fmt.Printf("  %s, %s   Custom path for DUES database\n", cmd("--dbpath"), cmd("-d"))
	fmt.Printf("  %s, %s   Password for the DUES database\n", cmd("--password"), cmd("-p"))
	fmt.Printf("  %s, %s   Custom chunk size (KB), default 256\n", cmd("--chonksize"), cmd("-c"))
	fmt.Printf("  %s, %s   Low resource mode\n", cmd("--low"), cmd("-l"))
	fmt.Printf("  %s, %s   Quick mode (less protection, more throughput)\n", cmd("--quick"), cmd("-q"))
	fmt.Printf("  %s, %s   Container mode\n", cmd("--container"), cmd("-x"))
	fmt.Printf("  %s, %s   Hierarchical index (requires container mode)\n", cmd("--hierarchical"), cmd("-i"))
	fmt.Printf("  %s, %s   Chunk compression level [fast|default|best]\n", cmd("--compress-level"), cmd("-z"))
	fmt.Printf("  %s, %s   Mode preset [quick|performance|low-resource]\n", cmd("--preset"), cmd("-P"))
	fmt.Printf("  %s, %s   Quick preset convenience alias\n", cmd("--quick-mode"), cmd("-Q"))
	fmt.Printf("  %s, %s   Performance preset convenience alias\n", cmd("--performance-mode"), cmd("-o"))
	fmt.Printf("  %s, %s   Low-resource preset convenience alias\n", cmd("--low-resource-mode"), cmd("-L"))

	printHelpSection("Preset Rules")
	fmt.Println("  Presets are convenience defaults.")
	fmt.Println("  Explicit flags always override preset-derived values.")
	fmt.Println("  Use only one preset at a time.")

	printHelpSection("Preset Decision Guide")
	fmt.Println("  quick:        Maximum ingest throughput, lower protection/compression")
	fmt.Println("  performance:  Balanced/default behavior for most workstations")
	fmt.Println("  low-resource: Best for constrained CPU/memory systems")
	fmt.Println("  Tip: start with --preset performance and override only what you need.")

	printHelpSection("Commands")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdTui), "Launch interactive TUI interface")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdStore), "Store file in database")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdRestore), "Restore file from database")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdList), "List saved files")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdSearch), "Search metadata/content index")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdMicro), "Manage micro-artefacts (extract, list)")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdEnrich), "Backfill file hierarchy metadata into graphdb")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdNear), "Find NeAR file objects")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdServer), "Run gRPC/Web server")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdReset), "Delete database")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdVeresion), "Show version details")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdStats), "Show stats about data")

	printHelpSection("Command-Level Help")
	fmt.Println("  Root help and command help are both available.")
	fmt.Println("  Use: dues help <command>")
	fmt.Println("  or : dues <command> --help")
	fmt.Println("  Example: dues help near")
	fmt.Println("  Example: dues near in --help")

	printHelpSection("Examples")
	fmt.Println("  dues store evidence.img --dbpath ./dues-data")
	fmt.Println("  dues store evidence.img --preset quick")
	fmt.Println("  dues store evidence.img --quick-mode --fts --enrich")
	fmt.Println("  dues restore <hash> --filepath recovered.bin")
	fmt.Println("  dues restore <hash> -y 8 -U 32 -m 100")
	fmt.Println("  dues search \"invoice\" --fts")
	fmt.Println("  dues enrich --dbpath ./dues-data")
	fmt.Println("  dues micro extract --top-k 20")
	fmt.Println("  dues near in <hash> --deep")

	printHelpSection("Workflow Recipes")
	fmt.Println("  Triage (fast ingest):")
	fmt.Println("    dues store E01-image.dd --preset quick --fts")
	fmt.Println("  Balanced case ingest:")
	fmt.Println("    dues store E01-image.dd --preset performance --enrich --fts")
	fmt.Println("  Low-resource laptop ingest:")
	fmt.Println("    dues store E01-image.dd --preset low-resource --compress-level default")
	fmt.Println("  Investigation loop:")
	fmt.Println("    dues search \"keyword\" --fts")
	fmt.Println("    dues near in <hash> --deep --advanced-deep --top-k 20")
	fmt.Println("    dues restore <hash> --filepath recovered.bin")
	fmt.Println("")
}

func printStoreHelp() {
	printHelpHeader("store")
	fmt.Println("Usage: dues store FILE [--sync|-s] [--no-index|-N] [--fts|-F] [--enrich|-E] [--hash-algo|-g [sha3|blake3]] [--hash-strategy|-S [sync|async|hocho]] [--pipeline|-Y [batch-owner|staged]] [--simhash|-H] [--revrel|-R] [--store-workers|-w N] [--store-queue|-W N] [global options]")
	fmt.Println("Stores a file in the DUES database using chunk-level deduplication.")
	fmt.Println("")
	printHelpSection("Most-Used Flags")
	fmt.Println("  -F, --fts            Enable full-text indexing during store")
	fmt.Println("  -E, --enrich         Enrich hierarchy metadata during store")
	fmt.Println("  -N, --no-index       Skip indexing")
	fmt.Println("  -w, --store-workers  Override worker count")
	fmt.Println("  -W, --store-queue    Override queue depth")
	fmt.Println("")
	printHelpSection("Quick Paths")
	fmt.Println("  Everyday default:")
	fmt.Println("    dues store FILE")
	fmt.Println("  Throughput-first:")
	fmt.Println("    dues store FILE --preset quick")
	fmt.Println("  Constrained machine:")
	fmt.Println("    dues store FILE --preset low-resource")
	fmt.Println("  Advanced tuning:")
	fmt.Println("    dues store FILE -w 24 -W 192 -Y batch-owner")
	fmt.Println("")
	printHelpSection("Store Pipeline")
	fmt.Println("  Fan-out/fan-in model:")
	fmt.Println("    worker goroutines: hash + materialize chunk payloads in parallel")
	fmt.Println("    single writer: batch metadata + relation/revrel writes")
	fmt.Println("  This keeps expensive chunk work parallel while preserving deterministic batch ownership.")
	fmt.Println("")
	printHelpSection("Store Tuning")
	fmt.Println("  --store-workers, -w N   Override worker count (0 = mode-aware default)")
	fmt.Println("  --store-queue, -W N     Override task queue depth (0 = mode-aware default)")
	fmt.Println("  --revrel, -R            Persist reverse-relation keys during ingest (off by default)")
	fmt.Println("  --io-engine, -I         Write engine [auto|stdlib|io-uring]")
	fmt.Println("  --io-uring-queue-depth, -j  io-uring queue depth (0 = auto-tuned)")
	fmt.Println("  --hash-strategy, -S     Hash strategy [sync|async|hocho]")
	fmt.Println("  --hocho-mode, -M        Hocho mode [baseline|reuse|postdedup] (only with --hash-strategy hocho)")
	fmt.Println("  Defaults are chosen by storage mode:")
	fmt.Println("    file mode: higher workers")
	fmt.Println("    container/hierarchical: lower workers + deeper queue")
	printExamples(
		"dues store E01-image.dd",
		"dues store E01-image.dd -w 24 -W 192",
		"dues store E01-image.dd -F -E -H",
		"dues store E01-image.dd --preset quick",
		"dues store E01-image.dd --preset low-resource --compress-level default",
		"dues store E01-image.dd --io-engine io-uring --io-uring-queue-depth 1024",
		"dues store evidence.raw --dbpath ./caseA",
		"dues store memory.dump --no-index --quick",
	)
}

func printEnrichHelp() {
	printHelpHeader("enrich")
	fmt.Println("Usage: dues enrich [global options]")
	fmt.Println("Backfills file-level hierarchy nodes into graphdb: evidence file -> partition -> indexed file.")
	printExamples(
		"dues enrich",
		"dues enrich --dbpath ./caseA",
	)
}

func printRestoreHelp() {
	printHelpHeader("restore")
	fmt.Println("Usage: dues restore HASH [--filepath|-f restored] [--restore-workers|-y N] [--restore-queue|-U N] [--restore-progress-ms|-m N] [--restore-buffer-mb|-b N] [global options]")
	fmt.Println("Restores a stored file by its hash.")
	printHelpSection("Most-Used Flags")
	fmt.Println("  -f, --filepath            Output path")
	fmt.Println("  -y, --restore-workers     Worker count")
	fmt.Println("  -U, --restore-queue       Queue depth")
	fmt.Println("  -m, --restore-progress-ms Progress update interval")
	fmt.Println("  -b, --restore-buffer-mb   Write buffer size in MB")
	printHelpSection("Restore Tuning")
	fmt.Println("  --restore-workers, -y N      Override worker count (0 = adaptive default)")
	fmt.Println("  --restore-queue, -U N        Override pipeline queue depth (0 = adaptive default)")
	fmt.Println("  --restore-progress-ms, -m N  Progress refresh interval in milliseconds")
	fmt.Println("  --restore-buffer-mb, -b N    Output write buffer size (MB, 0 = mode-aware default)")
	printExamples(
		"dues restore <hash>",
		"dues restore <hash> --filepath restored.img",
		"dues restore <hash> -y 8 -U 32 -m 100 -b 128",
		"dues restore <hash> --dbpath ./caseA",
	)
}

func printListHelp() {
	printHelpHeader("list")
	fmt.Println("Usage: dues list [--status|-u all|completed|pending|failed] [global options]")
	fmt.Println("Lists all saved files in the database.")
	printExamples(
		"dues list",
		"dues list --status completed",
		"dues list --dbpath ./caseA",
	)
}

func printStatsHelp() {
	printHelpHeader("stats")
	fmt.Println("Usage: dues stats [global options]")
	fmt.Println("Displays statistics about the DUES database.")
	fmt.Println()
	printHelpSection("Output includes")
	fmt.Println("  Total / completed evidence files")
	fmt.Println("  Total partitions and indexed files")
	fmt.Println("  Indexed deleted/fragmented stats (fragmented is 0 until fragmented ingestion is enabled)")
	fmt.Println("  Total logical size (sum of all stored file sizes)")
	fmt.Println("  Unique chunks vs total chunk references")
	fmt.Println("  Shared chunks (referenced by more than one file)")
	fmt.Println("  On-disk bytes and deduplication ratio")
	fmt.Println("  Requested/selected write engine and fallback count")
	fmt.Println("  io-uring queue depth, submit/wait/completion/error counters (when io-uring is selected)")
	printExamples(
		"dues stats",
		"dues stats --dbpath ./caseA",
	)
}

func printSearchHelp() {
	printHelpHeader("search")
	fmt.Println("Usage: dues search QUERY [--rank-alpha|-r N] [--fts|-F] [global options]")
	fmt.Println("Searches indexed data by query string.")
	fmt.Println("Use --fts to try sidecar full-text search first, then fallback to scan path.")
	printHelpSection("Most-Used Flags")
	fmt.Println("  -F, --fts         Prefer full-text sidecar index first")
	fmt.Println("  -r, --rank-alpha  Occurrence boost weight")
	printExamples(
		"dues search \"invoice\"",
		"dues search \"invoice\" --fts",
		"dues search \"user:alice\" -r 0.5",
		"dues search \"user:alice\" --dbpath ./caseA",
	)
}

func printMicroArtefactsHelp() {
	printHelpHeader("micro")
	fmt.Println("Usage: dues micro <extract|list> [options] [global options]")
	fmt.Println("Manage micro-artefacts extracted from indexed files in the database.")
	fmt.Println()
	printHelpSection("Most-Used Commands")
	fmt.Println("  dues micro extract -K 20")
	fmt.Println("  dues micro extract -V")
	fmt.Println("  dues micro list")
	fmt.Println()
	fmt.Println("  micro extract  — Extract micro-artefacts from all indexed files and populate graph database")
	fmt.Println("                  Options: --force|-V, --top-k|-K")
	fmt.Println("  micro list     — List all micro-artefacts in JSON format")
	fmt.Println()
	fmt.Println("Try: dues help micro extract")
	fmt.Println("or : dues help micro list")
	printExamples(
		"dues micro extract",
		"dues micro extract --top-k 20",
		"dues micro extract -V -K 5",
		"dues micro list",
	)
}

func printMicroExtractHelp() {
	printHelpHeader("micro extract")
	fmt.Println("Usage: dues micro extract [--force|-V] [--top-k|-K N] [global options]")
	fmt.Println("Extracts micro-artefacts from indexed files and writes them into graph storage.")
	printHelpSection("Options")
	fmt.Println("  --force, -V   Re-process files even when artefacts already exist")
	fmt.Println("  --top-k, -K   Limit exported HTML graph input to top-K files by artefact count (0 = all)")
	printExamples(
		"dues micro extract",
		"dues micro extract --top-k 10",
		"dues micro extract -V -K 5 --dbpath ./caseA",
	)
}

func printMicroListHelp() {
	printHelpHeader("micro list")
	fmt.Println("Usage: dues micro list [global options]")
	fmt.Println("Lists stored micro-artefacts in JSON format.")
	printExamples(
		"dues micro list",
		"dues micro list --dbpath ./caseA",
	)
}

func printNearHelp() {
	printHelpHeader("near")
	fmt.Println("Usage: dues near <in|out> ... [global options]")
	fmt.Println("Finds NeAR (Near Artefact Relation) file objects and generates a similarity report.")
	fmt.Println()
	printHelpSection("Most-Used Flags")
	fmt.Println("  -e, --deep           Enable phase-1 chunk-level drilldown")
	fmt.Println("  -a, --advanced-deep  Enable phase-2 full-file re-ranking")
	fmt.Println("  -k, --top-k          Candidate count for phase-2")
	fmt.Println("  -t, --explain-exact  Force explanation even on exact match (near out)")
	fmt.Println()
	fmt.Println("  near in   — query file is already stored in the DUES database (identified by hash)")
	fmt.Println("  near out  — query file is on disk outside the database (identified by path)")
	fmt.Println()
	fmt.Println("Both sub-commands produce a near_report.json with a ranked list of matching artefacts.")
	fmt.Println("Each match includes an overall_relatedness score that combines all active phases.")
	fmt.Println()
	fmt.Println("Try: dues help near in")
	fmt.Println("Try: dues help near out")
	printExamples(
		"dues near in <hash>",
		"dues near in <hash> --deep --advanced-deep",
		"dues near out ./suspect.bin",
		"dues near out ./suspect.bin --deep --advanced-deep",
	)
}

func printNearInHelp() {
	cmd := color.New(color.FgBlue, color.Bold).SprintFunc()
	printHelpHeader("near in")
	fmt.Println("Usage: dues near in HASH [--deep|-e] [--advanced-deep|-a] [--top-k|-k N] [global options]")
	fmt.Println()
	fmt.Println("Finds NeAR objects for a file already stored in the DUES database.")
	fmt.Println("Produces near_report.json with ranked matches and an overall_relatedness score per match.")

	printHelpSection("Options")
	fmt.Printf("  %s, %s   Phase 1: enable partial chunk SimHash matching (slower, finds more candidates)\n", cmd("--deep"), cmd("-e"))
	fmt.Printf("  %s, %s   Phase 2: full-file SimHash re-ranking of top-K candidates\n", cmd("--advanced-deep"), cmd("-a"))
	fmt.Printf("              Eliminates chunk-alignment noise from Phase 1 scores.\n")
	fmt.Printf("              Results are cached in the DB (F|||: namespace) — subsequent runs are O(1).\n")
	fmt.Printf("  %s, %s      Number of candidates for Phase 2 (default 0 = auto-select)\n", cmd("--top-k"), cmd("-k"))
	fmt.Printf("              Auto-selection scales with available memory × CPU threads, bounded [5, 100].\n")

	printHelpSection("Report Fields (per match)")
	fmt.Println("  overall_relatedness       Single score [0.0–1.0] combining all active phases")
	fmt.Println("  overall_relatedness_pct   Same value as a percentage")
	fmt.Println("  relatedness_basis         Which signals were used: exact | phase1 | phase1+phase2")
	fmt.Println("  phase1_deviation_estimate Fraction of chunk comparisons affected by edge-alignment noise")
	fmt.Println("  phase2_file_similarity    Full-file SimHash similarity (only present when --advanced-deep used)")

	printExamples(
		"dues near in <hash>",
		"dues near in <hash> --deep",
		"dues near in <hash> --deep --advanced-deep",
		"dues near in <hash> --deep --advanced-deep --top-k 20",
	)
}

func printNearOutHelp() {
	cmd := color.New(color.FgBlue, color.Bold).SprintFunc()
	printHelpHeader("near out")
	fmt.Println("Usage: dues near out FILE [--deep|-e] [--explain-exact|-t] [--advanced-deep|-a] [--top-k|-k N] [global options]")
	fmt.Println()
	fmt.Println("Finds NeAR objects for a file on disk that is outside the DUES database.")
	fmt.Println("Produces near_report.json with ranked matches and an overall_relatedness score per match.")

	printHelpSection("Options")
	fmt.Printf("  %s, %s   Phase 1: enable partial chunk SimHash matching (slower, finds more candidates)\n", cmd("--deep"), cmd("-e"))
	fmt.Printf("  %s, %s   Force chunk drilldown even when an exact file hash match exists\n", cmd("--explain-exact"), cmd("-t"))
	fmt.Printf("  %s, %s   Phase 2: full-file SimHash re-ranking of top-K candidates\n", cmd("--advanced-deep"), cmd("-a"))
	fmt.Printf("              Eliminates chunk-alignment noise from Phase 1 scores.\n")
	fmt.Printf("              Note: the query file is external, so its signature is computed fresh each run.\n")
	fmt.Printf("              Candidate signatures are cached in the DB and reused on subsequent runs.\n")
	fmt.Printf("  %s, %s      Number of candidates for Phase 2 (default 0 = auto-select)\n", cmd("--top-k"), cmd("-k"))
	fmt.Printf("              Auto-selection scales with available memory × CPU threads, bounded [5, 100].\n")

	printHelpSection("Report Fields (per match)")
	fmt.Println("  overall_relatedness       Single score [0.0–1.0] combining all active phases")
	fmt.Println("  overall_relatedness_pct   Same value as a percentage")
	fmt.Println("  relatedness_basis         Which signals were used: exact | phase1 | phase1+phase2")
	fmt.Println("  phase1_deviation_estimate Fraction of chunk comparisons affected by edge-alignment noise")
	fmt.Println("  phase2_file_similarity    Full-file SimHash similarity (only present when --advanced-deep used)")

	printExamples(
		"dues near out ./unknown.bin",
		"dues near out ./unknown.bin --deep",
		"dues near out ./unknown.bin --deep --advanced-deep",
		"dues near out ./unknown.bin --deep --advanced-deep --top-k 20",
		"dues near out ./unknown.bin --dbpath ./caseA",
	)
}

func printServerHelp() {
	printHelpHeader("server")
	fmt.Println("Usage: dues server [--hash-algo|-g sha3|blake3] [global options]")
	fmt.Println("Runs the combined gRPC/Web DUES server.")
	printExamples(
		"dues server",
		"dues server --hash-algo sha3",
		"dues server --dbpath ./caseA",
	)
}

func printResetHelp() {
	printHelpHeader("reset")
	fmt.Println("Usage: dues reset [global options]")
	fmt.Println("Deletes the database.")
	printExamples(
		"dues reset",
		"dues reset --dbpath ./caseA",
	)
}

func printTuiHelp() {
	printHelpHeader("tui")
	fmt.Println("Usage: dues tui [global options]")
	fmt.Println("Launches the interactive Terminal UI for DUES operations.")
	fmt.Println("Use the TUI to store, list, search, restore, run NeAR analysis, and reset the database.")
	printExamples(
		"dues tui",
		"dues tui --dbpath ./caseA",
		"dues tui --password secret123 --dbpath ./caseA",
		"dues tui --chonksize 512 --container",
	)

	printHelpSection("TUI Keys")
	fmt.Println("  Up/Down   Navigate menus")
	fmt.Println("  Enter     Select/confirm")
	fmt.Println("  Tab       Switch input fields")
	fmt.Println("  Esc       Return to previous screen")
	fmt.Println("  q         Quit (from main menu)")
	fmt.Println("  Ctrl+C    Force exit")
}

func printVersionHelp() {
	printHelpHeader("version")
	fmt.Println("Usage: dues version")
	fmt.Println("Shows version details, codename, quick feature overview, and preset guidance.")
	printExamples(
		"dues version",
		"dues -v",
	)
}

func printHelpHeader(name string) {
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	fmt.Println(header("\nCommand: " + name))
}

func printHelpSection(name string) {
	section := color.New(color.FgBlue, color.Bold).SprintFunc()
	fmt.Println(section("\n" + name))
}

func printExamples(examples ...string) {
	if len(examples) == 0 {
		return
	}

	label := color.New(color.FgBlue, color.Bold).SprintFunc()
	cmd := color.New(color.FgWhite).SprintFunc()
	fmt.Println(label("Examples:"))
	for _, ex := range examples {
		fmt.Println("  " + cmd(ex))
	}
}

func handle(err error) {
	if err != nil {
		fmt.Printf("\n\n %v \n\n", err)
		os.Exit(1)
	}
}
