// Package web 把 Vite 构建产物（web/dist）内嵌进 Go 二进制，让 Hify 最终
// 只需要分发一个可执行文件——这是 CLAUDE.md 里「打包成单个 Go 二进制
// （go:embed 内嵌前端静态资源）」那条约定的落地点。
//
// 为什么内嵌逻辑放在 web/ 而不是 internal/server/：go:embed 只能嵌入当前包
// 目录及其子目录，dist 在 web/ 下，所以嵌入点必须在这里。internal/server
// 只接收一个 fs.FS 接口，不关心资源从哪来，测试里可以塞 fstest.MapFS。
package web

import (
	"embed"
	"io/fs"
)

// all: 前缀让 embed 连 . 开头的文件一起收进来——仓库里提交了一个占位的
// web/dist/.gitkeep，因为 web/dist/ 是 gitignore 的构建产物，全新 clone 下
// 目录会是空的，而 go:embed 匹配不到任何文件时是编译错误而不是空 FS。
//
//go:embed all:dist
var distFS embed.FS

// Dist 返回以 dist 为根的前端资源。没有真实构建产物时（只有占位文件，
// 即开发者没跑过 npm run build）返回 nil，调用方据此跳过静态资源挂载——
// 开发期前端跑在 Vite dev server 上，本来就不该由 Go 来服务。
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
