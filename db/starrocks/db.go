// Copyright 2018 PingCAP, Inc.
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

package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/util"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// mysql properties
const (
	mysqlHost     = "mysql.host"
	mysqlPort     = "mysql.port"
	mysqlUser     = "mysql.user"
	mysqlPassword = "mysql.password"
	mysqlDBName   = "mysql.db"
)

type muxDriver struct {
	cursor    uint64
	instances []string
	internal  driver.Driver
}

func (drv *muxDriver) Open(name string) (driver.Conn, error) {
	k := atomic.AddUint64(&drv.cursor, 1)
	return drv.internal.Open(drv.instances[int(k)%len(drv.instances)])
}

type starRocksCreator struct {
	name string
}

type starRockslDB struct {
	p                 *properties.Properties
	db                *sql.DB
	verbose           bool
	forceIndexKeyword string

	bufPool *util.BufPool
}

type contextKey string

const stateKey = contextKey("starRockslDB")

type starRocksState struct {
	// Do we need a LRU cache here?
	stmtCache map[string]*sql.Stmt

	conn *sql.Conn
}

func (c starRocksCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	d := new(starRockslDB)
	d.p = p

	host := p.GetString(mysqlHost, "127.0.0.1")
	port := p.GetInt(mysqlPort, 3306)
	user := p.GetString(mysqlUser, "root")
	password := p.GetString(mysqlPassword, "")
	dbName := p.GetString(mysqlDBName, "test")

	var (
		db  *sql.DB
		err error
	)

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", user, password, host, port, dbName)
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	//threadCount := int(p.GetInt64(prop.ThreadCount, prop.ThreadCountDefault))
	db.SetMaxIdleConns(100)
	db.SetMaxOpenConns(100)

	d.verbose = p.GetBool(prop.Verbose, prop.VerboseDefault)
	d.db = db

	d.bufPool = util.NewBufPool()

	if err := d.createTable(c.name); err != nil {
		return nil, err
	}

	return d, nil
}

func (db *starRockslDB) createTable(driverName string) error {
	tableName := db.p.GetString(prop.TableName, prop.TableNameDefault)
	if db.p.GetBool(prop.DropData, prop.DropDataDefault) &&
		!db.p.GetBool(prop.DoTransactions, true) {
		if _, err := db.db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName)); err != nil {
			return err
		}
	}

	fieldCount := db.p.GetInt64(prop.FieldCount, prop.FieldCountDefault)
	fieldLength := db.p.GetInt64(prop.FieldLength, prop.FieldLengthDefault)

	buf := new(bytes.Buffer)
	s := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (YCSB_KEY VARCHAR(64)", tableName)
	buf.WriteString(s)

	for i := int64(0); i < fieldCount; i++ {
		buf.WriteString(fmt.Sprintf(", FIELD%d VARCHAR(%d)", i, fieldLength))
	}

	buf.WriteString(") ENGINE=ROW_STORE PRIMARY KEY (YCSB_KEY);")

	_, err := db.db.Exec(buf.String())
	return err
}

func (db *starRockslDB) Close() error {
	if db.db == nil {
		return nil
	}

	return db.db.Close()
}

func (db *starRockslDB) InitThread(ctx context.Context, _ int, _ int) context.Context {
	conn, err := db.db.Conn(ctx)
	if err != nil {
		panic(fmt.Sprintf("failed to create db conn %v", err))
	}

	state := &starRocksState{
		stmtCache: make(map[string]*sql.Stmt),
		conn:      conn,
	}

	return context.WithValue(ctx, stateKey, state)
}

func (db *starRockslDB) CleanupThread(ctx context.Context) {
	state := ctx.Value(stateKey).(*starRocksState)

	for _, stmt := range state.stmtCache {
		stmt.Close()
	}
	state.conn.Close()
}

func (db *starRockslDB) getAndCacheStmt(ctx context.Context, query string) (*sql.Stmt, error) {
	state := ctx.Value(stateKey).(*starRocksState)

	if stmt, ok := state.stmtCache[query]; ok {
		return stmt, nil
	}

	stmt, err := state.conn.PrepareContext(ctx, query)
	if err == sql.ErrConnDone {
		// Try build the connection and prepare again
		if state.conn, err = db.db.Conn(ctx); err == nil {
			stmt, err = state.conn.PrepareContext(ctx, query)
		}
	}

	if err != nil {
		return nil, err
	}

	state.stmtCache[query] = stmt
	return stmt, nil
}

func (db *starRockslDB) clearCacheIfFailed(ctx context.Context, query string, err error) {
	if err == nil {
		return
	}

	state := ctx.Value(stateKey).(*starRocksState)
	if stmt, ok := state.stmtCache[query]; ok {
		stmt.Close()
	}
	delete(state.stmtCache, query)
}

func (db *starRockslDB) queryRows(ctx context.Context, query string, count int, args ...interface{}) ([]map[string][]byte, error) {
	if db.verbose {
		fmt.Printf("%s %v\n", query, args)
	}

	stmt, err := db.getAndCacheStmt(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := stmt.QueryContext(ctx, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	vs := make([]map[string][]byte, 0, count)
	for rows.Next() {
		m := make(map[string][]byte, len(cols))
		dest := make([]interface{}, len(cols))
		for i := 0; i < len(cols); i++ {
			v := new([]byte)
			dest[i] = v
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}

		for i, v := range dest {
			m[cols[i]] = *v.(*[]byte)
		}

		vs = append(vs, m)
	}

	return vs, rows.Err()
}

func (db *starRockslDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	var query string
	if len(fields) == 0 {
		query = fmt.Sprintf(`SELECT * FROM %s %s WHERE YCSB_KEY = ?`, table, db.forceIndexKeyword)
	} else {
		query = fmt.Sprintf(`SELECT %s FROM %s %s WHERE YCSB_KEY = ?`, strings.Join(fields, ","), table, db.forceIndexKeyword)
	}

	rows, err := db.queryRows(ctx, query, 1, key)
	db.clearCacheIfFailed(ctx, query, err)

	if err != nil {
		return nil, err
	} else if len(rows) == 0 {
		return nil, nil
	}

	return rows[0], nil
}

func (db *starRockslDB) BatchRead(ctx context.Context, table string, keys []string, fields []string) ([]map[string][]byte, error) {
	args := make([]interface{}, 0, len(keys))
	buf := db.bufPool.Get()
	defer db.bufPool.Put(buf)
	if len(fields) == 0 {
		buf = append(buf, fmt.Sprintf(`SELECT * FROM %s %s WHERE YCSB_KEY IN (`, table, db.forceIndexKeyword)...)
	} else {
		buf = append(buf, fmt.Sprintf(`SELECT %s FROM %s %s WHERE YCSB_KEY IN (`, strings.Join(fields, ","), table, db.forceIndexKeyword)...)
	}
	for i, key := range keys {
		buf = append(buf, '?')
		if i < len(keys)-1 {
			buf = append(buf, ',')
		}
		args = append(args, key)
	}
	buf = append(buf, ')')

	query := string(buf[:])
	rows, err := db.queryRows(ctx, query, len(keys), args...)
	db.clearCacheIfFailed(ctx, query, err)

	if err != nil {
		return nil, err
	} else if len(rows) == 0 {
		return nil, nil
	}

	return rows, nil
}

func (db *starRockslDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	var query string
	if len(fields) == 0 {
		query = fmt.Sprintf(`SELECT * FROM %s %s WHERE YCSB_KEY >= ? LIMIT %d`, table, db.forceIndexKeyword, count)
	} else {
		query = fmt.Sprintf(`SELECT %s FROM %s %s WHERE YCSB_KEY >= ? LIMIT %d`, strings.Join(fields, ","), table, db.forceIndexKeyword, count)
	}

	rows, err := db.queryRows(ctx, query, count, startKey)
	db.clearCacheIfFailed(ctx, query, err)

	return rows, err
}

func (db *starRockslDB) execQuery(ctx context.Context, query string, args ...interface{}) error {
	if db.verbose {
		fmt.Printf("%s %v\n", query, args)
	}

	stmt, err := db.getAndCacheStmt(ctx, query)
	if err != nil {
		return err
	}

	_, err = stmt.ExecContext(ctx, args...)
	db.clearCacheIfFailed(ctx, query, err)
	return err
}

func (db *starRockslDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	buf := bytes.NewBuffer(db.bufPool.Get())
	defer func() {
		db.bufPool.Put(buf.Bytes())
	}()

	buf.WriteString("UPDATE ")
	buf.WriteString(table)
	buf.WriteString(" SET ")
	firstField := true
	pairs := util.NewFieldPairs(values)
	args := make([]interface{}, 0, len(values)+1)
	for _, p := range pairs {
		if firstField {
			firstField = false
		} else {
			buf.WriteString(", ")
		}

		buf.WriteString(p.Field)
		buf.WriteString(`= ?`)
		args = append(args, p.Value)
	}
	buf.WriteString(" WHERE YCSB_KEY = ?")

	args = append(args, key)

	return db.execQuery(ctx, buf.String(), args...)
}

func (db *starRockslDB) BatchUpdate(ctx context.Context, table string, keys []string, values []map[string][]byte) error {
	// mysql does not support BatchUpdate, fallback to Update like dbwrapper.go
	for i := range keys {
		err := db.Update(ctx, table, keys[i], values[i])
		if err != nil {
			return err
		}
	}
	return nil
}

func (db *starRockslDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	args := make([]interface{}, 0, 1+len(values))
	args = append(args, key)

	buf := bytes.NewBuffer(db.bufPool.Get())
	defer func() {
		db.bufPool.Put(buf.Bytes())
	}()

	buf.WriteString("INSERT INTO ")
	buf.WriteString(table)
	buf.WriteString(" (YCSB_KEY")

	pairs := util.NewFieldPairs(values)
	for _, p := range pairs {
		args = append(args, p.Value)
		buf.WriteString(" ,")
		buf.WriteString(p.Field)
	}
	buf.WriteString(") VALUES (?")

	for i := 0; i < len(pairs); i++ {
		buf.WriteString(" ,?")
	}

	buf.WriteByte(')')

	return db.execQuery(ctx, buf.String(), args...)
}

func (db *starRockslDB) BatchInsert(ctx context.Context, table string, keys []string, values []map[string][]byte) error {
	args := make([]interface{}, 0, (1+len(values))*len(keys))
	buf := db.bufPool.Get()
	defer db.bufPool.Put(buf)
	buf = append(buf, "INSERT INTO "...)
	buf = append(buf, table...)
	buf = append(buf, " (YCSB_KEY"...)

	valueString := strings.Builder{}
	valueString.WriteString("(?")
	pairs := util.NewFieldPairs(values[0])
	for _, p := range pairs {
		buf = append(buf, " ,"...)
		buf = append(buf, p.Field...)

		valueString.WriteString(" ,?")
	}
	// Example: INSERT INTO table ([columns]) VALUES
	buf = append(buf, ") VALUES "...)
	// Example: (?, ?, ?, ....)
	valueString.WriteByte(')')
	valueStrings := make([]string, 0, len(keys))
	for range keys {
		valueStrings = append(valueStrings, valueString.String())
	}
	// Example: INSERT INTO table ([columns]) VALUES (?, ?, ?...), (?, ?, ?), ...
	buf = append(buf, strings.Join(valueStrings, ",")...)

	for i, key := range keys {
		args = append(args, key)
		pairs := util.NewFieldPairs(values[i])
		for _, p := range pairs {
			args = append(args, p.Value)
		}
	}

	return db.execQuery(ctx, string(buf[:]), args...)
}

func (db *starRockslDB) Delete(ctx context.Context, table string, key string) error {
	query := fmt.Sprintf(`DELETE FROM %s WHERE YCSB_KEY = ?`, table)

	return db.execQuery(ctx, query, key)
}

func (db *starRockslDB) BatchDelete(ctx context.Context, table string, keys []string) error {
	args := make([]interface{}, 0, len(keys))
	buf := db.bufPool.Get()
	defer db.bufPool.Put(buf)
	buf = append(buf, fmt.Sprintf("DELETE FROM %s WHERE YCSB_KEY IN (", table)...)
	for i, key := range keys {
		buf = append(buf, '?')
		if i < len(keys)-1 {
			buf = append(buf, ',')
		}
		args = append(args, key)
	}
	buf = append(buf, ')')

	return db.execQuery(ctx, string(buf[:]), args...)
}

func (db *starRockslDB) Analyze(ctx context.Context, table string) error {
	_, err := db.db.Exec(fmt.Sprintf(`ANALYZE TABLE %s`, table))
	return err
}

func init() {
	fmt.Println("StarRocks")
	ycsb.RegisterDBCreator("starrocks", starRocksCreator{name: "starrocks"})
}
