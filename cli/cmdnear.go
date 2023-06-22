package cli

import (
	"indicer/lib/cnst"
	"indicer/lib/near"

	"github.com/ibraimgm/libcmd"
)

func NearInData(cmd *libcmd.Cmd) error {
	db, err := common(cmd)
	if err != nil {
		return err
	}

	deep := cmd.GetBool(cnst.FlagDeep)
	fhash := cmd.Operand(cnst.OperandHash)
	if fhash == "" {
		return cnst.ErrHashNotFound
	}

	return near.NearInFile(fhash, db, *deep)
}

func NearOutData(cmd *libcmd.Cmd) error {
	db, err := common(cmd)
	if err != nil {
		return err
	}

	file := cmd.Operand(cnst.OperandFile)
	if file == "" {
		return cnst.ErrFileNotFound
	}

	err = near.NearOutFile(file, db)
	if err != nil {
		return err
	}

	return db.Close()
}
