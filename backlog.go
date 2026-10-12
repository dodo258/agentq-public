package main

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Item struct {
	Text        string // 「- [ ] 」之后的原文
	Title       string // 「——」之前的部分，去掉进行中标记
	Fingerprint string
	InProgress  bool
	Stuck       bool
}

func (it Item) Eligible() bool { return !it.InProgress && !it.Stuck }

var inProgressRe = regexp.MustCompile(`（进行中[^）]*）`)

// 卡住标记只认 agent-task 写的两种形式，正文里提到这个词（如验收标准）不算：
//   - 括号标记：括号紧挨着「卡住」、后面接冒号、右括号或空白——（卡住：…）、(卡住)、【卡住】、[卡住 …]，位置不限；
//   - 行尾标记：去掉「（进行中…）」、行尾空白和句末标点后以「卡住」结尾，前面是行首、破折号（— 或 ——）或「；，。|」。
var (
	stuckBracketRe = regexp.MustCompile(`[（(【\[]\s*卡住(\s|[：:）)】\]]|$)`)
	stuckTailRe    = regexp.MustCompile(`(^|[—；;，,。|])\s*卡住$`)
)

// 等条件标记：「等主人拍板」里的条目写着 `（等条件：…）`，表示要等前置条件满足才轮到主人定（如「前面 7 条都合并」）。
// 只认括号紧挨「等条件」再接冒号的写法（全角或半角括号、冒号都行），正文里顺带提到这个词不算；条件由做完最后一条前置任务的 agent 去掉标记。
var waitCondRe = regexp.MustCompile(`[（(]\s*等条件\s*[：:]\s*([^）)]*)[）)]`)

// waitCondition 返回条目里等条件标记写的条件（去掉首尾空白）；没有标记返回 ok=false。
func waitCondition(text string) (cond string, ok bool) {
	m := waitCondRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

func isStuck(text string) bool {
	if stuckBracketRe.MatchString(text) {
		return true
	}
	t := strings.TrimSpace(inProgressRe.ReplaceAllString(text, ""))
	return stuckTailRe.MatchString(strings.TrimRight(t, " \t。.；;，,！!"))
}

// fingerprint = 条目原文去掉「（进行中…）」后的 sha1。
func fingerprint(line string) string {
	s := strings.TrimSpace(inProgressRe.ReplaceAllString(line, ""))
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// parseBacklog 只看「## 待做」到下一个「## 」之间、行首是「- [ ]」的行，跳过 HTML 注释。
func parseBacklog(text string) []Item {
	var items []Item
	eachBacklogLine(text, func(section, line string) {
		if !strings.HasPrefix(section, "待做") || !strings.HasPrefix(line, "- [ ]") {
			return
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))
		if text == "" {
			return
		}
		items = append(items, Item{
			Text:        text,
			Title:       itemTitle(text),
			Fingerprint: fingerprint(line),
			InProgress:  strings.Contains(line, "（进行中"),
			Stuck:       isStuck(text),
		})
	})
	return items
}

// doneCommitRe：agent-task 移到「已完成」时写的「合并提交 <号>」。
var doneCommitRe = regexp.MustCompile("合并提交\\s*[:：]?\\s*`?([0-9a-f]{7,40})(?:[^0-9A-Za-z_]|$)")

// doneMergeCommit 在「## 已完成」里找标题等于 title 的「- [x]」条目，返回第一条写的合并提交号；没有返回空。
func doneMergeCommit(text, title string) string {
	inDone := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "## ") {
			inDone = strings.HasPrefix(strings.TrimSpace(line[3:]), "已完成")
			continue
		}
		if !inDone || !(strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]")) {
			continue
		}
		t := strings.TrimSpace(line[len("- [x]"):])
		if itemTitle(t) != title {
			continue
		}
		if m := doneCommitRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

func itemTitle(text string) string {
	t := waitCondRe.ReplaceAllString(inProgressRe.ReplaceAllString(text, ""), "")
	if i := strings.Index(t, "——"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

type Project struct {
	Name string
	Dir  string
}

func (p Project) backlogPath() string { return filepath.Join(p.Dir, "docs", "BACKLOG.md") }

// items 读项目 BACKLOG；文件不存在当作没有条目。
func (p Project) items() ([]Item, error) {
	b, err := os.ReadFile(p.backlogPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseBacklog(string(b)), nil
}

// isGitMain：.git 必须是目录（worktree 和子模块的 .git 是文件）。
func isGitMain(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && fi.IsDir()
}

// scanProjects 列出 root 下一层、按名字排序的 git 主目录，跳过以 . 开头的目录。
func scanProjects(root string) ([]Project, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Project
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(root, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		if !isGitMain(dir) {
			continue
		}
		out = append(out, Project{Name: name, Dir: dir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
