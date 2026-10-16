package main

// The command-line definition: flags and commands.

import (
	"indicer/lib/cnst"

	"github.com/alecthomas/kingpin/v2"
)

// flagLogDir selects the log directory. It is read before parsing too, by
// logDirFromArgs.
const flagLogDir = "log-dir"

type cliFlags struct {
	logDir            *string
	dbpath            *string
	pwd               *string
	pwdFile           *string
	pwdStdin          *bool
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
	syncIndex           *bool
	noIndex             *bool
	enableFTS           *bool
	enableEnrichment    *bool
	storeHashAlgo       *string
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
}

type cliDefinition struct {
	app      *kingpin.Application
	flags    cliFlags
	commands cliCommands
}

func newCLIDefinition() cliDefinition {
	app := kingpin.New("DUES", "Deduplicated Unified Evidence Store")
	app.Version(getVersionText())
	app.VersionFlag.Short('v')
	app.HelpFlag.Short('h')

	flags := cliFlags{}
	flags.logDir = app.Flag(flagLogDir, "Directory for log and crash files (default: the per-user cache directory)").String()
	flags.dbpath = app.Flag(cnst.FlagDBPath, "Custom path for DUES database").Short(cnst.FlagDBPathShort).String()
	flags.pwd = app.Flag(cnst.FlagPassword, "Password for the DUES database (visible to other users; prefer --password-file, --password-stdin or DUES_PASSWORD)").Short(cnst.FlagPasswordShort).String()
	flags.pwdFile = app.Flag(flagPasswordFile, "Read the database password from the first line of this file").String()
	flags.pwdStdin = app.Flag(flagPasswordStdin, "Read the database password from the first line of standard input").Bool()
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

	return cliDefinition{app: app, flags: flags, commands: commands}
}
