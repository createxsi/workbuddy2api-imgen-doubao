// update.go 版本检测与更新接口：状态查询、手动检查、一键安装。
package panel

import (
	"context"
	"net/http"
	"time"
)

// updateSnapshot overview 内嵌的更新状态（未接入更新器时为 nil）。
func (p *Panel) updateSnapshot() any {
	if p.cfg.Updater == nil {
		return nil
	}
	return p.cfg.Updater.Status()
}

func (p *Panel) updateStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update api not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Updater.Status())
}

func (p *Panel) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update api not available")
		return
	}
	if err := p.cfg.Updater.Check(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Updater.Status())
}

// installUpdate 下载校验并暂存新版本，回 202 后触发优雅重启（由更新进程完成替换）。
// 不支持原地更新的环境由 Install 拒绝并返回原因。
func (p *Panel) installUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update api not available")
		return
	}
	if err := p.cfg.Updater.Install(context.WithoutCancel(r.Context())); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":         true,
		"restarting": true,
		"status":     p.cfg.Updater.Status(),
	})
	p.cfg.Updater.RestartAfter(800 * time.Millisecond)
}
