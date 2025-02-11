// Copyright 2021 - 2024 Matrix Origin
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

package mergetop

import (
	"github.com/matrixorigin/matrixone/pkg/common/reuse"
	"github.com/matrixorigin/matrixone/pkg/compare"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/pb/plan"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec"
	"github.com/matrixorigin/matrixone/pkg/vm"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

var _ vm.Operator = new(MergeTop)

type container struct {
	n     int // result vector number
	sels  []int64
	poses []int32           // sorted list of attributes
	cmps  []compare.Compare // compare structure used to do sort work

	limit         uint64
	limitExecutor colexec.ExpressionExecutor

	bat *batch.Batch // bat stores the final result of merge-top

	executorsForOrderList []colexec.ExpressionExecutor
}

type MergeTop struct {
	Limit *plan.Expr          // Limit store the number of mergeTop-operator
	ctr   container           // ctr stores the attributes needn't do Serialization work
	Fs    []*plan.OrderBySpec // Fs store the order information

	vm.OperatorBase
}

func (mergeTop *MergeTop) GetOperatorBase() *vm.OperatorBase {
	return &mergeTop.OperatorBase
}

func init() {
	reuse.CreatePool(
		func() *MergeTop {
			return &MergeTop{}
		},
		func(a *MergeTop) {
			*a = MergeTop{}
		},
		reuse.DefaultOptions[MergeTop]().
			WithEnableChecker(),
	)
}

func (mergeTop MergeTop) TypeName() string {
	return opName
}

func NewArgument() *MergeTop {
	return reuse.Alloc[MergeTop](nil)
}

func (mergeTop *MergeTop) WithLimit(limit *plan.Expr) *MergeTop {
	mergeTop.Limit = limit
	return mergeTop
}

func (mergeTop *MergeTop) WithFs(fs []*plan.OrderBySpec) *MergeTop {
	mergeTop.Fs = fs
	return mergeTop
}

func (mergeTop *MergeTop) Release() {
	if mergeTop != nil {
		reuse.Free(mergeTop, nil)
	}
}

func (mergeTop *MergeTop) Reset(proc *process.Process, pipelineFailed bool, err error) {
	mergeTop.ctr.reset()
}

func (mergeTop *MergeTop) Free(proc *process.Process, pipelineFailed bool, err error) {
	mergeTop.ctr.free(proc)
}

func (mergeTop *MergeTop) ExecProjection(proc *process.Process, input *batch.Batch) (*batch.Batch, error) {
	return input, nil
}

func (ctr *container) reset() {
	ctr.n = 0
	ctr.sels = nil
	ctr.poses = nil
	ctr.cmps = nil

	ctr.limit = 0
	if ctr.limitExecutor != nil {
		ctr.limitExecutor.ResetForNextQuery()
	}

	if ctr.bat != nil {
		ctr.bat.CleanOnlyData()
	}

	for _, executor := range ctr.executorsForOrderList {
		if executor != nil {
			executor.ResetForNextQuery()
		}
	}
}

func (ctr *container) free(proc *process.Process) {
	if ctr.bat != nil {
		ctr.bat.Clean(proc.Mp())
		ctr.bat = nil
	}

	for i := range ctr.executorsForOrderList {
		if ctr.executorsForOrderList[i] == nil {
			continue
		}
		ctr.executorsForOrderList[i].Free()
	}
	ctr.executorsForOrderList = nil

	if ctr.limitExecutor != nil {
		ctr.limitExecutor.Free()
	}
	ctr.limitExecutor = nil
}

func (ctr *container) compare(vi, vj int, i, j int64) int {
	for _, pos := range ctr.poses {
		if r := ctr.cmps[pos].Compare(vi, vj, i, j); r != 0 {
			return r
		}
	}
	return 0
}

func (ctr *container) Len() int {
	return len(ctr.sels)
}

func (ctr *container) Less(i, j int) bool {
	return ctr.compare(0, 0, ctr.sels[i], ctr.sels[j]) > 0
}

func (ctr *container) Swap(i, j int) {
	ctr.sels[i], ctr.sels[j] = ctr.sels[j], ctr.sels[i]
}

func (ctr *container) Push(x interface{}) {
	ctr.sels = append(ctr.sels, x.(int64))
}

func (ctr *container) Pop() interface{} {
	n := len(ctr.sels) - 1
	x := ctr.sels[n]
	ctr.sels = ctr.sels[:n]
	return x
}
