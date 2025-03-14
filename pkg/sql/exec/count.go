// Copyright 2018 The Cockroach Authors.
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

	"github.com/cockroachdb/cockroach/pkg/sql/exec/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/exec/types"
)

// countOp is an operator that counts the number of input rows it receives,
// consuming its entire input and outputting a batch with a single integer
// column containing a single integer, the count of rows received from the
// upstream.
type countOp struct {
	input Operator

	internalBatch coldata.Batch
	done          bool
	count         int64
}

var _ Operator = &countOp{}

// NewCountOp returns a new count operator that counts the rows in its input.
func NewCountOp(input Operator) Operator {
	c := &countOp{
		input: input,
	}
	c.internalBatch = coldata.NewMemBatchWithSize([]types.T{types.Int64}, 1)
	return c
}

func (c *countOp) Init() {
	c.input.Init()
	// Our output is always just one row.
	c.internalBatch.SetLength(1)
	c.count = 0
	c.done = false
}

func (c *countOp) Next(ctx context.Context) coldata.Batch {
	if c.done {
		c.internalBatch.SetLength(0)
		return c.internalBatch
	}
	for {
		bat := c.input.Next(ctx)
		length := bat.Length()
		if length == 0 {
			c.done = true
			c.internalBatch.ColVec(0).Int64()[0] = c.count
			return c.internalBatch
		}
		c.count += int64(length)
	}
}
