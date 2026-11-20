package dbio

import (
	"encoding/base64"
	"fmt"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"
)

// Partition and indexed records written since W6 carry their evidence ID,
// partition ID and path as fields. Earlier records carry them in Name:
//
//	partition:    base64(evidence ID) ||| <evidence name>_p<n>
//	indexed file: <raw evidence ID> ||| base64(partition ID) ||| <path>
//
// where the path may itself contain the separator. The functions below read
// either form, so a reader never splits a name itself.

// RecordEvidenceID returns the raw ID of the evidence a partition or indexed
// record belongs to.
func RecordEvidenceID(rec structs.IndexedFile) ([]byte, error) {
	if len(rec.EvidenceID) > 0 {
		return rec.EvidenceID, nil
	}
	return util.GetEvidenceFileHash(rec.Name)
}

// RecordPartitionID returns the raw ID of an indexed record's partition.
func RecordPartitionID(rec structs.IndexedFile) ([]byte, error) {
	if len(rec.PartitionID) > 0 {
		return rec.PartitionID, nil
	}
	_, mid, _, ok := legacyNameParts(rec.Name)
	if !ok || mid == "" {
		return nil, fmt.Errorf("record %q names no partition", rec.Name)
	}
	pid, err := base64.StdEncoding.DecodeString(mid)
	if err != nil {
		return nil, fmt.Errorf("record %q: partition ID: %w", rec.Name, err)
	}
	return pid, nil
}

// RecordPath returns an indexed file's path, or a partition's display name.
func RecordPath(rec structs.IndexedFile) string {
	if rec.Path != "" {
		return rec.Path
	}
	_, mid, last, ok := legacyNameParts(rec.Name)
	switch {
	case !ok:
		return rec.Name
	case last == "" && mid != "":
		return mid // a partition: base64(eid)|||name
	default:
		return last
	}
}

// legacyNameParts splits a legacy name into its evidence ID, the field after
// it, and the rest. A raw evidence ID is recognised by the separator after
// its 16 bytes (see util.GetEvidenceFileHash). For a two-field name the rest
// is empty.
func legacyNameParts(name string) (eid []byte, mid, rest string, ok bool) {
	sep := cnst.DataSeperator
	var tail string
	if len(name) >= 16+len(sep) && name[16:16+len(sep)] == sep {
		eid, tail = []byte(name[:16]), name[16+len(sep):]
	} else {
		head, t, found := strings.Cut(name, sep)
		if !found {
			return nil, "", "", false
		}
		id, err := base64.StdEncoding.DecodeString(head)
		if err != nil || len(id) == 0 {
			return nil, "", "", false
		}
		eid, tail = id, t
	}
	mid, rest, _ = strings.Cut(tail, sep)
	return eid, mid, rest, true
}
