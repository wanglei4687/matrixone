// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package mergeblock

import (
	"go.uber.org/zap"

	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/common/reuse"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/defines"
	"github.com/matrixorigin/matrixone/pkg/fileservice"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/objectio"
	"github.com/matrixorigin/matrixone/pkg/perfcounter"
	"github.com/matrixorigin/matrixone/pkg/sql/plan"
	"github.com/matrixorigin/matrixone/pkg/vm"
	"github.com/matrixorigin/matrixone/pkg/vm/engine"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/disttae"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

var _ vm.Operator = new(MergeBlock)

type Container struct {
	// mp is used to store the metaLoc Batch.
	// Notice that batches in mp should be free, since the memory of these batches be allocated from mpool.
	mp map[int]*batch.Batch
	// mp2 is used to store the normal data batches
	mp2          map[int][]*batch.Batch
	source       engine.Relation
	affectedRows uint64
}

type MergeBlock struct {
	AddAffectedRows bool
	Engine          engine.Engine
	Ref             *plan.ObjectRef
	container       Container

	vm.OperatorBase
}

func (mergeBlock *MergeBlock) GetOperatorBase() *vm.OperatorBase {
	return &mergeBlock.OperatorBase
}

func init() {
	reuse.CreatePool[MergeBlock](
		func() *MergeBlock {
			return &MergeBlock{}
		},
		func(a *MergeBlock) {
			*a = MergeBlock{}
		},
		reuse.DefaultOptions[MergeBlock]().
			WithEnableChecker(),
	)
}

func (mergeBlock MergeBlock) TypeName() string {
	return opName
}

func NewArgument() *MergeBlock {
	return reuse.Alloc[MergeBlock](nil)
}

func (mergeBlock *MergeBlock) WithObjectRef(ref *plan.ObjectRef) *MergeBlock {
	mergeBlock.Ref = ref
	return mergeBlock
}

func (mergeBlock *MergeBlock) WithEngine(eng engine.Engine) *MergeBlock {
	mergeBlock.Engine = eng
	return mergeBlock
}

func (mergeBlock *MergeBlock) WithAddAffectedRows(addAffectedRows bool) *MergeBlock {
	mergeBlock.AddAffectedRows = addAffectedRows
	return mergeBlock
}

func (mergeBlock *MergeBlock) Release() {
	if mergeBlock != nil {
		reuse.Free[MergeBlock](mergeBlock, nil)
	}
}

func (mergeBlock *MergeBlock) Reset(proc *process.Process, pipelineFailed bool, err error) {
	//can not reset affectRows, because MO need get affectRows after reset
	mergeBlock.resetMp(proc)
}

func (mergeBlock *MergeBlock) Free(proc *process.Process, pipelineFailed bool, err error) {
	mergeBlock.cleanMp(proc)
}

func (mergeBlock *MergeBlock) ExecProjection(proc *process.Process, input *batch.Batch) (*batch.Batch, error) {
	return input, nil
}

func (mergeBlock *MergeBlock) GetMetaLocBat(
	src *batch.Batch,
	proc *process.Process,
) bool {

	hasTblIdxCol := src.Attrs[0] == catalog.BlockMeta_TableIdx_Insert

	if len(mergeBlock.container.mp) > 0 {
		for _, bat := range mergeBlock.container.mp {
			bat.CleanOnlyData()
		}
		return hasTblIdxCol
	}

	var (
		vecStart int

		attrs []string
		typs  []types.Type
	)

	attrs = src.Attrs
	if hasTblIdxCol {
		// exclude the table id column
		attrs = attrs[1:]
		vecStart = 1
	}

	for idx := vecStart; idx < len(src.Vecs); idx++ {
		typs = append(typs, *src.Vecs[idx].GetType())
	}

	// src comes from old CN which haven't object stats column
	if src.Attrs[len(src.Attrs)-1] != catalog.ObjectMeta_ObjectStats {
		attrs = append(attrs, catalog.ObjectMeta_ObjectStats)
		typs = append(typs, types.T_binary.ToType())
	}

	bat := batch.NewWithSize(len(attrs))
	bat.Attrs = attrs
	for idx := 0; idx < len(attrs); idx++ {
		bat.Vecs[idx] = vector.NewVec(typs[idx])
	}
	mergeBlock.container.mp[0] = bat

	return hasTblIdxCol
}

func splitObjectStats(mergeBlock *MergeBlock, proc *process.Process,
	analyzer process.Analyzer,
	bat *batch.Batch, blkVec *vector.Vector, tblIdx []int16,
) error {
	// bat comes from old CN, no object stats vec in it.
	// to ensure all bats the TN received contain the object stats column, we should
	// construct the object stats from block info here.
	needLoad := bat.Attrs[len(bat.Attrs)-1] != catalog.ObjectMeta_ObjectStats

	fs, err := fileservice.Get[fileservice.FileService](proc.Base.FileService, defines.SharedFileServiceName)
	if err != nil {
		logutil.Error("get fs failed when split object stats. ", zap.Error(err))
		return err
	}

	objDataMeta := objectio.BuildObjectMeta(uint16(blkVec.Length()))
	var (
		statsIdx int
		statsVec *vector.Vector
		objStats objectio.ObjectStats
	)

	if len(tblIdx) > 0 {
		statsVec = bat.Vecs[2]
	} else {
		statsVec = bat.Vecs[1]
	}

	for idx := 0; idx < blkVec.Length(); idx++ {
		if len(tblIdx) > 0 && tblIdx[idx] < 0 {
			// will the data and blk infos mixed together in one batch?
			// batch [ data | data | blk info | blk info | .... ]
			continue
		}

		blkInfo := objectio.DecodeBlockInfo(blkVec.GetBytesAt(idx))
		if objectio.IsSameObjectLocVsMeta(blkInfo.MetaLocation(), objDataMeta) {
			continue
		}

		var destVec *vector.Vector
		if len(tblIdx) > 0 {
			destVec = mergeBlock.container.mp[int(tblIdx[idx])].Vecs[1]
		} else {
			destVec = mergeBlock.container.mp[0].Vecs[1]
		}

		if needLoad {
			crs := analyzer.GetOpCounterSet()
			newCtx := perfcounter.AttachS3RequestKey(proc.Ctx, crs)

			// comes from old version cn
			objStats, objDataMeta, err = disttae.ConstructObjStatsByLoadObjMeta(newCtx, blkInfo.MetaLocation(), fs)
			if err != nil {
				return err
			}
			analyzer.AddS3RequestCount(crs)
			analyzer.AddDiskIO(crs)

			vector.AppendBytes(destVec, objStats.Marshal(), false, proc.GetMPool())
		} else {
			// not comes from old version cn
			vector.AppendBytes(destVec, statsVec.GetBytesAt(statsIdx), false, proc.GetMPool())
			objDataMeta.BlockHeader().SetBlockID(&blkInfo.BlockID)
			statsIdx++
		}
	}

	return nil
}

func (mergeBlock *MergeBlock) Split(proc *process.Process, bat *batch.Batch, analyzer process.Analyzer) error {
	// meta loc and object stats
	hasTblIdxCol := mergeBlock.GetMetaLocBat(bat, proc)

	var (
		tblIdx      []int16
		blkInfosVec *vector.Vector
	)

	// table idx column only exists in the old implementation.
	if hasTblIdxCol {
		tblIdx = vector.MustFixedColWithTypeCheck[int16](bat.GetVector(0))
		blkInfosVec = bat.GetVector(1)
	} else {
		blkInfosVec = bat.GetVector(0)
	}

	var hasObject bool

	for i := range blkInfosVec.Length() { // append s3 writer returned blk info
		if (len(tblIdx) > 0 && tblIdx[i] >= 0) || !hasTblIdxCol {
			if mergeBlock.AddAffectedRows {
				blkInfo := objectio.DecodeBlockInfo(blkInfosVec.GetBytesAt(i))
				mergeBlock.container.affectedRows += uint64(blkInfo.MetaLocation().Rows())
			}

			var dest *vector.Vector
			if hasTblIdxCol {
				dest = mergeBlock.container.mp[int(tblIdx[i])].Vecs[0]
			} else {
				dest = mergeBlock.container.mp[0].Vecs[0]
			}
			vector.AppendBytes(dest, blkInfosVec.GetBytesAt(i), false, proc.GetMPool())

			hasObject = true

		} else { // append data
			idx := int(-(tblIdx[i] + 1))
			newBat := &batch.Batch{}
			if err := newBat.UnmarshalBinary(blkInfosVec.GetBytesAt(i)); err != nil {
				return err
			}
			if mergeBlock.AddAffectedRows {
				mergeBlock.container.affectedRows += uint64(newBat.RowCount())
			}
			mergeBlock.container.mp2[idx] = append(mergeBlock.container.mp2[idx], newBat)
		}
	}

	// exist blk info, split it
	if hasObject {
		if err := splitObjectStats(mergeBlock, proc, analyzer, bat, blkInfosVec, tblIdx); err != nil {
			return err
		}
	}

	for _, b := range mergeBlock.container.mp {
		b.SetRowCount(b.Vecs[0].Length())
	}
	return nil
}

func (mergeBlock *MergeBlock) resetMp(proc *process.Process) {
	for k := range mergeBlock.container.mp {
		mergeBlock.container.mp[k].CleanOnlyData()
	}
	for i, bats := range mergeBlock.container.mp2 {
		for _, bat := range bats {
			bat.Clean(proc.GetMPool())
		}
		mergeBlock.container.mp2[i] = mergeBlock.container.mp2[i][:0]
	}
}

func (mergeBlock *MergeBlock) cleanMp(proc *process.Process) {
	for k := range mergeBlock.container.mp {
		mergeBlock.container.mp[k].Clean(proc.GetMPool())
		mergeBlock.container.mp[k] = nil
	}
	mergeBlock.container.mp = nil
	for _, bats := range mergeBlock.container.mp2 {
		for _, bat := range bats {
			bat.Clean(proc.GetMPool())
		}
	}
	mergeBlock.container.mp2 = nil
}

func (mergeBlock *MergeBlock) GetAffectedRows() uint64 {
	return mergeBlock.container.affectedRows
}
