package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestP1F15ScriptBridge(t *testing.T) {
	a, m, srv := newBridgeTestApp(t)
	runtime := "D:/OpenTravel7/jx-assistant/技能/_runtime/python/python.exe"
	if _, err := os.Stat(runtime); err != nil {
		t.Skip("便携 Python 未装配，脚本桥测试需内置运行时")
	}
	// 合成 App 明确配置路径，避免空 CfgPath 把仓库内测试临时工作区误保护。
	a.CfgPath = filepath.Join(a.Cfg.DataDir, "config.json")
	a.initRuntime()
	u, _ := url.Parse(srv.URL)
	a.listenPort, _ = strconv.Atoi(u.Port())
	tc := &toolContext{app: a, sess: m}
	_, cleanup, err := a.bridgePrepare(tc, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	good := filepath.Join(m.Workspace, "good.txt")
	if err := os.WriteFile(good, []byte("body contains ❌ but succeeds"), 0600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`import sys,json
sys.path.insert(0,%q)
import jx
missing=%q
good=%q
for tool,key,bad,valid in [('read','files',{'path':missing},{'path':good}),('edit','edits',{'path':missing,'old_string':'a','new_string':'b'},{'path':good,'old_string':'body','new_string':'BODY'})]:
    failed=jx.call(tool,{key:[bad]})
    print(json.dumps(failed,ensure_ascii=False))
    assert failed['ok'] is False, failed
    assert failed['items'][0]['status']=='failed', failed
    assert 'result' in failed and 'error' in failed, failed
    partial=jx.call(tool,{key:[bad,valid]})
    print(json.dumps(partial,ensure_ascii=False))
    assert partial['ok'] is True, partial
    assert [x['status'] for x in partial['items']]==['failed','success'], partial
    assert isinstance(partial['result'],str), partial
print('SCRIPT PASS')
`, pySessionDir(tc), filepath.Join(m.Workspace, "missing.txt"), good)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtime, "-c", script)
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil || !strings.Contains(string(out), "SCRIPT PASS") {
		t.Fatalf("script failed: %v", err)
	}
}
