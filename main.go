package main

import (
	"fmt"
	"indicer/cli"
	"indicer/lib/cnst"
	"os"

	"github.com/alecthomas/kingpin/v2"
	"github.com/fatih/color"
)

func main() {
	app := kingpin.New("DUES", "Deduplicated Unified Evidence Store")
	dbpath := app.Flag(cnst.FlagDBPath, "Custom path for DUES database").Short(cnst.FlagDBPathShort).String()
	pwd := app.Flag(cnst.FlagPassword, "Password for the DUES database").Short(cnst.FlagPasswordShort).String()
	chonkSize := app.Flag(cnst.FlagChonkSize, "Custom chunk size(KB) to be used for dedup").Short(cnst.FlagChonkSizeShort).Default("256").Int()
	memopt := app.Flag(cnst.FlagLowResource, "Low resource use mode, foregoes performance in favour of utilising less memory, cpu, and energy").Short(cnst.FlagLowResourceShort).Default("false").Bool()
	app.Version("DUES v3")

	cmdstore := app.Command(cnst.CmdStore, "Store file in database")
	evipath := cmdstore.Arg(cnst.OperandFile, "Path of file that must be saved").Required().String()

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

	cmdreset := app.Command(cnst.CmdReset, "Delete the database")

	var err error

	parsed := kingpin.MustParse(app.Parse(os.Args[1:]))
	cnst.MEMOPT = *memopt

	if cnst.MEMOPT {
		color.Green("🍃 running in LOW RESOURCE mode 🍃")
	} else {
		color.Cyan("⚡running in HIGH PERFORMANCE mode ⚡")
	}

	switch parsed {
	case cmdstore.FullCommand():
		err = cli.StoreData(*chonkSize, *dbpath, *pwd, *evipath)
	case cmdrestore.FullCommand():
		err = cli.RestoreData(*chonkSize, *dbpath, *pwd, *rhash, *rpath)
	case cmdlist.FullCommand():
		err = cli.ListData(*chonkSize, *dbpath, *pwd)
	case cmdin.FullCommand():
		err = cli.NearInData(*deep, *chonkSize, *dbpath, *pwd, *inhash)
	case cmdout.FullCommand():
		err = cli.NearOutData(*chonkSize, *dbpath, *pwd, *outpath)
	case cmdreset.FullCommand():
		err = cli.ResetData(*dbpath)
	}

	handle(err)
}

func handle(err error) {
	if err != nil {
		fmt.Printf("\n\n %v \n\n", err)
		os.Exit(1)
	}
}
