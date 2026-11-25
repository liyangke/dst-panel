package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNoTask 表示队列里没有待执行任务，ClaimTask 用它而不是 nil error。
var ErrNoTask = errors.New("没有待执行任务")

// Task 是一条异步任务。长活一律走它：SteamCMD 下载模组在 45KB/s 的上行下要跑十几分钟，
// HTTP 请求不能挂在那里等，必须"提交即返回 id，前端轮询进度"。
type Task struct {
	ID       int64
	Kind     string
	Title    string
	Payload  string // JSON 文本，由调用方决定结构
	Status   string
	Progress int
	Message  string
	// Detail 是进度那组数的 JSON 原文（见 TaskDetail）。没有字节进度的任务是空串。
	Detail   string
	Attempts int

	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// TaskDetail 是进度条背后那几个可以对质的数：下了多少、总共多少、此刻多快、还要多久。
// 每一个都从外部命令的实时输出里读出来，面板不假设任何一台机器的带宽或整包大小。
// 界面把它们排成数字块，而不是从 message 那句中文里再解析一次。
type TaskDetail struct {
	DoneBytes   int64   `json:"doneBytes"`
	TotalBytes  int64   `json:"totalBytes"`
	BytesPerSec float64 `json:"bytesPerSec"`
	EtaSeconds  float64 `json:"etaSeconds"`
	// Phase 是当前阶段的中文名字，和 message 分开：message 是一整句，Phase 只标阶段。
	Phase string `json:"phase"`
	// Downloading 区分这组字节是"从网络下来的"还是"在校验磁盘上已有的"：
	// 两种情况用的是同一个整包字节量，界面要是都写成"已下"，
	// 一次 validate 就会报出 200 MB/s 的"下载速度"。
	Downloading bool `json:"downloading"`
}

// PayloadInto 把 payload 解回调用方的结构体。
func (t Task) PayloadInto(out any) error {
	if t.Payload == "" {
		return nil
	}
	return json.Unmarshal([]byte(t.Payload), out)
}

func (s *Store) CreateTask(kind, title string, payload any) (int64, error) {
	body := "{}"
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("序列化任务参数: %w", err)
		}
		body = string(raw)
	}
	res, err := s.db.Exec(
		`INSERT INTO tasks (kind, title, payload, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		kind, title, body, TaskQueued, nowMilli())
	if err != nil {
		return 0, fmt.Errorf("创建任务 %s: %w", kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("创建任务 %s 后取 id: %w", kind, err)
	}
	return id, nil
}

// ClaimTask 原子地领取最早的一条排队任务。
//
// SELECT + UPDATE 拆成两句也安全：连接池只有 1 条连接（见 store.Open），
// 面板内部不存在两个 worker 抢同一行的情况；_txlock=immediate 只是防外部进程。
// 领取时把 attempts 加一并清空上一轮的 progress/message，重试看起来就是干净的新任务。
func (s *Store) ClaimTask() (Task, error) {
	var out Task
	err := s.inTx(func(tx *sql.Tx) error {
		var (
			id        int64
			kind      string
			title     string
			payload   string
			created   int64
			startedAt sql.NullInt64
			finished  sql.NullInt64
		)
		row := tx.QueryRow(
			`SELECT id, kind, title, payload, created_at FROM tasks
			   WHERE status = ? ORDER BY id LIMIT 1`, TaskQueued)
		switch err := row.Scan(&id, &kind, &title, &payload, &created); err {
		case sql.ErrNoRows:
			return ErrNoTask
		case nil:
		default:
			return fmt.Errorf("取待执行任务: %w", err)
		}
		now := nowMilli()
		res, err := tx.Exec(
			`UPDATE tasks SET status = ?, started_at = ?, progress = 0, message = '', detail = '',
			        attempts = attempts + 1
			   WHERE id = ? AND status = ?`,
			TaskRunning, now, id, TaskQueued)
		if err != nil {
			return fmt.Errorf("领取任务 %d: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("领取任务 %d 后核对行数: %w", id, err)
		} else if n == 0 {
			return ErrNoTask
		}
		// 领取成功后再回读 attempts，避免在 Go 侧重复实现加一逻辑。
		if err := tx.QueryRow(
			`SELECT attempts, started_at, finished_at FROM tasks WHERE id = ?`, id).
			Scan(&out.Attempts, &startedAt, &finished); err != nil {
			return fmt.Errorf("回读任务 %d: %w", id, err)
		}
		out.ID, out.Kind, out.Title, out.Payload, out.Status = id, kind, title, payload, TaskRunning
		out.CreatedAt = time.UnixMilli(created).UTC()
		if startedAt.Valid {
			t := time.UnixMilli(startedAt.Int64).UTC()
			out.StartedAt = &t
		}
		if finished.Valid {
			t := time.UnixMilli(finished.Int64).UTC()
			out.FinishedAt = &t
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// SetTaskProgress 写百分比、界面那句话，以及进度条背后那组数。
// detail 传 nil 表示这条任务没有"字节数"可言（备份、恢复这类按条目数走的），
// 界面就只那句话——空着比编一个 0 好。
func (s *Store) SetTaskProgress(id int64, progress int, message string, detail *TaskDetail) error {
	if progress < 0 || progress > 100 {
		return fmt.Errorf("任务 %d 的进度要落在 0~100，实际是 %d", id, progress)
	}
	body := ""
	if detail != nil {
		raw, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("序列化任务 %d 的进度明细: %w", id, err)
		}
		body = string(raw)
	}
	_, err := s.db.Exec(`UPDATE tasks SET progress = ?, message = ?, detail = ? WHERE id = ?`, progress, message, body, id)
	if err != nil {
		return fmt.Errorf("更新任务 %d 进度: %w", id, err)
	}
	return nil
}

// FinishTask 收尾。status 只接受终态，避免把任务写成"还在跑"的假象。
func (s *Store) FinishTask(id int64, status, message string) error {
	if !IsTaskTerminal(status) {
		return fmt.Errorf("任务 %d 不能收尾成 %s（%s），只能是已完成/失败/已取消",
			id, status, LabelTaskStatus(status))
	}
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, message = ?, finished_at = ? WHERE id = ? AND status = ?`,
		status, message, nowMilli(), id, TaskRunning)
	if err != nil {
		return fmt.Errorf("结束任务 %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("任务 %d 不在执行中，没法收尾（可能已经被取消）", id)
	}
	return nil
}

// CancelTask 只能取消还在排队的任务。
// 已经在跑的任务由 worker 的 context 来中断，面板这边不做"假装取消了"的事：
// SteamCMD 被半路杀掉会留下半个模组目录，那种脏状态比多等几分钟更贵。
func (s *Store) CancelTask(id int64, reason string) error {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, message = ?, finished_at = ? WHERE id = ? AND status = ?`,
		TaskCanceled, reason, nowMilli(), id, TaskQueued)
	if err != nil {
		return fmt.Errorf("取消任务 %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, gerr := s.GetTask(id)
		if gerr != nil {
			return gerr
		}
		return fmt.Errorf("任务 %d 现在是%s，取消不了", id, LabelTaskStatus(cur.Status))
	}
	return nil
}

func (s *Store) GetTask(id int64) (Task, error) {
	t, err := scanTask(s.db.QueryRow(taskSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("没有 id=%d 的任务", id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("读取任务 %d: %w", id, err)
	}
	return t, nil
}

// ListTasks 按 id 倒序（就是按创建时间倒序）。limit<=0 用默认值，防止前端传 0 把全表拖出来。
func (s *Store) ListTasks(limit int, statuses ...string) ([]Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query := taskSelect
	args := make([]any, 0, len(statuses)+1)
	if len(statuses) > 0 {
		query += " WHERE status IN ("
		for i, st := range statuses {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, st)
		}
		query += ")"
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("列举任务: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("列举任务时解码一行: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountTasksByStatus 给总览页用："3 个在跑、2 个排队、昨天失败 1 个"。
func (s *Store) CountTasksByStatus() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("统计任务: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("统计任务时解码一行: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

// RecoverInterruptedTasks 在面板启动时把上一轮留下的 running 变成 failed。
//
// 为什么必须做：running 是 worker 持有的状态，而 worker 就是本进程；
// 进程没了它就再也没人去 FinishTask，那条任务会永远显示"执行中"，
// 用户只能靠猜来判断是不是还要重来一遍。
func (s *Store) RecoverInterruptedTasks() (int, error) {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, message = ?, finished_at = ? WHERE status = ?`,
		TaskFailed, "面板重启，任务被中断；确认没有残留文件后可以直接重提", nowMilli(), TaskRunning)
	if err != nil {
		return 0, fmt.Errorf("回收中断任务: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("回收中断任务后取行数: %w", err)
	}
	return int(n), nil
}

const taskSelect = `SELECT id, kind, title, payload, status, progress, message, detail, attempts,
       created_at, started_at, finished_at FROM tasks`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (Task, error) {
	var (
		t        Task
		created  int64
		started  sql.NullInt64
		finished sql.NullInt64
		progress sql.NullInt64
		message  sql.NullString
		detail   sql.NullString
		attempts sql.NullInt64
	)
	if err := row.Scan(&t.ID, &t.Kind, &t.Title, &t.Payload, &t.Status,
		&progress, &message, &detail, &attempts, &created, &started, &finished); err != nil {
		return Task{}, err
	}
	t.Progress = int(progress.Int64)
	t.Message = message.String
	t.Detail = detail.String
	t.Attempts = int(attempts.Int64)
	t.CreatedAt = time.UnixMilli(created).UTC()
	if started.Valid {
		v := time.UnixMilli(started.Int64).UTC()
		t.StartedAt = &v
	}
	if finished.Valid {
		v := time.UnixMilli(finished.Int64).UTC()
		t.FinishedAt = &v
	}
	return t, nil
}

func (s *Store) inTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}
