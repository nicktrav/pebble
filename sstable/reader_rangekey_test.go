package sstable

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/datadriven"
	"github.com/stretchr/testify/require"
)

func TestReader_RangeKeys_Iter(t *testing.T) {
	var r *Reader
	defer func() {
		if r != nil {
			require.NoError(t, r.Close())
		}
	}()

	datadriven.RunTest(t, "testdata/reader_range_keys", func(td *datadriven.TestData) string {
		switch td.Cmd {
		case "build":
			if r != nil {
				_ = r.Close()
				r = nil
			}

			var err error
			r, err = runBuildRangeKeys(td)
			if err != nil {
				return err.Error()
			}
			return ""

		case "scan":
			var filterFn func(k *InternalKey) bool
			for _, cmdArg := range td.CmdArgs {
				switch cmd := cmdArg.Key; cmd {
				case "filter-key-kinds":
					kinds := make(map[InternalKeyKind]bool)
					for _, s := range cmdArg.Vals {
						switch s {
						case "range-key-set":
							kinds[base.InternalKeyKindRangeKeySet] = true
						case "range-key-unset":
							kinds[base.InternalKeyKindRangeKeyUnset] = true
						case "range-key-del":
							kinds[base.InternalKeyKindRangeKeyDelete] = true
						default:
							return fmt.Sprintf("unknown key kind: %s", s)
						}
					}
					filterFn = func(k *InternalKey) bool { return kinds[k.Kind()] }
				default:
					return fmt.Sprintf("unknown command: %s", cmd)
				}
			}
			iter, err := r.NewRawRangeKeyIterFilter(filterFn)
			if err != nil {
				return err.Error()
			}
			defer iter.Close()

			var buf bytes.Buffer
			for s := iter.First(); s.Valid(); s = iter.Next() {
				_, _ = fmt.Fprintf(&buf, "%s\n", s)
			}
			return buf.String()

		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func Test(t *testing.T) {
	m := make(map[InternalKeyKind]bool)
	v := m[base.InternalKeyKindRangeDelete]
	fmt.Println(v)
}
