package main

import (
	"fmt"
	"indicer/cli"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"

	"github.com/alecthomas/kingpin/v2"
	"github.com/fatih/color"
	"github.com/klauspost/compress/zstd"
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
	app.Version("DUES v3.5")
	dbpath := app.Flag(cnst.FlagDBPath, "Custom path for DUES database").Short(cnst.FlagDBPathShort).String()
	pwd := app.Flag(cnst.FlagPassword, "Password for the DUES database").Short(cnst.FlagPasswordShort).String()
	chonkSize := app.Flag(cnst.FlagChonkSize, "Custom chunk size(KB) to be used for dedup").Short(cnst.FlagChonkSizeShort).Default("256").Int()
	memopt := app.Flag(cnst.FlagLowResource, "Low resource use mode, foregoes performance in favour of utilising less memory, cpu, and energy").Short(cnst.FlagLowResourceShort).Default("false").Bool()
	QUICKOPT := app.Flag(cnst.FlagFastMode, "Quick mode, forgoes encryption, intra-chunk & overall db compression in favour of higher throughput").Short(cnst.FlagFastModeShort).Default("false").Bool()
	containerMode := app.Flag(cnst.FlagContainerMode, "Use container-based storage (packs multiple chunks into 1GB containers)").Short(cnst.FlagContainerModeShort).Default("false").Bool()

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

	cmdreset := app.Command(cnst.CmdReset, "Delete the database")

	var err error

	parsed := kingpin.MustParse(app.Parse(os.Args[1:]))
	cnst.MEMOPT = *memopt
	cnst.QUICKOPT = *QUICKOPT
	cnst.CONTAINERMODE = *containerMode
	key := util.HashPassword(*pwd)

	if cnst.MEMOPT {
		color.Green("🍃 running in LOW RESOURCE mode 🍃")
	} else {
		color.Cyan("⚡ running in HIGH PERFORMANCE mode ⚡")
	}
	if cnst.QUICKOPT {
		color.Magenta("🛫 quick mode enabled 🛬")
	}
	if cnst.CONTAINERMODE {
		color.Yellow("📦 container mode enabled 📦")
	}

	switch parsed {
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
	}

	handle(err)

	err = cnst.ENCODER.Close()
	handle(err)
	cnst.DECODER.Close()
}

func handle(err error) {
	if err != nil {
		fmt.Printf("\n\n %v \n\n", err)
		os.Exit(1)
	}
}
