package orm

import (
	"context"
	"github.com/banbox/banbot/config"
	"github.com/banbox/banbot/core"
	"github.com/banbox/banexg/errs"
	"github.com/banbox/banexg/log"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	pool         *pgxpool.Pool
	AccTaskIDs   = make(map[string]int64)
	AccTasks     = make(map[string]*BotTask)
	taskIdAccMap = make(map[int64]string)
)

func Setup() *errs.Error {
	if pool != nil {
		pool.Close()
		pool = nil
	}
	dbCfg := config.Database
	if dbCfg == nil {
		return errs.NewMsg(core.ErrBadConfig, "database config is missing!")
	}
	poolCfg, err_ := pgxpool.ParseConfig(dbCfg.Url)
	if err_ != nil {
		return errs.New(core.ErrBadConfig, err_)
	}
	//poolCfg.BeforeAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
	//	log.Info(fmt.Sprintf("get conn: %v", conn))
	//	return true
	//}
	//poolCfg.AfterRelease = func(conn *pgx.Conn) bool {
	//	log.Info(fmt.Sprintf("del conn: %v", conn))
	//	return true
	//}
	//poolCfg.BeforeClose = func(conn *pgx.Conn) {
	//	log.Info(fmt.Sprintf("close conn: %v", conn))
	//}
	dbPool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return errs.New(core.ErrDbConnFail, err)
	}
	pool = dbPool
	row := pool.QueryRow(context.Background(), "show timezone;")
	var tz string
	err = row.Scan(&tz)
	if err != nil {
		return errs.New(core.ErrDbReadFail, err)
	}
	log.Info("connect db ok")
	return nil
}

func Conn(ctx context.Context) (*Queries, *pgxpool.Conn, *errs.Error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, errs.New(core.ErrDbConnFail, err)
	}
	return New(conn), conn, nil
}

type Tx struct {
	tx     pgx.Tx
	closed bool
}

func (t *Tx) Close(ctx context.Context, commit bool) *errs.Error {
	if t.closed {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	if commit {
		err = t.tx.Commit(ctx)
	} else {
		err = t.tx.Rollback(ctx)
	}
	t.closed = true
	if err != nil {
		return errs.New(core.ErrDbExecFail, err)
	}
	return nil
}

func (q *Queries) NewTx(ctx context.Context) (*Tx, *Queries, *errs.Error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, nil, errs.New(core.ErrDbConnFail, err)
	}
	nq := q.WithTx(tx)
	return &Tx{tx: tx}, nq, nil
}

func (q *Queries) Exec(sql string, args ...interface{}) *errs.Error {
	_, err_ := q.db.Exec(context.Background(), sql, args...)
	if err_ != nil {
		return errs.New(core.ErrDbExecFail, err_)
	}
	return nil
}

func GetTaskID(account string) int64 {
	if !core.EnvReal {
		account = config.DefAcc
	}
	if id, ok := AccTaskIDs[account]; ok {
		return id
	}
	return 0
}

func GetTask(account string) *BotTask {
	if !core.EnvReal {
		account = config.DefAcc
	}
	if task, ok := AccTasks[account]; ok {
		return task
	}
	return nil
}

func GetTaskAcc(id int64) string {
	if acc, ok := taskIdAccMap[id]; ok {
		return acc
	}
	return ""
}

func GetOpenODs(account string) map[int64]*InOutOrder {
	if !core.EnvReal {
		account = config.DefAcc
	}
	val, ok := AccOpenODs[account]
	if !ok {
		val = make(map[int64]*InOutOrder)
		AccOpenODs[account] = val
	}
	return val
}

func GetTriggerODs(account string) map[string][]*InOutOrder {
	if !core.EnvReal {
		account = config.DefAcc
	}
	val, ok := AccTriggerODs[account]
	if !ok {
		val = make(map[string][]*InOutOrder)
		AccTriggerODs[account] = val
	}
	return val
}

func AddTriggerOd(account string, od *InOutOrder) {
	triggerOds := GetTriggerODs(account)
	ods, _ := triggerOds[od.Symbol]
	triggerOds[od.Symbol] = append(ods, od)
}

/*
OpenNum
返回符合指定状态的尚未平仓订单的数量
*/
func OpenNum(account string, status int16) int {
	openNum := 0
	openOds := GetOpenODs(account)
	for _, od := range openOds {
		if od.Status >= status {
			openNum += 1
		}
	}
	return openNum
}
