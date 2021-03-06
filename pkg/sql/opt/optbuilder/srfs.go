// Copyright 2018 The Cockroach Authors.
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

package optbuilder

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
)

// srf represents an srf expression in an expression tree
// after it has been type-checked and added to the memo.
type srf struct {
	// The resolved function expression.
	*tree.FuncExpr

	// cols contains the output columns of the srf.
	cols []scopeColumn

	// group is the top level memo GroupID of the srf.
	group memo.GroupID
}

// Walk is part of the tree.Expr interface.
func (s *srf) Walk(v tree.Visitor) tree.Expr {
	return s
}

// TypeCheck is part of the tree.Expr interface.
func (s *srf) TypeCheck(ctx *tree.SemaContext, desired types.T) (tree.TypedExpr, error) {
	if ctx.Properties.Derived.SeenGenerator {
		// This error happens if this srf struct is nested inside a raw srf that
		// has not yet been replaced. This is possible since scope.replaceSRF first
		// calls f.Walk(s) on the external raw srf, which replaces any internal
		// raw srfs with srf structs. The next call to TypeCheck on the external
		// raw srf triggers this error.
		return nil, pgerror.UnimplementedWithIssueErrorf(26234, "nested set-returning functions")
	}

	return s, nil
}

// Eval is part of the tree.TypedExpr interface.
func (s *srf) Eval(_ *tree.EvalContext) (tree.Datum, error) {
	panic("srf must be replaced before evaluation")
}

var _ tree.Expr = &srf{}
var _ tree.TypedExpr = &srf{}

// buildZip builds a set of memo groups which represent a functional zip over
// the given expressions.
//
// Reminder, for context: the functional zip over iterators a,b,c
// returns tuples of values from a,b,c picked "simultaneously". NULLs
// are used when an iterator is "shorter" than another. For example:
//
//    zip([1,2,3], ['a','b']) = [(1,'a'), (2,'b'), (3, null)]
//
func (b *Builder) buildZip(exprs tree.Exprs, inScope *scope) (outScope *scope) {
	outScope = inScope.push()

	// We need to save and restore the previous value of the field in
	// semaCtx in case we are recursively called within a subquery
	// context.
	defer b.semaCtx.Properties.Restore(b.semaCtx.Properties)
	b.semaCtx.Properties.Require("FROM",
		tree.RejectAggregates|tree.RejectWindowApplications|tree.RejectNestedGenerators)

	// Build each of the provided expressions.
	elems := make([]memo.GroupID, len(exprs))
	for i, expr := range exprs {
		// Output column names should exactly match the original expression, so we
		// have to determine the output column name before we perform type
		// checking.
		_, label, err := tree.ComputeColNameInternal(b.semaCtx.SearchPath, expr)
		if err != nil {
			panic(builderError{err})
		}

		texpr := inScope.resolveType(expr, types.Any)
		elems[i] = b.buildScalarHelper(texpr, label, inScope, outScope)
	}

	// Get the output columns of the Zip operation and construct the Zip.
	colList := make(opt.ColList, len(outScope.cols))
	for i := 0; i < len(colList); i++ {
		colList[i] = outScope.cols[i].id
	}
	outScope.group = b.factory.ConstructZip(
		b.factory.InternList(elems), b.factory.InternColList(colList),
	)

	return outScope
}

// finishBuildGeneratorFunction finishes building a set-generating function
// (SRF) such as generate_series() or unnest(). It synthesizes new columns in
// outScope for each of the SRF's output columns.
func (b *Builder) finishBuildGeneratorFunction(
	f *tree.FuncExpr, group memo.GroupID, columns int, label string, inScope, outScope *scope,
) (out memo.GroupID) {
	typ := f.ResolvedType()

	// Add scope columns.
	if columns == 1 {
		// Single-column return type.
		b.synthesizeColumn(outScope, label, typ, f, group)
	} else {
		// Multi-column return type. Use the tuple labels in the SRF's return type
		// as column labels.
		tType := typ.(types.TTuple)
		for i := range tType.Types {
			b.synthesizeColumn(outScope, tType.Labels[i], tType.Types[i], nil, group)
		}
	}

	return group
}

// constructProjectSet constructs a lateral cross join between the given input
// group and a Zip operation constructed from the given srfs.
//
// This function is called at most once per SELECT clause, and it is only
// called if at least one SRF was discovered in the SELECT list. The apply join
// is necessary in case some of the SRFs depend on the input. For example,
// consider this query:
//
//   SELECT generate_series(t.a, t.a + 1) FROM t
//
// In this case, the inputs to generate_series depend on table t, so during
// execution, generate_series will be called once for each row of t (hence the
// apply join).
//
// If the SRFs do not depend on the input, then the optimizer will replace the
// apply join with a regular inner join during optimization.
func (b *Builder) constructProjectSet(in memo.GroupID, srfs []*srf) memo.GroupID {
	// Get the output columns and GroupIDs of the Zip operation.
	colList := make(opt.ColList, 0, len(srfs))
	elems := make([]memo.GroupID, len(srfs))
	for i, srf := range srfs {
		for _, col := range srf.cols {
			colList = append(colList, col.id)
		}
		elems[i] = srf.group
	}

	zip := b.factory.ConstructZip(
		b.factory.InternList(elems), b.factory.InternColList(colList),
	)

	return b.factory.ConstructInnerJoinApply(in, zip, b.factory.ConstructTrue())
}
