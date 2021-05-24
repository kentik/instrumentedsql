package instrumentedsql

import (
	"context"
	"database/sql/driver"
	"time"
)

type WrappedConn struct {
	opts
	Parent driver.Conn
}

// Compile time validation that our types implement the expected interfaces
var (
	_ driver.Conn               = WrappedConn{}
	_ driver.ConnBeginTx        = WrappedConn{}
	_ driver.ConnPrepareContext = WrappedConn{}
	_ driver.Execer             = WrappedConn{}
	_ driver.ExecerContext      = WrappedConn{}
	_ driver.Pinger             = WrappedConn{}
	_ driver.Queryer            = WrappedConn{}
	_ driver.QueryerContext     = WrappedConn{}
)

func (c WrappedConn) Prepare(query string) (driver.Stmt, error) {
	parent, err := c.Parent.Prepare(query)
	if err != nil {
		return nil, err
	}

	return wrappedStmt{opts: c.opts, query: query, parent: parent}, nil
}

func (c WrappedConn) Close() error {
	return c.Parent.Close()
}

func (c WrappedConn) Begin() (driver.Tx, error) {
	tx, err := c.Parent.Begin()
	if err != nil {
		return nil, err
	}

	return wrappedTx{opts: c.opts, parent: tx}, nil
}

func (c WrappedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (tx driver.Tx, err error) {
	if !c.hasOpExcluded(OpSQLTxBegin) {
		span := c.GetSpan(ctx).NewChild(OpSQLTxBegin)
		span.SetLabel("component", "database/sql")
		start := time.Now()
		defer func() {
			span.SetError(err)
			span.Finish()
			c.Log(ctx, OpSQLTxBegin, "err", err, "duration", time.Since(start))
		}()
	}

	if connBeginTx, ok := c.Parent.(driver.ConnBeginTx); ok {
		tx, err = connBeginTx.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}

		return wrappedTx{opts: c.opts, ctx: ctx, parent: tx}, nil
	}

	tx, err = c.Parent.Begin()
	if err != nil {
		return nil, err
	}

	return wrappedTx{opts: c.opts, ctx: ctx, parent: tx}, nil
}

func (c WrappedConn) PrepareContext(ctx context.Context, query string) (stmt driver.Stmt, err error) {
	if !c.hasOpExcluded(OpSQLPrepare) {
		span := c.GetSpan(ctx).NewChild(OpSQLPrepare)
		span.SetLabel("component", "database/sql")
		start := time.Now()
		defer func() {
			span.SetError(err)
			span.Finish()
			logQuery(ctx, c.opts, OpSQLPrepare, query, err, nil, start)
		}()
	}

	if connPrepareCtx, ok := c.Parent.(driver.ConnPrepareContext); ok {
		stmt, err := connPrepareCtx.PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}

		return wrappedStmt{opts: c.opts, ctx: ctx, query: query, parent: stmt}, nil
	}

	return c.Prepare(query)
}

func (c WrappedConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if execer, ok := c.Parent.(driver.Execer); ok {
		res, err := execer.Exec(query, args)
		if err != nil {
			return nil, err
		}

		return wrappedResult{opts: c.opts, parent: res}, nil
	}

	return nil, driver.ErrSkip
}

func (c WrappedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (r driver.Result, err error) {
	if !c.hasOpExcluded(OpSQLConnExec) {
		span := c.GetSpan(ctx).NewChild(OpSQLConnExec)
		span.SetLabel("component", "database/sql")
		span.SetLabel("query", query)
		if !c.OmitArgs {
			span.SetLabel("args", formatArgs(args))
		}
		start := time.Now()
		defer func() {
			span.SetError(err)
			span.Finish()

			logQuery(ctx, c.opts, OpSQLConnExec, query, err, args, start)
		}()
	}

	if execContext, ok := c.Parent.(driver.ExecerContext); ok {
		res, err := execContext.ExecContext(ctx, query, args)
		if err != nil {
			return nil, err
		}

		return wrappedResult{opts: c.opts, ctx: ctx, parent: res}, nil
	}

	// Fallback implementation
	dargs, err := namedValueToValue(args)
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return c.Exec(query, dargs)
	}
}

func (c WrappedConn) Ping(ctx context.Context) (err error) {
	if pinger, ok := c.Parent.(driver.Pinger); ok {
		if !c.hasOpExcluded(OpSQLPing) {
			span := c.GetSpan(ctx).NewChild(OpSQLPing)
			span.SetLabel("component", "database/sql")
			start := time.Now()
			defer func() {
				span.SetError(err)
				span.Finish()
				c.Log(ctx, OpSQLPing, "err", err, "duration", time.Since(start))
			}()
		}

		return pinger.Ping(ctx)
	}

	c.Log(ctx, OpSQLDummyPing, "duration", time.Duration(0))

	return nil
}

func (c WrappedConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	if queryer, ok := c.Parent.(driver.Queryer); ok {
		rows, err := queryer.Query(query, args)
		if err != nil {
			return nil, err
		}

		return wrappedRows{opts: c.opts, parent: rows}, nil
	}

	return nil, driver.ErrSkip
}

func (c WrappedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (rows driver.Rows, err error) {
	// Quick skip path: If the wrapped connection implements neither QueryerContext nor Queryer, we have absolutely nothing to do
	_, hasQueryerContext := c.Parent.(driver.QueryerContext)
	_, hasQueryer := c.Parent.(driver.Queryer)
	if !hasQueryerContext && !hasQueryer {
		return nil, driver.ErrSkip
	}

	if !c.hasOpExcluded(OpSQLConnQuery) {
		span := c.GetSpan(ctx).NewChild(OpSQLConnQuery)
		span.SetLabel("component", "database/sql")
		span.SetLabel("query", query)
		if !c.OmitArgs {
			span.SetLabel("args", formatArgs(args))
		}
		start := time.Now()
		defer func() {
			span.SetError(err)
			span.Finish()
			logQuery(ctx, c.opts, OpSQLConnQuery, query, err, args, start)
		}()
	}

	if queryerContext, ok := c.Parent.(driver.QueryerContext); ok {
		rows, err := queryerContext.QueryContext(ctx, query, args)
		if err != nil {
			return nil, err
		}

		return wrappedRows{opts: c.opts, ctx: ctx, parent: rows}, nil
	}

	dargs, err := namedValueToValue(args)
	if err != nil {
		return nil, err
	}

	select {
	default:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return c.Query(query, dargs)
}
