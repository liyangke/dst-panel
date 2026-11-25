package hoststats

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// 这一组测试钉的是"这一格到底把哪些进程算进来了"。
//
// 起因是 2026-11-04 用户看着总览页问的一句"内存 2 MB、CPU 0% 合理吗"：
// 界面拿的是 /api/v1/rooms 给的进程号，那个号是 screen 会话进程自己（云机实测 VmRSS 2024 kB），
// 真正在吃内存的饥荒服务端是它的子进程（同一台机器实测 882900 kB 与 1121888 kB）。
// 也就是说这一格一直是对的，只是它对的不是引擎。
//
// 平台读法（procSelf / procChildren）在这里当参数注入，所以这段加法在开发机上就测得到——
// 换成只测 host_unix.go 的写法，这台 Windows 上一个用例都跑不起来。

func fakeTree(nodes map[int]Proc) func(int) Proc {
	return func(pid int) Proc {
		if p, ok := nodes[pid]; ok {
			return p
		}
		return Proc{At: time.Now(), Err: errors.New("读不到进程 " + strconv.Itoa(pid))}
	}
}

func node(ticks, rss int64) Proc {
	return Proc{At: time.Now(), Ticks: uint64(ticks), RssBytes: rss}
}

func TestSumProcTreeCountsChildren(t *testing.T) {
	nodes := map[int]Proc{
		100: node(10, 2<<20),    // screen 会话进程自己：节拍几乎不动、内存只有几 MB
		101: node(900, 900<<20), // 引擎
		102: node(5, 1<<20),     // 引擎的某个子进程
	}
	kids := map[int][]int{100: {101}, 101: {102}}

	got := sumProcTree(100, fakeTree(nodes), func(pid int) []int { return kids[pid] })
	if got.Err != nil {
		t.Fatalf("不该有错误：%v", got.Err)
	}
	if want := int64(2<<20 + 900<<20 + 1<<20); got.RssBytes != want {
		t.Errorf("内存该把整棵树加起来，实际 %d，期望 %d", got.RssBytes, want)
	}
	if want := uint64(10 + 900 + 5); got.Ticks != want {
		t.Errorf("节拍该把整棵树加起来，实际 %d，期望 %d", got.Ticks, want)
	}
}

// 只报会话那一层是这个 bug 的原样：加一条"孩子必须真的进来"的钉子，
// 免得将来有人把 sumProcTree 换回 self(root)。
func TestSumProcTreeNotJustRoot(t *testing.T) {
	nodes := map[int]Proc{7: node(1, 2024*1024), 8: node(5000, 882900*1024)}
	kids := map[int][]int{7: {8}}
	got := sumProcTree(7, fakeTree(nodes), func(pid int) []int { return kids[pid] })
	if got.RssBytes <= nodes[7].RssBytes {
		t.Errorf("内存只数了会话进程自己（%d）：引擎那一层没加进来", got.RssBytes)
	}
	if got.Ticks <= nodes[7].Ticks {
		t.Errorf("节拍只数了会话进程自己（%d）：这一格会一直报 0%%", got.Ticks)
	}
}

func TestSumProcTreeSkipsUnreadableChild(t *testing.T) {
	nodes := map[int]Proc{1: node(10, 1000), 3: node(40, 4000)}
	kids := map[int][]int{1: {2, 3}} // 2 号刚退出，读不到
	got := sumProcTree(1, fakeTree(nodes), func(pid int) []int { return kids[pid] })
	if got.Err != nil {
		t.Fatalf("树里少一个子进程不该让整格变成读不到：%v", got.Err)
	}
	if got.RssBytes != 5000 || got.Ticks != 50 {
		t.Errorf("该是 1 号和 3 号相加，实际 %+v", got)
	}
}

func TestSumProcTreeStopsAtDepthCap(t *testing.T) {
	// 一条一直往下 fork 的链，深度超出上限时必须停，而不是走到天荒地老。
	nodes := map[int]Proc{}
	kids := map[int][]int{}
	for i := 0; i < procTreeDepth*4; i++ {
		pid := 1000 + i
		nodes[pid] = node(1, 1)
		kids[pid] = []int{pid + 1}
	}
	got := sumProcTree(1000, fakeTree(nodes), func(pid int) []int { return kids[pid] })
	if got.RssBytes > procTreeDepth+2 {
		t.Errorf("深度上限没起作用，一共加了 %d 层", got.RssBytes)
	}
}

func TestSumProcTreeLeafWhenNoChildren(t *testing.T) {
	nodes := map[int]Proc{5: node(3, 300)}
	got := sumProcTree(5, fakeTree(nodes), func(int) []int { return nil })
	if got.RssBytes != 300 || got.Ticks != 3 {
		t.Errorf("没有子进程时就该只报这一个，实际 %+v", got)
	}
}

func TestSumProcTreeRootUnreadable(t *testing.T) {
	got := sumProcTree(4242, fakeTree(map[int]Proc{}), func(int) []int { return nil })
	if got.Err == nil {
		t.Fatal("会话进程本身读不到时必须把那句说明传出去，不能报 0")
	}
}

func TestCleanCPUModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Intel(R) Core(TM) i7-9700 CPU @ 3.00GHz", "Intel Core i7-9700 CPU @ 3.00GHz"},
		{"Intel(R) Xeon(R) Platinum(R) 8255C", "Intel Xeon Platinum 8255C"},
		{"  AMD   EPYC 7543  ", "AMD EPYC 7543"},
	}
	for _, c := range cases {
		if got := cleanCPUModel(c.in); got != c.want {
			t.Errorf("cleanCPUModel(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
