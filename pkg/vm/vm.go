// Copyright 2021 Matrix Origin
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

package vm

import (
	"bytes"

	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

// call each operator's string function to show a query plan
func String(rootOp Operator, buf *bytes.Buffer) {
	HandleAllOp(rootOp, func(parentOp Operator, op Operator) error {
		if op.GetOperatorBase().NumChildren() > 0 {
			buf.WriteString(" -> ")
		}
		op.String(buf)
		return nil
	})
}

// do init work for each operator by calling its prepare function
func Prepare(op Operator, proc *process.Process) error {
	return HandleAllOp(op, func(parentOp Operator, op Operator) error {
		return op.Prepare(proc)
	})
}

func SetAnalyzeInfo(rootOp Operator, proc *process.Process) {
	HandleAllOp(rootOp, func(parentOp Operator, op Operator) error {
		switch op.OpType() {
		case Output:
			op.GetOperatorBase().SetIdx(-1)
			//case TableScan:
			//	op.GetOperatorBase().SetIdx(parentOp.GetOperatorBase().Idx)
		}
		return nil
	})

	idxMapMajor := make(map[int]int, 0)
	idxMapMinor := make(map[int]int, 0)
	HandleAllOp(rootOp, func(parentOp Operator, op Operator) error {
		opBase := op.GetOperatorBase()
		info := &OperatorInfo{
			Idx:     opBase.Idx,
			IsFirst: opBase.IsFirst,
			IsLast:  opBase.IsLast,

			CnAddr:      opBase.CnAddr,
			OperatorID:  opBase.OperatorID,
			ParallelID:  opBase.ParallelID,
			MaxParallel: opBase.MaxParallel,
		}
		opType := op.OpType()
		switch opType {
		case HashBuild, ShuffleBuild, IndexBuild, Filter, MergeGroup, MergeOrder:
			isMinor := true
			if opType == Filter {
				if opType != TableScan && opType != External {
					isMinor = false // restrict operator is minor only for scan
				}
			}

			if isMinor {
				if info.Idx >= 0 && info.Idx < len(proc.Base.AnalInfos) {
					info.ParallelMajor = false
					if pidx, ok := idxMapMinor[info.Idx]; ok {
						info.ParallelIdx = pidx
					} else {
						pidx = proc.Base.AnalInfos[info.Idx].AddNewParallel(false)
						idxMapMinor[info.Idx] = pidx
						info.ParallelIdx = pidx
					}
				}
			} else {
				info.ParallelIdx = -1
			}

		case TableScan, External, Order, Window, Group, Join, LoopJoin, Left, Single, Semi, RightSemi, Anti, RightAnti, Mark, Product, ProductL2:
			info.ParallelMajor = true
			if info.Idx >= 0 && info.Idx < len(proc.Base.AnalInfos) {
				if pidx, ok := idxMapMajor[info.Idx]; ok {
					info.ParallelIdx = pidx
				} else {
					pidx = proc.Base.AnalInfos[info.Idx].AddNewParallel(true)
					idxMapMajor[info.Idx] = pidx
					info.ParallelIdx = pidx
				}
			}
		default:
			info.ParallelIdx = -1 // do nothing for parallel analyze info
		}
		opBase.SetInfo(info)
		return nil
	})
}
