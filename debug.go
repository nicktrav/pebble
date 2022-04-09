package pebble

import (
	"bytes"
	"fmt"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/humanize"
	"github.com/cockroachdb/pebble/internal/manifest"
)

type Spy struct {
	db *DB
}

// NewSpy returns a new instance of Spy.
func (d *DB) NewSpy() Spy {
	return Spy{db: d}
}

type SliceStats struct {
	Memtable   SliceLevelStats
	LevelStats [numLevels]SliceLevelStats
}

func (s SliceStats) Pretty(cmp base.Comparer) string {
	var buf bytes.Buffer
	// Memtables.
	fmt.Fprintf(&buf, "___level___count____size\n")
	fmt.Fprintf(&buf, "   mtbls %7d %7s", s.Memtable.Count, humanize.Uint64(s.Memtable.Size))
	if s.Memtable.Count > 0 {
		fmt.Fprintf(&buf, " smallest: %s", s.Memtable.Smallest.Pretty(cmp.FormatKey))
		fmt.Fprintf(&buf, " largest: %s", s.Memtable.Largest.Pretty(cmp.FormatKey))
	}
	fmt.Fprintf(&buf, "\n")

	// Levels.
	for level := range s.LevelStats {
		s := s.LevelStats[level]
		fmt.Fprintf(&buf, "%8d %7d %7s", level, s.Count, humanize.Uint64(s.Size))
		if s.Count > 0 {
			fmt.Fprintf(&buf, " smallest: %s", s.Smallest.Pretty(cmp.FormatKey))
			fmt.Fprintf(&buf, " largest: %s", s.Largest.Pretty(cmp.FormatKey))
		}
		fmt.Fprintf(&buf, "\n")
	}
	return buf.String()
}

type SliceLevelStats struct {
	Count    int
	Size     uint64
	Smallest InternalKey
	Largest  InternalKey
}

func (s Spy) SliceStats(start, end []byte, exclusiveEnd bool) SliceStats {
	rs := s.db.loadReadState()
	defer rs.unref()
	var stats SliceStats

	// Memtables.
	cmp := s.db.cmp
	for _, mem := range rs.memtables {
		var count int
		var size uint64
		var smallest, largest base.InternalKey
		func() {
			mem.readerRef()
			defer mem.readerUnref()
			iter := mem.newIter(nil)
			defer iter.Close()

			k, _ := iter.First()
			if k == nil {
				return
			}
			if c := cmp(k.UserKey, end); c > 0 || c == 0 && exclusiveEnd {
				return
			}
			first := *k
			k, _ = iter.Last()
			if c := cmp(k.UserKey, start); c < 0 || c == 0 && k.IsExclusiveSentinel() {
				return
			}
			last := *k
			if count == 0 {
				smallest = first.Clone()
				largest = last.Clone()
			} else {
				if base.InternalCompare(cmp, first, smallest) < 0 {
					smallest = first.Clone()
				}
				if base.InternalCompare(cmp, last, largest) > 0 {
					largest = last.Clone()
				}
			}
			count++
			size += mem.inuseBytes()
		}()
		stats.Memtable = SliceLevelStats{
			Count:    count,
			Size:     size,
			Smallest: smallest,
			Largest:  largest,
		}
	}

	v := rs.current
	for level := range v.Levels {
		overlaps := v.Overlaps(level, cmp, start, end, exclusiveEnd)
		if level == 0 {
			// L0 is sorted by seqnum, but we want an iterator ordered by start key.
			files := make([]*fileMetadata, overlaps.Len())
			iter := overlaps.Iter()
			for i, m := 0, iter.First(); m != nil; i, m = i+1, iter.Next() {
				files[i] = m
			}
			overlaps = manifest.NewLevelSliceKeySorted(cmp, files)
		}
		levelStats := SliceLevelStats{
			Count: overlaps.Len(),
			Size:  overlaps.SizeSum(),
		}
		if overlaps.Len() > 0 {
			iter := overlaps.Iter()
			levelStats.Smallest = iter.First().Smallest.Clone()
			levelStats.Largest = iter.Last().Largest.Clone()
		}
		stats.LevelStats[level] = levelStats
	}
	return stats
}
