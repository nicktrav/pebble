package pebble

import (
	"fmt"
	"github.com/cockroachdb/pebble/internal/base"
	"testing"

	"github.com/cockroachdb/pebble/internal/humanize"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func Test(t *testing.T) {
	db, err := Open("", &Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)
	defer db.Close()

	start, end := []byte("\x00"), []byte("\xff")

	spy := db.NewSpy()
	printStats := func() {
		var count int
		var size uint64
		var smallest, largest base.InternalKey
		update := func(stats SliceLevelStats) {
			if stats.Count == 0 {
				return
			}
			if count == 0 || base.InternalCompare(db.cmp, stats.Smallest, smallest) < 0 {
				smallest = stats.Smallest
			}
			if count == 0 || base.InternalCompare(db.cmp, stats.Largest, largest) > 0 {
				largest = stats.Largest
			}
			count += stats.Count
			size += stats.Size
		}

		stats := spy.SliceStats(start, end, false)

		// Memtables.
		fmt.Printf("___level___count____size\n")
		fmt.Printf("   mtbls %7d %7s", stats.Memtable.Count, humanize.Uint64(stats.Memtable.Size))
		if stats.Memtable.Count > 0 {
			fmt.Printf(" smallest: %s", stats.Memtable.Smallest.Pretty(db.opts.Comparer.FormatKey))
			fmt.Printf(" largest: %s", stats.Memtable.Largest.Pretty(db.opts.Comparer.FormatKey))
		}
		fmt.Print("\n")
		update(stats.Memtable)

		// Levels.
		for level := range stats.LevelStats {
			s := stats.LevelStats[level]
			fmt.Printf("%8d %7d %7s", level, s.Count, humanize.Uint64(s.Size))
			if s.Count > 0 {
				fmt.Printf(" smallest: %s", s.Smallest.Pretty(db.opts.Comparer.FormatKey))
				fmt.Printf(" largest: %s", s.Largest.Pretty(db.opts.Comparer.FormatKey))
			}
			fmt.Print("\n")
			update(s)
		}

		// Totals.
		fmt.Printf("   total %7d %7s", count, humanize.Uint64(size))
		fmt.Printf(" smallest: %s", smallest.Pretty(db.opts.Comparer.FormatKey))
		fmt.Printf(" largest: %s\n", largest.Pretty(db.opts.Comparer.FormatKey))
	}

	// Put some keys in the DB.
	keyspace := testkeys.Alpha(1)
	for i := 0; i < keyspace.Count(); i++ {
		b := db.NewBatch()
		_ = b.Set(testkeys.Key(keyspace, i), nil, nil)
		err = b.Commit(nil)
		require.NoError(t, err)
	}

	// There shouldn't be any overlaps, as nothing is in an sstable yet.
	fmt.Println("before flush:")
	printStats()

	// Force a flush.
	err = db.Flush()
	require.NoError(t, err)

	fmt.Println("after flush 1:")
	printStats()

	for i := 0; i < keyspace.Count(); i++ {
		b := db.NewBatch()
		_ = b.Set(testkeys.Key(keyspace, i), nil, nil)
		err = b.Commit(nil)
		require.NoError(t, err)
	}

	fmt.Println("after commit 2:")
	printStats()
}
