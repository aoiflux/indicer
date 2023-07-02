package cli

import (
	"fmt"
	"indicer/lib/util"
	"os"
	"strings"

	"github.com/fatih/color"
)

func ResetData(dbpath string) error {
	var err error

	if dbpath == "" {
		dbpath, err = util.GetDBPath()
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
	return os.RemoveAll(dbpath)
}
