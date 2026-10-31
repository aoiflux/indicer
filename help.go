package main

// Help and version text.

import (
	"fmt"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"os"
	"strings"
)

func getVersionText() string {
	commit := ""
	if buildCommit != "" {
		commit = "  commit " + buildCommit
	}
	return fmt.Sprintf(
		"DUES v%s  (%s)%s\n"+
			"Deduplicated Unified Evidence Store\n\n"+
			"Highlights\n"+
			"  - Chunk-level deduplication, encrypted when a password is given\n"+
			"  - Fast restore, NeAR similarity analysis, and search\n"+
			"  - Short flags + presets for faster daily usage\n\n"+
			"Try: dues --help\n"+
			"For detailed guide: dues version\n",
		duesVersion,
		duesCodename,
		commit,
	)
}

func printVersionInfo() {
	p := helpPainter()
	title := func(s string) string { return p.Strong(ui.Forensics, s) }
	accent := func(s string) string { return p.Strong(ui.Highlight, s) }
	muted := func(s string) string { return p.Paint(ui.Muted, s) }

	if p.Decorated() {
		fmt.Println()
		ui.Banner(os.Stdout, buildInfo())
	} else {
		fmt.Println(title("\nDUES"), muted("Deduplicated Unified Evidence Store"))
		fmt.Println(accent("Version:"), fmt.Sprintf("%s (%s)", duesVersion, duesCodename))
		if buildCommit != "" {
			fmt.Println(accent("Commit:"), buildCommit)
		}
		fmt.Println(muted("----------------------------------------"))
	}

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
	case cnst.CmdRepair:
		printRepairHelp()
	case cmdReindex:
		printReindexHelp()
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
	case cmdCoffee, cmdFortune, cmdMeow:
		fmt.Printf("dues %s is one of DUES's hidden extras; --no-fun turns them off.\n", path[0])
	default:
		fmt.Println(helpPainter().Strong(ui.Error, "Unknown command: "+strings.Join(path, " ")))
		fmt.Println("")
		printRootHelp()
	}
}

func printRootHelp() {
	p := helpPainter()
	title := func(s string) string { return p.Strong(ui.Forensics, s) }
	cmd := helpCmd()
	muted := func(s string) string { return p.Paint(ui.Muted, s) }

	if p.Decorated() {
		fmt.Println()
		ui.Banner(os.Stdout, buildInfo())
	} else {
		fmt.Println(title("\nDUES - Deduplicated Unified Evidence Store"))
	}
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
	fmt.Printf("  %s, %s   Chunk size (KB) of a new database, default 256\n", cmd("--chonksize"), cmd("-c"))
	fmt.Printf("  %s, %s   Low resource mode\n", cmd("--low"), cmd("-l"))
	fmt.Printf("  %s, %s   Quick mode: a new database without table compression (encryption unchanged)\n", cmd("--quick"), cmd("-q"))
	fmt.Printf("  %s, %s   Container storage for a new database\n", cmd("--container"), cmd("-x"))
	fmt.Printf("  %s, %s   Chunk compression level [fast|default|best]\n", cmd("--compress-level"), cmd("-z"))
	fmt.Printf("  %s, %s   Mode preset [quick|performance|low-resource]\n", cmd("--preset"), cmd("-P"))
	fmt.Printf("  %s, %s   Quick preset convenience alias\n", cmd("--quick-mode"), cmd("-Q"))
	fmt.Printf("  %s, %s   Performance preset convenience alias\n", cmd("--performance-mode"), cmd("-o"))
	fmt.Printf("  %s, %s   Low-resource preset convenience alias\n", cmd("--low-resource-mode"), cmd("-L"))
	fmt.Printf("  %s WHEN      Colour: auto, always, never, or ansi (only the terminal theme's 16 colours)\n", cmd("--color"))
	fmt.Printf("  %s           Plain output: no colour, ASCII only, no animation (screen readers, logs)\n", cmd("--plain"))
	fmt.Printf("  %s              Behave as if no terminal is attached: plain output, and prompts refuse\n", cmd("--ci"))
	fmt.Printf("  %s          No easter eggs\n", cmd("--no-fun"))

	printHelpSection("Output")
	fmt.Println("  Results go to stdout; the run header, progress, notes, warnings and errors go")
	fmt.Println("  to stderr, so `dues list | jq` reads pure JSON. On a pipe nothing is coloured.")
	fmt.Println("  Environment: NO_COLOR, FORCE_COLOR, DUES_COLOR, DUES_PLAIN=1, DUES_NO_FUN=1,")
	fmt.Println("  DUES_THEME=dark|light. CI services are detected (CI, GITHUB_ACTIONS, ...).")

	printHelpSection("Database Layout")
	fmt.Println("  Chunk size, hash algorithm, storage and encryption are fixed when a database is")
	fmt.Println("  created and recorded in its manifest. An existing database keeps its own; a flag")
	fmt.Println("  that contradicts it is an error (exit 2).")

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
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdTui, 20)), "Launch interactive TUI interface")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdStore, 20)), "Store file in database")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdRestore, 20)), "Restore file from database")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdList, 20)), "List saved files")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdSearch, 20)), "Search metadata/content index")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdMicro, 20)), "Manage micro-artefacts (extract, list)")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdEnrich, 20)), "Backfill file hierarchy metadata into graphdb")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdNear, 20)), "Find NeAR file objects")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdServer, 20)), "Serve the database over Connect/gRPC/gRPC-Web (loopback by default)")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdReset, 20)), "Delete database")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdVeresion, 20)), "Show version details")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdStats, 20)), "Show stats about data")
	fmt.Printf("  %s %s\n", cmd(pad(cnst.CmdRepair, 20)), "Inspect, finish, verify or upgrade the database")
	fmt.Printf("  %s %s\n", cmd(pad(cmdReindex, 20)), "Rebuild the index of stored evidence")

	printHelpSection("Exit Codes")
	fmt.Println("  0 success; 1 failure; 2 usage error, or done with a warning (for example an")
	fmt.Println("  incomplete index); 70 internal error (see the crash file); 130/143 interrupted")

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
	fmt.Println("Usage: dues store FILE|FOLDER [--no-index|-N] [--require-verified] [--fts|-F] [--enrich|-E] [--hash-algo|-g [sha3|blake3]] [--hash-strategy|-S [sync|async|hocho]] [--pipeline|-Y [batch-owner|staged]] [--simhash|-H] [--revrel|-R] [--store-workers|-w N] [--store-queue|-W N] [global options]")
	fmt.Println("Stores a file, or every regular file in a folder, using chunk-level deduplication.")
	fmt.Println("E01 sets and VHD/VHDX disks are stored as the disk image they decode to, and a")
	fmt.Println("file whose content is already stored is recorded as an alias of it.")
	fmt.Println("Exits 2 when the content was stored but its index is incomplete, or an E01's")
	fmt.Println("stored MD5/SHA-1 does not match.")
	fmt.Println("")
	printHelpSection("Most-Used Flags")
	fmt.Println("  -F, --fts            Enable full-text indexing during store")
	fmt.Println("  -E, --enrich         Enrich hierarchy metadata during store")
	fmt.Println("  -N, --no-index       Skip indexing")
	fmt.Println("      --require-verified  Fail an E01 whose stored digests do not match, instead of storing it flagged")
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
	fmt.Println("  --io-engine, -I         Write engine [auto|stdlib|io-uring]; auto uses stdlib")
	fmt.Println("  --io-uring-queue-depth, -j  io-uring queue depth (0 = auto-tuned)")
	fmt.Println("  --hash-strategy, -S     Hash strategy [sync|async|hocho]")
	fmt.Println("  --hash-algo, -g         Chunk hash of a new database [sha3|blake3]")
	fmt.Println("  Defaults are chosen by storage mode:")
	fmt.Println("    file mode: higher workers")
	fmt.Println("    container: lower workers + deeper queue")
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
	fmt.Println("Usage: dues restore HASH [--filepath|-f restored] [--force] [--verify full|chunks|off] [--verify-ewf] [--restore-workers|-y N] [--restore-queue|-U N] [--restore-buffer-mb|-b N] [global options]")
	fmt.Println("Restores a stored file by its hash. The output is written beside the target and")
	fmt.Println("renamed into place only when the restore succeeded; an existing file is refused")
	fmt.Println("without --force. Exits 2 when the content was restored but is incomplete, or an")
	fmt.Println("E01 does not match its stored digests.")
	printHelpSection("Most-Used Flags")
	fmt.Println("  -f, --filepath            Output path")
	fmt.Println("      --force               Replace the output path if it exists")
	fmt.Println("      --verify              full (default): every chunk and the content's recorded hash;")
	fmt.Println("                            chunks: every chunk only; off: no separate checks")
	fmt.Println("                            (chunks of databases written by this release are always")
	fmt.Println("                            checked as they are decoded)")
	fmt.Println("      --verify-ewf          Also check a restored E01 against the MD5/SHA-1 its set recorded")
	fmt.Println("  -y, --restore-workers     Worker count")
	fmt.Println("  -U, --restore-queue       Queue depth")
	fmt.Println("  -b, --restore-buffer-mb   Write buffer size in MB")
	printHelpSection("Restore Tuning")
	fmt.Println("  --restore-workers, -y N      Override worker count (0 = adaptive default)")
	fmt.Println("  --restore-queue, -U N        Override pipeline queue depth (0 = adaptive default)")
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
	fmt.Println("  Total / completed evidence files, and the database's chunk size")
	fmt.Println("  Total partitions and indexed files")
	fmt.Println("  Indexed deleted and fragmented files")
	fmt.Println("  Total logical size (sum of all stored file sizes)")
	fmt.Println("  Unique chunks vs total chunk references")
	fmt.Println("  Shared chunks (referenced by more than one file)")
	fmt.Println("  On-disk bytes and deduplication ratio")
	fmt.Println("  This process's write engine and its counters, which count only what the same")
	fmt.Println("  process stored (in the TUI, after a store; from dues stats they are zero)")
	printExamples(
		"dues stats",
		"dues stats --dbpath ./caseA",
	)
}

func printRepairHelp() {
	printHelpHeader("repair")
	fmt.Println("Usage: dues repair [--fix [--reindex-failed] [--revrel]] [--verify] [--upgrade [--yes]] [global options]")
	fmt.Println("Without options, lists what interrupted ingests left, opening the database read-only.")
	printHelpSection("Options")
	fmt.Println("  --fix, -J          Finish what interrupted ingests left: unfinished attempts are marked")
	fmt.Println("                     failed (storing the file again resumes them), flushed ones are checked")
	fmt.Println("                     and committed, duplicates removed, and interrupted index passes marked")
	fmt.Println("  --reindex-failed   With --fix, rebuild every index whose pass failed or was interrupted")
	fmt.Println("  --revrel           With --fix, add the reverse relations NeAR needs to evidence stored without them")
	fmt.Println("  --verify           Read back every stored chunk and check it against its hash; exits 1 if any")
	fmt.Println("                     evidence is damaged")
	fmt.Println("  --upgrade          Move a legacy hierarchical block index into the database, and set aside")
	fmt.Println("                     indexes made from the raw bytes of E01 or VHD/VHDX evidence")
	fmt.Println("  --yes              With --upgrade, delete the moved block index files without asking")
	printExamples(
		"dues repair",
		"dues repair --fix",
		"dues repair --fix --reindex-failed",
		"dues repair --verify --dbpath ./caseA",
		"dues repair --upgrade --yes",
	)
}

func printReindexHelp() {
	printHelpHeader(cmdReindex)
	fmt.Println("Usage: dues reindex ID|HASH... | --failed | --all [--source PATH] [--fts] [--enrich] [global options]")
	fmt.Println("Rebuilds the index of stored evidence from its stored chunks, each one checked")
	fmt.Println("against its hash. The previous index, with its full-text documents and graph")
	fmt.Println("nodes, is deleted first. Features the evidence was stored with are rebuilt too.")
	printHelpSection("Options")
	fmt.Println("  --failed     Every evidence whose index pass failed or was interrupted")
	fmt.Println("  --all        Every stored evidence")
	fmt.Println("  --source     Read one evidence from this file instead; its content must match")
	fmt.Println("  --fts        Also build the full-text index")
	fmt.Println("  --enrich     Also write the graph nodes")
	printExamples(
		"dues reindex --failed",
		"dues reindex <hash>",
		"dues reindex <id> --source ./evidence.E01",
		"dues reindex --all --fts --dbpath ./caseA",
	)
}

func printSearchHelp() {
	printHelpHeader("search")
	fmt.Println("Usage: dues search QUERY [--rank-alpha|-r N] [--fts|-F] [--out PATH] [--force] [global options]")
	fmt.Println("Searches the content of every stored evidence, partition and indexed file, case-insensitively.")
	fmt.Println("Terms are literal: a b needs both, a|b either, and \"a b\" in double quotes is one phrase")
	fmt.Println("(at most 16 terms of at most 1024 bytes).")
	fmt.Println("Exits 2 when some objects could not be read; the report names them.")
	printHelpSection("Most-Used Flags")
	fmt.Println("  -F, --fts         Search only the indexed files the full-text index offers;")
	fmt.Println("                    the report says what that leaves out")
	fmt.Println("  -r, --rank-alpha  Occurrence boost weight")
	fmt.Println("      --out         Report path (default report.json)")
	fmt.Println("      --force       Replace the report if it exists")
	printExamples(
		"dues search \"invoice\"",
		"dues search \"invoice\" --fts",
		"dues search \"invoice|receipt\" -r 0.5",
		"dues search \"invoice\" --out case-a-invoice.json --dbpath ./caseA",
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
	fmt.Println("                  Options: --force|-V, --top-k|-K, --out, --overwrite")
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
	fmt.Println("Usage: dues micro extract [--force|-V] [--top-k|-K N] [--out PATH] [--overwrite] [global options]")
	fmt.Println("Extracts micro-artefacts from indexed files and writes them into graph storage.")
	fmt.Println("Files an earlier run examined are skipped. Exits 2 when some files could not be read.")
	printHelpSection("Options")
	fmt.Println("  --force, -V   Extract again from files an earlier run examined, replacing their artefacts")
	fmt.Println("  --top-k, -K   Limit exported HTML graph input to top-K files by artefact count (0 = all)")
	fmt.Println("  --out         Write the HTML graph view here (default graph.html in the working directory)")
	fmt.Println("  --overwrite   Replace the HTML graph view if it exists")
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
	fmt.Println("Both sub-commands write a report (near_report.json, or --out PATH; --force replaces it)")
	fmt.Println("with a ranked list of matching artefacts. Each match includes an overall_relatedness")
	fmt.Println("score that combines all active phases.")
	fmt.Println("NeAR needs reverse relations: store with --revrel, or add them with dues repair --fix --revrel.")
	fmt.Println("It exits 2 when some evidence could not be searched; the report says which.")
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
	cmd := helpCmd()
	printHelpHeader("near in")
	fmt.Println("Usage: dues near in HASH [--deep|-e] [--advanced-deep|-a] [--top-k|-k N] [--out PATH] [--force] [global options]")
	fmt.Println()
	fmt.Println("Finds NeAR objects for a file already stored in the DUES database.")
	fmt.Println("Writes near_report.json (or --out PATH) with ranked matches and an overall_relatedness score per match.")

	printHelpSection("Options")
	fmt.Printf("  %s, %s   Phase 1: enable partial chunk SimHash matching (slower, finds more candidates)\n", cmd("--deep"), cmd("-e"))
	fmt.Printf("  %s, %s   Phase 2: full-file SimHash re-ranking of top-K candidates\n", cmd("--advanced-deep"), cmd("-a"))
	fmt.Printf("              Eliminates chunk-alignment noise from Phase 1 scores.\n")
	fmt.Printf("              Signatures cached in the DB (F|||: namespace) are reused. dues near opens the\n")
	fmt.Printf("              database read-only, so only a writable session (the TUI) adds to that cache.\n")
	fmt.Printf("  %s, %s      Number of candidates for Phase 2 (default 0 = auto-select)\n", cmd("--top-k"), cmd("-k"))
	fmt.Printf("              Auto-selection scales with available memory × CPU threads, bounded [5, 100].\n")
	fmt.Printf("  %s          Report path (default near_report.json)\n", cmd("--out"))
	fmt.Printf("  %s        Replace the report if it exists\n", cmd("--force"))

	printHelpSection("Report Fields (per match)")
	fmt.Println("  overall_relatedness       Single score [0.0–1.0] combining all active phases")
	fmt.Println("  overall_relatedness_percent Same value as a percentage")
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
	cmd := helpCmd()
	printHelpHeader("near out")
	fmt.Println("Usage: dues near out FILE [--deep|-e] [--explain-exact|-t] [--advanced-deep|-a] [--top-k|-k N] [--out PATH] [--force] [global options]")
	fmt.Println()
	fmt.Println("Finds NeAR objects for a file on disk that is outside the DUES database.")
	fmt.Println("Writes near_report.json (or --out PATH) with ranked matches and an overall_relatedness score per match.")

	printHelpSection("Options")
	fmt.Printf("  %s, %s   Phase 1: enable partial chunk SimHash matching (slower, finds more candidates)\n", cmd("--deep"), cmd("-e"))
	fmt.Printf("  %s, %s   Force chunk drilldown even when an exact file hash match exists\n", cmd("--explain-exact"), cmd("-t"))
	fmt.Printf("  %s, %s   Phase 2: full-file SimHash re-ranking of top-K candidates\n", cmd("--advanced-deep"), cmd("-a"))
	fmt.Printf("              Eliminates chunk-alignment noise from Phase 1 scores.\n")
	fmt.Printf("              Note: the query file is external, so its signature is computed fresh each run.\n")
	fmt.Printf("              Candidate signatures cached in the DB are reused; dues near adds none.\n")
	fmt.Printf("  %s, %s      Number of candidates for Phase 2 (default 0 = auto-select)\n", cmd("--top-k"), cmd("-k"))
	fmt.Printf("              Auto-selection scales with available memory × CPU threads, bounded [5, 100].\n")
	fmt.Printf("  %s          Report path (default near_report.json)\n", cmd("--out"))
	fmt.Printf("  %s        Replace the report if it exists\n", cmd("--force"))

	printHelpSection("Report Fields (per match)")
	fmt.Println("  overall_relatedness       Single score [0.0–1.0] combining all active phases")
	fmt.Println("  overall_relatedness_percent Same value as a percentage")
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
	fmt.Println("Usage: dues server [--listen HOST:PORT]... [--token-file PATH] [--cors-origin ORIGIN]... [--hash-algo|-g sha3|blake3] [global options]")
	fmt.Println("Serves the database over Connect, gRPC and gRPC-Web, with resumable browser uploads.")
	fmt.Println("It listens on 127.0.0.1:50051 and [::1]:50051 unless told otherwise. docs/SERVER_API.md describes the protocol.")
	printHelpSection("Options")
	fmt.Println("  --listen        Serve on this host:port (repeatable). A non-loopback address needs a token")
	fmt.Println("  --token-file    Require the bearer token on the first line of this file (at least 32 characters)")
	fmt.Println("  --token         Require this bearer token (visible to other users; prefer --token-file or DUES_TOKEN)")
	fmt.Println("  --cors-origin   Also let pages from this origin call the server, as scheme://host[:port] (repeatable)")
	printHelpSection("Environment")
	fmt.Println("  DUES_TOKEN      The bearer token, when --token-file is not given")
	printHelpSection("Notes")
	fmt.Println("  Traffic is plain HTTP: put a TLS proxy or an SSH tunnel in front of a non-loopback address.")
	fmt.Println("  Health checks: GET /healthz and GET /readyz, without the token.")
	printExamples(
		"dues server",
		"dues server --dbpath ./caseA",
		"dues server --listen 192.168.1.10:50051 --token-file ./token",
		"dues server --cors-origin https://ui.example.org",
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
	fmt.Println("Use the TUI to store, list, search, restore, run NeAR analysis, view statistics and")
	fmt.Println("micro-artefacts, and reset the database. Its jobs run behind the screen; quitting")
	fmt.Println("while one runs asks first, and DUES waits for the jobs before closing the database.")
	printExamples(
		"dues tui",
		"dues tui --dbpath ./caseA",
		"dues tui --dbpath ./caseA --password-file ./caseA.key",
	)

	printHelpSection("TUI Keys")
	fmt.Println("  Up/Down, Tab  Move between items and fields")
	fmt.Println("  Enter         Select, confirm, or press the focused button")
	fmt.Println("  Space         Toggle the focused checkbox")
	fmt.Println("  Esc           Close a question, or go back to the menu")
	fmt.Println("  q             Back, or quit from the menu (typed in a text field)")
	fmt.Println("  Ctrl+X        Cancel the screen's job; on the menu, every job")
	fmt.Println("  Ctrl+C        Quit (asks first while jobs run)")
	fmt.Println("See docs/TUI_QUICKSTART.md.")
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

// helpPainter colours help on a terminal; piped help stays plain text.
func helpPainter() ui.Painter { return ui.For(os.Stdout) }

// helpCmd styles command and flag names: text to type.
func helpCmd() func(string) string {
	p := helpPainter()
	return func(s string) string { return p.Strong(ui.Highlight, s) }
}

// pad left-aligns s in width columns before it is coloured, so escape
// codes do not count toward the alignment.
func pad(s string, width int) string { return fmt.Sprintf("%-*s", width, s) }

func printHelpHeader(name string) {
	fmt.Println("\n" + helpPainter().Strong(ui.Forensics, "Command: "+name))
}

func printHelpSection(name string) {
	fmt.Println("\n" + helpPainter().Strong(ui.Forensics, name))
}

func printExamples(examples ...string) {
	if len(examples) == 0 {
		return
	}

	p := helpPainter()
	fmt.Println(p.Strong(ui.Forensics, "Examples:"))
	for _, ex := range examples {
		fmt.Println("  " + p.Paint(ui.Highlight, ex))
	}
}
