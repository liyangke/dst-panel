package backup

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/dst"
)

// import.go 把"别人给的一个存档包"变成面板自己的快照。
//
// 为什么要绕这一道，而不是把包直接解压进房间：
//   - 换存档目录的那条路已经存在，而且是踩过坑的那一条（整棵替换 save、动手前先打
//     恢复前的安全备份、逐个哈希校验、失败就改名换回去）。导入只产出一份普通快照，
//     后面那段就能原封不动复用，不必再写第二份"错了会丢世界"的代码。
//   - 用户上传的东西必须先以快照的身份进列表：看得见、能校验、能下载、能删，
//     出问题的时候才有话说。直接落进房间目录的话，那份档在面板里是个隐形文件。
//
// 校验看的是引擎自己的读档规则（本机服务端包里 data/scripts/shardindex.lua、
// gamelogic.lua 读到的，不是从别人面板抄的结论）：
//   - 专用服务端启动时读 <世界>/save/shardindex（shardindex.lua:166 Load 走
//     TheSim:GetPersistentString("shardindex")），把里面的 session_id 放进索引；
//   - 然后 CheckWorldFile 判 TheNet:GetWorldSessionFile(session_id) 在不在，
//     在就 LoadSlot→DoLoadWorld，不在就打印 "Load Slot: ... generating new world"
//     （gamelogic.lua:1103）。
//   - shardindex 读不出来时（IsDedicated 那一支，shardindex.lua:130）session_id 是 nil，
//     IsEmpty 为真，启动流程会 Delete 掉索引再 SetServerShardData 生成一个新世界。
//
// 所以"包里有 save/session/<某个编号>，却没有 shardindex"这一种必须拦住：
// 导进去照样起服，玩家进去是一个全新的世界，而一句报错都没有。
// 这一层拦不住的话，用户看到的是"面板说导入成功，可我的建筑呢"。

// 这几条上限防的是两件事：包被人换成别的东西，以及 zip 炸弹
// （解压比可以到 1000:1）。参照点是实测的：云机一份全房间存档解压前 137.9MB、
// 打成面板的包是 19.0MB。2GB 这道线比它宽十倍，足够任何真实存档通过，
// 又能把"一个 20MB 的包解开 40GB"挡在写盘之前。
const (
	// MaxImportArchiveBytes 是上传的包本身的大小上限，接口层用同一个数报上限。
	MaxImportArchiveBytes = 512 << 20
	maxImportUncompressed = 2 << 30
	maxImportEntryBytes   = 512 << 20
	maxImportEntries      = 20000
	maxImportDepth        = 24
	// 认世界编号要读 shardindex 的内容，引擎写它只有几 KB；超过这个数就不是索引文件。
	maxImportIndexBytes = 1 << 20
)

// 包里房间目录那一层（集群根）可以被面板收进快照的文件。
// 名单之外的一律不取：一个手工打包的目录里什么都有（截图、日志、模组解压出来的东西），
// 全取等于让一次导入往房间目录里写任意文件。cluster_token.txt 在这里，
// 是为了让它撞上 skipRel 那句"令牌不打进备份"的具体原因，而不是"不认识的名字"。
var clusterLevelFiles = []string{
	"cluster.ini", "clusteroptions.ini", "cluster_token.txt",
	"adminlist.txt", "blocklist.txt", "whitelist.txt",
}

// 只把这几个小文件的内容读进内存：认世界编号靠 shardindex，
// 判"包里没有 save 只有设定"靠 modoverrides 在不在。其余内容只在解开那一步流过。
var indexBasenames = map[string]bool{
	"shardindex": true, "saveindex": true, "modoverrides.lua": true,
}

// 专用服务端滚动的自动档是纯数字编号，云机实测是 10 位（0000000084 与它的 .meta）；
// 客户端早期的写法是 slot-0001/server.dat。两种都要认，判据见 checkShard 那段注释里的引擎读档顺序。
// 这里故意不锁死位数（只要求"至少 6 位纯数字"）：引擎的格式化写法变过，
// 而这一条只影响预览里"档位形状"那句话好不好看，世界目录里面的文件本来就全取，
// 认漏一个形状不会少拷任何文件。
var numberedSaveRE = regexp.MustCompile(`^[0-9]{6,}(\.meta)?$`)

// ImportShard 是"包里认出来的一个世界"。
type ImportShard struct {
	Shard       string   `json:"shard"`
	IndexFile   string   `json:"indexFile"`
	SessionID   string   `json:"sessionId"`
	WorldFiles  []string `json:"worldFiles"`
	Slots       int      `json:"slots"`
	FileCount   int      `json:"fileCount"`
	Bytes       int64    `json:"bytes"`
	HasModPrefs bool     `json:"hasModPrefs"`
	Problems    []string `json:"problems,omitempty"`
	Note        string   `json:"note"`
}

// ImportPreview 是一个存档包的结构结论，给界面在提交之前展示。
type ImportPreview struct {
	Package      string `json:"package"`
	Kind         string `json:"kind"`
	KindLabel    string `json:"kindLabel"`
	ArchiveBytes int64  `json:"archiveBytes"`
	// ArchiveLabel / UncompressedLabel 是给人看的大小。字节数留在上面那些字段里
	// 供判断，界面一律用这两句——否则前端要自己造一套单位换算，
	// 而面板里"多大"的说法已经有一套了（见 humanBytes），不该出现第二套。
	ArchiveLabel      string        `json:"archiveLabel"`
	UncompressedLabel string        `json:"uncompressedLabel"`
	UncompressedBytes int64         `json:"uncompressedBytes"`
	EntryCount        int           `json:"entryCount"`
	FileCount         int           `json:"fileCount"`
	Root              string        `json:"root"`
	Shape             string        `json:"shape"`
	ShapeNote         string        `json:"shapeNote"`
	Shards            []ImportShard `json:"shards"`
	// dropped / warnings / problems 不许写 omitempty：界面是把这三块当数组读的
	// （v-for 之前要先量 .length），空数组和"这个键不存在"对 JS 来说是两回事。
	Dropped   []string `json:"dropped"`
	Warnings  []string `json:"warnings"`
	Problems  []string `json:"problems"`
	CanImport bool     `json:"canImport"`
	Note      string   `json:"note"`
}

// orEmptyArray 把"没东西"的 nil 变成能序列化成 [] 的空数组。
//
// Go 里 nil 切片进 JSON 是 null，前端读 null.length 会在渲染那一下抛错，
// 整个抽屉直接空白——用户看到的是"点了没反应"，而不是"这个包没问题可报"。
func orEmptyArray[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// ImportOptions 是一次导入的参数。
type ImportOptions struct {
	// Cluster 是这份快照登记给哪个房间（恢复默认就换掉它的存档）。
	// 只校验它是不是一个合法的目录名；房间此刻在不在磁盘上由调用方（worker）按白名单问。
	Cluster string
	// OnUnpack / OnPack 报进度（已完成条目数、总数），nil 就是不要。
	// 分两段是因为这两段的用户可见含义不同：一个是"东西从包里出来"，
	// 一个是"东西进快照"，混成一个百分比的话，卡在 50% 就没人知道卡在哪一步。
	OnUnpack func(done, total int)
	OnPack   func(done, total int)
}

// pkgRaw 是包里的一个原始条目，只读了名字、大小和是不是目录。
type pkgRaw struct {
	name  string
	size  int64
	isDir bool
}

// pkgItem 是 pkgRaw 过了 cleanPkgPath 之后的形状：segs 是能安全使用的路径片段。
type pkgItem struct {
	segs  []string
	rel   string
	isDir bool
	size  int64
	name  string
}

// mapped 是一个包条目的归类结论。
type mapped struct {
	rel     string // 落进快照的路径；为空表示不要这一条
	dropWhy string // 不要的原因（rel 为空时才有意义）
}

// pkgContent 是"只读包目录"这一趟的产出。
type pkgContent struct {
	kind string
	// kindLabel 是给人看的格式说法（"zip 压缩包"）。界面不认裸格式值，
	// 而这里给中文是为了报错和预览用同一个说法。
	kindLabel    string
	archiveBytes int64
	raw          []pkgRaw
	// index 是几个小索引文件的内容，键是清洗过的包内路径（不是快照路径），
	// 所以 judge 归完类之后要按 mapping 再搬一次键。
	index map[string]string
	// special 是软链、硬链、设备这类条目：不是存档目录该有的东西，
	// 解开它要么写出预期外的文件、要么沿着链接写到目录外面，所以整包拦住。
	special []string
	// overLimit 记下"包大到面板不打算当存档处理"这件事，交给 judge 说成问题，
	// 而不是在这一层报错——报错用户只看到一句话，问题列表里他能看到全部原因。
	overLimit []string
}

// importPlan 是"这个包该怎么变成快照"的完整结论。
type importPlan struct {
	pkgName string

	kind         string
	kindLabel    string
	archiveBytes int64
	entryCount   int

	root  string
	shape string
	// shapeNote 是"面板把这个包当成什么形状"那句中文结论。
	shapeNote string

	items   []pkgItem
	mapping map[string]mapped
	// taken 是保留下来的快照路径集合（目录带尾斜杠），checkShard 靠它数文件。
	taken map[string]bool
	// index 是 taken 里那几个索引文件的内容，键换成快照路径。
	index map[string]string
	// claim 记住"这个快照路径是被包里哪个名字占用的"，用来抓 Master/master 撞车。
	claim map[string]string

	keepCount int
	files     int
	total     int64

	dropped  []string
	warnings []string
	problems []string
	shards   []ImportShard
}

// InspectImport 读一个包的目录（只读名字和大小，除了几个索引文件），给出结构结论。
//
// 接口层在上传落地之后马上调它，用户在"导入"按钮之前就能看到：
// 面板认出这是哪种包、里面有几个世界、世界编号是什么、有哪些东西被刻意丢掉了。
// 判断标准与 Import 完全同一份（同一个 planPackage），所以这里说能导，
// 导入就不会在半路因为"结构不对"失败。
func (m *Manager) InspectImport(pkgPath string) (ImportPreview, error) {
	plan, err := m.planPackage(pkgPath)
	if err != nil {
		return ImportPreview{}, err
	}
	return plan.preview(), nil
}

// Import 把一个外部存档包变成一份面板快照，返回它的清单视图。
//
// 它不碰任何房间目录：写出来的只有备份目录里的 .tar.gz + .manifest.json，
// 以及一个用完就删的临时解压树。要让它真的换掉世界的存档，用 Restore。
func (m *Manager) Import(ctx context.Context, pkgPath string, opts ImportOptions) (Snapshot, error) {
	if err := checkClusterName(opts.Cluster); err != nil {
		return Snapshot{}, err
	}
	plan, err := m.planPackage(pkgPath)
	if err != nil {
		return Snapshot{}, err
	}
	if len(plan.problems) > 0 {
		return Snapshot{}, fmt.Errorf("这个包不能导入：%s", strings.Join(dedup(plan.problems), "；"))
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("创建备份目录 %s: %w", m.Dir, err)
	}
	// 解压树放在备份目录里面：和归档同一个文件系统，改名才可能是原子的，
	// 也不需要在配置里再多开一个"临时目录"给人配错。
	staging := filepath.Join(m.Dir, unpackDirName, fmt.Sprintf("%d", time.Now().UnixNano()))
	defer func() {
		_ = os.RemoveAll(staging)
		_ = os.Remove(filepath.Join(m.Dir, unpackDirName))
	}()

	if err := m.unpackPackage(ctx, pkgPath, plan, staging, opts.OnUnpack); err != nil {
		return Snapshot{}, err
	}
	items, skipped, err := stagedItems(staging)
	if err != nil {
		return Snapshot{}, err
	}
	if len(items) == 0 {
		return Snapshot{}, fmt.Errorf("包已经解开了，但里面一个可打包的条目都没有，没做成快照")
	}
	note := plan.originNote(opts.Cluster)
	snap, err := m.packItems(ctx, opts.Cluster, LabelImported, items, dedup(append(plan.dropped, skipped...)), opts.OnPack)
	if err != nil {
		return Snapshot{}, err
	}
	// 把"这份快照来自哪个包"补进清单：它是外部来的，日后排查要靠这句话认出
	// 到底是面板打的还是别人传的。
	if mf, err := m.readManifest(snap.ID); err == nil {
		mf.Note = addNote(mf.Note, note)
		if err := m.writeManifest(mf); err != nil {
			return Snapshot{}, err
		}
		snap, err = m.Get(snap.ID)
		if err != nil {
			return Snapshot{}, err
		}
	} else {
		m.log().Warn("导入的快照已经打好，但清单读不回来，来历那句没能补上", "id", snap.ID, "err", err.Error())
	}
	return snap, nil
}

// ---- 读包目录（只读名字和大小） ----

// planPackage 读出包里的条目名单并判形状。
func (m *Manager) planPackage(pkgPath string) (*importPlan, error) {
	content, err := listPackage(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("读这个存档包（%s）: %w", pkgRef(pkgPath), err)
	}
	plan := &importPlan{
		mapping: map[string]mapped{},
		taken:   map[string]bool{},
		index:   map[string]string{},
		claim:   map[string]string{},
	}
	plan.judge(content)
	plan.pkgName = pkgRef(pkgPath)
	return plan, nil
}

// pkgRef 只取包的文件名进界面文案：完整路径里有 workDir，用户不需要看，
// 而报错里带绝对路径会让只读账号猜出这台机器的目录结构。
func pkgRef(pkgPath string) string { return filepath.Base(pkgPath) }

// listPackage 读出包里的条目名单，顺手把几个小索引文件的内容带出来。
//
// 它一次读完，绝不写盘：预览要在用户上传之后立刻给出结论，
// 而"先解开再看"等于让一个没验过的包往磁盘上写东西。
func listPackage(pkgPath string) (pkgContent, error) {
	content := pkgContent{index: map[string]string{}}
	f, err := os.Open(pkgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return content, fmt.Errorf("这个存档包已经不在了：上传的临时文件被删掉了（隔得久的上传会自己清理），重新上传一次")
		}
		return content, fmt.Errorf("打开存档包: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return content, fmt.Errorf("读存档包的信息: %w", err)
	}
	if st.IsDir() {
		return content, fmt.Errorf("%s 是一个目录而不是压缩包：要把整个房间目录压成一个 zip 或 tar.gz 再上传", st.Name())
	}
	content.archiveBytes = st.Size()
	if st.Size() > MaxImportArchiveBytes {
		content.overLimit = append(content.overLimit, fmt.Sprintf(
			"这个包本身有 %s，超过面板 %s 的上传上限，先确认上传的东西对不对",
			humanBytes(st.Size()), humanBytes(MaxImportArchiveBytes)))
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	head = head[:n]
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return content, fmt.Errorf("读存档包开头: %w", err)
	}
	kind, err := sniffPkgKind(head, st.Name())
	if err != nil {
		return content, err
	}
	content.kind = kind
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return content, fmt.Errorf("回到存档包开头: %w", err)
	}
	switch kind {
	case "zip":
		err = listZip(f, st.Size(), &content)
	case "tar.gz":
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			return content, fmt.Errorf("这个包标着 tar.gz，但 gzip 那一层读不开（多半是没传完就被截断了）: %w", gerr)
		}
		defer gz.Close()
		err = listTar(gz, &content)
		if err != nil && errors.Is(err, gzip.ErrHeader) {
			return content, fmt.Errorf("这个包标着 tar.gz，但 gzip 那一层读不开（多半是没传完就被截断了）")
		}
	case "tar":
		err = listTar(f, &content)
	}
	if err != nil {
		return content, err
	}
	switch kind {
	case "zip":
		content.kindLabel = "zip 压缩包"
	case "tar.gz":
		content.kindLabel = "tar.gz 压缩包"
	default:
		content.kindLabel = "tar 归档"
	}
	return content, nil
}

// sniffPkgKind 按包的实际字节认格式，不信文件名后缀。
//
// 后缀是人写的，字节是格式本身：只按后缀的话，一个改了名字的 .txt 会一路走到
// 解压那步才失败，而那时候临时文件已经落在磁盘上了。
func sniffPkgKind(head []byte, name string) (string, error) {
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return "tar.gz", nil
	}
	if len(head) >= 4 && head[0] == 'P' && head[1] == 'K' {
		switch {
		case head[2] == 3 && head[3] == 4:
			return "zip", nil
		case head[2] == 5 && head[3] == 6:
			return "", fmt.Errorf("这个 zip 是空的，里面一个文件都没有")
		default:
			return "", fmt.Errorf("%s 是 zip，但面板读不出它的目录区（分卷、加密或者带注释的包都不支持）；重新压一次，别加密", name)
		}
	}
	if len(head) >= 262 && string(head[257:262]) == "ustar" {
		return "tar", nil
	}
	return "", fmt.Errorf("认不出 %s 的格式：面板收 zip、tar.gz 和 tar 三种，判的是包开头那几个字节而不是文件名后缀", name)
}

func listZip(f *os.File, size int64, content *pkgContent) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("这个包标着 zip，但目录区读不开（多半是没传完就被截断，或者根本不是 zip）: %w", err)
	}
	for _, fh := range zr.File {
		if len(content.raw) > maxImportEntries {
			content.overLimit = append(content.overLimit, fmt.Sprintf(
				"包里的条目超过 %d 个，不是一个真实存档该有的量", maxImportEntries))
			return nil
		}
		mode := fh.Mode()
		if mode.IsDir() {
			content.raw = append(content.raw, pkgRaw{name: fh.Name, isDir: true})
			continue
		}
		if !mode.IsRegular() {
			content.special = append(content.special, fh.Name)
			continue
		}
		content.raw = append(content.raw, pkgRaw{name: fh.Name, size: int64(fh.UncompressedSize64)})
		if fh.UncompressedSize64 > maxImportEntryBytes {
			content.overLimit = append(content.overLimit, fmt.Sprintf(
				"包里的 %s 单个就有 %s，超过面板认的单个文件上限 %s",
				fh.Name, humanBytes(int64(fh.UncompressedSize64)), humanBytes(maxImportEntryBytes)))
		}
		if !indexBasenames[strings.ToLower(filepath.Base(strings.ReplaceAll(fh.Name, "\\", "/")))] {
			continue
		}
		if fh.UncompressedSize64 > maxImportIndexBytes {
			content.overLimit = append(content.overLimit, fmt.Sprintf(
				"包里的 %s 有 %s，不像引擎写的索引文件，面板没读它的内容",
				fh.Name, humanBytes(int64(fh.UncompressedSize64))))
			continue
		}
		rc, oerr := fh.Open()
		if oerr != nil {
			return fmt.Errorf("读包里的 %s: %w", fh.Name, oerr)
		}
		text, rerr := io.ReadAll(io.LimitReader(rc, maxImportIndexBytes+1))
		_ = rc.Close()
		if rerr != nil {
			return fmt.Errorf("读包里的 %s 的内容: %w", fh.Name, rerr)
		}
		if int64(len(text)) > maxImportIndexBytes {
			continue
		}
		content.index[normalizePkgName(fh.Name)] = string(dst.StripKleiHeader(text))
	}
	return nil
}

func listTar(r io.Reader, content *pkgContent) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读包里的条目名单: %w", err)
		}
		if len(content.raw) > maxImportEntries {
			content.overLimit = append(content.overLimit, fmt.Sprintf(
				"包里的条目超过 %d 个，不是一个真实存档该有的量", maxImportEntries))
			return nil
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			content.raw = append(content.raw, pkgRaw{name: hdr.Name, isDir: true})
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			content.special = append(content.special, hdr.Name)
			continue
		}
		content.raw = append(content.raw, pkgRaw{name: hdr.Name, size: hdr.Size})
		if hdr.Size > maxImportEntryBytes {
			content.overLimit = append(content.overLimit, fmt.Sprintf(
				"包里的 %s 单个就有 %s，超过面板认的单个文件上限 %s",
				hdr.Name, humanBytes(hdr.Size), humanBytes(maxImportEntryBytes)))
		}
		if !indexBasenames[strings.ToLower(filepath.Base(strings.ReplaceAll(hdr.Name, "\\", "/")))] {
			continue
		}
		if hdr.Size > maxImportIndexBytes {
			content.overLimit = append(content.overLimit, fmt.Sprintf(
				"包里的 %s 有 %s，不像引擎写的索引文件，面板没读它的内容",
				hdr.Name, humanBytes(hdr.Size)))
			continue
		}
		text, err := io.ReadAll(io.LimitReader(tr, maxImportIndexBytes+1))
		if err != nil {
			return fmt.Errorf("读包里的 %s 的内容: %w", hdr.Name, err)
		}
		if int64(len(text)) > maxImportIndexBytes {
			continue
		}
		content.index[normalizePkgName(hdr.Name)] = string(dst.StripKleiHeader(text))
	}
}

// normalizePkgName 把一个包内路径换成 judge 用的键（反斜杠换正斜杠、去掉开头的 ./）。
// 只做 cleanPkgPath 里那两步的"键"版本：判断脏路径还是得走 cleanPkgPath。
func normalizePkgName(name string) string {
	text := strings.ReplaceAll(name, "\\", "/")
	return strings.TrimPrefix(strings.TrimPrefix(text, "./"), "/")
}

// ---- 结构判断 ----

// cleanPkgPath 把归档里的一个路径变成可信的相对片段，返回拆开的各段。
//
// 反斜杠先换成斜杠：Windows 的压缩工具和手工改过的包两种分隔符都可能出现，
// 换完所有判断只看一种，不会漏。
// 拦掉绝对路径、盘符、"." 与 ".."、超深目录——这些都是"按它写文件就会写到
// 房间外面或者覆盖别的东西"的形状，一条都不放过。
func cleanPkgPath(name string) ([]string, error) {
	text := strings.ReplaceAll(name, "\\", "/")
	if text == "" || text == "." || text == "/" {
		return nil, fmt.Errorf("包里有空路径条目")
	}
	if strings.HasPrefix(text, "/") {
		return nil, fmt.Errorf("包里的 %q 是绝对路径", name)
	}
	segs := strings.Split(text, "/")
	if len(segs) > maxImportDepth {
		return nil, fmt.Errorf("包里的 %q 目录层数超过 %d 层", name, maxImportDepth)
	}
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			return nil, fmt.Errorf("包里的 %q 要跳到上层目录以外", name)
		}
		if strings.ContainsRune(s, ':') {
			return nil, fmt.Errorf("包里的 %q 带着盘符或冒号", name)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("包里有空路径条目")
	}
	return out, nil
}

// shardOf 认出一个世界目录名。引擎只认这两个字面值（改名会起不了服），
// 但用户从 Windows 拖出来的包经常是 master/caves：大小写按引擎的写法归一，
// 并在结论里说明归一过一次，别让人以为包里的名字本来就是这样。
func shardOf(seg string) (string, bool) {
	switch strings.ToLower(seg) {
	case "master":
		return dst.ShardMaster, true
	case "caves":
		return dst.ShardCaves, true
	}
	return "", false
}

// shardAt 找出这串路径里"世界目录 + save"那一段的下标；没有返回 -1。
// 只看第一处，因为 <世界>/save/ 里面再套一层 Master/save 是包被改过的形状，
// 归到外面那一层即可。
func shardAt(segs []string) int {
	for i := 0; i+1 < len(segs); i++ {
		if _, ok := shardOf(segs[i]); ok && segs[i+1] == "save" {
			return i
		}
	}
	return -1
}

// judge 是形状判断与校验的全部规则，预览与导入共用。
//
// 一次算完三类结论：problems（拦住，不给导入）、warnings（放行但要人知道）、
// dropped（刻意没取的东西，连原因）。分类必须在这里定死，
// 否则界面说"能导入"而任务里报"结构不对"，用户两头看不到原因。
func (p *importPlan) judge(content pkgContent) {
	p.kind = content.kind
	p.kindLabel = content.kindLabel
	p.archiveBytes = content.archiveBytes
	p.entryCount = len(content.raw)

	var bad int
	var badSample []string
	for _, r := range content.special {
		badSample = append(badSample, r)
	}
	if len(content.special) > 0 {
		p.problems = append(p.problems, fmt.Sprintf(
			"包里有 %d 个不是普通文件或目录的条目（%s）。饥荒的存档目录里不该有软链、硬链和设备文件，"+
				"照着这种包解压会写到预期外的地方；请用打包文件夹的命令重新打包（Linux 上 tar czf 存档.tar.gz 房间目录），别用带链接的中间目录",
			len(content.special), strings.Join(firstOf(badSample, 3), "、")))
	}
	for _, why := range content.overLimit {
		p.problems = append(p.problems, why)
	}

	var items []pkgItem
	for _, r := range content.raw {
		segs, err := cleanPkgPath(r.name)
		if err != nil {
			bad++
			if len(badSample) < 3 {
				badSample = append(badSample, r.name)
			}
			continue
		}
		items = append(items, pkgItem{segs: segs, rel: strings.Join(segs, "/"), isDir: r.isDir, size: r.size, name: r.name})
	}
	if bad > 0 {
		p.warnings = append(p.warnings, fmt.Sprintf(
			"包里有 %d 个条目的路径不正常（%s），面板不碰它们，只取认得出的那部分", bad, strings.Join(firstOf(badSample, 3), "、")))
	}
	p.items = items
	if len(items) == 0 {
		p.problems = append(p.problems, "包里没有任何路径正常、认得出的条目")
		return
	}
	if len(p.problems) > 0 {
		// 已经判定这个包不能用了，形状就别再往下判：
		// 一个解压比炸掉的包照样能认出"两个世界"，那句话只会把真正的结论盖掉。
		return
	}

	// 1) 找房间目录那一层：所有"世界/save/…"条目上面的公共前缀必须只有一个。
	root := ""
	roots := map[string]bool{}
	for _, it := range items {
		i := shardAt(it.segs)
		if i < 0 {
			continue
		}
		roots[strings.Join(it.segs[:i], "/")] = true
	}
	switch len(roots) {
	case 0:
		p.diagnoseNoShard(items)
		return
	case 1:
		for r := range roots {
			root = r
		}
	default:
		var names []string
		for r := range roots {
			names = append(names, orNone(r, "（包的最外层直接就是世界目录）"))
		}
		sort.Strings(names)
		p.problems = append(p.problems, fmt.Sprintf(
			"包里认出了 %d 个房间目录（%s），一次只能导入一个房间的世界；请把要导入的那个单独打包",
			len(names), strings.Join(names, "、")))
		return
	}
	p.root = root
	if root == "" {
		p.shape = "shard"
		p.shapeNote = "包的最外层就是世界目录（Master 或 Caves），面板按一个房间里的世界来认；" +
			"房间配置（cluster.ini 那几份）不在这份包里，导入后保持现在这台机器的设置。"
	} else {
		p.shape = "cluster"
		p.shapeNote = fmt.Sprintf("面板把包里的 %s 当成房间目录，取它下面的世界和房间配置。", root)
	}

	// 2) 逐条归类：世界目录里全取，房间根上只取名单里那几个文件，其余连原因丢掉。
	prefix := root
	if prefix != "" {
		prefix += "/"
	}
	shardSeen := map[string]*ImportShard{}
	for _, it := range items {
		rel, why := p.mapPath(it.segs, prefix, it.isDir)
		mk := it.rel + dirMark(it.isDir)
		if rel == "" {
			p.mapping[mk] = mapped{dropWhy: why}
			if why != "" {
				p.dropped = append(p.dropped, why)
			}
			continue
		}
		key := rel + dirMark(it.isDir)
		if from, clash := p.claim[key]; clash {
			// 两份内容落在快照里同一个位置上：这里必须把它降成"不取"，
			// 否则 mapping 说取、taken 里没有，解开那一步会写出预览时没登记的文件。
			p.mapping[mk] = mapped{dropWhy: fmt.Sprintf("%s：和 %s 落在快照的同一个位置", it.rel, from)}
			if from == it.rel {
				p.problems = append(p.problems, fmt.Sprintf(
					"包里 %s 这个名字出现了两次，面板没法判断该收哪一份；请重新打包", it.rel))
			} else {
				p.problems = append(p.problems, fmt.Sprintf(
					"包里的 %s 和 %s 在快照里是同一个位置（引擎只认 %s 这一种写法），面板没法两个都收；请把重复的那份删掉再打包",
					from, it.rel, rel))
			}
			continue
		}
		p.mapping[mk] = mapped{rel: rel}
		p.claim[key] = it.rel
		p.taken[key] = true
		p.keepCount++
		p.total += it.size
		if !it.isDir {
			p.files++
			if text, ok := content.index[it.rel]; ok {
				p.index[rel] = text
			}
		}
		first, _, _ := strings.Cut(rel, "/")
		sh, ok := shardOf(first)
		if !ok {
			continue
		}
		s := shardSeen[sh]
		if s == nil {
			s = &ImportShard{Shard: sh}
			shardSeen[sh] = s
		}
		if !it.isDir {
			s.FileCount++
			s.Bytes += it.size
		}
	}

	// 3) 按世界的目录逐个查引擎读档要的那几样。
	for _, sh := range []string{dst.ShardMaster, dst.ShardCaves} {
		s, ok := shardSeen[sh]
		if !ok {
			continue
		}
		p.checkShard(s)
	}
	for _, sh := range []string{dst.ShardMaster, dst.ShardCaves} {
		if s, ok := shardSeen[sh]; ok {
			p.shards = append(p.shards, *s)
		}
	}
	if len(p.shards) == 0 {
		p.problems = append(p.problems, fmt.Sprintf(
			"在 %s 这一层下面没找到 Master 或 Caves 世界目录，这个包里没有可导入的世界", orNone(root, "包的最外层")))
		return
	}
	if p.total > maxImportUncompressed {
		p.problems = append(p.problems, fmt.Sprintf(
			"认出来的内容解开有 %s，超过面板 %s 的上限，不像一个真实存档；先确认这个包是什么",
			humanBytes(p.total), humanBytes(maxImportUncompressed)))
	}
	if p.files == 0 {
		p.problems = append(p.problems, "认出了目录结构，但里面一个文件都没有")
	}

	// 4) 房间根上那些"恢复时会被保留"的配置，提前说清楚。
	p.noteConfigs()
}

// mapPath 决定一个条目取不取，以及取的话在快照里叫什么。
//
// isDir 必须传进来：黑名单里"引擎攒的旧日志副本"认的是 /server_log/ 这种带分隔符的形状，
// 而包里的目录条目写成 Master/backup/server_log（没有尾斜杠），不补这一刀就会
// 把日志目录本身当成"别的东西"收进来。同一个坑在 Collect 里踩过一次（见 archive.go 那段）。
func (p *importPlan) mapPath(segs []string, prefix string, isDir bool) (string, string) {
	joined := strings.Join(segs, "/")
	if prefix != "" && !strings.HasPrefix(joined, prefix) {
		// 公共前缀之外的东西：Mac 压缩带出来的 __MACOSX、用户自己塞的说明文件……
		return "", fmt.Sprintf("%s：不在认出的房间目录 %s 里", joined, orNone(p.root, "包的最外层"))
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(joined, prefix), "/")
	if rest == "" {
		return "", ""
	}
	parts := strings.Split(rest, "/")
	if sh, ok := shardOf(parts[0]); ok {
		// 世界目录里全取（save、leveldataoverride.lua、modoverrides.lua、server.ini…），
		// 凭据和日志这类由 skipRel 在归类这一步挡掉，理由写得更准。
		if parts[0] != sh {
			p.normalizeCase(joined, parts[0], sh)
		}
		rel := strings.Join(append([]string{sh}, parts[1:]...), "/")
		if why := skipRel(rel + dirMark(isDir)); why != "" {
			return "", fmt.Sprintf("%s：%s", joined, why)
		}
		return rel, ""
	}
	if len(parts) == 1 {
		for _, want := range clusterLevelFiles {
			if strings.EqualFold(parts[0], want) {
				if parts[0] != want {
					p.normalizeCase(joined, parts[0], want)
				}
				if why := skipRel(want); why != "" {
					return "", fmt.Sprintf("%s：%s", joined, why)
				}
				return want, ""
			}
		}
	}
	return "", fmt.Sprintf("%s：不是面板认得的世界目录，也不在房间配置名单（%s）里",
		joined, strings.Join(clusterLevelFiles, "、"))
}

func (p *importPlan) normalizeCase(name, from, to string) {
	p.warnings = append(p.warnings, fmt.Sprintf(
		"包里的 %s 用了 %s 这个名字，引擎只认 %s，快照里按 %s 登记", name, from, to, to))
}

// checkShard 查一个世界满不满足引擎的读档要求。
func (p *importPlan) checkShard(s *ImportShard) {
	idxPath := s.Shard + "/save/shardindex"
	legacyPath := s.Shard + "/save/saveindex"
	text, hasText := p.index[idxPath]
	switch {
	case hasText:
		s.IndexFile = "shardindex"
	case p.exists(idxPath):
		p.shardProblem(s, fmt.Sprintf(
			"%s 下面有 shardindex，但面板读不出它的内容（太大或者解不开），没法确认世界编号", s.Shard))
		return
	case p.exists(legacyPath):
		s.IndexFile = "saveindex"
		p.shardProblem(s, fmt.Sprintf(
			"%s 下面只有旧版 saveindex（客户端早期存档的写法），没有专用服务端用的 shardindex：导进去引擎认不出世界编号，会当成空房间重新生成一个世界",
			s.Shard))
		p.warnings = append(p.warnings, fmt.Sprintf(
			"要修这个包：先把这份存档放进一个房间、用专用服务端起一次服（引擎自己会写出 shardindex），再重新打包导入"))
		return
	default:
		s.IndexFile = ""
		p.shardProblem(s, fmt.Sprintf(
			"%s/save 下面没有 shardindex 这个文件：那是引擎进世界时认档的索引", s.Shard))
		p.warnings = append(p.warnings, "没有它服务端会把这个世界当成空的、重新生成一个"+
			"（shardindex.lua 里 dedicated 那一支把 session_id 留成 nil），"+
			"所以光把 save/session 拷进去是不够的。请连 save 目录一起打包，别只挑 session 里面的文件")
		return
	}

	id, err := sessionIDFromIndex(text)
	if err != nil {
		p.shardProblem(s, fmt.Sprintf("%s 的 shardindex 读不出世界编号：%s", s.Shard, err.Error()))
		return
	}
	if id == "" {
		p.shardProblem(s, fmt.Sprintf(
			"%s 的 shardindex 里 session_id 是空的：这份档还没真正存过一次世界", s.Shard))
		return
	}
	s.SessionID = id

	prefix := s.Shard + "/save/session/" + id + "/"
	files, slots, shapes := p.scanSession(prefix)
	if files == 0 {
		found := p.sessionIDsUnder(s.Shard + "/save/session/")
		msg := fmt.Sprintf(
			"%s 的索引写着世界编号 %s，但包里没有 save/session/%s 这一层（或者里面一个文件都没有）",
			s.Shard, id, id)
		if len(found) > 0 {
			// 编号是从包外面来的，人对"为什么对不上"毫无办法，除非面板把两边都念出来。
			// 只报索引那一边等于让用户去猜另一个名字。
			msg = fmt.Sprintf(
				"%s 的索引写着世界编号 %s，包里的 session 目录下却是 %s，两边对不上",
				s.Shard, id, strings.Join(found, "、"))
		}
		p.shardProblem(s, msg)
		p.warnings = append(p.warnings, "引擎按索引里的编号去找档，找不到就生成新世界；"+
			"这种对不上多半是打包时改过 session 里面的目录名，或者只拷了一部分。"+
			"请把 save/session 整层原样打包，别改里面那一层的目录名")
		return
	}
	s.WorldFiles = shapes
	s.Slots = slots
	s.Note = fmt.Sprintf("索引里的世界编号 %s，session 目录下 %d 个文件", id, files)
	if slots == 0 {
		p.warnings = append(p.warnings, fmt.Sprintf(
			"%s 的 session 目录里有 %d 个文件，但没认出常见的档位形状（slot-0001/ 或纯数字编号），面板照原样登记：%s",
			s.Shard, files, strings.Join(shapes, "、")))
	}
	// 模组配置单独说一句：导入等于把"这个房间开哪些模组"也换成了那份包里的名单，
	// 而模组文件本身不来自这个包（在 DST 的 mod 目录里），缺了要人去模组页补。
	if p.exists(s.Shard + "/modoverrides.lua") {
		s.HasModPrefs = true
		p.warnings = append(p.warnings, fmt.Sprintf(
			"%s 里带着模组配置（modoverrides.lua）：导入后这个房间按这份包里的模组名单开，"+
				"缺的模组要去模组页补齐，否则进世界会看到少了模组的内容", s.Shard))
	}
}

func (p *importPlan) shardProblem(s *ImportShard, msg string) {
	s.Problems = append(s.Problems, msg)
	p.problems = append(p.problems, msg)
}

// exists 问一个快照路径在不在被保留的条目里（目录两种写法都算）。
func (p *importPlan) exists(rel string) bool {
	return p.taken[rel] || p.taken[rel+"/"]
}

// scanSession 数一个 session 目录下的文件，并挑出常见的档位形状。
//
// 只按文件路径判断，不看包里的目录条目：zip 和 tar 都不保证带目录项
// （`zip -r` 会带，某些工具压出来的就没有），只认目录项的判断会在一种包上静默失配。
func (p *importPlan) scanSession(prefix string) (files, slots int, shapes []string) {
	top := map[string]bool{}
	slotSet := map[string]bool{}
	for rel := range p.taken {
		if strings.HasSuffix(rel, "/") || !strings.HasPrefix(rel, prefix) {
			continue
		}
		rest := strings.TrimPrefix(rel, prefix)
		if rest == "" {
			continue
		}
		files++
		first, tail, deeper := strings.Cut(rest, "/")
		if !deeper {
			top[first] = true
			continue
		}
		// session/<编号>/<这一段>/… —— 这一层是档位目录（slot-0001）或者
		// 专用服务端滚动编号档的临时层，都算"一个档"。
		slotSet[first] = true
		_ = tail
	}
	slots = len(slotSet)
	for name := range top {
		if numberedSaveRE.MatchString(name) || name == "server.dat" || strings.HasPrefix(name, "slot-") {
			shapes = append(shapes, name)
		}
	}
	for name := range slotSet {
		shapes = append(shapes, name+"/")
	}
	sort.Strings(shapes)
	if len(shapes) > 6 {
		shapes = shapes[:6]
	}
	return files, slots, shapes
}

// sessionIDsUnder 列出包里 <世界>/save/session/ 下面实际存在的世界编号目录名。
// 只在"索引指向的编号在包里没有"时用来把两边都念出来，判不判由调用方定。
func (p *importPlan) sessionIDsUnder(prefix string) []string {
	set := map[string]bool{}
	for rel := range p.taken {
		if !strings.HasPrefix(rel, prefix) {
			continue
		}
		id, _, _ := strings.Cut(strings.TrimPrefix(rel, prefix), "/")
		if id != "" && filepath.Base(id) == id {
			set[id] = true
		}
	}
	var out []string
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) > 3 {
		out = append(out[:3], "……")
	}
	return out
}

// sessionIDFromIndex 从 shardindex 的内容里取 session_id。
// 用的是面板本来就有的 Lua 数据解析器（绝不求值）。
func sessionIDFromIndex(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("文件是空的")
	}
	tbl, err := dst.ParseLuaTable(text)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(tbl.Get("session_id").AsString("")), nil
}

// diagnoseNoShard 在认不出世界目录时给出能照着做的下一步，而不是"格式不对"三个字。
//
// 这一句值得写这么长：上传一个"看起来对"的包然后被拒，用户唯一的线索就是这句话。
// 按包里到底有什么分四种情况，是因为四种包的修法完全不同。
func (p *importPlan) diagnoseNoShard(items []pkgItem) {
	var head []string
	seen := map[string]bool{}
	var hasSave, hasShardDir, looksLikeSaveInner bool
	for _, it := range items {
		for i, s := range it.segs {
			if s == "save" {
				hasSave = true
			}
			if s == "session" {
				looksLikeSaveInner = true
			}
			if _, ok := indexBasenames[strings.ToLower(s)]; ok && (i == 0 || it.segs[i-1] == "save") {
				looksLikeSaveInner = true
			}
			if _, ok := shardOf(s); ok {
				hasShardDir = true
			}
		}
		label := strings.Join(firstOf(it.segs, 3), "/")
		if it.isDir {
			label += "/"
		}
		if !seen[label] && len(head) < 5 {
			seen[label] = true
			head = append(head, label)
		}
	}
	list := strings.Join(head, "、")
	switch {
	case hasSave && hasShardDir:
		p.problems = append(p.problems, fmt.Sprintf(
			"包里既有 Master/Caves 也有 save 目录，但两者没套在一起（引擎要的形状是 房间/Master/save/session/世界编号/）。"+
				"包里的开头是 %s；多半是压的时候把两层分开选了，请从房间目录那层整个压", list))
	case hasShardDir:
		p.problems = append(p.problems, fmt.Sprintf(
			"包里有 Master 或 Caves 目录，可里面没有 save 这一层：那里面只有世界设定和模组配置的话，导进去没有任何存档可恢复。"+
				"包里的开头是 %s", list))
	case hasSave || looksLikeSaveInner:
		// 这一种最常见的成因就是"从 save 这一层开始压"：压的人已经钻到 Master/save 里面，
		// 上面的世界目录名和房间目录一起没了，面板只能看到 session/、shardindex 这些名字。
		// 认得出 session/ 才算得上这条：光要求有 save 段的话，"只压了 save 里面的东西"
		// 会一路掉到"认不出这是饥荒的包"，那句话谁也没法照着做。
		p.problems = append(p.problems, fmt.Sprintf(
			"包里的东西看着是从 save 这一层开始压的（认出了 session 或 shardindex 这些名字），"+
				"上面没有 Master 或 Caves 的名字。面板靠世界目录名认哪个是地表、哪个是洞穴，"+
				"请连 Master（或 Caves）目录本身一起压，更稳的是从房间目录那一层整个压。包里的开头是 %s", list))
	default:
		p.problems = append(p.problems, fmt.Sprintf(
			"认不出这是饥荒的存档包：没有找到 Master 或 Caves 世界目录。包里的开头是 %s；"+
				"要导入的是房间目录那一层（里面有 Master、Caves，通常还有 cluster.ini），压成 zip 或 tar.gz 都行", list))
	}
}

// noteConfigs 说清包里的房间配置在恢复时会怎么被对待。
//
// 恢复那一步（plan.dest）故意不覆盖已经存在的 cluster.ini 和 <世界>/server.ini：
// 端口和服务器名保持这台机器现在的样子。导入的包里那两份是别人机器的设置，
// 照抄过来最常见结果是两个房间抢同一个端口，界面看起来就像"恢复把服务器搞挂了"。
// 但这个结论必须提前说，不能等恢复完才说——不然用户第一反应是"导入没生效"。
func (p *importPlan) noteConfigs() {
	var kept, written []string
	for rel := range p.taken {
		if strings.HasSuffix(rel, "/") {
			continue
		}
		if rel == "cluster.ini" || strings.HasSuffix(rel, "/server.ini") && strings.Count(rel, "/") == 1 {
			kept = append(kept, rel)
			continue
		}
		if rel == "clusteroptions.ini" {
			written = append(written, rel)
		}
	}
	if len(kept) > 0 {
		sort.Strings(kept)
		p.warnings = append(p.warnings, fmt.Sprintf(
			"包里带着 %s。恢复存档时这些房间配置不会被覆盖，端口和服务器名跟着现在这台机器走；"+
				"要照这份包的设置开，去房间配置页自己改", strings.Join(kept, "、")))
	}
	if len(written) > 0 {
		sort.Strings(written)
		p.warnings = append(p.warnings, fmt.Sprintf(
			"包里的 %s 属于世界设置（人数上限、PVP 这些），恢复时会照这份包写进去", strings.Join(written, "、")))
	}
}

// preview 把结论变成给界面的那份数据。
func (p *importPlan) preview() ImportPreview {
	if p.shards == nil {
		p.shards = []ImportShard{}
	}
	return ImportPreview{
		Package:           p.pkgName,
		Kind:              p.kind,
		KindLabel:         p.kindLabel,
		ArchiveBytes:      p.archiveBytes,
		ArchiveLabel:      orDashBytes(p.archiveBytes),
		UncompressedBytes: p.total,
		UncompressedLabel: orDashBytes(p.total),
		EntryCount:        p.entryCount,
		FileCount:         p.files,
		Root:              p.root,
		Shape:             p.shape,
		ShapeNote:         p.shapeNote,
		Shards:            orEmptyArray(p.shards),
		Dropped:           orEmptyArray(dedup(p.dropped)),
		Warnings:          orEmptyArray(dedup(p.warnings)),
		Problems:          orEmptyArray(dedup(p.problems)),
		CanImport:         len(p.problems) == 0 && p.files > 0,
		Note:              p.note(),
	}
}

func (p *importPlan) note() string {
	if len(p.problems) > 0 {
		return "这个包先不能导入：" + strings.Join(dedup(p.problems), "；")
	}
	var worlds []string
	for _, s := range p.shards {
		worlds = append(worlds, fmt.Sprintf("%s（世界编号 %s，%d 个文件 %s）",
			s.Shard, s.SessionID, s.FileCount, humanBytes(s.Bytes)))
	}
	return fmt.Sprintf("认出来了：包里是 %d 个世界的存档——%s。导入会把它变成一份面板快照（%d 个文件、解压前 %s）；"+
		"要让房间真的用上这份档，还要再走一次恢复，那个世界的必须先停掉。",
		len(p.shards), strings.Join(worlds, "、"), p.files, humanBytes(p.total))
}

// originNote 写进导入那份快照的清单里。
func (p *importPlan) originNote(cluster string) string {
	var worlds []string
	for _, s := range p.shards {
		worlds = append(worlds, fmt.Sprintf("%s=%s", s.Shard, s.SessionID))
	}
	return fmt.Sprintf("外部导入的存档：来自包 %s，包里的房间目录是 %s，世界编号 %s，登记给房间 %s；"+
		"它不是这台机器打出来的档，出问题时的对照物是上传的那个包",
		p.pkgName, orNone(p.root, "包的最外层"), strings.Join(worlds, "、"), cluster)
}

// ---- 解开包（写盘只发生在临时解压树里） ----

// unpackPackage 把计划保留的条目写进解压树。
//
// 它必须和 judge 用同一份归类结论（plan.mapping），所以这里只认那张表：
// 表里没有的条目说明包在两次读取之间被换过，那种情况下继续写的就不是刚才预览过的东西了。
func (m *Manager) unpackPackage(ctx context.Context, pkgPath string, plan *importPlan, staging string,
	progress func(done, total int)) error {
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("建临时解压目录 %s: %w", filepath.Base(staging), err)
	}
	out := func(rel string) (string, error) {
		abs := filepath.Clean(filepath.Join(staging, filepath.FromSlash(rel)))
		root := filepath.Clean(staging)
		if abs == root || !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return "", fmt.Errorf("包里的 %q 会写到临时目录外面，已停止解压", rel)
		}
		return abs, nil
	}
	done := 0
	// written 是已经真的写到磁盘上的字节数。上限判断用它而不是 plan.total：
	// 后者是包里"声称"的大小，一个把大小字段全写成 1 字节的包能骗过声称值，
	// 而写盘这一路的字节是实测的。
	var written int64
	handle := func(raw pkgRaw, open func() (io.ReadCloser, error)) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		segs, err := cleanPkgPath(raw.name)
		if err != nil {
			return nil // judge 已经因为脏路径丢掉它了
		}
		rel := strings.Join(segs, "/")
		mappedKey := rel + dirMark(raw.isDir)
		dec, ok := plan.mapping[mappedKey]
		if !ok {
			return fmt.Errorf("包在预览之后又变了（读到了当时没有的条目 %q），已停止解压；重新上传一次再导入", pkgRef(raw.name))
		}
		if dec.rel == "" {
			return nil
		}
		done++
		if progress != nil {
			progress(done, plan.keepCount)
		}
		abs, err := out(dec.rel)
		if err != nil {
			return err
		}
		if raw.isDir {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return fmt.Errorf("建目录 %s: %w", dec.rel, err)
			}
			return nil
		}
		if raw.size > maxImportEntryBytes {
			return fmt.Errorf("包里的 %s 单个就有 %s，超过面板认的单个文件上限，已停止解压",
				dec.rel, humanBytes(raw.size))
		}
		// 上限问两遍：写之前按包里"声称"的大小拦（一个声称 1 字节的炸弹条目不会先落 40GB），
		// 写完之后按实测字节再拦一遍（声称值可以骗人，落在磁盘上的量不能）。
		if written+raw.size > maxImportUncompressed {
			return fmt.Errorf("解出来的内容将要超过面板 %s 的上限，卡在 %s 这一步，已停止解压；这个包不像一个真实存档",
				humanBytes(maxImportUncompressed), dec.rel)
		}
		rc, err := open()
		if err != nil {
			return fmt.Errorf("读包里的 %s: %w", dec.rel, err)
		}
		defer rc.Close()
		_, n, err := writeFile(abs, io.LimitReader(rc, maxImportEntryBytes+1), 0o644)
		if err != nil {
			return err
		}
		if n != raw.size {
			return fmt.Errorf("解出 %s 时读到 %d 字节，而包里登记的是 %d 字节：这个包是坏的（截断或改过），已停止解压",
				dec.rel, n, raw.size)
		}
		if written+n > maxImportUncompressed {
			return fmt.Errorf("解出来的内容到 %s 时已经超过面板 %s 的上限，已停止解压；这个包不像一个真实存档",
				dec.rel, humanBytes(maxImportUncompressed))
		}
		written += n
		return nil
	}

	f, err := os.Open(pkgPath)
	if err != nil {
		return fmt.Errorf("重新打开存档包: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("读存档包的信息: %w", err)
	}
	switch plan.kind {
	case "zip":
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return fmt.Errorf("重新读这个 zip 的目录区: %w", err)
		}
		for _, zf := range zr.File {
			fh := zf
			if err := handle(pkgRaw{name: fh.Name, size: int64(fh.UncompressedSize64), isDir: fh.Mode().IsDir()},
				func() (io.ReadCloser, error) { return fh.Open() }); err != nil {
				return err
			}
		}
	case "tar.gz", "tar":
		var r io.Reader = f
		if plan.kind == "tar.gz" {
			gz, err := gzip.NewReader(f)
			if err != nil {
				return fmt.Errorf("重新读这个包的 gzip 层: %w", err)
			}
			defer gz.Close()
			r = gz
		}
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("读包里的条目: %w", err)
			}
			if err := handle(pkgRaw{name: hdr.Name, size: hdr.Size, isDir: hdr.Typeflag == tar.TypeDir},
				func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("面板不认识这种包格式 %q", plan.kind)
	}
	if done != plan.keepCount {
		return fmt.Errorf("包里的条目和预览时读到的对不上（预览 %d 条，实际解出 %d 条），已停止解压；重新上传一次再导入",
			plan.keepCount, done)
	}
	return nil
}

// stagedItems 列出解压树里该进快照的条目。
//
// 形状和 Collect 一模一样（目录条目带尾斜杠、非普通文件跳掉、按路径排序），
// 这样 packItems 收到的两份输入可以完全同构。
// 这里不再问"备份目录本身"那一条：Collect 那条防的是快照目录配在房间里面时备份打进备份，
// 而解压树本身就是备份目录里的一个临时目录，遍历从它的根开始，撞不上自己。
func stagedItems(dir string) ([]srcFile, []string, error) {
	var items []srcFile
	var skipped []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("遍历解压出来的内容: %w", err)
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		whyRel := rel
		if d.IsDir() {
			whyRel = rel + "/"
		}
		if why := skipRel(whyRel); why != "" {
			skipped = append(skipped, rel+"："+why)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			items = append(items, srcFile{rel: rel + "/", abs: path})
			return nil
		}
		if !d.Type().IsRegular() {
			skipped = append(skipped, rel+"：不是普通文件（软链或设备），恢复时无处可放")
			return nil
		}
		items = append(items, srcFile{rel: rel, abs: path})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	sort.Strings(skipped)
	return items, skipped, nil
}

// ---- 小工具 ----

func dirMark(isDir bool) string {
	if isDir {
		return "/"
	}
	return ""
}

func orNone(v, alt string) string {
	if strings.TrimSpace(v) == "" {
		return alt
	}
	return v
}

// orDashBytes 给"还没算出来"的大小一个说法，不能报 0 B：
// 一个读不出大小的坏包被写成 0 B，用户会以为这是个空包，而真正的原因是包读不开。
func orDashBytes(n int64) string {
	if n <= 0 {
		return "读不出来"
	}
	return humanBytes(n)
}

// humanBytes 把字节数说成人能读的 MB/GB。导入的判断消息在这一层写成整句中文，
// 句子里的大小必须是这个格式——"140751872 字节"等于让人自己做除法。
// 单位和界面、任务中心那两份同名函数一致，三处不一致时同一个包在三个地方会是两个数。
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "0 B"
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/float64(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/float64(1<<10), 'f', 1, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

func dedup(list []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
