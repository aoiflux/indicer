package main

// Help and version text.

import (
	"fmt"
	"indicer/lib/cnst"
	"strings"

	"github.com/fatih/color"
)

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
	fmt.Printf("  %s       Directory for log and crash files\n", cmd("--log-dir"))
	fmt.Printf("  %s, %s   Password for the DUES database (visible to other users)\n", cmd("--password"), cmd("-p"))
	fmt.Printf("  %s  Read the password from a file (preferred; or set DUES_PASSWORD)\n", cmd("--password-file"))
	fmt.Printf("  %s Read the password from standard input\n", cmd("--password-stdin"))
	fmt.Printf("  %s, %s   Custom chunk size (KB), default 256\n", cmd("--chonksize"), cmd("-c"))
	fmt.Printf("  %s, %s   Low resource mode\n", cmd("--low"), cmd("-l"))
	fmt.Printf("  %s, %s   Quick mode (less protection, more throughput)\n", cmd("--quick"), cmd("-q"))
	fmt.Printf("  %s, %s   Container mode\n", cmd("--container"), cmd("-x"))
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
	fmt.Println("Usage: dues restore HASH [--filepath|-f restored] [--force] [--verify full|chunks|off] [--restore-workers|-y N] [--restore-queue|-U N] [--restore-progress-ms|-m N] [--restore-buffer-mb|-b N] [global options]")
	fmt.Println("Restores a stored file by its hash.")
	printHelpSection("Most-Used Flags")
	fmt.Println("  -f, --filepath            Output path")
	fmt.Println("      --force               Replace the output path if it exists")
	fmt.Println("      --verify              full (default): every chunk and the content's recorded hash;")
	fmt.Println("                            chunks: every chunk only; off: no checks")
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
		"dues restore <hash> --filepath restored.img --force --verify chunks",
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
	fmt.Println("Usage: dues reset [--yes] [global options]")
	fmt.Println("Deletes the DUES database in the data folder, and nothing else there.")
	fmt.Println("Asks you to type the folder name; --yes skips that, and is required when")
	fmt.Println("stdin is not a terminal. Refuses a folder that holds no DUES database, and")
	fmt.Println("your home folder, the working folder or a drive root.")
	printExamples(
		"dues reset",
		"dues reset --dbpath ./caseA",
		"dues reset --dbpath ./caseA --yes",
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
