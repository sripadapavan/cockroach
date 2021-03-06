// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Tamir Duberstein (tamird@gmail.com)

package sql

import (
	"fmt"
	"math"
	"strconv"

	"github.com/cockroachdb/cockroach/sql/parser"
)

// evalLimit evaluates the Count and Offset fields. If Count is missing, the
// value is MaxInt64. If Offset is missing, the value is 0
func (p *planner) evalLimit(limit *parser.Limit) (count, offset int64, err error) {
	count = math.MaxInt64
	offset = 0

	if limit == nil {
		return count, offset, nil
	}

	data := []struct {
		name string
		src  parser.Expr
		dst  *int64
	}{
		{"LIMIT", limit.Count, &count},
		{"OFFSET", limit.Offset, &offset},
	}

	for _, datum := range data {
		if datum.src != nil {
			typedSrc, err := parser.TypeCheckAndRequire(datum.src, p.evalCtx.Args,
				parser.TypeInt, datum.name)
			if err != nil {
				return 0, 0, err
			}

			normalized, err := p.parser.NormalizeExpr(p.evalCtx, typedSrc)
			if err != nil {
				return 0, 0, err
			}

			if p.evalCtx.PrepareOnly {
				continue
			}

			dstDatum, err := normalized.Eval(p.evalCtx)
			if err != nil {
				return 0, 0, err
			}

			if dstDatum == parser.DNull {
				// Use the default value.
				continue
			}

			dstDInt := *dstDatum.(*parser.DInt)
			val := int64(dstDInt)
			if val < 0 {
				return 0, 0, fmt.Errorf("negative value for %s", datum.name)
			}
			*datum.dst = val
		}
	}
	return count, offset, nil
}

// limit constructs a limitNode based on the LIMIT and OFFSET clauses.
func (p *planner) limit(count, offset int64, plan planNode) planNode {
	if count == math.MaxInt64 && offset == 0 {
		return plan
	}

	if count != math.MaxInt64 {
		plan.SetLimitHint(offset+count, false /* hard */)
	}

	return &limitNode{plan: plan, count: count, offset: offset}
}

type limitNode struct {
	plan      planNode
	count     int64
	offset    int64
	rowIndex  int64
	explain   explainMode
	debugVals debugValues
}

func (n *limitNode) ExplainTypes(f func(string, string)) { n.plan.ExplainTypes(f) }
func (n *limitNode) expandPlan() error                   { return n.plan.expandPlan() }
func (n *limitNode) Err() error                          { return n.plan.Err() }
func (n *limitNode) Start() error                        { return n.plan.Start() }
func (n *limitNode) Columns() []ResultColumn             { return n.plan.Columns() }
func (n *limitNode) Values() parser.DTuple               { return n.plan.Values() }
func (n *limitNode) Ordering() orderingInfo              { return n.plan.Ordering() }

func (n *limitNode) MarkDebug(mode explainMode) {
	if mode != explainDebug {
		panic(fmt.Sprintf("unknown debug mode %d", mode))
	}
	n.explain = mode
	n.plan.MarkDebug(mode)
}

func (n *limitNode) DebugValues() debugValues {
	if n.explain != explainDebug {
		panic(fmt.Sprintf("node not in debug mode (mode %d)", n.explain))
	}
	return n.debugVals
}

func (n *limitNode) Next() bool {
	// n.rowIndex is the 0-based index of the next row.
	// We don't do (n.rowIndex >= n.offset + n.count) to avoid overflow (count can be MaxInt64).
	if n.rowIndex-n.offset >= n.count {
		return false
	}

	for {
		if !n.plan.Next() {
			return false
		}

		if n.explain == explainDebug {
			n.debugVals = n.plan.DebugValues()
			if n.debugVals.output != debugValueRow {
				// Let the non-row debug values pass through.
				return true
			}
		}

		n.rowIndex++
		if n.rowIndex > n.offset {
			// Row within limits, return it.
			return true
		}

		if n.explain == explainDebug {
			// Return as a filtered row.
			n.debugVals.output = debugValueFiltered
			return true
		}
		// Fetch the next row.
	}
}

func (n *limitNode) ExplainPlan(_ bool) (string, string, []planNode) {
	var count string
	if n.count == math.MaxInt64 {
		count = "ALL"
	} else {
		count = strconv.FormatInt(n.count, 10)
	}

	return "limit", fmt.Sprintf("count: %s, offset: %d", count, n.offset), []planNode{n.plan}
}

func (*limitNode) SetLimitHint(_ int64, _ bool) {}
