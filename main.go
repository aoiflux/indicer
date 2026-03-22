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
	duesVersion  = "0.37"
	duesCodename = "<starfruit> spacebar"
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
	cmdversion := app.Command("version", "Show version details and quick feature overview")

	cmdstore := app.Command(cnst.CmdStore, "Store file in database")
	evipath := cmdstore.Arg(cnst.OperandFile, "Path of file that must be saved").Required().String()
	syncIndex := cmdstore.Flag(cnst.FlagSyncIndex, "Run file indexer synchronously, this will block dedup").Short(cnst.FlagSyncIndexShort).Default("false").Bool()
	noIndex := cmdstore.Flag(cnst.FlagNoIndex, "Don't run indexer").Short(cnst.FlagNoIndexShort).Default("false").Bool()

	cmdrestore := app.Command(cnst.CmdRestore, "Restore file from database")
	rpath := cmdrestore.Flag(cnst.FlagRestoreFilePath, "Path for restoring the file").Short(cnst.FlagRestoreFilePathShort).Default("restored").String()
	rhash := cmdrestore.Arg(cnst.OperandHash, "Hash of file that must be restoed").String()

	cmdlist := app.Command(cnst.CmdList, "List all the saved files in the database")

	cmdnear := app.Command(cnst.CmdNear, "Get NeAr file objects")
	cmdin := cmdnear.Command(cnst.SubCmdIn, "Finds NeAr objects & generates GReAt graph for file INside of the database")
	deep := cmdin.Flag(cnst.FlagDeep, "Enable/Disable partial chunk match").Short(cnst.FlagDeepShort).Default("false").Bool()
	inhash := cmdin.Arg(cnst.OperandHash, "Hash of the file in DUES DB for which you need to run NeAr").String()

	cmdout := cmdnear.Command(cnst.SubCmdOut, "Finds NeAr objects & generates GReAt graph for file OUTside of the database")
	outpath := cmdout.Arg(cnst.OperandFile, "Path to the file for which you need to run NeAr").String()

	cmdsearch := app.Command(cnst.CmdSearch, "Search anything in DUES DB")
	query := cmdsearch.Arg(cnst.OperandQuery, "Search query string").String()

	apiserver := app.Command(cnst.CmdServer, "Run gRPC / Web combined DUES server")
	cmdreset := app.Command(cnst.CmdReset, "Delete the database")

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

	// Hierarchical index requires container mode
	if cnst.HIERARCHICALINDEX && !cnst.CONTAINERMODE {
		color.Red("⚠️  Hierarchical index requires container mode. Enabling container mode automatically.")
		cnst.CONTAINERMODE = true
	}

	key := util.HashPassword(*pwd)

	if parsed != cmdversion.FullCommand() {
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
	case cmdstore.FullCommand():
		err = cli.StoreData(*chonkSize, *dbpath, *evipath, key, *syncIndex, *noIndex)
	case cmdrestore.FullCommand():
		err = cli.RestoreData(*chonkSize, *dbpath, *rhash, *rpath, key)
	case cmdlist.FullCommand():
		err = cli.ListData(*chonkSize, *dbpath, key)
	case cmdin.FullCommand():
		err = cli.NearInData(*deep, *chonkSize, *dbpath, *inhash, key)
	case cmdout.FullCommand():
		err = cli.NearOutData(*chonkSize, *dbpath, *outpath, key)
	case cmdsearch.FullCommand():
		err = cli.SearchCmd(*chonkSize, *query, *dbpath, key)
	case cmdreset.FullCommand():
		err = cli.ResetData(*dbpath)
	case apiserver.FullCommand():
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
			"  - Fast restore, NeAr similarity analysis, and search\n"+
			"  - Optional container + hierarchical index for scale\n\n"+
			"More: run dues version\n",
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
	fmt.Println("  - Run NeAr similarity checks")
	fmt.Println("  - Serve via gRPC/Web mode")

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
	case cnst.CmdSearch:
		printSearchHelp()
	case cnst.CmdServer:
		printServerHelp()
	case cnst.CmdReset:
		printResetHelp()
	case "version":
		printVersionHelp()
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
	fmt.Printf("  %s    Store file in database\n", cmd("store"))
	fmt.Printf("  %s  Restore file from database\n", cmd("restore"))
	fmt.Printf("  %s     List saved files\n", cmd("list"))
	fmt.Printf("  %s     Search metadata/content index\n", cmd("search"))
	fmt.Printf("  %s       Find NeAr file objects\n", cmd("near"))
	fmt.Printf("  %s     Run gRPC/Web server\n", cmd("server"))
	fmt.Printf("  %s      Delete database\n", cmd("reset"))
	fmt.Printf("  %s    Show version details\n", cmd("version"))

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
	fmt.Println("  dues near in <hash> --deep")
	fmt.Println("")
}

func printStoreHelp() {
	printHelpHeader("store")
	fmt.Println("Usage: dues store FILE [--sync|-s] [--no-index|-n] [global options]")
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

func printSearchHelp() {
	printHelpHeader("search")
	fmt.Println("Usage: dues search QUERY [global options]")
	fmt.Println("Searches indexed data by query string.")
	printExamples(
		"dues search \"invoice\"",
		"dues search \"user:alice\" --dbpath ./caseA",
	)
}

func printNearHelp() {
	printHelpHeader("near")
	fmt.Println("Usage: dues near <in|out> ... [global options]")
	fmt.Println("Finds NeAr file objects.")
	fmt.Println("Try: dues help near in")
	fmt.Println("Try: dues help near out")
	printExamples(
		"dues near in <hash>",
		"dues near out ./suspect.bin",
	)
}

func printNearInHelp() {
	printHelpHeader("near in")
	fmt.Println("Usage: dues near in HASH [--deep|-e] [global options]")
	fmt.Println("Finds NeAr objects for files inside the DUES database.")
	printExamples(
		"dues near in <hash>",
		"dues near in <hash> --deep",
	)
}

func printNearOutHelp() {
	printHelpHeader("near out")
	fmt.Println("Usage: dues near out FILE [global options]")
	fmt.Println("Finds NeAr objects for files outside the DUES database.")
	printExamples(
		"dues near out ./unknown.bin",
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
