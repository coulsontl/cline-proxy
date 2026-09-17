package app

import (
	"net/http"
	"os"
	"runtime"
	"time"
)

// Version 由构建时注入：
//
//	go build -ldflags "-X cline-go-proxy/internal/app.Version=$(git rev-parse --short HEAD)"
//
// 没注入时为 "dev"。排查线上问题时，先看 /admin/api/version 就能确认跑的是哪个构建，
// 不用靠"错误表里有没有某条日志"来推断部署是否生效。
var Version = "dev"

var processStart = time.Now()

// GET /admin/api/version
func handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	info := map[string]any{
		"version":   Version,
		"goVersion": runtime.Version(),
		"platform":  runtime.GOOS + "/" + runtime.GOARCH,
		"startedAt": processStart.Format(time.RFC3339),
		"uptime":    int64(time.Since(processStart).Seconds()),
	}
	// 二进制自身的 mtime/大小：即使 version 是 "dev"，也能一眼看出线上是不是刚部署的构建
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			info["binary"] = map[string]any{
				"name":    fi.Name(),
				"size":    fi.Size(),
				"builtAt": fi.ModTime().Format(time.RFC3339),
			}
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: info})
}