package gorose

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

type MapRow map[string]interface{}
type MapRows []MapRow

type objectType int

const (
	OBJECT_STRUCT       objectType = iota // 结构体 一条数据	(struct)
	OBJECT_STRUCT_SLICE                   // 结构体 多条数据	([]struct)
	OBJECT_MAP                            // map 一条数据		(map[string]interface{})
	OBJECT_MAP_SLICE                      // map 多条数据		([]map[string]interface{})
	OBJECT_STRING                         // 非结构体 表名字符串	("users")
)

type bind struct {
	// object是指传入的对象 [slice]map,[slice]struct
	// 传入的原始对象
	objectOrigin          interface{}
	objectOriginTableName []string
	// 解析出来的对象名字, 或者指定的method(TableName)获取到的名字
	objectName string
	// 一条结果的反射对象
	objectResult reflect.Value
	// 多条
	objectResultSlice reflect.Value
	// 传入结构体解析出来的字段
	objectFields []string
	// 传入的对象类型判定
	objectType objectType
	// 出入传入得是非slice对象, 则只需要取一条, 取多了也是浪费
	objectLimit int
}

type Session struct {
	IEngin
	*bind
	slaveDB      *sql.DB
	masterDB     *sql.DB
	tx           *sql.Tx
	lastInsertId int64
	sqlLogs      []string
	LastSql      string
	IOrm
}

var _ ISession = &Session{}

// NewSession : 初始化 Session
func NewSession(e IEngin) ISession {
	var s = new(Session)
	s.IEngin = e

	s.masterDB = s.GetExecuteDB()
	s.slaveDB = s.GetQueryDB()

	s.bind = new(bind)

	s.IOrm = NewOrm(s.bind)

	return s
}

// Close : 关闭 Session
func (s *Session) Close() {
	s.masterDB.Close()
	s.slaveDB.Close()
}

// Table : 传入绑定结果的对象, 参数一为对象, 可以是 struct, gorose.MapRow 或对应的切片
//		如果是做非query操作,第一个参数也可以仅仅指定为字符串表名
func (s *Session) Table(bind interface{}) ISession {
	s.objectOrigin = bind

	return s
}

func (s *Session) Begin() (err error) {
	s.tx, err = s.masterDB.Begin()
	return
}

func (s *Session) Rollback() (err error) {
	err = s.tx.Rollback()
	s.tx = nil
	return
}

func (s *Session) Commit() (err error) {
	err = s.tx.Commit()
	s.tx = nil
	return
}

func (s *Session) Transaction(closers ...func(ses ISession) error) (err error) {
	err = s.Begin()
	if err != nil {
		return err
	}

	for _, closer := range closers {
		err = closer(s)
		if err != nil {
			_ = s.Rollback()
			return
		}
	}
	return s.Commit()
}

func (s *Session) Query(sqlstring string, args ...interface{}) error {
	err := s.parseTable()

	if err != nil {
		return err
	}

	s.LastSql = fmt.Sprintf(sqlstring, args...)
	// 记录sqlLog
	if s.IfEnableQueryLog() {
		s.sqlLogs = append(s.sqlLogs, s.LastSql)
	}

	if err != nil {
		return err
	}

	stmt, err := s.slaveDB.Prepare(sqlstring)
	if err != nil {
		return err
	}

	defer stmt.Close()
	rows, err := stmt.Query(args...)
	if err != nil {
		return err
	}

	// make sure we always close rows
	defer rows.Close()

	return s.scan(rows)
}

func (s *Session) Execute(sqlstring string, args ...interface{}) (int64, error) {
	err := s.parseTable()

	if err != nil {
		return 0, err
	}
	//t_start := time.Now()

	s.LastSql = fmt.Sprintf(sqlstring, args...)
	// 记录sqlLog
	if s.IfEnableQueryLog() {
		s.sqlLogs = append(s.sqlLogs, s.LastSql)
	}

	var operType = strings.ToLower(sqlstring[0:6])
	if operType == "select" {
		return 0, errors.New("Execute does not allow select operations, please use Query")
	}

	var stmt *sql.Stmt
	if s.tx == nil {
		stmt, err = s.masterDB.Prepare(sqlstring)
	} else {
		stmt, err = s.tx.Prepare(sqlstring)
	}

	if err != nil {
		return 0, err
	}
	//return dba.parseExecute(stmt, operType, vals)

	var rowsAffected int64
	//var err error
	defer stmt.Close()
	result, errs := stmt.Exec(args...)
	if errs != nil {
		return 0, errs
	}

	if operType == "insert" {
		// get last insert id
		lastInsertId, err := result.LastInsertId()
		if err == nil {
			s.lastInsertId = lastInsertId
		}
	}
	// get rows affected
	rowsAffected, err = result.RowsAffected()

	//// 如果是事务, 则重置所有参数
	//if dba.Strans == true {
	//	dba.Reset("transaction")
	//}

	//// 持久化日志
	//if dba.Connection.Logger != nil {
	//	dba.Connection.Logger.Write(dba.LastSql, time.Since(t_start).String(), time.Now().Format("2006-01-02 15:04:05"))
	//}

	return rowsAffected, err
}

func (s *Session) scan(rows *sql.Rows) (err error) {
	//fmt.Printf("%#v\n",s.table)
	// 检查实多维数组还是一维数组
	switch s.objectType {
	case OBJECT_STRUCT:
		err = s.scanRow(rows, s.objectOrigin)
	case OBJECT_STRUCT_SLICE:
		err = s.scanAll(rows, s.objectResultSlice)
	case OBJECT_MAP:
		err = s.scanMap(rows, s.objectResult)
	case OBJECT_MAP_SLICE:
		err = s.scanMapAll(rows, s.objectResultSlice)
	default:
		err = errors.New("bind value error")
	}
	return
}

func (s *Session) scanMap(rows *sql.Rows, dst interface{}) (err error) {
	return s.scanMapAll(rows, dst)
}

func (s *Session) scanMapAll(rows *sql.Rows, dst interface{}) (err error) {
	//var result = make([]map[string]interface{}, 0)
	var columns []string
	if columns, err = rows.Columns(); err != nil {
		return
	}

	count := len(columns)
	values := make([]interface{}, count)
	scanArgs := make([]interface{}, count)

	for rows.Next() {
		for i := 0; i < count; i++ {
			scanArgs[i] = &values[i]
		}
		rows.Scan(scanArgs...)
		//entry := make(map[string]interface{})
		for i, col := range columns {
			var v interface{}
			val := values[i]
			if b, ok := val.([]byte); ok {
				v = string(b)
			} else {
				v = val
			}
			//entry[col] = v
			s.objectResult.SetMapIndex(reflect.ValueOf(col), reflect.ValueOf(v))
		}
		//result = append(result, entry)
		if s.objectType == OBJECT_MAP_SLICE {
			s.objectResultSlice.Set(reflect.Append(s.objectResultSlice, s.objectResult))
		}
	}
	return
}

// scan a single row of data into a struct.
func (s *Session) scanRow(rows *sql.Rows, dst interface{}) error {
	// check if there is data waiting
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}

	// get a list of targets
	var fields = strutForScan(dst)

	// perform the scan
	if err := rows.Scan(fields...); err != nil {
		//if err := rows.Scan(strutForScan(s.objectResult.Interface())...); err != nil {
		return err
	}

	return rows.Err()
}

// ScanAll scans all sql result rows into a slice of structs.
// It reads all rows and closes rows when finished.
// dst should be a pointer to a slice of the appropriate type.
// The new results will be appended to any existing data in dst.
func (s *Session) scanAll(rows *sql.Rows, dst interface{}) error {
	for rows.Next() {
		// scan it
		err := rows.Scan(strutForScan(s.objectResult.Interface())...)
		if err != nil {
			return err
		}
		// add to the result slice
		s.objectResultSlice.Set(reflect.Append(s.objectResultSlice, s.objectResult.Elem()))
	}

	return rows.Err()
}

func (s *Session) parseTable() (err error) {
	if s.objectOrigin == nil {
		return nil
	}
	var objectName string
	switch s.objectOrigin.(type) {
	case string: // 直接传入的是表名
		s.objectType = OBJECT_STRING
		objectName = s.objectOrigin.(string)

	// 传入的是struct
	default:
		// 清空字段值,避免手动传入字段污染struct字段
		s.objectFields = []string{}
		// make sure dst is an appropriate type
		dstVal := reflect.ValueOf(s.objectOrigin)

		sliceVal := reflect.Indirect(dstVal)

		switch sliceVal.Kind() {
		case reflect.Struct: // struct
			s.objectType = OBJECT_STRUCT
			objectName = sliceVal.Type().Name()
			s.objectResult = sliceVal
			// 默认只查一条
			s.objectLimit = 1
			// 是否设置了表名
			if tn := dstVal.MethodByName("objectName"); tn.IsValid() {
				objectName = tn.Call(nil)[0].String()
			}
			// 解析出字段
			s.parseFields()
		case reflect.Map: // map
			//fmt.Println("map")
			s.objectType = OBJECT_MAP
			// 默认只查一条
			s.objectLimit = 1
			//
			s.objectResult = sliceVal

		case reflect.Slice: // []struct
			eltType := sliceVal.Type().Elem()

			switch eltType.Kind() {
			case reflect.Map:
				s.objectType = OBJECT_MAP_SLICE
				//objectName = eltType.Name()
				s.objectResult = reflect.MakeMap(eltType)
				s.objectResultSlice = sliceVal

			case reflect.Struct:
				s.objectType = OBJECT_STRUCT_SLICE
				objectName = eltType.Name()
				s.objectResult = reflect.New(eltType)
				s.objectResultSlice = sliceVal
				// 是否设置了表名
				if tn := s.objectResult.MethodByName("objectName"); tn.IsValid() {
					objectName = tn.Call(nil)[0].String()
				}
				// 解析出字段
				s.parseFields()
			default:
				return fmt.Errorf("table只接收 struct,[]struct,map[string]interface{},[]map[string]interface{}, 但是传入的是: %T", s.objectOrigin)
			}
		default:
			return fmt.Errorf("table只接收 struct,[]struct,map[string]interface{},[]map[string]interface{}, 但是传入的是: %T", s.objectOrigin)
		}
	}

	s.objectName = objectName

	return
}

func (s *Session) parseFields() {
	if len(s.objectFields) == 0 {
		s.objectFields = getTagName(s.objectResult.Interface())
	}
}
