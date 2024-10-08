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

package compile

import (
	"context"
	"sync"
	"time"

	"github.com/matrixorigin/matrixone/pkg/common/reuse"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	txnClient "github.com/matrixorigin/matrixone/pkg/txn/client"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

// todo: Move it to a CN level structure next day.
var compileService *ServiceOfCompile

func init() {
	compileService = InitCompileService()
	txnClient.SetRunningPipelineManagement(compileService)
}

func GetCompileService() *ServiceOfCompile {
	return compileService
}

// ServiceOfCompile is used to manage the lifecycle of Compile structures,
// including their creation and deletion.
//
// It also tracks the currently active complies within a single CN.
type ServiceOfCompile struct {
	sync.Mutex

	// ongoing compiles with additional information.
	aliveCompiles map[*Compile]compileAdditionalInformation
}

// compileAdditionalInformation holds additional information for one compile.
// to help control one compile.
type compileAdditionalInformation struct {
	// mustReturnError holds an error that must be returned if set.
	mustReturnError error

	// queryCancel is a method to cancel an ongoing query.
	queryCancel context.CancelFunc
	// queryDone is a waiter that checks if this query has been completed or not.
	queryDone queryDoneWaiter
}

// kill one query and block until it was completed.
func (info *compileAdditionalInformation) kill(errResult error) {
	info.queryCancel()
	info.queryDone.checkCompleted()
	info.mustReturnError = errResult
}

type queryDoneWaiter chan bool

func newQueryDoneWaiter() queryDoneWaiter {
	return make(chan bool, 1)
}

func (waiter queryDoneWaiter) noticeQueryCompleted() {
	waiter <- true
}

func (waiter queryDoneWaiter) checkCompleted() {
	<-waiter
	waiter <- true
}

func (waiter queryDoneWaiter) clear() {
	for len(waiter) > 0 {
		<-waiter
	}
}

func InitCompileService() *ServiceOfCompile {
	srv := &ServiceOfCompile{
		aliveCompiles: make(map[*Compile]compileAdditionalInformation, 1024),
	}
	return srv
}

func (srv *ServiceOfCompile) getCompile(proc *process.Process) *Compile {
	runningCompile := reuse.Alloc[Compile](nil)
	// runningCompile.AllocMsg = time.Now().String() + " : " + string(debug.Stack())
	runningCompile.proc = proc
	return runningCompile
}

func (srv *ServiceOfCompile) recordRunningCompile(runningCompile *Compile) error {
	if runningCompile.queryStatus == nil {
		runningCompile.queryStatus = newQueryDoneWaiter()
	} else {
		runningCompile.queryStatus.clear()
	}

	queryCtx, queryCancel := process.GetQueryCtxFromProc(runningCompile.proc)

	srv.Lock()
	srv.aliveCompiles[runningCompile] = compileAdditionalInformation{
		mustReturnError: nil,
		queryCancel:     queryCancel,
		queryDone:       runningCompile.queryStatus,
	}
	srv.Unlock()

	err := queryCtx.Err()
	if err != nil {
		_, _ = srv.removeRunningCompile(runningCompile)
	}
	return err
}

func (srv *ServiceOfCompile) removeRunningCompile(c *Compile) (mustReturnError bool, err error) {
	c.queryStatus.noticeQueryCompleted()
	c.proc.SetBaseProcessRunningStatus(false)

	srv.Lock()

	// todo: because we don't deal with the mustReturnError now, I just ignore it.
	//if item, ok := srv.aliveCompiles[c]; ok {
	//	err = item.mustReturnError
	//}
	delete(srv.aliveCompiles, c)
	srv.Unlock()

	c.queryStatus.clear()
	//return err != nil, err
	return false, nil
}

func (srv *ServiceOfCompile) putCompile(c *Compile) {
	if !c.isPrepare {
		// c.FreeMsg = time.Now().String() + " : " + string(debug.Stack())
		reuse.Free[Compile](c, nil)
	}
}

func (srv *ServiceOfCompile) aliveCompile() int {
	srv.Lock()
	l := len(srv.aliveCompiles)
	srv.Unlock()
	return l
}

func (srv *ServiceOfCompile) PauseService() {
	srv.Lock()
}

func (srv *ServiceOfCompile) ResumeService() {
	srv.Unlock()
}

func (srv *ServiceOfCompile) KillAllQueriesWithError(err error) {
	logutil.Infof("compile service starts to kill all running queries.")
	start := time.Now()
	defer func() {
		logutil.Infof("compile service has killed all running queries, time cost: %.2f s", time.Since(start).Seconds())
	}()

	for _, v := range srv.aliveCompiles {
		v.kill(err)
	}
}
