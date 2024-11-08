// Copyright 2023 Matrix Origin
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

package merge

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/process"
)

const estimateMemUsagePerRow = 20

func estimateMergeConsume(objs []*catalog.ObjectEntry) (origSize, estSize int) {
	for _, o := range objs {
		origSize += int(o.OriginSize())

		// the main memory consumption is transfer table.
		// each row uses ~20B, so estimate size is 20 * rows.
		estSize += int(o.Rows()) * estimateMemUsagePerRow
	}
	return
}

func entryOutdated(entry *catalog.ObjectEntry, lifetime time.Duration) bool {
	createdAt := entry.CreatedAt.Physical()
	return time.Unix(0, createdAt).Add(lifetime).Before(time.Now())
}

type resourceController struct {
	proc *process.Process

	limit    int64
	using    int64
	reserved int64

	reservedMergeRows int64
	reservedMergeBlks uint64
	transferPageLimit int64

	cpuPercent float64
}

func (c *resourceController) setMemLimit(total int64) {
	cgroup, err := memlimit.FromCgroup()
	if cgroup != 0 && int64(cgroup) < total {
		c.limit = int64(cgroup / 4 * 3)
	} else {
		c.limit = total / 4 * 3
	}

	if c.limit > 200*common.Const1GBytes {
		c.transferPageLimit = c.limit / 25 * 2 // 8%
	} else if c.limit > 100*common.Const1GBytes {
		c.transferPageLimit = c.limit / 25 * 3 // 12%
	} else if c.limit > 40*common.Const1GBytes {
		c.transferPageLimit = c.limit / 25 * 4 // 16%
	} else {
		c.transferPageLimit = math.MaxInt64 // no limit
	}

	logutil.Info(
		"MergeExecutorMemoryInfo",
		common.AnyField("container-limit", common.HumanReadableBytes(int(cgroup))),
		common.AnyField("host-memory", common.HumanReadableBytes(int(total))),
		common.AnyField("process-limit", common.HumanReadableBytes(int(c.limit))),
		common.AnyField("transfer-page-limit", common.HumanReadableBytes(int(c.transferPageLimit))),
		common.AnyField("error", err),
	)
}

func (c *resourceController) refresh() {
	if c.limit == 0 {
		c.setMemLimit(memInfo("MemTotal"))
	}

	if c.proc == nil {
		c.proc, _ = process.NewProcess(int32(os.Getpid()))
	}
	if m, err := c.proc.MemoryInfo(); err == nil {
		c.using = int64(m.RSS)
	}

	if percents, err := cpu.Percent(0, false); err == nil {
		c.cpuPercent = percents[0]
	}
	c.reservedMergeRows = 0
	c.reservedMergeBlks = 0
	c.reserved = 0
}

func (c *resourceController) availableMem() int64 {
	avail := c.limit - c.using - c.reserved
	if avail < 0 {
		avail = 0
	}
	return avail
}

func (c *resourceController) printStats() {
	if c.reservedMergeBlks == 0 && c.availableMem() > 512*common.Const1MBytes {
		return
	}

	logutil.Info("MergeExecutorMemoryStats",
		common.AnyField("process-limit", common.HumanReadableBytes(int(c.limit))),
		common.AnyField("process-mem", common.HumanReadableBytes(int(c.using))),
		common.AnyField("inuse-mem", common.HumanReadableBytes(int(c.reserved))),
		common.AnyField("inuse-cnt", c.reservedMergeBlks),
	)
}

func (c *resourceController) reserveResources(objs []*catalog.ObjectEntry) {
	for _, obj := range objs {
		c.reservedMergeRows += int64(obj.Rows())
		c.reserved += estimateMemUsagePerRow * int64(obj.Rows())
		c.reservedMergeBlks += uint64(obj.BlkCnt())
	}
}

func (c *resourceController) resourceAvailable(objs []*catalog.ObjectEntry) bool {
	if c.reservedMergeRows*36 /*28 * 1.3 */ > c.transferPageLimit/8 {
		return false
	}

	mem := c.availableMem()
	if mem > constMaxMemCap {
		mem = constMaxMemCap
	}
	_, eSize := estimateMergeConsume(objs)
	return eSize <= int(2*mem/3)
}

func memInfo(info string) int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	available := int64(0)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), info) {
			_, err = fmt.Sscanf(s.Text(), info+": %d", &available)
			if err != nil {
				return 0
			}
			return available << 10
		}
	}
	if err = s.Err(); err != nil {
		panic(err)
	}
	return 0
}

func objectValid(objectEntry *catalog.ObjectEntry) bool {
	if objectEntry.IsAppendable() {
		return false
	}
	if !objectEntry.IsActive() {
		return false
	}
	if !objectEntry.IsCommitted() {
		return false
	}
	if objectEntry.IsCreatingOrAborted() {
		return false
	}
	return true
}
