/*
Copyright 2018 Iguazio Systems Ltd.

Licensed under the Apache License, Version 2.0 (the "License") with
an addition restriction as set forth herein. You may not use this
file except in compliance with the License. You may obtain a copy of
the License at http://www.apache.org/licenses/LICENSE-2.0.

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.

In addition, you may not use the software for any purposes that are
illegal under applicable law, and the grant of the foregoing license
under the Apache 2.0 license is conditioned upon your compliance with
such restriction.
*/

package appender

import (
	"fmt"
	"github.com/v3io/v3io-go-http"
	"github.com/v3io/v3io-tsdb/chunkenc"
	"github.com/v3io/v3io-tsdb/partmgr"
)

var MaxArraySize = 1024

const MAX_LATE_WRITE = 59 * 3600 * 1000 // max late arrival of 59min

func NewChunkStore() *chunkStore {
	store := chunkStore{}
	store.chunks[0] = &attrAppender{}
	store.chunks[1] = &attrAppender{}
	return &store
}

// store latest + previous chunks and their state
type chunkStore struct {
	state    storeState
	curChunk int
	lastTid  int
	pending  []pendingData
	chunks   [2]*attrAppender
}

// chunk appender
type attrAppender struct {
	appender  chunkenc.Appender
	partition *partmgr.DBPartition
	updMarker int
	updCount  int
	lastT     int64
	chunkMint int64
	writing   bool
}

func (a *attrAppender) initialize(partition *partmgr.DBPartition, t int64) {
	a.updCount = 0
	a.updMarker = 0
	a.lastT = 0
	a.writing = false
	a.partition = partition
	a.chunkMint = partition.GetChunkMint(t)
}

func (a *attrAppender) inRange(t int64) bool {
	return a.partition.InChunkRange(a.chunkMint, t)
}

func (a *attrAppender) isAhead(t int64) bool {
	return a.partition.IsAheadOfChunk(a.chunkMint, t)
}

// TODO: change appender from float to interface (allow map[str]interface cols)
func (a *attrAppender) appendAttr(t int64, v interface{}) {
	a.appender.Append(t, v.(float64))
	if t > a.lastT {
		a.lastT = t
	}
}

type pendingData struct {
	t int64
	v interface{}
}

type pendingList []pendingData

func (l pendingList) Len() int           { return len(l) }
func (l pendingList) Less(i, j int) bool { return l[i].t < l[j].t }
func (l pendingList) Swap(i, j int)      { l[i], l[j] = l[j], l[i] }

type storeState uint8

const (
	storeStateInit   storeState = 0
	storeStateGet    storeState = 1 // Getting old state from storage
	storeStateReady  storeState = 2 // Ready to update
	storeStateUpdate storeState = 3 // Update/write in progress
	storeStateSort   storeState = 3 // TBD sort chunk(s) in case of late arrivals
)

func (cs *chunkStore) IsReady() bool {
	return cs.state == storeStateReady
}

func (cs *chunkStore) GetState() storeState {
	return cs.state
}

func (cs *chunkStore) SetState(state storeState) {
	cs.state = state
}

// return how many un-commited writes
func (cs *chunkStore) UpdatesBehind() int {
	updatesBehind := len(cs.pending)
	for _, chunk := range cs.chunks {
		if chunk.appender != nil {
			updatesBehind += chunk.appender.Chunk().NumSamples() - chunk.updCount
		}
	}
	return updatesBehind
}

func (cs *chunkStore) GetMetricPath(metric *MetricState, basePath, table string) string {
	return fmt.Sprintf("%s/%s.%d", basePath, metric.name, metric.hash) // TODO: use TableID
}

// Read (Async) the current chunk state and data from the storage
func (cs *chunkStore) GetChunksState(mc *MetricsCache, metric *MetricState, t int64) error {

	// clac table mint
	// Get record meta + relevant attr
	// return get resp id & err

	part := mc.partitionMngr.TimeToPart(t)
	cs.chunks[0].initialize(part, t)
	chunk := chunkenc.NewXORChunk() // TODO: init based on schema, use init function
	app, _ := chunk.Appender()
	cs.chunks[0].appender = app

	if mc.cfg.OverrideOld {

		cs.state = storeStateReady
		return nil
	}

	path := cs.GetMetricPath(metric, mc.cfg.Path, part.GetPath())
	getInput := v3io.GetItemInput{
		Path: path, AttributeNames: []string{"_maxtime", "_meta"}}

	request, err := mc.container.GetItem(&getInput, mc.getRespChan)
	if err != nil {
		mc.logger.ErrorWith("GetItem Failed", "metric", metric.Lset, "err", err)
		return err
	}

	mc.logger.DebugWith("GetItems", "name", metric.name, "key", metric.key, "reqid", request.ID)
	mc.rmapMtx.Lock()
	defer mc.rmapMtx.Unlock()
	mc.requestsMap[request.ID] = metric

	cs.state = storeStateGet
	return nil

}

// Process the GetItem response from the storage and initialize or restore the current chunk
func (cs *chunkStore) ProcessGetResp(mc *MetricsCache, metric *MetricState, resp *v3io.Response) {
	// init chunk from resp (attr)
	// append last t/v into the chunk and clear pending

	cs.state = storeStateReady

	if resp.Error != nil {
		// assume the item not found   TODO: check error code
		return
	}

	item := resp.Output.(*v3io.GetItemOutput).Item
	var maxTime int64
	val := item["_maxtime"]
	if val != nil {
		maxTime = int64(val.(int))
	}
	mc.logger.DebugWith("Got Item", "name", metric.name, "key", metric.key, "maxt", maxTime)

	// TODO: using blob append, any implications ?

	// set Last TableId, no need to create metric object
	cs.lastTid = cs.chunks[0].partition.GetId()

}

// Append data to the right chunk and table based on the time and state
func (cs *chunkStore) Append(t int64, v interface{}) error {

	// if it was just created and waiting to get the state we append to temporary list
	if cs.state == storeStateGet || cs.state == storeStateInit {
		cs.pending = append(cs.pending, pendingData{t: t, v: v})
		return nil
	}

	cur := cs.chunks[cs.curChunk]
	if cur.inRange(t) {
		// Append t/v to current chunk
		cur.appendAttr(t, v)
		return nil
	}

	if cur.isAhead(t) {
		// time is ahead of this chunk time, advance cur chunk
		part := cur.partition
		cur = cs.chunks[cs.curChunk^1]

		// avoid append if there are still writes to old chunk
		if cur.writing {
			return fmt.Errorf("Error, append beyound cur chunk while IO in flight to prev", t)
		}

		chunk := chunkenc.NewXORChunk() // TODO: init based on schema, use init function
		app, err := chunk.Appender()
		if err != nil {
			return err
		}
		cur.initialize(part.NextPart(t), t) // TODO: next part
		cur.appender = app
		cur.appendAttr(t, v)
		cs.curChunk = cs.curChunk ^ 1

		return nil
	}

	// write to an older chunk

	prev := cs.chunks[cs.curChunk^1]
	// delayed Appends only allowed to previous chunk or within allowed window
	if prev.inRange(t) && t > cur.lastT-MAX_LATE_WRITE {
		// Append t/v to previous chunk
		prev.appendAttr(t, v)
		return nil
	}

	// if (mint - t) in allowed window may need to advance next (assume prev is not the one right before cur)

	return nil
}

// Write pending data of the current or previous chunk to the storage
func (cs *chunkStore) WriteChunks(mc *MetricsCache, metric *MetricState) error {

	tableId := -1
	expr := ""

	if cs.state == storeStateGet {
		// cannot write if restoring chunk state is in progress
		return nil
	}

	// TODO: maybe have a bool to indicate if we have new appends vs checking all the chunks..

	notInitialized := false //TODO: init depend on get

	for i := 0; i < 2; i++ {
		chunk := cs.chunks[cs.curChunk^(i&1)]

		if chunk.partition != nil {
			tid := chunk.partition.GetId()

			// write to a single table partition at a time, updated to 2nd partition will wait for next round
			if chunk.appender != nil && (tableId == -1 || tableId == tid) {

				samples := chunk.appender.Chunk().NumSamples()
				if samples > chunk.updCount {
					tableId = tid
					cs.state = storeStateUpdate
					meta, offsetByte, b := chunk.appender.Chunk().GetChunkBuffer()
					chunk.updMarker = ((offsetByte + len(b) - 1) / 8) * 8
					chunk.updCount = samples
					chunk.writing = true

					//notInitialized = (offsetByte == 0)
					if tableId > cs.lastTid {
						notInitialized = true
						cs.lastTid = tableId
					}
					expr = expr + chunkbuf2Expr(offsetByte, meta, b, chunk)

				}
			}

		}
	}

	if tableId == -1 { // no updates
		return nil
	}

	if notInitialized { // TODO: not correct, need to init once per metric/partition, maybe use cond expressions
		lblexpr := ""
		for _, lbl := range metric.Lset {
			if lbl.Name != "__name__" {
				lblexpr = lblexpr + fmt.Sprintf("%s='%s'; ", lbl.Name, lbl.Value)
			} else {
				lblexpr = lblexpr + fmt.Sprintf("_name='%s'; ", lbl.Value)
			}
		}
		expr = lblexpr + fmt.Sprintf("_lset='%s'; _meta=init_array(%d,'int'); ",
			metric.key, 24) + expr // TODO: compute meta arr size
	}

	path := cs.GetMetricPath(metric, mc.cfg.Path, "") // TODO: use TableID
	request, err := mc.container.UpdateItem(&v3io.UpdateItemInput{Path: path, Expression: &expr}, mc.responseChan)
	if err != nil {
		mc.logger.ErrorWith("UpdateItem Failed", "err", err)
		return err
	}

	mc.logger.DebugWith("updateMetric expression", "name", metric.name, "key", metric.key, "expr", expr, "reqid", request.ID)
	mc.rmapMtx.Lock()
	defer mc.rmapMtx.Unlock()
	mc.requestsMap[request.ID] = metric

	return nil
}

// Process the response for the chunk update request
func (cs *chunkStore) ProcessWriteResp() {

	for _, chunk := range cs.chunks {
		if chunk.writing {
			chunk.appender.Chunk().MoveOffset(uint16(chunk.updMarker))
			chunk.writing = false
		}
	}

	cs.state = storeStateReady

	for _, data := range cs.pending {
		cs.Append(data.t, data.v)
	}

}

func chunkbuf2Expr(offsetByte int, meta uint64, bytes []byte, app *attrAppender) string {

	expr := ""
	offset := offsetByte / 8
	ui := chunkenc.ToUint64(bytes)
	idx := app.partition.TimeToChunkId(app.chunkMint) // TODO: add DaysPerObj from part manager
	attr := app.partition.ChunkID2Attr("v", idx)

	if offsetByte == 0 {
		expr = expr + fmt.Sprintf("%s=init_array(%d,'int'); ", attr, MaxArraySize)
	}

	expr = expr + fmt.Sprintf("_meta[%d]=%d; ", idx, meta) // TODO: meta name as col variable
	for i := 0; i < len(ui); i++ {
		expr = expr + fmt.Sprintf("%s[%d]=%d; ", attr, offset, int64(ui[i]))
		offset++
	}
	expr += fmt.Sprintf("_maxtime=%d", app.lastT) // TODO: use max() expr

	return expr
}
