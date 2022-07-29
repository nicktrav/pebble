// Copyright 2019 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package metamorphic

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/internal/errorfs"
	"github.com/cockroachdb/pebble/vfs"
)

type test struct {
	// The list of ops to execute. The ops refer to slots in the batches, iters,
	// and snapshots slices.
	ops []op
	idx int
	// The DB the test is run on.
	dir       string
	db        *pebble.DB
	opts      *pebble.Options
	testOpts  *testOptions
	writeOpts *pebble.WriteOptions
	tmpDir    string
	// The slots for the batches, iterators, and snapshots. These are read and
	// written by the ops to pass state from one op to another.
	batches   []*pebble.Batch
	iters     []*retryableIter
	snapshots []*pebble.Snapshot
}

func newTest(ops []op) *test {
	return &test{
		ops: ops,
	}
}

// writeLogicalState writes out the logical state (all visible KV pairs) of the
// DB to a new file at the provided path.
func (t *test) writeLogicalState(path string) error {
	if t.db != nil {
		return errors.Newf("metamorphic: expected DB to be closed; found DB")
	}
	opts := t.opts.Clone()
	opts.ReadOnly = true
	var err error
	t.db, err = pebble.Open(t.dir, opts)
	if err != nil {
		return err
	}
	defer t.db.Close()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	iter := t.db.NewIter(nil)
	defer iter.Close()
	for i, _ := 0, iter.First(); iter.Valid(); i, _ = i+1, iter.Next() {
		_, _ = fmt.Fprintf(w, "%s -> %s #%d\n", iter.Key(), iter.Value(), i)
	}
	return nil
}

func (t *test) init(h *history, dir string, testOpts *testOptions) error {
	t.dir = dir
	t.testOpts = testOpts
	t.writeOpts = pebble.NoSync
	if testOpts.strictFS {
		t.writeOpts = pebble.Sync
	}
	t.opts = testOpts.opts.EnsureDefaults()
	t.opts.Logger = h
	t.opts.EventListener = pebble.MakeLoggingEventListener(t.opts.Logger)
	t.opts.DebugCheck = func(db *pebble.DB) error {
		// Wrap the ordinary DebugCheckLevels with retrying
		// of injected errors.
		return withRetries(func() error {
			return pebble.DebugCheckLevels(db)
		})
	}

	// If an error occurs and we were using an in-memory FS, attempt to clone to
	// on-disk in order to allow post-mortem debugging. Note that always using
	// the on-disk FS isn't desirable because there is a large performance
	// difference between in-memory and on-disk which causes different code paths
	// and timings to be exercised.
	maybeExit := func(err error) {
		if err == nil || errors.Is(err, errorfs.ErrInjected) {
			return
		}
		t.maybeSaveData()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Exit early on any error from a background operation.
	t.opts.EventListener.BackgroundError = func(err error) {
		t.opts.Logger.Infof("background error: %s", err)
		maybeExit(err)
	}
	t.opts.EventListener.CompactionEnd = func(info pebble.CompactionInfo) {
		t.opts.Logger.Infof("%s", info)
		maybeExit(info.Err)
	}
	t.opts.EventListener.FlushEnd = func(info pebble.FlushInfo) {
		t.opts.Logger.Infof("%s", info)
		if info.Err != nil && !strings.Contains(info.Err.Error(), "pebble: empty table") {
			maybeExit(info.Err)
		}
	}
	t.opts.EventListener.ManifestCreated = func(info pebble.ManifestCreateInfo) {
		t.opts.Logger.Infof("%s", info)
		maybeExit(info.Err)
	}
	t.opts.EventListener.ManifestDeleted = func(info pebble.ManifestDeleteInfo) {
		t.opts.Logger.Infof("%s", info)
		maybeExit(info.Err)
	}
	t.opts.EventListener.TableDeleted = func(info pebble.TableDeleteInfo) {
		t.opts.Logger.Infof("%s", info)
		maybeExit(info.Err)
	}
	t.opts.EventListener.TableIngested = func(info pebble.TableIngestInfo) {
		t.opts.Logger.Infof("%s", info)
		maybeExit(info.Err)
	}
	t.opts.EventListener.WALCreated = func(info pebble.WALCreateInfo) {
		t.opts.Logger.Infof("%s", info)
		maybeExit(info.Err)
	}
	t.opts.EventListener.WALDeleted = func(info pebble.WALDeleteInfo) {
		t.opts.Logger.Infof("%s", info)
		maybeExit(info.Err)
	}

	var db *pebble.DB
	var err error
	err = withRetries(func() error {
		db, err = pebble.Open(dir, t.opts)
		return err
	})
	if err != nil {
		return err
	}
	h.Recordf("db.Open() // %v", err)

	t.tmpDir = t.opts.FS.PathJoin(dir, "tmp")
	if err = t.opts.FS.MkdirAll(t.tmpDir, 0755); err != nil {
		return err
	}
	if t.testOpts.strictFS {
		// Sync the whole directory path for the tmpDir, since restartDB() is executed during
		// the test. That would reset MemFS to the synced state, which would make an unsynced
		// directory disappear in the middle of the test. It is the responsibility of the test
		// (not Pebble) to ensure that it can write the ssts that it will subsequently ingest
		// into Pebble.
		for {
			f, err := t.opts.FS.OpenDir(dir)
			if err != nil {
				return err
			}
			if err = f.Sync(); err != nil {
				return err
			}
			if err = f.Close(); err != nil {
				return err
			}
			if len(dir) == 1 {
				break
			}
			dir = t.opts.FS.PathDir(dir)
			// TODO(sbhola): PathDir returns ".", which OpenDir() complains about. Fix.
			if len(dir) == 1 {
				dir = "/"
			}
		}
	}

	t.db = db
	return nil
}

func (t *test) restartDB() error {
	if !t.testOpts.strictFS {
		return nil
	}
	t.opts.Cache.Ref()
	// The fs isn't necessarily a MemFS.
	fs, ok := vfs.Root(t.opts.FS).(*vfs.MemFS)
	if ok {
		fs.SetIgnoreSyncs(true)
	}
	if err := t.db.Close(); err != nil {
		return err
	}
	if ok {
		fs.ResetToSyncedState()
		fs.SetIgnoreSyncs(false)
	}
	err := withRetries(func() (err error) {
		t.db, err = pebble.Open(t.dir, t.opts)
		return err
	})
	t.opts.Cache.Unref()
	return err
}

// If an in-memory FS is being used, save the contents to disk.
func (t *test) maybeSaveData() {
	rootFS := vfs.Root(t.opts.FS)
	if rootFS == vfs.Default {
		return
	}
	_ = os.RemoveAll(t.dir)
	if _, err := vfs.Clone(rootFS, vfs.Default, t.dir, t.dir); err != nil {
		t.opts.Logger.Infof("unable to clone: %s: %v", t.dir, err)
	}
}

func (t *test) step(h *history) bool {
	if t.idx >= len(t.ops) {
		return false
	}
	t.ops[t.idx].run(t, h)
	t.idx++
	return true
}

func (t *test) setBatch(id objID, b *pebble.Batch) {
	if id.tag() != batchTag {
		panic(fmt.Sprintf("invalid batch ID: %s", id))
	}
	t.batches[id.slot()] = b
}

func (t *test) setIter(id objID, i *pebble.Iterator, filterMin, filterMax uint64) {
	if id.tag() != iterTag {
		panic(fmt.Sprintf("invalid iter ID: %s", id))
	}
	t.iters[id.slot()] = &retryableIter{
		iter:      i,
		lastKey:   nil,
		filterMin: filterMin,
		filterMax: filterMax,
	}
}

func (t *test) setSnapshot(id objID, s *pebble.Snapshot) {
	if id.tag() != snapTag {
		panic(fmt.Sprintf("invalid snapshot ID: %s", id))
	}
	t.snapshots[id.slot()] = s
}

func (t *test) clearObj(id objID) {
	switch id.tag() {
	case dbTag:
		t.db = nil
	case batchTag:
		t.batches[id.slot()] = nil
	case iterTag:
		t.iters[id.slot()] = nil
	case snapTag:
		t.snapshots[id.slot()] = nil
	}
}

func (t *test) getBatch(id objID) *pebble.Batch {
	if id.tag() != batchTag {
		panic(fmt.Sprintf("invalid batch ID: %s", id))
	}
	return t.batches[id.slot()]
}

func (t *test) getCloser(id objID) io.Closer {
	switch id.tag() {
	case dbTag:
		return t.db
	case batchTag:
		return t.batches[id.slot()]
	case iterTag:
		return t.iters[id.slot()]
	case snapTag:
		return t.snapshots[id.slot()]
	}
	panic(fmt.Sprintf("cannot close ID: %s", id))
}

func (t *test) getIter(id objID) *retryableIter {
	if id.tag() != iterTag {
		panic(fmt.Sprintf("invalid iter ID: %s", id))
	}
	return t.iters[id.slot()]
}

func (t *test) getReader(id objID) pebble.Reader {
	switch id.tag() {
	case dbTag:
		return t.db
	case batchTag:
		return t.batches[id.slot()]
	case snapTag:
		return t.snapshots[id.slot()]
	}
	panic(fmt.Sprintf("invalid reader ID: %s", id))
}

func (t *test) getWriter(id objID) pebble.Writer {
	switch id.tag() {
	case dbTag:
		return t.db
	case batchTag:
		return t.batches[id.slot()]
	}
	panic(fmt.Sprintf("invalid writer ID: %s", id))
}
