package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// restore.go 把一份快照变回磁盘上的存档。
//
// 这一层的三条规则都是被"错了就丢世界"逼出来的：
//
//  1. 整棵替换 save 目录，而不是只覆盖里面的文件。
//     引擎读档取的是 save/session/<GAMEID>/ 下编号最大的那个 slot 目录，存档时再往上加一号。
//     所以只要机器上还留着比快照更新的 slot，只覆盖 server.dat 之类的动作就是白做——
//     进游戏看到的还是恢复前的世界，而且一句报错都没有。换掉整棵 save
//     是唯一能让"世界回到备份时刻"成立的做法。
//
//  2. 动手之前先自动打一份快照。
//     恢复可能因为盘满、因为包被人改过、因为分片还在写档而失败，
//     任何一种都不该让人同时丢掉"当前这份还能玩的世界"。
//
//  3. 被移开的旧存档先改名留在原地，等新内容解压完、逐个哈希校验通过，才删掉它们。
//     中间任何一步出错就把改名的目录换回去。不存在"旧的已删、新的没写完"那个瞬间。

// Result 是一次恢复的结论，字段是给界面直接显示用的。
type Result struct {
	SnapshotID string `json:"snapshotId"`
	Cluster    string `json:"cluster"`
	// SafetyBackupID 是恢复前自动打的那一份，出问题时的退路。
	SafetyBackupID string `json:"safetyBackupId"`
	// ReplacedShards 是存档目录被整棵换掉的分片。
	ReplacedShards []string `json:"replacedShards"`
	// CreatedShards 是目标房间原本没有、这次新建出来的分片（跨房间迁移）。
	CreatedShards []string `json:"createdShards,omitempty"`
	WrittenFiles  int      `json:"writtenFiles"`
	// KeptConfig 是随包存在但没有覆盖目标房间的配置项，原因见 plan.dest。
	KeptConfig []string `json:"keptConfig,omitempty"`
	Verified   bool     `json:"verified"`
	Note       string   `json:"note"`
	// Aftermath 是失败之后磁盘处于哪种状态，只在 Restore 返回错误时有意义，成功时为空。
	// 单独立一个状态而不是复用"回滚成功/失败"两个布尔，是因为界面要回答的其实是
	// "我现在能不能直接把分片起起来"，而这句结论在四种失败位置上各不相同——
	// 把没动过盘的情况也说成"没能自动换回原存档"，等于替面板撒了一个吓人的谎。
	Aftermath string `json:"aftermath"`
}

// Aftermath 的取值。调用方（worker）拿它们选人话，界面上不出现这些英文值。
const (
	// AftermathUntouched：还没碰过存档目录就失败了，世界仍是恢复前那个。
	AftermathUntouched = "untouched"
	// AftermathRolledBack：动过目录，但移开的东西已经全部换回原位。
	AftermathRolledBack = "rolled-back"
	// AftermathRestored：恢复出来的内容已逐个校验通过，只是旧存档的中间目录没删干净。
	AftermathRestored = "restored"
	// AftermathHalfWritten：动过而且没能换回去，现场既不是旧的也不是新的，绝对不能启动。
	AftermathHalfWritten = "half-written"
)

// Options 是恢复的可选项。
type Options struct {
	// TargetCluster 是恢复到哪个房间，空串表示恢复回快照自己的房间。
	// 非空就是跨房间迁移：把 A 房的世界搬进 B 房。
	TargetCluster string
	// SkipSafety 只给测试用。生产路径一定做安全备份，所以它不来自任何接口参数。
	SkipSafety bool
}

// Restore 把 id 这份快照恢复到目标房间。
//
// 调用方（worker）负责这一层做不到的两件事：确认分片已经停掉，
// 以及恢复完之后要不要把房间再起起来。
func (m *Manager) Restore(ctx context.Context, id string, opts Options) (Result, error) {
	// 一路失败都从这份 res 返回：动手前它一直是"没碰过盘"，
	// 下面每进入一个真会改目录的阶段才改写它——漏标一处就会让界面说错话，
	// 而这里说错的代价是人对着半份世界启动分片。
	res := Result{SnapshotID: id, Aftermath: AftermathUntouched}
	snap, err := m.Get(id)
	if err != nil {
		return res, err
	}
	if !snap.Restorable {
		return res, fmt.Errorf("备份 %s 不能用来恢复：%s", id, snap.RestoreNote)
	}
	archive, err := m.ArchivePath(id)
	if err != nil {
		return res, fmt.Errorf("备份 %s 用不了: %w", id, err)
	}
	// 动手前先确认这份包本身是好的。"恢复失败的现场"和"用一个坏包恢复出来的现场"
	// 长得一模一样，而后者的代价是整个世界要重来。
	vr, err := m.Verify(ctx, id)
	if err != nil {
		return res, fmt.Errorf("恢复前校验这份备份时失败: %w", err)
	}
	if !vr.ArchiveOK {
		// "现有存档没被动过"这句归调用方说（它按 Aftermath 统一措辞），
		// 这里只负责说清是什么没过——两边各说一次，用户看到的那句才不会重复成两段。
		return res, fmt.Errorf("这份备份恢复前就没通过校验：%s", strings.TrimSuffix(vr.Note, "。"))
	}

	target := strings.TrimSpace(opts.TargetCluster)
	if target == "" {
		target = snap.Cluster
	}
	if err := checkClusterName(target); err != nil {
		return res, err
	}
	res.Cluster = target
	names, err := m.Paths.ClusterNames()
	if err != nil {
		return res, err
	}
	if !contains(names, target) {
		return res, fmt.Errorf("要恢复进的集群 %s 不存在，现有：%s", target, strings.Join(names, "、"))
	}

	if !opts.SkipSafety {
		// 把这次要恢复的那一份报成 protected：目录满额时那份安全备份会把最旧的快照
		// 当成超额删掉，而那往往就是正在恢复的这一份（实测踩过，表现是解压到一半归档不见了）。
		safety, err := m.Create(ctx, target, LabelBeforeRestore, id)
		if err != nil {
			return res, fmt.Errorf("恢复前的安全备份没做成：%w", err)
		}
		res.SafetyBackupID = safety.ID
	}

	clusterDir := m.Paths.ClusterDir(target)
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())

	// 第一遍：算落点，并把要覆盖的旧东西一一移开。
	p, staged, err := m.planAndStage(clusterDir, snap, nonce)
	if err != nil {
		// staged 为空说明失败在只读的计划阶段，什么都没移开，res 上那句"没碰过盘"就是对的；
		// 那种时候说"已经换回原位"等于让界面替一次没发生过的风险报平安。
		if len(staged) > 0 {
			if rollbackItems(staged) == nil {
				res.Aftermath = AftermathRolledBack
			} else {
				res.Aftermath = AftermathHalfWritten
			}
		}
		return res, err
	}
	// 计划算完了、旧存档也已移开：从这里往后的任何失败，默认按"现场两边都不对"记账，
	// 只有真正回滚成功或校验通过那两条路会把它改写掉。宁可把人吓停，也不能让人误起。
	res.Aftermath = AftermathHalfWritten
	res.ReplacedShards = p.replacedShards
	res.CreatedShards = p.createdShards
	res.KeptConfig = p.keptConfig

	// 第二遍：解压。
	werr := walkArchive(ctx, archive, func(hdr *tar.Header, r io.Reader) error {
		switch hdr.Typeflag {
		case tar.TypeDir:
			abs, ok, derr := p.dirTarget(hdr.Name)
			if derr != nil {
				return derr
			}
			if !ok {
				return nil
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return fmt.Errorf("建目录 %s: %w", relOf(clusterDir, abs), err)
			}
			return nil
		case tar.TypeReg:
		default:
			return fmt.Errorf("备份里有不是普通文件的条目 %q，这种包不做恢复", hdr.Name)
		}
		abs, ok := p.take[hdr.Name]
		if !ok {
			// 分两种情况，混在一起的代价完全不同：
			// 清单登记过但这次有意不写（目标房间已有配置要保留、或是令牌那类不该落盘的东西），
			// 这是计划的一部分，跳过就好；清单根本没登记过，说明包和清单不是同一件事，
			// 写下去的后果没人能解释，整次恢复判失败并回滚。
			if _, known := p.want[hdr.Name]; known {
				return nil
			}
			return fmt.Errorf("归档里有 %q 但清单没登记它，这份备份的包和清单对不上，已停止恢复", hdr.Name)
		}
		sum, size, err := writeFile(abs, r, 0o644)
		if err != nil {
			return err
		}
		res.WrittenFiles++
		e := p.want[hdr.Name]
		if sum != e.SHA256 || size != e.Size {
			return fmt.Errorf("写出的 %s 和包里的内容对不上（清单 %d 字节，写出 %d 字节），已停止恢复", hdr.Name, e.Size, size)
		}
		return nil
	})

	// 第三遍：落在磁盘上的内容再和清单比一次。
	if werr == nil {
		bad, verr := p.verifyOnDisk()
		if verr != nil {
			werr = verr
		} else if len(bad) > 0 {
			werr = fmt.Errorf("恢复后逐个校验有 %d 个文件和清单对不上：%s", len(bad), strings.Join(firstOf(bad, 3), "、"))
		} else {
			res.Verified = true
		}
	}
	if werr != nil {
		rbErr := p.rollback(staged)
		if rbErr != nil {
			res.Aftermath = AftermathHalfWritten
			return res, fmt.Errorf("%w；而且把原存档换回去也失败了，现场留在 %s：%v", werr, clusterDir, rbErr)
		}
		res.Aftermath = AftermathRolledBack
		res.Note = "已把原有存档换回原位，世界还是恢复前的样子"
		return res, werr
	}
	// 校验通过，这时才删掉移开的旧存档。
	if err := p.drop(staged); err != nil {
		// 世界里已经是快照那份档了，剩下的只是没删干净的中间目录：
		// 这种情况要说"可以起服，目录人工确认后再删"，不能说成现场两边都不对。
		res.Aftermath = AftermathRestored
		return res, fmt.Errorf("恢复的内容已校验通过，但清理旧存档的临时目录时出错：%w；旧存档还留在原地，确认后可以手工删", err)
	}

	var notes []string
	notes = append(notes, fmt.Sprintf("世界已回到备份时刻：%d 个文件写出并逐个比对哈希通过，存档目录被整棵换新的世界是 %s",
		res.WrittenFiles, strings.Join(res.ReplacedShards, "、")))
	if len(res.CreatedShards) > 0 {
		notes = append(notes, fmt.Sprintf("目标房间原本没有这些世界，已新建：%s", strings.Join(res.CreatedShards, "、")))
	}
	if len(res.KeptConfig) > 0 {
		notes = append(notes, fmt.Sprintf("房间配置保持现状没有覆盖（%s）——端口和服务器名不变，要照备份那套就手工改",
			strings.Join(res.KeptConfig, "、")))
	}
	// 刚用来恢复的那一份要保住：它是"现在这个世界来自哪个档"的唯一凭证，
	// 而这份安全备份加进来之后，最旧的那个位置经常正好是它。
	if removed, err := m.Prune(id); err != nil {
		m.log().Warn("恢复后的备份保留清理失败", "err", err.Error())
	} else if len(removed) > 0 {
		notes = append(notes, fmt.Sprintf("按保留上限 %d 份清理了 %d 份最旧的备份", m.keep(), len(removed)))
	}
	res.Note = strings.Join(notes, "；")
	res.Aftermath = ""
	return res, nil
}

// planAndStage 先算出所有落点（这一步只读，不动磁盘），再把要覆盖的东西改名移开。
//
// 落点必须在移开之前算完：dest 要看"目标房间现在有没有这个配置"来决定覆盖还是保留，
// 而移开之后那个文件暂时不在，先后顺序错了会把本该保留的配置写回去。
func (m *Manager) planAndStage(clusterDir string, snap Snapshot, nonce string) (*plan, []stagedItem, error) {
	p := &plan{
		clusterDir: clusterDir,
		want:       make(map[string]FileEntry, len(snap.Files)),
		take:       map[string]string{},
		dirs:       map[string]string{},
		saveRoots:  map[string]bool{},
	}
	for _, shard := range snap.SaveShards {
		// 分片名来自清单 JSON，而它下面要参与拼出"改名移开"和"删除"的路径。
		// 手工改过的清单写成 ../x 就等于让恢复动作去动房间外面的目录，所以先按单段目录名卡死。
		if shard == "" || shard == "." || shard == ".." || filepath.Base(shard) != shard {
			return nil, nil, fmt.Errorf("备份清单里的世界名 %q 不是合法的单段目录名，拒绝恢复", shard)
		}
		p.saveRoots[filepath.Clean(filepath.Join(clusterDir, shard, "save"))] = true
	}
	for _, f := range snap.Files {
		p.want[f.Path] = f
	}
	for _, f := range snap.Files {
		abs, keep, err := p.dest(f.Path, clusterDir)
		if err != nil {
			return nil, nil, err
		}
		if !keep {
			p.keptConfig = append(p.keptConfig, f.Path)
			continue
		}
		p.take[f.Path] = abs
		// 祖先目录全部登记：解压时只有落在这份名单里的目录才建，
		// 免得一个没登记的路径在房间外面开出目录。
		for d := filepath.Dir(abs); d != clusterDir && strings.HasPrefix(d, clusterDir+string(os.PathSeparator)); d = filepath.Dir(d) {
			p.dirs[filepath.Clean(d)] = filepath.Clean(d)
		}
	}
	sort.Strings(p.keptConfig)

	var staged []stagedItem
	for _, shard := range snap.SaveShards {
		orig := filepath.Join(clusterDir, shard, "save")
		exists, err := dirExists(orig)
		if err != nil {
			return p, staged, err
		}
		if !exists {
			p.createdShards = append(p.createdShards, shard)
			continue
		}
		aside := orig + ".rollback-" + nonce
		if err := os.Rename(orig, aside); err != nil {
			return p, staged, fmt.Errorf("把 %s 的现有存档移开时失败：%w。这个世界还在跑的时候不能恢复，先停服再试一次", shard, err)
		}
		staged = append(staged, stagedItem{aside: aside, orig: orig, isDir: true})
		p.replacedShards = append(p.replacedShards, shard)
	}
	// 非 save 的文件（世界设定 lua、目标缺配置时的 server.ini）也先移开。
	for _, rel := range sortedKeys(p.take) {
		if strings.Contains(rel, "/save/") {
			continue
		}
		abs := p.take[rel]
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		aside := abs + ".rollback-" + nonce
		if err := os.Rename(abs, aside); err != nil {
			return p, staged, fmt.Errorf("移开 %s 时失败: %w", rel, err)
		}
		staged = append(staged, stagedItem{aside: aside, orig: abs})
	}
	return p, staged, nil
}

// plan 是这次恢复的落点表。
type plan struct {
	clusterDir string
	want       map[string]FileEntry
	take       map[string]string
	dirs       map[string]string
	// saveRoots 是这份快照带存档的分片根目录（<房间>/<分片>/save），用于承认包里的空目录。
	saveRoots      map[string]bool
	keptConfig     []string
	replacedShards []string
	createdShards  []string
}

// dirTarget 决定包里的一个目录条目要不要在目标房间建出来。
//
// 两类要建：要落地的文件的祖先目录，以及存档目录树里的任何目录。
// 第二类必须单独承认：mod_config_data 这种目录一个文件都没有，清单的 files 名单里自然没它，
// 但引擎写模组存档时要的是目录存在。只按"这儿有没有文件要落"判断，空目录就会静默丢掉，
// 而恢复照样报成功。除此之外没有文件要落的目录不建——那多半是包被改过留下的空壳。
func (p *plan) dirTarget(name string) (string, bool, error) {
	rel := strings.TrimSuffix(name, "/")
	if rel == "" {
		return "", false, nil
	}
	if strings.Contains(rel, "..") {
		return "", false, fmt.Errorf("备份里的目录条目 %q 不是合法路径，拒绝恢复", name)
	}
	root := filepath.Clean(p.clusterDir)
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", false, fmt.Errorf("备份里的目录 %q 会建到房间外面，拒绝恢复", rel)
	}
	if _, ok := p.dirs[abs]; ok {
		return abs, true, nil
	}
	for saveRoot := range p.saveRoots {
		if abs == saveRoot || strings.HasPrefix(abs, saveRoot+string(os.PathSeparator)) {
			return abs, true, nil
		}
	}
	return "", false, nil
}

// dest 把一个归档内的相对路径映射到目标集群目录下的绝对路径，并决定要不要写。
//
// 三条规则：
//   - 解析结果落在集群目录之外的一律拒绝（包被人为改过、或路径里带 ..）；
//   - 令牌永远不写。快照本来就不含它，这里再挡一次是防有人在清单里手工塞一条；
//   - cluster.ini 与 <分片>/server.ini 只在目标缺它的时候才写。
//
// 第三条是取舍：覆盖已有配置会把这台机器现在的端口和服务器名换成备份当时的值，
// 同一台机器上两个房间撞同一个端口就是"立刻起不了服"，而界面看起来像恢复把服务器搞挂了。
// 但跨房间迁移时目标房间本来就没有这个分片的配置，不补齐它就少一个能跑的分片，
// 所以判据是"目标有没有"，不是"快照里有没有"。
func (p *plan) dest(rel, clusterDir string) (string, bool, error) {
	if strings.Contains(rel, "..") {
		return "", false, fmt.Errorf("备份清单里有非法路径 %q，拒绝恢复", rel)
	}
	if filepath.Base(filepath.FromSlash(rel)) == "cluster_token.txt" {
		return "", false, nil
	}
	root := filepath.Clean(clusterDir)
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if abs == root || !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", false, fmt.Errorf("备份里的 %q 会写到 %s 外面，拒绝恢复", rel, root)
	}
	isConfig := rel == "cluster.ini" || (strings.HasSuffix(rel, "/server.ini") && strings.Count(rel, "/") == 1)
	if isConfig {
		if ok, _ := fileExists(abs); ok {
			return abs, false, nil
		}
	}
	return abs, true, nil
}

// verifyOnDisk 把已经写出去的每个文件和清单再比一次。
//
// 这不是重复劳动：解压那一步比的是"从包里读到的"和"写进包时登记的"，
// 这里比的是"现在磁盘上的"和"清单"。中间隔着临时文件、rename、以及可能的写盘失败——
// 那正是"面板说恢复成功了，进游戏发现世界是坏的"的来源。
func (p *plan) verifyOnDisk() ([]string, error) {
	var bad []string
	for _, rel := range sortedKeys(p.take) {
		want := p.want[rel]
		sum, size, err := hashFile(p.take[rel])
		if err != nil {
			return bad, err
		}
		if sum != want.SHA256 || size != want.Size {
			bad = append(bad, fmt.Sprintf("%s（清单 %d 字节 %s…，磁盘 %d 字节 %s…）",
				rel, want.Size, want.SHA256[:8], size, sum[:8]))
		}
	}
	sort.Strings(bad)
	return bad, nil
}

// rollback 删掉这次新写的内容，把移开的旧存档换回去。
func (p *plan) rollback(staged []stagedItem) error {
	var errs []string
	for _, shard := range append(append([]string{}, p.replacedShards...), p.createdShards...) {
		target := filepath.Join(p.clusterDir, shard, "save")
		if err := os.RemoveAll(target); err != nil {
			errs = append(errs, fmt.Sprintf("清理恢复出的 %s: %v", target, err))
		}
	}
	for _, s := range staged {
		if err := os.Rename(s.aside, s.orig); err != nil {
			errs = append(errs, fmt.Sprintf("把 %s 换回原位失败: %v", s.aside, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("回滚有 %d 处没做成：%s", len(errs), strings.Join(errs, "；"))
	}
	return nil
}

// drop 删掉移开留下的旧存档——只在校验通过之后调用。
func (p *plan) drop(staged []stagedItem) error {
	var errs []string
	for _, s := range staged {
		if err := os.RemoveAll(s.aside); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.aside, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("旧存档的临时目录没清干净：%s", strings.Join(errs, "；"))
	}
	return nil
}

type stagedItem struct {
	aside string
	orig  string
	isDir bool
}

// rollbackItems 用于"还没开始解压就失败了"：只需把移开的东西换回去。
// 返回错误表示有目录没能换回原位——调用方要据此决定给用户哪句结论。
func rollbackItems(staged []stagedItem) error {
	var errs []string
	for _, s := range staged {
		if err := os.Rename(s.aside, s.orig); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.aside, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("有 %d 处没换回原位：%s", len(errs), strings.Join(errs, "；"))
	}
	return nil
}

// writeFile 落一个文件并返回它内容的哈希。临时文件 + rename，
// 这样中途失败不会留下半截 worldsave.gz 让引擎去读。
func writeFile(abs string, r io.Reader, perm os.FileMode) (string, int64, error) {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("建目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(abs)+".tmp-")
	if err != nil {
		return "", 0, fmt.Errorf("在 %s 建临时文件: %w", dir, err)
	}
	tmpName := tmp.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", n, fmt.Errorf("写 %s: %w", filepath.Base(abs), err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", n, fmt.Errorf("设置 %s 的权限: %w", filepath.Base(abs), err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", n, fmt.Errorf("关闭 %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return "", n, fmt.Errorf("替换 %s: %w", abs, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("校验时读不到 %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, fmt.Errorf("校验 %s 时读盘失败: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func relOf(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}
