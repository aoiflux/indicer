package main

// Help and version text.

import (
	"errors"
	"fmt"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/kingpin/v2"
	"github.com/charmbracelet/x/ansi"
)

// errVersionShown ends parsing after --version printed the identity line.
var errVersionShown = errors.New("version shown")

// printIdentity prints DUES's one-line identity: `dues --version`, and
// `dues version` where stdout is not a terminal.
func printIdentity() { fmt.Println(ui.Identity(buildInfo())) }

// printVersionInfo is `dues version`: on a terminal the banner (or, under
// --plain, the identity line) and an overview; elsewhere the identity line
// alone, for scripts and bug reports.
func printVersionInfo() {
	if !ui.Current().Out.TTY {
		printIdentity()
		return
	}
	p := helpPainter()
	muted := func(s string) string { return p.Paint(ui.Muted, s) }

	if p.Decorated() {
		fmt.Fprintln(helpOut)
		ui.Banner(os.Stdout, buildInfo())
	} else {
		printIdentity()
	}

	printHelpSection("Overview")
	fmt.Fprintln(helpOut, "  Built for evidence-heavy workflows where space, speed, and traceability")
	fmt.Fprintln(helpOut, "  matter.")

	printHelpSection("Core Capabilities")
	fmt.Fprintln(helpOut, "  - Store files with chunk-level deduplication")
	fmt.Fprintln(helpOut, "  - Restore by hash")
	fmt.Fprintln(helpOut, "  - Search indexed content and metadata")
	fmt.Fprintln(helpOut, "  - Run NeAR similarity checks")

	printHelpSection("Experimental Features")
	fmt.Fprintln(helpOut, "  - Rich TUI")
	fmt.Fprintln(helpOut, "  - Server via gRPC/Web mode")

	printHelpSection("Presets")
	fmt.Fprintln(helpOut, "  --preset quick        Fast ingest defaults (throughput-first)")
	fmt.Fprintln(helpOut, "  --preset performance  Balanced/high-performance defaults")
	fmt.Fprintln(helpOut, "  --preset low-resource Conservative defaults for constrained systems")
	fmt.Fprintln(helpOut, "  Aliases: --quick-mode, --performance-mode, --low-resource-mode")

	printHelpSection("Quick Start")
	fmt.Fprintln(helpOut, "  dues store FILE --dbpath ./caseA")
	fmt.Fprintln(helpOut, "  dues list --dbpath ./caseA")
	fmt.Fprintln(helpOut, "  dues restore HASH --filepath restored.bin")
	fmt.Fprintln(helpOut, "  dues search QUERY --fts")

	fmt.Fprintln(helpOut, muted("----------------------------------------"))
}

// maybeHandleHelp prints DUES's help when args ask for it, and reports
// whether they did, so kingpin's own help and usage text are never shown.
func maybeHandleHelp(args []string, model *kingpin.ApplicationModel) bool {
	topic, ok := helpTopic(args, model)
	if ok {
		printHelpForPath(topic)
	}
	return ok
}

// kingpinActions are the flags whose action replaces the command, so a
// command line with one of them is not a help request.
var kingpinActions = map[string]bool{
	"version":                true,
	"help-long":              true,
	"help-man":               true,
	"completion-bash":        true,
	"completion-script-bash": true,
	"completion-script-zsh":  true,
}

// helpTopic reads args for a help request: "help [command]", a command
// with --help or -h, or no command at all. It skips flags and their values
// with the parser's own model, so "dues -d x help store" asks for store's
// help. It returns false when args ask for no help, or hold a flag the
// parser does not know or a flag without its value: kingpin reports those.
func helpTopic(args []string, model *kingpin.ApplicationModel) ([]string, bool) {
	flags := helpFlags{long: map[string]*kingpin.FlagModel{}, short: map[rune]*kingpin.FlagModel{}}
	flags.add(model.FlagGroupModel)
	cmds := model.CmdGroupModel
	var words []string
	asked := func() ([]string, bool) {
		if len(words) > 0 && words[0] == "help" {
			return words[1:], true
		}
		return words, true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return nil, false
		case strings.HasPrefix(a, "--"):
			name, value, hasValue := strings.Cut(a[2:], "=")
			f, negated := flags.long[name], false
			if base, ok := strings.CutPrefix(name, "no-"); f == nil && ok {
				if g := flags.long[base]; g != nil && g.IsBoolFlag() {
					f, negated = g, true
				}
			}
			switch {
			case f == nil || kingpinActions[f.Name]:
				return nil, false
			case f.IsBoolFlag():
				if hasValue {
					if _, err := strconv.ParseBool(value); err != nil || negated {
						return nil, false
					}
				}
				if f == flags.help && !negated && !hasValue {
					return asked()
				}
			case !hasValue:
				if i+1 == len(args) {
					return nil, false
				}
				i++
			}
		case len(a) > 1 && a[0] == '-':
			// Short flags, perhaps several in one argument. One that takes a
			// value takes the rest of the argument, or the next argument.
			for rest := a[1:]; rest != ""; {
				r, size := utf8.DecodeRuneInString(rest)
				rest = rest[size:]
				f := flags.short[r]
				if f == nil || kingpinActions[f.Name] {
					return nil, false
				}
				if f == flags.help {
					return asked()
				}
				if f.IsBoolFlag() {
					continue
				}
				if rest == "" {
					if i+1 == len(args) {
						return nil, false
					}
					i++
				}
				break
			}
		default:
			words = append(words, a)
			// After "help" the words name the command whose help is asked.
			if len(words) > 1 || a != "help" {
				cmds = flags.enter(cmds, a)
			}
		}
	}
	if len(words) == 0 || words[0] == "help" {
		return asked()
	}
	return nil, false
}

// helpFlags are the flags known at a point of the command line: the global
// ones and those of each command named so far.
type helpFlags struct {
	long  map[string]*kingpin.FlagModel
	short map[rune]*kingpin.FlagModel
	help  *kingpin.FlagModel
}

func (h *helpFlags) add(g *kingpin.FlagGroupModel) {
	for _, f := range g.Flags {
		h.long[f.Name] = f
		if f.Short != 0 {
			h.short[f.Short] = f
		}
		if f.Name == "help" {
			h.help = f
		}
	}
}

// enter returns the subcommands of the command named word among cmds, and
// adds its flags; past a word that names no command, there are none.
func (h *helpFlags) enter(cmds *kingpin.CmdGroupModel, word string) *kingpin.CmdGroupModel {
	if cmds == nil {
		return nil
	}
	for _, c := range cmds.Commands {
		if c.Name == word || slices.Contains(c.Aliases, word) {
			h.add(c.FlagGroupModel)
			return c.CmdGroupModel
		}
	}
	return nil
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

	if path[0] == cmdPackage {
		if len(path) > 1 {
			switch path[1] {
			case cmdPackageExport:
				printPackageExportHelp()
				return
			case cmdPackageInspect:
				printPackageInspectHelp()
				return
			case cmdPackageVerify:
				printPackageVerifyHelp()
				return
			case cmdPackageImport:
				printPackageImportHelp()
				return
			}
		}
		printPackageHelp()
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
	case cmdConfig:
		printConfigHelp()
	case cnst.CmdTui:
		printTuiHelp()
	case cmdCoffee, cmdFortune, cmdMeow:
		fmt.Fprintf(helpOut, "dues %s is one of DUES's hidden extras. It runs under --no-fun too:\n", path[0])
		fmt.Fprintln(helpOut, "--no-fun only turns off the surprises after long commands and mew's animation.")
	default:
		fmt.Fprintln(helpOut, helpPainter().Strong(ui.Error, "Unknown command: "+strings.Join(path, " ")))
		fmt.Fprintln(helpOut, "")
		printRootHelp()
	}
}

func printRootHelp() {
	p := helpPainter()
	cmd := helpCmd()
	muted := func(s string) string { return p.Paint(ui.Muted, s) }

	if p.Decorated() {
		fmt.Fprintln(helpOut)
		ui.Banner(os.Stdout, buildInfo())
	} else {
		printIdentity()
	}
	fmt.Fprintln(helpOut, muted("Reliable evidence storage, deduplication, search, and restore."))
	printHelpSection("Usage")
	fmt.Fprintln(helpOut, "  dues [global options] <command> [command options]")

	printHelpSection("Global Options")
	fmt.Fprintf(helpOut, "  %s, %s   Show this help\n", cmd("--help"), cmd("-h"))
	fmt.Fprintf(helpOut, "  %s, %s   Show the version line\n", cmd("--version"), cmd("-v"))
	fmt.Fprintf(helpOut, "  %s FILE    Read settings from this config file (default: ~/.config/dues/dues.json, if it exists)\n", cmd("--config"))
	fmt.Fprintf(helpOut, "  %s     Read no config file\n", cmd("--no-config"))
	fmt.Fprintf(helpOut, "  %s, %s   Custom path for DUES database\n", cmd("--dbpath"), cmd("-d"))
	fmt.Fprintf(helpOut, "  %s       Directory for log and crash files\n", cmd("--log-dir"))
	fmt.Fprintf(helpOut, "  %s      Directory for temporary files (default: /tmp, or AppData\\Local\\Temp on Windows)\n", cmd("--temp-dir"))
	fmt.Fprintf(helpOut, "  %s         Write debug entries to the log\n", cmd("--debug"))
	fmt.Fprintf(helpOut, "  %s FILE    Write a Go execution trace of the run to this new file\n", cmd("--trace"))
	fmt.Fprintf(helpOut, "  %s  Read the database password from the first line of a file\n", cmd("--password-file"))
	fmt.Fprintf(helpOut, "  %s Read the database password from standard input\n", cmd("--password-stdin"))
	fmt.Fprintf(helpOut, "  %s, %s   Chunk size (KB) of a new database, default 256\n", cmd("--chonksize"), cmd("-c"))
	fmt.Fprintf(helpOut, "  %s, %s   Low resource mode\n", cmd("--low"), cmd("-l"))
	fmt.Fprintf(helpOut, "  %s, %s   Quick mode: a new database without table compression (encryption unchanged)\n", cmd("--quick"), cmd("-q"))
	fmt.Fprintf(helpOut, "  %s, %s   Container storage for a new database\n", cmd("--container"), cmd("-x"))
	fmt.Fprintf(helpOut, "  %s, %s   Chunk compression level [fast|default|best]\n", cmd("--compress-level"), cmd("-z"))
	fmt.Fprintf(helpOut, "  %s, %s   Mode preset [quick|performance|low-resource]\n", cmd("--preset"), cmd("-P"))
	fmt.Fprintf(helpOut, "  %s, %s   Quick preset convenience alias\n", cmd("--quick-mode"), cmd("-Q"))
	fmt.Fprintf(helpOut, "  %s, %s   Performance preset convenience alias\n", cmd("--performance-mode"), cmd("-o"))
	fmt.Fprintf(helpOut, "  %s, %s   Low-resource preset convenience alias\n", cmd("--low-resource-mode"), cmd("-L"))
	fmt.Fprintf(helpOut, "  %s WHEN      Colour: auto (256 colours on a terminal), always, never, ansi (the theme's 16), truecolor\n", cmd("--color"))
	fmt.Fprintf(helpOut, "  %s           Plain output: no colour, ASCII only, no animation (screen readers, logs, conhost)\n", cmd("--plain"))
	fmt.Fprintf(helpOut, "  %s              Behave as if no terminal is attached: plain output, and prompts refuse\n", cmd("--ci"))
	fmt.Fprintf(helpOut, "  %s THEME     Terminal background: auto, dark or light\n", cmd("--theme"))
	fmt.Fprintf(helpOut, "  %s         Progress on the terminal's tab and taskbar (Windows Terminal, ConEmu)\n", cmd("--taskbar"))
	fmt.Fprintf(helpOut, "  %s          No easter eggs\n", cmd("--no-fun"))

	printHelpSection("Output")
	fmt.Fprintln(helpOut, "  Results go to stdout; the run header, progress, notes, warnings and errors go")
	fmt.Fprintln(helpOut, "  to stderr, so `dues list | jq` reads pure JSON. On a pipe nothing is coloured.")
	fmt.Fprintln(helpOut, "  DUES reads no environment variables: every setting is a flag or a key in the")
	fmt.Fprintln(helpOut, "  config file (dues help config).")

	printHelpSection("Database Layout")
	fmt.Fprintln(helpOut, "  Chunk size, hash algorithm, storage and encryption are fixed when a database")
	fmt.Fprintln(helpOut, "  is created and recorded in its manifest. An existing database keeps its own;")
	fmt.Fprintln(helpOut, "  a flag that contradicts it is an error (exit 2).")

	printHelpSection("Preset Rules")
	fmt.Fprintln(helpOut, "  Presets are convenience defaults.")
	fmt.Fprintln(helpOut, "  Explicit flags always override preset-derived values.")
	fmt.Fprintln(helpOut, "  Use only one preset at a time.")

	printHelpSection("Preset Decision Guide")
	fmt.Fprintln(helpOut, "  quick:        Maximum ingest throughput, lower protection/compression")
	fmt.Fprintln(helpOut, "  performance:  Balanced/default behavior for most workstations")
	fmt.Fprintln(helpOut, "  low-resource: Best for constrained CPU/memory systems")
	fmt.Fprintln(helpOut, "  Tip: start with --preset performance and override only what you need.")

	printHelpSection("Commands")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdTui, 20)), "Launch interactive TUI interface")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdStore, 20)), "Store file in database")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdRestore, 20)), "Restore file from database")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdList, 20)), "List saved files")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdSearch, 20)), "Search metadata/content index")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdMicro, 20)), "Manage micro-artefacts (extract, list)")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdEnrich, 20)), "Backfill file hierarchy metadata into graphdb")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdNear, 20)), "Find NeAR file objects")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdServer, 20)), "Serve the database over Connect/gRPC/gRPC-Web (loopback by default)")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdReset, 20)), "Delete database")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdVeresion, 20)), "Show version details")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cmdConfig, 20)), "Show the settings in effect and where each came from")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdStats, 20)), "Show stats about data")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cnst.CmdRepair, 20)), "Inspect, finish, verify or upgrade the database")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cmdReindex, 20)), "Rebuild the index of stored evidence")
	fmt.Fprintf(helpOut, "  %s %s\n", cmd(pad(cmdPackage, 20)), "Export the database as a package; inspect, verify or import one")

	printHelpSection("Exit Codes")
	fmt.Fprintln(helpOut, "  0 success; 1 failure; 2 usage error, or done with a warning (for example an")
	fmt.Fprintln(helpOut, "  incomplete index); 70 internal error (see the crash file); 130/143 interrupted")

	printHelpSection("Command-Level Help")
	fmt.Fprintln(helpOut, "  Root help and command help are both available.")
	fmt.Fprintln(helpOut, "  Use: dues help <command>")
	fmt.Fprintln(helpOut, "  or : dues <command> --help")
	fmt.Fprintln(helpOut, "  Example: dues help near")
	fmt.Fprintln(helpOut, "  Example: dues near in --help")

	printHelpSection("Examples")
	fmt.Fprintln(helpOut, "  dues store evidence.img --dbpath ./dues-data")
	fmt.Fprintln(helpOut, "  dues store evidence.img --preset quick")
	fmt.Fprintln(helpOut, "  dues store evidence.img --quick-mode --fts --enrich")
	fmt.Fprintln(helpOut, "  dues restore <hash> --filepath recovered.bin")
	fmt.Fprintln(helpOut, "  dues restore <hash> -y 8 -U 32 -m 100")
	fmt.Fprintln(helpOut, "  dues search \"invoice\" --fts")
	fmt.Fprintln(helpOut, "  dues enrich --dbpath ./dues-data")
	fmt.Fprintln(helpOut, "  dues micro extract --top-k 20")
	fmt.Fprintln(helpOut, "  dues near in <hash> --deep")

	printHelpSection("Workflow Recipes")
	fmt.Fprintln(helpOut, "  Triage (fast ingest):")
	fmt.Fprintln(helpOut, "    dues store E01-image.dd --preset quick --fts")
	fmt.Fprintln(helpOut, "  Balanced case ingest:")
	fmt.Fprintln(helpOut, "    dues store E01-image.dd --preset performance --enrich --fts")
	fmt.Fprintln(helpOut, "  Low-resource laptop ingest:")
	fmt.Fprintln(helpOut, "    dues store E01-image.dd --preset low-resource --compress-level default")
	fmt.Fprintln(helpOut, "  Investigation loop:")
	fmt.Fprintln(helpOut, "    dues search \"keyword\" --fts")
	fmt.Fprintln(helpOut, "    dues near in <hash> --deep --advanced-deep --top-k 20")
	fmt.Fprintln(helpOut, "    dues restore <hash> --filepath recovered.bin")
	fmt.Fprintln(helpOut, "")
}

func printStoreHelp() {
	printHelpHeader("store")
	fmt.Fprintln(helpOut, "Usage: dues store FILE|FOLDER [--no-index|-N] [--require-verified] [--fts|-F] [--enrich|-E] [--hash-algo|-g [sha3|blake3]] [--hash-strategy|-S [sync|async|hocho]] [--pipeline|-Y [batch-owner|staged]] [--simhash|-H] [--revrel|-R] [--store-workers|-w N] [--store-queue|-W N] [--store-jobs N] [global options]")
	fmt.Fprintln(helpOut, "Stores a file, or every regular file in a folder, using chunk-level")
	fmt.Fprintln(helpOut, "deduplication. E01 sets and VHD/VHDX disks are stored as the disk image they")
	fmt.Fprintln(helpOut, "decode to, and a file whose content is already stored is recorded as an alias")
	fmt.Fprintln(helpOut, "of it.")
	fmt.Fprintln(helpOut, "Exits 2 when the content was stored but its index is incomplete, or an E01's")
	fmt.Fprintln(helpOut, "stored MD5/SHA-1 does not match.")
	fmt.Fprintln(helpOut, "")
	printHelpSection("Most-Used Flags")
	fmt.Fprintln(helpOut, "  -F, --fts            Enable full-text indexing during store")
	fmt.Fprintln(helpOut, "  -E, --enrich         Enrich hierarchy metadata during store")
	fmt.Fprintln(helpOut, "  -N, --no-index       Skip indexing")
	fmt.Fprintln(helpOut, "      --require-verified  Fail an E01 whose stored digests do not match, instead of storing it flagged")
	fmt.Fprintln(helpOut, "  -w, --store-workers  Override worker count")
	fmt.Fprintln(helpOut, "  -W, --store-queue    Override queue depth")
	fmt.Fprintln(helpOut, "")
	printHelpSection("Quick Paths")
	fmt.Fprintln(helpOut, "  Everyday default:")
	fmt.Fprintln(helpOut, "    dues store FILE")
	fmt.Fprintln(helpOut, "  Throughput-first:")
	fmt.Fprintln(helpOut, "    dues store FILE --preset quick")
	fmt.Fprintln(helpOut, "  Constrained machine:")
	fmt.Fprintln(helpOut, "    dues store FILE --preset low-resource")
	fmt.Fprintln(helpOut, "  Advanced tuning:")
	fmt.Fprintln(helpOut, "    dues store FILE -w 24 -W 192 -Y batch-owner")
	fmt.Fprintln(helpOut, "")
	printHelpSection("Store Pipeline")
	fmt.Fprintln(helpOut, "  Fan-out/fan-in model:")
	fmt.Fprintln(helpOut, "    worker goroutines: hash + materialize chunk payloads in parallel")
	fmt.Fprintln(helpOut, "    single writer: batch metadata + relation/revrel writes")
	fmt.Fprintln(helpOut, "  This keeps expensive chunk work parallel while preserving deterministic")
	fmt.Fprintln(helpOut, "  batch ownership.")
	fmt.Fprintln(helpOut, "")
	printHelpSection("Store Tuning")
	fmt.Fprintln(helpOut, "  --store-workers, -w N   Override worker count (0 = mode-aware default)")
	fmt.Fprintln(helpOut, "  --store-queue, -W N     Override task queue depth (0 = mode-aware default)")
	fmt.Fprintln(helpOut, "  --store-jobs N          Evidence of a folder stored at once (default 1).")
	fmt.Fprintln(helpOut, "                          0 picks a number for this machine that fits in")
	fmt.Fprintln(helpOut, "                          memory; a number that does not fit is refused.")
	fmt.Fprintln(helpOut, "                          Low-resource mode stores one at a time. Large")
	fmt.Fprintln(helpOut, "                          evidence and evidence that may hold the same")
	fmt.Fprintln(helpOut, "                          content are stored alone, and the report keeps")
	fmt.Fprintln(helpOut, "                          folder order.")
	fmt.Fprintln(helpOut, "  --revrel, -R            Persist reverse-relation keys during ingest (off by default)")
	fmt.Fprintln(helpOut, "  --io-engine, -I         Write engine [auto|stdlib|io-uring]; auto uses stdlib")
	fmt.Fprintln(helpOut, "  --io-uring-queue-depth, -j  io-uring queue depth (0 = auto-tuned)")
	fmt.Fprintln(helpOut, "  --hash-strategy, -S     Hash strategy [sync|async|hocho]")
	fmt.Fprintln(helpOut, "  --hash-algo, -g         Chunk hash of a new database [sha3|blake3]")
	fmt.Fprintln(helpOut, "  Defaults are chosen by storage mode:")
	fmt.Fprintln(helpOut, "    file mode: higher workers")
	fmt.Fprintln(helpOut, "    container: lower workers + deeper queue")
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
	fmt.Fprintln(helpOut, "Usage: dues enrich [global options]")
	fmt.Fprintln(helpOut, "Backfills file-level hierarchy nodes into graphdb: evidence file -> partition")
	fmt.Fprintln(helpOut, "-> indexed file.")
	printExamples(
		"dues enrich",
		"dues enrich --dbpath ./caseA",
	)
}

func printRestoreHelp() {
	printHelpHeader("restore")
	fmt.Fprintln(helpOut, "Usage: dues restore HASH [--filepath|-f restored] [--force] [--verify full|chunks|off] [--verify-ewf] [--restore-workers|-y N] [--restore-queue|-U N] [--restore-buffer-mb|-b N] [global options]")
	fmt.Fprintln(helpOut, "Restores a stored file by its hash. The output is written beside the target and")
	fmt.Fprintln(helpOut, "renamed into place only when the restore succeeded; an existing file is refused")
	fmt.Fprintln(helpOut, "without --force. Exits 2 when the content was restored but is incomplete, or an")
	fmt.Fprintln(helpOut, "E01 does not match its stored digests.")
	printHelpSection("Most-Used Flags")
	fmt.Fprintln(helpOut, "  -f, --filepath            Output path")
	fmt.Fprintln(helpOut, "      --force               Replace the output path if it exists")
	fmt.Fprintln(helpOut, "      --verify              full (default): every chunk and the content's recorded hash;")
	fmt.Fprintln(helpOut, "                            chunks: every chunk only; off: no separate checks")
	fmt.Fprintln(helpOut, "                            (chunks of databases written by this release are")
	fmt.Fprintln(helpOut, "                            always checked as they are decoded)")
	fmt.Fprintln(helpOut, "      --verify-ewf          Also check a restored E01 against the MD5/SHA-1 its set recorded")
	fmt.Fprintln(helpOut, "  -y, --restore-workers     Worker count")
	fmt.Fprintln(helpOut, "  -U, --restore-queue       Queue depth")
	fmt.Fprintln(helpOut, "  -b, --restore-buffer-mb   Write buffer size in MB")
	printHelpSection("Restore Tuning")
	fmt.Fprintln(helpOut, "  --restore-workers, -y N      Override worker count (0 = adaptive default)")
	fmt.Fprintln(helpOut, "  --restore-queue, -U N        Override pipeline queue depth (0 = adaptive default)")
	fmt.Fprintln(helpOut, "  --restore-buffer-mb, -b N    Output write buffer size (MB, 0 = mode-aware default)")
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
	fmt.Fprintln(helpOut, "Usage: dues list [--status|-u all|completed|pending|failed] [global options]")
	fmt.Fprintln(helpOut, "Lists all saved files in the database.")
	printExamples(
		"dues list",
		"dues list --status completed",
		"dues list --dbpath ./caseA",
	)
}

func printStatsHelp() {
	printHelpHeader("stats")
	fmt.Fprintln(helpOut, "Usage: dues stats [global options]")
	fmt.Fprintln(helpOut, "Displays statistics about the DUES database.")
	fmt.Fprintln(helpOut)
	printHelpSection("Output includes")
	fmt.Fprintln(helpOut, "  Total / completed evidence files, and the database's chunk size")
	fmt.Fprintln(helpOut, "  Total partitions and indexed files")
	fmt.Fprintln(helpOut, "  Indexed deleted and fragmented files")
	fmt.Fprintln(helpOut, "  Total logical size (sum of all stored file sizes)")
	fmt.Fprintln(helpOut, "  Unique chunks vs total chunk references")
	fmt.Fprintln(helpOut, "  Shared chunks (referenced by more than one file)")
	fmt.Fprintln(helpOut, "  On-disk bytes and deduplication ratio")
	fmt.Fprintln(helpOut, "  This process's write engine and its counters, which count only what the same")
	fmt.Fprintln(helpOut, "  process stored (in the TUI, after a store; from dues stats they are zero)")
	printExamples(
		"dues stats",
		"dues stats --dbpath ./caseA",
	)
}

func printRepairHelp() {
	printHelpHeader("repair")
	fmt.Fprintln(helpOut, "Usage: dues repair [--fix [--reindex-failed] [--revrel]] [--verify] [--upgrade [--yes]] [global options]")
	fmt.Fprintln(helpOut, "Without options, lists what interrupted ingests left, opening the database")
	fmt.Fprintln(helpOut, "read-only. Prints one JSON document on stdout, with a section for each step that")
	fmt.Fprintln(helpOut, "ran; the steps' messages and the --upgrade question go to stderr.")
	printHelpSection("Options")
	fmt.Fprintln(helpOut, "  --fix, -J          Finish what interrupted ingests left: unfinished attempts")
	fmt.Fprintln(helpOut, "                     are marked failed (storing the file again resumes them),")
	fmt.Fprintln(helpOut, "                     flushed ones are checked and committed, duplicates removed,")
	fmt.Fprintln(helpOut, "                     and interrupted index passes marked")
	fmt.Fprintln(helpOut, "  --reindex-failed   With --fix, rebuild every index whose pass failed or was interrupted")
	fmt.Fprintln(helpOut, "  --revrel           With --fix, add the reverse relations NeAR needs to evidence stored without them")
	fmt.Fprintln(helpOut, "  --verify           Read back every stored chunk and check it against its hash;")
	fmt.Fprintln(helpOut, "                     exits 1 if any evidence is damaged")
	fmt.Fprintln(helpOut, "  --upgrade          Move a legacy hierarchical block index into the database,")
	fmt.Fprintln(helpOut, "                     and set aside indexes made from the raw bytes of E01 or")
	fmt.Fprintln(helpOut, "                     VHD/VHDX evidence")
	fmt.Fprintln(helpOut, "  --yes              With --upgrade, delete the moved block index files without asking")
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
	fmt.Fprintln(helpOut, "Usage: dues reindex ID|HASH... | --failed | --all [--source PATH] [--fts] [--enrich] [--store-jobs N] [global options]")
	fmt.Fprintln(helpOut, "Rebuilds the index of stored evidence from its stored chunks, each one checked")
	fmt.Fprintln(helpOut, "against its hash. The previous index, with its full-text documents and graph")
	fmt.Fprintln(helpOut, "nodes, is deleted first. Features the evidence was stored with are rebuilt too.")
	printHelpSection("Options")
	fmt.Fprintln(helpOut, "  --failed     Every evidence whose index pass failed or was interrupted")
	fmt.Fprintln(helpOut, "  --all        Every stored evidence")
	fmt.Fprintln(helpOut, "  --source     Read one evidence from this file instead; its content must match")
	fmt.Fprintln(helpOut, "  --fts        Also build the full-text index")
	fmt.Fprintln(helpOut, "  --enrich     Also write the graph nodes")
	fmt.Fprintln(helpOut, "  --store-jobs N")
	fmt.Fprintln(helpOut, "               Evidence reindexed at once (default 1; 0 picks a number for")
	fmt.Fprintln(helpOut, "               this machine that fits in memory, and a number that does not")
	fmt.Fprintln(helpOut, "               fit is refused). A large one runs alone; the report reads as")
	fmt.Fprintln(helpOut, "               one at a time.")
	printExamples(
		"dues reindex --failed",
		"dues reindex <hash>",
		"dues reindex <id> --source ./evidence.E01",
		"dues reindex --all --fts --dbpath ./caseA",
		"dues reindex --all --store-jobs 4",
	)
}

func printPackageHelp() {
	printHelpHeader(cmdPackage)
	fmt.Fprintln(helpOut, "Usage: dues package <export|inspect|verify|import> ... [global options]")
	fmt.Fprintln(helpOut, "A package is a .duesarc holding a copy of the database's files as they are on")
	fmt.Fprintln(helpOut, "disk, and a manifest with each file's size and SHA-256. Move it, keep it, and")
	fmt.Fprintln(helpOut, "check it later; its content hash names the database state it holds.")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "  package export   Copy the database into a package")
	fmt.Fprintln(helpOut, "  package inspect  Describe a package from its manifest")
	fmt.Fprintln(helpOut, "  package verify   Check every file of a package against its manifest")
	fmt.Fprintln(helpOut, "  package import   Make a new database from a package")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "Try: dues help package export")
	printExamples(
		"dues package export --out caseA.duesarc --dbpath ./caseA",
		"dues package inspect caseA.duesarc",
		"dues package verify caseA.duesarc",
		"dues package import caseA.duesarc --dbpath ./caseA-copy",
	)
}

func printPackageExportHelp() {
	printHelpHeader("package export")
	fmt.Fprintln(helpOut, "Usage: dues package export [--out PATH] [--split-size-mb N] [--force]")
	fmt.Fprintln(helpOut, "         [--without-derived] [--host-info] [global options]")
	fmt.Fprintln(helpOut, "Copies the database into a package. Other DUES commands may read the database")
	fmt.Fprintln(helpOut, "meanwhile; none may write it, and the export refuses to start while one does.")
	fmt.Fprintln(helpOut, "A database that was not closed cleanly is refused: run dues repair --fix.")
	fmt.Fprintln(helpOut, "An encrypted database stays encrypted in the package, and needs no password;")
	fmt.Fprintln(helpOut, "with one, the password is checked first. Its full-text index and graph are")
	fmt.Fprintln(helpOut, "not encrypted, on disk or in the package.")
	printHelpSection("Options")
	fmt.Fprintln(helpOut, "  --out              Package path (default dues-<database>-<UTC time>.duesarc)")
	fmt.Fprintln(helpOut, "  --split-size-mb    Write parts of this many MiB and a .split.json sidecar")
	fmt.Fprintln(helpOut, "  --force            Replace the package if it exists")
	fmt.Fprintln(helpOut, "  --without-derived  Leave out the full-text index and the graph; reindex --fts,")
	fmt.Fprintln(helpOut, "                     enrich and micro extract rebuild them")
	fmt.Fprintln(helpOut, "  --host-info        Record this machine's OS, architecture and name")
	printExamples(
		"dues package export --dbpath ./caseA",
		"dues package export --out caseA.duesarc --split-size-mb 4096 --dbpath ./caseA",
		"dues package export --without-derived --password-file ./pw --dbpath ./caseB",
	)
}

func printPackageInspectHelp() {
	printHelpHeader("package inspect")
	fmt.Fprintln(helpOut, "Usage: dues package inspect PACKAGE [global options]")
	fmt.Fprintln(helpOut, "Describes a package from its manifest alone: who made it and when, the")
	fmt.Fprintln(helpOut, "database's format and layout, encryption, files, bytes and content hash.")
	fmt.Fprintln(helpOut, "It reads nothing else, so it is quick, and checks nothing: use verify.")
	printExamples(
		"dues package inspect caseA.duesarc",
		"dues package inspect caseA.duesarc.split.json",
	)
}

func printPackageVerifyHelp() {
	printHelpHeader("package verify")
	fmt.Fprintln(helpOut, "Usage: dues package verify PACKAGE [--deep] [global options]")
	fmt.Fprintln(helpOut, "Reads the whole package and checks it: a split package's parts against their")
	fmt.Fprintln(helpOut, "sidecar, then every file against the size and SHA-256 its manifest gives, and")
	fmt.Fprintln(helpOut, "that nothing is missing or added. Each problem is named; any fails the command.")
	printHelpSection("Options")
	fmt.Fprintln(helpOut, "  --deep  Also check the database the package holds: copy it into --temp-dir,")
	fmt.Fprintln(helpOut, "          open the copy read-only, count its evidence against the manifest")
	fmt.Fprintln(helpOut, "          and read every evidence back against its chunk hashes, as repair")
	fmt.Fprintln(helpOut, "          --verify does. It needs the package's size free there, and the")
	fmt.Fprintln(helpOut, "          password of an encrypted database. The copy is removed afterwards.")
	printExamples(
		"dues package verify caseA.duesarc",
		"dues package verify caseA.duesarc.001",
		"dues package verify caseB.duesarc --deep --password-file ./pw",
	)
}

func printPackageImportHelp() {
	printHelpHeader("package import")
	fmt.Fprintln(helpOut, "Usage: dues package import PACKAGE [global options]")
	fmt.Fprintln(helpOut, "Makes a new database at --dbpath (default ~/.dues) from a package. The folder")
	fmt.Fprintln(helpOut, "must be missing or empty: an import never merges into a database. The files")
	fmt.Fprintln(helpOut, "are written into a folder beside it and checked against the manifest as they")
	fmt.Fprintln(helpOut, "are written; the database is opened to check it, and only then does the folder")
	fmt.Fprintln(helpOut, "take the --dbpath name. A failure or an interruption leaves nothing behind.")
	fmt.Fprintln(helpOut, "An encrypted database is opened only with its password (--password-file);")
	fmt.Fprintln(helpOut, "without it, its files are checked and it is not opened.")
	printExamples(
		"dues package import caseA.duesarc --dbpath ./caseA",
		"dues package import caseB.duesarc.001 --dbpath ./caseB --password-file ./pw",
	)
}

func printSearchHelp() {
	printHelpHeader("search")
	fmt.Fprintln(helpOut, "Usage: dues search QUERY [--rank-alpha|-r N] [--fts|-F] [--out PATH] [--force] [global options]")
	fmt.Fprintln(helpOut, "Searches the content of every stored evidence, partition and indexed file,")
	fmt.Fprintln(helpOut, "case-insensitively. Terms are literal: a b needs both, a|b either, and \"a b\"")
	fmt.Fprintln(helpOut, "in double quotes is one phrase (at most 16 terms of at most 1024 bytes).")
	fmt.Fprintln(helpOut, "Exits 2 when some objects could not be read; the report names them.")
	printHelpSection("Most-Used Flags")
	fmt.Fprintln(helpOut, "  -F, --fts         Search only the indexed files the full-text index offers;")
	fmt.Fprintln(helpOut, "                    the report says what that leaves out")
	fmt.Fprintln(helpOut, "  -r, --rank-alpha  Occurrence boost weight")
	fmt.Fprintln(helpOut, "      --out         Report path (default report.json)")
	fmt.Fprintln(helpOut, "      --force       Replace the report if it exists")
	printExamples(
		"dues search \"invoice\"",
		"dues search \"invoice\" --fts",
		"dues search \"invoice|receipt\" -r 0.5",
		"dues search \"invoice\" --out case-a-invoice.json --dbpath ./caseA",
	)
}

func printMicroArtefactsHelp() {
	printHelpHeader("micro")
	fmt.Fprintln(helpOut, "Usage: dues micro <extract|list> [options] [global options]")
	fmt.Fprintln(helpOut, "Manage micro-artefacts extracted from indexed files in the database.")
	fmt.Fprintln(helpOut)
	printHelpSection("Most-Used Commands")
	fmt.Fprintln(helpOut, "  dues micro extract -K 20")
	fmt.Fprintln(helpOut, "  dues micro extract -V")
	fmt.Fprintln(helpOut, "  dues micro list")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "  micro extract  — Extract micro-artefacts from all indexed files and populate graph database")
	fmt.Fprintln(helpOut, "                  Options: --force|-V, --top-k|-K, --out, --overwrite")
	fmt.Fprintln(helpOut, "  micro list     — List all micro-artefacts in JSON format")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "Try: dues help micro extract")
	fmt.Fprintln(helpOut, "or : dues help micro list")
	printExamples(
		"dues micro extract",
		"dues micro extract --top-k 20",
		"dues micro extract -V -K 5",
		"dues micro list",
	)
}

func printMicroExtractHelp() {
	printHelpHeader("micro extract")
	fmt.Fprintln(helpOut, "Usage: dues micro extract [--force|-V] [--top-k|-K N] [--out PATH] [--overwrite] [global options]")
	fmt.Fprintln(helpOut, "Extracts micro-artefacts from indexed files and writes them into graph storage.")
	fmt.Fprintln(helpOut, "Files an earlier run examined are skipped.")
	fmt.Fprintln(helpOut, "Exits 2 when some files could not be read.")
	printHelpSection("Options")
	fmt.Fprintln(helpOut, "  --force, -V   Extract again from files an earlier run examined, replacing their artefacts")
	fmt.Fprintln(helpOut, "  --top-k, -K   Limit exported HTML graph input to top-K files by artefact count (0 = all)")
	fmt.Fprintln(helpOut, "  --out         Write the HTML graph view here (default graph.html in the working directory)")
	fmt.Fprintln(helpOut, "  --overwrite   Replace the HTML graph view if it exists")
	printExamples(
		"dues micro extract",
		"dues micro extract --top-k 10",
		"dues micro extract -V -K 5 --dbpath ./caseA",
	)
}

func printMicroListHelp() {
	printHelpHeader("micro list")
	fmt.Fprintln(helpOut, "Usage: dues micro list [global options]")
	fmt.Fprintln(helpOut, "Lists stored micro-artefacts in JSON format.")
	printExamples(
		"dues micro list",
		"dues micro list --dbpath ./caseA",
	)
}

func printNearHelp() {
	printHelpHeader("near")
	fmt.Fprintln(helpOut, "Usage: dues near <in|out> ... [global options]")
	fmt.Fprintln(helpOut, "Finds NeAR (Near Artefact Relation) file objects and writes a similarity report.")
	fmt.Fprintln(helpOut)
	printHelpSection("Most-Used Flags")
	fmt.Fprintln(helpOut, "  -e, --deep           Enable phase-1 chunk-level drilldown")
	fmt.Fprintln(helpOut, "  -a, --advanced-deep  Enable phase-2 full-file re-ranking")
	fmt.Fprintln(helpOut, "  -k, --top-k          Candidate count for phase-2")
	fmt.Fprintln(helpOut, "  -t, --explain-exact  Force explanation even on exact match (near out)")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "  near in   — query file is already stored in the DUES database (identified by hash)")
	fmt.Fprintln(helpOut, "  near out  — query file is on disk outside the database (identified by path)")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "Both sub-commands write a report (near_report.json, or --out PATH; --force")
	fmt.Fprintln(helpOut, "replaces it) with a ranked list of matching artefacts. Each match includes an")
	fmt.Fprintln(helpOut, "overall_relatedness score that combines all active phases.")
	fmt.Fprintln(helpOut, "NeAR needs reverse relations: store with --revrel, or add them with")
	fmt.Fprintln(helpOut, "dues repair --fix --revrel.")
	fmt.Fprintln(helpOut, "It exits 2 when some evidence could not be searched; the report says which.")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "Try: dues help near in")
	fmt.Fprintln(helpOut, "Try: dues help near out")
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
	fmt.Fprintln(helpOut, "Usage: dues near in HASH [--deep|-e] [--advanced-deep|-a] [--top-k|-k N] [--out PATH] [--force] [global options]")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "Finds NeAR objects for a file already stored in the DUES database.")
	fmt.Fprintln(helpOut, "Writes near_report.json (or --out PATH) with ranked matches and an")
	fmt.Fprintln(helpOut, "overall_relatedness score per match.")

	printHelpSection("Options")
	fmt.Fprintf(helpOut, "  %s, %s   Phase 1: enable partial chunk SimHash matching (slower, finds more candidates)\n", cmd("--deep"), cmd("-e"))
	fmt.Fprintf(helpOut, "  %s, %s   Phase 2: full-file SimHash re-ranking of top-K candidates\n", cmd("--advanced-deep"), cmd("-a"))
	fmt.Fprintf(helpOut, "              Eliminates chunk-alignment noise from Phase 1 scores.\n")
	fmt.Fprintf(helpOut, "              Signatures cached in the DB (F|||: namespace) are reused.\n")
	fmt.Fprintf(helpOut, "              dues near opens the database read-only, so only a writable\n")
	fmt.Fprintf(helpOut, "              session (the TUI) adds to that cache.\n")
	fmt.Fprintf(helpOut, "  %s, %s      Number of candidates for Phase 2 (default 0 = auto-select)\n", cmd("--top-k"), cmd("-k"))
	fmt.Fprintf(helpOut, "              Auto-selection scales with available memory × CPU threads,\n")
	fmt.Fprintf(helpOut, "              bounded [5, 100].\n")
	fmt.Fprintf(helpOut, "  %s          Report path (default near_report.json)\n", cmd("--out"))
	fmt.Fprintf(helpOut, "  %s        Replace the report if it exists\n", cmd("--force"))

	printHelpSection("Report Fields (per match)")
	fmt.Fprintln(helpOut, "  overall_relatedness       Single score [0.0–1.0] combining all active phases")
	fmt.Fprintln(helpOut, "  overall_relatedness_percent Same value as a percentage")
	fmt.Fprintln(helpOut, "  relatedness_basis         Which signals were used: exact | phase1 | phase1+phase2")
	fmt.Fprintln(helpOut, "  phase1_deviation_estimate Fraction of chunk comparisons affected by")
	fmt.Fprintln(helpOut, "                            edge-alignment noise")
	fmt.Fprintln(helpOut, "  phase2_file_similarity    Full-file SimHash similarity (only present when --advanced-deep used)")

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
	fmt.Fprintln(helpOut, "Usage: dues near out FILE [--deep|-e] [--explain-exact|-t] [--advanced-deep|-a] [--top-k|-k N] [--out PATH] [--force] [global options]")
	fmt.Fprintln(helpOut)
	fmt.Fprintln(helpOut, "Finds NeAR objects for a file on disk that is outside the DUES database.")
	fmt.Fprintln(helpOut, "Writes near_report.json (or --out PATH) with ranked matches and an")
	fmt.Fprintln(helpOut, "overall_relatedness score per match.")

	printHelpSection("Options")
	fmt.Fprintf(helpOut, "  %s, %s   Phase 1: enable partial chunk SimHash matching (slower, finds more candidates)\n", cmd("--deep"), cmd("-e"))
	fmt.Fprintf(helpOut, "  %s, %s   Force chunk drilldown even when an exact file hash match exists\n", cmd("--explain-exact"), cmd("-t"))
	fmt.Fprintf(helpOut, "  %s, %s   Phase 2: full-file SimHash re-ranking of top-K candidates\n", cmd("--advanced-deep"), cmd("-a"))
	fmt.Fprintf(helpOut, "              Eliminates chunk-alignment noise from Phase 1 scores.\n")
	fmt.Fprintf(helpOut, "              Note: the query file is external, so its signature is computed\n")
	fmt.Fprintf(helpOut, "              fresh each run. Candidate signatures cached in the DB are reused;\n")
	fmt.Fprintf(helpOut, "              dues near adds none.\n")
	fmt.Fprintf(helpOut, "  %s, %s      Number of candidates for Phase 2 (default 0 = auto-select)\n", cmd("--top-k"), cmd("-k"))
	fmt.Fprintf(helpOut, "              Auto-selection scales with available memory × CPU threads,\n")
	fmt.Fprintf(helpOut, "              bounded [5, 100].\n")
	fmt.Fprintf(helpOut, "  %s          Report path (default near_report.json)\n", cmd("--out"))
	fmt.Fprintf(helpOut, "  %s        Replace the report if it exists\n", cmd("--force"))

	printHelpSection("Report Fields (per match)")
	fmt.Fprintln(helpOut, "  overall_relatedness       Single score [0.0–1.0] combining all active phases")
	fmt.Fprintln(helpOut, "  overall_relatedness_percent Same value as a percentage")
	fmt.Fprintln(helpOut, "  relatedness_basis         Which signals were used: exact | phase1 | phase1+phase2")
	fmt.Fprintln(helpOut, "  phase1_deviation_estimate Fraction of chunk comparisons affected by")
	fmt.Fprintln(helpOut, "                            edge-alignment noise")
	fmt.Fprintln(helpOut, "  phase2_file_similarity    Full-file SimHash similarity (only present when --advanced-deep used)")

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
	fmt.Fprintln(helpOut, "Usage: dues server [--listen HOST:PORT]... [--token-file PATH] [--cors-origin ORIGIN]... [--hash-algo|-g sha3|blake3] [global options]")
	fmt.Fprintln(helpOut, "Serves the database over Connect, gRPC and gRPC-Web, with resumable browser")
	fmt.Fprintln(helpOut, "uploads. It listens on 127.0.0.1:50051 and [::1]:50051 unless told otherwise.")
	fmt.Fprintln(helpOut, "docs/SERVER_API.md describes the protocol.")
	printHelpSection("Options")
	fmt.Fprintln(helpOut, "  --listen        Serve on this host:port (repeatable). A non-loopback address needs a token")
	fmt.Fprintln(helpOut, "  --token-file    Require the bearer token on the first line of this file (at least 32 characters)")
	fmt.Fprintln(helpOut, "  --cors-origin   Also let pages from this origin call the server, as scheme://host[:port] (repeatable)")
	printHelpSection("Notes")
	fmt.Fprintln(helpOut, "  Traffic is plain HTTP: put a TLS proxy or an SSH tunnel in front of a")
	fmt.Fprintln(helpOut, "  non-loopback address.")
	fmt.Fprintln(helpOut, "  Health checks: GET /healthz and GET /readyz, without the token.")
	printExamples(
		"dues server",
		"dues server --dbpath ./caseA",
		"dues server --listen 192.168.1.10:50051 --token-file ./token",
		"dues server --cors-origin https://ui.example.org",
	)
}

func printResetHelp() {
	printHelpHeader("reset")
	fmt.Fprintln(helpOut, "Usage: dues reset [--yes] [global options]")
	fmt.Fprintln(helpOut, "Deletes the DUES database in the data folder, and nothing else there.")
	fmt.Fprintln(helpOut, "Asks you to type the folder name; --yes skips that, and is required when")
	fmt.Fprintln(helpOut, "stdin is not a terminal. Refuses a folder that holds no DUES database, and")
	fmt.Fprintln(helpOut, "your home folder, the working folder or a drive root.")
	printExamples(
		"dues reset",
		"dues reset --dbpath ./caseA",
		"dues reset --dbpath ./caseA --yes",
	)
}

func printTuiHelp() {
	printHelpHeader("tui")
	fmt.Fprintln(helpOut, "Usage: dues tui [global options]")
	fmt.Fprintln(helpOut, "Launches the interactive Terminal UI for DUES operations.")
	fmt.Fprintln(helpOut, "Use the TUI to store, list, search, restore, run NeAR analysis, view statistics")
	fmt.Fprintln(helpOut, "and micro-artefacts, and reset the database. Its jobs run behind the screen;")
	fmt.Fprintln(helpOut, "quitting while one runs asks first, and DUES waits for the jobs before closing")
	fmt.Fprintln(helpOut, "the database.")
	printExamples(
		"dues tui",
		"dues tui --dbpath ./caseA",
		"dues tui --dbpath ./caseA --password-file ./caseA.key",
	)

	printHelpSection("TUI Keys")
	fmt.Fprintln(helpOut, "  Up/Down, Tab  Move between items and fields")
	fmt.Fprintln(helpOut, "  Enter         Select, confirm, or press the focused button")
	fmt.Fprintln(helpOut, "  Space         Toggle the focused checkbox")
	fmt.Fprintln(helpOut, "  Esc           Close a question, or go back to the menu")
	fmt.Fprintln(helpOut, "  q             Back, or quit from the menu (typed in a text field)")
	fmt.Fprintln(helpOut, "  Ctrl+X        Cancel the screen's job; on the menu, every job")
	fmt.Fprintln(helpOut, "  Ctrl+C        Quit (asks first while jobs run)")
	fmt.Fprintln(helpOut, "See docs/TUI_QUICKSTART.md.")
}

func printVersionHelp() {
	printHelpHeader("version")
	fmt.Fprintln(helpOut, "Usage: dues version")
	fmt.Fprintln(helpOut, "On a terminal, shows the banner, a feature overview and preset guidance. Piped,")
	fmt.Fprintln(helpOut, "it prints one line: version, commit and codename. dues -v prints that line")
	fmt.Fprintln(helpOut, "anywhere.")
	printExamples(
		"dues version",
		"dues -v",
	)
}

func printConfigHelp() {
	cmd := helpCmd()
	printHelpHeader(cmdConfig)
	fmt.Fprintln(helpOut, "Usage: dues config [global options]")
	fmt.Fprintln(helpOut, "Prints, as JSON on stdout, the config file read and every setting it can hold:")
	fmt.Fprintln(helpOut, "the value in effect and its source (flag, config or default). It opens no")
	fmt.Fprintln(helpOut, "database and reads neither the password file nor the token file.")

	printHelpSection("The Config File")
	fmt.Fprintln(helpOut, "  Settings kept between runs, so they need not be typed every time. DUES reads")
	fmt.Fprintln(helpOut, "  ~/.config/dues/dues.json in the account's home directory, on every system,")
	fmt.Fprintf(helpOut, "  when it exists; %s FILE reads another, which must exist, and %s\n", cmd("--config"), cmd("--no-config"))
	fmt.Fprintln(helpOut, "  reads none. The file is one JSON object. Keys are flag names, exactly as")
	fmt.Fprintln(helpOut, "  typed; a command's own settings go in an object named after it, and apply to")
	fmt.Fprintln(helpOut, "  that command only, except that the TUI's stores and restores take store's")
	fmt.Fprintln(helpOut, "  and restore's. Paths must be absolute. On Linux and macOS the file must")
	fmt.Fprintln(helpOut, "  belong to you or root, and neither its group nor others may write it.")

	printHelpSection("Keys")
	fmt.Fprintln(helpOut, "  color, plain, ci, no-fun, theme, taskbar, log-dir, temp-dir, debug, dbpath,")
	fmt.Fprintln(helpOut, "  password-file, preset, compress-level, low")
	fmt.Fprintln(helpOut, "  store:   pipeline, hash-strategy, io-engine, io-uring-queue-depth,")
	fmt.Fprintln(helpOut, "           store-workers, store-queue, store-jobs")
	fmt.Fprintln(helpOut, "  restore: restore-workers, restore-queue, restore-buffer-mb")
	fmt.Fprintln(helpOut, "  server:  listen, token-file, cors-origin")
	fmt.Fprintln(helpOut, "  Not keys: the database layout (chonksize, container, hash-algo, quick), which")
	fmt.Fprintln(helpOut, "  a database fixes when it is created; choices for one run (fix, force, out,")
	fmt.Fprintln(helpOut, "  ...); and the password, which only a file named by password-file holds.")

	printHelpSection("Precedence")
	fmt.Fprintln(helpOut, "  A flag wins over the file, and the file over the default. A preset on the")
	fmt.Fprintln(helpOut, "  command line (--preset, -Q, -o, -L) replaces the file's preset,")
	fmt.Fprintln(helpOut, "  compress-level and low. The file's password-file applies unless")
	fmt.Fprintln(helpOut, "  --password-file or --password-stdin is given: with it, new databases are")
	fmt.Fprintln(helpOut, "  created encrypted. --password-file= uses none.")
	fmt.Fprintln(helpOut, "  A list (listen, cors-origin) on the command line replaces the file's.")

	printHelpSection("Errors")
	fmt.Fprintln(helpOut, "  A file that cannot be used (bad JSON, an unknown or repeated key, a bad value)")
	fmt.Fprintln(helpOut, "  stops every command with exit 2, naming the file, line and column. Fix it, or")
	fmt.Fprintf(helpOut, "  run with %s.\n", cmd("--no-config"))

	printHelpSection("Example")
	fmt.Fprintln(helpOut, `  {`)
	fmt.Fprintln(helpOut, `    "theme": "light",`)
	fmt.Fprintln(helpOut, `    "log-dir": "D:\\dues\\logs",`)
	fmt.Fprintln(helpOut, `    "dbpath": "D:\\cases\\c42",`)
	fmt.Fprintln(helpOut, `    "password-file": "D:\\keys\\c42.key",`)
	fmt.Fprintln(helpOut, `    "preset": "performance",`)
	fmt.Fprintln(helpOut, `    "store": { "pipeline": "staged", "store-workers": 8 },`)
	fmt.Fprintln(helpOut, `    "server": {`)
	fmt.Fprintln(helpOut, `      "listen": ["127.0.0.1:50051"],`)
	fmt.Fprintln(helpOut, `      "token-file": "D:\\keys\\server.token"`)
	fmt.Fprintln(helpOut, `    }`)
	fmt.Fprintln(helpOut, `  }`)
	printExamples(
		"dues config",
		"dues --config ./case42.json config",
		"dues --no-config store evidence.img",
	)
}

// helpPainter colours help on a terminal; piped help stays plain text.
func helpPainter() ui.Painter { return ui.For(os.Stdout) }

// helpWidth is the widest line help prints, so it reads unbroken in an
// 80-column terminal.
const helpWidth = 80

// helpOut is where help goes: stdout, with each line wider than helpWidth
// wrapped by wrapHelpLine. The identity line is not written through it:
// scripts read it as one line.
var helpOut io.Writer = helpWriter{}

type helpWriter struct{}

func (helpWriter) Write(p []byte) (int, error) {
	lines := strings.Split(string(p), "\n")
	var b strings.Builder
	for i, line := range lines {
		if i == len(lines)-1 {
			b.WriteString(line) // not ended yet: written as it is
			break
		}
		for _, l := range wrapHelpLine(line, helpWidth) {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	if _, err := io.WriteString(os.Stdout, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}

// wrapHelpLine breaks a line wider than width at spaces. The lines after
// the first start at its description column (see hangColumn). Colour codes
// are kept, and a word is never broken, so a flag name stays whole.
func wrapHelpLine(line string, width int) []string {
	total := ansi.StringWidth(line)
	if total <= width {
		return []string{line}
	}
	hang := hangColumn(ansi.Strip(line))
	indent := strings.Repeat(" ", hang)
	var out []string
	cur, curWidth, empty := ansi.Cut(line, 0, hang), hang, true
	for _, word := range bracketWords(ansi.Cut(line, hang, total)) {
		w := ansi.StringWidth(word)
		if !empty && curWidth+1+w > width {
			out = append(out, cur)
			cur, curWidth, empty = indent, hang, true
		}
		if !empty {
			cur += " "
			curWidth++
		}
		cur += word
		curWidth += w
		empty = false
	}
	return append(out, cur)
}

// bracketWords splits s at spaces, keeping a bracketed group such as
// "[--hash-algo|-g [sha3|blake3]]" or "[global options]" one word, so a
// synopsis never breaks inside an option.
func bracketWords(s string) []string {
	var words []string
	depth := 0
	for _, f := range strings.Fields(s) {
		if depth > 0 {
			words[len(words)-1] += " " + f
		} else {
			words = append(words, f)
		}
		plain := ansi.Strip(f) // colour codes hold a "[" too
		depth = max(depth+strings.Count(plain, "[")-strings.Count(plain, "]"), 0)
	}
	return words
}

// hangColumn is where the continuation of a wrapped help line starts: after
// "Usage: " in a synopsis; at the description in a flag or command row,
// after its first run of two or more spaces; at the line's own indent in
// prose.
func hangColumn(plain string) int {
	if strings.HasPrefix(plain, "Usage: ") {
		return len("Usage: ")
	}
	indent := len(plain) - len(strings.TrimLeft(plain, " "))
	if gap := strings.Index(plain[indent:], "  "); gap >= 0 {
		end := indent + gap
		for end < len(plain) && plain[end] == ' ' {
			end++
		}
		if col := ansi.StringWidth(plain[:end]); col <= helpWidth/2 {
			return col
		}
	}
	return indent
}

// helpCmd styles command and flag names: text to type.
func helpCmd() func(string) string {
	p := helpPainter()
	return func(s string) string { return p.Strong(ui.Highlight, s) }
}

// pad left-aligns s in width columns before it is coloured, so escape
// codes do not count toward the alignment.
func pad(s string, width int) string { return fmt.Sprintf("%-*s", width, s) }

func printHelpHeader(name string) {
	fmt.Fprintln(helpOut, "\n"+helpPainter().Strong(ui.Forensics, "Command: "+name))
}

func printHelpSection(name string) {
	fmt.Fprintln(helpOut, "\n"+helpPainter().Strong(ui.Forensics, name))
}

func printExamples(examples ...string) {
	if len(examples) == 0 {
		return
	}

	p := helpPainter()
	fmt.Fprintln(helpOut, p.Strong(ui.Forensics, "Examples:"))
	for _, ex := range examples {
		fmt.Fprintln(helpOut, "  "+p.Paint(ui.Highlight, ex))
	}
}
