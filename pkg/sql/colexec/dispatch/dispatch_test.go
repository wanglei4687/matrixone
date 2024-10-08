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

package dispatch

import (
	"bytes"
	"context"
	"testing"

	"github.com/matrixorigin/matrixone/pkg/common/mpool"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/value_scan"
	"github.com/matrixorigin/matrixone/pkg/testutil"
	"github.com/matrixorigin/matrixone/pkg/vm"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
	"github.com/stretchr/testify/require"
)

const (
	Rows = 10 // default rows
)

// add unit tests for cases
type dispatchTestCase struct {
	arg    *Dispatch
	types  []types.Type
	proc   *process.Process
	cancel context.CancelFunc
}

var (
	tcs []dispatchTestCase
)

func init() {
	tcs = []dispatchTestCase{
		newTestCase(),
		newTestCase(),
	}
}

func TestString(t *testing.T) {
	buf := new(bytes.Buffer)
	for _, tc := range tcs {
		tc.arg.String(buf)
	}
}

func TestPrepare(t *testing.T) {
	for _, tc := range tcs {
		err := tc.arg.Prepare(tc.proc)
		require.NoError(t, err)
	}
}

func TestDispatch(t *testing.T) {
	for _, tc := range tcs {
		err := tc.arg.Prepare(tc.proc)
		require.NoError(t, err)
		bats := []*batch.Batch{
			newBatch(tc.types, tc.proc, Rows),
			batch.EmptyBatch,
		}
		resetChildren(tc.arg, bats)
		/*{
			for _, vec := range bat.Vecs {
				if vec.IsOriginal() {
					vec.FreeOriginal(tc.proc.Mp())
				}
			}
		}*/
		_, _ = tc.arg.Call(tc.proc)
		tc.arg.Children[0].Reset(tc.proc, false, nil)
		tc.arg.Children[0].Free(tc.proc, false, nil)
		tc.arg.Reset(tc.proc, false, nil)
		tc.arg.Free(tc.proc, false, nil)
		for _, re := range tc.arg.LocalRegs {
			for len(re.Ch) > 0 {
				msg := <-re.Ch
				if msg.Batch == nil {
					break
				}
				msg.Batch.Clean(tc.proc.Mp())
			}
		}
		tc.proc.Free()
		require.Equal(t, int64(0), tc.proc.Mp().CurrNB())
	}
}

func newTestCase() dispatchTestCase {
	proc := testutil.NewProcessWithMPool("", mpool.MustNewZero())
	proc.Reg.MergeReceivers = make([]*process.WaitRegister, 2)
	ctx, cancel := context.WithCancel(context.Background())
	reg := &process.WaitRegister{Ctx: ctx, Ch: make(chan *process.RegisterMessage, 3)}
	return dispatchTestCase{
		proc:  proc,
		types: []types.Type{types.T_int8.ToType()},
		arg: &Dispatch{
			FuncId:    SendToAllLocalFunc,
			LocalRegs: []*process.WaitRegister{reg},
			OperatorBase: vm.OperatorBase{
				OperatorInfo: vm.OperatorInfo{
					Idx:     0,
					IsFirst: false,
					IsLast:  false,
				},
			},
		},
		cancel: cancel,
	}

}

// create a new block based on the type information
func newBatch(ts []types.Type, proc *process.Process, rows int64) *batch.Batch {
	return testutil.NewBatch(ts, false, int(rows), proc.Mp())
}

func resetChildren(arg *Dispatch, bats []*batch.Batch) {
	valueScanArg := &value_scan.ValueScan{
		Batchs: bats,
	}
	valueScanArg.Prepare(nil)
	if len(arg.Children) == 0 {
		arg.AppendChild(valueScanArg)

	} else {
		arg.Children = arg.Children[:0]
		arg.AppendChild(valueScanArg)
	}
}
