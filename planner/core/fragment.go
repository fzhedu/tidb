// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/ranger"
	"go.uber.org/zap"
)

// Fragment is cut from the whole pushed-down plan by network communication.
// Communication by pfs are always through shuffling / broadcasting / passing through.
type Fragment struct {
	/// following field are filled during getPlanFragment.
	// TODO: Strictly speaking, not all plan fragment contain table scan. we can do this assumption until more plans are supported.
	TableScan         *PhysicalTableScan          // result physical table scan
	ExchangeReceivers []*PhysicalExchangeReceiver // data receivers

	// following fields are filled after scheduling.
	ExchangeSender *PhysicalExchangeSender // data exporter
}

type mppTaskGenerator struct {
	ctx         sessionctx.Context
	startTS     uint64
	allocTaskID int64
}

// GenerateRootMPPTasks generate all mpp tasks and return root ones.
func GenerateRootMPPTasks(ctx sessionctx.Context, startTs uint64, sender *PhysicalExchangeSender) ([]*kv.MPPTask, error) {
	g := &mppTaskGenerator{ctx: ctx, startTS: startTs}
	return g.generateMPPTasks(sender)
}

func (e *mppTaskGenerator) generateMPPTasks(s *PhysicalExchangeSender) ([]*kv.MPPTask, error) {
	logutil.BgLogger().Info("Mpp will generate tasks", zap.String("plan", ToString(s)))
	tidbTask := &kv.MPPTask{
		StartTs: e.startTS,
		ID:      -1,
	}
	rootTasks, err := e.generateMPPTasksForFragment(s.Fragment)
	if err != nil {
		return nil, errors.Trace(err)
	}
	s.Tasks = []*kv.MPPTask{tidbTask}
	return rootTasks, nil
}

func (e *mppTaskGenerator) generateMPPTasksForFragment(f *Fragment) (tasks []*kv.MPPTask, err error) {
	for _, r := range f.ExchangeReceivers {
		r.Tasks, err = e.generateMPPTasksForFragment(r.ChildPf)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	if f.TableScan != nil {
		tasks, err = e.constructSinglePhysicalTable(context.Background(), f.TableScan.Table.ID, f.TableScan.Ranges)
	} else {
		tasks, err = e.constructSinglePhysicalTable(context.Background(), -1, nil)
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(tasks) == 0 {
		return nil, errors.New("cannot find mpp task")
	}
	for _, r := range f.ExchangeReceivers {
		s := r.ChildPf.ExchangeSender
		s.Tasks = tasks
	}
	return tasks, nil
}

// single physical table means a table without partitions or a single partition in a partition table.
func (e *mppTaskGenerator) constructSinglePhysicalTable(ctx context.Context, tableID int64, ranges []*ranger.Range) ([]*kv.MPPTask, error) {
	var kvRanges []kv.KeyRange
	if tableID != -1 {
		kvRanges = distsql.TableRangesToKVRanges(tableID, ranges, nil)
	}
	req := &kv.MPPBuildTasksRequest{KeyRanges: kvRanges}
	metas, err := e.ctx.GetMPPClient().ConstructMPPTasks(ctx, req)
	if err != nil {
		return nil, errors.Trace(err)
	}
	tasks := make([]*kv.MPPTask, 0, len(metas))
	for _, meta := range metas {
		e.allocTaskID++
		tasks = append(tasks, &kv.MPPTask{Meta: meta, ID: e.allocTaskID, StartTs: e.startTS, TableID: tableID})
	}
	return tasks, nil
}
