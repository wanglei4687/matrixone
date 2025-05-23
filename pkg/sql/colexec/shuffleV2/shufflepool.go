// Copyright 2024 Matrix Origin
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

package shuffleV2

import (
	"sync"

	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/objectio"

	"github.com/matrixorigin/matrixone/pkg/common/mpool"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

type shufflePoolStats struct { //for debug
	inputCNT    []int64
	outputCNT   []int64
	inputTotal  int64
	outputTotal int64
	//maxBatchCNT int   //max row of batches in shuffle pool
	//directRows  int64 //directly return by shuffle op, don't write into shuffle pool
}

func (sp *ShufflePoolV2) printStats() {
	sp.statsLock.Lock()
	defer sp.statsLock.Unlock()
	maxCNT := sp.stats.inputCNT[0]
	minCNT := sp.stats.inputCNT[0]
	for i := range sp.stats.inputCNT {
		sp.stats.inputTotal += sp.stats.inputCNT[i]
		sp.stats.outputTotal += sp.stats.outputCNT[i]
		if sp.stats.inputCNT[i] > maxCNT {
			maxCNT = sp.stats.inputCNT[i]
		}
		if sp.stats.inputCNT[i] < minCNT {
			minCNT = sp.stats.inputCNT[i]
		}
	}

	//logutil.Infof("shuffle pool stats: bucket num %v, input %v, output %v, average %v, max %v, min %v, maxBatchCnt %v, directRows %v",
	//	sp.bucketNum, sp.stats.inputTotal, sp.stats.outputTotal, sp.stats.inputTotal/int64(sp.bucketNum), maxCNT, minCNT, sp.stats.maxBatchCNT, sp.stats.directRows)
}

type ShufflePoolV2 struct {
	bucketNum     int32
	maxHolders    int32
	holders       int32
	finished      int32
	stoppers      int32
	batches       []*batch.CompactBatchs
	holderLock    sync.Mutex
	statsLock     sync.Mutex
	batchLocks    []sync.Mutex
	endingWaiters []chan bool
	batchWaiters  []chan bool
	stats         shufflePoolStats
}

func NewShufflePool(bucketNum int32, maxHolders int32) *ShufflePoolV2 {
	sp := &ShufflePoolV2{bucketNum: bucketNum, maxHolders: maxHolders}
	sp.holders = 0
	sp.finished = 0
	sp.stoppers = 0
	sp.batches = make([]*batch.CompactBatchs, sp.bucketNum)
	for i := range sp.batches {
		sp.batches[i] = batch.NewCompactBatchs(objectio.BlockMaxRows)
	}
	sp.batchLocks = make([]sync.Mutex, bucketNum)
	sp.endingWaiters = make([]chan bool, bucketNum)
	sp.batchWaiters = make([]chan bool, bucketNum)
	for i := range sp.batchWaiters {
		sp.batchWaiters[i] = make(chan bool, 1)
	}
	for i := range sp.endingWaiters {
		sp.endingWaiters[i] = make(chan bool, 1)
	}
	sp.stats.inputCNT = make([]int64, bucketNum)
	sp.stats.outputCNT = make([]int64, bucketNum)
	return sp
}

func (sp *ShufflePoolV2) hold() {
	sp.holderLock.Lock()
	defer sp.holderLock.Unlock()
	sp.holders++
	if sp.holders > sp.maxHolders {
		panic("shuffle pool too many holders!")
	}
}

func (sp *ShufflePoolV2) stopWriting() {
	sp.holderLock.Lock()
	defer sp.holderLock.Unlock()
	sp.stoppers++
	if sp.stoppers > sp.holders || sp.stoppers > sp.maxHolders {
		panic("shuffle pool too many stoppers!")
	}

	if sp.stoppers == sp.maxHolders {
		for i := range sp.endingWaiters {
			sp.endingWaiters[i] <- true
		}
	}
}

func (sp *ShufflePoolV2) allStop() bool {
	sp.holderLock.Lock()
	defer sp.holderLock.Unlock()
	return sp.stoppers == sp.maxHolders
}

func (sp *ShufflePoolV2) Reset(m *mpool.MPool) {
	sp.holderLock.Lock()
	defer sp.holderLock.Unlock()
	sp.finished++
	if sp.maxHolders != sp.holders || sp.maxHolders != sp.finished {
		return // still some other shuffle operators working
	}
	sp.printStats()
	for i := range sp.batches {
		if sp.batches[i] != nil {
			if sp.batches[i].RowCount() > 0 {
				logutil.Warnf("shuffle pool reset, batch %v rowcnt %v, maybe something wrong!", i, sp.batches[i].RowCount())
			}
			sp.batches[i].Clean(m)
		}
	}
	sp.holders = 0
	sp.finished = 0
	sp.stoppers = 0
	sp.endingWaiters = nil
	sp.batchWaiters = nil
	sp.stats = shufflePoolStats{}
}

func (sp *ShufflePoolV2) DebugPrint() { // only for debug
	sp.holderLock.Lock()
	defer sp.holderLock.Unlock()
	logutil.Warnf("shuffle pool print, maxHolders %v, holders %v, finished %v, stop writing %v", sp.maxHolders, sp.holders, sp.finished, sp.stoppers)
	sp.printStats()
	for i := range sp.batches {
		bat := sp.batches[i]
		if bat == nil {
			logutil.Infof("shuffle pool %p batches[%v] is nil", sp, i)
		} else {
			logutil.Infof("shuffle pool %p batches[%v] rowcnt %v", sp, i, bat.RowCount())
		}
	}
}

// shuffle operator is ending, release buf and sending remaining batches
func (sp *ShufflePoolV2) waitBatchOrEnd(shuffleIDX int32, proc *process.Process) {
	for {
		select {
		case <-sp.batchWaiters[shuffleIDX]:
			return
		case <-sp.endingWaiters[shuffleIDX]:
			return
		case <-proc.Ctx.Done():
			return
		}
	}
}

// if there is full batch  in pool, return it and put buf in the place to continue writing into pool
func (sp *ShufflePoolV2) getFullBatch(shuffleIDX int32) *batch.Batch {
	sp.batchLocks[shuffleIDX].Lock()
	defer sp.batchLocks[shuffleIDX].Unlock()
	var bat *batch.Batch
	if sp.batches[shuffleIDX].Length() > 1 {
		bat = sp.batches[shuffleIDX].PopFront()
		if bat != nil {
			bat.ShuffleIDX = shuffleIDX
		}
	}
	return bat
}

func (sp *ShufflePoolV2) getLastBatch(shuffleIDX int32) *batch.Batch {
	sp.batchLocks[shuffleIDX].Lock()
	defer sp.batchLocks[shuffleIDX].Unlock()

	bat := sp.batches[shuffleIDX].Pop()
	if bat != nil {
		bat.ShuffleIDX = shuffleIDX
	}
	return bat
}

func (sp *ShufflePoolV2) putAllBatchIntoPoolByShuffleIdx(srcBatch *batch.Batch, proc *process.Process, shuffleIDX int32) error {
	sp.batchLocks[shuffleIDX].Lock()
	defer sp.batchLocks[shuffleIDX].Unlock()

	err := sp.batches[shuffleIDX].Extend(proc.Mp(), srcBatch)
	if err != nil {
		return err
	}
	if sp.batches[shuffleIDX].Length() > 1 && len(sp.batchWaiters[shuffleIDX]) == 0 {
		sp.batchWaiters[shuffleIDX] <- true
	}
	//sp.statsLock.Lock()
	//if sp.batches[shuffleIDX].RowCount() > sp.stats.maxBatchCNT {
	//	sp.stats.maxBatchCNT = sp.batches[shuffleIDX].RowCount()
	//}
	//sp.stats.inputCNT[shuffleIDX] += int64(srcBatch.RowCount())
	//sp.statsLock.Unlock()
	// if sp.batches[shuffleIDX].RowCount() >= colexec.DefaultBatchSize && len(sp.batchWaiters[shuffleIDX]) == 0 {
	// 	sp.batchWaiters[shuffleIDX] <- true
	// }
	return nil
}

func (sp *ShufflePoolV2) putBatchIntoShuffledPoolsBySels(srcBatch *batch.Batch, sels [][]int32, proc *process.Process) error {
	var err error
	for i := range sp.batches {
		currentSels := sels[i]
		if len(currentSels) > 0 {
			sp.batchLocks[i].Lock()
			err = sp.batches[i].Union(proc.Mp(), srcBatch, currentSels)
			if err != nil {
				sp.batchLocks[i].Unlock()
				return err
			}
			if sp.batches[i].Length() > 1 && len(sp.batchWaiters[i]) == 0 {
				sp.batchWaiters[i] <- true
			}
			sp.batchLocks[i].Unlock()
			//sp.statsLock.Lock()
			//sp.stats.inputCNT[i] += int64(len(currentSels))
			//if bat.RowCount() > sp.stats.maxBatchCNT {
			//	sp.stats.maxBatchCNT = bat.RowCount()
			//}
			//sp.statsLock.Unlock()
		}
	}
	return nil
}

func (sp *ShufflePoolV2) statsDirectlySentBatch(srcBatch *batch.Batch) {
	//rows := int64(srcBatch.RowCount())
	//sp.statsLock.Lock()
	//sp.stats.inputCNT[srcBatch.ShuffleIDX] += rows
	//sp.stats.outputCNT[srcBatch.ShuffleIDX] += rows
	//sp.stats.directRows += rows
	//sp.statsLock.Unlock()
}
