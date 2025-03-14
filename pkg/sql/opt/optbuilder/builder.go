// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package optbuilder

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/delegate"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/norm"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/optgen/exprgen"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/transform"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
)

// Builder holds the context needed for building a memo structure from a SQL
// statement. Builder.Build() is the top-level function to perform this build
// process. As part of the build process, it performs name resolution and
// type checking on the expressions within Builder.stmt.
//
// The memo structure is the primary data structure used for query optimization,
// so building the memo is the first step required to optimize a query. The memo
// is maintained inside Builder.factory, which exposes methods to construct
// expression groups inside the memo. Once the expression tree has been built,
// the builder calls SetRoot on the memo to indicate the root memo group, as
// well as the set of physical properties (e.g., row and column ordering) that
// at least one expression in the root group must satisfy.
//
// A memo is essentially a compact representation of a forest of logically-
// equivalent query trees. Each tree is either a logical or a physical plan
// for executing the SQL query. After the build process is complete, the memo
// forest will contain exactly one tree: the logical query plan corresponding
// to the AST of the original SQL statement with some number of "normalization"
// transformations applied. Normalization transformations include heuristics
// such as predicate push-down that should always be applied. They do not
// include "exploration" transformations whose benefit must be evaluated with
// the optimizer's cost model (e.g., join reordering).
//
// See factory.go and memo.go inside the opt/xform package for more details
// about the memo structure.
type Builder struct {

	// -- Control knobs --
	//
	// These fields can be set before calling Build to control various aspects of
	// the building process.

	// AllowUnsupportedExpr is a control knob: if set, when building a scalar, the
	// builder takes any TypedExpr node that it doesn't recognize and wraps that
	// expression in an UnsupportedExpr node. This is temporary; it is used for
	// interfacing with the old planning code.
	AllowUnsupportedExpr bool

	// KeepPlaceholders is a control knob: if set, optbuilder will never replace
	// a placeholder operator with its assigned value, even when it is available.
	// This is used when re-preparing invalidated queries.
	KeepPlaceholders bool

	// -- Results --
	//
	// These fields are set during the building process and can be used after
	// Build is called.

	// IsCorrelated is set to true during semantic analysis if a scalar variable was
	// pulled from an outer scope, that is, if the query was found to be correlated.
	IsCorrelated bool

	// HadPlaceholders is set to true if we replaced any placeholders with their
	// values.
	HadPlaceholders bool

	// DisableMemoReuse is set to true if we encountered a statement that is not
	// safe to cache the memo for. This is the case for various DDL and SHOW
	// statements.
	DisableMemoReuse bool

	factory *norm.Factory
	stmt    tree.Statement

	ctx              context.Context
	semaCtx          *tree.SemaContext
	evalCtx          *tree.EvalContext
	catalog          cat.Catalog
	exprTransformCtx transform.ExprTransformContext
	scopeAlloc       []scope

	// If set, the planner will skip checking for the SELECT privilege when
	// resolving data sources (tables, views, etc). This is used when compiling
	// views and the view SELECT privilege has already been checked. This should
	// be used with care.
	skipSelectPrivilegeChecks bool

	// views contains a cache of views that have already been parsed, in case they
	// are referenced multiple times in the same query.
	views map[cat.View]*tree.Select

	// subquery contains a pointer to the subquery which is currently being built
	// (if any).
	subquery *subquery
}

// New creates a new Builder structure initialized with the given
// parsed SQL statement.
func New(
	ctx context.Context,
	semaCtx *tree.SemaContext,
	evalCtx *tree.EvalContext,
	catalog cat.Catalog,
	factory *norm.Factory,
	stmt tree.Statement,
) *Builder {
	return &Builder{
		factory: factory,
		stmt:    stmt,
		ctx:     ctx,
		semaCtx: semaCtx,
		evalCtx: evalCtx,
		catalog: catalog,
	}
}

// Build is the top-level function to build the memo structure inside
// Builder.factory from the parsed SQL statement in Builder.stmt. See the
// comment above the Builder type declaration for details.
//
// If any subroutines panic with a builderError or pgerror.Error as part of the
// build process, the panic is caught here and returned as an error.
func (b *Builder) Build() (err error) {
	defer func() {
		if r := recover(); r != nil {
			// This code allows us to propagate semantic and internal errors without
			// adding lots of checks for `if err != nil` throughout the code. This is
			// only possible because the code does not update shared state and does
			// not manipulate locks.
			if e, ok := r.(error); ok {
				err = e
				return
			}
			// Other panic objects can't be considered "safe" and thus are
			// propagated as crashes that terminate the session.
			panic(r)
		}
	}()

	// Special case for CannedOptPlan.
	if canned, ok := b.stmt.(*tree.CannedOptPlan); ok {
		b.factory.DisableOptimizations()
		_, err := exprgen.Build(b.catalog, b.factory, canned.Plan)
		return err
	}

	// Build the memo, and call SetRoot on the memo to indicate the root group
	// and physical properties.
	outScope := b.buildStmt(b.stmt, nil /* desiredTypes */, b.allocScope())
	physical := outScope.makePhysicalProps()
	b.factory.Memo().SetRoot(outScope.expr, physical)
	return nil
}

// builderError is used to wrap errors returned by various external APIs that
// occur during the build process. It exists for us to be able to panic on these
// errors and then catch them inside Builder.Build.
type builderError struct {
	error error
}

// builderError are errors.
func (b builderError) Error() string { return b.error.Error() }

// Cause implements the causer interface. This is used so that builderErrors
// can be peeked through by the common error facilities.
func (b builderError) Cause() error { return b.error }

// unimplementedWithIssueDetailf formats according to a format
// specifier and returns a Postgres error with the
// pg code FeatureNotSupported, wrapped in a
// builderError.
func unimplementedWithIssueDetailf(issue int, detail, format string, args ...interface{}) error {
	return unimplemented.NewWithIssueDetailf(issue, detail, format, args...)
}

// buildStmt builds a set of memo groups that represent the given SQL
// statement.
//
// NOTE: The following descriptions of the inScope parameter and outScope
//       return value apply for all buildXXX() functions in this directory.
//       Note that some buildXXX() functions pass outScope as a parameter
//       rather than a return value so its scopeColumns can be built up
//       incrementally across several function calls.
//
// inScope   This parameter contains the name bindings that are visible for this
//           statement/expression (e.g., passed in from an enclosing statement).
//
// outScope  This return value contains the newly bound variables that will be
//           visible to enclosing statements, as well as a pointer to any
//           "parent" scope that is still visible. The top-level memo expression
//           for the built statement/expression is returned in outScope.expr.
func (b *Builder) buildStmt(
	stmt tree.Statement, desiredTypes []*types.T, inScope *scope,
) (outScope *scope) {
	// NB: The case statements are sorted lexicographically.
	switch stmt := stmt.(type) {
	case *tree.CreateTable:
		return b.buildCreateTable(stmt, inScope)

	case *tree.Delete:
		return b.buildDelete(stmt, inScope)

	case *tree.Explain:
		return b.buildExplain(stmt, inScope)

	case *tree.Insert:
		return b.buildInsert(stmt, inScope)

	case *tree.ParenSelect:
		return b.buildSelect(stmt.Select, desiredTypes, inScope)

	case *tree.Select:
		return b.buildSelect(stmt, desiredTypes, inScope)

	case *tree.ShowTraceForSession:
		return b.buildShowTrace(stmt, inScope)

	case *tree.Update:
		return b.buildUpdate(stmt, inScope)

	default:
		newStmt, err := delegate.TryDelegate(b.ctx, b.catalog, b.evalCtx, stmt)
		if err != nil {
			panic(builderError{err})
		}
		if newStmt != nil {
			// Many delegate implementations resolve objects. It would be tedious to
			// register all those dependencies with the metadata (for cache
			// invalidation). We don't care about caching plans for these statements.
			b.DisableMemoReuse = true
			return b.buildStmt(newStmt, desiredTypes, inScope)
		}
		panic(unimplementedWithIssueDetailf(34848, stmt.StatementTag(), "unsupported statement: %T", stmt))
	}
}

func (b *Builder) allocScope() *scope {
	if len(b.scopeAlloc) == 0 {
		// scope is relatively large (~250 bytes), so only allocate in small
		// chunks.
		b.scopeAlloc = make([]scope, 4)
	}
	r := &b.scopeAlloc[0]
	b.scopeAlloc = b.scopeAlloc[1:]
	r.builder = b
	return r
}
