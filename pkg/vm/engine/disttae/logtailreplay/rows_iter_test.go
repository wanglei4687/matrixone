// Copyright 2023 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logtailreplay

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/matrixorigin/matrixone/pkg/vm/engine/readutil"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/btree"

	"github.com/matrixorigin/matrixone/pkg/common/mpool"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/objectio"
	"github.com/matrixorigin/matrixone/pkg/pb/api"
	"github.com/matrixorigin/matrixone/pkg/sql/plan/function"
)

func TestPartitionStateRowsIter(t *testing.T) {
	state := NewPartitionState("", false, 42)
	ctx := context.Background()
	pool := mpool.MustNewZero()
	packer := types.NewPacker()
	defer packer.Close()

	{
		// empty rows
		iter := state.NewRowsIter(types.BuildTS(0, 0), nil, false)
		n := 0
		for iter.Next() {
			n++
		}
		require.Equal(t, 0, n)
		require.Nil(t, iter.Close())
	}

	{
		// iter again
		iter := state.NewRowsIter(types.BuildTS(0, 0), nil, false)
		n := 0
		for iter.Next() {
			n++
		}
		require.Equal(t, 0, n)
		require.Nil(t, iter.Close())
	}

	const num = 128

	sid := objectio.NewSegmentid()
	buildRowID := func(i int) types.Rowid {
		blk := objectio.NewBlockid(sid, uint16(i), 0)
		return objectio.NewRowid(blk, uint32(0))
	}

	{
		// insert number i at time i with (i+1) row id
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		vec1 := vector.NewVec(types.T_int64.ToType())
		for i := 0; i < num; i++ {
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(i), 0), false, pool)
			vector.AppendFixed(vec1, int64(i), false, pool)
		}
		state.HandleRowsInsert(ctx, &api.Batch{
			Attrs: []string{"rowid", "time", "a"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(vec1),
			},
		}, 0, packer, pool)
	}

	// rows iter
	for i := 0; i < num; i++ {
		ts := types.BuildTS(int64(i), 0)
		iter := state.NewRowsIter(ts, nil, false)
		n := 0
		rowIDs := make(map[types.Rowid]bool)
		for iter.Next() {
			n++
			entry := iter.Entry()
			rowIDs[entry.RowID] = true
			// RowExists
			require.True(t, state.RowExists(entry.RowID, ts))
		}
		require.Equal(t, i+1, n)
		require.Equal(t, i+1, len(rowIDs))
		require.Nil(t, iter.Close())
	}

	// primary key iter
	for i := 0; i < num; i++ {
		ts := types.BuildTS(int64(i), 0)
		bs := readutil.EncodePrimaryKey(int64(i), packer)
		iter := state.NewPrimaryKeyIter(ts, function.EQUAL, [][]byte{bs})
		n := 0
		for iter.Next() {
			n++
		}
		require.Equal(t, 1, n)
		require.Nil(t, iter.Close())
		modified, _ := state.PKExistInMemBetween(ts.Prev(), ts.Next(), [][]byte{bs})
		require.True(t, modified)
	}

	{
		// insert duplicated rows
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		vec1 := vector.NewVec(types.T_int64.ToType())
		for i := 0; i < num; i++ {
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(i), 1), false, pool)
			vector.AppendFixed(vec1, int64(i), false, pool)
		}
		state.HandleRowsInsert(ctx, &api.Batch{
			Attrs: []string{"rowid", "time", "a"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(vec1),
			},
		}, 0, packer, pool)
	}

	// rows iter
	for i := 0; i < num; i++ {
		iter := state.NewRowsIter(types.BuildTS(int64(i), 0), nil, false)
		n := 0
		rowIDs := make(map[types.Rowid]bool)
		for iter.Next() {
			n++
			entry := iter.Entry()
			rowIDs[entry.RowID] = true
		}
		require.Equal(t, i+1, n)
		require.Equal(t, i+1, len(rowIDs))
		require.Nil(t, iter.Close())
	}

	deleteAt := 1000
	{
		// delete number i at (deleteAt+i) with (i+1) row id
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		tombStoneRowIDVec := vector.NewVec(types.T_Rowid.ToType())
		pkVec := vector.NewVec(types.T_int32.ToType())

		for i := 0; i < num; i++ {
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(deleteAt+i), 1), false, pool)
			vector.AppendFixed(tombStoneRowIDVec, types.RandomRowid(), false, pool)
			vector.AppendFixed(pkVec, int32(i), false, pool)
		}
		state.HandleRowsDelete(ctx, &api.Batch{
			Attrs: []string{"rowid", "time"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(pkVec),
				mustVectorToProto(tombStoneRowIDVec),
			},
		}, packer, pool)
	}

	for i := 0; i < num; i++ {
		{
			// rows iter
			iter := state.NewRowsIter(types.BuildTS(int64(deleteAt+i), 0), nil, false)
			rowIDs := make(map[types.Rowid]bool)
			n := 0
			for iter.Next() {
				n++
				entry := iter.Entry()
				rowIDs[entry.RowID] = true
			}
			require.Equal(t, num-i, n)
			require.Equal(t, num-i, len(rowIDs))
			require.Nil(t, iter.Close())
		}

		{
			// deleted rows iter
			rowid := buildRowID(i + 1)
			blockID := rowid.BorrowBlockID()
			iter := state.NewRowsIter(types.BuildTS(int64(deleteAt+i+1), 0), blockID, true)
			rowIDs := make(map[types.Rowid]bool)
			n := 0
			for iter.Next() {
				n++
				entry := iter.Entry()
				rowIDs[entry.RowID] = true
			}
			require.Equal(t, 1, n, "num is %d", i)
			require.Equal(t, 1, len(rowIDs))
			require.Nil(t, iter.Close())
		}

		{
			// primary key change detection
			ts := types.BuildTS(int64(deleteAt+i), 0)
			key := readutil.EncodePrimaryKey(int64(i), packer)
			modified, _ := state.PKExistInMemBetween(
				ts.Prev(),
				ts.Next(),
				[][]byte{key},
			)
			require.True(t, modified)
		}

		{
			// primary key iter
			key := readutil.EncodePrimaryKey(int64(i), packer)
			iter := state.NewPrimaryKeyIter(types.BuildTS(int64(deleteAt+i+1), 0),
				function.EQUAL, [][]byte{key})
			n := 0
			for iter.Next() {
				n++
			}
			iter.Close()
			require.Equal(t, 0, n) // not visible
		}

	}

	deleteAt = 2000
	{
		// duplicate delete
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		tombStoneRowIDVec := vector.NewVec(types.T_Rowid.ToType())
		pkVec := vector.NewVec(types.T_int32.ToType())

		for i := 0; i < num; i++ {
			vector.AppendFixed(tombStoneRowIDVec, types.RandomRowid(), false, pool)
			vector.AppendFixed(pkVec, int32(i), false, pool)
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(deleteAt+i), 1), false, pool)
		}
		state.HandleRowsDelete(ctx, &api.Batch{
			Attrs: []string{"rowid", "time", "a"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(pkVec),
				mustVectorToProto(tombStoneRowIDVec),
			},
		}, packer, pool)
	}

	for i := 0; i < num; i++ {
		{
			rowid := buildRowID(i + 1)
			blockID := rowid.BorrowBlockID()
			iter := state.NewRowsIter(types.BuildTS(int64(deleteAt+i), 0), blockID, true)
			rowIDs := make(map[types.Rowid]bool)
			n := 0
			for iter.Next() {
				n++
				entry := iter.Entry()
				rowIDs[entry.RowID] = true
			}
			require.Equal(t, 1, n)
			require.Equal(t, 1, len(rowIDs))
			require.Nil(t, iter.Close())
		}
	}

}

func TestInsertAndDeleteAtTheSameTimestamp(t *testing.T) {
	state := NewPartitionState("", false, 42)
	ctx := context.Background()
	pool := mpool.MustNewZero()
	packer := types.NewPacker()
	defer packer.Close()

	const num = 128

	sid := objectio.NewSegmentid()
	buildRowID := func(i int) types.Rowid {
		blk := objectio.NewBlockid(sid, uint16(i), 0)
		return objectio.NewRowid(blk, uint32(0))
	}

	{
		// insert number i at time i with (i+1) row id
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		vec1 := vector.NewVec(types.T_int64.ToType())
		for i := 0; i < num; i++ {
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(i), 0), false, pool)
			vector.AppendFixed(vec1, int64(i), false, pool)
		}
		state.HandleRowsInsert(ctx, &api.Batch{
			Attrs: []string{"rowid", "time", "a"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(vec1),
			},
		}, 0, packer, pool)
	}

	{
		// delete number i at the same time
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		tombStoneRowIDVec := vector.NewVec(types.T_Rowid.ToType())
		pkVec := vector.NewVec(types.T_int32.ToType())

		for i := 0; i < num; i++ {
			vector.AppendFixed(tombStoneRowIDVec, types.RandomRowid(), false, pool)
			vector.AppendFixed(pkVec, int32(i), false, pool)
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(i), 1), false, pool)
		}
		state.HandleRowsDelete(ctx, &api.Batch{
			Attrs: []string{"rowid", "time"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(pkVec),
				mustVectorToProto(tombStoneRowIDVec),
			},
		}, packer, pool)
	}

	{
		// should be deleted
		iter := state.NewRowsIter(
			types.BuildTS(num*2, 0),
			nil,
			false,
		)
		n := 0
		for iter.Next() {
			n++
		}
		require.Equal(t, 0, n)
		require.Nil(t, iter.Close())
	}

	{
		// iter deleted
		iter := state.NewRowsIter(
			types.BuildTS(num*2, 0),
			nil,
			true,
		)
		n := 0
		for iter.Next() {
			n++
		}
		require.Equal(t, num, n)
		require.Nil(t, iter.Close())
	}

	// should be detectable
	for i := 0; i < num; i++ {
		ts := types.BuildTS(int64(i), 0)
		key := readutil.EncodePrimaryKey(int64(i), packer)
		modified, _ := state.PKExistInMemBetween(ts.Prev(), ts.Next(), [][]byte{key})
		require.True(t, modified)
	}

}

func TestDeleteBeforeInsertAtTheSameTime(t *testing.T) {
	state := NewPartitionState("", false, 42)
	ctx := context.Background()
	pool := mpool.MustNewZero()
	packer := types.NewPacker()
	defer packer.Close()

	const num = 128

	sid := objectio.NewSegmentid()
	buildRowID := func(i int) types.Rowid {
		blk := objectio.NewBlockid(sid, uint16(i), 0)
		return objectio.NewRowid(blk, uint32(0))
	}

	{
		// delete number i at time i with (i+1) row id
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		tombStoneRowIDVec := vector.NewVec(types.T_Rowid.ToType())
		pkVec := vector.NewVec(types.T_int32.ToType())

		for i := 0; i < num; i++ {
			vector.AppendFixed(tombStoneRowIDVec, types.RandomRowid(), false, pool)
			vector.AppendFixed(pkVec, int32(i), false, pool)
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(i), 1), false, pool)
		}
		state.HandleRowsDelete(ctx, &api.Batch{
			Attrs: []string{"rowid", "time"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(pkVec),
				mustVectorToProto(tombStoneRowIDVec),
			},
		}, packer, pool)
	}

	{
		// insert number i at time i with (i+1) row id
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		vec1 := vector.NewVec(types.T_int64.ToType())
		for i := 0; i < num; i++ {
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(i), 0), false, pool)
			vector.AppendFixed(vec1, int64(i), false, pool)
		}
		state.HandleRowsInsert(ctx, &api.Batch{
			Attrs: []string{"rowid", "time", "a"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(vec1),
			},
		}, 0, packer, pool)
	}

	{
		// should be deleted
		iter := state.NewRowsIter(
			types.BuildTS(num*2, 0),
			nil,
			false,
		)
		n := 0
		for iter.Next() {
			n++
		}
		require.Equal(t, 0, n)
		require.Nil(t, iter.Close())
	}

	{
		// iter deleted
		iter := state.NewRowsIter(
			types.BuildTS(num*2, 0),
			nil,
			true,
		)
		n := 0
		for iter.Next() {
			n++
		}
		require.Equal(t, num, n)
		require.Nil(t, iter.Close())
	}

	// should be detectable
	for i := 0; i < num; i++ {
		ts := types.BuildTS(int64(i), 0)
		key := readutil.EncodePrimaryKey(int64(i), packer)
		modified, _ := state.PKExistInMemBetween(ts.Prev(), ts.Next(), [][]byte{key})
		require.True(t, modified)
	}

}

func TestPrimaryKeyModifiedWithDeleteOnly(t *testing.T) {
	state := NewPartitionState("", false, 42)
	ctx := context.Background()
	pool := mpool.MustNewZero()
	packer := types.NewPacker()
	defer packer.Close()

	const num = 128

	sid := objectio.NewSegmentid()
	buildRowID := func(i int) types.Rowid {
		blk := objectio.NewBlockid(sid, uint16(i), 0)
		return objectio.NewRowid(blk, uint32(0))
	}

	{
		// delete number i at time i with (i+1) row id
		rowIDVec := vector.NewVec(types.T_Rowid.ToType())
		tsVec := vector.NewVec(types.T_TS.ToType())
		tombStoneRowIDVec := vector.NewVec(types.T_Rowid.ToType())

		primaryKeyVec := vector.NewVec(types.T_int64.ToType())
		for i := 0; i < num; i++ {
			vector.AppendFixed(rowIDVec, buildRowID(i+1), false, pool)
			vector.AppendFixed(tsVec, types.BuildTS(int64(i), 1), false, pool)
			vector.AppendFixed(primaryKeyVec, int64(i), false, pool)
			vector.AppendFixed(tombStoneRowIDVec, types.RandomRowid(), false, pool)
		}
		state.HandleRowsDelete(ctx, &api.Batch{
			Attrs: []string{"rowid", "time", "i"},
			Vecs: []api.Vector{
				mustVectorToProto(rowIDVec),
				mustVectorToProto(tsVec),
				mustVectorToProto(primaryKeyVec), // with primary key
				mustVectorToProto(tombStoneRowIDVec),
			},
		}, packer, pool)
	}

	// should be detectable
	for i := 0; i < num; i++ {
		ts := types.BuildTS(int64(i), 0)
		key := readutil.EncodePrimaryKey(int64(i), packer)
		modified, _ := state.PKExistInMemBetween(ts.Prev(), ts.Next(), [][]byte{key})
		require.True(t, modified)
	}

}

func TestPrefixIn(t *testing.T) {
	pkTree := btree.NewBTreeGOptions((*PrimaryIndexEntry).Less, btree.Options{
		Degree: 64,
	})

	for i := 0; i < 10; i++ {
		pkTree.Set(&PrimaryIndexEntry{
			Bytes: []byte{byte(i)},
		})
	}

	var encodes = [][]byte{{0}, {2}, {4}}

	for i := 10; i < 150; i++ {
		encodes = append(encodes, []byte{byte(i)})
	}

	// len(encodes) >> len(pkTree) to trigger Scan all
	spec := InKind(encodes, function.PREFIX_IN)

	pkIter := &primaryKeyIter{
		primaryIndex: pkTree,
		iter:         pkTree.Iter(),
	}

	spec.Move(pkIter)
	require.Equal(t, []byte{0}, pkIter.iter.Item().Bytes)
	spec.Move(pkIter)
	require.Equal(t, []byte{2}, pkIter.iter.Item().Bytes)
	spec.Move(pkIter)
	require.Equal(t, []byte{4}, pkIter.iter.Item().Bytes)
}

func BenchmarkPrimaryKeyIter(b *testing.B) {
	tree := btree.NewBTreeGOptions((*PrimaryIndexEntry).Less,
		btree.Options{
			Degree: 64,
		})

	itemCnt := 1000 * 10

	for i := 0; i < itemCnt; i++ {
		xx := rand.Intn(itemCnt / 10)
		ts := types.BuildTS(rand.Int63n(int64(itemCnt)), 0)

		tree.Set(&PrimaryIndexEntry{
			Time:       ts,
			RowEntryID: int64(i),
			Bytes:      types.EncodeFixed[int](xx),
		})
	}

	iter1 := tree.Copy().Iter()
	iter2 := tree.Copy().Iter()

	b.Run("Seek", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			xx := rand.Intn(itemCnt / 10)
			ts := types.BuildTS(rand.Int63n(int64(itemCnt)), 0)
			iter1.Seek(&PrimaryIndexEntry{
				Time:       ts,
				RowEntryID: rand.Int63n(int64(itemCnt)),
				Bytes:      types.EncodeFixed[int](xx),
			})
		}
	})

	b.Run("Comparison Item", func(b *testing.B) {
		items := tree.Items()
		for i := 0; i < b.N; i++ {
			x := rand.Intn(tree.Len())
			y := rand.Intn(tree.Len())

			items[x].Less(items[y])
		}
	})

	b.Run("Comparison Bytes", func(b *testing.B) {
		items := tree.Items()
		for i := 0; i < b.N; i++ {
			x := rand.Intn(tree.Len())
			y := rand.Intn(tree.Len())

			bytes.Compare(items[x].Bytes, items[y].Bytes)
		}
	})

	b.Run("Next", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if !iter2.Next() {
				iter2.First()
			}
		}
	})
}
