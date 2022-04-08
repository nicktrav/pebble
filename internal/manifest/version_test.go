// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package manifest

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/datadriven"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/redact"
	"github.com/stretchr/testify/require"
)

func levelMetadata(level int, files ...*FileMetadata) LevelMetadata {
	return makeLevelMetadata(base.DefaultComparer.Compare, level, files)
}

func ikey(s string) InternalKey {
	return base.MakeInternalKey([]byte(s), 0, base.InternalKeyKindSet)
}

func TestIkeyRange(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	testCases := []struct {
		input, want string
	}{
		{
			"",
			"-",
		},
		{
			"a-e",
			"a-e",
		},
		{
			"a-e a-e",
			"a-e",
		},
		{
			"c-g a-e",
			"a-g",
		},
		{
			"a-e c-g a-e",
			"a-g",
		},
		{
			"b-d f-g",
			"b-g",
		},
		{
			"d-e b-d",
			"b-e",
		},
		{
			"e-e",
			"e-e",
		},
		{
			"f-g e-e d-e c-g b-d a-e",
			"a-g",
		},
	}
	for _, tc := range testCases {
		var f []*FileMetadata
		if tc.input != "" {
			for i, s := range strings.Split(tc.input, " ") {
				m := (&FileMetadata{
					FileNum: base.FileNum(i),
				}).ExtendPointKeyBounds(cmp, ikey(s[0:1]), ikey(s[2:3]))
				f = append(f, m)
			}
		}
		levelMetadata := makeLevelMetadata(base.DefaultComparer.Compare, 0, f)

		sm, la := KeyRange(base.DefaultComparer.Compare, levelMetadata.Iter())
		got := string(sm.UserKey) + "-" + string(la.UserKey)
		if got != tc.want {
			t.Errorf("KeyRange(%q) = %q, %q", tc.input, got, tc.want)
		}
	}
}

func BenchmarkL0Overlaps_Random(b *testing.B) {
	cmp := base.DefaultComparer.Compare
	seed := int64(1649432491)
	rng := rand.New(rand.NewSource(seed))
	keyspace := testkeys.Alpha(10)

	type params struct {
		lm                LevelMetadata
		smallest, largest []byte
	}

	makeParams := func(n int) params {
		files := make([]*FileMetadata, n)

		for i := range files {
			m := &FileMetadata{
				FileNum: base.FileNum(i),
			}
			smallest := testkeys.Key(keyspace, rng.Intn(keyspace.MaxLen()))
			largest := testkeys.Key(keyspace, rng.Intn(keyspace.MaxLen()))
			if cmp(smallest, largest) > 0 {
				smallest, largest = largest, smallest
			}
			smallestKey := base.MakeInternalKey(smallest, 0, base.InternalKeyKindSet)
			largestKey := base.MakeInternalKey(largest, 0, base.InternalKeyKindSet)
			m.ExtendPointKeyBounds(cmp, smallestKey, largestKey)
			files[i] = m
		}

		// Pick a random bounds.
		smallest := testkeys.Key(keyspace, rng.Intn(keyspace.MaxLen()))
		largest := testkeys.Key(keyspace, rng.Intn(keyspace.MaxLen()))
		if cmp(smallest, largest) > 0 {
			smallest, largest = largest, smallest
		}

		lm := makeLevelMetadata(cmp, 0, files)

		return params{lm, smallest, largest}
	}

	// Construct the test params.
	fileCounts := []int{10, 25, 50, 100, 1000}
	//fileCounts := []int{10}
	for _, c := range fileCounts {
		name := fmt.Sprintf("count=%d", c)
		b.Run(name, func(b *testing.B) {
			pprofPath := fmt.Sprintf("/tmp/%s.pprof", name)
			w, err := os.OpenFile(pprofPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if err != nil {
				b.Fatal(err)
			}
			var ps []params
			for i := 0; i < b.N; i++ {
				ps = append(ps, makeParams(c))
			}
			err = pprof.StartCPUProfile(w)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p := ps[i]
				_ = l0Overlaps(p.lm, base.DefaultComparer.Compare, p.smallest, p.largest, rng.Intn(2) == 0)
			}
			pprof.StopCPUProfile()
		})
	}
}

func BenchmarkL0Overlaps_WorstCase(b *testing.B) {
	// Construct a worst-case L0, that resembles the following, but scaled out to
	// a wider keyspace. The upper bounds on each file are *exclusive* (i.e.
	// exclusive sentinel keys). The numbers inside the boxes indicate iteration
	// order within L0.
	//
	// The original algorithm was iterative. Starting with file 001, no files
	// overlap until files 020 and 021, the bounds would extend, and the loop
	// would need to repeat. The next loop iteration picks up 002 and 003, and 018
	// and 019, and the loop repeats. The loop continues until all files are
	// chosen.
	//
	//                    |019|020|
	//                |017|       |018|
	//            |015|               |016|
	//        |013|                       |014|
	//    |011|                               |012|
	//  |009|                                   |010|
	//      |007|                           |008|
	//          |005|                   |006|
	//              |003|           |004|
	//                  |001|   |002|
	//                      |000|
	//  a   c   e   g   i   k   m   o   q   s   u   w
	//                      |   |
	//  0   2   4   6   8   10  12  14  16  18  20  22

	makeMetadata := func(nChars int) (LevelMetadata, []byte) {
		nFiles := nChars - 2

		alpha := testkeys.Alpha(10)
		key := func(i int) []byte {
			return testkeys.Key(alpha, i)
		}

		makeFile := func(start, end []byte, filenum int) *FileMetadata {
			m := &FileMetadata{FileNum: base.FileNum(filenum)}
			sKey := base.MakeInternalKey(start, 0, base.InternalKeyKindSet)
			eKey := base.MakeExclusiveSentinelKey(base.InternalKeyKindRangeDelete, end)
			m.ExtendPointKeyBounds(base.DefaultComparer.Compare, sKey, eKey)
			return m
		}

		files := make([]*FileMetadata, nFiles)
		files[0] = makeFile(key(nChars/2-1), key(nChars/2+1), 0)
		for i := 0; i < (nFiles-1)/4; i++ {
			files[2*i+1] = makeFile(key(nChars/2-2*i-3), key(nChars/2-2*i-1), 2*i+1)             // Bottom left.
			files[2*i+2] = makeFile(key(nChars/2+2*i+1), key(nChars/2+2*i+3), 2*i+2)             // Bottom right.
			files[nFiles-2*i-2] = makeFile(key(nChars/2-2*i-2), key(nChars/2-2*i), nFiles-2*i-2) // Top left.
			files[nFiles-2*i-1] = makeFile(key(nChars/2+2*i), key(nChars/2+2*i+2), nFiles-2*i-1) // Top right.
		}

		return makeLevelMetadata(base.DefaultComparer.Compare, 0, files), key(nChars / 2)
	}

	charCounts := []int{7, 11, 15, 19, 23, 207, 407, 4007}
	for _, c := range charCounts {
		b.Run(fmt.Sprintf("chars=%d", c), func(b *testing.B) {
			lm, mid := makeMetadata(c)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s := l0Overlaps(lm, base.DefaultComparer.Compare, mid, mid, false)
				require.Equal(b, c-2, s.length)
			}
		})
	}
}

func TestOverlaps(t *testing.T) {
	var v *Version
	cmp := testkeys.Comparer.Compare
	fmtKey := testkeys.Comparer.FormatKey
	datadriven.RunTest(t, "testdata/overlaps", func(d *datadriven.TestData) string {
		switch d.Cmd {
		case "define":
			var err error
			v, err = ParseVersionDebug(cmp, fmtKey, 64>>10 /* flush split bytes */, d.Input)
			if err != nil {
				return err.Error()
			}
			return v.String()
		case "overlaps":
			var level int
			var start, end string
			var exclusiveEnd bool
			d.ScanArgs(t, "level", &level)
			d.ScanArgs(t, "start", &start)
			d.ScanArgs(t, "end", &end)
			d.ScanArgs(t, "exclusive-end", &exclusiveEnd)
			var buf bytes.Buffer
			v.Overlaps(level, testkeys.Comparer.Compare, []byte(start), []byte(end), exclusiveEnd).Each(func(f *FileMetadata) {
				fmt.Fprintf(&buf, "%s\n", f.DebugString(base.DefaultFormatter, false))
			})
			return buf.String()
		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}

func TestContains(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	newFileMeta := func(fileNum base.FileNum, size uint64, smallest, largest base.InternalKey) *FileMetadata {
		m := (&FileMetadata{
			FileNum: fileNum,
			Size:    size,
		}).ExtendPointKeyBounds(cmp, smallest, largest)
		return m
	}
	m00 := newFileMeta(
		700,
		1,
		base.ParseInternalKey("b.SET.7008"),
		base.ParseInternalKey("e.SET.7009"),
	)
	m01 := newFileMeta(
		701,
		1,
		base.ParseInternalKey("c.SET.7018"),
		base.ParseInternalKey("f.SET.7019"),
	)
	m02 := newFileMeta(
		702,
		1,
		base.ParseInternalKey("f.SET.7028"),
		base.ParseInternalKey("g.SET.7029"),
	)
	m03 := newFileMeta(
		703,
		1,
		base.ParseInternalKey("x.SET.7038"),
		base.ParseInternalKey("y.SET.7039"),
	)
	m04 := newFileMeta(
		704,
		1,
		base.ParseInternalKey("n.SET.7048"),
		base.ParseInternalKey("p.SET.7049"),
	)
	m05 := newFileMeta(
		705,
		1,
		base.ParseInternalKey("p.SET.7058"),
		base.ParseInternalKey("p.SET.7059"),
	)
	m06 := newFileMeta(
		706,
		1,
		base.ParseInternalKey("p.SET.7068"),
		base.ParseInternalKey("u.SET.7069"),
	)
	m07 := newFileMeta(
		707,
		1,
		base.ParseInternalKey("r.SET.7078"),
		base.ParseInternalKey("s.SET.7079"),
	)

	m10 := newFileMeta(
		710,
		1,
		base.ParseInternalKey("d.SET.7108"),
		base.ParseInternalKey("g.SET.7109"),
	)
	m11 := newFileMeta(
		711,
		1,
		base.ParseInternalKey("g.SET.7118"),
		base.ParseInternalKey("j.SET.7119"),
	)
	m12 := newFileMeta(
		712,
		1,
		base.ParseInternalKey("n.SET.7128"),
		base.ParseInternalKey("p.SET.7129"),
	)
	m13 := newFileMeta(
		713,
		1,
		base.ParseInternalKey("p.SET.7148"),
		base.ParseInternalKey("p.SET.7149"),
	)
	m14 := newFileMeta(
		714,
		1,
		base.ParseInternalKey("p.SET.7138"),
		base.ParseInternalKey("u.SET.7139"),
	)

	v := Version{
		Levels: [NumLevels]LevelMetadata{
			0: levelMetadata(0, m00, m01, m02, m03, m04, m05, m06, m07),
			1: levelMetadata(1, m10, m11, m12, m13, m14),
		},
	}

	testCases := []struct {
		level int
		file  *FileMetadata
		want  bool
	}{
		// Level 0: m00=b-e, m01=c-f, m02=f-g, m03=x-y, m04=n-p, m05=p-p, m06=p-u, m07=r-s.
		// Note that:
		//   - the slice isn't sorted (e.g. m02=f-g, m03=x-y, m04=n-p),
		//   - m00 and m01 overlap (not just touch),
		//   - m06 contains m07,
		//   - m00, m01 and m02 transitively overlap/touch each other, and
		//   - m04, m05, m06 and m07 transitively overlap/touch each other.
		{0, m00, true},
		{0, m01, true},
		{0, m02, true},
		{0, m03, true},
		{0, m04, true},
		{0, m05, true},
		{0, m06, true},
		{0, m07, true},
		{0, m10, false},
		{0, m11, false},
		{0, m12, false},
		{0, m13, false},
		{0, m14, false},
		{1, m00, false},
		{1, m01, false},
		{1, m02, false},
		{1, m03, false},
		{1, m04, false},
		{1, m05, false},
		{1, m06, false},
		{1, m07, false},
		{1, m10, true},
		{1, m11, true},
		{1, m12, true},
		{1, m13, true},
		{1, m14, true},

		// Level 2: empty.
		{2, m00, false},
		{2, m14, false},
	}

	for _, tc := range testCases {
		got := v.Contains(tc.level, cmp, tc.file)
		if got != tc.want {
			t.Errorf("level=%d, file=%s\ngot %t\nwant %t", tc.level, tc.file, got, tc.want)
		}
	}
}

func TestVersionUnref(t *testing.T) {
	list := &VersionList{}
	list.Init(&sync.Mutex{})
	v := &Version{Deleted: func([]*FileMetadata) {}}
	v.Ref()
	list.PushBack(v)
	v.Unref()
	if !list.Empty() {
		t.Fatalf("expected version list to be empty")
	}
}

func TestCheckOrdering(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	fmtKey := base.DefaultComparer.FormatKey
	datadriven.RunTest(t, "testdata/version_check_ordering",
		func(d *datadriven.TestData) string {
			switch d.Cmd {
			case "check-ordering":
				v, err := ParseVersionDebug(cmp, fmtKey, 10<<20, d.Input)
				if err != nil {
					return err.Error()
				}
				// L0 files compare on sequence numbers. Use the seqnums from the
				// smallest / largest bounds for the table.
				v.Levels[0].Slice().Each(func(m *FileMetadata) {
					m.SmallestSeqNum = m.Smallest.SeqNum()
					m.LargestSeqNum = m.Largest.SeqNum()
				})
				if err = v.CheckOrdering(cmp, base.DefaultFormatter); err != nil {
					return err.Error()
				}
				return "OK"

			default:
				return fmt.Sprintf("unknown command: %s", d.Cmd)
			}
		})
}

func TestCheckConsistency(t *testing.T) {
	const dir = "./test"
	mem := vfs.NewMem()
	mem.MkdirAll(dir, 0755)

	cmp := base.DefaultComparer.Compare
	fmtKey := base.DefaultComparer.FormatKey
	parseMeta := func(s string) (*FileMetadata, error) {
		if len(s) == 0 {
			return nil, nil
		}
		parts := strings.Split(s, ":")
		if len(parts) != 2 {
			return nil, errors.Errorf("malformed table spec: %q", s)
		}
		fileNum, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, err
		}
		return &FileMetadata{
			FileNum: base.FileNum(fileNum),
			Size:    uint64(size),
		}, nil
	}

	datadriven.RunTest(t, "testdata/version_check_consistency",
		func(d *datadriven.TestData) string {
			switch d.Cmd {
			case "check-consistency":
				var filesByLevel [NumLevels][]*FileMetadata
				var files *[]*FileMetadata

				for _, data := range strings.Split(d.Input, "\n") {
					switch data {
					case "L0", "L1", "L2", "L3", "L4", "L5", "L6":
						level, err := strconv.Atoi(data[1:])
						if err != nil {
							return err.Error()
						}
						files = &filesByLevel[level]

					default:
						m, err := parseMeta(data)
						if err != nil {
							return err.Error()
						}
						if m != nil {
							*files = append(*files, m)
						}
					}
				}

				redactErr := false
				for _, arg := range d.CmdArgs {
					switch v := arg.String(); v {
					case "redact":
						redactErr = true
					default:
						return fmt.Sprintf("unknown argument: %q", v)
					}
				}

				v := NewVersion(cmp, fmtKey, 0, filesByLevel)
				err := v.CheckConsistency(dir, mem)
				if err != nil {
					if redactErr {
						redacted := redact.Sprint(err).Redact()
						return string(redacted)
					}
					return err.Error()
				}
				return "OK"

			case "build":
				for _, data := range strings.Split(d.Input, "\n") {
					m, err := parseMeta(data)
					if err != nil {
						return err.Error()
					}
					path := base.MakeFilepath(mem, dir, base.FileTypeTable, m.FileNum)
					_ = mem.Remove(path)
					f, err := mem.Create(path)
					if err != nil {
						return err.Error()
					}
					_, err = f.Write(make([]byte, m.Size))
					if err != nil {
						return err.Error()
					}
					f.Close()
				}
				return ""

			default:
				return fmt.Sprintf("unknown command: %s", d.Cmd)
			}
		})
}

func TestExtendBounds(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	parseBounds := func(line string) (lower, upper InternalKey) {
		parts := strings.Split(line, "-")
		if len(parts) == 1 {
			parts = strings.Split(parts[0], ":")
			start, end := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			lower = base.ParseInternalKey(start)
			switch k := lower.Kind(); k {
			case base.InternalKeyKindRangeDelete:
				upper = base.MakeRangeDeleteSentinelKey([]byte(end))
			case base.InternalKeyKindRangeKeySet, base.InternalKeyKindRangeKeyUnset, base.InternalKeyKindRangeKeyDelete:
				upper = base.MakeExclusiveSentinelKey(k, []byte(end))
			default:
				panic(fmt.Sprintf("unknown kind %s with end key", k))
			}
		} else {
			l, u := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			lower, upper = base.ParseInternalKey(l), base.ParseInternalKey(u)
		}
		return
	}
	format := func(m *FileMetadata) string {
		var b bytes.Buffer
		var smallest, largest string
		switch m.boundTypeSmallest {
		case boundTypePointKey:
			smallest = "point"
		case boundTypeRangeKey:
			smallest = "range"
		default:
			return fmt.Sprintf("unknown bound type %d", m.boundTypeSmallest)
		}
		switch m.boundTypeLargest {
		case boundTypePointKey:
			largest = "point"
		case boundTypeRangeKey:
			largest = "range"
		default:
			return fmt.Sprintf("unknown bound type %d", m.boundTypeLargest)
		}
		bounds, err := m.boundsMarker()
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, "%s\n", m.DebugString(base.DefaultFormatter, true))
		fmt.Fprintf(&b, "  bounds: (smallest=%s,largest=%s) (0x%08b)\n", smallest, largest, bounds)
		return b.String()
	}
	m := &FileMetadata{}
	datadriven.RunTest(t, "testdata/file_metadata_bounds", func(d *datadriven.TestData) string {
		switch d.Cmd {
		case "reset":
			m = &FileMetadata{}
			return ""
		case "extend-point-key-bounds":
			u, l := parseBounds(d.Input)
			m.ExtendPointKeyBounds(cmp, u, l)
			return format(m)
		case "extend-range-key-bounds":
			u, l := parseBounds(d.Input)
			m.ExtendRangeKeyBounds(cmp, u, l)
			return format(m)
		default:
			return fmt.Sprintf("unknown command %s\n", d.Cmd)
		}
	})
}

func TestFileMetadata_ParseRoundTrip(t *testing.T) {
	testCases := []struct {
		name   string
		input  string
		output string
	}{
		{
			name:  "point keys only",
			input: "000001:[a#0,SET-z#0,DEL] points:[a#0,SET-z#0,DEL]",
		},
		{
			name:  "range keys only",
			input: "000001:[a#0,RANGEKEYSET-z#0,RANGEKEYDEL] ranges:[a#0,RANGEKEYSET-z#0,RANGEKEYDEL]",
		},
		{
			name:  "point and range keys",
			input: "000001:[a#0,RANGEKEYSET-d#0,DEL] points:[b#0,SET-d#0,DEL] ranges:[a#0,RANGEKEYSET-c#0,RANGEKEYDEL]",
		},
		{
			name:   "whitespace",
			input:  " 000001 : [ a#0,SET - z#0,DEL] points : [ a#0,SET - z#0,DEL] ",
			output: "000001:[a#0,SET-z#0,DEL] points:[a#0,SET-z#0,DEL]",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseFileMetadataDebug(tc.input)
			require.NoError(t, err)
			err = m.Validate(base.DefaultComparer.Compare, base.DefaultFormatter)
			require.NoError(t, err)
			got := m.DebugString(base.DefaultFormatter, true)
			want := tc.input
			if tc.output != "" {
				want = tc.output
			}
			require.Equal(t, want, got)
		})
	}
}
