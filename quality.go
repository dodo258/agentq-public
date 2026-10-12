package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 一次满意率和返工率（docs/VISION.md「衡量」）。只认固定写法，判断不了的不计：
//   - 合并的条目：「## 已完成」里写了「合并提交 <号>」或「合并，提交 <号>」的「- [x]」，合并时间取那个提交在项目仓库里的提交时间；
//   - 验收意见：任何条目里写着「验收 <原条目标题> 的意见」（dispatch 技能的写法）；
//   - 返工：条目是 7 天内刚合并条目的验收意见，或条目的需求原文/它的合并提交说明里引用了 7 天内合并、不是本条自己分支上的提交。
const qualityWindow = 7 * 24 * time.Hour

var (
	mergedCommitRe = regexp.MustCompile("合并(?:提交|[，,]\\s*提交)[\\s:：`*]*([0-9a-f]{7,40})(?:[^0-9A-Za-z_]|$)")
	// 不带引号的写法要求「验收」后面有空白：「验收后按审查人的意见改」这类正文不算
	feedbackRe = regexp.MustCompile(`验收(?:[ \t]*「([^」]+)」|[ \t]+([^\s：:；;。，,「」][^；;。「」]*?))[ \t]*的意见`)
	hashRe     = regexp.MustCompile(`(?:^|[^0-9A-Za-z_])([0-9a-f]{7,40})(?:[^0-9A-Za-z_]|$)`)
	digitRe    = regexp.MustCompile(`[0-9]`)
)

// lastMergedCommit 取条目里最后一处「合并提交 <号>」的位置（FindStringSubmatchIndex 的格式）：
// 合并说明写在需求后面，需求里顺带写的「修上次合并提交 X 引入的…」不算本条的合并提交。
func lastMergedCommit(text string) []int {
	all := mergedCommitRe.FindAllStringSubmatchIndex(text, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// backlogEntry 是 BACKLOG 里的一条（任何一节、勾没勾都算），HTML 注释里的不算。
type backlogEntry struct {
	Section string // 「## 」后面的节名
	Done    bool   // 「- [x]」
	Text    string // 「- [ ] 」「- [x] 」之后的原文
	Title   string
}

// eachBacklogLine 逐行给出 BACKLOG 里 HTML 注释之外的行和它所在的节名（标题行本身也给出）。
func eachBacklogLine(text string, f func(section, line string)) {
	section, inComment := "", false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if inComment {
			if strings.Contains(line, "-->") {
				inComment = false
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!--") {
			if !strings.Contains(trimmed[4:], "-->") {
				inComment = true
			}
			continue
		}
		if start := strings.Index(line, "<!--"); start >= 0 && !strings.Contains(line[start:], "-->") {
			inComment = true // 行中间开始、跨行的注释：这一行本身照常处理
		}
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line[3:])
		}
		f(section, line)
	}
}

func backlogEntries(text string) []backlogEntry {
	var out []backlogEntry
	eachBacklogLine(text, func(section, line string) {
		var done bool
		switch {
		case strings.HasPrefix(line, "- [ ]"):
		case strings.HasPrefix(line, "- [x]"), strings.HasPrefix(line, "- [X]"):
			done = true
		default:
			return
		}
		t := strings.TrimSpace(line[len("- [ ]"):])
		if t == "" {
			return
		}
		out = append(out, backlogEntry{Section: section, Done: done, Text: t, Title: itemTitle(t)})
	})
	return out
}

// feedbackTargets 取条目里「验收 <标题> 的意见」的标题；模板占位（<…>）不算。
func feedbackTargets(text string) []string {
	var out []string
	for _, m := range feedbackRe.FindAllStringSubmatch(text, -1) {
		x := strings.TrimSpace(m[1] + m[2])
		if x == "" || strings.HasPrefix(x, "<") && strings.HasSuffix(x, ">") {
			continue
		}
		out = append(out, x)
	}
	return out
}

var normReplacer = strings.NewReplacer("`", "", "*", "", "「", "", "」", "", "“", "", "”", "", "\"", "", "'", "")

func normTitle(s string) string {
	return strings.ToLower(normReplacer.Replace(strings.Join(strings.Fields(s), "")))
}

// matchFeedback 找「验收 x 的意见」说的是「已完成」里哪一条：标题相同的优先；没有相同的，再找标题包含 x（至少 4 个字）、
// 本身不是验收意见的条目。返回候选的下标，self 那条不算；一个都没有或不止一个，调用方按「对不上」「判断不了」处理。
func matchFeedback(entries []backlogEntry, self int, x string) []int {
	nx := normTitle(x)
	var exact, part []int
	for i, e := range entries {
		if i == self || !e.Done || !strings.HasPrefix(e.Section, "已完成") {
			continue
		}
		nt := normTitle(e.Title)
		switch {
		case nt == nx:
			exact = append(exact, i)
		case len(feedbackTargets(e.Title)) == 0 &&
			utf8.RuneCountInString(nx) >= 4 && strings.Contains(nt, nx):
			part = append(part, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return part
}

// gitInfo 是一个提交的时间和说明；OK=false 表示在仓库里找不到（别的仓库的号、改写历史前的旧号、写错的号）。
type gitInfo struct {
	Full    string
	Time    time.Time
	Subject string
	Body    string
	OK      bool
}

type gitRepo struct {
	dir     string
	commits map[string]gitInfo
}

func (g *gitRepo) commit(h string) gitInfo {
	if c, ok := g.commits[h]; ok {
		return c
	}
	var c gitInfo
	out, err := exec.Command("git", "-C", g.dir, "show", "-s", "--format=%H%x00%ct%x00%s%x00%B", h+"^{commit}").Output()
	if f := strings.SplitN(string(out), "\x00", 4); err == nil && len(f) == 4 {
		if sec, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			c = gitInfo{Full: f[0], Time: time.Unix(sec, 0), Subject: f[2], Body: f[3], OK: true}
		}
	}
	g.commits[h] = c
	return c
}

// own 返回合并提交 m 自己分支上的提交：从 m 沿第一父提交往回走，到第一个「待办：」开头的提交（认领、记账这类只在 main 上的提交）为止。
// 本地合并时 m 是分支末端，往回是分支上的提交，直到认领提交；GitHub 压缩合并时 m 的父提交就是 main 上的，只剩 m 自己。
func (g *gitRepo) own(m string) map[string]bool {
	set := map[string]bool{}
	out, err := exec.Command("git", "-C", g.dir, "log", "--first-parent", "-n", "50", "--format=%H%x00%s", m).Output()
	if err != nil {
		return set
	}
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		h, subj, _ := strings.Cut(line, "\x00")
		if i > 0 && strings.HasPrefix(subj, "待办：") {
			break
		}
		set[h] = true
	}
	return set
}

// 一条合并的条目算出来的结果。Satisfied/Rework：1 是、0 否、-1 判断不了。
type qualityItem struct {
	Title      string
	Commit     string
	MergedAt   time.Time
	Satisfied  int
	SatisfyWhy string
	Rework     int
	ReworkWhy  string
}

type projectQuality struct {
	Name      string
	Items     []qualityItem
	NoTime    []string // 写了合并提交、但仓库里找不到那个提交的条目（不计）
	Unmatched []string // 对不上任何「已完成」条目的验收意见
	Feedback  int      // BACKLOG 里「验收 X 的意见」的总处数（对上、对不上都算）
	Err       string
}

type qualityReport struct {
	From, To time.Time
	Projects []projectQuality
}

// countTri 返回（是，否，判断不了）的条数。
func countTri(items []qualityItem, get func(qualityItem) int) (yes, no, unknown int) {
	for _, it := range items {
		switch get(it) {
		case 1:
			yes++
		case 0:
			no++
		default:
			unknown++
		}
	}
	return
}

func satisfiedOf(it qualityItem) int { return it.Satisfied }
func reworkOf(it qualityItem) int    { return it.Rework }

func rateText(yes, no, unknown int) string {
	s := "—"
	if n := yes + no; n > 0 {
		s = fmt.Sprintf("%.0f%%（%d/%d）", float64(yes)*100/float64(n), yes, n)
	}
	if unknown > 0 {
		s += fmt.Sprintf("，判断不了 %d 条不计", unknown)
	}
	return s
}

// statsText：「一次满意率 92%（12/13）；返工率 15%（2/13），判断不了 1 条不计」。
func statsText(items []qualityItem) string {
	if len(items) == 0 {
		return "最近 7 天没有合并的条目"
	}
	return "一次满意率 " + rateText(countTri(items, satisfiedOf)) + "；返工率 " + rateText(countTri(items, reworkOf))
}

func (r qualityReport) all() []qualityItem {
	var out []qualityItem
	for _, p := range r.Projects {
		out = append(out, p.Items...)
	}
	return out
}

// headline 是汇总和早报用的一行。
// 末尾带上验收意见的总处数：一条意见都没有时满意率 100% 只说明还没有意见数据。
func (r qualityReport) headline() string {
	n := 0
	for _, p := range r.Projects {
		n += p.Feedback
	}
	return fmt.Sprintf("最近 7 天（%s 到 %s）合并 %d 条：%s；各项目 BACKLOG 里的验收意见共 %d 处", whenCN(r.From),
		whenCN(r.To), len(r.all()), statsText(r.all()), n)
}

// quality 扫 projects_root 下各项目的 BACKLOG 和 git 历史，算 now 之前 7 天的一次满意率和返工率。
func (a *App) quality(now time.Time) qualityReport {
	r := qualityReport{From: now.Add(-qualityWindow), To: now}
	projects, err := scanProjects(a.projectsRoot())
	if err != nil {
		a.logf("警告：一次满意率和返工率：列项目失败：%v", err)
		return r
	}
	for _, p := range projects {
		raw, err := os.ReadFile(p.backlogPath())
		if os.IsNotExist(err) {
			continue
		}
		pq := projectQuality{Name: p.Name}
		if err != nil {
			pq.Err = err.Error()
		} else {
			pq = projectQualityOf(p.Name, string(raw), &gitRepo{dir: p.Dir, commits: map[string]gitInfo{}}, r.From, now)
		}
		r.Projects = append(r.Projects, pq)
	}
	return r
}

func projectQualityOf(name, backlog string, g *gitRepo, from, to time.Time) projectQuality {
	pq := projectQuality{Name: name}
	entries := backlogEntries(backlog)

	// 每条已完成条目的合并提交
	merged := map[int]gitInfo{}
	for i, e := range entries {
		if !e.Done || !strings.HasPrefix(e.Section, "已完成") {
			continue
		}
		if m := lastMergedCommit(e.Text); m != nil {
			h := e.Text[m[2]:m[3]]
			c := g.commit(h)
			if !c.OK {
				pq.NoTime = append(pq.NoTime, fmt.Sprintf("「%s」合并提交 %s", truncRunes(e.Title, 40), h))
				continue
			}
			merged[i] = c
		}
	}

	// 验收意见指向哪一条：feedback[目标下标] = 提意见的条目下标；ambiguous[候选下标] = 说不清是哪条的意见
	feedback := map[int][]int{}
	ambiguous := map[int][]int{}
	refs := map[int][][]int{} // 每条的意见各自的候选
	for i, e := range entries {
		for _, x := range feedbackTargets(e.Text) {
			c := matchFeedback(entries, i, x)
			refs[i] = append(refs[i], c)
			pq.Feedback++
			switch len(c) {
			case 0:
				pq.Unmatched = append(pq.Unmatched, fmt.Sprintf("「%s」里的「验收 %s 的意见」", truncRunes(e.Title, 30), x))
			case 1:
				feedback[c[0]] = append(feedback[c[0]], i)
			default:
				for _, k := range c {
					ambiguous[k] = append(ambiguous[k], i)
				}
			}
		}
	}

	var idx []int
	for i, c := range merged {
		if !c.Time.After(from) || c.Time.After(to) {
			continue
		}
		idx = append(idx, i)
	}
	sort.Slice(idx, func(x, y int) bool { return merged[idx[x]].Time.After(merged[idx[y]].Time) })

	for _, i := range idx {
		e, c := entries[i], merged[i]
		it := qualityItem{Title: e.Title, Commit: c.Full[:7], MergedAt: c.Time}

		switch {
		case len(feedback[i]) > 0:
			it.Satisfied = 0
			var ts []string
			for _, k := range feedback[i] {
				ts = append(ts, "「"+truncRunes(entries[k].Title, 30)+"」")
			}
			it.SatisfyWhy = "有验收意见：" + strings.Join(ts, "、")
		case len(ambiguous[i]) > 0:
			it.Satisfied = -1
			it.SatisfyWhy = fmt.Sprintf("「%s」的验收意见对得上不止一条，判断不了", truncRunes(entries[ambiguous[i][0]].Title, 30))
		default:
			it.Satisfied = 1
			it.SatisfyWhy = "没有验收意见"
		}

		it.Rework, it.ReworkWhy = reworkVerdict(entries, i, c, refs[i], merged, g)
		pq.Items = append(pq.Items, it)
	}
	return pq
}

// reworkVerdict 判断合并的条目 i 是不是返工：1 是、0 否、-1 判断不了，附依据。
func reworkVerdict(entries []backlogEntry, i int, c gitInfo, refs [][]int, merged map[int]gitInfo, g *gitRepo) (int, string) {
	var yes, unknown []string
	recent := func(t time.Time) bool { return t.Before(c.Time) && !t.Before(c.Time.Add(-qualityWindow)) }

	// ① 是 7 天内刚合并条目的验收意见
	for _, cand := range refs {
		if len(cand) != 1 {
			unknown = append(unknown, "验收意见对不上唯一一条")
			continue
		}
		o, ok := merged[cand[0]]
		switch {
		case !ok:
			unknown = append(unknown, fmt.Sprintf("意见对应的「%s」查不到合并时间", truncRunes(entries[cand[0]].Title, 30)))
		case recent(o.Time):
			yes = append(yes, fmt.Sprintf("是「%s」（%s 合并）的验收意见", truncRunes(entries[cand[0]].Title, 30), whenCN(o.Time)))
		}
	}

	// ② 条目原文（合并提交号之前的部分：标题和需求；后面是合并和验收说明，引用的多是本条自己的提交）和合并提交说明里
	// 引用的提交：7 天内合并、不是本条自己分支上的
	text := entries[i].Text
	if m := lastMergedCommit(text); m != nil {
		text = text[:m[0]]
	}
	var own map[string]bool
	seen := map[string]bool{}
	for _, src := range []string{text, c.Body} {
		for _, m := range hashRe.FindAllStringSubmatch(src, -1) {
			h := m[1]
			// 没有数字的多半是英文单词；只有数字的只认 7 位（git 默认的短号长度），更长的多是 CI 运行号、日期
			if seen[h] || !digitRe.MatchString(h) || len(h) > 7 && !strings.ContainsAny(h, "abcdef") || strings.HasPrefix(c.Full, h) {
				continue
			}
			seen[h] = true
			hc := g.commit(h)
			if !hc.OK {
				unknown = append(unknown, "引用的 "+h+" 在仓库里找不到")
				continue
			}
			if !recent(hc.Time) {
				continue
			}
			if own == nil {
				own = g.own(c.Full)
			}
			if own[hc.Full] {
				continue
			}
			what := hc.Subject // 引用的是别的条目的合并提交时，写那一条的标题
			for k, mc := range merged {
				if mc.Full == hc.Full {
					what = entries[k].Title
				}
			}
			yes = append(yes, fmt.Sprintf("引用了 %s（%s「%s」）", h, whenCN(hc.Time), truncRunes(what, 30)))
		}
	}
	switch {
	case len(yes) > 0:
		return 1, strings.Join(yes, "；")
	case len(unknown) > 0:
		return -1, strings.Join(unknown, "；")
	}
	return 0, "没有引用 7 天内合并的改动"
}

// qualitySection 是当天汇总末尾小结里的一节；没有项目时返回空。
func (a *App) qualitySection(now time.Time) string {
	r := a.quality(now)
	if len(r.Projects) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 一次满意率和返工率\n\n")
	fmt.Fprintf(&b, "- %s\n", r.headline())
	for _, p := range r.Projects {
		if len(p.Items) == 0 && p.Err == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s：%s\n", p.Name, projectLine(p))
	}
	b.WriteString("- 每条的依据：终端跑 `agentq quality`\n")
	return b.String()
}

func projectLine(p projectQuality) string {
	if p.Err != "" {
		return "BACKLOG 读不了：" + p.Err
	}
	s := fmt.Sprintf("合并 %d 条，%s", len(p.Items), statsText(p.Items))
	if len(p.NoTime) > 0 {
		s += fmt.Sprintf("；另有 %d 条合并提交在仓库里找不到，不计", len(p.NoTime))
	}
	return s
}

// cmdQuality 打印最近 7 天的一次满意率、返工率和每条的依据。
func (a *App) cmdQuality() error {
	r := a.quality(a.Now())
	fmt.Fprintln(a.Out, r.headline())
	for _, p := range r.Projects {
		if len(p.Items) == 0 && len(p.NoTime) == 0 && len(p.Unmatched) == 0 && p.Err == "" {
			continue
		}
		fmt.Fprintf(a.Out, "\n%s：%s\n", p.Name, projectLine(p))
		for _, it := range p.Items {
			fmt.Fprintf(a.Out, "  - %s %s「%s」\n      一次满意：%s（%s）\n      返工：%s（%s）\n", it.Commit, whenCN(it.MergedAt),
				truncRunes(it.Title, 50), triLabel(it.Satisfied), it.SatisfyWhy, triLabel(it.Rework), it.ReworkWhy)
		}
		for _, s := range p.NoTime {
			fmt.Fprintf(a.Out, "  - 不计（提交在仓库里找不到，可能是改写历史前的旧号）：%s\n", s)
		}
		for _, s := range p.Unmatched {
			fmt.Fprintf(a.Out, "  - 对不上条目的验收意见：%s\n", s)
		}
	}
	return nil
}

func triLabel(v int) string {
	switch v {
	case 1:
		return "是"
	case 0:
		return "否"
	}
	return "判断不了"
}
