package dbio

import (
	"expvar"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// LSM is how far behind badger's tree is at one moment: what a store before
// leaves it to compact while the next one writes.
type LSM struct {
	// L0Tables and L0Bytes are the level-0 tables waiting to be compacted;
	// badger stalls writes once there are NumLevelZeroTablesStall of them.
	L0Tables int
	L0Bytes  int64
	// LSMBytes and VlogBytes are the tree's and the value log's sizes.
	LSMBytes, VlogBytes int64
	// Compacting is how many tables compactions are working on now, and
	// CompactedBytes and L0WrittenBytes what they and memtable flushes
	// wrote so far. badger counts these for the whole process.
	Compacting     int64
	CompactedBytes int64
	L0WrittenBytes int64
}

// LSMStats samples db's tree.
func LSMStats(db *badger.DB) LSM {
	var s LSM
	for _, l := range db.Levels() {
		if l.Level == 0 {
			s.L0Tables, s.L0Bytes = l.NumTables, l.Size
		}
	}
	s.LSMBytes, s.VlogBytes = db.Size()
	s.Compacting = expvarInt("badger_compaction_current_num_lsm")
	s.L0WrittenBytes = expvarInt("badger_write_bytes_l0")
	if m, ok := expvar.Get("badger_write_bytes_compaction").(*expvar.Map); ok {
		m.Do(func(kv expvar.KeyValue) {
			if n, ok := kv.Value.(*expvar.Int); ok {
				s.CompactedBytes += n.Value()
			}
		})
	}
	return s
}

func expvarInt(name string) int64 {
	if n, ok := expvar.Get(name).(*expvar.Int); ok {
		return n.Value()
	}
	return 0
}

// Fields are s as log fields, each name prefixed with prefix.
func (s LSM) Fields(prefix string) []zap.Field {
	return []zap.Field{
		zap.Int(prefix+"l0_tables", s.L0Tables),
		zap.Int64(prefix+"l0_bytes", s.L0Bytes),
		zap.Int64(prefix+"lsm_bytes", s.LSMBytes),
		zap.Int64(prefix+"vlog_bytes", s.VlogBytes),
		zap.Int64(prefix+"compacting_tables", s.Compacting),
		zap.Int64(prefix+"compacted_bytes", s.CompactedBytes),
		zap.Int64(prefix+"l0_written_bytes", s.L0WrittenBytes),
	}
}
