package cli

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/ibraimgm/libcmd"
)

func ResetData(cmd *libcmd.Cmd) error {
	var err error
	dbpath := cmd.GetString(cnst.FlagDBPath)
	if *dbpath == "" {
		*dbpath, err = util.GetDBPath()
		if err != nil {
			return err
		}
	}

	color.Red("WARNING! This command will DELETE ALL the saved files.")
	fmt.Printf("Are you sure about this? [y/N] ")

	var in string
	fmt.Scanln(&in)
	in = strings.ToLower(in)

	if in != "y" {
		color.Blue("Your data is SAFE!")
		return nil
	}

	color.Red("Deleting ALL data!")
	return os.RemoveAll(*dbpath)
}
