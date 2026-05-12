package main

import (
	"fmt"
	"indicer/api"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/fatih/color"
	"github.com/klauspost/compress/zstd"
)

const (
	duesVersion  = "0.38"
	duesCodename = "<jackfruit> spacebar"
)

func init() {
	var err error

	cnst.DECODER, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	handle(err)

	cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
	handle(err)
}

func main() {
	app := kingpin.New("DUES", "Deduplicated Unified Evidence Store")
	app.Version(getVersionText())
	app.VersionFlag.Short('v')
	app.HelpFlag.Short('h')
	dbpath := app.Flag(cnst.FlagDBPath, "Custom path for DUES database").Short(cnst.FlagDBPathShort).String()
	pwd := app.Flag(cnst.FlagPassword, "Password for the DUES database").Short(cnst.FlagPasswordShort).String()
	chonkSize := app.Flag(cnst.FlagChonkSize, "Custom chunk size(KB) to be used for dedup").Short(cnst.FlagChonkSizeShort).Default("256").Int()
	memopt := app.Flag(cnst.FlagLowResource, "Low resource use mode, foregoes performance in favour of utilising less memory, cpu, and energy").Short(cnst.FlagLowResourceShort).Default("false").Bool()
	QUICKOPT := app.Flag(cnst.FlagFastMode, "Quick mode, forgoes encryption, intra-chunk & overall db compression in favour of higher throughput").Short(cnst.FlagFastModeShort).Default("false").Bool()
	containerMode := app.Flag(cnst.FlagContainerMode, "Use container-based storage (packs multiple chunks into 1GB containers)").Short(cnst.FlagContainerModeShort).Default("false").Bool()
	hierarchicalIndex := app.Flag(cnst.FlagHierarchicalIndex, "Use hierarchical block index (groups 1000 chunks per block, requires container mode)").Short(cnst.FlagHierarchicalShort).Default("false").Bool()
	cmdversion := app.Command(cnst.CmdVeresion, "Show version details and quick feature overview")

	cmdtui := app.Command(cnst.CmdTui, "Launch interactive TUI interface")

	cmdstore := app.Command(cnst.CmdStore, "Store file in database")
	evipath := cmdstore.Arg(cnst.OperandFile, "Path of file that must be saved").Required().String()
	noIndex := cmdstore.Flag(cnst.FlagNoIndex, "Don't run indexer").Short(cnst.FlagNoIndexShort).Default("false").Bool()
	hashAlgo := cmdstore.Flag(cnst.FlagHashAlgo, "Hashing algorithm to use [sha3|blake3] (default: BLAKE3)").Short(cnst.FlagHashAlgoShort).Default("blake3").String()

	cmdrestore := app.Command(cnst.CmdRestore, "Restore file from database")
	rpath := cmdrestore.Flag(cnst.FlagRestoreFilePath, "Path for restoring the file").Short(cnst.FlagRestoreFilePathShort).Default("restored").String()
	rhash := cmdrestore.Arg(cnst.OperandHash, "Hash of file that must be restoed").String()

	cmdlist := app.Command(cnst.CmdList, "List all the saved files in the database")
	cmdstats := app.Command(cnst.CmdStats, "Show database statistics")

	cmdnear := app.Command(cnst.CmdNear, "Get NeAR file objects")
	cmdin := cmdnear.Command(cnst.SubCmdIn, "Finds NeAR objects & generates GReAt graph for file INside of the database")
	deep := cmdin.Flag(cnst.FlagDeep, "Enable/Disable partial chunk match").Short(cnst.FlagDeepShort).Default("false").Bool()
	inVerify := cmdin.Flag(cnst.FlagAdvancedDeep, "Phase 2: full-file SimHash re-ranking of top K candidates").Short(cnst.FlagAdvancedDeepShort).Default("false").Bool()
	inTopK := cmdin.Flag(cnst.FlagTopK, "Top K candidates for Phase 2 re-ranking (0 = auto-select based on available resources)").Short(cnst.FlagTopKShort).Default("0").Int()
	inhash := cmdin.Arg(cnst.OperandHash, "Hash of the file in DUES DB for which you need to run NeAR").String()

	cmdout := cmdnear.Command(cnst.SubCmdOut, "Finds NeAR objects & generates GReAt graph for file OUTside of the database")
	outDeep := cmdout.Flag(cnst.FlagDeep, "Enable/Disable partial chunk match").Short(cnst.FlagDeepShort).Default("false").Bool()
	outExplainExact := cmdout.Flag(cnst.FlagExplainExact, "Force chunk-level drilldown even when an exact file hash match exists").Short(cnst.FlagExplainExactShort).Default("false").Bool()
	outVerify := cmdout.Flag(cnst.FlagAdvancedDeep, "Phase 2: full-file SimHash re-ranking of top K candidates").Short(cnst.FlagAdvancedDeepShort).Default("false").Bool()
	outTopK := cmdout.Flag(cnst.FlagTopK, "Top K candidates for Phase 2 re-ranking (0 = auto-select based on available resources)").Short(cnst.FlagTopKShort).Default("0").Int()
	outpath := cmdout.Arg(cnst.OperandFile, "Path to the file for which you need to run NeAR").String()

	cmdsearch := app.Command(cnst.CmdSearch, "Search anything in DUES DB")
	query := cmdsearch.Arg(cnst.OperandQuery, "Search query string").String()
	rankAlpha := cmdsearch.Flag("rank-alpha", "Occurrence boost weight for ranking (>= 0, default: 0.35)").Default("0.35").Float64()

	cmdmicro := app.Command(cnst.CmdMicro, "Manage micro-artefacts")
	microExtract := cmdmicro.Command("extract", "Extract micro-artefacts from all indexed files and populate graph database")
	microExtractForce := microExtract.Flag("force", "Re-process indexed files even when micro-artefacts already exist in graph").Default("false").Bool()
	microExtractTopK := microExtract.Flag("top-k", "Top-K indexed files (by artefact count) to include in exported HTML graph (0 = all)").Default("1").Int()

	microList := cmdmicro.Command(cnst.CmdList, "List all micro-artefacts in JSON format")

	cmdserver := app.Command(cnst.CmdServer, "Run gRPC / Web combined DUES server")
	hashAlgo = cmdserver.Flag(cnst.FlagHashAlgo, "Hashing algorithm to use [sha3|blake3] (default: BLAKE3)").Short(cnst.FlagHashAlgoShort).Default("blake3").String()

	cmdreset := app.Command(cnst.CmdReset, "Delete the database").Alias(cnst.CmdPurge).Alias(cnst.CmdDelete).Alias(cnst.CmdDestroy)

	var err error

	if maybeHandleHelp(os.Args[1:]) {
		err = cnst.ENCODER.Close()
		handle(err)
		cnst.DECODER.Close()
		return
	}

	parsed := kingpin.MustParse(app.Parse(os.Args[1:]))
	cnst.MEMOPT = *memopt
	cnst.QUICKOPT = *QUICKOPT
	cnst.CONTAINERMODE = *containerMode
	cnst.HIERARCHICALINDEX = *hierarchicalIndex
	cnst.HASHALGO = strings.ToUpper(*hashAlgo)
	if cnst.HASHALGO == "" {
		cnst.HASHALGO = cnst.BLAKE3
	}

	// Hierarchical index requires container mode
	if cnst.HIERARCHICALINDEX && !cnst.CONTAINERMODE {
		color.Red("⚠️  Hierarchical index requires container mode. Enabling container mode automatically.")
		cnst.CONTAINERMODE = true
	}

	key := util.HashPassword(*pwd)

	// Skip banner for TUI mode
	if parsed != cmdtui.FullCommand() && parsed != cmdversion.FullCommand() {
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
	}

	switch parsed {
	case cmdversion.FullCommand():
		printVersionInfo()
	case cmdtui.FullCommand():
		err = cli.TUICmd(*chonkSize, *dbpath, key)
	case cmdstore.FullCommand():
		err = cli.StoreData(*chonkSize, *dbpath, *evipath, key, *noIndex)
	case cmdrestore.FullCommand():
		err = cli.RestoreData(*chonkSize, *dbpath, *rhash, *rpath, key)
	case cmdlist.FullCommand():
		err = cli.ListData(*chonkSize, *dbpath, key)
	case cmdstats.FullCommand():
		err = cli.StatsData(*chonkSize, *dbpath, key)
	case cmdin.FullCommand():
		err = cli.NearInData(*deep, *inVerify, *inTopK, *chonkSize, *dbpath, *inhash, key)
	case cmdout.FullCommand():
		err = cli.NearOutData(*outDeep, *outExplainExact, *outVerify, *outTopK, *chonkSize, *dbpath, *outpath, key)
	case cmdsearch.FullCommand():
		err = cli.SearchCmd(*chonkSize, *query, *dbpath, key, *rankAlpha)
	case microExtract.FullCommand():
		err = cli.MicroArtefactCmd(*chonkSize, *dbpath, key, *microExtractForce, *microExtractTopK)
	case microList.FullCommand():
		err = cli.ListMicroArtefactsCmd(*chonkSize, *dbpath, key)
	case cmdreset.FullCommand():
		err = cli.ResetData(*dbpath)
	case cmdserver.FullCommand():
		err = api.Server(*chonkSize, *dbpath, key)
	}

	handle(err)

	err = cnst.ENCODER.Close()
	handle(err)
	cnst.DECODER.Close()
}

func getVersionText() string {
	return fmt.Sprintf(
		"DUES v%s  (%s)\n"+
			"Deduplicated Unified Evidence Store\n\n"+
			"Highlights\n"+
			"  - Chunk-level deduplication with encrypted storage\n"+
			"  - Fast restore, NeAR similarity analysis, and search\n"+
			"  - Optional container + hierarchical index for scale\n\n"+
			"For more info: please run dues version\n",
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

	printHelpSection("Quick Start")
	fmt.Println("  dues store FILE")
	fmt.Println("  dues list")
	fmt.Println("  dues restore HASH")
	fmt.Println("  dues search QUERY")

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
	case cnst.CmdMicro:
		printMicroArtefactsHelp()
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

	printHelpSection("Commands")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdTui), "Launch interactive TUI interface")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdStore), "Store file in database")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdRestore), "Restore file from database")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdList), "List saved files")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdSearch), "Search metadata/content index")
	fmt.Printf("  %-20s %s\n", cmd(cnst.CmdMicro), "Manage micro-artefacts (extract, list)")
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
	fmt.Println("  dues restore <hash> --filepath recovered.bin")
	fmt.Println("  dues search \"invoice\"")
	fmt.Println("  dues microartefacts")
	fmt.Println("  dues near in <hash> --deep")
	fmt.Println("")
}

func printStoreHelp() {
	printHelpHeader("store")
	fmt.Println("Usage: dues store FILE [--sync|-s] [--no-index|-n] [--hash-algo|-g [sha3|blake3]] [global options]")
	fmt.Println("Stores a file in the DUES database using chunk-level deduplication.")
	printExamples(
		"dues store E01-image.dd",
		"dues store evidence.raw --dbpath ./caseA",
		"dues store memory.dump --no-index --quick",
	)
}

func printRestoreHelp() {
	printHelpHeader("restore")
	fmt.Println("Usage: dues restore HASH [--filepath|-f restored] [global options]")
	fmt.Println("Restores a stored file by its hash.")
	printExamples(
		"dues restore <hash>",
		"dues restore <hash> --filepath restored.img",
		"dues restore <hash> --dbpath ./caseA",
	)
}

func printListHelp() {
	printHelpHeader("list")
	fmt.Println("Usage: dues list [global options]")
	fmt.Println("Lists all saved files in the database.")
	printExamples(
		"dues list",
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
	printExamples(
		"dues stats",
		"dues stats --dbpath ./caseA",
	)
}

func printSearchHelp() {
	printHelpHeader("search")
	fmt.Println("Usage: dues search QUERY [global options]")
	fmt.Println("Searches indexed data by query string.")
	printExamples(
		"dues search \"invoice\"",
		"dues search \"user:alice\" --dbpath ./caseA",
	)
}

func printMicroArtefactsHelp() {
	printHelpHeader("micro")
	fmt.Println("Usage: dues micro <extract|list> [options] [global options]")
	fmt.Println("Manage micro-artefacts extracted from indexed files in the database.")
	fmt.Println()
	fmt.Println("  micro extract  — Extract micro-artefacts from all indexed files and populate graph database")
	fmt.Println("  micro list     — List all micro-artefacts in JSON format")
	fmt.Println()
	fmt.Println("Try: dues help micro extract")
	fmt.Println("or : dues help micro list")
}

func printNearHelp() {
	printHelpHeader("near")
	fmt.Println("Usage: dues near <in|out> ... [global options]")
	fmt.Println("Finds NeAR (Near Artefact Relation) file objects and generates a similarity report.")
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
	fmt.Println("Usage: dues server [global options]")
	fmt.Println("Runs the combined gRPC/Web DUES server.")
	printExamples(
		"dues server",
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
	fmt.Println("Shows version details, codename, and quick feature overview.")
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
