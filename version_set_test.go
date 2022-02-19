// Copyright 2020 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/rangekey"
	"github.com/cockroachdb/pebble/record"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func writeAndIngest(t *testing.T, mem vfs.FS, d *DB, k InternalKey, v []byte, filename string) {
	path := mem.PathJoin("ext", filename)
	f, err := mem.Create(path)
	require.NoError(t, err)
	opts := sstable.WriterOptions{}
	if rangekey.IsRangeKey(k.Kind()) {
		opts.TableFormat = sstable.TableFormatPebblev2
	}
	w := sstable.NewWriter(f, opts)
	if rangekey.IsRangeKey(k.Kind()) {
		require.NoError(t, w.AddRangeKey(k, v))
	} else {
		require.NoError(t, w.Add(k, v))
	}
	require.NoError(t, w.Close())
	require.NoError(t, d.Ingest([]string{path}))
}

func TestVersionSetCheckpoint(t *testing.T) {
	mem := vfs.NewMem()
	require.NoError(t, mem.MkdirAll("ext", 0755))

	opts := &Options{
		FS:                  mem,
		MaxManifestFileSize: 1,
	}
	d, err := Open("", opts)
	require.NoError(t, err)

	// Multiple manifest files are created such that the latest one must have a correct snapshot
	// of the preceding state for the DB to be opened correctly and see the written data.
	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("a"), 0, InternalKeyKindSet), []byte("b"), "a")
	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("c"), 0, InternalKeyKindSet), []byte("d"), "c")
	require.NoError(t, d.Close())
	d, err = Open("", opts)
	require.NoError(t, err)
	checkValue := func(k string, expected string) {
		v, closer, err := d.Get([]byte(k))
		require.NoError(t, err)
		require.Equal(t, expected, string(v))
		closer.Close()
	}
	checkValue("a", "b")
	checkValue("c", "d")
	require.NoError(t, d.Close())
}

func TestVersionSetSeqNums(t *testing.T) {
	mem := vfs.NewMem()
	require.NoError(t, mem.MkdirAll("ext", 0755))

	opts := &Options{
		FS:                  mem,
		MaxManifestFileSize: 1,
	}
	d, err := Open("", opts)
	require.NoError(t, err)

	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("a"), 0, InternalKeyKindSet), []byte("b"), "a")
	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("c"), 0, InternalKeyKindSet), []byte("d"), "c")
	require.NoError(t, d.Close())
	d, err = Open("", opts)
	require.NoError(t, err)
	defer d.Close()

	// Check that the manifest has the correct LastSeqNum, equalling the highest
	// observed SeqNum.
	filenames, err := mem.List("")
	require.NoError(t, err)
	var manifest vfs.File
	for _, filename := range filenames {
		fileType, _, ok := base.ParseFilename(mem, filename)
		if ok && fileType == fileTypeManifest {
			manifest, err = mem.Open(filename)
			require.NoError(t, err)
		}
	}
	require.NotNil(t, manifest)
	defer manifest.Close()
	rr := record.NewReader(manifest, 0 /* logNum */)
	lastSeqNum := uint64(0)
	for {
		r, err := rr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var ve versionEdit
		err = ve.Decode(r)
		require.NoError(t, err)
		if ve.LastSeqNum != 0 {
			lastSeqNum = ve.LastSeqNum
		}
	}
	// 2 ingestions happened, so LastSeqNum should equal 2.
	require.Equal(t, uint64(2), lastSeqNum)
	// logSeqNum is always one greater than the last assigned sequence number.
	require.Equal(t, d.mu.versions.atomic.logSeqNum, lastSeqNum+1)
}

func TestVersionSet_Compatability(t *testing.T) {
	// Ensure forwards compatability with range keys. Specifically, version edits
	// written by a DB at a version prior to FormatRangeKeys should be readable by
	// newer versions of the DB.
	mem := vfs.NewMem()
	require.NoError(t, mem.MkdirAll("ext", 0755))

	opts := &Options{
		FS:                 mem,
		FormatMajorVersion: FormatRangeKeys - 1,
	}
	d, err := Open("", opts)
	require.NoError(t, err)

	// Ingest two files into the DB.
	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("a"), 0, InternalKeyKindSet), []byte("a"), "a")
	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("b"), 0, InternalKeyKindSet), []byte("b"), "b")

	// Close the DB.
	require.NoError(t, d.Close())

	// Bump the format major version to support range keys.
	opts.FormatMajorVersion = FormatRangeKeys
	d, err = Open("", opts)
	require.NoError(t, err)
	defer d.Close()

	// Write two more files, one that contains a range key, one that contains a
	// point key.
	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("a"), 0, InternalKeyKindRangeKeyDelete), []byte("z"), "a-rk")
	writeAndIngest(t, mem, d, base.MakeInternalKey([]byte("c"), 0, InternalKeyKindSet), []byte("c"), "c")

	// Collect manifest files.
	var manifests []string
	filenames, err := mem.List("")
	require.NoError(t, err)
	for _, filename := range filenames {
		fileType, _, ok := base.ParseFilename(mem, filename)
		if fileType != fileTypeManifest {
			continue
		}
		if !ok {
			t.Fatalf("could not parse file %s", filenames)
		}
		manifests = append(manifests, filename)
	}

	// The ordering of the filenames is not stable for a MemFS.
	sort.Strings(manifests)

	// Read the manifest and collect the metadata for each file added. The last
	// file is the most recent manifest.
	m, err := mem.Open(manifests[len(manifests)-1])
	require.NoError(t, err)

	rr := record.NewReader(m, 0 /* logNum */)
	var ranges []string
	for {
		r, err := rr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var ve versionEdit
		err = ve.Decode(r)
		require.NoError(t, err)

		// For each new file, collects its ranges.
		for _, f := range ve.NewFiles {
			var b bytes.Buffer
			meta := f.Meta
			fmt.Fprintf(&b, "%s:\n", f.Meta.FileNum)
			fmt.Fprintf(&b, "  combined: [%s-%s]\n", meta.Smallest, meta.Largest)
			fmt.Fprintf(&b, "    points: [%s-%s]\n", meta.SmallestPointKey, meta.LargestPointKey)
			fmt.Fprintf(&b, "    ranges: [%s-%s]", meta.SmallestRangeKey, meta.LargestRangeKey)
			ranges = append(ranges, b.String())
		}
	}

	const want = `000004:
  combined: [a#1,1-a#1,1]
    points: [a#1,1-a#1,1]
    ranges: [#0,0-#0,0]
000005:
  combined: [b#2,1-b#2,1]
    points: [b#2,1-b#2,1]
    ranges: [#0,0-#0,0]
000009:
  combined: [a#3,19-z#72057594037927935,21]
    points: [#0,0-#0,0]
    ranges: [a#3,19-z#72057594037927935,21]
000010:
  combined: [c#4,1-c#4,1]
    points: [c#4,1-c#4,1]
    ranges: [#0,0-#0,0]`

	// Confirm that ranges for the four tables is as expected.
	require.Equal(t, want, strings.Join(ranges, "\n"))
}
