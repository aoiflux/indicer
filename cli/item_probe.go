package cli

import (
	"time"

	"indicer/internal/runstats"
	"indicer/lib/dbio"
	"indicer/lib/evidence"
	"indicer/lib/logging"
	"indicer/lib/store"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// itemProbe measures one evidence of a folder store for the debug log: how
// long it took, and what badger's tree, the runtime and the credit ledger
// did meanwhile. Beside other evidence, the deltas are the whole run's while
// it ran; a heavy evidence runs alone, so its deltas are its own.
type itemProbe struct {
	on      bool
	db      *badger.DB
	credits *store.ChunkCredits
	start   time.Time
	lsm     dbio.LSM
	rt      runstats.Runtime
	credit  store.CreditStats
}

// startItemProbe samples before an evidence starts, when debug logging is on.
func startItemProbe(db *badger.DB, credits *store.ChunkCredits) itemProbe {
	if !logging.GetLogger().Core().Enabled(zap.DebugLevel) {
		return itemProbe{}
	}
	return itemProbe{
		on: true, db: db, credits: credits, start: time.Now(),
		lsm: dbio.LSMStats(db), rt: runstats.Sample(), credit: credits.Stats(),
	}
}

// done logs evidence i's StoreFolder ITEM record.
func (p itemProbe) done(i int, item evidence.PlanItem, heavy bool, err error) {
	if !p.on {
		return
	}
	took := time.Since(p.start)
	credit := p.credits.Stats()
	fields := []zap.Field{
		zap.Int("item", i), zap.String("file_path", item.Path), zap.Int64("size", item.Size),
		zap.Bool("heavy", heavy), zap.Int64("duration_ms", took.Milliseconds()),
		zap.Int64("credit_locked", credit.Locked-p.credit.Locked), zap.Int64("credit_held", credit.Held),
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	fields = append(fields, p.lsm.Fields("lsm_before_")...)
	fields = append(fields, dbio.LSMStats(p.db).Fields("lsm_after_")...)
	fields = append(fields, runstats.Sample().Since(p.rt).Fields("")...)
	logging.GetLogger().Debug("StoreFolder ITEM", fields...)
}
