// Copyright 2017 PingCAP, Inc.
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

package chunk

import (
	"fmt"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/stringutil"
)

// List is interface for ListInMemory and ListInDisk
type List interface {
	NumRowsOfChunk(int) int
	NumChunks() int
	GetRow(RowPtr) (row Row, err error)
	Add(chk *Chunk) (err error)
}

// ListInMemory holds a slice of chunks, use to append rows with max chunk size properly handled.
type ListInMemory struct {
	fieldTypes    []*types.FieldType
	initChunkSize int
	maxChunkSize  int
	length        int
	chunks        []*Chunk
	freelist      []*Chunk

	memTracker  *memory.Tracker // track memory usage.
	consumedIdx int             // chunk index in "chunks", has been consumed.
}

// RowPtr is used to get a row from a list.
// It is only valid for the list that returns it.
type RowPtr struct {
	ChkIdx uint32
	RowIdx uint32
}

var chunkListLabel fmt.Stringer = stringutil.StringerStr("chunk.ListInMemory")

// NewListInMemory creates a new ListInMemory with field types, init chunk size and max chunk size.
func NewListInMemory(fieldTypes []*types.FieldType, initChunkSize, maxChunkSize int) *ListInMemory {
	l := &ListInMemory{
		fieldTypes:    fieldTypes,
		initChunkSize: initChunkSize,
		maxChunkSize:  maxChunkSize,
		memTracker:    memory.NewTracker(chunkListLabel, -1),
		consumedIdx:   -1,
	}
	return l
}

// GetMemTracker returns the memory tracker of this ListInMemory.
func (l *ListInMemory) GetMemTracker() *memory.Tracker {
	return l.memTracker
}

// Len returns the length of the ListInMemory.
func (l *ListInMemory) Len() int {
	return l.length
}

// NumChunks returns the number of chunks in the ListInMemory.
func (l *ListInMemory) NumChunks() int {
	return len(l.chunks)
}

// GetChunk gets the Chunk by ChkIdx.
func (l *ListInMemory) GetChunk(chkIdx int) *Chunk {
	return l.chunks[chkIdx]
}

// AppendRow appends a row to the ListInMemory, the row is copied to the ListInMemory.
func (l *ListInMemory) AppendRow(row Row) RowPtr {
	chkIdx := len(l.chunks) - 1
	if chkIdx == -1 || l.chunks[chkIdx].NumRows() >= l.chunks[chkIdx].Capacity() || chkIdx == l.consumedIdx {
		newChk := l.allocChunk()
		l.chunks = append(l.chunks, newChk)
		if chkIdx != l.consumedIdx {
			l.memTracker.Consume(l.chunks[chkIdx].MemoryUsage())
			l.consumedIdx = chkIdx
		}
		chkIdx++
	}
	chk := l.chunks[chkIdx]
	rowIdx := chk.NumRows()
	chk.AppendRow(row)
	l.length++
	return RowPtr{ChkIdx: uint32(chkIdx), RowIdx: uint32(rowIdx)}
}

// Add adds a chunk to the ListInMemory, the chunk may be modified later by the list.
// Caller must make sure the input chk is not empty and not used any more and has the same field types.
func (l *ListInMemory) Add(chk *Chunk) (err error) {
	// FixMe: we should avoid add a Chunk that chk.NumRows() > list.maxChunkSize.
	if chk.NumRows() == 0 {
		return errors.New("chunk appended to ListInMemory should have at least 1 row")
	}
	if chkIdx := len(l.chunks) - 1; l.consumedIdx != chkIdx {
		l.memTracker.Consume(l.chunks[chkIdx].MemoryUsage())
		l.consumedIdx = chkIdx
	}
	l.memTracker.Consume(chk.MemoryUsage())
	l.consumedIdx++
	l.chunks = append(l.chunks, chk)
	l.length += chk.NumRows()
	return nil
}

func (l *ListInMemory) allocChunk() (chk *Chunk) {
	if len(l.freelist) > 0 {
		lastIdx := len(l.freelist) - 1
		chk = l.freelist[lastIdx]
		l.freelist = l.freelist[:lastIdx]
		l.memTracker.Consume(-chk.MemoryUsage())
		chk.Reset()
		return
	}
	if len(l.chunks) > 0 {
		return Renew(l.chunks[len(l.chunks)-1], l.maxChunkSize)
	}
	return New(l.fieldTypes, l.initChunkSize, l.maxChunkSize)
}

// GetRow gets a Row from the list by RowPtr,
// error will be promised to be nil but due to keeping the same interface with ListInDisk
func (l *ListInMemory) GetRow(ptr RowPtr) (row Row, err error) {
	chk := l.chunks[ptr.ChkIdx]
	return chk.GetRow(int(ptr.RowIdx)), nil
}

// NumRowsOfChunk returns the number of rows of a chunk.
func (l *ListInMemory) NumRowsOfChunk(chkID int) int {
	return l.GetChunk(chkID).NumRows()
}

// Reset resets the ListInMemory.
func (l *ListInMemory) Reset() {
	if lastIdx := len(l.chunks) - 1; lastIdx != l.consumedIdx {
		l.memTracker.Consume(l.chunks[lastIdx].MemoryUsage())
	}
	l.freelist = append(l.freelist, l.chunks...)
	l.chunks = l.chunks[:0]
	l.length = 0
	l.consumedIdx = -1
}

// preAlloc4Row pre-allocates the storage memory for a Row.
// NOTE: only used in test
// 1. The ListInMemory must be empty or holds no useful data.
// 2. The schema of the Row must be the same with the ListInMemory.
// 3. This API is paired with the `Insert()` function, which inserts all the
//    rows data into the ListInMemory after the pre-allocation.
func (l *ListInMemory) preAlloc4Row(row Row) (ptr RowPtr) {
	chkIdx := len(l.chunks) - 1
	if chkIdx == -1 || l.chunks[chkIdx].NumRows() >= l.chunks[chkIdx].Capacity() {
		newChk := l.allocChunk()
		l.chunks = append(l.chunks, newChk)
		if chkIdx != l.consumedIdx {
			l.memTracker.Consume(l.chunks[chkIdx].MemoryUsage())
			l.consumedIdx = chkIdx
		}
		chkIdx++
	}
	chk := l.chunks[chkIdx]
	rowIdx := chk.preAlloc(row)
	l.length++
	return RowPtr{ChkIdx: uint32(chkIdx), RowIdx: uint32(rowIdx)}
}

// Insert inserts `row` on the position specified by `ptr`.
// Note: Insert will cover the origin data, it should be called after
// PreAlloc.
func (l *ListInMemory) Insert(ptr RowPtr, row Row) {
	l.chunks[ptr.ChkIdx].insert(int(ptr.RowIdx), row)
}

// ListWalkFunc is used to walk the list.
// If error is returned, it will stop walking.
type ListWalkFunc = func(row Row) error

// Walk iterate the list and call walkFunc for each row.
func (l *ListInMemory) Walk(walkFunc ListWalkFunc) error {
	for i := 0; i < len(l.chunks); i++ {
		chk := l.chunks[i]
		for j := 0; j < chk.NumRows(); j++ {
			err := walkFunc(chk.GetRow(j))
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}
