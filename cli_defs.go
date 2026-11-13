package main

// The command-line definition: flags and commands.

import (
	"indicer/internal/ui"
	"indicer/lib/cnst"

	"github.com/alecthomas/kingpin/v2"
)

// Flags read before parsing too, by flagFromArgs and boolFlagFromArgs,
// because the log and the crash file exist before kingpin runs.
const (
	flagLogDir  = "log-dir"
	flagTempDir = "temp-dir"
	flagDebug   = "debug"
)

// flagTrace names the flag that traces a run.
const flagTrace = "trace"

// --graph-compact: whether a command that wrote the graph compacts it.
const (
	flagGraphCompact  = "graph-compact"
	graphCompactAuto  = "auto"
	graphCompactNever = "never"
)

// cmdReindex names the reindex command.
const cmdReindex = "reindex"

// The package command and its subcommands.
const (
	cmdPackage        = "package"
	cmdPackageExport  = "export"
	cmdPackageInspect = "inspect"
	cmdPackageVerify  = "verify"
	cmdPackageImport  = "import"
)

const (
	flagTrustedKey = "trusted-key"
	trustedKeyHelp = "Require the package to be signed by this public key (dues actor export-public writes one)"
)

// The graph command and its subcommands.
const (
	cmdGraph        = "graph"
	cmdGraphCompact = "compact"
	cmdGraphStatus  = "status"
)

// The actor command and its subcommands.
const (
	cmdActor             = "actor"
	cmdActorCreate       = "create"
	cmdActorList         = "list"
	cmdActorExportPublic = "export-public"
	cmdActorRotate       = "rotate"
	cmdActorRevoke       = "revoke"
)

// Who signs a writable command's graph commits, and the passphrase of that
// actor's key, which comes from a file or stdin only, as the password does.
const (
	flagActor                 = "actor"
	flagActorKeyPasswordFile  = "actor-key-password-file"
	flagActorKeyPasswordStdin = "actor-key-password-stdin"
)

// The hidden commands: not in help or completion, documented only as
// extras. They run under --no-fun too, because they are asked for by name;
// --no-fun only stops the surprises and mew's animation.
const (
	cmdCoffee  = "coffee"
	cmdFortune = "fortune"
	cmdMeow    = "mew"
)

type cliFlags struct {
	logDir            *string
	tempDir           *string
	debug             *bool
	trace             *string
	graphCompact      *string
	dbpath            *string
	pwd               *string
	pwdFile           *string
	pwdStdin          *bool
	actor             *string
	actorPwdFile      *string
	actorPwdStdin     *bool
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
	color             *string
	plain             *bool
	ci                *bool
	noFun             *bool
	theme             *string
	taskbar           *bool

	evipath             *string
	syncIndex           *bool
	noIndex             *bool
	requireVerified     *bool
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
	storeJobs           *int

	rpath             *string
	restoreForce      *bool
	restoreVerify     *string
	restoreVerifyEWF  *bool
	restoreWorkers    *int
	restoreQueue      *int
	restoreProgressMs *int
	restoreBufferMB   *int
	rhash             *string

	listStatus    *string
	repairFix     *bool
	repairUpg     *bool
	repairYes     *bool
	repairVfy     *bool
	repairReindex *bool
	repairRevrel  *bool
	resetYes      *bool

	reindexIDs    *[]string
	reindexFailed *bool
	reindexAll    *bool
	reindexSource *string
	reindexFTS    *bool
	reindexEnrich *bool
	reindexJobs   *int

	deep            *bool
	inVerify        *bool
	inTopK          *int
	inhash          *string
	outDeep         *bool
	outExplainExact *bool
	outVerify       *bool
	outTopK         *int
	outpath         *string
	inOut           *string
	inForce         *bool
	outOut          *string
	outForce        *bool

	query       *string
	rankAlpha   *float64
	fullText    *bool
	searchOut   *string
	searchForce *bool

	microExtractForce     *bool
	microExtractTopK      *int
	microExtractOut       *string
	microExtractOverwrite *bool

	serverHashAlgo    *string
	serverListen      *[]string
	serverToken       *string
	serverTokenFile   *string
	serverCORSOrigins *[]string
	serverUploadJobs  *int

	pkgOut            *string
	pkgSplitSizeMB    *int
	pkgForce          *bool
	pkgWithoutDerived *bool
	pkgHostInfo       *bool
	pkgInspectPath    *string
	pkgVerifyPath     *string
	pkgVerifyDeep     *bool
	pkgVerifyTrusted  *string
	pkgImportPath     *string
	pkgImportTrusted  *string

	graphRequireSigned *bool

	actorCreateName   *string
	actorExportName   *string
	actorExportOut    *string
	actorExportForce  *bool
	actorRotateName   *string
	actorRevokeKey    *string
	actorRevokeReason *string
}

type cliCommands struct {
	version      *kingpin.CmdClause
	config       *kingpin.CmdClause
	tui          *kingpin.CmdClause
	store        *kingpin.CmdClause
	restore      *kingpin.CmdClause
	list         *kingpin.CmdClause
	stats        *kingpin.CmdClause
	repair       *kingpin.CmdClause
	reindex      *kingpin.CmdClause
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
	pkg          *kingpin.CmdClause
	pkgExport    *kingpin.CmdClause
	pkgInspect   *kingpin.CmdClause
	pkgVerify    *kingpin.CmdClause
	pkgImport    *kingpin.CmdClause
	graph        *kingpin.CmdClause
	graphCompact *kingpin.CmdClause
	graphStatus  *kingpin.CmdClause
	actor        *kingpin.CmdClause
	actorCreate  *kingpin.CmdClause
	actorList    *kingpin.CmdClause
	actorExport  *kingpin.CmdClause
	actorRotate  *kingpin.CmdClause
	actorRevoke  *kingpin.CmdClause

	// The hidden extras; see docs/CLI_UX.md.
	coffee  *kingpin.CmdClause
	fortune *kingpin.CmdClause
	mew     *kingpin.CmdClause
}

type cliDefinition struct {
	app      *kingpin.Application
	flags    cliFlags
	commands cliCommands
	faults   faultFlags
}

// kingpin reads an argument "@FILE" as the arguments in FILE. That would
// store "@ev.bin" as whatever ev.bin lists, and be a second, hidden config
// file. Set once, before any parse: every parse reads it.
func init() { kingpin.EnableFileExpansion = false }

func newCLIDefinition() cliDefinition {
	app := kingpin.New("DUES", "Deduplicated Unified Evidence Store")
	// --version is DUES's own flag. kingpin's printed a block on stderr and,
	// with no command, went on to print its whole usage.
	app.Flag("version", "Show the version line").Short('v').PreAction(func(*kingpin.ParseContext) error {
		printIdentity()
		return errVersionShown
	}).Bool()
	app.HelpFlag.Short('h')

	flags := cliFlags{}
	// Read before parsing, by readConfig; kingpin only has to accept them.
	app.Flag(flagConfig, "Read settings from this config file (default: ~/.config/dues/dues.json, if it exists)").PlaceHolder("FILE").String()
	app.Flag(flagNoConfig, "Read no config file").Bool()
	flags.logDir = app.Flag(flagLogDir, "Directory for log and crash files (default: the per-user cache directory)").String()
	flags.tempDir = app.Flag(flagTempDir, "Directory for temporary files, such as decoded containers and read-only database copies (default: /tmp, or AppData\\Local\\Temp on Windows)").String()
	flags.debug = app.Flag(flagDebug, "Write debug entries to the log").Bool()
	// A diagnostic for one run: no config key, and not in the settings record.
	flags.trace = app.Flag(flagTrace, "Write a Go execution trace of the run to this new file, for go tool trace").PlaceHolder("FILE").String()
	flags.graphCompact = app.Flag(flagGraphCompact, "After a command that wrote the graph, compact it when its log has passed 1 GiB or 5 million records: auto or never").
		Default(graphCompactAuto).PlaceHolder("WHEN").Enum(graphCompactAuto, graphCompactNever)
	flags.dbpath = app.Flag(cnst.FlagDBPath, "Custom path for DUES database").Short(cnst.FlagDBPathShort).String()
	// --password and --token were removed: a secret on the command line is
	// visible to other users. They stay, hidden, so a script that still
	// passes one fails with what replaces it, not "unknown flag".
	flags.pwd = app.Flag(cnst.FlagPassword, "Removed: use --password-file or --password-stdin").Short(cnst.FlagPasswordShort).Hidden().String()
	flags.pwdFile = app.Flag(flagPasswordFile, "Read the database password from the first line of this file").String()
	flags.pwdStdin = app.Flag(flagPasswordStdin, "Read the database password from the first line of standard input").Bool()
	flags.actor = app.Flag(flagActor, "Sign the graph commits of a command that writes them as this actor, not the database's own").PlaceHolder("NAME").String()
	flags.actorPwdFile = app.Flag(flagActorKeyPasswordFile, "Read the passphrase of the actor's key from the first line of this file").PlaceHolder("FILE").String()
	flags.actorPwdStdin = app.Flag(flagActorKeyPasswordStdin, "Read the passphrase of the actor's key from the first line of standard input").Bool()
	flags.chonkSize = app.Flag(cnst.FlagChonkSize, "Custom chunk size(KB) to be used for dedup").Short(cnst.FlagChonkSizeShort).Default("256").Int()
	flags.preset = app.Flag(cnst.FlagPreset, "Apply convenience preset [quick|performance|low-resource] (explicit flags override preset)").Short(cnst.FlagPresetShort).Default("").String()
	flags.quickMode = app.Flag(cnst.FlagQuickMode, "Convenience preset: equivalent to --preset quick").Short(cnst.FlagQuickModeShort).Default("false").Bool()
	flags.performanceMode = app.Flag(cnst.FlagPerformanceMode, "Convenience preset: equivalent to --preset performance").Short(cnst.FlagPerformanceModeShort).Default("false").Bool()
	flags.lowResourceMode = app.Flag(cnst.FlagLowResourceMode, "Convenience preset: equivalent to --preset low-resource").Short(cnst.FlagLowResourceModeShort).Default("false").Bool()
	flags.memopt = app.Flag(cnst.FlagLowResource, "Low resource use mode, foregoes performance in favour of utilising less memory, cpu, and energy").Short(cnst.FlagLowResourceShort).Default("false").Bool()
	flags.quickOpt = app.Flag(cnst.FlagFastMode, "Quick mode: create the database without table compression, for throughput (encryption is unchanged)").Short(cnst.FlagFastModeShort).Default("false").Bool()
	flags.containerMode = app.Flag(cnst.FlagContainerMode, "Use container-based storage (packs multiple chunks into 1GB containers)").Short(cnst.FlagContainerModeShort).Default("false").Bool()
	flags.hierarchicalIndex = app.Flag(cnst.FlagHierarchicalIndex, "RETIRED: has no effect; chunk locations are always kept in the database").Short(cnst.FlagHierarchicalShort).Hidden().Default("false").Bool()
	flags.compressLevel = app.Flag(cnst.FlagCompressLevel, "Per-chunk zstd compression level [fast|default|best] (default: best)").Short(cnst.FlagCompressLevelShort).Default("best").String()
	// The presentation flags are read before parsing too, by ui.PreScan,
	// because help prints before kingpin runs; kingpin validates them.
	flags.color = app.Flag(ui.FlagColor, "Colour: auto (256 colours on a terminal), always, never, ansi (only the terminal theme's 16 colours) or truecolor").Default(string(ui.ColorAuto)).Enum(ui.ColorPolicies...)
	flags.plain = app.Flag(ui.FlagPlain, "Plain output: no colour, ASCII only, no animation (for screen readers, logs, the Windows console host)").Bool()
	flags.ci = app.Flag(ui.FlagCI, "Behave as if no terminal is attached: plain output, and prompts refuse").Bool()
	flags.noFun = app.Flag(ui.FlagNoFun, "No easter eggs").Bool()
	flags.theme = app.Flag(ui.FlagTheme, "Terminal background: auto, dark or light (auto: the neutral palette; the TUI asks the terminal)").Default(ui.ThemeNames[0]).Enum(ui.ThemeNames...)
	flags.taskbar = app.Flag(ui.FlagTaskbar, "Show progress on the terminal's tab and taskbar (Windows Terminal, ConEmu)").Bool()

	commands := cliCommands{}
	commands.version = app.Command(cnst.CmdVeresion, "Show version details and quick feature overview")
	commands.config = app.Command(cmdConfig, "Show the settings in effect and where each came from, as JSON")
	commands.tui = app.Command(cnst.CmdTui, "Launch interactive TUI interface")

	commands.store = app.Command(cnst.CmdStore, "Store file in database")
	flags.evipath = commands.store.Arg(cnst.OperandFile, "Path of file that must be saved").Required().String()
	flags.syncIndex = commands.store.Flag(cnst.FlagSyncIndex, "RETIRED: has no effect; the index pass always runs beside storage").Short(cnst.FlagSyncIndexShort).Hidden().Default("false").Bool()
	flags.noIndex = commands.store.Flag(cnst.FlagNoIndex, "Don't run indexer").Short(cnst.FlagNoIndexShort).Default("false").Bool()
	flags.requireVerified = commands.store.Flag("require-verified", "Fail an E01 whose stored MD5/SHA-1 does not match the decoded image, instead of storing it flagged").Default("false").Bool()
	flags.enableFTS = commands.store.Flag(cnst.FlagEnableFts, "Enable full-text search indexing (sidecar Bleve index)").Short(cnst.FlagEnableFtsShort).Default("false").Bool()
	flags.enableEnrichment = commands.store.Flag(cnst.FlagEnableEnrichment, "Upsert disk/partition/indexed-file metadata nodes into graphdb during store").Short(cnst.FlagEnableEnrichmentShort).Default("false").Bool()
	flags.storeHashAlgo = commands.store.Flag(cnst.FlagHashAlgo, "Hashing algorithm to use [sha3|blake3] (default: BLAKE3)").Short(cnst.FlagHashAlgoShort).Default(cnst.BLAKE3).String()
	flags.storeHashStrategy = commands.store.Flag(cnst.FlagHashStrategy, "Store hash strategy for evidence [sync|async|hocho] (default: sync)").Short(cnst.FlagHashStrategyShort).Default(cnst.SyncHashStrategy).String()
	flags.storePipeline = commands.store.Flag(cnst.FlagStorePipeline, "Store ingest pipeline mode [batch-owner|staged] (default: batch-owner)").Short(cnst.FlagStorePipelineShort).Default(cnst.StorePipelineBatch).String()
	flags.storeAsyncFileWrite = commands.store.Flag(cnst.FlagStoreAsyncFileWrite, "DEPRECATED: has no effect; chunk blobs are always written durably").Short(cnst.FlagStoreAsyncFileWriteShort).Hidden().Default("false").Bool()
	flags.storeIOEngine = commands.store.Flag(cnst.FlagStoreIOEngine, "Store write engine [auto|stdlib|io-uring] (default: auto, which uses stdlib; io-uring must be asked for)").Short(cnst.FlagStoreIOEngineShort).Default(cnst.StoreIOEngineAuto).String()
	flags.storeIOUringQueue = commands.store.Flag(cnst.FlagStoreIOUringQueueDepth, "io-uring SQ/CQ depth (0 = auto-tuned)").Short(cnst.FlagStoreIOUringQueueDepthShort).Default("0").Int()
	flags.hochoMode = commands.store.Flag(cnst.FlagHochoMode, "RETIRED: has no effect; partition and indexed-file hashes are always plain hashes").Short(cnst.FlagHochoModeShort).Hidden().Default(cnst.HochoModeBaseline).String()
	flags.enableSimhash = commands.store.Flag(cnst.FlagSimhash, "Compute and store per-chunk simhash signatures during ingest (enables NeAR chunk-level similarity; off by default)").Short(cnst.FlagSimhashShort).Default("false").Bool()
	flags.enableRevRel = commands.store.Flag(cnst.FlagEnableRevRel, "Persist reverse-relation keys during ingest (off by default for higher ingest throughput)").Short(cnst.FlagEnableRevRelShort).Default("false").Bool()
	flags.storeWorkers = commands.store.Flag(cnst.FlagStoreWorkers, "Override batch-owner worker count (0 = mode-aware default)").Short(cnst.FlagStoreWorkersShort).Default("0").Int()
	flags.storeQueue = commands.store.Flag(cnst.FlagStoreQueue, "Override batch-owner task queue depth (0 = mode-aware default)").Short(cnst.FlagStoreQueueShort).Default("0").Int()
	flags.storeJobs = commands.store.Flag(cnst.FlagStoreJobs, "Evidence of a folder stored at once (default 1; 0 = chosen for this machine)").Default("1").Int()

	commands.restore = app.Command(cnst.CmdRestore, "Restore file from database")
	flags.rpath = commands.restore.Flag(cnst.FlagRestoreFilePath, "Path for restoring the file").Short(cnst.FlagRestoreFilePathShort).Default("restored").String()
	flags.restoreForce = commands.restore.Flag("force", "Replace the restore path if it already exists").Default("false").Bool()
	flags.restoreVerify = commands.restore.Flag("verify", "What to check while restoring: full (every chunk and the whole content's recorded hash), chunks (every chunk), or off (v2 chunks are still checked as they are decoded)").Default("full").Enum("full", "chunks", "off")
	flags.restoreVerifyEWF = commands.restore.Flag("verify-ewf", "Also check a restored E01 evidence against the MD5 and SHA-1 its set recorded at acquisition").Default("false").Bool()
	flags.restoreWorkers = commands.restore.Flag(cnst.FlagRestoreWorkers, "Override restore worker count (0 = adaptive default)").Short(cnst.FlagRestoreWorkersShort).Default("0").Int()
	flags.restoreQueue = commands.restore.Flag(cnst.FlagRestoreQueue, "Override restore pipeline queue depth (0 = adaptive default)").Short(cnst.FlagRestoreQueueShort).Default("0").Int()
	flags.restoreProgressMs = commands.restore.Flag(cnst.FlagRestoreProgressMs, "RETIRED: has no effect; the progress display samples on its own").Short(cnst.FlagRestoreProgressMsShort).Hidden().Default("0").Int()
	flags.restoreBufferMB = commands.restore.Flag(cnst.FlagRestoreBufferMB, "Restore output buffer size in MB (0 = mode-aware default: low=32, high=256)").Short(cnst.FlagRestoreBufferMBShort).Default("0").Int()
	flags.rhash = commands.restore.Arg(cnst.OperandHash, "Hash or ID of the object to restore, as list shows it").String()

	commands.list = app.Command(cnst.CmdList, "List all the saved files in the database")
	flags.listStatus = commands.list.Flag(cnst.FlagListStatus, "Filter evidence by status [all|completed|pending|failed]").Short(cnst.FlagListStatusShort).Default("all").String()
	commands.stats = app.Command(cnst.CmdStats, "Show database statistics")
	commands.repair = app.Command(cnst.CmdRepair, "Inspect and optionally finish what interrupted ingests left")
	flags.repairFix = commands.repair.Flag(cnst.FlagRepairFix, "Finish what interrupted ingests left: fail unfinished attempts (storing the file again resumes them), commit flushed ones, remove duplicates, mark interrupted index passes").Short(cnst.FlagRepairFixShort).Default("false").Bool()
	flags.repairReindex = commands.repair.Flag("reindex-failed", "With --fix, rebuild every index whose pass failed or was interrupted").Default("false").Bool()
	flags.repairRevrel = commands.repair.Flag("revrel", "With --fix, add reverse relations (which near needs) to every stored evidence stored without them").Default("false").Bool()
	flags.repairUpg = commands.repair.Flag(cnst.FlagRepairUpgrade, "Move a legacy hierarchical block index into the database").Default("false").Bool()
	flags.repairYes = commands.repair.Flag("yes", "With --upgrade, delete the moved block index files without asking").Default("false").Bool()
	flags.repairVfy = commands.repair.Flag(cnst.FlagRepairVerify, "Read back every stored chunk and check it against its hash").Default("false").Bool()

	commands.reindex = app.Command(cmdReindex, "Rebuild the index of stored evidence from its stored chunks")
	flags.reindexIDs = commands.reindex.Arg("id", "Evidence ID or hash, as list shows it").Strings()
	flags.reindexFailed = commands.reindex.Flag("failed", "Every evidence whose index pass failed or was interrupted").Default("false").Bool()
	flags.reindexAll = commands.reindex.Flag("all", "Every stored evidence").Default("false").Bool()
	flags.reindexSource = commands.reindex.Flag("source", "Read the evidence from this file instead of the stored chunks (one evidence; its content must match)").String()
	flags.reindexFTS = commands.reindex.Flag(cnst.FlagEnableFts, "Also build the full-text index").Default("false").Bool()
	flags.reindexEnrich = commands.reindex.Flag(cnst.FlagEnableEnrichment, "Also upsert the graph nodes").Default("false").Bool()
	flags.reindexJobs = commands.reindex.Flag(cnst.FlagStoreJobs, "Evidence reindexed at once (default 1; 0 = chosen for this machine)").Default("1").Int()

	commands.near = app.Command(cnst.CmdNear, "Get NeAR file objects")
	commands.nearIn = commands.near.Command(cnst.SubCmdIn, "Finds NeAR objects & generates GReAt graph for file INside of the database")
	flags.deep = commands.nearIn.Flag(cnst.FlagDeep, "Enable/Disable partial chunk match").Short(cnst.FlagDeepShort).Default("false").Bool()
	flags.inVerify = commands.nearIn.Flag(cnst.FlagAdvancedDeep, "Phase 2: full-file SimHash re-ranking of top K candidates").Short(cnst.FlagAdvancedDeepShort).Default("false").Bool()
	flags.inTopK = commands.nearIn.Flag(cnst.FlagTopK, "Top K candidates for Phase 2 re-ranking (0 = auto-select based on available resources)").Short(cnst.FlagTopKShort).Default("0").Int()
	flags.inhash = commands.nearIn.Arg(cnst.OperandHash, "Hash of the file in DUES DB for which you need to run NeAR").String()
	flags.inOut = commands.nearIn.Flag("out", "Write the report to this path").Default("near_report.json").String()
	flags.inForce = commands.nearIn.Flag("force", "Replace the report path if it already exists").Default("false").Bool()

	commands.nearOut = commands.near.Command(cnst.SubCmdOut, "Finds NeAR objects & generates GReAt graph for file OUTside of the database")
	flags.outDeep = commands.nearOut.Flag(cnst.FlagDeep, "Enable/Disable partial chunk match").Short(cnst.FlagDeepShort).Default("false").Bool()
	flags.outExplainExact = commands.nearOut.Flag(cnst.FlagExplainExact, "Force chunk-level drilldown even when an exact file hash match exists").Short(cnst.FlagExplainExactShort).Default("false").Bool()
	flags.outVerify = commands.nearOut.Flag(cnst.FlagAdvancedDeep, "Phase 2: full-file SimHash re-ranking of top K candidates").Short(cnst.FlagAdvancedDeepShort).Default("false").Bool()
	flags.outTopK = commands.nearOut.Flag(cnst.FlagTopK, "Top K candidates for Phase 2 re-ranking (0 = auto-select based on available resources)").Short(cnst.FlagTopKShort).Default("0").Int()
	flags.outpath = commands.nearOut.Arg(cnst.OperandFile, "Path to the file for which you need to run NeAR").String()
	flags.outOut = commands.nearOut.Flag("out", "Write the report to this path").Default("near_report.json").String()
	flags.outForce = commands.nearOut.Flag("force", "Replace the report path if it already exists").Default("false").Bool()

	commands.search = app.Command(cnst.CmdSearch, "Search anything in DUES DB")
	flags.query = commands.search.Arg(cnst.OperandQuery, "Search query string").String()
	flags.rankAlpha = commands.search.Flag(cnst.FlagRankAlpha, "Occurrence boost weight for ranking (>= 0, default: 0.35)").Short(cnst.FlagRankAlphaShort).Default("0.35").Float64()
	flags.fullText = commands.search.Flag(cnst.FlagEnableFts, "Search only the indexed files the full-text index offers, falling back to a full search when it offers none").Short(cnst.FlagEnableFtsShort).Default("false").Bool()
	flags.searchOut = commands.search.Flag("out", "Write the report to this path").Default("report.json").String()
	flags.searchForce = commands.search.Flag("force", "Replace the report path if it already exists").Default("false").Bool()

	commands.micro = app.Command(cnst.CmdMicro, "Manage micro-artefacts")
	commands.microExtract = commands.micro.Command("extract", "Extract micro-artefacts from all indexed files and populate graph database")
	flags.microExtractForce = commands.microExtract.Flag("force", "Extract again from indexed files an earlier run examined, replacing their artefacts").Short(cnst.FlagMicroExtractForceShort).Default("false").Bool()
	flags.microExtractTopK = commands.microExtract.Flag("top-k", "Top-K indexed files (by artefact count) to include in exported HTML graph (0 = all)").Short(cnst.FlagMicroExtractTopKShort).Default("1").Int()
	flags.microExtractOut = commands.microExtract.Flag("out", "Write the HTML graph view to this path").Default("graph.html").String()
	flags.microExtractOverwrite = commands.microExtract.Flag("overwrite", "Replace the HTML graph view if it already exists").Default("false").Bool()
	commands.microList = commands.micro.Command(cnst.CmdList, "List all micro-artefacts in JSON format")

	commands.enrich = app.Command(cnst.CmdEnrich, "Backfill file hierarchy metadata enrichment into graphdb")

	commands.server = app.Command(cnst.CmdServer, "Run gRPC / Web combined DUES server")
	flags.serverHashAlgo = commands.server.Flag(cnst.FlagHashAlgo, "Hashing algorithm to use [sha3|blake3] (default: BLAKE3)").Short(cnst.FlagHashAlgoShort).Default(cnst.BLAKE3).String()
	flags.serverListen = commands.server.Flag(flagListen, "Serve on this host:port (repeatable; default 127.0.0.1:50051 and [::1]:50051). A non-loopback address needs a token").Strings()
	flags.serverTokenFile = commands.server.Flag(flagTokenFile, "Require the bearer token on the first line of this file").String()
	flags.serverToken = commands.server.Flag(flagToken, "Removed: use --token-file").Hidden().String()
	flags.serverCORSOrigins = commands.server.Flag(flagCORSOrigin, "Also let pages from this origin call the server, as scheme://host[:port] (repeatable)").Strings()
	flags.serverUploadJobs = commands.server.Flag(flagUploadJobs, "Uploads stored at once (default 1; 0 = chosen for this machine)").Default("1").Int()

	commands.pkg = app.Command(cmdPackage, "Export the database as a package; inspect, verify or import one")
	commands.pkgExport = commands.pkg.Command(cmdPackageExport, "Copy the database, as it is on disk, into a .duesarc package")
	flags.pkgOut = commands.pkgExport.Flag("out", "Write the package to this path (default: dues-<database>-<UTC time>.duesarc here)").String()
	flags.pkgSplitSizeMB = commands.pkgExport.Flag("split-size-mb", "Split the package into parts of this many MiB, described by a .split.json sidecar (0 = one file)").Default("0").Int()
	flags.pkgForce = commands.pkgExport.Flag("force", "Replace the package if it already exists").Default("false").Bool()
	flags.pkgWithoutDerived = commands.pkgExport.Flag("without-derived", "Leave out the full-text index and the graph, which reindex, enrich and micro extract rebuild").Default("false").Bool()
	flags.pkgHostInfo = commands.pkgExport.Flag("host-info", "Record this machine's OS, architecture and name in the package").Default("false").Bool()
	commands.pkgInspect = commands.pkg.Command(cmdPackageInspect, "Describe a package from its manifest")
	flags.pkgInspectPath = commands.pkgInspect.Arg("package", "The package, or a part or the sidecar of a split one").Required().String()
	commands.pkgVerify = commands.pkg.Command(cmdPackageVerify, "Check every file of a package against its manifest")
	flags.pkgVerifyPath = commands.pkgVerify.Arg("package", "The package, or a part or the sidecar of a split one").Required().String()
	flags.pkgVerifyDeep = commands.pkgVerify.Flag("deep", "Also copy the database into --temp-dir, open it read-only and read every evidence back against its chunk hashes").Default("false").Bool()
	flags.pkgVerifyTrusted = commands.pkgVerify.Flag(flagTrustedKey, trustedKeyHelp).PlaceHolder("FILE").String()
	commands.pkgImport = commands.pkg.Command(cmdPackageImport, "Make a new database, at --dbpath, from a package")
	flags.pkgImportPath = commands.pkgImport.Arg("package", "The package, or a part or the sidecar of a split one").Required().String()
	flags.pkgImportTrusted = commands.pkgImport.Flag(flagTrustedKey, trustedKeyHelp).PlaceHolder("FILE").String()

	commands.graph = app.Command(cmdGraph, "Compact the graph store, or describe it")
	commands.graphCompact = commands.graph.Command(cmdGraphCompact, "Fold the graph's log into its image, keeping the log as a segment")
	flags.graphRequireSigned = commands.graphCompact.Flag("require-signed", "From now on, refuse any commit that is not signed by a key of the keyring").Default("false").Bool()
	commands.graphStatus = commands.graph.Command(cmdGraphStatus, "Describe the graph store from its files: image, log, segments")

	commands.actor = app.Command(cmdActor, "Manage the actors that sign graph commits and packages")
	commands.actorCreate = commands.actor.Command(cmdActorCreate, "Make an actor, with a key its passphrase protects")
	flags.actorCreateName = commands.actorCreate.Arg("name", "The actor's name").Required().String()
	commands.actorList = commands.actor.Command(cmdActorList, "List every key of the keyring")
	commands.actorExport = commands.actor.Command(cmdActorExportPublic, "Write an actor's current public key, to keep outside the data root")
	flags.actorExportName = commands.actorExport.Arg("name", "The actor's name").Required().String()
	flags.actorExportOut = commands.actorExport.Flag("out", "Write the key to this path (default: standard output)").String()
	flags.actorExportForce = commands.actorExport.Flag("force", "Replace the path if it already exists").Default("false").Bool()
	commands.actorRotate = commands.actor.Command(cmdActorRotate, "Give an actor a new key, keeping the old one for what it signed")
	flags.actorRotateName = commands.actorRotate.Arg("name", "The actor's name").Required().String()
	commands.actorRevoke = commands.actor.Command(cmdActorRevoke, "Stop a key from signing; what it signed still verifies")
	flags.actorRevokeKey = commands.actorRevoke.Arg("key-id", "The key's ID, as dues actor list shows it").Required().String()
	flags.actorRevokeReason = commands.actorRevoke.Flag("reason", "Why the key is revoked, kept in the keyring").String()

	commands.reset = app.Command(cnst.CmdReset, "Delete the database").Alias(cnst.CmdPurge).Alias(cnst.CmdDelete).Alias(cnst.CmdDestroy)
	commands.coffee = app.Command(cmdCoffee, "").Hidden()
	commands.fortune = app.Command(cmdFortune, "").Hidden()
	commands.mew = app.Command(cmdMeow, "").Hidden()
	flags.resetYes = commands.reset.Flag("yes", "Delete without asking for the folder name (required when stdin is not a terminal)").Default("false").Bool()

	return cliDefinition{app: app, flags: flags, commands: commands, faults: addFaultFlags(app)}
}
