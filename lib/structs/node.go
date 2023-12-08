package structs

type ReverseRelation struct {
	RevRelFileID []byte `msgpack:"rev_rel_file_id"`
	Index        int64  `msgpack:"index"`
}
