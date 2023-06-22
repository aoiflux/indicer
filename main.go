package main

import (
	"fmt"
	"indicer/cli"
	"indicer/lib/cnst"
	"os"

	"github.com/ibraimgm/libcmd"
)

func main() {
	app := libcmd.NewApp("DUES", "Deduplicated Unified Evidence Store")
	app.Command(cnst.CmdStore, "Store file in database", cmdstore)
	app.Command(cnst.CmdRestore, "Restore file from database", cmdrestore)
	app.Command(cnst.CmdList, "List all the saved files in the database", cmdlist)
	app.Command(cnst.CmdNear, "Get NeAr file objects", cmdnear)
	app.Command(cnst.CmdReset, "Reset database", cmdreset)
	app.Usage = "dues <store|restore|list|near|reset> [-d] [dbpath] [-p] [password] [filepath|hash]"

	app.Run(func(*libcmd.Cmd) error {
		app.Help()
		return nil
	})

	if err := app.Parse(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func cmdstore(cmd *libcmd.Cmd) {
	cmd.String(cnst.FlagDBPath, cnst.FlagDBPathShort, "")
	cmd.String(cnst.FlagPassword, cnst.FlagPasswordShort, "")
	cmd.Int32(cnst.FlagChonkSize, cnst.FlagChonkSizeShort, 256)
	cmd.AddOperand(cnst.OperandFile, "")
	cmd.Run(func(cmd *libcmd.Cmd) error { return cli.StoreData(cmd) })
}
func cmdrestore(cmd *libcmd.Cmd) {
	cmd.String(cnst.FlagDBPath, cnst.FlagDBPathShort, "")
	cmd.String(cnst.FlagPassword, cnst.FlagPasswordShort, "")
	cmd.String(cnst.FlagRestoreFilePath, cnst.FlagRestoreFilePathShort, "restored")
	cmd.Int32(cnst.FlagChonkSize, cnst.FlagChonkSizeShort, 256)
	cmd.AddOperand(cnst.OperandHash, "")
	cmd.Run(func(cmd *libcmd.Cmd) error { return cli.RestoreData(cmd) })
}
func cmdlist(cmd *libcmd.Cmd) {
	cmd.String(cnst.FlagDBPath, cnst.FlagDBPathShort, "")
	cmd.String(cnst.FlagPassword, cnst.FlagPasswordShort, "")
	cmd.Int32(cnst.FlagChonkSize, cnst.FlagChonkSizeShort, 256)
	cmd.Run(func(cmd *libcmd.Cmd) error { return cli.ListData(cmd) })
}
func cmdnear(cmd *libcmd.Cmd) {
	cmd.Command(cnst.SubCmdIn, "Finds NeAr objects & generates GReAt graph for file INside of the database", func(cmd *libcmd.Cmd) {
		cmd.String(cnst.FlagDBPath, cnst.FlagDBPathShort, "")
		cmd.String(cnst.FlagPassword, cnst.FlagPasswordShort, "")
		cmd.Int32(cnst.FlagChonkSize, cnst.FlagChonkSizeShort, 256)
		cmd.Bool(cnst.FlagDeep, cnst.FlagDeepShort, false)
		cmd.AddOperand(cnst.OperandHash, "")

		cmd.Run(cli.NearInData)
	})

	cmd.Command(cnst.SubCmdOut, "Finds NeAr objects & generates GReAt graph for file OUTside of the database", func(cmd *libcmd.Cmd) {
		cmd.String(cnst.FlagDBPath, cnst.FlagDBPathShort, "")
		cmd.String(cnst.FlagPassword, cnst.FlagPasswordShort, "")
		cmd.Int32(cnst.FlagChonkSize, cnst.FlagChonkSizeShort, 256)
		cmd.AddOperand(cnst.OperandFile, "")

		cmd.Run(cli.NearOutData)
	})
}
func cmdreset(cmd *libcmd.Cmd) {
	cmd.String(cnst.FlagDBPath, cnst.FlagDBPathShort, "")
	cmd.Run(func(cmd *libcmd.Cmd) error { return cli.ResetData(cmd) })
}
