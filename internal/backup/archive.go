package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dstpanel/internal/dst"
)

// archive.go 负责"把一个集群的世界状态变成一个可校验的包"。
//
// 打包和校验用的是同一份登记：写包时边写边算 sha256，之后"校验快照"和
// "恢复完再确认写对了"都对着这份清单比。之所以不信"包能解开"就等于"包是对的"，
// 是因为截断的 gzip 一样能解出前面几十个文件。

// srcFile 是待打包的一个磁盘文件（或目录）。
type srcFile struct {
	rel string // 归档内路径，永远用正斜杠
	abs string
}

// Collect 列出这个集群该进快照的东西。
//
// 做法是"整个房间目录走一遍，按名单排除"，而不是"只挑我认识的那几个文件"：
// 引擎和玩家会往房间目录里放面板预料不到的东西（封禁名单、玩家名黑名单、模组自己的
// 存档数据、手工放的世界预设），白名单会让它们安静地漏掉——恢复完之后封禁名单是空的，
// 而没有任何一句报错说这事。排除名单见 skipRel，只有三类：凭据、面板命令通道、引擎日志。
//
// 归档内的路径全部用正斜杠，并且都以分片名或房间文件名开头，恢复时靠它认目的地。
// 条目按路径排好序，这样同一份存档打两次得到的序列可比较。
func (m *Manager) Collect(cluster string) ([]srcFile, []string, error) {
	if err := checkClusterName(cluster); err != nil {
		return nil, nil, err
	}
	clusterDir := m.Paths.ClusterDir(cluster)
	if ok, err := dirExists(clusterDir); err != nil {
		return nil, nil, err
	} else if !ok {
		return nil, nil, fmt.Errorf("集群目录 %s 不存在，没有可打包的东西", clusterDir)
	}
	if len(m.Paths.ShardNames(cluster)) == 0 {
		return nil, nil, fmt.Errorf("%s 下没有 Master 或 Caves 世界目录，这不是一个能备份的房间", clusterDir)
	}

	var items []srcFile
	var skipped []string
	skip := func(rel, why string) { skipped = append(skipped, rel+"："+why) }
	backupDir := filepath.Clean(m.Dir)

	walkErr := filepath.WalkDir(clusterDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// 引擎的自动存档是个滚动环：写新档的同时删掉编号最旧的那份。
			// 遍历撞上这一步不算失败，记一条中文说明继续走——不然定时备份
			// 每周都要撞上一次，而用户看到的是一条带 Go 英文的报错。
			if errors.Is(err, os.ErrNotExist) {
				if d != nil {
					if rel, rerr := filepath.Rel(clusterDir, path); rerr == nil {
						skip(filepath.ToSlash(rel), "遍历到这里时它已被引擎淘汰掉（自动存档在滚动最旧的档）")
					}
					if d.IsDir() {
						return filepath.SkipDir
					}
				}
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(clusterDir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		// 有人把快照目录配在房间里面时，备份会把备份打进备份，越打越大。
		if d.IsDir() && (path == backupDir || strings.HasPrefix(path, backupDir+string(os.PathSeparator))) {
			skip(rel, "备份目录本身不打包进备份")
			return filepath.SkipDir
		}
		whyRel := rel
		if d.IsDir() {
			// 目录按带尾斜杠的形式过黑名单。不补这一刀就漏一半：规则写的是
			// /server_log/ 这种带分隔符的形状，而 WalkDir 给的目录路径没有尾斜杠，
			// 于是日志目录本身作为空目录进了包（云机实测：包里多了 4 个
			// backup/server_log 与 backup/server_chat_log 目录，清单却写着"跳过了引擎日志"）。
			whyRel = rel + "/"
		}
		if why := skipRel(whyRel); why != "" {
			skip(rel, why)
			if d.IsDir() {
				// 整个子树一起跳过，不再逐个文件列：恢复存档时移开的 .rollback- 目录
				// 下面就是整棵旧档，一行"这个目录不打包"比几百行重复原因有用。
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// 空目录也进包：mod_config_data 这类目录不在的话，引擎是写档时才发现目录没了，
			// 那时候恢复已经算成功了。
			items = append(items, srcFile{rel: rel + "/", abs: path})
			return nil
		}
		if !d.Type().IsRegular() {
			// 软链和设备文件：存档目录里不该有。跳过并记下来，不能假装打包了它。
			skip(rel, "不是普通文件（软链或设备），恢复时无处可放")
			return nil
		}
		items = append(items, srcFile{rel: rel, abs: path})
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("遍历集群目录 %s: %w", clusterDir, walkErr)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	sort.Strings(skipped)
	return items, skipped, nil
}

// Create 打一份快照，返回它的清单视图。protected 见 Prune：
// 恢复流程打那份"恢复前的安全备份"时，要把正在恢复的那一份报进来，
// 否则这次备份自己就可能把它挤掉。
func (m *Manager) Create(ctx context.Context, cluster, label string, protected ...string) (Snapshot, error) {
	return m.CreateWithProgress(ctx, cluster, label, nil, protected...)
}

// CreateWithProgress 比 Create 多一个进度回调：每开始处理一个条目报一次（已完成数、总数）。
//
// 走任务队列的备份需要它：一百多 MB 的存档在这台 4h4g 上要压几十秒，
// 没有进度的话用户在任务中心只能看到"执行中"，分不清是在跑还是卡死了。
// progress 为 nil 时就是 Create 的行为。
//
// 落盘顺序是：包先写进同目录的临时文件（写完才知道大小和每个文件的哈希）→
// 改名成正式归档 → 写清单 → 按保留上限收尾。任何一步失败最多留下一个 .tmp 文件，
// 快照目录里不会出现"有归档没清单"的半成品，那种东西在列表里就是点不动的一行。
func (m *Manager) CreateWithProgress(ctx context.Context, cluster, label string, progress func(done, total int), protected ...string) (Snapshot, error) {
	items, skipped, err := m.Collect(cluster)
	if err != nil {
		return Snapshot{}, err
	}
	if len(items) == 0 {
		return Snapshot{}, fmt.Errorf("%s 里没有可打包的文件，先看这个房间有没有生成过存档", cluster)
	}
	return m.packItems(ctx, cluster, label, items, skipped, progress, protected...)
}

// packItems 把一份已经列好的条目打成"归档 + 清单"两个文件。
//
// Create（打现成的房间）与 Import（吃外部上传的包）共用这一份实现，因为上面那段落盘顺序
// 本身就是一条规矩，分开写两遍只会出现"其中一份守规矩"。
//
// cluster 在这里只是登记进清单的那个名字：打包过程一个字节都不读房间目录，
// 所以导入的外部存档也能走同一个函数——它的条目来自临时解压目录。
func (m *Manager) packItems(ctx context.Context, cluster, label string, items []srcFile, skipped []string,
	progress func(done, total int), protected ...string) (Snapshot, error) {
	if len(items) == 0 {
		return Snapshot{}, fmt.Errorf("%s 一个可打包的条目都没有，没做成快照", cluster)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("创建备份目录 %s: %w", m.Dir, err)
	}
	now := time.Now().UTC()
	id, err := m.freeID(cluster, label, now)
	if err != nil {
		return Snapshot{}, err
	}
	tmpPath := filepath.Join(m.Dir, id+ArchiveSuffix+".tmp")
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("创建临时归档 %s: %w", tmpPath, err)
	}

	var (
		entries  []FileEntry
		changed  []string
		vanished []string
		total    int64
		shardSet = map[string]bool{}
	)
	writeErr := func() error {
		gz := gzip.NewWriter(tmp)
		tw := tar.NewWriter(gz)
		for i, it := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if progress != nil {
				progress(i+1, len(items))
			}
			sum, n, isReg, grew, gone, err := m.packOne(tw, it)
			if err != nil {
				return err
			}
			if gone {
				vanished = append(vanished, it.rel)
				continue
			}
			if !isReg {
				// 目录只写进包，不登记进清单的文件表：Verify 是逐个普通文件比哈希的，
				// 目录登记了反而会报"清单里有而包里缺"，每份快照都校验不过。
				continue
			}
			if grew {
				changed = append(changed, it.rel)
			}
			entries = append(entries, FileEntry{Path: it.rel, Size: n, SHA256: sum})
			total += n
			if parts := strings.Split(it.rel, "/"); len(parts) >= 2 && parts[0] != "" {
				shardSet[parts[0]] = true
			}
		}
		if err := tw.Close(); err != nil {
			return fmt.Errorf("收尾 tar: %w", err)
		}
		if err := gz.Close(); err != nil {
			return fmt.Errorf("收尾 gzip: %w", err)
		}
		return tmp.Close()
	}()
	if writeErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		if ctx.Err() != nil {
			return Snapshot{}, fmt.Errorf("备份被中断（任务超时或面板退出），临时归档已删掉，存档没动过: %w", ctx.Err())
		}
		return Snapshot{}, writeErr
	}
	if err := os.Rename(tmpPath, m.archivePath(id)); err != nil {
		_ = os.Remove(tmpPath)
		return Snapshot{}, fmt.Errorf("把临时归档换成正式文件 %s: %w", m.archivePath(id), err)
	}
	// 包大小必须记进清单：它是"这个包被人替换过"最便宜的一道检测（不用解压就能比）。
	// 不记就等于 view 里那条大小检查永远是死代码，半截包照样标着"可恢复"。
	archiveStat, err := os.Stat(m.archivePath(id))
	if err != nil {
		return Snapshot{}, fmt.Errorf("归档已经写好，但读它的信息失败 %s: %w", m.archivePath(id), err)
	}

	mf := Manifest{
		ID:                id,
		Cluster:           cluster,
		Label:             label,
		CreatedAt:         now.Format(time.RFC3339),
		Shards:            sortedKeys(shardSet),
		PanelVersion:      m.Version,
		FileCount:         len(entries),
		UncompressedBytes: total,
		ArchiveBytes:      archiveStat.Size(),
		Files:             entries,
		Skipped:           skipped,
	}
	if len(changed) > 0 {
		// 边打边被写：包里的字节是自洽的（哈希算的就是写进包的内容），
		// 但这一份快照不再是"某一个时刻"的世界。说清楚，让人自己决定要不要停服重打。
		mf.Note = fmt.Sprintf("打包期间有 %d 个文件还在被服务端改写（%s），这份备份的时间点不完全一致；"+
			"要一份精确档就先把世界停掉再备份", len(changed), strings.Join(firstOf(changed, 3), "、"))
	}
	if len(vanished) > 0 {
		// 这条必须写进清单，而不是悄悄少几个文件：用户对比"房间里有 20 份自动存档、
		// 快照里只有 19 份"时，答案是"有一份在打包那一刻被引擎删掉了"，不是"面板漏了"。
		mf.Note = addNote(mf.Note, "打包期间有 %d 个条目被引擎从磁盘上淘汰掉了（%s），它们没进这份备份；"+
			"这是自动存档在滚动删最旧的档，最近的世界状态照样在里面",
			len(vanished), strings.Join(firstOf(vanished, 3), "、"))
	}
	if err := m.writeManifest(mf); err != nil {
		return Snapshot{}, err
	}
	// 打完就按 Keep 收一次尾。清理放在这里而不是交给调用方：漏一次就是少一次空间。
	removed, pruneErr := m.Prune(protected...)
	if pruneErr != nil {
		m.log().Warn("按保留上限清理旧备份失败", "err", pruneErr.Error())
	} else if len(removed) > 0 {
		mf.Note = addNote(mf.Note, "按保留上限 %d 份清理了 %d 份最旧的备份：%s",
			m.keep(), len(removed), strings.Join(removed, "、"))
		if err := m.writeManifest(mf); err != nil {
			return Snapshot{}, err
		}
	}
	snap, err := m.Get(id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("备份已经打好（%s），但回读清单时失败: %w", id, err)
	}
	return snap, nil
}

// addNote 往说明里再追加一句。已有说明为空时不能补分隔符，
// 否则界面看到的第一句是一个孤零零的"；按保留上限…"。
func addNote(note, format string, args ...any) string {
	s := fmt.Sprintf(format, args...)
	if strings.TrimSpace(note) == "" {
		return s
	}
	return note + "；" + s
}

func (m *Manager) writeManifest(mf Manifest) error {
	raw, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化清单 %s: %w", mf.ID, err)
	}
	return dst.WriteFileAtomic(m.manifestPath(mf.ID), raw, 0o644)
}

// packOne 把 Collect 列出的一个条目原样写进包，返回它进包的内容哈希与字节数。
//
// 取信息用的是已打开句柄的 Stat，不是先 os.Stat 再 os.Open：云机上实测过一次
// "列目录时还在、轮到打包时已经被引擎删掉"（save/session/<GAMEID>/0000000068），
// 那一次整个备份任务失败，报错还是 Go 的英文原文。分成两步问就永远有这一步的窗口，
// 打开一次问一次才是自洽的。gone 为真表示这个条目已经不存在，调用方记进清单说明即可。
func (m *Manager) packOne(tw *tar.Writer, it srcFile) (sum string, n int64, isReg, grew, gone bool, err error) {
	f, oerr := m.openFile(it.abs)
	if oerr != nil {
		if errors.Is(oerr, os.ErrNotExist) {
			return "", 0, false, false, true, nil
		}
		return "", 0, false, false, false, fmt.Errorf("打开 %s: %w", it.rel, oerr)
	}
	defer f.Close()
	st, serr := f.Stat()
	if serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			return "", 0, false, false, true, nil
		}
		return "", 0, false, false, false, fmt.Errorf("打包前读 %s 的信息: %w", it.rel, serr)
	}
	hdr := &tar.Header{Name: it.rel, ModTime: st.ModTime().UTC().Truncate(time.Second)}
	if st.IsDir() {
		// 空目录也要进包：mod_config_data 这类目录不在的话，
		// 引擎是写档时才发现目录没了，那时候恢复已经算成功了。
		hdr.Typeflag = tar.TypeDir
		hdr.Name = strings.TrimSuffix(hdr.Name, "/") + "/"
		hdr.Mode = 0o755
		if werr := tw.WriteHeader(hdr); werr != nil {
			return "", 0, false, false, false, fmt.Errorf("写目录条目 %s: %w", it.rel, werr)
		}
		return "", 0, false, false, false, nil
	}
	hdr.Typeflag = tar.TypeReg
	hdr.Mode = 0o644
	hdr.Size = st.Size()
	if werr := tw.WriteHeader(hdr); werr != nil {
		return "", 0, false, true, false, fmt.Errorf("写文件条目 %s: %w", it.rel, werr)
	}
	sum, n, grew, cerr := copyHash(tw, f, st.Size())
	if cerr != nil {
		return "", n, false, false, false, cerr
	}
	return sum, n, true, grew, false, nil
}

// copyHash 把已经打开的文件写进包，同时算 sha256：一次读盘同时喂给 tar 和哈希，
// 免得为了登记哈希再把这一百多兆读第二遍。
//
// 返回 grew 表示拷完之后文件还有新内容（分片正在写档）。这时包里的字节
// 依然和清单对得上，所以不报错——报错会让定时备份每次都撞在自动存档上。
// 反过来文件变短是真故障（包会短一块），由 n != size 那条拦下来。
func copyHash(w io.Writer, f *os.File, size int64) (sum string, n int64, grew bool, err error) {
	name := filepath.Base(f.Name())
	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(w, h), io.LimitReader(f, size))
	if err != nil {
		return "", n, false, fmt.Errorf("打包 %s: %w", name, err)
	}
	if n != size {
		return "", n, false, fmt.Errorf("打包 %s 时只读到 %d 字节，而文件大小是 %d：这个文件在备份过程中被截短了，"+
			"先停掉世界再备份一次", name, n, size)
	}
	tail := make([]byte, 1)
	if k, rerr := f.Read(tail); rerr == nil && k == 1 {
		grew = true
	}
	return hex.EncodeToString(h.Sum(nil)), n, grew, nil
}

// VerifyResult 是一次校验的结论。
type VerifyResult struct {
	SnapshotID string   `json:"snapshotId"`
	Checked    int      `json:"checked"`
	Missing    []string `json:"missing,omitempty"`
	Unexpected []string `json:"unexpected,omitempty"`
	Mismatched []string `json:"mismatched,omitempty"`
	ArchiveOK  bool     `json:"archiveOk"`
	Note       string   `json:"note"`
}

// Verify 把归档整个读一遍，逐文件比对大小与哈希。只读，不动任何存档。
//
// 这是"恢复前先确认这份快照没坏"那一步，也是别人把 .tar.gz 拷到另一台机器后
// 能自己做的检查，所以它给的是可执行的一句话，不是一句"校验通过"。
func (m *Manager) Verify(ctx context.Context, id string) (VerifyResult, error) {
	mf, err := m.readManifest(id)
	if err != nil {
		return VerifyResult{}, err
	}
	path, err := m.ArchivePath(id)
	if err != nil {
		return VerifyResult{}, err
	}
	want := make(map[string]FileEntry, len(mf.Files))
	for _, f := range mf.Files {
		want[f.Path] = f
	}
	res := VerifyResult{SnapshotID: id}
	seen := map[string]bool{}
	err = walkArchive(ctx, path, func(hdr *tar.Header, r io.Reader) error {
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		seen[hdr.Name] = true
		e, ok := want[hdr.Name]
		if !ok {
			res.Unexpected = append(res.Unexpected, hdr.Name)
			return nil
		}
		sum, size, err := readHash(r)
		if err != nil {
			return err
		}
		res.Checked++
		if size != e.Size || sum != e.SHA256 {
			res.Mismatched = append(res.Mismatched,
				fmt.Sprintf("%s：清单记 %d 字节 %s…，包里是 %d 字节 %s…", hdr.Name, e.Size, e.SHA256[:8], size, sum[:8]))
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	for _, f := range mf.Files {
		if !seen[f.Path] {
			res.Missing = append(res.Missing, f.Path)
		}
	}
	res.ArchiveOK = len(res.Missing) == 0 && len(res.Mismatched) == 0 && len(res.Unexpected) == 0
	switch {
	case !res.ArchiveOK:
		res.Note = fmt.Sprintf("这份备份校验不过：%d 个文件对不上哈希，清单里有而包里缺 %d 个，包里有而清单没登记 %d 个。别拿它做恢复。",
			len(res.Mismatched), len(res.Missing), len(res.Unexpected))
	case res.Checked == 0:
		res.Note = "包里没有清单登记过的任何文件，这份备份是空的"
	default:
		res.Note = fmt.Sprintf("%d 个文件逐个比过哈希，全部和清单对得上", res.Checked)
	}
	return res, nil
}

// walkArchive 顺序读包里的每个条目，ctx 结束就停。
func walkArchive(ctx context.Context, path string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		// 这份错误会原样进任务中心那一行的消息，所以"文件不在了"要说成中文：
		// 操作系统那句 "The system cannot find the file specified" 用户读不懂，
		// 也分不清是自己删了、清理删了，还是路径配错了。
		if os.IsNotExist(err) {
			return fmt.Errorf("归档 %s 已经不在了：备份目录里那个 .tar.gz 被删掉了（可能是手工删的，或按保留上限清理时删的）", filepath.Base(path))
		}
		return fmt.Errorf("打开归档 %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("归档 %s 不是合法的 gzip（多半是没写完就被截断）: %w", filepath.Base(path), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读归档 %s: %w", filepath.Base(path), err)
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

func readHash(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, fmt.Errorf("读包里的内容: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstOf(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[:n]
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func fileExists(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !st.IsDir(), nil
}

func dirExists(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return st.IsDir(), nil
}
