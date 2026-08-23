// Package memory 提供 agent 的跨会话长期记忆：把磁盘上的 Markdown 记忆文件
// 变成可关键词检索的索引。记忆 = 磁盘文件（唯一事实来源，OpenClaw 同款理念），
// 本包做"扫描 → 分块 → 建索引 → 检索 → 受控追加"。写入双层：常驻层 MEMORY.md
// 人手维护；归档层 memory/*.md 可经 Append 受控追加——Append 路径锁死只写归档层，
// 模型生成的内容进"搜索池"而非"常驻层"，天然规避污染高频上下文。
//
// 边界约定：
//   - 叶子包：不 import 任何内部包（同 session 的定位），避免循环依赖；
//   - 文件布局：<dir>/MEMORY.md（常驻层，启动注入用）+ <dir>/memory/*.md（归档层，可检索）；
//   - 中文检索：bigram（二元组）分词 + 子串兜底，零依赖、免词表——中文无空格，
//     朴素分词失效，bigram 是"够用"的最小解（设计文档 §4）；
//   - 惰性刷新：每次查询前按文件 mtime 比对，无变化零成本，有变化全量重建
//     （语料小、毫秒级，无需增量索引）；Append 落盘后下一次查询自动可见；
//   - 错误分层：空参数/未命中属预期失败（工具层映射为业务失败回灌），
//     目录不可读等系统边界才返回 error 透传。
package memory

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Hit 一次检索的单条命中。
type Hit struct {
	// Ref 定位标识：文件相对路径 + 块序号，如 "memory/20260823.md#2"，供 Get 精读。
	Ref string
	// Score 相关度分，越大越相关（同分按 Ref 字典序，结果确定可复现）。
	Score float64
	// Snippet 块内命中片段（query 附近的上下文窗口），供模型快速判断相关性。
	Snippet string
}

// substringBoost 子串兜底的固定加分。"整串包含" 本身是强相关信号，
// 给它一个与 BM25 分数可比的小常数，覆盖 bigram 切分噪声的场景。
const substringBoost = 1.0

// chunk 索引最小单元：一块文本 + 其 bigram 词频（TF）。
type chunk struct {
	ref     string         // "文件相对路径#块号"
	content string         // 块原文
	freq    map[string]int // bigram term → 出现次数
}

// Store 记忆仓库：<dir>/MEMORY.md + <dir>/memory/*.md 的只读检索索引。
// 模块对象、不接口化（唯一实现，同 session.Store 先例）；
// 单进程 CLI，互斥锁保护索引重建，-race 干净。
type Store struct {
	dir    string
	mu     sync.Mutex
	chunks []chunk              // 索引快照：全部分块
	index  map[string][]int     // bigram term → 包含它的块下标
	files  map[string]time.Time // 上次扫描的文件 mtime 快照，惰性刷新比对用
}

// New 构造记忆仓库，目录不存在则自动创建（幂等）。
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("memory: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: create dir %q: %w", dir, err)
	}
	return &Store{dir: dir, files: make(map[string]time.Time)}, nil
}

// Search 关键词检索：bigram 倒排命中 ∪ 子串兜底，按分降序取前 limit。
// query 为空、limit<1 属参数错误（预期失败）；目录不可读属系统边界。
// 无命中返回空切片（len==0），不视为错误。
func (s *Store) Search(query string, limit int) ([]Hit, error) {
	if query == "" {
		return nil, errors.New("memory: empty query")
	}
	if limit < 1 {
		return nil, errors.New("memory: limit must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil, err
	}

	terms := bigrams(query)
	// 每个查询 term 的文档频次（出现在多少块），BM25 idf 用。
	df := make(map[string]int, len(terms))
	for _, t := range terms {
		df[t] = len(s.index[t])
	}
	n := len(s.chunks)
	scores := make(map[int]float64, len(terms)) // 块下标 → 分数
	for _, t := range terms {
		// 轻量 BM25 变体：tf × idf，省略文档长度归一化（语料同质，收益不成比例）。
		idf := math.Log((float64(n)-float64(df[t])+0.5)/(float64(df[t])+0.5) + 1)
		for _, ci := range s.index[t] {
			scores[ci] += float64(s.chunks[ci].freq[t]) * idf
		}
	}
	// 子串兜底：整串包含 query 的块补固定分（救回 bigram 切分不中的精确串，
	// 如标识符 memory_search / DEEPSEEK_API_KEY）。
	for ci, c := range s.chunks {
		if strings.Contains(c.content, query) {
			scores[ci] += substringBoost
		}
	}
	if len(scores) == 0 {
		return nil, nil
	}

	hits := make([]Hit, 0, len(scores))
	for ci, sc := range scores {
		hits = append(hits, Hit{
			Ref:     s.chunks[ci].ref,
			Score:   sc,
			Snippet: snippet(s.chunks[ci].content, query),
		})
	}
	// 排序确定性：分数降序，同分按 Ref 字典序——重复调用结果一致，便于测试与复现。
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Ref < hits[j].Ref
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// Get 按 Ref 读回整块原文（memory_get 精读用）。
// ref 为空或块不在当前索引（文件被删/改动）属预期失败；目录不可读属系统边界。
func (s *Store) Get(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("memory: empty ref")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return "", err
	}
	for _, c := range s.chunks {
		if c.ref == ref {
			return c.content, nil
		}
	}
	return "", fmt.Errorf("memory: chunk %q not found", ref)
}

// Append 把一段内容追加进归档层文件 <dir>/memory/<name>.md（不存在则创建）。
// 只写归档层是设计约束：常驻层 MEMORY.md 保持人手维护，本方法路径写死 memory/
// 子目录，调用方（工具层）无从写入常驻层。name 须为纯文件名（字母/数字/-/_），
// 扩展名由本方法补全——校验拦路径分隔符，防路径注入（同 session.validateID 惯例）。
func (s *Store) Append(name, content string) error {
	if name == "" {
		return errors.New("memory: empty name")
	}
	if strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) >= 0 {
		return fmt.Errorf("memory: invalid name %q", name)
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("memory: empty content")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.dir, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("memory: create archive dir %q: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("memory: open %q: %w", name, err)
	}
	defer f.Close()
	// 前置换行保证与旧内容分隔；写入后下一次 Search 按 mtime 刷新即可见（惰性索引天然解耦）。
	if _, err := f.WriteString("\n" + content + "\n"); err != nil {
		return fmt.Errorf("memory: append %q: %w", name, err)
	}
	return nil
}

// refresh 惰性重建索引：扫描文件集并比对 mtime，无变化跳过；有变化全量重建。
// 目录不可读等系统边界错误在此返回；单个文件读取失败跳过该文件（容错，尽力而为）。
func (s *Store) refresh() error {
	current, err := scanFiles(s.dir)
	if err != nil {
		return err
	}
	if len(current) == len(s.files) {
		same := true
		for path, m := range current {
			if s.files[path] != m {
				same = false
				break
			}
		}
		if same {
			return nil // 无变化：复用旧索引，零成本
		}
	}
	chunks, err := buildIndex(s.dir, current)
	if err != nil {
		return err
	}
	index := make(map[string][]int, len(chunks))
	for i, c := range chunks {
		for term := range c.freq {
			index[term] = append(index[term], i)
		}
	}
	s.chunks, s.index, s.files = chunks, index, current
	return nil
}

// scanFiles 返回"文件路径 → mtime"快照：MEMORY.md（存在才纳入）+ memory/*.md。
// 归档目录尚不存在不算错误（只有常驻层可检索）。
func scanFiles(dir string) (map[string]time.Time, error) {
	files := make(map[string]time.Time)
	memFile := filepath.Join(dir, "MEMORY.md")
	if fi, err := os.Stat(memFile); err == nil && !fi.IsDir() {
		files[memFile] = fi.ModTime()
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("memory: stat %q: %w", memFile, err)
	}

	sub := filepath.Join(dir, "memory")
	entries, err := os.ReadDir(sub)
	if err != nil {
		if os.IsNotExist(err) {
			return files, nil
		}
		return nil, fmt.Errorf("memory: read dir %q: %w", sub, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // 单文件 stat 失败（可能被并发删除）：尽力而为，跳过
		}
		files[filepath.Join(sub, e.Name())] = info.ModTime()
	}
	return files, nil
}

// buildIndex 读取全部文件并按标题切块，构造块列表。
// 单个文件读取失败跳过（容错）；整目录不可读的错误已在 scanFiles 抛出。
func buildIndex(dir string, files map[string]time.Time) ([]chunk, error) {
	var chunks []chunk
	for path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // 单文件读取失败：跳过该文件，不阻塞其余检索
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel) // Ref 统一用斜杠，跨平台稳定
		for i, text := range splitChunks(string(data)) {
			chunks = append(chunks, chunk{
				ref:     fmt.Sprintf("%s#%d", rel, i),
				content: text,
				freq:    countTerms(bigrams(text)),
			})
		}
	}
	return chunks, nil
}

// splitChunks 按 "## " 标题行切块：标题行归属其后的块，首个标题前的头部为块 0。
// 空块（无内容）跳过，避免索引纯噪声。
func splitChunks(text string) []string {
	var blocks []string
	var cur []string
	flush := func() {
		if trimmed := strings.TrimSpace(strings.Join(cur, "\n")); trimmed != "" {
			blocks = append(blocks, trimmed)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "## ") {
			flush()
			cur = []string{line}
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return blocks
}

// bigrams 把文本切成相邻"有效字符"的二元组集合：空白与标点视为分隔符跳过，
// 只在相邻字母/数字/汉字之间生成 term。查询与文档两侧切分规则一致，
// 故 bigram 是否含噪声不影响命中正确性；跳过空白让"我们 决定"与"我们决定"同义。
func bigrams(text string) []string {
	var runes []rune
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			runes = append(runes, r)
		}
	}
	terms := make([]string, 0, len(runes))
	for i := 0; i+1 < len(runes); i++ {
		terms = append(terms, string(runes[i:i+2]))
	}
	return terms
}

// countTerms 统计 bigram term 的出现次数（TF）。
func countTerms(terms []string) map[string]int {
	freq := make(map[string]int, len(terms))
	for _, t := range terms {
		freq[t]++
	}
	return freq
}

// snippetRadius 命中片段上下文窗口半径（字符）。
const snippetRadius = 60

// snippet 取块内 query 首次出现位置附近窗口；找不到则取块开头。
func snippet(content, query string) string {
	runes := []rune(content)
	pos := 0
	if loc := strings.Index(content, query); loc >= 0 {
		pos = utf8.RuneCountInString(content[:loc]) - snippetRadius
		if pos < 0 {
			pos = 0
		}
	}
	end := pos + snippetRadius*2 + utf8.RuneCountInString(query)
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[pos:end])
}
