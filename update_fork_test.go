package main

// 二开版本「屏蔽程序自身更新」（update.go 的 SelfUpdateDisabled）的单测。
//
// 三件事必须成立，否则上游一发版就可能把二开的 exe 顶掉：
//  1. 开关为真时一个发布源请求都不发（不是"发了但不应用"）；
//  2. /api/status 不对外宣称有新版——前端横幅/胶囊/待重启提示全靠它；
//  3. 能替换程序本体的三条路（整包下载运行、engine 分块交换、待重启接力）
//     全部早退。
//
// 开关改回 false（恢复上游行为）后这些用例自动跳过——它们锁的是二开语义，
// 不是上游语义。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const forkSkipReason = "SelfUpdateDisabled 已改回 false（恢复上游行为），二开用例不适用"

// ① 不发更新检查请求：checkUpdateOnce 是发布源那条路的唯一入口（启动首查、
// 每小时轮询、手动检查都走它），这里直接喂一个会计数的发布源。
func TestForkSelfUpdateDisabledNoReleaseRequest(t *testing.T) {
	if !SelfUpdateDisabled {
		t.Skip(forkSkipReason)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"20991231-9","assets":[]}`))
	}))
	defer srv.Close()

	a := &App{Cfg: &Config{DataDir: t.TempDir(), UpdateURL: srv.URL}}
	a.checkUpdateOnce(srv.URL)
	// 技能包那条（autoUpdateMarketSkills）是异步 goroutine、打的是 marketURL，
	// 与这个发布源无关；给它跑完的时间，计数必须仍是 0
	time.Sleep(300 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("二开版本不该发出任何更新检查请求，实测打了 %d 次发布源", n)
	}
	if snap := updateSnapshot(); snap.Available {
		t.Fatalf("内部状态也不该出现「有新版本」: %+v", snap)
	}
}

// ② /api/status 口径：即使内部状态被别的路径写成「有新版 + 待重启」（比如升级前
// 留下的 data/updates/state.json），对外也必须恒为「没有可更新版本」。
func TestForkSelfUpdateDisabledStatusHasNoAvailable(t *testing.T) {
	if !SelfUpdateDisabled {
		t.Skip(forkSkipReason)
	}
	updateMu.Lock()
	old := updateState
	updateState = UpdateState{
		Current: appVersion, Available: true, Latest: "20991231-9",
		DownloadURL:    "http://127.0.0.1:1/installer.exe",
		PendingRestart: true, PendingVersion: "20991231-9",
	}
	updateMu.Unlock()
	defer func() {
		updateMu.Lock()
		updateState = old
		updateMu.Unlock()
	}()

	a := &App{Cfg: &Config{}}
	rec := httptest.NewRecorder()
	a.handleStatus(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/status 应 200，得到 %d", rec.Code)
	}
	var body struct {
		Update map[string]any `json:"update"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 /api/status 失败: %v", err)
	}
	if v, ok := body.Update["available"].(bool); !ok || v {
		t.Fatalf("available 必须恒为 false: %v", body.Update["available"])
	}
	if _, has := body.Update["pendingRestart"]; has {
		t.Fatalf("不该出现待重启提示: %v", body.Update["pendingRestart"])
	}
	if _, has := body.Update["downloadUrl"]; has {
		t.Fatalf("不该下发安装包直链: %v", body.Update["downloadUrl"])
	}
	if v, ok := body.Update["disabled"].(bool); !ok || !v {
		t.Fatalf("应标注 disabled（前端据此收起「软件更新」区）: %v", body.Update["disabled"])
	}
}

// ③ 能替换程序本体的路径全部早退：手动检查不联网、整包不下载、engine 不换
// exe、待重启接力不起（handleUpdateRestart 一旦放行就会 os.Exit，必须拦住）。
func TestForkSelfUpdateDisabledEngineReplacePaths(t *testing.T) {
	if !SelfUpdateDisabled {
		t.Skip(forkSkipReason)
	}
	a := &App{Cfg: &Config{DataDir: t.TempDir(), UpdateURL: "http://127.0.0.1:1/updates/latest"}}

	assertBlocked := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code == http.StatusOK {
			t.Fatalf("%s 应被拒绝（已禁用），实际返回 200: %s", name, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "已禁用") {
			t.Fatalf("%s 的拒绝理由应说明已禁用，实际: %s", name, rec.Body.String())
		}
	}

	// 手动「立即检查更新」：不联网、直接回已禁用
	rec := httptest.NewRecorder()
	a.handleUpdateCheck(rec, httptest.NewRequest("POST", "/api/update/check", nil))
	assertBlocked("手动检查更新", rec)

	// 整包安装包（会覆盖本程序 exe）：接口与执行体两道都拦住
	rec = httptest.NewRecorder()
	a.handleUpdateInstaller(rec, httptest.NewRequest("POST", "/api/update/installer", nil))
	assertBlocked("整包下载安装", rec)
	if err := a.downloadAndRunInstaller("http://127.0.0.1:1/installer.exe"); err == nil ||
		!strings.Contains(err.Error(), "已禁用") {
		t.Fatalf("整包下载执行体应早退，得到: %v", err)
	}

	// engine 分块交换（换掉本程序正位 exe）
	if err := a.applyEngineBlock(updateBlock{Version: "20991231-9", File: "engine.zip"},
		releaseInfo{}, "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "已禁用") {
		t.Fatalf("engine 分块交换应早退，得到: %v", err)
	}

	// 待重启接力：即便落盘里残留 pending（升级前留下的更晚版本号）也不重启
	if err := a.saveUpdateBlockState(updateBlockState{EnginePending: "20991231-9"}); err != nil {
		t.Fatalf("写 pending 记账失败: %v", err)
	}
	rec = httptest.NewRecorder()
	a.handleUpdateRestart(rec, httptest.NewRequest("POST", "/api/update/restart", nil))
	assertBlocked("待重启接力", rec)
}
