package gui

import (
	"bytes"

	"github.com/yuin/goldmark"
)

// markdownToHTML 用 goldmark 把 markdown 渲染为 HTML（/view 展示，gui.md §4.2）。
// goldmark 是 kb 已引入的零依赖纯 Go 引擎，GUI 复用不新增前端依赖。
func markdownToHTML(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := goldmark.Convert(src, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
