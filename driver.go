package ocsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"go.opencensus.io/trace"
)

var (
	regMu              sync.Mutex
	attrMissingContext = trace.StringAttribute("ocsql.warning", "missing upstream context")
	attrDeprecated     = trace.StringAttribute("ocsql.warning", "database driver uses deprecated features")
)

type ocDriver struct {
	parent  driver.Driver
	options TraceOptions
}

type ocConn struct {
	parent  driver.Conn
	options TraceOptions
}

type ocTx struct {
	parent  driver.Tx
	ctx     context.Context
	options TraceOptions
}

type ocStmt struct {
	parent  driver.Stmt
	query   string
	options TraceOptions
}

type ocResult struct {
	parent  driver.Result
	ctx     context.Context
	options TraceOptions
}

type ocRows struct {
	parent  driver.Rows
	ctx     context.Context
	options TraceOptions
}

// Register initializes and registers our ocsql wrapped database driver
// identified by its driverName and using provided TraceOptions. On success it
// returns the generated driverName to use when calling sql.Open.
func Register(driverName string, options ...TraceOption) (string, error) {
	// retrieve the driver implementation we need to wrap with instrumentation
	db, err := sql.Open(driverName, "")
	if err != nil {
		return "", err
	}
	dri := db.Driver()
	if err = db.Close(); err != nil {
		return "", err
	}

	regMu.Lock()
	defer regMu.Unlock()

	// Since we might want to register multiple ocsql drivers to have different
	// TraceOptions, but potentially the same underlying database driver, we
	// cycle through available driver names.
	driverName = "ocsql-" + driverName
	for i := int64(0); i < 100; i++ {
		var (
			found   = false
			regName = driverName + strconv.FormatInt(i, 10)
		)
		for _, name := range sql.Drivers() {
			if name == regName {
				found = true
			}
		}
		if !found {
			sql.Register(driverName, Wrap(dri, options...))
			return driverName, nil
		}
	}
	return "", errors.New("unable to register driver, all slots have been taken")
}

// Wrap takes a SQL driver and wraps it with OpenCensus instrumentation.
func Wrap(d driver.Driver, options ...TraceOption) driver.Driver {
	o := TraceOptions{}
	for _, option := range options {
		option(&o)
	}
	if o.QueryParams && !o.Query {
		o.QueryParams = false
	}
	return ocDriver{parent: d, options: o}
}

func (d ocDriver) Open(name string) (driver.Conn, error) {
	c, err := d.parent.Open(name)
	if err != nil {
		return nil, err
	}
	return &ocConn{parent: c, options: d.options}, nil
}

func (c ocConn) Ping(ctx context.Context) (err error) {
	if c.options.Ping {
		var span *trace.Span
		ctx, span = trace.StartSpan(ctx, "sql:ping")
		defer func() {
			if err != nil {
				span.SetStatus(trace.Status{
					Code:    trace.StatusCodeUnavailable,
					Message: err.Error(),
				})
			} else {
				span.SetStatus(trace.Status{Code: trace.StatusCodeOK})
			}
			span.End()
		}()
	}

	if pinger, ok := c.parent.(driver.Pinger); ok {
		err = pinger.Ping(ctx)
	}
	return
}

func (c ocConn) Exec(query string, args []driver.Value) (res driver.Result, err error) {
	if exec, ok := c.parent.(driver.Execer); ok {
		ctx, span := trace.StartSpan(context.Background(), "sql:exec")
		attrs := []trace.Attribute{
			attrDeprecated,
			trace.StringAttribute(
				"ocsql.deprecated", "driver does not support ExecerContext",
			),
		}
		if c.options.Query {
			attrs = append(attrs, trace.StringAttribute("sql.query", query))
			if c.options.QueryParams {
				attrs = append(attrs, paramsAttr(args)...)
			}
		}
		span.AddAttributes(attrs...)

		defer func() {
			setSpanStatus(span, err)
			span.End()
		}()

		if res, err = exec.Exec(query, args); err != nil {
			return nil, err
		}

		return ocResult{parent: res, ctx: ctx, options: c.options}, nil
	}

	return nil, driver.ErrSkip
}

func (c ocConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (res driver.Result, err error) {
	if execCtx, ok := c.parent.(driver.ExecerContext); ok {
		var span *trace.Span
		ctx, span = trace.StartSpan(ctx, "sql:exec")
		if c.options.Query {
			attrs := []trace.Attribute{
				trace.StringAttribute("sql.query", query),
			}
			if c.options.QueryParams {
				attrs = append(attrs, namedParamsAttr(args)...)
			}
			span.AddAttributes(attrs...)
		}

		defer func() {
			setSpanStatus(span, err)
			span.End()
		}()

		if res, err = execCtx.ExecContext(ctx, query, args); err != nil {
			return nil, err
		}

		return ocResult{parent: res, ctx: ctx, options: c.options}, nil
	}

	return nil, driver.ErrSkip
}

func (c ocConn) Query(query string, args []driver.Value) (rows driver.Rows, err error) {
	if queryer, ok := c.parent.(driver.Queryer); ok {
		ctx, span := trace.StartSpan(context.Background(), "sql:query")
		attrs := []trace.Attribute{
			attrDeprecated,
			trace.StringAttribute(
				"ocsql.deprecated", "driver does not support QueryerContext",
			),
		}
		if c.options.Query {
			attrs = append(attrs, trace.StringAttribute("sql.query", query))
			if c.options.QueryParams {
				attrs = append(attrs, paramsAttr(args)...)
			}
		}
		span.AddAttributes(attrs...)

		defer func() {
			setSpanStatus(span, err)
			span.End()
		}()

		rows, err = queryer.Query(query, args)
		if err != nil {
			return nil, err
		}

		return ocRows{parent: rows, ctx: ctx, options: c.options}, nil
	}

	return nil, driver.ErrSkip
}

func (c ocConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (rows driver.Rows, err error) {
	if queryerCtx, ok := c.parent.(driver.QueryerContext); ok {
		var span *trace.Span
		ctx, span = trace.StartSpan(ctx, "sql:query")
		if c.options.Query {
			attrs := []trace.Attribute{
				trace.StringAttribute("sql.query", query),
			}
			if c.options.QueryParams {
				attrs = append(attrs, namedParamsAttr(args)...)
			}
			span.AddAttributes(attrs...)
		}

		defer func() {
			setSpanStatus(span, err)
			span.End()
		}()

		rows, err = queryerCtx.QueryContext(ctx, query, args)
		if err != nil {
			return nil, err
		}

		return ocRows{parent: rows, ctx: ctx, options: c.options}, nil
	}

	return nil, driver.ErrSkip
}

func (c ocConn) Prepare(query string) (stmt driver.Stmt, err error) {
	_, span := trace.StartSpan(context.Background(), "sql:prepare")
	attrs := []trace.Attribute{attrMissingContext}
	if c.options.Query {
		attrs = append(attrs, trace.StringAttribute("sql.query", query))
	}
	span.AddAttributes(attrs...)

	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	stmt, err = c.parent.Prepare(query)
	if err != nil {
		return nil, err
	}

	stmt = wrapStmt(stmt, query, c.options)
	return
}

func (c *ocConn) Close() error {
	return c.parent.Close()
}

func (c *ocConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.TODO(), driver.TxOptions{})
}

func (c *ocConn) PrepareContext(ctx context.Context, query string) (stmt driver.Stmt, err error) {
	var span *trace.Span
	ctx, span = trace.StartSpan(ctx, "sql:prepare")
	if c.options.Query {
		span.AddAttributes(trace.StringAttribute("sql.query", query))
	}
	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	if prepCtx, ok := c.parent.(driver.ConnPrepareContext); ok {
		stmt, err = prepCtx.PrepareContext(ctx, query)
	} else {
		span.AddAttributes(attrMissingContext)
		stmt, err = c.parent.Prepare(query)
	}
	if err != nil {
		return nil, err
	}

	stmt = ocStmt{parent: stmt, query: query, options: c.options}
	return
}

func (c *ocConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.options.Transaction {
		if ctx == nil || ctx == context.TODO() {
			var appSpan *trace.Span
			ctx, appSpan = trace.StartSpan(context.Background(), "sql:transaction")
			appSpan.AddAttributes(attrMissingContext)
		} else {
			ctx, _ = trace.StartSpan(ctx, "sql:transaction")
		}
	}

	var span *trace.Span
	if ctx == nil || ctx == context.TODO() {
		ctx = context.Background()
		_, span = trace.StartSpan(ctx, "sql:begin_transaction")
		span.AddAttributes(attrMissingContext)
	} else {
		_, span = trace.StartSpan(ctx, "sql:begin_transaction")
	}
	defer span.End()

	if connBeginTx, ok := c.parent.(driver.ConnBeginTx); ok {
		tx, err := connBeginTx.BeginTx(ctx, opts)
		setSpanStatus(span, err)
		if err != nil {
			return nil, err
		}
		return ocTx{parent: tx, ctx: ctx}, nil
	}

	span.AddAttributes(attrDeprecated)
	span.AddAttributes(trace.StringAttribute(
		"ocsql.deprecated", "driver does not support ConnBeginTx",
	))
	tx, err := c.parent.Begin()
	setSpanStatus(span, err)
	if err != nil {
		return nil, err
	}
	return ocTx{parent: tx, ctx: ctx}, nil
}

func (t ocTx) Commit() (err error) {
	if t.options.Transaction {
		defer func() {
			if span := trace.FromContext(t.ctx); span != nil {
				span.SetStatus(trace.Status{Code: trace.StatusCodeOK})
				setSpanStatus(span, err)
				span.End()
			}
		}()
	}

	_, span := trace.StartSpan(t.ctx, "sql:commit")
	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	err = t.parent.Commit()
	return
}

func (t ocTx) Rollback() (err error) {
	if t.options.Transaction {
		defer func() {
			if span := trace.FromContext(t.ctx); span != nil {
				if err == nil {
					span.SetStatus(trace.Status{
						Code:    trace.StatusCodeAborted,
						Message: "transaction rollback",
					})
				} else {
					setSpanStatus(span, err)
				}
				span.End()
			}
		}()
	}

	_, span := trace.StartSpan(t.ctx, "sql:rollback")
	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	err = t.parent.Rollback()
	return
}

func (r ocResult) LastInsertId() (id int64, err error) {
	if r.options.LastInsertID {
		_, span := trace.StartSpan(r.ctx, "sql:last_insert_id")
		defer func() {
			setSpanStatus(span, err)
			span.End()
		}()
	}

	id, err = r.parent.LastInsertId()
	return
}

func (r ocResult) RowsAffected() (cnt int64, err error) {
	if r.options.RowsAffected {
		_, span := trace.StartSpan(r.ctx, "sql:rows_affected")
		defer func() {
			setSpanStatus(span, err)
			span.End()
		}()
	}

	cnt, err = r.parent.RowsAffected()
	return
}

func (s ocStmt) Exec(args []driver.Value) (res driver.Result, err error) {
	_, span := trace.StartSpan(context.Background(), "sql:exec")
	attrs := []trace.Attribute{
		attrDeprecated,
		trace.StringAttribute(
			"ocsql.deprecated", "driver does not support StmtExecContext",
		),
	}
	if s.options.Query {
		attrs = append(attrs, trace.StringAttribute("sql.query", s.query))
		if s.options.QueryParams {
			attrs = append(attrs, paramsAttr(args)...)
		}
	}
	span.AddAttributes(attrs...)

	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	res, err = s.parent.Exec(args)
	return
}

func (s ocStmt) Close() error {
	return s.parent.Close()
}

func (s ocStmt) NumInput() int {
	return s.parent.NumInput()
}

func (s ocStmt) Query(args []driver.Value) (rows driver.Rows, err error) {
	_, span := trace.StartSpan(context.Background(), "sql:query")
	attrs := []trace.Attribute{
		attrDeprecated,
		trace.StringAttribute(
			"ocsql.deprecated", "driver does not support StmtQueryContext",
		),
	}
	if s.options.Query {
		attrs = append(attrs, trace.StringAttribute("sql.query", s.query))
		if s.options.QueryParams {
			attrs = append(attrs, paramsAttr(args)...)
		}
	}
	span.AddAttributes(attrs...)

	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	rows, err = s.parent.Query(args)
	return
}

func (s ocStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (res driver.Result, err error) {
	var span *trace.Span
	ctx, span = trace.StartSpan(ctx, "sql:exec")
	if s.options.Query {
		attrs := []trace.Attribute{trace.StringAttribute("sql.query", s.query)}
		if s.options.QueryParams {
			attrs = append(attrs, namedParamsAttr(args)...)
		}
		span.AddAttributes(attrs...)
	}

	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	// we already tested driver to implement StmtExecContext
	execContext := s.parent.(driver.StmtExecContext)
	res, err = execContext.ExecContext(ctx, args)
	return
}

func (s ocStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (rows driver.Rows, err error) {
	var span *trace.Span
	ctx, span = trace.StartSpan(ctx, "sql:query")
	if s.options.Query {
		attrs := []trace.Attribute{trace.StringAttribute("sql.query", s.query)}
		if s.options.QueryParams {
			attrs = append(attrs, namedParamsAttr(args)...)
		}
		span.AddAttributes(attrs...)
	}

	defer func() {
		setSpanStatus(span, err)
		span.End()
	}()

	// we already tested driver to implement StmtQueryContext
	queryContext := s.parent.(driver.StmtQueryContext)
	rows, err = queryContext.QueryContext(ctx, args)
	return
}

func (r ocRows) Columns() []string {
	return r.parent.Columns()
}

func (r ocRows) Close() (err error) {
	if r.options.RowsClose {
		_, span := trace.StartSpan(r.ctx, "sql:rows_close")
		defer func() {
			setSpanStatus(span, err)
			span.End()
		}()
	}

	err = r.parent.Close()
	return
}

func (r ocRows) Next(dest []driver.Value) (err error) {
	if r.options.RowsNext {
		_, span := trace.StartSpan(r.ctx, "sql:rows_next")
		defer func() {
			if err == io.EOF {
				// not an error; expected to happen during iteration
				setSpanStatus(span, nil)
			} else {
				setSpanStatus(span, err)
			}
			span.End()
		}()
	}

	err = r.parent.Next(dest)
	return
}

func wrapStmt(stmt driver.Stmt, query string, options TraceOptions) driver.Stmt {
	s := ocStmt{parent: stmt, query: query, options: options}
	_, hasExeCtx := stmt.(driver.StmtExecContext)
	_, hasQryCtx := stmt.(driver.StmtQueryContext)

	switch {
	case !hasExeCtx && !hasQryCtx:
		return struct {
			driver.Stmt
		}{s}
	case !hasExeCtx && hasQryCtx:
		return struct {
			driver.Stmt
			driver.StmtQueryContext
		}{s, s}
	case hasExeCtx && !hasQryCtx:
		return struct {
			driver.Stmt
			driver.StmtExecContext
		}{s, s}
	case hasExeCtx && hasQryCtx:
		return struct {
			driver.Stmt
			driver.StmtExecContext
			driver.StmtQueryContext
		}{s, s, s}
	}
	panic("unreachable")
}

func paramsAttr(args []driver.Value) []trace.Attribute {
	attrs := make([]trace.Attribute, 0, len(args))
	for i, arg := range args {
		key := "sql.arg" + strconv.Itoa(i)
		attrs[i] = argToAttr(key, arg)
	}
	return attrs
}

func namedParamsAttr(args []driver.NamedValue) []trace.Attribute {
	attrs := make([]trace.Attribute, 0, len(args))
	for i, arg := range args {
		var key string
		if arg.Name != "" {
			key = arg.Name
		} else {
			key = "sql.arg." + strconv.Itoa(arg.Ordinal)
		}
		attrs[i] = argToAttr(key, arg.Value)
	}
	return attrs
}

func argToAttr(key string, val interface{}) trace.Attribute {
	switch v := val.(type) {
	case nil:
		return trace.StringAttribute(key, "")
	case int64:
		return trace.Int64Attribute(key, v)
	case float64:
		return trace.StringAttribute(key, fmt.Sprintf("%f", v))
	case bool:
		return trace.BoolAttribute(key, v)
	case []byte:
		if len(v) > 256 {
			v = v[0:256]
		}
		return trace.StringAttribute(key, fmt.Sprintf("%s", v))
	default:
		s := fmt.Sprintf("%v", v)
		if len(s) > 256 {
			s = s[0:256]
		}
		return trace.StringAttribute(key, s)
	}
}

func setSpanStatus(span *trace.Span, err error) {
	var status trace.Status
	switch err {
	case nil:
		status.Code = trace.StatusCodeOK
		span.SetStatus(status)
		return
	case context.Canceled:
		status.Code = trace.StatusCodeCancelled
	case context.DeadlineExceeded:
		status.Code = trace.StatusCodeDeadlineExceeded
	case sql.ErrNoRows:
		status.Code = trace.StatusCodeNotFound
	case sql.ErrTxDone, sql.ErrConnDone:
		status.Code = trace.StatusCodeFailedPrecondition
	default:
		status.Code = trace.StatusCodeUnknown
	}
	status.Message = err.Error()
	span.SetStatus(status)
}