// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package exec

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/exec/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/exec/types"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
)

// OrderedSynchronizer receives rows from multiple inputs and produces a single
// stream of rows, ordered according to a set of columns. The rows in each input
// stream are assumed to be ordered according to the same set of columns.
type OrderedSynchronizer struct {
	inputs      []Operator
	ordering    sqlbase.ColumnOrdering
	columnTypes []types.T

	// inputBatches stores the current batch for each input.
	inputBatches []coldata.Batch
	// inputIndices stores the current index into each input batch.
	inputIndices []uint16
	// comparators stores one comparator per ordering column.
	comparators []vecComparator
	output      coldata.Batch
}

// NewOrderedSynchronizer creates a new OrderedSynchronizer.
func NewOrderedSynchronizer(
	inputs []Operator, typs []types.T, ordering sqlbase.ColumnOrdering,
) *OrderedSynchronizer {
	return &OrderedSynchronizer{
		inputs:      inputs,
		ordering:    ordering,
		columnTypes: typs,
	}
}

// Next is part of the Operator interface.
func (o *OrderedSynchronizer) Next(ctx context.Context) coldata.Batch {
	if o.inputBatches == nil {
		o.inputBatches = make([]coldata.Batch, len(o.inputs))
		for i := range o.inputs {
			o.inputBatches[i] = o.inputs[i].Next(ctx)
			o.updateComparators(i)
		}
	}
	outputIdx := uint16(0)
	for outputIdx < coldata.BatchSize {
		// Determine the batch with the smallest row.
		minBatch := -1
		for i := range o.inputs {
			if o.inputBatches[i].Length() == 0 {
				// Input exhausted.
				continue
			}
			if minBatch == -1 || o.compareRow(i, minBatch) < 0 {
				minBatch = i
			}
		}
		if minBatch == -1 {
			// All inputs exhausted.
			break
		}

		// Copy the min row into the output.
		for i := range o.columnTypes {
			batch := o.inputBatches[minBatch]
			vec := batch.ColVec(i)
			srcStartIdx := o.inputIndices[minBatch]
			if sel := batch.Selection(); sel != nil {
				srcStartIdx = sel[srcStartIdx]
			}
			o.output.ColVec(i).Append(
				coldata.AppendArgs{
					ColType:     o.columnTypes[i],
					Src:         vec,
					DestIdx:     uint64(outputIdx),
					SrcStartIdx: srcStartIdx,
					SrcEndIdx:   srcStartIdx + 1,
				},
			)
		}

		// Advance the input batch, fetching a new batch if necessary.
		if o.inputIndices[minBatch]+1 < o.inputBatches[minBatch].Length() {
			o.inputIndices[minBatch]++
		} else {
			o.inputBatches[minBatch] = o.inputs[minBatch].Next(ctx)
			o.inputIndices[minBatch] = 0
			o.updateComparators(minBatch)
		}

		outputIdx++
	}
	o.output.SetLength(outputIdx)
	return o.output
}

// Init is part of the Operator interface.
func (o *OrderedSynchronizer) Init() {
	o.inputIndices = make([]uint16, len(o.inputs))
	o.output = coldata.NewMemBatch(o.columnTypes)
	for i := range o.inputs {
		o.inputs[i].Init()
	}
	o.comparators = make([]vecComparator, len(o.ordering))
	for i := range o.ordering {
		typ := o.columnTypes[o.ordering[i].ColIdx]
		o.comparators[i] = GetVecComparator(typ, len(o.inputs))
	}
}

func (o *OrderedSynchronizer) compareRow(batchIdx1 int, batchIdx2 int) int {
	batch1 := o.inputBatches[batchIdx1]
	batch2 := o.inputBatches[batchIdx2]
	valIdx1 := o.inputIndices[batchIdx1]
	valIdx2 := o.inputIndices[batchIdx2]
	if sel := batch1.Selection(); sel != nil {
		valIdx1 = sel[valIdx1]
	}
	if sel := batch2.Selection(); sel != nil {
		valIdx2 = sel[valIdx2]
	}
	for i := range o.ordering {
		info := o.ordering[i]
		res := o.comparators[i].compare(batchIdx1, batchIdx2, valIdx1, valIdx2)
		if res != 0 {
			switch d := info.Direction; d {
			case encoding.Ascending:
				return res
			case encoding.Descending:
				return -res
			default:
				panic(fmt.Sprintf("unexpected direction value %d", d))
			}
		}
	}
	return 0
}

// updateComparators should be run whenever a new batch is fetched. It updates
// all the relevant vectors in o.comparators.
func (o *OrderedSynchronizer) updateComparators(batchIdx int) {
	batch := o.inputBatches[batchIdx]
	if batch.Length() == 0 {
		return
	}
	for i := range o.ordering {
		vec := batch.ColVec(o.ordering[i].ColIdx)
		o.comparators[i].setVec(batchIdx, vec)
	}
}
