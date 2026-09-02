package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"hify/internal/platform/apperr"
	"hify/internal/platform/httperr"
)

// mountSPA 把内嵌的前端构建产物挂到 NoRoute 上：命中真实文件就发文件，
// 命中不了就回 index.html 交给前端路由（React Router 的 /agents、
// /knowledge 这类路径在服务端并不存在）。assets 为 nil 表示没有构建产物
// （开发期前端跑 Vite dev server），此时只注册 API 的 404，不碰静态资源。
//
// 注意顺序：Gin 的 NoRoute 只在所有已注册路由都没匹配上时才执行，而
// /api/v1/* 是显式注册的，所以这里能安全地假设「以 /api/ 开头 = 访问了一个
// 不存在的接口」，必须回 JSON 404 而不是 index.html——否则前端 fetch 拿到
// 一坨 HTML 再去 JSON.parse，报错信息会完全指错方向。
func mountSPA(r *gin.Engine, assets fs.FS) {
	notFoundAPI := func(c *gin.Context) {
		httperr.Write(c, apperr.NotFound("route_not_found", "接口不存在"))
	}

	if assets == nil {
		r.NoRoute(notFoundAPI)
		return
	}

	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			notFoundAPI(c)
			return
		}
		// 静态资源只接受读方法；POST /whatever 不该拿到一个 200 的 index.html。
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			notFoundAPI(c)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+c.Request.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}

		if info, err := fs.Stat(assets, name); err == nil && !info.IsDir() {
			// Vite 给 assets/ 下的文件名带内容哈希，改一次内容换一个文件名，
			// 所以可以放心长缓存；其余文件（favicon 等）交给协商缓存。
			if strings.HasPrefix(name, "assets/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(c.Writer, c.Request, assets, name)
			return
		}

		// SPA fallback。index.html 必须不缓存，否则前端发版后用户会拿着旧的
		// index 去引用已经被删掉的哈希资源，页面白屏。
		c.Header("Cache-Control", "no-cache")
		http.ServeFileFS(c.Writer, c.Request, assets, "index.html")
	})
}
