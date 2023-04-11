package pebble

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

const (
	pathLockTableL2 = "./testdata/2203/751840.sst"
	pathLockTableL3 = "./testdata/2203/750937.sst"
	pathLockTableL4 = "./testdata/2203/742967.sst"
	pathLockTableL5 = "./testdata/2203/611426.sst"
	pathLockTableL6 = "./testdata/2203/305950.sst"

	pathGlobalTableL5 = "./testdata/2203/696593.sst"
	pathGlobalTableL6 = "./testdata/2203/635692.sst"
)

const seekKeyHex = "017a6b12e9891284a0e479965e11eb851f0afe96fd362100ff0188000100030000000000000000000000000000000012"

func BenchmarkTest(t *testing.B) {
	// Set up a Pebble DB.
	dir := t.TempDir()
	opts := &Options{
		FS:       vfs.Default,
		Comparer: EngineComparer,
		Merger:   MVCCMerger,
		// Prevent movement in the LSM due to compactions.
		DisableAutomaticCompactions: true,
		FormatMajorVersion:          FormatNewest,
		Cache:                       NewCache(1 << 30), // 1 GiB.,
	}
	db, err := Open(dir, opts)
	require.NoError(t, err)

	// Ingest the tables into the DB. Tables are added in descending order of
	// level, in order to allow the tables to layer on top of each other. This
	// should mirror the state of the tables in the production DB.
	pathsToLevels := []struct {
		path  string
		level int
	}{
		{pathLockTableL6, 6},
		{pathLockTableL5, 5},
		{pathLockTableL4, 4},
		{pathLockTableL3, 3},
		{pathLockTableL2, 2},
		{pathGlobalTableL6, 6},
		{pathGlobalTableL5, 5},
	}
	for _, p := range pathsToLevels {
		var rewritePath string
		func() {
			// The SST first needs to be rewritten with zero seqnums.
			f, err := opts.FS.Open(p.path)
			require.NoError(t, err)
			fr, err := sstable.NewSimpleReadable(f)
			require.NoError(t, err)
			r, err := sstable.NewReader(fr, sstable.ReaderOptions{
				MergerName: opts.Merger.Name,
				Comparer:   EngineComparer,
			})
			require.NoError(t, err)
			defer r.Close()
			format, err := r.TableFormat()
			require.NoError(t, err)

			rewritePath = filepath.Join(dir, filepath.Base(p.path)+".new")
			f, err = opts.FS.Create(rewritePath)
			require.NoError(t, err)
			wOpts := DefaultPebbleOptions().MakeWriterOptions(0, format)
			wOpts.MergerName = "nullptr"
			fw := objstorageprovider.NewFileWritable(f)
			w := sstable.NewWriter(fw, wOpts)
			defer w.Close()

			// Point keys.
			iter, err := r.NewIter(nil, nil)
			require.NoError(t, err)
			defer iter.Close()
			var prev InternalKey
			for k, lv := iter.First(); k != nil; k, lv = iter.Next() {
				k.SetSeqNum(0)
				if wOpts.Comparer.Equal(prev.UserKey, k.UserKey) && prev.SeqNum() == k.SeqNum() {
					// This is a hack to get the test working. When we zero the seqnums,
					// two users keys that were previously ordered correctly will appear
					// to become out of order (i.e. foo#2,DEL foo#1,SET becomes foo#0,DEL
					// foo#0,SET, which is invalid). Instead, just drop the keys following
					// the first user key.
					continue
				}
				v, _, err := lv.Value(nil)
				require.NoError(t, err)
				err = w.Add(*k, v)
				require.NoError(t, err)
				prev = k.Clone()
			}

			// Range dels.
			fIter, err := r.NewRawRangeDelIter()
			require.NoError(t, err)
			if fIter != nil {
				defer fIter.Close()
				for s := fIter.First(); s != nil; s = fIter.Next() {
					err = w.DeleteRange(s.Start, s.End)
					require.NoError(t, err)
				}
			}
		}()

		// Ingest the rewritten SST. Force the level.
		_, err = db.ingest([]string{rewritePath}, forceLevelFunc(p.level))
		require.NoError(t, err)
	}

	// Assert that the levels are populated correctly.
	m := db.Metrics()
	require.Equal(t, 2, int(m.Levels[6].NumFiles))
	require.Equal(t, 2, int(m.Levels[5].NumFiles))
	require.Equal(t, 1, int(m.Levels[4].NumFiles))
	require.Equal(t, 1, int(m.Levels[3].NumFiles))
	require.Equal(t, 1, int(m.Levels[2].NumFiles))

	// Seek into the lock table.
	b, err := hex.DecodeString(seekKeyHex)
	require.NoError(t, err)
	t.ResetTimer()
	for i := 0; i < t.N; i++ {
		iter := db.NewIter(nil)
		iter.SeekPrefixGE(b)
		_ = iter.Close()
	}
}

func forceLevelFunc(level int) ingestTargetLevelFunc {
	return func(
		tableNewIters,
		keyspan.TableNewSpanIter,
		IterOptions,
		Compare,
		*version,
		int,
		map[*compaction]struct{},
		*fileMetadata,
	) (int, error) {
		return level, nil
	}
}
