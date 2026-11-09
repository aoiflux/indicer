package graph

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"

	"indicer/internal/console"
	"indicer/lib/logging"
	"indicer/lib/util"

	graphenedisk "github.com/aoiflux/graphene/disk"
	"go.uber.org/zap"
)

// Config is how this process's writable handles on one data root's graph
// end. Only the CLI's commands configure a root; the TUI, the server and the
// compression product leave every root unconfigured, so their handles never
// compact.
type Config struct {
	// AutoCompact has the last Release of a writable handle compact the store
	// when its log has reached CompactAt. A compaction that is refused or
	// fails is a warning, and the store stays as it was.
	AutoCompact bool
	// CompactAt is the log that calls for a compaction. A zero field takes
	// the size past which an open warns: 1 GiB, or 5 million records.
	CompactAt Threshold
	// Ctx is the command's context. Once it is cancelled no compaction
	// starts, and one already building stops. Nil never cancels.
	Ctx context.Context
}

// Threshold is a size of the log written since the last compaction. A log
// has reached it when it holds Bytes or Records.
type Threshold struct {
	Bytes   int64
	Records int64
}

// orDefault fills t's zero fields with the slow-open warning's sizes.
func (t Threshold) orDefault() Threshold {
	if t.Bytes <= 0 {
		t.Bytes = replayWarnBytes
	}
	if t.Records <= 0 {
		t.Records = replayWarnRecords
	}
	return t
}

var (
	configMu sync.Mutex
	// configs holds, per graph directory, the configurations in force, the
	// newest last.
	configs = map[string][]*Config{}
)

// Configure sets how this process's handles on root's graph end, until
// remove is called. A later Configure of the same root takes over until it
// is removed.
func Configure(root string, cfg Config) (remove func(), err error) {
	key, err := registryKey(util.GraphPath(root))
	if err != nil {
		return nil, err
	}
	c := &cfg
	configMu.Lock()
	configs[key] = append(configs[key], c)
	configMu.Unlock()
	return sync.OnceFunc(func() {
		configMu.Lock()
		defer configMu.Unlock()
		left := slices.DeleteFunc(configs[key], func(x *Config) bool { return x == c })
		if len(left) == 0 {
			delete(configs, key)
			return
		}
		configs[key] = left
	}), nil
}

// Configured is the configuration in force for root's graph.
func Configured(root string) (Config, bool) {
	key, err := registryKey(util.GraphPath(root))
	if err != nil {
		return Config{}, false
	}
	return configFor(key)
}

func configFor(key string) (Config, bool) {
	configMu.Lock()
	defer configMu.Unlock()
	if cs := configs[key]; len(cs) > 0 {
		return *cs[len(cs)-1], true
	}
	return Config{}, false
}

// compactIfDue is the compaction the last Release of a writable handle runs
// when its root is configured for it and the log has reached the threshold.
// It never fails the Release: what it stored is committed already, and a
// store that is not compacted opens as it did.
func (h *Handle) compactIfDue() {
	if h.readOnly {
		return
	}
	cfg, ok := configFor(h.key)
	if !ok || !cfg.AutoCompact {
		return
	}
	ctx := cfg.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || !logReached(h.dir, cfg.CompactAt.orDefault()) {
		return
	}
	rep, err := Compact(ctx, h)
	switch {
	case err == nil:
		logging.GetLogger().Info("graph: compacted",
			zap.String("dir", rep.Dir), zap.Int64("log_bytes", rep.LogBytes), zap.String("segment", rep.Segment),
			zap.Int("nodes", rep.Image.Nodes), zap.Int("edges", rep.Image.Edges),
			zap.Int64("image_bytes", rep.ImageBytes), zap.Duration("took", rep.Took))
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		// Ctrl-C: the store is as it was, and the command reports the
		// interruption itself.
		logging.GetLogger().Info("graph: compaction interrupted", zap.String("dir", h.dir), zap.Error(err))
	default:
		logging.GetLogger().Warn("graph: not compacted", zap.String("dir", h.dir), zap.Error(err))
		console.Errorln("warning: " + err.Error())
	}
}

// logReached reports whether the log in dir has reached t. Its records are
// counted only when it is large enough to hold that many.
func logReached(dir string, t Threshold) bool {
	size := fileSize(filepath.Join(dir, logFileName))
	if size >= t.Bytes {
		return true
	}
	if size < t.Records*minWALRecordBytes {
		return false
	}
	est, err := graphenedisk.PreflightOpen(dir)
	return err == nil && est.WALRecords+est.WALRecordsBuffered >= t.Records
}
